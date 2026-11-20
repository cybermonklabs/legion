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
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// unmappedRepo is the repository no project maps that these tests record issues on, as Dispatch
// resolves it, and unmappedNamed is how GitHub spells it: the two differ in case alone, so a
// record carrying GitHub's spelling could only have come from GitHub's own answer.
const (
	unmappedRepo  = "stranger/tools"
	unmappedNamed = "Stranger/Tools"
)

// unmappedRig is mappingRig with a GitHub that names unmappedRepo beside the two the project
// maps: the mapping names acme/widgets (its default) and zeta/tools, and nothing maps
// stranger/tools to any project. Each of opts is applied to the runtime before its boot reads
// the mapping, as mappingRigWith applies it.
func unmappedRig(t *testing.T, installs *installations, opts ...func(*workflowRuntime)) (*workflowRuntime, *mappingDispatch, *installations, jetstream.Stream) {
	t.Helper()
	github := newNamedGitHub(t, map[string]string{
		"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main", unmappedRepo: unmappedNamed + "@trunk",
	})
	return mappingRigWith(t, installs, append([]func(*workflowRuntime){func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github.url, defaultBranchTTL, time.Now)
	}}, opts...)...)
}

// handedOver is the Dispatch issue and the event of a root handed to Legion in todo on resolved.
func handedOver(key, resolved string, seq int64) (dispatch.Issue, intake.DispatchIssue) {
	return dispatch.Issue{Key: key, Status: "todo", Title: key, Rank: "A", HandedOver: true, ResolvedRepo: resolved, LastSeq: seq},
		intake.DispatchIssue{Key: key, Seq: seq, Status: "todo", Title: key, Rank: "A", HandedOver: true, ResolvedRepo: resolved}
}

// An issue whose repository no mapping names is recorded on it, with GitHub's own spelling of the
// repository and its default branch, and admitted, once GitHub answers that both role Apps are
// installed on it: the mapping decides the project's default repository and what the daemon
// prepares before an issue names one, never which repository an issue works (R4). Nothing is said
// on the issue, and the reads are the preparation's own: no read of the mapping is asked for.
func TestAnIssueOnARepositoryNoMappingNamesIsRecordedWithGitHubsSpellingAndAdmitted(t *testing.T) {
	ctx := context.Background()
	w, client, installs, _ := unmappedRig(t, &installations{})
	engine, admission := rigHandlers(w)
	issue, fact := handedOver("LEGION-901", unmappedRepo, 1)
	client.issue = issue
	requested := w.mappingRequested.Load()

	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-901 on %s = %v, want it prepared on what GitHub answers", unmappedRepo, err)
	}
	if prepared.RepositoryRefusal != "" {
		t.Fatalf("the prepared event refuses %q, want it recorded: both Apps are installed", prepared.RepositoryRefusal)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-901", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	got := recordedIssue(t, w, "LEGION-901")
	if got == nil || got.Repo != unmappedNamed || got.GitHubRepo != unmappedNamed || got.DefaultBranch != "trunk" {
		t.Fatalf("LEGION-901 = %+v, want it recorded on %s@trunk, GitHub's own spelling", got, unmappedNamed)
	}
	if got.Phase != phase.Admitted {
		t.Fatalf("LEGION-901 is in phase %s, want it admitted", got.Phase)
	}
	if notes := repositoryNotes(t, w, "LEGION-901"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-901 = %q, want none: it was admitted", notes)
	}
	for _, role := range []string{"implement " + unmappedRepo, "review " + unmappedRepo} {
		if reads := installs.reads(role); reads != 1 {
			t.Fatalf("the preparation made %d reads of %q, want one", reads, role)
		}
	}
	if asked := w.mappingRequested.Load(); asked != requested {
		t.Fatalf("the preparation asked for %d reads of the mapping, want none: the mapping has nothing to say about %s", asked-requested, unmappedRepo)
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
		branch := map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main", unmappedRepo: unmappedNamed + "@trunk"}[path]
		if branch == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		if path == unmappedRepo {
			named.Add(1)
		}
		name, head, _ := strings.Cut(branch, "@")
		_, _ = w.Write([]byte(`{"full_name":"` + name + `","default_branch":"` + head + `"}`))
	}})
	installs := &installations{}
	w, client, installs, _ := mappingRigWith(t, installs, func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github, defaultBranchTTL, time.Now)
	})
	engine, admission := rigHandlers(w)
	for i, key := range []string{"LEGION-911", "LEGION-912"} {
		issue, fact := handedOver(key, unmappedRepo, int64(i+1))
		client.issue = issue
		prepared, err := prepareAsDelivered(t, w, fact)
		if err != nil {
			t.Fatalf("prepare %s: %v", key, err)
		}
		if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-"+key, prepared, engine, admission); err != nil {
			t.Fatal(err)
		}
		if got := recordedIssue(t, w, key); got == nil || got.Repo != unmappedNamed {
			t.Fatalf("%s = %+v, want it recorded on %s", key, got, unmappedNamed)
		}
	}
	if reads := installs.reads("implement " + unmappedRepo); reads != 1 {
		t.Fatalf("the two issues made %d installation reads of %s, want one set of reads", reads, unmappedRepo)
	}
	if reads := named.Load(); reads != 1 {
		t.Fatalf("the two issues read the name and default branch of %s %d times, want once", unmappedRepo, reads)
	}
}

// An issue whose repository GitHub did not answer for waits: its event is applied and
// acknowledged, nothing is said on it and nothing is recorded, and the daemon reads the
// repository again of its own until GitHub answers, then recovers the issue from Dispatch's
// listing and admits it on it — with nobody moving the issue and no mapping change.
func TestAnIssueWaitsWhileGitHubHasNotAnsweredForItsRepositoryAndIsAdmittedWhenItDoes(t *testing.T) {
	ctx := context.Background()
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement "+unmappedRepo)
	w, client, installs, _ := unmappedRig(t, installs)
	engine, admission := rigHandlers(w)
	issue, fact := handedOver("LEGION-921", unmappedRepo, 1)
	client.issue = issue
	client.mu.Lock()
	client.listed = []dispatch.IssueSummary{
		{Key: "LEGION-921", Status: "todo", Title: "LEGION-921", Rank: "A", HandedOver: true, ResolvedRepo: unmappedRepo, LastSeq: 1},
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
		t.Fatalf("LEGION-921 recorded as %+v while GitHub does not answer for %s", got, unmappedRepo)
	}
	if notes := repositoryNotes(t, w, "LEGION-921"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-921 = %q while it waits, want none", notes)
	}
	if waiting := w.unreadReasons(); len(waiting) != 1 || !strings.HasPrefix(waiting[0], unmappedRepo+": ") {
		t.Fatalf("the daemon waits on %q, want %s alone", waiting, unmappedRepo)
	}

	installs.setFailing(nil, "implement "+unmappedRepo)
	testwait.Eventually(t, "LEGION-921 admitted on "+unmappedNamed+" with no one moving it", func() bool {
		got := recordedIssue(t, w, "LEGION-921")
		return got != nil && got.Repo == unmappedNamed
	})
	if notes := repositoryNotes(t, w, "LEGION-921"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-921 = %q, want none: it was left, then recovered", notes)
	}
	testwait.Eventually(t, "the daemon waiting on no repository", func() bool { return len(w.unreadReasons()) == 0 })
}

// An issue left on a repository no mapping names while GitHub did not answer is admitted once
// GitHub answers the preparation of another issue on that repository, with no event of its own,
// no restart and no read of the refresher's own: the answer moves the repository to the ones to
// recover and asks for the read that runs the recovery, which takes the issue from Dispatch's
// listing. The answer dropped the marker alone before, which took the repository out of every
// recovery, so the issue stood unadmitted until the next boot.
func TestAnIssueLeftUnreadIsAdmittedWhenAnotherIssuesPreparationGetsGitHubsAnswer(t *testing.T) {
	failing := &installations{}
	failing.setFailing(errors.New("GitHub answered 502"), "implement "+unmappedRepo)
	// The refresher's own read is never due in this test, so a recovery runs only where a read
	// is asked for. The refresher starts a wait once a run is over, so the wait it starts after
	// the recovery's own read of GitHub, the second read, says that recovery is over.
	var over atomic.Bool
	w, client, installs, _ := unmappedRig(t, failing, func(w *workflowRuntime) {
		w.recheckAfter = func(time.Duration) <-chan time.Time {
			if failing.reads("implement "+unmappedRepo) >= 2 {
				over.Store(true)
			}
			return nil
		}
	})
	const left, answered = "LEGION-961", "LEGION-962"
	deliver(t, w, client, issue(left, unmappedRepo, "todo", true, "", 1))
	testwait.Eventually(t, "the recovery GitHub fails again is over", over.Load)
	if got := recordedIssue(t, w, left); got != nil {
		t.Fatalf("%s recorded as %+v while GitHub does not answer for %s", left, got, unmappedRepo)
	}

	installs.setFailing(nil, "implement "+unmappedRepo)
	deliver(t, w, client, issue(answered, unmappedRepo, "todo", true, "", 1))
	if got := recordedIssue(t, w, answered); got == nil || got.Repo != unmappedNamed {
		t.Fatalf("%s = %+v, want it recorded on %s once GitHub answers", answered, got, unmappedNamed)
	}
	if !within(10*time.Second, func() bool {
		got := recordedIssue(t, w, left)
		return got != nil && got.Repo == unmappedNamed
	}) {
		t.Fatalf("%s = %+v after GitHub answered at %s's preparation, want it recovered and admitted on %s with no event of its own",
			left, recordedIssue(t, w, left), answered, unmappedNamed)
	}
	if notes := repositoryNotes(t, w, left); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none: it was left, then recovered", left, notes)
	}
	testwait.Eventually(t, "nothing left unread and nothing left to recover", func() bool {
		return len(w.unreadReasons()) == 0 && len(w.recoveringReasons()) == 0
	})
}

// A recovery GitHub fails again for a repository no mapping names tells no architect that GitHub
// answered for it, and keeps the children a release left to it: the failure of its own
// re-preparation is swallowed, so its listing succeeds while GitHub answered nothing. The
// architect is told once, of the child still unrecorded, by the recovery GitHub does answer.
// Every pass counted the repository as recovered before, so the first one told the architect
// that GitHub had answered for it again and dropped the wait, and the recovery that followed
// GitHub's answer told nothing.
func TestARecoveryGitHubFailsAgainTellsNoArchitectAndKeepsItsReleaseWaits(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement "+unmappedRepo)
	w, client, installs, _ := unmappedRig(t, installs)
	const tree, root, parent, child = "LEGION-963", "LEGION-964", "LEGION-965", "LEGION-966"
	liveRoot(t, w, client, tree)
	client.show(issue(parent, unmappedRepo, "backlog", false, tree, 1), issue(child, unmappedRepo, "todo", false, parent, 1))
	before := len(treeNotices(t, w, tree))
	listings := client.listingCount()
	deliver(t, w, client, issue(root, unmappedRepo, "todo", true, "", 1))
	// release_children leaves a child to a repository's recovery only while the daemon holds it
	// unread, as it does from the root's failed preparation on.
	if !w.awaitRecovery(unmappedRepo, tree, child, parent) {
		t.Fatalf("the wait for %s on %s was refused while the daemon holds it unread", child, unmappedRepo)
	}
	// A recovery lists once, at its start, so the second listing says the first recovery is over.
	testwait.Eventually(t, "a recovery GitHub fails again is over", func() bool { return client.listingCount() >= listings+2 })
	if got := treeNotices(t, w, tree)[before:]; len(got) != 0 {
		t.Fatalf("notices to %s's architect while GitHub does not answer for %s = %q, want none: nothing was recovered", tree, unmappedRepo, got)
	}
	if got := recordedIssue(t, w, root); got != nil {
		t.Fatalf("%s recorded as %+v while GitHub does not answer for %s", root, got, unmappedRepo)
	}

	installs.setFailing(nil, "implement "+unmappedRepo)
	testwait.Eventually(t, root+" admitted, and nothing left unread or to recover", func() bool {
		return recordedIssue(t, w, root) != nil && len(w.unreadReasons()) == 0 && len(w.recoveringReasons()) == 0
	})
	if got := recordedIssue(t, w, child); got != nil {
		t.Fatalf("%s recorded as %+v under backlog %s", child, got, parent)
	}
	notices := treeNotices(t, w, tree)[before:]
	if len(notices) != 1 || !strings.Contains(notices[0], "child-release-stalled") || !strings.Contains(notices[0], "GitHub answered for "+unmappedRepo+" again") ||
		!strings.Contains(notices[0], child+" (its parent "+parent+" is not recorded: release "+parent+" first, then "+child+")") {
		t.Fatalf("notices to %s's architect once GitHub answered for %s = %q, want one naming %s and %s: the wait was kept for the recovery that answered",
			tree, unmappedRepo, notices, child, parent)
	}
	time.Sleep(10 * installReadRetry.Max)
	if got := treeNotices(t, w, tree)[before:]; len(got) != 1 {
		t.Fatalf("notices after the recovery = %d, want the one only", len(got))
	}
}

// A repository no mapping names that GitHub did not answer for is let go once no todo issue is
// left on it: the only issue whose preparation reads GitHub for it left todo before a read
// answered, so no preparation will ever drop its marker, and the refresher stops reading the
// mapping and listing the project for it. The issue coming back to todo is prepared afresh and
// admitted on what GitHub says then. The marker stood for ever before, and the refresher read
// the whole mapping and listed the whole project on its backoff for as long as the daemon ran.
func TestARepositoryLeftUnreadIsLetGoOnceNoTodoIssueIsLeftOnIt(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement "+unmappedRepo)
	w, client, installs, _ := unmappedRig(t, installs)
	const key = "LEGION-967"
	deliver(t, w, client, issue(key, unmappedRepo, "todo", true, "", 1))
	if waiting := w.unreadReasons(); len(waiting) != 1 || !strings.HasPrefix(waiting[0], unmappedRepo+": ") {
		t.Fatalf("the daemon waits on %q, want %s alone", waiting, unmappedRepo)
	}

	// A person takes the issue back before GitHub answers, so nothing on the repository is left
	// for a preparation to read GitHub for.
	deliver(t, w, client, issue(key, unmappedRepo, "backlog", true, "", 2))
	if !within(10*time.Second, func() bool { return len(w.unreadReasons()) == 0 }) {
		t.Fatalf("the daemon still waits on %q with no todo issue left on %s, want the repository let go", w.unreadReasons(), unmappedRepo)
	}
	listings, reads := client.listingCount(), installs.reads("implement "+unmappedRepo)
	time.Sleep(10 * installReadRetry.Max)
	if got := client.listingCount(); got != listings {
		t.Fatalf("Dispatch listed %d times more once %s was let go, want none", got-listings, unmappedRepo)
	}
	if got := installs.reads("implement " + unmappedRepo); got != reads {
		t.Fatalf("%s read %d times more once it was let go, want none", unmappedRepo, got-reads)
	}
	if got := recordedIssue(t, w, key); got != nil {
		t.Fatalf("%s recorded as %+v, want it left in backlog", key, got)
	}

	installs.setFailing(nil, "implement "+unmappedRepo)
	deliver(t, w, client, issue(key, unmappedRepo, "todo", true, "", 3))
	if got := recordedIssue(t, w, key); got == nil || got.Repo != unmappedNamed {
		t.Fatalf("%s = %+v, want it admitted on %s at its move back to todo", key, got, unmappedNamed)
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
	installs.setFailing(errors.New("GitHub answered 502"), "implement "+unmappedRepo)
	w, client, installs, _ := unmappedRig(t, installs)
	const tree, root, parent, stalled, recorded = "LEGION-973", "LEGION-974", "LEGION-975", "LEGION-976", "LEGION-977"
	liveRoot(t, w, client, tree)
	// Two children a release_children left while GitHub did not answer, which it does only while
	// the daemon holds the repository unread, as it does from the root's failed preparation on:
	// one Dispatch now shows in backlog, which no recovery would record, and one the daemon
	// records since on a served repository.
	client.show(issue(parent, unmappedRepo, "backlog", false, tree, 1), issue(stalled, unmappedRepo, "backlog", false, parent, 1))
	deliver(t, w, client, issue(root, unmappedRepo, "todo", true, "", 1))
	if waiting := w.unreadReasons(); len(waiting) != 1 || !strings.HasPrefix(waiting[0], unmappedRepo+": ") {
		t.Fatalf("the daemon waits on %q, want %s alone", waiting, unmappedRepo)
	}
	if !w.awaitRecovery(unmappedRepo, tree, stalled, parent) || !w.awaitRecovery(unmappedRepo, tree, recorded, tree) {
		t.Fatalf("a wait on %s was refused while the daemon holds it unread", unmappedRepo)
	}
	deliver(t, w, client, issue(recorded, "acme/widgets", "todo", false, tree, 1))
	if got := recordedIssue(t, w, recorded); got == nil || got.Tree != tree {
		t.Fatalf("%s = %+v, want it recorded in %s's tree on the served repository", recorded, got, tree)
	}
	before := len(treeNotices(t, w, tree))

	// The root leaves todo before GitHub answers, so no preparation will read GitHub for the
	// repository again and the daemon lets it go. The notice is what the pass does last — the
	// marker and the waits are already gone by the time it commits — so it is what to wait on.
	deliver(t, w, client, issue(root, unmappedRepo, "backlog", true, "", 2))
	if !within(10*time.Second, func() bool { return len(treeNotices(t, w, tree)) > before }) {
		t.Fatalf("%s's architect was told nothing once %s was let go (unread: %q), want the children nothing will record named",
			tree, unmappedRepo, w.unreadReasons())
	}
	notices := treeNotices(t, w, tree)[before:]
	if len(notices) != 1 || !strings.Contains(notices[0], "child-release-stalled") ||
		!strings.Contains(notices[0], "the daemon no longer waits for GitHub to answer for "+unmappedRepo+", because no todo issue labelled for Legion or under a parent names it now") ||
		!strings.Contains(notices[0], stalled+" (backlog in Dispatch now, and its parent "+parent+" is not recorded: release "+parent+" first, then "+stalled+")") || strings.Contains(notices[0], recorded) {
		t.Fatalf("notices to %s's architect once %s was let go = %q, want one naming %s with its Dispatch status, not %s",
			tree, unmappedRepo, notices, stalled, recorded)
	}
	w.recoveringMu.Lock()
	left, unread := len(w.releaseWaits[unmappedRepo]), len(w.unread)
	w.recoveringMu.Unlock()
	if left != 0 || unread != 0 {
		t.Fatalf("%d release waits are left on %s and %d repositories are left unread, want both gone with the telling", left, unmappedRepo, unread)
	}
	time.Sleep(10 * installReadRetry.Max)
	if got := treeNotices(t, w, tree)[before:]; len(got) != 1 {
		t.Fatalf("notices after %s was let go = %d, want the one only", unmappedRepo, len(got))
	}
}

// A let-go notice says what is true of the repository it names and of each child. It opens with
// why the repository was let go: no todo issue an admission would record names it, or one does
// and the mapping or an earlier answer of GitHub's answers for it without a read of GitHub. It
// names a child whose parent the daemon does not record with that parent to release first, as the
// recovery's own notice does: release_children files a todo child under its parent's unread
// repository when the parent is what waits, and the child is recorded only under a recorded
// parent. It names a child on a repository the mapping now leaves out with that refusal, which
// is what stops it: for a missing App, with the step refuseChild's own notice gives, and for a
// read GitHub did not answer, with the mapping's own recovery, which records it. Before, every
// let-go notice said no todo issue named the repository, false where one did, and told a child
// that was todo all along to be released again once it is todo, which did nothing while its
// parent was unrecorded or its repository was refused.
func TestALetGoNoticeSaysWhyTheRepositoryWasLetGoAndWhatToReleaseFirst(t *testing.T) {
	const tree, parent, child = "LEGION-985", "LEGION-986", "LEGION-987"
	noTodo := "the daemon no longer waits for GitHub to answer for " + unmappedRepo + ", because no todo issue labelled for Legion or under a parent names it now"
	elsewhere := "the daemon no longer waits for GitHub to answer for " + unmappedRepo + ", because Dispatch's repository mapping, or a read GitHub answered for it at another issue's preparation, answers for it now"
	backlog := child + " (backlog in Dispatch now: release it again once it is todo)"
	for _, tc := range []struct {
		name string
		why  letGoWhy
		// shown is what Dispatch shows of the child, and of its parent where the wait names one
		// other than the tree's recorded root (parent). mapped is how Dispatch's mapping, once
		// read, answers for the repository: "" names it not at all, "missing" leaves it out for
		// a role App not installed on it, and "unread" leaves it out for a read GitHub did not
		// answer.
		shown          []dispatch.Issue
		parent, mapped string
		// opening and line are what the notice must say, and never what it must not.
		opening, line, never string
	}{
		{name: "no todo issue is left on it", why: letGoNoTodo, parent: tree,
			shown:   []dispatch.Issue{issue(child, unmappedRepo, "backlog", false, tree, 1)},
			opening: noTodo, line: backlog, never: elsewhere},
		{name: "a todo issue is left on it that the mapping answers for", why: letGoAnsweredElsewhere, parent: tree,
			shown:   []dispatch.Issue{issue(child, unmappedRepo, "backlog", false, tree, 1)},
			opening: elsewhere, line: backlog, never: "because no todo issue"},
		{name: "a todo child filed under its unrecorded parent's repository", why: letGoNoTodo, parent: parent,
			shown:   []dispatch.Issue{issue(parent, unmappedRepo, "backlog", false, tree, 1), issue(child, "acme/widgets", "todo", false, parent, 1)},
			opening: noTodo, line: child + " (todo in Dispatch now, and its parent " + parent + " is not recorded: release " + parent + " first, then " + child + ")",
			never: "release it again once it is todo"},
		{name: "a todo child on a repository the mapping now leaves out for a missing App", why: letGoAnsweredElsewhere, parent: tree, mapped: "missing",
			shown:   []dispatch.Issue{issue(child, unmappedRepo, "todo", false, tree, 1)},
			opening: elsewhere, line: child + " (todo in Dispatch now, and the daemon records no issue on " + unmappedRepo + ": the review App is not installed on " + unmappedRepo + "; install the App on it, or set a repository both Legion Apps are installed on, then release " + child + " again)",
			never: "release it again once it is todo"},
		{name: "a todo child on a repository the mapping leaves out for a read GitHub did not answer", why: letGoAnsweredElsewhere, parent: tree, mapped: "unread",
			shown:   []dispatch.Issue{issue(child, unmappedRepo, "todo", false, tree, 1)},
			opening: elsewhere, line: child + " (todo in Dispatch now; Dispatch's mapping leaves " + unmappedRepo + " out for now: the implement App's installation on " + unmappedRepo + " could not be read",
			never: "release it again once it is todo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, installs, _ := unmappedRig(t, &installations{})
			liveRoot(t, w, client, tree)
			client.show(tc.shown...)
			if tc.mapped != "" {
				client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true}, dispatch.RepoProject{Repo: "zeta/tools", Project: "LEGION"},
					dispatch.RepoProject{Repo: unmappedRepo, Project: "LEGION"})
				if tc.mapped == "missing" {
					installs.setMissing(true, "review "+unmappedRepo)
				} else {
					installs.setFailing(errors.New("GitHub answered 502"), "implement "+unmappedRepo)
				}
				if err := w.refreshMapping(ctx); err != nil {
					t.Fatalf("read the mapping that names %s: %v", unmappedRepo, err)
				}
				if held := w.repositories.lookup(unmappedRepo); held.source != fromMapping || held.refusal == "" || held.unread != (tc.mapped == "unread") {
					t.Fatalf("lookup(%s) = %+v once the mapping names it, want it left out (%s)", unmappedRepo, held, tc.mapped)
				}
			}
			listing, err := client.ListIssues(ctx, "LEGION", nil)
			if err != nil {
				t.Fatal(err)
			}
			before := len(treeNotices(t, w, tree))
			dropped := []letGo{{repo: unmappedRepo, why: tc.why, waits: map[string]releaseWait{child: {tree: tree, parent: tc.parent}}}}
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
// admits it the moment GitHub answers installed — with no clock moved, no restart and no mapping
// change. GitHub answering for the repository neither as installed nor as present is one answer,
// so the note says both, since the name may be a person's typo.
func TestARepositoryARoleAppIsNotInstalledOnIsRefusedAndAdmittedAtTheNextPreparation(t *testing.T) {
	ctx := context.Background()
	w, client, installs, _ := unmappedRig(t, &installations{missing: map[string]bool{"review " + unmappedRepo: true}})
	engine, admission := rigHandlers(w)
	issue, fact := handedOver("LEGION-931", unmappedRepo, 1)
	client.issue = issue
	requested := w.mappingRequested.Load()

	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-931 = %v, want it prepared with its refusal", err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-931", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-931"); got != nil {
		t.Fatalf("LEGION-931 recorded as %+v, want it refused: the review App is not installed on %s", got, unmappedRepo)
	}
	notes := repositoryNotes(t, w, "LEGION-931")
	if len(notes) != 1 || !strings.Contains(notes[0], "the review App is not installed on "+unmappedRepo) ||
		!strings.Contains(notes[0], "neither as installed nor as present") {
		t.Fatalf("notes on LEGION-931 = %q, want one naming the App and what GitHub answered", notes)
	}
	if asked := w.mappingRequested.Load(); asked != requested {
		t.Fatalf("the refusal asked for %d reads of the mapping, want none: no mapping change admits this issue", asked-requested)
	}

	// The operator installs the App. Nothing moves the issue, nothing changes the mapping and
	// nothing restarts: the next delivery of the same event reads GitHub again.
	installs.setMissing(false, "review "+unmappedRepo)
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
	if got := recordedIssue(t, w, "LEGION-931"); got == nil || got.Repo != unmappedNamed || got.Phase != phase.Admitted {
		t.Fatalf("LEGION-931 = %+v, want it admitted on %s at the next preparation", got, unmappedNamed)
	}
	if asked := w.mappingRequested.Load(); asked != requested {
		t.Fatalf("the admission asked for %d reads of the mapping, want none", asked-requested)
	}
}

// Recording an issue on a repository no mapping names puts that repository in the GitHub
// consumer's filters in the same pass, with no mapping change and no restart: work in a
// repository no project maps gets the same event service as work in a mapped one (R5). The
// repository caches follow the same set, which is the mapping's repositories and the
// repositories of the project's live records, and the state lists the repository from the same
// moment (R6).
func TestRecordingAnIssueOnAnUnmappedRepositoryHearsItInTheSamePass(t *testing.T) {
	ctx := context.Background()
	w, client, _, stream := unmappedRig(t, &installations{})
	engine, admission := rigHandlers(w)
	heard := "notifications.github." + strings.ReplaceAll(unmappedNamed, "/", ".") + ".>"
	if filters := githubFilterSubjects(t, stream); slices.Contains(filters, heard) {
		t.Fatalf("GitHub filters = %v before any record, want %s unheard", filters, heard)
	}
	if cached := repositoryNames(w.cacheRepositories()); slices.Contains(cached, unmappedNamed) {
		t.Fatalf("the repository caches follow %v before any record, want %s among none of them", cached, unmappedNamed)
	}
	if listed := servedNames(w); slices.Contains(listed, unmappedNamed) {
		t.Fatalf("the state lists %v before any record, want no %s", listed, unmappedNamed)
	}
	issue, fact := handedOver("LEGION-941", unmappedRepo, 1)
	client.issue = issue
	requested := w.mappingRequested.Load()

	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-941", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-941"); got == nil || got.Repo != unmappedNamed {
		t.Fatalf("LEGION-941 = %+v, want it recorded on %s", got, unmappedNamed)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, heard) {
		t.Fatalf("GitHub filters = %v, want %s heard from the record that names it", filters, heard)
	}
	if cached := repositoryNames(w.cacheRepositories()); !slices.Contains(cached, unmappedNamed) {
		t.Fatalf("the repository caches follow %v, want %s among them: a live record names it", cached, unmappedNamed)
	}
	if listed := servedNames(w); !slices.Contains(listed, unmappedNamed) {
		t.Fatalf("the state lists %v, want %s among them: a live record names it", listed, unmappedNamed)
	}
	if asked := w.mappingRequested.Load(); asked != requested {
		t.Fatalf("hearing the repository asked for %d reads of the mapping, want none", asked-requested)
	}
}

// servedNames is every repository the state lists as served (api.RepositoriesView.Mapped), as
// legion state reads them.
func servedNames(w *workflowRuntime) []string {
	var names []string
	for _, entry := range w.repositories.view(nil).Mapped {
		names = append(names, entry.Repo)
	}
	return names
}

// The state lists a repository only a record names from the records themselves (R6): a daemon
// that boots over a live tree on a repository no mapping names reports it from its boot's own
// read of the records, with no event of the issue and no preparation, and reports it no longer
// once that record is done. The state read the reads the preparations kept before, which a
// restart empties, so the repository stood in neither of its lists until an issue on it was
// prepared again, and its cache among the caches of repositories the daemon serves no longer.
func TestTheStateListsARepositoryOnlyALiveRecordNamesAfterARestart(t *testing.T) {
	ctx := context.Background()
	w, _, _, _ := unmappedRig(t, &installations{})
	put := func(at phase.Phase, status string) {
		t.Helper()
		if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
			return w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-968", Tree: "LEGION-968", Project: "LEGION", Title: "a tree on a repository no project maps",
				Phase: at, Generation: 1, Status: status, Rank: "A", Repo: unmappedNamed, DefaultBranch: "trunk", GitHubRepo: unmappedNamed})
		}); err != nil {
			t.Fatal(err)
		}
		// The boot's own read of the records, which is where a restarted daemon learns what its
		// records name: no preparation has read GitHub for the repository in this process.
		if _, err := w.heardAtBoot(ctx); err != nil {
			t.Fatal(err)
		}
	}
	put(phase.Implementing, "in_progress")
	if listed := servedNames(w); !slices.Contains(listed, unmappedNamed) {
		t.Fatalf("the state lists %v after a boot over a live record on %s, want it among them", listed, unmappedNamed)
	}
	put(phase.Done, "done")
	if listed := servedNames(w); slices.Contains(listed, unmappedNamed) {
		t.Fatalf("the state lists %v after a boot over a done record on %s, want it served no longer", listed, unmappedNamed)
	}
}

// The preparation of an issue Legion never records puts no repository in the state (R6): a todo
// child under a parent no live tree holds is prepared as any issue admission would record is,
// which reads GitHub for its repository and keeps the read, and no record names the repository.
// The state listed every repository a preparation had kept a read of before, so it named a
// repository no tree of the project works in.
func TestTheStateListsNoRepositoryOfAnIssueLegionNeverRecords(t *testing.T) {
	w, client, installs, _ := unmappedRig(t, &installations{})
	const key, parent = "LEGION-969", "LEGION-970"
	deliver(t, w, client, issue(key, unmappedRepo, "todo", false, parent, 1))
	if got := recordedIssue(t, w, key); got != nil {
		t.Fatalf("%s recorded as %+v, want a child under an unrecorded parent left alone", key, got)
	}
	if reads := installs.reads("implement " + unmappedRepo); reads != 1 {
		t.Fatalf("the preparation made %d reads of %s's installation, want one: it reads GitHub as any issue admission would record does", reads, unmappedRepo)
	}
	if got := w.repositories.lookup(unmappedRepo); got.source != fromRecorded || got.read.Name.String() != unmappedNamed {
		t.Fatalf("lookup(%s) = %+v, want the read the preparation kept", unmappedRepo, got)
	}
	if listed := servedNames(w); slices.Contains(listed, unmappedNamed) {
		t.Fatalf("the state lists %v, want no %s: no record names it", listed, unmappedNamed)
	}
}

// A person's own todo issue — not handed to Legion and under no parent — on a repository no
// mapping names asks GitHub nothing, at any of its events: admission never records it, so its
// apply never resolves the repository, and a read made for it is a read made for nothing. Nothing
// is left unread for it either, since no recovery would ever record it, and no read of the
// mapping is asked for. Its preparation read both Apps' installations and the repository's name
// at every event of the issue before, since no refusal is kept, each read up to
// installReadAttempt long.
func TestAPersonsOwnTodoIssueOnARepositoryNoMappingNamesAsksGitHubNothing(t *testing.T) {
	var named atomic.Int64
	github := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		path := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/repos/"))
		if path == unmappedRepo {
			named.Add(1)
		}
		branch := map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main", unmappedRepo: unmappedNamed + "@trunk"}[path]
		if branch == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		name, head, _ := strings.Cut(branch, "@")
		_, _ = w.Write([]byte(`{"full_name":"` + name + `","default_branch":"` + head + `"}`))
	}})
	w, client, installs, _ := mappingRigWith(t, &installations{}, func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github, defaultBranchTTL, time.Now)
	})
	requested := w.mappingRequested.Load()
	const key = "LEGION-985"
	deliver(t, w, client, issue(key, unmappedRepo, "todo", false, "", 1))
	deliver(t, w, client, issue(key, unmappedRepo, "todo", false, "", 2))
	if reads := installs.reads("implement "+unmappedRepo) + installs.reads("review "+unmappedRepo); reads != 0 {
		t.Fatalf("two events of a person's own todo issue on %s made %d installation reads, want none: nothing records the issue", unmappedRepo, reads)
	}
	if reads := named.Load(); reads != 0 {
		t.Fatalf("two events of a person's own todo issue on %s read its name and default branch %d times, want none", unmappedRepo, reads)
	}
	if got := recordedIssue(t, w, key); got != nil {
		t.Fatalf("%s recorded as %+v, want a person's own issue left alone", key, got)
	}
	if got := w.repositories.lookup(unmappedRepo).source; got != fromNeither {
		t.Fatalf("lookup(%s) answered from set %d, want no read kept: none was made", unmappedRepo, got)
	}
	if waiting := w.unreadReasons(); len(waiting) != 0 {
		t.Fatalf("the daemon waits on %q, want nothing left unread: no recovery would record the issue", waiting)
	}
	if asked := w.mappingRequested.Load(); asked != requested {
		t.Fatalf("the events asked for %d reads of the mapping, want none", asked-requested)
	}
}

// bootReconcile marks the context a test hands the boot's reconcile, so the installation reads
// made under it are told apart from the ones the refresher makes on its own context beside it.
type bootReconcile struct{}

// bootCountedInstallations is installations counting apart, in atBoot, the reads asked under a
// context bootReconcile marks.
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

// The boot's listing asks GitHub nothing for a repository no mapping names. It runs inside the
// boot's bounded reconcile (readinessAttempt(workflow.reconcile, bootTimeout, …)), where the
// installation reads of tens of todo issues on such repositories, or two slow ones, used the
// bound up, failed every read after it at once and then the boot attempt itself. The issue is
// left unread instead, with the marker the recovery reads, and the refresher's recovery after the
// boot reads the repository once and admits the issue on GitHub's answer, with nothing said on
// it. A person's own todo issue on another such repository is read for by neither.
func TestTheBootsListingAsksGitHubNothingForARepositoryNoMappingNamesAndItsRecoveryAdmitsIt(t *testing.T) {
	ctx := context.Background()
	installs := &bootCountedInstallations{installations: &installations{}}
	w, client, _, _ := unmappedRig(t, installs.installations, func(w *workflowRuntime) { w.installs = installs })
	w.bootID = "unmapped-listing-boot"
	const handed, persons, loose = "LEGION-981", "LEGION-982", "loose/notes"
	client.show(issue(handed, unmappedRepo, "todo", true, "", 1), issue(persons, loose, "todo", false, "", 1))
	if err := w.reconcile(context.WithValue(ctx, bootReconcile{}, true)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if reads := installs.atBoot.Load(); reads != 0 {
		t.Fatalf("the boot's listing made %d installation reads, want none: it runs inside the boot's bounded reconcile", reads)
	}
	testwait.Eventually(t, handed+" admitted on "+unmappedNamed+" by the recovery after the boot", func() bool {
		got := recordedIssue(t, w, handed)
		return got != nil && got.Repo == unmappedNamed
	})
	if notes := repositoryNotes(t, w, handed); len(notes) != 0 {
		t.Fatalf("notes on %s = %q, want none: it was left, then recovered", handed, notes)
	}
	testwait.Eventually(t, "nothing left unread and nothing left to recover", func() bool {
		return len(w.unreadReasons()) == 0 && len(w.recoveringReasons()) == 0
	})
	if reads := installs.reads("implement " + unmappedRepo); reads != 1 {
		t.Fatalf("%s's implement App was read %d times, want once, by the recovery", unmappedRepo, reads)
	}
	if reads := installs.reads("implement "+loose) + installs.reads("review "+loose); reads != 0 {
		t.Fatalf("%s, which only a person's own todo issue names, was read %d times, want none", loose, reads)
	}
	if got := recordedIssue(t, w, persons); got != nil {
		t.Fatalf("%s recorded as %+v, want a person's own issue left alone", persons, got)
	}
}

// The read that repairs a lost pull_request.opened event looks the pull request up on the issue's
// own repository, whatever the mapping names, so a tree on a repository no mapping names is
// repaired like any other (R5). The read mints on that repository's owner account, which is what
// lets it reach a repository the mapping never named.
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
	w, _, _, _ := unmappedRig(t, &installations{})
	w.tokens, w.githubAPI = roleTokens{}, github
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		return w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-951", Tree: "LEGION-951", Project: "LEGION", Title: "on a repository no project maps",
			Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", Repo: unmappedNamed, DefaultBranch: "trunk", GitHubRepo: unmappedNamed})
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.readIssuePullRequest(ctx, "LEGION-951"); err != nil {
		t.Fatal(err)
	}
	asked := *mu.Load()
	if want := []string{unmappedNamed}; !slices.Equal(asked, want) {
		t.Fatalf("the read asked GitHub for %v, want %v: the repository the issue's own record names", asked, want)
	}
}
