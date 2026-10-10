package main

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func newSimScreen(t *testing.T) tcell.SimulationScreen {
	t.Helper()
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(80, 25)
	return screen
}

// screenText returns the screen's contents with rows joined by newlines.
func screenText(screen tcell.SimulationScreen) string {
	cells, width, _ := screen.GetContents()
	var b strings.Builder
	for i, c := range cells {
		if i > 0 && i%width == 0 {
			b.WriteByte('\n')
		}
		if len(c.Runes) > 0 {
			b.WriteRune(c.Runes[0])
		}
	}
	return b.String()
}

func TestRunUIReturnsWhenUploadFinishes(t *testing.T) {
	screen := newSimScreen(t)
	updates := make(chan progress)
	done := make(chan error, 1)
	go func() {
		updates <- progress{url: "https://example.com/1", offset: 10}
		done <- nil
	}()

	last, err := runUI(screen, view{fileSize: 10}, updates, done, nil, nil)
	if err != nil || last.offset != 10 {
		t.Fatalf("runUI = %+v, %v, want offset 10 and no error", last, err)
	}
	if _, width, _ := screen.GetContents(); width != 0 {
		t.Error("screen was not finalized")
	}
}

func TestRunUIReturnsUploadError(t *testing.T) {
	screen := newSimScreen(t)
	failed := errors.New("boom")
	done := make(chan error, 1)
	done <- failed

	if _, err := runUI(screen, view{}, nil, done, nil, nil); !errors.Is(err, failed) {
		t.Fatalf("runUI err = %v, want %v", err, failed)
	}
}

func TestRunUIStopsOnCtrlC(t *testing.T) {
	screen := newSimScreen(t)
	screen.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)

	// The upload never finishes, so only Ctrl-C can end runUI.
	if _, err := runUI(screen, view{}, nil, nil, nil, nil); !errors.Is(err, errInterrupted) {
		t.Fatalf("runUI err = %v, want %v", err, errInterrupted)
	}
	if _, width, _ := screen.GetContents(); width != 0 {
		t.Error("screen was not finalized")
	}
}

// countingScreen counts how often the screen is shown.
type countingScreen struct {
	tcell.SimulationScreen
	shows int
}

func (s *countingScreen) Show() {
	s.shows++
	s.SimulationScreen.Show()
}

func TestRunUIIgnoresKeysWithoutRedrawing(t *testing.T) {
	screen := &countingScreen{SimulationScreen: newSimScreen(t)}
	for _, r := range "abcde" {
		screen.InjectKey(tcell.KeyRune, r, tcell.ModNone)
	}
	screen.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)

	if _, err := runUI(screen, view{}, nil, nil, nil, nil); !errors.Is(err, errInterrupted) {
		t.Fatalf("runUI err = %v, want %v", err, errInterrupted)
	}
	// Only the first frame: redrawing for every key lets a paste back up tcell's input.
	if screen.shows != 1 {
		t.Errorf("screen shown %d times, want 1", screen.shows)
	}
}

// hangingScreen is a screen whose Fini blocks until release is closed, like
// tcell's when its input goroutine is stuck.
type hangingScreen struct {
	tcell.SimulationScreen
	release chan struct{}
}

func (s hangingScreen) Fini() { <-s.release }

func TestFinishFallsBackWhenFiniHangs(t *testing.T) {
	screen := hangingScreen{newSimScreen(t), make(chan struct{})}
	defer close(screen.release)

	restored := false
	finish(screen, 10*time.Millisecond, func() { restored = true })
	if !restored {
		t.Error("fallback not called after Fini hung")
	}
}

func TestFinishUsesFini(t *testing.T) {
	screen := newSimScreen(t)

	finish(screen, time.Hour, func() { t.Error("fallback called although Fini returned") })
	if _, width, _ := screen.GetContents(); width != 0 {
		t.Error("screen was not finalized")
	}
}

func TestRunUIMapsSignals(t *testing.T) {
	tests := []struct {
		sig  os.Signal
		want error
	}{
		{os.Interrupt, errInterrupted},
		{syscall.SIGTERM, stopSignal{syscall.SIGTERM}},
		{syscall.SIGHUP, stopSignal{syscall.SIGHUP}},
	}
	for _, tt := range tests {
		screen := newSimScreen(t)
		signals := make(chan os.Signal, 1)
		signals <- tt.sig
		if _, err := runUI(screen, view{}, nil, nil, signals, nil); err != tt.want {
			t.Errorf("runUI after %v: err = %v, want %v", tt.sig, err, tt.want)
		}
	}
}

func TestDrawProgress(t *testing.T) {
	v := view{accountID: "acct", fileName: "video.mp4", fileSize: 200, maxRetries: 8}
	tests := []struct {
		name string
		p    progress
		want []string
	}{
		{"creating", progress{}, []string{"Account ID: acct", "File: video.mp4", "Status: Creating upload..."}},
		{"uploading", progress{url: "u", offset: 50}, []string{"Bytes Uploaded: 50 (25%)", "Total File Size: 200", "Status: Uploading..."}},
		{"waiting", progress{url: "u", err: errors.New("boom"), attempt: 2, retryIn: 4 * time.Second}, []string{"Status: Retrying in 4s (attempt 2 of 8). Err: boom"}},
		{"retrying", progress{url: "u", err: errors.New("boom"), attempt: 2}, []string{"Status: Retrying (attempt 2 of 8). Err: boom"}},
		{"retrying create", progress{err: errors.New("boom"), attempt: 1, retryIn: time.Second}, []string{"Status: Retrying in 1s (attempt 1 of 8). Err: boom"}},
		{"server text", progress{url: "u", err: errors.New("bad\x1b[31m\n\treply"), attempt: 1}, []string{"Err: bad[31m reply"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			screen := newSimScreen(t)
			defer screen.Fini()
			drawProgress(screen, v, tt.p)
			text := screenText(screen)
			for _, w := range tt.want {
				if !strings.Contains(text, w) {
					t.Errorf("screen is missing %q:\n%s", w, text)
				}
			}
		})
	}
}
