package screenview

import "time"

const (
	// dxgiQuietBeforeProbe is how long Desktop Duplication may deliver
	// nothing before GDI samples check whether the screen really is still.
	dxgiQuietBeforeProbe = 2 * time.Second
	// dxgiProbeInterval spaces those samples on a still screen.
	dxgiProbeInterval = 500 * time.Millisecond
)

// dxgiStallDetector tells a still screen from Desktop Duplication that has
// stopped delivering frames, as it does on a laptop with the lid closed while
// the desktop keeps changing (Windows stops presenting to a display that is
// off). Desktop Duplication reports nothing in both cases; a GDI sample that
// differs from the last frame shows the screen changed without it.
type dxgiStallDetector struct {
	lastFrame time.Time
	lastProbe time.Time
	stalled   bool
}

// frame records a Desktop Duplication frame; it reports whether capture
// was stalled until now.
func (d *dxgiStallDetector) frame(now time.Time) (recovered bool) {
	d.lastFrame = now
	recovered = d.stalled
	d.stalled = false
	return recovered
}

// started resets the quiet period for a new duplication.
func (d *dxgiStallDetector) started(now time.Time) {
	d.lastFrame = now
}

// useGDI reports whether to take a GDI frame now, after Desktop Duplication
// delivered nothing: always while stalled, otherwise as a periodic probe once
// it has been quiet for a while.
func (d *dxgiStallDetector) useGDI(now time.Time) bool {
	if d.stalled {
		return true
	}
	if now.Sub(d.lastFrame) < dxgiQuietBeforeProbe || now.Sub(d.lastProbe) < dxgiProbeInterval {
		return false
	}
	d.lastProbe = now
	return true
}

// sampled records a GDI frame taken while Desktop Duplication was quiet; it
// reports whether capture has just been found stalled.
func (d *dxgiStallDetector) sampled(changed bool) (nowStalled bool) {
	if changed && !d.stalled {
		d.stalled = true
		return true
	}
	return false
}
