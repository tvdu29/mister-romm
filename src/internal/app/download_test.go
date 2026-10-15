package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// dlFor builds the download of a fake game into the games folder of e.
func dlFor(e env, g *fakeGame, folder string) dlItem {
	return dlItem{id: strconv.Itoa(g.id), name: g.name, dest: filepath.Join(e.gamesDir, folder, g.name),
		size: int64(len(g.data)), sha1: g.sha1()}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestDownloadSurvivesACutConnectionAndResumes(t *testing.T) {
	rom := newFakeRomm(t)
	g := rom.addPlatform(1, "gb", "Game Boy", 1).games[0]
	rom.fault, rom.faultOnce = "cut:400", true
	e := testEnv(t, rom)
	it := dlFor(e, g, "GAMEBOY")

	if err := downloadOne(context.Background(), e, it, func(dlProgress) {}); err == nil {
		t.Fatal("a cut connection must be reported as an error")
	}
	if exists(it.dest) {
		t.Fatal("a half file must never look like a game")
	}
	if st, err := os.Stat(it.dest + ".part"); err != nil || st.Size() != 400 {
		t.Fatalf(".part = %v, %v; want the 400 bytes received", st, err)
	}
	// next time the download continues where it stopped
	if err := downloadOne(context.Background(), e, it, func(dlProgress) {}); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, it.dest)) != string(g.data) {
		t.Error("the resumed file differs from the original")
	}
	if got := rom.ranges; len(got) != 2 || got[0] != "" || got[1] != "bytes=400-" {
		t.Errorf("Range headers = %q", got)
	}
	if exists(it.dest + ".part") {
		t.Error(".part should be gone")
	}
}

func TestDownloadRestartsWhenTheServerIgnoresRange(t *testing.T) {
	rom := newFakeRomm(t)
	g := rom.addPlatform(1, "gb", "Game Boy", 1).games[0]
	rom.fault = "ignore-range"
	e := testEnv(t, rom)
	it := dlFor(e, g, "GAMEBOY")
	_ = os.MkdirAll(filepath.Dir(it.dest), 0o755)
	_ = os.WriteFile(it.dest+".part", []byte("garbage-from-an-older-attempt"), 0o644)
	if err := downloadOne(context.Background(), e, it, func(dlProgress) {}); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, it.dest)) != string(g.data) {
		t.Error("the file must be rebuilt from zero, not appended to the old part")
	}
}

func TestDownloadWithAnInconsistentContentRange(t *testing.T) {
	rom := newFakeRomm(t)
	g := rom.addPlatform(1, "gb", "Game Boy", 1).games[0]
	e := testEnv(t, rom)
	it := dlFor(e, g, "GAMEBOY")

	// a server that always answers a range from the wrong offset: no corrupted file may result
	rom.fault = "bad-range"
	_ = os.MkdirAll(filepath.Dir(it.dest), 0o755)
	_ = os.WriteFile(it.dest+".part", g.data[:100], 0o644)
	if err := downloadOne(context.Background(), e, it, func(dlProgress) {}); err == nil {
		t.Error("expected an error")
	}
	if exists(it.dest) {
		t.Error("no game file may exist after an inconsistent answer")
	}

	// a server that does it once: the program starts over and succeeds
	rom.fault, rom.faultOnce, rom.contentN = "bad-range", true, 0
	_ = os.WriteFile(it.dest+".part", g.data[:100], 0o644)
	if err := downloadOne(context.Background(), e, it, func(dlProgress) {}); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, it.dest)) != string(g.data) {
		t.Error("the file differs from the original")
	}
}

func TestDownloadRefusesMoreDataThanAnnounced(t *testing.T) {
	rom := newFakeRomm(t)
	g := rom.addPlatform(1, "gb", "Game Boy", 1).games[0]
	e := testEnv(t, rom)
	it := dlFor(e, g, "GAMEBOY")
	it.size = int64(len(g.data)) - 100 // the catalog says less than what the server sends
	err := downloadOne(context.Background(), e, it, func(dlProgress) {})
	if err == nil || !strings.Contains(err.Error(), "more data") {
		t.Fatalf("err = %v", err)
	}
	if exists(it.dest) {
		t.Error("the game file must not exist")
	}
}

func TestDownloadRefusesAShortFile(t *testing.T) {
	rom := newFakeRomm(t)
	g := rom.addPlatform(1, "gb", "Game Boy", 1).games[0]
	e := testEnv(t, rom)
	it := dlFor(e, g, "GAMEBOY")
	it.size = int64(len(g.data)) + 100 // the catalog says more than what the server has
	err := downloadOne(context.Background(), e, it, func(dlProgress) {})
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("err = %v", err)
	}
	if exists(it.dest) {
		t.Error("the game file must not exist")
	}
}

func TestDownloadDiscardsAnOversizedPart(t *testing.T) {
	rom := newFakeRomm(t)
	g := rom.addPlatform(1, "gb", "Game Boy", 1).games[0]
	e := testEnv(t, rom)
	it := dlFor(e, g, "GAMEBOY")
	_ = os.MkdirAll(filepath.Dir(it.dest), 0o755)
	_ = os.WriteFile(it.dest+".part", append(append([]byte{}, g.data...), "extra"...), 0o644)
	if err := downloadOne(context.Background(), e, it, func(dlProgress) {}); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, it.dest)) != string(g.data) {
		t.Error("the file differs from the original")
	}
}

func TestDownloadHTTPErrors(t *testing.T) {
	for _, tc := range []struct{ fault, want string }{
		{"500", "HTTP 500"}, {"404", "HTTP 404"},
	} {
		rom := newFakeRomm(t)
		g := rom.addPlatform(1, "gb", "Game Boy", 1).games[0]
		rom.fault = tc.fault
		e := testEnv(t, rom)
		it := dlFor(e, g, "GAMEBOY")
		err := downloadOne(context.Background(), e, it, func(dlProgress) {})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", tc.fault, err)
		}
		if exists(it.dest) {
			t.Errorf("%s: no game file may be created", tc.fault)
		}
	}
	// a rejected token says so
	rom := newFakeRomm(t)
	g := rom.addPlatform(1, "gb", "Game Boy", 1).games[0]
	e := testEnv(t, rom)
	e.token = "revoked-token-12345"
	err := downloadOne(context.Background(), e, dlFor(e, g, "GAMEBOY"), func(dlProgress) {})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("err = %v", err)
	}
}

func TestDownloadRefusesToWriteOutsideTheGamesFolder(t *testing.T) {
	rom := newFakeRomm(t)
	g := rom.addPlatform(1, "gb", "Game Boy", 1).games[0]
	e := testEnv(t, rom)
	outside := filepath.Join(filepath.Dir(e.gamesDir), "outside.gb")
	for _, it := range []dlItem{
		{id: strconv.Itoa(g.id), name: "../outside.gb", dest: filepath.Join(e.gamesDir, "GB", "../../outside.gb"), size: 10},
		{id: strconv.Itoa(g.id), name: "ok.gb", dest: outside, size: 10},
		{id: strconv.Itoa(g.id), name: "a/b.gb", dest: filepath.Join(e.gamesDir, "a", "b.gb"), size: 10},
	} {
		if err := downloadOne(context.Background(), e, it, func(dlProgress) {}); err == nil {
			t.Errorf("%+v: expected a refusal", it)
		}
	}
	if exists(outside) || exists(outside+".part") {
		t.Error("a file was written outside the games folder")
	}
	if n := rom.count("GET /api/roms/"); n != 0 {
		t.Errorf("%d requests were made for refused downloads", n)
	}
}

func TestRunDownloadsReportsEachGameAndContinuesAfterAFailure(t *testing.T) {
	rom := newFakeRomm(t)
	p := rom.addPlatform(1, "gb", "Game Boy", 3)
	e := testEnv(t, rom)
	missing := &fakeGame{id: 99999, name: "Not On Server.gb", data: []byte("x")}
	items := []dlItem{dlFor(e, p.games[0], "GAMEBOY"), dlFor(e, missing, "GAMEBOY"), dlFor(e, p.games[2], "GAMEBOY")}

	var starts []int
	done := map[int]dlDoneMsg{}
	runDownloads(context.Background(), e, items, func(m tea.Msg) {
		switch x := m.(type) {
		case dlStart:
			starts = append(starts, x.idx)
		case dlDoneMsg:
			done[x.idx] = x
		}
	})
	if len(starts) != 3 || len(done) != 3 {
		t.Fatalf("starts=%v done=%v", starts, done)
	}
	if done[0].err != "" || done[2].err != "" {
		t.Errorf("good games failed: %+v", done)
	}
	if !strings.Contains(done[1].err, "404") {
		t.Errorf("the missing game should fail with a 404: %+v", done[1])
	}
	if !exists(items[0].dest) || !exists(items[2].dest) || exists(items[1].dest) {
		t.Error("wrong set of files on the card")
	}
	if b, _ := os.ReadFile(filepath.Join(e.cfgDir, "last-error.log")); !strings.Contains(string(b), "Not On Server.gb") {
		t.Errorf("the failure was not logged: %q", b)
	}
}

func TestRunDownloadsCancelled(t *testing.T) {
	rom := newFakeRomm(t)
	p := rom.addPlatform(1, "gb", "Game Boy", 3)
	e := testEnv(t, rom)
	var items []dlItem
	for _, g := range p.games {
		items = append(items, dlFor(e, g, "GAMEBOY"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := map[int]dlDoneMsg{}
	runDownloads(ctx, e, items, func(m tea.Msg) {
		if x, ok := m.(dlDoneMsg); ok {
			done[x.idx] = x
			if x.idx == 0 {
				cancel() // the user cancels once the first game is finished
			}
		}
	})
	if done[0].err != "" || !done[1].cancelled || !done[2].cancelled {
		t.Errorf("done = %+v", done)
	}
	if exists(items[1].dest) || exists(items[2].dest) {
		t.Error("cancelled games must not be on the card")
	}
}

// ------------------------------------------------------------------ through the interface

// syncedHarness has a synced catalog of n games of one platform.
func syncedHarness(t *testing.T, slug, name string, n int) (*harness, *fakePlatform) {
	t.Helper()
	rom := newFakeRomm(t)
	p := rom.addPlatform(1, slug, name, n)
	h := newHarness(t, rom)
	h.selectPlatforms(p)
	h.press("s")
	if len(h.m.data.Games) != n {
		t.Fatalf("catalog has %d games, want %d", len(h.m.data.Games), n)
	}
	h.press("down") // left panel: the platform
	h.press("tab")  // focus the games
	return h, p
}

func TestUIDownloadSelectedGames(t *testing.T) {
	h, p := syncedHarness(t, "gb", "Game Boy", 5)
	h.press("space", "space") // tick the first two (the cursor moves down)
	h.press("d")
	if h.popTitle() != "Download" {
		t.Fatalf("popup = %q", h.popText())
	}
	txt := h.popText()
	for _, want := range []string{"Download 2 game(s)", "Free space:", "left after", "Game Boy 000.gb", "Game Boy 001.gb"} {
		if !strings.Contains(txt, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, txt)
		}
	}
	h.press("y")
	for i := 0; i < 2; i++ {
		dest := filepath.Join(h.root, "games", "GAMEBOY", p.games[i].name)
		if string(readFile(t, dest)) != string(p.games[i].data) {
			t.Errorf("%s is wrong on the card", dest)
		}
	}
	if exists(filepath.Join(h.root, "games", "GAMEBOY", p.games[2].name)) {
		t.Error("an unticked game was downloaded")
	}
	st := readState(h.cfgDir())
	if len(st) != 2 || st[strconv.Itoa(p.games[0].id)] == nil {
		t.Errorf("state.json = %v", st)
	}
	if !h.m.data.Games[0].Installed || !h.m.data.Games[1].Installed || h.m.data.Games[2].Installed {
		t.Error("Installed flags are wrong")
	}
	if h.m.toast != "Downloaded 2 game(s)" {
		t.Errorf("toast = %q", h.m.toast)
	}
	if len(h.m.sel) != 0 {
		t.Error("the selection should be cleared after queueing")
	}

	// asking again for the same games has nothing to do
	h.press("esc", "home", "d") // esc closes the downloads window the queue opened
	if !strings.Contains(h.popText(), "Nothing to download") {
		t.Errorf("popup = %q", h.popText())
	}
}

func TestUIDownloadCurrentGameWhenNothingIsTicked(t *testing.T) {
	h, p := syncedHarness(t, "gb", "Game Boy", 3)
	h.press("down", "d", "enter") // 2nd game; Yes is the default button
	if !exists(filepath.Join(h.root, "games", "GAMEBOY", p.games[1].name)) {
		t.Errorf("the game under the cursor was not downloaded; screen:\n%s", h.screen())
	}
}

func TestUIDownloadNeedsAFolderForThePlatform(t *testing.T) {
	h, _ := syncedHarness(t, "weird-console", "Weird Console", 2)
	h.press("d")
	txt := h.popText()
	if !strings.Contains(txt, "No folder known for: Weird Console") || !strings.Contains(txt, "Platform folders") {
		t.Errorf("popup = %q", txt)
	}
	// once a folder is mapped the download works
	h.press("esc")
	if err := saveMapping(h.cfgDir(), map[string]string{"weird-console": "Weird"}); err != nil {
		t.Fatal(err)
	}
	h.press("d", "y")
	if !exists(filepath.Join(h.root, "games", "Weird", "Weird Console 000.gb")) {
		t.Error("the game was not downloaded into the mapped folder")
	}
}

func TestUIDownloadReusesAnExistingFolderIgnoringCase(t *testing.T) {
	h, _ := syncedHarness(t, "gb", "Game Boy", 1)
	if err := os.MkdirAll(filepath.Join(h.root, "games", "gameboy-lowercase"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveMapping(h.cfgDir(), map[string]string{"gb": "GAMEBOY-LOWERCASE"}); err != nil {
		t.Fatal(err)
	}
	h.press("d", "y")
	if !exists(filepath.Join(h.root, "games", "gameboy-lowercase", "Game Boy 000.gb")) {
		t.Error("the existing folder was not reused")
	}
}

func TestUIDownloadNotEnoughSpace(t *testing.T) {
	h, _ := syncedHarness(t, "gb", "Game Boy", 2)
	h.m.data.Games[0].Size = 1 << 60
	h.press("d")
	if h.popTitle() != "Not enough space" {
		t.Errorf("popup = %q", h.popText())
	}
	if h.rom.count("GET /api/roms/") != 0 {
		t.Error("nothing may be requested when the card is too small")
	}
}

func TestUIDownloadWarnsWhenLittleSpaceRemains(t *testing.T) {
	h, _ := syncedHarness(t, "gb", "Game Boy", 1)
	free, ok := freeSpace(h.m.gamesDir())
	if !ok || free < 2*lowSpace {
		t.Skip("not enough free disk space to build the scenario")
	}
	h.m.data.Games[0].Size = free - lowSpace/2 // fits, but leaves less than the warning threshold
	h.press("d")
	if h.popTitle() != "Download" || !strings.Contains(h.popText(), "(low space!)") {
		t.Errorf("popup = %q", h.popText())
	}
	h.press("esc")
	h.m.data.Games[0].Size = 1000 // plenty left
	h.press("d")
	if strings.Contains(h.popText(), "low space") {
		t.Errorf("popup = %q", h.popText())
	}
}

func TestUIDownloadSkipsGamesAlreadyOnTheCard(t *testing.T) {
	h, p := syncedHarness(t, "gb", "Game Boy", 3)
	dir := filepath.Join(h.root, "games", "GAMEBOY")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, p.games[0].name), p.games[0].data, 0o644) // same size: present
	_ = os.WriteFile(filepath.Join(dir, p.games[1].name), []byte("different"), 0o644)
	h.press("a", "d")
	txt := h.popText()
	for _, want := range []string{"Download 1 game(s)", "1 already on the card", "1 with another file of the same name"} {
		if !strings.Contains(txt, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, txt)
		}
	}
	h.press("y")
	if string(readFile(t, filepath.Join(dir, p.games[1].name))) != "different" {
		t.Error("a file that is not ours was overwritten")
	}
}

func TestUICancelDownloads(t *testing.T) {
	h, p := syncedHarness(t, "gb", "Game Boy", 3)
	h.rom.contentDelay = 5 * time.Second
	h.press("a")
	_, cmd := h.m.Update(keyMsg("d"))
	h.pump(cmd)
	_, cmd = h.m.Update(keyMsg("y"))
	if h.m.dlJob == nil {
		t.Fatal("the downloads did not start")
	}
	time.Sleep(100 * time.Millisecond)
	h.m.Update(keyMsg("esc")) // hide the downloads window
	h.m.Update(keyMsg("c"))
	start := time.Now()
	h.pump(cmd)
	if time.Since(start) > 4*time.Second {
		t.Error("cancelling must interrupt the transfer")
	}
	if h.m.dlJob != nil {
		t.Error("the download job should be finished")
	}
	for _, g := range p.games {
		if exists(filepath.Join(h.root, "games", "GAMEBOY", g.name)) {
			t.Errorf("%s must not be on the card", g.name)
		}
	}
	if !strings.Contains(h.m.toast, "cancelled") {
		t.Errorf("toast = %q", h.m.toast)
	}
}

func TestUIDownloadFailureIsSummarised(t *testing.T) {
	h, _ := syncedHarness(t, "gb", "Game Boy", 2)
	h.rom.fault = "500"
	h.press("a", "d", "y")
	if h.popTitle() != "Downloads finished with errors" || !strings.Contains(h.popText(), "Failed: 2") {
		t.Errorf("popup = %q", h.popText())
	}
	if len(readState(h.cfgDir())) != 0 {
		t.Error("failed games must not be recorded as installed")
	}
}

func TestUIDeleteOnlyWhatWasDownloaded(t *testing.T) {
	h, p := syncedHarness(t, "gb", "Game Boy", 2)
	h.press("d", "y") // downloads the first game
	g := h.m.data.Games[0]
	dest := filepath.Join(h.root, "games", "GAMEBOY", p.games[0].name)
	if !g.Installed || !exists(dest) {
		t.Fatal("setup: the game should be installed")
	}
	h.m.deleteGame(g)
	if exists(dest) || g.Installed || len(readState(h.cfgDir())) != 0 {
		t.Error("the game was not removed cleanly")
	}

	// a file this program did not download is never touched
	other := filepath.Join(h.root, "games", "GAMEBOY", p.games[1].name)
	_ = os.WriteFile(other, []byte("mine"), 0o644)
	h.m.deleteGame(h.m.data.Games[1])
	if !exists(other) {
		t.Error("an untracked file was deleted")
	}
	if !strings.Contains(h.popText(), "left alone") {
		t.Errorf("popup = %q", h.popText())
	}
}
