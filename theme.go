package main

import (
	"image/color"
	"os"

	"charm.land/lipgloss/v2"
)

// Pal is the color set of one kind of window.
type Pal struct {
	Accent color.Color // border and title
	CurFG  color.Color // line under the cursor
	CurBG  color.Color
}

// Window kinds. Each one has its own color so you always know what you are looking at, and each
// title also says what it is (color is never the only cue).
const (
	kGames    = "games"    // the catalog, the menu panel, game context menus
	kSettings = "settings" // server and settings windows
	kMessage  = "message"  // information: game info, help, results, downloads
	kQuestion = "question" // confirmations
)

// Theme holds the colors and border glyphs. The Linux console only has 8 background colors and
// maps 256-color numbers to the nearest of 16, so on TERM=linux (or colors=16) plain 0-15 numbers
// are used.
type Theme struct {
	Pal     map[string]Pal
	SelFG   color.Color // ticked rows (whole row)
	SelBG   color.Color
	BadFG   color.Color // errors (server unreachable, token rejected)
	BadBG   color.Color
	Dim     color.Color // secondary text, unfocused border
	Normal  color.Color
	PopupBG color.Color
	Border  [6]string // tl, tr, bl, br, horizontal, vertical
	Name    string
}

func c(n string) color.Color { return lipgloss.Color(n) }

// newTheme: colors is "auto", "16" or "256".
func newTheme(ascii bool, colors string) Theme {
	t := Theme{Pal: map[string]Pal{}}
	sixteen := colors == "16" || (colors != "256" && (os.Getenv("TERM") == "linux" || os.Getenv("ROMM_COLORS") == "16"))
	if sixteen {
		t.Name = "16 colors (console)"
		t.Pal[kGames] = Pal{c("13"), c("15"), c("5")}    // magenta
		t.Pal[kSettings] = Pal{c("14"), c("0"), c("6")}  // cyan
		t.Pal[kMessage] = Pal{c("10"), c("0"), c("2")}   // green
		t.Pal[kQuestion] = Pal{c("11"), c("15"), c("3")} // yellow
		t.SelFG, t.SelBG = c("15"), c("4")
		t.BadFG, t.BadBG = c("15"), c("1")
		t.Dim = c("7")
	} else {
		t.Name = "256 colors"
		t.Pal[kGames] = Pal{c("212"), c("0"), c("212")}    // pink
		t.Pal[kSettings] = Pal{c("51"), c("0"), c("51")}   // cyan
		t.Pal[kMessage] = Pal{c("120"), c("0"), c("120")}  // green
		t.Pal[kQuestion] = Pal{c("214"), c("0"), c("214")} // orange
		t.SelFG, t.SelBG = c("15"), c("99")
		t.BadFG, t.BadBG = c("15"), c("160")
		t.Dim = c("245")
	}
	t.Normal, t.PopupBG = c("15"), c("0")
	if ascii {
		t.Border = [6]string{"+", "+", "+", "+", "-", "|"}
	} else {
		t.Border = [6]string{"┌", "┐", "└", "┘", "─", "│"}
	}
	return t
}
