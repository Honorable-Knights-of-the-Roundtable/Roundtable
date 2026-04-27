package audiodevice

import "github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/frame"

type DeviceProperties struct {
	Name        string
	SampleRate  int
	NumChannels int
	ID          int
	// For stereo input devices, which channel to extract when converting to mono.
	// 0 = left (Input 1), 1 = right (Input 2). Only used when NumChannels == 2.
	StereoChannelIndex int
}

// Interface for audio source device, e.g. microphones
//
// Source devices need only define some way to get data out of the device,
// which returns a channel (stream) of PCMFrames
type AudioSourceDevice interface {
	// Get the stream of this audio device.
	//
	// Raw audio data (as PCMFrames) will arrive on the returned channel.
	GetStream() <-chan frame.PCMFrame

	// Meaningfully close the AudioSourceDevice, including any cleanup of
	// memory and closing of channels.
	//
	// It is assumed that once closed, this device will transmit no more information.
	Close()

	GetDeviceProperties() DeviceProperties
}

// Interface for audio sink devices, e.g. speakers
//
// Sink devices need only define some way to consume data,
// taken as a channel (stream) of audio.PCMFrames
type AudioSinkDevice interface {
	// Set the source stream of this audio device.
	//
	// Raw audio data (as PCMFrames) will arrive on the given channel.
	//
	// When this stream is closed, it is assumed the device will be cleaned up
	// (memory will be freed, other channels will be closed, etc)
	SetStream(sourceStream <-chan frame.PCMFrame)

	GetDeviceProperties() DeviceProperties

	// Closing an AudioSinkDevice is not an easy task, because of the pipeline
	// techniques used in Roundtable. If a sink device that is actively receiving audio
	// is closed without closing the upstream source device, that source will
	// attempt to send on a closed channel, creating a panic.
	//
	// Instead, AudioSinkDevices should automatically close when the sourceStream
	// is closed, to affect a cascade of closures along a pipeline.
	//
	//
	// Close()
}
