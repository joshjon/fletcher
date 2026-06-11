package session

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
)

// PublicBackend is what the public HTTPS server needs from the session manager:
// resolve a hostname to the published port serving it, and dial that port inside
// the VM. Satisfied by *Manager.
type PublicBackend interface {
	// LookupPublicPort resolves a public hostname to its published port, or
	// ErrNotFound if no public port claims it.
	LookupPublicPort(ctx context.Context, host string) (PublishedPort, error)
	// DialPort opens a stream to a guest port (waking a stopped session).
	DialPort(ctx context.Context, ref string, port uint16) (net.Conn, error)
}

// PublicConfig configures the public HTTPS server.
type PublicConfig struct {
	Backend PublicBackend
	Logger  *slog.Logger
	// CertDir is where certmagic stores ACME accounts and issued certificates.
	CertDir string
	// Email is the optional ACME account contact.
	Email string
	// Staging uses Let's Encrypt's staging CA (untrusted certs, no rate limits).
	Staging bool
}

// PublicServer serves published public ports on the internet over HTTPS. It
// terminates TLS (certmagic issues a cert per published hostname, on demand) and
// reverse-proxies plaintext to the guest port over vsock via the backend, so the
// VM stays unroutable. It serves ONLY hostnames that map to a published public
// port; anything else gets a 404 and is refused a certificate. The Phase 2 half
// of Milestone 8 (DESIGN.md §5: the preview-proxy pattern, widened to the public
// internet under a domain the operator controls).
type PublicServer struct {
	backend PublicBackend
	logger  *slog.Logger
	magic   *certmagic.Config
	acme    *certmagic.ACMEIssuer
	proxy   *httputil.ReverseProxy
}

// portCtxKey carries the resolved published port from the handler to the
// proxy transport's dialer.
type portCtxKey struct{}

// NewPublicServer wires certmagic (on-demand TLS, issuing only for published
// public hostnames) and the reverse proxy.
func NewPublicServer(cfg PublicConfig) *PublicServer {
	s := &PublicServer{backend: cfg.Backend, logger: cfg.Logger}

	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return s.magic, nil
		},
	})
	magic := certmagic.New(cache, certmagic.Config{
		Storage: &certmagic.FileStorage{Path: cfg.CertDir},
		OnDemand: &certmagic.OnDemandConfig{
			// Only issue a cert for a hostname that maps to a published public
			// port. This is what makes it safe to face the internet: a stranger
			// pointing a domain at the box cannot make us mint certs for it.
			DecisionFunc: func(ctx context.Context, name string) error {
				if _, err := cfg.Backend.LookupPublicPort(ctx, name); err != nil {
					return fmt.Errorf("host %q is not a published public port", name)
				}
				return nil
			},
		},
	})
	ca := certmagic.LetsEncryptProductionCA
	if cfg.Staging {
		ca = certmagic.LetsEncryptStagingCA
	}
	acme := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA:     ca,
		Email:  cfg.Email,
		Agreed: true,
	})
	magic.Issuers = []certmagic.Issuer{acme}
	s.magic = magic
	s.acme = acme

	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: &http.Transport{
			// Ignore addr and dial the guest port the handler resolved; the VM
			// has no NIC, so this vsock relay is the only path in.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				pp, ok := ctx.Value(portCtxKey{}).(PublishedPort)
				if !ok {
					return nil, errors.New("no published port in request context")
				}
				return s.backend.DialPort(ctx, pp.SessionID, uint16(pp.GuestPort)) //nolint:gosec // guest port validated 1..65535
			},
			IdleConnTimeout: 90 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.logger.Warn("public proxy upstream",
				slog.String("host", hostOnly(r.Host)), slog.String("err", err.Error()))
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
	return s
}

// HTTPSHandler routes an inbound public request to the guest port serving its
// hostname, or 404s an unrecognised host. The daemon serves it on :443 with
// TLSConfig().
func (s *PublicServer) HTTPSHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostOnly(r.Host)
		pp, err := s.backend.LookupPublicPort(r.Context(), host)
		if err != nil {
			http.Error(w, "no published service for this host", http.StatusNotFound)
			return
		}
		s.logger.Info("public request",
			slog.String("host", host),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("session_id", pp.SessionID),
			slog.Int("guest_port", pp.GuestPort))
		ctx := context.WithValue(r.Context(), portCtxKey{}, pp)
		s.proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

// HTTPHandler is served on :80: it answers ACME HTTP-01 challenges and redirects
// everything else to HTTPS.
func (s *PublicServer) HTTPHandler() http.Handler {
	return s.acme.HTTPChallengeHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Same-origin HTTP->HTTPS redirect: host and path are echoed back to the
		// same host the client already asked for, so this is not an open redirect.
		target := "https://" + hostOnly(r.Host) + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently) //nolint:gosec // same-host scheme upgrade, not attacker-controlled destination
	}))
}

// TLSConfig is the server TLS config: certmagic supplies certificates on demand
// and answers the TLS-ALPN-01 challenge; h2 and http/1.1 are offered for normal
// traffic.
func (s *PublicServer) TLSConfig() *tls.Config {
	tc := s.magic.TLSConfig()
	tc.NextProtos = append([]string{"h2", "http/1.1"}, tc.NextProtos...)
	return tc
}

// Public certificate states reported by CertStatus (the wire values clients
// read for the per-port TLS chip).
const (
	// CertPending: no managed cert yet; certmagic issues on the first HTTPS
	// request to the host.
	CertPending = "pending"
	// CertValid: a current cert is cached/stored and not yet due for renewal.
	CertValid = "valid"
	// CertRenewing: a cert exists but is within its renewal window.
	CertRenewing = "renewing"
	// CertExpired: a cert exists but has passed its NotAfter.
	CertExpired = "expired"
)

// CertStatus reports the public TLS certificate state for a published public
// hostname, plus the cert's expiry (NotAfter, Unix seconds; 0 when there is no
// cert). It reads certmagic's managed-cert storage without issuing anything, so
// a host with no cert yet reads as CertPending (issuance happens on the first
// real HTTPS request).
func (s *PublicServer) CertStatus(ctx context.Context, host string) (status string, expiresAt int64) {
	cert, err := s.magic.CacheManagedCertificate(ctx, host)
	if err != nil || cert.Empty() {
		return CertPending, 0
	}
	if cert.Leaf != nil {
		expiresAt = cert.Leaf.NotAfter.Unix()
	}
	switch {
	case cert.Expired():
		return CertExpired, expiresAt
	case cert.NeedsRenewal(s.magic):
		return CertRenewing, expiresAt
	default:
		return CertValid, expiresAt
	}
}

// hostOnly strips any port from a request Host header.
func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.TrimSpace(h)
}
