package daemon

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// strangerRepo is the repository no record of these tests' project names before a test's own, as
// Dispatch resolves it, and strangerNamed is how GitHub spells it: the two differ in case alone,
// so a record carrying GitHub's spelling could only have come from GitHub's own answer.
const (
	strangerRepo  = "stranger/tools"
	strangerNamed = "Stranger/Tools"
)

// preparationRig is repositoryRig with a GitHub that names strangerRepo beside the anchors'
// acme/widgets and zeta/tools: no record names stranger/tools before a test records an issue on
// it. Each of opts is applied to the runtime as repositoryRigWith applies it.
func preparationRig(t *testing.T, installs *installations, opts ...func(*workflowRuntime)) (*workflowRuntime, *repositoryDispatch, *installations, jetstream.Stream) {
	t.Helper()
	github := newNamedGitHub(t, map[string]string{
		"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main", strangerRepo: strangerNamed + "@trunk",
	})
	return repositoryRigWith(t, installs, append([]func(*workflowRuntime){func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github.url, defaultBranchTTL, time.Now)
	}}, opts...)...)
}

// handedOver is the Dispatch issue and the event of a root handed to Legion in todo on resolved.
func handedOver(key, resolved string, seq int64) (dispatch.Issue, intake.DispatchIssue) {
	return dispatch.Issue{Key: key, Status: "todo", Title: key, Rank: "A", HandedOver: true, ResolvedRepo: resolved, LastSeq: seq},
		intake.DispatchIssue{Key: key, Seq: seq, Status: "todo", Title: key, Rank: "A", HandedOver: true, ResolvedRepo: resolved}
}

// An issue whose repository no record names is recorded on it, with GitHub's own spelling of the
// repository and its default branch, and admitted, once GitHub answers that both role Apps are
// installed on it: Dispatch's resolution of the issue decides which repository it works, and
// GitHub whether Legion can work it (R4). Nothing is said on the issue, and the reads are the
// preparation's own.
func TestAnIssueOnARepositoryNoRecordNamesIsRecordedWithGitHubsSpellingAndAdmitted(t *testing.T) {
	ctx := context.Background()
	w, client, installs, _ := preparationRig(t, &installations{})
	engine, admission := rigHandlers(w)
	issue, fact := handedOver("LEGION-901", strangerRepo, 1)
	client.issue = issue

	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-901 on %s = %v, want it prepared on what GitHub answers", strangerRepo, err)
	}
	if prepared.RepositoryRefusal != "" {
		t.Fatalf("the prepared event refuses %q, want it recorded: both Apps are installed", prepared.RepositoryRefusal)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-901", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	got := recordedIssue(t, w, "LEGION-901")
	if got == nil || got.Repo != strangerNamed || got.GitHubRepo != strangerNamed || got.DefaultBranch != "trunk" {
		t.Fatalf("LEGION-901 = %+v, want it recorded on %s@trunk, GitHub's own spelling", got, strangerNamed)
	}
	if got.Phase != phase.Admitted {
		t.Fatalf("LEGION-901 is in phase %s, want it admitted", got.Phase)
	}
	if notes := repositoryNotes(t, w, "LEGION-901"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-901 = %q, want none: it was admitted", notes)
	}
	for _, role := range []string{"implement " + strangerRepo, "review " + strangerRepo} {
		if reads := installs.reads(role); reads != 1 {
			t.Fatalf("the preparation made %d reads of %q, want one", reads, role)
		}
	}
}

// One recorded repository is read once however many issues name it: the recorded set holds
// GitHub's spelling and the default branch, so the second issue's preparation reads neither
// installation nor the repository again.
func TestTwoIssuesOnOneRecordedRepositoryMakeOneSetOfReads(t *testing.T) {
	ctx := context.Background()
	var named atomic.Int64
	github := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		path := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/repos/"))
		branch := map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main", strangerRepo: strangerNamed + "@trunk"}[path]
		if branch == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		if path == strangerRepo {
			named.Add(1)
		}
		name, head, _ := strings.Cut(branch, "@")
		_, _ = w.Write([]byte(`{"full_name":"` + name + `","default_branch":"` + head + `"}`))
	}})
	installs := &installations{}
	w, client, installs, _ := repositoryRigWith(t, installs, func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github, defaultBranchTTL, time.Now)
	})
	engine, admission := rigHandlers(w)
	for i, key := range []string{"LEGION-911", "LEGION-912"} {
		issue, fact := handedOver(key, strangerRepo, int64(i+1))
		client.issue = issue
		prepared, err := prepareAsDelivered(t, w, fact)
		if err != nil {
			t.Fatalf("prepare %s: %v", key, err)
		}
		if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-"+key, prepared, engine, admission); err != nil {
			t.Fatal(err)
		}
		if got := recordedIssue(t, w, key); got == nil || got.Repo != strangerNamed {
			t.Fatalf("%s = %+v, want it recorded on %s", key, got, strangerNamed)
		}
	}
	if reads := installs.reads("implement " + strangerRepo); reads != 1 {
		t.Fatalf("the two issues made %d installation reads of %s, want one set of reads", reads, strangerRepo)
	}
	if reads := named.Load(); reads != 1 {
		t.Fatalf("the two issues read the name and default branch of %s %d times, want once", strangerRepo, reads)
	}
}

// An issue whose repository GitHub did not answer for waits: its event is applied and
// acknowledged, nothing is said on it and nothing is recorded, and the daemon reads the
// repository again of its own until GitHub answers, then recovers the issue from Dispatch's
// listing and admits it on it — with nobody moving the issue and no restart.
func TestAnIssueWaitsWhileGitHubHasNotAnsweredForItsRepositoryAndIsAdmittedWhenItDoes(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement "+strangerRepo)
	w, client, installs, _ := preparationRig(t, installs)
	engine, admission := rigHandlers(w)
	issue, fact := handedOver("LEGION-921", strangerRepo, 1)
	client.issue = issue
	client.mu.Lock()
	client.listed = []dispatch.IssueSummary{
		{Key: "LEGION-921", Status: "todo", Title: "LEGION-921", Rank: "A", HandedOver: true, ResolvedRepo: strangerRepo, LastSeq: 1},
	}
	client.mu.Unlock()

	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-921 = %v, want it prepared to be applied and acknowledged", err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-921", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-921"); got != nil {
		t.Fatalf("LEGION-921 recorded as %+v while GitHub does not answer for %s", got, strangerRepo)
	}
	if notes := repositoryNotes(t, w, "LEGION-921"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-921 = %q while it waits, want none", notes)
	}
	if waiting := w.unreadReasons(); len(waiting) != 1 || !strings.HasPrefix(waiting[0], strangerRepo+": ") {
		t.Fatalf("the daemon waits on %q, want %s alone", waiting, strangerRepo)
	}

	installs.setFailing(nil, "implement "+strangerRepo)
	testwait.Eventually(t, "LEGION-921 admitted on "+strangerNamed+" with no one moving it", func() bool {
		got := recordedIssue(t, w, "LEGION-921")
		return got != nil && got.Repo == strangerNamed
	})
	if notes := repositoryNotes(t, w, "LEGION-921"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-921 = %q, want none: it was left, then recovered", notes)
	}
	testwait.Eventually(t, "the daemon waiting on no repository", func() bool { return len(w.unreadReasons()) == 0 })
}

// An issue left on a repository no record names while GitHub did not answer is admitted once
// GitHub answers the preparation of another issue on that repository, with no event of its own,
// no restart and no read of the refresher's own: the answer moves the repository to the ones to
// recover and asks for the read that runs the recovery, which takes the issue from Dispatch's
// listing. The answer dropped the marker alone before, which took the repository out of every
// recovery, so the issue stood unadmitted until the next boot.
func TestAnIssueLeftUnreadIsAdmittedWhenAnotherIssuesPreparationGetsGitHubsAnswer(t *testing.T) {
	failing := &installations{}
	failing.setFailing(errors.New("GitHub answered 502"), "implement "+strangerRepo)
	// The refresher's own read is never due in this test, so a recovery runs only where a read
	// is asked for. The refresher starts a wait once a run is over, so the wait it starts after
	// the recovery's own read of GitHub, the second read, says that recovery is over.
	var over atomic.Bool
	w, client, installs, _ := preparationRig(t, failing, func(w *workflowRuntime) {
		w.recheckAfter = func(time.Duration) <-chan time.Time {
			if failing.reads("implement "+strangerRepo) >= 2 {
				over.Store(true)
			}
			return nil
		}
	})
	const left, answered = "LEGION-961", "LEGION-962"
	deliver(t, w, client, issue(left, strangerRepo, "todo", true, "", 1))
	testwait.Eventually(t, "the recovery GitHub fails again is over", over.Load)
	if got := recordedIssue(t, w, left); got != nil {
		t.Fatalf("%s recorded as %+v while GitHub does not answer for %s", left, got, strangerRepo)
	}

	installs.setFailing(nil, "implement "+strangerRepo)
	deliver(t, w, client, issue(answered, strangerRepo, "todo", true, "", 1))
	if got := recordedIssue(t, w, answered); got == nil || got.Repo != strangerNamed {
		t.Fatalf("%s = %+v, want it recorded on %s once GitHub answers", answered, got, strangerNamed)
	}
	if !within(10*time.Second, func() bool {
		got := recordedIssue(t, w, left)
		return got != nil && got.Repo == strangerNamed
	}) {
		t.Fatalf("%s = %+v after GitHub answered at %s's preparation, want it recovered and admitted on %s with no event of its own",
			left, recordedIssue(t, w, left), answered, strangerNamed)
	}
	if notes := repositoryNotes(t, w, left); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none: it was left, then recovered", left, notes)
	}
	testwait.Eventually(t, "nothing left unread and nothing left to recover", func() bool {
		return len(w.unreadReasons()) == 0 && len(w.recoveringReasons()) == 0
	})
}

// A recovery GitHub fails again for a repository no record names tells no architect that GitHub
// answered for it, and keeps the children a release left to it: the failure of its own
// re-preparation is swallowed, so its listing succeeds while GitHub answered nothing. The
// architect is told once, of the child still unrecorded, by the recovery GitHub does answer.
// Every pass counted the repository as recovered before, so the first one told the architect
// that GitHub had answered for it again and dropped the wait, and the recovery that followed
// GitHub's answer told nothing.
func TestARecoveryGitHubFailsAgainTellsNoArchitectAndKeepsItsReleaseWaits(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement "+strangerRepo)
	w, client, installs, _ := preparationRig(t, installs)
	const tree, root, parent, child = "LEGION-963", "LEGION-964", "LEGION-965", "LEGION-966"
	liveRoot(t, w, client, tree)
	client.show(issue(parent, strangerRepo, "backlog", false, tree, 1), issue(child, strangerRepo, "todo", false, parent, 1))
	before := len(treeNotices(t, w, tree))
	listings := client.listingCount()
	deliver(t, w, client, issue(root, strangerRepo, "todo", true, "", 1))
	// release_children leaves a child to a repository's recovery only while the daemon holds it
	// unread, as it does from the root's failed preparation on.
	if !w.awaitRecovery(strangerRepo, tree, child, parent) {
		t.Fatalf("the wait for %s on %s was refused while the daemon holds it unread", child, strangerRepo)
	}
	// A recovery lists once, at its start, so the second listing says the first recovery is over.
	testwait.Eventually(t, "a recovery GitHub fails again is over", func() bool { return client.listingCount() >= listings+2 })
	if got := treeNotices(t, w, tree)[before:]; len(got) != 0 {
		t.Fatalf("notices to %s's architect while GitHub does not answer for %s = %q, want none: nothing was recovered", tree, strangerRepo, got)
	}
	if got := recordedIssue(t, w, root); got != nil {
		t.Fatalf("%s recorded as %+v while GitHub does not answer for %s", root, got, strangerRepo)
	}

	installs.setFailing(nil, "implement "+strangerRepo)
	testwait.Eventually(t, root+" admitted, and nothing left unread or to recover", func() bool {
		return recordedIssue(t, w, root) != nil && len(w.unreadReasons()) == 0 && len(w.recoveringReasons()) == 0
	})
	if got := recordedIssue(t, w, child); got != nil {
		t.Fatalf("%s recorded as %+v under backlog %s", child, got, parent)
	}
	notices := treeNotices(t, w, tree)[before:]
	if len(notices) != 1 || !strings.Contains(notices[0], "child-release-stalled") || !strings.Contains(notices[0], "GitHub answered for "+strangerRepo+" again") ||
		!strings.Contains(notices[0], child+" (its parent "+parent+" is not recorded: release "+parent+" first, then "+child+")") {
		t.Fatalf("notices to %s's architect once GitHub answered for %s = %q, want one naming %s and %s: the wait was kept for the recovery that answered",
			tree, strangerRepo, notices, child, parent)
	}
	time.Sleep(10 * installReadRetry.Max)
	if got := treeNotices(t, w, tree)[before:]; len(got) != 1 {
		t.Fatalf("notices after the recovery = %d, want the one only", len(got))
	}
}

// A repository no record names that GitHub did not answer for is let go once no todo issue is
// left on it: the only issue whose preparation reads GitHub for it left todo before a read
// answered, so no preparation will ever drop its marker, and the refresher stops listing the
// project for it. The issue coming back to todo is prepared afresh and admitted on what GitHub
// says then. The marker stood for ever before, and the refresher listed the whole project on its
// backoff for as long as the daemon ran.
func TestARepositoryLeftUnreadIsLetGoOnceNoTodoIssueIsLeftOnIt(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement "+strangerRepo)
	w, client, installs, _ := preparationRig(t, installs)
	const key = "LEGION-967"
	deliver(t, w, client, issue(key, strangerRepo, "todo", true, "", 1))
	if waiting := w.unreadReasons(); len(waiting) != 1 || !strings.HasPrefix(waiting[0], strangerRepo+": ") {
		t.Fatalf("the daemon waits on %q, want %s alone", waiting, strangerRepo)
	}

	// A person takes the issue back before GitHub answers, so nothing on the repository is left
	// for a preparation to read GitHub for.
	deliver(t, w, client, issue(key, strangerRepo, "backlog", true, "", 2))
	if !within(10*time.Second, func() bool { return len(w.unreadReasons()) == 0 }) {
		t.Fatalf("the daemon still waits on %q with no todo issue left on %s, want the repository let go", w.unreadReasons(), strangerRepo)
	}
	listings, reads := client.listingCount(), installs.reads("implement "+strangerRepo)
	time.Sleep(10 * installReadRetry.Max)
	if got := client.listingCount(); got != listings {
		t.Fatalf("Dispatch listed %d times more once %s was let go, want none", got-listings, strangerRepo)
	}
	if got := installs.reads("implement " + strangerRepo); got != reads {
		t.Fatalf("%s read %d times more once it was let go, want none", strangerRepo, got-reads)
	}
	if got := recordedIssue(t, w, key); got != nil {
		t.Fatalf("%s recorded as %+v, want it left in backlog", key, got)
	}

	installs.setFailing(nil, "implement "+strangerRepo)
	deliver(t, w, client, issue(key, strangerRepo, "todo", true, "", 3))
	if got := recordedIssue(t, w, key); got == nil || got.Repo != strangerNamed {
		t.Fatalf("%s = %+v, want it admitted on %s at its move back to todo", key, got, strangerNamed)
	}
}

// Letting a repository go tells each tree's architect, once, of every child its release_children
// left to that repository's recovery that nothing will record: the daemon stops reading GitHub
// for the repository, so no recovery will ever take these children, and the notice says that,
// names the status Dispatch gives each child now and what to release: here the child's parent
// first, since the daemon records neither and a child only under a recorded parent. The waits
// leave with the repository's marker, so nothing is told twice. A child recorded since is named
// in no notice. Before, the waits stayed in the daemon's memory for the whole of its life and the
// architect, whose release_children had answered that the child was released, heard nothing at
// all.
func TestLettingARepositoryGoTellsTheArchitectOfTheChildrenNothingWillRecord(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement "+strangerRepo)
	w, client, installs, _ := preparationRig(t, installs)
	const tree, root, parent, stalled, recorded = "LEGION-973", "LEGION-974", "LEGION-975", "LEGION-976", "LEGION-977"
	liveRoot(t, w, client, tree)
	// Two children a release_children left while GitHub did not answer, which it does only while
	// the daemon holds the repository unread, as it does from the root's failed preparation on:
	// one Dispatch now shows in backlog, which no recovery would record, and one the daemon
	// records since on a repository it records on.
	client.show(issue(parent, strangerRepo, "backlog", false, tree, 1), issue(stalled, strangerRepo, "backlog", false, parent, 1))
	deliver(t, w, client, issue(root, strangerRepo, "todo", true, "", 1))
	if waiting := w.unreadReasons(); len(waiting) != 1 || !strings.HasPrefix(waiting[0], strangerRepo+": ") {
		t.Fatalf("the daemon waits on %q, want %s alone", waiting, strangerRepo)
	}
	if !w.awaitRecovery(strangerRepo, tree, stalled, parent) || !w.awaitRecovery(strangerRepo, tree, recorded, tree) {
		t.Fatalf("a wait on %s was refused while the daemon holds it unread", strangerRepo)
	}
	deliver(t, w, client, issue(recorded, "acme/widgets", "todo", false, tree, 1))
	if got := recordedIssue(t, w, recorded); got == nil || got.Tree != tree {
		t.Fatalf("%s = %+v, want it recorded in %s's tree on acme/widgets", recorded, got, tree)
	}
	before := len(treeNotices(t, w, tree))

	// The root leaves todo before GitHub answers, so no preparation will read GitHub for the
	// repository again and the daemon lets it go. The notice is what the pass does last — the
	// marker and the waits are already gone by the time it commits — so it is what to wait on.
	deliver(t, w, client, issue(root, strangerRepo, "backlog", true, "", 2))
	if !within(10*time.Second, func() bool { return len(treeNotices(t, w, tree)) > before }) {
		t.Fatalf("%s's architect was told nothing once %s was let go (unread: %q), want the children nothing will record named",
			tree, strangerRepo, w.unreadReasons())
	}
	notices := treeNotices(t, w, tree)[before:]
	if len(notices) != 1 || !strings.Contains(notices[0], "child-release-stalled") ||
		!strings.Contains(notices[0], "the daemon no longer waits for GitHub to answer for "+strangerRepo+", because no todo issue labelled for Legion or under a parent names it now") ||
		!strings.Contains(notices[0], stalled+" (backlog in Dispatch now, and its parent "+parent+" is not recorded: release "+parent+" first, then "+stalled+")") || strings.Contains(notices[0], recorded) {
		t.Fatalf("notices to %s's architect once %s was let go = %q, want one naming %s with its Dispatch status, not %s",
			tree, strangerRepo, notices, stalled, recorded)
	}
	w.recoveringMu.Lock()
	left, unread := len(w.releaseWaits[strangerRepo]), len(w.unread)
	w.recoveringMu.Unlock()
	if left != 0 || unread != 0 {
		t.Fatalf("%d release waits are left on %s and %d repositories are left unread, want both gone with the telling", left, strangerRepo, unread)
	}
	time.Sleep(10 * installReadRetry.Max)
	if got := treeNotices(t, w, tree)[before:]; len(got) != 1 {
		t.Fatalf("notices after %s was let go = %d, want the one only", strangerRepo, len(got))
	}
}

// A let-go notice says what is true of the repository it names and of each child. It opens with
// why the repository was let go: no todo issue an admission would record names it, or one does
// and a read GitHub answered at another issue's preparation answers for it without a read of
// GitHub. It names a child whose parent the daemon does not record with that parent to release
// first, as the recovery's own notice does: release_children files a todo child under its
// parent's unread repository when the parent is what waits, and the child is recorded only under
// a recorded parent. Before, every let-go notice said no todo issue named the repository, false
// where one did, and told a child that was todo all along to be released again once it is todo,
// which did nothing while its parent was unrecorded.
func TestALetGoNoticeSaysWhyTheRepositoryWasLetGoAndWhatToReleaseFirst(t *testing.T) {
	const tree, parent, child = "LEGION-985", "LEGION-986", "LEGION-987"
	noTodo := "the daemon no longer waits for GitHub to answer for " + strangerRepo + ", because no todo issue labelled for Legion or under a parent names it now"
	elsewhere := "the daemon no longer waits for GitHub to answer for " + strangerRepo + ", because a read GitHub answered for it at another issue's preparation answers for it now"
	backlog := child + " (backlog in Dispatch now: release it again once it is todo)"
	for _, tc := range []struct {
		name string
		why  letGoWhy
		// shown is what Dispatch shows of the child, and of its parent where the wait names one
		// other than the tree's recorded root (parent).
		shown  []dispatch.Issue
		parent string
		// opening and line are what the notice must say, and never what it must not.
		opening, line, never string
	}{
		{name: "no todo issue is left on it", why: letGoNoTodo, parent: tree,
			shown:   []dispatch.Issue{issue(child, strangerRepo, "backlog", false, tree, 1)},
			opening: noTodo, line: backlog, never: elsewhere},
		{name: "a todo issue is left on it that another preparation's read answers for", why: letGoAnsweredElsewhere, parent: tree,
			shown:   []dispatch.Issue{issue(child, strangerRepo, "backlog", false, tree, 1)},
			opening: elsewhere, line: backlog, never: "because no todo issue"},
		{name: "a todo child filed under its unrecorded parent's repository", why: letGoNoTodo, parent: parent,
			shown:   []dispatch.Issue{issue(parent, strangerRepo, "backlog", false, tree, 1), issue(child, "acme/widgets", "todo", false, parent, 1)},
			opening: noTodo, line: child + " (todo in Dispatch now, and its parent " + parent + " is not recorded: release " + parent + " first, then " + child + ")",
			never: "release it again once it is todo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, _, _ := preparationRig(t, &installations{})
			liveRoot(t, w, client, tree)
			client.show(tc.shown...)
			listing, err := client.ListIssues(ctx, "LEGION", nil)
			if err != nil {
				t.Fatal(err)
			}
			before := len(treeNotices(t, w, tree))
			dropped := []letGo{{repo: strangerRepo, why: tc.why, waits: map[string]releaseWait{child: {tree: tree, parent: tc.parent}}}}
			if err := w.tellLetGoWaits(ctx, dropped, listing); err != nil {
				t.Fatalf("tell the let-go waits: %v", err)
			}
			notices := treeNotices(t, w, tree)[before:]
			if len(notices) != 1 || !strings.Contains(notices[0], tc.opening) || !strings.Contains(notices[0], tc.line) || strings.Contains(notices[0], tc.never) {
				t.Fatalf("notices to %s's architect = %q, want one opening %q and naming %q, never %q", tree, notices, tc.opening, tc.line, tc.never)
			}
		})
	}
}

// An issue whose repository a role App is not installed on is refused with that App named, and
// nothing of the refusal is kept: the next preparation of the same issue reads GitHub again and
// admits it the moment GitHub answers installed — with no clock moved and no restart. GitHub
// answering for the repository neither as installed nor as present is one answer, so the note
// says both, since the name may be a person's typo.
func TestARepositoryARoleAppIsNotInstalledOnIsRefusedAndAdmittedAtTheNextPreparation(t *testing.T) {
	ctx := context.Background()
	w, client, installs, _ := preparationRig(t, &installations{missing: map[string]bool{"review " + strangerRepo: true}})
	engine, admission := rigHandlers(w)
	issue, fact := handedOver("LEGION-931", strangerRepo, 1)
	client.issue = issue

	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-931 = %v, want it prepared with its refusal", err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-931", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-931"); got != nil {
		t.Fatalf("LEGION-931 recorded as %+v, want it refused: the review App is not installed on %s", got, strangerRepo)
	}
	notes := repositoryNotes(t, w, "LEGION-931")
	if len(notes) != 1 || !strings.Contains(notes[0], "the review App is not installed on "+strangerRepo) ||
		!strings.Contains(notes[0], "neither as installed nor as present") {
		t.Fatalf("notes on LEGION-931 = %q, want one naming the App and what GitHub answered", notes)
	}

	// The operator installs the App. Nothing moves the issue and nothing restarts: the next
	// delivery of the same event reads GitHub again.
	installs.setMissing(false, "review "+strangerRepo)
	prepared, err = prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-931 once the App is installed = %v, want it prepared", err)
	}
	if prepared.RepositoryRefusal != "" {
		t.Fatalf("the second preparation refuses %q, want the refusal kept nowhere", prepared.RepositoryRefusal)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-931-again", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-931"); got == nil || got.Repo != strangerNamed || got.Phase != phase.Admitted {
		t.Fatalf("LEGION-931 = %+v, want it admitted on %s at the next preparation", got, strangerNamed)
	}
}

// Recording an issue on a repository no record named before puts that repository in the GitHub
// consumer's filters in the same pass, with no restart: work in a repository the project never
// worked gets the same event service as work in one it did (R5). The repository caches follow
// the same records, the repositories of the project's live records, and the state lists the
// repository from the same moment (R6). Control (dispatch://INFRA-4401 check a-go, item 3b): with
// the record-driven path deleted (hearRecorded), the repository stays unheard.
func TestRecordingAnIssueOnANewRepositoryHearsItInTheSamePass(t *testing.T) {
	ctx := context.Background()
	w, client, _, stream := preparationRig(t, &installations{})
	engine, admission := rigHandlers(w)
	heard := "notifications.github." + strings.ReplaceAll(strangerNamed, "/", ".") + ".>"
	if filters := githubFilterSubjects(t, stream); slices.Contains(filters, heard) {
		t.Fatalf("GitHub filters = %v before any record, want %s unheard", filters, heard)
	}
	if cached := repositoryNames(w.cacheRepositories()); slices.Contains(cached, strangerNamed) {
		t.Fatalf("the repository caches follow %v before any record, want %s among none of them", cached, strangerNamed)
	}
	if _, listed := liveEntry(w, strangerNamed); listed {
		t.Fatalf("the state lists %s before any record names it", strangerNamed)
	}
	issue, fact := handedOver("LEGION-941", strangerRepo, 1)
	client.issue = issue

	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-941", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-941"); got == nil || got.Repo != strangerNamed {
		t.Fatalf("LEGION-941 = %+v, want it recorded on %s", got, strangerNamed)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, heard) {
		t.Fatalf("GitHub filters = %v, want %s heard from the record that names it", filters, heard)
	}
	if cached := repositoryNames(w.cacheRepositories()); !slices.Contains(cached, strangerNamed) {
		t.Fatalf("the repository caches follow %v, want %s among them: a live record names it", cached, strangerNamed)
	}
	if _, listed := liveEntry(w, strangerNamed); !listed {
		t.Fatalf("the state does not list %s, want it listed: a live record names it", strangerNamed)
	}
}

// A launch of an issue whose record names a repository the cache set does not hold — a production
// check's look on a done issue, whose repository no live record names any longer — prepares that
// repository first and adds it to the cache set and the heard set (launchRepository, INFRA-4401
// D8), so the pod it launches finds the repository's cache, with no restart. The launch works the
// record's repository at the record's default branch. Control (dispatch://INFRA-4401 check a-go,
// item 3b): with the launch's preparation deleted (repositoryOf's launch path), the repository is
// neither cached nor heard. The done record is written through the bare store, so no read of the
// live records it would ask for replaces the cache set the launch adds to.
func TestALaunchAddsItsRepositoryToTheCacheSetAndTheHeardSet(t *testing.T) {
	ctx := context.Background()
	w, _, installs, stream := preparationRig(t, &installations{})
	putOutboxIssue(t, w.pool, w.records.(projectRecords).Store, doneOn("LEGION-942", "LEGION", strangerNamed, "trunk"))
	heard := "notifications.github." + strings.ReplaceAll(strangerNamed, "/", ".") + ".>"
	if slices.Contains(repositoryNames(w.cacheRepositories()), strangerNamed) || slices.Contains(githubFilterSubjects(t, stream), heard) {
		t.Fatalf("%s cached or heard before the launch, want neither: no live record names it and the boot did not read its record", strangerNamed)
	}
	repo, branch, err := w.launchRepository(ctx, "LEGION-942")
	if err != nil || repo.String() != strangerNamed || branch != "trunk" {
		t.Fatalf("launchRepository = %s@%s, %v; want the record's %s@trunk", repo, branch, err, strangerNamed)
	}
	if reads := installs.reads("implement " + strangerNamed); reads != 1 {
		t.Fatalf("the launch made %d reads of the implement App's installation on %s, want its preparation's one", reads, strangerNamed)
	}
	if cached := repositoryNames(w.cacheRepositories()); !slices.Contains(cached, strangerNamed) {
		t.Fatalf("the cache set after the launch = %v, want %s in it", cached, strangerNamed)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, heard) {
		t.Fatalf("GitHub filters after the launch = %v, want %s heard", filters, heard)
	}
}

// The state lists a repository only a record names from the records themselves (R6): a daemon
// that boots over a live tree on a repository no other record names reports it from its boot's
// own read of the records, with no event of the issue and no preparation, and reports it no
// longer once that record is done. The state read the reads the preparations kept before, which a
// restart empties, so the repository stood in neither of its lists until an issue on it was
// prepared again.
func TestTheStateListsARepositoryOnlyALiveRecordNamesAfterARestart(t *testing.T) {
	ctx := context.Background()
	w, _, _, _ := preparationRig(t, &installations{})
	put := func(at phase.Phase, status string) {
		t.Helper()
		// Written through the bare store, so no read of the live records it would ask for sets the
		// live set before the boot's own read below does.
		putOutboxIssue(t, w.pool, w.records.(projectRecords).Store, record.Issue{Key: "LEGION-968", Tree: "LEGION-968", Project: "LEGION", Title: "a tree on a repository no other record names",
			Phase: at, Generation: 1, Status: status, Rank: "A", Repo: strangerNamed, DefaultBranch: "trunk", GitHubRepo: strangerNamed})
		// The boot's own read of the records, which is where a restarted daemon learns what its
		// records name: no preparation has read GitHub for the repository in this process.
		if _, err := w.heardAtBoot(ctx); err != nil {
			t.Fatal(err)
		}
	}
	put(phase.Implementing, "in_progress")
	if _, listed := liveEntry(w, strangerNamed); !listed {
		t.Fatalf("the state does not list %s after a boot over a live record on it, want it listed", strangerNamed)
	}
	put(phase.Done, "done")
	if _, listed := liveEntry(w, strangerNamed); listed {
		t.Fatalf("the state lists %s after a boot over a done record on it, want it listed no longer", strangerNamed)
	}
}

// The preparation of an issue Legion never records puts no repository in the state (R6): a todo
// child under a parent no live tree holds is prepared as any issue admission would record is,
// which reads GitHub for its repository and keeps the read, and no record names the repository.
// The state listed every repository a preparation had kept a read of before, so it named a
// repository no tree of the project works in.
func TestTheStateListsNoRepositoryOfAnIssueLegionNeverRecords(t *testing.T) {
	w, client, installs, _ := preparationRig(t, &installations{})
	const key, parent = "LEGION-969", "LEGION-970"
	deliver(t, w, client, issue(key, strangerRepo, "todo", false, parent, 1))
	if got := recordedIssue(t, w, key); got != nil {
		t.Fatalf("%s recorded as %+v, want a child under an unrecorded parent left alone", key, got)
	}
	if reads := installs.reads("implement " + strangerRepo); reads != 1 {
		t.Fatalf("the preparation made %d reads of %s's installation, want one: it reads GitHub as any issue admission would record does", reads, strangerRepo)
	}
	if read, recorded := w.repositories.lookup(strangerRepo); !recorded || read.Name.String() != strangerNamed {
		t.Fatalf("lookup(%s) = %+v, %v; want the read the preparation kept", strangerRepo, read, recorded)
	}
	if _, listed := liveEntry(w, strangerNamed); listed {
		t.Fatalf("the state lists %s, want it unlisted: no record names it", strangerNamed)
	}
}

// A person's own todo issue — not handed to Legion and under no parent — on a repository no
// record names asks GitHub nothing, at any of its events: admission never records it, so its
// apply never resolves the repository, and a read made for it is a read made for nothing. Nothing
// is left unread for it either, since no recovery would ever record it. Its preparation read both
// Apps' installations and the repository's name at every event of the issue before, since no
// refusal is kept, each read up to installReadAttempt long.
func TestAPersonsOwnTodoIssueOnARepositoryNoRecordNamesAsksGitHubNothing(t *testing.T) {
	var named atomic.Int64
	github := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		path := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/repos/"))
		if path == strangerRepo {
			named.Add(1)
		}
		branch := map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main", strangerRepo: strangerNamed + "@trunk"}[path]
		if branch == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		name, head, _ := strings.Cut(branch, "@")
		_, _ = w.Write([]byte(`{"full_name":"` + name + `","default_branch":"` + head + `"}`))
	}})
	w, client, installs, _ := repositoryRigWith(t, &installations{}, func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github, defaultBranchTTL, time.Now)
	})
	const key = "LEGION-985"
	deliver(t, w, client, issue(key, strangerRepo, "todo", false, "", 1))
	deliver(t, w, client, issue(key, strangerRepo, "todo", false, "", 2))
	if reads := installs.reads("implement "+strangerRepo) + installs.reads("review "+strangerRepo); reads != 0 {
		t.Fatalf("two events of a person's own todo issue on %s made %d installation reads, want none: nothing records the issue", strangerRepo, reads)
	}
	if reads := named.Load(); reads != 0 {
		t.Fatalf("two events of a person's own todo issue on %s read its name and default branch %d times, want none", strangerRepo, reads)
	}
	if got := recordedIssue(t, w, key); got != nil {
		t.Fatalf("%s recorded as %+v, want a person's own issue left alone", key, got)
	}
	if entry := w.repositories.entryOf(strangerRepo); entry != (repositoryEntry{}) {
		t.Fatalf("%s's entry = %+v, want none kept: no read was made", strangerRepo, entry)
	}
	if waiting := w.unreadReasons(); len(waiting) != 0 {
		t.Fatalf("the daemon waits on %q, want nothing left unread: no recovery would record the issue", waiting)
	}
}

// bootReconcile marks the context a test hands the boot's reconcile, so the installation reads
// made under it are told apart from the ones the refresher makes on its own context beside it.
type bootReconcile struct{}

// bootCountedInstallations is installations counting apart, in atBoot, the reads asked under a
// context bootReconcile marks, by either form of the read.
type bootCountedInstallations struct {
	*installations
	atBoot atomic.Int64
}

func (i *bootCountedInstallations) InstalledOn(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	if ctx.Value(bootReconcile{}) != nil {
		i.atBoot.Add(1)
	}
	return i.installations.InstalledOn(ctx, role, repo)
}

func (i *bootCountedInstallations) InstalledUnder(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	return i.InstalledOn(ctx, role, repo)
}

// The boot's listing asks GitHub nothing for a repository no record names. It runs inside the
// boot's bounded reconcile (readinessAttempt(workflow.reconcile, bootTimeout, …)), where the
// installation reads of tens of todo issues on such repositories, or two slow ones, used the
// bound up, failed every read after it at once and then the boot attempt itself. The issue is
// left unread instead, with the marker the recovery reads, and the refresher's recovery after the
// boot reads the repository once and admits the issue on GitHub's answer, with nothing said on
// it. A person's own todo issue on another such repository is read for by neither.
func TestTheBootsListingAsksGitHubNothingForARepositoryNoRecordNamesAndItsRecoveryAdmitsIt(t *testing.T) {
	ctx := context.Background()
	installs := &bootCountedInstallations{installations: &installations{}}
	w, client, _, _ := preparationRig(t, installs.installations, func(w *workflowRuntime) { w.installs = installs })
	w.bootID = "new-repository-listing-boot"
	const handed, persons, loose = "LEGION-981", "LEGION-982", "loose/notes"
	client.show(issue(handed, strangerRepo, "todo", true, "", 1), issue(persons, loose, "todo", false, "", 1))
	if err := w.reconcile(context.WithValue(ctx, bootReconcile{}, true)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if reads := installs.atBoot.Load(); reads != 0 {
		t.Fatalf("the boot's listing made %d installation reads, want none: it runs inside the boot's bounded reconcile", reads)
	}
	testwait.Eventually(t, handed+" admitted on "+strangerNamed+" by the recovery after the boot", func() bool {
		got := recordedIssue(t, w, handed)
		return got != nil && got.Repo == strangerNamed
	})
	if notes := repositoryNotes(t, w, handed); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none: it was left, then recovered", handed, notes)
	}
	testwait.Eventually(t, "nothing left unread and nothing left to recover", func() bool {
		return len(w.unreadReasons()) == 0 && len(w.recoveringReasons()) == 0
	})
	if reads := installs.reads("implement " + strangerRepo); reads != 1 {
		t.Fatalf("%s's implement App was read %d times, want once, by the recovery", strangerRepo, reads)
	}
	if reads := installs.reads("implement "+loose) + installs.reads("review "+loose); reads != 0 {
		t.Fatalf("%s, which only a person's own todo issue names, was read %d times, want none", loose, reads)
	}
	if got := recordedIssue(t, w, persons); got != nil {
		t.Fatalf("%s recorded as %+v, want a person's own issue left alone", persons, got)
	}
}

// The read that repairs a lost pull_request.opened event looks the pull request up on the issue's
// own repository, whatever else the records name, so a tree on a repository no other record names
// is repaired like any other (R5). The read mints on that repository's owner account, which is
// what lets it reach that repository.
func TestThePerIssueReadLooksUpARepositoryOnlyARecordNames(t *testing.T) {
	ctx := context.Background()
	var listed []string
	var mu atomic.Pointer[[]string]
	mu.Store(&listed)
	github := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if repo, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/pulls"); ok {
			asked := append(slices.Clone(*mu.Load()), repo)
			mu.Store(&asked)
		}
		_, _ = w.Write([]byte(`[]`))
	}})
	w, _, _, _ := preparationRig(t, &installations{})
	w.tokens, w.githubAPI = roleTokens{}, github
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		return w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-951", Tree: "LEGION-951", Project: "LEGION", Title: "on a repository no other record names",
			Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", Repo: strangerNamed, DefaultBranch: "trunk", GitHubRepo: strangerNamed})
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.readIssuePullRequest(ctx, "LEGION-951"); err != nil {
		t.Fatal(err)
	}
	asked := *mu.Load()
	if want := []string{strangerNamed}; !slices.Equal(asked, want) {
		t.Fatalf("the read asked GitHub for %v, want %v: the repository the issue's own record names", asked, want)
	}
}
