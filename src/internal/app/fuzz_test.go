package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// Plain `go test` runs the seeds below as regression tests; `go test -fuzz=FuzzName` explores.
// Everything here parses data that comes from the network or from files on the card.

func FuzzSafeName(f *testing.F) {
	for _, s := range []string{"", ".", "..", "a/b", `a\b`, "ok.gb", "evil\x1b[2J", "nul\x00", "ünï.gb", "a\nb", strings.Repeat("a", 300)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !safeName(s) {
			return
		}
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) || len(s) > 255 {
			t.Fatalf("safeName accepted %q", s)
		}
		for _, r := range s {
			if unicode.IsControl(r) {
				t.Fatalf("safeName accepted a control character in %q", s)
			}
		}
		// a safe name always stays inside the folder it is joined to
		base := filepath.Join("games", "GB")
		if !within(base, filepath.Join(base, s)) {
			t.Fatalf("%q escapes its folder", s)
		}
	})
}

func FuzzClean(f *testing.F) {
	for _, s := range []string{"", "plain", "a\x1b[31mred", "tab\there", "\x00\x07\x08", "line\r\nbreak", "ünï 🎮"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := clean(s)
		for _, r := range out {
			if unicode.IsControl(r) {
				t.Fatalf("clean(%q) = %q still has a control character", s, out)
			}
		}
		if clean(out) != out {
			t.Fatalf("clean is not idempotent on %q", s)
		}
	})
}

func FuzzWithin(f *testing.F) {
	f.Add("/media/fat/games", "/media/fat/games/GB/x.gb")
	f.Add("/media/fat/games", "/media/fat/games/../../etc")
	f.Add("", "/etc")
	f.Add("a", "a/../b")
	f.Fuzz(func(t *testing.T, base, p string) {
		if !within(base, p) {
			return
		}
		rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(p))
		if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			t.Fatalf("within(%q, %q) is true but the relative path is %q", base, p, rel)
		}
	})
}

func FuzzParseVersion(f *testing.F) {
	for _, s := range []string{"v1.2.3", "1.2.3", "v1.2", "dev", "", "v1.2.3-rc1", "v-1.2.3", "v1.2.3.4", "v99999999999999999999.0.0", "dev-abc1234"} {
		f.Add(s, "v1.0.0")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		_ = newerVersion(a, b)
		if newerVersion(a, b) && newerVersion(b, a) {
			t.Fatalf("%q and %q are each newer than the other", a, b)
		}
		if newerVersion(a, a) {
			t.Fatalf("%q is newer than itself", a)
		}
	})
}

func FuzzParseSums(f *testing.F) {
	h := strings.Repeat("ab", 32)
	for _, s := range []string{"", h + "  file\n", h + " *file\n", "short  file", h + "  a  b", "\x00\n\n" + h + "\tfile"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for name, sum := range parseSums(s) {
			if len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
				t.Fatalf("parseSums returned %q for %q", sum, name)
			}
		}
	})
}

func FuzzCatalogFile(f *testing.F) {
	us := "\x1f"
	for _, s := range []string{"", "1" + us + "Game.gb" + us + "10" + us + "aa\n", "x" + us + "y\n", "1" + us + "../bad" + us + "1" + us + "\n", strings.Repeat("9"+us, 50), "\n\n\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p := filepath.Join(t.TempDir(), "1.tsv")
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Skip()
		}
		for id, r := range readCatalog(p) {
			if !safeName(r.name) || id == "" || strings.Trim(id, "0123456789") != "" {
				t.Fatalf("readCatalog accepted %+v", r)
			}
		}
	})
}

func FuzzNormalizeURL(f *testing.F) {
	for _, s := range []string{"", "romm.lan", "http://192.168.1.2:8080/", "https://x//", "ftp://x", "has space", "  trim  ", "http://", "://"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, ok := normalizeURL(s)
		if !ok {
			return
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") || strings.HasSuffix(u, "/") || strings.ContainsAny(u, " \t") {
			t.Fatalf("normalizeURL(%q) = %q", s, u)
		}
	})
}

func FuzzDetail(f *testing.F) {
	for _, s := range []string{"", `{"detail":"x"}`, `{"detail":{"a":1}}`, "<html>", "\x00", `{"detail":null}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got := detail(s); len(got) > 400 {
			t.Fatalf("detail returned %d characters", len(got))
		}
	})
}

func FuzzCheckCredentials(f *testing.F) {
	f.Add("abcdefgh12345", "dev-1", "roms.read")
	f.Add("abc\ndef12345", "", "x")
	f.Add("", "", "")
	f.Fuzz(func(t *testing.T, token, device, scope string) {
		if checkCredentials(token, device, []string{scope}) != nil {
			return
		}
		// accepted values can never add a line to config.ini
		for _, v := range []string{token, device, scope} {
			if strings.ContainsAny(v, "\r\n\x00") {
				t.Fatalf("accepted %q", v)
			}
		}
	})
}
