package api

//ci:reads packages/contracts/schemas/dispatch-openapi.json TestReferenceTargetsNameTheVersionThatLastHeldABlockAnEditRemoved

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphseedtest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	lastHeldSpec = ":::requirement{#removed_statement}\nThe statement to remove.\n:::\n\n" +
		":::requirement{#kept}\nThe statement to keep.\n:::\n"
	lastHeldProse = "A plain block in the issue's own fragment."
)

func lastHeldBlockRef(key, slug, block string) string {
	ref, _ := text.FormatBlockRef(text.Ref{Kind: "block", IssueKey: key, ID: slug, Block: block})
	return ref
}

func editLastHeldTree(t *testing.T, handler http.Handler, tree decisionDocument, ops ...map[string]any) {
	t.Helper()
	succeeded(t, "edit the tree fragment", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+tree.id+"/edits",
		map[string]any{"ops": ops}, "alice"))
}

func removeLastHeldNode(t *testing.T, rig *moveRig, key, address string) {
	t.Helper()
	as := personCaller(t, rig.handler, "alice")
	count := deletedCount(t, "count "+address, deleteNodeAs(as, key, address, true, -1, ""))
	if count < 1 {
		t.Fatalf("the dry run for %s takes %d nodes, want at least the node itself", address, count)
	}
	_, tag := placementOf(t, rig.database, refs.BlockNodeKind, address)
	if taken := deletedCount(t, "delete "+address, deleteNodeAs(as, key, address, false, count, tag)); taken != count {
		t.Fatalf("the DELETE of %s takes %d nodes, want %d", address, taken, count)
	}
}

// lastHeldQuiet waits until the store of rig owes no version: the rig's own quiet, and then no
// query drain either. A settlement that moves a query's record writes no version of that query: in
// the transaction that clears its pending row it marks the issue's queries owed
// (issue_query_marks), and it writes their versions after that commit
// (docs.Service.DrainOwedQueries). A read of a query's newest version made between the two is one
// version short. The read below counts both tables in one statement, so it finds neither row only
// once the drain's commit, which writes the versions and clears the mark, is in. That wait's bound
// is the test binary's deadline (testwait.UntilPolling), and its failure prints the two counts.
func lastHeldQuiet(t *testing.T, rig *moveRig) {
	t.Helper()
	rig.quiet(t)
	testwait.UntilPolling(t, 20*time.Millisecond,
		"the store to owe no settlement and no query drain (the rows of doc_settlements_pending, then of issue_query_marks)",
		func() ([2]int, bool) {
			var owed [2]int
			if err := rig.database.Pool.QueryRow(context.Background(),
				`select (select count(*) from doc_settlements_pending), (select count(*) from issue_query_marks)`,
			).Scan(&owed[0], &owed[1]); err != nil {
				t.Fatalf("read the settlements and the query drains the store owes: %v", err)
			}
			return owed, owed == [2]int{}
		})
}

type lastHeldWant struct {
	ref     string
	version int // zero for no last_held
	gone    bool
}

func checkLastHeld(t *testing.T, handler http.Handler, wants ...lastHeldWant) {
	t.Helper()
	references := make([]string, len(wants))
	for i, want := range wants {
		references[i] = want.ref
	}
	response := referenceTargetsRequest(t, handler, references, "")
	if response.Code != http.StatusOK {
		t.Fatalf("read targets %v: status=%d body=%s", references, response.Code, response.Body.String())
	}
	got := decodeBody[model.ReferenceTargets](t, response).Targets
	wire := decodeBody[struct {
		Targets []map[string]json.RawMessage `json:"targets"`
	}](t, response).Targets
	if len(got) != len(wants) {
		t.Fatalf("read %d targets, want %d: %s", len(got), len(wants), response.Body.String())
	}
	for i, want := range wants {
		member := got[i]
		if member.Ref != want.ref || member.Gone != want.gone || (member.Gone && member.Name != "") {
			t.Errorf("%s answered %+v, want the echoed ref and gone=%t (unnamed if gone)", want.ref, member, want.gone)
		}
		t.Logf("%s last_held = %q", member.Ref, member.LastHeld)
		if want.version == 0 {
			if _, present := wire[i]["last_held"]; present {
				t.Errorf("%s answers last_held %q, want the member omitted", member.Ref, member.LastHeld)
			}
			continue
		}
		wantRef, err := parseRequestRef(want.ref, testServerURL)
		if err != nil {
			t.Fatal(err)
		}
		wantRef.Pin = want.version
		parsed, err := parseRequestRef(member.LastHeld, testServerURL)
		if err != nil || parsed != wantRef || member.LastHeld != fmt.Sprintf("%s@v%d", want.ref, want.version) {
			t.Errorf("%s last_held = %q, parser result %+v (%v), want %s@v%d", member.Ref, member.LastHeld, parsed, err, want.ref, want.version)
		}
	}
}

func TestReferenceTargetsNameTheVersionThatLastHeldABlockAnEditRemoved(t *testing.T) {
	rig := newMoveRig(t, "HELD")
	issue := createDigestIssue(t, rig.handler, rig.database, "HELD", "Last held after a room edit", lastHeldSpec, false)
	key := issue.Key
	spec := readDecisionDocument(t, rig.database, key, "spec")
	tree := readDecisionDocument(t, rig.database, key, issueTreeName)
	if spec.room != tree.id || spec.id == tree.id {
		t.Fatalf("spec %+v is not a fragment of issue tree %+v", spec, tree)
	}
	editLastHeldTree(t, rig.handler, tree, map[string]any{"op": "insert", "after": "end", "markdown": lastHeldProse +
		"\n\n:::requirement{#tree-statement}\nThe tree fragment's statement.\n:::\n"})
	_, prose := textBlockOf(t, rig.database, tree.id, "paragraph", lastHeldProse)
	// These edits leave the removed blocks' words unchanged. The answer must use the last
	// version that held them, not the version of their last non-null text row.
	editLastHeldTree(t, rig.handler, tree, map[string]any{"op": "insert", "after": "end", "markdown": "A second paragraph stands."})
	editSpec(t, rig.handler, key, []map[string]any{{"op": "replace", "find": "The statement to keep.", "with": "The kept statement, revised."}})
	lens := moveDrainLens(t, rig.handler, key)
	lastHeldQuiet(t, rig)
	beforeSpec := newestQueryVersion(t, rig.database, spec.id)
	beforeTree := newestQueryVersion(t, rig.database, tree.id)
	if beforeSpec.readSeq == nil {
		t.Fatalf("the spec version %d before editing has no recorded cut", beforeSpec.number)
	}
	editSpec(t, rig.handler, key, []map[string]any{{"op": "delete", "block": "removed_statement"}})
	lastHeldQuiet(t, rig)
	beforeTreeStatement := newestQueryVersion(t, rig.database, spec.id).number
	if nodes := treeNodes(t, rig.handler, key, fmt.Sprintf("?view=spec&v=%d", beforeTreeStatement)); nodes[tree.address("tree-statement")].ID == "" {
		t.Fatalf("the spec query version %d does not hold the statement in the tree fragment", beforeTreeStatement)
	}
	editLastHeldTree(t, rig.handler, tree, map[string]any{"op": "delete", "block": prose},
		map[string]any{"op": "delete", "block": "tree-statement"})
	// A version after the removal must not replace the last version that held either block.
	editSpec(t, rig.handler, key, []map[string]any{{"op": "replace", "find": "The kept statement, revised.", "with": "The kept statement, revised again."}})
	editLastHeldTree(t, rig.handler, tree, map[string]any{"op": "insert", "after": "end", "markdown": "A later, unrelated paragraph."})
	succeeded(t, "take the lens gone", writeStateAs(t, rig.handler, key, lens, graphstore.StateGone, "alice"))
	lastHeldQuiet(t, rig)
	checkLastHeld(t, rig.handler,
		lastHeldWant{lastHeldBlockRef(key, "spec", "removed_statement"), beforeSpec.number, true},
		lastHeldWant{lastHeldBlockRef(key, "spec", "tree-statement"), beforeTreeStatement, true},
		lastHeldWant{lastHeldBlockRef(key, issueTreeName, prose), beforeTree.number, true},
		lastHeldWant{lastHeldBlockRef(key, "spec", "kept"), 0, false},
		lastHeldWant{"dispatch://" + lens, 0, true},
	)
	// The contracts module is rendered from the committed OpenAPI document and never committed
	// itself, so the document is what declares the field (`schemaFields`).
	fields := schemaFields(committedDocument(t, repositoryRoot(t)))["ReferenceTarget"]
	if !slices.Contains(fields, "last_held") {
		t.Errorf("%s ReferenceTarget properties = %v, want last_held (regenerate from the route table)", OpenAPIDocumentPath, fields)
	}
}

func TestReferenceTargetsNameTheVersionThatLastHeldAGoneNode(t *testing.T) {
	rig := newMoveRig(t, "GONE")
	issue := createDigestIssue(t, rig.handler, rig.database, "GONE", "Last held after DELETE", lastHeldSpec, false)
	key := issue.Key
	statement := lastHeldBlockRef(key, "spec", "removed_statement")
	uploadDocument(t, rig.handler, "/api/v1/issues/"+key, "verification.md",
		graphEditCheck("no-result", "The check has no result.", lastHeldBlockRef(key, "spec", "kept")))
	uploadDocument(t, rig.handler, "/api/v1/issues/"+key, "plan.md",
		":::plan_task{#task serves=\""+lastHeldBlockRef(key, "spec", "kept")+"\"}\nThe task to remove.\n:::\n")
	tree := readDecisionDocument(t, rig.database, key, issueTreeName)
	editLastHeldTree(t, rig.handler, tree, map[string]any{"op": "insert", "after": "end", "markdown": lastHeldProse})
	_, prose := textBlockOf(t, rig.database, tree.id, "paragraph", lastHeldProse)
	lens := moveDrainLens(t, rig.handler, key)
	succeeded(t, "take the lens gone", writeStateAs(t, rig.handler, key, lens, graphstore.StateGone, "alice"))
	lastHeldQuiet(t, rig)

	var wants []lastHeldWant
	for _, want := range []struct{ slug, block, query string }{
		{"spec", "removed_statement", graphstore.QuerySpec},
		{model.ArtifactSlugVerification, "no-result", graphstore.QueryVerification},
		{model.ArtifactSlugPlan, "task", graphstore.QueryPlan},
		{issueTreeName, prose, ""},
	} {
		doc := readDecisionDocument(t, rig.database, key, want.slug)
		before := newestQueryVersion(t, rig.database, doc.id)
		address := doc.address(want.block)
		if want.query != "" {
			if _, held := treeNodes(t, rig.handler, key, fmt.Sprintf("?view=%s&v=%d", want.query, before.number))[address]; !held {
				t.Fatalf("the %s query version %d did not hold %s before DELETE", want.query, before.number, address)
			}
		}
		removeLastHeldNode(t, rig, key, address)
		lastHeldQuiet(t, rig)
		after := newestQueryVersion(t, rig.database, doc.id)
		if after.number <= before.number {
			t.Fatalf("DELETE %s left version %d, want a version after %d", address, after.number, before.number)
		}
		if want.query != "" {
			if _, held := treeNodes(t, rig.handler, key, fmt.Sprintf("?view=%s&v=%d", want.query, after.number))[address]; held {
				t.Fatalf("the %s query version %d still holds %s after DELETE", want.query, after.number, address)
			}
		}
		wants = append(wants, lastHeldWant{lastHeldBlockRef(key, want.slug, want.block), before.number, true})
	}
	checkLastHeld(t, rig.handler, append(wants,
		lastHeldWant{lastHeldBlockRef(key, "spec", "kept"), 0, false},
		lastHeldWant{"dispatch://" + lens, 0, true},
		lastHeldWant{statement, wants[0].version, true}, // the same reference in the same batch
	)...)
}

func TestReferenceTargetsGiveNoLastHeldWhereTheCallerMayNotSeeTheNode(t *testing.T) {
	rig := newMoveRig(t, "HIDE")
	issue := createDigestIssue(t, rig.handler, rig.database, "HIDE", "Private history", lastHeldSpec, false)
	key := issue.Key
	spec := readDecisionDocument(t, rig.database, key, "spec")
	ref := lastHeldBlockRef(key, "spec", "removed_statement")
	unknown := lastHeldBlockRef(key, "spec", "unknown")
	lastHeldQuiet(t, rig)
	before := newestQueryVersion(t, rig.database, spec.id).number
	editSpec(t, rig.handler, key, []map[string]any{{"op": "delete", "block": "removed_statement"}})
	lastHeldQuiet(t, rig)
	noHistory := func(what string, expected ...string) {
		t.Helper()
		response := dispatchRequest(t, rig.handler, http.MethodGet, referenceTargetsOf(expected...), nil, "bob")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
		}
		members := decodeBody[model.ReferenceTargets](t, response).Targets
		if len(members) != len(expected) {
			t.Fatalf("%s: got %d targets, want %d", what, len(members), len(expected))
		}
		for i, address := range expected {
			if want := (model.ReferenceTarget{Ref: address, Kind: "block", Gone: true}); !reflect.DeepEqual(members[i], want) {
				t.Errorf("%s: %s reads %+v, want exactly %+v", what, address, members[i], want)
			}
		}
		if strings.Contains(response.Body.String(), `"last_held"`) {
			t.Errorf("%s: hidden block names a version: %s", what, response.Body.String())
		}
	}
	checkLastHeld(t, rig.handler, lastHeldWant{ref, before, true})
	seedExec(t, rig.database, "make the issue's project private to its owner", `update projects set owner = 'alice', visibility = 'private' where key = 'HIDE'`)
	noHistory("outside the project", ref, unknown)
	checkLastHeld(t, rig.handler, lastHeldWant{ref, before, true})
	seedExec(t, rig.database, "return the project to public", `update projects set visibility = 'public' where key = 'HIDE'`)
	bob := dispatchRequest(t, rig.handler, http.MethodGet, referenceTargetsOf(ref), nil, "bob")
	if bob.Code != http.StatusOK || decodeBody[model.ReferenceTargets](t, bob).Targets[0].LastHeld == "" {
		t.Fatalf("public control: Bob's history request reads status=%d body=%s", bob.Code, bob.Body.String())
	}

	// The graph writer does not admit author-only issues, artifacts or document blocks. Seed
	// their visibility with the shared test helper, leaving the real edit's history unchanged.
	ctx := t.Context()
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := graphseedtest.SeedAuthorOnly(ctx, tx, graphstore.Node{
		Kind: refs.BlockNodeKind, ID: spec.address("removed_statement"), Supertag: "requirement", State: graphstore.StateGone,
		Project: "HIDE", MadeAt: newestStateTuple(t, rig.database, refs.BlockNodeKind, spec.address("removed_statement")).At,
		MadeBy: model.Actor{Kind: "user", ID: "alice"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	noHistory("another author's gone block", ref, lastHeldBlockRef(key, issueTreeName, "removed_statement"), unknown)
	checkLastHeld(t, rig.handler, lastHeldWant{ref, before, true})
}

func TestReferenceTargetsGiveLastHeldToBlocksAlone(t *testing.T) {
	rig := newMoveRig(t, "ONLY")
	issue := createDigestIssue(t, rig.handler, rig.database, "ONLY", "Only document blocks have versions", lastHeldSpec, false)
	key := issue.Key
	lens := created201[lensInvoked](t, "invoke a lens with a finding", dispatchRequest(t, rig.handler, http.MethodPost,
		"/api/v1/issues/"+key+"/lenses", map[string]any{"on": "dispatch://" + key, "kind": graphstore.LensKindPreMortem,
			"findings": []string{"A finding's words are not a document."}}, "alice"))
	if len(lens.Findings) != 1 {
		t.Fatalf("lens answers %d findings, want one", len(lens.Findings))
	}
	risk := created201[struct{ ID string }](t, "write a risk", dispatchRequest(t, rig.handler, http.MethodPost,
		"/api/v1/issues/"+key+"/risks", map[string]any{"lens": lens.Address, "heading": "A risk",
			"early_warning": "A warning", "prevention": "A prevention"}, "alice"))
	constraint := created201[constraintRead](t, "write a constraint", dispatchRequest(t, rig.handler, http.MethodPost,
		"/api/v1/issues/"+key+"/constraints", map[string]any{"standard": "RFC 9110", "reference": "section 13.1.1"}, "alice"))
	finding := refs.BlockRef(refs.StoreRef(graphstore.KindLens, lens.ID), lens.Findings[0].BlockID)
	live := lastHeldWant{lastHeldBlockRef(key, "spec", "kept"), 0, false}
	others := []string{refs.StoreRef(graphstore.KindLens, lens.ID), finding,
		refs.StoreRef(graphstore.KindRisk, risk.ID), refs.StoreRef(graphstore.KindConstraint, constraint.ID)}
	wants := []lastHeldWant{live}
	for _, ref := range others {
		wants = append(wants, lastHeldWant{ref, 0, false})
	}
	checkLastHeld(t, rig.handler, wants...)
	succeeded(t, "take the lens and its subtree gone", writeStateAs(t, rig.handler, key, "lens/"+lens.ID, graphstore.StateGone, "alice"))
	succeeded(t, "take the constraint gone", writeStateAs(t, rig.handler, key, "constraint/"+constraint.ID, graphstore.StateGone, "alice"))
	for i := 1; i < len(wants); i++ {
		wants[i].gone = true
	}
	checkLastHeld(t, rig.handler, wants...)
}

func TestReferenceTargetsETagChangesWithLastHeld(t *testing.T) {
	rig := newMoveRig(t, "TAG")
	issue := createDigestIssue(t, rig.handler, rig.database, "TAG", "ETag for block history", lastHeldSpec, false)
	ref := lastHeldBlockRef(issue.Key, "spec", "removed_statement")
	cut := []map[string]any{{"op": "delete", "block": "removed_statement"}}
	editSpec(t, rig.handler, issue.Key, cut)
	lastHeldQuiet(t, rig)
	first, tag := referenceTargets(t, rig.handler, []string{ref})
	if len(first) != 1 || !first[0].Gone || first[0].LastHeld == "" {
		t.Fatalf("first removal reads %+v, want one gone member with last_held", first)
	}
	if unchanged := referenceTargetsRequest(t, rig.handler, []string{ref}, tag); unchanged.Code != http.StatusNotModified {
		t.Fatalf("unchanged history answers %d %s, want 304", unchanged.Code, unchanged.Body.String())
	}
	editSpec(t, rig.handler, issue.Key, []map[string]any{{"op": "insert", "after": "end",
		"markdown": ":::requirement{#removed_statement}\nThe restored statement.\n:::\n"}})
	editSpec(t, rig.handler, issue.Key, cut)
	lastHeldQuiet(t, rig)
	next := referenceTargetsRequest(t, rig.handler, []string{ref}, tag)
	if next.Code != http.StatusOK || next.Header().Get("ETag") == tag {
		t.Fatalf("history after the second removal answers %d %s under %s, want 200 under a new ETag", next.Code, next.Body.String(), next.Header().Get("ETag"))
	}
	second := decodeBody[model.ReferenceTargets](t, next).Targets
	if len(second) != 1 || second[0].LastHeld == "" || second[0].LastHeld == first[0].LastHeld {
		t.Fatalf("the first removal reads %+v and the second reads %+v, want a new last_held", first, second)
	}
	t.Logf("%s last_held changes from %q to %q", ref, first[0].LastHeld, second[0].LastHeld)
	first[0].LastHeld, second[0].LastHeld = "", ""
	if !reflect.DeepEqual(first, second) {
		t.Errorf("the ETag control changed more than last_held: %+v and %+v", first, second)
	}
}
