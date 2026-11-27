package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The boot's stale-plugin pass holds the API's start until it returns, so it is bounded as a
// whole (stalePluginBootBound). Over a store that takes each read and never answers, two stale
// claims of a role that is none of Legion's hold the boot for the one bound, not for a bound
// each, and one line names the two claims left to the next sweep. No line of a failed read is
// written for a read the pass's own bound ended.
//
// The control is the pass without its own bound, run by hand: each read then ends at its own
// bound, one after the other, and the pass holds the boot for twice the bound.
func TestTheBootsStalePluginPassHoldsTheBootForOneBoundWhateverTheFleet(t *testing.T) {
	hung := func(ctx context.Context, issue string, role claim.Role) (runtime.Backend, error) {
		<-ctx.Done()
		return "", fmt.Errorf("the role %s of the plan of %s: %w", role, issue, ctx.Err())
	}
	logs := &syncBuffer{}
	s := stalePassSupervision(t, logs, hung)
	s.claims = staleProbers(t, 2)

	began := time.Now()
	s.bootStalePlugins(s.claims)
	held := time.Since(began)

	if held >= stalePluginBootBound+stalePluginBootBound/2 {
		t.Fatalf("the boot's pass held the boot %s over a store that never answers, want its one bound, %s", held, stalePluginBootBound)
	}
	lines := loggedLines(t, logs.String(), "supervise: the stale-plugin pass's bound ended before it read every claim")
	if len(lines) != 1 || lines[0]["claims"] != float64(2) {
		t.Fatalf("the lines naming the claims left = %+v, want one naming 2; it logged:\n%s", lines, logs.String())
	}
	if failed := loggedLines(t, logs.String(), stalePassReadFailed); len(failed) != 0 {
		t.Fatalf("the pass wrote %d lines of a failed read for reads its own bound ended, want none", len(failed))
	}
}
