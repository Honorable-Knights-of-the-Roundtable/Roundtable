package main

import (
	"context"
	"flag"
	"log/slog"
	"time"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/cmd/config"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/audioapi"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/encoderdecoder"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/networking"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/peer"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/utils"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice/device"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/signalling"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"github.com/spf13/viper"
)

func initializeConnectionManager(localPeerIdentifier signalling.PeerIdentifier) *networking.ConnectionManager {
	// avoid polluting the main namespace with the options and config structs

	codecs, err := utils.GetUserAuthorizedCodecs(viper.GetStringSlice("codecs"))
	if err != nil {
		slog.Error("error when loading user authorized codecs", "err", err)
		panic(err)
	}
	if len(codecs) == 0 {
		slog.Error("at least one codec must be authorized in config")
		panic("no codecs authorized")
	}
	slog.Debug("authorized codecs", "codecs", codecs)

	// --------------------------------------------------------------------------------

	opusFactory, err := encoderdecoder.NewOpusFactory(
		viper.GetDuration("OPUSFrameDuration"),
		viper.GetInt("OPUSBufferSafetyFactor"),
	)
	if err != nil {
		slog.Error("error when creating OPUS factory", "err", err)
		panic(err)
	}

	peerFactory := peer.NewPeerFactory(
		codecs[0],
		opusFactory,
		slog.Default(),
	)

	// --------------------------------------------------------------------------------

	webrtcConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: viper.GetStringSlice("ICEServers")}},
	}

	offerOptions := webrtc.OfferOptions{}
	answerOptions := webrtc.AnswerOptions{}

	manager, err := networking.NewConnectionManager(
		viper.GetString("signallingserver"),
		peerFactory,
		localPeerIdentifier,
		codecs,
		webrtcConfig,
		offerOptions,
		answerOptions,
		slog.Default(),
	)
	if err != nil {
		slog.Error("error when creating connection manager", "err", err)
		panic(err)
	}
	return manager
}

func main() {
	configFilePath := flag.String("configFilePath", "config.yaml", "Set the file path to the config file.")
	flag.Parse()

	config.LoadConfig(*configFilePath)
	logFilePointer, err := utils.ConfigureDefaultLogger(
		viper.GetString("loglevel"),
		viper.GetString("logfile"),
		slog.HandlerOptions{},
	)
	if err != nil {
		slog.Error("error while configuring default logger", "err", err)
		panic(err)
	}
	if logFilePointer != nil {
		defer logFilePointer.Close()
	}

	// --------------------------------------------------------------------------------
	// Set the local peer identifier to offer to peers

	localPeerIdentifier := signalling.PeerIdentifier{
		Uuid:     uuid.New(),
		PublicIP: "", // In a real client, one would need to query a STUN server to retrieve this
	}

	// --------------------------------------------------------------------------------
	// Create RtAudio input device (microphone)
	frameDuration := 20 * time.Millisecond

	api, err := audioapi.NewRtAudioApi(frameDuration)
	if err != nil {
		slog.Error("error while creating rtaudio api", "err", err)
		return
	}
	inputDevice, err := api.InitDefaultInputDevice()
	if err != nil {
		slog.Error("error while creating default input device", "err", err)
		return
	}

	// --------------------------------------------------------------------------------

	connectionManager := initializeConnectionManager(localPeerIdentifier)

	// --------------------------------------------------------------------------------
	// Make an offer to the answering client on 127.0.0.1:1067

	// In the real client, one would get this information as a BASE64 encoded JSON string,
	// then unmarshal into this struct. We forgo this for simplicity.
	remotePeerInformation := signalling.PeerIdentifier{
		Uuid:     uuid.UUID{},
		PublicIP: "http://127.0.0.1:1067",
	}

	ctx := context.Background()
	err = connectionManager.Dial(ctx, remotePeerInformation)
	if err != nil {
		slog.Error("error during dial of answering client", "err", err)
		return
	}
	// In a real client, we would have listening logic for any new connections
	// And treat any new connections identically, no matter if we offered or answered
	peer := <-connectionManager.ConnectedPeerChannel

	slog.Debug("established new connection", "codec", peer.GetDeviceProperties())

	// --------------------------------------------------------------------------------
	// Stream audio from microphone across the connection

	codec := peer.GetDeviceProperties()
	processedInput := device.NewAudioFormatConversionDevice(
		inputDevice.GetDeviceProperties(),
		codec,
	)

	processedInput.SetStream(inputDevice.GetStream())
	peer.SetStream(processedInput.GetStream())

	slog.Info("Streaming audio from microphone - press Ctrl+C to stop")

	// --------------------------------------------------------------------------------
	// Shut down peer and disconnect from remote
	slog.Info("Shutting down peer")
	// peer.Close()
	<-peer.GetContext().Done()
}
