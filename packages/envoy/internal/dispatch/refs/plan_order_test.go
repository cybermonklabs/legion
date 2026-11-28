package refs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// orderAuthor writes every version, value and relation the ordered read's fixtures hold, and
// reads them back.
var orderAuthor = privateAuthor

// orderBlock is one block of the plan an ordered-read fixture writes for one issue: its id, its
// type, what its needs names - a plan node as "<issue>#<block id>", an issue by its key - the
// requirement of its own document its serves names ("" for none), and its size ("" for none).
type orderBlock struct {
	id, typ string
	needs   []string
	serves  string
	size    string
}

// orderTask is a plan node of size size that needs needs.
func orderTask(id, size string, needs ...string) orderBlock {
	return orderBlock{id: id, typ: planTaskType, needs: needs, size: size}
}

// orderRequirement is a requirement, which a plan node may serve and so stand under.
func orderRequirement(id string) orderBlock { return orderBlock{id: id, typ: requirementType} }

// orderFixture is one transaction over the test's database, rolled back when its test ends, with
// the issues it creates and the document each one's plan is written in.
type orderFixture struct {
	t         *testing.T
	tx        pgx.Tx
	documents map[string]*orderDocument
}

// orderDocument is where one issue's plan is written: the artifact its blocks are keyed by and
// that artifact's ref_key - the issue's spec, or the tree of the issue's room for an issue
// converted into it - the spec whose fragment the blocks stand in where that artifact is the
// tree, and how many versions of it the fixture wrote.
type orderDocument struct {
	artifact, refKey, fragment string
	versions                   int
}

// newOrderFixture begins the fixture's transaction on database and creates projects.
func newOrderFixture(t *testing.T, database *store.Store, projects ...string) *orderFixture {
	t.Helper()
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the fixture transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	for _, project := range projects {
		createReferenceProject(t, tx, project)
	}
	return &orderFixture{t: t, tx: tx, documents: map[string]*orderDocument{}}
}

// issue creates issue key under parent ("" for a root) with its spec.
func (f *orderFixture) issue(key, parent string) {
	f.t.Helper()
	project, number, _ := strings.Cut(key, "-")
	var parentKey *string
	if parent != "" {
		parentKey = &parent
	}
	if _, err := f.tx.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key)
		values ($1, $2, $3::int, 'An issue of the tree', '{"kind":"user","id":"alice"}', 'U', $4)
	`, key, project, number, parentKey); err != nil {
		f.t.Fatalf("create issue %s: %v", key, err)
	}
	spec, refKey := createReferenceArtifact(f.t, f.tx, key, project, "spec")
	f.documents[key] = &orderDocument{artifact: spec, refKey: refKey}
}

// convert moves issue key's spec into the issue's room: the issue's tree artifact with its
// issue_trees row and the spec's document_rooms row naming it, so the plan written after keys
// its blocks by the tree, `<KEY>/tree#<id>`.
func (f *orderFixture) convert(key string) {
	f.t.Helper()
	project, _, _ := strings.Cut(key, "-")
	spec := f.documents[key].artifact
	tree, treeRef := insertIssueRoom(f.t, f.tx, key, project, spec)
	f.documents[key] = &orderDocument{artifact: tree, refKey: treeRef, fragment: spec}
}

// address is the store address of block id of issue key's plan.
func (f *orderFixture) address(key, id string) string { return f.documents[key].refKey + "#" + id }

// task, issueStart and issueDone name the nodes of the answer: plan node id of issue key, and
// issue key's start and done.
func (f *orderFixture) task(key, id string) OrderNode {
	return OrderNode{Kind: BlockNodeKind, ID: f.address(key, id)}
}

func issueStart(key string) OrderNode {
	return OrderNode{Kind: graphstore.KindIssue, ID: key, Half: HalfStart}
}

func issueDone(key string) OrderNode {
	return OrderNode{Kind: graphstore.KindIssue, ID: key, Half: HalfDone}
}

// issueReference is an issue's reference as a plan node's needs writes it.
func issueReference(key string) string { return "dispatch://" + key }

// reference is what a needs entry or a serves of issue key's plan names, as its attribute writes
// it: "<issue>#<block id>" a plan node ("#<block id>" of key's own plan), a bare key an issue.
func (f *orderFixture) reference(key, named string) string {
	issue, block, isBlock := strings.Cut(named, "#")
	if !isBlock {
		return issueReference(named)
	}
	if issue == "" {
		issue = key
	}
	written, ok := text.FormatBlockRef(text.Ref{Kind: BlockNodeKind, IssueKey: issue, ID: strings.TrimPrefix(f.documents[issue].refKey, issue+"/"), Block: block})
	if !ok {
		f.t.Fatalf("write a reference to block %s of %s", block, issue)
	}
	return written
}

// plan writes the next version of issue key's plan, by orderAuthor: blocks as the markdown of
// typed blocks, each opening line holding its serves, its needs and its size, read and rendered as
// a version write renders it, so each block carries the attributes its line writes and each
// pointer is one its attributes hold (pmdoc.BlockPointers). It runs the block index pass over
// that render and appends what the pass stated at the sentinel.
func (f *orderFixture) plan(key string, blocks ...orderBlock) {
	f.t.Helper()
	document := f.documents[key]
	document.versions++
	var source strings.Builder
	for index, block := range blocks {
		if index > 0 {
			source.WriteString("\n")
		}
		attributes := "#" + block.id
		if block.serves != "" {
			attributes += ` serves="` + f.reference(key, "#"+block.serves) + `"`
		}
		if len(block.needs) > 0 {
			needs := make([]string, len(block.needs))
			for at, named := range block.needs {
				needs[at] = f.reference(key, named)
			}
			attributes += ` needs="` + strings.Join(needs, " ") + `"`
		}
		if block.size != "" {
			attributes += ` size="` + block.size + `"`
		}
		source.WriteString(":::" + block.typ + "{" + attributes + "}\nThe block " + block.id + ".\n:::\n")
	}
	tree, err := pmdoc.Parse(source.String())
	if err != nil {
		f.t.Fatalf("read the plan of %s: %v\n%s", key, err, source.String())
	}
	markdown, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		f.t.Fatalf("render the plan of %s: %v", key, err)
	}
	written := make([]model.ArtifactBlock, len(offsets))
	for index, offset := range offsets {
		written[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To, Attributes: offset.Attributes}
	}
	pointers := PointersOf(tree)
	versions := Versions{}
	if document.fragment == "" {
		at := writeAuthoredVersion(f.t, f.tx, document.artifact, document.versions, markdown, orderAuthor)
		versions = OneVersion(document.artifact, document.versions, at)
	} else {
		treeAt := writeAuthoredVersion(f.t, f.tx, document.artifact, document.versions, "", orderAuthor)
		specAt := writeAuthoredVersion(f.t, f.tx, document.fragment, document.versions, markdown, orderAuthor)
		versions[document.artifact] = Version{Number: document.versions, At: treeAt}
		versions[document.fragment] = Version{Number: document.versions, At: specAt}
		for index := range written {
			written[index].Document = document.fragment
		}
	}
	indexAndCause(f.t, f.tx, document.artifact, versions, markdown, written, pointers...)
}

// needs states that issue key waits on each of targets, as the issue route's writer states it
// (graphstore.IssueNeeds).
func (f *orderFixture) needs(key string, targets ...string) {
	f.t.Helper()
	tuples := make([]graphstore.Tuple, len(targets))
	for index, target := range targets {
		tuples[index] = graphstore.IssueNeeds(key, target, true, orderAuthor, time.Now())
	}
	if err := graphstore.Write(context.Background(), f.tx, nil, tuples); err != nil {
		f.t.Fatalf("state what %s needs: %v", key, err)
	}
}

// overtake marks the node of kind and id overtaken, as a request's write of a node's state does
// (api's writeNodeState): the node the store holds, or, for an issue the store holds no node of,
// the issue's node.
func (f *orderFixture) overtake(kind, id string) {
	f.t.Helper()
	project, _, _ := strings.Cut(id, "-")
	f.markOvertaken(kind, id, graphstore.IssueNode(id, project, orderAuthor, time.Now()))
}

// overtakeItem marks the ask, comment or message of kind and id overtaken, as overtake marks a
// node: the node the store holds, or, where the fixture wrote the item's row alone, a node of that
// kind in project, as the item's writer states one.
func (f *orderFixture) overtakeItem(kind, id, project string) {
	f.t.Helper()
	f.markOvertaken(kind, id, graphstore.Node{Kind: kind, ID: id, Supertag: kind, Visibility: graphstore.VisibilityProject, Project: project})
}

// markOvertaken writes the node of kind and id overtaken: the node the store holds, else made.
func (f *orderFixture) markOvertaken(kind, id string, made graphstore.Node) {
	f.t.Helper()
	ctx := context.Background()
	node, held, err := graphstore.ReadNode(ctx, f.tx, kind, id)
	if err != nil {
		f.t.Fatalf("read %s %s: %v", kind, id, err)
	}
	if !held {
		node = made
	}
	node.State, node.Restates, node.KeepsCause = graphstore.StateOvertaken, false, true
	node.MadeAt, node.MadeBy, node.SourceEvent = time.Now(), orderAuthor, graphstore.NoEvent
	if err := graphstore.Write(ctx, f.tx, []graphstore.Node{node}, nil); err != nil {
		f.t.Fatalf("mark %s %s overtaken: %v", kind, id, err)
	}
}

// place moves the block moved from the place the index pass gave it under the node of kind toKind
// and id toID, as a move writes it: its newest part_of retracted, the new one asserted.
func (f *orderFixture) place(moved, toKind, toID string) {
	f.t.Helper()
	ctx := context.Background()
	var fromKind, fromID string
	if err := f.tx.QueryRow(ctx, `
		select to_kind, to_id from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind <> $4
		order by seq desc limit 1
	`, BlockNodeKind, moved, graphstore.TuplePartOf, graphstore.KindValue).Scan(&fromKind, &fromID); err != nil {
		f.t.Fatalf("read the part_of of %s: %v", moved, err)
	}
	at := time.Now()
	placement := func(toKind, toID string, asserted bool) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: BlockNodeKind, FromID: moved, Kind: graphstore.TuplePartOf, ToKind: toKind, ToID: toID,
			At: at, Who: orderAuthor, SourceEvent: graphstore.NoEvent, Asserted: asserted,
		}
	}
	if err := store.RunWrite(ctx, f.tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nil, []graphstore.Tuple{placement(fromKind, fromID, false), placement(toKind, toID, true)})
		return nil
	}); err != nil {
		f.t.Fatalf("move %s under %s %s: %v", moved, toKind, toID, err)
	}
}

// read is the ordered read of issue key for who, read twice to hold it to one answer, and held to
// the same answer over the tuples and over the head projection (readWithMarks).
func (f *orderFixture) read(who model.Actor, key string) TreeOrder {
	f.t.Helper()
	answer, err := PlanOrder(context.Background(), f.tx, who, key)
	if err != nil {
		f.t.Fatalf("read the order of %s as %s: %v", key, who.ID, err)
	}
	again, err := PlanOrder(context.Background(), f.tx, who, key)
	if err != nil {
		f.t.Fatalf("read the order of %s as %s again: %v", key, who.ID, err)
	}
	if !reflect.DeepEqual(answer, again) {
		f.t.Fatalf("two reads of the order of %s answer\n%+v\nand\n%+v\nwant one answer", key, answer, again)
	}
	walked, headed := f.readWithMarks(who, key, false), f.readWithMarks(who, key, true)
	if !reflect.DeepEqual(walked, answer) {
		f.t.Fatalf("the order of %s as %s over the tuples answers\n%+v\nwant the read's\n%+v", key, who.ID, walked, answer)
	}
	if !reflect.DeepEqual(headed, walked) {
		f.t.Errorf("the order of %s as %s over the head rows answers\n%+v\nwant the order over the tuples\n%+v", key, who.ID, headed, walked)
	}
	return answer
}

// readWithMarks is the ordered read of issue key for who in a savepoint of the fixture's
// transaction, which it rolls back: with no issue marked, which walks the tuples (planTreeWalk);
// or, where marked is true, with the head rows of every node the store holds written again
// (graphstore.WriteHeads) and every issue marked whole, which reads the rows (planTreeHeads), and
// is held there to the same answer from the rows key's plan view reads (PlanOrderOfRows).
func (f *orderFixture) readWithMarks(who model.Actor, key string, marked bool) TreeOrder {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.tx.Begin(ctx)
	if err != nil {
		f.t.Fatalf("open a savepoint for the order of %s: %v", key, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `delete from issue_head_marks`); err != nil {
		f.t.Fatalf("take the head marks away: %v", err)
	}
	if marked {
		rows, err := tx.Query(ctx, `
			select kind, id from graph_nodes
			union select from_kind, from_id from graph_tuples
			union select '`+ItemComment.BlockKind()+`', node_id from comment_blocks
			union select '`+ItemMessage.BlockKind()+`', node_id from message_blocks
			union select '`+ItemAsk.BlockKind()+`', node_id from ask_blocks`)
		if err != nil {
			f.t.Fatalf("list the nodes of the store: %v", err)
		}
		nodes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphstore.NodeKey, error) {
			var node graphstore.NodeKey
			return node, row.Scan(&node.Kind, &node.ID)
		})
		if err != nil {
			f.t.Fatalf("list the nodes of the store: %v", err)
		}
		if err := graphstore.WriteHeads(ctx, tx, nodes); err != nil {
			f.t.Fatalf("write the head rows of the store's nodes: %v", err)
		}
		if _, err := tx.Exec(ctx, `insert into issue_head_marks (issue_key, filled_at) select key, now() from issues`); err != nil {
			f.t.Fatalf("mark every issue whole: %v", err)
		}
	}
	answer, err := PlanOrder(ctx, tx, who, key)
	if err != nil {
		f.t.Fatalf("read the order of %s as %s with marks %t: %v", key, who.ID, marked, err)
	}
	if !marked {
		return answer
	}
	heads, held, err := HeadViewRows(ctx, tx, who, key, HeadView{Types: graphstore.QueryTakes(graphstore.QueryPlan)})
	if err != nil || !held {
		f.t.Fatalf("read the rows of the plan view of %s as %s: marked %t, %v", key, who.ID, held, err)
	}
	ofRows, err := PlanOrderOfRows(ctx, tx, who, key, heads)
	if err != nil {
		f.t.Fatalf("read the order of %s as %s from the rows of its plan view: %v", key, who.ID, err)
	}
	if !reflect.DeepEqual(ofRows, answer) {
		f.t.Errorf("the order of %s as %s from the rows of its plan view answers\n%+v\nwant the order over the head rows\n%+v", key, who.ID, ofRows, answer)
	}
	return answer
}

// namesOf is the name of each of nodes, in order.
func namesOf(nodes []OrderedNode) []OrderNode {
	names := make([]OrderNode, len(nodes))
	for index, node := range nodes {
		names[index] = node.OrderNode
	}
	return names
}

// positionsOf is each placed node's position in order.
func positionsOf(order []OrderNode) map[OrderNode]int {
	positions := make(map[OrderNode]int, len(order))
	for position, node := range order {
		positions[node] = position
	}
	return positions
}

// requireOrderStands fails unless answer stands as one order: every node of its order once, in
// no cycle and not overtaken, and each after every node it needs that has a position.
func requireOrderStands(t *testing.T, answer TreeOrder) {
	t.Helper()
	positions := positionsOf(namesOf(answer.Order))
	if len(positions) != len(answer.Order) {
		t.Fatalf("the order names a node twice: %v", namesOf(answer.Order))
	}
	for _, cycle := range answer.Cycles {
		for _, node := range cycle {
			if _, placed := positions[node.OrderNode]; placed {
				t.Errorf("%+v stands in the cycle %v and at a position of the order: the order hides the cycle", node.OrderNode, namesOf(cycle))
			}
		}
	}
	for _, node := range answer.Overtaken {
		if _, placed := positions[node.OrderNode]; placed {
			t.Errorf("%+v is overtaken and stands at a position of the order", node.OrderNode)
		}
	}
	for position, node := range answer.Order {
		for _, need := range node.Needs {
			if before, placed := positions[need]; placed && before >= position {
				t.Errorf("%+v stands at %d, not after %+v, which it needs, at %d", node.OrderNode, position, need, before)
			}
		}
	}
}

// orderNamed is the node of answer named name, wherever the answer holds it.
func orderNamed(t *testing.T, answer TreeOrder, name OrderNode) OrderedNode {
	t.Helper()
	held := slices.Clone(answer.Order)
	for _, cycle := range answer.Cycles {
		held = append(held, cycle...)
	}
	held = append(held, answer.Overtaken...)
	for _, node := range held {
		if node.OrderNode == name {
			return node
		}
	}
	t.Fatalf("the answer holds no %+v", name)
	return OrderedNode{}
}

// One read answers the order the whole tree of an issue runs in (R11 of the issue-tree spec, and
// c-ordered-read of its Verification document). A parent issue, CORE-1, holds two
// children, CORE-2 and CORE-3, the second converted so its room's tree keys its plan, and each
// holds plan nodes; the plan nodes need plan nodes of their own issue and of the issue above,
// CORE-1's p2 needs the issue CORE-2, and CORE-3 needs CORE-2 and OTHR-1, an issue of another
// project. Each plan is markdown, every need and size written in its plan node's opening line
// (`size="2h"`), indexed by the block index pass. The answer holds every issue as its start and
// its done and every plan node, each once, after every node it needs, with the size its line
// writes beside it in whole minutes, none where it writes none, and its issue the one its part_of
// chain reaches: a1 stands under a requirement of CORE-2. A parent holding a child is no cycle,
// and its done sits after each child's done; a plan node sits after its issue's start and before
// its issue's done; a sibling waiting on a sibling keeps its order; and p2, which needs CORE-2,
// comes back after every plan node of CORE-2 and after CORE-2's done. OTHR-1 is a fixed node
// with no position. A diamond, p3 and p4 needing p1 and p5 needing both, is no cycle. A node
// marked overtaken, a node under an overtaken requirement and an overtaken issue with its plan
// get no position and are reported as such. A plan node reserved for its author is no node of
// another reader's order. A plan node of CORE-1 that needs an issue outside CORE-1's tree is
// refused at the write. The check's control - a read that takes the child's start half for a plan
// node's need of an issue - brings p2 back before CORE-2's plan nodes and before CORE-2's done,
// and the test names it: a wrong whole order fails the test without ending it, so the placement
// of p2 after every plan node of CORE-2 and after CORE-2's done is read, and named where it does
// not hold. Then an edit of CORE-2's plan gives a5 a need of p2, which makes a cycle of p2,
// CORE-2's done and a5, reported with those three, while every other node keeps its position; the
// same edit takes a2's size out of its line, and the read answers a2 with no size.
//
// A second tree seeds every cycle the check names: two plan nodes of ACME-1 that need each
// other; three nodes across two issues, ACME-1's q3 needing the issue ACME-2 and ACME-2's d1
// needing q3; one from the issue ACME-3, which needs its sibling ACME-4, through ACME-4's f1 to
// q4 of ACME-1, which needs ACME-3, and back; and q5, which needs itself. Each is reported with
// its nodes and none stands in the order, while q6, which needs q1 of the first cycle, and every
// node in no cycle keeps a position. Under the check's own control - the read walking the plan
// nodes of one issue alone, as it did before - the read answers an order and reports neither the
// cycle through ACME-2 nor the one from the child ACME-3 through ACME-4's f1 to q4 of ACME-1 and
// back: the test names each cycle the read does not report and the position the order gives each
// of its nodes the read still holds.
func TestTheOrderedReadWalksTheWholeTreeAndReportsEveryCycle(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)

	t.Run("the whole tree in one order", func(t *testing.T) {
		f := newOrderFixture(t, database, "CORE", "OTHR")
		f.issue("CORE-1", "")
		f.issue("CORE-2", "CORE-1")
		f.issue("CORE-3", "CORE-1")
		f.issue("CORE-4", "")
		f.issue("CORE-5", "CORE-2")
		f.issue("OTHR-1", "")
		f.convert("CORE-3")
		f.plan("CORE-1",
			orderTask("p1", "1h"), orderTask("p2", "", "CORE-2"),
			orderTask("p3", "2h", "CORE-1#p1"), orderTask("p4", "2h", "CORE-1#p1"), orderTask("p5", "30m", "CORE-1#p3", "CORE-1#p4"),
			orderTask("p6", "1h"))
		aPlan := []orderBlock{
			orderRequirement("r1"), {id: "a1", typ: planTaskType, serves: "r1", size: "1h30m"}, orderTask("a2", "45m", "CORE-2#a1"),
			orderRequirement("r2"), {id: "a3", typ: planTaskType, serves: "r2", size: "1h"},
		}
		f.plan("CORE-2", aPlan...)
		f.plan("CORE-3", orderTask("b1", "3h", "CORE-1#p1"))
		f.plan("CORE-5", orderTask("e1", "1h"))
		f.needs("CORE-3", "CORE-2", "OTHR-1")
		f.overtake(BlockNodeKind, f.address("CORE-1", "p6"))
		f.overtake(BlockNodeKind, f.address("CORE-2", "r2"))
		f.overtake(graphstore.KindIssue, "CORE-5")

		answer := f.read(orderAuthor, "CORE-1")
		requireOrderStands(t, answer)
		p := func(id string) OrderNode { return f.task("CORE-1", id) }
		a := func(id string) OrderNode { return f.task("CORE-2", id) }
		b1 := f.task("CORE-3", "b1")
		// v245's R11: the walk "starts from the nodes that no node needs, in the base order": p2,
		// the first, takes CORE-2 whole before it, which it needs, and p1's chain comes after.
		want := []OrderNode{
			issueStart("CORE-1"), issueStart("CORE-2"), a("a1"), a("a2"), issueDone("CORE-2"), p("p2"),
			p("p1"), p("p3"), p("p4"), p("p5"),
			issueStart("CORE-3"), b1, issueDone("CORE-3"), issueDone("CORE-1"),
		}
		if got := namesOf(answer.Order); !reflect.DeepEqual(got, want) {
			t.Errorf("the tree of CORE-1 runs in the order\n%v\nwant every issue's two halves and every plan node once, in R11's needs-first walk:\n%v", got, want)
		}
		if len(answer.Cycles) != 0 {
			t.Errorf("the tree with a diamond and no cycle reports the cycles %v, want none", answer.Cycles)
		}
		positions := positionsOf(namesOf(answer.Order))
		after := func(later, earlier OrderNode, why string) {
			t.Helper()
			if positions[later] <= positions[earlier] {
				t.Errorf("%+v stands at %d and %+v at %d: %s", later, positions[later], earlier, positions[earlier], why)
			}
		}
		for _, child := range []string{"CORE-2", "CORE-3"} {
			after(issueDone("CORE-1"), issueDone(child), "a parent's done sits after each child's done")
			after(issueStart(child), issueStart("CORE-1"), "a child starts after its parent starts")
		}
		for _, node := range answer.Order {
			if node.Kind != BlockNodeKind {
				after(issueDone(node.ID), issueStart(node.ID), "an issue is done after it starts")
				continue
			}
			after(node.OrderNode, issueStart(node.Issue), "a plan node sits after its issue's start")
			after(issueDone(node.Issue), node.OrderNode, "its issue's done sits after the plan node")
		}
		after(issueStart("CORE-3"), issueDone("CORE-2"), "a sibling waiting on a sibling keeps its order")
		for _, earlier := range []OrderNode{a("a1"), a("a2"), issueDone("CORE-2")} {
			after(p("p2"), earlier, "a parent's plan node that needs its child comes back after every plan node of that child and after the child's done")
		}
		after(b1, p("p1"), "a child's plan node that needs a plan node of its parent comes back after it")

		sizes := map[OrderNode]string{
			p("p1"): "60", p("p2"): "", p("p3"): "120", p("p4"): "120", p("p5"): "30", a("a1"): "90", a("a2"): "45", b1: "180",
			issueStart("CORE-1"): "", issueDone("CORE-1"): "",
		}
		for name, size := range sizes {
			if got := orderNamed(t, answer, name).Size; got != size {
				t.Errorf("%+v comes back with the size %q, want %q", name, got, size)
			}
		}
		for name, issue := range map[OrderNode]string{a("a1"): "CORE-2", b1: "CORE-3", p("p1"): "CORE-1", issueDone("CORE-2"): "CORE-2"} {
			if got := orderNamed(t, answer, name).Issue; got != issue {
				t.Errorf("%+v stands under the issue %q, want %q, the issue its part_of chain reaches", name, got, issue)
			}
		}
		if !strings.HasPrefix(b1.ID, "CORE-3/tree#") {
			t.Errorf("b1 of CORE-3's room is keyed %s, want a block of the room's tree", b1.ID)
		}
		for name, needs := range map[OrderNode][]OrderNode{
			p("p5"):              {issueStart("CORE-1"), p("p3"), p("p4")},
			p("p2"):              {issueStart("CORE-1"), issueDone("CORE-2")},
			issueStart("CORE-3"): {issueStart("CORE-1"), issueDone("CORE-2"), issueDone("OTHR-1")},
			issueDone("CORE-2"):  {issueStart("CORE-2"), a("a1"), a("a2")},
			issueDone("CORE-1"):  {issueStart("CORE-1"), p("p1"), p("p2"), p("p3"), p("p4"), p("p5"), issueDone("CORE-2"), issueDone("CORE-3")},
		} {
			if got := orderNamed(t, answer, name).Needs; !reflect.DeepEqual(got, needs) {
				t.Errorf("%+v comes back needing %v, want %v", name, got, needs)
			}
		}
		if want := []OrderNode{issueDone("OTHR-1")}; !reflect.DeepEqual(answer.Fixed, want) {
			t.Errorf("the fixed nodes are %v, want %v: an issue of another project that CORE-3 needs has no position", answer.Fixed, want)
		}
		wantOvertaken := []OrderNode{p("p6"), a("a3"), issueStart("CORE-5"), issueDone("CORE-5"), f.task("CORE-5", "e1")}
		if got := namesOf(answer.Overtaken); !reflect.DeepEqual(got, wantOvertaken) {
			t.Errorf("the overtaken nodes are %v, want %v: p6 overtaken, a3 under an overtaken requirement, CORE-5 overtaken and e1 under it", got, wantOvertaken)
		}

		// A plan node of CORE-1 may need an issue under CORE-1, and one outside its tree - a root
		// of the same project, an issue of another project - is refused at the write.
		writable, _ := pmdoc.Element{Kind: pmdoc.ItemElement, Block: planTaskType, Attribute: "needs", Name: graphstore.KindIssue}.Versions()
		for _, write := range []struct{ target, refusal string }{{"CORE-2", ""}, {"CORE-4", "not under CORE-1"}, {"OTHR-1", "not under CORE-1"}} {
			tree, err := pmdoc.Parse(":::plan_task{#t9 needs=\"" + issueReference(write.target) + "\"}\nAfter it.\n:::\n")
			if err != nil {
				t.Fatalf("parse a plan node needing %s: %v", write.target, err)
			}
			err = pmdoc.CheckBlockPointers(tree, nil, func(targets []text.Ref) (pmdoc.BlockWriting, map[string]pmdoc.BlockTarget, error) {
				writing, found, err := BlockTargets(ctx, f.tx, f.documents["CORE-1"].artifact, storedAddress(t, f.tx, f.documents["CORE-1"].artifact), nil, targets)
				writing.Creatable = func() (int, error) { return writable.CreatableSince, nil }
				return writing, found, err
			})
			switch {
			case write.refusal == "" && err != nil:
				t.Errorf("a plan node of CORE-1 needing %s, an issue under it, is refused: %v", write.target, err)
			case write.refusal != "" && (!errors.Is(err, pmdoc.ErrBlockPointer) || !strings.Contains(err.Error(), write.refusal)):
				t.Errorf("a plan node of CORE-1 needing %s, outside its tree, is answered %v, want it refused naming %q", write.target, err, write.refusal)
			}
		}

		// A plan node reserved for its author is no node of another reader's order, and no other
		// node needs it there; its author reads the order as before.
		reserveForAuthor(t, f.tx, BlockNodeKind, f.address("CORE-1", "p4"))
		if got := f.read(orderAuthor, "CORE-1"); !reflect.DeepEqual(got, answer) {
			t.Errorf("its author reads the order with p4 reserved as\n%+v\nwant the order read before\n%+v", got, answer)
		}
		other := f.read(privateReader, "CORE-1")
		requireOrderStands(t, other)
		if slices.Contains(namesOf(other.Order), p("p4")) || slices.Contains(orderNamed(t, other, p("p5")).Needs, p("p4")) {
			t.Errorf("another reader's order holds p4, reserved for its author: %v, p5 needing %v", namesOf(other.Order), orderNamed(t, other, p("p5")).Needs)
		}

		// a5 of CORE-2 needs p2, which needs CORE-2: a cycle of p2, CORE-2's done and a5. The same
		// edit takes a2's size out of its opening line, which leaves a2 no size value.
		edited := slices.Clone(aPlan)
		edited[2] = orderTask("a2", "", "CORE-2#a1")
		f.plan("CORE-2", append(edited, orderTask("a5", "1h", "CORE-1#p2"))...)
		cycled := f.read(orderAuthor, "CORE-1")
		requireOrderStands(t, cycled)
		wantCycle := [][]OrderNode{{p("p2"), issueDone("CORE-2"), a("a5")}}
		if got := cycleNames(cycled); !reflect.DeepEqual(got, wantCycle) {
			t.Fatalf("with a5 needing p2 the read reports the cycles %v, want %v", got, wantCycle)
		}
		wantKept := []OrderNode{
			issueStart("CORE-1"), p("p1"), p("p3"), p("p4"), p("p5"),
			issueStart("CORE-2"), a("a1"), a("a2"), issueStart("CORE-3"), b1, issueDone("CORE-3"), issueDone("CORE-1"),
		}
		if got := namesOf(cycled.Order); !reflect.DeepEqual(got, wantKept) {
			t.Errorf("with the cycle the order is\n%v\nwant every node outside it kept:\n%v", got, wantKept)
		}
		if size := orderNamed(t, cycled, a("a2")).Size; size != "" {
			t.Errorf("a2, whose size an edit took out of its opening line, comes back with the size %q, want none", size)
		}
		if value, held, err := graphstore.CurrentValue(ctx, f.tx, BlockNodeKind, a("a2").ID, AttributePlanSize); err != nil || held {
			t.Errorf("a2 holds the size value %q (held %t, %v) after the edit that took it out, want none", value, held, err)
		}
		if size := orderNamed(t, cycled, a("a1")).Size; size != "90" {
			t.Errorf("a1, whose size the edit kept, written 1h30m, comes back with the size %q, want 90 minutes", size)
		}
	})

	t.Run("every cycle reported", func(t *testing.T) {
		f := newOrderFixture(t, database, "ACME")
		f.issue("ACME-1", "")
		for _, child := range []string{"ACME-2", "ACME-3", "ACME-4"} {
			f.issue(child, "ACME-1")
		}
		f.plan("ACME-1",
			orderTask("q1", "1h", "ACME-1#q2"), orderTask("q2", "1h", "ACME-1#q1"),
			orderTask("q3", "1h", "ACME-2"), orderTask("q4", "1h", "ACME-3"), orderTask("q5", "1h", "ACME-1#q5"),
			orderTask("q6", "1h", "ACME-1#q1"), orderTask("q7", "1h"))
		f.plan("ACME-2", orderTask("d1", "1h", "ACME-1#q3"), orderTask("d2", "1h"))
		f.plan("ACME-4", orderTask("f1", "1h", "ACME-1#q4"))
		f.needs("ACME-3", "ACME-4")

		answer := f.read(orderAuthor, "ACME-1")
		requireOrderStands(t, answer)
		q := func(id string) OrderNode { return f.task("ACME-1", id) }
		wantCycles := [][]OrderNode{
			{q("q1"), q("q2")},
			{q("q3"), issueDone("ACME-2"), f.task("ACME-2", "d1")},
			{q("q4"), issueStart("ACME-3"), issueDone("ACME-3"), issueDone("ACME-4"), f.task("ACME-4", "f1")},
			{q("q5")},
		}
		// A cycle the read does not report is one it answers an order for: each of its nodes the
		// order places, at its position, is named before the cycles are compared whole.
		positions := positionsOf(namesOf(answer.Order))
		got := cycleNames(answer)
		for _, cycle := range wantCycles {
			if slices.ContainsFunc(got, func(reported []OrderNode) bool { return reflect.DeepEqual(reported, cycle) }) {
				continue
			}
			var placed []string
			for _, node := range cycle {
				if position, held := positions[node]; held {
					placed = append(placed, fmt.Sprintf("%+v at %d", node, position))
				}
			}
			t.Errorf("the read answers an order and reports no cycle %v: it places %d of its nodes, %s", cycle, len(placed), strings.Join(placed, ", "))
		}
		if !reflect.DeepEqual(got, wantCycles) {
			t.Fatalf("the read reports the cycles\n%v\nwant each with its nodes:\n%v", got, wantCycles)
		}
		// v245's R11: the walk "starts from the nodes that no node needs, in the base order": the
		// cycle through q3 comes first and places ACME-2's start and d2, which ACME-2's done waits on.
		want := []OrderNode{issueStart("ACME-1"), issueStart("ACME-2"), f.task("ACME-2", "d2"), issueStart("ACME-4"), q("q6"), q("q7"), issueDone("ACME-1")}
		if got := namesOf(answer.Order); !reflect.DeepEqual(got, want) {
			t.Errorf("with the cycles the order is\n%v\nwant every node in no cycle kept at a position:\n%v", got, want)
		}
		for _, cycle := range answer.Cycles {
			for _, node := range cycle {
				if node.Kind == BlockNodeKind && node.Size != "60" {
					t.Errorf("%+v of a cycle comes back with the size %q, want its own, 60 minutes", node.OrderNode, node.Size)
				}
			}
		}
	})
}

// The ordered read keeps each chain of steps together (R11 of the issue-tree spec, at its
// version 245, and c-ordered-read of its Verification document): the position is a needs-first
// depth-first walk from the nodes that no node needs, in the base order, which places each node's
// unplaced needs before it, so each need stands just before the first node that needs it.
// CHAIN-1's plan writes the tasks a, b and c in that order, and c needs a: the read answers b, a,
// c, as the issue page's Plan chart draws them. CHAIN-2 writes two tasks, p1 and p2, and its child
// CHAIN-3 two, c1 and c2, where p1 needs c2: the base order is CHAIN-2's own tasks in their
// document's order, then CHAIN-3 with its tasks in theirs, and the read answers c2, p1, p2, c1, so
// c2 stands just before p1. CHAIN-4's task p needs its child CHAIN-5, whose tasks are a, b and c, c
// needing a: the read answers CHAIN-5 whole before p, its tasks in the order CHAIN-5's own read
// answers them, b, a, c, since a done half places first the steps that no node needs. In each tree
// an issue's start stands before its plan nodes and its children, and its done after them. The
// control, a read that places the nodes as Kahn's algorithm frees them with a tie broken by id,
// answers a, b, c for CHAIN-1, and the test fails, printing the order it read and the order it
// wants.
func TestTheOrderedReadKeepsEachChainOfStepsTogether(t *testing.T) {
	database := storetest.Open(t)
	f := newOrderFixture(t, database, "CHAIN")
	f.issue("CHAIN-1", "")
	f.issue("CHAIN-2", "")
	f.issue("CHAIN-3", "CHAIN-2")
	f.issue("CHAIN-4", "")
	f.issue("CHAIN-5", "CHAIN-4")
	f.plan("CHAIN-1", orderTask("a", ""), orderTask("b", ""), orderTask("c", "", "CHAIN-1#a"))
	f.plan("CHAIN-3", orderTask("c1", ""), orderTask("c2", ""))
	f.plan("CHAIN-2", orderTask("p1", "", "CHAIN-3#c2"), orderTask("p2", ""))
	f.plan("CHAIN-5", orderTask("a", ""), orderTask("b", ""), orderTask("c", "", "CHAIN-5#a"))
	f.plan("CHAIN-4", orderTask("p", "", "CHAIN-5"))

	task := f.task
	for _, tree := range []struct {
		root string
		want []OrderNode
		why  string
	}{
		{"CHAIN-1", []OrderNode{issueStart("CHAIN-1"), task("CHAIN-1", "b"), task("CHAIN-1", "a"), task("CHAIN-1", "c"), issueDone("CHAIN-1")},
			"b, which no node needs, first, then a just before c, the first node that needs it"},
		{"CHAIN-2", []OrderNode{
			issueStart("CHAIN-2"), issueStart("CHAIN-3"), task("CHAIN-3", "c2"), task("CHAIN-2", "p1"), task("CHAIN-2", "p2"),
			task("CHAIN-3", "c1"), issueDone("CHAIN-3"), issueDone("CHAIN-2"),
		}, "CHAIN-2's own tasks first in their document's order, CHAIN-3's after in theirs, and c2 just before p1, the first node that needs it"},
		{"CHAIN-4", []OrderNode{
			issueStart("CHAIN-4"), issueStart("CHAIN-5"), task("CHAIN-5", "b"), task("CHAIN-5", "a"), task("CHAIN-5", "c"),
			issueDone("CHAIN-5"), task("CHAIN-4", "p"), issueDone("CHAIN-4"),
		}, "CHAIN-5 whole just before p, which needs it, with its tasks in the order CHAIN-5's own read answers them"},
		{"CHAIN-5", []OrderNode{issueStart("CHAIN-5"), task("CHAIN-5", "b"), task("CHAIN-5", "a"), task("CHAIN-5", "c"), issueDone("CHAIN-5")},
			"the three tasks in the order they stand in inside CHAIN-4's read"},
	} {
		answer := f.read(orderAuthor, tree.root)
		requireOrderStands(t, answer)
		if len(answer.Cycles) != 0 {
			t.Errorf("the tree of %s reports the cycles %v, want none", tree.root, cycleNames(answer))
		}
		if got := namesOf(answer.Order); !reflect.DeepEqual(got, tree.want) {
			t.Errorf("the ordered read of %s answers the order\n%v\nwant R11's needs-first walk, %s:\n%v", tree.root, got, tree.why, tree.want)
		}
	}
}

// TestTheOrderedReadReachesAPlanNodeUnderTheConversation: a plan node that stands under a block
// of a comment of the issue - as a criterion a person writes in that exchange stands, and every
// node under it - takes its place in the issue's order, as one under the issue's own node does.
// The comment holds no part_of into the issue (the walk of the tree reads the conversation apart
// and walks down from its blocks), so the walk down the issue's part_of tuples alone reaches no
// node under it, and the plan view answered no position for such a node. Before readPlanTree
// starts from the blocks of the issue's asks, comments and messages too, TALK-1's order holds a
// alone, and the test fails, printing it.
func TestTheOrderedReadReachesAPlanNodeUnderTheConversation(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	f := newOrderFixture(t, database, "TALK")
	f.issue("TALK-1", "")
	f.plan("TALK-1", orderTask("a", ""), orderTask("b", "", "TALK-1#a"))
	comment := createReferenceComment(t, f.tx, "TALK-1")
	block := "comment/" + comment + "#b1"
	if _, err := f.tx.Exec(ctx, `
		insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt,
		                            made_at, changed_revision, changed_at)
		values ($1::uuid, 'b1', $2, 0, 'paragraph', 'sha', 'The thread the plan comes out of.', now(), 1, now())
	`, comment, block); err != nil {
		t.Fatalf("index the comment's block: %v", err)
	}
	// b moves from the place the index pass gave it to the comment's block, as a move writes it.
	f.place(f.address("TALK-1", "b"), ItemComment.BlockKind(), block)

	answer := f.read(orderAuthor, "TALK-1")
	requireOrderStands(t, answer)
	want := []OrderNode{issueStart("TALK-1"), f.task("TALK-1", "a"), f.task("TALK-1", "b"), issueDone("TALK-1")}
	if got := namesOf(answer.Order); !reflect.DeepEqual(got, want) {
		t.Errorf("the ordered read of TALK-1 answers the order\n%v\nwant b, which stands under the comment's block %s, at its place after a, which it needs:\n%v", got, block, want)
	}
}

// TestTheOrderedReadGivesNoPositionUnderAClosedConversationNode: a node whose state is overtaken
// counts as closed with its subtree (R7 of the issue-tree spec, r-reparent-and-state), an ask, a
// comment or a message among them, and the plan read gives every plan node under one no position
// (c-ordered-read), as it gives one under an overtaken block none. CLOSE-1's plan writes the tasks
// a to g, and each but a moves under a node of the issue's conversation, as a move or a plan
// task's `parent` attribute places it: b under a block of a comment marked overtaken; c under a
// block of an overtaken message; d under a live comment itself, its part_of naming the comment; e
// under an overtaken comment itself; f under a block of a reply to the overtaken comment, which the
// tree hangs under that comment; and g under a block of a live reply to a live comment. The read
// answers a, d and g in their places and b, c, e and f overtaken, over the tuples and over the head
// rows alike (orderFixture.read). The faults it holds the read to: one that reads no item's own
// state answers b and c at positions; one that starts the conversation at the items' blocks alone
// answers d and e in neither the order nor the overtaken; and one that hangs every item under the
// issue answers f at a position. Each fails the test, which prints the order and the overtaken
// nodes it read.
func TestTheOrderedReadGivesNoPositionUnderAClosedConversationNode(t *testing.T) {
	database := storetest.Open(t)
	f := newOrderFixture(t, database, "CLOSE")
	const key = "CLOSE-1"
	f.issue(key, "")
	f.plan(key, orderTask("a", ""), orderTask("b", ""), orderTask("c", ""), orderTask("d", ""), orderTask("e", ""), orderTask("f", ""), orderTask("g", ""))
	at := time.Now()
	const person = `{"kind":"user","id":"alice"}`
	closed, closedBlock := conversationItem(t, f.tx, "comments", key, person, "The thread a step came out of.", at, "")
	message, messageBlock := conversationItem(t, f.tx, "messages", key, person, "The message a step came out of.", at, "")
	live, _ := conversationItem(t, f.tx, "comments", key, person, "The thread a step stands under.", at, "")
	overtaken, _ := conversationItem(t, f.tx, "comments", key, person, "The thread another step stands under.", at, "")
	_, replyBlock := conversationItem(t, f.tx, "comments", key, person, "A reply in the closed thread.", at, closed)
	_, liveReplyBlock := conversationItem(t, f.tx, "comments", key, person, "A reply in the live thread.", at, live)
	f.place(f.address(key, "b"), ItemComment.BlockKind(), closedBlock)
	f.place(f.address(key, "c"), ItemMessage.BlockKind(), messageBlock)
	f.place(f.address(key, "d"), graphstore.KindComment, live)
	f.place(f.address(key, "e"), graphstore.KindComment, overtaken)
	f.place(f.address(key, "f"), ItemComment.BlockKind(), replyBlock)
	f.place(f.address(key, "g"), ItemComment.BlockKind(), liveReplyBlock)
	f.overtakeItem(graphstore.KindComment, closed, "CLOSE")
	f.overtakeItem(graphstore.KindMessage, message, "CLOSE")
	f.overtakeItem(graphstore.KindComment, overtaken, "CLOSE")

	answer := f.read(orderAuthor, key)
	requireOrderStands(t, answer)
	order := namesOf(answer.Order)
	closedNodes := slices.SortedFunc(slices.Values(namesOf(answer.Overtaken)), compareOrderNodes)
	wantOrder := []OrderNode{issueStart(key), f.task(key, "a"), f.task(key, "d"), f.task(key, "g"), issueDone(key)}
	wantClosed := slices.SortedFunc(slices.Values([]OrderNode{f.task(key, "b"), f.task(key, "c"), f.task(key, "e"), f.task(key, "f")}), compareOrderNodes)
	if !reflect.DeepEqual(order, wantOrder) || !reflect.DeepEqual(closedNodes, wantClosed) {
		t.Errorf("the ordered read of %s answers the order\n%v\nand overtaken\n%v\nwant a, d (under a live comment) and g (under a live reply) in the order:\n%v\n"+
			"and b, c, e and f, under an overtaken comment, message or reply's thread, overtaken:\n%v", key, order, closedNodes, wantOrder, wantClosed)
	}
}

// cycleNames is the name of each node of each cycle answer reports.
func cycleNames(answer TreeOrder) [][]OrderNode {
	cycles := make([][]OrderNode, len(answer.Cycles))
	for index, cycle := range answer.Cycles {
		cycles[index] = namesOf(cycle)
	}
	return cycles
}
