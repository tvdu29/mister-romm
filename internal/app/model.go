package app

import (
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type focus int

const (
	fLeft focus = iota
	fRight
)

type leftItem struct{ kind, id, name, count string }

// popup is a window drawn over the interface.
type popup struct {
	kind  string // confirm | menu | info | help | downloads
	title string
	lines []string
	items []string
	cur   int
	act   func(m *model, label string) tea.Cmd

	text string                                // input: the text being edited
	done func(m *model, text string) tea.Cmd   // input: called with the text on Enter
	dyn  func(m *model) (labels, ids []string) // settings: items rebuilt on every draw

	chk     []chkItem // checklist: items with a tick
	running bool      // tool: still working
}

type model struct {
	data *Data
	th   Theme

	// styles are built once: creating them per row is needless work on a 800 MHz Cortex-A9
	sSel, sDim, sNorm, sBad lipgloss.Style
	curSt, accSt            map[string]lipgloss.Style // per window kind: cursor line and accent

	leftKey, leftStr string // cache of the left panel (it rarely changes)
	w, h             int
	focus            focus

	left []leftItem
	lcur int

	view      []int // indexes into data.Games for the current scope + search
	rcur      int
	roff      int
	query     string
	searching bool
	sel       map[string]bool

	pop   *popup
	queue []*dl
	toast string

	sessionInsecure bool // certificate checks are off for this session only (clock not set)
	syncJob, dlJob  *job
	toolJob         *job
	clockJob        *job
	syncText        string
	syncQuiet       bool
	dlBatch         []*dl
	autosyncDone    bool
	saveErr         string // last failure to write a file: shown in the status bar until it works again

	srv     srvStatus  // state of the connection to the RomM server
	pair    *pairState // pairing in progress (device flow)
	pairGen int        // incremented to invalidate stale pairing messages
}

func newModel(d *Data, th Theme) *model {
	m := &model{data: d, th: th, sel: map[string]bool{}, w: 100, h: 30}
	m.applyStyles()
	m.buildLeft()
	m.refilter()
	m.firstRun()
	return m
}

// Init: with a wrong clock (1970 right after boot) the clock is set first, because every HTTPS
// certificate would look "not yet valid"; otherwise the server is checked straight away.
func (m *model) Init() tea.Cmd {
	if !clockOK() {
		j, cmd := startJob(runClock)
		m.clockJob = j
		m.srv = srvStatus{state: "clock"}
		return cmd
	}
	return m.checkServer()
}

// firstRun guides a new installation: no server yet -> the Server window; not paired -> a hint.
func (m *model) firstRun() {
	if err := checkStorage(m.cfgDir()); err != nil {
		m.saveErr = err.Error()
		m.message("Cannot save anything here", []string{
			"Nothing is stored between two runs, so you would have to",
			"pair the device again every time.", "",
			"Folder: " + m.cfgDir(), "Problem: " + err.Error(), "",
			"Start the program with --root DIR to choose a folder",
			"you can write to (default: /media/fat on a MiSTer)."})
		return
	}
	switch {
	case m.cfg("server_url", "") == "":
		m.openServer()
	case m.cfg("device_id", "") == "" && m.cfg("scopes", "") == "":
		m.toast = "Not paired yet: open Server and choose \"Pair this device\""
	}
}

func (m *model) applyStyles() {
	th := m.th
	m.sSel = lipgloss.NewStyle().Foreground(th.SelFG).Background(th.SelBG)
	m.sDim = lipgloss.NewStyle().Foreground(th.Dim)
	m.sNorm = lipgloss.NewStyle().Foreground(th.Normal)
	m.sBad = lipgloss.NewStyle().Foreground(th.BadFG).Background(th.BadBG).Bold(true)
	m.curSt, m.accSt = map[string]lipgloss.Style{}, map[string]lipgloss.Style{}
	for k, p := range th.Pal {
		m.curSt[k] = lipgloss.NewStyle().Foreground(p.CurFG).Background(p.CurBG).Bold(true)
		m.accSt[k] = lipgloss.NewStyle().Foreground(p.Accent).Bold(true)
	}
	m.leftKey = "" // force a redraw of the cached left panel
}

// applyTheme rebuilds the theme from the saved settings (borders, colors).
func (m *model) applyTheme() {
	m.th = newTheme(m.cfg("tui_ascii", "") == "1", m.cfg("tui_colors", "auto"))
	m.applyStyles()
}

func (m *model) curStyle(kind string) lipgloss.Style { return m.curSt[kind] }
func (m *model) accStyle(kind string) lipgloss.Style { return m.accSt[kind] }

// ---------------------------------------------------------------- state helpers

func (m *model) buildLeft() {
	installed := 0
	for _, g := range m.data.Games {
		if g.Installed {
			installed++
		}
	}
	n := func(i int) string { return fmt.Sprintf("%d", i) }
	m.left = []leftItem{{"all", "", "All games", n(len(m.data.Games))}}
	for _, p := range m.data.Platforms {
		m.left = append(m.left, leftItem{"plat", p.ID, p.Name, n(p.Count)})
	}
	m.left = append(m.left,
		leftItem{"sep", "", "", ""},
		leftItem{"installed", "", "Installed", n(installed)},
		leftItem{"downloads", "", "Downloads", n(m.activeDownloads())},
		leftItem{"sync", "", "Sync catalog", ""},
		leftItem{"platforms", "", "Platforms to index", ""},
		leftItem{"server", "", "Server", ""},
		leftItem{"settings", "", "Settings", ""},
	)
}

func (m *model) cur() leftItem { return m.left[m.lcur] }

func (m *model) isListView() bool {
	switch m.cur().kind {
	case "all", "plat", "installed":
		return true
	}
	return false
}

func (m *model) activeDownloads() int {
	n := 0
	for _, d := range m.queue {
		if d.state == "queued" || d.state == "running" {
			n++
		}
	}
	return n
}

func (m *model) refilter() {
	words := strings.Fields(strings.ToLower(m.query))
	m.view = m.view[:0]
	it := m.cur()
	for i, g := range m.data.Games {
		switch it.kind {
		case "plat":
			if g.PID != it.id {
				continue
			}
		case "installed":
			if !g.Installed {
				continue
			}
		case "all":
		default:
			continue
		}
		ok := true
		for _, w := range words {
			if !strings.Contains(g.Low, w) {
				ok = false
				break
			}
		}
		if ok {
			m.view = append(m.view, i)
		}
	}
	m.clampRight()
}

func (m *model) listRows() int {
	r := m.bodyH() - 2 - 1 // borders and the search line
	if r < 1 {
		r = 1
	}
	return r
}

func (m *model) bodyH() int { return m.h - 2 }

func (m *model) clampRight() {
	if m.rcur >= len(m.view) {
		m.rcur = len(m.view) - 1
	}
	if m.rcur < 0 {
		m.rcur = 0
	}
	rows := m.listRows()
	if m.rcur < m.roff {
		m.roff = m.rcur
	}
	if m.rcur >= m.roff+rows {
		m.roff = m.rcur - rows + 1
	}
	if m.roff < 0 {
		m.roff = 0
	}
}

func (m *model) currentGame() *Game {
	if len(m.view) == 0 {
		return nil
	}
	return m.data.Games[m.view[m.rcur]]
}

func (m *model) selectedGames() []*Game {
	var out []*Game
	for _, g := range m.data.Games {
		if m.sel[g.Key()] {
			out = append(out, g)
		}
	}
	return out
}

func (m *model) selectLeft(i int) {
	if i < 0 || i >= len(m.left) {
		return
	}
	for m.left[i].kind == "sep" { // separators are not selectable
		if i > m.lcur {
			i++
		} else {
			i--
		}
		if i < 0 || i >= len(m.left) {
			return
		}
	}
	if i != m.lcur {
		m.lcur = i
		m.query, m.searching, m.rcur, m.roff = "", false, 0, 0
		m.refilter()
	}
}

// ---------------------------------------------------------------------- update

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.clampRight()
	case srvStatus:
		m.srv = msg
		if msg.state == "connected" && len(m.data.Platforms) == 0 && m.pop == nil {
			m.toast = "Connected. Press p to choose the platforms to index, then s to sync."
		}
		if msg.state == "connected" && !m.autosyncDone {
			m.autosyncDone = true
			if m.cfg("autosync", "1") != "0" {
				return m, m.startSync(false, true)
			}
		}
	case platformsMsg:
		m.showPlatforms(msg)
	case fromJob:
		cmd := m.onJob(msg)
		if _, closed := msg.msg.(jobClosed); closed {
			return m, cmd
		}
		return m, tea.Batch(cmd, msg.j.wait())
	case connResultMsg:
		m.pop = &popup{kind: "info", title: msg.title, lines: msg.lines}
		return m, m.checkServer()
	case pairInitMsg, pairTickMsg, pairPollMsg, exchangeMsg:
		cmd, _ := m.handlePair(msg)
		return m, cmd
	case tea.KeyPressMsg:
		m.toast = ""
		if m.pop != nil {
			return m, m.popupKey(msg)
		}
		return m, m.key(msg)
	}
	return m, nil
}

func isSpace(msg tea.KeyPressMsg) bool { return msg.String() == "space" || msg.String() == " " }

func (m *model) key(msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if k == "ctrl+c" {
		return tea.Quit
	}
	if m.searching {
		switch k {
		case "esc":
			m.query, m.searching = "", false
			m.refilter()
		case "enter", "down":
			m.searching = false
		case "backspace":
			if r := []rune(m.query); len(r) > 0 {
				m.query = string(r[:len(r)-1])
				m.rcur, m.roff = 0, 0
				m.refilter()
			}
		default:
			if t := msg.Key().Text; t != "" {
				m.query += t
				m.rcur, m.roff = 0, 0
				m.refilter()
			}
		}
		return nil
	}

	switch k {
	case "q":
		lines := []string{"Quit mister-romm?"}
		if m.dlJob != nil || m.syncJob != nil {
			lines = append(lines, "", "Something is still running. Partial downloads resume next time.")
		}
		m.confirm("Quit", lines, func(m *model) tea.Cmd { return tea.Quit })
		return nil
	case "s":
		return m.startSync(false, false)
	case "p":
		return m.openPlatforms()
	case "c":
		if m.syncJob != nil {
			m.syncJob.cancel()
			m.toast = "Cancelling the sync..."
		} else if m.dlJob != nil {
			m.cancelDownloads()
			m.toast = "Cancelling the downloads..."
		}
		return nil
	case "?":
		m.openHelp()
		return nil
	case "tab", "shift+tab":
		if m.focus == fLeft {
			m.focus = fRight
		} else {
			m.focus = fLeft
		}
		return nil
	}

	if m.focus == fLeft {
		switch k {
		case "up", "k":
			m.selectLeft(m.lcur - 1)
		case "down", "j":
			m.selectLeft(m.lcur + 1)
		case "enter", "right", "l":
			switch m.cur().kind {
			case "server":
				m.openServer()
			case "settings":
				m.openSettings()
			case "sync":
				return m.startSync(false, false)
			case "platforms":
				return m.openPlatforms()
			case "downloads":
				m.focus = fRight
				m.openDownloads()
			default:
				m.focus = fRight
			}
		}
		return nil
	}

	// right panel
	switch k {
	case "left", "h", "esc":
		m.focus = fLeft
		return nil
	}
	if !m.isListView() {
		if k == "enter" {
			switch m.cur().kind {
			case "downloads":
				m.openDownloads()
			case "server":
				m.openServer()
			case "settings":
				m.openSettings()
			case "sync":
				return m.startSync(false, false)
			case "platforms":
				return m.openPlatforms()
			}
		}
		return nil
	}
	rows := m.listRows()
	switch k {
	case "up", "k":
		m.rcur--
	case "down", "j":
		m.rcur++
	case "pgup":
		m.rcur -= rows
	case "pgdown":
		m.rcur += rows
	case "home", "g":
		m.rcur = 0
	case "end", "G":
		m.rcur = len(m.view) - 1
	case "/":
		m.searching = true
	case "a":
		for _, i := range m.view {
			m.sel[m.data.Games[i].Key()] = true
		}
	case "x":
		m.sel = map[string]bool{}
	case "enter":
		return m.contextMenu()
	case "d":
		return m.askDownload()
	default:
		if isSpace(msg) {
			if g := m.currentGame(); g != nil {
				m.toggle(g)
				m.rcur++
			}
		}
	}
	m.clampRight()
	return nil
}

func (m *model) toggle(g *Game) {
	if m.sel[g.Key()] {
		delete(m.sel, g.Key())
	} else {
		m.sel[g.Key()] = true
	}
}

// ---------------------------------------------------------------------- popups

func (m *model) confirm(title string, lines []string, yes func(m *model) tea.Cmd) {
	m.pop = &popup{kind: "confirm", title: title, lines: lines, items: []string{"Yes", "No"}, cur: 0,
		act: func(m *model, label string) tea.Cmd {
			if label == "Yes" {
				return yes(m)
			}
			return nil
		}}
}

func (m *model) popupKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.pop
	k := msg.String()
	if k == "ctrl+c" {
		return tea.Quit
	}
	switch p.kind {
	case "info", "help":
		m.pop = nil
		return nil
	case "pairing":
		if k == "esc" {
			m.cancelPairing()
		}
		return nil
	case "tool":
		if k == "esc" || (!p.running && (k == "enter" || k == "q")) {
			if m.toolJob != nil {
				m.toolJob.cancel()
			}
			m.pop = nil
		}
		return nil
	case "checklist":
		switch k {
		case "esc", "q":
			m.pop = nil
		case "up", "k":
			if p.cur > 0 {
				p.cur--
			}
		case "down", "j":
			if p.cur < len(p.chk)-1 {
				p.cur++
			}
		case "pgup":
			p.cur = max(0, p.cur-10)
		case "pgdown":
			p.cur = min(len(p.chk)-1, p.cur+10)
		case "a":
			for i := range p.chk {
				p.chk[i].on = true
			}
		case "x":
			for i := range p.chk {
				p.chk[i].on = false
			}
		case "enter":
			items := p.chk
			m.pop = nil
			return m.applyPlatforms(items)
		default:
			if isSpace(msg) && len(p.chk) > 0 {
				p.chk[p.cur].on = !p.chk[p.cur].on
				if p.cur < len(p.chk)-1 {
					p.cur++
				}
			}
		}
		return nil
	case "input":
		switch k {
		case "esc":
			m.pop = nil
		case "enter":
			m.pop = nil
			return p.done(m, p.text)
		case "backspace":
			if r := []rune(p.text); len(r) > 0 {
				p.text = string(r[:len(r)-1])
			}
		case "ctrl+u":
			p.text = ""
		default:
			if t := msg.Key().Text; t != "" {
				p.text += t
			}
		}
		return nil
	case "settings":
		_, ids := p.dyn(m)
		switch k {
		case "esc", "q":
			m.pop = nil
		case "up", "k":
			if p.cur > 0 {
				p.cur--
			}
		case "down", "j":
			if p.cur < len(ids)-1 {
				p.cur++
			}
		case "enter":
			return p.act(m, ids[p.cur])
		}
		return nil
	case "downloads":
		switch k {
		case "esc", "enter", "q":
			m.pop = nil
		case "c":
			m.confirm("Cancel downloads", []string{"Cancel all queued and running downloads?"}, func(m *model) tea.Cmd {
				m.cancelDownloads()
				return nil
			})
		}
		return nil
	}
	horizontal := p.kind == "confirm"
	prev, next := "up", "down"
	if horizontal {
		prev, next = "left", "right"
	}
	switch k {
	case "esc", "q":
		m.pop = nil
	case prev, "shift+tab":
		if p.cur > 0 {
			p.cur--
		}
	case next, "tab":
		if p.cur < len(p.items)-1 {
			p.cur++
		}
	case "y":
		if horizontal {
			m.pop = nil
			return p.act(m, "Yes")
		}
	case "n":
		if horizontal {
			m.pop = nil
			return p.act(m, "No")
		}
	case "enter":
		m.pop = nil
		return p.act(m, p.items[p.cur])
	}
	return nil
}

func (m *model) contextMenu() tea.Cmd {
	g := m.currentGame()
	if g == nil {
		return nil
	}
	items := []string{}
	if g.Installed {
		items = append(items, "Delete from the card")
	} else {
		items = append(items, "Download")
	}
	if m.sel[g.Key()] {
		items = append(items, "Unselect")
	} else {
		items = append(items, "Select")
	}
	items = append(items, "Game info", "Close")
	m.pop = &popup{kind: "menu", title: trunc(g.Name, 44), items: items,
		act: func(m *model, label string) tea.Cmd {
			switch label {
			case "Download":
				return m.startDownload([]*Game{g})
			case "Delete from the card":
				m.confirm("Delete", []string{"Delete this game from the card?", "", trunc(g.Name, 52), "", "Only games downloaded by this program can be deleted."}, func(m *model) tea.Cmd {
					m.deleteGame(g)
					return nil
				})
			case "Select", "Unselect":
				m.toggle(g)
			case "Game info":
				m.openInfo(g)
			}
			return nil
		}}
	return nil
}

func (m *model) openInfo(g *Game) {
	inst := "no"
	if g.Installed {
		inst = "yes"
	}
	lines := []string{
		"Name:      " + g.Name,
		"Platform:  " + g.Platform,
		"Size:      " + humanSize(g.Size),
		"On card:   " + inst,
		"RomM id:   " + g.ID,
	}
	if g.SHA1 != "" {
		lines = append(lines, "SHA1:      "+g.SHA1)
	}
	if g.Path != "" {
		lines = append(lines, "Path:      "+g.Path)
	}
	m.pop = &popup{kind: "info", title: "Game info", lines: lines}
}

func (m *model) openHelp() {
	m.pop = &popup{kind: "help", title: "Keys", lines: []string{
		"Tab / <- ->     switch between the panels",
		"Up Down j k     move   (PgUp PgDn g G: jump)",
		"/               search (all words must match)",
		"Space           tick / untick the game, move down",
		"a / x           tick all shown / untick all",
		"Enter           context menu of the game",
		"d               download the ticked games",
		"s / p / c       sync the catalog / platforms to index / cancel",
		"q               quit      ?  this help",
		"",
		"Colors: pink = games, cyan = settings, green = information,",
		"        yellow/orange = questions",
		"",
		"* after [ ] means the game is already on the card",
	}}
}

func (m *model) openDownloads() {
	m.pop = &popup{kind: "downloads", title: "Downloads"}
}

// ------------------------------------------------------------------- downloads

// advance moves the simulated download forward. Real downloads will replace this.

// checkStorage verifies that the settings folder can be written.
func checkStorage(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// noteErr records a failure to write a file, so it shows in the status bar and in the next window.
func (m *model) noteErr(what string, err error) {
	if err != nil {
		m.saveErr = what + ": " + err.Error()
		logError(m.cfgDir(), "save "+what+": "+err.Error())
	}
}
