package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/peer"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/signalling"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

type joinResult struct {
	peers []string
	err   error
}

// ConnectionManager handles networking in the application using WebRTC
//
// Specifically, once instantiated, the ConnectionManager handles listening for connections,
// accepting new connections, passing connections back to be stored with the peer.
//
// Note that this class *only* handles creating connections, both offering and answering (to use the WebRTC terminology).
//
// Actually sending/receiving data on those connections should be handled by the webrtc.PeerConnections themselves,
// and closing those connections is handled by the Peer object under github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/peer/Peer
//
// The general flow of connections is as follows:
//
//  1. On startup, each client connects to the signalling server via WebSocket and registers itself with its UUID.
//
//  2. When a user wants to connect to a remote peer, they call ConnectionManager.Dial with the remote peer's identifier.
//
//  3. Dial creates a WebRTC offer and sends it to the signalling server as a WSMessage addressed to the remote peer.
//
//  4. The signalling server routes the offer to the remote peer. The remote peer's readLoop picks it up,
//     creates an answer, and sends it back via the signalling server.
//
//  5. Dial receives the answer, finalizes the PeerConnection, and returns.
//     Incoming connections arrive on ConnectedPeerChannel.
type ConnectionManager struct {
	logger *slog.Logger

	peerFactory         *peer.PeerFactory
	localPeerIdentifier signalling.PeerIdentifier

	webrtcAPI               *webrtc.API
	connectionConfiguration webrtc.Configuration
	connectionOfferOptions  webrtc.OfferOptions
	connectionAnswerOptions webrtc.AnswerOptions

	ws   *websocket.Conn
	wsMu sync.Mutex // gorilla websocket writes are not concurrent-safe

	// pendingDials maps offer UUIDs to channels waiting for an answer.
	// Registered before sending the offer to avoid a race where the answer
	// arrives before the channel is ready.
	pendingDials   map[uuid.UUID]chan signalling.SignallingAnswer
	pendingDialsMu sync.Mutex

	// pendingJoin receives the result of a JoinRoom call from the signalling server.
	pendingJoin   chan joinResult
	pendingJoinMu sync.Mutex

	// A channel to return established incoming connections.
	//
	// Once instantiated with NewConnectionManager, the caller should listen on
	// this channel for new connections, as this signals a peer has dialed,
	// authenticated, and is ready to send data.
	ConnectedPeerChannel chan *peer.Peer
}

func (manager *ConnectionManager) connectedPeerCallback(peer *peer.Peer) {
	manager.ConnectedPeerChannel <- peer
}

// Create a new ConnectionManager connected to the signalling server via WebSocket.
//
// signallingServerURL is the full WebSocket URL of the signalling server (e.g. "ws://127.0.0.1:1066/ws").
//
// peerFactory is a factory to make new peers when offering or answering connections.
//
// codecs defines the audio codecs to use for negotiation. At least one must match between peers.
//
// connectionConfiguration defines the configuration to use for all webrtc.PeerConnections.
// connectionOfferOptions and connectionAnswerOptions configure offering/answering sides respectively.
// See https://github.com/pion/webrtc for details.
//
// logger allows for a child logger to be used specifically for this client.
// If nil, slog.Default() is used.
func NewConnectionManager(
	signallingServerURL string,
	peerFactory *peer.PeerFactory,
	localPeerIdentifier signalling.PeerIdentifier,
	codecs []webrtc.RTPCodecCapability,
	connectionConfig webrtc.Configuration,
	connectionOfferOptions webrtc.OfferOptions,
	connectionAnswerOptions webrtc.AnswerOptions,
	logger *slog.Logger,
) (*ConnectionManager, error) {
	if logger == nil {
		logger = slog.Default()
	}

	mediaEngine := &webrtc.MediaEngine{}
	for i, codec := range codecs {
		err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: codec,
			PayloadType:        webrtc.PayloadType(100 + i), // See https://www.iana.org/assignments/rtp-parameters/rtp-parameters.xhtml
		}, webrtc.RTPCodecTypeAudio)
		if err != nil {
			logger.Error("error while registering codec", "codec", codec, "err", err)
		}
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	ws, _, err := websocket.DefaultDialer.Dial(signallingServerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to signalling server at %s: %w", signallingServerURL, err)
	}

	manager := &ConnectionManager{
		logger:                  logger,
		peerFactory:             peerFactory,
		localPeerIdentifier:     localPeerIdentifier,
		webrtcAPI:               api,
		connectionConfiguration: connectionConfig,
		connectionOfferOptions:  connectionOfferOptions,
		connectionAnswerOptions: connectionAnswerOptions,
		ws:                      ws,
		pendingDials:            make(map[uuid.UUID]chan signalling.SignallingAnswer),
		ConnectedPeerChannel:    make(chan *peer.Peer),
	}

	// Register our ID with the signalling server so it can route messages to us
	if err := manager.sendWSMessage(signalling.WSMessage{
		Type: "register",
		From: localPeerIdentifier.Uuid.String(),
	}); err != nil {
		ws.Close()
		return nil, fmt.Errorf("failed to register with signalling server: %w", err)
	}

	go manager.readLoop()

	return manager, nil
}

func (manager *ConnectionManager) sendWSMessage(msg signalling.WSMessage) error {
	manager.wsMu.Lock()
	defer manager.wsMu.Unlock()
	return manager.ws.WriteJSON(msg)
}

// readLoop reads messages from the signalling server WebSocket and dispatches them.
// Runs in its own goroutine for the lifetime of the ConnectionManager.
func (manager *ConnectionManager) readLoop() {
	for {
		var msg signalling.WSMessage
		if err := manager.ws.ReadJSON(&msg); err != nil {
			manager.logger.Error("signalling websocket read error, closing", "err", err)
			return
		}

		switch msg.Type {
		case "offer":
			go manager.handleIncomingOffer(msg)
		case "answer":
			manager.handleIncomingAnswer(msg)
		case "room-peers":
			manager.handleRoomPeers(msg)
		case "error":
			manager.handleSignallingError(msg)
		default:
			manager.logger.Warn("unknown signalling message type", "type", msg.Type)
		}
	}
}

// handleIncomingOffer processes an incoming SDP offer from a remote peer.
// Creates an answering PeerConnection, generates an answer, and sends it back via the signalling server.
func (manager *ConnectionManager) handleIncomingOffer(msg signalling.WSMessage) {
	requestLogger := manager.logger.WithGroup("request").With("requestUUID", uuid.New().String())
	requestLogger.Debug("new incoming session offer")

	var signallingOffer signalling.SignallingOffer
	if err := json.Unmarshal(msg.Data, &signallingOffer); err != nil {
		requestLogger.Error("error while unmarshalling signalling offer", "err", err)
		return
	}

	requestLogger = requestLogger.With("offerUUID", signallingOffer.OfferUUID.String())
	requestLogger.Info("session offer received")

	pc, err := manager.webrtcAPI.NewPeerConnection(manager.connectionConfiguration)
	if err != nil {
		requestLogger.Error("error while creating new peer connection for listening", "err", err)
		return
	}

	if err := manager.peerFactory.NewAnsweringPeer(signallingOffer.OfferingPeerID, pc, manager.connectedPeerCallback); err != nil {
		requestLogger.Error("error while creating new answering peer from factory", "err", err)
		return
	}

	if err := pc.SetRemoteDescription(signallingOffer.WebRTCSessionDescription); err != nil {
		requestLogger.Error("error while setting remote description", "err", err)
		pc.Close()
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		requestLogger.Error("error while creating answer", "err", err)
		pc.Close()
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		requestLogger.Error("error while setting local description", "err", err)
		pc.Close()
		return
	}

	<-webrtc.GatheringCompletePromise(pc)
	requestLogger.Debug("answering peer connection ICE resolved")

	signallingAnswer := signalling.SignallingAnswer{
		OfferUUID:                signallingOffer.OfferUUID,
		WebRTCSessionDescription: *pc.LocalDescription(),
	}
	answerData, err := json.Marshal(signallingAnswer)
	if err != nil {
		requestLogger.Error("error while marshalling answer", "err", err)
		pc.Close()
		return
	}

	if err := manager.sendWSMessage(signalling.WSMessage{
		Type: "answer",
		To:   signallingOffer.OfferingPeerID.Uuid.String(),
		From: manager.localPeerIdentifier.Uuid.String(),
		Data: answerData,
	}); err != nil {
		requestLogger.Error("error while sending answer via signalling server", "err", err)
		pc.Close()
	}
}

// handleRoomPeers delivers the peer list from the server to the waiting JoinRoom call.
func (manager *ConnectionManager) handleRoomPeers(msg signalling.WSMessage) {
	var data struct {
		Peers []string `json:"peers"`
	}
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		manager.logger.Error("error while unmarshalling room-peers", "err", err)
		return
	}
	manager.deliverJoinResult(joinResult{peers: data.Peers})
}

// handleSignallingError delivers a server-side error to the waiting JoinRoom call (if any).
func (manager *ConnectionManager) handleSignallingError(msg signalling.WSMessage) {
	var data struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		manager.logger.Error("error while unmarshalling signalling error", "err", err)
		return
	}
	manager.logger.Warn("signalling server error", "message", data.Message)
	manager.deliverJoinResult(joinResult{err: fmt.Errorf("%s", data.Message)})
}

func (manager *ConnectionManager) deliverJoinResult(result joinResult) {
	manager.pendingJoinMu.Lock()
	ch := manager.pendingJoin
	manager.pendingJoinMu.Unlock()

	if ch == nil {
		manager.logger.Warn("received signalling response but no join is pending")
		return
	}
	ch <- result
}

func (manager *ConnectionManager) SendDisconnectMessage(ctx context.Context) error {
	if err := manager.sendWSMessage(signalling.WSMessage{
		Type: "disconnect",
		From: manager.localPeerIdentifier.Uuid.String(),
		// Data: "",
	}); err != nil {
		return fmt.Errorf("failed to send disconnect message: %w", err)
	}
	return nil
}

// JoinRoom joins a named room on the signalling server and dials every peer already in it.
// Dials are made concurrently so a slow peer doesn't block the others.
// TODO(Jake):  This should probably return a `User` object or something, but I am unsure what that will look like
//
//	So for now it just returns a []string
func (manager *ConnectionManager) JoinRoom(ctx context.Context, roomName string) ([]string, error) {

	var peers []string
	joinData, err := json.Marshal(struct {
		Room string `json:"room"`
	}{Room: roomName})
	if err != nil {
		return peers, fmt.Errorf("failed to marshal join payload: %w", err)
	}

	// Register the channel before sending to avoid a race where the response
	// arrives before we're ready to receive it.
	ch := make(chan joinResult, 1)
	manager.pendingJoinMu.Lock()
	manager.pendingJoin = ch
	manager.pendingJoinMu.Unlock()
	defer func() {
		manager.pendingJoinMu.Lock()
		manager.pendingJoin = nil
		manager.pendingJoinMu.Unlock()
	}()

	if err := manager.sendWSMessage(signalling.WSMessage{
		Type: "join",
		From: manager.localPeerIdentifier.Uuid.String(),
		Data: joinData,
	}); err != nil {
		return peers, fmt.Errorf("failed to send join message: %w", err)
	}

	select {
	case <-ctx.Done():
		return peers, ctx.Err()
	case result := <-ch:
		if result.err != nil {
			return peers, result.err
		}
		peers = result.peers
		manager.logger.Info("joined room", "room", roomName, "existing_peers", len(peers))
		for _, peerUUID := range peers {
			id, err := uuid.Parse(peerUUID)
			if err != nil {
				manager.logger.Warn("invalid peer UUID in room-peers", "uuid", peerUUID)
				continue
			}
			remotePeer := signalling.PeerIdentifier{Uuid: id}
			go func(p signalling.PeerIdentifier) {
				if err := manager.Dial(ctx, p); err != nil {
					manager.logger.Error("failed to dial room peer", "peer", p.Uuid, "err", err)
				}
			}(remotePeer)
		}
	}

	return peers, nil
}

// handleIncomingAnswer correlates an incoming answer with the waiting Dial call via the offer UUID.
func (manager *ConnectionManager) handleIncomingAnswer(msg signalling.WSMessage) {
	var signallingAnswer signalling.SignallingAnswer
	if err := json.Unmarshal(msg.Data, &signallingAnswer); err != nil {
		manager.logger.Error("error while unmarshalling signalling answer", "err", err)
		return
	}

	manager.pendingDialsMu.Lock()
	ch, ok := manager.pendingDials[signallingAnswer.OfferUUID]
	manager.pendingDialsMu.Unlock()

	if !ok {
		manager.logger.Warn("received answer for unknown offer UUID", "offerUUID", signallingAnswer.OfferUUID)
		return
	}

	ch <- signallingAnswer
}

// Dial attempts to make a connection to a remote peer via the signalling server.
// Returns a non-nil error if the connection cannot be established or if ctx is cancelled.
func (manager *ConnectionManager) Dial(ctx context.Context, remotePeerIdentifier signalling.PeerIdentifier) error {
	offerUUID := uuid.New()
	requestLogger := manager.logger.WithGroup("request").With(
		"requestUUID", uuid.New().String(),
		"offerUUID", offerUUID.String(),
		"remotePeerUUID", remotePeerIdentifier.Uuid,
	)
	requestLogger.Info("new SDP offer started")

	pc, err := manager.webrtcAPI.NewPeerConnection(manager.connectionConfiguration)
	if err != nil {
		requestLogger.Error("error while creating new peer connection for dialing", "err", err)
		return err
	}

	if err := manager.peerFactory.NewOfferingPeer(remotePeerIdentifier, pc, manager.connectedPeerCallback); err != nil {
		requestLogger.Error("error while creating new offering peer from factory", "err", err)
		return err
	}

	offer, err := pc.CreateOffer(&manager.connectionOfferOptions)
	if err != nil {
		requestLogger.Error("error while creating offer", "err", err)
		pc.Close()
		return err
	}

	if err = pc.SetLocalDescription(offer); err != nil {
		requestLogger.Error("error while setting local description", "err", err)
		pc.Close()
		return err
	}

	// Wait for ICE gathering so the offer SDP includes our STUN-discovered public
	// IP candidate. Without this the answerer never learns our public address and
	// hole-punching cannot happen from their side.
	<-webrtc.GatheringCompletePromise(pc)
	requestLogger.Debug("offering peer ICE gathered")

	// Register the answer channel before sending the offer to avoid a race where
	// the answer arrives before we're ready to receive it.
	answerCh := make(chan signalling.SignallingAnswer, 1)
	manager.pendingDialsMu.Lock()
	manager.pendingDials[offerUUID] = answerCh
	manager.pendingDialsMu.Unlock()
	defer func() {
		manager.pendingDialsMu.Lock()
		delete(manager.pendingDials, offerUUID)
		manager.pendingDialsMu.Unlock()
	}()

	signallingOffer := signalling.SignallingOffer{
		AnsweringPeerID:          remotePeerIdentifier,
		OfferingPeerID:           manager.localPeerIdentifier,
		OfferUUID:                offerUUID,
		WebRTCSessionDescription: *pc.LocalDescription(), // post-gathering SDP with ICE candidates
	}
	offerData, err := json.Marshal(signallingOffer)
	if err != nil {
		requestLogger.Error("error while marshalling offer", "err", err)
		pc.Close()
		return err
	}

	if err := manager.sendWSMessage(signalling.WSMessage{
		Type: "offer",
		To:   remotePeerIdentifier.Uuid.String(),
		From: manager.localPeerIdentifier.Uuid.String(),
		Data: offerData,
	}); err != nil {
		requestLogger.Error("error while sending offer via signalling server", "err", err)
		pc.Close()
		return err
	}
	requestLogger.Debug("offer sent, waiting for answer")

	select {
	case <-ctx.Done():
		pc.Close()
		return ctx.Err()
	case signallingAnswer := <-answerCh:
		if err = pc.SetRemoteDescription(signallingAnswer.WebRTCSessionDescription); err != nil {
			requestLogger.Error("error while setting remote description", "err", err)
			pc.Close()
			return err
		}
	}

	requestLogger.Info("peer connection set")
	return nil
}
