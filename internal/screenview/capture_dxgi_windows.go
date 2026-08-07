package screenview

import (
	"errors"
	"fmt"
	"image"
	"image/draw"

	"github.com/kirides/go-d3d/d3d11"
	"github.com/kirides/go-d3d/outputduplication"
)

const (
	maxDXGIOutputs      = 16
	dxgiFrameTimeoutMS  = 250
	initialDXGIAttempts = 4
)

type dxgiOutput struct {
	duplicator *outputduplication.OutputDuplicator
	bounds     image.Rectangle
	last       *image.RGBA
}

type dxgiCapturer struct {
	device  *d3d11.ID3D11Device
	context *d3d11.ID3D11DeviceContext
	outputs []dxgiOutput
	bounds  image.Rectangle
}

func newDXGICapturer() (*dxgiCapturer, error) {
	device, context, err := d3d11.NewD3D11Device()
	if err != nil {
		return nil, err
	}
	capturer := &dxgiCapturer{device: device, context: context}
	for index := 0; index < maxDXGIOutputs; index++ {
		duplicator, duplicateErr := outputduplication.NewIDXGIOutputDuplication(device, context, uint(index))
		if duplicateErr != nil {
			if index == 0 {
				capturer.Close()
				return nil, duplicateErr
			}
			break
		}
		bounds, boundsErr := duplicator.GetBounds()
		if boundsErr != nil || bounds.Empty() {
			duplicator.Release()
			capturer.Close()
			if boundsErr != nil {
				return nil, boundsErr
			}
			return nil, errors.New("DirectX output has empty desktop bounds")
		}
		duplicator.UpdatePointerInfo = true
		duplicator.DrawPointer = true
		capturer.outputs = append(capturer.outputs, dxgiOutput{duplicator: duplicator, bounds: bounds})
		if len(capturer.outputs) == 1 {
			capturer.bounds = bounds
		} else {
			capturer.bounds = capturer.bounds.Union(bounds)
		}
	}
	if len(capturer.outputs) == 0 || capturer.bounds.Empty() {
		capturer.Close()
		return nil, errors.New("DirectX found no attached desktop outputs")
	}
	return capturer, nil
}

func (c *dxgiCapturer) Close() {
	for index := range c.outputs {
		if c.outputs[index].duplicator != nil {
			c.outputs[index].duplicator.Release()
			c.outputs[index].duplicator = nil
		}
	}
	c.outputs = nil
	if c.context != nil {
		c.context.Release()
		c.context = nil
	}
	if c.device != nil {
		c.device.Release()
		c.device = nil
	}
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
