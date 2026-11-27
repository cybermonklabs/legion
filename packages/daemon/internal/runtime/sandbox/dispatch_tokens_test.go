package sandbox

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The two counts a deployment under dispatch_token_broker reads before it removes
// dispatch_token_transition_file (dispatch://INFRA-4425 R4): the launches since the daemon's boot
// that carried the transition token, with the time of the last, and the live launches whose start
// carried a Dispatch token, whichever daemon started them (Runtime.DispatchTokens).

// beforeTheBroker is a tester and an implementer of two issues, each in a pod of its own that a
// daemon before dispatch_token_broker built (dispatchBearer), so neither pod carries an exchange
// identity. Its launches, and those of every daemon restartedUnder restarts over it, must not give
// up, so their waits are bounded by the test binary's deadline (withWaitsToTheDeadline).
func beforeTheBroker(t *testing.T) (*rig, runtime.Locator, runtime.Locator) {
	t.Helper()
	g := newRig(t, nil, withOptions(dispatchBearer))
	return g, g.spawn(workerSpec(t)), g.spawn(childSpec(t))
}

// restartedUnder is a daemon restarted over g's running pods under move, inheriting g's
// BootTimeout through secondRuntime, with the launcher of each of locs connected to it and
// reporting the generation loc records, as a real launcher does once it has redialled the restarted
// daemon.
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

// resumedInto is loc's claim resumed at generation 2 by r, on the session its running generation
// recorded, into the pod it runs in: the launch fails the test if it replaces that pod.
func resumedInto(t *testing.T, g *rig, r *Runtime, loc runtime.Locator) runtime.Locator {
	t.Helper()
	spec := workerSpec(t)
	if loc.Claim == childToken {
		spec = childSpec(t)
	}
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-"+string(loc.Claim)+"-g2", resumeSession
	next, err := r.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume %s: %v", loc.Claim, err)
	}
	if next.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume %s replaced its pod %s; this test is about the pod a launch keeps", loc.Claim, loc.Sandbox.PodUID)
	}
	return next
}

// adoptedAndObserved adopts locs into r as a daemon's boot does (ReconcileOrphans), starts its
// Observe, and returns the channel once it has delivered one Alive observation of each claim. An
// informer may requeue a claim before another's first observation, so firstOfEach skips same-claim
// repeats rather than treating a count of observations as a count of adopted claims.
func adoptedAndObserved(t *testing.T, g *rig, r *Runtime, locs ...runtime.Locator) <-chan runtime.Observation {
	t.Helper()
	var known []runtime.Known
	claims := make([]claim.Token, 0, len(locs))
	for _, loc := range locs {
		known = append(known, runtime.Known{Claim: loc.Claim, Locator: &loc})
		claims = append(claims, loc.Claim)
	}
	if err := r.ReconcileOrphans(g.ctx, known, time.Hour); err != nil {
		t.Fatal(err)
	}
	observations, err := r.Observe(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, obs := range firstOfEach(t, observations, claims...) {
		if obs.Kind != runtime.Alive {
			t.Fatalf("%s of %s: %s, want Alive", obs.Kind, obs.Locator.Claim, obs.Detail)
		}
	}
	return observations
}

// replacedByItsRound ends loc's pod as its update round does (ReplacePod) and waits for the Gone
// that pod's end is observed as, which lets loc's process go from the watch. Observe delivers that
// Gone from a goroutine of its own once the pod store has lost the pod, and blocks until it is read,
// so it is never dropped; the wait is bounded by the test binary's deadline (testwait.Context), as
// next's is, never by a span a loaded machine can put the observer off past.
func replacedByItsRound(t *testing.T, g *rig, r *Runtime, observations <-chan runtime.Observation, loc runtime.Locator) {
	t.Helper()
	if err := r.ReplacePod(g.ctx, runtime.Known{Claim: loc.Claim, Locator: &loc}, stillDue); err != nil {
		t.Fatalf("ReplacePod %s: %v", loc.Claim, err)
	}
	ended := testwait.Context(t)
	for {
		select {
		case obs, ok := <-observations:
			if !ok {
				t.Fatalf("the observation channel closed before the Gone of %s", loc.Claim)
			}
			if obs.Locator.Claim == loc.Claim && obs.Kind == runtime.Gone {
				return
			}
		case <-ended.Done():
			t.Fatalf("no Gone of %s before the test binary's deadline after its pod's replacement", loc.Claim)
		}
	}
}

// liveLaunches waits for r's count of live launches carrying a Dispatch token to reach want: the
// observer lets a process go (forgetIf) once it has delivered the process's Gone, so the count can
// fall a moment after replacedByItsRound returns. The wait is bounded by the test binary's deadline
// (testwait.Until), and its failure prints the count it last read.
func liveLaunches(t *testing.T, r *Runtime, want int, when string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("%s, the live launches carrying a Dispatch token to number %d", when, want), func() (int, bool) {
		live := r.DispatchTokens().LiveLaunches
		return live, live == want
	})
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

// Two launches into kept pods built before the setting carry the transition token: each raises the
// first count, the time of the last is when its start was sent, and both generations are live
// carrying a token, which the transition line names beside the claim and the pod. A daemon
// restarted over those pods counts both again from the record each start wrote, while the pods
// still run, and none of its own boot's transition launches; replacing one pod by its update round
// lets its process go, and the count falls to 1, then, with the other, to 0.
func TestTheDispatchTokenCountsFollowTheLaunches(t *testing.T) {
	t.Parallel()
	g, tester, implementer := beforeTheBroker(t)
	logs := &lockedLog{}
	r2 := restartedUnder(t, g, func(o *Options) {
		dispatchBroker(testTransitionToken)(o)
		o.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}, tester, implementer)
	if got := r2.DispatchTokens(); got != (DispatchTokens{}) {
		t.Fatalf("a daemon that launched nothing yet counts %+v, want nothing", got)
	}

	keptTester := resumedInto(t, g, r2, tester)
	keptImplementer := resumedInto(t, g, r2, implementer)
	sent := *g.now.Load()
	got := r2.DispatchTokens()
	if got.TransitionLaunches != 2 || !got.LastTransitionLaunch.Equal(sent) || got.LiveLaunches != 2 {
		t.Fatalf("after two launches into kept pods with no exchange identity the counts are %+v, want 2 transition launches, the last at %s, and 2 live launches carrying a Dispatch token",
			got, sent.Format(time.RFC3339Nano))
	}
	lines := logged(t, logs, "sandbox: a launch into a pod with no exchange identity carries the transition Dispatch token")
	if len(lines) != 2 {
		t.Fatalf("the runtime logged %d transition launches, want 2: %s", len(lines), logs.String())
	}
	if last := lines[1]; last["claim"] != string(childToken) || last["pod"] != implementer.Sandbox.Name ||
		last["transitionLaunches"] != float64(2) || last["liveLaunchesWithDispatchToken"] != float64(2) {
		t.Errorf("the second transition line is %v, want the implementer's claim and pod with 2 transition launches and 2 live", last)
	}
	var counts []any
	for _, line := range logged(t, logs, "sandbox: live launches carrying a Dispatch token") {
		counts = append(counts, line["count"])
	}
	if len(counts) != 2 || counts[0] != float64(1) || counts[1] != float64(2) {
		t.Errorf("the runtime logged the live count as %v, want it at each change: 1, then 2", counts)
	}

	r3 := restartedUnder(t, g, dispatchBroker(testTransitionToken), keptTester, keptImplementer)
	observations := adoptedAndObserved(t, g, r3, keptTester, keptImplementer)
	got = r3.DispatchTokens()
	if got.LiveLaunches != 2 {
		t.Fatalf("after a restart the live launches carrying a Dispatch token number %d while both kept pods still run, want 2: the record each start wrote says it carried one", got.LiveLaunches)
	}
	if got.TransitionLaunches != 0 || !got.LastTransitionLaunch.IsZero() {
		t.Errorf("a restarted daemon that launched nothing counts %d transition launches, the last at %s, want none of its own boot",
			got.TransitionLaunches, got.LastTransitionLaunch)
	}

	replacedByItsRound(t, g, r3, observations, keptTester)
	liveLaunches(t, r3, 1, "with the tester's pod replaced by its round")
	replacedByItsRound(t, g, r3, observations, keptImplementer)
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
	t.Parallel()
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
