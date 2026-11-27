package refs

import (
	"context"
	"errors"
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
	ctx := context.Background()
	node, held, err := graphstore.ReadNode(ctx, f.tx, kind, id)
	if err != nil {
		f.t.Fatalf("read %s %s: %v", kind, id, err)
	}
	if !held {
		project, _, _ := strings.Cut(id, "-")
		node = graphstore.IssueNode(id, project, orderAuthor, time.Now())
	}
	node.State, node.Restates, node.KeepsCause = graphstore.StateOvertaken, false, true
	node.MadeAt, node.MadeBy, node.SourceEvent = time.Now(), orderAuthor, graphstore.NoEvent
	if err := graphstore.Write(ctx, f.tx, []graphstore.Node{node}, nil); err != nil {
		f.t.Fatalf("mark %s %s overtaken: %v", kind, id, err)
	}
}

// read is the ordered read of issue key for who, read twice to hold it to one answer.
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

// orderOf orders nodes as the ordered read orders the tree it read (waitGraph), each waiting on
// what needs answers of it, over the base order base, and answers the order and the cycles by
// name.
func orderOf(nodes []OrderedNode, base []OrderNode, needs func(OrderedNode) []OrderNode) ([]OrderNode, [][]OrderNode) {
	graph := newWaitGraph(namesOf(nodes), base)
	for _, node := range nodes {
		for _, need := range needs(node) {
			graph.wait(node.OrderNode, need)
		}
	}
	placed, looped := graph.order()
	order := make([]OrderNode, len(placed))
	for index, node := range placed {
		order[index] = graph.nodes[node]
	}
	cycles := make([][]OrderNode, len(looped))
	for index, cycle := range looped {
		for _, node := range cycle {
			cycles[index] = append(cycles[index], graph.nodes[node])
		}
	}
	return order, cycles
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
// refused at the write. The control: the order a read gets that takes CORE-2's start half for
// p2's need of CORE-2 brings p2 back before CORE-2's plan nodes, which the test names, so it is
// the done half that holds p2 after them. Then an edit of CORE-2's plan gives a5 a need of p2,
// which makes a cycle of p2, CORE-2's done and a5, reported with those three, while every other
// node keeps its position; the same edit takes a2's size out of its line, and the read answers a2
// with no size.
//
// A second tree seeds every cycle the check names: two plan nodes of ACME-1 that need each
// other; three nodes across two issues, ACME-1's q3 needing the issue ACME-2 and ACME-2's d1
// needing q3; one from the issue ACME-3, which needs its sibling ACME-4, through ACME-4's f1 to
// q4 of ACME-1, which needs ACME-3, and back; and q5, which needs itself. Each is reported with
// its nodes and none stands in the order, while q6, which needs q1 of the first cycle, and every
// node in no cycle keeps a position. The check's own control - the read walking the plan nodes of
// one issue alone, as it did before - finds neither the cycle through ACME-2 nor the one through
// ACME-3 and ACME-4, which the test names.
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
			t.Fatalf("the tree of CORE-1 runs in the order\n%v\nwant every issue's two halves and every plan node once, in R11's needs-first walk:\n%v", got, want)
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

		// The control: with CORE-2's start half taken for p2's need of the issue CORE-2, p2 comes
		// back before CORE-2's plan nodes, so it is the done half that holds it after them. The
		// walk runs over the read's base order: each issue's start, its plan nodes, its children
		// and its done.
		base := []OrderNode{
			issueStart("CORE-1"), p("p1"), p("p2"), p("p3"), p("p4"), p("p5"),
			issueStart("CORE-2"), a("a1"), a("a2"), issueDone("CORE-2"),
			issueStart("CORE-3"), b1, issueDone("CORE-3"), issueDone("CORE-1"),
		}
		startHalf, _ := orderOf(answer.Order, base, func(node OrderedNode) []OrderNode {
			if node.Kind != BlockNodeKind {
				return node.Needs
			}
			needs := slices.Clone(node.Needs)
			for index, need := range needs {
				if need.Kind == graphstore.KindIssue {
					needs[index].Half = HalfStart
				}
			}
			return needs
		})
		taken := positionsOf(startHalf)
		if taken[p("p2")] >= taken[a("a1")] || taken[p("p2")] >= taken[a("a2")] {
			t.Fatalf("with CORE-2's start half taken for p2's need, p2 stands at %d, a1 at %d and a2 at %d: the control does not move p2 before CORE-2's plan nodes",
				taken[p("p2")], taken[a("a1")], taken[a("a2")])
		}
		t.Logf("the control: with CORE-2's start half taken for the need, the parent's plan node p2 comes back at %d, before the child's plan nodes a1 at %d and a2 at %d; the read answers it at %d, after a1 at %d, a2 at %d and CORE-2's done at %d",
			taken[p("p2")], taken[a("a1")], taken[a("a2")], positions[p("p2")], positions[a("a1")], positions[a("a2")], positions[issueDone("CORE-2")])

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
		if got := cycleNames(answer); !reflect.DeepEqual(got, wantCycles) {
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

		// The check's control: a read walking the plan nodes of ACME-1 alone, as the read did
		// before it walked the tree, finds the two cycles inside ACME-1 and neither of the two
		// that run through its children.
		var own []OrderedNode
		for _, node := range append(slices.Clone(answer.Order), slices.Concat(answer.Cycles...)...) {
			if node.Issue == "ACME-1" {
				own = append(own, node)
			}
		}
		_, alone := orderOf(own, nil, func(node OrderedNode) []OrderNode { return node.Needs })
		if wantAlone := [][]OrderNode{{q("q1"), q("q2")}, {q("q5")}}; !reflect.DeepEqual(alone, wantAlone) {
			t.Fatalf("the walk of ACME-1's plan nodes alone reports the cycles %v, want only the two inside ACME-1, %v", alone, wantAlone)
		}
		t.Logf("the control: the walk of ACME-1's plan nodes alone reports the cycles %v and answers q3 and q4 an order, where the walk of the tree reports %v",
			alone, wantCycles)
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
	// b moves from the place the index pass gave it to the comment's block, as a move writes it:
	// the old part_of retracted, the new one asserted.
	moved := f.address("TALK-1", "b")
	var fromKind, fromID string
	if err := f.tx.QueryRow(ctx, `
		select to_kind, to_id from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind <> $4
		order by seq desc limit 1
	`, BlockNodeKind, moved, graphstore.TuplePartOf, graphstore.KindValue).Scan(&fromKind, &fromID); err != nil {
		t.Fatalf("read the part_of of %s: %v", moved, err)
	}
	at := time.Now()
	placement := func(toKind, toID string, asserted bool) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: BlockNodeKind, FromID: moved, Kind: graphstore.TuplePartOf, ToKind: toKind, ToID: toID,
			At: at, Who: orderAuthor, SourceEvent: graphstore.NoEvent, Asserted: asserted,
		}
	}
	if err := store.RunWrite(ctx, f.tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nil, []graphstore.Tuple{placement(fromKind, fromID, false), placement(ItemComment.BlockKind(), block, true)})
		return nil
	}); err != nil {
		t.Fatalf("move %s under %s: %v", moved, block, err)
	}

	answer := f.read(orderAuthor, "TALK-1")
	requireOrderStands(t, answer)
	want := []OrderNode{issueStart("TALK-1"), f.task("TALK-1", "a"), f.task("TALK-1", "b"), issueDone("TALK-1")}
	if got := namesOf(answer.Order); !reflect.DeepEqual(got, want) {
		t.Errorf("the ordered read of TALK-1 answers the order\n%v\nwant b, which stands under the comment's block %s, at its place after a, which it needs:\n%v", got, block, want)
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
