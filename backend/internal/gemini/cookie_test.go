package gemini

import (
	"strings"
	"testing"
)

func TestParseCookies(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantPSID string
		wantTS   string
		wantLen  int
	}{
		{
			name:     "plain pair",
			raw:      "__Secure-1PSID=g.a000abc; __Secure-1PSIDTS=sidts-xyz",
			wantPSID: "g.a000abc",
			wantTS:   "sidts-xyz",
			wantLen:  2,
		},
		{
			name:     "value containing equals",
			raw:      "__Secure-1PSID=g.a000a=b=c; SID=other",
			wantPSID: "g.a000a=b=c",
			wantLen:  2,
		},
		{
			name:     "leading Cookie label",
			raw:      "Cookie: __Secure-1PSID=g.a000abc",
			wantPSID: "g.a000abc",
			wantLen:  1,
		},
		{
			name:     "multi-line paste",
			raw:      "__Secure-1PSID=g.a000abc\n__Secure-1PSIDTS=sidts-xyz",
			wantPSID: "g.a000abc",
			wantTS:   "sidts-xyz",
			wantLen:  2,
		},
		{
			name:    "empty segments ignored",
			raw:     ";; __Secure-1PSID=g.a000abc ;;",
			wantLen: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jar := ParseCookies(tc.raw)
			if jar.Len() != tc.wantLen {
				t.Fatalf("cookie count = %d, want %d (jar: %s)", jar.Len(), tc.wantLen, jar.String())
			}
			if tc.wantPSID != "" && jar.Get(CookiePSID) != tc.wantPSID {
				t.Errorf("PSID = %q, want %q", jar.Get(CookiePSID), tc.wantPSID)
			}
			if tc.wantTS != "" && jar.Get(CookiePSIDTS) != tc.wantTS {
				t.Errorf("PSIDTS = %q, want %q", jar.Get(CookiePSIDTS), tc.wantTS)
			}
		})
	}
}

func TestCookieJarMergeDirections(t *testing.T) {
	// Merge must not overwrite; Override must. Getting these the wrong way round
	// is invisible until either a rotation is silently discarded (Merge
	// overwriting) or an operator's fresh paste is silently discarded by a
	// stale preflight cookie (Override used where Merge belongs).
	current := ParseCookies("__Secure-1PSID=old-psid; __Secure-1PSIDTS=old-ts")
	other := ParseCookies("__Secure-1PSID=new-psid; __Secure-1PSIDTS=new-ts; EXTRA=1")

	merged := current.Clone()
	merged.Merge(other)
	if merged.Get(CookiePSID) != "old-psid" {
		t.Errorf("Merge overwrote an existing value: PSID = %q", merged.Get(CookiePSID))
	}
	if merged.Get("EXTRA") != "1" {
		t.Errorf("Merge failed to add a missing cookie")
	}

	overridden := current.Clone()
	overridden.Override(other)
	if overridden.Get(CookiePSIDTS) != "new-ts" {
		t.Errorf("Override kept the old value: PSIDTS = %q", overridden.Get(CookiePSIDTS))
	}
	if overridden.Get(CookiePSID) != "new-psid" {
		t.Errorf("Override kept the old PSID: %q", overridden.Get(CookiePSID))
	}
}

func TestParseSetCookie(t *testing.T) {
	cases := []struct {
		raw       string
		wantName  string
		wantValue string
		wantOK    bool
	}{
		{"__Secure-1PSIDTS=sidts-new; Path=/; Domain=.google.com; Secure; HttpOnly", CookiePSIDTS, "sidts-new", true},
		{"NID=511; expires=Wed, 21 Oct 2026 07:28:00 GMT", "NID", "511", true},
		{"", "", "", false},
		{"novalue", "", "", false},
		{"empty=", "", "", false},
	}
	for _, tc := range cases {
		name, value, ok := ParseSetCookie(tc.raw)
		if ok != tc.wantOK || name != tc.wantName || value != tc.wantValue {
			t.Errorf("ParseSetCookie(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.raw, name, value, ok, tc.wantName, tc.wantValue, tc.wantOK)
		}
	}
}

func TestCollectSetCookiesSplitsOnCommaSafely(t *testing.T) {
	// A naive comma split breaks on `expires=Wed, 21 Oct ...`, which is present
	// on every Google cookie. The rotation path depends on this: a bad split
	// either loses the new __Secure-1PSIDTS or invents a cookie named "21 Oct".
	headers := []string{
		"__Secure-1PSIDTS=sidts-new; expires=Wed, 21 Oct 2026 07:28:00 GMT; path=/; domain=.google.com",
		"NID=511; expires=Thu, 22 Oct 2026 07:28:00 GMT, SID=abc; path=/; domain=.google.com",
	}
	jar := CollectSetCookies(headers)

	if got := jar.Get(CookiePSIDTS); got != "sidts-new" {
		t.Errorf("PSIDTS = %q, want sidts-new", got)
	}
	if got := jar.Get("SID"); got != "abc" {
		t.Errorf("SID = %q, want abc (comma-joined header not split)", got)
	}
	for _, name := range jar.Names() {
		if strings.Contains(name, " ") {
			t.Errorf("invented a cookie name with a space: %q", name)
		}
	}
}

func TestCredentialJarPrefersStoredValues(t *testing.T) {
	// After a rotation the split PSIDTS is newer than the raw string it was
	// parsed from. If Jar() let the raw string win, every rotation would be
	// discarded the moment it succeeded.
	cred := Credential{
		Cookie: "__Secure-1PSID=g.a000abc; __Secure-1PSIDTS=stale",
		PSID:   "g.a000abc",
		PSIDTS: "fresh",
	}
	jar := cred.Jar()
	if got := jar.Get(CookiePSIDTS); got != "fresh" {
		t.Errorf("PSIDTS = %q, want fresh", got)
	}
}

func TestValidateCredential(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"complete", "__Secure-1PSID=g.a000" + strings.Repeat("x", 40) + "; __Secure-1PSIDTS=s", false},
		{"missing psid", "__Secure-1PSIDTS=sidts-only", true},
		{"not a cookie", "hello world", true},
		{"empty", "", true},
		{"short psid", "__Secure-1PSID=abc", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCredential(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateCredential(%q) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
		})
	}
}
