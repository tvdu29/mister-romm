package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// ------------------------------------------------------------------ text helpers

// width is a fast version of lipgloss.Width for the common case (ASCII text, possibly with
// color escape sequences). Measuring grapheme clusters is the most expensive part of a redraw on a
// slow CPU; anything that is not plain ASCII still goes through lipgloss.
func width(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b == 0x1b:
			if i+1 < len(s) && s[i+1] == '[' { // CSI ... final byte
				i += 2
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
				continue
			}
			return lipgloss.Width(s)
		case b >= 0x80 || b < 0x20 || b == 0x7f:
			return lipgloss.Width(s)
		default:
			n++
		}
	}
	return n
}

func trunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if width(s) <= w {
		return s
	}
	if w <= 3 {
		return ansi.Truncate(s, w, "")
	}
	return ansi.Truncate(s, w-3, "") + "..."
}

func padRight(s string, w int) string {
	if d := w - width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func padLeft(s string, w int) string {
	if d := w - width(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

func (m *model) st() lipgloss.Style { return lipgloss.NewStyle() }

func (m *model) cursorStyle() lipgloss.Style { return m.curSt[kGames] }
func (m *model) selStyle() lipgloss.Style    { return m.sSel }
func (m *model) dimStyle() lipgloss.Style    { return m.sDim }
func (m *model) accentStyle() lipgloss.Style { return m.accSt[kGames] }

// row renders a plain string at exactly w cells with the given style.
func (m *model) row(plain string, w int, s lipgloss.Style) string {
	return s.Render(padRight(trunc(plain, w), w))
}

// box draws a bordered panel with its title in the top border, exactly w x h cells.
func (m *model) box(title string, lines []string, w, h int, focused bool, kind string) string {
	bc := m.th.Dim
	if focused {
		bc = m.th.Pal[kind].Accent
	}
	bs := m.st().Foreground(bc)
	ts := m.st().Foreground(bc).Bold(true)
	b := m.th.Border
	iw := w - 2
	t := " " + title + " "
	if width(t) > iw-2 {
		t = trunc(t, iw-2)
	}
	tw := width(t)
	var out []string
	out = append(out, bs.Render(b[0]+b[4])+ts.Render(t)+bs.Render(strings.Repeat(b[4], iw-1-tw)+b[1]))
	for i := 0; i < h-2; i++ {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		out = append(out, bs.Render(b[5])+padRight(line, iw)+bs.Render(b[5]))
	}
	out = append(out, bs.Render(b[2]+strings.Repeat(b[4], iw)+b[3]))
	return strings.Join(out, "\n")
}

// ------------------------------------------------------------------------ view

func (m *model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m *model) render() string {
	w, h := m.w, m.h
	if w < 50 || h < 10 {
		return "Terminal too small (need at least 50x10)."
	}
	lw := w / 4
	if lw < 24 {
		lw = 24
	}
	if lw > 32 {
		lw = 32
	}
	bh := m.bodyH()
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.leftPanel(lw, bh), m.rightPanel(w-lw, bh))
	base := m.header(w) + "\n" + body + "\n" + m.footer(w)
	if m.pop == nil {
		return base
	}
	p := m.renderPopup()
	pw, ph := lipgloss.Size(p)
	x, y := (w-pw)/2, (h-ph)/2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return lipgloss.NewCompositor(
		lipgloss.NewLayer(base),
		lipgloss.NewLayer(p).X(x).Y(y).Z(1),
	).Render()
}

// header is the status bar: program name, server state, platforms and games, and what is running.
func (m *model) header(w int) string {
	title := m.cursorStyle().Render(" mister-romm ")
	dim := m.dimStyle()
	plat := fmt.Sprintf("%d", len(m.data.Platforms))
	games := fmt.Sprintf("%d", len(m.data.Games))
	tail := dim.Render("  Platforms ") + m.sNorm.Render(plat) + dim.Render("  Games ") + m.sNorm.Render(games)
	used := width(title) + width(" Server ") + width(tail)
	bar := title + dim.Render(" Server ") + m.srvBadge(w-used-1) + tail
	if m.saveErr != "" {
		if room := w - width(bar) - 3; room > 12 {
			bar += "  " + m.sBad.Render(trunc(" NOT SAVED: "+m.saveErr+" ", room))
		}
	} else if busy := m.busyText(); busy != "" {
		if room := w - width(bar) - 3; room > 8 {
			bar += "  " + m.accStyle(kGames).Render(trunc(busy, room))
		}
	}
	if pad := w - width(bar); pad > 0 {
		bar += strings.Repeat(" ", pad)
	}
	return bar
}

// busyText describes the background work, for the status bar.
func (m *model) busyText() string {
	switch {
	case m.syncJob != nil:
		return m.syncText
	case m.dlJob != nil:
		done, n := 0, len(m.dlBatch)
		var cur *dl
		for _, d := range m.dlBatch {
			if d.state == "done" {
				done++
			}
			if d.state == "running" || d.state == "verifying" {
				cur = d
			}
		}
		t := fmt.Sprintf("Downloading %d/%d", done+1, n)
		if cur != nil && cur.speed > 0 {
			t += " " + humanSize(cur.speed) + "/s"
		}
		return t
	}
	return ""
}

func (m *model) footer(w int) string {
	var s string
	switch {
	case m.toast != "":
		return m.row(" "+m.toast, w, m.accentStyle())
	case m.pop != nil && m.pop.kind == "checklist":
		s = " Space tick   a all   x none   Enter apply and sync   Esc cancel"
	case m.pop != nil && m.pop.kind == "tool":
		s = " Esc stop / close"
	case m.pop != nil && m.pop.kind == "pairing":
		s = " approve the code on your phone   Esc cancel"
	case m.pop != nil && m.pop.kind == "input":
		s = " type the text   Enter save   Esc cancel   Ctrl+U clear"
	case m.pop != nil:
		s = " Up/Down choose   Enter confirm   Esc close"
	case m.searching:
		s = " type to filter   Enter keep the filter   Esc clear"
	case m.focus == fLeft:
		s = " Up/Down choose   Enter/Right open   Tab panels   ? help   q quit"
	case m.isListView():
		s = " Space tick   / search   Enter menu   d download   a all   x none   Tab panels   ? help   q quit"
	default:
		s = " Left back   Tab panels   ? help   q quit"
	}
	return m.row(s, w, m.dimStyle())
}

func (m *model) leftPanel(w, h int) string {
	key := fmt.Sprintf("%d|%d|%d|%d|%v|%d|%d", w, h, m.lcur, len(m.left), m.focus, m.activeDownloads(), len(m.sel))
	for _, it := range m.left {
		key += "|" + it.name + it.count
	}
	if key == m.leftKey {
		return m.leftStr
	}
	m.leftKey, m.leftStr = key, m.leftPanelBuild(w, h)
	return m.leftStr
}

func (m *model) leftPanelBuild(w, h int) string {
	iw := w - 2
	rows := h - 2
	start := 0
	if m.lcur >= rows {
		start = m.lcur - rows + 1
	}
	var lines []string
	for i := start; i < len(m.left) && len(lines) < rows; i++ {
		it := m.left[i]
		if it.kind == "sep" {
			lines = append(lines, m.dimStyle().Render(strings.Repeat(m.th.Border[4], iw)))
			continue
		}
		prefix := " "
		if i == m.lcur && m.focus != fLeft {
			prefix = ">"
		}
		nameW := iw - 2 - len(it.count) - 1
		if nameW < 4 {
			nameW = 4
		}
		plain := prefix + padRight(trunc(it.name, nameW), nameW) + " " + it.count
		switch {
		case i == m.lcur && m.focus == fLeft:
			lines = append(lines, m.row(plain, iw, m.cursorStyle()))
		case i == m.lcur:
			lines = append(lines, m.row(plain, iw, m.accentStyle()))
		default:
			lines = append(lines, m.row(plain, iw, m.sNorm))
		}
	}
	return m.box("Menu", lines, w, h, m.focus == fLeft, kGames)
}

func (m *model) rightPanel(w, h int) string {
	iw := w - 2
	focused := m.focus == fRight
	it := m.cur()
	title := it.name
	if it.kind == "all" {
		title = "Catalog - all games"
	}
	var lines []string
	kind := kGames

	switch it.kind {
	case "server":
		lines, kind = m.serverLines(), kSettings
	case "settings":
		lines, kind = m.settingsLines(), kSettings
	case "downloads":
		lines, kind = m.downloadLines(iw), kMessage
	case "sync":
		lines, kind = m.actionLines("Sync catalog", "Downloads the list of games of your indexed platforms (not the games).", "Incremental: only what changed since the last time.", "Enter or s to start, c to cancel while it runs."), kSettings
	case "platforms":
		lines, kind = m.actionLines("Platforms to index", "Choose which platforms of your server appear in the catalog.", "Nothing is downloaded: you pick the games later.", "Enter or p to open the list."), kSettings
	default:
		lines = m.listLines(iw, h-2, it.kind)
	}
	return m.box(title, lines, w, h, focused, kind)
}

func (m *model) listLines(iw, rows int, kind string) []string {
	var lines []string
	// search line
	q := ""
	switch {
	case m.searching:
		q = m.accentStyle().Render("/ " + m.query + "_")
	case m.query != "":
		q = m.accentStyle().Render("/ "+m.query) + m.dimStyle().Render("   (Esc in the list: back, / to edit)")
	default:
		q = m.dimStyle().Render("/ to search")
	}
	count := m.dimStyle().Render(fmt.Sprintf("%d shown  %d ticked", len(m.view), len(m.sel)))
	gap := iw - width(q) - width(count) - 2
	if gap < 1 {
		count, gap = "", 1
	}
	lines = append(lines, " "+q+strings.Repeat(" ", gap)+count)

	if len(m.data.Games) == 0 {
		return append(lines, "",
			m.accentStyle().Render(" The catalog is empty. Three steps:"), "",
			m.step(1, "Server > Pair this device", m.srv.state == "connected"),
			m.step(2, "p : choose the platforms to index", len(m.data.Platforms) > 0),
			m.step(3, "s : sync the catalog (downloads the game list)", false), "",
			" Settings are read from:", "   "+m.cfgDir())
	}
	if len(m.view) == 0 {
		return append(lines, "", " No game matches \""+m.query+"\".")
	}

	showPlat := (kind == "all" || kind == "installed") && iw >= 70
	colsW := 9
	if showPlat {
		colsW += 15
	}
	nameW := iw - 6 - colsW
	if nameW < 8 {
		nameW = 8
	}
	for i := m.roff; i < len(m.view) && len(lines) < rows; i++ {
		g := m.data.Games[m.view[i]]
		box, star := "[ ]", " "
		if m.sel[g.Key()] {
			box = "[X]"
		}
		if g.Installed {
			star = "*"
		}
		plain := " " + box + star + " " + padRight(trunc(g.Name, nameW), nameW) + padLeft(humanSize(g.Size), 9)
		if showPlat {
			plain += "  " + padRight(trunc(g.Platform, 13), 13)
		}
		switch {
		case i == m.rcur && m.focus == fRight:
			lines = append(lines, m.row(plain, iw, m.cursorStyle()))
		case i == m.rcur:
			lines = append(lines, m.row(plain, iw, m.accentStyle()))
		case m.sel[g.Key()]:
			lines = append(lines, m.row(plain, iw, m.selStyle()))
		default:
			lines = append(lines, m.row(plain, iw, m.sNorm))
		}
	}
	return lines
}

func (m *model) serverLines() []string {
	c := m.data.Config
	get := func(k, d string) string {
		if v := c[k]; v != "" {
			return v
		}
		return d
	}
	return []string{
		"",
		" Server:       " + get("server_url", "(not set)"),
		" Status:       " + m.pairStatus(),
		" Device id:    " + get("device_id", "(not paired)"),
		" Permissions:  " + get("scopes", "-"),
		" Verify TLS:   " + map[bool]string{true: "no", false: "yes"}[c["verify_tls"] == "0"],
		"",
		" Settings are saved in:",
		"   " + m.cfgDir(),
		"",
		m.accStyle(kSettings).Render(" Press Enter to edit the server settings."),
	}
}

func (m *model) settingsLines() []string {
	c := m.data.Config
	games := c["games_dir"]
	if games == "" {
		games = filepath.Join(m.data.Root, "games")
	}
	auto := "on"
	if c["autosync"] == "0" {
		auto = "off"
	}
	border := "box-drawing characters"
	if m.th.Border[0] == "+" {
		border = "ASCII (--ascii)"
	}
	return []string{
		"",
		" Games folder:    " + games,
		" Auto-sync:       " + auto + " (config.ini)",
		" Colors:          " + m.th.Name,
		" Borders:         " + border,
		" Saved in:        " + m.cfgDir(),
		" Terminal:        " + fmt.Sprintf("%dx%d", m.w, m.h),
		"",
		m.accStyle(kSettings).Render(" Press Enter to edit the settings."),
	}
}

func (m *model) downloadLines(iw int) []string {
	if len(m.queue) == 0 {
		return []string{"", " No download yet.", "", " Tick games in the catalog and press d."}
	}
	var lines []string
	free := ""
	if f, ok := freeSpace(m.gamesDir()); ok {
		free = "   free space " + humanSize(f)
	}
	lines = append(lines, "", m.dimStyle().Render(" Games folder: "+trunc(m.gamesDir(), iw-20)+free), "")
	start := 0
	if len(m.queue) > 6 {
		start = len(m.queue) - 6 // show the most recent entries
	}
	for _, d := range m.queue[start:] {
		lines = append(lines, " "+trunc(d.g.Name, iw-4))
		lines = append(lines, " "+m.bar(d, iw-4))
	}
	return lines
}

func (m *model) bar(d *dl, w int) string {
	const label = 30
	bw := w - label
	if bw < 8 {
		bw = 8
	}
	filled, pct := 0, 0
	if d.total > 0 {
		filled = int(d.done * int64(bw) / d.total)
		pct = int(d.done * 100 / d.total)
	}
	on := m.accStyle(kMessage).Render(strings.Repeat("#", filled))
	off := m.dimStyle().Render(strings.Repeat("-", bw-filled))
	var txt string
	switch d.state {
	case "queued":
		txt = " waiting"
	case "running":
		txt = fmt.Sprintf(" %3d%% %s/%s", pct, humanSize(d.done), humanSize(d.total))
		if d.speed > 0 {
			txt += " " + humanSize(d.speed) + "/s"
		}
	case "verifying":
		txt = " verifying the hash..."
	case "done":
		txt = " done"
	case "cancelled":
		txt = " cancelled"
	case "failed":
		return "[" + on + off + "] " + m.sBad.Render(" failed ") + " " + trunc(d.err, w-bw-12)
	}
	return "[" + on + off + "]" + txt
}

// ---------------------------------------------------------------------- popups

// popupKind maps a window to its color family.
func popupKind(p *popup) string {
	switch p.kind {
	case "confirm":
		return kQuestion
	case "menu":
		return kGames
	case "settings", "input", "pairing", "checklist":
		return kSettings
	}
	return kMessage // info, help, downloads
}

func (m *model) renderPopup() string {
	p := m.pop
	kind := popupKind(p)
	maxW := m.w - 6
	var lines []string
	pb := m.st().Background(m.th.PopupBG).Foreground(m.th.Normal)
	cur := m.curStyle(kind)

	switch p.kind {
	case "confirm":
		for _, l := range p.lines {
			lines = append(lines, " "+l)
		}
		lines = append(lines, "")
		var btn []string
		for i, it := range p.items {
			s := "[ " + it + " ]"
			if i == p.cur {
				btn = append(btn, cur.Render(s))
			} else {
				btn = append(btn, pb.Render(s))
			}
		}
		lines = append(lines, " "+strings.Join(btn, "   "))
	case "menu":
		for i, it := range p.items {
			if i == p.cur {
				lines = append(lines, cur.Render(" "+padRight(it, 36)))
			} else {
				lines = append(lines, pb.Render(" "+padRight(it, 36)))
			}
		}
	case "settings":
		labels, _ := p.dyn(m)
		for i, it := range labels {
			if i == p.cur {
				lines = append(lines, cur.Render(" "+padRight(it, 56)))
			} else {
				lines = append(lines, pb.Render(" "+padRight(it, 56)))
			}
		}
		lines = append(lines, "", m.dimStyle().Render(" Enter: change   Esc: close"))
	case "pairing":
		for _, l := range m.pairingLines() {
			lines = append(lines, l)
		}
	case "tool":
		rows := m.h - 8
		src := p.lines
		if len(src) > rows {
			src = src[len(src)-rows:] // follow the end while the report is being written
		}
		for _, l := range src {
			lines = append(lines, " "+l)
		}
		if p.running {
			lines = append(lines, "", m.dimStyle().Render(" Working...   Esc: stop"))
		} else {
			lines = append(lines, "", m.dimStyle().Render(" Done.   Enter or Esc: close"))
		}
	case "checklist":
		rows := m.h - 10
		if rows < 3 {
			rows = 3
		}
		start := 0
		if p.cur >= rows {
			start = p.cur - rows + 1
		}
		ticked := 0
		for _, it := range p.chk {
			if it.on {
				ticked++
			}
		}
		for i := start; i < len(p.chk) && i < start+rows; i++ {
			box := "[ ] "
			if p.chk[i].on {
				box = "[X] "
			}
			row := " " + box + p.chk[i].label
			if i == p.cur {
				lines = append(lines, cur.Render(padRight(trunc(row, 70), 70)))
			} else {
				lines = append(lines, pb.Render(padRight(trunc(row, 70), 70)))
			}
		}
		lines = append(lines, "", m.dimStyle().Render(fmt.Sprintf(" %d ticked   Space: tick   a/x: all/none   Enter: apply and sync   Esc: cancel", ticked)))
	case "input":
		for _, l := range p.lines {
			lines = append(lines, " "+l)
		}
		lines = append(lines, "", m.accStyle(kind).Render(" > "+p.text+"_"))
	case "downloads":
		lines = append(lines, m.downloadLines(60)...)
		lines = append(lines, "", m.dimStyle().Render(" Esc: hide (downloads continue)   c: cancel all"))
	default: // info, help
		for _, l := range p.lines {
			lines = append(lines, " "+l)
		}
		lines = append(lines, "", m.dimStyle().Render(" Press any key to close"))
	}

	pw := width(" " + p.title + " ")
	for _, l := range lines {
		if lw := width(l); lw > pw {
			pw = lw
		}
	}
	pw += 4
	if pw < 40 {
		pw = 40
	}
	if pw > maxW {
		pw = maxW
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], pw-2, "")
	}
	ph := len(lines) + 2
	if ph > m.h-2 {
		ph = m.h - 2
		lines = lines[:ph-2]
	}
	return m.box(p.title, lines, pw, ph, true, kind)
}

func (m *model) pairStatus() string {
	if m.cfg("device_id", "") != "" || m.cfg("scopes", "") != "" {
		return "paired (use \"Test connection\" to check)"
	}
	return "not paired: choose \"Pair this device\""
}

func (m *model) actionLines(title string, text ...string) []string {
	lines := []string{"", " " + m.accStyle(kSettings).Render(title), ""}
	for _, t := range text {
		lines = append(lines, " "+t)
	}
	if last := m.cfg("catalog_synced", ""); last != "" && title == "Sync catalog" {
		if sec, err := strconv.ParseInt(last, 10, 64); err == nil {
			lines = append(lines, "", " Last sync: "+time.Unix(sec, 0).Format("2006-01-02 15:04"))
		}
	}
	return lines
}

// step renders one line of the first-run checklist.
func (m *model) step(n int, text string, done bool) string {
	mark := "[ ]"
	if done {
		mark = "[X]"
	}
	return fmt.Sprintf(" %s %d. %s", mark, n, text)
}
