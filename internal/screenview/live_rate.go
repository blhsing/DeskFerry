package screenview

import (
	"fmt"
	"sync/atomic"
	"time"
)

// compressionLevel is one step of extra compression for a busy screen.
type compressionLevel struct {
	// Lossless sends every tile losslessly (PNG-style).
	Lossless    bool
	Quality     int
	MotionLossy bool
	MotionScale int
	// Video sends the region in motion as an H.264 stream.
	Video bool
}

// compressionLevels go from full quality to the strongest compression. The
// controller only moves past level 0 when fewer than targetFPS frames reach
// the viewer while changes are waiting, and keeps a step only if it raises
// the frame rate.
var compressionLevels = []compressionLevel{
	{Lossless: true, MotionScale: 1},
	{Quality: 80, MotionScale: 1},
	{Quality: 60, MotionLossy: true, MotionScale: 1},
	{Quality: 45, MotionLossy: true, MotionScale: 1},
	{Quality: 45, MotionLossy: true, MotionScale: 1, Video: true},
	{Quality: 45, MotionLossy: true, MotionScale: 2},
	{Quality: 40, MotionLossy: true, MotionScale: 3},
	{Quality: 35, MotionLossy: true, MotionScale: 4},
}

// DefaultMinFPS is the frame rate compression aims for unless a viewer sets
// another; MaxMinFPS bounds what a viewer may ask for.
const (
	DefaultMinFPS = 30
	MaxMinFPS     = 120
)

const (
	targetFPS          = DefaultMinFPS
	rateWindow         = time.Second
	busyFraction       = 0.6
	trialGain          = 1.15
	idleResetAfter     = 5 * time.Second
	initialCooldown    = 10 * time.Second
	maximumCooldown    = 2 * time.Minute
	downgradeHeadroom  = 1.1
	minimumTrialFrames = 2
	// maxTrialExtensions bounds how long a trial waits for frames to arrive.
	maxTrialExtensions = 4
	// headroomWindows is how many consecutive windows must have frame rate
	// to spare before the controller tries less compression, so it does not
	// bounce between levels (restarting a video stream each time).
	headroomWindows = 5
	// activeFraction is the share of a window the screen must spend changing
	// for the window to count as motion. Compression that keeps up removes
	// the backlog, so the backlog alone cannot tell motion from calm.
	activeFraction = 0.3
)

type rateTrial struct {
	from, to int
	before   float64
	// warmup is how many windows to ignore first: a video stream starts with
	// a large key frame, so its first second says little.
	warmup int
	// extended counts windows added because nothing reached the viewer at
	// either level (a large frame still draining), which says nothing about
	// the trial.
	extended int
}

// fpsController picks the compression level from the delivered frame rate.
type fpsController struct {
	// levels are the compressionLevels this session can use.
	levels   []compressionLevel
	level    int
	maxLevel int
	logf     func(string, ...any)
	// target is the frame rate to keep; 0 means lossless only.
	target float64

	acks        atomic.Int64
	windowStart time.Time
	busy        time.Duration
	changing    time.Duration
	idle        time.Duration
	lastFPS     float64

	trial        *rateTrial
	headroom     int
	upCooldown   map[int]time.Time
	upPenalty    map[int]time.Duration
	downCooldown map[int]time.Time
	downPenalty  map[int]time.Duration
	// keptTiles is how many tiles were moving when a level was kept; a
	// lighter level is tried without spare frame rate only once the motion
	// has shrunk well below that.
	keptTiles map[int]int
	// remembered is the level in use before the screen calmed down; when
	// motion returns the controller goes straight back to it.
	remembered int
	// totalTiles is the size of the screen in tiles; motion over half of it
	// goes straight to video, which JPEG steps cannot match there.
	totalTiles int
}

// newFPSController offers the levels the viewer's features allow; video
// also needs an encoder on this PC.
func newFPSController(features []string, videoAvailable bool, logf func(string, ...any)) *fpsController {
	video := videoAvailable && hasFeature(features, FeatureH264)
	var levels []compressionLevel
	for _, level := range compressionLevels {
		// With video, reduced-resolution JPEG is worse than the video's own
		// bitrate adaptation, so video is the strongest step.
		if level.MotionScale > 1 && (!hasFeature(features, FeatureScaledJPEG) || video) {
			continue
		}
		if level.Video && (!videoAvailable || !hasFeature(features, FeatureH264)) {
			continue
		}
		levels = append(levels, level)
	}
	return &fpsController{
		levels:       levels,
		target:       targetFPS,
		maxLevel:     len(levels) - 1,
		logf:         logf,
		upCooldown:   map[int]time.Time{},
		upPenalty:    map[int]time.Duration{},
		downCooldown: map[int]time.Time{},
		downPenalty:  map[int]time.Duration{},
		keptTiles:    map[int]int{},
	}
}

func (c *fpsController) current() compressionLevel { return c.levels[c.level] }

// dropVideo removes the video level after the encoder kept failing and
// offers reduced-resolution JPEG instead when the viewer supports it.
func (c *fpsController) dropVideo(features []string) {
	current := c.levels[c.level]
	var levels []compressionLevel
	for _, level := range compressionLevels {
		if level.Video || (level.MotionScale > 1 && !hasFeature(features, FeatureScaledJPEG)) {
			continue
		}
		levels = append(levels, level)
	}
	c.levels, c.maxLevel, c.trial = levels, len(levels)-1, nil
	c.level = 0
	for index, level := range levels {
		if level.Quality >= current.Quality || index == 0 {
			c.level = index
		}
	}
	c.upCooldown, c.upPenalty = map[int]time.Time{}, map[int]time.Duration{}
	c.downCooldown, c.downPenalty = map[int]time.Time{}, map[int]time.Duration{}
	c.keptTiles, c.remembered = map[int]int{}, 0
}

// lossless reports that the viewer asked for no lossy compression at all.
func (c *fpsController) lossless() bool { return c.target <= 0 }

// setTarget sets the frame rate to keep; 0 turns lossy compression off.
func (c *fpsController) setTarget(fps int) {
	fps = max(0, min(MaxMinFPS, fps))
	if float64(fps) == c.target {
		return
	}
	c.target = float64(fps)
	c.trial = nil
	c.upCooldown, c.upPenalty = map[int]time.Time{}, map[int]time.Duration{}
	c.downCooldown, c.downPenalty = map[int]time.Time{}, map[int]time.Duration{}
	if fps == 0 {
		c.setLevel(0, "the viewer asked for lossless only")
	}
	if c.logf != nil {
		c.logf("screen minimum frame rate set to %d", fps)
	}
}

// delivered counts a frame the viewer acknowledged.
func (c *fpsController) delivered() { c.acks.Add(1) }

// observe records how long changes were waiting and how long the screen was
// changing during dt and, once a window has passed, adjusts the level.
// motion counts the tiles in motion.
func (c *fpsController) observe(now time.Time, pending, changing bool, dt time.Duration, motion func() int) {
	if c.windowStart.IsZero() {
		c.windowStart = now
		return
	}
	if pending {
		c.busy += dt
	}
	if changing {
		c.changing += dt
	}
	elapsed := now.Sub(c.windowStart)
	if elapsed < rateWindow {
		return
	}
	frames := c.acks.Swap(0)
	fps := float64(frames) / elapsed.Seconds()
	busy := c.busy.Seconds()/elapsed.Seconds() >= busyFraction
	active := busy || c.changing.Seconds()/elapsed.Seconds() >= activeFraction
	c.windowStart, c.busy, c.changing, c.lastFPS = now, 0, 0, fps
	tiles := 0
	if motion != nil {
		tiles = motion()
	}
	c.decide(now, fps, busy, active, frames, tiles)
}

// restartWindow starts a fresh measurement, so a trial is judged only from
// when its level actually took effect (a video encoder takes a moment to
// start).
func (c *fpsController) restartWindow(now time.Time) {
	c.windowStart, c.busy, c.changing = now, 0, 0
	c.acks.Store(0)
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

// decide adjusts the level after a window. busy means changes were waiting
// for the link; active means the screen kept changing (busy or not).
func (c *fpsController) decide(now time.Time, fps float64, busy, active bool, frames int64, tiles int) {
	if c.lossless() {
		return
	}
	if !active {
		if c.trial != nil {
			// The screen calmed down mid-trial; the measurement is void.
			c.setLevel(c.trial.from, "screen stopped changing during the trial")
			c.trial = nil
		}
		c.idle += rateWindow
		if c.idle >= idleResetAfter && c.level != 0 {
			c.remembered = c.level
			c.setLevel(0, "screen has been calm")
		}
		return
	}
	c.idle = 0
	if trial := c.trial; trial != nil && trial.warmup > 0 {
		trial.warmup--
		return
	}
	if trial := c.trial; trial != nil {
		c.trial = nil
		if trial.to > trial.from {
			if fps < 1 && trial.before < 1 {
				// Nothing arrived before or during the trial: a large frame is
				// still on its way. Keep measuring; if nothing ever arrives the
				// stronger level cannot hurt, so it stays.
				if trial.extended < maxTrialExtensions {
					trial.extended++
					c.trial = trial
				}
				return
			}
			if fps >= trial.before*trialGain && fps >= trial.before+1 {
				delete(c.upPenalty, trial.to)
				c.keptTiles[trial.to] = max(tiles, 1)
				if c.logf != nil {
					c.logf("screen compression level %d kept: %.1f fps, was %.1f", trial.to, fps, trial.before)
				}
				return
			}
			penalize(c.upCooldown, c.upPenalty, trial.to, now)
			c.setLevel(trial.from, fmt.Sprintf("did not raise the frame rate (%.1f fps, was %.1f)", fps, trial.before))
			return
		}
		if fps >= c.target || (!busy && fps >= trial.before*0.9) {
			delete(c.downPenalty, trial.from)
			if c.logf != nil {
				c.logf("screen compression level %d kept: %.1f fps", trial.to, fps)
			}
			return
		}
		penalize(c.downCooldown, c.downPenalty, trial.from, now)
		c.setLevel(trial.from, fmt.Sprintf("frame rate fell to %.1f fps at the lighter level", fps))
		return
	}
	if frames < minimumTrialFrames && fps >= 1 {
		return
	}
	// Spare capacity: frames to spare, or the link keeping up with motion
	// that has shrunk well below what the current level was kept for.
	shrunk := c.level > 0 && tiles*2 <= c.keptTiles[c.level]
	spare := fps >= c.target*downgradeHeadroom || (!busy && shrunk)
	if c.levels[c.level].Video {
		// Video already sends the moving area at the rate the link allows;
		// spare frame rate there does not mean JPEG could keep it. Try
		// lighter levels only once the moving area has shrunk.
		spare = shrunk
	}
	if spare {
		c.headroom++
	} else {
		c.headroom = 0
	}
	switch {
	case busy && fps < c.target && c.nextUp(now) > c.level:
		// A step that did not help is cooling down; try the step after it
		// rather than stalling below the one that would.
		next := c.nextUp(now)
		if video := c.maxLevel; c.levels[video].Video && c.totalTiles > 0 && tiles*2 >= c.totalTiles && now.After(c.upCooldown[video]) {
			next = video
		}
		c.remembered = 0
		c.trial = &rateTrial{from: c.level, to: next, before: fps}
		if c.levels[next].Video {
			c.trial.warmup = 1
		}
		c.setLevel(next, fmt.Sprintf("trying more compression at %.1f fps", fps))
	case spare && c.level > 0 && now.After(c.downCooldown[c.level]) && c.headroom >= headroomWindows:
		c.headroom = 0
		// Frames encoded at the stronger level are still arriving during the
		// first window, so the lighter level is judged from the second.
		c.trial = &rateTrial{from: c.level, to: c.level - 1, before: fps, warmup: 1}
		c.setLevel(c.level-1, fmt.Sprintf("trying less compression at %.1f fps", fps))
	}
}

// nextUp returns the lightest stronger level that is not cooling down, or the
// current level when there is none.
func (c *fpsController) nextUp(now time.Time) int {
	if c.remembered > c.level && c.remembered <= c.maxLevel && now.After(c.upCooldown[c.remembered]) {
		return c.remembered
	}
	for level := c.level + 1; level <= c.maxLevel; level++ {
		if now.After(c.upCooldown[level]) {
			return level
		}
	}
	return c.level
}

func (c *fpsController) describe(level int) string {
	value := c.levels[level]
	switch {
	case c.lossless():
		return "lossless only"
	case level == 0:
		return "full quality"
	case value.Lossless:
		return "lossless"
	case !value.MotionLossy && value.MotionScale <= 1:
		return fmt.Sprintf("JPEG %d for photographic areas", value.Quality)
	case value.Video:
		return "H.264 video for the moving area"
	case value.MotionScale > 1:
		return fmt.Sprintf("JPEG %d, moving areas at 1/%d resolution", value.Quality, value.MotionScale)
	default:
		return fmt.Sprintf("JPEG %d, moving areas lossy", value.Quality)
	}
}
