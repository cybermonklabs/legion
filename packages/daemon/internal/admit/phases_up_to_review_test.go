package admit

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// rowRuleCase is one issue of the reading below: a started root of a live tree of its own,
// standing in one phase of the workflow, with whether the fixture gives it a row and whether it
// holds one after the pass.
type rowRuleCase struct {
	key string
	// stands is the phase its record carries, and from the phase a hold came from, which is
	// the phase its work stands at (record.StandsAt).
	stands phase.Phase
	from   phase.Phase
	// status is the Dispatch status an issue in that phase shows.
	status string
	// seeded gives the issue one of the cap's rows before the pass, as the daemon's own start
	// of it did under the release this change replaces.
	seeded bool
	// holds is whether it holds a row once the pass has read it.
	holds bool
	why   string
}

// rowRuleRoot records one case as a root of a live tree of its own: its record, its Dispatch
// mirror row and its tree lifecycle, open under the workflow. A root of its own tree is what
// each case is, so no case can be answered by the tree of another, and none of them registers a
// design gate, so no first phase of any of them is in the waiting line.
func rowRuleRoot(t *testing.T, pool *pgxpool.Pool, row rowRuleCase, rank string) {
	t.Helper()
	stands := record.Issue{Key: row.key, Tree: row.key, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: row.key, Phase: row.stands, Generation: 1,
		Status: row.status, DispatchStatus: row.status, Rank: rank, HandedOver: true, LastDispatchSeq: 1}
	if row.from != "" {
		stands.Hold = &record.Hold{From: row.from}
	}
	if row.stands == phase.AwaitingPerson {
		stands.Awaiting = &record.Awaiting{Ask: "ask-" + row.key, Since: record.EventMark{Seq: 5}}
	}
	putIssue(t, pool, stands)
	seedMirror(t, pool, row.key, row.status)
	seedLifecycle(t, pool, row.key)
	if row.seeded {
		takeSlot(t, pool, row.key)
	}
}

// An issue holds one of the cap's rows from the moment the daemon starts it until its work is
// past its review: the phases up to the review keep a row, and accepting, retro, merging and
// awaiting_merge keep none (record.KeepsSlot). A hold keeps the row of the phase it came from,
// because that is where the work stands (record.StandsAt), and a hold the record names no phase
// for keeps one: the daemon knows only that the issue is held.
//
// The first reading gives one started issue each phase and runs one admission pass over them.
// Four of them carry a row the release before this one wrote, which is what this change finds
// in a running deployment, and the pass deletes those four rows and no others.
//
// Then the work of two of them comes back: the architect sends one from accepting and one from
// retro to implementing, each owing its implementer a start. With the cap full each waits among
// the issues whose work came back, which is ahead of an issue whose first phase has yet to
// start, and each takes a row again as the rows free.
func TestOnlyPhasesUpToReviewHoldASlot(t *testing.T) {
	pool := migratedPool(t)
	rows := []rowRuleCase{
		{key: "LEGION-410", stands: phase.Admitted, status: "in_progress", seeded: true, holds: true,
			why: "the daemon started the root's architect on this row and its design is being written"},
		{key: "LEGION-411", stands: phase.AwaitingPerson, status: "in_progress", seeded: true, holds: true,
			why: "a root waiting on a person keeps its place against the cap, and wins none back from the line when the answer comes"},
		{key: "LEGION-412", stands: phase.Planning, status: "in_progress", seeded: true, holds: true,
			why: "a worker is writing the change"},
		{key: "LEGION-413", stands: phase.Implementing, status: "in_progress", seeded: true, holds: true,
			why: "a worker is writing the change"},
		{key: "LEGION-414", stands: phase.Testing, status: "testing", seeded: true, holds: true,
			why: "a worker is writing the change"},
		{key: "LEGION-415", stands: phase.Reviewing, status: "needs_review", seeded: true, holds: true,
			why: "the review is the last phase the cap counts: its outcome can send the change back to be written again"},
		{key: "LEGION-416", stands: phase.Held, from: phase.Implementing, status: "in_progress", seeded: true, holds: true,
			why: "a hold leaves the work where it stood, and implementing keeps a row"},
		{key: "LEGION-417", stands: phase.Held, status: "in_progress", seeded: true, holds: true,
			why: "a hold the record names no phase for keeps a row: the daemon knows only that the issue is held"},
		{key: "LEGION-418", stands: phase.Held, from: phase.Merging, status: "in_progress",
			why: "a hold from merging stands past the issue's review, so no pass grants it a row"},
		{key: "LEGION-420", stands: phase.Accepting, status: "needs_review", seeded: true,
			why: "the architect reads a reviewed head here: the change is written, so the pass takes the row back"},
		{key: "LEGION-421", stands: phase.Retro, status: "retro", seeded: true,
			why: "the retro writes no more of the change, so the pass takes the row back"},
		{key: "LEGION-422", stands: phase.Merging, status: "retro", seeded: true,
			why: "the merger merges work already reviewed, so the pass takes the row back"},
		{key: "LEGION-430", stands: phase.Accepting, status: "needs_review",
			why: "an issue past its review that holds no row is offered none"},
		{key: "LEGION-431", stands: phase.Retro, status: "retro",
			why: "an issue past its review that holds no row is offered none"},
		{key: "LEGION-432", stands: phase.Merging, status: "retro",
			why: "an issue past its review that holds no row is offered none"},
	}
	var seeded, want []string
	for index, row := range rows {
		rowRuleRoot(t, pool, row, string(rune('a'+index)))
		if row.seeded {
			seeded = append(seeded, row.key)
		}
		if row.holds {
			want = append(want, row.key)
		}
	}
	// The cap is the rows the rule keeps, so it stands full from the first pass on: no row this
	// reading frees is handed to anything, and the waiting line below is held by the cap alone.
	admission := quiet(t, len(want))

	tick(t, pool, admission, "the first reading")

	assertRowHolders(t, pool, want...)
	for _, row := range rows {
		if held := holdsSlot(t, pool, row.key); held != row.holds {
			t.Fatalf("after the pass %s in %s holds one of the cap's rows = %t, want %t: %s", row.key, row.stands, held, row.holds, row.why)
		}
	}
	var deleted []string
	for _, key := range seeded {
		if !holdsSlot(t, pool, key) {
			deleted = append(deleted, key)
		}
	}
	if gone := []string{"LEGION-420", "LEGION-421", "LEGION-422"}; !slices.Equal(deleted, gone) {
		t.Fatalf("the pass deleted the rows of %v, want %v: the three phases past an issue's review and no others", deleted, gone)
	}

	// The work of two issues comes back to a phase that keeps a row, from accepting and from
	// retro, and a root that never ran waits behind them for its first phase.
	for _, key := range []string{"LEGION-420", "LEGION-421"} {
		back := issue(t, pool, key)
		back.Phase, back.Status, back.DispatchStatus = phase.Implementing, "in_progress", "in_progress"
		putIssue(t, pool, back)
		oweMemberStart(t, pool, key, key, phase.Implementing)
	}
	seedWaiting(t, pool, "LEGION-440", "z")

	tick(t, pool, admission, "the work that came back")

	assertWaiting(t, pool, []string{"LEGION-420", "LEGION-421", "LEGION-440"})
	assertRowHolders(t, pool, want...)

	// Each row the cap frees goes to the issue whose work came back, before the root whose
	// first phase has yet to start.
	closeTree(t, pool, admission, "planning-closed", "LEGION-412", "c")

	if !holdsSlot(t, pool, "LEGION-420") || holdsSlot(t, pool, "LEGION-440") {
		t.Fatalf("the rows after one freed = %#v, want the first row to have gone to LEGION-420, whose work came back", slots(t, pool))
	}

	closeTree(t, pool, admission, "testing-closed", "LEGION-414", "e")

	if !holdsSlot(t, pool, "LEGION-421") || holdsSlot(t, pool, "LEGION-440") {
		t.Fatalf("the rows after a second freed = %#v, want the next row to have gone to LEGION-421, whose work came back", slots(t, pool))
	}
	assertWaiting(t, pool, []string{"LEGION-440"})
}
