package app

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// collect runs runSync and returns the final message and the progress texts.
func collect(t *testing.T, e env, plats map[string]*selPlatform, full bool) syncDone {
	t.Helper()
	return collectCtx(t, context.Background(), e, plats, full)
}

func collectCtx(t *testing.T, ctx context.Context, e env, plats map[string]*selPlatform, full bool) syncDone {
	t.Helper()
	var last syncDone
	got := false
	runSync(ctx, e, plats, full, func(m tea.Msg) {
		if d, ok := m.(syncDone); ok {
			last, got = d, true
		}
	})
	if !got {
		t.Fatal("runSync never reported its end")
	}
	return last
}

func TestSyncIndexesEveryPage(t *testing.T) {
	rom := newFakeRomm(t)
	rom.addPlatform(1, "gb", "Game Boy", 120) // 3 pages of 50
	e := testEnv(t, rom)
	d := collect(t, e, map[string]*selPlatform{"1": {Slug: "gb", Name: "Game Boy"}}, false)
	if d.err != "" {
		t.Fatal(d.err)
	}
	if d.total != 120 || d.added != 120 {
		t.Errorf("total=%d added=%d, want 120/120", d.total, d.added)
	}
	rows := readCatalog(catalogPath(e.cfgDir, "1"))
	if len(rows) != 120 {
		t.Fatalf("catalog has %d rows", len(rows))
	}
	r := rows[strconv.Itoa(1*10000+7)]
	if r.name != "Game Boy 007.gb" || r.size != 1007 || r.sha1 == "" {
		t.Errorf("row = %+v", r)
	}
	if n := rom.count("GET /api/roms?"); n != 3 {
		t.Errorf("%d page requests, want 3", n)
	}
	if d.synced["1"] == "" {
		t.Error("the sync date of the platform is missing")
	}
}

func TestSyncIncrementalOnlyAsksForChanges(t *testing.T) {
	rom := newFakeRomm(t)
	p := rom.addPlatform(1, "gb", "Game Boy", 30)
	e := testEnv(t, rom)
	sel := map[string]*selPlatform{"1": {Slug: "gb", Name: "Game Boy"}}
	first := collect(t, e, sel, false)
	sel["1"].SyncedAt = first.synced["1"]

	// one game changes on the server, one is added
	p.games[3].updated = time.Now()
	p.games[3].data = append(p.games[3].data, 'z')
	p.games = append(p.games, &fakeGame{id: 10999, name: "New Game.gb", data: []byte("new"), updated: time.Now()})

	second := collect(t, e, sel, false)
	if second.err != "" {
		t.Fatal(second.err)
	}
	if second.added != 1 || second.total != 31 {
		t.Errorf("added=%d total=%d, want 1/31", second.added, second.total)
	}
	reqs := rom.requests()
	if !strings.Contains(reqs[len(reqs)-1], "updated_after=") {
		t.Errorf("the second sync did not ask for changes only: %s", reqs[len(reqs)-1])
	}
	rows := readCatalog(catalogPath(e.cfgDir, "1"))
	if rows["10003"].size != 1004 {
		t.Errorf("the changed game was not refreshed: %+v", rows["10003"])
	}
	if rows["10999"].name != "New Game.gb" {
		t.Error("the new game is missing")
	}

	// a full sync ignores the saved date
	collect(t, e, sel, true)
	reqs = rom.requests()
	if strings.Contains(reqs[len(reqs)-1], "updated_after=") {
		t.Errorf("a full sync must not be incremental: %s", reqs[len(reqs)-1])
	}
}

func TestSyncRefusesUnsafeNames(t *testing.T) {
	rom := newFakeRomm(t)
	p := rom.addPlatform(1, "gb", "Game Boy", 2)
	p.games = append(p.games,
		&fakeGame{id: 501, name: "../../etc/passwd", data: []byte("x"), updated: time.Now()},
		&fakeGame{id: 502, name: "a/b.gb", data: []byte("x"), updated: time.Now()},
		&fakeGame{id: 503, name: "evil\x1b[2J.gb", data: []byte("x"), updated: time.Now()},
		&fakeGame{id: 504, name: "..", data: []byte("x"), updated: time.Now()},
	)
	e := testEnv(t, rom)
	d := collect(t, e, map[string]*selPlatform{"1": {Slug: "gb", Name: "Game Boy"}}, false)
	if d.err != "" {
		t.Fatal(d.err)
	}
	if d.unsafe != 4 || d.total != 2 {
		t.Errorf("unsafe=%d total=%d, want 4/2", d.unsafe, d.total)
	}
	for _, r := range readCatalog(catalogPath(e.cfgDir, "1")) {
		if !safeName(r.name) {
			t.Errorf("unsafe name indexed: %q", r.name)
		}
	}
}

func TestSyncErrors(t *testing.T) {
	sel := map[string]*selPlatform{"1": {Slug: "gb", Name: "Game Boy"}}

	t.Run("rejected token", func(t *testing.T) {
		rom := newFakeRomm(t)
		rom.addPlatform(1, "gb", "Game Boy", 3)
		e := testEnv(t, rom)
		e.token = "wrong-token-value"
		d := collect(t, e, sel, false)
		if !strings.Contains(d.err, "401") || !strings.Contains(d.err, "token") {
			t.Errorf("err = %q", d.err)
		}
		if _, err := os.Stat(catalogPath(e.cfgDir, "1")); err == nil {
			t.Error("a failed sync must not write a catalog")
		}
	})
	t.Run("server down", func(t *testing.T) {
		rom := newFakeRomm(t)
		e := testEnv(t, rom)
		rom.srv.Close()
		d := collect(t, e, sel, false)
		if d.err == "" || !strings.Contains(d.err, "Game Boy") {
			t.Errorf("err = %q", d.err)
		}
	})
	t.Run("not a game list", func(t *testing.T) {
		rom := newFakeRomm(t)
		rom.addPlatform(1, "gb", "Game Boy", 1)
		for _, body := range []string{"<html>proxy login</html>", `{"items": "no", "total": 1}`, " ", `{"items":[{"id":"x"}]}`} {
			rom.romsBody = body
			e := testEnv(t, rom)
			d := collect(t, e, sel, false)
			if d.err == "" {
				t.Errorf("body %q: the sync should fail", body)
			}
			if _, err := os.Stat(catalogPath(e.cfgDir, "1")); err == nil {
				t.Errorf("body %q: no catalog must be written", body)
			}
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		rom := newFakeRomm(t)
		rom.addPlatform(1, "gb", "Game Boy", 60)
		rom.pageDelay = 300 * time.Millisecond
		e := testEnv(t, rom)
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		d := collectCtx(t, ctx, e, sel, false)
		if !d.cancelled || d.err == "" {
			t.Errorf("cancelled=%v err=%q", d.cancelled, d.err)
		}
	})
}

func TestFetchPlatformsCleansAndSorts(t *testing.T) {
	rom := newFakeRomm(t)
	rom.addPlatform(1, "snes", "Super Nintendo", 1)
	rom.addPlatform(2, "gb", "Game\x1b[31m Boy", 1)
	list, err := fetchPlatforms(context.Background(), testEnv(t, rom))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "Game[31m Boy" || list[1].Slug != "snes" {
		t.Errorf("list = %+v", list)
	}
}

// ------------------------------------------------------------------ through the interface

func TestUISyncLoadsTheCatalog(t *testing.T) {
	rom := newFakeRomm(t)
	gb := rom.addPlatform(1, "gb", "Game Boy", 40)
	h := newHarness(t, rom)
	h.selectPlatforms(gb)

	h.press("s")
	if len(h.m.data.Games) != 40 {
		t.Fatalf("%d games after the sync, want 40", len(h.m.data.Games))
	}
	if !strings.Contains(h.m.toast, "40 games") {
		t.Errorf("toast = %q", h.m.toast)
	}
	if h.m.syncJob != nil {
		t.Error("the sync job should be finished")
	}
	if sel := readSelection(h.cfgDir()); sel["1"].SyncedAt == "" {
		t.Error("the sync date was not saved in selection.json")
	}
	if h.m.cfg("catalog_synced", "") == "" {
		t.Error("catalog_synced was not recorded")
	}
	// the games are listed in the right panel and can be searched
	h.press("tab")
	if !strings.Contains(h.screen(), "Game Boy 000.gb") {
		t.Errorf("the catalog is not on screen:\n%s", h.screen())
	}
}

func TestUISyncNeedsAPlatformAndAPairedDevice(t *testing.T) {
	h := newHarness(t, nil) // no server, no token
	h.press("s")
	if h.popTitle() != "Sync catalog" || !strings.Contains(h.popText(), "No platform") {
		t.Errorf("popup = %q", h.popText())
	}
	h.press("esc")

	rom := newFakeRomm(t)
	gb := rom.addPlatform(1, "gb", "Game Boy", 1)
	h2 := newHarness(t, rom)
	h2.selectPlatforms(gb)
	h2.m.data.Token = ""
	h2.press("s")
	if !strings.Contains(h2.popText(), "not paired") {
		t.Errorf("popup = %q", h2.popText())
	}
}

func TestUISyncFailureIsReportedAndLogged(t *testing.T) {
	rom := newFakeRomm(t)
	gb := rom.addPlatform(1, "gb", "Game Boy", 3)
	h := newHarness(t, rom)
	h.selectPlatforms(gb)
	h.m.data.Token = "revoked-token-12345"
	h.press("s")
	if h.popTitle() != "Sync failed" {
		t.Fatalf("popup = %q", h.popText())
	}
	b, _ := os.ReadFile(h.cfgDir() + "/last-error.log")
	if !strings.Contains(string(b), "sync:") {
		t.Errorf("last-error.log = %q", b)
	}
}

func TestUICancelSync(t *testing.T) {
	rom := newFakeRomm(t)
	gb := rom.addPlatform(1, "gb", "Game Boy", 60)
	rom.pageDelay = 400 * time.Millisecond
	h := newHarness(t, rom)
	h.selectPlatforms(gb)

	_, cmd := h.m.Update(keyMsg("s"))
	if h.m.syncJob == nil {
		t.Fatal("the sync did not start")
	}
	h.m.Update(keyMsg("c"))
	h.pump(cmd)
	if h.m.syncJob != nil {
		t.Error("the sync job should be gone")
	}
	if h.m.toast != "Sync cancelled" {
		t.Errorf("toast = %q", h.m.toast)
	}
	if len(h.m.data.Games) != 0 {
		t.Error("a cancelled sync must not leave a catalog")
	}
	// a second sync cannot start while one runs
	rom.pageDelay = 300 * time.Millisecond
	_, cmd = h.m.Update(keyMsg("s"))
	h.m.Update(keyMsg("s"))
	if h.m.toast != "A sync is already running" {
		t.Errorf("toast = %q", h.m.toast)
	}
	h.m.Update(keyMsg("c"))
	h.pump(cmd)
}
