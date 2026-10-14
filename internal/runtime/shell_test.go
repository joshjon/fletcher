package runtime_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
)

func TestShellTargets(t *testing.T) {
	for _, spec := range []runtime.ShellSpec{
		{},
		{TerminalName: "main"},
		{TerminalName: "project_123-test", WorkDir: "/workspace/my project"},
		{TerminalName: "build", WorkDir: "/volume/app"},
		{TerminalName: "main", WorkDir: "/"},
	} {
		require.NoError(t, spec.Validate())
	}
	for _, spec := range []runtime.ShellSpec{
		{WorkDir: "/workspace"},
		{TerminalName: "a:b"},
		{TerminalName: "a.b"},
		{TerminalName: "a\n"},
		{TerminalName: "a/b"},
		{TerminalName: strings.Repeat("a", 129)},
		{TerminalName: "main", WorkDir: "relative"},
		{TerminalName: "main", WorkDir: "/a/../b"},
		{TerminalName: "main", WorkDir: "/a\x00"},
		{TerminalName: "main", WorkDir: "/a//b"},
	} {
		require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(spec.Validate()), "%+v", spec)
	}
}
