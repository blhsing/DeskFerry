//go:build windows

package sharebanner

import (
	"testing"

	"github.com/lxn/win"
)

func TestStackedTopPlacesBannersBelowEarlierOnes(t *testing.T) {
	// Banners 400 wide and 60 high, centred at x=1000, 8 px from the top.
	first := stackedTop(1000, 8, 400, 60, 8, nil)
	if first != 8 {
		t.Fatalf("the first banner sits at %d; want the top (8)", first)
	}
	firstRect := win.RECT{Left: 800, Top: first, Right: 1200, Bottom: first + 60}
	second := stackedTop(1000, 8, 400, 60, 8, []win.RECT{firstRect})
	if second != 76 {
		t.Fatalf("the second banner sits at %d; want just below the first (76)", second)
	}
	secondRect := win.RECT{Left: 800, Top: second, Right: 1200, Bottom: second + 60}
	// Listed out of order, both earlier banners are still cleared.
	if third := stackedTop(1000, 8, 400, 60, 8, []win.RECT{secondRect, firstRect}); third != 144 {
		t.Fatalf("the third banner sits at %d; want below the second (144)", third)
	}
	// Once the first closes, the second moves back up.
	if up := stackedTop(1000, 8, 400, 60, 8, nil); up != 8 {
		t.Fatalf("with no earlier banner left it sits at %d; want 8", up)
	}
	// A banner dragged elsewhere on the screen does not push the stack down.
	dragged := win.RECT{Left: 100, Top: 8, Right: 500, Bottom: 68}
	if top := stackedTop(1000, 8, 400, 60, 8, []win.RECT{dragged}); top != 8 {
		t.Fatalf("a banner off to the side moved this one to %d", top)
	}
}
