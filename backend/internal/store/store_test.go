package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"geminiweb2api/internal/config"
	"geminiweb2api/internal/gemini"
)

// testCookie is a plausible paste: the two cookies that actually matter, in the
// order a browser would copy them.
const testCookie = "__Secure-1PSID=g.a000testpsidvalue0000000000; __Secure-1PSIDTS=sidts-test-value"

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir(), "admin", "admin12345")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newTestAccount builds the smallest account the store will accept. The cookie
// carries the PSID, and the store derives the identity fields from it.
func newTestAccount() *Account {
	return &Account{Name: "probe", Kind: KindCookie, Cookie: testCookie, Enabled: true}
}

// TestSaveAccountStateCallbackReadsSettings is a regression guard for a total
// service deadlock. Mutation callbacks run while the write lock is held, and
// production callbacks (admin.syncQuota, pool.release) read settings from
// inside them. Go's sync.RWMutex is not reentrant, so if Settings() took the
// read lock the goroutine would wait on itself forever and every subsequent
// request would block behind the leaked write lock.
func TestSaveAccountStateCallbackReadsSettings(t *testing.T) {
	st := newTestStore(t)

	account := newTestAccount()
	if err := st.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		st.SaveAccountState(account.ID, func(target *Account) {
			// Mirrors admin.syncQuota: read settings from within the callback.
			target.CooldownUntil = time.Now().Add(st.Settings().CooldownBase())
			target.Status = StatusCooldown
		})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("SaveAccountState deadlocked: callback could not read Settings")
	}

	updated, ok := st.AccountByID(account.ID)
	if !ok {
		t.Fatal("account disappeared")
	}
	if updated.Status != StatusCooldown {
		t.Fatalf("callback mutation was not applied: status=%q", updated.Status)
	}
	if updated.CooldownUntil.IsZero() {
		t.Fatal("callback did not set CooldownUntil")
	}
}

// TestSettingsReadableUnderWriteLock covers every mutation helper that runs a
// caller supplied callback under the write lock.
func TestSettingsReadableUnderWriteLock(t *testing.T) {
	st := newTestStore(t)

	account := newTestAccount()
	if err := st.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	key, err := st.CreateClientKey("probe-key", 10, 2)
	if err != nil {
		t.Fatalf("create client key: %v", err)
	}

	cases := []struct {
		name string
		run  func()
	}{
		{"UpdateAccount", func() {
			if _, err := st.UpdateAccount(account.ID, func(a *Account) {
				a.Priority = 1 + int(st.Settings().Routing.CooldownBaseSec%9)
			}); err != nil {
				t.Errorf("UpdateAccount: %v", err)
			}
		}},
		{"UpdateAccounts", func() {
			if _, err := st.UpdateAccounts([]string{account.ID}, func(a *Account) {
				a.MaxConcurrent = 1 + int(st.Settings().Routing.MaxAttempts)
			}); err != nil {
				t.Errorf("UpdateAccounts: %v", err)
			}
		}},
		{"UpdateClientKey", func() {
			if _, err := st.UpdateClientKey(key.ID, func(k *ClientKey) {
				k.RPMLimit = 30 + st.Settings().Audit.RetentionDays
			}); err != nil {
				t.Errorf("UpdateClientKey: %v", err)
			}
		}},
		{"UpdateModel", func() {
			models := st.ListModels()
			if len(models) == 0 {
				return
			}
			if _, err := st.UpdateModel(models[0].ID, func(m *ModelConfig) {
				m.Description = "probe-" + st.Settings().Routing.Strategy
			}); err != nil {
				t.Errorf("UpdateModel: %v", err)
			}
		}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.run()
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s deadlocked: callback could not read Settings", tc.name)
			}
		})
	}
}

// TestSettingsSnapshotFollowsUpdate makes sure the lock-free snapshot stays in
// sync with the persisted state after a settings change.
func TestSettingsSnapshotFollowsUpdate(t *testing.T) {
	st := newTestStore(t)

	if err := st.UpdateSettings(func(s *config.Settings) {
		s.Routing.CooldownBaseSec = 123
		s.Audit.RetentionDays = 11
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if got := st.Settings().Routing.CooldownBaseSec; got != 123 {
		t.Fatalf("snapshot not refreshed: cooldownBaseSec=%d", got)
	}
	if got := st.Settings().CooldownBase(); got != 123*time.Second {
		t.Fatalf("derived duration wrong: %v", got)
	}

	// A fresh store reading the same directory must observe the persisted value.
	// Persistence is debounced, so force a flush before reopening.
	if err := st.Save(); err != nil {
		t.Fatalf("flush store: %v", err)
	}
	reopened, err := Open(st.DataDir(), "admin", "admin12345")
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Settings().Routing.CooldownBaseSec; got != 123 {
		t.Fatalf("persisted settings not reloaded: %d", got)
	}
}

// TestConcurrentSettingsReadsAndWrites exercises the atomic snapshot under the
// race detector.
func TestConcurrentSettingsReadsAndWrites(t *testing.T) {
	st := newTestStore(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = st.Settings().CooldownBase()
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 50; i++ {
			if err := st.UpdateSettings(func(s *config.Settings) {
				s.Routing.CooldownBaseSec = 10 + i
			}); err != nil {
				t.Errorf("update settings: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}

// ListAccounts and AccountByID must hand out detached copies. The pool reads
// account fields outside of any lock, so returning pointers into the live state
// would race with SaveAccountState mutating those same objects.
func TestListAccountsReturnsDetachedSnapshot(t *testing.T) {
	st := newTestStore(t)

	account := newTestAccount()
	account.ID = "acc_1"
	if err := st.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	snapshot := st.ListAccounts()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot length = %d, want 1", len(snapshot))
	}

	// Mutating the snapshot must not reach the store.
	snapshot[0].Status = "tampered"
	snapshot[0].Cookie = "tampered"
	snapshot[0].PSIDTS = "tampered"
	snapshot[0].Priority = 999
	snapshot[0].Quota = &Quota{Note: "tampered"}

	live, ok := st.AccountByID("acc_1")
	if !ok {
		t.Fatal("account missing")
	}
	if live.Status == "tampered" || live.Cookie == "tampered" || live.PSIDTS == "tampered" || live.Priority == 999 {
		t.Fatalf("snapshot mutation leaked into the store: %+v", live)
	}
	if live.Quota != nil && live.Quota.Note == "tampered" {
		t.Fatal("nested quota pointer was shared with the snapshot")
	}

	// And the reverse: a store mutation must not rewrite an earlier snapshot.
	before := st.ListAccounts()[0]
	st.SaveAccountState("acc_1", func(a *Account) {
		a.Status = StatusInvalid
		a.FailCount = 7
	})
	if before.Status == StatusInvalid || before.FailCount == 7 {
		t.Fatal("store mutation rewrote an already returned snapshot")
	}
}

func TestAccountByIDReturnsDetachedCopy(t *testing.T) {
	st := newTestStore(t)

	account := newTestAccount()
	account.ID = "acc_1"
	if err := st.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	first, ok := st.AccountByID("acc_1")
	if !ok {
		t.Fatal("account missing")
	}
	second, ok := st.AccountByID("acc_1")
	if !ok {
		t.Fatal("account missing on second read")
	}
	if first == second {
		t.Fatal("AccountByID returned the same pointer twice; callers could race on it")
	}

	if _, ok := st.AccountByID("nope"); ok {
		t.Fatal("unknown id reported as found")
	}
}

// --- cookie-derived identity ------------------------------------------------

// TestAddAccountDerivesIdentityFromCookie pins the invariant the whole pool
// rests on: a pasted cookie is enough to build a working account.
//
// The PSID is what the console displays, what the session cache is keyed on and
// what duplicate detection compares, so if AddAccount ever stopped filling it
// in, the failure would be silent — accounts would be added, listed and probed,
// and the pool would treat every one of them as unroutable.
func TestAddAccountDerivesIdentityFromCookie(t *testing.T) {
	st := newTestStore(t)

	account := newTestAccount()
	if err := st.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	stored, ok := st.AccountByID(account.ID)
	if !ok {
		t.Fatal("account missing after add")
	}
	if stored.PSID != "g.a000testpsidvalue0000000000" {
		t.Fatalf("psid = %q, want it read out of the cookie", stored.PSID)
	}
	if stored.PSIDTS != "sidts-test-value" {
		t.Fatalf("psidts = %q, want it read out of the cookie", stored.PSIDTS)
	}
	if stored.Identifier == "" {
		t.Fatal("identifier was left empty; the console has nothing to show")
	}
	if stored.Status != StatusActive {
		t.Fatalf("status = %q, want %q for a freshly added account", stored.Status, StatusActive)
	}
	if stored.Kind != KindCookie {
		t.Fatalf("kind = %q, want %q", stored.Kind, KindCookie)
	}
}

// TestAddAccountRejectsDuplicatePSID covers the rotation case specifically.
//
// Duplicate detection compares the PSID rather than the whole cookie string,
// because the PSIDTS half is rewritten by the refresher every few minutes. A
// check on the full string would therefore let the same account be added twice
// the moment its first rotation landed, and the pool would then route to two
// entries that share one upstream identity and fight over one quota.
func TestAddAccountRejectsDuplicatePSID(t *testing.T) {
	st := newTestStore(t)

	first := newTestAccount()
	if err := st.AddAccount(first); err != nil {
		t.Fatalf("add first: %v", err)
	}

	// Same PSID, rotated PSIDTS: still the same account.
	second := newTestAccount()
	second.Name = "rotated"
	second.Cookie = "__Secure-1PSID=g.a000testpsidvalue0000000000; __Secure-1PSIDTS=a-different-sidts"
	if err := st.AddAccount(second); err == nil {
		t.Fatal("a rotated copy of an existing account was accepted as a new one")
	}

	if got := len(st.ListAccounts()); got != 1 {
		t.Fatalf("account count = %d, want 1", got)
	}
}

// --- repairing a settings file an older build wrote -------------------------

// TestOpeningARepairedSettingsFileWritesTheRepairBack covers the half of a
// settings migration that is easy to leave out.
//
// Normalize fixes the value in memory, which is enough to make the process work
// — but the file keeps the old value, so the file describes a configuration the
// process is not using. That disagreement is not cosmetic: it is precisely how a
// broken default outlives a fix to the default, because the next reader sees a
// perfectly ordinary-looking path and concludes the fix did not apply.
func TestOpeningARepairedSettingsFileWritesTheRepairBack(t *testing.T) {
	dir := t.TempDir()

	// Seed a settings file the way an earlier build would have left it: the
	// whole default set written out, including the one value that cannot work.
	//
	// The rest of the state is seeded too, and that part is not decoration. An
	// empty admin makes Open write a snapshot of its own while creating the
	// account, which would carry the repaired settings to disk by accident and
	// let this test pass with the write-back removed.
	seeded := config.DefaultSettings(dir)
	seeded.Upstream.BaseURL = "https://gemini.google.com/app"
	raw, err := json.Marshal(State{
		Version:  schemaVersion,
		Admin:    Admin{Username: "admin", Salt: "seeded", PasswordHash: "seeded"},
		Settings: seeded,
		Models:   BuiltinModels(),
	})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), raw, 0o644); err != nil {
		t.Fatalf("seed %s: %v", stateFile, err)
	}

	st, err := Open(dir, "admin", "admin12345")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// The live settings are repaired...
	want := config.DefaultSettings(dir).Upstream.BaseURL
	live := st.Settings()
	if live.Upstream.BaseURL != want {
		t.Errorf("live base url = %q, want %q", live.Upstream.BaseURL, want)
	}

	// ...and so is the file, which is what this test is about. The write is
	// queued rather than immediate, so give the loop a moment.
	var onDisk config.Settings
	deadline := time.Now().Add(3 * time.Second)
	for {
		content, readErr := os.ReadFile(filepath.Join(dir, stateFile))
		if readErr == nil {
			var got State
			if json.Unmarshal(content, &got) == nil {
				onDisk = got.Settings
				if onDisk.Upstream.BaseURL == live.Upstream.BaseURL {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the repair never reached %s: baseURL=%q", stateFile, onDisk.Upstream.BaseURL)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- the view projection ----------------------------------------------------

// TestAccountViewNeverSerialisesTheCookie is a leak guard. AccountView is an
// explicit projection precisely so that a raw cookie cannot reach an API
// response, and the only way to keep that promise is to assert it.
func TestAccountViewNeverSerialisesTheCookie(t *testing.T) {
	account := newTestAccount()
	account.ID = "acc_1"
	account.PSID = "g.a000testpsidvalue0000000000"
	account.PSIDTS = "sidts-test-value"

	raw, err := json.Marshal(NewAccountView(account, 2))
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	body := string(raw)

	if contains(body, "sidts-test-value") {
		t.Fatalf("the raw cookie leaked into the API view: %s", body)
	}
	if !contains(body, "hasPsidts") {
		t.Fatalf("hasPsidts is missing; the console cannot tell a live cookie from a dead one: %s", body)
	}

	// Masking is the only rendering of the credential an operator sees, so it
	// has to be present and it has to be partial.
	var view AccountView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}
	if view.CookieMasked == "" {
		t.Fatal("cookieMasked is empty")
	}
	if contains(view.CookieMasked, "testpsidvalue0000000000") {
		t.Fatalf("cookieMasked exposes the whole PSID: %q", view.CookieMasked)
	}
	if len(view.CookieNames) != 2 {
		t.Fatalf("cookieNames = %v, want both cookies named", view.CookieNames)
	}
	if view.Inflight != 2 {
		t.Fatalf("inflight = %d, want 2", view.Inflight)
	}
}

// TestBuiltinModelsMatchTheClientCatalogue pins the store's catalogue to the
// one the client can actually resolve.
//
// The store list is derived from gemini.BuiltinModels, so this is really a
// guard against the derivation being replaced by a hand-written list again —
// which is exactly what had happened, and had already lost two entries.
func TestBuiltinModelsMatchTheClientCatalogue(t *testing.T) {
	models := BuiltinModels()
	specs := gemini.BuiltinModels()
	if len(models) != len(specs) {
		t.Fatalf("catalogue length = %d, client has %d", len(models), len(specs))
	}
	for i, model := range models {
		if model.ID != specs[i].ID {
			t.Fatalf("entry %d: id = %q, client has %q", i, model.ID, specs[i].ID)
		}
		if _, ok := gemini.LookupModel(model.ID); !ok {
			t.Fatalf("model %q is listed but the client cannot resolve it", model.ID)
		}
		if model.Name == "" || model.Type == "" {
			t.Fatalf("incomplete catalogue entry: %+v", model)
		}
		if !model.Enabled || !model.Builtin {
			t.Fatalf("builtin model %q is not enabled", model.ID)
		}
	}
	// The default has to be present, or an unnamed request resolves to a model
	// the console does not list.
	found := false
	for _, model := range models {
		if model.ID == gemini.DefaultModelID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the default model %q is missing from the catalogue", gemini.DefaultModelID)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
