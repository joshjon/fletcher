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
	// PortForwardPort is the vsock port a session guest listens on for generic
	// host-initiated loopback forwards. The host dials it, writes a 2-byte
	// big-endian target port (WriteDialPort), and the guest splices the rest of
	// the connection to that loopback port inside the VM. This generalises the
	// SSH relay (a fixed loopback:22 forward) to any port a published session
	// serves, so the daemon can broker a preview/published port the same way -
	// the VM still has no network route.
	PortForwardPort = 1027
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
	// AppEnv is extra environment layered onto a run_app session's app process on
	// top of the image's own env (the user-set env vars). Unlike Env (which seeds
	// login shells and exec/shell), these reach the deployed app itself; a key
	// here replaces the same key from the image.
	AppEnv []string `json:"appEnv,omitempty"`
	// WorkDir is the working directory; defaults to "/" if empty.
	WorkDir string `json:"workDir"`
	// Forwards are loopback services to relay to the host before running.
	Forwards []Forward `json:"forwards,omitempty"`
	// Credentials are agent login files the host seeds into a freshly created
	// session at setup (e.g. the box's saved ~/.claude). Sent only on the create
	// path, so a later start never overwrites a token the session refreshed.
	Credentials []CredentialFile `json:"credentials,omitempty"`
}

// CredentialFile is one file seeded into a new session's fork so it boots
// already logged in. The guest writes it as root then hands it to the login
// user (the agent runs as that user and refreshes the token in place).
type CredentialFile struct {
	// Path is the absolute guest path to write (e.g. /home/fletcher/.claude/...).
	Path string `json:"path"`
	// Mode is the file's permission bits (e.g. 0o600 for a token file).
	Mode uint32 `json:"mode"`
	// Data is the file contents.
	Data []byte `json:"data"`
}

// RequestKind is the type of a host->guest control message in session mode.
type RequestKind string

const (
	// RequestSetup brings up the session's service forwards (gateway, MCP) from
	// Request.Spec.Forwards, then acks with a single Exit frame. The host sends
	// it once just after a session boots, since - unlike the ephemeral path - a
	// session guest receives no initial Spec to carry the forwards. Applied once
	// per guest lifetime, so a resend after a hibernation restore is a no-op.
	RequestSetup RequestKind = "setup"
	// RequestExec runs Request.Spec and frames its output back.
	RequestExec RequestKind = "exec"
	// RequestShutdown syncs and resets the VM so the VMM exits cleanly.
	RequestShutdown RequestKind = "shutdown"
	// RequestShell opens an interactive PTY (Request.Shell). After the request
	// the connection is full-duplex: the host sends KindStdin/KindResize frames
	// and the guest sends KindStdout frames, then one KindExit.
	RequestShell RequestKind = "shell"
	// RequestStat asks the guest for a liveness sample (Stat); the host uses it
	// to tell a working session from an idle one before auto-stopping.
	RequestStat RequestKind = "stat"
	// RequestWriteFile uploads a file into the guest fork. The host sends the
	// Request (Kind + File), the guest acks readiness with a FileResult (Error
	// set if it cannot open the destination), then the host streams File.Size
	// raw bytes and the guest replies with a final FileResult (BytesWritten +
	// Sha256, or Error). The write is atomic (temp file + rename) and the file is
	// handed to the login user.
	RequestWriteFile RequestKind = "write_file"
	// RequestReadFile downloads a file out of the guest fork. The host sends the
	// Request (Kind + File.Path), the guest replies with a FileResult (Size +
	// Mode, or Error), then streams that many raw bytes.
	RequestReadFile RequestKind = "read_file"
	// RequestListDir lists a directory in the guest fork. The host sends the
	// Request (Kind + File.Path), the guest replies with a DirListing. Served in
	// pure Go (os.ReadDir), so it works on an image with no shell.
	RequestListDir RequestKind = "list_dir"
)

// DirEntry is one entry in a guest directory listing.
type DirEntry struct {
	Name          string `json:"name"`
	Size          int64  `json:"size,omitempty"`
	Mode          uint32 `json:"mode,omitempty"`
	IsDir         bool   `json:"isDir,omitempty"`
	ModTime       int64  `json:"modTime,omitempty"`
	IsSymlink     bool   `json:"isSymlink,omitempty"`
	SymlinkTarget string `json:"symlinkTarget,omitempty"`
}

// DirListing is the guest's reply to a RequestListDir: the resolved directory,
// its entries, and whether the listing was capped. Error is non-empty on failure
// (e.g. the path is missing or not a directory).
type DirListing struct {
	Path      string     `json:"path,omitempty"`
	Entries   []DirEntry `json:"entries,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// WriteDirListing sends a DirListing as a length-prefixed JSON message.
func WriteDirListing(w io.Writer, l DirListing) error { return writeJSON(w, l) }

// ReadDirListing reads a DirListing written by WriteDirListing.
func ReadDirListing(r io.Reader) (DirListing, error) {
	var l DirListing
	err := readJSON(r, &l)
	return l, err
}

// FileSpec names a file to transfer and, for a write, how many bytes follow.
type FileSpec struct {
	// Path is the file inside the guest. Absolute is used as-is; relative
	// resolves under the login user's home.
	Path string `json:"path"`
	// Mode is the destination's unix permission bits for a write (0 uses 0644).
	Mode uint32 `json:"mode,omitempty"`
	// Size is the number of raw bytes that follow the request, for a write.
	Size int64 `json:"size,omitempty"`
}

// FileResult is the guest's reply to a file transfer. For a read it carries the
// file's size and mode; for a write, the bytes written and the content hash.
// Error is non-empty when the operation failed.
type FileResult struct {
	Size         int64  `json:"size,omitempty"`
	Mode         uint32 `json:"mode,omitempty"`
	BytesWritten int64  `json:"bytesWritten,omitempty"`
	Sha256       string `json:"sha256,omitempty"`
	Error        string `json:"error,omitempty"`
}

// WriteFileResult sends a FileResult as a length-prefixed JSON message.
func WriteFileResult(w io.Writer, r FileResult) error { return writeJSON(w, r) }

// ReadFileResult reads a FileResult written by WriteFileResult.
func ReadFileResult(r io.Reader) (FileResult, error) {
	var fr FileResult
	err := readJSON(r, &fr)
	return fr, err
}

// Stat is the guest's liveness sample, returned for a RequestStat.
type Stat struct {
	// Load1 is the 1-minute load average: a proxy for in-guest work in flight
	// (an agent or task running even with no host connection attached).
	Load1 float64 `json:"load1"`
	// AppRestarts is how many times the app supervisor has restarted a run_app
	// session's app since the VM booted (0 for a non-app session).
	AppRestarts int64 `json:"app_restarts"`
}

// WriteStat sends a Stat as a length-prefixed JSON message.
func WriteStat(w io.Writer, stat Stat) error { return writeJSON(w, stat) }

// ReadStat reads a Stat written by WriteStat.
func ReadStat(r io.Reader) (Stat, error) {
	var stat Stat
	err := readJSON(r, &stat)
	return stat, err
}

// WriteDialPort writes the 2-byte big-endian target-port header the guest's
// generic port-forward relay (PortForwardPort) reads to learn which loopback
// port to splice the rest of the connection to.
func WriteDialPort(w io.Writer, port uint16) error {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], port)
	_, err := w.Write(b[:])
	return err
}

// ReadDialPort reads the target-port header written by WriteDialPort.
func ReadDialPort(r io.Reader) (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b[:]), nil
}

// ShellSpec parameterises an interactive PTY session.
type ShellSpec struct {
	// Term is the TERM value set inside the PTY (e.g. xterm-256color).
	Term string `json:"term"`
	// Cols and Rows are the initial window size.
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
	// Env is extra environment for the login shell.
	Env []string `json:"env,omitempty"`
	// ControlMode runs tmux in control mode (tmux -CC); the stream then carries
	// the tmux control protocol instead of a rendered terminal.
	ControlMode bool `json:"control_mode,omitempty"`
}

// Request is one host->guest control message a session guest serves. The
// ephemeral path uses Spec directly; sessions wrap it so the same connection
// can also carry a clean shutdown or an interactive shell.
type Request struct {
	Kind  RequestKind `json:"kind"`
	Spec  Spec        `json:"spec,omitempty"`
	Shell ShellSpec   `json:"shell,omitempty"`
	File  FileSpec    `json:"file,omitempty"`
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
