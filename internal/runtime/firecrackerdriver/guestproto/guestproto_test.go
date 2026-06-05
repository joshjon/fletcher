package guestproto_test

import (
	"bytes"
	"testing"

	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

func TestSpecRoundTrip(t *testing.T) {
	want := guestproto.Spec{
		Command: "echo hi && exit 3",
		Env:     []string{"PATH=/bin", "HOME=/root"},
		WorkDir: "/workspace",
	}
	var buf bytes.Buffer
	if err := guestproto.WriteSpec(&buf, want); err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	got, err := guestproto.ReadSpec(&buf)
	if err != nil {
		t.Fatalf("ReadSpec: %v", err)
	}
	if got.Command != want.Command || got.WorkDir != want.WorkDir || len(got.Env) != len(want.Env) {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payloads := []struct {
		kind byte
		data []byte
	}{
		{guestproto.KindStdout, []byte("hello")},
		{guestproto.KindStderr, []byte("oops")},
		{guestproto.KindStdout, nil}, // empty frame is valid
		{guestproto.KindExit, guestproto.EncodeExit(7)},
	}
	for _, p := range payloads {
		if err := guestproto.WriteFrame(&buf, p.kind, p.data); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	for i, want := range payloads {
		kind, data, err := guestproto.ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame[%d]: %v", i, err)
		}
		if kind != want.kind {
			t.Errorf("frame[%d] kind = %d, want %d", i, kind, want.kind)
		}
		if !bytes.Equal(data, want.data) {
			t.Errorf("frame[%d] data = %q, want %q", i, data, want.data)
		}
	}
}

func TestExitCodec(t *testing.T) {
	for _, code := range []int32{0, 1, 7, 127, 255} {
		got, err := guestproto.DecodeExit(guestproto.EncodeExit(code))
		if err != nil {
			t.Fatalf("DecodeExit(%d): %v", code, err)
		}
		if got != code {
			t.Errorf("exit round trip = %d, want %d", got, code)
		}
	}
	if _, err := guestproto.DecodeExit([]byte{1, 2}); err == nil {
		t.Error("expected error decoding a short exit payload")
	}
}
