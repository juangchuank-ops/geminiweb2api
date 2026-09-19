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
// The framing is `<length>\n<payload>` repeated, where length counts UTF-16
// code units — JavaScript string length, not bytes. Treating it as bytes works
// for ASCII and silently desynchronises the moment a reply contains an emoji or
// any CJK character outside the BMP, which is exactly what happens in practice.
type FrameParser struct {
	buf     strings.Builder
	rest    string // buffered text not yet consumed
	started bool   // XSSI prefix already handled
	length  int    // expected UTF-16 length of the frame being assembled
	haveLen bool
}

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
		if !ok {
			break
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
