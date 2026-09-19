package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"geminiweb2api/internal/config"
	"geminiweb2api/internal/gateway"
	"geminiweb2api/internal/gemini"
	"geminiweb2api/internal/pool"
	"geminiweb2api/internal/refresher"
	"geminiweb2api/internal/store"
)

// API serves the admin console.
type API struct {
	store     *store.Store
	pool      *pool.Pool
	client    *gemini.Client
	refresher *refresher.Service
	settings  func() config.Settings
	started   time.Time
}

func New(st *store.Store, p *pool.Pool, client *gemini.Client, refreshSvc *refresher.Service, settings func() config.Settings) *API {
	return &API{store: st, pool: p, client: client, refresher: refreshSvc, settings: settings, started: time.Now()}
}

// Register installs every admin route on the mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/api/auth/login", a.login)
	mux.HandleFunc("GET /admin/api/auth/me", a.guard(a.me))
	mux.HandleFunc("POST /admin/api/auth/logout", a.guard(a.logout))
	mux.HandleFunc("POST /admin/api/auth/password", a.guard(a.changePassword))

	mux.HandleFunc("GET /admin/api/dashboard", a.guard(a.dashboard))

	mux.HandleFunc("GET /admin/api/accounts", a.guard(a.listAccounts))
	mux.HandleFunc("POST /admin/api/accounts", a.guard(a.createAccount))
	mux.HandleFunc("GET /admin/api/accounts/groups", a.guard(a.accountGroups))
	mux.HandleFunc("GET /admin/api/accounts/export", a.guard(a.exportAccounts))
	mux.HandleFunc("POST /admin/api/accounts/import", a.guard(a.importAccounts))
	mux.HandleFunc("POST /admin/api/accounts/batch", a.guard(a.batchAccounts))
	mux.HandleFunc("POST /admin/api/accounts/probe-all", a.guard(a.probeAll))
	mux.HandleFunc("POST /admin/api/accounts/refresh-all", a.guard(a.refreshAll))
	mux.HandleFunc("POST /admin/api/accounts/cleanup", a.guard(a.cleanupAccounts))
	mux.HandleFunc("PATCH /admin/api/accounts/{id}", a.guard(a.updateAccount))
	mux.HandleFunc("DELETE /admin/api/accounts/{id}", a.guard(a.deleteAccount))
	mux.HandleFunc("POST /admin/api/accounts/{id}/probe", a.guard(a.probeAccount))
	mux.HandleFunc("POST /admin/api/accounts/{id}/refresh", a.guard(a.refreshAccount))

	mux.HandleFunc("GET /admin/api/refresh", a.guard(a.refreshOverview))
	mux.HandleFunc("POST /admin/api/refresh/run", a.guard(a.refreshRun))

	mux.HandleFunc("GET /admin/api/client-keys", a.guard(a.listClientKeys))
	mux.HandleFunc("POST /admin/api/client-keys", a.guard(a.createClientKey))
	mux.HandleFunc("PATCH /admin/api/client-keys/{id}", a.guard(a.updateClientKey))
	mux.HandleFunc("DELETE /admin/api/client-keys/{id}", a.guard(a.deleteClientKey))

	mux.HandleFunc("GET /admin/api/models", a.guard(a.listModels))
	mux.HandleFunc("PATCH /admin/api/models/{id}", a.guard(a.updateModel))

	mux.HandleFunc("GET /admin/api/audits", a.guard(a.listAudits))
	mux.HandleFunc("DELETE /admin/api/audits", a.guard(a.clearAudits))
	mux.HandleFunc("GET /admin/api/audits/{id}", a.guard(a.auditDetail))

	mux.HandleFunc("GET /admin/api/settings", a.guard(a.getSettings))
	mux.HandleFunc("PUT /admin/api/settings", a.guard(a.saveSettings))

	mux.HandleFunc("GET /admin/api/gallery", a.guard(a.gallery))
}

// ---------------------------------------------------------------- middleware

type ctxKey string

const sessionKey ctxKey = "session"

func (a *API) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" || !a.store.ValidateSession(token) {
			writeError(w, http.StatusUnauthorized, "登录已过期，请重新登录")
			return
		}
		next(w, r)
	}
}

func bearer(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("authorization"))
	if len(raw) > 7 && strings.EqualFold(raw[:7], "bearer ") {
		return strings.TrimSpace(raw[7:])
	}
	return ""
}

// --------------------------------------------------------------------- auth

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var request loginRequest
	if err := decode(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if !a.store.VerifyPassword(strings.TrimSpace(request.Username), request.Password) {
		writeError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	token, _ := a.store.IssueSession()
	writeJSON(w, http.StatusOK, map[string]any{
		"token":    token,
		"username": request.Username,
		"role":     "admin",
	})
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	admin := a.store.AdminProfile()
	writeJSON(w, http.StatusOK, map[string]any{"username": admin.Username, "role": "admin"})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	a.store.RevokeSession(bearer(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) changePassword(w http.ResponseWriter, r *http.Request) {
	var request struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := decode(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	admin := a.store.AdminProfile()
	if !a.store.VerifyPassword(admin.Username, request.CurrentPassword) {
		writeError(w, http.StatusBadRequest, "当前密码不正确")
		return
	}
	if err := a.store.SetPassword(admin.Username, request.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.store.RevokeAllSessions()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- dashboard

func (a *API) dashboard(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	days := parsePeriodDays(period)
	timezone := r.URL.Query().Get("timezone")
	location := time.UTC
	if timezone != "" {
		if parsed, err := time.LoadLocation(timezone); err == nil {
			location = parsed
		}
	}

	settings := a.settings()
	now := time.Now()
	windowStart := now.AddDate(0, 0, -days)

	audits := a.store.ListAudits()
	accounts := a.store.ListAccounts()
	models := a.store.ListModels()
	keys := a.store.ListClientKeys()

	var (
		requests         int
		failures         int
		promptTokens     int
		completionTokens int
		reasoningTokens  int
		firstTokenSum    int64
		firstTokenCount  int
		latencySum       int64
		latencyCount     int
	)

	modelCounts := map[string]int{}
	modelTokens := map[string]int{}
	bucketRequests := map[string]int{}
	bucketFailures := map[string]int{}
	bucketTokens := map[string]int{}

	for _, audit := range audits {
		if audit.CreatedAt.Before(windowStart) {
			continue
		}
		requests++
		if audit.Status >= 400 {
			failures++
		}
		promptTokens += audit.PromptTokens
		completionTokens += audit.CompletionTokens
		if audit.FirstTokenMs > 0 {
			firstTokenSum += audit.FirstTokenMs
			firstTokenCount++
		}
		if audit.LatencyMs > 0 {
			latencySum += audit.LatencyMs
			latencyCount++
		}
		modelCounts[audit.Model]++
		modelTokens[audit.Model] += audit.PromptTokens + audit.CompletionTokens

		key := bucketKey(audit.CreatedAt.In(location), days)
		bucketRequests[key]++
		bucketTokens[key] += audit.PromptTokens + audit.CompletionTokens
		if audit.Status >= 400 {
			bucketFailures[key]++
		}
	}

	successRate := 0.0
	if requests > 0 {
		successRate = float64(requests-failures) / float64(requests) * 100
	}
	averageFirstToken := int64(0)
	if firstTokenCount > 0 {
		averageFirstToken = firstTokenSum / int64(firstTokenCount)
	}
	averageLatency := int64(0)
	if latencyCount > 0 {
		averageLatency = latencySum / int64(latencyCount)
	}

	type modelRow struct {
		Model    string `json:"model"`
		Requests int    `json:"requests"`
		Tokens   int    `json:"tokens"`
	}
	topModels := make([]modelRow, 0, len(modelCounts))
	for model, count := range modelCounts {
		topModels = append(topModels, modelRow{Model: model, Requests: count, Tokens: modelTokens[model]})
	}
	sort.Slice(topModels, func(i, j int) bool { return topModels[i].Requests > topModels[j].Requests })
	if len(topModels) > 8 {
		topModels = topModels[:8]
	}

	type trendRow struct {
		Bucket   string `json:"bucket"`
		Requests int    `json:"requests"`
		Failures int    `json:"failures"`
		Tokens   int    `json:"tokens"`
	}
	trend := make([]trendRow, 0, len(bucketRequests))
	for key, count := range bucketRequests {
		trend = append(trend, trendRow{Bucket: key, Requests: count, Failures: bucketFailures[key], Tokens: bucketTokens[key]})
	}
	sort.Slice(trend, func(i, j int) bool { return trend[i].Bucket < trend[j].Bucket })

	// routable counts accounts the scheduler can actually use right now. It is
	// not the same as active: an account whose cooldown has already elapsed is
	// still labelled cooldown but is perfectly schedulable, and reporting only
	// the status count made a healthy pool look dead.
	_, active, cooldown, disabled, invalid, routable := a.pool.Summary()
	enabledModels := 0
	for _, model := range models {
		if model.Enabled {
			enabledModels++
		}
	}
	activeKeys := 0
	for _, key := range keys {
		if key.Enabled {
			activeKeys++
		}
	}

	type activityRow struct {
		ID        string    `json:"id"`
		Time      time.Time `json:"time"`
		Model     string    `json:"model"`
		Status    int       `json:"status"`
		LatencyMs int64     `json:"latencyMs"`
		Account   string    `json:"account"`
	}
	activity := make([]activityRow, 0, 8)
	for _, audit := range audits {
		if len(activity) == 8 {
			break
		}
		activity = append(activity, activityRow{
			ID: audit.ID, Time: audit.CreatedAt, Model: audit.Model,
			Status: audit.Status, LatencyMs: audit.LatencyMs, Account: audit.AccountName,
		})
	}

	type distributionRow struct {
		Type  string `json:"type"`
		Count int    `json:"count"`
	}
	kindCounts := map[string]int{}
	for _, account := range accounts {
		kind := account.Kind
		if kind == "" {
			kind = store.KindCookie
		}
		kindCounts[kind]++
	}
	distribution := make([]distributionRow, 0, len(kindCounts))
	for kind, count := range kindCounts {
		label := "Cookie"
		if kind == store.KindGuest {
			label = "游客"
		}
		distribution = append(distribution, distributionRow{Type: label, Count: count})
	}
	sort.Slice(distribution, func(i, j int) bool { return distribution[i].Count > distribution[j].Count })

	writeJSON(w, http.StatusOK, map[string]any{
		"usage": map[string]any{
			"requests":                     requests,
			"failedRequests":               failures,
			"successRate":                  successRate,
			"inputTokens":                  promptTokens,
			"outputTokens":                 completionTokens,
			"reasoningTokens":              reasoningTokens,
			"tokens":                       promptTokens + completionTokens,
			"averageFirstTokenMs":          averageFirstToken,
			"firstTokenSamples":            firstTokenCount,
			"averageLatencyMs":             averageLatency,
			"averageOutputTokensPerSecond": 0,
			"throughputSamples":            0,
			"estimatedCostUsd":             0,
		},
		"resources": map[string]any{
			"totalAccounts":    len(accounts),
			"activeAccounts":   active,
			"routableAccounts": routable,
			"cooldownAccounts": cooldown,
			"disabledAccounts": disabled,
			"invalidAccounts":  invalid,
			"totalModels":      len(models),
			"enabledModels":    enabledModels,
			"totalClientKeys":  len(keys),
			"activeClientKeys": activeKeys,
		},
		"trend":        trend,
		"topModels":    topModels,
		"distribution": distribution,
		"activity":     activity,
		"upstream": map[string]any{
			"baseURL":   settings.Upstream.BaseURL,
			"poolTotal": len(accounts),
			// Kept in sync with /health, which has always reported the routable
			// count under the same name.
			"poolAvailable":    routable,
			"averageLatencyMs": averageLatency,
		},
	})
}

func parsePeriodDays(value string) int {
	value = strings.TrimSuffix(value, "d")
	days, err := strconv.Atoi(value)
	if err != nil || days <= 0 {
		return 30
	}
	if days > 365 {
		return 365
	}
	return days
}

func bucketKey(at time.Time, days int) string {
	if days <= 2 {
		return at.Format("2006-01-02T15:00")
	}
	return at.Format("2006-01-02")
}

// ----------------------------------------------------------------- accounts

func (a *API) listAccounts(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page := maxInt(1, config.ParseIntOrDefault(query.Get("page"), 1))
	pageSize := clampInt(config.ParseIntOrDefault(query.Get("pageSize"), 20), 1, 500)
	search := strings.ToLower(strings.TrimSpace(query.Get("search")))
	status := query.Get("status")
	kind := query.Get("kind")
	group := query.Get("group")
	sortBy := query.Get("sortBy")
	sortOrder := query.Get("sortOrder")

	inflight := a.pool.Snapshot()
	accounts := a.store.ListAccounts()

	views := make([]store.AccountView, 0, len(accounts))
	for _, account := range accounts {
		if status != "" && account.Status != status {
			continue
		}
		if kind != "" && account.Kind != kind {
			continue
		}
		if group != "" && account.Group != group {
			continue
		}
		if search != "" {
			haystack := strings.ToLower(account.Name + " " + account.Remark + " " + account.Group + " " +
				account.Identifier)
			if !strings.Contains(haystack, search) {
				continue
			}
		}
		views = append(views, store.NewAccountView(account, inflight[account.ID]))
	}

	sortViews(views, sortBy, sortOrder)

	total := len(views)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	_, active, cooldown, disabled, invalid, routable := a.pool.Summary()
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    views[start:end],
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
		"summary": map[string]any{
			"total": total, "active": active, "cooldown": cooldown,
			"disabled": disabled, "invalid": invalid, "routable": routable,
		},
	})
}

func sortViews(views []store.AccountView, field, order string) {
	if field == "" {
		return
	}
	less := func(i, j int) bool {
		left, right := views[i], views[j]
		switch field {
		case "name":
			return strings.ToLower(left.Name) < strings.ToLower(right.Name)
		case "status":
			return left.Status < right.Status
		case "priority":
			return left.Priority < right.Priority
		case "lastUsedAt":
			return left.LastUsedAt.Before(right.LastUsedAt)
		default:
			return left.CreatedAt.Before(right.CreatedAt)
		}
	}
	sort.SliceStable(views, func(i, j int) bool {
		if order == "asc" {
			return less(i, j)
		}
		return less(j, i)
	})
}

func (a *API) accountGroups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"groups": a.store.AccountGroups()})
}

type accountPayload struct {
	Name string `json:"name"`
	// Cookie is the whole credential. It is the only required field for a
	// cookie account, which is what makes "paste and save" work.
	Cookie     string `json:"cookie"`
	Identifier string `json:"identifier"`
	// AuthUser selects which signed-in Google account to act as: 0 is the
	// default, N means /u/N. It is a pointer because "not provided" and "zero"
	// are different requests: getting the index wrong does not error, it
	// quietly answers as a different account, so an update that omits the field
	// must leave the stored index alone rather than reset it to the default.
	AuthUser      *int   `json:"authUser"`
	Model         string `json:"model"`
	BaseURL       string `json:"baseURL"`
	Group         string `json:"group"`
	Remark        string `json:"remark"`
	Priority      int    `json:"priority"`
	MaxConcurrent int    `json:"maxConcurrent"`
	Enabled       *bool  `json:"enabled"`
	Kind          string `json:"kind"`
}

func (a *API) createAccount(w http.ResponseWriter, r *http.Request) {
	var payload accountPayload
	if err := decode(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	account, err := buildAccount(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.AddAccount(account); err != nil {
		if err == store.ErrAlreadyExists {
			writeError(w, http.StatusConflict, "该账号（按 __Secure-1PSID 判定）已存在于号池中")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The probe talks to the upstream and can take a while, so the account is
	// returned immediately and the probe continues in background.
	go a.probeDetached(account.ID)
	writeJSON(w, http.StatusOK, map[string]any{"account": a.view(account.ID)})
}

// buildAccount validates an account payload and fills in derived fields.
//
// A guest account is the one case that needs no credential: Gemini serves
// anonymous sessions, and they are worth pooling because they cost nothing to
// obtain. Everything else needs a cookie, and the validation that runs on it is
// deliberately shallow — only Google can say whether a session is live, so the
// check catches the two mistakes that are actually common (pasting just
// __Secure-1PSIDTS, pasting something that is not a cookie) rather than
// pretending to verify the credential.
func buildAccount(payload accountPayload) (*store.Account, error) {
	requestedKind := strings.TrimSpace(payload.Kind)
	kind := firstNonEmpty(requestedKind, store.KindCookie)
	cookie := strings.TrimSpace(payload.Cookie)

	enabled := true
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}

	account := &store.Account{
		Name:          strings.TrimSpace(payload.Name),
		Kind:          kind,
		BaseURL:       strings.TrimSpace(payload.BaseURL),
		Group:         strings.TrimSpace(payload.Group),
		Remark:        strings.TrimSpace(payload.Remark),
		Enabled:       enabled,
		Model:         strings.TrimSpace(payload.Model),
		Priority:      clampInt(firstNonZero(payload.Priority, 50), 1, 100),
		MaxConcurrent: clampInt(firstNonZero(payload.MaxConcurrent, 2), 1, 256),
	}
	if payload.AuthUser != nil {
		account.AuthUser = clampInt(*payload.AuthUser, 0, 20)
	}

	// A guest is a deliberate choice and carries no credential, so an explicit
	// guest kind needs no cookie. An *empty form* means the same thing: the
	// console's add panel posts only the fields the operator filled in, so
	// "nothing at all" must add a guest rather than fail.
	//
	// What must not fall through is an operator who asked for a cookie account
	// and pasted nothing. Adding a guest instead would look like success while
	// quietly giving every request the guest quota.
	if kind == store.KindGuest || (cookie == "" && requestedKind == "") {
		account.Kind = store.KindGuest
		if account.Name == "" {
			account.Name = "游客"
		}
		return account, nil
	}
	if cookie == "" {
		return nil, errors.New("请粘贴 gemini.google.com 的 Cookie，或选择「游客」类型")
	}

	if err := gemini.ValidateCredential(cookie); err != nil {
		return nil, err
	}

	psid, psidts, _, _ := gemini.DescribeCredential(cookie)
	account.Cookie = cookie
	account.PSID = psid
	account.PSIDTS = psidts

	identifier := strings.TrimSpace(payload.Identifier)
	if identifier == "" {
		// Google does not expose the address anywhere the cookie carries, so
		// the only stable handle is a prefix of the session id.
		identifier = gemini.AccountLabel(gemini.Credential{PSID: psid})
	}
	account.Identifier = identifier
	if account.Name == "" {
		account.Name = identifier
	}

	return account, nil
}

func (a *API) updateAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var payload accountPayload
	if err := decode(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	// Validate a replacement cookie *before* touching the account: a
	// half-applied update would leave the pool holding a credential nobody can
	// use, and the operator's only clue would be requests failing later.
	replacement := strings.TrimSpace(payload.Cookie)
	if replacement != "" {
		if err := gemini.ValidateCredential(replacement); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	cookieReplaced := false
	account, err := a.store.UpdateAccount(id, func(account *store.Account) {
		if strings.TrimSpace(payload.Name) != "" {
			account.Name = strings.TrimSpace(payload.Name)
		}
		if replacement != "" && replacement != account.Cookie {
			cookieReplaced = true
			psid, psidts, _, _ := gemini.DescribeCredential(replacement)
			account.Cookie = replacement
			account.PSID = psid
			account.PSIDTS = psidts
			// A new cookie is a new session: clear the rotation bookkeeping so
			// the sweep picks it up immediately instead of waiting out an
			// interval that belonged to the previous credential.
			account.RefreshAt = time.Time{}
			account.RefreshFailures = 0
			account.RefreshStatus = ""
			account.RefreshError = ""
			// An invalid account was retired for a credential that no longer
			// exists; the replacement is the thing that un-retires it.
			account.Status = store.StatusActive
			account.CooldownUntil = time.Time{}
			account.FailCount = 0
			account.LastError = ""
		}
		if identifier := strings.TrimSpace(payload.Identifier); identifier != "" {
			account.Identifier = identifier
		}
		if payload.AuthUser != nil {
			account.AuthUser = clampInt(*payload.AuthUser, 0, 20)
		}
		account.Model = strings.TrimSpace(payload.Model)
		// BaseURL, group and remark are cleared when omitted, because for these
		// three "empty" is a meaningful value the operator may want to set.
		account.BaseURL = strings.TrimSpace(payload.BaseURL)
		account.Group = strings.TrimSpace(payload.Group)
		account.Remark = strings.TrimSpace(payload.Remark)
		if payload.Priority > 0 {
			account.Priority = clampInt(payload.Priority, 1, 100)
		}
		if payload.MaxConcurrent > 0 {
			account.MaxConcurrent = clampInt(payload.MaxConcurrent, 1, 256)
		}
		if payload.Enabled != nil {
			account.Enabled = *payload.Enabled
		}
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	// Re-probe only when the credential changed; the rest of the fields do not
	// affect whether the account works.
	if cookieReplaced {
		go a.probeDetached(account.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": a.view(account.ID)})
}

func (a *API) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteAccount(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) batchAccounts(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Action        string   `json:"action"`
		IDs           []string `json:"ids"`
		MaxConcurrent int      `json:"maxConcurrent"`
	}
	if err := decode(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(payload.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "未选择账号")
		return
	}

	switch payload.Action {
	case "delete":
		deleted, err := a.store.DeleteAccounts(payload.IDs)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
	case "enable", "disable":
		enabled := payload.Action == "enable"
		updated, err := a.store.UpdateAccounts(payload.IDs, func(account *store.Account) {
			account.Enabled = enabled
			if !enabled {
				account.Status = store.StatusDisabled
			} else if account.Status == store.StatusDisabled {
				account.Status = store.StatusActive
			}
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"updated": updated})
	case "clearCooldown":
		updated, err := a.store.UpdateAccounts(payload.IDs, func(account *store.Account) {
			account.Status = store.StatusActive
			account.CooldownUntil = time.Time{}
			account.FailCount = 0
			account.LastError = ""
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"updated": updated})
	case "concurrency":
		value := clampInt(payload.MaxConcurrent, 1, 256)
		updated, err := a.store.UpdateAccounts(payload.IDs, func(account *store.Account) {
			account.MaxConcurrent = value
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"updated": updated})
	case "probe":
		succeeded, failed := 0, 0
		for _, id := range payload.IDs {
			if err := a.syncQuota(r.Context(), id); err != nil {
				failed++
				continue
			}
			succeeded++
		}
		writeJSON(w, http.StatusOK, map[string]any{"succeeded": succeeded, "failed": failed})
	default:
		writeError(w, http.StatusBadRequest, "不支持的操作")
	}
}

func (a *API) cleanupAccounts(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Statuses []string `json:"statuses"`
	}
	if err := decode(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(payload.Statuses) == 0 {
		writeError(w, http.StatusBadRequest, "请至少选择一种状态")
		return
	}
	deleted, err := a.store.DeleteAccountsByStatus(payload.Statuses)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (a *API) importAccounts(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Cookies string          `json:"cookies"`
		JSON    json.RawMessage `json:"json"`
	}
	if err := decode(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	entries := parseImport(payload.Cookies, payload.JSON)
	if len(entries) == 0 {
		writeError(w, http.StatusBadRequest, "没有解析到可导入的账号")
		return
	}

	created, updated, failed := 0, 0, 0
	failures := make([]string, 0, 4)
	record := func(cookie string, err error) {
		failed++
		if len(failures) < 4 {
			failures = append(failures, describeCookie(cookie)+": "+err.Error())
		}
	}

	for _, entry := range entries {
		account, err := buildAccount(accountPayload{
			Name: entry.Name, Cookie: entry.Cookie,
			Identifier: entry.Identifier, AuthUser: entry.authUserPtr(), Model: entry.Model,
			BaseURL: entry.BaseURL, Group: entry.Group, Remark: entry.Remark,
			Priority: entry.Priority, MaxConcurrent: entry.MaxConcurrent,
		})
		if err != nil {
			record(entry.Cookie, err)
			continue
		}

		if existing := a.findByCookie(account.Cookie); existing != nil {
			// Re-importing is the documented way to refresh an expired cookie,
			// so an existing entry is updated rather than rejected.
			_, err := a.store.UpdateAccount(existing.ID, func(target *store.Account) {
				target.Cookie = account.Cookie
				target.PSID = account.PSID
				target.PSIDTS = account.PSIDTS
				if entry.Name != "" {
					target.Name = entry.Name
				}
				if account.Identifier != "" {
					target.Identifier = account.Identifier
				}
				// A fresh cookie is a fresh session: clear the rotation
				// bookkeeping and un-retire the account, because whatever made
				// it invalid was the credential that just got replaced.
				target.RefreshAt = time.Time{}
				target.RefreshFailures = 0
				target.RefreshStatus = ""
				target.RefreshError = ""
				target.Status = store.StatusActive
				target.CooldownUntil = time.Time{}
				target.FailCount = 0
				target.LastError = ""
			})
			if err != nil {
				record(entry.Cookie, err)
				continue
			}
			updated++
			continue
		}

		if err := a.store.AddAccount(account); err != nil {
			record(entry.Cookie, err)
			continue
		}
		created++
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"created": created, "updated": updated, "failed": failed, "errors": failures,
	})
}

// describeCookie renders enough of a credential to identify it in an error
// message without echoing it back into the console.
func describeCookie(cookie string) string {
	psid, _, _, ok := gemini.DescribeCredential(strings.TrimSpace(cookie))
	if !ok {
		return "该账号"
	}
	if len(psid) <= 14 {
		return psid
	}
	return psid[:8] + "…" + psid[len(psid)-4:]
}

type importEntry struct {
	Name          string `json:"name"`
	Cookie        string `json:"cookie"`
	Identifier    string `json:"identifier"`
	AuthUser      int    `json:"authUser"`
	Model         string `json:"model"`
	BaseURL       string `json:"baseURL"`
	Group         string `json:"group"`
	Remark        string `json:"remark"`
	Priority      int    `json:"priority"`
	MaxConcurrent int    `json:"maxConcurrent"`
}

// authUserPtr keeps "the import did not mention an index" distinguishable from
// "the import said index 0", which matters because index 0 and index 1 are
// different Google accounts and picking the wrong one is silent.
func (e importEntry) authUserPtr() *int {
	if e.AuthUser <= 0 {
		return nil
	}
	return &e.AuthUser
}

// parseImport accepts the shapes an operator is likely to paste.
//
// Recognised line formats, in the order they are tried:
//
//	<cookie>
//	<name>----<cookie>
//
// A JSON array (or {"accounts": [...]}) takes precedence when supplied, which
// is what the export endpoint emits and what makes a round trip lossless.
//
// There is no per-line region shorthand here, unlike the sibling 2api projects.
// Those proxy services that run separate mainland and international deployments
// with separate account databases; gemini.google.com is one host worldwide, so
// the only thing a "region" could mean is the egress country — a property of
// the gateway, not of an account.
func parseImport(rawCookies string, rawJSON json.RawMessage) []importEntry {
	if len(rawJSON) > 0 {
		var list []importEntry
		if err := json.Unmarshal(rawJSON, &list); err == nil && len(list) > 0 {
			return list
		}
		var wrapped struct {
			Accounts []importEntry `json:"accounts"`
		}
		if err := json.Unmarshal(rawJSON, &wrapped); err == nil && len(wrapped.Accounts) > 0 {
			return wrapped.Accounts
		}
	}

	entries := make([]importEntry, 0, 16)
	for _, line := range strings.Split(rawCookies, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if index := strings.Index(line, "----"); index > 0 {
			entries = append(entries, importEntry{
				Name:   strings.TrimSpace(line[:index]),
				Cookie: strings.TrimSpace(line[index+4:]),
			})
			continue
		}

		entries = append(entries, importEntry{Cookie: line})
	}
	return entries
}

func (a *API) exportAccounts(w http.ResponseWriter, r *http.Request) {
	limit := clampInt(config.ParseIntOrDefault(r.URL.Query().Get("limit"), 10000), 1, 10000)
	accounts := a.store.ListAccounts()
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].CreatedAt.After(accounts[j].CreatedAt) })
	if len(accounts) > limit {
		accounts = accounts[:limit]
	}
	out := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		// The export is the import format: everything needed to rebuild the
		// account, so a round trip through a file is lossless. The cookie is
		// included because it *is* the credential — an export without it would
		// be a list of names.
		out = append(out, map[string]any{
			"name": account.Name, "cookie": account.Cookie,
			"identifier": account.Identifier, "authUser": account.AuthUser,
			"model":   account.Model,
			"baseURL": account.BaseURL, "group": account.Group, "remark": account.Remark,
			"priority": account.Priority, "maxConcurrent": account.MaxConcurrent,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out, "count": len(out)})
}

func (a *API) probeAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.store.AccountByID(id); !ok {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	probeCtx, cancel := context.WithTimeout(r.Context(), probeTimeout(a.settings()))
	defer cancel()

	err := a.syncQuota(probeCtx, id)
	message := "OK"
	if err != nil {
		message = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": err == nil, "message": message, "account": a.view(id),
	})
}

func (a *API) probeAll(w http.ResponseWriter, r *http.Request) {
	healthy, unhealthy := 0, 0
	for _, account := range a.store.ListAccounts() {
		if !account.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout(a.settings()))
		err := a.syncQuota(ctx, account.ID)
		cancel()
		if err != nil {
			unhealthy++
			continue
		}
		healthy++
	}
	writeJSON(w, http.StatusOK, map[string]any{"healthy": healthy, "unhealthy": unhealthy})
}

// refreshAll rotates every eligible account's cookie on demand.
//
// It deliberately ignores the per-account interval: this is the button an
// operator presses because something is already wrong, and having it answer
// "nothing was due" would be useless exactly when it is needed.
func (a *API) refreshAll(w http.ResponseWriter, r *http.Request) {
	summary := a.refresher.Sweep(r.Context(), a.refreshableAccounts())
	writeJSON(w, http.StatusOK, map[string]any{"summary": summary})
}

// refreshableAccounts lists the accounts a sweep may touch.
func (a *API) refreshableAccounts() []*store.Account {
	var out []*store.Account
	for _, account := range a.store.ListAccounts() {
		if !account.Enabled || account.Kind == store.KindGuest || account.Cookie == "" {
			continue
		}
		// A retired account cannot be refreshed — the rotation endpoint would
		// reject it — so including it would only add noise to the summary.
		if account.Status == store.StatusInvalid {
			continue
		}
		out = append(out, account)
	}
	return out
}

// probeDetached runs a probe outside the request lifecycle with a bounded
// timeout, so a slow upstream never blocks the console.
func (a *API) probeDetached(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout(a.settings()))
	defer cancel()
	_ = a.syncQuota(ctx, id)
}

// syncQuota probes an account and records the outcome on it.
//
// The name comes from the console, not from the protocol: this API has no quota
// endpoint. What *is* observable is whether the app shell loads and whether it
// issued a real session — and "loaded but not signed in" is precisely what an
// expired cookie looks like, because every request then succeeds as a guest.
// That distinction is the whole point of probing.
func (a *API) syncQuota(ctx context.Context, id string) error {
	account, ok := a.store.AccountByID(id)
	if !ok {
		return store.ErrNotFound
	}

	// A guest has no session, so there is nothing to probe and nothing that
	// could be "rejected". Asking upstream anyway comes back as
	// ErrInvalidCredential, and recording that would retire the one account kind
	// that is supposed to work without a credential — leaving a pool where the
	// fallback is created already dead, which is worse than having none at all.
	if account.Kind == store.KindGuest || account.Cookie == "" {
		a.store.SaveAccountState(id, func(target *store.Account) {
			target.Quota = &store.Quota{
				SyncedAt: time.Now(), Available: true, Authenticated: false,
				Note: "游客账号：按上游匿名额度作答，无需会话",
			}
			// Un-retire anything a previous build wrongly retired, so an
			// existing pool heals instead of staying broken.
			if target.Status == store.StatusInvalid {
				target.Status = store.StatusActive
			}
			target.CooldownUntil = time.Time{}
			target.LastError = ""
		})
		return nil
	}

	result, err := a.client.Probe(ctx, gateway.CredentialOf(account))

	latency := int64(0)
	message := "OK"
	authenticated := false
	if result != nil {
		latency = result.LatencyMs
		if result.Note != "" {
			message = result.Note
		}
		authenticated = result.Authenticated
	}
	if err != nil {
		message = err.Error()
	}

	a.store.SaveAccountState(id, func(target *store.Account) {
		target.Quota = &store.Quota{
			SyncedAt: time.Now(), Available: err == nil,
			Authenticated: authenticated, LatencyMs: latency, Note: message,
		}
		if result != nil {
			if result.BuildLabel != "" {
				target.BuildLabel = result.BuildLabel
			}
			if result.Language != "" {
				target.Language = result.Language
			}
		}
		if err != nil {
			if errors.Is(err, gemini.ErrInvalidCredential) {
				target.Status = store.StatusInvalid
			} else if target.Status != store.StatusDisabled {
				target.Status = store.StatusCooldown
				target.CooldownUntil = time.Now().Add(a.settings().CooldownBase())
			}
			target.LastError = truncate(message, 200)
			return
		}
		target.Status = store.StatusActive
		target.CooldownUntil = time.Time{}
		target.LastError = ""
	})
	return err
}

// refreshOverview reports the sweep's configuration and state.
func (a *API) refreshOverview(w http.ResponseWriter, r *http.Request) {
	settings := a.settings()
	accounts := a.store.ListAccounts()

	// counts describes the accounts the sweep can actually act on, not the whole
	// pool. A guest has no cookie to rotate and a disabled account is not
	// scheduled, so counting either as "pending" would leave the console
	// reporting work that will never happen — a badge that is permanently wrong
	// is worse than no badge, because it teaches the operator to ignore it.
	counts := map[string]int{}
	now := time.Now()
	interval := settings.RefreshInterval()
	var next time.Time

	for _, account := range accounts {
		if account.Kind == store.KindGuest || !account.Enabled || account.Cookie == "" {
			continue
		}
		if account.Status == store.StatusInvalid {
			continue
		}

		status := account.RefreshStatus
		if status == "" {
			status = "pending"
		}
		counts[status]++

		if account.RefreshAt.IsZero() {
			next = now
			continue
		}
		due := account.RefreshAt.Add(interval)
		if !due.After(now) {
			next = now
			continue
		}
		if next.IsZero() || due.Before(next) {
			next = due
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       settings.Refresh.Enabled,
		"intervalMin":   settings.Refresh.IntervalMin,
		"gapSeconds":    settings.Refresh.GapSeconds,
		"timeoutSec":    settings.Refresh.TimeoutSec,
		"retireAfter":   settings.Refresh.RetireAfterFailures,
		"counts":        counts,
		"nextRunAt":     next,
		"lastSummary":   a.refresher.LastSummary(),
		"totalAccounts": len(accounts),
	})
}

// refreshRun triggers a sweep immediately, ignoring the interval.
func (a *API) refreshRun(w http.ResponseWriter, r *http.Request) {
	due := a.refreshableAccounts()
	if len(due) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"summary": refresher.Summary{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": a.refresher.Sweep(r.Context(), due)})
}

// refreshAccount rotates one account's cookie.
func (a *API) refreshAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	outcome, err := a.refresher.RefreshAccount(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	// A rejected rotation is reported as a bad gateway rather than a 200 with a
	// sad body: the console keys its toast off the status, and "refreshed" is
	// the one thing this call must never appear to have done when it did not.
	status := http.StatusOK
	body := map[string]any{"outcome": outcome, "account": a.view(id)}
	if outcome.Status == store.RefreshInvalid || outcome.Status == store.RefreshFailed {
		status = http.StatusBadGateway
		// The error key is duplicated on purpose. The console reads `error` off
		// any failed response, so without it the operator is told "HTTP 502"
		// instead of why the rotation was refused — and the reason is the only
		// actionable part: an invalid session means re-paste the cookie, while
		// a transient failure means just try again.
		body["error"] = firstNonEmpty(outcome.Error, "Cookie 刷新失败")
	}
	writeJSON(w, status, body)
}

// probeTimeout bounds one probe. It is shorter than the request timeout because
// a probe is a page fetch, not a generation.
func probeTimeout(settings config.Settings) time.Duration {
	timeout := settings.RequestTimeout()
	if timeout > 60*time.Second {
		timeout = 60 * time.Second
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return timeout
}

func (a *API) view(id string) *store.AccountView {
	inflight := a.pool.Snapshot()
	account, ok := a.store.AccountByID(id)
	if !ok {
		return nil
	}
	view := store.NewAccountView(account, inflight[id])
	return &view
}

func (a *API) findByCookie(cookie string) *store.Account {
	for _, account := range a.store.ListAccounts() {
		if account.Cookie == cookie {
			return account
		}
	}
	return nil
}

// -------------------------------------------------------------- client keys

func (a *API) listClientKeys(w http.ResponseWriter, r *http.Request) {
	keys := a.store.ListClientKeys()
	items := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		items = append(items, map[string]any{
			"id": key.ID, "name": key.Name, "key": key.Key, "maskedKey": maskKey(key.Key),
			"enabled": key.Enabled, "rpmLimit": key.RPMLimit, "maxConcurrent": key.MaxConcurrent,
			"totalRequests": key.TotalRequests, "createdAt": key.CreatedAt, "lastUsedAt": key.LastUsedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func maskKey(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:11] + "…" + key[len(key)-4:]
}

func (a *API) createClientKey(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Name          string `json:"name"`
		RPMLimit      int    `json:"rpmLimit"`
		MaxConcurrent int    `json:"maxConcurrent"`
	}
	if err := decode(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if strings.TrimSpace(payload.Name) == "" {
		writeError(w, http.StatusBadRequest, "密钥名称不能为空")
		return
	}
	key, err := a.store.CreateClientKey(strings.TrimSpace(payload.Name), maxInt(0, payload.RPMLimit), clampInt(payload.MaxConcurrent, 1, 256))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": map[string]any{
		"id": key.ID, "name": key.Name, "key": key.Key, "maskedKey": maskKey(key.Key),
		"enabled": key.Enabled, "rpmLimit": key.RPMLimit, "maxConcurrent": key.MaxConcurrent,
		"totalRequests": key.TotalRequests, "createdAt": key.CreatedAt, "lastUsedAt": key.LastUsedAt,
	}})
}

func (a *API) updateClientKey(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Name          string `json:"name"`
		Enabled       *bool  `json:"enabled"`
		RPMLimit      *int   `json:"rpmLimit"`
		MaxConcurrent *int   `json:"maxConcurrent"`
	}
	if err := decode(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	key, err := a.store.UpdateClientKey(r.PathValue("id"), func(key *store.ClientKey) {
		if strings.TrimSpace(payload.Name) != "" {
			key.Name = strings.TrimSpace(payload.Name)
		}
		if payload.Enabled != nil {
			key.Enabled = *payload.Enabled
		}
		if payload.RPMLimit != nil {
			key.RPMLimit = maxInt(0, *payload.RPMLimit)
		}
		if payload.MaxConcurrent != nil {
			key.MaxConcurrent = clampInt(*payload.MaxConcurrent, 1, 256)
		}
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "密钥不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": map[string]any{
		"id": key.ID, "name": key.Name, "key": key.Key, "maskedKey": maskKey(key.Key),
		"enabled": key.Enabled, "rpmLimit": key.RPMLimit, "maxConcurrent": key.MaxConcurrent,
		"totalRequests": key.TotalRequests, "createdAt": key.CreatedAt, "lastUsedAt": key.LastUsedAt,
	}})
}

func (a *API) deleteClientKey(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteClientKey(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "密钥不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ------------------------------------------------------------------- models

func (a *API) listModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": a.store.ListModels()})
}

func (a *API) updateModel(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Enabled     *bool  `json:"enabled"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decode(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	model, err := a.store.UpdateModel(r.PathValue("id"), func(model *store.ModelConfig) {
		if payload.Enabled != nil {
			model.Enabled = *payload.Enabled
		}
		if strings.TrimSpace(payload.Name) != "" {
			model.Name = strings.TrimSpace(payload.Name)
		}
		if strings.TrimSpace(payload.Description) != "" {
			model.Description = strings.TrimSpace(payload.Description)
		}
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "模型不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": model})
}

// ------------------------------------------------------------------- audits

func (a *API) listAudits(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page := maxInt(1, config.ParseIntOrDefault(query.Get("page"), 1))
	pageSize := clampInt(config.ParseIntOrDefault(query.Get("pageSize"), 20), 1, 500)
	search := strings.ToLower(strings.TrimSpace(query.Get("search")))
	status := query.Get("status")

	audits := a.store.ListAudits()
	filtered := make([]*store.Audit, 0, len(audits))
	for _, audit := range audits {
		if status == "success" && audit.Status >= 400 {
			continue
		}
		if status == "failed" && audit.Status < 400 {
			continue
		}
		if search != "" {
			haystack := strings.ToLower(audit.ID + " " + audit.Model + " " + audit.KeyName + " " + audit.AccountName)
			if !strings.Contains(haystack, search) {
				continue
			}
		}
		filtered = append(filtered, audit)
	}

	total := len(filtered)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := minInt(start+pageSize, total)

	writeJSON(w, http.StatusOK, map[string]any{
		"items": filtered[start:end], "total": total, "page": page, "pageSize": pageSize,
	})
}

func (a *API) auditDetail(w http.ResponseWriter, r *http.Request) {
	audit, ok := a.store.AuditByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "记录不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": audit})
}

func (a *API) clearAudits(w http.ResponseWriter, r *http.Request) {
	if err := a.store.ClearAudits(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ----------------------------------------------------------------- settings

func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	settings := a.settings()
	writeJSON(w, http.StatusOK, map[string]any{
		"server": map[string]any{
			"addr": settings.Server.Addr, "maxConcurrentRequests": settings.Server.MaxConcurrentRequests,
			"adminUsername": a.store.AdminProfile().Username,
		},
		"upstream": map[string]any{
			"baseURL":              settings.Upstream.BaseURL,
			"language":             settings.Upstream.Language,
			"defaultModel":         settings.Upstream.DefaultModel,
			"requestTimeoutSec":    settings.Upstream.RequestTimeoutSec,
			"streamIdleTimeoutSec": settings.Upstream.StreamIdleTimeoutSec,
			"proxy":                settings.Upstream.Proxy,
			"userAgent":            settings.Upstream.UserAgent,
			"rotateURL":            settings.Upstream.RotateURL,
		},
		"routing": settings.Routing,
		"audit":   settings.Audit,
		"media":   settings.Media,
		"refresh": settings.Refresh,
		"about": map[string]any{
			"version": gateway.Version, "buildTime": a.started.Format(time.RFC3339),
			"dataDir": a.store.DataDir(), "upstreamURL": settings.Upstream.BaseURL,
		},
	})
}

func (a *API) saveSettings(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Server        *config.ServerSettings   `json:"server"`
		Upstream      *config.UpstreamSettings `json:"upstream"`
		Routing       *config.RoutingSettings  `json:"routing"`
		Audit         *config.AuditSettings    `json:"audit"`
		Media         *config.MediaSettings    `json:"media"`
		Refresh       *config.RefreshSettings  `json:"refresh"`
		AdminPassword string                   `json:"adminPassword"`
	}
	if err := decode(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	if err := a.store.UpdateSettings(func(settings *config.Settings) {
		if payload.Server != nil {
			settings.Server.MaxConcurrentRequests = payload.Server.MaxConcurrentRequests
			if strings.TrimSpace(payload.Server.AdminUsername) != "" {
				settings.Server.AdminUsername = strings.TrimSpace(payload.Server.AdminUsername)
			}
		}
		if payload.Upstream != nil {
			settings.Upstream = *payload.Upstream
		}
		if payload.Routing != nil {
			settings.Routing = *payload.Routing
		}
		if payload.Audit != nil {
			settings.Audit = *payload.Audit
		}
		if payload.Media != nil {
			settings.Media = *payload.Media
		}
		if payload.Refresh != nil {
			settings.Refresh = *payload.Refresh
		}
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if payload.AdminPassword != "" {
		admin := a.store.AdminProfile()
		username := admin.Username
		if payload.Server != nil && strings.TrimSpace(payload.Server.AdminUsername) != "" {
			username = strings.TrimSpace(payload.Server.AdminUsername)
		}
		if err := a.store.SetPassword(username, payload.AdminPassword); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	a.getSettings(w, r)
}

// ------------------------------------------------------------------ gallery

func (a *API) gallery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": a.store.ListMedia()})
}

// ------------------------------------------------------------------ helpers

func decode(r *http.Request, target any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, target)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
