package app

import (
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

// ---------------------------------------------------------------- config.ini

func cfgPath(root string) string {
	return filepath.Join(root, "Scripts", ".config", "romm", "config.ini")
}

// setConfig updates one key of config.ini and keeps every other line (including the token, which
// this program never loads). The file stays private (0600), as written by romm.sh.
func setConfig(root, key, value string) error {
	if strings.ContainsAny(value, "\r\n\x00") || strings.ContainsAny(key, "=\r\n\x00") {
		return fmt.Errorf("refusing to store a value with a line break in config.ini")
	}
	path := cfgPath(root)
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
	found := false
	for i, l := range lines {
		if k, _, ok := strings.Cut(l, "="); ok && k == key {
			lines[i], found = key+"="+value, true
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp) // a leftover file could have looser permissions
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// set stores a setting in memory and on disk, and reports a failure in a message window.
func (m *model) set(key, value string) {
	if key != "token" { // the token is kept out of the settings map (it lives in m.data.Token)
		m.data.Config[key] = value
	}
	m.noteErr("config.ini", setConfig(m.data.Root, key, value))
}

func (m *model) cfg(key, def string) string {
	if v := m.data.Config[key]; v != "" {
		return v
	}
	return def
}

func normalizeURL(u string) (string, bool) {
	u = strings.TrimSpace(u)
	for strings.HasSuffix(u, "/") {
		u = strings.TrimSuffix(u, "/")
	}
	if u == "" || strings.ContainsAny(u, " \t") {
		return "", false
	}
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", false
	}
	return u, true
}

// ------------------------------------------------------------------- windows

func (m *model) message(title string, lines []string) {
	if m.saveErr != "" { // a failed save must never be hidden by the next message
		lines = append(append([]string{}, lines...), "", "WARNING: not saved to disk - "+m.saveErr)
	}
	m.pop = &popup{kind: "info", title: title, lines: lines}
}

// openInput shows a one-line text field; done receives the text when Enter is pressed.
func (m *model) openInput(title string, prompt []string, text string, done func(m *model, text string) tea.Cmd) {
	m.pop = &popup{kind: "input", title: title, lines: prompt, text: text, done: done}
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func (m *model) serverItems() (labels, ids []string) {
	tlsOn := m.cfg("verify_tls", "1") != "0"
	paired := "not paired"
	if m.cfg("device_id", "") != "" || m.cfg("scopes", "") != "" {
		paired = "paired"
	}
	labels = []string{
		"Server URL: " + m.cfg("server_url", "(not set)"),
		"Verify TLS certificate: " + yesNo(tlsOn),
		"Pair this device (approve from a phone)",
		"Pair with a code",
		"Test connection",
		"Disconnect (forget the token) - " + paired,
	}
	ids = []string{"url", "tls", "pair", "code", "test", "forget"}
	return
}

// forgetCredentials drops the token and the pairing data of this device.
func (m *model) forgetCredentials() {
	m.set("token", "")
	m.data.Token = ""
	m.set("device_id", "")
	m.set("scopes", "")
}

func (m *model) openServer() {
	m.pop = &popup{kind: "settings", title: "Server settings", dyn: (*model).serverItems,
		act: func(m *model, id string) tea.Cmd {
			switch id {
			case "url":
				m.openInput("Server URL", []string{"Address of your RomM server,", "e.g. https://romm.lan or http://192.168.1.20:8080"},
					m.cfg("server_url", ""), func(m *model, text string) tea.Cmd {
						u, ok := normalizeURL(text)
						if !ok {
							m.message("Invalid address", []string{"Entered: [" + text + "]", "", "Expected: http(s)://host[:port]"})
							return nil
						}
						if u != m.cfg("server_url", "") {
							m.set("server_url", u)
							// a token belongs to one server: forget it when the address changes
							m.forgetCredentials()
							return m.startPairing() // a new address means a device to pair
						}
						m.openServer()
						return m.checkServer()
					})
			case "tls":
				m.set("verify_tls", map[bool]string{true: "0", false: "1"}[m.cfg("verify_tls", "1") != "0"])
				return m.checkServer()
			case "pair":
				return m.startPairing()
			case "code":
				m.openCodePairing()
			case "test":
				return m.testConnection()
			case "forget":
				m.confirm("Disconnect", []string{"Forget the token stored on this device?", "", "It stays valid in RomM until you revoke it there."}, func(m *model) tea.Cmd {
					m.forgetCredentials()
					m.openServer()
					return m.checkServer()
				})
			default:
				m.pop = nil
			}
			return nil
		}}
}

func (m *model) settingsItems() (labels, ids []string) {
	border := "box-drawing"
	if m.th.Border[0] == "+" {
		border = "ASCII"
	}
	labels = []string{
		"Games folder: " + m.cfg("games_dir", filepath.Join(m.data.Root, "games")),
		"Auto-sync the catalog at start: " + onOff(m.cfg("autosync", "1") != "0"),
		"Platform folders (where each platform's games go)",
		"Rebuild the catalog from scratch",
		"Speed test (internet, card, server)",
		"Diagnostics",
		"Check for updates (installed: " + version + ")",
		"Borders: " + border,
		"Colors: " + m.cfg("tui_colors", "auto") + " (" + m.th.Name + ")",
	}
	ids = []string{"games", "autosync", "folders", "rebuild", "speed", "doctor", "update", "borders", "colors"}
	return
}

func (m *model) openSettings() {
	m.pop = &popup{kind: "settings", title: "Settings", dyn: (*model).settingsItems,
		act: func(m *model, id string) tea.Cmd {
			switch id {
			case "games":
				m.openInput("Games folder", []string{"Absolute path where the game folders live", "(e.g. /media/fat/games)"},
					m.cfg("games_dir", filepath.Join(m.data.Root, "games")), func(m *model, text string) tea.Cmd {
						text = strings.TrimRight(strings.TrimSpace(text), "/")
						if !strings.HasPrefix(text, "/") {
							m.message("Invalid path", []string{"The path must start with /"})
							return nil
						}
						m.set("games_dir", text)
						m.openSettings()
						return nil
					})
			case "folders":
				m.openFolders()
			case "rebuild":
				m.pop = nil
				return m.startSync(true, false)
			case "speed":
				return m.openSpeedTest()
			case "doctor":
				return m.openDoctor()
			case "update":
				return m.openUpdate()
			case "autosync":
				m.set("autosync", map[bool]string{true: "0", false: "1"}[m.cfg("autosync", "1") != "0"])
			case "borders":
				m.set("tui_ascii", map[bool]string{true: "0", false: "1"}[m.cfg("tui_ascii", "") == "1"])
				m.applyTheme()
			case "colors":
				next := map[string]string{"auto": "256", "256": "16", "16": "auto"}[m.cfg("tui_colors", "auto")]
				if next == "" {
					next = "auto"
				}
				m.set("tui_colors", next)
				m.applyTheme()
			default:
				m.pop = nil
			}
			return nil
		}}
}

// ------------------------------------------------------------ connection test

type connResultMsg struct {
	title string
	lines []string
}

func (m *model) testConnection() tea.Cmd {
	url := m.cfg("server_url", "")
	if url == "" {
		m.message("Test connection", []string{"No server address is set yet."})
		return nil
	}
	client := m.env().client
	m.pop = &popup{kind: "info", title: "Test connection", lines: []string{"Contacting " + url + " ..."}}
	return func() tea.Msg {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", url+"/api/heartbeat", nil)
		resp, err := client.Do(req)
		if err != nil {
			return connResultMsg{"Connection failed", []string{explainErr(err, clockOK())}}
		}
		defer resp.Body.Close()
		var hb struct {
			System struct {
				Version string `json:"VERSION"`
			} `json:"SYSTEM"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxJSON)).Decode(&hb)
		if resp.StatusCode != 200 {
			return connResultMsg{"Connection problem", []string{fmt.Sprintf("The server answered HTTP %d.", resp.StatusCode)}}
		}
		v := hb.System.Version
		if v == "" {
			v = "(unknown version)"
		}
		return connResultMsg{"Connected", []string{"RomM " + v, fmt.Sprintf("answered in %d ms", time.Since(start).Milliseconds())}}
	}
}

// ------------------------------------------------------------ platform folders

// openFolders lists the indexed platforms with the folder their games go to; Enter edits it.
func (m *model) openFolders() {
	items := func(m *model) (labels, ids []string) {
		e := m.env()
		for _, p := range m.data.Platforms {
			f := resolveFolder(e, p.Slug, p.Name)
			if f == "" {
				f = "(unknown: choose one)"
			}
			labels = append(labels, trunc(p.Name, 34)+"  ->  "+f)
			ids = append(ids, p.ID)
		}
		if len(labels) == 0 {
			labels, ids = []string{"No platform is indexed yet"}, []string{"none"}
		}
		return
	}
	m.pop = &popup{kind: "settings", title: "Platform folders", dyn: items,
		act: func(m *model, id string) tea.Cmd {
			p := m.platformByID(id)
			if p == nil {
				m.pop = nil
				return nil
			}
			cur := resolveFolder(m.env(), p.Slug, p.Name)
			dirs := listDirs(m.gamesDir())
			m.openInput("Folder for "+p.Name, []string{"Name of the folder inside " + m.gamesDir(), "Existing folders: " + trunc(strings.Join(dirs, " "), 60)}, cur,
				func(m *model, text string) tea.Cmd {
					text = strings.Trim(strings.TrimSpace(text), "/")
					if text == "" || strings.ContainsAny(text, "/\\") {
						m.message("Invalid folder", []string{"Use a plain folder name (no slash)."})
						return nil
					}
					mp := loadMapping(m.cfgDir())
					mp[p.Slug] = text
					if err := saveMapping(m.cfgDir(), mp); err != nil {
						m.message("Could not save", []string{err.Error()})
						return nil
					}
					m.openFolders()
					return nil
				})
			return nil
		}}
}

func listDirs(dir string) []string {
	var out []string
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				out = append(out, e.Name())
			}
		}
	}
	return out
}
