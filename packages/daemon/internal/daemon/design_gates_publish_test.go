package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// gatePublishProject is the Dispatch project every design-gate publication here records under. The
// outbox scopes a row to its issue key's prefix (record.Store.DueRows), so the project the runner
// claims for and the keys the records carry are one.
const gatePublishProject = "LEGION"

// gatePublishSteps are the design-gate steps these deployments gate. Two of them, because an
// issue's publication carries the pairs of every step at once: a deployment of one step could not
// tell a publication of the issue's whole set from a publication of the row a write touched.
var gatePublishSteps = []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}

// gatePublished is one design-gate publication as Dispatch received it: the issue whose row it was
// written onto and the set it carried.
type gatePublished struct {
	Issue string
	Gates []dispatch.DesignGateDocument
}

// gatePublishDispatch is a Dispatch that takes design-gate publications and records each. answers
// is what the next PUTs answer, oldest first, so a test drives an outage, a refusal and a success
// in the order its rows meet them; a PUT past the list is taken.
type gatePublishDispatch struct {
	puts    []gatePublished
	answers []error
}

func (d *gatePublishDispatch) PutDesignGates(_ context.Context, key string, gates []dispatch.DesignGateDocument) error {
	d.puts = append(d.puts, gatePublished{Issue: key, Gates: append(make([]dispatch.DesignGateDocument, 0, len(gates)), gates...)})
	if len(d.answers) == 0 {
		return nil
	}
	answer := d.answers[0]
	d.answers = d.answers[1:]
	return answer
}

// taken is every publication sent to Dispatch since the last call, refused ones included, and
// empties the record: each assertion of "this act published" or "this act published nothing"
// reads the writes made since the one before it.
func (d *gatePublishDispatch) taken() []gatePublished {
	taken := d.puts
	d.puts = nil
	return taken
}

func (d *gatePublishDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return nil, nil
}
func (d *gatePublishDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	return dispatch.Issue{Key: key}, nil
}
func (d *gatePublishDispatch) SetStatus(context.Context, string, string) (dispatch.Issue, error) {
	return dispatch.Issue{}, nil
}
func (d *gatePublishDispatch) SetStatusAt(context.Context, dispatch.Issue, string) (dispatch.Issue, error) {
	return dispatch.Issue{}, nil
}
func (d *gatePublishDispatch) ReleaseClaim(context.Context, string) error { return nil }
func (d *gatePublishDispatch) HandOver(context.Context, dispatch.Issue, string) error {
	return errors.New("gatePublishDispatch takes no hand-over")
}
func (d *gatePublishDispatch) PostMessage(context.Context, string, string) error { return nil }
func (d *gatePublishDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *gatePublishDispatch) EventsAfter(context.Context, string, int64) ([]json.RawMessage, error) {
	return nil, nil
}
func (d *gatePublishDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}
func (d *gatePublishDispatch) Handoff(context.Context, string) (dispatch.Handoff, error) {
	return dispatch.Handoff{}, nil
}
func (d *gatePublishDispatch) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	return nil, nil
}
func (d *gatePublishDispatch) WebhookAppInstalled(context.Context, string) (bool, error) {
	return true, nil
}
func (d *gatePublishDispatch) PutGatedSteps(context.Context, string, []string) error { return nil }

// gatePublishRig is the daemon over a database of its own: the workflow engine and the admission
// every gate path runs through, and the board lane that takes the project's design-gate
// publications to Dispatch. Nothing here stands in for a gate write — the tests drive the facts
// the architect's registration, a person's review, an adoption, a re-admission and a child's
// re-entry arrive as, and read what Dispatch was told.
type gatePublishRig struct {
	t         *testing.T
	ctx       context.Context
	pool      *store.Pool
	records   *record.Postgres
	engine    *workflow.Engine
	admission *admit.Admission
	client    *gatePublishDispatch
	runner    *outbox
	// ahead moves the rig's clock past the wall clock, so a row's backoff is read rather than
	// waited out. The clock is the wall clock plus this offset rather than a moment of its own,
	// because the one collaborator a test cannot hand a clock to is admission, whose own now is
	// the wall clock (admit.New): a publication it queues is due at the wall clock, and a rig
	// frozen at a date of its own would never find one due.
	ahead time.Duration
}

func newGatePublishRig(t *testing.T, slots int) *gatePublishRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	rig := &gatePublishRig{t: t, ctx: context.Background(), pool: pool, records: records, client: &gatePublishDispatch{}}
	rig.engine = workflow.New(records, workflow.Config{Project: gatePublishProject, Repositories: testRepositories,
		Steps: gatePublishSteps, Linger: time.Hour, Clock: rig.clock}, quietLogger())
	rig.admission = admit.New(records, rig.engine, slots, gatePublishProject, quietLogger())
	rig.runner = &outbox{pool: pool, records: records, dispatch: rig.client, dispatchProject: gatePublishProject,
		now: rig.clock, log: quietLogger()}
	return rig
}

// clock is the rig's now: the wall clock, which is the clock admission queues under, moved by
// whatever the test has advanced.
func (r *gatePublishRig) clock() time.Time { return time.Now().UTC().Add(r.ahead) }

// inTx runs fn in a transaction of its own: how the rigs seed the records a daemon that had
// already run would hold, and how they read those records back.
func (r *gatePublishRig) inTx(fn func(ctx context.Context, tx pgx.Tx) error) {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error { return fn(r.ctx, tx) }); err != nil {
		r.t.Fatalf("the rig's transaction: %v", err)
	}
}

// seedRoot records key as a root of its own tree, running under a tree lifecycle the workflow
// opened: what admission's promotion leaves, and what record.TreeLive reads as a live tree.
func (r *gatePublishRig) seedRoot(key string, at phase.Phase, rank string) {
	r.t.Helper()
	token, err := claim.ProjectToken(gatePublishProject)
	if err != nil {
		r.t.Fatalf("the project token of %s: %v", gatePublishProject, err)
	}
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Project: gatePublishProject, Title: key, Tree: key, Phase: at, Generation: 1,
			Status: "in_progress", DispatchStatus: "in_progress", Rank: rank, HandedOver: true, LastDispatchSeq: 1}); err != nil {
			return err
		}
		slots, err := r.records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		if err := r.records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: len(slots), AdmittedAt: r.clock()}); err != nil {
			return err
		}
		_, err = r.records.OpenTreeLifecycle(ctx, tx, token, key, treelifecycle.AuthorityWorkflow)
		return err
	})
}

// seedMember records key as an issue of tree under parent, at the phase and status given: the
// member of a tree whose generation a clear empties.
func (r *gatePublishRig) seedMember(key, tree, parent string, at phase.Phase, status, rank string, seq int64) {
	r.t.Helper()
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return r.records.PutIssue(ctx, tx, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Project: gatePublishProject, Title: key, Tree: tree, Parent: record.ParentOf(parent),
			Phase: at, Generation: 1, Status: status, DispatchStatus: status, Rank: rank, HandedOver: true, LastDispatchSeq: seq})
	})
}

// seedGates writes the gate rows of issue, one per step of pairs, each approved at version 1: the
// rows a tree already gated holds when the test's own act reaches it.
func (r *gatePublishRig) seedGates(issue string, pairs ...record.DesignGateDocument) {
	r.t.Helper()
	approved := 1
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		for _, pair := range pairs {
			if err := r.records.PutGate(ctx, tx, record.DesignGate{Issue: issue, Step: pair.Step,
				ArtifactID: pair.ArtifactID, LatestVersion: 1, ApprovedVersion: &approved}); err != nil {
				return err
			}
		}
		return nil
	})
}

// apply applies fact through the workflow engine and then admission, as intake does for every
// fact the daemon takes.
func (r *gatePublishRig) apply(id string, fact intake.Fact) {
	r.t.Helper()
	result, err := intake.ApplyFact(r.ctx, r.pool, "test", id, fact, r.engine, r.admission)
	if err != nil {
		r.t.Fatalf("apply %s: %v", id, err)
	}
	if result.Refusal != nil {
		r.t.Fatalf("apply %s was refused: %+v", id, result.Refusal)
	}
}

// published runs the board lane over the project's design-gate publications alone and answers what
// Dispatch was told since the last call. The lane is the real one (outbox.drain), so a row it
// publishes is finished and a set it never queued is a set nobody is told.
func (r *gatePublishRig) published() []gatePublished {
	r.t.Helper()
	if drained, _ := r.runner.drain(r.ctx, "", []record.OutboxKind{record.OutboxKindDesignGatesPublish}); !drained {
		r.t.Fatal("the design-gates lane did not drain")
	}
	return r.client.taken()
}

// gatesOf is the pairs issue's gate rows record now, in step order: what a publication of that
// issue carries (record.GateDocuments).
func (r *gatePublishRig) gatesOf(issue string) []record.DesignGateDocument {
	r.t.Helper()
	var pairs []record.DesignGateDocument
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		gates, err := r.records.GatesOf(ctx, tx, issue)
		if err != nil {
			return err
		}
		pairs = record.GateDocuments(gates)
		return nil
	})
	return pairs
}

// gateOf is the gate row of issue's step, which must be there.
func (r *gatePublishRig) gateOf(issue string, step record.DesignStep) record.DesignGate {
	r.t.Helper()
	var gate record.DesignGate
	var found bool
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		gates, err := r.records.GatesOf(ctx, tx, issue)
		if err != nil {
			return err
		}
		gate, found = gates[step]
		return nil
	})
	if !found {
		r.t.Fatalf("%s records no %s gate", issue, step)
	}
	return gate
}

// wantPublished compares what Dispatch was told with want, in order.
func (r *gatePublishRig) wantPublished(what string, got []gatePublished, want ...gatePublished) {
	r.t.Helper()
	if len(want) == 0 {
		if len(got) != 0 {
			r.t.Fatalf("%s published %+v, want nothing", what, got)
		}
		return
	}
	if !reflect.DeepEqual(got, want) {
		r.t.Fatalf("%s published %+v, want %+v", what, got, want)
	}
}

// wire is a record's step-to-document pairs as Dispatch's route takes them.
func wire(pairs ...record.DesignGateDocument) []dispatch.DesignGateDocument {
	sent := make([]dispatch.DesignGateDocument, 0, len(pairs))
	for _, pair := range pairs {
		sent = append(sent, dispatch.DesignGateDocument{Step: string(pair.Step), ArtifactID: pair.ArtifactID})
	}
	return sent
}

// The document each of an issue's design-gate steps records reaches Dispatch because the write
// that moved those pairs queued the publication, whichever path made it
// (record.PublishChangedGates). Three paths are driven here, and each is the real one:
//
//  1. an architect's registration, which is the path that binds a step to a document and so the
//     one that most often moves the pairs. The publication carries the issue's whole set as the
//     write left it, not the one row the registration touched, so a tree gated on two steps is
//     published with both once its architect has registered both;
//  2. a person's review and a new version of a registered document, which move a version and a
//     review mark and leave every step on the document it named. Every review and every version
//     of every gated tree reaches the same helper, and each would otherwise cost Dispatch a write
//     that says what it already holds, so these publish nothing;
//  3. the carry of a finished root's gate onto a child admitted as a root of its own (admit's
//     readmit). It is the one path that gives an issue a gate without its architect registering
//     anything, so what Dispatch learns of the child's documents, it learns here or nowhere.
func TestGateChangeQueuesItsDispatchPublication(t *testing.T) {
	const root, criteria, spec = "LEGION-100", "artifact-criteria", "artifact-spec"
	r := newGatePublishRig(t, 2)
	r.seedRoot(root, phase.Admitted, "A")

	// 1. The architect registers the criteria document of its root's gate.
	r.apply("register-criteria", intake.GateRegistered{Issue: root, Step: record.DesignStepCriteria,
		ArtifactID: criteria, Version: 1, DispatchVersion: 1})
	criteriaPair := record.DesignGateDocument{Step: record.DesignStepCriteria, ArtifactID: criteria}
	r.wantPublished("the registration of the criteria document", r.published(),
		gatePublished{Issue: root, Gates: wire(criteriaPair)})

	// It then registers the spec, and the publication carries both pairs: the set the record holds
	// after the write, not the row the write touched.
	r.apply("register-spec", intake.GateRegistered{Issue: root, Step: record.DesignStepSpec,
		ArtifactID: spec, Version: 1, DispatchVersion: 1})
	specPair := record.DesignGateDocument{Step: record.DesignStepSpec, ArtifactID: spec}
	both := []record.DesignGateDocument{criteriaPair, specPair}
	if got := r.gatesOf(root); !reflect.DeepEqual(got, both) {
		t.Fatalf("the gates of %s record %+v, want %+v", root, got, both)
	}
	r.wantPublished("the registration of the spec", r.published(), gatePublished{Issue: root, Gates: wire(both...)})

	// 2. A new version of the registered spec, and then a person's approval of it. Each moves the
	// gate — the first its latest version, the second its approval mark — and neither moves a pair.
	r.apply("spec-version-2", intake.DispatchArtifact{Key: root, ArtifactID: spec,
		Kind: intake.DispatchArtifactVersion, Version: 2, Mark: record.EventMark{Seq: 7}})
	if gate := r.gateOf(root, record.DesignStepSpec); gate.LatestVersion != 2 {
		t.Fatalf("the spec gate of %s stands at version %d after its version event, want 2: the write this row is about never happened",
			root, gate.LatestVersion)
	}
	r.wantPublished("a new version of the registered spec", r.published())

	r.apply("spec-approved-2", intake.DispatchArtifact{Key: root, ArtifactID: spec,
		Kind: intake.DispatchArtifactApproved, Version: 2, Mark: record.EventMark{Seq: 8},
		Actor: dispatch.Actor{Kind: "user", ID: "alice"}})
	gate := r.gateOf(root, record.DesignStepSpec)
	if gate.ApprovedVersion == nil || *gate.ApprovedVersion != 2 {
		t.Fatalf("the spec gate of %s records %v approved after the person's approval, want version 2: the write this row is about never happened",
			root, gate.ApprovedVersion)
	}
	if got := r.gatesOf(root); !reflect.DeepEqual(got, both) {
		t.Fatalf("the gates of %s record %+v after a version and a review, want the pairs unmoved at %+v", root, got, both)
	}
	r.wantPublished("a person's approval of the registered spec", r.published())

	// 3. A finished root that lingers, gated on both its steps, and a child of it mid-run. The
	// person hands the child to Legion while the tree is done, so admission admits it as a root of
	// its own and carries the old root's rows onto it.
	const old, child, oldCriteria, oldSpec = "LEGION-200", "LEGION-201", "artifact-old-criteria", "artifact-old-spec"
	until := r.clock().Add(time.Hour)
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return r.records.PutIssue(ctx, tx, record.Issue{Key: old, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Project: gatePublishProject, Title: old, Tree: old, Phase: phase.Done,
			Generation: 1, Status: "done", DispatchStatus: "done", Rank: "B", HandedOver: true, LingerUntil: &until,
			LastDispatchSeq: 1})
	})
	carried := []record.DesignGateDocument{
		{Step: record.DesignStepCriteria, ArtifactID: oldCriteria},
		{Step: record.DesignStepSpec, ArtifactID: oldSpec},
	}
	r.seedGates(old, carried...)
	r.seedMember(child, old, old, phase.Implementing, "in_progress", "C", 2)
	r.wantPublished("the seeded records of the finished tree", r.published())

	r.apply("child-handed-over", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 3,
		Type: "issue.updated", Status: "todo", Title: child, Parent: old, Rank: "C", HandedOver: true})
	admitted := readIssue(t, r.pool, child)
	if admitted == nil || admitted.Tree != child {
		t.Fatalf("%s = %+v after its hand-over, want a root of its own: the carry this row is about never happened", child, admitted)
	}
	if got := r.gatesOf(child); !reflect.DeepEqual(got, carried) {
		t.Fatalf("the gates carried to %s = %+v, want the old root's %+v", child, got, carried)
	}
	r.wantPublished("the carry of the old root's gate onto its child", r.published(),
		gatePublished{Issue: child, Gates: wire(carried...)})
}

// Every path that empties an issue's design gates queues one publication of the empty set for each
// issue it emptied (workflow's clearGates): those rows recorded a document until the delete,
// and Dispatch holds the set they recorded until something tells it otherwise. The delete names
// the issues it emptied rather than the clear naming every issue it covers, so an issue of the
// tree that held no gate row costs Dispatch no write.
//
// The three deletes are the three the daemon makes, each driven here through the fact that makes
// it: an adoption, which moves a queued root and its whole tree into a live tree and clears each
// member's generation (workflow.Engine.Adopt); a re-admission, which starts a new generation of
// the root's whole tree (workflow.Engine.ClearTreeGeneration); and a child set back to todo under
// a live tree, which clears its own run alone (workflow.Engine.ReenterChild). Each tree holds
// more than one member, so a clear that published the issue it was keyed by rather than the
// issues it emptied would be caught.
func TestClearedGenerationPublishesTheEmptySetPerIssue(t *testing.T) {
	empty := []dispatch.DesignGateDocument{}
	criteria := record.DesignGateDocument{Step: record.DesignStepCriteria, ArtifactID: "artifact-criteria"}
	spec := record.DesignGateDocument{Step: record.DesignStepSpec, ArtifactID: "artifact-spec"}

	t.Run("an adoption clears every member of the tree it takes in", func(t *testing.T) {
		const live, queued, member, ungated = "LEGION-100", "LEGION-200", "LEGION-201", "LEGION-202"
		r := newGatePublishRig(t, 3)
		r.seedRoot(live, phase.Implementing, "A")
		r.seedGates(live, spec)
		// The queued root never ran, so it is adoptable: its own tree, waiting in the line, with
		// two members under it and gates on two of the three.
		r.inTx(func(ctx context.Context, tx pgx.Tx) error {
			return r.records.PutIssue(ctx, tx, record.Issue{Key: queued, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Project: gatePublishProject, Title: queued, Tree: queued,
				Parent: record.ParentOf(live), Phase: phase.Admitted, Generation: 1, Status: "todo",
				DispatchStatus: "todo", Rank: "B", HandedOver: true, LastDispatchSeq: 2})
		})
		r.seedGates(queued, criteria, spec)
		r.seedMember(member, queued, queued, phase.Done, "backlog", "C", 2)
		r.seedGates(member, spec)
		r.seedMember(ungated, queued, queued, phase.Done, "backlog", "D", 2)
		r.wantPublished("the seeded records", r.published())

		r.apply("tick", intake.ControllerTick{})
		if got := readIssue(t, r.pool, queued); got == nil || got.Tree != live {
			t.Fatalf("%s = %+v after the tick, want it a member of the tree of %s: the adoption this row is about never happened", queued, got, live)
		}
		// The queued root and the member that held a row are published empty, once each; the
		// member that held none, and the live tree's own root, are not published at all.
		r.wantPublished("the adoption", r.published(),
			gatePublished{Issue: queued, Gates: empty}, gatePublished{Issue: member, Gates: empty})
	})

	t.Run("a re-admission clears every member of the tree that starts again", func(t *testing.T) {
		const root, member, ungated = "LEGION-300", "LEGION-301", "LEGION-302"
		r := newGatePublishRig(t, 3)
		until := r.clock().Add(time.Hour)
		r.inTx(func(ctx context.Context, tx pgx.Tx) error {
			return r.records.PutIssue(ctx, tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Project: gatePublishProject, Title: root, Tree: root, Phase: phase.Done,
				Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A", HandedOver: true,
				LingerUntil: &until, LastDispatchSeq: 1})
		})
		r.seedGates(root, criteria, spec)
		r.seedMember(member, root, root, phase.Done, "done", "B", 1)
		r.seedGates(member, criteria)
		r.seedMember(ungated, root, root, phase.Done, "done", "C", 1)
		r.wantPublished("the seeded records", r.published())

		r.apply("root-todo", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 4,
			Type: "issue.updated", Status: "todo", Title: root, Rank: "A", HandedOver: true})
		if got := readIssue(t, r.pool, root); got == nil || got.Generation != 2 {
			t.Fatalf("%s = %+v after its todo, want generation 2: the re-admission this row is about never happened", root, got)
		}
		r.wantPublished("the re-admission", r.published(),
			gatePublished{Issue: root, Gates: empty}, gatePublished{Issue: member, Gates: empty})
	})

	t.Run("a child re-entering its phase clears its own", func(t *testing.T) {
		const root, child, sibling = "LEGION-400", "LEGION-401", "LEGION-402"
		r := newGatePublishRig(t, 3)
		r.seedRoot(root, phase.Implementing, "A")
		r.seedGates(root, criteria, spec)
		r.seedMember(child, root, root, phase.Done, "backlog", "B", 2)
		r.seedGates(child, criteria, spec)
		r.seedMember(sibling, root, root, phase.Done, "backlog", "C", 2)
		r.seedGates(sibling, spec)
		r.wantPublished("the seeded records", r.published())

		r.apply("child-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 3,
			Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true})
		got := readIssue(t, r.pool, child)
		if got == nil || got.Generation != 2 || got.Tree != root {
			t.Fatalf("%s = %+v after its todo, want generation 2 in the tree of %s: the re-entry this row is about never happened", child, got, root)
		}
		// Its own run alone: the tree's root and the sibling that stayed parked keep their rows and
		// are told nothing.
		r.wantPublished("the child's re-entry", r.published(), gatePublished{Issue: child, Gates: empty})
		if pairs := r.gatesOf(root); len(pairs) != 2 {
			t.Fatalf("the gates of %s = %+v after its child re-entered, want both rows kept", root, pairs)
		}
		if pairs := r.gatesOf(sibling); len(pairs) != 1 {
			t.Fatalf("the gates of %s = %+v after its sibling re-entered, want its row kept", sibling, pairs)
		}
	})
}

// The executor writes the issue's whole set onto the issue's Dispatch row, and what becomes of the
// row is decided by Dispatch's answer alone (the outbox's publishDesignGates, permanentRefusal).
//
// Only the route's own 404 ISSUE_NOT_FOUND ends the row: an issue Dispatch does not know is one it
// will not know on the retry either. Every other answer is an outage the row rides out, and the
// 404 a Dispatch whose release has no design-gates route answers is the one that matters most:
// its router writes that refusal under the code NOT_FOUND, so a rule reading the status alone, or
// reading every coded 4xx as final, would drop every publication of a deployment whose daemon came
// up first, which is exactly what the retry lets happen in either order.
func TestGatePublicationExecutorSendsTheSetAndRetriesARefusal(t *testing.T) {
	const issue = "LEGION-208"
	r := newGatePublishRig(t, 1)
	set := []record.DesignGateDocument{
		{Step: record.DesignStepCriteria, ArtifactID: "artifact-criteria"},
		{Step: record.DesignStepSpec, ArtifactID: "artifact-spec"},
	}
	r.client.answers = []error{
		fmt.Errorf("publish the design gates of %s: %w", issue, errors.New("Dispatch is down")),
		&dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "no route for PUT /api/v1/issues/" + issue + "/design-gates"},
		&dispatch.Error{Status: http.StatusNotFound, Message: "<html>404 Not Found</html>"},
		&dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound, Message: "issue not found"},
	}
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return r.records.EnqueueDesignGates(ctx, tx, record.DesignGatesPublish{Issue: issue, Gates: set}, r.clock())
	})

	sent := gatePublished{Issue: issue, Gates: wire(set...)}
	for _, step := range []struct {
		what  string
		rows  int
		after time.Duration
	}{
		{what: "a Dispatch that is down", rows: 1},
		{what: "the router's 404 NOT_FOUND of a Dispatch with no such route", rows: 1, after: time.Minute},
		{what: "a 404 with no code at all, from whatever stands in front of Dispatch", rows: 1, after: time.Minute},
		{what: "the 404 that names the issue as unknown", rows: 0, after: time.Minute},
	} {
		r.ahead += step.after
		r.wantPublished(step.what, r.published(), sent)
		if rows := outboxRows(t, r.pool); rows != step.rows {
			t.Fatalf("after %s the outbox holds %d rows, want %d", step.what, rows, step.rows)
		}
	}
	if answers := len(r.client.answers); answers != 0 {
		t.Fatalf("%d of the answers were never given, so the row never met them", answers)
	}

	// The ended row is ended for its refusal, not for having run: a publication queued after it
	// runs and is taken.
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return r.records.EnqueueDesignGates(ctx, tx, record.DesignGatesPublish{Issue: issue, Gates: set}, r.clock())
	})
	r.wantPublished("a publication queued after the refusal", r.published(), sent)
	if rows := outboxRows(t, r.pool); rows != 0 {
		t.Fatalf("the outbox holds %d rows after the publication Dispatch took, want none", rows)
	}
}
