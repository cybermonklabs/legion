package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The issue in implementing these tests start from, and the child its architect files under it
// in triage and never releases, which sends this daemon no event.
const (
	startedParent = "CAPTURE-60"
	filedChild    = "CAPTURE-61"
)

// The controller tick's re-read reads the children of every issue whose phases have started
// (dispatch://INFRA-3320 R13), in both branches of the read (readObserved), so a child filed and
// not released reaches the daemon within one wake interval though it sends no event, and so does
// its move to icebox. The admission pass of the same tick reads the mirror the re-read wrote:
// the first tick moves the parent into awaiting_children, and the tick after the child's move to
// icebox takes it back to the start it held.
//
// Below the single-read bound, where an issue has started a phase, the turn lists the project
// once and takes from that listing the summary of each key of this project it waits on and of
// each issue whose parent has started: the filed child alone at first, whose row the mirror never
// held. Once the parent waits, its open child is a key the turn waits on, and it is still taken
// from that one listing rather than read singly, since the listing holds every key of the project
// with the same fields. Above the bound the one listing the turn makes already takes every
// summary, the child's among them.
func TestTheTickRereadReadsTheChildrenOfAnIssueWhosePhasesStarted(t *testing.T) {
	parent := startedParent
	filed := dispatch.IssueSummary{Key: filedChild, Status: "triage", Parent: &parent, LastSeq: 1}

	t.Run("below the single-read bound", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		// The listing shows the parent at a sequence past its mirror row, which the turn must not
		// take: it waits on nothing, and the listing is taken for the keys the turn waits on and
		// the children alone.
		listedParent := dispatch.IssueSummary{Key: startedParent, Status: "testing", LastSeq: 5}
		client := &tickDispatch{summaries: []dispatch.IssueSummary{listedParent, filed}}
		w := tickRuntime(t, pool, client)
		seedStartedParent(t, pool, startedParent)

		w.applyControllerTick(context.Background(), time.Now())

		if asked, listings := client.asked(), client.listings(); len(asked) != 0 || listings != 1 {
			t.Fatalf("the first turn read %v singly and listed the project %d times, want no single read and the one listing of the children", asked, listings)
		}
		mirrored := tickMirrored(t, pool)
		if row := mirrored[filedChild]; row.Status != "triage" || row.Parent != startedParent || row.Seq != 1 {
			t.Fatalf("the mirror holds %s as %+v, want the listing's reading: triage under %s", filedChild, row, startedParent)
		}
		if row := mirrored[startedParent]; row.Status != "in_progress" || row.Seq != 1 {
			t.Fatalf("the mirror holds %s as %+v, want its own row as seeded (in_progress, 1): the listing is taken for the waited-on keys and the children alone", startedParent, row)
		}
		if got := recordOf(t, pool, record.NewStore(), startedParent); got.Phase != phase.AwaitingChildren {
			t.Fatalf("%s after the turn = %s, want awaiting_children: the turn's own pass read its filed child", startedParent, got.Phase)
		}

		// The child moves to icebox. It still sends no event; the next turn takes it from its one
		// listing, now as a key the turn waits on, since the waiting parent's open child is one,
		// and reads it singly no more than it did the first time.
		iceboxed := filed
		iceboxed.Status, iceboxed.LastSeq = "icebox", 2
		client.summaries = []dispatch.IssueSummary{listedParent, iceboxed}

		w.applyControllerTick(context.Background(), time.Now().Add(time.Second))

		if asked, listings := client.asked(), client.listings(); len(asked) != 0 || listings != 2 {
			t.Fatalf("the turns read %v singly and listed the project %d times, want no single read and a listing on each", asked, listings)
		}
		if row := tickMirrored(t, pool)[filedChild]; row.Status != "icebox" || row.Seq != 2 {
			t.Fatalf("the mirror holds %s as %+v, want icebox at 2", filedChild, row)
		}
		if got := recordOf(t, pool, record.NewStore(), startedParent); got.Phase != phase.Implementing || got.ChildrenWait != nil {
			t.Fatalf("%s after its only child moved to icebox = phase %s, held start %+v; want implementing, the start it held", startedParent, got.Phase, got.ChildrenWait)
		}
	})

	t.Run("above the single-read bound", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		client := &tickDispatch{summaries: []dispatch.IssueSummary{filed}}
		for i := 1; i <= blockerReadFanout+1; i++ {
			client.summaries = append(client.summaries, dispatch.IssueSummary{Key: ownBlocker(i), Status: "in_progress", LastSeq: 1})
		}
		w := tickRuntime(t, pool, client)
		seedStartedParent(t, pool, startedParent)
		seedBlockedRoots(t, pool, blockerReadFanout+1)

		w.applyControllerTick(context.Background(), time.Now())

		if asked, listings := client.asked(), client.listings(); len(asked) != 0 || listings != 1 {
			t.Fatalf("the turn read %v singly and listed the project %d times, want the one listing and no single read", asked, listings)
		}
		if row := tickMirrored(t, pool)[filedChild]; row.Status != "triage" || row.Parent != startedParent {
			t.Fatalf("the mirror holds %s as %+v, want the listing's reading: triage under %s", filedChild, row, startedParent)
		}
		if got := recordOf(t, pool, record.NewStore(), startedParent); got.Phase != phase.AwaitingChildren {
			t.Fatalf("%s after the turn = %s, want awaiting_children", startedParent, got.Phase)
		}
	})

	// A listing that fails leaves out the children it would have read, and the turn reads this
	// project's waited-on keys one at a time instead, as it does where no issue has started a
	// phase: a blocker whose close the daemon missed is still caught on this turn.
	t.Run("below the single-read bound, a failed listing", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		client := &tickDispatch{summaries: []dispatch.IssueSummary{filed}, issues: map[string]dispatch.Issue{
			ownBlocker(1): {Key: ownBlocker(1), Project: "CAPTURE", Status: "done", LastSeq: 7},
		}}
		w := tickRuntime(t, pool, failingListing{client})
		seedStartedParent(t, pool, startedParent)
		seedBlockedRoots(t, pool, 2)

		w.applyControllerTick(context.Background(), time.Now())

		if asked, listings := slices.Sorted(slices.Values(client.asked())), client.listings(); !slices.Equal(asked, []string{ownBlocker(1), ownBlocker(2)}) || listings != 1 {
			t.Fatalf("the turn read %v singly and listed the project %d times, want the one failed listing and %s and %s read singly",
				asked, listings, ownBlocker(1), ownBlocker(2))
		}
		mirrored := tickMirrored(t, pool)
		if row := mirrored[ownBlocker(1)]; row.Status != "done" || row.Seq != 7 {
			t.Fatalf("the mirror holds %s as %+v, want the single read's answer (done, 7)", ownBlocker(1), row)
		}
		if row, held := mirrored[filedChild]; held {
			t.Fatalf("the mirror holds %s as %+v, want no row: the listing that would have read it failed", filedChild, row)
		}
	})
}

// failingListing is Dispatch whose project listing fails while its single reads answer: the
// listing still counts as asked (tickDispatch.listings).
type failingListing struct{ *tickDispatch }

func (d failingListing) ListIssues(ctx context.Context, project string, statuses []string) ([]dispatch.IssueSummary, error) {
	_, _ = d.tickDispatch.ListIssues(ctx, project, statuses)
	return nil, errors.New("the listing is unreachable")
}

// seedStartedParent records key as a root of this project at work in implementing: its record,
// its mirror row, its tree lifecycle open under the workflow and the one admission slot, as
// promotion leaves a root whose first phase it started. No child of it is in the mirror.
func seedStartedParent(t *testing.T, pool *store.Pool, key string) {
	t.Helper()
	seedTickIssue(t, pool, record.Issue{
		Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "CAPTURE",
		Title: key, Tree: key, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A",
		HandedOver: true, LastDispatchSeq: 1,
	})
	seedTickMirror(t, pool, key, "in_progress", 1)
	ctx := context.Background()
	token, err := claim.ProjectToken("CAPTURE")
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		if _, err := records.OpenTreeLifecycle(ctx, tx, token, key, treelifecycle.AuthorityWorkflow); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: 0, AdmittedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("open the tree of %s and give it its slot: %v", key, err)
	}
}
