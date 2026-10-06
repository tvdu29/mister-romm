package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"https://romm.lan/":           "https://romm.lan",
		" http://192.168.1.5:8080// ": "http://192.168.1.5:8080",
		"romm.lan":                    "https://romm.lan",
	}
	for in, want := range cases {
		if got, ok := normalizeURL(in); !ok || got != want {
			t.Errorf("normalizeURL(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "ro mm"} {
		if _, ok := normalizeURL(bad); ok {
			t.Errorf("normalizeURL(%q) should be refused", bad)
		}
	}
}

func TestCatalogRoundTripDropsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog", "1.tsv")
	rows := map[string]catRow{
		"2": {id: "2", name: "beta (USA).sfc", size: 20, sha1: "bb"},
		"1": {id: "1", name: "Alpha (USA).sfc", size: 10},
	}
	if err := writeCatalog(path, rows); err != nil {
		t.Fatal(err)
	}
	// stray screen output written by an old version must be ignored when reading
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("\x1b[H\x1b[2J Indexing...\nnot-a-number\x1fx\x1f1\x1f\n")
	f.Close()
	got := readCatalog(path)
	if len(got) != 2 || got["1"].name != "Alpha (USA).sfc" || got["2"].sha1 != "bb" {
		t.Fatalf("unexpected catalog: %#v", got)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "1\x1fAlpha") { // sorted by name, case-insensitive
		t.Errorf("catalog not sorted by name: %q", string(b))
	}
}

func TestSetConfigKeepsOtherLinesAndIsPrivate(t *testing.T) {
	root := t.TempDir()
	path := cfgPath(root)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("server_url=http://a\ntoken=secret\n"), 0o644)
	if err := setConfig(root, "server_url", "http://b"); err != nil {
		t.Fatal(err)
	}
	if err := setConfig(root, "autosync", "0"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if got := string(b); got != "server_url=http://b\ntoken=secret\nautosync=0\n" {
		t.Errorf("config.ini = %q", got)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("config.ini permissions = %v, want 0600", st.Mode().Perm())
	}
}

func TestResolveFolder(t *testing.T) {
	games := t.TempDir()
	_ = os.MkdirAll(filepath.Join(games, "SNES"), 0o755)
	_ = os.MkdirAll(filepath.Join(games, "GAMEBOY"), 0o755)
	e := env{gamesDir: games, mapping: map[string]string{"weirdbox": "BOX"}}
	for slug, want := range map[string]string{"snes": "SNES", "gb": "GAMEBOY", "weirdbox": "BOX", "nes": "NES", "zzz": ""} {
		if got := resolveFolder(e, slug, "Unknown"); got != want {
			t.Errorf("resolveFolder(%q) = %q, want %q", slug, got, want)
		}
	}
	if got := resolveFolder(env{gamesDir: games, mapping: map[string]string{}}, "x", "snes"); got != "SNES" {
		t.Errorf("a platform named like an existing folder should match it, got %q", got)
	}
}

// romServer serves one game with Range support and counts the bytes it sent.
func romServer(t *testing.T, data []byte, sent *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := 0
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(len(data)-1)+"/"+strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusPartialContent)
		}
		n, _ := w.Write(data[start:])
		*sent += n
	}))
}

func sha(b []byte) string { h := sha1.Sum(b); return hex.EncodeToString(h[:]) }

func TestDownloadResumesAndVerifies(t *testing.T) {
	data := []byte(strings.Repeat("0123456789abcdef", 4096)) // 64 KiB
	sent := 0
	srv := romServer(t, data, &sent)
	defer srv.Close()
	dir := t.TempDir()
	e := env{base: srv.URL, client: srv.Client(), longClient: srv.Client(), cfgDir: dir}
	it := dlItem{id: "7", name: "Game (USA).gb", dest: filepath.Join(dir, "GAMEBOY", "Game (USA).gb"), size: int64(len(data)), sha1: sha(data)}

	// a partial file left by an interrupted download is resumed, not restarted
	_ = os.MkdirAll(filepath.Dir(it.dest), 0o755)
	_ = os.WriteFile(it.dest+".part", data[:30000], 0o644)
	if err := downloadOne(context.Background(), e, it, func(dlProgress) {}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(it.dest)
	if sha(got) != sha(data) {
		t.Fatal("downloaded file differs from the original")
	}
	if sent != len(data)-30000 {
		t.Errorf("server sent %d bytes, want only the missing %d", sent, len(data)-30000)
	}
	if _, err := os.Stat(it.dest + ".part"); err == nil {
		t.Error(".part file should be gone")
	}
}

func TestDownloadRejectsWrongHash(t *testing.T) {
	data := []byte(strings.Repeat("x", 5000))
	sent := 0
	srv := romServer(t, data, &sent)
	defer srv.Close()
	dir := t.TempDir()
	e := env{base: srv.URL, client: srv.Client(), longClient: srv.Client(), cfgDir: dir}
	it := dlItem{id: "1", name: "Bad.gb", dest: filepath.Join(dir, "Bad.gb"), size: int64(len(data)), sha1: strings.Repeat("0", 40)}
	if err := downloadOne(context.Background(), e, it, func(dlProgress) {}); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("expected a hash error, got %v", err)
	}
	for _, f := range []string{it.dest, it.dest + ".part"} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s must not be kept after a hash mismatch", f)
		}
	}
	// archives are hashed by RomM on their members, so the hash is not compared
	it2 := dlItem{id: "2", name: "Pack.zip", dest: filepath.Join(dir, "Pack.zip"), size: int64(len(data)), sha1: strings.Repeat("0", 40)}
	if err := downloadOne(context.Background(), e, it2, func(dlProgress) {}); err != nil {
		t.Errorf("archive should not be hash-checked: %v", err)
	}
}

func TestDetailAndArchive(t *testing.T) {
	if got := detail(`{"detail":"authorization_pending"}`); got != "authorization_pending" {
		t.Errorf("detail = %q", got)
	}
	if !isArchive("A.ZIP") || isArchive("A.gb") {
		t.Error("isArchive misclassifies")
	}
}
