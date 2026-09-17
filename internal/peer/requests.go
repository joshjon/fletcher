package peer

import "context"

type requestLease struct{ cancel context.CancelFunc }

// AuthenticateRequest binds a request's lifetime to its peer, including streams
// over an operator-provided VPN; deleting the peer cancels active requests.
func (s *Service) AuthenticateRequest(ctx context.Context, token string) (Peer, context.Context, func(), error) {
	s.requestsMu.Lock()
	defer s.requestsMu.Unlock()
	p, err := s.AuthenticateToken(ctx, token)
	if err != nil {
		return Peer{}, ctx, func() {}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	lease := &requestLease{cancel: cancel}
	if s.requests == nil {
		s.requests = make(map[string]map[*requestLease]struct{})
	}
	if s.requests[p.ID] == nil {
		s.requests[p.ID] = make(map[*requestLease]struct{})
	}
	s.requests[p.ID][lease] = struct{}{}
	release := func() {
		cancel()
		s.requestsMu.Lock()
		defer s.requestsMu.Unlock()
		delete(s.requests[p.ID], lease)
		if len(s.requests[p.ID]) == 0 {
			delete(s.requests, p.ID)
		}
	}
	return p, ctx, release, nil
}
