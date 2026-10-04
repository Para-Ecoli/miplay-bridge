package config

import "testing"

func TestDefaultMatchesVerifiedHardware(t *testing.T) {
	cfg := Default()
	if cfg.Port != 8092 {
		t.Fatalf("default port = %d, want 8092 (8091 belongs to the local-output sidecar)", cfg.Port)
	}
	if cfg.Card != 0 || cfg.PCMDevice != 0 {
		t.Fatalf("default card/pcm = %d/%d, want 0/0", cfg.Card, cfg.PCMDevice)
	}
	if got := cfg.ALSAHardwareDevice(); got != "hw:0,0" {
		t.Fatalf("ALSAHardwareDevice() = %q, want hw:0,0", got)
	}
	if got := cfg.ALSAHardwareCard(); got != "hw:0" {
		t.Fatalf("ALSAHardwareCard() = %q, want hw:0", got)
	}
	if cfg.AutoMuteControl != "Auto-Mute Mode" {
		t.Fatalf("AutoMuteControl = %q, want Auto-Mute Mode", cfg.AutoMuteControl)
	}
	if !cfg.FixAutoMute {
		t.Fatal("FixAutoMute must default to true: without it the 3.5mm jack stays muted")
	}
	if cfg.MPDMixerControl != "Headphone" {
		t.Fatalf("MPDMixerControl = %q, want Headphone", cfg.MPDMixerControl)
	}
	if cfg.SoundDirectory != "/dev/snd" {
		t.Fatalf("SoundDirectory = %q, want /dev/snd", cfg.SoundDirectory)
	}
	if got := cfg.ControlDevicePath(); got != "/dev/snd/controlC0" {
		t.Fatalf("ControlDevicePath() = %q", got)
	}
	if got := cfg.PCMPlaybackDevicePath(); got != "/dev/snd/pcmC0D0p" {
		t.Fatalf("PCMPlaybackDevicePath() = %q", got)
	}
}

func TestDefaultCoversReceiverAndRenderer(t *testing.T) {
	cfg := Default()
	if !cfg.MiPlayEnabled {
		t.Fatal("MiPlayEnabled must default to true: the receiver is the whole point")
	}
	if cfg.MiPlayControlPort != 8899 {
		t.Fatalf("MiPlayControlPort = %d, want the Mi-connect convention 8899", cfg.MiPlayControlPort)
	}
	if cfg.MiPlayAlsaBufferUS != 200000 {
		t.Fatalf("MiPlayAlsaBufferUS = %d, want 200000", cfg.MiPlayAlsaBufferUS)
	}
	if !cfg.DLNAEnabled {
		t.Fatal("DLNAEnabled must default to true")
	}
	if cfg.DataDirectory != "/data" {
		t.Fatalf("DataDirectory = %q, want /data so identities survive restarts", cfg.DataDirectory)
	}
	if got := cfg.MiPlayDeviceIDFile(); got != "/data/miplay-device-id" {
		t.Fatalf("MiPlayDeviceIDFile() = %q", got)
	}
	if got := cfg.DLNAUDNFile(); got != "/data/dlna-udn" {
		t.Fatalf("DLNAUDNFile() = %q", got)
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("BRIDGE_PORT", "9001")
	t.Setenv("BRIDGE_TOKEN", "s3cret")
	t.Setenv("ALSA_CARD", "2")
	t.Setenv("ALSA_PCM_DEVICE", "3")
	t.Setenv("MPD_MIXER_CONTROL", "Master")
	t.Setenv("ALSA_FIX_AUTO_MUTE", "false")
	t.Setenv("BRIDGE_TEST_TONE_HZ", "1000")
	t.Setenv("MIPLAY_CONTROL_PORT", "9987")
	t.Setenv("MIPLAY_ALSA_BUFFER_US", "350000")
	t.Setenv("DLNA_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9001 {
		t.Fatalf("Port = %d", cfg.Port)
	}
	if cfg.Token != "s3cret" {
		t.Fatalf("Token = %q", cfg.Token)
	}
	if cfg.Card != 2 || cfg.PCMDevice != 3 {
		t.Fatalf("card/pcm = %d/%d", cfg.Card, cfg.PCMDevice)
	}
	if got := cfg.ALSAHardwareDevice(); got != "hw:2,3" {
		t.Fatalf("ALSAHardwareDevice() = %q", got)
	}
	if cfg.MPDMixerControl != "Master" {
		t.Fatalf("MPDMixerControl = %q", cfg.MPDMixerControl)
	}
	if cfg.FixAutoMute {
		t.Fatal("ALSA_FIX_AUTO_MUTE=false must disable the fix")
	}
	if cfg.TestToneFrequencyHz != 1000 {
		t.Fatalf("TestToneFrequencyHz = %d", cfg.TestToneFrequencyHz)
	}
	if cfg.MiPlayControlPort != 9987 {
		t.Fatalf("MiPlayControlPort = %d", cfg.MiPlayControlPort)
	}
	if cfg.MiPlayAlsaBufferUS != 350000 {
		t.Fatalf("MiPlayAlsaBufferUS = %d", cfg.MiPlayAlsaBufferUS)
	}
	if cfg.DLNAEnabled {
		t.Fatal("DLNA_ENABLED=false must disable the renderer")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := []struct{ name, value string }{
		{"BRIDGE_PORT", "70000"},
		{"MIPLAY_CONTROL_PORT", "0"},
		{"MIPLAY_ALSA_BUFFER_US", "1"},
		{"DLNA_SSDP_MAX_AGE", "5"},
		{"ALSA_DEFAULT_VOLUME", "101"},
		{"ALSA_FIX_AUTO_MUTE", "maybe"},
		{"DLNA_ENABLED", "maybe"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(testCase.name, testCase.value)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%s must be rejected", testCase.name, testCase.value)
			}
		})
	}
}
