package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// fakeProjectRelease is the daemon's project release as the routes reach it, holding to the one
// rule a release of one process keeps (internal/daemon's projectReleaser, whose own tests run the
// real one): the first start records a release, and every start after it answers that release and
// starts nothing, a dry run's among them.
type fakeProjectRelease struct {
	mu sync.Mutex
	// noPod is a daemon on a runtime that runs no pod.
	noPod bool
	// paused is what every start answers while it is set, before anything of the release acts, as
	// the daemon's release answers a *store.ProjectPausedError while a pause holds the project.
	paused error
	// started is this process's release once one started.
	started *ProjectReleaseResponse
	// starts is every bound a start recorded a release with, and dryRuns how many dry runs ran.
	starts  []time.Duration
	dryRuns int
	round   string
	answers []fakeReleaseAnswer
	// refuse is what Answer answers, nil for an answer it records.
	refuse error
}

// fakeReleaseAnswer is one answer the route handed the release.
type fakeReleaseAnswer struct {
	token      claim.Token
	session    string
	generation uint64
	head       string
}

// fakeReleaseStarted is the record the fake's release starts with.
var fakeReleaseStarted = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

func (f *fakeProjectRelease) Start(_ context.Context, bound time.Duration, dryRun bool) (ProjectReleaseResponse, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noPod {
		return ProjectReleaseResponse{}, false, ErrProjectReleaseRunsNoPod
	}
	if f.paused != nil {
		return ProjectReleaseResponse{}, false, f.paused
	}
	if f.started != nil {
		return *f.started, false, nil
	}
	if bound == 0 {
		bound = 30 * time.Minute
	}
	if dryRun {
		f.dryRuns++
		finished := fakeReleaseStarted
		return ProjectReleaseResponse{DryRun: true, StartedAt: fakeReleaseStarted, BoundAt: fakeReleaseStarted.Add(bound),
			Bound: bound.String(), FinishedAt: &finished, Result: ProjectReleaseResult{}.Listed()}, false, nil
	}
	f.starts = append(f.starts, bound)
	f.started = &ProjectReleaseResponse{ID: 7, StartedAt: fakeReleaseStarted, BoundAt: fakeReleaseStarted.Add(bound),
		Bound: bound.String(), Result: ProjectReleaseResult{}.Listed()}
	f.round = "release:7"
	return *f.started, true, nil
}

func (f *fakeProjectRelease) Read(context.Context) (ProjectReleaseResponse, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started == nil {
		return ProjectReleaseResponse{}, false, nil
	}
	return *f.started, true, nil
}

func (f *fakeProjectRelease) AskRound() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.round
}

func (f *fakeProjectRelease) Answer(_ context.Context, token claim.Token, session string, generation uint64, head string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		return f.refuse
	}
	f.answers = append(f.answers, fakeReleaseAnswer{token: token, session: session, generation: generation, head: head})
	return nil
}

// progress is the release as the fake's next read answers it: lines added, or finished.
func (f *fakeProjectRelease) progress(change func(*ProjectReleaseResponse)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.started)
}

// projectReleaseRouteHarness is the daemon's API with release, the routes' project release,
// over real claim machines, as newHarness builds them.
func projectReleaseRouteHarness(t *testing.T, release *fakeProjectRelease) *harness {
	t.Helper()
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store,
		ProjectRelease: release,
	}).Handler
	return h
}

// The release's two routes (dispatch://INFRA-3583 R6): the first POST starts
// the release with the bound it names and answers its record 202; a second POST, and a dry run
// after it, answer that same release 200 and start nothing; GET answers the release's progress,
// then its result once it finished.
func TestProjectReleaseRouteStartsOneReleaseAndAnswersItThereafter(t *testing.T) {
	release := &fakeProjectRelease{}
	h := projectReleaseRouteHarness(t, release)

	t.Run("a GET before any release is 404", func(t *testing.T) {
		wantRefusal(t, h.operator(http.MethodGet, ProjectReleaseRoute, nil), http.StatusNotFound,
			"no project release of this project was recorded")
	})

	t.Run("the first POST starts the release with its bound and answers 202", func(t *testing.T) {
		recorder := h.operator(http.MethodPost, ProjectReleaseRoute, ProjectReleaseRequest{Bound: "45m"})
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("the first POST = %d, want 202; body %s", recorder.Code, recorder.Body)
		}
		var view ProjectReleaseResponse
		decodeInto(t, recorder, &view)
		if view.ID != 7 || view.Bound != "45m0s" || view.FinishedAt != nil {
			t.Fatalf("the first POST answered %+v, want release 7 started with a 45m bound, unfinished", view)
		}
		if len(release.starts) != 1 || release.starts[0] != 45*time.Minute {
			t.Fatalf("the release was started %v, want once with 45m", release.starts)
		}
	})

	t.Run("a second POST answers the release that runs 200 and starts nothing", func(t *testing.T) {
		recorder := h.operator(http.MethodPost, ProjectReleaseRoute, ProjectReleaseRequest{Bound: "5m"})
		if recorder.Code != http.StatusOK {
			t.Fatalf("the second POST = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		var view ProjectReleaseResponse
		decodeInto(t, recorder, &view)
		if view.ID != 7 || view.Bound != "45m0s" || len(release.starts) != 1 {
			t.Fatalf("the second POST answered %+v after %d starts, want release 7 as it was and no second start", view, len(release.starts))
		}
	})

	t.Run("a dry run once a release ran answers that release", func(t *testing.T) {
		recorder := h.operator(http.MethodPost, ProjectReleaseRoute, ProjectReleaseRequest{DryRun: true})
		if recorder.Code != http.StatusOK {
			t.Fatalf("the dry run = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		var view ProjectReleaseResponse
		decodeInto(t, recorder, &view)
		if view.ID != 7 || view.DryRun || release.dryRuns != 0 || len(release.starts) != 1 {
			t.Fatalf("the dry run answered %+v, after %d dry runs and %d starts; want release 7 and nothing run", view, release.dryRuns, len(release.starts))
		}
	})

	t.Run("GET answers the release's progress and then its result", func(t *testing.T) {
		release.progress(func(view *ProjectReleaseResponse) {
			view.Result.Sandboxes = []ReleasedSandbox{{Name: "legion-legion-legion-208", Kind: "issue", Issue: "LEGION-208", Outcome: ReleaseWaiting}}
		})
		var running ProjectReleaseResponse
		recorder := h.operator(http.MethodGet, ProjectReleaseRoute, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		decodeInto(t, recorder, &running)
		if running.FinishedAt != nil || len(running.Result.Sandboxes) != 1 || running.Result.Sandboxes[0].Outcome != ReleaseWaiting {
			t.Fatalf("GET while the release runs answered %+v, want its one waiting line and no finish", running)
		}
		finished := fakeReleaseStarted.Add(10 * time.Minute)
		release.progress(func(view *ProjectReleaseResponse) {
			view.FinishedAt = &finished
			view.Result.Sandboxes[0].Outcome = ReleaseDeletedClean
			view.Result.Claims = []ReleasedClaim{{Token: "legion-legion-legion-208-architect", Issue: "LEGION-208", Role: claim.RoleArchitect,
				Session: "ses_architect", Ended: "project_release", BootStarts: true}}
		})
		var done ProjectReleaseResponse
		recorder = h.operator(http.MethodGet, ProjectReleaseRoute, nil)
		decodeInto(t, recorder, &done)
		if done.FinishedAt == nil || done.Result.Sandboxes[0].Outcome != ReleaseDeletedClean || len(done.Result.Claims) != 1 {
			t.Fatalf("GET after the release finished answered %+v, want its result", done)
		}
	})
}

// A dry run before any release answers its report 200 and records nothing.
func TestProjectReleaseRouteAnswersADryRunWithItsReport(t *testing.T) {
	release := &fakeProjectRelease{}
	h := projectReleaseRouteHarness(t, release)
	recorder := h.operator(http.MethodPost, ProjectReleaseRoute, ProjectReleaseRequest{DryRun: true, Bound: "10m"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("the dry run = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var view ProjectReleaseResponse
	decodeInto(t, recorder, &view)
	if !view.DryRun || view.ID != 0 || view.Bound != "10m0s" || release.dryRuns != 1 || len(release.starts) != 0 {
		t.Fatalf("the dry run answered %+v after %d dry runs and %d starts, want a dry run's report and no release", view, release.dryRuns, len(release.starts))
	}
	wantRefusal(t, h.operator(http.MethodGet, ProjectReleaseRoute, nil), http.StatusNotFound, "no project release of this project was recorded")
}

// A daemon on the tmux runtime runs no pod, and refuses the release 409 with a sentence that says
// so.
func TestProjectReleaseRouteRefusesOnTheTmuxRuntime(t *testing.T) {
	h := projectReleaseRouteHarness(t, &fakeProjectRelease{noPod: true})
	for _, body := range []ProjectReleaseRequest{{}, {DryRun: true}} {
		recorder := h.operator(http.MethodPost, ProjectReleaseRoute, body)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("POST %+v on tmux = %d, want 409; body %s", body, recorder.Code, recorder.Body)
		}
		var failure Failure
		decodeInto(t, recorder, &failure)
		if failure.Code != projectReleaseNoPod || failure.Error != ProjectReleaseRunsNoPod {
			t.Fatalf("POST %+v on tmux answered %+v, want %s and %q", body, failure, projectReleaseNoPod, ProjectReleaseRunsNoPod)
		}
	}
}

// dispatch://INFRA-3583 R13: a release the daemon's release refuses because a pause holds the
// project (store.ProjectPausedError) is answered 409 PROJECT_PAUSED, a dry run's included: the
// body names the pause that stands and each pod a pause still holds, with the claims of it stored
// suspended by `released`, and the sentence names those pods and the resume that ends each hold.
// A refusal that names only held pods, once the project runs again, names no pause and tells the
// operator to resume a role of each pod rather than the project. The daemon's own release over a
// real store is TestAProjectReleaseIsRefusedWhileAPauseHoldsTheProject (internal/daemon).
func TestProjectReleaseRouteAnswersPausedWhileAPauseHoldsTheProject(t *testing.T) {
	held := store.HeldPausePod{Issue: "LEGION-208", Pause: 7, HeldAt: fakeReleaseStarted, Claims: []claim.Token{"legion-legion-legion-208-implementer"}}
	standing := store.Pause{ID: 7, Project: "LEGION", Since: fakeReleaseStarted, By: store.PauseActor{Kind: store.PauseByOperator}, Reason: "an outage"}
	for _, tc := range []struct {
		name    string
		refused *store.ProjectPausedError
		want    []string
	}{
		{"paused", &store.ProjectPausedError{Project: "legion", Pause: standing, Paused: true, Held: []store.HeldPausePod{held}},
			[]string{"the project is paused (pause 7", "LEGION-208 (resume one of legion-legion-legion-208-implementer)", "Resume the project first (legion project resume)"}},
		{"held after the resume", &store.ProjectPausedError{Project: "legion", Held: []store.HeldPausePod{held}},
			[]string{"a pause still holds 1 pod(s)", "LEGION-208 (resume one of legion-legion-legion-208-implementer)", "legion claims resume --claim <token>"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := &fakeProjectRelease{paused: fmt.Errorf("start the project release of legion: %w", tc.refused)}
			h := projectReleaseRouteHarness(t, release)
			for _, body := range []ProjectReleaseRequest{{DryRun: true}, {}} {
				recorder := h.operator(http.MethodPost, ProjectReleaseRoute, body)
				if recorder.Code != http.StatusConflict {
					t.Fatalf("POST %+v = %d, want 409; body %s", body, recorder.Code, recorder.Body)
				}
				var refused PauseFailure
				decodeInto(t, recorder, &refused)
				if refused.Code != projectPaused || len(refused.HeldPods) != 1 || refused.HeldPods[0].Issue != "LEGION-208" ||
					!slices.Equal(refused.HeldPods[0].Claims, held.Claims) || (refused.Pause != nil) != tc.refused.Paused {
					t.Fatalf("POST %+v answered %+v, want PROJECT_PAUSED naming LEGION-208's hold and its claim, and the pause only while one stands", body, refused)
				}
				for _, want := range append(tc.want, "nothing was") {
					if !strings.Contains(refused.Error, want) {
						t.Fatalf("POST %+v answered the sentence %q, which does not say %q", body, refused.Error, want)
					}
				}
			}
			if len(release.starts) != 0 || release.dryRuns != 0 {
				t.Fatalf("a refused release started %d releases and %d dry runs, want none", len(release.starts), release.dryRuns)
			}
			wantRefusal(t, h.operator(http.MethodGet, ProjectReleaseRoute, nil), http.StatusNotFound, "no project release of this project was recorded")
		})
	}
}

// A bound that is no positive Go duration is refused before anything starts.
func TestProjectReleaseRouteRefusesABoundThatIsNoPositiveDuration(t *testing.T) {
	release := &fakeProjectRelease{}
	h := projectReleaseRouteHarness(t, release)
	for _, bound := range []string{"soon", "0s", "-5m"} {
		t.Run(bound, func(t *testing.T) {
			wantRefusal(t, h.operator(http.MethodPost, ProjectReleaseRoute, ProjectReleaseRequest{Bound: bound}), http.StatusBadRequest,
				fmt.Sprintf("bound %q is no positive Go duration, such as 30m", bound))
		})
	}
	if len(release.starts) != 0 || release.dryRuns != 0 {
		t.Fatalf("a refused bound started %d releases and %d dry runs, want none", len(release.starts), release.dryRuns)
	}
}

// dispatch://INFRA-3583 R6: a role's ready_for_update naming the running release's round is handed
// to the release, with the claim's session, generation and head, and the claim's machine records
// nothing of it; the release's refusal is the route's answer. A round no release runs reaches the
// machine, which refuses a round its claim is not in.
func TestProjectReleaseHandsAnAnswerToItsRoundToTheRelease(t *testing.T) {
	release := &fakeProjectRelease{round: "release:7"}
	h := projectReleaseRouteHarness(t, release)
	token, boot := h.launch("LEGION-208", claim.RoleImplementer)
	session := "ses_implementer"
	registration := h.registered(boot, session)
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatal("no machine for the implementer")
	}
	if err := machine.Handle(h.ctx, supervise.RequestReady{Claim: token, Generation: registration.Generation, Session: session}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	answer := claim.ReadyForUpdateRequest{ClaimToken: token, SessionID: session, Secret: registration.Secret,
		Generation: registration.Generation, Round: "release:7", Head: "9f1c0a7d4b2e6f8a3c5d1e7b9a0f2c4d6e8b1a35"}

	t.Run("the release's round goes to the release", func(t *testing.T) {
		if recorder := h.readyForUpdate(answer); recorder.Code != http.StatusNoContent {
			t.Fatalf("the answer to the release's ask = %d, want 204; body %s", recorder.Code, recorder.Body)
		}
		want := fakeReleaseAnswer{token: token, session: session, generation: registration.Generation, head: answer.Head}
		if len(release.answers) != 1 || release.answers[0] != want {
			t.Fatalf("the release was handed %+v, want %+v once", release.answers, want)
		}
		if c := machine.Claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" || c.UpdateRound != "" {
			t.Fatalf("the claim after the release's answer = %+v, want no round column written", c)
		}
	})

	t.Run("the release's refusal is the answer", func(t *testing.T) {
		release.refuse = fmt.Errorf("ready_for_update for %s at generation 9: %w", token, claim.StaleGeneration)
		wantRefusal(t, h.readyForUpdate(answer), http.StatusConflict, "Stale generation")
		release.refuse = nil
	})

	t.Run("a round no release runs reaches the machine", func(t *testing.T) {
		other := answer
		other.Round = "release:6"
		recorder := h.readyForUpdate(other)
		if recorder.Code != http.StatusConflict || len(release.answers) != 1 {
			t.Fatalf("an answer to another release's round = %d with %d answers handed on, want the machine's 409 and none; body %s",
				recorder.Code, len(release.answers), recorder.Body)
		}
	})
}
