package fname_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/fname"
)

func TestCallerFuncNameSkip0IsImmediateCaller(t *testing.T) {
	got := fname.CallerFuncName(0)
	require.True(t, strings.HasSuffix(got, "TestCallerFuncNameSkip0IsImmediateCaller"),
		"got %q", got)
}

// nameFromHelper mirrors how background.Go uses CallerFuncName(1): it
// returns the name of the helper's own caller (one frame above itself).
func nameFromHelper() string {
	return fname.CallerFuncName(1)
}

func TestCallerFuncNameSkip1ReachesThroughHelper(t *testing.T) {
	got := nameFromHelper()
	require.True(t, strings.HasSuffix(got, "TestCallerFuncNameSkip1ReachesThroughHelper"),
		"got %q", got)
}

func TestShortFuncNameStripsPath(t *testing.T) {
	require.Equal(t, "Create",
		fname.ShortFuncName("github.com/joshjon/fletcher/internal/job.(*Service).Create"))
	require.Equal(t, "main", fname.ShortFuncName("main"))
}
