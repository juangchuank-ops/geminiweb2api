package gemini

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildInnerSlots(t *testing.T) {
	spec, _ := ResolveModel("gemini-3.8-flash")
	inner := buildInner("hello", "en", spec, 0, nil, nil)

	if len(inner) != innerSlots {
		t.Fatalf("inner length = %d, want %d", len(inner), innerSlots)
	}

	message, ok := inner[slotMessage].([]any)
	if !ok || len(message) != 7 {
		t.Fatalf("slot 0 = %#v, want a 7-element array", inner[slotMessage])
	}
	if message[0] != "hello" {
		t.Errorf("prompt = %#v, want hello", message[0])
	}
	// A nil attachment slot is what the web client sends with no attachment;
	// an empty array is a different request.
	if message[3] != nil {
		t.Errorf("attachment slot = %#v, want nil", message[3])
	}

	if language, ok := inner[slotLanguage].([]any); !ok || language[0] != "en" {
		t.Errorf("slot 1 = %#v, want [en]", inner[slotLanguage])
	}
	if inner[slotStreaming] != 1 {
		t.Errorf("streaming flag = %#v, want 1", inner[slotStreaming])
	}
	if inner[slotModelNum] != spec.Number {
		t.Errorf("model number = %#v, want %d", inner[slotModelNum], spec.Number)
	}
	if metadata, ok := inner[slotMetadata].([]any); !ok || len(metadata) != 10 {
		t.Errorf("slot 2 = %#v, want the 10-element default metadata block", inner[slotMetadata])
	}
}

func TestBuildInnerKeepsConversationMetadata(t *testing.T) {
	spec, _ := ResolveModel("gemini-3.8-flash")
	metadata := []any{"c_abc", "r_def", "rc_ghi", nil, nil, nil, nil, nil, nil, ""}
	inner := buildInner("again", "en", spec, 0, metadata, nil)

	got, ok := inner[slotMetadata].([]any)
	if !ok || len(got) < 2 {
		t.Fatalf("slot 2 = %#v", inner[slotMetadata])
	}
	if got[0] != "c_abc" || got[1] != "r_def" {
		t.Errorf("conversation metadata was not carried through: %#v", got)
	}
}

func TestOuterPayloadShape(t *testing.T) {
	spec, _ := ResolveModel("gemini-3.8-flash")
	inner := buildInner("hi", "en", spec, 0, nil, nil)

	body, err := outerPayload(inner, "AT-TOKEN")
	if err != nil {
		t.Fatalf("outerPayload: %v", err)
	}
	if !strings.HasPrefix(body, "at=AT-TOKEN&f.req=") {
		t.Fatalf("body does not start with the at field: %q", truncate(body, 80))
	}

	// The transport wraps the request array in a two-element array whose second
	// element is the request array *as a string*. Reading it as a nested object
	// yields nothing, so the double encoding is asserted rather than assumed.
	encoded := strings.TrimPrefix(body, "at=AT-TOKEN&f.req=")
	decoded, err := urlDecodeForm(encoded)
	if err != nil {
		t.Fatalf("f.req is not form-encoded: %v", err)
	}

	var outer []any
	if err := json.Unmarshal([]byte(decoded), &outer); err != nil {
		t.Fatalf("f.req is not a JSON array: %v", err)
	}
	if len(outer) != 2 {
		t.Fatalf("outer length = %d, want 2", len(outer))
	}
	if outer[0] != nil {
		t.Errorf("outer[0] = %#v, want null", outer[0])
	}
	innerStr, ok := outer[1].(string)
	if !ok {
		t.Fatalf("outer[1] is %T, want string — the request array must be JSON-encoded twice", outer[1])
	}

	var roundTripped []any
	if err := json.Unmarshal([]byte(innerStr), &roundTripped); err != nil {
		t.Fatalf("outer[1] is not JSON: %v", err)
	}
	if len(roundTripped) != innerSlots {
		t.Errorf("round-tripped inner length = %d, want %d", len(roundTripped), innerSlots)
	}
}

func TestParseTurnExtractsTextAndMetadata(t *testing.T) {
	// A realistic frame payload: slot 1 is the conversation block, slot 4 the
	// candidates, and the answer sits at candidate[1][0].
	payload := `[null,["c_abc","r_def",null,null,null,null,null,null,null,""],null,null,[["rc_ghi",["你好，世界"]]]]`
	turn, ok := parseTurn(Frame{Payload: payload})
	if !ok {
		t.Fatal("parseTurn reported no content")
	}
	if turn.Text != "你好，世界" {
		t.Errorf("text = %q", turn.Text)
	}
	if turn.CID != "c_abc" || turn.RID != "r_def" {
		t.Errorf("conversation ids = (%q, %q)", turn.CID, turn.RID)
	}
	if turn.RCID != "rc_ghi" {
		t.Errorf("rcid = %q", turn.RCID)
	}
	if turn.Metadata == nil || len(turn.Metadata) != 10 {
		t.Errorf("metadata = %#v", turn.Metadata)
	}
}

func TestParseTurnExtractsThoughts(t *testing.T) {
	// Reasoning lives at candidate[37][0][0] — deep enough that a path typo
	// silently yields an empty thinking trace instead of an error.
	candidate := make([]any, 38)
	candidate[0] = "rc_1"
	candidate[1] = []any{"answer"}
	candidate[37] = []any{[]any{"thinking hard"}}
	payload := []any{nil, []any{"c_1", "r_1"}, nil, nil, []any{candidate}}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	turn, _ := parseTurn(Frame{Payload: string(raw)})
	if turn.Thoughts != "thinking hard" {
		t.Errorf("thoughts = %q, want %q", turn.Thoughts, "thinking hard")
	}
}

func TestParseTurnDetectsCompletion(t *testing.T) {
	candidate := make([]any, 9)
	candidate[0] = "rc_1"
	candidate[1] = []any{"done"}
	candidate[8] = []any{float64(2)}
	payload := []any{nil, []any{"c_1", "r_1"}, nil, nil, []any{candidate}}
	raw, _ := json.Marshal(payload)

	turn, _ := parseTurn(Frame{Payload: string(raw)})
	if !turn.Completed {
		t.Error("completion marker candidate[8][0] == 2 was not detected")
	}
}

func TestParseTurnToleratesShortArrays(t *testing.T) {
	// Frames arrive in every intermediate shape while a reply is being built.
	// Indexing past the end must be a non-event, not a panic.
	for _, payload := range []string{`[]`, `[null]`, `[null,null,null,null,[]]`, `{}`} {
		if _, ok := parseTurn(Frame{Payload: payload}); ok {
			t.Errorf("parseTurn(%q) claimed content", payload)
		}
	}
}

// TestDiffCumulative pins the streaming contract: every frame repeats the whole
// answer, so the increment is what gets emitted. Returning the frame verbatim
// would duplicate the reply once per frame.
func TestDiffCumulative(t *testing.T) {
	var last string

	if got := diffCumulative("你", &last); got != "你" {
		t.Errorf("first delta = %q, want 你", got)
	}
	if got := diffCumulative("你好", &last); got != "好" {
		t.Errorf("second delta = %q, want 好", got)
	}
	// An unchanged frame is not a delta.
	if got := diffCumulative("你好", &last); got != "" {
		t.Errorf("repeat delta = %q, want empty", got)
	}
	// A rewritten prefix cannot be expressed as an append. The contract is to
	// emit nothing rather than a corrupted slice — the final text still ends up
	// correct because the caller uses the accumulated value for the response.
	if got := diffCumulative("hello world", &last); got != "" {
		t.Errorf("rewrite delta = %q, want empty", got)
	}
	if last != "hello world" {
		t.Errorf("last was not updated on rewrite: %q", last)
	}
	// After the rewrite, appending resumes normally.
	if got := diffCumulative("hello world!", &last); got != "!" {
		t.Errorf("delta after rewrite = %q, want !", got)
	}
}

func TestCleanArtifacts(t *testing.T) {
	text := "看这张图 https://googleusercontent.com/card_content/123456 很好看"
	cleaned := cleanArtifacts(text)
	if strings.Contains(cleaned, "googleusercontent.com") {
		t.Errorf("placeholder link survived: %q", cleaned)
	}
	if !strings.Contains(cleaned, "很好看") {
		t.Errorf("real text was removed: %q", cleaned)
	}

	// A genuine link must not be stripped.
	real := "文档在 https://example.com/docs 里"
	if got := cleanArtifacts(real); got != real {
		t.Errorf("a real URL was altered: %q", got)
	}
}

func TestModelHeaderShape(t *testing.T) {
	spec, _ := ResolveModel("gemini-3.8-flash")
	header, ok := modelHeader(spec, "SESSION-ID")
	if !ok {
		t.Fatal("gemini-3.8-flash has a known upstream id and must produce a header")
	}

	if !strings.Contains(header, `"`+spec.Upstream+`"`) {
		t.Errorf("header is missing the upstream id: %s", header)
	}
	// The trailing pair is [streamFlag, sessionID]; dropping it is accepted for
	// non-streaming calls and rejected for streaming ones.
	if !strings.HasSuffix(header, `1,"SESSION-ID"]`) {
		t.Errorf("header is missing the trailing [streamFlag, sessionID] pair: %s", header)
	}

	var parsed []any
	if err := json.Unmarshal([]byte(header), &parsed); err != nil {
		t.Fatalf("header is not valid JSON: %v", err)
	}
	if parsed[4] != spec.Upstream {
		t.Errorf("model id sits at index 4, got %#v", parsed[4])
	}
	// Capacity and mode number are the two claims that must agree with the
	// payload; the upstream rejects a mismatch with BardErrorInfo 1052 rather
	// than a readable error, so their positions are pinned.
	if parsed[11] != float64(spec.Capacity) {
		t.Errorf("capacity sits at index 11, got %#v", parsed[11])
	}
	if parsed[14] != float64(spec.Number) {
		t.Errorf("mode number sits at index 14, got %#v", parsed[14])
	}
}

// TestModelHeaderIsOmittedWithoutAnUpstreamID covers the degradation path.
//
// A guessed hex id is answered with BardErrorInfo 1052, which surfaces as a
// failed request that looks like a broken account. Sending no header at all is
// accepted and routed by the mode number, so a mode whose id is unknown must
// produce no header rather than an invented one.
func TestModelHeaderIsOmittedWithoutAnUpstreamID(t *testing.T) {
	spec, ok := LookupModel("gemini-3.8-flash-thinking")
	if !ok {
		t.Fatal("gemini-3.8-flash-thinking should be in the catalogue")
	}
	if spec.Upstream != "" {
		t.Fatalf("fixture changed: %q now has an upstream id", spec.ID)
	}
	if header, ok := modelHeader(spec, "SESSION-ID"); ok || header != "" {
		t.Fatalf("a model without an upstream id produced a header: %q", header)
	}
}

func TestImageRefs(t *testing.T) {
	if refs := imageRefs(nil); refs != nil {
		t.Errorf("no images should produce a nil slot, got %#v", refs)
	}
	refs := imageRefs([]UploadedImage{{URL: "https://example.com/a.png"}, {URL: "  "}})
	if len(refs) != 1 {
		t.Fatalf("blank URLs must be dropped, got %#v", refs)
	}
	triple, ok := refs[0].([]any)
	if !ok || len(triple) != 3 || triple[2] != "https://example.com/a.png" {
		t.Errorf("attachment ref = %#v, want a [null,null,url] triple", refs[0])
	}
}

func urlDecodeForm(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '+':
			b.WriteByte(' ')
		case '%':
			if i+2 >= len(s) {
				return "", errBadEscape
			}
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if !ok1 || !ok2 {
				return "", errBadEscape
			}
			b.WriteByte(hi<<4 | lo)
			i += 2
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), nil
}

type badEscapeError struct{}

func (badEscapeError) Error() string { return "bad percent escape" }

var errBadEscape = badEscapeError{}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
