package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Self-update: the program downloads its latest release from GitHub, checks it against the
// SHA256SUMS published with it, and replaces the binary the launcher (Scripts/romm.sh) runs. The
// running copy lives in /tmp on a MiSTer, so replacing the file on the card is safe; the new
// version starts next time.

const updateRepo = "tvdu29/mister-romm"

// githubAPI and githubHost are variables only so that the tests can use a local server.
var (
	githubAPI  = "https://api.github.com"
	githubHost = func(host string) bool {
		return host == "github.com" || host == "api.github.com" || strings.HasSuffix(host, ".githubusercontent.com")
	}
)

// maxBinary caps the download (the binary is about 9 MB).
const maxBinary = 64 << 20

type release struct {
	tag       string
	assetName string
	assetURL  string
	assetSize int64
	sumsURL   string
}

// releaseAsset is the release file built for this machine.
func releaseAsset() string {
	switch runtime.GOARCH {
	case "arm":
		return "romm-tui-linux-armv7"
	case "amd64":
		return "romm-tui-linux-amd64"
	}
	return ""
}

// githubClient never skips certificate checks (it fetches a program to run) and follows
// redirects to GitHub hosts only, over HTTPS.
func (m *model) githubClient() *http.Client {
	pool, _ := m.caPool()
	return &http.Client{Timeout: 0, Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       &tls.Config{RootCAs: pool},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 45 * time.Second,
	}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" || !githubHost(req.URL.Hostname()) {
			return fmt.Errorf("refusing a redirect to %s://%s", req.URL.Scheme, req.URL.Host)
		}
		return nil
	}}
}

func ghGet(ctx context.Context, c *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mister-romm/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// fetchLatestRelease asks GitHub for the latest release and the files this machine needs.
func fetchLatestRelease(ctx context.Context, c *http.Client) (release, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := ghGet(ctx, c, githubAPI+"/repos/"+updateRepo+"/releases/latest")
	if err != nil {
		return release{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return release{}, fmt.Errorf("unexpected answer from GitHub: %v", err)
	}
	r := release{tag: clean(out.Tag), assetName: releaseAsset()}
	if r.assetName == "" {
		return r, fmt.Errorf("no release is built for this machine (%s)", runtime.GOARCH)
	}
	for _, a := range out.Assets {
		switch a.Name {
		case r.assetName:
			r.assetURL, r.assetSize = a.URL, a.Size
		case "SHA256SUMS":
			r.sumsURL = a.URL
		}
	}
	if r.tag == "" || r.assetURL == "" || r.sumsURL == "" {
		return r, fmt.Errorf("the latest release (%s) lacks %s or SHA256SUMS", r.tag, r.assetName)
	}
	return r, nil
}

// parseVersion reads "v1.2.3" (a suffix such as "-rc1" is ignored).
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// isNewer reports whether latest is a newer version than cur. A development build (no version)
// is never told to update by itself, but can still be updated by hand.
func isNewer(latest, cur string) bool {
	l, ok1 := parseVersion(latest)
	c, ok2 := parseVersion(cur)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// sumFor finds the SHA-256 of a file in a SHA256SUMS file ("<hex>  <name>" lines).
func sumFor(sums []byte, name string) string {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// checkELF makes sure the file is a Linux program for this machine before it replaces the
// running one.
func checkELF(b []byte) error {
	if len(b) < 20 || string(b[:4]) != "\x7fELF" {
		return errors.New("the downloaded file is not a Linux program")
	}
	want := map[string]uint16{"arm": 40, "amd64": 62, "arm64": 183, "386": 3}[runtime.GOARCH]
	if got := uint16(b[18]) | uint16(b[19])<<8; want != 0 && got != want {
		return fmt.Errorf("the downloaded program is for another processor (machine %d)", got)
	}
	return nil
}

// updateTarget is the file to replace: the binary the launcher copies and runs, else (on a PC)
// the running program itself.
func updateTarget(cfgDir string) (string, error) {
	p := filepath.Join(cfgDir, "bin", "romm-tui")
	if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	if strings.HasPrefix(exe, os.TempDir()+string(filepath.Separator)) {
		return "", fmt.Errorf("cannot find the installed program (%s)", p)
	}
	return exe, nil
}

// runUpdate downloads the release, checks it and installs it over target. The previous version
// is kept as <target>.prev.
func runUpdate(ctx context.Context, c *http.Client, r release, target string, send func(tea.Msg)) {
	line := func(f string, a ...any) { send(toolLine{text: fmt.Sprintf(f, a...)}) }
	fail := func(err error) {
		if ctx.Err() != nil {
			line("Cancelled: nothing was changed.")
			return
		}
		line("FAILED: %v", err)
		line("Nothing was changed.")
	}
	line("Downloading %s %s ...", r.assetName, r.tag)
	sums, err := fetchAll(ctx, c, r.sumsURL, 64<<10)
	if err != nil {
		fail(fmt.Errorf("SHA256SUMS: %w", err))
		return
	}
	want := sumFor(sums, r.assetName)
	if want == "" {
		fail(fmt.Errorf("SHA256SUMS has no line for %s", r.assetName))
		return
	}
	bin, err := fetchAll(ctx, c, r.assetURL, maxBinary)
	if err != nil {
		fail(err)
		return
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		fail(errors.New("SHA-256 mismatch: the download is damaged or was altered"))
		return
	}
	if err := checkELF(bin); err != nil {
		fail(err)
		return
	}
	line("%s, SHA-256 verified.", humanSize(int64(len(bin))))
	if err := installBinary(target, bin); err != nil {
		fail(err)
		return
	}
	line("")
	line("Installed %s in %s", r.tag, target)
	line("Quit (q) and start romm again from the Scripts menu to use it.")
}

func fetchAll(ctx context.Context, c *http.Client, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	resp, err := ghGet(ctx, c, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("the file is larger than %s", humanSize(limit))
	}
	return b, nil
}

// installBinary writes the new program next to the old one and swaps them, so the target is
// always a complete program, whatever happens.
func installBinary(target string, bin []byte) error {
	tmp := target + ".new"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := f.Write(bin); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	prev := target + ".prev"
	_ = os.Remove(prev)
	if err := os.Link(target, prev); err != nil { // exFAT has no hard links: copy instead
		if b, rerr := os.ReadFile(target); rerr == nil {
			_ = os.WriteFile(prev, b, 0o755)
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ---------------------------------------------------------------- UI

type updateCheckMsg struct {
	rel   release
	err   string
	quiet bool
}

// checkUpdate asks GitHub for the latest release. Quiet: at start, only a hint when there is one.
func (m *model) checkUpdate(quiet bool) tea.Cmd {
	if !quiet {
		m.pop = &popup{kind: "info", title: "Update", lines: []string{"Asking GitHub for the latest version ..."}}
	}
	c := m.githubClient()
	return func() tea.Msg {
		r, err := fetchLatestRelease(context.Background(), c)
		if err != nil {
			return updateCheckMsg{rel: r, err: explainErr(err, clockOK()), quiet: quiet}
		}
		return updateCheckMsg{rel: r, quiet: quiet}
	}
}

func (m *model) onUpdateCheck(msg updateCheckMsg) tea.Cmd {
	if msg.err == "" {
		m.latest = msg.rel.tag
	}
	if msg.quiet {
		if msg.err == "" && isNewer(msg.rel.tag, version) && m.pop == nil {
			m.toast = "mister-romm " + msg.rel.tag + " is available: Settings > Update"
		}
		return nil
	}
	if m.pop == nil || m.pop.title != "Update" { // the window was closed meanwhile
		return nil
	}
	if msg.err != "" {
		logError(m.cfgDir(), "update: "+msg.err)
		m.message("Update", []string{"Could not get the latest version:", msg.err})
		return nil
	}
	r := msg.rel
	target, err := updateTarget(m.cfgDir())
	if err != nil {
		m.message("Update", []string{err.Error()})
		return nil
	}
	lines := []string{"Installed: " + version, "Latest:    " + r.tag, ""}
	switch {
	case isNewer(r.tag, version):
		lines = append(lines, fmt.Sprintf("Download and install %s (%s)?", r.tag, humanSize(r.assetSize)))
	case parseVersionOK(version):
		lines = append(lines, "You already have the latest version.", "", "Install it again anyway?")
	default:
		lines = append(lines, "This is a development build: install the release?")
	}
	lines = append(lines, "", "File: "+target)
	m.confirm("Update", lines, func(m *model) tea.Cmd {
		if m.toolJob != nil {
			m.toast = "Another task is already running"
			return nil
		}
		c := m.githubClient()
		m.pop = &popup{kind: "tool", title: "Update", running: true}
		j, cmd := startJob(func(ctx context.Context, send func(tea.Msg)) { runUpdate(ctx, c, r, target, send) })
		m.toolJob = j
		return cmd
	})
	return nil
}

func parseVersionOK(v string) bool { _, ok := parseVersion(v); return ok }

// updateLabel is the Settings entry, with the version found at start when it is newer.
func (m *model) updateLabel() string {
	if m.latest != "" && isNewer(m.latest, version) {
		return "Update mister-romm: " + m.latest + " available (installed " + version + ")"
	}
	return "Update mister-romm (installed " + version + ")"
}
