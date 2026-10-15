package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// RomM "device" pairing (RFC 8628 style): this device asks for a code, the user approves it in a
// browser (phone), and the device collects a token bound to it. Endpoints and answers were
// checked against a RomM 5.3.1 server (its OpenAPI spec and real answers while pending).

var wantedScopes = []string{"platforms.read", "roms.read", "collections.read"}

type pairState struct {
	deviceCode, userCode, url string
	interval                  time.Duration
	deadline, nextPoll        time.Time
	lastErr                   string
}

type pairInitMsg struct {
	gen int
	ps  *pairState
	err string
}
type pairTickMsg struct{ gen int }
type pairPollMsg struct {
	gen             int
	state           string // pending | slow | ok | fail | neterr
	text            string
	token, deviceID string
	scopes          []string
}
type exchangeMsg struct {
	ok     bool
	text   string
	token  string
	scopes []string
}

// deviceIdentifier is a stable id for this MiSTer: a MAC address, else the host name.
func deviceIdentifier() string {
	if ents, err := filepath.Glob("/sys/class/net/*/address"); err == nil {
		for _, f := range ents {
			if strings.Contains(f, "/lo/") {
				continue
			}
			if b, err := os.ReadFile(f); err == nil {
				if a := strings.TrimSpace(string(b)); a != "" && a != "00:00:00:00:00:00" {
					return a
				}
			}
		}
	}
	h, _ := os.Hostname()
	return h
}

func postJSON(c *http.Client, url string, in any, out any) (int, string, error) {
	b, _ := json.Marshal(in)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode, string(raw), nil
}

// detail is the message of an error answer, cleaned and short enough to show: a server could
// send a megabyte of it.
func detail(raw string) string {
	var d struct {
		Detail any `json:"detail"`
	}
	s := ""
	if json.Unmarshal([]byte(raw), &d) == nil && d.Detail != nil {
		s = clean(fmt.Sprint(d.Detail))
	} else {
		s = clean(strings.TrimSpace(raw))
	}
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "..."
	}
	return s
}

func (m *model) saveCredentials(token, deviceID string, scopes []string) error {
	if err := checkCredentials(token, deviceID, scopes); err != nil {
		return err
	}
	m.data.Token = token
	m.set("token", token)
	m.set("device_id", deviceID)
	m.set("scopes", strings.Join(scopes, " "))
	return nil
}

// ----------------------------------------------------------- pairing from a phone

func (m *model) startPairing() tea.Cmd {
	base := m.cfg("server_url", "")
	if base == "" {
		m.message("Pair this device", []string{"Set the server address first (Server URL)."})
		return nil
	}
	m.pairGen++
	gen := m.pairGen
	m.pair = nil
	m.pop = &popup{kind: "pairing", title: "Pair this device", lines: []string{"Contacting " + base + " ..."}}
	c := m.client()
	return func() tea.Msg {
		host, _ := os.Hostname()
		var out struct {
			DeviceCode string `json:"device_code"`
			UserCode   string `json:"user_code"`
			PathFull   string `json:"verification_path_complete"`
			ExpiresIn  int    `json:"expires_in"`
			Interval   int    `json:"interval"`
		}
		status, raw, err := postJSON(c, base+"/api/auth/device/init", map[string]any{
			"client_device_identifier": deviceIdentifier(),
			"name":                     "MiSTer " + host,
			"client":                   "mister-romm",
			"platform":                 "mister",
			"client_version":           version,
			"requested_scopes":         wantedScopes,
		}, &out)
		if err != nil {
			return pairInitMsg{gen: gen, err: err.Error()}
		}
		if status != 200 && status != 201 || out.DeviceCode == "" {
			return pairInitMsg{gen: gen, err: fmt.Sprintf("HTTP %d: %s", status, detail(raw))}
		}
		iv := time.Duration(out.Interval) * time.Second
		if iv < 5*time.Second {
			iv = 5 * time.Second
		}
		exp := time.Duration(out.ExpiresIn) * time.Second
		if exp <= 0 {
			exp = 10 * time.Minute
		}
		return pairInitMsg{gen: gen, ps: &pairState{deviceCode: out.DeviceCode, userCode: clean(out.UserCode),
			url: base + clean(out.PathFull), interval: iv, deadline: time.Now().Add(exp), nextPoll: time.Now().Add(iv)}}
	}
}

func pairTick(gen int) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return pairTickMsg{gen: gen} })
}

func (m *model) pollCmd(gen int, deviceCode string) tea.Cmd {
	base := m.cfg("server_url", "")
	c := m.client()
	return func() tea.Msg {
		var out struct {
			AccessToken string   `json:"access_token"`
			DeviceID    string   `json:"device_id"`
			Scopes      []string `json:"scopes"`
		}
		status, raw, err := postJSON(c, base+"/api/auth/device/token", map[string]string{"device_code": deviceCode}, &out)
		switch {
		case err != nil:
			return pairPollMsg{gen: gen, state: "neterr", text: err.Error()}
		case status == 200 && out.AccessToken != "":
			return pairPollMsg{gen: gen, state: "ok", token: out.AccessToken, deviceID: out.DeviceID, scopes: out.Scopes}
		}
		d := detail(raw)
		switch {
		case strings.Contains(d, "authorization_pending"):
			return pairPollMsg{gen: gen, state: "pending"}
		case strings.Contains(d, "slow_down"):
			return pairPollMsg{gen: gen, state: "slow"}
		}
		return pairPollMsg{gen: gen, state: "fail", text: fmt.Sprintf("HTTP %d: %s", status, d)}
	}
}

// handlePair processes the messages of both pairing flows.
func (m *model) handlePair(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case pairInitMsg:
		if msg.gen != m.pairGen {
			return nil, true
		}
		if msg.err != "" {
			m.message("Pairing failed", []string{msg.err, "", "Check the address and the connection (Server > Test connection)."})
			return nil, true
		}
		m.pair = msg.ps
		return pairTick(msg.gen), true

	case pairTickMsg:
		if msg.gen != m.pairGen || m.pair == nil {
			return nil, true
		}
		now := time.Now()
		if now.After(m.pair.deadline) {
			m.pair = nil
			m.message("Pairing expired", []string{"The code was not approved in time.", "Start the pairing again."})
			return nil, true
		}
		if now.After(m.pair.nextPoll) {
			m.pair.nextPoll = now.Add(m.pair.interval)
			return tea.Batch(m.pollCmd(msg.gen, m.pair.deviceCode), pairTick(msg.gen)), true
		}
		return pairTick(msg.gen), true

	case pairPollMsg:
		if msg.gen != m.pairGen || m.pair == nil {
			return nil, true
		}
		switch msg.state {
		case "ok":
			m.pair = nil
			m.pairGen++
			if err := m.saveCredentials(msg.token, msg.deviceID, msg.scopes); err != nil {
				m.message("Pairing refused", []string{err.Error(), "", "Nothing was saved."})
				return nil, true
			}
			return m.paired(msg.scopes), true
		case "slow":
			m.pair.interval += 5 * time.Second
		case "neterr":
			m.pair.lastErr = msg.text
		case "fail":
			m.pair = nil
			m.pairGen++
			m.message("Pairing refused", []string{msg.text})
		default:
			m.pair.lastErr = ""
		}
		return nil, true

	case exchangeMsg:
		if !msg.ok {
			m.message("Pairing failed", []string{msg.text, "", "Codes are single-use and expire quickly: generate a new one."})
			return nil, true
		}
		if err := m.saveCredentials(msg.token, "", msg.scopes); err != nil {
			m.message("Pairing refused", []string{err.Error(), "", "Nothing was saved."})
			return nil, true
		}
		return m.paired(msg.scopes), true
	}
	return nil, false
}

// paired runs after a successful pairing: with no platform indexed yet, the platform picker opens
// straight away (Enter there starts the sync), otherwise a short confirmation is shown.
func (m *model) paired(scopes []string) tea.Cmd {
	if len(readSelection(m.cfgDir())) == 0 {
		return tea.Batch(m.checkServer(), m.openPlatforms())
	}
	m.message("Paired", []string{"This device is now connected to RomM.", "Permissions: " + strings.Join(scopes, " ")})
	return m.checkServer()
}

func (m *model) pairingLines() []string {
	p := m.pair
	if p == nil {
		return []string{"Contacting " + m.cfg("server_url", "") + " ..."}
	}
	left := time.Until(p.deadline).Round(time.Second)
	if left < 0 {
		left = 0
	}
	lines := []string{
		" 1. On a phone or a PC, open:",
		"    " + p.url,
		"",
		" 2. Log in to RomM and approve this device.",
		"    Code shown there: " + p.userCode,
		"",
		fmt.Sprintf(" Waiting for your approval...  %s left", left),
	}
	if p.lastErr != "" {
		lines = append(lines, " (network: "+p.lastErr+", retrying)")
	}
	return append(lines, "", " Esc: cancel")
}

func (m *model) cancelPairing() {
	m.pairGen++
	m.pair = nil
	m.pop = nil
}

// ------------------------------------------------------------- pairing with a code

// openCodePairing asks for a code generated in RomM (profile > Client API Tokens > Pair).
func (m *model) openCodePairing() {
	m.openInput("Pair with a code", []string{
		"In RomM: profile > Client API Tokens > create a token > Pair.",
		"Enter the code (format XXXX-XXXX):"}, "", func(m *model, text string) tea.Cmd {
		code := strings.ToUpper(strings.Join(strings.Fields(text), ""))
		if len(code) < 6 || len(code) > 16 || strings.Trim(code, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
			m.message("Invalid code", []string{"The code only contains letters, digits and dashes (e.g. ABCD-EFGH)."})
			return nil
		}
		base := m.cfg("server_url", "")
		if base == "" {
			m.message("Pair with a code", []string{"Set the server address first (Server URL)."})
			return nil
		}
		m.pop = &popup{kind: "info", title: "Pair with a code", lines: []string{"Contacting the server ..."}}
		c := m.client()
		return func() tea.Msg {
			var out struct {
				Raw    string   `json:"raw_token"`
				Scopes []string `json:"scopes"`
			}
			status, raw, err := postJSON(c, base+"/api/client-tokens/exchange", map[string]string{"code": code}, &out)
			switch {
			case err != nil:
				return exchangeMsg{text: err.Error()}
			case status != 200 || out.Raw == "":
				return exchangeMsg{text: fmt.Sprintf("HTTP %d: %s", status, detail(raw))}
			}
			return exchangeMsg{ok: true, token: out.Raw, scopes: out.Scopes}
		}
	})
}
