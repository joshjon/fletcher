package buildinfo_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/buildinfo"
)

func TestInfoReturnsLinkTimeValues(t *testing.T) {
	got := buildinfo.Info()
	require.Equal(t, buildinfo.Version, got.Version)
	require.Equal(t, buildinfo.Commit, got.Commit)
	require.Equal(t, buildinfo.Date, got.Date)
}
