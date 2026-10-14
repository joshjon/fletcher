//go:build linux

package main

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

func TestNamedShellCommand(t *testing.T) {
	spec := guestproto.ShellSpec{TerminalName: "project_123-test", WorkDir: "/workspace/my project", ControlMode: true}
	cmd := buildShellCommand(spec, loginUser{home: "/home/fletcher"}, "/bin/bash", "/usr/bin/tmux", spec.WorkDir)
	require.Equal(t, []string{
		"/usr/bin/tmux", "-u", "-L", "fletcher", "-CC", "new-session", "-A", "-s",
		"project_123-test", "-c", "/workspace/my project", "/bin/bash", "-l",
	}, cmd.Args)
	require.Equal(t, spec.WorkDir, cmd.Dir)
	// Another label names an independent durable terminal in the same server.
	spec.TerminalName = "project_123-build"
	other := buildShellCommand(spec, loginUser{}, "/bin/bash", "/usr/bin/tmux", spec.WorkDir)
	require.Contains(t, other.Args, "project_123-build")
	require.NotContains(t, other.Args, "project_123-test")
}

func TestLegacyShellCommandKeepsTheDefaultTerminal(t *testing.T) {
	cmd := buildShellCommand(guestproto.ShellSpec{}, loginUser{}, "/bin/sh", "/usr/bin/tmux", "/workspace")
	require.Equal(t, []string{"/usr/bin/tmux", "-u", "-L", "fletcher", "new-session", "-A", "-s", "main", "-c", "/workspace", "/bin/sh", "-l"}, cmd.Args)
	plain := buildShellCommand(guestproto.ShellSpec{}, loginUser{}, "/bin/sh", "", "/workspace")
	require.Equal(t, []string{"/bin/sh", "-l"}, plain.Args)
}

func TestInvalidNamedTargetIsRejectedWithoutStartingAShell(t *testing.T) {
	cmd, err := shellCommand(guestproto.ShellSpec{TerminalName: "invalid:name"}, loginUser{})
	require.Error(t, err)
	require.Nil(t, cmd)
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	done := make(chan struct{})
	go func() { defer close(done); serveControl(server) }()
	require.NoError(t, guestproto.WriteRequest(client, guestproto.Request{
		Kind: guestproto.RequestShellTarget, Shell: guestproto.ShellSpec{TerminalName: "main", WorkDir: "relative"},
	}))
	require.NoError(t, guestproto.ReadShellReady(client))
	kind, data, err := guestproto.ReadFrame(client)
	require.NoError(t, err)
	require.Equal(t, guestproto.KindStderr, kind)
	require.Contains(t, string(data), "working directory")
	kind, data, err = guestproto.ReadFrame(client)
	require.NoError(t, err)
	require.Equal(t, guestproto.KindExit, kind)
	code, err := guestproto.DecodeExit(data)
	require.NoError(t, err)
	require.EqualValues(t, 1, code)
	<-done
}
