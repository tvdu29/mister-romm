package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The update tests build the real program (with a chosen version) and serve it from a fake
// GitHub, so the whole chain is exercised: lookup, download, checksum, ELF check, the -version
// run, installation and the hand-over to the new copy.

var (
	binDir   string
	binMu    sync.Mutex
	binCache = map[string][]byte{}
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "romm-test-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// builtProgram compiles cmd/romm-tui for this machine, reporting the given version.
func builtProgram(t *testing.T, tag string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the program: skipped with -short")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go tool is not in PATH")
	}
	binMu.Lock()
	defer binMu.Unlock()
	if b, ok := binCache[tag]; ok {
		return b
	}
	out := filepath.Join(binDir, "romm-"+tag)
	cmd := exec.Command(goBin, "build", "-trimpath", "-ldflags", "-X main.version="+tag, "-o", out, "../../cmd/romm-tui")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the program: %v\n%s", err, msg)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	binCache[tag] = b
	return b
}

type ghOpts struct {
	tag      string
	bin      []byte
	sums     string // overrides the hash written in SHA256SUMS
	noSumFor bool   // SHA256SUMS does not list the file
	cutAfter int    // the binary download is cut after that many bytes
	declared int64  // size announced by the release (default: the real one)
}

// fakeGitHub serves /latest, the binary and SHA256SUMS. The URLs it hands out point to itself.
func fakeGitHub(t *testing.T, o ghOpts) (*httptest.Server, release) {
	t.Helper()
	asset := binaryAsset()
	sum := sha256.Sum256(o.bin)
	hash := hex.EncodeToString(sum[:])
	if o.sums != "" {
		hash = o.sums
	}
	mux := http.NewServeMux()
	var srv *httptest.Server
	declared := o.declared
	if declared == 0 {
		declared = int64(len(o.bin))
	}
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q,"size":%d},{"name":"SHA256SUMS","browser_download_url":%q,"size":100}]}`,
			o.tag, asset, srv.URL+"/bin", declared, srv.URL+"/sums")
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
		if o.cutAfter > 0 {
			w.Header().Set("Content-Length", fmt.Sprint(len(o.bin)))
			_, _ = w.Write(o.bin[:o.cutAfter])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write(o.bin)
	})
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		if !o.noSumFor {
			fmt.Fprintf(w, "%s  %s\n", hash, asset)
		}
		fmt.Fprintf(w, "%s  other-file\n", strings.Repeat("0", 64))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rel, err := fetchRelease(context.Background(), srv.Client(), srv.URL+"/latest")
	if err != nil {
		t.Fatal(err)
	}
	return srv, rel
}

// installed makes a fake installed program and returns its path.
func installed(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bin", "romm-tui")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func leftovers() int {
	m, _ := filepath.Glob(filepath.Join(os.TempDir(), tmpBinPrefix+"upd.*"))
	return len(m)
}

func runUpd(t *testing.T, srv *httptest.Server, rel release, dest string) (staged string, lines []string, err error) {
	t.Helper()
	var progress []string
	staged, err = runUpdate(context.Background(), srv.Client(), rel, dest,
		func(s string) { lines = append(lines, s) }, func(s string) { progress = append(progress, s) })
	if err == nil && len(progress) == 0 {
		t.Error("no download progress was reported")
	}
	return
}

func TestUpdateEndToEnd(t *testing.T) {
	bin := builtProgram(t, "v9.9.9")
	srv, rel := fakeGitHub(t, ghOpts{tag: "v9.9.9", bin: bin})
	dest := installed(t, "old-binary")
	before := leftovers()

	staged, lines, err := runUpd(t, srv, rel, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(staged)
	if !bytes.Equal(readFile(t, dest), bin) {
		t.Error("the installed program is not the downloaded one")
	}
	if string(readFile(t, dest+".old")) != "old-binary" {
		t.Error("the previous program was not kept as .old")
	}
	if st, _ := os.Stat(dest); st.Mode().Perm()&0o100 == 0 {
		t.Error("the installed program is not executable")
	}
	if exists(dest + ".new") {
		t.Error("the staging file was left behind")
	}
	out, err := exec.Command(dest, "-version").Output()
	if err != nil || !strings.Contains(string(out), "v9.9.9") {
		t.Errorf("the installed program says %q (%v)", out, err)
	}
	if staged == "" || !strings.HasPrefix(filepath.Base(staged), tmpBinPrefix) || leftovers() != before+1 {
		t.Errorf("staged copy = %q, leftovers %d -> %d", staged, before, leftovers())
	}
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "Installed v9.9.9") {
		t.Errorf("lines = %q", lines)
	}
}

func TestUpdateRefusals(t *testing.T) {
	good := builtProgram(t, "v9.9.9")
	other := builtProgram(t, "v9.9.8")
	wrongArch := fakeELFProgram(99)
	html := []byte("<html>rate limited</html>" + strings.Repeat(" ", 200))

	cases := []struct {
		name string
		o    ghOpts
		want string
	}{
		{"wrong checksum", ghOpts{tag: "v9.9.9", bin: good, sums: strings.Repeat("ab", 32)}, "checksum"},
		{"not listed in SHA256SUMS", ghOpts{tag: "v9.9.9", bin: good, noSumFor: true}, "no entry"},
		{"not a program", ghOpts{tag: "v9.9.9", bin: html}, "not a Linux program"},
		{"another processor", ghOpts{tag: "v9.9.9", bin: wrongArch}, "another processor"},
		{"reports another version", ghOpts{tag: "v9.9.9", bin: other}, "reports"},
		{"connection cut", ghOpts{tag: "v9.9.9", bin: good, cutAfter: 4096}, ""},
		{"absurd announced size", ghOpts{tag: "v9.9.9", bin: good, declared: maxBinary + 1}, "unexpected size"},
		{"announces more than it has", ghOpts{tag: "v9.9.9", bin: good, declared: int64(len(good)) + 10}, "incomplete"},
		{"announces less than it sends", ghOpts{tag: "v9.9.9", bin: good, declared: int64(len(good)) - 10}, "more data"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, rel := fakeGitHub(t, c.o)
			dest := installed(t, "old-binary")
			before := leftovers()
			staged, _, err := runUpd(t, srv, rel, dest)
			if err == nil {
				os.Remove(staged)
				t.Fatal("the update should have been refused")
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
			if string(readFile(t, dest)) != "old-binary" {
				t.Error("the installed program was modified")
			}
			if exists(dest+".old") || exists(dest+".new") {
				t.Error("a backup or staging file was left next to the program")
			}
			if staged != "" || leftovers() != before {
				t.Errorf("staged = %q, temporary copies %d -> %d", staged, before, leftovers())
			}
		})
	}
}

// fakeELFProgram is a minimal ELF header for the given machine, followed by padding.
func fakeELFProgram(machine byte) []byte {
	h := make([]byte, 4096)
	copy(h, "\x7fELF")
	h[18] = machine
	return h
}

func TestUpdateCannotInstallIntoAReadOnlyFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	bin := builtProgram(t, "v9.9.9")
	srv, rel := fakeGitHub(t, ghOpts{tag: "v9.9.9", bin: bin})
	dest := installed(t, "old-binary")
	if err := os.Chmod(filepath.Dir(dest), 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(dest), 0o755)
	before := leftovers()
	staged, _, err := runUpd(t, srv, rel, dest)
	if err == nil {
		os.Remove(staged)
		t.Fatal("expected an error")
	}
	if string(readFile(t, dest)) != "old-binary" {
		t.Error("the installed program was modified")
	}
	if staged != "" || leftovers() != before {
		t.Error("temporary copy left behind")
	}
}

func TestUpdateCancelled(t *testing.T) {
	bin := builtProgram(t, "v9.9.9")
	srv, rel := fakeGitHub(t, ghOpts{tag: "v9.9.9", bin: bin})
	dest := installed(t, "old-binary")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := leftovers()
	staged, err := runUpdate(ctx, srv.Client(), rel, dest, func(string) {}, func(string) {})
	if err == nil || staged != "" || leftovers() != before {
		t.Errorf("staged=%q err=%v", staged, err)
	}
	if string(readFile(t, dest)) != "old-binary" {
		t.Error("the installed program was modified")
	}
}

// ------------------------------------------------------------------ GitHub client

func TestGitHubClientFollowsNoRedirectOutsideGitHub(t *testing.T) {
	var gotAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("secret place"))
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()
	h := newHarness(t, nil)
	h.m.data.Token = "romm-token-must-not-leak"
	_, err := ghGet(context.Background(), h.m.githubClient(), redirector.URL)
	if err == nil || !strings.Contains(err.Error(), "refusing a redirect") {
		t.Fatalf("err = %v", err)
	}
	if gotAuth != "" {
		t.Errorf("the redirect target received credentials: %q", gotAuth)
	}
}

func TestGitHubClientNeverSkipsCertificateChecks(t *testing.T) {
	tlsSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) }))
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake is expected
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	h := newHarness(t, nil)
	// the RomM server may be reached without certificate verification (clock not set, self-signed)...
	h.m.sessionInsecure = true
	h.m.data.Config["verify_tls"] = "0"
	resp, err := ghGet(context.Background(), h.m.env().client, tlsSrv.URL)
	if err != nil {
		t.Fatalf("the RomM client should accept this server in insecure mode: %v", err)
	}
	resp.Body.Close()
	// ...but a program to be run is never downloaded that way
	if _, err := ghGet(context.Background(), h.m.githubClient(), tlsSrv.URL); err == nil {
		t.Fatal("the update client accepted an unverified certificate")
	}
}

func TestGitHubRateLimitIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "rate limit", http.StatusForbidden) }))
	defer srv.Close()
	_, err := fetchRelease(context.Background(), srv.Client(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("err = %v", err)
	}
}

// ------------------------------------------------------------------ through the interface

func withVersion(t *testing.T, v string) {
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

func withReleaseURL(t *testing.T, u string) {
	old := releaseURL
	releaseURL = u
	t.Cleanup(func() { releaseURL = old })
}

// updateHarness has an installed program on the card and a fake GitHub with the given release.
func updateHarness(t *testing.T, cur string, o ghOpts) (*harness, string) {
	t.Helper()
	srv, _ := fakeGitHub(t, o)
	withReleaseURL(t, srv.URL+"/latest")
	withVersion(t, cur)
	h := newHarness(t, nil)
	dest := installedBinary(h.root)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return h, dest
}

func TestUIUpdateFullFlow(t *testing.T) {
	bin := builtProgram(t, "v9.9.9")
	h, dest := updateHarness(t, "v1.0.0", ghOpts{tag: "v9.9.9", bin: bin})
	h.m.openSettings()
	if !strings.Contains(strings.Join(settingsLabels(h.m), "\n"), "Check for updates (installed: v1.0.0)") {
		t.Error("the Settings window has no update entry")
	}
	h.m.pop = nil

	h.pump(h.m.openUpdate())
	if h.popTitle() != "Update available" {
		t.Fatalf("popup = %q", h.popText())
	}
	txt := h.popText()
	for _, want := range []string{"v1.0.0  ->  v9.9.9", "romm-tui.old", "Free space: card"} {
		if !strings.Contains(txt, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, txt)
		}
	}
	before := leftovers()
	h.press("y")
	defer os.Remove(h.m.relaunch)
	if !h.quit || h.m.relaunch == "" {
		t.Fatalf("the program should leave to restart into the new one (quit=%v relaunch=%q)", h.quit, h.m.relaunch)
	}
	if !bytes.Equal(readFile(t, dest), bin) || string(readFile(t, dest+".old")) != "old-binary" {
		t.Error("installation is wrong")
	}
	if leftovers() != before+1 {
		t.Error("exactly one staged copy is expected")
	}
}

func settingsLabels(m *model) []string { l, _ := m.settingsItems(); return l }

func TestUIUpdateAlreadyUpToDate(t *testing.T) {
	bin := builtProgram(t, "v9.9.9")
	h, dest := updateHarness(t, "v9.9.9", ghOpts{tag: "v9.9.9", bin: bin})
	h.pump(h.m.openUpdate())
	if h.popTitle() == "Update available" || !strings.Contains(h.popText(), "up to date") {
		t.Errorf("popup = %q", h.popText())
	}
	if string(readFile(t, dest)) != "old-binary" {
		t.Error("nothing should be installed")
	}
}

func TestUIUpdateOffersTheReleaseToADevelopmentBuild(t *testing.T) {
	bin := builtProgram(t, "v9.9.9")
	h, _ := updateHarness(t, "dev", ghOpts{tag: "v9.9.9", bin: bin})
	h.pump(h.m.openUpdate())
	if h.popTitle() != "Update available" || !strings.Contains(h.popText(), "development build") {
		t.Errorf("popup = %q", h.popText())
	}
}

func TestUIUpdateRefusals(t *testing.T) {
	bin := builtProgram(t, "v9.9.9")
	h, _ := updateHarness(t, "v1.0.0", ghOpts{tag: "v9.9.9", bin: bin})

	h.m.syncJob = &job{cancel: func() {}}
	h.pump(h.m.openUpdate())
	if !strings.Contains(h.popText(), "sync or a download is running") {
		t.Errorf("popup = %q", h.popText())
	}
	h.m.syncJob = nil

	h.m.toolJob = &job{cancel: func() {}}
	h.pump(h.m.openUpdate())
	if h.m.toast != "Another test is already running" {
		t.Errorf("toast = %q", h.m.toast)
	}
	h.m.toolJob = nil

	// no installed program on the card, and the test binary lives in /tmp: nothing to replace
	h2 := newHarness(t, nil)
	h2.pump(h2.m.openUpdate())
	if !strings.Contains(h2.popText(), "cannot find the installed program") {
		t.Errorf("popup = %q", h2.popText())
	}
}

func TestUIUpdateGitHubUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	withReleaseURL(t, url+"/latest")
	withVersion(t, "v1.0.0")
	h := newHarness(t, nil)
	dest := installedBinary(h.root)
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	_ = os.WriteFile(dest, []byte("old-binary"), 0o755)
	h.pump(h.m.openUpdate())
	if h.popTitle() == "Update available" || !strings.Contains(h.popText(), "FAILED") {
		t.Errorf("popup = %q", h.popText())
	}
	if h.m.toolJob != nil && h.m.pop != nil && h.m.pop.running {
		t.Error("the window should not stay on 'working'")
	}
}

func TestUIUpdateNotEnoughSpace(t *testing.T) {
	bin := builtProgram(t, "v9.9.9")
	h, dest := updateHarness(t, "v1.0.0", ghOpts{tag: "v9.9.9", bin: bin, declared: 1 << 50})
	h.pump(h.m.openUpdate())
	if h.popTitle() != "Not enough space" {
		t.Errorf("popup = %q", h.popText())
	}
	if string(readFile(t, dest)) != "old-binary" {
		t.Error("nothing should be installed")
	}
}

func TestUIUpdateDeclined(t *testing.T) {
	bin := builtProgram(t, "v9.9.9")
	h, dest := updateHarness(t, "v1.0.0", ghOpts{tag: "v9.9.9", bin: bin})
	h.pump(h.m.openUpdate())
	h.press("n")
	if h.m.pop != nil || h.quit || string(readFile(t, dest)) != "old-binary" {
		t.Error("declining must change nothing")
	}
}

// ------------------------------------------------------------------ temporary copies and restart

// helper processes: the test binary re-runs itself to check what only a separate process can show.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("ROMM_HELPER") {
	case "":
		t.Skip("helper process")
	case "removeowncopy":
		removeOwnCopy()
	case "relaunch":
		os.Args = []string{"romm", "-version"}
		fmt.Fprintln(os.Stderr, "relaunch failed:", relaunch(os.Getenv("ROMM_TARGET")))
		os.Exit(3)
	}
	os.Exit(0)
}

func runHelper(t *testing.T, exe, mode string, env ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(exe, "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), append([]string{"ROMM_HELPER=" + mode}, env...)...)
	return cmd.CombinedOutput()
}

func copyExe(t *testing.T, dir, pattern string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := copyFile(self, f.Name(), 0o700); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestRemoveOwnCopyDeletesOnlyTemporaryCopies(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("relies on /proc/self/exe")
	}
	tmpCopy := copyExe(t, os.TempDir(), tmpBinPrefix+"XXXXXX")
	defer os.Remove(tmpCopy)
	if out, err := runHelper(t, tmpCopy, "removeowncopy"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if exists(tmpCopy) {
		t.Error("the temporary copy was not removed")
	}

	// a program installed elsewhere, or under another name in /tmp, is left alone
	for _, p := range []string{copyExe(t, t.TempDir(), tmpBinPrefix+"XXXXXX"), copyExe(t, os.TempDir(), "unrelated-XXXXXX")} {
		if out, err := runHelper(t, p, "removeowncopy"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !exists(p) {
			t.Errorf("%s must not be removed", p)
		}
		os.Remove(p)
	}
}

func TestRelaunchHandsOverToTheNewProgram(t *testing.T) {
	bin := builtProgram(t, "v9.9.9")
	target := filepath.Join(t.TempDir(), "romm-new")
	if err := os.WriteFile(target, bin, 0o700); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	out, err := runHelper(t, self, "relaunch", "ROMM_TARGET="+target)
	if err != nil || !strings.Contains(string(out), "mister-romm-tui v9.9.9") {
		t.Fatalf("err=%v output=%q", err, out)
	}
}
