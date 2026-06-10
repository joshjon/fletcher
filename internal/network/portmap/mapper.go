package portmap

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// refreshInterval is how often the Mapper re-requests its mappings. It is
// well under defaultMapLifetime so a router that grants a shorter lease
// than requested (NAT-PMP/UPnP leases are often minutes, not hours) never
// lets a mapping lapse between refreshes. This is the bug that previously
// dropped the WireGuard forward: the old code mapped once and never renewed.
const refreshInterval = 10 * time.Minute

// Mapper keeps a set of port mappings alive. Callers register desired
// mappings with Ensure; Run refreshes them on a timer until its context is
// cancelled, then releases them so Fletcher does not leave stale forwards
// behind on the router. Safe for concurrent use.
type Mapper struct {
	logger *slog.Logger

	// mapFn/unmapFn default to the package Map/Unmap; tests override them.
	mapFn   func(context.Context, Request) (Result, error)
	unmapFn func(context.Context, Request) error

	mu      sync.Mutex
	desired map[string]Request
	method  string // last protocol that worked, for status reporting
}

// NewMapper builds an empty Mapper.
func NewMapper(logger *slog.Logger) *Mapper {
	return &Mapper{
		logger:  logger,
		mapFn:   Map,
		unmapFn: Unmap,
		desired: make(map[string]Request),
	}
}

func mappingKey(r Request) string {
	return string(r.Protocol) + ":" + strconv.Itoa(int(r.InternalPort))
}

// Ensure installs a mapping now and remembers it for periodic refresh and
// for release on shutdown. The initial result/error is returned (callers
// use the result's external IP to derive the public endpoint), but the
// mapping is remembered even on failure so a later refresh can recover
// (e.g. after the router reboots or UPnP/NAT-PMP becomes available).
func (m *Mapper) Ensure(ctx context.Context, r Request) (Result, error) {
	m.mu.Lock()
	m.desired[mappingKey(r)] = r
	m.mu.Unlock()

	res, err := m.mapFn(ctx, r)
	if err == nil {
		m.mu.Lock()
		m.method = res.Method
		m.mu.Unlock()
		m.logger.Info("router port-forward installed",
			slog.String("method", res.Method),
			slog.String("proto", string(r.Protocol)),
			slog.Int("port", int(r.InternalPort)),
			slog.Duration("lease", res.LeaseDuration),
			slog.String("desc", r.Description))
	} else {
		m.logger.Warn("router port-forward unavailable; the box may be unreachable from outside the LAN until forwarded manually",
			slog.String("proto", string(r.Protocol)),
			slog.Int("port", int(r.InternalPort)),
			slog.String("err", err.Error()))
	}
	return res, err
}

// Method reports the protocol of the last successful mapping ("nat-pmp" or
// "upnp"), or "" if none has succeeded. Used by doctor for honest status.
func (m *Mapper) Method() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.method
}

func (m *Mapper) snapshot() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, 0, len(m.desired))
	for _, r := range m.desired {
		out = append(out, r)
	}
	return out
}

// Run refreshes every Ensured mapping on refreshInterval until ctx is
// cancelled, then releases them all. Intended to run as a long-lived actor.
func (m *Mapper) Run(ctx context.Context) error {
	t := time.NewTicker(refreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.releaseAll() //nolint:contextcheck // ctx is already cancelled at shutdown; releaseAll uses a fresh short-lived context on purpose
			return nil
		case <-t.C:
			m.refresh(ctx)
		}
	}
}

func (m *Mapper) refresh(ctx context.Context) {
	for _, r := range m.snapshot() {
		res, err := m.mapFn(ctx, r)
		if err != nil {
			// Debug, not Warn: the initial Ensure already warned loudly if
			// mapping is unavailable; a 10-minute Warn cadence would just be
			// noise for a box that legitimately has no UPnP/NAT-PMP.
			m.logger.Debug("router port-forward refresh failed",
				slog.String("proto", string(r.Protocol)),
				slog.Int("port", int(r.InternalPort)),
				slog.String("err", err.Error()))
			continue
		}
		m.mu.Lock()
		m.method = res.Method
		m.mu.Unlock()
	}
}

// releaseAll deletes every mapping the Mapper installed. Best-effort and
// time-boxed: the parent context is already cancelled at shutdown, so it
// uses a fresh short-lived context.
func (m *Mapper) releaseAll() {
	reqs := m.snapshot()
	if len(reqs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, r := range reqs {
		if err := m.unmapFn(ctx, r); err != nil {
			m.logger.Debug("router port-forward release failed (it will expire on its own)",
				slog.String("proto", string(r.Protocol)),
				slog.Int("port", int(r.InternalPort)),
				slog.String("err", err.Error()))
			continue
		}
		m.logger.Info("router port-forward released",
			slog.String("proto", string(r.Protocol)),
			slog.Int("port", int(r.InternalPort)))
	}
}
