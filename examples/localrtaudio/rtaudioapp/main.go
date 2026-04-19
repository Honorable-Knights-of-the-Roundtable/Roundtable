package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/cmd/application"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/cmd/config"

	// "github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/device"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/audioapi"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/encoderdecoder"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/networking"
	"github.com/Honorable-Knights-of-the-Roundtable/rtaudiowrapper"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/peer"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/utils"

	// "github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice/device"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/signalling"
	"github.com/google/uuid"
	"github.com/pion/stun/v3"
	"github.com/pion/webrtc/v4"
	"github.com/spf13/viper"
)

func stunme() {
	// Resolve the STUN server address
	stunAddr := "stun.l.google.com:19302"

	// Open a UDP connection to the STUN server
	conn, err := net.Dial("udp4", stunAddr)
	if err != nil {
		log.Fatalf("failed to dial STUN server: %v", err)
	}
	defer conn.Close()

	// Create a new STUN client using the UDP connection
	client, err := stun.NewClient(conn)
	if err != nil {
		log.Fatalf("failed to create STUN client: %v", err)
	}
	defer client.Close()

	// Build a STUN Binding Request message
	msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest)

	var publicAddr stun.XORMappedAddress

	// Send the request and handle the response
	err = client.Do(msg, func(res stun.Event) {
		if res.Error != nil {
			log.Fatalf("STUN request failed: %v", res.Error)
		}

		// Extract the XOR-MAPPED-ADDRESS attribute from the response
		if err := publicAddr.GetFrom(res.Message); err != nil {
			log.Fatalf("failed to get XOR-MAPPED-ADDRESS: %v", err)
		}
	})
	if err != nil {
		log.Fatalf("failed to perform STUN transaction: %v", err)
	}

	fmt.Printf("Public IP:  http://%s:%d\n", publicAddr.IP, publicAddr.Port)
	fmt.Printf("Public Port: %d\n", publicAddr.Port)
	fmt.Printf("Full address: %s\n", publicAddr.String())
}

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

const defaultFile = "recordings/default.wav"
const micTestFile = "recordings/micTestFile.wav"

var currentFile string
var selectedDeviceID int = -1 // -1 means use default
var useLoopback bool = false  // Enable WASAPI loopback mode for system audio capture

func print_devices() {
	audio, err := rtaudiowrapper.Create(rtaudiowrapper.APIUnspecified)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create rtaudio device\n")
	}
	defer audio.Destroy()

	devices, err := audio.Devices()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s", err)
	}

	fmt.Printf("\nAvailable Audio Devices:\n")
	fmt.Printf("%-4s %-50s %-8s %-8s %-8s\n", "ID", "Name", "In", "Out", "Duplex")
	fmt.Printf("%s\n", strings.Repeat("-", 85))
	for i, device := range devices {
		inputCh := fmt.Sprintf("%d", device.NumInputChannels)
		outputCh := fmt.Sprintf("%d", device.NumOutputChannels)
		marker := ""
		if i == audio.DefaultInputDeviceId() {
			marker = " [DEFAULT IN]"
		}
		duplexCh := fmt.Sprintf("%d", device.NumDuplexChannels)
		fmt.Printf("%-4d %-50s %-8s %-8s %-8s%s\n", i, device.Name, inputCh, outputCh, duplexCh, marker)
	}
	fmt.Printf("Use 'select' to choose a device for recording\n")
}

func Record(outpath string) {
	audio, err := rtaudiowrapper.Create(rtaudiowrapper.APIUnspecified)
	if err != nil {
		log.Fatal(err)
	}
	defer audio.Destroy()

	devices, err := audio.Devices()
	for _, device := range devices {
		fmt.Println(device.String())
	}
	if err != nil {
		log.Fatal(err)
	}

	var inputDevice rtaudiowrapper.DeviceInfo

	// Normal input recording mode
	if selectedDeviceID >= 0 && selectedDeviceID < len(devices) {
		inputDevice = devices[selectedDeviceID]
	} else {
		inputDevice = audio.DefaultInputDevice()
		fmt.Printf("Recording from default device: %s\n", inputDevice.Name)
	}

	if inputDevice.NumInputChannels == 0 {
		log.Fatal("Selected device has no input channels. Choose a different device.")
	}

	// Determine channels based on mode
	channels := inputDevice.NumInputChannels
	sampleRate := inputDevice.PreferredSampleRate

	// Initialize recording data with a large buffer (e.g., 10 minutes worth)
	// Adjust this if you need longer recordings
	maxDuration := 600 // seconds (10 minutes)
	maxFrames := int(sampleRate) * maxDuration

	// Initialize recording data
	data := &rtaudiowrapper.RecordingData{
		Buffer:       make([]int16, maxFrames*channels),
		TotalFrames:  maxFrames,
		FrameCounter: 0,
		Channels:     channels,
	}

	// Setup stream parameters
	var inputParams *rtaudiowrapper.StreamParams
	var outputParams *rtaudiowrapper.StreamParams

	// Normal recording: only input params
	inputParams = &rtaudiowrapper.StreamParams{
		DeviceID:     uint(inputDevice.ID),
		NumChannels:  uint(channels),
		FirstChannel: 0,
	}
	outputParams = nil

	options := rtaudiowrapper.StreamOptions{
		Flags: rtaudiowrapper.FlagsScheduleRealtime | rtaudiowrapper.FlagsMinimizeLatency,
	}

	// Go callback function that replaces the C input_callback
	callbackCount := 0
	cb := func(out, in rtaudiowrapper.Buffer, dur time.Duration, status rtaudiowrapper.StreamStatus) int {
		callbackCount++

		// Get input buffer as int16 slice
		inputData := in.Int16()
		if inputData == nil {
			if callbackCount <= 5 {
				fmt.Printf("DEBUG: Callback %d - inputData is nil\n", callbackCount)
			}
			return 0
		}

		nFrames := in.Len()

		// Debug: Check audio levels periodically (every 50 callbacks ~= every 0.5 seconds at 48kHz)
		if callbackCount <= 5 || callbackCount%50 == 0 {
			var maxSample int16 = 0
			for i := range inputData {
				if inputData[i] > maxSample {
					maxSample = inputData[i]
				} else if -inputData[i] > maxSample {
					maxSample = -inputData[i]
				}
			}
			percentage := float64(maxSample) / 32767.0 * 100.0
			fmt.Printf("\nDEBUG: Callback %d - frames=%d, maxSample=%d (%.1f%% of max), status=%d\n",
				callbackCount, nFrames, maxSample, percentage, status)
		}

		// Calculate how many frames to copy (stop if we reach buffer limit)
		frames := nFrames
		if data.FrameCounter+nFrames > data.TotalFrames {
			frames = data.TotalFrames - data.FrameCounter
			if frames <= 0 {
				return 2 // Buffer full, stop recording
			}
		}

		// Copy data to our buffer
		offset := data.FrameCounter * data.Channels
		samplesToCopy := frames * data.Channels
		copy(data.Buffer[offset:offset+samplesToCopy], inputData[:samplesToCopy])
		data.FrameCounter += frames

		return 0
	}

	err = audio.Open(outputParams, inputParams, rtaudiowrapper.FormatInt16, sampleRate, uint(512), cb, &options)
	if err != nil {
		log.Fatal(err)
	}

	err = audio.Start()
	if err != nil {
		log.Fatal("Audio failed to start\n", err)
	}

	// Create a channel to signal when user wants to stop
	stopChan := make(chan struct{})

	// Start a goroutine to wait for user input
	go func() {
		bufio.NewReader(os.Stdin).ReadBytes('\n')
		close(stopChan)
	}()

	// Wait for recording to complete or user to stop
	for audio.IsRunning() {
		select {
		case <-stopChan:
			fmt.Printf("\nStopping recording...\n")
			audio.Stop()
			goto cleanup
		default:
			time.Sleep(100 * time.Millisecond)
			duration := float64(data.FrameCounter) / float64(sampleRate)
			fmt.Printf("\rRecording: %.1f seconds (%d frames)", duration, data.FrameCounter)
		}
	}
cleanup:
	fmt.Printf("\n\nRecording complete. Recorded %d frames (%.1f seconds).\n",
		data.FrameCounter, float64(data.FrameCounter)/float64(sampleRate))

	// Write WAV file
	fmt.Printf("Writing WAV file: %s\n", outpath)
	if err := rtaudiowrapper.WriteWavFile(outpath, data, uint32(sampleRate), 16); err != nil {
		fmt.Printf("Error writing WAV file: %v\n", err)
		return
	}
	fmt.Printf("Successfully wrote %s\n", outpath)
}

func printCommands() {
	fmt.Fprintf(os.Stderr, "Available commands:\n")
	fmt.Fprintf(os.Stderr, "    join <room>        - Join a room and connect to everyone in it\n")
	fmt.Fprintf(os.Stderr, "    record-peer [secs] - Record raw incoming audio from first peer to a WAV file\n")
	fmt.Fprintf(os.Stderr, "    test               - Test current audio device\n")
	fmt.Fprintf(os.Stderr, "    stun               - Get public IP from STUN server\n")
	fmt.Fprintf(os.Stderr, "    devices|list       - List all audio devices\n")
	fmt.Fprintf(os.Stderr, "    select             - Select a device for recording\n")
}

func newLocalPeerIdentifier() signalling.PeerIdentifier {
	return signalling.PeerIdentifier{
		Uuid:     uuid.New(),
		PublicIP: "", // In a real client, one would need to query a STUN server to retrieve this
	}
}

func repl(api *audioapi.RtAudioApi, app *application.App) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("\n====== Roundtable REPL ======\n")
	printCommands()
	fmt.Printf("==================================\n\n")
	// publicIP := "http://127.0.0.1:1067"

	for {
		fmt.Printf(">> ")
		if !scanner.Scan() {
			break
		}
		cmd := strings.TrimSpace(scanner.Text())

		switch cmd {
		case "join":
			{
				parts := strings.SplitN(cmd, " ", 2)
				var roomName string
				if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
					roomName = strings.TrimSpace(parts[1])
				} else {
					fmt.Printf("Enter room name: ")
					if !scanner.Scan() {
						break
					}
					roomName = strings.TrimSpace(scanner.Text())
				}
				if roomName == "" {
					fmt.Fprintf(os.Stderr, "room name cannot be empty\n")
					break
				}
				ctx := context.Background()
				if err := app.JoinRoom(ctx, roomName); err != nil {
					slog.Error("error joining room", "room", roomName, "err", err)
				}
			}
		case "record-peer":
			duration := 5 * time.Second
			outPath := "recordings/peer_raw.wav"
			fmt.Printf("Recording %s of raw incoming peer audio to %s...\n", duration, outPath)
			samples, sampleRate, numChannels, err := app.RecordPeerAudio(duration)
			if err != nil {
				fmt.Fprintf(os.Stderr, "record-peer error: %v\n", err)
				break
			}
			// Convert float32 → int16 for WAV
			int16Samples := make([]int16, len(samples))
			for i, s := range samples {
				if s > 1.0 {
					s = 1.0
				}
				if s < -1.0 {
					s = -1.0
				}
				int16Samples[i] = int16(s * 32767)
			}
			if err := os.MkdirAll("recordings", 0755); err != nil {
				fmt.Fprintf(os.Stderr, "failed to create recordings dir: %v\n", err)
				break
			}
			data := &rtaudiowrapper.RecordingData{Buffer: int16Samples, TotalFrames: len(int16Samples) / numChannels, FrameCounter: len(int16Samples) / numChannels, Channels: numChannels}
			if err := rtaudiowrapper.WriteWavFile(outPath, data, uint32(sampleRate), 16); err != nil {
				fmt.Fprintf(os.Stderr, "failed to write WAV: %v\n", err)
				break
			}
			fmt.Printf("Saved %d samples (%dHz, %dch) to %s\n", len(samples), sampleRate, numChannels, outPath)
			fmt.Printf("Play it back with: play %s\n", outPath)

		case "stun":
			{
				stunme()
			}
		case "close":
			fallthrough
		case "exit":
			{
				// app.Close()
				return
			}

		case "test":
			dir := filepath.Dir(micTestFile)
			err := os.MkdirAll(dir, 0755)
			if err != nil {
				log.Fatalf("Failed to create directory: %v", err)
			}

			Record(micTestFile)

			// TODO: (JAKE) Seems like it is playing the audioback in mono...
			fmt.Printf("Playing from: %s\n", micTestFile)
			if err := rtaudiowrapper.Speaker(micTestFile); err != nil {
				fmt.Fprintf(os.Stderr, "Error playing file: %v\n", err)
				os.Exit(1)
			}

		case "devices":
			fallthrough
		case "list":
			print_devices()

		case "select":
			print_devices()
			fmt.Printf("Enter device ID: ")
			if !scanner.Scan() {
				break
			}
			var deviceID int
			_, err := fmt.Sscanf(strings.TrimSpace(scanner.Text()), "%d", &deviceID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Invalid device ID\n")
				break
			}
			selectedDeviceID = deviceID
			devices := api.InputDevices()
			newDev, err := api.InitInputDeviceFromID(devices[deviceID])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Invalid device\n")
				break
			}

			app.SetInputDevice(newDev)
			fmt.Printf("Selected device ID: %d\n", selectedDeviceID)
		default:
			fmt.Fprintf(os.Stderr, "Invalid command: %s\n", cmd)
			printCommands()
		}
	}

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
	// Handle signals to shutdown gracefully on CTRL+C

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	signalInterruptContext, signalInterruptContextCancel := context.WithCancel(context.Background())
	go func() {
		<-sigs
		signal.Reset()
		signalInterruptContextCancel()
	}()

	// --------------------------------------------------------------------------------
	// Setup RtAudioApi
	frameDuration := time.Millisecond * 20
	api, err := audioapi.NewRtAudioApi(frameDuration)

	if err != nil {
		slog.Error("error while creating rtaudio api", "err", err)
		return
	}

	localPeerIdentifier := newLocalPeerIdentifier()
	jsonID, _ := json.Marshal(localPeerIdentifier)
	fmt.Printf("Your peer ID: %s\n", base64.StdEncoding.EncodeToString(jsonID))

	connectionManager := initializeConnectionManager(localPeerIdentifier)

	app, err := application.NewApp(api, connectionManager)

	if err != nil {
		slog.Error("error in making new app", "err", err)
		panic(err)
	}
	// --------------------------------------------------------------------------------
	// Start repl
	repl(api, app)

	// --------------------------------------------------------------------------------

	<-signalInterruptContext.Done()
	// If interrupted with CTRL+C, just exit
	slog.Debug("closing gracefully")
	app.Close()

}
