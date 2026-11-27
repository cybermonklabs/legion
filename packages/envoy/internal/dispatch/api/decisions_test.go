package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The Decisions read reads the store alone. Its entries are held
// by decision_entries_test.go: the order, the shaped links and their staleness, and the review state
// a Keep moves are TestTheDecisionsReadAnswersEveryDecisionInOrder's, and a decision whose words
// change after a Keep waiting again is TestAReadingIsAChildNodeOfTheDecisionItReads'. The Keep on a
// decision node is TestAKeepIsTwoAppendsOnTheDecisionNodeThatDecisionKeepsProjects'. This file holds
// the members the release before reads, the Keep on a heading of a decisions document an earlier
// release stored, each person's last read version of a document, and the ties a first indexing
// leaves current.

// holdIt is the spec's second callout, written with its id, hold-it.
func holdIt(body string) string {
	return ":::callout{#hold-it kind=\"note\" title=\"\"}\n" + body + "\n:::"
}

// readIssueDecisions is GET /api/v1/issues/{key}/decisions as alice.
func readIssueDecisions(t *testing.T, handler http.Handler, key string) issueDecisions {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/decisions", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/%s/decisions: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[issueDecisions](t, response)
}

// keepBlock is a person's Keep of the decision at blockID, at the text sum the read served them.
func keepBlock(t *testing.T, handler http.Handler, artifactID, blockID, sum string) model.DecisionKeep {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+artifactID+"/blocks/"+blockID+"/keep",
		map[string]any{"text_sha256": sum}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("keep %s: status=%d body=%s", blockID, response.Code, response.Body.String())
	}
	return decodeBody[model.DecisionKeep](t, response)
}

// oldMembersSpec is the spec of the old members' test: one requirement, an ask that shaped it,
// which bob answers, and an ask nobody answers.
const oldMembersSpec = ":::requirement{#r1}\nThe service answers over one transport.\n:::\n\n" +
	":::ask{#a1 urgency=\"med\" multiple=\"false\" shaped=\"dispatch://CORE-1/spec#b-r1\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n\n" +
	":::ask{#a2 urgency=\"low\" multiple=\"false\"}\nWhich region first?\n\n- East: Near the team\n- West: Near the users\n:::\n"

// The Decisions read's old members answer from the store for one release, for a page of the
// release before. `decisions` answers each decision node
// and each answered ask in model.Decision's shape: kind `agent` for a `decision` block and
// `person` for an answered ask, links from their `shaped` tuples, waiting_review from the node, and
// no entry for an open ask. `document` answers null while the issue holds no decisions document.
// With one an earlier release stored standing, `document` answers its address and the issue's
// `decisions_artifact_id` its id, found by its slug when its role is null, and none of its
// sections reaches `decisions`. Once it is retired, `document` answers null and
// `decisions_artifact_id` "".
func TestTheOldDecisionsMembersAnswerFromTheStore(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", oldMembersSpec)
	if issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which the answered ask's shaped link names", issue.Key)
	}
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	settleIssue(t, documents, issue.Key)
	answered, open := blockAskID(t, handler, issue.Key, "a1"), blockAskID(t, handler, issue.Key, "a2")
	answerDecisionBlock(t, handler, answered, "bob", []string{"REST"}, "REST, plain HTTP.", nil)
	agentEdit(t, handler, spec.id, latestVersion(t, database, spec.id).Number+1, map[string]string{
		"op": "insert", "after": "end",
		"markdown": ":::decision{#d1 shaped=\"dispatch://CORE-1/spec#b-r1\"}\nWe ship one region first.\n:::\n",
	})

	byRef := func(view issueDecisions) map[string]model.Decision {
		decisions := make(map[string]model.Decision, len(view.Decisions))
		for _, decision := range view.Decisions {
			decisions[decision.Ref] = decision
		}
		return decisions
	}
	agentRef, personRef := "dispatch://CORE-1/spec#b-d1", "dispatch://CORE-1/ask/"+answered
	requireOld := func(when string, view issueDecisions) {
		t.Helper()
		decisions := byRef(view)
		agent, person := decisions[agentRef], decisions[personRef]
		if len(view.Decisions) != 2 || agent.Kind != "agent" || person.Kind != "person" {
			t.Fatalf("%s the old decisions member = %s, want the decision %s (agent) and the answered ask %s (person) and nothing else",
				when, entryJSON(t, view.Decisions), agentRef, personRef)
		}
		if _, listed := decisions["dispatch://CORE-1/ask/"+open]; listed {
			t.Fatalf("%s the old decisions member lists the open ask %s", when, open)
		}
		for _, decision := range []model.Decision{agent, person} {
			if len(decision.Links) != 1 || decision.Links[0].To.ID != spec.pointed("r1") {
				t.Errorf("%s %s answers the links %s, want one, to %s, from its shaped tuple",
					when, decision.Ref, entryJSON(t, decision.Links), spec.pointed("r1"))
			}
		}
		if !agent.WaitingReview || person.WaitingReview || agent.TextSHA256 != decisionread.TextSum(agent.Text) {
			t.Errorf("%s the decision waits %t and the answered ask %t, want true and false, and the decision's sum is its words'",
				when, agent.WaitingReview, person.WaitingReview)
		}
		for _, entry := range view.Entries {
			if entry.Node.Kind == graphstore.KindAsk && entry.Node.ID == open {
				return
			}
		}
		t.Errorf("%s the entries hold no open ask %s: the old member leaves out what entries keep", when, open)
	}

	view := readIssueDecisions(t, handler, issue.Key)
	requireOld("with no decisions document", view)
	if view.Document != nil {
		t.Fatalf("the old document member = %q with no decisions document, want null", *view.Document)
	}
	// A decision's text carries the tree a reader is shown, for a caller that asked for the trees.
	requireDisplayTree(t, handler, "/api/v1/issues/"+issue.Key+"/decisions", "decisions.0.text", "decisions.0.text_tree")

	standing := seedDecisionsDocument(t, database, documents, issue.Key,
		"# Decisions\n\n## Transport\n\nHTTP it is.\n\n## Regions\n\nOne region first. This shapes dispatch://CORE-1/spec#b-r1.\n")
	if _, err := database.Pool.Exec(ctx, `update artifacts set role = null where id = $1`, standing); err != nil {
		t.Fatalf("clear the stored role: %v", err)
	}
	view = readIssueDecisions(t, handler, issue.Key)
	requireOld("while a decisions document stands", view)
	if want := "dispatch://CORE-1/artifact/decisions-md"; view.Document == nil || *view.Document != want {
		t.Fatalf("the old document member = %v while the decisions document stands, want %s", view.Document, want)
	}
	if named := readIssueRoleDocuments(t, handler, issue.Key); named.DecisionsArtifactID != standing {
		t.Fatalf("the issue read names decisions_artifact_id %q while the roleless decisions document stands, want %s, found by its slug",
			named.DecisionsArtifactID, standing)
	}

	retireTestDocument(t, database, standing)
	view = readIssueDecisions(t, handler, issue.Key)
	requireOld("once the decisions document is retired", view)
	if view.Document != nil {
		t.Fatalf("the old document member = %q once the decisions document is retired, want null", *view.Document)
	}
	if named := readIssueRoleDocuments(t, handler, issue.Key); named.DecisionsArtifactID != "" {
		t.Fatalf("the issue read names decisions_artifact_id %q once the decisions document is retired, want \"\"", named.DecisionsArtifactID)
	}
}

// The old `decisions` member answers a `decision` block a person decided and nobody kept as kind
// `person`, for a page of the release before, which lists a decision only where it waits for
// review, is kind `person` or holds a Keep, or is replaced: the spec's p1, which alice's
// credential wrote, would otherwise read kind `agent`, waiting for nobody and kept by nobody, and
// stand in none of that page's lists. Its entry keeps kind `agent`, the node's shape. The agent's
// decision a1 stays kind `agent` and waits, and once alice keeps her p1 it reads kind `agent`
// with her Keep, which that page lists as hers.
func TestTheOldDecisionsMemberAnswersAPersonsUnkeptDecisionAsAPersons(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::decision{#p1}\nAlice's spec decides.\n:::\n")
	agentUploadAs(t, handler, issue.Key, "agent.md", ":::decision{#a1}\nThe agent decides.\n:::\n", nil)
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	requireDecidedBy(t, database, "the person's decision", spec.address("p1"), "alice")
	read := func() (map[string]model.Decision, map[string]decisionEntry) {
		t.Helper()
		view := readIssueDecisions(t, handler, issue.Key)
		decisions := make(map[string]model.Decision, len(view.Decisions))
		for _, decision := range view.Decisions {
			decisions[decision.BlockID] = decision
		}
		entries := make(map[string]decisionEntry, len(view.Entries))
		for _, entry := range view.Entries {
			entries[entry.Node.ID] = entry
		}
		return decisions, entries
	}

	decisions, entries := read()
	person, agent := decisions["p1"], decisions["a1"]
	if len(decisions) != 2 || person.Kind != "person" || person.WaitingReview || person.Kept != nil ||
		agent.Kind != "agent" || !agent.WaitingReview || agent.Kept != nil {
		t.Fatalf("the old decisions member = %s, want alice's p1 of kind person waiting for nobody and the agent's a1 of kind agent waiting for review, neither kept",
			entryJSON(t, decisions))
	}
	if entry, found := entries[spec.address("p1")]; !found || entry.Kind != "agent" || entry.DecidedBy == nil || entry.DecidedBy.Kind != graphstore.KindPerson {
		t.Errorf("the entry of p1 reads %s (found %t), want kind agent, the node's shape, decided by a person", entryJSON(t, entry), found)
	}

	keepBlock(t, handler, spec.id, "p1", person.TextSHA256)
	decisions, _ = read()
	if kept := decisions["p1"]; kept.Kind != "agent" || kept.Kept == nil || kept.Kept.KeptBy != "alice" || kept.WaitingReview {
		t.Errorf("after alice's Keep the old decisions member answers p1 as %s, want kind agent with alice's Keep, waiting for nobody", entryJSON(t, kept))
	}
}

// An issue that holds two decisions documents as decisions_documents() reads them - one an earlier
// release gave the decisions role under another slug, and one with no role under the decisions
// document's slug - names the same one in both old members: the oldest, by created_at and then id
// (refs.DecisionsDocumentOf), in the issue read's decisions_artifact_id as in the Decisions read's
// document. The younger is given the lower id, so a rule that took the lowest id would name it.
func TestBothOldMembersNameTheOldestOfAnIssuesTwoDecisionsDocuments(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	const older, younger = "ffffffff-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"
	if _, err := database.Pool.Exec(ctx, `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, role, created_by, created_at)
		select v.id::uuid, i.key, i.project_key, v.slug, v.name, 'doc', false, v.role,
		       '{"kind":"session","id":"`+decidingSession+`"}', v.at
		from issues i, (values ($2, 'decided-md', 'decided.md', 'decisions', now() - interval '1 hour'),
		                       ($3, 'decisions-md', 'decisions.md', null, now())) v (id, slug, name, role, at)
		where i.key = $1
	`, issue.Key, older, younger); err != nil {
		t.Fatalf("store the issue's two decisions documents: %v", err)
	}
	if named := readIssueRoleDocuments(t, handler, issue.Key); named.DecisionsArtifactID != older {
		t.Fatalf("the issue read names decisions_artifact_id %q, want %s, the older of its two decisions documents", named.DecisionsArtifactID, older)
	}
	if view := readIssueDecisions(t, handler, issue.Key); view.Document == nil || !strings.HasSuffix(*view.Document, "/artifact/decided-md") {
		t.Fatalf("the Decisions read's document = %v, want the address of decided-md, the document decisions_artifact_id names", view.Document)
	}
}

// Until the decisions step retires it, a heading of a decisions document an earlier release stored
// is kept as the release before kept it: the Keep reads the
// section through the step's section reader, whose text runs from the heading to its last block, so
// its sum is the one that release served. A Keep of another sum is 409 DECISION_CHANGED and writes
// nothing; the Keep of the sum answers 200 and writes the decision_keeps row the conversion carries
// onto the node. A heading under `## Replaced`, a block that opens no section and a bearer's Keep are
// refused. Once the document is retired, its heading is 404 TARGET_NOT_FOUND. A heading's node is
// a plain block and no decision, so the Keep's read of a decision node leaves it out without
// logging it as a decision whose stored value does not parse.
func TestAHeadingOfAStandingDecisionsDocumentIsKeptUntilItIsRetired(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	logs := captureLogs(t)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	decisions := seedDecisionsDocument(t, database, deps.Docs.(*docs.Service), issue.Key,
		"# Decisions\n\n## Transport\n\nHTTP it is.\n\n## Replaced\n\n### Files\n\nReplaced by HTTP.\n")
	blocks := blockIDsByText(t, handler, decisions)
	transport, files, paragraph := blocks["## Transport"], blocks["### Files"], blocks["HTTP it is."]
	if transport == "" || files == "" || paragraph == "" {
		t.Fatalf("the decisions document's blocks = %v, want the two headings and the paragraph", blocks)
	}
	keepPath := func(block string) string { return "/api/v1/artifacts/" + decisions + "/blocks/" + block + "/keep" }
	keeps := func() int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from decision_keeps where artifact_id = $1`, decisions).Scan(&count); err != nil {
			t.Fatalf("count the Keeps: %v", err)
		}
		return count
	}
	sum := decisionread.TextSum("## Transport\n\nHTTP it is.")

	changed := dispatchRequest(t, handler, http.MethodPost, keepPath(transport), map[string]any{"text_sha256": decisionread.TextSum("## Transport\n\nHTTP/2 it is.")}, "alice")
	if changed.Code != http.StatusConflict || !strings.Contains(changed.Body.String(), "DECISION_CHANGED") || keeps() != 0 {
		t.Fatalf("a Keep of words the section does not hold: status=%d body=%s, %d Keeps; want 409 DECISION_CHANGED and none written",
			changed.Code, changed.Body.String(), keeps())
	}
	keep := keepBlock(t, handler, decisions, transport, sum)
	// The sum is no part of the Keep's wire shape (model.DecisionKeep), so it is read off the row.
	var keptSum string
	if err := database.Pool.QueryRow(ctx, `select text_sha256 from decision_keeps where artifact_id = $1 and block_id = $2`,
		decisions, transport).Scan(&keptSum); err != nil {
		t.Fatalf("read the Keep's row: %v", err)
	}
	if keep.BlockID != transport || keptSum != sum || keep.KeptBy != "alice" || keeps() != 1 {
		t.Fatalf("the Keep of the heading = %#v with the row's sum %q, %d Keeps; want %s kept by alice at %s, one decision_keeps row",
			keep, keptSum, keeps(), transport, sum)
	}
	for _, refusal := range []struct{ what, block string }{
		{"a heading under `## Replaced`", files},
		{"a block that opens no section", paragraph},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, keepPath(refusal.block), map[string]any{"text_sha256": sum}, "alice")
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "TARGET_NOT_FOUND") {
			t.Fatalf("a Keep on %s: status=%d body=%s, want 404 TARGET_NOT_FOUND", refusal.what, response.Code, response.Body.String())
		}
	}
	bearer := sessionRequest(t, handler, http.MethodPost, keepPath(transport), map[string]any{"text_sha256": sum, "actor": sessionActor()})
	if bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), codeScopeRequired) {
		t.Fatalf("a bearer's Keep: status=%d body=%s, want 403 %s", bearer.Code, bearer.Body.String(), codeScopeRequired)
	}
	if warned := logs.Lines("a decision whose stored value does not parse"); warned != "" {
		t.Fatalf("the Keeps of the decisions document's blocks logged:\n%swant no parse failure: a heading's node is no decision", warned)
	}

	retireTestDocument(t, database, decisions)
	retired := dispatchRequest(t, handler, http.MethodPost, keepPath(transport), map[string]any{"text_sha256": sum}, "alice")
	if retired.Code != http.StatusNotFound || !strings.Contains(retired.Body.String(), "TARGET_NOT_FOUND") {
		t.Fatalf("a Keep on a heading of the retired document: status=%d body=%s, want 404 TARGET_NOT_FOUND",
			retired.Code, retired.Body.String())
	}
}

// Each person's last read version of a document: null before they read it, the version they
// recorded after, and a bearer may read neither route.
func TestDocumentReadRecordsEachPersonsLastReadVersion(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", shipIt("Ship it.")+"\n\n"+holdIt("Hold it.")+"\n")
	path := "/api/v1/me/documents/" + issue.PrimaryArtifactID + "/read"

	first := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
	if first.Code != http.StatusOK {
		t.Fatalf("read the read mark: status=%d body=%s", first.Code, first.Body.String())
	}
	if read := decodeBody[model.DocumentRead](t, first); read.Version != nil || read.ReadAt != nil {
		t.Fatalf("before any read the mark = %#v, want a null version", read)
	}

	stored := dispatchRequest(t, handler, http.MethodPut, path, map[string]any{"version": 1}, "alice")
	if stored.Code != http.StatusOK {
		t.Fatalf("record the read mark: status=%d body=%s", stored.Code, stored.Body.String())
	}
	if read := decodeBody[model.DocumentRead](t, stored); read.Version == nil || *read.Version != 1 {
		t.Fatalf("the recorded mark = %#v, want version 1", read)
	}
	again := decodeBody[model.DocumentRead](t, dispatchRequest(t, handler, http.MethodGet, path, nil, "alice"))
	if again.Version == nil || *again.Version != 1 || again.ReadAt == nil {
		t.Fatalf("the stored mark = %#v, want version 1 with its time", again)
	}
	if bob := decodeBody[model.DocumentRead](t, dispatchRequest(t, handler, http.MethodGet, path, nil, "bob")); bob.Version != nil {
		t.Fatalf("another person's mark = %#v, want null: the mark is one person's", bob)
	}

	for _, probe := range []struct {
		method string
		body   any
	}{
		{http.MethodGet, nil},
		{http.MethodPut, map[string]any{"version": 2, "actor": sessionActor()}},
	} {
		response := sessionRequest(t, handler, probe.method, path, probe.body)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), codeScopeRequired) {
			t.Fatalf("a bearer's %s: status=%d body=%s, want 403 %s", probe.method, response.Code, response.Body.String(), codeScopeRequired)
		}
	}
}

// firstIndexingSpec is the spec of TestAFirstIndexingMarksNothingOutOfDate: two requirements, a
// decision that shapes both and one that shapes the second. The issue the test creates is CORE-1,
// the first of its project.
const firstIndexingSpec = ":::requirement{#r1}\nOne.\n:::\n\n:::requirement{#r2}\nTwo.\n:::\n\n" +
	":::decision{#d1 shaped=\"dispatch://CORE-1/spec#b-r1 dispatch://CORE-1/spec#b-r2\"}\nOne transport for both.\n:::\n\n" +
	":::decision{#d2 shaped=\"dispatch://CORE-1/spec#b-r2\"}\nThe second ships first.\n:::\n"

// A first indexing of a document holding decisions marks no tie out of date, in both cases of a
// first indexing: the document's first version, written while it has no block rows, and the
// backfill over a document no block-aware build indexed. The spec's decisions shape its
// requirements in the version that writes them all, so no requirement's text changed after a
// tie and no decision's words did, and a later version that leaves every requirement and
// decision alone is one more version the backfill walks. Each link is judged on the change the
// index recorded for its requirement, so it holds text_changed_at, and reads changed_since_tie
// false and stale false.
func TestAFirstIndexingMarksNothingOutOfDate(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "A first indexing", "actor": sessionActor(), "spec": firstIndexingSpec,
	})
	requireAgentWrite(t, "the agent's issue", created, http.StatusCreated)
	issue := decodeBody[model.Issue](t, created)
	if issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which the decisions' shaped links name", issue.Key)
	}
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	ties := map[string][]string{
		"d1": {spec.address("r1"), spec.address("r2")},
		"d2": {spec.address("r2")},
	}
	judge := func(when string) {
		t.Helper()
		entries := readEntries(t, handler, issue.Key)
		for _, block := range []string{"d1", "d2"} {
			entry := requireEntry(t, entries, "the decision "+block, refs.BlockNodeKind+":"+spec.address(block))
			if targets := shapedTargets(entry); !slices.Equal(targets, ties[block]) {
				t.Fatalf("%s the decision %s answers the links %v, want %v", when, block, targets, ties[block])
			}
			for _, link := range entry.Shaped {
				if link.TextChangedAt == nil || link.ChangedSinceTie || link.Stale {
					t.Errorf("%s the link %s of %s reads %s, want its requirement's change recorded, changed_since_tie false and stale false",
						when, link.To.ID, block, entryJSON(t, link))
				}
			}
		}
	}

	// The document's first version, written while it had no block rows: the create's own write
	// indexes it.
	requireIndexedAt(t, database, spec.id, 1)
	judge("at the first version,")
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": "A paragraph no decision shapes."}})

	// The backfill: the state migration 0121 left a document no block-aware build indexed, then
	// one reconciliation tick, as the block index's own tests run it.
	for _, statement := range []string{
		`delete from block_refs where artifact_id = $1`,
		`delete from document_blocks where artifact_id = $1`,
		`delete from document_block_texts where artifact_id = $1`,
		`delete from document_indexed_versions where artifact_id = $1`,
		`update document_block_versions set version = null where artifact_id = $1`,
	} {
		if _, err := database.Pool.Exec(ctx, statement, spec.id); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := deps.Docs.(*docs.Service).ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
		t.Fatalf("reconcile the block index: %v", err)
	}
	requireIndexedAt(t, database, spec.id, 2)
	judge("after the backfill,")
}

// The open questions are the issue's open asks whose block is in its spec, oldest first, and an
// issue with no decisions document and no decision answers them with a null document and no
// decisions. The two asks are written in two versions, so one is older than the other: two that one
// settlement writes share their created_at and the order between them is their ids'. An open ask
// whose row names the spec but a block the spec's current blocks do not hold is none of them: a
// reopen restores such a row with no block to write, which a later settlement retracts.
func TestIssueDecisionsListTheSpecsOpenQuestions(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: 20 * time.Millisecond})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::ask{#decide_first}\nWhich store?\n:::\n\nA paragraph.\n")
	view, _ := waitForOpenQuestions(t, handler, issue.Key, 1)
	if view.Document != nil || len(view.Decisions) != 0 {
		t.Fatalf("an issue with no decisions document and no decision = %#v, want a null document and no decisions", view)
	}

	editSpec(t, handler, issue.Key, []map[string]any{
		{"op": "insert", "after": "end", "markdown": ":::ask{#decide_second}\nWhich name?\n:::"},
	})
	_, questions := waitForOpenQuestions(t, handler, issue.Key, 2)
	if fmt.Sprint(questions) != "[Which store? Which name?]" {
		t.Fatalf("open_questions = %v, want the spec's two asks oldest first", questions)
	}
	// Each question is the ask row the ask list answers, and that row declares a question tree,
	// so this view answers one too: a caller that asked for the trees is given the same row by
	// both routes, and a client that reads either draws what the server read rather than its own
	// reading of the markdown.
	requireDisplayTree(t, handler, "/api/v1/issues/"+issue.Key+"/decisions",
		"open_questions.0.question", "open_questions.0.question_tree")

	if tag, err := database.Pool.Exec(context.Background(), `update asks set block_id = 'decide_gone' where issue_key = $1 and block_id = 'decide_second'`, issue.Key); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("point the second ask's row at a block the spec does not hold: %v", err)
	}
	if _, questions := waitForOpenQuestions(t, handler, issue.Key, 1); fmt.Sprint(questions) != "[Which store?]" {
		t.Fatalf("open_questions = %v once the second ask's block is none the spec holds, want the first ask alone", questions)
	}
}

// One value of an ask's node that does not parse drops that one ask from the Decisions read and
// never fails the whole read (R4): the open questions and the entries are answered without it, as
// they are answered for every other ask of the spec. Two asks of the spec hold a value written by
// hand - an answered_at that is no time, and a selected that is no list of labels - beside one
// whose node is as the server wrote it. The same holds while the spec owes its block index, where
// the open questions are read by the ask blocks the spec's fragment holds (specOpenQuestions):
// reading those blocks decodes no answer, so the ask whose node holds an answered_at that is no
// time is left out there too, and the read answers the open ask beside it.
func TestAnAskWhoseNodeHoldsAValueThatDoesNotParseIsLeftOutOfTheDecisionsRead(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: 20 * time.Millisecond})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE",
		":::ask{#fine}\nWhich store?\n:::\n\n:::ask{#late}\nWhich name?\n:::\n\n:::ask{#listed}\nWhich port?\n:::\n")
	waitForOpenQuestions(t, handler, issue.Key, 3)
	fine, late, listed := blockAskID(t, handler, issue.Key, "fine"), blockAskID(t, handler, issue.Key, "late"), blockAskID(t, handler, issue.Key, "listed")
	for ask, written := range map[string][2]string{
		late:   {graphstore.TupleAnsweredAt, "next Tuesday"},
		listed: {graphstore.TupleSelected, "Yes, No"},
	} {
		if _, err := database.Pool.Exec(ctx, `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			values ($1, $2, $3, $4, $5, now(), '{"kind":"user","id":"alice"}'::jsonb, 0, true)
		`, graphstore.KindAsk, ask, written[0], graphstore.KindValue, written[1]); err != nil {
			t.Fatalf("state %s %q on the node of ask %s: %v", written[0], written[1], ask, err)
		}
	}

	view := readIssueDecisions(t, handler, issue.Key)
	if len(view.OpenQuestions) != 1 || view.OpenQuestions[0].ID != fine {
		ids := []string{}
		for _, question := range view.OpenQuestions {
			ids = append(ids, question.ID)
		}
		t.Errorf("open_questions = %v, want the one ask whose node parses, %s", ids, fine)
	}
	entries := entriesByNode(view.Entries)
	if _, held := entries[graphstore.KindAsk+":"+fine]; !held {
		t.Errorf("the entries hold no entry for the ask %s whose node parses: %s", fine, entryJSON(t, view.Entries))
	}
	for _, dropped := range []string{late, listed} {
		if _, held := entries[graphstore.KindAsk+":"+dropped]; held {
			t.Errorf("the entries hold an entry for the ask %s, whose node holds a value that does not parse", dropped)
		}
	}

	t.Run("a spec that owes its block index", func(t *testing.T) {
		t.Parallel()
		handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
		documents := deps.Docs.(*docs.Service)
		createReferenceAPIProject(t, handler, "OWED")
		issue := createSpecIssue(t, handler, "OWED", "A spec.\n")
		key, spec := issue.Key, issue.PrimaryArtifactID
		reconcileUntilNothingOwed(t, handler, deps, key)
		const lateAsk = ":::ask{#q-late urgency=\"med\" multiple=\"false\"}\nWhen does the queue ship?\n:::\n"
		written := uploadDocument(t, handler, "/api/v1/issues/"+key, "spec.md",
			"A spec.\n\n"+owedAsk+"\n"+lateAsk+"\n"+deferralList(itemsOverTheBlockBound, "end"))
		version := written.Version.Number
		settleIssue(t, documents, key)
		if latest := latestVersion(t, database, spec).Number; latest != version {
			t.Fatalf("the settlement after the upload left the spec of %s at version %d, want the upload's version %d", key, latest, version)
		}
		fine, late := blockAskID(t, handler, key, "q-owed"), blockAskID(t, handler, key, "q-late")
		var named bool
		if err := database.Pool.QueryRow(ctx, `select node_kind is not null from asks where id = $1::uuid`, late).Scan(&named); err != nil || !named {
			t.Fatalf("the row of the ask %s names its node: %t, %v; want it named, so the read takes its answer off the node", late, named, err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			values ($1, $2, $3, $4, 'next Tuesday', now(), '{"kind":"user","id":"alice"}'::jsonb, 0, true)
		`, graphstore.KindAsk, late, graphstore.TupleAnsweredAt, graphstore.KindValue); err != nil {
			t.Fatalf("state answered_at \"next Tuesday\" on the node of ask %s: %v", late, err)
		}
		if owed := readCoverage(t, handler, key).OwedDocuments; !slices.Contains(owed, model.OwedDocument{ArtifactID: spec, Version: version}) {
			t.Fatalf("owed_documents of %s does not name the spec at version %d: %+v; the case reads a spec that owes its block index", key, version, owed)
		}

		view := readIssueDecisions(t, handler, key)
		ids := []string{}
		for _, question := range view.OpenQuestions {
			ids = append(ids, question.ID)
		}
		if !slices.Equal(ids, []string{fine}) {
			t.Errorf("while the spec of %s owes its block index, open_questions = %v, want the one ask whose node parses, %s", key, ids, fine)
		}
		if _, held := entriesByNode(view.Entries)[graphstore.KindAsk+":"+late]; held {
			t.Errorf("the entries hold an entry for the ask %s, whose node holds a value that does not parse", late)
		}
	})
}

// One ask row of the spec whose own stored value does not decode drops that one ask from the open
// questions and never fails the Decisions read (R4), as a value of its node that does not parse
// drops it: the read answers 200 with every other open question of the spec. Of two open asks, one
// row holds an author written by hand that is no actor. Every other read of the decisions drops
// that one ask the same way and answers 200 with the other: the Decisions read's entries, which
// walk the issue's tree, and the tree read with no view and with ?view=decisions. Each of them
// reaches the issue's asks through the walk of its conversation, which loads each ask and its
// parts as every read of the graph does (refs' loadAskNodes and itemBlockNodes), and that load
// leaves out an ask, and each part of it, whose maker does not decode rather than fail the read.
func TestASpecAskWhoseRowDoesNotDecodeIsLeftOutOfTheOpenQuestions(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: 20 * time.Millisecond})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::ask{#fine}\nWhich store?\n:::\n\n:::ask{#broken}\nWhich name?\n:::\n")
	waitForOpenQuestions(t, handler, issue.Key, 2)
	fine, broken := blockAskID(t, handler, issue.Key, "fine"), blockAskID(t, handler, issue.Key, "broken")
	if tag, err := database.Pool.Exec(context.Background(), `update asks set author = '"alice"'::jsonb where id = $1::uuid`, broken); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write an author that is no actor on the row of ask %s: %v", broken, err)
	}

	view := readIssueDecisions(t, handler, issue.Key)
	ids := []string{}
	for _, question := range view.OpenQuestions {
		ids = append(ids, question.ID)
	}
	if !slices.Equal(ids, []string{fine}) {
		t.Errorf("open_questions = %v, want the one ask whose row decodes, %s", ids, fine)
	}
	entries := entriesByNode(view.Entries)
	if _, held := entries[graphstore.KindAsk+":"+fine]; !held {
		t.Errorf("the entries hold no entry for the ask %s whose row decodes: %s", fine, entryJSON(t, view.Entries))
	}
	if _, held := entries[graphstore.KindAsk+":"+broken]; held {
		t.Errorf("the entries hold an entry for the ask %s, whose row holds an author that is no actor", broken)
	}
	for _, query := range []string{"", "?view=decisions"} {
		tree := treeRead(t, handler, issue.Key, query, "alice", false)
		if held := tree.nodesNamed(graphstore.KindAsk, fine); len(held) != 1 {
			t.Errorf("the tree read%s answers the ask %s, whose row decodes, %d times, want once; it answers %v", query, fine, len(held), tree.addresses())
		}
		if held := tree.nodesNamed(graphstore.KindAsk, broken); len(held) != 0 {
			t.Errorf("the tree read%s answers the ask %s, whose row holds an author that is no actor, as %s", query, broken, entryJSON(t, held))
		}
	}
}

// waitForOpenQuestions reads the view until it holds want open questions: settlement indexes a
// spec's ask blocks, and until it has the issue holds none. Its one bound is the test binary's
// deadline (testwait.UntilPolling), and its failure prints the questions it last read.
func waitForOpenQuestions(t *testing.T, handler http.Handler, key string, want int) (issueDecisions, []string) {
	t.Helper()
	var view issueDecisions
	questions := testwait.UntilPolling(t, 20*time.Millisecond, fmt.Sprintf("the issue's decisions to hold %d open questions (the questions last read)", want), func() ([]string, bool) {
		view = readIssueDecisions(t, handler, key)
		questions := []string{}
		for _, question := range view.OpenQuestions {
			questions = append(questions, question.Question)
		}
		return questions, len(questions) == want
	})
	return view, questions
}

// An issue born converted answers its spec's decision as an issue of one-document rooms answers
// it. The agent's decision, written into the spec's fragment of the issue room, is one entry: its
// text and its tree are its words, its document is the spec - the spec's id, its dispatch://
// address and the decision's block id - and its ref is the spec's address of its block. Its two
// shaped links, to the two paragraphs the spec's blocks read lists, each name the spec's own
// address of that block, `<KEY>/spec#<id>`, with the block's id, its text, the version that wrote
// it, the spec and the block's ref; so the spec's margin, which draws a decision's card beside each
// block of the spec on screen that a link reaches, draws one beside each paragraph. The release
// before's decisions member answers the same.
func TestAConvertedIssuesDecisionAnswersAsAnUnconvertedIssuesDecisionDoes(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "Notices arrive late.\n\nReaders poll for them.\n")
	spec := requireIssueRoomFragment(t, database, issue.Key, "spec")
	room := spec.nodeKey
	paragraphs := blockIDsByText(t, handler, spec.id)
	late, poll := paragraphs["Notices arrive late."], paragraphs["Readers poll for them."]
	if late == "" || poll == "" {
		t.Fatalf("the spec's blocks read = %v, want its two paragraphs", paragraphs)
	}
	pointer := func(block string) string { return "dispatch://" + issue.Key + "/spec#b-" + block }
	agentEdit(t, handler, spec.id, latestVersion(t, database, spec.id).Number+1, map[string]string{
		"op": "insert", "after": "end",
		"markdown": "## Decisions\n\n:::decision{#d1 shaped=\"" + pointer(late) + " " + pointer(poll) + "\"}\n" +
			"### Store notices in a table\n\nChosen for the simplest storage.\n:::\n",
	})

	target := "/api/v1/issues/" + issue.Key + "/decisions"
	view := decodeBody[issueDecisions](t, treesRequest(t, handler, target, true))
	if len(view.Entries) != 1 {
		t.Fatalf("the Decisions read answers the entries %s, want one, the agent's decision", entryJSON(t, view.Entries))
	}
	entry := requireEntry(t, view.Entries, "the agent's decision", refs.BlockNodeKind+":"+room+"#d1")
	if entry.Kind != "agent" || !strings.Contains(entry.Text, "Store notices in a table") ||
		!strings.Contains(entry.Text, "Chosen for the simplest storage.") || entry.TextSHA256 != decisionread.TextSum(entry.Text) {
		t.Errorf("the decision's entry is kind %q with the text %q and the sum %s, want an agent's, its words and their sum",
			entry.Kind, entry.Text, entry.TextSHA256)
	}
	requireDisplayTree(t, handler, target, "entries.0.text", "entries.0.text_tree")
	wantDocument := decisionEntryDocument{ArtifactID: spec.id, Ref: "dispatch://" + issue.Key + "/spec", BlockID: "d1", Block: room + "#d1"}
	if entry.Document == nil || *entry.Document != wantDocument {
		t.Errorf("the decision's entry names the document %s, want %s: the spec, whose fragment holds its block", entryJSON(t, entry.Document), entryJSON(t, wantDocument))
	}
	if want := pointer("d1"); entry.Ref != want {
		t.Errorf("the decision's entry answers the ref %q, want %q, the spec's address of its block", entry.Ref, want)
	}
	requireSpecLinks := func(what string, links []model.DecisionLink) {
		t.Helper()
		words := map[string]string{late: "Notices arrive late.", poll: "Readers poll for them."}
		if len(links) != len(words) {
			t.Fatalf("%s answers the shaped links %s, want one to each paragraph", what, entryJSON(t, links))
		}
		for _, link := range links {
			text, shaped := words[link.To.BlockID]
			if !shaped || link.To.Kind != refs.BlockNodeKind || link.To.ID != spec.pointed(link.To.BlockID) ||
				link.To.Ref != pointer(link.To.BlockID) || link.To.Document != "dispatch://"+issue.Key+"/spec" ||
				!strings.Contains(link.To.Text, text) || link.To.ChangedVersion != 1 || link.FromBlock != "d1" || link.Stale {
				t.Errorf("%s answers the shaped link %s, want one to a paragraph of the spec, at its node %s, at the spec's address %s, with its words, version 1 and the spec",
					what, entryJSON(t, link), spec.pointed("<id>"), pointer("<id>"))
			}
		}
	}
	requireSpecLinks("the decision's entry", entry.Shaped)
	// The margin's shape of the links (useMarginItems, linkReachesDocument): each one whose block the
	// spec holds, as the link's document says - its node is the issue room's, whichever document of
	// the room holds the block - placed beside the spec's block of its block id.
	placed := []string{}
	for _, link := range entry.Shaped {
		if link.To.BlockID != "" && link.To.Document == "dispatch://"+issue.Key+"/spec" {
			placed = append(placed, link.To.BlockID)
		}
	}
	slices.Sort(placed)
	if want := []string{late, poll}; !slices.Equal(placed, slices.Sorted(slices.Values(want))) {
		t.Errorf("the spec's margin places the decision beside the blocks %v, want the two paragraphs %v", placed, want)
	}
	if len(view.Decisions) != 1 || view.Decisions[0].Ref != entry.Ref || view.Decisions[0].Text != entry.Text {
		t.Fatalf("the old decisions member = %s, want the decision the entry answers", entryJSON(t, view.Decisions))
	}
	requireSpecLinks("the old decisions member", view.Decisions[0].Links)
}
