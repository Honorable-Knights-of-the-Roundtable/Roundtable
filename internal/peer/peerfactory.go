package peer

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/encoderdecoder"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/frame"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/signalling"
	"github.com/pion/webrtc/v4"
)

type PeerFactory struct {
	logger *slog.Logger

	audioTrackRTPCodecCapability webrtc.RTPCodecCapability
	opusFactory                  encoderdecoder.OpusFactory
}

// Create a new PeerFactory.
//
// audioTrackRTPCodecCapability defines the preferred configuration to use for all audio tracks created on peer connections.
// See https://github.com/pion/webrtc for details on these options. Valid codecs are defined in github.com/Honorable-Knights-of-the-Roundtable/Roundtable/internal/networking/codecs.go
//
// logger allows for a child logger to be used specifically for this client. Create a child logger like:
// ```go
// childLogger := slog.Default().With(
//
//	slog.Group("PeerFactory"),
//
// )
// ```
// If no logger is given, slog.Default() is used.
func NewPeerFactory(
	audioTrackRTPCodecCapability webrtc.RTPCodecCapability,
	opusFactory encoderdecoder.OpusFactory,
	logger *slog.Logger,
) *PeerFactory {
	if logger == nil {
		logger = slog.Default()
	}

	factory := &PeerFactory{
		logger:                       logger,
		audioTrackRTPCodecCapability: audioTrackRTPCodecCapability,
		opusFactory:                  opusFactory,
	}

	return factory
}

// --------------------------------------------------------------------------------
// SETUP METHODS
// Methods to initialize important peer properties, including connection handlers

// Create an audio track and start streaming audio packets along it.streaming audio along it.
// This function:
// - Creates a new TrackLocalStaticSample, using the factory's CodecCapability
// - Attaches that sample to the peer's connection
// - Sets connectionAudioInputTrack on the Peer struct
//
// Once the connection has been fully established, the track's data should be checked
// for the negotiated codec and properties (e.g. sample rate, channels) and
// the peer's encoder/decoder should be set.
func (factory *PeerFactory) connectionAudioInputTrackSetup(core *peerCore) error {
	trackID := fmt.Sprintf("%s audio", core.Identifier().Uuid.String())
	streamID := fmt.Sprintf("%s audio stream", core.Identifier().Uuid.String())
	track, err := webrtc.NewTrackLocalStaticSample(
		factory.audioTrackRTPCodecCapability,
		trackID,
		streamID,
	)
	if err != nil {
		return err
	}

	_, err = core.connection.AddTrack(track)
	if err != nil {
		return err
	}

	core.setConnectionAudioInputTrack(track)

	return nil
}

// Handle the connection state change of a peerCore, i.e. the connection *before* full initialization
//
// onConnectedCallback is a function to be called when the peerCore is wrapped and finalized,
// allowing the peer to be returned by the connectionManager.
//
// onConnectedCallback is not called if wrapping the peer returns an error.
func (factory *PeerFactory) peerCoreConnectionStateChangeHandler(
	core *peerCore,
	onConnectedCallback func(*Peer),
) func(webrtc.PeerConnectionState) {
	var once sync.Once
	return func(pcs webrtc.PeerConnectionState) {
		core.logger.Debug("peer connection state change", "new state", pcs.String())
		switch pcs {
		case webrtc.PeerConnectionStateConnected:
			core.logger.Info("peer connection connected")
			once.Do(func() {
				wrappedPeer, err := factory.wrapPeerCore(core)
				if err == nil {
					onConnectedCallback(wrappedPeer)
				}
			})

		case webrtc.PeerConnectionStateFailed:
			core.logger.Info("peer connection failed")
			// TODO: Handle failed connection
			core.Close()

		case webrtc.PeerConnectionStateDisconnected:
			core.logger.Info("peer connection disconnected")
			// TODO: Handle disconnected connection
			core.Close()

		case webrtc.PeerConnectionStateClosed:
			core.logger.Info("peer connection closed")
			// TODO: Handle closed connection
			core.Close()
		}
	}
}

// --------------------------------------------------------------------------------
// PEER CREATION
// Methods to create new peers
// Split by offering peers and answering peers
// Offering peers create the meta-data channels (heartbeat, etc)

// Handle creation of a new peer on the offering side of the connection.
//
// Takes a created (but not processed) *webrtc.PeerConnection, and adds
// heartbeat and outgoing audio track.
//
// The given identifier is to represent the *remote* peer, not the local peer.
//
// If anything goes wrong, this method returns a nil Peer and a non-nil error.
func (factory *PeerFactory) NewOfferingPeer(
	identifier signalling.PeerIdentifier,
	connection *webrtc.PeerConnection,
	onConnectedCallback func(*Peer),
) error {
	core := newPeerCore(identifier, connection)
	core.connection.OnConnectionStateChange(
		factory.peerCoreConnectionStateChangeHandler(core, onConnectedCallback),
	)

	// --------------------------------------------------------------------------------
	// Audio track setup

	err := factory.connectionAudioInputTrackSetup(core)
	if err != nil {
		factory.logger.Error("error while creating new audio track for peer", "err", err)
		return err
	}

	// --------------------------------------------------------------------------------
	// Start heartbeat data channel for network latency check between peers

	heartbeatDataChannel, err := connection.CreateDataChannel("heartbeat", &webrtc.DataChannelInit{})
	if err != nil {
		core.logger.Error("error while creating heartbeat channel", "err", err)
		return err
	}
	// Unfortunately, creating a channel does not trigger the OnDataChannel callback defined in connectionBaseSetup
	core.setConnectionHeartbeatDataChannel(heartbeatDataChannel)

	return nil
}

// Handle creation of a new peer on the answering side of the connection.
//
// Takes a created (but not processed) *webrtc.PeerConnection, and adds
// an outgoing audio track. The heartbeat channel is made by the offering peer.
//
// If anything goes wrong, this method returns a nil Peer and a non-nil error.
func (factory *PeerFactory) NewAnsweringPeer(
	identifier signalling.PeerIdentifier,
	connection *webrtc.PeerConnection,
	onConnectedCallback func(*Peer),
) error {
	core := newPeerCore(identifier, connection)
	core.connection.OnConnectionStateChange(
		factory.peerCoreConnectionStateChangeHandler(core, onConnectedCallback),
	)

	// --------------------------------------------------------------------------------
	// Audio track setup

	err := factory.connectionAudioInputTrackSetup(core)
	if err != nil {
		factory.logger.Error("error while creating new audio track for peer", "err", err)
		return err
	}

	return nil
}

// Wrap a peerCore into a full-fledged Peer object, ready for audio I/O.
// This function is called by the connection state change handler attached to the peerCore
// in both the NewOfferingPeer and NewAnsweringPeer methods.
//
// See factory.peerCoreConnectionStateChangeHandler for the call of this function.
//
// Returns a Peer that wraps the newly connected peerCore. Returns an error if something goes wrong
func (factory *PeerFactory) wrapPeerCore(core *peerCore) (*Peer, error) {
	codec := core.connectionAudioInputTrack.Codec()
	core.logger.Info("wrapping peer core",
		"negotiatedMimeType", codec.MimeType,
		"negotiatedClockRate", codec.ClockRate,
		"negotiatedChannels", codec.Channels,
		"intendedChannels", factory.audioTrackRTPCodecCapability.Channels,
	)
	audioEncoderDecoder, err := factory.opusFactory.NewOpusEncoderDecoder(
		int(codec.ClockRate),
		int(codec.Channels),
	)
	if err != nil {
		core.logger.Error(
			"error during creation of audio encoder/decoder",
			"negotiatedCodec", codec,
			"err", err,
		)
		core.Close()
		return nil, err
	}

	wrappedPeer := &Peer{
		peerCore:            core,
		audioSinkChannel:    make(chan frame.PCMFrame),
		audioEncoderDecoder: audioEncoderDecoder,
	}

	// Shadow the connection state change handler to prevent wrapping the core more than once
	wrappedPeer.connection.OnConnectionStateChange(wrappedPeer.onConnectionStateChangeHandler)

	// Keep listening for the connection's track, but use the shadowed
	// method to call wrappedPeer.receiveAudioOutputHandler() when ready.
	//
	// If peerCore already received a track, this method will reject all future tracks.
	// (see internals, where a conditional early-returns if connectionAudioOutputTrack is set)
	wrappedPeer.connection.OnTrack(wrappedPeer.onTrackHandler)

	// If the peerCore already received a track (peerCore.onTrackHandler) then just listening for audio
	if wrappedPeer.connectionAudioOutputTrack != nil {
		wrappedPeer.receiveAudioOutputHandler()
	}
	return wrappedPeer, nil
}
