//go:build linux && integration

package api_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/background"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// TestLiveVMProjectWorkspace creates and removes only its own disposable session
// on an explicitly selected daemon. It never restarts services or changes networking.
func TestLiveVMProjectWorkspace(t *testing.T) {
	socket, image := os.Getenv("FLETCHER_TEST_SOCKET"), os.Getenv("FLETCHER_TEST_IMAGE")
	if socket == "" || image == "" {
		t.Skip("set FLETCHER_TEST_SOCKET and FLETCHER_TEST_IMAGE to test a live VM daemon with a tmux-equipped image")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetUnencryptedHTTP2(true)
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	sessions := fletcherv1connect.NewSessionServiceClient(client, "http://unix")
	projects := fletcherv1connect.NewProjectServiceClient(client, "http://unix")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	name := fmt.Sprintf("workspace-smoke-%d", time.Now().UnixNano())
	// The unique name also permits cleanup after an uncertain create reply.
	ref := name
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(t.Context()), 45*time.Second)
		defer done()
		_, err := sessions.DeleteSession(cleanup, connect.NewRequest(&fletcherv1.DeleteSessionRequest{Ref: ref}))
		require.NoError(t, err, "remove disposable workspace session")
	})
	created, err := sessions.CreateSession(ctx, connect.NewRequest(&fletcherv1.CreateSessionRequest{
		Name: name, Image: image, Gateway: "off", EgressPolicy: "none",
	}))
	require.NoError(t, err)
	sessionID := created.Msg.GetSession().GetId()
	ref = sessionID
	require.Equal(t, "vm", created.Msg.GetSession().GetExecutionKind())
	dir := "/workspace/" + name
	initial, nested := dir+"/initial folder", dir+"/next folder"
	execGuest := func(command string) string {
		t.Helper()
		response, err := sessions.ExecSession(ctx, connect.NewRequest(&fletcherv1.ExecSessionRequest{Ref: sessionID, Command: command}))
		require.NoError(t, err)
		require.Zero(t, response.Msg.GetExitCode(), response.Msg.GetStderr())
		return response.Msg.GetStdout()
	}
	runtimeInfo := execGuest("uname -r; tmux -V; cat /proc/1/comm")
	require.Contains(t, runtimeInfo, "fletcher-init", "this smoke test requires a real Fletcher guest, not a mock driver")
	t.Logf("guest runtime: %s", strings.ReplaceAll(strings.TrimSpace(runtimeInfo), "\n", ", "))
	bootID := strings.TrimSpace(execGuest("cat /proc/sys/kernel/random/boot_id"))
	require.NotEmpty(t, bootID)
	execGuest(fmt.Sprintf("mkdir -p '%s' '%s'; printf retained > '%s/keep'; sync", initial, nested, dir))
	createProject := func(suffix, path string) *fletcherv1.Project {
		t.Helper()
		response, err := projects.CreateProject(ctx, connect.NewRequest(&fletcherv1.CreateProjectRequest{
			Name: name + suffix, SessionRef: sessionID, WorkingDir: path,
		}))
		require.NoError(t, err)
		return response.Msg.GetProject()
	}
	project := createProject("", initial)
	openProject := func(id string) {
		t.Helper()
		response, err := projects.OpenProject(ctx, connect.NewRequest(&fletcherv1.OpenProjectRequest{Ref: id}))
		require.NoError(t, err)
		require.Equal(t, sessionID, response.Msg.GetProject().GetSessionId())
	}
	openProject(project.GetId())
	openProject(project.GetId())
	mainName := project.GetId() + "-main"
	main := openWorkspaceTerminal(t, ctx, sessions, sessionID, mainName, initial)
	original := main.pane(t)
	require.Equal(t, initial, original[2])
	require.Equal(t, mainName, original[3])
	main.send(t, fmt.Sprintf("send-keys -t %s -l 'cd \"%s\"; printf \"workspace-marker\\n\"'\nsend-keys -t %s Enter\n", original[0], nested, original[0]))
	require.Eventually(t, func() bool { return main.pane(t)[2] == nested }, 10*time.Second, 50*time.Millisecond)
	second := openWorkspaceTerminal(t, ctx, sessions, sessionID, project.GetId()+"-second", main.pane(t)[2])
	secondPane := second.pane(t)
	require.Equal(t, nested, secondPane[2], "new tabs inherit the live cwd supplied by the client")
	require.NotEqual(t, original[0], secondPane[0])
	require.NotEqual(t, original[1], secondPane[1])
	main.detach()
	second.detach()
	reattached := openWorkspaceTerminal(t, ctx, sessions, sessionID, mainName, initial)
	reconnected := reattached.pane(t)
	require.Equal(t, original[:2], reconnected[:2], "reattachment must preserve the pane and shell process")
	require.Equal(t, nested, reconnected[2], "reattachment must not reset the live cwd to the bookmark")
	restored := reattached.command(t, "capture-pane -p -t "+reconnected[0], "workspace-marker")
	require.Contains(t, restored, "workspace-marker")

	_, err = projects.DeleteProject(ctx, connect.NewRequest(&fletcherv1.DeleteProjectRequest{Ref: project.GetId()}))
	require.NoError(t, err)
	require.Equal(t, reconnected, reattached.pane(t), "bookmark deletion must not terminate or replace its terminal")
	require.Equal(t, "retained", execGuest("cat '"+dir+"/keep'"))
	reattached.detach()
	project = createProject("-cold", initial)
	_, err = sessions.StopSession(ctx, connect.NewRequest(&fletcherv1.StopSessionRequest{Ref: sessionID}))
	require.NoError(t, err)
	metadata, err := projects.GetProject(ctx, connect.NewRequest(&fletcherv1.GetProjectRequest{Ref: project.GetId()}))
	require.NoError(t, err)
	require.Equal(t, sessionID, metadata.Msg.GetProject().GetSessionId())
	stopped, err := sessions.GetSession(ctx, connect.NewRequest(&fletcherv1.GetSessionRequest{Ref: sessionID}))
	require.NoError(t, err)
	require.Equal(t, fletcherv1.SessionState_SESSION_STATE_STOPPED, stopped.Msg.GetSession().GetState(), "reading a bookmark must not start compute")
	openProject(project.GetId())
	require.Equal(t, "retained", execGuest("cat '"+dir+"/keep'"))
	wakeBootID := strings.TrimSpace(execGuest("cat /proc/sys/kernel/random/boot_id"))
	resumed := openWorkspaceTerminal(t, ctx, sessions, sessionID, mainName, initial)
	if wakeBootID == bootID {
		require.Equal(t, reconnected, resumed.pane(t), "hibernation must preserve the named shell and cwd")
		t.Log("Stop/Open restored the hibernated VM and named terminal")
	}
	_, err = sessions.RestartSession(ctx, connect.NewRequest(&fletcherv1.RestartSessionRequest{Ref: sessionID}))
	require.NoError(t, err)
	select {
	case <-resumed.done:
	case <-time.After(5 * time.Second):
		t.Fatal("cold restart did not end the old terminal stream")
	}
	resumed.detach()
	require.NotEqual(t, wakeBootID, strings.TrimSpace(execGuest("cat /proc/sys/kernel/random/boot_id")), "explicit Restart must cold-boot the retained disk")
	require.Equal(t, "retained", execGuest("cat '"+dir+"/keep'"))
	cold := openWorkspaceTerminal(t, ctx, sessions, sessionID, project.GetId()+"-main", initial)
	require.Equal(t, initial, cold.pane(t)[2])
	cold.detach()
	missing := createProject("-missing", dir+"/missing")
	_, err = projects.OpenProject(ctx, connect.NewRequest(&fletcherv1.OpenProjectRequest{Ref: missing.GetId()}))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "missing directories must not silently fall back")
	require.Contains(t, err.Error(), "directory")
	openProject(project.GetId())
	_, err = sessions.DeleteSession(ctx, connect.NewRequest(&fletcherv1.DeleteSessionRequest{Ref: sessionID}))
	require.NoError(t, err)
	_, err = projects.GetProject(ctx, connect.NewRequest(&fletcherv1.GetProjectRequest{Ref: project.GetId()}))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err), "session deletion must remove its registrations")
	t.Log("verified named-pane reconnect, live cwd, output capture, bookmark-only deletion, cold-boot disk reuse and missing-directory rejection")
}

type workspaceTerminal struct {
	stream  *connect.BidiStreamForClient[fletcherv1.ShellSessionRequest, fletcherv1.ShellSessionResponse]
	detach  func()
	mu      sync.Mutex
	output  strings.Builder
	changed chan struct{}
	done    chan struct{}
	err     error
	query   int
}

func openWorkspaceTerminal(t *testing.T, ctx context.Context, client fletcherv1connect.SessionServiceClient, ref, name, dir string) *workspaceTerminal {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	terminal := &workspaceTerminal{stream: client.ShellSession(ctx), changed: make(chan struct{}, 1), done: make(chan struct{})}
	require.NoError(t, terminal.stream.Send(&fletcherv1.ShellSessionRequest{Msg: &fletcherv1.ShellSessionRequest_Start{Start: &fletcherv1.ShellStart{
		Ref: ref, TerminalName: name, WorkDir: dir, Term: "xterm-256color", Cols: 100, Rows: 30, ControlMode: true,
	}}}))
	terminal.detach = sync.OnceFunc(func() {
		cancel()
		_ = terminal.stream.CloseRequest()
		_ = terminal.stream.CloseResponse()
		select {
		case <-terminal.done:
		case <-time.After(5 * time.Second):
			t.Error("workspace terminal did not detach")
		}
	})
	t.Cleanup(terminal.detach)
	background.GoNamed(ctx, "test.workspace.receive", func(context.Context) {
		defer close(terminal.done)
		for {
			message, err := terminal.stream.Receive()
			if err != nil {
				terminal.err = err
				return
			}
			terminal.mu.Lock()
			terminal.output.Write(message.GetData())
			terminal.mu.Unlock()
			select {
			case terminal.changed <- struct{}{}:
			default:
			}
		}
	})
	terminal.wait(t, "%session-changed", 0)
	return terminal
}

func (w *workspaceTerminal) send(t *testing.T, command string) {
	t.Helper()
	require.NoError(t, w.stream.Send(&fletcherv1.ShellSessionRequest{Msg: &fletcherv1.ShellSessionRequest_Stdin{Stdin: []byte(command)}}))
}

func (w *workspaceTerminal) wait(t *testing.T, text string, offset int) string {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		w.mu.Lock()
		output := w.output.String()[offset:]
		w.mu.Unlock()
		if strings.Contains(output, text) {
			return output
		}
		select {
		case <-w.changed:
		case <-w.done:
			t.Fatalf("workspace terminal ended before %q: %v", text, w.err)
		case <-timer.C:
			t.Fatalf("workspace terminal did not produce %q; output: %q", text, output)
		}
	}
}

func (w *workspaceTerminal) command(t *testing.T, command, expected string) string {
	t.Helper()
	w.mu.Lock()
	offset := w.output.Len()
	w.mu.Unlock()
	w.send(t, command+"\n")
	return w.wait(t, expected, offset)
}

func (w *workspaceTerminal) pane(t *testing.T) []string {
	t.Helper()
	w.query++
	marker := fmt.Sprintf("FLETCHER_PROBE_%d=", w.query)
	end := fmt.Sprintf("|FLETCHER_END_%d", w.query)
	output := w.command(t, "display-message -p '"+marker+"#{pane_id}|#{pane_pid}|#{pane_current_path}|#{session_name}"+end+"'", end)
	for line := range strings.SplitSeq(strings.ReplaceAll(output, "\r", ""), "\n") {
		if value, ok := strings.CutPrefix(line, marker); ok {
			fields := strings.Split(strings.TrimSuffix(value, end), "|")
			require.Len(t, fields, 4)
			return fields
		}
	}
	t.Fatalf("missing pane response in %q", output)
	return nil
}
