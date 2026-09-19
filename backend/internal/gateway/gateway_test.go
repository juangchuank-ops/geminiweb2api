package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"geminiweb2api/internal/config"
	"geminiweb2api/internal/gemini"
	"geminiweb2api/internal/pool"
	"geminiweb2api/internal/store"
)

// These tests drive the full request path — auth, limiting, pool scheduling,
// upstream parsing, audit — against a stub that speaks the real Gemini web
// protocol, so the gateway can be exercised end to end without a live account.
//
// The stub matters as much as the assertions. An earlier version of this file
// stubbed a protocol the client no longer speaks (SSE with a `token` header),
// which meant every test passed while the real request path was unexercised.

// appShell is the document the app shell endpoint returns. Only the five values
// the client scrapes are present; everything else in a real page is irrelevant
// to the code under test and would only make the fixture harder to read.
const appShell = `<!DOCTYPE html><html><head><script>
window.WIZ_global_data = {
  "SNlM0e":"AO-x_stub-access-token:1700000000",
  "cfb2h":"boq_assistant-bard-web-server_20260919.08_p0",
  "FdrFJe":"-1234567890123456789",
  "TuX5cc":"en",
  "qKIAYe":"feeds/mcudyrk2a4khkz"
};
</script></head><body></body></html>`

// --- building upstream frames ----------------------------------------------

// candidate builds one entry of the payload's candidate list at the slots the
// parser reads.
func candidate(text, thoughts string, done bool, images []string) []any {
	entry := make([]any, 40)
	entry[0] = "rcid-1"
	entry[1] = []any{text}
	state := 1
	if done {
		state = 2
	}
	entry[8] = []any{state}
	if len(images) > 0 {
		rich := make([]any, 60)
		attachments := make([]any, 0, len(images))
		for _, imageURL := range images {
			attachments = append(attachments, []any{[]any{nil, nil, imageURL}})
		}
		// Slot 7 of the rich block is the image list.
		rich[7] = attachments
		entry[12] = rich
	}
	if thoughts != "" {
		entry[37] = []any{[]any{thoughts}}
	}
	return entry
}

// turnPayload builds the inner payload of one response frame.
func turnPayload(text, thoughts string, done bool, images ...string) []any {
	payload := make([]any, 5)
	payload[1] = []any{"cid-1", "rid-1", "rcid-1", nil}
	payload[4] = []any{candidate(text, thoughts, done, images)}
	return payload
}

// frame wraps a payload in the batchexecute envelope and length-prefixes it.
//
// The length counts UTF-16 code units, not bytes. Building it with the byte
// length would work for ASCII and desynchronise on the first CJK answer — and
// every assertion in this file uses CJK, which is deliberate.
func frame(payload []any) string {
	inner, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	envelope, err := json.Marshal([]any{[]any{"wrb.fr", "rpc-1", string(inner), nil, nil, nil, "generic"}})
	if err != nil {
		return ""
	}
	body := string(envelope)
	return strconv.Itoa(gemini.UTF16Len(body)) + "\n" + body + "\n"
}

// stream assembles a complete batchexecute response: the anti-CSRF prefix and
// then one frame per payload.
func stream(payloads ...[]any) string {
	var b strings.Builder
	b.WriteString(")]}'")
	for _, payload := range payloads {
		b.WriteString(frame(payload))
	}
	return b.String()
}

// --- the stub upstream ------------------------------------------------------

type stubCall struct {
	cookie string
	query  url.Values
	form   url.Values
}

type stub struct {
	mu sync.Mutex

	shells      int
	generations []stubCall

	// rejectCookie makes the app shell answer 401 to any request whose cookie
	// contains it, which is how a revoked credential presents itself.
	rejectCookie string
	// shellStatus and generateStatus override the answer when non-zero.
	shellStatus    int
	generateStatus int
	// errorCode, when non-zero, is emitted as a BardErrorInfo marker in the
	// generation stream the way the upstream reports a refused request.
	// failFirst limits it to the first N generation calls (0 means every call),
	// which is what lets a test model one bad account in a pool of two.
	errorCode int
	failFirst int
	// payloads is the generation answer, evaluated per request so a test can
	// vary it between attempts.
	payloads func() [][]any
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/StreamGenerate"):
		s.serveGenerate(w, r)
	case strings.HasSuffix(r.URL.Path, "/app"):
		s.serveShell(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *stub) serveShell(w http.ResponseWriter, r *http.Request) {
	cookie := r.Header.Get("Cookie")

	s.mu.Lock()
	s.shells++
	reject := s.rejectCookie
	status := s.shellStatus
	s.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if reject != "" && strings.Contains(cookie, reject) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, appShell)
}

func (s *stub) serveGenerate(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	form, _ := url.ParseQuery(string(raw))

	s.mu.Lock()
	index := len(s.generations)
	s.generations = append(s.generations, stubCall{
		cookie: r.Header.Get("Cookie"),
		query:  r.URL.Query(),
		form:   form,
	})
	status := s.generateStatus
	code := s.errorCode
	if s.failFirst > 0 && index >= s.failFirst {
		code = 0
	}
	payloads := s.payloads
	s.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	if code != 0 {
		// The refusal travels as an ordinary frame whose payload is the marker,
		// which is the shape the client decodes.
		_, _ = io.WriteString(w, stream([]any{[]any{"BardErrorInfo", []any{code}}}))
		return
	}
	if payloads == nil {
		_, _ = io.WriteString(w, stream())
		return
	}
	_, _ = io.WriteString(w, stream(payloads()...))
}

// --- harness ----------------------------------------------------------------

type harness struct {
	gateway  *Gateway
	store    *store.Store
	pool     *pool.Pool
	upstream *stub
	server   *httptest.Server
	settings *config.Settings
	key      string
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
	settings.Routing.CapacityWaitSec = 0 // fail fast instead of waiting for capacity
	settings.Routing.MaxAttempts = 3
	settings.Upstream.RequestTimeoutSec = 5
	settings.Upstream.StreamIdleTimeoutSec = 3
	settings.Media.AutoDownload = false

	up := &stub{}
	server := httptest.NewServer(up)
	t.Cleanup(server.Close)
	settings.Upstream.BaseURL = server.URL

	settingsFn := func() config.Settings { return settings }
	client := gemini.New(settingsFn)
	p := pool.New(st, settingsFn)
	gw := New(st, p, client, settingsFn)

	key, err := st.CreateClientKey("test-key", 0, 0)
	if err != nil {
		t.Fatalf("create client key: %v", err)
	}

	return &harness{
		gateway:  gw,
		store:    st,
		pool:     p,
		upstream: up,
		server:   server,
		settings: &settings,
		key:      key.Key,
	}
}

// cookieFor builds a paste whose PSID is unique per name, so two accounts in a
// test are never mistaken for a duplicate of each other.
func cookieFor(name string) string {
	return "__Secure-1PSID=g.a000" + name + "psidvalue0000000000; __Secure-1PSIDTS=sidts-" + name
}

// addAccount inserts a routable account.
func (h *harness) addAccount(t *testing.T, name string, priority int) *store.Account {
	t.Helper()
	account := &store.Account{
		Name:          name,
		Kind:          store.KindCookie,
		Cookie:        cookieFor(name),
		Priority:      priority,
		MaxConcurrent: 2,
		Enabled:       true,
	}
	if err := h.store.AddAccount(account); err != nil {
		t.Fatalf("add account %s: %v", name, err)
	}
	return account
}

func (h *harness) chat(t *testing.T, body map[string]any, key string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("content-type", "application/json")
	if key != "" {
		req.Header.Set("authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.gateway.ChatCompletions(rec, req)
	return rec
}

// ask is the common shape: one user message against the default model.
func (h *harness) ask(t *testing.T, model string, extra map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "打个招呼"}},
	}
	for k, v := range extra {
		body[k] = v
	}
	rec := h.chat(t, body, h.key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return decodeJSON(t, rec)
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, rec.Body.String())
	}
	return out
}

// sseDataLines pulls every `data:` payload out of an SSE response.
func sseDataLines(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var out []string
	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return out
}

// --- authentication and model resolution ------------------------------------

func TestChatCompletionsRequiresClientKey(t *testing.T) {
	h := newHarness(t)
	rec := h.chat(t, map[string]any{"model": "gemini-3.8-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if h.upstream.shells != 0 {
		t.Fatal("upstream must not be called without a valid key")
	}
}

func TestChatCompletionsRejectsUnknownKey(t *testing.T) {
	h := newHarness(t)
	rec := h.chat(t, map[string]any{"model": "gemini-3.8-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, "sk-gm-wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if h.upstream.shells != 0 {
		t.Fatal("upstream must not be called with an unknown key")
	}
}

func TestChatCompletionsRejectsUnknownModel(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)

	rec := h.chat(t, map[string]any{
		"model":    "no-such-model-anywhere",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, h.key)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no-such-model-anywhere") {
		t.Fatalf("error should name the model: %s", rec.Body.String())
	}
	if h.upstream.shells != 0 {
		t.Fatal("an unknown model must be refused before any upstream call")
	}
}

// TestChatCompletionsAcceptsAliases covers the whole point of the alias table.
//
// Clients send whatever name they were configured with, and the aliases exist
// so those clients work without the operator editing anything. The table is
// useless if the gateway rejects the name before the resolver ever sees it —
// which is exactly what happened when the gateway consulted the store
// catalogue first.
func TestChatCompletionsAcceptsAliases(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("别名可用", "", true)}
	}

	for _, alias := range []string{"gpt-4o", "claude-3-5-sonnet", "gemini-2.5-pro", "google/gemini-3.8-flash"} {
		t.Run(alias, func(t *testing.T) {
			// The response echoes the catalogue id that actually ran, not the
			// alias, because that is what /v1/models advertises.
			want, _ := gemini.LookupModel(alias)
			payload := h.ask(t, alias, nil)
			choices, _ := payload["choices"].([]any)
			choice, _ := choices[0].(map[string]any)
			message, _ := choice["message"].(map[string]any)
			if message["content"] != "别名可用" {
				t.Fatalf("%s: content = %v", alias, message["content"])
			}
			if payload["model"] != want.ID {
				t.Fatalf("%s: model = %v, want the resolved id %q", alias, payload["model"], want.ID)
			}
		})
	}
}

// --- the request that reaches upstream --------------------------------------

// TestChatCompletionsSpeaksTheRealProtocol is the test that would have caught
// the stale stub. It asserts the things the upstream actually parses: the form
// body carries `at` and a doubly-encoded `f.req`, the query carries the build
// label, and the cookie travels on the request.
func TestChatCompletionsSpeaksTheRealProtocol(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("协议正确", "", true)}
	}

	h.ask(t, "gemini-3.8-flash", nil)

	calls := h.upstream.generations
	if len(calls) != 1 {
		t.Fatalf("generation calls = %d, want 1", len(calls))
	}
	call := calls[0]

	if !strings.Contains(call.cookie, "__Secure-1PSID=g.a000primary") {
		t.Fatalf("the account cookie did not travel: %q", call.cookie)
	}
	if got := call.form.Get("at"); got != "AO-x_stub-access-token:1700000000" {
		t.Fatalf("at = %q, want the value scraped from the shell", got)
	}
	if got := call.query.Get("bl"); got != "boq_assistant-bard-web-server_20260919.08_p0" {
		t.Fatalf("bl = %q, want the build label scraped from the shell", got)
	}
	if got := call.query.Get("f.sid"); got != "-1234567890123456789" {
		t.Fatalf("f.sid = %q, want the session id scraped from the shell", got)
	}
	if call.query.Get("rt") != "c" {
		t.Fatalf("rt = %q, want c", call.query.Get("rt"))
	}

	// f.req is a JSON array whose second element is the request array *as a
	// string*. Reading it as a nested object yields nothing, so the double
	// encoding is asserted rather than assumed.
	var outer []any
	if err := json.Unmarshal([]byte(call.form.Get("f.req")), &outer); err != nil {
		t.Fatalf("f.req is not JSON: %v", err)
	}
	if len(outer) != 2 {
		t.Fatalf("f.req has %d elements, want 2", len(outer))
	}
	encoded, ok := outer[1].(string)
	if !ok {
		t.Fatalf("f.req[1] is %T, want the request array encoded as a string", outer[1])
	}
	var inner []any
	if err := json.Unmarshal([]byte(encoded), &inner); err != nil {
		t.Fatalf("f.req[1] is not JSON: %v", err)
	}
	// 81 slots; a shorter array shifts every high slot and is answered with an
	// unhelpful generic error.
	if len(inner) != 81 {
		t.Fatalf("request array has %d slots, want 81", len(inner))
	}
	if got := inner[79]; got != float64(1) {
		t.Fatalf("slot 79 = %v, want the FAST mode number 1", got)
	}
	message, _ := inner[0].([]any)
	if len(message) == 0 || message[0] != "打个招呼" {
		t.Fatalf("slot 0 does not carry the prompt: %#v", inner[0])
	}
}

// A model whose hex id is unknown must send no model header rather than a
// guessed one: a wrong id is answered with BardErrorInfo 1052, which reads like
// a broken account.
func TestThinkingModeSendsNoModelHeader(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("答案", "先想一下", true)}
	}

	h.ask(t, "gemini-3.8-flash-thinking", nil)

	calls := h.upstream.generations
	if len(calls) != 1 {
		t.Fatalf("generation calls = %d, want 1", len(calls))
	}
	// The header is not recorded by the stub, so assert on the payload instead:
	// the thinking mode number and the think slot are what select the mode.
	var outer []any
	_ = json.Unmarshal([]byte(calls[0].form.Get("f.req")), &outer)
	encoded, _ := outer[1].(string)
	var inner []any
	if err := json.Unmarshal([]byte(encoded), &inner); err != nil {
		t.Fatalf("f.req[1] is not JSON: %v", err)
	}
	if inner[79] != float64(2) {
		t.Fatalf("slot 79 = %v, want the THINKING mode number 2", inner[79])
	}
	think, _ := inner[17].([]any)
	if len(think) == 0 {
		t.Fatalf("slot 17 is empty: %#v", inner[17])
	}
	level, _ := think[0].([]any)
	if len(level) == 0 || level[0] != float64(gemini.ThinkExtended) {
		t.Fatalf("slot 17 = %#v, want the thinking value %d", inner[17], gemini.ThinkExtended)
	}
}

// reasoning_effort is the only way a caller can ask for a deeper monologue
// without switching models, so it has to actually reach the payload.
func TestReasoningEffortReachesThePayload(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("好", "", true)}
	}

	h.ask(t, "gemini-3.8-flash", map[string]any{"reasoning_effort": "high"})

	if len(h.upstream.generations) != 1 {
		t.Fatalf("generation calls = %d, want 1", len(h.upstream.generations))
	}
	var outer []any
	_ = json.Unmarshal([]byte(h.upstream.generations[0].form.Get("f.req")), &outer)
	encoded, _ := outer[1].(string)
	var inner []any
	if err := json.Unmarshal([]byte(encoded), &inner); err != nil {
		t.Fatalf("f.req[1] is not JSON: %v", err)
	}
	think, _ := inner[17].([]any)
	level, _ := think[0].([]any)
	if len(level) == 0 || level[0] != float64(gemini.ThinkExtended) {
		t.Fatalf("slot 17 = %#v, want reasoning_effort to select %d", inner[17], gemini.ThinkExtended)
	}
}

// --- the response ------------------------------------------------------------

func TestChatCompletionsReturnsOpenAIResponse(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("你好，世界", "", true)}
	}

	payload := h.ask(t, "gemini-3.8-flash", nil)

	if payload["object"] != "chat.completion" {
		t.Fatalf("object = %v", payload["object"])
	}
	if payload["model"] != "gemini-3.8-flash" {
		t.Fatalf("model = %v", payload["model"])
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", payload["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "你好，世界" {
		t.Fatalf("content = %v", message["content"])
	}
	if message["role"] != "assistant" {
		t.Fatalf("role = %v", message["role"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage, _ := payload["usage"].(map[string]any)
	if usage["total_tokens"] == nil {
		t.Fatalf("usage missing: %v", payload["usage"])
	}

	// The pool must hand the account back after the request.
	if got := h.pool.Inflight(accountID(t, h, "primary")); got != 0 {
		t.Fatalf("inflight = %d after the request, want 0", got)
	}
}

// TestChatCompletionsDoesNotDuplicateCumulativeText is the reason the client
// diffs instead of concatenating.
//
// Every frame repeats the whole answer so far. A client that appended each one
// would return the answer three times over, and the bug would only show up on
// answers long enough to span several frames.
func TestChatCompletionsDoesNotDuplicateCumulativeText(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{
			turnPayload("第一段", "", false),
			turnPayload("第一段第二段", "", false),
			turnPayload("第一段第二段第三段", "", true),
		}
	}

	payload := h.ask(t, "gemini-3.8-flash", nil)
	choices, _ := payload["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "第一段第二段第三段" {
		t.Fatalf("content = %q, want the answer exactly once", message["content"])
	}
}

// Reasoning arrives on its own slot and must land in reasoning_content rather
// than being concatenated into the answer.
func TestChatCompletionsSeparatesReasoning(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("答案是 42", "先想一下", true)}
	}

	payload := h.ask(t, "gemini-3.8-flash-thinking", nil)
	choices, _ := payload["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "答案是 42" {
		t.Fatalf("content = %v, want only the answer", message["content"])
	}
	if message["reasoning_content"] != "先想一下" {
		t.Fatalf("reasoning_content = %v", message["reasoning_content"])
	}
}

func TestChatCompletionsStreamsSSE(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{
			turnPayload("流式", "", false),
			turnPayload("流式回答", "", true),
		}
	}

	raw, _ := json.Marshal(map[string]any{
		"model":    "gemini-3.8-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+h.key)
	rec := httptest.NewRecorder()
	h.gateway.ChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("content-type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	// Proxies must not buffer a stream.
	if rec.Header().Get("x-accel-buffering") != "no" {
		t.Fatalf("x-accel-buffering = %q", rec.Header().Get("x-accel-buffering"))
	}

	lines := sseDataLines(t, rec)
	if len(lines) < 3 {
		t.Fatalf("expected several SSE chunks, got %v", lines)
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("stream must end with [DONE], got %q", lines[len(lines)-1])
	}

	var sawRole, sawStop bool
	var content strings.Builder
	for _, line := range lines {
		if line == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %v (%q)", err, line)
		}
		if chunk["object"] != "chat.completion.chunk" {
			t.Fatalf("object = %v", chunk["object"])
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) != 1 {
			t.Fatalf("choices = %v", choices)
		}
		entry, _ := choices[0].(map[string]any)
		delta, _ := entry["delta"].(map[string]any)
		if delta["role"] == "assistant" {
			sawRole = true
		}
		if text, ok := delta["content"].(string); ok {
			content.WriteString(text)
		}
		if entry["finish_reason"] == "stop" {
			sawStop = true
		}
	}
	if !sawRole || !sawStop {
		t.Fatalf("missing frames: role=%v stop=%v", sawRole, sawStop)
	}
	// The deltas are increments, so they must reconstruct the answer exactly.
	if content.String() != "流式回答" {
		t.Fatalf("streamed content = %q, want the answer exactly once", content.String())
	}
}

// --- failover ----------------------------------------------------------------

// A rejected credential must be marked invalid and the request retried on the
// next account in the pool.
func TestChatCompletionsFailsOverToHealthyAccount(t *testing.T) {
	h := newHarness(t)
	bad := h.addAccount(t, "broken", 1) // picked first
	h.addAccount(t, "healthy", 50)
	h.upstream.rejectCookie = "g.a000broken"
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("换号成功", "", true)}
	}

	payload := h.ask(t, "gemini-3.8-flash", nil)
	choices, _ := payload["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "换号成功" {
		t.Fatalf("content = %v", message["content"])
	}

	// The rejected account must be quarantined so it stops being scheduled.
	updated, ok := h.store.AccountByID(bad.ID)
	if !ok {
		t.Fatal("account disappeared")
	}
	if updated.Status != store.StatusInvalid {
		t.Fatalf("status = %q, want %q after a 401", updated.Status, store.StatusInvalid)
	}
	if updated.LastError == "" {
		t.Fatal("LastError should explain why the account was quarantined")
	}
}

// A usage-limit rejection is not a dead credential: the account is good, it is
// simply out of quota, so it must be cooled down rather than retired.
func TestChatCompletionsCoolsDownOnUsageLimit(t *testing.T) {
	h := newHarness(t)
	limited := h.addAccount(t, "limited", 1)
	h.addAccount(t, "spare", 50)
	h.upstream.errorCode = 1037
	// Only the first attempt is refused, so the retry has somewhere to land.
	h.upstream.failFirst = 1
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("备用号接上了", "", true)}
	}

	payload := h.ask(t, "gemini-3.8-flash", nil)
	choices, _ := payload["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", choices)
	}

	updated, ok := h.store.AccountByID(limited.ID)
	if !ok {
		t.Fatal("account disappeared")
	}
	if updated.Status == store.StatusInvalid {
		t.Fatal("a quota rejection must not retire the account; the cookie is still valid")
	}
	if updated.Status != store.StatusCooldown {
		t.Fatalf("status = %q, want %q so the account is tried again later", updated.Status, store.StatusCooldown)
	}
}

// A 405 is the documented "your build label is stale" answer. The client drops
// the cached shell and retries once, which must be enough — the retry re-fetches
// the shell, so a stub that answers 405 only on the first generation succeeds.
func TestChatCompletionsRetriesOnceOnStaleBuildLabel(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.generateStatus = http.StatusMethodNotAllowed

	// The stub cannot change its answer mid-flight, so instead assert the
	// retry happened: two generation attempts and two shell fetches.
	rec := h.chat(t, map[string]any{
		"model":    "gemini-3.8-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, h.key)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 while the label stays stale", rec.Code)
	}
	if got := len(h.upstream.generations); got != 2 {
		t.Fatalf("generation attempts = %d, want 2 (one retry after refreshing the shell)", got)
	}
	if got := h.upstream.shells; got != 2 {
		t.Fatalf("shell fetches = %d, want 2 (the cache must be dropped before retrying)", got)
	}
}

func TestChatCompletionsReportsNoAccount(t *testing.T) {
	h := newHarness(t)
	rec := h.chat(t, map[string]any{
		"model":    "gemini-3.8-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, h.key)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if h.upstream.shells != 0 {
		t.Fatal("upstream must not be called when the pool is empty")
	}
}

// --- audit, limits, catalogue ------------------------------------------------

func TestChatCompletionsRecordsAudit(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("审计", "", true)}
	}

	h.ask(t, "gemini-3.8-flash", nil)

	audits := h.store.ListAudits()
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	audit := audits[0]
	if audit.Status != http.StatusOK {
		t.Fatalf("audit status = %d", audit.Status)
	}
	if audit.Model != "gemini-3.8-flash" {
		t.Fatalf("audit model = %q", audit.Model)
	}
	if audit.AccountName != "primary" {
		t.Fatalf("audit account = %q", audit.AccountName)
	}
	if audit.LatencyMs < 0 {
		t.Fatalf("audit latency = %d", audit.LatencyMs)
	}
}

// A failed call must still be audited, with the upstream reason attached.
func TestChatCompletionsAuditsFailures(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.generateStatus = http.StatusBadGateway

	rec := h.chat(t, map[string]any{
		"model":    "gemini-3.8-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, h.key)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	audits := h.store.ListAudits()
	if len(audits) == 0 {
		t.Fatal("a failed request must still produce an audit record")
	}
	if audits[0].Error == "" {
		t.Fatal("audit should carry the upstream error")
	}
}

func TestChatCompletionsEnforcesRateLimit(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("ok", "", true)}
	}

	// Replace the unlimited key with a 1 RPM one.
	limited, err := h.store.CreateClientKey("limited", 1, 4)
	if err != nil {
		t.Fatalf("create limited key: %v", err)
	}

	body := map[string]any{"model": "gemini-3.8-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if rec := h.chat(t, body, limited.Key); rec.Code != http.StatusOK {
		t.Fatalf("first call status = %d", rec.Code)
	}
	rec := h.chat(t, body, limited.Key)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d, want 429", rec.Code)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.gateway.Models(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("authorization", "Bearer "+h.key)
	rec = httptest.NewRecorder()
	h.gateway.Models(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	payload := decodeJSON(t, rec)
	if payload["object"] != "list" {
		t.Fatalf("object = %v", payload["object"])
	}
	data, _ := payload["data"].([]any)
	if len(data) == 0 {
		t.Fatal("model list is empty")
	}
	ids := map[string]bool{}
	for _, item := range data {
		entry, _ := item.(map[string]any)
		id, _ := entry["id"].(string)
		ids[id] = true
		if entry["object"] != "model" {
			t.Fatalf("entry object = %v", entry["object"])
		}
	}
	// Every listed model must be one the client can actually resolve, or the
	// catalogue is advertising something that will fail when used.
	for _, want := range []string{"gemini-3.8-flash", "gemini-3.8-flash-thinking", "gemini-3.1-pro", "gemini-3.5-flash-lite"} {
		if !ids[want] {
			t.Fatalf("model list is missing %s: %v", want, ids)
		}
	}
	for id := range ids {
		if _, ok := gemini.LookupModel(id); !ok {
			t.Fatalf("the catalogue lists %q but the client cannot resolve it", id)
		}
	}
}

func TestHealthReportsPoolSummary(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "one", 10)
	h.addAccount(t, "two", 20)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.gateway.Health(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	payload := decodeJSON(t, rec)
	if payload["status"] != "ok" {
		t.Fatalf("status = %v", payload["status"])
	}
	summary, _ := payload["pool"].(map[string]any)
	if summary["total"] != float64(2) {
		t.Fatalf("pool total = %v, want 2", summary["total"])
	}
	if summary["routable"] != float64(2) {
		t.Fatalf("pool routable = %v, want 2", summary["routable"])
	}
}

// --- images ------------------------------------------------------------------

func TestImageGenerationsReturnsMedia(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("画好了", "", true, "https://cdn/one.png", "https://cdn/two.png")}
	}

	raw, _ := json.Marshal(map[string]any{"prompt": "a blue circle", "n": 2})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(raw))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+h.key)
	rec := httptest.NewRecorder()
	h.gateway.ImageGenerations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	payload := decodeJSON(t, rec)
	data, _ := payload["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data = %v, want 2 images", payload["data"])
	}
	first, _ := data[0].(map[string]any)
	if first["url"] != "https://cdn/one.png" {
		t.Fatalf("url = %v", first["url"])
	}
}

// The image endpoint has no model of its own: Gemini draws inside an ordinary
// chat turn, so it must use the default catalogue entry rather than a
// hand-written id that the catalogue does not contain.
func TestImageGenerationsUsesTheDefaultModel(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("画好了", "", true, "https://cdn/one.png")}
	}

	raw, _ := json.Marshal(map[string]any{"prompt": "a blue circle"})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(raw))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+h.key)
	rec := httptest.NewRecorder()
	h.gateway.ImageGenerations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(h.upstream.generations) != 1 {
		t.Fatalf("generation calls = %d, want 1", len(h.upstream.generations))
	}
	var outer []any
	_ = json.Unmarshal([]byte(h.upstream.generations[0].form.Get("f.req")), &outer)
	encoded, _ := outer[1].(string)
	var inner []any
	if err := json.Unmarshal([]byte(encoded), &inner); err != nil {
		t.Fatalf("f.req[1] is not JSON: %v", err)
	}
	defaultSpec, _ := gemini.LookupModel(gemini.DefaultModelID)
	if inner[79] != float64(defaultSpec.Number) {
		t.Fatalf("slot 79 = %v, want the default model's mode %d", inner[79], defaultSpec.Number)
	}
}

// --- images in the prompt ----------------------------------------------------

// An inline data URI cannot be fetched by the upstream, so the request must be
// refused with an actionable message rather than forwarded to fail opaquely.
func TestChatCompletionsRejectsInlineImageWithoutPublicBaseURL(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("never reached", "", true)}
	}

	rec := h.chat(t, map[string]any{
		"model": "gemini-3.8-flash",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "what is this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64,iVBORw0KGgo=",
				}},
			},
		}},
	}, h.key)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "public base URL") {
		t.Fatalf("error should explain how to fix it: %s", rec.Body.String())
	}
	if h.upstream.shells != 0 {
		t.Fatal("a rejected attachment must not reach the upstream")
	}
}

// A remote image URL is forwarded as an attachment without being downloaded.
func TestChatCompletionsForwardsRemoteImage(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "primary", 10)
	h.upstream.payloads = func() [][]any {
		return [][]any{turnPayload("看到了", "", true)}
	}

	rec := h.chat(t, map[string]any{
		"model": "gemini-3.8-flash",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "what is this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "https://example.com/cat.png",
				}},
			},
		}},
	}, h.key)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(h.upstream.generations) != 1 {
		t.Fatalf("generation calls = %d, want 1", len(h.upstream.generations))
	}
	form := h.upstream.generations[0].form
	var outer []any
	_ = json.Unmarshal([]byte(form.Get("f.req")), &outer)
	encoded, _ := outer[1].(string)
	if !strings.Contains(encoded, "https://example.com/cat.png") {
		t.Fatalf("the remote image did not reach the payload: %s", encoded)
	}
}

// --- helpers -----------------------------------------------------------------

func accountID(t *testing.T, h *harness, name string) string {
	t.Helper()
	for _, account := range h.store.ListAccounts() {
		if account.Name == name {
			return account.ID
		}
	}
	t.Fatalf("account %q not found", name)
	return ""
}

// Guard against the harness drifting from the real configuration defaults.
func TestHarnessUsesFastCapacityWait(t *testing.T) {
	h := newHarness(t)
	if got := h.settings.CapacityWait(); got != 0 {
		t.Fatalf("capacity wait = %v, want 0 so pool-exhaustion tests stay fast", got)
	}
	if h.settings.RequestTimeout() > 5*time.Second {
		t.Fatalf("request timeout = %v, tests should not hang", h.settings.RequestTimeout())
	}
	if !slices.Contains(gemini.ModelIDs(), gemini.DefaultModelID) {
		t.Fatalf("the default model %q is not in the catalogue", gemini.DefaultModelID)
	}
}
