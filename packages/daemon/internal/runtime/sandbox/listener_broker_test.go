package sandbox

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

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

// TestNoLaunchSecretIsAnEnvoyBearer: the runtime refuses an ENVOY_TOKEN launch secret, which no
// daemon hands out since the listener takes no shared bearer. The control is the same options with
// no such secret, which configure.
func TestNoLaunchSecretIsAnEnvoyBearer(t *testing.T) {
	for name, tc := range map[string]struct {
		move func(*Options)
		want string
	}{
		"an Envoy bearer": {func(o *Options) {
			o.LaunchSecrets = []string{envoyTokenKey}
		}, "LaunchSecrets names ENVOY_TOKEN: the Envoy listener takes no shared bearer"},
		"control: no bearer": {func(*Options) {}, ""},
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
// the exchange identity would start with no identity it can trade, and so could not reach the
// listener at all until the pod's update round replaced the pod. listenerCredential refuses that
// launch before anything of the pod is touched, with runtime.ErrAwaitingPodRound naming the pod,
// which the supervisor waits out charging nothing, as a Dispatch launch under
// dispatch_token_broker with no transition token is. The control is the same kept pod with the
// identity, whose launch starts.
func TestAKeptPodWithNoEnvoyCredentialWaitsForItsRound(t *testing.T) {
	t.Run("no Envoy bearer", func(t *testing.T) {
		g, loc := beforeTheIdentity(t)
		r2 := restartedOver(t, g, loc, func(*Options) {})
		before := starts(g)
		g.clearActions()
		_, err := r2.Resume(g.ctx, &loc, noEnvoyBearerSpec(t))
		wantAwaitingRound(t, g, loc, err, before,
			"pod "+loc.Sandbox.Name+" carries no exchange identity", "the Envoy listener, which takes no shared bearer", "its update round replaces it")
	})

	t.Run("control: the same pod with the exchange identity", func(t *testing.T) {
		g := newRig(t, nil, withOptions(dispatchBearer))
		loc := g.spawn(workerSpec(t))
		r2 := restartedOver(t, g, loc, func(*Options) {})
		next := resumeTester(t, g, r2, loc)
		if next.Sandbox.PodUID != loc.Sandbox.PodUID {
			t.Fatalf("Resume replaced the pod; this case is about the pod a launch keeps")
		}
		if start := startSent(t, g, 2); start.Files[bootTokenKey] != "boot-tester-g2" {
			t.Fatalf("the launch into the kept pod carries files %v, want its own boot token", slices.Sorted(maps.Keys(start.Files)))
		}
	})
}

// TestAPodWithTheExchangeIdentityStartsWithNoEnvoyBearer: under a daemon that holds no Envoy
// bearer, a launch that builds its pod, and one that keeps a pod built with the exchange identity,
// each start with no ENVOY_TOKEN among the start command's files and no ENVOY_TOKEN_FILE told to
// the role, which trades the pod's identity for its own broker token instead.
func TestAPodWithTheExchangeIdentityStartsWithNoEnvoyBearer(t *testing.T) {
	g := newRig(t, nil)
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

	r2 := restartedOver(t, g, loc, func(*Options) {})
	next, err := r2.Resume(g.ctx, &loc, noEnvoyBearerSpec(t))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if next.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the pod; this case is about the pod a launch keeps")
	}
	wantNoEnvoyBearer("the launch that kept its pod", startSent(t, g, 2))
}
