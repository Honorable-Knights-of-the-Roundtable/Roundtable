package audioapi

import (
	"fmt"
	"log/slog"
	"time"

	internaldevice "github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/device"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/rtaudiowrapper"
	"github.com/google/uuid"
)

type RtAudioApi struct {
	logger        *slog.Logger
	audio         rtaudiowrapper.RtAudio
	frameDuration time.Duration
}

// Create a new RTAudioAPI, with a frameDuration to be given to all created devices
func NewRtAudioApi(frameDuration time.Duration) (*RtAudioApi, error) {
	uuid := uuid.New()
	logger := slog.Default().With(
		"rtaudio input device uuid", uuid,
	)

	audio, err := rtaudiowrapper.Create(rtaudiowrapper.APIUnspecified)
	if err != nil {
		logger.Error("failed to create rtaudio interface", "err", err)
	}

	return &RtAudioApi{
		logger:        logger,
		audio:         audio,
		frameDuration: frameDuration,
	}, nil
}

// Filters RtAudio devices to get only input
func (api *RtAudioApi) InputDevices() []AudioIODevice {
	devices, err := api.audio.Devices()
	if err != nil {
		api.logger.Error("Failed to initialise devices")
		return nil
	}

	inputDevices := make([]AudioIODevice, 0)

	for _, d := range devices {
		if d.NumInputChannels > 0 {
			inputDevice := AudioIODevice{
				ID:   d.ID,
				Name: d.Name,
				DeviceProperties: audiodevice.DeviceProperties{
					SampleRate:  int(d.PreferredSampleRate),
					NumChannels: d.NumInputChannels,
				},
			}
			inputDevices = append(inputDevices, inputDevice)
		}
	}

	return inputDevices
}

// Filters RtAudio devices to get only output
func (api *RtAudioApi) OutputDevices() []AudioIODevice {
	devices, err := api.audio.Devices()
	if err != nil {
		api.logger.Error("Failed to initialise devices")
		return nil
	}

	outputDevices := make([]AudioIODevice, 0)

	for _, d := range devices {
		if d.NumOutputChannels > 0 {
			outputDevice := AudioIODevice{
				ID:   d.ID,
				Name: d.Name,
				DeviceProperties: audiodevice.DeviceProperties{
					SampleRate:  int(d.PreferredSampleRate),
					NumChannels: d.NumOutputChannels,
				},
			}
			outputDevices = append(outputDevices, outputDevice)
		}
	}

	return outputDevices
}

// NewRtAudioInputDevice creates a new RtAudioInputDevice using the default input device.
// bufferFrames determines the size of audio chunks (typically 512 or 1024).
func (api *RtAudioApi) InitInputDeviceFromID(ioDevice AudioIODevice) (audiodevice.AudioSourceDevice, error) {
	audio, err := rtaudiowrapper.Create(rtaudiowrapper.APIUnspecified)
	if err != nil {
		slog.Error("failed to create rtaudio interface", "err", err)
		return nil, fmt.Errorf("failed to create audio interface: %w", err)
	}

	// Find Picked Device
	devices, err := audio.Devices()
	if err != nil {
		slog.Error("failed to get devices", "err", err)
		return nil, fmt.Errorf("failed to get devices: %w", err)
	}

	var currentDevice *rtaudiowrapper.DeviceInfo
	for _, d := range devices {

		if d.ID == ioDevice.ID {
			currentDevice = &d
			break
		}
	}
	if currentDevice == nil {
		return nil, fmt.Errorf("device with ID %d not found", ioDevice.ID)
	}

	device, err := internaldevice.NewRtAudioInputDevice(currentDevice, api.frameDuration, audio)
	if err != nil {
		return nil, err
	}

	return device, nil
}

// NewRtAudioInputDevice creates a new RtAudioInputDevice using the default input device.
// bufferFrames determines the size of audio chunks (typically 512 or 1024).
func (api *RtAudioApi) InitDefaultInputDevice() (audiodevice.AudioSourceDevice, error) {
	defaultInputDevice := api.audio.DefaultInputDevice()
	return api.InitInputDeviceFromID(AudioIODevice{
		ID:   defaultInputDevice.ID,
		Name: defaultInputDevice.Name,
		DeviceProperties: audiodevice.DeviceProperties{
			SampleRate:  int(defaultInputDevice.PreferredSampleRate),
			NumChannels: defaultInputDevice.NumInputChannels,
		},
	})
}

// NewRtAudioOutputDevice creates a new RtAudioOutputDevice using the default output device.
// sampleRate and numChannels define the expected audio format.
// bufferFrames determines the size of audio chunks (typically 512 or 1024).
func (api *RtAudioApi) InitOutputDeviceFromID(ioDevice AudioIODevice) (audiodevice.AudioSinkDevice, error) {
	audio, err := rtaudiowrapper.Create(rtaudiowrapper.APIUnspecified)
	if err != nil {
		slog.Error("failed to create rtaudio interface", "err", err)
		return nil, fmt.Errorf("failed to create audio interface: %w", err)
	}

	devices, err := audio.Devices()
	if err != nil {
		slog.Error("failed to get devices", "err", err)
		return nil, fmt.Errorf("failed to get devices: %w", err)
	}

	for _, d := range devices {
		fmt.Println(d)
	}

	var currentDevice *rtaudiowrapper.DeviceInfo
	for _, d := range devices {

		if d.ID == ioDevice.ID {
			currentDevice = &d
			break
		}
	}
	if currentDevice == nil {
		return nil, fmt.Errorf("device with ID %d not found", ioDevice.ID)
	}

	device, err := internaldevice.NewRtAudioOutputDevice(currentDevice, api.frameDuration, audio)
	if err != nil {
		return nil, err
	}
	return device, nil
}

func (api *RtAudioApi) InitDefaultOutputDevice() (audiodevice.AudioSinkDevice, error) {
	defaultOutputDevice := api.audio.DefaultOutputDevice()
	return api.InitOutputDeviceFromID(AudioIODevice{
		ID:   defaultOutputDevice.ID,
		Name: defaultOutputDevice.Name,
		DeviceProperties: audiodevice.DeviceProperties{
			SampleRate:  int(defaultOutputDevice.PreferredSampleRate),
			NumChannels: defaultOutputDevice.NumInputChannels,
		},
	})
}
