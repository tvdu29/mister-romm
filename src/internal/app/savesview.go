package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The Saves view: one row per game that has a save on the card or in RomM, with what differs.

// savesState is the part of the model used by the Saves view.
type savesState struct {
	job     *job
	text    string // progress, for the status bar
	rows    []*saveRow
	devices map[string]string
	orphans int
	scanned bool
	cur     int
	off     int
}

func (m *model) savesDir() string {
	return m.cfg("saves_dir", filepath.Join(m.data.Root, "saves"))
}

// startSaveScan compares the saves of the card with the saves of RomM, in the background.
func (m *model) startSaveScan() tea.Cmd {
	if m.saves.job != nil {
		m.toast = "The saves are already being compared"
		return nil
	}
	if m.cfg("server_url", "") == "" || m.data.Token == "" {
		m.message("Saves", []string{"This device is not paired with a server yet.", "", "Open Server and choose \"Pair this device\"."})
		return nil
	}
	if len(m.data.Platforms) == 0 {
		m.message("Saves", []string{"No platform is indexed yet.", "", "Press p and tick the platforms whose saves you want to see."})
		return nil
	}
	byPID := map[string]*saveScanPlatform{}
	var plats []saveScanPlatform
	for _, p := range m.data.Platforms {
		plats = append(plats, saveScanPlatform{id: p.ID, slug: p.Slug, name: p.Name})
	}
	for i := range plats {
		byPID[plats[i].id] = &plats[i]
	}
	for _, g := range m.data.Games {
		if p := byPID[g.PID]; p != nil {
			p.games = append(p.games, saveScanGame{id: g.ID, name: g.Name})
		}
	}
	e, dir := m.env(), m.savesDir()
	m.saves.text = "Saves: starting..."
	j, cmd := startJob(func(ctx context.Context, send func(tea.Msg)) { runSaveScan(ctx, e, dir, plats, send) })
	m.saves.job = j
	return cmd
}

func (m *model) finishSaveScan(d saveScanDone) {
	m.saves.text = ""
	if d.err != "" {
		if d.cancelled {
			m.toast = "Comparison of the saves cancelled"
			return
		}
		logError(m.cfgDir(), "saves: "+d.err)
		lines := []string{d.err}
		if strings.Contains(d.err, "HTTP 401") || strings.Contains(d.err, "HTTP 403") {
			lines = append(lines, "", "This device was probably paired before saves were supported.",
				"Pair it again (Server > Pair this device) and approve the",
				"\"assets\" and \"devices\" permissions.")
		}
		m.message("Saves", lines)
		return
	}
	m.saves.rows, m.saves.devices, m.saves.orphans, m.saves.scanned = d.rows, d.devices, d.orphans, true
	m.saves.cur, m.saves.off = 0, 0
	m.buildLeft()
}

// saveCounts counts the rows by state.
func (m *model) saveCounts() map[saveState]int {
	n := map[saveState]int{}
	for _, r := range m.saves.rows {
		n[r.state]++
	}
	return n
}

func (m *model) savesCount() string {
	if !m.saves.scanned {
		return ""
	}
	return fmt.Sprintf("%d", len(m.saves.rows))
}

// saveTag is the short state shown at the start of a row.
func saveTag(s saveState) string {
	switch s {
	case ssSame:
		return "same "
	case ssDifferent:
		return "DIFF "
	case ssLocalOnly:
		return "card "
	case ssCloudOnly:
		return "romm "
	case ssUnsupported:
		return "n/a  "
	}
	return "ERR  "
}

// shortTime shows a date of the card only when the clock was set when it was written.
func shortTime(t time.Time) string {
	if t.IsZero() || t.Year() < 2020 {
		return "date unknown"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func (m *model) deviceName(id string) string {
	if id == "" {
		return ""
	}
	if n := m.saves.devices[id]; n != "" {
		return n
	}
	return id
}

func (m *model) savesLines(iw, rows int) []string {
	c := m.saveCounts()
	var head string
	switch {
	case m.saves.job != nil:
		head = m.row(" "+m.saves.text, iw, m.accentStyle())
	case !m.saves.scanned:
		head = m.row(" Enter or r to compare the saves of the card with RomM", iw, m.dimStyle())
	default:
		head = m.row(fmt.Sprintf(" %d different   %d only in RomM   %d only on the card   %d same",
			c[ssDifferent], c[ssCloudOnly], c[ssLocalOnly], c[ssSame]), iw, m.dimStyle())
	}
	lines := []string{head}
	if !m.saves.scanned {
		return append(lines, m.wrapRows(iw, "",
			" Compares the battery saves of the card (saves/<core>/<game>.sav)",
			" with the saves RomM keeps for the same games, for example the",
			" ones uploaded by RetroArch or the web player.", "",
			" Nothing is changed: this only reads.", "",
			" Supported: SNES, NES, Game Boy / Color, GBA, Mega Drive, PlayStation.")...)
	}
	if len(m.saves.rows) == 0 {
		lines = append(lines, m.wrapRows(iw, "", " No save found for the games of the indexed platforms.")...)
		if m.saves.orphans > 0 {
			lines = append(lines, m.wrapRows(iw, fmt.Sprintf(" (%d save(s) of the card match no game of the catalog)", m.saves.orphans))...)
		}
		return lines
	}
	dateW := 16
	nameW := iw - 1 - 5 - 1 - 2 - dateW - 2 - 12
	if nameW < 12 {
		nameW, dateW = iw-9, 0
	}
	for i := m.saves.off; i < len(m.saves.rows) && len(lines) < rows; i++ {
		r := m.saves.rows[i]
		plain := " " + saveTag(r.state) + " " + padRight(trunc(r.name, nameW), nameW)
		if dateW > 0 {
			when, who := "", ""
			if r.cloud != nil {
				when, who = r.cloud.UpdatedAt.Local().Format("2006-01-02 15:04"), m.deviceName(r.cloud.Device)
				if who == "" {
					who = r.cloud.Emulator
				}
			}
			plain += "  " + padRight(when, dateW) + "  " + trunc(who, 12)
		}
		switch {
		case i == m.saves.cur && m.focus == fRight:
			lines = append(lines, m.row(plain, iw, m.cursorStyle()))
		case i == m.saves.cur:
			lines = append(lines, m.row(plain, iw, m.accentStyle()))
		case r.state == ssDifferent || r.state == ssError:
			lines = append(lines, m.row(plain, iw, m.accStyle(kQuestion)))
		case r.state == ssSame || r.state == ssUnsupported:
			lines = append(lines, m.row(plain, iw, m.dimStyle()))
		default:
			lines = append(lines, m.row(plain, iw, m.sNorm))
		}
	}
	return lines
}

// wrapRows renders plain lines cut to the panel width.
func (m *model) wrapRows(iw int, text ...string) []string {
	out := make([]string, len(text))
	for i, t := range text {
		out[i] = m.row(t, iw, m.sNorm)
	}
	return out
}

func (m *model) clampSaves() {
	s := &m.saves
	if s.cur >= len(s.rows) {
		s.cur = len(s.rows) - 1
	}
	if s.cur < 0 {
		s.cur = 0
	}
	rows := m.listRows()
	if s.cur < s.off {
		s.off = s.cur
	}
	if s.cur >= s.off+rows {
		s.off = s.cur - rows + 1
	}
	if s.off < 0 {
		s.off = 0
	}
}

// savesKey handles the keys of the Saves view (right panel).
func (m *model) savesKey(k string) tea.Cmd {
	if !m.saves.scanned {
		if k == "enter" || k == "r" {
			return m.startSaveScan()
		}
		return nil
	}
	rows := m.listRows()
	switch k {
	case "r":
		return m.startSaveScan()
	case "up", "k":
		m.saves.cur--
	case "down", "j":
		m.saves.cur++
	case "pgup":
		m.saves.cur -= rows
	case "pgdown":
		m.saves.cur += rows
	case "home", "g":
		m.saves.cur = 0
	case "end", "G":
		m.saves.cur = len(m.saves.rows) - 1
	case "enter":
		if m.saves.cur < len(m.saves.rows) {
			m.openSaveInfo(m.saves.rows[m.saves.cur])
		}
	}
	m.clampSaves()
	return nil
}

func (m *model) openSaveInfo(r *saveRow) {
	state := map[saveState]string{
		ssSame:        "same game data on the card and in RomM",
		ssDifferent:   "the card and RomM have different saves",
		ssLocalOnly:   "only on the card",
		ssCloudOnly:   "only in RomM",
		ssUnsupported: "the save format of this core is not handled yet",
		ssError:       "could not be compared",
	}[r.state]
	lines := []string{
		"Game:      " + r.name,
		"Platform:  " + r.platform + "   (MiSTer core: " + r.core + ")",
		"State:     " + state,
	}
	if r.err != "" {
		lines = append(lines, "Problem:   "+r.err)
	}
	lines = append(lines, "", "On the card:")
	if r.local != nil {
		lines = append(lines, "  "+r.local.path, "  "+humanSize(r.local.size)+", written "+shortTime(r.local.modTime))
	} else {
		lines = append(lines, "  no save")
	}
	lines = append(lines, "", "In RomM:")
	if s := r.cloud; s != nil {
		slot := s.Slot
		if slot == "" {
			slot = "(none)"
		}
		lines = append(lines, "  "+s.FileName,
			"  "+humanSize(s.Size)+", uploaded "+s.UpdatedAt.Local().Format("2006-01-02 15:04"),
			"  slot "+slot+", emulator "+orDash(s.Emulator)+", from "+orDash(m.deviceName(s.Device)))
		if r.cloudN > 1 {
			lines = append(lines, fmt.Sprintf("  %d versions kept by RomM", r.cloudN))
		}
	} else {
		lines = append(lines, "  no save")
	}
	if r.state == ssDifferent {
		lines = append(lines, "", "The date of the card is only right if the clock was set when the",
			"game saved (it is after this program has run once since boot).")
	}
	m.pop = &popup{kind: "info", title: "Save", lines: lines}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
