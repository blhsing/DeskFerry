//go:build windows

package homewindows

import "testing"

func TestParseScreenZoom(t *testing.T) {
	tests := []struct {
		input string
		want  float64
		ok    bool
	}{
		{"Auto Fit", 0, true},
		{"fit", 0, true},
		{"137.5%", 1.375, true},
		{"10", 0.10, true},
		{"1600%", 16, true},
		{"9%", 0, false},
		{"1601%", 0, false},
		{"large", 0, false},
	}
	for _, test := range tests {
		got, ok := parseScreenZoom(test.input)
		if ok != test.ok || got != test.want {
			t.Errorf("parseScreenZoom(%q) = (%v, %t), want (%v, %t)", test.input, got, ok, test.want, test.ok)
		}
	}
}
