package provider

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// collect runs ScanSSE over input and returns the dispatched payloads as
// strings plus any error.
func collect(t *testing.T, input string) ([]string, error) {
	t.Helper()
	var got []string
	err := ScanSSE(strings.NewReader(input), func(d []byte) error {
		got = append(got, string(d))
		return nil
	})
	return got, err
}

func TestScanSSECRLF(t *testing.T) {
	// CRLF line endings: TrimRight strips "\r\n" so events parse normally.
	input := "data: {\"a\":1}\r\n\r\ndata: {\"b\":2}\r\n\r\n"
	got, err := collect(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("got %v", got)
	}
}

func TestScanSSEMultilineJSON(t *testing.T) {
	// A JSON object split across many "data:" continuation lines is joined
	// with "\n" and must parse as valid JSON once reassembled.
	input := "data: {\n" +
		"data: \"k\": \"v\",\n" +
		"data: \"n\": 42\n" +
		"data: }\n" +
		"\n"
	got, err := collect(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 payload, got %v", got)
	}
	var obj struct {
		K string `json:"k"`
		N int    `json:"n"`
	}
	if err := json.Unmarshal([]byte(got[0]), &obj); err != nil {
		t.Fatalf("reassembled payload not valid JSON: %v\npayload=%q", err, got[0])
	}
	if obj.K != "v" || obj.N != 42 {
		t.Fatalf("parsed=%+v", obj)
	}
}

func TestScanSSEIgnoresEventIDRetry(t *testing.T) {
	// event:, id:, retry: fields are interleaved and must be ignored; only the
	// data payload is dispatched.
	input := "event: message\n" +
		"id: 123\n" +
		"retry: 5000\n" +
		"data: {\"x\":1}\n" +
		"\n"
	got, err := collect(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != `{"x":1}` {
		t.Fatalf("got %v", got)
	}
}

func TestScanSSELargePayload(t *testing.T) {
	// A single data payload larger than the default 64KiB bufio line size
	// exercises the 1 MiB reader buffer. ReadBytes still returns the full line.
	big := strings.Repeat("a", 100*1024) // 100 KiB
	input := "data: " + big + "\n\n"
	got, err := collect(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 payload, got %d", len(got))
	}
	if len(got[0]) != len(big) || got[0] != big {
		t.Fatalf("payload truncated: len=%d want=%d", len(got[0]), len(big))
	}
}

func TestScanSSEMultipleDONE(t *testing.T) {
	// The first [DONE] stops the stream cleanly (returns nil); any subsequent
	// data is never dispatched.
	input := "data: {\"a\":1}\n\ndata: [DONE]\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\n"
	got, err := collect(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("got %v", got)
	}
}

func TestScanSSEDONEWithSpaces(t *testing.T) {
	// flush compares TrimSpace(payload) == "[DONE]", so surrounding whitespace
	// in the data value is tolerated.
	input := "data:   [DONE]  \n\n"
	got, err := collect(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("[DONE] should not dispatch, got %v", got)
	}
}

func TestScanSSEEmptyInput(t *testing.T) {
	got, err := collect(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no payloads, got %v", got)
	}
}

func TestScanSSEOnlyBlankLines(t *testing.T) {
	// Blank lines with no preceding data must flush nothing.
	got, err := collect(t, "\n\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no payloads, got %v", got)
	}
}

// TestScanSSECallbackErrorPropagates documents that an onData error aborts the
// scan and is returned to the caller.
func TestScanSSECallbackErrorPropagates(t *testing.T) {
	sentinel := errString("boom")
	var calls int
	err := ScanSSE(strings.NewReader("data: a\n\ndata: b\n\n"), func(d []byte) error {
		calls++
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("err=%v want=%v", err, sentinel)
	}
	if calls != 1 {
		t.Fatalf("callback should stop after first error, calls=%d", calls)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// FuzzScanSSE feeds arbitrary bytes through ScanSSE and asserts it never
// panics. It also checks the onData contract: when the scan returns nil, the
// only payload ever withheld is the [DONE] sentinel; every other dispatched
// payload is non-[DONE].
func FuzzScanSSE(f *testing.F) {
	seeds := []string{
		"data: {\"a\":1}\n\n",
		"data: [DONE]\n\n",
		"event: x\ndata: y\n\n",
		": comment\n\n",
		"data: a\ndata: b\n\n",
		"",
		"\r\n\r\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		err := ScanSSE(bytes.NewReader(data), func(d []byte) error {
			// Contract: the [DONE] sentinel is intercepted by ScanSSE and must
			// never reach onData.
			if bytes.Equal(bytes.TrimSpace(d), []byte("[DONE]")) {
				t.Fatalf("onData received [DONE] sentinel: %q", d)
			}
			return nil
		})
		// A bytes.Reader never errors, so ScanSSE must return nil here.
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
