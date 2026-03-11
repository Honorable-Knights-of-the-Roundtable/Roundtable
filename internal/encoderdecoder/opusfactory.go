package encoderdecoder

import (
	"errors"
	"time"
)

var (
	errInvalidFrameDuration      error = errors.New("given frame duration is not a valid OPUS frame duration")
	errInvalidBufferSafetyFactor error = errors.New("buffer safety factor must be strictly positive")
)

type OpusFactory struct {
	frameDuration      OPUSFrameDuration
	bufferSafetyFactor int
}

// Create a new OPUS factory that produces OPUSEncoderDecoder with the specified values.
//
// frameDuration determines how many samples are required to encode a frame of audio.
// longer frameDurations reduce network bandwidth, increase audioQuality, and increase latency.
//
// bufferSafetyFactor is a multiplier to all buffer lengths in the OPUSEncoderDecoder
// to prevent the overwriting of memory (encoded/decoded frames) before it can be consumed.
// A larger bufferSafetyFactor will result in a greater memory overhead (usually on the order of kilobytes)
// but more robust encoding and decoding, especially when working in highly parallelized, high
// throughput environments.
// For very small frameDurations, consider raising the safety factor.
// TODO:(JAKE) Why not just make the frameDuration parameter a OPUSFrameDuration
//              or can you not do that in Go?
func NewOpusFactory(
	frameDuration time.Duration,
	bufferSafetyFactor int,
) (OpusFactory, error) {
	opusFrameDuration := OPUSFrameDuration(frameDuration)
	switch opusFrameDuration {
	case OPUS_FRAME_DURATION_2_POINT_5_MS:
	case OPUS_FRAME_DURATION_5_MS:
	case OPUS_FRAME_DURATION_10_MS:
	case OPUS_FRAME_DURATION_20_MS:
	case OPUS_FRAME_DURATION_40_MS:
	case OPUS_FRAME_DURATION_60_MS:
	case OPUS_FRAME_DURATION_120_MS:
	default:
		return OpusFactory{}, errInvalidFrameDuration
	}

	if bufferSafetyFactor <= 0 {
		return OpusFactory{}, errInvalidBufferSafetyFactor
	}

	return OpusFactory{
		frameDuration:      opusFrameDuration,
		bufferSafetyFactor: bufferSafetyFactor,
	}, nil
}

func (f OpusFactory) NewOpusEncoderDecoder(sampleRate int, numChannels int) (*OpusEncoderDecoder, error) {
	return newOpusEncoderDecoder(sampleRate, numChannels, f.frameDuration, f.bufferSafetyFactor)
}
