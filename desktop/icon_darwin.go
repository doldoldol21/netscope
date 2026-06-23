//go:build darwin

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// statusIcon draws the menu-bar template glyph: a small activity wave. macOS
// renders a template image adaptively for light/dark menu bars.
func statusIcon() []byte {
	const w, h = 40, 22
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	black := color.RGBA{0, 0, 0, 255}
	// a simple sparkline: flat — up — down spike — up — flat
	pts := [][2]int{{4, 13}, {11, 13}, {15, 6}, {20, 18}, {25, 9}, {29, 13}, {36, 13}}
	for i := 0; i+1 < len(pts); i++ {
		drawLine(img, pts[i][0], pts[i][1], pts[i+1][0], pts[i+1][1], black)
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// iconWaveCanvas matches statusIcon's proportions so the idle and animated icons
// line up. The wave uses an integer number of wavelengths across the drawable
// width so it scrolls seamlessly (no edge jump on wrap).
const (
	iconW, iconH = 40, 22
	iconWaves    = 2 // wavelengths across the width → seamless tiling
)

// iconFrameSet renders an animation cycle of a scrolling sine wave at a given
// amplitude: n template PNGs (macOS tints them for light/dark). Higher amplitude
// = a taller, livelier wave, which the animator ties to throughput so the icon
// visibly reacts to traffic rather than only changing speed.
func iconFrameSet(n int, amp float64) [][]byte {
	mid := float64(iconH) / 2.0
	period := float64(iconW) / float64(iconWaves)
	black := color.RGBA{0, 0, 0, 255}
	frames := make([][]byte, n)
	for f := 0; f < n; f++ {
		phase := float64(f) / float64(n) // 0..1 over the cycle
		img := image.NewRGBA(image.Rect(0, 0, iconW, iconH))
		yAt := func(x int) int {
			return int(math.Round(mid + amp*math.Sin(2*math.Pi*(float64(x)/period+phase))))
		}
		px, py := 2, yAt(2)
		for x := 3; x <= iconW-2; x++ {
			y := yAt(x)
			drawLine(img, px, py, x, y, black)
			px, py = x, y
		}
		var buf bytes.Buffer
		_ = png.Encode(&buf, img)
		frames[f] = buf.Bytes()
	}
	return frames
}

// drawLine draws a thick (3px) line between two points.
func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	dx, dy := abs(x1-x0), abs(y1-y0)
	sx, sy := sign(x1-x0), sign(y1-y0)
	err := dx - dy
	for {
		for ox := -1; ox <= 1; ox++ {
			for oy := -1; oy <= 1; oy++ {
				img.Set(x0+ox, y0+oy, c)
			}
		}
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

func sign(a int) int {
	switch {
	case a > 0:
		return 1
	case a < 0:
		return -1
	default:
		return 0
	}
}
