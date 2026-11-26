package fake

// podupdate.go is the fake's part for the daemon's update rounds: the scripting of a round's
// drift, replacements and recorded acts, the calls a round makes of the runtime, from PodDrift
// to ReplaceIssuePod, and the end of an issue's pod that a project release asks (EndIssuePod).

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// FailPodDrift makes every later PodDrift return err; nil clears it.
func (r *Runtime) FailPodDrift(err error) { r.fail("PodDrift", err) }

// FailReplacePod makes every later ReplacePod return err; nil clears it.
func (r *Runtime) FailReplacePod(err error) { r.fail("ReplacePod", err) }

// FailReplacePodAfterItsSuspension makes every later ReplacePod that due lets through land its
// suspension and then return err, as the Sandbox runtime answers a replacement whose wait for the
// pod to go ran out once the suspension was written (replaceRoundPod's awaitPodGone): the call is
// recorded as ReplacePod, and the pod's Sandbox stands suspended over the pod from then on
// (suspendedOver). nil clears it.
func (r *Runtime) FailReplacePodAfterItsSuspension(err error) {
	r.fail("ReplacePodAfterItsSuspension", err)
}

// ScriptSuspendedOver has the issue's Sandbox stand suspended over pod, as a replacement whose
// suspension landed leaves the cluster for a daemon that did not make it, one restarted since
// among them: a ReplacePod or a ReplaceIssuePod of pod replaces nothing and answers nil.
func (r *Runtime) ScriptSuspendedOver(pod string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suspendedOver[pod] = true
}

// ScriptSandboxGone has the issue's Sandbox of uid gone, as the Sandbox runtime finds it once the
// API server confirmed its delete, and the kept volume it owned with it: an act that takes that
// volume (runtime.TakesVolume) is made from then on.
func (r *Runtime) ScriptSandboxGone(uid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sandboxGone[uid] = true
}

// HoldReplacePod makes every later ReplacePod run hold before it asks whether the replacement is
// still due, which is where the Sandbox runtime waits out the pod's launch turn. A test holds
// there to do what that wait lets happen — a worker of the round starting a turn, a claim of the
// issue being launched into the kept pod — and then lets the call go on. nil clears it; the hook
// runs with none of the fake's own locks held, so it may drive the fake again.
func (r *Runtime) HoldReplacePod(hold func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.holdReplace = hold
}

// FindNoPodToReplace makes every later ReplacePod answer nil without asking due and without
// replacing anything, as the Sandbox runtime answers a Sandbox that is absent or suspended, or
// whose pod is no longer the one the locator records: there is no pod of the round to end. The
// call is recorded as ReplacePodNoPod, so no ReplacePod call stands for a replacement. false
// clears it.
func (r *Runtime) FindNoPodToReplace(none bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replaceNoPod = none
}

// FailReplaceIssuePod makes every later ReplaceIssuePod return err once due has let it through,
// as the Sandbox runtime answers a write the cluster refused; nil clears it.
func (r *Runtime) FailReplaceIssuePod(err error) { r.fail("ReplaceIssuePod", err) }

// HoldReplaceIssuePod makes every later ReplaceIssuePod run hold before it asks whether the
// replacement is still due, which is where the Sandbox runtime waits out the pod's launch turn, as
// HoldReplacePod does for ReplacePod. nil clears it; the hook runs with none of the fake's own
// locks held, so it may drive the fake again.
func (r *Runtime) HoldReplaceIssuePod(hold func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.holdIssue = hold
}

// PodDrift records its call and answers what ScriptPodDrift set for the claim, nil for a claim no
// call named and for one with no process, as a runtime answers a claim that runs nowhere, and
// runtime.ErrPodGone for a claim ScriptPodGone named, as a runtime answers a claim whose
// recorded pod is not there.
func (r *Runtime) PodDrift(_ context.Context, k runtime.Known) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "PodDrift", Pod: copyKnown(k)})
	if err := r.failures["PodDrift"]; err != nil {
		return nil, err
	}
	if k.Locator == nil {
		return nil, nil
	}
	if r.gone[k.Claim] {
		return nil, runtime.ErrPodGone
	}
	return append([]string(nil), r.drift[k.Claim]...), nil
}

// IssuePod records its call and answers what ScriptIssuePod set for the issue, followed to the pod
// a launch built in its place where the fake replaced it (podNow), no pod where the last pod so
// followed was replaced with nothing built in its place, and read false for an issue no call
// named.
func (r *Runtime) IssuePod(issue string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "IssuePod", Issue: issue})
	pod, read := r.issuePods[issue]
	if now, live := r.podNow(pod); live {
		return now, read
	}
	return "", read
}

// IssueKeepsVolume answers what ScriptKeptVolume set for the issue: true where its kept Sandbox
// was made before since, cut to the second as the Sandbox runtime cuts it, since the API server
// records a creation time to the second. It records no call: the daemon asks it for the words of a
// message and for the rename pass's live-pod rules (bootKeep, podEndKeep), whose effects a test
// reads instead.
func (r *Runtime) IssueKeepsVolume(issue string, since time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	created, kept := r.keptVolumes[issue]
	return kept && created.Before(since.Truncate(time.Second))
}

// ReplacePod plays the Sandbox runtime's own order: it waits where that runtime waits for the
// pod's launch turn (HoldReplacePod), answers nil there for a pod it finds no longer there
// (FindNoPodToReplace), asks due whether the replacement is still due, and only then replaces
// anything. A due that refuses records a ReplacePodNotDue call and changes nothing — no
// replacement was made, so no ReplacePod call stands for one — and its error is returned as the
// runtime hands it back. Otherwise the call is recorded, the claim's drift forgotten — the place
// it named is gone, and the one built in its stead is built from the configuration the runtime
// has now — and what FailReplacePod set is answered, nil by default. A pod whose Sandbox stands
// suspended over it (suspendedOver) is a pod no longer there to replace, as FindNoPodToReplace's
// is; one that FailReplacePodAfterItsSuspension holds lands its suspension and returns that error.
func (r *Runtime) ReplacePod(ctx context.Context, k runtime.Known, due func(context.Context) error) error {
	r.mu.Lock()
	hold := r.holdReplace
	r.mu.Unlock()
	if hold != nil {
		hold()
	}
	pod := ""
	if k.Locator != nil && k.Locator.Sandbox != nil {
		pod = k.Locator.Sandbox.PodUID
	}
	r.mu.Lock()
	if r.replaceNoPod || r.suspendedOver[pod] {
		r.calls = append(r.calls, Call{Method: "ReplacePodNoPod", Pod: copyKnown(k)})
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	if err := due(ctx); err != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, Call{Method: "ReplacePodNotDue", Pod: copyKnown(k)})
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "ReplacePod", Pod: copyKnown(k)})
	if err := r.failures["ReplacePodAfterItsSuspension"]; err != nil {
		r.suspendedOver[pod] = true
		return err
	}
	if err := r.failures["ReplacePod"]; err != nil {
		return err
	}
	delete(r.drift, k.Claim)
	r.took[pod] = true
	return nil
}

// ReadAct is the Sandbox runtime's read of whether a recorded act on the issue's pod was made
// (runtime.ActReader). It takes the pods' launch turn without waiting (turn), and answers
// runtime.ErrTurnBusy, calling nothing, where a launch or a replacement by issue holds it, as the
// Sandbox runtime answers a turn another holds. Under the turn, an act that takes the pod
// (runtime.TakesPod) is made where its target is taken (podTaken); it is not made otherwise. An act
// that takes the kept volume (runtime.TakesVolume) is made where the Sandbox of its target uid is
// gone (ScriptSandboxGone), not made where the issue's Sandbox runs a live pod of its own that is
// not taken, which uses the volume, and
// uncertain otherwise, as the Sandbox runtime reads a Sandbox of that uid that still stands
// Suspended. It records no call, and decide runs with mu free.
func (r *Runtime) ReadAct(ctx context.Context, issue string, act runtime.RecordedAct, decide func(context.Context, runtime.ActState) error) error {
	if !r.turn.TryLock() {
		return runtime.ErrTurnBusy
	}
	defer r.turn.Unlock()
	state, err := r.actState(issue, act)
	if err != nil {
		return err
	}
	return decide(ctx, state)
}

// actState is ReadAct's reading of act on the issue's pod, which its caller makes under the pods'
// launch turn (turn), with mu free.
func (r *Runtime) actState(issue string, act runtime.RecordedAct) (runtime.ActState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch act.Takes {
	case runtime.TakesPod:
		if r.podTaken(issue, act.Target) {
			return runtime.ActMade, nil
		}
		return runtime.ActNotMade, nil
	case runtime.TakesVolume:
		held, read := r.issuePods[issue]
		now, live := r.podNow(held)
		switch {
		case r.sandboxGone[act.Target]:
			return runtime.ActMade, nil
		case read && live && !r.podTaken(issue, now):
			return runtime.ActNotMade, nil
		}
		return runtime.ActUncertain, nil
	}
	return runtime.ActUncertain, fmt.Errorf("fake runtime: no act takes %q", act.Takes)
}

// EndIssuePod plays the Sandbox runtime's end of an issue's pod as a project release asks it
// (SuspendIssue), under a name of its own, so that the fake is no runtime.IssueSuspender and the
// daemon's close takes no pod of it: under the pods' launch turn (turn) it asks authorize, and a
// false or an error ends it with nothing done; then it runs each of settle with the read of a
// recorded act that runtime hands such a hook (runtime.ActRead), ReadAct's reading with no turn
// taken again, and an error ends it with nothing done; then the issue's Sandbox holds no pod, which
// IssuePod answers from then on, and the call is recorded as EndIssuePod.
func (r *Runtime) EndIssuePod(ctx context.Context, issue string, authorize func(context.Context) (bool, error),
	settle ...func(context.Context, runtime.ActRead) error) error {
	r.turn.Lock()
	defer r.turn.Unlock()
	if act, err := authorize(ctx); err != nil || !act {
		return err
	}
	read := func(_ context.Context, act runtime.RecordedAct) (runtime.ActState, error) {
		return r.actState(issue, act)
	}
	for _, s := range settle {
		if err := s(ctx, read); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "EndIssuePod", Issue: issue})
	r.issuePods[issue] = ""
	return nil
}

// podGone is whether the issue's Sandbox no longer holds pod: the fake replaced it by issue or at a
// launch (replaced), a replacement's suspension landed over it (suspendedOver), or IssuePod answers
// another pod or none for the issue (ScriptIssuePod). An issue no ScriptIssuePod named is taken to
// hold it. Called with mu held.
func (r *Runtime) podGone(issue, pod string) bool {
	_, gone := r.replaced[pod]
	gone = gone || r.suspendedOver[pod]
	if held, read := r.issuePods[issue]; read {
		now, live := r.podNow(held)
		gone = gone || !live || now != pod
	}
	return gone
}

// podTaken is whether pod is taken from the issue's Sandbox: a ReplacePod replaced it (took), or
// the Sandbox no longer holds it (podGone). It is the fake's one test of a taken pod, as the
// Sandbox runtime has one (podTaken). Called with mu held.
func (r *Runtime) podTaken(issue, pod string) bool {
	return r.took[pod] || r.podGone(issue, pod)
}

// ReplaceIssuePod plays the Sandbox runtime's own order, as ReplacePod does: it waits where that
// runtime waits for the pod's launch turn (HoldReplaceIssuePod), then takes that turn (turn),
// answers nil for a pod the issue's Sandbox does not hold, asks due whether the replacement is
// still due, and only then replaces anything. The issue's Sandbox does not hold podUID where the
// fake already replaced podUID, or where IssuePod answers another pod or none for the issue, and
// an issue no ScriptIssuePod named is taken to hold it: such a call is recorded as
// ReplaceIssuePodNoPod and asks nothing. A due that refuses records a ReplaceIssuePodNotDue call
// and changes nothing, and its error is returned as the runtime hands it back. Otherwise the call
// is recorded and what FailReplaceIssuePod set is answered. A replacement that is not refused is
// remembered (replaced): IssuePod answers no pod for it from then on, and a launch scripted into
// it comes up in a new pod (launchInto).
func (r *Runtime) ReplaceIssuePod(ctx context.Context, issue, podUID string, due func(context.Context) error) error {
	r.mu.Lock()
	hold := r.holdIssue
	r.mu.Unlock()
	if hold != nil {
		hold()
	}
	r.turn.Lock()
	defer r.turn.Unlock()
	call := Call{Issue: issue, PodUID: podUID}
	r.mu.Lock()
	if r.podGone(issue, podUID) {
		call.Method = "ReplaceIssuePodNoPod"
		r.calls = append(r.calls, call)
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	if err := due(ctx); err != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		call.Method = "ReplaceIssuePodNotDue"
		r.calls = append(r.calls, call)
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	call.Method = "ReplaceIssuePod"
	r.calls = append(r.calls, call)
	if err := r.failures["ReplaceIssuePod"]; err != nil {
		return err
	}
	r.replaced[podUID] = ""
	return nil
}
