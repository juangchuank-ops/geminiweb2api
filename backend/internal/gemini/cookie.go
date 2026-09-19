package gemini

import (
	"sort"
	"strings"
)

// Cookie names that matter to this protocol. Everything else the browser sends
// along is carried through untouched, because Google's edge checks the whole
// jar, not just these two.
const (
	CookiePSID   = "__Secure-1PSID"
	CookiePSIDTS = "__Secure-1PSIDTS"
)

// CookieJar is an ordered cookie store.
//
// Order is preserved because the Cookie header is replayed verbatim: Google's
// consent layer is sensitive to a jar that looks hand-assembled, and the
// browser's own order is the one that is known to work.
type CookieJar struct {
	names  []string
	values map[string]string
}

// NewCookieJar returns an empty jar.
func NewCookieJar() *CookieJar {
	return &CookieJar{values: map[string]string{}}
}

// ParseCookies reads a `name=value; name=value` string, as copied out of a
// browser's Cookie header or a devtools "Copy as cURL".
//
// Parsing is forgiving: surrounding whitespace, a leading "Cookie:" label, and
// stray newlines from a multi-line paste are all accepted, because the failure
// mode of being strict here is an account that looks invalid for reasons the
// operator cannot see.
func ParseCookies(raw string) *CookieJar {
	jar := NewCookieJar()
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "Cookie:")
	raw = strings.TrimPrefix(raw, "cookie:")
	// A HAR or devtools paste often arrives with line breaks.
	raw = strings.NewReplacer("\r", ";", "\n", ";").Replace(raw)

	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, "=")
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+1:])
		if name == "" || value == "" {
			continue
		}
		jar.Set(name, value)
	}
	return jar
}

// Set stores a cookie, keeping first-seen order for names already present.
func (j *CookieJar) Set(name, value string) {
	if _, seen := j.values[name]; !seen {
		j.names = append(j.names, name)
	}
	j.values[name] = value
}

// Get reads a cookie value.
func (j *CookieJar) Get(name string) string {
	return j.values[name]
}

// Has reports whether a non-empty value is stored.
func (j *CookieJar) Has(name string) bool {
	return j.values[name] != ""
}

// Len returns the number of cookies held.
func (j *CookieJar) Len() int {
	return len(j.names)
}

// Clone returns an independent copy.
func (j *CookieJar) Clone() *CookieJar {
	out := NewCookieJar()
	for _, name := range j.names {
		out.Set(name, j.values[name])
	}
	return out
}

// String renders the Cookie header value.
func (j *CookieJar) String() string {
	parts := make([]string, 0, len(j.names))
	for _, name := range j.names {
		if value := j.values[name]; value != "" {
			parts = append(parts, name+"="+value)
		}
	}
	return strings.Join(parts, "; ")
}

// Names lists the stored cookie names in order. Used by the console to show
// which cookies an account actually carries without revealing values.
func (j *CookieJar) Names() []string {
	out := make([]string, len(j.names))
	copy(out, j.names)
	return out
}

// Merge copies every cookie from other that this jar does not already have.
//
// Existing values always win. That direction matters: a rotated
// __Secure-1PSIDTS arriving in a response must not be clobbered by the stale
// one the operator pasted in an hour ago, while cookies the operator supplied
// (SAPISID, SID) must never be replaced by whatever an unauthenticated
// preflight happens to hand back.
func (j *CookieJar) Merge(other *CookieJar) {
	if other == nil {
		return
	}
	for _, name := range other.names {
		if !j.Has(name) {
			j.Set(name, other.values[name])
		}
	}
}

// Override copies every cookie from other, replacing existing values.
//
// This is the opposite direction of Merge and is used for exactly one thing:
// applying a freshly rotated __Secure-1PSIDTS, where the new value must win.
func (j *CookieJar) Override(other *CookieJar) {
	if other == nil {
		return
	}
	for _, name := range other.names {
		if value := other.values[name]; value != "" {
			j.Set(name, value)
		}
	}
}

// ParseSetCookie reads a single Set-Cookie response header.
//
// Only the name and value are kept. Attributes (Path, Domain, Expires, HttpOnly,
// SameSite) describe where a *browser* may store the cookie; this gateway
// replays cookies explicitly on every request, so carrying them would only add
// fields that can go stale.
func ParseSetCookie(header string) (name, value string, ok bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", "", false
	}
	// Everything up to the first ';' is the name=value pair.
	if idx := strings.Index(header, ";"); idx >= 0 {
		header = header[:idx]
	}
	idx := strings.Index(header, "=")
	if idx <= 0 {
		return "", "", false
	}
	name = strings.TrimSpace(header[:idx])
	value = strings.TrimSpace(header[idx+1:])
	if name == "" || value == "" {
		return "", "", false
	}
	// A deletion is expressed as an empty value or an already-expired cookie;
	// both arrive here as an empty value and must not erase a live cookie.
	if value == `""` {
		return "", "", false
	}
	return name, value, true
}

// CollectSetCookies folds a response's Set-Cookie headers into a jar.
func CollectSetCookies(headerValues []string) *CookieJar {
	jar := NewCookieJar()
	for _, raw := range headerValues {
		// A single header may carry several cookies joined by commas when the
		// sender did not split them; the Expires attribute also contains a
		// comma, so the split has to be on ", " followed by a cookie name.
		for _, part := range splitSetCookieHeader(raw) {
			if name, value, ok := ParseSetCookie(part); ok {
				jar.Set(name, value)
			}
		}
	}
	return jar
}

// splitSetCookieHeader splits a comma-joined Set-Cookie header.
//
// A naive strings.Split on "," breaks on `Expires=Wed, 21 Oct 2026 ...`, which
// is present on every Google cookie. The discriminator is that a cookie starts
// with `token=` where token has no space in it, so a comma only starts a new
// cookie when the next segment looks like `name=`.
func splitSetCookieHeader(raw string) []string {
	var out []string
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] != ',' {
			continue
		}
		rest := raw[i+1:]
		trimmed := strings.TrimLeft(rest, " ")
		eq := strings.Index(trimmed, "=")
		if eq <= 0 {
			continue
		}
		name := trimmed[:eq]
		// A cookie name never contains a space, a semicolon or a comma; an
		// Expires date segment (" 21 Oct 2026 00:00:00 GMT") always does.
		if strings.ContainsAny(name, " \t;,") {
			continue
		}
		out = append(out, raw[start:i])
		start = i + 1
	}
	out = append(out, raw[start:])
	return out
}

// SortedNames is a stable ordering helper for the console.
func SortedNames(jar *CookieJar) []string {
	names := jar.Names()
	sort.Strings(names)
	return names
}
