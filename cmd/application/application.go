package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	// "log/slog"

	// "log/slog"
	"sync"
	"time"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/audioapi"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/networking"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/peer"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice/device"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/frame"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/signalling"
	"github.com/google/uuid"
)

// The main application representation for the client.
//
// Holds references to the audio input / output devices,
// the audio IO library (e.g. RTAudio, PortAudio, to generate above devices),
// the connected peers, and so on.
//
// This struct provides a good basis for integration of the TUI.
type App struct {
	// --------------------------------------------------------------------------------
	// Connections and Peers

	// Handle connections (offering and answering) and produce connected peers on
	// the ConnectedPeerChannel.
	connectionManager *networking.ConnectionManager

	// A list of currently connected peers. All peers should receive data from
	// the client's audio input device and send data to the client's output device.
	//
	// These connections should be made when the Peer is received from the ConnectionManager.
	connectedPeers      []*ApplicationPeer
	connectedPeersMutex sync.Mutex

	// A list of all rejected peers, those that have been disconnected
	// or failed to connected during the lifetime of this client
	// (e.g. by a Codec mismatch). Use this information to prevent
	// trying to reconnect to the same failing peers over and over.
	//
	// The PeerIdentifier.UUID is unique to an instance of Roundtable,
	// so a remote client restarting will generate a new UUID
	// and hence allow a new attempt at connection.
	rejectedPeerIdentifiers []signalling.PeerIdentifier

	// --------------------------------------------------------------------------------
	// Audio Input, Output, API, and Devices

	// The audio device API for the host machine. Allows querying of
	// input and output devices (microphones and speakers) and for
	// opening / selecting those input / output devices.
	//
	// Supported APIs are RTAudio and PortAudio
	audioIODeviceAPI audioapi.AudioIODeviceAPI

	// Audio Data Flow from Application to Peer (input path)
	// | ----------------------------------- Application ----------------------------------- |	   | -------- ApplicationPeer -------- |
	// Client's audio input device (e.g. microphone) -> AudioAugmentationDevice -> FanOutDevice -> [AudioFormatConversionDevice -> Peer]

	// TODO: Perhaps this shouldn't be public, but a getter would have no purpose other than returning this
	// The audio input device of the client, i.e. the microphone of choice
	AudioInputDevice audiodevice.AudioSourceDevice

	// Augmentation of the input audio, e.g. for setting this client's volume before sending to the remote peer
	inputAugmentationDevice *device.AudioAugmentationDevice

	// FanOutDevice to copy audio data from the microphone (more specifically the inputAugmentationDevice) to all connected peers
	inputFanOutDevice *device.FanOutDevice

	// Audio Data Flow from Peer to Application (output path)
	// | ---------------------- ApplicationPeer ---------------------- |    | -------------------- Application -------------------- |
	// [ Peer -> AudioFormatConversionDevice -> AudioAugmentationDevice] -> FanInDevice -> Client's audio output device (e.g. speaker)

	// TODO: Perhaps this shouldn't be public, but a getter would have no purpose other than returning this
	// The audio output device, i.e. the speaker of choice
	AudioOutputDevice audiodevice.AudioSinkDevice

	// FanInDevice to mix audio from all connected peers back into a single frame to send to speakers
	outputFanInDevice *device.FanInDevice

	// For stereo input devices: which channel to use when converting to mono.
	// 0 = left (Input 1), 1 = right (Input 2). Default 0.
	preferredInputChannel int

	// The AudioIODevice metadata for the currently selected input/output devices.
	// Kept in sync with AudioInputDevice/AudioOutputDevice so callers can get the
	// name and ID without cross-referencing the device list.
	currentInputDevice  audioapi.AudioIODevice
	currentOutputDevice audioapi.AudioIODevice

	micMonitorMu   sync.Mutex
	micMonitorStop func()

	currentRoom   string
	currentRoomMu sync.Mutex
}

// --------------------------------------------------------------------------------
// Initialization of App

// Create and initialize a new application using the given audioIODeviceAPI
// (for handling getting/setting the input and output devices, microphone and speaker resp.)
func NewApp(
	audioIODeviceAPI audioapi.AudioIODeviceAPI,
	connectionManager *networking.ConnectionManager,
) (*App, error) {
	app := &App{
		connectionManager:       connectionManager,
		connectedPeers:          make([]*ApplicationPeer, 0),
		rejectedPeerIdentifiers: make([]signalling.PeerIdentifier, 0),

		audioIODeviceAPI: audioIODeviceAPI,
		// The remaining audio struct items are initialized by calls to SetInputDevice, SetOutputDevice
	}

	// --------------------------------------------------------------------------------
	// Set the initial input/output devices to defaults.

	defaultInputDevice, err := audioIODeviceAPI.InitDefaultInputDevice()
	if err != nil {
		return nil, err
	}
	app.SetInputDevice(defaultInputDevice)
	app.currentInputDevice = findDeviceByID(audioIODeviceAPI.InputDevices(), defaultInputDevice.GetDeviceProperties().ID)

	defaultOutputDevice, err := audioIODeviceAPI.InitDefaultOutputDevice()
	if err != nil {
		return nil, err
	}
	app.SetOutputDevice(defaultOutputDevice)
	app.currentOutputDevice = findDeviceByID(audioIODeviceAPI.OutputDevices(), defaultOutputDevice.GetDeviceProperties().ID)

	// --------------------------------------------------------------------------------
	// Start listening for new peers
	go func() {
		for newPeer := range connectionManager.ConnectedPeerChannel {
			app.handleConnectedPeer(newPeer)
		}
	}()

	return app, nil
}

func (app *App) SetRoomUpdateCallback(cb func(peers []string)) {
	app.connectionManager.SetRoomUpdateCallback(cb)
}

func (app *App) handleConnectedPeer(newPeer *peer.Peer) {
	// TODO: Reject peer if already connected / in rejected peer list?

	// TODO: Handle application level logic of receiving chat room information,
	// dialing new peers, any listeners that need to be set?

	app.connectedPeersMutex.Lock()
	defer app.connectedPeersMutex.Unlock()

	sinkAudioFormatConversionDevice := device.NewAudioFormatConversionDevice(
		app.propsWithChannel(app.AudioInputDevice.GetDeviceProperties()),
		newPeer.GetDeviceProperties(),
	)
	newPeer.SetStream(sinkAudioFormatConversionDevice.GetStream())
	sinkAudioFormatConversionDevice.SetStream(app.inputFanOutDevice.GetStream())

	sourceAudioFormatConversionDevice := device.NewAudioFormatConversionDevice(
		newPeer.GetDeviceProperties(),
		app.AudioOutputDevice.GetDeviceProperties(),
	)
	sourceAudioAugmentationDevice := device.NewAudioAugmentationDevice(
		app.AudioInputDevice.GetDeviceProperties(),
	)
	sourceAudioFormatConversionDevice.SetStream(newPeer.GetStream())
	sourceAudioAugmentationDevice.SetStream(sourceAudioFormatConversionDevice.GetStream())
	app.outputFanInDevice.SetStream(sourceAudioAugmentationDevice.GetStream())

	appPeer := ApplicationPeer{
		peer:                              newPeer,
		sourceAudioAugmentationDevice:     sourceAudioAugmentationDevice,
		sourceAudioFormatConversionDevice: &sourceAudioFormatConversionDevice,
		sinkAudioFormatConversionDevice:   &sinkAudioFormatConversionDevice,
	}

	app.connectedPeers = append(app.connectedPeers, &appPeer)

	go func() {
		<-newPeer.GetContext().Done()
		app.removePeer(newPeer)
	}()
}

func (app *App) removePeer(p *peer.Peer) {
	app.connectedPeersMutex.Lock()
	defer app.connectedPeersMutex.Unlock()
	for i, ap := range app.connectedPeers {
		if ap.peer == p {
			app.connectedPeers = append(app.connectedPeers[:i], app.connectedPeers[i+1:]...)
			return
		}
	}
}

// DisconnectPeer closes the connection to the peer with the given UUID.
// Returns an error if no connected peer with that UUID is found.
// The peer is removed from the connected peers list automatically once closed.
func (app *App) DisconnectPeer(id uuid.UUID) error {
	app.connectedPeersMutex.Lock()
	defer app.connectedPeersMutex.Unlock()
	for _, ap := range app.connectedPeers {
		if ap.peer.Identifier().Uuid == id {
			ap.Close()
			return nil
		}
	}
	return fmt.Errorf("no connected peer with UUID %s", id)
}

// DisconnectAll closes all currently connected peers, effectively leaving the room.
// The audio devices remain running so the app can join a new room afterwards.
func (app *App) DisconnectAll() {
	app.connectedPeersMutex.Lock()
	peers := app.connectedPeers
	app.connectedPeers = nil
	app.connectedPeersMutex.Unlock()

	for _, ap := range peers {
		ap.Close()
	}
}

// --------------------------------------------------------------------------------
// Getters and Setters for App
// May be useful in TUI calls

// Close and cleanup the application.
//
// This method calls close on the input device, and all peers.
// After calling close, the app should be discarded. Further interactions may panic.
func (app *App) Close() {
	app.connectedPeersMutex.Lock()
	defer app.connectedPeersMutex.Unlock()

	app.AudioInputDevice.Close()
	for _, peer := range app.connectedPeers {
		peer.Close()
	}
	app.outputFanInDevice.Close()

	// Stop the RT output stream. In the pull model there is no channel cascade to
	// trigger this automatically, so we close it explicitly.
	type closer interface{ Close() }
	if c, ok := app.AudioOutputDevice.(closer); ok {
		c.Close()
	}
}

func (app *App) SetInputDevice(inputDevice audiodevice.AudioSourceDevice) {
	inputDeviceProperties := inputDevice.GetDeviceProperties()

	inputAugmentationDevice := device.NewAudioAugmentationDevice(inputDeviceProperties)
	inputAugmentationDevice.SetStream(inputDevice.GetStream())

	inputFanOutDevice := device.NewFanOutDevice(inputDeviceProperties)
	inputFanOutDevice.SetStream(inputAugmentationDevice.GetStream())

	// Change all peers to work with new inputs
	// Note we are changing the input device, and hence possibly also the input device properties
	// So we must also update the audio format conversions
	//
	// Update affected devices moving from right to left
	// To avoid accidentally sending new frames to peers before all conversion are set up
	//
	// | ----------------------------------- Application ----------------------------------- |	   | -------- ApplicationPeer -------- |
	// Client's audio input device (e.g. microphone) -> AudioAugmentationDevice -> FanOutDevice -> [AudioFormatConversionDevice -> Peer]

	app.connectedPeersMutex.Lock()
	for _, appPeer := range app.connectedPeers {
		newSinkAudioFormatConversionDevice := device.NewAudioFormatConversionDevice(
			app.propsWithChannel(inputDeviceProperties),
			appPeer.peer.GetDeviceProperties(),
		)

		appPeer.peer.SetStream(newSinkAudioFormatConversionDevice.GetStream())
		newSinkAudioFormatConversionDevice.SetStream(inputFanOutDevice.GetStream())

		appPeer.sinkAudioFormatConversionDevice.Close()
		appPeer.sinkAudioFormatConversionDevice = &newSinkAudioFormatConversionDevice
	}
	app.connectedPeersMutex.Unlock()

	// We made all devices correctly, now affect changes to App
	if app.AudioInputDevice != nil {
		oldInputDevice := app.AudioInputDevice
		defer oldInputDevice.Close()
	}
	app.AudioInputDevice = inputDevice
	app.inputAugmentationDevice = inputAugmentationDevice
	app.inputFanOutDevice = &inputFanOutDevice

	// slog.Debug("updated set input device", "new properties", app.audioInputDevice.GetDeviceProperties())
}

func (app *App) SetOutputDevice(outputDevice audiodevice.AudioSinkDevice) {
	outputDeviceProperties := outputDevice.GetDeviceProperties()

	outputFanInDevice := device.NewFanInDevice(outputDeviceProperties)

	// Pull model: if the output device supports SetFiller, wire the hardware callback
	// directly to FanInDevice.Fill, eliminating the software ticker entirely.
	type pullSink interface{ SetFiller(func([]float32)) }
	if ps, ok := outputDevice.(pullSink); ok {
		ps.SetFiller(outputFanInDevice.Fill)
	}

	// Change all peers to work with new output
	// Note we are changing the output device, and hence possibly also the output device properties
	// So we must also update the audio format conversions
	//
	// Update affected devices moving from left to right
	// to avoid the FanInDevice consuming frames in the wrong format
	//
	// | ---------------------- ApplicationPeer ---------------------- |    | -------------------- Application -------------------- |
	// [ Peer -> AudioFormatConversionDevice -> AudioAugmentationDevice] -> FanInDevice -> Client's audio output device (e.g. speaker)

	app.connectedPeersMutex.Lock()
	for _, appPeer := range app.connectedPeers {
		newSourceAudioFormatConversionDevice := device.NewAudioFormatConversionDevice(
			appPeer.peer.GetDeviceProperties(),
			outputDeviceProperties,
		)

		newSourceAudioFormatConversionDevice.SetStream(appPeer.peer.GetStream())
		appPeer.sourceAudioAugmentationDevice.SetStream(newSourceAudioFormatConversionDevice.GetStream())
		outputFanInDevice.SetStream(appPeer.sourceAudioAugmentationDevice.GetStream())

		appPeer.sourceAudioFormatConversionDevice.Close()
		appPeer.sourceAudioFormatConversionDevice = &newSourceAudioFormatConversionDevice
	}
	app.connectedPeersMutex.Unlock()

	// We made all devices correctly, now affect changes to App
	if app.outputFanInDevice != nil {
		oldFanInDevice := app.outputFanInDevice
		defer oldFanInDevice.Close()
	}
	app.outputFanInDevice = outputFanInDevice
	app.AudioOutputDevice = outputDevice

	// slog.Debug("updated set output device", "new properties", app.audioOutputDevice.GetDeviceProperties())
}
func (app *App) DisconnectRooms(ctx context.Context) error {
	app.currentRoomMu.Lock()
	app.currentRoom = ""
	app.currentRoomMu.Unlock()
	return app.connectionManager.SendDisconnectMessage(ctx)
}

// JoinRoom joins a named room on the signalling server and dials all peers already in it.
// Returns an error if already in a room.
// TODO(Jake):  As mentioned at connectionManager.JoinRoom, this should probably return a `User` object or something,
//
//	but I am unsure what that will look like, so for now it just returns a []string
func (app *App) JoinRoom(ctx context.Context, roomName string) ([]string, error) {
	app.currentRoomMu.Lock()
	if app.currentRoom != "" {
		room := app.currentRoom
		app.currentRoomMu.Unlock()
		return nil, fmt.Errorf("already in room %q, disconnect first", room)
	}
	app.currentRoom = roomName
	app.currentRoomMu.Unlock()

	peers, err := app.connectionManager.JoinRoom(ctx, roomName)
	if err != nil {
		app.currentRoomMu.Lock()
		app.currentRoom = ""
		app.currentRoomMu.Unlock()
	}
	return peers, err
}

// propsWithChannel returns a copy of props with StereoChannelIndex set to the app's current preference.
func (app *App) propsWithChannel(props audiodevice.DeviceProperties) audiodevice.DeviceProperties {
	props.StereoChannelIndex = app.preferredInputChannel
	return props
}

// SetInputChannel sets which physical input channel to use for stereo input devices.
// idx is 0-based: 0 = Input 1 (left), 1 = Input 2 (right).
// Re-wires format conversion for all currently connected peers.
func (app *App) SetInputChannel(idx int) {
	app.preferredInputChannel = idx
	if app.AudioInputDevice == nil {
		return
	}
	props := app.propsWithChannel(app.AudioInputDevice.GetDeviceProperties())

	app.connectedPeersMutex.Lock()
	defer app.connectedPeersMutex.Unlock()
	for _, appPeer := range app.connectedPeers {
		newConv := device.NewAudioFormatConversionDevice(props, appPeer.peer.GetDeviceProperties())
		appPeer.peer.SetStream(newConv.GetStream())
		newConv.SetStream(app.inputFanOutDevice.GetStream())
		appPeer.sinkAudioFormatConversionDevice.Close()
		appPeer.sinkAudioFormatConversionDevice = &newConv
	}
}

func (app *App) GetInputDevices() []audioapi.AudioIODevice {
	return app.audioIODeviceAPI.InputDevices()
}

func (app *App) GetOutputDevices() []audioapi.AudioIODevice {
	return app.audioIODeviceAPI.OutputDevices()
}

func (app *App) GetCurrentInputDevice() audioapi.AudioIODevice {
	return app.currentInputDevice
}

func (app *App) GetCurrentOutputDevice() audioapi.AudioIODevice {
	return app.currentOutputDevice
}

// SelectInputDevice opens the given device and makes it the active input.
func (app *App) SelectInputDevice(dev audioapi.AudioIODevice) error {
	opened, err := app.audioIODeviceAPI.InitInputDeviceFromID(dev)
	if err != nil {
		return err
	}
	app.SetInputDevice(opened)
	app.currentInputDevice = dev
	return nil
}

// SelectOutputDevice opens the given device and makes it the active output.
func (app *App) SelectOutputDevice(dev audioapi.AudioIODevice) error {
	opened, err := app.audioIODeviceAPI.InitOutputDeviceFromID(dev)
	if err != nil {
		return err
	}
	app.SetOutputDevice(opened)
	app.currentOutputDevice = dev
	return nil
}

func findDeviceByID(devices []audioapi.AudioIODevice, id int) audioapi.AudioIODevice {
	for _, dev := range devices {
		if dev.ID == id {
			return dev
		}
	}
	return audioapi.AudioIODevice{}
}

// GetPreferredInputChannel returns the current 0-based channel index preference (0 = Input 1, 1 = Input 2).
func (app *App) GetPreferredInputChannel() int {
	return app.preferredInputChannel
}

// SetInputGain sets the volume multiplier applied to the local microphone input.
// 1.0 is unity gain. Use values > 1.0 to boost a quiet input device.
func (app *App) SetInputGain(gain float32) {
	app.inputAugmentationDevice.SetVolumeAdjustMagnitude(gain)
}

// Not sure if audioIODeviceAPI should just be public but will do getters for now
func (app *App) GetInputGain() float32 {
	return app.inputAugmentationDevice.GetVolumeAdjustMagnitude()
}

// TapInputAudio records raw audio from the local mic input until ctx is cancelled.
// Returns float32 samples plus the device's sample rate and channel count.
func (app *App) TapInputAudio(ctx context.Context) ([]float32, int, int, error) {
	tap := app.inputFanOutDevice.GetStream()
	props := app.AudioInputDevice.GetDeviceProperties()

	var samples []float32
	for {
		select {
		case <-ctx.Done():
			return samples, props.SampleRate, props.NumChannels, nil
		case f, ok := <-tap:
			if !ok {
				return samples, props.SampleRate, props.NumChannels, nil
			}
			samples = append(samples, f...)
		}
	}
}

// TapInputAudio records raw audio from the local mic input until ctx is cancelled.
// Returns float32 samples plus the device's sample rate and channel count.
// Prints incoming audio levels, will be reworked to provide some kind of data output instead,
// but fine for now
func (app *App) TapInputAudioWithStats(ctx context.Context) ([]float32, int, int, error) {
	tap := app.inputFanOutDevice.GetStream()
	props := app.AudioInputDevice.GetDeviceProperties()

	var samples []float32
	for {
		select {
		case <-ctx.Done():
			return samples, props.SampleRate, props.NumChannels, nil
		case f, ok := <-tap:
			if !ok {
				return samples, props.SampleRate, props.NumChannels, nil
			}

			var peak float32
			for _, s := range f {
				if s < 0 {
					s = -s
				}
				if s > peak {
					peak = s
				}
			}
			fmt.Printf("\rRecorded %d samples, peak level: %.2f%%", len(samples), peak*100)
			samples = append(samples, f...)
		}
	}
}

func (app *App) StartMicSelfPlayback() {
	app.micMonitorMu.Lock()
	defer app.micMonitorMu.Unlock()

	if app.micMonitorStop != nil {
		return // already running
	}

	tap := app.inputFanOutDevice.GetStream()
	conv := device.NewAudioFormatConversionDevice(
		app.inputFanOutDevice.GetDeviceProperties(),
		app.outputFanInDevice.GetDeviceProperties(),
	)
	conv.SetStream(tap)
	app.outputFanInDevice.SetStream(conv.GetStream())
	app.micMonitorStop = conv.Close
}

func (app *App) StopMicSelfPlayback() {
	app.micMonitorMu.Lock()
	defer app.micMonitorMu.Unlock()

	if app.micMonitorStop != nil {
		app.micMonitorStop()
		app.micMonitorStop = nil
	}
}

// RecordPeerAudio records raw decoded audio from the first connected peer for the given
// duration and returns the samples and the peer's device properties (sample rate, channels).
// The samples are in the same format as they arrive from the network — before any local
// format conversion or mixing — so they can be written straight to a WAV file for inspection.
func (app *App) RecordPeerAudio(duration time.Duration) ([]float32, int, int, error) {
	app.connectedPeersMutex.Lock()
	if len(app.connectedPeers) == 0 {
		app.connectedPeersMutex.Unlock()
		return nil, 0, 0, fmt.Errorf("no connected peers")
	}
	target := app.connectedPeers[0].peer
	props := target.GetDeviceProperties()
	app.connectedPeersMutex.Unlock()

	tap := make(chan frame.PCMFrame, 256)
	target.SetDebugTap(tap)
	defer target.SetDebugTap(nil)

	var samples []float32
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			return samples, props.SampleRate, props.NumChannels, nil
		case f, ok := <-tap:
			if !ok {
				return samples, props.SampleRate, props.NumChannels, nil
			}
			samples = append(samples, f...)
		}
	}
}

// Taking the remote peer information as a Base64-encoded JSON-representation of the signalling.PeerIdentifier
// dial the peer specified and return.
//
// Note that this method does not guarantee that the remote peer actually accepts the connection!
// Nor does it guarantee that once this method returns, the remote peer is connected!
//
// If the connection is made successfully, the peer is added to the ConnectedPeerList by
// app.handleConnectedPeer
func (app *App) DialRemotePeer(ctx context.Context, encodedPeerIdentifier string) error {
	// Decode and unmarshal the encoded peer identifier
	decodedPeerIdentifier, err := base64.StdEncoding.DecodeString(encodedPeerIdentifier)
	if err != nil {
		return err
	}

	var peerIdentifier signalling.PeerIdentifier
	if err := json.Unmarshal(decodedPeerIdentifier, &peerIdentifier); err != nil {
		return err
	}

	// Now the decoded peer ID lives in the peerIdentifier struct,
	// dial it with the connectionManager
	if err := app.connectionManager.Dial(ctx, peerIdentifier); err != nil {
		return err
	}

	// We have dialled with on issue, we must now hope the connection goes through
	// but unblock the main thread in the mean time.
	return nil
}
