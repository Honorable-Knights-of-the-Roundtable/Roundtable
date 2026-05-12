package ipc

import (
	"encoding/json"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/audioapi"
)

// Types to describe the Interprocess communication between local audio server and frontend client

type Incoming struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

type Outgoing struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

type InitData struct {
	InputDevices        []audioapi.AudioIODevice `json:"input_devices"`
	OutputDevices       []audioapi.AudioIODevice `json:"output_devices"`
	CurrentInputDevice  audioapi.AudioIODevice   `json:"current_input_device"`
	CurrentOutputDevice audioapi.AudioIODevice   `json:"current_output_device"`
	Channel             int                      `json:"channel"`
	Gain                float32                  `json:"gain"`
}

type RoomJoinedData struct {
	Peers []string `json:"peers"`
}

type ErrorData struct {
	Message string `json:"message"`
}
