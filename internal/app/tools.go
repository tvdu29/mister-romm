package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

type toolLine struct{ text string }
type clockResult struct{ ok bool }

// ---------------------------------------------------------------- clock

// The MiSTer has no battery-backed clock: right after boot the date is 1970 and every HTTPS
// certificate looks "not yet valid". Same recovery as the official Scripts/update.sh: start ntpd
// and wait until it has synchronised.
func runClock(ctx context.Context, send func(tea.Msg)) {
	for _, bin := range []string{"ntpd", "ntpq", "start-stop-daemon"} {
		if _, err := exec.LookPath(bin); err != nil {
			send(clockResult{ok: clockOK()})
			return
		}
	}
	const conf, pid = "/etc/ntp.conf", "/var/run/ntpd.pid"
	if b, err := os.ReadFile(conf); err != nil || !strings.Contains(string(b), "\nserver") && !strings.HasPrefix(string(b), "server") && !strings.Contains(string(b), "\npool") {
		if f, err := os.OpenFile(conf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, "server pool.ntp.org iburst")
			fmt.Fprintln(f, "server time.cloudflare.com iburst")
			f.Close()
		}
	}
	_ = exec.Command("start-stop-daemon", "-K", "-p", pid).Run()
	_ = os.Remove(pid)
	_ = exec.Command("start-stop-daemon", "-S", "-q", "-p", pid, "-x", "/usr/sbin/ntpd", "--", "-g", "-p", pid).Run()
	for i := 0; i < 10 && ctx.Err() == nil; i++ {
		out, _ := exec.Command("ntpq", "-c", "rv 0").CombinedOutput()
		s := strings.ToLower(string(out))
		if !strings.Contains(s, "connection refused") && !strings.Contains(s, "sync_unspec") {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	time.Sleep(time.Second)
	send(clockResult{ok: clockOK()})
}

// ---------------------------------------------------------------- diagnostics

type docInfo struct {
	version, term      string
	w, h               int
	stdinTerminal      bool
	lastErrors         []string
	selected           int
	games              int
	configured, paired bool
}

func runDoctor(ctx context.Context, e env, in docInfo, send func(tea.Msg)) {
	line := func(f string, a ...any) { send(toolLine{fmt.Sprintf(f, a...)}) }
	line("mister-romm %s  (%s/%s)", in.version, runtime.GOOS, runtime.GOARCH)
	line("Card root:     %s", e.root)
	line("Settings:      %s", e.cfgDir)
	line("Games folder:  %s", e.gamesDir)
	if free, ok := freeSpace(e.gamesDir); ok {
		line("Free space:    %s", humanSize(free))
	}
	line("")
	if clockOK() {
		line("[ok]     clock: %s", time.Now().Format("2006-01-02 15:04:05"))
	} else {
		line("[WRONG]  clock: %s - HTTPS fails with \"certificate is not yet valid\"", time.Now().Format("2006-01-02 15:04:05"))
		for _, bin := range []string{"ntpd", "ntpq", "start-stop-daemon"} {
			if _, err := exec.LookPath(bin); err != nil {
				line("         %s not found: the clock cannot be set automatically", bin)
			}
		}
	}
	line("[info]   certificates: %s", e.caSource)
	line("")
	line("Terminal: TERM=%s  size=%dx%d  stdin is a terminal: %v", in.term, in.w, in.h, in.stdinTerminal)
	line("")
	if !in.configured {
		line("[--]     no server address set")
	} else {
		line("Server: %s", e.base)
		ctx2, cancel := context.WithTimeout(ctx, 20*time.Second)
		resp, err := e.get(ctx2, e.client, "/api/heartbeat", nil)
		cancel()
		switch {
		case err != nil:
			line("[FAILED] reachable: %s", explainErr(err, clockOK()))
			if !e.insecure {
				e2 := e
				e2.token = "" // the token must never reach a peer whose certificate is not verified
				e2.client = insecureClient(e.client)
				ctx3, c3 := context.WithTimeout(ctx, 20*time.Second)
				if r2, err2 := e2.get(ctx3, e2.client, "/api/heartbeat", nil); err2 == nil {
					r2.Body.Close()
					line("[info]   it works with certificate verification OFF: the problem is the clock or the CA bundle")
				}
				c3()
			}
		case resp.StatusCode != 200:
			line("[FAILED] reachable: HTTP %d", resp.StatusCode)
			resp.Body.Close()
		default:
			resp.Body.Close()
			line("[ok]     reachable")
		}
		if in.paired {
			ctx4, c4 := context.WithTimeout(ctx, 30*time.Second)
			if pl, err := fetchPlatforms(ctx4, e); err != nil {
				line("[FAILED] token: %s", explainErr(err, clockOK()))
			} else {
				line("[ok]     token accepted, %d platforms on the server", len(pl))
			}
			c4()
		} else {
			line("[--]     not paired")
		}
	}
	line("")
	line("Platforms indexed: %d   Games in the catalog: %d", in.selected, in.games)
	if len(in.lastErrors) > 0 {
		line("")
		line("Recent errors (last-error.log):")
		for _, l := range in.lastErrors {
			line("  %s", trunc(l, 90))
		}
	}
}

func insecureClient(c *http.Client) *http.Client {
	if t, ok := c.Transport.(*http.Transport); ok {
		t2 := t.Clone()
		t2.TLSClientConfig = t.TLSClientConfig.Clone()
		t2.TLSClientConfig.InsecureSkipVerify = true
		return &http.Client{Timeout: c.Timeout, Transport: t2}
	}
	return c
}

func tailLines(path string, n int) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// ---------------------------------------------------------------- speed test

func measure(ctx context.Context, c *http.Client, url string, hdr map[string]string, token string, limit time.Duration) (int64, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	n, _ := io.Copy(io.Discard, resp.Body) // a timeout is not an error here: we measure what arrived
	return n, time.Since(start), nil
}

func rate(n int64, d time.Duration) string {
	if d <= 0 {
		return "?"
	}
	return humanSize(int64(float64(n)/d.Seconds())) + "/s"
}

// runSpeed measures where the download speed goes: the internet link (a public test file, nothing
// saved), the card's write speed, and the RomM server itself (the start of a real game, not saved).
func runSpeed(ctx context.Context, e env, send func(tea.Msg)) {
	line := func(f string, a ...any) { send(toolLine{fmt.Sprintf(f, a...)}) }
	line("1. Internet link, TLS and CPU (speed.cloudflare.com, 20 MB, not saved)")
	n, d, err := measure(ctx, e.longClient, "https://speed.cloudflare.com/__down?bytes=20000000", nil, "", 25*time.Second)
	if err != nil {
		line("   failed: %s", explainErr(err, clockOK()))
	} else {
		line("   %s", rate(n, d))
	}
	line("")
	line("2. Writing to the card (%s, 32 MB, deleted afterwards)", e.gamesDir)
	_ = os.MkdirAll(e.gamesDir, 0o755)
	tmp := filepath.Join(e.gamesDir, ".romm-speedtest")
	if f, err := os.Create(tmp); err != nil {
		line("   failed: %s", err)
	} else {
		buf := make([]byte, 1<<20)
		start := time.Now()
		for i := 0; i < 32 && ctx.Err() == nil; i++ {
			if _, err := f.Write(buf); err != nil {
				break
			}
		}
		_ = f.Sync()
		el := time.Since(start)
		f.Close()
		os.Remove(tmp)
		line("   %s", rate(32<<20, el))
	}
	line("")
	line("3. The RomM server (%s)", e.base)
	if e.token == "" {
		line("   skipped (this device is not paired)")
		return
	}
	var id, name string
	sel := readSelection(e.cfgDir)
	for _, pid := range sortedKeys(sel) {
		for _, r := range readCatalog(catalogPath(e.cfgDir, pid)) {
			if r.size >= 10<<20 && (id == "" || r.id < id) {
				id, name = r.id, r.name
			}
		}
		if id != "" {
			break
		}
	}
	if id == "" {
		line("   skipped (sync the catalog first: it needs a game of at least 10 MB)")
		return
	}
	n, d, err = measure(ctx, e.longClient, e.base+"/api/roms/"+id+"/content/"+urlPath(name), map[string]string{"Range": "bytes=0-67108863"}, e.token, 12*time.Second)
	if err != nil {
		line("   failed: %s", explainErr(err, clockOK()))
	} else {
		line("   %s   (test game: %s)", rate(n, d), trunc(name, 40))
	}
	line("")
	line("The download cannot be faster than the slowest of the three.")
	line("1 high, 3 low: the server side (upload speed, reverse proxy).")
	line("1 low too: this device (network adapter, CPU/TLS).  2 low: the card.")
}
