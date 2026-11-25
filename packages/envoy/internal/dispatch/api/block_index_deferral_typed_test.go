package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAVersionAboveTheBoundListsItsUnreachedChecksAndRequirementsAtOnce counts the requirements
// and checks the reads list at once: in each deferral where the tree the write wrote
// renders as the version's markdown - a document above the bound, an issue room above the bound
// and a room left owed - the write that accepts a version writes each requirement, check and plan
// task it holds, with its text value, its part_of, its row, its serves and, in a room, its
// document, and no block of the version that is neither typed nor an ancestor of one; the spec,
// the Verification and the plan are written by uploads, and in each deferral the spec and the
// Verification by edits too, which defer past the bound as an upload does in the two deferrals by
// size; the issue create writes a spec in the two deferrals by size alone, since a create cannot
// leave a room owed (writeAcceptedVersions). Right after each write, with no reconciliation run,
// each reads in the coverage read and in its view of the tree read, after its ranked siblings with
// no rank, the census reads no row without its node and tuples, and both reads name the document
// owed; once the reconciliation runs, each holds one of each, its first rank among them, nothing is
// owed and the store census reads zero.
func TestAVersionAboveTheBoundListsItsUnreachedChecksAndRequirementsAtOnce(t *testing.T) {
	for _, deferral := range acceptedDeferrals {
		t.Run(deferral.name, func(t *testing.T) {
			s := writeAcceptedVersions(t, deferral)
			issue := s.fixture.issue.Key
			requireCoverageListsThem(t, s.handler, issue, s.fixture.written)
			requireInTheViews(t, s.handler, issue, s.specKey, deferral.converted, s.deferred())
			requireWrittenAtOnce(t, s.database, deferral.converted, s.deferred())
			requireCensusRowsAgree(t, s.database, "right after the deferred writes", "block", "block_ref")
			if s.created != nil {
				requireCoverageListsThem(t, s.handler, s.created.issue.Key, s.created.written)
				requireWrittenAtOnce(t, s.database, deferral.converted, s.created.written)
				requireInTheViews(t, s.handler, s.created.issue.Key, blockKeyOf(t, s.database, s.created.issue.PrimaryArtifactID), deferral.converted, s.created.written)
			}
			if t.Failed() {
				return
			}
			s.reconcile(t)
			requireIndexedOnce(t, s.database, s.deferred())
			requireNothingOwed(t, s.handler, issue, "after the reconciliation")
			if s.created != nil {
				requireIndexedOnce(t, s.database, s.created.written)
				requireNothingOwed(t, s.handler, s.created.issue.Key, "after the reconciliation")
			}
			requireCoverageListsThem(t, s.handler, issue, s.fixture.written)
			requireStoreCensusZero(t, s.database, "after the reconciliation of "+deferral.name)
		})
	}
}

// TestARequirementUnderACriterionInAListIsReachedAtOnce: in each rendering deferral, a requirement
// two levels under a criterion inside a list (criterionChain) and a check under a heading's list in
// the Verification (headingCheck): right after the write, with no reconciliation run, the spec view
// and the verification view of the tree read answer the requirement and the check, and so does the
// coverage read over the tuples, which takes its requirements by the walk from the issue (the
// coverage route takes them from the spec's markdown); the criterion, the ancestor the part_of of
// r6 names, holds its node, its text value, its part_of and its row and no rank, so every part_of
// of the chain names a node the store holds; and
// after the reconciliation each holds one of each and its first rank.
func TestARequirementUnderACriterionInAListIsReachedAtOnce(t *testing.T) {
	for _, deferral := range acceptedDeferrals {
		t.Run(deferral.name, func(t *testing.T) {
			s := writeAcceptedVersions(t, deferral)
			issue := s.fixture.issue.Key
			var chain []writtenBlock
			for _, block := range s.fixture.written {
				if slices.ContainsFunc(criterionChain, func(held acceptedBlock) bool { return held.id == block.id }) || block.id == headingCheck.id {
					chain = append(chain, block)
				}
			}
			requireTwinListsRequirement(t, s.deps, s.database, issue, "r5")
			requireInTheViews(t, s.handler, issue, s.specKey, deferral.converted, chain)
			requireWrittenAtOnce(t, s.database, deferral.converted, chain)
			t.Logf("before the reconciliation: %s", partOfChain(t, s.database, s.specKey+"#r5"))
			t.Logf("before the reconciliation: %s", partOfChain(t, s.database, blockKeyOf(t, s.database, s.fixture.documentNamed(t, "verification.md"))+"#c9"))
			report, err := graphcensus.StoreCensus(context.Background(), s.database.Pool.Config().ConnString(), false, docs.ReturnDeclined)
			if err != nil {
				t.Fatalf("take the store census: %v", err)
			}
			var printed strings.Builder
			report.Write(&printed)
			t.Logf("the store census before the reconciliation reads %d differences:\n%s", report.Differences(), printed.String())
			if t.Failed() {
				return
			}
			s.reconcile(t)
			requireIndexedOnce(t, s.database, chain)
		})
	}
}

// TestTheDeferredWriteStatesEachBlocksTextRowAndMembershipOnce: in each rendering deferral, each
// typed block and each ancestor the deferred writes wrote holds its document_block_texts row and its
// in_document tuple right after its write, and after the
// reconciliation exactly one of each, and its first rank: the reconciliation's pass finds them
// standing and appends none again. In the two deferrals by size each document then takes a second
// deferred version before the reconciliation runs, as two uploads before a tick do, which changes
// no typed block, and right after it each block still holds one of each. The reconciliation then
// states each document's facts under the second version's event rather than the event of the write
// that stated the block (docs' versionEventID), so a membership it appended again would be a second
// tuple the store keeps, and not a repeat of the same event graph_tuples_once drops. A room left
// owed is that case already: its documents take later versions, edits and the settlement's, after
// the writes that stated their blocks. The spec's next deferred version, which a room left owed
// takes too, adds a paragraph and leaves the criterion and the requirements under it
// (criterionChain) as the deferred write of the version before stated them: each keeps its
// document_blocks row, its changed_version and its document_block_texts rows exactly as they stood
// before that version's write.
func TestTheDeferredWriteStatesEachBlocksTextRowAndMembershipOnce(t *testing.T) {
	for _, deferral := range acceptedDeferrals {
		t.Run(deferral.name, func(t *testing.T) {
			s := writeAcceptedVersions(t, deferral)
			blocks := s.deferred()
			if s.created != nil {
				blocks = append(blocks, s.created.written...)
			}
			check := func(when string, ranks int) {
				t.Helper()
				for _, block := range blocks {
					held := readHeldTypedBlock(t, s.database, block.address())
					if held.textRows != 1 || held.inDocument != 1 || held.ranks != ranks {
						t.Errorf("%s the %s %s holds %d document_block_texts rows, %d in_document tuples and %d ranks, want one text row, one in_document and %d ranks",
							when, block.typ, block.address(), held.textRows, held.inDocument, held.ranks, ranks)
					}
				}
			}
			check("right after its write", 0)
			if !deferral.leftOwed {
				for _, name := range []string{"verification.md", "plan.md"} {
					s.fixture.writeAgain(t, s.fixture.documentNamed(t, name))
				}
				s.created.writeAgain(t, s.created.issue.PrimaryArtifactID)
			}
			spec := s.fixture.issue.PrimaryArtifactID
			kept := make([]blockRows, len(criterionChain))
			for at, block := range criterionChain {
				kept[at] = readBlockRows(t, s.database, s.specKey+"#"+block.id)
			}
			s.fixture.writeAgain(t, spec)
			version := latestVersion(t, s.database, spec).Number
			for at, block := range criterionChain {
				if held := readBlockRows(t, s.database, s.specKey+"#"+block.id); held != kept[at] {
					t.Errorf("the deferred version %d of the spec left the %s %s#%s as it stood but moved its rows: before it %s; after it %s",
						version, block.typ, s.specKey, block.id, kept[at], held)
				}
			}
			check("right after the next deferred versions", 0)
			if t.Failed() {
				return
			}
			s.reconcile(t)
			check("after the reconciliation", 1)
		})
	}
}

// TestARoomLeftOwedRunsTheTypedNodePassOnItsFragment: where a browser's unversioned edit of the plan
// leaves an issue room owed its block index, the whole typed-node pass runs on the fragment its
// write wrote: through an upload of the Verification, its
// decision, the reading it closes with, its check and its plan task are nodes at once, and through
// an edit of the spec its requirement is; and no block of the plan's fragment is, not even the plan
// task the browser's edit wrote, which waits for the room pass that closes the case.
func TestARoomLeftOwedRunsTheTypedNodePassOnItsFragment(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	createReferenceAPIProject(t, handler, "FRAG")
	issue := createDigestIssue(t, handler, database, "FRAG", "The fragment of a room left owed", typedDocument("", false, acceptedRequirement("r1")), false)
	fixture := &acceptedFixture{handler: handler, database: database, documents: deps.Docs.(*docs.Service), issue: issue, converted: true}
	plan := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", "The plan to come.\n").Artifact.ID
	fixture.holdOwedBy(t, plan, typedBlockMarkdown(issue.Key, acceptedPlanTask("tp", "r1")))
	fixture.upload(t, "verification.md", false, acceptedCheck("c1", "r1"), acceptedBlock{id: "d1", typ: "decision", reading: "g1"}, acceptedPlanTask("t9", "r1"))
	fixture.edit(t, issue.PrimaryArtifactID, acceptedRequirement("r2"))
	key := blockKeyOf(t, database, plan)
	for _, block := range []struct{ id, supertag string }{
		{"d1", refs.DecisionSupertag}, {"g1", refs.ReadingSupertag}, {"c1", refs.BlockSupertag}, {"t9", refs.BlockSupertag}, {"r2", refs.BlockSupertag},
	} {
		if held := readHeldTypedBlock(t, database, key+"#"+block.id); held.state != graphstore.StateLive || held.supertag != block.supertag {
			t.Errorf("the write of the room left owed left %s#%s at %q under the supertag %q, want a live node of %q", key, block.id, held.state, held.supertag, block.supertag)
		}
	}
	if held := readHeldTypedBlock(t, database, key+"#tp"); held.state != "" || held.rows != 0 {
		t.Errorf("the writes of the room left owed wrote the plan's block %s#tp, which a browser's unversioned edit holds, at %q with %d rows: want no node of another fragment", key, held.state, held.rows)
	}
	// A check of the written fragment hangs under the requirement of the spec's fragment it serves,
	// as a pass over the whole room would place it: that block stands in the store though the pass
	// covers no version of the spec.
	if chain := partOfChain(t, database, key+"#c1"); !strings.HasPrefix(chain, key+"#c1 -> block:"+key+"#r1 ") {
		t.Errorf("the check c1 of the room left owed climbs %s, want it under the requirement r1 it serves", chain)
	}
}

// requireGoneAtOnce fails t unless, right after what and with no reconciliation run, the store
// holds the block node key#id of type typ gone, holds no document_blocks row and no block_refs row
// of it, and its view of the tree read of issueKey answers no node of it.
func requireGoneAtOnce(t *testing.T, handler http.Handler, database *store.Store, issueKey, key, typ, id, what string) {
	t.Helper()
	address := key + "#" + id
	held := readHeldTypedBlock(t, database, address)
	var refsHeld int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from block_refs where from_node = $1`, address).Scan(&refsHeld); err != nil {
		t.Fatalf("count the block_refs rows of %s: %v", address, err)
	}
	switch {
	case held.state != graphstore.StateGone:
		t.Errorf("after %s the %s %s reads node state %q, want gone at once", what, typ, address, held.state)
	case held.rows != 0 || refsHeld != 0:
		t.Errorf("after %s the %s %s holds %d document_blocks rows and %d block_refs rows, want none", what, typ, address, held.rows, refsHeld)
	}
	if node, found := blockNodeOf(treeRead(t, handler, issueKey, acceptedViews[typ], "alice", false), id); found {
		t.Errorf("after %s %s of %s still answers the %s %s, at state %s", what, acceptedViews[typ], issueKey, typ, id, node.State)
	}
}

// TestATypedBlockADeferredVersionRemovesReadsGoneAtOnce: in each deferral where the tree the write
// wrote renders as the version's markdown, a version under the bound holds a requirement, a check,
// a plan task and a decision closing with a reading, indexed in its own write; a later version that
// defers its block index and removes each of them states its node gone in its own transaction and
// deletes its document_blocks row and its block_refs rows, a decision's and
// a reading's as the rest's, so the coverage read and each view of the tree read drop it at once,
// before any reconciliation. In each deferral the plan task's removal is the edit route's, which
// defers past the bound as an upload does (docs.WithDeferredBlockIndex).
func TestATypedBlockADeferredVersionRemovesReadsGoneAtOnce(t *testing.T) {
	for _, deferral := range acceptedDeferrals {
		t.Run(deferral.name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: deferral.leftOwed})
			createReferenceAPIProject(t, handler, "GONE")
			issue := createDigestIssue(t, handler, database, "GONE", "The removals of "+deferral.name,
				typedDocument("", false, acceptedRequirement("r1"), acceptedRequirement("r2")), !deferral.converted)
			fixture := &acceptedFixture{handler: handler, database: database, documents: deps.Docs.(*docs.Service), issue: issue, converted: deferral.converted}
			owner := "/api/v1/issues/" + issue.Key
			verification := uploadDocument(t, handler, owner, "verification.md", typedDocument(issue.Key, false,
				acceptedCheck("c1", "r1"), acceptedCheck("c2", "r2"), acceptedBlock{id: "d1", typ: "decision", reading: "g1"})).Artifact.ID
			plan := uploadDocument(t, handler, owner, "plan.md",
				typedDocument(issue.Key, false, acceptedPlanTask("t1", "r1"), acceptedPlanTask("t2", "r2"))).Artifact.ID
			for _, id := range []string{issue.PrimaryArtifactID, verification, plan} {
				requireIndexedAt(t, database, id, latestVersion(t, database, id).Number)
			}
			// What defers each removal: the list past the bound in the version itself or in a document
			// of the room written before it, or a browser's edit of another fragment no settlement has
			// versioned - the plan's while the Verification and the spec are uploaded, the
			// Verification's while the plan is edited.
			if deferral.leftOwed {
				fixture.holdOwedBy(t, plan, "")
			}
			uploadDocument(t, handler, owner, "verification.md", typedDocument(issue.Key, deferral.spec, acceptedCheck("c2", "r2")))
			key := blockKeyOf(t, database, verification)
			for _, block := range []struct{ typ, id string }{{"check", "c1"}, {"decision", "d1"}, {"reading", "g1"}} {
				requireGoneAtOnce(t, handler, database, issue.Key, key, block.typ, block.id, "the version of the Verification that removed "+block.id)
			}
			if read := requirementOf(t, readCoverage(t, handler, issue.Key), "r1"); read.Checks != 0 {
				t.Errorf("after the version that removed c1 the coverage read counts %d checks under r1, want none", read.Checks)
			}
			if deferral.leftOwed {
				fixture.holdOwedBy(t, verification, "")
			}
			// In a document above the bound the edit's own list puts the plan past it.
			ops := []map[string]any{{"op": "delete", "block": "t1"}}
			if deferral.plan {
				ops = append(ops, map[string]any{"op": "insert", "after": "end", "markdown": deferralList(itemsOverTheBlockBound, "end")})
			}
			succeeded(t, "delete t1", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+plan+"/edits", map[string]any{"ops": ops}, "alice"))
			// The edit's own version defers, which a later upload of the room's spec cannot mark for it.
			if indexed := blockIndexVersion(t, database, plan); indexed != nil {
				t.Errorf("the edit that removed t1 indexed the plan at %d in its own request, want its block index deferred", *indexed)
			}
			requireGoneAtOnce(t, handler, database, issue.Key, blockKeyOf(t, database, plan), "plan_task", "t1", "the version of the plan that removed t1")
			if read := requirementOf(t, readCoverage(t, handler, issue.Key), "r1"); read.PlanTasks != 0 {
				t.Errorf("after the version that removed t1 the coverage read counts %d plan tasks under r1, want none", read.PlanTasks)
			}
			uploadDocument(t, handler, owner, "spec", typedDocument(issue.Key, deferral.spec, acceptedRequirement("r2")))
			requireGoneAtOnce(t, handler, database, issue.Key, blockKeyOf(t, database, issue.PrimaryArtifactID), "requirement", "r1", "the version of the spec that removed r1")
			if coverage := readCoverage(t, handler, issue.Key); slices.ContainsFunc(coverage.Requirements, func(requirement model.RequirementCoverage) bool { return requirement.BlockID == "r1" }) {
				t.Errorf("after the version that removed r1 the coverage read still lists it: %+v", coverage.Requirements)
			}
			for _, id := range []string{issue.PrimaryArtifactID, verification, plan} {
				if indexed := blockIndexVersion(t, database, id); indexed != nil && *indexed == latestVersion(t, database, id).Number {
					t.Fatalf("%s is indexed at its version %d, want it owed: the removal did not defer its block index, so this test proves nothing", id, *indexed)
				}
			}
		})
	}
}

// TestANoChangeWriteKeepsThePreviousTypedNodes: an upload above the bound whose replacement changed
// nothing writes no tree, so the typed nodes the version before wrote, in
// a document that is its own room and in an issue room above the bound, stand live and unchanged:
// its write states no node and no row of them and marks none gone. The issue create always seeds
// the tree it writes, so no create's replacement changes nothing.
func TestANoChangeWriteKeepsThePreviousTypedNodes(t *testing.T) {
	for _, room := range []struct {
		name      string
		converted bool
	}{{"a document above the bound", false}, {"an issue room above the bound", true}} {
		t.Run(room.name, func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
			createReferenceAPIProject(t, handler, "SAME")
			issue := createDigestIssue(t, handler, database, "SAME", "The unchanged upload of "+room.name,
				typedDocument("", false, acceptedRequirement("r1")), !room.converted)
			owner := "/api/v1/issues/" + issue.Key
			content := typedDocument(issue.Key, true, acceptedCheck("c1", "r1"), acceptedCheck("c2", "r1"))
			verification := uploadDocument(t, handler, owner, "verification.md", content).Artifact.ID
			key := blockKeyOf(t, database, verification)
			before := map[string]heldTypedBlock{}
			for _, id := range []string{"c1", "c2"} {
				if before[id] = readHeldTypedBlock(t, database, key+"#"+id); before[id].state != graphstore.StateLive {
					t.Fatalf("the first upload left the check %s at %q, want it live: this test proves nothing", id, before[id].state)
				}
			}
			again := uploadDocument(t, handler, owner, "verification.md", content)
			if again.Version.Number != 2 {
				t.Fatalf("the unchanged upload wrote version %d, want 2", again.Version.Number)
			}
			for _, id := range []string{"c1", "c2"} {
				if after := readHeldTypedBlock(t, database, key+"#"+id); after != before[id] {
					t.Errorf("the upload whose replacement changed nothing moved the check %s from %+v to %+v, want it live and unchanged", id, before[id], after)
				}
			}
		})
	}
}

// writeDisagreeingVersion writes, below the routes, through the document service, the next version
// of document artifactID holding versionMarkdown, in a transaction whose tree of the document is
// tree's - "" for a write that writes no tree - and indexes it as an upload does
// (IndexDocumentReferences), so the tree the transaction leaves and the version's markdown
// disagree. It answers the version's number.
func writeDisagreeingVersion(t *testing.T, database *store.Store, service *docs.Service, artifactID, tree, versionMarkdown string) int {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if tree != "" {
		if _, err := service.ReplaceText(joined, artifactID, tree, alice, nil); err != nil {
			t.Fatalf("write the tree of %s: %v", artifactID, err)
		}
	}
	version, err := ledger.QueueVersion(ctx, artifactID, versionMarkdown, []model.Actor{alice}, nil)
	if err != nil {
		t.Fatalf("queue the version of %s: %v", artifactID, err)
	}
	_, indexed, err := service.IndexDocumentReferences(joined, artifactID, version.Number, versionMarkdown)
	if err != nil {
		t.Fatalf("index version %d of %s: %v", version.Number, artifactID, err)
	}
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, indexed)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit version %d of %s: %v", version.Number, artifactID, err)
	}
	return version.Number
}

// requireNoNode fails t unless the store holds no node of the block key#id.
func requireNoNode(t *testing.T, database *store.Store, key, id, what string) {
	t.Helper()
	if held := readHeldTypedBlock(t, database, key+"#"+id); held.state != "" || held.rows != 0 {
		t.Errorf("%s wrote the node %s#%s at %q with %d rows, want none: the tree and the version disagree", what, key, id, held.state, held.rows)
	}
}

// TestAnOwedDocumentIsNamedByTheReads: the coverage read and the tree read each name every document
// of the issue that owes its block index by either mark: the three writes
// whose tree and markdown disagree - an upload whose written tree does not render as its version's
// markdown, and two writes to an issue room whose own fragment reads otherwise than the version,
// one whose writer wrote no tree and one whose tree does not render as its markdown - each write
// no requirement, check or plan-task node, and the reads name the document and the version whose
// blocks wait; so do they name a document whose version the roll's trigger nulled (a version the
// release before inserts) and a document owed by store_owed_at alone, its version standing, under
// the backfill's mark (markStaleBatch) and under the tick's (readEventsAfterCursor); and an issue
// that owes nothing answers no member.
func TestAnOwedDocumentIsNamedByTheReads(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	service := deps.Docs.(*docs.Service)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "OWING")
	verificationOf := func(title string, converted bool, checks ...acceptedBlock) (model.Issue, string) {
		t.Helper()
		issue := createDigestIssue(t, handler, database, "OWING", title, typedDocument("", false, acceptedRequirement("r1")), !converted)
		id := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", typedDocument(issue.Key, false, checks...)).Artifact.ID
		requireIndexedAt(t, database, id, 1)
		return issue, id
	}
	for _, disagreeing := range []struct {
		name          string
		converted     bool
		tree, version []acceptedBlock
	}{
		{"an upload whose written tree does not render as its version", false,
			[]acceptedBlock{acceptedCheck("c1", "r1"), acceptedCheck("c2", "r1")}, []acceptedBlock{acceptedCheck("c1", "r1"), acceptedCheck("c3", "r1")}},
		{"a write to an issue room whose writer wrote no tree", true,
			nil, []acceptedBlock{acceptedCheck("c1", "r1"), acceptedCheck("c4", "r1")}},
		{"a write to an issue room whose tree does not render as its version", true,
			[]acceptedBlock{acceptedCheck("c1", "r1"), acceptedCheck("c5", "r1")}, []acceptedBlock{acceptedCheck("c1", "r1"), acceptedCheck("c6", "r1")}},
	} {
		issue, verification := verificationOf("The owed document of "+disagreeing.name, disagreeing.converted, acceptedCheck("c1", "r1"))
		tree := ""
		if disagreeing.tree != nil {
			tree = typedDocument(issue.Key, false, disagreeing.tree...)
		}
		version := writeDisagreeingVersion(t, database, service, verification, tree, typedDocument(issue.Key, false, disagreeing.version...))
		key := blockKeyOf(t, database, verification)
		for _, block := range slices.Concat(disagreeing.tree, disagreeing.version) {
			if block.id != "c1" {
				requireNoNode(t, database, key, block.id, disagreeing.name)
			}
		}
		requireOwedNamed(t, handler, issue.Key, verification, version, disagreeing.name)
	}

	// A version the release before writes during a roll: its insert alone, which migration 0122's
	// trigger marks owed (document_block_versions.version null), with no pass of this build's.
	rolled, rolledVerification := verificationOf("The owed document of a version the release before wrote", false, acceptedCheck("c1", "r1"))
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 2, $2, '[{"kind":"user","id":"alice"}]')`, rolledVerification, typedDocument(rolled.Key, false, acceptedCheck("c1", "r1"))); err != nil {
		t.Fatalf("insert a version as the release before writes one: %v", err)
	}
	if indexed := blockIndexVersion(t, database, rolledVerification); indexed != nil {
		t.Fatalf("the trigger left %s indexed at %d, want its version null", rolledVerification, *indexed)
	}
	requireOwedNamed(t, handler, rolled.Key, rolledVerification, 2, "a version the release before wrote")

	// A document stated by a pass of an earlier rule, its version standing, marked owed by the
	// backfill (graphcensus' markStaleBatch) and by the tick (docs' readEventsAfterCursor), each
	// through the statement that writer runs.
	for _, mark := range []struct{ name, statement string }{
		{"the backfill's mark (markStaleBatch)", graphcensus.StaleBatchMarkStatement},
		{"the tick's mark (readEventsAfterCursor)", docs.MarkVersionedDocumentsStatement},
	} {
		issue, verification := verificationOf("The owed document of "+mark.name, false, acceptedCheck("c1", "r1"))
		if _, err := database.Pool.Exec(ctx, `update document_block_versions set stated_rule = 1 where artifact_id = $1`, verification); err != nil {
			t.Fatalf("state %s by an earlier rule: %v", verification, err)
		}
		if _, err := database.Pool.Exec(ctx, mark.statement, []string{verification}, refs.BlockIndexMarkingRule); err != nil {
			t.Fatalf("run %s: %v", mark.name, err)
		}
		var owedAt *time.Time
		var version *int
		if err := database.Pool.QueryRow(ctx, `select store_owed_at, version from document_block_versions where artifact_id = $1`, verification).Scan(&owedAt, &version); err != nil || owedAt == nil || version == nil {
			t.Fatalf("after %s %s holds store_owed_at %v and version %v (%v), want a mark beside a standing version: this case proves nothing", mark.name, verification, owedAt, version, err)
		}
		requireOwedNamed(t, handler, issue.Key, verification, *version, "a document owed by store_owed_at alone under "+mark.name)
	}

	settled, _ := verificationOf("The issue that owes nothing", false, acceptedCheck("c1", "r1"))
	requireNothingOwed(t, handler, settled.Key, "on an issue whose documents are all indexed,")
}
