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
// it cannot settle leaves the pod, with a reason that names it.

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

// releaseRoundPod runs a project release of the rig's project over mark, on the rig's store and
// machines, with the issue's one pod listed and the release's lines in the rig's log, and answers
// that pod's line once the release finished.
func (r *podUpdateRig) releaseRoundPod(mark *projectRelease) api.ReleasedSandbox {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.t.Cleanup(cancel)
	listed := &releasePods{sandboxes: []runtime.ProjectSandbox{{Name: roundReleasePod, Kind: runtime.ProjectSandboxIssue,
		Issue: podUpdateIssue, Tree: podUpdateIssue, PodUID: podUpdatePod, ConfiguredImage: true}}}
	release := testReleaser(ctx, mark, releasingStore{r.st}, r.sup, listed)
	release.pods, release.log = &roundPods{releasePods: listed, rt: r.rt}, r.log
	if _, started, err := release.Start(ctx, time.Second, false); err != nil || !started {
		r.t.Fatalf("Start = started %v, %v; want the release recorded", started, err)
	}
	var finished store.ProjectRelease
	testwait.Eventually(r.t, "the project release finished", func() bool {
		got, found, err := r.st.ReadProjectRelease(ctx, podUpdateProject)
		finished = got
		return err == nil && found && !got.FinishedAt.IsZero()
	})
	view, err := api.ProjectReleaseResponseOf(finished)
	if err != nil {
		r.t.Fatalf("read the finished release: %v", err)
	}
	return releaseLine(r.t, view, roundReleasePod)
}

// standingOperation is the operation of the one record of a replacement the store holds.
func (r *podUpdateRig) standingOperation() string {
	r.t.Helper()
	var operation string
	if err := r.pool.QueryRow(r.ctx, `select operation from pod_replacements`).Scan(&operation); err != nil {
		r.t.Fatalf("read the one record of a pod replacement: %v", err)
	}
	return operation
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
// at the next boot is true; one whose act was not made is forgotten. A record that reads uncertain,
// a read that fails, a store that cannot read the records and a forget that fails each leave the
// pod undeleted, with a reason naming the record and what was uncertain or failed, and the release
// forgets no record then. A record of another issue is not read.
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
	}{
		{name: "a record whose act was made stands, and one not made is forgotten", standing: []store.PodReplacement{notMade, made, another},
			forgot: []string{"op-not-made"}, stands: []string{"op-made", "op-another-issue"}},
		{name: "a record that reads uncertain leaves the pod", standing: []store.PodReplacement{notMade, uncertain},
			stands: []string{"op-not-made", "op-uncertain"}, says: []string{"the runtime cannot say whether the recorded replacement op-uncertain"}},
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
			pods := &releasePods{sandboxes: []runtime.ProjectSandbox{{Name: launcherReadPod, Kind: runtime.ProjectSandboxIssue,
				Issue: "LEGION-208", Tree: "LEGION-208", PodUID: "uid-208", ConfiguredImage: true}},
				acts: acts, actErr: tc.actErr, launcherWork: &runtime.WorkspaceWork{Head: passedOverHead}}
			line := releaseLine(t, api.ProjectReleaseResponse{Result: runRelease(t, records, sup, pods)}, launcherReadPod)
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
		})
	}
}
