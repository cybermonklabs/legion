package daemon

// A reviewer starts only in an issue pod on the configured image (reviewer_pod.go). These cases
// drive the update-round rig (podupdate_test.go): real machines over a real Postgres, the runtime
// faked, and the runtime's reading of the issue pod's image scripted (podImageScript).

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

const (
	// reviewerPodConfiguredImage is the image the runtime launches every container with now, and
	// reviewerPodOldImage the one the issue's pod was built from by an earlier release.
	reviewerPodConfiguredImage = "ghcr.io/cybermonklabs/legion-worker:v19.51.0"
	reviewerPodOldImage        = "ghcr.io/cybermonklabs/legion-worker:v19.50.0"
	// reviewerPodDrift is the drift the runtime reports for that pod, spelled as podShapeDrift
	// spells an image.
	reviewerPodDrift = "image " + reviewerPodOldImage + ", now " + reviewerPodConfiguredImage
)

// podImageScript is the Sandbox runtime's reading of the issue pod's image
// (sandbox.Runtime.IssuePodImage), as a test sets it: the pod the Sandbox of issue runs now and
// the image it runs, or no live pod at all, which is what the round's replacement leaves until the
// next launch builds a fresh pod on the configured image. Every other issue has no live pod.
type podImageScript struct {
	issue string
	mu    sync.Mutex
	read  sandbox.PodImage
	live  bool
	// ended is each pod ReplaceIssuePod ended, in order.
	ended []string
}

// ReplaceIssuePod ends the issue's live pod as the Sandbox runtime does: nothing where the live
// pod is another or none, else the last word and then no live pod.
func (p *podImageScript) ReplaceIssuePod(ctx context.Context, issue, pod string, due func(context.Context) error) error {
	p.mu.Lock()
	there := issue == p.issue && p.live && p.read.Pod == pod
	p.mu.Unlock()
	if !there {
		return nil
	}
	if err := due(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ended = append(p.ended, pod)
	p.read, p.live = sandbox.PodImage{}, false
	return nil
}

// endedPods is every pod ReplaceIssuePod ended.
func (p *podImageScript) endedPods() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.ended)
}

func (p *podImageScript) IssuePodImage(issue string) (sandbox.PodImage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if issue != p.issue {
		return sandbox.PodImage{}, false
	}
	return p.read, p.live
}

// runs sets the issue's live pod and the image it runs.
func (p *podImageScript) runs(pod, image string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.read, p.live = sandbox.PodImage{Pod: pod, Image: image, Configured: reviewerPodConfiguredImage}, true
}

// replaced is the issue's Sandbox suspended by the round's replacement: no live pod.
func (p *podImageScript) replaced() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.read, p.live = sandbox.PodImage{}, false
}

// readsPodImages hands the rig's reviewer gate the scripted reading, with the issue's pod running
// image, and answers the script.
func (r *podUpdateRig) readsPodImages(image string) *podImageScript {
	r.t.Helper()
	script := &podImageScript{issue: podUpdateIssue}
	script.runs(podUpdatePod, image)
	r.s.reviewerPods.images = script
	return script
}

// spendTheMinutesOpenings makes the project's pod_update_rounds_per_minute spent at the next
// sweep: one round a minute, and this daemon opened one a moment ago (beginPodUpdateSweep).
func (r *podUpdateRig) spendTheMinutesOpenings() {
	r.s.cfg.PodUpdateRoundsPerMinute = new(1)
	r.s.podUpdateMu.Lock()
	r.s.podUpdateOpenedAt = r.clock.Now()
	r.s.podUpdateMu.Unlock()
}

// startReviewer is the daemon starting the reviewer's claim, as the outbox's start row does for a
// queued claim (RequestSpawn), and answers what the start answered.
func (r *podUpdateRig) startReviewer() error {
	r.t.Helper()
	return r.launchable(claim.RoleReviewer).Handle(r.ctx, supervise.RequestSpawn{Claim: r.tokens[claim.RoleReviewer]})
}

// reviewerLaunches is every process the fake started for the reviewer's claim (Spawn and
// Resume), split at the first replacement of the pod: the ones before it were started while the
// issue's only pod was the one the round replaces.
func (r *podUpdateRig) reviewerLaunches() (before, after []fake.Call) {
	replaced := false
	for _, call := range r.rt.Calls() {
		switch {
		case call.Method == "ReplacePod":
			replaced = true
		case (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == r.tokens[claim.RoleReviewer]:
			if replaced {
				after = append(after, call)
			} else {
				before = append(before, call)
			}
		}
	}
	return before, after
}

// heldReviewerStart is a reviewer start the rule must hold: the fake started no reviewer process,
// and the start answered a wait, which the outbox's start row backs off on.
func heldReviewerStart(t *testing.T, rig *podUpdateRig, err error) {
	t.Helper()
	if before, _ := rig.reviewerLaunches(); len(before) > 0 {
		t.Fatalf("the fake records %d reviewer process(es) started while the issue's pod %s runs %s, not the configured %s: the reviewer started in the old pod",
			len(before), podUpdatePod, reviewerPodOldImage, reviewerPodConfiguredImage)
	}
	if !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the reviewer's start in a pod on another image answered %v, want a wait", err)
	}
	if c := rig.claimOf(claim.RoleReviewer); c.State != supervise.StateQueued || c.Locator != nil {
		t.Fatalf("the reviewer is %s at %+v after its held start, want it queued with no process", c.State, c.Locator)
	}
}

// replacedLines is every line that says the issue pod is replaced before its reviewer starts.
func replacedLines(t *testing.T, rig *podUpdateRig) []map[string]any {
	t.Helper()
	return loggedMessages(t, rig.logs.String(), reviewerPodReplacedLine)
}

// wantOneReplacedLine holds that the line was written exactly once, at info, for the issue's pod,
// naming both images.
func wantOneReplacedLine(t *testing.T, rig *podUpdateRig) {
	t.Helper()
	lines := replacedLines(t, rig)
	if len(lines) != 1 {
		t.Fatalf("the daemon logged %q %d times, want once for the pod: %+v", reviewerPodReplacedLine, len(lines), lines)
	}
	line := lines[0]
	for field, want := range map[string]string{
		"level": "INFO", "issue": podUpdateIssue, "pod": podUpdatePod, "image": reviewerPodOldImage, "configured_image": reviewerPodConfiguredImage,
	} {
		if line[field] != want {
			t.Errorf("the line's %s is %v, want %s: %+v", field, line[field], want, line)
		}
	}
}

// startsInTheNewPod is the reviewer's start once the round has replaced the pod: it starts, in the
// pod that replaced the old one, and the fake records that one process and no other for it.
func startsInTheNewPod(t *testing.T, rig *podUpdateRig, images *podImageScript) {
	t.Helper()
	images.replaced()
	rig.startInPod(claim.RoleReviewer, podUpdateReplacementPod)
	before, after := rig.reviewerLaunches()
	if len(before) != 0 || len(after) != 1 {
		t.Fatalf("the fake records %d reviewer process(es) before the replacement and %d after, want none and one", len(before), len(after))
	}
}

// Before the daemon starts or relaunches a reviewer process in an issue pod, it reads whether that
// pod runs the configured image. Where it does not, the daemon starts no reviewer there: it opens
// that pod's update round at once, outside the project's limits on openings, or joins the round
// already open, and the reviewer starts in the pod the round's replacement makes. The stale-plugin
// lane relaunches no reviewer in place in such a pod, and still relaunches the other roles there.
// A pod on the configured image, and a runtime that runs no issue pods (tmux), start the reviewer
// at once.
func TestAReviewerStartInAPodOnAnotherImageReplacesThePodFirst(t *testing.T) {
	t.Run("a start opens the round at once and starts the reviewer in the new pod", func(t *testing.T) {
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester),
			podUpdateRole{role: claim.RoleReviewer, state: supervise.StateQueued})
		images := rig.readsPodImages(reviewerPodOldImage)
		rig.drifts(reviewerPodDrift)
		rig.spendTheMinutesOpenings()

		// The control: with the minute's openings spent, the drift alone opens no round.
		rig.sweepSettled()
		if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != "" {
			t.Fatalf("the drifted pod's round opened as %q under spent limits, so this case cannot show the reviewer's start opening it", c.UpdateRound)
		}

		heldReviewerStart(t, rig, rig.startReviewer())
		heldReviewerStart(t, rig, rig.startReviewer())
		wantOneReplacedLine(t, rig)

		rig.tick()
		opened := rig.awaitRound(claim.RoleImplementer, podUpdateRound)
		if want := reviewerStartReason + ", " + reviewerPodDrift; opened.UpdateReasons != want {
			t.Fatalf("the round opened for %q, want %q", opened.UpdateReasons, want)
		}
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
			rig.awaitAsked(role, podUpdateRound)
		}
		heldReviewerStart(t, rig, rig.startReviewer())
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
			rig.signal(role, "a1b2c3d")
		}
		rig.awaitReplacement(1, "once every role in the pod answered")

		startsInTheNewPod(t, rig, images)
		wantOneReplacedLine(t, rig)
	})

	t.Run("a start while a round is open over the pod joins it", func(t *testing.T) {
		inRound := func(role claim.Role) podUpdateRole {
			return podUpdateRole{role: role, state: supervise.StateReady, session: true, inPod: true, contract: api.DaemonAPIVersion,
				round: podUpdateRound, reasons: reviewerPodDrift}
		}
		rig := newPodUpdateRig(t, inRound(claim.RoleImplementer), inRound(claim.RoleTester),
			podUpdateRole{role: claim.RoleReviewer, state: supervise.StateQueued})
		images := rig.readsPodImages(reviewerPodOldImage)
		rig.drifts(reviewerPodDrift)
		rig.spendTheMinutesOpenings()

		heldReviewerStart(t, rig, rig.startReviewer())
		wantOneReplacedLine(t, rig)
		rig.sweepSettled()
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
			if c := rig.claimOf(role); c.UpdateRound != podUpdateRound || c.UpdateReasons != reviewerPodDrift {
				t.Fatalf("the %s is in round %q for %q after the reviewer's start, want the round already open, %q for %q",
					role, c.UpdateRound, c.UpdateReasons, podUpdateRound, reviewerPodDrift)
			}
		}
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
			rig.signal(role, "a1b2c3d")
		}
		rig.awaitReplacement(1, "once every role of the open round answered")

		startsInTheNewPod(t, rig, images)
	})

	t.Run("a start with no process of any claim in the pod opens the round and replaces the pod through its Sandbox", func(t *testing.T) {
		rig := newPodUpdateRig(t,
			podUpdateRole{role: claim.RoleImplementer, state: supervise.StateFailed, session: true, contract: api.DaemonAPIVersion},
			podUpdateRole{role: claim.RoleReviewer, state: supervise.StateQueued})
		images := rig.readsPodImages(reviewerPodOldImage)
		rig.spendTheMinutesOpenings()

		heldReviewerStart(t, rig, rig.startReviewer())
		wantOneReplacedLine(t, rig)
		rig.sweepSettled()
		if ended := images.endedPods(); !slices.Equal(ended, []string{podUpdatePod}) {
			t.Fatalf("the Sandbox ended pods %v, want the held pod %s once: no other role's process is needed for it", ended, podUpdatePod)
		}
		if before, _ := rig.reviewerLaunches(); len(before) != 0 {
			t.Fatalf("the fake records %d reviewer process(es) before the replacement, want none", len(before))
		}
		rig.startInPod(claim.RoleReviewer, podUpdateReplacementPod)
	})

	t.Run("a podless round waits for a suspended claim with a session until its bound", func(t *testing.T) {
		rig := newPodUpdateRig(t,
			podUpdateRole{role: claim.RoleImplementer, state: supervise.StateSuspended, session: true, contract: api.DaemonAPIVersion,
				suspendedBy: supervise.SuspendedByDaemon},
			podUpdateRole{role: claim.RoleReviewer, state: supervise.StateQueued})
		images := rig.readsPodImages(reviewerPodOldImage)

		heldReviewerStart(t, rig, rig.startReviewer())
		rig.sweepSettled()
		rig.awaitRound(claim.RoleImplementer, podUpdateRound)
		rig.sweepSettled()
		if ended := images.endedPods(); len(ended) != 0 {
			t.Fatalf("the Sandbox ended pods %v before the round's bound, while the suspended implementer, whose agent ran and may hold unpushed work, had not answered", ended)
		}

		rig.clockAt(podUpdateStart.Add(podUpdateGrace))
		rig.sweepSettled()
		if ended := images.endedPods(); !slices.Equal(ended, []string{podUpdatePod}) {
			t.Fatalf("the Sandbox ended pods %v at the round's bound, want the held pod %s once", ended, podUpdatePod)
		}
	})

	t.Run("the stale-plugin lane relaunches no reviewer in place and still relaunches another role", func(t *testing.T) {
		stale := func(role claim.Role) podUpdateRole {
			return podUpdateRole{role: role, state: supervise.StateReady, session: true, inPod: true, contract: api.DaemonAPIVersion - 1}
		}
		rig := newPodUpdateRig(t, stale(claim.RoleImplementer), stale(claim.RoleReviewer))
		rig.readsPodImages(reviewerPodOldImage)
		claims, err := rig.sup.Claims(rig.ctx)
		if err != nil {
			t.Fatalf("read the claims for the sweep: %v", err)
		}

		rig.s.refreshStalePlugins(claims, "sweep")
		rig.awaitClaim(claim.RoleImplementer, "the implementer relaunched in place onto this daemon's plugin", func(c supervise.Claim) bool {
			return c.State == supervise.StateLaunching
		})
		m, ok := rig.sup.Machine(rig.tokens[claim.RoleReviewer])
		if !ok {
			t.Fatal("the reviewer is not supervised")
		}
		m.Wait()

		if c := rig.claimOf(claim.RoleReviewer); c.State != supervise.StateReady {
			t.Fatalf("the reviewer is %s, want it left running for its pod's round", c.State)
		}
		resumes := rig.rt.CallsOf("Resume")
		if len(resumes) != 1 || resumes[0].Spec.Claim != rig.tokens[claim.RoleImplementer] {
			t.Fatalf("the lane started %d processes (%+v), want the implementer's alone", len(resumes), resumes)
		}
		if lines := replacedLines(t, rig); len(lines) != 0 {
			t.Fatalf("the lane tried to relaunch the reviewer in place, which the start's own check refused: %+v", lines)
		}
		noInstall := func(c supervise.Claim) bool { return noPluginInstall(rig.s.cfg, c) }
		if left := claimsToRefresh(claims, noInstall, rig.s.reviewerPods.behind); left != 1 {
			t.Fatalf("the lane counts %d claims left to refresh, want the implementer's alone", left)
		}
	})

	// A task for a reviewer process already running in a pod on another image is held, whichever
	// backend runs it and whatever contract it records, and the pod's round opens at once; the
	// task goes to the reviewer once it runs in the new pod. Another role of that pod is handed
	// its task as before.
	for name, tc := range map[string]struct {
		contract int
		backend  runtime.Backend
	}{
		"a task for a reviewer on Oh My Pi at this daemon's contract waits and opens the round": {contract: api.DaemonAPIVersion},
		"a task for a reviewer on Claude Code waits and opens the round":                        {contract: api.DaemonAPIVersion - 1, backend: runtime.BackendClaude},
	} {
		t.Run(name, func(t *testing.T) {
			reviewer := podUpdateRole{role: claim.RoleReviewer, state: supervise.StateReady, session: true, inPod: true, contract: tc.contract}
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), reviewer)
			if tc.backend != "" {
				rig.s.cfg.WorkerBackends = map[claim.Role]runtime.Backend{claim.RoleReviewer: tc.backend}
			}
			rig.readsPodImages(reviewerPodOldImage)
			rig.drifts(reviewerPodDrift)
			rig.spendTheMinutesOpenings()

			rig.handle(claim.RoleImplementer, supervise.RequestDeliver{Claim: rig.tokens[claim.RoleImplementer], Task: "implement",
				ID: "outbox:41", Phase: phase.Implementing, Generation: 1})
			rig.handle(claim.RoleReviewer, supervise.RequestDeliver{Claim: rig.tokens[claim.RoleReviewer], Task: "review",
				ID: "outbox:42", Phase: phase.Implementing, Generation: 1})
			rig.settleAll()
			if sent := rig.prompts(); sent[claim.RoleReviewer] != 0 || sent[claim.RoleImplementer] != 1 {
				t.Fatalf("the prompts sent are %v, want the implementer's task alone: no new task reaches a reviewer process in a pod on another image", sent)
			}
			if p := rig.claimOf(claim.RoleReviewer).Pending; p == nil || p.ID != "outbox:42" || !p.ConfirmedAt.IsZero() {
				t.Fatalf("the reviewer holds %+v, want its task pending and unsent", p)
			}
			wantOneReplacedLine(t, rig)

			rig.tick()
			opened := rig.awaitRound(claim.RoleImplementer, podUpdateRound)
			if want := reviewerStartReason + ", " + reviewerPodDrift; opened.UpdateReasons != want {
				t.Fatalf("the round opened for %q, want %q", opened.UpdateReasons, want)
			}
		})
	}

	t.Run("a start in a pod on the configured image starts at once with no round", func(t *testing.T) {
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), podUpdateRole{role: claim.RoleReviewer, state: supervise.StateQueued})
		rig.readsPodImages(reviewerPodConfiguredImage)

		rig.startInPod(claim.RoleReviewer, podUpdatePod)
		rig.sweepSettled()
		rig.sweepSettled()

		if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != "" {
			t.Fatalf("the implementer is in round %q, want no round over a pod on the configured image", c.UpdateRound)
		}
		if lines := replacedLines(t, rig); len(lines) != 0 {
			t.Fatalf("the daemon logged %+v for a pod on the configured image", lines)
		}
		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("the pod was replaced %d times, want never", len(calls))
		}
	})

	t.Run("a runtime that runs no issue pods starts the reviewer at once", func(t *testing.T) {
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), podUpdateRole{role: claim.RoleReviewer, state: supervise.StateQueued})

		rig.startInPod(claim.RoleReviewer, podUpdatePod)

		if lines := replacedLines(t, rig); len(lines) != 0 {
			t.Fatalf("the daemon logged %+v under a runtime that reads no pod image", lines)
		}
		if before, _ := rig.reviewerLaunches(); !slices.ContainsFunc(before, func(c fake.Call) bool { return c.Method == "Spawn" }) {
			t.Fatal("the fake records no reviewer start")
		}
	})
}
