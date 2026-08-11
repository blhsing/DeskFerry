package screenview

import (
	"errors"
	"image"
	"strings"
	"testing"
)

func TestCaptureWithRetryRecoversFromTransientDesktopFailure(t *testing.T) {
	attempts := 0
	got, err := captureWithRetry(func() (*image.RGBA, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("desktop is switching")
		}
		return image.NewRGBA(image.Rect(0, 0, 2, 2)), nil
	}, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || got.Bounds().Dx() != 2 {
		t.Fatalf("attempts=%d bounds=%v", attempts, got.Bounds())
	}
}

func TestCaptureWithRetryReturnsLastFailure(t *testing.T) {
	attempts := 0
	_, err := captureWithRetry(func() (*image.RGBA, error) {
		attempts++
		return nil, errors.New("desktop unavailable")
	}, 3, 0)
	if err == nil || !strings.Contains(err.Error(), "after 3 attempts") || !strings.Contains(err.Error(), "desktop unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts=%d, want 3", attempts)
	}
}
