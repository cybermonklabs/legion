package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The repository an issue's record is corrected onto is Dispatch's own fact, and these tests read
// what the daemon's two passes over a recorded issue's resolution do with it: the live Dispatch
// event (the engine's dispatchIssue) and the boot listing (admission's reconcile).
// Each runs over a real PostgreSQL, as every test of this package does, and reads the record and
// the Dispatch messages the pass queued, never the statement that writes the record, which has its
// own test (record's TestCorrectIssueRepository...).

// correctionKey is the recorded issue every test here moves between repositories, and
// correctionProject the Dispatch project it belongs to.
const (
	correctionKey     = "LEGION-400"
	correctionProject = "LEGION"
	// correctionGeneration is the generation the issue is recorded at, which no correction moves.
	correctionGeneration uint64 = 2
)

// correctionMoment is the engine's clock in these tests, so the moment a correction is stamped
// with is one the test can read back.
var correctionMoment = time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)

// correctionRepositories is a project's repositories as the daemon holds them in memory
// (workflow.Repositories): each served repository's default branch and GitHub's own spelling of
// it, by the name Dispatch resolves an issue to. A name the map does not hold is a repository
// Legion serves no installation of, and a test adds one where what it reads is the pass that
// finds the repository served at last.
type correctionRepositories map[string]workflow.Resolution

func (r correctionRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	if resolution, ok := r[resolved]; ok {
		return resolution, nil
	}
	return workflow.Resolution{Refusal: resolved + " is not mapped to project " + correctionProject + " in Dispatch"}, nil
}

// correctionRig is one test's daemon: the records, the repositories it serves, and the two
// handlers a Dispatch fact runs through, over a database of its own. Its issue is the running
// root of a live tree, recorded on acme/widgets, which is what makes a moved phase or generation
// visible: nothing in a correction may start, stop or re-run anything.
type correctionRig struct {
	t          *testing.T
	pool       *pgxpool.Pool
	repos      correctionRepositories
	records    record.Store
	engine     *workflow.Engine
	admission  *admit.Admission
	generation uint64
}

func newCorrectionRig(t *testing.T) *correctionRig {
	t.Helper()
	r := &correctionRig{
		t:    t,
		pool: isolatedOutboxPool(t),
		repos: correctionRepositories{
			"acme/widgets": {Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
			"acme/tools":   {Repo: "acme/tools", DefaultBranch: "release/v7.1", GitHubRepo: "acme/tools"},
		},
		records:    record.NewStore(),
		generation: correctionGeneration,
	}
	r.engine = workflow.New(r.records, workflow.Config{Project: correctionProject, Repositories: r.repos, Steps: config.DesignGateOff.Steps(),
		Clock: func() time.Time { return correctionMoment }}, quietLogger())
	r.admission = admit.New(r.records, r.engine, 1, correctionProject, quietLogger())
	recordCorrectionRoot(t, r.pool, r.records)
	return r
}

// recordCorrectionRoot records the issue the tests here move: the running root of a live tree, on
// acme/widgets at main, at the generation and in the phase a correction may not move.
func recordCorrectionRoot(t *testing.T, pool *pgxpool.Pool, records record.Store) {
	t.Helper()
	ctx := context.Background()
	root := record.Issue{Key: correctionKey, Tree: correctionKey, Project: correctionProject, Title: "root", Phase: phase.Implementing,
		Generation: correctionGeneration, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A", HandedOver: true, LastDispatchSeq: 1,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: correctionMoment}); err != nil {
			return err
		}
		// The tree runs, which its open lifecycle is what says (record.TreeLive): without it
		// admission would take a root at a running status with no tree of it running out of the
		// workflow, which is not what these tests read.
		return openTreeTx(ctx, tx, correctionProject, root.Tree)
	}); err != nil {
		t.Fatal(err)
	}
}

// event applies one live Dispatch event of the issue, at the running status its record holds,
// through the pair of handlers the daemon applies a Dispatch fact through, the workflow then
// admission.
func (r *correctionRig) event(seq int64, resolved string) {
	r.t.Helper()
	r.move(seq, "in_progress", resolved)
}

// move applies one live Dispatch event of the issue at status, the way a person's own move of it
// reaches the daemon: the workflow handler then admission's, in one fact.
func (r *correctionRig) move(seq int64, status, resolved string) {
	r.t.Helper()
	applyFact(r.t, r.pool, "dispatch", fmt.Sprintf("repository-%d", seq), intake.DispatchIssue{Key: correctionKey, Seq: seq, Type: "issue.updated",
		Status: status, Title: "root", Rank: "A", HandedOver: true, ResolvedRepo: resolved}, r.engine, r.admission)
}

// listing runs admission's boot and recovery pass over a Dispatch listing that shows the issue at
// seq on resolved, at the running status its record holds.
func (r *correctionRig) listing(seq int64, resolved string) {
	r.t.Helper()
	r.listingAt(seq, "in_progress", resolved)
}

// listingAt runs that pass over a listing that shows the issue at seq, at status, on resolved,
// with the consumer already caught up, so nothing is held back for it.
func (r *correctionRig) listingAt(seq int64, status, resolved string) {
	r.t.Helper()
	reconcileAdmission(r.t, r.pool, r.admission, []dispatch.IssueSummary{
		{Key: correctionKey, Title: "root", Status: status, Rank: "A", HandedOver: true, LastSeq: seq, ResolvedRepo: resolved},
	}, 0, intake.DispatchConsumerPosition{Idle: true})
}

// issue is the record as it stands.
func (r *correctionRig) issue() record.Issue {
	r.t.Helper()
	var issue *record.Issue
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(ctx, tx, correctionKey)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	if issue == nil {
		r.t.Fatalf("%s is not recorded", correctionKey)
	}
	return *issue
}

// messages is every Dispatch message the passes queued, in the order they were queued.
func (r *correctionRig) messages() []string {
	r.t.Helper()
	return dispatchMessages(r.t, r.pool)
}

// dispatchMessages is the body of every Dispatch message queued in pool's outbox, in the order
// they were queued.
func dispatchMessages(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select payload->>'body' from outbox where kind = $1 order by id",
		string(record.OutboxKindDispatchMessage))
	if err != nil {
		t.Fatalf("read the Dispatch messages: %v", err)
	}
	bodies, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the Dispatch messages: %v", err)
	}
	return bodies
}

// repoNotes is how many repository notes the issue holds, the row that makes a note once per
// resolution (record.RepoNote).
func (r *correctionRig) repoNotes() int {
	r.t.Helper()
	var notes int
	if err := r.pool.QueryRow(context.Background(), "select count(*) from repo_notes where issue = $1", correctionKey).Scan(&notes); err != nil {
		r.t.Fatalf("count the repository notes: %v", err)
	}
	return notes
}

// correctedOn fails unless the record is on repo at branch with GitHub's spelling github, naming
// before as the repository the issue left at the moment of the correction, with nothing waiting.
func (r *correctionRig) correctedOn(issue record.Issue, repo, branch, github, before string) {
	r.t.Helper()
	if issue.Repo != repo || issue.DefaultBranch != branch || issue.GitHubRepo != github {
		r.t.Fatalf("the record is on %s at %s, spelled %s; want %s at %s, spelled %s", issue.Repo, issue.DefaultBranch, issue.GitHubRepo, repo, branch, github)
	}
	if issue.RepoBefore != before {
		r.t.Fatalf("the repository the issue left = %q, want %q", issue.RepoBefore, before)
	}
	if issue.RepoCorrectedAt == nil || !issue.RepoCorrectedAt.Equal(correctionMoment) {
		r.t.Fatalf("the correction's moment = %v, want %v", issue.RepoCorrectedAt, correctionMoment)
	}
	if issue.RepoChangePending != "" || issue.RepoChangeReason != "" {
		r.t.Fatalf("the waiting change after the correction = %q because %q, want none: the correction applied it", issue.RepoChangePending, issue.RepoChangeReason)
	}
}

// corrected is correctedOn with the issue's generation and phase where they stood, which is every
// correction of an issue nothing else moved in the same pass.
func (r *correctionRig) corrected(issue record.Issue, repo, branch, github, before string) {
	r.t.Helper()
	r.correctedOn(issue, repo, branch, github, before)
	if issue.Generation != r.generation || issue.Phase != phase.Implementing {
		r.t.Fatalf("the corrected issue is at generation %d in %s, want generation %d in %s: a correction takes no new generation and moves no phase",
			issue.Generation, issue.Phase, r.generation, phase.Implementing)
	}
}

// correctRecord moves key's record onto repo at branch, stamped at, as the workflow's own pass
// does when Dispatch names another repository for the issue
// (workflow.(*Engine).ApplyRepositoryChange), and fails t unless the record moved.
func correctRecord(t *testing.T, pool *pgxpool.Pool, records record.Store, key, repo, branch string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		corrected, err := records.CorrectIssueRepository(ctx, tx, key, repo, branch, repo, at)
		if err != nil {
			return err
		}
		if !corrected {
			return fmt.Errorf("the record of %s was already on %s", key, repo)
		}
		return nil
	}); err != nil {
		t.Fatalf("correct the repository of %s onto %s: %v", key, repo, err)
	}
}

// A live Dispatch event that resolves a recorded issue to another repository the project serves
// corrects the record onto it there and then: the record names that repository, its default
// branch and GitHub's spelling of it, keeps the one the issue left with the moment it moved, and
// the issue's generation and phase do not move.
func TestRepositoryCorrectionFollowsALiveDispatchEvent(t *testing.T) {
	r := newCorrectionRig(t)
	r.event(2, "acme/tools")
	issue := r.issue()
	r.corrected(issue, "acme/tools", "release/v7.1", "acme/tools", "acme/widgets")
	if issue.LastDispatchSeq != 2 {
		t.Fatalf("the record is at sequence %d, want the event's 2: the correction runs beside the observation, not instead of it", issue.LastDispatchSeq)
	}
	if messages := r.messages(); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing: its record followed Dispatch, so there is nothing for anyone to do", messages)
	}
}

// The boot and recovery listing corrects the record the same way: a daemon that was down while
// the repository changed in Dispatch reads the change from the listing alone, with no event of it
// ever delivered.
func TestRepositoryCorrectionFollowsTheBootListing(t *testing.T) {
	r := newCorrectionRig(t)
	r.listing(3, "acme/tools")
	issue := r.issue()
	r.corrected(issue, "acme/tools", "release/v7.1", "acme/tools", "acme/widgets")
	if issue.LastDispatchSeq != 3 {
		t.Fatalf("the record is at sequence %d, want the listing's 3", issue.LastDispatchSeq)
	}
	if messages := r.messages(); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing: its record followed Dispatch", messages)
	}
}

// An issue out of the workflow whose repository changed in Dispatch meanwhile is re-admitted onto
// the repository Dispatch resolves it to now. The re-admission is the daemon's own
// (admission's readmit: a new generation, admitted, waiting for its slot), and the repository
// reaches the record through the same two passes: the move's live event (the engine's
// dispatchIssue) and the boot and recovery listing (admission's reconcile), each of which follows
// Dispatch's resolution before admission re-admits in the same fact. dispatch://INFRA-2865
// measured the absence: a park and a re-admission gave a new generation on the old repository,
// because nothing re-resolved the repository, and the new run cloned the repository the issue had
// left.
func TestRepositoryCorrectionFollowsAReadmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		readmit func(*correctionRig)
	}{
		{"a live Dispatch event", func(r *correctionRig) { r.move(3, "todo", "acme/tools") }},
		{"the boot listing", func(r *correctionRig) { r.listingAt(3, "todo", "acme/tools") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCorrectionRig(t)
			// A person parks the running root, which takes its tree out of the workflow: the
			// issue is recorded on acme/widgets still, which is what Dispatch resolved it to then.
			r.move(2, "backlog", "acme/widgets")
			if parked := r.issue(); !parked.Lingers() || parked.Repo != "acme/widgets" {
				t.Fatalf("the parked root = %#v, want its tree lingering out of the workflow on acme/widgets", parked)
			}
			// The repository is corrected in Dispatch while the issue is parked, and the move back
			// to todo is the first thing the daemon reads of either change.
			tc.readmit(r)

			issue := r.issue()
			r.correctedOn(issue, "acme/tools", "release/v7.1", "acme/tools", "acme/widgets")
			// The run the re-admission opens is the issue's next generation, which the pass's own
			// promotion then starts on the corrected repository: the status it leaves is that
			// start's, not the todo the re-admission wrote.
			if issue.Generation != correctionGeneration+1 || issue.Phase != phase.Admitted {
				t.Fatalf("the re-admitted root is at generation %d in %s, want generation %d admitted: the re-admission is the daemon's own",
					issue.Generation, issue.Phase, correctionGeneration+1)
			}
			if issue.LingerUntil != nil {
				t.Fatalf("the re-admitted root lingers until %v, want a new generation that lingers for nothing", issue.LingerUntil)
			}
			if issue.LastDispatchSeq != 3 {
				t.Fatalf("the record is at sequence %d, want the re-admission's 3", issue.LastDispatchSeq)
			}
			if messages := r.messages(); len(messages) != 0 {
				t.Fatalf("the issue was told %q, want nothing: its record followed Dispatch onto a repository Legion serves", messages)
			}
		})
	}
}

// A repository Legion serves no installation of is not written onto the record: the record keeps
// the repository it has, the issue is told once however many passes read the same resolution, and
// the change waits on the record with the reason it waits.
func TestRepositoryCorrectionTellsAnIssueWhoseNewRepositoryLegionDoesNotServe(t *testing.T) {
	r := newCorrectionRig(t)
	r.event(2, "acme/gadgets")
	r.event(3, "acme/gadgets")
	r.listing(4, "acme/gadgets")
	issue := r.issue()
	if issue.Repo != "acme/widgets" || issue.DefaultBranch != "main" || issue.RepoBefore != "" || issue.RepoCorrectedAt != nil {
		t.Fatalf("the record is on %s at %s, having left %q at %v; want it still on acme/widgets at main, corrected in nothing",
			issue.Repo, issue.DefaultBranch, issue.RepoBefore, issue.RepoCorrectedAt)
	}
	if issue.RepoChangePending != "acme/gadgets" || issue.RepoChangeReason != "acme/gadgets is not mapped to project LEGION in Dispatch" {
		t.Fatalf("the waiting change = %q because %q, want acme/gadgets and why it waits", issue.RepoChangePending, issue.RepoChangeReason)
	}
	want := "Legion did not move this issue off acme/widgets: acme/gadgets is not mapped to project LEGION in Dispatch. Map acme/gadgets to the project in Dispatch, or set another repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name). Legion moves the issue itself once it serves the repository that Dispatch resolves the issue to."
	if messages := r.messages(); len(messages) != 1 || messages[0] != want {
		t.Fatalf("the issue was told %q, want it told once, saying %q", messages, want)
	}
}

// The waiting change is applied by the first pass that finds the repository served, and the pair
// the record was carrying is cleared with it. Nothing in Dispatch changes for that pass: the
// recovery listing shows the issue at the sequence the record already holds, and the mapping is
// what moved.
func TestRepositoryCorrectionAppliesTheWaitingChangeOnceTheRepositoryIsServed(t *testing.T) {
	r := newCorrectionRig(t)
	r.event(2, "acme/gadgets")
	if pending := r.issue().RepoChangePending; pending != "acme/gadgets" {
		t.Fatalf("the waiting change = %q, want acme/gadgets before the repository is served", pending)
	}
	r.repos["acme/gadgets"] = workflow.Resolution{Repo: "acme/gadgets", DefaultBranch: "trunk", GitHubRepo: "acme/gadgets"}
	r.listing(2, "acme/gadgets")
	r.corrected(r.issue(), "acme/gadgets", "trunk", "acme/gadgets", "acme/widgets")
	if notes := r.repoNotes(); notes != 1 {
		t.Fatalf("the issue holds %d repository notes, want the one the refusal wrote: the correction says nothing of its own", notes)
	}
	if messages := r.messages(); len(messages) != 1 {
		t.Fatalf("the issue was told %q, want only what it was told while the change waited", messages)
	}
}

// A resolution naming the repository the record already holds writes nothing and says nothing: no
// correction, no note, and no repository note row to say one was posted. That is the common pass,
// which every event and every listing of every recorded issue takes.
func TestRepositoryCorrectionWritesNothingWhenDispatchNamesTheRecordedRepository(t *testing.T) {
	r := newCorrectionRig(t)
	r.event(2, "acme/widgets")
	r.listing(3, "acme/widgets")
	issue := r.issue()
	if issue.Repo != "acme/widgets" || issue.RepoBefore != "" || issue.RepoCorrectedAt != nil {
		t.Fatalf("the record is on %s, having left %q at %v; want it on acme/widgets with no correction written",
			issue.Repo, issue.RepoBefore, issue.RepoCorrectedAt)
	}
	if issue.RepoChangePending != "" || issue.RepoChangeReason != "" {
		t.Fatalf("the waiting change = %q because %q, want none: nothing changed", issue.RepoChangePending, issue.RepoChangeReason)
	}
	if messages := r.messages(); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing", messages)
	}
	if notes := r.repoNotes(); notes != 0 {
		t.Fatalf("the issue holds %d repository notes, want none", notes)
	}
}

// A correction onto a served repository whose default branch no admission has read is applied,
// and the record names that repository's real default branch: the preparation of a recorded
// issue's event reads it (prepareRecorded), so the resolution the engine makes inside the fact's
// own transaction, which performs no I/O, answers rather than failing the fact — and a fact that
// fails there is delivered again for ever. This rig is the runtime's own mapping and default
// branches over a GitHub stand-in, rather than the resolutions the tests above hold in memory.
func TestRepositoryCorrectionReadsTheDefaultBranchOfTheRepositoryItMovesTo(t *testing.T) {
	ctx := context.Background()
	r, w, branches := newRuntimeCorrectionRig(t)

	// The daemon has read the default branch of the repository the issue is recorded on, as the
	// admission that recorded it did, and of no other: acme/tools is served and unread.
	if _, err := branches.Named(ctx, ghrepo.MustParse("acme/widgets")); err != nil {
		t.Fatalf("read the default branch of the recorded repository: %v", err)
	}
	if _, read := branches.Kept("acme/tools"); read {
		t.Fatal("the default branch of acme/tools was read before the correction, which is the read this test proves the preparation makes")
	}
	event := intake.DispatchIssue{Key: correctionKey, Seq: 2, Type: "issue.updated", Status: "in_progress", Title: "root",
		Rank: "A", HandedOver: true, ResolvedRepo: "acme/tools"}
	prepared, err := w.prepareRepository(ctx, event)
	if err != nil {
		t.Fatalf("prepare the event that moves %s to acme/tools: %v", correctionKey, err)
	}
	applyFact(t, r.pool, "dispatch", "repository-unread-branch", prepared, r.engine, r.admission)

	issue := r.issue()
	r.corrected(issue, "acme/tools", "release/v7.1", "acme/tools", "acme/widgets")
	if messages := r.messages(); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing: its record followed Dispatch onto a repository Legion serves", messages)
	}
}

// The boot listing makes that read for a recorded issue of any status, not only for a
// listed todo: admission's reconcile has the engine follow Dispatch's resolution for every issue it
// holds a record of (admit's applySummary), so a listing that moves a running root onto a served
// repository whose default branch nobody read would otherwise fail that whole pass, every pass,
// and the root would stay on the repository it left. The issue here is the running root of a live
// tree, in in_progress, and the listing shows it at the sequence its record already holds, so
// nothing of it is held back for the Dispatch consumer: Dispatch's resolution is the only thing
// that moved.
func TestRepositoryCorrectionReadsTheDefaultBranchForARecordedIssueTheListingShowsOutOfTodo(t *testing.T) {
	ctx := context.Background()
	r, w, branches := newRuntimeCorrectionRig(t)
	w.consumers = correctionConsumers(t, r.pool)
	w.engine, w.admission = r.engine, r.admission

	// The daemon has read the default branch of the repository the issue is recorded on, and of no
	// other: acme/tools is served and unread.
	if _, err := branches.Named(ctx, ghrepo.MustParse("acme/widgets")); err != nil {
		t.Fatalf("read the default branch of the recorded repository: %v", err)
	}
	if _, read := branches.Kept("acme/tools"); read {
		t.Fatal("the default branch of acme/tools was read before the listing, which is the read this test proves the listing makes")
	}
	listing := []dispatch.IssueSummary{{Key: correctionKey, Title: "root", Status: "in_progress", Rank: "A",
		HandedOver: true, LastSeq: 1, ResolvedRepo: "acme/tools"}}
	if err := w.reconcileListing(ctx, listing, nil, false); err != nil {
		t.Fatalf("the listing that moves %s to acme/tools: %v", correctionKey, err)
	}

	issue := r.issue()
	r.corrected(issue, "acme/tools", "release/v7.1", "acme/tools", "acme/widgets")
	if messages := r.messages(); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing: its record followed Dispatch onto a repository Legion serves", messages)
	}
}

// newRuntimeCorrectionRig is a correctionRig whose engine resolves repositories through the
// runtime's own mapping and default branches (w) rather than through resolutions held in memory:
// acme/widgets, the project's default, and acme/tools are mapped, and a GitHub stand-in answers
// their default branches, main and release/v7.1. branches is w's, and has read neither yet.
func newRuntimeCorrectionRig(t *testing.T) (*correctionRig, *workflowRuntime, *defaultBranches) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	recordCorrectionRoot(t, pool, records)
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main", "acme/tools": "acme/tools@release/v7.1"})
	repos := newRepositories(correctionProject, ghrepo.Renames{})
	repos.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}, {Repo: ghrepo.MustParse("acme/tools")}}, nil)
	branches := newDefaultBranches(roleTokens{}, repos, github.url, defaultBranchTTL, time.Now)
	w := &workflowRuntime{pool: pool, records: records, repositories: repos, defaultBranches: branches,
		dispatchProject: correctionProject, log: quietLogger()}
	engine := workflow.New(records, workflow.Config{Project: correctionProject, Repositories: w, Steps: config.DesignGateOff.Steps(),
		Clock: func() time.Time { return correctionMoment }}, quietLogger())
	admission := admit.New(records, engine, 1, correctionProject, quietLogger())
	r := &correctionRig{t: t, pool: pool, records: records, engine: engine, admission: admission, generation: correctionGeneration}
	return r, w, branches
}

// correctionConsumers opens this test's own durable consumers, which the listing reconcile reads
// the notification stream's target and the Dispatch consumer's position from.
func correctionConsumers(t *testing.T, pool *pgxpool.Pool) *intake.Consumers {
	t.Helper()
	conn := testnats.Connect(t, workflowNATS(t))
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	consumers, err := intake.OpenConsumers(context.Background(), js, pool, intake.ConsumerSpec{
		Project: correctionProject, Repositories: []ghrepo.Repository{ghrepo.MustParse("acme/widgets")},
		AckWait: time.Second, NakDelay: time.Millisecond, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	return consumers
}
