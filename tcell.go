package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/gdamore/tcell/v2/terminfo"
	"golang.org/x/term"
)

var errInterrupted = errors.New("upload interrupted")

// stopSignal is the error runUI returns when a signal other than SIGINT ends it.
type stopSignal struct{ sig syscall.Signal }

func (s stopSignal) Error() string { return fmt.Sprintf("upload stopped by signal (%s)", s.sig) }

// view holds the fixed details shown above the upload's progress.
type view struct {
	accountID  string
	fileName   string
	fileSize   int64
	maxRetries int
}

// runUI shows the upload's progress until the worker reports on done, the
// user quits with Ctrl-C or SIGINT (errInterrupted), or another signal arrives
// (stopSignal). signals must be registered before screen.Init. runUI is the
// only code that touches screen, and it restores the terminal before
// returning, also when it panics: with Fini, or with restore if Fini hangs
// (see finish). restore may be nil.
func runUI(screen tcell.Screen, v view, updates <-chan progress, done <-chan error, signals <-chan os.Signal, restore func()) (last progress, err error) {
	defer finish(screen, finiTimeout, restore)

	// Events are read on tcell's goroutine and handled here, never acted on there.
	events := make(chan tcell.Event)
	quit := make(chan struct{})
	defer close(quit)
	go screen.ChannelEvents(events, quit)

	// Redraw only when something changed. Ignored keys are dropped without a
	// redraw, so a paste or a held-down key can't back up tcell's input.
	drawProgress(screen, v, last)
	for {
		select {
		case last = <-updates:
			drawProgress(screen, v, last)
		case uploadErr := <-done:
			return last, uploadErr
		case sig := <-signals:
			if s, ok := sig.(syscall.Signal); ok && sig != os.Interrupt {
				return last, stopSignal{s}
			}
			return last, errInterrupted
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			switch ev := ev.(type) {
			case *tcell.EventKey:
				if ev.Key() == tcell.KeyCtrlC {
					return last, errInterrupted
				}
			case *tcell.EventResize:
				drawProgress(screen, v, last)
				screen.Sync()
			}
		}
	}
}

// finiTimeout bounds how long finish waits for tcell to restore the terminal.
const finiTimeout = 2 * time.Second

// finish restores the terminal with screen.Fini, or with fallback if Fini
// hasn't returned after timeout. tcell (v2.8.1 through v2.13.10) can hang in
// Fini when input is still queued: its input goroutine blocks sending to a
// full channel that nothing reads any more, and Fini waits for it. The stuck
// goroutines are left behind; main exits soon after.
func finish(screen tcell.Screen, timeout time.Duration, fallback func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		screen.Fini()
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		if fallback != nil {
			fallback()
		}
	}
}

// savedTerminal is the terminal's state from before tcell took it over.
type savedTerminal struct {
	tty   *os.File
	state *term.State
	ti    *terminfo.Terminfo // The $TERM entry tcell uses, or nil if there is none.
}

// saveTerminal records the state of the controlling terminal, so restore can
// put it back without tcell. It returns nil if there is no /dev/tty (Windows,
// or no controlling terminal); restore then does nothing.
func saveTerminal() *savedTerminal {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	state, err := term.GetState(int(tty.Fd()))
	if err != nil {
		tty.Close()
		return nil
	}
	ti, _ := tcell.LookupTerminfo(os.Getenv("TERM"))
	return &savedTerminal{tty: tty, state: state, ti: ti}
}

// restore resets the terminal's mode to the saved state, then sends what
// tcell's Fini would have: show the cursor, reset colors and attributes,
// leave keypad mode, turn line wrapping back on, then clear the screen and
// leave the alternate screen unless TCELL_ALTSCREEN=disable.
func (t *savedTerminal) restore() {
	if t == nil {
		return
	}
	_ = term.Restore(int(t.tty.Fd()), t.state)
	if t.ti == nil {
		return
	}
	seqs := []string{t.ti.ShowCursor, t.ti.ResetFgBg, t.ti.AttrOff, t.ti.ExitKeypad, t.ti.EnableAutoMargin}
	if os.Getenv("TCELL_ALTSCREEN") != "disable" {
		seqs = append(seqs, t.ti.Clear, t.ti.ExitCA)
	}
	for _, s := range seqs {
		t.ti.TPuts(t.tty, s)
	}
}

func drawProgress(screen tcell.Screen, v view, p progress) {
	screen.Clear()
	screenW, _ := screen.Size()

	// Boundaries
	tCellDraw(screen, 0, 1, screenW, 1, tcell.StyleDefault, getChars("~", screenW))

	// Text
	line := fmt.Sprintf("Account ID: %s", v.accountID)
	tCellDraw(screen, 0, 0, len(line), 0, tcell.StyleDefault, line)

	// Progress
	line = fmt.Sprintf("  ==> File: %s", v.fileName)
	tCellDraw(screen, 0, 3, len(line), 3, tcell.StyleDefault, line)

	percent := int64(100)
	if v.fileSize > 0 {
		percent = p.offset * 100 / v.fileSize
	}
	line = fmt.Sprintf("      || Bytes Uploaded: %d (%d%%)", p.offset, percent)
	tCellDraw(screen, 0, 4, len(line), 4, tcell.StyleDefault, line)
	line = fmt.Sprintf("      || Total File Size: %d", v.fileSize)
	tCellDraw(screen, 0, 5, len(line), 5, tcell.StyleDefault, line)

	// Status, wrapped over two rows since errors can be long.
	switch {
	case p.retryIn > 0:
		line = fmt.Sprintf("Status: Retrying in %s (attempt %d of %d). Err: %s", p.retryIn, p.attempt, v.maxRetries, p.err)
	case p.err != nil:
		line = fmt.Sprintf("Status: Retrying (attempt %d of %d). Err: %s", p.attempt, v.maxRetries, p.err)
	case p.url == "":
		line = "Status: Creating upload..."
	default:
		line = "Status: Uploading..."
	}
	// Errors may hold server text, line breaks included.
	line = strings.Join(strings.Fields(printable(line)), " ")
	tCellDraw(screen, 0, 7, screenW, 8, tcell.StyleDefault, line)

	line = "Press Ctrl-C to cancel."
	tCellDraw(screen, 0, 10, len(line), 10, tcell.StyleDefault, line)

	// Display
	screen.Show()
}

func tCellDraw(screen tcell.Screen, x1, y1, x2, y2 int, style tcell.Style, text string) {
	row := y1
	col := x1
	for _, r := range text {
		screen.SetContent(col, row, r, nil, style)
		col++
		if col >= x2 {
			row++
			col = x1
		}
		if row > y2 {
			break
		}
	}
}

func getChars(char string, count int) string {
	final := ""
	for x := 0; x < count; x++ {
		final += char
	}
	return final
}
