package app

import (
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
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Self-update: the release workflow publishes the bare MiSTer binary and SHA256SUMS next to the
// zip. Settings > Check for updates fetches the latest release, downloads the binary, verifies
// it, replaces the installed one and restarts into it. romm.sh is never touched: it only copies
// the installed binary to /tmp and runs it, so the installed file can be replaced while running.

const (
	maxBinary    = 64 << 20
	tmpBinPrefix = "romm-tui." // copies made by romm.sh (romm-tui.XXXXXX) and by the updater
)

// releaseURL is a variable only so that the tests can use a local server.
var releaseURL = "https://api.github.com/repos/tvdu29/mister-romm/releases/latest"

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type release struct {
	Tag    string         `json:"tag_name"`
	Assets []releaseAsset `json:"assets"`
}

func (r release) asset(name string) (releaseAsset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return releaseAsset{}, false
}

// toolProgress rewrites the last line of the tool window (a percentage) instead of adding one.
type toolProgress struct{ text string }

type updateInfo struct {
	rel    release
	target string
}

type updateDone struct {
	tag, staged string
	err         error
}

// ------------------------------------------------------------------ versions

// parseVersion reads "v1.2.3" (a pre-release or build suffix is ignored).
func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if s == "" || len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// newerVersion reports whether latest is a strictly newer release than cur. A development build
// (or anything that is not a version) is never updated.
func newerVersion(cur, latest string) bool {
	c, ok1 := parseVersion(cur)
	l, ok2 := parseVersion(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// binaryAsset is the release file for this platform ("" when there is none).
func binaryAsset() string {
	switch runtime.GOARCH {
	case "arm":
		return "romm-tui-linux-armv7"
	case "amd64":
		return "romm-tui-linux-amd64"
	}
	return ""
}

// installedBinary is where the zip puts the program, and where romm.sh runs it from.
func installedBinary(root string) string {
	return filepath.Join(root, "Scripts", ".config", "romm", "bin", "romm-tui")
}

// updateTarget is the file to replace: the program installed on the card (the one romm.sh runs),
// else on a PC the running program itself. A copy under /tmp is never a target.
func updateTarget(root string) (string, error) {
	p := installedBinary(root)
	if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
		return p, nil
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(exe, filepath.Clean(os.TempDir())+string(filepath.Separator)) {
		return "", fmt.Errorf("cannot find the installed program (%s)", p)
	}
	return exe, nil
}

// checkELF makes sure the file is a Linux program for this processor, before anything is run.
func checkELF(head []byte) error {
	if len(head) < 20 || string(head[:4]) != "\x7fELF" {
		return errors.New("the downloaded file is not a Linux program")
	}
	want := map[string]uint16{"arm": 40, "amd64": 62, "arm64": 183, "386": 3}[runtime.GOARCH]
	if got := uint16(head[18]) | uint16(head[19])<<8; want != 0 && got != want {
		return fmt.Errorf("the downloaded program is for another processor (machine %d)", got)
	}
	return nil
}

// parseSums reads the output of sha256sum: "<hex>  <name>" per line.
func parseSums(text string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && len(f[0]) == sha256.Size*2 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out
}

// ------------------------------------------------------------------- network

// githubHost says whether a redirect may go there (the release files are served from another
// GitHub host). It is a variable so that the tests can use a local server.
var githubHost = func(host string) bool {
	return host == "github.com" || host == "api.github.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

// githubClient talks to GitHub: no RomM token, and certificates are ALWAYS verified, even when
// the verification is turned off for the RomM server (a program to be run is fetched here).
// Redirects stay on HTTPS and on GitHub hosts.
func (m *model) githubClient() *http.Client {
	pool, _ := m.caPool()
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" || !githubHost(req.URL.Hostname()) {
				return fmt.Errorf("refusing a redirect to %s://%s", req.URL.Scheme, req.URL.Host)
			}
			return nil
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSClientConfig:       &tls.Config{RootCAs: pool},
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 45 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
	}
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
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("GitHub refused the request (HTTP %d, rate limit?): try again later", resp.StatusCode)
		}
		return nil, fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func fetchRelease(ctx context.Context, c *http.Client, url string) (release, error) {
	var r release
	resp, err := ghGet(ctx, c, url)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSON)).Decode(&r); err != nil {
		return r, err
	}
	if r.Tag == "" {
		return r, errors.New("no release found")
	}
	return r, nil
}

// downloadTo streams a release file into f and returns its sha256. size is the announced size
// (never trusted beyond +1 byte, to detect an overflow); progress is called each time the
// percentage changes.
func downloadTo(ctx context.Context, c *http.Client, a releaseAsset, f *os.File, progress func(done, total int64)) (string, error) {
	if a.Size <= 0 || a.Size > maxBinary {
		return "", fmt.Errorf("unexpected size for %s: %d", a.Name, a.Size)
	}
	resp, err := ghGet(ctx, c, a.URL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	h := sha256.New()
	buf := make([]byte, 64*1024)
	body := io.LimitReader(resp.Body, a.Size+1)
	var done int64
	last := -1
	progress(0, a.Size)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			if _, err := f.Write(buf[:n]); err != nil {
				return "", err
			}
			done += int64(n)
			if done > a.Size {
				return "", errors.New("the server sent more data than announced")
			}
			if pct := int(done * 100 / a.Size); pct != last {
				last = pct
				progress(done, a.Size)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	if done != a.Size {
		return "", fmt.Errorf("incomplete download: %d of %d bytes", done, a.Size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fetchSums(ctx context.Context, c *http.Client, a releaseAsset) (map[string]string, error) {
	resp, err := ghGet(ctx, c, a.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseSums(string(b)), nil
}

// --------------------------------------------------------------------- jobs

func (m *model) openUpdate() tea.Cmd {
	switch {
	case m.toolJob != nil:
		m.toast = "Another test is already running"
		return nil
	case m.syncJob != nil || m.dlJob != nil:
		m.message("Check for updates", []string{"A sync or a download is running.", "Wait for it to finish (or cancel it with c), then try again."})
		return nil
	case binaryAsset() == "":
		m.message("Check for updates", []string{"No release file exists for " + runtime.GOOS + "/" + runtime.GOARCH + "."})
		return nil
	}
	target, err := updateTarget(m.data.Root)
	if err != nil {
		m.message("Check for updates", []string{err.Error(), "", "Only the copy installed on the SD card can update itself."})
		return nil
	}
	c := m.githubClient()
	m.pop = &popup{kind: "tool", title: "Check for updates", running: true}
	j, cmd := startJob(func(ctx context.Context, send func(tea.Msg)) {
		send(toolLine{"Installed version: " + version})
		send(toolLine{"Looking for the latest release on GitHub ..."})
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		rel, err := fetchRelease(cctx, c, releaseURL)
		if err != nil {
			send(toolLine{"[FAILED] " + explainErr(err, clockOK())})
			return
		}
		send(toolLine{"Latest release:    " + rel.Tag})
		// a development build has no version to compare: the release is offered
		if _, isRelease := parseVersion(version); isRelease && !newerVersion(version, rel.Tag) {
			send(toolLine{""})
			send(toolLine{"You are up to date."})
			return
		}
		send(updateInfo{rel, target})
	})
	m.toolJob = j
	return cmd
}

func (m *model) offerUpdate(u updateInfo) {
	bin, okBin := u.rel.asset(binaryAsset())
	_, okSums := u.rel.asset("SHA256SUMS")
	if !okBin || !okSums {
		m.message("Check for updates", []string{u.rel.Tag + " is out, but its files are incomplete", "(" + binaryAsset() + " or SHA256SUMS is missing)."})
		return
	}
	// the copy under /tmp (RAM on the MiSTer) takes one binary; the card takes the new one and
	// the kept previous one
	const margin = 4 << 20
	tmpFree, okTmp := freeSpace(os.TempDir())
	cardFree, okCard := freeSpace(filepath.Dir(u.target))
	if okTmp && tmpFree < bin.Size+margin {
		m.message("Not enough space", []string{"Not enough memory in " + os.TempDir() + ":", humanSize(tmpFree) + " free, " + humanSize(bin.Size+margin) + " needed."})
		return
	}
	if okCard && cardFree < 2*bin.Size+margin {
		m.message("Not enough space", []string{"Not enough room on the card:", humanSize(cardFree) + " free, " + humanSize(2*bin.Size+margin) + " needed."})
		return
	}
	from := version
	if _, isRelease := parseVersion(version); !isRelease {
		from = version + " (development build)"
	}
	lines := []string{
		from + "  ->  " + u.rel.Tag, "",
		"Downloads " + humanSize(bin.Size) + ", verifies it, replaces the installed program",
		"(the previous one is kept as romm-tui.old) and restarts.", ""}
	if okCard && okTmp {
		lines = append(lines, "Free space: card "+humanSize(cardFree)+", "+os.TempDir()+" "+humanSize(tmpFree), "")
	}
	m.confirm("Update available", append(lines, "Update now?"), func(m *model) tea.Cmd { return m.startUpdate(u.rel, u.target) })
}

func (m *model) startUpdate(rel release, dest string) tea.Cmd {
	c := m.githubClient()
	m.pop = &popup{kind: "tool", title: "Updating to " + rel.Tag, running: true}
	j, cmd := startJob(func(ctx context.Context, send func(tea.Msg)) {
		line := func(s string) { send(toolLine{s}) }
		prog := func(s string) { send(toolProgress{s}) }
		staged, err := runUpdate(ctx, c, rel, dest, line, prog)
		if err != nil {
			line("[FAILED] " + explainErr(err, clockOK()))
			line("The installed program was not changed.")
		}
		send(updateDone{tag: rel.Tag, staged: staged, err: err})
	})
	m.toolJob = j
	return cmd
}

// runUpdate downloads and checks the new binary in a private copy under /tmp (the card may not
// allow executing files, which is why romm.sh also runs from /tmp), installs it, and returns the
// path of the copy to restart into. On failure the copy is removed.
func runUpdate(ctx context.Context, c *http.Client, rel release, dest string, line, prog func(string)) (staged string, err error) {
	bin, _ := rel.asset(binaryAsset())
	sumsAsset, _ := rel.asset("SHA256SUMS")
	f, err := os.CreateTemp("", tmpBinPrefix+"upd.")
	if err != nil {
		return "", err
	}
	path := f.Name()
	staged = path
	defer func() {
		f.Close()
		if err != nil {
			_ = os.Remove(path) // not staged: a "return "", err" has already cleared the result
		}
	}()

	line("Downloading " + bin.Name + " ...")
	got, err := downloadTo(ctx, c, bin, f, func(done, total int64) {
		prog(fmt.Sprintf("  %3d%%   %s / %s", done*100/total, humanSize(done), humanSize(total)))
	})
	if err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}

	line("Verifying ...")
	sums, err := fetchSums(ctx, c, sumsAsset)
	if err != nil {
		return "", err
	}
	want, ok := sums[bin.Name]
	if !ok {
		return "", fmt.Errorf("SHA256SUMS has no entry for %s", bin.Name)
	}
	if got != want {
		return "", errors.New("checksum mismatch: the download is corrupted")
	}
	head := make([]byte, 20)
	if hf, herr := os.Open(path); herr == nil {
		_, _ = io.ReadFull(hf, head)
		hf.Close()
	}
	if err = checkELF(head); err != nil {
		return "", err
	}
	if err = os.Chmod(path, 0o700); err != nil {
		return "", err
	}
	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, path, "-version").Output()
	if err != nil {
		return "", fmt.Errorf("the new program does not start: %v", err)
	}
	if !strings.Contains(string(out), rel.Tag) {
		return "", fmt.Errorf("the new program reports %q, expected %s", strings.TrimSpace(string(out)), rel.Tag)
	}

	line("Installing ...")
	if err = installBinary(path, dest); err != nil {
		return "", err
	}
	line("Installed " + rel.Tag + ".")
	return staged, nil
}

// installBinary puts src at dest with a single atomic rename, so dest is always a complete
// program. The previous one is kept next to it as dest.old (best effort).
func installBinary(src, dest string) error {
	tmp := dest + ".new"
	_ = os.Remove(tmp)
	if err := copyFile(src, tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if _, err := os.Stat(dest); err == nil {
		_ = copyFile(dest, dest+".old", 0o755)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	_ = os.Remove(dst)
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// finishUpdate leaves the interface so that Run can restart into the new program.
func (m *model) finishUpdate(d updateDone) tea.Cmd {
	if d.err != nil {
		return nil
	}
	m.relaunch = d.staged
	return tea.Quit
}

// ------------------------------------------------------------------ restart

// relaunch replaces this process with the program at path (same terminal, same arguments).
// exec keeps the process id and frees everything the old version held. It only returns on error.
func relaunch(path string) error {
	return syscall.Exec(path, append([]string{path}, os.Args[1:]...), os.Environ())
}

// removeOwnCopy deletes this program's file when it is a temporary copy (romm.sh runs a copy
// from /tmp, a RAM disk on the MiSTer; the updater restarts into another one). Removing a running
// executable is safe on Linux, and it keeps at most one copy alive however many updates are done.
func removeOwnCopy() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if filepath.Dir(exe) == filepath.Clean(os.TempDir()) && strings.HasPrefix(filepath.Base(exe), tmpBinPrefix) {
		_ = os.Remove(exe)
	}
}
