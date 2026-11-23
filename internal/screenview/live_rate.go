package screenview

import (
	"fmt"
	"sync/atomic"
	"time"
)

// compressionLevel is one step of extra compression for a busy screen.
type compressionLevel struct {
	Quality     int
	MotionLossy bool
	MotionScale int
}

// compressionLevels go from full quality to the strongest compression. The
// controller only moves past level 0 when fewer than targetFPS frames reach
// the viewer while changes are waiting, and keeps a step only if it raises
// the frame rate.
var compressionLevels = []compressionLevel{
	{Quality: 80, MotionScale: 1},
	{Quality: 60, MotionLossy: true, MotionScale: 1},
	{Quality: 45, MotionLossy: true, MotionScale: 1},
	{Quality: 45, MotionLossy: true, MotionScale: 2},
	{Quality: 40, MotionLossy: true, MotionScale: 3},
	{Quality: 35, MotionLossy: true, MotionScale: 4},
}

// scaledLevelStart is the first level that needs FeatureScaledJPEG.
const scaledLevelStart = 3

const (
	targetFPS          = 30
	rateWindow         = time.Second
	busyFraction       = 0.6
	trialGain          = 1.15
	idleResetAfter     = 5 * time.Second
	initialCooldown    = 10 * time.Second
	maximumCooldown    = 2 * time.Minute
	downgradeHeadroom  = 1.1
	minimumTrialFrames = 2
)

type rateTrial struct {
	from, to int
	before   float64
}

// fpsController picks the compression level from the delivered frame rate.
type fpsController struct {
	level    int
	maxLevel int
	logf     func(string, ...any)

	acks        atomic.Int64
	windowStart time.Time
	busy        time.Duration
	idle        time.Duration
	lastFPS     float64

	trial        *rateTrial
	upCooldown   map[int]time.Time
	upPenalty    map[int]time.Duration
	downCooldown map[int]time.Time
	downPenalty  map[int]time.Duration
}

func newFPSController(scaledSupported bool, logf func(string, ...any)) *fpsController {
	maxLevel := len(compressionLevels) - 1
	if !scaledSupported {
		maxLevel = scaledLevelStart - 1
	}
	return &fpsController{
		maxLevel:     maxLevel,
		logf:         logf,
		upCooldown:   map[int]time.Time{},
		upPenalty:    map[int]time.Duration{},
		downCooldown: map[int]time.Time{},
		downPenalty:  map[int]time.Duration{},
	}
}

func (c *fpsController) current() compressionLevel { return compressionLevels[c.level] }

// delivered counts a frame the viewer acknowledged.
func (c *fpsController) delivered() { c.acks.Add(1) }

// observe records how long changes were waiting during dt and, once a window
// has passed, adjusts the level.
func (c *fpsController) observe(now time.Time, pending bool, dt time.Duration) {
	if c.windowStart.IsZero() {
		c.windowStart = now
		return
	}
	if pending {
		c.busy += dt
	}
	elapsed := now.Sub(c.windowStart)
	if elapsed < rateWindow {
		return
	}
	frames := c.acks.Swap(0)
	fps := float64(frames) / elapsed.Seconds()
	busy := c.busy.Seconds()/elapsed.Seconds() >= busyFraction
	c.windowStart, c.busy, c.lastFPS = now, 0, fps
	c.decide(now, fps, busy, frames)
}

func penalize(until map[int]time.Time, penalty map[int]time.Duration, key int, now time.Time) {
	next := penalty[key] * 2
	if next == 0 {
		next = initialCooldown
	}
	next = min(next, maximumCooldown)
	penalty[key] = next
	until[key] = now.Add(next)
}

func (c *fpsController) setLevel(level int, reason string) {
	if level == c.level {
		return
	}
	if c.logf != nil {
		c.logf("screen compression level %d -> %d (%s): %s", c.level, level, c.describe(level), reason)
	}
	c.level = level
}

func (c *fpsController) decide(now time.Time, fps float64, busy bool, frames int64) {
	if !busy {
		if c.trial != nil {
			// The screen calmed down mid-trial; the measurement is void.
			c.setLevel(c.trial.from, "screen stopped changing during the trial")
			c.trial = nil
		}
		c.idle += rateWindow
		if c.idle >= idleResetAfter && c.level != 0 {
			c.setLevel(0, "screen has been calm")
		}
		return
	}
	c.idle = 0
	if trial := c.trial; trial != nil {
		c.trial = nil
		if trial.to > trial.from {
			if fps >= trial.before*trialGain && fps >= trial.before+1 {
				delete(c.upPenalty, trial.to)
				if c.logf != nil {
					c.logf("screen compression level %d kept: %.1f fps, was %.1f", trial.to, fps, trial.before)
				}
				return
			}
			penalize(c.upCooldown, c.upPenalty, trial.to, now)
			c.setLevel(trial.from, fmt.Sprintf("did not raise the frame rate (%.1f fps, was %.1f)", fps, trial.before))
			return
		}
		if fps >= targetFPS {
			delete(c.downPenalty, trial.from)
			return
		}
		penalize(c.downCooldown, c.downPenalty, trial.from, now)
		c.setLevel(trial.from, fmt.Sprintf("frame rate fell to %.1f fps at the lighter level", fps))
		return
	}
	if frames < minimumTrialFrames && fps >= 1 {
		return
	}
	switch {
	case fps < targetFPS && c.level < c.maxLevel && now.After(c.upCooldown[c.level+1]):
		c.trial = &rateTrial{from: c.level, to: c.level + 1, before: fps}
		c.setLevel(c.level+1, fmt.Sprintf("trying more compression at %.1f fps", fps))
	case fps >= targetFPS*downgradeHeadroom && c.level > 0 && now.After(c.downCooldown[c.level]):
		c.trial = &rateTrial{from: c.level, to: c.level - 1, before: fps}
		c.setLevel(c.level-1, fmt.Sprintf("trying less compression at %.1f fps", fps))
	}
}

func (c *fpsController) describe(level int) string {
	value := compressionLevels[level]
	switch {
	case level == 0:
		return "full quality"
	case value.MotionScale > 1:
		return fmt.Sprintf("JPEG %d, moving areas at 1/%d resolution", value.Quality, value.MotionScale)
	default:
		return fmt.Sprintf("JPEG %d, moving areas lossy", value.Quality)
	}
}
