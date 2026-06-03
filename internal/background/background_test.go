package background_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/background"
)

// syncBuffer is a thread-safe in-memory log target. Tests write through
// the goroutine spawned by background.Go (potentially after the test body
// returns), so a raw bytes.Buffer would race.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) Contains(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.buf.String(), substr)
}

func swapDefaultLogger(t *testing.T, w *syncBuffer) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func TestGoExecutesAndCarriesContext(t *testing.T) {
	type ctxKey struct{}
	parent := context.WithValue(context.Background(), ctxKey{}, "value")

	var wg sync.WaitGroup
	wg.Add(1)
	var observed string
	background.Go(parent, func(ctx context.Context) {
		defer wg.Done()
		if v, ok := ctx.Value(ctxKey{}).(string); ok {
			observed = v
		}
	})
	wg.Wait()
	require.Equal(t, "value", observed)
}

func TestGoRecoversAndLogsPanic(t *testing.T) {
	var buf syncBuffer
	swapDefaultLogger(t, &buf)

	background.Go(context.Background(), func(_ context.Context) {
		panic("boom")
	})
	require.Eventually(t, func() bool {
		return buf.Contains("goroutine panic") && buf.Contains("boom")
	}, time.Second, 10*time.Millisecond)
}

func TestGoNamedUsesProvidedName(t *testing.T) {
	var buf syncBuffer
	swapDefaultLogger(t, &buf)

	background.GoNamed(context.Background(), "custom-worker", func(_ context.Context) {
		panic("x")
	})
	require.Eventually(t, func() bool {
		return buf.Contains("name=custom-worker")
	}, time.Second, 10*time.Millisecond)
}
