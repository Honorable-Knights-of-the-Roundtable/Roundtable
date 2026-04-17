package device

import (
	"log/slog"
	"sync"
	"time"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/frame"
	"github.com/Honorable-Knights-of-the-Roundtable/rtaudiowrapper"
	"github.com/google/uuid"
)

// RtAudioOutputDevice is an AudioOutputDevice that plays audio to speakers using RtAudio.
// It implements the AudioSinkDevice interface.
type RtAudioOutputDevice struct {
	logger *slog.Logger
	uuid   uuid.UUID

	audio        rtaudiowrapper.RtAudio
	sampleRate   int
	numChannels  int
	dataChannel  <-chan frame.PCMFrame
	bufferFrames uint
	DeviceID     int

	// Internal buffer to handle streaming from channel to rtaudio callback
	frameQueue   chan frame.PCMFrame
	shutdownOnce sync.Once
	closeWg      sync.WaitGroup
}

func NewRtAudioOutputDevice(
	deviceInfo *rtaudiowrapper.DeviceInfo,
	frameDuration time.Duration,
	audio rtaudiowrapper.RtAudio,
) (*RtAudioOutputDevice, error) {
	uuid := uuid.New()
	logger := slog.Default().With(
		"rtaudio output device uuid", uuid,
	)

	name := deviceInfo.Name
	sampleRate := int(deviceInfo.PreferredSampleRate)
	channels := deviceInfo.NumOutputChannels
	bufferFrames := uint(int(sampleRate) * int(frameDuration) / int(time.Second))

	logger.Debug(
		"initialized rtaudio output device",
		"device", name,
		"sampleRate", sampleRate,
		"channels", channels,
		"bufferFrames", bufferFrames,
		"DeviceID", deviceInfo.ID,
	)

	device := &RtAudioOutputDevice{
		logger:       logger,
		uuid:         uuid,
		DeviceID:     deviceInfo.ID,
		audio:        audio,
		sampleRate:   sampleRate,
		numChannels:  channels,
		bufferFrames: bufferFrames,
		frameQueue:   make(chan frame.PCMFrame, 8), // Buffer to smooth out playback
	}
	return device, nil
}

// SetStream sets the source channel for audio data and starts playback.
// This method starts the RtAudio stream and begins consuming PCM frames from the channel.
func (d *RtAudioOutputDevice) SetStream(sourceChannel <-chan frame.PCMFrame) {
	d.dataChannel = sourceChannel

	// Set up stream parameters for output
	params := rtaudiowrapper.StreamParams{
		DeviceID:     uint(d.DeviceID),
		NumChannels:  uint(d.numChannels),
		FirstChannel: 0,
	}

	// Output callback function
	cb := func(out rtaudiowrapper.Buffer, in rtaudiowrapper.Buffer, dur time.Duration, status rtaudiowrapper.StreamStatus) int {
		// d.logger.Debug("sending output from: ", "DeviceID", d.DeviceID)
		outputData := out.Float32()
		if outputData == nil {
			return 0
		}

		select {
		case pcmFrame, ok := <-d.frameQueue:
			if !ok {
				// Channel closed — fill with silence and stop
				for i := range outputData {
					outputData[i] = 0
				}
				return 2
			}
			copy(outputData, pcmFrame)
		default:
			// No frame ready — fill with silence to avoid an underflow stall
			for i := range outputData {
				outputData[i] = 0
			}
		}

		return 0
	}

	err := d.audio.Open(&params, nil, rtaudiowrapper.FormatFloat32, uint(d.sampleRate), d.bufferFrames, cb, nil)
	if err != nil {
		d.logger.Error("failed to open audio stream", "err", err)
		return
	}

	err = d.audio.Start()
	if err != nil {
		d.logger.Error("failed to start audio stream", "err", err)
		d.audio.Close()
		return
	}

	d.logger.Info("rtaudio output device started successfully")

	// Start goroutine to feed frames from source channel to internal queue
	d.closeWg.Add(1)
	go func() {
		defer d.closeWg.Done()
		defer close(d.frameQueue)

		for pcmFrame := range sourceChannel {
			select {
			case d.frameQueue <- pcmFrame:
			}
		}

		d.logger.Debug("source channel closed")
	}()
}

// Close stops the audio stream and cleans up resources.
func (d *RtAudioOutputDevice) Close() {
	d.logger.Debug("shutdown called")
	d.shutdownOnce.Do(func() {
		// Stop audio stream first
		if d.audio.IsRunning() {
			if err := d.audio.Stop(); err != nil {
				d.logger.Error("error stopping audio stream", "err", err)
			}
		}

		d.audio.Close()
		d.audio.Destroy()

		// Wait for the streaming goroutine to finish (with timeout)
		done := make(chan struct{})
		go func() {
			d.closeWg.Wait()
			close(done)
		}()

		select {
		case <-done:
			d.logger.Info("rtaudio output device closed")
		case <-time.After(1 * time.Second):
			d.logger.Warn("timeout waiting for output device to close")
		}
	})
}

// GetDeviceProperties returns the audio properties (sample rate, channels) of this device.
func (d *RtAudioOutputDevice) GetDeviceProperties() audiodevice.DeviceProperties {
	return audiodevice.DeviceProperties{
		SampleRate:  d.sampleRate,
		NumChannels: d.numChannels,
		ID:          d.DeviceID,
	}
}
