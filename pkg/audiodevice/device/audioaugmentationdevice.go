package device

import (
	"sync"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/frame"
)

// Middle-man processing device to handle audio augmentations,
// such as volume controls
// This device is both a sink and a source!
type AudioAugmentationDevice struct {
	deviceProperties audiodevice.DeviceProperties

	// The stream that data *arrives on*
	// i.e. the stream that acts like a source, as it produces frames
	sourceStream <-chan frame.PCMFrame

	// The stream that data *leaves on*
	// i.e. the stream that acts like a sink, as it consumes frames
	sinkStream chan frame.PCMFrame

	augmentationFunctions []audioAugmentationFunction
	volumeAdjustMagnitude float32

	shutdownOnce sync.Once
}

// Create a new AudioFormatConversionDevice, automatically adding
// audioAugmentationFunctions:
//   - volumeAdjust (controlled with AudioAugmentationDevice.SetVolume)
//     (0.0 for mute, no cap on volume, but beware of clipping)
//
// Note one must still call SetStream, passing in the source channel,
// and GetStream, to receive the sink channel, to use this device, in an
// effort to remain consistent with the device interfaces.
//
// This device will only start converting once SetStream is called.
func NewAudioAugmentationDevice(deviceProperties audiodevice.DeviceProperties) *AudioAugmentationDevice {
	device := &AudioAugmentationDevice{
		deviceProperties:      deviceProperties,
		volumeAdjustMagnitude: 1.0,
		sinkStream:            make(chan frame.PCMFrame),
	}

	formatConversionFunctions := []audioAugmentationFunction{
		device.volumeAdjust,
	}
	device.augmentationFunctions = formatConversionFunctions

	return device
}

// --------------------------------------------------------------------------------
// AudioSourceDevice Interface

// Get the source stream of this audio device.
// Raw audio data (as PCMFrames) will arrive on the returned channel.
func (d *AudioAugmentationDevice) GetStream() <-chan frame.PCMFrame {
	return d.sinkStream
}

// Meaningfully close the AudioSourceDevice, including any cleanup of
// memory and closing of channels.
//
// It is assumed that once closed, this device will transmit no more information,
// and will consume no more information.
func (d *AudioAugmentationDevice) Close() {
	d.shutdownOnce.Do(func() {
		close(d.sinkStream)
	})
}

// The device properties of the incoming and outgoing PCMFrames should be identical,
// so this serves as both Source and Sink Device Properties
func (d *AudioAugmentationDevice) GetDeviceProperties() audiodevice.DeviceProperties {
	return d.deviceProperties
}

// --------------------------------------------------------------------------------
// AudioSinkDevice Interface

// Set the source channel of this audio device, i.e. where data comes from.
// Raw audio data (as PCMFrames) will arrive on the given channel.
//
// When this stream is closed, it is assumed the device will be cleaned up
// (memory will be freed, other channels will be closed, etc)
func (d *AudioAugmentationDevice) SetStream(sourceStream <-chan frame.PCMFrame) {
	d.sourceStream = sourceStream
	go func() {
		defer func() {
			recover()
			d.Close()
		}()
		for pcmFrame := range d.sourceStream {
			for _, f := range d.augmentationFunctions {
				pcmFrame = f(pcmFrame)
			}
			d.sinkStream <- pcmFrame
		}
	}()
}

// --------------------------------------------------------------------------------
// Methods relating to changing the augmentation functions

// Set the volumeAdjustMagnitude to a new value. Must be non-negative.
// 0.0 means muted, 1.0 is natural scaling, technically uncapped but
// audio encoded as PCMFrames clip if values are made too large.
func (d *AudioAugmentationDevice) SetVolumeAdjustMagnitude(volumeAdjustMagnitude float32) {
	if volumeAdjustMagnitude < 0.0 {
		volumeAdjustMagnitude = 0.0
	}
	d.volumeAdjustMagnitude = volumeAdjustMagnitude
}

// Get the current volumeAdjustMagnitude.
func (d *AudioAugmentationDevice) GetVolumeAdjustMagnitude() float32 {
	return d.volumeAdjustMagnitude
}

// --------------------------------------------------------------------------------

// There is an expectation that an audioAugmentationFunction will produce
// PCMFrames with the same device properties as what is given in sourceFrame
//
// In fact, for many audioAugmentationFunctions, the returned PCMFrame
// is the exact same underlying memory in an effort to avoid reallocations.
type audioAugmentationFunction func(sourceFrame frame.PCMFrame) frame.PCMFrame

func (d *AudioAugmentationDevice) volumeAdjust(sourceFrame frame.PCMFrame) frame.PCMFrame {
	for i := range sourceFrame {
		sourceFrame[i] *= d.volumeAdjustMagnitude
		// TODO: Should we handle clipping here?
		// If audio does slip outside [-1.0, 1.0], the program seems to handle it (clips at speaker)
	}
	return sourceFrame
}
