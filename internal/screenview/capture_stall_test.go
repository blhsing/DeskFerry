package screenview

import (
	"testing"
	"time"
)

func TestDXGIStallDetectorTellsAStillScreenFromAStalledCapture(t *testing.T) {
	var d dxgiStallDetector
	start := time.Now()
	d.started(start)
	if d.useGDI(start.Add(time.Second)) {
		t.Fatal("probed before Desktop Duplication was quiet for long")
	}
	// A still screen: probes now and then, finds nothing, stays on DXGI.
	probes := 0
	for ms := 2000; ms < 6000; ms += 50 {
		if d.useGDI(start.Add(time.Duration(ms) * time.Millisecond)) {
			probes++
			if d.sampled(false) {
				t.Fatal("a still screen was taken for a stall")
			}
		}
	}
	if probes < 6 || probes > 9 {
		t.Fatalf("%d probes in 4 s of a still screen; want about two a second", probes)
	}
	// The desktop changed without Desktop Duplication noticing: stalled.
	now := start.Add(7 * time.Second)
	if !d.useGDI(now) || !d.sampled(true) {
		t.Fatal("a GDI change while Desktop Duplication was quiet was not taken for a stall")
	}
	for ms := 0; ms < 200; ms += 50 {
		if !d.useGDI(now.Add(time.Duration(ms) * time.Millisecond)) {
			t.Fatal("a stalled capture must use GDI every time")
		}
	}
	if !d.frame(now.Add(time.Second)) {
		t.Fatal("a Desktop Duplication frame after a stall must report recovery")
	}
	if d.useGDI(now.Add(time.Second + 100*time.Millisecond)) {
		t.Fatal("GDI was used right after Desktop Duplication recovered")
	}
}
