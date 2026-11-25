package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The Legion reviewer's Important 1 on #5285 at 7da847c0: two acts on one target (what the act
// takes and the uid of what it takes) each stand recorded under an operation of their own, and
// the runtime reads an act made from its target alone. One target is taken once, so the finish of
// one act on it finishes every record of that target, in the one transaction that queues its
// notice, and a later finish of another record of it finds nothing and queues nothing. A finish
// with no notice, of an act made over a read that found nothing, takes the other records of its
// target too: the read before the act that took the target is the last word of what it held. A
// record of another target stands.
func TestTheFinishOfAnActFinishesEveryRecordOfItsTarget(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	for _, r := range []PodReplacement{
		{Operation: "round-1", Issue: rootIssue, PodUID: "pod-a", Act: runtime.PodAct("pod-a"), Record: []byte(`{}`)},
		{Operation: "launch-1", Issue: rootIssue, PodUID: "pod-a", Act: runtime.PodAct("pod-a"), Record: []byte(`{}`)},
		{Operation: "launch-2", Issue: rootIssue, PodUID: "pod-a", Act: runtime.VolumeAct("uid-sandbox"), Record: []byte(`{}`)},
		{Operation: "launch-3", Issue: rootIssue, Act: runtime.VolumeAct("uid-sandbox"), Record: []byte(`{}`)},
	} {
		if err := st.RecordPodReplacement(ctx, "legion", r); err != nil {
			t.Fatalf("record %s: %v", r.Operation, err)
		}
	}
	notice := workspaceTakenRow(t)

	finish, err := st.FinishPodReplacement(ctx, "legion", rootIssue, "launch-1", false, notice)
	if err != nil || !finish.Found || !finish.Told {
		t.Fatalf("the finish of launch-1 = %+v, %v; want its record finished and its notice queued", finish, err)
	}
	if standing := operationsStanding(t, st); !slices.Equal(standing, []string{"launch-2", "launch-3"}) {
		t.Errorf("records standing once launch-1's act on pod-a is finished = %v, want the two records of the Sandbox's volume: round-1's record of pod-a goes with launch-1's", standing)
	}
	finish, err = st.FinishPodReplacement(ctx, "legion", rootIssue, "round-1", false, notice)
	if err != nil || finish.Found || finish.Told {
		t.Errorf("the later finish of round-1 = %+v, %v; want no record found and nothing queued", finish, err)
	}
	if queued := workspaceTakenQueued(t, st); queued != 1 {
		t.Errorf("workspace-taken notices queued = %d, want one for the one act on pod-a", queued)
	}

	finish, err = st.FinishPodReplacement(ctx, "legion", rootIssue, "launch-2", false, nil)
	if err != nil || !finish.Found || finish.Told {
		t.Fatalf("the finish of launch-2 with no notice = %+v, %v; want its record finished and nothing queued", finish, err)
	}
	if standing := operationsStanding(t, st); len(standing) != 0 {
		t.Errorf("records standing once launch-2's delete of the Sandbox is finished = %v, want none: launch-3's record of the same Sandbox goes with it", standing)
	}
}

// A launch's delete of the issue's Sandbox and its kept volume over a live pod of that Sandbox
// takes the pod with the volume (runtime.TakenPod.WithPod), which its row holds: its finish takes
// the records of acts on that pod too, and leaves a record of another pod (the Legion reviewer's
// Important 1 on #5285 at acda4d1e). The same delete recorded with no pod taken with it leaves the
// pod's records, as a launch's delete over a Sandbox that ran no pod of its own does.
func TestTheFinishOfAVolumeActWithItsPodFinishesThatPodsRecords(t *testing.T) {
	for _, tc := range []struct {
		name     string
		withPod  bool
		standing []string
	}{
		{name: "with_its_pod", withPod: true, standing: []string{"round-b"}},
		{name: "control:_no_pod_taken_with_it", standing: []string{"round-a", "round-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			for _, r := range []PodReplacement{
				{Operation: "round-a", Issue: rootIssue, PodUID: "pod-a", Act: runtime.PodAct("pod-a"), Record: []byte(`{}`)},
				{Operation: "round-b", Issue: rootIssue, PodUID: "pod-b", Act: runtime.PodAct("pod-b"), Record: []byte(`{}`)},
				{Operation: "launch-volume", Issue: rootIssue, PodUID: "pod-a", Act: runtime.VolumeAct("uid-sandbox"), WithPod: tc.withPod, Record: []byte(`{}`)},
			} {
				if err := st.RecordPodReplacement(ctx, "legion", r); err != nil {
					t.Fatalf("record %s: %v", r.Operation, err)
				}
			}
			if finish, err := st.FinishPodReplacement(ctx, "legion", rootIssue, "launch-volume", false, nil); err != nil || !finish.Found {
				t.Fatalf("the finish of launch-volume = %+v, %v; want its record finished", finish, err)
			}
			if standing := operationsStanding(t, st); !slices.Equal(standing, tc.standing) {
				t.Errorf("records standing once the launch's delete of the Sandbox is finished = %v, want %v", standing, tc.standing)
			}
		})
	}
}

// The Legion reviewer's Important on #5285 at 69e00593: one rule names the targets each record's act
// takes (PodReplacement.Targets), and the finish reads it from the rows. A launch's delete of the
// issue's Sandbox and its kept volume over a live pod of it (WithPod) names that pod beside the
// Sandbox. So a round's finish of the pod's act takes the launch's record of the delete, and no
// other record of the Sandbox, since the suspension of a pod takes no volume. A round's finish whose
// call took nothing yields to that record: it goes alone, with nothing queued, and the launch's
// record stands to tell the architect. The next launch's delete of the same Sandbox takes the first
// launch's record, and with it the round's record of the pod that Sandbox ran. A finish that may
// yield and finds no record to yield to takes as any finish does. A record of another pod stands in
// each case.
func TestTheFinishOfAnActTakesTheRecordsOfEveryTargetItTook(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation string
		yield     bool
		// took is every record the finish takes, oldest first, and standing every record left.
		took, standing []string
	}{
		{name: "a_round's_act_on_the_pod", operation: "round-a",
			took: []string{"launch-volume", "round-a"}, standing: []string{"launch-reset", "round-b", "launch-next"}},
		{name: "a_round's_call_that_took_nothing", operation: "round-a", yield: true,
			standing: []string{"launch-reset", "launch-volume", "round-b", "launch-next"}},
		{name: "the_next_delete_of_the_sandbox", operation: "launch-next",
			took: []string{"launch-reset", "launch-volume", "round-a", "launch-next"}, standing: []string{"round-b"}},
		{name: "control:_a_call_that_took_nothing_of_a_pod_no_delete_took", operation: "round-b", yield: true,
			took: []string{"round-b"}, standing: []string{"launch-reset", "launch-volume", "round-a", "launch-next"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			for _, r := range []PodReplacement{
				{Operation: "launch-reset", Issue: rootIssue, Act: runtime.VolumeAct("uid-sandbox"), Record: []byte(`{}`)},
				{Operation: "launch-volume", Issue: rootIssue, PodUID: "pod-a", Act: runtime.VolumeAct("uid-sandbox"), WithPod: true, Record: []byte(`{}`)},
				{Operation: "round-a", Issue: rootIssue, PodUID: "pod-a", Act: runtime.PodAct("pod-a"), Record: []byte(`{}`)},
				{Operation: "round-b", Issue: rootIssue, PodUID: "pod-b", Act: runtime.PodAct("pod-b"), Record: []byte(`{}`)},
				{Operation: "launch-next", Issue: rootIssue, Act: runtime.VolumeAct("uid-sandbox"), Record: []byte(`{}`)},
			} {
				if err := st.RecordPodReplacement(ctx, "legion", r); err != nil {
					t.Fatalf("record %s: %v", r.Operation, err)
				}
			}
			finish, err := st.FinishPodReplacement(ctx, "legion", rootIssue, tc.operation, tc.yield, workspaceTakenRow(t))
			if err != nil || !finish.Found {
				t.Fatalf("the finish of %s = %+v, %v; want its record found", tc.operation, finish, err)
			}
			took := make([]string, 0, len(finish.Took))
			for _, r := range finish.Took {
				took = append(took, r.Operation)
			}
			if yielded := len(tc.took) == 0; !slices.Equal(took, tc.took) || finish.Yielded != yielded || finish.Told == yielded {
				t.Errorf("the finish of %s took %v, yielded %t and told %t; want it to take %v, yield %t and tell %t",
					tc.operation, took, finish.Yielded, finish.Told, tc.took, yielded, !yielded)
			}
			if standing := operationsStanding(t, st); !slices.Equal(standing, tc.standing) {
				t.Errorf("records standing once %s is finished = %v, want %v", tc.operation, standing, tc.standing)
			}
			if queued, want := workspaceTakenQueued(t, st), len(tc.took) > 0; (queued == 1) != want || queued > 1 {
				t.Errorf("workspace-taken notices queued = %d, want one only where the finish took its records", queued)
			}
		})
	}
}

// workspaceTakenRow is a workspace-taken notice to rootIssue's architect, as a finish queues it.
func workspaceTakenRow(t *testing.T) *IdleReleaseRow {
	t.Helper()
	row, err := record.NewOutboxRow(rootIssue, record.Notice{Kind: record.WorkspaceTakenNotice, Role: claim.RoleArchitect,
		Reason: "The pod is gone."}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload}
}

// workspaceTakenQueued is how many workspace-taken notices to rootIssue the outbox holds.
func workspaceTakenQueued(t *testing.T, st *Store) int {
	t.Helper()
	var queued int
	if err := st.Pool().QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'notice' and payload->>'kind' = 'workspace-taken'`,
		rootIssue).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	return queued
}

// operationsStanding is the operation of every record of a pod replacement that stands, oldest
// first.
func operationsStanding(t *testing.T, st *Store) []string {
	t.Helper()
	standing, err := st.PodReplacements(context.Background(), "legion")
	if err != nil {
		t.Fatal(err)
	}
	operations := make([]string, 0, len(standing))
	for _, r := range standing {
		operations = append(operations, r.Operation)
	}
	return operations
}
