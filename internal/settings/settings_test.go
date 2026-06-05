package settings

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// memQuerier is a tiny in-memory Querier covering just the settings queries.
type memQuerier struct {
	sqliteq.Querier
	m map[string]string
}

func (q *memQuerier) UpsertSetting(_ context.Context, p sqliteq.UpsertSettingParams) error {
	q.m[p.Key] = p.Value
	return nil
}

func (q *memQuerier) ListSettings(_ context.Context) ([]sqliteq.Setting, error) {
	out := make([]sqliteq.Setting, 0, len(q.m))
	for k, v := range q.m {
		out = append(out, sqliteq.Setting{Key: k, Value: v})
	}
	return out, nil
}

func (q *memQuerier) DeleteSetting(_ context.Context, key string) (int64, error) {
	if _, ok := q.m[key]; ok {
		delete(q.m, key)
		return 1, nil
	}
	return 0, nil
}

func newStore() *Store { return NewStore(&memQuerier{m: map[string]string{}}) }

func TestSetRejectsUnknownKey(t *testing.T) {
	require.Error(t, newStore().Set(context.Background(), "bogus", "x"))
}

func TestSetValidatesEnumAndPort(t *testing.T) {
	s := newStore()
	require.Error(t, s.Set(context.Background(), KeyRuntime, "qemu"))   // not in enum
	require.NoError(t, s.Set(context.Background(), KeyRuntime, "runc")) // valid
	require.Error(t, s.Set(context.Background(), KeyWireGuardPort, "0"))
	require.Error(t, s.Set(context.Background(), KeyWireGuardPort, "abc"))
	require.NoError(t, s.Set(context.Background(), KeyWireGuardPort, "51820"))
}

func TestDescribeCoversAllKeysAndReflectsSet(t *testing.T) {
	s := newStore()
	require.NoError(t, s.Set(context.Background(), KeyLogLevel, "debug"))
	views, err := s.Describe(context.Background())
	require.NoError(t, err)
	require.Len(t, views, len(registry))
	for _, v := range views {
		require.NotEmpty(t, v.Description)
		if v.Key == KeyLogLevel {
			require.True(t, v.Set)
			require.Equal(t, "debug", v.Value)
		}
	}
}
