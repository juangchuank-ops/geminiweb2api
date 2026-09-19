package gemini

import "testing"

func TestResolveModel(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantID  string
		wantHit bool
	}{
		{"exact", "gemini-flash", "gemini-flash", true},
		{"case insensitive", "Gemini-Flash", "gemini-flash", true},
		{"whitespace", "  gemini-pro  ", "gemini-pro", true},
		{"openai alias", "gpt-4o", "gemini-flash", true},
		{"claude alias", "claude-3-5-sonnet", "gemini-flash", true},
		{"upstream style", "gemini-2.5-flash", "gemini-flash", true},
		{"provider prefix", "google/gemini-flash", "gemini-flash", true},
		{"suffix hint", "gemini-flash:free", "gemini-flash", true},
		{"unknown falls back", "totally-made-up", "gemini-flash", false},
		{"empty falls back", "", "gemini-flash", false},
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
		case 0, CapacityBasic, CapacityAdvanced, CapacityPlus:
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
	spec, ok := LookupModel("gemini-flash-thinking")
	if !ok {
		t.Fatal("the thinking mode is not in the catalogue; reasoning is unreachable by name")
	}
	if spec.Number != modeThinking {
		t.Errorf("thinking mode number = %d, want %d", spec.Number, modeThinking)
	}
	if spec.Think != ThinkExtended {
		t.Errorf("thinking mode think slot = %d, want %d", spec.Think, ThinkExtended)
	}

	// And the aliases that clients actually send for a reasoning model must
	// land on it rather than on the plain Flash entry.
	for _, alias := range []string{"deepseek-reasoner"} {
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
