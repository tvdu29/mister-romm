package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"v1.2.3", "v1.2.4", true},
		{"v1.2.3", "v1.3.0", true},
		{"v1.9.0", "v1.10.0", true}, // numeric, not alphabetical
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.2.2", false},
		{"v2.0.0", "v1.9.9", false},
		{"dev", "v9.9.9", false},
		{"dev-abc1234", "v9.9.9", false},
		{"v1.0.0", "garbage", false},
		{"v1.0.0", "v1.0.1-rc1", true},
	}
	for _, c := range cases {
		if got := newerVersion(c.cur, c.latest); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.cur, c.latest, got, c.want)
		}
	}
}

func TestParseSums(t *testing.T) {
	h := strings.Repeat("ab", 32)
	notHex := strings.Repeat("ab", 31) + "zz"
	got := parseSums(h + "  romm-tui-linux-armv7\n" + strings.ToUpper(h) + " *other\nnot a sum line\nshort  x\n" + notHex + "  bad\n")
	if got["romm-tui-linux-armv7"] != h || got["other"] != h || len(got) != 2 {
		t.Fatalf("parseSums = %v", got)
	}
}

func TestInstallBinaryKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	src, dest := filepath.Join(dir, "src"), filepath.Join(dir, "romm-tui")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(src, dest); err != nil {
		t.Fatal(err)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if read(dest) != "new" || read(dest+".old") != "old" {
		t.Fatalf("dest=%q old=%q", read(dest), read(dest+".old"))
	}
	if _, err := os.Stat(dest + ".new"); err == nil {
		t.Fatal("the staging file was left behind")
	}
	if st, _ := os.Stat(dest); st.Mode().Perm()&0o100 == 0 {
		t.Fatal("the installed program is not executable")
	}
}

func TestFetchReleaseAndDownload(t *testing.T) {
	payload := []byte("binary-content")
	sum := sha256.Sum256(payload)
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":"romm-tui-linux-armv7","browser_download_url":%q,"size":%d}]}`,
			srv.URL+"/bin", len(payload))
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv = httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	rel, err := fetchRelease(ctx, srv.Client(), srv.URL+"/latest")
	if err != nil || rel.Tag != "v1.2.3" {
		t.Fatalf("fetchRelease = %+v, %v", rel, err)
	}
	if _, err := fetchRelease(ctx, srv.Client(), srv.URL+"/missing"); err == nil {
		t.Fatal("a 404 must be an error")
	}
	a, ok := rel.asset("romm-tui-linux-armv7")
	if !ok {
		t.Fatal("asset not found")
	}
	f, err := os.CreateTemp(t.TempDir(), "dl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := downloadTo(ctx, srv.Client(), a, f, func(done, total int64) {})
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("downloadTo = %q, %v", got, err)
	}
	a.Size = int64(len(payload)) - 1 // the server sends more than announced
	if _, err := downloadTo(ctx, srv.Client(), a, f, func(done, total int64) {}); err == nil {
		t.Fatal("an overflow must be an error")
	}
}

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

func TestUpdateTargetPrefersTheInstalledProgram(t *testing.T) {
	root := t.TempDir()
	p := installedBinary(root)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("x"), 0o755)
	if got, err := updateTarget(root); err != nil || got != p {
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
