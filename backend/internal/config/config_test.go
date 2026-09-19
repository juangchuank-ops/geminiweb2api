package config

import "testing"

// TestNormalizeRepairsTheKnownBrokenLegacyDefault pins the one thing that makes
// a default fix reach an install that already exists.
//
// Normalize otherwise only fills in *empty* settings, and a fresh install writes
// a copy of every default into the settings file. So changing a default reaches
// new installs and nothing else: an existing install keeps the old value on disk
// and there is no symptom, because the value looks like a normal configuration.
//
// The value under test was shipped as a default by an earlier build. It points
// at the app *shell* rather than the origin, so every upstream call resolves to
// a URL that is not an endpoint — a failure that reads like a broken cookie.
func TestNormalizeRepairsTheKnownBrokenLegacyDefault(t *testing.T) {
	settings := Settings{}
	settings.Upstream.BaseURL = legacyDefaultBaseURL

	settings.Normalize(t.TempDir())

	def := DefaultSettings(t.TempDir())
	if settings.Upstream.BaseURL != def.Upstream.BaseURL {
		t.Errorf("base url = %q, want it repaired to %q", settings.Upstream.BaseURL, def.Upstream.BaseURL)
	}
}

// TestNormalizeLeavesDeliberateOverridesAlone is the other half of that rule: a
// repair must not become a reset.
//
// An operator pointing the gateway at a mirror has to keep that value. Only the
// exact legacy string is rewritten — a host that merely resembles it is left
// untouched.
func TestNormalizeLeavesDeliberateOverridesAlone(t *testing.T) {
	cases := []struct {
		name string
		base string
	}{
		{"a mirror", "https://gemini-mirror.internal"},
		{"a self-hosted relay", "http://127.0.0.1:9090"},
		{"the same host without the path", "https://gemini.google.com"},
		{"a path that merely resembles the legacy one", "https://gemini.google.com/app/extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := Settings{}
			settings.Upstream.BaseURL = tc.base

			settings.Normalize(t.TempDir())

			if settings.Upstream.BaseURL != tc.base {
				t.Errorf("base url = %q, want the override %q kept", settings.Upstream.BaseURL, tc.base)
			}
		})
	}
}

// TestNormalizeFillsRefreshSettings covers the fields a settings file written by
// an older build cannot carry.
//
// A zero interval is the dangerous one: the sweep compares `now - RefreshAt`
// against it, so a zero would make every account permanently due and turn the
// scheduler into a tight rotation loop against an endpoint that throttles.
func TestNormalizeFillsRefreshSettings(t *testing.T) {
	settings := Settings{}
	settings.Normalize(t.TempDir())

	def := DefaultSettings(t.TempDir())
	if settings.Refresh.IntervalMin != def.Refresh.IntervalMin {
		t.Errorf("interval = %d, want %d", settings.Refresh.IntervalMin, def.Refresh.IntervalMin)
	}
	if settings.Refresh.TimeoutSec != def.Refresh.TimeoutSec {
		t.Errorf("timeout = %d, want %d", settings.Refresh.TimeoutSec, def.Refresh.TimeoutSec)
	}
	if settings.Refresh.RetireAfterFailures != def.Refresh.RetireAfterFailures {
		t.Errorf("retire threshold = %d, want %d",
			settings.Refresh.RetireAfterFailures, def.Refresh.RetireAfterFailures)
	}
	// Enabled has no "zero" to repair — false is a legitimate choice — so it
	// must survive Normalize untouched.
	settings2 := Settings{}
	settings2.Refresh.Enabled = false
	settings2.Normalize(t.TempDir())
	if settings2.Refresh.Enabled {
		t.Error("Normalize turned the sweep back on; false is a deliberate setting, not a missing one")
	}
}

// TestNormalizeReportsChange pins the return value, because the caller writes
// the file back only when it is true. A repair that is not persisted leaves the
// settings file describing a configuration the process is not using.
func TestNormalizeReportsChange(t *testing.T) {
	fresh := DefaultSettings(t.TempDir())
	if fresh.Normalize(t.TempDir()) {
		t.Error("Normalize reported a change on a freshly defaulted settings struct")
	}

	broken := Settings{}
	broken.Upstream.BaseURL = legacyDefaultBaseURL
	if !broken.Normalize(t.TempDir()) {
		t.Error("Normalize did not report repairing a known-broken default")
	}
}
