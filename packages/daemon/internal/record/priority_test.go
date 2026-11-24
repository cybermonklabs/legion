package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// Dispatch serves an issue's priority as an integer from 0 (P0, the most urgent) to 3 (P3), or
// null for none, and refuses any other value (its INVALID_PRIORITY). The record keeps the four
// levels and none, and reads a value Dispatch never writes as none, saying it could not read it.
// None is the zero value, so a record built or stored before the daemon read priorities reads as
// an issue with none.
func TestDispatchPriorityReadsDispatchsFourLevelsAndNone(t *testing.T) {
	level := func(n int) *int { return &n }
	for _, tc := range []struct {
		name  string
		level *int
		want  Priority
		read  bool
		word  string
	}{
		{name: "null", level: nil, want: NoPriority, read: true, word: ""},
		{name: "0", level: level(0), want: P0, read: true, word: "P0"},
		{name: "1", level: level(1), want: P1, read: true, word: "P1"},
		{name: "2", level: level(2), want: P2, read: true, word: "P2"},
		{name: "3", level: level(3), want: P3, read: true, word: "P3"},
		{name: "4, which Dispatch never writes", level: level(4), want: NoPriority, read: false, word: ""},
		{name: "-1, which Dispatch never writes", level: level(-1), want: NoPriority, read: false, word: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, read := DispatchPriority(tc.level)
			if got != tc.want || read != tc.read {
				t.Fatalf("DispatchPriority = %v, %t; want %v, %t", got, read, tc.want, tc.read)
			}
			if got.String() != tc.word {
				t.Fatalf("the priority's word = %q, want %q", got.String(), tc.word)
			}
		})
	}
	if (Issue{}).Priority != NoPriority {
		t.Fatalf("a record built with no priority carries %v, want none", (Issue{}).Priority)
	}
}

// The priority that places an issue is its own, else its tree root's, else none: a child's own
// priority counts over its root's, a child with none takes its root's, and a root is its own tree
// root. The root's is read from the root's record at that moment, never copied onto the child, so
// a change of the root's priority in Dispatch moves its children with no event of theirs.
func TestAnIssueTakesItsOwnPriorityElseItsTreeRootsElseNone(t *testing.T) {
	root := linePartIssue("LEGION-10", "LEGION-10", "in_progress", phase.Admitted, "M")
	root.Priority = P1
	unranked := linePartIssue("LEGION-20", "LEGION-20", "in_progress", phase.Admitted, "N")
	for _, tc := range []struct {
		name  string
		issue Issue
		root  Issue
		want  Priority
	}{
		{name: "a P0 child of a P1 tree", issue: withPriority(linePartIssue("LEGION-11", "LEGION-10", "todo", phase.Admitted, "O"), P0), root: root, want: P0},
		{name: "a P3 child of a P1 tree", issue: withPriority(linePartIssue("LEGION-12", "LEGION-10", "todo", phase.Admitted, "P"), P3), root: root, want: P3},
		{name: "a child with none in a P1 tree", issue: linePartIssue("LEGION-13", "LEGION-10", "todo", phase.Admitted, "Q"), root: root, want: P1},
		{name: "the P1 root itself", issue: root, root: root, want: P1},
		{name: "a child with none in a tree whose root has none", issue: linePartIssue("LEGION-21", "LEGION-20", "todo", phase.Admitted, "R"), root: unranked, want: NoPriority},
		{name: "a child with none whose root has no record", issue: linePartIssue("LEGION-31", "LEGION-30", "todo", phase.Admitted, "S"), root: Issue{}, want: NoPriority},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PriorityOf(tc.issue, tc.root); got != tc.want {
				t.Fatalf("PriorityOf(%s under %s) = %v, want %v", tc.issue.Key, tc.issue.Tree, got, tc.want)
			}
		})
	}
}

// withPriority is issue with its own Dispatch priority set to priority.
func withPriority(issue Issue, priority Priority) Issue {
	issue.Priority = priority
	return issue
}

// The store keeps an issue's own priority as Dispatch's own level, 0 to 3, and none as null: a
// record written with P0 reads back P0 from a column holding 0, a change to P2 and then to none
// reads back each in turn, and a record written with none holds null. The column is the one an
// operator reads in psql beside Dispatch's own issue, so it holds Dispatch's numbers.
func TestTheRecordKeepsAnIssuesOwnPriorityAsDispatchsLevel(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	column := func(key string) *int16 {
		t.Helper()
		var level *int16
		must(t, st.Pool().QueryRow(ctx, `select priority from issues where key = $1`, key).Scan(&level))
		return level
	}
	read := func(key string) Priority {
		t.Helper()
		var got *Issue
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			got, err = records.Issue(ctx, tx, key)
			must(t, err)
		})
		if got == nil {
			t.Fatalf("%s has no record", key)
		}
		return got.Priority
	}

	urgent := issueFixture("LEGION-401")
	urgent.Priority = P0
	inTx(t, st, func(tx pgx.Tx) { must(t, records.PutIssue(ctx, tx, urgent)) })
	if got := read(urgent.Key); got != P0 {
		t.Fatalf("%s reads back at %v, want P0", urgent.Key, got)
	}
	if level := column(urgent.Key); level == nil || *level != 0 {
		t.Fatalf("the priority column of %s holds %v, want 0, Dispatch's level of P0", urgent.Key, level)
	}

	for _, change := range []Priority{P2, NoPriority} {
		urgent.Priority = change
		inTx(t, st, func(tx pgx.Tx) { must(t, records.PutIssue(ctx, tx, urgent)) })
		if got := read(urgent.Key); got != change {
			t.Fatalf("%s reads back at %v after the write of %v, want %v", urgent.Key, got, change, change)
		}
	}
	if level := column(urgent.Key); level != nil {
		t.Fatalf("the priority column of %s holds %d after its priority was taken off, want null", urgent.Key, *level)
	}

	plain := issueFixture("LEGION-402")
	inTx(t, st, func(tx pgx.Tx) { must(t, records.PutIssue(ctx, tx, plain)) })
	if got := read(plain.Key); got != NoPriority {
		t.Fatalf("%s, written with none, reads back at %v", plain.Key, got)
	}
	if level := column(plain.Key); level != nil {
		t.Fatalf("the priority column of %s, written with none, holds %d, want null", plain.Key, *level)
	}
}

// Level writes a priority back as Dispatch serves it, so the store's column and the wire hold the
// level DispatchPriority read: each of Dispatch's four levels, and null for none, makes the round
// trip.
func TestLevelIsTheLevelDispatchPriorityRead(t *testing.T) {
	for _, level := range []*int{nil, new(0), new(1), new(2), new(3)} {
		priority, read := DispatchPriority(level)
		if !read {
			t.Fatalf("DispatchPriority(%v) could not read a level Dispatch writes", level)
		}
		back := priority.Level()
		if (back == nil) != (level == nil) || (back != nil && *back != *level) {
			t.Errorf("%v.Level() = %v, want the level %v it was read from", priority, back, level)
		}
	}
}
