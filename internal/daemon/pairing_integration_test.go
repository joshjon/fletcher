package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/network/pairingtls"
	"github.com/joshjon/fletcher/internal/network/wireguard"
	"github.com/joshjon/fletcher/internal/peer"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

type fakeServerKey struct{ priv, pub wireguard.Key }

func (f fakeServerKey) ServerPrivateKey(context.Context) (wireguard.Key, error) { return f.priv, nil }
func (f fakeServerKey) ServerPublicKey(context.Context) (wireguard.Key, error)  { return f.pub, nil }

// TestPairingListenerCompletesPairOverPinnedTLS proves the bootstrap the
// whole feature exists for: a native client, before it is a WireGuard
// peer, reaches CompletePair over the public TLS listener (pinning the
// self-signed cert by fingerprint), and the daemon registers the peer
// with the client-generated public key and hands back a token. This is
// the leg that previously deadlocked (CompletePair was only reachable
// over the tunnel that could not yet exist).
func TestPairingListenerCompletesPairOverPinnedTLS(t *testing.T) {
	ctx := context.Background()

	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	peerSvc := peer.NewService(sqliteq.New(db), peer.Options{
		PublicEndpoint: "home.example.com:51820",
		APIEndpoint:    "10.99.0.1:11700",
	})

	serverKP, err := wireguard.GenerateKeypair()
	require.NoError(t, err)

	mat, err := pairingtls.EnsureCert(t.TempDir())
	require.NoError(t, err)

	srv := newPairingServer(connectDeps{
		peers:     peerSvc,
		serverKey: fakeServerKey{priv: serverKP.Private, pub: serverKP.Public},
	}, mat, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	// The client trusts no CA - it pins the exact leaf fingerprint from
	// the (out-of-band) pairing blob, exactly as the iOS app does.
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // we pin the leaf fingerprint in VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no peer certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != mat.Fingerprint {
				return errors.New("pairing cert fingerprint mismatch")
			}
			return nil
		},
	}}}
	client := fletcherv1connect.NewPeerServiceClient(httpClient, "https://"+ln.Addr().String())

	// BeginPair runs daemon-side (the operator's `peer pair --mobile`);
	// the client only ever calls CompletePair.
	begun, err := peerSvc.BeginPair(ctx, "phone")
	require.NoError(t, err)

	clientKP, err := wireguard.GenerateKeypair()
	require.NoError(t, err)

	resp, err := client.CompletePair(ctx, connect.NewRequest(&fletcherv1.CompletePairRequest{
		PairingCode:     begun.Code,
		Name:            "phone",
		ClientPublicKey: string(clientKP.Public),
	}))
	require.NoError(t, err)
	require.NotEmpty(t, resp.Msg.GetApiToken())
	require.Equal(t, "phone", resp.Msg.GetPeer().GetName())

	// The peer is now registered with the client's public key, so the
	// daemon can complete a WireGuard handshake when the app brings up
	// the tunnel next.
	peers, err := peerSvc.List(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, peers, 1)
	require.Equal(t, clientKP.Public, peers[0].PublicKey)
}
