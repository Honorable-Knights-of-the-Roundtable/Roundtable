package config

import (
	"log/slog"

	"errors"
	"github.com/Honorable-Knights-of-the-Roundtable/roundtable/internal/encoderdecoder"
	"github.com/spf13/viper"
	"io"
	"os"
)

func ConfigureDefaultLogger(logLevel string, logFile string, loggerOptions slog.HandlerOptions) (*os.File, error) {

	switch logLevel {
	case "none":
		// No logging is required, disable the logger and return
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		return nil, nil
	case "error":
		loggerOptions.Level = slog.LevelError
	case "warn":
		loggerOptions.Level = slog.LevelWarn
	case "info":
		loggerOptions.Level = slog.LevelInfo
	case "debug":
		loggerOptions.Level = slog.LevelDebug
	default:
		return nil, errors.New("unexpected log level")
	}

	// --------------------------------------------------------------------------------

	var logFilePointer *os.File
	var slogHandler slog.Handler
	if logFile == "" {
		logFilePointer = nil
		slogHandler = slog.NewTextHandler(os.Stdout, &loggerOptions)
	} else {
		logFilePointer, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return nil, err
		}
		slogHandler = slog.NewJSONHandler(logFilePointer, &loggerOptions)
	}

	// --------------------------------------------------------------------------------

	slog.SetDefault(slog.New(slogHandler))
	return logFilePointer, nil
}

func setViperDefaults() {
	viper.SetDefault("loglevel", "info")
	viper.SetDefault("logfile", "")
	viper.SetDefault("localport", 1066)
	viper.SetDefault("timeout", 30)
	viper.SetDefault("codecs", []string{"CodecOpus48000Mono", "CodecOpus24000Mono", "CodecOpus48000Stereo", "CodecOpus24000Stereo"})
	viper.SetDefault("OPUSFrameDuration", encoderdecoder.OPUS_FRAME_DURATION_20_MS)
	viper.SetDefault("OPUSBufferSafetyFactor", 16)
}

func LoadConfig(configFilePath string) {
	setViperDefaults()

	viper.SetConfigFile(configFilePath)
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			slog.Info("no config file found", "configFilePath", configFilePath)
		} else {
			slog.Error("error during config read", "err", err)
			panic(err)
		}
	}

	// The user *must* specify at least one ICE Server
	if !viper.IsSet("ICEServers") || len(viper.GetStringSlice("ICEServers")) == 0 {
		slog.Error("at least one ICE server must be specified. See the `config` section of the README.")
		panic("no ICE server specified")
	}
}
