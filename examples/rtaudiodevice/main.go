package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	// "time"

	"github.com/Honorable-Knights-of-the-Roundtable/rtaudiowrapper"
)

const defaultFile = "recordings/default.wav"
var currentFile string
var selectedDeviceID int = -1 // -1 means use default
var useLoopback bool = false  // Enable WASAPI loopback mode for system audio capture


type RecordingData struct {
	buffer       []int16
	totalFrames  int
	frameCounter int
	channels     int
}

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

func repl() {
	scanner := bufio.NewScanner(os.Stdin)
	// currentFile := defaultFile
	var filePath string
	fmt.Printf("\n====== Audio Recording REPL ======\n")
	fmt.Printf("\nCMDS: \n")
	fmt.Printf("    'loopback' to enable WASAPI loopback mode for system audio capture\n")
	fmt.Printf("    'devices' to list available audio devices\n")
	fmt.Printf("    'select' to choose a device\n")
	fmt.Printf("    'record' to start recording\n")
	fmt.Printf("==================================\n\n")

	for {
		fmt.Printf(">> ")
		if !scanner.Scan() {
			break
		}
		cmd := strings.TrimSpace(scanner.Text())

		switch cmd {
		case "record":
			fmt.Printf("Write to default file? %s [y/n]: ", defaultFile)
			if !scanner.Scan() {
				break
			}
			answer := strings.TrimSpace(scanner.Text())

			if answer == "n" {
				fmt.Printf("enter output file path: ")
				if !scanner.Scan() {
					break
				}
				filePath = strings.TrimSpace(scanner.Text())
			} else {
				filePath = defaultFile
			}
			currentFile = filePath

			dir := filepath.Dir(filePath)
			err := os.MkdirAll(dir, 0755)
			if err != nil {
				log.Fatalf("Failed to create directory: %v", err)
			}

			Record(filePath)
		case "play":
			fmt.Printf("Play from default file? %s [y/n]: ", defaultFile)
			if !scanner.Scan() {
				break
			}
			answer := strings.TrimSpace(scanner.Text())

			if answer == "n" {
				fmt.Printf("enter input file path: ")
				if !scanner.Scan() {
					break
				}
				filePath = strings.TrimSpace(scanner.Text())
			} else {
				filePath = defaultFile
			}

			fmt.Printf("Playing from: %s\n", filePath)
			if err := rtaudiowrapper.Speaker(filePath); err != nil {
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
			fmt.Printf("Selected device ID: %d\n", selectedDeviceID)

		case "edit":
			fmt.Printf("\n=== WAV Editor ===\n")
			fmt.Printf("Enter input file: ")
			if !scanner.Scan() {
				break
			}
			inputFile := strings.TrimSpace(scanner.Text())

			wav, err := ReadWavFile(inputFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
				break
			}

			fmt.Printf("Loaded: %d channels, %d Hz, %.2f seconds\n",
				wav.Channels, wav.SampleRate, wav.Duration())
			fmt.Printf("\nAvailable operations:\n")
			fmt.Printf("  1. Trim (specify start and end time)\n")
			fmt.Printf("  2. Adjust volume (specify gain multiplier)\n")
			fmt.Printf("  3. Reverse\n")
			fmt.Printf("  4. Fade in/out\n")
			fmt.Printf("  5. Save as-is\n")
			fmt.Printf("Enter operation number: ")
			if !scanner.Scan() {
				break
			}
			op := strings.TrimSpace(scanner.Text())

			var result *WavData
			switch op {
			case "1":
				fmt.Printf("Enter start time (seconds): ")
				if !scanner.Scan() {
					break
				}
				var start float64
				fmt.Sscanf(scanner.Text(), "%f", &start)

				fmt.Printf("Enter end time (seconds): ")
				if !scanner.Scan() {
					break
				}
				var end float64
				fmt.Sscanf(scanner.Text(), "%f", &end)

				result = wav.Trim(start, end)
				fmt.Printf("Trimmed to %.2f-%.2f seconds\n", start, end)

			case "2":
				fmt.Printf("Enter volume gain (1.0 = no change, 2.0 = double): ")
				if !scanner.Scan() {
					break
				}
				var gain float64
				fmt.Sscanf(scanner.Text(), "%f", &gain)

				result = wav.AdjustVolume(gain)
				fmt.Printf("Adjusted volume by %.2fx\n", gain)

			case "3":
				result = wav.Reverse()
				fmt.Printf("Reversed audio\n")

			case "4":
				fmt.Printf("Enter fade in duration (seconds): ")
				if !scanner.Scan() {
					break
				}
				var fadeIn float64
				fmt.Sscanf(scanner.Text(), "%f", &fadeIn)

				fmt.Printf("Enter fade out duration (seconds): ")
				if !scanner.Scan() {
					break
				}
				var fadeOut float64
				fmt.Sscanf(scanner.Text(), "%f", &fadeOut)

				result = wav.FadeIn(fadeIn).FadeOut(fadeOut)
				fmt.Printf("Applied fade in (%.2fs) and fade out (%.2fs)\n", fadeIn, fadeOut)

			case "5":
				result = wav

			default:
				fmt.Fprintf(os.Stderr, "Invalid operation\n")
			}

			if result != nil {
				fmt.Printf("Enter output file: ")
				if !scanner.Scan() {
					break
				}
				outputFile := strings.TrimSpace(scanner.Text())

				if err := result.SaveWavFile(outputFile); err != nil {
					fmt.Fprintf(os.Stderr, "Error saving: %v\n", err)
				} else {
					fmt.Printf("Saved to %s (%.2f seconds)\n", outputFile, result.Duration())
				}
			}

		case "loopback":
			// Check if we're using WASAPI (Windows only feature)
			audio, err := rtaudiowrapper.Create(rtaudiowrapper.APIUnspecified)
			if err == nil {
				currentAPI := audio.CurrentAPI()
				audio.Destroy()

				if currentAPI != rtaudiowrapper.APIWindowsWASAPI {
					fmt.Printf("WARNING: Loopback mode only works on Windows with WASAPI\n")
					fmt.Printf("    Current API: %s\n", currentAPI)
					fmt.Printf("    Loopback may not function correctly on this platform\n")
				}
			}

			useLoopback = !useLoopback
			if useLoopback {
				fmt.Printf("WASAPI Loopback mode ENABLED\n")
				fmt.Printf("    This will capture system audio output (what you hear)\n")
				fmt.Printf("    Select an OUTPUT device (speakers/headphones) to capture from\n")
			} else {
				fmt.Printf("WASAPI Loopback mode DISABLED\n")
				fmt.Printf("    Will use standard microphone/input recording\n")
			}

		default:
			fmt.Fprintf(os.Stderr, "Invalid command: %s\n", cmd)
			fmt.Fprintf(os.Stderr, "Available commands:\n")
			fmt.Fprintf(os.Stderr, "    record     - Start recording audio\n")
			fmt.Fprintf(os.Stderr, "    play       - Play back recorded audio\n")
			fmt.Fprintf(os.Stderr, "    devices    - List all audio devices\n")
			fmt.Fprintf(os.Stderr, "    list       - List all audio devices\n")
			fmt.Fprintf(os.Stderr, "    select     - Select a device for recording\n")
			fmt.Fprintf(os.Stderr, "    loopback   - Toggle WASAPI loopback mode (capture system audio)\n")
			fmt.Fprintf(os.Stderr, "    edit       - Edit WAV files (trim, volume, reverse, fade)\n")
			flag.Usage()
		}
	}

}

func Record(outpath string)  {
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

	if useLoopback {
		// WASAPI Loopback mode: use output device to capture what's playing
		fmt.Printf("\n=== WASAPI LOOPBACK MODE ===\n")
		if selectedDeviceID >= 0 && selectedDeviceID < len(devices) {
			inputDevice = devices[selectedDeviceID]
		} else {
			// Use default output device for loopback
			inputDevice = audio.DefaultOutputDevice()
		}
		// For loopback, we need the device to have output channels
		if inputDevice.NumOutputChannels == 0 {
			log.Fatal("Selected device has no output channels. For loopback mode, select a device that plays audio (speakers/headphones).")
		}

		// In WASAPI loopback, we capture from the output device
		// The channels should be based on output channels
		fmt.Printf("Capturing system audio from: %s\n", inputDevice.Name)
		fmt.Printf("Output channels: %d\n", inputDevice.NumOutputChannels)
	} else {
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
	}

	// Determine channels based on mode
	var channels int
	if useLoopback {
		channels = inputDevice.NumOutputChannels
	} else {
		channels = inputDevice.NumInputChannels
	}
	sampleRate := inputDevice.PreferredSampleRate

	// Initialize recording data with a large buffer (e.g., 10 minutes worth)
	// Adjust this if you need longer recordings
	maxDuration := 600 // seconds (10 minutes)
	maxFrames := int(sampleRate) * maxDuration

	// Initialize recording data
	data := &RecordingData{
		buffer:       make([]int16, maxFrames*channels),
		totalFrames:  maxFrames,
		frameCounter: 0,
		channels:     channels,
	}

	// Setup stream parameters
	var inputParams *rtaudiowrapper.StreamParams
	var outputParams *rtaudiowrapper.StreamParams

	if useLoopback {
		// WASAPI Loopback: Set BOTH input and output to the same device
		// This triggers loopback mode in RtAudio
		outputParams = &rtaudiowrapper.StreamParams{
			DeviceID:     uint(inputDevice.ID),
			NumChannels:  uint(channels),
			FirstChannel: 0,
		}
		inputParams = &rtaudiowrapper.StreamParams{
			DeviceID:     uint(inputDevice.ID),
			NumChannels:  uint(channels),
			FirstChannel: 0,
		}
	} else {
		// Normal recording: only input params
		inputParams = &rtaudiowrapper.StreamParams{
			DeviceID:     uint(inputDevice.ID),
			NumChannels:  uint(channels),
			FirstChannel: 0,
		}
		outputParams = nil
	}

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
			for i := range inputData{
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
		if data.frameCounter+nFrames > data.totalFrames {
			frames = data.totalFrames - data.frameCounter
			if frames <= 0 {
				return 2 // Buffer full, stop recording
			}
		}

		// Copy data to our buffer
		offset := data.frameCounter * data.channels
		samplesToCopy := frames * data.channels
		copy(data.buffer[offset:offset+samplesToCopy], inputData[:samplesToCopy])
		data.frameCounter += frames

		return 0
	}

	err = audio.Open(outputParams, inputParams, rtaudiowrapper.FormatInt16, sampleRate, uint(512), cb, &options)
	if err != nil {
		log.Fatal(err)
	}

	err = audio.Start()
	if err != nil {
		log.Fatal("Audio failed to start\n",err)
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
			duration := float64(data.frameCounter) / float64(sampleRate)
			fmt.Printf("\rRecording: %.1f seconds (%d frames)", duration, data.frameCounter)
		}
	}
cleanup:
	fmt.Printf("\n\nRecording complete. Recorded %d frames (%.1f seconds).\n",
		data.frameCounter, float64(data.frameCounter)/float64(sampleRate))

	// Write WAV file
	fmt.Printf("Writing WAV file: %s\n", outpath)
	if err := WriteWavFile(outpath, data, uint32(sampleRate), 16); err != nil {
		fmt.Printf("Error writing WAV file: %v\n", err)
		return
	}
	fmt.Printf("Successfully wrote %s\n", outpath)
}

// writeWavFile writes audio data to a WAV file
func WriteWavFile(filename string, data *RecordingData, sampleRate uint32, bitsPerSample uint32) error {
	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	channels := uint32(data.channels)

	// Calculate sizes based on actual frames recorded
	dataSize := uint32(data.frameCounter) * channels * (bitsPerSample / 8)

	// Write RIFF header
	file.Write([]byte("RIFF"))
	binary.Write(file, binary.LittleEndian, uint32(36+dataSize)) // ChunkSize
	file.Write([]byte("WAVE"))

	// Write fmt subchunk
	file.Write([]byte("fmt "))
	binary.Write(file, binary.LittleEndian, uint32(16))                                    // Subchunk1Size (PCM)
	binary.Write(file, binary.LittleEndian, uint16(1))                                     // AudioFormat (PCM)
	binary.Write(file, binary.LittleEndian, uint16(channels))                              // NumChannels
	binary.Write(file, binary.LittleEndian, uint32(sampleRate))                            // SampleRate
	binary.Write(file, binary.LittleEndian, uint32(sampleRate*channels*(bitsPerSample/8))) // ByteRate
	binary.Write(file, binary.LittleEndian, uint16(channels*(bitsPerSample/8)))            // BlockAlign
	binary.Write(file, binary.LittleEndian, uint16(bitsPerSample))                         // BitsPerSample

	// Write data subchunk
	file.Write([]byte("data"))
	binary.Write(file, binary.LittleEndian, uint32(dataSize)) // Subchunk2Size

	// Write audio data from Go slice
	totalSamples := data.frameCounter * data.channels
	for i := 0; i < totalSamples; i++ {
		if err := binary.Write(file, binary.LittleEndian, data.buffer[i]); err != nil {
			return fmt.Errorf("failed to write sample: %w", err)
		}
	}

	return nil
}



func main() {
	repl()
	// Define command-line flags
	// mode := flag.String("mode", "record", "Mode: 'record' or 'play'")
	// file := flag.String("file", "./assets/media.wav", "WAV file path")
	//
	// flag.Parse()
	// fmt.Printf("File %s\n", *file)

	// dir := filepath.Dir(*file)
	// err := os.MkdirAll(dir, 0755)
	// if err != nil {
	// 	log.Fatalf("Failed to create directory: %v", err)
	// }

	// switch *mode {
	// case "record":
	// 	fmt.Printf("Recording to: %s\n", *file)
	// 	rtaudiowrapper.Record(*file)
	// case "play":
	// 	fmt.Printf("Playing from: %s\n", *file)
	// 	if err := rtaudiowrapper.Speaker(*file); err != nil {
	// 		fmt.Fprintf(os.Stderr, "Error playing file: %v\n", err)
	// 		os.Exit(1)
	// 	}
	// case "devices":
	// 	audio, err := rtaudiowrapper.Create(rtaudiowrapper.APIUnspecified)
	// 	if err != nil {
	// 		fmt.Fprintf(os.Stderr, "Failed to create rtaudio device\n")
	// 	}
	//
	// 	devices, err := audio.Devices()
	// 	if err != nil {
	// 		fmt.Fprintf(os.Stderr, "%s", err)
	// 	}
	//
	// 	for _, device := range devices {
	// 		fmt.Printf("%v\n", device)
	// 	}
	//
	// default:
	// 	fmt.Fprintf(os.Stderr, "Invalid mode: %s. Use 'record' or 'play'\n", *mode)
	// 	flag.Usage()
	// 	os.Exit(1)
	// }
}

