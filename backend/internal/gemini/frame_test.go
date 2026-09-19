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
