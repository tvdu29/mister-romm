package app

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/tvdu29/mister-romm/src/internal/savefmt"
)

// Saves: the battery saves of the card (saves/<Core>/<game>.sav) compared with the saves RomM
// keeps for the same games, which other clients (RetroArch, the web player...) upload in the raw
// emulator format. This part only reads: nothing is written to the card or to the server.

// srvSave is one save stored by RomM (GET /api/saves).
type srvSave struct {
	ID        int       `json:"id"`
	RomID     int       `json:"rom_id"`
	FileName  string    `json:"file_name"`
	Size      int64     `json:"file_size_bytes"`
	Emulator  string    `json:"emulator"`
	Slot      string    `json:"slot"`
	Hash      string    `json:"content_hash"`
	Device    string    `json:"origin_device_id"`
	UpdatedAt time.Time `json:"updated_at"`
	Missing   bool      `json:"missing_from_fs"`
}

// autosaveSlot is where every RomM client keeps the save it syncs (web player, Grout, Argosy,
// Playnite...): the server pairs saves by (rom, slot) and keeps a version per upload.
const autosaveSlot = "autosave"

// fetchSaves lists the saves of the user for one platform.
func fetchSaves(ctx context.Context, e env, pid string) ([]srvSave, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := e.get(ctx, e.client, "/api/saves?platform_id="+pid, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s", explainHTTP(resp))
	}
	var list []srvSave
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSON)).Decode(&list); err != nil {
		return nil, fmt.Errorf("the server answered with something that is not a save list: %v", err)
	}
	for i := range list {
		list[i].FileName, list[i].Emulator = clean(list[i].FileName), clean(list[i].Emulator)
		list[i].Slot, list[i].Device = clean(list[i].Slot), clean(list[i].Device)
	}
	return list, nil
}

// fetchSaveContent downloads one save (never more than savefmt.MaxSize) and checks its hash when
// the server gave one.
func fetchSaveContent(ctx context.Context, e env, s srvSave) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := e.get(ctx, e.client, "/api/saves/"+strconv.Itoa(s.ID)+"/content", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s", explainHTTP(resp))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, savefmt.MaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > savefmt.MaxSize {
		return nil, fmt.Errorf("the save is larger than %s", humanSize(savefmt.MaxSize))
	}
	if s.Hash != "" && !isZip(b) && !strings.EqualFold(md5hex(b), s.Hash) {
		return nil, fmt.Errorf("hash mismatch: the save was damaged on the way")
	}
	return b, nil
}

// fetchDevices maps device ids to their names, to show where a save comes from. Optional: an
// empty map when the token may not read devices.
func fetchDevices(ctx context.Context, e env) map[string]string {
	out := map[string]string{}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := e.get(ctx, e.client, "/api/devices", nil)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out
	}
	var list []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxJSON)).Decode(&list) == nil {
		for _, d := range list {
			out[d.ID] = clean(d.Name)
		}
	}
	return out
}

func md5hex(b []byte) string { h := md5.Sum(b); return hex.EncodeToString(h[:]) }

// isZip: some clients upload a folder of saves as a zip, which is not a battery save.
func isZip(b []byte) bool { return len(b) >= 4 && string(b[:4]) == "PK\x03\x04" }

// newestSave picks the save to compare with: the newest of the autosave slot, else the newest
// of any slot (saves uploaded by hand, older clients).
func newestSave(list []srvSave) *srvSave {
	var best *srvSave
	better := func(s *srvSave) bool {
		if best == nil {
			return true
		}
		if a, b := s.Slot == autosaveSlot, best.Slot == autosaveSlot; a != b {
			return a
		}
		return s.UpdatedAt.After(best.UpdatedAt)
	}
	for i := range list {
		if s := &list[i]; !s.Missing && better(s) {
			best = s
		}
	}
	return best
}

// ---------------------------------------------------------------- the card

// saveCores maps a RomM platform slug to the MiSTer core that writes its saves, when it is not
// the games folder (Game Boy Color games are run by the Gameboy core).
var saveCores = map[string]string{"gbc": "GAMEBOY"}

// saveCoreFor returns the MiSTer core of a platform, or "" when none is known.
func saveCoreFor(e env, slug, name string) string {
	if c := saveCores[slug]; c != "" {
		return c
	}
	return resolveFolder(e, slug, name)
}

// baseName is the name the MiSTer gives the save of a game: the file name without its extension.
// A RomM game made of several files (a folder) keeps its whole name.
func baseName(name string) string {
	if ext := filepath.Ext(name); ext != "" && len(ext) <= 5 && !strings.Contains(ext, " ") {
		return strings.TrimSuffix(name, ext)
	}
	return name
}

// localSave is a .sav file of the card.
type localSave struct {
	path    string
	size    int64
	modTime time.Time
}

// scanSaveDir lists the .sav files of one core folder, by lower-cased base name.
func scanSaveDir(dir string) map[string]localSave {
	out := map[string]localSave{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, en := range ents {
		n := en.Name()
		if en.IsDir() || !strings.EqualFold(filepath.Ext(n), ".sav") || strings.HasPrefix(n, ".") {
			continue
		}
		info, err := en.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out[strings.ToLower(strings.TrimSuffix(n, filepath.Ext(n)))] = localSave{path: filepath.Join(dir, n), size: info.Size(), modTime: info.ModTime()}
	}
	return out
}

// ---------------------------------------------------------------- comparison

// saveState is what a row of the Saves view says.
type saveState int

const (
	ssSame        saveState = iota // the card and RomM have the same game data
	ssDifferent                    // both have a save, with different data
	ssLocalOnly                    // only on the card
	ssCloudOnly                    // only in RomM
	ssUnsupported                  // the save format of this core is not handled
	ssError                        // could not be compared (see err)
)

// stateOrder sorts the view: what needs a decision first.
var stateOrder = map[saveState]int{ssDifferent: 0, ssCloudOnly: 1, ssLocalOnly: 2, ssError: 3, ssSame: 4, ssUnsupported: 5}

// saveRow is one game of the Saves view.
type saveRow struct {
	gameID, pid, name, platform, core string
	local                             *localSave
	cloud                             *srvSave
	cloudN                            int // number of versions RomM keeps for this game
	state                             saveState
	err                               string
}

// saveScanGame is what the scan needs to know about a game of the catalog.
type saveScanGame struct{ id, name string }

// saveScanPlatform is an indexed platform and its games.
type saveScanPlatform struct {
	id, slug, name string
	games          []saveScanGame
}

type saveScanProgress struct{ text string }
type saveScanDone struct {
	rows      []*saveRow
	devices   map[string]string
	orphans   int // .sav files of the card that match no game of the catalog
	err       string
	cancelled bool
}

// runSaveScan lists the saves of the card and of RomM for every indexed platform and compares them.
func runSaveScan(ctx context.Context, e env, savesDir string, plats []saveScanPlatform, send func(tea.Msg)) {
	var done saveScanDone
	fail := func(err string) {
		done.err, done.cancelled = err, ctx.Err() != nil
		send(done)
	}
	done.devices = fetchDevices(ctx, e)
	for _, p := range plats {
		send(saveScanProgress{text: "Saves: listing " + p.name})
		core := saveCoreFor(e, p.slug, p.name)
		var local map[string]localSave
		if core != "" {
			if d := existingDir(savesDir, core); d != "" {
				local = scanSaveDir(filepath.Join(savesDir, d))
			}
		}
		cloud, err := fetchSaves(ctx, e, p.id)
		if err != nil {
			fail("listing the saves of " + p.name + ": " + explainErr(err, clockOK()))
			return
		}
		byRom := map[string][]srvSave{}
		for _, s := range cloud {
			id := strconv.Itoa(s.RomID)
			byRom[id] = append(byRom[id], s)
		}
		used := map[string]bool{}
		for _, g := range p.games {
			key := strings.ToLower(baseName(g.name))
			l, hasLocal := local[key]
			versions := byRom[g.id]
			if !hasLocal && len(versions) == 0 {
				continue
			}
			r := &saveRow{gameID: g.id, pid: p.id, name: g.name, platform: p.name, core: core, cloudN: len(versions)}
			if hasLocal {
				used[key] = true
				l := l
				r.local = &l
			}
			r.cloud = newestSave(versions)
			done.rows = append(done.rows, r)
		}
		done.orphans += len(local) - len(used)
	}
	for i, r := range done.rows {
		if ctx.Err() != nil {
			fail("cancelled")
			return
		}
		send(saveScanProgress{text: fmt.Sprintf("Saves: comparing %d/%d", i+1, len(done.rows))})
		compareSave(ctx, e, r)
	}
	sort.SliceStable(done.rows, func(i, j int) bool {
		a, b := done.rows[i], done.rows[j]
		if ra, rb := stateOrder[a.state], stateOrder[b.state]; ra != rb {
			return ra < rb
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	})
	send(done)
}

// compareSave sets the state of a row. The cloud save is downloaded only when its hash cannot
// settle it (a save converted from the card has exactly the hash RomM computed).
func compareSave(ctx context.Context, e env, r *saveRow) {
	f := savefmt.ForCore(r.core)
	switch {
	case r.local == nil && r.cloud == nil:
		r.state = ssCloudOnly // versions exist but all are missing on the server
		r.err = "the files of this save are missing on the server"
		return
	case f == nil || !f.Verified:
		r.state = ssUnsupported
		return
	case r.cloud == nil:
		r.state = ssLocalOnly
		return
	case r.local == nil:
		r.state = ssCloudOnly
		return
	}
	mister, err := readSave(r.local.path)
	if err != nil {
		r.state, r.err = ssError, err.Error()
		return
	}
	if raw, err := f.ToRetroArch(mister); err == nil && strings.EqualFold(md5hex(raw), r.cloud.Hash) {
		r.state = ssSame
		return
	}
	raw, err := fetchSaveContent(ctx, e, *r.cloud)
	switch {
	case err != nil:
		r.state, r.err = ssError, explainErr(err, clockOK())
	case isZip(raw):
		r.state, r.err = ssUnsupported, "the RomM save is an archive (uploaded by another kind of client)"
	case f.Same(mister, raw):
		r.state = ssSame
	default:
		r.state = ssDifferent
	}
}

// readSave reads a save of the card, refusing anything that cannot be a save.
func readSave(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > savefmt.MaxSize {
		return nil, fmt.Errorf("the save on the card is too large (%s)", humanSize(st.Size()))
	}
	return os.ReadFile(path)
}
