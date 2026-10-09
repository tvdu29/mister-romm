package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// fakeSaves serves /api/saves for platform 1 and the content of each save.
func fakeSaves(t *testing.T, saves []srvSave, content map[int][]byte, fetched *[]int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/saves" && r.URL.Query().Get("platform_id") == "1":
			_ = json.NewEncoder(w).Encode(saves)
		case r.URL.Path == "/api/saves":
			_, _ = w.Write([]byte("[]"))
		case r.URL.Path == "/api/devices":
			_, _ = w.Write([]byte(`[{"id":"dev1","name":"Steam Deck"}]`))
		case strings.HasPrefix(r.URL.Path, "/api/saves/") && strings.HasSuffix(r.URL.Path, "/content"):
			var id int
			_, _ = fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/api/saves/"), "%d/content", &id)
			*fetched = append(*fetched, id)
			_, _ = w.Write(content[id])
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestSaveScanStates(t *testing.T) {
	root := t.TempDir()
	saves := filepath.Join(root, "saves")
	snes := filepath.Join(saves, "SNES")
	_ = os.MkdirAll(snes, 0o755)
	write := func(name string, b []byte) { _ = os.WriteFile(filepath.Join(snes, name), b, 0o644) }

	same := bytes.Repeat([]byte{1}, 8192)
	diffLocal, diffCloud := bytes.Repeat([]byte{2}, 8192), bytes.Repeat([]byte{3}, 8192)
	write("Same (USA).sav", same)
	write("Diff (USA).sav", diffLocal)
	write("Local (USA).sav", same)
	write("Orphan.sav", same)

	cloud := []srvSave{
		{ID: 10, RomID: 1, FileName: "Same (USA).srm", Slot: "autosave", Hash: md5hex(same)},
		{ID: 11, RomID: 2, FileName: "Diff (USA).srm", Slot: "autosave", Hash: md5hex(diffCloud), Device: "dev1"},
		{ID: 12, RomID: 2, FileName: "Diff (USA) old.srm", Slot: "", Hash: md5hex(diffLocal)}, // older slot-less copy: not chosen
		{ID: 13, RomID: 4, FileName: "Cloud (USA).srm", Slot: "autosave", Hash: md5hex(same)},
	}
	var fetched []int
	srv := fakeSaves(t, cloud, map[int][]byte{11: diffCloud}, &fetched)
	defer srv.Close()

	e := env{base: srv.URL, client: srv.Client(), gamesDir: filepath.Join(root, "games"), mapping: map[string]string{}}
	plat := saveScanPlatform{id: "1", slug: "snes", name: "SNES", games: []saveScanGame{
		{"1", "Same (USA).sfc"}, {"2", "Diff (USA).sfc"}, {"3", "Local (USA).sfc"}, {"4", "Cloud (USA).sfc"}, {"5", "Nothing (USA).sfc"},
	}}
	var done saveScanDone
	runSaveScan(context.Background(), e, saves, []saveScanPlatform{plat}, func(msg tea.Msg) {
		if d, ok := msg.(saveScanDone); ok {
			done = d
		}
	})
	if done.err != "" {
		t.Fatal(done.err)
	}
	got := map[string]saveState{}
	for _, r := range done.rows {
		got[r.gameID] = r.state
	}
	want := map[string]saveState{"1": ssSame, "2": ssDifferent, "3": ssLocalOnly, "4": ssCloudOnly}
	for id, st := range want {
		if got[id] != st {
			t.Errorf("game %s: state %d, want %d", id, got[id], st)
		}
	}
	if len(done.rows) != 4 {
		t.Errorf("%d rows, want 4 (a game without any save is not listed)", len(done.rows))
	}
	if done.rows[0].gameID != "2" {
		t.Errorf("differences must come first, got game %s", done.rows[0].gameID)
	}
	if done.orphans != 1 {
		t.Errorf("orphans = %d, want 1", done.orphans)
	}
	if len(fetched) != 1 || fetched[0] != 11 {
		t.Errorf("only the save whose hash differs should be downloaded, got %v", fetched)
	}
	if done.devices["dev1"] != "Steam Deck" {
		t.Error("device names not loaded")
	}
}

func TestSaveScanUnsupportedAndBadHash(t *testing.T) {
	root := t.TempDir()
	saves := filepath.Join(root, "saves")
	_ = os.MkdirAll(filepath.Join(saves, "Saturn"), 0o755)
	_ = os.MkdirAll(filepath.Join(saves, "GBA"), 0o755)
	_ = os.WriteFile(filepath.Join(saves, "Saturn", "Panzer.sav"), []byte{1, 2}, 0o644)
	_ = os.WriteFile(filepath.Join(saves, "GBA", "Zelda.sav"), bytes.Repeat([]byte{1}, 8192), 0o644)
	cloud := []srvSave{{ID: 20, RomID: 7, Slot: "autosave", Hash: strings.Repeat("0", 32)}}
	var fetched []int
	srv := fakeSaves(t, cloud, map[int][]byte{20: bytes.Repeat([]byte{9}, 8192)}, &fetched)
	defer srv.Close()
	e := env{base: srv.URL, client: srv.Client(), gamesDir: filepath.Join(root, "games"), mapping: map[string]string{}}
	plats := []saveScanPlatform{
		{id: "9", slug: "saturn", name: "Saturn", games: []saveScanGame{{"6", "Panzer.cue"}}},
		{id: "1", slug: "gba", name: "GBA", games: []saveScanGame{{"7", "Zelda.gba"}}},
	}
	var done saveScanDone
	runSaveScan(context.Background(), e, saves, plats, func(msg tea.Msg) {
		if d, ok := msg.(saveScanDone); ok {
			done = d
		}
	})
	states := map[string]*saveRow{}
	for _, r := range done.rows {
		states[r.gameID] = r
	}
	if r := states["6"]; r == nil || r.state != ssUnsupported {
		t.Error("a Saturn save must be listed as not supported")
	}
	if r := states["7"]; r == nil || r.state != ssError || !strings.Contains(r.err, "hash") {
		t.Errorf("a save that does not match its hash must be an error, got %+v", r)
	}
}

func TestBaseName(t *testing.T) {
	for in, want := range map[string]string{
		"Super Metroid (USA).sfc":     "Super Metroid (USA)",
		"Final Fantasy VII (USA)":     "Final Fantasy VII (USA)", // a multi-file game (folder)
		"Hits Vol. 2 (USA)":           "Hits Vol. 2 (USA)",
		"Sonic the Hedgehog (W).md":   "Sonic the Hedgehog (W)",
		"Pokemon - Emerald (USA).gba": "Pokemon - Emerald (USA)",
		"Gran Turismo 2 (USA).m3u":    "Gran Turismo 2 (USA)",
		"Strange.Name.v1.1 (USA).nes": "Strange.Name.v1.1 (USA)",
		"Crash Bandicoot (USA).chd":   "Crash Bandicoot (USA)",
	} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSavesViewRenders(t *testing.T) {
	m := bigModel(400)
	for i, it := range m.left {
		if it.kind == "saves" {
			m.selectLeft(i)
		}
	}
	if m.cur().kind != "saves" {
		t.Fatal("no Saves entry in the menu")
	}
	if out := m.render(); !strings.Contains(out, "Enter or r to compare") {
		t.Error("the view should explain how to start before the first comparison")
	}
	now := time.Date(2026, 10, 9, 18, 40, 0, 0, time.UTC)
	m.finishSaveScan(saveScanDone{
		devices: map[string]string{"dev1": "Steam Deck"},
		rows: []*saveRow{
			{gameID: "1", name: "Super Metroid (USA).sfc", platform: "SNES", core: "SNES", state: ssDifferent,
				local: &localSave{path: "/media/fat/saves/SNES/Super Metroid (USA).sav", size: 8192, modTime: time.Unix(5, 0)},
				cloud: &srvSave{ID: 3, FileName: "Super Metroid (USA) [2026-10-09_18-40-00].srm", Slot: "autosave", Device: "dev1", UpdatedAt: now, Size: 8192}, cloudN: 3},
			{gameID: "2", name: "Zelda (USA).sfc", platform: "SNES", core: "SNES", state: ssSame},
		},
	})
	for _, w := range []int{50, 80, 160} {
		m.w = w
		out := m.render()
		if !strings.Contains(out, "DIFF") || !strings.Contains(out, "Super Metr") {
			t.Errorf("width %d: rows missing:\n%s", w, out)
		}
		for i, l := range strings.Split(out, "\n") {
			if width(l) > w {
				t.Errorf("width %d: line %d is %d cells wide", w, i, width(l))
			}
		}
	}
	m.focus = fRight
	m.savesKey("enter")
	if m.pop == nil || !strings.Contains(strings.Join(m.pop.lines, "\n"), "date unknown") || !strings.Contains(strings.Join(m.pop.lines, "\n"), "Steam Deck") {
		t.Errorf("details window: %+v", m.pop)
	}
	_ = m.render()
}
