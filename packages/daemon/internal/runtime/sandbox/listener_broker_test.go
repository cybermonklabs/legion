package sandbox

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// listenerBroker is a deployment whose daemon holds no Envoy bearer (Options.ListenerBroker):
// envoy_token_file is left out beside the exchange audience, so every role in a pod built now trades
// that pod's exchange identity for a broker token of its own, and no launch carries ENVOY_TOKEN.
func listenerBroker(o *Options) {
	o.AgentSecrets = &AgentSecrets{URL: keptBrokerURL, Audience: "agent-secrets", TokenExpiry: time.Hour, ExchangeAudience: testExchangeAudience}
	o.ListenerBroker = true
}

// noEnvoyBearerSpec is the tester's launch at generation 2 on the session its running generation
// recorded, as resumeTester makes it, from a daemon that holds no Envoy bearer: its secrets carry no
// ENVOY_TOKEN.
func noEnvoyBearerSpec(t *testing.T) runtime.SpawnSpec {
	t.Helper()
	spec := workerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	spec.Secrets = maps.Clone(spec.Secrets)
	delete(spec.Secrets, "ENVOY_TOKEN")
	return spec
}

// starts is how many start commands the tester's launcher has been sent.
func starts(g *rig) int {
	n := 0
	for _, frame := range g.sent(workerToken) {
		if _, ok := frame.(shimwire.LauncherStart); ok {
			n++
		}
	}
	return n
}

// TestListenerBrokerNeedsTheExchangeIdentityAndNoBearer: the runtime refuses ListenerBroker beside
// no exchange audience, where no role would have any credential for the listener, and beside an
// ENVOY_TOKEN launch secret, which a daemon that holds no bearer never hands out. The control is
// ListenerBroker with the exchange audience and no bearer, which configures.
func TestListenerBrokerNeedsTheExchangeIdentityAndNoBearer(t *testing.T) {
	for name, tc := range map[string]struct {
		move func(*Options)
		want string
	}{
		"no exchange audience": {func(o *Options) {
			o.AgentSecrets = &AgentSecrets{URL: keptBrokerURL, Audience: "agent-secrets", TokenExpiry: time.Hour}
			o.ListenerBroker = true
		}, "ListenerBroker is set but AgentSecrets names no exchange audience"},
		"an Envoy bearer among the launch secrets": {func(o *Options) {
			listenerBroker(o)
			o.LaunchSecrets = []string{envoyTokenKey}
		}, "ListenerBroker is set but LaunchSecrets names ENVOY_TOKEN"},
		"control: the exchange audience and no bearer": {listenerBroker, ""},
	} {
		t.Run(name, func(t *testing.T) {
			opts := goldenOptions()
			tc.move(&opts)
			_, err := configure(opts)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("configure = %v, want it configured", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("configure = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// TestAKeptPodWithNoEnvoyCredentialWaitsForItsRound: a role relaunched into a pod built before
// the exchange identity, by a daemon that holds no Envoy bearer, would start with neither a bearer
// nor an identity it can trade, and so could not reach the listener at all until the pod's update
// round replaced the pod. That launch is refused before anything of the pod is touched, with
// runtime.ErrAwaitingPodRound naming the pod, which the supervisor waits out charging nothing, as a
// Dispatch launch under dispatch_token_broker with no transition token is. The control is the
// same kept pod relaunched by a daemon that still holds the bearer: it starts, as before.
func TestAKeptPodWithNoEnvoyCredentialWaitsForItsRound(t *testing.T) {
	t.Run("no Envoy bearer", func(t *testing.T) {
		g := newRig(t, nil, withOptions(dispatchBearer))
		loc := g.spawn(workerSpec(t))
		r2 := restartedOver(t, g, loc, listenerBroker)
		before := starts(g)
		g.clearActions()
		_, err := r2.Resume(g.ctx, &loc, noEnvoyBearerSpec(t))
		if !errors.Is(err, runtime.ErrAwaitingPodRound) {
			t.Fatalf("Resume = %v, want runtime.ErrAwaitingPodRound", err)
		}
		for _, want := range []string{"pod " + loc.Sandbox.Name + " carries no exchange identity", "envoy_token_file is not set", "its update round replaces it"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not say %q", err, want)
			}
		}
		var refusal *runtime.AwaitingPodRound
		if !errors.As(err, &refusal) || refusal.Pod.Sandbox == nil || refusal.Pod.Sandbox.PodUID != loc.Sandbox.PodUID {
			t.Fatalf("the refusal %v names no locator of the kept pod %s, want a runtime.AwaitingPodRound naming it", err, loc.Sandbox.PodUID)
		}
		if after := starts(g); after != before {
			t.Errorf("the pod's launcher was sent %d start commands by the refused launch, want none", after-before)
		}
		if writes := g.writes(); len(writes) > 0 {
			t.Errorf("the refused launch wrote %+v, want nothing of the pod touched", writes)
		}
	})

	t.Run("control: the daemon holds the bearer", func(t *testing.T) {
		g := newRig(t, nil, withOptions(dispatchBearer))
		loc := g.spawn(workerSpec(t))
		r2 := restartedOver(t, g, loc, dispatchBearer)
		next := resumeTester(t, g, r2, loc)
		if next.Sandbox.PodUID != loc.Sandbox.PodUID {
			t.Fatalf("Resume replaced the pod; this case is about the pod a launch keeps")
		}
		if start := startSent(t, g, 2); start.Files["ENVOY_TOKEN"] != "envoy-bearer" {
			t.Fatalf("the launch into the kept pod carries files %v, want the bearer as ENVOY_TOKEN", slices.Sorted(maps.Keys(start.Files)))
		}
	})
}

// TestAPodWithTheExchangeIdentityStartsWithNoEnvoyBearer: under a daemon that holds no Envoy
// bearer, a launch that builds its pod, and one that keeps a pod built with the exchange identity,
// each start with no ENVOY_TOKEN among the start command's files and no ENVOY_TOKEN_FILE told to
// the role, which trades the pod's identity for its own broker token instead.
func TestAPodWithTheExchangeIdentityStartsWithNoEnvoyBearer(t *testing.T) {
	g := newRig(t, nil, withOptions(listenerBroker))
	spec := workerSpec(t)
	spec.Secrets = maps.Clone(spec.Secrets)
	delete(spec.Secrets, "ENVOY_TOKEN")
	loc := g.spawn(spec)
	if found := exchangeTokenVolumes(g.pod(loc.Sandbox.Name).Spec, testExchangeAudience); len(found) != 1 {
		t.Fatalf("the fresh pod projects %+v for the exchange, want its one token", found)
	}
	wantNoEnvoyBearer := func(what string, start shimwire.LauncherStart) {
		t.Helper()
		if _, carried := start.Files["ENVOY_TOKEN"]; carried {
			t.Errorf("%s carries ENVOY_TOKEN among its files", what)
		}
		if pointer, told := startEnv(start)["ENVOY_TOKEN_FILE"]; told {
			t.Errorf("%s tells its role ENVOY_TOKEN_FILE=%s, want none", what, pointer)
		}
	}
	wantNoEnvoyBearer("the launch that built its pod", lastStart(t, g, workerToken))

	r2 := restartedOver(t, g, loc, listenerBroker)
	next, err := r2.Resume(g.ctx, &loc, noEnvoyBearerSpec(t))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if next.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the pod; this case is about the pod a launch keeps")
	}
	wantNoEnvoyBearer("the launch that kept its pod", startSent(t, g, 2))
}
