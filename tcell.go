package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/gdamore/tcell/v2"
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
// only code that touches screen, and it calls Fini before returning, also
// when it panics.
func runUI(screen tcell.Screen, v view, updates <-chan progress, done <-chan error, signals <-chan os.Signal) (last progress, err error) {
	defer screen.Fini()

	// Events are read on tcell's goroutine and handled here, never acted on there.
	events := make(chan tcell.Event)
	quit := make(chan struct{})
	defer close(quit)
	go screen.ChannelEvents(events, quit)

	for {
		drawProgress(screen, v, last)

		select {
		case last = <-updates:
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
				screen.Sync()
			}
		}
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
