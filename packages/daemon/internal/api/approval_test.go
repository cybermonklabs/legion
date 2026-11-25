package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The trees of these tests are a neutral project's, ACME, as every example of this public
// repository is. ownRoot is the root of F3 (dispatch-carried-approval.json), and ownDocument its
// spec; otherRoot is a root whose tree a hand-over left, and carriedRoot and chainedRoot the roots
// two hand-overs made of its work.
const (
	approvalProject = "ACME"
	ownRoot         = "ACME-2694"
	ownDocument     = "aec8fb48-fd46-458a-99a3-a1b199a46b38"
	ownChild        = "ACME-2695"
	secondRoot      = "ACME-2696"
	secondDocument  = "0c6f1e2d-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	otherRoot       = "ACME-900"
	otherDocument   = "5b0f3c2e-7d14-4a9b-8e61-2c9a7f4d1b03"
	carriedRoot     = "ACME-950"
	chainedRoot     = "ACME-951"
)

// carriedApprovalFixture is F3: Dispatch's carried approval after an exact undo, as its events
// route served it, scrubbed for this public repository.
const carriedApprovalFixture = "../workflow/testdata/dispatch-carried-approval.json"

// approvalPerson is the person whose reviews the facts below carry.
var approvalPerson = dispatch.Actor{Kind: "user", ID: "person@example.com"}

// specGated is the deployment's `gates.design: root-issues`, which gates the spec step alone.
var specGated = config.DesignGateRootIssues.Steps()

// r1Rest is R1's text, the approval message's requirement, after its first sentence and its first
// clause: the words every form of the message keeps.
const r1Rest = "at the highest standard. The team will bring reasons to change it. " +
	"Be prepared to refuse them, and to help find a way to keep the goal and the spec whole. " +
	"A change to what the spec delivers goes to the person as one decision before any work on it, " +
	"and a requirement the work missed goes back to the work."

// ownWords is the message of a root whose own spec a person approved at version.
func ownWords(key string, version int) string {
	return fmt.Sprintf("A person has approved version %d of the spec of %s (dispatch://%s/spec@v%d). ", version, key, key, version) +
		"Your top responsibility is now to deliver it and no other, " + r1Rest
}

// carriedWords is the message of tree, which carries a copy of from's gate that version opened.
func carriedWords(from, tree string, version int) string {
	return fmt.Sprintf("A person has approved version %d of the spec of %s (dispatch://%s/spec@v%d), whose work this tree, %s, carries. ",
		version, from, from, version, tree) +
		"Your top responsibility is now to deliver what it asks of this tree, and no other, " + r1Rest
}

// keylessWords is carriedWords for a copy whose hand-over row names no root: the document is named
// by its id.
func keylessWords(document, tree string, version int) string {
	return fmt.Sprintf("A person has approved version %d of the spec document %s, whose work this tree, %s, carries. ", version, document, tree) +
		"Your top responsibility is now to deliver what it asks of this tree, and no other, " + r1Rest
}

// approvalRig is the daemon's own handler chain (the mirror, the engine, admission) over a tree of
// ACME's, with the API serving the claims of that tree, as heldChildHarness builds it.
type approvalRig struct {
	*harness
	records  record.Store
	handlers []intake.Handler
	// seq is the newest Dispatch event sequence a fact has carried.
	seq int64
	// facts counts the facts applied, which names each one.
	facts int
}

// newApprovalRig is the rig of a deployment that gates steps.
func newApprovalRig(t *testing.T, steps []record.DesignStep) *approvalRig {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: approvalProject, Repositories: testRepositories, Steps: steps}, log)
	r := &approvalRig{harness: h, records: records, seq: 100,
		handlers: []intake.Handler{blockers.NewMirror(records, approvalProject), engine, admit.New(records, engine, 4, approvalProject, log)}}
	r.serve(steps)
	return r
}

// serve is the API of a daemon that gates steps over the same store and claims: what the same
// deployment serves once a restart has taken another `gates.design`.
func (r *approvalRig) serve(steps []record.DesignStep) {
	r.handler = NewServer("127.0.0.1", 8437, Options{
		Steps: steps, Supervisor: r.supervisor, BootTokens: r.tokens, Project: testProject, DispatchProject: approvalProject,
		OperatorToken: testOperatorToken, Controller: r.store, Pool: r.store.Pool(), Record: r.records, Releaser: r.runtime,
		Trees: r.store, RoleFor: phase.RoleFor, Log: slog.New(slog.NewTextHandler(r.serverLog, nil)),
	}).Handler
}

// next is the sequence of the next Dispatch event.
func (r *approvalRig) next() int64 {
	r.seq++
	return r.seq
}

// mark is the place of the event at seq in its issue's event log.
func mark(seq int64) record.EventMark {
	return record.EventMark{Seq: seq, At: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Second)}
}

// apply runs fact through the handler chain in one transaction, as the daemon's intake does.
func (r *approvalRig) apply(fact intake.Fact) {
	r.t.Helper()
	r.facts++
	if _, err := intake.ApplyFact(context.Background(), r.store.Pool(), "dispatch", fmt.Sprintf("approval-fact-%d", r.facts), fact, r.handlers...); err != nil {
		r.t.Fatalf("apply %T: %v", fact, err)
	}
}

// issue is the record of key.
func (r *approvalRig) issue(key string) record.Issue {
	r.t.Helper()
	got := recordedChild(r.t, r.harness, key)
	if got == nil {
		r.t.Fatalf("no record of %s", key)
	}
	return *got
}

// read runs read in a read-only transaction.
func (r *approvalRig) read(read func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.TransactWith(context.Background(), r.store.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(_ context.Context, tx pgx.Tx) error { return read(tx) }); err != nil {
		r.t.Fatalf("read the records: %v", err)
	}
}

// write runs write in a transaction: the fixture's own records.
func (r *approvalRig) write(write func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.Transact(context.Background(), r.store.Pool(), func(_ context.Context, tx pgx.Tx) error { return write(tx) }); err != nil {
		r.t.Fatalf("seed the records: %v", err)
	}
}

// unnamedFromRoot writes key's hand-over row as a release before from_root wrote it: the
// migration gave such a row the column's default, so it names no root.
func (r *approvalRig) unnamedFromRoot(key string) {
	r.t.Helper()
	r.write(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "update handed_over_roots set from_root = default where issue = $1", key)
		return err
	})
}

// root records key as an admitted root of its own live tree whose architect registered version of
// document as its spec, as register_gate's fact carries it with no review read.
func (r *approvalRig) root(key, document string, version int) {
	r.t.Helper()
	r.recordIssue(record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key, Project: approvalProject,
		Title: key, Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A", HandedOver: true, LastDispatchSeq: 1})
	r.openTree(r.t, approvalProject, key)
	r.apply(intake.GateRegistered{Issue: key, Step: record.DesignStepSpec, ArtifactID: document, Version: version, DispatchVersion: version,
		Since: record.EventMark{Seq: r.seq}})
}

// review applies a person's review or a new version of key's document, at the next sequence.
func (r *approvalRig) review(key, document string, kind intake.DispatchArtifactKind, version int) {
	r.t.Helper()
	r.reviewAt(key, document, kind, version, r.next())
}

// reviewAt is review at seq.
func (r *approvalRig) reviewAt(key, document string, kind intake.DispatchArtifactKind, version int, seq int64) {
	r.t.Helper()
	fact := intake.DispatchArtifact{Key: key, ArtifactID: document, Kind: kind, Version: version, Mark: mark(seq)}
	switch kind {
	case intake.DispatchArtifactChangesRequested:
		fact.Reason, fact.Actor = "split the migration out", approvalPerson
	case intake.DispatchArtifactApproved:
		fact.Actor = approvalPerson
	}
	r.apply(fact)
}

// lingering records key as a root whose tree left the workflow: its phase done, its tree
// lingering, in status, keeping the rest of the record key holds, or at generation 1 where it has
// none.
func (r *approvalRig) lingering(key, status string) {
	r.t.Helper()
	until := time.Now().Add(time.Hour)
	issue := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key, Project: approvalProject,
		Title: key, Generation: 1, Rank: "A", HandedOver: true, LastDispatchSeq: 1}
	if stored := recordedChild(r.t, r.harness, key); stored != nil {
		issue = *stored
	}
	issue.Phase, issue.Status, issue.DispatchStatus, issue.LingerUntil = phase.Done, status, status, &until
	r.recordIssue(issue)
}

// outOfWorkflow records ownRoot as a root a person moved to the backlog after its run, out of the
// workflow, whose spec row holds document's version latest as its earlier run registered it.
func (r *approvalRig) outOfWorkflow(latest int) {
	r.t.Helper()
	r.lingering(ownRoot, "backlog")
	r.write(func(tx pgx.Tx) error {
		return r.records.PutGate(context.Background(), tx, record.DesignGate{Issue: ownRoot, Step: record.DesignStepSpec, ArtifactID: ownDocument, LatestVersion: latest})
	})
}

// outOfWorkflowApproval is a person's approval of ownRoot's spec at version while the root is out
// of the workflow in the backlog, prepared with Dispatch's view of the root as the daemon reads it
// for a person's approval: its handover.
func (r *approvalRig) outOfWorkflowApproval(version int) {
	r.t.Helper()
	r.apply(intake.DispatchArtifact{Key: ownRoot, ArtifactID: ownDocument, Kind: intake.DispatchArtifactApproved, Version: version,
		Mark: mark(r.next()), Actor: approvalPerson, Issue: &dispatch.Issue{Key: ownRoot, Project: approvalProject, Title: ownRoot,
			Status: "backlog", Labels: []string{dispatch.LegionLabel}, HandedOver: true, PrimaryArtifactID: ownDocument}})
}

// todo is key's move into todo with the legion label, which re-admits a root that left the
// workflow as a new generation.
func (r *approvalRig) todo(key string) {
	r.t.Helper()
	before := r.issue(key).Generation
	r.apply(intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: r.next(), Type: "issue.updated", Status: "todo",
		Title: key, Rank: "A", HandedOver: true})
	if got := r.issue(key); got.Generation != before+1 || got.Phase != phase.Admitted {
		r.t.Fatalf("%s after its todo = generation %d at %s, want it re-admitted at generation %d", key, got.Generation, got.Phase, before+1)
	}
}

// handOver records child as a member of the lingering oldRoot's tree that started its phase, and
// has the daemon hand it over as a root of its own: admission's re-admission with carry, which
// copies oldRoot's gate rows to it.
func (r *approvalRig) handOver(oldRoot, child string) {
	r.t.Helper()
	old := r.issue(oldRoot)
	parent := oldRoot
	r.recordIssue(record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: oldRoot, Project: approvalProject,
		Title: child, Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "B", LastDispatchSeq: 1})
	r.write(func(tx pgx.Tx) error {
		return r.records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: oldRoot, Generation: old.Generation})
	})
	r.apply(intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: r.next(), Type: "issue.updated", Status: "todo",
		Title: child, Parent: oldRoot, Rank: "B", HandedOver: true, DaemonActor: true})
	if got := r.issue(child); got.Tree != child || got.Generation != 2 {
		r.t.Fatalf("handed-over %s = tree %s at generation %d, want a root of its own at generation 2", child, got.Tree, got.Generation)
	}
}

// carried is carriedRoot handed over out of otherRoot's tree, whose spec row was gate.
func (r *approvalRig) carried(gate record.DesignGate) {
	r.t.Helper()
	r.lingering(otherRoot, "done")
	gate.Issue, gate.Step, gate.ArtifactID = otherRoot, record.DesignStepSpec, otherDocument
	r.write(func(tx pgx.Tx) error { return r.records.PutGate(context.Background(), tx, gate) })
	r.handOver(otherRoot, carriedRoot)
}

// handedOverRoot is the hand-over row of key.
func (r *approvalRig) handedOverRoot(key string) *record.HandedOverRoot {
	r.t.Helper()
	var handed *record.HandedOverRoot
	r.read(func(tx pgx.Tx) error {
		var err error
		handed, err = r.records.HandedOverRoot(context.Background(), tx, key)
		return err
	})
	return handed
}

// clearedSpecApproval is the re-admission's record of key.
func (r *approvalRig) clearedSpecApproval(key string) *record.ClearedSpecApproval {
	r.t.Helper()
	var kept *record.ClearedSpecApproval
	r.read(func(tx pgx.Tx) error {
		var err error
		kept, err = r.records.ClearedSpecApproval(context.Background(), tx, key)
		return err
	})
	return kept
}

// architect is a live claim of key's own architect.
func (r *approvalRig) architect(key string) liveClaim {
	r.t.Helper()
	return newLiveClaimIn(r.t, r.harness, key, key, claim.RoleArchitect)
}

// approvalBody is the route's request for c.
func approvalBody(c liveClaim) ClaimApprovalRequest {
	return ClaimApprovalRequest{ClaimToken: c.token, SessionID: c.session, Secret: c.secret}
}

// approval is what the route answers c.
func (r *approvalRig) approval(c liveClaim) *ClaimApproval {
	r.t.Helper()
	recorder := r.request(http.MethodPost, "/legion/v1/claims/approval", approvalBody(c), nil)
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("the approval of %s = %d, want 200; body %s", c.token, recorder.Code, recorder.Body)
	}
	var answer ClaimApprovalResponse
	decodeInto(r.t, recorder, &answer)
	return answer.Approval
}

// wantApproval holds the route's answer to c to document, version and message.
func (r *approvalRig) wantApproval(c liveClaim, document string, version int, message string) {
	r.t.Helper()
	got := r.approval(c)
	if got == nil {
		r.t.Fatalf("the approval of %s = none, want version %d of %s", c.issue, version, document)
	}
	if got.Document != document || got.Version != version || got.Message != message {
		r.t.Fatalf("the approval of %s = %+v,\nwant document %s, version %d, message:\n%s", c.issue, *got, document, version, message)
	}
}

// wantNone holds the route to answering c that no approval stands.
func (r *approvalRig) wantNone(c liveClaim) {
	r.t.Helper()
	if got := r.approval(c); got != nil {
		r.t.Fatalf("the approval of %s (%s) = %+v, want none", c.issue, c.token, *got)
	}
}

// The message is R1's text with its first sentence naming the version and the document: the root's
// own spec, or for a carried copy the other root's spec and the tree that carries its work, where
// "deliver it and no other" becomes "deliver what it asks of this tree, and no other"; a copy whose
// hand-over row names no root names the document by its id.
func TestApprovalRouteCarriesR1sWords(t *testing.T) {
	for _, tc := range []struct {
		name  string
		words prompts.Approval
		want  string
	}{
		{"the own first sentence", prompts.Approval{Tree: "ACME-2694", Spec: "ACME-2694", Document: ownDocument, Version: 24},
			ownWords("ACME-2694", 24)},
		{"the carried first sentence and wording", prompts.Approval{Tree: "ACME-950", Spec: "ACME-900", Document: otherDocument, Version: 12, Carried: true},
			carriedWords("ACME-900", "ACME-950", 12)},
		{"the carried wording with no key", prompts.Approval{Tree: "ACME-950", Document: otherDocument, Version: 12, Carried: true},
			keylessWords(otherDocument, "ACME-950", 12)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := prompts.ApprovalMessage(tc.words)
			if err != nil {
				t.Fatalf("fill the words: %v", err)
			}
			if got != tc.want {
				t.Fatalf("the message =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// An approval stands while the deployment gates the spec step, for the architect of a tree root,
// where the root's spec row records a version a person's approval set; a carried copy's row
// answers the carried message naming the root its hand-over came from, at the version the row
// records; and a root re-admitted with no spec row is answered by the re-admission's record, which
// stands for that row until its next registration: a new version raises its latest version, a
// later approval moves it, a changes request the row would take clears it, one the row would
// ignore leaves it (one older than the approval it records, or one on a version older than its
// latest), and the registration's row keeps its approval and a latest version newer than the
// registration's read. Every other claim, and a deployment that gates no spec step, is answered
// none.
func TestApprovalRouteAnswersTheStandingApproval(t *testing.T) {
	t.Run("A1 approved at the latest version", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.root(ownRoot, ownDocument, 3)
		architect := r.architect(ownRoot)
		r.wantNone(architect)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 3, ownWords(ownRoot, 3))
	})
	t.Run("A2 a later version after the approval", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.root(ownRoot, ownDocument, 3)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactVersion, 4)
		r.wantApproval(r.architect(ownRoot), "dispatch://ACME-2694/spec", 3, ownWords(ownRoot, 3))
	})
	t.Run("A3 a changes request on its own spec", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.root(ownRoot, ownDocument, 3)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		architect := r.architect(ownRoot)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 3, ownWords(ownRoot, 3))
		r.review(ownRoot, ownDocument, intake.DispatchArtifactChangesRequested, 3)
		r.wantNone(architect)
	})
	t.Run("A4 a later approval", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.root(ownRoot, ownDocument, 3)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactVersion, 4)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 4)
		r.wantApproval(r.architect(ownRoot), "dispatch://ACME-2694/spec", 4, ownWords(ownRoot, 4))
	})
	t.Run("A5 an exact undo", func(t *testing.T) {
		// F3: a person approved version 24, an edit wrote version 45, and its exact undo wrote
		// version 46, whose text is version 24's. Dispatch carried the approval to 46 in the write
		// of 46, as the event at sequence 1457 records.
		r := newApprovalRig(t, specGated)
		r.root(ownRoot, ownDocument, 24)
		r.reviewAt(ownRoot, ownDocument, intake.DispatchArtifactApproved, 24, 1300)
		r.reviewAt(ownRoot, ownDocument, intake.DispatchArtifactVersion, 45, 1440)
		architect := r.architect(ownRoot)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 24, ownWords(ownRoot, 24))
		r.reviewAt(ownRoot, ownDocument, intake.DispatchArtifactVersion, 46, 1456)
		r.apply(carriedApproval(t))
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 46, ownWords(ownRoot, 46))
	})
	t.Run("A6 gates.design off", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.root(ownRoot, ownDocument, 3)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		architect := r.architect(ownRoot)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 3, ownWords(ownRoot, 3))
		// The same deployment, restarted under `gates.design: off`: no person's approval opens a
		// gate there, so the row its approval left says nothing.
		r.serve(config.DesignGateOff.Steps())
		r.wantNone(architect)
	})
	t.Run("A7 a deployment that gates no spec step", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.root(ownRoot, ownDocument, 3)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		architect := r.architect(ownRoot)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 3, ownWords(ownRoot, 3))
		r.serve([]record.DesignStep{record.DesignStepDecisions, record.DesignStepCriteria})
		r.wantNone(architect)
	})
	t.Run("A8 a child's architect and each phase worker", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.root(ownRoot, ownDocument, 3)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		parent := ownRoot
		r.recordIssue(record.Issue{Key: ownChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: ownRoot, Project: approvalProject,
			Title: ownChild, Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "B", HandedOver: true, LastDispatchSeq: 1})
		r.wantApproval(r.architect(ownRoot), "dispatch://ACME-2694/spec", 3, ownWords(ownRoot, 3))
		r.wantNone(newLiveClaimIn(t, r.harness, ownRoot, ownChild, claim.RoleArchitect))
		for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
			r.wantNone(newLiveClaimIn(t, r.harness, ownRoot, ownRoot, role))
		}
	})
	t.Run("A9 a carried root whose copy a person's approval of the other root's spec opened", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.carried(record.DesignGate{LatestVersion: 12})
		architect := r.architect(carriedRoot)
		r.wantNone(architect)
		r.review(otherRoot, otherDocument, intake.DispatchArtifactApproved, 12)
		r.wantApproval(architect, "dispatch://ACME-900/spec", 12, carriedWords(otherRoot, carriedRoot, 12))
	})
	t.Run("A9a a carried root whose copy was carried open", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.carried(record.DesignGate{LatestVersion: 12, ApprovedVersion: new(12), ApprovedMark: mark(r.next())})
		r.wantApproval(r.architect(carriedRoot), "dispatch://ACME-900/spec", 12, carriedWords(otherRoot, carriedRoot, 12))
	})
	t.Run("A9b A9 after a changes request on the other root's spec", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.carried(record.DesignGate{LatestVersion: 12})
		r.review(otherRoot, otherDocument, intake.DispatchArtifactApproved, 12)
		r.review(otherRoot, otherDocument, intake.DispatchArtifactChangesRequested, 12)
		r.wantApproval(r.architect(carriedRoot), "dispatch://ACME-900/spec", 12, carriedWords(otherRoot, carriedRoot, 12))
	})
	t.Run("A9c a carried root whose hand-over row names no root", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.carried(record.DesignGate{LatestVersion: 12, ApprovedVersion: new(12), ApprovedMark: mark(r.next())})
		r.unnamedFromRoot(carriedRoot)
		architect := r.architect(carriedRoot)
		r.wantApproval(architect, otherDocument, 12, keylessWords(otherDocument, carriedRoot, 12))
		if lines := r.serverLog.lines("api: the approval route names a carried spec by its document", carriedRoot, otherDocument); len(lines) != 1 {
			t.Fatalf("log lines for the copy named by its document = %q, want one", lines)
		}
	})
	t.Run("A9d a root carried from a carried root", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.carried(record.DesignGate{LatestVersion: 12, ApprovedVersion: new(12), ApprovedMark: mark(r.next())})
		r.lingering(carriedRoot, "done")
		r.handOver(carriedRoot, chainedRoot)
		r.wantApproval(r.architect(chainedRoot), "dispatch://ACME-900/spec", 12, carriedWords(otherRoot, chainedRoot, 12))
	})
	t.Run("A9e a copy carried closed", func(t *testing.T) {
		t.Run("with an older approved version on its row", func(t *testing.T) {
			r := newApprovalRig(t, specGated)
			r.carried(record.DesignGate{LatestVersion: 12, ApprovedVersion: new(11), ApprovedMark: mark(r.next())})
			r.wantApproval(r.architect(carriedRoot), "dispatch://ACME-900/spec", 11, carriedWords(otherRoot, carriedRoot, 11))
		})
		t.Run("with none", func(t *testing.T) {
			r := newApprovalRig(t, specGated)
			r.carried(record.DesignGate{LatestVersion: 12})
			r.wantNone(r.architect(carriedRoot))
		})
	})
	t.Run("A10 a root approved at its latest version while out of the workflow, re-admitted, before its registration", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.outOfWorkflowApproval(2)
		r.todo(ownRoot)
		r.wantApproval(r.architect(ownRoot), "dispatch://ACME-2694/spec", 2, ownWords(ownRoot, 2))
	})
	t.Run("A11 the same after its start task is told and a compaction", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.outOfWorkflowApproval(2)
		r.todo(ownRoot)
		architect := r.architect(ownRoot)
		r.tellStartAndCompact(architect)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 2, ownWords(ownRoot, 2))
	})
	t.Run("A12 the same after a changes request on its document", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.outOfWorkflowApproval(2)
		r.todo(ownRoot)
		architect := r.architect(ownRoot)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 2, ownWords(ownRoot, 2))
		r.review(ownRoot, ownDocument, intake.DispatchArtifactChangesRequested, 2)
		r.wantNone(architect)
	})
	t.Run("A13 the same after its registration", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.outOfWorkflowApproval(2)
		approved := r.seq
		r.todo(ownRoot)
		architect := r.architect(ownRoot)
		// The new architect's register_gate reads the person's approval of the latest version from
		// Dispatch, and its fact carries it.
		r.apply(intake.GateRegistered{Issue: ownRoot, Step: record.DesignStepSpec, ArtifactID: ownDocument, Version: 2, DispatchVersion: 2,
			ApprovedVersion: 2, ApprovedMark: mark(approved), ReviewedBy: approvalPerson, Since: record.EventMark{Seq: r.seq}})
		if kept := r.clearedSpecApproval(ownRoot); kept != nil {
			t.Fatalf("the re-admission's record after the registration = %+v, want it cleared", *kept)
		}
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 2, ownWords(ownRoot, 2))
	})
	t.Run("A14 a root approved below its latest version, re-admitted, after its registration of the latest version", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.outOfWorkflowApproval(2)
		approved := r.seq
		r.review(ownRoot, ownDocument, intake.DispatchArtifactVersion, 3)
		r.todo(ownRoot)
		architect := r.architect(ownRoot)
		// The new architect's register_gate reads version 3, which no person has approved, so its
		// fact carries no approval.
		r.apply(intake.GateRegistered{Issue: ownRoot, Step: record.DesignStepSpec, ArtifactID: ownDocument, Version: 3, DispatchVersion: 3,
			Since: record.EventMark{Seq: r.seq}})
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 2, ownWords(ownRoot, 2))
		r.wantApprovedRow(ownRoot, 3, 2, mark(approved))
		if kept := r.clearedSpecApproval(ownRoot); kept != nil {
			t.Fatalf("the re-admission's record after the registration = %+v, want it cleared", *kept)
		}
	})
	t.Run("A15 a root re-admitted, then approved at a later version before its registration", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.outOfWorkflowApproval(2)
		r.todo(ownRoot)
		architect := r.architect(ownRoot)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactVersion, 3)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 2, ownWords(ownRoot, 2))
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 3, ownWords(ownRoot, 3))
		r.wantRecord(record.ClearedSpecApproval{Issue: ownRoot, ArtifactID: ownDocument, Version: 3, LatestVersion: 3, Mark: mark(r.seq)})
	})
	t.Run("A16 a re-admitted root's changes requests on its document, older and newer than its approval", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		older := r.next()
		r.outOfWorkflowApproval(2)
		approved := r.seq
		r.todo(ownRoot)
		architect := r.architect(ownRoot)
		// A changes request the person gave before the approval, which the daemon applies late: a
		// row ignores it (classify.StaleReview), and so does the record.
		r.reviewAt(ownRoot, ownDocument, intake.DispatchArtifactChangesRequested, 2, older)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 2, ownWords(ownRoot, 2))
		r.wantRecord(record.ClearedSpecApproval{Issue: ownRoot, ArtifactID: ownDocument, Version: 2, LatestVersion: 2, Mark: mark(approved)})
		r.review(ownRoot, ownDocument, intake.DispatchArtifactChangesRequested, 2)
		if kept := r.clearedSpecApproval(ownRoot); kept != nil {
			t.Fatalf("the re-admission's record after a newer changes request = %+v, want it cleared", *kept)
		}
		r.wantNone(architect)
	})
	t.Run("A17 A15 after a registration whose read predates the later approval", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.outOfWorkflowApproval(2)
		r.todo(ownRoot)
		architect := r.architect(ownRoot)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactVersion, 3)
		// register_gate's read begins here and finds version 3 unapproved; the person's approval of
		// version 3 reaches the daemon before the registration does.
		since := r.seq
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		approved := r.seq
		r.apply(intake.GateRegistered{Issue: ownRoot, Step: record.DesignStepSpec, ArtifactID: ownDocument, Version: 3, DispatchVersion: 3,
			Since: record.EventMark{Seq: since}})
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 3, ownWords(ownRoot, 3))
		r.wantApprovedRow(ownRoot, 3, 3, mark(approved))
		if kept := r.clearedSpecApproval(ownRoot); kept != nil {
			t.Fatalf("the re-admission's record after the registration = %+v, want it cleared", *kept)
		}
	})
	t.Run("A18 a re-admitted root's changes request on a version older than its latest", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.outOfWorkflowApproval(2)
		approved := r.seq
		r.review(ownRoot, ownDocument, intake.DispatchArtifactVersion, 3)
		r.todo(ownRoot)
		architect := r.architect(ownRoot)
		r.wantRecord(record.ClearedSpecApproval{Issue: ownRoot, ArtifactID: ownDocument, Version: 2, LatestVersion: 3, Mark: mark(approved)})
		// The person asks for changes to version 3, and version 4 is written; the request reaches
		// the daemon after the version. A row at version 4 ignores it, and so does the record.
		changes := r.next()
		r.review(ownRoot, ownDocument, intake.DispatchArtifactVersion, 4)
		r.reviewAt(ownRoot, ownDocument, intake.DispatchArtifactChangesRequested, 3, changes)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 2, ownWords(ownRoot, 2))
		r.wantRecord(record.ClearedSpecApproval{Issue: ownRoot, ArtifactID: ownDocument, Version: 2, LatestVersion: 4, Mark: mark(approved)})
	})
	t.Run("A19 A18 after a registration whose read predates the record's latest version", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.outOfWorkflowApproval(2)
		approved := r.seq
		r.review(ownRoot, ownDocument, intake.DispatchArtifactVersion, 3)
		r.todo(ownRoot)
		architect := r.architect(ownRoot)
		// register_gate's read begins here and finds version 3. The person asks for changes to
		// version 3, and version 4 is written. The version reaches the daemon before the
		// registration, and the changes request after it. The row takes the record's latest
		// version, 4, and so ignores the request, as a row at version 4 does (A18).
		since := r.seq
		changes := r.next()
		r.review(ownRoot, ownDocument, intake.DispatchArtifactVersion, 4)
		r.apply(intake.GateRegistered{Issue: ownRoot, Step: record.DesignStepSpec, ArtifactID: ownDocument, Version: 3, DispatchVersion: 3,
			Since: record.EventMark{Seq: since}})
		r.reviewAt(ownRoot, ownDocument, intake.DispatchArtifactChangesRequested, 3, changes)
		r.wantApproval(architect, "dispatch://ACME-2694/spec", 2, ownWords(ownRoot, 2))
		r.wantApprovedRow(ownRoot, 4, 2, mark(approved))
		if kept := r.clearedSpecApproval(ownRoot); kept != nil {
			t.Fatalf("the re-admission's record after the registration = %+v, want it cleared", *kept)
		}
	})
}

// wantApprovedRow holds key's spec row to version latest, with the person's approval of version
// approved, placed at placed.
func (r *approvalRig) wantApprovedRow(key string, latest, approved int, placed record.EventMark) {
	r.t.Helper()
	var row *record.DesignGate
	r.read(func(tx pgx.Tx) error {
		gates, err := r.records.GatesOf(context.Background(), tx, key)
		row = record.SpecGate(gates)
		return err
	})
	if row == nil {
		r.t.Fatalf("%s holds no spec row, want version %d with the approval of version %d", key, latest, approved)
	}
	if row.LatestVersion != latest || row.ApprovedVersion == nil || *row.ApprovedVersion != approved ||
		row.ApprovedMark.Seq != placed.Seq || !row.ApprovedMark.At.Equal(placed.At) {
		got := "no approval"
		if row.ApprovedVersion != nil {
			got = fmt.Sprintf("the approval of version %d at %+v", *row.ApprovedVersion, row.ApprovedMark)
		}
		r.t.Fatalf("the spec row of %s = version %d with %s, want version %d with the approval of version %d at %+v",
			key, row.LatestVersion, got, latest, approved, placed)
	}
}

// tellStartAndCompact tells c's architect the start task its re-admission's promotion queued, as
// the outbox delivers a start, lets that turn end, and has its session compacted.
func (r *approvalRig) tellStartAndCompact(c liveClaim) {
	r.t.Helper()
	task := startedTask(r.t, r.harness, c.issue, claim.RoleArchitect)
	if task == "" {
		r.t.Fatalf("the re-admission of %s queued no start task for its architect", c.issue)
	}
	m, _ := r.supervisor.Machine(c.token)
	ctx := context.Background()
	for _, event := range []supervise.Event{
		supervise.StreamTurnEnd{Claim: c.token},
		supervise.RequestDeliver{Claim: c.token, Task: task, ID: "outbox:start", Generation: r.issue(c.issue).Generation},
		supervise.StreamTurnStart{Claim: c.token, DeliveryID: "outbox:start"},
		supervise.StreamTurnEnd{Claim: c.token},
	} {
		if err := m.Handle(ctx, event); err != nil {
			r.t.Fatalf("%T on %s: %v", event, c.token, err)
		}
	}
	connected(r.harness, c.token)
	if answer := decodeCompaction(r.t, r.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(c.token)+"/compact", nil)); !answer.Compacted {
		r.t.Fatalf("the compaction of %s answered %+v, want it compacted", c.token, answer)
	}
}

// carriedApproval is F3 (carriedApprovalFixture) decoded as the daemon's live consumer decodes it.
func carriedApproval(t *testing.T) intake.Fact {
	t.Helper()
	raw, err := os.ReadFile(carriedApprovalFixture)
	if err != nil {
		t.Fatalf("read the captured carried approval: %v", err)
	}
	var captured struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatalf("decode the captured carried approval: %v", err)
	}
	fact, unread, err := intake.DecodeDispatchEvent(approvalProject, ownRoot, captured.Event)
	if err != nil || len(unread) > 0 {
		t.Fatalf("decode the captured carried approval's event = %v, unread %v", err, unread)
	}
	artifact, ok := fact.(intake.DispatchArtifact)
	if !ok || !artifact.CarriedApproval() || artifact.Version != 46 || artifact.Mark.Seq != 1457 {
		t.Fatalf("the captured event decodes to %#v, want the carried approval of version 46 at sequence 1457", fact)
	}
	return fact
}

// The re-admission records the spec row it deletes whatever version it approved, the latest one or
// one the document has since moved past, with that approval's document and mark; a row with no
// standing approval, and a carried copy of another root's gate, leave no record.
func TestApprovalRouteReadmissionRecordsTheSpecRowAtAnyVersion(t *testing.T) {
	t.Run("approved at its latest version", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.outOfWorkflowApproval(2)
		approved := r.seq
		r.todo(ownRoot)
		r.wantRecord(record.ClearedSpecApproval{Issue: ownRoot, ArtifactID: ownDocument, Version: 2, LatestVersion: 2, Mark: mark(approved)})
		r.wantApproval(r.architect(ownRoot), "dispatch://ACME-2694/spec", 2, ownWords(ownRoot, 2))
	})
	t.Run("approved below its latest version", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.outOfWorkflowApproval(2)
		approved := r.seq
		r.review(ownRoot, ownDocument, intake.DispatchArtifactVersion, 3)
		r.todo(ownRoot)
		r.wantRecord(record.ClearedSpecApproval{Issue: ownRoot, ArtifactID: ownDocument, Version: 2, LatestVersion: 3, Mark: mark(approved)})
		r.wantApproval(r.architect(ownRoot), "dispatch://ACME-2694/spec", 2, ownWords(ownRoot, 2))
	})
	t.Run("a changes request on its spec", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.outOfWorkflow(2)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactChangesRequested, 2)
		r.todo(ownRoot)
		if kept := r.clearedSpecApproval(ownRoot); kept != nil {
			t.Fatalf("the re-admission's record = %+v, want none: no approval stood", *kept)
		}
		r.wantNone(r.architect(ownRoot))
	})
	t.Run("a carried copy of another root's gate", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.carried(record.DesignGate{LatestVersion: 12, ApprovedVersion: new(12), ApprovedMark: mark(r.next())})
		r.lingering(carriedRoot, "done")
		r.todo(carriedRoot)
		if kept := r.clearedSpecApproval(carriedRoot); kept != nil {
			t.Fatalf("the re-admission's record = %+v, want none: the copy holds another root's approval", *kept)
		}
		r.wantNone(r.architect(carriedRoot))
	})
}

// wantRecord holds the re-admission's record of want.Issue to want.
func (r *approvalRig) wantRecord(want record.ClearedSpecApproval) {
	r.t.Helper()
	got := r.clearedSpecApproval(want.Issue)
	if got == nil || got.Issue != want.Issue || got.ArtifactID != want.ArtifactID || got.Version != want.Version ||
		got.LatestVersion != want.LatestVersion || got.Mark.Seq != want.Mark.Seq || !got.Mark.At.Equal(want.Mark.At) {
		r.t.Fatalf("the re-admission's record of %s = %+v, want %+v", want.Issue, got, want)
	}
}

// A carry records the root whose gate it copied, and a carry out of a carried root records the
// root that holds the document: the one its own row names, or none where that row names none. A
// carry of no gate records no root.
func TestApprovalRouteCarryRecordsTheOtherRoot(t *testing.T) {
	approved := record.DesignGate{LatestVersion: 12, ApprovedVersion: new(12), ApprovedMark: mark(500)}
	t.Run("one carry", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.carried(approved)
		r.wantHandedOver(carriedRoot, record.HandedOverRoot{Issue: carriedRoot, Generation: 2, GateCarried: true, FromRoot: otherRoot})
	})
	t.Run("a chain of two carries", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.carried(approved)
		r.lingering(carriedRoot, "done")
		r.handOver(carriedRoot, chainedRoot)
		r.wantHandedOver(chainedRoot, record.HandedOverRoot{Issue: chainedRoot, Generation: 2, GateCarried: true, FromRoot: otherRoot})
	})
	t.Run("a chain through a row that names no root", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.carried(approved)
		r.unnamedFromRoot(carriedRoot)
		r.lingering(carriedRoot, "done")
		r.handOver(carriedRoot, chainedRoot)
		r.wantHandedOver(chainedRoot, record.HandedOverRoot{Issue: chainedRoot, Generation: 2, GateCarried: true})
	})
	t.Run("a carry of no gate", func(t *testing.T) {
		r := newApprovalRig(t, specGated)
		r.lingering(otherRoot, "done")
		r.handOver(otherRoot, carriedRoot)
		r.wantHandedOver(carriedRoot, record.HandedOverRoot{Issue: carriedRoot, Generation: 2})
	})
}

// wantHandedOver holds the hand-over row of key to want.
func (r *approvalRig) wantHandedOver(key string, want record.HandedOverRoot) {
	r.t.Helper()
	if got := r.handedOverRoot(key); got == nil || *got != want {
		r.t.Fatalf("the hand-over row of %s = %+v, want %+v", key, got, want)
	}
}

// The route answers for the claim its secret authenticates and for no other: a root whose spec no
// person approved is answered none beside a root whose approval stands, another claim's token with
// this claim's secret is refused as any wrong secret is, and a body that names an issue is refused
// before anything is read.
func TestApprovalRouteAnswersOnlyItsOwnClaim(t *testing.T) {
	r := newApprovalRig(t, specGated)
	r.root(ownRoot, ownDocument, 3)
	r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
	r.root(secondRoot, secondDocument, 1)
	own, second := r.architect(ownRoot), r.architect(secondRoot)

	r.wantApproval(own, "dispatch://ACME-2694/spec", 3, ownWords(ownRoot, 3))
	r.wantNone(second)

	borrowed := approvalBody(own)
	borrowed.Secret = second.secret
	wantRefusal(t, r.request(http.MethodPost, "/legion/v1/claims/approval", borrowed, nil), claim.InvalidSecret.Status, claim.InvalidSecret.Message)

	naming := map[string]any{"claimToken": second.token, "sessionId": second.session, "secret": second.secret, "issue": ownRoot}
	if recorder := r.request(http.MethodPost, "/legion/v1/claims/approval", naming, nil); recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "unknown field") {
		t.Fatalf("a body naming an issue = %d %s, want 400 for the unknown field", recorder.Code, recorder.Body)
	}
	for _, missing := range []string{"claimToken", "sessionId", "secret"} {
		body := map[string]any{"claimToken": second.token, "sessionId": second.session, "secret": second.secret}
		delete(body, missing)
		wantRefusal(t, r.request(http.MethodPost, "/legion/v1/claims/approval", body, nil), http.StatusBadRequest, missing+" is required")
	}
}

// The answer of a standing approval, as the route writes it.
func TestApprovalRouteStandingAnswerGolden(t *testing.T) {
	answer, err := approvalAnswer(&record.StandingApproval{Root: ownRoot, Spec: ownRoot, ArtifactID: ownDocument, Version: 24})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	golden(t, "claim-approval.json", answer)
}

// The answer where no approval stands: `approval` is null, never absent.
func TestApprovalRouteNoneAnswerGolden(t *testing.T) {
	answer, err := approvalAnswer(nil)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	golden(t, "claim-approval-none.json", answer)
}
