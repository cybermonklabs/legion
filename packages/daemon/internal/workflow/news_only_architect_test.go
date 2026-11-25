package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// News of the approved design alone is no work a root architect holds, whether it waits in the
// outbox as a start (record.SuperviseRequest.News) or on the claim as a task
// (supervise.Delivery.NewsOnly). It hands the architect no entry's work, so the rule that closes
// the difference between the work a role is owed and the work it holds (dispatch://INFRA-2614)
// reads such an architect as holding nothing:
//
//   - the sweep's reading of a start on its way (ArchitectStartPending) holds no start of the
//     entry the architect's row names back for it, so the sweep writes that start while the news
//     waits, and the entry's task then takes the untaken news task's place in the claim's hands
//     (supervise's Machine.supersedes), carrying that news itself;
//   - the second stall arm's reading of the daemon's next move for a failed claim
//     (ArchitectRelaunchOf) counts no start of news alone, which finishes without acting at a
//     failed claim (the daemon's newsOnlyRefusal) and so relaunches nothing. The project has
//     ended no pause, so the zero floor bounds no start's age.
//
// A start or a task that brings the architect work is still read as one, as the controls show.
// The root stands at generation 2, and its architect's row names entry 3.
func TestNewsAloneHoldsNoRootArchitectStartBack(t *testing.T) {
	t.Parallel()
	const rowEntry = 3
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Phase: phase.Admitted, Generation: 2}
	newsStart := record.QueuedSupervise{ID: 7, Request: record.SuperviseRequest{Op: "start", Tree: root.Key, Role: claim.RoleArchitect,
		Generation: root.Generation, News: true}}
	wakeStart := record.QueuedSupervise{ID: 7, Request: record.SuperviseRequest{Op: "start", Tree: root.Key, Role: claim.RoleArchitect,
		Generation: root.Generation, Task: "Your pod was released and has come back."}}
	newsTask := supervise.Delivery{ID: "outbox:6", Task: "The approved design of LEGION-208 changed.", Generation: root.Generation,
		News: []byte(`{"task":""}`), NewsOnly: true}
	wakeTask := supervise.Delivery{ID: "outbox:6", Task: "Your pod was released and has come back.", Generation: root.Generation}
	holding := func(task supervise.Delivery) *record.RoleClaim {
		return &record.RoleClaim{State: supervise.StateIdle, Tree: root.Key, GivenEntry: rowEntry - 1, Pending: &task}
	}

	for _, tc := range []struct {
		name    string
		run     record.RoleRun
		pending bool
		why     string
	}{
		{
			name: "no claim, and a start of news alone queued", run: record.RoleRun{Queued: []record.QueuedSupervise{newsStart}},
			why: "a start of news alone reaches no claim that does not exist, and hands no entry's work to one that does",
		},
		{
			name: "a claim holding an untaken task of news alone", run: record.RoleRun{Claim: holding(newsTask)},
			why: "the task holds no entry's work, and the entry's task takes its place while no turn has taken it",
		},
		{
			name: "a claim holding an untaken task of news alone, and a start of news alone queued",
			run:  record.RoleRun{Queued: []record.QueuedSupervise{newsStart}, Claim: holding(newsTask)},
			why:  "neither brings the architect the work its row names",
		},
		{
			name: "control: no claim, and a wake's start queued", run: record.RoleRun{Queued: []record.QueuedSupervise{wakeStart}},
			pending: true, why: "that start creates the claim, and a second would start two architects of one tree",
		},
		{
			name: "control: a claim holding an untaken wake's task", run: record.RoleRun{Claim: holding(wakeTask)},
			pending: true, why: "a wake's task is superseded by nothing, so the claim sends it first and the start would wait behind it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if reason, pending := ArchitectStartPending(root, rowEntry, tc.run); pending != tc.pending {
				t.Fatalf("ArchitectStartPending = %t (%q), want %t: %s", pending, reason, tc.pending, tc.why)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		queued record.QueuedSupervise
		want   ArchitectRelaunch
		why    string
	}{
		{name: "a start of news alone queued", queued: newsStart, want: RelaunchNone,
			why: "it finishes without acting at a failed claim, so it is no move that brings the claim back"},
		{name: "control: a wake's start queued", queued: wakeStart, want: RelaunchNext,
			why: "a start of the run that brings the architect work is the daemon's next move"},
	} {
		t.Run("a failed claim with "+tc.name, func(t *testing.T) {
			failed := record.RoleRun{Queued: []record.QueuedSupervise{tc.queued},
				Claim: &record.RoleClaim{State: supervise.StateFailed, Tree: root.Key, GivenEntry: rowEntry}}
			if got := ArchitectRelaunchOf(failed, root, time.Time{}, time.Now()); got != tc.want {
				t.Fatalf("ArchitectRelaunchOf = %d, want %d: %s", got, tc.want, tc.why)
			}
		})
	}
}
