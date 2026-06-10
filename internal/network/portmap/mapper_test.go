package portmap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func testMapper(t *testing.T) *Mapper {
	t.Helper()
	return NewMapper(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestMapperEnsureRemembersAndRecordsMethod(t *testing.T) {
	m := testMapper(t)
	var calls []Request
	var mu sync.Mutex
	m.mapFn = func(_ context.Context, r Request) (Result, error) {
		mu.Lock()
		calls = append(calls, r)
		mu.Unlock()
		return Result{Method: "nat-pmp", ExternalPort: r.InternalPort}, nil
	}

	res, err := m.Ensure(context.Background(), Request{Protocol: ProtocolTCP, InternalPort: 51821})
	require.NoError(t, err)
	require.Equal(t, "nat-pmp", res.Method)
	require.Equal(t, "nat-pmp", m.Method())
	require.Len(t, m.snapshot(), 1)
}

func TestMapperRemembersEvenOnFailure(t *testing.T) {
	m := testMapper(t)
	m.mapFn = func(context.Context, Request) (Result, error) {
		return Result{}, errors.New("no router")
	}
	_, err := m.Ensure(context.Background(), Request{Protocol: ProtocolUDP, InternalPort: 51820})
	require.Error(t, err)
	// Remembered so a later refresh can recover once the router is reachable.
	require.Len(t, m.snapshot(), 1)
	require.Empty(t, m.Method())
}

func TestMapperRefreshReMapsAll(t *testing.T) {
	m := testMapper(t)
	var count int
	var mu sync.Mutex
	m.mapFn = func(context.Context, Request) (Result, error) {
		mu.Lock()
		count++
		mu.Unlock()
		return Result{Method: "upnp"}, nil
	}
	_, _ = m.Ensure(context.Background(), Request{Protocol: ProtocolUDP, InternalPort: 51820})
	_, _ = m.Ensure(context.Background(), Request{Protocol: ProtocolTCP, InternalPort: 51821})
	mu.Lock()
	count = 0
	mu.Unlock()

	m.refresh(context.Background())
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, count, "refresh should re-map every remembered request")
}

func TestMapperReleaseAllUnmapsEach(t *testing.T) {
	m := testMapper(t)
	m.mapFn = func(context.Context, Request) (Result, error) { return Result{Method: "nat-pmp"}, nil }
	var released []Request
	var mu sync.Mutex
	m.unmapFn = func(_ context.Context, r Request) error {
		mu.Lock()
		released = append(released, r)
		mu.Unlock()
		return nil
	}
	_, _ = m.Ensure(context.Background(), Request{Protocol: ProtocolUDP, InternalPort: 51820})
	_, _ = m.Ensure(context.Background(), Request{Protocol: ProtocolTCP, InternalPort: 51821})

	m.releaseAll()
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, released, 2, "shutdown should release every installed mapping")
}

func TestMappingKeyDistinguishesProtocol(t *testing.T) {
	// Same port, different protocol must be distinct mappings (e.g. the
	// daemon maps UDP 51820 and could map TCP 51820 independently).
	require.NotEqual(t,
		mappingKey(Request{Protocol: ProtocolUDP, InternalPort: 51820}),
		mappingKey(Request{Protocol: ProtocolTCP, InternalPort: 51820}),
	)
}
