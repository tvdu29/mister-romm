package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// browsing is a harness with a synced catalog, the cursor on the first game.
func browsing(t *testing.T, n int) (*harness, *fakePlatform) {
	t.Helper()
	return syncedHarness(t, "gb", "Game Boy", n)
}

func TestUIFocusAndMenuNavigation(t *testing.T) {
	h, _ := browsing(t, 5)
	// syncedHarness moved to the platform entry and focused the games; go back to the menu
	h.press("esc")
	if h.m.focus != fLeft {
		t.Fatal("esc in the games must give the focus back to the menu")
	}
	h.press("home") // no effect in the menu
	start := h.m.lcur
	h.press("up")
	h.press("up")
	h.press("up")
	if h.m.lcur != 0 {
		t.Errorf("the cursor must stop at the first entry, lcur=%d (was %d)", h.m.lcur, start)
	}
	for i := 0; i < len(h.m.left)+3; i++ {
		h.press("down")
	}
	if h.m.lcur != len(h.m.left)-1 {
		t.Errorf("the cursor must stop at the last entry, lcur=%d of %d", h.m.lcur, len(h.m.left))
	}
	if h.m.cur().kind != "settings" {
		t.Errorf("last entry = %q", h.m.cur().kind)
	}
	h.press("tab")
	if h.m.focus != fRight {
		t.Error("tab must switch the panel")
	}
	h.press("tab")
	if h.m.focus != fLeft {
		t.Error("tab must switch back")
	}
}

func TestUIGameCursorKeepsInsideTheList(t *testing.T) {
	h, _ := browsing(t, 100)
	h.press("up")
	if h.m.rcur != 0 {
		t.Errorf("rcur = %d", h.m.rcur)
	}
	h.press("end")
	if h.m.rcur != 99 {
		t.Errorf("end: rcur = %d", h.m.rcur)
	}
	h.press("down", "down", "pgdown", "pgdown")
	if h.m.rcur != 99 {
		t.Errorf("past the end: rcur = %d", h.m.rcur)
	}
	h.press("home")
	h.press("pgdown")
	if h.m.rcur != h.m.listRows() {
		t.Errorf("pgdown moves by one page: rcur=%d rows=%d", h.m.rcur, h.m.listRows())
	}
	h.press("pgup", "pgup")
	if h.m.rcur != 0 {
		t.Errorf("rcur = %d", h.m.rcur)
	}
	for _, k := range []string{"j", "j", "j", "k"} {
		h.press(k)
	}
	if h.m.rcur != 2 {
		t.Errorf("j/k: rcur = %d", h.m.rcur)
	}
	h.press("G")
	if h.m.rcur != 99 {
		t.Errorf("G: rcur = %d", h.m.rcur)
	}
	h.press("g")
	if h.m.rcur != 0 {
		t.Errorf("g: rcur = %d", h.m.rcur)
	}
	if !strings.Contains(h.screen(), "Game Boy 000.gb") {
		t.Errorf("screen:\n%s", h.screen())
	}
}

func TestUISearch(t *testing.T) {
	rom := newFakeRomm(t)
	p := rom.addPlatform(1, "gb", "Game Boy", 3)
	p.games[0].name, p.games[1].name, p.games[2].name = "Super Mario Land.gb", "Mario Kart.gb", "Zelda Link's Awakening.gb"
	h := newHarness(t, rom)
	h.selectPlatforms(p)
	h.press("s", "down", "tab")
	if len(h.m.view) != 3 {
		t.Fatalf("view = %d", len(h.m.view))
	}

	h.press("/")
	if !h.m.searching {
		t.Fatal("/ must start the search")
	}
	h.typeText("mario")
	if len(h.m.view) != 2 {
		t.Errorf("'mario' matches %d games, want 2", len(h.m.view))
	}
	h.typeText(" land") // every word must appear
	if len(h.m.view) != 1 || h.m.data.Games[h.m.view[0]].Name != "Super Mario Land.gb" {
		t.Errorf("'mario land' view = %v", h.m.view)
	}
	h.press("backspace", "backspace", "backspace", "backspace", "backspace")
	if len(h.m.view) != 2 {
		t.Errorf("after backspace: %d games", len(h.m.view))
	}
	h.press("enter") // keep the filter, leave the field
	if h.m.searching || h.m.query != "mario" {
		t.Errorf("searching=%v query=%q", h.m.searching, h.m.query)
	}
	// keys act on the list again, and the filter limits what 'a' ticks
	h.press("a")
	if len(h.m.sel) != 2 {
		t.Errorf("%d ticked, want only the 2 shown", len(h.m.sel))
	}
	h.press("x")
	if len(h.m.sel) != 0 {
		t.Error("x must untick everything")
	}
	h.press("/", "esc")
	if h.m.query != "" || len(h.m.view) != 3 {
		t.Errorf("esc must clear the search: query=%q view=%d", h.m.query, len(h.m.view))
	}
	// case-insensitive
	h.press("/")
	h.typeText("ZELDA")
	if len(h.m.view) != 1 {
		t.Errorf("search must ignore case, got %d", len(h.m.view))
	}
	h.press("esc")
	// a search without result: no key may crash the interface
	h.press("/")
	h.typeText("nothing like this")
	if len(h.m.view) != 0 {
		t.Fatal("expected no result")
	}
	h.press("down", "enter", "d", "space", "esc", "esc")
	_ = h.screen()
}

func TestUISelection(t *testing.T) {
	h, _ := browsing(t, 4)
	h.press("space")
	if len(h.m.sel) != 1 || h.m.rcur != 1 {
		t.Errorf("sel=%d rcur=%d: space ticks and moves down", len(h.m.sel), h.m.rcur)
	}
	h.press("up", "space") // tick again the same game: untick
	if len(h.m.sel) != 0 {
		t.Errorf("space on a ticked game must untick it: %d", len(h.m.sel))
	}
	h.press("a")
	if len(h.m.sel) != 4 {
		t.Errorf("a: %d", len(h.m.sel))
	}
	h.press("x")
	if len(h.m.sel) != 0 {
		t.Errorf("x: %d", len(h.m.sel))
	}
	if !strings.Contains(h.screen(), "[ ]") {
		t.Errorf("the checkboxes are not drawn:\n%s", h.screen())
	}
}

func TestUIMenuViewsFollowThePlatformsAndInstalledGames(t *testing.T) {
	rom := newFakeRomm(t)
	gb := rom.addPlatform(1, "gb", "Game Boy", 3)
	snes := rom.addPlatform(2, "snes", "Super Nintendo", 2)
	h := newHarness(t, rom)
	h.selectPlatforms(gb, snes)
	h.press("s")
	if len(h.m.data.Games) != 5 || h.m.left[0].name != "All games" || h.m.left[0].count != "5" {
		t.Fatalf("games=%d left0=%+v", len(h.m.data.Games), h.m.left[0])
	}
	h.press("down") // Game Boy
	if len(h.m.view) != 3 {
		t.Errorf("Game Boy shows %d games", len(h.m.view))
	}
	h.press("down") // Super Nintendo
	if len(h.m.view) != 2 {
		t.Errorf("Super Nintendo shows %d games", len(h.m.view))
	}
	h.press("up", "tab", "d", "y") // download one Game Boy game
	h.press("esc", "tab")
	for h.m.cur().kind != "installed" {
		h.press("down")
	}
	if len(h.m.view) != 1 || !h.m.data.Games[h.m.view[0]].Installed {
		t.Errorf("Installed shows %d games", len(h.m.view))
	}
	if h.m.left[h.m.lcur].count != "1" {
		t.Errorf("Installed counter = %q", h.m.left[h.m.lcur].count)
	}
}

func TestUIContextMenu(t *testing.T) {
	h, p := browsing(t, 2)
	h.press("enter")
	if h.m.pop == nil || h.m.pop.kind != "menu" || strings.Join(h.m.pop.items, ",") != "Download,Game info" {
		t.Fatalf("popup = %+v", h.m.pop)
	}
	h.press("down", "enter") // Game info
	txt := h.popText()
	for _, want := range []string{"Game Boy 000.gb", "Game Boy", "On card:   no", "SHA1:      " + p.games[0].sha1()} {
		if !strings.Contains(txt, want) {
			t.Errorf("info lacks %q:\n%s", want, txt)
		}
	}
	h.press("esc")
	h.press("enter", "enter", "y") // Download, confirm
	h.press("esc")
	h.press("enter") // now installed: offers deletion
	if h.m.pop == nil || h.m.pop.items[0] != "Delete from the card" {
		t.Fatalf("popup = %+v", h.m.pop)
	}
	h.press("enter")
	if h.popTitle() != "Delete" {
		t.Fatalf("popup = %q", h.popText())
	}
	h.press("n")
	if !exists(filepath.Join(h.root, "games", "GAMEBOY", p.games[0].name)) {
		t.Error("declining must keep the game")
	}
	h.press("enter", "enter", "y")
	if exists(filepath.Join(h.root, "games", "GAMEBOY", p.games[0].name)) {
		t.Error("the game should be deleted")
	}
}

func TestUIQuit(t *testing.T) {
	h := newHarness(t, nil)
	h.press("q")
	if h.popTitle() != "Quit" || h.quit {
		t.Fatalf("popup = %q quit=%v", h.popText(), h.quit)
	}
	h.press("n")
	if h.quit || h.m.pop != nil {
		t.Error("declining must keep running")
	}
	h.press("q", "esc")
	if h.quit {
		t.Error("esc must not quit")
	}
	h.press("q", "y")
	if !h.quit {
		t.Error("yes must quit")
	}

	h2 := newHarness(t, nil)
	h2.press("ctrl+c")
	if !h2.quit {
		t.Error("ctrl+c quits at once")
	}
	h3 := newHarness(t, nil)
	h3.m.openHelp()
	h3.press("ctrl+c")
	if !h3.quit {
		t.Error("ctrl+c quits even from a window")
	}

	h4 := newHarness(t, nil)
	h4.m.dlJob = &job{cancel: func() {}}
	h4.press("q")
	if !strings.Contains(h4.popText(), "still running") {
		t.Errorf("popup = %q", h4.popText())
	}
}

func TestUIHelp(t *testing.T) {
	h := newHarness(t, nil)
	h.press("?")
	if h.popTitle() != "Keys" || !strings.Contains(h.screen(), "Quit") && !strings.Contains(h.screen(), "quit") {
		t.Errorf("screen:\n%s", h.screen())
	}
	h.press("x")
	if h.m.pop != nil {
		t.Error("any key closes the help")
	}
}

func TestUISettingsArePersisted(t *testing.T) {
	h := newHarness(t, nil)
	h.m.openSettings()
	ids := func() []string { _, i := h.m.settingsItems(); return i }
	idx := func(id string) int {
		for i, x := range ids() {
			if x == id {
				return i
			}
		}
		t.Fatalf("no %q entry", id)
		return -1
	}
	goTo := func(id string) {
		h.m.pop.cur = idx(id)
	}
	ini := func() string { return configText(t, h) }

	goTo("autosync")
	h.press("enter")
	if h.m.cfg("autosync", "1") != "1" && !strings.Contains(ini(), "autosync=") {
		t.Error("autosync was not stored")
	}
	before := h.m.cfg("autosync", "1")
	h.press("enter")
	if h.m.cfg("autosync", "1") == before {
		t.Error("enter must toggle autosync")
	}

	goTo("borders")
	h.press("enter")
	if !strings.Contains(ini(), "tui_ascii=1") || h.m.th.Border[0] != "+" {
		t.Errorf("borders not switched to ASCII:\n%s", ini())
	}
	h.press("enter")
	if !strings.Contains(ini(), "tui_ascii=0") {
		t.Errorf("borders not switched back:\n%s", ini())
	}

	goTo("colors")
	var seen []string
	for i := 0; i < 3; i++ {
		h.press("enter")
		seen = append(seen, h.m.cfg("tui_colors", "auto"))
	}
	if strings.Join(seen, ",") != "256,16,auto" {
		t.Errorf("colors cycle = %v", seen)
	}

	goTo("games")
	h.press("enter")
	if h.popTitle() != "Games folder" {
		t.Fatalf("popup = %q", h.popText())
	}
	for range h.m.pop.text {
		h.press("backspace")
	}
	h.typeText("relative/path")
	h.press("enter")
	if !strings.Contains(h.popText(), "must start with /") || strings.Contains(ini(), "relative/path") {
		t.Errorf("a relative path must be refused: %q", h.popText())
	}
	h.press("esc")
	h.m.openSettings()
	goTo("games")
	h.press("enter")
	for range h.m.pop.text {
		h.press("backspace")
	}
	h.typeText("/media/usb0/games/")
	h.press("enter")
	if h.m.gamesDir() != "/media/usb0/games" || !strings.Contains(ini(), "games_dir=/media/usb0/games\n") {
		t.Errorf("games_dir = %q\n%s", h.m.gamesDir(), ini())
	}

	// the other lines of the file survive every change
	if strings.Count(ini(), "\n") < 3 {
		t.Errorf("config.ini lost lines:\n%s", ini())
	}
}

func TestUISettingsEntersFromTheMenu(t *testing.T) {
	h := newHarness(t, nil)
	for h.m.cur().kind != "settings" {
		h.press("down")
	}
	h.press("enter")
	if h.popTitle() != "Settings" {
		t.Errorf("popup = %q", h.popText())
	}
	h.press("esc")
	for h.m.cur().kind != "server" {
		h.press("up")
	}
	h.press("enter")
	if h.popTitle() != "Server settings" {
		t.Errorf("popup = %q", h.popText())
	}
}

func TestUIPlatformPicker(t *testing.T) {
	rom := newFakeRomm(t)
	rom.addPlatform(1, "gb", "Game Boy", 4)
	rom.addPlatform(2, "snes", "Super Nintendo", 3)
	rom.addPlatform(3, "weird", "Weird Console", 1)
	h := newHarness(t, rom)

	h.press("p")
	if h.popTitle() != "Platforms to index" || len(h.m.pop.chk) != 3 {
		t.Fatalf("popup = %q", h.popText())
	}
	for _, want := range []string{"Game Boy (4 games) -> GAMEBOY", "Super Nintendo (3 games) -> SNES", "Weird Console (1 games) -> ?"} {
		found := false
		for _, c := range h.m.pop.chk {
			found = found || c.label == want
		}
		if !found {
			t.Errorf("no entry %q in %+v", want, h.m.pop.chk)
		}
	}
	h.press("space", "down", "down", "space") // tick the first and the third
	h.press("enter")
	if h.m.syncJob != nil {
		t.Error("the sync should be finished")
	}
	sel := readSelection(h.cfgDir())
	if len(sel) != 2 || sel["1"] == nil || sel["3"] == nil {
		t.Fatalf("selection = %v", sel)
	}
	if len(h.m.data.Games) != 5 {
		t.Errorf("%d games after choosing the platforms, want 5 (the sync starts by itself)", len(h.m.data.Games))
	}

	// unticking a platform drops its catalog but keeps what is on the card
	dir := filepath.Join(h.root, "games", "GAMEBOY")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "Game Boy 000.gb"), []byte("x"), 0o644)
	h.press("p")
	h.press("space") // untick Game Boy
	h.press("enter")
	if exists(catalogPath(h.cfgDir(), "1")) {
		t.Error("the catalog of an unticked platform must be removed")
	}
	if !exists(filepath.Join(dir, "Game Boy 000.gb")) {
		t.Error("the games on the card must stay")
	}
	h.press("esc")
}

func TestUIPlatformPickerErrors(t *testing.T) {
	h := newHarness(t, nil)
	h.press("p")
	if !strings.Contains(h.popText(), "not paired") {
		t.Errorf("popup = %q", h.popText())
	}
	rom := newFakeRomm(t)
	h2 := newHarness(t, rom)
	h2.m.data.Token = "revoked-token-12345"
	h2.press("p")
	if h2.popTitle() != "Platforms" || !strings.Contains(h2.popText(), "401") {
		t.Errorf("popup = %q", h2.popText())
	}
}

func TestUIFirstRunGuidance(t *testing.T) {
	root := t.TempDir()
	m := newModel(loadData(root), newTheme(true, "auto"))
	if m.pop == nil && m.toast == "" {
		t.Error("a new installation should guide the user")
	}
	// nothing to read means an explanation on screen, not a blank panel
	m.pop = nil
	m.w, m.h = 100, 30
	if txt := stripANSI(m.render()); !strings.Contains(txt, "catalog is empty") {
		t.Errorf("empty catalog screen:\n%s", txt)
	}
}

func TestUIUnwritableSettingsFolderIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	root := t.TempDir()
	cfg := filepath.Join(root, "Scripts", ".config", "romm")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(cfg, 0o755)
	m := newModel(loadData(root), newTheme(true, "auto"))
	if m.saveErr == "" || m.pop == nil || !strings.Contains(strings.Join(m.pop.lines, " "), "Cannot save") && !strings.Contains(m.pop.title, "Cannot save") {
		t.Errorf("saveErr=%q pop=%+v", m.saveErr, m.pop)
	}
	m.set("autosync", "0")
	if !strings.Contains(strings.Join(popLines(m), "\n"), "not saved") && m.saveErr == "" {
		t.Error("a failed save must stay visible")
	}
	_ = fmt.Sprint(m)
}

func popLines(m *model) []string {
	if m.pop == nil {
		return nil
	}
	return m.pop.lines
}
