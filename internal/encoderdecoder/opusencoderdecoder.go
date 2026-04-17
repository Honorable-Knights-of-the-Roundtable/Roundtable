package encoderdecoder

import (
	"errors"
	"time"

	"github.com/Honorable-Knights-of-the-Roundtable/opus"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/frame"
)

type OpusEncoderDecoder struct {
	sampleRate  int
	numChannels int

	// The frame duration to use for OPUS encoding. Must be a very specific number,
	// hence defined by the OPUSFrameDuration enumeration.
	//
	// Defines how many samples are required to be present before encoding and sending
	// an encoded frame. See the documentation of the OPUSFrameDuration type for information.
	frameDuration OPUSFrameDuration

	encoder *opus.Encoder

	// The number of samples in a single encoding frame.
	// Equal to sampleRate * numChannels * frameDuration
	encodingFrameSize int

	// Buffer to hold incoming PCM Frames before using them for encoding.
	// Should be large enough to hold potentially many PCM frames,
	// in case encoding and forwarding the frames does not happen for some time.
	pcmFrameBuffer frame.PCMFrame
	// The current bounds of the unencoded data in the pcmFrameBuffer.
	// Head is always less than or equal to Tail.
	// Head defines the first sample that has *not* been encoded and sent.
	// Head therefore increments in sizes of the valid OPUS frame size.
	pcmFrameBufferHead int
	// Tail defines the last frame that has not been encoded and sent.
	// Since data may come in with arbitrary numbers of samples, Tail may
	// increment with arbitrary intervals.
	//
	// If a new PCM frame would ever push Tail beyond the end of the buffer,
	// the unencoded data is instead copied to the start of the buffer
	// and Head/Tail are set appropriately.
	pcmFrameBufferTail int

	// Buffer to hold encoded frames. Should be large enough to hold encoded frames
	// for potentially some time before being consumed, as the consumer of this device
	// may not consume immediately.
	encodedFrameBuffer frame.EncodedFrame
	// The index of the first byte left unsent.
	// If a new encoded frame would push past the end of the buffer, reset encodedFrameBufferTail
	// to the start of the buffer and copy the data over.
	encodedFrameBufferTail int
	// A buffer of encoded frames to reuse in the return of Encode
	encodedFrameReturnBuffer []frame.EncodedFrame

	decoder                *opus.Decoder
	decodedFrameBuffer     frame.PCMFrame
	decodedFrameBufferTail int
}

func newOpusEncoderDecoder(
	sampleRate int,
	numChannels int,
	frameDuration OPUSFrameDuration,
	bufferSafetyFactor int,
) (*OpusEncoderDecoder, error) {
	encoder, errEnc := opus.NewEncoder(sampleRate, numChannels, opus.Application(opus.AppVoIP))
	decoder, errDec := opus.NewDecoder(sampleRate, numChannels)
	if err := errors.Join(errEnc, errDec); err != nil {
		return nil, err
	}

	encodingFrameSize := int(frameDuration) * sampleRate * numChannels / int(time.Second)

	// The buffer needs to be large enough such that an incoming frame of PCM data
	// can be loaded into the pcmFrameBuffer without overwriting the already present data
	//
	// Recall that the frameDuration determines the number of samples possible before the data
	// is encoded and sent. The number of samples required to encode and send a frame is:
	// sampleRate * numChannels * frameDuration
	//
	// For safety, we introduce an additional safety factor of several multiples of this frame size
	// This incurs a static additional memory cost per device, but prevents allocations.
	//
	// For a concrete worst-case example, consider sampleRate = 48000 stereo (numChannels = 2) data with
	// frameDuration = OPUS_FRAME_DURATION_120_MS. Each frame would therefore require 11520 samples
	// which (at 32 bits per sample) is 46080 bytes, i.e. 45 kilobytes of memory. With a safety factor of 16
	// this gives up 720 kilobytes per buffer per device. This is likely negligible considering the
	// constant memory cost and safety in audio streaming.
	bufferSize := bufferSafetyFactor * encodingFrameSize

	return &OpusEncoderDecoder{
		sampleRate:               sampleRate,
		numChannels:              numChannels,
		frameDuration:            frameDuration,
		encoder:                  encoder,
		encodingFrameSize:        encodingFrameSize,
		pcmFrameBuffer:           make(frame.PCMFrame, bufferSize),
		encodedFrameBuffer:       make(frame.EncodedFrame, bufferSize),
		pcmFrameBufferHead:       0,
		pcmFrameBufferTail:       0,
		encodedFrameBufferTail:   0,
		encodedFrameReturnBuffer: make([]frame.EncodedFrame, bufferSafetyFactor),
		decoder:                  decoder,
		decodedFrameBuffer:       make(frame.PCMFrame, bufferSize),
		decodedFrameBufferTail:   0,
	}, nil
}

func (encdec OpusEncoderDecoder) GetFrameDuration() time.Duration {
	return time.Duration(encdec.frameDuration)
}

func (encdec *OpusEncoderDecoder) Encode(pcmData frame.PCMFrame) ([]frame.EncodedFrame, error) {
	if len(pcmData) > len(encdec.pcmFrameBuffer) {
		// Somehow, we have received so much data that we cannot even store it!
		// We *could* handle in parts, but instead just return an error.
		// TODO: Handle massive PCM frames.

		// slog.Debug(
		// 	"pcm frame too large",
		// 	"pcmFrame length", len(pcmData),
		// 	"pcmFrameBuffer length", len(encdec.pcmFrameBuffer),
		// 	"encdec.pcmFrameBufferHead", encdec.pcmFrameBufferHead,
		// 	"encdec.pcmFrameBufferTail", encdec.pcmFrameBufferTail,
		// )

		return nil, errors.New("pcm frame len larger than pcmFrameBuffer")
	}

	// If the incoming data would push Tail beyond the end of the buffer,
	// copy the unencoded data to the start of the buffer to make room.
	if len(pcmData)+encdec.pcmFrameBufferTail > len(encdec.pcmFrameBuffer) {
		copy(encdec.pcmFrameBuffer, encdec.pcmFrameBuffer[encdec.pcmFrameBufferHead:encdec.pcmFrameBufferTail])
		encdec.pcmFrameBufferTail = encdec.pcmFrameBufferTail - encdec.pcmFrameBufferHead
		encdec.pcmFrameBufferHead = 0
	}

	// Copy the new data in.
	copy(encdec.pcmFrameBuffer[encdec.pcmFrameBufferTail:], pcmData)
	encdec.pcmFrameBufferTail += len(pcmData)

	// While we can still encode something
	numEncodedFrames := 0
	for encdec.pcmFrameBufferTail-encdec.pcmFrameBufferHead > encdec.encodingFrameSize {
		// It's a little gross, but an encoding *should* make the data smaller, so...
		// It's okay to use the encodingFrameSize as a benchmark
		if encdec.encodedFrameBufferTail+encdec.encodingFrameSize > len(encdec.encodedFrameBuffer) {
			encdec.encodedFrameBufferTail = 0
		}

		// Encode the next frame into the encoding buffer, which we assume has enough space (and will not over run the buffer)
		nextEncodingFrame := encdec.pcmFrameBuffer[encdec.pcmFrameBufferHead : encdec.pcmFrameBufferHead+encdec.encodingFrameSize]
		numEncodedBytes, err := encdec.encoder.EncodeFloat32(
			nextEncodingFrame,
			encdec.encodedFrameBuffer[encdec.encodedFrameBufferTail:],
		)
		if err != nil {
			// Something went wrong, return what we have and give up
			// But be sure to skip past the "bad" frame, first!
			encdec.pcmFrameBufferHead += encdec.encodingFrameSize
			return encdec.encodedFrameReturnBuffer, err
		}

		// Add the newly encoded frame into the return buffer
		encdec.encodedFrameReturnBuffer[numEncodedFrames] = encdec.encodedFrameBuffer[encdec.encodedFrameBufferTail : encdec.encodedFrameBufferTail+numEncodedBytes]
		numEncodedFrames += 1
		// March the encoded frame buffer head forward to prevent overwriting the newly encoded bytes
		encdec.encodedFrameBufferTail += numEncodedBytes
		// March the head of the PCM Frame buffer forward by the encoding frame size, since these samples are now dealt with
		// This can never go past the end of the buffer, thanks to the loop condition
		encdec.pcmFrameBufferHead += encdec.encodingFrameSize
	}

	// slog.Debug("encoding finished",
	// 	"incomingDataLen", len(pcmData),
	// 	"pcmFrameBufferHead", encdec.pcmFrameBufferHead,
	// 	"pcmFrameBufferTail", encdec.pcmFrameBufferTail,
	// 	"encodedFrameBufferTail", encdec.encodedFrameBufferTail,
	// 	"numEncodedFrames", numEncodedFrames,
	// )
	return encdec.encodedFrameReturnBuffer[:numEncodedFrames], nil
}

func (encdec *OpusEncoderDecoder) Decode(encodedData frame.EncodedFrame) (frame.PCMFrame, error) {
	// Decode the incoming frame into the decodedFrame buffer.
	// This side of things is MUCH easier than encoding, since we may decode an arbitrary number of bytes
	// All we need to worry about is overrunning the buffer.
	//
	// We know that we *probably* will not be sent data that is longer than encodingFrameSize,
	// so we can use this as a good estimate for the buffer length. However... this isn't perfect.
	// If a frame is ever sent that is much too long, we may try to decode into a buffer that is too small
	//
	// Thankfully, this does not panic, but instead errors, so instead of crashing, we simply
	// miss that audio.

	if encdec.decodedFrameBufferTail+2*encdec.encodingFrameSize > len(encdec.decodedFrameBuffer) {
		encdec.decodedFrameBufferTail = 0
	}
	numDecodedSamples, err := encdec.decoder.DecodeFloat32(encodedData, encdec.decodedFrameBuffer[encdec.decodedFrameBufferTail:])
	if err != nil {
		return nil, err
	}
	numSamples := numDecodedSamples * encdec.numChannels
	// Copy into an owned slice before returning. The decodedFrameBuffer is a ring
	// buffer that will be overwritten on the next Decode call; callers (channels,
	// debug tap) may hold the slice across calls, so we must not alias the buffer.
	result := make(frame.PCMFrame, numSamples)
	copy(result, encdec.decodedFrameBuffer[encdec.decodedFrameBufferTail:encdec.decodedFrameBufferTail+numSamples])
	encdec.decodedFrameBufferTail += numSamples

	// slog.Debug("decoding frame",
	// 	"incomingDataLen", len(encodedData),
	// 	"encodingFrameSize", encdec.encodingFrameSize,
	// 	"decodedFrameBufferTail", encdec.decodedFrameBufferTail,
	// 	"numDecodedSamples", numDecodedSamples,
	// )
	return result, nil
}
