package gemini

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// rotateBody is the exact payload the accounts host expects.
//
// It looks like a placeholder and it is: Google's rotation endpoint takes no
// meaningful input and authenticates purely off the cookie jar, returning a
// fresh __Secure-1PSIDTS in Set-Cookie. Sending anything else — an empty body,
// a JSON object, the cookie value itself — is answered with a 400, so this
// literal is reproduced verbatim.
const rotateBody = `[000,"-0000000000000000000"]`

// Rotation outcomes worth naming, so the console can explain a failure instead
// of showing a status code.
var (
	// ErrRotateThrottled means the rotation endpoint refused because it was
	// called too often. It is not a problem with the account.
	ErrRotateThrottled = fmt.Errorf("rotation throttled")

	// ErrRotateNoCookie means the call succeeded but no __Secure-1PSIDTS came
	// back, which happens when the session is authenticated by other means.
	ErrRotateNoCookie = fmt.Errorf("rotation returned no __Secure-1PSIDTS")
)

// minRotateInterval guards the rotation endpoint.
//
// Google answers a rapid repeat with 429 and, more annoyingly, keeps throttling
// for a while afterwards. Ten minutes is the interval the reference
// implementation settled on, and it is comfortably inside the lifetime of the
// cookie being renewed.
const minRotateInterval = 10 * time.Minute

// RefreshCookie rotates __Secure-1PSIDTS and returns the new value.
//
// This is what keeps an account alive: __Secure-1PSID is long-lived, but
// __Secure-1PSIDTS expires in hours, and once it is gone every request is
// answered as if the account were signed out. Refreshing it is the difference
// between an account that works for a day and one that works indefinitely.
//
// The returned jar is the full updated jar — the caller should persist it, not
// just the PSIDTS value, because the response also reissues other cookies that
// the next request is expected to echo back.
func (c *Client) RefreshCookie(ctx context.Context, cred Credential) (string, *CookieJar, error) {
	jar := cred.Jar()
	if !jar.Has(CookiePSID) {
		return "", nil, fmt.Errorf("%w: __Secure-1PSID is missing", ErrInvalidCredential)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rotateURL(), strings.NewReader(rotateBody))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://accounts.google.com")
	req.Header.Set("Accept", "*/*")
	c.applyBrowserHeaders(req, cred)
	req.Header.Set("Cookie", jar.String())

	resp, err := c.httpClient(cred).Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused for the next rotation.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 401 here means the *primary* cookie is gone. The account is dead and
		// has to be re-added from a browser; no amount of retrying helps.
		return "", nil, fmt.Errorf("%w: rotation rejected the session", ErrInvalidCredential)
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", nil, ErrRotateThrottled
	case resp.StatusCode >= 400:
		return "", nil, fmt.Errorf("%w: rotation returned %d", ErrUpstreamUnavailable, resp.StatusCode)
	}

	updated := jar.Clone()
	updated.Override(CollectSetCookies(resp.Header.Values("Set-Cookie")))

	fresh := updated.Get(CookiePSIDTS)
	if fresh == "" || fresh == cred.PSIDTS {
		return "", updated, ErrRotateNoCookie
	}
	return fresh, updated, nil
}

// rotateURL resolves the rotation endpoint.
//
// The default is the accounts host rather than the app host, because the cookie
// being renewed is a Google *account* cookie and gemini.google.com does not
// reissue it. The setting exists for deployments that reach Google through a
// mirror, and for tests, which otherwise cannot exercise this path at all.
func (c *Client) rotateURL() string {
	if configured := strings.TrimSpace(c.settings().Upstream.RotateURL); configured != "" {
		return configured
	}
	return rotateCookiesURL
}

// RotateInterval returns how long to wait between rotations for this client.
func (c *Client) RotateInterval() time.Duration {
	minutes := c.settings().Refresh.IntervalMin
	if minutes <= 0 {
		return minRotateInterval
	}
	interval := time.Duration(minutes) * time.Minute
	if interval < time.Minute {
		// Anything faster is a self-inflicted 429.
		return time.Minute
	}
	return interval
}
