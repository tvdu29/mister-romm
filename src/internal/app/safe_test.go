package app

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "../x", "a\x1b[2J", strings.Repeat("a", 256)} {
		if safeName(bad) {
			t.Errorf("%q must be refused", bad)
		}
	}
	for _, ok := range []string{"Game (USA).gb", "Pokémon Rouge.gb", "..hidden"} {
		if !safeName(ok) {
			t.Errorf("%q must be accepted", ok)
		}
	}
}

func TestCleanStripsControlChars(t *testing.T) {
	if got := clean("a\x1b[31mb\nc"); strings.ContainsAny(got, "\x1b\n") {
		t.Errorf("control characters left in %q", got)
	}
}

func TestCheckCredentials(t *testing.T) {
	if err := checkCredentials("abcdefgh12345", "dev-1", []string{"roms.read"}); err != nil {
		t.Error(err)
	}
	if checkCredentials("abc\nx=1", "dev", nil) == nil {
		t.Error("token with newline accepted")
	}
	if checkCredentials("abcdefgh12345", "dev\n", nil) == nil {
		t.Error("device id with newline accepted")
	}
}

func TestRemoveGameFileConfinement(t *testing.T) {
	games := filepath.Join(t.TempDir(), "games")
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.MkdirAll(filepath.Join(games, "SNES"), 0o755)
	_ = os.WriteFile(outside, []byte("x"), 0o644)

	if err := removeGameFile(games, outside); err == nil {
		t.Error("deleting outside the games folder must fail")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("outside file was deleted")
	}
	// a symlinked folder pointing outside must not be followed
	_ = os.Symlink(filepath.Dir(outside), filepath.Join(games, "LINK"))
	if err := removeGameFile(games, filepath.Join(games, "LINK", "secret")); err == nil {
		t.Error("symlinked folder escaped the games folder")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("file deleted through a symlink")
	}
	rom := filepath.Join(games, "SNES", "a.sfc")
	_ = os.WriteFile(rom, []byte("x"), 0o644)
	if err := removeGameFile(games, rom); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rom); err == nil {
		t.Error("game not deleted")
	}
}

func TestSetConfigRejectsNewlines(t *testing.T) {
	dir := t.TempDir()
	if err := setConfig(dir, "token", "a\nroot=/"); err == nil {
		t.Error("newline in value accepted")
	}
}

func TestRedirectPolicy(t *testing.T) {
	first, _ := http.NewRequest("GET", "https://romm.example/api", nil)
	same, _ := http.NewRequest("GET", "https://romm.example/other", nil)
	other := &http.Request{URL: &url.URL{Scheme: "https", Host: "evil.example"}}
	down := &http.Request{URL: &url.URL{Scheme: "http", Host: "romm.example"}}
	if sameHostRedirect(same, []*http.Request{first}) != nil {
		t.Error("same-host redirect refused")
	}
	if sameHostRedirect(other, []*http.Request{first}) == nil || sameHostRedirect(down, []*http.Request{first}) == nil {
		t.Error("cross-host or downgrade redirect accepted")
	}
}
