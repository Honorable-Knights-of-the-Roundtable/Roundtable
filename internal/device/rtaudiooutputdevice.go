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
	name         string
	sampleRate   int
	numChannels  int
	dataChannel  <-chan frame.PCMFrame
	bufferFrames uint
	DeviceID     int

	// Internal buffer to handle streaming from channel to rtaudio callback
	frameQueue   chan frame.PCMFrame
	shutdownOnce sync.Once
	closeWg      sync.WaitGroup

	// sampleBuf is a pre-allocated FIFO that decouples the FanInDevice frame size
	// from the actual JACK/ALSA period size. Accessed only from the callback thread.
	sampleBuf     []float32
	sampleBufHead int
	sampleBufTail int

	// filler, when set via SetFiller, is called directly from the hardware callback
	// instead of pulling from frameQueue. Eliminates the software ticker and period-size
	// mismatch entirely.
	filler func([]float32)
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
	if channels > 2 {
		logger.Warn("output device reports unusual channel count, clamping to 2", "device", name, "reported", channels)
		channels = 2
	}
	bufferFrames := uint(int(sampleRate) * int(frameDuration) / int(time.Second))

	logger.Debug(
		"initialized rtaudio output device",
		"device", name,
		"sampleRate", sampleRate,
		"channels", channels,
		"bufferFrames", bufferFrames,
		"DeviceID", deviceInfo.ID,
	)

	// Pre-allocate 1 second of stereo audio to avoid RT-thread allocations.
	sampleBufCap := sampleRate * channels

	device := &RtAudioOutputDevice{
		logger:       logger,
		uuid:         uuid,
		DeviceID:     deviceInfo.ID,
		name:         deviceInfo.Name,
		audio:        audio,
		sampleRate:   sampleRate,
		numChannels:  channels,
		bufferFrames: bufferFrames,
		frameQueue:   make(chan frame.PCMFrame, 8), // Buffer to smooth out playback
		sampleBuf:    make([]float32, sampleBufCap),
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

	// Output callback function.
	// Uses d.sampleBuf as a FIFO to bridge the FanInDevice frame size to whatever
	// period size JACK/ALSA actually uses. Only called from the RtAudio callback
	// thread so sampleBuf needs no locking.
	cb := func(out rtaudiowrapper.Buffer, in rtaudiowrapper.Buffer, dur time.Duration, status rtaudiowrapper.StreamStatus) int {
		outputData := out.Float32()
		if outputData == nil {
			return 0
		}

		needed := len(outputData)
		written := 0

		for written < needed {
			available := d.sampleBufTail - d.sampleBufHead
			if available == 0 {
				// Refill from the frame queue.
				select {
				case pcmFrame, ok := <-d.frameQueue:
					if !ok {
						for i := written; i < needed; i++ {
							outputData[i] = 0
						}
						return 2
					}
					// Compact sampleBuf if the incoming frame won't fit at the tail.
					if len(pcmFrame) > len(d.sampleBuf)-d.sampleBufTail {
						copy(d.sampleBuf, d.sampleBuf[d.sampleBufHead:d.sampleBufTail])
						d.sampleBufTail -= d.sampleBufHead
						d.sampleBufHead = 0
					}
					// If the frame still doesn't fit the buffer is genuinely full;
					// drop the oldest samples to make room.
					if len(pcmFrame) > len(d.sampleBuf)-d.sampleBufTail {
						excess := len(pcmFrame) - (len(d.sampleBuf) - d.sampleBufTail)
						copy(d.sampleBuf, d.sampleBuf[excess:d.sampleBufTail])
						d.sampleBufTail -= excess
					}
					copy(d.sampleBuf[d.sampleBufTail:], pcmFrame)
					d.sampleBufTail += len(pcmFrame)
					available = d.sampleBufTail - d.sampleBufHead
				default:
					// No frame available — fill remaining output with silence.
					for i := written; i < needed; i++ {
						outputData[i] = 0
					}
					return 0
				}
			}

			toWrite := available
			if toWrite > needed-written {
				toWrite = needed - written
			}
			copy(outputData[written:], d.sampleBuf[d.sampleBufHead:d.sampleBufHead+toWrite])
			d.sampleBufHead += toWrite
			written += toWrite
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

// SetFiller wires this device to use a pull model: the given filler func is called
// directly from the hardware callback with the exact output slice to fill, eliminating
// the software ticker, frameQueue goroutine, and sampleBuf entirely. SetFiller also
// opens and starts the RT stream, so SetStream must NOT be called when using this method.
func (d *RtAudioOutputDevice) SetFiller(filler func([]float32)) {
	d.filler = filler

	params := rtaudiowrapper.StreamParams{
		DeviceID:     uint(d.DeviceID),
		NumChannels:  uint(d.numChannels),
		FirstChannel: 0,
	}

	cb := func(out rtaudiowrapper.Buffer, in rtaudiowrapper.Buffer, dur time.Duration, status rtaudiowrapper.StreamStatus) int {
		outputData := out.Float32()
		if outputData == nil {
			return 0
		}
		d.filler(outputData)
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
		Name:        d.name,
		SampleRate:  d.sampleRate,
		NumChannels: d.numChannels,
		ID:          d.DeviceID,
	}
}
