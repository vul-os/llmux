package provider

import (
	"strings"
	"testing"
)

func TestScanSSE(t *testing.T) {
	input := "data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\ndata: ignored\n\n"
	var got []string
	err := ScanSSE(strings.NewReader(input), func(d []byte) error {
		got = append(got, string(d))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("got %v", got)
	}
}

func TestScanSSEMultiline(t *testing.T) {
	// Multi-line data fields are joined with "\n".
	input := "data: line1\ndata: line2\n\n"
	var got []string
	ScanSSE(strings.NewReader(input), func(d []byte) error {
		got = append(got, string(d))
		return nil
	})
	if len(got) != 1 || got[0] != "line1\nline2" {
		t.Fatalf("got %q", got)
	}
}

func TestScanSSEIgnoresComments(t *testing.T) {
	input := ": keep-alive\ndata: {\"x\":1}\n\n"
	var got []string
	ScanSSE(strings.NewReader(input), func(d []byte) error {
		got = append(got, string(d))
		return nil
	})
	if len(got) != 1 || got[0] != `{"x":1}` {
		t.Fatalf("got %v", got)
	}
}

func TestScanSSENoTrailingBlank(t *testing.T) {
	// Final event without a trailing blank line must still flush at EOF.
	input := "data: {\"x\":1}"
	var got []string
	ScanSSE(strings.NewReader(input), func(d []byte) error {
		got = append(got, string(d))
		return nil
	})
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
}
