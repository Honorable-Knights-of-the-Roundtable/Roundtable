package rnnoise

/*
#include <rnnoise.h>
#include <stdlib.h>
*/
import "C"
import (
	"fmt"
	"unsafe"
)

// FrameSize is the number of samples RNNoise processes per call.
// Fixed at 480 samples (10ms at 48kHz mono).
const FrameSize = 480

// Denoiser wraps a single RNNoise processing state.
//
// RNNoise is stateful — it must receive consecutive frames from the same audio
// stream to maintain context. Do not share a Denoiser across goroutines.
type Denoiser struct {
	state *C.DenoiseState
}

// NewDenoiser creates a new Denoiser using the built-in RNNoise model.
func NewDenoiser() (*Denoiser, error) {
	state := C.rnnoise_create(nil)
	if state == nil {
		return nil, fmt.Errorf("rnnoise_create returned nil")
	}
	return &Denoiser{state: state}, nil
}

// ProcessFrame denoises a single 480-sample mono frame.
//
// Input samples must be in the standard PCM float32 range [-1.0, 1.0].
// Returns a new 480-sample frame with noise removed, in the same range.
// Panics if len(in) != FrameSize.
func (d *Denoiser) ProcessFrame(in []float32) []float32 {
	if len(in) != FrameSize {
		panic(fmt.Sprintf("rnnoise: ProcessFrame requires exactly %d samples, got %d", FrameSize, len(in)))
	}

	// RNNoise expects samples in the range [-32768, 32767].
	cIn := make([]C.float, FrameSize)
	for i, v := range in {
		cIn[i] = C.float(v * 32768.0)
	}

	cOut := make([]C.float, FrameSize)
	C.rnnoise_process_frame(d.state, (*C.float)(unsafe.Pointer(&cOut[0])), (*C.float)(unsafe.Pointer(&cIn[0])))

	result := make([]float32, FrameSize)
	for i, v := range cOut {
		result[i] = float32(v) / 32768.0
	}
	return result
}

// Destroy frees the underlying RNNoise state. Call this when done.
func (d *Denoiser) Destroy() {
	if d.state != nil {
		C.rnnoise_destroy(d.state)
		d.state = nil
	}
}
