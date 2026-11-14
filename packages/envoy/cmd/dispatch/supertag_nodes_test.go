package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// typeMigrations are the files that write the type nodes, in the order the runner applies them:
// the seed, then each migration that has changed a type since. The test runs them all a second
// time, in that order and through the same path the migration runner runs each, to prove a second
// run leaves the same nodes. The seed alone would not do: run again, it puts back the version and
// the attributes it was written at, which every later type migration has moved on from.
var typeMigrations = []string{
	"../../internal/dispatch/store/migrations/20261109051232_graph_supertags.up.sql",
	"../../internal/dispatch/store/migrations/20261113230000_graph_supertags_check_ci_check.up.sql",
}

// blockSchemaFile is the seed of the typed block types. It is read from disk rather than from
// pmdoc, so what the store holds is compared with the file itself.
const blockSchemaFile = "../../internal/dispatch/pmdoc/schema/blocks.json"

// closedSupertags is the list of types the root spec closes at twenty-nine names
// (dispatch://INFRA-1149/spec, R6). The store holds one supertag node for each, and beyond them
// only an entry an approved document of this tree creates (authorisedSupertags), so this list is
// written here as the test's own copy: a seed that adds or drops a name has to be read by a
// person before this test agrees with it.
var closedSupertags = []string{
	"issue", "document", "document version", "block", "callout", "ask", "requirement",
	"plan_task", "check", "result", "decision", "reading", "criterion", "proposed",
	"constraint", "lens", "risk", "comment", "message", "person", "session", "role", "agent",
	"page", "interaction", "delivery_attempt", "component", "supertag", "attribute",
}

// authorisedSupertags is every supertag the closed list does not name that an approved document
// of this tree creates, each with the reference of the document that creates it. A bare
// prohibition would refuse a correct store: the root's own c-derived requires the thirtieth
// entry below, so a test that allowed the closed list alone would fail the build that
// requirement asks for. What the check is for is a supertag nobody authorised, so an entry
// beyond the closed list passes only where this list names its document, and the run prints
// every one of them with that reference.
//
// The reference is written here rather than read: these checks run where no Dispatch is
// reachable, so a test that called one could not run at all.
var authorisedSupertags = map[string]string{
	"lens_kind": "dispatch://INFRA-1950/spec R7",
}

// TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt proves that a type is data: the seed
// writes the closed list as nodes, a typed block type's attributes come from blocks.json field
// for field, the schema the server serves and validates against is built from those nodes, and
// nothing but a migration writes one. A type that exists only in the file, or only in the
// store, fails here. A pointer is listed by its target's type as that type stands at each read.
func TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	pool := database.Pool
	// The installed schema is process-wide, and this test installs the store's. The hook puts
	// back the schema this binary booted with once the test is over.
	pmdoctest.UseBlockSchema(t, decodeSchema(t, pmdoc.SchemaJSON()))

	file := readBlockSchemaFile(t)
	drift := &schemaDrift{}

	requireClosedSupertagList(t, ctx, pool)
	requireAttributeNodesMatchTheFile(t, ctx, pool, file, drift)
	requireTheStoreBuildsTheFilesSchema(t, ctx, pool, file, drift)
	drift.report(t)
	requireASecondRunOfTheTypeMigrationsLeavesTheSameNodes(t, ctx, pool)
	requireNoRouteOrCommandWritesAType(t)
	requireTheEraseRefusesATypeNode(t, ctx, pool)
	requireTheServedSchemaFollowsTheNodes(t, ctx, pool)
	requireAPointerReadsItsTargetsTypeAtEachRead(t, ctx, pool)
	requireAnUnknownKindIsSkipped(t, ctx, pool, file)
}

// requireClosedSupertagList holds the store to one supertag node per name of the closed list,
// and beyond them only to an entry authorisedSupertags names with its document, each with the
// base attributes every node of this store carries, as node columns and as tuples. Every entry
// beyond the closed list is printed with the document that authorises it, and one with no
// document fails.
func requireClosedSupertagList(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	nodes, err := graphstore.NodesOfKind(ctx, pool, schemanodes.KindSupertag)
	if err != nil {
		t.Fatalf("read the supertag nodes: %v", err)
	}
	var names []string
	for _, node := range nodes {
		names = append(names, node.ID)
		if node.Supertag != schemanodes.KindSupertag {
			t.Errorf("supertag node %q carries supertag %q, want %q", node.ID, node.Supertag, schemanodes.KindSupertag)
		}
		requireBaseAttributes(t, ctx, pool, node)
	}
	want := slices.Clone(closedSupertags)
	slices.Sort(want)
	got := slices.Clone(names)
	slices.Sort(got)
	// Every supertag the closed list does not name, reported with the document that authorises
	// it, and a failure for one nothing authorises.
	for _, name := range missing(got, want) {
		document, authorised := authorisedSupertags[name]
		if !authorised {
			t.Errorf("the store holds supertag node %q, which the closed list does not name and no approved document of this tree authorises", name)
			continue
		}
		t.Logf("the store holds supertag %q beyond the closed list, created by %s", name, document)
		want = append(want, name)
		slices.Sort(want)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the store holds supertag nodes %v, want the closed list and the supertags this tree authorises %v\nonly in the store: %v\nonly in the list: %v",
			got, want, missing(got, want), missing(want, got))
	}
}

// requireBaseAttributes holds one node to carrying visibility and state as columns and as the
// tuples the node row projects.
func requireBaseAttributes(t *testing.T, ctx context.Context, pool *store.Pool, node graphstore.Node) {
	t.Helper()
	if node.Visibility != graphstore.VisibilityProject || node.State != graphstore.StateLive {
		t.Errorf("node %s/%s is %s/%s, want %s/%s",
			node.Kind, node.ID, node.Visibility, node.State, graphstore.VisibilityProject, graphstore.StateLive)
	}
	if node.SourceEvent != graphstore.NoEvent {
		t.Errorf("node %s/%s records event %d, want the sentinel %d: the seed records no event",
			node.Kind, node.ID, node.SourceEvent, graphstore.NoEvent)
	}
	for attribute, want := range map[string]string{"visibility": node.Visibility, "state": node.State} {
		value, held, err := graphstore.CurrentValue(ctx, pool, node.Kind, node.ID, attribute)
		if err != nil {
			t.Fatalf("read %s of node %s/%s: %v", attribute, node.Kind, node.ID, err)
		}
		if !held || value != want {
			t.Errorf("node %s/%s carries %s as the tuple %q (held %t), want %q",
				node.Kind, node.ID, attribute, value, held, want)
		}
	}
}

// requireAttributeNodesMatchTheFile holds every typed block type's attribute nodes to the file,
// field by field and in both directions: an attribute only in the file, or only in the store,
// fails, and so does one whose kind, choices, default, server flag, targets or scope differs.
// The file names an attribute as a document writes it, and its node is the one
// schemanodes.StoreName names, `<type>/<store name>`, so ask's `state` is the node
// ask/answer_state. Each type and attribute it fails on is recorded in drift.
func requireAttributeNodesMatchTheFile(t *testing.T, ctx context.Context, pool *store.Pool, file pmdoc.BlockSchema, drift *schemaDrift) {
	t.Helper()
	nodes, err := graphstore.NodesOfKind(ctx, pool, schemanodes.KindAttribute)
	if err != nil {
		t.Fatalf("read the attribute nodes: %v", err)
	}
	held := map[string]graphstore.Node{}
	for _, node := range nodes {
		held[node.ID] = node
		requireBaseAttributes(t, ctx, pool, node)
		if node.Supertag != schemanodes.KindAttribute {
			t.Errorf("attribute node %q carries supertag %q, want %q", node.ID, node.Supertag, schemanodes.KindAttribute)
		}
	}
	wanted := map[string]struct{}{}
	for _, typ := range file.Types {
		tied := map[string]struct{}{}
		tuples, err := graphstore.CurrentIn(ctx, pool, schemanodes.KindSupertag, typ.Name, []string{schemanodes.TupleAttributeOf})
		if err != nil {
			t.Fatalf("read the attributes tied to %q: %v", typ.Name, err)
		}
		for _, tuple := range tuples {
			tied[tuple.FromID] = struct{}{}
		}
		for position, declared := range typ.Attributes {
			id := typ.Name + "/" + schemanodes.StoreName(typ.Name, declared.Name)
			wanted[id] = struct{}{}
			if _, tiedToType := tied[id]; !tiedToType {
				t.Errorf("no %s tuple ties attribute node %q to supertag %q", schemanodes.TupleAttributeOf, id, typ.Name)
				drift.add(id)
			}
			node, inStore := held[id]
			if !inStore {
				t.Errorf("%s declares attribute %q of %q, which the store holds no node for", blockSchemaFile, declared.Name, typ.Name)
				drift.add(id)
				continue
			}
			requireAttributeNodeCarries(t, node, typ.Name, position, declared, drift)
		}
		if len(tied) != len(typ.Attributes) {
			t.Errorf("supertag %q is tied to %d attribute nodes, want the %d its type declares", typ.Name, len(tied), len(typ.Attributes))
			drift.add(typ.Name)
		}
	}
	for id := range held {
		if _, declared := wanted[id]; !declared {
			t.Errorf("the store holds attribute node %q, which %s declares nowhere", id, blockSchemaFile)
			drift.add(id)
		}
	}
}

// requireAttributeNodeCarries holds one attribute node of type typeName to what the file says
// about that attribute: its name and place, the store name schemanodes.StoreName gives it, and
// each of the six fields a BlockAttributeSchema carries. A node that differs is recorded in drift.
func requireAttributeNodeCarries(t *testing.T, node graphstore.Node, typeName string, position int, declared pmdoc.BlockAttribute, drift *schemaDrift) {
	t.Helper()
	carried := node.Attributes
	if name, _ := carried["name"].(string); name != declared.Name {
		t.Errorf("attribute node %q carries name %q, want %q", node.ID, name, declared.Name)
		drift.add(node.ID)
	}
	if held, want := carried["store_name"], schemanodes.StoreName(typeName, declared.Name); held != want {
		t.Errorf("attribute node %q carries store name %v, want %q", node.ID, held, want)
		drift.add(node.ID)
	}
	if at, ok := carried["position"].(float64); !ok || int(at) != position {
		t.Errorf("attribute node %q carries position %v, want %d", node.ID, carried["position"], position)
		drift.add(node.ID)
	}
	// The six fields of BlockAttributeSchema, compared as the file writes them: the node's
	// attributes are the attribute's own JSON with the name, the store name and the position
	// beside it, so one encoding of each side names every difference at once.
	stored := map[string]any{}
	for key, value := range carried {
		if key == "name" || key == "store_name" || key == "position" {
			continue
		}
		stored[key] = value
	}
	wanted := map[string]any{}
	if err := json.Unmarshal(mustMarshal(t, declared.BlockAttributeSchema), &wanted); err != nil {
		t.Fatalf("read the file's attribute %q: %v", node.ID, err)
	}
	if string(mustMarshal(t, stored)) != string(mustMarshal(t, wanted)) {
		t.Errorf("attribute node %q carries %s, want the file's %s", node.ID, mustMarshal(t, stored), mustMarshal(t, wanted))
		drift.add(node.ID)
	}
}

// requireTheStoreBuildsTheFilesSchema reads the schema back out of the nodes and holds it to the
// file, type for type and attribute for attribute, recording in drift what the two differ on.
func requireTheStoreBuildsTheFilesSchema(t *testing.T, ctx context.Context, pool *store.Pool, file pmdoc.BlockSchema, drift *schemaDrift) {
	t.Helper()
	built, err := schemanodes.Read(ctx, pool)
	if err != nil {
		t.Fatalf("build the schema from the store's nodes: %v", err)
	}
	if string(mustMarshal(t, built)) != string(mustMarshal(t, file)) {
		t.Errorf("the schema the store's nodes build is\n%s\nwant the file's\n%s", mustMarshal(t, built), mustMarshal(t, file))
		drift.compare(built, file)
	}
}

// typeMigrationExample is the type migration a failing run names as the shape of the one a change
// to blocks.json needs: it writes the node of the attribute it adds, with its tuples, and moves
// every block type to the new version.
const typeMigrationExample = "20261113230000_graph_supertags_check_ci_check.up.sql"

// schemaDrift is every type and attribute on which blocks.json and the store's type nodes
// disagree, and the two schema versions where those differ, gathered while the checks above
// report each field. A failing run ends on one instruction naming them all (report), so whoever
// changed the file reads once what the change still needs rather than once for every field.
type schemaDrift struct {
	// names are the types, each by its name, and the attributes, each by its node's id,
	// `<type>/<store name>`.
	names                     []string
	fileVersion, storeVersion int
}

// add records one type or attribute, once.
func (d *schemaDrift) add(name string) {
	if !slices.Contains(d.names, name) {
		d.names = append(d.names, name)
	}
}

// compare records what the schema the store's nodes build and the file's differ on beyond their
// attributes: the version, and each type only one of them declares, or whose content rule,
// renderer or place among the types differs. The attributes are the attribute nodes' own, which
// requireAttributeNodesMatchTheFile has recorded field by field.
func (d *schemaDrift) compare(built, file pmdoc.BlockSchema) {
	if built.Version != file.Version {
		d.fileVersion, d.storeVersion = file.Version, built.Version
	}
	builtOrder, fileOrder := commonTypes(built, file), commonTypes(file, built)
	for index, name := range fileOrder {
		if builtOrder[index] != name {
			d.add(name)
		}
	}
	for _, want := range file.Types {
		index := slices.IndexFunc(built.Types, func(typ pmdoc.BlockTypeSchema) bool { return typ.Name == want.Name })
		if index < 0 || built.Types[index].Content != want.Content || built.Types[index].Render != want.Render {
			d.add(want.Name)
		}
	}
	for _, typ := range built.Types {
		if !slices.ContainsFunc(file.Types, func(want pmdoc.BlockTypeSchema) bool { return want.Name == typ.Name }) {
			d.add(typ.Name)
		}
	}
}

// commonTypes is the names of the types of of that in declares too, in of's order, so the order
// of two schemas is compared over the types both hold and a type only one holds moves no other.
func commonTypes(of, in pmdoc.BlockSchema) []string {
	var names []string
	for _, typ := range of.Types {
		if slices.ContainsFunc(in.Types, func(other pmdoc.BlockTypeSchema) bool { return other.Name == typ.Name }) {
			names = append(names, typ.Name)
		}
	}
	return names
}

// report fails the run once with what a change to blocks.json needs: the types and attributes
// the file and the store's nodes disagree on, the two versions where they differ, and the
// migration that brings the store to the file. It says nothing where the two agree.
func (d *schemaDrift) report(t *testing.T) {
	t.Helper()
	if len(d.names) == 0 && d.fileVersion == d.storeVersion {
		return
	}
	what := "the block schema"
	if len(d.names) > 0 {
		what = strings.Join(slices.Sorted(slices.Values(d.names)), ", ")
	}
	versions := ""
	if d.fileVersion != d.storeVersion {
		versions = fmt.Sprintf(", and the file is at version %d where the store's nodes build version %d", d.fileVersion, d.storeVersion)
	}
	t.Errorf("pmdoc/schema/blocks.json and the store's type nodes disagree on %s%s. "+
		"A change to pmdoc/schema/blocks.json needs a store migration of its own, like %s: "+
		"it writes the node of each type or attribute the change adds or alters, with its tuples, and moves every block type's supertag node to the new version. "+
		"Write one for this change and add it to typeMigrations in this file.",
		what, versions, typeMigrationExample)
}

// requireASecondRunOfTheTypeMigrationsLeavesTheSameNodes runs every type migration again, in
// order, and holds every node row and every tuple to being the one the first run left.
func requireASecondRunOfTheTypeMigrationsLeavesTheSameNodes(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	before, beforeTuples := schemaRows(t, ctx, pool)
	for _, migration := range typeMigrations {
		text, err := os.ReadFile(filepath.Clean(migration))
		if err != nil {
			t.Fatalf("read the type migration %s: %v", filepath.Base(migration), err)
		}
		if _, err := pool.Exec(ctx, string(text)); err != nil {
			t.Fatalf("run the type migration %s a second time: %v", filepath.Base(migration), err)
		}
	}
	after, afterTuples := schemaRows(t, ctx, pool)
	if !slices.Equal(before, after) {
		t.Errorf("a second run of the type migrations left different nodes\nbefore: %v\nafter:  %v", before, after)
	}
	if beforeTuples != afterTuples {
		t.Errorf("a second run of the type migrations left %d tuples, want the %d the first run wrote", afterTuples, beforeTuples)
	}
}

// requireNoRouteOrCommandWritesAType reads the route table and the subcommands and fails on one
// that could create, retitle or delete a type node. Authoring a supertag is offered to nobody:
// a type changes by a migration alone.
func requireNoRouteOrCommandWritesAType(t *testing.T) {
	t.Helper()
	var index bytes.Buffer
	if err := api.WriteRouteIndex(&index); err != nil {
		t.Fatalf("read the route table: %v", err)
	}
	var table struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
			Auth   string `json:"auth"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(index.Bytes(), &table); err != nil {
		t.Fatalf("read the route index: %v", err)
	}
	schemaRoutes := 0
	for _, route := range table.Routes {
		names := namesAType(route.Path)
		if names && route.Method != "GET" {
			t.Errorf("%s %s addresses a type node and writes; no route creates, retitles or deletes one", route.Method, route.Path)
		}
		if route.Path == "/api/v1/schema/blocks" {
			schemaRoutes++
			if route.Method != "GET" || route.Auth != "public" {
				t.Errorf("the block schema route is %s %s (%s), want a public GET", route.Method, route.Path, route.Auth)
			}
		}
	}
	if schemaRoutes != 1 {
		t.Errorf("the route table holds %d rows for /api/v1/schema/blocks, want exactly the one that serves it", schemaRoutes)
	}
	for _, command := range subcommands {
		if namesAType(command.name) {
			t.Errorf("subcommand %q addresses a type node; a type changes by a migration alone", command.name)
		}
	}
}

// namesAType reports whether a route's path or a subcommand's name addresses a type node.
func namesAType(name string) bool {
	for _, word := range []string{"supertag", "schema/blocks", "block-schema", "blockschema", "/types", "attribute-of"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

// requireTheEraseRefusesATypeNode runs `envoy-dispatch erase --node <kind>:<id> --confirm`
// through the command table the binary dispatches on (runSubcommand), for one supertag node and
// one attribute node, and holds each to a refusal that names the node, with the node's row, the
// tuples out of it and into it, and the erasures recorded of it all as they were. The erase is
// the one path in this tree that deletes a node row, and its name holds none of the words
// namesAType reads, so the route and command list above cannot see it: only running it can.
func requireTheEraseRefusesATypeNode(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	databaseURL := pool.Config().ConnString()
	for _, node := range []struct{ kind, id string }{
		{schemanodes.KindSupertag, "callout"},
		{schemanodes.KindAttribute, "callout/kind"},
	} {
		address := node.kind + ":" + node.id
		before := typeNodeHeld(t, ctx, pool, node.kind, node.id)
		if before.row == "" || before.out+before.in == 0 {
			t.Fatalf("the store holds %+v for %s, want its node and its tuples, so that a refusal to erase them proves something", before, address)
		}
		var stdout, stderr bytes.Buffer
		code := runSubcommand(ctx, []string{"erase", "--node", address, "--confirm"},
			envGetter(map[string]string{"DATABASE_URL": databaseURL}), &stdout, &stderr)
		if code == 0 || !strings.Contains(stderr.String(), "refuses "+address) {
			t.Errorf("envoy-dispatch erase --node %s --confirm exited %d with stderr %q and stdout %q, want a refusal naming the type node: no command deletes one",
				address, code, stderr.String(), stdout.String())
		}
		if after := typeNodeHeld(t, ctx, pool, node.kind, node.id); after != before {
			t.Errorf("after the erase of %s the store holds %+v, want the node as it was before: %+v", address, after, before)
		}
	}
}

// typeNodeState is what the store holds of one type node: its row as text, empty where there is
// none, the tuples out of it and into it, and the erasures recorded of it.
type typeNodeState struct {
	row               string
	out, in, erasures int
}

func typeNodeHeld(t *testing.T, ctx context.Context, pool *store.Pool, kind, id string) typeNodeState {
	t.Helper()
	var held typeNodeState
	if err := pool.QueryRow(ctx, `
		select coalesce((select n::text from graph_nodes n where n.kind = $1 and n.id = $2), ''),
		       (select count(*) from graph_tuples where from_kind = $1 and from_id = $2),
		       (select count(*) from graph_tuples where to_kind = $1 and to_id = $2),
		       (select count(*) from graph_erasures where node_kind = $1 and node_id = $2)
	`, kind, id).Scan(&held.row, &held.out, &held.in, &held.erasures); err != nil {
		t.Fatalf("read the type node %s:%s: %v", kind, id, err)
	}
	return held
}

// requireTheServedSchemaFollowsTheNodes changes a type in the database alone - a field of the
// attribute node callout/kind, then a field of the supertag node callout itself - starts the
// server's install again after each (InstallFromStore, the call the server's boot makes), and
// holds both the schema the server serves (pmdoc.SchemaJSON, the bytes GET
// /api/v1/schema/blocks writes) and the refusal a write earns to following the node.
func requireTheServedSchemaFollowsTheNodes(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	const danger = ":::callout{#c1 kind=\"danger\" title=\"T\"}\nOnly.\n:::\n"
	if _, err := pmdoc.ParseForWrite(danger, nil); err == nil {
		t.Fatalf("a callout of kind danger is accepted before the store declares that choice")
	}
	if _, err := pool.Exec(ctx, `
		update graph_nodes
		set attributes = jsonb_set(attributes, '{choices}', '["note", "warning", "danger"]'::jsonb)
		where kind = $1 and id = 'callout/kind'
	`, schemanodes.KindAttribute); err != nil {
		t.Fatalf("add a choice to the callout kind attribute node: %v", err)
	}
	if err := schemanodes.InstallFromStore(ctx, pool); err != nil {
		t.Fatalf("install the schema the changed node builds: %v", err)
	}
	served := decodeSchema(t, pmdoc.SchemaJSON())
	choices := attributeChoices(t, served, "callout", "kind")
	if !slices.Contains(choices, "danger") {
		t.Errorf("the schema the server serves offers callout kinds %v, want the node's own, holding danger", choices)
	}
	if _, err := pmdoc.ParseForWrite(danger, nil); err != nil {
		t.Errorf("a callout of kind danger is refused after the store declares that choice: %v", err)
	}

	// The supertag node's own content rule. The file gives callout block+, so a callout that
	// holds a list is written; the store alone moves it to paragraph+, after which the served
	// schema says paragraph+ and the same callout is refused.
	const listed = ":::callout{#c2 kind=\"note\" title=\"T\"}\n- one\n- two\n:::\n"
	if _, err := pmdoc.ParseForWrite(listed, nil); err != nil {
		t.Fatalf("a callout holding a list is refused before the store changes the callout's content rule: %v", err)
	}
	if content := typeContent(t, decodeSchema(t, pmdoc.SchemaJSON()), "callout"); content != pmdoc.BlockContentBlocks {
		t.Fatalf("the served schema gives callout the content %q before the store changes it, want the file's %q", content, pmdoc.BlockContentBlocks)
	}
	changed, err := pool.Exec(ctx, `
		update graph_nodes
		set attributes = jsonb_set(attributes, '{content}', to_jsonb($2::text))
		where kind = $1 and id = 'callout'
	`, schemanodes.KindSupertag, string(pmdoc.BlockContentParagraphs))
	if err != nil || changed.RowsAffected() != 1 {
		t.Fatalf("change the content rule of the supertag node callout: %d row(s), %v", changed.RowsAffected(), err)
	}
	if err := schemanodes.InstallFromStore(ctx, pool); err != nil {
		t.Fatalf("install the schema the changed supertag node builds: %v", err)
	}
	if content := typeContent(t, decodeSchema(t, pmdoc.SchemaJSON()), "callout"); content != pmdoc.BlockContentParagraphs {
		t.Errorf("the schema the server serves gives callout the content %q, want the supertag node's own %q", content, pmdoc.BlockContentParagraphs)
	}
	if _, err := pmdoc.ParseForWrite(listed, nil); err == nil {
		t.Errorf("a callout holding a list is accepted after the supertag node's content rule became %q", pmdoc.BlockContentParagraphs)
	}
}

// requireAPointerReadsItsTargetsTypeAtEachRead seeds one block pointer as its rows stand: a
// plan_task's serves at a requirement, carrying in to_types the targets the attribute node
// plan_task/serves holds, as the index copies them. It reads the link through the three readers
// that list or count it - the edges in at its target (graph_edges' block_refs arm), the edges out
// of its document (refs.Edges' own block_refs arm) and the target's backlink count - and then
// retypes the target in document_blocks alone and reads again, with no document indexed again and
// the pointer's rows as they were. block_ref_listed reads the target's type at each read, so the
// link is hidden while the target is a callout and listed once it is a requirement again.
func requireAPointerReadsItsTargetsTypeAtEachRead(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	var targets []string
	if err := pool.QueryRow(ctx, `
		select coalesce(array(select jsonb_array_elements_text(attributes->'targets')), '{}')
		from graph_nodes where kind = $1 and id = 'plan_task/serves'
	`, schemanodes.KindAttribute).Scan(&targets); err != nil {
		t.Fatalf("read the targets of the attribute node plan_task/serves: %v", err)
	}
	if !slices.Contains(targets, "requirement") || slices.Contains(targets, "callout") {
		t.Fatalf("plan_task/serves points at %v, want requirement and not callout, so that the retype below moves the link", targets)
	}
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('STP', 'Supertag pointers', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('STP-1', 'STP', 1, 'Pointers', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed the pointer's project and issue: %v", err)
	}
	var document, refKey string
	if err := pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('STP-1', 'STP', 'pointers', 'pointers.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`).Scan(&document, &refKey); err != nil {
		t.Fatalf("seed the pointer's document: %v", err)
	}
	task, requirement := refKey+"#t1", refKey+"#r1"
	if _, err := pool.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, 'r1', $2, 'requirement', md5('r1'), 'The requirement the task serves.', 1, now()),
		       ($1::uuid, 't1', $3, 'plan_task', md5('t1'), 'The task that serves it.', 1, now())
	`, document, requirement, task); err != nil {
		t.Fatalf("seed the pointer's two blocks: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, to_types)
		values ($1::uuid, 't1', $2, 'block', $3, 'serves', 1, $4::text[])
	`, document, task, requirement, targets); err != nil {
		t.Fatalf("seed the pointer row: %v", err)
	}
	rows := pointerRows(t, ctx, pool, document)

	requirePointerListed(t, ctx, pool, "the target a requirement", document, task, requirement, true)
	retypeBlock(t, ctx, pool, requirement, "callout")
	requirePointerListed(t, ctx, pool, "the target retyped to a callout", document, task, requirement, false)
	retypeBlock(t, ctx, pool, requirement, "requirement")
	requirePointerListed(t, ctx, pool, "the target a requirement again", document, task, requirement, true)
	if after := pointerRows(t, ctx, pool, document); !slices.Equal(after, rows) {
		t.Errorf("the retype rewrote the pointer's rows, which it must not touch:\nbefore %v\nafter  %v", rows, after)
	}
}

// requirePointerListed holds the three readers of a pointer to listing it, or to hiding it: the
// serves edge in at the requirement, the serves edge out of the document, and the requirement's
// backlink count.
func requirePointerListed(t *testing.T, ctx context.Context, pool *store.Pool, stage, document, task, requirement string, listed bool) {
	t.Helper()
	in, err := refs.Edges(ctx, pool, model.Actor{}, refs.Query{Direction: "in", Kind: "block", ID: requirement})
	if err != nil {
		t.Fatalf("%s: read the edges in at %s: %v", stage, requirement, err)
	}
	out, err := refs.Edges(ctx, pool, model.Actor{}, refs.Query{Direction: "out", Kind: "artifact", ID: document})
	if err != nil {
		t.Fatalf("%s: read the edges out of %s: %v", stage, document, err)
	}
	counts, err := refs.BacklinkCounts(ctx, pool, model.Actor{}, "block", []string{requirement}, nil)
	if err != nil {
		t.Fatalf("%s: count the backlinks of %s: %v", stage, requirement, err)
	}
	want := 0
	if listed {
		want = 1
	}
	if got := servesEdges(in, task); got != want {
		t.Errorf("%s: the edges in at %s list %d serves edge(s) from %s, want %d (%+v)", stage, requirement, got, task, want, in)
	}
	if got := servesEdges(out, requirement); got != want {
		t.Errorf("%s: the edges out of the document list %d serves edge(s) at %s, want %d (%+v)", stage, got, requirement, want, out)
	}
	if got := counts[requirement]; got != want {
		t.Errorf("%s: %s counts %d backlink(s), want %d", stage, requirement, got, want)
	}
}

// servesEdges counts the serves edges of a read whose far end is node.
func servesEdges(edges []model.GraphEdge, node string) int {
	count := 0
	for _, edge := range edges {
		if edge.Kind == "serves" && edge.Node.ID == node {
			count++
		}
	}
	return count
}

// retypeBlock changes the type of one current block row and nothing else, which is how the
// pointer's target changes type here: no version is written and no document is indexed.
func retypeBlock(t *testing.T, ctx context.Context, pool *store.Pool, node, typ string) {
	t.Helper()
	changed, err := pool.Exec(ctx, `update document_blocks set type = $2 where node_id = $1`, node, typ)
	if err != nil || changed.RowsAffected() != 1 {
		t.Fatalf("retype %s to %s: %d row(s), %v", node, typ, changed.RowsAffected(), err)
	}
}

// pointerRows is every block_refs row of document and its citing block's row, each as text.
func pointerRows(t *testing.T, ctx context.Context, pool *store.Pool, document string) []string {
	t.Helper()
	read, err := pool.Query(ctx, `
		select r::text from block_refs r where r.artifact_id = $1::uuid
		union all
		select d::text from document_blocks d where d.artifact_id = $1::uuid and d.block_id = 't1'
		order by 1
	`, document)
	if err != nil {
		t.Fatalf("read the pointer's rows: %v", err)
	}
	defer read.Close()
	var rows []string
	for read.Next() {
		var row string
		if err := read.Scan(&row); err != nil {
			t.Fatalf("read a pointer row: %v", err)
		}
		rows = append(rows, row)
	}
	if err := read.Err(); err != nil {
		t.Fatalf("read the pointer's rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("the document holds %d pointer and citing row(s), want its one pointer and its one plan_task", len(rows))
	}
	return rows
}

// requireAnUnknownKindIsSkipped adds a supertag the block schema never declared and a node of a
// kind this build cannot address, and holds the reader to skipping both and to taking a new
// block type with no Go file changed.
func requireAnUnknownKindIsSkipped(t *testing.T, ctx context.Context, pool *store.Pool, file pmdoc.BlockSchema) {
	t.Helper()
	writer := `{"kind": "system", "id": "supertag-test"}`
	if _, err := pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values
			('supertag', 'lantern', 'supertag', 'project', 'live', '{}'::jsonb, now(), $1::jsonb, 0),
			('lantern', 'one', 'lantern', 'project', 'live', '{}'::jsonb, now(), $1::jsonb, 0)
	`, writer); err != nil {
		t.Fatalf("add a supertag and a node of a kind this build cannot address: %v", err)
	}
	built, err := schemanodes.Read(ctx, pool)
	if err != nil {
		t.Fatalf("read the schema beside a kind this build cannot address: %v", err)
	}
	for _, typ := range built.Types {
		if typ.Name == "lantern" {
			t.Errorf("the reader took the supertag %q, which declares no block type", typ.Name)
		}
	}
	if len(built.Types) != len(file.Types) {
		t.Errorf("the schema beside the unknown kinds holds %d types, want the %d the file declares", len(built.Types), len(file.Types))
	}

	// A block type added as a row alone, with no Go file changed, is served.
	if _, err := pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values
			('supertag', 'claim', 'supertag', 'project', 'live',
				jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host', 'version', $2::int, 'position', 99),
				now(), $1::jsonb, 0),
			('attribute', 'claim/supports', 'attribute', 'project', 'live',
				jsonb_build_object('name', 'supports', 'store_name', 'supports', 'position', 0, 'kind', 'block[]', 'targets', jsonb_build_array('requirement')),
				now(), $1::jsonb, 0)
	`, writer, file.Version); err != nil {
		t.Fatalf("add a block type as a row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
		values ('attribute', 'claim/supports', 'attribute_of', 'supertag', 'claim', now(), $1::jsonb, 0, null, true)
	`, writer); err != nil {
		t.Fatalf("tie the new type's attribute to it: %v", err)
	}
	if err := schemanodes.InstallFromStore(ctx, pool); err != nil {
		t.Fatalf("install the schema the new block type builds: %v", err)
	}
	if !pmdoc.IsTypedBlock("claim") {
		t.Errorf("the type the store gained is no typed block of the installed schema")
	}
	if !slices.Contains(pmdoc.LinkKinds(), "supports") {
		t.Errorf("the link kinds are %v, want the new type's supports among them", pmdoc.LinkKinds())
	}
}

// schemaRows is every supertag and attribute node as its row stands, and how many tuples those
// nodes hold: what a second seed must leave untouched.
func schemaRows(t *testing.T, ctx context.Context, pool *store.Pool) ([]string, int) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		select kind || ' ' || id || ' ' || supertag || ' ' || visibility || ' ' || state || ' ' ||
			attributes::text || ' ' || made_at::text || ' ' || made_by::text || ' ' || source_event::text
		from graph_nodes
		where kind in ('supertag', 'attribute')
		order by kind, id
	`)
	if err != nil {
		t.Fatalf("read the type nodes: %v", err)
	}
	defer rows.Close()
	var written []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("read a type node: %v", err)
		}
		written = append(written, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the type nodes: %v", err)
	}
	var tuples int
	if err := pool.QueryRow(ctx, `
		select count(*) from graph_tuples where from_kind in ('supertag', 'attribute')
	`).Scan(&tuples); err != nil {
		t.Fatalf("count the type nodes' tuples: %v", err)
	}
	return written, tuples
}

func readBlockSchemaFile(t *testing.T) pmdoc.BlockSchema {
	t.Helper()
	body, err := os.ReadFile(filepath.Clean(blockSchemaFile))
	if err != nil {
		t.Fatalf("read %s: %v", blockSchemaFile, err)
	}
	return decodeSchema(t, body)
}

func decodeSchema(t *testing.T, body []byte) pmdoc.BlockSchema {
	t.Helper()
	var value pmdoc.BlockSchema
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode a block schema: %v", err)
	}
	return value
}

func attributeChoices(t *testing.T, value pmdoc.BlockSchema, typeName, attribute string) []string {
	t.Helper()
	for _, typ := range value.Types {
		if typ.Name != typeName {
			continue
		}
		declared, held := typ.Attributes.Lookup(attribute)
		if !held {
			t.Fatalf("type %q declares no attribute %q", typeName, attribute)
		}
		return declared.Choices
	}
	t.Fatalf("the schema declares no type %q", typeName)
	return nil
}

func typeContent(t *testing.T, value pmdoc.BlockSchema, typeName string) pmdoc.BlockContentRule {
	t.Helper()
	for _, typ := range value.Types {
		if typ.Name == typeName {
			return typ.Content
		}
	}
	t.Fatalf("the schema declares no type %q", typeName)
	return ""
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %T: %v", value, err)
	}
	return encoded
}

// missing is what the left list holds and the right does not, as a difference a reader can act
// on.
func missing(left, right []string) []string {
	var only []string
	for _, name := range left {
		if !slices.Contains(right, name) {
			only = append(only, name)
		}
	}
	if only == nil {
		return []string{}
	}
	return only
}
