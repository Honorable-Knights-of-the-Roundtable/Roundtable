package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"log/slog"
	"time"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/cmd/application"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/cmd/config"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/audioapi"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/encoderdecoder"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/networking"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/peer"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/utils"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
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
	configFilePath := flag.String("configFilePath", "offeringpeer/config.yaml", "Set the file path to the config file.")
	audioFile := flag.String("audioFile", "./assets/media.wav", "Set the file path to the audio file to play.")
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

	localPeerIdentifier := signalling.PeerIdentifier{
		Uuid:     uuid.New(),
		PublicIP: "", // In a real client, one would need to query a STUN server to retrieve this
	}
	connectionManager := initializeConnectionManager(localPeerIdentifier)

	dummyAPI := audioapi.NewDummyAudioIODeviceAPI(audiodevice.DeviceProperties{
		SampleRate:  8000,
		NumChannels: 1,
	})

	app, err := application.NewApp(
		dummyAPI,
		connectionManager,
	)
	if err != nil {
		slog.Error("error in making new app", "err", err)
		panic(err)
	}

	// --------------------------------------------------------------------------------
	// Make an offer to the answering client on 127.0.0.1:1067

	remotePeerIdentifier := signalling.PeerIdentifier{
		Uuid:     uuid.UUID{},
		PublicIP: "http://127.0.0.1:1067",
	}
	jsonPeerIdentifier, _ := json.Marshal(remotePeerIdentifier)
	encodedPeerIdentifier := base64.StdEncoding.EncodeToString(jsonPeerIdentifier)

	ctx := context.Background()
	if err := app.DialRemotePeer(ctx, encodedPeerIdentifier); err != nil {
		slog.Error("error during dial of answering client", "err", err)
		return
	}

	// --------------------------------------------------------------------------------

	inputDevice, err := device.NewFileAudioInputDevice(
		*audioFile,
		10*time.Millisecond,
	)
	if err != nil {
		slog.Error("error while opening file for audio input device", "err", err)
		return
	}

	// Test changing the input device *after* the peer has connected!
	// time.Sleep(10 * time.Second)

	app.SetInputDevice(&inputDevice)

	fileDuration, _ := inputDevice.Duration()
	inputDevice.Play(context.Background())
	time.Sleep(fileDuration + time.Second)

	// --------------------------------------------------------------------------------
	// Shut down peer and disconnect from remote
	slog.Info("Shutting down app")
	app.Close()
}
