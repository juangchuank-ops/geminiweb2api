package gemini

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// generatePath is the batchexecute endpoint that produces a reply.
//
// It is a path rather than a setting because it is part of the transport, not a
// deployment choice: unlike the host (which operators may point at a mirror)
// there is no variant of this path in use. Everything about it that can change
// — the build label, the language, the session id — travels in the query.
const generatePath = "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"

// rotateCookiesURL is Google's cookie rotation endpoint.
//
// It is deliberately absolute and not derived from the account's base URL:
// rotation happens against the accounts host, which is a different service from
// the one serving Gemini. An account pointed at a mirror still rotates here.
const rotateCookiesURL = "https://accounts.google.com/RotateCookies"

// newRequestUUID mints the uppercase v4 UUID the payload and headers both carry.
//
// The two must match: slot 59 of the payload and the
// `x-goog-ext-525005358-jspb` header are compared, and a mismatch is rejected
// without saying so.
func newRequestUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A predictable UUID is worse than none, but failing the request here
		// would turn a source of entropy exhaustion into an outage. The shape
		// is still valid, so the request proceeds.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])

	return strings.ToUpper(string(out[:]))
}
