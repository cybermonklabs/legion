package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// This file is c-write-protocol of dispatch://INFRA-1098 (R29, the root's
// dispatch://INFRA-1098/spec#b-r-write-protocol): one fence orders the store's writes by a
// document's version in document_block_versions, a reader records graph_tuples.seq, and nothing
// else numbers a state. It drives the fence through the block index writer it holds for, the
// point facts it does not hold for, two moves that race, an import, and a commit held open.

// placementVersion is the text of version number of the fixture document. Up to version 7 the two
// callouts stand side by side, mover's words naming the version; from version 8 on mover stands
// under holder, as the person's edit of version 8 moves it.
func placementVersion(number int) string {
	mover := fmt.Sprintf("The block a person's edit moves, as version %d holds it.", number)
	if number < 8 {
		return ":::callout{#holder kind=\"note\" title=\"\"}\nThe block the edit moves another under.\n:::\n\n" +
			":::callout{#mover kind=\"note\" title=\"\"}\n" + mover + "\n:::\n"
	}
	return "::::callout{#holder kind=\"note\" title=\"\"}\nThe block the edit moves another under.\n\n" +
		":::callout{#mover kind=\"note\" title=\"\"}\n" + mover + "\n:::\n\n::::\n"
}

// writeProtocolRig is what the steps of TestAStaleWriterNeverReplacesANewerPlacement share: the
// database, the document service, the fixture document, its ref_key and its two blocks' node
// addresses.
type writeProtocolRig struct {
	database         *store.Store
	service          *Service
	document, refKey string
	holder, mover    string
	person           model.Actor
}

// edit writes version number of the rig's document as a person's edit does: the document's text
// replaced, then its version written through the real version write path (writeVersion), and
// answers the version's event.
func (r *writeProtocolRig) edit(t *testing.T, number int) int64 {
	t.Helper()
	if _, err := joinedReplaceText(r.service, r.document, placementVersion(number), r.person); err != nil {
		t.Fatalf("edit the document as version %d records it: %v", number, err)
	}
	written, event := writeVersionThroughTheRealPath(t, r.service, r.document, fmt.Sprintf("Version %d", number))
	if written.Version.Number != number {
		t.Fatalf("the person's edit wrote version %d, want %d", written.Version.Number, number)
	}
	return event
}

// TestAStaleWriterNeverReplacesANewerPlacement drives R29's write protocol (c-write-protocol). A
// committed transaction advances the version document_block_versions holds for each document it
// writes, and that is the fence's one number; graph_tuples.seq is the reader's, which a query, a
// census and an approval each record; no revision column stands on graph_nodes, found by the same
// search that finds one planted in a scratch migration, and no third number stands on the store.
//
// The ordering the report describes is staged: an indexing pass over version 7 of a document is
// held, a person's edit commits version 8 with a new parent for one block, and the held pass then
// writes. It states the revision it read, its part_of assertion is refused, and the parent of
// version 8 stands; the refusal is the pass's own answer, and the pass, reading the current
// placement and deciding again, writes nothing over it. The fence holds for the block index writer
// alone: a sibling order, a retraction and a value written by a transaction that read version 7
// are each accepted. Two concurrent moves of one block commit one and refuse the other, and no
// block holds two parents. A historical writer - the backfill, an importer - asserts only on a node
// that holds no current placement: run over a block that holds one, no part_of such a writer wrote
// lands on a node that held a current part_of at that tuple's seq. A transaction whose commit is
// held shows a second connection no event and no node for its write, nor the event to a
// subscriber, until the commit; a rolled-back one shows neither, ever.
//
// Each clause is a step of its own, so a clause this tree does not hold yet fails by name and the
// clauses after it still run.
func TestAStaleWriterNeverReplacesANewerPlacement(t *testing.T) {
	database, service := blockIndexService(t)
	rig := &writeProtocolRig{database: database, service: service, person: model.Actor{Kind: "user", ID: "alice"}}
	rig.document = createOwedDocument(t, database, service, "CORE-1", "", "placement", placementVersion(1), "")
	settle(t, database, rig.document)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("the first tick indexed %d documents, want the fixture's one", tick.Documents)
	}
	if err := database.Pool.QueryRow(context.Background(), `select ref_key from artifacts where id = $1`, rig.document).Scan(&rig.refKey); err != nil {
		t.Fatalf("read the fixture document's ref_key: %v", err)
	}
	rig.holder, rig.mover = blockAddress(t, database, rig.document, "holder"), blockAddress(t, database, rig.document, "mover")

	for _, step := range []struct {
		name string
		run  func(t *testing.T, rig *writeProtocolRig)
	}{
		{"a committed transaction advances the document version for each document it writes", writeProtocolStepVersions},
		{"two numbers with two names, and no third: no revision column on graph_nodes, and a query, a census and an approval record the read sequence", writeProtocolStepTwoNumbers},
		{"a held pass over version 7 is refused once a person's edit commits version 8, and point facts at a stale version are accepted", writeProtocolStepStalePass},
		{"two concurrent moves of one block commit one and refuse the other", writeProtocolStepConcurrentMoves},
		{"no part_of a historical writer wrote lands on a node that already held a placement", writeProtocolStepImport},
		{"a held commit shows no event and no node; a rollback shows neither, ever", writeProtocolStepHeldCommit},
	} {
		t.Run(step.name, func(t *testing.T) { step.run(t, rig) })
	}
}

func writeProtocolStepVersions(t *testing.T, rig *writeProtocolRig) {
	if stands := blockIndexAt(t, rig.database, rig.document); stands == nil || *stands != 1 {
		t.Fatalf("the fixture document's blocks stand at %s, want version 1", standsText(stands))
	}
	for number := 2; number <= 7; number++ {
		rig.edit(t, number)
		if stands := blockIndexAt(t, rig.database, rig.document); stands == nil || *stands != number {
			t.Errorf("the transaction that committed version %d of the document leaves document_block_versions at %s, want %d", number, standsText(stands), number)
		}
	}
	// A second document, written by transactions of its own, carries its own version.
	other := createOwedDocument(t, rig.database, rig.service, "CORE-1", "", "placement-beside", "A paragraph beside the fixture.\n", "")
	settle(t, rig.database, other)
	reconcileBlockIndex(t, rig.service, "")
	if _, err := joinedReplaceText(rig.service, other, "A paragraph beside the fixture, edited.\n", rig.person); err != nil {
		t.Fatalf("edit the second document: %v", err)
	}
	if written, _ := writeVersionThroughTheRealPath(t, rig.service, other, "The second document's edit"); written.Version.Number != 2 {
		t.Fatalf("the second document's edit wrote version %d, want 2", written.Version.Number)
	}
	if stands := blockIndexAt(t, rig.database, other); stands == nil || *stands != 2 {
		t.Errorf("the transaction that committed version 2 of the second document leaves its document_block_versions at %s, want 2", standsText(stands))
	}
	if stands := blockIndexAt(t, rig.database, rig.document); stands == nil || *stands != 7 {
		t.Errorf("the second document's write moved the fixture document's version to %s, want it at 7", standsText(stands))
	}
}

// revisionName is a column name that names a revision or a version: a third number beside the
// document version and the read sequence, where it stands on graph_nodes or graph_tuples.
var revisionName = regexp.MustCompile(`(?i)revision|version|^rev$|^rev_|_rev$`)

// storeTableStatement is a create table or an alter table of graph_nodes or graph_tuples, up to its
// semicolon, read from a migration with its line comments taken out.
var storeTableStatement = regexp.MustCompile(`(?is)\b(create\s+table|alter\s+table)\s+(?:if\s+(?:not\s+)?exists\s+)?(?:only\s+)?(?:public\.)?(graph_nodes|graph_tuples)\b(.*?);`)

// addedColumn is one column an alter table adds, and definedColumn one a create table's list
// defines, each by its name.
var (
	addedColumn   = regexp.MustCompile(`(?i)\badd\s+(?:column\s+)?(?:if\s+not\s+exists\s+)?([a-z_][a-z0-9_]*)`)
	definedColumn = regexp.MustCompile(`(?im)^[\s(]*([a-z_][a-z0-9_]*)\s+[a-z]`)
	sqlComment    = regexp.MustCompile(`--[^\n]*`)
)

// revisionColumns is the search: every column the up migrations of the directories give
// graph_nodes or graph_tuples whose name names a revision or a version, each as
// "<migration>: <table>.<column>".
func revisionColumns(t *testing.T, directories ...string) []string {
	t.Helper()
	var found []string
	for _, directory := range directories {
		names, err := filepath.Glob(filepath.Join(directory, "*.up.sql"))
		if err != nil {
			t.Fatalf("list the migrations of %s: %v", directory, err)
		}
		for _, name := range names {
			body, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			text := sqlComment.ReplaceAllString(string(body), "")
			for _, statement := range storeTableStatement.FindAllStringSubmatch(text, -1) {
				var columns []string
				if strings.EqualFold(strings.Fields(statement[1])[0], "create") {
					for _, column := range definedColumn.FindAllStringSubmatch(statement[3], -1) {
						columns = append(columns, column[1])
					}
				} else {
					for _, column := range addedColumn.FindAllStringSubmatch(statement[3], -1) {
						columns = append(columns, column[1])
					}
				}
				for _, column := range columns {
					if slices.Contains([]string{"constraint", "primary", "unique", "check", "foreign"}, strings.ToLower(column)) {
						continue
					}
					if revisionName.MatchString(column) {
						found = append(found, filepath.Base(name)+": "+statement[2]+"."+column)
					}
				}
			}
		}
	}
	return found
}

func writeProtocolStepTwoNumbers(t *testing.T, rig *writeProtocolRig) {
	ctx := context.Background()
	migrations := filepath.Join("..", "store", "migrations")
	scratch := t.TempDir()
	const planted = "99999999999999_planted_revision.up.sql"
	if err := os.WriteFile(filepath.Join(scratch, planted), []byte("alter table graph_nodes add column revision bigint;\n"), 0o600); err != nil {
		t.Fatalf("plant a revision column in a scratch migration: %v", err)
	}
	if found := revisionColumns(t, migrations, scratch); !slices.Equal(found, []string{planted + ": graph_nodes.revision"}) {
		t.Fatalf("the search over the migrations and the scratch one finds %v, want the planted column alone: a search that misses it proves nothing", found)
	}
	if found := revisionColumns(t, migrations); len(found) > 0 {
		t.Errorf("the migrations give the store a third number: %v; the fence compares the document version in document_block_versions and a reader records graph_tuples.seq", found)
	}
	rows, err := rig.database.Pool.Query(ctx, `
		select table_name::text, column_name::text from information_schema.columns
		where table_schema = 'public' and table_name in ('graph_nodes', 'graph_tuples', 'document_block_versions')
		order by table_name, column_name
	`)
	if err != nil {
		t.Fatalf("read the store's columns: %v", err)
	}
	held, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Table, Column string }])
	if err != nil {
		t.Fatalf("read the store's columns: %v", err)
	}
	documentVersion, readSequence := false, false
	for _, column := range held {
		switch {
		case column.Table == "document_block_versions" && column.Column == "version":
			documentVersion = true
		case column.Table == "graph_tuples" && column.Column == "seq":
			readSequence = true
		case column.Table != "document_block_versions" && revisionName.MatchString(column.Column):
			t.Errorf("the store holds %s.%s, a third number beside the document version and the read sequence", column.Table, column.Column)
		}
	}
	if !documentVersion || !readSequence {
		t.Errorf("the store holds the document version (document_block_versions.version) %t and the read sequence (graph_tuples.seq) %t, want both", documentVersion, readSequence)
	}
	// Each reader records the read sequence under its own name.
	var highest int64
	if err := rig.database.Pool.QueryRow(ctx, `select coalesce(max(seq), 0) from graph_tuples`).Scan(&highest); err != nil {
		t.Fatalf("read the read sequence: %v", err)
	}
	record, err := ReadQueryRecord(ctx, rig.database.Pool, "CORE-1", graphstore.QuerySpec, refs.TreeCut{})
	switch {
	case err != nil:
		t.Errorf("read a query of CORE-1 at the head: %v", err)
	case record.Cut.ReadSeq != highest:
		t.Errorf("a query of CORE-1 records the read sequence %d, want graph_tuples.seq at the commit it saw, %d", record.Cut.ReadSeq, highest)
	}
	if _, recorded := reflect.TypeOf(graphcensus.StoreReport{}).FieldByName("ReadSeq"); !recorded {
		t.Errorf("a census records no read sequence: graphcensus.StoreReport, what census --store answers, holds no ReadSeq, the graph_tuples.seq at the commit it read")
	}
	// An approval records its read sequence where its version does: the read_seq of the
	// version_renders row of the version it approves, a carried approval's being its new version's.
	// artifact_reviews takes no statement of this test beyond the one writer's (QueueReview).
	artifact, err := reviewedArtifact(ctx, rig.database.Pool, rig.document)
	if err != nil {
		t.Fatalf("read the fixture document for its review: %v", err)
	}
	approved, _ := newestStoredVersion(t, rig)
	review, _ := writeHashReview(t, rig.database, artifact, approved)
	var approvalRead int64
	switch err := rig.database.Pool.QueryRow(ctx, `select read_seq from version_renders where artifact_id = $1 and number = $2`,
		review.ArtifactID, review.Version).Scan(&approvalRead); {
	case errors.Is(err, pgx.ErrNoRows):
		t.Errorf("an approval records no read sequence: the review %s approves version %d of %s, and that version holds no version_renders row, whose read_seq is the graph_tuples.seq the approval read",
			review.ID, review.Version, rig.refKey)
	case err != nil:
		t.Fatalf("read the version_renders row of version %d of %s: %v", review.Version, rig.refKey, err)
	case approvalRead < 1 || approvalRead > highest:
		t.Errorf("version %d of %s, which the review %s approves, records the read sequence %d, want a graph_tuples.seq at or below %d", review.Version, rig.refKey, review.ID, approvalRead, highest)
	}
}

func writeProtocolStepStalePass(t *testing.T, rig *writeProtocolRig) {
	ctx := context.Background()
	const stale, newer = 7, 8
	if stands := blockIndexAt(t, rig.database, rig.document); stands == nil || *stands != stale {
		t.Fatalf("the document's blocks stand at %s, want version %d, which the held pass reads", standsText(stands), stale)
	}
	if was := currentPartOf(t, rig.database, rig.mover); was.ToKind != graphstore.KindArtifact {
		t.Fatalf("at version %d block %s is part of %s/%s, want the document: the person's edit must move it", stale, rig.mover, was.ToKind, was.ToID)
	}
	// The held pass reads version 7 as an indexing pass reads it, its markdown and its blocks.
	staleMarkdown, staleBlocks, err := rig.service.TextWithBlocks(ctx, rig.document)
	if err != nil {
		t.Fatalf("read version %d of the document: %v", stale, err)
	}
	staleConn, err := rig.database.Pool.Dedicated(ctx, "write-protocol-held-pass")
	if err != nil {
		t.Fatalf("open the held pass's connection: %v", err)
	}
	defer staleConn.Close(ctx)
	staleTx, err := staleConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the held pass: %v", err)
	}
	defer staleTx.Rollback(ctx)
	// A writer of point facts reads the document at version 7 too, before the edit commits.
	if err := statedApart(ctx, rig.database, blockValueTuple(rig.holder, "probe", "asserted before the edit", true)); err != nil {
		t.Fatalf("assert the value the stale writer retracts: %v", err)
	}
	pointTx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the point facts' writer: %v", err)
	}
	defer pointTx.Rollback(ctx)
	var pointRead int
	if err := pointTx.QueryRow(ctx, `select version from document_block_versions where artifact_id = $1`, rig.document).Scan(&pointRead); err != nil || pointRead != stale {
		t.Fatalf("the point facts' writer reads version %d (%v), want %d", pointRead, err, stale)
	}

	// The person's edit commits version 8, which moves mover under holder.
	newerEvent := rig.edit(t, newer)
	if stands := blockIndexAt(t, rig.database, rig.document); stands == nil || *stands != newer {
		t.Fatalf("after the person's edit the document's blocks stand at %s, want version %d", standsText(stands), newer)
	}
	moved := currentPartOf(t, rig.database, rig.mover)
	if moved.ToKind != refs.BlockNodeKind || moved.ToID != rig.holder || moved.SourceEvent != newerEvent {
		t.Fatalf("after version %d block %s is part of %s/%s under event %d, want block/%s under the edit's event %d",
			newer, rig.mover, moved.ToKind, moved.ToID, moved.SourceEvent, rig.holder, newerEvent)
	}
	stated := parentsNow(t, rig.database, rig.document)

	// The held pass writes, stating the revision it read.
	before := highestTuple(t, rig.database)
	_, won, write, _, err := refs.IndexBlocks(ctx, staleTx, rig.document, refs.OneVersion(rig.document, stale, time.Now()), staleMarkdown, staleBlocks, nil, nil, refs.TypedFacts{})
	if err != nil {
		t.Fatalf("run the held pass at version %d: %v", stale, err)
	}
	if err := graphstore.Caused(ctx, staleTx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the held pass stated: %v", err)
	}
	if err := staleTx.Commit(ctx); err != nil {
		t.Fatalf("commit the held pass: %v", err)
	}
	if won {
		t.Errorf("the held pass at version %d answered that it stands, over the stored version %d: its part_of assertion was not refused", stale, newer)
	}
	if appended := tuplesSince(t, rig.database, rig.document, before); appended != 0 {
		t.Errorf("the held pass at version %d appended %d tuples below the stored version %d, want none", stale, appended, newer)
	}
	if stands := blockIndexAt(t, rig.database, rig.document); stands == nil || *stands != newer {
		t.Errorf("after the held pass at version %d the document's blocks stand at %s, want version %d", stale, standsText(stands), newer)
	}
	if now := currentPartOf(t, rig.database, rig.mover); now.Seq != moved.Seq || now.ToID != rig.holder {
		t.Errorf("block %s is part of %s/%s (tuple %d), the parent the older pass at revision %d asserted, standing over the newer edit's block/%s at revision %d (tuple %d)",
			rig.mover, now.ToKind, now.ToID, now.Seq, stale, rig.holder, newer, moved.Seq)
	}
	if now := parentsNow(t, rig.database, rig.document); !sameParents(now, stated) {
		t.Errorf("the held pass at version %d moved the parents version %d stated from %v to %v", stale, newer, stated, now)
	}

	// The pass reads the current placement and decides again: the document stands at version 8,
	// whose placement is the store's, so it writes nothing over it.
	if stands := blockIndexAt(t, rig.database, rig.document); stands != nil {
		currentMarkdown, currentBlocks, err := rig.service.TextWithBlocks(ctx, rig.document)
		if err != nil {
			t.Fatalf("read the current version of the document: %v", err)
		}
		again := highestTuple(t, rig.database)
		reread, err := rig.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the pass's second decision: %v", err)
		}
		_, won, write, _, err = refs.IndexBlocks(ctx, reread, rig.document, refs.OneVersion(rig.document, *stands, time.Now()), currentMarkdown, currentBlocks, nil, nil, refs.TypedFacts{})
		if err == nil {
			err = graphstore.Caused(ctx, reread, write, graphstore.NoEvent)
		}
		if err == nil {
			err = reread.Commit(ctx)
		}
		_ = reread.Rollback(ctx)
		if err != nil {
			t.Fatalf("the pass's second decision, at the version %d it read: %v", *stands, err)
		}
		if appended := tuplesSince(t, rig.database, rig.document, again); won || appended != 0 {
			t.Errorf("deciding again at the current version %d the pass answered won %t and appended %d tuples, want nothing written over the current placement", *stands, won, appended)
		}
		if now := currentPartOf(t, rig.database, rig.mover); now.Seq != moved.Seq {
			t.Errorf("deciding again the pass moved block %s to %s/%s (tuple %d), want version %d's tuple %d", rig.mover, now.ToKind, now.ToID, now.Seq, newer, moved.Seq)
		}
	}

	// The fence for the block index writer alone, at a stale document version.
	fenced := highestTuple(t, rig.database)
	fenceTx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the stale block index write: %v", err)
	}
	_, won, write, _, err = refs.IndexBlocks(ctx, fenceTx, rig.document, refs.OneVersion(rig.document, 3, time.Now()), staleMarkdown, staleBlocks, nil, nil, refs.TypedFacts{})
	if err == nil {
		err = graphstore.Caused(ctx, fenceTx, write, graphstore.NoEvent)
	}
	if err == nil {
		err = fenceTx.Commit(ctx)
	}
	_ = fenceTx.Rollback(ctx)
	if err != nil {
		t.Fatalf("the block index write at version 3: %v", err)
	}
	if appended := tuplesSince(t, rig.database, rig.document, fenced); won || appended != 0 {
		t.Errorf("the block index writer at version 3, below the stored version %d, answered won %t and appended %d tuples, want it refused", newer, won, appended)
	}

	// A sibling order, a retraction and a value, by the writer that read version 7: point facts
	// the owning write appends, which no fence refuses.
	order := blockValueTuple(rig.holder, refs.AttributeRank, "0zz", true)
	retraction := blockValueTuple(rig.holder, "probe", "asserted before the edit", false)
	value := blockValueTuple(rig.mover, "probe", "written at version 7", true)
	pointed := highestTuple(t, rig.database)
	if err := graphstore.Write(ctx, pointTx, nil, []graphstore.Tuple{order, retraction, value}); err != nil {
		t.Errorf("the point facts written at version %d were refused: %v", stale, err)
	} else if err := pointTx.Commit(ctx); err != nil {
		t.Errorf("the point facts written at version %d did not commit: %v", stale, err)
	}
	for _, fact := range []struct {
		what          string
		address, name string
		want          string
		wantStated    bool
	}{
		{"the sibling order", rig.holder, refs.AttributeRank, "0zz", true},
		{"the retraction", rig.holder, "probe", "", false},
		{"the value", rig.mover, "probe", "written at version 7", true},
	} {
		held, stated, err := graphstore.CurrentValue(ctx, rig.database.Pool, refs.BlockNodeKind, fact.address, fact.name)
		if err != nil {
			t.Fatalf("read the %s of %s: %v", fact.name, fact.address, err)
		}
		if held != fact.want || stated != fact.wantStated {
			t.Errorf("%s written by a writer that read version %d left the %s of %s at %q (stated %t), want it accepted: %q (stated %t)",
				fact.what, stale, fact.name, fact.address, held, stated, fact.want, fact.wantStated)
		}
	}
	if appended := tuplesSince(t, rig.database, rig.document, pointed); appended != 3 {
		t.Errorf("the point facts written at version %d appended %d tuples, want their 3", stale, appended)
	}
}

// statedApart appends tuples in a transaction of its own, as a write that states facts and no
// node does.
func statedApart(ctx context.Context, database *store.Store, tuples ...graphstore.Tuple) error {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// blockValueTuple is the value tuple of attribute on the block node at address, asserted or
// retracted, written by alice now with no event.
func blockValueTuple(address, attribute, value string, asserted bool) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: refs.BlockNodeKind, FromID: address, Kind: attribute, ToKind: graphstore.KindValue, ToID: value,
		At: time.Now().UTC(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: asserted,
	}
}

// movePass is the blocks of one pass over the document that places mover where nested says: under
// holder, or at the document's top level. Holder takes the markdown's first byte and mover its
// second, under holder's range only when nested; every other block takes one byte of its own
// after them, so it stands at the top level either way.
func movePass(t *testing.T, rig *writeProtocolRig, nested bool) []model.ArtifactBlock {
	t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(),
		`select block_id from document_blocks where artifact_id = $1 order by block_id`, rig.document)
	if err != nil {
		t.Fatalf("read the blocks of the document: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the blocks of the document: %v", err)
	}
	holderTo := 1
	if nested {
		holderTo = 2
	}
	blocks := []model.ArtifactBlock{
		{ID: "holder", Type: "callout", From: 0, To: holderTo},
		{ID: "mover", Type: "callout", From: 1, To: 2},
	}
	at := 2
	for _, id := range ids {
		if id == "holder" || id == "mover" {
			continue
		}
		blocks = append(blocks, model.ArtifactBlock{ID: id, Type: "paragraph", From: at, To: at + 1})
		at++
	}
	return blocks
}

func writeProtocolStepConcurrentMoves(t *testing.T, rig *writeProtocolRig) {
	ctx := context.Background()
	_, markdown := newestStoredVersion(t, rig)
	toTop, underHolder := movePass(t, rig, false), movePass(t, rig, true)
	if len(markdown) < len(toTop) {
		t.Fatalf("the stored markdown holds %d bytes for %d blocks", len(markdown), len(toTop))
	}
	next := nextVersionRow(t, rig.database, rig.document, markdown)
	first, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the first move: %v", err)
	}
	defer first.Rollback(ctx)
	_, firstWon, firstWrite, _, err := refs.IndexBlocks(ctx, first, rig.document, refs.OneVersion(rig.document, next, time.Now()), markdown, toTop, nil, nil, refs.TypedFacts{})
	if err != nil {
		t.Fatalf("the first move at version %d: %v", next, err)
	}
	if err := graphstore.Caused(ctx, first, firstWrite, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the first move stated: %v", err)
	}
	secondConn, err := rig.database.Pool.Dedicated(ctx, "write-protocol-second-move")
	if err != nil {
		t.Fatalf("open the second move's connection: %v", err)
	}
	defer secondConn.Close(ctx)
	pid := secondConn.PgConn().PID()
	type moveAnswer struct {
		won bool
		err error
	}
	answered := make(chan moveAnswer, 1)
	go func() {
		answered <- func() moveAnswer {
			tx, err := secondConn.Begin(ctx)
			if err != nil {
				return moveAnswer{err: err}
			}
			defer tx.Rollback(ctx)
			_, won, write, _, err := refs.IndexBlocks(ctx, tx, rig.document, refs.OneVersion(rig.document, next, time.Now()), markdown, underHolder, nil, nil, refs.TypedFacts{})
			if err != nil {
				return moveAnswer{err: err}
			}
			if err := graphstore.Caused(ctx, tx, write, graphstore.NoEvent); err != nil {
				return moveAnswer{err: err}
			}
			return moveAnswer{won: won, err: tx.Commit(ctx)}
		}()
	}()
	if early, done := storetest.WaitForQueuedBehind(t, first, pid, answered); done {
		t.Fatalf("the second move answered (won %t, %v) without waiting on the first", early.won, early.err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first move: %v", err)
	}
	second := <-answered
	t.Logf("the first move answered won %t; the second answered won %t, %v", firstWon, second.won, second.err)
	if !firstWon || second.err == nil && second.won {
		t.Errorf("two concurrent moves of block %s at version %d: the first answered won %t, the second won %t (%v); want one committed and one refused",
			rig.mover, next, firstWon, second.won, second.err)
	}
	// parentsNow and currentPartOf each fail on a block holding other than one parent.
	parents := parentsNow(t, rig.database, rig.document)
	if parent := currentPartOf(t, rig.database, rig.mover); parent.ToKind != graphstore.KindArtifact {
		t.Errorf("after the two moves block %s is part of %s/%s, want the document the committed move put it in (all parents %v)", rig.mover, parent.ToKind, parent.ToID, parents)
	}
}

// newestStoredVersion is the number and the markdown of the document's newest version that stores
// markdown.
func newestStoredVersion(t *testing.T, rig *writeProtocolRig) (int, string) {
	t.Helper()
	var number int
	var markdown string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select number, markdown from artifact_versions
		where artifact_id = $1 and markdown is not null order by number desc limit 1
	`, rig.document).Scan(&number, &markdown); err != nil {
		t.Fatalf("read the newest stored version of the document: %v", err)
	}
	return number, markdown
}

// historicalWriters are the writers that state what was true before the store, or what another
// system holds, rather than a change of the document they write: the backfill of the graph store
// (graphcensus/backfill.go, backfillActor), the backfill of comments' anchor blocks
// (AnchorBlockBackfillActor), the architecture importer (architecture/importer.go, SystemActor)
// and the pre-mortem conversion (api/premortem_convert.go, PremortemConverter), which states a
// pre-mortem's risks and their preventions from a document written before them. Each is named by
// its actor's id, the one thing its tuples carry that tells them apart; all but the anchor
// backfill's are spelled here because none of them is reachable from this package, and api imports
// this package. An issue's cutover conversion is no historical writer and stays off the list.
var historicalWriters = []string{"dispatch-backfill-graph-store", AnchorBlockBackfillActor.ID, "architecture-importer", "premortem-convert"}

// historicalPlacement is one tuple a historical writer wrote, as the clause examines it: the node
// it runs out of, its kind, the tuple written whole, and whether it is a part_of that landed on a
// node already holding a current part_of at its seq.
type historicalPlacement struct {
	from, kind, tuple string
	landed            bool
}

// historicalPlacementsStatement reads every tuple a historical writer ($1, their actor ids) wrote,
// in seq order, each with whether it is an asserted part_of whose node held a current part_of at
// its seq: some far end whose newest part_of tuple before it - by time, then seq, as a current read
// orders them (refs' treeRelationsStatement) - asserts.
const historicalPlacementsStatement = `
	select t.from_kind || ':' || t.from_id, t.kind,
		format('seq=%s from=%s:%s kind=%s to=%s:%s asserted=%s at=%s who=%s source_event=%s',
			t.seq, t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.asserted, t.at, t.who, t.source_event),
		t.kind = 'part_of' and t.asserted and exists (
			select 1 from (
				select distinct on (held.to_kind, held.to_id) held.asserted
				from graph_tuples held
				where held.from_kind = t.from_kind and held.from_id = t.from_id and held.kind = 'part_of' and held.seq < t.seq
				order by held.to_kind, held.to_id, held.at desc, held.seq desc
			) placement
			where placement.asserted
		)
	from graph_tuples t
	where t.who->>'kind' = 'system' and t.who->>'id' = any($1)
	order by t.seq`

// historicalPlacements is every tuple a historical writer wrote, read through q, as
// historicalPlacementsStatement reads it.
func historicalPlacements(t *testing.T, q Queryer) []historicalPlacement {
	t.Helper()
	rows, err := q.Query(context.Background(), historicalPlacementsStatement, historicalWriters)
	if err != nil {
		t.Fatalf("read the tuples the historical writers wrote: %v", err)
	}
	examined, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (historicalPlacement, error) {
		var placement historicalPlacement
		err := row.Scan(&placement.from, &placement.kind, &placement.tuple, &placement.landed)
		return placement, err
	})
	if err != nil {
		t.Fatalf("read the tuples the historical writers wrote: %v", err)
	}
	return examined
}

func writeProtocolStepImport(t *testing.T, rig *writeProtocolRig) {
	ctx := context.Background()
	placed := currentPartOf(t, rig.database, rig.mover)
	// The release before rewrites the block's row naming no node, so the backfill reads it as one
	// of the rows written before the store, while the node it names holds a placement.
	if _, err := rig.database.Pool.Exec(ctx, `
		update document_blocks set node_kind = null, node_address = null where artifact_id = $1 and block_id = 'mover'
	`, rig.document); err != nil {
		t.Fatalf("leave block mover's row naming no node: %v", err)
	}
	if _, err := graphcensus.Backfill(ctx, rig.database, io.Discard); err != nil {
		t.Fatalf("run the backfill (graphcensus.Backfill): %v", err)
	}
	examined := historicalPlacements(t, rig.database.Pool)
	placements, nodes := 0, map[string]bool{}
	for _, tuple := range examined {
		nodes[tuple.from] = true
		if tuple.kind == graphstore.TuplePartOf {
			placements++
		}
		if tuple.from == refs.BlockNodeKind+":"+rig.mover {
			t.Logf("a historical writer wrote out of block %s, which held part_of %s/%s (tuple %d): %s", rig.mover, placed.ToKind, placed.ToID, placed.Seq, tuple.tuple)
		}
	}
	t.Logf("examined %d tuples the historical writers %v wrote, out of %d nodes, %d of them part_of", len(examined), historicalWriters, len(nodes), placements)
	if len(examined) == 0 {
		t.Errorf("the clause examined no tuple a historical writer wrote, so it proves nothing: the backfill over block %s wrote none", rig.mover)
	}
	for _, tuple := range examined {
		if tuple.landed {
			t.Errorf("a historical writer's part_of lands on %s, which already held a current part_of at its seq: %s; a historical writer asserts only on a node that holds no current placement",
				tuple.from, tuple.tuple)
		}
	}
}

// heldCommitLock is the advisory lock a held commit waits on: the deferred trigger
// writeProtocolHoldCommit takes it at the commit of every transaction that inserted an event, and
// the test holds it to keep such a commit from finishing.
const heldCommitLock = 1149029

func writeProtocolStepHeldCommit(t *testing.T, rig *writeProtocolRig) {
	ctx := context.Background()
	if _, err := rig.database.Pool.Exec(ctx, `
		create function write_protocol_hold_commit() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock(`+fmt.Sprint(heldCommitLock)+`);
			return null;
		end $$;
		create constraint trigger write_protocol_hold_commit after insert on events
			deferrable initially deferred for each row execute function write_protocol_hold_commit();
	`); err != nil {
		t.Fatalf("install the trigger that holds a commit: %v", err)
	}
	defer func() {
		if _, err := rig.database.Pool.Exec(context.Background(), `
			drop trigger write_protocol_hold_commit on events; drop function write_protocol_hold_commit();
		`); err != nil {
			t.Errorf("drop the trigger that holds a commit: %v", err)
		}
	}()
	published, unsubscribe := rig.service.events.Subscribe()
	defer unsubscribe()
	listener, err := rig.database.Pool.Dedicated(ctx, "write-protocol-listener")
	if err != nil {
		t.Fatalf("open the second connection's listener: %v", err)
	}
	defer listener.Close(ctx)
	if _, err := listener.Exec(ctx, `listen dispatch_events`); err != nil {
		t.Fatalf("listen on dispatch_events: %v", err)
	}

	// write writes version number of the document through the real version write path, its event
	// appended in its transaction and handed to the ledger, which publishes it after the commit
	// (Ledger.Commit), and then commits it or rolls it back.
	write := func(number int, commit bool) error {
		if _, err := joinedReplaceText(rig.service, rig.document, placementVersion(number), rig.person); err != nil {
			return fmt.Errorf("edit the document as version %d records it: %w", number, err)
		}
		tx, err := rig.database.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		joined, ledger := rig.service.Join(ctx, tx)
		defer ledger.Discard()
		result, err := rig.service.NamedVersion(joined, rig.document, fmt.Sprintf("Version %d", number), rig.person)
		if err != nil {
			return fmt.Errorf("write version %d: %w", number, err)
		}
		if !result.Wrote || result.Version.Number != number {
			return fmt.Errorf("the version write wrote %t, version %d, want version %d", result.Wrote, result.Version.Number, number)
		}
		payload, err := json.Marshal(map[string]any{"artifact_id": rig.document, "version": map[string]any{"number": number}})
		if err != nil {
			return err
		}
		event, err := documentEvent(ctx, tx, "artifact.version", payload)
		if err != nil {
			return err
		}
		key := "CORE-1"
		eventSink(joined)(model.Event{ID: event, IssueKey: &key, Type: "artifact.version", Actor: rig.person, Payload: json.RawMessage(payload)})
		if err := causeInWritePhase(joined, tx, result.Blocks, event); err != nil {
			return fmt.Errorf("append what version %d's block index stated: %w", number, err)
		}
		if !commit {
			return nil
		}
		return ledger.Commit(ctx)
	}
	// seen reads, on the second connection, whether the store answers an event after afterEvent
	// and the node of version number.
	seen := func(afterEvent int64, number int) (events, nodes int) {
		t.Helper()
		if err := rig.database.Pool.QueryRow(ctx, `
			select (select count(*) from events where id > $1),
			       (select count(*) from graph_nodes where kind = $2 and id = $3)
		`, afterEvent, graphstore.KindVersion, graphstore.VersionID(rig.refKey, number)).Scan(&events, &nodes); err != nil {
			t.Fatalf("read the events and the version node on the second connection: %v", err)
		}
		return events, nodes
	}
	// heard answers the first event the service publishes within quiet, and false where none.
	heard := func(quiet time.Duration) (model.Event, bool) {
		select {
		case event := <-published:
			return event, true
		case <-time.After(quiet):
			return model.Event{}, false
		}
	}
	// notified answers whether the second connection's listener is told of an event within quiet.
	notified := func(quiet time.Duration) bool {
		waited, cancel := context.WithTimeout(ctx, quiet)
		defer cancel()
		_, err := listener.WaitForNotification(waited)
		return err == nil
	}
	drain := func() {
		for {
			if _, ok := heard(50 * time.Millisecond); !ok {
				return
			}
		}
	}
	lastEvent := func() int64 {
		var id int64
		if err := rig.database.Pool.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&id); err != nil {
			t.Fatalf("read the newest event: %v", err)
		}
		return id
	}
	latest, _ := newestStoredVersion(t, rig)
	held := latest + 1

	holder, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder of the commit: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock($1)`, heldCommitLock); err != nil {
		t.Fatalf("take the lock a commit waits on: %v", err)
	}
	drain()
	for notified(50 * time.Millisecond) {
	}
	afterEvent := lastEvent()
	committed := make(chan error, 1)
	go func() { committed <- write(held, true) }()
	if waiters := storetest.WaitForLockWaiters(t, holder, 1); waiters < 1 {
		select {
		case err := <-committed:
			t.Fatalf("the write of version %d finished (%v) without its commit waiting on the held lock", held, err)
		default:
			t.Fatalf("the write of version %d never reached its commit", held)
		}
	}
	events, nodes := seen(afterEvent, held)
	if events != 0 || nodes != 0 {
		t.Errorf("while the commit of version %d is held, a second connection reads %d events of the write and %d nodes of its version, want neither", held, events, nodes)
	}
	if event, ok := heard(500 * time.Millisecond); ok {
		t.Errorf("while the commit of version %d is held, the service published the event %d (%s): a notification became eligible before its own revision committed", held, event.ID, event.Type)
	}
	if notified(500 * time.Millisecond) {
		t.Errorf("while the commit of version %d is held, the second connection's listener was told of an event", held)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release the held commit: %v", err)
	}
	select {
	case err := <-committed:
		if err != nil {
			t.Fatalf("the held write of version %d: %v", held, err)
		}
	case <-time.After(fenceWait):
		t.Fatalf("the write of version %d did not commit within %s of its release", held, fenceWait)
	}
	if events, nodes := seen(afterEvent, held); events == 0 || nodes != 1 {
		t.Errorf("once the commit of version %d is released, a second connection reads %d events of the write and %d nodes of its version, want both", held, events, nodes)
	}
	if event, ok := heard(5 * time.Second); !ok || event.Type != "artifact.version" || event.ID <= afterEvent {
		t.Errorf("once the commit of version %d is released, the service published %+v (heard %t), want the write's artifact.version event", held, event, ok)
	}
	if !notified(5 * time.Second) {
		t.Errorf("once the commit of version %d is released, the second connection's listener was told of no event", held)
	}

	// The rollback: the same write, its transaction rolled back, shows neither, ever.
	drain()
	for notified(50 * time.Millisecond) {
	}
	afterEvent = lastEvent()
	rolled := held + 1
	if err := write(rolled, false); err != nil {
		t.Fatalf("the write of version %d, rolled back: %v", rolled, err)
	}
	for _, wait := range []time.Duration{0, time.Second} {
		time.Sleep(wait)
		if events, nodes := seen(afterEvent, rolled); events != 0 || nodes != 0 {
			t.Errorf("%v after the rollback of version %d a second connection reads %d events of the write and %d nodes of its version, want neither", wait, rolled, events, nodes)
		}
	}
	if event, ok := heard(time.Second); ok {
		t.Errorf("after the rollback of version %d the service published %+v, want nothing", rolled, event)
	}
	if notified(time.Second) {
		t.Errorf("after the rollback of version %d the second connection's listener was told of an event", rolled)
	}
}
