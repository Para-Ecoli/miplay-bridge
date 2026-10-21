package output

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
	"github.com/miplay-bridge/miplay-bridge/internal/mpd"
)

// fakeKernel is a minimal in-process MPD server: exactly the command subset
// the arbitration engine drives.
type fakeKernel struct {
	listener net.Listener

	mu       sync.Mutex
	commands []string
	state    string
	busy     bool
	volume   int
}

func newFakeKernel(t *testing.T) *fakeKernel {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	kernel := &fakeKernel{listener: listener, state: "stop", volume: 100}
	go kernel.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return kernel
}

func (k *fakeKernel) port() int {
	return k.listener.Addr().(*net.TCPAddr).Port
}

func (k *fakeKernel) serve() {
	for {
		connection, err := k.listener.Accept()
		if err != nil {
			return
		}
		go k.handle(connection)
	}
}

func (k *fakeKernel) handle(connection net.Conn) {
	defer connection.Close()
	fmt.Fprint(connection, "OK MPD 0.23.5\n")
	reader := bufio.NewReader(connection)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k.mu.Lock()
		k.commands = append(k.commands, line)
		state, busy := k.state, k.busy
		k.mu.Unlock()

		if busy {
			fmt.Fprint(connection, "ACK [50@0] {add} Resource busy\n")
			continue
		}

		fields := strings.Fields(line)
		switch fields[0] {
		case "status":
			k.mu.Lock()
			volume := k.volume
			k.mu.Unlock()
			fmt.Fprintf(connection, "state: %s\nelapsed: 3.500\nduration: 210.000\nvolume: %d\nOK\n", state, volume)
		case "currentsong":
			fmt.Fprint(connection, "file: http://127.0.0.1:8080/stream/1\nTitle: Test Track\nArtist: Test Artist\nAlbum: Test Album\nduration: 210.000\nOK\n")
		case "play":
			k.setState("play")
			fmt.Fprint(connection, "OK\n")
		case "pause":
			if len(fields) > 1 && fields[1] == "1" {
				k.setState("pause")
			} else {
				k.setState("play")
			}
			fmt.Fprint(connection, "OK\n")
		case "stop":
			k.setState("stop")
			fmt.Fprint(connection, "OK\n")
		case "setvol":
			value, _ := strconv.Atoi(fields[1])
			k.mu.Lock()
			k.volume = value
			k.mu.Unlock()
			fmt.Fprint(connection, "OK\n")
		case "clear", "add", "seekcur":
			fmt.Fprint(connection, "OK\n")
		default:
			fmt.Fprintf(connection, "ACK [5@0] {%s} unknown command\n", fields[0])
		}
	}
}

func (k *fakeKernel) setState(state string) {
	k.mu.Lock()
	k.state = state
	k.mu.Unlock()
}

func (k *fakeKernel) setVolume(volume int) {
	k.mu.Lock()
	k.volume = volume
	k.mu.Unlock()
}

func (k *fakeKernel) setBusy(busy bool) {
	k.mu.Lock()
	k.busy = busy
	k.mu.Unlock()
}

func (k *fakeKernel) recorded() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string{}, k.commands...)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testEngine assembles a real engine around a fake kernel.
func testEngine(t *testing.T, kernel *fakeKernel) *Engine {
	t.Helper()
	cfg := config.Default()
	cfg.MPDHost = "127.0.0.1"
	cfg.MPDPort = kernel.port()
	cfg.DataDirectory = t.TempDir()
	logger := testLogger()
	manager := mpd.NewManager(cfg, logger)
	return New(cfg, logger, manager)
}

func TestClaimArbitration(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	if engine.ActiveSource() != SourceIdle {
		t.Fatalf("a fresh engine must be idle, active = %q", engine.ActiveSource())
	}
	if err := engine.Claim(SourceIdle); err == nil {
		t.Fatal("the idle source cannot claim the sound card")
	}
	if err := engine.Claim(SourceDLNA); err != nil {
		t.Fatal(err)
	}
	if err := engine.Claim(SourceDLNA); err != nil {
		t.Fatalf("re-claiming the same source must be idempotent: %v", err)
	}
	if err := engine.Claim(SourceAPI); !errors.Is(err, ErrSourceBusy) {
		t.Fatalf("Claim(api) while dlna is active = %v, want ErrSourceBusy", err)
	}
	if engine.ActiveSource() != SourceDLNA {
		t.Fatalf("active = %q", engine.ActiveSource())
	}

	// Releasing a source that does not own the slot is a no-op.
	engine.Release(SourceAPI)
	if engine.ActiveSource() != SourceDLNA {
		t.Fatal("releasing a non-owner must not free the slot")
	}
	engine.Release(SourceDLNA)
	if engine.ActiveSource() != SourceIdle {
		t.Fatal("release must free the slot")
	}
	engine.Release(SourceDLNA) // idempotent

	// The Player-interface spellings delegate to Claim/Release.
	if err := engine.ClaimDLNA(); err != nil {
		t.Fatal(err)
	}
	engine.ReleaseDLNA()
	if engine.ActiveSource() != SourceIdle {
		t.Fatal("ReleaseDLNA must free the slot")
	}
}

func TestReleaseSourceForcesIdle(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	if previous := engine.ReleaseSource(); previous != SourceIdle {
		t.Fatalf("previous = %q, want idle", previous)
	}
	if err := engine.Claim(SourceAPI); err != nil {
		t.Fatal(err)
	}
	if previous := engine.ReleaseSource(); previous != SourceAPI {
		t.Fatalf("previous = %q, want api", previous)
	}
	if engine.ActiveSource() != SourceIdle {
		t.Fatalf("ReleaseSource must force the slot back to idle, active = %q", engine.ActiveSource())
	}
}

func TestClaimMiPlayChecksTheKernelState(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	kernel.setState("play")
	if err := engine.Claim(SourceMiPlay); !errors.Is(err, ErrSourceBusy) {
		t.Fatalf("Claim(miplay) with a playing kernel = %v, want ErrSourceBusy", err)
	}
	kernel.setState("pause")
	if err := engine.Claim(SourceMiPlay); !errors.Is(err, ErrSourceBusy) {
		t.Fatalf("Claim(miplay) with a paused kernel = %v, want ErrSourceBusy", err)
	}
	kernel.setState("stop")
	if err := engine.Claim(SourceMiPlay); err != nil {
		t.Fatalf("Claim(miplay) with a stopped kernel: %v", err)
	}
	if engine.ActiveSource() != SourceMiPlay {
		t.Fatalf("active = %q", engine.ActiveSource())
	}
}

func TestClaimMiPlayToleratesUnreachableKernel(t *testing.T) {
	// Port 1 is reserved and never listening in this test environment.
	cfg := config.Default()
	cfg.MPDHost = "127.0.0.1"
	cfg.MPDPort = 1
	logger := testLogger()
	engine := New(cfg, logger, mpd.NewManager(cfg, logger))
	if err := engine.Claim(SourceMiPlay); err != nil {
		t.Fatalf("an unreachable kernel must not block the MiPlay claim: %v", err)
	}
}

func TestStopReleasesDLNAAndAPIClaims(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)
	ctx := context.Background()

	if err := engine.ClaimDLNA(); err != nil {
		t.Fatal(err)
	}
	if err := engine.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if engine.ActiveSource() != SourceIdle {
		t.Fatalf("Stop must release the dlna claim, active = %q", engine.ActiveSource())
	}

	if err := engine.Claim(SourceAPI); err != nil {
		t.Fatal(err)
	}
	if err := engine.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if engine.ActiveSource() != SourceIdle {
		t.Fatalf("Stop must release the api claim, active = %q", engine.ActiveSource())
	}
	// Stopping an idle engine stays fine.
	if err := engine.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPlaySeekSequence(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)
	ctx := context.Background()

	if err := engine.Load(ctx, "http://host/song.flac"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Play(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.Seek(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if err := engine.Seek(ctx, -5); err != nil {
		t.Fatal(err)
	}

	commands := kernel.recorded()
	expected := []string{"clear", "add http://host/song.flac", "play", "seekcur 10.000", "seekcur 0.000"}
	if len(commands) != len(expected) {
		t.Fatalf("commands = %v, want %v", commands, expected)
	}
	for index, want := range expected {
		if commands[index] != want {
			t.Fatalf("commands[%d] = %q, want %q (full: %v)", index, commands[index], want, commands)
		}
	}
}

func TestPauseAndInfoSnapshot(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)
	ctx := context.Background()

	if err := engine.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	info := engine.Info(ctx)
	if info.State != "pause" {
		t.Fatalf("state = %q", info.State)
	}
	if info.Elapsed != 3.5 || info.Duration != 210 {
		t.Fatalf("elapsed/duration = %v/%v", info.Elapsed, info.Duration)
	}
	if info.Title != "Test Track" || info.Artist != "Test Artist" || info.Album != "Test Album" {
		t.Fatalf("song = %q / %q / %q", info.Title, info.Artist, info.Album)
	}
	if info.URI != "http://127.0.0.1:8080/stream/1" {
		t.Fatalf("uri = %q", info.URI)
	}
	if info.Error != "" {
		t.Fatalf("error = %q", info.Error)
	}
}

func TestLoadPropagatesDeviceBusy(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	kernel.setBusy(true)
	err := engine.Load(context.Background(), "http://host/song.flac")
	if err == nil {
		t.Fatal("expected the busy kernel to fail the load")
	}
	if !mpd.IsDeviceBusy(err) {
		t.Fatalf("IsDeviceBusy(%v) = false", err)
	}
}

func TestEngineWithoutKernelReportsUnavailable(t *testing.T) {
	logger := testLogger()
	engine := New(config.Default(), logger, nil)
	ctx := context.Background()

	// The read-only snapshot degrades gracefully.
	info := engine.Info(ctx)
	if info.State != "unknown" || info.Error == "" {
		t.Fatalf("info = %+v", info)
	}
	// Every mutating surface refuses with ErrUnavailable.
	for name, call := range map[string]func() error{
		"clear": func() error { return engine.Clear(ctx) },
		"play":  func() error { return engine.Play(ctx) },
		"pause": func() error { return engine.Pause(ctx) },
		"stop":  func() error { return engine.Stop(ctx) },
		"seek":  func() error { return engine.Seek(ctx, 1) },
	} {
		if err := call(); !errors.Is(err, mpd.ErrUnavailable) {
			t.Fatalf("%s = %v, want ErrUnavailable", name, err)
		}
	}
}

func TestSuperviseReleasesStoppedKernel(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Supervise(ctx)

	// While the kernel keeps streaming the slot stays claimed.
	kernel.setState("play")
	if err := engine.ClaimDLNA(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if engine.ActiveSource() != SourceDLNA {
		t.Fatal("a playing kernel must keep the dlna claim")
	}

	// Two consecutive stopped ticks release the slot.
	kernel.setState("stop")
	deadline := time.Now().Add(6 * time.Second)
	for engine.ActiveSource() == SourceDLNA {
		if time.Now().After(deadline) {
			t.Fatal("a stopped kernel must release the slot within two monitor ticks")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// protocol volume: kernel software gain
// ---------------------------------------------------------------------------

func TestEngineVolumeWithoutKernelDegrades(t *testing.T) {
	// No kernel: the software gain surface reports the missing playback
	// kernel instead of touching any hardware mixer.
	engine := New(config.Default(), testLogger(), nil)
	ctx := context.Background()

	if _, err := engine.SetVolume(ctx, 50); !errors.Is(err, ErrNoVolumeControl) {
		t.Fatalf("SetVolume = %v, want ErrNoVolumeControl", err)
	}
	if _, err := engine.SetMute(ctx, true); !errors.Is(err, ErrNoVolumeControl) {
		t.Fatalf("SetMute = %v, want ErrNoVolumeControl", err)
	}
	if _, _, err := engine.Volume(ctx); !errors.Is(err, ErrNoVolumeControl) {
		t.Fatalf("Volume = %v, want ErrNoVolumeControl", err)
	}
}

func TestEngineVolumeDrivesTheKernelSoftwareMixer(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)
	ctx := context.Background()

	if percent, muted, err := engine.Volume(ctx); err != nil || percent != 100 || muted {
		t.Fatalf("initial Volume = %d/%v/%v, want 100/false", percent, muted, err)
	}
	if percent, err := engine.SetVolume(ctx, 33); err != nil || percent != 33 {
		t.Fatalf("SetVolume(33) = %d, %v", percent, err)
	}
	if percent, err := engine.SetVolume(ctx, 150); err != nil || percent != 100 {
		t.Fatalf("SetVolume(150) = %d, %v, want the clamped 100", percent, err)
	}
	if muted, err := engine.SetMute(ctx, true); err != nil || !muted {
		t.Fatalf("SetMute(true) = %v, %v", muted, err)
	}
	// A level set while muted is remembered but not written to the kernel.
	if percent, err := engine.SetVolume(ctx, 40); err != nil || percent != 40 {
		t.Fatalf("SetVolume(40) while muted = %d, %v", percent, err)
	}
	if percent, muted, err := engine.Volume(ctx); err != nil || percent != 40 || !muted {
		t.Fatalf("muted Volume = %d/%v/%v, want 40/true", percent, muted, err)
	}
	if muted, err := engine.SetMute(ctx, false); err != nil || muted {
		t.Fatalf("SetMute(false) = %v, %v", muted, err)
	}

	// An external client changing the kernel's software gain is reflected.
	kernel.setVolume(42)
	if percent, muted, err := engine.Volume(ctx); err != nil || percent != 42 || muted {
		t.Fatalf("live Volume = %d/%v/%v, want 42/false", percent, muted, err)
	}

	var setvol []string
	for _, command := range kernel.recorded() {
		if strings.HasPrefix(command, "setvol") {
			setvol = append(setvol, command)
		}
	}
	want := []string{"setvol 33", "setvol 100", "setvol 0", "setvol 40"}
	if len(setvol) != len(want) {
		t.Fatalf("setvol commands = %v, want %v", setvol, want)
	}
	for index := range want {
		if setvol[index] != want[index] {
			t.Fatalf("setvol commands = %v, want %v", setvol, want)
		}
	}
}
