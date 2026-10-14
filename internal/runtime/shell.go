package runtime

import (
	"path"
	"strings"
	"unicode"

	"github.com/joshjon/fletcher/internal/errs"
)

// Validate checks guest shell targets without treating a working directory as an isolation boundary.
func (s ShellSpec) Validate() error {
	if s.WorkDir != "" {
		if len(s.WorkDir) > 4096 || !path.IsAbs(s.WorkDir) || path.Clean(s.WorkDir) != s.WorkDir || strings.IndexFunc(s.WorkDir, unicode.IsControl) >= 0 {
			return errs.New(errs.CategoryInvalidArgument, "working directory must be a clean absolute guest path of at most 4096 bytes without control characters")
		}
		if s.TerminalName == "" {
			return errs.New(errs.CategoryInvalidArgument, "a working directory requires a named terminal")
		}
	}
	if s.TerminalName == "" {
		return nil
	}
	if len(s.TerminalName) > 128 {
		return errs.New(errs.CategoryInvalidArgument, "terminal name must be at most 128 bytes")
	}
	for _, r := range s.TerminalName {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if !valid {
			return errs.New(errs.CategoryInvalidArgument, "terminal name must contain only letters, digits, '_' or '-'")
		}
	}
	return nil
}
