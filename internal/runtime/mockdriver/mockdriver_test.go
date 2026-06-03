package mockdriver_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/runtime/mockdriver"
)

func TestRunReturnsZeroForSuccessfulCommand(t *testing.T) {
	d := mockdriver.New()
	var stdout, stderr bytes.Buffer
	res, err := d.Run(context.Background(), runtime.Spec{
		JobID:   "job_test",
		Command: "echo hello && echo world 1>&2",
	}, &stdout, &stderr)
	require.NoError(t, err)
	require.Equal(t, int32(0), res.ExitCode)
	require.Contains(t, stdout.String(), "hello")
	require.Contains(t, stderr.String(), "world")
}

func TestRunReturnsNonZeroForFailingCommand(t *testing.T) {
	d := mockdriver.New()
	res, err := d.Run(context.Background(), runtime.Spec{Command: "exit 7"}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, int32(7), res.ExitCode)
}

func TestRunHonoursContextCancellation(t *testing.T) {
	d := mockdriver.New()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := d.Run(ctx, runtime.Spec{Command: "sleep 5"}, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
}
