package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedPointerAttribute writes the supertag node of typeName, the attribute node of its attrName
// pointer with targets (nil for no limit), and the attribute_of tuple that ties them, as migration
// 20261109051232 seeds the shipped schema's nodes. A test that swaps in a type the shipped schema
// does not hold - claim - writes these so the store holds what the swapped-in schema declares, as
// the type migration that declares a type writes its nodes in a deployment, whose installed schema
// is read from them (pmdoc/schemanodes): the store's writers take a claim's node only once its
// supertag node stands (graphstore.ErrSupertagNotHeld), graph_edges' tuple arm reads a pointer's
// allowed targets from that attribute node (block_pointer_targets), and a stamped pointer row is
// answered by that arm, so without the node the arm finds no limit at all. It is docs' helper of
// the same name: graphstore's TestEveryStoreWriteCallSiteIsListed refuses a raw write of the store
// outside a test file, so the two packages cannot share it through pmdoctest.
func seedPointerAttribute(t *testing.T, database *store.Store, typeName, attrName string, targets []string) {
	t.Helper()
	ctx := context.Background()
	attributes := map[string]any{"kind": "block[]", "name": attrName, "store_name": attrName, "position": 0}
	if targets != nil {
		attributes["targets"] = targets
	}
	encoded, err := json.Marshal(attributes)
	if err != nil {
		t.Fatalf("encode the %s/%s attribute: %v", typeName, attrName, err)
	}
	for _, seed := range []struct {
		what, statement string
		args            []any
	}{
		{"the " + typeName + " supertag node", `
			insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			values ('supertag', $1, 'supertag', 'project', 'live', '{}'::jsonb, now(), '{"kind":"system","id":"block-schema-seed"}', 0)
			on conflict (kind, id) do nothing`, []any{typeName}},
		{"the " + typeName + "/" + attrName + " attribute node", `
			insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			values ('attribute', $1, 'attribute', 'project', 'live', $2, now(), '{"kind":"system","id":"block-schema-seed"}', 0)
			on conflict (kind, id) do update set attributes = excluded.attributes`, []any{typeName + "/" + attrName, encoded}},
		{"the attribute_of tying " + typeName + "/" + attrName + " to its supertag", `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
			select 'attribute', $1, 'attribute_of', 'supertag', $2, now(), '{"kind":"system","id":"block-schema-seed"}', 0, null, true
			where not exists (
				select 1 from graph_tuples held
				where held.from_kind = 'attribute' and held.from_id = $1 and held.kind = 'attribute_of'
				  and held.to_kind = 'supertag' and held.to_id = $2 and held.source_event = 0 and held.asserted)`,
			[]any{typeName + "/" + attrName, typeName}},
	} {
		if _, err := database.Pool.Exec(ctx, seed.statement, seed.args...); err != nil {
			t.Fatalf("seed %s: %v", seed.what, err)
		}
	}
}

// edgeSources is each edge as "<kind> <node kind> <node id>", sorted.
func edgeSources(edges []model.GraphEdge) []string {
	sources := make([]string, len(edges))
	for index, edge := range edges {
		sources[index] = edge.Kind + " " + edge.Node.Kind + " " + edge.Node.ID
	}
	sort.Strings(sources)
	return sources
}

// requireEdgeSources fails unless edges are exactly want (edgeSources), in any order.
func requireEdgeSources(t *testing.T, stage string, edges []model.GraphEdge, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := edgeSources(edges); !reflect.DeepEqual(got, want) && (len(got) != 0 || len(want) != 0) {
		t.Fatalf("%s: edges = %q, want %q", stage, got, want)
	}
}

// A3: a saved claim whose serves attribute names a requirement is a link named serves from the
// claim's block to the requirement, listed and queried by that name, and is the only link the
// pointer makes: neither the claim nor its document lists a mentions edge beside it.
func TestASavedBlockPointerIsALinkNamedByItsAttribute(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	handler, database := blockIndexHandler(t)
	// BlockPointerSchema(nil)'s claim: a serves pointer at requirements alone.
	seedPointerAttribute(t, database, "claim", "serves", []string{"requirement"})
	issue, _ := pointerSpec(t, handler)
	specRef := "dispatch://" + issue.Key + "/spec"
	r1 := specRef + "#b-r1"
	checksRef := "dispatch://" + issue.Key + "/artifact/checks-md"
	// The spec is a fragment of its issue's room, whose tree keys its blocks' nodes; checks.md plays
	// no role, so it is a room of its own, whose ref_key keys its blocks' nodes (R5 of this branch's
	// spec).
	nodes := readDecisionDocument(t, database, issue.Key, "spec").nodeKey
	c1 := issue.Key + "/checks-md#c1"
	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", r1)); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim: status=%d body=%s", response.Code, response.Body.String())
	}
	checks := readChecks(t, database, issue.Key).ID

	requireEdgeSources(t, "links to r1", graphEdges(t, handler, url.Values{"to": {r1}}).Edges, "serves block "+c1)
	requireEdgeSources(t, "serves links to r1", graphEdges(t, handler, url.Values{"to": {r1}, "kind": {"serves"}}).Edges, "serves block "+c1)
	requireEdgeSources(t, "mentions of r1", graphEdges(t, handler, url.Values{"to": {r1}, "kind": {"mentions"}}).Edges)
	// A kind no build of this fleet emits is listed in unknown_kinds rather than refused, as a
	// newer build's `block[]` attribute name would be during a release.
	unknown := graphEdges(t, handler, url.Values{"to": {r1}, "kind": {"nope"}})
	requireEdgeSources(t, "an unknown kind", unknown.Edges)
	if !reflect.DeepEqual(unknown.UnknownKinds, []string{"nope"}) {
		t.Fatalf("an unknown kind: unknown_kinds = %q, want [nope]", unknown.UnknownKinds)
	}

	atR1 := 0
	for _, edge := range graphEdges(t, handler, url.Values{"to": {specRef}}).Edges {
		if edge.Node.Kind == "artifact" && edge.Node.ID == checks {
			t.Fatalf("the spec's links list checks.md's own %s edge beside its claim's: %#v", edge.Kind, edge)
		}
		if edge.Block != nil && edge.Block.ID == nodes+"#r1" && edge.Node.ID == c1 && edge.Kind == "serves" {
			atR1++
		}
	}
	if atR1 != 1 {
		t.Fatalf("the spec's links at r1 from the claim = %d, want 1 serves edge", atR1)
	}
	links := readBlockLinks(t, handler, checksRef, specRef).Links
	if len(links) != 1 || links[0].Kind != "serves" || links[0].From.BlockID != "c1" || links[0].To.BlockID != "r1" {
		t.Fatalf("block links from checks.md to the spec = %#v, want c1 serves r1", links)
	}

	// The control: a claim pointing at nothing links nothing.
	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", "")); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim with no pointer: status=%d body=%s", response.Code, response.Body.String())
	}
	requireEdgeSources(t, "links to r1 once the claim points at nothing", graphEdges(t, handler, url.Values{"to": {r1}}).Edges)
	if links := readBlockLinks(t, handler, checksRef, specRef).Links; len(links) != 0 {
		t.Fatalf("block links once the claim points at nothing = %#v, want none", links)
	}
}

// A pointer and a prose citation of one target are separate edges, each from the block holding
// it: the claim's serves and supports edges from the claim, and the mention from the claim's
// paragraph. Dropping the prose keeps the pointer's edge as it was first written.
func TestAPointerAndAProseCitationOfOneTargetAreTwoEdges(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(pmdoc.BlockAttributes{
		{Name: "serves", BlockAttributeSchema: pmdoc.BlockAttributeSchema{Kind: pmdoc.BlockAttributeBlockList, Targets: []string{"requirement"}}},
		{Name: "supports", BlockAttributeSchema: pmdoc.BlockAttributeSchema{Kind: pmdoc.BlockAttributeBlockList}},
	}))
	handler, database := blockIndexHandler(t)
	// This schema's claim: a serves pointer at requirements alone, a supports pointer at any block.
	seedPointerAttribute(t, database, "claim", "serves", []string{"requirement"})
	seedPointerAttribute(t, database, "claim", "supports", nil)
	issue, _ := pointerSpec(t, handler)
	r1 := "dispatch://" + issue.Key + "/spec#b-r1"
	// The spec is a fragment of its issue's room; checks.md plays no role, so it is a room of its
	// own, whose ref_key keys its blocks' nodes (R5 of this branch's spec).
	c1 := issue.Key + "/checks-md#c1"
	claim := func(body string) string {
		return ":::claim{#c1 supports=\"" + r1 + "\" serves=\"" + r1 + "\"}\n" + body + "\n:::\n"
	}
	prose := "It holds, per " + r1 + "."
	if response := uploadChecks(t, handler, issue.Key, claim(prose)); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim: status=%d body=%s", response.Code, response.Body.String())
	}
	checks := readChecks(t, database, issue.Key).ID
	paragraph := blockIDsByText(t, handler, checks)[prose]
	if paragraph == "" {
		t.Fatalf("GET …/blocks of checks.md lists no block for %q", prose)
	}
	requireEdgeSources(t, "links to r1", graphEdges(t, handler, url.Values{"to": {r1}}).Edges,
		"serves block "+c1, "supports block "+c1, "mentions block "+issue.Key+"/checks-md#"+paragraph)
	servesVersion := func() int {
		t.Helper()
		for _, row := range readBlockRefs(t, database, checks) {
			if row.BlockID == "c1" && row.Kind == "serves" {
				return row.CreatedVersion
			}
		}
		t.Fatal("checks.md holds no serves row from c1")
		return 0
	}
	if version := servesVersion(); version != 1 {
		t.Fatalf("the serves edge was created at version %d, want 1", version)
	}

	if response := uploadChecks(t, handler, issue.Key, claim("It holds.")); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim without its prose citation: status=%d body=%s", response.Code, response.Body.String())
	}
	requireEdgeSources(t, "links to r1 without the prose citation", graphEdges(t, handler, url.Values{"to": {r1}}).Edges,
		"serves block "+c1, "supports block "+c1)
	if version := servesVersion(); version != 1 {
		t.Fatalf("the serves edge was created at version %d after the prose went, want still 1", version)
	}
}

// A block[] attribute links its typed block to each distinct target it names, once, under the
// attribute's name, and makes no mentions link for those values, though each value is written in
// the block's opening line and the extraction finds it there as a mention too. A value repeated as
// written is refused before it is stored (pmdoc.ParseBlockPointerValue), so one target is named
// twice by an unpinned and a pinned value, which refs.pointerPairs counts as one target. That case
// comes first, on a claim with no link yet: two equal pairs there are one insert writing one new
// row twice, which the store refuses, while against a link already stored they change nothing.
func TestABlockPointerAttributeLinksEachDistinctTargetOnce(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	handler, database := blockIndexHandler(t)
	// BlockPointerSchema(nil)'s claim: a serves pointer at requirements alone.
	seedPointerAttribute(t, database, "claim", "serves", []string{"requirement"})
	createReferenceAPIProject(t, handler, "ACME")
	issue := createSpecIssue(t, handler, "ACME", ":::requirement{#r1}\nA requirement.\n:::\n\n:::requirement{#r2}\nAnother requirement.\n:::\n")
	specRef := "dispatch://" + issue.Key + "/spec"
	r1, r2 := specRef+"#b-r1", specRef+"#b-r2"
	// The spec is a fragment of its issue's room, whose tree keys its blocks' nodes, and a pointer
	// is stored at the node its address resolves to (refs.ResolvedBlockSQL); checks.md plays no
	// role, so it is a room of its own, whose ref_key keys its blocks' nodes (R5 of this branch's
	// spec).
	nodes := readDecisionDocument(t, database, issue.Key, "spec").nodeKey
	r1Node, r2Node := nodes+"#r1", nodes+"#r2"
	c1 := issue.Key + "/checks-md#c1"

	requireRows := func(stage string, want ...string) {
		t.Helper()
		var got []string
		for _, row := range readBlockRefs(t, database, readChecks(t, database, issue.Key).ID) {
			if row.BlockID == "c1" {
				got = append(got, row.Kind+" "+row.ToID)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: c1's block_refs rows = %q, want %q", stage, got, want)
		}
	}

	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", r1+" "+r1+"@v1")); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim that names r1 unpinned and pinned: status=%d body=%s", response.Code, response.Body.String())
	}
	requireRows("one target named twice", "serves "+r1Node)
	requireEdgeSources(t, "one target named twice: links to r1", graphEdges(t, handler, url.Values{"to": {r1}}).Edges, "serves block "+c1)

	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", r1+" "+r2)); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim that names two targets: status=%d body=%s", response.Code, response.Body.String())
	}
	requireRows("two targets", "serves "+r1Node, "serves "+r2Node)
	for _, target := range []string{r1, r2} {
		requireEdgeSources(t, "two targets: links to "+target, graphEdges(t, handler, url.Values{"to": {target}}).Edges, "serves block "+c1)
	}
}

// The graph lists a pointer's edge only while its target has a type the attribute allows, and no
// other edge stands in for it while it is hidden; the document-level outgoing read lists the
// citation whatever the target's type (D7). A target retyped back, or deleted and restored, is
// listed again.
func TestABlockPointerIsListedOnlyWhileItsTargetHasAnAllowedType(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	handler, database := blockIndexHandler(t)
	// BlockPointerSchema(nil)'s claim: a serves pointer at requirements alone.
	seedPointerAttribute(t, database, "claim", "serves", []string{"requirement"})
	issue, _ := pointerSpec(t, handler)
	specRef := "dispatch://" + issue.Key + "/spec"
	r1 := specRef + "#b-r1"
	// The spec is a fragment of its issue's room, whose tree keys its blocks' nodes; a reference
	// keeps the address written, which resolves to that node (refs.ResolvedBlockSQL). checks.md
	// plays no role, so it is a room of its own, whose ref_key keys its blocks' nodes (R5 of this
	// branch's spec).
	nodes := readDecisionDocument(t, database, issue.Key, "spec").nodeKey
	r1Node, r1Written := nodes+"#r1", issue.Key+"/spec#r1"
	checksRef := "dispatch://" + issue.Key + "/artifact/checks-md"
	c1 := issue.Key + "/checks-md#c1"
	if response := uploadChecks(t, handler, issue.Key, claimMarkdown("c1", r1)); response.Code != http.StatusCreated {
		t.Fatalf("upload the claim: status=%d body=%s", response.Code, response.Body.String())
	}
	checks := readChecks(t, database, issue.Key).ID

	// target is the state of r1: "requirement", "callout" or "" (deleted).
	requireListed := func(stage, target string) {
		t.Helper()
		listed := target == "requirement"
		want := 0
		if listed {
			want = 1
		}
		switch target {
		case "requirement":
			requireEdgeSources(t, stage+": links to r1", graphEdges(t, handler, url.Values{"to": {r1}}).Edges, "serves block "+c1)
		case "callout":
			requireEdgeSources(t, stage+": links to r1", graphEdges(t, handler, url.Values{"to": {r1}}).Edges)
		default:
			requireReferenceNotFound(t, handler, url.Values{"to": {r1}}, `no block "r1" in `+specRef)
		}
		if links := readBlockLinks(t, handler, checksRef, specRef).Links; len(links) != want || listed && links[0].Kind != "serves" {
			t.Fatalf("%s: block links from checks.md to the spec = %#v, want %d serves link", stage, links, want)
		}
		toR1 := 0
		for _, edge := range graphEdges(t, handler, url.Values{"from": {checksRef}}).Edges {
			if edge.Node.ID == r1Node {
				if edge.Kind != "serves" || edge.Block == nil || edge.Block.ID != c1 {
					t.Fatalf("%s: checks.md's links to r1 hold %#v, want only c1's serves edge", stage, edge)
				}
				toR1++
			}
		}
		if toR1 != want {
			t.Fatalf("%s: checks.md's links to r1 = %d, want %d", stage, toR1, want)
		}
		atR1 := 0
		for _, edge := range graphEdges(t, handler, url.Values{"to": {specRef}}).Edges {
			if edge.Node.Kind == "artifact" && edge.Node.ID == checks {
				t.Fatalf("%s: the spec's links list checks.md's own %s edge: %#v", stage, edge.Kind, edge)
			}
			if edge.Block != nil && edge.Block.ID == r1Node {
				if edge.Kind != "serves" || edge.Node.ID != c1 {
					t.Fatalf("%s: the spec's links at r1 hold %#v, want only c1's serves edge", stage, edge)
				}
				atR1++
			}
		}
		if atR1 != want {
			t.Fatalf("%s: the spec's links at r1 = %d, want %d", stage, atR1, want)
		}
		if target != "" {
			outgoing := decodeBody[model.ArtifactReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+checks+"/references", nil, "alice")).Outgoing
			if len(outgoing) != 1 || outgoing[0].Kind != "block" || outgoing[0].ToID != r1Written {
				t.Fatalf("%s: checks.md's outgoing references = %#v, want r1", stage, outgoing)
			}
		}
	}
	requireListed("pointing at a requirement", "requirement")

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "retype", "block": "r1", "type": "callout"}})
	requireListed("r1 retyped to a callout", "callout")

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "retype", "block": "r1", "type": "requirement"}})
	requireListed("r1 retyped back to a requirement", "requirement")

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "delete", "block": "r1"}})
	requireListed("r1 deleted", "")

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::requirement{#r1}\nA requirement.\n:::\n"}})
	requireListed("r1 restored", "requirement")
}

// tasksNeedingEachOther is two plan tasks, t-first and t-second, each of whose needs names the
// other as a block of document, a dispatch:// reference to a document of an issue.
func tasksNeedingEachOther(document string) string {
	return ":::plan_task{#t-first needs=\"" + document + "#b-t-second\"}\nThe first task.\n:::\n\n" +
		":::plan_task{#t-second needs=\"" + document + "#b-t-first\"}\nThe second task.\n:::\n"
}

// Two plan tasks of a converted issue that need each other, each naming the other at its
// document's own address, state needs tuples that name the other's node in the issue's room
// (`<KEY>/tree#<id>`), and the ordered read (refs.PlanOrder) answers the two as a cycle and gives
// neither a position, whichever version of their document writes them. A plan.md's first upload
// creates the document and joins it to the issue's room in one transaction, which writes the
// document's document_rooms row only in its write phase, after the room's pass has resolved the
// pointers' far ends; its second version is written once that row stands; and the issue's create
// writes the two tasks in its spec, joined to the room that same transaction creates. The issue
// create takes no plan, so its spec is the one document it writes them in.
func TestPlanTasksThatNeedEachOtherNameTheirRoomNodesFromTheFirstVersion(t *testing.T) {
	ctx := context.Background()
	handler, database := blockIndexHandler(t)
	reader := model.Actor{Kind: "user", ID: "alice"}
	for _, write := range []struct {
		name, project, slug string
		// writes creates the project's issue of key, a converted issue, and writes the two tasks
		// into its document slug.
		writes func(t *testing.T, project, key string) model.Issue
	}{{
		name: "the plan's first version", project: "FIRST", slug: "plan-md",
		writes: func(t *testing.T, project, key string) model.Issue {
			issue, _ := createRoomIssue(t, handler, database, project, "A plan written once", "The spec's words.\n", "")
			uploadAs(t, handler, key, "plan.md", tasksNeedingEachOther("dispatch://"+key+"/artifact/plan-md"), "alice")
			return issue
		},
	}, {
		name: "the plan's second version", project: "SECOND", slug: "plan-md",
		writes: func(t *testing.T, project, key string) model.Issue {
			issue, _ := createRoomIssue(t, handler, database, project, "A plan written twice", "The spec's words.\n", "")
			uploadAs(t, handler, key, "plan.md", "The plan holds no task yet.\n", "alice")
			uploadAs(t, handler, key, "plan.md", tasksNeedingEachOther("dispatch://"+key+"/artifact/plan-md"), "alice")
			return issue
		},
	}, {
		name: "the spec the issue is created with", project: "BORN", slug: "spec",
		writes: func(t *testing.T, project, key string) model.Issue {
			issue, _ := createRoomIssue(t, handler, database, project, "A spec holding its tasks", tasksNeedingEachOther("dispatch://"+key+"/spec"), "")
			return issue
		},
	}} {
		t.Run(write.name, func(t *testing.T) {
			createRoomProject(t, handler, write.project)
			// The project's first issue.
			key := write.project + "-1"
			if issue := write.writes(t, write.project, key); issue.Key != key {
				t.Fatalf("the project's first issue is %s, want %s, the key its tasks name", issue.Key, key)
			}
			document := readDecisionDocument(t, database, key, write.slug)
			if document.room == document.id {
				t.Fatalf("%s of %s is a room of its own, want a fragment of the issue's room", write.slug, key)
			}
			first, second := document.address("t-first"), document.address("t-second")
			for _, need := range []struct{ from, to string }{{first, second}, {second, first}} {
				held := currentTies(t, database, need.from, graphstore.TupleNeeds)
				if len(held) != 1 || held[0].ToKind != refs.BlockNodeKind || held[0].ToID != need.to {
					t.Errorf("the stored needs of %s are %+v, want one, to the room's node %s:%s", need.from, held, refs.BlockNodeKind, need.to)
				}
			}

			order, err := refs.PlanOrder(ctx, database.Pool, reader, key)
			if err != nil {
				t.Fatalf("read the order of %s: %v", key, err)
			}
			tasks := []refs.OrderNode{{Kind: refs.BlockNodeKind, ID: first}, {Kind: refs.BlockNodeKind, ID: second}}
			var cycles [][]refs.OrderNode
			for _, cycle := range order.Cycles {
				var nodes []refs.OrderNode
				for _, node := range cycle {
					nodes = append(nodes, node.OrderNode)
				}
				cycles = append(cycles, nodes)
			}
			if !reflect.DeepEqual(cycles, [][]refs.OrderNode{tasks}) {
				t.Errorf("the ordered read of %s answers the cycles %+v, want the one cycle %+v", key, cycles, tasks)
			}
			for position, node := range order.Order {
				if node.OrderNode == tasks[0] || node.OrderNode == tasks[1] {
					t.Errorf("the ordered read of %s gives %s the position %d, want none: it stands in a cycle", key, node.ID, position)
				}
			}
		})
	}
}
