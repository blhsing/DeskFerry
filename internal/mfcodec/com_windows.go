//go:build windows

// Package mfcodec encodes and decodes H.264 with Windows Media Foundation,
// preferring hardware encoders. It calls the COM interfaces directly, so it
// needs no cgo.
package mfcodec

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	mfplat                = windows.NewLazySystemDLL("mfplat.dll")
	ole32                 = windows.NewLazySystemDLL("ole32.dll")
	procMFStartup         = mfplat.NewProc("MFStartup")
	procMFShutdown        = mfplat.NewProc("MFShutdown")
	procMFCreateMediaType = mfplat.NewProc("MFCreateMediaType")
	procMFCreateSample    = mfplat.NewProc("MFCreateSample")
	procMFCreateMemoryBuf = mfplat.NewProc("MFCreateMemoryBuffer")
	procMFTEnumEx         = mfplat.NewProc("MFTEnumEx")
	procCoInitializeEx    = ole32.NewProc("CoInitializeEx")
	procCoUninitialize    = ole32.NewProc("CoUninitialize")
	procCoCreateInstance  = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree     = ole32.NewProc("CoTaskMemFree")
)

const (
	mfVersion                = 0x00020070
	coinitMultithreaded      = 0x0
	clsctxInprocServer       = 0x1
	mftEnumFlagSyncMFT       = 0x1
	mftEnumFlagAsyncMFT      = 0x2
	mftEnumFlagHardware      = 0x4
	mftEnumFlagSortAndFilter = 0x40

	mftMessageCommandFlush         = 0x0
	mftMessageCommandDrain         = 0x1
	mftMessageNotifyBeginStreaming = 0x10000000
	mftMessageNotifyEndStreaming   = 0x10000001
	mftMessageNotifyEndOfStream    = 0x10000002
	mftMessageNotifyStartOfStream  = 0x10000003

	mftOutputStreamProvidesSamples   = 0x100
	mftOutputStreamCanProvideSamples = 0x200

	meTransformNeedInput     = 601
	meTransformHaveOutput    = 602
	meTransformDrainComplete = 603

	vtUI4  = 19
	vtUI8  = 21
	vtBool = 11
)

var (
	errNeedMoreInput = errors.New("transform needs more input")
	errStreamChange  = errors.New("transform output type changed")
)

const (
	hrNeedMoreInput  = 0xC00D6D72
	hrStreamChange   = 0xC00D6D61
	hrNoEvents       = 0xC00D3E80
	hrNotImplemented = 0x80004001
)

// GUID is a Windows GUID.
type GUID = windows.GUID

func mustGUID(text string) GUID {
	guid, err := windows.GUIDFromString(text)
	if err != nil {
		panic(err)
	}
	return guid
}

var (
	mftCategoryVideoEncoder = mustGUID("{f79eac7d-e545-4387-bdee-d647d7bde42a}")
	mftCategoryVideoDecoder = mustGUID("{d6c02d4b-6833-45b4-971a-05a4b04bab91}")
	mfMediaTypeVideo        = mustGUID("{73646976-0000-0010-8000-00AA00389B71}")
	mfVideoFormatH264       = mustGUID("{34363248-0000-0010-8000-00AA00389B71}")
	mfVideoFormatNV12       = mustGUID("{3231564E-0000-0010-8000-00AA00389B71}")
	mfMTMajorType           = mustGUID("{48eba18e-f8c9-4687-bf11-0a74c9f96a8f}")
	mfMTSubtype             = mustGUID("{f7e34c9a-42e8-4714-b74b-cb29d72c35e5}")
	mfMTFrameSize           = mustGUID("{1652c33d-d6b2-4012-b834-72030849a37d}")
	mfMTFrameRate           = mustGUID("{c459a2e8-3d2c-4e44-b132-fee5156c7bb0}")
	mfMTAvgBitrate          = mustGUID("{20332624-fb0d-4d9e-bd0d-cbf6786c102e}")
	mfMTInterlaceMode       = mustGUID("{e2724bb8-e676-4806-b4b2-a8d6efb44ccd}")
	mfMTPixelAspectRatio    = mustGUID("{c6376a1e-8d0a-4027-be45-6d9a0ad39bb6}")
	mfMTMPEG2Profile        = mustGUID("{ad76a80b-2d5c-4e0b-b375-64e520137036}")
	mfMTDefaultStride       = mustGUID("{644b4e48-1e02-4516-b0eb-c01ca9d49ac6}")
	mfTransformAsyncUnlock  = mustGUID("{e5666d6b-3422-4eb6-a421-da7db1f8e207}")
	mfTransformAsync        = mustGUID("{f81a699a-649a-497d-8c73-29f8fed6ad7a}")
	mfLowLatency            = mustGUID("{9c27891a-ed7a-40e1-88e8-b22727a024ee}")
	mftFriendlyNameAttr     = mustGUID("{314ffbae-5b41-4c95-9c19-4e7d586face3}")
	iidIMFTransform         = mustGUID("{bf94c121-5b05-4e6f-8000-ba598961414d}")
	iidIMFMediaEventGen     = mustGUID("{2CD0BD52-BCD5-4B89-B62C-EADC0C031E7D}")
	iidICodecAPI            = mustGUID("{901db4c7-31ce-41a2-85dc-8fa0bf41b8da}")
	clsidMSH264Decoder      = mustGUID("{62CE7E72-4C71-4d20-B15D-452831A87D9D}")
	codecAPIForceKeyFrame   = mustGUID("{398c1b98-8353-475a-9ef2-8f265d260345}")
	codecAPIRateControlMode = mustGUID("{1c0608e9-370c-4710-8a58-cb6181c42423}")
	codecAPIMeanBitRate     = mustGUID("{f7222374-2144-4815-b550-a37f8e12ee52}")
	codecAPIGOPSize         = mustGUID("{95f31b26-95a4-41aa-9303-246a7fc6eef1}")
	codecAPILowLatency      = mustGUID("{9c27891a-ed7a-40e1-88e8-b22727a024ee}")
	codecAPIBFrames         = mustGUID("{2b0adae9-7e01-4a7b-8d0d-9f5c3e11d4d0}")
)

// comObject is any COM interface pointer.
type comObject struct {
	vtbl *[64]uintptr
}

// call invokes a vtable method. Out-parameters are passed as
// uintptr(unsafe.Pointer(&local)); uintptrescapes moves those locals to the
// heap, otherwise a stack growth during the call leaves native code writing
// into the old stack and the Go variable unset.
//
//go:uintptrescapes
func (o *comObject) call(index int, args ...uintptr) uintptr {
	result, _, _ := syscall.SyscallN(o.vtbl[index], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return result
}

func (o *comObject) Release() {
	if o != nil {
		o.call(2)
	}
}

func (o *comObject) QueryInterface(iid *GUID) (*comObject, error) {
	var out *comObject
	if hr := o.call(0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out))); failed(hr) {
		return nil, hrError("QueryInterface", hr)
	}
	return out, nil
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

type hresultError struct {
	op string
	hr uint32
}

func (e *hresultError) Error() string { return fmt.Sprintf("%s failed: HRESULT 0x%08X", e.op, e.hr) }

func hrError(op string, hr uintptr) error { return &hresultError{op: op, hr: uint32(hr)} }

func hrIs(err error, hr uint32) bool {
	var target *hresultError
	return errors.As(err, &target) && target.hr == hr
}

// IMFAttributes vtable slots shared by media types, samples and activates.
const (
	attrGetUINT32 = 7
	attrGetUINT64 = 8
	attrGetGUID   = 10
	attrSetUINT32 = 21
	attrSetUINT64 = 22
	attrSetGUID   = 24
)

func setUINT32(o *comObject, key GUID, value uint32) error {
	if hr := o.call(attrSetUINT32, uintptr(unsafe.Pointer(&key)), uintptr(value)); failed(hr) {
		return hrError("SetUINT32", hr)
	}
	return nil
}

func setUINT64(o *comObject, key GUID, value uint64) error {
	if hr := o.call(attrSetUINT64, uintptr(unsafe.Pointer(&key)), uintptr(value)); failed(hr) {
		return hrError("SetUINT64", hr)
	}
	return nil
}

func setGUID(o *comObject, key, value GUID) error {
	if hr := o.call(attrSetGUID, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&value))); failed(hr) {
		return hrError("SetGUID", hr)
	}
	return nil
}

func getUINT32(o *comObject, key GUID) (uint32, error) {
	var value uint32
	if hr := o.call(attrGetUINT32, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&value))); failed(hr) {
		return 0, hrError("GetUINT32", hr)
	}
	return value, nil
}

func getUINT64(o *comObject, key GUID) (uint64, error) {
	var value uint64
	if hr := o.call(attrGetUINT64, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&value))); failed(hr) {
		return 0, hrError("GetUINT64", hr)
	}
	return value, nil
}

func getGUID(o *comObject, key GUID) (GUID, error) {
	var value GUID
	if hr := o.call(attrGetGUID, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&value))); failed(hr) {
		return GUID{}, hrError("GetGUID", hr)
	}
	return value, nil
}

func newMediaType() (*comObject, error) {
	var mediaType *comObject
	if hr, _, _ := procMFCreateMediaType.Call(uintptr(unsafe.Pointer(&mediaType))); failed(hr) {
		return nil, hrError("MFCreateMediaType", hr)
	}
	return mediaType, nil
}

// IMFTransform vtable slots.
const (
	mftGetInputStreamInfo     = 6
	mftGetOutputStreamInfo    = 7
	mftGetAttributes          = 8
	mftGetInputAvailableType  = 13
	mftGetOutputAvailableType = 14
	mftSetInputType           = 15
	mftSetOutputType          = 16
	mftGetOutputCurrentType   = 18
	mftProcessMessage         = 23
	mftProcessInput           = 24
	mftProcessOutput          = 25
)

// IMFSample and IMFMediaBuffer slots.
const (
	sampleSetSampleTime       = 36
	sampleSetSampleDuration   = 38
	sampleConvertToContiguous = 41
	sampleAddBuffer           = 42
	bufferLock                = 3
	bufferUnlock              = 4
	bufferGetCurrentLength    = 5
	bufferSetCurrentLength    = 6
)

type outputDataBuffer struct {
	StreamID uint32
	_        uint32
	Sample   *comObject
	Status   uint32
	_        uint32
	Events   uintptr
}

type outputStreamInfo struct {
	Flags     uint32
	Size      uint32
	Alignment uint32
}

// newSample wraps data in a new IMFSample.
func newSample(data []byte, time, duration int64) (*comObject, error) {
	var buffer *comObject
	if hr, _, _ := procMFCreateMemoryBuf.Call(uintptr(len(data)), uintptr(unsafe.Pointer(&buffer))); failed(hr) {
		return nil, hrError("MFCreateMemoryBuffer", hr)
	}
	defer buffer.Release()
	var pointer uintptr
	if hr := buffer.call(bufferLock, uintptr(unsafe.Pointer(&pointer)), 0, 0); failed(hr) {
		return nil, hrError("IMFMediaBuffer.Lock", hr)
	}
	if pointer == 0 {
		buffer.call(bufferUnlock)
		return nil, errors.New("media buffer has no memory")
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(pointer)), len(data)), data)
	buffer.call(bufferUnlock)
	buffer.call(bufferSetCurrentLength, uintptr(len(data)))
	var sample *comObject
	if hr, _, _ := procMFCreateSample.Call(uintptr(unsafe.Pointer(&sample))); failed(hr) {
		return nil, hrError("MFCreateSample", hr)
	}
	sample.call(sampleAddBuffer, uintptr(unsafe.Pointer(buffer)))
	sample.call(sampleSetSampleTime, uintptr(time))
	sample.call(sampleSetSampleDuration, uintptr(duration))
	return sample, nil
}

// emptySample returns a sample with a buffer of size bytes, for transforms
// that do not allocate their own output.
func emptySample(size int) (*comObject, error) {
	var buffer *comObject
	if hr, _, _ := procMFCreateMemoryBuf.Call(uintptr(size), uintptr(unsafe.Pointer(&buffer))); failed(hr) {
		return nil, hrError("MFCreateMemoryBuffer", hr)
	}
	defer buffer.Release()
	var sample *comObject
	if hr, _, _ := procMFCreateSample.Call(uintptr(unsafe.Pointer(&sample))); failed(hr) {
		return nil, hrError("MFCreateSample", hr)
	}
	sample.call(sampleAddBuffer, uintptr(unsafe.Pointer(buffer)))
	return sample, nil
}

// sampleBytes copies a sample's contiguous data.
func sampleBytes(sample *comObject) ([]byte, error) {
	var buffer *comObject
	if hr := sample.call(sampleConvertToContiguous, uintptr(unsafe.Pointer(&buffer))); failed(hr) {
		return nil, hrError("ConvertToContiguousBuffer", hr)
	}
	defer buffer.Release()
	var pointer uintptr
	var maxLength, length uint32
	if hr := buffer.call(bufferLock, uintptr(unsafe.Pointer(&pointer)), uintptr(unsafe.Pointer(&maxLength)), uintptr(unsafe.Pointer(&length))); failed(hr) {
		return nil, hrError("IMFMediaBuffer.Lock", hr)
	}
	defer buffer.call(bufferUnlock)
	if pointer == 0 || length == 0 {
		return nil, nil
	}
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(pointer)), int(length))...), nil
}

// variant is a minimal VARIANT for ICodecAPI.SetValue.
type variant struct {
	VT    uint16
	_     [3]uint16
	Value uint64
	_     uint64
}

func codecSetUINT32(codec *comObject, key GUID, value uint32) error {
	v := variant{VT: vtUI4, Value: uint64(value)}
	if hr := codec.call(9, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&v))); failed(hr) {
		return hrError("ICodecAPI.SetValue", hr)
	}
	return nil
}
