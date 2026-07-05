package guestproto_test

import (
	"bytes"
	"testing"

	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

// FuzzReadFrame feeds arbitrary bytes to the frame reader. It must never
// panic, and the maxFrame cap bounds any allocation a corrupt length prefix
// could ask for. Successful reads must round-trip back to identical bytes.
func FuzzReadFrame(f *testing.F) {
	seed := func(kind byte, payload []byte) []byte {
		var buf bytes.Buffer
		if err := guestproto.WriteFrame(&buf, kind, payload); err != nil {
			f.Fatalf("WriteFrame: %v", err)
		}
		return buf.Bytes()
	}
	f.Add(seed(guestproto.KindStdout, []byte("hello")))
	f.Add(seed(guestproto.KindStderr, nil))
	f.Add(seed(guestproto.KindExit, guestproto.EncodeExit(7)))
	f.Add(seed(guestproto.KindResize, guestproto.EncodeResize(80, 24)))
	// Truncations: header only, partial header, header promising more payload
	// than follows.
	f.Add([]byte{})
	f.Add([]byte{guestproto.KindStdout})
	f.Add([]byte{guestproto.KindStdout, 0, 0, 0})
	f.Add([]byte{guestproto.KindStdout, 0, 0, 0, 5, 'h', 'i'})
	// Length prefix far past the maxFrame cap.
	f.Add([]byte{guestproto.KindExit, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		kind, payload, err := guestproto.ReadFrame(bytes.NewReader(data))
		if err != nil {
			return
		}
		var buf bytes.Buffer
		if err := guestproto.WriteFrame(&buf, kind, payload); err != nil {
			t.Fatalf("re-encode of a successfully read frame: %v", err)
		}
		if !bytes.Equal(buf.Bytes(), data[:buf.Len()]) {
			t.Fatalf("frame did not round-trip: read %q from %q", buf.Bytes(), data)
		}
	})
}

// FuzzReadRequest feeds arbitrary bytes to the length-prefixed JSON reader
// behind every typed message. It must never panic regardless of what the
// length prefix or body contains.
func FuzzReadRequest(f *testing.F) {
	seed := func(req guestproto.Request) []byte {
		var buf bytes.Buffer
		if err := guestproto.WriteRequest(&buf, req); err != nil {
			f.Fatalf("WriteRequest: %v", err)
		}
		return buf.Bytes()
	}
	f.Add(seed(guestproto.Request{Kind: guestproto.RequestExec, Spec: guestproto.Spec{Command: "true"}}))
	f.Add(seed(guestproto.Request{Kind: guestproto.RequestShutdown}))
	f.Add(seed(guestproto.Request{
		Kind:  guestproto.RequestShell,
		Shell: guestproto.ShellSpec{Term: "xterm", Cols: 80, Rows: 24},
	}))
	// Truncated header, truncated body, non-JSON body, oversized length prefix.
	f.Add([]byte{0, 0})
	f.Add([]byte{0, 0, 0, 10, '{', '}'})
	f.Add([]byte{0, 0, 0, 2, 'h', 'i'})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = guestproto.ReadRequest(bytes.NewReader(data))
	})
}
