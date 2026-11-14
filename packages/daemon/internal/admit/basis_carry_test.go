package admit

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The tree a linger hands a child of over in these tests (dispatch://INFRA-3320, R2 and R8): the
// old root LEGION-208, done and lingering, whose spec step rests on the approval of LEGION-100,
// the ancestor, and LEGION-209, the child the hand-over makes a root of its own.
const (
	carryRoot     = "LEGION-208"
	carryChild    = "LEGION-209"
	carryAncestor = "LEGION-100"
	carryRootDoc  = "artifact-208"
	carryBasisDoc = "artifact-100"
	carryAsk      = "ask-100"
)

// carryRegisteredAt is the daemon's own time of the old root's registration on the basis.
var carryRegisteredAt = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// carryBasis is a person's approval of version 3 of LEGION-100's primary document, as the old
// root's registration recorded it.
func carryBasis() record.GateBasis {
	return record.GateBasis{Issue: carryAncestor, ArtifactID: carryBasisDoc, Version: 3,
		Approval: record.EventMark{Seq: 40, At: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)}, Approver: "user:alice",
		Serves: []string{"dispatch://LEGION-100/spec#b-r1@v3"}, Reason: "LEGION-208 delivers r1 of LEGION-100.",
		Session: "session-architect", RegisteredAt: carryRegisteredAt}
}

// carriedOnBasis is the old root's spec step open at version 4 on carryBasis, keeping apart the
// person's approval of version 2 of its own document.
func carriedOnBasis() record.DesignGate {
	return record.OpenOnBasis(record.DesignGate{Issue: carryRoot, ArtifactID: carryRootDoc, LatestVersion: 4,
		ApprovedVersion: new(2), ApprovedMark: record.EventMark{Seq: 9, At: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)}}, carryBasis())
}

// seedCarry records the old root done and lingering, with the view fact its own spec registration
// wrote, its child mid-merge in its tree with the hand-over fence that the linger's expiry wrote,
// each of gates as a row of the old root, and the ancestor as seed records it.
func seedCarry(t *testing.T, pool *pgxpool.Pool, ancestor func(context.Context, pgx.Tx, *record.Postgres) error, gates ...record.DesignGate) {
	t.Helper()
	until := fixedNow.Add(time.Hour)
	parent := carryRoot
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		puts := []error{
			records.PutIssue(ctx, tx, record.Issue{Key: carryRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: carryRoot, Project: testProject, Title: carryRoot, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until,
				ViewOf: carryAncestor}),
			records.PutIssue(ctx, tx, record.Issue{Key: carryChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: carryRoot, Project: testProject, Title: carryChild, Parent: &parent, Phase: phase.Merging, Generation: 1, Status: "retro",
				Rank: "B", LastDispatchSeq: 1}),
			records.PutPullRequest(ctx, tx, record.PullRequest{Issue: carryChild, Repo: "acme/widgets", Number: 42, Branch: "legion/" + carryChild,
				HeadSHA: "head-42", Generation: 1, State: record.PullRequestOpen}),
			records.PutHandover(ctx, tx, record.Handover{Issue: carryChild, Tree: carryRoot, Generation: 1}),
		}
		for _, gate := range gates {
			puts = append(puts, records.PutGate(ctx, tx, gate))
		}
		if ancestor != nil {
			puts = append(puts, ancestor(ctx, tx, records))
		}
		for _, put := range puts {
			if put != nil {
				t.Fatalf("seed the lingering tree: %v", put)
			}
		}
	})
}

// handOver applies the daemon's own todo of the child, which the linger's hand-over writes, and
// fails the test unless the child became a root of its own at generation 2.
func handOver(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: carryChild, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: carryChild, Parent: carryRoot, Rank: "B", HandedOver: true, DaemonActor: true}, admission.engine)
	if got := issue(t, pool, carryChild); got.Tree != carryChild || got.Generation != 2 {
		t.Fatalf("the handed-over child = %#v, want its own tree at generation 2", got)
	}
}

// mirrorAncestor records what Dispatch last showed of the ancestor in the daemon's mirror.
func mirrorAncestor(status string) func(context.Context, pgx.Tx, *record.Postgres) error {
	return func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		return records.PutDispatchIssue(ctx, tx, record.DispatchIssueRow{Key: carryAncestor, Project: testProject, Status: status, Seq: 5})
	}
}

// recordAncestor records the ancestor as the root of a tree of its own standing in at, as the
// daemon writes Dispatch's status for that phase, and mirrors that status.
func recordAncestor(at phase.Phase, status string) func(context.Context, pgx.Tx, *record.Postgres) error {
	return func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: carryAncestor, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: carryAncestor, Project: testProject, Title: carryAncestor, Phase: at, Generation: 1, Status: status, DispatchStatus: status,
			Rank: "A", HandedOver: true, LastDispatchSeq: 5}); err != nil {
			return err
		}
		return mirrorAncestor(status)(ctx, tx, records)
	}
}

// childGates is every gate the child holds after the carry, by step.
func childGates(t *testing.T, pool *pgxpool.Pool) map[record.DesignStep]record.DesignGate {
	t.Helper()
	var gates map[record.DesignStep]record.DesignGate
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if gates, err = record.NewStore().GatesOf(context.Background(), tx, carryChild); err != nil {
			t.Fatalf("read the child's gates: %v", err)
		}
	})
	return gates
}

// childCatchUp is the catch-up the child's architect reads, as the daemon composes it for a
// launch of that architect (workflow.QueueCatchUp), each step of steps by step.
func childCatchUp(t *testing.T, pool *pgxpool.Pool, steps []record.DesignStep) map[record.DesignStep]record.CatchUpGate {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		child, err := records.Issue(ctx, tx, carryChild)
		if err != nil || child == nil {
			t.Fatalf("read the child: %#v, %v", child, err)
		}
		if err := workflow.QueueCatchUp(ctx, tx, records, steps, *child, 1, fixedNow, time.Time{}); err != nil {
			t.Fatalf("queue the child's catch-up: %v", err)
		}
	})
	var catchUp *record.CatchUp
	for _, effect := range effects(t, pool) {
		if notice, ok := effect.payload.(record.Notice); ok && effect.issue == carryChild && notice.Kind == "catch-up" {
			catchUp = notice.CatchUp
		}
	}
	if catchUp == nil {
		t.Fatalf("the outbox holds no catch-up of %s", carryChild)
	}
	gates := map[record.DesignStep]record.CatchUpGate{}
	for _, gate := range catchUp.Gates {
		gates[gate.Step] = gate
	}
	return gates
}

// restingOn is the key of each issue whose gate rests on the ancestor (record.Store.GatesRestingOn).
func restingOn(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var keys []string
	inTx(t, pool, func(tx pgx.Tx) {
		gates, err := record.NewStore().GatesRestingOn(context.Background(), tx, carryAncestor)
		if err != nil {
			t.Fatalf("read the gates resting on %s: %v", carryAncestor, err)
		}
		for _, gate := range gates {
			keys = append(keys, gate.Issue)
		}
	})
	return keys
}

// sameBasis says whether got is want as the store reads it back: every field, the times compared
// as instants.
func sameBasis(got *record.GateBasis, want record.GateBasis) bool {
	return got != nil && got.Issue == want.Issue && got.ArtifactID == want.ArtifactID && got.Version == want.Version &&
		got.Approval.Seq == want.Approval.Seq && got.Approval.At.Equal(want.Approval.At) && got.Approver == want.Approver &&
		slices.Equal(got.Serves, want.Serves) && got.Reason == want.Reason && got.Session == want.Session &&
		got.RegisteredAt.Equal(want.RegisteredAt) && got.Pending == nil
}

// A child that a linger hands over carries a step that an ancestor's approval opened with its
// basis, the person's approval of an earlier version that the step keeps apart, and its rest on
// the ancestor, while the ancestor is neither done nor accepted: the child reads the step as open
// on that basis in its record and its catch-up, and the ancestor's sign-off waits for it as it
// waited for the old root. The carry writes no view fact, so the child's tasks name no ancestor's
// requirements: its merger is resumed with no view sentence, though the old root's spec was a view.
func TestAHandOverCarriesAStepOpenOnABasisWithItsBasisAndItsRest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ancestor func(context.Context, pgx.Tx, *record.Postgres) error
	}{
		{name: "the ancestor implements in a tree of its own", ancestor: recordAncestor(phase.Implementing, "in_progress")},
		{name: "the ancestor is in no tree the daemon records", ancestor: mirrorAncestor("in_progress")},
		{name: "the daemon knows nothing of the ancestor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			old := carriedOnBasis()
			seedCarry(t, pool, tc.ancestor, old)

			handOver(t, pool, admission)

			gate, ok := childGates(t, pool)[record.DesignStepSpec]
			if !ok || gate.ArtifactID != carryRootDoc || gate.LatestVersion != 4 || !gate.OnBasis() || !record.GateOpen(gate) ||
				gate.ApprovedVersion == nil || *gate.ApprovedVersion != 4 || gate.ApprovedMark != (record.EventMark{}) {
				t.Fatalf("the carried spec step = %#v, want it open at version 4 of the old root's document on the ancestor's basis", gate)
			}
			if !sameBasis(gate.Basis, *old.Basis) {
				t.Fatalf("the carried basis = %#v, want the old root's %#v", gate.Basis, *old.Basis)
			}
			if kept := gate.ApprovedByPerson(); kept == nil || kept.Version != 2 || kept.Mark.Seq != 9 {
				t.Fatalf("the person's approval the carried step keeps = %#v, want the old root's of version 2", kept)
			}
			if gate.RestsOn != carryAncestor {
				t.Fatalf("the carried step rests on %q, want %s", gate.RestsOn, carryAncestor)
			}
			if got := restingOn(t, pool); !slices.Equal(got, []string{carryRoot, carryChild}) {
				t.Fatalf("the gates resting on %s are of %v, want the old root's and the child's", carryAncestor, got)
			}
			if got := childCatchUp(t, pool, admission.engine.GatedSteps())[record.DesignStepSpec]; got.Unregistered || !got.Open ||
				got.Artifact != carryRootDoc || got.Version != 4 {
				t.Fatalf("the child's catch-up of the spec step = %#v, want it registered and open at version 4", got)
			}
			inTx(t, pool, func(tx pgx.Tx) {
				handed, err := record.NewStore().HandedOverRoot(context.Background(), tx, carryChild)
				if err != nil || handed == nil || handed.Generation != 2 || !handed.GateCarried {
					t.Fatalf("the handed-over root = %#v, %v; want generation 2 with its gate carried", handed, err)
				}
			})

			if got := issue(t, pool, carryChild).ViewOf; got != "" {
				t.Fatalf("the child's view fact = %q, want none: the old root's view fact is of its own documents", got)
			}
			var mergerTasks []string
			for _, effect := range effects(t, pool) {
				if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == carryChild && request.Op == "start" &&
					request.Role == claim.RoleMerger {
					mergerTasks = append(mergerTasks, request.Task)
				}
			}
			want := "Continue " + carryChild + ". Issue: " + carryChild + ". Phase: merging. Resume the existing phase work."
			if len(mergerTasks) != 1 || mergerTasks[0] != want {
				t.Fatalf("the carried merger's tasks = %q, want one resuming merging with no view sentence: %q", mergerTasks, want)
			}
		})
	}
}

// A basis is only on an ancestor that is neither done nor accepted (R1), so a hand-over after the
// ancestor is done, or accepted, carries no row for the step that rests on it: the child's
// catch-up reports that step unregistered, and its architect registers the child's own document
// for it. Every other step of the old root comes as before. Done is what the daemon's mirror of
// Dispatch shows, and accepted is retro there, or retro or merging in the daemon's record.
func TestAHandOverOntoAnAcceptedAncestorCarriesNoRowForTheStepOnItsBasis(t *testing.T) {
	steps := []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}
	for _, tc := range []struct {
		name     string
		ancestor func(context.Context, pgx.Tx, *record.Postgres) error
	}{
		{name: "Dispatch shows the ancestor done", ancestor: mirrorAncestor(record.DoneStatus)},
		{name: "Dispatch shows the ancestor in retro", ancestor: mirrorAncestor("retro")},
		{name: "the daemon records the ancestor in retro", ancestor: recordAncestor(phase.Retro, "retro")},
		{name: "the daemon records the ancestor in merging", ancestor: func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
			// The record alone says so: the mirror still shows the ancestor at work.
			if err := recordAncestor(phase.Merging, "retro")(ctx, tx, records); err != nil {
				return err
			}
			return records.PutDispatchIssue(ctx, tx, record.DispatchIssueRow{Key: carryAncestor, Project: testProject, Status: "in_progress", Seq: 6})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmissionWithSteps(t, 1, steps, slog.New(slog.NewTextHandler(io.Discard, nil)))
			criteria := record.DesignGate{Issue: carryRoot, Step: record.DesignStepCriteria, ArtifactID: "artifact-criteria", LatestVersion: 2,
				ApprovedVersion: new(2)}
			seedCarry(t, pool, tc.ancestor, criteria, carriedOnBasis())

			handOver(t, pool, admission)

			gates := childGates(t, pool)
			if gate, ok := gates[record.DesignStepSpec]; ok {
				t.Fatalf("the child holds a spec step %#v, want none: the ancestor its basis names is accepted", gate)
			}
			if gate, ok := gates[record.DesignStepCriteria]; !ok || gate.ArtifactID != "artifact-criteria" || !record.GateOpen(gate) {
				t.Fatalf("the carried criteria step = %#v, want the old root's, open on the person's approval", gate)
			}
			if got := restingOn(t, pool); !slices.Equal(got, []string{carryRoot}) {
				t.Fatalf("the gates resting on %s are of %v, want the old root's alone", carryAncestor, got)
			}
			catchUp := childCatchUp(t, pool, steps)
			if got := catchUp[record.DesignStepSpec]; !got.Unregistered || got.Open {
				t.Fatalf("the child's catch-up of the spec step = %#v, want it unregistered", got)
			}
			if got := catchUp[record.DesignStepCriteria]; got.Unregistered || !got.Open {
				t.Fatalf("the child's catch-up of the criteria step = %#v, want it carried open", got)
			}
			inTx(t, pool, func(tx pgx.Tx) {
				handed, err := record.NewStore().HandedOverRoot(context.Background(), tx, carryChild)
				if err != nil || handed == nil || handed.Generation != 2 || !handed.GateCarried {
					t.Fatalf("the handed-over root = %#v, %v; want generation 2 with its gate carried", handed, err)
				}
			})
		})
	}
}

// A step that waits on the ancestor's open approval request comes closed and registered on the
// old root's document with no basis: the wait was the old root's registration, on one request, and
// the child's architect registers again. A step whose basis a new version of the old root's
// document ended comes as the old root held it, closed on the person's approval it kept. Either
// keeps the rest the old root had (record.DesignGate.RestsOn), from the first time a basis on the
// ancestor opened the step, while the ancestor is neither done nor accepted: the child rests on
// it as the old root did. A wait that never opened the step leaves no rest, and a rest on an
// accepted ancestor does not come with the step.
func TestAHandOverCarriesAStepWithNoStandingBasisClosedWithTheRestTheOldRootHad(t *testing.T) {
	ended := record.CloseBasis(carriedOnBasis())
	ended.LatestVersion = 5
	pending := carryBasis()
	pending.Version = 4
	for _, tc := range []struct {
		name     string
		old      record.DesignGate
		ancestor func(context.Context, pgx.Tx, *record.Postgres) error
		approved *int
		latest   int
		rests    bool
	}{
		{name: "a step that waits on the ancestor's request and never opened",
			old:      record.PendOnBasis(record.DesignGate{Issue: carryRoot, ArtifactID: carryRootDoc, LatestVersion: 4}, pending, carryAsk),
			ancestor: recordAncestor(phase.Implementing, "in_progress"), latest: 4},
		{name: "a step that waits on the ancestor's request after a basis on it opened the step",
			old:      record.PendOnBasis(carriedOnBasis(), pending, carryAsk),
			ancestor: recordAncestor(phase.Implementing, "in_progress"), approved: new(2), latest: 4, rests: true},
		{name: "a step whose basis a new version ended", old: ended,
			ancestor: recordAncestor(phase.Implementing, "in_progress"), approved: new(2), latest: 5, rests: true},
		{name: "a step whose basis a new version ended, on an ancestor Dispatch shows done", old: ended,
			ancestor: mirrorAncestor(record.DoneStatus), approved: new(2), latest: 5},
		{name: "a step whose basis a new version ended, on an ancestor the daemon records in retro", old: ended,
			ancestor: recordAncestor(phase.Retro, "retro"), approved: new(2), latest: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			seedCarry(t, pool, tc.ancestor, tc.old)

			handOver(t, pool, admission)

			gate, ok := childGates(t, pool)[record.DesignStepSpec]
			if !ok || gate.ArtifactID != carryRootDoc || gate.LatestVersion != tc.latest || gate.Basis != nil || gate.KeptPerson != nil ||
				record.GateOpen(gate) {
				t.Fatalf("the carried spec step = %#v, want it closed at version %d of the old root's document, with no basis",
					gate, tc.latest)
			}
			if (gate.ApprovedVersion == nil) != (tc.approved == nil) || tc.approved != nil && *gate.ApprovedVersion != *tc.approved {
				t.Fatalf("the carried step's approval = %v, want %v", gate.ApprovedVersion, tc.approved)
			}
			wantRest, wantResting := "", []string{carryRoot}
			if tc.rests {
				wantRest, wantResting = carryAncestor, []string{carryRoot, carryChild}
			}
			if gate.RestsOn != wantRest {
				t.Fatalf("the carried step rests on %q, want %q", gate.RestsOn, wantRest)
			}
			if got := restingOn(t, pool); !slices.Equal(got, wantResting) {
				t.Fatalf("the gates resting on %s are of %v, want %v", carryAncestor, got, wantResting)
			}
			inTx(t, pool, func(tx pgx.Tx) {
				waiting, err := record.NewStore().GatesByPendingAsk(context.Background(), tx, carryAsk)
				if err != nil {
					t.Fatalf("read the gates pending on %s: %v", carryAsk, err)
				}
				for _, row := range waiting {
					if row.Issue == carryChild {
						t.Fatalf("the child's step waits on %s: %#v, want no wait carried", carryAsk, row)
					}
				}
			})
			if got := childCatchUp(t, pool, admission.engine.GatedSteps())[record.DesignStepSpec]; got.Unregistered || got.Open || got.Version != tc.latest {
				t.Fatalf("the child's catch-up of the spec step = %#v, want it registered and closed at version %d", got, tc.latest)
			}
		})
	}
}
