// Package savefmt converts battery saves between the MiSTer format (saves/<Core>/<game>.sav) and
// the raw format of emulators (RetroArch .srm), which is the format other RomM clients upload.
//
// Only cartridge/memory card saves are handled: savestates are core dumps and cannot be converted.
// The rules follow the MiSTer cores and euan-forrester/save-file-converter (GPL-3.0), which was
// used as the reference for the MiSTer formats.
//
// Going to the MiSTer, the file that is already on the card (if any) is used as a template: the
// result keeps its size and the bytes that only the MiSTer uses (RTC block, padding), so a save
// that came from the MiSTer converts back to exactly the same bytes.
package savefmt

import (
	"bytes"
	"errors"
	"fmt"
	"math/bits"
	"strings"
)

// MaxSize is the largest save accepted on either side (a Saturn cart is 1 MB on the MiSTer).
const MaxSize = 2 << 20

// Format converts the saves of one MiSTer core.
type Format struct {
	Core     string // MiSTer core (folder of saves/)
	Verified bool   // conversion known to be right; others are refused for now
	toRaw    func(m []byte) ([]byte, error)
	toMister func(raw, cur []byte) ([]byte, error)
	same     func(m, raw []byte) bool // nil: converting raw with m as template gives m back
}

var formats = map[string]*Format{}

func register(f *Format) { formats[strings.ToUpper(f.Core)] = f }

// ForCore returns the format of a MiSTer core folder (case-insensitive), or nil when unknown.
func ForCore(core string) *Format { return formats[strings.ToUpper(core)] }

// Cores lists the cores with a known format.
func Cores() []string {
	var out []string
	for _, f := range formats {
		out = append(out, f.Core)
	}
	return out
}

var errEmpty = errors.New("empty save file")

func checkSize(b []byte) error {
	switch {
	case len(b) == 0:
		return errEmpty
	case len(b) > MaxSize:
		return fmt.Errorf("save file too large (%d bytes)", len(b))
	}
	return nil
}

// ToRetroArch converts a MiSTer save to the raw format used by emulators.
func (f *Format) ToRetroArch(mister []byte) ([]byte, error) {
	if err := checkSize(mister); err != nil {
		return nil, err
	}
	return f.toRaw(mister)
}

// ToMiSTer converts a raw emulator save to the MiSTer format. cur is the save already on the card
// (nil when there is none): it gives the size and the MiSTer-only bytes to keep.
func (f *Format) ToMiSTer(raw, cur []byte) ([]byte, error) {
	if err := checkSize(raw); err != nil {
		return nil, err
	}
	if len(cur) > MaxSize {
		cur = nil
	}
	return f.toMister(raw, cur)
}

// Same reports whether a raw save holds the same game data as the MiSTer save: converting it to
// the MiSTer format, with the MiSTer save as template, gives back exactly the MiSTer file.
func (f *Format) Same(mister, raw []byte) bool {
	if checkSize(mister) != nil || checkSize(raw) != nil {
		return false
	}
	if f.same != nil {
		return f.same(mister, raw)
	}
	m, err := f.ToMiSTer(raw, mister)
	return err == nil && bytes.Equal(m, mister)
}

// ------------------------------------------------------------------ helpers

func isPow2(n int) bool { return n > 0 && n&(n-1) == 0 }

// prevPow2 is the largest power of 2 <= n (n > 0).
func prevPow2(n int) int { return 1 << (bits.Len(uint(n)) - 1) }

func clone(b []byte) []byte { return append([]byte(nil), b...) }

// padTo extends b to size n. The bytes come from the template cur when it is long enough (the
// MiSTer keeps whatever was there), otherwise they are fill.
func padTo(b []byte, n int, cur []byte, fill byte) []byte {
	out := clone(b)
	for i := len(b); i < n; i++ {
		if i < len(cur) {
			out = append(out, cur[i])
		} else {
			out = append(out, fill)
		}
	}
	return out
}

// identity: the MiSTer file is the raw save.
func identity(m []byte) ([]byte, error) { return clone(m), nil }

// padded: same data, the MiSTer file is at least min bytes (and keeps the size of the save already
// on the card).
func padded(min int, fill byte) func(raw, cur []byte) ([]byte, error) {
	return func(raw, cur []byte) ([]byte, error) {
		n := max(len(raw), min, len(cur))
		return padTo(raw, n, cur, fill), nil
	}
}

func init() {
	// SNES: raw SRAM of the right size, no RTC (S-RTC/SPC7110 use the system clock).
	register(&Format{Core: "SNES", Verified: true, toRaw: identity,
		toMister: func(raw, cur []byte) ([]byte, error) { return clone(raw), nil }})

	// NES: an iNES ROM gives a 32 KB file whatever the real SRAM size (usually 8 KB). Emulators
	// load the large file, so it is uploaded as it is.
	register(&Format{Core: "NES", Verified: true, toRaw: identity, toMister: padded(32768, 0x00)})

	// Master System / Game Gear: like the NES, padded to 32 KB. Not verified: Genesis Plus GX may cut
	// the trailing 0xFF of the save, and what the MiSTer expects there is not known.
	register(&Format{Core: "SMS", toRaw: identity, toMister: padded(32768, 0x00)})

	// PlayStation: a raw 128 KB memory card on both sides.
	register(&Format{Core: "PSX", Verified: true, toRaw: psxCard,
		toMister: func(raw, cur []byte) ([]byte, error) { return psxCard(raw) }})

	register(&Format{Core: "GAMEBOY", Verified: true, toRaw: gbToRaw, toMister: gbToMister})
	register(&Format{Core: "GBA", Verified: true, toRaw: gbaToRaw, toMister: gbaToMister})
	register(&Format{Core: "MegaDrive", Verified: true, toRaw: mdToRaw, toMister: mdToMister, same: mdSame})
}

// ------------------------------------------------------------------ PlayStation

const psxCardSize = 128 * 1024

func psxCard(b []byte) ([]byte, error) {
	if len(b) != psxCardSize || b[0] != 'M' || b[1] != 'C' {
		return nil, fmt.Errorf("not a PlayStation memory card (%d bytes)", len(b))
	}
	return clone(b), nil
}

// ------------------------------------------------------------------ Game Boy / Game Boy Color

// The Gameboy core saves the cartridge RAM in 512-byte blocks, plus one more block for games with
// a clock (MBC3/HuC3): timestamp and saved time, the rest 0xFF. Emulators keep the clock in another
// file (.rtc), and VBA-style saves append 44 or 48 bytes of clock data instead.

const gbRTCBlock = 512

func gbHasRTC(n int) bool { return n > gbRTCBlock && isPow2(n-gbRTCBlock) }

func gbToRaw(m []byte) ([]byte, error) {
	if gbHasRTC(len(m)) {
		return clone(m[:len(m)-gbRTCBlock]), nil
	}
	return clone(m), nil
}

func gbToMister(raw, cur []byte) ([]byte, error) {
	if n := len(raw); !isPow2(n) && (isPow2(n-44) || isPow2(n-48)) {
		raw = raw[:prevPow2(n)] // VBA-style clock: not the MiSTer layout, dropped
	}
	out := clone(raw)
	if gbHasRTC(len(cur)) && len(cur)-gbRTCBlock == len(raw) {
		out = append(out, cur[len(raw):]...) // keep the clock of the card
	}
	return out, nil
}

// ------------------------------------------------------------------ Game Boy Advance

// Same data as emulators, except that the core needs at least 8 KB (it cannot tell a 512 B EEPROM
// from an 8 KB one) and may append RTC data, which makes the size not a power of 2. mGBA can also
// append its own clock data to the .srm.

func gbaToRaw(m []byte) ([]byte, error) {
	if !isPow2(len(m)) {
		return clone(m[:prevPow2(len(m))]), nil
	}
	return clone(m), nil
}

func gbaToMister(raw, cur []byte) ([]byte, error) {
	if !isPow2(len(raw)) {
		raw = raw[:prevPow2(len(raw))]
	}
	n := max(len(raw), 8192)
	if len(cur) > 0 {
		base := len(cur)
		if !isPow2(base) {
			base = prevPow2(base)
		}
		if base >= n {
			return padTo(raw, len(cur), cur, 0x00), nil // keeps the RTC data of the card
		}
	}
	return padTo(raw, n, cur, 0x00), nil
}

// ------------------------------------------------------------------ Mega Drive / Genesis

// The MiSTer stores SRAM one byte per byte, padded to 64 KB with 0xFF. Emulators (Genesis Plus GX,
// PicoDrive) store it as seen on the 16-bit bus: each byte in the low half of a word, the high half
// 0x00 ("byte expanded"), and Genesis Plus GX cuts the trailing 0xFF. Small EEPROM saves are not
// expanded on either side. Based on superg/srmtools via save-file-converter.

const (
	mdMisterSize   = 65536
	mdSmallestSRAM = 512
)

// trimEnd removes the padding at the end (a run of 0x00 or 0xFF), but never below a power of 2:
// saves are chip sized, so what looks like padding under that size is data.
func trimEnd(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	v := b[len(b)-1]
	if v != 0x00 && v != 0xFF {
		return b
	}
	n := len(b)
	for n > 0 && b[n-1] == v {
		n--
	}
	if n == 0 {
		return b // all padding: nothing to tell, keep everything
	}
	if !isPow2(n) {
		n = 1 << bits.Len(uint(n)) // next power of 2
	}
	if n > len(b) {
		n = len(b)
	}
	return b[:n]
}

func mdIsExpanded(b []byte) bool {
	for i := 0; i+1 < len(b); i += 2 {
		if hi, lo := b[i], b[i+1]; hi != lo && hi != 0x00 && hi != 0xFF {
			return false
		}
	}
	return true
}

func mdToRaw(m []byte) ([]byte, error) {
	data := trimEnd(m)
	if len(data) < mdSmallestSRAM {
		return clone(data), nil // EEPROM: not expanded
	}
	out := make([]byte, 2*len(data))
	for i, c := range data {
		out[2*i+1] = c
	}
	return out, nil
}

func mdToMister(raw, cur []byte) ([]byte, error) {
	data := raw
	if len(raw) >= mdSmallestSRAM && mdIsExpanded(raw) {
		data = make([]byte, len(raw)/2)
		for i := range data {
			data[i] = raw[2*i+1]
		}
	}
	// The padding is always 0xFF, never taken from the card: Genesis Plus GX cuts trailing 0xFF
	// that are real data, so old bytes of the card must not come back there.
	return padTo(data, max(len(data), mdMisterSize, len(cur)), nil, 0xFF), nil
}

// mdSame compares the data without the trailing 0xFF, which either side may cut or add.
func mdSame(m, raw []byte) bool {
	conv, err := mdToMister(raw, nil)
	return err == nil && bytes.Equal(trimFF(conv), trimFF(m))
}

// trimFF cuts the trailing 0xFF bytes (bytes.TrimRight works on runes, not bytes).
func trimFF(b []byte) []byte {
	n := len(b)
	for n > 0 && b[n-1] == 0xFF {
		n--
	}
	return b[:n]
}
