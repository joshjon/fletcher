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
	"strings"
	"time"

	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/network/wireguard"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

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
	q sqliteq.Querier
}

// NewService wires a Service to a sqlc querier.
func NewService(q sqliteq.Querier) *Service { return &Service{q: q} }

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
