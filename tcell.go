package main

import (
	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

// tCellDraw draws text on row y starting at column x, cut off at the right edge of the screen.
func tCellDraw(screen tcell.Screen, x, y int, style tcell.Style, text string) {
	screenW, _ := screen.Size()
	for _, r := range text {
		width := runewidth.RuneWidth(r)
		if width == 0 {
			// Control characters and combining marks would corrupt the layout.
			continue
		}
		if x+width > screenW {
			break
		}
		screen.SetContent(x, y, r, nil, style)
		x += width
	}
}

func getChars(char string, count int) string {
	final := ""
	for x := 0; x < count; x++ {
		final += char
	}
	return final
}
