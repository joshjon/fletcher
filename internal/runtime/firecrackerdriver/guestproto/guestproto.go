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
	// Port is the vsock port the guest dials on the host for the control
	// connection (spec + output). Arbitrary but fixed.
	Port = 1024
	// ControlPort is the vsock port a session guest listens on for
	// host-initiated control connections (exec, shutdown). Unlike Port (which
	// the ephemeral guest dials out on), here the host connects to the guest.
	ControlPort = 1025
	// SSHPort is the vsock port a session guest listens on and relays to its
	// loopback sshd, so the daemon can broker SSH into the VM without the VM
	// having any network route (the preview-proxy pattern, for SSH).
	SSHPort = 1026
	// ForwardPortBase is the first vsock port used for service forwards; the
	// host assigns ForwardPortBase, +1, +2, ... one per Forward.
	ForwardPortBase = 1100
	// HostCID is the well-known vsock context ID of the host (VMADDR_CID_HOST).
	HostCID = 2
	// GuestCID is the context ID assigned to the microVM's vsock device.
	GuestCID = 3
)

// Forward is a loopback service inside the VM relayed to the host over vsock.
// The agent connects to ListenAddr (e.g. the gateway base-URL host:port); the
// guest relays each connection to the host on VsockPort, where the daemon
// proxies it to the matching unix socket. This is how an agent reaches the
// model gateway and MCP server without the VM having any network egress.
type Forward struct {
	// ListenAddr is the TCP address the guest listens on inside the VM.
	ListenAddr string `json:"listenAddr"`
	// VsockPort is the host vsock port the guest relays accepted connections to.
	VsockPort uint32 `json:"vsockPort"`
}

// Spec is the job description the host sends the guest.
type Spec struct {
	// Command is run via `/bin/sh -c`.
	Command string `json:"command"`
	// Env is the environment ("KEY=value" entries) for the command.
	Env []string `json:"env"`
	// WorkDir is the working directory; defaults to "/" if empty.
	WorkDir string `json:"workDir"`
	// Forwards are loopback services to relay to the host before running.
	Forwards []Forward `json:"forwards,omitempty"`
}

// RequestKind is the type of a host->guest control message in session mode.
type RequestKind string

const (
	// RequestExec runs Request.Spec and frames its output back.
	RequestExec RequestKind = "exec"
	// RequestShutdown syncs and resets the VM so the VMM exits cleanly.
	RequestShutdown RequestKind = "shutdown"
	// RequestShell opens an interactive PTY (Request.Shell). After the request
	// the connection is full-duplex: the host sends KindStdin/KindResize frames
	// and the guest sends KindStdout frames, then one KindExit.
	RequestShell RequestKind = "shell"
)

// ShellSpec parameterises an interactive PTY session.
type ShellSpec struct {
	// Term is the TERM value set inside the PTY (e.g. xterm-256color).
	Term string `json:"term"`
	// Cols and Rows are the initial window size.
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
	// Env is extra environment for the login shell.
	Env []string `json:"env,omitempty"`
}

// Request is one host->guest control message a session guest serves. The
// ephemeral path uses Spec directly; sessions wrap it so the same connection
// can also carry a clean shutdown or an interactive shell.
type Request struct {
	Kind  RequestKind `json:"kind"`
	Spec  Spec        `json:"spec,omitempty"`
	Shell ShellSpec   `json:"shell,omitempty"`
}

// Frame kinds. KindStdout/KindStderr/KindExit flow guest->host; KindStdin and
// KindResize flow host->guest on an interactive shell connection.
const (
	KindStdout byte = 1
	KindStderr byte = 2
	KindExit   byte = 3
	KindStdin  byte = 4
	KindResize byte = 5
)

// maxFrame caps a single frame's payload so a corrupt length can't make the
// reader allocate unbounded memory.
const maxFrame = 16 << 20

// WriteSpec sends spec as a length-prefixed JSON message.
func WriteSpec(w io.Writer, spec Spec) error { return writeJSON(w, spec) }

// ReadSpec reads a spec written by WriteSpec.
func ReadSpec(r io.Reader) (Spec, error) {
	var spec Spec
	err := readJSON(r, &spec)
	return spec, err
}

// WriteRequest sends a session control message as a length-prefixed JSON value.
func WriteRequest(w io.Writer, req Request) error { return writeJSON(w, req) }

// ReadRequest reads a request written by WriteRequest.
func ReadRequest(r io.Reader) (Request, error) {
	var req Request
	err := readJSON(r, &req)
	return req, err
}

// writeJSON writes v as a 4-byte-length-prefixed JSON message.
func writeJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data))) //nolint:gosec // JSON message is far below 4 GiB
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// readJSON reads a message written by writeJSON into v.
func readJSON(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return fmt.Errorf("message too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	if err := json.Unmarshal(buf, v); err != nil {
		return fmt.Errorf("unmarshal message: %w", err)
	}
	return nil
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

// EncodeResize renders a window size as the 4-byte payload of a Resize frame.
func EncodeResize(cols, rows uint16) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:], cols)
	binary.BigEndian.PutUint16(b[2:], rows)
	return b[:]
}

// DecodeResize parses a Resize frame payload back into a window size.
func DecodeResize(payload []byte) (cols, rows uint16, err error) {
	if len(payload) != 4 {
		return 0, 0, errors.New("resize frame payload must be 4 bytes")
	}
	return binary.BigEndian.Uint16(payload[0:]), binary.BigEndian.Uint16(payload[2:]), nil
}
