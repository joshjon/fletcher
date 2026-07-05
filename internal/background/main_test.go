package background_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running: the
// package exists to spawn goroutines, so leaks here would be silent and
// systemic.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
