package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A daemon whose runtime runs no pod — the tmux runtime, whose runtime implements none of
// runtime.ProjectPods — refuses the release, a dry run included, before anything of it acts: the
// mark is not set, so the daemon goes on starting what it starts.
func TestProjectReleaseRefusesARuntimeThatRunsNoPod(t *testing.T) {
	mark := &projectRelease{}
	r := &projectReleaser{ctx: context.Background(), project: "legion", mark: mark, defaultBound: time.Minute, now: time.Now, log: quietLogger()}
	for _, dryRun := range []bool{false, true} {
		if _, started, err := r.Start(context.Background(), 0, dryRun); !errors.Is(err, api.ErrProjectReleaseRunsNoPod) || started {
			t.Fatalf("Start(dryRun %v) on a runtime with no pods = started %v, %v; want the tmux refusal", dryRun, started, err)
		}
	}
	if mark.held() {
		t.Fatal("the refused release set the mark")
	}
	if round := r.AskRound(); round != "" {
		t.Fatalf("the refused release asks under %q, want no round", round)
	}
}

// pausePreCheckSkipped is the real store with the release's first read of the pause answering no
// refusal, as when a pause commits between that read and the release's own transaction: what then
// refuses the release is the transaction's read (store.StartProjectRelease).
type pausePreCheckSkipped struct{ *store.Store }

func (pausePreCheckSkipped) PauseRefusesProjectRelease(context.Context, string) error { return nil }

// dispatch://INFRA-3583 R13, over the real store: the release, a dry run included, answers a
// *store.ProjectPausedError while the project is paused, and again after the resume while the pod
// the pause stopped is still held, naming that pod and the claim of it the pause stopped. Each
// refusal comes before anything of the release acts: the mark is not set, no Sandbox is listed,
// read, stopped or deleted, no release is recorded, and the stopped claim stays as the pause left
// it. A pause that commits between the release's first read and its transaction is refused there,
// and the mark the release set is cleared again. Once the last hold has ended at a role's ready,
// the release is taken, and deletes the pod.
func TestAProjectReleaseIsRefusedWhileAPauseHoldsTheProject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := outboxTreeStore(t, isolatedOutboxPool(t))
	token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	running := supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer,
		Generation: 1, Session: "ses_implementer", State: supervise.StateIdle}
	if err := st.PutClaim(ctx, running); err != nil {
		t.Fatal(err)
	}
	paused, asked, err := st.Pause(ctx, store.PauseRequest{Project: "LEGION", By: store.PauseActor{Kind: store.PauseByOperator},
		Reason: "a provider outage", At: time.Now(), StopRow: pause.StopRow})
	if err != nil || len(asked) != 1 {
		t.Fatalf("pause = %d stops asked, %v; want the implementer's", len(asked), err)
	}
	// The pause's stop of the implementer, as its stop transaction leaves it: the claim stored
	// suspended by `released`, and its pod held.
	stopped := running
	stopped.State, stopped.SuspendedBy = supervise.StateSuspended, supervise.SuspendedByReleased
	if err := st.PutClaim(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		if err := store.LockIssueForPause(ctx, tx, "LEGION-208"); err != nil {
			return err
		}
		_, err := store.OpenPausePodHoldTx(ctx, tx, paused, "LEGION-208", time.Now())
		return err
	}); err != nil {
		t.Fatalf("hold the pod of LEGION-208: %v", err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	pods := &releasePods{sandboxes: []runtime.ProjectSandbox{{Name: "legion-legion-legion-208", Kind: runtime.ProjectSandboxIssue,
		Issue: "LEGION-208", Tree: "LEGION-208", PodUID: "uid-208", ConfiguredImage: true}}}
	mark := &projectRelease{}
	r := &projectReleaser{ctx: ctx, project: "legion", mark: mark, store: st, machines: sup, pods: pods,
		defaultBound: time.Minute, stopTimeout: time.Second, reread: time.Second, poll: 10 * time.Millisecond,
		retry: releaseRetry, listPause: releaseRetry, now: time.Now, log: quietLogger()}

	refused := func(when string, wantPaused bool, dryRuns ...bool) {
		t.Helper()
		for _, dryRun := range dryRuns {
			_, started, err := r.Start(ctx, time.Second, dryRun)
			var held *store.ProjectPausedError
			if !errors.As(err, &held) || started || held.Paused != wantPaused || wantPaused && held.Pause.ID != paused.ID ||
				len(held.Held) != 1 || held.Held[0].Issue != "LEGION-208" || !slices.Equal(held.Held[0].Claims, []claim.Token{token}) {
				t.Fatalf("%s, Start(dryRun %v) = started %v, %v (%+v); want the pause's refusal naming LEGION-208 and %s, paused %t",
					when, dryRun, started, err, held, token, wantPaused)
			}
		}
		if mark.held() {
			t.Fatalf("%s, the refused release set the mark", when)
		}
		pods.mu.Lock()
		touched := pods.storeLists + pods.lists + len(pods.deleted) + len(pods.removed) + len(pods.released)
		pods.mu.Unlock()
		if touched != 0 {
			t.Fatalf("%s, the refused release listed, stopped or deleted Sandboxes: %d acts of the runtime's pods, want none", when, touched)
		}
		if release, found, err := st.ReadProjectRelease(ctx, "legion"); err != nil || found {
			t.Fatalf("%s, the refused release left the record %+v (%v), want none", when, release, err)
		}
		if c, ok, err := st.ClaimByToken(ctx, token); err != nil || !ok || c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByReleased {
			t.Fatalf("%s, the claim the pause stopped is stored %s by %q (%t, %v), want it as the pause left it", when, c.State, c.SuspendedBy, ok, err)
		}
	}
	refused("while the project is paused", true, true, false)

	// A dry run records nothing, so only the release's own transaction reads the pause again.
	r.store = pausePreCheckSkipped{st}
	refused("with a pause committed after the release's first read", true, false)
	r.store = st

	if _, err := st.Resume(ctx, store.ResumeRequest{Project: "LEGION", By: store.PauseActor{Kind: store.PauseByOperator}, At: time.Now()}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	refused("after the resume, with the pod still held", false, true, false)

	if ended, err := st.EndPausePodHold(ctx, "LEGION", "LEGION-208", store.PausePodHoldReady, time.Now()); err != nil || !ended {
		t.Fatalf("end the hold of LEGION-208 at a role's ready = %t, %v", ended, err)
	}
	if view, started, err := r.Start(ctx, time.Second, false); err != nil || !started || view.ID == 0 {
		t.Fatalf("Start once the last hold ended = %+v, started %v, %v; want the release recorded", view, started, err)
	}
	testwait.Eventually(t, "the release taken once the last hold ended finished", func() bool {
		release, found, err := st.ReadProjectRelease(ctx, "legion")
		return err == nil && found && !release.FinishedAt.IsZero()
	})
	pods.mu.Lock()
	defer pods.mu.Unlock()
	if !slices.Equal(pods.deleted, []string{"LEGION-208"}) {
		t.Fatalf("the release taken once the last hold ended deleted the pods of %v, want LEGION-208's", pods.deleted)
	}
}

// releaseRecord is a project release's store with the release's own record in memory: the start
// records release 1 with the stop set it is given and answers takenOver as the idle releases it
// took over, the finish hands its result to finished, and the claims are the ones the machines
// store (claims), after claimsFails reads of them that fail, each with claimsErr where it is set.
// finishFails is how many finish writes it refuses before it takes one, and with loseFinishAnswer
// the write it takes answers an error all the same, as one whose answer the connection lost; once
// a write was taken, every later one is refused as the store refuses a write of a finished
// release. finishes counts the writes asked, and reads the reads of the claims.
type releaseRecord struct {
	stop             []store.ProjectReleaseClaim
	takenOver        []store.TakenOverIdleRelease
	claims           *outboxClaimStore
	finished         chan json.RawMessage
	finishFails      int
	loseFinishAnswer bool
	claimsFails      int
	claimsErr        error

	mu       sync.Mutex
	finishes int
	taken    bool
	reads    int
}

// PauseRefusesProjectRelease answers that no pause holds the project: the release's refusal is
// the store's own, which TestAProjectReleaseIsRefusedWhileAPauseHoldsTheProject runs over a real
// one.
func (*releaseRecord) PauseRefusesProjectRelease(context.Context, string) error { return nil }

func (s *releaseRecord) StartProjectRelease(_ context.Context, project string, startedAt, boundAt time.Time) (store.ProjectRelease, []store.TakenOverIdleRelease, []store.ProjectReleaseClaim, error) {
	return store.ProjectRelease{ID: 1, Project: project, StartedAt: startedAt, BoundAt: boundAt}, s.takenOver, s.stop, nil
}

func (*releaseRecord) RecordProjectRelease(context.Context, int64, json.RawMessage) error { return nil }

func (s *releaseRecord) FinishProjectRelease(_ context.Context, id int64, result json.RawMessage, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishes++
	switch {
	case s.taken:
		return fmt.Errorf("finish project release %d: %w", id, store.ErrNoUnfinishedProjectRelease)
	case s.finishes <= s.finishFails:
		return errors.New("the store refused the finish")
	}
	s.taken = true
	s.finished <- result
	if s.loseFinishAnswer {
		return errors.New("the connection closed before the store answered")
	}
	return nil
}

// finishWrites is how many finish writes the store was asked.
func (s *releaseRecord) finishWrites() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishes
}

func (*releaseRecord) ReadProjectRelease(context.Context, string) (store.ProjectRelease, bool, error) {
	return store.ProjectRelease{}, false, nil
}

func (*releaseRecord) ProjectReleaseStopSet(context.Context, string) ([]store.ProjectReleaseClaim, error) {
	return nil, nil
}

func (s *releaseRecord) ClaimByToken(_ context.Context, token claim.Token) (supervise.Claim, bool, error) {
	s.claims.mu.Lock()
	defer s.claims.mu.Unlock()
	c, ok := s.claims.claims[token]
	return c, ok, nil
}

func (s *releaseRecord) Claims(context.Context) ([]supervise.Claim, error) {
	s.mu.Lock()
	s.reads++
	refused := s.reads <= s.claimsFails
	s.mu.Unlock()
	if refused {
		if s.claimsErr != nil {
			return nil, s.claimsErr
		}
		return nil, errors.New("the store refused the read of the claims")
	}
	s.claims.mu.Lock()
	defer s.claims.mu.Unlock()
	claims := make([]supervise.Claim, 0, len(s.claims.claims))
	for _, c := range s.claims.claims {
		claims = append(claims, c)
	}
	return claims, nil
}

// ClaimActivity is the activity report the machines store for token's claim at launch generation,
// as the store reads it (store.Store.ClaimActivity): none for another launch, nor for a launch
// that has reported nothing.
func (s *releaseRecord) ClaimActivity(ctx context.Context, token claim.Token, generation uint64) (supervise.ActivityReport, bool, error) {
	c, ok, _ := s.ClaimByToken(ctx, token)
	if !ok || c.Generation != generation || c.ActivityAt.IsZero() {
		return supervise.ActivityReport{}, false, nil
	}
	return supervise.ActivityReport{At: c.ActivityAt, Background: c.ActivityBackground, Kinds: c.ActivityKinds}, true, nil
}

// claimReads is how many reads of the claims the store was asked.
func (s *releaseRecord) claimReads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// releasePods is a runtime's part of a project release over the Sandboxes it lists: each issue
// pod's delete asks its authorize and records the issue it deleted, each other Sandbox's delete
// records its name, and each release of the controller's claim records the claim and answers
// releaseErr; with deleteErr set, each delete of either kind is refused with it, once an issue
// pod's authorize said yes. Each read of an issue pod's workspace is refused, with readErr where
// it is set. The runtime's store lists the Sandboxes after storeListFails lists of it that fail,
// each with storeListErr where it is set, and the API server lists the same Sandboxes as its store
// after listFails lists of it that fail; storeLists and lists count the lists asked of each.
type releasePods struct {
	sandboxes      []runtime.ProjectSandbox
	storeListFails int
	listFails      int
	releaseErr     error
	deleteErr      error
	storeListErr   error
	readErr        error

	mu         sync.Mutex
	deleted    []string
	removed    []string
	released   []claim.Token
	storeLists int
	lists      int
}

func (p *releasePods) SuspendIssue(ctx context.Context, issue, _ string, authorize func(context.Context) (bool, error)) error {
	if ok, err := authorize(ctx); err != nil || !ok {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deleteErr != nil {
		return p.deleteErr
	}
	p.deleted = append(p.deleted, issue)
	return nil
}

func (p *releasePods) ProjectSandboxes(context.Context) ([]runtime.ProjectSandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.storeLists++
	if p.storeLists <= p.storeListFails {
		if p.storeListErr != nil {
			return nil, p.storeListErr
		}
		return nil, errors.New("the Sandbox store may be stale: its watch has failed")
	}
	return p.sandboxes, nil
}

func (p *releasePods) ProjectSandboxesFromAPI(context.Context) ([]runtime.ProjectSandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lists++
	if p.lists <= p.listFails {
		return nil, errors.New("the API server answered 503")
	}
	return p.sandboxes, nil
}

func (p *releasePods) ReadUnpushed(context.Context, string, claim.Role) (runtime.UnpushedWork, error) {
	if p.readErr != nil {
		return runtime.UnpushedWork{}, p.readErr
	}
	return runtime.UnpushedWork{}, errors.New("the test's pods read no workspace")
}

func (p *releasePods) DeleteProjectSandbox(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deleteErr != nil {
		return p.deleteErr
	}
	p.removed = append(p.removed, name)
	return nil
}

func (p *releasePods) ReleaseProjectController(_ context.Context, token claim.Token) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.released = append(p.released, token)
	return p.releaseErr
}

// releaseRetry is the first wait before a failed write of a release a test builds is tried again,
// the pause between two tries of its read of the claims or of its arm of a backstop, and the pause
// between two of its lists of the project: short, so a test that fails a write, a read, an arm or
// a list waits milliseconds rather than the daemon's seconds, and never zero, which would make a
// try follow a failed one at once.
const releaseRetry = 10 * time.Millisecond

// runRelease runs a project release of the project legion over sup's machines, pods and records,
// whose finished channel it reads, with a bound of a second, and answers the result its finish
// recorded. A claim that never settles costs the bound and then the stop timeout before its pod is
// left (stopDeadline), so both are short. Each of configure is applied to the releaser before it
// starts.
func runRelease(t *testing.T, records *releaseRecord, sup *supervisor, pods *releasePods, configure ...func(*projectReleaser)) api.ProjectReleaseResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &projectReleaser{ctx: ctx, project: "legion", mark: &projectRelease{}, store: records, machines: sup, pods: pods,
		defaultBound: time.Minute, stopTimeout: time.Second, reread: time.Second, poll: 10 * time.Millisecond,
		retry: releaseRetry, listPause: releaseRetry, now: time.Now, log: quietLogger()}
	for _, c := range configure {
		c(r)
	}
	if _, started, err := r.Start(ctx, time.Second, false); err != nil || !started {
		t.Fatalf("Start = started %v, %v; want the release started", started, err)
	}
	var result api.ProjectReleaseResult
	select {
	case raw := <-records.finished:
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the release did not record its finish")
	}
	return result
}

// A claim of the stop set that failed or retired before the release came to its pod holds no
// process, so the release does not wait for it to be stored suspended: the pod is deleted rather
// than left once the wait for its stop runs out, and the claim's line says how it ended, which no
// boot starts.
func TestProjectReleaseDeletesThePodOfAStoppedClaimThatFailedOrRetired(t *testing.T) {
	for _, ended := range []supervise.ClaimState{supervise.StateFailed, supervise.StateRetired} {
		t.Run(string(ended), func(t *testing.T) {
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
			if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208",
				Issue: "LEGION-208", Role: claim.RoleImplementer, Session: "ses_implementer", State: ended}, ""); err != nil {
				t.Fatal(err)
			}
			records := &releaseRecord{claims: sup.deps.Store.(*outboxClaimStore), finished: make(chan json.RawMessage, 1),
				// The stop set as the release's own transaction read it, while the claim still ran.
				stop: []store.ProjectReleaseClaim{{Token: token, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer,
					Session: "ses_implementer", State: supervise.StateWorking}}}
			pods := &releasePods{sandboxes: []runtime.ProjectSandbox{{Name: "legion-legion-legion-208", Kind: runtime.ProjectSandboxIssue,
				Issue: "LEGION-208", Tree: "LEGION-208", PodUID: "uid-208", ConfiguredImage: true}}}
			result := runRelease(t, records, sup, pods)
			if len(result.Sandboxes) != 1 || result.Sandboxes[0].Outcome != api.ReleaseDeletedUnread {
				t.Fatalf("the pod's line is %+v, want it deleted with no read", result.Sandboxes)
			}
			pods.mu.Lock()
			deleted := slices.Clone(pods.deleted)
			pods.mu.Unlock()
			if !slices.Equal(deleted, []string{"LEGION-208"}) {
				t.Fatalf("the release deleted the pods of %v, want LEGION-208's", deleted)
			}
			if len(result.Claims) != 1 || result.Claims[0].Ended != api.ReleasedClaimEnd(ended) || result.Claims[0].BootStarts || result.Claims[0].Detail != "" {
				t.Fatalf("the claim's line is %+v, want it ended %s, which the next boot does not start", result.Claims, ended)
			}
		})
	}
}

// The release's asks end with it: once the release finished, an ask of its round that the outbox
// runs meets the claim-round read as any other update ask does, and since no claim records that
// round it finishes without a publish, while the mark still holds every start until the process
// ends. The route that takes an answer reads the same round as the outbox (AskRound), and neither
// reads the release's round once it finished.
func TestProjectReleaseAsksNothingOnceItFinished(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	if _, _, err := sup.Create(ctx, supervise.Claim{Token: mustClaimToken(t, "LEGION-208", claim.RoleImplementer), Project: "legion",
		Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer, State: supervise.StateQueued}, ""); err != nil {
		t.Fatal(err)
	}
	mark := &projectRelease{}
	release := &releaseRecord{claims: sup.deps.Store.(*outboxClaimStore), finished: make(chan json.RawMessage, 1)}
	r := &projectReleaser{ctx: ctx, project: "legion", mark: mark, store: release, machines: sup, pods: &releasePods{},
		defaultBound: time.Minute, stopTimeout: time.Second, reread: time.Second, poll: 10 * time.Millisecond,
		retry: releaseRetry, listPause: releaseRetry, now: time.Now, log: quietLogger()}
	if _, started, err := r.Start(ctx, time.Minute, false); err != nil || !started {
		t.Fatalf("Start = started %v, %v; want the release started", started, err)
	}
	select {
	case <-release.finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the release did not finish")
	}
	if !mark.held() {
		t.Fatal("the release's finish cleared the mark, which holds every start until the process ends")
	}
	if round := r.AskRound(); round != "" {
		t.Fatalf("the finished release takes answers under %q, want no round", round)
	}

	publisher := &outboxPublisher{}
	logs := &syncBuffer{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion",
		log: jsonLogger(logs), now: time.Now, release: mark}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.UpdateAsk{Role: claim.RoleImplementer, Round: projectReleaseRound(1),
		Pod: "uid-208", Text: "Push your work and answer the release."}, time.Now()))
	if ran := runDue(t, runner, time.Now().UTC()); ran != 1 {
		t.Fatalf("rows run after the release finished = %d, want the ask", ran)
	}
	if published := publisher.publishes(); len(published) != 0 {
		t.Fatalf("the ask of the finished release was published as %#v, want no publish", published)
	}
	if _, lastError, present := outboxRowState(t, pool, "LEGION-208"); present {
		t.Fatalf("the ask of the finished release still waits in the outbox (%q), want it finished", lastError)
	}
	if lines := loggedLines(t, logs.String(), `"msg":"`+askLeftLine+`"`); len(lines) != 1 || lines[0]["round"] != projectReleaseRound(1) {
		t.Fatalf("lines %q = %v in\n%s\nwant one naming the release's round", askLeftLine, lines, logs.String())
	}
}

// releaseCache and releaseController are the two Sandboxes of a project whose release takes no
// issue pod (controllerPods): a repository cache and the controller's pod.
const (
	releaseCache      = "legion-legion-cache-widgets"
	releaseController = "legion-legion-controller"
)

// controllerPods is the runtime's part of a release of a project whose Sandboxes are a repository
// cache and the controller's pod, and no issue pod.
func controllerPods() *releasePods {
	return &releasePods{sandboxes: []runtime.ProjectSandbox{
		{Name: releaseCache, Kind: runtime.ProjectSandboxCache},
		{Name: releaseController, Kind: runtime.ProjectSandboxController},
	}}
}

// releaseTheController runs a project release of the project legion over pods and records, whose
// stop set is the controller's claim, and answers the result its finish recorded as the result of
// a response, which the rig's line helpers read (releaseLine, releaseClaimLine). The claim is
// stored suspended by the project release already, so the release's stop of it finds it settled
// with no process to stop, and the next boot would start it. records reads the claims as the
// release's machines store them. Each of configure is applied to the releaser before it starts.
func releaseTheController(t *testing.T, records *releaseRecord, pods *releasePods, configure ...func(*projectReleaser)) api.ProjectReleaseResponse {
	t.Helper()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	controller := claim.ControllerToken("legion")
	if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: controller, Project: "legion", Role: claim.RoleController,
		Session: "ses_controller", State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByProjectRelease}, ""); err != nil {
		t.Fatal(err)
	}
	records.claims = sup.deps.Store.(*outboxClaimStore)
	records.finished = make(chan json.RawMessage, 1)
	records.stop = []store.ProjectReleaseClaim{{Token: controller, Role: claim.RoleController, Session: "ses_controller",
		State: supervise.StateSuspended}}
	return api.ProjectReleaseResponse{Result: runRelease(t, records, sup, pods, configure...)}
}

// dispatch://INFRA-3583 R6: the release lists the project again from the API server once every
// issue pod is gone, and one timeout or 5xx of the API server there does not stop it short, which
// would leave the controller's claim and pod and the repository caches standing. The list is
// tried again, and the release goes on to delete the cache and the controller's pod.
func TestProjectReleaseListsTheProjectAgainAfterAListThatFailed(t *testing.T) {
	pods := controllerPods()
	pods.listFails = 1
	release := releaseTheController(t, &releaseRecord{}, pods)
	if release.Result.Failure != "" {
		t.Fatalf("the release stopped short: %s", release.Result.Failure)
	}
	for _, name := range []string{releaseCache, releaseController} {
		if line := releaseLine(t, release, name); line.Outcome != api.ReleaseDeleted {
			t.Errorf("the line of %s is %+v, want it deleted", name, line)
		}
	}
	pods.mu.Lock()
	defer pods.mu.Unlock()
	if pods.lists != 2 || !slices.Equal(pods.removed, []string{releaseCache}) ||
		!slices.Equal(pods.released, []claim.Token{claim.ControllerToken("legion")}) {
		t.Fatalf("the release listed the API server %d times, deleted %v and released the controllers %v; want two lists, the cache deleted and the controller released",
			pods.lists, pods.removed, pods.released)
	}
}

// dispatch://INFRA-3583 R6: the release's first list of the project's Sandboxes is tried again
// after a list that failed, as the list once every issue pod is gone is. A release that stopped
// short there would take no pod of the project while its mark holds every start of the project
// until the next boot, so one stale read of the runtime's store does not stop it: the release goes
// on to delete the cache and the controller's pod.
func TestProjectReleaseListsTheProjectAgainAfterAFirstListThatFailed(t *testing.T) {
	pods := controllerPods()
	pods.storeListFails = 1
	release := releaseTheController(t, &releaseRecord{}, pods)
	if release.Result.Failure != "" {
		t.Fatalf("the release stopped short: %s", release.Result.Failure)
	}
	for _, name := range []string{releaseCache, releaseController} {
		if line := releaseLine(t, release, name); line.Outcome != api.ReleaseDeleted {
			t.Errorf("the line of %s is %+v, want it deleted", name, line)
		}
	}
	pods.mu.Lock()
	defer pods.mu.Unlock()
	if pods.storeLists != 2 || !slices.Equal(pods.removed, []string{releaseCache}) ||
		!slices.Equal(pods.released, []claim.Token{claim.ControllerToken("legion")}) {
		t.Fatalf("the release listed the runtime's store %d times, deleted %v and released the controllers %v; want two lists, the cache deleted and the controller released",
			pods.storeLists, pods.removed, pods.released)
	}
}

// dispatch://INFRA-3583 R6: each claim's line ends as the store holds the claim at the release's
// end, and one read of the claims that failed does not leave every line's end unwritten: the read
// is made again, and the controller's line ends project_release, which the next boot starts.
func TestProjectReleaseReadsTheClaimsAgainAfterAReadThatFailed(t *testing.T) {
	records := &releaseRecord{claimsFails: 1}
	release := releaseTheController(t, records, controllerPods())
	if line := releaseClaimLine(t, release, claim.ControllerToken("legion")); len(release.Result.Claims) != 1 ||
		line.Ended != api.ReleasedClaimEnd(supervise.SuspendedByProjectRelease) || !line.BootStarts || line.Detail != "" {
		t.Fatalf("the claim lines are %+v, want the controller's ended project_release, which the next boot starts, with no detail", release.Result.Claims)
	}
	if reads := records.claimReads(); reads != 2 {
		t.Fatalf("the claims were read %d times, want twice: the refused read and the one that answered", reads)
	}
}

// dispatch://INFRA-3583 R6: the release arms the backstop each idle release it took over would
// have armed for the claims it had stopped, which is what wakes those claims after the next boot
// when no delivery does, and one arm that failed does not leave them without it: the arm is made
// again. An idle release that had stopped no claim is armed nothing.
func TestProjectReleaseArmsAgainTheBackstopOfATakenOverReleaseAfterAnArmThatFailed(t *testing.T) {
	var mu sync.Mutex
	var tries, armed []string
	arm := func(_ context.Context, issue string) error {
		mu.Lock()
		defer mu.Unlock()
		tries = append(tries, issue)
		if len(tries) == 1 {
			return errors.New("the store refused the backstop")
		}
		armed = append(armed, issue)
		return nil
	}
	records := &releaseRecord{takenOver: []store.TakenOverIdleRelease{
		{Issue: "LEGION-209", Suspended: []claim.Role{claim.RoleReviewer}},
		{Issue: "LEGION-210"},
	}}
	releaseTheController(t, records, controllerPods(), func(r *projectReleaser) { r.armBackstops = arm })
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(tries, []string{"LEGION-209", "LEGION-209"}) || !slices.Equal(armed, []string{"LEGION-209"}) {
		t.Fatalf("the release tried the arms of %v and armed the backstops of %v; want LEGION-209's tried twice, the refused arm and the one that landed, and armed once",
			tries, armed)
	}
}

// dispatch://INFRA-3583 R6, R8: a controller's pod the release leaves — here because the runtime's
// release of the controller's claim failed — has the controller's claim held for an operator, as
// a left issue pod's claims are, so no boot starts the controller while its pod stands. The claim
// is stored suspended by an operator, and its line ends `operator`, which the next boot does not
// start.
func TestProjectReleaseHoldsTheClaimOfAControllerWhosePodItLeaves(t *testing.T) {
	pods := controllerPods()
	pods.releaseErr = errors.New("the volume guard refused the delete")
	records := &releaseRecord{}
	release := releaseTheController(t, records, pods)
	if line := releaseLine(t, release, releaseController); line.Outcome != api.ReleaseLeft || !strings.Contains(line.Reason, "the volume guard refused the delete") {
		t.Fatalf("the controller's line is %+v, want it left for the refused delete", line)
	}
	controller := claim.ControllerToken("legion")
	if line := releaseClaimLine(t, release, controller); len(release.Result.Claims) != 1 ||
		line.Ended != api.ReleasedClaimEnd(supervise.SuspendedByOperator) || line.BootStarts {
		t.Fatalf("the claim lines are %+v, want the controller's ended operator, which the next boot does not start", release.Result.Claims)
	}
	stored, ok, err := records.ClaimByToken(context.Background(), controller)
	if err != nil || !ok || stored.State != supervise.StateSuspended || stored.SuspendedBy != supervise.SuspendedByOperator {
		t.Fatalf("the controller's claim is stored %s by %q (%t, %v), want suspended by an operator", stored.State, stored.SuspendedBy, ok, err)
	}
}

// dispatch://INFRA-3583 R7: the release's finish is the one write of its record once its mark is
// finished, and a record left unfinished reads as a release that goes on until the next boot,
// which then says a boot ended it before it finished. A finish whose write failed is written
// again, and the record is finished with each claim's line whole. A write that landed with its
// answer lost is not written again once the store refuses the next try as finished.
func TestProjectReleaseWritesItsFinishAgainAfterAWriteThatFailed(t *testing.T) {
	t.Run("a write the store refused", func(t *testing.T) {
		records := &releaseRecord{finishFails: 1}
		release := releaseTheController(t, records, controllerPods())
		if writes := records.finishWrites(); writes != 2 {
			t.Fatalf("the finish was written %d times, want twice: the refused write and the one that landed", writes)
		}
		if line := releaseClaimLine(t, release, claim.ControllerToken("legion")); len(release.Result.Claims) != 1 ||
			line.Ended != api.ReleasedClaimEnd(supervise.SuspendedByProjectRelease) || !line.BootStarts {
			t.Fatalf("the finished record's claim lines are %+v, want the controller's ended project_release, which the next boot starts", release.Result.Claims)
		}
		if line := releaseLine(t, release, releaseController); line.Outcome != api.ReleaseDeleted {
			t.Fatalf("the finished record's controller line is %+v, want it deleted", line)
		}
	})
	t.Run("a write whose answer was lost", func(t *testing.T) {
		records := &releaseRecord{loseFinishAnswer: true}
		releaseTheController(t, records, controllerPods())
		testwait.Eventually(t, "the finish written again after the write whose answer was lost", func() bool {
			return records.finishWrites() >= 2
		})
		// A try after the refused one would come the doubled wait, 2*releaseRetry, after it.
		time.Sleep(10 * releaseRetry)
		if writes := records.finishWrites(); writes != 2 {
			t.Fatalf("the finish was written %d times, want twice: the one whose answer was lost and the one refused as finished", writes)
		}
	})
}

// releaseFarSideError is a far side's error text as a Kubernetes client writes one: the request's
// URL quoted, with a userinfo and a query, then the far side's words, a NUL among them.
const releaseFarSideError = `Delete "https://user:secret@api.example/apis?labelSelector=x&token=y": the server refused` +
	"\x00" + ` the request`

// releaseFarSideKept is releaseFarSideError as the store keeps a far side's error text
// (storetext.FarSideError): the URL without its userinfo and its query, the NUL as U+FFFD, and
// every other word as written.
const releaseFarSideKept = `Delete "https://api.example/apis": the server refused` + "\uFFFD" + ` the request`

// releaseFarSideLastWords is how the log keeps the last words of releaseFarSideLong, the
// release's longer far side's error: the URL they name without its userinfo and its query.
const releaseFarSideLastWords = `the last request went to "https://api.example/apis" and these are the last words`

// releaseFarSideLong is a far side's error text longer than the store keeps of one
// (storetext.FarSideErrorRunes): releaseFarSideError, then a relayed command's stderr, then last
// words that name a URL with a userinfo and a query.
var releaseFarSideLong = releaseFarSideError + "\n" +
	strings.Repeat("the relayed command's stderr goes on, line after line.\n", 2*storetext.FarSideErrorRunes) +
	`the last request went to "https://user:secret@api.example/apis?token=y" and these are the last words`

// cybermonklabs/legion#4463: every far side's error text the daemon stores or serves is kept as
// storetext.FarSideError keeps it, valid text with no URL userinfo or query, cut to its start
// where it is longer than storetext.FarSideErrorRunes, and the log gets the whole text through
// storetext.LogWhole. The release's result is stored (project_releases.result) and served (its
// GET, the state's projectRelease). So each path by which an error's text enters the result keeps
// it that way: the reason of an issue pod the release left because the runtime refused its delete
// (leave), the reason of a repository cache whose delete the runtime refused (takeOthers), the
// refusal on an issue pod's line when its role's shim refused the read of its workspace (read),
// the detail of a claim's line when the claims could not be read at the end (writeClaims), and the
// release's failure when the project's Sandboxes could not be listed (fail). A text longer than
// the store keeps leaves only its start in the result, and leave, takeOthers, read and fail log
// the whole of it, in parts under projectReleaseErrorInFull, which join to the text whose start
// the result keeps: leave, takeOthers and fail after their own line, and read at each read the
// shim refused; writeClaims logs the failed read's error on its own line.
func TestProjectReleaseKeepsAFarSidesErrorTextAsTheStoreKeepsIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// text runs the release with refused as the far side's error and configure applied to the
		// releaser before it starts, and answers the text of its result that took the error's.
		text func(t *testing.T, refused error, configure func(*projectReleaser)) string
		// inFull is how many times the path logs the whole of a text it cuts under
		// projectReleaseErrorInFull: once for leave, takeOthers and fail, once for each read a
		// shim refused, which is the pod's first read and its last here, and none for writeClaims,
		// which logs its read's error on its own line. names is what each of those parts names of
		// its release, its Sandbox, its issue and its role, with nothing under a name it leaves out.
		inFull int
		names  map[string]any
	}{
		{name: "the reason of an issue pod whose delete the runtime refused", inFull: 1,
			names: map[string]any{"release": float64(1), "sandbox": "legion-legion-legion-208"},
			text: func(t *testing.T, refused error, configure func(*projectReleaser)) string {
				sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
				token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
				// A retired claim holds no process: its pod is deleted unread, once its stop settled
				// at once.
				if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208",
					Issue: "LEGION-208", Role: claim.RoleImplementer, Session: "ses_implementer", State: supervise.StateRetired}, ""); err != nil {
					t.Fatal(err)
				}
				records := &releaseRecord{claims: sup.deps.Store.(*outboxClaimStore), finished: make(chan json.RawMessage, 1),
					stop: []store.ProjectReleaseClaim{{Token: token, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer,
						Session: "ses_implementer", State: supervise.StateWorking}}}
				pods := &releasePods{sandboxes: []runtime.ProjectSandbox{{Name: "legion-legion-legion-208", Kind: runtime.ProjectSandboxIssue,
					Issue: "LEGION-208", Tree: "LEGION-208", PodUID: "uid-208", ConfiguredImage: true}}, deleteErr: refused}
				line := releaseLine(t, api.ProjectReleaseResponse{Result: runRelease(t, records, sup, pods, configure)}, "legion-legion-legion-208")
				if line.Outcome != api.ReleaseLeft {
					t.Errorf("the pod's line is %+v, want it left for its refused delete", line)
				}
				return line.Reason
			}},
		{name: "the reason of a repository cache whose delete the runtime refused", inFull: 1,
			names: map[string]any{"release": float64(1), "sandbox": releaseCache},
			text: func(t *testing.T, refused error, configure func(*projectReleaser)) string {
				pods := controllerPods()
				pods.deleteErr = refused
				line := releaseLine(t, releaseTheController(t, &releaseRecord{}, pods, configure), releaseCache)
				if line.Outcome != api.ReleaseLeft {
					t.Errorf("the cache's line is %+v, want it left for its refused delete", line)
				}
				return line.Reason
			}},
		{name: "the refusal of an issue pod's workspace read", inFull: 2,
			names: map[string]any{"issue": "LEGION-208", "role": string(claim.RoleImplementer)},
			text: func(t *testing.T, refused error, configure func(*projectReleaser)) string {
				sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
				token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
				// A ready claim with a process is a running role, which the release reads the pod
				// through. The test has no outbox, so no role is asked, and the stop set holds no
				// claim, so the pod is deleted after its last read with no stop to wait for.
				if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208",
					Issue: "LEGION-208", Role: claim.RoleImplementer, Session: "ses_implementer", State: supervise.StateReady,
					Locator: workingPane(token)}, ""); err != nil {
					t.Fatal(err)
				}
				records := &releaseRecord{claims: sup.deps.Store.(*outboxClaimStore), finished: make(chan json.RawMessage, 1)}
				pods := &releasePods{sandboxes: []runtime.ProjectSandbox{{Name: "legion-legion-legion-208", Kind: runtime.ProjectSandboxIssue,
					Issue: "LEGION-208", Tree: "LEGION-208", PodUID: "uid-208", ConfiguredImage: true}}, readErr: refused}
				line := releaseLine(t, api.ProjectReleaseResponse{Result: runRelease(t, records, sup, pods, configure)}, "legion-legion-legion-208")
				if line.Outcome != api.ReleaseDeletedReadRefused {
					t.Errorf("the pod's line is %+v, want it deleted with its read refused", line)
				}
				return line.Refusal
			}},
		{name: "the detail of a claim's line when the claims could not be read at the end",
			text: func(t *testing.T, refused error, configure func(*projectReleaser)) string {
				release := releaseTheController(t, &releaseRecord{claimsFails: projectReleaseTries, claimsErr: refused}, controllerPods(), configure)
				return releaseClaimLine(t, release, claim.ControllerToken("legion")).Detail
			}},
		{name: "the release's failure when the project's Sandboxes could not be listed", inFull: 1,
			names: map[string]any{"release": float64(1)},
			text: func(t *testing.T, refused error, configure func(*projectReleaser)) string {
				pods := controllerPods()
				pods.storeListFails, pods.storeListErr = projectReleaseTries, refused
				return releaseTheController(t, &releaseRecord{}, pods, configure).Result.Failure
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept := func(t *testing.T, text string) {
				t.Helper()
				for _, leaked := range []string{"secret", "token=y", "\x00"} {
					if strings.Contains(text, leaked) {
						t.Errorf("the text %q holds %q, which no far side's error text the store keeps holds", text, leaked)
					}
				}
				if !strings.Contains(text, releaseFarSideKept) {
					t.Errorf("the text %q lacks the rest of the far side's error, %q", text, releaseFarSideKept)
				}
			}
			t.Run("a text the store keeps whole", func(t *testing.T) {
				kept(t, tc.text(t, errors.New(releaseFarSideError), func(*projectReleaser) {}))
			})
			t.Run("a text longer than the store keeps", func(t *testing.T) {
				logs := &syncBuffer{}
				text := tc.text(t, errors.New(releaseFarSideLong), func(r *projectReleaser) { r.log = jsonLogger(logs) })
				kept(t, text)
				if runes := utf8.RuneCountInString(text); runes != storetext.FarSideErrorRunes || !strings.HasSuffix(text, "…") {
					t.Errorf("the text is %d runes ending %q, want its start, %d runes ending with …", runes, text[max(0, len(text)-16):], storetext.FarSideErrorRunes)
				}
				if tc.inFull == 0 {
					return
				}
				parts := loggedMessages(t, logs.String(), projectReleaseErrorInFull)
				if len(parts) == 0 || len(parts)%tc.inFull != 0 {
					t.Fatalf("%d parts logged under %q, want the whole text logged %d times, in as many parts each time",
						len(parts), projectReleaseErrorInFull, tc.inFull)
				}
				each := len(parts) / tc.inFull
				for logged := range tc.inFull {
					var whole strings.Builder
					for i, part := range parts[logged*each : (logged+1)*each] {
						named := true
						for _, name := range []string{"release", "sandbox", "issue", "role"} {
							named = named && part[name] == tc.names[name]
						}
						if part["level"] != "INFO" || !named || part["part"] != float64(i+1) || part["parts"] != float64(each) {
							t.Fatalf("logged part %v, want part %d of %d at INFO naming %v", part, i+1, each, tc.names)
						}
						text, _ := part["error"].(string)
						whole.WriteString(text)
					}
					if !strings.HasSuffix(whole.String(), releaseFarSideLastWords) {
						t.Fatalf("the %d parts logged under %q join to %d bytes ending %q, want the far side's whole text, ending %q",
							each, projectReleaseErrorInFull, whole.Len(), whole.String()[max(0, whole.Len()-96):], releaseFarSideLastWords)
					}
					for _, leaked := range []string{"secret", "token=y", "\x00"} {
						if strings.Contains(whole.String(), leaked) {
							t.Errorf("the parts logged under %q hold %q", projectReleaseErrorInFull, leaked)
						}
					}
					if storetext.FarSideError(whole.String()) != text {
						t.Errorf("the result's text is not the start of the text the log holds whole")
					}
				}
			})
		})
	}
}
