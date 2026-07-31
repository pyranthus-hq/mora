// Package appbundle generates the deterministic macOS app icon for Mora.app.
//
// The one and only icon source is the committed pixel-art eye at
// docs/assets/mora-eye.svg. That file is a flat list of axis-aligned unit
// <rect> cells on an integer grid, so it can be parsed and re-rasterized
// exactly — no SVG engine, no font, no external tool. The renderer composes
// the art onto a macOS rounded-square icon tile and encodes a multi-size
// .icns container using only the standard library, so the same input bytes
// and toolchain always produce the same output bytes.
package appbundle

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"
)

// Background of the rounded-square tile. The deepest color of the committed
// palette (the pupil), so the tile stays inside the artwork's own palette
// instead of inventing a new brand color.
var iconBackground = color.NRGBA{R: 0x04, G: 0x11, B: 0x0d, A: 0xff}

// PixelArt is the rasterized source artwork: a W×H grid of palette cells.
// Cells without a rect stay nil (transparent — the tile background shows).
type PixelArt struct {
	W, H int
	grid []*color.NRGBA
}

// At returns the source cell color and whether the cell is painted.
func (a *PixelArt) At(x, y int) (color.NRGBA, bool) {
	if x < 0 || y < 0 || x >= a.W || y >= a.H {
		return color.NRGBA{}, false
	}
	c := a.grid[y*a.W+x]
	if c == nil {
		return color.NRGBA{}, false
	}
	return *c, true
}

// svgDoc mirrors only what the pixel-art file uses: a viewBox and unit rects.
type svgDoc struct {
	ViewBox string    `xml:"viewBox,attr"`
	Rects   []svgRect `xml:"rect"`
}

type svgRect struct {
	X      string `xml:"x,attr"`
	Y      string `xml:"y,attr"`
	Width  string `xml:"width,attr"`
	Height string `xml:"height,attr"`
	Fill   string `xml:"fill,attr"`
}

// ParsePixelSVG rasterizes the pixel-art SVG into a grid, painting rects in
// document order (SVG painter's model). It fails closed on anything the
// committed artwork does not use: a non-origin viewBox, fractional or
// out-of-bounds rects, or a fill that is not #rrggbb.
func ParsePixelSVG(data []byte) (*PixelArt, error) {
	var doc svgDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing pixel-art SVG: %w", err)
	}
	fields := strings.Fields(doc.ViewBox)
	if len(fields) != 4 || fields[0] != "0" || fields[1] != "0" {
		return nil, fmt.Errorf("pixel-art SVG viewBox must be \"0 0 W H\", got %q", doc.ViewBox)
	}
	w, errW := strconv.Atoi(fields[2])
	h, errH := strconv.Atoi(fields[3])
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return nil, fmt.Errorf("pixel-art SVG viewBox must have positive integer size, got %q", doc.ViewBox)
	}
	if len(doc.Rects) == 0 {
		return nil, fmt.Errorf("pixel-art SVG contains no rect cells")
	}
	art := &PixelArt{W: w, H: h, grid: make([]*color.NRGBA, w*h)}
	for i, r := range doc.Rects {
		x, err := atoiAttr(r.X, "x", i)
		if err != nil {
			return nil, err
		}
		y, err := atoiAttr(r.Y, "y", i)
		if err != nil {
			return nil, err
		}
		rw, err := atoiAttr(r.Width, "width", i)
		if err != nil {
			return nil, err
		}
		rh, err := atoiAttr(r.Height, "height", i)
		if err != nil {
			return nil, err
		}
		if x < 0 || y < 0 || rw <= 0 || rh <= 0 || x+rw > w || y+rh > h {
			return nil, fmt.Errorf("rect %d (%d,%d %dx%d) escapes the %dx%d grid", i, x, y, rw, rh, w, h)
		}
		c, err := parseHexColor(r.Fill)
		if err != nil {
			return nil, fmt.Errorf("rect %d: %w", i, err)
		}
		for dy := 0; dy < rh; dy++ {
			for dx := 0; dx < rw; dx++ {
				cc := c
				art.grid[(y+dy)*w+(x+dx)] = &cc
			}
		}
	}
	return art, nil
}

func atoiAttr(v, name string, rect int) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("rect %d: attribute %s=%q is not an integer", rect, name, v)
	}
	return n, nil
}

func parseHexColor(s string) (color.NRGBA, error) {
	if len(s) != 7 || s[0] != '#' {
		return color.NRGBA{}, fmt.Errorf("fill %q is not #rrggbb", s)
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return color.NRGBA{}, fmt.Errorf("fill %q is not #rrggbb", s)
	}
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}, nil
}

// Tile geometry, in integer per-mille of the canvas so every size derives
// from the same ratios. These follow Apple's macOS icon grid: the rounded
// square occupies the canvas minus a ~9.8% margin per side, with a corner
// radius of ~22.5% of the square's side.
const (
	tileInsetPerMille  = 98  // canvas margin on each side
	tileRadiusPerMille = 225 // corner radius, relative to the square side
	artBoxPerMille     = 760 // art content box, relative to the square side
	subSamples         = 4   // 4x4 supersampling for the rounded corners
)

// RenderIcon draws one square icon tile of the given size: an anti-aliased
// rounded square in the palette's deepest color with the pixel art centered
// on it, scaled by nearest neighbor so the pixel edges stay crisp.
func RenderIcon(art *PixelArt, size int) (*image.NRGBA, error) {
	if size < 16 {
		return nil, fmt.Errorf("icon size %d is below the minimum of 16", size)
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	inset := (size*tileInsetPerMille + 500) / 1000
	side := size - 2*inset
	radius := (side*tileRadiusPerMille + 500) / 1000
	drawRoundedSquare(img, inset, side, radius)

	// Fit the art into the content box, preserving aspect. Prefer an integer
	// scale factor (pixel-perfect); fall back to nearest-neighbor resampling
	// only when the tile is too small for even 1:1.
	box := (side*artBoxPerMille + 500) / 1000
	destW, destH := fitArt(art.W, art.H, box)
	ox := (size - destW) / 2
	oy := (size - destH) / 2
	for py := 0; py < destH; py++ {
		sy := py * art.H / destH
		for px := 0; px < destW; px++ {
			sx := px * art.W / destW
			if c, ok := art.At(sx, sy); ok {
				img.SetNRGBA(ox+px, oy+py, c)
			}
		}
	}
	return img, nil
}

func fitArt(srcW, srcH, box int) (int, int) {
	f := box / srcW
	if fh := box / srcH; fh < f {
		f = fh
	}
	if f >= 1 {
		return srcW * f, srcH * f
	}
	// Tiny tiles: scale down preserving aspect.
	destW, destH := box, box*srcH/srcW
	if destH > box {
		destH, destW = box, box*srcW/srcH
	}
	if destW < 1 {
		destW = 1
	}
	if destH < 1 {
		destH = 1
	}
	return destW, destH
}

// drawRoundedSquare fills a rounded square with iconBackground, anti-aliasing
// the corner boundary with fixed supersampling. All sampling is deterministic:
// the same size always yields the same coverage, hence the same bytes.
func drawRoundedSquare(img *image.NRGBA, inset, side, radius int) {
	lo, hi := float64(inset), float64(inset+side)
	r := float64(radius)
	for y := inset; y < inset+side; y++ {
		for x := inset; x < inset+side; x++ {
			covered := 0
			for sy := 0; sy < subSamples; sy++ {
				py := float64(y) + (float64(sy)+0.5)/subSamples
				for sx := 0; sx < subSamples; sx++ {
					px := float64(x) + (float64(sx)+0.5)/subSamples
					if insideRoundedRect(px, py, lo, hi, r) {
						covered++
					}
				}
			}
			if covered == 0 {
				continue
			}
			c := iconBackground
			c.A = uint8((covered*255 + subSamples*subSamples/2) / (subSamples * subSamples))
			img.SetNRGBA(x, y, c)
		}
	}
}

func insideRoundedRect(px, py, lo, hi, r float64) bool {
	if px < lo || px > hi || py < lo || py > hi {
		return false
	}
	var cx, cy float64
	switch {
	case px < lo+r && py < lo+r:
		cx, cy = lo+r, lo+r
	case px > hi-r && py < lo+r:
		cx, cy = hi-r, lo+r
	case px < lo+r && py > hi-r:
		cx, cy = lo+r, hi-r
	case px > hi-r && py > hi-r:
		cx, cy = hi-r, hi-r
	default:
		return true
	}
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= r*r
}

// icnsChunk maps an ICNS type code to the pixel size of its embedded PNG.
// The order is fixed — it is part of the deterministic output contract.
var icnsChunks = []struct {
	code string
	size int
}{
	{"icp4", 16},   // 16x16
	{"icp5", 32},   // 32x32
	{"ic07", 128},  // 128x128
	{"ic08", 256},  // 256x256
	{"ic09", 512},  // 512x512
	{"ic10", 1024}, // 512x512@2x
	{"ic11", 32},   // 16x16@2x
	{"ic12", 64},   // 32x32@2x
	{"ic13", 256},  // 128x128@2x
	{"ic14", 512},  // 256x256@2x
}

// GenerateICNS renders the full multi-resolution .icns container from the
// pixel-art SVG bytes. Same input, same toolchain — same output bytes.
func GenerateICNS(svg []byte) ([]byte, error) {
	art, err := ParsePixelSVG(svg)
	if err != nil {
		return nil, err
	}
	pngBySize := map[int][]byte{}
	for _, ch := range icnsChunks {
		if _, done := pngBySize[ch.size]; done {
			continue
		}
		img, err := RenderIcon(art, ch.size)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("encoding %dpx PNG: %w", ch.size, err)
		}
		pngBySize[ch.size] = buf.Bytes()
	}

	var body bytes.Buffer
	for _, ch := range icnsChunks {
		data := pngBySize[ch.size]
		body.WriteString(ch.code)
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(8+len(data)))
		body.Write(n[:])
		body.Write(data)
	}

	var out bytes.Buffer
	out.WriteString("icns")
	var total [4]byte
	binary.BigEndian.PutUint32(total[:], uint32(8+body.Len()))
	out.Write(total[:])
	out.Write(body.Bytes())
	return out.Bytes(), nil
}
