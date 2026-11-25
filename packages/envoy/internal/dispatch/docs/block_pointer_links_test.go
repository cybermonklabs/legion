package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func claimPointing(id, serves, body string) string {
	return ":::claim{#" + id + " serves=\"" + serves + "\"}\n" + body + "\n:::\n"
}

// seedPointerAttribute writes the supertag node of typeName, the attribute node of its attrName
// pointer and the attribute_of tuple that ties them, as migration 20261109051232 seeds the
// shipped schema's nodes into graph_nodes. The tuple arm of graph_edges reads a pointer's
// allowed targets from this attribute node (block_pointer_targets), not from the citing row, so
// a test over a type the shipped schema does not hold - claim - seeds its node here or the arm
// finds no targets. The node of a typed block carries its type's supertag (R26 of the issue-tree
// spec), and the store refuses a node of a type it holds no supertag node of
// (graphstore.ErrSupertagNotHeld), so every test that writes a claim seeds its type here first, as
// a deployment whose schema holds the type has it installed (pmdoc/schemanodes). targets is the
// types the pointer may name, null for no limit.
func seedPointerAttribute(t *testing.T, database *store.Store, typeName, attrName string, targets []string) {
	t.Helper()
	updatePointerTargets(t, database, typeName, attrName, targets)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values ('supertag', $1, 'supertag', 'project', 'live', '{}'::jsonb, now(), '{"kind":"system","id":"block-schema-seed"}', 0)
		on conflict (kind, id) do nothing`, typeName); err != nil {
		t.Fatalf("seed the %s supertag node: %v", typeName, err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
		select 'attribute', $1, 'attribute_of', 'supertag', $2, now(), '{"kind":"system","id":"block-schema-seed"}', 0, null, true
		where not exists (
			select 1 from graph_tuples held
			where held.from_kind = 'attribute' and held.from_id = $1 and held.kind = 'attribute_of'
			  and held.to_kind = 'supertag' and held.to_id = $2 and held.source_event = 0 and held.asserted)`,
		typeName+"/"+attrName, typeName); err != nil {
		t.Fatalf("tie the %s/%s attribute to its supertag: %v", typeName, attrName, err)
	}
}

// updatePointerTargets writes the attribute node of typeName's attrName pointer with targets,
// which is what a schema change and its notify-driven reinstall do in a deployment
// (pmdoc/schemanodes). It is a write of one node, no pass over any document: the tuple arm
// reads the new targets at once, which is how a change to an attribute's targets reaches every
// link of that attribute with no reindex of the documents that cite it.
func updatePointerTargets(t *testing.T, database *store.Store, typeName, attrName string, targets []string) {
	t.Helper()
	attributes := map[string]any{"kind": "block[]", "name": attrName, "store_name": attrName, "position": 0}
	if targets != nil {
		attributes["targets"] = targets
	}
	encoded, err := json.Marshal(attributes)
	if err != nil {
		t.Fatalf("encode the %s/%s attribute: %v", typeName, attrName, err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values ('attribute', $1, 'attribute', 'project', 'live', $2, now(), '{"kind":"system","id":"block-schema-seed"}', 0)
		on conflict (kind, id) do update set attributes = excluded.attributes`,
		typeName+"/"+attrName, encoded); err != nil {
		t.Fatalf("seed the %s/%s attribute node: %v", typeName, attrName, err)
	}
}

// writeIndexedDocument creates document slug of CORE-1 holding markdown as its version 1, written
// and indexed in one transaction as an upload of a new document is, and returns its uuid.
func writeIndexedDocument(t *testing.T, database *store.Store, service *Service, slug, markdown string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`, slug).Scan(&id); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	if err := joinedWrite(service, func(ctx context.Context) error {
		written, err := service.SeedText(ctx, id, markdown, model.Actor{Kind: "user", ID: "alice"})
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
		`, id, written); err != nil {
			return err
		}
		_, write, err := service.IndexDocumentReferences(ctx, id, 1, written)
		if err != nil {
			return err
		}
		// The caller of IndexDocumentReferences appends the pass's tuples under the event that
		// caused the write, as the version-write and reconciliation paths do (block_index.go's
		// Caused). This helper writes no event, so the tuples carry the store's sentinel, which
		// is what a version no event recorded leaves. Without this the rows and their nodes are
		// written but the tuples are not, and the graph_edges tuple arm, which answers every row
		// that names its node, would answer nothing for this document.
		return causeInWritePhase(ctx, tx, write, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("write document %s: %v", slug, err)
	}
	return id
}

// editIndexedDocument applies ops to the document and writes the version they leave, as an agent's
// edit is.
func editIndexedDocument(t *testing.T, service *Service, artifactID string, ops ...model.EditOp) {
	t.Helper()
	if err := joinedWrite(service, func(ctx context.Context) error {
		if _, err := service.ApplyOps(ctx, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
			return err
		}
		result, err := service.SnapshotVersion(ctx, artifactID, model.Actor{Kind: "user", ID: "alice"})
		if err == nil && !result.Wrote {
			t.Errorf("the edit of %s wrote no version", artifactID)
		}
		return err
	}); err != nil {
		t.Fatalf("edit %s: %v", artifactID, err)
	}
}

// requireLinksTo fails unless the graph lists exactly want ("<kind> <source node id>") pointing at
// the block node nodeID.
func requireLinksTo(t *testing.T, database *store.Store, stage, nodeID string, want ...string) {
	t.Helper()
	edges, err := refs.Edges(context.Background(), database.Pool, model.Actor{}, refs.Query{Direction: "in", Kind: "block", ID: nodeID})
	if err != nil {
		t.Fatalf("%s: read the links to %s: %v", stage, nodeID, err)
	}
	got := make([]string, len(edges))
	for index, edge := range edges {
		got[index] = edge.Kind + " " + edge.Node.ID
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) || len(got) > 0 && !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: links to %s = %q, want %q", stage, nodeID, got, want)
	}
}

// pointerRow is one block_refs row, every column a test compares.
type pointerRow struct {
	BlockID, Kind, ToKind, ToID string
	ToTypes                     []string
	CreatedVersion              int
	CreatedAt                   time.Time
	SourceSeq                   *int64
}

func pointerRowsOf(t *testing.T, database *store.Store, artifactID string) []pointerRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, kind, to_kind, to_id, to_types, created_version, created_at, source_seq
		from block_refs where artifact_id = $1 order by block_id, kind, to_kind, to_id
	`, artifactID)
	if err != nil {
		t.Fatalf("read block_refs: %v", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pointerRow, error) {
		var pointer pointerRow
		err := row.Scan(&pointer.BlockID, &pointer.Kind, &pointer.ToKind, &pointer.ToID, &pointer.ToTypes, &pointer.CreatedVersion, &pointer.CreatedAt, &pointer.SourceSeq)
		return pointer, err
	})
	if err != nil {
		t.Fatalf("scan block_refs: %v", err)
	}
	return found
}

// A change of an attribute's targets reaches the pointers a document already stores at once, with
// no pass over the documents that cite it: the tuple arm of graph_edges reads a pointer's allowed
// targets from the attribute node (block_pointer_targets), so a write of that node - a schema
// change and its reinstall - lists the edges the new targets allow without re-indexing any
// citing document.
//
// The attribute is read as pmdoc/schemanodes.Read installs it: through the current attribute_of
// tuple alone and from a live attribute node. An attribute_of the store has since retracted, and
// an attribute node it holds gone, each stop answering, so the link they limited is listed with no
// limit, as the installed schema, which declares no such attribute, lists it.
func TestATargetsChangeReachesStoredPointersAtOnce(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	database, service := blockIndexService(t)
	seedPointerAttribute(t, database, "claim", "serves", []string{"requirement"})
	target := writeIndexedDocument(t, database, service, "target", ":::requirement{#r1}\nA requirement.\n:::\n")
	writeIndexedDocument(t, database, service, "checks", claimPointing("c1", "dispatch://CORE-1/artifact/target#b-r1", "It holds."))
	requireLinksTo(t, database, "pointing at a requirement", "CORE-1/target#r1", "serves CORE-1/checks#c1")

	editIndexedDocument(t, service, target, model.EditOp{Op: "retype", Block: "r1", Type: "callout"})
	requireLinksTo(t, database, "r1 retyped to a callout", "CORE-1/target#r1")

	// The attribute now allows callouts. The tuple arm reads this off the attribute node, so the
	// edge at r1 - a callout - is listed again at once, with no re-index of checks.md.
	updatePointerTargets(t, database, "claim", "serves", []string{"requirement", "callout"})
	requireLinksTo(t, database, "serves now allowing callouts, checks.md not re-indexed", "CORE-1/target#r1", "serves CORE-1/checks#c1")

	updatePointerTargets(t, database, "claim", "serves", []string{"requirement"})
	requirePointerTargets(t, database, "serves back at requirements alone", "CORE-1/checks#c1", "serves", []string{"requirement"})
	requireLinksTo(t, database, "serves back at requirements alone", "CORE-1/target#r1")

	stateAttributeOf(t, database, "claim", "serves", false)
	requirePointerTargets(t, database, "the attribute_of retracted", "CORE-1/checks#c1", "serves", nil)
	requireLinksTo(t, database, "the attribute_of retracted", "CORE-1/target#r1", "serves CORE-1/checks#c1")

	stateAttributeOf(t, database, "claim", "serves", true)
	requirePointerTargets(t, database, "the attribute_of asserted again", "CORE-1/checks#c1", "serves", []string{"requirement"})
	requireLinksTo(t, database, "the attribute_of asserted again", "CORE-1/target#r1")

	if _, err := database.Pool.Exec(context.Background(),
		`update graph_nodes set state = 'gone' where kind = 'attribute' and id = 'claim/serves'`); err != nil {
		t.Fatalf("end the claim/serves attribute: %v", err)
	}
	requirePointerTargets(t, database, "the attribute gone", "CORE-1/checks#c1", "serves", nil)
	requireLinksTo(t, database, "the attribute gone", "CORE-1/target#r1", "serves CORE-1/checks#c1")
}

// stateAttributeOf appends an attribute_of tuple from typeName's attrName attribute to its
// supertag, asserting or retracting it, as a schema change that adds or removes the attribute from
// the type does.
func stateAttributeOf(t *testing.T, database *store.Store, typeName, attrName string, asserted bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the attribute_of write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
		FromKind: "attribute", FromID: typeName + "/" + attrName, Kind: "attribute_of", ToKind: "supertag", ToID: typeName,
		At: time.Now(), Who: model.Actor{Kind: "system", ID: "block-schema-seed"}, SourceEvent: graphstore.NoEvent, Asserted: asserted,
	}}); err != nil {
		t.Fatalf("state the attribute_of of %s/%s %t: %v", typeName, attrName, asserted, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the attribute_of write: %v", err)
	}
}

// requirePointerTargets fails unless block_pointer_targets answers want for the pointer attribute
// of the block at nodeID: the targets graph_edges' tuple arm holds that block's links to, nil for
// no limit.
func requirePointerTargets(t *testing.T, database *store.Store, stage, nodeID, attribute string, want []string) {
	t.Helper()
	var targets []string
	if err := database.Pool.QueryRow(context.Background(),
		`select block_pointer_targets($1, $2)`, nodeID, attribute).Scan(&targets); err != nil {
		t.Fatalf("%s: read the targets of %s's %s: %v", stage, nodeID, attribute, err)
	}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("%s: block_pointer_targets(%s, %s) = %#v, want %#v", stage, nodeID, attribute, targets, want)
	}
}

// A browser's write is never refused: a settled browser tree holding a pointer to a block of a type
// the attribute does not allow writes its version and stores the pointer, which the graph does not
// list until its target has an allowed type.
func TestABrowserWrittenPointerToADisallowedTypeIsStoredAndNotListed(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	database, service := blockIndexService(t)
	seedPointerAttribute(t, database, "claim", "serves", []string{"requirement"})
	target := writeIndexedDocument(t, database, service, "target", ":::callout{#k1 kind=\"note\" title=\"\"}\nA callout.\n:::\n")
	checks := writeIndexedDocument(t, database, service, "checks", "First.\n")

	browserReplaceText(t, service, checks, claimPointing("c1", "dispatch://CORE-1/artifact/target#b-k1", "It holds."), model.Actor{Kind: "user", ID: "bob"})
	waitForLandedAppends(t, service, checks)
	state := service.room(checks)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	service.settleRoom(checks, generation)

	var versions int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, checks).Scan(&versions); err != nil {
		t.Fatalf("count the versions of checks.md: %v", err)
	}
	state.mu.Lock()
	failed, failures := state.failed, state.settleFailures
	state.mu.Unlock()
	if versions != 2 || failed != nil || failures != 0 {
		t.Fatalf("settlement wrote %d versions (failed %v, %d failures), want version 2 and no failure", versions, failed, failures)
	}
	var stored []pointerRow
	for _, row := range pointerRowsOf(t, database, checks) {
		stored = append(stored, pointerRow{BlockID: row.BlockID, Kind: row.Kind, ToKind: row.ToKind, ToID: row.ToID, ToTypes: row.ToTypes, CreatedVersion: row.CreatedVersion})
	}
	if want := []pointerRow{{BlockID: "c1", Kind: "serves", ToKind: "block", ToID: "CORE-1/target#k1", ToTypes: []string{"requirement"}, CreatedVersion: 2}}; !reflect.DeepEqual(stored, want) {
		t.Fatalf("block_refs of checks.md = %#v, want %#v", stored, want)
	}
	requireLinksTo(t, database, "pointing at a callout", "CORE-1/target#k1")

	editIndexedDocument(t, service, target, model.EditOp{Op: "retype", Block: "k1", Type: "requirement"})
	requireLinksTo(t, database, "k1 retyped to a requirement", "CORE-1/target#k1", "serves CORE-1/checks#c1")
}

// The reconciliation indexes a document's pointers as its version write did: re-indexing the same
// version leaves every block_refs row as it was, the pointer's targets included.
func TestTheReconciliationIndexesTheSamePointersAsTheVersionWrite(t *testing.T) {
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
	database, service := blockIndexService(t)
	seedPointerAttribute(t, database, "claim", "serves", []string{"requirement"})
	writeIndexedDocument(t, database, service, "target", ":::requirement{#r1}\nA requirement.\n:::\n")
	const r1 = "dispatch://CORE-1/artifact/target#b-r1"
	checks := writeIndexedDocument(t, database, service, "checks", claimPointing("c1", r1, "It holds, per "+r1+".")+"\nSee dispatch://CORE-2.\n")
	paragraph := holderOf(t, service, checks, "It holds, per")
	other := holderOf(t, service, checks, "See dispatch://CORE-2")

	written := pointerRowsOf(t, database, checks)
	kinds := make([]string, len(written))
	for index, row := range written {
		toTypes := "null"
		if row.ToTypes != nil {
			toTypes = fmt.Sprint(row.ToTypes)
		}
		kinds[index] = row.Kind + " " + row.BlockID + " " + row.ToID + " " + toTypes
	}
	want := []string{
		"serves c1 CORE-1/target#r1 [requirement]",
		"mentions " + paragraph + " CORE-1/target#r1 null",
		"mentions " + other + " CORE-2 null",
	}
	sort.Strings(kinds)
	sort.Strings(want)
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("block_refs the version write stored = %q, want %q", kinds, want)
	}

	if _, err := database.Pool.Exec(context.Background(), `update document_block_versions set version = null where artifact_id = $1`, checks); err != nil {
		t.Fatalf("mark checks.md owed: %v", err)
	}
	settle(t, database, checks)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("block index tick = %#v, want checks.md indexed", tick)
	}
	if at := blockIndexAt(t, database, checks); at == nil || *at != 1 {
		t.Fatalf("checks.md indexed at %v, want version 1", at)
	}
	if again := pointerRowsOf(t, database, checks); !reflect.DeepEqual(again, written) {
		t.Fatalf("block_refs after the reconciliation = %#v, want %#v", again, written)
	}
}

// A rendered version can repeat a block id (a browser write can, before settlement repairs it),
// and the index's node for that id is its first holder with an offset. Pointers are written only
// from that node, and only when it is the typed block holding them. A claim repeating the id of a
// paragraph before it writes no edge (the paragraph's own mention of the same target stays), nor
// does a claim repeating the id of a claim before it (whose own pointers stay). A list item has an
// offset too, because a move can nest a block under one, so a list item that holds an id first is
// that id's node. A claim after it that repeats the id writes no pointer. The list item has no
// words of its own, so it holds no row. The version write, which indexes the tree, and the
// reconciliation, which reads the version's markdown back (no paragraph's or list item's id is
// written there), store the same rows. Both paths take the id's node from a rendering of the same
// tree, and that tree holds the list item's id. Only the pointers are read back from the markdown,
// and the claim's opening line writes its id.
func TestARepeatedBlockIDWritesPointersOnlyFromItsNode(t *testing.T) {
	const (
		r1 = "dispatch://CORE-1/artifact/target#b-r1"
		r2 = "dispatch://CORE-1/artifact/target#b-r2"
	)
	for _, test := range []struct {
		name     string
		markdown string
		// repeat gives the later block the earlier one's id, and returns that id.
		repeat func(t *testing.T, service *Service, checks string, tree *pmdoc.Node) string
		// want is the stored rows, each "<kind> <block id> <to id>", given the repeated id; nil for none.
		want func(id string) []string
	}{
		{
			name:     "a paragraph, then a claim",
			markdown: "Per " + r1 + ".\n\n" + claimPointing("c1", r1, "It holds."),
			repeat: func(t *testing.T, service *Service, checks string, tree *pmdoc.Node) string {
				paragraph := holderOf(t, service, checks, "Per ")
				tree.Children[1].Attrs[pmdoc.BlockIDAttr] = paragraph
				return paragraph
			},
			want: func(id string) []string { return []string{"mentions " + id + " CORE-1/target#r1"} },
		},
		{
			name:     "a claim, then a claim",
			markdown: claimPointing("c1", r1, "It holds.") + "\n" + claimPointing("c2", r2, "So does this."),
			repeat: func(t *testing.T, service *Service, checks string, tree *pmdoc.Node) string {
				tree.Children[1].Attrs[pmdoc.BlockIDAttr] = "c1"
				return "c1"
			},
			want: func(id string) []string { return []string{"serves " + id + " CORE-1/target#r1"} },
		},
		{
			name:     "a list item, then a claim",
			markdown: "- An item.\n\n" + claimPointing("c1", r1, "It holds."),
			repeat: func(t *testing.T, service *Service, checks string, tree *pmdoc.Node) string {
				// The list item's id is not written in markdown, so the claim's value changes too,
				// which the version must write.
				tree.Children[0].Children[0].Attrs[pmdoc.BlockIDAttr] = "c1"
				tree.Children[1].Attrs["serves"] = []any{r2}
				return "c1"
			},
			// The list item is the id's node, so the claim writes no pointer.
			want: func(string) []string { return nil },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
			database, service := blockIndexService(t)
			seedPointerAttribute(t, database, "claim", "serves", []string{"requirement"})
			writeIndexedDocument(t, database, service, "target", ":::requirement{#r1}\nOne.\n:::\n\n:::requirement{#r2}\nTwo.\n:::\n")
			checks := writeIndexedDocument(t, database, service, "checks", test.markdown)
			var id string
			editLiveTree(t, service, checks, func(tree *pmdoc.Node) *pmdoc.Node {
				id = test.repeat(t, service, checks, tree)
				return tree
			})
			if err := joinedWrite(service, func(ctx context.Context) error {
				result, err := service.SnapshotVersion(ctx, checks, model.Actor{Kind: "user", ID: "bob"})
				if err == nil && !result.Wrote {
					t.Errorf("the repeated id wrote no version of checks.md")
				}
				return err
			}); err != nil {
				t.Fatalf("write the version repeating %s: %v", id, err)
			}
			var markdown string
			if err := database.Pool.QueryRow(context.Background(), `select markdown from artifact_versions where artifact_id = $1 and number = 2`, checks).Scan(&markdown); err != nil {
				t.Fatalf("read version 2 of checks.md: %v", err)
			}
			if !strings.Contains(markdown, ":::claim{#"+id+" ") {
				t.Fatalf("version 2 of checks.md = %q, want a claim repeating the id %s", markdown, id)
			}
			requireRows := func(stage string) {
				t.Helper()
				var stored []string
				for _, row := range pointerRowsOf(t, database, checks) {
					stored = append(stored, row.Kind+" "+row.BlockID+" "+row.ToID)
				}
				if want := test.want(id); !reflect.DeepEqual(stored, want) {
					t.Fatalf("%s: block_refs of checks.md = %q, want %q", stage, stored, want)
				}
			}
			requireRows("the version write")

			if _, err := database.Pool.Exec(context.Background(), `update document_block_versions set version = null where artifact_id = $1`, checks); err != nil {
				t.Fatalf("mark checks.md owed: %v", err)
			}
			settle(t, database, checks)
			if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
				t.Fatalf("block index tick = %#v, want checks.md indexed", tick)
			}
			requireRows("the reconciliation")
		})
	}
}

// A typed block's id is written in its opening line as it reads back (pmdoc.TypedBlockIDAttribute),
// whatever id a browser's update gave it, so the version write, which indexes the tree, and the
// reconciliation, which reads the version's markdown back, store the same rows: a claim whose id
// holds a space keeps its pointer on both. An id holding U+0000 is written U+FFFD, as markdown reads
// that character, and both paths list the claim's pointer under the id as written.
func TestATypedBlockIDReadsBackFromTheMarkdownAsTheTreeHoldsIt(t *testing.T) {
	const r1 = "dispatch://CORE-1/artifact/target#b-r1"
	for _, test := range []struct{ name, id, written string }{
		{name: "an id holding a space", id: "a b", written: "a b"},
		{name: "an id holding U+0000", id: "c\x00d", written: "c\uFFFDd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(nil))
			database, service := blockIndexService(t)
			seedPointerAttribute(t, database, "claim", "serves", []string{"requirement"})
			writeIndexedDocument(t, database, service, "target", ":::requirement{#r1}\nOne.\n:::\n")
			checks := writeIndexedDocument(t, database, service, "checks", claimPointing("c1", r1, "It holds."))
			editLiveTree(t, service, checks, func(tree *pmdoc.Node) *pmdoc.Node {
				tree.Children[0].Attrs[pmdoc.BlockIDAttr] = test.id
				return tree
			})
			if err := joinedWrite(service, func(ctx context.Context) error {
				result, err := service.SnapshotVersion(ctx, checks, model.Actor{Kind: "user", ID: "bob"})
				if err == nil && !result.Wrote {
					t.Errorf("the claim's new id wrote no version of checks.md")
				}
				return err
			}); err != nil {
				t.Fatalf("write the version giving the claim the id %q: %v", test.id, err)
			}
			written := pointerRowsOf(t, database, checks)
			var stored []string
			for _, row := range written {
				stored = append(stored, row.Kind+" "+row.BlockID+" "+row.ToID)
			}
			if want := []string{"serves " + test.written + " CORE-1/target#r1"}; !reflect.DeepEqual(stored, want) {
				t.Fatalf("the version write: block_refs of checks.md = %q, want %q", stored, want)
			}

			if _, err := database.Pool.Exec(context.Background(), `update document_block_versions set version = null where artifact_id = $1`, checks); err != nil {
				t.Fatalf("mark checks.md owed: %v", err)
			}
			settle(t, database, checks)
			if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
				t.Fatalf("block index tick = %#v, want checks.md indexed", tick)
			}
			if again := pointerRowsOf(t, database, checks); !reflect.DeepEqual(again, written) {
				t.Fatalf("block_refs after the reconciliation = %#v, want the version write's %#v", again, written)
			}

			var markdown string
			if err := database.Pool.QueryRow(context.Background(), `select markdown from artifact_versions where artifact_id = $1 and number = 2`, checks).Scan(&markdown); err != nil {
				t.Fatalf("read version 2 of checks.md: %v", err)
			}
			read, err := pmdoc.ParseRendering(markdown)
			if err != nil {
				t.Fatalf("version 2 of checks.md = %q, which does not read back: %v", markdown, err)
			}
			if id := read.Children[0].Attrs[pmdoc.BlockIDAttr]; id != test.written {
				t.Fatalf("version 2 of checks.md = %q, whose claim reads back with id %q, want %q", markdown, id, test.written)
			}
		})
	}
}
