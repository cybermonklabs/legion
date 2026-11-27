package sandbox

import (
	"testing"

	"github.com/sjawhar/envoy/testparallel"
)

// TestEveryTestCallsParallelOrSaysWhyNot holds the package's tests to the rule of a package whose
// tests run beside each other (testparallel.Require; docs/testing.md, "A test in a package whose
// tests run beside each other").
func TestEveryTestCallsParallelOrSaysWhyNot(t *testing.T) {
	t.Parallel()
	testparallel.Require(t, ".")
}
