package job

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
)

func TestNormaliseCredentials(t *testing.T) {
	t.Run("nil and empty pass through as nil", func(t *testing.T) {
		got, err := normaliseCredentials(nil)
		require.NoError(t, err)
		require.Nil(t, got)

		got, err = normaliseCredentials([]string{})
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("known names sort + dedupe", func(t *testing.T) {
		got, err := normaliseCredentials([]string{"codex", "claude", "codex"})
		require.NoError(t, err)
		require.Equal(t, []string{"claude", "codex"}, got)
	})

	t.Run("unknown name fails with InvalidArgument", func(t *testing.T) {
		_, err := normaliseCredentials([]string{"claude", "nope"})
		require.Error(t, err)
		require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(err))
	})
}

func TestCredentialEncodeDecodeRoundTrip(t *testing.T) {
	cases := [][]string{
		nil,
		{"claude"},
		{"claude", "codex", "gemini"},
	}
	for _, in := range cases {
		encoded, err := encodeCredentials(in)
		require.NoError(t, err)

		got, err := decodeCredentials(encoded)
		require.NoError(t, err)
		if len(in) == 0 {
			require.Nil(t, got)
		} else {
			require.Equal(t, in, got)
		}
	}
}

func TestCredentialEncodeEmptyIsEmptyString(t *testing.T) {
	// The DB column has DEFAULT '' so the "no credentials" sentinel must be
	// the empty string, not a JSON "null" or "[]" literal.
	encoded, err := encodeCredentials(nil)
	require.NoError(t, err)
	require.Equal(t, "", encoded)
}

func TestCredentialDecodeRejectsGarbage(t *testing.T) {
	_, err := decodeCredentials("not-json")
	require.Error(t, err)
}

func TestSupervisorResolveCredentials(t *testing.T) {
	t.Run("empty encoded returns no mounts", func(t *testing.T) {
		sup := &Supervisor{credentialsRoot: "/anywhere"}
		got, err := sup.resolveCredentials("")
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("credentials requested with no root configured fails clearly", func(t *testing.T) {
		sup := &Supervisor{credentialsRoot: ""}
		encoded, err := encodeCredentials([]string{"claude"})
		require.NoError(t, err)

		_, err = sup.resolveCredentials(encoded)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no credentials root configured")
	})

	t.Run("missing host directory fails with the path it tried", func(t *testing.T) {
		sup := &Supervisor{credentialsRoot: t.TempDir()}
		encoded, err := encodeCredentials([]string{"claude"})
		require.NoError(t, err)

		_, err = sup.resolveCredentials(encoded)
		require.Error(t, err)
		require.Contains(t, err.Error(), AllowedCredentials[CredentialClaude].HostRelPath)
	})

	t.Run("present host directories resolve to mount entries", func(t *testing.T) {
		root := t.TempDir()
		for _, c := range AllowedCredentials {
			require.NoError(t, os.MkdirAll(filepath.Join(root, c.HostRelPath), 0o700))
		}
		sup := &Supervisor{credentialsRoot: root}
		encoded, err := encodeCredentials([]string{"claude", "codex"})
		require.NoError(t, err)

		mounts, err := sup.resolveCredentials(encoded)
		require.NoError(t, err)
		require.Len(t, mounts, 2)
		require.Equal(t, filepath.Join(root, AllowedCredentials[CredentialClaude].HostRelPath), mounts[0].Source)
		require.Equal(t, AllowedCredentials[CredentialClaude].GuestPath, mounts[0].Destination)
		require.False(t, mounts[0].ReadOnly, "credentials must be rw so token refresh can write back")
	})
}
