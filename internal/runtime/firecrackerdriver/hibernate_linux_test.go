//go:build linux

package firecrackerdriver

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	fcruntime "github.com/joshjon/fletcher/internal/runtime"
)

func TestSnapshotIdentityBindsBootConfiguration(t *testing.T) {
	d := &Driver{memSizeMib: 512, vcpuCount: 1}
	base := fcruntime.SessionSpec{SessionID: "session_example", RootfsPath: "/fake/root.ext4", EgressPolicy: "allowlist", Env: []string{"EXAMPLE=old"}}
	for name, mutate := range map[string]func(*fcruntime.SessionSpec){
		"rootfs":          func(s *fcruntime.SessionSpec) { s.RootfsPath = "/fake/new.ext4" },
		"volume":          func(s *fcruntime.SessionSpec) { s.VolumePath = "/fake/data.ext4" },
		"policy":          func(s *fcruntime.SessionSpec) { s.EgressPolicy = "none" },
		"environment":     func(s *fcruntime.SessionSpec) { s.Env = []string{"EXAMPLE=new"} },
		"app environment": func(s *fcruntime.SessionSpec) { s.AppEnv = []string{"SECRET=sensitive-value"} },
		"app mode":        func(s *fcruntime.SessionSpec) { s.RunApp = true },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			require.NotEqual(t, d.snapshotIdentity(base), d.snapshotIdentity(changed))
			require.NotContains(t, d.snapshotIdentity(changed), "sensitive-value")
		})
	}
	seeded := base
	seeded.Credentials = []fcruntime.CredentialFile{{Path: "/fake/login", Data: []byte("seed")}}
	require.Equal(t, d.snapshotIdentity(base), d.snapshotIdentity(seeded))
	other := *d
	other.memSizeMib = 1024
	require.NotEqual(t, d.snapshotIdentity(base), other.snapshotIdentity(base))
	other = *d
	other.vcpuCount = 2
	require.NotEqual(t, d.snapshotIdentity(base), other.snapshotIdentity(base))
}

func TestSnapshotValidationRejectsOldConfiguration(t *testing.T) {
	d := &Driver{}
	base := fcruntime.SessionSpec{RootfsPath: "/fake/root.ext4", Env: []string{"GATEWAY=on"}}
	vmDir := t.TempDir()
	_, state, _ := snapshotPaths(vmDir)
	require.NoError(t, os.WriteFile(state, []byte("state"), 0o600))
	require.NoError(t, writeSnapshotMeta(vmDir, d.snapshotIdentity(base)))
	require.True(t, d.hasValidSnapshot(vmDir, base))
	base.Env = nil
	require.False(t, d.hasValidSnapshot(vmDir, base))
}
