package daemon

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// groupConcluded fails the test unless the fake GitHub holds exactly one legion-verdict run on
// group, made by the one App and concluding want, and the store records that run as the daemon's
// own write.
func groupConcluded(t *testing.T, github *verdictGitHub, pool *store.Pool, group, want string) {
	t.Helper()
	runs := github.legionRuns(group)
	if len(runs) != 1 || runs[0].Conclusion != want || runs[0].App != verdictAppSlug {
		t.Fatalf("legion-verdict runs on %s = %+v, want one of the one App concluding %s", group, runs, want)
	}
	if recorded := verdictWrites(t, pool, group); len(recorded) != 1 || recorded[0].GitHubID != runs[0].ID ||
		recorded[0].Kind != store.VerdictWriteCheckRun || string(recorded[0].Conclusion) != want {
		t.Fatalf("the store records %+v on %s, want the one check run %d concluding %s", recorded, group, runs[0].ID, want)
	}
}

// mergeGroup is GitHub's merge_group event of acme/widgets' pull request number at the group's
// commit group, as intake decodes it.
func mergeGroup(action string, number int, group string) intake.MergeGroup {
	return intake.MergeGroup{Repo: "acme/widgets", Number: number, Action: action, HeadSHA: group}
}

// C4 (R6) and C2: a merge group's checks_requested, applied through the merge group's handler,
// writes legion-verdict on the group's commit from its pull request's verdict at the pull
// request's head: success where that verdict stands, failure where it does not (the reviewer
// asked for changes, or a person asked for changes of a head every step passed at), and its
// destroyed writes nothing.
func TestLegionVerdictMergeGroup(t *testing.T) {
	rig := func(t *testing.T, seed func(*testing.T, *store.Pool)) *verdictRig {
		t.Helper()
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, seed)
		rig.w.handlers = append(rig.w.handlers, mergeGroupHandler{w: rig.w})
		return rig
	}
	t.Run("checks_requested where the verdict stands", func(t *testing.T) {
		rig := rig(t, acceptingAtCode)
		rig.acceptedAndWritten(t)
		rig.apply(t, mergeGroup("checks_requested", 86, "group-86"))
		rig.written(t)
		groupConcluded(t, rig.github, rig.pool, "group-86", "success")
	})
	t.Run("checks_requested where the verdict does not stand", func(t *testing.T) {
		rig := rig(t, reviewingWithPass)
		rig.apply(t, reviewerVerdict("changes", "code", 1))
		rig.written(t)
		rig.apply(t, mergeGroup("checks_requested", 86, "group-86"))
		rig.written(t)
		groupConcluded(t, rig.github, rig.pool, "group-86", "failure")
	})
	t.Run("checks_requested after a person's request for changes of a head every step passed at", func(t *testing.T) {
		rig := rig(t, acceptingAtCode)
		rig.acceptedAndWritten(t)
		rig.review(t, personsReview(1, "changes_requested"))
		rig.written(t)
		rig.apply(t, mergeGroup("checks_requested", 86, "group-86"))
		rig.written(t)
		groupConcluded(t, rig.github, rig.pool, "group-86", "failure")
	})
	t.Run("destroyed", func(t *testing.T) {
		rig := rig(t, acceptingAtCode)
		rig.acceptedAndWritten(t)
		rig.apply(t, mergeGroup("destroyed", 86, "group-86"))
		rig.written(t)
		if runs := rig.github.legionRuns("group-86"); len(runs) != 0 {
			t.Fatalf("legion-verdict runs on group-86 = %+v, want none: a destroyed group asks for no check", runs)
		}
		if recorded := verdictWrites(t, rig.pool, "group-86"); len(recorded) != 0 {
			t.Fatalf("the store records %+v on group-86, want no write", recorded)
		}
	})
}

// verdictBootTokens is the token source a boot takes (loginTokens), whose verdict token is the one
// the fake GitHub of the writer's rigs takes for a write of legion-verdict (verdictBearer).
type verdictBootTokens struct{ loginTokens }

func (verdictBootTokens) VerdictToken(ctx context.Context, owner string) (appauth.Lease, error) {
	return roleTokens{}.VerdictToken(ctx, owner)
}

// The workflow runtime the boot builds (openWorkflow) applies the merge group's handler with the
// rest of its handlers: a checks_requested of a verdict repository's pull request, applied through
// them, writes legion-verdict on the group's commit. Pull request 90 is recorded as an outside
// pull request with no review, so its verdict does not stand and the group gets failure.
func TestMergeGroupHandlerIsRegistered(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servedBootCall(t, w, r, "CAPTURE") {
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(board.Close)
	github := newVerdictGitHub()
	serve := github.serve(t)
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		// A read of acme/widgets' own name and default branch is answered.
		if r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets" {
			_, _ = io.WriteString(w, `{"full_name":"acme/widgets","default_branch":"main"}`)
			return
		}
		serve(w, r)
	}})
	cfg := config.Config{
		Project: "CAPTURE", DispatchURL: board.URL, AdmissionCap: 1, Projects: map[string]config.Project{"CAPTURE": {}},
		VerdictRepositories: []config.VerdictRepository{widgetsEntry}, GitHubApp: config.GitHubApp{AppID: verdictAppID},
	}
	w, err := openWorkflow(ctx, cfg, st, "project-id", "dispatch-token", nil, newRepositories(cfg.Project, cfg.RepositoryRenames),
		appauth.NewInstalled(), slog.New(slog.DiscardHandler), verdictBootTokens{loginTokens{login: verdictAppLogin}}, api, nil, time.Now, nil)
	if err != nil {
		t.Fatalf("open the workflow: %v", err)
	}
	run, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go w.writeVerdicts(run)

	applyFact(t, pool, "github", "outside-90", outsideOpened, w.handlers...)
	applyFact(t, pool, "github", "merge-group-90", mergeGroup("checks_requested", 90, "group-90"), w.handlers...)
	testwait.Eventually(t, "the writer of legion-verdict to hold no write", func() bool { return w.verdicts.idle() })
	groupConcluded(t, github, pool, "group-90", "failure")
}

// A write queued on a merge group's commit (queueMergeGroupWrite) decides the pull request at the
// head it stands at now, of whichever kind it is, and writes legion-verdict on the group's commit:
// success where the verdict stands at that head, and failure where none does, which a head's own
// write leaves unwritten. A repository no verdict entry names queues nothing.
func TestLegionVerdictAQueuedMergeGroupWriteDecidesThePullRequestAtItsHead(t *testing.T) {
	widgets := ghrepo.MustParse("acme/widgets")
	t.Run("a Legion head whose verdict stands", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
		rig.acceptedAndWritten(t)
		rig.w.queueMergeGroupWrite(widgets, 86, "group-86", false)
		rig.written(t)
		groupConcluded(t, rig.github, rig.pool, "group-86", "success")
	})
	t.Run("a Legion head that moved past its verdict", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
		rig.acceptedAndWritten(t)
		rig.apply(t, intake.PullRequestSynchronized{Repo: "acme/widgets", Number: 86, Branch: "legion/CAPTURE-1", Before: "code", HeadSHA: "code-2"})
		rig.written(t)
		rig.w.queueMergeGroupWrite(widgets, 86, "group-86", false)
		rig.written(t)
		groupConcluded(t, rig.github, rig.pool, "group-86", "failure")
	})
	t.Run("a Legion head with no verdict", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingWithPass)
		rig.w.queueMergeGroupWrite(widgets, 86, "group-86", false)
		rig.written(t)
		groupConcluded(t, rig.github, rig.pool, "group-86", "failure")
		if runs := rig.github.legionRuns("code"); len(runs) != 0 {
			t.Fatalf("legion-verdict runs on the pull request's head code = %+v, want none: the group's write writes on the group alone", runs)
		}
	})
	t.Run("an outside head a person approved", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded)
		rig.apply(t, outsideOpened)
		rig.review(t, outsideReview(1, "a-writer", "approved"))
		rig.written(t)
		rig.w.queueMergeGroupWrite(widgets, 90, "group-90", false)
		rig.written(t)
		groupConcluded(t, rig.github, rig.pool, "group-90", "success")
	})
	// A pull request no record holds, opened before the daemon heard its repository, is read from
	// GitHub at the head it shows, recorded there as an outside pull request, and gets failure, since
	// no review is recorded at that head. One on another project's issue branch gets failure too
	// (N7): this daemon holds no step of that issue, so no verdict stands at its head, and the daemon
	// says why once.
	t.Run("a pull request no record holds", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded)
		rig.github.mu.Lock()
		rig.github.pulls[92] = `{"number":92,"state":"open","base":{"ref":"main"},"head":{"ref":"feature/faster-tests","sha":"out-9"}}`
		rig.github.pulls[93] = `{"number":93,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/OTHER-1","sha":"other-1"}}`
		rig.github.mu.Unlock()
		rig.w.queueMergeGroupWrite(widgets, 92, "group-92", false)
		rig.w.queueMergeGroupWrite(widgets, 93, "group-93", false)
		rig.written(t)
		groupConcluded(t, rig.github, rig.pool, "group-92", "failure")
		rig.github.mu.Lock()
		reads := maps.Clone(rig.github.pullReads)
		rig.github.mu.Unlock()
		if reads[92] != 1 || reads[93] != 1 {
			t.Fatalf("GitHub was asked for pull requests 92 and 93 %d and %d times, want once each: the read that finds the head decides it", reads[92], reads[93])
		}
		if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			pr, found, err := store.OutsidePullRequestTx(context.Background(), tx, widgets, 92)
			if err == nil && (!found || pr.Head != "out-9") {
				t.Errorf("the outside pull request 92 reads %+v (found %v), want it recorded at out-9", pr, found)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		groupConcluded(t, rig.github, rig.pool, "group-93", "failure")
		if n := strings.Count(rig.logs.String(), otherProjectsLine); n != 1 {
			t.Fatalf("the daemon logged %q %d times, want once for pull request 93", otherProjectsLine, n)
		}
	})
	t.Run("a repository no entry names", func(t *testing.T) {
		writer := newVerdictWriter([]config.VerdictRepository{widgetsEntry}, verdictAppID, slog.New(slog.DiscardHandler))
		w := &workflowRuntime{verdicts: writer, repositories: servedAcme(), log: slog.New(slog.DiscardHandler)}
		w.queueMergeGroupWrite(ghrepo.MustParse("acme/gadgets"), 86, "group-86", false)
		if !writer.idle() {
			t.Fatal("a merge group's write on acme/gadgets, which no entry names, is queued; want none")
		}
		w.queueMergeGroupWrite(widgets, 86, "group-86", false)
		if writer.idle() {
			t.Fatal("a merge group's write on acme/widgets, which an entry names, is not queued")
		}
	})
}

// N9: an outside pull request's current head, which a merge group of it decides, is its newest
// sighting by GitHub's clock of the pull request, not by the order the daemon recorded them in.
// Here a person approved pull request 90 at out-1, a push moved it to out-2, and then the opened
// event of out-1 is delivered again, late: the group still decides out-2, where no review stands,
// and gets failure rather than the success of the stale head.
func TestLegionVerdictAMergeGroupDecidesTheOutsideHeadByGitHubsClock(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded)
	opened := outsideOpened
	opened.UpdatedAt = time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)
	rig.apply(t, opened)
	rig.review(t, outsideReview(1, "a-writer", "approved"))
	rig.written(t)
	moved := opened
	moved.HeadSHA, moved.UpdatedAt = "out-2", opened.UpdatedAt.Add(time.Minute)
	rig.apply(t, moved)
	rig.apply(t, opened)
	rig.w.queueMergeGroupWrite(ghrepo.MustParse("acme/widgets"), 90, "group-90", false)
	rig.written(t)
	groupConcluded(t, rig.github, rig.pool, "group-90", "failure")
}
