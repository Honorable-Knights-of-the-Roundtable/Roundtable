package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/cmd/application"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/audioapi"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/pkg/audiodevice"
	"github.com/Honorable-Knights-of-the-Roundtable/rtaudiowrapper"
)

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
	peers, err := app.JoinRoom(ctx, roomName)
	if err != nil {
		slog.Error("error joining room", "room", roomName, "err", err)
	}

	if peers == nil {
		slog.Error("Peers was nil even with app.JoinRoom succeeding, something is probably wrong ", "room", roomName)
	}
	fmt.Printf("Users in room: %d\n", len(peers))
}

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

	samples, sampleRate, numChannels, err := app.TapInputAudioWithStats(ctx)
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
	if err := rtaudiowrapper.Speaker(micTestFile, app.GetCurrentOutputDevice().Name); err != nil {
		fmt.Fprintf(os.Stderr, "Error playing file: %v\n", err)
		os.Exit(1)
	}
}

func readDeviceID(devices []audioapi.AudioIODevice) (int, bool) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("Enter device ID: ")
	if !scanner.Scan() {
		return 0, false
	}
	var id int
	if _, err := fmt.Sscanf(strings.TrimSpace(scanner.Text()), "%d", &id); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid device ID\n")
		return 0, false
	}
	if id < 0 || id >= len(devices) {
		fmt.Fprintf(os.Stderr, "Invalid device ID, device remains the same\n")
		return 0, false
	}
	return id, true
}

func printDevices(devices []audioapi.AudioIODevice) string {
	var sb strings.Builder
	for i, dev := range devices {
		inputCh := fmt.Sprintf("%d", clampChannels(dev.DeviceProperties.NumChannels))
		fmt.Fprintf(&sb, "%-4d %-50s %-8s \n", i, dev.Name, inputCh)
	}

	return sb.String()
}

func selectInput(app *application.App) {
	devices := app.GetInputDevices()
	fmt.Println("Current Input Device:", app.GetCurrentInputDevice().Name)
	fmt.Println(printDevices(devices))
	id, ok := readDeviceID(devices)
	if !ok {
		return
	}
	if err := app.SelectInputDevice(devices[id]); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to init input device: %v\n", err)
	}
}

func selectOutput(app *application.App) {
	devices := app.GetOutputDevices()
	fmt.Println("Current Output Device:", app.GetCurrentOutputDevice().Name)
	fmt.Println(printDevices(devices))
	id, ok := readDeviceID(devices)
	if !ok {
		return
	}
	if err := app.SelectOutputDevice(devices[id]); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to init output device: %v\n", err)
	}
}

func printCommands() {
	fmt.Fprintf(os.Stderr, "Available commands:\n")
	fmt.Fprintf(os.Stderr, "    join <room>        - Join a room and connect to everyone in it\n")
	fmt.Fprintf(os.Stderr, "    record-peer [secs] - Record raw incoming audio from first peer to a WAV file\n")
	fmt.Fprintf(os.Stderr, "    test               - Test current audio device\n")
	fmt.Fprintf(os.Stderr, "    input              - Select input audio device\n")
	fmt.Fprintf(os.Stderr, "    output              - Select output audio device\n")
	fmt.Fprintf(os.Stderr, "    channel <1|2>      - Select input channel for stereo devices (default: 1)\n")
	fmt.Fprintf(os.Stderr, "    gain <value>       - Set input gain multiplier (e.g. gain 200)\n")
	fmt.Fprintf(os.Stderr, "    disconnect         - Disconnect from current room\n")
	fmt.Fprintf(os.Stderr, "    help               - prints this message\n")
	fmt.Fprintf(os.Stderr, "    close|exit         - exit the repl\n")
}

func disconnectAllRooms(app *application.App) {
	app.DisconnectAll()
	ctx := context.Background()
	err := app.DisconnectRooms(ctx)
	if err != nil {
		slog.Error("Error with leaving room", "err", err)
	}
	fmt.Println("Disconnected from room")
}

func repl(app *application.App) {
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
		case "input":
			selectInput(app)
		case "output":
			selectOutput(app)
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
			disconnectAllRooms(app)
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

// TODO(Jake): This shouldn't be here probably
func clampChannels(n int) int {
	if n > 2 {
		return 2
	}
	return n
}
