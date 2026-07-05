package appspec_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/appspec"
)

func TestArgv(t *testing.T) {
	tests := []struct {
		name       string
		entrypoint []string
		cmd        []string
		want       []string
	}{
		{
			name: "nil entrypoint and cmd produce an empty argv",
			want: []string{},
		},
		{
			name:       "empty entrypoint and cmd produce an empty argv",
			entrypoint: []string{},
			cmd:        []string{},
			want:       []string{},
		},
		{
			name:       "entrypoint only",
			entrypoint: []string{"/bin/server", "--verbose"},
			want:       []string{"/bin/server", "--verbose"},
		},
		{
			name: "cmd only",
			cmd:  []string{"nginx", "-g", "daemon off;"},
			want: []string{"nginx", "-g", "daemon off;"},
		},
		{
			name:       "entrypoint followed by cmd, as docker layers them",
			entrypoint: []string{"/docker-entrypoint.sh"},
			cmd:        []string{"redis-server", "--port", "6380"},
			want:       []string{"/docker-entrypoint.sh", "redis-server", "--port", "6380"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := appspec.Spec{Entrypoint: tt.entrypoint, Cmd: tt.cmd}
			require.Equal(t, tt.want, s.Argv())
		})
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	t.Run("full spec survives the round trip", func(t *testing.T) {
		want := appspec.Spec{
			Entrypoint: []string{"/docker-entrypoint.sh"},
			Cmd:        []string{"redis-server"},
			Env:        []string{"PATH=/usr/local/bin:/usr/bin", "REDIS_VERSION=7.2"},
			WorkingDir: "/data",
			User:       "redis:redis",
		}
		// Write creates the parent dirs, so point dest at the spec's canonical
		// nested path inside a staging dir.
		dest := filepath.Join(t.TempDir(), appspec.Path)
		require.NoError(t, appspec.Write(want, dest))

		got, err := appspec.Read(dest)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})

	t.Run("zero spec survives the round trip", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "app.json")
		require.NoError(t, appspec.Write(appspec.Spec{}, dest))

		got, err := appspec.Read(dest)
		require.NoError(t, err)
		require.Equal(t, appspec.Spec{}, got)
	})
}

func TestReadMissingFile(t *testing.T) {
	_, err := appspec.Read(filepath.Join(t.TempDir(), "does-not-exist.json"))
	require.Error(t, err)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestReadMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))

	_, err := appspec.Read(path)
	require.Error(t, err)
	require.ErrorContains(t, err, "parse app spec")
}
