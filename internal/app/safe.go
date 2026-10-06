package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// Everything that comes from the server is untrusted: names become file names, text goes to the
// terminal, tokens go to config.ini. The helpers below are the only way these values are accepted.

// safeName reports whether n can be used as ONE file or folder name: no separator, no control
// character (newlines would corrupt the catalog, escape sequences would drive the terminal), and
// not "." or "..".
func safeName(n string) bool {
	if n == "" || n == "." || n == ".." || len(n) > 255 {
		return false
	}
	for _, r := range n {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// clean removes control characters from text received from the server before it is displayed,
// so that it cannot move the cursor, change colors or spoof the interface.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// within reports whether p is base itself or lies inside it (purely lexical).
func within(base, p string) bool {
	if base == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

var (
	tokenRe = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{8,1000}$`)
	idRe    = regexp.MustCompile(`^[A-Za-z0-9._:-]{0,128}$`)
	scopeRe = regexp.MustCompile(`^[a-z0-9._:-]{1,64}$`)
)

// checkCredentials refuses a token, device id or scope list that could not be a real RomM answer
// (in particular anything with a newline, which would inject lines into config.ini).
func checkCredentials(token, deviceID string, scopes []string) error {
	if !tokenRe.MatchString(token) {
		return fmt.Errorf("the server sent a token in an unexpected format")
	}
	if !idRe.MatchString(deviceID) {
		return fmt.Errorf("the server sent a device id in an unexpected format")
	}
	for _, s := range scopes {
		if !scopeRe.MatchString(s) {
			return fmt.Errorf("the server sent a permission in an unexpected format")
		}
	}
	return nil
}

// removeGameFile deletes a file this program downloaded. The target must be a regular file that
// really lies inside gamesDir once symlinks are resolved: a tampered state.json or a symlink in
// the games folder cannot make it delete anything else.
func removeGameFile(gamesDir, path string) error {
	baseReal, err := filepath.EvalSymlinks(gamesDir)
	if err != nil {
		return err
	}
	dirReal, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // already gone
		}
		return err
	}
	target := filepath.Join(dirReal, filepath.Base(path))
	if !within(baseReal, target) || target == baseReal {
		return fmt.Errorf("the file is outside the games folder")
	}
	fi, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	return os.Remove(target)
}
