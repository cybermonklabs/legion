package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The record carry (R14): a version whose record digest equals the digest of the approved
// version, as a paragraph moved in the text before a requirement leaves it, carries the approval,
// its artifact.approved before the version's artifact.version, and a version whose digest moved
// carries nothing.

// requireApprovalCarriedByRecord fails unless version number of the document artifactID answers the
// digest version approved does, and alice's approval stands at number as the record carry of her
// approval of approved: the newest review, approved, in her name, with the record carry's reason
// (docs.RecordCarryReason) and carried from approved.
func requireApprovalCarriedByRecord(t *testing.T, handler http.Handler, database *store.Store, artifactID string, approved, number int) {
	t.Helper()
	if got, want := versionDigestOf(t, database, artifactID, number), versionDigestOf(t, database, artifactID, approved); got != want {
		t.Errorf("version %d of %s answers the digest %s, want the approved version %d's, %s", number, artifactID, got, approved, want)
	}
	reviews := listReviews(t, handler, artifactID)
	if len(reviews) == 0 {
		t.Fatalf("%s holds no review, want the record carry at version %d", artifactID, number)
	}
	carried := reviews[0]
	if carried.Version != number || carried.State != "approved" || carried.Actor.ID != "alice" || carried.Reason == nil ||
		*carried.Reason != docs.RecordCarryReason(approved) || carried.CarriedFromVersion == nil || *carried.CarriedFromVersion != approved {
		t.Errorf("the newest review of %s reads %+v (reason %v), want alice's approval of version %d carried from version %d with the reason %q",
			artifactID, carried, carried.Reason, number, approved, docs.RecordCarryReason(approved))
	}
}

// approvedBeforeVersionStatement reads the id and the seq of the newest artifact.approved naming
// version $2 of the document $1 and those of the newest artifact.version naming it.
const approvedBeforeVersionStatement = `
	select approved.id, approved.seq, versioned.id, versioned.seq
	from (select e.id, e.seq from events e
	      where e.type = 'artifact.approved' and e.payload->>'artifact_id' = $1 and (e.payload->>'version')::int = $2
	      order by e.id desc limit 1) approved,
	     (select e.id, e.seq from events e
	      where e.type = 'artifact.version' and e.payload->>'artifact_id' = $1
	        and (case jsonb_typeof(e.payload->'version') when 'object' then e.payload->'version'->>'number' else e.payload->>'version' end)::int = $2
	      order by e.id desc limit 1) versioned`

// requireApprovedBeforeVersion fails unless the artifact.approved of the carry to version number of
// artifactID comes before the version's artifact.version, in event id and in seq (R14).
func requireApprovedBeforeVersion(t *testing.T, database *store.Store, artifactID string, number int) {
	t.Helper()
	var approvedID, approvedSeq, versionID, versionSeq int64
	if err := database.Pool.QueryRow(context.Background(), approvedBeforeVersionStatement, artifactID, number).Scan(
		&approvedID, &approvedSeq, &versionID, &versionSeq); err != nil {
		t.Fatalf("read the artifact.approved and the artifact.version of version %d of %s: %v", number, artifactID, err)
	}
	if approvedID >= versionID || approvedSeq >= versionSeq {
		t.Errorf("the carry to version %d of %s appended artifact.approved at id %d, seq %d and the version's artifact.version at id %d, seq %d: want the approval first in both",
			number, artifactID, approvedID, approvedSeq, versionID, versionSeq)
	}
}

// documentName is the name of the document artifactID, which an upload over it names.
func documentName(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	var name string
	if err := database.Pool.QueryRow(context.Background(), `select name from artifacts where id = $1::uuid`, artifactID).Scan(&name); err != nil {
		t.Fatalf("read the name of %s: %v", artifactID, err)
	}
	return name
}

// recordTableSpec is the spec of the record carry table's issue, CORE-1: the memo callout, a
// criterion with a requirement under it, three paragraphs, a callout holding a second requirement,
// a list and a decision with a reading. An ask block joins it once the issue is written.
const recordTableSpec = ":::callout{#memo}\nWe build the urgent list.\n:::\n\n" +
	":::criterion{#k1}\nI want the urgent asks first.\n:::\n\n" +
	":::requirement{#r1 parent=\"dispatch://CORE-1/spec#b-k1\"}\nAn urgent ask comes first.\n:::\n\n" +
	"A first paragraph.\n\n" +
	"::::callout{#box kind=\"note\" title=\"\"}\n:::requirement{#r2}\nAn ask keeps its urgency.\n:::\n::::\n\n" +
	"A second paragraph.\n\n" +
	"- one item\n- two items\n\n" +
	"A third paragraph.\n\n" +
	"::::decision{#d1}\nWe ship on Friday.\n\n:::reading{#d1-r1}\nThe agent reads it as Friday.\n:::\n::::\n"

// requireAskOfBlock fails unless the document artifactID holds the ask of its block blockID, as the
// carry reads a document's block asks (refs.AsksOfDocumentSQL).
func requireAskOfBlock(t *testing.T, database *store.Store, artifactID, blockID string) {
	t.Helper()
	var held bool
	if err := database.Pool.QueryRow(context.Background(), `
		select exists (select 1 from asks a where `+refs.AsksOfDocumentSQL("a", "$1::uuid")+` and a.block_id = $2)`,
		artifactID, blockID).Scan(&held); err != nil || !held {
		t.Fatalf("read the ask of block %s of %s (%v): held %t, want the ask its settlement opened", blockID, artifactID, err, held)
	}
}

// recordTableAsk is the ask block the table's spec takes through the edits route; the room's
// settlement opens its ask.
const recordTableAsk = ":::ask{#a1 urgency=\"high\" multiple=\"false\"}\nWhich transport?\n\n- REST: Matches the platform\n- gRPC: Adds streaming\n:::\n"

// recordTableChecks is the table's Verification document: c1 serving serves, c2 serving r1.
func recordTableChecks(serves string) string {
	return ":::check{#c1 serves=\"dispatch://CORE-1/spec#b-" + serves + "\" surface=\"cli\" run=\"go test ./...\"}\nThe list reads urgent asks first.\n:::\n\n" +
		":::check{#c2 serves=\"dispatch://CORE-1/spec#b-r1\" surface=\"cli\" run=\"go test ./...\"}\nThe list reads an ask's urgency back.\n:::\n"
}

// recordTable is the server and the issue the rows of TestTheRecordCarryCarriesOnlyWhereTheDigestHolds
// share.
type recordTable struct {
	rig                      *digestRig
	documents                *docs.Service
	spec, verification, plan decisionDocument
}

// blockIDOfText is the id of the block of the document artifactID whose text, trimmed, is text.
func blockIDOfText(t *testing.T, handler http.Handler, artifactID, text string) string {
	t.Helper()
	for held, id := range blockIDsByText(t, handler, artifactID) {
		if strings.TrimSpace(held) == text {
			return id
		}
	}
	t.Fatalf("the document %s holds no block reading %q", artifactID, text)
	return ""
}

// blockID is the id of the spec's block whose text is text.
func (f *recordTable) blockID(t *testing.T, text string) string {
	t.Helper()
	return blockIDOfText(t, f.rig.handler, f.spec.id, text)
}

// newest is the newest version of each of the issue's spec, Verification document and plan.
func (f *recordTable) newest(t *testing.T) map[string]int {
	t.Helper()
	return map[string]int{
		"spec":         newestVersionOf(t, f.rig.database, f.spec.id),
		"verification": newestVersionOf(t, f.rig.database, f.verification.id),
		"plan":         newestVersionOf(t, f.rig.database, f.plan.id),
	}
}

// settleSpec has alice's browser make edit in the spec's fragment of the issue's room, waits until
// the room stores it, and settles the issue's documents on the test's goroutine
// (docs.Service.SettleIssueDocuments): the table's server settles after an hour, so a resumption
// would only arm that hour's timer.
func (f *recordTable) settleSpec(t *testing.T, edit func(*pmdoc.Node) error) {
	t.Helper()
	peer := connectBrowserPeer(t, f.rig.handler, f.documents, f.spec.id)
	stored := storedUpdateCount(t, f.rig.database, f.spec.id)
	peer.edit(t, edit)
	peer.barrier(t)
	awaitStoredUpdates(t, f.rig.database, f.spec.id, stored+1)
	settleIssue(t, f.documents, "CORE-1")
	awaitSettled(t, f.rig.database, f.spec.id)
}

// withoutBlockReading is a browser edit that takes out the top-level block of the document whose
// first text run is text.
func withoutBlockReading(text string) func(*pmdoc.Node) error {
	return func(tree *pmdoc.Node) error {
		for index, child := range tree.Children {
			if len(child.Children) > 0 && child.Children[0].Text == text {
				tree.Children = slices.Delete(tree.Children, index, index+1)
				return nil
			}
		}
		return fmt.Errorf("the document holds no block reading %q", text)
	}
}

// recordCarryRow is one write of the record carry's table: its name, the query whose approval it
// is weighed against, whether its version must carry that approval by its record, the documents it
// writes a version of, and the write itself.
type recordCarryRow struct {
	name    string
	query   string
	carries bool
	writes  []string
	write   func(t *testing.T, f *recordTable)
}

// TestTheRecordCarryCarriesOnlyWhereTheDigestHolds holds the record carry to its claim through
// the real routes (R14): over CORE-1, born converted, whose spec holds the memo callout, a criterion
// with a requirement under it, paragraphs, a callout holding a requirement, a list, a decision with
// a reading and an ask block, beside a Verification document of two checks and a plan, each row
// approves its query, makes one write - through the edits route, an upload, a settlement of a
// browser's edit, or the node route's PATCH - and reads the version it wrote once the fill has run.
// A version carries alice's approval by its record only where its digest equals the approved one,
// never over a moved digest; the rows that must carry - a paragraph moved before a requirement,
// edited, added after the last requirement and removed - carry it, its artifact.approved before the
// version's artifact.version, and read approved at latest_version; the rows that must not - a
// requirement moved before a paragraph, a requirement's word, a container holding a requirement
// moved, a paragraph moved before the memo, a paragraph retyped into a requirement, a requirement's
// parent moved by the node route, a requirement written into the plan, a check retargeted - move the
// digest, carry nothing and read stale. A block the node route moves into another document's block
// writes no version, and the approval stands. Each write versions only the documents its row names:
// a requirement written into the plan writes a version of the spec at its drain, and every other
// write versions no other query. No mark of the issue's queries stands after any row. After the
// table the store census reads no difference.
func TestTheRecordCarryCarriesOnlyWhereTheDigestHolds(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	createReferenceAPIProject(t, handler, "CORE")
	documents := deps.Docs.(*docs.Service)
	issue := createDigestIssue(t, handler, database, "CORE", "The record carry's table", recordTableSpec, false)
	if issue.Key != "CORE-1" {
		t.Fatalf("the table's issue is %s, want CORE-1, which its spec's references name", issue.Key)
	}
	f := &recordTable{rig: &digestRig{handler: handler, database: database, documents: documents, issue: issue}, documents: documents}
	f.spec = readDecisionDocument(t, database, issue.Key, "spec")
	f.rig.spec = f.spec
	// The ask block's ask is opened by the room's settlement (docs' reconcileAskBlocks), which this
	// server runs after an hour: it is settled here, so every version the table writes holds an ask
	// block that agrees with its ask, which every carry asks first (asksAgreeWithTheirNodes). A block
	// whose ask no settlement has opened carries nothing, and no row would carry.
	editThroughTheArtifactRoute(t, handler, f.spec.id, []map[string]any{{"op": "insert", "after": "end", "markdown": recordTableAsk}})
	settleIssue(t, documents, issue.Key)
	requireAskOfBlock(t, database, f.spec.id, "a1")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "verification.md", recordTableChecks("r1"))
	uploadDocument(t, handler, owner, "plan.md", ":::plan_task{#t1 serves=\"dispatch://CORE-1/spec#b-r1\"}\nBuild the order.\n:::\n")
	f.verification = readDecisionDocument(t, database, issue.Key, "verification-md")
	f.plan = readDecisionDocument(t, database, issue.Key, "plan-md")
	publishDesignGates(t, handler, issue.Key, []issueDesignGate{
		{Step: model.GatedStepCriteria, ArtifactID: f.verification.id},
		{Step: model.GatedStepSpec, ArtifactID: f.spec.id},
	})
	for _, document := range []decisionDocument{f.spec, f.verification, f.plan} {
		awaitDigestsFilled(t, database, document.id)
	}
	specName := documentName(t, database, f.spec.id)

	edit := func(ops ...map[string]any) func(*testing.T, *recordTable) {
		return func(t *testing.T, f *recordTable) {
			t.Helper()
			editThroughTheArtifactRoute(t, f.rig.handler, f.spec.id, ops)
		}
	}
	rows := []recordCarryRow{
		{name: "a paragraph moved before a requirement", query: "spec", carries: true, writes: []string{"spec"}, write: func(t *testing.T, f *recordTable) {
			edit(map[string]any{"op": "move", "block": f.blockID(t, "A first paragraph."), "after": "block:k1"})(t, f)
		}},
		{name: "a paragraph edited", query: "spec", carries: true, writes: []string{"spec"},
			write: edit(map[string]any{"op": "replace", "find": "A second paragraph.", "with": "A second paragraph, edited."})},
		{name: "a requirement moved before a paragraph", query: "spec", writes: []string{"spec"},
			write: edit(map[string]any{"op": "move", "block": "r1", "after": "block:k1"})},
		{name: "a requirement's word edited", query: "spec", writes: []string{"spec"},
			write: edit(map[string]any{"op": "replace", "find": "An urgent ask comes first.", "with": "An urgent ask always comes first."})},
		{name: "a paragraph added after the last requirement", query: "spec", carries: true, writes: []string{"spec"}, write: func(t *testing.T, f *recordTable) {
			_, text := documentTexts(t, f.rig.handler, f.spec.id)
			uploadAs(t, f.rig.handler, "CORE-1", specName, text+"\nA paragraph after the last requirement.\n", "alice")
		}},
		{name: "a paragraph removed", query: "spec", carries: true, writes: []string{"spec"}, write: func(t *testing.T, f *recordTable) {
			f.settleSpec(t, withoutBlockReading("A paragraph after the last requirement."))
		}},
		{name: "a paragraph the node route moves under a block of the plan", query: "spec", write: func(t *testing.T, f *recordTable) {
			moveNodeUnder(t, f.rig.handler, f.rig.database, "the first paragraph", f.spec.address(f.blockID(t, "A first paragraph.")), f.plan.address("t1"))
		}},
		{name: "a container holding a requirement moved before another", query: "spec", writes: []string{"spec"},
			write: edit(map[string]any{"op": "move", "block": "box", "after": "block:memo"})},
		{name: "a paragraph moved before the memo callout", query: "spec", writes: []string{"spec"}, write: func(t *testing.T, f *recordTable) {
			edit(map[string]any{"op": "move", "block": f.blockID(t, "A second paragraph, edited."), "before": "block:memo"})(t, f)
		}},
		{name: "a paragraph retyped into a requirement", query: "spec", writes: []string{"spec"}, write: func(t *testing.T, f *recordTable) {
			edit(map[string]any{"op": "retype", "block": f.blockID(t, "A third paragraph."), "type": "requirement"})(t, f)
		}},
		{name: "a requirement's parent moved by the node route", query: "spec", writes: []string{"spec"}, write: func(t *testing.T, f *recordTable) {
			moveNodeUnder(t, f.rig.handler, f.rig.database, "r1", f.spec.address("r1"), f.spec.address("box"))
		}},
		{name: "a requirement written into the plan document", query: "spec", writes: []string{"plan", "spec"}, write: func(t *testing.T, f *recordTable) {
			editThroughTheArtifactRoute(t, f.rig.handler, f.plan.id, []map[string]any{{"op": "insert", "after": "end",
				"markdown": ":::requirement{#r9}\nA requirement the plan states.\n:::\n"}})
		}},
		{name: "a check retargeted", query: "verification", writes: []string{"verification"}, write: func(t *testing.T, f *recordTable) {
			uploadAs(t, f.rig.handler, "CORE-1", "verification.md", recordTableChecks("r2"), "alice")
		}},
	}
	for _, row := range rows {
		target, step := f.spec, model.GatedStepSpec
		if row.query == "verification" {
			target, step = f.verification, model.GatedStepCriteria
		}
		gate := model.AskApprovalDocument{ArtifactID: target.id}
		approveStanding(t, f.rig, issue.Key, step, target.id, row.name)
		approved := newestVersionOf(t, database, target.id)
		reviews := len(listReviews(t, handler, target.id))
		before := f.newest(t)
		row.write(t, f)
		for _, document := range []decisionDocument{f.spec, f.verification, f.plan} {
			awaitDigestsFilled(t, database, document.id)
		}
		after := f.newest(t)
		for name, number := range before {
			want := number
			if slices.Contains(row.writes, name) {
				want++
			}
			if after[name] != want {
				t.Errorf("%s: the %s of CORE-1 stands at version %d, want %d", row.name, name, after[name], want)
			}
		}
		requireNoQueryMark(t, database, row.name, issue.Key)
		latest := after[row.query]
		if latest == approved {
			if got := len(listReviews(t, handler, target.id)); got != reviews {
				t.Errorf("%s: the %s holds %d reviews, want the %d before the write, which wrote no version of it", row.name, row.query, got, reviews)
			}
			requireGate(t, f.rig, issue.Key, step, gate, true, row.name)
			continue
		}
		unmoved := versionDigestOf(t, database, target.id, latest) == versionDigestOf(t, database, target.id, approved)
		newest := listReviews(t, handler, target.id)[0]
		carried := newest.Version == latest && newest.Reason != nil && docs.IsRecordCarry(*newest.Reason)
		if carried && !unmoved {
			t.Errorf("%s: the record carry carried alice's approval to version %d of the %s, whose digest moved from version %d's", row.name, latest, row.query, approved)
		}
		if !row.carries {
			if unmoved {
				t.Errorf("%s: version %d of the %s answers the approved version %d's digest, want the write to move the record", row.name, latest, row.query, approved)
			}
			if carried {
				t.Errorf("%s: version %d of the %s holds the record carry %+v, want none", row.name, latest, row.query, newest)
			}
			requireGate(t, f.rig, issue.Key, step, gate, false, row.name)
			continue
		}
		requireApprovalCarriedByRecord(t, handler, database, target.id, approved, latest)
		requireApprovedBeforeVersion(t, database, target.id, latest)
		requireGate(t, f.rig, issue.Key, step, gate, true, row.name)
		if read := readApproval(t, handler, target.id); read.Approval == nil || read.Approval.Version == nil || *read.Approval.Version != latest || read.Approval.LatestVersion != latest {
			t.Errorf("%s: the approval of the %s reads %+v, want approved at version %d, its latest version", row.name, row.query, read.Approval, latest)
		}
	}
	requireStoreCensusZero(t, database, "after the record carry's table")
}

// recordLaterSpec is the spec of the issues TestARecordCarryWaitsForTheRequestOrTheReopen writes: a
// criterion and a requirement, which the spec query takes, and a paragraph it does not take.
const recordLaterSpec = ":::criterion{#k1}\nI want the urgent list.\n:::\n\n:::requirement{#r1}\nAn urgent ask comes first.\n:::\n\nA paragraph after it.\n"

// restoredAndMoved is a browser edit of recordLaterSpec's text as a write that moved its
// requirement's word left it: the requirement's words put back, and the paragraph moved before the
// requirement, so the record is the approved version's and the text is not.
func restoredAndMoved(tree *pmdoc.Node) error {
	if len(tree.Children) != 3 || len(tree.Children[1].Children) == 0 || len(tree.Children[1].Children[0].Children) == 0 {
		return fmt.Errorf("the spec holds %d top-level blocks, want the criterion, the requirement and the paragraph", len(tree.Children))
	}
	tree.Children[1].Children[0].Children[0].Text = "An urgent ask comes first."
	tree.Children[1], tree.Children[2] = tree.Children[2], tree.Children[1]
	return nil
}

// TestARecordCarryWaitsForTheRequestOrTheReopen holds the record carry of a version no write's rule
// carried to (R14): a version whose record digest is the approved one carries the approval, in the
// approver's name, at the first moment its record is known. request: CORE-1's spec is approved, an
// upload appends 2,001 list items, which its write leaves to the block-index reconciliation, so the
// write states no record and its rule carries nothing (docs.StatedWhole needs the version stated);
// the reconciliation's pass states it with the approved record's digest and carries the approval
// to it. That pass carried nothing before INFRA-1098's architect decided, on 2026-11-26, that
// R15's first pass carries such a version (dispatch://INFRA-1253/comment/dfd12b58-03ff-4f84-aa01-3020f8e7651b):
// the newest review is alice's, carried from her approval with the record carry's reason, and the
// approval request then finds the latest version approved and opens no ask, its answer and the
// approval read approved at the latest version. reopen: CORE-1's spec is approved, a
// word of its requirement moves the record and an approval request opens on that version; the issue
// closes, alice's browser puts the word back and moves the paragraph before the requirement inside
// the close margin, and the settlement on the closed issue writes that version and carries nothing.
// The reopen follows it (docs.Service.FollowClosedIssueVersions) and carries the approval by the
// version's stored record digest, answering the restored request in alice's name. Its server
// settles a room 20 ms after its last update, as every closed-issue test's does
// (newClosedSettlementHandler): the resumption it runs (ResumeOwedSettlementsForTest) arms a
// room's settle timer, which a server settling after an hour would hold past the test's deadline.
func TestARecordCarryWaitsForTheRequestOrTheReopen(t *testing.T) {
	t.Parallel()
	t.Run("request", func(t *testing.T) {
		ctx := context.Background()
		handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
		createReferenceAPIProject(t, handler, "CORE")
		issue := createDigestIssue(t, handler, database, "CORE", "A spec past the bound", recordLaterSpec, false)
		spec := readDecisionDocument(t, database, issue.Key, "spec")
		awaitDigestsFilled(t, database, spec.id)
		approved := reviewDocument(t, handler, spec.id, "alice", "approved", "").Version
		_, text := documentTexts(t, handler, spec.id)
		uploadAs(t, handler, issue.Key, documentName(t, database, spec.id), text+"\n"+strings.Repeat("- an item\n", 2001), "alice")
		deferred := newestVersionOf(t, database, spec.id)
		if deferred != approved+1 {
			t.Fatalf("the upload past the bound wrote version %d of %s, want %d", deferred, spec.refKey, approved+1)
		}
		if reviews := listReviews(t, handler, spec.id); len(reviews) == 0 || reviews[0].Version != approved {
			t.Fatalf("the reviews of %s read %+v after the upload past the bound, want alice's approval of version %d newest: the write states no record of its version", spec.refKey, reviews, approved)
		}
		if _, err := deps.Docs.(*docs.Service).ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
			t.Fatalf("reconcile the block index: %v", err)
		}
		if got, want := versionDigestOf(t, database, spec.id, deferred), versionDigestOf(t, database, spec.id, approved); got != want {
			t.Fatalf("version %d of %s, whose list items alone are new, answers the digest %s once its pass stated it, want the approved version %d's, %s", deferred, spec.refKey, got, approved, want)
		}
		requireApprovalCarriedByRecord(t, handler, database, spec.id, approved, deferred)
		requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+spec.id+"/approval-requests", map[string]any{"actor": sessionActor()})
		if requested.Code != http.StatusOK {
			t.Fatalf("request approval of %s: status=%d body=%s, want 200: the spec stands approved by its digest", spec.refKey, requested.Code, requested.Body.String())
		}
		answer := decodeBody[struct {
			Ask      json.RawMessage `json:"ask"`
			Version  int             `json:"version"`
			Approval struct {
				State         string `json:"state"`
				Version       *int   `json:"version"`
				LatestVersion int    `json:"latest_version"`
			} `json:"approval"`
		}](t, requested)
		if string(answer.Ask) != "null" || answer.Version != deferred || answer.Approval.State != "approved" || answer.Approval.Version == nil ||
			*answer.Approval.Version != deferred || answer.Approval.LatestVersion != deferred {
			t.Errorf("the approval request of %s answers %+v (ask %s), want no ask and approved at version %d, its latest", spec.refKey, answer, answer.Ask, deferred)
		}
		requireApprovalCarriedByRecord(t, handler, database, spec.id, approved, deferred)
		if read := readApproval(t, handler, spec.id); read.Approval == nil || read.Approval.Version == nil || *read.Approval.Version != deferred || read.Approval.LatestVersion != deferred {
			t.Errorf("the approval of %s reads %+v, want approved at version %d, its latest version", spec.refKey, read.Approval, deferred)
		}
	})
	t.Run("reopen", func(t *testing.T) {
		ctx := context.Background()
		handler, database, deps := newTestServer(t, testServerOptions{browserPeers: true})
		documents := deps.Docs.(*docs.Service)
		createReferenceAPIProject(t, handler, "CORE")
		issue := createDigestIssue(t, handler, database, "CORE", "A spec restored while closed", recordLaterSpec, false)
		spec := readDecisionDocument(t, database, issue.Key, "spec")
		awaitDigestsFilled(t, database, spec.id)
		approved := reviewDocument(t, handler, spec.id, "alice", "approved", "").Version
		editThroughTheArtifactRoute(t, handler, spec.id, []map[string]any{{"op": "replace", "find": "An urgent ask comes first.", "with": "An urgent ask comes second."}})
		awaitDigestsFilled(t, database, spec.id)
		moved := newestVersionOf(t, database, spec.id)
		if moved != approved+1 || versionDigestOf(t, database, spec.id, moved) == versionDigestOf(t, database, spec.id, approved) {
			t.Fatalf("the edit of the requirement's word wrote version %d of %s, want version %d whose digest moved", moved, spec.refKey, approved+1)
		}
		request := requestApproval(t, handler, spec.id)
		peer := connectBrowserPeer(t, handler, documents, spec.id)
		closeIssueThroughPatch(t, handler, database, issue.Key)
		awaitSettled(t, database, spec.id)
		stored := storedUpdateCount(t, database, spec.id)
		peer.edit(t, restoredAndMoved)
		peer.barrier(t)
		awaitStoredUpdates(t, database, spec.id, stored+1)
		if err := documents.ResumeOwedSettlementsForTest(ctx, 0); err != nil {
			t.Fatalf("settle the closed issue's spec: %v", err)
		}
		awaitSettled(t, database, spec.id)
		awaitDigestsFilled(t, database, spec.id)
		closed := newestVersionOf(t, database, spec.id)
		if closed != moved+1 {
			t.Fatalf("the settlement on the closed issue left version %d of %s newest, want %d", closed, spec.refKey, moved+1)
		}
		if got, want := versionDigestOf(t, database, spec.id, closed), versionDigestOf(t, database, spec.id, approved); got != want {
			t.Fatalf("version %d of %s, its requirement's word put back and its paragraph moved, answers the digest %s, want the approved version %d's, %s", closed, spec.refKey, got, approved, want)
		}
		if reviews := listReviews(t, handler, spec.id); len(reviews) == 0 || reviews[0].Version != approved {
			t.Fatalf("the reviews of %s read %+v after the closed issue's settlement, want alice's approval of version %d newest: such a settlement carries nothing", spec.refKey, reviews, approved)
		}
		if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
			t.Fatalf("reopen %s: status=%d body=%s", issue.Key, reopened.Code, reopened.Body.String())
		}
		if err := documents.ResumeOwedSettlementsForTest(ctx, 0); err != nil {
			t.Fatalf("settle what the reopen left owed: %v", err)
		}
		awaitSettled(t, database, spec.id)
		requireApprovalCarriedByRecord(t, handler, database, spec.id, approved, closed)
		if read := readApproval(t, handler, spec.id); read.Approval == nil || read.Approval.Version == nil || *read.Approval.Version != closed || read.Approval.LatestVersion != closed {
			t.Errorf("the approval of %s reads %+v after the reopen, want approved at version %d, its latest version", spec.refKey, read.Approval, closed)
		}
		// The request's approval.approved is the approval that stood before it was opened; whether the
		// carry answered it is the ask's own state and answer.
		if answered := readBlockAsk(t, handler, request.ID); answered.State != "answered" || answered.Answer == nil || answered.Answer.User != "alice" ||
			answered.Approval == nil || answered.Approval.Version != closed {
			t.Errorf("the restored approval request %s is %q answered by %+v at %+v, want it answered in alice's name at version %d",
				request.ID, answered.State, answered.Answer, answered.Approval, closed)
		}
	})
}
