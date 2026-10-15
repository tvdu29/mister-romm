package app

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// The screen must fit the terminal whatever is shown: a line wider than the console wraps and
// scrambles the whole interface, a taller screen scrolls it. Checked for many sizes, themes and
// windows, with wide characters and very long names.

func richModel(th Theme) *model {
	d := &Data{Config: map[string]string{"server_url": "http://romm.lan"}, Root: "/x", Token: "tok"}
	names := []string{
		"Super Mario Land (World).gb",
		"ゲームボーイ 冒険の書 (Japan).gb",
		"Emoji 🎮 Quest (USA).gb",
		strings.Repeat("A very long game name ", 12) + ".gb",
		"x.gb",
	}
	for p := 1; p <= 3; p++ {
		pl := &Platform{ID: fmt.Sprint(p), Slug: "gb", Name: fmt.Sprintf("Platform %d with a rather long name", p)}
		for i := 0; i < 60; i++ {
			name := fmt.Sprintf("%02d %s", i, names[i%len(names)])
			d.Games = append(d.Games, &Game{ID: fmt.Sprint(p*1000 + i), PID: pl.ID, Platform: pl.Name, Name: name,
				Low: strings.ToLower(name), Size: int64(i+1) << 20, Installed: i%7 == 0})
			pl.Count++
		}
		d.Platforms = append(d.Platforms, pl)
	}
	m := newModel(d, th)
	m.pop, m.toast = nil, ""
	m.focus = fRight
	return m
}

func (m *model) addQueue() {
	for i, st := range []string{"done", "running", "verifying", "failed", "queued", "cancelled", "queued"} {
		g := m.data.Games[i*3]
		m.queue = append(m.queue, &dl{g: g, state: st, done: int64(i) << 19, total: 5 << 20, speed: 1 << 20, err: "HTTP 500: boom",
			item: dlItem{name: g.Name, size: 5 << 20}})
	}
}

func checkFits(t *testing.T, label string, m *model) {
	t.Helper()
	out := stripANSI(m.render())
	lines := strings.Split(out, "\n")
	if m.w < 50 || m.h < 10 {
		if len(lines) != 1 {
			t.Errorf("%s: the too-small message must be one line", label)
		}
		return
	}
	if len(lines) > m.h {
		t.Errorf("%s: %d lines on a %d-line terminal", label, len(lines), m.h)
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w > m.w {
			t.Errorf("%s: line %d is %d columns wide on a %d-column terminal: %q", label, i, w, m.w, l)
			return
		}
	}
}

func TestRenderFitsEveryTerminalAndWindow(t *testing.T) {
	themes := map[string]Theme{
		"box-auto": newTheme(false, "auto"), "ascii-16": newTheme(true, "16"), "box-256": newTheme(false, "256"),
	}
	sizes := [][2]int{{30, 8}, {49, 20}, {50, 10}, {51, 11}, {64, 20}, {80, 25}, {80, 30}, {100, 40}, {160, 50}, {200, 60}}
	windows := map[string]func(m *model){
		"none":      func(m *model) {},
		"help":      func(m *model) { m.openHelp() },
		"settings":  func(m *model) { m.openSettings() },
		"server":    func(m *model) { m.openServer() },
		"downloads": func(m *model) { m.addQueue(); m.openDownloads() },
		"info":      func(m *model) { m.openInfo(m.data.Games[1]) },
		"message": func(m *model) {
			m.message("A title", []string{"line one", strings.Repeat("a long line ", 15), "line three"})
		},
		"confirm": func(m *model) { m.confirm("Sure?", []string{"Really do it?", strings.Repeat("x", 90)}, nil) },
		"input":   func(m *model) { m.openInput("Name", []string{"Enter a name"}, strings.Repeat("t", 70), nil) },
		"menu":    func(m *model) { m.focus = fRight; m.contextMenu() },
		"tool": func(m *model) {
			m.pop = &popup{kind: "tool", title: "Diagnostics", running: true}
			for i := 0; i < 60; i++ {
				m.pop.lines = append(m.pop.lines, fmt.Sprintf("line %d of a long report %s", i, strings.Repeat("-", 70)))
			}
		},
		"checklist": func(m *model) {
			var items []chkItem
			for i := 0; i < 80; i++ {
				items = append(items, chkItem{id: fmt.Sprint(i), label: fmt.Sprintf("Platform number %d (123 games) -> FOLDER", i), on: i%3 == 0})
			}
			m.pop = &popup{kind: "checklist", title: "Platforms to index", chk: items}
		},
		"pairing": func(m *model) {
			m.pop = &popup{kind: "pairing", title: "Pair this device"}
			m.pair = &pairState{userCode: "ABCD-1234", url: "http://romm.lan/device?code=ABCD-1234"}
		},
	}
	for tn, th := range themes {
		for _, sz := range sizes {
			for wn, open := range windows {
				for _, view := range []string{"all", "platform", "installed", "downloads", "settings", "server", "sync"} {
					if wn != "none" && view != "all" {
						continue // windows are checked over the default view only
					}
					m := richModel(th)
					m.w, m.h = sz[0], sz[1]
					m.addQueue()
					for i, it := range m.left {
						if it.kind == view || (view == "platform" && it.kind == "plat") {
							m.selectLeft(i)
							break
						}
					}
					if wn == "downloads" {
						m.queue = m.queue[:0]
					}
					open(m)
					checkFits(t, fmt.Sprintf("%s %dx%d window=%s view=%s", tn, sz[0], sz[1], wn, view), m)
					m.toast = "A toast " + strings.Repeat("long ", 30)
					m.saveErr = "config.ini: permission denied"
					checkFits(t, fmt.Sprintf("%s %dx%d window=%s view=%s +toast", tn, sz[0], sz[1], wn, view), m)
				}
			}
		}
	}
}

func TestRenderShowsTheSelectionCursorAndSearch(t *testing.T) {
	m := richModel(newTheme(true, "auto"))
	m.w, m.h = 100, 30
	m.sel[m.data.Games[2].Key()] = true
	m.query, m.searching = "mario", true
	m.refilter()
	out := stripANSI(m.render())
	for _, want := range []string{"/ mario_", "Super Mario Land"} {
		if !strings.Contains(out, want) {
			t.Errorf("screen lacks %q:\n%s", want, out)
		}
	}
	m.query, m.searching = "", false
	m.refilter()
	out = stripANSI(m.render())
	if !strings.Contains(out, "[X]") || !strings.Contains(out, "[ ]") {
		t.Errorf("tick boxes missing:\n%s", out)
	}
	if !strings.Contains(out, "*") {
		t.Errorf("the marker of games already on the card is missing:\n%s", out)
	}
}

func TestRenderSmallTerminalMessage(t *testing.T) {
	m := richModel(newTheme(true, "auto"))
	m.w, m.h = 49, 9
	if out := m.render(); !strings.Contains(out, "too small") {
		t.Errorf("render = %q", out)
	}
}
