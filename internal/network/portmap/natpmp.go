package portmap

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// natpmpServerPort is the well-known UDP port the gateway listens on for
// NAT-PMP (RFC 6886) and PCP (RFC 6887) requests.
const natpmpServerPort = 5351

// natpmpOpcode maps a Protocol to its NAT-PMP map opcode (UDP=1, TCP=2).
func natpmpOpcode(p Protocol) (byte, bool) {
	switch p {
	case ProtocolUDP:
		return 1, true
	case ProtocolTCP:
		return 2, true
	default:
		return 0, false
	}
}

// mapNATPMP installs a port mapping on the gateway via NAT-PMP. It first
// asks the gateway for its external address (so the caller can derive the
// public endpoint), then requests the mapping. A lifetime of 0 deletes the
// mapping. Errors are informational: the caller falls back to UPnP.
//
// NAT-PMP matters because some routers (including ones that ship a working
// NAT-PMP responder) silently ignore UPnP TCP mapping requests - they
// return success over SOAP but never install the forward. NAT-PMP is the
// documented fallback for exactly that case.
func mapNATPMP(ctx context.Context, req Request, gateway net.IP) (Result, error) {
	op, ok := natpmpOpcode(req.Protocol)
	if !ok {
		return Result{}, fmt.Errorf("natpmp: invalid protocol %q", req.Protocol)
	}
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: gateway, Port: natpmpServerPort})
	if err != nil {
		return Result{}, fmt.Errorf("natpmp dial %s: %w", gateway, err)
	}
	defer func() { _ = conn.Close() }()

	// External-address request (opcode 0): version 0, opcode 0.
	extResp, err := natpmpExchange(ctx, conn, []byte{0, 0}, 12)
	if err != nil {
		return Result{}, fmt.Errorf("natpmp external address: %w", err)
	}
	if rc := binary.BigEndian.Uint16(extResp[2:4]); rc != 0 {
		return Result{}, fmt.Errorf("natpmp external address result code %d", rc)
	}
	externalIP := net.IPv4(extResp[8], extResp[9], extResp[10], extResp[11]).String()

	lifetime := uint32(req.LeaseDuration / time.Second) //nolint:gosec // a lease in seconds fits a uint32
	// Map request: version 0, opcode, 2 reserved, internal port, suggested
	// external port, lifetime (seconds).
	body := make([]byte, 12)
	body[1] = op
	binary.BigEndian.PutUint16(body[4:6], req.InternalPort)
	binary.BigEndian.PutUint16(body[6:8], req.ExternalPort)
	binary.BigEndian.PutUint32(body[8:12], lifetime)

	mapResp, err := natpmpExchange(ctx, conn, body, 16)
	if err != nil {
		return Result{}, fmt.Errorf("natpmp map: %w", err)
	}
	if rc := binary.BigEndian.Uint16(mapResp[2:4]); rc != 0 {
		return Result{}, fmt.Errorf("natpmp map result code %d", rc)
	}
	return Result{
		Method:        "nat-pmp",
		ExternalIP:    externalIP,
		ExternalPort:  binary.BigEndian.Uint16(mapResp[10:12]),
		LeaseDuration: time.Duration(binary.BigEndian.Uint32(mapResp[12:16])) * time.Second,
	}, nil
}

// unmapNATPMP deletes a mapping by requesting it with a zero lifetime and
// zero external port, per RFC 6886.
func unmapNATPMP(ctx context.Context, req Request, gateway net.IP) error {
	op, ok := natpmpOpcode(req.Protocol)
	if !ok {
		return fmt.Errorf("natpmp: invalid protocol %q", req.Protocol)
	}
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: gateway, Port: natpmpServerPort})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	body := make([]byte, 12)
	body[1] = op
	binary.BigEndian.PutUint16(body[4:6], req.InternalPort)
	// External port 0 + lifetime 0 = delete.
	_, err = natpmpExchange(ctx, conn, body, 16)
	return err
}

// natpmpExchange sends a request and reads the matching response, retrying
// with the spec's escalating timeouts. minLen guards against truncated
// replies; the version byte must be 0.
func natpmpExchange(ctx context.Context, conn *net.UDPConn, body []byte, minLen int) ([]byte, error) {
	buf := make([]byte, 32)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		timeout := time.Duration(attempt+1) * time.Second
		if d, ok := ctx.Deadline(); ok {
			if remaining := time.Until(d); remaining < timeout {
				timeout = remaining
			}
		}
		_ = conn.SetDeadline(time.Now().Add(timeout))
		if _, err := conn.Write(body); err != nil {
			lastErr = err
			continue
		}
		n, err := conn.Read(buf)
		if err != nil {
			lastErr = err
			continue
		}
		if n < minLen {
			lastErr = fmt.Errorf("natpmp short response: %d bytes", n)
			continue
		}
		if buf[0] != 0 {
			lastErr = fmt.Errorf("natpmp unexpected version %d", buf[0])
			continue
		}
		return buf[:n], nil
	}
	if lastErr == nil {
		lastErr = errors.New("natpmp: no response")
	}
	return nil, lastErr
}

// defaultGatewayV4 returns the IPv4 default-route gateway by parsing
// /proc/net/route. NAT-PMP/PCP requests are addressed to the gateway
// directly (unlike UPnP, which discovers via SSDP multicast). Returns an
// error on non-Linux hosts (no /proc) so the caller falls back to UPnP.
func defaultGatewayV4() (net.IP, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Scan() // skip header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		// Default route: destination 0.0.0.0 with the RTF_GATEWAY flag set.
		if fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&0x2 == 0 { // 0x2 = RTF_GATEWAY
			continue
		}
		ip, err := hexToIPv4LE(fields[2])
		if err != nil {
			continue
		}
		return ip, nil
	}
	return nil, errors.New("no IPv4 default gateway found")
}

// hexToIPv4LE decodes the little-endian hex IPv4 form /proc/net/route uses
// (e.g. "0100A8C0" -> 192.168.0.1).
func hexToIPv4LE(h string) (net.IP, error) {
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return nil, err
	}
	return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).To4(), nil //nolint:gosec // extracting the 4 octets of a 32-bit address
}
