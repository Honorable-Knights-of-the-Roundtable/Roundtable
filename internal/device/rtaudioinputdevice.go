package device

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/frame"
	"github.com/Honorable-Knights-of-the-Roundtable/rtaudiowrapper"
	"github.com/google/uuid"
)

// RtAudioInputDevice is an AudioInputDevice that captures audio from a microphone using RtAudio.
// It implements the AudioSourceDevice interface.
type RtAudioInputDevice struct {
	logger *slog.Logger
	uuid   uuid.UUID

	audio        rtaudiowrapper.RtAudio
	sampleRate   uint
	numChannels  int
	dataChannel  chan frame.PCMFrame
	errorChannel chan error
	framesLost   atomic.Uint64
	DeviceID     int

	ctx           context.Context
	ctxCancelFunc context.CancelFunc
	shutdownOnce  sync.Once
}

func NewRtAudioInputDevice(
	deviceInfo *rtaudiowrapper.DeviceInfo,
	frameDuration time.Duration,
	audio rtaudiowrapper.RtAudio,
) (*RtAudioInputDevice, error) {
	uuid := uuid.New()
	logger := slog.Default().With(
		"rtaudio input device uuid", uuid,
	)

	name := deviceInfo.Name
	numChannels := deviceInfo.NumInputChannels
	sampleRate := deviceInfo.PreferredSampleRate

	ctx, ctxCancelFunc := context.WithCancel(context.Background())
	dataChannel := make(chan frame.PCMFrame)
	errorChannel := make(chan error, 5)

	bufferFrames := uint(int(sampleRate) * int(frameDuration) / int(time.Second))
	slog.Debug(
		"initialized rtaudio input device",
		"device", name,
		"sampleRate", sampleRate,
		"channels", numChannels,
		"bufferFrames", bufferFrames,
	)

	inputDevice := &RtAudioInputDevice{
		logger:        logger,
		uuid:          uuid,
		DeviceID:      deviceInfo.ID,
		audio:         audio,
		sampleRate:    sampleRate,
		numChannels:   numChannels,
		dataChannel:   dataChannel,
		errorChannel:  errorChannel,
		framesLost:    atomic.Uint64{},
		ctx:           ctx,
		ctxCancelFunc: ctxCancelFunc,
	}

	params := rtaudiowrapper.StreamParams{
		DeviceID:     uint(deviceInfo.ID),
		NumChannels:  uint(numChannels),
		FirstChannel: 0,
	}

	// Set up stream parameters

	options := rtaudiowrapper.StreamOptions{
		Flags: rtaudiowrapper.FlagsScheduleRealtime | rtaudiowrapper.FlagsMinimizeLatency,
	}

	cb := func(out, in rtaudiowrapper.Buffer, dur time.Duration, status rtaudiowrapper.StreamStatus) int {
		// Check if we should stop
		select {
		case <-ctx.Done():
			return 2 // Stop the stream
		default:
		}

		inputData := in.Float32()
		if inputData == nil {
			return 0
		}
		// Convert float32 slice to PCMFrame (already in correct format)
		pcmFrame := make(frame.PCMFrame, len(inputData))
		copy(pcmFrame, inputData)
		dataChannel <- pcmFrame

		// TODO: (JAKE) So this keeps happening, but I'm unsure what to do about it, the log pollutes the repl
		// so not reporting for now
		//Check for input overflow
		// if status&rtaudiowrapper.StatusInputOverflow != 0 {
		// 	logger.Warn("input overflow detected")
		// }

		return 0
	}

	err := audio.Open(nil, &params, rtaudiowrapper.FormatFloat32, sampleRate, bufferFrames, cb, &options)
	if err != nil {
		// TODO Unsure if it is ok to have the shared pointer to audio
		audio.Destroy()
		logger.Error("failed to open audio stream", "err", err)
		return nil, fmt.Errorf("failed to open audio stream: %w", err)
	}

	if err := audio.Start(); err != nil {
		audio.Close()
		audio.Destroy()
		logger.Error("failed to start audio stream", "err", err)
		return nil, fmt.Errorf("failed to start audio stream: %w", err)
	}

	logger.Info("rtaudio input device started successfully")

	return inputDevice, nil
}

// GetStream returns the channel that will receive PCM audio frames from the microphone.
func (d *RtAudioInputDevice) GetStream() <-chan frame.PCMFrame {
	return d.dataChannel
}

// Close stops the audio stream and cleans up resources.
func (d *RtAudioInputDevice) Close() {
	d.logger.Debug("shutdown called")
	d.shutdownOnce.Do(func() {
		if d.audio.IsRunning() {
			if err := d.audio.Stop(); err != nil {
				d.logger.Error("error stopping audio stream", "err", err)
			}
		}

		d.audio.Close()
		d.audio.Destroy()

		close(d.dataChannel)
		close(d.errorChannel)
		d.ctxCancelFunc()

		totalLost := d.framesLost.Load()
		if totalLost > 0 {
			d.logger.Warn("frames dropped during capture", "totalFrames", totalLost)
		}
		d.logger.Info("rtaudio input device closed")
	})
}

// GetDeviceProperties returns the audio properties (sample rate, channels) of this device.
func (d *RtAudioInputDevice) GetDeviceProperties() audiodevice.DeviceProperties {
	return audiodevice.DeviceProperties{
		SampleRate:  int(d.sampleRate),
		NumChannels: d.numChannels,
	}
}
