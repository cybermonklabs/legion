package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// passReads counts the reads of the project's live records admission makes through the store it
// is given: each pass reads them once at its start (adoptQueuedRoots) and again only after a write
// of its own makes them stale (reread), so a fact that runs no pass reads them not at all. Every
// other read and write is the real store's.
type passReads struct {
	record.Store
	live int
}

func (s *passReads) LiveIssues(ctx context.Context, tx pgx.Tx, project string) ([]record.Issue, error) {
	s.live++
	return s.Store.LiveIssues(ctx, tx, project)
}

// passCounted is an admission of cap slots whose store counts the reads of a pass (passReads), on
// an engine of its own under the design gate newAdmission's engine runs, as the daemon gives it
// one.
func passCounted(t *testing.T, cap int) (*Admission, *passReads) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGate("").Steps(), Clock: func() time.Time { return fixedNow }}, log)
	reads := &passReads{Store: record.NewStore()}
	admission := New(reads, engine, cap, testProject, log)
	admission.now = func() time.Time { return fixedNow }
	return admission, reads
}

// applyAsTheDaemon applies fact through the daemon's handlers in the daemon's order
// (daemon/workflow.go): the mirror, the workflow engine admission holds, and admission.
func applyAsTheDaemon(t *testing.T, pool *pgxpool.Pool, admission *Admission, source, eventID string, fact intake.Fact) {
	t.Helper()
	mirror := blockers.NewMirror(record.NewStore(), testProject)
	if _, err := intake.ApplyFact(context.Background(), pool, source, eventID, fact, mirror, admission.engine, admission); err != nil {
		t.Fatalf("ApplyFact %s/%s: %v", source, eventID, err)
	}
}

// A GitHub fact whose transaction writes no record of the project runs no admission pass: a push
// to an issue branch no pull request records, and a push to a base no recorded pull request is
// on. A root waits in the line with a slot free, so any pass would admit it; after the push no
// read of a pass has run and the slot is still free. Before, every fact ran the whole pass under
// the one fact lock, so a repository's pushes and workflow runs queued every consumer's facts
// behind passes that decided nothing new.
func TestAGitHubFactThatWritesNoRecordRunsNoAdmissionPass(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact intake.Fact
	}{
		{name: "a push to a branch no issue records", fact: intake.Push{Repo: "acme/widgets", Branch: "legion/LEGION-UNRECORDED",
			Before: "1111111", After: "2222222", Pusher: "someone", Sender: "someone"}},
		{name: "a push to a base no recorded pull request is on", fact: intake.BasePush{Repo: "acme/widgets", Branch: "main",
			After: "3333333"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission, reads := passCounted(t, 1)
			seedWaiting(t, pool, "LEGION-ROOT", "A")

			applyAsTheDaemon(t, pool, admission, "github", "push", tc.fact)

			if reads.live != 0 {
				t.Fatalf("the push read the project's live records %d times, want 0: a fact that writes no record runs no pass", reads.live)
			}
			assertSlots(t, pool, nil)
			assertWaiting(t, pool, []string{"LEGION-ROOT"})
		})
	}
}

// A fact whose transaction writes an issue record runs the pass once, and that pass promotes the
// waiting line: a push to the branch a recorded pull request names, which the workflow records on
// that pull request's record as a push waiting for its head to arrive, admits the root waiting
// for the free slot. The pass reads the project's live records once, since promoting that root
// writes nothing that makes them stale.
func TestAFactThatWritesAnIssueRecordRunsOnePassThatPromotesTheLine(t *testing.T) {
	pool := migratedPool(t)
	admission, reads := passCounted(t, 2)
	seedSlottedInPhase(t, pool, testProject, "LEGION-PR", "A", phase.Implementing, nil)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{State: record.PullRequestOpen,
			Issue: "LEGION-PR", Repo: "acme/widgets", Number: 12, Branch: "legion/LEGION-PR", HeadSHA: "1111111", Base: "main",
			Failing: []string{}}); err != nil {
			t.Fatalf("seed the pull request of LEGION-PR: %v", err)
		}
	})
	seedWaiting(t, pool, "LEGION-ROOT", "B")

	applyAsTheDaemon(t, pool, admission, "github", "push", intake.Push{Repo: "acme/widgets", Branch: "legion/LEGION-PR",
		Before: "1111111", After: "2222222", Pusher: "someone", Sender: "someone"})

	if !pushRecorded(t, pool, "LEGION-PR", "2222222") {
		t.Fatal("the pull request's record holds no push to 2222222, want the push the workflow writes into it")
	}
	if reads.live != 1 {
		t.Fatalf("the push read the project's live records %d times, want 1: one pass", reads.live)
	}
	assertSlots(t, pool, []record.Slot{
		{Issue: "LEGION-PR", Index: 0, AdmittedAt: fixedNow},
		{Issue: "LEGION-ROOT", Index: 1, AdmittedAt: fixedNow},
	})
	assertWaiting(t, pool, nil)
}

// The Dispatch consumer's position that reaches the stream position a held root waits on runs
// the pass that releases the hold, though the fact writes no record of its own: the boot's listing
// found the root behind the stream and held it, and the position fact the daemon's poll applies
// once the consumer gets there admits it into the free slot.
func TestADispatchPositionAtAHeldRootsTargetRunsThePassThatReleasesIt(t *testing.T) {
	pool := migratedPool(t)
	admission, reads := passCounted(t, 1)
	putIssue(t, pool, record.Issue{Key: "LEGION-HELD", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "LEGION-HELD", Tree: "LEGION-HELD", Phase: phase.Admitted, Generation: 1, Status: "todo",
		Rank: "A", HandedOver: true, LastDispatchSeq: 1})
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-HELD", ResolvedRepo: "acme/widgets", Title: "LEGION-HELD", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 2},
	}, 3, 1, false)
	if !admission.Held() {
		t.Fatal("Held() = false after the listing found LEGION-HELD behind the stream, want it held")
	}
	assertSlots(t, pool, nil)
	reads.live = 0

	applyAsTheDaemon(t, pool, admission, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 3})

	if reads.live == 0 {
		t.Fatal("the position that reached the hold's target read no live records, want the pass that releases the hold")
	}
	if admission.Held() {
		t.Fatal("Held() = true after the position reached the hold's target, want the hold released")
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-HELD", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, nil)
}

// pushRecorded says whether the record of issue's pull request holds a push that left its branch
// at sha.
func pushRecorded(t *testing.T, pool *pgxpool.Pool, issue, sha string) bool {
	t.Helper()
	var recorded bool
	if err := pool.QueryRow(context.Background(), `select exists (select 1 from pull_requests,
		jsonb_array_elements(coalesce(pull_requests.pushes::jsonb, '[]')) as push where issue = $1 and push->>'sha' = $2)`,
		issue, sha).Scan(&recorded); err != nil {
		t.Fatalf("read the pushes of %s's pull request: %v", issue, err)
	}
	return recorded
}
