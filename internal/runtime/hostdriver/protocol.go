package hostdriver

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"regexp"

	"github.com/joshjon/fletcher/internal/runtime"
)

const (
	maxPacket       = 4 << 20
	protocolVersion = 1
)

var (
	sessionIDPattern = regexp.MustCompile(`^session_[0-9a-hjkmnp-tv-z]{26}$`)
	userPattern      = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

type request struct {
	Version int
	Op      string
	Session runtime.HostSessionSpec
	Shell   runtime.ShellSpec
	Path    string
}

type packet struct {
	Version int                 `json:",omitempty"`
	Error   string              `json:",omitempty"`
	Account runtime.HostAccount `json:",omitzero"`
	Listing runtime.DirListing  `json:",omitzero"`
	Kind    string              `json:",omitempty"`
	Data    []byte              `json:",omitempty"`
	Size    runtime.WinSize     `json:",omitzero"`
	Exit    int32               `json:",omitempty"`
}

func writePacket(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode host executor packet: %w", err)
	}
	if len(data) > maxPacket {
		return fmt.Errorf("host executor packet exceeds limit")
	}
	//nolint:gosec // packet length was bounded to maxPacket above
	if err := binary.Write(w, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func readPacket(r io.Reader, v any) error {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return err
	}
	if n == 0 || n > maxPacket {
		return fmt.Errorf("invalid host executor packet size")
	}
	data := make([]byte, int(n))
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode host executor packet: %w", err)
	}
	return nil
}
