package daemon

// A recorded issue Dispatch moves onto another repository. Since dispatch://INFRA-4401 the daemon
// reads no repository mapping: every repository both role Apps are installed on records an issue,
// read from GitHub at each preparation (prepareRead), and a recorded issue's record follows
// Dispatch onto another repository by its own reads at that preparation (prepareRecorded,
// correctionReads), always, whatever the repository state keeps (repositories.recordRepository),
// and once per repository in a follow pass (readCorrection): both Apps installed, the correction
// applies at the name and default branch GitHub answered then
// (intake.DispatchIssue.RepositoryRead) through the one writer
// (workflow.Engine.ApplyRepositoryChange, record.Store.CorrectIssueRepository); a role App not
// installed is a refusal the preparation carries on the fact
// (intake.DispatchIssue.RepositoryRefusal), and the change waits on the record with it; a read
// GitHub did not answer, a read the preparation's bound cut short, or a fact no preparation read
// GitHub for, applies nothing and says nothing, and a follow reads the issue again until GitHub
// answers (requestFollow, requestFollowOf, followRecorded). A waiting change is applied at the
// issue's next event or at the next boot, whatever the repository: nothing reads it again in
// between. These tests run the daemon's own preparation and handlers over a real PostgreSQL and
// NATS, with a GitHub that names strangerRepo as strangerNamed, a repository no record names
// before a test's own.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// onStranger fails unless key's record is on strangerNamed at branch, GitHub's own spelling and the
// default branch GitHub answered, having left acme/widgets, with nothing waiting and its generation
// where it was.
func onStranger(t *testing.T, w *workflowRuntime, key, branch string) {
	t.Helper()
	got := recordedIssue(t, w, key)
	if got == nil || got.Repo != strangerNamed || got.GitHubRepo != strangerNamed || got.DefaultBranch != branch {
		t.Fatalf("%s = %+v, want it on %s at %s, GitHub's own spelling and default branch", key, got, strangerNamed, branch)
	}
	if got.RepoCorrection == nil || got.RepoCorrection.Before != "acme/widgets" {
		t.Fatalf("the correction of %s = %+v, want acme/widgets left", key, got.RepoCorrection)
	}
	if got.RepoChange != nil {
		t.Fatalf("%s still waits on %+v, want nothing waiting", key, got.RepoChange)
	}
	if got.Generation != correctionGeneration {
		t.Fatalf("%s is at generation %d, want %d: a correction takes no new generation", key, got.Generation, correctionGeneration)
	}
}

// onWidgets fails unless key's record is still on acme/widgets at main, uncorrected, and answers
// it.
func onWidgets(t *testing.T, w *workflowRuntime, key string) record.Issue {
	t.Helper()
	got := recordedIssue(t, w, key)
	if got == nil || got.Repo != "acme/widgets" || got.DefaultBranch != "main" || got.RepoCorrection != nil {
		t.Fatalf("%s = %+v, want it still on acme/widgets at main, uncorrected", key, got)
	}
	return *got
}

// waitingNote is what an issue whose change onto strangerRepo waits for role's App is told.
func waitingNote(role string) string {
	return "Legion did not move this issue off acme/widgets: the " + role + " App is not installed on " + strangerRepo + ". Install it on " + strangerRepo +
		", or set another repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name). " +
		"Legion moves the issue at its next event or at Legion's next boot, once both Legion Apps are installed on " + strangerRepo + "."
}

// changingGitHub is a GitHub that names the rig's two anchors' repositories at main, answeredRepo
// as answeredNamed at main, and strangerRepo as strangerNamed at the default branch it holds now,
// which a test changes (rename), counting the reads of strangerRepo's name and default branch
// (reads).
type changingGitHub struct {
	branch atomic.Pointer[string]
	reads  atomic.Int64
}

// answeredRepo is a second repository no record names, which GitHub names as answeredNamed and
// both Apps are installed on unless a test says otherwise.
const (
	answeredRepo  = "kappa/gizmos"
	answeredNamed = "Kappa/Gizmos"
)

func newChangingGitHub(t *testing.T, branch string) (*changingGitHub, string) {
	t.Helper()
	g := &changingGitHub{}
	g.rename(branch)
	url := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		var name, head string
		switch strings.ToLower(strings.TrimPrefix(r.URL.Path, "/repos/")) {
		case "acme/widgets":
			name, head = "acme/widgets", "main"
		case "zeta/tools":
			name, head = "zeta/tools", "main"
		case answeredRepo:
			name, head = answeredNamed, "main"
		case strangerRepo:
			g.reads.Add(1)
			name, head = strangerNamed, *g.branch.Load()
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"full_name":"` + name + `","default_branch":"` + head + `"}`))
	}})
	return g, url
}

// rename has GitHub answer strangerRepo's default branch as branch from now on.
func (g *changingGitHub) rename(branch string) { g.branch.Store(&branch) }

// changingRig is repositoryRigWith over a changingGitHub whose strangerRepo starts at trunk.
func changingRig(t *testing.T, installs *installations, opts ...func(*workflowRuntime)) (*workflowRuntime, *repositoryDispatch, *changingGitHub) {
	t.Helper()
	github, url := newChangingGitHub(t, "trunk")
	w, client, _, _ := repositoryRigWith(t, installs, append([]func(*workflowRuntime){func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, url, defaultBranchTTL, time.Now)
	}}, opts...)...)
	return w, client, github
}

// keepAnEntry admits LEGION-1100, a root handed to Legion on strangerRepo, which has the daemon
// keep its read of the repository at trunk (the repository state, repositories.recordRepository,
// and defaultBranches' own read), and fails unless both hold it: the kept read the corrections
// below must not write.
func keepAnEntry(t *testing.T, w *workflowRuntime, client *repositoryDispatch) {
	t.Helper()
	deliver(t, w, client, issue("LEGION-1100", strangerRepo, "todo", true, "", 1))
	if got := recordedIssue(t, w, "LEGION-1100"); got == nil || got.Repo != strangerNamed || got.DefaultBranch != "trunk" {
		t.Fatalf("LEGION-1100 = %+v, want it admitted on %s at trunk, which keeps the read", got, strangerNamed)
	}
	if read, held := w.repositories.lookup(strangerRepo); !held || read.DefaultBranch != "trunk" {
		t.Fatalf("lookup(%s) = %+v (%t), want the repository state to keep it at trunk", strangerRepo, read, held)
	}
	if kept, read := w.defaultBranches.Kept(strangerRepo); !read || kept.DefaultBranch != "trunk" {
		t.Fatalf("defaultBranches keeps %+v (%v) for %s, want trunk", kept, read, strangerRepo)
	}
}

// A live event that moves a recorded issue onto a repository both role Apps are installed on
// corrects the record onto it there and then, at GitHub's own spelling and default branch, with
// nothing said on the issue: the reads are the preparation's own, made as an admission onto the
// repository makes them. GitHub intake hears the repository from the correction's commit, as it
// does from a recording's. Every correction reads GitHub itself, so a second issue moved onto the
// same repository reads it again rather than taking the read the first kept.
func TestRepositoryCorrectionFollowsAnEventOntoARepositoryBothAppsAreInstalledOn(t *testing.T) {
	t.Parallel()
	w, client, installs, stream := preparationRig(t, &installations{})
	const first, second = "LEGION-1001", "LEGION-1002"
	recordCorrectionRoot(t, w.pool, w.records, first, first, 0)
	recordCorrectionRoot(t, w.pool, w.records, second, second, 1)
	heard := "notifications.github." + strings.ReplaceAll(strangerNamed, "/", ".") + ".>"

	deliver(t, w, client, issue(first, strangerRepo, "in_progress", true, "", 2))
	onStranger(t, w, first, "trunk")
	if notes := repositoryNotes(t, w, first); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none: its record followed Dispatch onto a repository both Apps are installed on", first, notes)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, heard) {
		t.Fatalf("GitHub filters = %v, want %s heard from the correction that names it", filters, heard)
	}

	deliver(t, w, client, issue(second, strangerRepo, "in_progress", true, "", 2))
	onStranger(t, w, second, "trunk")
	for _, role := range []string{"implement " + strangerRepo, "review " + strangerRepo} {
		if reads := installs.reads(role); reads != 2 {
			t.Fatalf("the two corrections made %d reads of %q, want one each: a correction writes no kept read", reads, role)
		}
	}
}

// A recorded issue Dispatch moves onto a repository a role App is not installed on keeps its
// record's repository, the change waiting on the record with GitHub's refusal, the one the
// preparation carried on the fact, and the issue told once however many events read the same
// resolution, and told that the change is applied at its next event or at the next boot. Nothing
// of the refusal is kept, so the event after the App is installed reads GitHub again and applies
// the change, with no restart. Before dispatch://INFRA-4401 a read of the repository mapping that
// served such a repository applied a change waiting on it with no event; nothing reads a
// repository no record names again now, so the next event is what applies it.
func TestRepositoryCorrectionWaitsWhileARoleAppIsNotInstalledAndTheNextEventAppliesIt(t *testing.T) {
	t.Parallel()
	installs := &installations{}
	installs.setMissing(true, "review "+strangerRepo)
	w, client, installs, _ := preparationRig(t, installs)
	const key = "LEGION-1011"
	recordCorrectionRoot(t, w.pool, w.records, key, key, 0)

	deliver(t, w, client, issue(key, strangerRepo, "in_progress", true, "", 2))
	deliver(t, w, client, issue(key, strangerRepo, "in_progress", true, "", 3))
	got := onWidgets(t, w, key)
	want := record.RepositoryChange{Pending: strangerRepo, Reason: "the review App is not installed on " + strangerRepo}
	if got.RepoChange == nil || *got.RepoChange != want {
		t.Fatalf("the waiting change of %s = %+v, want %+v", key, got.RepoChange, want)
	}
	if notes := repositoryNotes(t, w, key); len(notes) != 1 || notes[0] != waitingNote("review") {
		t.Fatalf("notes on %s = %q, want it told once, saying %q", key, notes, waitingNote("review"))
	}
	if reads := installs.reads("review " + strangerRepo); reads != 2 {
		t.Fatalf("the two events made %d reads of the review App's installation, want one each: no refusal is kept", reads)
	}

	installs.setMissing(false, "review "+strangerRepo)
	deliver(t, w, client, issue(key, strangerRepo, "in_progress", true, "", 4))
	onStranger(t, w, key, "trunk")
}

// A correction writes GitHub's answer at the correction and never the read the daemon kept of the
// repository at an earlier preparation (the repository state, repositories.recordRepository, and
// the default-branch read defaultBranches holds within its TTL): an admission on strangerRepo
// keeps that read at trunk first, then GitHub's answer changes, and the issue is corrected. A
// default branch renamed since is the one the record takes; an App uninstalled since leaves the
// change waiting with GitHub's reason and the record where it was, as for an App never installed;
// a read GitHub does not answer writes nothing, while its event is applied all the same, the
// record taking its sequence; and a hand-over, whose preparation (prepareHandover) reads GitHub
// at the hand-over, takes the renamed branch as an event does.
func TestRepositoryCorrectionWritesGitHubsAnswerAtTheCorrection(t *testing.T) {
	t.Parallel()
	const key = "LEGION-1041"
	for _, tc := range []struct {
		name string
		// change is what changes on GitHub after the read was kept.
		change func(github *changingGitHub, installs *installations)
		// correct moves key onto strangerRepo as the row's pass does.
		correct func(t *testing.T, w *workflowRuntime, client *repositoryDispatch)
		// branch is the default branch the record is corrected at, "" for a record left on
		// acme/widgets; waits is the change left waiting there, nil for none.
		branch string
		waits  *record.RepositoryChange
		note   string
		// seq is the Dispatch sequence the record holds once the row's event is applied, 0 where
		// the row does not read it: an event whose correction writes nothing is applied all the
		// same, so a redelivery of it stops at the sequence fence.
		seq int64
	}{
		{name: "a default branch renamed since the read was kept",
			change:  func(github *changingGitHub, _ *installations) { github.rename("release/v2") },
			correct: deliverCorrection,
			branch:  "release/v2"},
		{name: "an App uninstalled since the read was kept",
			change:  func(_ *changingGitHub, installs *installations) { installs.setMissing(true, "review "+strangerRepo) },
			correct: deliverCorrection,
			waits:   &record.RepositoryChange{Pending: strangerRepo, Reason: "the review App is not installed on " + strangerRepo},
			note:    waitingNote("review")},
		{name: "a read GitHub does not answer",
			change: func(_ *changingGitHub, installs *installations) {
				installs.setFailing(errors.New("GitHub answered 502"), "implement "+strangerRepo)
			},
			correct: deliverCorrection,
			seq:     2},
		{name: "a hand-over the outbox prepares",
			change:  func(github *changingGitHub, _ *installations) { github.rename("release/v2") },
			correct: handOverCorrection,
			branch:  "release/v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installs := &installations{}
			w, client, github := changingRig(t, installs, quickInstallReads)
			recordCorrectionRoot(t, w.pool, w.records, key, key, 0)
			keepAnEntry(t, w, client)
			tc.change(github, installs)

			tc.correct(t, w, client)
			if tc.branch != "" {
				onStranger(t, w, key, tc.branch)
				return
			}
			got := onWidgets(t, w, key)
			if (got.RepoChange == nil) != (tc.waits == nil) || got.RepoChange != nil && *got.RepoChange != *tc.waits {
				t.Fatalf("the waiting change of %s = %+v, want %+v", key, got.RepoChange, tc.waits)
			}
			notes := repositoryNotes(t, w, key)
			if tc.note == "" && len(notes) != 0 || tc.note != "" && (len(notes) != 1 || notes[0] != tc.note) {
				t.Fatalf("notes on %s = %q, want %q", key, notes, tc.note)
			}
			if tc.seq != 0 && got.LastDispatchSeq != tc.seq {
				t.Fatalf("%s is at Dispatch sequence %d, want %d: the event applies though its correction writes nothing",
					key, got.LastDispatchSeq, tc.seq)
			}
		})
	}
}

// deliverCorrection delivers the live event that moves LEGION-1041 onto strangerRepo, prepared as
// the Dispatch consumer prepares it.
func deliverCorrection(t *testing.T, w *workflowRuntime, client *repositoryDispatch) {
	t.Helper()
	deliver(t, w, client, issue("LEGION-1041", strangerRepo, "in_progress", true, "", 2))
}

// handOverCorrection applies the correction the outbox's hand-over applies: its preparation
// (prepareHandover, which the outbox runs outside the fact lock) and then the engine's pass over
// the fact admission's consumeHandover builds from it, Dispatch's resolution with GitHub's read.
func handOverCorrection(t *testing.T, w *workflowRuntime, _ *repositoryDispatch) {
	t.Helper()
	ctx := context.Background()
	handover, err := w.prepareHandover(ctx, intake.Handover{Issue: "LEGION-1041", ResolvedRepo: strangerRepo})
	if err != nil {
		t.Fatalf("prepare the hand-over: %v", err)
	}
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		stored, err := w.records.Issue(ctx, tx, "LEGION-1041")
		if err != nil {
			return err
		}
		return w.engine.ApplyRepositoryChange(ctx, tx, *stored, intake.DispatchIssue{
			Key:                   "LEGION-1041",
			ResolvedRepo:          handover.ResolvedRepo,
			RepositoryRead:        handover.RepositoryRead,
			RepositoryRefusal:     handover.RepositoryRefusal,
			RepositoryRefusalKind: handover.RepositoryRefusalKind,
		})
	}); err != nil {
		t.Fatalf("apply the hand-over's correction: %v", err)
	}
}

// A correction whose reads GitHub does not answer writes nothing and says nothing, and its issue
// is owed a follow (requestFollowOf) that the refresher makes at once, no other follow being owed
// here, and again on its backoff (reads().retry) while GitHub still does not answer, each pass
// reading the repository again and writing nothing: the record stays where it was, with nothing
// waiting and nothing said. Once GitHub answers, the follow corrects the record onto GitHub's
// spelling and default branch, with no further event of the issue and no boot, as any correction
// does, and the follow is settled. A live event and a hand-over owe it alike. The repository is
// one no record names, which nothing else reads again. Control: with no follow owed for such a
// correction, nothing in the running process read the change again, and the record stayed on the
// repository the issue left until the issue's next event or the next boot.
func TestACorrectionGitHubDidNotAnswerIsFollowedOnceGitHubAnswers(t *testing.T) {
	t.Parallel()
	const key = "LEGION-1041"
	for _, tc := range []struct {
		name string
		// correct moves key onto strangerRepo as the row's pass does.
		correct func(t *testing.T, w *workflowRuntime, client *repositoryDispatch)
		// seq is the Dispatch sequence the record holds once the row's pass is applied: an
		// event's is applied though its correction writes nothing, and a hand-over carries none.
		seq int64
	}{
		{name: "a live event", correct: deliverCorrection, seq: 2},
		{name: "a hand-over the outbox prepares", correct: handOverCorrection, seq: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installs := &installations{}
			implement := "implement " + strangerRepo
			installs.setFailing(errors.New("GitHub answered 502"), implement)
			w, client, _ := changingRig(t, installs, quickInstallReads)
			recordCorrectionRoot(t, w.pool, w.records, key, key, 0)
			client.show(issue(key, strangerRepo, "in_progress", true, "", 2))

			tc.correct(t, w, client)
			read := installs.reads(implement)
			if !testwait.Holds(testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll, func() bool { return installs.reads(implement) >= read+2 }) {
				t.Errorf("%s's implement App was read %d times in all, %d of them after the correction, want the follow to read it again "+
					"on its backoff while GitHub does not answer", strangerRepo, installs.reads(implement), installs.reads(implement)-read)
			}
			got := onWidgets(t, w, key)
			if got.RepoChange != nil {
				t.Fatalf("%s waits on %+v, want nothing waiting: a read GitHub did not answer is no refusal", key, got.RepoChange)
			}
			if notes := repositoryNotes(t, w, key); len(notes) != 0 {
				t.Fatalf("notes on %s = %q, want none: a read GitHub did not answer says nothing", key, notes)
			}
			if got.LastDispatchSeq != tc.seq {
				t.Fatalf("%s is at Dispatch sequence %d, want %d", key, got.LastDispatchSeq, tc.seq)
			}

			installs.setFailing(nil, implement)
			testwait.Eventually(t, key+" corrected onto "+strangerNamed+" with no event of it and no boot", func() bool {
				got := recordedIssue(t, w, key)
				return got != nil && got.Repo == strangerNamed
			})
			onStranger(t, w, key, "trunk")
			if notes := repositoryNotes(t, w, key); len(notes) != 0 {
				t.Fatalf("notes on %s = %q, want none: its record followed Dispatch onto a repository both Apps are installed on", key, notes)
			}
			testwait.Eventually(t, "the follow settled once GitHub answered", func() bool {
				return w.followApplied.Load() >= w.followRequested.Load()
			})
		})
	}
}

// A follow owed for one issue whose correction GitHub does not answer goes on reading it, and a
// follow asked for another issue while it is owed reads that issue too: the passes made for the
// first are not narrowed to it. Each issue is corrected once GitHub answers for the repository it
// moves onto, the one whose repository answers first while the other's still does not, with no
// event of either after the one GitHub did not answer, and the follow is settled once both are.
func TestAFollowOwedForOneIssueReadsAnotherAskedForWhileItIsOwed(t *testing.T) {
	t.Parallel()
	const first, second = "LEGION-1091", "LEGION-1092"
	installs := &installations{}
	stranger, answered := "implement "+strangerRepo, "implement "+answeredRepo
	installs.setFailing(errors.New("GitHub answered 502"), stranger, answered)
	w, client, _ := changingRig(t, installs, quickInstallReads)
	recordCorrectionRoot(t, w.pool, w.records, first, first, 0)
	recordCorrectionRoot(t, w.pool, w.records, second, second, 1)

	deliver(t, w, client, issue(first, strangerRepo, "in_progress", true, "", 2))
	read := installs.reads(stranger)
	testwait.Eventually(t, "the follow owed for "+first+" reading "+strangerRepo+" again", func() bool { return installs.reads(stranger) >= read+2 })
	deliver(t, w, client, issue(second, answeredRepo, "in_progress", true, "", 2))
	installs.setFailing(nil, answered)
	testwait.Eventually(t, second+" corrected onto "+answeredNamed+" with no event of it", func() bool {
		got := recordedIssue(t, w, second)
		return got != nil && got.Repo == answeredNamed
	})
	if got := recordedIssue(t, w, second); got.DefaultBranch != "main" || got.RepoCorrection == nil || got.RepoCorrection.Before != "acme/widgets" {
		t.Fatalf("%s = %+v, want it on %s at main, having left acme/widgets", second, got, answeredNamed)
	}
	if got := onWidgets(t, w, first); got.RepoChange != nil {
		t.Fatalf("%s waits on %+v, want nothing waiting: GitHub still does not answer for %s", first, got.RepoChange, strangerRepo)
	}
	if applied, asked := w.followApplied.Load(), w.followRequested.Load(); applied >= asked {
		t.Fatalf("the follow is applied (%d of %d) with %s unread, want it owed for that issue", applied, asked, first)
	}

	installs.setFailing(nil, stranger)
	testwait.Eventually(t, "the follow settled once GitHub answered for both repositories", func() bool {
		return w.followApplied.Load() >= w.followRequested.Load()
	})
	onStranger(t, w, first, "trunk")
	for _, key := range []string{first, second} {
		if notes := repositoryNotes(t, w, key); len(notes) != 0 {
			t.Fatalf("notes on %s = %q, want none", key, notes)
		}
	}
}

// An issue a follow is owed for, whose reads GitHub still does not answer, starts no follow pass
// when it is asked for again (requestFollowOf), as each event or hand-over of it asks: the pass
// that left it owed armed the follow's own clock (reads().retry's first wait, 30 s in production,
// productionRepositoryReads), and the issue waits for that. Each pass reads the records,
// Dispatch's whole listing and GitHub, so through a GitHub outage a pass at each ask would cost
// that much per event. The ask is counted all the same, and an issue no pass owes yet is followed
// at once. A pass is counted by its read of strangerRepo's implement App, which every pass here
// makes once and nothing else makes. Control: with every ask waking the refresher, the second ask
// of the owed issue starts a pass at once.
func TestAnAskOfAnIssueAFollowIsOwedForWaitsTheFollowsClock(t *testing.T) {
	t.Parallel()
	const owed, added = "LEGION-1096", "LEGION-1097"
	installs := &installations{}
	implement := "implement " + strangerRepo
	installs.setFailing(errors.New("GitHub answered 502"), implement)
	w, client, _ := changingRig(t, installs)
	for i, key := range []string{owed, added} {
		recordCorrectionRoot(t, w.pool, w.records, key, key, i)
	}
	client.show(issue(owed, strangerRepo, "in_progress", true, "", 1), issue(added, strangerRepo, "in_progress", true, "", 1))
	owes := func(key string) bool {
		w.followMu.Lock()
		defer w.followMu.Unlock()
		return w.followOwed[key]
	}

	w.requestFollowOf(owed)
	testwait.Eventually(t, "a pass leaving "+owed+" owed, GitHub not having answered", func() bool {
		return installs.reads(implement) >= 1 && owes(owed)
	})
	read, asked := installs.reads(implement), w.followRequested.Load()
	w.requestFollowOf(owed)
	if follows := w.followRequested.Load() - asked; follows != 1 {
		t.Fatalf("the second ask of %s counted %d follows, want one", owed, follows)
	}
	if testwait.Holds(2*time.Second, testwait.Poll, func() bool { return installs.reads(implement) > read }) {
		t.Fatalf("the second ask of %s started a pass at once (%d reads of %s after it), want none before the follow's clock",
			owed, installs.reads(implement)-read, strangerRepo)
	}

	// The wait ends well inside the clock's 30 s, so the pass it sees is the ask's own.
	w.requestFollowOf(added)
	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "a pass at once for "+added+", which no pass owed", func() bool {
		return installs.reads(implement) > read
	})
}

// The boot's listing reads GitHub for no repository, a recorded issue's change included: it runs
// inside the boot's bounded reconcile, so the change applies nothing there, whatever read the
// daemon keeps of the repository, and the boot asks for the follow instead (requestFollow), which
// the refresher makes once the boot is over and whose own preparation of the issue reads GitHub
// and corrects the record at GitHub's answer then, with no event of the issue. The read kept here
// is at trunk while GitHub now answers release/v2: a boot that wrote the kept read would leave the
// record at trunk, which the follow would then find on the repository Dispatch names and leave.
// This is the next boot that applies a waiting change, as the next event does.
func TestTheBootsListingLeavesARepositoryCorrectionToTheFollowAfterIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	installs := &bootCountedInstallations{installations: &installations{}}
	w, client, github := changingRig(t, installs.installations, func(w *workflowRuntime) { w.installs = installs })
	w.bootID = "correction-boot"
	const key = "LEGION-1031"
	recordCorrectionRoot(t, w.pool, w.records, key, key, 0)
	keepAnEntry(t, w, client)
	github.rename("release/v2")
	client.show(issue(key, strangerRepo, "in_progress", true, "", 1))
	follows := w.followRequested.Load()
	before := installs.reads("implement " + strangerRepo)

	if err := w.reconcile(context.WithValue(ctx, bootReconcile{}, true)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if reads := installs.atBoot.Load(); reads != 0 {
		t.Fatalf("the boot's listing made %d installation reads, want none: it runs inside the boot's bounded reconcile", reads)
	}
	if asked := w.followRequested.Load(); asked == follows {
		t.Fatal("the boot's listing asked for no follow, want one for the change it left unread")
	}
	testwait.Eventually(t, key+" corrected onto "+strangerNamed+" by the follow after the boot", func() bool {
		got := recordedIssue(t, w, key)
		return got != nil && got.Repo == strangerNamed
	})
	onStranger(t, w, key, "release/v2")
	if notes := repositoryNotes(t, w, key); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none", key, notes)
	}
	if reads := installs.reads("implement "+strangerRepo) - before; reads != 1 {
		t.Fatalf("%s's implement App was read %d times after the boot began, want once, by the follow", strangerRepo, reads)
	}
}

// slowInstallations is installations whose reads of strangerRepo GitHub answers after wait each,
// or, while hang is set, not before the read's context ends, which it counts in cut: a GitHub that
// answers that repository slowly, or not within a pass's bound.
type slowInstallations struct {
	*installations
	wait time.Duration
	hang atomic.Bool
	cut  atomic.Int64
}

func (i *slowInstallations) InstalledOn(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	if !strings.EqualFold(repo.String(), strangerRepo) {
		return i.installations.InstalledOn(ctx, role, repo)
	}
	var answered <-chan time.Time
	if !i.hang.Load() {
		answered = time.After(i.wait)
	}
	select {
	case <-answered:
		return i.installations.InstalledOn(ctx, role, repo)
	case <-ctx.Done():
		i.cut.Add(1)
		return false, ctx.Err()
	}
}

func (i *slowInstallations) InstalledUnder(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	return i.InstalledOn(ctx, role, repo)
}

// A follow pass (followListing) that moves fifteen recorded issues onto one repository, as a
// root's move moves every child that inherits its repository, reads that repository once: the
// first issue's preparation reads both Apps' installations and GitHub's name and default branch,
// every other issue of the pass takes that read, and each is corrected onto GitHub's spelling and
// default branch from it. The pass runs within one bound (the App-call attempt bound, callBounds, two seconds here), and
// GitHub answers each installation read of the repository in a tenth of a second: read once per
// issue, one after another, the reads would use the bound up two thirds of the way through the
// issues, and every read after it would fail on the expired context. The follow is asked for as a
// boot's listing asks for it (requestFollow).
func TestAFollowReadsARepositoryOnceForEveryIssueItMovesOntoIt(t *testing.T) {
	// Not parallel: the follow's reads of fifteen issues must fit calls.attempt (two seconds) of real
	// time, which the load of the package's parallel tests on a 4-CPU runner can take from them.
	calls := productionAppCalls()
	calls.attempt = 2 * time.Second
	installs := &slowInstallations{installations: &installations{}, wait: 100 * time.Millisecond}
	w, client, github := changingRig(t, installs.installations, func(w *workflowRuntime) { w.installs, w.appCalls = installs, &calls })
	keys := make([]string, 15)
	for i := range keys {
		keys[i] = fmt.Sprintf("LEGION-%d", 1051+i)
		recordCorrectionRoot(t, w.pool, w.records, keys[i], keys[i], i)
		client.show(issue(keys[i], strangerRepo, "in_progress", true, "", 1))
	}

	w.requestFollow()
	follows := w.followRequested.Load()
	applied := testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), testwait.Poll, func() bool { return w.followApplied.Load() >= follows })
	for _, role := range []string{"implement " + strangerRepo, "review " + strangerRepo} {
		if reads := installs.reads(role); reads != 1 {
			t.Errorf("the follow made %d reads of %q for %d issues, want one: every issue of the pass takes the first one's", reads, role, len(keys))
		}
	}
	if reads := github.reads.Load(); reads != 1 {
		t.Errorf("the follow read %s's name and default branch %d times for %d issues, want once", strangerRepo, reads, len(keys))
	}
	if !applied {
		t.Fatalf("the follow never committed: each of its passes ran out of its bound of %s", calls.attempt)
	}
	for _, key := range keys {
		onStranger(t, w, key, "trunk")
		if notes := repositoryNotes(t, w, key); len(notes) != 0 {
			t.Fatalf("notes on %s = %q, want none: its record followed Dispatch onto a repository both Apps are installed on", key, notes)
		}
	}
}

// A follow pass whose bound ends while it reads a repository writes nothing for any issue moved
// onto it: the read the bound cut short answered for the bound, not for GitHub, so it is no refusal
// for a change to wait with, nothing is said on any issue, and the repository is not left unread,
// as it is for a read GitHub does not answer, which would have the refresher recover a repository
// GitHub never failed. The follow stays owed, and the pass after GitHub answers corrects every
// issue onto GitHub's spelling and default branch.
func TestAFollowWhoseBoundEndsWhileItReadsARepositoryWritesNothingAndIsMadeAgain(t *testing.T) {
	// Not parallel: the pass after GitHub answers must read and correct three issues within
	// calls.attempt (half a second) of real time, which the load of the package's parallel tests on a
	// 4-CPU runner can take from it.
	calls := productionAppCalls()
	calls.attempt = 500 * time.Millisecond
	installs := &slowInstallations{installations: &installations{}}
	installs.hang.Store(true)
	w, client, _ := changingRig(t, installs.installations, quickInstallReads, func(w *workflowRuntime) { w.installs, w.appCalls = installs, &calls })
	keys := []string{"LEGION-1071", "LEGION-1072", "LEGION-1073"}
	for i, key := range keys {
		recordCorrectionRoot(t, w.pool, w.records, key, key, i)
		client.show(issue(key, strangerRepo, "in_progress", true, "", 1))
	}

	w.requestFollow()
	follows := w.followRequested.Load()
	testwait.Eventually(t, "two passes of the follow cut short by their bound", func() bool { return installs.cut.Load() >= 2 })
	if applied := w.followApplied.Load(); applied >= follows {
		t.Fatalf("the follow is applied (%d of %d) with every pass cut short, want it owed", applied, follows)
	}
	for _, key := range keys {
		if got := onWidgets(t, w, key); got.RepoChange != nil {
			t.Fatalf("%s waits on %+v, want nothing waiting: a read its bound cut short is no refusal", key, got.RepoChange)
		}
		if notes := repositoryNotes(t, w, key); len(notes) != 0 {
			t.Fatalf("notes on %s = %q, want none: a read its bound cut short says nothing", key, notes)
		}
	}
	if waiting := w.unreadReasons(); len(waiting) != 0 {
		t.Fatalf("the daemon leaves %q unread, want nothing: GitHub did not fail to answer, the pass's bound ended", waiting)
	}

	installs.hang.Store(false)
	testwait.Eventually(t, "the follow applied once GitHub answers", func() bool { return w.followApplied.Load() >= follows })
	for _, key := range keys {
		onStranger(t, w, key, "trunk")
		if notes := repositoryNotes(t, w, key); len(notes) != 0 {
			t.Fatalf("notes on %s = %q, want none", key, notes)
		}
	}
}

// A follow pass whose bound ends after it has read some repositories writes the corrections it
// prepared before the bound ended: those reads were GitHub's answers, so the issues they move are
// moved now, onto a repository another record names and onto one no record names alike, and a
// change GitHub refused waits on its record with the issue told. The issue whose read the bound
// cut short, and every issue after it, write nothing and say nothing, and the follow stays owed
// for them. The passes after it read those issues alone: none of them reads a repository the cut
// pass already read, not even the one whose change still waits, which a full pass would read
// again every time and ahead of them. Once GitHub answers, the next pass completes the follow.
func TestAFollowWhoseBoundEndsWritesWhatItPreparedAndLeavesTheRestOwed(t *testing.T) {
	// Not parallel: a pass must read the records, list the issues, prepare three of them and commit
	// their corrections within calls.attempt (half a second) of real time, which the load of the
	// package's parallel tests on a 4-CPU runner can take from it.
	calls := productionAppCalls()
	calls.attempt = 500 * time.Millisecond
	const refusedRepo = "lambda/gadgets"
	installs := &slowInstallations{installations: &installations{}}
	installs.setMissing(true, "review "+refusedRepo)
	installs.hang.Store(true)
	w, client, _ := changingRig(t, installs.installations, quickInstallReads, func(w *workflowRuntime) { w.installs, w.appCalls = installs, &calls })
	const anchored, answered, refused, cut = "LEGION-1081", "LEGION-1082", "LEGION-1083", "LEGION-1084"
	for i, key := range []string{anchored, answered, refused, cut} {
		recordCorrectionRoot(t, w.pool, w.records, key, key, i)
	}
	client.show(issue(anchored, "zeta/tools", "in_progress", true, "", 1), issue(answered, answeredRepo, "in_progress", true, "", 1),
		issue(refused, refusedRepo, "in_progress", true, "", 1), issue(cut, strangerRepo, "in_progress", true, "", 1))

	w.requestFollow()
	follows := w.followRequested.Load()
	testwait.Eventually(t, "the corrections the cut pass prepared written", func() bool {
		got := recordedIssue(t, w, answered)
		return got != nil && got.Repo == answeredNamed
	})
	if got := recordedIssue(t, w, anchored); got == nil || got.Repo != "zeta/tools" || got.DefaultBranch != "main" || got.RepoCorrection == nil {
		t.Fatalf("%s = %+v, want it corrected onto zeta/tools at main with the pass that moved %s", anchored, got, answered)
	}
	if got := recordedIssue(t, w, answered); got.GitHubRepo != answeredNamed || got.DefaultBranch != "main" || got.RepoChange != nil {
		t.Fatalf("%s = %+v, want it on %s at main, GitHub's answer, with nothing waiting", answered, got, answeredNamed)
	}
	want := record.RepositoryChange{Pending: refusedRepo, Reason: "the review App is not installed on " + refusedRepo}
	if got := onWidgets(t, w, refused); got.RepoChange == nil || *got.RepoChange != want {
		t.Fatalf("the waiting change of %s = %+v, want %+v", refused, got.RepoChange, want)
	}
	if notes := repositoryNotes(t, w, refused); len(notes) != 1 {
		t.Fatalf("notes on %s = %q, want it told once that its change waits", refused, notes)
	}
	if got := onWidgets(t, w, cut); got.RepoChange != nil {
		t.Fatalf("%s waits on %+v, want nothing waiting: a read its bound cut short is no refusal", cut, got.RepoChange)
	}
	if notes := repositoryNotes(t, w, cut); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none: a read its bound cut short says nothing", cut, notes)
	}
	if applied := w.followApplied.Load(); applied >= follows {
		t.Fatalf("the follow is applied (%d of %d) with %s unread, want it owed for that issue", applied, follows, cut)
	}
	if waiting := w.unreadReasons(); len(waiting) != 0 {
		t.Fatalf("the daemon leaves %q unread, want nothing: GitHub did not fail to answer, the pass's bound ended", waiting)
	}

	testwait.Eventually(t, "a second pass cut short by its bound", func() bool { return installs.cut.Load() >= 2 })
	installs.hang.Store(false)
	testwait.Eventually(t, "the follow applied once GitHub answers", func() bool { return w.followApplied.Load() >= follows })
	onStranger(t, w, cut, "trunk")
	if notes := repositoryNotes(t, w, cut); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none", cut, notes)
	}
	for _, role := range []string{"implement " + answeredRepo, "review " + refusedRepo} {
		if reads := installs.reads(role); reads != 1 {
			t.Errorf("the follow made %d reads of %q, want one, by the cut pass: every pass after it reads only the issue it left", reads, role)
		}
	}
	if notes := repositoryNotes(t, w, refused); len(notes) != 1 {
		t.Fatalf("notes on %s = %q, want it told once", refused, notes)
	}
}

// A follow pass that moves two recorded issues onto a repository a role App is not installed on
// reads that repository once and gives both issues GitHub's refusal from that one read
// (followReads): the refusal is GitHub's answer for the repository, not for either issue. Each
// issue keeps its record's repository with the change waiting on it, and each is told once why.
func TestAFollowMovingTwoIssuesOntoARepositoryMissingAnAppSharesItsRefusal(t *testing.T) {
	t.Parallel()
	installs := &installations{}
	installs.setMissing(true, "review "+strangerRepo)
	w, client, github := changingRig(t, installs)
	keys := []string{"LEGION-1091", "LEGION-1092"}
	for i, key := range keys {
		recordCorrectionRoot(t, w.pool, w.records, key, key, i)
		client.show(issue(key, strangerRepo, "in_progress", true, "", 1))
	}

	w.requestFollow()
	followsSettled(t, w)
	want := record.RepositoryChange{Pending: strangerRepo, Reason: "the review App is not installed on " + strangerRepo}
	for _, key := range keys {
		if got := onWidgets(t, w, key); got.RepoChange == nil || *got.RepoChange != want {
			t.Fatalf("the waiting change of %s = %+v, want %+v", key, got.RepoChange, want)
		}
		if notes := repositoryNotes(t, w, key); len(notes) != 1 || notes[0] != waitingNote("review") {
			t.Fatalf("notes on %s = %q, want it told once, saying %q", key, notes, waitingNote("review"))
		}
	}
	for _, role := range []string{"implement " + strangerRepo, "review " + strangerRepo} {
		if reads := installs.reads(role); reads != 1 {
			t.Errorf("the follow made %d reads of %q for %d issues, want one: the second issue takes the first one's refusal",
				reads, role, len(keys))
		}
	}
	if reads := github.reads.Load(); reads != 1 {
		t.Errorf("the follow read %s's name and default branch %d times for %d issues, want once", strangerRepo, reads, len(keys))
	}
}

// A follow that moves the last live record off a repository asks for a read of the live records
// at its commit (projectRecords' CorrectIssueRepository, refreshLive), as the rename pass does
// after its writes: the repository the issue left leaves the cache set and the live set, so its
// cache retires and the refresher stops reading it. The follow writes no other record that would
// ask. The record starts on answeredRepo, which no anchor names, moved there through the bare
// store, which asks for nothing. Control: with the correction asking for no read, the cache set
// keeps answeredNamed.
func TestAFollowThatMovesARecordOffARepositoryReadsTheLiveRecordsAgain(t *testing.T) {
	t.Parallel()
	w, client, _ := changingRig(t, &installations{})
	const key = "LEGION-1095"
	bare := w.records.(projectRecords).Store
	recordCorrectionRoot(t, w.pool, bare, key, key, 0)
	correctRecord(t, w.pool, bare, key, answeredNamed, "main", time.Now().UTC())
	cached := func(repo string) bool { return slices.Contains(repositoryNames(w.cacheRepositories()), repo) }
	w.wakeLive()
	testwait.Eventually(t, answeredNamed+" in the cache set before the follow", func() bool { return cached(answeredNamed) })
	client.show(issue(key, strangerRepo, "in_progress", true, "", 1))

	w.requestFollow()
	followsSettled(t, w)
	if got := recordedIssue(t, w, key); got == nil || got.Repo != strangerNamed {
		t.Fatalf("%s = %+v, want it moved onto %s by the follow", key, got, strangerNamed)
	}
	left := ghrepo.MustParse(answeredNamed)
	testwait.Eventually(t, answeredNamed+" out of the cache set and the live set, and "+strangerNamed+" cached", func() bool {
		return !cached(answeredNamed) && !w.repositories.isLive(left) && cached(strangerNamed)
	})
}

// sequenceKey is the issue the two tests of a Dispatch fact at the record's own sequence move.
const sequenceKey = "LEGION-1121"

// followAtTheRecordsSequence records sequenceKey on acme/widgets and applies its live event at
// sequence 2, still on acme/widgets, so the record takes that sequence. Then Dispatch resolves
// the issue to strangerRepo at that same sequence, as a move of the project's default repository
// does, which Dispatch sends no event of the issue for, and the follow runs over that listing
// (requestFollow). It answers the runtime and the record as the follow left it.
func followAtTheRecordsSequence(t *testing.T) (*workflowRuntime, record.Issue) {
	t.Helper()
	w, client, _ := changingRig(t, &installations{})
	recordCorrectionRoot(t, w.pool, w.records, sequenceKey, sequenceKey, 0)
	deliver(t, w, client, issue(sequenceKey, "acme/widgets", "in_progress", true, "", 2))
	if got := onWidgets(t, w, sequenceKey); got.LastDispatchSeq != 2 {
		t.Fatalf("after its event %s is at Dispatch sequence %d, want the event's 2", sequenceKey, got.LastDispatchSeq)
	}
	client.show(issue(sequenceKey, strangerRepo, "in_progress", true, "", 2))
	w.requestFollow()
	followsSettled(t, w)
	return w, *recordedIssue(t, w, sequenceKey)
}

// A follow's listing at the record's own sequence is no older than the record
// (record.Issue.Outdates, as of a read): the listing says the issue's log ran no further than the
// event the record took, and it is Dispatch's resolution of the issue as of the listing, which
// the project's default repository moved with no event of the issue. So the follow corrects the
// record onto the repository Dispatch resolves it to now, with nothing said, and the record's
// sequence stays the event's. Control: with the follow asking whether its listing is older as of
// an event, the record stays on acme/widgets.
func TestAFollowAtTheRecordsOwnSequenceCorrectsTheRecord(t *testing.T) {
	t.Parallel()
	w, got := followAtTheRecordsSequence(t)
	onStranger(t, w, sequenceKey, "trunk")
	if got.LastDispatchSeq != 2 {
		t.Fatalf("after the follow %s is at Dispatch sequence %d, want the event's 2: a correction moves no sequence", sequenceKey, got.LastDispatchSeq)
	}
	if notes := repositoryNotes(t, w, sequenceKey); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none: its record followed Dispatch onto a repository both Apps are installed on", sequenceKey, notes)
	}
}

// A live event at the record's own sequence is that event delivered again: the event is Dispatch
// as of its own sequence, which the record took when the event was applied, so it moves nothing
// (record.Issue.Outdates, as of an event). It reaches the handlers again under another processed
// id where two doors apply one event, as a route's own status write and its echo from the
// consumer do (intake.OwnStatusWrite). Here the event at sequence 2 still resolves the issue to
// acme/widgets, which the follow at that sequence has since moved the record off
// (followAtTheRecordsSequence): the record stays on strangerRepo with the follow's correction,
// at sequence 2, with nothing waiting and nothing said. Control: with the engine's fence asking
// whether the event is older as of a read, the event moves the record back onto acme/widgets.
func TestAnEventDeliveredAgainAtTheRecordsSequenceLeavesTheFollowsCorrection(t *testing.T) {
	t.Parallel()
	w, followed := followAtTheRecordsSequence(t)
	onStranger(t, w, sequenceKey, "trunk")
	event := intake.DispatchIssue{Key: sequenceKey, Seq: 2, Type: "issue.updated", Status: "in_progress", Title: sequenceKey,
		Rank: "A" + sequenceKey, HandedOver: true, ResolvedRepo: "acme/widgets"}
	prepared, err := prepareAsDelivered(t, w, event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(context.Background(), w.pool, "dispatch", sequenceKey+"-delivered-again", prepared, w.handlers...); err != nil {
		t.Fatal(err)
	}

	onStranger(t, w, sequenceKey, "trunk")
	got := recordedIssue(t, w, sequenceKey)
	if c := got.RepoCorrection; !c.At.Equal(followed.RepoCorrection.At) {
		t.Fatalf("the correction of %s = %+v, want the follow's %+v: no later correction", sequenceKey, c, followed.RepoCorrection)
	}
	if got.LastDispatchSeq != 2 {
		t.Fatalf("%s is at Dispatch sequence %d, want 2", sequenceKey, got.LastDispatchSeq)
	}
	if notes := repositoryNotes(t, w, sequenceKey); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none", sequenceKey, notes)
	}
}
