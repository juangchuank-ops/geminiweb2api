package gemini

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"geminiweb2api/internal/config"
)

// defaultUserAgent is the browser string sent when the operator has not set
// one. It matches a current stable Chrome on Windows, because the app shell
// serves a different (and occasionally broken) document to unknown clients.
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Credential is everything needed to act as one account.
//
// There is no fingerprint here, unlike some sibling projects: Gemini's web
// protocol signs nothing, so a cookie is replayable from anywhere. What it does
// require is the *whole* jar — Google's edge rejects a request that carries
// only the two __Secure cookies on some paths — which is why Cookie is kept as
// the operator pasted it and parsed on demand.
type Credential struct {
	// Cookie is the raw cookie string. It is the primary field; the split-out
	// values below are conveniences for display and rotation.
	Cookie string
	// PSID is __Secure-1PSID, the account identity.
	PSID string
	// PSIDTS is __Secure-1PSIDTS, which rotates and is what the refresher
	// rewrites.
	PSIDTS string
	// AuthUser selects which Google account to act as when several are signed
	// in: 0 is the default, N means /u/N.
	AuthUser int
	// Model optionally pins the account to a model, overriding the request.
	Model string
	// BaseURL optionally overrides the host for this account.
	BaseURL string
}

// Jar parses the credential into a cookie jar.
//
// Values stored separately win over the raw string when both are present: the
// refresher writes PSIDTS back after a rotation, and the raw string it was
// parsed from is by then stale.
func (c Credential) Jar() *CookieJar {
	jar := ParseCookies(c.Cookie)
	if c.PSID != "" {
		jar.Set(CookiePSID, c.PSID)
	}
	if c.PSIDTS != "" {
		jar.Set(CookiePSIDTS, c.PSIDTS)
	}
	return jar
}

// CacheKey identifies the session cache slot for this credential.
func (c Credential) CacheKey() string {
	if c.PSID != "" {
		return c.PSID
	}
	// Without a PSID the jar is not a session, but a stable key is still
	// needed so repeated failures do not each cost a page fetch.
	return c.Cookie
}

// Label is a short human-readable identifier for logs.
func (c Credential) Label() string {
	if c.PSID != "" {
		if len(c.PSID) > 14 {
			return c.PSID[:14] + "…"
		}
		return c.PSID
	}
	if len(c.Cookie) > 14 {
		return c.Cookie[:14] + "…"
	}
	return c.Cookie
}

// UploadedImage is a remote image forwarded to the model.
//
// Gemini's own upload channel (content-push) needs a push id and a separate
// tenant header, and the file reference it returns is only valid for the
// account that uploaded it. Rather than half-implement that, images are passed
// by URL: the model fetches them itself, which is also what the web UI does
// when you paste a link.
type UploadedImage struct {
	URL  string
	Name string
}

// Options describes one upstream call.
type Options struct {
	Credential Credential
	Text       string
	// Model is the model the caller asked for, resolved against the catalogue.
	// An unknown name falls back to the default rather than failing, because
	// every client hardcodes some model name and refusing would break all of
	// them at once.
	Model string
	// ThinkMode overrides the model's own reasoning depth (slot 17). Nil keeps
	// the model's default, which is the right answer for every caller that has
	// no opinion — the catalogue already encodes which modes think.
	ThinkMode *int
	// Images are forwarded by URL.
	Images []UploadedImage
	// Metadata continues an existing conversation. Nil starts a new one.
	Metadata []any
	// Timeout bounds the whole call.
	Timeout time.Duration
	// IdleTimeout bounds the gap between chunks. A stream that stalls without
	// closing is the common failure mode, and a total timeout alone lets it
	// hold a connection until the client gives up.
	IdleTimeout time.Duration
	// Callbacks are invoked as content arrives. They run on the calling
	// goroutine and must not block.
	OnDelta    func(string)
	OnThinking func(string)
	OnProgress func(string)
}

// Result is the aggregated outcome of one call.
type Result struct {
	Text     string
	Thinking string
	Media    []MediaRef
	// CID and RID identify the conversation, for callers that continue it.
	CID string
	RID string
	// Metadata is the conversation block to replay on the next turn.
	Metadata []any
	// Model is the catalogue id that actually ran, which may differ from the
	// requested one.
	Model string
	// StopReason is "stop" for a completed turn and "length" otherwise.
	StopReason string
}

// Client talks to the Gemini web app.
type Client struct {
	http     *http.Client
	settings func() config.Settings
	sessions *sessionCache

	mu         sync.Mutex
	transports map[string]*http.Transport
}

// New builds a client. settingsFn is re-read on every request so runtime
// setting changes take effect without a restart.
func New(settingsFn func() config.Settings) *Client {
	return &Client{
		http:       &http.Client{},
		settings:   settingsFn,
		sessions:   newSessionCache(20 * time.Minute),
		transports: map[string]*http.Transport{},
	}
}

// baseURL resolves the host for a credential.
func (c *Client) baseURL(cred Credential) string {
	if cred.BaseURL != "" {
		return cred.BaseURL
	}
	if settings := c.settings(); settings.Upstream.BaseURL != "" {
		return settings.Upstream.BaseURL
	}
	return "https://gemini.google.com"
}

// httpClient returns a client whose transport honours the configured proxy.
//
// Transports are cached per proxy string so connection pools are reused;
// building one per request would open a fresh TCP (and TLS) connection every
// time and, against Google, would look like exactly the traffic pattern the
// rate limiter is looking for.
func (c *Client) httpClient(cred Credential) *http.Client {
	proxy := strings.TrimSpace(c.settings().Upstream.Proxy)

	c.mu.Lock()
	transport, ok := c.transports[proxy]
	if !ok {
		transport = &http.Transport{
			MaxIdleConns:        128,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		}
		if proxy != "" {
			if parsed, err := url.Parse(proxy); err == nil {
				transport.Proxy = func(req *http.Request) (*url.URL, error) {
					// Loopback never goes through a proxy: the proxy is there
					// to reach the internet, and 127.0.0.1 is by definition not
					// on it. Sending it anyway earns an empty-bodied 502 that
					// reads exactly like an upstream outage.
					if isLoopbackHost(req.URL.Hostname()) {
						return nil, nil
					}
					return parsed, nil
				}
			}
		}
		c.transports[proxy] = transport
	}
	c.mu.Unlock()

	client := *c.http
	client.Transport = transport
	return &client
}

// isLoopbackHost reports whether host names the local machine.
func isLoopbackHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// applyBrowserHeaders sets the headers the app shell expects.
func (c *Client) applyBrowserHeaders(req *http.Request, cred Credential) {
	settings := c.settings()
	userAgent := strings.TrimSpace(settings.Upstream.UserAgent)
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if cred.AuthUser > 0 {
		req.Header.Set("X-Goog-AuthUser", fmt.Sprint(cred.AuthUser))
	}
}

// Completion runs one generation and returns the assembled answer.
func (c *Client) Completion(ctx context.Context, opts Options) (*Result, error) {
	settings := c.settings()

	spec, _ := ResolveModel(opts.Model)
	if opts.Credential.Model != "" {
		if pinned, ok := LookupModel(opts.Credential.Model); ok {
			spec = pinned
		}
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = time.Duration(settings.Upstream.RequestTimeoutSec) * time.Second
	}
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	idleTimeout := opts.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = time.Duration(settings.Upstream.StreamIdleTimeoutSec) * time.Second
	}

	// A stale build label is answered with 405 rather than a message, so the
	// cached shell is dropped and the request retried exactly once. Retrying
	// more than once would mean the shell is not the problem.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		result, err := c.attempt(ctx, cancel, opts, spec, idleTimeout)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !isStaleBuildLabel(err) {
			break
		}
		c.sessions.invalidate(opts.Credential.CacheKey())
	}
	return nil, lastErr
}

// staleBuildLabelError marks the one failure that a session refresh can fix.
type staleBuildLabelError struct{ status int }

func (e *staleBuildLabelError) Error() string {
	return fmt.Sprintf("upstream rejected the build label (HTTP %d)", e.status)
}

func isStaleBuildLabel(err error) bool {
	var target *staleBuildLabelError
	return asError(err, &target)
}

// asError is a tiny errors.As wrapper kept local so this file does not import
// errors just for one call.
func asError[T error](err error, target *T) bool {
	for err != nil {
		if typed, ok := err.(T); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// attempt performs a single upstream request.
func (c *Client) attempt(ctx context.Context, cancel context.CancelFunc, opts Options, spec ModelSpec, idleTimeout time.Duration) (*Result, error) {
	session, jar, err := c.session(ctx, opts.Credential)
	if err != nil {
		return nil, err
	}

	language := session.Language
	if language == "" {
		language = "en"
	}

	think := spec.Think
	if opts.ThinkMode != nil {
		think = *opts.ThinkMode
	}
	inner := buildInner(opts.Text, language, spec, think, opts.Metadata, imageRefs(opts.Images))
	body, err := outerPayload(inner, session.AccessToken)
	if err != nil {
		return nil, err
	}

	requestID := int(time.Now().UnixNano()/1e6) % 1000000
	query := url.Values{}
	if session.BuildLabel != "" {
		query.Set("bl", session.BuildLabel)
	}
	query.Set("hl", language)
	query.Set("_reqid", fmt.Sprint(requestID))
	query.Set("rt", "c")
	if session.SessionID != "" {
		query.Set("f.sid", session.SessionID)
	}

	endpoint := strings.TrimRight(c.baseURL(opts.Credential), "/") + generatePath
	if opts.Credential.AuthUser > 0 {
		endpoint = strings.TrimRight(c.baseURL(opts.Credential), "/") +
			"/u/" + fmt.Sprint(opts.Credential.AuthUser) + generatePath
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"?"+query.Encode(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("Origin", "https://gemini.google.com")
	req.Header.Set("Referer", "https://gemini.google.com/")
	req.Header.Set("X-Same-Domain", "1")
	c.applyBrowserHeaders(req, opts.Credential)
	req.Header.Set("Cookie", jar.String())

	requestUUID := newRequestUUID()
	// The header is only sent when the catalogue knows this mode's hex id. A
	// guessed id is answered with BardErrorInfo 1052, which reads like a broken
	// account rather than a broken model, so the headerless path is preferred
	// over a guess — upstream routes by the mode number alone.
	if header, ok := modelHeader(spec, requestUUID); ok {
		req.Header.Set(modelHeaderKey, header)
	}
	req.Header.Set("x-goog-ext-73010989-jspb", "[0]")
	req.Header.Set("x-goog-ext-73010990-jspb", "[0,0,0]")
	req.Header.Set("x-goog-ext-525005358-jspb", `["`+requestUUID+`",1]`)

	resp, err := c.httpClient(opts.Credential).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == 400:
		// 405 is the documented "your bl is stale" answer. A 400 with no body
		// is usually the same thing after a build rollover.
		return nil, &staleBuildLabelError{status: resp.StatusCode}
	case resp.StatusCode != http.StatusOK:
		// Drain a little of the body so the connection can be reused, and to
		// give the classifier something to look at.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if kind := classifyStatus(resp.StatusCode); kind != ErrUpstreamUnavailable {
			return nil, &UpstreamError{Kind: kind, Msg: fmt.Sprintf("upstream returned %d", resp.StatusCode)}
		}
		return nil, fmt.Errorf("%w: upstream returned %d: %s", ErrUpstreamUnavailable, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	return c.consume(ctx, cancel, resp.Body, opts, spec, idleTimeout)
}

// consume reads the length-prefixed stream and assembles the answer.
func (c *Client) consume(ctx context.Context, cancel context.CancelFunc, body io.Reader, opts Options, spec ModelSpec, idleTimeout time.Duration) (*Result, error) {
	parser := NewFrameParser()

	result := &Result{Model: spec.ID, StopReason: "length"}
	var lastText, lastThoughts string
	mediaSeen := map[string]bool{}
	completed := false

	// The idle watchdog cancels the request context when the stream stalls.
	// Without it a half-open connection holds a pool slot until the total
	// timeout, which for a long answer is minutes of doing nothing.
	var watchdog *time.Timer
	if idleTimeout > 0 && cancel != nil {
		watchdog = time.AfterFunc(idleTimeout, cancel)
		defer watchdog.Stop()
	}

	reader := bufio.NewReaderSize(body, 64*1024)
	buffer := make([]byte, 32*1024)

	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if watchdog != nil {
				watchdog.Reset(idleTimeout)
			}
			chunk := string(buffer[:n])

			if code, ok := detectErrorCode(chunk); ok {
				return nil, &UpstreamError{
					Code: code,
					Kind: classifyCode(code),
					Msg:  fmt.Sprintf("upstream rejected the request (BardErrorInfo %d)", code),
				}
			}

			for _, frame := range parser.Feed(chunk) {
				if code, ok := detectErrorCode(frame.Payload); ok {
					return nil, &UpstreamError{
						Code: code,
						Kind: classifyCode(code),
						Msg:  fmt.Sprintf("upstream rejected the request (BardErrorInfo %d)", code),
					}
				}

				turn, ok := parseTurn(frame)
				if !ok {
					continue
				}
				if turn.CID != "" {
					result.CID = turn.CID
				}
				if turn.RID != "" {
					result.RID = turn.RID
				}
				if turn.Metadata != nil {
					result.Metadata = turn.Metadata
				}
				for _, item := range turn.Media {
					if mediaSeen[item.URL] {
						continue
					}
					mediaSeen[item.URL] = true
					result.Media = append(result.Media, item)
				}
				if turn.Completed {
					completed = true
				}

				if delta := diffCumulative(turn.Text, &lastText); delta != "" {
					if opts.OnDelta != nil {
						opts.OnDelta(delta)
					}
				}
				if delta := diffCumulative(turn.Thoughts, &lastThoughts); delta != "" {
					if opts.OnThinking != nil {
						opts.OnThinking(delta)
					}
				}
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			// A cancelled context is the watchdog firing or the client going
			// away; report it as a timeout rather than as a transport fault.
			if ctx.Err() != nil {
				break
			}
			return nil, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, readErr)
		}
	}

	for _, frame := range parser.Flush() {
		if turn, ok := parseTurn(frame); ok {
			if turn.CID != "" {
				result.CID = turn.CID
			}
			if turn.RID != "" {
				result.RID = turn.RID
			}
			if turn.Metadata != nil {
				result.Metadata = turn.Metadata
			}
			if turn.Completed {
				completed = true
			}
			if delta := diffCumulative(turn.Text, &lastText); delta != "" {
				if opts.OnDelta != nil {
					opts.OnDelta(delta)
				}
			}
			if delta := diffCumulative(turn.Thoughts, &lastThoughts); delta != "" {
				if opts.OnThinking != nil {
					opts.OnThinking(delta)
				}
			}
		}
	}

	result.Text = cleanArtifacts(lastText)
	result.Thinking = lastThoughts
	if completed {
		result.StopReason = "stop"
	}

	if result.Text == "" && result.Thinking == "" && len(result.Media) == 0 {
		return nil, fmt.Errorf("%w: upstream returned an empty answer", ErrUpstreamUnavailable)
	}
	return result, nil
}

// diffCumulative turns a cumulative field into the increment since the last
// frame, updating last in place.
//
// Every frame repeats the whole answer so far. Appending it verbatim would
// multiply the reply by the number of frames, and simply taking the last one
// would break streaming — so the difference is what gets emitted.
//
// The prefix test is not just an optimisation: the model sometimes rewrites an
// earlier part of the answer (a code fence closing late, a markdown escape
// settling), and when that happens the safe move is to treat the whole thing as
// new rather than to emit a corrupted slice.
func diffCumulative(current string, last *string) string {
	if current == "" || current == *last {
		return ""
	}
	if strings.HasPrefix(current, *last) {
		delta := current[len(*last):]
		*last = current
		return delta
	}
	*last = current
	return ""
}

// imageRefs renders forwarded images into the attachment slot.
//
// The shape is the one the web UI uses for a link the user pasted: a triple of
// nulls with the URL in the third position.
func imageRefs(images []UploadedImage) []any {
	if len(images) == 0 {
		return nil
	}
	refs := make([]any, 0, len(images))
	for _, image := range images {
		if strings.TrimSpace(image.URL) == "" {
			continue
		}
		refs = append(refs, []any{nil, nil, image.URL})
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

// detectErrorCode looks for a BardErrorInfo marker in a raw chunk.
//
// It is scanned for as text rather than read from the parsed frame because the
// marker appears inside frames that otherwise parse as ordinary results, and
// because it can straddle a chunk boundary in the raw stream.
func detectErrorCode(chunk string) (int, bool) {
	const marker = "BardErrorInfo"
	idx := strings.Index(chunk, marker)
	if idx < 0 {
		return 0, false
	}
	rest := chunk[idx+len(marker):]
	// The marker appears in three shapes and all three have to be read:
	//   `BardErrorInfo [1037]`      inside a serialised array
	//   `"BardErrorInfo":[1060]`    as an object key
	//   `["BardErrorInfo",[1037]]`  as an array pair in the decoded payload
	// Quotes, whitespace, a colon or the separating comma can therefore sit
	// between the name and the bracket, so they are skipped rather than treated
	// as a malformed marker. The comma is only skipped *after* the marker has
	// been found, and a bracketed number still has to follow, so this cannot
	// latch onto unrelated text.
	rest = strings.TrimLeft(rest, " \t\r\n\":,")
	if !strings.HasPrefix(rest, "[") {
		return 0, false
	}
	rest = rest[1:]
	end := strings.IndexAny(rest, ",]")
	if end < 0 {
		return 0, false
	}
	digits := strings.TrimSpace(rest[:end])
	if digits == "" {
		return 0, false
	}
	code := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
		code = code*10 + int(r-'0')
	}
	if code == 0 {
		return 0, false
	}
	return code, true
}
