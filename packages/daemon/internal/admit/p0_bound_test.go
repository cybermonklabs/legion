package admit

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The P0 bound (C3 of dispatch://INFRA-5636, R10 of dispatch://INFRA-3199): it has no value unless
// legion.yaml gives one, and with no value every P0 issue that waits only for a slot starts. A
// value set while the daemon runs — a number, or none whatever legion.yaml says — replaces
// legion.yaml's until it is cleared, and the next admission pass uses it with no restart: each
// change is a fact (intake.P0BoundSet) whose own pass admits under it. At the bound a P0 issue
// waits first in the line and the pass logs the refusal with the bound. Every case stands at a cap
// its holders below P0 fill, so every slot a case gives out is one the cap would refuse.

// p0BoundChange applies one change of the P0 bound as the routes apply it, by the operator.
func p0BoundChange(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string, mode store.P0BoundMode, bound int) {
	t.Helper()
	change := intake.P0BoundSet{Mode: mode, Bound: bound, By: store.PauseActor{Kind: store.PauseByOperator},
		Reason: "the gateway refuses whole sessions", At: fixedNow}
	if _, err := intake.ApplyFact(context.Background(), pool, "api", eventID, change, engineStub{}, admission); err != nil {
		t.Fatalf("apply the P0 bound change %s: %v", eventID, err)
	}
}

// p0Line seeds the fixture every case of this file reads: a cap of one that LEGION-1, at P1,
// fills; P0 holders up to held; and P0 roots waiting, keyed LEGION-90 on, waiting in that order
// behind a P1 root that never ran, LEGION-80, which the P0 roots must stand ahead of.
func p0Line(t *testing.T, pool *pgxpool.Pool, held, waiting int) []string {
	t.Helper()
	p0Holder(t, pool, "LEGION-1", "A", record.P1)
	for i := range held {
		p0Holder(t, pool, "LEGION-"+string(rune('2'+i)), "B"+string(rune('a'+i)), record.P0)
	}
	p0Queued(t, pool, "LEGION-80", "C", record.P1, nil)
	keys := make([]string, waiting)
	for i := range keys {
		keys[i] = "LEGION-9" + string(rune('0'+i))
		p0Queued(t, pool, keys[i], "D"+string(rune('a'+i)), record.P0, nil)
	}
	return keys
}

// p0HoldersNow counts the P0 issues holding a slot.
func p0HoldersNow(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return holdersNow(t, pool).P0
}

// With no bound in legion.yaml and none set, every P0 issue that waits only for a slot starts,
// whatever the number of P0 holders already: here four hold a slot and five more wait.
func TestWithNoBoundEveryP0IssueStarts(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	waiting := p0Line(t, pool, 4, 5)

	tick(t, pool, admission, "tick")

	for _, key := range waiting {
		if !holdsSlot(t, pool, key) {
			t.Errorf("%s, at P0 with no bound, holds no slot", key)
		}
	}
	if holdsSlot(t, pool, "LEGION-80") {
		t.Error("LEGION-80, at P1 at a full cap, holds a slot")
	}
	if got := p0HoldersNow(t, pool); got != 9 {
		t.Fatalf("%d P0 issues hold a slot, want all 9", got)
	}
}

// A bound of 2 that legion.yaml gives is used from the first pass: with two P0 holders the third
// P0 issue starts no slot, waits first in the line, ahead of the P1 root, and the pass logs its
// refusal with the bound.
func TestAtTheBoundAP0IssueWaitsFirstInTheLineAndTheRefusalIsLogged(t *testing.T) {
	pool := migratedPool(t)
	var logged bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewJSONHandler(&logged, nil)))
	admission.UseP0Bound(new(2))
	waiting := p0Line(t, pool, 2, 1)

	tick(t, pool, admission, "tick")

	if holdsSlot(t, pool, waiting[0]) {
		t.Fatalf("%s holds a slot with two P0 holders at a bound of 2", waiting[0])
	}
	if line := waitingLine(t, pool, testProject, nil); len(line) == 0 || line[0] != waiting[0] {
		t.Fatalf("the waiting line is %v, want %s first: a P0 issue at the bound waits first in the line", line, waiting[0])
	}
	refusals := loggedAs(t, &logged, p0BoundRefusalMessage)
	if len(refusals) != 1 || refusals[0]["issue"] != waiting[0] || refusals[0]["p0_bound"] != float64(2) || refusals[0]["p0_holders"] != float64(2) {
		t.Fatalf("the refusals logged = %v, want one naming %s, p0_bound 2 and p0_holders 2; the log:\n%s", refusals, waiting[0], logged.String())
	}
}

// p0BoundRefusalMessage is the message of the line a pass writes for a P0 issue it holds at the
// bound.
const p0BoundRefusalMessage = "admission holds a P0 issue at the P0 bound"

// A value set while the daemon runs replaces legion.yaml's at the pass of its own fact, and a clear
// returns to legion.yaml's at the pass of its: the fixture's daemon never restarts. The bound is
// 2 in legion.yaml, with two P0 holders and two P0 issues waiting.
func TestABoundSetLiveReplacesTheConfiguredOneUntilItIsCleared(t *testing.T) {
	t.Run("a number", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		admission.UseP0Bound(new(2))
		waiting := p0Line(t, pool, 2, 2)
		tick(t, pool, admission, "tick")
		if got := p0HoldersNow(t, pool); got != 2 {
			t.Fatalf("%d P0 issues hold a slot under legion.yaml's bound of 2, want 2", got)
		}

		p0BoundChange(t, pool, admission, "set-3", store.P0BoundSet, 3)
		if got := p0HoldersNow(t, pool); got != 3 || !holdsSlot(t, pool, waiting[0]) {
			t.Fatalf("%d P0 issues hold a slot after a live bound of 3, want 3, %s among them", got, waiting[0])
		}

		// The clear returns to legion.yaml's 2: no slot is taken back, and the last P0 issue waits.
		p0BoundChange(t, pool, admission, "clear", store.P0BoundCleared, 0)
		tick(t, pool, admission, "after-clear")
		if got := p0HoldersNow(t, pool); got != 3 || holdsSlot(t, pool, waiting[1]) {
			t.Fatalf("%d P0 issues hold a slot after the clear, %s among them = %v; want 3, it not among them",
				got, waiting[1], holdsSlot(t, pool, waiting[1]))
		}
	})

	t.Run("none", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		admission.UseP0Bound(new(2))
		waiting := p0Line(t, pool, 2, 2)
		tick(t, pool, admission, "tick")
		if got := p0HoldersNow(t, pool); got != 2 {
			t.Fatalf("%d P0 issues hold a slot under legion.yaml's bound of 2, want 2", got)
		}

		p0BoundChange(t, pool, admission, "unbounded", store.P0BoundUnbounded, 0)
		for _, key := range waiting {
			if !holdsSlot(t, pool, key) {
				t.Errorf("%s holds no slot after the bound was set to none, whatever legion.yaml says", key)
			}
		}
		if holdsSlot(t, pool, "LEGION-80") {
			t.Error("LEGION-80, at P1 at a full cap, holds a slot")
		}
	})

	t.Run("a lower number stops nothing", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		p0Line(t, pool, 3, 1)
		tick(t, pool, admission, "tick")
		if got := p0HoldersNow(t, pool); got != 4 {
			t.Fatalf("%d P0 issues hold a slot with no bound, want 4", got)
		}

		p0BoundChange(t, pool, admission, "set-1", store.P0BoundSet, 1)
		if got := p0HoldersNow(t, pool); got != 4 {
			t.Fatalf("%d P0 issues hold a slot after a bound of 1, want the 4 that held one: a bound takes no slot back", got)
		}
	})
}

// The bound in force is read through one statement every reader shares (store.EffectiveP0Bound):
// legion.yaml's while no change stands, a set's number or none, and legion.yaml's again after a
// clear.
func TestTheBoundInForceIsTheNewestLiveValueElseTheConfiguredOne(t *testing.T) {
	configured := new(5)
	for _, tc := range []struct {
		name   string
		newest store.P0BoundChange
		found  bool
		want   *int
	}{
		{name: "no change", want: configured},
		{name: "a set of 2", newest: store.P0BoundChange{Mode: store.P0BoundSet, Bound: 2}, found: true, want: new(2)},
		{name: "a set of none", newest: store.P0BoundChange{Mode: store.P0BoundUnbounded}, found: true},
		{name: "a clear", newest: store.P0BoundChange{Mode: store.P0BoundCleared}, found: true, want: configured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := store.EffectiveP0Bound(configured, tc.newest, tc.found)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("the bound in force = %v, want %v", boundWords(got), boundWords(tc.want))
			}
		})
	}
	if got := store.EffectiveP0Bound(nil, store.P0BoundChange{}, false); got != nil {
		t.Fatalf("with no bound in legion.yaml and none set, the bound in force is %d, want none", *got)
	}
}
