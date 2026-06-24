//go:build darwin

package main

import (
	"math"
	"time"
)

const (
	animFrames = 18 // frames in one wave cycle (smoothness)
	// Below this throughput the icon is idle: it shows the lowest-amplitude
	// gentle wave at a slow crawl, so a quiet menu bar stays calm.
	animIdleBps = 2 * 1024.0
	animFullBps = 8 * 1024 * 1024.0 // throughput at which speed/amplitude saturate
	animMinFPS  = 3.0
	animMaxFPS  = 12.0
)

// animLevels are the wave amplitudes (px) from quiet to saturated. The animator
// picks a level from current throughput so the icon's *size* — not just its
// speed — tracks traffic, making the mapping legible at a glance. Ten steps
// (vs the original five) keep the amplitude from visibly popping as traffic
// ramps; the frames are pre-rendered once so the extra levels are nearly free.
var animLevels = buildAnimLevels(10, 1.5, 7.5)

func buildAnimLevels(n int, lo, hi float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = lo + (hi-lo)*float64(i)/float64(n-1)
	}
	return out
}

// startMenuBarAnimator drives the menu-bar icon: a scrolling wave whose amplitude
// and speed both rise with throughput (busier link → taller, faster wave), and
// which rests on the static glyph when disabled. The loop is all Go; cgo is just
// the per-frame image swap.
func startMenuBarAnimator() {
	// Pre-render every amplitude level's cycle once.
	sets := make([][][]byte, len(animLevels))
	for i, amp := range animLevels {
		sets[i] = iconFrameSet(animFrames, amp)
	}
	idle := statusIcon()
	go func() {
		time.Sleep(7 * time.Second) // let the status item + first poll land
		frame := 0
		wasOff := false
		for {
			// Display asleep: stop swapping frames (icon isn't visible). Poll
			// cheaply until it wakes.
			if !menuBarAnimationActive() {
				time.Sleep(time.Second)
				continue
			}
			bps, animate := currentRateBps()
			if !animate {
				if !wasOff {
					setStatusImage(idle)
					wasOff = true
				}
				time.Sleep(400 * time.Millisecond)
				continue
			}
			wasOff = false
			t := intensity(bps) // 0..1
			set := sets[int(math.Round(t*float64(len(sets)-1)))]
			setStatusImage(set[frame%len(set)])
			frame++
			fps := animMinFPS + t*(animMaxFPS-animMinFPS)
			time.Sleep(time.Duration(float64(time.Second) / fps))
		}
	}()
}

// intensity maps throughput (bytes/sec) to a 0..1 activity level, log-scaled
// between idle and saturation so the wave ramps smoothly across everyday rates.
func intensity(bps float64) float64 {
	if bps <= animIdleBps {
		return 0
	}
	t := math.Log(bps/animIdleBps+1) / math.Log(animFullBps/animIdleBps+1)
	if t > 1 {
		t = 1
	}
	return t
}
