package config

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config holds process-level bootstrap options. Runtime-tunable gateway
// parameters live in the persistent store instead.
type Config struct {
	Addr          string
	DataDir       string
	StaticDir     string
	AdminUser     string
	AdminPassword string
}

func Load() Config {
	var cfg Config
	flag.StringVar(&cfg.Addr, "addr", env("GEMINIWEB2API_ADDR", "127.0.0.1:8080"), "HTTP listen address")
	flag.StringVar(&cfg.DataDir, "data", env("GEMINIWEB2API_DATA", "data"), "persistent data directory")
	flag.StringVar(&cfg.StaticDir, "static", env("GEMINIWEB2API_STATIC", "frontend/dist"), "built frontend directory")
	flag.StringVar(&cfg.AdminUser, "admin-user", env("GEMINIWEB2API_ADMIN_USER", "admin"), "initial admin username")
	flag.StringVar(&cfg.AdminPassword, "admin-password", env("GEMINIWEB2API_ADMIN_PASSWORD", ""), "initial admin password (random when empty)")
	flag.Parse()

	if cfg.AdminPassword == "" {
		cfg.AdminPassword = env("GEMINIWEB2API_ADMIN_PASSWORD", "")
	}
	return cfg
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// Settings mirrors the runtime-tunable section exposed by the admin console.
type Settings struct {
	Server   ServerSettings   `json:"server"`
	Upstream UpstreamSettings `json:"upstream"`
	Routing  RoutingSettings  `json:"routing"`
	Audit    AuditSettings    `json:"audit"`
	Media    MediaSettings    `json:"media"`
	Refresh  RefreshSettings  `json:"refresh"`
}

// RefreshSettings drives the cookie keep-alive sweep.
//
// This is the feature that decides whether a pool survives a night. Gemini's
// __Secure-1PSID is long-lived, but __Secure-1PSIDTS expires within hours, and
// a session whose PSIDTS has lapsed is answered as a guest: requests still
// succeed, with a much smaller quota and no access to the account's models.
// Nothing in a response says so, which is why the sweep exists rather than
// being left to whoever notices the quality dropped.
type RefreshSettings struct {
	// Enabled turns the background sweep on. It is on by default because an
	// account pool with expired PSIDTS cookies is indistinguishable from a
	// working one until it quietly starts answering as a guest.
	Enabled bool `json:"enabled"`
	// IntervalMin is how often each account is rotated. Ten minutes matches
	// what Google's endpoint tolerates; going much below that earns a 429 that
	// persists for a while.
	IntervalMin int `json:"intervalMin"`
	// GapSeconds spaces accounts apart inside one sweep. Rotation is a
	// risk-control sensitive call, and a burst from one IP is the pattern that
	// gets the whole range throttled.
	GapSeconds int `json:"gapSeconds"`
	// TimeoutSec bounds a single rotation.
	TimeoutSec int `json:"timeoutSec"`
	// RetireAfterFailures retires an account once this many consecutive
	// rotations were rejected as unauthenticated. It is a safety valve for a
	// revoked session, not for a flaky network — only 401s count.
	RetireAfterFailures int `json:"retireAfterFailures"`
}

type ServerSettings struct {
	Addr                  string `json:"addr"`
	MaxConcurrentRequests int    `json:"maxConcurrentRequests"`
	AdminUsername         string `json:"adminUsername"`
}

// UpstreamSettings describes how to reach the Gemini web app.
//
// There is only one host here, unlike the sibling projects that proxy a
// service with separate mainland and international deployments: gemini.google.com
// is the same host worldwide, and the difference an operator actually has to
// deal with is the *egress* country, not the account's. Requests from a country
// where Gemini is not offered are answered with BardErrorInfo 1060 regardless of
// how healthy the cookie is, which is what the proxy setting is for.
type UpstreamSettings struct {
	BaseURL string `json:"baseURL"`
	// Language is the `hl` parameter and the account's reported UI language.
	// It also selects the language the model answers in when the prompt is
	// ambiguous, so it is a setting rather than a constant.
	Language string `json:"language"`
	// DefaultModel is used when a client names a model that is not in the
	// catalogue. Refusing instead would break every client that hardcodes a
	// model name, which is most of them.
	DefaultModel string `json:"defaultModel"`
	// RequestTimeoutSec bounds a whole call, including a long generation.
	RequestTimeoutSec int `json:"requestTimeoutSec"`
	// StreamIdleTimeoutSec bounds the gap *between* chunks. A stalled stream
	// that never closes is the common failure and a total timeout alone lets it
	// hold a pool slot for minutes.
	StreamIdleTimeoutSec int `json:"streamIdleTimeoutSec"`
	// Proxy is the egress for upstream calls. It is required whenever the
	// gateway runs in a country where Gemini is not offered.
	Proxy string `json:"proxy"`
	// UserAgent is sent on the app shell fetch and on generation.
	UserAgent string `json:"userAgent"`
	// RotateURL is the cookie-rotation endpoint. It is a setting rather than a
	// constant because it lives on a different host from the app itself, which
	// means a deployment that reaches Gemini through a mirror has to be able to
	// point the rotation at the same place — and because a hard-coded endpoint
	// is an endpoint no test can exercise.
	RotateURL string `json:"rotateURL"`
}

type RoutingSettings struct {
	Strategy        string `json:"strategy"`
	CooldownBaseSec int    `json:"cooldownBaseSec"`
	CooldownMaxSec  int    `json:"cooldownMaxSec"`
	MaxAttempts     int    `json:"maxAttempts"`
	CapacityWaitSec int    `json:"capacityWaitSec"`
	StickyTTLSec    int    `json:"stickyTTLSec"`
	PreferIdle      bool   `json:"preferIdle"`
}

type AuditSettings struct {
	RetentionDays  int  `json:"retentionDays"`
	MaxRecords     int  `json:"maxRecords"`
	RecordBody     bool `json:"recordBody"`
	BodyLimitBytes int  `json:"bodyLimitBytes"`
}

type MediaSettings struct {
	GeneratedDir   string `json:"generatedDir"`
	PublicBaseURL  string `json:"publicBaseURL"`
	MaxTotalSizeMB int    `json:"maxTotalSizeMB"`
	AutoDownload   bool   `json:"autoDownload"`
}

// Known-broken defaults shipped by an earlier build.
//
// Normalize only fills in *empty* values, and a fresh install freezes a copy of
// every default into the settings file — so a default that later turns out to
// be wrong stays wrong on every existing install, with no symptom. Listing the
// bad values here is what makes a fix actually reach those installs.
//
// There is one entry so far: an early build pointed BaseURL at a placeholder
// host, which produced a DNS failure that read like the account being broken.
const legacyDefaultBaseURL = "https://gemini.google.com/app"

// DefaultSettings returns the built-in runtime configuration.
func DefaultSettings(dataDir string) Settings {
	return Settings{
		Server: ServerSettings{
			Addr:                  "127.0.0.1:8080",
			MaxConcurrentRequests: 64,
			AdminUsername:         "admin",
		},
		Upstream: UpstreamSettings{
			BaseURL:              "https://gemini.google.com",
			Language:             "en",
			DefaultModel:         "gemini-flash",
			RequestTimeoutSec:    300,
			StreamIdleTimeoutSec: 120,
			Proxy:                "",
			UserAgent:            defaultUserAgent,
			RotateURL:            DefaultRotateURL,
		},
		Routing: RoutingSettings{
			Strategy:        "least_inflight",
			CooldownBaseSec: 60,
			CooldownMaxSec:  900,
			MaxAttempts:     3,
			CapacityWaitSec: 20,
			StickyTTLSec:    300,
			PreferIdle:      true,
		},
		Audit: AuditSettings{
			RetentionDays:  7,
			MaxRecords:     5000,
			RecordBody:     true,
			BodyLimitBytes: 8192,
		},
		Media: MediaSettings{
			GeneratedDir:   filepath.Join(dataDir, "generated"),
			PublicBaseURL:  "",
			MaxTotalSizeMB: 2048,
			AutoDownload:   true,
		},
		Refresh: RefreshSettings{
			// On by default. Turning it off is a deliberate choice to accept
			// that accounts silently degrade to guest quota overnight.
			Enabled:             true,
			IntervalMin:         10,
			GapSeconds:          3,
			TimeoutSec:          30,
			RetireAfterFailures: 3,
		},
	}
}

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// DefaultRotateURL is where Google reissues __Secure-1PSIDTS.
//
// It sits on accounts.google.com, not on gemini.google.com: the cookie that
// expires is a Google account cookie, and the app host does not reissue it.
const DefaultRotateURL = "https://accounts.google.com/RotateCookies"

// Normalize fills in absent settings and repairs known-broken legacy defaults,
// reporting whether it changed anything.
//
// The report matters because a repair that is not written back leaves the
// settings file describing a configuration the process is not using — and a
// file that disagrees with the running process is how a broken value survives a
// fix in the first place.
func (s *Settings) Normalize(dataDir string) bool {
	before := *s
	def := DefaultSettings(dataDir)

	if s.Server.MaxConcurrentRequests <= 0 {
		s.Server.MaxConcurrentRequests = def.Server.MaxConcurrentRequests
	}
	if s.Server.AdminUsername == "" {
		s.Server.AdminUsername = def.Server.AdminUsername
	}

	if s.Upstream.BaseURL == "" || s.Upstream.BaseURL == legacyDefaultBaseURL {
		s.Upstream.BaseURL = def.Upstream.BaseURL
	}
	if s.Upstream.Language == "" {
		s.Upstream.Language = def.Upstream.Language
	}
	if s.Upstream.DefaultModel == "" {
		s.Upstream.DefaultModel = def.Upstream.DefaultModel
	}
	if s.Upstream.RequestTimeoutSec < 5 {
		s.Upstream.RequestTimeoutSec = def.Upstream.RequestTimeoutSec
	}
	if s.Upstream.StreamIdleTimeoutSec < 5 {
		s.Upstream.StreamIdleTimeoutSec = def.Upstream.StreamIdleTimeoutSec
	}
	if s.Upstream.UserAgent == "" {
		s.Upstream.UserAgent = def.Upstream.UserAgent
	}
	if s.Upstream.RotateURL == "" {
		s.Upstream.RotateURL = def.Upstream.RotateURL
	}

	switch s.Routing.Strategy {
	case "least_inflight", "round_robin", "priority", "random":
	default:
		s.Routing.Strategy = def.Routing.Strategy
	}
	if s.Routing.CooldownBaseSec <= 0 {
		s.Routing.CooldownBaseSec = def.Routing.CooldownBaseSec
	}
	if s.Routing.CooldownMaxSec < s.Routing.CooldownBaseSec {
		s.Routing.CooldownMaxSec = def.Routing.CooldownMaxSec
	}
	if s.Routing.MaxAttempts < 1 || s.Routing.MaxAttempts > 20 {
		s.Routing.MaxAttempts = def.Routing.MaxAttempts
	}
	if s.Routing.CapacityWaitSec < 0 {
		s.Routing.CapacityWaitSec = def.Routing.CapacityWaitSec
	}
	if s.Routing.StickyTTLSec < 0 {
		s.Routing.StickyTTLSec = def.Routing.StickyTTLSec
	}

	if s.Audit.MaxRecords < 100 {
		s.Audit.MaxRecords = def.Audit.MaxRecords
	}
	if s.Audit.RetentionDays < 1 {
		s.Audit.RetentionDays = def.Audit.RetentionDays
	}
	if s.Audit.BodyLimitBytes < 256 {
		s.Audit.BodyLimitBytes = def.Audit.BodyLimitBytes
	}

	if s.Media.GeneratedDir == "" {
		s.Media.GeneratedDir = def.Media.GeneratedDir
	}
	if s.Media.MaxTotalSizeMB < 64 {
		s.Media.MaxTotalSizeMB = def.Media.MaxTotalSizeMB
	}

	if s.Refresh.IntervalMin <= 0 {
		s.Refresh.IntervalMin = def.Refresh.IntervalMin
	}
	if s.Refresh.GapSeconds < 0 {
		s.Refresh.GapSeconds = def.Refresh.GapSeconds
	}
	if s.Refresh.TimeoutSec < 5 {
		s.Refresh.TimeoutSec = def.Refresh.TimeoutSec
	}
	if s.Refresh.RetireAfterFailures <= 0 {
		s.Refresh.RetireAfterFailures = def.Refresh.RetireAfterFailures
	}

	return *s != before
}

func (s Settings) Clone() Settings {
	raw, err := json.Marshal(s)
	if err != nil {
		return s
	}
	var clone Settings
	if err := json.Unmarshal(raw, &clone); err != nil {
		return s
	}
	return clone
}

func (s Settings) RequestTimeout() time.Duration {
	return time.Duration(s.Upstream.RequestTimeoutSec) * time.Second
}

func (s Settings) StreamIdleTimeout() time.Duration {
	return time.Duration(s.Upstream.StreamIdleTimeoutSec) * time.Second
}

func (s Settings) CooldownBase() time.Duration {
	return time.Duration(s.Routing.CooldownBaseSec) * time.Second
}

func (s Settings) CooldownMax() time.Duration {
	return time.Duration(s.Routing.CooldownMaxSec) * time.Second
}

func (s Settings) CapacityWait() time.Duration {
	return time.Duration(s.Routing.CapacityWaitSec) * time.Second
}

func (s Settings) StickyTTL() time.Duration {
	return time.Duration(s.Routing.StickyTTLSec) * time.Second
}

func (s Settings) Retention() time.Duration {
	return time.Duration(s.Audit.RetentionDays) * 24 * time.Hour
}

func (s Settings) MediaLimitBytes() int64 {
	return int64(s.Media.MaxTotalSizeMB) * 1024 * 1024
}

// RefreshInterval is how long to wait between rotations of the same account.
func (s Settings) RefreshInterval() time.Duration {
	minutes := s.Refresh.IntervalMin
	if minutes <= 0 {
		minutes = 10
	}
	return time.Duration(minutes) * time.Minute
}

// RefreshGap is the pause between two accounts inside one sweep.
func (s Settings) RefreshGap() time.Duration {
	return time.Duration(s.Refresh.GapSeconds) * time.Second
}

// RefreshTimeout bounds a single rotation call.
func (s Settings) RefreshTimeout() time.Duration {
	seconds := s.Refresh.TimeoutSec
	if seconds <= 0 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

// ParseIntOrDefault is a small helper for query parameters.
func ParseIntOrDefault(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
