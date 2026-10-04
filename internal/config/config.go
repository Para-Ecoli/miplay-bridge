// Package config resolves the miplay-bridge shell configuration from the
// environment.
//
// Every knob has a working default that matches the verified target machine
// (card 0 / PCM device 0 / Headphone mixer control) so the container starts
// with no environment at all. The defaults are documented in README.md and are
// kept stable so an operator gets the same behaviour across deployments.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved shell configuration.
type Config struct {
	// Port is the HTTP control port: API, embedded console and the
	// DLNA description/SOAP/GENA paths. Default 8092.
	Port int
	// BindAddress is the HTTP listen address. Default 0.0.0.0 so the phone,
	// the DLNA controller and operators can all reach it.
	BindAddress string
	// Token, when set, is required in x-bridge-token / Authorization on every
	// /api/* request. Empty means the shell trusts its network.
	Token string
	// DataDirectory holds the persisted device identity (MiPlay UUID, DLNA
	// UDN). It must survive container restarts, otherwise phones accumulate
	// duplicate entries in their casting device pickers.
	DataDirectory string

	// SoundDirectory is the ALSA device node directory. It is the only
	// enumeration source: /proc/asound is NOT readable inside the container
	// (verified on the target host), so nothing here may depend on it.
	SoundDirectory string
	// Card is the ALSA card index. Default 0 (Intel HDA / ALC269VB).
	Card int
	// PCMDevice is the ALSA PCM device index on that card. Default 0.
	PCMDevice int

	// AutoMuteControl is the HD-Audio simple mixer control that silently mutes
	// both output pins when left at its factory default. Disabling it is the
	// single most important step of the whole feature.
	AutoMuteControl string
	// AutoMuteDisabledValue is written to AutoMuteControl on startup.
	AutoMuteDisabledValue string
	// MasterControl / HeadphoneControl / SpeakerControl are unmuted on startup.
	MasterControl    string
	HeadphoneControl string
	SpeakerControl   string
	// DefaultVolume is applied to the master control on startup (percent).
	DefaultVolume int
	// FixAutoMute can be turned off for exotic sound cards that reject the
	// control. Default true: the fix is mandatory on the verified hardware.
	FixAutoMute bool

	// AmixerPath is the alsa-utils binary used for every mixer operation.
	AmixerPath string

	// MPDBinary / MPDConfPath / MPDTemplatePath drive the playback kernel.
	MPDBinary       string
	MPDConfPath     string
	MPDTemplatePath string
	// MPDHost / MPDPort is the loopback control socket. MPD is never exposed
	// to the LAN: only this shell talks to it.
	MPDHost string
	MPDPort int
	// MPDMusicDirectory must exist for MPD to start. It is mounted read-only.
	MPDMusicDirectory string
	// MPDDataDirectory holds tag_cache/state/playlists.
	MPDDataDirectory string
	// MPDMixerControl is the hardware mixer control MPD drives for volume.
	MPDMixerControl string
	// MPDStartTimeout bounds how long we wait for MPD to accept connections.
	MPDStartTimeout time.Duration
	// MPDMaxRestarts bounds automatic restarts of the playback kernel.
	MPDMaxRestarts int
	// MPDLogFile is the MPD log target. Empty lets MPD log to stderr, which is
	// what `mpd --no-daemon` does by default.
	MPDLogFile string

	// MiPlayEnabled toggles the whole 小米妙播 receiver: mDNS advertising on
	// 5353/udp, the binary control server on MiPlayControlPort and the
	// realtime ffmpeg|aplay pipeline.
	MiPlayEnabled bool
	// MiPlayControlPort is the TCP control port phones connect to (advertised
	// in the mDNS appsData payload). Default 8899, the Mi-connect convention.
	MiPlayControlPort int
	// MiPlayName is the friendly name shown in the phone's 妙播 device list.
	MiPlayName string
	// MiPlayAdvertiseAddress overrides the LAN IPv4 used in mDNS answers and
	// in the Safety channel key derivation. Empty means auto-detect.
	MiPlayAdvertiseAddress string
	// MiPlayHandshakeTimeout bounds the control-channel authentication phase;
	// a phone that stalls mid-handshake must not hold the slot forever.
	MiPlayHandshakeTimeout time.Duration
	// MiPlayAlsaBufferUS is the aplay buffer time in microseconds, the primary
	// latency knob of the realtime pipeline. 200ms absorbs Wi-Fi jitter while
	// staying interactive.
	MiPlayAlsaBufferUS int
	// FFmpegPath is the MPEG-TS/AAC decoder binary of the realtime pipeline.
	FFmpegPath string
	// AplayPath is the raw-PCM ALSA output binary of the realtime pipeline.
	AplayPath string

	// DLNAEnabled toggles the UPnP MediaRenderer (SSDP advertising, device
	// description, SOAP control, GENA events).
	DLNAEnabled bool
	// DLNAFriendlyName is the name DLNA controllers display.
	DLNAFriendlyName string
	// DLNAAdvertiseAddress overrides the LAN IPv4 used in SSDP LOCATION
	// headers and the device description. Empty means auto-detect.
	DLNAAdvertiseAddress string
	// DLNASSDPMaxAge is the SSDP cache-control max-age in seconds.
	DLNASSDPMaxAge int

	// TestToneSeconds / TestToneFrequencyHz describe /api/test-play audio.
	TestToneSeconds     int
	TestToneFrequencyHz int
	// TestToneTTL is how long the internally served test tone stays reachable
	// after /api/test-play asks MPD to fetch it.
	TestToneTTL time.Duration

	// LogLevel is one of debug, info, warn, error.
	LogLevel string
}

// Default returns the built-in defaults verified against the target host.
func Default() Config {
	return Config{
		Port:          8092,
		BindAddress:   "0.0.0.0",
		DataDirectory: "/data",

		SoundDirectory: "/dev/snd",
		Card:           0,
		PCMDevice:      0,

		AutoMuteControl:       "Auto-Mute Mode",
		AutoMuteDisabledValue: "Disabled",
		MasterControl:         "Master",
		HeadphoneControl:      "Headphone",
		SpeakerControl:        "Speaker",
		DefaultVolume:         80,
		FixAutoMute:           true,

		AmixerPath: "amixer",

		MPDBinary:         "mpd",
		MPDConfPath:       "/etc/mpd.conf",
		MPDTemplatePath:   "/etc/miplay-bridge/mpd.conf.template",
		MPDHost:           "127.0.0.1",
		MPDPort:           6600,
		MPDMusicDirectory: "/music",
		MPDDataDirectory:  "/var/lib/mpd",
		MPDMixerControl:   "Headphone",
		MPDStartTimeout:   20 * time.Second,
		MPDMaxRestarts:    5,

		MiPlayEnabled:          true,
		MiPlayControlPort:      8899,
		MiPlayName:             "妙播桥",
		MiPlayHandshakeTimeout: 15 * time.Second,
		MiPlayAlsaBufferUS:     200000,
		FFmpegPath:             "ffmpeg",
		AplayPath:              "aplay",

		DLNAEnabled:      true,
		DLNAFriendlyName: "妙播桥",
		DLNASSDPMaxAge:   1800,

		TestToneSeconds:     2,
		TestToneFrequencyHz: 440,
		TestToneTTL:         120 * time.Second,

		LogLevel: "info",
	}
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	cfg := Default()

	if err := parseIntEnv("BRIDGE_PORT", &cfg.Port, 1, 65535); err != nil {
		return cfg, err
	}
	if v := strings.TrimSpace(os.Getenv("BRIDGE_BIND")); v != "" {
		cfg.BindAddress = v
	}
	cfg.Token = strings.TrimSpace(os.Getenv("BRIDGE_TOKEN"))
	if v := strings.TrimSpace(os.Getenv("BRIDGE_DATA_DIR")); v != "" {
		cfg.DataDirectory = v
	}

	if v := strings.TrimSpace(os.Getenv("ALSA_SOUND_DIRECTORY")); v != "" {
		cfg.SoundDirectory = v
	}
	if err := parseIntEnv("ALSA_CARD", &cfg.Card, 0, 32); err != nil {
		return cfg, err
	}
	if err := parseIntEnv("ALSA_PCM_DEVICE", &cfg.PCMDevice, 0, 32); err != nil {
		return cfg, err
	}
	if v := strings.TrimSpace(os.Getenv("ALSA_AUTO_MUTE_CONTROL")); v != "" {
		cfg.AutoMuteControl = v
	}
	if v := strings.TrimSpace(os.Getenv("ALSA_MASTER_CONTROL")); v != "" {
		cfg.MasterControl = v
	}
	if v := strings.TrimSpace(os.Getenv("ALSA_HEADPHONE_CONTROL")); v != "" {
		cfg.HeadphoneControl = v
	}
	if v := strings.TrimSpace(os.Getenv("ALSA_SPEAKER_CONTROL")); v != "" {
		cfg.SpeakerControl = v
	}
	if err := parseIntEnv("ALSA_DEFAULT_VOLUME", &cfg.DefaultVolume, 0, 100); err != nil {
		return cfg, err
	}
	if err := parseBoolEnv("ALSA_FIX_AUTO_MUTE", &cfg.FixAutoMute); err != nil {
		return cfg, err
	}
	if v := strings.TrimSpace(os.Getenv("AMIXER_PATH")); v != "" {
		cfg.AmixerPath = v
	}

	if v := strings.TrimSpace(os.Getenv("MPD_BINARY")); v != "" {
		cfg.MPDBinary = v
	}
	if v := strings.TrimSpace(os.Getenv("MPD_CONF_PATH")); v != "" {
		cfg.MPDConfPath = v
	}
	if v := strings.TrimSpace(os.Getenv("MPD_TEMPLATE_PATH")); v != "" {
		cfg.MPDTemplatePath = v
	}
	if v := strings.TrimSpace(os.Getenv("MPD_HOST")); v != "" {
		cfg.MPDHost = v
	}
	if err := parseIntEnv("MPD_PORT", &cfg.MPDPort, 1, 65535); err != nil {
		return cfg, err
	}
	if v := strings.TrimSpace(os.Getenv("MPD_MUSIC_DIRECTORY")); v != "" {
		cfg.MPDMusicDirectory = v
	}
	if v := strings.TrimSpace(os.Getenv("MPD_DATA_DIRECTORY")); v != "" {
		cfg.MPDDataDirectory = v
	}
	if v := strings.TrimSpace(os.Getenv("MPD_MIXER_CONTROL")); v != "" {
		cfg.MPDMixerControl = v
	}
	if err := parseIntEnv("MPD_MAX_RESTARTS", &cfg.MPDMaxRestarts, 0, 100); err != nil {
		return cfg, err
	}
	if err := parseSecondsEnv("MPD_START_TIMEOUT_SECONDS", &cfg.MPDStartTimeout, 1, 300); err != nil {
		return cfg, err
	}
	cfg.MPDLogFile = strings.TrimSpace(os.Getenv("MPD_LOG_FILE"))

	if err := parseBoolEnv("MIPLAY_ENABLED", &cfg.MiPlayEnabled); err != nil {
		return cfg, err
	}
	if err := parseIntEnv("MIPLAY_CONTROL_PORT", &cfg.MiPlayControlPort, 1, 65535); err != nil {
		return cfg, err
	}
	if v := strings.TrimSpace(os.Getenv("MIPLAY_NAME")); v != "" {
		cfg.MiPlayName = v
	}
	cfg.MiPlayAdvertiseAddress = strings.TrimSpace(os.Getenv("MIPLAY_ADVERTISE_ADDRESS"))
	if err := parseSecondsEnv("MIPLAY_HANDSHAKE_TIMEOUT_SECONDS", &cfg.MiPlayHandshakeTimeout, 1, 120); err != nil {
		return cfg, err
	}
	if err := parseIntEnv("MIPLAY_ALSA_BUFFER_US", &cfg.MiPlayAlsaBufferUS, 10000, 2000000); err != nil {
		return cfg, err
	}
	if v := strings.TrimSpace(os.Getenv("FFMPEG_PATH")); v != "" {
		cfg.FFmpegPath = v
	}
	if v := strings.TrimSpace(os.Getenv("APLAY_PATH")); v != "" {
		cfg.AplayPath = v
	}

	if err := parseBoolEnv("DLNA_ENABLED", &cfg.DLNAEnabled); err != nil {
		return cfg, err
	}
	if v := strings.TrimSpace(os.Getenv("DLNA_FRIENDLY_NAME")); v != "" {
		cfg.DLNAFriendlyName = v
	}
	cfg.DLNAAdvertiseAddress = strings.TrimSpace(os.Getenv("DLNA_ADVERTISE_ADDRESS"))
	if err := parseIntEnv("DLNA_SSDP_MAX_AGE", &cfg.DLNASSDPMaxAge, 60, 86400); err != nil {
		return cfg, err
	}

	if err := parseIntEnv("BRIDGE_TEST_TONE_SECONDS", &cfg.TestToneSeconds, 1, 30); err != nil {
		return cfg, err
	}
	if err := parseIntEnv("BRIDGE_TEST_TONE_HZ", &cfg.TestToneFrequencyHz, 20, 20000); err != nil {
		return cfg, err
	}
	if v := strings.TrimSpace(os.Getenv("BRIDGE_LOG_LEVEL")); v != "" {
		cfg.LogLevel = strings.ToLower(v)
	}

	return cfg, nil
}

func parseIntEnv(name string, target *int, min, max int) error {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return fmt.Errorf("invalid %s %q (want %d..%d)", name, raw, min, max)
	}
	*target = value
	return nil
}

func parseSecondsEnv(name string, target *time.Duration, min, max int) error {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < min || seconds > max {
		return fmt.Errorf("invalid %s %q (want %d..%d)", name, raw, min, max)
	}
	*target = time.Duration(seconds) * time.Second
	return nil
}

func parseBoolEnv(name string, target *bool) error {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		*target = true
	case "0", "false", "no", "off":
		*target = false
	default:
		return fmt.Errorf("invalid %s %q (want a boolean)", name, raw)
	}
	return nil
}

// Addr is the HTTP listen address.
func (c Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.BindAddress, c.Port)
}

// MPDAddress is the MPD loopback control address.
func (c Config) MPDAddress() string {
	return fmt.Sprintf("%s:%d", c.MPDHost, c.MPDPort)
}

// ALSAHardwareDevice is the exclusive ALSA PCM name, e.g. "hw:0,0".
func (c Config) ALSAHardwareDevice() string {
	return fmt.Sprintf("hw:%d,%d", c.Card, c.PCMDevice)
}

// ALSAHardwareCard is the exclusive ALSA card name, e.g. "hw:0".
func (c Config) ALSAHardwareCard() string {
	return fmt.Sprintf("hw:%d", c.Card)
}

// ControlDevicePath is the ALSA control node for the configured card.
func (c Config) ControlDevicePath() string {
	return fmt.Sprintf("%s/controlC%d", strings.TrimRight(c.SoundDirectory, "/"), c.Card)
}

// PCMPlaybackDevicePath is the ALSA playback node for the configured card.
func (c Config) PCMPlaybackDevicePath() string {
	return fmt.Sprintf("%s/pcmC%dD%dp", strings.TrimRight(c.SoundDirectory, "/"), c.Card, c.PCMDevice)
}

// MiPlayDeviceIDFile is where the stable MiPlay device UUID is persisted.
func (c Config) MiPlayDeviceIDFile() string {
	return filepath.Join(c.DataDirectory, "miplay-device-id")
}

// DLNAUDNFile is where the stable DLNA device UDN is persisted.
func (c Config) DLNAUDNFile() string {
	return filepath.Join(c.DataDirectory, "dlna-udn")
}
