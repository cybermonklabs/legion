package testwait

import (
	"strings"
	"testing"
	"time"

	shared "github.com/sjawhar/envoy/testwait"
	_ "github.com/sjawhar/envoy/testnatsinprocess"
)

// A daemon test binary that links Envoy's in-process testnats reaches the one public testwait
// state. The first call below is the same shared Diagnostics call Envoy's internal wrapper makes;
// the second is the daemon wrapper. The latter's spent wait sees the former's completed wait and
// names it rather than claiming no testwait wait used the deadline up.
func TestADaemonCollateralSeesEnvoyStateInOneBinary(t *testing.T) {
	shared.ResetForTest()
	t.Cleanup(shared.ResetForTest)
	now := time.Now()
	shared.Diagnostics(t, shared.Wait{What: "Envoy's wait", Began: now.Add(-24 * time.Hour), End: now.Add(-time.Millisecond), EndDescription: "the caller's end"})
	got := diagnostics(t, "the daemon's spent wait", now, now.Add(-time.Second), true, "the caller's end")
	if !strings.Contains(got, "longest completed testwait give-up recorded so far waited") || !strings.Contains(got, "waiting for Envoy's wait") {
		t.Errorf("the daemon collateral line did not see Envoy's shared state:\n%s", got)
	}
}
