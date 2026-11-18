package refs

import (
	"context"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// mainClosureQuery is the closure statement main runs, held here letter for letter as the
// oracle of the comparison below. It is a copy of the text this change replaced, not text built
// from the package's own constants: a statement built from those would move with them, and the
// two sides would agree however the reach of the walk changed.
const mainClosureQuery = `
		with recursive closure as (
			select case when r.to_kind = 'block' then split_part(r.to_id, '#', 1) else r.to_id end as ref_key, 1 as depth, r.from_kind as via_kind, r.from_id as via_id
			from refs r
			where (r.to_kind = 'artifact' or (r.to_kind = 'block' and exists (select 1 from document_blocks d where d.node_id = r.to_id))) and (
				  (r.from_kind = 'ask'      and r.from_id in (select id::text from asks      where issue_key = $1))
				or (r.from_kind = 'comment'  and r.from_id in (select id::text from comments  where issue_key = $1))
				or (r.from_kind = 'message'  and r.from_id in (select id::text from messages  where issue_key = $1))
				or (r.from_kind = 'artifact' and r.from_id in (select id::text from artifacts where issue_key = $1))
			)
			union
			select case when r.to_kind = 'block' then split_part(r.to_id, '#', 1) else r.to_id end, c.depth + 1, 'artifact', a.id::text
			from closure c
			join artifacts a on a.ref_key = c.ref_key
			join refs r on r.from_kind = 'artifact' and r.from_id = a.id::text and (r.to_kind = 'artifact' or (r.to_kind = 'block' and exists (select 1 from document_blocks d where d.node_id = r.to_id)))
			where c.depth < 9
		)
		select distinct on (c.ref_key) c.ref_key, c.depth, c.via_kind, c.via_id
		from closure c
		join artifacts a on a.ref_key = c.ref_key
		where a.issue_key is distinct from $1
		order by c.ref_key, c.depth, c.via_kind, c.via_id
	`

// closureRow is one row of the closure statement, before Closure reduces it to a Member.
type closureRow struct {
	refKey  string
	depth   int
	viaKind string
	viaID   string
}

// closureFixture is the data both tests below read: an issue whose comment cites a document and
// two document blocks, one of which no current version holds, and a chain of documents ten deep
// so that the walk reports its ninth hop truncated.
type closureFixture struct {
	database *store.Store
	// refKeys names the ref_key of each document the closure reaches, in the order the walk
	// reaches them: the comment's own targets first, then the chain.
	heldBlockDocument string
	droppedBlock      string
	comment           string
}

// newClosureFixture writes the fixture and commits it, so that the plan test can read the
// statement's plan on a connection of its own.
func newClosureFixture(t *testing.T) closureFixture {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the closure fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")

	// A chain of ten documents: the comment reaches the first, and each cites the next, so the
	// tenth sits at depth ten and the walk stops with its ninth hop reported as truncated.
	ids := make(map[string]string)
	refs := make(map[string]string)
	for index := 1; index <= 10; index++ {
		slug := "d" + strconv.Itoa(index)
		ids[slug], refs[slug] = createReferenceArtifact(t, tx, "", "CORE", slug)
	}
	for index := 1; index < 10; index++ {
		insertReference(t, tx, "artifact", ids["d"+strconv.Itoa(index)], "artifact", refs["d"+strconv.Itoa(index+1)])
	}

	// Two block targets of the same shape, one held by a current version of its document and
	// one not: the held one reaches its document and the dropped one reaches nothing, which is
	// the answer the document_blocks probe decides.
	held, heldRef := createReferenceArtifact(t, tx, "", "CORE", "held-md")
	_, droppedRef := createReferenceArtifact(t, tx, "", "CORE", "dropped-md")
	insertDocumentBlock(t, tx, held, "b1", heldRef+"#b1")
	comment := createReferenceComment(t, tx, "CORE-1")
	insertReference(t, tx, "comment", comment, "artifact", refs["d1"])
	insertReference(t, tx, "comment", comment, "block", heldRef+"#b1")
	insertReference(t, tx, "comment", comment, "block", droppedRef+"#b1")
	// The recursive term meets a block target too: the first document cites a block of the
	// held document, and a block of the dropped one.
	insertReference(t, tx, "artifact", ids["d1"], "block", heldRef+"#b1")
	insertReference(t, tx, "artifact", ids["d1"], "block", droppedRef+"#b1")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the closure fixture: %v", err)
	}
	return closureFixture{database: database, heldBlockDocument: heldRef, droppedBlock: droppedRef, comment: comment}
}

func insertDocumentBlock(t *testing.T, tx pgx.Tx, artifactID, blockID, nodeID string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, $2, $3, 'paragraph', 'sha', 'excerpt', 1, now())
	`, artifactID, blockID, nodeID); err != nil {
		t.Fatalf("index block %q: %v", nodeID, err)
	}
}

// closureStatementRows runs statement with args and answers its rows in the order it gave them,
// each by the four columns main's statement answers, which this package's statement answers first.
func closureStatementRows(t *testing.T, database *store.Store, statement string, args ...any) []closureRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement, args...)
	if err != nil {
		t.Fatalf("run the closure statement: %v", err)
	}
	defer rows.Close()
	var got []closureRow
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("read a closure row: %v", err)
		}
		refKey, refKeyRead := values[0].(string)
		depth, depthRead := values[1].(int32)
		viaKind, viaKindRead := values[2].(string)
		viaID, viaIDRead := values[3].(string)
		if !refKeyRead || !depthRead || !viaKindRead || !viaIDRead {
			t.Fatalf("a closure row leads with %#v, want a ref_key, a depth, a via kind and a via id", values[:4])
		}
		got = append(got, closureRow{refKey: refKey, depth: int(depth), viaKind: viaKind, viaID: viaID})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the closure rows: %v", err)
	}
	return got
}

// documentMember is the member a closure answers for the document stored at refKey, read
// through q, reached at depth through via: kind artifact at the document's own address
// (ArtifactRef).
func documentMember(t *testing.T, q Queryer, refKey string, depth int, via model.ReferenceVia) Member {
	t.Helper()
	var issue, session *string
	var project, slug, kind string
	var primary bool
	if err := q.QueryRow(context.Background(), `
		select issue_key, coalesce(project_key, ''), session_id, slug, kind, is_primary from artifacts where ref_key = $1
	`, refKey).Scan(&issue, &project, &session, &slug, &kind, &primary); err != nil {
		t.Fatalf("read the document at %s: %v", refKey, err)
	}
	return Member{Kind: graphstore.KindArtifact, Address: ArtifactRef(issue, project, session, slug, kind, primary),
		RefKey: refKey, Depth: depth, Via: via}
}

// The closure answers, row for row and in the same order, what main's statement answered over
// the same data, and Closure reduces main's rows to the members and the truncation flag it
// reports: a block its document still holds reaches that document, one no version holds reaches
// nothing, and the tenth document of the chain is the ninth hop that is reported truncated. It
// reads as somebody who proves no author, and no node of this data is reserved for one, so the
// predicates it binds leave every row main answered.
func TestTheClosureAnswersWhatMainsStatementDid(t *testing.T) {
	fixture := newClosureFixture(t)
	want := closureStatementRows(t, fixture.database, mainClosureQuery, "CORE-1")
	if len(want) < 9 {
		t.Fatalf("main's closure answered %d rows, too few for the comparison to decide anything: %v", len(want), want)
	}
	if got := closureStatementRows(t, fixture.database, closureQuery(), "CORE-1", VisibilityArg(model.Actor{})); !reflect.DeepEqual(got, want) {
		t.Fatalf("the closure answered\n%v\nwant main's rows\n%v", got, want)
	}

	// Main's own reduction of its own rows: a row at depth nine is the truncation and no member,
	// and the rest are ordered by depth then ref key. No document of this data moved, so each
	// member is the document of kind artifact at its own address.
	wantMembers := []Member{}
	wantTruncated := false
	for _, row := range want {
		if row.depth == 9 {
			wantTruncated = true
			continue
		}
		wantMembers = append(wantMembers, documentMember(t, fixture.database.Pool, row.refKey, row.depth,
			model.ReferenceVia{Kind: row.viaKind, ID: row.viaID}))
	}
	sort.SliceStable(wantMembers, func(left, right int) bool {
		if wantMembers[left].Depth != wantMembers[right].Depth {
			return wantMembers[left].Depth < wantMembers[right].Depth
		}
		return wantMembers[left].RefKey < wantMembers[right].RefKey
	})
	if !wantTruncated {
		t.Fatal("main's closure reached no ninth hop, so the fixture decides nothing about truncation")
	}
	members, truncated, err := Closure(context.Background(), fixture.database.Pool, model.Actor{}, "CORE-1")
	if err != nil {
		t.Fatalf("read the closure: %v", err)
	}
	if !truncated || !reflect.DeepEqual(members, wantMembers) {
		t.Fatalf("Closure = %#v (truncated %t); want %#v (truncated true)", members, truncated, wantMembers)
	}
	for _, member := range members {
		if member.RefKey == fixture.droppedBlock {
			t.Errorf("the closure holds %s, whose only citation names a block no current version holds", fixture.droppedBlock)
		}
	}
}

// The closure reaches document_blocks through document_blocks_node_id, the hash index on
// exactly the column it probes, and never reads that table whole. An `exists` probe written in
// the predicate is planned as a hashed subplan instead, which reads all of document_blocks to
// build its hash and cannot use that index at all; the lateral join is what keeps the probe a
// keyed one. Every read of refs is keyed too, which is why the predicate names the target kinds
// of its own: with the block test moved above a join, that is the only clause the planner can
// restrict the scan with.
func TestTheClosureProbesDocumentBlocksThroughItsNodeIndex(t *testing.T) {
	fixture := newClosureFixture(t)
	ctx := context.Background()
	tx, err := fixture.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	// A fixture this small is read fastest whole, so sequential scans are off: what is left is
	// the index each read is written for, and a read no index serves still shows as a scan
	// carrying no index condition.
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("set local enable_seqscan = off: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+closureQuery(), "CORE-1", VisibilityArg(model.Actor{})).Scan(&raw); err != nil {
		t.Fatalf("explain the closure: %v", err)
	}
	plan := explaintest.OnePlan(t, raw)

	if explaintest.SeqScansRelation(t, plan, "document_blocks") {
		t.Errorf("the closure reads document_blocks whole:\n%s", plan)
	}
	explaintest.NewIndexKeys(fixture.database.Pool).RequireIndexKeyed(t, plan,
		"document_blocks_node_id", "the closure", explaintest.CondRequired)
	for _, scan := range explaintest.IndexScans(t, plan) {
		if scan.Relation == "refs" && scan.Cond == "" {
			t.Errorf("the closure reads %s with no index condition, so it reads every refs row:\n%s", scan, plan)
		}
	}
	if explaintest.SeqScansRelation(t, plan, "refs") {
		t.Errorf("the closure reads refs whole:\n%s", plan)
	}
}

// writeLensNode writes the lens id under the issue key, live or gone, through the store's writer
// as the reasoning write stores one: the node and its part_of. A second call moves its state
// alone, as the state route's write does.
func writeLensNode(t *testing.T, database *store.Store, id, key, project, state string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lens's write: %v", err)
	}
	defer tx.Rollback(ctx)
	writer, at := model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC()
	node, held, err := graphstore.ReadNode(ctx, tx, graphstore.KindLens, id)
	if err != nil {
		t.Fatalf("read the lens %s: %v", id, err)
	}
	var tuples []graphstore.Tuple
	if !held {
		node = graphstore.Node{Kind: graphstore.KindLens, ID: id, Supertag: graphstore.KindLens,
			Visibility: graphstore.VisibilityProject, Project: project}
		tuples = []graphstore.Tuple{{
			FromKind: graphstore.KindLens, FromID: id, Kind: graphstore.TuplePartOf, ToKind: graphstore.KindIssue, ToID: key,
			At: at, Who: writer, SourceEvent: graphstore.NoEvent, Asserted: true,
		}}
	}
	node.State, node.MadeAt, node.MadeBy, node.SourceEvent = state, at, writer, graphstore.NoEvent
	node.Restates, node.KeepsCause, node.Changed = false, held, held
	if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, tuples); err != nil {
		t.Fatalf("write the lens %s %s: %v", id, state, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the lens's write: %v", err)
	}
}

// A citation of a document whose own address moved to a lens - the pre-mortem conversion moves
// the address of the document it retires to the lens it wrote in its place (api.ConvertPremortem)
// - reaches that lens: the closure of the citing issue holds one member of kind lens at the lens's
// address, with the document's ref_key and the citation's depth and via, and walks no further from
// it, so the document the moved one cites is no member. A citation of a block of the moved
// document is no citation of its address, and still reaches the document itself and walks on from
// it. Once the lens is gone the move reaches nothing that stands (block_moved_to), and the
// citation of the address reaches the document again.
func TestTheClosureAnswersTheLensACitedDocumentMovedTo(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, project := range []string{"CORE", "OPS", "DOC"} {
		createReferenceProject(t, tx, project)
		createReferenceIssue(t, tx, project+"-1", project)
	}
	moved, movedRef := createReferenceArtifact(t, tx, "OPS-1", "OPS", "graph-premortem-md")
	_, citedRef := createReferenceArtifact(t, tx, "", "CORE", "cited-md")
	insertDocumentBlock(t, tx, moved, "b1", movedRef+"#b1")
	insertReference(t, tx, "artifact", moved, "artifact", citedRef)
	addressCitation := createReferenceComment(t, tx, "CORE-1")
	insertReference(t, tx, "comment", addressCitation, "artifact", movedRef)
	blockCitation := createReferenceComment(t, tx, "DOC-1")
	insertReference(t, tx, "comment", blockCitation, "block", movedRef+"#b1")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the fixture: %v", err)
	}
	const lens = "00000000-0000-4000-8000-0000000003a1"
	writeLensNode(t, database, lens, "OPS-1", "OPS", graphstore.StateLive)
	moveTx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the move: %v", err)
	}
	defer moveTx.Rollback(ctx)
	if err := graphstore.Write(ctx, moveTx, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindArtifact, FromID: movedRef, Kind: graphstore.TupleMovedTo, ToKind: graphstore.KindLens, ToID: lens,
		At: time.Now().UTC(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}); err != nil {
		t.Fatalf("move %s to the lens: %v", movedRef, err)
	}
	if err := moveTx.Commit(ctx); err != nil {
		t.Fatalf("commit the move: %v", err)
	}

	closureOf := func(key string) []Member {
		t.Helper()
		members, truncated, err := Closure(ctx, database.Pool, model.Actor{}, key)
		if err != nil || truncated {
			t.Fatalf("read the closure of %s: truncated %t (%v)", key, truncated, err)
		}
		return members
	}
	movedDocument := documentMember(t, database.Pool, movedRef, 1, model.ReferenceVia{Kind: "comment", ID: addressCitation})
	lensMember := movedDocument
	lensMember.Kind, lensMember.Address = graphstore.KindLens, "dispatch://"+graphstore.KindLens+"/"+lens
	cited := documentMember(t, database.Pool, citedRef, 2, model.ReferenceVia{Kind: "artifact", ID: moved})
	byBlock := movedDocument
	byBlock.Via.ID = blockCitation

	if got := closureOf("CORE-1"); !reflect.DeepEqual(got, []Member{lensMember}) {
		t.Errorf("the closure of CORE-1, whose comment cites the moved document's address, is %#v; want the lens alone, %#v", got, lensMember)
	}
	if got := closureOf("DOC-1"); !reflect.DeepEqual(got, []Member{byBlock, cited}) {
		t.Errorf("the closure of DOC-1, whose comment cites a block of the moved document, is %#v; want the document and what it cites, %#v",
			got, []Member{byBlock, cited})
	}

	writeLensNode(t, database, lens, "OPS-1", "OPS", graphstore.StateGone)
	if got := closureOf("CORE-1"); !reflect.DeepEqual(got, []Member{movedDocument, cited}) {
		t.Errorf("once the lens is gone the closure of CORE-1 is %#v; want the document and what it cites, %#v", got, []Member{movedDocument, cited})
	}
}
