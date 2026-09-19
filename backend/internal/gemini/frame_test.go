package gemini

import (
	"fmt"
	"strings"
	"testing"
)

// buildStream renders a batchexecute response body the way the upstream does:
// the XSSI prefix, then repeated `<utf16 length>\n<payload>` frames.
func buildStream(payloads ...string) string {
	var b strings.Builder
	b.WriteString(xssiPrefix)
	b.WriteString("\n")
	for _, payload := range payloads {
		fmt.Fprintf(&b, "%d\n%s", UTF16Len(payload), payload)
	}
	return b.String()
}

// buildStreamDeclaring renders a stream whose declared lengths come from
// declared(payload) rather than from UTF16Len.
//
// This helper exists because buildStream cannot falsify anything: it derives the
// length from UTF16Len, the very function the parser uses, so the fixture and
// the parser share one assumption and can only ever agree with each other. A
// real capture disagreed with both — the declared length did not land on a frame
// boundary, the parser desynchronised at the first frame, and every subsequent
// request came back with an empty answer while the tests stayed green.
//
// The frames are newline-terminated here, which is what the upstream emits and
// what makes an over-long declared length visible: the parser consumes the
// newline and then bites into the *next* length marker.
func buildStreamDeclaring(declared func(string) int, payloads ...string) string {
	var b strings.Builder
	b.WriteString(xssiPrefix)
	b.WriteString("\n")
	for _, payload := range payloads {
		fmt.Fprintf(&b, "%d\n%s\n", declared(payload), payload)
	}
	return b.String()
}

func TestFrameParserBasic(t *testing.T) {
	payload := `[["wrb.fr",null,"{\"a\":1}"]]`
	stream := buildStream(payload)

	frames := NewFrameParser().Feed(stream)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Payload != `{"a":1}` {
		t.Errorf("payload = %q, want %q", frames[0].Payload, `{"a":1}`)
	}
}

// TestFrameParserUsesUTF16Length is the test that matters most in this file.
//
// The frame header counts UTF-16 code units, which is JavaScript string length.
// A byte-based reader agrees with it for ASCII and desynchronises on exactly
// the inputs that occur in practice — any CJK or emoji character in a reply.
// The failure is silent: the parser simply never finds a complete frame again,
// and the answer comes back empty.
func TestFrameParserUsesUTF16Length(t *testing.T) {
	// Each CJK character is 3 bytes in UTF-8 but 1 UTF-16 unit; the emoji is
	// 4 bytes but 2 units.
	payload := `[["wrb.fr",null,"{\"t\":\"你好世界😀\"}"]]`

	bytes := len(payload)
	units := UTF16Len(payload)
	if bytes == units {
		t.Fatalf("test payload is not discriminating: bytes == units == %d", bytes)
	}
	if got := UTF16Len("😀"); got != 2 {
		t.Fatalf("UTF16Len(emoji) = %d, want 2", got)
	}

	stream := buildStream(payload)

	// Feed byte by byte to also exercise the incremental path: a parser that
	// only works when handed the whole body is not a streaming parser.
	parser := NewFrameParser()
	var frames []Frame
	for i := 0; i < len(stream); i++ {
		frames = append(frames, parser.Feed(stream[i:i+1])...)
	}

	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 — length was probably read as bytes", len(frames))
	}
	if !strings.Contains(frames[0].Payload, "你好世界😀") {
		t.Errorf("payload lost its non-ASCII content: %q", frames[0].Payload)
	}
}

func TestFrameParserMultipleFrames(t *testing.T) {
	// Real streams carry several frames: an initial one, then incremental
	// updates. Each has to be delivered exactly once.
	stream := buildStream(
		`[["wrb.fr",null,"{\"step\":1}"]]`,
		`[["wrb.fr",null,"{\"step\":2}"]]`,
		`[["wrb.fr",null,"{\"step\":3}"]]`,
	)
	frames := NewFrameParser().Feed(stream)
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	for i, frame := range frames {
		want := fmt.Sprintf(`{"step":%d}`, i+1)
		if frame.Payload != want {
			t.Errorf("frame %d payload = %q, want %q", i, frame.Payload, want)
		}
	}
}

func TestFrameParserSkipsNonWrbFr(t *testing.T) {
	// Transport metadata entries are interleaved with results and must be
	// ignored rather than treated as failures.
	stream := buildStream(`[["di",1],["af.httprm",2],["wrb.fr",null,"{\"ok\":1}"]]`)
	frames := NewFrameParser().Feed(stream)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Payload != `{"ok":1}` {
		t.Errorf("payload = %q", frames[0].Payload)
	}
}

func TestFrameParserHandlesSplitPrefix(t *testing.T) {
	// The prefix can arrive split across chunks, which happens whenever the
	// first read is short.
	stream := buildStream(`[["wrb.fr",null,"{}"]]`)
	parser := NewFrameParser()
	var frames []Frame
	frames = append(frames, parser.Feed(stream[:2])...)
	frames = append(frames, parser.Feed(stream[2:])...)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
}

func TestFrameParserIgnoresNoiseBetweenFrames(t *testing.T) {
	// A stray non-numeric line must not derail the parser: the stream is still
	// framed after it.
	payload := `[["wrb.fr",null,"{\"ok\":1}"]]`
	stream := xssiPrefix + "\nnot-a-length\n" + fmt.Sprintf("%d\n%s", UTF16Len(payload), payload)
	frames := NewFrameParser().Feed(stream)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
}

// TestFrameParserRecoversFromAWrongDeclaredLength is the test the file was
// missing.
//
// A real capture declared a length that did not land on a frame boundary. The
// parser consumed past the end of the first frame, bit into the next length
// marker, and from then on found no frames at all — the request came back with
// an empty answer, not an error, so nothing upstream of the parser noticed. The
// only reason it was found at all is that someone counted frames by hand.
//
// The declared length is therefore treated as a hint, not as truth: if it does
// not land on a boundary, the frame is rebuilt from the payload's own brackets.
func TestFrameParserRecoversFromAWrongDeclaredLength(t *testing.T) {
	payloads := []string{
		`[["wrb.fr",null,"{\"step\":1}"]]`,
		`[["wrb.fr",null,"{\"step\":2}"]]`,
		`[["wrb.fr",null,"{\"step\":3}"]]`,
	}

	for _, delta := range []int{-5, -2, -1, 0, 1, 2, 5} {
		delta := delta
		t.Run(fmt.Sprintf("delta%+d", delta), func(t *testing.T) {
			stream := buildStreamDeclaring(func(p string) int { return UTF16Len(p) + delta }, payloads...)

			parser := NewFrameParser()
			var frames []Frame
			// Feed byte by byte as well: the recovery has to work while the
			// stream is still arriving, not only on a complete body.
			for i := 0; i < len(stream); i++ {
				frames = append(frames, parser.Feed(stream[i:i+1])...)
			}

			if len(frames) != len(payloads) {
				t.Fatalf("got %d frames, want %d (delta %+d desynchronised the parser)",
					len(frames), len(payloads), delta)
			}
			for i, frame := range frames {
				want := fmt.Sprintf(`{"step":%d}`, i+1)
				if frame.Payload != want {
					t.Errorf("frame %d payload = %q, want %q", i, frame.Payload, want)
				}
			}
		})
	}
}

// TestFrameParserRecoversWhenTheLengthIsCountedInBytes covers the specific
// mismatch that a Chinese-language reply hits and an English one does not.
//
// If the upstream declares the payload's *byte* length while the parser advances
// in UTF-16 units, the two agree only while the text is ASCII. Each CJK
// character is 3 bytes but 1 unit, so the parser runs past the end of the frame
// by two bytes per character — and the failure is silent.
func TestFrameParserRecoversWhenTheLengthIsCountedInBytes(t *testing.T) {
	payloads := []string{
		`[["wrb.fr",null,"{\"t\":\"你好世界\"}"]]`,
		`[["wrb.fr",null,"{\"t\":\"再见\"}"]]`,
	}

	stream := buildStreamDeclaring(func(p string) int { return len(p) }, payloads...)

	parser := NewFrameParser()
	frames := parser.Feed(stream)
	if len(frames) != len(payloads) {
		t.Fatalf("got %d frames, want %d", len(frames), len(payloads))
	}
	if !strings.Contains(frames[0].Payload, "你好世界") {
		t.Errorf("payload lost its non-ASCII content: %q", frames[0].Payload)
	}
	if !strings.Contains(frames[1].Payload, "再见") {
		t.Errorf("second frame payload = %q", frames[1].Payload)
	}
	if parser.Resyncs() == 0 {
		t.Error("expected the structural path to have been used, but no resync was counted")
	}
}

// TestFrameParserRecoversFromAnAbsurdLength pins the case where the declared
// length is larger than the whole buffer: the parser must fall through to the
// structure instead of waiting forever for bytes that will never satisfy it.
func TestFrameParserRecoversFromAnAbsurdLength(t *testing.T) {
	stream := buildStreamDeclaring(func(string) int { return 999999 },
		`[["wrb.fr",null,"{\"ok\":1}"]]`)

	frames := NewFrameParser().Feed(stream)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 — an unsatisfiable length stalled the parser", len(frames))
	}
	if frames[0].Payload != `{"ok":1}` {
		t.Errorf("payload = %q", frames[0].Payload)
	}
}

// TestFrameParserDoesNotResyncOnAWellFormedStream keeps the fast path honest.
//
// The recovery must not become the normal path: a parser that always rebuilds
// boundaries structurally would pass every test above while quietly ignoring the
// length prefix altogether.
func TestFrameParserDoesNotResyncOnAWellFormedStream(t *testing.T) {
	parser := NewFrameParser()
	frames := parser.Feed(buildStream(
		`[["wrb.fr",null,"{\"a\":1}"]]`,
		`[["wrb.fr",null,"{\"t\":\"你好\"}"]]`,
		`[["wrb.fr",null,"{\"b\":2}"]]`,
	))
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	if parser.Resyncs() != 0 {
		t.Errorf("Resyncs() = %d on a well-formed stream, want 0", parser.Resyncs())
	}
}

func TestBalancedArrayEnd(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  int
		found bool
	}{
		{"plain", `[["wrb.fr",null,"{}"]]`, 22, true},
		{"nested", `[[1,[2,[3]]],4]`, 15, true},
		// A `]` inside a string must not close the frame early. Payloads are
		// full of code and prose, so this is routine rather than exotic.
		{"bracket inside string", `[["a","]"],["b"]]`, 17, true},
		{"escaped quote then bracket", `[["a","\"]"]]`, 13, true},
		{"escaped backslash before quote", `[["a","\\"]]`, 12, true},
		{"leading whitespace", "\n  [1]", 6, true},
		{"incomplete", `[["wrb.fr",null,"{}"`, 0, false},
		{"unclosed string", `[["a","b]`, 0, false},
		{"no bracket", "just text", 0, false},
		{"empty", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := balancedArrayEnd(tc.in)
			if found != tc.found || got != tc.want {
				t.Errorf("balancedArrayEnd(%q) = (%d, %v), want (%d, %v)",
					tc.in, got, found, tc.want, tc.found)
			}
		})
	}
}

func TestFrameBoundaryOK(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},       // ambiguous mid-stream: not confirmable
		{"\n", false},     // trailing newline only: same
		{"  \n\t", false}, // trailing whitespace only: same
		{"123\n[[", true}, // the next length marker
		{"\n\n456\n[[", true},
		{"abc", false},   // mid-payload
		{"123", false},   // a length with no newline yet
		{"12a\n", false}, // digits then something else
		{"{}\n", false},  // payload tail
	}
	for _, tc := range cases {
		if got := frameBoundaryOK(tc.in); got != tc.want {
			t.Errorf("frameBoundaryOK(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDetectErrorCode(t *testing.T) {
	cases := []struct {
		chunk    string
		wantCode int
		wantOK   bool
	}{
		{`...,"BardErrorInfo [1037]",...`, 1037, true},
		{`"BardErrorInfo":[1060]`, 1060, true},
		// The decoded payload of an error frame is an array pair. Missing this
		// shape is the difference between "the pool cools an out-of-quota
		// account" and "the pool retires a perfectly good cookie".
		{`[["BardErrorInfo",[1037]]]`, 1037, true},
		{`no marker here`, 0, false},
		{`"BardErrorInfo"`, 0, false},
		{`BardErrorInfo []`, 0, false},
		// A bare name in prose must not be read as a code.
		{`see BardErrorInfo, and then [1]`, 0, false},
	}
	for _, tc := range cases {
		code, ok := detectErrorCode(tc.chunk)
		if ok != tc.wantOK || code != tc.wantCode {
			t.Errorf("detectErrorCode(%q) = (%d, %v), want (%d, %v)",
				tc.chunk, code, ok, tc.wantCode, tc.wantOK)
		}
	}
}

func TestClassifyCode(t *testing.T) {
	// The mapping decides whether the pool retires an account, so each of these
	// is load-bearing.
	cases := map[int]error{
		codeUnauthenticated:    ErrInvalidCredential,
		codeUsageLimit:         ErrUsageLimit,
		codeModelHeaderInvalid: ErrModelUnavailable,
		codeLocationRejected:   ErrLocationRejected,
		codeTemporaryError:     ErrUpstreamUnavailable,
	}
	for code, want := range cases {
		if got := classifyCode(code); got != want {
			t.Errorf("classifyCode(%d) = %v, want %v", code, got, want)
		}
	}
}
