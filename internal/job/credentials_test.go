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

	t.Run("known names dedupe", func(t *testing.T) {
		got, err := normaliseCredentials([]string{"git", "git"})
		require.NoError(t, err)
		require.Equal(t, []string{"git"}, got)
	})

	t.Run("unknown name fails with InvalidArgument", func(t *testing.T) {
		_, err := normaliseCredentials([]string{"git", "nope"})
		require.Error(t, err)
		require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(err))
	})
}

func TestWriteGitCredential(t *testing.T) {
	read := func(t *testing.T, root, rel string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, ".config", "git", rel))
		require.NoError(t, err)
		return string(data)
	}

	t.Run("writes the store helper, host line, and identity", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, WriteGitCredential(root, "github.com", "me", "t0ken", "Me", "me@example.com"))

		require.Equal(t, "https://me:t0ken@github.com\n", read(t, root, "credentials"))
		cfg := read(t, root, "config")
		require.Contains(t, cfg, "helper = store")
		require.Contains(t, cfg, "name = Me")
		require.Contains(t, cfg, "email = me@example.com")
	})

	t.Run("a second host is appended, both kept", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, WriteGitCredential(root, "github.com", "me", "gh", "", ""))
		require.NoError(t, WriteGitCredential(root, "gitlab.com", "me", "gl", "", ""))

		creds := read(t, root, "credentials")
		require.Contains(t, creds, "https://me:gh@github.com")
		require.Contains(t, creds, "https://me:gl@gitlab.com")
	})

	t.Run("re-saving a host replaces its line, not duplicates it", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, WriteGitCredential(root, "github.com", "me", "old", "", ""))
		require.NoError(t, WriteGitCredential(root, "github.com", "me", "new", "", ""))

		require.Equal(t, "https://me:new@github.com\n", read(t, root, "credentials"))
	})

	t.Run("a blank identity on a later call keeps the saved one", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, WriteGitCredential(root, "github.com", "me", "gh", "Me", "me@example.com"))
		require.NoError(t, WriteGitCredential(root, "gitlab.com", "me", "gl", "", ""))

		cfg := read(t, root, "config")
		require.Contains(t, cfg, "name = Me")
		require.Contains(t, cfg, "email = me@example.com")
	})

	t.Run("missing fields and a non-bare host are rejected", func(t *testing.T) {
		root := t.TempDir()
		for _, tc := range []struct{ host, user, token string }{
			{"", "me", "t"},
			{"github.com", "", "t"},
			{"github.com", "me", ""},
			{"https://github.com", "me", "t"},
			{"github.com/me", "me", "t"},
		} {
			err := WriteGitCredential(root, tc.host, tc.user, tc.token, "", "")
			require.Error(t, err)
			require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(err))
		}
	})

	t.Run("seeds into a session via ResolveCredentialFiles", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, WriteGitCredential(root, "github.com", "me", "t0ken", "", ""))

		files, err := ResolveCredentialFiles(root, []string{CredentialGit})
		require.NoError(t, err)

		paths := make(map[string]string, len(files))
		for _, f := range files {
			paths[f.Path] = string(f.Data)
		}
		require.Equal(t, "https://me:t0ken@github.com\n", paths[homeRel+"/.config/git/credentials"])
		require.Contains(t, paths[homeRel+"/.config/git/config"], "helper = store")
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
		encoded, err := encodeCredentials([]string{CredentialGit})
		require.NoError(t, err)

		_, err = sup.resolveCredentials(encoded)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no credentials root configured")
	})

	t.Run("missing host directory fails with the path it tried", func(t *testing.T) {
		sup := &Supervisor{credentialsRoot: t.TempDir()}
		encoded, err := encodeCredentials([]string{CredentialGit})
		require.NoError(t, err)

		_, err = sup.resolveCredentials(encoded)
		require.Error(t, err)
		require.Contains(t, err.Error(), AllowedCredentials[CredentialGit].HostRelPath)
	})

	t.Run("present host directories resolve to mount entries", func(t *testing.T) {
		root := t.TempDir()
		for _, c := range AllowedCredentials {
			require.NoError(t, os.MkdirAll(filepath.Join(root, c.HostRelPath), 0o700))
		}
		sup := &Supervisor{credentialsRoot: root}
		encoded, err := encodeCredentials([]string{CredentialGit})
		require.NoError(t, err)

		mounts, err := sup.resolveCredentials(encoded)
		require.NoError(t, err)
		require.Len(t, mounts, 1)
		require.Equal(t, filepath.Join(root, AllowedCredentials[CredentialGit].HostRelPath), mounts[0].Source)
		require.Equal(t, AllowedCredentials[CredentialGit].GuestPath, mounts[0].Destination)
		require.False(t, mounts[0].ReadOnly, "credentials must be rw so token refresh can write back")
	})
}
