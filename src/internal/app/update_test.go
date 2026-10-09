package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestIsNewer(t *testing.T) {
	for _, c := range []struct {
		latest, cur string
		want        bool
	}{
		{"v1.0.2", "v1.0.1", true},
		{"v1.10.0", "v1.9.9", true},
		{"v2.0.0-rc1", "v1.9.0", true},
		{"v1.0.1", "v1.0.1", false},
		{"v1.0.0", "v1.0.1", false},
		{"v1.0.1", "dev", false},
		{"v1.0.1", "dev-abc123", false},
		{"garbage", "v1.0.0", false},
	} {
		if got := isNewer(c.latest, c.cur); got != c.want {
			t.Errorf("isNewer(%q, %q) = %v", c.latest, c.cur, got)
		}
	}
}

func TestSumFor(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("B", 64)
	sums := []byte(a + "  mister-romm-v1-sdcard.zip\n" + b + " *romm-tui-linux-armv7\nshort  romm-tui-linux-amd64\n")
	if got := sumFor(sums, "romm-tui-linux-armv7"); got != strings.ToLower(b) {
		t.Errorf("got %q", got)
	}
	if got := sumFor(sums, "romm-tui-linux-amd64"); got != "" {
		t.Error("a malformed line must not be used")
	}
}

// fakeELF is the start of a Linux program for this machine.
func fakeELF(extra string) []byte {
	machine := map[string]byte{"arm": 40, "amd64": 62, "arm64": 183, "386": 3}[runtime.GOARCH]
	h := make([]byte, 64)
	copy(h, "\x7fELF")
	h[18] = machine
	return append(h, extra...)
}

func TestCheckELF(t *testing.T) {
	if err := checkELF(fakeELF("x")); err != nil {
		t.Error(err)
	}
	if checkELF([]byte("<html>not found</html>........")) == nil {
		t.Error("an HTML page must be refused")
	}
	other := fakeELF("x")
	other[18] = 99
	if checkELF(other) == nil {
		t.Error("a program for another processor must be refused")
	}
}

// fakeGitHub serves a release whose binary is bin; sums may be altered to simulate damage.
func fakeGitHub(t *testing.T, tag string, bin []byte, sum string) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + updateRepo + "/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []map[string]any{
				{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/dl/SHA256SUMS", "size": 100},
				{"name": releaseAsset(), "browser_download_url": srv.URL + "/dl/bin", "size": len(bin)},
			}})
		case "/dl/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", sum, releaseAsset())
		case "/dl/bin":
			_, _ = w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

func runUpdateForTest(t *testing.T, srv *httptest.Server, target string) string {
	old := githubAPI
	githubAPI = srv.URL
	defer func() { githubAPI = old }()
	r, err := fetchLatestRelease(context.Background(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	runUpdate(context.Background(), srv.Client(), r, target, func(msg tea.Msg) {
		if l, ok := msg.(toolLine); ok {
			out = append(out, l.text)
		}
	})
	return strings.Join(out, "\n")
}

func TestUpdateInstallsAndKeepsThePreviousVersion(t *testing.T) {
	if releaseAsset() == "" {
		t.Skip("no release for this machine")
	}
	bin := fakeELF("new version")
	sum := sha256.Sum256(bin)
	srv := fakeGitHub(t, "v9.0.0", bin, hex.EncodeToString(sum[:]))
	defer srv.Close()
	target := filepath.Join(t.TempDir(), "romm-tui")
	_ = os.WriteFile(target, []byte("old version"), 0o755)

	out := runUpdateForTest(t, srv, target)
	if !strings.Contains(out, "Installed v9.0.0") {
		t.Fatalf("update failed:\n%s", out)
	}
	if got, _ := os.ReadFile(target); string(got) != string(bin) {
		t.Error("the program was not replaced")
	}
	if st, _ := os.Stat(target); st.Mode().Perm()&0o100 == 0 {
		t.Error("the new program must be executable")
	}
	if got, _ := os.ReadFile(target + ".prev"); string(got) != "old version" {
		t.Error("the previous version must be kept as .prev")
	}
	if _, err := os.Stat(target + ".new"); err == nil {
		t.Error("no .new file must be left behind")
	}
}

func TestUpdateRefusesADamagedDownload(t *testing.T) {
	if releaseAsset() == "" {
		t.Skip("no release for this machine")
	}
	srv := fakeGitHub(t, "v9.0.0", fakeELF("tampered"), strings.Repeat("0", 64))
	defer srv.Close()
	target := filepath.Join(t.TempDir(), "romm-tui")
	_ = os.WriteFile(target, []byte("old version"), 0o755)
	out := runUpdateForTest(t, srv, target)
	if !strings.Contains(out, "SHA-256 mismatch") || !strings.Contains(out, "Nothing was changed") {
		t.Errorf("expected a refusal:\n%s", out)
	}
	if got, _ := os.ReadFile(target); string(got) != "old version" {
		t.Error("the program must not be touched after a failed check")
	}
}

func TestUpdateTargetPrefersTheInstalledProgram(t *testing.T) {
	cfg := t.TempDir()
	p := filepath.Join(cfg, "bin", "romm-tui")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("x"), 0o755)
	if got, err := updateTarget(cfg); err != nil || got != p {
		t.Errorf("updateTarget = %q, %v; want %q", got, err, p)
	}
}

func TestGithubRedirectsStayOnGitHub(t *testing.T) {
	for host, ok := range map[string]bool{
		"github.com": true, "objects.githubusercontent.com": true, "release-assets.githubusercontent.com": true,
		"evil.com": false, "githubusercontent.com.evil.com": false, "notgithub.com": false,
	} {
		if githubHost(host) != ok {
			t.Errorf("githubHost(%q) = %v", host, !ok)
		}
	}
}
