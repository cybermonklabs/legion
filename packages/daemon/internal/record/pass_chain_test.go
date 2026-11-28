package record

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
// of each key the walk started from; and the records themselves. It starts from every record, a
// done one too, where Store.ChainRows starts only from records not at done: a test that holds a
// done record reads a walk from it that the store would not make. It knows no project boundary
// (inProjectParentSQL): every row a caller copies is of one project, and the boundary's own tests
// read the store.
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
// the children-first cycle (childWaits) are two readings of the same chains (Chains), so a row of
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
	waits := childWaits(chains, []Issue{child})
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

// A chain stops at its project's boundary (R19 of dispatch://INFRA-3320), as Dispatch's own walk
// does: a Dispatch parent of another project is no issue above, whether the copy holds no row of
// it or holds that project's row of it, the issue at work in that project's daemon's records. So
// the issue right under such a parent is a root, its own top issue and gate root, and its own
// architect's: a notice its parent's architect would hear goes to its own (OwningIssue from the
// parent), and the other project's architect acts on nothing of it (ArchitectActsOn). An issue of
// the same project further down reads its chain up to the last link of its own project, with that
// link above it. ReadChains and ReadChainsOf make the one walk, so both answer so, and nothing of
// the issue's project stands under the other project's issue.
func TestAChainEndsAtAParentInAnotherProject(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const (
		unrowed = "LEGION-40" // under OTHER-40, which the copy holds no row of
		top     = "LEGION-42" // under OTHER-42, which the copy holds, at work in project OTHER
		under   = "LEGION-41" // under LEGION-42
		foreign = "OTHER-42"
	)
	inTx(t, st, func(tx pgx.Tx) {
		other := chainIssue(foreign, foreign, "", phase.Implementing, "in_progress")
		other.Project, other.HoldsGate = "OTHER", true
		for _, issue := range []Issue{
			chainIssue(unrowed, unrowed, "OTHER-40", phase.Implementing, "in_progress"),
			chainIssue(top, top, foreign, phase.Implementing, "in_progress"),
			chainIssue(under, top, top, phase.Implementing, "in_progress"),
			other,
		} {
			must(t, records.PutIssue(ctx, tx, issue))
			putCopy(t, ctx, tx, issue)
		}
	})

	everyArchitectRuns := func(claim.Token) bool { return true }
	inTx(t, st, func(tx pgx.Tx) {
		pass, err := ReadChains(ctx, tx, records, "LEGION", nil)
		must(t, err)
		of, err := ReadChainsOf(ctx, tx, records, "LEGION", []string{unrowed, under, top})
		must(t, err)
		for _, read := range []struct {
			name   string
			chains *Chains
		}{{"ReadChains", pass}, {"ReadChainsOf", of}} {
			for _, tc := range []struct {
				key, top string
				links    []string
			}{
				{unrowed, unrowed, nil},
				{top, top, nil},
				{under, top, []string{top}},
			} {
				chains := read.chains
				if chain := chains.Chain(tc.key); !chain.Known() || !slices.Equal(chain.Links, tc.links) {
					t.Errorf("%s: the chain of %s = %v, %q at %s; want %v, known: a parent of another project ends it",
						read.name, tc.key, chain.Links, chain.Cause, chain.At, tc.links)
				}
				if root, top, gate := chains.IsRoot(tc.key), chains.TopIssue(tc.key), chains.GateRoot(tc.key); root != (tc.key == tc.top) ||
					top != tc.top || gate != tc.top {
					t.Errorf("%s: %s a root %t, its top issue %s and its gate root %s; want a root %t, and %s for both",
						read.name, tc.key, root, top, gate, tc.key == tc.top, tc.top)
				}
				if owner := OwningIssue("legion", chains, tc.key, true, everyArchitectRuns); owner != tc.top {
					t.Errorf("%s: the owner walk from above %s ends at %q, want %s: no issue of another project is above it", read.name, tc.key, owner, tc.top)
				}
				if ArchitectActsOn("legion", chains, architectToken(t, foreign), tc.key) || !ArchitectActsOn("legion", chains, architectToken(t, tc.top), tc.key) {
					t.Errorf("%s: on %s %s's architect acts %t and %s's %t; want only %s's", read.name, tc.key,
						foreign, ArchitectActsOn("legion", chains, architectToken(t, foreign), tc.key), tc.top, ArchitectActsOn("legion", chains, architectToken(t, tc.top), tc.key), tc.top)
				}
			}
		}
		if beneath := pass.Under(foreign); len(beneath) != 0 {
			t.Errorf("under %s = %v, want nothing of project LEGION", foreign, beneath)
		}
	})
}

// A child of another project is no child of the wait (R9 of dispatch://INFRA-3320), as a parent
// of another project is no issue above it: the copy's children of a parent are those of the
// parent's own project (Chains.Children, Store.HasDispatchChildren), so an open child of another
// project holds none of the parent's phase starts (OpenChildWaits), while an open child of the
// parent's own project, the control, does.
func TestAChildInAnotherProjectIsNoChildOfTheWait(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	parent := chainIssue("LEGION-50", "LEGION-50", "", phase.Implementing, "in_progress")
	foreign := chainIssue("OTHER-51", "OTHER-51", parent.Key, phase.Implementing, "in_progress")
	foreign.Project = "OTHER"
	control := chainIssue("LEGION-52", "LEGION-52", "", phase.Implementing, "in_progress")
	own := chainIssue("LEGION-53", control.Key, control.Key, phase.Implementing, "in_progress")
	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range []Issue{parent, foreign, control, own} {
			must(t, records.PutIssue(ctx, tx, issue))
			putCopy(t, ctx, tx, issue)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		chains, err := ReadChains(ctx, tx, records, "LEGION", nil)
		must(t, err)
		if children := chains.Children(parent.Key); len(children) != 0 {
			t.Errorf("the children of %s = %v, want none: %s is of another project", parent.Key, children, foreign.Key)
		}
		for key, want := range map[string]bool{parent.Key: false, control.Key: true} {
			has, err := records.HasDispatchChildren(ctx, tx, "LEGION", key)
			must(t, err)
			if has != want {
				t.Errorf("whether %s has a Dispatch child = %t, want %t", key, has, want)
			}
		}
		waits, err := OpenChildWaits(ctx, tx, records, "LEGION", []Issue{parent, control})
		must(t, err)
		if wait, held := waits[parent.Key]; held {
			t.Errorf("%s waits for %+v, want no wait: its one open child is of another project", parent.Key, wait.Open)
		}
		if open := waits[control.Key].Open; len(open) != 1 || open[0].Key != own.Key {
			t.Errorf("%s waits for %+v, want its open child %s", control.Key, open, own.Key)
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

// One pass reads its parent chain once (LineFacts.Chains, ReadChains), and every walk of the pass
// reads that one read: ReadLineFacts' own walks — the cycle of each child (childWaits), the child
// left out of each parent's open children (withoutHeldUnderAccepted) and the issues beneath the
// root held before its retro (retroHoldsOf) — and the line (Waiting) and the held list (HeldList)
// judged from its facts, as the admission pass, the state and the controller's tick read them
// (blockers.JudgeLine, admit's tickRows, projection's Project). A started root and a root held
// before its retro, each with a child filed under it that the daemon does not record, give
// ReadLineFacts those walks. An admitted child under a done parent beneath an issue in merging
// gives the line its walk past that parent, which is none of the pass's own records, where the
// hold keeps the child out of the line (HeldUnderAccepted) and where the ladder names the hold
// (HeldReasonOf). The held list takes the chain for its own ladder, which a pass gives no issue to
// here: the line leaves a row for every issue it does not place, and the held list keeps that row.
//
// Nothing of a read shows outside the pass but its cost, so the reads are counted. The pass sends
// six statements whatever its walks ask: the chain read's three (the rows of the copy its chains
// reach, the children of each issue they start from, and the records of both), one for the gates,
// one for the starts the project owes and one for the gates that rest on the held root
// (RestingRootsOn), which names no root and so reads no record. A walk that read the chain again
// would send three more. And no walk reads a row of the copy twice (Chains.steps): the pass's own
// walks read each row once, and the line and the held list walk what those walks kept.
func TestAPassBuildsItsParentChainOnce(t *testing.T) {
	const (
		started      = "LEGION-960" // a root in implementing, filed under a done issue
		held         = "LEGION-961" // a root held before its retro
		filedUnder   = "LEGION-962" // the done issue the started root was filed under
		startedChild = "LEGION-963" // filed under the started root, never released
		heldChild    = "LEGION-964" // filed under the held root, never released
		acceptedRoot = "LEGION-965" // the root of a running tree, at admitted
		merging      = "LEGION-966" // an issue under that root in merging, so accepted
		doneParent   = "LEGION-967" // done under the merging issue: none of the pass's records
		heldAtAdmit  = "LEGION-968" // admitted under the done parent, held under the merging issue
	)
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	recorded := []Issue{
		chainIssue(started, started, filedUnder, phase.Implementing, "in_progress"),
		planHeld(held, held, nil, phase.Retro, HoldForChildren),
		chainIssue(acceptedRoot, acceptedRoot, "", phase.Admitted, "in_progress"),
		chainIssue(merging, acceptedRoot, acceptedRoot, phase.Merging, "needs_review"),
		chainIssue(heldAtAdmit, acceptedRoot, doneParent, phase.Admitted, "todo"),
		chainIssue(filedUnder, filedUnder, "", phase.Done, DoneStatus),
		chainIssue(doneParent, acceptedRoot, merging, phase.Done, DoneStatus),
	}
	// The pass's own records are the live ones, as a pass reads them: neither done issue is among
	// them.
	issues := recorded[:5]
	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range recorded {
			must(t, records.PutIssue(ctx, tx, issue))
			putCopy(t, ctx, tx, issue)
		}
		for _, reading := range []DispatchReading{
			{Key: startedChild, Project: "LEGION", Seq: 1, Status: Carry("backlog"), Parent: Carry(started), Blockers: Carry[[]string](nil)},
			{Key: heldChild, Project: "LEGION", Seq: 1, Status: Carry("backlog"), Parent: Carry(held), Blockers: Carry[[]string](nil)},
		} {
			must(t, records.PutDispatchIssue(ctx, tx, reading))
		}
		// The accepted root's design is approved, so its admitted child reaches the hold.
		approved := 1
		must(t, records.PutGate(ctx, tx, DesignGate{Issue: acceptedRoot, ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: &approved}))
	})

	inTx(t, st, func(tx pgx.Tx) {
		counted := &countedTx{Tx: tx}
		facts, err := ReadLineFacts(ctx, counted, records, "LEGION", issues, nil)
		must(t, err)
		walked := facts.Chains.steps
		// The line is handed the children-first waits as blockers.JudgeLine hands them on.
		facts.ChildWaits = ChildWaitsOf(facts)
		line, dropped := Waiting(issues, nil, facts)
		heldRows := HeldList(issues, nil, line, dropped, facts)
		if len(facts.StartedParentWaits[started].Open) != 1 || len(facts.RetroHolds[held].Open) != 1 || len(line)+len(heldRows) != len(issues) {
			t.Fatalf("the pass holds the open children %+v of %s and the holds %+v of %s, and its line and held list place %d of its %d issues; want %s and %s open, and every issue placed: without the pass's walks this test proves nothing",
				facts.StartedParentWaits[started].Open, started, facts.RetroHolds[held], held, len(line)+len(heldRows), len(issues), startedChild, heldChild)
		}
		if links := facts.Chains.Chain(heldAtAdmit).Links; !slices.Equal(links, []string{doneParent, merging, acceptedRoot}) {
			t.Fatalf("the chain of %s = %v, want %s, %s and %s: without the walk past the done parent this test proves nothing", heldAtAdmit, links, doneParent, merging, acceptedRoot)
		}
		want := HeldIssue{Issue: heldAtAdmit, Reason: HeldAcceptedAncestor, HeldBy: []string{merging}}
		first := slices.IndexFunc(dropped, func(row HeldIssue) bool { return row.Issue == heldAtAdmit })
		named := slices.IndexFunc(heldRows, func(row HeldIssue) bool { return row.Issue == heldAtAdmit })
		if inLine(line, heldAtAdmit) || first < 0 || !reflect.DeepEqual(dropped[first], want) || named < 0 || !reflect.DeepEqual(heldRows[named], want) {
			t.Fatalf("the line holds %s: in the line %v, its rows %+v, the held list %+v; want it out of the line and held as %+v in both: without the line's walk past %s this test proves nothing",
				heldAtAdmit, inLine(line, heldAtAdmit), dropped, heldRows, want, doneParent)
		}

		if counted.statements != 6 {
			t.Fatalf("the pass, its line and its held list sent %d statements, want 6: the chain read's three once, the gates, the owed starts and the gates resting on %s; every walk reads the pass's one chain (LineFacts.Chains)",
				counted.statements, held)
		}
		if rows := len(facts.Chains.rows); walked > rows {
			t.Fatalf("the pass's own walks read %d rows of the copy, of the %d its chain read holds: a walk read a row a walk before it kept", walked, rows)
		}
		if facts.Chains.steps != walked {
			t.Fatalf("the line and the held list read %d rows of the copy again, want none: they walk the chain the pass's own walks kept", facts.Chains.steps-walked)
		}
	})
}
