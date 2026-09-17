package api

import "context"

type peerIDKey struct{}

// WithPeerID attaches the authenticated device identity to a request context.
func WithPeerID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, peerIDKey{}, id)
}

func currentPeerID(ctx context.Context) string {
	id, _ := ctx.Value(peerIDKey{}).(string)
	return id
}
