package web

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"sync"
)

// MapPlaceholderPath is where the hub serves gravel's generic map image: what a Discord server
// card shows for a map the Organization settings give no image (#97). Discord fetches it from the
// hub's public URL, so it is a plain PNG with no text, nothing of any game's.
const MapPlaceholderPath = "/static/map-placeholder.png"

// The image's size: 16:9, as wide as a card's media gallery shows it.
const (
	placeholderW = 1280
	placeholderH = 720
)

// mapPlaceholder draws the image once: a slate gradient, a faint grid, contour rings and a map
// pin in the middle. Drawn, not committed, so it is reproducible and the repository stays text.
var mapPlaceholder = sync.OnceValue(func() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, placeholderW, placeholderH))
	top, bottom := color.NRGBA{0x24, 0x2e, 0x3b, 0xff}, color.NRGBA{0x12, 0x18, 0x21, 0xff}
	grid := color.NRGBA{0x2e, 0x3a, 0x49, 0xff}
	contour := color.NRGBA{0x3a, 0x48, 0x59, 0xff}
	pin := color.NRGBA{0x6b, 0x7d, 0x92, 0xff}
	cx, cy := float64(placeholderW)/2, float64(placeholderH)/2-20
	ring := func(x, y, ox, oy, r, w float64) bool { return math.Abs(math.Hypot(x-ox, y-oy)-r) < w }
	for y := range placeholderH {
		f := float64(y) / placeholderH
		bg := color.NRGBA{lerp(top.R, bottom.R, f), lerp(top.G, bottom.G, f), lerp(top.B, bottom.B, f), 0xff}
		for x := range placeholderW {
			fx, fy := float64(x), float64(y)
			c := bg
			if x%80 == 0 || y%80 == 0 {
				c = grid
			}
			for _, r := range []float64{140, 230, 320, 410} {
				if ring(fx, fy, cx-260, cy+120, r, 1.2) || ring(fx, fy, cx+300, cy-90, r*0.8, 1.2) {
					c = contour
				}
			}
			// The pin: a ring with a dot, over a point on the ground.
			if ring(fx, fy, cx, cy, 46, 9) || math.Hypot(fx-cx, fy-cy) < 14 || ring(fx, fy, cx, cy+92, 18, 2.5) {
				c = pin
			}
			if dy := fy - (cy + 46); dy >= 0 && dy < 42 && math.Abs(fx-cx) < 22*(1-dy/42) {
				c = pin
			}
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err) // an in-memory NRGBA always encodes
	}
	return buf.Bytes()
})

func lerp(a, b uint8, f float64) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*f) }

// mapPlaceholderImage serves the image; it never changes within a build.
func mapPlaceholderImage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(mapPlaceholder())
}
