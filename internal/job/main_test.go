package job_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running; the
// supervisor tests spawn run loops that must fully drain on cancel.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
