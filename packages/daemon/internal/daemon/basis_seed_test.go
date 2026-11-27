package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
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

// basisDispatch is outboxDispatch with each document's own approval and reviews, by document id,
// the issue each document belongs to, which the approval read names, each issue as Dispatch
// shows it, by key, and each ask as Dispatch reads it by its id.
type basisDispatch struct {
	outboxDispatch
	approvals map[string]dispatch.Approval
	reviews   map[string][]dispatch.Review
	owners    map[string]string
	issues    map[string]dispatch.Issue
	asks      map[string]dispatch.Ask
}

func (d *basisDispatch) Approval(_ context.Context, artifactID string) (dispatch.Approval, error) {
	approval := d.approvals[artifactID]
	approval.IssueKey = cmp.Or(approval.IssueKey, d.owners[artifactID])
	return approval, nil
}

func (d *basisDispatch) Reviews(_ context.Context, artifactID string) ([]dispatch.Review, error) {
	return d.reviews[artifactID], nil
}

func (d *basisDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	issue, ok := d.issues[key]
	if !ok {
		return dispatch.Issue{}, fmt.Errorf("Dispatch has no issue %s", key)
	}
	return issue, nil
}

func (d *basisDispatch) Ask(_ context.Context, id string) (dispatch.Ask, error) {
	ask, ok := d.asks[id]
	if !ok {
		return dispatch.Ask{}, &dispatch.Error{Status: http.StatusNotFound, Code: oauth2calltest.Said("NOT_FOUND"), Message: "ask not found"}
	}
	return ask, nil
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
	client := &basisDispatch{approvals: map[string]dispatch.Approval{}, reviews: map[string][]dispatch.Review{},
		owners: map[string]string{basisSeedDoc: basisSeedAncestor, basisSeedRootDoc: basisSeedRoot},
		issues: map[string]dispatch.Issue{basisSeedAncestor: {Key: basisSeedAncestor, Status: "in_progress", Labels: []string{dispatch.LegionLabel}, HandedOver: true}},
		asks:   map[string]dispatch.Ask{}}
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
	putOutboxGate(r.t, r.pool, r.records, record.OpenOnBasis(record.DesignGate{Issue: key, ArtifactID: document, LatestVersion: 4}, basisSeedBasis(3)))
}

// pendingOnBasis writes key's spec step closed at version 4, waiting on the ancestor's request
// ask-100 for a person's approval of version 4.
func (r *basisSeedRig) pendingOnBasis(key, document string) {
	r.t.Helper()
	putOutboxGate(r.t, r.pool, r.records, record.PendOnBasis(record.DesignGate{Issue: key, ArtifactID: document, LatestVersion: 4}, basisSeedBasis(4), "ask-100"))
}

// gate is the root's spec step as the store holds it.
func (r *basisSeedRig) gate() record.DesignGate {
	r.t.Helper()
	var gate *record.DesignGate
	if err := store.Transact(context.Background(), r.pool, func(_ context.Context, tx pgx.Tx) error {
		gates, err := r.records.GatesOf(context.Background(), tx, basisSeedRoot)
		gate = record.SpecGate(gates)
		return err
	}); err != nil || gate == nil {
		r.t.Fatalf("read the gate of %s: %#v, %v", basisSeedRoot, gate, err)
	}
	return *gate
}

// noticesOfKind is every notice of kind the outbox holds keyed by issue, oldest first, delivered
// or not: the basis tests' rigs read what the daemon told an architect through it.
func noticesOfKind(t *testing.T, pool *store.Pool, issue string, kind record.NoticeKind) []record.Notice {
	t.Helper()
	return noticesIn(t, pool, "outbox", issue, kind)
}

// noticesIn is every notice of kind that table holds keyed by issue, oldest first: the outbox
// itself (noticesOfKind) or its record of every row written (outbox_written, writtenNotices).
func noticesIn(t *testing.T, pool *store.Pool, table, issue string, kind record.NoticeKind) []record.Notice {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from `+table+` where kind = $1 and issue = $2
		and payload->>'kind' = $3 order by id`, string(record.OutboxKindNotice), issue, string(kind))
	if err != nil {
		t.Fatalf("read the %s notices of %s in %s: %v", kind, issue, table, err)
	}
	defer rows.Close()
	var notices []record.Notice
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan a %s notice of %s in %s: %v", kind, issue, table, err)
		}
		var notice record.Notice
		if err := json.Unmarshal(payload, &notice); err != nil {
			t.Fatalf("decode a %s notice of %s in %s: %v", kind, issue, table, err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the %s notices of %s in %s: %v", kind, issue, table, err)
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
		Ask: "ask-100", Generation: generation, RegisteredAt: basisSeedBasis(4).RegisteredAt}
}

// The pending-basis seed reads Dispatch's state of the ancestor's document and of its issue, and
// of the request's close where the document's approval shows the request closed (readBasis), the
// one read that opens or ends a step's wait on the ancestor's request, and the workflow judges it
// by its one verdict: the person's approval of the version, as the document's latest review of its
// latest version, opens the step; a changes request as that latest review ends the wait, also where
// the review list shows the person's approval of the version before it (decision E1: the seed no
// longer opens on that earlier approval and passes the changes request on); a newer version ends
// it whatever the list shows of the version before; and so does the request closed with no
// approval, by whoever Dispatch shows closed it, while a request still waiting changes nothing.
// The person's approval opens no step on an ancestor that Dispatch shows accepted or without the
// legion label: the wait ends instead. A read for another generation changes nothing, and the
// same read again is told nothing new.
func TestThePendingBasisSeedOpensOrEndsTheWaitAsDispatchShowsIt(t *testing.T) {
	approvedAt, sentBackAt := basisSeedApprovedAt.Add(24*time.Hour), basisSeedApprovedAt.Add(25*time.Hour)
	reason := "name the rollback path"
	// The review list of version 4 Dispatch serves, newest first: the person approved it, and then
	// sent it back.
	history := []dispatch.Review{{Version: 4, State: "changes_requested", Actor: basisSeedPerson, At: sentBackAt, Reason: reason},
		{Version: 4, State: "approved", Actor: basisSeedPerson, At: approvedAt}}
	changesEnded := `user:alice requested changes on version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec): "name the rollback path".`
	approvedRead := dispatch.Approval{State: "approved", LatestVersion: 4, Version: new(4), At: approvedAt, Seq: 60, By: basisSeedPerson}
	sentBackRead := dispatch.Approval{State: "changes_requested", LatestVersion: 4, Version: new(4), Reason: &reason, At: sentBackAt, Seq: 61, By: basisSeedPerson}
	for _, tc := range []struct {
		name       string
		approval   dispatch.Approval
		reviews    []dispatch.Review
		ancestor   *dispatch.Issue
		generation uint64
		open       bool
		pending    bool
		ended      string
	}{
		{name: "approved by the person", approval: approvedRead, open: true},
		{name: "approved by the person, with the ancestor done", approval: approvedRead,
			ancestor: &dispatch.Issue{Key: basisSeedAncestor, Status: "done", Labels: []string{dispatch.LegionLabel}, HandedOver: true},
			ended:    "Dispatch shows LEGION-100 done, so LEGION-100 is accepted."},
		{name: "approved by the person, with the ancestor no longer labelled", approval: approvedRead,
			ancestor: &dispatch.Issue{Key: basisSeedAncestor, Status: "in_progress", Labels: []string{"infra"}},
			ended:    "LEGION-100 no longer carries the legion label."},
		{name: "approved and then sent back", approval: sentBackRead, reviews: history, ended: changesEnded},
		{name: "approved, sent back, and then a newer version", approval: dispatch.Approval{State: "draft", LatestVersion: 5}, reviews: history,
			ended: "Version 5 of the primary document of LEGION-100 (dispatch://LEGION-100/spec) is newer than version 4"},
		{name: "the request closed with no approval", approval: dispatch.Approval{State: "draft", LatestVersion: 4},
			ended: "session:session-architect closed approval request ask-100 of version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec), as resolved, with no approval of that version."},
		{name: "still waiting", approval: dispatch.Approval{State: "awaiting", LatestVersion: 4, AskID: "ask-100", WaitingOn: "human"}, pending: true},
		{name: "another generation", approval: approvedRead, generation: 2, pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newBasisSeedRig(t)
			rig.pendingOnBasis(basisSeedRoot, basisSeedRootDoc)
			rig.client.approvals[basisSeedDoc], rig.client.reviews[basisSeedDoc] = tc.approval, tc.reviews
			if tc.ancestor != nil {
				rig.client.issues[basisSeedAncestor] = *tc.ancestor
			}
			// What Dispatch shows of the request, which only a read that finds it closed reads.
			rig.client.asks["ask-100"] = dispatch.Ask{State: "resolved", ClosedBy: dispatch.Actor{Kind: "session", ID: "session-architect"}}
			generation := max(tc.generation, 1)

			rig.execute(basisSeedPending(generation))
			rig.execute(basisSeedPending(generation))

			gate := rig.gate()
			if record.GateOpen(gate) != tc.open || gate.OnBasis() != tc.open || gate.PendingOnBasis() != tc.pending {
				t.Fatalf("the spec step = %#v, want open=%t pending=%t", gate, tc.open, tc.pending)
			}
			if tc.open && gate.Basis.Approval != (record.EventMark{Seq: 60, At: approvedAt}) {
				t.Fatalf("the basis = %#v, want it on the person's approval of version 4 at sequence 60, %s", gate.Basis, approvedAt)
			}
			if opened := noticesOfKind(t, rig.pool, basisSeedRoot, "design-approved"); (len(opened) == 1) != tc.open || len(opened) > 1 {
				t.Fatalf("design-approved notices = %#v, want one for an opening alone", opened)
			}
			ended := noticesOfKind(t, rig.pool, basisSeedRoot, "basis-ended")
			if (tc.ended == "") != (len(ended) == 0) || len(ended) > 1 || tc.ended != "" && !strings.Contains(ended[0].Reason, tc.ended) {
				t.Fatalf("basis-ended notices = %#v, want one naming %q, or none", ended, tc.ended)
			}
			root, ancestor := noticesOfKind(t, rig.pool, basisSeedRoot, "basis-changes-requested"), noticesOfKind(t, rig.pool, basisSeedAncestor, "basis-changes-requested")
			if len(root) != 0 || len(ancestor) != 0 {
				t.Fatalf("basis-changes-requested notices = %#v to the root and %#v to the ancestor, want none: the seed opens no step before a changes request", root, ancestor)
			}
		})
	}
}

// A step that registers again on the request it waited on, once the seed's read ended its first
// wait, waits on a registration of its own, and the seed's read of that wait is a fact of its own:
// the ancestor's legion label removed, given back while the step registers again on the still-open
// request, and removed again with nothing else changed, ends the second wait as it ended the first,
// and the root's architect is told each end. The engine's clock gives each registration a time
// finer than the store keeps, so the seed the registration queues from the step it has not yet
// read back still names the wait the store holds.
func TestTheSameReadEndsAWaitRegisteredAgainOnTheSameRequest(t *testing.T) {
	rig := newBasisSeedRig(t)
	now := basisSeedApprovedAt.Add(time.Hour + 123456789*time.Nanosecond)
	engine := workflow.New(rig.records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Clock: func() time.Time { return now }}, quietLogger())
	rig.runner.handlers = []intake.Handler{engine}
	register := func(id string, since int64) record.PendingBasisSeed {
		t.Helper()
		before := len(pendingBasisSeedsOf(t, rig.pool, basisSeedRoot))
		if _, err := intake.ApplyFact(context.Background(), rig.pool, "test", id, intake.GateRegistered{Issue: basisSeedRoot, ArtifactID: basisSeedRootDoc,
			Version: 4, DispatchVersion: 4, Since: record.EventMark{Seq: since}, Basis: &record.GateBasis{Issue: basisSeedAncestor, ArtifactID: basisSeedDoc,
				Version: 4, Serves: []string{"dispatch://LEGION-100/spec#b-r1@v4"}, Reason: "LEGION-208 delivers r1 of LEGION-100.", Session: "session-architect",
				Pending: &record.PendingBasis{Ask: "ask-100"}}}, engine); err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
		if gate := rig.gate(); !gate.PendingOnBasis() || gate.Basis.Pending.Ask != "ask-100" || record.GateOpen(gate) {
			t.Fatalf("the spec step after %s = %#v, want it closed, waiting on ask-100", id, gate)
		}
		seeds := pendingBasisSeedsOf(t, rig.pool, basisSeedRoot)
		if len(seeds) != before+1 {
			t.Fatalf("pending-basis seeds after %s = %#v, want the %d before it and the registration's", id, seeds, before)
		}
		return seeds[before]
	}
	labelled := func(on bool) {
		issue := dispatch.Issue{Key: basisSeedAncestor, Status: "in_progress", Labels: []string{"infra"}}
		if on {
			issue.Labels = []string{dispatch.LegionLabel}
			issue.HandedOver = true
		}
		rig.client.issues[basisSeedAncestor] = issue
	}
	const unlabelled = "LEGION-100 no longer carries the legion label."
	ends := func(after string, want int) {
		t.Helper()
		if gate := rig.gate(); gate.Basis != nil || record.GateOpen(gate) || gate.LatestVersion != 4 {
			t.Fatalf("the spec step after %s = %#v, want its wait ended: closed at version 4 with no basis", after, gate)
		}
		ended := noticesOfKind(t, rig.pool, basisSeedRoot, "basis-ended")
		if len(ended) != want || !strings.Contains(ended[want-1].Reason, unlabelled) {
			t.Fatalf("basis-ended notices after %s = %#v, want %d, the last naming %q", after, ended, want, unlabelled)
		}
	}
	// Dispatch shows the request open on version 4 throughout.
	rig.client.approvals[basisSeedDoc] = dispatch.Approval{State: "awaiting", LatestVersion: 4, AskID: "ask-100", WaitingOn: "human"}

	first := register("register", 6)
	labelled(false)
	rig.execute(first)
	ends("the first read", 1)

	labelled(true)
	now = now.Add(time.Hour)
	second := register("register-again", 8)
	labelled(false)
	rig.execute(second)
	ends("the same read of the second wait", 2)
}

// The gate seed of a step an ancestor's approval opened keeps today's read of the root's own
// document, which returns at the version registered, and reads the ancestor's reviews of the
// version the basis names as well: each person's changes request on it, given after the approval
// the step opened on, reaches the root's architect and the ancestor's once, however many seeds
// read it, naming the reason the review list gives it, and the step stays open. A changes request
// given before that approval passes nothing on. A newer version of the root's own document closes
// the step as ever, and the closed step reads no ancestor.
func TestTheGateSeedOfABasisStepPassesOnTheAncestorsChangesRequestOnce(t *testing.T) {
	approved := dispatch.Review{Version: 3, State: "approved", Actor: basisSeedPerson, At: basisSeedApprovedAt}
	reason := "name the rollback path"
	asked := dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3), Reason: &reason, At: basisSeedApprovedAt.Add(time.Hour), Seq: 45, By: basisSeedPerson}
	firstAsked := dispatch.Review{Version: 3, State: "changes_requested", Actor: basisSeedPerson, At: asked.At, Reason: reason}
	seed := record.GateSeed{ArtifactID: basisSeedRootDoc, Step: record.DesignStepSpec, Version: 4, Generation: 1}

	t.Run("two changes requests after the approval", func(t *testing.T) {
		rig := newBasisSeedRig(t)
		rig.openOnBasis(basisSeedRoot, basisSeedRootDoc)
		// The document's approval shows the newest review alone, so the first one's reason is the
		// review list's to give.
		restore := "and the restore path"
		rig.client.approvals[basisSeedDoc] = dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3), Reason: &restore,
			At: basisSeedApprovedAt.Add(2 * time.Hour), Seq: 47, By: basisSeedPerson}
		rig.client.reviews[basisSeedDoc] = []dispatch.Review{{Version: 3, State: "changes_requested", Actor: basisSeedPerson,
			At: basisSeedApprovedAt.Add(2 * time.Hour), Reason: restore}, firstAsked, approved}

		rig.execute(seed)
		rig.execute(seed)

		if gate := rig.gate(); !gate.OnBasis() || !record.GateOpen(gate) || gate.Basis.Version != 3 {
			t.Fatalf("the spec step = %#v, want it open on version 3 of the ancestor's document", gate)
		}
		root, ancestor := noticesOfKind(t, rig.pool, basisSeedRoot, "basis-changes-requested"), noticesOfKind(t, rig.pool, basisSeedAncestor, "basis-changes-requested")
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
			if got := noticesOfKind(t, rig.pool, basisSeedRoot, kind); len(got) != 0 {
				t.Fatalf("%s notices = %#v, want none", kind, got)
			}
		}
	})

	t.Run("a changes request before the approval", func(t *testing.T) {
		rig := newBasisSeedRig(t)
		rig.openOnBasis(basisSeedRoot, basisSeedRootDoc)
		rig.client.approvals[basisSeedDoc] = dispatch.Approval{State: "approved", LatestVersion: 3, Version: new(3), At: basisSeedApprovedAt, Seq: 40, By: basisSeedPerson}
		rig.client.reviews[basisSeedDoc] = []dispatch.Review{approved, {Version: 3, State: "changes_requested", Actor: basisSeedPerson,
			At: basisSeedApprovedAt.Add(-time.Hour)}}

		rig.execute(seed)

		if gate := rig.gate(); !gate.OnBasis() || !record.GateOpen(gate) {
			t.Fatalf("the spec step = %#v, want it open on the basis", gate)
		}
		if got := noticesOfKind(t, rig.pool, basisSeedRoot, "basis-changes-requested"); len(got) != 0 {
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
		if got := noticesOfKind(t, rig.pool, basisSeedRoot, "basis-changes-requested"); len(got) != 0 {
			t.Fatalf("basis-changes-requested notices = %#v, want none: the closed step reads no ancestor", got)
		}
	})
}

// At boot the daemon queues one pending-basis seed for each step of the project that waits on an
// ancestor's request, whatever phase its root stands in, in the pass that queues the publication
// of every recorded gate (queueRecordedGatesPublish), and none for a step open on a basis, for an
// issue Dispatch shows done, or for another project's issue. The seed it queued then opens the
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
	if err := boot.queueRecordedGatesPublish(context.Background()); err != nil {
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
	rig.execute(seeds[0].seed)
	if gate := rig.gate(); !gate.OnBasis() || !record.GateOpen(gate) || gate.Basis.Version != 4 {
		t.Fatalf("the spec step after the boot's seed = %#v, want it open on version 4 of the ancestor's document", gate)
	}
	var stood *record.DesignGate
	if err := store.Transact(context.Background(), rig.pool, func(_ context.Context, tx pgx.Tx) error {
		gates, err := rig.records.GatesOf(context.Background(), tx, open)
		stood = record.SpecGate(gates)
		return err
	}); err != nil || stood == nil || !stood.OnBasis() || !record.GateOpen(*stood) || stood.Basis.Version != 3 {
		t.Fatalf("the open step after the boot = %#v, %v; want it open on version 3 as it stood", stood, err)
	}
}

// countedBasisDispatch is basisDispatch counting each document's approval reads, each issue's
// reads and each ask's reads, and answering failure to every read of the document, issue or ask
// failing names.
type countedBasisDispatch struct {
	*basisDispatch
	approvalReads map[string]int
	issueReads    map[string]int
	askReads      map[string]int
	failing       string
	failure       error
}

func newCountedBasisDispatch(client *basisDispatch) *countedBasisDispatch {
	return &countedBasisDispatch{basisDispatch: client, approvalReads: map[string]int{}, issueReads: map[string]int{}, askReads: map[string]int{}}
}

func (d *countedBasisDispatch) Approval(ctx context.Context, artifactID string) (dispatch.Approval, error) {
	d.approvalReads[artifactID]++
	if artifactID == d.failing {
		return dispatch.Approval{}, d.failure
	}
	return d.basisDispatch.Approval(ctx, artifactID)
}

func (d *countedBasisDispatch) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	d.issueReads[key]++
	if key == d.failing {
		return dispatch.Issue{}, d.failure
	}
	return d.basisDispatch.GetIssue(ctx, key)
}

func (d *countedBasisDispatch) Ask(ctx context.Context, id string) (dispatch.Ask, error) {
	d.askReads[id]++
	if id == d.failing {
		return dispatch.Ask{}, d.failure
	}
	return d.basisDispatch.Ask(ctx, id)
}

// cueReadsCounted counts how often the workflow looks up steps by basis document, pending
// request, or ancestor issue while deciding which steps an event cues.
type cueReadsCounted struct {
	record.Store
	reads int
}

func (s *cueReadsCounted) GatesForBasisArtifact(ctx context.Context, tx pgx.Tx, artifactID string) ([]record.DesignGate, error) {
	s.reads++
	return s.Store.GatesForBasisArtifact(ctx, tx, artifactID)
}

func (s *cueReadsCounted) GatesByPendingAsk(ctx context.Context, tx pgx.Tx, ask string) ([]record.DesignGate, error) {
	s.reads++
	return s.Store.GatesByPendingAsk(ctx, tx, ask)
}

func (s *cueReadsCounted) GatesRestingOn(ctx context.Context, tx pgx.Tx, ancestors ...string) ([]record.DesignGate, error) {
	s.reads++
	return s.Store.GatesRestingOn(ctx, tx, ancestors...)
}

// pendingBasisSeedsOf is every pending-basis seed the outbox holds for issue, oldest first.
func pendingBasisSeedsOf(t *testing.T, pool *store.Pool, issue string) []record.PendingBasisSeed {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where kind = $1 and issue = $2 order by id`,
		string(record.OutboxKindPendingBasisSeed), issue)
	if err != nil {
		t.Fatalf("read the pending-basis seeds of %s: %v", issue, err)
	}
	defer rows.Close()
	var seeds []record.PendingBasisSeed
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan a pending-basis seed of %s: %v", issue, err)
		}
		var seed record.PendingBasisSeed
		if err := json.Unmarshal(payload, &seed); err != nil {
			t.Fatalf("decode a pending-basis seed of %s: %v", issue, err)
		}
		seeds = append(seeds, seed)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the pending-basis seeds of %s: %v", issue, err)
	}
	return seeds
}

// The ancestor of TestAnEventReadsDispatchOnlyThroughTheSeedOfEachStepItCues, its primary document
// and its approval request: an issue the daemon does not record, so each of its events reaches the
// steps that wait or rest on it and no record of its own.
const (
	cueAncestor = "LEGION-150"
	cueDoc      = "artifact-150"
	cueAsk      = "ask-150"
)

// An event of an ancestor's document, of its approval request or of its issue leads to a read of
// Dispatch only for a step it is a cue for, and only through that step's pending-basis seed: it
// queues one seed for each step it cues and none for any other, and those seeds are the only reads
// of Dispatch it leads to, of the document and of its issue once each,
// and of the request none, since Dispatch shows it open (the cases that read the request are
// TestThePendingBasisSeedReadsTheCloseOnlyWhereTheApprovalShowsTheRequestClosed's). A document
// event at the basis version or a newer one, other than that version's own version event, a close
// of the request other than a person's answer, and an event that shows the ancestor leaving
// R1's first condition are cues for the root's waiting step, and for no step that stands open on
// the document. An event no step waits on queues no seed and leads to no read of Dispatch: the
// version's own version event, an older version's review, another document's event, a close of
// another request, another issue's leave, and every cue while no step waits. A person's answer of
// the request, a close of a question and an event that shows the ancestor at work cannot cue any
// step, so the workflow does not even look up steps for them.
func TestAnEventReadsDispatchOnlyThroughTheSeedOfEachStepItCues(t *testing.T) {
	const open, openDoc = "LEGION-209", "artifact-209"
	person := dispatch.Actor{Kind: "user", ID: "alice"}
	review := func(kind intake.DispatchArtifactKind, version int) intake.DispatchArtifact {
		fact := intake.DispatchArtifact{Key: cueAncestor, ArtifactID: cueDoc, Kind: kind, Version: version, Mark: record.EventMark{Seq: 60}}
		if kind != intake.DispatchArtifactVersion {
			fact.Actor = person
		}
		return fact
	}
	closed := func(ask, state string) intake.DispatchAskClosed {
		return intake.DispatchAskClosed{Key: cueAncestor, Ask: ask, Kind: record.AskApproval, Artifact: cueDoc, State: state, Actor: person,
			Mark: record.EventMark{Seq: 61}}
	}
	issue := func(key, status string, labelled bool) intake.DispatchIssue {
		return intake.DispatchIssue{Key: key, Seq: 62, Type: "issue.updated", Status: status, Title: key, Rank: "T", HandedOver: labelled}
	}
	question := closed("ask-q", "resolved")
	question.Kind = record.AskQuestion
	another := review(intake.DispatchArtifactApproved, 4)
	another.ArtifactID = "artifact-other"
	basis := func(version int) record.GateBasis {
		return record.GateBasis{Issue: cueAncestor, ArtifactID: cueDoc, Version: version, Approval: record.EventMark{Seq: 40, At: basisSeedApprovedAt},
			Approver: basisSeedPerson.Name(), Serves: []string{"dispatch://LEGION-150/spec#b-r1@v3"}, Reason: "The root delivers r1 of LEGION-150.",
			Session: "session-architect", RegisteredAt: basisSeedApprovedAt.Add(time.Hour)}
	}
	waiting := record.PendingBasisSeed{Step: record.DesignStepSpec, ArtifactID: basisSeedRootDoc, BasisArtifactID: cueDoc, BasisVersion: 4, Ask: cueAsk,
		Generation: 1, RegisteredAt: basis(4).RegisteredAt}
	for _, tc := range []struct {
		name string
		fact intake.Fact
		// waits says the root's spec step waits on the request; open's step stands open on the
		// document in every case.
		waits bool
		// cue says fact is a cue for the waiting step, and none that it can be a cue for no step
		// at all.
		cue, none bool
	}{
		{name: "a person's approval of the version", fact: review(intake.DispatchArtifactApproved, 4), waits: true, cue: true},
		{name: "a changes request on the version", fact: review(intake.DispatchArtifactChangesRequested, 4), waits: true, cue: true},
		{name: "a newer version", fact: review(intake.DispatchArtifactVersion, 5), waits: true, cue: true},
		{name: "a close of the request", fact: closed(cueAsk, "resolved"), waits: true, cue: true},
		{name: "the ancestor done", fact: issue(cueAncestor, "done", true), waits: true, cue: true},
		{name: "the ancestor no longer labelled", fact: issue(cueAncestor, "in_progress", false), waits: true, cue: true},
		{name: "the version's own version event", fact: review(intake.DispatchArtifactVersion, 4), waits: true},
		{name: "an approval of an older version", fact: review(intake.DispatchArtifactApproved, 3), waits: true},
		{name: "an event of another document", fact: another, waits: true},
		{name: "a close of another request", fact: closed("ask-9", "resolved"), waits: true},
		{name: "another issue done", fact: issue("LEGION-300", "done", true), waits: true},
		{name: "a person's approval of the version, with no step waiting", fact: review(intake.DispatchArtifactApproved, 4)},
		{name: "a close of the request, with no step waiting", fact: closed(cueAsk, "resolved")},
		{name: "the ancestor done, with no step waiting", fact: issue(cueAncestor, "done", true)},
		{name: "a person's answer of the request", fact: closed(cueAsk, "answered"), waits: true, none: true},
		{name: "a close of a question", fact: question, waits: true, none: true},
		{name: "the ancestor at work", fact: issue(cueAncestor, "in_progress", true), waits: true, none: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newBasisSeedRig(t)
			putOutboxIssue(t, rig.pool, rig.records, record.Issue{Key: open, Tree: open, Project: "LEGION", Title: "open", Phase: phase.Admitted, Generation: 1,
				Status: "in_progress", DispatchStatus: "in_progress", Rank: "V", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets"})
			putOutboxGate(t, rig.pool, rig.records, record.OpenOnBasis(record.DesignGate{Issue: open, ArtifactID: openDoc, LatestVersion: 4}, basis(3)))
			if tc.waits {
				putOutboxGate(t, rig.pool, rig.records, record.PendOnBasis(record.DesignGate{Issue: basisSeedRoot, ArtifactID: basisSeedRootDoc, LatestVersion: 4},
					basis(4), cueAsk))
			}
			// What Dispatch shows: the request open on version 4, and the ancestor at work.
			rig.client.owners[cueDoc] = cueAncestor
			rig.client.approvals[cueDoc] = dispatch.Approval{State: "awaiting", LatestVersion: 4, AskID: cueAsk, WaitingOn: "human"}
			rig.client.issues[cueAncestor] = dispatch.Issue{Key: cueAncestor, Status: "in_progress", Labels: []string{dispatch.LegionLabel}, HandedOver: true}
			client := newCountedBasisDispatch(rig.client)
			rig.runner.dispatch = client
			counted := &cueReadsCounted{Store: rig.records}
			engine := workflow.New(counted, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())

			if _, err := intake.ApplyFact(context.Background(), rig.pool, "dispatch", "event", tc.fact, engine); err != nil {
				t.Fatalf("apply the event: %v", err)
			}

			if tc.none && counted.reads != 0 {
				t.Fatalf("the event read the store's steps %d times, want none: it can be a cue for no step", counted.reads)
			}
			var want []record.PendingBasisSeed
			if tc.cue {
				want = []record.PendingBasisSeed{waiting}
			}
			if seeds := pendingBasisSeedsOf(t, rig.pool, basisSeedRoot); !slices.Equal(seeds, want) {
				t.Fatalf("pending-basis seeds of %s = %#v, want %#v", basisSeedRoot, seeds, want)
			}
			if seeds := pendingBasisSeedsOf(t, rig.pool, open); len(seeds) != 0 {
				t.Fatalf("pending-basis seeds of %s = %#v, want none: the event is no cue for a step standing open", open, seeds)
			}
			for _, seed := range want {
				rig.execute(seed)
			}
			if reads := len(want); client.approvalReads[cueDoc] != reads || client.issueReads[cueAncestor] != reads || client.askReads[cueAsk] != 0 {
				t.Fatalf("Dispatch was read for the document %d times, for the ancestor %d times and for the request %d times, want %d, %d and none: the seeds' reads alone, of a request Dispatch shows open",
					client.approvalReads[cueDoc], client.issueReads[cueAncestor], client.askReads[cueAsk], reads, reads)
			}
			if tc.waits {
				if gate := rig.gate(); !gate.PendingOnBasis() || record.GateOpen(gate) {
					t.Fatalf("the spec step = %#v, want it still waiting on %s: Dispatch shows the request open and the ancestor at work", gate, cueAsk)
				}
			}
		})
	}
}

// The pending-basis seed reads the request its step waits on by its id (dispatch.Client.Ask) only
// where the approval it read shows that request no longer open with no approval of the version
// (workflow.RequestClosed): no review of the latest version, and no open request or another one.
// Every other seed reads the document's approval and the ancestor's issue once each and nothing
// more: the person's approval, an approval that names nobody, a changes request on the version, a
// newer version, and the request still waiting. The read of the close names who closed the request,
// so the end of the wait names them in the closer's sentence and as its actor, and a read that
// finds another closer is a fact of its own (basisReadFound). A request whose read shows it open
// while its approval shows it closed, its approval and its ask read either side of a reopen, is no
// close: the seed's run fails, to read again on its backoff, and the step still waits.
func TestThePendingBasisSeedReadsTheCloseOnlyWhereTheApprovalShowsTheRequestClosed(t *testing.T) {
	approvedAt := basisSeedApprovedAt.Add(24 * time.Hour)
	reason := "name the rollback path"
	closer := dispatch.Actor{Kind: "session", ID: "session-architect"}
	const closedSentence = "session:session-architect closed approval request ask-100 of version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec), as resolved, with no approval of that version."
	for _, tc := range []struct {
		name     string
		approval dispatch.Approval
		// closed says the approval shows ask-100 closed, so the seed reads it once.
		closed bool
	}{
		{name: "approved by the person", approval: dispatch.Approval{State: "approved", LatestVersion: 4, Version: new(4), At: approvedAt, Seq: 60, By: basisSeedPerson}},
		{name: "approved naming nobody", approval: dispatch.Approval{State: "approved", LatestVersion: 4, Version: new(4), At: approvedAt, Seq: 60}},
		{name: "changes requested on the version", approval: dispatch.Approval{State: "changes_requested", LatestVersion: 4, Version: new(4), Reason: &reason,
			At: approvedAt, Seq: 61, By: basisSeedPerson}},
		{name: "a newer version", approval: dispatch.Approval{State: "awaiting", LatestVersion: 5, AskID: "ask-100", WaitingOn: "agent"}},
		{name: "still waiting", approval: dispatch.Approval{State: "awaiting", LatestVersion: 4, AskID: "ask-100", WaitingOn: "human"}},
		{name: "closed with no approval", approval: dispatch.Approval{State: "draft", LatestVersion: 4}, closed: true},
		{name: "another request open on the version", approval: dispatch.Approval{State: "awaiting", LatestVersion: 4, AskID: "ask-200", WaitingOn: "human"},
			closed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newBasisSeedRig(t)
			rig.pendingOnBasis(basisSeedRoot, basisSeedRootDoc)
			rig.client.approvals[basisSeedDoc] = tc.approval
			rig.client.asks["ask-100"] = dispatch.Ask{State: "resolved", ClosedBy: closer}
			client := newCountedBasisDispatch(rig.client)
			rig.runner.dispatch = client

			rig.execute(basisSeedPending(1))

			closes := 0
			if tc.closed {
				closes = 1
			}
			if client.approvalReads[basisSeedDoc] != 1 || client.issueReads[basisSeedAncestor] != 1 || client.askReads["ask-100"] != closes {
				t.Fatalf("Dispatch was read for the document %d times, for the ancestor %d times and for the request %d times, want once, once and %d",
					client.approvalReads[basisSeedDoc], client.issueReads[basisSeedAncestor], client.askReads["ask-100"], closes)
			}
			ended := noticesOfKind(t, rig.pool, basisSeedRoot, "basis-ended")
			if tc.closed && (len(ended) != 1 || !strings.Contains(ended[0].Reason, closedSentence) || ended[0].Actor != closer.Name()) {
				t.Fatalf("basis-ended notices = %#v, want one naming %q, made by %s", ended, closedSentence, closer.Name())
			}
			if !tc.closed && slices.ContainsFunc(ended, func(n record.Notice) bool { return strings.Contains(n.Reason, "closed approval request") }) {
				t.Fatalf("basis-ended notices = %#v, want none naming a close: the seed read none", ended)
			}
		})
	}

	t.Run("closed by a person's answer", func(t *testing.T) {
		rig := newBasisSeedRig(t)
		rig.pendingOnBasis(basisSeedRoot, basisSeedRootDoc)
		rig.client.approvals[basisSeedDoc] = dispatch.Approval{State: "draft", LatestVersion: 4}
		rig.client.asks["ask-100"] = dispatch.Ask{State: "answered", ClosedBy: basisSeedPerson}

		rig.execute(basisSeedPending(1))

		const sentence = "user:alice closed approval request ask-100 of version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec), as answered, with no approval of that version."
		if got := noticesOfKind(t, rig.pool, basisSeedRoot, "basis-ended"); len(got) != 1 || got[0].Actor != "user:alice" || !strings.Contains(got[0].Reason, sentence) {
			t.Fatalf("basis-ended notices = %#v, want one naming %q and actor user:alice", got, sentence)
		}
	})

	t.Run("another closer is another fact", func(t *testing.T) {
		read := func(actor dispatch.Actor) intake.BasisRead {
			return intake.BasisRead{Approval: dispatch.Approval{State: "draft", LatestVersion: 4}, Ancestor: intake.AncestorRead{Status: "in_progress", HandedOver: true},
				Closed: &intake.BasisClose{Actor: actor, State: "resolved"}}
		}
		if one, other := basisReadFound(read(closer)), basisReadFound(read(basisSeedPerson)); one == other {
			t.Fatalf("the read closed by %s and the read closed by %s are both %q, want two facts", closer.Name(), basisSeedPerson.Name(), one)
		}
	})

	t.Run("the request read open again", func(t *testing.T) {
		rig := newBasisSeedRig(t)
		rig.pendingOnBasis(basisSeedRoot, basisSeedRootDoc)
		rig.client.approvals[basisSeedDoc] = dispatch.Approval{State: "draft", LatestVersion: 4}
		rig.client.asks["ask-100"] = dispatch.Ask{State: "open"}

		row := mustOutboxRow(t, basisSeedRoot, basisSeedPending(1), time.Now())
		if err := rig.runner.execute(context.Background(), row); err == nil || permanentRefusal(row, err) {
			t.Fatalf("the seed's run = %v, finished refused %t; want an error, retried", err, permanentRefusal(row, err))
		}
		if gate := rig.gate(); !gate.PendingOnBasis() || record.GateOpen(gate) {
			t.Fatalf("the spec step = %#v, want it still waiting: no read ended it", gate)
		}
		if got := noticesOfKind(t, rig.pool, basisSeedRoot, "basis-ended"); len(got) != 0 {
			t.Fatalf("basis-ended notices = %#v, want none", got)
		}
	})
}

// A pending-basis seed whose read of Dispatch fails returns that failure as the row's error, and
// the outbox retries the row on its backoff rather than finishing it refused, however the read
// fails: Dispatch down, a coded refusal that would finish a status write's row, no answer for the
// ancestor's issue, or, where the approval shows the request closed, Dispatch down for the read
// of that request's close (permanentRefusal names no pending-basis seed). The seed's read alone
// opens or ends the wait and no event waits on it, so a long outage puts the opening or the end
// off by the seed's retries and nothing else: the step stays waiting, and nobody is told anything.
func TestAFailedPendingBasisSeedIsRetriedOnItsBackoff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failing string
		failure error
		// closed has Dispatch show the request closed with no approval, which the seed's read of
		// it would end the wait on, rather than the person's approval, which would open the step.
		closed bool
	}{
		{"Dispatch down for the document", basisSeedDoc, &dispatch.Error{Status: http.StatusServiceUnavailable, Code: oauth2calltest.Said("UNAVAILABLE"), Message: "Dispatch is down"}, false},
		{"Dispatch refusing the document", basisSeedDoc, &dispatch.Error{Status: http.StatusNotFound, Code: oauth2calltest.Said("NOT_FOUND"), Message: "no such artifact"}, false},
		{"no answer for the ancestor's issue", basisSeedAncestor, &dispatch.TransientError{Err: errors.New("dial tcp: connection refused")}, false},
		{"Dispatch down for the request's close", "ask-100", &dispatch.Error{Status: http.StatusServiceUnavailable, Code: oauth2calltest.Said("UNAVAILABLE"), Message: "Dispatch is down"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newBasisSeedRig(t)
			rig.pendingOnBasis(basisSeedRoot, basisSeedRootDoc)
			// What Dispatch would answer: the person's approval of the version, which would open the
			// step, or the request closed by a session, which would end the wait.
			rig.client.approvals[basisSeedDoc] = dispatch.Approval{State: "approved", LatestVersion: 4, Version: new(4), At: basisSeedApprovedAt.Add(24 * time.Hour),
				Seq: 60, By: basisSeedPerson}
			if tc.closed {
				rig.client.approvals[basisSeedDoc] = dispatch.Approval{State: "draft", LatestVersion: 4}
				rig.client.asks["ask-100"] = dispatch.Ask{State: "resolved", ClosedBy: dispatch.Actor{Kind: "session", ID: "session-architect"}}
			}
			client := newCountedBasisDispatch(rig.client)
			client.failing, client.failure = tc.failing, tc.failure
			now := time.Now().UTC().Truncate(time.Microsecond)
			rig.runner.dispatch, rig.runner.now = client, func() time.Time { return now }
			row := mustOutboxRow(t, basisSeedRoot, basisSeedPending(1), now)

			if err := rig.runner.execute(ctx, row); !errors.Is(err, tc.failure) || permanentRefusal(row, err) {
				t.Fatalf("the seed's run = %v, finished refused %t; want Dispatch's failure, retried", err, permanentRefusal(row, err))
			}

			enqueueOutbox(t, rig.pool, rig.records, row)
			if err := rig.runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the outbox: %v", err)
			}
			var attempts int
			var nextAt time.Time
			var lastError string
			if err := rig.pool.QueryRow(ctx, "select attempts, next_at, last_error from outbox where kind = $1 and issue = $2",
				string(record.OutboxKindPendingBasisSeed), basisSeedRoot).Scan(&attempts, &nextAt, &lastError); err != nil {
				t.Fatalf("read the seed after its failed run: %v; want it still queued, as a row finished refused is not", err)
			}
			if attempts != 1 || !nextAt.Equal(now.Add(record.RetryBackoff(0))) || !strings.Contains(lastError, tc.failure.Error()) {
				t.Fatalf("the seed is on attempt %d, due at %s, with error %q; want attempt 1 due at %s, after its first backoff, keeping %q",
					attempts, nextAt, lastError, now.Add(record.RetryBackoff(0)), tc.failure.Error())
			}
			if gate := rig.gate(); !gate.PendingOnBasis() || record.GateOpen(gate) {
				t.Fatalf("the spec step = %#v, want it still waiting: no read opened or ended it", gate)
			}
			for _, kind := range []record.NoticeKind{"design-approved", "basis-ended"} {
				if got := noticesOfKind(t, rig.pool, basisSeedRoot, kind); len(got) != 0 {
					t.Fatalf("%s notices = %#v, want none", kind, got)
				}
			}
		})
	}
}
