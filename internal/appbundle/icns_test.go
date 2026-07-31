package appbundle

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func readEyeSVG(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "assets", "mora-eye.svg"))
	if err != nil {
		t.Fatalf("reading committed pixel-art source: %v", err)
	}
	return data
}

func TestParsePixelSVGCommittedArt(t *testing.T) {
	art, err := ParsePixelSVG(readEyeSVG(t))
	if err != nil {
		t.Fatal(err)
	}
	if art.W != 30 || art.H != 25 {
		t.Fatalf("grid = %dx%d, want 30x25", art.W, art.H)
	}
	// Ground-truth cells from docs/assets/mora-eye.svg.
	cases := []struct {
		x, y    int
		want    color.NRGBA
		painted bool
	}{
		{14, 9, color.NRGBA{0x04, 0x11, 0x0d, 0xff}, true},  // pupil
		{15, 12, color.NRGBA{0x04, 0x11, 0x0d, 0xff}, true}, // pupil
		{14, 3, color.NRGBA{0xbf, 0xf5, 0xe6, 0xff}, true},  // highlight
		{13, 3, color.NRGBA{0x13, 0x3c, 0x32, 0xff}, true},  // dark iris ring
		{10, 0, color.NRGBA{0x2f, 0xbf, 0x9a, 0xff}, true},  // outer border
		{0, 0, color.NRGBA{}, false},                        // transparent corner
		{29, 24, color.NRGBA{}, false},                      // transparent corner
	}
	for _, tc := range cases {
		got, ok := art.At(tc.x, tc.y)
		if ok != tc.painted || (ok && got != tc.want) {
			t.Errorf("At(%d,%d) = (%v, %v), want (%v, %v)", tc.x, tc.y, got, ok, tc.want, tc.painted)
		}
	}
}

func TestParsePixelSVGFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		svg  string
	}{
		{"no rects", `<svg viewBox="0 0 30 25"></svg>`},
		{"bad viewBox origin", `<svg viewBox="1 0 30 25"><rect x="0" y="0" width="1" height="1" fill="#000000"/></svg>`},
		{"rect escapes grid", `<svg viewBox="0 0 30 25"><rect x="29" y="0" width="2" height="1" fill="#000000"/></svg>`},
		{"fractional rect", `<svg viewBox="0 0 30 25"><rect x="0.5" y="0" width="1" height="1" fill="#000000"/></svg>`},
		{"non-hex fill", `<svg viewBox="0 0 30 25"><rect x="0" y="0" width="1" height="1" fill="teal"/></svg>`},
		{"not xml", `this is not svg`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePixelSVG([]byte(tc.svg)); err == nil {
				t.Fatal("expected a fail-closed parse error")
			}
		})
	}
}

func TestGenerateICNSDeterministic(t *testing.T) {
	svg := readEyeSVG(t)
	first, err := GenerateICNS(svg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateICNS(svg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("GenerateICNS is not byte-deterministic across runs")
	}
}

func TestGenerateICNSStructure(t *testing.T) {
	icns, err := GenerateICNS(readEyeSVG(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(icns) < 8 || string(icns[:4]) != "icns" {
		t.Fatal("output does not start with the icns magic")
	}
	if got := binary.BigEndian.Uint32(icns[4:8]); int(got) != len(icns) {
		t.Fatalf("icns header length = %d, want %d", got, len(icns))
	}
	want := []struct {
		code string
		size int
	}{
		{"icp4", 16}, {"icp5", 32}, {"ic07", 128}, {"ic08", 256}, {"ic09", 512},
		{"ic10", 1024}, {"ic11", 32}, {"ic12", 64}, {"ic13", 256}, {"ic14", 512},
	}
	off := 8
	for _, w := range want {
		if off+8 > len(icns) {
			t.Fatalf("icns truncated before chunk %s", w.code)
		}
		code := string(icns[off : off+4])
		length := int(binary.BigEndian.Uint32(icns[off+4 : off+8]))
		if code != w.code {
			t.Fatalf("chunk = %s, want %s", code, w.code)
		}
		if off+length > len(icns) || length <= 8 {
			t.Fatalf("chunk %s has invalid length %d", code, length)
		}
		img, err := png.Decode(bytes.NewReader(icns[off+8 : off+length]))
		if err != nil {
			t.Fatalf("chunk %s does not hold a valid PNG: %v", code, err)
		}
		b := img.Bounds()
		if b.Dx() != w.size || b.Dy() != w.size {
			t.Fatalf("chunk %s PNG is %dx%d, want %dx%d", code, b.Dx(), b.Dy(), w.size, w.size)
		}
		off += length
	}
	if off != len(icns) {
		t.Fatalf("icns has %d trailing bytes after the last chunk", len(icns)-off)
	}
}

// TestRenderIconComposition pins the 1024px tile geometry: transparent
// outside the rounded square, palette background inside it, and crisp
// (unblended) nearest-neighbor pixel-art edges at exact destination
// coordinates derived from the frozen layout math.
func TestRenderIconComposition(t *testing.T) {
	art, err := ParsePixelSVG(readEyeSVG(t))
	if err != nil {
		t.Fatal(err)
	}
	img, err := RenderIcon(art, 1024)
	if err != nil {
		t.Fatal(err)
	}

	// inset=100, side=824, radius=185, art box=626 → integer scale 20,
	// dest art 600x500 at origin (212, 262).
	for _, p := range []struct{ x, y int }{{0, 0}, {1023, 0}, {0, 1023}, {1023, 1023}, {50, 50}} {
		if a := img.NRGBAAt(p.x, p.y).A; a != 0 {
			t.Errorf("pixel (%d,%d) outside the rounded square has alpha %d, want 0", p.x, p.y, a)
		}
	}
	bg := color.NRGBA{0x04, 0x11, 0x0d, 0xff}
	if got := img.NRGBAAt(512, 150); got != bg {
		t.Errorf("tile background = %v, want palette background %v", got, bg)
	}
	// Transparent source cell over the tile shows the background through.
	if got := img.NRGBAAt(215, 270); got != bg {
		t.Errorf("transparent art cell = %v, want background %v", got, bg)
	}
	// Source (14,3)=#bff5e6 maps to dest x 492..511, y 322..341; its left
	// neighbor (13,3)=#133c32 ends at x 491. A hard edge proves crispness.
	light := color.NRGBA{0xbf, 0xf5, 0xe6, 0xff}
	dark := color.NRGBA{0x13, 0x3c, 0x32, 0xff}
	if got := img.NRGBAAt(492, 330); got != light {
		t.Errorf("art pixel (492,330) = %v, want %v", got, light)
	}
	if got := img.NRGBAAt(491, 330); got != dark {
		t.Errorf("art pixel (491,330) = %v, want %v", got, dark)
	}
	// Pupil center retains the exact palette color.
	if got := img.NRGBAAt(212+14*20+10, 262+10*20+10); got != bg {
		t.Errorf("pupil = %v, want %v", got, bg)
	}
}

func TestRenderIconTinySizes(t *testing.T) {
	art, err := ParsePixelSVG(readEyeSVG(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{16, 32, 64} {
		img, err := RenderIcon(art, size)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
			t.Fatalf("size %d: bounds %v", size, b)
		}
	}
	if _, err := RenderIcon(art, 8); err == nil {
		t.Fatal("sizes below 16 must be refused")
	}
}
