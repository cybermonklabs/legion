package daemon

// Review round 13's first finding on cybermonklabs/legion#4049 (dispatch://INFRA-3583): a record of
// a replacement of an issue pod (pod_replacements) that the project release's mark left standing
// must not outlive the pod the release deletes. Once the pod is gone, the next boot's sweep reads
// the recorded act made (finishReplacements; the Sandbox runtime answers runtime.ActMade for a
// Sandbox that is gone) and tells the issue's architect of a replacement that never took the pod.
// Two ways leave such a record:
//
//   - a launch's reading of the round answers yes and keeps the replacement in memory
//     (roundPodEndsAtLaunch, replacedAtLaunch) while the mark is still clear, the mark is set just
//     after, and the runtime refuses the launch before it suspends anything (the Sandbox
//     runtime's releaseHolds, which internal/runtime/sandbox's
//     TestALaunchTheProjectReleaseRefusesAfterRoundPodEndsLeavesItsPodForTheSettle holds), so the
//     record stands with the pod still running;
//   - a round's call that failed for another reason, here one that ran out waiting for the pod's
//     launch turn, leaves its record for the next sweep (replaceForRound), and no sweep reads it
//     while the mark holds.
//
// The release settles each record of the pod under the pod's launch turn its delete holds, before
// the delete (settleReplacements): an act not made is forgotten, an act made stands, and a record
// it cannot settle leaves the pod, with a reason that names it, as a store that cannot read the
// records does, with a reason that says so.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// roundReleasePod is the Sandbox of the pod update rig's issue.
const roundReleasePod = "legion-legion-legion-484"

// releaseForgotLine is the release's line for a record of a replacement it forgot before its
// delete (settleReplacements).
const releaseForgotLine = "project release: forgot the record of a replacement of the issue's pod that took nothing, before it deletes the pod"

// roundPods is the pod update rig's runtime as a project release reaches it: releasePods over the
// issue's one pod, whose delete is the fake runtime's end of that pod (fake.Runtime.EndIssuePod),
// which runs the release's settle under the fake's launch turn with the fake's read of a recorded
// act, and whose read through the pod's launcher is the fake's.
type roundPods struct {
	*releasePods
	rt *fake.Runtime
}

func (p *roundPods) SuspendIssue(ctx context.Context, issue, _ string, authorize func(context.Context) (bool, error),
	settle ...func(context.Context, runtime.ActRead) error) error {
	return p.rt.EndIssuePod(ctx, issue, authorize, settle...)
}

func (p *roundPods) PodWorkspaceWork(ctx context.Context, issue string) (runtime.WorkspaceWork, error) {
	return p.rt.PodWorkspaceWork(ctx, issue)
}

// roundReleaser is a project release of the rig's project over mark that lives for ctx, on the
// rig's store and machines, with the issue's one pod listed and the release's lines in the rig's
// log.
func (r *podUpdateRig) roundReleaser(ctx context.Context, mark *projectRelease) *projectReleaser {
	listed := &releasePods{sandboxes: []runtime.ProjectSandbox{{Name: roundReleasePod, Kind: runtime.ProjectSandboxIssue,
		Issue: podUpdateIssue, Tree: podUpdateIssue, PodUID: podUpdatePod, ConfiguredImage: true}}}
	release := testReleaser(ctx, mark, releasingStore{r.st}, r.sup, listed)
	release.pods, release.log = &roundPods{releasePods: listed, rt: r.rt}, r.log
	return release
}

// releaseRoundPod runs a project release of the rig's project over mark (roundReleaser), and
// answers the issue's pod's line once the release finished (roundReleaseLine).
func (r *podUpdateRig) releaseRoundPod(mark *projectRelease) api.ReleasedSandbox {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.t.Cleanup(cancel)
	if _, started, err := r.roundReleaser(ctx, mark).Start(ctx, time.Second, false); err != nil || !started {
		r.t.Fatalf("Start = started %v, %v; want the release recorded", started, err)
	}
	return r.roundReleaseLine()
}

// roundReleaseLine waits for the project release of the rig's project to finish, and answers the
// line of the issue's pod.
func (r *podUpdateRig) roundReleaseLine() api.ReleasedSandbox {
	r.t.Helper()
	var finished store.ProjectRelease
	testwait.Eventually(r.t, "the project release finished", func() bool {
		got, found, err := r.st.ReadProjectRelease(context.Background(), podUpdateProject)
		finished = got
		return err == nil && found && !got.FinishedAt.IsZero()
	})
	view, err := api.ProjectReleaseResponseOf(finished)
	if err != nil {
		r.t.Fatalf("read the finished release: %v", err)
	}
	return releaseLine(r.t, view, roundReleasePod)
}

// standingOperation is the operation of the one record of a replacement of the rig's issue that
// the store holds; the test fails unless the store holds exactly one record of the issue.
func (r *podUpdateRig) standingOperation() string {
	r.t.Helper()
	standing, err := r.st.PodReplacements(r.ctx, podUpdateProject)
	if err != nil {
		r.t.Fatalf("read the records of pod replacements: %v", err)
	}
	standing = slices.DeleteFunc(standing, func(recorded store.PodReplacement) bool { return recorded.Issue != podUpdateIssue })
	if len(standing) != 1 {
		r.t.Fatalf("the store holds %d records of a replacement of %s's pod (%+v), want exactly one", len(standing), podUpdateIssue, standing)
	}
	return standing[0].Operation
}

// wantSettledByTheRelease runs the project release over mark, the mark the rig's supervision
// reads, and holds it to the rig's one record, of a replacement that took nothing, forgotten
// before the delete, with the release's line naming the issue, the pod and the operation; the pod
// deleted once, with what its launcher read, since no role process runs in it; and no notice to
// the issue's architect at the next boot.
func (r *podUpdateRig) wantSettledByTheRelease(mark *projectRelease) {
	r.t.Helper()
	operation := r.standingOperation()
	line := r.releaseRoundPod(mark)
	if records := r.replacementRecords(); records != 0 {
		r.t.Errorf("the store holds %d pod_replacements record(s) of %s after the release deleted its pod, want none: the release forgets a record whose act was not made before its delete",
			records, podUpdateIssue)
	}
	if forgot := loggedLines(r.t, r.logs.String(), releaseForgotLine); len(forgot) != 1 || forgot[0]["issue"] != podUpdateIssue ||
		forgot[0]["pod"] != podUpdatePod || forgot[0]["operation"] != operation {
		r.t.Errorf("the release logged %v for the records it forgot, want one line naming %s, pod %s and operation %s", forgot, podUpdateIssue, podUpdatePod, operation)
	}
	if line.Outcome != api.ReleaseDeletedLost || line.Commits == nil || *line.Commits != roundUnpushed.Count || line.Reason != releaseLauncherRead {
		r.t.Errorf("the pod's line is %s, want deleted_lost with the %d commit its launcher read, and that read as its reason", lineJSON(line), roundUnpushed.Count)
	}
	if ended := r.rt.CallsOf("EndIssuePod"); len(ended) != 1 || ended[0].Issue != podUpdateIssue {
		r.t.Errorf("the release ended the pods %+v, want %s's once", ended, podUpdateIssue)
	}
	r.wantNoNoticeAtTheNextBoot()
}

// wantNoNoticeAtTheNextBoot restarts the rig as the next boot does, on a runtime whose Sandbox of
// the issue holds no pod, as the release left it, has that boot's sweep finish the records that
// stand (finishReplacements), and holds it to no workspace-taken notice to the issue's architect
// and no record left.
func (r *podUpdateRig) wantNoNoticeAtTheNextBoot() {
	r.t.Helper()
	next := r.restart()
	next.rt.ScriptIssuePod(podUpdateIssue, "")
	claims, err := next.sup.Claims(next.ctx)
	if err != nil {
		r.t.Fatalf("read the claims at the next boot: %v", err)
	}
	if _, err := next.s.finishReplacements(next.ctx, claims); err != nil {
		r.t.Fatalf("the next boot's finish of the standing records: %v", err)
	}
	if told := next.architectNotices(record.WorkspaceTakenNotice); len(told) != 0 {
		r.t.Errorf("the next boot told the architect %+v, want no notice: no recorded replacement took the pod the release deleted", told)
	}
	if records := next.replacementRecords(); records != 0 {
		r.t.Errorf("the store holds %d pod_replacements record(s) after the next boot's sweep, want none", records)
	}
}

// (a): the launch's reading of the round answers yes, its record written and its replacement kept
// in memory, while the mark is clear. The mark is set then, and the runtime refuses the launch
// before it suspends anything, so the pod still runs as the issue's own: the act is not made, and
// the release forgets its record before it deletes the pod.
func TestProjectReleaseSettlesTheRecordOfALaunchRefusedAfterRoundPodEnds(t *testing.T) {
	t.Parallel()
	rig := podlessBreakingRound(t)
	mark := &projectRelease{}
	rig.s.plan.release = mark
	if ends, err := rig.s.roundPodEndsAtLaunch(rig.ctx, rig.tokens[claim.RoleImplementer], podUpdatePod); err != nil || !ends {
		t.Fatalf("the launch's reading of the round = %t, %v; want the pod replaced", ends, err)
	}
	if records := rig.replacementRecords(); records != 1 {
		t.Fatalf("the store holds %d records once the launch's reading answered, want the launch's", records)
	}
	if _, kept := rig.s.launchReplaced(podUpdateIssue, podUpdateRound); !kept {
		t.Fatal("the daemon keeps no replacement at the launch in memory, or this case proves nothing")
	}
	rig.wantSettledByTheRelease(mark)
}

// (b): the round's replacement by issue runs out waiting for the pod's launch turn, as a call that
// ran out of time does, and leaves its record standing with the pod still running. The release
// forgets that record before it deletes the pod.
func TestProjectReleaseSettlesTheRecordOfARoundCallThatRanOut(t *testing.T) {
	t.Parallel()
	rig := podlessBreakingRound(t)
	rig.rt.FailReplaceIssuePod(context.DeadlineExceeded)
	rig.tick()
	if asked := rig.byIssueAsks(); len(asked) != 1 {
		t.Fatalf("the replacement by issue was asked %+v, want once", asked)
	}
	if records := rig.replacementRecords(); records != 1 {
		t.Fatalf("the store holds %d records once the round's call ran out, want the round's", records)
	}
	rig.rt.FailReplaceIssuePod(nil)
	mark := &projectRelease{}
	rig.s.plan.release = mark
	rig.wantSettledByTheRelease(mark)
}

// (c): the release's decision of each record of the pod, from the runtime's read of its act under
// the pod's launch turn. A record whose act was made took an earlier pod and stands, and its notice
// at the next boot is true; one whose act was not made is forgotten. A record that reads uncertain
// and a read that fails leave the pod undeleted, with a reason naming the record and what was
// uncertain or failed, and the release forgets no record then; a forget that fails leaves it with
// a reason naming the record and the error, and a store that cannot read the records with a reason
// saying so. A record of another issue is not read. A claim of the stop set in a pod left ends held
// for an operator.
func TestProjectReleaseSettlesThePodsReplacementRecordsByTheirActs(t *testing.T) {
	t.Parallel()
	notMade := store.PodReplacement{Operation: "op-not-made", Issue: "LEGION-208", PodUID: "uid-208", Act: runtime.PodAct("uid-208")}
	made := store.PodReplacement{Operation: "op-made", Issue: "LEGION-208", PodUID: "uid-207", Act: runtime.PodAct("uid-207")}
	uncertain := store.PodReplacement{Operation: "op-uncertain", Issue: "LEGION-208", PodUID: "uid-208", Act: runtime.VolumeAct("uid-sandbox-208")}
	// The pods' read answers uncertain for an act it names no state of, so a read of this record
	// would leave the pod.
	another := store.PodReplacement{Operation: "op-another-issue", Issue: "LEGION-209", PodUID: "uid-209", Act: runtime.PodAct("uid-209")}
	acts := map[runtime.RecordedAct]runtime.ActState{notMade.Act: runtime.ActNotMade, made.Act: runtime.ActMade, uncertain.Act: runtime.ActUncertain}
	for _, tc := range []struct {
		name                            string
		standing                        []store.PodReplacement
		actErr, replacementsErr, forget error
		forgot, stands                  []string
		// says is what the reason of the line of a pod left says, nil for a pod deleted.
		says []string
		// stopped puts a claim of the stop set in the pod, which the release stops and, as it
		// leaves the pod, holds for an operator.
		stopped bool
	}{
		{name: "a record whose act was made stands, and one not made is forgotten", standing: []store.PodReplacement{notMade, made, another},
			forgot: []string{"op-not-made"}, stands: []string{"op-made", "op-another-issue"}},
		{name: "a record that reads uncertain leaves the pod", standing: []store.PodReplacement{notMade, uncertain},
			stands: []string{"op-not-made", "op-uncertain"}, says: []string{"the runtime cannot say whether the recorded replacement op-uncertain"}, stopped: true},
		{name: "a read of an act that fails leaves the pod", standing: []store.PodReplacement{notMade},
			actErr: errors.New("read sandbox legion-legion-legion-208: the API server answered 503"),
			stands: []string{"op-not-made"}, says: []string{"op-not-made", "the API server answered 503"}},
		{name: "a store that cannot read the records leaves the pod", replacementsErr: errors.New("the store did not answer"),
			says: []string{"the records of the replacements of its pod could not be read", "the store did not answer"}},
		{name: "a forget that fails leaves the pod", standing: []store.PodReplacement{notMade}, forget: errors.New("the store refused the delete"),
			stands: []string{"op-not-made"}, says: []string{"op-not-made", "the store refused the delete"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			records := &releaseRecord{claims: sup.deps.Store.(*outboxClaimStore), finished: make(chan json.RawMessage, 1),
				replacements: slices.Clone(tc.standing), replacementsErr: tc.replacementsErr, forgetErr: tc.forget}
			token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
			if tc.stopped {
				// A claim the release stops, which runs no role process, so the pod is still read
				// through its launcher.
				if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208",
					Issue: "LEGION-208", Role: claim.RoleImplementer, Session: "ses_implementer", State: supervise.StateSuspended,
					SuspendedBy: supervise.SuspendedByProjectRelease}, ""); err != nil {
					t.Fatal(err)
				}
				records.stop = []store.ProjectReleaseClaim{{Token: token, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer,
					Session: "ses_implementer", State: supervise.StateSuspended}}
			}
			pods := &releasePods{sandboxes: []runtime.ProjectSandbox{{Name: launcherReadPod, Kind: runtime.ProjectSandboxIssue,
				Issue: "LEGION-208", Tree: "LEGION-208", PodUID: "uid-208", ConfiguredImage: true}},
				acts: acts, actErr: tc.actErr, launcherWork: &runtime.WorkspaceWork{Head: passedOverHead}}
			release := api.ProjectReleaseResponse{Result: runRelease(t, records, sup, pods)}
			line := releaseLine(t, release, launcherReadPod)
			pods.mu.Lock()
			deleted := slices.Clone(pods.deleted)
			pods.mu.Unlock()
			if tc.says == nil {
				if line.Outcome != api.ReleaseDeletedClean || !slices.Equal(deleted, []string{"LEGION-208"}) {
					t.Errorf("the pod's line is %s and the release deleted the pods of %v, want LEGION-208's deleted_clean", lineJSON(line), deleted)
				}
			} else {
				if line.Outcome != api.ReleaseLeft || len(deleted) != 0 {
					t.Errorf("the pod's line is %s and the release deleted the pods of %v, want the pod left: the release does not delete over a record it cannot settle",
						lineJSON(line), deleted)
				}
				for _, said := range tc.says {
					if !strings.Contains(line.Reason, said) {
						t.Errorf("the left pod's reason is %q, want it to say %q", line.Reason, said)
					}
				}
			}
			records.mu.Lock()
			forgot, stands := slices.Clone(records.forgot), make([]string, 0, len(records.replacements))
			for _, r := range records.replacements {
				stands = append(stands, r.Operation)
			}
			records.mu.Unlock()
			if !slices.Equal(forgot, tc.forgot) || !slices.Equal(stands, tc.stands) {
				t.Errorf("the release forgot %v and left %v standing, want %v forgotten and %v standing", forgot, stands, tc.forgot, tc.stands)
			}
			if !tc.stopped {
				return
			}
			if ended := releaseClaimLine(t, release, token); ended.Ended != api.ReleasedClaimEnd(supervise.SuspendedByOperator) || ended.BootStarts {
				t.Errorf("the stopped claim's line is %+v, want it ended operator, which the next boot does not start: the release holds each claim of a pod it leaves for an operator",
					ended)
			}
			stored, ok, err := records.ClaimByToken(context.Background(), token)
			if err != nil || !ok || stored.State != supervise.StateSuspended || stored.SuspendedBy != supervise.SuspendedByOperator {
				t.Errorf("the stopped claim is stored %s by %q (%t, %v), want suspended by an operator", stored.State, stored.SuspendedBy, ok, err)
			}
		})
	}
}

// sweepInFlightWindow is how long TestProjectReleaseWaitsForAPodUpdateSweepInFlight holds a pass
// of the sweep in flight once the release has set its mark: a release that does not wait for that
// pass deletes the rig's pod well within it, since nothing of the pod has it wait (no role process
// runs there, and its one claim is suspended already).
const sweepInFlightWindow = 2 * time.Second

// The simplify pass over review round 13's fixes on cybermonklabs/legion#4049: a pass of the sweep
// that read the release's mark clear before the mark was set (sweepOnce) runs on into the pod
// update sweep, whose round over a drift that breaks the pod writes the record of its replacement
// by issue (recordRoundReplacement) once the release has begun. The release waits for that pass
// once it has set its mark (projectRelease.awaitSweep), so it records, reads and deletes nothing
// until the pass has ended; the round's call meets the runtime's refusal for the mark, its record
// is forgotten, and the release deletes the pod with no record standing and no notice queued.
// Without the wait, the release's settle reads the records before the round's is written and
// deletes the pod, the call then finds the pod taken and answers nil, and the round ends as a
// replacement made, telling the issue's architect of a replacement that never took the pod.
func TestProjectReleaseWaitsForAPodUpdateSweepInFlight(t *testing.T) {
	t.Parallel()
	rig := podlessBreakingRound(t)
	mark := &projectRelease{}
	rig.s.plan.release = mark
	// The Sandbox runtime refuses, while the mark holds, the replacement of a pod it finds still
	// there (replaceRoundPod's releaseHolds, after its podTaken). The fake reads no mark, so it
	// answers that refusal to each replacement by issue of a pod it still holds: the sweep's call
	// comes only after the release has set the mark.
	rig.rt.FailReplaceIssuePod(runtime.ErrProjectReleased)

	letGo := rig.holdRecordWrites()
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		// The pass runs on a hold mark of its own, as the orphan sweep's goroutine does.
		rig.s.sweepOnce(store.ForConcurrentWorker(context.Background()))
	}()
	t.Cleanup(func() { letGo(); <-swept })
	rig.awaitRecordWriteHeld()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	started := make(chan error, 1)
	go func() {
		_, ok, err := rig.roundReleaser(ctx, mark).Start(ctx, time.Second, false)
		if err == nil && !ok {
			err = errors.New("the start answered without recording a release")
		}
		started <- err
	}()
	testwait.Eventually(t, "the release to set its mark", mark.held)
	if testwait.Holds(sweepInFlightWindow, 10*time.Millisecond, func() bool { return len(rig.rt.CallsOf("EndIssuePod")) > 0 }) {
		t.Errorf("the release deleted the pod while a pass of the sweep that read its mark clear was still in flight, want it to wait for that pass")
	}
	letGo()
	testwait.Receive(t, "the pass of the sweep to end", swept)
	if err := testwait.Receive(t, "the release's start to answer", started); err != nil {
		t.Fatalf("Start = %v; want the release recorded", err)
	}
	line := rig.roundReleaseLine()

	if line.Outcome != api.ReleaseDeletedLost {
		t.Errorf("the pod's line is %s, want deleted_lost by what its launcher read", lineJSON(line))
	}
	var methods []string
	for _, call := range rig.rt.Calls() {
		methods = append(methods, call.Method)
	}
	replaced := slices.IndexFunc(methods, func(method string) bool { return strings.HasPrefix(method, "ReplaceIssuePod") })
	if ended := slices.Index(methods, "EndIssuePod"); replaced < 0 || ended < 0 || replaced > ended {
		t.Errorf("the runtime was called %v, want the round's replacement by issue asked before the release's delete of the pod", methods)
	}
	if records := rig.replacementRecords(); records != 0 {
		t.Errorf("the store holds %d pod_replacements record(s) once the release deleted the pod, want none", records)
	}
	if told := rig.rows(record.OutboxKindNotice); len(told) != 0 {
		t.Errorf("the outbox holds the notices %+v, want none: no replacement took the pod the release deleted", told)
	}
	rig.wantNoNoticeAtTheNextBoot()
}
