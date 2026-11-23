package screenview

import (
	"fmt"
	"image"
	"time"
)

// VideoEncoder encodes a screen region as H.264 (Annex B).
type VideoEncoder interface {
	// Encode encodes rect of src; it may return no data while the encoder
	// fills its pipeline.
	Encode(src *image.RGBA, rect image.Rectangle, keyframe bool) ([]byte, error)
	SetBitrate(bitrate int)
	Name() string
	Close()
}

// VideoEncoderFactory opens an encoder for a picture of width x height
// (multiples of 16).
type VideoEncoderFactory func(width, height, fps, bitrate int) (VideoEncoder, error)

// VideoDecoder decodes an H.264 stream into pictures cropped to the rect.
type VideoDecoder interface {
	Decode(data []byte, width, height int) (*image.RGBA, error)
	Close()
}

// VideoDecoderFactory opens a decoder for a new stream.
type VideoDecoderFactory func() (VideoDecoder, error)

const (
	videoFPS = 30
	// A region smaller than this many tiles is cheaper as tiles.
	minVideoTiles = 24
	// The stream keeps a larger rect this long before shrinking to the
	// current motion, to avoid restarting it for every small change.
	videoShrinkDelay = 3 * time.Second
	minVideoBitrate  = 500_000
	maxVideoBitrate  = 40_000_000
)

type videoStream struct {
	rect        image.Rectangle
	encoder     VideoEncoder
	started     bool
	shrinkSince time.Time
	bitrate     int
	frames      int
}

// safeEncode turns a panic in a video encoder (native codec code included)
// into an error, so the session falls back to tiles instead of ending.
func safeEncode(encoder VideoEncoder, src *image.RGBA, rect image.Rectangle, keyframe bool) (data []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("video encoder panicked: %v", recovered)
		}
	}()
	return encoder.Encode(src, rect, keyframe)
}

func liveCapabilities(video bool) []string {
	capabilities := []string{"control", "clipboard", "ui", "sas", "settle", "cursor"}
	if video {
		capabilities = append(capabilities, FeatureH264)
	}
	return capabilities
}

func alignVideo(value int) int { return (value + 15) &^ 15 }

// motionBounds returns the bounding box of the tiles in motion and their
// count.
func (g *tileGrid) motionBounds(now time.Time) (image.Rectangle, int) {
	var bounds image.Rectangle
	count := 0
	for index := range g.lastChange {
		if g.moving(index, now) {
			bounds = bounds.Union(g.tileRect(index))
			count++
		}
	}
	return bounds, count
}

// videoBitrate aims below the throughput the viewer acknowledged while
// changes were waiting, which is what the link really delivers.
func (s *liveServer) videoBitrate() int {
	throughput := s.throughput
	if throughput == 0 {
		throughput = 500_000 // bytes per second until measured
	}
	bitrate := int(throughput * 8 * 0.7)
	return max(minVideoBitrate, min(maxVideoBitrate, bitrate))
}

// measureThroughput updates the delivered rate once a second while the
// screen keeps changing; on a calm screen the rate says nothing about the
// link.
func (s *liveServer) measureThroughput(now time.Time, pending bool) {
	if s.tpStart.IsZero() {
		s.tpStart = now
		s.ackedBytes.Store(0)
		return
	}
	elapsed := now.Sub(s.tpStart)
	if elapsed < time.Second {
		return
	}
	bytes := float64(s.ackedBytes.Swap(0))
	s.tpStart = now
	if !pending || s.perf.heldFraction() < 0.5 {
		return
	}
	rate := bytes / elapsed.Seconds()
	if s.throughput == 0 {
		s.throughput = rate
	} else {
		s.throughput = s.throughput*0.6 + rate*0.4
	}
}

// maxVideoFailures is how many encoder failures a session tolerates before
// it stops using video.
const maxVideoFailures = 3

// videoFailed ends the current stream; after repeated failures the session
// stops using video.
func (s *liveServer) videoFailed(err error) {
	s.stopVideo()
	s.videoFailures++
	if s.videoFailures < maxVideoFailures {
		s.logf("screen video failed (%d of %d), restarting the stream: %v", s.videoFailures, maxVideoFailures, err)
		return
	}
	s.logf("screen video failed %d times, using tiles instead: %v", s.videoFailures, err)
	s.opts.VideoEncoder = nil
	s.rate.dropVideo(s.request.Features)
}

// stableVideoRect widens the motion bounds so the stream is not restarted
// (with a new key frame) for every small change in the moving area: motion
// over most of the screen uses the whole screen, and motion that grows past
// the current stream takes the union of both.
func (s *liveServer) stableVideoRect(grid *tileGrid, bounds image.Rectangle) image.Rectangle {
	screen := image.Rect(0, 0, grid.width, grid.height)
	if bounds.Dx()*bounds.Dy()*2 >= screen.Dx()*screen.Dy() {
		return screen
	}
	if s.video != nil && !bounds.In(s.video.rect) && bounds.Overlaps(s.video.rect) {
		bounds = bounds.Union(s.video.rect)
		if bounds.Dx()*bounds.Dy()*2 >= screen.Dx()*screen.Dy() {
			return screen
		}
	}
	return bounds
}

func (s *liveServer) stopVideo() {
	if s.video != nil {
		s.video.encoder.Close()
		s.video = nil
	}
}

// encodeVideo sends the region in motion as video. Tiles it covers are
// marked sent but lossy, so they are refined losslessly once still.
func (s *liveServer) encodeVideo(grid *tileGrid, reference *image.RGBA, now time.Time) []EncodedRect {
	bounds, count := grid.motionBounds(now)
	if count < minVideoTiles {
		s.stopVideo()
		return nil
	}
	bounds = s.stableVideoRect(grid, bounds)
	if s.video != nil {
		keep := bounds.In(s.video.rect)
		if keep && bounds.Dx()*bounds.Dy()*2 < s.video.rect.Dx()*s.video.rect.Dy() {
			if s.video.shrinkSince.IsZero() {
				s.video.shrinkSince = now
			}
			keep = now.Sub(s.video.shrinkSince) < videoShrinkDelay
		} else if keep {
			s.video.shrinkSince = time.Time{}
		}
		if !keep {
			s.stopVideo()
		}
	}
	if s.video == nil {
		bitrate := s.videoBitrate()
		encoder, err := s.opts.VideoEncoder(alignVideo(bounds.Dx()), alignVideo(bounds.Dy()), videoFPS, bitrate)
		if err != nil {
			s.videoFailed(fmt.Errorf("start: %w", err))
			return nil
		}
		s.logf("screen video started: %s for %v at %d kbit/s", encoder.Name(), bounds, bitrate/1000)
		s.video = &videoStream{rect: bounds, encoder: encoder, bitrate: bitrate}
		s.rate.restartWindow(time.Now())
	}
	if bitrate := s.videoBitrate(); bitrate > s.video.bitrate*5/4 || bitrate < s.video.bitrate*4/5 {
		s.video.encoder.SetBitrate(bitrate)
		s.video.bitrate = bitrate
	}
	covered := false
	for index, dirty := range grid.dirty {
		if dirty && grid.tileRect(index).Overlaps(s.video.rect) && grid.tileRect(index).In(s.video.rect) {
			covered = true
			break
		}
	}
	if !covered {
		return nil
	}
	encodeStarted := time.Now()
	data, err := safeEncode(s.video.encoder, reference, s.video.rect, !s.video.started)
	elapsed := time.Since(encodeStarted)
	s.perf.add(&s.perf.video, elapsed)
	s.perf.add(&s.perf.videoBytes, time.Duration(len(data))*time.Millisecond)
	s.video.frames++
	if s.video.frames <= 3 || s.video.frames%150 == 0 {
		s.logf("screen video frame %d: %d bytes in %s", s.video.frames, len(data), elapsed.Round(time.Millisecond))
	}
	if err != nil {
		s.videoFailed(err)
		return nil
	}
	for index, dirty := range grid.dirty {
		if dirty && grid.tileRect(index).In(s.video.rect) {
			grid.dirty[index] = false
			grid.lossy[index] = true
		}
	}
	if len(data) == 0 {
		return nil
	}
	flags := byte(0)
	if !s.video.started {
		flags = videoStreamStart
		s.video.started = true
	}
	payload := make([]byte, 0, len(data)+1)
	payload = append(payload, flags)
	payload = append(payload, data...)
	rect := s.video.rect
	return []EncodedRect{{X: rect.Min.X, Y: rect.Min.Y, W: rect.Dx(), H: rect.Dy(), Enc: EncH264, Data: payload}}
}
