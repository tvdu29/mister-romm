package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorReport(t *testing.T) {
	rom := newFakeRomm(t)
	rom.addPlatform(1, "gb", "Game Boy", 2)
	rom.addPlatform(2, "snes", "Super Nintendo", 2)
	h := newHarness(t, rom)
	h.pump(h.m.openDoctor())
	report := strings.Join(h.m.pop.lines, "\n")
	for _, want := range []string{"mister-romm", "Free space:", "[ok]     reachable", "[ok]     token accepted, 2 platforms", "Games in the catalog: 0"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if h.m.pop.running {
		t.Error("the window should show that the report is complete")
	}
	if strings.Contains(report, rom.token) {
		t.Error("the report must never print the token")
	}

	// a rejected token and an unreachable server are told apart
	h.m.data.Token = "revoked-token-12345"
	h.pump(h.m.openDoctor())
	if r := strings.Join(h.m.pop.lines, "\n"); !strings.Contains(r, "[FAILED] token") {
		t.Errorf("report:\n%s", r)
	}
	rom.srv.Close()
	h.pump(h.m.openDoctor())
	if r := strings.Join(h.m.pop.lines, "\n"); !strings.Contains(r, "[FAILED] reachable") {
		t.Errorf("report:\n%s", r)
	}

	h2 := newHarness(t, nil)
	h2.pump(h2.m.openDoctor())
	if r := strings.Join(h2.m.pop.lines, "\n"); !strings.Contains(r, "no server address set") || !strings.Contains(r, "not paired") && !strings.Contains(r, "no server") {
		t.Errorf("report:\n%s", r)
	}
}

func TestDoctorShowsRecentErrors(t *testing.T) {
	h := newHarness(t, nil)
	for i := 0; i < 12; i++ {
		logError(h.cfgDir(), "failure number "+string(rune('A'+i)))
	}
	h.pump(h.m.openDoctor())
	r := strings.Join(h.m.pop.lines, "\n")
	if !strings.Contains(r, "last-error.log") || !strings.Contains(r, "failure number L") || strings.Contains(r, "failure number A") {
		t.Errorf("only the recent errors belong in the report:\n%s", r)
	}
}

func TestToolsRefuseToRunTwiceAtOnce(t *testing.T) {
	h := newHarness(t, nil)
	h.m.toolJob = &job{cancel: func() {}}
	h.pump(h.m.openDoctor())
	if h.m.toast != "Another test is already running" {
		t.Errorf("toast = %q", h.m.toast)
	}
	h.m.toast = ""
	h.m.openSpeedTest()
	if h.m.toast != "Another test is already running" {
		t.Errorf("toast = %q", h.m.toast)
	}
}

func TestSpeedTestAsksBeforeUsingTheNetwork(t *testing.T) {
	h := newHarness(t, nil)
	h.m.openSpeedTest()
	if h.popTitle() != "Speed test" || !strings.Contains(h.popText(), "speed.cloudflare.com") || !strings.Contains(h.popText(), "Nothing is saved") {
		t.Errorf("popup = %q", h.popText())
	}
	h.press("n")
	if h.m.toolJob != nil || h.m.pop != nil {
		t.Error("declining must not start anything")
	}
}

func TestWrongClockOffersToContinueWithoutCertificateChecks(t *testing.T) {
	rom := newFakeRomm(t)
	h := newHarness(t, rom)
	h.pump(h.m.finishClock(clockResult{ok: true}))
	if h.m.srv.state != "connected" || h.m.sessionInsecure {
		t.Errorf("state=%q insecure=%v", h.m.srv.state, h.m.sessionInsecure)
	}

	h2 := newHarness(t, rom)
	h2.m.finishClock(clockResult{ok: false})
	if h2.popTitle() != "Clock not set" || h2.m.srv.state != "clock" {
		t.Fatalf("popup = %q state=%q", h2.popText(), h2.m.srv.state)
	}
	h2.press("n")
	if h2.m.sessionInsecure {
		t.Error("declining must keep the certificate checks")
	}
	h2.m.finishClock(clockResult{ok: false})
	h2.press("y")
	if !h2.m.sessionInsecure || !h2.m.insecure() {
		t.Error("accepting turns the checks off for this session")
	}
	if h2.m.cfg("verify_tls", "1") != "1" {
		t.Error("the setting on disk must not change: it is for this session only")
	}
	if strings.Contains(configText(t, h2), "verify_tls=0") {
		t.Error("config.ini must not record the session choice")
	}
}

func TestInitChecksTheServerWhenTheClockIsRight(t *testing.T) {
	if !clockOK() {
		t.Skip("this machine's clock is wrong")
	}
	rom := newFakeRomm(t)
	h := newHarness(t, rom)
	h.pump(h.m.Init())
	if h.m.srv.state != "connected" {
		t.Errorf("state = %q", h.m.srv.state)
	}
	if h.m.clockJob != nil {
		t.Error("no clock job is needed")
	}
}

func TestAutoSyncRunsOnceWhenConnected(t *testing.T) {
	rom := newFakeRomm(t)
	gb := rom.addPlatform(1, "gb", "Game Boy", 3)
	h := newHarness(t, rom)
	h.m.data.Config["autosync"] = "1"
	h.selectPlatforms(gb)
	h.pump(h.m.checkServer())
	if len(h.m.data.Games) != 3 || !h.m.autosyncDone {
		t.Errorf("games=%d autosyncDone=%v", len(h.m.data.Games), h.m.autosyncDone)
	}
	n := rom.count("GET /api/roms?")
	h.pump(h.m.checkServer())
	if rom.count("GET /api/roms?") != n {
		t.Error("the automatic sync must run only once per session")
	}
	h2 := newHarness(t, rom)
	h2.m.data.Config["autosync"] = "0"
	h2.selectPlatforms(gb)
	h2.pump(h2.m.checkServer())
	if len(h2.m.data.Games) != 0 {
		t.Error("autosync is off")
	}
}

func TestDefaultRoot(t *testing.T) {
	t.Setenv("ROMM_ROOT", "/some/where")
	if got := defaultRoot(); got != "/some/where" {
		t.Errorf("defaultRoot = %q", got)
	}
	t.Setenv("ROMM_ROOT", "")
	if got := defaultRoot(); got == "" {
		t.Error("defaultRoot must always give a place to store things")
	}
}

func TestWritable(t *testing.T) {
	if !writable(filepath.Join(t.TempDir(), "a", "b")) {
		t.Error("a new folder under a writable one is writable")
	}
	f := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(f, nil, 0o644)
	if writable(filepath.Join(f, "sub")) {
		t.Error("nothing can be created under a regular file")
	}
}

func TestGameLookups(t *testing.T) {
	h, _ := syncedHarness(t, "gb", "Game Boy", 2)
	if p := h.m.platformByID("1"); p == nil || p.Name != "Game Boy" || h.m.platformByID("zz") != nil {
		t.Error("platformByID")
	}
	if dateNow() == "" {
		t.Error("dateNow")
	}
	_ = isTerminal(os.Stdin) // either answer is fine, it must not fail
}
