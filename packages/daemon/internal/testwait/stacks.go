package testwait

import (
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	shared "github.com/sjawhar/envoy/testwait"
)

// diagnostics is the shared stateful diagnostic of a wait that ended. started and end are the
// caller's clock values; endDescription names the caller's own end in a collateral line.
func diagnostics(t testing.TB, what string, started, end time.Time, spent bool, endDescription string) string {
	shared.MarkExplicit(t)
	return shared.Diagnostics(t, shared.Wait{What: what, Began: started, End: end, Spent: spent, EndDescription: endDescription})
}

func printStacks(t testing.TB, stacks string) { shared.Print(t, stacks) }

func fail(t testing.TB, text, stacks string) {
	t.Helper()
	shared.Fail(t, text, stacks)
}

func watchBound(t testing.TB, function string, end time.Time, span bool, endDescription, site string) {
	shared.Watch(t, shared.Bound{Function: function, End: end, Span: span, EndDescription: endDescription, Site: site})
}

// callerSite is the source line that asked an internal testwait helper for a bound.
func callerSite() string {
	_, file, line, _ := runtime.Caller(2)
	return filepath.Base(file) + ":" + strconv.Itoa(line)
}
