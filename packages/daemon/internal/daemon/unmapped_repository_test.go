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

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
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
// stranger/tools to any project.
func unmappedRig(t *testing.T, installs *installations) (*workflowRuntime, *mappingDispatch, *installations, jetstream.Stream) {
	t.Helper()
	github := newNamedGitHub(t, map[string]string{
		"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main", unmappedRepo: unmappedNamed + "@trunk",
	})
	return mappingRigWith(t, installs, func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github.url, defaultBranchTTL, time.Now)
	})
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
// repositories of the project's live records.
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
	if asked := w.mappingRequested.Load(); asked != requested {
		t.Fatalf("hearing the repository asked for %d reads of the mapping, want none", asked-requested)
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
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		return w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-951", Tree: "LEGION-951", Project: "LEGION", Title: "on a repository no project maps",
			Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", Repo: unmappedNamed, DefaultBranch: "trunk", GitHubRepo: unmappedNamed})
	}); err != nil {
		t.Fatal(err)
	}

	if err := w.readIssuePullRequest(ctx, "LEGION-951"); err != nil {
		t.Fatal(err)
	}
	asked := *mu.Load()
	if want := []string{unmappedNamed}; !slices.Equal(asked, want) {
		t.Fatalf("the read asked GitHub for %v, want %v: the repository the issue's own record names", asked, want)
	}
}
