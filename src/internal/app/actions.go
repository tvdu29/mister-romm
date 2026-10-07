package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// ---------------------------------------------------------------- data

// reloadCatalog re-reads the catalog and the list of installed games from disk.
func (m *model) reloadCatalog() {
	d := loadData(m.data.Root)
	m.data.Games, m.data.Platforms = d.Games, d.Platforms
	m.buildLeft()
	m.refilter()
}

func (m *model) platformByID(id string) *Platform {
	for _, p := range m.data.Platforms {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// ---------------------------------------------------------------- sync

func (m *model) startSync(full, quiet bool) tea.Cmd {
	return m.startSyncFor(readSelection(m.cfgDir()), full, quiet)
}

// startSyncFor syncs the given platforms (the caller may hold a selection that could not be saved).
func (m *model) startSyncFor(sel map[string]*selPlatform, full, quiet bool) tea.Cmd {
	if m.syncJob != nil {
		m.toast = "A sync is already running"
		return nil
	}
	if len(sel) == 0 {
		if !quiet {
			m.message("Sync catalog", []string{"No platform is indexed yet.", "", "Press p (or choose \"Platforms to index\") and tick the platforms you want."})
		}
		return nil
	}
	if m.cfg("server_url", "") == "" || m.data.Token == "" {
		if !quiet {
			m.message("Sync catalog", []string{"This device is not paired with a server yet.", "", "Open Server and choose \"Pair this device\"."})
		}
		return nil
	}
	e := m.env()
	m.syncText = "Indexing..."
	j, cmd := startJob(func(ctx context.Context, send func(tea.Msg)) { runSync(ctx, e, sel, full, send) })
	m.syncJob = j
	return cmd
}

func (m *model) finishSync(d syncDone) tea.Cmd {
	m.syncText = ""
	if d.err != "" {
		if d.cancelled {
			m.toast = "Sync cancelled"
			return nil
		}
		logError(m.cfgDir(), "sync: "+d.err)
		m.message("Sync failed", []string{d.err})
		return nil
	}
	sel := readSelection(m.cfgDir())
	for pid, at := range d.synced {
		if p, ok := sel[pid]; ok {
			p.SyncedAt = at
		}
	}
	m.noteErr("selection.json", writeSelection(m.cfgDir(), sel))
	m.set("catalog_synced", fmt.Sprintf("%d", time.Now().Unix()))
	m.reloadCatalog()
	m.toast = fmt.Sprintf("Catalog up to date: %d games (%+d)", d.total, d.added)
	if d.unsafe > 0 {
		logError(m.cfgDir(), fmt.Sprintf("sync: %d game(s) refused because of an unsafe file name", d.unsafe))
		m.toast += fmt.Sprintf(" - %d refused: unsafe file name sent by the server", d.unsafe)
	}
	return nil
}

// ---------------------------------------------------------------- platforms

type chkItem struct {
	id, slug, name, label string
	on                    bool
}

type platformsMsg struct {
	list []srvPlatform
	err  string
}

func (m *model) openPlatforms() tea.Cmd {
	if m.cfg("server_url", "") == "" || m.data.Token == "" {
		m.message("Platforms", []string{"This device is not paired with a server yet.", "", "Open Server and choose \"Pair this device\"."})
		return nil
	}
	m.pop = &popup{kind: "info", title: "Platforms", lines: []string{"Loading the platform list ..."}}
	e := m.env()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		list, err := fetchPlatforms(ctx, e)
		if err != nil {
			return platformsMsg{err: explainErr(err, clockOK())}
		}
		return platformsMsg{list: list}
	}
}

func (m *model) showPlatforms(msg platformsMsg) {
	if msg.err != "" {
		logError(m.cfgDir(), "platforms: "+msg.err)
		m.message("Platforms", []string{msg.err})
		return
	}
	e := m.env()
	sel := readSelection(m.cfgDir())
	var items []chkItem
	for _, p := range msg.list {
		id := fmt.Sprintf("%d", p.ID)
		folder := resolveFolder(e, p.Slug, p.title())
		if folder == "" {
			folder = "?"
		}
		_, on := sel[id]
		items = append(items, chkItem{id: id, slug: p.Slug, name: p.title(),
			label: fmt.Sprintf("%s (%d games) -> %s", p.title(), p.RomCount, folder), on: on})
	}
	m.pop = &popup{kind: "checklist", title: "Platforms to index", chk: items}
}

func (m *model) applyPlatforms(items []chkItem) tea.Cmd {
	old := readSelection(m.cfgDir())
	next := map[string]*selPlatform{}
	added := 0
	for _, it := range items {
		if !it.on {
			continue
		}
		if p, ok := old[it.id]; ok {
			next[it.id] = p
		} else {
			next[it.id] = &selPlatform{Slug: it.slug, Name: it.name}
			added++
		}
	}
	for id := range old {
		if _, keep := next[id]; !keep {
			_ = os.Remove(catalogPath(m.cfgDir(), id)) // the games already on the card stay
		}
	}
	m.noteErr("selection.json", writeSelection(m.cfgDir(), next))
	m.reloadCatalog()
	if len(next) == 0 {
		m.toast = "No platform indexed"
		return nil
	}
	// Enter applies the choice and starts the sync right away (incremental: only what changed, and the
	// full list for platforms that were just added).
	if added > 0 {
		m.toast = fmt.Sprintf("%d platform(s) added: syncing...", added)
	}
	return m.startSyncFor(next, false, false)
}

// ---------------------------------------------------------------- downloads

// dl is one entry of the download queue.
type dl struct {
	g           *Game
	item        dlItem
	done, total int64
	speed       int64
	state       string // queued | running | verifying | done | failed | cancelled
	err         string
}

type dlPlan struct {
	todo      []*Game
	items     []dlItem
	bytes     int64
	present   int
	conflicts int
	unsafe    int
	unmapped  []string
}

func (m *model) planDownload(games []*Game) dlPlan {
	var p dlPlan
	e := m.env()
	seen := map[string]bool{}
	for _, g := range games {
		if g.Installed {
			continue
		}
		pl := m.platformByID(g.PID)
		slug, name := "", g.Platform
		if pl != nil {
			slug, name = pl.Slug, pl.Name
		}
		folder := resolveFolder(e, slug, name)
		if folder == "" {
			if !seen[name] {
				seen[name] = true
				p.unmapped = append(p.unmapped, name)
			}
			continue
		}
		if !safeName(g.Name) || !safeName(folder) {
			p.unsafe++ // a file name from the server must never choose where it is written
			continue
		}
		dest := filepath.Join(e.gamesDir, folder, g.Name)
		if !within(e.gamesDir, dest) {
			p.unsafe++
			continue
		}
		if st, err := os.Stat(dest); err == nil {
			if st.Size() == g.Size {
				p.present++
			} else {
				p.conflicts++
			}
			continue
		}
		p.todo = append(p.todo, g)
		p.bytes += g.Size
		p.items = append(p.items, dlItem{id: g.ID, pid: g.PID, name: g.Name, dest: dest, sha1: g.SHA1, size: g.Size})
	}
	return p
}

func (m *model) askDownload() tea.Cmd {
	games := m.selectedGames()
	if len(games) == 0 {
		if g := m.currentGame(); g != nil {
			games = []*Game{g}
		}
	}
	p := m.planDownload(games)
	if len(p.items) == 0 {
		lines := []string{"Nothing to download."}
		if p.present > 0 {
			lines = append(lines, fmt.Sprintf("%d already on the card (not managed by this program).", p.present))
		}
		if p.conflicts > 0 {
			lines = append(lines, fmt.Sprintf("%d with the same name but a different size (skipped).", p.conflicts))
		}
		if len(p.unmapped) > 0 {
			lines = append(lines, "", "No folder known for: "+strings.Join(p.unmapped, ", "), "Set it in Settings > Platform folders.")
		}
		if p.unsafe > 0 {
			lines = append(lines, "", fmt.Sprintf("%d game(s) refused: unsafe file name sent by the server.", p.unsafe))
		}
		m.message("Download", lines)
		return nil
	}
	if free, ok := freeSpace(m.gamesDir()); ok && p.bytes > free {
		m.message("Not enough space", []string{fmt.Sprintf("Needed: %s, available: %s.", humanSize(p.bytes), humanSize(free))})
		return nil
	}
	lines := []string{fmt.Sprintf("Download %d game(s), %s?", len(p.items), humanSize(p.bytes)), ""}
	if free, ok := freeSpace(m.gamesDir()); ok { // p.bytes <= free: checked just above
		warn := ""
		if free-p.bytes < lowSpace {
			warn = "   (low space!)"
		}
		lines = append(lines, fmt.Sprintf("Free space: %s  ->  %s left after%s", humanSize(free), humanSize(free-p.bytes), warn), "")
	}
	for i, it := range p.items {
		if i == 8 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(p.items)-8))
			break
		}
		lines = append(lines, "  - "+trunc(it.name, 48))
	}
	if p.present > 0 || p.conflicts > 0 || len(p.unmapped) > 0 {
		lines = append(lines, "")
	}
	if p.present > 0 {
		lines = append(lines, fmt.Sprintf("(%d already on the card, skipped)", p.present))
	}
	if p.conflicts > 0 {
		lines = append(lines, fmt.Sprintf("(%d with another file of the same name, skipped)", p.conflicts))
	}
	if len(p.unmapped) > 0 {
		lines = append(lines, "(no folder known for: "+strings.Join(p.unmapped, ", ")+")")
	}
	m.confirm("Download", lines, func(m *model) tea.Cmd { return m.queueDownloads(p) })
	return nil
}

// lowSpace is what must remain on the card after a download for it not to be flagged.
const lowSpace = 500 << 20

// startDownload downloads one game straight away (context menu).
func (m *model) startDownload(games []*Game) tea.Cmd {
	p := m.planDownload(games)
	if len(p.items) == 0 {
		m.toast = "Nothing to download (already on the card, or no folder known for its platform)"
		return nil
	}
	return m.queueDownloads(p)
}

func (m *model) queueDownloads(p dlPlan) tea.Cmd {
	for i, it := range p.items {
		m.queue = append(m.queue, &dl{g: p.todo[i], item: it, total: it.size, state: "queued"})
	}
	m.sel = map[string]bool{}
	m.buildLeft()
	m.openDownloads()
	return m.kickDownloads()
}

// kickDownloads starts a download job for everything that is queued, unless one is running.
func (m *model) kickDownloads() tea.Cmd {
	if m.dlJob != nil {
		return nil
	}
	var batch []*dl
	var items []dlItem
	for _, d := range m.queue {
		if d.state == "queued" {
			batch = append(batch, d)
			items = append(items, d.item)
		}
	}
	if len(batch) == 0 {
		return nil
	}
	m.dlBatch = batch
	e := m.env()
	j, cmd := startJob(func(ctx context.Context, send func(tea.Msg)) { runDownloads(ctx, e, items, send) })
	m.dlJob = j
	return cmd
}

func (m *model) cancelDownloads() {
	if m.dlJob != nil {
		m.dlJob.cancel()
	}
	for _, d := range m.queue {
		if d.state == "queued" {
			d.state = "cancelled"
		}
	}
	m.buildLeft()
}

func (m *model) onDownloadMsg(msg tea.Msg) {
	switch x := msg.(type) {
	case dlStart:
		if x.idx < len(m.dlBatch) {
			m.dlBatch[x.idx].state = "running"
		}
	case dlProgress:
		if x.idx < len(m.dlBatch) {
			d := m.dlBatch[x.idx]
			d.done, d.total, d.speed = x.done, x.total, x.speed
			if x.phase == "verifying" {
				d.state = "verifying"
			}
		}
	case dlDoneMsg:
		if x.idx >= len(m.dlBatch) {
			return
		}
		d := m.dlBatch[x.idx]
		switch {
		case x.cancelled:
			d.state = "cancelled"
		case x.err != "":
			d.state, d.err = "failed", x.err
		default:
			d.state, d.done = "done", d.total
			roms := readState(m.cfgDir())
			roms[d.item.id] = &stateRom{Path: d.item.dest, Size: d.item.size, Reasons: []string{"rom"}}
			m.noteErr("state.json", writeState(m.cfgDir(), roms))
			d.g.Installed, d.g.Path = true, d.item.dest
			m.buildLeft()
			m.refilter()
		}
	}
}

// downloadSummary is shown when a batch ends.
func (m *model) downloadSummary() {
	ok, failed, cancelled := 0, 0, 0
	var first string
	for _, d := range m.dlBatch {
		switch d.state {
		case "done":
			ok++
		case "failed":
			failed++
			if first == "" {
				first = d.item.name + ": " + d.err
			}
		case "cancelled", "queued", "running":
			cancelled++
		}
	}
	m.dlBatch = nil
	switch {
	case failed > 0:
		m.message("Downloads finished with errors", []string{fmt.Sprintf("Downloaded: %d   Failed: %d   Cancelled: %d", ok, failed, cancelled), "", "First error:", first})
	case cancelled > 0:
		m.toast = fmt.Sprintf("Downloads cancelled (%d done). Partial files resume next time.", ok)
	default:
		m.toast = fmt.Sprintf("Downloaded %d game(s)", ok)
	}
}

// ---------------------------------------------------------------- delete

// deleteGame removes a game this program downloaded. Files that are not tracked are never touched.
func (m *model) deleteGame(g *Game) {
	roms := readState(m.cfgDir())
	r, ok := roms[g.ID]
	if !ok || r.Path == "" {
		m.message("Delete", []string{"This game was not downloaded by this program, so it is left alone."})
		return
	}
	if err := removeGameFile(m.gamesDir(), r.Path); err != nil {
		m.message("Delete failed", []string{err.Error()})
		return
	}
	delete(roms, g.ID)
	m.noteErr("state.json", writeState(m.cfgDir(), roms))
	g.Installed, g.Path = false, ""
	m.buildLeft()
	m.refilter()
	m.toast = "Deleted " + trunc(g.Name, 50)
}
