package peer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/network/wireguard"
)

// PendingPairTTL is how long a pairing code stays valid after BeginPair
// before the slot is reaped and the reserved address released.
const PendingPairTTL = 10 * time.Minute

// BeginPairResult is the host-facing result of BeginPair: the issued
// pairing code, the reserved tunnel address, and the slot's deadline.
type BeginPairResult struct {
	Code      string
	Address   string
	ExpiresAt time.Time
}

// CompletedPair is the host-facing result of CompletePair: the
// registered peer plus its one-time API token.
type CompletedPair struct {
	Peer     Peer
	APIToken string
}

// pendingPair is one in-flight BeginPair slot.
type pendingPair struct {
	name      string
	address   string
	expiresAt time.Time
}

// pendingPairs holds in-flight BeginPair slots keyed by pairing code.
// Lazy expiry: every operation sweeps expired entries before acting,
// so the size is bounded by the number of concurrent in-progress
// pairings within a PendingPairTTL window.
type pendingPairs struct {
	mu   sync.Mutex
	pool map[string]pendingPair
}

func newPendingPairs() *pendingPairs {
	return &pendingPairs{pool: make(map[string]pendingPair)}
}

// addresses returns the /32s currently reserved by in-flight pairings,
// sweeping expired entries as a side effect.
func (p *pendingPairs) addresses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	out := make([]string, 0, len(p.pool))
	for _, slot := range p.pool {
		out = append(out, slot.address)
	}
	return out
}

// reserve atomically picks a free address (via the supplied closure,
// which gets the current pending-reserved set) and records a new slot
// with the given TTL. Used by BeginPair so the address pick and the
// slot write happen under one lock.
func (p *pendingPairs) reserve(
	name string,
	pick func(reserved []string) (string, error),
	ttl time.Duration,
) (BeginPairResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	addr, err := pick(p.reservedLocked())
	if err != nil {
		return BeginPairResult{}, err
	}
	code, err := generatePairingCode()
	if err != nil {
		return BeginPairResult{}, err
	}
	expiresAt := time.Now().Add(ttl)
	p.pool[code] = pendingPair{name: name, address: addr, expiresAt: expiresAt}
	return BeginPairResult{Code: code, Address: addr, ExpiresAt: expiresAt}, nil
}

// redeem consumes a pairing code once. The slot is removed on success;
// the caller is responsible for any further work (persisting the
// peer). Returns CategoryUnauthenticated on unknown/expired codes and
// CategoryInvalidArgument when the supplied name does not match the
// one the slot was issued for.
func (p *pendingPairs) redeem(code, name string) (pendingPair, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	slot, ok := p.pool[code]
	if !ok {
		return pendingPair{}, errs.New(errs.CategoryUnauthenticated, "pairing code is invalid or expired")
	}
	if slot.name != name {
		return pendingPair{}, errs.New(errs.CategoryInvalidArgument, "pairing code was issued for a different name")
	}
	delete(p.pool, code)
	return slot, nil
}

func (p *pendingPairs) sweepLocked() {
	now := time.Now()
	for k, slot := range p.pool {
		if now.After(slot.expiresAt) {
			delete(p.pool, k)
		}
	}
}

func (p *pendingPairs) reservedLocked() []string {
	out := make([]string, 0, len(p.pool))
	for _, slot := range p.pool {
		out = append(out, slot.address)
	}
	return out
}

// generatePairingCode returns a 128-bit base64url one-time code.
func generatePairingCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate pairing code: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// BeginPair starts a client-keygen pairing flow used by native clients
// (iOS) that generate their own WireGuard keypair locally. The daemon
// reserves a tunnel address, mints a pairing code with PendingPairTTL,
// and returns the slot details. No peer row is committed until
// CompletePair redeems the code with the client's public key.
//
// Fails with CategoryFailedPrecondition if the daemon has no
// public-endpoint configured, CategoryConflict if the name is taken,
// CategoryInvalidArgument on empty name.
func (s *Service) BeginPair(ctx context.Context, name string) (BeginPairResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return BeginPairResult{}, errs.New(errs.CategoryInvalidArgument, "name is required")
	}
	if s.PublicEndpoint() == "" {
		return BeginPairResult{}, errs.New(errs.CategoryFailedPrecondition,
			"daemon has no public-endpoint configured; restart with --public-endpoint <host:port> or set FLETCHER_PUBLIC_ENDPOINT")
	}
	if _, err := s.q.GetPeerByName(ctx, name); err == nil {
		return BeginPairResult{}, ErrNameTaken
	} else if !errors.Is(err, sql.ErrNoRows) {
		return BeginPairResult{}, fmt.Errorf("lookup name: %w", err)
	}
	return s.pending.reserve(name, func(reserved []string) (string, error) {
		return s.nextAvailableAddress(ctx, reserved)
	}, PendingPairTTL)
}

// CompletePair finishes a client-keygen pairing flow. The caller
// supplies the pairing code from BeginPair, the same name, and the
// WireGuard public key the client generated locally. The daemon
// registers the peer with that public key and returns the per-peer
// API token. The private half never enters the daemon.
//
// Fails with CategoryUnauthenticated on unknown/expired codes,
// CategoryInvalidArgument on a name mismatch or malformed public key.
func (s *Service) CompletePair(ctx context.Context, code, name string, clientPub wireguard.Key) (CompletedPair, error) {
	if err := wireguard.ValidatePublicKey(clientPub); err != nil {
		return CompletedPair{}, errs.Wrap(err, errs.CategoryInvalidArgument)
	}
	slot, err := s.pending.redeem(code, name)
	if err != nil {
		return CompletedPair{}, err
	}
	created, err := s.CreateWithPublicKey(ctx, CreateWithPublicKeyParams{
		Name:       slot.name,
		AllowedIPs: []string{slot.address},
		PublicKey:  clientPub,
	})
	if err != nil {
		return CompletedPair{}, err
	}
	return CompletedPair(created), nil
}
