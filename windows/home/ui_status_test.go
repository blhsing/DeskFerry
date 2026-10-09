//go:build windows

package homewindows

import (
	"testing"

	"deskferry/windows/uikit"
)

func TestOverallStatus(t *testing.T) {
	cases := []struct {
		tunnel, work string
		text         string
		tone         uikit.Tone
	}{
		{"Running", "Online", "Connected", uikit.ToneSuccess},
		{"Running", "Offline", "Work agent offline", uikit.ToneDanger},
		{"Stopped", "Offline", "Work agent offline", uikit.ToneDanger},
		{"Stopped", "Check relay", "Check relay", uikit.ToneWarning},
		{"Stopped", "Checking", "Checking", uikit.ToneWarning},
		{"Stopped", "Online", "Ready to connect", uikit.ToneInfo},
		{"Stopped", "", "Stopped", uikit.ToneNeutral},
		{"Running", "", "Running", uikit.ToneSuccess},
	}
	for _, tc := range cases {
		text, tone := overallStatus(tc.tunnel, tc.work)
		if text != tc.text || tone != tc.tone {
			t.Errorf("overallStatus(%q, %q) = %q/%s, want %q/%s", tc.tunnel, tc.work, text, tone, tc.text, tc.tone)
		}
	}
}

func TestRDPSessionText(t *testing.T) {
	if got := rdpSessionText(false, 3); got != "No active sessions" {
		t.Fatalf("stopped tunnel = %q", got)
	}
	if got := rdpSessionText(true, 0); got != "No active sessions" {
		t.Fatalf("idle tunnel = %q", got)
	}
	if got := rdpSessionText(true, 2); got != "2 active" {
		t.Fatalf("active tunnel = %q", got)
	}
	if uikit.ToneFor(rdpSessionText(true, 2)) != uikit.ToneSuccess || uikit.ToneFor(rdpSessionText(true, 0)) != uikit.ToneNeutral {
		t.Fatal("session chip tones do not follow the status vocabulary")
	}
}
