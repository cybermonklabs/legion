package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The repository an issue's record is corrected onto is Dispatch's own fact, and these tests read
// what the daemon's passes over a recorded issue's resolution do with it: the live Dispatch event
// (the engine's dispatchIssue), the boot listing (admission's reconcile), which reads GitHub for
// no repository and so applies no change, and the follow after the boot (followListing), which
// applies the changes the boot's listing left. Each runs over a real PostgreSQL, as every test of
// this package does, and reads the record and the Dispatch messages the pass queued, never the
// statement that writes the record, which has its own test (record's
// TestCorrectIssueRepository...).

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

// correctionReadsOf is what the daemon's preparation of a fact moving a recorded issue onto each
// repository reads of GitHub (prepareRecorded, correctionReads), by the name Dispatch resolves the
// issue to: GitHub's name and default branch where both role Apps are installed on it, or the
// refusal GitHub's answer made where one is not. A repository it does not name is one whose reads
// GitHub did not answer, which carries neither, as does every fact no preparation read GitHub for:
// the boot's listing.
type correctionReadsOf map[string]correctionRead

// correctionRig is one test's daemon: the records, what its preparations read of GitHub, and the
// two handlers a Dispatch fact runs through, over a database of its own. Its issue is the running
// root of a live tree, recorded on acme/widgets, which is what makes a moved phase or generation
// visible: nothing in a correction may start, stop or re-run anything.
type correctionRig struct {
	t          *testing.T
	pool       *store.Pool
	reads      correctionReadsOf
	records    record.Store
	engine     *workflow.Engine
	admission  *admit.Admission
	generation uint64
}

// served is the read a preparation makes of repo where both role Apps are installed on it and
// GitHub names it at branch.
func served(repo, branch string) correctionRead {
	return correctionRead{read: &ghbranch.Repository{Name: ghrepo.MustParse(repo), DefaultBranch: branch}}
}

func newCorrectionRig(t *testing.T) *correctionRig {
	t.Helper()
	r := &correctionRig{
		t:    t,
		pool: isolatedOutboxPool(t),
		reads: correctionReadsOf{
			"acme/widgets": served("acme/widgets", "main"),
			"acme/tools":   served("acme/tools", "release/v7.1"),
		},
		records:    record.NewStore(),
		generation: correctionGeneration,
	}
	r.engine = workflow.New(r.records, workflow.Config{Project: correctionProject, Repositories: testRepositories, Steps: config.DesignGateOff.Steps(),
		Clock: func() time.Time { return correctionMoment }}, quietLogger())
	r.admission = admit.New(r.records, r.engine, 1, correctionProject, quietLogger())
	recordCorrectionRoot(t, r.pool, r.records, correctionKey, "root", 0)
	return r
}

// prepared is fact as the daemon's preparation leaves it (prepareRecorded): carrying what GitHub
// answered for the repository it resolves to, as reads holds it.
func (r *correctionRig) prepared(fact intake.DispatchIssue) intake.DispatchIssue {
	answer := r.reads[fact.ResolvedRepo]
	fact.RepositoryRead, fact.RepositoryRefusal = answer.read, answer.refusal
	if answer.refusal != "" {
		fact.RepositoryRefusalKind = intake.RefusedByGitHub
	}
	return fact
}

// recordCorrectionRoot records key, titled title, as the issue a test moves: the running root of a
// live tree holding slot, on acme/widgets at main, at Dispatch sequence 1, at the generation and in
// the phase a correction may not move, with nothing waiting and no correction.
func recordCorrectionRoot(t *testing.T, pool *store.Pool, records record.Store, key, title string, slot int) {
	t.Helper()
	ctx := context.Background()
	root := record.Issue{Key: key, Tree: key, Project: correctionProject, Title: title, Phase: phase.Implementing,
		Generation: correctionGeneration, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A", HandedOver: true, LastDispatchSeq: 1,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: slot, AdmittedAt: correctionMoment}); err != nil {
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
// prepared as the Dispatch consumer prepares it, through the pair of handlers the daemon applies a
// Dispatch fact through, the workflow then admission.
func (r *correctionRig) event(seq int64, resolved string) {
	r.t.Helper()
	r.move(seq, "in_progress", resolved)
}

// move applies one live Dispatch event of the issue at status, the way a person's own move of it
// reaches the daemon: prepared, then the workflow handler then admission's, in one fact.
func (r *correctionRig) move(seq int64, status, resolved string) {
	r.t.Helper()
	applyFact(r.t, r.pool, "dispatch", fmt.Sprintf("repository-%d", seq), r.prepared(intake.DispatchIssue{Key: correctionKey, Seq: seq, Type: "issue.updated",
		Status: status, Title: "root", Rank: "A", HandedOver: true, ResolvedRepo: resolved}), r.engine, r.admission)
}

// listing runs admission's boot pass over a Dispatch listing that shows the issue at seq on
// resolved, at the running status its record holds.
func (r *correctionRig) listing(seq int64, resolved string) {
	r.t.Helper()
	r.listingAt(seq, "in_progress", resolved)
}

// listingAt runs that pass over a listing that shows the issue at seq, at status, on resolved,
// with the consumer already caught up, so nothing is held back for it. The boot's listing reads
// GitHub for no repository (prepareRecorded), so its facts carry no read.
func (r *correctionRig) listingAt(seq int64, status, resolved string) {
	r.t.Helper()
	reconcileAdmission(r.t, r.pool, r.admission, []dispatch.IssueSummary{
		{Key: correctionKey, Title: "root", Status: status, Rank: "A", HandedOver: true, LastSeq: seq, ResolvedRepo: resolved},
	}, 0, intake.DispatchConsumerPosition{Idle: true})
}

// follow applies what the follow after a boot writes for the issue over a listing that shows it
// at seq on resolved (followListing): its single-issue correction, under the fact lock, from the
// read its preparation made of GitHub.
func (r *correctionRig) follow(seq int64, resolved string) {
	r.t.Helper()
	fact := r.prepared(intake.ListedIssue(dispatch.IssueSummary{Key: correctionKey, Title: "root", Status: "in_progress", Rank: "A",
		HandedOver: true, LastSeq: seq, ResolvedRepo: resolved}))
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return err
		}
		stored, err := r.records.Issue(ctx, tx, correctionKey)
		if err != nil {
			return err
		}
		return r.engine.ApplyRepositoryChange(ctx, tx, *stored, fact)
	}); err != nil {
		r.t.Fatalf("the follow onto %s: %v", resolved, err)
	}
}

// issue is the record as it stands.
func (r *correctionRig) issue() record.Issue {
	r.t.Helper()
	var issue *record.Issue
	ctx := context.Background()
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
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
func dispatchMessages(t *testing.T, pool *store.Pool) []string {
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
	if c := issue.RepoCorrection; c == nil || c.Before != before || !c.At.Equal(correctionMoment) {
		r.t.Fatalf("the correction = %+v, want %q left at %v", c, before, correctionMoment)
	}
	if issue.RepoChange != nil {
		r.t.Fatalf("the waiting change after the correction = %+v, want none: the correction applied it", issue.RepoChange)
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

// uncorrected fails unless the record is still on acme/widgets at main with no correction, and
// answers it.
func (r *correctionRig) uncorrected() record.Issue {
	r.t.Helper()
	issue := r.issue()
	if issue.Repo != "acme/widgets" || issue.DefaultBranch != "main" || issue.RepoCorrection != nil {
		r.t.Fatalf("the record is on %s at %s, corrected %+v; want it still on acme/widgets at main, corrected in nothing",
			issue.Repo, issue.DefaultBranch, issue.RepoCorrection)
	}
	return issue
}

// correctRecord moves key's record onto repo at branch, stamped at, as the workflow's own pass
// does when Dispatch names another repository for the issue
// (workflow.(*Engine).ApplyRepositoryChange), and fails t unless the record moved.
func correctRecord(t *testing.T, pool *store.Pool, records record.Store, key, repo, branch string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
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

// A live Dispatch event that resolves a recorded issue to another repository both role Apps are
// installed on corrects the record onto it there and then: the record names that repository, its
// default branch and GitHub's spelling of it, keeps the one the issue left with the moment it
// moved, and the issue's generation and phase do not move.
func TestRepositoryCorrectionFollowsALiveDispatchEvent(t *testing.T) {
	t.Parallel()
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

// A daemon that was down while the repository changed in Dispatch reads the change from its boot
// alone, with no event of it ever delivered. The boot's listing reads GitHub for no repository, so
// its pass applies nothing to the record and says nothing; the follow after the boot, whose
// preparation of the issue reads GitHub, corrects it.
func TestRepositoryCorrectionFollowsTheBootListing(t *testing.T) {
	t.Parallel()
	r := newCorrectionRig(t)
	r.listing(3, "acme/tools")
	if issue := r.uncorrected(); issue.LastDispatchSeq != 3 || issue.RepoChange != nil {
		t.Fatalf("after the boot's listing the record is at sequence %d, waiting on %+v; want the listing's 3 and nothing waiting",
			issue.LastDispatchSeq, issue.RepoChange)
	}
	r.follow(3, "acme/tools")
	r.corrected(r.issue(), "acme/tools", "release/v7.1", "acme/tools", "acme/widgets")
	if messages := r.messages(); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing: its record followed Dispatch", messages)
	}
}

// An issue out of the workflow whose repository changed in Dispatch meanwhile is re-admitted onto
// the repository Dispatch resolves it to now. The re-admission is the daemon's own
// (admission's readmit: a new generation, admitted, waiting for its slot), and the repository
// reaches the record through the move's live event (the engine's dispatchIssue), which follows
// Dispatch's resolution before admission re-admits in the same fact, or, where the boot's listing
// is the first read of the move, through the follow after that boot. dispatch://INFRA-2865
// measured the absence: a park and a re-admission gave a new generation on the old repository,
// because nothing re-resolved the repository, and the new run cloned the repository the issue had
// left.
func TestRepositoryCorrectionFollowsAReadmission(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		readmit func(*correctionRig)
	}{
		{"a live Dispatch event", func(r *correctionRig) { r.move(3, "todo", "acme/tools") }},
		{"the boot listing and the follow after it", func(r *correctionRig) {
			r.listingAt(3, "todo", "acme/tools")
			r.follow(3, "acme/tools")
		}},
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
			// The run the re-admission opens is the issue's next generation, and the status it
			// leaves is that run's start, not the todo the re-admission wrote. A live event's
			// re-admission reads GitHub for the change, so that start is on the corrected
			// repository; the boot's listing reads none, so admission promotes the root on the
			// repository it left, and the follow after the boot corrects it.
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
				t.Fatalf("the issue was told %q, want nothing: its record followed Dispatch onto a repository both Apps are installed on", messages)
			}
		})
	}
}

// gadgetsRefused is what a preparation reads of acme/gadgets while its review App is not
// installed on it: GitHub's refusal.
var gadgetsRefused = correctionRead{refusal: "the review App is not installed on acme/gadgets", kind: intake.RefusedByGitHub}

// gadgetsNote is what an issue whose change onto acme/gadgets waits for its review App is told.
const gadgetsNote = "Legion did not move this issue off acme/widgets: the review App is not installed on acme/gadgets. Install it on acme/gadgets, or set another repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name). Legion moves the issue at its next event or at Legion's next boot, once both Legion Apps are installed on acme/gadgets."

// A repository a role App is not installed on is not written onto the record: the record keeps
// the repository it has, the issue is told once however many passes read the same resolution, and
// the change waits on the record with the reason it waits. The boot's listing, which reads GitHub
// for nothing, writes nothing over it.
func TestRepositoryCorrectionTellsAnIssueWhoseNewRepositoryLegionCannotWork(t *testing.T) {
	t.Parallel()
	r := newCorrectionRig(t)
	r.reads["acme/gadgets"] = gadgetsRefused
	r.event(2, "acme/gadgets")
	r.event(3, "acme/gadgets")
	r.listing(4, "acme/gadgets")
	issue := r.uncorrected()
	if c := issue.RepoChange; c == nil || *c != (record.RepositoryChange{Pending: "acme/gadgets", Reason: "the review App is not installed on acme/gadgets"}) {
		t.Fatalf("the waiting change = %+v, want acme/gadgets and why it waits", c)
	}
	if messages := r.messages(); len(messages) != 1 || messages[0] != gadgetsNote {
		t.Fatalf("the issue was told %q, want it told once, saying %q", messages, gadgetsNote)
	}
}

// A resolution GitHub has not answered for writes nothing and says nothing: no correction, no
// waiting change and no note. The daemon's own preparation of such a fact asks for a follow of the
// issue (prepareRecorded, requestFollowOf), which reads GitHub again until it answers
// (TestACorrectionGitHubDidNotAnswerIsFollowedOnceGitHubAnswers).
func TestRepositoryCorrectionWritesNothingForARepositoryGitHubHasNotAnsweredFor(t *testing.T) {
	t.Parallel()
	r := newCorrectionRig(t)
	r.event(2, "stranger/tools")
	r.listing(3, "stranger/tools")
	r.follow(3, "stranger/tools")
	if issue := r.uncorrected(); issue.RepoChange != nil {
		t.Fatalf("the record waits on %+v, want nothing written", issue.RepoChange)
	}
	if messages := r.messages(); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing", messages)
	}
	if notes := r.repoNotes(); notes != 0 {
		t.Fatalf("the issue holds %d repository notes, want none", notes)
	}
}

// The waiting change is applied by the first pass whose preparation finds both role Apps
// installed on the repository, and the pair the record was carrying is cleared with it. Since
// dispatch://INFRA-4401 no read of a repository mapping reads the repository again in between, so
// that pass is the issue's next event or the follow after the next boot: each holds here. Nothing
// is said of the correction itself.
func TestRepositoryCorrectionAppliesTheWaitingChangeAtTheNextEventOrTheNextBoot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		apply func(*correctionRig)
	}{
		{"the issue's next event", func(r *correctionRig) { r.event(3, "acme/gadgets") }},
		{"the follow after the next boot", func(r *correctionRig) {
			r.listing(3, "acme/gadgets")
			r.follow(3, "acme/gadgets")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCorrectionRig(t)
			r.reads["acme/gadgets"] = gadgetsRefused
			r.event(2, "acme/gadgets")
			if change := r.issue().RepoChange; change == nil || change.Pending != "acme/gadgets" {
				t.Fatalf("the waiting change = %+v, want acme/gadgets before the App is installed", change)
			}
			r.reads["acme/gadgets"] = served("acme/gadgets", "trunk")
			tc.apply(r)
			r.corrected(r.issue(), "acme/gadgets", "trunk", "acme/gadgets", "acme/widgets")
			if notes := r.repoNotes(); notes != 1 {
				t.Fatalf("the issue holds %d repository notes, want the one the refusal wrote: the correction says nothing of its own", notes)
			}
			if messages := r.messages(); len(messages) != 1 {
				t.Fatalf("the issue was told %q, want only what it was told while the change waited", messages)
			}
		})
	}
}

// A resolution naming the repository the record already holds writes nothing and says nothing: no
// correction, no note, and no repository note row to say one was posted. That is the common pass,
// which every event and every listing of every recorded issue takes.
func TestRepositoryCorrectionWritesNothingWhenDispatchNamesTheRecordedRepository(t *testing.T) {
	t.Parallel()
	r := newCorrectionRig(t)
	r.event(2, "acme/widgets")
	r.listing(3, "acme/widgets")
	r.follow(3, "acme/widgets")
	if issue := r.uncorrected(); issue.RepoChange != nil {
		t.Fatalf("the waiting change = %+v, want none: nothing changed", issue.RepoChange)
	}
	if messages := r.messages(); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing", messages)
	}
	if notes := r.repoNotes(); notes != 0 {
		t.Fatalf("the issue holds %d repository notes, want none", notes)
	}
}

// A correction onto a repository whose default branch no admission has read is applied, and the
// record names that repository's real default branch: the preparation of a recorded issue's event
// reads both role Apps' installations and GitHub's name and default branch of it
// (prepareRecorded), so the resolution the engine makes inside the fact's own transaction, which
// performs no I/O, answers from that read. This rig is the runtime's own preparation over a GitHub
// stand-in, rather than the reads the tests above hold in memory.
func TestRepositoryCorrectionReadsTheDefaultBranchOfTheRepositoryItMovesTo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, w, branches := newRuntimeCorrectionRig(t)

	// The daemon has read the default branch of the repository the issue is recorded on, as the
	// admission that recorded it did, and of no other.
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
		t.Fatalf("the issue was told %q, want nothing: its record followed Dispatch onto a repository both Apps are installed on", messages)
	}
}

// The boot's listing reads GitHub for no repository, for a recorded issue of any status: a listing
// that moves a running root, in in_progress and at the sequence its record already holds, so that
// nothing of it is held back for the Dispatch consumer, applies nothing and asks for the follow
// after the boot instead (requestFollow). That follow, over Dispatch's listing, reads the
// repository and corrects the record, with Dispatch's resolution the only thing that moved.
func TestRepositoryCorrectionReadsTheDefaultBranchForARecordedIssueTheBootsListingShowsOutOfTodo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, w, branches := newRuntimeCorrectionRig(t)
	w.consumers = correctionConsumers(t, r.pool)
	w.engine, w.admission = r.engine, r.admission

	if _, err := branches.Named(ctx, ghrepo.MustParse("acme/widgets")); err != nil {
		t.Fatalf("read the default branch of the recorded repository: %v", err)
	}
	listing := []dispatch.IssueSummary{{Key: correctionKey, Title: "root", Status: "in_progress", Rank: "A",
		HandedOver: true, LastSeq: 1, ResolvedRepo: "acme/tools"}}
	w.dispatch.(*repositoryDispatch).listed = listing
	follows := w.followRequested.Load()
	if _, err := w.reconcileListing(ctx, listing, nil, true); err != nil {
		t.Fatalf("the boot's listing that moves %s to acme/tools: %v", correctionKey, err)
	}
	r.uncorrected()
	if _, read := branches.Kept("acme/tools"); read {
		t.Fatal("the boot's listing read acme/tools, want no read of GitHub at the boot")
	}
	if asked := w.followRequested.Load(); asked == follows {
		t.Fatal("the boot's listing asked for no follow, want one for the change it left")
	}
	if left, err := w.followListing(ctx, nil); err != nil || len(left) != 0 {
		t.Fatalf("the follow after the boot = %v, %v; want it to read every issue", left, err)
	}
	r.corrected(r.issue(), "acme/tools", "release/v7.1", "acme/tools", "acme/widgets")
	if messages := r.messages(); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing: its record followed Dispatch onto a repository both Apps are installed on", messages)
	}
}

// newRuntimeCorrectionRig is a correctionRig whose engine resolves repositories through the
// runtime's own preparation (w) rather than through reads held in memory: both role Apps are
// installed on every repository, and a GitHub stand-in answers the default branches of acme/widgets
// and acme/tools, main and release/v7.1. branches is w's, and has read neither yet.
func newRuntimeCorrectionRig(t *testing.T) (*correctionRig, *workflowRuntime, *defaultBranches) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	recordCorrectionRoot(t, pool, records, correctionKey, "root", 0)
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main", "acme/tools": "acme/tools@release/v7.1"})
	repos := newRepositories(correctionProject, ghrepo.Renames{})
	branches := newDefaultBranches(roleTokens{}, repos, github.url, defaultBranchTTL, time.Now)
	w := &workflowRuntime{pool: pool, records: records, repositories: repos, defaultBranches: branches, installs: &installations{},
		dispatch: &repositoryDispatch{}, dispatchProject: correctionProject, log: quietLogger()}
	engine := workflow.New(records, workflow.Config{Project: correctionProject, Repositories: w, Steps: config.DesignGateOff.Steps(),
		Clock: func() time.Time { return correctionMoment }}, quietLogger())
	admission := admit.New(records, engine, 1, correctionProject, quietLogger())
	r := &correctionRig{t: t, pool: pool, records: records, engine: engine, admission: admission, generation: correctionGeneration}
	return r, w, branches
}

// correctionConsumers opens this test's own durable consumers, which the listing reconcile reads
// the notification stream's target and the Dispatch consumer's position from.
func correctionConsumers(t *testing.T, pool *store.Pool) *intake.Consumers {
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
