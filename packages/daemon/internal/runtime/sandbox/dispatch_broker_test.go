package sandbox

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Under dispatch_token_broker (DispatchCredential.Broker) a launch into a pod that carries an
// exchange identity carries no Dispatch token: not in its start command's Files, which the
// launcher writes into the generation's private directory, and so no DISPATCH_TOKEN_FILE in its
// role's environment, which is told DISPATCH_TOKEN_BROKER=1 instead. A launch that keeps a pod
// built before that identity carries the transition token as a launch carried the bearer before,
// or, with none named, waits for the pod's update round (dispatch://INFRA-4425 R3, R4).

// testTransitionToken is the shared Dispatch token dispatch_token_transition_file holds.
const testTransitionToken = "shared-transition-token-3a9f"

// dispatchBearer is a deployment before the setting: Dispatch with the shared bearer, its pods
// enrolled with the secrets broker and carrying no exchange identity.
func dispatchBearer(o *Options) {
	o.AgentSecrets = &AgentSecrets{URL: keptBrokerURL, Audience: "agent-secrets", TokenExpiry: time.Hour}
	o.DispatchURL, o.Dispatch = testDispatchURL, DispatchCredential{Bearer: "dispatch-bearer"}
}

// dispatchBroker is the same deployment with dispatch_token_broker on, beside the exchange
// audience the setting needs, and transition as dispatch_token_transition_file's token ("" for no
// such file).
func dispatchBroker(transition string) func(*Options) {
	return func(o *Options) {
		o.AgentSecrets = &AgentSecrets{URL: keptBrokerURL, Audience: "agent-secrets", TokenExpiry: time.Hour, ExchangeAudience: testExchangeAudience}
		o.DispatchURL, o.Dispatch = testDispatchURL, DispatchCredential{Broker: true, Transition: transition}
	}
}

// wantNoDispatchToken fails the test unless start, what's start command, carries no Dispatch token
// and tells its role DISPATCH_TOKEN_BROKER=1, and no word of it holds the transition token.
func wantNoDispatchToken(t *testing.T, what string, start shimwire.LauncherStart) {
	t.Helper()
	env := startEnv(start)
	if _, carried := start.Files[dispatchTokenKey]; carried {
		t.Errorf("%s carries %s among its files %v, want no Dispatch token under the broker", what, dispatchTokenKey, slices.Sorted(maps.Keys(start.Files)))
	}
	if pointer, told := env["DISPATCH_TOKEN_FILE"]; told {
		t.Errorf("%s tells its role DISPATCH_TOKEN_FILE=%s, want none under the broker", what, pointer)
	}
	if got := env[shimwire.DispatchTokenBrokerVariable]; got != "1" {
		t.Errorf("%s tells its role %s=%q, want 1", what, shimwire.DispatchTokenBrokerVariable, got)
	}
	for _, word := range slices.Concat(start.Argv, start.Env, slices.Collect(maps.Values(start.Files))) {
		if strings.Contains(word, testTransitionToken) {
			t.Errorf("%s carries the transition token, in %q", what, word)
		}
	}
}

// brokerLaunch is what the broker golden records of one launch: the environment its role's
// process starts with — its pod's container's, then its start command's (workerOf) — and the names
// of the files its start command carries, never their values, which are credentials and a prompt.
type brokerLaunch struct {
	Env   map[string]string `json:"env"`
	Files []string          `json:"files"`
}

// A fresh issue pod's launch under the setting carries no Dispatch token: its start command's
// Files hold no DISPATCH_TOKEN, its role is told no DISPATCH_TOKEN_FILE, and it is told
// DISPATCH_TOKEN_BROKER=1. The golden pins that launch's whole environment and the names of its
// Files, read as the launch baseline reads them (TestLaunchBaselineOfADeploymentWithNoExperiment),
// since none of it rides the Sandbox manifest: every DISPATCH_ variable travels in the start
// command. With the setting off every other golden is the one it was (TestManifestGoldens).
func TestNoDispatchBearerIsHandedOutWithTheBrokerKey(t *testing.T) {
	t.Parallel()
	opts := goldenOptions()
	dispatchBroker("")(&opts)
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	start := launcherCommand(launchOf(t, r, spec), r)
	wantNoDispatchToken(t, "a fresh issue pod's launch", start)
	env := envOf(workerOf(t, r, spec))
	if pointer, told := env["DISPATCH_TOKEN_FILE"]; told || env[shimwire.DispatchTokenBrokerVariable] != "1" {
		t.Fatalf("the role's process is told DISPATCH_TOKEN_FILE=%q (set: %t) and %s=%q, want no pointer and 1",
			pointer, told, shimwire.DispatchTokenBrokerVariable, env[shimwire.DispatchTokenBrokerVariable])
	}
	checkGolden(t, "pod-with-dispatch-broker", brokerLaunch{Env: env, Files: slices.Sorted(maps.Keys(start.Files))})
}

// A launch that keeps a pod built before the setting, which carries no exchange identity, carries
// the transition token as every launch carried the shared bearer before: as DISPATCH_TOKEN in its
// start command's Files, which its role reads through DISPATCH_TOKEN_FILE, and with no
// DISPATCH_TOKEN_BROKER, since its role has no identity to trade until the pod's update round
// replaces the pod.
func TestAKeptPodWithoutTheExchangeIdentityGetsTheTransitionToken(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil, withOptions(dispatchBearer), withWaitsToTheDeadline())
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, dispatchBroker(testTransitionToken))
	next := resumeTester(t, g, r2, loc)
	if next.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the pod; this test is about the pod a launch keeps")
	}
	if found := exchangeTokenVolumes(g.pod(loc.Sandbox.Name).Spec, testExchangeAudience); len(found) > 0 {
		t.Fatalf("the kept pod projects %+v, so it does carry the identity and the case proves nothing", found)
	}
	start := lastStart(t, g, workerToken)
	env := startEnv(start)
	token, carried := start.Files[dispatchTokenKey]
	if !carried && env[shimwire.DispatchTokenBrokerVariable] == "" {
		t.Fatalf("the launch of %s at generation %d into pod %s, which carries no exchange identity, carries no Dispatch credential at all: no %s among its files %v and no %s",
			workerToken, start.Generation, loc.Sandbox.Name, dispatchTokenKey, slices.Sorted(maps.Keys(start.Files)), shimwire.DispatchTokenBrokerVariable)
	}
	if token != testTransitionToken {
		t.Errorf("the launch into the kept pod carries %s %q, want the transition token", dispatchTokenKey, token)
	}
	if want := generationDir(start.Generation) + "/" + dispatchTokenKey; env["DISPATCH_TOKEN_FILE"] != want {
		t.Errorf("the launch tells its role DISPATCH_TOKEN_FILE=%q, want the generation's private %s", env["DISPATCH_TOKEN_FILE"], want)
	}
	if value, told := env[shimwire.DispatchTokenBrokerVariable]; told {
		t.Errorf("the launch tells its role %s=%q, whose pod carries no identity to trade", shimwire.DispatchTokenBrokerVariable, value)
	}
}

// A launch into a pod that carries an exchange identity never carries the transition token, with
// dispatch_token_transition_file named: neither a launch that builds its pod, which carries the
// identity whenever the deployment names an exchange audience, nor one that keeps a pod built with
// it.
func TestAPodWithTheExchangeIdentityNeverGetsTheTransitionToken(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil, withOptions(dispatchBroker(testTransitionToken)), withWaitsToTheDeadline())
	loc := g.spawn(workerSpec(t))
	if found := exchangeTokenVolumes(g.pod(loc.Sandbox.Name).Spec, testExchangeAudience); len(found) != 1 {
		t.Fatalf("the fresh pod projects %+v for the exchange, want its one token", found)
	}
	wantNoDispatchToken(t, "the launch that built its pod", lastStart(t, g, workerToken))

	r2 := restartedOver(t, g, loc, dispatchBroker(testTransitionToken))
	next := resumeTester(t, g, r2, loc)
	if next.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the pod; this case is about the pod a launch keeps")
	}
	wantNoDispatchToken(t, "the launch that kept its pod", lastStart(t, g, workerToken))
}

// A launch that would keep a pod with no exchange identity, under the setting with no transition
// file named, is refused before anything of the pod is touched: no write reaches the cluster and
// no start reaches the pod's launcher, and the refusal is runtime.ErrAwaitingPodRound, naming the
// pod and the round that replaces it, which the supervisor waits out charging nothing.
//
// The refusal names the pod by a locator too (runtime.AwaitingPodRound): the one a process of the
// refused claim in that pod would have, at the launch's own generation, which this runtime's
// PodDrift takes as it takes a running process's and answers the identity the pod lacks. That is
// what the supervisor keeps of a claim that records no process, and what the update round opens
// over and replaces the pod through where no claim records one.
func TestAKeptPodWithNoTransitionFileWaitsForItsRound(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil, withOptions(dispatchBearer), withWaitsToTheDeadline())
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, dispatchBroker(""))
	starts := func() int {
		n := 0
		for _, frame := range g.sent(workerToken) {
			if _, ok := frame.(shimwire.LauncherStart); ok {
				n++
			}
		}
		return n
	}
	before := starts()
	g.clearActions()
	spec := workerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	_, err := r2.Resume(g.ctx, &loc, spec)
	if !errors.Is(err, runtime.ErrAwaitingPodRound) {
		t.Fatalf("Resume = %v, want runtime.ErrAwaitingPodRound", err)
	}
	for _, want := range []string{"pod " + loc.Sandbox.Name + " carries no exchange identity", "dispatch_token_transition_file is not set", "its update round replaces it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if after := starts(); after != before {
		t.Errorf("the pod's launcher was sent %d start commands by the refused launch, want none", after-before)
	}
	if writes := g.writes(); len(writes) > 0 {
		t.Errorf("the refused launch wrote %+v, want nothing of the pod touched", writes)
	}
	if pod := g.pod(loc.Sandbox.Name); pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
		t.Errorf("the pod after the refusal is %v, want %s still running", pod, loc.Sandbox.PodUID)
	}
	var refusal *runtime.AwaitingPodRound
	if !errors.As(err, &refusal) {
		t.Fatalf("the refusal %v names no pod by a locator, want a runtime.AwaitingPodRound", err)
	}
	want := runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: workerToken, Incarnation: runtime.SandboxIncarnation(loc.Sandbox.PodUID, spec.Generation),
		Sandbox: &runtime.SandboxLocator{
			Namespace: loc.Sandbox.Namespace, Name: loc.Sandbox.Name, PodUID: loc.Sandbox.PodUID,
			Container: loc.Sandbox.Container, Generation: spec.Generation,
		},
	}
	if !reflect.DeepEqual(refusal.Pod, want) {
		t.Fatalf("the refusal names the pod as %+v (%+v), want %+v (%+v): the locator of the claim's process in the kept pod at this launch's generation",
			refusal.Pod, refusal.Pod.Sandbox, want, want.Sandbox)
	}
	if err := r2.checkLocator(refusal.Pod); err != nil {
		t.Fatalf("the runtime refuses the locator its own refusal named: %v", err)
	}
	identity := (movedAddress{agentSecretsExchangeVolume, "(unset)", enrollmentName(exchangeTokenProjection(r2.agentSecrets))}).String()
	if drifted := driftOf(t, g, r2, refusal.Pod); !slices.Contains(drifted, identity) {
		t.Errorf("PodDrift through the refusal's locator = %q, want %q among it: the identity the pod lacks is the drift its round is opened for",
			drifted, identity)
	}
}

// The round replaces the pod through the locator the refusal named, as it replaces a pod through
// the locator of a process there, and the claim's next launch builds the pod in its place, which
// carries the exchange identity: that launch carries no Dispatch token at all, and tells its role
// DISPATCH_TOKEN_BROKER=1 (dispatch://INFRA-4425 R4).
func TestTheRoundReplacesThePodItsRefusalNamesAndTheNextLaunchTradesTheIdentity(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil, withOptions(dispatchBearer), withWaitsToTheDeadline())
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, dispatchBroker(""))
	spec := workerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	_, err := r2.Resume(g.ctx, &loc, spec)
	var refusal *runtime.AwaitingPodRound
	if !errors.As(err, &refusal) {
		t.Fatalf("Resume = %v, want a runtime.AwaitingPodRound naming the kept pod", err)
	}

	if err := r2.ReplacePod(g.ctx, runtime.Known{Claim: workerToken, Locator: &refusal.Pod}, stillDue); err != nil {
		t.Fatalf("ReplacePod through the refusal's locator: %v", err)
	}
	spec.Generation, spec.BootToken = 3, "boot-tester-g3"
	next, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("the launch after the round: %v", err)
	}
	if next.Sandbox.PodUID == loc.Sandbox.PodUID {
		t.Fatalf("the launch after the round kept pod %s, which the round replaced", loc.Sandbox.PodUID)
	}
	if found := exchangeTokenVolumes(g.pod(loc.Sandbox.Name).Spec, testExchangeAudience); len(found) != 1 {
		t.Fatalf("the pod in the replaced one's place projects %+v for the exchange, want its one token", found)
	}
	start := lastStart(t, g, workerToken)
	if start.Generation != 3 {
		t.Fatalf("the last start is of generation %d, want the launch after the round's 3", start.Generation)
	}
	wantNoDispatchToken(t, "the launch after the round", start)
}

// The controller's launch takes the same path as an issue pod's, and its pod is never one a launch
// keeps without the exchange identity: no round covers it, so a launch replaces it for that drift
// (movedInPod). So under the setting the controller's launch carries no Dispatch token, whether it
// builds the pod or finds one built before the setting, with no transition file named.
func TestTheControllerLaunchCarriesNoDispatchToken(t *testing.T) {
	t.Parallel()
	t.Run("a pod the launch builds", func(t *testing.T) {
		g := newRig(t, nil, withOptions(dispatchBroker(testTransitionToken)), withWaitsToTheDeadline())
		g.spawn(controllerSpec(t))
		wantNoDispatchToken(t, "the controller's launch", lastStart(t, g, controllerToken))
	})
	t.Run("a pod built before the setting", func(t *testing.T) {
		g := newRig(t, nil, withOptions(dispatchBearer), withWaitsToTheDeadline())
		spec := controllerSpec(t)
		loc := g.spawn(spec)
		r2 := secondRuntime(t, g, dispatchBroker(""))()
		g.connectRunning(controllerToken, loc.Sandbox.Generation)
		g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
			state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
			return connected && state.Child != nil
		})
		spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-controller-g2", controllerSession
		next, err := r2.Resume(g.ctx, &loc, spec)
		if err != nil {
			t.Fatalf("Resume the controller: %v; no round covers its pod, so its launch replaces a pod with no exchange identity rather than waiting for one", err)
		}
		if next.Sandbox.PodUID == loc.Sandbox.PodUID {
			t.Fatalf("the controller's launch kept pod %s, which carries no exchange identity", loc.Sandbox.PodUID)
		}
		wantNoDispatchToken(t, "the controller's launch into its replaced pod", lastStart(t, g, controllerToken))
	})
}
