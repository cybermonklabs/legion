package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The root whose spec step rests on, or waits on, the ancestor's primary document in these tests
// (dispatch://INFRA-3320, R1 and R3), and the two documents.
const (
	basisSeedRoot     = "LEGION-208"
	basisSeedRootDoc  = "artifact-208"
	basisSeedAncestor = "LEGION-100"
	basisSeedDoc      = "artifact-100"
)

// basisSeedPerson is the person who reviews the ancestor's document, and basisSeedApprovedAt
// Dispatch's time of their approval of its version 3.
var (
	basisSeedPerson     = dispatch.Actor{Kind: "user", ID: "alice"}
	basisSeedApprovedAt = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
)

// basisDispatch is outboxDispatch with each document's own approval and reviews, by document id.
type basisDispatch struct {
	outboxDispatch
	approvals map[string]dispatch.Approval
	reviews   map[string][]dispatch.Review
}

func (d *basisDispatch) Approval(_ context.Context, artifactID string) (dispatch.Approval, error) {
	return d.approvals[artifactID], nil
}

func (d *basisDispatch) Reviews(_ context.Context, artifactID string) ([]dispatch.Review, error) {
	return d.reviews[artifactID], nil
}

// basisSeedRig is the outbox of one daemon over a store holding the root, admitted in its own
// running tree, and the ancestor, the root of a running tree of its own, with the workflow engine
// applying the facts the seeds read.
type basisSeedRig struct {
	t       *testing.T
	pool    *store.Pool
	records record.Store
	client  *basisDispatch
	runner  *outbox
}

func newBasisSeedRig(t *testing.T) *basisSeedRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	client := &basisDispatch{approvals: map[string]dispatch.Approval{}, reviews: map[string][]dispatch.Review{}}
	rig := &basisSeedRig{t: t, pool: pool, records: records, client: client,
		runner: &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, handlers: []intake.Handler{engine}, now: time.Now}}
	for _, issue := range []record.Issue{
		{Key: basisSeedRoot, Tree: basisSeedRoot, Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "U", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
		{Key: basisSeedAncestor, Tree: basisSeedAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Planning, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "T", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
	} {
		putOutboxIssue(t, pool, records, issue)
		openTree(t, pool, "LEGION", issue.Key)
	}
	// The root's own document stands at the version registered, with no review: its read
	// returns there.
	client.approvals[basisSeedRootDoc] = dispatch.Approval{IssueKey: basisSeedRoot, State: "draft", LatestVersion: 4, Primary: true}
	return rig
}

// basisSeedBasis is the ancestor's approval of version of its document, or its request ask-100
// for one while pending, as the registration recorded it.
func basisSeedBasis(version int) record.GateBasis {
	return record.GateBasis{Issue: basisSeedAncestor, ArtifactID: basisSeedDoc, Version: version,
		Approval: record.EventMark{Seq: 40, At: basisSeedApprovedAt}, Approver: basisSeedPerson.Name(),
		Serves: []string{"dispatch://LEGION-100/spec#b-r1@v3"}, Reason: "LEGION-208 delivers r1 of LEGION-100.", Session: "session-architect",
		RegisteredAt: basisSeedApprovedAt.Add(time.Hour)}
}

// openOnBasis writes key's spec step open at version 4 of its own document on the ancestor's
// approval of version 3.
func (r *basisSeedRig) openOnBasis(key, document string) {
	r.t.Helper()
	r.putGate(record.OpenOnBasis(record.DesignGate{Issue: key, ArtifactID: document, LatestVersion: 4}, basisSeedBasis(3)))
}

// pendingOnBasis writes key's spec step closed at version 4, waiting on the ancestor's request
// ask-100 for a person's approval of version 4.
func (r *basisSeedRig) pendingOnBasis(key, document string) {
	r.t.Helper()
	r.putGate(record.PendOnBasis(record.DesignGate{Issue: key, ArtifactID: document, LatestVersion: 4}, basisSeedBasis(4), "ask-100"))
}

func (r *basisSeedRig) putGate(gate record.DesignGate) {
	r.t.Helper()
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		return r.records.PutGate(context.Background(), tx, gate)
	}); err != nil {
		r.t.Fatalf("put the gate of %s: %v", gate.Issue, err)
	}
}

// gate is the root's spec step as the store holds it.
func (r *basisSeedRig) gate() record.DesignGate {
	r.t.Helper()
	var gate *record.DesignGate
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		gates, err := r.records.GatesOf(context.Background(), tx, basisSeedRoot)
		gate = record.SpecGate(gates)
		return err
	}); err != nil || gate == nil {
		r.t.Fatalf("read the gate of %s: %#v, %v", basisSeedRoot, gate, err)
	}
	return *gate
}

// notices is every notice of kind the outbox holds keyed by issue, in order.
func (r *basisSeedRig) notices(issue string, kind record.NoticeKind) []record.Notice {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `select payload from outbox where kind = 'notice' and issue = $1
		and payload->>'kind' = $2 order by id`, issue, string(kind))
	if err != nil {
		r.t.Fatalf("read the %s notices of %s: %v", kind, issue, err)
	}
	defer rows.Close()
	var notices []record.Notice
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			r.t.Fatalf("scan a %s notice of %s: %v", kind, issue, err)
		}
		var notice record.Notice
		if err := json.Unmarshal(payload, &notice); err != nil {
			r.t.Fatalf("decode a %s notice of %s: %v", kind, issue, err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("iterate the %s notices of %s: %v", kind, issue, err)
	}
	return notices
}

func (r *basisSeedRig) execute(payload record.OutboxPayload) {
	r.t.Helper()
	if err := r.runner.execute(context.Background(), mustOutboxRow(r.t, basisSeedRoot, payload, time.Now())); err != nil {
		r.t.Fatalf("execute %T: %v", payload, err)
	}
}

// basisSeedPending is the pending-basis seed a registration of the root's pending step queues.
func basisSeedPending(generation uint64) record.PendingBasisSeed {
	return record.PendingBasisSeed{Step: record.DesignStepSpec, ArtifactID: basisSeedRootDoc, BasisArtifactID: basisSeedDoc, BasisVersion: 4,
		Ask: "ask-100", Generation: generation}
}

// The pending-basis seed reads the ancestor's approval and reviews at the version the step waits
// on, and applies what it finds as the events it missed would have: the person's approval opens
// the step; their approval and then a changes request on that version opens it and passes the
// changes request on to both architects; a changes request, a newer version or the request closed
// with no approval ends the wait; a request still waiting changes nothing. Each changes request
// names the reason the review list gives it, also where a newer version leaves the document's
// approval showing none. A read for another generation changes nothing, and the same read again
// is told nothing new.
func TestThePendingBasisSeedOpensOrEndsTheWaitAsDispatchShowsIt(t *testing.T) {
	approvedAt, sentBackAt := basisSeedApprovedAt.Add(24*time.Hour), basisSeedApprovedAt.Add(25*time.Hour)
	approved := dispatch.Review{Version: 4, State: "approved", Kind: "person", Actor: basisSeedPerson, At: approvedAt, AskID: "ask-100"}
	sentBack := dispatch.Review{Version: 4, State: "changes_requested", Kind: "person", Actor: basisSeedPerson, At: sentBackAt, Reason: "name the rollback path"}
	reason := "name the rollback path"
	changesEnded := `user:alice requested changes on version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec): "name the rollback path".`
	for _, tc := range []struct {
		name       string
		approval   dispatch.Approval
		reviews    []dispatch.Review
		generation uint64
		open       bool
		pending    bool
		ended      string
		asked      int
	}{
		{name: "approved by the person", approval: dispatch.Approval{State: "approved", LatestVersion: 4, Version: new(4), At: approvedAt, Seq: 60, By: basisSeedPerson},
			reviews: []dispatch.Review{approved}, open: true},
		{name: "approved and then sent back", approval: dispatch.Approval{State: "changes_requested", LatestVersion: 4, Version: new(4), Reason: &reason,
			At: sentBackAt, Seq: 61, By: basisSeedPerson}, reviews: []dispatch.Review{sentBack, approved}, open: true, asked: 1},
		{name: "approved, sent back, and then a newer version", approval: dispatch.Approval{State: "draft", LatestVersion: 5},
			reviews: []dispatch.Review{sentBack, approved}, open: true, asked: 1},
		{name: "sent back", approval: dispatch.Approval{State: "changes_requested", LatestVersion: 4, Version: new(4), Reason: &reason, At: sentBackAt, Seq: 61,
			By: basisSeedPerson}, reviews: []dispatch.Review{sentBack}, ended: changesEnded},
		{name: "sent back, and then a newer version", approval: dispatch.Approval{State: "draft", LatestVersion: 5}, reviews: []dispatch.Review{sentBack}, ended: changesEnded},
		{name: "a newer version", approval: dispatch.Approval{State: "draft", LatestVersion: 5}, ended: "Version 5 of the primary document of LEGION-100"},
		{name: "the request closed with no approval", approval: dispatch.Approval{State: "draft", LatestVersion: 4},
			ended: "Dispatch shows approval request ask-100 of version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec) closed with no approval"},
		{name: "still waiting", approval: dispatch.Approval{State: "awaiting", LatestVersion: 4, AskID: "ask-100", WaitingOn: "human"}, pending: true},
		{name: "another generation", approval: dispatch.Approval{State: "approved", LatestVersion: 4, Version: new(4), At: approvedAt, Seq: 60, By: basisSeedPerson},
			reviews: []dispatch.Review{approved}, generation: 2, pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newBasisSeedRig(t)
			rig.pendingOnBasis(basisSeedRoot, basisSeedRootDoc)
			rig.client.approvals[basisSeedDoc], rig.client.reviews[basisSeedDoc] = tc.approval, tc.reviews
			generation := max(tc.generation, 1)

			rig.execute(basisSeedPending(generation))
			rig.execute(basisSeedPending(generation))

			gate := rig.gate()
			if record.GateOpen(gate) != tc.open || gate.OnBasis() != tc.open || gate.PendingOnBasis() != tc.pending {
				t.Fatalf("the spec step = %#v, want open=%t pending=%t", gate, tc.open, tc.pending)
			}
			if tc.open && gate.Basis.Approval != (record.EventMark{Seq: 60, At: approvedAt}) && gate.Basis.Approval != (record.EventMark{At: approvedAt}) {
				t.Fatalf("the basis = %#v, want it on the person's approval of version 4 at %s", gate.Basis, approvedAt)
			}
			if opened := rig.notices(basisSeedRoot, "design-approved"); (len(opened) == 1) != tc.open || len(opened) > 1 {
				t.Fatalf("design-approved notices = %#v, want one for an opening alone", opened)
			}
			ended := rig.notices(basisSeedRoot, "basis-ended")
			if (tc.ended == "") != (len(ended) == 0) || len(ended) > 1 || tc.ended != "" && !strings.Contains(ended[0].Reason, tc.ended) {
				t.Fatalf("basis-ended notices = %#v, want one naming %q, or none", ended, tc.ended)
			}
			root, ancestor := rig.notices(basisSeedRoot, "basis-changes-requested"), rig.notices(basisSeedAncestor, "basis-changes-requested")
			if len(root) != tc.asked || len(ancestor) != tc.asked ||
				tc.asked == 1 && (!strings.Contains(root[0].Reason, `"name the rollback path"`) || !strings.Contains(ancestor[0].Reason, `"name the rollback path"`)) {
				t.Fatalf("basis-changes-requested notices = %#v to the root and %#v to the ancestor, want %d each naming the reason", root, ancestor, tc.asked)
			}
		})
	}
}

// The gate seed of a step an ancestor's approval opened keeps today's read of the root's own
// document, which returns at the version registered, and reads the ancestor's reviews of the
// version the basis names as well: each person's changes request on it, given after the approval
// the step opened on, reaches the root's architect and the ancestor's once, however many seeds
// read it, naming the reason the review list gives it, and the step stays open. A changes request
// given before that approval passes nothing on. A newer version of the root's own document closes
// the step as ever, and the closed step reads no ancestor.
func TestTheGateSeedOfABasisStepPassesOnTheAncestorsChangesRequestOnce(t *testing.T) {
	approved := dispatch.Review{Version: 3, State: "approved", Kind: "person", Actor: basisSeedPerson, At: basisSeedApprovedAt}
	reason := "name the rollback path"
	asked := dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3), Reason: &reason, At: basisSeedApprovedAt.Add(time.Hour), Seq: 45, By: basisSeedPerson}
	firstAsked := dispatch.Review{Version: 3, State: "changes_requested", Kind: "person", Actor: basisSeedPerson, At: asked.At, Reason: reason}
	seed := record.GateSeed{ArtifactID: basisSeedRootDoc, Step: record.DesignStepSpec, Version: 4, Generation: 1}

	t.Run("two changes requests after the approval", func(t *testing.T) {
		rig := newBasisSeedRig(t)
		rig.openOnBasis(basisSeedRoot, basisSeedRootDoc)
		// The document's approval shows the newest review alone, so the first one's reason is the
		// review list's to give.
		restore := "and the restore path"
		rig.client.approvals[basisSeedDoc] = dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3), Reason: &restore,
			At: basisSeedApprovedAt.Add(2 * time.Hour), Seq: 47, By: basisSeedPerson}
		rig.client.reviews[basisSeedDoc] = []dispatch.Review{{Version: 3, State: "changes_requested", Kind: "person", Actor: basisSeedPerson,
			At: basisSeedApprovedAt.Add(2 * time.Hour), Reason: restore}, firstAsked, approved}

		rig.execute(seed)
		rig.execute(seed)

		if gate := rig.gate(); !gate.OnBasis() || !record.GateOpen(gate) || gate.Basis.Version != 3 {
			t.Fatalf("the spec step = %#v, want it open on version 3 of the ancestor's document", gate)
		}
		root, ancestor := rig.notices(basisSeedRoot, "basis-changes-requested"), rig.notices(basisSeedAncestor, "basis-changes-requested")
		if len(root) != 2 || len(ancestor) != 2 {
			t.Fatalf("basis-changes-requested notices = %#v to the root and %#v to the ancestor, want two each", root, ancestor)
		}
		for i, want := range []string{`"name the rollback path"`, `"and the restore path"`} {
			if root[i].Ref != "dispatch://LEGION-100/spec" || root[i].Version != 3 || root[i].Actor != "user:alice" ||
				!strings.Contains(root[i].Reason, want) || !strings.Contains(root[i].Reason, "The step stays open.") {
				t.Fatalf("the root's notice %d = %#v, want it naming version 3 of LEGION-100's document, the person and the reason %s", i, root[i], want)
			}
			if !strings.Contains(ancestor[i].Reason, "The step of LEGION-208 stays open.") || !strings.Contains(ancestor[i].Reason, want) {
				t.Fatalf("the ancestor's notice %d = %#v, want it naming the root and the reason %s", i, ancestor[i], want)
			}
		}
		for _, kind := range []record.NoticeKind{"design-changes-requested", "basis-ended"} {
			if got := rig.notices(basisSeedRoot, kind); len(got) != 0 {
				t.Fatalf("%s notices = %#v, want none", kind, got)
			}
		}
	})

	t.Run("a changes request before the approval", func(t *testing.T) {
		rig := newBasisSeedRig(t)
		rig.openOnBasis(basisSeedRoot, basisSeedRootDoc)
		rig.client.approvals[basisSeedDoc] = dispatch.Approval{State: "approved", LatestVersion: 3, Version: new(3), At: basisSeedApprovedAt, Seq: 40, By: basisSeedPerson}
		rig.client.reviews[basisSeedDoc] = []dispatch.Review{approved, {Version: 3, State: "changes_requested", Kind: "person", Actor: basisSeedPerson,
			At: basisSeedApprovedAt.Add(-time.Hour)}}

		rig.execute(seed)

		if gate := rig.gate(); !gate.OnBasis() || !record.GateOpen(gate) {
			t.Fatalf("the spec step = %#v, want it open on the basis", gate)
		}
		if got := rig.notices(basisSeedRoot, "basis-changes-requested"); len(got) != 0 {
			t.Fatalf("basis-changes-requested notices = %#v, want none for a review the approval followed", got)
		}
	})

	t.Run("a newer version of the root's own document", func(t *testing.T) {
		rig := newBasisSeedRig(t)
		rig.openOnBasis(basisSeedRoot, basisSeedRootDoc)
		rig.client.approvals[basisSeedRootDoc] = dispatch.Approval{IssueKey: basisSeedRoot, State: "draft", LatestVersion: 5, Primary: true}
		rig.client.approvals[basisSeedDoc] = asked
		rig.client.reviews[basisSeedDoc] = []dispatch.Review{firstAsked, approved}

		rig.execute(seed)

		if gate := rig.gate(); gate.Basis != nil || record.GateOpen(gate) || gate.LatestVersion != 5 {
			t.Fatalf("the spec step = %#v, want it closed at version 5 with no basis", gate)
		}
		if got := rig.notices(basisSeedRoot, "basis-changes-requested"); len(got) != 0 {
			t.Fatalf("basis-changes-requested notices = %#v, want none: the closed step reads no ancestor", got)
		}
	})
}

// At boot the daemon queues one pending-basis seed for each step of the project that waits on an
// ancestor's request, whatever phase its root stands in, and none for a step open on a basis, for
// an issue Dispatch shows done, or for another project's issue. The seed it queued then opens the
// step on the person's approval the daemon missed, and the boot left the open step as it was.
func TestTheBootQueuesAPendingBasisSeedForEachPendingStep(t *testing.T) {
	rig := newBasisSeedRig(t)
	const (
		open         = "LEGION-209"
		done         = "LEGION-210"
		otherProject = "OTHER-7"
	)
	for _, issue := range []record.Issue{
		{Key: open, Tree: open, Project: "LEGION", Title: "open", Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: "V", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
		{Key: done, Tree: done, Project: "LEGION", Title: "done", Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "done",
			Rank: "W", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
		{Key: otherProject, Tree: otherProject, Project: "OTHER", Title: "elsewhere", Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: "X", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
	} {
		putOutboxIssue(t, rig.pool, rig.records, issue)
	}
	// The root waits in a phase of its tree, which the boot's catch-up does not read.
	root := *readIssue(t, rig.pool, basisSeedRoot)
	root.Phase = phase.Implementing
	putOutboxIssue(t, rig.pool, rig.records, root)
	rig.pendingOnBasis(basisSeedRoot, basisSeedRootDoc)
	rig.openOnBasis(open, "artifact-209")
	rig.pendingOnBasis(done, "artifact-210")
	rig.pendingOnBasis(otherProject, "artifact-other")

	boot := &workflowRuntime{pool: rig.pool, records: rig.records, dispatchProject: "LEGION", log: quietLogger()}
	if err := boot.queuePendingBasisSeeds(context.Background()); err != nil {
		t.Fatalf("the boot's pass: %v", err)
	}

	rows, err := rig.pool.Query(context.Background(), `select issue, payload from outbox where kind = $1 order by id`, string(record.OutboxKindPendingBasisSeed))
	if err != nil {
		t.Fatalf("read the pending-basis seeds: %v", err)
	}
	type queued struct {
		issue string
		seed  record.PendingBasisSeed
	}
	var seeds []queued
	for rows.Next() {
		var row queued
		var payload []byte
		if err := rows.Scan(&row.issue, &payload); err != nil {
			t.Fatalf("scan a pending-basis seed: %v", err)
		}
		if err := json.Unmarshal(payload, &row.seed); err != nil {
			t.Fatalf("decode a pending-basis seed: %v", err)
		}
		seeds = append(seeds, row)
	}
	rows.Close()
	if want := (queued{issue: basisSeedRoot, seed: basisSeedPending(1)}); len(seeds) != 1 || seeds[0] != want {
		t.Fatalf("pending-basis seeds = %#v, want the pending step's alone: %#v", seeds, want)
	}

	rig.client.approvals[basisSeedDoc] = dispatch.Approval{State: "approved", LatestVersion: 4, Version: new(4), At: basisSeedApprovedAt.Add(24 * time.Hour),
		Seq: 60, By: basisSeedPerson}
	rig.client.reviews[basisSeedDoc] = []dispatch.Review{{Version: 4, State: "approved", Kind: "person", Actor: basisSeedPerson, At: basisSeedApprovedAt.Add(24 * time.Hour)}}
	rig.execute(seeds[0].seed)
	if gate := rig.gate(); !gate.OnBasis() || !record.GateOpen(gate) || gate.Basis.Version != 4 {
		t.Fatalf("the spec step after the boot's seed = %#v, want it open on version 4 of the ancestor's document", gate)
	}
	var stood *record.DesignGate
	if err := pgx.BeginFunc(context.Background(), rig.pool, func(tx pgx.Tx) error {
		gates, err := rig.records.GatesOf(context.Background(), tx, open)
		stood = record.SpecGate(gates)
		return err
	}); err != nil || stood == nil || !stood.OnBasis() || !record.GateOpen(*stood) || stood.Basis.Version != 3 {
		t.Fatalf("the open step after the boot = %#v, %v; want it open on version 3 as it stood", stood, err)
	}
}
