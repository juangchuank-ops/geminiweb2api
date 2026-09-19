package gemini

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ProbeResult summarises one connectivity check.
type ProbeResult struct {
	// OK means the account produced a usable session.
	OK bool
	// Authenticated distinguishes a signed-in account from a guest. A guest
	// session still answers requests, so "reachable" alone does not mean the
	// cookie is doing anything.
	Authenticated bool
	// LatencyMs is the round trip of the app shell fetch.
	LatencyMs int64
	// BuildLabel is the build the host is currently serving, which is what a
	// stale `bl` setting has to be compared against.
	BuildLabel string
	// Language is the account's UI language as reported by the shell.
	Language string
	// Note is a human-readable summary for the console.
	Note string
}

// Probe checks that an account can reach Gemini and is still signed in.
//
// It always fetches a fresh shell rather than reading the session cache. A probe
// that answers from cache would report a dead cookie as healthy for as long as
// the cache lives, which is precisely the window in which an operator is
// looking at the console trying to work out what broke.
func (c *Client) Probe(ctx context.Context, cred Credential) (*ProbeResult, error) {
	started := time.Now()
	session, jar, err := c.discover(ctx, cred)
	latency := time.Since(started).Milliseconds()

	if err != nil {
		return &ProbeResult{LatencyMs: latency, Note: err.Error()}, err
	}

	result := &ProbeResult{
		OK:            true,
		Authenticated: session.Authenticated(),
		LatencyMs:     latency,
		BuildLabel:    session.BuildLabel,
		Language:      session.Language,
	}

	switch {
	case session.Authenticated():
		result.Note = "会话正常"
	case session.Usable():
		// The shell loaded but issued no session id: this is what an expired
		// cookie looks like from the outside, and it is worth saying so
		// explicitly because every request will still succeed — as a guest,
		// with the guest's much smaller quota.
		result.Note = "已连通，但未识别到登录会话（Cookie 可能已过期）"
	default:
		result.Note = "已连通，但页面未返回会话参数"
	}

	// Persist the rotated jar so the caller can store any refreshed PSIDTS.
	if jar != nil {
		c.sessions.put(cred.CacheKey(), &sessionEntry{session: session, jar: jar})
	}
	return result, nil
}

// DescribeCredential splits a pasted cookie string into the parts the console
// shows and stores.
//
// Only the two __Secure cookies are extracted by name; the full string is kept
// as-is by the caller because Google's edge inspects the whole jar. This exists
// so an operator can see at a glance whether the paste actually contained a
// session, rather than finding out one request later.
func DescribeCredential(raw string) (psid, psidts string, cookieCount int, ok bool) {
	jar := ParseCookies(raw)
	psid = jar.Get(CookiePSID)
	psidts = jar.Get(CookiePSIDTS)
	cookieCount = jar.Len()
	ok = psid != ""
	return psid, psidts, cookieCount, ok
}

// ValidateCredential reports why a pasted cookie cannot be used, or nil when it
// looks usable.
//
// The check is deliberately shallow — it can only tell whether the string is
// well-formed, never whether the session is live, since only Google can answer
// that. It exists to catch the two mistakes that are actually common: pasting
// only __Secure-1PSIDTS (which is not a session by itself) and pasting
// something that is not a cookie at all.
func ValidateCredential(raw string) error {
	if raw == "" {
		return fmt.Errorf("Cookie 不能为空")
	}
	psid, _, count, ok := DescribeCredential(raw)
	if !ok {
		if count == 0 {
			return fmt.Errorf("没有解析出任何 Cookie，请粘贴浏览器里的完整 Cookie 串")
		}
		return fmt.Errorf("缺少 %s，请确认复制的是 gemini.google.com 下的完整 Cookie", CookiePSID)
	}
	if len(psid) < 20 {
		return fmt.Errorf("%s 长度异常（%d 字符），看起来不是完整值", CookiePSID, len(psid))
	}
	return nil
}

// AccountLabel derives a stable, human-recognisable name for an account from
// its credential.
//
// Google does not put the address anywhere the cookie exposes, so the PSID is
// the only stable handle available. A short prefix of it is enough to tell two
// accounts apart in a list without printing a value that is itself a
// credential.
func AccountLabel(cred Credential) string {
	psid := cred.PSID
	if psid == "" {
		psid = cred.Jar().Get(CookiePSID)
	}
	if psid == "" {
		return ""
	}
	// The value is `g.a000<random>`; the `g.a000` prefix is identical across
	// every account, so keeping it would spend the whole label on a constant.
	trimmed := psid
	if strings.HasPrefix(trimmed, "g.a") && len(trimmed) > 6 {
		trimmed = trimmed[6:]
	}
	if len(trimmed) > 10 {
		return "acct-" + trimmed[:10]
	}
	return "acct-" + trimmed
}
