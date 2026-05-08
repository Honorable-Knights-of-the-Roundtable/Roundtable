package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"flag"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/widget"

	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/cmd/config"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/audioapi"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/ipc"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/utils"
	"github.com/gorilla/websocket"

	"github.com/spf13/viper"
)

// ---- App state --------------------------------------------------------------

type User struct {
	Name  string
	Muted bool
}

type AppState struct {
	mu                  sync.Mutex
	Users               []User
	// TODO(Jake): This mirrors ipc.InitData, I am unsure if it should just be a shared type or not
	InputDevices        []audioapi.AudioIODevice
	OutputDevices       []audioapi.AudioIODevice
	CurrentInputDevice  audioapi.AudioIODevice
	CurrentOutputDevice audioapi.AudioIODevice
	Channel             int
	Gain                float32
	Deafened            bool
}

// ---- WebSocket client -------------------------------------------------------

type WSClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *WSClient) send(msgType string, data any) {
	b, _ := json.Marshal(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.WriteJSON(ipc.Incoming{Type: msgType, Data: b})
}

func connectWS(client *WSClient, addr string, state *AppState, onUpdate func()) error {
	conn, _, err := websocket.DefaultDialer.Dial(addr, nil)
	if err != nil {
		slog.Error("ws connect failed", "err", err)
		return err
	}
	client.conn = conn

	onUpdate()

	go func() {
		defer conn.Close()
		for {
			var msg ipc.Incoming
			if err := conn.ReadJSON(&msg); err != nil {
				slog.Error("ReadJSON error", "err", err)
				return
			}

			state.mu.Lock()
			switch msg.Type {
			case "init":
				var ev ipc.InitData
				if err := json.Unmarshal(msg.Data, &ev); err != nil {
					slog.Error("Unmarshal initData error", "err", err)
				}
				fmt.Printf("InitData\n%v", ev)
				state.InputDevices = ev.InputDevices
				state.OutputDevices = ev.OutputDevices
				state.CurrentInputDevice = ev.CurrentInputDevice
				state.CurrentOutputDevice = ev.CurrentOutputDevice
				state.Gain = ev.Gain

			case "room_joined":
				slog.Info("room_joined")
				var ev ipc.RoomJoinedData
				if json.Unmarshal(msg.Data, &ev) == nil {
					state.Users = make([]User, len(ev.Peers))
					for i, p := range ev.Peers {
						state.Users[i] = User{Name: p}
					}
				}
			case "peer_joined":
				slog.Info("peer_joined")
				var ev struct{ Name string `json:"name"` }
				if json.Unmarshal(msg.Data, &ev) == nil {
					state.Users = append(state.Users, User{Name: ev.Name})
				}
			case "peer_left":
				slog.Info("peer_left")
				var ev struct{ Name string `json:"name"` }
				if json.Unmarshal(msg.Data, &ev) == nil {
					for i, u := range state.Users {
						if u.Name == ev.Name {
							state.Users = append(state.Users[:i], state.Users[i+1:]...)
							break
						}
					}
				}
			default:
				slog.Info("unknown message", "type", msg.Type)
			}
			state.mu.Unlock()

			onUpdate()
		}
	}()

	return nil
}

func deviceNames(devices []audioapi.AudioIODevice) []string {
	names := make([]string, len(devices))
	for i, d := range devices {
		names[i] = d.Name
	}
	return names
}

func newUserList(state *AppState) *widget.List {
	var userList *widget.List
	userList = widget.NewList(
		func() int {
			state.mu.Lock()
			defer state.mu.Unlock()
			return len(state.Users)
		},
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil,
				widget.NewButton("Mute", nil),
				widget.NewLabel(""),
			)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			row := obj.(*fyne.Container)
			label := row.Objects[0].(*widget.Label)
			btn := row.Objects[1].(*widget.Button)

			state.mu.Lock()
			if id >= len(state.Users) {
				state.mu.Unlock()
				return
			}
			name := state.Users[id].Name
			muted := state.Users[id].Muted
			state.mu.Unlock()

			label.SetText(name)
			if muted {
				btn.SetText("Unmute")
			} else {
				btn.SetText("Mute")
			}
			btn.OnTapped = func() {
				state.mu.Lock()
				if id < len(state.Users) {
					state.Users[id].Muted = !state.Users[id].Muted
				}
				state.mu.Unlock()
				userList.RefreshItem(id)
			}
		},
	)
	return userList
}

func newJoinRow(client *WSClient) *fyne.Container {
	roomEntry := widget.NewEntry()
	roomEntry.SetText("my-room")
	roomEntry.SetPlaceHolder("Room name")

	joinBtn := widget.NewButton("Join", func() {
		if client != nil && roomEntry.Text != "" {
			client.send("join", map[string]string{"room": roomEntry.Text})
		}
	})
	disconnectBtn := widget.NewButton("Disconnect", func() {
		if client != nil {
			client.send("disconnect", nil)
		}
	})

	return container.NewBorder(nil, nil, nil,
		container.NewHBox(joinBtn, disconnectBtn),
		roomEntry,
	)
}

// ---- Main -------------------------------------------------------------------

func main() {
	configFilePath := flag.String("configFilePath", "config.yaml", "Set the file path to the config file.")
	flag.Parse()

	config.LoadConfig(*configFilePath)
	logFilePointer, err := utils.ConfigureDefaultLogger(
		viper.GetString("loglevel"),
		viper.GetString("logfile"),
		slog.HandlerOptions{},
	)
	if err != nil {
		slog.Error("error while configuring default logger", "err", err)
		panic(err)
	}
	if logFilePointer != nil {
		defer logFilePointer.Close()
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	signalInterruptContext, signalInterruptContextCancel := context.WithCancel(context.Background())
	go func() {
		<-sigs
		signal.Reset()
		signalInterruptContextCancel()
	}()

	a := app.New()
	a.Settings().SetTheme(theme.DarkTheme())
	w := a.NewWindow("Roundtable")
	w.Resize(fyne.NewSize(400, 580))

	var state AppState
	var client WSClient

	w.SetCloseIntercept(func() {
		if client.conn != nil {
			client.send("disconnect", nil)
		}
		w.Close()
	})
	state.Users = []User{{"DefaultUser", false}}

	statusLabel := widget.NewLabel("Connecting to backend")
	userList := newUserList(&state)
	joinRow := newJoinRow(&client)

	// Audio settings — created before connectWS so onUpdate can refresh them
	inputSelect := widget.NewSelect(nil, func(name string) {
		state.mu.Lock()
		if name == state.CurrentInputDevice.Name {
			state.mu.Unlock()
			return
		}
		for _, dev := range state.InputDevices {
			if dev.Name == name {
				state.mu.Unlock()
				client.send("input", dev)
				return
			}
		}
		state.mu.Unlock()
	})

	outputSelect := widget.NewSelect(nil, func(name string) {
		state.mu.Lock()
		if name == state.CurrentOutputDevice.Name {
			state.mu.Unlock()
			return
		}
		for _, dev := range state.OutputDevices {
			if dev.Name == name {
				state.mu.Unlock()
				client.send("output", dev)
				return
			}
		}
		state.mu.Unlock()
	})

	// channelGroup := widget.NewRadioGroup([]string{"Input 1", "Input 2"}, nil)
	// channelGroup.SetSelected("Input 1")
	// channelGroup.Horizontal = true
	//
	// gainLabel := widget.NewLabel("1.0x")
	// gainSlider := widget.NewSlider(0, 5)
	// gainSlider.SetValue(1.0)
	// gainSlider.Step = 0.1
	// gainSlider.OnChanged = func(v float64) {
	// 	gainLabel.SetText(fmt.Sprintf("%.1fx", v))
	// }
	//
	// gainRow := container.NewBorder(nil, nil, nil, gainLabel, gainSlider)
	settingsForm := widget.NewForm(
		widget.NewFormItem("Input", inputSelect),
		widget.NewFormItem("Output", outputSelect),
		// widget.NewFormItem("Channel", channelGroup),
		// widget.NewFormItem("Gain", gainRow),
	)
	audioSettings := container.NewVBox(widget.NewSeparator(), settingsForm)
	//
	// micTestBtn := widget.NewButton("Mic Test", nil)
	//
	// deafenBtn := widget.NewButton("Deafen All", nil)
	// deafenBtn.OnTapped = func() {
	// 	if client == nil {
	// 		return
	// 	}
	// 	state.mu.Lock()
	// 	state.Deafened = !state.Deafened
	// 	deafened := state.Deafened
	// 	state.mu.Unlock()
	// 	if deafened {
	// 		deafenBtn.SetText("Undeafen")
	// 		client.send("deafen", nil)
	// 	} else {
	// 		deafenBtn.SetText("Deafen All")
	// 		client.send("undeafen", nil)
	// 	}
	// }
	//
	bottom := container.NewVBox(
		audioSettings,
		// widget.NewSeparator(),
		// container.NewGridWithColumns(2, micTestBtn, deafenBtn),
	)

	top := container.NewVBox(statusLabel, joinRow)
	content := container.NewBorder(top, bottom, nil, nil, userList)
	w.SetContent(content)

	err = connectWS(&client, "ws://127.0.0.1:42069/ws", &state, func() {
		fyne.Do(func() {
			statusLabel.SetText("Connected")
			state.mu.Lock()
			inputNames := deviceNames(state.InputDevices)
			outputNames := deviceNames(state.OutputDevices)
			currentInput := state.CurrentInputDevice.Name
			currentOutput := state.CurrentOutputDevice.Name
			state.mu.Unlock()
			inputSelect.SetOptions(inputNames)
			inputSelect.SetSelected(currentInput)
			outputSelect.SetOptions(outputNames)
			outputSelect.SetSelected(currentOutput)
			userList.Refresh()
		})
	})

	if err != nil {
		slog.Error("connectWS failed", "err", err)
		statusLabel.SetText("Failed to connect to backend")
	}


	w.ShowAndRun()

	<-signalInterruptContext.Done()
	slog.Debug("closing gui gracefully")
	if client.conn != nil {
		client.send("disconnect", nil)
	}
	w.Close()
}
