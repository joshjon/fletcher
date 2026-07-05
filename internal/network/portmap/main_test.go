package portmap

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running; the
// discovery and renewal paths spawn goroutines that must stop with their
// contexts.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
