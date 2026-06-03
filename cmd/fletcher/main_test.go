package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/buildinfo"
)

func TestVersionHumanOutput(t *testing.T) {
	out := captureStdout(t, func() {
		require.NoError(t, newApp().Run(context.Background(), []string{"fletcher", "version"}))
	})
	require.Contains(t, out, "fletcher "+buildinfo.Version)
	require.Contains(t, out, "commit "+buildinfo.Commit)
	require.Contains(t, out, "built "+buildinfo.Date)
}

func TestVersionJSONOutput(t *testing.T) {
	out := captureStdout(t, func() {
		require.NoError(t, newApp().Run(context.Background(), []string{"fletcher", "version", "--json"}))
	})
	var got buildinfo.Information
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, buildinfo.Info(), got)
}

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written. CLI commands write to os.Stdout directly; redirecting at the
// process level is the most honest way to assert on their output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	require.NoError(t, w.Close())
	return <-done
}
