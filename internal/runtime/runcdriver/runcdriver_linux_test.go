//go:build linux

package runcdriver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/runtime"
)

func TestJobArgsAndMounts(t *testing.T) {
	spec := runtime.Spec{
		Command: "echo hi",
		Mounts: []runtime.Mount{
			{Source: "/host/creds", Destination: "/fork/creds", ReadOnly: true},
		},
	}

	t.Run("no forwarder binary runs the command directly", func(t *testing.T) {
		d := &Driver{forwards: []Forward{{Listen: "127.0.0.1:11500", HostSocket: "/run/gw.sock"}}}
		args, mounts := d.jobArgsAndMounts(spec)
		require.Equal(t, []string{"/bin/sh", "-c", "echo hi"}, args)
		require.Equal(t, spec.Mounts, mounts)
	})

	t.Run("no forwards runs the command directly", func(t *testing.T) {
		d := &Driver{forwarderBin: "/usr/local/bin/fletcher"}
		args, mounts := d.jobArgsAndMounts(spec)
		require.Equal(t, []string{"/bin/sh", "-c", "echo hi"}, args)
		require.Equal(t, spec.Mounts, mounts)
	})

	t.Run("forwards wrap the command and add bind mounts", func(t *testing.T) {
		d := &Driver{
			forwarderBin: "/usr/local/bin/fletcher",
			forwards: []Forward{
				{Listen: "127.0.0.1:11500", HostSocket: "/run/gateway.sock"},
				{Listen: "127.0.0.1:11501", HostSocket: "/run/mcp.sock"},
			},
		}
		args, mounts := d.jobArgsAndMounts(spec)

		require.Equal(t, []string{
			forkBinPath, "fork-run",
			"--forward", "127.0.0.1:11500=" + fwdSocketPath(0),
			"--forward", "127.0.0.1:11501=" + fwdSocketPath(1),
			"--",
			"/bin/sh", "-c", "echo hi",
		}, args)

		require.Equal(t, []runtime.Mount{
			{Source: "/host/creds", Destination: "/fork/creds", ReadOnly: true},
			{Source: "/usr/local/bin/fletcher", Destination: forkBinPath, ReadOnly: true},
			{Source: "/run/gateway.sock", Destination: fwdSocketPath(0), ReadOnly: false},
			{Source: "/run/mcp.sock", Destination: fwdSocketPath(1), ReadOnly: false},
		}, mounts)
	})

	t.Run("the caller's mounts slice is not mutated", func(t *testing.T) {
		// Give the caller's slice spare capacity so an in-place append would
		// write into its backing array if jobArgsAndMounts forgot to clone.
		callerMounts := make([]runtime.Mount, 1, 4)
		callerMounts[0] = runtime.Mount{Source: "/a", Destination: "/b"}
		d := &Driver{
			forwarderBin: "/usr/local/bin/fletcher",
			forwards:     []Forward{{Listen: "127.0.0.1:11500", HostSocket: "/run/gw.sock"}},
		}
		_, _ = d.jobArgsAndMounts(runtime.Spec{Command: "true", Mounts: callerMounts})
		require.Equal(t, runtime.Mount{Source: "/a", Destination: "/b"}, callerMounts[0])
		require.Equal(t, runtime.Mount{}, callerMounts[:2][1], "spare capacity must stay untouched")
	})
}

func TestOCIConfig(t *testing.T) {
	spec := runtime.Spec{
		WorkDir: "/snapshots/job-1/rootfs",
		Env:     []string{"FOO=bar"},
	}
	args := []string{"/bin/sh", "-c", "true"}
	cfg := ociConfig(spec, args, nil)

	t.Run("all capability sets are empty", func(t *testing.T) {
		process, ok := cfg["process"].(map[string]any)
		require.True(t, ok)
		caps, ok := process["capabilities"].(map[string]any)
		require.True(t, ok)
		for _, set := range []string{"bounding", "effective", "permitted", "ambient"} {
			require.Empty(t, caps[set], "capability set %q must be empty", set)
		}
	})

	t.Run("all six namespaces are present", func(t *testing.T) {
		lnx, ok := cfg["linux"].(map[string]any)
		require.True(t, ok)
		namespaces, ok := lnx["namespaces"].([]map[string]any)
		require.True(t, ok)
		got := make([]string, 0, len(namespaces))
		for _, ns := range namespaces {
			typ, isString := ns["type"].(string)
			require.True(t, isString)
			got = append(got, typ)
		}
		require.ElementsMatch(t, []string{"pid", "ipc", "uts", "mount", "user", "network"}, got)
	})

	t.Run("uid and gid map a single id to the daemon's own", func(t *testing.T) {
		lnx, ok := cfg["linux"].(map[string]any)
		require.True(t, ok)
		uids, ok := lnx["uidMappings"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, uids, 1)
		require.Equal(t, map[string]any{"containerID": 0, "hostID": os.Geteuid(), "size": 1}, uids[0])

		gids, ok := lnx["gidMappings"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, gids, 1)
		require.Equal(t, map[string]any{"containerID": 0, "hostID": os.Getegid(), "size": 1}, gids[0])
	})

	t.Run("process runs args in /workspace with layered env and no new privileges", func(t *testing.T) {
		process, ok := cfg["process"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, args, process["args"])
		require.Equal(t, "/workspace", process["cwd"])
		require.Equal(t, true, process["noNewPrivileges"])

		env, ok := process["env"].([]string)
		require.True(t, ok)
		require.Contains(t, env, "HOME=/home/fletcher")
		require.Contains(t, env, "FOO=bar")
	})

	t.Run("root points at the spec workdir", func(t *testing.T) {
		root, ok := cfg["root"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, spec.WorkDir, root["path"])
	})
}

func TestBuildMounts(t *testing.T) {
	t.Run("no extras yields only the standard pseudo-mounts", func(t *testing.T) {
		mounts := buildMounts(nil)
		require.Len(t, mounts, 6)
		dests := make([]string, 0, len(mounts))
		for _, m := range mounts {
			dest, ok := m["destination"].(string)
			require.True(t, ok)
			dests = append(dests, dest)
		}
		require.Equal(t, []string{"/proc", "/dev", "/dev/pts", "/dev/shm", "/dev/mqueue", "/sys"}, dests)
	})

	t.Run("read-only and read-write binds get matching options", func(t *testing.T) {
		mounts := buildMounts([]runtime.Mount{
			{Source: "/host/ro", Destination: "/fork/ro", ReadOnly: true},
			{Source: "/host/rw", Destination: "/fork/rw", ReadOnly: false},
		})
		require.Len(t, mounts, 8)
		require.Equal(t, map[string]any{
			"destination": "/fork/ro",
			"type":        "bind",
			"source":      "/host/ro",
			"options":     []string{"rbind", "ro"},
		}, mounts[6])
		require.Equal(t, map[string]any{
			"destination": "/fork/rw",
			"type":        "bind",
			"source":      "/host/rw",
			"options":     []string{"rbind", "rw"},
		}, mounts[7])
	})
}

func TestWriteOCIConfig(t *testing.T) {
	t.Run("empty workdir is rejected", func(t *testing.T) {
		err := writeOCIConfig(t.TempDir(), runtime.Spec{}, []string{"/bin/true"}, nil)
		require.Error(t, err)
		require.ErrorContains(t, err, "WorkDir is required")
	})

	t.Run("config.json parses back with the same shape", func(t *testing.T) {
		bundle := t.TempDir()
		spec := runtime.Spec{WorkDir: "/snapshots/job-2/rootfs"}
		args := []string{"/bin/sh", "-c", "exit 0"}
		mounts := []runtime.Mount{{Source: "/host/x", Destination: "/fork/x", ReadOnly: true}}
		require.NoError(t, writeOCIConfig(bundle, spec, args, mounts))

		data, err := os.ReadFile(filepath.Join(bundle, "config.json"))
		require.NoError(t, err)

		var got map[string]any
		require.NoError(t, json.Unmarshal(data, &got))
		require.Equal(t, "1.0.2", got["ociVersion"])

		process, ok := got["process"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, []any{"/bin/sh", "-c", "exit 0"}, process["args"])

		root, ok := got["root"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, spec.WorkDir, root["path"])

		// 6 standard pseudo-mounts plus the one bind.
		gotMounts, ok := got["mounts"].([]any)
		require.True(t, ok)
		require.Len(t, gotMounts, 7)
	})
}
