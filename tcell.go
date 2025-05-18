package main

import "github.com/gdamore/tcell/v2"

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
