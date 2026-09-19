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
// an opaque hex id plus a capacity. The two must agree — a mismatch is answered
// with BardErrorInfo 1052 (MODEL_HEADER_INVALID) rather than a readable
// complaint.
//
// The header is therefore optional here in a way the number is not. Upstream
// accepts a headerless request and routes it by number alone, so a mode whose
// hex id is not known is listed with an empty Upstream and simply sends no
// header, instead of guessing an id and earning a 1052.
type ModelSpec struct {
	// ID is the public model name clients send to /v1/chat/completions. It is
	// the *marketed* name — `gemini-3.8-flash` — because that is the string a
	// human reads off a model list and types into a client's settings.
	ID string
	// Upstream is the hex id carried in the model header. Empty means the
	// request is sent without one.
	Upstream string
	// Number goes into payload slot 79. It is the mode, not a list index.
	Number int
	// Capacity is the value at index 11 of the model header. Captured headers
	// carry 1 for the models the free tier is served and 2 for the paid ones;
	// it is a property of the model as the web client sends it, not a
	// preference a caller expresses. Zero means "make no claim", which is what
	// an empty Upstream requires.
	Capacity int
	// Think is the reasoning depth written to slot 17. Captured traffic uses 4
	// for the ordinary modes and 0 for the thinking modes; it is a slot value,
	// not a level, so it is carried per model rather than derived.
	Think int
	// Description is shown in the model catalogue page.
	//
	// There used to be a `Tier` field next to this one, set on every entry and
	// read by nothing. It is gone: it existed to hold the fiction described in
	// the catalogue comment below, and a field nothing consumes is how that
	// fiction stayed invisible.
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

// Capacity values carried at index 11 of the model header.
//
// There are exactly two, and they come from captured headers: 1 for the models
// the free tier is served (3.6 Flash, 3.5 Flash-Lite) and 2 for the paid ones
// (3.8 Flash, 3.1 Pro). A third value, 4, used to live here as a "Plus" tier.
// No captured header shows it, and the three models that carried it turned out
// to be a *newer* model under a tier-shaped name, so it was deleted rather than
// kept as an unverified guess. If an account ever needs a capacity the
// catalogue does not carry, that is an operator setting to add — not a model
// name to invent.
const (
	CapacityFree = 1
	CapacityPaid = 2
)

// modelHeaderKey carries the model selection. It is a protocol constant, not a
// setting: the upstream parses this exact name.
const modelHeaderKey = "x-goog-ext-525001261-jspb"

// builtinModels is the catalogue, keyed on the name each model is *marketed*
// under — the string the Gemini app, the developer docs and every client's
// model picker show a human.
//
// That keying is load-bearing, not cosmetic. A 2api is reached by clients whose
// model field was filled in by a person reading a model list, so
// `gemini-3.8-flash` has to resolve. An earlier revision of this table had it
// backwards: it was keyed on invented internal names (`gemini-flash`,
// `gemini-flash-plus`) and pushed the real names into the alias table below, so
// the names nobody had ever seen were primary and the names everybody uses were
// second-class.
//
// The hex ids are opaque and Google rotates them. Two rules, both of which this
// table used to violate:
//
//   - A hex id identifies a model, not a capacity. 56fdd199312815e2 was listed
//     three times as the "Plus" and "Advanced" variants of a model whose own id
//     was fbb127bbb056c959. The tier suffix was fiction — the entries were a
//     *newer* model filed under the wrong name, which is how the default model
//     ended up pinned to the previous generation while the current one sat
//     behind a subscription-shaped name nobody would think to try.
//   - Google upgrades a model in place. 3.7 Flash became 3.8 Flash under the
//     same id, so an id is not a version. 3.7 is therefore an alias below, not
//     a second entry here.
//
// Entries with no hex id are the modes whose id no reference client publishes.
// They take the headerless path and are routed by the mode number alone. The
// thinking mode is deliberately left on that path even though one reference
// implementation claims it shares 3.8 Flash's id: the headerless route is
// verified working, and an unverified id on every reasoning request is a 1052
// on every reasoning request.
var builtinModels = []ModelSpec{
	{ID: "gemini-3.8-flash", Upstream: "56fdd199312815e2", Number: modeFast, Capacity: CapacityPaid,
		Think:       ThinkDefault,
		Description: "3.8 Flash，默认模型。网页端当前的 Flash，速度与质量均衡"},
	{ID: "gemini-3.8-flash-thinking", Upstream: "", Number: modeThinking, Capacity: 0,
		Think:       ThinkExtended,
		Description: "3.8 Flash 思考模式，输出最长（约 2 万字），适合难题"},
	{ID: "gemini-3.1-pro", Upstream: "e6fa609c3fa255c0", Number: modePro, Capacity: CapacityPaid,
		Think:       ThinkDefault,
		Description: "3.1 Pro，需要账号有 Pro/Ultra 订阅，免费号会被上游降级"},
	{ID: "gemini-auto", Upstream: "", Number: modeAuto, Capacity: 0,
		Think:       ThinkDefault,
		Description: "Auto，由上游按问题难度自动选档"},
	{ID: "gemini-3.8-flash-thinking-lite", Upstream: "", Number: modeFastDynamicThinking, Capacity: 0,
		Think:       ThinkExtended,
		Description: "3.8 Flash 动态思考，按需决定思考深度"},
	{ID: "gemini-3.5-flash-lite", Upstream: "cf41b0e0dd7d53e5", Number: modeFlashLite, Capacity: CapacityFree,
		Think:       ThinkDefault,
		Description: "3.5 Flash-Lite，最快最省；免费号被降级时落到的就是它"},
	{ID: "gemini-3.6-flash", Upstream: "fbb127bbb056c959", Number: modeFast, Capacity: CapacityFree,
		Think:       ThinkDefault,
		Description: "3.6 Flash，上一代 Flash。留着是因为旧客户端会写死这个名字"},
}

// aliasModels maps every other name a client might send onto the catalogue
// above.
//
// Two kinds live here. The first is other vendors' names, so a client
// configured for `gpt-4o` does not need its config edited to point at this
// gateway. The second is *retired Gemini names* — including the invented names
// this project used to publish as primary. Those matter more than they look: a
// 2api sits behind clients that were pointed at it once and never touched
// again, so an old name resolving is the difference between "works" and a 400
// nobody can explain. `gemini-flash` in particular is what this project's own
// README and console advertised until this change, so it has to keep working.
//
// The mapping is deliberately coarse, because Gemini's web tier — not the
// requested name — decides what actually runs.
var aliasModels = map[string]string{
	// Names this project used to publish as primary. Kept so an operator who
	// copied them out of an older README, or whose client still has one pinned,
	// is not broken by the rename.
	"gemini-flash":               "gemini-3.8-flash",
	"gemini-flash-plus":          "gemini-3.8-flash",
	"gemini-flash-advanced":      "gemini-3.8-flash",
	"gemini-flash-thinking":      "gemini-3.8-flash-thinking",
	"gemini-flash-thinking-lite": "gemini-3.8-flash-thinking-lite",
	"gemini-pro":                 "gemini-3.1-pro",
	"gemini-pro-thinking":        "gemini-3.1-pro",
	"gemini-pro-plus":            "gemini-3.1-pro",
	"gemini-pro-advanced":        "gemini-3.1-pro",
	"gemini-flash-lite":          "gemini-3.5-flash-lite",
	"gemini-flash-lite-plus":     "gemini-3.5-flash-lite",
	"gemini-flash-lite-advanced": "gemini-3.5-flash-lite",

	// Google's own names, current and retired. `gemini-3.7-flash` is here
	// rather than in the catalogue because 3.7 Flash *is* 3.8 Flash: Google
	// upgraded it in place under the same hex id.
	"gemini-3.7-flash":          "gemini-3.8-flash",
	"gemini-3.7-flash-thinking": "gemini-3.8-flash-thinking",
	"gemini-3.5-flash":          "gemini-3.6-flash",
	"gemini-3.1-flash-lite":     "gemini-3.5-flash-lite",
	"gemini-3-flash":            "gemini-3.8-flash",
	"gemini-3-flash-preview":    "gemini-3.8-flash",
	"gemini-3-pro":              "gemini-3.1-pro",
	"gemini-3-pro-preview":      "gemini-3.1-pro",
	"gemini-2.5-flash":          "gemini-3.8-flash",
	"gemini-2.5-flash-lite":     "gemini-3.5-flash-lite",
	"gemini-2.5-pro":            "gemini-3.1-pro",
	"gemini-2.0-flash":          "gemini-3.8-flash",
	"gemini-2.0-flash-exp":      "gemini-3.8-flash",
	"gemini-2.0-flash-lite":     "gemini-3.5-flash-lite",
	"gemini-1.5-flash":          "gemini-3.8-flash",
	"gemini-1.5-pro":            "gemini-3.1-pro",

	// Other vendors.
	"gpt-4":             "gemini-3.8-flash",
	"gpt-4o":            "gemini-3.8-flash",
	"gpt-4o-mini":       "gemini-3.5-flash-lite",
	"gpt-4-turbo":       "gemini-3.8-flash",
	"gpt-3.5-turbo":     "gemini-3.5-flash-lite",
	"gpt-5":             "gemini-3.8-flash",
	"claude-3-5-sonnet": "gemini-3.8-flash",
	"claude-3-7-sonnet": "gemini-3.8-flash",
	"claude-sonnet-4":   "gemini-3.8-flash",
	"claude-opus-4":     "gemini-3.8-flash",
	"claude-3-opus":     "gemini-3.1-pro",
	"claude-3-haiku":    "gemini-3.5-flash-lite",
	"deepseek-chat":     "gemini-3.8-flash",
	"deepseek-reasoner": "gemini-3.8-flash-thinking",
	"o1":                "gemini-3.1-pro",
	"o3-mini":           "gemini-3.8-flash",
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
	// Strip an OpenAI-style provider prefix: `google/gemini-3.8-flash`.
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
const DefaultModelID = "gemini-3.8-flash"

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
	return ModelSpec{ID: DefaultModelID, Upstream: "56fdd199312815e2", Number: modeFast,
		Capacity: CapacityPaid, Think: ThinkDefault}, false
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
