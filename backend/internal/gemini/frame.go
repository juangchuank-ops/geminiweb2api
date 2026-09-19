package gemini

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Frame is one `wrb.fr` envelope lifted out of the stream.
//
// Google's batchexecute transport wraps every RPC result in a small array whose
// shape is `["wrb.fr", <rpcId>, <jsonString>, ...]`. The payload this gateway
// cares about is always the third element, and it is always a *string*
// containing JSON — the double encoding is part of the transport, not an
// accident, and reading it as a nested object silently yields nothing.
type Frame struct {
	RPCID   string
	Payload string
}

// xssiPrefix is prepended to every batchexecute response. It is an
// anti-CSRF measure: a bare `[...]` is valid JavaScript, so a script tag
// pointed at the endpoint could read the response, whereas `)]}'` is a syntax
// error. It must be stripped before the length-prefixed body is parsed.
const xssiPrefix = ")]}'"

// FrameParser incrementally decodes the length-prefixed batchexecute stream.
//
// The framing is `<length>\n<payload>` repeated. The declared length is used as
// a fast path, but it is *not* trusted blindly — see Feed.
//
// Why the distrust: the unit the upstream counts in is not stable, and at least
// two independent implementations gave up on it (tmc/nlm: "the exact counting
// varies. Instead of trusting the length values, extract JSON arrays directly
// by finding balanced brackets"; zexadev/gemini-web2api-go scans line by line
// and never reads the length at all). When the declared length is wrong the
// parser desynchronises once and then reports *zero* frames for the rest of the
// stream, which surfaces as an empty answer rather than an error.
type FrameParser struct {
	buf     strings.Builder
	rest    string // buffered text not yet consumed
	started bool   // XSSI prefix already handled
	length  int    // expected UTF-16 length of the frame being assembled
	haveLen bool
	resyncs int // frames whose declared length was rejected and rebuilt structurally
}

// Resyncs reports how many frames had to be recovered structurally because the
// declared length did not land on a frame boundary.
//
// Non-zero is not an error — it is the parser doing its job — but a stream that
// resyncs on *every* frame is telling you the upstream changed its counting
// again, and the fast path is now dead weight.
func (p *FrameParser) Resyncs() int { return p.resyncs }

// NewFrameParser returns a parser ready to consume a stream.
func NewFrameParser() *FrameParser {
	return &FrameParser{}
}

// Feed adds a decoded chunk of the response body and returns every frame that
// became complete.
func (p *FrameParser) Feed(chunk string) []Frame {
	p.rest += chunk

	if !p.started {
		// The prefix may be split across chunks; wait for enough to decide.
		trimmed := strings.TrimLeft(p.rest, "\r\n ")
		if len(trimmed) < len(xssiPrefix) {
			return nil
		}
		if strings.HasPrefix(trimmed, xssiPrefix) {
			p.rest = trimmed[len(xssiPrefix):]
		} else {
			// Not the prefix we expected. The body may still be framed, so
			// carry on rather than discarding a possibly usable response.
			p.rest = trimmed
		}
		p.started = true
	}

	var frames []Frame

	for {
		if !p.haveLen {
			idx := strings.IndexByte(p.rest, '\n')
			if idx < 0 {
				// A length marker longer than this is not a length marker.
				if len(p.rest) > 20 {
					p.rest = p.rest[len(p.rest)-20:]
				}
				break
			}
			raw := strings.TrimSpace(p.rest[:idx])
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				// Noise between frames. Drop the line and keep looking; the
				// stream is still framed after it.
				p.rest = p.rest[idx+1:]
				continue
			}
			p.length = n
			p.haveLen = true
			p.rest = p.rest[idx+1:]
		}

		consumed, ok := utf16PrefixBytes(p.rest, p.length)
		if !ok || !frameBoundaryOK(p.rest[consumed:]) {
			// The declared length did not land on a frame boundary, so it is
			// not telling us where this frame ends. Rebuild the boundary from
			// the payload's own structure instead: the payload is a JSON array,
			// so its extent is knowable exactly, and unlike the length it
			// cannot be wrong.
			end, found := balancedArrayEnd(p.rest)
			if !found {
				// Either the frame is still arriving, or this frame is not
				// JSON at all.
				//
				// Do NOT fall back to consuming the declared length here. That
				// was the first version of this fix, and it reintroduced the
				// very bug it was meant to cure: while a frame is still
				// streaming in, the length "completes" it one byte early, the
				// structure is not closed yet, and consuming on the strength of
				// a length that just failed its boundary check eats the frame
				// whole. Waiting is always safe — the caller feeds more bytes.
				break
			}
			if consumed != end {
				p.resyncs++
			}
			consumed = end
		}
		payload := p.rest[:consumed]
		p.rest = p.rest[consumed:]
		p.haveLen = false

		if strings.TrimSpace(payload) == "" {
			continue
		}
		frames = append(frames, decodeFrame(payload)...)
	}

	return frames
}

// Flush drains any frame left when the body ends.
func (p *FrameParser) Flush() []Frame {
	if !p.haveLen && strings.TrimSpace(p.rest) == "" {
		return nil
	}
	return p.Feed("\n0\n")
}

// decodeFrame parses one length-delimited payload into wrb.fr envelopes.
//
// A single payload is a JSON array of entries; entries that are not `wrb.fr`
// envelopes (`di`, `af.httprm`, `e`) carry transport metadata and are skipped
// rather than treated as errors, because the upstream adds new ones without
// notice.
func decodeFrame(payload string) []Frame {
	var top []json.RawMessage
	if err := json.Unmarshal([]byte(payload), &top); err != nil {
		return nil
	}

	var out []Frame
	for _, raw := range top {
		var entry []json.RawMessage
		if err := json.Unmarshal(raw, &entry); err != nil || len(entry) < 3 {
			continue
		}
		var kind string
		if err := json.Unmarshal(entry[0], &kind); err != nil || kind != "wrb.fr" {
			continue
		}
		var body string
		if err := json.Unmarshal(entry[2], &body); err != nil || body == "" {
			continue
		}
		frame := Frame{Payload: body}
		if len(entry) > 1 {
			_ = json.Unmarshal(entry[1], &frame.RPCID)
		}
		out = append(out, frame)
	}
	return out
}

// frameBoundaryOK reports whether rest begins where the next frame begins:
// optional whitespace, then the next length marker, then its newline.
//
// An empty or whitespace-only rest is deliberately reported as *not* OK. Mid
// stream, "nothing left" is ambiguous — it means either the frame ended exactly
// here or the buffer simply has not received the next byte yet — and treating it
// as a valid boundary is what let an under-declared length truncate a frame by
// its last byte and drop it silently. Callers that reach the true end of the
// stream are served by the structural path instead, which does not need to
// guess.
func frameBoundaryOK(rest string) bool {
	i := 0
	for i < len(rest) && (rest[i] == '\n' || rest[i] == '\r' || rest[i] == ' ' || rest[i] == '\t') {
		i++
	}
	if i >= len(rest) {
		return false
	}
	digits := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
		digits++
	}
	return digits > 0 && i < len(rest) && rest[i] == '\n'
}

// balancedArrayEnd returns the offset just past the first balanced top-level
// JSON array in s.
//
// It walks string literals and escapes rather than counting brackets naively:
// payloads are full of code and prose, so a `]` inside a string is routine and
// would otherwise end the frame early.
//
// found is false when the array is not closed yet, which for a stream means
// "keep buffering" — the caller must not treat it as a malformed frame.
func balancedArrayEnd(s string) (end int, found bool) {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return 0, false
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// utf16PrefixBytes returns the number of bytes of s that make up the first n
// UTF-16 code units, and whether s is long enough to contain them.
//
// Characters outside the BMP (emoji, rare CJK) occupy two UTF-16 code units in
// a JavaScript string but four bytes in UTF-8, so byte length and the frame's
// declared length disagree on exactly the inputs that matter.
func utf16PrefixBytes(s string, n int) (int, bool) {
	if n == 0 {
		return 0, true
	}
	units := 0
	for i, r := range s {
		if units >= n {
			return i, true
		}
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	if units >= n {
		return len(s), true
	}
	return 0, false
}

// UTF16Len reports the JavaScript string length of s.
func UTF16Len(s string) int {
	units := 0
	for _, r := range s {
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}
