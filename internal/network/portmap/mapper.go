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

// releaseTimeout bounds how long releasing a single mapping may take on
// shutdown. Release is best-effort - a mapping not released here expires on
// its own - so this stays short enough that shutdown is never blocked, even
// when the router has become unreachable.
const releaseTimeout = 2 * time.Second

// entry is one desired mapping plus whether it is currently installed (so
// shutdown only releases forwards that actually exist - releasing a mapping
// that never installed would do pointless, slow network work).
type entry struct {
	req       Request
	installed bool
	method    string
}

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
	desired map[string]*entry
	method  string // last protocol that worked, for status reporting
}

// NewMapper builds an empty Mapper.
func NewMapper(logger *slog.Logger) *Mapper {
	return &Mapper{
		logger:  logger,
		mapFn:   Map,
		unmapFn: Unmap,
		desired: make(map[string]*entry),
	}
}

func mappingKey(r Request) string {
	return string(r.Protocol) + ":" + strconv.Itoa(int(r.InternalPort))
}

// Ensure installs a mapping now and remembers it for periodic refresh and
// for release on shutdown. The initial result/error is returned (callers
// use the result's external IP to derive the public endpoint); the mapping
// is remembered even on failure so a later refresh can recover (e.g. after
// the router reboots or UPnP/NAT-PMP becomes available), but it is only
// marked installed - and so only released on shutdown - once a map succeeds.
func (m *Mapper) Ensure(ctx context.Context, r Request) (Result, error) {
	res, err := m.mapFn(ctx, r)
	m.record(r, res, err)
	if err == nil {
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

// record updates the remembered state for r after a map attempt.
func (m *Mapper) record(r Request, res Result, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.desired[mappingKey(r)]
	if !ok {
		e = &entry{req: r}
		m.desired[mappingKey(r)] = e
	}
	if err != nil {
		e.installed = false
		return
	}
	e.installed = true
	e.method = res.Method
	m.method = res.Method
}

// Method reports the protocol of the last successful mapping ("nat-pmp" or
// "upnp"), or "" if none has succeeded. Used by doctor for honest status.
func (m *Mapper) Method() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.method
}

// requests returns the requests to refresh (all desired).
func (m *Mapper) requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, 0, len(m.desired))
	for _, e := range m.desired {
		out = append(out, e.req)
	}
	return out
}

// installedRequests returns only the requests currently installed - the set
// to release on shutdown.
func (m *Mapper) installedRequests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, 0, len(m.desired))
	for _, e := range m.desired {
		if e.installed {
			out = append(out, e.req)
		}
	}
	return out
}

// Run refreshes every Ensured mapping on refreshInterval until ctx is
// cancelled, then releases the installed ones. Intended to run as a
// long-lived actor.
func (m *Mapper) Run(ctx context.Context) error {
	t := time.NewTicker(refreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.releaseAll() //nolint:contextcheck // ctx is already cancelled at shutdown; releaseAll uses fresh short-lived contexts on purpose
			return nil
		case <-t.C:
			m.refresh(ctx)
		}
	}
}

func (m *Mapper) refresh(ctx context.Context) {
	for _, r := range m.requests() {
		res, err := m.mapFn(ctx, r)
		m.record(r, res, err)
		if err != nil {
			// Debug, not Warn: the initial Ensure already warned loudly if
			// mapping is unavailable; a 10-minute Warn cadence would just be
			// noise for a box that legitimately has no UPnP/NAT-PMP.
			m.logger.Debug("router port-forward refresh failed",
				slog.String("proto", string(r.Protocol)),
				slog.Int("port", int(r.InternalPort)),
				slog.String("err", err.Error()))
		}
	}
}

// releaseAll deletes every installed mapping, each under its own short
// timeout and concurrently, so shutdown is bounded by releaseTimeout
// regardless of how many mappings exist or whether the router still
// responds. Best-effort: anything not released expires on its own.
func (m *Mapper) releaseAll() {
	reqs := m.installedRequests()
	if len(reqs) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, r := range reqs {
		wg.Add(1)
		go func(r Request) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
			defer cancel()
			if err := m.unmapFn(ctx, r); err != nil {
				m.logger.Debug("router port-forward release failed (it will expire on its own)",
					slog.String("proto", string(r.Protocol)),
					slog.Int("port", int(r.InternalPort)),
					slog.String("err", err.Error()))
				return
			}
			m.logger.Info("router port-forward released",
				slog.String("proto", string(r.Protocol)),
				slog.Int("port", int(r.InternalPort)))
		}(r)
	}
	wg.Wait()
}
