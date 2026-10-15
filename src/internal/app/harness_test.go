package app

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// ------------------------------------------------------------------ fake RomM
//
// fakeRomm is a small RomM server for the tests: platforms, paginated game lists with
// updated_after, ranged downloads, the heartbeat and both pairing flows. Faults can be injected
// (cut connections, ignored ranges, errors) to check how the program copes with a bad network.

type fakeGame struct {
	id      int
	name    string
	data    []byte
	updated time.Time
}

func (g *fakeGame) sha1() string { h := sha1.Sum(g.data); return hex.EncodeToString(h[:]) }

type fakePlatform struct {
	id         int
	slug, name string
	games      []*fakeGame
}

type fakeRomm struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	token     string
	version   string
	platforms []*fakePlatform
	reqs      []string // "METHOD /path?query" of every request, in order

	romsBody     string        // when set, the game list answers with this raw text (malformed JSON tests)
	contentDelay time.Duration // delay before a download starts (cancellation tests)
	pageDelay    time.Duration // delay before answering a game list page (cancellation tests)
	fault        string        // content fault: "", "ignore-range", "cut:N", "bad-range", "500", "404"
	faultOnce    bool          // the fault only applies to the first content request
	contentN     int           // content requests seen
	ranges       []string      // Range header of every content request

	pollReplies []string // answers of the pairing token endpoint, one per poll: pending slow denied ok
	pollN       int
}

func newFakeRomm(t *testing.T) *fakeRomm {
	r := &fakeRomm{t: t, token: "tok-0123456789abcdef", version: "5.3.1"}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRomm) URL() string { return r.srv.URL }

// addPlatform adds a platform with n games named "<Name> NNN.gb" of 1000+i bytes.
func (r *fakeRomm) addPlatform(id int, slug, name string, n int) *fakePlatform {
	p := &fakePlatform{id: id, slug: slug, name: name}
	for i := 0; i < n; i++ {
		data := bytes.Repeat([]byte{byte('a' + i%26)}, 1000+i)
		p.games = append(p.games, &fakeGame{id: id*10000 + i, name: fmt.Sprintf("%s %03d.gb", name, i),
			data: data, updated: time.Now().Add(-48 * time.Hour)})
	}
	r.mu.Lock()
	r.platforms = append(r.platforms, p)
	r.mu.Unlock()
	return p
}

func (r *fakeRomm) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reqs...)
}

func (r *fakeRomm) count(prefix string) int {
	n := 0
	for _, q := range r.requests() {
		if strings.HasPrefix(q, prefix) {
			n++
		}
	}
	return n
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (r *fakeRomm) authorized(req *http.Request) bool {
	return req.Header.Get("Authorization") == "Bearer "+r.token
}

func (r *fakeRomm) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req.Method+" "+req.URL.RequestURI())
	r.mu.Unlock()
	path := req.URL.Path
	switch {
	case path == "/api/heartbeat":
		jsonOut(w, 200, map[string]any{"SYSTEM": map[string]any{"VERSION": r.version}})
	case path == "/api/auth/device/init":
		jsonOut(w, 200, map[string]any{"device_code": "dev-code-123", "user_code": "ABCD-1234",
			"verification_path_complete": "/device?code=ABCD-1234", "expires_in": 600, "interval": 5})
	case path == "/api/auth/device/token":
		r.mu.Lock()
		reply := "pending"
		if r.pollN < len(r.pollReplies) {
			reply = r.pollReplies[r.pollN]
		}
		r.pollN++
		r.mu.Unlock()
		switch reply {
		case "ok":
			jsonOut(w, 200, map[string]any{"access_token": r.token, "device_id": "dev-1", "scopes": []string{"roms.read", "platforms.read"}})
		case "slow":
			jsonOut(w, 400, map[string]any{"detail": "slow_down"})
		case "denied":
			jsonOut(w, 400, map[string]any{"detail": "access_denied"})
		default:
			jsonOut(w, 400, map[string]any{"detail": "authorization_pending"})
		}
	case path == "/api/client-tokens/exchange":
		var in struct{ Code string }
		_ = json.NewDecoder(req.Body).Decode(&in)
		if in.Code == "GOOD-CODE" {
			jsonOut(w, 200, map[string]any{"raw_token": r.token, "scopes": []string{"roms.read"}})
			return
		}
		jsonOut(w, 404, map[string]any{"detail": "invalid or expired code"})
	case !r.authorized(req):
		jsonOut(w, 401, map[string]any{"detail": "Not authenticated"})
	case path == "/api/platforms":
		var out []map[string]any
		for _, p := range r.platforms {
			out = append(out, map[string]any{"id": p.id, "slug": p.slug, "name": p.name, "display_name": p.name, "rom_count": len(p.games)})
		}
		jsonOut(w, 200, out)
	case path == "/api/roms":
		r.serveRoms(w, req)
	case strings.HasPrefix(path, "/api/roms/") && strings.Contains(path, "/content/"):
		r.serveContent(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (r *fakeRomm) serveRoms(w http.ResponseWriter, req *http.Request) {
	if r.pageDelay > 0 {
		time.Sleep(r.pageDelay)
	}
	if r.romsBody != "" {
		_, _ = w.Write([]byte(r.romsBody))
		return
	}
	q := req.URL.Query()
	pid, _ := strconv.Atoi(q.Get("platform_ids"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	var since time.Time
	if s := q.Get("updated_after"); s != "" {
		var err error
		if since, err = time.Parse("2006-01-02T15:04:05-07:00", s); err != nil {
			jsonOut(w, 422, map[string]any{"detail": "bad updated_after " + s})
			return
		}
	}
	var matching []*fakeGame
	for _, p := range r.platforms {
		if p.id == pid {
			for _, g := range p.games {
				if since.IsZero() || g.updated.After(since) {
					matching = append(matching, g)
				}
			}
		}
	}
	items := []map[string]any{}
	for i := offset; i < len(matching) && i < offset+limit; i++ {
		g := matching[i]
		items = append(items, map[string]any{"id": g.id, "fs_name": g.name, "fs_size_bytes": len(g.data), "sha1_hash": g.sha1()})
	}
	jsonOut(w, 200, map[string]any{"items": items, "total": len(matching)})
}

func (r *fakeRomm) serveContent(w http.ResponseWriter, req *http.Request) {
	rest := strings.TrimPrefix(req.URL.Path, "/api/roms/")
	idS, _, _ := strings.Cut(rest, "/")
	id, _ := strconv.Atoi(idS)
	var game *fakeGame
	for _, p := range r.platforms {
		for _, g := range p.games {
			if g.id == id {
				game = g
			}
		}
	}
	r.mu.Lock()
	r.contentN++
	n := r.contentN
	r.ranges = append(r.ranges, req.Header.Get("Range"))
	fault := r.fault
	if r.faultOnce && n > 1 {
		fault = ""
	}
	r.mu.Unlock()
	if game == nil {
		http.NotFound(w, req)
		return
	}
	if r.contentDelay > 0 {
		select {
		case <-time.After(r.contentDelay):
		case <-req.Context().Done():
			return
		}
	}
	switch {
	case fault == "500":
		jsonOut(w, 500, map[string]any{"detail": "internal error"})
	case fault == "404":
		jsonOut(w, 404, map[string]any{"detail": "file not found on disk"})
	case fault == "ignore-range":
		w.Header().Set("Content-Length", strconv.Itoa(len(game.data)))
		_, _ = w.Write(game.data)
	case fault == "bad-range":
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 5-%d/%d", len(game.data)-1, len(game.data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(game.data)-5))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(game.data[5:])
	case strings.HasPrefix(fault, "cut:"):
		cut, _ := strconv.Atoi(strings.TrimPrefix(fault, "cut:"))
		start := 0
		if rg := req.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(game.data)-1, len(game.data)))
			w.Header().Set("Content-Length", strconv.Itoa(len(game.data)-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(game.data)))
		}
		end := min(start+cut, len(game.data))
		_, _ = w.Write(game.data[start:end])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drop the connection in the middle of the body
	default:
		http.ServeContent(w, req, game.name, time.Time{}, bytes.NewReader(game.data))
	}
}

// ------------------------------------------------------------------ UI harness

// harness drives the real model the way Bubble Tea does: keys in, commands run, messages fed back.
type harness struct {
	t    *testing.T
	m    *model
	root string
	rom  *fakeRomm
	quit bool
}

// newHarness builds a model on a temporary card, paired with the fake server (when given).
func newHarness(t *testing.T, rom *fakeRomm) *harness {
	t.Helper()
	root := t.TempDir()
	if rom != nil {
		cfg := filepath.Join(root, "Scripts", ".config", "romm")
		if err := os.MkdirAll(cfg, 0o700); err != nil {
			t.Fatal(err)
		}
		ini := "server_url=" + rom.URL() + "\ntoken=" + rom.token + "\ndevice_id=dev-1\nscopes=roms.read\nautosync=0\n"
		if err := os.WriteFile(filepath.Join(cfg, "config.ini"), []byte(ini), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := newModel(loadData(root), newTheme(true, "auto"))
	m.pop, m.toast = nil, "" // first-run hints are tested on their own
	m.w, m.h = 100, 30
	return &harness{t: t, m: m, root: root, rom: rom}
}

func (h *harness) cfgDir() string { return h.m.cfgDir() }

// selectPlatforms stores the platform selection the way the platform picker does.
func (h *harness) selectPlatforms(ps ...*fakePlatform) {
	sel := map[string]*selPlatform{}
	for _, p := range ps {
		sel[strconv.Itoa(p.id)] = &selPlatform{Slug: p.slug, Name: p.name}
	}
	if err := writeSelection(h.cfgDir(), sel); err != nil {
		h.t.Fatal(err)
	}
}

var specialKeys = map[string]rune{
	"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab, "up": tea.KeyUp, "down": tea.KeyDown,
	"left": tea.KeyLeft, "right": tea.KeyRight, "backspace": tea.KeyBackspace, "pgup": tea.KeyPgUp,
	"pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd, "space": tea.KeySpace,
}

func keyMsg(k string) tea.KeyPressMsg {
	if k == "ctrl+c" {
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	if c, ok := specialKeys[k]; ok {
		if k == "space" {
			return tea.KeyPressMsg{Code: c, Text: " "}
		}
		return tea.KeyPressMsg{Code: c}
	}
	r := []rune(k)
	if len(r) != 1 {
		panic("keyMsg: unknown key " + k)
	}
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

// press sends keys one by one, running the commands they produce.
func (h *harness) press(keys ...string) {
	h.t.Helper()
	for _, k := range keys {
		_, cmd := h.m.Update(keyMsg(k))
		h.pump(cmd)
	}
}

// typeText sends text to a search or input field, one character at a time.
func (h *harness) typeText(s string) {
	h.t.Helper()
	for _, r := range s {
		h.press(string(r))
	}
}

// pump runs commands and feeds their messages back until nothing is left to do.
func (h *harness) pump(cmd tea.Cmd) {
	h.t.Helper()
	queue := []tea.Cmd{cmd}
	deadline := time.Now().Add(20 * time.Second)
	for len(queue) > 0 {
		if time.Now().After(deadline) {
			h.t.Fatal("pump: the commands never finished")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := h.run(c)
		switch x := msg.(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, x...)
		case tea.QuitMsg:
			h.quit = true
		case pairTickMsg:
			// the pairing clock ticks forever: the tests that care drive it by hand
		default:
			_, next := h.m.Update(msg)
			queue = append(queue, next)
		}
	}
}

func (h *harness) run(c tea.Cmd) tea.Msg {
	h.t.Helper()
	ch := make(chan tea.Msg, 1)
	go func() { ch <- c() }()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(15 * time.Second):
		h.t.Fatal("a command is blocked")
		return nil
	}
}

// screen renders the whole interface as plain text.
func (h *harness) screen() string { return stripANSI(h.m.render()) }

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (h *harness) popTitle() string {
	if h.m.pop == nil {
		return ""
	}
	return h.m.pop.title
}

func (h *harness) popText() string {
	if h.m.pop == nil {
		return ""
	}
	return h.m.pop.title + "\n" + strings.Join(h.m.pop.lines, "\n")
}

// testEnv is an env for the code that does not need a model.
func testEnv(t *testing.T, rom *fakeRomm) env {
	t.Helper()
	dir := t.TempDir()
	e := env{cfgDir: filepath.Join(dir, "cfg"), gamesDir: filepath.Join(dir, "games"), root: dir}
	if rom != nil {
		e.base, e.token = rom.URL(), rom.token
		e.client, e.longClient = rom.srv.Client(), rom.srv.Client()
	}
	return e
}
