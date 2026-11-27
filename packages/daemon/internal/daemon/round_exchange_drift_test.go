package daemon

// Which drift of a pod's exchange identity breaks the pod (R7 of #5285, spec v26, on the Legion
// reviewer's Medium 2): only one where a launch into the pod is refused until its round replaces
// it. That is where the deployment sets dispatch_token_broker, names no
// dispatch_token_transition_file, and the pod carries no identity the exchange trades (the Sandbox
// runtime's dispatchCredential). A round that kept such a pod would hold for good an issue whose
// only claim waits on it. Any other drift of the exchange identity is one the pod's roles can keep
// working under: a token of another lifetime still trades, and where the identity is missing and
// no launch is refused, a role keeps the credential it had or carries the transition token.
//
// Each row runs the real Sandbox runtime's PodDrift over the fake Kubernetes API of
// sandbox_api_test.go. A runtime of the deployment the pod was built under writes the issue's
// Sandbox: its launch sets the Sandbox Running with the pod template it builds, then fails, as no
// launcher answers here, and leaves the Sandbox Suspended. The rig sets that Sandbox Running again
// and the controller stand-in makes its pod from that template. A runtime of the deployment as it
// stands now reads that pod's drift, and the round of the pod-update rig is opened over the
// reasons that read gives, over a workspace that holds one commit origin does not hold. So the
// mark on each reason is the one drift.go puts there under the row's settings, and the outcome is
// the round's own: a replacement after the read and the architect told the commit, or a keep.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// exchangeVolume is the pod volume that projects the exchange identity's token, the name a drift
// of that identity opens on (the Sandbox runtime's agentSecretsExchangeVolume).
const exchangeVolume = "agent-secrets-exchange"

// exchangeAudience is the audience the broker's exchange trades a pod's identity at.
const exchangeAudience = "legion-broker-exchange"

// enrolledPods is a deployment that enrolls every pod with the secrets broker and names no exchange
// audience: every deployment before the exchange identity.
func enrolledPods(o *sandbox.Options) {
	o.AgentSecrets = &sandbox.AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
}

// exchangingPods is enrolledPods with the exchange identity turned on, and no
// dispatch_token_broker.
func exchangingPods(o *sandbox.Options) {
	enrolledPods(o)
	o.AgentSecrets.ExchangeAudience = exchangeAudience
}

// brokeredPods is exchangingPods with dispatch_token_broker on and transition as
// dispatch_token_transition_file's token ("" for no such file).
func brokeredPods(transition string) func(*sandbox.Options) {
	return func(o *sandbox.Options) {
		exchangingPods(o)
		o.DispatchURL, o.Dispatch = "https://dispatch.internal.example", sandbox.DispatchCredential{Broker: true, Transition: transition}
	}
}

// runSandboxAgain sets the Sandbox name Running again, with edit applied to its pod template's
// spec, and has the controller stand-in make its pod. It answers that pod's uid.
func (a *issueSandboxAPI) runSandboxAgain(t *testing.T, name string, edit func(podSpec map[string]any)) string {
	t.Helper()
	held := a.sandbox(name)
	if held == nil {
		t.Fatalf("the API holds no Sandbox %s to run again", name)
	}
	object := kubeClone(held)
	spec, _ := object["spec"].(map[string]any)
	template, _ := spec["podTemplate"].(map[string]any)
	podSpec, _ := template["spec"].(map[string]any)
	if podSpec == nil {
		t.Fatalf("the Sandbox %s holds no pod template, so no launch wrote one", name)
	}
	edit(podSpec)
	spec["operatingMode"] = "Running"
	a.mu.Lock()
	a.putLocked("sandboxes", name, object)
	a.mu.Unlock()
	a.reconcile()
	a.mu.Lock()
	pod := a.objects["pods"][name]
	a.mu.Unlock()
	if pod == nil {
		t.Fatalf("the controller stand-in made no pod of the Sandbox %s", name)
	}
	uid, _ := pod["metadata"].(map[string]any)["uid"].(string)
	return uid
}

// exchangeTokenLives sets the lifetime of the token pod's exchange volume projects to seconds.
func exchangeTokenLives(t *testing.T, seconds int) func(map[string]any) {
	return func(podSpec map[string]any) {
		t.Helper()
		volumes, _ := podSpec["volumes"].([]any)
		for _, v := range volumes {
			volume, _ := v.(map[string]any)
			if volume["name"] != exchangeVolume {
				continue
			}
			projected, _ := volume["projected"].(map[string]any)
			sources, _ := projected["sources"].([]any)
			for _, s := range sources {
				if token, _ := s.(map[string]any)["serviceAccountToken"].(map[string]any); token != nil {
					token["expirationSeconds"] = float64(seconds)
					return
				}
			}
		}
		t.Fatalf("the pod template's volumes %v hold no %s token, so the pod carries no exchange identity and the case proves nothing", volumes, exchangeVolume)
	}
}

// exchangeDriftOf is what the Sandbox runtime's PodDrift answers, under the deployment now, for the
// implementer's process in an issue pod that a launch under the deployment built made. lifetime,
// where it is not zero, is the lifetime of the exchange token that pod projects, which no setting
// moves: a pod that a build of another lifetime made.
func exchangeDriftOf(t *testing.T, built, now func(*sandbox.Options), lifetime int) []string {
	t.Helper()
	api := newIssueSandboxAPI()
	url := api.serve(t)
	token := mustClaimToken(t, podUpdateIssue, claim.RoleImplementer)
	name := sandbox.SandboxName(token)
	prompt := filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(prompt, []byte("You are the role.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Cleanups run last first, so each runtime, registered after the server, stops before it.
	buildCtx, stopBuild := context.WithCancel(context.Background())
	t.Cleanup(stopBuild)
	opts := issueSandboxOptions(t, firstLaunchStore{}, quietLogger())
	opts.BootTimeout, opts.TerminationGrace = 300*time.Millisecond, 100*time.Millisecond
	built(&opts)
	builder, err := sandbox.New(buildCtx, sandboxRESTConfig(url), opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := runtime.SpawnSpec{
		Claim: token, Project: podUpdateProject, Tree: podUpdateIssue, Issue: podUpdateIssue, Role: claim.RoleImplementer,
		Generation: 1, BootToken: "boot-implementer", Prompt: runtime.PromptParts{RolePromptPaths: []string{prompt}},
		Repository: ghrepo.MustParse("acme/widgets"), DefaultBranch: "main",
	}
	if _, err := builder.Spawn(buildCtx, spec); err == nil || !strings.Contains(err.Error(), "wait for its new pod") {
		t.Fatalf("the launch that builds the pod = %v, want it to set the Sandbox Running and wait out a pod the stand-in never made", err)
	}
	stopBuild()
	if mode := api.mode(name); mode != "Suspended" {
		t.Fatalf("the Sandbox %s after the failed launch is %s, want it Suspended with the template that launch wrote", name, mode)
	}
	edit := func(map[string]any) {}
	if lifetime != 0 {
		edit = exchangeTokenLives(t, lifetime)
	}
	podUID := api.runSandboxAgain(t, name, edit)

	readCtx, stopRead := context.WithCancel(context.Background())
	t.Cleanup(stopRead)
	opts = issueSandboxOptions(t, firstLaunchStore{}, quietLogger())
	now(&opts)
	reader, err := sandbox.New(readCtx, sandboxRESTConfig(url), opts)
	if err != nil {
		t.Fatal(err)
	}
	loc := runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(podUID, 1),
		Sandbox: &runtime.SandboxLocator{Namespace: sandboxNamespace, Name: name, PodUID: podUID, Container: runtime.IssuePodContainer, Generation: 1},
	}
	var reasons []string
	if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), testwait.Poll, func() bool {
		reasons, err = reader.PodDrift(readCtx, runtime.Known{Claim: token, Locator: &loc})
		return err == nil
	}) {
		t.Fatalf("PodDrift of pod %s: %v", podUID, err)
	}
	api.requireServed(t)
	return reasons
}

// exchangeReason is the reason of reasons that names the exchange identity, the mark taken off,
// and false where none does.
func exchangeReason(reasons []string) (string, bool) {
	for _, reason := range reasons {
		reason, _ = strings.CutPrefix(reason, runtime.BreakingDrift)
		if strings.HasPrefix(reason, exchangeVolume+" ") {
			return reason, true
		}
	}
	return "", false
}

func TestAnExchangeIdentityDriftBreaksThePodOnlyWhereALaunchIntoItIsRefused(t *testing.T) {
	t.Parallel()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	for _, tc := range []struct {
		name string
		// built is the deployment the pod was built under, lifetime the lifetime of the exchange
		// token it projects where a build of another lifetime made it, and now the deployment its
		// drift is read under.
		built    func(*sandbox.Options)
		lifetime int
		now      func(*sandbox.Options)
		// names is what the exchange identity's reason says the pod holds.
		names    string
		replaces bool
	}{
		{name: "the_broker_with_no_transition_file_and_a_pod_with_no_identity", built: enrolledPods, now: brokeredPods(""),
			names: exchangeVolume + " (unset), now", replaces: true},
		{name: "the_same_pod_with_a_transition_file_named", built: enrolledPods, now: brokeredPods("shared-transition-token"),
			names: exchangeVolume + " (unset), now"},
		{name: "the_same_pod_without_the_broker", built: enrolledPods, now: exchangingPods,
			names: exchangeVolume + " (unset), now"},
		{name: "a_pod_whose_exchange_token_has_another_lifetime", built: exchangingPods, lifetime: 7200, now: brokeredPods(""),
			names: exchangeVolume + " audience=" + exchangeAudience + " expiry=7200s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reasons := exchangeDriftOf(t, tc.built, tc.now, tc.lifetime)
			t.Logf("PodDrift under the row's settings: %q", reasons)
			if reason, ok := exchangeReason(reasons); !ok || !strings.HasPrefix(reason, tc.names) {
				t.Fatalf("PodDrift = %q, want the exchange identity's reason, saying %q: without it the case proves nothing", reasons, tc.names)
			}

			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
			rig.answerRoundOver(reasons, roles...)
			if tc.replaces {
				// The round's end is awaited first, so a round that keeps the pod fails here at once
				// rather than at the wait for a replacement that never comes.
				for _, role := range roles {
					rig.awaitSwept(role, "the round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
				}
				if calls := rig.replacements(); len(calls) != 1 {
					reason, _ := rig.keptReason()
					t.Fatalf("the round ended with %d replacements through a process (%+v) and the keep %q, want the pod replaced once: the drift breaks the pod",
						len(calls), calls, reason)
				}
				rig.wantToldOnce("of a round over a drift that breaks the pod")
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
