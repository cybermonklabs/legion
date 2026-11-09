package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// seedMigration is the file that writes the type nodes. The test runs it a second time, through
// the same path the migration runner runs it, to prove a second seed leaves the same nodes.
const seedMigration = "../../internal/dispatch/store/migrations/20261109051232_graph_supertags.up.sql"

// blockSchemaFile is the seed of the typed block types. It is read from disk rather than from
// pmdoc, so what the store holds is compared with the file itself.
const blockSchemaFile = "../../internal/dispatch/pmdoc/schema/blocks.json"

// closedSupertags is the list of types the root spec closes at twenty-nine names
// (dispatch://INFRA-1149/spec, R6). The store holds one supertag node for each and no other, so
// this list is written here as the test's own copy: a seed that adds or drops a name has to be
// read by a person before this test agrees with it.
var closedSupertags = []string{
	"issue", "document", "document version", "block", "callout", "ask", "requirement",
	"plan_task", "check", "result", "decision", "reading", "criterion", "proposed",
	"constraint", "lens", "risk", "comment", "message", "person", "session", "role", "agent",
	"page", "interaction", "delivery_attempt", "component", "supertag", "attribute",
}

// TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt proves that a type is data: the seed
// writes the closed list as nodes, a typed block type's attributes come from blocks.json field
// for field, the schema the server serves and validates against is built from those nodes, and
// nothing but a migration writes one. A type that exists only in the file, or only in the
// store, fails here.
func TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	pool := database.Pool
	// The installed schema is process-wide, and this test installs the store's. The hook puts
	// back the schema this binary booted with once the test is over.
	pmdoctest.UseBlockSchema(t, decodeSchema(t, pmdoc.SchemaJSON()))

	file := readBlockSchemaFile(t)

	requireClosedSupertagList(t, ctx, pool)
	requireAttributeNodesMatchTheFile(t, ctx, pool, file)
	requireTheStoreBuildsTheFilesSchema(t, ctx, pool, file)
	requireASecondSeedLeavesTheSameNodes(t, ctx, pool)
	requireNoRouteOrCommandWritesAType(t)
	requireTheServedSchemaFollowsTheNodes(t, ctx, pool)
	requireAnUnknownKindIsSkipped(t, ctx, pool, file)
}

// requireClosedSupertagList holds the store to one supertag node per name of the closed list and
// no other, each with the base attributes every node of this store carries, as node columns and
// as tuples.
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
	if !slices.Equal(got, want) {
		t.Errorf("the store holds supertag nodes %v, want the closed list %v\nonly in the store: %v\nonly in the list: %v",
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
func requireAttributeNodesMatchTheFile(t *testing.T, ctx context.Context, pool *store.Pool, file pmdoc.BlockSchema) {
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
			id := typ.Name + "/" + declared.Name
			wanted[id] = struct{}{}
			if _, tiedToType := tied[id]; !tiedToType {
				t.Errorf("no %s tuple ties attribute node %q to supertag %q", schemanodes.TupleAttributeOf, id, typ.Name)
			}
			node, inStore := held[id]
			if !inStore {
				t.Errorf("%s declares attribute %q of %q, which the store holds no node for", blockSchemaFile, declared.Name, typ.Name)
				continue
			}
			requireAttributeNodeCarries(t, node, position, declared)
		}
		if len(tied) != len(typ.Attributes) {
			t.Errorf("supertag %q is tied to %d attribute nodes, want the %d its type declares", typ.Name, len(tied), len(typ.Attributes))
		}
	}
	for id := range held {
		if _, declared := wanted[id]; !declared {
			t.Errorf("the store holds attribute node %q, which %s declares nowhere", id, blockSchemaFile)
		}
	}
}

// requireAttributeNodeCarries holds one attribute node to what the file says about that
// attribute: its name and place, and each of the six fields a BlockAttributeSchema carries.
func requireAttributeNodeCarries(t *testing.T, node graphstore.Node, position int, declared pmdoc.BlockAttribute) {
	t.Helper()
	carried := node.Attributes
	if name, _ := carried["name"].(string); name != declared.Name {
		t.Errorf("attribute node %q carries name %q, want %q", node.ID, name, declared.Name)
	}
	if at, ok := carried["position"].(float64); !ok || int(at) != position {
		t.Errorf("attribute node %q carries position %v, want %d", node.ID, carried["position"], position)
	}
	// The six fields of BlockAttributeSchema, compared as the file writes them: the node's
	// attributes are the attribute's own JSON with the name and the position beside it, so one
	// encoding of each side names every difference at once.
	stored := map[string]any{}
	for key, value := range carried {
		if key == "name" || key == "position" {
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
	}
}

// requireTheStoreBuildsTheFilesSchema reads the schema back out of the nodes and holds it to the
// file, type for type and attribute for attribute.
func requireTheStoreBuildsTheFilesSchema(t *testing.T, ctx context.Context, pool *store.Pool, file pmdoc.BlockSchema) {
	t.Helper()
	built, err := schemanodes.Read(ctx, pool)
	if err != nil {
		t.Fatalf("build the schema from the store's nodes: %v", err)
	}
	if string(mustMarshal(t, built)) != string(mustMarshal(t, file)) {
		t.Errorf("the schema the store's nodes build is\n%s\nwant the file's\n%s", mustMarshal(t, built), mustMarshal(t, file))
	}
}

// requireASecondSeedLeavesTheSameNodes runs the seed migration again and holds every node row
// and every tuple to being the one the first seed left.
func requireASecondSeedLeavesTheSameNodes(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	before, beforeTuples := schemaRows(t, ctx, pool)
	seed, err := os.ReadFile(filepath.Clean(seedMigration))
	if err != nil {
		t.Fatalf("read the seed migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(seed)); err != nil {
		t.Fatalf("run the seed migration a second time: %v", err)
	}
	after, afterTuples := schemaRows(t, ctx, pool)
	if !slices.Equal(before, after) {
		t.Errorf("a second seed left different nodes\nbefore: %v\nafter:  %v", before, after)
	}
	if beforeTuples != afterTuples {
		t.Errorf("a second seed left %d tuples, want the %d the first seed wrote", afterTuples, beforeTuples)
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

// requireTheServedSchemaFollowsTheNodes changes a type in the database alone, starts the server's
// install again, and holds both the schema the server serves and the refusal a write earns to
// following the node.
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
				jsonb_build_object('name', 'supports', 'position', 0, 'kind', 'block[]', 'targets', jsonb_build_array('requirement')),
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
