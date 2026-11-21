package record

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// copyOf is the copy's row of each of issues as a reading of Dispatch writes it: its project, the
// parent its record names and the status Dispatch last showed it in.
func copyOf(issues ...Issue) []DispatchIssueRow {
	rows := make([]DispatchIssueRow, len(issues))
	for i, issue := range issues {
		status := issue.DispatchStatus
		if status == "" {
			status = issue.Status
		}
		parent := ""
		if issue.Parent != nil {
			parent = *issue.Parent
		}
		rows[i] = DispatchIssueRow{Key: issue.Key, Project: issue.Project, Status: status, Parent: parent}
	}
	return rows
}

// chainsOf is the chains a read of copied and records makes (ReadChains), walked in memory as
// Store.ChainRows walks the copy: from each record's key and each todo row of the copy, up to
// MaxChainLinks+1 rows from each, a key already on the walk's path ending it; the copy's children
// of each key the walk started from; and the records themselves.
func chainsOf(records []Issue, copied []DispatchIssueRow) *Chains {
	byKey := make(map[string]DispatchIssueRow, len(copied))
	for _, row := range copied {
		byKey[row.Key] = row
	}
	var starts []string
	for _, issue := range records {
		starts = append(starts, issue.Key)
	}
	for _, row := range copied {
		if row.Status == "todo" {
			starts = append(starts, row.Key)
		}
	}
	slices.Sort(starts)
	starts = slices.Compact(starts)
	at := map[string]int{}
	var rows []ChainRow
	for _, start := range starts {
		path := []string{start}
		for key, depth := start, 0; ; depth++ {
			row, present := byKey[key]
			if i, read := at[key]; !read {
				at[key] = len(rows)
				rows = append(rows, ChainRow{Key: key, Depth: depth, Parent: row.Parent, Status: row.Status, Present: present})
			} else if depth < rows[i].Depth {
				rows[i].Depth = depth
			}
			if !present || row.Parent == "" || depth >= MaxChainLinks+1 || slices.Contains(path, row.Parent) {
				break
			}
			key = row.Parent
			path = append(path, key)
		}
	}
	children := map[string][]DispatchIssueRow{}
	for _, start := range starts {
		for _, row := range copied {
			if row.Parent == start {
				children[start] = append(children[start], row)
			}
		}
		slices.SortFunc(children[start], func(a, b DispatchIssueRow) int { return strings.Compare(a.Key, b.Key) })
	}
	return newChains(rows, children, records)
}

// putCopy writes the copy's row of issue as a reading of Dispatch does (Store.PutDispatchIssue):
// the parent its record names and the status Dispatch last showed it in, at a sequence above every
// value the copy holds of it, so the row reads as the record does whatever was written before.
func putCopy(t *testing.T, ctx context.Context, tx pgx.Tx, issue Issue) {
	t.Helper()
	row := copyOf(issue)[0]
	var seq int64
	if err := tx.QueryRow(ctx, `select coalesce(max(greatest(status_seq, parent_seq)), 0) from dispatch_issues where key = $1`,
		issue.Key).Scan(&seq); err != nil {
		t.Fatalf("read the copy's sequences of %s: %v", issue.Key, err)
	}
	must(t, NewStore().PutDispatchIssue(ctx, tx, DispatchReading{Key: row.Key, Project: row.Project, Seq: seq + 1,
		Status: Carry(row.Status), Parent: Carry(row.Parent)}))
}

// One pass walks one chain: the hold under an accepted ancestor (Waiting's HeldUnderAccepted) and
// the children-first cycle (ChildWaits) are two readings of the same chains (Chains), so a row of
// the copy one of them read is read by the other from what the first kept. The chain is the
// copy's, so it crosses an issue already done that the pass's own records leave out: the issue
// under a done parent is held by the merging issue above that parent, and its child's blocked_by
// link to that same issue is named a cycle.
func TestOnePassWalksOneChain(t *testing.T) {
	root := chainIssue("LEGION-900", "LEGION-900", "", phase.Admitted, "in_progress")
	merging := chainIssue("LEGION-901", "LEGION-900", "LEGION-900", phase.Merging, "needs_review")
	doneParent := chainIssue("LEGION-902", "LEGION-900", "LEGION-901", phase.Done, "done")
	child := chainIssue("LEGION-903", "LEGION-900", "LEGION-902", phase.Admitted, "todo")
	grandchild := DispatchIssueRow{Key: "LEGION-904", Project: "LEGION", Status: "backlog", Parent: child.Key,
		Blockers: []string{merging.Key}}
	chains := chainsOf([]Issue{root, merging, doneParent, child}, append(copyOf(root, merging, doneParent, child), grandchild))

	if ancestor := AcceptedAncestor(chains, child.Key); ancestor != merging.Key {
		t.Fatalf("the accepted ancestor of %s = %q, want %s past the done parent %s", child.Key, ancestor, merging.Key, doneParent.Key)
	}
	read := chains.steps
	waits := ChildWaits(chains, []Issue{child})
	if open := waits[child.Key].Open; len(open) != 1 || open[0].Key != grandchild.Key || open[0].Cycle != merging.Key {
		t.Fatalf("the open children of %s = %+v, want %s naming %s as its cycle", child.Key, open, grandchild.Key, merging.Key)
	}
	if chains.steps != read {
		t.Fatalf("the cycle walk read %d rows of the copy again, want none: the hold's walk kept the chain", chains.steps-read)
	}
}

// A chain is unknown where the copy cannot show its top (R16 of dispatch://INFRA-3320): a link
// the copy holds no row of, the issue's own row among them; a loop of rows the copy does hold,
// which two late moves leave where the copy has read the second and not yet the first; and a
// chain longer than Dispatch's 32 links, which a stale row leaves. An unknown chain names its
// cause and the link it stopped at, and its issue is no root, so nothing is made a root on it. A
// chain of exactly 32 links is the bound's other side and is known, with its top issue in work
// above an issue 32 links below it.
//
// One read is three queries whatever the number of chains, and a pass that asks every question
// of every issue of a tree reads each row of that tree's chain once: the chain of an issue whose
// parent's chain is already read joins it rather than reading it again. A key the read neither
// started from nor reached is unread until a caller names it.
func TestALoopAndAChainOver32LinksAreUnknown(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	// copied writes the copy's row of key as a reading of Dispatch does.
	copied := func(tx pgx.Tx, project, key, parent, status string, seq int64) {
		t.Helper()
		must(t, records.PutDispatchIssue(ctx, tx, DispatchReading{Key: key, Project: project, Seq: seq,
			Status: Carry(status), Parent: Carry(parent), Blockers: Carry[[]string](nil)}))
	}
	// recorded is the daemon's record of key, at work under parent.
	recorded := func(tx pgx.Tx, project, key, parent string, stands phase.Phase, status string) {
		t.Helper()
		issue := chainIssue(key, key, parent, stands, status)
		issue.Project = project
		must(t, records.PutIssue(ctx, tx, issue))
	}
	links := func(from, to int) []string {
		keys := make([]string, 0, to-from+1)
		for n := from; n <= to; n++ {
			keys = append(keys, fmt.Sprintf("LEGION-%d", n))
		}
		return keys
	}
	inTx(t, st, func(tx pgx.Tx) {
		recorded(tx, "LEGION", "LEGION-10", "LEGION-11", phase.Implementing, "in_progress")
		copied(tx, "LEGION", "LEGION-10", "LEGION-11", "in_progress", 1)
		copied(tx, "LEGION", "LEGION-11", "LEGION-12", "backlog", 1)
		recorded(tx, "LEGION", "LEGION-15", "", phase.Implementing, "in_progress")

		// LEGION-22 stood under LEGION-21; then it moved to the top and LEGION-21 moved under it.
		// The copy has read the second move and not yet the first.
		recorded(tx, "LEGION", "LEGION-20", "LEGION-21", phase.Implementing, "in_progress")
		copied(tx, "LEGION", "LEGION-20", "LEGION-21", "in_progress", 1)
		copied(tx, "LEGION", "LEGION-21", "LEGION-22", "in_progress", 7)
		copied(tx, "LEGION", "LEGION-22", "LEGION-21", "in_progress", 5)

		// Two chains with a recorded issue at work at each end: LEGION-100 stands 33 links below
		// LEGION-133, and LEGION-200 32 below LEGION-232. Only the ends are recorded.
		for _, chain := range []struct{ bottom, top int }{{100, 133}, {200, 232}} {
			bottom, top := fmt.Sprintf("LEGION-%d", chain.bottom), fmt.Sprintf("LEGION-%d", chain.top)
			recorded(tx, "LEGION", bottom, fmt.Sprintf("LEGION-%d", chain.bottom+1), phase.Implementing, "in_progress")
			recorded(tx, "LEGION", top, "", phase.Implementing, "in_progress")
			for n := chain.bottom; n < chain.top; n++ {
				copied(tx, "LEGION", fmt.Sprintf("LEGION-%d", n), fmt.Sprintf("LEGION-%d", n+1), "in_progress", 1)
			}
			copied(tx, "LEGION", top, "", "in_progress", 1)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		chains, err := ReadChains(ctx, tx, records, "LEGION", nil)
		must(t, err)
		for _, tc := range []struct {
			name string
			key  string
			want Chain
		}{
			{"a link the copy holds no row of", "LEGION-10", Chain{Links: links(11, 11), Cause: ChainMissingRow, At: "LEGION-12"}},
			{"an issue the copy holds no row of", "LEGION-15", Chain{Cause: ChainMissingRow, At: "LEGION-15"}},
			{"a loop of rows the copy holds", "LEGION-20", Chain{Links: links(21, 22), Cause: ChainRepeats, At: "LEGION-21"}},
			{"a link inside that loop", "LEGION-22", Chain{Links: links(21, 21), Cause: ChainRepeats, At: "LEGION-22"}},
			{"33 links", "LEGION-100", Chain{Links: links(101, 132), Cause: ChainTooLong, At: "LEGION-133"}},
			{"32 links", "LEGION-200", Chain{Links: links(201, 232)}},
		} {
			got := chains.Chain(tc.key)
			if !slices.Equal(got.Links, tc.want.Links) || got.Cause != tc.want.Cause || got.At != tc.want.At {
				t.Errorf("%s: the chain of %s = %v, %q at %s; want %v, %q at %s", tc.name, tc.key,
					got.Links, got.Cause, got.At, tc.want.Links, tc.want.Cause, tc.want.At)
			}
			if got.Known() != tc.want.Known() {
				t.Errorf("%s: the chain of %s reads known %t, want %t", tc.name, tc.key, got.Known(), tc.want.Known())
			}
			// No issue above any of these is in work but LEGION-232, so only the known chain's
			// issue stands under an issue in work; every other one would be a root on a chain
			// read as known.
			if chains.IsRoot(tc.key) {
				t.Errorf("%s: %s reads a root, want none: an unknown chain makes no root, and a known one under an issue in work is none", tc.name, tc.key)
			}
		}
		// The issue 32 links below a top in work has that top above it, the 31 issues between
		// them in no one's work, and the top as its gate root; the top over 32 links away is on
		// no chain that is known, so it stands above nothing and is no issue's gate root.
		if above, top, gate := chains.Above("LEGION-200"), chains.TopIssue("LEGION-200"), chains.GateRoot("LEGION-200"); !slices.Equal(above, []string{"LEGION-232"}) ||
			top != "LEGION-232" || gate != "LEGION-232" {
			t.Errorf("above LEGION-200 = %v, its top %s and its gate root %s; want LEGION-232 for each", above, top, gate)
		}
		if above, gate := chains.Above("LEGION-100"), chains.GateRoot("LEGION-100"); len(above) != 0 || gate != "" {
			t.Errorf("above LEGION-100 = %v and its gate root %q; want none: its chain stops before LEGION-133", above, gate)
		}
		if !chains.IsRoot("LEGION-133") || !slices.Equal(chains.Under("LEGION-232"), []string{"LEGION-200"}) || len(chains.Under("LEGION-133")) != 0 {
			t.Errorf("LEGION-133 a root %t, under LEGION-232 %v, under LEGION-133 %v; want a root, LEGION-200 and none",
				chains.IsRoot("LEGION-133"), chains.Under("LEGION-232"), chains.Under("LEGION-133"))
		}
	})

	// One tree in a project of its own: a root, an issue under it, one under that, and two todo
	// children the daemon does not record yet, which the read starts from as every todo row.
	tree := []string{"TREE-1", "TREE-2", "TREE-3", "TREE-4", "TREE-5"}
	inTx(t, st, func(tx pgx.Tx) {
		recorded(tx, "TREE", "TREE-1", "", phase.Implementing, "in_progress")
		copied(tx, "TREE", "TREE-1", "", "in_progress", 1)
		recorded(tx, "TREE", "TREE-2", "TREE-1", phase.Implementing, "in_progress")
		copied(tx, "TREE", "TREE-2", "TREE-1", "in_progress", 1)
		recorded(tx, "TREE", "TREE-3", "TREE-2", phase.Admitted, "todo")
		copied(tx, "TREE", "TREE-3", "TREE-2", "todo", 1)
		copied(tx, "TREE", "TREE-4", "TREE-3", "todo", 1)
		copied(tx, "TREE", "TREE-5", "TREE-3", "todo", 1)
		copied(tx, "TREE", "TREE-9", "TREE-1", "backlog", 1)
	})
	inTx(t, st, func(tx pgx.Tx) {
		counted := &countedTx{Tx: tx}
		chains, err := ReadChains(ctx, counted, records, "TREE", nil)
		must(t, err)
		if counted.statements != 3 {
			t.Errorf("one read of the chains sent %d statements, want 3: the rows the chains reach, the children of each issue they start from and the records of both", counted.statements)
		}
		for _, key := range tree {
			chains.Chain(key)
			chains.Above(key)
			chains.TopIssue(key)
			chains.GateRoot(key)
			chains.StateTree(key)
			chains.Under(key)
		}
		if chains.steps != len(tree) {
			t.Errorf("every question of every issue of the tree read %d rows, want %d: each row of a shared chain is read once", chains.steps, len(tree))
		}
		if above, gate, under := chains.Above("TREE-4"), chains.GateRoot("TREE-4"), chains.Under("TREE-1"); !slices.Equal(above, []string{"TREE-3", "TREE-2", "TREE-1"}) ||
			gate != "TREE-1" || !slices.Equal(under, []string{"TREE-2", "TREE-3"}) {
			t.Errorf("above TREE-4 = %v with gate root %s, under TREE-1 %v; want TREE-3, TREE-2 and TREE-1, TREE-1, and TREE-2 and TREE-3 in work", above, gate, under)
		}
		if children := chains.Children("TREE-3"); len(children) != 2 || children[0].Key != "TREE-4" || children[1].Key != "TREE-5" {
			t.Errorf("the children of TREE-3 = %v, want TREE-4 and TREE-5 from the copy", children)
		}
		if chain := chains.Chain("TREE-9"); chain.Cause != ChainUnread || chain.At != "TREE-9" {
			t.Errorf("the chain of TREE-9, which the read neither started from nor reached = %v, %q at %s; want unread at itself", chain.Links, chain.Cause, chain.At)
		}

		named, err := ReadChains(ctx, tx, records, "TREE", []string{"TREE-9"})
		must(t, err)
		if chain := named.Chain("TREE-9"); !chain.Known() || !slices.Equal(chain.Links, []string{"TREE-1"}) {
			t.Errorf("the chain of TREE-9 named to the read = %v, %q at %s; want TREE-1, known", chain.Links, chain.Cause, chain.At)
		}
	})
}

// countedTx is tx with every statement sent through it counted.
type countedTx struct {
	pgx.Tx
	statements int
}

func (tx *countedTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx.statements++
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *countedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.statements++
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *countedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx.statements++
	return tx.Tx.QueryRow(ctx, sql, args...)
}
