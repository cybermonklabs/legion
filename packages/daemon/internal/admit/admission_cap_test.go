package admit

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// capSetter is who changes the cap in these tests: the controller, from its session.
var capSetter = store.PauseActor{Kind: store.PauseByController, Session: "ses_controller"}

// setCap applies a change of the cap as the cap routes do (api's changeAdmissionCap): one fact of
// its own id through the handlers, cap 0 being a clear, made by the controller, its reason named
// after the event.
func setCap(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string, cap int) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "admission-cap", eventID,
		intake.AdmissionCapSet{Cap: cap, By: capSetter, Reason: "a person asked for " + eventID, At: fixedNow},
		engineStub{}, admission); err != nil {
		t.Fatalf("set the cap to %d: %v", cap, err)
	}
}

// assertSlotted compares the issues holding a slot, in slot order, with want.
func assertSlotted(t *testing.T, pool *pgxpool.Pool, step string, want ...string) {
	t.Helper()
	if got := slottedKeys(t, pool); !slices.Equal(got, want) {
		t.Fatalf("%s: slots = %v, want %v", step, got, want)
	}
}

// capChanges is every change of the test project's cap, newest first, as the state reads them.
func capChanges(t *testing.T, pool *pgxpool.Pool) []store.AdmissionCapChange {
	t.Helper()
	var changes []store.AdmissionCapChange
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if changes, err = store.AdmissionCapChangesTx(context.Background(), tx, testProject, store.AdmissionCapChangesShown); err != nil {
			t.Fatalf("read the cap changes: %v", err)
		}
	})
	return changes
}

// A cap set while the daemon runs is in force from the pass of its own fact on, with no restart
// and no other fact: four roots wait under a ceiling of 3, and a cap of 1 admits one of them, a
// raise to 2 admits a second on the raise's own pass, and a raise to the ceiling a third. A lower
// cap then stops nothing that runs and starts nothing new: the three keep their slots, the fourth
// root keeps waiting, and a controller's tick under the lower cap admits nothing either. Each
// change is recorded with the caps before and after it, who made it and why.
//
// The control is the first step: a pass that read the ceiling rather than the cap set admits three
// roots there, and a pass that read the cap only at boot admits none at the raises.
func TestACapSetAtRunTimeIsInForceFromItsOwnPass(t *testing.T) {
	pool := migratedPool(t)
	for _, key := range []string{"LEGION-A", "LEGION-B", "LEGION-C", "LEGION-D"} {
		seedWaiting(t, pool, key, key[len(key)-1:])
	}
	admission := quiet(t, 3)

	setCap(t, pool, admission, "cap-1", 1)
	assertSlotted(t, pool, "a cap of 1 under a ceiling of 3", "LEGION-A")
	setCap(t, pool, admission, "cap-2", 2)
	assertSlotted(t, pool, "a raise to 2", "LEGION-A", "LEGION-B")
	setCap(t, pool, admission, "cap-3", 3)
	assertSlotted(t, pool, "a raise to the ceiling", "LEGION-A", "LEGION-B", "LEGION-C")

	setCap(t, pool, admission, "cap-lowered", 1)
	assertSlotted(t, pool, "a lower cap", "LEGION-A", "LEGION-B", "LEGION-C")
	tick(t, pool, admission, "tick-under-the-lower-cap")
	assertSlotted(t, pool, "a tick under the lower cap", "LEGION-A", "LEGION-B", "LEGION-C")
	if got := issue(t, pool, "LEGION-D"); got.Status != "todo" {
		t.Fatalf("LEGION-D stands %q under the lower cap, want todo: it was started", got.Status)
	}

	var got []string
	for _, change := range capChanges(t, pool) {
		got = append(got, fmt.Sprintf("%d %d->%d of %d by %s %s for %q", change.Cap, change.CapBefore, change.CapAfter,
			change.Ceiling, change.By.Kind, change.By.Session, change.Reason))
	}
	want := []string{
		`1 3->1 of 3 by controller ses_controller for "a person asked for cap-lowered"`,
		`3 2->3 of 3 by controller ses_controller for "a person asked for cap-3"`,
		`2 1->2 of 3 by controller ses_controller for "a person asked for cap-2"`,
		`1 3->1 of 3 by controller ses_controller for "a person asked for cap-1"`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the cap changes =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A clear returns the project to the deployment's cap on the clear's own pass: under a ceiling of
// 2 and a cap of 1, one root runs, and the clear admits the second. The clear is recorded with no
// cap, from 1 to the ceiling.
func TestAClearReturnsTheProjectToTheDeploymentsCap(t *testing.T) {
	pool := migratedPool(t)
	for _, key := range []string{"LEGION-A", "LEGION-B", "LEGION-C"} {
		seedWaiting(t, pool, key, key[len(key)-1:])
	}
	admission := quiet(t, 2)
	setCap(t, pool, admission, "cap-1", 1)
	assertSlotted(t, pool, "a cap of 1 under a ceiling of 2", "LEGION-A")

	setCap(t, pool, admission, "clear", 0)
	assertSlotted(t, pool, "the clear", "LEGION-A", "LEGION-B")
	if changes := capChanges(t, pool); len(changes) != 2 || !changes[0].Cleared() || changes[0].CapBefore != 1 || changes[0].CapAfter != 2 {
		t.Fatalf("the newest change = %+v, want the clear from 1 to the ceiling 2", changes)
	}
}

// A change that changes nothing writes no row and logs one Debug line: a set of the cap that
// stands, which a person may ask for again, and a clear where no cap stands
// clears nothing. The project's changes are the two that changed something, and the log holds one
// Debug line for each of the three that did not, naming the cap in force.
//
// The control is the first set of 2, which does change the cap and is recorded and logged at Info:
// a rule that wrote no row for any set would leave the list empty.
func TestAChangeThatChangesNothingWritesNoRowAndLogsOneDebugLine(t *testing.T) {
	pool := migratedPool(t)
	var logs testlog.Buffer
	admission := newAdmission(t, 4, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	setCap(t, pool, admission, "clear-of-none", 0)
	setCap(t, pool, admission, "set-2", 2)
	setCap(t, pool, admission, "set-2-again", 2)
	setCap(t, pool, admission, "set-2-once-more", 2)
	setCap(t, pool, admission, "set-3", 3)

	changes := capChanges(t, pool)
	if len(changes) != 2 || changes[0].Cap != 3 || changes[0].Reason != "a person asked for set-3" || changes[1].Cap != 2 ||
		changes[1].Reason != "a person asked for set-2" {
		t.Fatalf("the cap changes = %+v, want the set of 3 and the first set of 2 alone", changes)
	}
	var unchanged, changed []string
	for line := range strings.Lines(logs.String()) {
		switch {
		case strings.Contains(line, "the cap change changes nothing"):
			if !strings.Contains(line, "level=DEBUG") {
				t.Errorf("the line of a change that changes nothing is not at Debug: %s", line)
			}
			unchanged = append(unchanged, line)
		case strings.Contains(line, "the cap changed"):
			changed = append(changed, line)
		}
	}
	if len(unchanged) != 3 || !strings.Contains(unchanged[0], "set=clear cap=4") ||
		!strings.Contains(unchanged[1], "set=2 cap=2") || !strings.Contains(unchanged[2], `reason="a person asked for set-2-once-more"`) {
		t.Fatalf("the lines of the changes that change nothing = %q, want the clear of none at 4 and the two sets of 2", unchanged)
	}
	if len(changed) != 2 || !strings.Contains(changed[0], "level=INFO") || !strings.Contains(changed[0], "cap_before=4 cap_after=2") ||
		!strings.Contains(changed[1], "cap_before=2 cap_after=3") {
		t.Fatalf("the lines of the changes = %q, want the set of 2 from 4 and the set of 3 from 2, at Info", changed)
	}
}

// The cap set is kept in the store, so a daemon that boots again admits under it (an admission
// built anew on the same database, as the boot builds one): the restart under the same ceiling
// admits as many roots as the cap set, and a restart under a ceiling a later configuration lowered
// below that cap admits as many as the ceiling. The cap is set while nothing waits, so no pass of
// the first admission admits anything, and every slot below is the restarted admission's.
//
// The controls: an admission that ignored the stored cap admits four roots in the first case, and
// one that took the stored cap over the ceiling admits three in the second.
func TestTheCapSetHoldsAcrossARestartUnderTheCeilingThen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ceiling int
		want    []string
	}{
		{name: "the same ceiling", ceiling: 4, want: []string{"LEGION-A", "LEGION-B", "LEGION-C"}},
		{name: "a ceiling lowered below the cap", ceiling: 2, want: []string{"LEGION-A", "LEGION-B"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			setCap(t, pool, quiet(t, 4), "cap-3", 3)
			assertSlotted(t, pool, "the cap set while nothing waits")
			for _, key := range []string{"LEGION-A", "LEGION-B", "LEGION-C", "LEGION-D"} {
				seedWaiting(t, pool, key, key[len(key)-1:])
			}

			tick(t, pool, quiet(t, tc.ceiling), "tick-after-the-restart")
			assertSlotted(t, pool, "the restarted daemon's first pass", tc.want...)
		})
	}
}

// Whether a slot stands free is judged under the cap set too (slotFree): with a ceiling of 2, a
// cap of 1 and its one slot taken, a todo nobody handed to Legion wakes the controller for no free
// slot. The control is the same todo under the clear, which leaves the second slot of the ceiling
// free and wakes the controller once.
func TestASlotIsFreeOnlyUnderTheCapSet(t *testing.T) {
	pool := migratedPool(t)
	registerController(t, pool)
	seedWaiting(t, pool, "LEGION-A", "A")
	admission := quiet(t, 2)
	setCap(t, pool, admission, "cap-1", 1)
	assertSlotted(t, pool, "a cap of 1 under a ceiling of 2", "LEGION-A")

	apply(t, pool, admission, "unhanded-todo", unlabelledTodo("LEGION-OUTSIDE"), engineStub{})
	assertControllerNotices(t, pool, nil)

	setCap(t, pool, admission, "clear", 0)
	outside := unlabelledTodo("LEGION-OUTSIDE")
	outside.Seq = 2
	apply(t, pool, admission, "unhanded-todo-again", outside, engineStub{})
	assertControllerNotices(t, pool, []effect{{kind: record.OutboxKindControllerNotice, issue: "LEGION-OUTSIDE",
		payload: record.ControllerNotice{Kind: record.TodoNotice}}})
}
