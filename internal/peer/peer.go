// Package peer owns the daemon's WireGuard-peer registry. A peer is a
// device (phone, laptop) authorised to connect to the daemon over
// WireGuard; the daemon stores its name, public key, and allowed-IPs
// claim. Private keys for peer-side devices are generated locally at
// Create time and returned to the caller exactly once - the daemon
// retains only the public half.
package peer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/network/wireguard"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// DefaultTunnelCIDR is the WireGuard subnet new peers are allocated
// from when `fletcher peer pair` is used without explicit addressing.
// The .1 address is reserved for the server interface; peer pairing
// hands out .2 through .254.
const DefaultTunnelCIDR = "10.99.0.0/24"

// ErrNotFound is returned when a peer ID does not exist.
var ErrNotFound = errs.New(errs.CategoryNotFound, "peer not found")

// ErrNameTaken is returned when CreatePeer would collide on the unique name.
var ErrNameTaken = errs.New(errs.CategoryConflict, "peer name already exists")

// idPrefix is the typeid prefix for peer IDs.
const idPrefix = "peer"

// Peer is the domain shape of a peers row.
type Peer struct {
	ID         string
	Name       string
	PublicKey  wireguard.Key
	AllowedIPs []string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// CreateParams configures a new peer.
type CreateParams struct {
	Name string
	// AllowedIPs are the addresses the daemon will route to this peer
	// (typically a single /32 inside the WireGuard subnet).
	AllowedIPs []string
}

// Created bundles the persisted Peer with the one-time secret returned to
// the caller. PrivateKey is non-empty only on Create - the daemon does
// not store it.
type Created struct {
	Peer       Peer
	PrivateKey wireguard.Key
}

// Service is the high-level peers API.
type Service struct {
	q          sqliteq.Querier
	tunnelCIDR string

	mu             sync.RWMutex
	publicEndpoint string
}

// Options configures a Service's pair-time defaults.
type Options struct {
	// TunnelCIDR is the subnet `peer pair` allocates /32s from. Empty
	// uses DefaultTunnelCIDR (10.99.0.0/24).
	TunnelCIDR string
	// PublicEndpoint is the host:port (e.g. "home.example.com:51820")
	// that `peer pair` renders into client wg-quick configs. Empty
	// causes PairPeer to fail with a clear error pointing at how to
	// set it; CreatePeer with an explicit server_endpoint still works.
	PublicEndpoint string
}

// NewService wires a Service to a sqlc querier with the given options.
func NewService(q sqliteq.Querier, opts Options) *Service {
	cidr := opts.TunnelCIDR
	if cidr == "" {
		cidr = DefaultTunnelCIDR
	}
	return &Service{q: q, tunnelCIDR: cidr, publicEndpoint: opts.PublicEndpoint}
}

// TunnelCIDR returns the subnet used for auto-allocation. The caller
// renders this into the server-side AllowedIPs when needed.
func (s *Service) TunnelCIDR() string { return s.tunnelCIDR }

// PublicEndpoint returns the operator-configured host:port peers should
// dial, or "" if unset. Returned for use in pair-time config rendering.
func (s *Service) PublicEndpoint() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.publicEndpoint
}

// SetPublicEndpoint replaces the public endpoint advertised in pair-time
// configs. Used by the daemon's networking setup when UPnP discovery
// produces an endpoint the operator didn't supply at boot.
func (s *Service) SetPublicEndpoint(endpoint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publicEndpoint = endpoint
}

// NextAvailableAddress returns the next free /32 inside the service's
// TunnelCIDR, suitable for assigning to a new peer. The .1 host address
// is reserved for the server interface; allocation starts at .2 and
// scans existing peers to skip ones already claimed. Returns a
// CategoryConflict error if the subnet is exhausted.
func (s *Service) NextAvailableAddress(ctx context.Context) (string, error) {
	prefix, err := netip.ParsePrefix(s.tunnelCIDR)
	if err != nil {
		return "", fmt.Errorf("parse tunnel cidr %q: %w", s.tunnelCIDR, err)
	}
	taken, err := s.takenAddresses(ctx)
	if err != nil {
		return "", err
	}
	server := prefix.Addr().Next()
	candidate := server.Next()
	for prefix.Contains(candidate) && candidate.Next().IsValid() {
		if !taken[candidate] && !candidate.IsMulticast() {
			return candidate.String() + "/32", nil
		}
		candidate = candidate.Next()
	}
	return "", errs.Newf(errs.CategoryConflict, "tunnel subnet %s is exhausted", s.tunnelCIDR)
}

// takenAddresses returns the set of host addresses already claimed by
// existing peers (parsing each peer's AllowedIPs).
func (s *Service) takenAddresses(ctx context.Context) (map[netip.Addr]bool, error) {
	rows, err := s.q.ListPeers(ctx, sqliteq.ListPeersParams{Limit: 1 << 30, Offset: 0})
	if err != nil {
		return nil, fmt.Errorf("list peers for allocation: %w", err)
	}
	taken := make(map[netip.Addr]bool, len(rows))
	for _, r := range rows {
		for _, raw := range strings.Split(r.AllowedIps, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			if p, perr := netip.ParsePrefix(raw); perr == nil {
				taken[p.Addr()] = true
				continue
			}
			if a, aerr := netip.ParseAddr(raw); aerr == nil {
				taken[a] = true
			}
		}
	}
	return taken, nil
}

// Create generates a fresh keypair, persists the peer with the public
// half, and returns both halves so the caller can hand the private key
// to the device once.
func (s *Service) Create(ctx context.Context, p CreateParams) (Created, error) {
	if err := p.validate(); err != nil {
		return Created{}, err
	}
	if existing, err := s.q.GetPeerByName(ctx, p.Name); err == nil && existing.ID != "" {
		return Created{}, ErrNameTaken
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Created{}, fmt.Errorf("lookup name: %w", err)
	}

	kp, err := wireguard.GenerateKeypair()
	if err != nil {
		return Created{}, err
	}
	id, err := typeid.WithPrefix(idPrefix)
	if err != nil {
		return Created{}, fmt.Errorf("generate id: %w", err)
	}
	now := time.Now().Unix()
	row, err := s.q.CreatePeer(ctx, sqliteq.CreatePeerParams{
		ID:         id.String(),
		Name:       p.Name,
		PublicKey:  string(kp.Public),
		AllowedIps: strings.Join(p.AllowedIPs, ","),
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		return Created{}, fmt.Errorf("create peer: %w", err)
	}
	return Created{Peer: peerFromRow(row), PrivateKey: kp.Private}, nil
}

// Get returns a peer by ID.
func (s *Service) Get(ctx context.Context, id string) (Peer, error) {
	row, err := s.q.GetPeer(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Peer{}, ErrNotFound
	}
	if err != nil {
		return Peer{}, fmt.Errorf("get peer: %w", err)
	}
	return peerFromRow(row), nil
}

// List returns peers newest-first.
func (s *Service) List(ctx context.Context, limit, offset int32) ([]Peer, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.q.ListPeers(ctx, sqliteq.ListPeersParams{
		Limit:  int64(limit),
		Offset: int64(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	out := make([]Peer, len(rows))
	for i, r := range rows {
		out[i] = peerFromRow(r)
	}
	return out, nil
}

// Delete removes a peer. Returns false (no error) if the peer was missing.
func (s *Service) Delete(ctx context.Context, id string) (bool, error) {
	n, err := s.q.DeletePeer(ctx, id)
	if err != nil {
		return false, fmt.Errorf("delete peer: %w", err)
	}
	return n > 0, nil
}

func (p CreateParams) validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errs.New(errs.CategoryInvalidArgument, "name is required")
	}
	if len(p.AllowedIPs) == 0 {
		return errs.New(errs.CategoryInvalidArgument, "at least one allowed_ip is required")
	}
	return nil
}

func peerFromRow(r sqliteq.Peer) Peer {
	allowed := []string{}
	if r.AllowedIps != "" {
		allowed = strings.Split(r.AllowedIps, ",")
	}
	return Peer{
		ID:         r.ID,
		Name:       r.Name,
		PublicKey:  wireguard.Key(r.PublicKey),
		AllowedIPs: allowed,
		CreatedAt:  time.Unix(r.CreatedAt, 0).UTC(),
		UpdatedAt:  time.Unix(r.UpdatedAt, 0).UTC(),
	}
}
