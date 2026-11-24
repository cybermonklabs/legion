package main

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// budgetsUntilTheDeadlineMargin is what budgetsUntilTheDeadline leaves of the test binary's
// deadline, so a registration or a repair that never answers fails its row by name rather than at
// go test's own timeout.
const budgetsUntilTheDeadlineMargin = 30 * time.Second

// budgetsUntilTheDeadline gives each of rigs' registrations, and the consumer calls each one makes
// before it answers, what is left of the test binary's deadline less
// budgetsUntilTheDeadlineMargin, in place of their 3.5 s and 5 s (listenerDeps.registrationBudget,
// sessionDelivery.registrationCallBudget). It is for a row whose own work runs inside those bounds
// - a session's consumer lock it holds while it writes a row and a filter, which a registration's
// store work and its own consumer call both wait for, or a whole registration on another listener
// that a hook runs in each pass of a confirm's repair - and asserts what the registration writes,
// not how soon: a loaded machine slows the test's work as it slows the bus's, so on the product's
// bounds the row's verdict was the machine's. The rows that assert those bounds keep them
// (write_routes_frozen_bus_test.go).
func budgetsUntilTheDeadline(t *testing.T, rigs ...*sessionConsumerRig) {
	t.Helper()
	budget := testwait.Remaining(t, budgetsUntilTheDeadlineMargin)
	for _, rig := range rigs {
		rig.deps.registrationBudget = budget
		rig.deps.consumers.registrationCallBudget = budget
	}
}
