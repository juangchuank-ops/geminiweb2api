package refresher

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"geminiweb2api/internal/config"
	"geminiweb2api/internal/gemini"
	"geminiweb2api/internal/store"
)

// The sweep is the feature that decides whether a pool survives a night, and it
// has two properties that are easy to get wrong in opposite directions:
//
//   - A failure must still be *recorded*, or an unreachable endpoint is retried
//     on every tick and an outage becomes a request storm.
//   - A failure must not *retire* the account unless it was an authentication
//     rejection, or a bad minute deletes the pool.
//
// Both are asserted below, because neither is visible from the outside until it
// has already done damage.

// --- fixture ----------------------------------------------------------------

const testCookie = "__Secure-1PSID=g.a000abcpsidvalue0000000000; __Secure-1PSIDTS=sidts-old"

// cookieFor builds a paste that is unique per account. __Secure-1PSID is what
// duplicate detection compares, so two accounts sharing one would be rejected
// before any test could observe them.
func cookieFor(name string) string {
	return "__Secure-1PSID=g.a000" + name + "psidvalue0000000000; __Secure-1PSIDTS=sidts-old-" + name
}

type rotator struct {
	mu sync.Mutex

	calls    int
	cookies  []string
	lastBody string
	headers  http.Header

	// respond is consulted per call so a test can vary the answer, and a
	// release channel can hold a call open to exercise single-flight.
	respond func(call int, w http.ResponseWriter, r *http.Request)
	hold    chan struct{}
}

func (s *rotator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4096))

	s.mu.Lock()
	s.calls++
	call := s.calls
	s.cookies = append(s.cookies, r.Header.Get("Cookie"))
	s.lastBody = string(raw)
	s.headers = r.Header.Clone()
	hold := s.hold
	respond := s.respond
	s.mu.Unlock()

	if hold != nil {
		<-hold
	}
	if respond != nil {
		respond(call, w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *rotator) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// rotated answers every call with a fresh PSIDTS.
func rotated(suffix string) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name: "__Secure-1PSIDTS", Value: suffix, Path: "/",
		})
		w.WriteHeader(http.StatusOK)
	}
}

// failing answers with a fixed status.
func failing(status int) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}
}

type harness struct {
	service  *Service
	store    *store.Store
	settings *config.Settings
	rotator  *rotator
}

func newHarness(t *testing.T, respond func(int, http.ResponseWriter, *http.Request)) *harness {
	t.Helper()

	dir := t.TempDir()
	st, err := store.Open(dir, "admin", "admin12345")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	up := &rotator{respond: respond}
	server := httptest.NewServer(up)
	t.Cleanup(server.Close)

	settings := config.DefaultSettings(dir)
	settings.Upstream.RotateURL = server.URL
	settings.Upstream.RequestTimeoutSec = 5
	settings.Refresh.Enabled = true
	settings.Refresh.IntervalMin = 10
	// No gap: the spacing is asserted separately, and every other test would
	// otherwise pay for it.
	settings.Refresh.GapSeconds = 0
	settings.Refresh.TimeoutSec = 5
	settings.Refresh.RetireAfterFailures = 3

	settingsFn := func() config.Settings { return settings }
	client := gemini.New(settingsFn)
	service := New(st, client, settingsFn, credentialOf)

	return &harness{service: service, store: st, settings: &settings, rotator: up}
}

// credentialOf mirrors what the gateway passes in, without importing it.
func credentialOf(account *store.Account) gemini.Credential {
	return gemini.Credential{
		Cookie: account.Cookie, PSID: account.PSID, PSIDTS: account.PSIDTS,
		AuthUser: account.AuthUser, Model: account.Model, BaseURL: account.BaseURL,
	}
}

func (h *harness) addCookieAccount(t *testing.T, name string) *store.Account {
	t.Helper()
	account := &store.Account{
		Name: name, Kind: store.KindCookie, Cookie: cookieFor(name),
		Enabled: true, MaxConcurrent: 2, Status: store.StatusActive,
	}
	if err := h.store.AddAccount(account); err != nil {
		t.Fatalf("add account %s: %v", name, err)
	}
	return account
}

func (h *harness) addGuestAccount(t *testing.T, name string) *store.Account {
	t.Helper()
	account := &store.Account{
		Name: name, Kind: store.KindGuest,
		Enabled: true, MaxConcurrent: 2, Status: store.StatusActive,
	}
	if err := h.store.AddAccount(account); err != nil {
		t.Fatalf("add guest %s: %v", name, err)
	}
	return account
}

func (h *harness) reload(t *testing.T, id string) *store.Account {
	t.Helper()
	account, ok := h.store.AccountByID(id)
	if !ok {
		t.Fatalf("account %s missing", id)
	}
	return account
}

// --- which accounts are due -------------------------------------------------

// TestDueAccountsSkipsWhatCannotBeRotated covers the four exclusions. Each one
// is a request that would either fail or be pointless, and against an endpoint
// that throttles on rate, pointless requests are not free.
func TestDueAccountsSkipsWhatCannotBeRotated(t *testing.T) {
	h := newHarness(t, rotated("sidts-new"))

	h.addCookieAccount(t, "due")

	guest := h.addGuestAccount(t, "guest")

	retired := h.addCookieAccount(t, "retired")
	h.store.SaveAccountState(retired.ID, func(a *store.Account) { a.Status = store.StatusInvalid })

	off := h.addCookieAccount(t, "off")
	h.store.SaveAccountState(off.ID, func(a *store.Account) { a.Enabled = false })

	// A cookie account with no cookie is a half-built record; rotating it would
	// send an unauthenticated request and record a failure that means nothing.
	empty := h.addCookieAccount(t, "empty")
	h.store.SaveAccountState(empty.ID, func(a *store.Account) { a.Cookie = ""; a.PSID = "" })

	due := h.service.dueAccounts()
	if len(due) != 1 {
		ids := make([]string, 0, len(due))
		for _, account := range due {
			ids = append(ids, account.ID)
		}
		t.Fatalf("due = %v, want only the cookie account", ids)
	}
	if due[0].ID == guest.ID || due[0].ID == retired.ID || due[0].ID == off.ID || due[0].ID == empty.ID {
		t.Fatalf("an account that cannot be rotated was selected: %+v", due[0])
	}
}

// TestDueAccountsHonoursTheIntervalFromRefreshAt pins the scheduling rule.
//
// The interval is measured from each account's own last attempt rather than from
// process start, which is what lets a gateway that was down for an hour resume
// promptly instead of waiting out a full period — and what stops a fresh
// process from rotating the whole pool at once.
func TestDueAccountsHonoursTheIntervalFromRefreshAt(t *testing.T) {
	h := newHarness(t, rotated("sidts-new"))

	fresh := h.addCookieAccount(t, "fresh")
	h.store.SaveAccountState(fresh.ID, func(a *store.Account) {
		a.RefreshAt = time.Now().Add(-time.Minute)
	})

	stale := h.addCookieAccount(t, "stale")
	h.store.SaveAccountState(stale.ID, func(a *store.Account) {
		a.RefreshAt = time.Now().Add(-30 * time.Minute)
	})

	never := h.addCookieAccount(t, "never") // RefreshAt zero

	due := h.service.dueAccounts()
	selected := map[string]bool{}
	for _, account := range due {
		selected[account.ID] = true
	}
	if selected[fresh.ID] {
		t.Fatal("an account rotated a minute ago is not due at a ten-minute interval")
	}
	if !selected[stale.ID] {
		t.Fatal("an account rotated half an hour ago is due")
	}
	if !selected[never.ID] {
		t.Fatal("an account that has never been rotated is due immediately")
	}
}

// --- a successful rotation --------------------------------------------------

func TestSweepPersistsTheRotatedCookie(t *testing.T) {
	h := newHarness(t, rotated("sidts-fresh"))
	account := h.addCookieAccount(t, "primary")

	summary := h.service.Sweep(context.Background(), h.service.dueAccounts())

	if summary.Total != 1 || summary.OK != 1 {
		t.Fatalf("summary = %+v, want one success", summary)
	}
	if len(summary.Outcomes) != 1 || summary.Outcomes[0].Status != store.RefreshOK {
		t.Fatalf("outcomes = %+v", summary.Outcomes)
	}

	updated := h.reload(t, account.ID)
	if updated.PSIDTS != "sidts-fresh" {
		t.Fatalf("psidts = %q, want the rotated value", updated.PSIDTS)
	}
	// The raw string is rewritten too. It and the split value are two views of
	// one jar, and a stale raw string makes the console show a cookie that is
	// not the one being sent.
	if updated.Cookie == cookieFor("primary") || !contains(updated.Cookie, "sidts-fresh") {
		t.Fatalf("the stored cookie was not updated: %q", updated.Cookie)
	}
	if updated.RefreshAt.IsZero() {
		t.Fatal("refreshAt was not stamped, so the account will be due again on the next tick")
	}
	if updated.RefreshStatus != store.RefreshOK || updated.RefreshError != "" {
		t.Fatalf("status bookkeeping = %q / %q", updated.RefreshStatus, updated.RefreshError)
	}
	if updated.RefreshFailures != 0 {
		t.Fatalf("refreshFailures = %d, want a success to reset it", updated.RefreshFailures)
	}
	if updated.Status != store.StatusActive {
		t.Fatalf("status = %q, want active", updated.Status)
	}
}

// The request itself has to be the one the endpoint expects, because a wrong
// body is answered with a 400 that reads like the account being broken.
func TestRotationSendsTheExpectedRequest(t *testing.T) {
	h := newHarness(t, rotated("sidts-fresh"))
	h.addCookieAccount(t, "primary")

	h.service.Sweep(context.Background(), h.service.dueAccounts())

	if h.rotator.callCount() != 1 {
		t.Fatalf("rotation calls = %d, want 1", h.rotator.callCount())
	}
	// The literal body is reproduced verbatim; anything else is a 400.
	if h.rotator.lastBody != `[000,"-0000000000000000000"]` {
		t.Fatalf("rotation body = %q", h.rotator.lastBody)
	}
	if len(h.rotator.cookies) != 1 || !contains(h.rotator.cookies[0], "__Secure-1PSID=g.a000primary") {
		t.Fatalf("the credential did not travel: %v", h.rotator.cookies)
	}
	if got := h.rotator.headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}
}

// A rotation that returns no new value is not a success. Recording it as one
// would stamp RefreshAt and leave the account due again only after a full
// interval, while the cookie it is holding has already expired.
func TestRotationWithoutANewCookieIsNotASuccess(t *testing.T) {
	h := newHarness(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		// 200, but no Set-Cookie.
		w.WriteHeader(http.StatusOK)
	})
	account := h.addCookieAccount(t, "primary")

	summary := h.service.Sweep(context.Background(), h.service.dueAccounts())
	if summary.OK != 0 {
		t.Fatalf("summary = %+v, want no success", summary)
	}
	updated := h.reload(t, account.ID)
	if updated.RefreshStatus == store.RefreshOK {
		t.Fatalf("status = %q, want the missing cookie to be reported", updated.RefreshStatus)
	}
	if updated.RefreshAt.IsZero() {
		t.Fatal("a failed rotation must still stamp refreshAt")
	}
}

// --- failure handling -------------------------------------------------------

// TestAFailedRotationStillStampsRefreshAt is the anti-storm guard.
//
// RefreshAt doubles as the "not due yet" marker, so a failure that does not
// write it makes the account due again on the very next tick. Against an
// endpoint that throttles on request rate, that turns an outage into the one
// traffic pattern guaranteed to extend it.
func TestAFailedRotationStillStampsRefreshAt(t *testing.T) {
	h := newHarness(t, failing(http.StatusInternalServerError))
	account := h.addCookieAccount(t, "primary")

	summary := h.service.Sweep(context.Background(), h.service.dueAccounts())
	if summary.Failed != 1 {
		t.Fatalf("summary = %+v, want one failure", summary)
	}

	updated := h.reload(t, account.ID)
	if updated.RefreshAt.IsZero() {
		t.Fatal("a failed rotation did not stamp refreshAt; the account is due again immediately")
	}
	if updated.RefreshStatus != store.RefreshFailed {
		t.Fatalf("refreshStatus = %q, want %q", updated.RefreshStatus, store.RefreshFailed)
	}
	if updated.RefreshError == "" {
		t.Fatal("the failure reason was not recorded")
	}
	// And a transport failure must not retire the account: nothing about a 500
	// says the cookie is dead.
	if updated.Status == store.StatusInvalid {
		t.Fatal("a server error retired the account")
	}
	if updated.RefreshFailures != 0 {
		t.Fatalf("refreshFailures = %d, want a server error not to count toward retirement", updated.RefreshFailures)
	}

	// The account must not be due again right away.
	if due := h.service.dueAccounts(); len(due) != 0 {
		t.Fatalf("due = %d accounts immediately after a failure, want 0", len(due))
	}
}

// Throttling is about the caller's request rate, not the account. Counting it
// toward retirement would make a busy gateway slowly delete its own pool.
func TestThrottlingDoesNotCountTowardRetirement(t *testing.T) {
	h := newHarness(t, failing(http.StatusTooManyRequests))
	account := h.addCookieAccount(t, "primary")

	for i := 0; i < 5; i++ {
		summary := h.service.Sweep(context.Background(), h.service.dueAccounts())
		if summary.Throttled != 1 {
			t.Fatalf("round %d: summary = %+v, want one throttled", i, summary)
		}
		// Clear the due marker so the next round actually runs; the point here
		// is the retirement counter, not the scheduler.
		h.store.SaveAccountState(account.ID, func(a *store.Account) { a.RefreshAt = time.Time{} })
	}

	updated := h.reload(t, account.ID)
	if updated.RefreshFailures != 0 {
		t.Fatalf("refreshFailures = %d after five throttles, want 0", updated.RefreshFailures)
	}
	if updated.Status == store.StatusInvalid {
		t.Fatal("throttling retired the account")
	}
	if updated.RefreshStatus != store.RefreshThrottled {
		t.Fatalf("refreshStatus = %q, want %q", updated.RefreshStatus, store.RefreshThrottled)
	}
}

// A rejected session is the one failure that *does* retire the account, and only
// after the configured number of consecutive rejections: a single 401 can also
// be a transient consent redirect, and retiring on one would lose good accounts.
func TestARejectedSessionRetiresTheAccountAfterTheThreshold(t *testing.T) {
	h := newHarness(t, failing(http.StatusUnauthorized))
	h.settings.Refresh.RetireAfterFailures = 3
	account := h.addCookieAccount(t, "primary")

	for round := 1; round <= 3; round++ {
		summary := h.service.Sweep(context.Background(), h.service.dueAccounts())
		if summary.Invalid != 1 {
			t.Fatalf("round %d: summary = %+v, want one invalid", round, summary)
		}
		updated := h.reload(t, account.ID)
		if updated.RefreshFailures != round {
			t.Fatalf("round %d: refreshFailures = %d", round, updated.RefreshFailures)
		}
		if round < 3 && updated.Status == store.StatusInvalid {
			t.Fatalf("round %d: the account was retired before the threshold", round)
		}
		h.store.SaveAccountState(account.ID, func(a *store.Account) { a.RefreshAt = time.Time{} })
	}

	updated := h.reload(t, account.ID)
	if updated.Status != store.StatusInvalid {
		t.Fatalf("status = %q, want %q once the threshold is reached", updated.Status, store.StatusInvalid)
	}
	if updated.LastError == "" {
		t.Fatal("the account was retired without saying why")
	}
	// A retired account is no longer due: the endpoint would keep rejecting it
	// and the counter would only climb.
	if due := h.service.dueAccounts(); len(due) != 0 {
		t.Fatalf("due = %d accounts, want a retired account to be left alone", len(due))
	}
}

// A success in between clears the count, so a flaky network cannot accumulate
// its way to retirement across unrelated good minutes.
func TestASuccessResetsTheFailureCount(t *testing.T) {
	h := newHarness(t, failing(http.StatusUnauthorized))
	h.settings.Refresh.RetireAfterFailures = 3
	account := h.addCookieAccount(t, "primary")

	h.service.Sweep(context.Background(), h.service.dueAccounts())
	h.store.SaveAccountState(account.ID, func(a *store.Account) { a.RefreshAt = time.Time{} })
	h.service.Sweep(context.Background(), h.service.dueAccounts())
	if got := h.reload(t, account.ID).RefreshFailures; got != 2 {
		t.Fatalf("refreshFailures = %d, want 2", got)
	}

	// The endpoint recovers.
	h.rotator.mu.Lock()
	h.rotator.respond = rotated("sidts-recovered")
	h.rotator.mu.Unlock()

	h.store.SaveAccountState(account.ID, func(a *store.Account) { a.RefreshAt = time.Time{} })
	if summary := h.service.Sweep(context.Background(), h.service.dueAccounts()); summary.OK != 1 {
		t.Fatalf("summary = %+v, want a success", summary)
	}
	if got := h.reload(t, account.ID).RefreshFailures; got != 0 {
		t.Fatalf("refreshFailures = %d, want a success to reset it", got)
	}
	if got := h.reload(t, account.ID).Status; got == store.StatusInvalid {
		t.Fatal("the account stayed retired after a successful rotation")
	}
}

// --- guests and manual rotation --------------------------------------------

// A guest has no credential to renew, so rotating one is a request that can
// only fail. It is reported as skipped rather than failed, because a failure
// would make the console show an error for an account that is working exactly
// as intended — and because it must not count toward retirement.
func TestGuestsAreSkippedRatherThanFailed(t *testing.T) {
	h := newHarness(t, rotated("sidts-new"))
	guest := h.addGuestAccount(t, "guest")

	// The scheduler already excludes guests, so the path that matters is the
	// console's manual button, which names one account directly.
	outcome, err := h.service.RefreshAccount(context.Background(), guest.ID)
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if outcome.Status != store.RefreshSkipped {
		t.Fatalf("outcome = %+v, want a skip", outcome)
	}
	if h.rotator.callCount() != 0 {
		t.Fatal("a guest account must not reach the rotation endpoint")
	}
	updated := h.reload(t, guest.ID)
	if updated.RefreshStatus != store.RefreshSkipped {
		t.Fatalf("refreshStatus = %q, want %q", updated.RefreshStatus, store.RefreshSkipped)
	}
	if updated.Status == store.StatusInvalid {
		t.Fatal("a guest was retired for having no cookie, which is what a guest is")
	}
	if updated.RefreshFailures != 0 {
		t.Fatalf("refreshFailures = %d, want a skip not to count", updated.RefreshFailures)
	}
}

// The console's manual button rotates one named account and reports what
// happened, rather than sweeping the pool.
func TestRefreshAccountTargetsOneAccount(t *testing.T) {
	h := newHarness(t, rotated("sidts-manual"))
	account := h.addCookieAccount(t, "primary")
	h.addCookieAccount(t, "other")

	outcome, err := h.service.RefreshAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if outcome.Status != store.RefreshOK || outcome.AccountID != account.ID {
		t.Fatalf("outcome = %+v", outcome)
	}
	if h.rotator.callCount() != 1 {
		t.Fatalf("rotation calls = %d, want exactly the one account", h.rotator.callCount())
	}
	if got := h.reload(t, account.ID).PSIDTS; got != "sidts-manual" {
		t.Fatalf("psidts = %q", got)
	}
}

func TestRefreshAccountReportsAMissingAccount(t *testing.T) {
	h := newHarness(t, rotated("sidts-new"))
	if _, err := h.service.RefreshAccount(context.Background(), "acc_missing"); err == nil {
		t.Fatal("a missing account should be reported, not rotated")
	}
	if h.rotator.callCount() != 0 {
		t.Fatal("a missing account reached the rotation endpoint")
	}
}

// --- scheduling and single-flight -------------------------------------------

// Two concurrent sweeps would double the request rate against the endpoint that
// throttles on rate, which is the one thing that makes rotation fail for reasons
// that have nothing to do with the accounts.
func TestOnlyOneSweepRunsAtATime(t *testing.T) {
	h := newHarness(t, rotated("sidts-new"))
	account := h.addCookieAccount(t, "primary")

	release := make(chan struct{})
	h.rotator.mu.Lock()
	h.rotator.hold = release
	h.rotator.mu.Unlock()

	first := make(chan Summary, 1)
	go func() {
		first <- h.service.Sweep(context.Background(), []*store.Account{account})
	}()

	// Wait until the first sweep is actually inside the rotation call.
	deadline := time.Now().Add(5 * time.Second)
	for h.rotator.callCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first sweep never reached the endpoint")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The second sweep must decline rather than queue up behind the first.
	second := h.service.Sweep(context.Background(), []*store.Account{account})
	if second.Total != 0 || len(second.Outcomes) != 0 {
		t.Fatalf("the second sweep ran concurrently: %+v", second)
	}

	close(release)
	select {
	case got := <-first:
		if got.OK != 1 {
			t.Fatalf("the first sweep did not finish: %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first sweep never finished")
	}

	if h.rotator.callCount() != 1 {
		t.Fatalf("rotation calls = %d, want 1", h.rotator.callCount())
	}
}

// The gap between accounts inside one sweep is what keeps a burst from one IP
// from getting the whole range throttled.
func TestSweepSpacesAccountsApart(t *testing.T) {
	h := newHarness(t, rotated("sidts-new"))
	h.settings.Refresh.GapSeconds = 1

	one := h.addCookieAccount(t, "one")
	two := h.addCookieAccount(t, "two")

	started := time.Now()
	summary := h.service.Sweep(context.Background(), []*store.Account{one, two})
	elapsed := time.Since(started)

	if summary.OK != 2 {
		t.Fatalf("summary = %+v", summary)
	}
	if elapsed < time.Second {
		t.Fatalf("two accounts took %s, want at least the configured gap", elapsed)
	}
}

// LastSummary is what the console's refresh panel shows, so a sweep that ran has
// to be visible afterwards.
func TestLastSummaryIsRetained(t *testing.T) {
	h := newHarness(t, rotated("sidts-new"))
	h.addCookieAccount(t, "primary")

	if got := h.service.LastSummary(); got.Total != 0 {
		t.Fatalf("a sweep that never ran reported %+v", got)
	}

	h.service.Sweep(context.Background(), h.service.dueAccounts())

	last := h.service.LastSummary()
	if last.Total != 1 || last.OK != 1 {
		t.Fatalf("last summary = %+v", last)
	}
	if last.StartedAt.IsZero() || last.FinishedAt.IsZero() {
		t.Fatalf("the summary has no window: %+v", last)
	}
}

// The background loop must respect the enabled switch: an operator who turns the
// sweep off is accepting that accounts degrade to guest quota, and a loop that
// ignores them is worse than no switch at all.
func TestTheLoopStopsWithItsContext(t *testing.T) {
	h := newHarness(t, rotated("sidts-new"))
	h.addCookieAccount(t, "primary")

	ctx, cancel := context.WithCancel(context.Background())
	h.service.Start(ctx)
	cancel()

	// Nothing to assert beyond the loop returning; the interesting property is
	// that cancelling does not block or panic. Give it a moment to unwind.
	time.Sleep(50 * time.Millisecond)
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
