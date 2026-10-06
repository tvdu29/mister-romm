package app

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// bigModel builds a synthetic library of n games, only to measure speed (not shipped as data).
func bigModel(n int) *model {
	d := &Data{Config: map[string]string{}, Root: "/x"}
	words := strings.Fields("Super Mega Ultra Final Street Sonic Mario Zelda Metroid Castlevania Contra Donkey Kong Fighter Racing Quest Dragon Legend World Land Adventure Turbo Battle Island Master Spyro Crash")
	for p := 1; p <= 4; p++ {
		pl := &Platform{ID: strconv.Itoa(p), Name: fmt.Sprintf("Platform %d", p)}
		for i := 0; i < n/4; i++ {
			name := fmt.Sprintf("%s %s %s (USA) [%d].sfc", words[i%len(words)], words[(i*7)%len(words)], words[(i*13)%len(words)], i)
			d.Games = append(d.Games, &Game{ID: strconv.Itoa(p*1000000 + i), PID: pl.ID, Platform: pl.Name, Name: name, Low: strings.ToLower(name), Size: int64(i%9+1) << 20})
			pl.Count++
		}
		d.Platforms = append(d.Platforms, pl)
	}
	m := newModel(d, newTheme(false, "auto"))
	m.w, m.h = 80, 30 // what a 640x480 console shows
	m.focus = fRight
	m.pop, m.toast = nil, "" // first-run window and hints are not part of the measurement
	return m
}

func BenchmarkRender(b *testing.B) {
	m := bigModel(40000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.render()
	}
}

func BenchmarkRenderWithPopup(b *testing.B) {
	m := bigModel(40000)
	m.openHelp()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.render()
	}
}

func BenchmarkSearch(b *testing.B) {
	m := bigModel(40000)
	m.query = "spyro dragon"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.refilter()
	}
}
