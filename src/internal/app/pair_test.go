package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func configText(t *testing.T, h *harness) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(h.cfgDir(), "config.ini"))
	return string(b)
}

// unpaired is a harness whose device holds no credentials yet.
func unpaired(t *testing.T, rom *fakeRomm) *harness {
	t.Helper()
	h := newHarness(t, rom)
	h.m.forgetCredentials()
	if h.m.data.Token != "" || strings.Contains(configText(t, h), "token=tok") {
		t.Fatal("setup: the device should not be paired")
	}
	return h
}

// begin starts the phone pairing and returns once the code is displayed.
func begin(t *testing.T, h *harness) {
	t.Helper()
	cmd := h.m.startPairing()
	if h.popTitle() != "Pair this device" {
		t.Fatalf("popup = %q", h.popText())
	}
	next, _ := h.m.handlePair(h.run(cmd))
	if next == nil || h.m.pair == nil {
		t.Fatalf("pairing did not start: %q", h.popText())
	}
}

// poll makes the next poll due and runs it, feeding the answer back.
func poll(t *testing.T, h *harness) tea.Cmd {
	t.Helper()
	h.m.pair.nextPoll = time.Now().Add(-time.Second)
	cmd, _ := h.m.handlePair(pairTickMsg{gen: h.m.pairGen})
	msg := h.run(cmd)
	b, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("a due poll must start a request, got %T", msg)
	}
	var next tea.Cmd
	for _, c := range b {
		if pm, ok := h.run(c).(pairPollMsg); ok {
			next, _ = h.m.handlePair(pm)
		}
	}
	return next
}

func TestPhonePairingFlow(t *testing.T) {
	rom := newFakeRomm(t)
	rom.addPlatform(1, "gb", "Game Boy", 3)
	rom.pollReplies = []string{"pending", "slow", "ok"}
	h := unpaired(t, rom)
	begin(t, h)

	lines := strings.Join(h.m.pairingLines(), "\n")
	for _, want := range []string{rom.URL() + "/device?code=ABCD-1234", "ABCD-1234", "left"} {
		if !strings.Contains(lines, want) {
			t.Errorf("pairing window lacks %q:\n%s", want, lines)
		}
	}
	if h.m.pair.interval != 5*time.Second {
		t.Errorf("interval = %v", h.m.pair.interval)
	}

	poll(t, h) // pending: keeps waiting
	if h.m.pair == nil || h.m.data.Token != "" {
		t.Fatal("pending must keep waiting")
	}
	poll(t, h) // slow_down: polls less often
	if h.m.pair.interval != 10*time.Second {
		t.Errorf("interval after slow_down = %v, want 10s", h.m.pair.interval)
	}
	h.m.pair.nextPoll = time.Now().Add(-time.Second)
	cmd, _ := h.m.handlePair(pairTickMsg{gen: h.m.pairGen})
	for _, c := range cmd().(tea.BatchMsg) {
		if pm, ok := h.run(c).(pairPollMsg); ok {
			next, _ := h.m.handlePair(pm)
			h.pump(next) // the follow-up: server check and the platform list
		}
	}

	if h.m.pair != nil || h.m.data.Token != rom.token {
		t.Fatalf("not paired: pair=%v token=%q", h.m.pair, h.m.data.Token)
	}
	ini := configText(t, h)
	for _, want := range []string{"token=" + rom.token, "device_id=dev-1", "scopes=roms.read platforms.read"} {
		if !strings.Contains(ini, want) {
			t.Errorf("config.ini lacks %q:\n%s", want, ini)
		}
	}
	if st, _ := os.Stat(filepath.Join(h.cfgDir(), "config.ini")); st.Mode().Perm() != 0o600 {
		t.Errorf("config.ini mode = %v, must stay private", st.Mode().Perm())
	}
	// with nothing indexed yet the platform picker opens straight away
	if h.popTitle() != "Platforms to index" {
		t.Errorf("popup = %q", h.popText())
	}
	if h.m.srv.state != "connected" {
		t.Errorf("server state = %q", h.m.srv.state)
	}
}

func TestPhonePairingWhenAPlatformIsAlreadyIndexed(t *testing.T) {
	rom := newFakeRomm(t)
	gb := rom.addPlatform(1, "gb", "Game Boy", 1)
	rom.pollReplies = []string{"ok"}
	h := unpaired(t, rom)
	h.selectPlatforms(gb)
	begin(t, h)
	h.pump(poll(t, h))
	if h.popTitle() != "Paired" || !strings.Contains(h.popText(), "roms.read") {
		t.Errorf("popup = %q", h.popText())
	}
}

func TestPhonePairingDenied(t *testing.T) {
	rom := newFakeRomm(t)
	rom.pollReplies = []string{"denied"}
	h := unpaired(t, rom)
	begin(t, h)
	poll(t, h)
	if h.popTitle() != "Pairing refused" || !strings.Contains(h.popText(), "access_denied") {
		t.Errorf("popup = %q", h.popText())
	}
	if h.m.pair != nil || h.m.data.Token != "" || strings.Contains(configText(t, h), "token=tok") {
		t.Error("nothing may be saved when the pairing is refused")
	}
}

func TestPhonePairingExpires(t *testing.T) {
	rom := newFakeRomm(t)
	h := unpaired(t, rom)
	begin(t, h)
	h.m.pair.deadline = time.Now().Add(-time.Second)
	h.m.handlePair(pairTickMsg{gen: h.m.pairGen})
	if h.popTitle() != "Pairing expired" || h.m.pair != nil {
		t.Errorf("popup = %q pair=%v", h.popText(), h.m.pair)
	}
}

func TestPhonePairingCancelledIgnoresLateAnswers(t *testing.T) {
	rom := newFakeRomm(t)
	h := unpaired(t, rom)
	begin(t, h)
	gen := h.m.pairGen
	h.press("esc")
	if h.m.pop != nil || h.m.pair != nil {
		t.Fatal("esc must cancel the pairing")
	}
	// an answer that was already on its way arrives afterwards
	h.m.handlePair(pairPollMsg{gen: gen, state: "ok", token: rom.token, deviceID: "dev-1", scopes: []string{"roms.read"}})
	h.m.handlePair(pairInitMsg{gen: gen})
	if h.m.data.Token != "" || strings.Contains(configText(t, h), "token=tok") {
		t.Error("a cancelled pairing must not save credentials")
	}
}

func TestPhonePairingServerProblems(t *testing.T) {
	rom := newFakeRomm(t)
	h := unpaired(t, rom)
	rom.srv.Close()
	cmd := h.m.startPairing()
	h.m.handlePair(h.run(cmd))
	if h.popTitle() != "Pairing failed" {
		t.Errorf("popup = %q", h.popText())
	}

	h2 := newHarness(t, nil) // no server address
	h2.m.startPairing()
	if !strings.Contains(h2.popText(), "Set the server address first") {
		t.Errorf("popup = %q", h2.popText())
	}
}

func TestPairingRefusesCredentialsThatCouldCorruptTheConfig(t *testing.T) {
	rom := newFakeRomm(t)
	rom.pollReplies = []string{"ok"}
	h := unpaired(t, rom)
	rom.token = "abcdefgh\ninjected=1" // what a hostile server would send
	begin(t, h)
	poll(t, h)
	if h.popTitle() != "Pairing refused" {
		t.Errorf("popup = %q", h.popText())
	}
	if strings.Contains(configText(t, h), "injected") || h.m.data.Token != "" {
		t.Errorf("a malicious token reached the configuration:\n%s", configText(t, h))
	}
}

func TestPairingWithACode(t *testing.T) {
	rom := newFakeRomm(t)
	h := unpaired(t, rom)

	h.m.openCodePairing()
	h.typeText("good-code") // lower case and dash are accepted
	h.press("enter")
	if h.m.data.Token != rom.token || !strings.Contains(configText(t, h), "token="+rom.token) {
		t.Fatalf("not paired: popup=%q", h.popText())
	}

	h2 := unpaired(t, rom)
	h2.m.openCodePairing()
	h2.typeText("WRONG-CODE")
	h2.press("enter")
	if h2.popTitle() != "Pairing failed" || !strings.Contains(h2.popText(), "invalid or expired code") || h2.m.data.Token != "" {
		t.Errorf("popup = %q", h2.popText())
	}

	for _, bad := range []string{"abc", "has space!", "ünïcode-123", strings.Repeat("A", 40)} {
		h3 := unpaired(t, rom)
		h3.m.openCodePairing()
		h3.typeText(bad)
		h3.press("enter")
		if h3.popTitle() != "Invalid code" {
			t.Errorf("%q: popup = %q", bad, h3.popText())
		}
	}
	if n := rom.count("POST /api/client-tokens/exchange"); n != 2 {
		t.Errorf("%d exchange requests, want 2 (invalid codes must not reach the server)", n)
	}
}

func TestDisconnectForgetsTheToken(t *testing.T) {
	rom := newFakeRomm(t)
	h := newHarness(t, rom)
	if h.m.data.Token == "" {
		t.Fatal("setup: should be paired")
	}
	h.m.openServer()
	h.press("down", "down", "down", "down", "down", "enter") // Disconnect
	if h.popTitle() != "Disconnect" {
		t.Fatalf("popup = %q", h.popText())
	}
	h.press("n")
	if h.m.data.Token == "" {
		t.Error("declining must keep the token")
	}
	h.m.openServer()
	h.press("down", "down", "down", "down", "down", "enter", "y")
	ini := configText(t, h)
	if h.m.data.Token != "" || strings.Contains(ini, "tok-") || strings.Contains(ini, "device_id=dev-1") {
		t.Errorf("credentials remain:\n%s", ini)
	}
	if !strings.Contains(ini, "server_url=") {
		t.Error("the server address must be kept")
	}
}

func TestChangingTheServerAddressForgetsTheTokenAndStartsPairing(t *testing.T) {
	rom := newFakeRomm(t)
	other := newFakeRomm(t)
	h := newHarness(t, rom)
	h.m.openServer()
	h.press("enter") // Server URL
	if h.popTitle() != "Server URL" {
		t.Fatalf("popup = %q", h.popText())
	}
	for range rom.URL() {
		h.press("backspace")
	}
	h.typeText(other.URL())
	h.press("enter")
	if h.m.data.Token != "" || strings.Contains(configText(t, h), "tok-") {
		t.Error("a token belongs to one server")
	}
	if h.m.cfg("server_url", "") != other.URL() {
		t.Errorf("server_url = %q", h.m.cfg("server_url", ""))
	}
	if h.popTitle() != "Pair this device" {
		t.Errorf("a new address means a device to pair; popup = %q", h.popText())
	}
}

func TestTestConnection(t *testing.T) {
	rom := newFakeRomm(t)
	h := newHarness(t, rom)
	h.pump(h.m.testConnection())
	if h.popTitle() != "Connected" || !strings.Contains(h.popText(), "RomM 5.3.1") {
		t.Errorf("popup = %q", h.popText())
	}
	rom.srv.Close()
	h.pump(h.m.testConnection())
	if h.popTitle() != "Connection failed" {
		t.Errorf("popup = %q", h.popText())
	}
	h2 := newHarness(t, nil)
	h2.pump(h2.m.testConnection())
	if !strings.Contains(h2.popText(), "No server address") {
		t.Errorf("popup = %q", h2.popText())
	}
}

// ------------------------------------------------------------------ connection state

func TestCheckServerStates(t *testing.T) {
	rom := newFakeRomm(t)
	rom.addPlatform(1, "gb", "Game Boy", 1)
	state := func(h *harness) srvStatus {
		t.Helper()
		cmd := h.m.checkServer()
		if cmd == nil {
			return h.m.srv
		}
		return h.run(cmd).(srvStatus)
	}

	h := newHarness(t, rom)
	if s := state(h); s.state != "connected" || s.version != "5.3.1" {
		t.Errorf("paired: %+v", s)
	}
	h.m.data.Token = ""
	if s := state(h); s.state != "not_paired" || s.version != "5.3.1" {
		t.Errorf("no token: %+v", s)
	}
	h.m.data.Token = "revoked-token-12345"
	if s := state(h); s.state != "unauthorized" {
		t.Errorf("rejected token: %+v", s)
	}
	if s := state(newHarness(t, nil)); s.state != "not_configured" {
		t.Errorf("no server: %+v", s)
	}
	rom.srv.Close()
	h.m.data.Token = rom.token
	if s := state(h); s.state != "unreachable" || s.err == "" {
		t.Errorf("server down: %+v", s)
	}
}

func TestStatusBarShowsTheState(t *testing.T) {
	rom := newFakeRomm(t)
	h := newHarness(t, rom)
	h.pump(h.m.checkServer())
	if !strings.Contains(h.screen(), "connected RomM 5.3.1") {
		t.Errorf("status bar:\n%s", h.screen())
	}
	h.m.data.Token = "revoked-token-12345"
	h.pump(h.m.checkServer())
	if !strings.Contains(h.screen(), "token rejected") {
		t.Errorf("status bar:\n%s", h.screen())
	}
}
