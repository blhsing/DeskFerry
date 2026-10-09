//go:build windows

package uikit

import (
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

var (
	fontFamiliesOnce sync.Once
	fontFamilies     map[string]bool
	fontFamilyMu     sync.Mutex
	enumFamilyFunc   = syscall.NewCallback(enumFontFamilyCallback)
)

func enumFontFamilyCallback(logFont *win.LOGFONT, _ uintptr, _ uint32, _ uintptr) uintptr {
	name := syscall.UTF16ToString(logFont.LfFaceName[:])
	if name != "" && !strings.HasPrefix(name, "@") {
		fontFamilyMu.Lock()
		fontFamilies[strings.ToLower(name)] = true
		fontFamilyMu.Unlock()
	}
	return 1
}

func loadFontFamilies() {
	fontFamilies = make(map[string]bool)
	hdc := win.GetDC(0)
	if hdc == 0 {
		return
	}
	defer win.ReleaseDC(0, hdc)
	logFont := win.LOGFONT{LfCharSet: win.DEFAULT_CHARSET}
	_, _, _ = procEnumFontFamiliesExW.Call(uintptr(hdc), uintptr(unsafe.Pointer(&logFont)), enumFamilyFunc, 0, 0)
}

// FontInstalled reports whether a GDI font family is installed.
func FontInstalled(family string) bool {
	fontFamiliesOnce.Do(loadFontFamilies)
	fontFamilyMu.Lock()
	defer fontFamilyMu.Unlock()
	return fontFamilies[strings.ToLower(family)]
}

func firstInstalled(fallback string, candidates ...string) string {
	for _, candidate := range candidates {
		if FontInstalled(candidate) {
			return candidate
		}
	}
	return fallback
}

// TextFamily is the body font: Segoe UI Variable Text on Windows 11, Segoe UI otherwise.
func TextFamily() string {
	return firstInstalled("Segoe UI", "Segoe UI Variable Text", "Segoe UI")
}

// DisplayFamily is the title font family.
func DisplayFamily() string {
	return firstInstalled(TextFamily(), "Segoe UI Variable Display", "Segoe UI")
}

// semiboldFamily returns a semibold face family name and whether it is a
// genuine semibold family (otherwise the caller should request bold).
func semiboldFamily(display bool) (string, bool) {
	candidates := []string{"Segoe UI Variable Text Semibold", "Segoe UI Variable Text Semibol", "Segoe UI Semibold"}
	if display {
		candidates = []string{"Segoe UI Variable Display Semib", "Segoe UI Variable Display Semibold", "Segoe UI Semibold"}
	}
	for _, candidate := range candidates {
		if FontInstalled(candidate) {
			return candidate, true
		}
	}
	if display {
		return DisplayFamily(), false
	}
	return TextFamily(), false
}

// MonoFamily is Cascadia Mono with Consolas as the fallback.
func MonoFamily() string {
	return firstInstalled("Consolas", "Cascadia Mono", "Consolas")
}

func semibold(display bool, pointSize int) Font {
	family, ok := semiboldFamily(display)
	return Font{Family: family, PointSize: pointSize, Bold: !ok}
}

// BodyFont is the default control font.
func BodyFont() Font { return Font{Family: TextFamily(), PointSize: 10} }

// LabelFont is used for field labels.
func LabelFont() Font { return Font{Family: TextFamily(), PointSize: 10} }

// CaptionFont is used for helper text, captions and timestamps.
func CaptionFont() Font { return Font{Family: TextFamily(), PointSize: 9} }

// CaptionStrongFont is used for tile captions.
func CaptionStrongFont() Font { return semibold(false, 9) }

// TitleFont is used for card titles.
func TitleFont() Font { return semibold(true, 11) }

// DisplayFont is used for window titles.
func DisplayFont() Font { return semibold(true, 16) }

// MetricFont is used for status tile values.
func MetricFont() Font { return semibold(true, 12) }

// MonoFont is used for logs, room details and command output.
func MonoFont() Font { return Font{Family: MonoFamily(), PointSize: 9} }

var (
	walkFontsMu sync.Mutex
	walkFonts   = map[string]*walk.Font{}
)

// walkFont creates (once) a walk font for custom painting.
func walkFont(font Font) *walk.Font {
	key := font.Family + "|" + strconv.FormatBool(font.Bold) + "|" + strconv.Itoa(font.PointSize)
	walkFontsMu.Lock()
	defer walkFontsMu.Unlock()
	if existing := walkFonts[key]; existing != nil {
		return existing
	}
	style := walk.FontStyle(0)
	if font.Bold {
		style |= walk.FontBold
	}
	created, err := walk.NewFont(font.Family, font.PointSize, style)
	if err != nil {
		created, _ = walk.NewFont("Segoe UI", font.PointSize, style)
	}
	walkFonts[key] = created
	return created
}
