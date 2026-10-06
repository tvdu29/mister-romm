package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// The files below are shared with the former shell script (romm.sh), so an existing card keeps working.

type stateRom struct {
	Path     string   `json:"path"`
	Size     int64    `json:"size"`
	Reasons  []string `json:"reasons,omitempty"`
	Excluded bool     `json:"excluded,omitempty"`
}

type selPlatform struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	SyncedAt string `json:"synced_at,omitempty"`
}

func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readState(cfgDir string) map[string]*stateRom {
	var st struct {
		Roms map[string]*stateRom `json:"roms"`
	}
	if b, err := os.ReadFile(filepath.Join(cfgDir, "state.json")); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Roms == nil {
		st.Roms = map[string]*stateRom{}
	}
	return st.Roms
}

func writeState(cfgDir string, roms map[string]*stateRom) error {
	b, _ := json.MarshalIndent(map[string]any{"roms": roms}, "", "  ")
	return writeFileAtomic(filepath.Join(cfgDir, "state.json"), b, 0o644)
}

func readSelection(cfgDir string) map[string]*selPlatform {
	var sel struct {
		Platforms map[string]*selPlatform `json:"platforms"`
	}
	if b, err := os.ReadFile(filepath.Join(cfgDir, "selection.json")); err == nil {
		_ = json.Unmarshal(b, &sel)
	}
	if sel.Platforms == nil {
		sel.Platforms = map[string]*selPlatform{}
	}
	return sel.Platforms
}

func writeSelection(cfgDir string, p map[string]*selPlatform) error {
	b, _ := json.MarshalIndent(map[string]any{"platforms": p}, "", "  ")
	return writeFileAtomic(filepath.Join(cfgDir, "selection.json"), b, 0o644)
}

func loadMapping(cfgDir string) map[string]string {
	mp := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(cfgDir, "mapping.json")); err == nil {
		_ = json.Unmarshal(b, &mp)
	}
	return mp
}

func saveMapping(cfgDir string, mp map[string]string) error {
	b, _ := json.MarshalIndent(mp, "", "  ")
	return writeFileAtomic(filepath.Join(cfgDir, "mapping.json"), b, 0o644)
}

// defaultFolders maps a RomM platform slug to the MiSTer game folder (core name). The folder names
// exist on a standard card; the slugs were checked against a real RomM 5.3.1 server.
var defaultFolders = map[string]string{
	"nes": "NES", "famicom": "NES",
	"snes": "SNES", "sfam": "SNES", "super-nintendo": "SNES",
	"genesis": "MegaDrive", "megadrive": "MegaDrive", "mega-drive": "MegaDrive", "genesis-slash-megadrive": "MegaDrive",
	"psx": "PSX", "ps": "PSX", "playstation": "PSX",
	"saturn": "Saturn",
	"gb":     "GAMEBOY", "gameboy": "GAMEBOY",
	"gbc": "GBC", "gba": "GBA",
	"sms": "SMS", "mastersystem": "SMS",
	"n64": "N64",
}

// existingDir finds a folder of games/ by name, ignoring case.
func existingDir(gamesDir, want string) string {
	ents, err := os.ReadDir(gamesDir)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if e.IsDir() && strings.EqualFold(e.Name(), want) {
			return e.Name()
		}
	}
	return ""
}

// resolveFolder returns the folder name for a platform, or "" when none is known.
func resolveFolder(e env, slug, name string) string {
	cand := e.mapping[slug]
	if cand == "" {
		cand = defaultFolders[slug]
	}
	if cand != "" {
		if d := existingDir(e.gamesDir, cand); d != "" {
			return d
		}
		return cand
	}
	if d := existingDir(e.gamesDir, slug); d != "" {
		return d
	}
	return existingDir(e.gamesDir, name)
}

// freeSpace returns the free bytes of the filesystem holding dir (or its nearest existing parent).
func freeSpace(dir string) (int64, bool) {
	for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			var s syscall.Statfs_t
			if syscall.Statfs(d, &s) == nil {
				return int64(s.Bavail) * int64(s.Bsize), true
			}
			return 0, false
		}
	}
	return 0, false
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

func urlPath(name string) string { return (&url.URL{Path: name}).EscapedPath() }
