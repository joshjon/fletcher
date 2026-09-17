package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClearBuildCacheRefusesActiveBuildAndPreservesOtherFiles(t *testing.T) {
	root := t.TempDir()
	m := NewManager(nil, nil, nil, AgentEnv{}, nil, Options{ImagesDir: filepath.Join(root, "images")})
	require.NoError(t, os.WriteFile(filepath.Join(root, "buildcache.ext4"), []byte("cache"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "snap-live.ext4"), []byte("session"), 0o600))
	m.buildCacheSem <- struct{}{}
	require.ErrorContains(t, m.ClearBuildCache(t.Context()), "using the cache")
	require.FileExists(t, filepath.Join(root, "buildcache.ext4"))
	<-m.buildCacheSem
	require.NoError(t, m.ClearBuildCache(t.Context()))
	require.NoFileExists(t, filepath.Join(root, "buildcache.ext4"))
	require.FileExists(t, filepath.Join(root, "snap-live.ext4"))
	require.NoError(t, m.ClearBuildCache(t.Context()))
}
