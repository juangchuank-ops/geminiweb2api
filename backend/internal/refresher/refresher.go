// Package refresher keeps pooled cookies alive.
//
// Gemini's web session rests on two cookies with very different lifetimes:
// __Secure-1PSID is long-lived, while __Secure-1PSIDTS expires within hours.
// When PSIDTS lapses, requests do not start failing — they start being answered
// as a *guest*. The status code stays 200, the answer still arrives, and the
// only visible difference is a much smaller quota and a model tier the account
// did not ask for. Nothing in a response says "your cookie expired".
//
// So the sweep is not an optimisation, it is the thing that keeps a pool
// honest, and it runs on a schedule rather than on demand because the failure
// it prevents is silent.
package refresher

import (
	"context"
	"log"
	"sync"
	"time"

	"geminiweb2api/internal/config"
	"geminiweb2api/internal/gemini"
	"geminiweb2api/internal/store"
)

// Outcome is what happened to one account in one sweep.
type Outcome struct {
	AccountID   string    `json:"accountId"`
	AccountName string    `json:"accountName"`
	Status      string    `json:"status"`
	Error       string    `json:"error"`
	At          time.Time `json:"at"`
}

// Summary is the result of a sweep.
type Summary struct {
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Total      int       `json:"total"`
	OK         int       `json:"ok"`
	Failed     int       `json:"failed"`
	Throttled  int       `json:"throttled"`
	Skipped    int       `json:"skipped"`
	Invalid    int       `json:"invalid"`
	Outcomes   []Outcome `json:"outcomes"`
}

// Service runs the cookie sweep.
type Service struct {
	store        *store.Store
	client       *gemini.Client
	settings     func() config.Settings
	credentialOf func(*store.Account) gemini.Credential

	mu       sync.Mutex
	sweeping bool
	last     Summary
}

// New builds the service. credentialOf is injected rather than imported from
// the gateway so this package does not depend on the HTTP layer.
func New(st *store.Store, client *gemini.Client, settingsFn func() config.Settings, credentialOf func(*store.Account) gemini.Credential) *Service {
	return &Service{
		store:        st,
		client:       client,
		settings:     settingsFn,
		credentialOf: credentialOf,
	}
}

// Start launches the background sweep and returns immediately. It stops when
// ctx is cancelled.
func (s *Service) Start(ctx context.Context) {
	go s.loop(ctx)
}

// loop wakes periodically and rotates whatever is due.
//
// The tick is short relative to the rotation interval on purpose: the interval
// is enforced per account from its own RefreshAt, so a process that was down
// for an hour resumes promptly instead of waiting out a full period from
// startup. The tick only decides how quickly "due" is noticed.
func (s *Service) loop(ctx context.Context) {
	const tick = 30 * time.Second

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		if s.settings().Refresh.Enabled {
			if due := s.dueAccounts(); len(due) > 0 {
				s.Sweep(ctx, due)
			}
		}

		timer.Reset(tick)
	}
}

// dueAccounts lists the accounts whose rotation interval has elapsed.
func (s *Service) dueAccounts() []*store.Account {
	settings := s.settings()
	interval := settings.RefreshInterval()
	now := time.Now()

	var due []*store.Account
	for _, account := range s.store.ListAccounts() {
		if !account.Enabled || account.Kind == store.KindGuest || account.Cookie == "" {
			continue
		}
		// A retired account is not worth a rotation call: the endpoint would
		// reject it and the count would only keep climbing.
		if account.Status == store.StatusInvalid {
			continue
		}
		if account.RefreshAt.IsZero() || now.Sub(account.RefreshAt) >= interval {
			due = append(due, account)
		}
	}
	return due
}

// Sweep rotates the given accounts, spacing them out.
//
// Only one sweep runs at a time. Two concurrent sweeps would double the request
// rate against an endpoint that throttles on rate, which is the one thing that
// makes rotation fail for reasons that have nothing to do with the accounts.
func (s *Service) Sweep(ctx context.Context, accounts []*store.Account) Summary {
	s.mu.Lock()
	if s.sweeping {
		s.mu.Unlock()
		return Summary{}
	}
	s.sweeping = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sweeping = false
		s.mu.Unlock()
	}()

	summary := Summary{StartedAt: time.Now(), Total: len(accounts)}
	gap := s.settings().RefreshGap()

	for i, account := range accounts {
		if ctx.Err() != nil {
			break
		}
		if i > 0 && gap > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(gap):
			}
		}

		outcome := s.refreshOne(ctx, account)
		summary.Outcomes = append(summary.Outcomes, outcome)
		switch outcome.Status {
		case store.RefreshOK:
			summary.OK++
		case store.RefreshThrottled:
			summary.Throttled++
		case store.RefreshSkipped:
			summary.Skipped++
		case store.RefreshInvalid:
			summary.Invalid++
		default:
			summary.Failed++
		}
	}

	summary.FinishedAt = time.Now()

	s.mu.Lock()
	s.last = summary
	s.mu.Unlock()

	if summary.OK > 0 || summary.Failed > 0 || summary.Invalid > 0 {
		log.Printf("Cookie 刷新完成: 成功 %d · 失败 %d · 限流 %d · 跳过 %d · 失效 %d",
			summary.OK, summary.Failed, summary.Throttled, summary.Skipped, summary.Invalid)
	}
	return summary
}

// LastSummary returns the most recent sweep, for the console.
func (s *Service) LastSummary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// RefreshAccount rotates one account by id, for the console's manual action.
func (s *Service) RefreshAccount(ctx context.Context, id string) (Outcome, error) {
	account, ok := s.store.AccountByID(id)
	if !ok {
		return Outcome{}, store.ErrNotFound
	}
	return s.refreshOne(ctx, account), nil
}

// refreshOne rotates a single account and records the outcome.
//
// The record is written on every path, including the failures, because
// RefreshAt doubles as the "not due yet" marker. Recording only successes would
// make an unreachable endpoint retry every tick, turning an outage into a
// request storm against the one endpoint that punishes request storms.
func (s *Service) refreshOne(ctx context.Context, account *store.Account) Outcome {
	outcome := Outcome{
		AccountID:   account.ID,
		AccountName: displayName(account),
		At:          time.Now(),
	}

	if account.Kind == store.KindGuest || account.Cookie == "" {
		outcome.Status = store.RefreshSkipped
		outcome.Error = "游客账号无需刷新"
		s.record(account.ID, store.RefreshSkipped, outcome.Error, false)
		return outcome
	}

	settings := s.settings()
	timeout := settings.RefreshTimeout()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	_, jar, err := s.client.RefreshCookie(callCtx, s.credentialOf(account))
	if err != nil {
		switch {
		case isInvalid(err):
			// The primary cookie is gone. Count it, and retire the account once
			// the count is convincing — a single 401 can also be a transient
			// consent redirect, and retiring on one would lose good accounts.
			outcome.Status = store.RefreshInvalid
			outcome.Error = err.Error()
			s.record(account.ID, store.RefreshInvalid, outcome.Error, true)
		case isThrottled(err):
			// Throttling is about the caller's rate, not the account. It must
			// not count toward retirement, or a busy gateway would slowly
			// delete its own pool.
			outcome.Status = store.RefreshThrottled
			outcome.Error = err.Error()
			s.record(account.ID, store.RefreshThrottled, outcome.Error, false)
		default:
			outcome.Status = store.RefreshFailed
			outcome.Error = err.Error()
			s.record(account.ID, store.RefreshFailed, outcome.Error, false)
		}
		return outcome
	}

	outcome.Status = store.RefreshOK
	s.applyRotation(account.ID, jar)
	return outcome
}

// applyRotation persists a successful rotation.
//
// Both the split-out PSIDTS and the raw cookie string are rewritten. They are
// two views of the same jar and only one of them is authoritative at request
// time (the split value wins), but leaving the raw string stale would make the
// console show a cookie that no longer matches what is being sent — the kind of
// disagreement that costs an hour the next time something breaks.
func (s *Service) applyRotation(id string, jar *gemini.CookieJar) {
	s.store.SaveAccountState(id, func(account *store.Account) {
		if jar != nil {
			if value := jar.Get(gemini.CookiePSIDTS); value != "" {
				account.PSIDTS = value
			}
			if rendered := jar.String(); rendered != "" {
				account.Cookie = rendered
			}
		}
		account.RefreshAt = time.Now()
		account.RefreshStatus = store.RefreshOK
		account.RefreshError = ""
		account.RefreshFailures = 0
	})
}

// record stores a failed attempt.
func (s *Service) record(id, status, message string, countFailure bool) {
	settings := s.settings()
	threshold := settings.Refresh.RetireAfterFailures
	if threshold <= 0 {
		threshold = 3
	}

	s.store.SaveAccountState(id, func(account *store.Account) {
		account.RefreshAt = time.Now()
		account.RefreshStatus = status
		account.RefreshError = message
		if countFailure {
			account.RefreshFailures++
			if account.RefreshFailures >= threshold && account.Status != store.StatusInvalid {
				// Retire rather than disable: disabled means "the operator
				// turned this off", invalid means "this credential is dead",
				// and only the second one should prompt a re-paste.
				account.Status = store.StatusInvalid
				account.LastError = "Cookie 已失效，需要重新登录获取"
			}
		}
	})
}

func isInvalid(err error) bool {
	return errorsIs(err, gemini.ErrInvalidCredential)
}

func isThrottled(err error) bool {
	return errorsIs(err, gemini.ErrRotateThrottled) || errorsIs(err, gemini.ErrRateLimited)
}

// errorsIs is errors.Is without importing errors at the call sites above.
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
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

func displayName(account *store.Account) string {
	if account.Name != "" {
		return account.Name
	}
	if account.Identifier != "" {
		return account.Identifier
	}
	return store.MaskCookie(account.Cookie)
}
