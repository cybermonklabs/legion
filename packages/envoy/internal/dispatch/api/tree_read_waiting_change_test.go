package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The words of each decision of the waiting-change test, which a Change of it quotes: one
// decision for each rule of a Change waiting on review (model.IssueTreeWaitingChange).
const (
	waitingOldWords    = "We order the queue by urgency."
	waitingNoneWords   = "We keep the board on one page."
	waitingKeptWords   = "We cache each read for a minute."
	waitingEditWords   = "We page the list by fifty."
	waitingEditOther   = "We sort each page by age."
	waitingAgentWords  = "We answer each ask within a day."
	waitingDelWords    = "We log each write once."
	waitingReplWords   = "We shipped the old exporter."
	waitingHidWords    = "We hold drafts for a week."
	waitingEdgeWords   = "We retry a failed send twice."
	waitingReadingText = "The agent reads the board as one page of cards."
)

// waitingDecisions is what the agent writes into the spec: a decision for each case, wedit of two
// paragraphs, whose second an edit rewords, and wrepl replaced.
const waitingDecisions = ":::decision{#wold}\n" + waitingOldWords + "\n:::\n\n" +
	":::decision{#wnone}\n" + waitingNoneWords + "\n:::\n\n" +
	":::decision{#wkept}\n" + waitingKeptWords + "\n:::\n\n" +
	":::decision{#wedit}\n" + waitingEditWords + "\n\n" + waitingEditOther + "\n:::\n\n" +
	":::decision{#wagent}\n" + waitingAgentWords + "\n:::\n\n" +
	":::decision{#wdel}\n" + waitingDelWords + "\n:::\n\n" +
	":::decision{#wrepl replaced=\"true\"}\n" + waitingReplWords + "\n:::\n\n" +
	":::decision{#whid}\n" + waitingHidWords + "\n:::\n\n" +
	":::decision{#wedge}\n" + waitingEdgeWords + "\n:::\n"

// waitingRead is what the waiting-change test reads of a tree read, each member as the read
// writes it: each node's kind, id, parent and made_at, its decision's waiting_change, absent
// where it answers none, and comments_before.
type waitingRead struct {
	Nodes []struct {
		Kind   string          `json:"kind"`
		ID     string          `json:"id"`
		MadeAt json.RawMessage `json:"made_at"`
		Parent *struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"parent"`
		Decision *struct {
			WaitingChange json.RawMessage `json:"waiting_change"`
		} `json:"decision"`
	} `json:"nodes"`
	CommentsBefore *string `json:"comments_before"`
}

// waitingChangeRead is a waiting_change as the read writes it.
type waitingChangeRead struct {
	CommentID string          `json:"comment_id"`
	CreatedAt json.RawMessage `json:"created_at"`
}

// readWaiting is the tree read of key as login, with query, failing on anything but 200.
func readWaiting(t *testing.T, handler http.Handler, key, query, login string) waitingRead {
	t.Helper()
	response := treeResponse(t, handler, key, query, login, false)
	if response.Code != http.StatusOK {
		t.Fatalf("%s reads the tree of %s%s: status=%d body=%s", login, key, query, response.Code, response.Body.String())
	}
	return decodeBody[waitingRead](t, response)
}

// waitingOf is the waiting_change the decision node of read at address answers, nil for null; it
// fails unless read holds the node with a decision member that answers the field.
func waitingOf(t *testing.T, what string, read waitingRead, address string) *waitingChangeRead {
	t.Helper()
	for _, node := range read.Nodes {
		if node.Kind != refs.BlockNodeKind || node.ID != address {
			continue
		}
		if node.Decision == nil || len(node.Decision.WaitingChange) == 0 {
			t.Fatalf("%s: the decision %s answers no waiting_change member", what, address)
		}
		if bytes.Equal(node.Decision.WaitingChange, []byte("null")) {
			return nil
		}
		var change waitingChangeRead
		if err := json.Unmarshal(node.Decision.WaitingChange, &change); err != nil {
			t.Fatalf("%s: decode the waiting_change %s of %s: %v", what, node.Decision.WaitingChange, address, err)
		}
		return &change
	}
	t.Fatalf("%s: the read holds no decision node %s", what, address)
	return nil
}

// requireWaiting fails unless the decision node at address answers the Change want, or null where
// want is "".
func requireWaiting(t *testing.T, what string, read waitingRead, address, want string) *waitingChangeRead {
	t.Helper()
	got := waitingOf(t, what, read, address)
	switch {
	case want == "" && got != nil:
		t.Errorf("%s: the decision %s answers waiting_change %s made at %s, want null", what, address, got.CommentID, got.CreatedAt)
	case want != "" && got == nil:
		t.Errorf("%s: the decision %s answers waiting_change null, want the Change %s", what, address, want)
	case want != "" && got.CommentID != want:
		t.Errorf("%s: the decision %s answers waiting_change %s, want the Change %s", what, address, got.CommentID, want)
	}
	return got
}

// placedComment is where a comment node stands in the pages of a read: its parent and its made_at
// as the read writes it.
type placedComment struct {
	parent string
	madeAt json.RawMessage
}

// walkComments reads every comment page of the read with no view of key as login, the first and
// each comments_before after it, and answers each comment node by its id.
func walkComments(t *testing.T, handler http.Handler, key, login string) map[string]placedComment {
	t.Helper()
	placed := map[string]placedComment{}
	query := ""
	for page := 0; ; page++ {
		read := readWaiting(t, handler, key, query, login)
		for _, node := range read.Nodes {
			if node.Kind != graphstore.KindComment {
				continue
			}
			comment := placedComment{madeAt: node.MadeAt}
			if node.Parent != nil {
				comment.parent = node.Parent.ID
			}
			placed[node.ID] = comment
		}
		if read.CommentsBefore == nil {
			return placed
		}
		if page == 10 {
			t.Fatalf("the read of %s still answers a comments_before after 10 pages", key)
		}
		query = "?comments_before=" + url.QueryEscape(*read.CommentsBefore)
	}
}

// TestTheTreeReadAnswersTheChangeEachDecisionWaitsOn: an agent writes decisions into a converted
// issue's spec, and people write Changes on their words, as the issue page's Change composer
// anchors them, and 200 other comments after the first, so that Change stands off the read's first
// comment page. Each decision node of the read answers waiting_change by the page's rule of a Change
// waiting on review (INFRA-1196's decision 578, part T's decision-review.ts): the newest comment that
// stands under the decision, written by a person, after the decision's words last changed and after
// its last Keep, that its author has not deleted, on a decision that is not replaced, which the
// reader may see; null where none is. Each Change it answers is the comment the read's pages answer
// under that decision, created_at the made_at its node answers, byte for byte, and the decisions view
// answers each the same.
func TestTheTreeReadAnswersTheChangeEachDecisionWaitsOn(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "WAIT")
	key := createDigestIssue(t, handler, database, "WAIT", "Decisions that wait on Changes", "The spec the agent decides in.\n", false).Key
	spec := readDecisionDocument(t, database, key, "spec")
	agentEdit(t, handler, spec.id, latestVersion(t, database, spec.id).Number+1, map[string]string{"op": "insert", "after": "end", "markdown": waitingDecisions})
	decision := func(block string) string { return spec.address(block) }

	// write posts a comment of the issue anchored on quote in the spec, as login, or as the agent
	// where login is "", and answers its id.
	write := func(what, login, quote string) string {
		t.Helper()
		body := map[string]any{"body": "Change this: " + quote, "anchor": map[string]any{"artifact": "spec", "quote": quote}}
		var response *httptest.ResponseRecorder
		if login == "" {
			body["actor"] = sessionActor()
			response = sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", body)
		} else {
			response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", body, login)
		}
		comment := decodeBody[model.Comment](t, created(t, what, response))
		if comment.Anchor == nil || comment.Anchor.BlockID == nil {
			t.Fatalf("%s answers the anchor %+v, want one naming the block of the words it quotes", what, comment.Anchor)
		}
		return comment.ID
	}
	setMadeAt := func(t *testing.T, comment string, at time.Time) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `update comments set created_at = $2 where id = $1::uuid`, comment, at); err != nil {
			t.Fatalf("set when comment %s was made: %v", comment, err)
		}
	}

	oldChange := write("alice's Change of wold", "alice", "order the queue by urgency")
	for index := range issueTreeCommentPageSize {
		created(t, fmt.Sprintf("comment %d", index+1), dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments",
			map[string]any{"body": fmt.Sprintf("Remark %d on the queue.", index+1)}, "bob"))
	}
	keptChange := write("alice's Change of wkept", "alice", "cache each read for a minute")
	editChange := write("alice's Change of wedit", "alice", "page the list by fifty")
	personChange := write("alice's Change of wagent", "alice", "answer each ask within a day")
	agentComment := write("the agent's comment on wagent", "", "answer each ask within a day")
	firstDel := write("alice's first Change of wdel", "alice", "log each write once")
	secondDel := write("alice's second Change of wdel", "alice", "log each write once")
	thirdDel := write("alice's third Change of wdel", "alice", "log each write once")
	replChange := write("alice's Change of the replaced wrepl", "alice", "shipped the old exporter")
	seenHid := write("alice's Change of whid that bob may see", "alice", "hold drafts for a week")
	hiddenHid := write("alice's Change of whid reserved for her", "alice", "hold drafts for a week")
	edgeChange := write("alice's Change of wedge", "alice", "retry a failed send twice")

	before := treeRead(t, handler, key, "", "alice", false)
	kept, _ := before.node(t, refs.BlockNodeKind, decision("wkept"))
	keepBlock(t, handler, spec.id, "wkept", kept.Decision.TextSHA256)
	agentEdit(t, handler, spec.id, latestVersion(t, database, spec.id).Number+1, replaceOp(waitingEditOther, "We sort each page by age, the oldest first."))
	deleteThreadComment(t, handler, thirdDel, "alice")
	succeeded(t, "alice writes a reading into wnone", editDocument(t, handler, spec.id, map[string]any{"ops": []map[string]any{{
		"op": "insert", "into": "block:wnone", "markdown": ":::reading{#gnone}\n" + waitingReadingText + "\n:::\n",
	}}}))
	readingComment := write("alice's comment on wnone's reading", "alice", "reads the board as one page of cards")
	if _, err := database.Pool.Exec(ctx, `update graph_nodes set visibility = $3 where kind = $1 and id = $2`,
		graphstore.KindComment, hiddenHid, graphstore.VisibilityAuthor); err != nil {
		t.Fatalf("reserve alice's Change %s of whid for her: %v", hiddenHid, err)
	}
	awaitSettled(t, database, spec.id)
	indexOwedDocuments(t, database, deps)

	first := readWaiting(t, handler, key, "", "alice")
	placed := walkComments(t, handler, key, "alice")
	// standsUnder fails unless the read's pages answer comment under the node at address, as the
	// page's rule finds the decision a comment hangs under (parentsIn), so a case of a comment the
	// rule leaves out leaves it out for its own condition.
	standsUnder := func(t *testing.T, what, comment, address string) {
		t.Helper()
		if held, found := placed[comment]; !found || held.parent != address {
			t.Fatalf("%s: the read's pages answer the comment %s under %q (held %t), want under %s, so this case proves nothing",
				what, comment, held.parent, found, address)
		}
	}
	// answersChange fails unless the decision at address answers the Change want on read, the
	// comment the pages answer under that decision, created_at its made_at as the read writes it.
	answersChange := func(t *testing.T, what string, read waitingRead, address, want string) {
		t.Helper()
		got := requireWaiting(t, what, read, address, want)
		if got == nil || got.CommentID != want {
			return
		}
		standsUnder(t, what, want, address)
		if held := placed[want]; !bytes.Equal(got.CreatedAt, held.madeAt) {
			t.Errorf("%s: the decision %s answers the Change %s created_at %s, want %s, the made_at the read answers its node with",
				what, address, want, got.CreatedAt, held.madeAt)
		}
	}

	t.Run("a decision older than the comment page answers its Change on the first page", func(t *testing.T) {
		if first.CommentsBefore == nil {
			t.Fatalf("the read's first page answers comments_before null, want the older comments it leaves off")
		}
		for _, node := range first.Nodes {
			if node.Kind == graphstore.KindComment && node.ID == oldChange {
				t.Fatalf("the read's first page answers wold's Change %s, want it on an older comment page", oldChange)
			}
		}
		answersChange(t, "the first page", first, decision("wold"), oldChange)
		answersChange(t, "the decisions view", readWaiting(t, handler, key, "?view="+graphstore.QueryDecisions, "alice"), decision("wold"), oldChange)
	})

	t.Run("a decision with no Change answers null", func(t *testing.T) {
		requireWaiting(t, "the first page", first, decision("wnone"), "")
		// A reading alice wrote under wnone after its words is a node of a person under the
		// decision, and no comment; a comment on the reading's words stands under the reading.
		read := treeRead(t, handler, key, "", "alice", false)
		reading, _ := read.node(t, refs.BlockNodeKind, spec.address("gnone"))
		none, _ := read.node(t, refs.BlockNodeKind, decision("wnone"))
		if reading.Parent == nil || reading.Parent.ID != decision("wnone") || len(reading.Author) == 0 || reading.Author[0].Kind != "user" ||
			reading.MadeAt == nil || none.Decision.ChangedAt == nil || !reading.MadeAt.After(*none.Decision.ChangedAt) {
			t.Fatalf("the reading %s stands under %+v, written by %+v at %v, want under wnone, by a person, after wnone's words changed at %v, so this case proves nothing",
				reading.ID, reading.Parent, reading.Author, reading.MadeAt, none.Decision.ChangedAt)
		}
		standsUnder(t, "the comment on wnone's reading", readingComment, reading.ID)
		requireWaiting(t, "the decisions view", readWaiting(t, handler, key, "?view="+graphstore.QueryDecisions, "alice"), decision("wnone"), "")
	})

	t.Run("a Change a Keep or an edit of the decision's words came after answers null", func(t *testing.T) {
		standsUnder(t, "wkept's Change", keptChange, decision("wkept"))
		standsUnder(t, "wedit's Change", editChange, decision("wedit"))
		requireWaiting(t, "wkept after alice's Keep", first, decision("wkept"), "")
		requireWaiting(t, "wedit after the agent's edit of its words", first, decision("wedit"), "")
		node, _ := treeRead(t, handler, key, "", "alice", false).node(t, refs.BlockNodeKind, decision("wedit"))
		if node.Decision.ChangedAt == nil {
			t.Fatalf("wedit answers no changed_at")
		}
		setMadeAt(t, editChange, node.Decision.ChangedAt.Add(time.Microsecond))
		requireWaiting(t, "wedit with its Change made after the edit", readWaiting(t, handler, key, "", "alice"), decision("wedit"), editChange)
	})

	t.Run("an agent's comment is no Change", func(t *testing.T) {
		standsUnder(t, "the agent's comment", agentComment, decision("wagent"))
		answersChange(t, "the first page", first, decision("wagent"), personChange)
	})

	t.Run("the newest Change its author has not deleted waits", func(t *testing.T) {
		standsUnder(t, "the deleted Change", thirdDel, decision("wdel"))
		standsUnder(t, "the first Change", firstDel, decision("wdel"))
		answersChange(t, "the first page", first, decision("wdel"), secondDel)
	})

	t.Run("a replaced decision answers null", func(t *testing.T) {
		standsUnder(t, "the Change of wrepl", replChange, decision("wrepl"))
		requireWaiting(t, "the first page", first, decision("wrepl"), "")
	})

	// A comment the read's comment pages never answer is no Change any reader is answered: the
	// walk reads the conversation as the server sees the store (docs' treeWalk.readConversation),
	// so alice's Change reserved for her stands on neither her pages nor bob's, though it stands
	// in the store where her visible Change of whid stands, and each is answered that older one.
	t.Run("a Change the reader may not see is not answered", func(t *testing.T) {
		hiddenAt, _ := placementOf(t, database, graphstore.KindComment, hiddenHid)
		seenAt, _ := placementOf(t, database, graphstore.KindComment, seenHid)
		if hiddenAt.ToKind != seenAt.ToKind || hiddenAt.ToID != seenAt.ToID {
			t.Fatalf("alice's reserved Change %s stands under %s and her visible one under %s, want the same block of whid's words, so this case proves nothing",
				hiddenHid, hiddenAt.ToID, seenAt.ToID)
		}
		for _, login := range []string{"alice", "bob"} {
			if _, seen := walkComments(t, handler, key, login)[hiddenHid]; seen {
				t.Fatalf("%s's read answers alice's reserved Change %s, so this case proves nothing", login, hiddenHid)
			}
			answersChange(t, login+"'s read", readWaiting(t, handler, key, "", login), decision("whid"), seenHid)
		}
	})

	t.Run("a Change made at the decision's last change or Keep is none", func(t *testing.T) {
		answersChange(t, "the first page", first, decision("wedge"), edgeChange)
		read := treeRead(t, handler, key, "", "alice", false)
		edge, _ := read.node(t, refs.BlockNodeKind, decision("wedge"))
		keptNode, _ := read.node(t, refs.BlockNodeKind, decision("wkept"))
		if edge.Decision.ChangedAt == nil || keptNode.Decision.Kept == nil {
			t.Fatalf("wedge answers changed_at %v and wkept the Keep %+v, want both", edge.Decision.ChangedAt, keptNode.Decision.Kept)
		}
		for _, moment := range []struct {
			what, address, comment string
			at                     time.Time
		}{
			{"wedge's words last changed", decision("wedge"), edgeChange, *edge.Decision.ChangedAt},
			{"wkept's Keep", decision("wkept"), keptChange, keptNode.Decision.Kept.KeptAt},
		} {
			setMadeAt(t, moment.comment, moment.at)
			requireWaiting(t, "a Change made when "+moment.what, readWaiting(t, handler, key, "", "alice"), moment.address, "")
			setMadeAt(t, moment.comment, moment.at.Add(time.Microsecond))
			requireWaiting(t, "a Change made a microsecond after "+moment.what, readWaiting(t, handler, key, "", "alice"), moment.address, moment.comment)
		}
	})
}
