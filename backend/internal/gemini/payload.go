package gemini

import (
	"encoding/json"
	"strings"
)

// Payload slot indices of the request array.
//
// The web client builds an 81-element sparse array and posts it as a string.
// Positions matter and there is no schema: an index that is off by one does not
// produce an error, it produces a request that quietly asks for something else.
// The names below are the ones that were verified against captured traffic;
// slots that are always empty are simply never written.
const (
	slotMessage   = 0  // [prompt, 0, null, attachments, null, null, 0]
	slotLanguage  = 1  // ["en"]
	slotMetadata  = 2  // conversation ids on a follow-up, defaults on a first turn
	slotModelHint = 6  // [1]
	slotStreaming = 7  // 1
	slotTurnKind  = 10 // 1
	slotUnknown11 = 11 // 0
	slotThinking  = 17 // [[level]]
	slotUnknown18 = 18 // 0
	slotGem       = 19 // a Gem id, when a system prompt is applied
	slotUnknown27 = 27 // 1
	slotOutputCap = 30 // [4]
	slotPersist   = 41 // [1] keep in history
	slotTemporary = 45 // 1 = do not keep in history
	slotUnknown53 = 53 // 0
	slotUUID      = 59 // a fresh uppercase UUID per request
	slotEmptyList = 61 // []
	slotUnknown68 = 68 // 1
	slotModelNum  = 79 // model mode number
	// slotExtended is written as 1. Reference clients either write 1 or leave
	// the slot out entirely and both are accepted; nothing verified writes 2,
	// so the "extended thinking" reading of this slot is not relied on here —
	// reasoning depth is selected through slotThinking instead.
	slotExtended = 80
)

// innerSlots is the length of the request array. The web client allocates 81
// and leaves most of them null; sending a shorter array shifts every high slot
// and is answered with an unhelpful generic error.
const innerSlots = 81

// defaultMetadata is the conversation block of a first turn. On a follow-up it
// is replaced wholesale by the block the previous response returned.
func defaultMetadata() []any {
	return []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
}

// buildInner assembles the request payload.
//
// fileRefs are upstream attachment references; a nil slice produces the same
// shape the web client sends when there is no attachment.
func buildInner(prompt, language string, spec ModelSpec, thinkMode int, metadata []any, fileRefs []any) []any {
	inner := make([]any, innerSlots)

	message := make([]any, 7)
	message[0] = prompt
	message[1] = 0
	if len(fileRefs) > 0 {
		message[3] = fileRefs
	}
	message[6] = 0
	inner[slotMessage] = message

	inner[slotLanguage] = []any{language}
	if metadata == nil {
		metadata = defaultMetadata()
	}
	inner[slotMetadata] = metadata
	inner[slotModelHint] = []any{1}
	inner[slotStreaming] = 1
	inner[slotTurnKind] = 10
	inner[slotUnknown11] = 0
	inner[slotThinking] = []any{[]any{thinkMode}}
	inner[slotUnknown18] = 0
	inner[slotUnknown27] = 1
	inner[slotOutputCap] = []any{4}
	inner[slotPersist] = []any{1}
	inner[slotUnknown53] = 0
	inner[slotEmptyList] = []any{}
	inner[slotUnknown68] = 1
	inner[slotModelNum] = spec.Number
	inner[slotExtended] = 1

	return inner
}

// outerPayload wraps the request array the way the transport expects: a
// two-element array whose second element is the request array *as a string*.
// The double encoding is the protocol, not a mistake — the server re-parses it.
func outerPayload(inner []any, at string) (string, error) {
	encoded, err := json.Marshal(inner)
	if err != nil {
		return "", err
	}
	outer := []any{nil, string(encoded)}
	body, err := json.Marshal(outer)
	if err != nil {
		return "", err
	}
	// The `at` token is a form field beside f.req, not part of it.
	return "at=" + urlEncodeForm(at) + "&f.req=" + urlEncodeForm(string(body)), nil
}

// urlEncodeForm escapes a value for application/x-www-form-urlencoded.
//
// net/url's QueryEscape is close but not identical: it encodes a space as '+',
// which the upstream accepts, and leaves '~' alone, which it also accepts. The
// differences that matter here are cosmetic, so the standard library is used
// rather than a hand-rolled copy — there is no signature over this body, and
// introducing one would add a way to get it wrong.
func urlEncodeForm(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// Turn is everything one response frame says about the conversation.
type Turn struct {
	// Text is the model's answer as of this frame. It is *cumulative*: every
	// frame repeats the whole answer so far, so the caller diffs it against the
	// previous frame to get the increment. Appending it verbatim would
	// duplicate the answer many times over.
	Text string
	// Thoughts is the cumulative reasoning trace, when the model emits one.
	Thoughts string
	// CID, RID and RCID identify the conversation, the reply and the reply
	// candidate. Only CID and RID are needed to continue a conversation.
	CID  string
	RID  string
	RCID string
	// Metadata is the conversation block to send on the next turn, captured
	// verbatim so a field this code does not understand is still carried
	// forward.
	Metadata []any
	// Completed is set once the model marks the turn finished.
	Completed bool
	// Media holds attachment URLs found on the candidate.
	Media []MediaRef
	// HasContent reports whether any of the fields above were present. A frame
	// with no content is transport noise and should not reset the diff state.
	HasContent bool
}

// MediaRef is an attachment URL and its kind.
type MediaRef struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

// parseTurn lifts the conversation state out of one frame.
func parseTurn(frame Frame) (Turn, bool) {
	var payload []any
	if err := json.Unmarshal([]byte(frame.Payload), &payload); err != nil {
		return Turn{}, false
	}

	turn := Turn{}

	// Slot 1 is the conversation block: [cid, rid, ...]. It is sent back
	// verbatim as slot 2 on the next turn.
	if meta, ok := index(payload, 1).([]any); ok && len(meta) > 0 {
		turn.Metadata = meta
		turn.CID, _ = meta[0].(string)
		if len(meta) > 1 {
			turn.RID, _ = meta[1].(string)
		}
	}

	candidates, _ := index(payload, 4).([]any)
	for _, raw := range candidates {
		candidate, ok := raw.([]any)
		if !ok {
			continue
		}

		if text, ok := nestedString(candidate, 1, 0); ok {
			turn.Text = text
			turn.HasContent = true
		}
		if thoughts, ok := nestedString(candidate, 37, 0, 0); ok {
			turn.Thoughts = thoughts
		}
		if rcid, ok := index(candidate, 0).(string); ok && rcid != "" {
			turn.RCID = rcid
		}
		// Slot 8 slot 0 == 2 marks the turn as finished.
		if state, ok := nestedInt(candidate, 8, 0); ok && state == 2 {
			turn.Completed = true
		}
		turn.Media = append(turn.Media, collectMedia(candidate)...)
	}

	return turn, turn.HasContent || turn.Completed
}

// collectMedia gathers attachment URLs from the rich-content block.
//
// Only explicitly-named keys are read. A bare `url` appears in dozens of
// unrelated places in this payload (tracking, avatars, source links) and
// treating every one of them as an attachment floods the gallery with noise, so
// a key has to say what it is.
func collectMedia(candidate []any) []MediaRef {
	rich, ok := index(candidate, 12).([]any)
	if !ok {
		return nil
	}

	var out []MediaRef
	appendURLs := func(slot int, kind string) {
		entries, ok := index(rich, slot).([]any)
		if !ok {
			return
		}
		for _, entry := range entries {
			if url, ok := firstDeepString(entry); ok && strings.HasPrefix(url, "http") {
				out = append(out, MediaRef{Kind: kind, URL: url})
			}
		}
	}

	appendURLs(1, "web")
	appendURLs(7, "image")
	appendURLs(59, "video")
	return out
}

// index reads a positional slot, tolerating a short array.
func index(container []any, i int) any {
	if i < 0 || i >= len(container) {
		return nil
	}
	return container[i]
}

// nestedString walks a chain of indices looking for a string.
func nestedString(container []any, path ...int) (string, bool) {
	current := any(container)
	for _, idx := range path {
		arr, ok := current.([]any)
		if !ok {
			return "", false
		}
		current = index(arr, idx)
	}
	s, ok := current.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// nestedInt walks a chain of indices looking for a number.
func nestedInt(container []any, path ...int) (int, bool) {
	current := any(container)
	for _, idx := range path {
		arr, ok := current.([]any)
		if !ok {
			return 0, false
		}
		current = index(arr, idx)
	}
	f, ok := current.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// firstDeepString returns the first non-empty string anywhere inside value.
//
// It is deliberately shape-blind: the upstream nests the same information at
// different depths depending on the media type, and a path-anchored reader
// breaks every time that nesting changes.
func firstDeepString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		if v == "" {
			return "", false
		}
		return v, true
	case []any:
		for _, item := range v {
			if s, ok := firstDeepString(item); ok {
				return s, true
			}
		}
	case map[string]any:
		for _, item := range v {
			if s, ok := firstDeepString(item); ok {
				return s, true
			}
		}
	}
	return "", false
}

// cleanArtifacts removes placeholder links the web UI swaps for rendered
// attachments. They are meaningless to an API client, which would otherwise
// receive a bare googleusercontent URL in the middle of a sentence.
func cleanArtifacts(text string) string {
	if !strings.Contains(text, "googleusercontent.com/") {
		return text
	}
	fields := strings.Fields(text)
	kept := fields[:0]
	for _, field := range fields {
		if isArtifactURL(field) {
			continue
		}
		kept = append(kept, field)
	}
	return strings.Join(kept, " ")
}

func isArtifactURL(field string) bool {
	trimmed := strings.Trim(field, " \t\r\n")
	if !strings.HasPrefix(trimmed, "http") {
		return false
	}
	idx := strings.Index(trimmed, "googleusercontent.com/")
	if idx < 0 {
		return false
	}
	rest := trimmed[idx+len("googleusercontent.com/"):]
	// The placeholder always ends in a numeric id, possibly after extra
	// segments such as image_collection/image_retrieval/.
	last := rest
	if slash := strings.LastIndex(rest, "/"); slash >= 0 {
		last = rest[slash+1:]
	}
	last = strings.TrimRight(last, ".,)")
	if last == "" {
		return false
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
