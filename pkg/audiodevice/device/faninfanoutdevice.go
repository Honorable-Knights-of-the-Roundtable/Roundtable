package device

import (
	"context"
	"sync"
	"time"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/frame"
)

// --------------------------------------------------------------------------------
// Fan Out Device (One to Many)

// A FanOutDevice is both an AudioSourceDevice and an AudioSinkDevice.
//
// Unlike other AudioSourceDevices, a call to GetStream does *not* return the
// singular output stream (sinkStream), but instead creates a *new* output stream unique to that call.
// Once added, there is no manual way to remove a sinkStream.
// A sinkStream is automatically removed if it does not accept a frame for a certain duration.
//
// The input stream (the sourceStream) is listened to and data forwarded (copied) to
// all sinkStreams. This may be an expensive process.
//
// Be sure to call SetStream before calls to GetStream to prevent the channels returned
// by GetStream from timing out before any data is ready to be received.
//
// Adding and removing sinkStreams is concurrency safe thanks to a mutex.
type FanOutDevice struct {
	deviceProperties audiodevice.DeviceProperties
	// A master context to cancel ALL sinks at once
	// sink channel contexts will be spawned as sub-contexts of this one.
	masterContext           context.Context
	masterContextCancelFunc context.CancelFunc

	sourceStream <-chan frame.PCMFrame

	sinksMutex sync.RWMutex
	sinks      []*fanOutSink
}

type fanOutSink struct {
	ctx       context.Context
	ctxCancel context.CancelFunc
	stream    chan frame.PCMFrame
}

// Return a new sink context related to the fanOutDevice.masterContext
// such that the returned context has a fresh timeout, but is still canceled
// if the masterContext is canceled
func (d *FanOutDevice) newSinkContext() (context.Context, context.CancelFunc) {
	const SINK_TIMEOUT = 5 * time.Second
	return context.WithTimeout(d.masterContext, SINK_TIMEOUT)
}

// Create a new FanOutDevice.
// The given device properties are for book-keeping only.
// The properties do not influence the behavior of the FanOutDevice in any way.
func NewFanOutDevice(properties audiodevice.DeviceProperties) FanOutDevice {
	masterContext, masterContextCancelFunction := context.WithCancel(context.Background())
	return FanOutDevice{
		deviceProperties:        properties,
		masterContext:           masterContext,
		masterContextCancelFunc: masterContextCancelFunction,
		sinks:                   make([]*fanOutSink, 0),
	}
}

func (d *FanOutDevice) GetDeviceProperties() audiodevice.DeviceProperties {
	return d.deviceProperties
}

// Set the stream of this device to copy data from.
// This method should be called only once, and once the sourceStream is closed
// then all sinkChannels are closed.
func (d *FanOutDevice) SetStream(sourceStream <-chan frame.PCMFrame) {
	d.sourceStream = sourceStream

	go func() {
		for data := range d.sourceStream {
			d.sinksMutex.Lock()
			// TODO: One channel blocking here will cause all channels to block.
			// Current select approach drops data to listeners who can't accept it... is that fine?
			for _, sink := range d.sinks {
				select {
				case sink.stream <- data:
					// We sent some data, let's refresh the sink context
					// First, cancel the old context
					sink.ctxCancel()
					// Then make a new one
					sink.ctx, sink.ctxCancel = d.newSinkContext()
				case <-sink.ctx.Done():
					// The sink didn't respond and has timed out, remove it
					close(sink.stream)
					numSinks := len(d.sinks)
					for i, s := range d.sinks {
						if s.stream == sink.stream {
							d.sinks[i] = d.sinks[numSinks-1]
							d.sinks = d.sinks[:numSinks-1]
							continue
						}
					}
				default:
					// We couldn't send data, but the sink hasn't timed out, just move on
				}
			}
			d.sinksMutex.Unlock()
		}
		// When sourceStream closes, close this device
		d.Close()
	}()
}

// Get a new stream from this fan out device.
//
// This method returns a new stream that data from the sourceChannel is copied to.
// The returned channel must consume data as it arrives and is fanned out.
// If enough frames are rejected by the channel (e.g. because it is blocking)
// then the channel is closed. The close occurs with a timeout, set to 5 seconds.
//
// Be sure to call this method AFTER setStream, otherwise you risk
// the returned channel closing from a timeout before any data can be written to it!
func (d *FanOutDevice) GetStream() <-chan frame.PCMFrame {
	d.sinksMutex.Lock()
	defer d.sinksMutex.Unlock()

	sinkCtx, sinkCtxCancel := d.newSinkContext()
	newSink := &fanOutSink{
		ctx:       sinkCtx,
		ctxCancel: sinkCtxCancel,
		stream:    make(chan frame.PCMFrame),
	}
	d.sinks = append(d.sinks, newSink)

	return newSink.stream
}

func (d *FanOutDevice) Close() {
	d.sinksMutex.Lock()
	defer d.sinksMutex.Unlock()
	d.masterContextCancelFunc()
	for _, sink := range d.sinks {
		sink.ctxCancel()
		close(sink.stream)
	}
	d.sinks = d.sinks[:0]
}

// --------------------------------------------------------------------------------
// Fan In Device (Many to One)

// A FanInDevice mixes audio from multiple source streams into a single output.
//
// Unlike other AudioSinkDevices, a call to SetStream does *not* set the singular input stream
// but instead adds the given stream to a list of sourceStreams which are all mixed together.
// A closed sourceStream's goroutine exits cleanly; the device itself is not closed.
//
// Audio is consumed via Fill, which is called directly from the hardware audio callback
// (pull model). Each call to Fill reads however many samples the hardware needs, mixing all
// available source audio and clipping to [-1.0, 1.0]. This means no software ticker, no
// intermediate channel, and no period-size mismatch — the hardware clock drives everything.
type FanInDevice struct {
	deviceProperties audiodevice.DeviceProperties

	shutdownOnce sync.Once

	sourcesMutex sync.RWMutex
	sources      []*fanInSource
}

type fanInSource struct {
	stream     <-chan frame.PCMFrame
	buffer     frame.PCMFrame
	mutex      sync.Mutex
	bufferHead int
	bufferTail int
}

func (source *fanInSource) listen() {
	go func() {
		for frame := range source.stream {
			source.mutex.Lock()
			// If new frame is big enough to handle the entire buffer by itself,
			// just overwrite all existing data
			if len(frame) > len(source.buffer) {
				copy(source.buffer, frame[len(frame)-len(source.buffer):])
				source.bufferHead = 0
				source.bufferTail = len(source.buffer)

				source.mutex.Unlock()
				continue
			}

			// Compact: move unread data to the start of the buffer to reclaim space at the tail.
			if len(frame)+source.bufferTail > len(source.buffer) {
				copy(source.buffer, source.buffer[source.bufferHead:source.bufferTail])
				source.bufferTail = source.bufferTail - source.bufferHead
				source.bufferHead = 0
			}

			// If the frame still doesn't fit after compaction (buffer is genuinely full),
			// drop the oldest samples to make room rather than panicking.
			if len(frame)+source.bufferTail > len(source.buffer) {
				excess := len(frame) + source.bufferTail - len(source.buffer)
				copy(source.buffer, source.buffer[excess:source.bufferTail])
				source.bufferTail -= excess
			}

			copy(source.buffer[source.bufferTail:], frame)
			source.bufferTail += len(frame)

			source.mutex.Unlock()
		}
	}()
}

// NewFanInDevice creates a new FanInDevice.
// The given device properties are a promise: it is expected that all
// incoming frames will have EXACTLY this format. Therefore, consider using
// an AudioFormatConversionDevice before this device.
func NewFanInDevice(properties audiodevice.DeviceProperties) *FanInDevice {
	return &FanInDevice{
		deviceProperties: properties,
		sources:          make([]*fanInSource, 0),
	}
}

func (d *FanInDevice) GetDeviceProperties() audiodevice.DeviceProperties {
	return d.deviceProperties
}

// SetStream adds a new source stream to be mixed into Fill output.
//
// The given stream is read from and combined with all other streams set this way.
func (d *FanInDevice) SetStream(sourceStream <-chan frame.PCMFrame) {
	d.sourcesMutex.Lock()
	defer d.sourcesMutex.Unlock()
	newFanInSource := &fanInSource{
		stream: sourceStream,
		buffer: make(frame.PCMFrame, d.deviceProperties.SampleRate*d.deviceProperties.NumChannels),
	}
	newFanInSource.listen()
	d.sources = append(d.sources, newFanInSource)
}

// Fill mixes all available source audio into dst.
// Called directly from the hardware audio callback — no allocation, no channel ops.
func (d *FanInDevice) Fill(dst []float32) {
	clear(dst)
	d.sourcesMutex.RLock()
	defer d.sourcesMutex.RUnlock()
	for _, source := range d.sources {
		source.mutex.Lock()
		available := source.bufferTail - source.bufferHead
		if available > 0 {
			toRead := min(available, len(dst))
			for i := 0; i < toRead; i++ {
				dst[i] += source.buffer[source.bufferHead+i]
			}
			source.bufferHead += toRead
		}
		source.mutex.Unlock()
	}
	for i, v := range dst {
		dst[i] = max(-1.0, min(1.0, v))
	}
}

// Close stops this device and discards all source references.
func (d *FanInDevice) Close() {
	d.shutdownOnce.Do(func() {
		d.sourcesMutex.Lock()
		defer d.sourcesMutex.Unlock()
		d.sources = d.sources[:0]
	})
}
