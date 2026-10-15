package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Long sessions on a MiSTer must not grow: goroutines, idle connections and the heap are checked
// after many repetitions of the operations a user does all day.

func settle() {
	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
}

func heapInUse() uint64 {
	settle()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapInuse
}

func TestRepeatedWorkDoesNotAccumulateGoroutinesOrMemory(t *testing.T) {
	rom := newFakeRomm(t)
	gb := rom.addPlatform(1, "gb", "Game Boy", 30)
	h := newHarness(t, rom)
	h.selectPlatforms(gb)
	round := func() {
		h.press("s")                 // sync
		h.pump(h.m.checkServer())    // status check
		h.pump(h.m.testConnection()) // connection test
		h.press("p", "esc")          // platform list
	}
	for i := 0; i < 3; i++ { // warm up: pools, caches
		round()
	}
	g0, m0 := runtime.NumGoroutine(), heapInUse()
	const rounds = 40
	for i := 0; i < rounds; i++ {
		round()
	}
	g1, m1 := runtime.NumGoroutine(), heapInUse()
	t.Logf("goroutines %d -> %d, heap %d KiB -> %d KiB after %d rounds", g0, g1, m0>>10, m1>>10, rounds)
	if g1 > g0+8 {
		t.Errorf("goroutines grew from %d to %d: connections or jobs are not released", g0, g1)
	}
	if m1 > m0+8<<20 {
		t.Errorf("heap grew from %d to %d KiB", m0>>10, m1>>10)
	}
}

func TestDownloadsAndDeletionsDoNotLeak(t *testing.T) {
	h, _ := syncedHarness(t, "gb", "Game Boy", 20)
	round := func(i int) {
		h.m.sel = map[string]bool{}
		h.press("home", "d", "y") // download the first game not yet installed
		h.press("esc")
		for _, g := range h.m.data.Games {
			if g.Installed {
				h.m.deleteGame(g)
			}
		}
		_ = strconv.Itoa(i)
	}
	for i := 0; i < 3; i++ {
		round(i)
	}
	g0, m0 := runtime.NumGoroutine(), heapInUse()
	for i := 0; i < 40; i++ {
		round(i)
	}
	g1, m1 := runtime.NumGoroutine(), heapInUse()
	t.Logf("goroutines %d -> %d, heap %d KiB -> %d KiB, queue length %d", g0, g1, m0>>10, m1>>10, len(h.m.queue))
	if g1 > g0+8 {
		t.Errorf("goroutines grew from %d to %d", g0, g1)
	}
	if m1 > m0+8<<20 {
		t.Errorf("heap grew from %d to %d KiB", m0>>10, m1>>10)
	}
	// the queue keeps every finished entry for the session (about 100 bytes each): a known, harmless growth
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd on this system")
	}
	return len(ents)
}

func TestCancelledJobsDoNotLeak(t *testing.T) {
	rom := newFakeRomm(t)
	gb := rom.addPlatform(1, "gb", "Game Boy", 60)
	h := newHarness(t, rom)
	h.selectPlatforms(gb)
	rom.set(func() { rom.pageDelay = 150 * time.Millisecond })
	cancelSync := func() {
		_, cmd := h.m.Update(keyMsg("s"))
		time.Sleep(30 * time.Millisecond)
		h.m.Update(keyMsg("c"))
		h.pump(cmd)
		if h.m.syncJob != nil {
			t.Fatal("the cancelled sync is still registered")
		}
	}
	for i := 0; i < 3; i++ {
		cancelSync()
	}
	g0, f0 := runtime.NumGoroutine(), openFDs(t)
	for i := 0; i < 25; i++ {
		cancelSync()
	}
	settle()
	g1, f1 := runtime.NumGoroutine(), openFDs(t)
	t.Logf("goroutines %d -> %d, open files %d -> %d after 25 cancelled syncs", g0, g1, f0, f1)
	if g1 > g0+8 || f1 > f0+8 {
		t.Errorf("a cancelled sync leaves resources behind: goroutines %d -> %d, files %d -> %d", g0, g1, f0, f1)
	}

	// the same for cancelled downloads
	rom.set(func() { rom.pageDelay, rom.contentDelay = 0, 5*time.Second })
	h.press("s")
	h.press("down", "tab")
	cancelDl := func() {
		h.m.sel = map[string]bool{}
		h.press("home")
		_, cmd := h.m.Update(keyMsg("d"))
		h.pump(cmd)
		_, cmd = h.m.Update(keyMsg("y"))
		time.Sleep(30 * time.Millisecond)
		h.m.Update(keyMsg("esc"))
		h.m.Update(keyMsg("c"))
		h.pump(cmd)
		if h.m.dlJob != nil {
			t.Fatal("the cancelled download is still registered")
		}
	}
	cancelDl()
	g0, f0 = runtime.NumGoroutine(), openFDs(t)
	for i := 0; i < 15; i++ {
		cancelDl()
	}
	settle()
	g1, f1 = runtime.NumGoroutine(), openFDs(t)
	t.Logf("goroutines %d -> %d, open files %d -> %d after 15 cancelled downloads", g0, g1, f0, f1)
	if g1 > g0+8 || f1 > f0+8 {
		t.Errorf("a cancelled download leaves resources behind: goroutines %d -> %d, files %d -> %d", g0, g1, f0, f1)
	}
	// and no half file is kept as a game
	left, _ := filepath.Glob(filepath.Join(h.root, "games", "*", "*.gb"))
	if len(left) != 0 {
		t.Errorf("cancelled downloads left games on the card: %v", left)
	}
}

func TestFailedUpdatesDoNotLeak(t *testing.T) {
	bin := builtProgram(t, "v9.9.9")
	srv, rel := fakeGitHub(t, ghOpts{tag: "v9.9.9", bin: bin, sums: strings.Repeat("ab", 32)}) // always refused
	dest := installed(t, "old-binary")
	attempt := func() {
		if staged, _, err := runUpd(t, srv, rel, dest); err == nil {
			os.Remove(staged)
			t.Fatal("expected a refusal")
		}
	}
	attempt()
	g0, f0, tmp0 := runtime.NumGoroutine(), openFDs(t), leftovers()
	for i := 0; i < 8; i++ {
		attempt()
	}
	settle()
	g1, f1, tmp1 := runtime.NumGoroutine(), openFDs(t), leftovers()
	t.Logf("goroutines %d -> %d, open files %d -> %d, /tmp copies %d -> %d after 8 refused updates", g0, g1, f0, f1, tmp0, tmp1)
	if g1 > g0+8 || f1 > f0+8 || tmp1 != tmp0 {
		t.Errorf("refused updates leak: goroutines %d -> %d, files %d -> %d, /tmp copies %d -> %d", g0, g1, f0, f1, tmp0, tmp1)
	}
}
