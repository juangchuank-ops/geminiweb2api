package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"geminiweb2api/internal/config"
	"geminiweb2api/internal/gemini"
	"geminiweb2api/internal/pool"
	"geminiweb2api/internal/store"
)

// Version is reported by /health.
const Version = "0.1.0"

// Gateway exposes the OpenAI-compatible surface.
type Gateway struct {
	store    *store.Store
	pool     *pool.Pool
	client   *gemini.Client
	settings func() config.Settings
	limiter  *limiter
}

func New(st *store.Store, p *pool.Pool, client *gemini.Client, settings func() config.Settings) *Gateway {
	return &Gateway{
		store:    st,
		pool:     p,
		client:   client,
		settings: settings,
		limiter:  newLimiter(),
	}
}

// respWriter records whether the response has already been committed.
type respWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *respWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *respWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *respWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// ------------------------------------------------------------- chat handler

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	User     string        `json:"user"`
	// ReasoningEffort is OpenAI's spelling of "think harder". It overrides the
	// reasoning depth the chosen model would use on its own, which is the only
	// way a caller can ask for a deeper monologue without switching models.
	ReasoningEffort string `json:"reasoning_effort"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// ChatCompletions implements POST /v1/chat/completions.
func (g *Gateway) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	out := &respWriter{ResponseWriter: w}
	started := time.Now()

	key, err := g.authenticate(r)
	if err != nil {
		writeError(out, http.StatusUnauthorized, err.Error())
		return
	}
	if err := g.limiter.acquire(key); err != nil {
		writeError(out, http.StatusTooManyRequests, err.Error())
		return
	}
	defer g.limiter.release(key)

	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeError(out, http.StatusBadRequest, "cannot read request body")
		return
	}

	var request chatRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(out, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Model resolution goes through the client's catalogue first, because the
	// console's catalogue is keyed on the ids the *client* understands. Asking
	// the store directly would reject every name the alias table exists to
	// accept — `gpt-4o`, `gemini-2.5-pro`, `deepseek-reasoner` — and refusing
	// those while /v1/models advertises gemini-flash is the most common way a
	// 2api appears broken for a reason the operator cannot see.
	requested := strings.TrimSpace(request.Model)
	spec, understood := gemini.ResolveModel(requested)
	if !understood && requested != "" {
		writeError(out, http.StatusBadRequest,
			fmt.Sprintf("unknown model %q; see /v1/models for the catalogue", requested))
		return
	}

	model, ok := g.store.ModelByID(spec.ID)
	if !ok || !model.Enabled {
		writeError(out, http.StatusBadRequest, fmt.Sprintf("unknown or disabled model %q", spec.ID))
		return
	}
	if model.Type != "chat" {
		writeError(out, http.StatusBadRequest, fmt.Sprintf("model %q is not a chat model", spec.ID))
		return
	}

	prompt, images, err := g.buildPrompt(r.Context(), request.Messages)
	if err != nil {
		writeError(out, http.StatusBadRequest, err.Error())
		return
	}
	if prompt == "" && len(images) == 0 {
		writeError(out, http.StatusBadRequest, "messages must contain text or image content")
		return
	}

	// The reasoning depth normally comes from the model the caller picked —
	// the catalogue encodes which modes think — and `reasoning_effort` only
	// overrides it when the caller actually has an opinion.
	thinkMode := reasoningDepth(request.ReasoningEffort)

	settings := g.settings()
	var (
		lastErr      error
		accountName  string
		result       *gemini.Result
		firstToken   int64
		retries      int
		streamedFlag bool
	)

	for attempt := 0; attempt < settings.Routing.MaxAttempts; attempt++ {
		lease, err := g.pool.Acquire(r.Context(), request.User)
		if err != nil {
			lastErr = err
			break
		}
		accountName = displayName(lease.Account)
		if attempt > 0 {
			retries = attempt
		}

		opts := gemini.Options{
			Credential:  CredentialOf(lease.Account),
			Text:        prompt,
			Model:       spec.ID,
			ThinkMode:   thinkMode,
			Images:      images,
			Timeout:     settings.RequestTimeout(),
			IdleTimeout: settings.StreamIdleTimeout(),
		}

		if request.Stream {
			var streamed bool
			result, firstToken, streamed, err = g.streamCompletion(out, r, opts, model, started)
			streamedFlag = streamed
		} else {
			result, err = g.client.Completion(r.Context(), opts)
		}
		lease.Release(err)

		lastErr = err
		if err == nil {
			break
		}
		if r.Context().Err() != nil || out.wroteHeader {
			break
		}
		if isPoolExhausted(err) {
			break
		}
	}

	promptTokens := estimateTokens(prompt)
	if lastErr != nil {
		g.recordAudit(r, key, model, accountName, started, http.StatusBadGateway, 0, promptTokens, 0, request.Stream, retries, string(body), "", lastErr.Error())
		if out.wroteHeader {
			writeSSEError(out, lastErr.Error())
			return
		}
		writeError(out, http.StatusBadGateway, lastErr.Error())
		return
	}
	if result == nil {
		writeError(out, http.StatusBadGateway, "upstream returned no result")
		return
	}

	completionTokens := estimateTokens(result.Text + result.Thinking)
	g.store.RecordModelUsage(model.ID, 1, int64(promptTokens+completionTokens))
	g.store.BumpClientKeyUsage(key.ID)

	if request.Stream {
		mediaURLs := g.persistMediaList(result.Media, prompt, model.ID, accountName)
		g.finishStream(out, result, model, mediaURLs, streamedFlag)
		g.recordAudit(r, key, model, accountName, started, http.StatusOK, firstToken, promptTokens, completionTokens, true, retries, string(body), result.Text, "")
		return
	}

	response := map[string]any{
		"id":      "chatcmpl-" + randomID(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model.ID,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           result.Text,
					"reasoning_content": result.Thinking,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
	if len(result.Media) > 0 {
		response["media"] = g.persistMediaList(result.Media, prompt, model.ID, accountName)
	}

	raw, _ := json.Marshal(response)
	g.recordAudit(r, key, model, accountName, started, http.StatusOK, firstToken, promptTokens, completionTokens, false, retries, string(body), string(raw), "")
	out.Header().Set("content-type", "application/json")
	out.WriteHeader(http.StatusOK)
	_, _ = out.Write(raw)
}

// streamCompletion forwards upstream deltas as OpenAI SSE chunks.
func (g *Gateway) streamCompletion(
	out *respWriter,
	r *http.Request,
	opts gemini.Options,
	model *store.ModelConfig,
	started time.Time,
) (*gemini.Result, int64, bool, error) {
	var mu sync.Mutex
	var firstToken int64
	var streamed bool
	id := "chatcmpl-" + randomID(12)
	created := time.Now().Unix()

	writeChunk := func(delta map[string]any, finish any) {
		mu.Lock()
		defer mu.Unlock()
		if !out.wroteHeader {
			out.Header().Set("content-type", "text/event-stream")
			out.Header().Set("cache-control", "no-cache")
			out.Header().Set("connection", "keep-alive")
			out.Header().Set("x-accel-buffering", "no")
			out.WriteHeader(http.StatusOK)
		}
		payload := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model.ID,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		raw, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(out, "data: %s\n\n", raw)
		out.Flush()
	}

	writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)

	opts.OnDelta = func(text string) {
		mu.Lock()
		if firstToken == 0 {
			firstToken = time.Since(started).Milliseconds()
		}
		streamed = true
		mu.Unlock()
		writeChunk(map[string]any{"content": text}, nil)
	}
	opts.OnThinking = func(text string) {
		writeChunk(map[string]any{"reasoning_content": text}, nil)
	}

	result, err := g.client.Completion(r.Context(), opts)
	if err != nil {
		return result, firstToken, streamed, err
	}
	if firstToken == 0 {
		firstToken = time.Since(started).Milliseconds()
	}
	return result, firstToken, streamed, nil
}

// finishStream terminates the SSE response, emitting any content that was not
// already streamed as deltas (media URLs, and text when the upstream produced
// no incremental frames).
func (g *Gateway) finishStream(out *respWriter, result *gemini.Result, model *store.ModelConfig, mediaURLs []string, streamed bool) {
	if !out.wroteHeader {
		out.Header().Set("content-type", "text/event-stream")
		out.Header().Set("cache-control", "no-cache")
		out.Header().Set("connection", "keep-alive")
		out.Header().Set("x-accel-buffering", "no")
		out.WriteHeader(http.StatusOK)
	}
	if !streamed {
		if result.Thinking != "" {
			writeChunkRaw(out, model, map[string]any{"reasoning_content": result.Thinking}, nil)
		}
		if result.Text != "" {
			writeChunkRaw(out, model, map[string]any{"content": result.Text}, nil)
		}
	}
	if len(mediaURLs) > 0 {
		writeChunkRaw(out, model, map[string]any{"media": mediaURLs}, nil)
	}
	writeChunkRaw(out, model, map[string]any{}, "stop")
	_, _ = io.WriteString(out, "data: [DONE]\n\n")
	out.Flush()
}

func writeChunkRaw(out *respWriter, model *store.ModelConfig, delta map[string]any, finish any) {
	payload := map[string]any{
		"id":      "chatcmpl-" + randomID(12),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model.ID,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	raw, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(out, "data: %s\n\n", raw)
}

// ------------------------------------------------------------ prompt build

func (g *Gateway) buildPrompt(ctx context.Context, messages []chatMessage) (string, []gemini.UploadedImage, error) {
	if len(messages) == 0 {
		return "", nil, nil
	}
	var builder strings.Builder
	var images []gemini.UploadedImage

	for _, message := range messages {
		text, urls := parseContent(message.Content)
		switch message.Role {
		case "system":
			builder.WriteString("[系统指令] " + text + "\n\n")
		case "assistant":
			builder.WriteString("助手：" + text + "\n")
		default:
			if len(messages) > 1 {
				builder.WriteString("用户：" + text + "\n")
			} else {
				builder.WriteString(text + "\n")
			}
		}

		for _, raw := range urls {
			image, err := g.resolveImage(raw)
			if err != nil {
				return "", nil, err
			}
			images = append(images, image)
		}
	}

	return strings.TrimSpace(builder.String()), images, nil
}

// resolveImage turns an OpenAI image reference into something the agent can
// fetch on its own.
//
// Remote URLs pass through untouched. Inline data URIs are written into the
// media directory and republished under Media.PublicBaseURL, because the agent
// downloads attachments server-side and cannot dereference a data: URI. When no
// public base URL is configured the request is rejected with an actionable
// message rather than being forwarded to fail opaquely upstream.
func (g *Gateway) resolveImage(raw string) (gemini.UploadedImage, error) {
	if !strings.HasPrefix(raw, "data:") {
		return gemini.UploadedImage{URL: raw, Name: imageName(raw)}, nil
	}

	index := strings.Index(raw, ",")
	if index < 0 {
		return gemini.UploadedImage{}, fmt.Errorf("invalid data URI")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw[index+1:]))
	if err != nil {
		return gemini.UploadedImage{}, fmt.Errorf("invalid base64 image payload: %w", err)
	}

	settings := g.settings()
	if strings.TrimSpace(settings.Media.PublicBaseURL) == "" {
		return gemini.UploadedImage{}, fmt.Errorf("inline base64 images require the media public base URL to be configured; otherwise send a publicly reachable image URL")
	}
	if err := os.MkdirAll(settings.Media.GeneratedDir, 0o755); err != nil {
		return gemini.UploadedImage{}, fmt.Errorf("prepare media directory: %w", err)
	}

	name := "inline_" + randomID(12) + ".png"
	target := filepath.Join(settings.Media.GeneratedDir, name)
	if err := os.WriteFile(target, decoded, 0o644); err != nil {
		return gemini.UploadedImage{}, fmt.Errorf("store inline image: %w", err)
	}
	return gemini.UploadedImage{
		URL:  strings.TrimRight(settings.Media.PublicBaseURL, "/") + "/" + name,
		Name: name,
	}, nil
}

// imageName derives a filename from a URL, falling back to a stable default.
func imageName(raw string) string {
	index := strings.LastIndex(raw, "/")
	if index < 0 {
		return "image.png"
	}
	candidate := raw[index+1:]
	if query := strings.Index(candidate, "?"); query >= 0 {
		candidate = candidate[:query]
	}
	if candidate == "" {
		return "image.png"
	}
	return candidate
}

func parseContent(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil
	}
	var text strings.Builder
	var urls []string
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text":
			text.WriteString(part.Text)
		case "image_url", "input_image":
			if part.ImageURL.URL != "" {
				urls = append(urls, part.ImageURL.URL)
			}
		}
	}
	return text.String(), urls
}

// ------------------------------------------------------------ image handler

type imageRequest struct {
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	ResponseFormat string `json:"response_format"`
	Model          string `json:"model"`
}

// ImageGenerations implements POST /v1/images/generations.
func (g *Gateway) ImageGenerations(w http.ResponseWriter, r *http.Request) {
	out := &respWriter{ResponseWriter: w}
	started := time.Now()

	key, err := g.authenticate(r)
	if err != nil {
		writeError(out, http.StatusUnauthorized, err.Error())
		return
	}
	if err := g.limiter.acquire(key); err != nil {
		writeError(out, http.StatusTooManyRequests, err.Error())
		return
	}
	defer g.limiter.release(key)

	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	var request imageRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(out, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(request.Prompt) == "" {
		writeError(out, http.StatusBadRequest, "prompt is required")
		return
	}

	// There is no separate image model. Gemini's web UI draws images inside an
	// ordinary chat turn — the request is text and the answer carries media —
	// so this endpoint reuses the default chat mode rather than a catalogue
	// entry of its own. A dedicated entry would be a model the console lists
	// and the client cannot resolve.
	model, ok := g.store.ModelByID(gemini.DefaultModelID)
	if !ok || !model.Enabled {
		writeError(out, http.StatusServiceUnavailable, "image generation is disabled")
		return
	}

	settings := g.settings()
	var (
		lastErr     error
		accountName string
		result      *gemini.Result
	)

	for attempt := 0; attempt < settings.Routing.MaxAttempts; attempt++ {
		lease, err := g.pool.Acquire(r.Context(), "")
		if err != nil {
			lastErr = err
			break
		}
		accountName = displayName(lease.Account)
		result, err = g.client.Completion(r.Context(), gemini.Options{
			Credential:  CredentialOf(lease.Account),
			Text:        request.Prompt,
			Model:       gemini.DefaultModelID,
			Timeout:     settings.RequestTimeout(),
			IdleTimeout: settings.StreamIdleTimeout(),
		})
		lease.Release(err)
		lastErr = err
		if err == nil || isPoolExhausted(err) {
			break
		}
	}

	if lastErr != nil {
		g.recordAudit(r, key, model, accountName, started, http.StatusBadGateway, 0, 0, 0, false, 0, string(body), "", lastErr.Error())
		writeError(out, http.StatusBadGateway, lastErr.Error())
		return
	}

	urls := g.persistMediaList(result.Media, request.Prompt, model.ID, accountName)
	items := make([]any, 0, len(urls))
	for index, url := range urls {
		if request.ResponseFormat == "b64_json" {
			path := filepath.Join(settings.Media.GeneratedDir, filepath.Base(url))
			if data, err := os.ReadFile(path); err == nil {
				items = append(items, map[string]any{"b64_json": base64.StdEncoding.EncodeToString(data)})
				continue
			}
		}
		source := ""
		if index < len(result.Media) {
			source = result.Media[index].URL
		}
		items = append(items, map[string]any{"url": url, "source_url": source})
	}

	g.store.RecordModelUsage(model.ID, 1, 0)
	g.store.BumpClientKeyUsage(key.ID)
	response := map[string]any{"created": time.Now().Unix(), "data": items}
	raw, _ := json.Marshal(response)
	g.recordAudit(r, key, model, accountName, started, http.StatusOK, 0, 0, 0, false, 0, string(body), string(raw), "")

	out.Header().Set("content-type", "application/json")
	out.WriteHeader(http.StatusOK)
	_, _ = out.Write(raw)
}

// Models implements GET /v1/models.
func (g *Gateway) Models(w http.ResponseWriter, r *http.Request) {
	if _, err := g.authenticate(r); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	data := make([]any, 0, 4)
	for _, model := range g.store.ListModels() {
		if !model.Enabled {
			continue
		}
		data = append(data, map[string]any{"id": model.ID, "object": "model", "created": 0, "owned_by": "gemini"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// Health implements GET /health.
func (g *Gateway) Health(w http.ResponseWriter, r *http.Request) {
	total, active, cooldown, disabled, invalid, routable := g.pool.Summary()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": Version,
		"pool": map[string]any{
			"total": total, "active": active, "cooldown": cooldown,
			"disabled": disabled, "invalid": invalid, "routable": routable,
		},
	})
}

// ------------------------------------------------------------- media helper

func (g *Gateway) persistMediaList(media []gemini.MediaRef, prompt, model, account string) []string {
	urls := make([]string, 0, len(media))
	for _, item := range media {
		urls = append(urls, g.persistMedia(item, prompt, model, account))
	}
	return urls
}

func (g *Gateway) persistMedia(media gemini.MediaRef, prompt, model, account string) string {
	settings := g.settings()
	item := &store.MediaItem{
		ID:          "media_" + randomID(8),
		Kind:        media.Kind,
		SourceURL:   media.URL,
		URL:         media.URL,
		Prompt:      prompt,
		Model:       model,
		AccountName: account,
		CreatedAt:   time.Now(),
	}

	if settings.Media.AutoDownload {
		extension := ".bin"
		switch media.Kind {
		case "image":
			extension = ".png"
		case "video":
			extension = ".mp4"
		}
		filename := item.ID + extension
		target := filepath.Join(settings.Media.GeneratedDir, filename)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, media.URL, nil)
		if err == nil {
			if resp, err := http.DefaultClient.Do(req); err == nil {
				if resp.StatusCode < 400 {
					if file, err := os.Create(target); err == nil {
						if _, err := io.Copy(file, io.LimitReader(resp.Body, 256<<20)); err == nil {
							item.URL = "/media/" + filename
						} else {
							_ = os.Remove(target)
						}
						_ = file.Close()
					}
				}
				_ = resp.Body.Close()
			}
		}
		cancel()
	}

	g.store.AddMedia(item)
	return item.URL
}

// --------------------------------------------------------------- audit glue

func (g *Gateway) recordAudit(
	r *http.Request,
	key *store.ClientKey,
	model *store.ModelConfig,
	accountName string,
	started time.Time,
	status int,
	firstTokenMs int64,
	promptTokens, completionTokens int,
	stream bool,
	retries int,
	requestBody, responseBody, errText string,
) {
	settings := g.settings()
	audit := &store.Audit{
		ID:               "req_" + randomID(10),
		CreatedAt:        time.Now(),
		KeyName:          key.Name,
		Model:            model.ID,
		AccountName:      accountName,
		Status:           status,
		LatencyMs:        time.Since(started).Milliseconds(),
		FirstTokenMs:     firstTokenMs,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		Stream:           stream,
		Retries:          retries,
		IP:               clientIP(r),
		UserAgent:        r.Header.Get("user-agent"),
		Error:            errText,
	}
	if settings.Audit.RecordBody {
		audit.RequestBody = clampBody(requestBody, settings.Audit.BodyLimitBytes)
		audit.ResponseBody = clampBody(responseBody, settings.Audit.BodyLimitBytes)
	}
	g.store.AppendAudit(audit)
}

// reasoningDepth maps OpenAI's `reasoning_effort` onto the payload's think
// slot, returning nil when the caller has no opinion.
//
// The mapping is deliberately binary. Captured traffic only ever uses two
// values in slot 17 — 4 for an ordinary mode and 0 for a thinking mode — so
// there is no ladder of depths to climb: asking for more reasoning means
// switching to a thinking mode, and `reasoning_effort` can only request that
// switch or decline it. Inventing intermediate values would be sending the
// upstream numbers nothing has ever been observed to accept.
func reasoningDepth(effort string) *int {
	extended := gemini.ThinkExtended
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "":
		return nil
	case "none", "minimal", "low":
		// Explicitly declining: keep the model's ordinary depth.
		plain := gemini.ThinkDefault
		return &plain
	default:
		// medium / high / anything else the caller invented.
		return &extended
	}
}

// clampBody keeps an audited body within the configured limit.
func clampBody(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// ------------------------------------------------------------------ helpers

func (g *Gateway) authenticate(r *http.Request) (*store.ClientKey, error) {
	raw := strings.TrimSpace(r.Header.Get("authorization"))
	if raw == "" {
		raw = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if len(raw) > 7 && strings.EqualFold(raw[:7], "bearer ") {
		raw = strings.TrimSpace(raw[7:])
	}
	if raw == "" {
		return nil, fmt.Errorf("missing API key")
	}
	key, ok := g.store.ClientKeyByValue(raw)
	if !ok {
		return nil, fmt.Errorf("invalid API key")
	}
	if !key.Enabled {
		return nil, fmt.Errorf("API key disabled")
	}
	return key, nil
}

// CredentialOf projects a pooled account into the upstream identity bundle.
// It is exported because the admin console probes accounts with the same
// mapping, and a second copy of these fields would be a second place to forget
// when the protocol grows one.
func CredentialOf(account *store.Account) gemini.Credential {
	return gemini.Credential{
		Cookie:   account.Cookie,
		PSID:     account.PSID,
		PSIDTS:   account.PSIDTS,
		AuthUser: account.AuthUser,
		Model:    account.Model,
		BaseURL:  account.BaseURL,
	}
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

func isPoolExhausted(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "no account available") || strings.Contains(message, "all accounts are busy")
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("x-forwarded-for"); forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	host := r.RemoteAddr
	if index := strings.LastIndex(host, ":"); index >= 0 {
		host = host[:index]
	}
	return host
}

func randomID(length int) string {
	buf := make([]byte, (length+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)[:length]
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return len([]rune(text))/2 + 1
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
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": "geminiweb2api_error"}})
}

func writeSSEError(w http.ResponseWriter, message string) {
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
}
