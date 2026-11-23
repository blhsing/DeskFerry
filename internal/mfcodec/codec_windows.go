//go:build windows

package mfcodec

import (
	"errors"
	"fmt"
	"image"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// thread runs Media Foundation calls on one OS thread with COM initialized.
type thread struct {
	calls chan func()
	done  chan struct{}
}

func newThread() (*thread, error) {
	t := &thread{calls: make(chan func()), done: make(chan struct{})}
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(t.done)
		procCoInitializeEx.Call(0, coinitMultithreaded)
		defer procCoUninitialize.Call()
		if hr, _, _ := procMFStartup.Call(mfVersion, 0); failed(hr) {
			ready <- hrError("MFStartup", hr)
			return
		}
		defer procMFShutdown.Call()
		ready <- nil
		for call := range t.calls {
			call()
		}
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	return t, nil
}

// do runs fn on the Media Foundation thread. A panic in fn (a codec that
// misbehaves) is returned as an error instead of ending the process.
func (t *thread) do(fn func()) (err error) {
	finished := make(chan struct{})
	t.calls <- func() {
		defer close(finished)
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("media foundation call panicked: %v", recovered)
			}
		}()
		fn()
	}
	<-finished
	return err
}

func (t *thread) stop() {
	close(t.calls)
	<-t.done
}

type registerTypeInfo struct {
	Major GUID
	Sub   GUID
}

// enumTransforms activates the transforms of category that convert in to out.
func enumTransforms(category GUID, flags uint32, in, out *registerTypeInfo) ([]*comObject, []string) {
	var activates uintptr
	var count uint32
	hr, _, _ := procMFTEnumEx.Call(uintptr(unsafe.Pointer(&category)), uintptr(flags),
		uintptr(unsafe.Pointer(in)), uintptr(unsafe.Pointer(out)), uintptr(unsafe.Pointer(&activates)), uintptr(unsafe.Pointer(&count)))
	if failed(hr) || count == 0 || activates == 0 {
		return nil, nil
	}
	defer procCoTaskMemFree.Call(activates)
	list := unsafe.Slice((**comObject)(unsafe.Pointer(activates)), int(count))
	var transforms []*comObject
	var names []string
	for _, activate := range list {
		name := friendlyName(activate)
		var transform *comObject
		if hr := activate.call(33, uintptr(unsafe.Pointer(&iidIMFTransform)), uintptr(unsafe.Pointer(&transform))); !failed(hr) && transform != nil {
			transforms = append(transforms, transform)
			names = append(names, name)
		}
		activate.Release()
	}
	return transforms, names
}

func friendlyName(activate *comObject) string {
	var text *uint16
	var length uint32
	if hr := activate.call(13, uintptr(unsafe.Pointer(&mftFriendlyNameAttr)), uintptr(unsafe.Pointer(&text)), uintptr(unsafe.Pointer(&length))); failed(hr) || text == nil {
		return "unnamed transform"
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(text)))
	return windows.UTF16PtrToString(text)
}

// transform drives one Media Foundation transform, synchronous or async.
type transform struct {
	mft             *comObject
	events          *comObject
	async           bool
	needInput       int
	providesSamples bool
	outputSize      int
}

func newTransform(mft *comObject) *transform {
	t := &transform{mft: mft}
	var attributes *comObject
	if hr := mft.call(mftGetAttributes, uintptr(unsafe.Pointer(&attributes))); !failed(hr) && attributes != nil {
		if value, err := getUINT32(attributes, mfTransformAsync); err == nil && value != 0 {
			t.async = true
			_ = setUINT32(attributes, mfTransformAsyncUnlock, 1)
		}
		_ = setUINT32(attributes, mfLowLatency, 1)
		attributes.Release()
	}
	if t.async {
		if events, err := mft.QueryInterface(&iidIMFMediaEventGen); err == nil {
			t.events = events
		} else {
			t.async = false
		}
	}
	return t
}

func (t *transform) refreshOutputInfo() {
	var info outputStreamInfo
	if hr := t.mft.call(mftGetOutputStreamInfo, 0, uintptr(unsafe.Pointer(&info))); !failed(hr) {
		t.providesSamples = info.Flags&(mftOutputStreamProvidesSamples|mftOutputStreamCanProvideSamples) != 0
		t.outputSize = int(info.Size)
	}
}

// acceptOutputType sets the transform's first offered output type, as
// required after MF_E_TRANSFORM_STREAM_CHANGE.
func (t *transform) acceptOutputType() error {
	var mediaType *comObject
	if hr := t.mft.call(mftGetOutputAvailableType, 0, 0, uintptr(unsafe.Pointer(&mediaType))); failed(hr) {
		return hrError("GetOutputAvailableType", hr)
	}
	defer mediaType.Release()
	if hr := t.mft.call(mftSetOutputType, 0, uintptr(unsafe.Pointer(mediaType)), 0); failed(hr) {
		return hrError("SetOutputType", hr)
	}
	t.refreshOutputInfo()
	return nil
}

func (t *transform) message(message uint32) {
	t.mft.call(mftProcessMessage, uintptr(message), 0)
}

// pollEvents handles queued async events for up to wait and returns how many
// outputs became ready. It returns early once an output is ready.
func (t *transform) pollEvents(wait time.Duration) (int, error) {
	deadline := time.Now().Add(wait)
	outputs := 0
	for {
		var event *comObject
		hr := t.events.call(3, 1, uintptr(unsafe.Pointer(&event))) // GetEvent(MF_EVENT_FLAG_NO_WAIT)
		if uint32(hr) == hrNoEvents {
			if outputs > 0 || !time.Now().Before(deadline) {
				return outputs, nil
			}
			time.Sleep(time.Millisecond)
			continue
		}
		if failed(hr) {
			return outputs, hrError("GetEvent", hr)
		}
		var eventType uint32
		event.call(33, uintptr(unsafe.Pointer(&eventType)))
		event.Release()
		switch eventType {
		case meTransformNeedInput:
			t.needInput++
		case meTransformHaveOutput:
			outputs++
		}
	}
}

// feed gives sample to the transform. An async transform must hand over its
// finished output before it asks for more input, so outputs that become
// ready while waiting go to collect.
func (t *transform) feed(sample *comObject, collect func() error) error {
	if t.async {
		deadline := time.Now().Add(2 * time.Second)
		for t.needInput == 0 {
			outputs, err := t.pollEvents(10 * time.Millisecond)
			if err != nil {
				return err
			}
			for ; outputs > 0; outputs-- {
				if err := collect(); err != nil {
					return err
				}
			}
			if t.needInput == 0 && time.Now().After(deadline) {
				return errors.New("video transform did not ask for input")
			}
		}
		t.needInput--
	}
	if hr := t.mft.call(mftProcessInput, 0, uintptr(unsafe.Pointer(sample)), 0); failed(hr) {
		return hrError("ProcessInput", hr)
	}
	return nil
}

// processOutput returns one output sample, errNeedMoreInput, or errStreamChange.
func (t *transform) processOutput() (*comObject, error) {
	buffer := outputDataBuffer{}
	if !t.providesSamples {
		sample, err := emptySample(max(t.outputSize, 1<<20))
		if err != nil {
			return nil, err
		}
		buffer.Sample = sample
	}
	var status uint32
	hr := t.mft.call(mftProcessOutput, 0, 1, uintptr(unsafe.Pointer(&buffer)), uintptr(unsafe.Pointer(&status)))
	if buffer.Events != 0 {
		(*comObject)(unsafe.Pointer(buffer.Events)).Release()
	}
	switch {
	case uint32(hr) == hrNeedMoreInput:
		buffer.Sample.Release()
		return nil, errNeedMoreInput
	case uint32(hr) == hrStreamChange:
		buffer.Sample.Release()
		return nil, errStreamChange
	case failed(hr):
		buffer.Sample.Release()
		return nil, hrError("ProcessOutput", hr)
	case buffer.Sample == nil:
		// Some hardware encoders report success without a sample.
		return nil, errNeedMoreInput
	}
	return buffer.Sample, nil
}

func (t *transform) release() {
	if t.events != nil {
		t.events.Release()
	}
	if t.mft != nil {
		t.message(mftMessageNotifyEndOfStream)
		t.message(mftMessageNotifyEndStreaming)
		t.mft.Release()
	}
}

// Encoder is an H.264 encoder for one picture size.
type Encoder struct {
	thread   *thread
	t        *transform
	codec    *comObject
	name     string
	width    int
	height   int
	fps      int
	frame    int64
	nv12     []byte
	hardware bool
}

// EncoderOptions configures NewEncoder.
type EncoderOptions struct {
	// Width and Height are the picture size; use AlignedSize.
	Width, Height int
	FPS           int
	Bitrate       int
	// SoftwareOnly skips hardware encoders.
	SoftwareOnly bool
}

// NewEncoder opens a hardware H.264 encoder when one is available, else the
// Microsoft software encoder.
func NewEncoder(opts EncoderOptions) (*Encoder, error) {
	if opts.Width <= 0 || opts.Height <= 0 || opts.Width%16 != 0 || opts.Height%16 != 0 {
		return nil, errors.New("encoder picture size must be a positive multiple of 16")
	}
	if opts.FPS <= 0 {
		opts.FPS = 30
	}
	if opts.Bitrate <= 0 {
		opts.Bitrate = 4_000_000
	}
	worker, err := newThread()
	if err != nil {
		return nil, err
	}
	encoder := &Encoder{thread: worker, width: opts.Width, height: opts.Height, fps: opts.FPS}
	var openErr error
	worker.do(func() { openErr = encoder.open(opts) })
	if openErr != nil {
		worker.stop()
		return nil, openErr
	}
	return encoder, nil
}

func (e *Encoder) open(opts EncoderOptions) error {
	in := registerTypeInfo{Major: mfMediaTypeVideo, Sub: mfVideoFormatNV12}
	out := registerTypeInfo{Major: mfMediaTypeVideo, Sub: mfVideoFormatH264}
	var candidates []*comObject
	var names []string
	if !opts.SoftwareOnly {
		candidates, names = enumTransforms(mftCategoryVideoEncoder, mftEnumFlagHardware|mftEnumFlagAsyncMFT|mftEnumFlagSortAndFilter, &in, &out)
	}
	hardwareCount := len(candidates)
	software, softwareNames := enumTransforms(mftCategoryVideoEncoder, mftEnumFlagSyncMFT|mftEnumFlagSortAndFilter, &in, &out)
	candidates = append(candidates, software...)
	names = append(names, softwareNames...)
	var failures []string
	for index, candidate := range candidates {
		if e.t != nil {
			candidate.Release()
			continue
		}
		if err := e.configure(candidate, opts); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", names[index], err))
			candidate.Release()
			e.t, e.codec = nil, nil
			continue
		}
		e.name = names[index]
		e.hardware = index < hardwareCount
	}
	if e.t == nil {
		if len(failures) == 0 {
			return errors.New("no H.264 encoder is installed")
		}
		return fmt.Errorf("no H.264 encoder could be configured: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (e *Encoder) configure(mft *comObject, opts EncoderOptions) error {
	t := newTransform(mft)
	output, err := newMediaType()
	if err != nil {
		return err
	}
	defer output.Release()
	_ = setGUID(output, mfMTMajorType, mfMediaTypeVideo)
	_ = setGUID(output, mfMTSubtype, mfVideoFormatH264)
	_ = setUINT64(output, mfMTFrameSize, uint64(opts.Width)<<32|uint64(opts.Height))
	_ = setUINT64(output, mfMTFrameRate, uint64(opts.FPS)<<32|1)
	_ = setUINT64(output, mfMTPixelAspectRatio, 1<<32|1)
	_ = setUINT32(output, mfMTAvgBitrate, uint32(opts.Bitrate))
	_ = setUINT32(output, mfMTInterlaceMode, 2)
	_ = setUINT32(output, mfMTMPEG2Profile, 66) // Baseline: no B-frames, decodable everywhere
	if hr := mft.call(mftSetOutputType, 0, uintptr(unsafe.Pointer(output)), 0); failed(hr) {
		return hrError("SetOutputType", hr)
	}
	input, err := newMediaType()
	if err != nil {
		return err
	}
	defer input.Release()
	_ = setGUID(input, mfMTMajorType, mfMediaTypeVideo)
	_ = setGUID(input, mfMTSubtype, mfVideoFormatNV12)
	_ = setUINT64(input, mfMTFrameSize, uint64(opts.Width)<<32|uint64(opts.Height))
	_ = setUINT64(input, mfMTFrameRate, uint64(opts.FPS)<<32|1)
	_ = setUINT64(input, mfMTPixelAspectRatio, 1<<32|1)
	_ = setUINT32(input, mfMTInterlaceMode, 2)
	if hr := mft.call(mftSetInputType, 0, uintptr(unsafe.Pointer(input)), 0); failed(hr) {
		return hrError("SetInputType", hr)
	}
	if codec, err := mft.QueryInterface(&iidICodecAPI); err == nil {
		e.codec = codec
		_ = codecSetUINT32(codec, codecAPIRateControlMode, 0) // CBR
		_ = codecSetUINT32(codec, codecAPIMeanBitRate, uint32(opts.Bitrate))
		_ = codecSetUINT32(codec, codecAPIGOPSize, uint32(opts.FPS*4))
		_ = codecSetUINT32(codec, codecAPIBFrames, 0)
		v := variant{VT: vtBool, Value: 0xFFFF}
		codec.call(9, uintptr(unsafe.Pointer(&codecAPILowLatency)), uintptr(unsafe.Pointer(&v)))
	}
	t.refreshOutputInfo()
	t.message(mftMessageNotifyBeginStreaming)
	t.message(mftMessageNotifyStartOfStream)
	e.t = t
	return nil
}

// Name names the encoder in use, for diagnostics.
func (e *Encoder) Name() string { return e.name }

// Hardware reports whether a hardware encoder is in use.
func (e *Encoder) Hardware() bool { return e.hardware }

// SetBitrate changes the target bitrate.
func (e *Encoder) SetBitrate(bitrate int) {
	e.thread.do(func() {
		if e.codec != nil {
			_ = codecSetUINT32(e.codec, codecAPIMeanBitRate, uint32(bitrate))
		}
	})
}

// Encode encodes rect of src, padded to the encoder size, and returns the
// Annex B access units the encoder produced, which may be none while it
// fills its pipeline.
func (e *Encoder) Encode(src *image.RGBA, rect image.Rectangle, keyframe bool) ([]byte, error) {
	var out []byte
	var err error
	if panicErr := e.thread.do(func() { out, err = e.encode(src, rect, keyframe) }); panicErr != nil {
		return nil, panicErr
	}
	return out, err
}

func (e *Encoder) encode(src *image.RGBA, rect image.Rectangle, keyframe bool) ([]byte, error) {
	e.nv12 = RGBAToNV12(src, rect, e.width, e.height, e.nv12)
	duration := int64(10_000_000 / e.fps)
	sample, err := newSample(e.nv12, e.frame*duration, duration)
	if err != nil {
		return nil, err
	}
	defer sample.Release()
	e.frame++
	if keyframe && e.codec != nil {
		_ = codecSetUINT32(e.codec, codecAPIForceKeyFrame, 1)
	}
	var out []byte
	// collect pulls one output; errNeedMoreInput means none was ready.
	collect := func() error {
		for {
			result, err := e.t.processOutput()
			if errors.Is(err, errStreamChange) {
				// The encoder renegotiated its output; accept its new type.
				// An async encoder announces the output again afterwards.
				if err := e.t.acceptOutputType(); err != nil {
					return err
				}
				if e.t.async {
					return nil
				}
				continue
			}
			if err != nil {
				return err
			}
			data, err := sampleBytes(result)
			result.Release()
			if err != nil {
				return err
			}
			out = append(out, data...)
			return nil
		}
	}
	if err := e.t.feed(sample, collect); err != nil {
		return out, err
	}
	if !e.t.async {
		for {
			if err := collect(); errors.Is(err, errNeedMoreInput) {
				return out, nil
			} else if err != nil {
				return out, err
			}
		}
	}
	// Give the hardware a moment to finish this frame, then take whatever
	// else is ready without waiting.
	wait := 50 * time.Millisecond
	for {
		outputs, err := e.t.pollEvents(wait)
		if err != nil {
			return out, err
		}
		if outputs == 0 {
			return out, nil
		}
		for ; outputs > 0; outputs-- {
			if err := collect(); err != nil && !errors.Is(err, errNeedMoreInput) {
				return out, err
			}
		}
		wait = 0
	}
}

// Close releases the encoder.
func (e *Encoder) Close() {
	e.thread.do(func() {
		if e.codec != nil {
			e.codec.Release()
		}
		if e.t != nil {
			e.t.release()
		}
	})
	e.thread.stop()
}

// Decoder decodes an H.264 Annex B stream to RGBA pictures.
type Decoder struct {
	thread      *thread
	t           *transform
	stride      int
	planeHeight int
	width       int
	height      int
}

// NewDecoder opens the Microsoft H.264 decoder.
func NewDecoder() (*Decoder, error) {
	worker, err := newThread()
	if err != nil {
		return nil, err
	}
	decoder := &Decoder{thread: worker}
	var openErr error
	worker.do(func() { openErr = decoder.open() })
	if openErr != nil {
		worker.stop()
		return nil, openErr
	}
	return decoder, nil
}

func (d *Decoder) open() error {
	var mft *comObject
	if hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMSH264Decoder)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIMFTransform)), uintptr(unsafe.Pointer(&mft))); failed(hr) {
		return hrError("create the H.264 decoder", hr)
	}
	d.t = newTransform(mft)
	input, err := newMediaType()
	if err != nil {
		return err
	}
	defer input.Release()
	_ = setGUID(input, mfMTMajorType, mfMediaTypeVideo)
	_ = setGUID(input, mfMTSubtype, mfVideoFormatH264)
	if hr := mft.call(mftSetInputType, 0, uintptr(unsafe.Pointer(input)), 0); failed(hr) {
		return hrError("decoder SetInputType", hr)
	}
	if err := d.pickOutput(); err != nil {
		return err
	}
	d.t.message(mftMessageNotifyBeginStreaming)
	d.t.message(mftMessageNotifyStartOfStream)
	return nil
}

// pickOutput selects NV12 output and reads the picture geometry.
func (d *Decoder) pickOutput() error {
	for index := 0; ; index++ {
		var mediaType *comObject
		if hr := d.t.mft.call(mftGetOutputAvailableType, 0, uintptr(index), uintptr(unsafe.Pointer(&mediaType))); failed(hr) {
			return hrError("decoder has no NV12 output; GetOutputAvailableType", hr)
		}
		subtype, _ := getGUID(mediaType, mfMTSubtype)
		if subtype != mfVideoFormatNV12 {
			mediaType.Release()
			continue
		}
		hr := d.t.mft.call(mftSetOutputType, 0, uintptr(unsafe.Pointer(mediaType)), 0)
		if size, err := getUINT64(mediaType, mfMTFrameSize); err == nil {
			d.width, d.height = int(size>>32), int(uint32(size))
		}
		d.stride = d.width
		if stride, err := getUINT32(mediaType, mfMTDefaultStride); err == nil && int32(stride) > 0 {
			d.stride = int(int32(stride))
		}
		mediaType.Release()
		if failed(hr) {
			return hrError("decoder SetOutputType", hr)
		}
		d.t.refreshOutputInfo()
		return nil
	}
}

// Decode feeds one Annex B access unit and returns the newest decoded
// picture cropped to width x height, or nil when none is ready yet.
func (d *Decoder) Decode(data []byte, width, height int) (*image.RGBA, error) {
	var picture *image.RGBA
	var err error
	if panicErr := d.thread.do(func() { picture, err = d.decode(data, width, height) }); panicErr != nil {
		return nil, panicErr
	}
	return picture, err
}

func (d *Decoder) decode(data []byte, width, height int) (*image.RGBA, error) {
	sample, err := newSample(data, 0, 0)
	if err != nil {
		return nil, err
	}
	defer sample.Release()
	if err := d.t.feed(sample, func() error { return nil }); err != nil {
		return nil, err
	}
	var picture *image.RGBA
	for {
		result, err := d.t.processOutput()
		if errors.Is(err, errNeedMoreInput) {
			return picture, nil
		}
		if errors.Is(err, errStreamChange) {
			if err := d.pickOutput(); err != nil {
				return picture, err
			}
			continue
		}
		if err != nil {
			return picture, err
		}
		bytes, err := sampleBytes(result)
		result.Release()
		if err != nil {
			return picture, err
		}
		if d.stride <= 0 || len(bytes) < d.stride*d.height*3/2 {
			return picture, errors.New("decoded picture is smaller than its format")
		}
		planeHeight := len(bytes) * 2 / 3 / d.stride
		picture = NV12ToRGBA(bytes, d.stride, planeHeight, min(width, d.width), min(height, d.height))
	}
}

// Close releases the decoder.
func (d *Decoder) Close() {
	d.thread.do(func() {
		if d.t != nil {
			d.t.release()
		}
	})
	d.thread.stop()
}
