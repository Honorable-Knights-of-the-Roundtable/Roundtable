package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
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
	mu    sync.Mutex
	Users []User
	// TODO(Jake): This mirrors ipc.InitData, I am unsure if it should just be a shared type or not
	InputDevices        []audioapi.AudioIODevice
	OutputDevices       []audioapi.AudioIODevice
	CurrentInputDevice  audioapi.AudioIODevice
	CurrentOutputDevice audioapi.AudioIODevice
	Channel             int
	Gain                float32
	Deafened            bool
	Testing             bool
}

// ---- WebSocket client -------------------------------------------------------

type WSClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *WSClient) send(msgType string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		slog.Error("failed to marshal in send", "err", err)
	}
	fmt.Printf("Marshaled %v\n", string(b))
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
				state.InputDevices = ev.InputDevices
				state.OutputDevices = ev.OutputDevices
				state.CurrentInputDevice = ev.CurrentInputDevice
				state.CurrentOutputDevice = ev.CurrentOutputDevice
				state.Gain = ev.Gain
				state.Channel = ev.Channel

			case "room_joined":
				var ev ipc.RoomJoinedData
				if json.Unmarshal(msg.Data, &ev) == nil {
					state.Users = make([]User, len(ev.Peers))
					for i, p := range ev.Peers {
						state.Users[i] = User{Name: p}
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

func newInputSelect(state *AppState, client *WSClient) *widget.Select {
	return widget.NewSelect(nil, func(name string) {
		state.mu.Lock()
		if name == state.CurrentInputDevice.Name {
			state.mu.Unlock()
			return
		}
		for _, dev := range state.InputDevices {
			if dev.Name == name {
				state.mu.Unlock()
				client.send("input", map[string]audioapi.AudioIODevice{"device": dev})
				return
			}
		}
		state.mu.Unlock()
	})
}

func newOutputSelect(state *AppState, client *WSClient) *widget.Select {
	return widget.NewSelect(nil, func(name string) {
		state.mu.Lock()
		if name == state.CurrentOutputDevice.Name {
			state.mu.Unlock()
			return
		}
		for _, dev := range state.OutputDevices {
			if dev.Name == name {
				state.mu.Unlock()
				client.send("output", map[string]audioapi.AudioIODevice{"device": dev})
				return
			}
		}
		state.mu.Unlock()
	})
}

func newChannelGroup(client *WSClient) *widget.RadioGroup {
	channelGroup := widget.NewRadioGroup([]string{"1", "2"},
		func(s string) {
			fmt.Printf("clicked with value %s\n", s)
			channel, err := strconv.Atoi(s)
			fmt.Printf("parsed value %d\n", channel)
			if err != nil {
				slog.Error("could not parse channelGroup option", "err", err)
				return
			}
			client.send("channel", map[string]int{"channel": channel})
		},
	)
	channelGroup.Horizontal = true

	return channelGroup
}

// TODO: The highlight for showing that it is recording is jank, but fine for now
func newMicTestBtn(state *AppState, client *WSClient) *widget.Button {
	micTestBtn := widget.NewButtonWithIcon("Mic Test", theme.MediaRecordIcon(), nil)
	micTestBtn.Importance = widget.LowImportance
	micTestBtn.OnTapped = func() {
		state.mu.Lock()
		state.Testing = !state.Testing
		// User is saying to stop testing
		if state.Testing {
			micTestBtn.Importance = widget.HighImportance
			client.send("test", map[string]bool{"testing": false})
		} else {
			// User is saying to start testing
			micTestBtn.Importance = widget.LowImportance
			client.send("test", map[string]bool{"testing": true})
		}

		state.mu.Unlock()
	}
	return micTestBtn
}

// ---- Main -------------------------------------------------------------------

func main() {
	configFilePath := flag.String("configFilePath", "config.yaml", "Set the file path to the config file.")
	serverAddr := flag.String("serverAddr", "ws://127.0.0.1:42069/ws", "WebSocket address of the roundtable backend.")
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
	state.Testing = false
	var client WSClient

	w.SetCloseIntercept(func() {
		if client.conn != nil {
			client.send("disconnect", nil)
		}
		w.Close()
	})
	state.Users = []User{}

	statusLabel := widget.NewLabel("Connecting to backend")
	userList := newUserList(&state)
	joinRow := newJoinRow(&client)

	// Audio settings — created before connectWS so onUpdate can refresh them
	inputSelect := newInputSelect(&state, &client)
	outputSelect := newOutputSelect(&state, &client)

	channelGroup := newChannelGroup(&client)

	gainLabel := widget.NewLabel("1.0x")
	gainSlider := widget.NewSlider(0, 5)
	gainSlider.SetValue(1.0)
	gainSlider.Step = 0.1
	gainSlider.OnChanged = func(v float64) {
		gainLabel.SetText(fmt.Sprintf("%.1fx", v))
		client.send("gain", map[string]float32{"gain": float32(v)})
	}

	gainRow := container.NewBorder(nil, nil, nil, gainLabel, gainSlider)
	settingsForm := widget.NewForm(
		widget.NewFormItem("Input", inputSelect),
		widget.NewFormItem("Output", outputSelect),
		widget.NewFormItem("Channel", channelGroup),
		widget.NewFormItem("Gain", gainRow),
	)
	audioSettings := container.NewVBox(widget.NewSeparator(), settingsForm)
	micTestBtn := newMicTestBtn(&state, &client)

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
		widget.NewSeparator(),
		container.NewVBox(micTestBtn),
		// container.NewGridWithColumns(2, micTestBtn, deafenBtn),
	)

	top := container.NewVBox(statusLabel, joinRow)
	content := container.NewBorder(top, bottom, nil, nil, userList)
	w.SetContent(content)

	err = connectWS(&client, *serverAddr, &state, func() {
		fyne.Do(func() {
			statusLabel.SetText("Connected")
			state.mu.Lock()
			inputNames := deviceNames(state.InputDevices)
			outputNames := deviceNames(state.OutputDevices)
			currentInput := state.CurrentInputDevice.Name
			currentOutput := state.CurrentOutputDevice.Name
			channel := state.Channel
			gain := state.Gain
			state.mu.Unlock()
			inputSelect.SetOptions(inputNames)
			inputSelect.SetSelected(currentInput)
			outputSelect.SetOptions(outputNames)
			outputSelect.SetSelected(currentOutput)
			channelGroup.SetSelected(strconv.Itoa(channel))
			gainSlider.SetValue(float64(gain))
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
