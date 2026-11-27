package refs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// newProjectDocument creates one document that a project holds and no issue does, in a project
// of its own. No issue holds its decisions to one, so a decision of it may tie to the blocks and
// the asks of any issue (foreignTies).
func newProjectDocument(t *testing.T, database *store.Store, name, slug string) parentDocument {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the project document fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	project := parentProject(name)
	createReferenceProject(t, tx, project)
	id, refKey := createReferenceArtifact(t, tx, "", project, slug)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the project document fixture: %v", err)
	}
	return parentDocument{database: database, artifact: id, refKey: refKey, topKind: graphstore.KindArtifact, topID: refKey}
}

// indexWhole records version number of the document, rendered as version and written whole by
// writer, and indexes it as a version write does: the typed-node pass's facts read against the
// version's own row (versionFacts), with every block writer's (writerSources), and one block index
// pass (IndexBlocks) that writes every block's rows and the typed ties among them, a decision's
// decided_by included, committed under the version's event.
func (d parentDocument) indexWhole(t *testing.T, number int, version typedVersion, writer model.Actor) {
	t.Helper()
	ctx := context.Background()
	record := d.writeVersion(t, number, version.markdown, writer)
	commitIndex(t, d.database.Pool, func(tx pgx.Tx) (graphstore.Uncaused, error) {
		facts, err := versionFacts(ctx, tx, d.artifact, number, record.at, version, writerSources(tx, d.artifact, writer))
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		_, won, write, _, err := IndexBlocks(ctx, tx, d.artifact, OneVersion(d.artifact, number, record.at), version.markdown, version.blocks, version.located, version.pointers, "", facts)
		if err == nil && !won {
			err = fmt.Errorf("version %d of %s lost the revision fence", number, d.refKey)
		}
		return write, err
	}, record.event)
}

// blockRefLines is every block_refs row of the document as "<block> <kind> -> <to_kind> <to_id>",
// sorted.
func blockRefLines(t *testing.T, d parentDocument) []string {
	t.Helper()
	rows, err := d.database.Pool.Query(context.Background(), `
		select block_id || ' ' || kind || ' -> ' || to_kind || ' ' || to_id from block_refs where artifact_id = $1::uuid
	`, d.artifact)
	if err != nil {
		t.Fatalf("read the block_refs rows of %s: %v", d.refKey, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the block_refs rows of %s: %v", d.refKey, err)
	}
	slices.Sort(lines)
	return lines
}

// referenceLines is each row of one list of the references read as "<node kind> <node id>
// [<kinds>] via <via kind> <via id>", sorted.
func referenceLines(rows []model.ReferenceLink) []string {
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, fmt.Sprintf("%s %s [%s] via %s %s", row.Node.Kind, row.Node.ID, strings.Join(row.Kinds, ","), row.Via.Kind, row.Via.ID))
	}
	slices.Sort(lines)
	return lines
}

// The references read lists a link only where it is a mention, or a typed block's `block[]`
// pointer of a kind the installed block schema declares while the graph read lists it
// (blockRefLinked). block_refs holds two more kinds of a decision's ties, which graph_edges and
// the graph read list and the references read does not: decided_by, which the block index states
// to the person who wrote the decision, and from_ask, to the ask the decision settles.
//
// The decision issue's document holds a decision whose uncertainty_reason cites the target issue,
// so the decision's own block mentions it, and whose decided_by the block index states to alice,
// who wrote it. The issue's links hold the target issue, by the mention alone, and not alice's
// person node, which the graph read of the document still lists by its decided_by. A decision's
// one pointer kind, shaped, reaches no further than its own issue (foreignTies), whose content
// the read never lists, so the pointer beside a mention is read on the in side: the target issue
// holds a requirement and an ask, and a document a project holds - no issue, so its decision may
// tie to any issue - holds a decision whose shaped points at that requirement, whose from_ask
// names that ask, and whose uncertainty_reason cites the target issue. The target issue's
// linked_from holds that document by its mention and its shaped, and not by its from_ask, and the
// decision issue's document by its mention. A decided_by row never reaches the in statement: its
// far end is a person or a session, never an issue's own content, so from_ask is the kind that
// holds the in statement to the rule.
func TestTheReferencesReadListsOnlyMentionsAndDeclaredPointers(t *testing.T) {
	database := parentDatabase(t)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	const ask = "5e0c7d1a-2b3c-4d5e-8f90-a1b2c3d4e5f6"

	target := newParentDocument(t, database, "TGT", "spec-md")
	target.indexWhole(t, 1, renderTypedVersion(t, ":::requirement{#r1}\nThe requirement.\n:::\n"), alice)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the ask fixture: %v", err)
	}
	insertItemWithID(t, tx, ItemAsk, ask, target.issue)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the ask fixture: %v", err)
	}

	cites := `uncertainty="low" uncertainty_reason="dispatch://` + target.issue + ` may reopen it"`
	decisions := newParentDocument(t, database, "DEC", "choices-md")
	decisions.indexWhole(t, 1, renderTypedVersion(t, ":::decision{#d1 "+cites+"}\nWe chose REST.\n:::\n"), alice)
	requirement := "dispatch://" + target.issue + "/artifact/spec-md#b-r1"
	notes := newProjectDocument(t, database, "NOTE", "notes-md")
	notes.indexWhole(t, 1, renderTypedVersion(t, ":::decision{#p1 shaped=\""+requirement+"\" from_ask=\"dispatch://"+
		target.issue+"/ask/"+ask+"\" "+cites+"}\nWe chose REST too.\n:::\n"), alice)

	// The rows the read must leave out stand beside the ones it lists: each decision's decided_by
	// to alice, and the project document's from_ask beside its shaped and its mention.
	requirementRef, _ := text.ParseRef(requirement, "")
	requirementTarget, _ := TargetOf(requirementRef)
	for _, document := range []struct {
		name string
		doc  parentDocument
		want []string
	}{
		{"the decision issue's document", decisions, []string{
			"d1 decided_by -> person alice",
			"d1 mentions -> issue " + target.issue,
		}},
		{"the project's document", notes, []string{
			"p1 decided_by -> person alice",
			"p1 from_ask -> ask " + ask,
			"p1 mentions -> issue " + target.issue,
			"p1 shaped -> block " + requirementTarget.ID,
		}},
	} {
		if got := blockRefLines(t, document.doc); !slices.Equal(got, document.want) {
			t.Fatalf("%s holds the block_refs rows\n\t%v\nwant\n\t%v", document.name, got, document.want)
		}
	}

	who := model.Actor{}
	decided, err := Closure(ctx, database.Pool, who, decisions.issue, "")
	if err != nil {
		t.Fatalf("read the references of %s: %v", decisions.issue, err)
	}
	reached, err := Closure(ctx, database.Pool, who, target.issue, "")
	if err != nil {
		t.Fatalf("read the references of %s: %v", target.issue, err)
	}
	pointers := pmdoc.LinkKinds()
	lists := []struct {
		name string
		got  []model.ReferenceLink
		want []string
	}{
		{decisions.issue + "'s links", decided.Links, []string{
			"issue " + target.issue + " [mentions] via artifact " + decisions.artifact,
		}},
		{decisions.issue + "'s linked_from", decided.LinkedFrom, []string{}},
		{target.issue + "'s links", reached.Links, []string{}},
		{target.issue + "'s linked_from", reached.LinkedFrom, []string{
			"artifact " + decisions.artifact + " [mentions] via issue " + target.issue,
			"artifact " + notes.artifact + " [mentions,shaped] via artifact " + target.refKey,
		}},
	}
	for _, list := range lists {
		for _, row := range list.got {
			for _, kind := range row.Kinds {
				if kind != "mentions" && !slices.Contains(pointers, kind) {
					t.Errorf("%s lists %s %s by %s (kinds %v): a %s link is neither a mention nor a pointer kind the schema declares (%v)",
						list.name, row.Node.Kind, row.Node.ID, kind, row.Kinds, kind, pointers)
				}
			}
		}
		slices.Sort(list.want)
		if got := referenceLines(list.got); !slices.Equal(got, list.want) {
			t.Errorf("%s are\n\t%v\nwant\n\t%v", list.name, got, list.want)
		}
	}

	// The graph read still lists the decided_by the references read leaves out: blockRefListed
	// is graph_edges' rule, which answers a decision's decided_by (the census reads it there).
	edges, err := Edges(ctx, database.Pool, who, Query{Direction: "out", Kind: "artifact", ID: decisions.artifact})
	if err != nil {
		t.Fatalf("read the graph out of %s: %v", decisions.refKey, err)
	}
	if !slices.ContainsFunc(edges, func(edge model.GraphEdge) bool {
		return edge.Kind == graphstore.TupleDecidedBy && edge.Node.Kind == graphstore.KindPerson && edge.Node.ID == "alice"
	}) {
		var listed []string
		for _, edge := range edges {
			listed = append(listed, edge.Kind+" -> "+edge.Node.Kind+" "+edge.Node.ID)
		}
		t.Errorf("the graph read out of %s lists %v, and not d1's decided_by to person alice, which graph_edges answers", decisions.refKey, listed)
	}
}
