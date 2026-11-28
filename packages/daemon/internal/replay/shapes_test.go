package replay

import (
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// gateShape is a shape a release wrote its `workflow: design gate registered` line in
// (ParseLogLine).
type gateShape string

const (
	// gateToday is the shape of b7866f96 on: step, changes_requested and gated_steps.
	gateToday gateShape = "today's"
	// gateBeforeSteps is the shape of the releases from 5c4548f0 until b7866f96: changes_requested
	// and the policy of the deployment's design gate, and no step or gated_steps.
	gateBeforeSteps gateShape = "before b7866f96"
	// gateBeforeChangesRequested is the shape of the releases before 5c4548f0: the policy, and
	// none of the three.
	gateBeforeChangesRequested gateShape = "before 5c4548f0"
)

// registeredAs is key's architect registering version 1 of its spec at at, open and with a changes
// request standing on it as given, in the shape a release wrote the line in (workflow's
// gateRegistered at that release). The policy an older line names is off where the registration
// opened the gate at once, as a release that read no review opened it only where the deployment
// gated nothing.
func registeredAs(b *windowBuilder, at time.Time, key string, open, changesRequested bool, shape gateShape) {
	b.t.Helper()
	policy := "root-issues"
	if open {
		policy = "off"
	}
	args := []any{"issue", key, "artifact", b.artifact(key)}
	switch shape {
	case gateToday:
		args = append(args, "step", string(record.DesignStepSpec), "version", 1, "open", open, "changes_requested", changesRequested,
			"gated_steps", []string{})
	case gateBeforeSteps:
		args = append(args, "version", 1, "open", open, "changes_requested", changesRequested, "policy", policy)
	case gateBeforeChangesRequested:
		args = append(args, "version", 1, "open", open, "policy", policy)
	}
	b.info(at, "workflow: design gate registered", args...)
}

// deathsRanOut is the why of a failed claim whose worker's deaths with work outstanding ran out.
const deathsRanOut = "deaths with work outstanding ran out"

// The daemon's log holds older shapes of two line kinds, and the trace reads each as the release
// that wrote it meant it (ParseLogLine). A window that holds a design gate registration in each of
// its three shapes — open at once with no changes_requested (before 5c4548f0), with a changes
// request and no step (before b7866f96), and today's — and an implementer's failed claim with no
// cause (before 97519812) beside one with its cause, gives the same visits, holds and gate facts as
// the same window written in today's shapes alone, a line with no cause being one whose cause is
// unrecorded; and the older registrations name no gated steps. A line of a shape no release wrote,
// or with a member of another JSON type, is still refused.
func TestTheTraceReadsTheOlderShapesAsTheirReleasesMeantThem(t *testing.T) {
	t.Parallel()
	window := func(t *testing.T, older bool) Window {
		shape := func(written gateShape) gateShape {
			if older {
				return written
			}
			return gateToday
		}
		b := newWindowBuilder(t, clock(10, 0), clock(14, 0))
		b.reaches(2 * time.Hour)
		b.boot(clock(8, 0))
		designed := func(key, rank string, filed, at time.Time, open, changes bool, written gateShape) {
			promotedRoot(b, filed, key, rank)
			b.turnEnds(at, key, claim.RoleArchitect)
			registeredAs(b, at, key, open, changes, shape(written))
			if open {
				b.rootFirstPhase(at, key)
			}
		}
		designed("ACME-1", "a1", clock(8, 5), clock(8, 20), true, false, gateBeforeChangesRequested)
		designed("ACME-2", "a2", clock(8, 10), clock(8, 25), false, true, gateBeforeSteps)
		designed("ACME-3", "a3", clock(8, 15), clock(8, 30), true, false, gateToday)
		rootThrough(b, "ACME-4", "a4", clock(8, 20), clock(8, 35), builtMove{clock(9, 0), phase.Implementing, workflow.TriggerPlannerCompleted})
		cause := ""
		if !older {
			cause = string(supervise.CauseUnrecorded)
		}
		b.claimFailedAs(clock(9, 20), "ACME-4", claim.RoleImplementer, cause, deathsRanOut)
		rootThrough(b, "ACME-5", "a5", clock(8, 25), clock(8, 40), builtMove{clock(9, 5), phase.Implementing, workflow.TriggerPlannerCompleted})
		b.claimFailedAs(clock(9, 25), "ACME-5", claim.RoleImplementer, string(supervise.CauseDied), deathsRanOut)
		return b.build()
	}
	mixedWindow := window(t, true)
	mixed, today := placedTrace(t, mixedWindow), placedTrace(t, window(t, false))

	if keys := slices.Sorted(maps.Keys(mixed.Issues)); !slices.Equal(keys, slices.Sorted(maps.Keys(today.Issues))) {
		t.Fatalf("the window in the older shapes holds the issues %v, in today's %v", keys, slices.Sorted(maps.Keys(today.Issues)))
	}
	for key, want := range today.Issues {
		got := mixed.Issues[key]
		if !sameReading(got.Visits, want.Visits) {
			t.Errorf("%s's visits read from the older shapes are\n  %s\nfrom today's\n  %s", key, describeReading(got.Visits), describeReading(want.Visits))
		}
		if !sameReading(got.Waits, want.Waits) {
			t.Errorf("%s's waits read from the older shapes are\n  %s\nfrom today's\n  %s", key, describeReading(got.Waits), describeReading(want.Waits))
		}
		if !sameReading(got.Gates, want.Gates) {
			t.Errorf("%s's gate facts read from the older shapes are\n  %s\nfrom today's\n  %s", key, describeReading(got.Gates), describeReading(want.Gates))
		}
	}

	// What the older lines meant, said outright: each registration is of the spec, the oldest
	// requested no changes and the middle one did, and the failure with no cause holds its issue
	// for an unrecorded one.
	registration := func(key string) GateFact {
		for _, fact := range mixed.Issues[key].Gates {
			if fact.Kind == GateRegisteredFact {
				return fact
			}
		}
		t.Fatalf("the trace holds no registration of %s", key)
		return GateFact{}
	}
	for _, check := range []struct {
		key                    string
		open, changesRequested bool
	}{{"ACME-1", true, false}, {"ACME-2", false, true}} {
		if fact := registration(check.key); fact.Step != record.DesignStepSpec || fact.Open != check.open || fact.ChangesRequested != check.changesRequested {
			t.Errorf("%s's registration reads as %+v; want the spec, open %t, changes requested %t", check.key, fact, check.open, check.changesRequested)
		}
	}
	held := slices.IndexFunc(mixed.Issues["ACME-4"].Waits, func(w Wait) bool { return w.Kind == WaitHeld })
	if held < 0 || mixed.Issues["ACME-4"].Waits[held].Reason != string(supervise.CauseUnrecorded) ||
		!mixed.Issues["ACME-4"].Waits[held].Start.Equal(clock(9, 20)) || mixed.Issues["ACME-4"].Waits[held].From != phase.Implementing {
		t.Errorf("ACME-4's waits are %+v; want it held from implementing at 09:20 for %s", mixed.Issues["ACME-4"].Waits, supervise.CauseUnrecorded)
	}
	for _, raw := range mixedWindow.Log {
		line, err := ParseLogLine(raw)
		if err != nil {
			t.Fatal(err)
		}
		if registered, ok := line.(GateRegistered); ok && (registered.Issue == "ACME-1" || registered.Issue == "ACME-2") && registered.GatedSteps != nil {
			t.Errorf("the older registration of %s names the gated steps %v; want none", registered.Issue, registered.GatedSteps)
		}
	}

	at := clock(9, 0)
	gate := "workflow: design gate registered"
	gateHead := []any{"issue", "ACME-1", "artifact", "00000000-0000-4000-8000-0000000000a1"}
	for _, refused := range []struct {
		name string
		msg  string
		args []any
	}{
		{"a registration with step and no gated_steps", gate, append(slices.Clone(gateHead), "step", "spec", "version", 1, "open", true,
			"changes_requested", false)},
		{"a registration with gated_steps and no step", gate, append(slices.Clone(gateHead), "version", 1, "open", true,
			"changes_requested", false, "gated_steps", []string{})},
		{"a registration with step and gated_steps and no changes_requested", gate, append(slices.Clone(gateHead), "step", "spec",
			"version", 1, "open", true, "gated_steps", []string{})},
		{"an older registration whose changes_requested is a string", gate, append(slices.Clone(gateHead), "version", 1, "open", true,
			"changes_requested", "false", "policy", "off")},
		{"an older registration whose open is a string", gate, append(slices.Clone(gateHead), "version", 1, "open", "true",
			"policy", "off")},
		{"a failed claim whose cause is a number", "supervise: claim failed", []any{"claim", "legion-acme-acme-4-implementer",
			"cause", 3, "why", deathsRanOut}},
		{"an older failed claim with no why", "supervise: claim failed", []any{"claim", "legion-acme-acme-4-implementer",
			"launchFailures", 0, "deaths", 3}},
	} {
		t.Run(refused.name, func(t *testing.T) {
			if line, err := ParseLogLine(jsonLine(t, at, slog.LevelInfo, refused.msg, refused.args...)); err == nil {
				t.Errorf("ParseLogLine reads it as %s; want it refused", describeReading(line))
			}
		})
	}
}
