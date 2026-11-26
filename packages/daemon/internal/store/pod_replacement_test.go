package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/lockwaittest"

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
	t.Parallel()
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
	notice := workspaceTaken(t, nil)

	finish, err := st.FinishPodReplacement(ctx, "legion", rootIssue, "launch-1", nil, notice)
	if err != nil || !finish.Found || !finish.Told {
		t.Fatalf("the finish of launch-1 = %+v, %v; want its record finished and its notice queued", finish, err)
	}
	if standing := operationsStanding(t, st); !slices.Equal(standing, []string{"launch-2", "launch-3"}) {
		t.Errorf("records standing once launch-1's act on pod-a is finished = %v, want the two records of the Sandbox's volume: round-1's record of pod-a goes with launch-1's", standing)
	}
	finish, err = st.FinishPodReplacement(ctx, "legion", rootIssue, "round-1", nil, notice)
	if err != nil || finish.Found || finish.Told {
		t.Errorf("the later finish of round-1 = %+v, %v; want no record found and nothing queued", finish, err)
	}
	if queued := workspaceTakenQueued(t, st); queued != 1 {
		t.Errorf("workspace-taken notices queued = %d, want one for the one act on pod-a", queued)
	}

	finish, err = st.FinishPodReplacement(ctx, "legion", rootIssue, "launch-2", nil, nil)
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
	t.Parallel()
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
			if finish, err := st.FinishPodReplacement(ctx, "legion", rootIssue, "launch-volume", nil, nil); err != nil || !finish.Found {
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
// call took nothing yields to that record: it takes nothing and queues nothing, the record its yield
// answers is written over the launch's, which tells the architect once its act is confirmed from the
// read that tells most of the two (the Legion reviewer's Medium 1 on #5285 at 4b123279), and the
// round's record goes, so no record of a call that took nothing is left to be read later as an act of
// its own (the Legion reviewer's Medium 1 on #5285 at 18d81dad). The next launch's delete of the same
// Sandbox takes the first launch's record, and with it the round's record of the pod that Sandbox
// ran. A finish that may yield and finds no record to yield to takes as any finish does, and asks its
// yield nothing. A record of another pod stands in each case.
func TestTheFinishOfAnActTakesTheRecordsOfEveryTargetItTook(t *testing.T) {
	t.Parallel()
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
			var handed []PodReplacement
			// moved is the record the yield answers for the record it yields to, and onto that record
			// and the two records, oldest first, the yield was handed.
			moved := []byte(`{"moved": "round-a"}`)
			var onto []string
			var yield func(onto PodReplacement, both []PodReplacement) ([]byte, error)
			if tc.yield {
				yield = func(to PodReplacement, both []PodReplacement) ([]byte, error) {
					names := make([]string, 0, len(both))
					for _, r := range both {
						names = append(names, r.Operation)
					}
					onto = append(onto, to.Operation+"<-"+strings.Join(names, ","))
					return moved, nil
				}
			}
			finish, err := st.FinishPodReplacement(ctx, "legion", rootIssue, tc.operation, yield, workspaceTaken(t, &handed))
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
			if len(handed) != len(finish.Took) || len(handed) > 0 && handed[0].Operation != took[0] {
				t.Errorf("the notice was asked for with %d records, want every record the finish took, %v, oldest first", len(handed), took)
			}
			if standing := operationsStanding(t, st); !slices.Equal(standing, tc.standing) {
				t.Errorf("records standing once %s is finished = %v, want %v", tc.operation, standing, tc.standing)
			}
			if queued, want := workspaceTakenQueued(t, st), len(tc.took) > 0; (queued == 1) != want || queued > 1 {
				t.Errorf("workspace-taken notices queued = %d, want one only where the finish took its records", queued)
			}
			wantOnto := []string(nil)
			if finish.Yielded {
				wantOnto = []string{"launch-volume<-launch-volume," + tc.operation}
				var record string
				if err := st.Pool().QueryRow(ctx, `select record::text from pod_replacements where operation = 'launch-volume'`).Scan(&record); err != nil || record != `{"moved": "round-a"}` {
					t.Errorf("the record of launch-volume after the yield = %q, %v; want the record the yield answered, %s", record, err, moved)
				}
			}
			if !slices.Equal(onto, wantOnto) {
				t.Errorf("the yield was asked %v, want %v", onto, wantOnto)
			}
		})
	}
}

// The Legion reviewer's fix 1 on #5285 at 4b123279: two finishes of one issue that run at once,
// each of a target of its own, both commit. Each finish takes the rows of its issue's records in one
// order, oldest first, before it holds any of them, so neither holds a row that the other waits on
// while it waits itself, and PostgreSQL aborts neither as a deadlock (40P01). other plays the finish
// that took the older record's row first: it asks for the newer record's row once the finish of the
// newer record waits.
func TestTwoFinishesOfOneIssueAtOnceBothCommit(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	for _, r := range []PodReplacement{
		{Operation: "round-a", Issue: rootIssue, PodUID: "pod-a", Act: runtime.PodAct("pod-a"), Record: []byte(`{}`)},
		{Operation: "round-b", Issue: rootIssue, PodUID: "pod-b", Act: runtime.PodAct("pod-b"), Record: []byte(`{}`)},
	} {
		if err := st.RecordPodReplacement(ctx, "legion", r); err != nil {
			t.Fatalf("record %s: %v", r.Operation, err)
		}
	}
	other, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	lock := func(operation string) error {
		_, err := other.Exec(ctx, `select 1 from pod_replacements where project = 'legion' and issue = $1 and operation = $2 for update`,
			rootIssue, operation)
		return err
	}
	if err := lock("round-a"); err != nil {
		t.Fatalf("lock round-a's row: %v", err)
	}
	type result struct {
		finish PodReplacementFinish
		err    error
	}
	finished := make(chan result, 1)
	go func() {
		finish, err := st.FinishPodReplacement(ctx, "legion", rootIssue, "round-b", nil, nil)
		finished <- result{finish, err}
	}()
	awaitLockWait(t, st)
	if err := lock("round-b"); err != nil {
		t.Fatalf("the other finish's lock of round-b's row while the finish of round-b waits = %v; want it granted: a finish holds no row of its issue while it waits for an older one", err)
	}
	if err := other.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-finished; got.err != nil || !got.finish.Found {
		t.Fatalf("the finish of round-b = %+v, %v; want its record finished", got.finish, got.err)
	}
	if standing := operationsStanding(t, st); !slices.Equal(standing, []string{"round-a"}) {
		t.Errorf("records standing once round-b is finished = %v, want round-a's", standing)
	}
}

// awaitLockWait waits until a session of st's database waits on a lock.
func awaitLockWait(t *testing.T, st *Store) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waiting int
		if err := st.Pool().QueryRow(context.Background(), `select count(*) from pg_stat_activity
			where datname = current_database() and `+lockwaittest.Waiting).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no session waited on a lock within 10s")
		}
	}
}

// workspaceTaken is the notice a finish asks for: a workspace-taken notice to rootIssue's
// architect. It keeps in handed, where it is not nil, the records the finish took.
func workspaceTaken(t *testing.T, handed *[]PodReplacement) func([]PodReplacement) (*IdleReleaseRow, error) {
	t.Helper()
	row, err := record.NewOutboxRow(rootIssue, record.Notice{Kind: record.WorkspaceTakenNotice, Role: claim.RoleArchitect,
		Reason: "The pod is gone."}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return func(took []PodReplacement) (*IdleReleaseRow, error) {
		if handed != nil {
			*handed = took
		}
		return &IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload}, nil
	}
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
