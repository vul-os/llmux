package provider

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// ScanSSE reads a text/event-stream body and invokes onData with the payload of
// each `data:` field. It stops (returning nil) when the upstream sends the
// `[DONE]` sentinel, when the reader is exhausted, or when onData errors.
//
// It handles multi-line data fields (joined with "\n" per the SSE spec) and
// ignores comments and other field types (event:, id:, retry:).
func ScanSSE(r io.Reader, onData func(data []byte) error) error {
	br := bufio.NewReaderSize(r, 1<<20) // 1 MiB lines (big tool-call args)
	var dataBuf bytes.Buffer

	flush := func() error {
		if dataBuf.Len() == 0 {
			return nil
		}
		payload := dataBuf.Bytes()
		dataBuf.Reset()
		if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			return errDone
		}
		return onData(payload)
	}

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			switch {
			case len(trimmed) == 0:
				// Event boundary: dispatch accumulated data.
				if e := flush(); e != nil {
					if e == errDone {
						return nil
					}
					return e
				}
			case bytes.HasPrefix(trimmed, []byte(":")):
				// Comment, ignore.
			case bytes.HasPrefix(trimmed, []byte("data:")):
				chunk := bytes.TrimPrefix(trimmed, []byte("data:"))
				chunk = bytes.TrimPrefix(chunk, []byte(" "))
				if dataBuf.Len() > 0 {
					dataBuf.WriteByte('\n')
				}
				dataBuf.Write(chunk)
			default:
				// Other SSE fields (event:, id:, retry:) are not needed here.
			}
		}
		if err != nil {
			if err == io.EOF {
				if e := flush(); e != nil && e != errDone {
					return e
				}
				return nil
			}
			return err
		}
	}
}

// errDone is the internal sentinel signalling the [DONE] marker.
var errDone = errors.New("sse done")
