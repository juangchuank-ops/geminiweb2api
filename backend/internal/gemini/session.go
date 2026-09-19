package gemini

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session carries the per-account values scraped from the app shell.
//
// None of them are credentials; they are the anti-CSRF token and build
// identifiers the web client itself reads out of the page on every load. They
// are cached because fetching them costs a full page download and they change
// on the order of hours, not requests.
type Session struct {
	// AccessToken is the `SNlM0e` value, posted as the `at` form field. Guest
	// sessions have none, and requests without it are accepted but anonymous.
	AccessToken string
	// BuildLabel is the `cfb2h` value, sent as the `bl` query parameter. A
	// stale label is answered with HTTP 405, which is why it is refreshed
	// rather than pinned.
	BuildLabel string
	// SessionID is the `FdrFJe` value, sent as `f.sid`.
	SessionID string
	// Language is the account's UI language, e.g. "en".
	Language string
	// PushID is the `qKIAYe` value used by the upload endpoint.
	PushID string
	// FetchedAt is when these values were read.
	FetchedAt time.Time
}

// Usable reports whether the shell carried enough to make a request.
func (s *Session) Usable() bool {
	return s != nil && (s.AccessToken != "" || s.BuildLabel != "")
}

// Authenticated reports whether the shell looked like a signed-in account.
//
// A guest page still yields an access token, so the token alone cannot tell the
// two apart. The session id is only issued to a real session.
func (s *Session) Authenticated() bool {
	return s != nil && s.AccessToken != "" && s.SessionID != ""
}

var (
	reAccessToken = regexp.MustCompile(`"SNlM0e":\s*"(.*?)"`)
	reBuildLabel  = regexp.MustCompile(`"cfb2h":\s*"(.*?)"`)
	reSessionID   = regexp.MustCompile(`"FdrFJe":\s*"(.*?)"`)
	reLanguage    = regexp.MustCompile(`"TuX5cc":\s*"(.*?)"`)
	rePushID      = regexp.MustCompile(`"qKIAYe":\s*"(.*?)"`)
)

// parseSession pulls the shell values out of the page HTML.
func parseSession(html string) *Session {
	session := &Session{
		AccessToken: firstGroup(reAccessToken, html),
		BuildLabel:  firstGroup(reBuildLabel, html),
		SessionID:   firstGroup(reSessionID, html),
		Language:    firstGroup(reLanguage, html),
		PushID:      firstGroup(rePushID, html),
		FetchedAt:   time.Now(),
	}
	return session
}

func firstGroup(re *regexp.Regexp, s string) string {
	match := re.FindStringSubmatch(s)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

// sessionCache keeps one discovered session per credential.
//
// The key is the __Secure-1PSID, because that is the part of a Google session
// that identifies the account: __Secure-1PSIDTS rotates underneath it and every
// other cookie in the jar is incidental.
type sessionCache struct {
	mu      sync.Mutex
	entries map[string]*sessionEntry
	ttl     time.Duration
}

type sessionEntry struct {
	session *Session
	jar     *CookieJar
}

func newSessionCache(ttl time.Duration) *sessionCache {
	if ttl <= 0 {
		ttl = 20 * time.Minute
	}
	return &sessionCache{entries: map[string]*sessionEntry{}, ttl: ttl}
}

func (c *sessionCache) get(key string) (*sessionEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(entry.session.FetchedAt) > c.ttl {
		delete(c.entries, key)
		return nil, false
	}
	return entry, true
}

func (c *sessionCache) put(key string, entry *sessionEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Bound the map: one entry per live account is the expected size, and a
	// pathological stream of distinct credentials should not grow it forever.
	if len(c.entries) > 512 {
		for k, v := range c.entries {
			if time.Since(v.session.FetchedAt) > c.ttl {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = entry
}

func (c *sessionCache) invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// appPath renders the app shell URL for an account index.
//
// A Google login can hold several accounts, and the one to act as is selected
// by a path segment: /u/1/app is the second account in the chooser. Omitting it
// acts as the default, which is why an account added with the wrong index
// appears to work but answers as somebody else.
func appPath(authUser int) string {
	if authUser <= 0 {
		return "/app"
	}
	return "/u/" + strconv.Itoa(authUser) + "/app"
}

// discover fetches the app shell and reads the session values out of it.
//
// The returned jar is the one to keep: the response may have rotated
// __Secure-1PSIDTS, and discarding that would mean rotating again on the next
// call, which is what earns a 429 from the rotation endpoint.
func (c *Client) discover(ctx context.Context, cred Credential) (*Session, *CookieJar, error) {
	jar := cred.Jar()
	if !jar.Has(CookiePSID) {
		return nil, nil, fmt.Errorf("%w: __Secure-1PSID is missing", ErrInvalidCredential)
	}

	endpoint := strings.TrimRight(c.baseURL(cred), "/") + appPath(cred.AuthUser)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	c.applyBrowserHeaders(req, cred)
	req.Header.Set("Cookie", jar.String())

	resp, err := c.httpClient(cred).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	// The app shell is a large document; cap the read so a misrouted host
	// cannot make the gateway buffer something enormous.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}

	updated := jar.Clone()
	updated.Override(CollectSetCookies(resp.Header.Values("Set-Cookie")))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, updated, fmt.Errorf("%w: app shell returned %d", ErrInvalidCredential, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, updated, ErrRateLimited
	case resp.StatusCode >= 500:
		return nil, updated, fmt.Errorf("app shell returned %d", resp.StatusCode)
	}

	// A redirect to a *different host* means the consent flow, which carries no
	// session values — following it only makes the failure slower. The
	// comparison is against the host that was actually asked, not against
	// gemini.google.com, so that a deployment pointing BaseURL at a mirror (or
	// at a test server) is not mistaken for a stale cookie.
	if final := resp.Request.URL.Host; final != "" && final != req.URL.Host {
		return nil, updated, fmt.Errorf("%w: redirected to %s", ErrInvalidCredential, final)
	}

	session := parseSession(string(body))
	if !session.Usable() {
		return nil, updated, fmt.Errorf("%w: app shell carried no session values", ErrInvalidCredential)
	}
	if session.Language == "" {
		session.Language = "en"
	}

	return session, updated, nil
}

// session returns a cached session for the credential, discovering one when the
// cache is cold or has aged out.
func (c *Client) session(ctx context.Context, cred Credential) (*Session, *CookieJar, error) {
	key := cred.CacheKey()

	if entry, ok := c.sessions.get(key); ok {
		return entry.session, entry.jar, nil
	}

	session, jar, err := c.discover(ctx, cred)
	if err != nil {
		return nil, jar, err
	}
	c.sessions.put(key, &sessionEntry{session: session, jar: jar})
	return session, jar, nil
}
