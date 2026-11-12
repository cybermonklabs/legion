package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A hand-over the outbox completes is the one door into the workflow that raises a generation with
// no Dispatch event and no PATCH that says anything about the repository: the status executor reads
// the issue from Dispatch, and that read is the whole of what the daemon knows of its Dispatch
// state. These tests read what the executor builds its intake.Handover fact with — the read's own
// resolution — and what the record is on once admission has consumed it, for both ways the
// executor reaches that fact: Dispatch already showing the todo and the label, and the hand-over's
// own PATCH.

// handOverCorrectionRoot is the done root lingering after its sign-off, and handOverCorrectionChild
// the member it hands to Legion as a root of its own.
const handOverCorrectionRoot, handOverCorrectionChild = "LEGION-307", "LEGION-308"

// handOverCorrectionMoment is the engine's clock here, so a correction is stamped with a moment the
// test can read back.
var handOverCorrectionMoment = time.Date(2026, 11, 12, 10, 15, 0, 0, time.UTC)

// handOverCorrectionRepositories serves the repository the child is recorded on and the one
// Dispatch moves it to, each at a default branch of its own.
var handOverCorrectionRepositories = servedRepositories{"acme/widgets": "main", "acme/tools": "release/v7.1"}

// handOverCorrectionRig is one test's daemon: the records, the two handlers the hand-over's fact
// runs through, and the outbox row that completes the hand-over, over a database of its own.
type handOverCorrectionRig struct {
	t         *testing.T
	pool      *pgxpool.Pool
	records   record.Store
	engine    *workflow.Engine
	admission *admit.Admission
}

func newHandOverCorrectionRig(t *testing.T) *handOverCorrectionRig {
	t.Helper()
	r := &handOverCorrectionRig{t: t, pool: isolatedOutboxPool(t), records: record.NewStore()}
	r.engine = workflow.New(r.records, workflow.Config{Project: "LEGION", Repositories: handOverCorrectionRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour,
		Clock: func() time.Time { return handOverCorrectionMoment }}, quietLogger())
	r.admission = admit.New(r.records, r.engine, 1, "LEGION", quietLogger())
	r.seed()
	return r
}

// seed records the lingering root and the child it handed to Legion, both on acme/widgets, with
// the durable fence the linger's hand-over wrote for it.
func (r *handOverCorrectionRig) seed() {
	r.t.Helper()
	ctx := context.Background()
	until := handOverCorrectionMoment.Add(time.Hour)
	parent := handOverCorrectionRoot
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		for _, put := range []error{
			r.records.PutIssue(ctx, tx, record.Issue{Key: handOverCorrectionRoot, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Tree: handOverCorrectionRoot, Project: "LEGION", Title: handOverCorrectionRoot,
				Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A", HandedOver: true, LingerUntil: &until}),
			r.records.PutIssue(ctx, tx, record.Issue{Key: handOverCorrectionChild, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Tree: handOverCorrectionRoot, Project: "LEGION", Title: handOverCorrectionChild, Parent: &parent,
				Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "todo", Rank: "B", LastDispatchSeq: 4}),
			r.records.PutHandover(ctx, tx, record.Handover{Issue: handOverCorrectionChild, Tree: handOverCorrectionRoot, Generation: 1}),
		} {
			if put != nil {
				return put
			}
		}
		return nil
	}); err != nil {
		r.t.Fatalf("seed the hand-over: %v", err)
	}
}

// complete runs the hand-over's outbox row against a Dispatch that answers the status executor's
// read with shown, and reports that stand-in so the test can read the writes it took.
func (r *handOverCorrectionRig) complete(shown dispatch.Issue) *outboxDispatch {
	r.t.Helper()
	client := &outboxDispatch{issue: shown}
	row := mustOutboxRow(r.t, handOverCorrectionChild, record.HandOverStatusWrite("todo", "in_progress", handOverCorrectionRoot, 1),
		handOverCorrectionMoment)
	runner := &outbox{pool: r.pool, records: r.records, dispatch: client, handlers: []intake.Handler{r.engine, r.admission}, log: quietLogger()}
	if err := runner.execute(context.Background(), row); err != nil {
		r.t.Fatalf("complete the hand-over: %v", err)
	}
	return client
}

// child is the handed-over child's record as it stands.
func (r *handOverCorrectionRig) child() record.Issue {
	r.t.Helper()
	ctx := context.Background()
	var got *record.Issue
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		got, err = r.records.Issue(ctx, tx, handOverCorrectionChild)
		return err
	}); err != nil {
		r.t.Fatalf("read the handed-over child: %v", err)
	}
	if got == nil {
		r.t.Fatalf("%s is not recorded", handOverCorrectionChild)
	}
	return *got
}

// messages is every Dispatch message the hand-over queued, in the order they were queued.
func (r *handOverCorrectionRig) messages() []string {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), "select payload->>'body' from outbox where kind = $1 order by id",
		string(record.OutboxKindDispatchMessage))
	if err != nil {
		r.t.Fatalf("read the Dispatch messages: %v", err)
	}
	defer rows.Close()
	var bodies []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			r.t.Fatalf("scan a Dispatch message: %v", err)
		}
		bodies = append(bodies, body)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the Dispatch messages: %v", err)
	}
	return bodies
}

// readmitted fails unless the child came back as a root of its own at its next generation, which
// is the hand-over's own work whatever its repository does.
func (r *handOverCorrectionRig) readmitted(got record.Issue) {
	r.t.Helper()
	if got.Tree != handOverCorrectionChild || got.Generation != 2 {
		r.t.Fatalf("the handed-over child = %#v, want it re-admitted as a tree of its own at generation 2", got)
	}
}

// The repository the status executor's read answered with is what the hand-over's fact carries, so
// a child handed to Legion while its repository changed in Dispatch opens its new generation on the
// repository Dispatch resolves it to now, at that repository's own default branch, keeping the one
// it left. Both ways the executor reaches the fact carry it: the read that already shows the todo
// and the label, where no write is made at all, and the read behind the hand-over's own PATCH.
func TestRepositoryCorrectionFollowsAHandOverTheOutboxCompletes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		shown     dispatch.Issue
		handOvers int
	}{
		{name: "Dispatch already shows the todo and the label", handOvers: 0,
			shown: dispatch.Issue{Key: handOverCorrectionChild, Status: "todo", HandedOver: true, Labels: []string{"legion"},
				ResolvedRepo: "acme/tools"}},
		{name: "the hand-over's own PATCH", handOvers: 1,
			shown: dispatch.Issue{Key: handOverCorrectionChild, Status: "in_progress", ResolvedRepo: "acme/tools"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHandOverCorrectionRig(t)

			client := r.complete(tc.shown)

			if len(client.handOvers) != tc.handOvers {
				t.Fatalf("hand-over PATCHes %v, want %d", client.handOvers, tc.handOvers)
			}
			got := r.child()
			r.readmitted(got)
			if got.Repo != "acme/tools" || got.DefaultBranch != "release/v7.1" || got.GitHubRepo != "acme/tools" {
				t.Fatalf("the re-admitted child is on %s at %s, spelled %s; want acme/tools at release/v7.1, spelled acme/tools",
					got.Repo, got.DefaultBranch, got.GitHubRepo)
			}
			if got.RepoBefore != "acme/widgets" || got.RepoCorrectedAt == nil || !got.RepoCorrectedAt.Equal(handOverCorrectionMoment) {
				t.Fatalf("the correction's marks = %q at %v, want acme/widgets at %v",
					got.RepoBefore, got.RepoCorrectedAt, handOverCorrectionMoment)
			}
			if messages := r.messages(); len(messages) != 0 {
				t.Fatalf("the issue was told %q, want nothing: its record followed Dispatch onto a repository Legion serves", messages)
			}
		})
	}
}

// A read that answered no repository carries none into the fact, and the child comes back on the
// repository its own record holds: an absent resolution is no statement by Dispatch that the issue
// is on no repository, so nothing is corrected and nothing is said to the issue.
func TestRepositoryCorrectionIsNotWrittenWhenAHandOversDispatchReadAnswersNoRepository(t *testing.T) {
	r := newHandOverCorrectionRig(t)

	r.complete(dispatch.Issue{Key: handOverCorrectionChild, Status: "todo", HandedOver: true, Labels: []string{"legion"}})

	got := r.child()
	r.readmitted(got)
	if got.Repo != "acme/widgets" || got.DefaultBranch != "main" || got.GitHubRepo != "acme/widgets" {
		t.Fatalf("the re-admitted child is on %s at %s, spelled %s; want the record's own acme/widgets at main",
			got.Repo, got.DefaultBranch, got.GitHubRepo)
	}
	if got.RepoBefore != "" || got.RepoCorrectedAt != nil {
		t.Fatalf("the correction's marks = %q at %v, want none written: the read answered no repository to follow",
			got.RepoBefore, got.RepoCorrectedAt)
	}
	if got.RepoChangePending != "" || got.RepoChangeReason != "" {
		t.Fatalf("the waiting change = %q because %q, want none", got.RepoChangePending, got.RepoChangeReason)
	}
	if messages := r.messages(); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing", messages)
	}
}
