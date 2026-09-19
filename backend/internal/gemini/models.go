package gemini

import (
	"strings"
)

// ModelSpec is one entry of Gemini's web model catalogue.
//
// The web client never sends a model *name*. What selects a model is a small
// integer in slot 79 of the payload, and the integer is a mode from the client's
// own `MODE_CATEGORY` enum rather than an index into a list:
//
//	1 = FAST                  4 = AUTO
//	2 = THINKING              5 = FAST_DYNAMIC_THINKING
//	3 = PRO                   6 = FLASH_LITE
//
// A second, independent claim travels in the `x-goog-ext-525001261-jspb` header:
// an opaque hex id plus a tier capacity. The two must agree — a mismatch is
// answered with BardErrorInfo 1052 (MODEL_HEADER_INVALID) rather than a readable
// complaint.
//
// The header is therefore optional here in a way the number is not. Upstream
// accepts a headerless request and routes it by number alone, so a mode whose
// hex id is not known is listed with an empty Upstream and simply sends no
// header, instead of guessing an id and earning a 1052.
type ModelSpec struct {
	// ID is the public model name clients send to /v1/chat/completions.
	ID string
	// Upstream is the hex id carried in the model header. Empty means the
	// request is sent without one.
	Upstream string
	// Number goes into payload slot 79. It is the mode, not a list index.
	Number int
	// Capacity is the tier claim in the model header: 1 basic, 2 advanced,
	// 4 plus. It is a statement about the account, not a preference — the
	// upstream rejects the request when the account does not own the tier.
	// Zero means "make no claim", which is what an empty Upstream requires.
	Capacity int
	// Think is the reasoning depth written to slot 17. Captured traffic uses 4
	// for the ordinary modes and 0 for the thinking modes; it is a slot value,
	// not a level, so it is carried per model rather than derived.
	Think int
	// Tier is a human-readable label for the console.
	Tier string
	// Description is shown in the model catalogue page.
	Description string
}

// Mode numbers, named so the catalogue below reads as prose.
const (
	modeFast                = 1
	modeThinking            = 2
	modePro                 = 3
	modeAuto                = 4
	modeFastDynamicThinking = 5
	modeFlashLite           = 6
)

// Think slot values. Ordinary modes carry 4, the thinking modes carry 0.
//
// They are exported because the gateway has to name them when a caller asks for
// a reasoning depth explicitly, and a second copy of "4" and "0" in another
// package is how a protocol detail silently forks.
const (
	ThinkDefault  = 4
	ThinkExtended = 0
)

// Capacity tiers used by the model header.
const (
	CapacityBasic    = 1
	CapacityAdvanced = 2
	CapacityPlus     = 4
)

// modelHeaderKey carries the model selection. It is a protocol constant, not a
// setting: the upstream parses this exact name.
const modelHeaderKey = "x-goog-ext-525001261-jspb"

// builtinModels is the catalogue mirrored from the web client's model picker.
//
// The hex ids are opaque and Google rotates them; they are kept as data (and as
// settings-overridable entries) so a rotation is a config edit rather than a
// rebuild. The modes that are listed without one — thinking, dynamic thinking
// and auto — are the ones whose id is not published by any reference client,
// and they are exactly the entries that rely on the headerless path.
var builtinModels = []ModelSpec{
	{ID: "gemini-flash", Upstream: "fbb127bbb056c959", Number: modeFast, Capacity: CapacityBasic,
		Think: ThinkDefault, Tier: "basic",
		Description: "Flash，默认模型，速度与质量均衡"},
	{ID: "gemini-flash-thinking", Upstream: "", Number: modeThinking, Capacity: 0,
		Think: ThinkExtended, Tier: "basic",
		Description: "Flash 思考模式，输出最长（约 2 万字），适合难题"},
	{ID: "gemini-pro", Upstream: "9d8ca3786ebdfbea", Number: modePro, Capacity: CapacityBasic,
		Think: ThinkDefault, Tier: "basic",
		Description: "Pro，基础档推理模型"},
	{ID: "gemini-auto", Upstream: "", Number: modeAuto, Capacity: 0,
		Think: ThinkDefault, Tier: "basic",
		Description: "Auto，由上游按问题难度自动选档"},
	{ID: "gemini-flash-thinking-lite", Upstream: "", Number: modeFastDynamicThinking, Capacity: 0,
		Think: ThinkExtended, Tier: "basic",
		Description: "Flash 动态思考，按需决定思考深度"},
	{ID: "gemini-flash-lite", Upstream: "cf41b0e0dd7d53e5", Number: modeFlashLite, Capacity: CapacityBasic,
		Think: ThinkDefault, Tier: "basic",
		Description: "Flash Lite，最快最省"},

	// The remaining entries are the same three modes at a higher tier. They
	// exist because the tier is a claim about the account: an account that owns
	// Plus has to say so to be routed to the Plus capacity, and an account that
	// does not will be rejected for asking.
	{ID: "gemini-flash-plus", Upstream: "56fdd199312815e2", Number: modeFast, Capacity: CapacityPlus,
		Think: ThinkDefault, Tier: "plus",
		Description: "Flash Plus，需要账号本身是 Plus 订阅"},
	{ID: "gemini-pro-plus", Upstream: "e6fa609c3fa255c0", Number: modePro, Capacity: CapacityPlus,
		Think: ThinkDefault, Tier: "plus",
		Description: "Pro Plus，需要账号本身是 Plus 订阅"},
	{ID: "gemini-flash-lite-plus", Upstream: "8c46e95b1a07cecc", Number: modeFlashLite, Capacity: CapacityPlus,
		Think: ThinkDefault, Tier: "plus",
		Description: "Flash Lite Plus，需要账号本身是 Plus 订阅"},
	{ID: "gemini-flash-advanced", Upstream: "56fdd199312815e2", Number: modeFast, Capacity: CapacityAdvanced,
		Think: ThinkDefault, Tier: "advanced",
		Description: "Flash Advanced，需要账号本身是 Advanced 订阅"},
	{ID: "gemini-pro-advanced", Upstream: "e6fa609c3fa255c0", Number: modePro, Capacity: CapacityAdvanced,
		Think: ThinkDefault, Tier: "advanced",
		Description: "Pro Advanced，需要账号本身是 Advanced 订阅"},
	{ID: "gemini-flash-lite-advanced", Upstream: "8c46e95b1a07cecc", Number: modeFlashLite, Capacity: CapacityAdvanced,
		Think: ThinkDefault, Tier: "advanced",
		Description: "Flash Lite Advanced，需要账号本身是 Advanced 订阅"},
}

// aliasModels maps the names OpenAI-shaped clients actually send onto the
// catalogue above. They exist so a client that hardcodes `gpt-4o` still works
// without the operator editing anything; the mapping is deliberately coarse,
// because Gemini's web tier — not the requested name — decides what runs.
var aliasModels = map[string]string{
	"gpt-4":                 "gemini-flash",
	"gpt-4o":                "gemini-flash",
	"gpt-4o-mini":           "gemini-flash-lite",
	"gpt-4-turbo":           "gemini-flash",
	"gpt-3.5-turbo":         "gemini-flash-lite",
	"gpt-5":                 "gemini-flash",
	"claude-3-5-sonnet":     "gemini-flash",
	"claude-3-7-sonnet":     "gemini-flash",
	"claude-sonnet-4":       "gemini-flash",
	"claude-opus-4":         "gemini-flash",
	"claude-3-opus":         "gemini-pro",
	"claude-3-haiku":        "gemini-flash-lite",
	"gemini-2.0-flash":      "gemini-flash",
	"gemini-2.0-flash-exp":  "gemini-flash",
	"gemini-2.5-flash":      "gemini-flash",
	"gemini-2.5-pro":        "gemini-pro",
	"gemini-1.5-flash":      "gemini-flash",
	"gemini-1.5-pro":        "gemini-pro",
	"gemini-3-flash":        "gemini-flash",
	"gemini-3-pro":          "gemini-pro",
	"gemini-flash-thinking": "gemini-flash-thinking",
	"gemini-pro-thinking":   "gemini-pro",
	"deepseek-chat":         "gemini-flash",
	"deepseek-reasoner":     "gemini-flash-thinking",
	"o1":                    "gemini-pro",
	"o3-mini":               "gemini-flash",
}

// BuiltinModels returns a copy of the catalogue so callers cannot mutate the
// package-level slice.
func BuiltinModels() []ModelSpec {
	out := make([]ModelSpec, len(builtinModels))
	copy(out, builtinModels)
	return out
}

// ModelIDs lists the public ids, for the console and /v1/models.
func ModelIDs() []string {
	out := make([]string, 0, len(builtinModels))
	for _, m := range builtinModels {
		out = append(out, m.ID)
	}
	return out
}

// LookupModel resolves a client-supplied model name.
//
// Resolution is forgiving on purpose. A 2api sits behind clients that send
// whatever name they were configured with — including the upstream's own names,
// OpenAI names, and names that simply do not exist — and refusing all of them
// produces a gateway that "does not work" for reasons the operator cannot see.
// An unknown name therefore falls back to the default model rather than
// erroring.
func LookupModel(name string) (ModelSpec, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return ModelSpec{}, false
	}
	// Strip an OpenAI-style provider prefix: `google/gemini-flash`.
	if idx := strings.LastIndex(key, "/"); idx >= 0 {
		key = key[idx+1:]
	}
	// A `:free` / `:nitro` style suffix is a routing hint, not part of the id.
	if idx := strings.LastIndex(key, ":"); idx >= 0 {
		key = key[:idx]
	}

	if alias, ok := aliasModels[key]; ok {
		key = alias
	}
	for _, m := range builtinModels {
		if m.ID == key {
			return m, true
		}
	}
	return ModelSpec{}, false
}

// DefaultModelID is what an unnamed request resolves to.
const DefaultModelID = "gemini-flash"

// ResolveModel never fails: it returns the requested model when it is known and
// the default otherwise, so a call is never rejected purely for naming a model
// the catalogue does not carry. The bool reports whether the name was actually
// understood, which is what the gateway uses to decide between answering and
// telling the caller the name was wrong.
func ResolveModel(name string) (ModelSpec, bool) {
	if spec, ok := LookupModel(name); ok {
		return spec, true
	}
	if spec, ok := LookupModel(DefaultModelID); ok {
		return spec, false
	}
	// builtinModels is non-empty by construction; this keeps the compiler happy.
	return ModelSpec{ID: DefaultModelID, Upstream: "fbb127bbb056c959", Number: modeFast,
		Capacity: CapacityBasic, Think: ThinkDefault}, false
}

// modelHeader renders the model selection header value, or false when the model
// has no known id and no header should be sent.
//
// The trailing pair is `[streamFlag, sessionID]`; the web client appends both
// after the number. Leaving them off is accepted by the upstream for
// non-streaming calls and rejected for streaming ones, so they are always
// written.
func modelHeader(spec ModelSpec, sessionID string) (string, bool) {
	if spec.Upstream == "" {
		return "", false
	}
	return "[1,null,null,null,\"" + spec.Upstream + "\",null,null,0,[4,5,6,8],null,null," +
		itoa(spec.Capacity) + ",null,null," + itoa(spec.Number) + ",1,\"" + sessionID + "\"]", true
}

// itoa avoids pulling strconv in for two small non-negative integers.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	pos := len(digits)
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[pos:])
}
