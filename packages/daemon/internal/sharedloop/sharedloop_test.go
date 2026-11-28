// Package sharedloop holds the daemon module's compiled import of the model loop Dispatch's server
// answers through, github.com/sjawhar/envoy/agentloop. The loop is shared by both modules of
// go.work, so it lives outside packages/envoy/internal/; were it moved under internal/, this
// package would stop compiling, which is the rule that keeps it shared rather than Dispatch's
// alone.
package sharedloop

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/agentloop"
)

// The daemon module reaches agentloop.Run, and Run refuses a request it cannot make - here one
// with no model and no bounds - before it calls anything.
func TestTheDaemonModuleReachesTheSharedModelLoop(t *testing.T) {
	if _, err := agentloop.Run(context.Background(), agentloop.Request{}); err == nil {
		t.Fatal("agentloop.Run made a run of a request with no model and no bounds, want it refused")
	}
}
