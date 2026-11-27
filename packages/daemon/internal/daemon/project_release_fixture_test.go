package daemon

// The project release's shared fixtures: a store with the release's own record in memory
// (releaseRecord), a runtime's part of a release over the Sandboxes it lists (releasePods), and
// the release a test builds and runs over them (testReleaser, runRelease).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// releaseRecord is a project release's store with the release's own record in memory: the start
// records release 1 with the stop set it is given and answers takenOver as the idle releases it
// took over, the finish hands its result to finished, and the claims are the ones the machines
// store (claims), after claimsFails reads of them that fail, each with claimsErr where it is set.
// finishFails is how many finish writes it refuses before it takes one, and with loseFinishAnswer
// the write it takes answers an error all the same, as one whose answer the connection lost; once
// a write was taken, every later one is refused as the store refuses a write of a finished
// release. finishes counts the writes asked, and reads the reads of the claims. holds is the
// session names each claim holds, each read of them refused with holdsErr where it is set, and
// merged whether the record holds the pull request of every issue merged, each read of it refused
// with mergedErr where it is set. replacements is the standing records of the replacements of the
// project's issue pods, each read of them refused with replacementsErr where it is set, each
// forget refused with forgetErr where it is set, and forgot is each operation forgotten.
type releaseRecord struct {
	stop             []store.ProjectReleaseClaim
	takenOver        []store.TakenOverIdleRelease
	claims           *outboxClaimStore
	finished         chan json.RawMessage
	finishFails      int
	loseFinishAnswer bool
	claimsFails      int
	claimsErr        error
	holds            map[claim.Token][]supervise.Hold
	merged           bool
	holdsErr         error
	mergedErr        error
	replacementsErr  error
	forgetErr        error

	mu           sync.Mutex
	finishes     int
	taken        bool
	reads        int
	replacements []store.PodReplacement
	forgot       []string
}

// PauseRefusesProjectRelease answers that no pause holds the project: the release's refusal is
// the store's own, which TestAProjectReleaseIsRefusedWhileAPauseHoldsTheProject runs over a real
// one.
func (*releaseRecord) PauseRefusesProjectRelease(context.Context, string) error { return nil }

func (s *releaseRecord) StartProjectRelease(_ context.Context, project string, now func() time.Time, bound time.Duration) (store.ProjectRelease, []store.TakenOverIdleRelease, []store.ProjectReleaseClaim, error) {
	startedAt := now().UTC()
	return store.ProjectRelease{ID: 1, Project: project, StartedAt: startedAt, BoundAt: startedAt.Add(bound)}, s.takenOver, s.stop, nil
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

func (s *releaseRecord) ClaimHolds(_ context.Context, _ string, token claim.Token) ([]supervise.Hold, error) {
	return s.holds[token], s.holdsErr
}

func (s *releaseRecord) PullRequestMerged(context.Context, string) (bool, error) {
	return s.merged, s.mergedErr
}

// EndPodHoldStreaks ends nothing: no hold of this store's pods stands. The end of a deleted pod's
// streaks is the store's own, which TestAProjectReleaseEndsTheHoldOfThePodItDeletes runs over a
// real one.
func (*releaseRecord) EndPodHoldStreaks(context.Context, string, string) error { return nil }

// PodReplacements is replacements, the standing records of the replacements of the project's
// issue pods, or replacementsErr where it is set.
func (s *releaseRecord) PodReplacements(context.Context, string) ([]store.PodReplacement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replacementsErr != nil {
		return nil, s.replacementsErr
	}
	return slices.Clone(s.replacements), nil
}

// ForgetPodReplacement takes the record of operation out of replacements and adds operation to
// forgot, or answers forgetErr, taking nothing, where it is set.
func (s *releaseRecord) ForgetPodReplacement(_ context.Context, _, issue, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forgetErr != nil {
		return s.forgetErr
	}
	s.replacements = slices.DeleteFunc(s.replacements, func(r store.PodReplacement) bool { return r.Issue == issue && r.Operation == operation })
	s.forgot = append(s.forgot, operation)
	return nil
}

// releasePods is a runtime's part of a project release over the Sandboxes it lists: each issue
// pod's delete asks its authorize, then runs each settle under it with the read of a recorded act
// that acts scripts (readAct), and records the issue it deleted; each other Sandbox's delete
// records its name, and each release of the controller's claim records the claim and answers
// releaseErr; with deleteErr set, each delete of either kind is refused with it, once an issue
// pod's authorize and settle said yes. Each read of an issue pod's workspace is recorded (reads)
// and refused, with readErr where it is set, and each read through a pod's launcher is recorded
// (launcherReads) and answers launcherWork where it is set, launcherErr where that is set, and a
// refusal otherwise, as a pod with no launcher connected answers. The runtime's store lists the
// Sandboxes after storeListFails lists of it that fail, each with storeListErr where it is set,
// and the API server lists the same Sandboxes as its store after listFails lists of it that fail;
// storeLists and lists count the lists asked of each.
type releasePods struct {
	sandboxes      []runtime.ProjectSandbox
	storeListFails int
	listFails      int
	releaseErr     error
	deleteErr      error
	storeListErr   error
	readErr        error
	launcherWork   *runtime.WorkspaceWork
	launcherErr    error
	acts           map[runtime.RecordedAct]runtime.ActState
	actErr         error

	mu            sync.Mutex
	deleted       []string
	removed       []string
	released      []claim.Token
	reads         []runtime.UnpushedRead
	launcherReads []string
	storeLists    int
	lists         int
}

func (p *releasePods) SuspendIssue(ctx context.Context, issue, _ string, authorize func(context.Context) (bool, error),
	settle ...func(context.Context, runtime.ActRead) error) error {
	if ok, err := authorize(ctx); err != nil || !ok {
		return err
	}
	for _, s := range settle {
		if err := s(ctx, p.readAct); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deleteErr != nil {
		return p.deleteErr
	}
	p.deleted = append(p.deleted, issue)
	return nil
}

// readAct is the runtime's read of a recorded act under the pod's launch turn (runtime.ActRead):
// actErr where it is set, and otherwise the state acts names for act, uncertain for one it does
// not name.
func (p *releasePods) readAct(_ context.Context, act runtime.RecordedAct) (runtime.ActState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.actErr != nil {
		return runtime.ActUncertain, p.actErr
	}
	if state, ok := p.acts[act]; ok {
		return state, nil
	}
	return runtime.ActUncertain, nil
}

func (p *releasePods) PodWorkspaceWork(_ context.Context, issue string) (runtime.WorkspaceWork, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.launcherReads = append(p.launcherReads, issue)
	switch {
	case p.launcherWork != nil:
		return *p.launcherWork, nil
	case p.launcherErr != nil:
		return runtime.WorkspaceWork{}, p.launcherErr
	}
	return runtime.WorkspaceWork{}, errors.New("no launcher of the test's pods that reads a workspace is connected")
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

func (p *releasePods) ReadUnpushed(_ context.Context, read runtime.UnpushedRead, _ claim.Role) (runtime.UnpushedWork, error) {
	p.mu.Lock()
	p.reads = append(p.reads, read)
	p.mu.Unlock()
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

// testReleaser is a project release of the project legion over mark, st, sup's machines and pods,
// as the release's tests build one: a default bound of a minute, a stop timeout and a re-read of a
// second, a poll of 10ms, and releaseRetry between two tries and between two lists.
func testReleaser(ctx context.Context, mark *projectRelease, st projectReleaseStore, sup *supervisor, pods *releasePods) *projectReleaser {
	return &projectReleaser{ctx: ctx, project: "legion", mark: mark, store: st, machines: sup, pods: pods,
		defaultBound: time.Minute, stopTimeout: time.Second, reread: time.Second, poll: 10 * time.Millisecond,
		retry: releaseRetry, listPause: releaseRetry, now: time.Now, log: quietLogger()}
}

// runRelease runs a project release of the project legion over sup's machines, pods and records,
// whose finished channel it reads, with a bound of a second, and answers the result its finish
// recorded. A claim that never settles costs the bound and then the stop timeout before its pod is
// left (stopDeadline), so both are short. Each of configure is applied to the releaser before it
// starts.
func runRelease(t *testing.T, records *releaseRecord, sup *supervisor, pods *releasePods, configure ...func(*projectReleaser)) api.ProjectReleaseResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := testReleaser(ctx, &projectRelease{}, records, sup, pods)
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
