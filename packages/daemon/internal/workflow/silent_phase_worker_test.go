package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// silentNow is the moment every row below is judged at, and the moment of the reading the rule
// was written from (dispatch://INFRA-3163). A table of values needs a clock of its own: the rule
// reads no store and no wall clock, so the only times in it are the two the caller passes.
var silentNow = time.Date(2026, 11, 12, 8, 18, 0, 0, time.UTC)

// silentRun is everything SilentPhaseWorker reads of one phase worker, held together so a row
// can change exactly one term of the true case and leave the rest as it stands.
type silentRun struct {
	issue    record.Issue
	root     record.Issue
	gate     record.TreeGate
	role     claim.Role
	run      record.RoleRun
	lastStep time.Time
	bound    time.Duration
	now      time.Time
}

// answer is the rule's answer for this run.
func (s silentRun) answer() bool {
	return SilentPhaseWorker(s.issue, s.root, s.gate, s.role, s.run, s.lastStep, s.bound, s.now)
}

// startFor is StartFor's answer for the same run, which the rule's own true case must never
// draw: the two repairs would then start one claim twice.
func (s silentRun) startFor() bool {
	return StartFor(s.run, s.root, s.issue.Generation, s.issue.Phase)
}

// aSilentImplementer is the rule's true case: the implementer of an issue in implementing, of a
// tree that runs, whose process is up and between turns, which holds no task, is in no update
// round and has no queued supervise row of its own, and whose last recorded step is two hours
// old against a bound of thirty minutes.
func aSilentImplementer() silentRun {
	issue := record.Issue{Key: "LEGION-209", Tree: "LEGION-208", Project: "LEGION", Phase: phase.Implementing, Generation: 3}
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Phase: phase.Admitted, Generation: 3}
	return silentRun{
		issue:    issue,
		root:     root,
		role:     claim.RoleImplementer,
		run:      record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateReady, Tree: root.Key}},
		lastStep: silentNow.Add(-2 * time.Hour),
		bound:    30 * time.Minute,
		now:      silentNow,
	}
}

// inState is the run with the claim's state moved, which is the state both this rule and
// StartFor read of it.
func inState(s *silentRun, state supervise.ClaimState) {
	s.run.Claim.State = state
}

// The rule names a phase worker nothing will prompt, and every term of it is load-bearing: the
// true case is a worker up and between turns with no task and a step older than the bound, and
// each row below flips one term of that case and is answered false. The rows are the terms in
// the order SilentPhaseWorker asks them.
func TestASilentPhaseWorkerIsAWorkerUpBetweenTurnsHoldingNoTaskWhoseLastStepPassedTheBound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*silentRun)
		want   bool
	}{
		{
			name: "up, between turns, holding nothing, last seen two hours ago",
			want: true,
		},
		{
			name:   "a bound of zero switches the rule off",
			change: func(s *silentRun) { s.bound = 0 },
		},
		{
			name:   "this daemon holds no claim of the role's run",
			change: func(s *silentRun) { s.run.Claim = nil },
		},
		{
			name: "the issue is held, so no worker of it is at work",
			change: func(s *silentRun) {
				s.issue.Phase = phase.Held
				s.issue.Hold = &record.Hold{From: phase.Implementing}
			},
		},
		{
			name:   "the run is the tester's, not the one the phase's work belongs to",
			change: func(s *silentRun) { s.role = claim.RoleTester },
		},
		{
			name:   "the tree lingers after its close, so nothing of it runs",
			change: func(s *silentRun) { s.root.LingerUntil = &silentNow },
		},
		{
			name:   "a turn is running",
			change: func(s *silentRun) { inState(s, supervise.StateWorking) },
		},
		{
			name:   "the claim is suspended",
			change: func(s *silentRun) { inState(s, supervise.StateSuspended) },
		},
		{
			name:   "the claim failed",
			change: func(s *silentRun) { inState(s, supervise.StateFailed) },
		},
		{
			name:   "the claim is retired",
			change: func(s *silentRun) { inState(s, supervise.StateRetired) },
		},
		{
			name:   "the claim's process is still coming up",
			change: func(s *silentRun) { inState(s, supervise.StateLaunching) },
		},
		{
			name: "an update round is open on the claim",
			change: func(s *silentRun) {
				s.run.Claim.UpdateRound = supervise.PodRound("7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11")
			},
		},
		{
			name: "the claim holds a task of this run and phase it still works",
			change: func(s *silentRun) {
				s.run.Claim.Pending = &supervise.Delivery{
					ID: "delivery-1", Task: "implement the plan",
					Generation: s.issue.Generation, Phase: s.issue.Phase, QueuedAt: silentNow.Add(-3 * time.Hour),
				}
			},
		},
		{
			name: "a start for the phase is queued and the outbox has still to run it",
			change: func(s *silentRun) {
				s.run.Queued = []record.QueuedSupervise{{ID: 9, Request: record.SuperviseRequest{
					Op: "start", Tree: s.root.Key, Role: claim.RoleImplementer,
					Generation: s.issue.Generation, Phase: s.issue.Phase,
				}}}
			},
		},
		{
			name: "the release of the issue's idle pod is queued and still acts",
			change: func(s *silentRun) {
				s.run.Queued = []record.QueuedSupervise{{ID: 9, Request: record.SuperviseRequest{
					Op: "release", Tree: s.root.Key, Role: claim.RoleImplementer, Generation: s.issue.Generation,
				}}}
			},
		},
		{
			name:   "the claim is recorded in another tree, where its turn is live",
			change: func(s *silentRun) { s.run.Claim.Tree = "LEGION-300" },
		},
		{
			name:   "the last step is inside the bound",
			change: func(s *silentRun) { s.lastStep = silentNow.Add(-5 * time.Minute) },
		},
		{
			name:   "the last step is exactly the bound old, which has not passed it",
			change: func(s *silentRun) { s.lastStep = silentNow.Add(-30 * time.Minute) },
		},
		{
			name:   "the record holds no step and no turn start, so there is nothing to judge from",
			change: func(s *silentRun) { s.lastStep = time.Time{} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := aSilentImplementer()
			if tc.change != nil {
				tc.change(&s)
			}
			if got := s.answer(); got != tc.want {
				t.Errorf("SilentPhaseWorker = %v, want %v, for %s", got, tc.want, tc.name)
			}
		})
	}
}

// A worker the rule names is never one StartFor would start, so admission's repair of a slot
// whose worker has no task (admit's resumeSlottedWorkers, whose gate StartFor is) and this rule
// never act on one claim. Where StartFor does answer true — here the queued release of an idle
// pod, whose wake brings the pod back with the role's task — the rule answers false and leaves
// the claim to that repair.
func TestNoRunIsBothASilentPhaseWorkerAndAStartForsOwn(t *testing.T) {
	silent := aSilentImplementer()
	if !silent.answer() {
		t.Fatalf("the rule's own true case answers false, so this test proves nothing")
	}
	if silent.startFor() {
		t.Errorf("StartFor answers true for the run the rule names silent, so both repairs would start that claim")
	}

	released := aSilentImplementer()
	released.run.Queued = []record.QueuedSupervise{{ID: 9, Request: record.SuperviseRequest{
		Op: "release", Tree: released.root.Key, Role: claim.RoleImplementer, Generation: released.issue.Generation,
	}}}
	if !released.startFor() {
		t.Fatalf("StartFor answers false for a run with a release still queued, so this case proves nothing")
	}
	if released.answer() {
		t.Errorf("the rule names a claim silent that StartFor would start, which is the pair this rule may never make")
	}
}
