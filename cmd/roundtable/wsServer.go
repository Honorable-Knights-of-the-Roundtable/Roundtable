package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/cmd/application"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/audioapi"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/ipc"
	"github.com/gorilla/websocket"
)

type threadSafeWriter struct {
	*websocket.Conn
	sync.Mutex
}

func (t *threadSafeWriter) WriteJSON(v any) error {
	t.Lock()
	defer t.Unlock()
	return t.Conn.WriteJSON(v)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	app  *application.App
	conn *threadSafeWriter
	mu   sync.Mutex
}

func NewServer(app *application.App) *Server {
	s := &Server{app: app}
	app.SetRoomUpdateCallback(func(peers []string) {
		s.SendEvent(ipc.Outgoing{Type: "room_joined", Data: ipc.RoomJoinedData{Peers: peers}})
	})
	return s
}

func (s *Server) SendEvent(event any) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()

	if conn != nil {
		conn.WriteJSON(event)
	}
}

func (s *Server) sendError(message string) {
	s.SendEvent(ipc.Outgoing{Type: "error", Data: ipc.ErrorData{Message: message}})
}

func (s *Server) join(roomName string) {
	ctx := context.Background()
	_, err := s.app.JoinRoom(ctx, roomName)
	if err != nil {
		slog.Error("error joining room", "room", roomName, "err", err)
		s.sendError(fmt.Sprintf("failed to join room: %v", err))
	}
	// room_joined is sent via the SetRoomUpdateCallback when the signalling server
	// broadcasts the updated member list.
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("websocket upgrade failed", "err", err)
		return
	}
	defer conn.Close()

	safe := &threadSafeWriter{Conn: conn}

	s.mu.Lock()
	s.conn = safe
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.conn = nil
		s.mu.Unlock()
	}()

	slog.Info("frontend connected")
	s.SendEvent(ipc.Outgoing{Type: "init", Data: ipc.InitData{
		InputDevices:        s.app.GetInputDevices(),
		OutputDevices:       s.app.GetOutputDevices(),
		CurrentInputDevice:  s.app.GetCurrentInputDevice(),
		CurrentOutputDevice: s.app.GetCurrentOutputDevice(),
		Channel:             s.app.GetPreferredInputChannel(),
		Gain:                s.app.GetInputGain(),
	}})

	for {
		var msg ipc.Incoming
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Error("websocket read error", "err", err)
			}
			return
		}
		s.handleCommand(msg)
	}
}

func (s *Server) handleCommand(msg ipc.Incoming) {
	switch msg.Type {
	case "join":
		var data struct {
			Room string `json:"room"`
		}
		if err := json.Unmarshal(msg.Data, &data); err != nil || data.Room == "" {
			s.sendError("invalid join message: room name required")
			return
		}
		s.join(data.Room)

	case "input":
		slog.Info("input command")
		var data struct {
			InputDevice audioapi.AudioIODevice `json:"device"`
		}
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			slog.Warn("invalid channel message", "err", err)
			return
		}
		s.app.SelectInputDevice(data.InputDevice)

	case "output":
		slog.Info("output command")
		var data struct {
			OutputDevice audioapi.AudioIODevice `json:"device"`
		}
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			slog.Warn("invalid channel message", "err", err)
			return
		}
		s.app.SelectOutputDevice(data.OutputDevice)

	case "test":
		slog.Info("test command")
		var data struct {
			Testing bool `json:"testing"`
		}

		if err := json.Unmarshal(msg.Data, &data); err != nil {
			slog.Warn("invalid channel message", "err", err)
			return
		}
		if data.Testing {
			slog.Info("s.app.StopMicSelfPlayback()")
			s.app.StopMicSelfPlayback()
		} else {
			slog.Info("s.app.StartMicSelfPlayback()")
			s.app.StartMicSelfPlayback()
		}

		// TODO: micTest(s.app)
	case "channel":
		var data struct {
			Channel int `json:"channel"`
		}
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			slog.Warn("invalid channel message", "err", err)
			return
		}
		s.app.SetInputChannel(data.Channel)
	case "gain":
		var data struct {
			Gain float32 `json:"gain"`
		}
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			slog.Warn("invalid gain message", "err", err)
			return
		}
		s.app.SetInputGain(data.Gain)
	case "disconnect":
		slog.Info("disconnect command")
		s.app.DisconnectAll()
		ctx := context.Background()
		err := s.app.DisconnectRooms(ctx)
		if err != nil {
			slog.Error("Error with leaving room", "err", err)
		}
		s.SendEvent(ipc.Outgoing{Type: "room_joined", Data: ipc.RoomJoinedData{Peers: []string{}}})
	case "close", "exit":
		s.app.Close()
		os.Exit(0)
	default:
		slog.Warn("unknown command type", "type", msg.Type)
	}
}

func (s *Server) Start(listenAddress string) error {
	fmt.Printf("Starting local voip server at: %s\n", listenAddress)
	slog.Info("starting local voip server", "address", listenAddress)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)

	if err := http.ListenAndServe(listenAddress, mux); err != nil {
		slog.Error("server failed", "err", err)
		return err
	}

	return nil
}
