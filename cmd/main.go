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
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/signalling"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"github.com/spf13/viper"
)

func Record(outpath string, inputDevice audiodevice.AudioSourceDevice) {
	audio, err := rtaudiowrapper.Create(rtaudiowrapper.APIUnspecified)
	if err != nil {
		log.Fatal(err)
	}
	defer audio.Destroy()
	devProperties := inputDevice.GetDeviceProperties()

	// Determine channels based on mode
	channels := devProperties.NumChannels
	sampleRate := devProperties.SampleRate
	if channels == 0 {
		log.Fatal("Selected device has no input channels. Choose a different device.")
	}

	// Initialize recording data with a large buffer (e.g., 10 minutes worth)
	// Adjust this if you need longer recordings
	maxDuration := 600 // seconds (10 minutes)
	maxFrames := sampleRate * maxDuration

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
		DeviceID:     uint(devProperties.ID),
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

	err = audio.Open(outputParams, inputParams, rtaudiowrapper.FormatInt16, uint(sampleRate), uint(512), cb, &options)
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

// TODO: Convert to general api
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

// TODO: Convert to general api
func printInputDevices(api *audioapi.RtAudioApi) {

	audio, err := rtaudiowrapper.Create(rtaudiowrapper.APIUnspecified)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create rtaudio device\n")
	}
	defer audio.Destroy()

	devices, err := audio.Devices()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s", err)
	}

	found := 0
	fmt.Printf("\nAvailable Input Devices:\n")
	fmt.Printf("%-4s %-50s %-8s %-8s %-8s\n", "ID", "Name", "In", "Out", "Duplex")
	fmt.Printf("%s\n", strings.Repeat("-", 85))
	for i, device := range devices {
		if device.NumInputChannels > 0 {
			inputCh := fmt.Sprintf("%d", device.NumInputChannels)
			outputCh := fmt.Sprintf("%d", device.NumOutputChannels)
			marker := ""
			if i == audio.DefaultInputDeviceId() {
				marker = " [DEFAULT IN]"
			}
			duplexCh := fmt.Sprintf("%d", device.NumDuplexChannels)
			fmt.Printf("%-4d %-50s %-8s %-8s %-8s%s\n", found, device.Name, inputCh, outputCh, duplexCh, marker)
			found += 1
		}
	}
}

// TODO: Convert to general api
func printOutputDevices(api *audioapi.RtAudioApi) {

	audio, err := rtaudiowrapper.Create(rtaudiowrapper.APIUnspecified)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create rtaudio device\n")
	}
	defer audio.Destroy()

	devices, err := audio.Devices()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s", err)
	}

	fmt.Printf("\nAvailable Input Devices:\n")
	fmt.Printf("%-4s %-50s %-8s %-8s %-8s\n", "ID", "Name", "In", "Out", "Duplex")
	fmt.Printf("%s\n", strings.Repeat("-", 85))

	found := 0
	for i, device := range devices {

		if device.NumOutputChannels > 0 {
			inputCh := fmt.Sprintf("%d", device.NumInputChannels)
			outputCh := fmt.Sprintf("%d", device.NumOutputChannels)
			marker := ""
			if i == audio.DefaultInputDeviceId() {
				marker = " [DEFAULT IN]"
			}
			duplexCh := fmt.Sprintf("%d", device.NumDuplexChannels)
			fmt.Printf("%-4d %-50s %-8s %-8s %-8s%s\n", found, device.Name, inputCh, outputCh, duplexCh, marker)
			found += 1
		}
	}
}

func newLocalPeerIdentifier() signalling.PeerIdentifier {
	return signalling.PeerIdentifier{
		Uuid:     uuid.New(),
		PublicIP: "", // In a real client, one would need to query a STUN server to retrieve this
	}
}

//	func changeInputDeviceFromId(id int) {
//		audio, err := rtaudiowrapper.Create(rtaudiowrapper.APIUnspecified)
//		if err != nil {
//			log.Fatal(err)
//		}
//		defer audio.Destroy()
//
//		devices, err := audio.Devices()
//		for _, device := range devices {
//			fmt.Println(device.String())
//		}
//		if err != nil {
//			log.Fatal(err)
//		}
//
//		var inputDevice rtaudiowrapper.DeviceInfo
//
//		// Normal input recording mode
//		if selectedDeviceID >= 0 && selectedDeviceID < len(devices) {
//			inputDevice = devices[selectedDeviceID]
//		} else {
//			inputDevice = audio.DefaultInputDevice()
//			fmt.Printf("Recording from default device: %s\n", inputDevice.Name)
//		}
//
//		if inputDevice.NumInputChannels == 0 {
//			log.Fatal("Selected device has no input channels. Choose a different device.")
//		}
//	}
func join(cmd string, app *application.App) {
	scanner := bufio.NewScanner(os.Stdin)
	parts := strings.SplitN(cmd, " ", 2)
	var roomName string
	if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
		roomName = strings.TrimSpace(parts[1])
	} else {
		fmt.Printf("Enter room name: ")
		if !scanner.Scan() {
			return
		}
		roomName = strings.TrimSpace(scanner.Text())
	}
	if roomName == "" {
		fmt.Fprintf(os.Stderr, "room name cannot be empty\n")
		return
	}
	ctx := context.Background()
	if err := app.JoinRoom(ctx, roomName); err != nil {
		slog.Error("error joining room", "room", roomName, "err", err)
	}
}

func recordPeer(app *application.App) {
	duration := 5 * time.Second
	outPath := "recordings/peer_raw.wav"
	fmt.Printf("Recording %s of raw incoming peer audio to %s...\n", duration, outPath)
	samples, sampleRate, numChannels, err := app.RecordPeerAudio(duration)
	if err != nil {
		fmt.Fprintf(os.Stderr, "record-peer error: %v\n", err)
		return
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
		return
	}
	data := &rtaudiowrapper.RecordingData{Buffer: int16Samples, TotalFrames: len(int16Samples) / numChannels, FrameCounter: len(int16Samples) / numChannels, Channels: numChannels}
	if err := rtaudiowrapper.WriteWavFile(outPath, data, uint32(sampleRate), 16); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write WAV: %v\n", err)
		return
	}
	fmt.Printf("Saved %d samples (%dHz, %dch) to %s\n", len(samples), sampleRate, numChannels, outPath)
	fmt.Printf("Play it back with: play %s\n", outPath)
}

func micTest(app *application.App) {
	if err := os.MkdirAll(filepath.Dir(micTestFile), 0755); err != nil {
		log.Fatalf("Failed to create directory: %v", err)
	}

	fmt.Println("Recording from mic... press Enter to stop")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		bufio.NewReader(os.Stdin).ReadBytes('\n')
		cancel()
	}()

	samples, sampleRate, numChannels, err := app.TapInputAudio(ctx)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "recording error: %v\n", err)
		return
	}

	// Debug: report peak level so we can tell if the mic captured anything
	var peak float32
	for _, s := range samples {
		if s < 0 {
			s = -s
		}
		if s > peak {
			peak = s
		}
	}
	fmt.Printf("Recorded %d samples, peak level: %.2f%%\n", len(samples), peak*100)

	// Mix stereo down to mono using the user's selected channel so both ears hear the playback.
	if numChannels == 2 {
		ch := app.GetPreferredInputChannel()
		mono := make([]float32, len(samples)/2)
		for i := range mono {
			mono[i] = samples[i*2+ch]
		}
		samples = mono
		numChannels = 1
	}

	int16Samples := make([]int16, len(samples))
	for i, s := range samples {
		if s > 1.0 {
			s = 1.0
		} else if s < -1.0 {
			s = -1.0
		}
		int16Samples[i] = int16(s * 32767)
	}

	data := &rtaudiowrapper.RecordingData{
		Buffer:       int16Samples,
		TotalFrames:  len(int16Samples) / numChannels,
		FrameCounter: len(int16Samples) / numChannels,
		Channels:     numChannels,
	}
	if err := rtaudiowrapper.WriteWavFile(micTestFile, data, uint32(sampleRate), 16); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing file: %v\n", err)
		return
	}

	fmt.Printf("Playing from: %s\n", micTestFile)
	if err := rtaudiowrapper.Speaker(micTestFile); err != nil {
		fmt.Fprintf(os.Stderr, "Error playing file: %v\n", err)
		os.Exit(1)
	}
}

func selectDevice(api *audioapi.RtAudioApi, app *application.App, devices []audioapi.AudioIODevice) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("Enter device ID: ")
	if !scanner.Scan() {
		return
	}
	var selectedDeviceID int
	_, err := fmt.Sscanf(strings.TrimSpace(scanner.Text()), "%d", &selectedDeviceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid device ID\n")
		return
	}

	if selectedDeviceID >= 0 && selectedDeviceID < len(devices) {
		newDev, err := api.InitInputDeviceFromID(devices[selectedDeviceID])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to init DeviceFromId with error: %v \n", err)
			return
		}
		app.SetInputDevice(newDev)
	} else {
		fmt.Printf("Invalid Device Id, device remains the same\n")
	}
}

func selectInput(api *audioapi.RtAudioApi, app *application.App) {
	devices := api.InputDevices()
	printInputDevices(api)
	selectDevice(api, app, devices)
}

func selectOutput(api *audioapi.RtAudioApi, app *application.App) {
	devices := api.OutputDevices()
	printOutputDevices(api)
	selectDevice(api, app, devices)
}

func printCommands() {
	fmt.Fprintf(os.Stderr, "Available commands:\n")
	fmt.Fprintf(os.Stderr, "    join <room>        - Join a room and connect to everyone in it\n")
	fmt.Fprintf(os.Stderr, "    record-peer [secs] - Record raw incoming audio from first peer to a WAV file\n")
	fmt.Fprintf(os.Stderr, "    test               - Test current audio device\n")
	fmt.Fprintf(os.Stderr, "    devices            - List all audio devices\n")
	fmt.Fprintf(os.Stderr, "    listInput          - List all input audio devices\n")
	fmt.Fprintf(os.Stderr, "    listOutput         - List all output audio devices\n")
	fmt.Fprintf(os.Stderr, "    input              - Select input audio device\n")
	fmt.Fprintf(os.Stderr, "    output              - Select output audio device\n")
	fmt.Fprintf(os.Stderr, "    channel <1|2>      - Select input channel for stereo devices (default: 1)\n")
	fmt.Fprintf(os.Stderr, "    gain <value>       - Set input gain multiplier (e.g. gain 200)\n")
	fmt.Fprintf(os.Stderr, "    disconnect         - Disconnect from current room\n")
	fmt.Fprintf(os.Stderr, "    help               - prints this message\n")
	fmt.Fprintf(os.Stderr, "    close|exit         - exit the repl\n")
}

func disconnect(app *application.App) {
	app.DisconnectAll()
	fmt.Println("Disconnected from room")
}

func repl(api *audioapi.RtAudioApi, app *application.App) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("\n====== Roundtable REPL ======\n")
	printCommands()
	fmt.Printf("==================================\n\n")

	for {
		fmt.Printf(">> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		cmd := parts[0]

		switch cmd {
		case "join":
			join(line, app)
		case "devices":
			print_devices()
		case "listInput":
			printInputDevices(api)
		case "input":
			selectInput(api, app)
		case "output":
			selectOutput(api, app)
		case "listOutput":
			printOutputDevices(api)
		case "record-peer":
			recordPeer(app)
		case "test":
			micTest(app)
		case "channel":
			if len(parts) != 2 {
				fmt.Fprintf(os.Stderr, "usage: channel <1|2>\n")
				break
			}
			var ch int
			if _, err := fmt.Sscanf(parts[1], "%d", &ch); err != nil || ch < 1 || ch > 2 {
				fmt.Fprintf(os.Stderr, "channel: must be 1 or 2\n")
				break
			}
			app.SetInputChannel(ch - 1)
			fmt.Printf("Input channel set to Input %d\n", ch)
		case "gain":
			if len(parts) != 2 {
				fmt.Fprintf(os.Stderr, "usage: gain <value>  (e.g. gain 200)\n")
				break
			}
			var g float32
			if _, err := fmt.Sscanf(parts[1], "%f", &g); err != nil || g < 0 {
				fmt.Fprintf(os.Stderr, "gain: value must be a non-negative number\n")
				break
			}
			app.SetInputGain(g)
			fmt.Printf("Input gain set to %.1f\n", g)
		case "disconnect":
			disconnect(app)
		case "help":
			printCommands()
		case "close", "exit":
			app.Close()
			os.Exit(0)
			return
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
