package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/joshjon/fletcher/internal/api"
	"github.com/joshjon/fletcher/internal/network/wireguard"
	"github.com/joshjon/fletcher/internal/secrets"
)

// wgServerSecretName is the well-known key under which the daemon stores
// its WireGuard server private key inside the secrets store.
const wgServerSecretName = "wireguard_server_private_key"

// serverKeyProvider is the daemon's implementation of
// api.ServerKeyProvider. It lazily generates a server keypair on first
// access and persists the private half in the secrets store.
type serverKeyProvider struct {
	store *secrets.Store
}

// Ensure the type satisfies the API contract.
var _ api.ServerKeyProvider = (*serverKeyProvider)(nil)

func newServerKeyProvider(store *secrets.Store) *serverKeyProvider {
	return &serverKeyProvider{store: store}
}

// ServerPrivateKey returns the daemon's WireGuard private key, generating
// and persisting one on first call.
func (p *serverKeyProvider) ServerPrivateKey(ctx context.Context) (wireguard.Key, error) {
	priv, err := p.store.Get(ctx, wgServerSecretName)
	if err == nil {
		return wireguard.Key(priv), nil
	}
	if !errors.Is(err, secrets.ErrNotFound) {
		return "", fmt.Errorf("load wireguard server key: %w", err)
	}
	kp, err := wireguard.GenerateKeypair()
	if err != nil {
		return "", err
	}
	if err := p.store.Set(ctx, wgServerSecretName, string(kp.Private)); err != nil {
		return "", fmt.Errorf("persist wireguard server key: %w", err)
	}
	return kp.Private, nil
}

// ServerPublicKey returns the public half of the daemon's WireGuard key.
func (p *serverKeyProvider) ServerPublicKey(ctx context.Context) (wireguard.Key, error) {
	priv, err := p.ServerPrivateKey(ctx)
	if err != nil {
		return "", err
	}
	return wireguard.PublicFromPrivate(priv)
}
