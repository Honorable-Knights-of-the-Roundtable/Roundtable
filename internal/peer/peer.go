package peer

import (
	"io"
	"sync"
	"time"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/encoderdecoder"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/frame"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// The logical representation of a connected peer across the network.
//
// This struct is a wrapper peerCore (handling the actual connection) and
// channels in/out to handle audio from this client and the remote client.
//
// A Peer object should not be constructed directly. Instead, use a connectionManager
// with the corresponding Dial and ListenForConnection methods, which in turn call
// the injected PeerFactory methods NewOfferingPeer and NewAnsweringPeer methods respectively.
//
// The PeerFactory constructs a peerCore, which then waits to be fully connected
// (failed connections are closed gracefully) before being wrapped into a Peer,
// which finally are passed back to the connectionManager to be sent along the
// ConnectedPeerChannel, ready for processing by the application.
type Peer struct {
	*peerCore

	// --------------------------------------------------------------------------------
	// Audio Source / Sink fields

	// Audio data from this client is passed in on this channel to be sent to remote peers.
	// This field is the source into this peer. When working with this attribute, model the
	// peer as an AudioSinkDevice, i.e. something that consumes data coming on the audioSourceChannel.
	audioSourceChannel <-chan frame.PCMFrame

	// Audio data from remote peers is passed along this channel to be played on the audio output device.
	// This field is the sink out of this peer. When working with this attribute, model the
	// peer as an AudioSourceDevice, i.e. something that produces data on the audioSinkChannel.
	audioSinkChannel chan frame.PCMFrame

	// audioSourceChannel waitgroup, to ensure the receiveAudioSourceHandler go routine finishes
	audioSinkChannelWaitGroup sync.WaitGroup

	// Audio encoder / decoder to be used for this connection only
	audioEncoderDecoder *encoderdecoder.OpusEncoderDecoder

	// Optional tap for debug recording. When non-nil, a copy of every decoded frame
	// is sent here (non-blocking — frames are dropped if the tap can't keep up).
	debugTap chan<- frame.PCMFrame
}

// --------------------------------------------------------------------------------
// audiodevice.AudioSinkDevice Interface

// Set the audioSinkChannel of this peer. Data sent on the channel will be consumed by this device.
// The given channel should produce raw PCM frames from this clients audio input device (e.g. microphone)
func (peer *Peer) SetStream(sourceChannel <-chan frame.PCMFrame) {
	peer.audioSourceChannel = sourceChannel
	peer.sendAudioInputHandler()
}

// Shutdown this peer. Handles disconnecting to remote peer and stopping streams.
// Also called the peer.ctx cancel function, so peer.ctx.Done() will signal.
//
// This function is idempotent.
//
// Shadow of the peerCore method
func (peer *Peer) Close() {
	peer.shutdownOnce.Do(func() {
		peer.ctxCancelFunc()
		peer.connection.Close()
		peer.audioSinkChannelWaitGroup.Wait()

		if peer.audioSinkChannel != nil {
			close(peer.audioSinkChannel)
		}
	})
}

// The DeviceProperties of a Peer define both the source and sink properties.
// That is, the audio properties being sent to the peer (to be forwarded across the network)
// match the audio properties being received by the peer.
// Both of these values are determined by the negotiated codec.
func (peer *Peer) GetDeviceProperties() audiodevice.DeviceProperties {
	if peer.connectionAudioInputTrack == nil {
		return audiodevice.DeviceProperties{}
	}
	codec := peer.connectionAudioInputTrack.Codec()
	return audiodevice.DeviceProperties{
		SampleRate:  int(codec.ClockRate),
		NumChannels: int(codec.Channels),
	}
}

// --------------------------------------------------------------------------------
// audiodevice.AudioSourceDevice Interface

// Get the audioSourceChannel of this peer.
// The returned channel produces raw PCM Frames from the remote peer.
//
// When this peer is shutdown, the given channel is closed (hence, no data is to be sent on it anymore)
func (peer *Peer) GetStream() <-chan frame.PCMFrame {
	return peer.audioSinkChannel
}

// SetDebugTap sets a channel that receives a copy of every decoded PCM frame as it
// arrives from the remote peer, before any format conversion or mixing.
// Pass nil to disable. Frames are dropped (not blocked) if the tap channel is full.
func (peer *Peer) SetDebugTap(ch chan<- frame.PCMFrame) {
	peer.debugTap = ch
}

// --------------------------------------------------------------------------------
// CONNECTION HANDLERS
// Handlers for various aspects of the PeerConnection

// OnConnectionStateChange handler
// Handles changes of state on the connection, such as connection establishment and graceful shutdown
// This handler
func (peer *Peer) onConnectionStateChangeHandler(pcs webrtc.PeerConnectionState) {
	peer.logger.Debug("peer connection state change", "new state", pcs.String())
	switch pcs {
	case webrtc.PeerConnectionStateConnected:
		peer.logger.Info("peer connection connected")

	case webrtc.PeerConnectionStateFailed:
		peer.logger.Info("peer connection failed")
		// TODO: Handle failed connection
		peer.Close()

	case webrtc.PeerConnectionStateDisconnected:
		peer.logger.Info("peer connection disconnected")
		// TODO: Handle disconnected connection
		peer.Close()

	case webrtc.PeerConnectionStateClosed:
		peer.logger.Info("peer connection closed")
		// TODO: Handle closed connection
		peer.Close()
	}
}

// OnTrack handler
// Handle initialization of a new track, offered by the remote peer.
// Should start listening for packets from the remote peer, decoding them, and streaming them out.
//
// This handler shadows the peerCore handler, but includes the connectionAudioOutputTrack call
// If, when a Peer is constructed by wrapping a peerCore, the connectionAudioOutputTrack is already set,
// this function is not attached. Instead, connectionAudioOutputTrack is called directly (see PeerFactory.wrapPeerCore)
func (peer *Peer) onTrackHandler(tr *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
	if peer.connectionAudioOutputTrack != nil {
		peer.logger.Debug(
			"received track but connectionAudioOutputTrack already exists",
			"track ID", tr.ID(),
			"track kind", tr.Kind().String(),
		)
		return
	}

	peer.logger.Debug(
		"received track",
		"track ID", tr.ID(),
		"track kind", tr.Kind().String(),
	)

	peer.connectionAudioOutputTrack = tr
	peer.receiveAudioOutputHandler()
}

// audioSinkTrack onOpen handler
// Handle audio along the audioSinkChannel (e.g. from a microphone) by forwarding through the PeerConnection audio track.
func (peer *Peer) sendAudioInputHandler() {
	go func() {
		frameIndex := 0
		for {
			select {
			case <-peer.ctx.Done():
				return
			case pcmData, ok := <-peer.audioSourceChannel:
				if !ok {
					return
				}

				// peer.logger.Debug(
				// 	"new frame ready",
				// 	"frameIndex", frameIndex,
				// 	"pcmDataLen", len(pcmData),
				// 	"duration", duration,
				// )

				encodedFrames, err := peer.audioEncoderDecoder.Encode(pcmData)
				if err != nil {
					peer.logger.Error(
						"error while encoding pcm data",
						"frameIndex", frameIndex,
						"pcmDataLen", len(pcmData),
						"err", err,
					)
					continue
				}

				for _, frame := range encodedFrames {
					mediaSample := media.Sample{
						Data:      frame,
						Duration:  peer.audioEncoderDecoder.GetFrameDuration(),
						Timestamp: time.Now(),
					}

					peer.connectionAudioInputTrack.WriteSample(mediaSample)
					frameIndex += 1
				}
			}
		}
		// Once the audioInputChannel is closed or the context is canceled, this go routine will die
	}()
}

// Handle audio being received by the peer and forward along audioOutputChannel.
//
// When the context is canceled, this method returns gracefully as soon as the next packet arrives.
func (peer *Peer) receiveAudioOutputHandler() {
	peer.audioSinkChannelWaitGroup.Go(func() {
		frameIndex := 0
		for {
			select {
			case <-peer.ctx.Done():
				return
			default:
			}

			pkt, _, err := peer.connectionAudioOutputTrack.ReadRTP()
			if err != nil {
				if err == io.EOF {
					peer.logger.Debug("connection audio data track closed")
					return
				}
				peer.logger.Error(
					"error while receiving audio data from remote peer",
					"frameIndex", frameIndex,
					"err", err,
				)
				continue
			}

			decodedPayload, err := peer.audioEncoderDecoder.Decode(pkt.Payload)
			if err != nil {
				peer.logger.Error(
					"error while decoding packet from remote client",
					"frameIndex", frameIndex,
					"err", err,
				)
				continue
			}
			// TODO: Handle dropped and out-of-order packets?

			if peer.debugTap != nil {
				select {
				case peer.debugTap <- decodedPayload:
				default:
				}
			}

			select {
			case peer.audioSinkChannel <- decodedPayload:
			}
			// slog.Debug(
			// 	"frame sent",
			// 	"frameIndex", frameIndex,
			// 	"pcmDataLen", len(decodedPayload),
			// )

			frameIndex += 1
		}

		// This goroutine dies when the given context is canceled, which occurs in the peer.Close method
	})
}
