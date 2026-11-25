package admit

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// committedLine is one owner branch of admission whose line says what its transaction wrote:
// seed builds the fixture and answers the one fact that reaches the branch, applied through the
// handlers a daemon applies it through, admission last; wrote reads the branch's durable write;
// line is the line's message as the text handler writes it, and detail, where set, a field the
// committed line carries. holds is whether the branch also clears an in-memory hold as the hook
// of its commit (Admission.Held).
type committedLine struct {
	name   string
	line   string
	detail string
	holds  bool
	seed   func(t *testing.T, pool *pgxpool.Pool, log *slog.Logger) (source string, fact intake.Fact, handlers []intake.Handler)
	wrote  func(t *testing.T, pool *pgxpool.Pool) bool
}

func committedLines() []committedLine {
	return []committedLine{
		{name: "the release of a held root", line: `msg="admission: the Dispatch consumer has caught up; releasing held roots"`, detail: "admitted=1", holds: true,
			seed: func(t *testing.T, pool *pgxpool.Pool, log *slog.Logger) (string, intake.Fact, []intake.Handler) {
				admission := newAdmission(t, 1, log)
				putIssue(t, pool, record.Issue{Key: "LEGION-EDGE", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
					Project: testProject, Title: "LEGION-EDGE", Tree: "LEGION-EDGE", Phase: phase.Admitted, Generation: 1, Status: "todo",
					Rank: "A", HandedOver: true, LastDispatchSeq: 1})
				reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-EDGE", ResolvedRepo: "acme/widgets",
					Title: "LEGION-EDGE", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 2}}, 5, 2, false)
				if !admission.Held() {
					t.Fatal("the boot listing holds no root: the case's premise")
				}
				return "dispatch", intake.DispatchConsumerPosition{AckFloorStream: 5}, []intake.Handler{engineStub{}, admission}
			},
			wrote: func(t *testing.T, pool *pgxpool.Pool) bool { return holdsSlot(t, pool, "LEGION-EDGE") }},
		{name: "the hand-over event", line: `msg="admission hand-over" issue=LEGION-209`,
			seed: func(t *testing.T, pool *pgxpool.Pool, log *slog.Logger) (string, intake.Fact, []intake.Handler) {
				admission := newAdmission(t, 1, log)
				engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
					Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
				seedFinishedChildUnderLingeringRoot(t, pool, "LEGION-208", "LEGION-209")
				return "dispatch", intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
						Status: "todo", Title: "LEGION-209", Parent: "LEGION-208", Rank: "B", HandedOver: true, DaemonActor: true},
					[]intake.Handler{engine, admission}
			},
			wrote: func(t *testing.T, pool *pgxpool.Pool) bool {
				got := issue(t, pool, "LEGION-209")
				return got.Tree == "LEGION-209" && got.Generation == 2
			}},
		{name: "a slot given back to an issue whose work came back", line: `msg="admission gives a slot back to an issue whose work came back"`,
			seed: func(t *testing.T, pool *pgxpool.Pool, log *slog.Logger) (string, intake.Fact, []intake.Handler) {
				admission := newAdmission(t, 1, log)
				const returning = "LEGION-32"
				putIssue(t, pool, record.Issue{Key: returning, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
					Project: testProject, Title: returning, Tree: returning, Phase: phase.Implementing, Generation: 1, Status: "in_progress",
					Rank: "A", HandedOver: true})
				seedLifecycle(t, pool, returning)
				seedMirror(t, pool, returning, "in_progress")
				oweStart(t, pool, returning, returning, phase.Implementing)
				return "controller", intake.ControllerTick{}, []intake.Handler{engineStub{}, admission}
			},
			wrote: func(t *testing.T, pool *pgxpool.Pool) bool { return holdsSlot(t, pool, "LEGION-32") }},
		{name: "a slot given back to an issue whose wait ended", line: `msg="admission gives a slot back to an issue whose wait ended"`,
			seed: func(t *testing.T, pool *pgxpool.Pool, log *slog.Logger) (string, intake.Fact, []intake.Handler) {
				admission := newAdmission(t, 4, log)
				const key, blocker = "LEGION-40", "LEGION-49"
				seedMirror(t, pool, blocker, "in_progress")
				seedStarted(t, pool, key, phase.Reviewing, nil)
				seedMirror(t, pool, key, "in_progress", blocker)
				tick(t, pool, admission, "release-for-the-wait")
				if released := issue(t, pool, key); released.ReleasedForWaitAt == nil || holdsSlot(t, pool, key) {
					t.Fatalf("%s = %+v, want it released for its wait: the case's premise", key, released)
				}
				// The blocker's own event, through the mirror first as the daemon applies it: the wait
				// ends in the transaction that gives the row back.
				return "dispatch", intake.DispatchIssue{Key: blocker, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
						Status: "done", Title: blocker, Rank: "Z"},
					[]intake.Handler{blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission}
			},
			wrote: func(t *testing.T, pool *pgxpool.Pool) bool { return holdsSlot(t, pool, "LEGION-40") }},
		{name: "a root that waits for its Dispatch children", line: `msg="admission: an issue waits for the Dispatch children under it"`,
			seed: func(t *testing.T, pool *pgxpool.Pool, log *slog.Logger) (string, intake.Fact, []intake.Handler) {
				seedSlotted(t, pool, cfRoot, "A")
				seedMirror(t, pool, cfRoot, "in_progress")
				seedGate(t, pool, record.DesignGate{Issue: cfRoot, ArtifactID: "artifact-1", LatestVersion: 1})
				admission := newAdmission(t, 3, log)
				seedMirrorChild(t, pool, cfFirst, "backlog", cfRoot)
				return "dispatch", intake.DispatchArtifact{Key: cfRoot, ArtifactID: "artifact-1", Kind: intake.DispatchArtifactApproved,
					Version: 1, Mark: record.EventMark{Seq: 4, At: reviewedAt}}, []intake.Handler{admission.engine, admission}
			},
			wrote: func(t *testing.T, pool *pgxpool.Pool) bool { return len(childrenFirstNotices(t, pool, cfRoot)) == 1 }},
		// The orphan's key has Dispatch's shape, as every key the daemon records has (intake's
		// decodeDispatchIssueEvent drops any other): the suspend of the claims of the tree it leaves
		// builds each role's claim token from that key (workflow's claimTargets).
		{name: "an orphan the record holds as a child", line: `msg="admission orphan" issue=LEGION-77`,
			seed: func(t *testing.T, pool *pgxpool.Pool, log *slog.Logger) (string, intake.Fact, []intake.Handler) {
				admission := newAdmission(t, 1, log)
				gone := "LEGION-GONE"
				putIssue(t, pool, record.Issue{Key: "LEGION-77", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
					Project: testProject, Title: "LEGION-77", Tree: gone, Parent: &gone, Phase: phase.Admitted, Generation: 1,
					Status: "backlog", Rank: "B", LastDispatchSeq: 1})
				return "dispatch", intake.DispatchIssue{Key: "LEGION-77", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
					Status: "todo", Title: "LEGION-77", Parent: gone, Rank: "B", HandedOver: handed}, []intake.Handler{engineStub{}, admission}
			},
			wrote: func(t *testing.T, pool *pgxpool.Pool) bool {
				return issue(t, pool, "LEGION-77").Tree == "LEGION-77"
			}},
		{name: "an orphan the record does not hold", line: `msg="admission orphan" issue=LEGION-ORPHAN`,
			seed: func(t *testing.T, pool *pgxpool.Pool, log *slog.Logger) (string, intake.Fact, []intake.Handler) {
				admission := newAdmission(t, 1, log)
				return "dispatch", intake.DispatchIssue{Key: "LEGION-ORPHAN", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated",
					Status: "todo", Title: "orphan", Parent: "LEGION-MISSING", Rank: "A", HandedOver: handed}, []intake.Handler{engineStub{}, admission}
			},
			wrote: func(t *testing.T, pool *pgxpool.Pool) bool { return maybeIssue(t, pool, "LEGION-ORPHAN") != nil }},
	}
}

// Each owner branch of admission whose line says what its transaction wrote writes the line as
// the hook of that commit. The branch's fact is applied once with its COMMIT cut before it
// reaches PostgreSQL: the transaction rolls back, the branch's write is not there, and no line
// is. The same fact applied again commits: the write is there, and the line once.
func TestAnAdmissionLineOfACommittedWriteIsWrittenOnlyOnceItsTransactionCommits(t *testing.T) {
	for _, tc := range committedLines() {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			var logs bytes.Buffer
			source, fact, handlers := tc.seed(t, pool, slog.New(slog.NewTextHandler(&logs, nil)))
			if tc.wrote(t, pool) {
				t.Fatal("the branch's write is there before its fact applied: the case proves nothing")
			}
			logs.Reset()
			lines := func() int { return strings.Count(logs.String(), tc.line) }
			admission := handlers[len(handlers)-1].(*Admission)

			rolledBack := cutBeforeCommit(t, pool, func(faulted *pgxpool.Pool) error {
				_, err := intake.ApplyFact(context.Background(), faulted, source, "committed-line", fact, handlers...)
				return err
			})
			if rolledBack == nil {
				t.Fatal("the fact whose COMMIT was cut before it reached the server = nil, want its failure")
			}
			if tc.wrote(t, pool) {
				t.Fatal("the rolled-back fact's write is there: the cut came after the commit")
			}
			if got := lines(); got != 0 {
				t.Errorf("lines after the rolled-back fact = %d, want 0; log:\n%s", got, logs.String())
			}
			if tc.holds && !admission.Held() {
				t.Error("the rolled-back fact cleared the hold, want it held until a release commits")
			}

			if _, err := intake.ApplyFact(context.Background(), pool, source, "committed-line", fact, handlers...); err != nil {
				t.Fatalf("the same fact applied again: %v", err)
			}
			if !tc.wrote(t, pool) {
				t.Fatal("the fact applied again made no write: the fact does not reach its branch")
			}
			if got := lines(); got != 1 {
				t.Errorf("lines after the committed fact = %d, want 1; log:\n%s", got, logs.String())
			}
			if tc.detail != "" && !strings.Contains(logs.String(), tc.detail) {
				t.Errorf("the committed line does not carry %q; log:\n%s", tc.detail, logs.String())
			}
			if tc.holds && admission.Held() {
				t.Error("the committed fact left the hold, want it cleared")
			}
		})
	}
}

// cutBeforeCommit runs apply against pool's database through a proxy whose fault cuts the
// connection of the first fact transaction (the lock every fact takes, intake.SerializeFacts) as
// its COMMIT leaves, so PostgreSQL rolls it back, and answers what apply returned.
func cutBeforeCommit(t *testing.T, pool *pgxpool.Pool, apply func(faulted *pgxpool.Pool) error) error {
	t.Helper()
	proxied, fault := testdb.CommitFaults(t, pool.Config().ConnString())
	faulted, err := pgxpool.New(context.Background(), proxied)
	if err != nil {
		t.Fatalf("open a pool through the fault proxy: %v", err)
	}
	t.Cleanup(faulted.Close)
	fired := fault.Cut("legion.apply_fact", testdb.CutBeforeCommit)
	err = apply(faulted)
	testdb.Met(t, fired)
	return err
}
