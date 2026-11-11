package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// idleRigIdleTeardown and idleRigIdleCancel are the two Info lines the idle release writes
// (outbox.releaseIssue, outbox.cancelRelease). Each says that the roles of the issue are coming
// back to the pod the release was about: the first that they resume in the same sessions at the
// next wake, the second that they go on in the same pod. Both are true of a release that
// suspended live claims and of no other, which is why the release of a pod no role can use
// writes neither: it suspended nothing, every claim of its issue is failed, retired or
// suspended, and the pod is gone for good.
const (
	idleRigIdleTeardown = "outbox: released the idle pod of an issue; its roles resume in the same sessions at the next wake"
	idleRigIdleCancel   = "outbox: the idle release of an issue's pod did not finish; its roles go on in the same pod"
)

// idleRigUnownedCancel is what the daemon logs when the release of a pod no role can use did not
// finish (outbox.cancelRelease): the pod stands with every claim of it as it was, since that
// release never touched one.
const idleRigUnownedCancel = "outbox: the release of an issue's pod that no role can use did not finish; the pod stands with every claim of it as it was"

// idleRigUnownedPass is the line the second rule's pass writes once it has decided every pod of
// the project no claim owns (workflowRuntime.releaseUnownedPods). It carries how many issues the
// pass decided, how many it released and how many it held under each reason, which is what tells
// a reader of the daemon's log that this rule ran at all on a day it released nothing — a thing
// no release line can say.
const idleRigUnownedPass = "unowned release: decided every pod of the project no claim owns"

// comingBackWords are the words of the idle release's two lines that say the issue's roles are
// coming back to the pod, read one by one rather than as whole sentences so that a reworded idle
// line borrowed by this rule is still caught: `resume` carries both the idle teardown's "resume
// in the same sessions" and the idle cancel's `resumed` attribute, and the other three carry
// what each of those lines says about the roles and the pod they return to.
var comingBackWords = []string{"resume", "its roles", "the same session", "the same pod"}

// watchedLog is a logger whose Info lines a test reads while the daemon writes them, as the
// cancel tests of idle_release_test.go read the line a release wrote.
func watchedLog() (*syncBuffer, *slog.Logger) {
	logs := &syncBuffer{}
	return logs, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// releaseLogLines is every line of logs whose message is message, which is how one release's
// own line is read out of the whole log its lane wrote.
func releaseLogLines(logs *syncBuffer, message string) []string {
	var found []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="`+message+`"`) {
			found = append(found, line)
		}
	}
	return found
}

// releaseLogLine is the one line of logs whose message is message. It fails where there is not
// exactly one: each of these lines ends one act of one release, and a test reading the first of
// two would miss a second release of the same pod writing the same words.
func releaseLogLine(t *testing.T, logs *syncBuffer, message string) string {
	t.Helper()
	found := releaseLogLines(logs, message)
	if len(found) != 1 {
		t.Fatalf("the log holds %d lines saying %q, want the one; it holds:\n%s", len(found), message, logs)
	}
	return found[0]
}

// heldOnce is how the pass's line carries one pod held under hold. The counts of its holds are
// one attribute, `reason=count` per reason, and slog quotes an attribute whose value holds an
// `=`, so a pass that kept one pod reads `held="<reason>=1"`.
func heldOnce(hold workflow.IdleHold) string {
	return fmt.Sprintf("held=%q", hold.String()+"=1")
}

// saysNoRoleComesBack requires line to carry none of the words that say the issue's roles are
// coming back to the pod (comingBackWords), and the whole log to hold neither line the idle
// release writes: a release of a pod no role can use that borrowed either would tell an operator
// reading the log to wait for sessions that are failed, retired or suspended.
func saysNoRoleComesBack(t *testing.T, logs *syncBuffer, line, why string) {
	t.Helper()
	for _, word := range comingBackWords {
		if strings.Contains(line, word) {
			t.Errorf("%s says %q: %s\n%s", why, word, "no role of this pod is coming back to it", line)
		}
	}
	for _, idle := range []string{idleRigIdleTeardown, idleRigIdleCancel} {
		if strings.Contains(logs.String(), idle) {
			t.Errorf("the log holds the idle release's own line %q, which belongs to a release that suspended live claims:\n%s",
				idle, logs)
		}
	}
}

// earlierReleasesBundle leaves the issue holding the workspace bundle an earlier release of its
// pod saved and no pod has restored, saved through the store that release saves it through
// (store.PutWorkspaceBundle), and answers its size in bytes. The pod its work came off is a pod
// of an earlier life of the issue, so nothing standing now is its source and its one restore
// attempt is unspent.
//
// It is the state the record of this rule's release must be read in. This release saves no
// bundle, and the teardown's ordinary bundle read is per issue (store.WorkspaceBundleSize), so a
// teardown that made that read would record these bytes as its own and charge a reader of the
// records for work this release never saved.
func (r *idleRig) earlierReleasesBundle(t *testing.T) int {
	t.Helper()
	saved := idleRigEarlierBundle(r.issue.Key, time.Now().UTC().Add(-time.Hour))
	if err := r.st.PutWorkspaceBundle(context.Background(), r.issue.Key, idleRigEarlierRelease, saved); err != nil {
		t.Fatalf("save the earlier release's bundle of %s: %v", r.issue.Key, err)
	}
	return saved.Size()
}

// savedBundleBytes is the size of the bundle the issue holds and whether it holds one at all,
// which is how the bytes the record carries are told from the bytes the database holds.
func (r *idleRig) savedBundleBytes(t *testing.T) (int, bool) {
	t.Helper()
	size, held, err := r.st.WorkspaceBundleSize(context.Background(), r.issue.Key)
	if err != nil {
		t.Fatalf("read the saved bundle of %s: %v", r.issue.Key, err)
	}
	return size, held
}

// suspendedByOperator leaves the rig's one live claim as an operator's own suspension leaves it:
// suspended by the operator, holding no process, and backdated with the rest of the pod. It is
// the one suspension no daemon rule undoes (supervise.Claim.RuleMayResume), and the operator's
// resume brings that role back into this very pod, so the rule keeps the pod for it
// (workflow.IdleHoldUnownedOperatorSuspended).
func (r *idleRig) suspendedByOperator(t *testing.T) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), `update claims set state = $2, suspended_by = $3,
		locator = null, updated_at = now() - interval '30 minutes' where token = $1`,
		string(r.token), string(supervise.StateSuspended), string(supervise.SuspendedByOperator)); err != nil {
		t.Fatalf("suspend the implementer as an operator does: %v", err)
	}
}

// startQueuedAt queues a start for a role of the issue at the outbox id row, due in an hour so
// that a drain runs the release's own rows and leaves this one standing. It is the shape of row
// the idle release's own cancel tests queue: the daemon's `start` request, written straight into
// the outbox at an id of the test's choosing, since what a release reads of it is where it sits
// against the release's rows and nothing else.
func (r *idleRig) startQueuedAt(t *testing.T, row int64, role claim.Role) {
	t.Helper()
	payload := fmt.Sprintf(`{"op":"start","tree":%q,"role":%q,"generation":1}`, r.issue.Tree, string(role))
	if _, err := r.pool.Exec(context.Background(), `insert into outbox (id, kind, issue, payload, attempts, next_at, last_error)
		values ($1, 'supervise', $2, $3, 0, now() + interval '1 hour', '')`, row, r.issue.Key, payload); err != nil {
		t.Fatalf("queue the start of %s's %s at row %d: %v", r.issue.Key, role, row, err)
	}
}

// INFRA-1341 R6 and R9: what the release of a pod no role can use leaves behind for a deployment
// to read — the record of the release, the mark the teardown reads, the line that ends the
// release and the line its pass writes — and, as much, what none of them says.
//
// Every reading here is of a release that suspended no role, which is the whole of this rule:
// the record names no role and no bundle bytes, the mark names the rule rather than a role, and
// both lines say that the pod went with nobody in it. The idle release's own two lines say the
// opposite — the roles resume in the same sessions, the roles go on in the same pod — and an
// operator reading one of them over this release would wait for sessions that are failed,
// retired or suspended, and for a pod that is gone.
//
// The issue of every subtest that drives this rule holds the workspace bundle an earlier
// release of its pod saved and no pod has restored (earlierReleasesBundle). That is the state
// the record must be read in: the teardown's ordinary bundle read is per issue, so a release
// that made it would report another release's bytes as its own. The last subtest, which drives
// the idle rule instead, seeds no such bundle and says there why it cannot.
func TestAReleaseOfAnIssueWithNoLiveClaimRecordsItself(t *testing.T) {
	ctx := context.Background()

	t.Run("the record, the mark and the line of the release that took the pod", func(t *testing.T) {
		rig := newIdleRig(t)
		rig.stoppedImplementer(t, false)
		// A second claim, so the line the teardown writes has more than one state to name: the
		// architect an earlier release left `released` beside the implementer whose launch
		// budget ran out. Neither can take a turn, which is the case the line states.
		rig.releasedArchitect(t)
		bundle := rig.earlierReleasesBundle(t)
		logs, log := watchedLog()
		rig.runner.log = log
		began := time.Now().UTC()

		rig.w.releaseUnownedPods(ctx)

		// The mark stands while the teardown is queued, and it is what tells the teardown which
		// release it belongs to: the kind the rule wrote, naming no role at all. The check
		// constraint of issue_releases refuses an empty role under any other kind.
		// It is read as an error and not a fatal one, so that a mark of the wrong kind is
		// reported beside what the teardown then did with it rather than in place of it.
		if kind, role := rig.releaseMark(t); kind != "unowned" || role != "" {
			t.Errorf("the mark = kind %q for role %q, want the unowned kind naming no role", kind, role)
		}

		rig.drain(t)

		records := rig.records(t)
		if len(records.Releases) != 1 {
			t.Fatalf("releases recorded = %+v, want the one the teardown completed", records.Releases)
		}
		recorded := records.Releases[0]
		if recorded.Project != "legion" || recorded.Issue != rig.issue.Key || recorded.Generation != 1 {
			t.Errorf("the release = %+v, want %s at generation 1 of legion", recorded, rig.issue.Key)
		}
		if len(recorded.Roles) != 0 {
			t.Errorf("the release records the roles %v, want none: it suspended no claim, and the architect"+
				" it found released belongs to the release that suspended it", recorded.Roles)
		}
		if recorded.BundleBytes != 0 {
			t.Errorf("the release records a bundle of %d bytes, want none: it saved none, and the %d bytes"+
				" the issue holds are the earlier release's", recorded.BundleBytes, bundle)
		}
		if recorded.At.IsZero() || recorded.At.Before(began.Add(-time.Second)) ||
			recorded.At.After(time.Now().UTC().Add(time.Second)) {
			t.Errorf("the release was recorded at %s, want the moment the teardown finished, which is after %s",
				recorded.At, began)
		}
		// The bytes above are zero because this release saved nothing, and not because the
		// teardown deleted the row: the work an earlier release took out of this issue's pod is
		// the only copy of it, and the pod a later start makes is owed it.
		if size, held := rig.savedBundleBytes(t); !held || size != bundle {
			t.Errorf("the issue holds %d bundle bytes (held %t), want the earlier release's %d left as they were",
				size, held, bundle)
		}
		if len(records.Holds) != 0 {
			t.Errorf("holds recorded = %+v, want none: the pod was released, not kept", records.Holds)
		}

		// The line that ends the release: the issue, the Sandbox it deleted, and the state of
		// every claim of the issue, which is the case for the release stated claim by claim.
		line := releaseLogLine(t, logs, idleRigUnownedTeardown)
		sandbox, present := rig.rt.IssueSandbox(rig.issue.Key)
		if !present || sandbox == "" {
			t.Fatalf("the rig's runtime holds no Sandbox name for %s, so the line has none to carry", rig.issue.Key)
		}
		states := store.ClaimStates{
			{Role: claim.RoleArchitect, State: supervise.StateSuspended},
			{Role: claim.RoleImplementer, State: supervise.StateFailed},
		}
		for _, want := range []string{"issue=" + rig.issue.Key, "sandbox=" + sandbox, states.String()} {
			if !strings.Contains(line, want) {
				t.Errorf("the line that ended the release does not name %q:\n%s", want, line)
			}
		}
		saysNoRoleComesBack(t, logs, line, "the line that ended the release")
	})

	t.Run("the line the pass writes on a pass that released nothing", func(t *testing.T) {
		rig := newIdleRig(t)
		// A stopped claim that still records the process it was last launched as: the pod stands
		// past the quiet period with nobody able to take a turn in it, and the rule keeps it
		// because the teardown's own fence would refuse its claim.
		rig.stoppedImplementer(t, true)
		rig.earlierReleasesBundle(t)
		logs, log := watchedLog()
		rig.w.log = log

		rig.w.releaseUnownedPods(ctx)

		line := releaseLogLine(t, logs, idleRigUnownedPass)
		for _, want := range []string{"decided=1", "released=0",
			heldOnce(workflow.IdleHoldUnownedClaimProcess)} {
			if !strings.Contains(line, want) {
				t.Errorf("the pass's line does not carry %q, which is what a reader counts this rule's work by:\n%s",
					want, line)
			}
		}
	})

	// One pass per hold this rule can reach on a pod that stands, and what each pass records.
	// Three of them are a pod standing past the quiet period with 3 CPU and 19 GiB held by
	// something that is not work, and each is recorded under its own name, so a deployment
	// reading the records can tell which of the three it is paying for. The other three are no
	// such pod — one whose Sandbox is already gone, one whose release is already running, and
	// one that is simply not quiet yet — and a row for any of them would say a pod was kept
	// where nothing was kept (recordableHold).
	//
	// Every row reads the pass's own line as well, because the counts are where a hold the
	// records do not carry is still readable: a pass that keeps a pod and records nothing is
	// told from a pass that decided nothing at all only there.
	t.Run("one pass per hold the rule can reach", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			hold     workflow.IdleHold
			recorded bool
			seed     func(t *testing.T, rig *idleRig)
		}{
			{
				name: "a stopped claim still records a process in the pod",
				hold: workflow.IdleHoldUnownedClaimProcess, recorded: true,
				seed: func(t *testing.T, rig *idleRig) { rig.stoppedImplementer(t, true) },
			},
			{
				name: "an operator's own suspension holds a claim of the pod",
				hold: workflow.IdleHoldUnownedOperatorSuspended, recorded: true,
				seed: func(t *testing.T, rig *idleRig) { rig.suspendedByOperator(t) },
			},
			{
				name: "a start waits for delivery to a role of the pod",
				hold: workflow.IdleHoldUnownedDelivery, recorded: true,
				seed: func(t *testing.T, rig *idleRig) {
					rig.stoppedImplementer(t, false)
					// The one delivery such an issue carries: a start queued for a stopped
					// claim, which is what brings a role back into this pod.
					enqueueOutbox(t, rig.pool, rig.issues, mustOutboxRow(t, rig.issue.Key,
						record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
							Role: claim.RoleImplementer, Generation: 1}, time.Now().UTC()))
				},
			},
			{
				name: "the runtime holds no Sandbox for the issue",
				hold: workflow.IdleHoldUnownedSandboxGone,
				seed: func(t *testing.T, rig *idleRig) {
					rig.stoppedImplementer(t, false)
					rig.rt.sandboxGone = true
				},
			},
			{
				name: "a release of the pod is already under way",
				hold: workflow.IdleHoldUnownedReleasing,
				seed: func(t *testing.T, rig *idleRig) {
					rig.stoppedImplementer(t, false)
					// The mark of this rule's own release, left by the pass before: the
					// teardown is on the lane and the pod is already going.
					rig.w.releaseUnownedPods(ctx)
					if !rig.marked(t) {
						t.Fatal("the first pass left no release to be already under way")
					}
				},
			},
			{
				name: "the pod recorded something inside the quiet period",
				hold: workflow.IdleHoldUnownedNotIdle,
				seed: func(t *testing.T, rig *idleRig) {
					rig.stoppedImplementer(t, false)
					rig.wrote(t, rig.token, 2)
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rig := newIdleRig(t)
				rig.earlierReleasesBundle(t)
				tc.seed(t, rig)
				logs, log := watchedLog()
				rig.w.log = log

				rig.w.releaseUnownedPods(ctx)

				// The line is read as an error rather than a fatal one, so that what the pass
				// recorded is reported beside what it wrote.
				lines := releaseLogLines(logs, idleRigUnownedPass)
				if len(lines) != 1 || !strings.Contains(lines[0], "released=0") ||
					!strings.Contains(lines[0], heldOnce(tc.hold)) {
					t.Errorf("the pass wrote %v, want the one line holding the one pod under %s", lines, tc.hold)
				}
				holds := rig.records(t).Holds
				if !tc.recorded {
					if len(holds) != 0 {
						t.Fatalf("holds recorded = %+v, want none: %s is no pod standing idle with nobody in it",
							holds, tc.hold)
					}
					return
				}
				if len(holds) != 1 {
					t.Fatalf("holds recorded = %+v, want the one pass that kept the pod under %s", holds, tc.hold)
				}
				hold := holds[0]
				if hold.Project != "legion" || hold.Issue != rig.issue.Key || hold.Reason != tc.hold.String() {
					t.Fatalf("the hold = %+v, want %s kept under %s", hold, rig.issue.Key, tc.hold)
				}
				// This rule names one reason per pass and no kinds beside it, so the detail a
				// background-work hold carries is empty here.
				if hold.Detail != "" || hold.Passes != 1 {
					t.Fatalf("the hold = %+v, want the one pass and no detail beside the reason", hold)
				}
			})
		}
	})

	t.Run("a start queued between the mark and the teardown cancels the release", func(t *testing.T) {
		rig := newIdleRig(t)
		rig.stoppedImplementer(t, false)
		rig.earlierReleasesBundle(t)
		logs, log := watchedLog()
		rig.runner.log = log

		rig.w.releaseUnownedPods(ctx)
		// A start queued above the release's own row once its mark stood: the pod is the one
		// that must answer it, so the release goes rather than the pod. The teardown of this
		// release is its one row and so its last, which is what the start is read against.
		rig.startQueuedAt(t, rig.boundary(t)+10, claim.RoleTester)

		rig.drain(t)

		if _, suspended := rig.rt.reads(); suspended != 0 {
			t.Fatalf("Sandbox teardowns = %d, want none: the pod stands to answer the start", suspended)
		}
		if rig.marked(t) {
			t.Fatal("the cancelled release left its mark behind")
		}
		records := rig.records(t)
		if len(records.Releases) != 0 {
			t.Fatalf("releases recorded = %+v, want none: the release was cancelled", records.Releases)
		}
		if len(records.Holds) != 1 || records.Holds[0].Reason != workflow.IdleHoldOvertaken.String() {
			t.Fatalf("holds recorded = %+v, want the one %s", records.Holds, workflow.IdleHoldOvertaken)
		}
		line := releaseLogLine(t, logs, idleRigUnownedCancel)
		if !strings.Contains(line, "issue="+rig.issue.Key) ||
			!strings.Contains(line, "hold="+workflow.IdleHoldOvertaken.String()) {
			t.Errorf("the cancelled release's line does not name the issue and the hold that stopped it:\n%s", line)
		}
		// The pod stands, and not one role of it is running in it: every claim is where the
		// stop that ended it left it, so the line must not say the roles go on in the same pod.
		saysNoRoleComesBack(t, logs, line, "the cancelled release's line")
	})

	// The idle release's own cancel, cancelled before its first role stop, so that nothing of
	// the pod was ever suspended and the two cancels differ only in the release they belong to.
	// Its wording is unchanged by this rule: an idle release that stopped short leaves live
	// claims going on in the pod it kept, which is what its line has always said.
	t.Run("an ordinary idle release cancelled before its first role stop keeps its own wording", func(t *testing.T) {
		rig := newIdleRig(t)
		// This issue holds no earlier release's bundle, where every issue above does: a bundle
		// no pod has restored is a hold of the idle rule itself (IdleHoldBundleUnrestored), so
		// that rule would keep this pod rather than mark the release whose cancel is read here.
		// The second rule has no such conjunct — it saves no bundle, so an issue holding one is
		// still its pod to take — which is why the two subtests are seeded differently.
		logs, log := watchedLog()
		rig.runner.log = log
		// The save is the idle release's first act and the one act before its first role stop.
		// A bundle it cannot build stops the release there.
		rig.rt.bundleErr = errors.New("the launcher is gone")

		rig.w.releaseIdlePod(ctx, rig.issue.Key)
		rig.drain(t)

		if _, suspended := rig.rt.reads(); suspended != 0 {
			t.Fatalf("Sandbox teardowns = %d, want none: the release stopped at its save", suspended)
		}
		if rig.marked(t) {
			t.Fatal("the cancelled release left its mark behind")
		}
		// The line and the attributes it has always carried: the hold that stopped the release
		// and the roles its cancel resumed, which is the empty set here because no role stop of
		// it had run. The release of a pod no role can use carries no such attribute at all,
		// since it suspends nothing there is to resume.
		line := releaseLogLine(t, logs, idleRigIdleCancel)
		for _, want := range []string{"issue=" + rig.issue.Key,
			"hold=" + workflow.IdleHoldBundleSave.String(), "resumed=[]"} {
			if !strings.Contains(line, want) {
				t.Errorf("the idle release's cancelled line does not carry %q:\n%s", want, line)
			}
		}
		if strings.Contains(logs.String(), idleRigUnownedCancel) {
			t.Errorf("the idle release's cancel wrote the line of a release that suspends no role:\n%s", logs)
		}
	})
}
