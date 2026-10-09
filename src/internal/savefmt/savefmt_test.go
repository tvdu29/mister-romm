package savefmt

import (
	"bytes"
	"math/rand"
	"testing"
)

func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	r.Read(b)
	return b
}

func fill(n int, v byte) []byte { return bytes.Repeat([]byte{v}, n) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// A save that went MiSTer -> raw -> MiSTer must come back byte for byte, and be "the same".
func TestRoundTripFromMister(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	psx := randBytes(r, psxCardSize)
	psx[0], psx[1] = 'M', 'C'
	gbClock := cat(fill(5*2, 0x12), fill(gbRTCBlock-10, 0xFF))
	cases := map[string][]byte{
		"SNES":      randBytes(r, 8192),
		"NES":       randBytes(r, 32768),
		"SMS":       randBytes(r, 32768),
		"PSX":       psx,
		"GAMEBOY":   cat(randBytes(r, 32768), gbClock),
		"GBA":       cat(randBytes(r, 65536), randBytes(r, 16)),
		"MegaDrive": cat(randBytes(r, 8192), fill(mdMisterSize-8192, 0xFF)),
	}
	for core, m := range cases {
		f := ForCore(core)
		raw, err := f.ToRetroArch(m)
		if err != nil {
			t.Fatalf("%s: %v", core, err)
		}
		back, err := f.ToMiSTer(raw, m)
		if err != nil {
			t.Fatalf("%s: %v", core, err)
		}
		if !bytes.Equal(back, m) {
			t.Errorf("%s: MiSTer -> raw -> MiSTer changed the file", core)
		}
		if !f.Same(m, raw) {
			t.Errorf("%s: a save and its own conversion are not the same", core)
		}
		changed := bytes.Clone(raw)
		changed[3] ^= 0xFF // a data byte on every format (index 0 is a fill byte on the Mega Drive)
		if f.Same(m, changed) {
			t.Errorf("%s: a different save is reported as the same", core)
		}
	}
}

func TestGameboyClock(t *testing.T) {
	f := ForCore("gameboy")
	ram := fill(8192, 0x42)
	clock := cat([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, fill(gbRTCBlock-10, 0xFF))
	raw, _ := f.ToRetroArch(cat(ram, clock))
	if !bytes.Equal(raw, ram) {
		t.Fatalf("the clock block must be removed for emulators, got %d bytes", len(raw))
	}
	// a new save from an emulator keeps the clock of the card
	newRAM := fill(8192, 0x43)
	got, _ := f.ToMiSTer(newRAM, cat(ram, clock))
	if !bytes.Equal(got, cat(newRAM, clock)) {
		t.Error("the clock of the card was not kept")
	}
	// no save on the card: no clock block
	if got, _ := f.ToMiSTer(newRAM, nil); len(got) != 8192 {
		t.Errorf("got %d bytes, want 8192", len(got))
	}
	// VBA-style clock data at the end of an emulator save is dropped
	if got, _ := f.ToMiSTer(cat(newRAM, fill(48, 7)), nil); !bytes.Equal(got, newRAM) {
		t.Error("VBA clock data was not removed")
	}
	// MBC2 (512 B) is not mistaken for a clock
	if got, _ := f.ToRetroArch(fill(512, 1)); len(got) != 512 {
		t.Errorf("512 B save cut to %d bytes", len(got))
	}
}

func TestGBAEepromAndRTC(t *testing.T) {
	f := ForCore("GBA")
	eeprom := fill(512, 0x55)
	m, _ := f.ToMiSTer(eeprom, nil)
	if len(m) != 8192 || !bytes.Equal(m[:512], eeprom) {
		t.Fatalf("a 512 B EEPROM must be padded to 8 KB, got %d bytes", len(m))
	}
	if !f.Same(m, eeprom) {
		t.Error("padded EEPROM should be the same as the emulator save")
	}
	// mGBA may append clock data: removed before going to the MiSTer
	if m, _ := f.ToMiSTer(cat(fill(65536, 1), fill(16, 2)), nil); len(m) != 65536 {
		t.Errorf("got %d bytes, want 65536", len(m))
	}
	// the RTC data of the card is kept
	cur := cat(fill(65536, 1), fill(32, 9))
	if m, _ := f.ToMiSTer(fill(65536, 3), cur); !bytes.Equal(m, cat(fill(65536, 3), fill(32, 9))) {
		t.Error("the RTC data of the card was not kept")
	}
}

func TestMegaDrive(t *testing.T) {
	f := ForCore("megadrive")
	data := []byte("HELLO SONIC")
	data = append(data, fill(1024-len(data), 0x20)...)
	m := cat(data, fill(mdMisterSize-len(data), 0xFF))
	raw, _ := f.ToRetroArch(m)
	if len(raw) != 2048 || raw[0] != 0 || raw[1] != 'H' || raw[3] != 'E' {
		t.Fatalf("SRAM must be byte expanded: % x", raw[:8])
	}
	// Genesis Plus GX cuts the trailing 0xFF: still the same save
	gpgx := append(bytes.Clone(raw), 0x00, 0xFF, 0x00, 0xFF)
	if !f.Same(m, gpgx) {
		t.Error("trailing 0xFF should not make the saves different")
	}
	back, _ := f.ToMiSTer(raw, nil)
	if !bytes.Equal(back, m) {
		t.Error("raw -> MiSTer differs")
	}
	// old data of the card never comes back in place of trimmed 0xFF
	old := cat(fill(4096, 0x77), fill(mdMisterSize-4096, 0xFF))
	if back, _ := f.ToMiSTer(raw, old); !bytes.Equal(back, m) {
		t.Error("bytes of the old save leaked into the new one")
	}
	// EEPROM saves are small and not expanded
	ee := fill(128, 0x31)
	if raw, _ := f.ToRetroArch(cat(ee, fill(mdMisterSize-128, 0xFF))); !bytes.Equal(raw, ee) {
		t.Errorf("EEPROM save should not be expanded, got %d bytes", len(raw))
	}
}

func TestNESKeepsCardBytesBeyondTheData(t *testing.T) {
	f := ForCore("NES")
	r := rand.New(rand.NewSource(2))
	cur := randBytes(r, 32768) // the MiSTer leaves uninitialised memory after the real 8 KB
	raw := bytes.Clone(cur[:8192])
	if !f.Same(cur, raw) {
		t.Error("an 8 KB emulator save equal to the start of the card save should be the same")
	}
	if m, _ := f.ToMiSTer(raw, nil); len(m) != 32768 || !bytes.Equal(m[:8192], raw) || m[9000] != 0 {
		t.Error("without a card save, pad with zeros to 32 KB")
	}
}

func TestRefusals(t *testing.T) {
	if ForCore("Saturn") != nil {
		t.Error("Saturn is not handled yet")
	}
	if _, err := ForCore("PSX").ToMiSTer(fill(1000, 0), nil); err == nil {
		t.Error("a file that is not a memory card must be refused")
	}
	if _, err := ForCore("SNES").ToRetroArch(nil); err == nil {
		t.Error("an empty save must be refused")
	}
	if _, err := ForCore("SNES").ToMiSTer(make([]byte, MaxSize+1), nil); err == nil {
		t.Error("a huge save must be refused")
	}
	if ForCore("SNES").Same(nil, nil) {
		t.Error("empty files are never the same save")
	}
}
