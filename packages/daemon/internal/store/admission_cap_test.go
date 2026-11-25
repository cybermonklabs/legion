package store

import (
	"context"
	"fmt"
	"strings"
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
	inForce := func(project string) AdmissionCapChange {
		t.Helper()
		var newest AdmissionCapChange
		transact("read the cap of "+project, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			newest, err = AdmissionCapTx(ctx, tx, project)
			return err
		})
		return newest
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

	if newest := inForce("LEGION"); newest != (AdmissionCapChange{}) {
		t.Fatalf("a project nobody changed the cap of reads %+v, want the zero change", newest)
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
	if newest := inForce("LEGION"); newest != first {
		t.Fatalf("LEGION's cap in force = %+v; want the change written, %+v", newest, first)
	}

	second := record(AdmissionCapChange{Project: "LEGION", Cap: 3, CapBefore: 6, CapAfter: 3, Ceiling: 8,
		By: PauseActor{Kind: PauseByOperator}, Reason: "spend per hour is high", At: capChangeAt.Add(time.Hour)})
	if newest := inForce("LEGION"); newest != second {
		t.Fatalf("LEGION's cap in force once the operator changed it = %+v; want %+v", newest, second)
	}

	cleared := record(AdmissionCapChange{Project: "LEGION", CapBefore: 3, CapAfter: 8, Ceiling: 8,
		By: byController, Reason: "the signals are back to normal", At: capChangeAt.Add(2 * time.Hour)})
	if newest := inForce("LEGION"); !newest.Cleared() || newest != cleared {
		t.Fatalf("LEGION's newest change once cleared = %+v; want the clear, %+v", newest, cleared)
	}
	if got := EffectiveAdmissionCap(4, 8, cleared); got != 4 {
		t.Fatalf("the cap in force after a clear = %d, want the starting cap 4, not the ceiling 8", got)
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

// The store keeps every change of the cap, and the state lists the newest
// AdmissionCapChangesShown of them (dispatch://INFRA-5616/spec#b-r7-tick-log): 53 changes leave 53
// rows of the project's in the table, and the list the state reads holds the newest 50, newest
// first, the oldest three left out of it but not out of the table. The count is read from the
// table itself, not through the list, so a write that trims the table to the list's length fails
// here, and so does a list that returns every row or the oldest 50.
func TestTheStoreKeepsEveryChangeAndTheStateListsTheNewestFifty(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	const made = AdmissionCapChangesShown + 3
	ids := make([]int64, 0, made)
	for i := range made {
		if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			written, err := RecordAdmissionCapChangeTx(ctx, tx, AdmissionCapChange{Project: "LEGION", Cap: 1 + i%2,
				CapBefore: 2 - i%2, CapAfter: 1 + i%2, Ceiling: 4, By: PauseActor{Kind: PauseByOperator},
				Reason: fmt.Sprintf("change %d", i), At: capChangeAt.Add(time.Duration(i) * time.Minute)})
			ids = append(ids, written.ID)
			return err
		}); err != nil {
			t.Fatalf("record change %d: %v", i, err)
		}
	}
	var rows int
	if err := st.Pool().QueryRow(ctx, `select count(*) from project_admission_cap_changes where project = 'LEGION'`).Scan(&rows); err != nil {
		t.Fatalf("count the table's rows: %v", err)
	}
	if rows != made {
		t.Fatalf("the table holds %d rows of LEGION after %d changes, want every one of them", rows, made)
	}
	var listed []AdmissionCapChange
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		listed, err = AdmissionCapChangesTx(ctx, tx, "LEGION", AdmissionCapChangesShown)
		return err
	}); err != nil {
		t.Fatalf("list the changes: %v", err)
	}
	if len(listed) != AdmissionCapChangesShown {
		t.Fatalf("the list holds %d changes, want the newest %d", len(listed), AdmissionCapChangesShown)
	}
	for i, change := range listed {
		if want := ids[made-1-i]; change.ID != want || change.Reason != fmt.Sprintf("change %d", made-1-i) {
			t.Fatalf("change %d of the list = id %d %q, want id %d, change %d: the newest first", i, change.ID, change.Reason, want, made-1-i)
		}
	}
}

// A change refuses a negative cap, a reason that is only white space or longer than
// AdmissionCapReasonMax characters, and an actor of neither shape before any row is written.
func TestAChangeRefusesANegativeCapABlankOrLongReasonAndAnActorOfNoShape(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	valid := AdmissionCapChange{Project: "LEGION", Cap: 4, CapBefore: 8, CapAfter: 4, Ceiling: 8,
		By: PauseActor{Kind: PauseByOperator}, Reason: strings.Repeat("é", AdmissionCapReasonMax), At: capChangeAt}
	for name, change := range map[string]func(AdmissionCapChange) AdmissionCapChange{
		"a negative cap":               func(c AdmissionCapChange) AdmissionCapChange { c.Cap = -1; return c },
		"a blank reason":               func(c AdmissionCapChange) AdmissionCapChange { c.Reason = " \n"; return c },
		"a reason one character long":  func(c AdmissionCapChange) AdmissionCapChange { c.Reason += "x"; return c },
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
	// The control: the change every case above alters, its reason exactly at the bound in
	// characters (and twice that in bytes), is itself recorded.
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordAdmissionCapChangeTx(ctx, tx, valid)
		return err
	}); err != nil {
		t.Fatalf("the valid change: %v, want it recorded", err)
	}
}

// The cap a pass admits under is the starting cap where nobody set one or the newest change was a
// clear, and the smaller of the cap set and the ceiling otherwise: a cap set below the starting
// cap, above it and below the ceiling, at the ceiling, and above a ceiling a later configuration
// lowered.
func TestTheEffectiveAdmissionCapIsTheStartWhereNoneStandsElseTheSmallerOfTheSetCapAndTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		name                string
		start, ceiling, set int
		want                int
	}{
		{name: "none set, or a clear", start: 4, ceiling: 8, want: 4},
		{name: "set below the starting cap", start: 4, ceiling: 8, set: 3, want: 3},
		{name: "set above the starting cap, under the ceiling", start: 4, ceiling: 8, set: 6, want: 6},
		{name: "set at the ceiling", start: 4, ceiling: 8, set: 8, want: 8},
		{name: "set above a ceiling lowered since", start: 4, ceiling: 5, set: 8, want: 5},
	} {
		if got := EffectiveAdmissionCap(tc.start, tc.ceiling, AdmissionCapChange{Cap: tc.set}); got != tc.want {
			t.Errorf("%s: EffectiveAdmissionCap(%d, %d, %d) = %d, want %d", tc.name, tc.start, tc.ceiling, tc.set, got, tc.want)
		}
	}
}
