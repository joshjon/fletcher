// Package guestproto is the wire protocol between the daemon (host) and the
// fletcher guest agent running as init inside a Firecracker microVM. It is
// platform-neutral so both the host driver and the cmd/fletcher-guest binary
// share one definition.
//
// Transport is a single vsock stream. The guest, once booted, dials the host
// (CID 2) on Port; the host has a listener waiting. Over that connection:
//
//	host  -> guest : the job Spec (length-prefixed JSON), exactly once
//	guest -> host  : a sequence of output frames, then one Exit frame
//
// Output frames carry the job command's stdout/stderr; the Exit frame carries
// its exit code. The host demultiplexes the streams back to the caller's
// writers, so the job sees its program's real output - not the VM console.
package guestproto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// Port is the vsock port the guest dials on the host. Arbitrary but fixed.
	Port = 1024
	// HostCID is the well-known vsock context ID of the host (VMADDR_CID_HOST).
	HostCID = 2
	// GuestCID is the context ID assigned to the microVM's vsock device.
	GuestCID = 3
)

// Spec is the job description the host sends the guest.
type Spec struct {
	// Command is run via `/bin/sh -c`.
	Command string `json:"command"`
	// Env is the environment ("KEY=value" entries) for the command.
	Env []string `json:"env"`
	// WorkDir is the working directory; defaults to "/" if empty.
	WorkDir string `json:"workDir"`
}

// Frame kinds.
const (
	KindStdout byte = 1
	KindStderr byte = 2
	KindExit   byte = 3
)

// maxFrame caps a single frame's payload so a corrupt length can't make the
// reader allocate unbounded memory.
const maxFrame = 16 << 20

// WriteSpec sends spec as a length-prefixed JSON message.
func WriteSpec(w io.Writer, spec Spec) error {
	data, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal spec: %w", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data))) //nolint:gosec // JSON spec is far below 4 GiB
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// ReadSpec reads a spec written by WriteSpec.
func ReadSpec(r io.Reader) (Spec, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Spec{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return Spec{}, fmt.Errorf("spec too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Spec{}, err
	}
	var spec Spec
	if err := json.Unmarshal(buf, &spec); err != nil {
		return Spec{}, fmt.Errorf("unmarshal spec: %w", err)
	}
	return spec, nil
}

// WriteFrame writes one framed message: [kind][uint32 len][payload].
func WriteFrame(w io.Writer, kind byte, payload []byte) error {
	var hdr [5]byte
	hdr[0] = kind
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload))) //nolint:gosec // payload is capped at maxFrame
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads one framed message. It returns io.EOF cleanly when the guest
// closes the connection after its final frame.
func ReadFrame(r io.Reader) (kind byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame too large: %d bytes", n)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// EncodeExit renders an exit code as the 4-byte payload of an Exit frame.
func EncodeExit(code int32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(code)) //nolint:gosec // deliberate int32<->uint32 reinterpret
	return b[:]
}

// DecodeExit parses an Exit frame payload back into an exit code.
func DecodeExit(payload []byte) (int32, error) {
	if len(payload) != 4 {
		return 0, errors.New("exit frame payload must be 4 bytes")
	}
	return int32(binary.BigEndian.Uint32(payload)), nil //nolint:gosec // deliberate uint32<->int32 reinterpret
}
