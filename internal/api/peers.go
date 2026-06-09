package api

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/errs"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/network/wireguard"
	"github.com/joshjon/fletcher/internal/peer"
)

// PeersBackend is the consumer-defined interface the PeersService handler
// needs.
type PeersBackend interface {
	Create(ctx context.Context, p peer.CreateParams) (peer.Created, error)
	Get(ctx context.Context, id string) (peer.Peer, error)
	List(ctx context.Context, limit, offset int32) ([]peer.Peer, error)
	Delete(ctx context.Context, id string) (bool, error)
	// NextAvailableAddress is the auto-allocator backing PairPeer.
	NextAvailableAddress(ctx context.Context) (string, error)
	// BeginPair starts a client-keygen pairing flow; the daemon reserves
	// a tunnel address and pairing code without persisting a peer.
	BeginPair(ctx context.Context, name string) (peer.BeginPairResult, error)
	// CompletePair finishes a client-keygen pairing flow, registering
	// the peer with the caller-supplied public key.
	CompletePair(ctx context.Context, code, name string, clientPub wireguard.Key) (peer.CompletedPair, error)
	// PublicEndpoint returns the operator-configured host:port for
	// PairPeer; empty disables pairing with a clear error.
	PublicEndpoint() string
	// APIEndpoint returns the tunnel-side host:port clients dial to drive
	// the daemon's network API.
	APIEndpoint() string
	// PairingEndpoint returns the public host:port a client dials to call
	// CompletePair before the tunnel is up; empty when no public endpoint
	// or pairing listener is available.
	PairingEndpoint() string
	// PairingTLSFingerprint returns the lowercase hex SHA-256 of the
	// pairing listener's leaf certificate, for the client to pin.
	PairingTLSFingerprint() string
	// TunnelCIDR is the subnet the server side announces as AllowedIPs
	// to peers (so they route only fletcher-network traffic through).
	TunnelCIDR() string
}

// ServerKeyProvider exposes the daemon's WireGuard server identity. It is
// supplied by the daemon (which loads the private half from the secrets
// store at startup).
type ServerKeyProvider interface {
	ServerPrivateKey(ctx context.Context) (wireguard.Key, error)
	ServerPublicKey(ctx context.Context) (wireguard.Key, error)
}

// PeerSyncer pushes the current peer registry into the running
// WireGuard tunnel, if any. Production wires this to a closure that
// rebuilds the list from PeersBackend and calls Tunnel.SetPeers; nil is
// a no-op (Mac dev, no tunnel configured, etc.).
type PeerSyncer interface {
	SyncPeers(ctx context.Context) error
}

// PeersService implements fletcherv1connect.PeerServiceHandler.
type PeersService struct {
	fletcherv1connect.UnimplementedPeerServiceHandler
	peers     PeersBackend
	serverKey ServerKeyProvider
	syncer    PeerSyncer
}

// NewPeersService wires a PeersService. syncer may be nil; when set,
// every peer create/delete pushes the new registry into the running
// tunnel without needing a daemon restart.
func NewPeersService(peers PeersBackend, serverKey ServerKeyProvider, syncer PeerSyncer) *PeersService {
	return &PeersService{peers: peers, serverKey: serverKey, syncer: syncer}
}

// syncPeers fires SyncPeers if a syncer is wired. Failures are logged
// but not returned: a peer is already persisted in the DB at this
// point, and the tunnel will pick up changes on next restart even if
// the live sync fails.
func (s *PeersService) syncPeers(ctx context.Context) {
	if s.syncer == nil {
		return
	}
	if err := s.syncer.SyncPeers(ctx); err != nil {
		// We have no logger handle here; the syncer is expected to log
		// internally before returning. Swallowing keeps the RPC success
		// path clean.
		_ = err
	}
}

// PairPeer is the one-call pairing path: the daemon auto-allocates a
// tunnel IP, uses its configured public_endpoint, and returns a fully-
// rendered client wg-quick config. Fails clearly if public_endpoint
// is unset.
func (s *PeersService) PairPeer(ctx context.Context, req *connect.Request[fletcherv1.PairPeerRequest]) (*connect.Response[fletcherv1.PairPeerResponse], error) {
	endpoint := s.peers.PublicEndpoint()
	if endpoint == "" {
		return nil, errs.New(errs.CategoryFailedPrecondition,
			"daemon has no public-endpoint configured; restart with --public-endpoint <host:port> or set FLETCHER_PUBLIC_ENDPOINT")
	}
	if s.serverKey == nil {
		return nil, errors.New("server key provider not configured")
	}
	address, err := s.peers.NextAvailableAddress(ctx)
	if err != nil {
		return nil, err
	}
	serverPub, err := s.serverKey.ServerPublicKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("load server public key: %w", err)
	}
	created, err := s.peers.Create(ctx, peer.CreateParams{
		Name:       req.Msg.GetName(),
		AllowedIPs: []string{address},
	})
	if err != nil {
		return nil, err
	}
	s.syncPeers(ctx)
	clientCfg := wireguard.RenderClient(wireguard.ClientConfig{
		PrivateKey:          created.PrivateKey,
		Address:             address,
		ServerPublicKey:     serverPub,
		Endpoint:            endpoint,
		AllowedIPs:          []string{s.peers.TunnelCIDR()},
		PersistentKeepalive: 25,
	})
	return connect.NewResponse(&fletcherv1.PairPeerResponse{
		Peer:         peerToProto(created.Peer),
		ClientConfig: clientCfg,
		PrivateKey:   string(created.PrivateKey),
		Address:      address,
		Endpoint:     endpoint,
		ApiToken:     created.APIToken,
		ApiEndpoint:  s.peers.APIEndpoint(),
	}), nil
}

// BeginPair starts a client-keygen pairing flow used by native clients
// (iOS) that generate their own WireGuard keypair locally. The daemon
// reserves a tunnel address and returns the material the client needs
// to assemble its own wg-quick config; no peer row is committed until
// CompletePair is called.
func (s *PeersService) BeginPair(ctx context.Context, req *connect.Request[fletcherv1.BeginPairRequest]) (*connect.Response[fletcherv1.BeginPairResponse], error) {
	if s.serverKey == nil {
		return nil, errors.New("server key provider not configured")
	}
	res, err := s.peers.BeginPair(ctx, req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	serverPub, err := s.serverKey.ServerPublicKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("load server public key: %w", err)
	}
	return connect.NewResponse(&fletcherv1.BeginPairResponse{
		PairingCode:           res.Code,
		ExpiresAt:             res.ExpiresAt.Unix(),
		ServerPublicKey:       string(serverPub),
		Endpoint:              s.peers.PublicEndpoint(),
		Address:               res.Address,
		AllowedIps:            []string{s.peers.TunnelCIDR()},
		ApiEndpoint:           s.peers.APIEndpoint(),
		PersistentKeepalive:   25,
		PairingEndpoint:       s.peers.PairingEndpoint(),
		PairingTlsFingerprint: s.peers.PairingTLSFingerprint(),
	}), nil
}

// CompletePair finishes a client-keygen pairing flow. The caller
// supplies the pairing code, the name from BeginPair, and the
// WireGuard public key it generated locally; the daemon registers the
// peer and returns the per-peer API token. The private half never
// touches the daemon.
func (s *PeersService) CompletePair(ctx context.Context, req *connect.Request[fletcherv1.CompletePairRequest]) (*connect.Response[fletcherv1.CompletePairResponse], error) {
	done, err := s.peers.CompletePair(ctx,
		req.Msg.GetPairingCode(),
		req.Msg.GetName(),
		wireguard.Key(req.Msg.GetClientPublicKey()),
	)
	if err != nil {
		return nil, err
	}
	s.syncPeers(ctx)
	return connect.NewResponse(&fletcherv1.CompletePairResponse{
		Peer:        peerToProto(done.Peer),
		ApiToken:    done.APIToken,
		ApiEndpoint: s.peers.APIEndpoint(),
	}), nil
}

// CreatePeer mints a peer, returns its public-half record + (optionally)
// a rendered client wg-quick config that includes the one-time private key.
func (s *PeersService) CreatePeer(ctx context.Context, req *connect.Request[fletcherv1.CreatePeerRequest]) (*connect.Response[fletcherv1.CreatePeerResponse], error) {
	m := req.Msg
	created, err := s.peers.Create(ctx, peer.CreateParams{
		Name:       m.GetName(),
		AllowedIPs: m.GetAllowedIps(),
	})
	if err != nil {
		return nil, err
	}

	s.syncPeers(ctx)
	resp := &fletcherv1.CreatePeerResponse{
		Peer:       peerToProto(created.Peer),
		PrivateKey: string(created.PrivateKey),
	}
	// Optionally render the client wg-quick config. We render when the
	// caller supplied enough info; otherwise the peer row is created and
	// the caller is expected to render later.
	if m.GetClientAddress() != "" && m.GetServerEndpoint() != "" {
		serverPub, err := s.resolveServerPublicKey(ctx, wireguard.Key(m.GetServerPublicKey()))
		if err != nil {
			return nil, err
		}
		allowed := m.GetClientAllowedIps()
		if len(allowed) == 0 {
			allowed = []string{"10.99.0.0/24"}
		}
		resp.ClientConfig = wireguard.RenderClient(wireguard.ClientConfig{
			PrivateKey:          created.PrivateKey,
			Address:             m.GetClientAddress(),
			DNS:                 m.GetClientDns(),
			ServerPublicKey:     serverPub,
			Endpoint:            m.GetServerEndpoint(),
			AllowedIPs:          allowed,
			PersistentKeepalive: 25,
		})
	}
	return connect.NewResponse(resp), nil
}

// GetPeer returns a peer by id.
func (s *PeersService) GetPeer(ctx context.Context, req *connect.Request[fletcherv1.GetPeerRequest]) (*connect.Response[fletcherv1.GetPeerResponse], error) {
	got, err := s.peers.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.GetPeerResponse{Peer: peerToProto(got)}), nil
}

// ListPeers returns peers newest-first.
func (s *PeersService) ListPeers(ctx context.Context, req *connect.Request[fletcherv1.ListPeersRequest]) (*connect.Response[fletcherv1.ListPeersResponse], error) {
	got, err := s.peers.List(ctx, req.Msg.GetLimit(), req.Msg.GetOffset())
	if err != nil {
		return nil, err
	}
	protos := make([]*fletcherv1.Peer, len(got))
	for i, p := range got {
		protos[i] = peerToProto(p)
	}
	return connect.NewResponse(&fletcherv1.ListPeersResponse{Peers: protos}), nil
}

// DeletePeer removes a peer. Missing IDs return existed=false rather
// than an error.
func (s *PeersService) DeletePeer(ctx context.Context, req *connect.Request[fletcherv1.DeletePeerRequest]) (*connect.Response[fletcherv1.DeletePeerResponse], error) {
	existed, err := s.peers.Delete(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if existed {
		s.syncPeers(ctx)
	}
	return connect.NewResponse(&fletcherv1.DeletePeerResponse{Existed: existed}), nil
}

// ServerConfig renders the daemon-side wg-quick config aggregating every
// registered peer. The response contains the server private key inline,
// so callers should treat it as sensitive (same trust model as a file at
// /etc/wireguard/fletcher.conf).
func (s *PeersService) ServerConfig(ctx context.Context, req *connect.Request[fletcherv1.ServerConfigRequest]) (*connect.Response[fletcherv1.ServerConfigResponse], error) {
	if s.serverKey == nil {
		return nil, errors.New("server key provider not configured")
	}
	priv, err := s.serverKey.ServerPrivateKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("load server key: %w", err)
	}
	pub, err := wireguard.PublicFromPrivate(priv)
	if err != nil {
		return nil, fmt.Errorf("derive public key: %w", err)
	}
	allPeers, err := s.peers.List(ctx, 1000, 0)
	if err != nil {
		return nil, err
	}
	entries := make([]wireguard.PeerEntry, len(allPeers))
	for i, p := range allPeers {
		entries[i] = wireguard.PeerEntry{
			Name:       p.Name,
			PublicKey:  p.PublicKey,
			AllowedIPs: p.AllowedIPs,
		}
	}
	cfg := wireguard.RenderServer(wireguard.ServerConfig{
		PrivateKey: priv,
		Address:    req.Msg.GetAddress(),
		ListenPort: int(req.Msg.GetListenPort()),
		Peers:      entries,
	})
	return connect.NewResponse(&fletcherv1.ServerConfigResponse{
		Config:    cfg,
		PublicKey: string(pub),
	}), nil
}

func (s *PeersService) resolveServerPublicKey(ctx context.Context, override wireguard.Key) (wireguard.Key, error) {
	if override != "" {
		return override, nil
	}
	if s.serverKey == nil {
		return "", errors.New("server key provider not configured")
	}
	return s.serverKey.ServerPublicKey(ctx)
}

func peerToProto(p peer.Peer) *fletcherv1.Peer {
	return &fletcherv1.Peer{
		Id:         p.ID,
		Name:       p.Name,
		PublicKey:  string(p.PublicKey),
		AllowedIps: append([]string(nil), p.AllowedIPs...),
		CreatedAt:  p.CreatedAt.Unix(),
		UpdatedAt:  p.UpdatedAt.Unix(),
	}
}
