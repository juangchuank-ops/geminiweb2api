package store

import (
	"strings"
	"time"

	"geminiweb2api/internal/config"
	"geminiweb2api/internal/gemini"
)

// Account status values.
const (
	StatusActive   = "active"
	StatusCooldown = "cooldown"
	StatusDisabled = "disabled"
	StatusInvalid  = "invalid"
)

// Account kinds.
//
// A guest account carries no cookie at all. Gemini serves anonymous sessions
// with a small quota, and they are genuinely useful — they need no login and
// can absorb overflow — but they are a different thing from a signed-in
// account, so the distinction is recorded rather than inferred from an empty
// cookie.
const (
	KindCookie = "cookie"
	KindGuest  = "guest"
)

// Quota describes the last probe of an account.
type Quota struct {
	SyncedAt time.Time `json:"syncedAt"`
	// Available means the app shell loaded.
	Available bool `json:"available"`
	// Authenticated means a real session came back. A guest session is
	// available but not authenticated, and the difference is what tells an
	// operator that their cookie has expired while requests keep succeeding.
	Authenticated bool   `json:"authenticated"`
	LatencyMs     int64  `json:"latencyMs"`
	Note          string `json:"note"`
}

// Cookie refresh outcome values stored on an account.
const (
	RefreshOK        = "ok"
	RefreshFailed    = "failed"
	RefreshThrottled = "throttled" // the endpoint refused; the account is fine
	RefreshSkipped   = "skipped"   // guest account, disabled, or no cookie
	// RefreshInvalid means the rotation endpoint rejected the session. The
	// primary cookie is gone and the account has to be re-added from a browser.
	RefreshInvalid = "invalid"
)

// Account is one pooled identity.
//
// The cookie is the whole credential. Unlike the sibling 2api projects there is
// no token to decode, no fingerprint to replay and no signature to compute:
// Gemini's web protocol authenticates with the cookie jar alone. What the
// account record carries beyond it is bookkeeping the console needs to make
// sense of a pool — which is exactly why the raw value is never serialised
// (see AccountView).
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Cookie is the complete cookie string as pasted from a browser. It is
	// kept whole rather than reassembled from parts because Google's edge
	// inspects the full jar on some paths, and a reconstructed one is a
	// different jar.
	Cookie string `json:"cookie"`
	// PSID is __Secure-1PSID, the long-lived account identity. It is stored
	// separately because it is what the console shows, what the session cache
	// is keyed on, and the one value whose absence means "not an account".
	PSID string `json:"psid"`
	// PSIDTS is __Secure-1PSIDTS, which expires in hours and is rewritten by
	// the refresher. It is stored separately so a rotation updates one field
	// instead of rewriting the operator's paste.
	PSIDTS string `json:"psidts"`
	// Identifier is a human-recognisable label. Google does not expose the
	// account address through the cookie, so this is derived from the PSID
	// unless the operator supplies something better.
	Identifier string `json:"identifier"`
	// AuthUser selects which signed-in Google account to act as: 0 is the
	// default, N means /u/N. Getting it wrong does not error — it quietly
	// answers as a different account.
	AuthUser int `json:"authUser"`
	// Model optionally pins the account to a catalogue entry, overriding the
	// model the client asked for. Useful when an account's tier cannot serve
	// the default.
	Model string `json:"model"`
	// BaseURL optionally overrides the host for this account.
	BaseURL       string    `json:"baseURL"`
	Group         string    `json:"group"`
	Remark        string    `json:"remark"`
	Enabled       bool      `json:"enabled"`
	Priority      int       `json:"priority"`
	MaxConcurrent int       `json:"maxConcurrent"`
	Status        string    `json:"status"`
	CooldownUntil time.Time `json:"cooldownUntil"`
	FailCount     int       `json:"failCount"`
	SuccessCount  int       `json:"successCount"`
	LastUsedAt    time.Time `json:"lastUsedAt"`
	LastError     string    `json:"lastError"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	Quota         *Quota    `json:"quota,omitempty"`

	// RefreshAt is the last rotation attempt, successful or not. It doubles as
	// the scheduler's "already rotated recently" marker, which is why it is
	// written even on failure: an endpoint outage must not turn into a
	// rotation storm on every tick.
	RefreshAt     time.Time `json:"refreshAt"`
	RefreshStatus string    `json:"refreshStatus"`
	RefreshError  string    `json:"refreshError"`
	// RefreshFailures counts consecutive rotations rejected as unauthenticated.
	// It is deliberately not incremented by throttling or network errors — a
	// bad minute must not retire a healthy account.
	RefreshFailures int `json:"refreshFailures"`

	// BuildLabel and Language are the last values read from the app shell.
	// They are kept for the console's benefit; the client caches its own copy
	// for requests.
	BuildLabel string `json:"buildLabel"`
	Language   string `json:"language"`
}

// AccountView is the API representation. It is an explicit projection rather
// than an embedded struct so the raw cookie can never be serialised by accident.
type AccountView struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	Identifier    string    `json:"identifier"`
	AuthUser      int       `json:"authUser"`
	Model         string    `json:"model"`
	BaseURL       string    `json:"baseURL"`
	Group         string    `json:"group"`
	Remark        string    `json:"remark"`
	Enabled       bool      `json:"enabled"`
	Priority      int       `json:"priority"`
	MaxConcurrent int       `json:"maxConcurrent"`
	Status        string    `json:"status"`
	CooldownUntil time.Time `json:"cooldownUntil"`
	FailCount     int       `json:"failCount"`
	SuccessCount  int       `json:"successCount"`
	LastUsedAt    time.Time `json:"lastUsedAt"`
	LastError     string    `json:"lastError"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	// No omitempty: the console declares quota as a required field that may be
	// null, so the key must always be present. Omitting it would make the
	// property undefined instead of null and silently break strict checks.
	Quota           *Quota    `json:"quota"`
	RefreshAt       time.Time `json:"refreshAt"`
	RefreshStatus   string    `json:"refreshStatus"`
	RefreshError    string    `json:"refreshError"`
	RefreshFailures int       `json:"refreshFailures"`
	BuildLabel      string    `json:"buildLabel"`
	Language        string    `json:"language"`
	// HasPSIDTS tells the console whether a rotation has ever succeeded, which
	// is the difference between "cookie pasted" and "cookie alive".
	HasPSIDTS bool `json:"hasPsidts"`
	// CookieMasked is a display-only rendering of the credential.
	CookieMasked string `json:"cookieMasked"`
	// CookieNames lists which cookies the paste actually contained.
	CookieNames []string `json:"cookieNames"`
	Inflight    int      `json:"inflight"`
}

// NewAccountView projects an account for API responses.
func NewAccountView(account *Account, inflight int) AccountView {
	return AccountView{
		ID: account.ID, Name: account.Name, Kind: account.Kind,
		Identifier: account.Identifier, AuthUser: account.AuthUser,
		Model: account.Model, BaseURL: account.BaseURL,
		Group: account.Group, Remark: account.Remark,
		Enabled: account.Enabled, Priority: account.Priority,
		MaxConcurrent: account.MaxConcurrent, Status: account.Status,
		CooldownUntil: account.CooldownUntil, FailCount: account.FailCount,
		SuccessCount: account.SuccessCount, LastUsedAt: account.LastUsedAt,
		LastError: account.LastError, CreatedAt: account.CreatedAt, UpdatedAt: account.UpdatedAt,
		Quota:           account.Quota,
		RefreshAt:       account.RefreshAt,
		RefreshStatus:   account.RefreshStatus,
		RefreshError:    account.RefreshError,
		RefreshFailures: account.RefreshFailures,
		BuildLabel:      account.BuildLabel,
		Language:        account.Language,
		HasPSIDTS:       account.PSIDTS != "",
		CookieMasked:    MaskCookie(account.Cookie),
		CookieNames:     CookieNamesOf(account.Cookie),
		Inflight:        inflight,
	}
}

type ClientKey struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Key           string    `json:"key"`
	Enabled       bool      `json:"enabled"`
	RPMLimit      int       `json:"rpmLimit"`
	MaxConcurrent int       `json:"maxConcurrent"`
	TotalRequests int64     `json:"totalRequests"`
	CreatedAt     time.Time `json:"createdAt"`
	LastUsedAt    time.Time `json:"lastUsedAt"`
}

type Audit struct {
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"createdAt"`
	KeyName          string    `json:"keyName"`
	Model            string    `json:"model"`
	AccountName      string    `json:"accountName"`
	Status           int       `json:"status"`
	LatencyMs        int64     `json:"latencyMs"`
	FirstTokenMs     int64     `json:"firstTokenMs"`
	PromptTokens     int       `json:"promptTokens"`
	CompletionTokens int       `json:"completionTokens"`
	Stream           bool      `json:"stream"`
	Retries          int       `json:"retries"`
	IP               string    `json:"ip"`
	UserAgent        string    `json:"userAgent"`
	Error            string    `json:"error"`
	RequestBody      string    `json:"requestBody"`
	ResponseBody     string    `json:"responseBody"`
}

type MediaItem struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	URL         string    `json:"url"`
	SourceURL   string    `json:"sourceUrl"`
	Prompt      string    `json:"prompt"`
	Model       string    `json:"model"`
	AccountName string    `json:"accountName"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Admin struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
	Salt         string `json:"salt"`
}

type ModelConfig struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Upstream    string `json:"upstream"`
	Type        string `json:"type"`
	Enabled     bool   `json:"enabled"`
	Builtin     bool   `json:"builtin"`
	Description string `json:"description"`
	Requests    int64  `json:"requests"`
	Tokens      int64  `json:"tokens"`
}

type State struct {
	Version    int             `json:"version"`
	Admin      Admin           `json:"admin"`
	Settings   config.Settings `json:"settings"`
	Accounts   []*Account      `json:"accounts"`
	ClientKeys []*ClientKey    `json:"clientKeys"`
	Audits     []*Audit        `json:"audits"`
	Media      []*MediaItem    `json:"media"`
	Models     []*ModelConfig  `json:"models"`
}

// BuiltinModels is the model catalogue exposed through /v1/models.
//
// It is *derived* from gemini.BuiltinModels rather than written out again. The
// two used to be separate lists and had already drifted — the store's copy was
// missing two entries — which is a quiet failure mode: a model the client can
// serve but the console does not list is invisible to every operator, and a
// model the console lists but the client cannot resolve is a 400 with no
// explanation. Deriving makes drift impossible rather than merely discouraged.
func BuiltinModels() []*ModelConfig {
	specs := gemini.BuiltinModels()
	out := make([]*ModelConfig, 0, len(specs))
	for _, spec := range specs {
		out = append(out, &ModelConfig{
			ID:       spec.ID,
			Name:     modelDisplayName(spec.ID),
			Upstream: spec.Upstream,
			Type:     "chat",
			Enabled:  true,
			Builtin:  true,
			// The tier is part of the description rather than a field because
			// it is not selectable — an account's tier is a fact about the
			// account, and a client cannot talk the upstream into a different
			// one.
			Description: spec.Description,
		})
	}
	return out
}

// modelDisplayName turns `gemini-flash-lite-advanced` into
// `Gemini Flash Lite Advanced`. The ids are the only source of truth for the
// name, so deriving it keeps the console in step with the catalogue.
func modelDisplayName(id string) string {
	parts := strings.Split(id, "-")
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}
