package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDataRoundTrip(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "Scripts", ".config", "romm")
	_ = os.MkdirAll(filepath.Join(cfg, "catalog"), 0o755)
	_ = os.WriteFile(filepath.Join(cfg, "config.ini"), []byte("server_url=http://x\ntoken=secret-token-123\nautosync=0\nweird line without equals\n"), 0o600)
	if err := writeSelection(cfg, map[string]*selPlatform{"2": {Slug: "snes", Name: "Super Nintendo"}, "1": {Slug: "gb", Name: "Game Boy"}}); err != nil {
		t.Fatal(err)
	}
	rows := map[string]catRow{
		"11": {id: "11", name: "b game.gb", size: 2048, sha1: "aa"},
		"10": {id: "10", name: "A game.gb", size: 1024},
	}
	if err := writeCatalog(catalogPath(cfg, "1"), rows); err != nil {
		t.Fatal(err)
	}
	if err := writeCatalog(catalogPath(cfg, "2"), map[string]catRow{"20": {id: "20", name: "Zelda.sfc", size: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(cfg, map[string]*stateRom{"10": {Path: "/media/fat/games/GAMEBOY/A game.gb", Size: 1024}}); err != nil {
		t.Fatal(err)
	}

	d := loadData(root)
	if d.Token != "secret-token-123" || d.Config["token"] != "" {
		t.Errorf("the token must be kept apart from the settings: token=%q config=%v", d.Token, d.Config)
	}
	if d.Config["server_url"] != "http://x" || d.Config["autosync"] != "0" {
		t.Errorf("config = %v", d.Config)
	}
	if len(d.Platforms) != 2 || d.Platforms[0].Name != "Game Boy" || d.Platforms[0].Count != 2 {
		t.Errorf("platforms = %+v", d.Platforms)
	}
	if len(d.Games) != 3 {
		t.Fatalf("%d games", len(d.Games))
	}
	var a *Game
	for _, g := range d.Games {
		if g.Name == "A game.gb" {
			a = g
		}
	}
	if a == nil || !a.Installed || a.Path == "" || a.Low != "a game.gb" || a.Size != 1024 {
		t.Errorf("game = %+v", a)
	}
	if d.Games[0].Name != "A game.gb" {
		t.Errorf("games must be sorted by name, first = %q", d.Games[0].Name)
	}
}

func TestLoadDataToleratesMissingAndCorruptFiles(t *testing.T) {
	root := t.TempDir()
	if d := loadData(root); len(d.Games) != 0 || len(d.Platforms) != 0 || d.Token != "" {
		t.Errorf("an empty card gives an empty catalog: %+v", d)
	}
	cfg := filepath.Join(root, "Scripts", ".config", "romm")
	_ = os.MkdirAll(filepath.Join(cfg, "catalog"), 0o755)
	for name, content := range map[string]string{
		"selection.json": "{not json", "state.json": "\x00\x01garbage", "mapping.json": "[1,2,3]", "config.ini": "\xff\xfe",
	} {
		_ = os.WriteFile(filepath.Join(cfg, name), []byte(content), 0o644)
	}
	_ = os.WriteFile(filepath.Join(cfg, "catalog", "1.tsv"), []byte("garbage\n"), 0o644)
	d := loadData(root) // must not panic
	if len(d.Games) != 0 {
		t.Errorf("%d games from garbage", len(d.Games))
	}
	if len(readSelection(cfg)) != 0 || len(readState(cfg)) != 0 || len(loadMapping(cfg)) != 0 {
		t.Error("corrupt files must read as empty")
	}
}

func TestStateFilesRoundTrip(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "romm")
	sel := map[string]*selPlatform{"1": {Slug: "gb", Name: "Game Boy", SyncedAt: "2026-01-01T00:00:00+00:00"}}
	if err := writeSelection(cfg, sel); err != nil {
		t.Fatal(err)
	}
	if got := readSelection(cfg); got["1"] == nil || got["1"].SyncedAt != sel["1"].SyncedAt {
		t.Errorf("selection = %+v", got)
	}
	st := map[string]*stateRom{"7": {Path: "/g/x.gb", Size: 5, Reasons: []string{"rom"}}}
	if err := writeState(cfg, st); err != nil {
		t.Fatal(err)
	}
	if got := readState(cfg); got["7"] == nil || got["7"].Path != "/g/x.gb" || got["7"].Reasons[0] != "rom" {
		t.Errorf("state = %+v", got)
	}
	if err := saveMapping(cfg, map[string]string{"gb": "GAMEBOY"}); err != nil {
		t.Fatal(err)
	}
	if loadMapping(cfg)["gb"] != "GAMEBOY" {
		t.Error("mapping not saved")
	}
	if ents, _ := filepath.Glob(filepath.Join(cfg, "*.tmp")); len(ents) != 0 {
		t.Errorf("temporary files left: %v", ents)
	}
}

func TestWriteFileAtomicReplacesWholeFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "file.json")
	for _, content := range []string{"first", "second, longer than the first"} {
		if err := writeFileAtomic(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if string(readFile(t, p)) != content {
			t.Errorf("content = %q", readFile(t, p))
		}
	}
}

func TestFreeSpace(t *testing.T) {
	dir := t.TempDir()
	free, ok := freeSpace(dir)
	if !ok || free <= 0 {
		t.Fatalf("free space of an existing folder: %d %v", free, ok)
	}
	// a games folder that does not exist yet is measured on the nearest existing parent
	if f2, ok := freeSpace(filepath.Join(dir, "games", "GAMEBOY")); !ok || f2 <= 0 {
		t.Errorf("nearest parent: %d %v", f2, ok)
	}
	if _, ok := freeSpace(""); ok {
		t.Error("an empty path has no free space")
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0B", 1023: "1023B", 1024: "1KB", 1536: "2KB", 1 << 20: "1.0MB", 5<<20 + 512<<10: "5.5MB", 1 << 30: "1.0GB", 3 << 40: "3072.0GB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestResolveFolderRules(t *testing.T) {
	games := t.TempDir()
	for _, d := range []string{"snes", "Custom Dir", "Weird Console"} {
		_ = os.MkdirAll(filepath.Join(games, d), 0o755)
	}
	e := env{gamesDir: games, mapping: map[string]string{}}
	cases := []struct {
		slug, name, mapped, want string
	}{
		{"snes", "Super Nintendo", "", "snes"},            // default SNES matches the existing folder, ignoring case
		{"gb", "Game Boy", "", "GAMEBOY"},                 // default name, folder created later
		{"gb", "Game Boy", "Custom Dir", "Custom Dir"},    // the user's mapping wins
		{"gb", "Game Boy", "../escape", ""},               // an unsafe mapping is ignored: the platform stays unmapped
		{"gb", "Game Boy", "a/b", ""},                     // a path is not a folder name
		{"unknown", "Weird Console", "", "Weird Console"}, // falls back to the platform name
		{"unknown", "Nothing Like It", "", ""},            // nothing known
		{"genesis-slash-megadrive", "Mega Drive", "", "MegaDrive"},
	}
	for _, c := range cases {
		e.mapping = map[string]string{}
		if c.mapped != "" {
			e.mapping[c.slug] = c.mapped
		}
		if got := resolveFolder(e, c.slug, c.name); got != c.want {
			t.Errorf("resolveFolder(%q, %q, mapped %q) = %q, want %q", c.slug, c.name, c.mapped, got, c.want)
		}
	}
}

func TestCheckStorage(t *testing.T) {
	if err := checkStorage(filepath.Join(t.TempDir(), "new", "dir")); err != nil {
		t.Errorf("a new folder must be created: %v", err)
	}
	file := filepath.Join(t.TempDir(), "plain")
	_ = os.WriteFile(file, nil, 0o644)
	if err := checkStorage(filepath.Join(file, "sub")); err == nil {
		t.Error("a folder under a regular file cannot work")
	}
	if os.Geteuid() != 0 {
		ro := filepath.Join(t.TempDir(), "ro")
		_ = os.MkdirAll(ro, 0o755)
		_ = os.Chmod(ro, 0o555)
		defer os.Chmod(ro, 0o755)
		if err := checkStorage(ro); err == nil {
			t.Error("a read-only folder must be reported")
		}
	}
}

func TestLogErrorStaysSmall(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 600; i++ {
		logError(dir, strings.Repeat("x", 60))
	}
	b := readFile(t, filepath.Join(dir, "last-error.log"))
	if n := strings.Count(string(b), "\n"); n > 400 || len(b) > 40000 {
		t.Errorf("last-error.log grew to %d lines / %d bytes", n, len(b))
	}
	if st, _ := os.Stat(filepath.Join(dir, "last-error.log")); st.Mode().Perm() != 0o600 && st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
}

func TestExplainErrAddsTheLikelyCause(t *testing.T) {
	cases := []struct {
		err     string
		clockOK bool
		want    string
	}{
		{"x509: certificate has expired or is not yet valid", false, "clock of this device is wrong"},
		{"x509: certificate signed by unknown authority", true, "Verify TLS"},
		{"dial tcp: lookup romm.lan: no such host", true, "cannot resolve"},
		{"connection refused", true, ""},
	}
	for _, c := range cases {
		got := explainErr(errString(c.err), c.clockOK)
		if !strings.HasPrefix(got, c.err) || (c.want != "" && !strings.Contains(got, c.want)) || (c.want == "" && got != c.err) {
			t.Errorf("explainErr(%q) = %q", c.err, got)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestDetailExtractsTheServerMessage(t *testing.T) {
	cases := map[string]string{
		`{"detail":"nope"}`:    "nope",
		`{"detail":["a","b"]}`: "",
		`{"other":1}`:          "",
		`not json at all`:      "not json at all",
		``:                     "",
		`{"detail":"` + strings.Repeat("x", 500) + `"}`: "",
	}
	for in, want := range cases {
		got := detail(in)
		if want != "" && got != want {
			t.Errorf("detail(%.30q) = %q, want %q", in, got, want)
		}
		if len([]rune(got)) > 303 {
			t.Errorf("detail(%.30q) is %d characters long", in, len(got))
		}
	}
}
