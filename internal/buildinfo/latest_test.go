package buildinfo_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/buildinfo"
)

func TestUpgradeAvailable(t *testing.T) {
	cases := []struct {
		current string
		latest  string
		want    bool
	}{
		{"v0.1.0", "v0.1.1", true},
		{"0.1.0", "v0.1.1", true},   // accept un-prefixed current
		{"v0.1.0", "0.1.1", true},   // accept un-prefixed latest
		{"v0.2.0", "v0.1.0", false}, // running newer than published
		{"v0.1.0", "v0.1.0", false}, // up to date
		{"dev", "v0.1.0", false},    // dev build never shows notice
		{"", "v0.1.0", false},       // empty current never shows
		{"v0.1.0", "weird-tag", false},
	}
	for _, c := range cases {
		got := buildinfo.UpgradeAvailable(c.current, c.latest)
		require.Equal(t, c.want, got, "UpgradeAvailable(%q, %q)", c.current, c.latest)
	}
}
