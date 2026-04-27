package device

import (
	"log/slog"
	"sync"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/frame"
	"github.com/oov/audio/resampler"
)

const (
	// To avoid reallocating for every source frame, reuse a buffer with "enough size".
	// Since we don't know the frame duration (number of samples) beforehand, we must estimate.
	//
	// As a rough estimate, 48000Hz stereo audio with a latency of 120ms is 11520 samples
	// So a buffer of 2**14 = 16384 should be enough for anything.
	bufferSize int = 16384
)

// Middle-man processing device to handle format mismatches
// between the source data format to the sink data format.
//
// e.g. if the source format is mono, but the sink format specifies stereo,
// this device will handle the conversion.
//
// This device is both a sink and a source!
type AudioFormatConversionDevice struct {
	// The stream that data *arrives on*
	// i.e. the stream that acts like a source, as it produces frames
	sourceStream     <-chan frame.PCMFrame
	sourceProperties audiodevice.DeviceProperties

	// The stream that data *leaves on*
	// i.e. the stream that acts like a sink, as it consumes frames
	sinkStream     chan frame.PCMFrame
	sinkProperties audiodevice.DeviceProperties

	// The functions to apply when processing the source data to sink format
	formatConversionFunctions []audioFormatConversionFunction

	shutdownOnce sync.Once
}

// Create a new AudioFormatConversionDevice by defining:
// - the source properties (the properties of the audio being fed into this device)
// - the sink properties (the properties of the audio leaving this device)
//
// Note one must still call SetStream, passing in the source channel,
// and GetStream, to receive the sink channel, to use this device, in an
// effort to remain consistent with the device interfaces.
//
// This device will only start converting once SetStream is called.
func NewAudioFormatConversionDevice(
	sourceProperties audiodevice.DeviceProperties,
	sinkProperties audiodevice.DeviceProperties,
) AudioFormatConversionDevice {
	formatConversionFunctions := make([]audioFormatConversionFunction, 0)

	if sourceProperties.NumChannels == 1 && sinkProperties.NumChannels == 2 {
		slog.Debug("adding mono to stereo")
		formatConversionFunctions = append(formatConversionFunctions, monoToStereo())
	}
	if sourceProperties.NumChannels == 2 && sinkProperties.NumChannels == 1 {
		slog.Debug("adding stereo to mono")
		formatConversionFunctions = append(formatConversionFunctions, stereoToMono(sourceProperties.StereoChannelIndex))
	}
	if sourceProperties.SampleRate != sinkProperties.SampleRate {
		slog.Debug("adding resampler")
		formatConversionFunctions = append(formatConversionFunctions, newResampleFunction(sourceProperties, sinkProperties))
	}

	return AudioFormatConversionDevice{
		sourceProperties:          sourceProperties,
		sinkProperties:            sinkProperties,
		sinkStream:                make(chan frame.PCMFrame),
		formatConversionFunctions: formatConversionFunctions,
	}
}

// --------------------------------------------------------------------------------
// AudioSourceDevice Interface

// Get the source stream of this audio device.
// Raw audio data (as PCMFrames) will arrive on the returned channel.
func (d *AudioFormatConversionDevice) GetStream() <-chan frame.PCMFrame {
	return d.sinkStream
}

// Meaningfully close the AudioSourceDevice, including any cleanup of
// memory and closing of channels.
//
// It is assumed that once closed, this device will transmit no more information.
func (d *AudioFormatConversionDevice) Close() {
	d.shutdownOnce.Do(func() {
		close(d.sinkStream)
	})
}

// WARNING:
// GetDeviceProperties of the AudioFormatConversionDevice returns the
// device properties of the LEAVING data. i.e. the data that exits this device!
//
// If you need the properties of the data entering this device, call GetSourceDeviceProperties()
func (d *AudioFormatConversionDevice) GetDeviceProperties() audiodevice.DeviceProperties {
	return d.sinkProperties
}

// --------------------------------------------------------------------------------
// AudioSinkDevice Interface

// Set the source channel of this audio device, i.e. where data comes from.
// Raw audio data (as PCMFrames) will arrive on the given channel.
//
// When this stream is closed, it is assumed the device will be cleaned up
// (memory will be freed, other channels will be closed, etc)
func (d *AudioFormatConversionDevice) SetStream(sourceStream <-chan frame.PCMFrame) {
	d.sourceStream = sourceStream
	go func() {
		defer func() {
			// If sinkStream was closed externally while this goroutine was
			// still sending (e.g. re-wiring during SetInputChannel), recover
			// so the whole program doesn't crash. Close() is idempotent.
			recover()
			d.Close()
		}()
		for pcmFrame := range d.sourceStream {
			for _, f := range d.formatConversionFunctions {
				pcmFrame = f(pcmFrame)
			}
			d.sinkStream <- pcmFrame
		}
	}()
}

func (d *AudioFormatConversionDevice) GetSourceDeviceProperties() audiodevice.DeviceProperties {
	return d.sourceProperties
}

// --------------------------------------------------------------------------------

// There is an expectation that an audioFormatConversionFunction will produce
// PCMFrames with different device properties than what are given in sourceFrame
type audioFormatConversionFunction func(sourceFrame frame.PCMFrame) frame.PCMFrame

func monoToStereo() audioFormatConversionFunction {
	buf := make(frame.PCMFrame, bufferSize)
	return func(sourceFrame frame.PCMFrame) frame.PCMFrame {
		for i, v := range sourceFrame {
			buf[2*i] = v
			buf[2*i+1] = v
		}
		return buf[:2*len(sourceFrame)]
	}
}

func stereoToMono(channelIndex int) audioFormatConversionFunction {
	buf := make(frame.PCMFrame, bufferSize)
	return func(sourceFrame frame.PCMFrame) frame.PCMFrame {
		if len(sourceFrame)%2 == 1 {
			sourceFrame = sourceFrame[:len(sourceFrame)-1]
		}
		for i := range len(sourceFrame) / 2 {
			buf[i] = sourceFrame[2*i+channelIndex]
		}
		return buf[:len(sourceFrame)/2]
	}
}

func newResampleFunction(sourceProperties audiodevice.DeviceProperties, sinkProperties audiodevice.DeviceProperties) audioFormatConversionFunction {
	if sinkProperties.NumChannels == 1 {
		r := resampler.New(1, sourceProperties.SampleRate, sinkProperties.SampleRate, 10)
		buf := make(frame.PCMFrame, bufferSize)
		return func(sourceFrame frame.PCMFrame) frame.PCMFrame {
			_, written := r.ProcessFloat32(0, sourceFrame, buf)
			return buf[:written]
		}
	} else {
		r := resampler.New(2, sourceProperties.SampleRate, sinkProperties.SampleRate, 10)
		leftSourceBuf := make(frame.PCMFrame, bufferSize/2)
		rightSourceBuf := make(frame.PCMFrame, bufferSize/2)
		leftSinkBuf := make(frame.PCMFrame, bufferSize/2)
		rightSinkBuf := make(frame.PCMFrame, bufferSize/2)
		buf := make(frame.PCMFrame, bufferSize)
		return func(sourceFrame frame.PCMFrame) frame.PCMFrame {
			if len(sourceFrame)%2 == 1 {
				sourceFrame = sourceFrame[:len(sourceFrame)-1]
			}

			// Decode to planar, sourceFrame is interleaved
			for i := range len(sourceFrame) / 2 {
				leftSourceBuf[i] = sourceFrame[2*i]
				rightSourceBuf[i] = sourceFrame[2*i+1]
			}

			// Process both channels
			_, written := r.ProcessFloat32(0, leftSourceBuf[:len(sourceFrame)/2], leftSinkBuf)
			r.ProcessFloat32(1, rightSourceBuf[:len(sourceFrame)/2], rightSinkBuf)

			// Interleave again
			for i := range written {
				buf[2*i] = leftSinkBuf[i]
				buf[2*i+1] = rightSinkBuf[i]
			}
			return buf[:2*written]
		}

	}
}
