package refs

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// writeAuthoredVersion writes version number of the document as a settlement stores it, with
// every writer it read in the order it stores them (docs' actorSlice), under the database's clock
// at the statement, and answers that time.
func writeAuthoredVersion(t *testing.T, tx pgx.Tx, artifact string, number int, markdown string, authors ...model.Actor) time.Time {
	t.Helper()
	encoded, err := json.Marshal(authors)
	if err != nil {
		t.Fatalf("encode the authors of version %d: %v", number, err)
	}
	var at time.Time
	if err := tx.QueryRow(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors, created_at)
		values ($1::uuid, $2, $3, $4, clock_timestamp())
		returning created_at
	`, artifact, number, markdown, encoded).Scan(&at); err != nil {
		t.Fatalf("write version %d: %v", number, err)
	}
	return at
}

// authoredBy is every authored_by tuple the log holds out of the block node address, in its
// sequence order: its far end and the whole actor it records as who.
func authoredBy(t *testing.T, tx pgx.Tx, address string) []graphstore.Tuple {
	t.Helper()
	rows, err := tx.Query(context.Background(), `
		select to_kind, to_id, who, asserted from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3
		order by seq
	`, BlockNodeKind, address, graphstore.TupleAuthoredBy)
	if err != nil {
		t.Fatalf("read the authored_by of %s: %v", address, err)
	}
	defer rows.Close()
	var held []graphstore.Tuple
	for rows.Next() {
		var tuple graphstore.Tuple
		var who []byte
		if err := rows.Scan(&tuple.ToKind, &tuple.ToID, &who, &tuple.Asserted); err != nil {
			t.Fatalf("scan an authored_by of %s: %v", address, err)
		}
		if err := json.Unmarshal(who, &tuple.Who); err != nil {
			t.Fatalf("decode the who of an authored_by of %s: %v", address, err)
		}
		held = append(held, tuple)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the authored_by of %s: %v", address, err)
	}
	return held
}

// requireAuthors fails unless the block node address records exactly authors, by one asserted
// authored_by tuple each in that order, each tuple's far end the author's own node and its who
// the author's whole actor, and unless the node itself is made by the first of them at at, and a
// read of the node (Node) answers all of them, in that order, as its makers at at.
func requireAuthors(t *testing.T, tx pgx.Tx, what, address string, at time.Time, authors ...model.Actor) {
	t.Helper()
	ctx := context.Background()
	held := authoredBy(t, tx, address)
	if len(held) != len(authors) {
		t.Fatalf("%s records %d authored_by tuple(s) %+v, want one for each of the %d authors of the version that first held it",
			what, len(held), held, len(authors))
	}
	for index, author := range authors {
		kind, id, _ := graphstore.AuthorOf(author)
		if tuple := held[index]; tuple.ToKind != kind || tuple.ToID != id || !tuple.Asserted || !reflect.DeepEqual(tuple.Who, author) {
			t.Errorf("%s's authored_by %d is %s/%s (asserted %t) by %+v, want %s/%s by the whole actor %+v",
				what, index+1, tuple.ToKind, tuple.ToID, tuple.Asserted, tuple.Who, kind, id, author)
		}
		var nodes int
		if err := tx.QueryRow(ctx, `select count(*) from graph_nodes where kind = $1 and id = $2`, kind, id).Scan(&nodes); err != nil {
			t.Fatalf("read the node of %s/%s: %v", kind, id, err)
		}
		if nodes != 1 {
			t.Errorf("%s's author %s/%s has %d node(s), want the one its authored_by points at", what, kind, id, nodes)
		}
	}
	var madeBy []byte
	var madeAt time.Time
	if err := tx.QueryRow(ctx, `select made_by, made_at from graph_nodes where kind = $1 and id = $2`, BlockNodeKind, address).Scan(&madeBy, &madeAt); err != nil {
		t.Fatalf("read the node of %s: %v", what, err)
	}
	var maker model.Actor
	if err := json.Unmarshal(madeBy, &maker); err != nil {
		t.Fatalf("decode the made_by of %s: %v", what, err)
	}
	if !reflect.DeepEqual(maker, authors[0]) || !madeAt.Equal(at) {
		t.Errorf("%s's node is made by %+v at %s, want the first author %+v at the version's own time %s", what, maker, madeAt, authors[0], at)
	}
	node, found, err := Node(ctx, tx, model.Actor{}, BlockNodeKind, address, false)
	if err != nil || !found {
		t.Fatalf("read the node of %s: found %t, %v", what, found, err)
	}
	if !reflect.DeepEqual(node.MadeBy, authors) || node.MadeAt == nil || !node.MadeAt.Equal(at) {
		t.Errorf("a read of %s answers it made by %+v at %v, want %+v at %s", what, node.MadeBy, node.MadeAt, authors, at)
	}
}

// A node records its authors by authored_by tuples, one per author of the version that first
// holds it, in that version's own order, each through graphstore.AuthorFacts: its far end the
// author's person or session node and its who the author's whole stored actor (T7 of
// the issue-tree plan). That holds for an authored node of any document, here a
// requirement of an issue's spec, and for every block of an issue room, where the version is that
// of the block's own document and not the newest the room's pass covers. A paragraph of a
// document that is no room records none. A later version by a third writer adds no author.
func TestABlockRecordsEveryAuthorOfItsFirstVersion(t *testing.T) {
	tx := windowFixture(t)
	owner, service := "bob", "system:serviceaccount:legion:worker"
	agent := model.Actor{
		Kind: "session", ID: "worker-9", Owner: &owner, Service: &service,
		Origin: &model.ActorOrigin{Host: "worker-host", Cwd: "/workspace"},
		Act:    &model.ActorAct{Session: "worker-9", Scopes: []string{model.ScopePerson}},
	}
	person := model.Actor{Kind: "user", ID: "alice"}
	third := model.Actor{Kind: "user", ID: "carol"}

	// A document block: the requirement of an issue's spec, first written in a version the agent
	// and the person both wrote, stored in the settlement's order (the session before the user).
	createReferenceProject(t, tx, "AUTH")
	createReferenceIssue(t, tx, "AUTH-1", "AUTH")
	document, documentRef := createReferenceArtifact(t, tx, "AUTH-1", "AUTH", "spec-md")
	first, firstBlocks := documentOf([]part{{"r1", "requirement", "It records who wrote it."}, {"p1", "paragraph", "Prose."}})
	firstAt := writeAuthoredVersion(t, tx, document, 1, first, agent, person)
	indexAndCause(t, tx, document, OneVersion(document, 1, firstAt), first, firstBlocks)
	requireAuthors(t, tx, "the requirement", documentRef+"#r1", firstAt, agent, person)
	if held := authoredBy(t, tx, documentRef+"#p1"); len(held) != 0 {
		t.Errorf("a paragraph of a document no room holds records the authored_by %+v, want none", held)
	}

	// A room block: x of the spec's fragment, first written in the spec's version 1, which the
	// agent and the person both wrote, while the newest version the room's pass covers is the
	// Verification document's, which erin wrote, and the tree's own is dave's.
	spec, _, verification, tree, treeRef := roomFixture(t, tx)
	dave, erin := model.Actor{Kind: "user", ID: "dave"}, model.Actor{Kind: "user", ID: "erin"}
	treeAt := writeAuthoredVersion(t, tx, tree, 1, "", dave)
	specAt := writeAuthoredVersion(t, tx, spec, 1, "Room words.", agent, person)
	verificationAt := writeAuthoredVersion(t, tx, verification, 1, "Check.", erin)
	room, roomBlocks := documentOf([]part{{"x", "paragraph", "Room words."}, {"y", "paragraph", "Check."}})
	roomBlocks[0].Document, roomBlocks[1].Document = spec, verification
	indexAndCause(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 1, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, room, roomBlocks)
	requireAuthors(t, tx, "the room's block x", treeRef+"#x", specAt, agent, person)
	requireAuthors(t, tx, "the room's block y", treeRef+"#y", verificationAt, erin)

	// A later version by a third writer rewrites both blocks and adds no author to either.
	second, secondBlocks := documentOf([]part{{"r1", "requirement", "It records who wrote it first."}, {"p1", "paragraph", "Prose."}})
	secondAt := writeAuthoredVersion(t, tx, document, 2, second, third)
	indexAndCause(t, tx, document, OneVersion(document, 2, secondAt), second, secondBlocks)
	requireAuthors(t, tx, "the requirement after carol's version", documentRef+"#r1", firstAt, agent, person)
	specNext := writeAuthoredVersion(t, tx, spec, 2, "Room words, rewritten.", third)
	rewritten, rewrittenBlocks := documentOf([]part{{"x", "paragraph", "Room words, rewritten."}, {"y", "paragraph", "Check."}})
	rewrittenBlocks[0].Document, rewrittenBlocks[1].Document = spec, verification
	indexAndCause(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 2, At: specNext}, verification: {Number: 1, At: verificationAt},
	}, rewritten, rewrittenBlocks)
	requireAuthors(t, tx, "the room's block x after carol's version", treeRef+"#x", specAt, agent, person)
	if changes := blockChanges(t, tx, tree); changes["x"].version != 2 {
		t.Errorf("x changed at version %d, want the spec's version 2, which carol's rewrite is", changes["x"].version)
	}
}

// unrecordAuthors deletes every authored_by tuple out of the block node address, which leaves the
// node, its row and its texts as a build before this one wrote them, or as the drain of
// T6 of the issue-tree plan left a block it indexed: with no author recorded.
func unrecordAuthors(t *testing.T, tx pgx.Tx, address string) {
	t.Helper()
	if tag, err := tx.Exec(context.Background(), `
		delete from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3
	`, BlockNodeKind, address, graphstore.TupleAuthoredBy); err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("delete the authored_by of %s: %d rows, %v", address, tag.RowsAffected(), err)
	}
}

// A node written before its authors were recorded - by a build before this one, or by the drain
// of T6 of the issue-tree plan, which recorded none - takes them at the next pass that indexes
// its document, which the drain marks every stored document for: one authored_by per author of
// its own first version, in that version's order, and not the authors of the version that pass
// indexes, which a third writer wrote. A requirement of a document and a block of an issue room
// alike, each left untouched by the version the pass indexes.
func TestANodeWrittenBeforeItsAuthorsTakesThemAtTheNextPass(t *testing.T) {
	tx := windowFixture(t)
	agent := model.Actor{Kind: "session", ID: "worker-9"}
	person, third := model.Actor{Kind: "user", ID: "alice"}, model.Actor{Kind: "user", ID: "carol"}

	createReferenceProject(t, tx, "AUTH")
	createReferenceIssue(t, tx, "AUTH-1", "AUTH")
	document, documentRef := createReferenceArtifact(t, tx, "AUTH-1", "AUTH", "spec-md")
	first, firstBlocks := documentOf([]part{{"r1", "requirement", "It records who wrote it."}, {"p1", "paragraph", "Prose."}})
	firstAt := writeAuthoredVersion(t, tx, document, 1, first, agent, person)
	indexAndCause(t, tx, document, OneVersion(document, 1, firstAt), first, firstBlocks)
	unrecordAuthors(t, tx, documentRef+"#r1")
	second, secondBlocks := documentOf([]part{{"r1", "requirement", "It records who wrote it."}, {"p1", "paragraph", "Prose, rewritten."}})
	secondAt := writeAuthoredVersion(t, tx, document, 2, second, third)
	indexAndCause(t, tx, document, OneVersion(document, 2, secondAt), second, secondBlocks)
	requireAuthors(t, tx, "the requirement written before its authors were recorded", documentRef+"#r1", firstAt, agent, person)

	spec, _, verification, tree, treeRef := roomFixture(t, tx)
	treeAt := writeAuthoredVersion(t, tx, tree, 1, "", third)
	specAt := writeAuthoredVersion(t, tx, spec, 1, "Room words.", agent, person)
	verificationAt := writeAuthoredVersion(t, tx, verification, 1, "Check.", third)
	room, roomBlocks := documentOf([]part{{"x", "paragraph", "Room words."}, {"y", "paragraph", "Check."}})
	roomBlocks[0].Document, roomBlocks[1].Document = spec, verification
	indexAndCause(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 1, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, room, roomBlocks)
	unrecordAuthors(t, tx, treeRef+"#x")
	verificationNext := writeAuthoredVersion(t, tx, verification, 2, "Check, rewritten.", third)
	next, nextBlocks := documentOf([]part{{"x", "paragraph", "Room words."}, {"y", "paragraph", "Check, rewritten."}})
	nextBlocks[0].Document, nextBlocks[1].Document = spec, verification
	indexAndCause(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 1, At: specAt}, verification: {Number: 2, At: verificationNext},
	}, next, nextBlocks)
	requireAuthors(t, tx, "the room block written before its authors were recorded", treeRef+"#x", specAt, agent, person)
}

// A plan node's needs naming an issue (T5a's widened needs, T7 of the issue-tree plan) is
// a pointer as any other is, which replaceBlockRefs appends: a needs tuple from the plan node to
// the issue's own node, issue:<KEY>, beside the block_refs row that projects it.
func TestAPlanNodesNeedsNamingAnIssueIsATupleToTheIssue(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('WIND-2', 'WIND', 2, 'The child', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create the issue the plan node needs: %v", err)
	}
	document, documentRef := createReferenceArtifact(t, tx, "WIND-1", "WIND", "spec-md")
	markdown, blocks := documentOf([]part{{"t1", "plan_task", "After the child."}})
	at := writeAuthoredVersion(t, tx, document, 1, markdown, model.Actor{Kind: "user", ID: "alice"})
	pointers := []Pointer{{Block: "t1", BlockType: "plan_task", Kind: "needs",
		Target: text.Ref{Kind: "issue", IssueKey: "WIND-2", ID: "WIND-2"}, Targets: []string{"plan_task"}}}
	_, won, write, _, err := IndexBlocks(ctx, tx, document, OneVersion(document, 1, at), markdown, blocks, nil, pointers, TypedFacts{})
	if err != nil || !won {
		t.Fatalf("index the plan: won %t, %v", won, err)
	}
	if err := graphstore.Caused(ctx, tx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the plan's index stated: %v", err)
	}
	held, err := graphstore.CurrentOut(ctx, tx, BlockNodeKind, documentRef+"#t1", []string{"needs"})
	if err != nil {
		t.Fatalf("read the plan node's needs: %v", err)
	}
	if len(held) != 1 || held[0].ToKind != graphstore.KindIssue || held[0].ToID != "WIND-2" {
		t.Fatalf("the plan node states the needs %+v, want one tuple to issue:WIND-2", held)
	}
	var toKind, toID string
	if err := tx.QueryRow(ctx, `
		select to_kind, to_id from block_refs where from_node = $1 and kind = 'needs'
	`, documentRef+"#t1").Scan(&toKind, &toID); err != nil || toKind != graphstore.KindIssue || toID != "WIND-2" {
		t.Fatalf("the plan node's needs row names %s:%s (%v), want issue:WIND-2", toKind, toID, err)
	}
}

// indexAndCause runs one block index pass in the fixture's transaction and appends what it
// stated, at the sentinel, as a caller with no event of its own does.
func indexAndCause(t *testing.T, tx pgx.Tx, artifact string, versions Versions, markdown string, blocks []model.ArtifactBlock) {
	t.Helper()
	ctx := context.Background()
	_, won, write, _, err := IndexBlocks(ctx, tx, artifact, versions, markdown, blocks, nil, nil, TypedFacts{})
	if err != nil || !won {
		t.Fatalf("index %s: won %t, %v", artifact, won, err)
	}
	if err := graphstore.Caused(ctx, tx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the index of %s stated: %v", artifact, err)
	}
}

// requireChainReachesIssue fails unless the part_of chain the block node address climbs through,
// as the store holds it now, reaches issue's node within ten steps and passes no node twice.
func requireChainReachesIssue(t *testing.T, tx pgx.Tx, address, issue string) {
	t.Helper()
	kind, id := BlockNodeKind, address
	seen := map[string]bool{}
	for range 10 {
		if kind == graphstore.KindIssue && id == issue {
			return
		}
		if seen[kind+":"+id] {
			t.Fatalf("the chain up from %s passes %s:%s twice: a part_of cycle", address, kind, id)
		}
		seen[kind+":"+id] = true
		held, err := graphstore.CurrentOut(context.Background(), tx, kind, id, []string{graphstore.TuplePartOf})
		if err != nil || len(held) != 1 {
			t.Fatalf("%s:%s, on the chain up from %s, stands under %+v (%v), want exactly one part_of", kind, id, address, held, err)
		}
		kind, id = held[0].ToKind, held[0].ToID
	}
	t.Fatalf("the chain up from %s does not reach issue:%s within ten steps", address, issue)
}

// servingRequirement is one requirement of a version a cycle test writes: its id, the block its
// serves names, and the block its parent attribute names ("" for none), each written
// "<document>#<block>".
type servingRequirement struct{ id, serves, parent string }

// servingDocuments is issue WIND-1 with two documents, the spec and the plan, by name: each one's
// artifact id and ref_key.
type servingDocuments map[string]struct{ artifact, refKey string }

// openServingDocuments writes the spec and the plan of WIND-1 in the fixture's transaction.
func openServingDocuments(t *testing.T, tx pgx.Tx) servingDocuments {
	t.Helper()
	spec, specRef := createReferenceArtifact(t, tx, "WIND-1", "WIND", "spec-md")
	var plan, planRef string
	if err := tx.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('WIND-1', 'WIND', 'plan-md', 'plan.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`).Scan(&plan, &planRef); err != nil {
		t.Fatalf("create the plan: %v", err)
	}
	return servingDocuments{"spec": {spec, specRef}, "plan": {plan, planRef}}
}

// address is the node address of the block "<document>#<block>" names.
func (d servingDocuments) address(block string) string {
	name, id, _ := strings.Cut(block, "#")
	return d[name].refKey + "#" + id
}

// ref is the reference a pointer writes to the block "<document>#<block>" names.
func (d servingDocuments) ref(block string) text.Ref {
	name, id, _ := strings.Cut(block, "#")
	return text.Ref{Kind: BlockNodeKind, IssueKey: "WIND-1", ID: strings.TrimPrefix(d[name].refKey, "WIND-1/"), Block: id}
}

// index writes version number of document name holding requirements, by alice, and runs its block
// index pass in the fixture's transaction, with its cause appended at the sentinel.
func (d servingDocuments) index(t *testing.T, tx pgx.Tx, name string, number int, requirements ...servingRequirement) {
	t.Helper()
	ctx := context.Background()
	parts := make([]part, len(requirements))
	var pointers []Pointer
	for position, requirement := range requirements {
		parts[position] = part{requirement.id, "requirement", "It serves another one."}
		pointers = append(pointers, Pointer{Block: requirement.id, BlockType: "requirement", Kind: "serves",
			Target: d.ref(requirement.serves), Targets: []string{"criterion", "requirement"}})
		if requirement.parent != "" {
			pointers = append(pointers, Pointer{Block: requirement.id, BlockType: "requirement", Kind: pmdoc.ParentAttribute, Target: d.ref(requirement.parent)})
		}
	}
	markdown, blocks := documentOf(parts)
	artifact := d[name].artifact
	at := writeAuthoredVersion(t, tx, artifact, number, markdown, model.Actor{Kind: "user", ID: "alice"})
	_, won, write, _, err := IndexBlocks(ctx, tx, artifact, OneVersion(artifact, number, at), markdown, blocks, nil, pointers, TypedFacts{})
	if err != nil || !won {
		t.Fatalf("index version %d of the %s: won %t, %v", number, name, won, err)
	}
	if err := graphstore.Caused(ctx, tx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what version %d of the %s stated: %v", number, name, err)
	}
}

// requireUnder fails unless the block "<document>#<block>" names stands under the block parent
// names, by exactly one current part_of.
func (d servingDocuments) requireUnder(t *testing.T, tx pgx.Tx, block, parent string) {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), tx, BlockNodeKind, d.address(block), []string{graphstore.TuplePartOf})
	if err != nil || len(held) != 1 || held[0].ToKind != BlockNodeKind || held[0].ToID != d.address(parent) {
		t.Fatalf("%s stands under %+v (%v), want %s", block, held, err, parent)
	}
}

// Requirements of two documents of one issue that serve each other leave no part_of cycle,
// whichever way the passes reach them. Two of them: the spec's r1 serves the plan's r2 and r2
// serves r1, indexed in either order. The first placed has no target standing yet and hangs under
// the issue, and the second hangs under it. A version giving the first a parent attribute naming
// the second places it again by that, which would hang it under a block of another document
// standing under it, read off the store's chain of that block, so the pass passes over it. Three of
// them: the spec's a serves the plan's t, the spec's x serves a, and t serves x, so the first
// passes hang a under the issue, x under a and t under x. A version of the spec then gives a a
// parent attribute naming t and x one naming a, so the pass places both again: a under t would
// close a cycle through x, which this pass places under a only after it places a, and which the
// walk from a reaches through t's chain as the store holds it. So each node's chain reaches the
// issue, and no walk meets a cycle.
func TestTwoDocumentsThatServeEachOtherLeaveNoCycle(t *testing.T) {
	for _, first := range []string{"spec", "plan"} {
		t.Run(first+" first", func(t *testing.T) {
			tx := windowFixture(t)
			documents := openServingDocuments(t, tx)
			second := map[string]string{"spec": "plan", "plan": "spec"}[first]
			id := map[string]string{"spec": "r1", "plan": "r2"}
			block := func(name string) string { return name + "#" + id[name] }
			documents.index(t, tx, first, 1, servingRequirement{id: id[first], serves: block(second)})
			documents.index(t, tx, second, 1, servingRequirement{id: id[second], serves: block(first)})
			documents.requireUnder(t, tx, block(second), block(first))
			// The first's parent attribute names the second, which stands under it: a change of
			// the parent attribute, so the pass places the first again.
			documents.index(t, tx, first, 2, servingRequirement{id: id[first], serves: block(second), parent: block(second)})
			for _, name := range []string{first, second} {
				requireChainReachesIssue(t, tx, documents.address(block(name)), "WIND-1")
			}
		})
	}
	t.Run("three nodes across two documents", func(t *testing.T) {
		tx := windowFixture(t)
		documents := openServingDocuments(t, tx)
		documents.index(t, tx, "spec", 1, servingRequirement{id: "a", serves: "plan#t"}, servingRequirement{id: "x", serves: "spec#a"})
		documents.index(t, tx, "plan", 1, servingRequirement{id: "t", serves: "spec#x"})
		documents.requireUnder(t, tx, "spec#x", "spec#a")
		documents.requireUnder(t, tx, "plan#t", "spec#x")
		documents.index(t, tx, "spec", 2,
			servingRequirement{id: "a", serves: "plan#t", parent: "plan#t"}, servingRequirement{id: "x", serves: "spec#a", parent: "spec#a"})
		for _, block := range []string{"spec#a", "spec#x", "plan#t"} {
			requireChainReachesIssue(t, tx, documents.address(block), "WIND-1")
		}
	})
}

// A retarget of an authored node's serves over a store another release indexed the document into
// since this one last stated it (standsAtVersion's stale) moves no part_of, as a retarget over a
// store that is not stale moves none (D4 of the issue-tree plan): check c of the plan
// serves the spec's r1 and stands under it; version 2 retargets it to r2 and c stays under r1; the
// plan's statement then goes stale, as the release before leaves it, and the drain marks it owed,
// and the pass over version 2 again states no part_of of c and leaves it under r1.
func TestARetargetOverAStaleStoreMovesNoAuthoredNode(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	documents := openServingDocuments(t, tx)
	spec, specBlocks := documentOf([]part{{"r1", "requirement", "The first one."}, {"r2", "requirement", "The second one."}})
	specAt := writeAuthoredVersion(t, tx, documents["spec"].artifact, 1, spec, model.Actor{Kind: "user", ID: "alice"})
	indexAndCause(t, tx, documents["spec"].artifact, OneVersion(documents["spec"].artifact, 1, specAt), spec, specBlocks)
	plan := documents["plan"].artifact
	markdown, blocks := documentOf([]part{{"c", "check", "It checks one of them."}})
	serves := func(target string) []Pointer {
		return []Pointer{{Block: "c", BlockType: "check", Kind: "serves", Target: documents.ref(target), Targets: []string{"requirement"}}}
	}
	index := func(what string, number int, at time.Time, target string) {
		t.Helper()
		_, won, write, _, err := IndexBlocks(ctx, tx, plan, OneVersion(plan, number, at), markdown, blocks, nil, serves(target), TypedFacts{})
		if err != nil || !won {
			t.Fatalf("%s: won %t, %v", what, won, err)
		}
		if err := graphstore.Caused(ctx, tx, write, graphstore.NoEvent); err != nil {
			t.Fatalf("append what %s stated: %v", what, err)
		}
	}
	index("the plan's version 1", 1, writeAuthoredVersion(t, tx, plan, 1, markdown, model.Actor{Kind: "user", ID: "bob"}), "spec#r1")
	documents.requireUnder(t, tx, "plan#c", "spec#r1")
	secondAt := writeAuthoredVersion(t, tx, plan, 2, markdown, model.Actor{Kind: "user", ID: "bob"})
	index("the plan's version 2, retargeting c to r2", 2, secondAt, "spec#r2")
	documents.requireUnder(t, tx, "plan#c", "spec#r1")

	if _, err := tx.Exec(ctx, `
		update document_block_versions set store_version = 0, store_owed_at = now() where artifact_id = $1::uuid
	`, plan); err != nil {
		t.Fatalf("leave the plan's statement stale: %v", err)
	}
	var mark int64
	if err := tx.QueryRow(ctx, `select coalesce(max(seq), 0) from graph_tuples`).Scan(&mark); err != nil {
		t.Fatalf("read the end of the tuple log: %v", err)
	}
	index("the pass over version 2 with the plan's statement stale", 2, secondAt, "spec#r2")
	var placed int
	if err := tx.QueryRow(ctx, `
		select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3 and seq > $4
	`, BlockNodeKind, documents.address("plan#c"), graphstore.TuplePartOf, mark).Scan(&placed); err != nil {
		t.Fatalf("read the part_of tuples the stale pass stated of c: %v", err)
	}
	if placed != 0 {
		t.Errorf("the pass over a stale store stated %d part_of tuple(s) of c, want none: a serves retarget moves no node", placed)
	}
	documents.requireUnder(t, tx, "plan#c", "spec#r1")
}

// A requirement's From: line is the last line opening with "From:" outside a code span (R3 and R22
// of the issue-tree spec, fromLine): words quoting the label in code name no source, a
// fenced code block is code too, and a label inside a word is no label.
func TestTheFromLineIsTheLastOneOutsideACodeSpan(t *testing.T) {
	for _, test := range []struct{ name, body, line string }{
		{"a From: line", ":::requirement{#r1}\nIt is sized.\nFrom: dispatch://WIND-1/comment/c1\n:::", "From: dispatch://WIND-1/comment/c1"},
		{"a From: quoted in code before the From: line", "It reads its `From:` line, as dispatch://WIND-1/comment/c2 said.\nFrom: dispatch://WIND-1/comment/c1",
			"From: dispatch://WIND-1/comment/c1"},
		{"the last of two From: lines", "From: dispatch://WIND-1/comment/c2\nFrom: dispatch://WIND-1/comment/c1", "From: dispatch://WIND-1/comment/c1"},
		{"a From: quoted in code alone", "It reads its `From: dispatch://WIND-1/comment/c2` line.", ""},
		{"a From: in a fenced code block", "```\nFrom: dispatch://WIND-1/comment/c2\n```\nIt is sized.", ""},
		{"a code span of two backticks around one", "It reads ``a ` From: c2`` here.\nFrom: c1", "From: c1"},
		{"a backtick no run closes", "It costs `5.\nFrom: c1", "From: c1"},
		{"an escaped backtick opens no span", "It reads \\`From: c1", "From: c1"},
		{"a label inside a word", "WhereFrom: c2", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			start, end, written := fromLine(test.body)
			got := ""
			if written {
				got = test.body[start:end]
			}
			if got != test.line {
				t.Errorf("the From: line of %q is %q (written %t), want %q", test.body, got, written, test.line)
			}
		})
	}
}

// A read of a block node (Node) answers it made by the authors its node records, the who of each
// authored_by it states now in their sequence order, and by the authors of the version that first
// held it only where it states none (T7 of the issue-tree plan). A requirement first seen
// at version 3 of a document whose versions 1 and 2 no block-aware build indexed has no first
// version Dispatch knows, while its pass recorded version 3's author, bob.
func TestABlockIsMadeByTheAuthorsItsNodeRecords(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	bob := model.Actor{Kind: "user", ID: "bob"}
	document, documentRef := createReferenceArtifact(t, tx, "WIND-1", "WIND", "spec-md")
	earlier, _ := documentOf([]part{{"p1", "paragraph", "Earlier words."}})
	writeAuthoredVersion(t, tx, document, 1, earlier, model.Actor{Kind: "user", ID: "alice"})
	writeAuthoredVersion(t, tx, document, 2, earlier, model.Actor{Kind: "user", ID: "alice"})
	markdown, blocks := documentOf([]part{{"p1", "paragraph", "Earlier words."}, {"r1", "requirement", "It is first seen here."}})
	at := writeAuthoredVersion(t, tx, document, 3, markdown, bob)
	indexAndCause(t, tx, document, OneVersion(document, 3, at), markdown, blocks)
	if held := authoredBy(t, tx, documentRef+"#r1"); len(held) != 1 || !reflect.DeepEqual(held[0].Who, bob) {
		t.Fatalf("r1 records the authored_by %+v, want bob's, version 3's author", held)
	}
	node, found, err := Node(ctx, tx, model.Actor{}, BlockNodeKind, documentRef+"#r1", false)
	if err != nil || !found {
		t.Fatalf("read r1: found %t, %v", found, err)
	}
	if !reflect.DeepEqual(node.MadeBy, []model.Actor{bob}) {
		t.Errorf("a read of r1 answers it made by %+v, want %+v, the author its node records", node.MadeBy, []model.Actor{bob})
	}
}

// A block of an issue room moved from one fragment into another keeps the authors of the version
// that first held it, and no read takes them from the document it stands in now (T7 and T12a of
// the issue-tree plan). x is written in the spec's fragment at the spec's version 4,
// which alice wrote, then moved into the Verification document's fragment at that document's
// version 2, which carol wrote: the room's text rows of x record versions 4 and 2 under the tree,
// and the lower is the Verification document's. x records alice, and a read of x answers alice. A
// node of x with no author recorded, as a build before this one left a room node, takes none at
// the next pass, rather than carol, and a read of it answers no maker.
func TestARoomBlockMovedIntoAnotherFragmentKeepsItsFirstAuthors(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	alice, carol, erin := model.Actor{Kind: "user", ID: "alice"}, model.Actor{Kind: "user", ID: "carol"}, model.Actor{Kind: "user", ID: "erin"}
	spec, _, verification, tree, treeRef := roomFixture(t, tx)
	treeAt := writeAuthoredVersion(t, tx, tree, 1, "", erin)
	specAt := writeAuthoredVersion(t, tx, spec, 4, "Moved words.", alice)
	verificationAt := writeAuthoredVersion(t, tx, verification, 1, "Check.", erin)
	first, firstBlocks := documentOf([]part{{"y", "paragraph", "Check."}, {"x", "paragraph", "Moved words."}})
	firstBlocks[0].Document, firstBlocks[1].Document = verification, spec
	indexAndCause(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 4, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, first, firstBlocks)
	specNext := writeAuthoredVersion(t, tx, spec, 5, "", carol)
	verificationNext := writeAuthoredVersion(t, tx, verification, 2, "Check.\n\nMoved words.", carol)
	moved, movedBlocks := documentOf([]part{{"y", "paragraph", "Check."}, {"x", "paragraph", "Moved words."}})
	movedBlocks[0].Document, movedBlocks[1].Document = verification, verification
	indexAndCause(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 5, At: specNext}, verification: {Number: 2, At: verificationNext},
	}, moved, movedBlocks)

	x := treeRef + "#x"
	madeBy := func(what string, want []model.Actor) {
		t.Helper()
		node, found, err := Node(ctx, tx, model.Actor{}, BlockNodeKind, x, false)
		if err != nil || !found {
			t.Fatalf("read x %s: found %t, %v", what, found, err)
		}
		if !reflect.DeepEqual(node.MadeBy, want) {
			t.Errorf("a read of x %s answers it made by %+v, want %+v", what, node.MadeBy, want)
		}
	}
	if held := authoredBy(t, tx, x); len(held) != 1 || !reflect.DeepEqual(held[0].Who, alice) {
		t.Fatalf("the moved x records the authored_by %+v, want alice's, the spec's version 4's author", held)
	}
	madeBy("once moved", []model.Actor{alice})

	unrecordAuthors(t, tx, x)
	verificationLast := writeAuthoredVersion(t, tx, verification, 3, "Check, rewritten.\n\nMoved words.", erin)
	last, lastBlocks := documentOf([]part{{"y", "paragraph", "Check, rewritten."}, {"x", "paragraph", "Moved words."}})
	lastBlocks[0].Document, lastBlocks[1].Document = verification, verification
	indexAndCause(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 5, At: specNext}, verification: {Number: 3, At: verificationLast},
	}, last, lastBlocks)
	if held := authoredBy(t, tx, x); len(held) != 0 {
		t.Errorf("the moved x with no author recorded takes the authored_by %+v at the next pass, want none: no row says which document's version first held it", held)
	}
	madeBy("with no author recorded", []model.Actor{})
}
