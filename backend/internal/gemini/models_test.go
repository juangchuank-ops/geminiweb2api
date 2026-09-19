package gemini

import (
	"strings"
	"testing"
)

func TestResolveModel(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantID  string
		wantHit bool
	}{
		{"exact", "gemini-3.8-flash", "gemini-3.8-flash", true},
		{"case insensitive", "Gemini-3.8-Flash", "gemini-3.8-flash", true},
		{"whitespace", "  gemini-3.1-pro  ", "gemini-3.1-pro", true},
		{"openai alias", "gpt-4o", "gemini-3.8-flash", true},
		{"claude alias", "claude-3-5-sonnet", "gemini-3.8-flash", true},
		{"upstream style", "gemini-2.5-flash", "gemini-3.8-flash", true},
		{"same model, previous name", "gemini-3.7-flash", "gemini-3.8-flash", true},
		{"provider prefix", "google/gemini-3.8-flash", "gemini-3.8-flash", true},
		{"suffix hint", "gemini-3.8-flash:free", "gemini-3.8-flash", true},
		{"unknown falls back", "totally-made-up", "gemini-3.8-flash", false},
		{"empty falls back", "", "gemini-3.8-flash", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, hit := ResolveModel(tc.input)
			if spec.ID != tc.wantID {
				t.Errorf("ResolveModel(%q).ID = %q, want %q", tc.input, spec.ID, tc.wantID)
			}
			if hit != tc.wantHit {
				t.Errorf("ResolveModel(%q) hit = %v, want %v", tc.input, hit, tc.wantHit)
			}
			// Whatever comes back must be usable: the mode number is what
			// routes the request, so it can never be zero.
			if spec.Number == 0 {
				t.Errorf("resolved model has no mode number: %#v", spec)
			}
		})
	}
}

func TestBuiltinModelsAreUniqueAndComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range BuiltinModels() {
		if seen[spec.ID] {
			t.Errorf("duplicate model id %q", spec.ID)
		}
		seen[spec.ID] = true
		if spec.Number == 0 {
			t.Errorf("%s has no mode number", spec.ID)
		}
		// The hex id and the capacity claim travel together: a header without
		// an id is unbuildable, and an id without a capacity makes a tier claim
		// of zero, which the upstream reads as a malformed header.
		switch {
		case spec.Upstream == "" && spec.Capacity != 0:
			t.Errorf("%s has a capacity but no upstream id, so no header can carry it", spec.ID)
		case spec.Upstream != "" && spec.Capacity == 0:
			t.Errorf("%s has an upstream id but no capacity", spec.ID)
		}
		switch spec.Capacity {
		case 0, CapacityFree, CapacityPaid:
		default:
			t.Errorf("%s has an unknown capacity %d", spec.ID, spec.Capacity)
		}
		// Slot 17 only carries two values in captured traffic.
		switch spec.Think {
		case ThinkDefault, ThinkExtended:
		default:
			t.Errorf("%s has an unknown think slot value %d", spec.ID, spec.Think)
		}
	}
	if !seen[DefaultModelID] {
		t.Errorf("the default model %q is not in the catalogue", DefaultModelID)
	}
}

// TestThinkingModesAreListed guards the one capability that has no other way to
// be reached: the web UI exposes reasoning as a separate toggle, not as a model,
// so a client can only ask for it by name.
func TestThinkingModesAreListed(t *testing.T) {
	spec, ok := LookupModel("gemini-3.8-flash-thinking")
	if !ok {
		t.Fatal("the thinking mode is not in the catalogue; reasoning is unreachable by name")
	}
	if spec.Number != modeThinking {
		t.Errorf("thinking mode number = %d, want %d", spec.Number, modeThinking)
	}
	if spec.Think != ThinkExtended {
		t.Errorf("thinking mode think slot = %d, want %d", spec.Think, ThinkExtended)
	}
	// It must stay on the headerless path. One reference implementation claims
	// the thinking mode shares 3.8 Flash's hex id, but the headerless route is
	// verified working and a wrong id is a 1052 on every reasoning request.
	if spec.Upstream != "" {
		t.Errorf("thinking mode now carries hex %q; verify it against a capture before trusting it", spec.Upstream)
	}

	// And the aliases that clients actually send for a reasoning model must
	// land on it rather than on the plain Flash entry.
	for _, alias := range []string{"deepseek-reasoner", "gemini-flash-thinking"} {
		got, ok := LookupModel(alias)
		if !ok {
			t.Errorf("%s did not resolve", alias)
			continue
		}
		if got.Number != modeThinking {
			t.Errorf("%s resolved to %s (mode %d), want a thinking mode", alias, got.ID, got.Number)
		}
	}
}

// TestCatalogueIsKeyedOnTheMarketedName pins the relationship the catalogue got
// backwards once: the name a human reads off a model list is the primary entry,
// and the internal names this project used to publish are aliases pointing at
// it. The failure mode is not a crash — it is a model list full of names no
// client will ever send, which is indistinguishable from a broken catalogue to
// anyone reading /v1/models.
func TestCatalogueIsKeyedOnTheMarketedName(t *testing.T) {
	primary := map[string]bool{}
	for _, spec := range BuiltinModels() {
		primary[spec.ID] = true
		if !strings.HasPrefix(spec.ID, "gemini-") {
			t.Errorf("catalogue id %q does not look like a marketed model name", spec.ID)
		}
	}

	// The marketed names must be primary, not aliases.
	for _, want := range []string{"gemini-3.8-flash", "gemini-3.1-pro", "gemini-3.5-flash-lite"} {
		if !primary[want] {
			t.Errorf("%q is not a primary catalogue id; the model list would not offer it", want)
		}
	}

	// And every name this project used to publish must still resolve, or an
	// operator upgrading in place gets a 400 for the model their client has
	// pinned.
	retired := map[string]string{
		"gemini-flash":               "gemini-3.8-flash",
		"gemini-flash-plus":          "gemini-3.8-flash",
		"gemini-flash-advanced":      "gemini-3.8-flash",
		"gemini-flash-thinking":      "gemini-3.8-flash-thinking",
		"gemini-flash-thinking-lite": "gemini-3.8-flash-thinking-lite",
		"gemini-pro":                 "gemini-3.1-pro",
		"gemini-pro-plus":            "gemini-3.1-pro",
		"gemini-pro-advanced":        "gemini-3.1-pro",
		"gemini-flash-lite":          "gemini-3.5-flash-lite",
		"gemini-flash-lite-plus":     "gemini-3.5-flash-lite",
		"gemini-flash-lite-advanced": "gemini-3.5-flash-lite",
	}
	for old, want := range retired {
		got, ok := LookupModel(old)
		if !ok {
			t.Errorf("%q no longer resolves; clients with it pinned break on upgrade", old)
			continue
		}
		if got.ID != want {
			t.Errorf("%q resolved to %q, want %q", old, got.ID, want)
		}
		if got.Number == 0 {
			t.Errorf("%q resolved to a model with no mode number", old)
		}
	}
}

// TestHexIDsIdentifyModelsNotCapacities pins the other half of the same
// mistake. The catalogue used to list one hex id three times as the "basic",
// "plus" and "advanced" variants of a single model. Two entries may share a hex
// id only when they are the same model in two *modes* (3.8 Flash and its
// thinking mode), never when they differ only in capacity — that shape is
// exactly what let a newer model masquerade as a subscription tier.
func TestHexIDsIdentifyModelsNotCapacities(t *testing.T) {
	type key struct {
		hex  string
		mode int
	}
	seen := map[key]string{}
	for _, spec := range BuiltinModels() {
		if spec.Upstream == "" {
			continue
		}
		k := key{spec.Upstream, spec.Number}
		if prev, dup := seen[k]; dup {
			t.Errorf("%s and %s share hex %s and mode %d; one of them is a renamed capacity, not a model",
				prev, spec.ID, spec.Upstream, spec.Number)
		}
		seen[k] = spec.ID
	}
}

// TestDefaultModelIsTheCurrentFlash guards the specific regression that started
// all of this: the default pointed at the previous generation's hex id, because
// the current Flash had been filed under a "Plus" name and the id left in the
// default slot was the one before it.
func TestDefaultModelIsTheCurrentFlash(t *testing.T) {
	spec, ok := LookupModel(DefaultModelID)
	if !ok {
		t.Fatalf("default model %q is not in the catalogue", DefaultModelID)
	}
	if spec.Upstream != "56fdd199312815e2" {
		t.Errorf("default model carries hex %q, want the current Flash id 56fdd199312815e2", spec.Upstream)
	}
	if spec.Number != modeFast {
		t.Errorf("default model mode = %d, want %d (FAST)", spec.Number, modeFast)
	}
	if spec.Capacity == 0 {
		t.Error("default model makes no capacity claim, but it carries a hex id, so the header would be malformed")
	}
}

func TestBuiltinModelsAreMutableSafe(t *testing.T) {
	first := BuiltinModels()
	if len(first) == 0 {
		t.Fatal("empty catalogue")
	}
	original := first[0].ID
	first[0].ID = "mutated"

	if BuiltinModels()[0].ID != original {
		t.Error("BuiltinModels leaks its backing array; a caller can corrupt the catalogue")
	}
}

func TestParseSessionExtractsShellValues(t *testing.T) {
	// The shell is a large HTML document; only a handful of fields matter and
	// they are embedded in a script block rather than returned as JSON.
	html := `<!DOCTYPE html><html><head><script>
	window.WIZ_global_data = {
	  "SNlM0e":"AO-x_abc123:1700000000",
	  "cfb2h":"boq_assistant-bard-web-server_20260919.08_p0",
	  "FdrFJe":"-1234567890123456789",
	  "TuX5cc":"en",
	  "qKIAYe":"feeds/mcudyrk2a4khkz"
	};
	</script></head><body></body></html>`

	session := parseSession(html)
	if session.AccessToken != "AO-x_abc123:1700000000" {
		t.Errorf("access token = %q", session.AccessToken)
	}
	if session.BuildLabel != "boq_assistant-bard-web-server_20260919.08_p0" {
		t.Errorf("build label = %q", session.BuildLabel)
	}
	if session.SessionID != "-1234567890123456789" {
		t.Errorf("session id = %q", session.SessionID)
	}
	if session.Language != "en" {
		t.Errorf("language = %q", session.Language)
	}
	if !session.Usable() {
		t.Error("session should be usable")
	}
	if !session.Authenticated() {
		t.Error("a session with both a token and a session id is authenticated")
	}
}

func TestSessionAuthenticatedRequiresSessionID(t *testing.T) {
	// A guest page still yields an access token. Treating that as "signed in"
	// is what makes an expired cookie invisible: requests keep succeeding, as
	// a guest, with the guest's quota.
	guest := &Session{AccessToken: "AO-token"}
	if !guest.Usable() {
		t.Error("a guest session is still usable")
	}
	if guest.Authenticated() {
		t.Error("a guest session must not report as authenticated")
	}
}

func TestParseSessionOnEmptyPage(t *testing.T) {
	session := parseSession("<html><body>Sign in</body></html>")
	if session.Usable() {
		t.Error("a page with no shell values must not be usable")
	}
}

func TestAppPath(t *testing.T) {
	if got := appPath(0); got != "/app" {
		t.Errorf("appPath(0) = %q, want /app", got)
	}
	if got := appPath(1); got != "/u/1/app" {
		t.Errorf("appPath(1) = %q, want /u/1/app", got)
	}
}

func TestAccountLabelIsStableAndShort(t *testing.T) {
	cred := Credential{PSID: "g.a000abcdefghijklmnop"}
	label := AccountLabel(cred)
	if label != "acct-abcdefghij" {
		t.Errorf("AccountLabel = %q, want acct-abcdefghij", label)
	}
	if label == cred.PSID {
		t.Error("AccountLabel must not return the credential itself")
	}
	// The same input must always give the same label, or the console would show
	// a different name on every page load.
	if AccountLabel(cred) != label {
		t.Error("AccountLabel is not deterministic")
	}
}
