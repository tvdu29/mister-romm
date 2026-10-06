package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Game is one entry of the local catalog (an index of what the RomM server has).
type Game struct {
	ID        string
	PID       string // platform id
	Platform  string // platform name
	Name      string
	Low       string // lower-cased name, for searching
	Size      int64
	SHA1      string
	Installed bool
	Path      string
}

// Key identifies a game across platforms.
func (g *Game) Key() string { return g.PID + ":" + g.ID }

// Platform is a platform that is indexed in the catalog.
type Platform struct {
	ID, Slug, Name string
	Count          int
}

// Data is everything the UI shows.
type Data struct {
	Games     []*Game
	Platforms []*Platform
	Config    map[string]string // config.ini without the token
	Token     string            // used only to check the connection; never displayed
	Root      string
}

func humanSize(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1fGB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1fMB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.0fKB", float64(n)/k)
	}
	return fmt.Sprintf("%dB", n)
}

// loadData reads the files written by this program (Scripts/.config/romm): the catalog synced from the
// RomM server, the selected platforms and the list of games already downloaded. There is no demo
// data: without a catalog the interface shows how to create one.
func loadData(root string) *Data {
	d := &Data{Config: map[string]string{}, Root: root}
	cfgDir := filepath.Join(root, "Scripts", ".config", "romm")

	// config.ini (the token is never loaded)
	if f, err := os.Open(filepath.Join(cfgDir, "config.ini")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, v, ok := strings.Cut(sc.Text(), "="); ok {
				if k == "token" {
					d.Token = v
				} else {
					d.Config[k] = v
				}
			}
		}
		f.Close()
	}

	{
		var sel struct {
			Platforms map[string]struct{ Slug, Name string } `json:"platforms"`
		}
		if b, err := os.ReadFile(filepath.Join(cfgDir, "selection.json")); err == nil {
			_ = json.Unmarshal(b, &sel)
		}
		var st struct {
			Roms map[string]struct {
				Path string `json:"path"`
				Size int64  `json:"size"`
			} `json:"roms"`
		}
		if b, err := os.ReadFile(filepath.Join(cfgDir, "state.json")); err == nil {
			_ = json.Unmarshal(b, &st)
		}
		pids := make([]string, 0, len(sel.Platforms))
		for pid := range sel.Platforms {
			pids = append(pids, pid)
		}
		sort.Slice(pids, func(i, j int) bool { return sel.Platforms[pids[i]].Name < sel.Platforms[pids[j]].Name })
		for _, pid := range pids {
			p := sel.Platforms[pid]
			f, err := os.Open(filepath.Join(cfgDir, "catalog", pid+".tsv"))
			if err != nil {
				continue
			}
			pl := &Platform{ID: pid, Slug: p.Slug, Name: p.Name}
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				// id SEP name SEP size SEP sha1, parsed without allocating a slice per line
				line := sc.Text()
				id, rest, ok1 := strings.Cut(line, "\x1f")
				name, rest, ok2 := strings.Cut(rest, "\x1f")
				sizeS, sha, _ := strings.Cut(rest, "\x1f")
				if !ok1 || !ok2 || !safeName(name) {
					continue
				}
				size, _ := strconv.ParseInt(sizeS, 10, 64)
				g := &Game{ID: id, PID: pid, Platform: p.Name, Name: name, Low: strings.ToLower(name), Size: size, SHA1: sha}
				if r, ok := st.Roms[g.ID]; ok && r.Path != "" {
					g.Installed, g.Path = true, r.Path
				}
				d.Games = append(d.Games, g)
				pl.Count++
			}
			f.Close()
			d.Platforms = append(d.Platforms, pl)
		}
	}
	sort.Slice(d.Platforms, func(i, j int) bool { return d.Platforms[i].Name < d.Platforms[j].Name })
	return d
}
