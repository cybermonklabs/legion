package sandbox

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The two counts a deployment under dispatch_token_broker reads before it removes
// dispatch_token_transition_file (dispatch://INFRA-4425 R4): the launches since the daemon's boot
// that carried the transition token, with the time of the last, and the live launches whose start
// carried a Dispatch token, whichever daemon started them (Runtime.DispatchTokens).

// beforeTheBroker is a tester and an implementer of two issues, each in a pod of its own that a
// daemon before dispatch_token_broker built (dispatchBearer), edited into pods built before the
// exchange identity (builtWithout), so neither pod carries one.
func beforeTheBroker(t *testing.T) (*rig, runtime.Locator, runtime.Locator) {
	t.Helper()
	g := newRig(t, nil, withOptions(dispatchBearer))
	tester, implementer := g.spawn(workerSpec(t)), g.spawn(childSpec(t))
	g.builtWithout(tester.Sandbox.Name, exchangeIdentityVolume)
	g.builtWithout(implementer.Sandbox.Name, exchangeIdentityVolume)
	return g, tester, implementer
}

// restartedUnder is a daemon restarted over g's running pods under move, with the launcher of each
// of locs connected to it and reporting the generation loc records, as a real launcher does once it
// has redialled the restarted daemon.
func restartedUnder(t *testing.T, g *rig, move func(*Options), locs ...runtime.Locator) *Runtime {
	t.Helper()
	r := secondRuntime(t, g, move)()
	for _, loc := range locs {
		g.connectRunning(loc.Claim, loc.Sandbox.Generation)
	}
	g.eventually("every launcher to report its child to the restarted daemon", func() bool {
		for _, loc := range locs {
			state, connected := r.launchers.state(loc.Claim, loc.Sandbox.PodUID)
			if !connected || state.Child == nil || state.Child.Generation != loc.Sandbox.Generation {
				return false
			}
		}
		return true
	})
	return r
}

// resumeOf is the launch of loc's claim at generation 2, on the session its running generation
// recorded, into the pod it runs in.
func resumeOf(t *testing.T, loc runtime.Locator) runtime.SpawnSpec {
	t.Helper()
	spec := workerSpec(t)
	if loc.Claim == childToken {
		spec = childSpec(t)
	}
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-"+string(loc.Claim)+"-g2", resumeSession
	return spec
}

// adoptedAndObserved adopts locs into r as a daemon's boot does (ReconcileOrphans), starts its
// Observe, and returns the channel once it has delivered one observation of each, failing the test
// unless each is Alive.
func adoptedAndObserved(t *testing.T, g *rig, r *Runtime, locs ...runtime.Locator) <-chan runtime.Observation {
	t.Helper()
	var known []runtime.Known
	for _, loc := range locs {
		known = append(known, runtime.Known{Claim: loc.Claim, Locator: &loc})
	}
	if err := r.ReconcileOrphans(g.ctx, known, time.Hour); err != nil {
		t.Fatal(err)
	}
	observations, err := r.Observe(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range locs {
		if obs := next(t, observations); obs.Kind != runtime.Alive {
			t.Fatalf("%s of %s: %s, want Alive", obs.Kind, obs.Locator.Claim, obs.Detail)
		}
	}
	return observations
}

// replacedByItsRound ends loc's pod as its update round does (ReplacePod) and waits for the Gone
// that pod's end is observed as, which lets loc's process go from the watch.
func replacedByItsRound(t *testing.T, g *rig, r *Runtime, observations <-chan runtime.Observation, loc runtime.Locator) {
	t.Helper()
	if err := r.ReplacePod(g.ctx, runtime.Known{Claim: loc.Claim, Locator: &loc}, stillDue); err != nil {
		t.Fatalf("ReplacePod %s: %v", loc.Claim, err)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case obs := <-observations:
			if obs.Locator.Claim == loc.Claim && obs.Kind == runtime.Gone {
				return
			}
		case <-deadline:
			t.Fatalf("no Gone of %s within a second of its pod's replacement", loc.Claim)
		}
	}
}

// liveLaunches waits a second for r's count of live launches carrying a Dispatch token to reach
// want, failing the test with the count it last read.
func liveLaunches(t *testing.T, r *Runtime, want int, when string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for live := r.DispatchTokens().LiveLaunches; live != want; live = r.DispatchTokens().LiveLaunches {
		if time.Now().After(deadline) {
			t.Fatalf("%s the live launches carrying a Dispatch token number %d, want %d", when, live, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// logged is every record of logs written with msg, each decoded whole.
func logged(t *testing.T, logs *lockedLog, msg string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the runtime logged %q, which is not one JSON record: %v", line, err)
		}
		if record["msg"] == msg {
			records = append(records, record)
		}
	}
	return records
}

// Two launches into kept pods built before the exchange identity, under
// dispatch_token_transition_file, wait for their pods' update rounds: dispatchCredential would hand
// each the transition token, but listenerCredential refuses each after it, since the Envoy
// listener takes no shared bearer and a role of a pod with no identity to trade would hold no
// credential for it. A refused launch sends no start, so neither raises the transition count, the
// live count stays 0 on the daemon that refused them, and neither the transition line nor a live
// count line is logged. A daemon restarted over those pods counts the generations an earlier daemon
// started with the shared bearer from the record each start wrote, while the pods still run, and
// no transition launch of its own boot; replacing one pod by its update round lets its process go,
// and the count falls to 1, then, with the other, to 0.
func TestTheDispatchTokenCountsFollowTheLaunches(t *testing.T) {
	g, tester, implementer := beforeTheBroker(t)
	logs := &lockedLog{}
	r2 := restartedUnder(t, g, func(o *Options) {
		dispatchBroker(testTransitionToken)(o)
		o.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}, tester, implementer)
	if got := r2.DispatchTokens(); got != (DispatchTokens{}) {
		t.Fatalf("a daemon that launched nothing yet counts %+v, want nothing", got)
	}

	for _, loc := range []runtime.Locator{tester, implementer} {
		if _, err := r2.Resume(g.ctx, &loc, resumeOf(t, loc)); !errors.Is(err, runtime.ErrAwaitingPodRound) || !strings.Contains(err.Error(), "the Envoy listener, which takes no shared bearer") {
			t.Fatalf("Resume %s = %v, want listenerCredential's runtime.ErrAwaitingPodRound", loc.Claim, err)
		}
	}
	if got := r2.DispatchTokens(); got != (DispatchTokens{}) {
		t.Fatalf("after two refused launches into kept pods with no exchange identity the counts are %+v, want no transition launch and 0 live launches carrying a Dispatch token", got)
	}
	if lines := logged(t, logs, "sandbox: a launch into a pod with no exchange identity carries the transition Dispatch token"); len(lines) != 0 {
		t.Errorf("the runtime logged %d transition launches for two refused launches, want none: %s", len(lines), logs.String())
	}
	if lines := logged(t, logs, "sandbox: live launches carrying a Dispatch token"); len(lines) != 0 {
		t.Errorf("the runtime logged the live count %d times for two refused launches, want never: %s", len(lines), logs.String())
	}

	r3 := restartedUnder(t, g, dispatchBroker(testTransitionToken), tester, implementer)
	observations := adoptedAndObserved(t, g, r3, tester, implementer)
	got := r3.DispatchTokens()
	if got.LiveLaunches != 2 {
		t.Fatalf("after a restart the live launches carrying a Dispatch token number %d while both kept pods still run, want 2: the record each start of the shared bearer wrote says it carried one", got.LiveLaunches)
	}
	if got.TransitionLaunches != 0 || !got.LastTransitionLaunch.IsZero() {
		t.Errorf("a restarted daemon that launched nothing counts %d transition launches, the last at %s, want none of its own boot",
			got.TransitionLaunches, got.LastTransitionLaunch)
	}

	replacedByItsRound(t, g, r3, observations, tester)
	liveLaunches(t, r3, 1, "with the tester's pod replaced by its round")
	replacedByItsRound(t, g, r3, observations, implementer)
	liveLaunches(t, r3, 0, "with both pods replaced by their rounds")
}

// rewriteRecord edits loc's address record on its Sandbox in the cluster, as JSON, into the record
// a daemon that wrote it otherwise would have left: a runtime built after it reads it so.
func rewriteRecord(t *testing.T, g *rig, loc runtime.Locator, edit func(map[string]any)) {
	t.Helper()
	role, _ := loc.Claim.Role()
	s := g.sandbox(loc.Sandbox.Name)
	if s == nil {
		t.Fatalf("sandbox %s is absent", loc.Sandbox.Name)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(s.Annotations[addressesAnnotation(role)]), &record); err != nil {
		t.Fatalf("read %s's address record %q: %v", loc.Claim, s.Annotations[addressesAnnotation(role)], err)
	}
	edit(record)
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	s.Annotations[addressesAnnotation(role)] = string(encoded)
	u, err := encodeSandbox(*s)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.dyn.Tracker().Update(sandboxGVR, u, testNamespace); err != nil {
		t.Fatal(err)
	}
}

// A daemon restarted under the setting counts the live generations an earlier daemon started from
// the record each start wrote: one whose record lacks the field was started by a daemon before it,
// which gave every launch the token while Dispatch was configured, so it counts; one recorded as
// carrying none does not.
func TestARestartCountsGenerationsAnEarlierDaemonStarted(t *testing.T) {
	g, tester, implementer := beforeTheBroker(t)
	rewriteRecord(t, g, tester, func(record map[string]any) { delete(record, "dispatchToken") })
	rewriteRecord(t, g, implementer, func(record map[string]any) { record["dispatchToken"] = false })

	r2 := restartedUnder(t, g, dispatchBroker(testTransitionToken), tester, implementer)
	adoptedAndObserved(t, g, r2, tester, implementer)
	r2.mu.Lock()
	_, testerCounted := r2.dispatchTokenClaims[tester.Claim]
	_, implementerCounted := r2.dispatchTokenClaims[implementer.Claim]
	r2.mu.Unlock()
	if !testerCounted {
		t.Errorf("the tester's generation, whose record a daemon before the field wrote, is not counted as carrying a Dispatch token")
	}
	if implementerCounted {
		t.Errorf("the implementer's generation, recorded as carrying no Dispatch token, is counted as carrying one")
	}
	if got := r2.DispatchTokens(); got.LiveLaunches != 1 || got.TransitionLaunches != 0 {
		t.Errorf("the restarted daemon counts %+v, want 1 live launch carrying a Dispatch token and no transition launch", got)
	}
}
