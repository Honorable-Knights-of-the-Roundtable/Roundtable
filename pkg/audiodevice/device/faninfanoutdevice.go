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

// A FanInDevice is both an AudioSourceDevice and an AudioSinkDevice.
//
// Unlike other AudioSinkDevices, a call to SetStream does *not* set the singular input stream
// but instead adds the given stream to a list of sourceStreams which are all checked for input
// and combined together to be returned along the sinkStream.
// A closed sourceStream is removed from the list, but does not close this device.
//
// The output stream (the sinkStream) gives the combined PCMFrames of the sourceStreams.
// Frames are not buffered, but read from the sourceStreams at once (streams with no data are skipped)
// and combined by simple addition (with clipping to values of +/- 1.0).
//
// The frameDuration defines how long the FanInDevice waits before
// sending a new frame of audio. This therefore also defines how many samples
// will exist in the produced frame (SampleRate*NumChannels*frameDuration/1Second).
// That is, a FanInDevice will always produce frames of a definite size!
type FanInDevice struct {
	deviceProperties audiodevice.DeviceProperties
	frameDuration    time.Duration

	masterContext           context.Context
	masterContextCancelFunc context.CancelFunc

	shutdownOnce sync.Once

	sourcesMutex sync.RWMutex
	sources      []*fanInSource

	sinkStream chan frame.PCMFrame
	sinkBuffer frame.PCMFrame
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

			// If we are about to overwrite the end of the buffer, loop back to start
			//
			// TODO: May cause jitter if head of buffer is overwritten, but...
			// very unlikely since audio should be consumed faster than this.
			if len(frame)+source.bufferTail > len(source.buffer) {
				copy(source.buffer, source.buffer[source.bufferHead:source.bufferTail])
				source.bufferTail = source.bufferTail - source.bufferHead
				source.bufferHead = 0
			}

			// Copy new data in --- we know there must be enough room after tail by above checks
			copy(source.buffer[source.bufferTail:], frame)
			source.bufferTail += len(frame)

			// data is consumed by fan in device, so that's all she wrote here

			source.mutex.Unlock()
		}
	}()
}

// Create a new FanInDevice.
// The given device properties are a promise: it is expected that all
// incoming frames will have EXACTLY this format. Therefore, consider using
// an AudioFormatConversionDevice before this device.
//
// The given frameDuration defines how long the FanInDevice waits before
// sending a new frame of audio. This therefore also defines how many samples
// will exist in the produced frame (SampleRate*NumChannels*frameDuration/1Second).
// That is, a FanInDevice will always produce frames of a definite size!
func NewFanInDevice(properties audiodevice.DeviceProperties, frameDuration time.Duration) *FanInDevice {
	masterContext, masterContextCancelFunction := context.WithCancel(context.Background())

	d := &FanInDevice{
		deviceProperties:        properties,
		frameDuration:           frameDuration,
		masterContext:           masterContext,
		masterContextCancelFunc: masterContextCancelFunction,
		sources:                 make([]*fanInSource, 0),
		sinkStream:              make(chan frame.PCMFrame),
		// The sink buffer should be large enough to hold PCM frames from any device.
		// It's incredibly unlikely that one full second of audio will ever arrive,
		// so leave enough room for this many samples.
		sinkBuffer: make(frame.PCMFrame, properties.SampleRate*properties.NumChannels),
	}
	d.startListening()

	return d
}

func (d *FanInDevice) startListening() {
	go func() {

		// We know how large a frame we expected based on the ticker
		expectedFrameLength := d.deviceProperties.NumChannels * d.deviceProperties.SampleRate * int(d.frameDuration) / int(time.Second)

		// Define the start index of the current output frame
		sinkBufferHead := 0

		listenTicker := time.NewTicker(d.frameDuration)
		defer listenTicker.Stop()
		for {
			select {
			case <-listenTicker.C:
			case <-d.masterContext.Done():
				return
			}

			// The index into the sink buffer at which the frame to be sent ends
			// The counterpart ot sinkBufferHead
			sinkBufferTail := sinkBufferHead + expectedFrameLength

			// Check if the largest possible buffer (something of expectedFrameSize)
			// would overrun the sinkBuffer
			if sinkBufferTail > len(d.sinkBuffer) {
				copy(d.sinkBuffer, d.sinkBuffer[sinkBufferHead:])
				sinkBufferHead = 0
				sinkBufferTail = expectedFrameLength
			}

			// Zero out the current frame to be sent
			clear(d.sinkBuffer[sinkBufferHead:sinkBufferTail])

			// Read frames in from each source (or at least as much as we can)
			d.sourcesMutex.Lock()
			for _, source := range d.sources {

				source.mutex.Lock()

				// If there is not enough data to fill the frame, don't take anything.
				if source.bufferTail-source.bufferHead < expectedFrameLength {
					source.mutex.Unlock()
					continue
				}

				// It is weird, but okay to unlock immediately after this,
				// since all we *really* care about in concurrency terms is the position of Tail
				// The underlying data may change, but that's just going to cause glitchy audio,
				// not differing frame lengths

				frame := source.buffer[source.bufferHead : source.bufferHead+expectedFrameLength]
				source.bufferHead += expectedFrameLength
				source.mutex.Unlock()

				for frameIndex := 0; frameIndex < expectedFrameLength; frameIndex += 1 {
					d.sinkBuffer[sinkBufferHead+frameIndex] += frame[frameIndex]
				}
			}
			d.sourcesMutex.Unlock()

			// We have read from every source, and have something to send.
			// Now the existing frame lives at d.sinkBuffer[sinkBufferHead:sinkBufferTail]
			// So perform a single clipping loop, then send, and update the tail

			for i := sinkBufferHead; i < sinkBufferTail; i += 1 {
				d.sinkBuffer[i] = max(-1.0, min(1.0, d.sinkBuffer[i]))
			}
			select {
			case <-d.masterContext.Done():
				return
			case d.sinkStream <- d.sinkBuffer[sinkBufferHead:sinkBufferTail]:
			default:
			}

			// Update the head to the tail, since we have sent the frame
			sinkBufferHead = sinkBufferTail
		}
		// This goroutine closes when the master context is cancelled,
		// which occurs when the Close function of this device is called.
	}()
}

func (d *FanInDevice) GetDeviceProperties() audiodevice.DeviceProperties {
	return d.deviceProperties
}

// Set a new stream of this device to receive data from.
//
// The given stream is read from and combined with all other streams set this way.
// When the given sourceStream is closed, it is removed from this device.
func (d *FanInDevice) SetStream(sourceStream <-chan frame.PCMFrame) {
	d.sourcesMutex.Lock()
	defer d.sourcesMutex.Unlock()
	newFanInSource := &fanInSource{
		stream:     sourceStream,
		buffer:     make(frame.PCMFrame, d.deviceProperties.SampleRate*d.deviceProperties.NumChannels),
		bufferHead: 0,
		bufferTail: 0,
	}
	newFanInSource.listen()

	d.sources = append(d.sources, newFanInSource)
}

// Get the output of this FanInDevice.
//
// The returned stream combines data from all source streams
// simply adding these streams together and clipping as required.
func (d *FanInDevice) GetStream() <-chan frame.PCMFrame {
	return d.sinkStream
}

// Close this device.
// Stop listening on the sourceStreams, and close the sinkStream
func (d *FanInDevice) Close() {
	d.shutdownOnce.Do(func() {
		d.sourcesMutex.Lock()
		defer d.sourcesMutex.Unlock()
		d.masterContextCancelFunc()
		close(d.sinkStream)
		d.sources = d.sources[:0]
	})
}
