package airplay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestCaptureStartupProbeTimeoutTracksFrameRate(t *testing.T) {
	if got := captureStartupProbeTimeout(30); got != minimumCaptureStartupProbeTimeout {
		t.Fatalf("30 fps startup timeout = %v, want %v", got, minimumCaptureStartupProbeTimeout)
	}
	if got := captureStartupProbeTimeout(1); got != 5*time.Second {
		t.Fatalf("1 fps startup timeout = %v, want 5s", got)
	}
	if got := captureStartupProbeTimeout(0); got != minimumCaptureStartupProbeTimeout {
		t.Fatalf("default startup timeout = %v, want %v", got, minimumCaptureStartupProbeTimeout)
	}
}

func TestProbeX11CaptureSourceUsesRealXHandshake(t *testing.T) {
	requireX11CaptureTestStack(t)
	display, stop := startTestXvfb(t)
	if err := probeX11CaptureSource(context.Background(), display, CaptureConfig{}); err != nil {
		t.Fatalf("live X11 source probe: %v", err)
	}

	stop()
	started := time.Now()
	err := probeX11CaptureSource(context.Background(), display, CaptureConfig{})
	if err == nil {
		t.Fatal("stale X11 display passed the source probe")
	}
	for _, want := range []string{display, "GStreamer stderr"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("stale X11 source error %q is missing %q", err, want)
		}
	}
	if elapsed := time.Since(started); elapsed > displaySourceProbeTimeout+time.Second {
		t.Fatalf("stale X11 source probe took %v", elapsed)
	}
}

func TestX11CaptureValidatesAndRetainsLiveXvfbPipeline(t *testing.T) {
	requireX11CaptureTestStack(t)
	display, _ := startTestXvfb(t)
	t.Setenv("DISPLAY", display)

	cfg := CaptureConfig{
		FPS: 10, Bitrate: 500, HWAccel: "none", VideoCodec: VideoCodecH264,
		MaxWidth: 320, MaxHeight: 240,
	}
	encoder, err := detectGstEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStartup()
	lifetimeCtx, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	capture, err := startPreparedX11Capture(startupCtx, lifetimeCtx, cfg, encoder, true)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Stop()

	prefetched, ok := capture.frames.(*prefetchedVideoAccessUnitReader)
	if !ok || len(prefetched.units) < 2 {
		t.Fatalf("validated X11 capture retained %#v, want prefetched live access units", capture.frames)
	}
	if capture.cmd == nil || capture.cmd.Process == nil {
		t.Fatal("validated X11 capture did not retain its production process")
	}
	select {
	case <-capture.waitCh:
		t.Fatalf("validated X11 production process exited early: %v", capture.waitErr)
	default:
	}
	if _, err := capture.ReadVideoAccessUnit(); err != nil {
		t.Fatalf("read prefetched X11 access unit: %v", err)
	}
	cancelLifetime()
	select {
	case <-capture.waitCh:
	case <-time.After(3 * time.Second):
		t.Fatal("X11 production process ignored its lifetime context")
	}
}

func TestX11CaptureStartupHonorsPreparationContext(t *testing.T) {
	requireX11CaptureTestStack(t)
	display, _ := startTestXvfb(t)
	t.Setenv("DISPLAY", display)

	cfg := CaptureConfig{
		FPS: 10, Bitrate: 500, HWAccel: "none", VideoCodec: VideoCodecH264,
		MaxWidth: 320, MaxHeight: 240,
	}
	encoder, err := detectGstEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	startupCtx, cancelStartup := context.WithCancel(context.Background())
	cancelStartup()
	lifetimeCtx, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	started := time.Now()
	capture, err := startPreparedX11Capture(startupCtx, lifetimeCtx, cfg, encoder, true)
	if capture != nil {
		capture.Stop()
		t.Fatal("canceled preparation context returned a live X11 capture")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled preparation error = %v, want context canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("canceled X11 startup took %v", elapsed)
	}
}

func TestX11CaptureRejectsStaleDisplayWithGStreamerEvidence(t *testing.T) {
	requireX11CaptureTestStack(t)
	display, stop := startTestXvfb(t)
	stop()
	t.Setenv("DISPLAY", display)

	cfg := CaptureConfig{
		FPS: 30, Bitrate: 500, HWAccel: "none", VideoCodec: VideoCodecH264,
		MaxWidth: 320, MaxHeight: 240,
	}
	encoder, err := detectGstEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	capture, err := startPreparedX11Capture(ctx, ctx, cfg, encoder, true)
	if capture != nil {
		capture.Stop()
		t.Fatal("stale DISPLAY returned a live capture")
	}
	if err == nil {
		t.Fatal("stale DISPLAY returned no error")
	}
	for _, want := range []string{"failed startup validation", "GStreamer stderr", display} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("stale DISPLAY error %q is missing %q", err, want)
		}
	}
	if elapsed := time.Since(started); elapsed > captureStartupProbeTimeout(cfg.FPS)+2*time.Second {
		t.Fatalf("stale DISPLAY validation took %v", elapsed)
	}
}

// installFakeXrandr puts an executable `xrandr` script on PATH that ignores
// its arguments and prints output. Tests calling it must use t.Setenv (as this
// helper does for PATH), so they cannot run in parallel.
func installFakeXrandr(t *testing.T, output string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "xrandr")
	// Heredoc quoting keeps the canned query verbatim; the script ignores the
	// --display/--query arguments detectMonitor passes.
	body := "#!/bin/sh\ncat <<'DOUBLETAKE_TEST_XRANDR_END'\n" + output + "DOUBLETAKE_TEST_XRANDR_END\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake xrandr: %v", err)
	}
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatalf("chmod fake xrandr: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const fakeXrandrPrimaryQuery = `Screen 0: minimum 8 x 8, current 3840 x 1080, maximum 32767 x 32767
DP-3 connected primary 1920x1080+1920+0 (normal left inverted right x axis y axis) 527mm x 296mm
	1920x1080     60.00*+  59.94    50.00
DP-1 connected 1920x1080+0+0 (normal left inverted right x axis y axis) 527mm x 296mm
	1920x1080     60.00*+
HDMI-0 disconnected (normal left inverted right x axis y axis)
DP-4 connected (normal left inverted right x axis y axis) 527mm x 296mm
`

const fakeXrandrNoPrimaryQuery = `Screen 0: minimum 8 x 8, current 3200 x 1024, maximum 32767 x 32767
DP-1 connected 1920x1080+0+0 (normal left inverted right x axis y axis) 527mm x 296mm
	1920x1080     60.00*+
DP-2 connected 1280x1024+1920+0 (normal left inverted right x axis y axis) 527mm x 296mm
	1280x1024     60.00*+
`

func TestDetectMonitorSelectsRequestedOutput(t *testing.T) {
	installFakeXrandr(t, fakeXrandrPrimaryQuery)
	ctx := context.Background()

	tests := []struct {
		name                       string
		want                       string
		startX, startY, endX, endY int
	}{
		// The secondary is not the primary; selecting it must win over the
		// primary-first fallback.
		{"secondary by name", "DP-1", 0, 0, 1920, 1080},
		{"primary by name", "DP-3", 1920, 0, 3840, 1080},
		// Unknown names fall back to the primary instead of failing.
		{"unknown name falls back to primary", "NOPE-9", 1920, 0, 3840, 1080},
		// HDMI-0 is present but disconnected: it must be ignored.
		{"disconnected name falls back to primary", "HDMI-0", 1920, 0, 3840, 1080},
		// DP-4 is connected but has no parseable geometry.
		{"unparseable name falls back to primary", "DP-4", 1920, 0, 3840, 1080},
		// Empty keeps the legacy primary behavior.
		{"empty keeps legacy primary behavior", "", 1920, 0, 3840, 1080},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			startX, startY, endX, endY := detectMonitor(ctx, ":99", tc.want)
			if startX != tc.startX || startY != tc.startY || endX != tc.endX || endY != tc.endY {
				t.Fatalf("detectMonitor(want=%q) = (%d, %d, %d, %d), want (%d, %d, %d, %d)",
					tc.want, startX, startY, endX, endY, tc.startX, tc.startY, tc.endX, tc.endY)
			}
		})
	}
}

func TestDetectMonitorFallsBackToFirstConnectedWithoutPrimary(t *testing.T) {
	installFakeXrandr(t, fakeXrandrNoPrimaryQuery)
	ctx := context.Background()

	startX, startY, endX, endY := detectMonitor(ctx, ":99", "")
	if startX != 0 || startY != 0 || endX != 1920 || endY != 1080 {
		t.Fatalf("detectMonitor without primary = (%d, %d, %d, %d), want (0, 0, 1920, 1080)",
			startX, startY, endX, endY)
	}

	// An explicit selection still works when no primary line exists.
	startX, startY, endX, endY = detectMonitor(ctx, ":99", "DP-2")
	if startX != 1920 || startY != 0 || endX != 3200 || endY != 1024 {
		t.Fatalf("detectMonitor(want=DP-2) without primary = (%d, %d, %d, %d), want (1920, 0, 3200, 1024)",
			startX, startY, endX, endY)
	}
}

func TestParseXrandrGeometry(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		x, y  int
		w, h  int
		valid bool
	}{
		{
			name:  "primary with comments",
			line:  "DP-3 connected primary 1920x1080+1920+0 (normal left inverted right x axis y axis) 527mm x 296mm",
			x:     1920, y: 0, w: 1920, h: 1080, valid: true,
		},
		{
			name:  "secondary without primary",
			line:  "DP-1 connected 1920x1080+0+0 (normal left inverted right x axis y axis)",
			x:     0, y: 0, w: 1920, h: 1080, valid: true,
		},
		{
			name:  "disconnected line has no geometry",
			line:  "HDMI-0 disconnected (normal left inverted right x axis y axis)",
			valid: false,
		},
		{
			name:  "connected with no mode",
			line:  "DP-4 connected (normal left inverted right x axis y axis) 527mm x 296mm",
			valid: false,
		},
		{
			name:  "sub-640 width is rejected",
			line:  "DP-5 connected 512x384+0+0 (normal left inverted right x axis y axis)",
			valid: false,
		},
		{
			name:  "negative offset",
			line:  "DP-6 connected 1280x1024+-1920+0 (normal left inverted right x axis y axis)",
			x:     -1920, y: 0, w: 1280, h: 1024, valid: true,
		},
		{
			name:  "incomplete geometry is rejected",
			line:  "DP-7 connected 1280x1024 (normal left inverted right x axis y axis)",
			valid: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x, y, w, h, ok := parseXrandrGeometry(tc.line)
			if ok != tc.valid {
				t.Fatalf("parseXrandrGeometry(%q) ok = %t, want %t", tc.line, ok, tc.valid)
			}
			if !tc.valid {
				return
			}
			if x != tc.x || y != tc.y || w != tc.w || h != tc.h {
				t.Fatalf("parseXrandrGeometry(%q) = (%d, %d, %d, %d), want (%d, %d, %d, %d)",
					tc.line, x, y, w, h, tc.x, tc.y, tc.w, tc.h)
			}
		})
	}
}

func requireX11CaptureTestStack(t *testing.T) {
	t.Helper()
	for _, executable := range []string{"Xvfb", "gst-launch-1.0"} {
		if _, err := exec.LookPath(executable); err != nil {
			t.Skipf("%s is unavailable", executable)
		}
	}
	for _, element := range []string{"ximagesrc", "fakesink", "x264enc", "h264parse", "rtph264pay", "rtponviftimestamp", "rtpstreampay"} {
		if !hasGstElement(element) {
			t.Skipf("GStreamer element %s is unavailable", element)
		}
	}
}

func startTestXvfb(t *testing.T) (string, func()) {
	t.Helper()
	cmd := exec.Command("Xvfb", "-displayfd", "1", "-screen", "0", "320x240x24", "-nolisten", "tcp")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start Xvfb: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
			select {
			case <-waitCh:
			case <-time.After(2 * time.Second):
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				<-waitCh
			}
		})
	}
	t.Cleanup(stop)

	type displayResult struct {
		value string
		err   error
	}
	ready := make(chan displayResult, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		ready <- displayResult{value: strings.TrimSpace(line), err: readErr}
	}()
	select {
	case result := <-ready:
		if result.err != nil {
			stop()
			t.Fatalf("read Xvfb display number: %v (%s)", result.err, stderr.String())
		}
		if _, err := strconv.Atoi(result.value); err != nil {
			stop()
			t.Fatalf("Xvfb display number %q: %v", result.value, err)
		}
		return ":" + result.value, stop
	case <-time.After(3 * time.Second):
		stop()
		t.Fatalf("Xvfb did not become ready (%s)", stderr.String())
	}
	return "", stop
}
