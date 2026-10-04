package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gdamore/tcell/v2"
)

var errInterrupted = errors.New("upload interrupted")

// view holds the fixed details shown above the upload's progress.
type view struct {
	accountID  string
	fileName   string
	fileSize   int64
	maxRetries int
}

// runUI shows the upload's progress until the worker reports on done, or the
// user quits with Ctrl-C or the process gets SIGINT/SIGTERM (errInterrupted).
// It is the only code that touches screen, and it calls Fini before returning
// (also when it panics), so the terminal is restored on every exit path.
func runUI(screen tcell.Screen, v view, updates <-chan progress, done <-chan error) (last progress, err error) {
	defer func() {
		p := recover()
		screen.Fini()
		if p != nil {
			panic(p)
		}
	}()

	// Events are read on tcell's goroutine and handled here, never acted on there.
	events := make(chan tcell.Event)
	quit := make(chan struct{})
	defer close(quit)
	go screen.ChannelEvents(events, quit)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	for {
		drawProgress(screen, v, last)

		select {
		case last = <-updates:
		case uploadErr := <-done:
			return last, uploadErr
		case <-signals:
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
	case p.url == "":
		line = "Status: Creating upload..."
	case p.retryIn > 0:
		line = fmt.Sprintf("Status: Retrying in %s (attempt %d of %d). Err: %s", p.retryIn, p.attempt, v.maxRetries, p.err)
	case p.err != nil:
		line = fmt.Sprintf("Status: Retrying (attempt %d of %d). Err: %s", p.attempt, v.maxRetries, p.err)
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
