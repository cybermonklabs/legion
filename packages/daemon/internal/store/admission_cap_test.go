package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// capChangeAt is the moment the first change of these tests is made.
var capChangeAt = time.Date(2026, 11, 24, 4, 8, 25, 0, time.UTC)

// The changes of the cap are one append-only list per project: each change is read back whole, the
// newest is the one in force, a clear leaves none in force, and the list answers newest first, at
// most the limit asked, of its own project alone. Each step reads through the reads the admission
// pass and the state make (AdmissionCapTx, AdmissionCapChangesTx), so a change written with a
// field lost, a read that took the oldest row for the one in force, a clear read as a cap, a list
// in the wrong order or past its limit, or a read of another project's changes fails here.
func TestTheCapChangesAreAListPerProjectWhoseNewestIsInForce(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	transact := func(name string, fn func(ctx context.Context, tx pgx.Tx) error) {
		t.Helper()
		if err := Transact(ctx, st.Pool(), fn); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	record := func(change AdmissionCapChange) AdmissionCapChange {
		t.Helper()
		var written AdmissionCapChange
		transact("record a change of "+change.Project, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			written, err = RecordAdmissionCapChangeTx(ctx, tx, change)
			return err
		})
		return written
	}
	inForce := func(project string) (AdmissionCapChange, bool) {
		t.Helper()
		var newest AdmissionCapChange
		var found bool
		transact("read the cap of "+project, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			newest, found, err = AdmissionCapTx(ctx, tx, project)
			return err
		})
		return newest, found
	}
	listed := func(project string, limit int) []AdmissionCapChange {
		t.Helper()
		var changes []AdmissionCapChange
		transact("list the changes of "+project, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			changes, err = AdmissionCapChangesTx(ctx, tx, project, limit)
			return err
		})
		return changes
	}

	if _, found := inForce("LEGION"); found {
		t.Fatal("a project nobody changed the cap of reads a change")
	}
	if changes := listed("LEGION", AdmissionCapChangesShown); changes == nil || len(changes) != 0 {
		t.Fatalf("the changes of a project nobody changed the cap of = %#v, want an empty list", changes)
	}

	byController := PauseActor{Kind: PauseByController, Session: "ses_controller"}
	first := record(AdmissionCapChange{Project: "LEGION", Cap: 6, CapBefore: 8, CapAfter: 6, Ceiling: 8,
		By: byController, Reason: "the merge queue waits 40 minutes", At: capChangeAt})
	record(AdmissionCapChange{Project: "INFRA", Cap: 2, CapBefore: 4, CapAfter: 2, Ceiling: 4,
		By: PauseActor{Kind: PauseByOperator}, Reason: "a pool is down", At: capChangeAt})
	if first.ID == 0 {
		t.Fatal("the change answered no id")
	}
	if newest, found := inForce("LEGION"); !found || newest != first {
		t.Fatalf("LEGION's cap in force = %+v, %v; want the change written, %+v", newest, found, first)
	}

	second := record(AdmissionCapChange{Project: "LEGION", Cap: 3, CapBefore: 6, CapAfter: 3, Ceiling: 8,
		By: PauseActor{Kind: PauseByOperator}, Reason: "spend per hour is high", At: capChangeAt.Add(time.Hour)})
	if newest, found := inForce("LEGION"); !found || newest != second {
		t.Fatalf("LEGION's cap in force once the operator changed it = %+v, %v; want %+v", newest, found, second)
	}

	cleared := record(AdmissionCapChange{Project: "LEGION", CapBefore: 3, CapAfter: 8, Ceiling: 8,
		By: byController, Reason: "the signals are back to normal", At: capChangeAt.Add(2 * time.Hour)})
	if newest, found := inForce("LEGION"); !found || !newest.Cleared() || newest != cleared {
		t.Fatalf("LEGION's newest change once cleared = %+v, %v; want the clear, %+v", newest, found, cleared)
	}
	if got := EffectiveAdmissionCap(8, cleared, true); got != 8 {
		t.Fatalf("the cap in force after a clear = %d, want the ceiling 8", got)
	}

	if got := listed("LEGION", AdmissionCapChangesShown); len(got) != 3 || got[0] != cleared || got[1] != second || got[2] != first {
		t.Fatalf("LEGION's changes = %+v, want the clear, the operator's 3 and the controller's 6, newest first", got)
	}
	if got := listed("LEGION", 2); len(got) != 2 || got[0] != cleared || got[1] != second {
		t.Fatalf("LEGION's newest two changes = %+v, want the clear and the operator's 3", got)
	}
	if got := listed("INFRA", AdmissionCapChangesShown); len(got) != 1 || got[0].Cap != 2 {
		t.Fatalf("INFRA's changes = %+v, want its own cap of 2 alone", got)
	}
}

// A change refuses a negative cap, a reason that is only white space and an actor of neither shape
// before any row is written.
func TestAChangeRefusesANegativeCapABlankReasonAndAnActorOfNoShape(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	valid := AdmissionCapChange{Project: "LEGION", Cap: 4, CapBefore: 8, CapAfter: 4, Ceiling: 8,
		By: PauseActor{Kind: PauseByOperator}, Reason: "a pool is down", At: capChangeAt}
	for name, change := range map[string]func(AdmissionCapChange) AdmissionCapChange{
		"a negative cap":               func(c AdmissionCapChange) AdmissionCapChange { c.Cap = -1; return c },
		"a blank reason":               func(c AdmissionCapChange) AdmissionCapChange { c.Reason = " \n"; return c },
		"a controller with no session": func(c AdmissionCapChange) AdmissionCapChange { c.By = PauseActor{Kind: PauseByController}; return c },
		"an operator with a session": func(c AdmissionCapChange) AdmissionCapChange {
			c.By = PauseActor{Kind: PauseByOperator, Session: "ses_1"}
			return c
		},
	} {
		err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := RecordAdmissionCapChangeTx(ctx, tx, change(valid))
			return err
		})
		if err == nil {
			t.Errorf("%s was recorded, want it refused", name)
		}
	}
	// The control: the change every case above alters is itself recorded.
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordAdmissionCapChangeTx(ctx, tx, valid)
		return err
	}); err != nil {
		t.Fatalf("the valid change: %v, want it recorded", err)
	}
}

// The cap a pass admits under is the ceiling where nobody set one or the newest change was a clear,
// and the smaller of the cap set and the ceiling otherwise: a cap set below the ceiling, at it, and
// above a ceiling a later configuration lowered.
func TestTheEffectiveAdmissionCapIsTheSmallerOfTheSetCapAndTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ceiling, set int
		found        bool
		want         int
	}{
		{name: "none set", ceiling: 8, want: 8},
		{name: "a clear", ceiling: 8, set: 0, found: true, want: 8},
		{name: "set below the ceiling", ceiling: 8, set: 3, found: true, want: 3},
		{name: "set at the ceiling", ceiling: 8, set: 8, found: true, want: 8},
		{name: "set above a ceiling lowered since", ceiling: 5, set: 8, found: true, want: 5},
	} {
		if got := EffectiveAdmissionCap(tc.ceiling, AdmissionCapChange{Cap: tc.set}, tc.found); got != tc.want {
			t.Errorf("%s: EffectiveAdmissionCap(%d, %d, %v) = %d, want %d", tc.name, tc.ceiling, tc.set, tc.found, got, tc.want)
		}
	}
}
