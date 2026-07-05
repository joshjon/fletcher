package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsPermissionDenied(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "wrapped fs.ErrPermission",
			err:  fmt.Errorf("dial unix: %w", fs.ErrPermission),
			want: true,
		},
		{
			name: "literal permission denied string",
			err:  errors.New("dial unix /run/fletcher/fletcher.sock: connect: permission denied"),
			want: true,
		},
		{
			name: "mixed case string still matches",
			err:  errors.New("connect: Permission Denied"),
			want: true,
		},
		{
			name: "connection refused is not permission",
			err:  errors.New("dial unix /run/fletcher/fletcher.sock: connect: connection refused"),
			want: false,
		},
		{
			name: "missing socket is not permission",
			err:  fmt.Errorf("dial unix: %w", fs.ErrNotExist),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isPermissionDenied(tt.err))
		})
	}
}

func TestDiagnoseDaemonError(t *testing.T) {
	// A socket path whose parent does not exist either, so socketGroup cannot
	// stat anything and the permission plan falls back to the default group.
	missingSocket := filepath.Join(string(filepath.Separator), "nonexistent-fletcher-test", "fletcher.sock")

	t.Run("unreachable daemon gets the start-daemon plan", func(t *testing.T) {
		err := errors.New("dial unix: connect: connection refused")
		res := diagnoseDaemonError(missingSocket, err)

		require.Equal(t, CategoryDaemon, res.Category)
		require.Equal(t, StatusFail, res.Status)
		require.Contains(t, res.Detail, missingSocket)
		require.Contains(t, res.Detail, "connection refused")
		require.NotNil(t, res.Plan)
		require.Equal(t, "start-daemon", res.Plan.ID)
		require.Equal(t, PriorityBlocker, res.Plan.Priority)
	})

	t.Run("permission denied routes to the socket-permission plan", func(t *testing.T) {
		err := fmt.Errorf("dial unix: %w", fs.ErrPermission)
		res := diagnoseDaemonError(missingSocket, err)

		require.Equal(t, StatusFail, res.Status)
		require.NotNil(t, res.Plan)
		require.Equal(t, "socket-permission", res.Plan.ID)
	})
}

func TestSocketPermissionResult(t *testing.T) {
	t.Run("unknown socket falls back to the fletcher group", func(t *testing.T) {
		socketPath := filepath.Join(string(filepath.Separator), "nonexistent-fletcher-test", "fletcher.sock")
		res := socketPermissionResult(socketPath)

		require.Equal(t, StatusFail, res.Status)
		require.NotNil(t, res.Plan)
		require.Equal(t, "socket-permission", res.Plan.ID)
		require.Equal(t, PriorityBlocker, res.Plan.Priority)
		require.Contains(t, res.Plan.Why, `"fletcher"`)
		require.Contains(t, res.Plan.Why, "not a member")
		require.Len(t, res.Plan.Options, 1)
		require.Contains(t, res.Plan.Options[0].Steps, "sudo usermod -aG fletcher $USER")
	})

	t.Run("statable socket tailors the plan to membership state", func(t *testing.T) {
		// A real file lets socketGroup resolve the owning group. Which branch we
		// land in depends on the environment (the file's group is normally the
		// user's primary group, so membership usually holds); assert the branch
		// that matches what the helpers report so the test is deterministic.
		socketPath := filepath.Join(t.TempDir(), "fletcher.sock")
		require.NoError(t, os.WriteFile(socketPath, nil, 0o600))

		group, gid, known := socketGroup(socketPath)
		require.True(t, known, "a plain file must be statable")

		res := socketPermissionResult(socketPath)
		require.NotNil(t, res.Plan)
		require.Len(t, res.Plan.Options, 1)
		steps := res.Plan.Options[0].Steps

		if userInGroup(gid) {
			require.Contains(t, res.Plan.Why, "already belongs")
			require.Contains(t, steps, "newgrp "+group)
		} else {
			require.Contains(t, res.Plan.Why, "not a member")
			require.Contains(t, steps, "sudo usermod -aG "+group+" $USER")
		}
	})
}
