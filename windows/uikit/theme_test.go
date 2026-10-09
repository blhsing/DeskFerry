//go:build windows

package uikit

import "testing"

func TestToneForStatusVocabulary(t *testing.T) {
	cases := map[string]Tone{
		"Running":                  ToneSuccess,
		"Connected":                ToneSuccess,
		"Online":                   ToneSuccess,
		"2 active":                 ToneSuccess,
		"Checking":                 ToneWarning,
		"Connecting":               ToneWarning,
		"Reconnecting":             ToneWarning,
		"Starting":                 ToneWarning,
		"Check relay":              ToneWarning,
		"Offline":                  ToneDanger,
		"Error":                    ToneDanger,
		"Self-test failed":         ToneDanger,
		"Disconnected":             ToneDanger,
		"Stopped":                  ToneNeutral,
		"Unknown":                  ToneNeutral,
		"No active sessions":       ToneNeutral,
		"Not installed":            ToneNeutral,
		"":                         ToneNeutral,
		"something else":           ToneNeutral,
		"Work agent offline":       ToneDanger,
		"Connected · Work offline": ToneDanger,
	}
	for text, want := range cases {
		if got := ToneFor(text); got != want {
			t.Errorf("ToneFor(%q) = %s, want %s", text, got, want)
		}
	}
}
