package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	// "log/slog"
	"sync"
	"time"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/audioapi"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/networking"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/peer"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice/device"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/signalling"
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

	// The audio input device of the client, i.e. the microphone of choice
	audioInputDevice audiodevice.AudioSourceDevice

	// Augmentation of the input audio, e.g. for setting this client's volume before sending to the remote peer
	inputAugmentationDevice *device.AudioAugmentationDevice

	// FanOutDevice to copy audio data from the microphone (more specifically the inputAugmentationDevice) to all connected peers
	inputFanOutDevice *device.FanOutDevice

	// Audio Data Flow from Peer to Application (output path)
	// | ---------------------- ApplicationPeer ---------------------- |    | -------------------- Application -------------------- |
	// [ Peer -> AudioFormatConversionDevice -> AudioAugmentationDevice] -> FanInDevice -> Client's audio output device (e.g. speaker)

	// The audio output device, i.e. the speaker of choice
	audioOutputDevice audiodevice.AudioSinkDevice

	// FanInDevice to mix audio from all connected peers back into a single frame to send to speakers
	outputFanInDevice *device.FanInDevice
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

	defaultOutputDevice, err := audioIODeviceAPI.InitDefaultOutputDevice()
	if err != nil {
		return nil, err
	}
	app.SetOutputDevice(defaultOutputDevice)

	// --------------------------------------------------------------------------------
	// Start listening for new peers
	go func() {
		for newPeer := range connectionManager.ConnectedPeerChannel {
			app.handleConnectedPeer(newPeer)
		}
	}()

	return app, nil
}

func (app *App) handleConnectedPeer(newPeer *peer.Peer) {
	// TODO: Reject peer if already connected / in rejected peer list?

	// TODO: Handle application level logic of receiving chat room information,
	// dialing new peers, any listeners that need to be set?

	app.connectedPeersMutex.Lock()
	defer app.connectedPeersMutex.Unlock()

	sinkAudioFormatConversionDevice := device.NewAudioFormatConversionDevice(
		app.audioInputDevice.GetDeviceProperties(),
		newPeer.GetDeviceProperties(),
	)
	newPeer.SetStream(sinkAudioFormatConversionDevice.GetStream())
	sinkAudioFormatConversionDevice.SetStream(app.inputFanOutDevice.GetStream())

	sourceAudioFormatConversionDevice := device.NewAudioFormatConversionDevice(
		newPeer.GetDeviceProperties(),
		app.audioOutputDevice.GetDeviceProperties(),
	)
	sourceAudioAugmentationDevice := device.NewAudioAugmentationDevice(
		app.audioInputDevice.GetDeviceProperties(),
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

	app.audioInputDevice.Close()
	for _, peer := range app.connectedPeers {
		peer.Close()
	}
	app.outputFanInDevice.Close()
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
			inputDeviceProperties,
			appPeer.peer.GetDeviceProperties(),
		)

		appPeer.peer.SetStream(newSinkAudioFormatConversionDevice.GetStream())
		newSinkAudioFormatConversionDevice.SetStream(inputFanOutDevice.GetStream())

		appPeer.sinkAudioFormatConversionDevice.Close()
		appPeer.sinkAudioFormatConversionDevice = &newSinkAudioFormatConversionDevice
	}
	app.connectedPeersMutex.Unlock()

	// We made all devices correctly, now affect changes to App
	if app.audioInputDevice != nil {
		oldInputDevice := app.audioInputDevice
		defer oldInputDevice.Close()
	}
	app.audioInputDevice = inputDevice
	app.inputAugmentationDevice = inputAugmentationDevice
	app.inputFanOutDevice = &inputFanOutDevice

	// slog.Debug("updated set input device", "new properties", app.audioInputDevice.GetDeviceProperties())
}

func (app *App) SetOutputDevice(outputDevice audiodevice.AudioSinkDevice) {
	outputDeviceProperties := outputDevice.GetDeviceProperties()

	// TODO: Handle wait latency better
	// Maybe have this be dependency injected? Or read from Viper?
	outputFanInDevice := device.NewFanInDevice(outputDeviceProperties, 20*time.Millisecond)
	outputDevice.SetStream(outputFanInDevice.GetStream())

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
	app.audioOutputDevice = outputDevice

	// slog.Debug("updated set output device", "new properties", app.audioOutputDevice.GetDeviceProperties())
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
