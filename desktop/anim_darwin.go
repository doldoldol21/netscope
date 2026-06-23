//go:build darwin

package main

import (
	"math"
	"time"
)

const (
	animFrameCount = 18 // frames in one wave cycle (smoothness)
	// Below this throughput the icon is considered idle: it holds the static
	// glyph instead of animating, so a quiet menu bar stays calm.
	animIdleBps = 2 * 1024.0
	animMinFPS  = 4.0  // just-barely-active crawl
	animMaxFPS  = 16.0 // saturated at high throughput
)

// startMenuBarAnimator drives the menu-bar icon. When animation is enabled and
// there's traffic, it cycles a scrolling-wave glyph at a speed that scales with
// throughput (RunCat-style: busier link → faster wave). When idle or disabled it
// shows the static glyph. The whole loop lives in Go; cgo is just the per-frame
// image swap.
func startMenuBarAnimator() {
	frames := iconFrames(animFrameCount)
	idle := statusIcon()
	go func() {
		// Let the status item exist and the first rate poll land first.
		time.Sleep(7 * time.Second)
		i := 0
		wasIdle := true
		for {
			bps, animate := currentRateBps()
			if !animate || bps < animIdleBps {
				// Park on the static glyph; only redraw on the transition so we
				// don't fight a user who turned animation off.
				if !wasIdle {
					setStatusImage(idle)
					wasIdle = true
				}
				time.Sleep(400 * time.Millisecond)
				continue
			}
			wasIdle = false
			setStatusImage(frames[i%len(frames)])
			i++
			time.Sleep(time.Duration(float64(time.Second) / fpsForRate(bps)))
		}
	}()
}

// fpsForRate maps throughput (bytes/sec) to an animation frame rate. Log-scaled
// so the wave speeds up smoothly from idle to ~10 MB/s, then saturates.
func fpsForRate(bps float64) float64 {
	const fullBps = 10 * 1024 * 1024 // throughput at which we hit animMaxFPS
	t := math.Log(bps/animIdleBps+1) / math.Log(fullBps/animIdleBps+1)
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return animMinFPS + t*(animMaxFPS-animMinFPS)
}
