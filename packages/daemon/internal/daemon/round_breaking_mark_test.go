package daemon

// Which rounds break their pod (R7 of #5285, spec v23): the spec's architect
// ruled at 04:00Z, on the Legion reviewer's point 2, that a drift of the Kubernetes service
// account the pod runs as breaks the pod, and that a drift of the leak check digest does not. GPT-6.1
// Sol's non-blocking point on #5285 at 0259f8af asks that the classification come from a trusted
// mark rather than from a search of the displayed reasons: the runtime marks a reason as breaking
// where it decides the drift (runtime.BreakingDrift at the reason's start, the Sandbox runtime's
// PodDrift for the account and for the exchange identity), and the round's recorded reasons carry
// the mark at their start where any reason does. A value that a reason displays can hold the
// mark's words without making the round break its pod. And the Legion reviewer's point 8: an
// earlier round's keep of the pod holds no round over a drift that breaks the pod, which no role
// can keep working under. Spec v25 adds a drift of the pod's exchange identity (the Sandbox
// runtime's movedExchangeIdentity) to the drifts that break the pod, for the account's reason, and
// spec v26 narrows it, on the Legion reviewer's Medium 2, to where a launch into the pod is refused
// until the pod's round replaces it: under dispatch_token_broker with no transition file named, a
// pod built before the exchange identity, so a round that kept such a pod would leave an issue
// whose only claim waits on it unable to run again. Its row scripts the reason as PodDrift gives it
// there (podUpdateExchangeDrift); round_exchange_drift_test.go reads each case off the real PodDrift.
//
// Each round here is over a working copy that holds a change. A round over a drift that breaks the
// pod replaces the pod after the read and tells the issue's architect what it took; any other
// round keeps the pod. The rig scripts the reasons PodDrift gives, so that the leak check digest's
// reason carries no mark is the Sandbox runtime's to hold (leak_check_test.go's
// TestAChangedManifestDigestIsDrift wants that reason exactly), and no row here stands for it
// (the Legion reviewer's minor point 12 on fa9deb04).

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// imageDrift is a drift of the image, which the roles can keep working under.
const imageDrift = "image legion-worker:v22.16.2, now legion-worker:v22.17.0"

// answerRoundOver is answerRound over every reason of reasons at once.
func (r *podUpdateRig) answerRoundOver(reasons []string, answering ...claim.Role) {
	r.t.Helper()
	r.drifts(reasons...)
	r.tick()
	for _, role := range answering {
		r.awaitAsked(role, podUpdateRound)
	}
	for _, role := range answering {
		r.signal(role, "a1b2c3d")
	}
	r.settleAll()
}

// wantReplacedAndTold holds the rig to one replacement of the pod over the change in the working
// copy, one workspace-taken notice naming it, and no keep.
func (r *podUpdateRig) wantReplacedAndTold(what string) {
	r.t.Helper()
	r.awaitReplacement(1, what)
	r.wantToldOnceNaming(what, roundChangeHead)
}

func TestOnlyAReasonThatOpensOnTheMarkMakesARoundBreakItsPod(t *testing.T) {
	t.Parallel()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	for _, tc := range []struct {
		name    string
		reasons []string
		breaks  bool
	}{
		{name: "a_drift_of_the_account", reasons: []string{podUpdateAccountDrift}, breaks: true},
		{name: "a_drift_of_the_account_after_a_drift_of_the_image", reasons: []string{imageDrift, podUpdateAccountDrift}, breaks: true},
		{name: "a_drift_of_the_exchange_identity", reasons: []string{podUpdateExchangeDrift}, breaks: true},
		{name: "control:_a_value_that_holds_the_mark's_words", reasons: []string{"image legion-worker:" + runtime.BreakingDrift + "v22.16.2, now legion-worker:v22.17.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundChanged)
			rig.answerRoundOver(tc.reasons, roles...)
			if tc.breaks {
				rig.wantReplacedAndTold("of a round over a drift that breaks the pod")
				if reason, _ := rig.keptReason(); reason != "" {
					t.Fatalf("the round recorded a keep %q, want none: it replaced the pod", reason)
				}
				return
			}
			rig.roundEndsKept("over a drift the roles can keep working under", roles...)
			if reason, pod := rig.keptReason(); reason != pods.DeclineUnpushedWork || pod != podUpdatePod {
				t.Fatalf("the decline row reads %q of pod %q, want %s of the round's pod %s", reason, pod, pods.DeclineUnpushedWork, podUpdatePod)
			}
			if told := rig.architectNotices(record.WorkspaceTakenNotice); len(told) != 0 {
				t.Fatalf("the architect was told a replacement took the pod %+v, want nothing: the round kept it", told)
			}
		})
	}
}

// The Legion reviewer's point 8: an earlier round kept the pod over a drift of the image, and the
// change is still in the working copy. The account then drifts too. The keep holds no round over
// that drift: the round opens, asks, and replaces the pod after its read, and the architect is told
// what the replacement took.
func TestAKeptPodGetsARoundOverADriftThatBreaksIt(t *testing.T) {
	t.Parallel()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundChanged)
	rig.answerRoundOver([]string{imageDrift}, roles...)
	rig.roundEndsKept("over a drift of the image", roles...)
	if reason, _ := rig.keptReason(); reason != pods.DeclineUnpushedWork {
		t.Fatalf("the decline row reads %q, want %s: this case needs the keep", reason, pods.DeclineUnpushedWork)
	}

	rig.answerRoundOver([]string{imageDrift, podUpdateAccountDrift}, roles...)
	rig.wantReplacedAndTold("of the kept pod once its account drifted")
}

// Spec v23 (the spec's architect, R7): a round keeps the reasons it recorded at its open, so a
// round open when the account drifts is still a round over the
// drift it was opened for. The round opens over a drift of the image and asks its roles; the
// account then drifts, and the roles answer. That round's read finds the change in the working
// copy and keeps the pod. The keep holds back no round over the account's drift: the next sweep
// reads that drift, opens a round over it, and that round replaces the pod after its read and
// tells the architect what it took.
func TestARoundOverTheAccountFollowsTheKeepOfARoundOpenWhenTheAccountDrifted(t *testing.T) {
	t.Parallel()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundChanged)
	rig.drifts(imageDrift)
	rig.tick()
	for _, role := range roles {
		rig.awaitAsked(role, podUpdateRound)
	}
	rig.drifts(imageDrift, podUpdateAccountDrift)
	for _, role := range roles {
		rig.signal(role, "a1b2c3d")
	}
	rig.settleAll()
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateReasons != imageDrift {
		t.Fatalf("the round records the reasons %q, want %q, which it was opened over: this case needs a round open before the account drifted", c.UpdateReasons, imageDrift)
	}
	rig.roundEndsKept("over the drift of the image it was opened for", roles...)
	if reason, _ := rig.keptReason(); reason != pods.DeclineUnpushedWork {
		t.Fatalf("the decline row reads %q, want %s: the round over the image keeps the pod", reason, pods.DeclineUnpushedWork)
	}

	rig.answerRoundOver([]string{imageDrift, podUpdateAccountDrift}, roles...)
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateReasons != "" && !strings.HasPrefix(c.UpdateReasons, runtime.BreakingDrift) {
		t.Fatalf("the second round records the reasons %q, want them to open on the account's", c.UpdateReasons)
	}
	rig.wantReplacedAndTold("of the kept pod by the round over its account")
}
