package device

import (
	"log/slog"
	"sync"

	"github.com/Honorable-Knights-of-the-Roundtable/rnnoise"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/frame"
)

// RNNoiseDevice is a passthrough audio device that applies RNNoise noise suppression.
// It implements both AudioSourceDevice and AudioSinkDevice, sitting between the
// microphone input and the downstream pipeline.
//
// RNNoise requires exactly 480 mono samples per frame at 48kHz. Incoming frames
// of any size are buffered and split into 480-sample chunks automatically.
//
// Only mono 48kHz audio is supported. For other configurations the device passes
// audio through unmodified and logs a warning.
type RNNoiseDevice struct {
	deviceProperties audiodevice.DeviceProperties

	sourceStream <-chan frame.PCMFrame
	sinkStream   chan frame.PCMFrame

	denoiser *rnnoise.Denoiser

	// leftover holds samples not yet assembled into a full 480-sample RNNoise frame.
	leftover []float32

	shutdownOnce sync.Once
}

// NewRNNoiseDevice creates a new RNNoiseDevice for the given device properties.
// Returns an error if the underlying RNNoise state cannot be allocated.
func NewRNNoiseDevice(deviceProperties audiodevice.DeviceProperties) (*RNNoiseDevice, error) {
	denoiser, err := rnnoise.NewDenoiser()
	if err != nil {
		return nil, err
	}

	if deviceProperties.SampleRate != 48000 || deviceProperties.NumChannels != 1 {
		slog.Warn("RNNoiseDevice: RNNoise requires mono 48kHz audio; audio will be passed through unmodified",
			"sampleRate", deviceProperties.SampleRate,
			"numChannels", deviceProperties.NumChannels,
		)
	}

	return &RNNoiseDevice{
		deviceProperties: deviceProperties,
		sinkStream:       make(chan frame.PCMFrame),
		denoiser:         denoiser,
		leftover:         make([]float32, 0, rnnoise.FrameSize),
	}, nil
}

// --------------------------------------------------------------------------------
// AudioSourceDevice interface

func (d *RNNoiseDevice) GetStream() <-chan frame.PCMFrame {
	return d.sinkStream
}

func (d *RNNoiseDevice) GetDeviceProperties() audiodevice.DeviceProperties {
	return d.deviceProperties
}

func (d *RNNoiseDevice) Close() {
	d.shutdownOnce.Do(func() {
		d.denoiser.Destroy()
		close(d.sinkStream)
	})
}

// --------------------------------------------------------------------------------
// AudioSinkDevice interface

func (d *RNNoiseDevice) SetStream(sourceStream <-chan frame.PCMFrame) {
	d.sourceStream = sourceStream
	go func() {
		for pcmFrame := range sourceStream {
			d.processIncoming(pcmFrame)
		}
		d.Close()
	}()
}

// --------------------------------------------------------------------------------

// processIncoming buffers the incoming samples and forwards them downstream in
// 480-sample RNNoise-processed chunks. Any leftover samples that don't fill a
// full chunk are held until the next call.
func (d *RNNoiseDevice) processIncoming(incoming frame.PCMFrame) {
	// Combine leftover from previous frame with new samples.
	buf := append(d.leftover, incoming...)

	i := 0
	for i+rnnoise.FrameSize <= len(buf) {
		chunk := buf[i : i+rnnoise.FrameSize]
		denoised := d.denoiser.ProcessFrame(chunk)
		d.sinkStream <- frame.PCMFrame(denoised)
		i += rnnoise.FrameSize
	}

	// Save any remaining samples for the next incoming frame.
	remainder := buf[i:]
	if len(remainder) == 0 {
		d.leftover = d.leftover[:0]
	} else {
		d.leftover = make([]float32, len(remainder))
		copy(d.leftover, remainder)
	}
}
