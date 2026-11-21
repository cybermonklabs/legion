package record

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// copied is one value of a row of the copy and the sequence of the reading that wrote it.
type copied struct {
	value any
	seq   int64
}

// copiedValue is value's column of key's row and the sequence of the reading that wrote it
// (<value>_seq), read by SQL: a list as a []string, empty when it holds none, and a text as a
// string, a parent of none (null) as "".
func copiedValue(t *testing.T, tx pgx.Tx, key, value string) copied {
	t.Helper()
	query := fmt.Sprintf(`select %[1]s, %[1]s_seq from dispatch_issues where key = $1`, value)
	var seq int64
	switch value {
	case "blockers", "labels":
		var list []string
		if err := tx.QueryRow(t.Context(), query, key).Scan(&list, &seq); err != nil {
			t.Fatalf("read the %s of %s: %v", value, key, err)
		}
		return copied{list, seq}
	default:
		var text *string
		if err := tx.QueryRow(t.Context(), query, key).Scan(&text, &seq); err != nil {
			t.Fatalf("read the %s of %s: %v", value, key, err)
		}
		if text == nil {
			return copied{"", seq}
		}
		return copied{*text, seq}
	}
}

// holdsValues fails t unless key's row holds each value of want, by column, at its sequence.
func holdsValues(t *testing.T, tx pgx.Tx, key, after string, want map[string]copied) {
	t.Helper()
	for _, value := range slices.Sorted(maps.Keys(want)) {
		if got := copiedValue(t, tx, key, value); !reflect.DeepEqual(got, want[value]) {
			t.Errorf("after %s the %s of %s = %#v at %d, want %#v at %d", after, value, key, got.value, got.seq, want[value].value, want[value].seq)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

// Each of the five values of a row keeps the sequence of the reading that wrote it, and a reading
// changes it only at a higher sequence: a reading at the value's own sequence changes nothing,
// whether it carries another value or none, and an older one changes nothing either.
func TestTheCopyChangesOnlyOnANewerReading(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	for i, tc := range []struct {
		value string
		// carry makes reading carry v as the row's value, v being the value as the copy reads it.
		carry        func(reading *DispatchReading, v any)
		v1, v2, none any
	}{
		{value: "parent", carry: func(r *DispatchReading, v any) { r.Parent = Carry(v.(string)) },
			v1: "LEGION-801", v2: "LEGION-802", none: ""},
		{value: "status", carry: func(r *DispatchReading, v any) { r.Status = Carry(v.(string)) },
			v1: "todo", v2: "in_progress", none: ""},
		{value: "blockers", carry: func(r *DispatchReading, v any) { r.Blockers = Carry(v.([]string)) },
			v1: []string{"LEGION-901"}, v2: []string{"LEGION-902", "LEGION-903"}, none: []string{}},
		{value: "labels", carry: func(r *DispatchReading, v any) { r.Labels = Carry(v.([]string)) },
			v1: []string{"legion"}, v2: []string{"legion", "urgent"}, none: []string{}},
		{value: "rank", carry: func(r *DispatchReading, v any) { r.Rank = Carry(v.(string)) },
			v1: "a0", v2: "b0", none: ""},
	} {
		t.Run(tc.value, func(t *testing.T) {
			key := fmt.Sprintf("LEGION-%d", 900+i)
			put := func(tx pgx.Tx, seq int64, v any, full bool) {
				t.Helper()
				reading := DispatchReading{Key: key, Project: "LEGION", Seq: seq}
				if full {
					reading = DispatchReading{Key: key, Project: "LEGION", Seq: seq, Status: Carry("backlog"), Parent: Carry("LEGION-800"),
						Blockers: Carry([]string{"LEGION-904"}), Labels: Carry([]string{"design"}), Rank: Carry("z0")}
				}
				tc.carry(&reading, v)
				must(t, records.PutDispatchIssue(t.Context(), tx, reading))
			}
			holds := func(tx pgx.Tx, after string, v any, seq int64) {
				t.Helper()
				holdsValues(t, tx, key, after, map[string]copied{tc.value: {v, seq}})
			}
			inTx(t, st, func(tx pgx.Tx) {
				put(tx, 100, tc.v1, true)
				holds(tx, "a reading at 100 carrying every value", tc.v1, 100)
				put(tx, 100, tc.v2, false)
				holds(tx, "a reading at 100 carrying another value", tc.v1, 100)
				put(tx, 100, tc.none, false)
				holds(tx, "a reading at 100 carrying none", tc.v1, 100)
				if tc.value == "parent" {
					children, err := records.DispatchChildren(t.Context(), tx, []string{"LEGION-801"})
					must(t, err)
					if got := children["LEGION-801"]; len(got) != 1 || got[0].Key != key {
						t.Fatalf("the children of LEGION-801 after the readings at 100 = %#v, want %s alone", got, key)
					}
				}
				put(tx, 99, tc.v2, false)
				holds(tx, "a reading at 99", tc.v1, 100)
				put(tx, 101, tc.v2, false)
				holds(tx, "a reading at 101", tc.v2, 101)
			})
		})
	}
}

// A value no reading wrote is at sequence 0, and a reading at any sequence fills it, the one
// sequence a value already holds included; every value a reading did write stays as it wrote it.
// So the release's read, which carries no labels and no rank, leaves both for a later reading at
// its own sequence to fill.
func TestAnEqualReadingFillsOnlyAValueNoReadingWrote(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 100,
			Status: Carry("todo"), Parent: Carry("LEGION-800"), Blockers: Carry([]string{"LEGION-901"})}))
		holdsValues(t, tx, "LEGION-900", "a reading at 100 carrying the status, the parent and the blockers", map[string]copied{
			"labels": {[]string{}, 0}, "rank": {"", 0}})

		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 100,
			Status: Carry("in_progress"), Parent: Carry("LEGION-802"), Blockers: Carry([]string{"LEGION-902"}),
			Labels: Carry([]string{"legion"}), Rank: Carry("a0")}))
		holdsValues(t, tx, "LEGION-900", "a reading at 100 carrying every value", map[string]copied{
			"labels": {[]string{"legion"}, 100}, "rank": {"a0", 100},
			"status": {"todo", 100}, "parent": {"LEGION-800", 100}, "blockers": {[]string{"LEGION-901"}, 100}})

		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-910", Project: "LEGION", Seq: 50,
			Blockers: Carry([]string{"LEGION-901"})}))
		holdsValues(t, tx, "LEGION-910", "a reading at 50 carrying the blockers of a new key", map[string]copied{
			"status": {"", 0}, "blockers": {[]string{"LEGION-901"}, 50}})
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-910", Project: "LEGION", Seq: 50,
			Status: Carry("todo")}))
		holdsValues(t, tx, "LEGION-910", "a reading at 50 carrying the status", map[string]copied{
			"status": {"todo", 50}, "blockers": {[]string{"LEGION-901"}, 50}})
	})
}

// A reading at a value's own sequence replaces no value a reading wrote, and removes none: no
// parent is a value a reading writes, so an equal reading that carries a parent leaves the issue
// under none, and the parent it names has no child.
func TestAnEqualReadingChangesNoValueItWrote(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	// parent is LEGION-900's parent as the copy holds it, "null" for none, and its sequence.
	parent := func(tx pgx.Tx) (string, int64) {
		t.Helper()
		var parent string
		var seq int64
		if err := tx.QueryRow(t.Context(), `select coalesce(parent, 'null'), parent_seq from dispatch_issues where key = $1`, "LEGION-900").Scan(&parent, &seq); err != nil {
			t.Fatalf("read the parent of LEGION-900: %v", err)
		}
		return parent, seq
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 100, Parent: Carry("")}))
		if got, seq := parent(tx); got != "null" || seq != 100 {
			t.Fatalf("after a reading at 100 carrying no parent the parent = %s at %d, want null at 100", got, seq)
		}
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 100, Parent: Carry("LEGION-800")}))
		if got, seq := parent(tx); got != "null" || seq != 100 {
			t.Fatalf("after a reading at 100 carrying LEGION-800 the parent = %s at %d, want null at 100", got, seq)
		}
		children, err := records.DispatchChildren(t.Context(), tx, []string{"LEGION-800"})
		must(t, err)
		if len(children["LEGION-800"]) != 0 {
			t.Fatalf("the children of LEGION-800 = %#v, want none", children)
		}
	})
}

// A parent's children in the copy are its children in every project: a child of another project
// is a row of that project, written by the controller tick's read of it, and the parent waits for
// it as for a child of its own (ChildOpen). So DispatchChildren and HasDispatchChildren read the
// parent column alone, whatever project the child is in.
func TestDispatchChildrenReadsEveryProject(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-1", Project: "LEGION", Seq: 1,
			Status: Carry("in_progress"), Parent: Carry(""), Blockers: Carry[[]string](nil)}))
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "OTHR-5", Project: "OTHR", Seq: 3,
			Status: Carry("in_progress"), Parent: Carry("LEGION-1"), Blockers: Carry[[]string](nil)}))

		children, err := records.DispatchChildren(t.Context(), tx, []string{"LEGION-1"})
		must(t, err)
		if got := children["LEGION-1"]; len(got) != 1 || got[0].Key != "OTHR-5" || got[0].Project != "OTHR" {
			t.Fatalf("the children of LEGION-1 = %#v, want OTHR-5 of project OTHR alone", got)
		}
		has, err := records.HasDispatchChildren(t.Context(), tx, "LEGION-1")
		must(t, err)
		if !has {
			t.Fatal("HasDispatchChildren(LEGION-1) = false, want true: OTHR-5 is its child")
		}
	})
}

// Each value keeps the sequence of the reading that wrote it, not the row's: a reading that does
// not carry the parent leaves it at its own older sequence, so a reading between the two replaces
// the parent alone, and the row's sequence stays the highest it took.
func TestEachValueKeepsItsOwnSequence(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		for _, reading := range []DispatchReading{
			{Key: "LEGION-900", Project: "LEGION", Seq: 100, Status: Carry("todo"), Parent: Carry("LEGION-801"),
				Blockers: Carry([]string{"LEGION-901"}), Labels: Carry([]string{"legion"}), Rank: Carry("a0")},
			{Key: "LEGION-900", Project: "LEGION", Seq: 200, Status: Carry("in_progress"),
				Blockers: Carry([]string{"LEGION-902"}), Labels: Carry([]string{"legion", "urgent"}), Rank: Carry("b0")},
			{Key: "LEGION-900", Project: "LEGION", Seq: 150, Status: Carry("done"), Parent: Carry("LEGION-802"),
				Blockers: Carry([]string{"LEGION-903"}), Labels: Carry([]string{"design"}), Rank: Carry("c0")},
		} {
			must(t, records.PutDispatchIssue(t.Context(), tx, reading))
		}
		holdsValues(t, tx, "LEGION-900", "readings at 100, 200 without the parent, and 150", map[string]copied{
			"parent": {"LEGION-802", 150}, "status": {"in_progress", 200}, "blockers": {[]string{"LEGION-902"}, 200},
			"labels": {[]string{"legion", "urgent"}, 200}, "rank": {"b0", 200}})
		if seq, held, err := records.DispatchIssueSeq(t.Context(), tx, "LEGION-900"); err != nil || !held || seq != 200 {
			t.Fatalf("the row's sequence = %d, %v, %v; want 200", seq, held, err)
		}
	})
}

// A reading that carries no value is a fault of the caller, which would otherwise write a row of
// defaults no reading showed: it is refused, naming the key, and writes nothing, its claim
// included.
func TestAReadingThatCarriesNoValueIsRefused(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		err := records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 5,
			ClaimedBy: "user:alice", ClaimSeq: 5})
		if err == nil || !strings.Contains(err.Error(), "LEGION-900") {
			t.Fatalf("a reading carrying no value = %v, want an error naming LEGION-900", err)
		}
		if _, held, err := records.DispatchIssueSeq(t.Context(), tx, "LEGION-900"); err != nil || held {
			t.Fatalf("the copy holds a row of LEGION-900 (%v, %v), want none", held, err)
		}
		if holder, err := records.DispatchClaim(t.Context(), tx, "LEGION-900"); err != nil || holder != "" {
			t.Fatalf("the claim of LEGION-900 = %q, %v; want none", holder, err)
		}
	})
}

// The mirror is written from events that can arrive out of order — a boot listing beside a live
// event, a redelivery after a newer one — so each write is fenced on Dispatch's own sequence: a
// fresher observation lands, an older one leaves the row exactly as it stands.
func TestPutDispatchIssueTakesOnlyAFresherObservation(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 5,
			Status: Carry("todo"), Parent: Carry(""), Blockers: Carry([]string{"LEGION-901"})}))

		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 7,
			Status: Carry("in_progress"), Parent: Carry(""), Blockers: Carry[[]string](nil)}))
		rows, err := records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil {
			t.Fatalf("list the mirror: %v", err)
		}
		want := []DispatchIssueRow{{Key: "LEGION-900", Project: "LEGION", Status: "in_progress", Blockers: []string{}, Seq: 7, Labels: []string{}}}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("after the newer observation the mirror = %#v, want %#v", rows, want)
		}

		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 4,
			Status: Carry("backlog"), Parent: Carry(""), Blockers: Carry([]string{"LEGION-902"})}))
		rows, err = records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil {
			t.Fatalf("list the mirror: %v", err)
		}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("after the older observation the mirror = %#v, want the fresher row %#v", rows, want)
		}
	})
}

// A fact the daemon mints from its own record carries no Dispatch sequence. It is no reading of
// Dispatch's, so it writes nothing at all: no row for a key the copy has no row of, no value of a
// row that exists, and no value of that row no reading has written, which is at sequence 0 and
// which any reading fills.
func TestPutDispatchIssueWritesNothingWithoutASequence(t *testing.T) {
	records := NewStore()
	t.Run("a key with no row gets none", func(t *testing.T) {
		inTx(t, migratedStore(t), func(tx pgx.Tx) {
			must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION",
				Status: Carry("todo"), Parent: Carry(""), Blockers: Carry([]string{"LEGION-901"})}))
			rows, err := records.DispatchIssues(t.Context(), tx, "LEGION")
			if err != nil {
				t.Fatalf("list the mirror: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("a zero-sequence write on an unmirrored key left %#v, want nothing", rows)
			}
		})
	})
	t.Run("a row that exists keeps its values", func(t *testing.T) {
		inTx(t, migratedStore(t), func(tx pgx.Tx) {
			must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 6,
				Status: Carry("in_progress"), Parent: Carry(""), Blockers: Carry([]string{"LEGION-901"})}))
			must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION",
				Status: Carry("done"), Parent: Carry(""), Blockers: Carry[[]string](nil)}))
			rows, err := records.DispatchIssues(t.Context(), tx, "LEGION")
			if err != nil {
				t.Fatalf("list the mirror: %v", err)
			}
			want := []DispatchIssueRow{{Key: "LEGION-900", Project: "LEGION", Status: "in_progress", Blockers: []string{"LEGION-901"}, Seq: 6, Labels: []string{}}}
			if !reflect.DeepEqual(rows, want) {
				t.Fatalf("a zero-sequence write over a mirrored key left %#v, want it untouched as %#v", rows, want)
			}
		})
	})
	t.Run("a value no reading wrote stays unwritten", func(t *testing.T) {
		inTx(t, migratedStore(t), func(tx pgx.Tx) {
			must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 6,
				Status: Carry("in_progress"), Parent: Carry("LEGION-800"), Blockers: Carry([]string{"LEGION-901"})}))
			holdsValues(t, tx, "LEGION-900", "a reading at 6 carrying no labels", map[string]copied{"labels": {[]string{}, 0}})
			must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION",
				Labels: Carry([]string{"legion"})}))
			holdsValues(t, tx, "LEGION-900", "a zero-sequence write carrying labels", map[string]copied{"labels": {[]string{}, 0}})
		})
	})
}

// The database is shared by every project's daemon, and each judges what its own issues wait on:
// the listing is one project's rows, by key.
func TestDispatchIssuesListsOneProject(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		for _, reading := range []DispatchReading{
			{Key: "LEGION-902", Project: "LEGION", Seq: 2, Status: Carry("todo"), Parent: Carry(""), Blockers: Carry([]string{"LEGION-901"})},
			{Key: "LEGION-901", Project: "LEGION", Seq: 1, Status: Carry("done"), Parent: Carry(""), Blockers: Carry[[]string](nil)},
			{Key: "INFRA-7", Project: "INFRA", Seq: 3, Status: Carry("todo"), Parent: Carry(""), Blockers: Carry[[]string](nil)},
		} {
			must(t, records.PutDispatchIssue(t.Context(), tx, reading))
		}
		rows, err := records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil {
			t.Fatalf("list the mirror: %v", err)
		}
		want := []DispatchIssueRow{
			{Key: "LEGION-901", Project: "LEGION", Status: "done", Blockers: []string{}, Seq: 1, Labels: []string{}},
			{Key: "LEGION-902", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-901"}, Seq: 2, Labels: []string{}},
		}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("the mirror of LEGION = %#v, want %#v", rows, want)
		}
	})
}

// The claim on a mirror row carries a fence of its own. An observation that shows the claim
// writes it only over an older claim, one that does not show it leaves it, and the status fence
// and the claim fence each move only their own columns: a claim event newer than the row's status
// moves the claim alone, and an issue event older than that claim but newer than the status moves
// the status alone.
func TestTheMirrorsClaimHasAFenceOfItsOwn(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	read := func(tx pgx.Tx) DispatchIssueRow {
		t.Helper()
		rows, err := records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil || len(rows) != 1 {
			t.Fatalf("the mirror = %#v, %v; want one row", rows, err)
		}
		return rows[0]
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 5,
			Status: Carry("todo"), Parent: Carry(""), Blockers: Carry[[]string](nil), ClaimedBy: "user:alice", ClaimSeq: 5}))
		if got, want := read(tx), (DispatchIssueRow{Key: "LEGION-900", Project: "LEGION", Status: "todo", Blockers: []string{}, Seq: 5, ClaimedBy: "user:alice", ClaimSeq: 5, Labels: []string{}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("first observation = %#v, want %#v", got, want)
		}

		// A claim event at 9: the claim moves, the status stays.
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-900", "session:architect", 9))
		// An issue event at 7 showing an older claim: the status moves, the claim stays.
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 7,
			Status: Carry("in_progress"), Parent: Carry(""), Blockers: Carry[[]string](nil), ClaimedBy: "user:alice", ClaimSeq: 7}))
		if got, want := read(tx), (DispatchIssueRow{Key: "LEGION-900", Project: "LEGION", Status: "in_progress", Blockers: []string{}, Seq: 7, ClaimedBy: "session:architect", ClaimSeq: 9, Labels: []string{}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("after a newer claim and an older issue event = %#v, want %#v", got, want)
		}

		// A release older than the claim held, and an issue event that does not show the claim.
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-900", "", 8))
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 10,
			Status: Carry("testing"), Parent: Carry(""), Blockers: Carry[[]string](nil)}))
		if got := read(tx); got.ClaimedBy != "session:architect" || got.ClaimSeq != 9 || got.Status != "testing" || got.Seq != 10 {
			t.Fatalf("after an older release and a view without the claim = %#v, want the claim kept and the status moved", got)
		}

		// A newer release clears it, and a newer view showing nobody keeps it cleared.
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-900", "", 11))
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-900", Project: "LEGION", Seq: 12,
			Status: Carry("testing"), Parent: Carry(""), Blockers: Carry[[]string](nil), ClaimSeq: 12}))
		if got := read(tx); got.ClaimedBy != "" || got.ClaimSeq != 12 {
			t.Fatalf("after a release = %#v, want nobody at 12", got)
		}
	})
}

// A claim event for a key the mirror has no row of yet is kept, but makes no row: the row needs a
// status and the blockers, which only an issue's own observation carries, so the mirror lists
// nothing for the key until then. The issue's first observation, an older event delivered after the
// claim (one nak'd and redelivered), then shows the claim the newer event set, not its own. A claim
// with no sequence writes nothing.
func TestAClaimDeliveredBeforeItsIssuesFirstEventIsKept(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-901", "session:s-1", 2))
		rows, err := records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil || len(rows) != 0 {
			t.Fatalf("the mirror after a claim of an unmirrored key = %#v, %v; want no row", rows, err)
		}
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: "LEGION-901", Project: "LEGION", Seq: 1,
			Status: Carry("todo"), Parent: Carry(""), Blockers: Carry[[]string](nil), ClaimSeq: 1}))
		rows, err = records.DispatchIssues(t.Context(), tx, "LEGION")
		if want := []DispatchIssueRow{{Key: "LEGION-901", Project: "LEGION", Status: "todo", Blockers: []string{}, Seq: 1, ClaimedBy: "session:s-1", ClaimSeq: 2, Labels: []string{}}}; err != nil || !reflect.DeepEqual(rows, want) {
			t.Fatalf("the mirror after the issue's older first event = %#v, %v; want %#v", rows, err, want)
		}
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-901", "user:alice", 0))
		if rows, err = records.DispatchIssues(t.Context(), tx, "LEGION"); err != nil || rows[0].ClaimSeq != 2 || rows[0].ClaimedBy != "session:s-1" {
			t.Fatalf("the mirror after a claim with no sequence = %#v, %v; want the claim at 2 kept", rows, err)
		}
	})
}

// A project's refill mark empties that project's rows of the copy once, for the listing of the
// same boot to fill: with no mark nothing changes, the mark taken is that project's alone, and a
// mark already taken empties nothing again.
func TestTakingARefillMarkEmptiesOnlyThatProjectsRowsOnce(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	put := func(tx pgx.Tx, key string) {
		t.Helper()
		project, _, _ := strings.Cut(key, "-")
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchReading{Key: key, Project: project, Seq: 1, Status: Carry("todo")}))
	}
	held := func(tx pgx.Tx) []string {
		t.Helper()
		rows, err := tx.Query(t.Context(), `select key from dispatch_issues order by key`)
		must(t, err)
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		must(t, err)
		return keys
	}
	inTx(t, st, func(tx pgx.Tx) {
		for _, key := range []string{"LEGION-1", "LEGION-2", "OTHER-1"} {
			put(tx, key)
		}
		removed, refilled, err := records.TakeDispatchRefill(t.Context(), tx, "LEGION")
		must(t, err)
		if refilled || len(removed) != 0 {
			t.Fatalf("a refill of LEGION with no mark = %v, %v; want nothing removed and false", removed, refilled)
		}
		if got, want := held(tx), []string{"LEGION-1", "LEGION-2", "OTHER-1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the copy after a refill with no mark = %v, want %v", got, want)
		}

		_, err = tx.Exec(t.Context(), `insert into dispatch_issue_refills (project) values ('LEGION'), ('OTHER')`)
		must(t, err)
		removed, refilled, err = records.TakeDispatchRefill(t.Context(), tx, "LEGION")
		must(t, err)
		if want := []string{"LEGION-1", "LEGION-2"}; !refilled || !reflect.DeepEqual(removed, want) {
			t.Fatalf("a refill of LEGION with its mark = %v, %v; want %v and true", removed, refilled, want)
		}
		if got, want := held(tx), []string{"OTHER-1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the copy after LEGION's refill = %v, want %v", got, want)
		}
		rows, err := tx.Query(t.Context(), `select project || ' ' || (refilled_at is not null)::text from dispatch_issue_refills order by project`)
		must(t, err)
		marks, err := pgx.CollectRows(rows, pgx.RowTo[string])
		must(t, err)
		if want := []string{"LEGION true", "OTHER false"}; !reflect.DeepEqual(marks, want) {
			t.Fatalf("the marks, each with whether it is stamped = %q, want %q", marks, want)
		}

		put(tx, "LEGION-3")
		removed, refilled, err = records.TakeDispatchRefill(t.Context(), tx, "LEGION")
		must(t, err)
		if refilled || len(removed) != 0 {
			t.Fatalf("a second refill of LEGION = %v, %v; want nothing removed and false", removed, refilled)
		}
		if got, want := held(tx), []string{"LEGION-3", "OTHER-1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the copy after a second refill = %v, want %v", got, want)
		}
	})
}

// The copy has an index on its parent column. A parent's children, in every project, are read by
// it (DispatchChildren, `where parent = any($1)`), so the column alone serves that read; a walk up
// a chain reads by key, which the primary key serves.
func TestTheCopyIndexesItsParentColumn(t *testing.T) {
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		// Each index of the table, with whether its first key column is parent (pg_index.indkey[0];
		// an expression has no column there).
		type index struct {
			Definition string
			OnParent   bool
		}
		rows, err := tx.Query(t.Context(), `select pg_get_indexdef(i.indexrelid), coalesce(a.attname = 'parent', false)
			from pg_index i left join pg_attribute a on a.attrelid = i.indrelid and a.attnum = i.indkey[0]
			where i.indrelid = 'dispatch_issues'::regclass order by 1`)
		must(t, err)
		indexes, err := pgx.CollectRows(rows, pgx.RowToStructByPos[index])
		must(t, err)
		var onParent []string
		for _, found := range indexes {
			if found.OnParent {
				onParent = append(onParent, found.Definition)
			}
		}
		if len(onParent) != 1 {
			t.Fatalf("indexes of dispatch_issues whose first key column is parent = %q, want one; the table's indexes are %+v", onParent, indexes)
		}
		t.Logf("the copy's index on its parent column: %s", onParent[0])
	})
}
