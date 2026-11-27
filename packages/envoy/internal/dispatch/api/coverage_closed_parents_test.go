package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// TestTheCoverageReadClosesARequirementUnderAnOvertakenNodeOfAnyKind holds the coverage route to
// the rule the tree read is drawn by: a node whose state is overtaken counts as closed with its
// subtree, so the coverage read lists no gap for a requirement under it (R7 of INFRA-1253's spec,
// dispatch://INFRA-1253/spec#b-r-reparent-and-state), whatever the kind of that node. A
// requirement's parent attribute may name a comment or a message of its own issue, or a block of
// one (refs' authoredPlaces.candidates); a move may place it under a lens; and the issue's own node
// stands above every requirement of its walk.
//
// On an issue born converted and on one that has not converted, alice writes the comments C, B and
// L and the message M. The spec holds r0, and then rc, whose parent attribute names C; rb, whose
// parent attribute names B's first block; rm, whose names M; rl, whose names L; and rn, which a
// move places under a trade-off lens alice invokes on r0. No check serves any of them. C, B, M and
// the lens are set overtaken through the state route, and L stays live.
//
// The walk places each requirement under the node its case names. The tree read with no view, the
// page's source, answers rc, rb, rm, rl and rn under those nodes, C, B, M and the lens overtaken
// and L live, so its reader finds rc, rb, rm and rn closed. The coverage route answers rc, rb, rm
// and rn closed, with no gap, and r0 and rl gaps, the two lines the issue's gaps hold; each of the
// six holds its own state, live. Once the issue's own node is set overtaken, which the tree read
// answers on its root, every requirement reads closed and the issue lists no gap.
//
// A requirement's parent attribute may also name a comment of an issue above (candidates): on a
// child the release before created, which is no view, ca's names a comment of its parent, set
// overtaken. The child's walk takes ca in its second part under that comment, the parent's tree
// read answers ca under it, marked with the child, and the comment overtaken, and the child's
// coverage answers ca closed and c0, its other requirement, its one gap.
//
// The control: the read as it stood before, which took the state of the block nodes of the walk
// and of the asks of its conversation alone, reads nothing overtaken above rc, rb, rm, rn and ca,
// and then above any requirement once the issue's node is overtaken, so it answers each of them a
// gap and this test fails naming each.
func TestTheCoverageReadClosesARequirementUnderAnOvertakenNodeOfAnyKind(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	for _, seeded := range []struct {
		name, project string
		unconverted   bool
	}{{"converted", "CLPC", false}, {"unconverted", "CLPU", true}} {
		t.Run(seeded.name, func(t *testing.T) {
			createReferenceAPIProject(t, handler, seeded.project)
			key := createDigestIssue(t, handler, database, seeded.project, "Requirements under overtaken nodes",
				":::requirement{#r0}\nStands under the issue.\n:::\n", seeded.unconverted).Key
			closing, _ := personComment(t, handler, database, key, "The board must page.")
			blocked, blockRef := personComment(t, handler, database, key, "The list must sort.\n\nBy age.")
			standing, _ := personComment(t, handler, database, key, "The list keeps its order.")
			message := decodeBody[writtenRecord](t, created(t, "alice's message on "+key, dispatchRequest(t, handler, http.MethodPost,
				"/api/v1/issues/"+key+"/messages", map[string]any{"body": "The board must hold every urgent ask."}, "alice"))).ID
			comment := func(id string) string { return "dispatch://" + key + "/comment/" + id }
			editSpec(t, handler, key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::requirement{#rc parent=\"" + comment(closing) + "\"}\n" +
				"Under an overtaken comment.\n:::\n\n" +
				":::requirement{#rb parent=\"" + blockRef + "\"}\nUnder a block of an overtaken comment.\n:::\n\n" +
				":::requirement{#rm parent=\"dispatch://" + key + "/message/" + message + "\"}\nUnder an overtaken message.\n:::\n\n" +
				":::requirement{#rl parent=\"" + comment(standing) + "\"}\nUnder a live comment.\n:::\n\n" +
				":::requirement{#rn}\nUnder an overtaken lens.\n:::\n"}})
			lens := decodeBody[lensInvoked](t, created(t, "alice's lens on r0 of "+key, dispatchRequest(t, handler, http.MethodPost,
				"/api/v1/issues/"+key+"/lenses", map[string]any{"on": refs.BlockRef("dispatch://"+key+"/spec", "r0"), "kind": "trade-off"}, "alice")))
			spec := readDecisionDocument(t, database, key, "spec")
			for _, address := range []string{graphstore.KindComment + "/" + closing, graphstore.KindComment + "/" + blocked,
				graphstore.KindMessage + "/" + message, lens.Address} {
				succeeded(t, "set "+address+" overtaken", writeStateAs(t, handler, key, address, graphstore.StateOvertaken, "alice"))
			}
			movePartOf(t, database, key, refs.BlockNodeKind, spec.address("rn"), graphstore.KindLens, lens.ID, false)

			parsed, parsedOK := text.ParseRef(blockRef, "")
			block, targeted := refs.TargetOf(parsed)
			if !parsedOK || !targeted {
				t.Fatalf("B's first block %s names no node", blockRef)
			}
			cases := []struct {
				requirement string
				parent      refs.Target
				state       string
			}{
				{"rc", refs.Target{Kind: graphstore.KindComment, ID: closing}, graphstore.StateOvertaken},
				{"rb", block, graphstore.StateLive},
				{"rm", refs.Target{Kind: graphstore.KindMessage, ID: message}, graphstore.StateOvertaken},
				{"rl", refs.Target{Kind: graphstore.KindComment, ID: standing}, graphstore.StateLive},
				{"rn", refs.Target{Kind: graphstore.KindLens, ID: lens.ID}, graphstore.StateOvertaken},
			}
			walk, err := docs.WalkIssueTree(context.Background(), database.Pool, key, refs.TreeCut{})
			if err != nil {
				t.Fatalf("walk the tree of %s: %v", key, err)
			}
			walked := make(map[refs.Target]refs.Target, len(walk.Nodes))
			for _, node := range walk.Nodes {
				walked[refs.Target{Kind: node.Kind, ID: node.ID}] = node.Parent
			}
			for _, want := range cases {
				if parent := walked[refs.Target{Kind: refs.BlockNodeKind, ID: spec.address(want.requirement)}]; parent != want.parent {
					t.Fatalf("the walk of %s places %s under %+v, want %+v: this case proves nothing", key, want.requirement, parent, want.parent)
				}
			}
			if parent := walked[block]; parent != (refs.Target{Kind: graphstore.KindComment, ID: blocked}) {
				t.Fatalf("the walk of %s places B's block %s under %+v, want B %s: this case proves nothing", key, block.ID, parent, blocked)
			}

			page := treeRead(t, handler, key, "", "alice", false)
			for _, want := range cases {
				node, _ := page.node(t, refs.BlockNodeKind, spec.address(want.requirement))
				if node.Parent == nil || node.Parent.Kind != want.parent.Kind || node.Parent.ID != want.parent.ID {
					t.Fatalf("the tree read of %s places %s under %+v, want %s %s: this case proves nothing", key, want.requirement, node.Parent, want.parent.Kind, want.parent.ID)
				}
				if parent, _ := page.node(t, want.parent.Kind, want.parent.ID); parent.State != want.state {
					t.Fatalf("the tree read of %s answers %s's parent %s %s at %q, want %q: this case proves nothing", key, want.requirement, want.parent.Kind, want.parent.ID, parent.State, want.state)
				}
			}
			if node, _ := page.node(t, graphstore.KindComment, blocked); node.State != graphstore.StateOvertaken {
				t.Fatalf("the tree read of %s answers B %s at %q, want overtaken, the state its reader closes rb by: this case proves nothing", key, blocked, node.State)
			}

			coverage := readCoverage(t, handler, key)
			if len(coverage.Requirements) != 6 {
				t.Fatalf("the coverage route of %s lists the requirements %+v, want r0, rc, rb, rm, rl and rn", key, coverage.Requirements)
			}
			for _, want := range []struct {
				block, coverage string
				gaps            []string
			}{
				{"r0", "gap", []string{noCheckReason}},
				{"rc", "closed", nil},
				{"rb", "closed", nil},
				{"rm", "closed", nil},
				{"rl", "gap", []string{noCheckReason}},
				{"rn", "closed", nil},
			} {
				requirement := requirementOf(t, coverage, want.block)
				if requirement.State != graphstore.StateLive || requirement.Coverage != want.coverage || !slices.Equal(requirement.Gaps, want.gaps) {
					t.Errorf("the coverage route of %s answers %s as %+v, want state live, %s and the gaps %q", key, want.block, requirement, want.coverage, want.gaps)
				}
			}
			gaps := slices.Sorted(slices.Values(coverage.Gaps))
			if want := []string{noCheckGap("r0"), noCheckGap("rl")}; !slices.Equal(gaps, want) {
				t.Errorf("the coverage route of %s lists the gaps %q, want %q: rc, rb, rm and rn stand under an overtaken node", key, coverage.Gaps, want)
			}

			issueNode := graphstore.KindIssue + "/" + key
			succeeded(t, "set "+issueNode+" overtaken", writeStateAs(t, handler, key, issueNode, graphstore.StateOvertaken, "alice"))
			if root, _ := treeRead(t, handler, key, "", "alice", false).node(t, graphstore.KindIssue, key); root.State != graphstore.StateOvertaken {
				t.Fatalf("the tree read of %s answers its own node at %q, want overtaken: this case proves nothing", key, root.State)
			}
			coverage = readCoverage(t, handler, key)
			for _, requirement := range coverage.Requirements {
				if requirement.Coverage != "closed" || len(requirement.Gaps) != 0 {
					t.Errorf("the coverage route of %s, whose own node is overtaken, answers %s as %+v, want closed with no gap", key, requirement.BlockID, requirement)
				}
			}
			if len(coverage.Gaps) != 0 {
				t.Errorf("the coverage route of %s, whose own node is overtaken, lists the gaps %q, want none", key, coverage.Gaps)
			}
		})
	}

	t.Run("under a comment of the issue above", func(t *testing.T) {
		createReferenceAPIProject(t, handler, "CLPA")
		parent := createDigestIssue(t, handler, database, "CLPA", "The parent", ":::requirement{#p0}\nThe parent's own.\n:::\n", false).Key
		said, _ := personComment(t, handler, database, parent, "The child must page too.")
		child := createDigestChild(t, handler, database, "CLPA", "The child", ":::requirement{#c0}\nThe child's own.\n:::\n", parent, true).Key
		editSpec(t, handler, child, []map[string]any{{"op": "insert", "after": "end",
			"markdown": ":::requirement{#ca parent=\"dispatch://" + parent + "/comment/" + said + "\"}\nUnder the parent's overtaken comment.\n:::\n"}})
		succeeded(t, "set the parent's comment overtaken", writeStateAs(t, handler, parent, graphstore.KindComment+"/"+said, graphstore.StateOvertaken, "alice"))
		ca := readDecisionDocument(t, database, child, "spec").address("ca")
		comment := refs.Target{Kind: graphstore.KindComment, ID: said}

		walk, err := docs.WalkIssueTree(context.Background(), database.Pool, child, refs.TreeCut{})
		if err != nil {
			t.Fatalf("walk the tree of %s: %v", child, err)
		}
		if at := slices.IndexFunc(walk.Nodes, func(node docs.TreeNode) bool { return node.Kind == refs.BlockNodeKind && node.ID == ca }); at < 0 ||
			!walk.Nodes[at].Above || walk.Nodes[at].Parent != comment {
			t.Fatalf("the walk of %s takes ca %s at %d, want it in the second part under the parent's comment %s: this case proves nothing", child, ca, at, said)
		}
		page := treeRead(t, handler, parent, "", "alice", false)
		if node, _ := page.node(t, refs.BlockNodeKind, ca); node.Parent == nil || node.Parent.Kind != comment.Kind || node.Parent.ID != comment.ID || node.Issue != child {
			t.Fatalf("the tree read of %s answers ca as %+v, want it under its comment %s, marked with %s: this case proves nothing", parent, node, said, child)
		}
		if node, _ := page.node(t, comment.Kind, comment.ID); node.State != graphstore.StateOvertaken {
			t.Fatalf("the tree read of %s answers its comment %s at %q, want overtaken: this case proves nothing", parent, said, node.State)
		}

		coverage := readCoverage(t, handler, child)
		if requirement := requirementOf(t, coverage, "ca"); requirement.Coverage != "closed" || len(requirement.Gaps) != 0 {
			t.Errorf("the coverage route of %s answers ca, under the overtaken comment %s of %s, as %+v, want closed with no gap", child, said, parent, requirement)
		}
		if want := []string{noCheckGap("c0")}; !slices.Equal(coverage.Gaps, want) {
			t.Errorf("the coverage route of %s lists the gaps %q, want %q: ca stands under an overtaken comment", child, coverage.Gaps, want)
		}
	})
}

// TestTheCoverageReadClimbsTheTreeOfTheIssueAbove holds a child's coverage read to the tree of the
// issue above as that issue's tree read draws it. A node whose state is overtaken "counts as closed
// with its subtree" (R7 of INFRA-1253's spec, dispatch://INFRA-1253/spec#b-r-reparent-and-state).
// A child's requirement whose parent attribute names a comment or a message of an issue above, or
// a block of one, stands in that issue's tree read under that node, marked with the child (R8's
// second part). So it is closed where that node, or a node above it in the tree of the issue above,
// up to that issue's own node, is overtaken as the reader may see each (INFRA-1253's architect, the
// issue's comment 53c8e843 of 2026-11-27).
//
// In an overtaken subtree: P, born converted, holds p0, p1 and p2, and p1 is moved under p2. alice
// writes on P the comment H on the text of p1's paragraph, R, a reply to H, the message M and N, a
// reply to M. C, a child of P that the release before created, so no view, holds c0 and, by their
// parent attributes, ch under H, cr under R's first block and cn under N. P's walk places H under
// the paragraph, the paragraph under p1, p1 under p2, R under H, R's block under R, N under M and
// M under P, and C's walk takes ch, cr and cn in its second part under the nodes their parent
// attributes name. Once p2 is set overtaken, P's tree read answers p2 overtaken and draws ch and
// cr inside its subtree and cn outside it, and C's coverage answers ch and cr closed and cn and c0
// gaps. Once P's own node is set overtaken, which P's tree read answers on its root, cn reads
// closed too, and c0 stays a gap: c0 stands under C's own node, live, and P's tree read draws no
// node of C's own walk, since a walk does not enter an issue under the issue.
//
// The control: the read that climbs no node of the issue above past the one the child's
// requirement names reads H, R's block and N live, so it answers ch, cr and cn gaps, and this test
// fails naming each.
func TestTheCoverageReadClimbsTheTreeOfTheIssueAbove(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})

	t.Run("in an overtaken subtree", func(t *testing.T) {
		createReferenceAPIProject(t, handler, "CLPB")
		parent := createDigestIssue(t, handler, database, "CLPB", "The parent", ":::requirement{#p0}\nThe parent's own.\n:::\n\n"+
			":::requirement{#p1}\nThe one a comment is on.\n:::\n\n:::requirement{#p2}\nThe one p1 moves under.\n:::\n", false)
		spec := readDecisionDocument(t, database, parent.Key, "spec")
		paragraph := blockIDOfText(t, handler, parent.PrimaryArtifactID, "The one a comment is on.")
		head := createBlockAnchoredComment(t, handler, parent.Key, map[string]any{"artifact": parent.PrimaryArtifactID, "quote": "comment is on", "block_id": paragraph})
		reply := createThreadComment(t, handler, parent.Key, map[string]any{"body": "Its reply.\n\nIn two parts.", "reply_to": head.ID}, "alice")
		replyBlock := firstItemBlock(t, database, refs.ItemComment, parent.Key, reply.ID)
		root := createIssueMessage(t, handler, parent.Key, map[string]any{"body": "The board must page."}, "alice")
		answer := createIssueMessage(t, handler, parent.Key, map[string]any{"body": "And sort.", "in_reply_to": root.ID}, "alice")
		movePartOf(t, database, parent.Key, refs.BlockNodeKind, spec.address("p1"), refs.BlockNodeKind, spec.address("p2"), false)
		child := createDigestChild(t, handler, database, "CLPB", "The child", ":::requirement{#c0}\nThe child's own.\n:::\n", parent.Key, true).Key
		editSpec(t, handler, child, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::requirement{#ch parent=\"dispatch://" + parent.Key +
			"/comment/" + head.ID + "\"}\nUnder the comment on p1.\n:::\n\n:::requirement{#cr parent=\"" + replyBlock + "\"}\nUnder a block of its reply.\n:::\n\n" +
			":::requirement{#cn parent=\"dispatch://" + parent.Key + "/message/" + answer.ID + "\"}\nUnder a reply to a message.\n:::\n"}})
		succeeded(t, "set p2 overtaken", writeStateAs(t, handler, parent.Key, spec.address("p2"), graphstore.StateOvertaken, "alice"))
		childSpec := readDecisionDocument(t, database, child, "spec")

		parsed, parsedOK := text.ParseRef(replyBlock, "")
		block, targeted := refs.TargetOf(parsed)
		if !parsedOK || !targeted {
			t.Fatalf("R's first block %s names no node", replyBlock)
		}
		nodeOf := func(kind, id string) refs.Target { return refs.Target{Kind: kind, ID: id} }
		headNode, replyNode := nodeOf(graphstore.KindComment, head.ID), nodeOf(graphstore.KindComment, reply.ID)
		rootNode, answerNode := nodeOf(graphstore.KindMessage, root.ID), nodeOf(graphstore.KindMessage, answer.ID)
		p1, p2 := nodeOf(refs.BlockNodeKind, spec.address("p1")), nodeOf(refs.BlockNodeKind, spec.address("p2"))
		above := walkParents(t, database, parent.Key)
		for _, want := range []struct {
			node, parent refs.Target
		}{
			{headNode, nodeOf(refs.BlockNodeKind, spec.address(paragraph))}, {nodeOf(refs.BlockNodeKind, spec.address(paragraph)), p1}, {p1, p2},
			{replyNode, headNode}, {block, replyNode}, {answerNode, rootNode}, {rootNode, nodeOf(graphstore.KindIssue, parent.Key)},
		} {
			if held := above[want.node]; held != want.parent {
				t.Fatalf("the walk of %s places %+v under %+v, want %+v: this case proves nothing", parent.Key, want.node, held, want.parent)
			}
		}
		own := walkParents(t, database, child)
		for requirement, want := range map[string]refs.Target{"ch": headNode, "cr": block, "cn": answerNode} {
			if held := own[nodeOf(refs.BlockNodeKind, childSpec.address(requirement))]; held != want {
				t.Fatalf("the walk of %s places %s under %+v, want %+v: this case proves nothing", child, requirement, held, want)
			}
		}

		page := treeRead(t, handler, parent.Key, "", "alice", false)
		if node, _ := page.node(t, refs.BlockNodeKind, p2.ID); node.State != graphstore.StateOvertaken {
			t.Fatalf("the tree read of %s answers p2 at %q, want overtaken: this case proves nothing", parent.Key, node.State)
		}
		for requirement, inside := range map[string]bool{"ch": true, "cr": true, "cn": false} {
			if drawn := drawnUnder(t, page, nodeOf(refs.BlockNodeKind, childSpec.address(requirement)), p2); drawn != inside {
				t.Fatalf("the tree read of %s draws %s of %s inside p2's subtree: %t, want %t: this case proves nothing", parent.Key, requirement, child, drawn, inside)
			}
		}
		requireCoverages(t, "with p2 overtaken", readCoverage(t, handler, child), map[string]string{"c0": "gap", "ch": "closed", "cr": "closed", "cn": "gap"})

		issueNode := graphstore.KindIssue + "/" + parent.Key
		succeeded(t, "set "+issueNode+" overtaken", writeStateAs(t, handler, parent.Key, issueNode, graphstore.StateOvertaken, "alice"))
		if node, _ := treeRead(t, handler, parent.Key, "", "alice", false).node(t, graphstore.KindIssue, parent.Key); node.State != graphstore.StateOvertaken {
			t.Fatalf("the tree read of %s answers its own node at %q, want overtaken: this case proves nothing", parent.Key, node.State)
		}
		requireCoverages(t, "with "+parent.Key+" overtaken", readCoverage(t, handler, child), map[string]string{"c0": "gap", "ch": "closed", "cr": "closed", "cn": "closed"})
	})
}

// walkParents is the parent the walk of the issue key at the head places each of its nodes under.
func walkParents(t *testing.T, database *store.Store, key string) map[refs.Target]refs.Target {
	t.Helper()
	walk, err := docs.WalkIssueTree(context.Background(), database.Pool, key, refs.TreeCut{})
	if err != nil {
		t.Fatalf("walk the tree of %s: %v", key, err)
	}
	parents := make(map[refs.Target]refs.Target, len(walk.Nodes))
	for _, node := range walk.Nodes {
		parents[refs.Target{Kind: node.Kind, ID: node.ID}] = node.Parent
	}
	return parents
}

// drawnUnder reports whether the tree read page draws the node target at any depth under the node
// ancestor, by the parent it answers each node under.
func drawnUnder(t *testing.T, page treeAnswer, target, ancestor refs.Target) bool {
	t.Helper()
	parents := make(map[refs.Target]refs.Target, len(page.Nodes))
	for _, node := range page.Nodes {
		if node.Parent != nil {
			parents[refs.Target{Kind: node.Kind, ID: node.ID}] = refs.Target{Kind: node.Parent.Kind, ID: node.Parent.ID}
		}
	}
	for at, steps := parents[target], 0; at != (refs.Target{}) && steps <= len(parents); at, steps = parents[at], steps+1 {
		if at == ancestor {
			return true
		}
	}
	return false
}

// requireCoverages fails unless coverage answers each requirement of want, by block id, at the
// coverage want names, with one gap, that no check serves it, where that is a gap and none where
// it is closed, and lists exactly the gap lines of those it answers a gap.
func requireCoverages(t *testing.T, when string, coverage model.IssueCoverage, want map[string]string) {
	t.Helper()
	var gaps []string
	for block, state := range want {
		requirement := requirementOf(t, coverage, block)
		var lines []string
		if state == "gap" {
			lines = []string{noCheckReason}
			gaps = append(gaps, noCheckGap(block))
		}
		if requirement.Coverage != state || !slices.Equal(requirement.Gaps, lines) {
			t.Errorf("the coverage route of %s, %s, answers %s as %+v, want %s with the gaps %q", coverage.Issue, when, block, requirement, state, lines)
		}
	}
	slices.Sort(gaps)
	if held := slices.Sorted(slices.Values(coverage.Gaps)); !slices.Equal(held, gaps) {
		t.Errorf("the coverage route of %s, %s, lists the gaps %q, want %q", coverage.Issue, when, coverage.Gaps, gaps)
	}
}
