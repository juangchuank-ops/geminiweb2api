package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"geminiweb2api/internal/config"
	"geminiweb2api/internal/gateway"
	"geminiweb2api/internal/gemini"
	"geminiweb2api/internal/pool"
	"geminiweb2api/internal/refresher"
	"geminiweb2api/internal/store"
)

// The console is the only interface an operator has, so its projections and its
// input parsing are the surface that has to be right. Everything here is about
// one of those two: what a pasted cookie becomes, and what the settings page is
// allowed to hide.

// testCookie builds a plausible paste. The seed only has to be unique per
// account, because __Secure-1PSID is what identifies it.
func testCookie(seed string) string {
	return "__Secure-1PSID=g.a000" + seed + "psidvalue0000000000; __Secure-1PSIDTS=sidts-" + seed
}

type harness struct {
	api      *API
	store    *store.Store
	pool     *pool.Pool
	settings *config.Settings
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dir := t.TempDir()
	st, err := store.Open(dir, "admin", "admin12345")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	settings := config.DefaultSettings(dir)
	// Point upstream at a closed local port. The console fires detached probes
	// and rotations, and a test must not reach the internet — a refused
	// connection on loopback fails immediately instead of hanging.
	settings.Upstream.BaseURL = "http://127.0.0.1:1"
	settings.Upstream.RequestTimeoutSec = 5

	settingsFn := func() config.Settings { return settings }
	client := gemini.New(settingsFn)
	p := pool.New(st, settingsFn)

	return &harness{
		api:      New(st, p, client, nil, settingsFn),
		store:    st,
		pool:     p,
		settings: &settings,
	}
}

// addAccount inserts an account through the store, for tests that need one to
// already exist.
func (h *harness) addAccount(t *testing.T, seed string) *store.Account {
	t.Helper()
	account, err := buildAccount(accountPayload{Cookie: testCookie(seed)})
	if err != nil {
		t.Fatalf("buildAccount: %v", err)
	}
	if err := h.store.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	return account
}

// patch drives updateAccount the way the router does, path value included.
func (h *harness) patch(t *testing.T, id string, payload accountPayload) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/accounts/"+id, strings.NewReader(string(raw)))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.api.updateAccount(rec, req)
	return rec
}

// --- what a pasted cookie becomes -------------------------------------------

func TestBuildAccountReadsTheCredentialOutOfThePaste(t *testing.T) {
	account, err := buildAccount(accountPayload{Cookie: testCookie("abc")})
	if err != nil {
		t.Fatalf("buildAccount: %v", err)
	}

	if account.PSID != "g.a000abcpsidvalue0000000000" {
		t.Fatalf("psid = %q, want it read out of the cookie", account.PSID)
	}
	if account.PSIDTS != "sidts-abc" {
		t.Fatalf("psidts = %q, want it read out of the cookie", account.PSIDTS)
	}
	if account.Kind != store.KindCookie {
		t.Fatalf("kind = %q, want %q", account.Kind, store.KindCookie)
	}
	// The cookie itself is stored whole: Google's edge inspects the full jar,
	// and a jar reassembled from two named cookies is a different jar.
	if account.Cookie != testCookie("abc") {
		t.Fatalf("the paste was not stored verbatim: %q", account.Cookie)
	}
	// The label is what the console shows, so an account without one is an
	// account nobody can identify in a list.
	if account.Identifier == "" || account.Name == "" {
		t.Fatalf("the account has no label: identifier=%q name=%q", account.Identifier, account.Name)
	}
	if account.Identifier != account.Name {
		t.Fatalf("identifier %q and name %q should agree when no name was given", account.Identifier, account.Name)
	}
	// Defaults have to be usable without the operator touching anything.
	if account.Priority <= 0 || account.MaxConcurrent <= 0 {
		t.Fatalf("defaults are unusable: priority=%d maxConcurrent=%d", account.Priority, account.MaxConcurrent)
	}
	if !account.Enabled {
		t.Fatal("a freshly added account should be enabled")
	}
}

// The label must not be the credential. It is shown on every page of the
// console, so leaking the whole PSID there would leak the session.
func TestBuildAccountLabelIsNotTheCredential(t *testing.T) {
	account, err := buildAccount(accountPayload{Cookie: testCookie("abc")})
	if err != nil {
		t.Fatalf("buildAccount: %v", err)
	}
	if strings.Contains(account.Identifier, account.PSID) {
		t.Fatalf("the label %q contains the whole PSID", account.Identifier)
	}
	if len(account.Identifier) > 24 {
		t.Fatalf("the label %q is too long for a table column", account.Identifier)
	}
}

func TestBuildAccountRejectsUnusableCookies(t *testing.T) {
	cases := map[string]string{
		"empty":            "   ",
		"not a cookie":     "hello world",
		"only psidts":      "__Secure-1PSIDTS=sidts-only",
		"psid too short":   "__Secure-1PSID=g.a000short",
		"cookie name only": "__Secure-1PSID=",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			// An explicit cookie kind with a bad value is the case that must
			// fail. Falling through to a guest here would silently add an
			// account that answers every request with the guest quota.
			if _, err := buildAccount(accountPayload{Cookie: raw, Kind: store.KindCookie}); err == nil {
				t.Fatalf("expected %q to be rejected", raw)
			}
		})
	}
}

// A guest is a deliberate choice: no credential at all, still routable.
func TestBuildAccountAcceptsGuestWithoutCookie(t *testing.T) {
	account, err := buildAccount(accountPayload{Kind: store.KindGuest})
	if err != nil {
		t.Fatalf("buildAccount: %v", err)
	}
	if account.Kind != store.KindGuest {
		t.Fatalf("kind = %q, want %q", account.Kind, store.KindGuest)
	}
	if account.PSID != "" || account.Cookie != "" {
		t.Fatalf("a guest must carry no credential: psid=%q cookie=%q", account.PSID, account.Cookie)
	}
	if account.Name == "" {
		t.Fatal("a guest still needs a name in the list")
	}
}

// Omitting the kind entirely with no cookie is the same request as asking for a
// guest, and it must not error: the console's "add account" form sends only what
// the operator filled in.
func TestBuildAccountTreatsAnEmptyPasteAsAGuest(t *testing.T) {
	account, err := buildAccount(accountPayload{})
	if err != nil {
		t.Fatalf("buildAccount: %v", err)
	}
	if account.Kind != store.KindGuest {
		t.Fatalf("kind = %q, want %q", account.Kind, store.KindGuest)
	}
}

func TestBuildAccountHonoursExplicitFields(t *testing.T) {
	disabled := false
	authUser := 3
	account, err := buildAccount(accountPayload{
		Name: "  主号  ", Cookie: testCookie("abc"), Identifier: "my-label",
		AuthUser: &authUser, Model: "gemini-pro", BaseURL: "https://mirror.example",
		Group: "team-a", Remark: "  备用  ",
		Priority: 999, MaxConcurrent: 9999, Enabled: &disabled,
	})
	if err != nil {
		t.Fatalf("buildAccount: %v", err)
	}

	if account.Name != "主号" {
		t.Fatalf("name = %q, want it trimmed", account.Name)
	}
	if account.Identifier != "my-label" {
		t.Fatalf("identifier = %q, want the operator's label", account.Identifier)
	}
	if account.AuthUser != 3 {
		t.Fatalf("authUser = %d", account.AuthUser)
	}
	if account.Model != "gemini-pro" || account.BaseURL != "https://mirror.example" {
		t.Fatalf("model/baseURL not honoured: %+v", account)
	}
	if account.Group != "team-a" || account.Remark != "备用" {
		t.Fatalf("group/remark not honoured: group=%q remark=%q", account.Group, account.Remark)
	}
	// Out-of-range values are clamped rather than rejected: a console that
	// refuses to save because a spinner went past its own maximum is worse than
	// one that saves the maximum.
	if account.Priority != 100 {
		t.Fatalf("priority = %d, want it clamped to 100", account.Priority)
	}
	if account.MaxConcurrent != 256 {
		t.Fatalf("maxConcurrent = %d, want it clamped to 256", account.MaxConcurrent)
	}
	if account.Enabled {
		t.Fatal("enabled=false was not honoured")
	}
}

// --- replacing a cookie through the console ---------------------------------

// TestUpdateAccountWithNewCookieResetsTheRefreshBookkeeping covers the half of
// "refresh token" that lives in the console.
//
// A replacement cookie is a new session. The rotation bookkeeping belongs to the
// old one, so leaving it in place means the sweep waits out an interval that was
// measured against a credential that no longer exists — and, worse, an account
// retired for a dead cookie stays retired after the operator has pasted a live
// one, which reads as the console ignoring them.
func TestUpdateAccountWithNewCookieResetsTheRefreshBookkeeping(t *testing.T) {
	h := newHarness(t)
	account := h.addAccount(t, "old")

	// Age the account into the state a dead cookie leaves behind.
	h.store.SaveAccountState(account.ID, func(a *store.Account) {
		a.Status = store.StatusInvalid
		a.FailCount = 4
		a.LastError = "cookie 已失效"
		a.RefreshAt = time.Now().Add(-time.Hour)
		a.RefreshFailures = 3
		a.RefreshStatus = store.RefreshInvalid
		a.RefreshError = "unauthenticated"
	})

	rec := h.patch(t, account.ID, accountPayload{Cookie: testCookie("new")})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	updated, ok := h.store.AccountByID(account.ID)
	if !ok {
		t.Fatal("account disappeared")
	}
	if updated.PSID != "g.a000newpsidvalue0000000000" {
		t.Fatalf("psid = %q, want the replacement's", updated.PSID)
	}
	if updated.PSIDTS != "sidts-new" {
		t.Fatalf("psidts = %q, want the replacement's", updated.PSIDTS)
	}
	if updated.Cookie != testCookie("new") {
		t.Fatalf("cookie was not replaced: %q", updated.Cookie)
	}
	// The exact status is deliberately not asserted. A cookie replacement fires
	// a detached re-probe, and against the closed port this harness points at
	// that probe parks the account in cooldown — which is correct, and is also a
	// race with this read. What is deterministic, and what actually matters, is
	// that the account is no longer retired: an operator who pastes a live
	// cookie must not still be looking at an invalid account.
	if updated.Status == store.StatusInvalid {
		t.Fatal("the replacement did not un-retire the account")
	}
	// LastError is not asserted for the same reason: the detached probe owns it
	// from here on, and what it writes is the probe's outcome, not the old
	// cookie's.
	if updated.FailCount != 0 {
		t.Fatalf("failCount = %d, want the replacement to clear it", updated.FailCount)
	}
	if !updated.RefreshAt.IsZero() || updated.RefreshFailures != 0 ||
		updated.RefreshStatus != "" || updated.RefreshError != "" {
		t.Fatalf("rotation bookkeeping survived the replacement: %+v", updated)
	}
}

// An update that does not mention the cookie must leave it alone. Clearing it
// would make the account unroutable — the pool checks the PSID — and the
// operator's only clue would be that requests stopped.
func TestUpdateAccountWithoutCookieKeepsTheCredential(t *testing.T) {
	h := newHarness(t)
	account := h.addAccount(t, "keep")

	rec := h.patch(t, account.ID, accountPayload{Name: "renamed", Priority: 7})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	updated, ok := h.store.AccountByID(account.ID)
	if !ok {
		t.Fatal("account disappeared")
	}
	if updated.Cookie != testCookie("keep") || updated.PSID != "g.a000keeppsidvalue0000000000" {
		t.Fatalf("the credential was disturbed: cookie=%q psid=%q", updated.Cookie, updated.PSID)
	}
	if updated.Name != "renamed" || updated.Priority != 7 {
		t.Fatalf("the update did not apply: name=%q priority=%d", updated.Name, updated.Priority)
	}
}

// Omitting authUser must not reset it. Getting the index wrong does not error —
// it answers as a *different* Google account — so a rename that silently moved
// an account from /u/1 to /u/0 would be invisible until someone noticed the
// answers were coming from the wrong account.
func TestUpdateAccountWithoutAuthUserKeepsTheIndex(t *testing.T) {
	h := newHarness(t)
	account := h.addAccount(t, "multi")
	authUser := 2
	h.store.SaveAccountState(account.ID, func(a *store.Account) { a.AuthUser = authUser })

	rec := h.patch(t, account.ID, accountPayload{Name: "still the second account"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	updated, _ := h.store.AccountByID(account.ID)
	if updated.AuthUser != 2 {
		t.Fatalf("authUser = %d, want 2: an omitted index must not be read as 0", updated.AuthUser)
	}

	// And an explicit 0 is still honoured, because that is a real request.
	zero := 0
	rec = h.patch(t, account.ID, accountPayload{AuthUser: &zero})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	updated, _ = h.store.AccountByID(account.ID)
	if updated.AuthUser != 0 {
		t.Fatalf("authUser = %d, want an explicit 0 to be applied", updated.AuthUser)
	}
}

// A malformed replacement must be refused before anything is written, so the
// account keeps working while the operator fixes their paste.
func TestUpdateAccountRejectsABadReplacementWithoutTouchingTheAccount(t *testing.T) {
	h := newHarness(t)
	account := h.addAccount(t, "keep")

	rec := h.patch(t, account.ID, accountPayload{Cookie: "not-a-cookie"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}

	updated, _ := h.store.AccountByID(account.ID)
	if updated.Cookie != testCookie("keep") || updated.PSID != "g.a000keeppsidvalue0000000000" {
		t.Fatalf("a rejected replacement changed the account: cookie=%q psid=%q", updated.Cookie, updated.PSID)
	}
}

func TestUpdateAccountReportsAMissingAccount(t *testing.T) {
	h := newHarness(t)
	rec := h.patch(t, "acc_missing", accountPayload{Name: "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// --- import parsing ---------------------------------------------------------

func TestParseImportShapes(t *testing.T) {
	cookies := testCookie("a") + "\n" +
		"named----" + testCookie("b") + "\n" +
		"# a comment\n" +
		"\n" +
		"   " + testCookie("c") + "   \n"

	entries := parseImport(cookies, nil)
	if len(entries) != 3 {
		t.Fatalf("parsed %d entries, want 3: %+v", len(entries), entries)
	}
	if entries[0].Cookie != testCookie("a") || entries[0].Name != "" {
		t.Fatalf("a bare cookie parsed as %+v", entries[0])
	}
	if entries[1].Name != "named" || entries[1].Cookie != testCookie("b") {
		t.Fatalf("name----cookie parsed as %+v", entries[1])
	}
	if entries[2].Cookie != testCookie("c") {
		t.Fatalf("surrounding whitespace was not trimmed: %q", entries[2].Cookie)
	}
}

// JSON input wins over the text box, which is what makes an export/import round
// trip lossless.
func TestParseImportPrefersJSON(t *testing.T) {
	blob, err := json.Marshal(map[string]any{
		"accounts": []map[string]any{{"cookie": testCookie("json"), "name": "from-json"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	entries := parseImport(testCookie("text"), blob)
	if len(entries) != 1 || entries[0].Cookie != testCookie("json") || entries[0].Name != "from-json" {
		t.Fatalf("entries = %+v, want the JSON payload", entries)
	}
}

// An import that says nothing about the account index must not be read as
// "index 0", for the same reason the console must not: the two are different
// Google accounts.
func TestImportEntryAuthUserIsOptional(t *testing.T) {
	if got := (importEntry{}).authUserPtr(); got != nil {
		t.Fatalf("an unmentioned index became %d", *got)
	}
	if got := (importEntry{AuthUser: 0}).authUserPtr(); got != nil {
		t.Fatalf("an explicit 0 should still be treated as unset for import purposes, got %d", *got)
	}
	got := (importEntry{AuthUser: 2}).authUserPtr()
	if got == nil || *got != 2 {
		t.Fatalf("authUserPtr = %v, want 2", got)
	}
}

// describeCookie is what keeps a cookie from leaking into an error message
// shown in the console.
func TestDescribeCookieMasksTheCredential(t *testing.T) {
	described := describeCookie(testCookie("abc"))
	if strings.Contains(described, "abcpsidvalue0000000000") {
		t.Fatalf("describeCookie leaked the PSID: %q", described)
	}
	if described == "" {
		t.Fatal("describeCookie returned nothing to show")
	}
	if got := describeCookie(""); got == "" {
		t.Fatal("an unusable cookie still needs a placeholder")
	}
}

// --- settings projection ----------------------------------------------------

// jsonKeys returns a struct's json field names, skipping anything untagged.
func jsonKeys(t *testing.T, value any) map[string]bool {
	t.Helper()
	kind := reflect.TypeOf(value)
	keys := make(map[string]bool, kind.NumField())
	for i := range kind.NumField() {
		name := strings.Split(kind.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		keys[name] = true
	}
	return keys
}

// getSettings builds its response by hand rather than marshalling
// config.Settings. That is deliberate — the console must not receive every
// internal knob — but it means a new setting is invisible to the console until
// someone remembers to add it here. This test is that reminder: a setting that
// exists but cannot be seen or edited is a setting that does not exist.
func TestGetSettingsExposesEveryProjectedField(t *testing.T) {
	h := newHarness(t)

	rec := httptest.NewRecorder()
	h.api.getSettings(rec, httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}

	// Only the sections written out field by field; the rest are handed to the
	// encoder wholesale and cannot drift.
	cases := []struct {
		section string
		value   any
	}{
		{"server", h.settings.Server},
		{"upstream", h.settings.Upstream},
	}
	for _, tc := range cases {
		want := jsonKeys(t, tc.value)
		got := body[tc.section]
		if got == nil {
			t.Fatalf("the %s section is missing entirely", tc.section)
		}
		for key := range want {
			if _, ok := got[key]; !ok {
				t.Errorf("%s is missing %q, so the console cannot show or edit it", tc.section, key)
			}
		}
		for key := range got {
			if !want[key] {
				t.Errorf("%s exposes %q, which is not a field of the settings struct", tc.section, key)
			}
		}
	}

	// The refresh section is what makes the cookie keep-alive configurable, and
	// it is the feature the whole "刷新 token" ask is about, so its presence is
	// asserted explicitly rather than left to the loop above.
	if body["refresh"] == nil {
		t.Fatal("the refresh section is missing, so the sweep cannot be configured from the console")
	}
	for _, key := range []string{"enabled", "intervalMin", "gapSeconds", "timeoutSec", "retireAfterFailures"} {
		if _, ok := body["refresh"][key]; !ok {
			t.Errorf("refresh is missing %q", key)
		}
	}
}

// The server section reports the *store's* admin username, not the settings
// copy: the store is what the login actually checks against, and a console that
// shows a different name than the one that works is worse than showing nothing.
func TestGetSettingsReportsTheRealAdminUsername(t *testing.T) {
	h := newHarness(t)
	// SetPassword is the only path that renames the administrator, because the
	// name and the credential have to move together.
	if err := h.store.SetPassword("operator", "operator-password"); err != nil {
		t.Fatalf("set username: %v", err)
	}

	rec := httptest.NewRecorder()
	h.api.getSettings(rec, httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil))

	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["server"]["adminUsername"] != "operator" {
		t.Fatalf("adminUsername = %v, want the stored one", body["server"]["adminUsername"])
	}
}

// --- cookie rotation routes -------------------------------------------------
//
// The rotation routes are the console's only window onto the sweep, and they
// carry a status-code contract the frontend depends on: a rotation that did not
// happen must never come back as a 200. These tests pin that down, plus the
// one thing the Go-level sweep tests cannot see — that the reason survives the
// trip through the HTTP response.

// rotator stands in for accounts.google.com/RotateCookies.
//
// Pointing the endpoint at a stub is only possible because the URL is a
// setting. Hard-coding it made this whole path untestable, which is why it was
// lifted out.
type rotator struct {
	mu     sync.Mutex
	server *httptest.Server
	calls  int
	status int
	fresh  string
}

func newRotator(t *testing.T) *rotator {
	t.Helper()

	r := &rotator{status: http.StatusOK, fresh: "sidts-rotated"}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.calls++
		status, fresh := r.status, r.fresh
		r.mu.Unlock()

		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: gemini.CookiePSIDTS, Value: fresh, Path: "/"})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *rotator) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *rotator) answer(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

// newRotationHarness is newHarness with a live refresher aimed at a stub.
//
// The gap is zeroed so a sweep over several accounts finishes instantly, and
// the base URL still points at a closed port so the detached *probe* the
// console fires cannot reach the internet.
func newRotationHarness(t *testing.T) (*harness, *rotator) {
	t.Helper()

	dir := t.TempDir()
	st, err := store.Open(dir, "admin", "admin12345")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rot := newRotator(t)

	settings := config.DefaultSettings(dir)
	settings.Upstream.BaseURL = "http://127.0.0.1:1"
	settings.Upstream.RequestTimeoutSec = 5
	settings.Upstream.RotateURL = rot.server.URL
	settings.Refresh.TimeoutSec = 5
	settings.Refresh.GapSeconds = 0

	settingsFn := func() config.Settings { return settings }
	client := gemini.New(settingsFn)
	p := pool.New(st, settingsFn)
	service := refresher.New(st, client, settingsFn, gateway.CredentialOf)

	return &harness{api: New(st, p, client, service, settingsFn), store: st, pool: p, settings: &settings}, rot
}

// rotate drives refreshAccount the way the router does, path value included.
func (h *harness) rotate(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/"+id+"/refresh", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.api.refreshAccount(rec, req)
	return rec
}

type refreshResponse struct {
	Error   string `json:"error"`
	Outcome struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"outcome"`
	Account struct {
		RefreshStatus   string `json:"refreshStatus"`
		RefreshFailures int    `json:"refreshFailures"`
		HasPsidts       bool   `json:"hasPsidts"`
		Status          string `json:"status"`
	} `json:"account"`
}

func decodeRefresh(t *testing.T, rec *httptest.ResponseRecorder) refreshResponse {
	t.Helper()
	var body refreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return body
}

// A rejected rotation must not read as success. The console keys its toast off
// the status code, so a 200 here would announce "refreshed" about the one thing
// that certainly did not happen.
func TestRefreshAccountReportsARejectedRotationAsABadGateway(t *testing.T) {
	h, rot := newRotationHarness(t)
	rot.answer(http.StatusUnauthorized)

	account := h.addAccount(t, "rejected")
	rec := h.rotate(t, account.ID)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}

	body := decodeRefresh(t, rec)
	if body.Outcome.Status != store.RefreshInvalid {
		t.Fatalf("outcome.status = %q, want %q", body.Outcome.Status, store.RefreshInvalid)
	}
	// The console reads `error` off any failed response. Without it an operator
	// is shown "HTTP 502" and cannot tell a dead cookie from a hiccup — and the
	// difference decides whether they re-paste or just retry.
	if body.Error == "" {
		t.Fatalf("a 502 must carry a reason, got %s", rec.Body.String())
	}
	if body.Error != body.Outcome.Error {
		t.Fatalf("error %q should repeat the outcome's reason %q", body.Error, body.Outcome.Error)
	}
}

// A throttle is about the caller's rate, not the account. Dressing it up as a
// failure is how an operator deletes an account that was never broken.
func TestRefreshAccountReportsAThrottleAsSuccess(t *testing.T) {
	h, rot := newRotationHarness(t)
	rot.answer(http.StatusTooManyRequests)

	account := h.addAccount(t, "throttled")
	rec := h.rotate(t, account.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a throttle (body %s)", rec.Code, rec.Body.String())
	}
	if body := decodeRefresh(t, rec); body.Outcome.Status != store.RefreshThrottled {
		t.Fatalf("outcome.status = %q, want %q", body.Outcome.Status, store.RefreshThrottled)
	}

	// And it must not have counted toward retirement.
	live, _ := h.store.AccountByID(account.ID)
	if live.Status == store.StatusInvalid {
		t.Fatalf("a throttle retired the account")
	}
	if live.RefreshFailures != 0 {
		t.Fatalf("refreshFailures = %d, want 0 — throttling is not the account's fault", live.RefreshFailures)
	}
}

// The rotation has to land in the store. A sweep that reports success while the
// next request still sends the expired cookie has done nothing at all.
func TestRefreshAccountPersistsTheRotatedCookie(t *testing.T) {
	h, _ := newRotationHarness(t)

	account := h.addAccount(t, "persist")
	rec := h.rotate(t, account.ID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	live, ok := h.store.AccountByID(account.ID)
	if !ok {
		t.Fatalf("the account vanished")
	}
	if live.PSIDTS != "sidts-rotated" {
		t.Fatalf("PSIDTS = %q, want the rotated value", live.PSIDTS)
	}
	// The raw cookie is rewritten too: it is the other view of the same jar,
	// and a stale copy would make the console show a credential that no longer
	// matches what is being sent.
	if !strings.Contains(live.Cookie, "sidts-rotated") {
		t.Fatalf("cookie %q still carries the old PSIDTS", live.Cookie)
	}
	if live.RefreshStatus != store.RefreshOK {
		t.Fatalf("refreshStatus = %q, want %q", live.RefreshStatus, store.RefreshOK)
	}
	if live.RefreshAt.IsZero() {
		t.Fatalf("refreshAt was not stamped, so the sweep would rotate again immediately")
	}
	if live.RefreshFailures != 0 {
		t.Fatalf("refreshFailures = %d, want 0", live.RefreshFailures)
	}

	if body := decodeRefresh(t, rec); !body.Account.HasPsidts {
		t.Fatalf("hasPsidts = false after a successful rotation")
	}
}

// An unknown id is a 404, not a crash and not a silent 200.
func TestRefreshAccountRejectsAnUnknownID(t *testing.T) {
	h, _ := newRotationHarness(t)

	rec := h.rotate(t, "acct_does_not_exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// The overview is what the toolbar reads to decide whether to say anything at
// all, so its shape is a contract, not an implementation detail.
func TestRefreshOverviewDescribesTheScheduler(t *testing.T) {
	h, _ := newRotationHarness(t)

	never := h.addAccount(t, "never-rotated")

	guest, err := buildAccount(accountPayload{Kind: store.KindGuest})
	if err != nil {
		t.Fatalf("buildAccount(guest): %v", err)
	}
	if err := h.store.AddAccount(guest); err != nil {
		t.Fatalf("add guest: %v", err)
	}

	rec := httptest.NewRecorder()
	h.api.refreshOverview(rec, httptest.NewRequest(http.MethodGet, "/admin/api/refresh", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body struct {
		Enabled       bool           `json:"enabled"`
		IntervalMin   int            `json:"intervalMin"`
		GapSeconds    int            `json:"gapSeconds"`
		TimeoutSec    int            `json:"timeoutSec"`
		RetireAfter   int            `json:"retireAfter"`
		Counts        map[string]int `json:"counts"`
		TotalAccounts int            `json:"totalAccounts"`
		NextRunAt     string         `json:"nextRunAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if body.TotalAccounts != 2 {
		t.Fatalf("totalAccounts = %d, want 2 — the total is the whole pool", body.TotalAccounts)
	}
	// counts, unlike totalAccounts, only covers what the sweep can act on. The
	// guest has no cookie to rotate, so counting it as pending would leave the
	// toolbar claiming work that will never happen.
	if body.Counts["pending"] != 1 {
		t.Fatalf("counts = %v, want exactly 1 pending (the guest is not rotatable)", body.Counts)
	}
	if body.IntervalMin <= 0 {
		t.Fatalf("intervalMin = %d, want the configured interval", body.IntervalMin)
	}
	// An account that has never rotated is due right now, so the next run is
	// not in the future. The console renders this as "next rotation".
	if body.NextRunAt == "" {
		t.Fatalf("nextRunAt is missing")
	}

	_ = never
}

// refresh-all is the button an operator presses because something is already
// wrong. It must not spend a rotation call on an account that has no cookie.
func TestRefreshAllOnlyRotatesAccountsThatHaveSomethingToRotate(t *testing.T) {
	h, rot := newRotationHarness(t)

	h.addAccount(t, "eligible")

	guest, err := buildAccount(accountPayload{Kind: store.KindGuest})
	if err != nil {
		t.Fatalf("buildAccount(guest): %v", err)
	}
	if err := h.store.AddAccount(guest); err != nil {
		t.Fatalf("add guest: %v", err)
	}

	disabled := h.addAccount(t, "disabled")
	if _, err := h.store.UpdateAccount(disabled.ID, func(account *store.Account) {
		account.Enabled = false
	}); err != nil {
		t.Fatalf("disable account: %v", err)
	}

	rec := httptest.NewRecorder()
	h.api.refreshAll(rec, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/refresh-all", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body struct {
		Summary struct {
			Total   int `json:"total"`
			OK      int `json:"ok"`
			Skipped int `json:"skipped"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if body.Summary.Total != 1 {
		t.Fatalf("total = %d, want only the one eligible account", body.Summary.Total)
	}
	if body.Summary.OK != 1 {
		t.Fatalf("ok = %d, want 1", body.Summary.OK)
	}
	if calls := rot.callCount(); calls != 1 {
		t.Fatalf("the stub was called %d times, want 1 — guests and disabled accounts must be filtered out before the call", calls)
	}
}

// --- probing a guest --------------------------------------------------------

// A guest account must survive its own probe.
//
// Regression guard. The probe used to run against every account, and a guest
// has no cookie, so the upstream answered "credential rejected" — which the
// probe recorded as a dead credential and retired the account. Since a retired
// account is not routable, that turned the guest fallback into an account that
// was dead on arrival: added, immediately invalid, never used. The one account
// kind that is supposed to work without a credential was the only one that
// could not.
func TestProbingAGuestDoesNotRetireIt(t *testing.T) {
	h := newHarness(t)

	guest, err := buildAccount(accountPayload{Kind: store.KindGuest})
	if err != nil {
		t.Fatalf("buildAccount(guest): %v", err)
	}
	if err := h.store.AddAccount(guest); err != nil {
		t.Fatalf("add guest: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/"+guest.ID+"/probe", nil)
	req.SetPathValue("id", guest.ID)
	rec := httptest.NewRecorder()
	h.api.probeAccount(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var body struct {
		OK      bool `json:"ok"`
		Account struct {
			Kind   string `json:"kind"`
			Status string `json:"status"`
			Quota  *struct {
				Available     bool   `json:"available"`
				Authenticated bool   `json:"authenticated"`
				Note          string `json:"note"`
			} `json:"quota"`
		} `json:"account"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if !body.OK {
		t.Fatalf("probing a guest reported failure")
	}
	if body.Account.Status == store.StatusInvalid {
		t.Fatalf("the guest was retired by its own probe — it can no longer serve traffic")
	}
	if body.Account.Status != store.StatusActive {
		t.Fatalf("status = %q, want %q", body.Account.Status, store.StatusActive)
	}
	if body.Account.Quota == nil {
		t.Fatalf("no quota snapshot was recorded")
	}
	// Unauthenticated is correct and expected here; what matters is that the
	// console can tell "a guest, as designed" from "a cookie that lapsed".
	if body.Account.Quota.Authenticated {
		t.Fatalf("a guest reported an authenticated session")
	}
	if body.Account.Quota.Note == "" {
		t.Fatalf("the snapshot carries no explanation")
	}

	// And it must still be routable, which is the whole point: a guest exists to
	// answer when every cookie has failed, so an unusable guest is no fallback.
	if _, _, _, _, _, routable := h.pool.Summary(); routable < 1 {
		t.Fatalf("the guest is not routable after being probed (summary says %d routable)", routable)
	}
}

// A guest that a previous build had already retired must be brought back rather
// than staying broken: an operator upgrading should not have to delete and
// re-add their fallback account.
func TestProbingAGuestUnRetiresOneAlreadyMarkedInvalid(t *testing.T) {
	h := newHarness(t)

	guest, err := buildAccount(accountPayload{Kind: store.KindGuest})
	if err != nil {
		t.Fatalf("buildAccount(guest): %v", err)
	}
	if err := h.store.AddAccount(guest); err != nil {
		t.Fatalf("add guest: %v", err)
	}
	if _, err := h.store.UpdateAccount(guest.ID, func(account *store.Account) {
		account.Status = store.StatusInvalid
		account.LastError = "credential rejected: __Secure-1PSID is missing"
	}); err != nil {
		t.Fatalf("retire guest: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/"+guest.ID+"/probe", nil)
	req.SetPathValue("id", guest.ID)
	h.api.probeAccount(httptest.NewRecorder(), req)

	live, _ := h.store.AccountByID(guest.ID)
	if live.Status != store.StatusActive {
		t.Fatalf("status = %q, want the guest to be un-retired", live.Status)
	}
	if live.LastError != "" {
		t.Fatalf("lastError = %q, want it cleared", live.LastError)
	}
}
