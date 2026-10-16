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

func TestDefaultCoversTheHardwareMixer(t *testing.T) {
	cfg := Default()
	if cfg.AmixerPath != "amixer" {
		t.Fatalf("AmixerPath = %q, want the bare binary resolved through PATH", cfg.AmixerPath)
	}
	if cfg.VolumeControl != "" {
		t.Fatalf("VolumeControl = %q, want auto-detection", cfg.VolumeControl)
	}
	if cfg.VolumeDefault != 80 {
		t.Fatalf("VolumeDefault = %d, want 80", cfg.VolumeDefault)
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("BRIDGE_PORT", "9001")
	t.Setenv("BRIDGE_TOKEN", "s3cret")
	t.Setenv("ALSA_CARD", "2")
	t.Setenv("ALSA_PCM_DEVICE", "3")
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

func TestLoadReadsMixerEnvironment(t *testing.T) {
	t.Setenv("AMIXER_PATH", " /opt/alsa/bin/amixer ")
	t.Setenv("BRIDGE_VOLUME_CONTROL", " Headphone ")
	t.Setenv("BRIDGE_VOLUME_DEFAULT", "65")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AmixerPath != "/opt/alsa/bin/amixer" {
		t.Fatalf("AmixerPath = %q", cfg.AmixerPath)
	}
	if cfg.VolumeControl != "Headphone" {
		t.Fatalf("VolumeControl = %q", cfg.VolumeControl)
	}
	if cfg.VolumeDefault != 65 {
		t.Fatalf("VolumeDefault = %d", cfg.VolumeDefault)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := []struct{ name, value string }{
		{"BRIDGE_PORT", "70000"},
		{"MIPLAY_CONTROL_PORT", "0"},
		{"MIPLAY_ALSA_BUFFER_US", "1"},
		{"DLNA_SSDP_MAX_AGE", "5"},
		{"DLNA_ENABLED", "maybe"},
		{"BRIDGE_VOLUME_DEFAULT", "150"},
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
