package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphseedtest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The blocks the plan of TestEveryDeclaredPlanAttributeSurvivesTheConversionAndThePlanView holds
// beside one block of each other type the plan query takes: the first task, which writes every
// attribute plan_task declares that points at nothing; the second, which needs the first; the
// part, a task that serves nothing and whose parent is the first; and the spec's requirement both
// tasks serve.
const (
	planFirst       = "t-first"
	planSecond      = "t-second"
	planPart        = "t-part"
	planRequirement = "r1"
)

// planRoleType is the role block of a task's owner, a type a later schema declares and the plan
// query already takes (graphstore.QueryTakes). No migration of the store holds its type yet, and
// the store's writers take a node only of a type the store holds (graphstore.ErrSupertagNotHeld):
// in a deployment the type migration that declares it writes its supertag node, which the
// installed schema is read from (pmdoc/schemanodes), so the test seeds that node beside the schema
// it swaps in.
const planRoleType = "plan_role"

// planAttributesSchema is the block schema in force with what later pieces declare on the plan:
// a `steps` attribute of kind string on plan_task, where the schema declares none yet, and the
// typed block plan_role.
func planAttributesSchema(t *testing.T) pmdoc.BlockSchema {
	t.Helper()
	var schema pmdoc.BlockSchema
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &schema); err != nil {
		t.Fatalf("decode the block schema in force: %v", err)
	}
	task := slices.IndexFunc(schema.Types, func(typ pmdoc.BlockTypeSchema) bool { return typ.Name == planTaskBlockType })
	if task < 0 {
		t.Fatalf("the block schema in force declares no %s", planTaskBlockType)
	}
	if !schema.Types[task].Attributes.Has("steps") {
		schema.Types[task].Attributes = append(slices.Clone(schema.Types[task].Attributes), pmdoc.BlockAttribute{
			Name: "steps", BlockAttributeSchema: pmdoc.BlockAttributeSchema{Kind: pmdoc.BlockAttributeString},
		})
	}
	if !slices.ContainsFunc(schema.Types, func(typ pmdoc.BlockTypeSchema) bool { return typ.Name == planRoleType }) {
		schema.Types = append(schema.Types, pmdoc.BlockTypeSchema{
			Name: planRoleType, Content: pmdoc.BlockContentParagraphs, Render: "host", Attributes: pmdoc.BlockAttributes{},
		})
	}
	return schema
}

// planAttributesPlan is the plan the test writes in an issue: its markdown; the type of each of
// its blocks, by block id; of each block, the value of each attribute that points at nothing its
// opening line writes, as its node holds it - the text the line writes, and a duration in whole
// minutes; and of each block, each pointer it writes as the tuple it is, by the tuple's kind, to
// the block id of its far end.
type planAttributesPlan struct {
	markdown string
	types    map[string]string
	values   map[string]map[string]string
	pointers map[string]map[string]string
}

// planValueNames is the name of each attribute plan_task declares in schema that its opening line
// writes and that points at nothing: a value of a plan node.
func planValueNames(schema pmdoc.BlockSchema) []string {
	var names []string
	for _, attribute := range planTaskAttributes(schema) {
		if attribute.Kind != pmdoc.BlockAttributeBlockList && attribute.Kind != pmdoc.BlockAttributeAsk && attribute.InMarkdown() {
			names = append(names, attribute.Name)
		}
	}
	return names
}

// planTaskAttributes is the attributes plan_task declares in schema, in declaration order.
func planTaskAttributes(schema pmdoc.BlockSchema) pmdoc.BlockAttributes {
	for _, typ := range schema.Types {
		if typ.Name == planTaskBlockType {
			return typ.Attributes
		}
	}
	return nil
}

// planAttributeWritten is the text the test writes for attribute, an attribute of plan_task that
// points at nothing, in the plan's first version or, revised, its second, and the value its node
// holds of it: the text itself, but for a duration, which the node holds in whole minutes.
func planAttributeWritten(t *testing.T, attribute pmdoc.BlockAttribute, revised bool) (text, value string) {
	t.Helper()
	switch attribute.Kind {
	case pmdoc.BlockAttributeString:
		if attribute.Name == "steps" && revised {
			return "implementer tester reviewer", "implementer tester reviewer"
		}
		if attribute.Name == "steps" {
			return "implementer reviewer", "implementer reviewer"
		}
		words := "the " + attribute.Name + " of the first task"
		return words, words
	case pmdoc.BlockAttributeMinutes:
		return "1h30m", "90"
	case pmdoc.BlockAttributeStringList:
		items := []string{attribute.Name + "-one", attribute.Name + "-two"}
		encoded, err := json.Marshal(items)
		if err != nil {
			t.Fatalf("encode the %s of the first task: %v", attribute.Name, err)
		}
		return writtenList(t, items), string(encoded)
	case pmdoc.BlockAttributeEnum:
		return attribute.Choices[0], attribute.Choices[0]
	case pmdoc.BlockAttributeBool:
		return "true", "true"
	case pmdoc.BlockAttributeTimestamp:
		return "2026-11-25T10:00:00Z", "2026-11-25T10:00:00Z"
	case pmdoc.BlockAttributeActor:
		return "alice", "alice"
	}
	t.Fatalf("plan_task declares the attribute %s of the kind %s, which this test writes no value of", attribute.Name, attribute.Kind)
	return "", ""
}

// planAttributesWritten is the plan of the issue key under schema, its first version or, revised,
// its second, in which the first task writes another steps and the part writes none. The first
// task writes every attribute of plan_task that points at nothing, and the part its steps; a
// pointer whose targets take a requirement is written by both tasks, naming the spec's
// requirement; the parent by the part, naming the first task, which makes its part_of; and every
// other pointer by the second task, naming the first. One block of each other type the plan query
// takes stands after the three. Each opening line writes its attributes in the order plan_task
// declares them, as the renderer writes them, so the plan's text is the text the version stores.
func planAttributesWritten(t *testing.T, schema pmdoc.BlockSchema, key string, revised bool) planAttributesPlan {
	t.Helper()
	plan := planAttributesPlan{types: map[string]string{}, values: map[string]map[string]string{}, pointers: map[string]map[string]string{}}
	lines := map[string][]string{}
	for _, block := range []string{planFirst, planSecond, planPart} {
		plan.types[block] = planTaskBlockType
		plan.values[block] = map[string]string{}
		plan.pointers[block] = map[string]string{}
	}
	write := func(block, name, text string) {
		lines[block] = append(lines[block], name+`="`+text+`"`)
	}
	point := func(block, name, far, kind, farBlock string) {
		write(block, name, far)
		plan.pointers[block][kind] = farBlock
	}
	planBlock := func(block string) string { return "dispatch://" + key + "/artifact/plan-md#b-" + block }
	requirement := "dispatch://" + key + "/spec#b-" + planRequirement
	for _, attribute := range planTaskAttributes(schema) {
		if !attribute.InMarkdown() || attribute.Server {
			continue
		}
		switch {
		case attribute.Kind == pmdoc.BlockAttributeBlockList && attribute.Name == pmdoc.ParentAttribute:
			point(planPart, attribute.Name, planBlock(planFirst), graphstore.TuplePartOf, planFirst)
		case attribute.Kind == pmdoc.BlockAttributeBlockList && slices.Contains(attribute.Targets, "requirement"):
			point(planFirst, attribute.Name, requirement, attribute.Name, planRequirement)
			point(planSecond, attribute.Name, requirement, attribute.Name, planRequirement)
		case attribute.Kind == pmdoc.BlockAttributeBlockList:
			point(planSecond, attribute.Name, planBlock(planFirst), attribute.Name, planFirst)
		case attribute.Kind == pmdoc.BlockAttributeAsk:
			t.Fatalf("plan_task declares the ask attribute %s, which this test writes no value of", attribute.Name)
		default:
			text, value := planAttributeWritten(t, attribute, revised)
			write(planFirst, attribute.Name, text)
			plan.values[planFirst][attribute.Name] = value
			if attribute.Name == "steps" && !revised {
				write(planPart, attribute.Name, "tester")
				plan.values[planPart][attribute.Name] = "tester"
			}
		}
	}
	opening := func(typ, block string) string {
		return ":::" + typ + "{" + strings.Join(append([]string{"#" + block}, lines[block]...), " ") + "}\n"
	}
	blocks := []string{
		opening(planTaskBlockType, planFirst) + "The first task, which writes every attribute.\n:::\n",
		opening(planTaskBlockType, planSecond) + "The second task, which needs the first.\n:::\n",
		opening(planTaskBlockType, planPart) + "The part of the first task.\n:::\n",
	}
	for _, typ := range graphstore.QueryTakes(graphstore.QueryPlan) {
		if typ == planTaskBlockType {
			continue
		}
		if !slices.ContainsFunc(schema.Types, func(declared pmdoc.BlockTypeSchema) bool { return declared.Name == typ }) {
			t.Fatalf("the plan query takes the type %s, which the schema of this test declares no block of", typ)
		}
		block := strings.ReplaceAll(typ, "_", "-") + "-1"
		plan.types[block] = typ
		blocks = append(blocks, opening(typ, block)+"The block of the "+typ+" type.\n:::\n")
	}
	plan.markdown = strings.Join(blocks, "\n")
	return plan
}

// blockTextsByID is the text of each block GET …/blocks lists of the document artifactID, by its
// id: what the node of the block answers as its text.
func blockTextsByID(t *testing.T, handler http.Handler, artifactID string) map[string]string {
	t.Helper()
	texts := map[string]string{}
	for text, id := range blockIDsByText(t, handler, artifactID) {
		texts[id] = text
	}
	return texts
}

// planQueryVersion is the newest version of the plan query node of the issue key: the version of
// the artifact row whose ref_key is a node of kind artifact and supertag query whose query is
// plan, which `?view=plan&v=` reads.
func planQueryVersion(t *testing.T, database *store.Store, key string) int {
	t.Helper()
	var number int
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce((select max(v.number) from artifact_versions v where v.artifact_id = a.id), 0)
		from artifacts a
		join graph_nodes n on n.kind = $2 and n.id = a.ref_key and n.supertag = $3 and n.attributes->>$4 = $5
		where a.issue_key = $1`,
		key, graphstore.KindArtifact, graphstore.SupertagQuery, graphstore.AttributeQuery, graphstore.QueryPlan).Scan(&number); err != nil {
		t.Fatalf("read the plan query node of %s: %v", key, err)
	}
	if number == 0 {
		t.Fatalf("the plan query node of %s holds no version", key)
	}
	return number
}

// holdUnconverted is holdIssuesUnconverted, so an issue created while its process stands is
// created unconverted, answering the function that takes that process away again, so no live
// process lacks the issue room, which the conversion's gate (docs.IssueRoomGate) requires.
func holdUnconverted(t *testing.T, database *store.Store) func() {
	t.Helper()
	instance := holdIssuesUnconverted(t, database)
	return func() {
		t.Helper()
		if _, err := database.Pool.Exec(context.Background(), `delete from server_processes where instance = $1::uuid`, instance); err != nil {
			t.Fatalf("take away the server process %s that lists no issue room: %v", instance, err)
		}
	}
}

// requirePlanView fails unless read, the plan view of the issue key (at, "" for the head, or " at
// version <n>"), answers each block of plan, whose document is document and whose requirement
// stands in spec: once, with its text byte for byte as texts holds it; each value names, the
// attributes of a plan node, in its `attributes` member as plan holds it, and none plan does not
// hold; each pointer as its tuple, where its far end is a node of the answer; and the part under
// the node of the first task.
func requirePlanView(t *testing.T, read treeAnswer, key, at string, document, spec decisionDocument, plan planAttributesPlan, texts map[string]string, names []string) {
	t.Helper()
	address := func(block string) string {
		if block == planRequirement {
			return spec.address(block)
		}
		return document.address(block)
	}
	for _, block := range slices.Sorted(maps.Keys(plan.types)) {
		typ := plan.types[block]
		nodes := read.nodesNamed(refs.BlockNodeKind, address(block))
		if len(nodes) != 1 {
			t.Errorf("the plan view of %s%s answers no node for the %s block %s (%d of %s)", key, at, typ, block, len(nodes), address(block))
			continue
		}
		node := nodes[0]
		if want := texts[block]; node.Text != want {
			t.Errorf("the plan view of %s%s answers %s with text %q, want %q", key, at, block, node.Text, want)
		}
		if typ == planTaskBlockType {
			for _, name := range names {
				want, writes := plan.values[block][name]
				got, holds := node.Attributes[name]
				switch {
				case writes && !holds:
					t.Errorf("the plan view of %s%s answers %s with no value %s, want %s", key, at, block, name, want)
				case writes && got != want, !writes && holds:
					t.Errorf("the plan view of %s%s answers %s with %s %q, want %q", key, at, block, name, got, want)
				}
			}
		}
		for _, kind := range slices.Sorted(maps.Keys(plan.pointers[block])) {
			far := address(plan.pointers[block][kind])
			if len(read.nodesNamed(refs.BlockNodeKind, far)) == 0 {
				continue
			}
			tuple := model.IssueTreeTuple{
				Kind: kind, From: model.IssueTreeEnd{Kind: refs.BlockNodeKind, ID: address(block)}, To: model.IssueTreeEnd{Kind: refs.BlockNodeKind, ID: far},
			}
			if !slices.Contains(read.Tuples, tuple) {
				t.Errorf("the plan view of %s%s answers no %s tuple from %s to %s, which its opening line writes", key, at, kind, block, plan.pointers[block][kind])
			}
		}
	}
	if parts := read.nodesNamed(refs.BlockNodeKind, address(planPart)); len(parts) == 1 {
		under := "no node"
		if parts[0].Parent != nil {
			under = parts[0].Parent.ID
		}
		if under != address(planFirst) {
			t.Errorf("the plan view of %s%s answers the part %s under %s, want it under %s", key, at, planPart, under, planFirst)
		}
	}
}

// TestEveryDeclaredPlanAttributeSurvivesTheConversionAndThePlanView is c-plan-attributes (R18 and
// R8 of the issue tree's spec). Under the block schema in force with a `steps` attribute of kind
// string on plan_task and the typed block plan_role, it writes one plan holding a block carrying
// each attribute plan_task declares and a block of each type the plan query takes
// (planAttributesWritten), read off the schema and graphstore.QueryTakes, so an attribute or a
// type a later build adds enters the test with no change to it. It writes that plan in two issues:
// the first born converted, the second created unconverted, holding the plan as a document, and
// then converted by docs.ConvertIssue. In the first it writes a second version, in which the first
// task writes another steps and the part writes none.
//
// In each issue `GET /api/v1/issues/{key}/tree?view=plan` after its last write - the second
// version in the first issue, the conversion in the second - and `?view=plan&v=<n>` at the
// version that wrote the first plan answer each node with its text byte for byte, each value of a
// plan node in its `attributes` member with the text its opening line writes, `steps` among them,
// and `size` in whole minutes, each pointer as its tuple, the plan_role block among the view's
// nodes, and the part under the node of the first task (requirePlanView). The head of the first
// issue answers the new steps on the first task and no steps on the part.
func TestEveryDeclaredPlanAttributeSurvivesTheConversionAndThePlanView(t *testing.T) {
	ctx := context.Background()
	schema := planAttributesSchema(t)
	pmdoctest.UseBlockSchema(t, schema)
	names := planValueNames(schema)
	if !slices.Contains(names, "steps") || !slices.Contains(names, refs.AttributePlanSize) {
		t.Fatalf("plan_task declares the values %v, want steps and size among them", names)
	}
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphseedtest.SeedSupertags(ctx, tx, planRoleType)
	}); err != nil {
		t.Fatalf("seed the type %s the swapped-in schema declares: %v", planRoleType, err)
	}
	service := deps.Docs.(*docs.Service)
	const spec = ":::requirement{#" + planRequirement + "}\nEvery task says how it is carried out.\n:::\n"

	t.Run("an issue born converted", func(t *testing.T) {
		createRoomProject(t, handler, "BORN")
		issue, _ := createRoomIssue(t, handler, database, "BORN", "A plan born converted", spec, "")
		key := issue.Key
		first := planAttributesWritten(t, schema, key, false)
		uploadDocument(t, handler, "/api/v1/issues/"+key, "plan.md", first.markdown)
		document, specDocument := readDecisionDocument(t, database, key, "plan-md"), readDecisionDocument(t, database, key, "spec")
		firstTexts := blockTextsByID(t, handler, document.id)
		firstVersion := planQueryVersion(t, database, key)

		revised := planAttributesWritten(t, schema, key, true)
		uploadDocument(t, handler, "/api/v1/issues/"+key, "plan.md", revised.markdown)
		head := treeRead(t, handler, key, "?view=plan", "alice", false)
		requirePlanView(t, head, key, "", document, specDocument, revised, blockTextsByID(t, handler, document.id), names)
		at := fmt.Sprintf("?view=plan&v=%d", firstVersion)
		requirePlanView(t, treeRead(t, handler, key, at, "alice", false), key, fmt.Sprintf(" at version %d", firstVersion),
			document, specDocument, first, firstTexts, names)
	})

	t.Run("an issue the conversion converts", func(t *testing.T) {
		createRoomProject(t, handler, "MOVED")
		release := holdUnconverted(t, database)
		issue, _ := createRoomIssue(t, handler, database, "MOVED", "A plan the conversion moves", spec, "")
		key := issue.Key
		plan := planAttributesWritten(t, schema, key, false)
		uploadDocument(t, handler, "/api/v1/issues/"+key, "plan.md", plan.markdown)
		if document := readDecisionDocument(t, database, key, "plan-md"); document.room != document.id {
			t.Fatalf("the plan of %s is a fragment of the room %s, want a room of its own before the conversion: this case proves nothing", key, document.room)
		}
		release()
		// The conversion defers an issue one of whose documents owes its block index, so a tick
		// runs through whatever the writes before it left owed first.
		if _, err := service.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
			t.Fatalf("run the block-index tick over what the writes of %s left owed: %v", key, err)
		}
		converted, err := service.ConvertIssue(ctx, key, docs.IssueRoomGate, false)
		if err != nil || converted.Outcome != docs.IssueConverted {
			t.Fatalf("the conversion of %s answered %+v, %v; want it converted", key, converted, err)
		}
		document, specDocument := readDecisionDocument(t, database, key, "plan-md"), readDecisionDocument(t, database, key, "spec")
		if document.room == document.id {
			t.Fatalf("the plan of %s is still a room of its own after the conversion", key)
		}
		texts := blockTextsByID(t, handler, document.id)
		requirePlanView(t, treeRead(t, handler, key, "?view=plan", "alice", false), key, "", document, specDocument, plan, texts, names)
		version := planQueryVersion(t, database, key)
		requirePlanView(t, treeRead(t, handler, key, fmt.Sprintf("?view=plan&v=%d", version), "alice", false), key, fmt.Sprintf(" at version %d", version),
			document, specDocument, plan, texts, names)
	})
}
