// Package app is mister-romm: a terminal interface to browse a RomM server and download games on a MiSTer.
//
// Left panel: menu and platforms. Right panel: the synced catalog, always selectable and
// searchable. Pop-ups (confirmations, context menus, downloads, settings) are drawn over it.
// It does everything on its own: pairing, catalog sync, downloads, delete, diagnostics.
package app

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"

	tea "charm.land/bubbletea/v2"
)

// version is the build version (set by Run).
var version = "dev"

// Run starts the program; v is the build version printed by -version.
func Run(v string) {
	version = v
	root := flag.String("root", "", "data root: settings live in <root>/Scripts/.config/romm and games in <root>/games (default: /media/fat on a MiSTer, else ~/.config/mister-romm)")
	ascii := flag.Bool("ascii", os.Getenv("ROMM_ASCII") != "", "ASCII borders instead of box-drawing characters")
	ver := flag.Bool("version", false, "print the version")
	flag.Parse()
	if *ver {
		fmt.Println("mister-romm-tui", version)
		return
	}

	removeOwnCopy()

	// Tuned for a dual-core 800 MHz Cortex-A9 with a slow framebuffer console: few redraws per
	// second, and a lazier garbage collector (the catalog is a few MB, memory is not the limit).
	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(400)

	if *root == "" {
		*root = defaultRoot()
	}
	data := loadData(*root)
	useASCII := *ascii || data.Config["tui_ascii"] == "1"
	colors := data.Config["tui_colors"]
	if colors == "" {
		colors = "auto"
	}
	m := newModel(data, newTheme(useASCII, colors))

	// Use stdin/stdout directly instead of opening /dev/tty: the MiSTer starts scripts through
	// agetty on tty2, which provides a terminal on stdin even where /dev/tty is not available.
	p := tea.NewProgram(m, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout), tea.WithFPS(20))
	final, err := p.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mister-romm-tui:", err)
		os.Exit(1)
	}
	// an update was installed: restart into it (the terminal has been restored by now)
	if fm, ok := final.(*model); ok && fm.relaunch != "" {
		err := relaunch(fm.relaunch)
		_ = os.Remove(fm.relaunch)
		fmt.Fprintln(os.Stderr, "mister-romm-tui: update installed, restart it from the Scripts menu:", err)
	}
}

// defaultRoot is the MiSTer card (/media/fat) when it exists and can be written, otherwise a folder
// in the user's configuration directory, so that settings and the catalog are always saved
// somewhere (on a PC /media/fat does not exist).
func defaultRoot() string {
	if r := os.Getenv("ROMM_ROOT"); r != "" {
		return r
	}
	if st, err := os.Stat("/media/fat"); err == nil && st.IsDir() && writable("/media/fat/Scripts/.config/romm") {
		return "/media/fat"
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "mister-romm")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".mister-romm")
	}
	return "/media/fat"
}

// writable reports whether files can be created in dir (creating it if needed).
func writable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	return os.Remove(name) == nil
}
