package screenview

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"syscall"
	"unsafe"

	"github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/d3d11"
	"github.com/kirides/go-d3d/dxgi"
	"github.com/kirides/go-d3d/outputduplication"
	"golang.org/x/sys/windows"
)

const (
	maxDXGIAdapters     = 16
	maxDXGIOutputs      = 16
	dxgiFrameTimeoutMS  = 250
	initialDXGIAttempts = 4
	d3d11SDKVersion     = 7
)

var procD3D11CreateDevice = windows.NewLazySystemDLL("d3d11.dll").NewProc("D3D11CreateDevice")

type dxgiOutput struct {
	duplicator *outputduplication.OutputDuplicator
	bounds     image.Rectangle
	last       *image.RGBA
}

type dxgiCapturer struct {
	devices []dxgiDevice
	outputs []dxgiOutput
	bounds  image.Rectangle
}

type dxgiDevice struct {
	device  *d3d11.ID3D11Device
	context *d3d11.ID3D11DeviceContext
}

func newDXGICapturer() (*dxgiCapturer, error) {
	var factory *dxgi.IDXGIFactory1
	if err := dxgi.CreateDXGIFactory1(&factory); err != nil {
		return nil, fmt.Errorf("create DirectX adapter factory: %w", err)
	}
	defer factory.Release()

	capturer := &dxgiCapturer{}
	var attempts []error
	for adapterIndex := 0; adapterIndex < maxDXGIAdapters; adapterIndex++ {
		var adapter *dxgi.IDXGIAdapter1
		hr := factory.EnumAdapters1(uint32(adapterIndex), &adapter)
		if d3d.HRESULT(hr).Failed() {
			break
		}
		var desc dxgi.DXGI_ADAPTER_DESC1
		name := fmt.Sprintf("adapter %d", adapterIndex)
		if descHR := adapter.GetDesc1(&desc); !d3d.HRESULT(descHR).Failed() {
			name = fmt.Sprintf("adapter %d %q flags=%s", adapterIndex, desc.DescriptionString(), desc.Flags)
		}
		device, context, deviceErr := newD3D11DeviceForAdapter(adapter)
		adapter.Release()
		if deviceErr != nil {
			attempts = append(attempts, fmt.Errorf("%s: create device: %w", name, deviceErr))
			continue
		}

		deviceUsed := false
		for outputIndex := 0; outputIndex < maxDXGIOutputs; outputIndex++ {
			duplicator, duplicateErr := outputduplication.NewIDXGIOutputDuplication(device, context, uint(outputIndex))
			if duplicateErr != nil {
				if outputIndex == 0 {
					attempts = append(attempts, fmt.Errorf("%s output 0: %w", name, duplicateErr))
				}
				break
			}
			bounds, boundsErr := duplicator.GetBounds()
			if boundsErr != nil || bounds.Empty() {
				duplicator.Release()
				if boundsErr != nil {
					attempts = append(attempts, fmt.Errorf("%s output %d bounds: %w", name, outputIndex, boundsErr))
				} else {
					attempts = append(attempts, fmt.Errorf("%s output %d has empty desktop bounds", name, outputIndex))
				}
				continue
			}
			if !primaryDisplayBounds(bounds) {
				duplicator.Release()
				continue
			}
			duplicator.UpdatePointerInfo = true
			duplicator.DrawPointer = true
			capturer.outputs = append(capturer.outputs, dxgiOutput{duplicator: duplicator, bounds: bounds})
			capturer.bounds = bounds
			deviceUsed = true
			break
		}
		if deviceUsed {
			capturer.devices = append(capturer.devices, dxgiDevice{device: device, context: context})
			break
		} else {
			context.Release()
			device.Release()
		}
	}
	if len(capturer.outputs) == 0 || capturer.bounds.Empty() {
		capturer.Close()
		if len(attempts) == 0 {
			return nil, errors.New("DirectX found no attached desktop outputs")
		}
		return nil, fmt.Errorf("DirectX found no capturable desktop outputs: %w", errors.Join(attempts...))
	}
	return capturer, nil
}

func primaryDisplayBounds(bounds image.Rectangle) bool {
	return image.Pt(0, 0).In(bounds)
}

// newD3D11DeviceForAdapter creates the capture device on the adapter that owns
// the output. In an RDP or indirect-display session that adapter can be marked
// as software, so selecting the first hardware adapter is not sufficient.
func newD3D11DeviceForAdapter(adapter *dxgi.IDXGIAdapter1) (*d3d11.ID3D11Device, *d3d11.ID3D11DeviceContext, error) {
	featureLevels := [...]uint32{0xc100, 0xc000, 0xb100, 0xb000, 0xa100, 0xa000}
	selectedFeatureLevel := uint32(0)
	var device *d3d11.ID3D11Device
	var context *d3d11.ID3D11DeviceContext
	const d3dDriverTypeUnknown = 0
	result, _, _ := syscall.SyscallN(
		procD3D11CreateDevice.Addr(),
		uintptr(unsafe.Pointer(adapter)),
		uintptr(d3dDriverTypeUnknown),
		0,
		0,
		uintptr(unsafe.Pointer(&featureLevels[0])),
		uintptr(len(featureLevels)),
		uintptr(d3d11SDKVersion),
		uintptr(unsafe.Pointer(&device)),
		uintptr(unsafe.Pointer(&selectedFeatureLevel)),
		uintptr(unsafe.Pointer(&context)),
	)
	if d3d.HRESULT(result).Failed() {
		return nil, nil, d3d.HRESULT(result)
	}
	if device == nil || context == nil {
		if context != nil {
			context.Release()
		}
		if device != nil {
			device.Release()
		}
		return nil, nil, errors.New("D3D11CreateDevice returned an empty device or context")
	}
	return device, context, nil
}

func (c *dxgiCapturer) Close() {
	for index := range c.outputs {
		if c.outputs[index].duplicator != nil {
			c.outputs[index].duplicator.Release()
			c.outputs[index].duplicator = nil
		}
	}
	c.outputs = nil
	for index := range c.devices {
		if c.devices[index].context != nil {
			c.devices[index].context.Release()
		}
		if c.devices[index].device != nil {
			c.devices[index].device.Release()
		}
	}
	c.devices = nil
}

func (c *dxgiCapturer) Capture() (*image.RGBA, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, c.bounds.Dx(), c.bounds.Dy()))
	for index := range c.outputs {
		output := &c.outputs[index]
		frame := image.NewRGBA(image.Rect(0, 0, output.bounds.Dx(), output.bounds.Dy()))
		attempts := 1
		if output.last == nil {
			attempts = initialDXGIAttempts
		}
		var captureErr error
		for attempt := 0; attempt < attempts; attempt++ {
			captureErr = output.duplicator.GetImage(frame, dxgiFrameTimeoutMS)
			if captureErr == nil {
				output.last = frame
				break
			}
			if !errors.Is(captureErr, outputduplication.ErrNoImageYet) {
				break
			}
		}
		if captureErr != nil {
			if errors.Is(captureErr, outputduplication.ErrNoImageYet) && output.last != nil {
				frame = output.last
			} else {
				return nil, fmt.Errorf("capture DirectX output %d: %w", index, captureErr)
			}
		}
		destination := output.bounds.Sub(c.bounds.Min)
		draw.Draw(canvas, destination, frame, frame.Bounds().Min, draw.Src)
	}
	return canvas, nil
}
