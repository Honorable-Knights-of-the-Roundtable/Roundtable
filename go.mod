module github.com/Honorable-Knights-of-the-Roundtable/roundtable

go 1.25.1

require (
	github.com/Honorable-Knights-of-the-Roundtable/opus v0.0.0
	github.com/Honorable-Knights-of-the-Roundtable/rtaudiowrapper v0.0.0-00010101000000-000000000000
	github.com/go-audio/audio v1.0.0
	github.com/go-audio/wav v1.1.0
	github.com/google/uuid v1.6.0
	github.com/oov/audio v0.0.0-20171004131523-88a2be6dbe38
	github.com/pion/webrtc/v4 v4.1.5
	github.com/spf13/viper v1.21.0
)

replace github.com/Honorable-Knights-of-the-Roundtable/opus => ./internal/opus

replace github.com/Honorable-Knights-of-the-Roundtable/rtaudiowrapper => ./internal/rtaudiowrapper

require (
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-audio/riff v1.0.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pion/datachannel v1.5.10 // indirect
	github.com/pion/dtls/v3 v3.0.7 // indirect
	github.com/pion/ice/v4 v4.0.10 // indirect
	github.com/pion/interceptor v0.1.41 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.0.7 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.15 // indirect
	github.com/pion/rtp v1.8.23 // indirect
	github.com/pion/sctp v1.8.39 // indirect
	github.com/pion/sdp/v3 v3.0.16 // indirect
	github.com/pion/srtp/v3 v3.0.8 // indirect
	github.com/pion/stun/v3 v3.0.0 // indirect
	github.com/pion/transport/v3 v3.0.8 // indirect
	github.com/pion/turn/v4 v4.1.1 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.43.0 // indirect
	golang.org/x/net v0.46.0 // indirect
	golang.org/x/sys v0.37.0 // indirect
	golang.org/x/text v0.30.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)
