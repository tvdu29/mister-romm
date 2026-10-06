package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Mozilla's CA bundle (as published by curl). The MiSTer image may not ship one, and without it
// no HTTPS certificate can be verified.
//
//go:embed cacert.pem
var embeddedCA []byte

// env is a snapshot of everything a background job needs, so jobs never touch the model.
type env struct {
	base, token      string
	insecure         bool
	root, cfgDir     string
	gamesDir         string
	caFile, caSource string
	mapping          map[string]string
	client           *http.Client // with timeout, for API calls
	longClient       *http.Client // without total timeout, for downloads
}

func (m *model) cfgDir() string { return filepath.Join(m.data.Root, "Scripts", ".config", "romm") }

func (m *model) gamesDir() string {
	return m.cfg("games_dir", filepath.Join(m.data.Root, "games"))
}

// caPool picks the certificate authorities used to verify the server, in this order: ca_file from
// config.ini, a cacert.pem next to the settings, the one installed by the MiSTer Downloader, the
// system one, then the bundle embedded in this program.
func (m *model) caPool() (*x509.CertPool, string) {
	cands := []string{m.cfg("ca_file", ""), filepath.Join(m.cfgDir(), "cacert.pem"),
		filepath.Join(m.data.Root, "Scripts", ".config", "downloader", "cacert.pem"), "/etc/ssl/certs/cacert.pem",
		"/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/cert.pem"}
	for _, f := range cands {
		if f == "" {
			continue
		}
		if b, err := os.ReadFile(f); err == nil && len(b) > 0 {
			p := x509.NewCertPool()
			if p.AppendCertsFromPEM(b) {
				return p, f
			}
		}
	}
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(embeddedCA)
	return p, "built-in bundle"
}

func (m *model) insecure() bool { return m.cfg("verify_tls", "1") == "0" || m.sessionInsecure }

func (m *model) env() env {
	pool, src := m.caPool()
	mk := func(timeout time.Duration) *http.Client {
		return &http.Client{Timeout: timeout, CheckRedirect: sameHostRedirect, Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSClientConfig:       &tls.Config{RootCAs: pool, InsecureSkipVerify: m.insecure()},
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 45 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		}}
	}
	mapping := loadMapping(m.cfgDir())
	return env{base: m.cfg("server_url", ""), token: m.data.Token, insecure: m.insecure(), root: m.data.Root,
		cfgDir: m.cfgDir(), gamesDir: m.gamesDir(), caSource: src, mapping: mapping,
		client: mk(0), longClient: mk(0)}
}

// client is the short-timeout client used by the pairing and status code.
func (m *model) client() *http.Client { return m.env().client }

func (e env) get(ctx context.Context, c *http.Client, path string, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", e.base+path, nil)
	if err != nil {
		return nil, err
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	req.Header.Set("User-Agent", "mister-romm/"+version)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return c.Do(req)
}

// explainHTTP turns a failed answer into one readable line.
func explainHTTP(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	d := detail(string(b))
	switch resp.StatusCode {
	case 401, 403:
		return fmt.Sprintf("HTTP %d: the token was rejected or lacks a permission (%s)", resp.StatusCode, d)
	}
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, d)
}

// explainErr adds the likely cause to the most common network failures.
func explainErr(err error, clockOK bool) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "certificate has expired") || strings.Contains(s, "not yet valid") || strings.Contains(s, "x509"):
		hint := "check the clock and the CA bundle, or turn off \"Verify TLS\" in the Server window"
		if !clockOK {
			hint = "the clock of this device is wrong (" + time.Now().Format("2006-01-02") + ")"
		}
		return s + " - " + hint
	case strings.Contains(s, "no such host"):
		return s + " - cannot resolve the server name (network not ready yet?)"
	}
	return s
}

func clockOK() bool { return time.Now().Year() >= 2024 }

// logError appends to <config>/last-error.log (kept small) so failures can be read afterwards.
func logError(cfgDir, msg string) {
	f := filepath.Join(cfgDir, "last-error.log")
	_ = os.MkdirAll(cfgDir, 0o700)
	if fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(fh, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
		fh.Close()
	}
	if st, err := os.Stat(f); err == nil && st.Size() > 20000 {
		if b, err := os.ReadFile(f); err == nil {
			lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			if len(lines) > 100 {
				lines = lines[len(lines)-100:]
			}
			_ = os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		}
	}
}

// maxJSON caps the JSON bodies read from the server (a catalog page is far smaller).
const maxJSON = 64 << 20

// sameHostRedirect follows redirects within the same host and scheme only, so the token (sent as
// a header) never reaches another server and HTTPS is never downgraded.
func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		return fmt.Errorf("refusing a redirect to %s://%s", req.URL.Scheme, req.URL.Host)
	}
	return nil
}
