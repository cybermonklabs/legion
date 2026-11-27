package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The issues of the tests of a re-parent seed's wait for its parent's chain (the architect's
// ruling, dispatch://INFRA-4730/comment/000d0b0f-743a-4130-8ba2-3c95b97d1ff7: the telling stays
// owed until the chain is known): CAPTURE-300, a root at work, whose architect stands above
// CAPTURE-500 once its move is known; CAPTURE-208, a Dispatch issue the daemon records nothing
// of, under which CAPTURE-500 stands at first; CAPTURE-500, recorded under CAPTURE-208; and
// CAPTURE-700, of which the copy holds no row, which Dispatch serves on the backlog under
// CAPTURE-300.
const (
	seedWaitAbove  = "CAPTURE-300"
	seedWaitBefore = "CAPTURE-208"
	seedWaitIssue  = "CAPTURE-500"
	seedWaitParent = "CAPTURE-700"
)

// A person moves CAPTURE-500 under CAPTURE-700 while the daemon's merge close of it is on its way,
// and the close's answer, which the outbox applies as the daemon's own write (applyEntries),
// carries the new parent and done together. The engine queues the seed of the move, which waits
// on CAPTURE-700's chain, before the close lands (closeLanded), which takes the issue to phase
// done in the same fact: no admission pass reads a done record, so nothing notes CAPTURE-500 or
// CAPTURE-700 for R16's re-read. The seed's wait has the re-read read CAPTURE-700 and the links
// above it from Dispatch, and once the copy holds them the seed tells the architect above the
// issue now, CAPTURE-300's, once.
func TestAReparentSeedOfAnIssueItsCloseTookToDoneHasItsParentRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	client := &tickDispatch{}
	w := seedWaitRuntime(t, pool, client)
	seedWaitIssues(t, pool, client, record.Issue{Phase: phase.Merging, Status: "retro", DispatchStatus: "retro",
		PendingClose: &record.PendingClose{Generation: 1, Kind: "merge", Reason: "the pull request merged"}})

	closed := dispatch.Issue{Key: seedWaitIssue, Project: "CAPTURE", Title: seedWaitIssue, Status: "done", Parent: new(seedWaitParent),
		Rank: "B", LastSeq: 3, ResolvedRepo: "acme/widgets"}
	client.serve(closed)
	if err := w.outbox.applyEntries(ctx, record.OutboxRow{Issue: seedWaitIssue}, seedWaitIssue,
		[]dispatch.StatusChangeEntry{{Key: seedWaitIssue, Status: "done", Parent: new(seedWaitParent), PreviousLastSeq: 2, Issue: &closed}}); err != nil {
		t.Fatalf("apply the close's answer: %v", err)
	}
	// The case's premise: the close landed with the move in one fact, and no pass noted anything.
	if got := readIssue(t, pool, seedWaitIssue); got == nil || got.Phase != phase.Done || got.Status != "done" || got.ToldParent != seedWaitParent {
		t.Fatalf("premise: %s after the close's answer is %+v, want phase done, status done and told_parent %s", seedWaitIssue, got, seedWaitParent)
	}
	var live []record.Issue
	if err := store.TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		live, err = w.records.LiveIssues(ctx, tx, "CAPTURE")
		return err
	}); err != nil {
		t.Fatalf("read the live issues: %v", err)
	}
	if slices.ContainsFunc(live, func(issue record.Issue) bool { return issue.Key == seedWaitIssue }) {
		t.Fatalf("premise: %s, at phase done, is among the live issues an admission pass reads", seedWaitIssue)
	}
	if noted := w.admission.UnknownChains(); len(noted) != 0 {
		t.Fatalf("premise: the pass of the close's answer noted %v for the re-read, want nothing", noted)
	}

	seedWaitTells(t, w, client, pool, []string{seedWaitParent, seedWaitAbove})
}

// A person moves CAPTURE-500, at work, under CAPTURE-700, whose row the copy lacks: the seed of
// the move waits on CAPTURE-700's chain. Before R16's re-read turns, the person stops the issue
// (backlog) and then, stopped, moves it under CAPTURE-300: out of Legion's work that move queues
// no seed and leaves told_parent at CAPTURE-700, so the seed still waits on CAPTURE-700, while the
// issue's own chain, which every pass and the re-read of the issue read, now runs through
// CAPTURE-300 and is known. The seed's wait has the re-read read CAPTURE-700 all the same, and the
// seed tells CAPTURE-300's architect, above the issue through CAPTURE-700, once: in the running
// daemon, whose re-read still holds the passes' note of the issue and reads the issue's chain
// under its later parent, and in a new process, whose re-read holds no note at all.
func TestAReparentSeedOfAnIssueMovedAgainOutOfWorkHasItsParentRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		restart bool
		asked   []string
	}{
		{name: "the running daemon", asked: []string{seedWaitIssue, seedWaitAbove, seedWaitParent, seedWaitAbove}},
		{name: "a new process", restart: true, asked: []string{seedWaitParent, seedWaitAbove}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := isolatedOutboxPool(t)
			client := &tickDispatch{}
			w := seedWaitRuntime(t, pool, client)
			seedWaitIssues(t, pool, client, record.Issue{Phase: phase.Implementing, Status: "in_progress", DispatchStatus: "in_progress"})

			for _, move := range []struct {
				seq            int64
				status, parent string
			}{{2, "in_progress", seedWaitParent}, {3, "backlog", seedWaitParent}, {4, "backlog", seedWaitAbove}} {
				fact := intake.DispatchIssue{Key: seedWaitIssue, ResolvedRepo: "acme/widgets", Seq: move.seq, Type: "issue.updated",
					Status: move.status, Title: seedWaitIssue, Parent: move.parent, Rank: "B"}
				if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", fmt.Sprintf("move-%d", move.seq), fact, w.handlers...); err != nil {
					t.Fatalf("apply the person's %s under %s at %d: %v", move.status, move.parent, move.seq, err)
				}
			}
			client.serve(dispatch.Issue{Key: seedWaitIssue, Project: "CAPTURE", Title: seedWaitIssue, Status: "backlog", Parent: new(seedWaitAbove),
				Rank: "B", LastSeq: 4, ResolvedRepo: "acme/widgets"})
			// The case's premise: the seed still waits on CAPTURE-700, and the issue's own chain
			// is known through CAPTURE-300.
			got := readIssue(t, pool, seedWaitIssue)
			if got == nil || got.ToldParent != seedWaitParent || record.InLegionsWork(*got) || got.Parent == nil || *got.Parent != seedWaitAbove {
				t.Fatalf("premise: %s after the moves is %+v, want it out of Legion's work under %s with told_parent %s", seedWaitIssue, got, seedWaitAbove, seedWaitParent)
			}
			if !rereadKnown(t, pool, seedWaitIssue) || rereadKnown(t, pool, seedWaitParent) {
				t.Fatalf("premise: the chain of %s known %t and of %s known %t, want the first known and the second not",
					seedWaitIssue, rereadKnown(t, pool, seedWaitIssue), seedWaitParent, rereadKnown(t, pool, seedWaitParent))
			}

			if tc.restart {
				w = seedWaitRuntime(t, pool, client)
			}
			seedWaitTells(t, w, client, pool, tc.asked)
		})
	}
}

// seedWaitRuntime is the re-read's runtime over pool (tickRuntime) with the outbox the boot
// attaches to it (attach): the seed's executor and R16's re-read, each as the daemon runs them.
func seedWaitRuntime(t *testing.T, pool *store.Pool, client *tickDispatch) *workflowRuntime {
	t.Helper()
	w := tickRuntime(t, pool, client)
	sup, _ := newOutboxSupervisor(t, "capture", t.TempDir())
	w.attach(&supervision{supervisor: sup, log: quietLogger()})
	return w
}

// seedWaitIssues records CAPTURE-300 at work and CAPTURE-500 under CAPTURE-208 at the phase and
// status issue names, writes the copy's rows of all three, and has Dispatch serve CAPTURE-300 and
// CAPTURE-700, which the copy lacks.
func seedWaitIssues(t *testing.T, pool *store.Pool, client *tickDispatch, issue record.Issue) {
	t.Helper()
	seedRereadIssue(t, pool, seedWaitAbove)
	seedRereadRow(t, pool, seedWaitAbove, "", 1)
	seedRereadRow(t, pool, seedWaitBefore, "", 1)
	issue.Key, issue.Repo, issue.DefaultBranch, issue.GitHubRepo, issue.Project = seedWaitIssue, "acme/widgets", "main", "acme/widgets", "CAPTURE"
	issue.Title, issue.Tree, issue.Parent, issue.Generation, issue.Rank, issue.LastDispatchSeq = seedWaitIssue, seedWaitIssue, record.ParentOf(seedWaitBefore), 1, "B", 1
	seedTickIssue(t, pool, issue)
	seedRereadRow(t, pool, seedWaitIssue, seedWaitBefore, 1)
	client.serve(dispatch.Issue{Key: seedWaitAbove, Project: "CAPTURE", Status: "in_progress", LastSeq: 1})
	client.serve(dispatch.Issue{Key: seedWaitParent, Project: "CAPTURE", Status: "backlog", Parent: new(seedWaitAbove), LastSeq: 2})
}

// seedWaitTells runs CAPTURE-500's one re-parent seed as the outbox's lane runs a row (execute):
// it waits on CAPTURE-700's chain and tells nobody. One turn of R16's re-read (rereadDue) then
// asks Dispatch for asked, in that order, CAPTURE-700's chain among them, and applies the answers,
// so the copy holds that chain known; and the seed's next attempt tells CAPTURE-300's architect,
// above the issue now, once, in the words of the move from CAPTURE-208 to CAPTURE-700.
func seedWaitTells(t *testing.T, w *workflowRuntime, client *tickDispatch, pool *store.Pool, asked []string) {
	t.Helper()
	ctx := context.Background()
	rows := rowsOfKind(t, pool, record.OutboxKindReparentSeed)
	if len(rows) != 1 || rows[0].Issue != seedWaitIssue {
		t.Fatalf("premise: the outbox holds re-parent seeds %+v, want one of %s", rows, seedWaitIssue)
	}
	payload, err := record.DecodeOutboxPayload(rows[0])
	if err != nil {
		t.Fatalf("decode the re-parent seed: %v", err)
	}
	if seed := payload.(record.ReparentSeed); seed.Parent != seedWaitParent || seed.OldParent != seedWaitBefore {
		t.Fatalf("premise: the seed is %+v, want the move from %s to %s", seed, seedWaitBefore, seedWaitParent)
	}
	if err := w.outbox.execute(ctx, rows[0]); !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, record.ErrChainUnknown) {
		t.Fatalf("the seed's first attempt = %v, want it waiting for the chain of %s", err, seedWaitParent)
	}
	if told := seedWaitTold(t, pool); len(told) != 0 {
		t.Fatalf("while the chain of %s is unknown the seed told %+v, want nobody", seedWaitParent, told)
	}

	before := len(client.asked())
	w.rereadDue(ctx, map[string]*chainReread{}, time.Now())
	if got := client.asked()[before:]; !slices.Equal(got, asked) {
		t.Errorf("R16's re-read asked Dispatch for %v, want %v: the chain of %s, which the seed waits on, read from Dispatch", got, asked, seedWaitParent)
	}
	if !rereadKnown(t, pool, seedWaitParent) {
		t.Errorf("the copy holds the chain of %s unknown after the re-read applied Dispatch's answers", seedWaitParent)
	}

	if err := w.outbox.execute(ctx, rows[0]); err != nil {
		t.Fatalf("the seed's attempt once the chain of %s is known = %v, want it applied", seedWaitParent, err)
	}
	told := seedWaitTold(t, pool)
	if len(told) != 1 || told[0].To != seedWaitAbove ||
		!strings.Contains(told[0].Reason, "changed from "+seedWaitBefore+" to "+seedWaitParent) ||
		!strings.Contains(told[0].Reason, "The architect of "+seedWaitAbove+" now stands above it") {
		t.Fatalf("the seed told %+v, want one reparented notice to %s's architect of the move from %s to %s", told, seedWaitAbove, seedWaitBefore, seedWaitParent)
	}
}

// seedWaitTold is each reparented notice of CAPTURE-500 the outbox holds, oldest first.
func seedWaitTold(t *testing.T, pool *store.Pool) []record.Notice {
	t.Helper()
	var told []record.Notice
	for _, row := range rowsOfKind(t, pool, record.OutboxKindNotice) {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode notice row %d: %v", row.ID, err)
		}
		if notice := payload.(record.Notice); row.Issue == seedWaitIssue && notice.Kind == record.ReparentedNotice {
			told = append(told, notice)
		}
	}
	return told
}
