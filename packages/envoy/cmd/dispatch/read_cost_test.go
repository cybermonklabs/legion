package main

// dispatch://INFRA-1149's c-read-cost: no read this child changes gets slower, and no event
// outruns its node.
//
// The test is TestNoReadThisChildChangesGetsSlowerAndNoEventOutrunsItsNode at the end of this
// file; everything above it is the four corpora it writes, the eight reads it measures over each
// of them, and the publish-after-commit sequence it runs last. graph_cost_test.go holds the two
// arms, the clock and the order statistic the two cost measurements share, and write_cost_test.go
// the document generator and the corpus writers both use.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The four corpora, as c-read-cost states them: a document of 8,001 blocks nested eight deep, a
// node with 5,000 inbound tuples, a chain of 200 blocks from a document down to a leaf, and a
// person with 50,000 rows of unrelated history. A read that is unchanged on one shape can be
// ruinous on another, which is why the run states its shapes rather than measuring one.
const (
	readCostProject = "RCOST"
	// readCostHeavyBlocks is the first shape: one document of this many blocks, nesting
	// writeCostNesting levels deep, with 40 pointers for each 1,000 blocks of it.
	readCostHeavyBlocks = 8_001
	// readCostInbound is the second shape: one block with this many tuples into it, which are
	// the pointers of a document written for the purpose.
	readCostInbound = 5_000
	// readCostChainBlocks is the third shape: a chain of this many blocks, each pointing at the
	// next, from the document down to a leaf. The Proof schema refuses a document that nests
	// more than 100 blocks, so a chain this long is a chain of pointers and not of nesting -
	// which is the chain c-visibility walks too ("a public block points at it, and it points at
	// a second public block, so a chain of three runs through it").
	readCostChainBlocks = 200
	// readCostHistoryRows is the fourth shape: a person read while the store holds this many
	// rows of history that are not theirs, written as readCostHistoryDocuments documents of
	// readCostHistoryBlocks blocks by another person.
	readCostHistoryRows      = 50_000
	readCostHistoryDocuments = 7
	readCostHistoryBlocks    = 7_143
	// readCostPersonComments is how many comments the person of the fourth shape wrote, so that
	// their page answers rows of their own out of that history.
	readCostPersonComments = 200
	// readCostOther is the login the fourth shape's history belongs to; costLogin is the person
	// whose page is read.
	readCostOther = "mallory"
	// readCostPhrase is the start of the phrase each shape's own main document carries, which
	// nothing else of the corpus does, so the search of a shape matches that shape's corpus
	// (readCostShapePhrase).
	readCostPhrase = "zarf"
	// readCostIndexWait bounds the wait for the block-index reconciliation to index a corpus.
	readCostIndexWait = 30 * time.Minute
)

// readCostShape is one corpus and what the eight reads address on it.
type readCostShape struct {
	// name is the shape as the check states it, printed on every line of the table.
	name string
	// issue is the issue the shape hangs under, document its main document (by artifact id)
	// and documentRef that document's dispatch:// address (costDocumentAddress).
	issue       string
	document    string
	documentRef string
	// into is the address of the block the tuples-into read reads, outOf the address of the
	// block the tuples-out read reads, and chain the address the chain walk starts at.
	into  string
	outOf string
	chain string
	// chainSteps is how many reads the walk down this shape's chain takes, which the run holds
	// each walk to: a chain that broke would otherwise be read as a fast chain. A shape that
	// states its own length (the chain of 200) carries it from the seed and the walk taken
	// before the measurement is held to it; every other shape's is that walk's own count.
	chainSteps int
	// phrase is the word this shape's main document carries and no other document of the
	// corpus does, which the search read of this shape asks for.
	phrase string
	// rows is what the shape holds, printed before any reading is taken.
	rows []string
}

// ───── the four corpora ───────────────────────────────────────────────────

// seedReadCost writes one arm's four corpora through that arm's own API and answers them in the
// order they are measured. No row here is written by SQL: each arm holds what its own build
// writes for these requests.
func seedReadCost(t *testing.T, arm *costArm) []*readCostShape {
	t.Helper()
	started := time.Now()
	arm.server.call(t, http.MethodPost, "/api/v1/projects",
		`{"key":"`+readCostProject+`","name":"The read-cost corpora"}`, http.StatusCreated)
	shapes := []*readCostShape{
		seedReadCostHeavy(t, arm),
		seedReadCostInbound(t, arm),
		seedReadCostChain(t, arm),
		seedReadCostHistory(t, arm),
	}
	// The planner reads a table it has no statistics for as reltuples = -1, and chooses for it
	// accordingly: measured on this pod, the unchanged BlockLinks statement costs 177 to 1,831 ms
	// over an un-analysed copy of this corpus and 1 to 9 ms over the analysed one, on both arms
	// and under force_custom_plan alike. A corpus written in one burst and read at once is that
	// cold database, which no deployment serves: autovacuum has analysed a live table long
	// before a person reads it. So the run analyses what it wrote, and measures the plan
	// production takes.
	arm.analyze(t)
	t.Logf("%s: the four corpora were written in %s", arm.name, time.Since(started).Round(time.Second))
	return shapes
}

// seedReadCostHeavy is the first shape: one document of 8,001 blocks nesting eight deep, with 40
// pointers at blocks of another document for each 1,000 blocks of it.
func seedReadCostHeavy(t *testing.T, arm *costArm) *readCostShape {
	t.Helper()
	phrase := readCostShapePhrase("heavy")
	issue := createCostIssue(t, arm, readCostProject, "The document of 8,001 blocks")
	targets := costPointerTargets(t, arm, issue, "heavy-targets.md", costPointerCount(readCostHeavyBlocks))
	document := costUploadDocument(t, arm, issue, "heavy.md",
		costDocumentText(readCostHeavyBlocks, writeCostNesting, costPointerCount(readCostHeavyBlocks), targets, 0, -1)+
			"\n"+phrase+" is in this document and in no other of the corpus.\n")
	return &readCostShape{
		name:        "a document of 8,001 blocks nested eight deep",
		phrase:      phrase,
		issue:       issue,
		document:    document,
		documentRef: costDocumentAddress(t, arm, document),
		into:        targets[0],
	}
}

// seedReadCostInbound is the second shape: one block with 5,000 tuples into it, which are the
// pointers of a document of 5,000 items written at it.
func seedReadCostInbound(t *testing.T, arm *costArm) *readCostShape {
	t.Helper()
	phrase := readCostShapePhrase("inbound")
	issue := createCostIssue(t, arm, readCostProject, "The node of 5,000 inbound tuples")
	target := costPointerTargets(t, arm, issue, "inbound-target.md", 1)[0]
	var text strings.Builder
	text.WriteString("# The document that points at one block\n\n")
	text.WriteString(phrase + " is in this document and in no other of the corpus.\n\n")
	for index := range readCostInbound {
		fmt.Fprintf(&text, "- item %d points at %s\n", index, target)
	}
	document := costUploadDocument(t, arm, issue, "inbound.md", text.String())
	return &readCostShape{
		name:        "a node with 5,000 inbound tuples",
		phrase:      phrase,
		issue:       issue,
		document:    document,
		documentRef: costDocumentAddress(t, arm, document),
		into:        target,
	}
}

// seedReadCostChain is the third shape: a chain of 200 blocks from the document down to a leaf,
// each block pointing at the next.
//
// The chain is built from its leaf up, one block to a version. A block keeps its id while its
// own text is unchanged and a block whose text changed is a new node, so a chain cannot be
// written by rewriting every block of one version with the pointers of the version before it:
// every pointer would then name a block that version replaced. Each version here adds one block
// at the top, pointing at the block the version before it added, and leaves every other block's
// text alone. The run walks the chain before it measures anything and holds it to its length, so
// a chain that did not form fails rather than being measured as a fast one.
func seedReadCostChain(t *testing.T, arm *costArm) *readCostShape {
	t.Helper()
	phrase := readCostShapePhrase("chain")
	issue := createCostIssue(t, arm, readCostProject, "The chain of 200 blocks")
	head := "# The chain this run walks\n\n" + phrase + " is in this document and in no other of the corpus.\n\n"
	body := "Link 0 of the chain is its leaf.\n\n"
	var document, address string
	var links []string
	for step := range readCostChainBlocks {
		if step > 0 {
			body = fmt.Sprintf("Link %d of the chain points at %s#b-%s.\n\n", step, address, links[0]) + body
		}
		document = costUploadDocument(t, arm, issue, "chain.md", head+body)
		if address == "" {
			address = costDocumentAddress(t, arm, document)
		}
		links = readCostChainLinks(t, arm, document)
		if len(links) != step+1 {
			t.Fatalf("%s: the chain holds %d block(s) after %d version(s), want %d: a block whose text a version changed is a new node, and this chain would not be the stated shape",
				arm.name, len(links), step+1, step+1)
		}
	}
	return &readCostShape{
		name:        "a chain of 200 blocks from a document down to a leaf",
		phrase:      phrase,
		issue:       issue,
		document:    document,
		documentRef: address,
		into:        address + "#b-" + links[len(links)-1],
		chain:       address + "#b-" + links[0],
		chainSteps:  readCostChainBlocks,
	}
}

// readCostChainLinks are the ids of the chain document's link blocks, in document order: every
// paragraph but the one carrying the shape's search phrase.
func readCostChainLinks(t *testing.T, arm *costArm, document string) []string {
	t.Helper()
	links := []string{}
	for index, block := range costBlocksOf(t, arm, document) {
		if block.Type != "paragraph" || index == 1 {
			continue
		}
		links = append(links, block.ID)
	}
	return links
}

// seedReadCostHistory is the fourth shape: a person read while the store holds 50,000 rows of
// history that are not theirs. The history is seven documents of another person's, which this
// child's arm holds as 50,000 block nodes and their tuples beside the 50,000 projection rows
// both arms hold; the person of the shape wrote 200 comments of their own, so that their page
// answers rows out of that pile rather than nothing.
func seedReadCostHistory(t *testing.T, arm *costArm) *readCostShape {
	t.Helper()
	phrase := readCostShapePhrase("person")
	issue := createCostIssue(t, arm, readCostProject, "The person read against 50,000 rows of other history")
	targets := costPointerTargets(t, arm, issue, "history-targets.md", costPointerCount(readCostHistoryBlocks))
	for index := range readCostHistoryDocuments {
		costUploadDocumentAs(t, arm, readCostOther, issue, fmt.Sprintf("history%d.md", index),
			costDocumentText(readCostHistoryBlocks, writeCostNesting, costPointerCount(readCostHistoryBlocks), targets, index, -1))
	}
	document := costUploadDocument(t, arm, issue, "person.md",
		"# The document of the person's own shape\n\n"+
			phrase+" is in this document and in no other of the corpus.\n\n"+
			"A paragraph the person's own page hangs beside.\n")
	for index := range readCostPersonComments {
		body, err := json.Marshal(map[string]any{
			"body": fmt.Sprintf("Comment %d of the person whose page this shape reads.", index),
		})
		if err != nil {
			t.Fatalf("encode the person's comment: %v", err)
		}
		arm.server.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", string(body), http.StatusCreated)
	}
	return &readCostShape{
		name:        "a person with 50,000 rows of unrelated history",
		phrase:      phrase,
		issue:       issue,
		document:    document,
		documentRef: costDocumentAddress(t, arm, document),
		into:        targets[0],
	}
}

// costUploadDocumentAs writes one version of a document as another person, so a corpus can hold
// history that is not the person's whose page is read.
func costUploadDocumentAs(t *testing.T, arm *costArm, login, issue, name, content string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"name": name, "content": content})
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	answer := arm.server.send(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", string(body), asUser(login))
	if answer.status != http.StatusCreated {
		t.Fatalf("%s: upload %s as %s: status %d: %s", arm.name, name, login, answer.status, answer.body)
	}
}

// ───── what each shape holds, and what each read addresses on it ──────────

// settleReadCost finishes each shape once its arm has indexed it: it resolves the addresses the
// reads need, which are the server's own answers and not a guess, and reads back what each shape
// holds.
func settleReadCost(t *testing.T, arm *costArm, shapes []*readCostShape) {
	t.Helper()
	for _, shape := range shapes {
		shape.outOf = readCostPointerAt(t, arm, shape.into)
		if shape.chain == "" {
			shape.chain = shape.outOf
		}
		_, steps := readCostWalkOnce(t, arm, shape)
		if shape.chainSteps == 0 {
			shape.chainSteps = steps
		} else if steps != shape.chainSteps {
			t.Fatalf("%s: the walk down %q takes %d read(s), and the shape states a chain of %d",
				arm.name, shape.name, steps, shape.chainSteps)
		}
		shape.rows = []string{
			fmt.Sprintf("%d block(s) of the shape's own documents", arm.count(t,
				`select count(*) from document_blocks b join artifacts a on a.id = b.artifact_id where a.issue_key = $1`, shape.issue)),
			fmt.Sprintf("%d block reference(s)", arm.count(t,
				`select count(*) from block_refs r join artifacts a on a.id = r.artifact_id where a.issue_key = $1`, shape.issue)),
			fmt.Sprintf("%d comment(s)", arm.count(t, `select count(*) from comments where issue_key = $1`, shape.issue)),
		}
		if arm.head {
			shape.rows = append(shape.rows,
				fmt.Sprintf("%d tuple(s) into the node the inbound read reads", arm.count(t,
					`select count(*) from graph_tuples where to_kind = 'block' and to_id = $1 and asserted`,
					readCostBlockID(shape.into))))
		}
		t.Logf("%s: the shape %q holds %s", arm.name, shape.name, strings.Join(shape.rows, ", "))
	}
}

// readCostBlockID is the block id a `dispatch://...#b-<id>` address names.
func readCostBlockID(ref string) string {
	if cut := strings.LastIndex(ref, "#b-"); cut >= 0 {
		return ref[cut+len("#b-"):]
	}
	return ref
}

// readCostPointerAt is the address of one block that points at the block ref names, read out of
// that block's own inbound edges: the tuples-out read and the chain walk both start there.
func readCostPointerAt(t *testing.T, arm *costArm, ref string) string {
	t.Helper()
	edges := readCostEdges(t, arm, "to", ref)
	for _, edge := range edges.Edges {
		if edge.Node.Kind == "block" && edge.Node.Ref != "" {
			return edge.Node.Ref
		}
	}
	t.Fatalf("%s: nothing points at %s, so no read of this shape has a node to start from: %+v", arm.name, ref, edges)
	return ""
}

// readCostEdges is one edges read, off the clock, for the addresses the reads need.
func readCostEdges(t *testing.T, arm *costArm, direction, ref string) model.GraphReferences {
	t.Helper()
	read := arm.server.call(t, http.MethodGet,
		"/api/v1/references?"+direction+"="+url.QueryEscape(ref), "", http.StatusOK)
	var answer model.GraphReferences
	if err := json.Unmarshal(read, &answer); err != nil {
		t.Fatalf("%s: read the %s edges of %s: %v: %s", arm.name, direction, ref, err, read)
	}
	return answer
}

// ───── the eight reads ────────────────────────────────────────────────────

// readCostRead is one read this child changes, measured over each of the four shapes.
type readCostRead struct {
	name string
	// headOnly marks a read the commit before this child does not serve at all, which has no
	// arm to be compared with and is reported rather than held to the budget.
	headOnly bool
	take     func(t *testing.T, arm *costArm, shape *readCostShape) time.Duration
}

func readCostReads() []readCostRead {
	return []readCostRead{
		{name: "the list of a kind", take: func(t *testing.T, arm *costArm, shape *readCostShape) time.Duration {
			taken, _ := arm.timed(t, http.MethodGet, "/api/v1/block-links?from="+
				url.QueryEscape(shape.documentRef)+"&to="+
				url.QueryEscape(shape.documentRef), "", http.StatusOK)
			return taken
		}},
		{name: "search", take: func(t *testing.T, arm *costArm, shape *readCostShape) time.Duration {
			taken, _ := arm.timed(t, http.MethodGet,
				"/api/v1/search?project="+readCostProject+"&q="+url.QueryEscape(shape.phrase), "", http.StatusOK)
			return taken
		}},
		{name: "the tuples out of a node", take: func(t *testing.T, arm *costArm, shape *readCostShape) time.Duration {
			taken, _ := arm.timed(t, http.MethodGet,
				"/api/v1/references?from="+url.QueryEscape(shape.outOf), "", http.StatusOK)
			return taken
		}},
		{name: "the tuples into a node", take: func(t *testing.T, arm *costArm, shape *readCostShape) time.Duration {
			taken, _ := arm.timed(t, http.MethodGet,
				"/api/v1/references?to="+url.QueryEscape(shape.into), "", http.StatusOK)
			return taken
		}},
		{name: "the backlink counts", take: func(t *testing.T, arm *costArm, shape *readCostShape) time.Duration {
			taken, _ := arm.timed(t, http.MethodGet, "/api/v1/issues/"+shape.issue, "", http.StatusOK)
			return taken
		}},
		{name: "the tree expansion from a document", take: func(t *testing.T, arm *costArm, shape *readCostShape) time.Duration {
			taken, _ := arm.timed(t, http.MethodGet, "/api/v1/artifacts/"+shape.document+"/blocks", "", http.StatusOK)
			return taken
		}},
		{name: "the chain read", take: readCostWalk},
		{name: "the person page", headOnly: true, take: func(t *testing.T, arm *costArm, shape *readCostShape) time.Duration {
			taken, _ := arm.timed(t, http.MethodGet, "/api/v1/people/"+costLogin+"/nodes", "", http.StatusOK)
			return taken
		}},
	}
}

// readCostWalk is the chain read: the walk from the chain's first block along the pointer each
// block holds, down to the leaf. One reading is the whole walk, and the run holds every walk to
// the chain's own length, so a chain that broke is a failure here rather than a fast reading.
func readCostWalk(t *testing.T, arm *costArm, shape *readCostShape) time.Duration {
	t.Helper()
	taken, steps := readCostWalkOnce(t, arm, shape)
	if steps != shape.chainSteps {
		t.Fatalf("%s: the walk down %q took %d read(s), want the %d its chain holds: a chain that is not the stated shape is not the measurement this check asks for",
			arm.name, shape.name, steps, shape.chainSteps)
	}
	return taken
}

// readCostWalkOnce is that walk, with the step count it took: the measurement holds every walk
// to the count the shape states, and the pass before the measurement reads that count for a
// shape whose chain is whatever its corpus gave it.
func readCostWalkOnce(t *testing.T, arm *costArm, shape *readCostShape) (time.Duration, int) {
	t.Helper()
	total, steps, at := time.Duration(0), 0, shape.chain
	seen := map[string]bool{}
	for at != "" && !seen[at] {
		seen[at] = true
		taken, answer := arm.timed(t, http.MethodGet,
			"/api/v1/references?from="+url.QueryEscape(at), "", http.StatusOK)
		total += taken
		steps++
		var read model.GraphReferences
		if err := json.Unmarshal(answer.body, &read); err != nil {
			t.Fatalf("%s: walk the chain at %s: %v: %s", arm.name, at, err, answer.body)
		}
		at = ""
		for _, edge := range read.Edges {
			if edge.Node.Kind == "block" && edge.Node.Ref != "" && !seen[edge.Node.Ref] {
				at = edge.Node.Ref
				break
			}
		}
	}
	return total, steps
}

// ───── no event outruns its node ──────────────────────────────────────────

// readCostEventNodeSequence proves no event is published before the transaction that wrote it
// commits, and none at all for a transaction that rolled back.
//
// One connection of the test's own holds a transaction open: it appends an event through the
// real broker and writes a node through the real store writer under that event's id. A second
// connection - the server's own pool, through its event-log route, and a connection of the
// test's own for the node - reads while that transaction is open and finds neither the event nor
// the node. The transaction commits, and the event and its node are both there at once. The run
// then does the whole sequence again with a rollback, and neither ever arrives. So a publish
// before the commit, and a publish for a transaction that rolled back, each fail here.
func readCostEventNodeSequence(t *testing.T, arm *costArm, issue string) {
	t.Helper()
	for _, ending := range []string{"commit", "rollback"} {
		ctx := context.Background()
		before := readCostHighestEvent(t, arm)
		conn, err := arm.pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("take the writing connection: %v", err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			conn.Release()
			t.Fatalf("open the writing transaction: %v", err)
		}
		broker := events.NewBroker()
		actor := model.Actor{Kind: "user", ID: costLogin}
		appended, err := broker.Append(ctx, tx, model.Event{
			IssueKey: &issue, Type: "comment.created", Actor: actor,
			Payload: map[string]any{"reason": "the event-and-node sequence of c-read-cost"},
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			conn.Release()
			t.Fatalf("append the event: %v", err)
		}
		node := graphstore.Node{
			Kind: "comment", ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", appended.ID),
			Supertag: "comment", Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			MadeAt: time.Now().UTC(), MadeBy: actor, SourceEvent: appended.ID,
		}
		if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, nil); err != nil {
			_ = tx.Rollback(ctx)
			conn.Release()
			t.Fatalf("write the event's node: %v", err)
		}
		// The transaction is open. A second connection sees neither side of it. What is held
		// against the stream is this event's own id and not an empty answer: the server's own
		// loops write events of their own while this runs, and an event of theirs on the
		// stream is no failure of this one's.
		if readCostEventOnTheStream(t, arm, before, appended.ID) {
			t.Errorf("the event appended at %d was on the event stream while the transaction that appended it was still open, so an event outran its commit",
				appended.ID)
		}
		if readCostNodeIsThere(t, arm, node) {
			t.Errorf("the node of an event not yet committed is readable on a second connection")
		}

		if ending == "commit" {
			if err := tx.Commit(ctx); err != nil {
				conn.Release()
				t.Fatalf("commit the writing transaction: %v", err)
			}
		} else if err := tx.Rollback(ctx); err != nil {
			conn.Release()
			t.Fatalf("roll the writing transaction back: %v", err)
		}
		conn.Release()

		onTheStream := readCostEventOnTheStream(t, arm, before, appended.ID)
		readable := readCostNodeIsThere(t, arm, node)
		switch ending {
		case "commit":
			if !onTheStream {
				t.Errorf("the committed event %d never arrived on the event stream", appended.ID)
			}
			if !readable {
				t.Errorf("the committed event %d arrived with its node unreadable, so an event outran its node", appended.ID)
			}
			t.Logf("the event appended at %d arrived on a second connection the moment its transaction committed, with its node readable at once", appended.ID)
		case "rollback":
			if onTheStream {
				t.Errorf("the event %d of a rolled-back transaction arrived on the event stream", appended.ID)
			}
			if readable {
				t.Errorf("the node of a rolled-back transaction is readable")
			}
			t.Logf("the same sequence ending in a rollback published no event at all (the id it would have taken was %d) and left no node", appended.ID)
		}
	}
}

// readCostHighestEvent is the event log's highest id now, which the reads below page from.
func readCostHighestEvent(t *testing.T, arm *costArm) int64 {
	t.Helper()
	return arm.count(t, `select coalesce(max(id), 0) from events`)
}

// readCostEventOnTheStream reports whether the event stream, read on the server's own
// connection past the id after, answers the event id: the read a reader of the log takes.
func readCostEventOnTheStream(t *testing.T, arm *costArm, after, id int64) bool {
	t.Helper()
	return slices.Contains(readCostEventsAfter(t, arm, after), id)
}

// readCostEventsAfter is what that stream answers past after.
func readCostEventsAfter(t *testing.T, arm *costArm, after int64) []int64 {
	t.Helper()
	read := arm.server.call(t, http.MethodGet,
		"/api/v1/events/log?after="+strconv.FormatInt(after, 10), "", http.StatusOK)
	var answer []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(read, &answer); err != nil {
		t.Fatalf("read the event log past %d: %v: %s", after, err, read)
	}
	ids := make([]int64, 0, len(answer))
	for _, event := range answer {
		ids = append(ids, event.ID)
	}
	return ids
}

// readCostNodeIsThere reports whether the node is readable on a connection of the test's own,
// which is not the one the writing transaction holds.
func readCostNodeIsThere(t *testing.T, arm *costArm, node graphstore.Node) bool {
	t.Helper()
	ctx := context.Background()
	conn, err := arm.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take the reading connection: %v", err)
	}
	defer conn.Release()
	_, found, err := graphstore.ReadNode(ctx, conn.Conn(), node.Kind, node.ID)
	if err != nil {
		t.Fatalf("read the node %s %s: %v", node.Kind, node.ID, err)
	}
	return found
}

// ───── the run ────────────────────────────────────────────────────────────

// No read this child changes gets slower, and no event outruns its node.
//
// Eight reads are measured over each of four corpora, at this child's head and at the commit
// this branch forked from, in one run, interleaved, over one corpus written to each arm by one
// seeding pass. This child's arm is read twice over, once with the rows of graph_store_cutover
// on and once with them off, so the view's cutover test is inside every reading of it. Every
// reading is printed and the statistic is a real order statistic by nearest rank. It takes 200
// readings of each read on each arm, as the root's R22 requires, because p99 by nearest rank is
// the ceil(0.99 x n)th: at twenty readings, and at fifty, that is the slowest of them, so one
// scheduling hiccup of this pod decides a cell on its own. At two hundred it is the 198th. A read
// whose p99 rises by more than 100 ms fails, and the failure prints the read, the shape and both
// readings.
//
// The run then reads each notification against its own node from a second connection, which is
// the second half of this check: an event is on no reader's stream before the transaction that
// appended it commits, it is there with its node the moment it does, and a transaction that
// rolled back publishes nothing at all.
func TestNoReadThisChildChangesGetsSlowerAndNoEventOutrunsItsNode(t *testing.T) {
	t.Log(costGVisorNote)
	t.Log("how each arm is driven: both arms are server processes of their own build, each serving " +
		"a database of its own which that build migrated at boot, and each is driven over its own " +
		"HTTP API by this test, which encodes every request before the clock starts and reads every " +
		"answer to its end before it stops. This child's arm is read in both cutover states, which " +
		"are two readings of one server: the rows of graph_store_cutover are set before each " +
		"reading, off the clock, and the older build has no such table.")

	listener := costEnvoy(t)
	listener.Accept(true)
	head, base := startCostArms(t, listener.URL)
	shapes := map[*costArm][]*readCostShape{}
	for _, arm := range []*costArm{head, base} {
		shapes[arm] = seedReadCost(t, arm)
	}
	costRunBackfill(t, head)
	for _, arm := range []*costArm{head, base} {
		costBlockIndexDrained(t, arm, readCostIndexWait)
		t.Logf("%s: the whole corpus holds %d document block(s), %d block reference(s) and %d comment(s)%s",
			arm.name,
			arm.count(t, `select count(*) from document_blocks`),
			arm.count(t, `select count(*) from block_refs`),
			arm.count(t, `select count(*) from comments`),
			costStoreCounts(t, arm))
		settleReadCost(t, arm, shapes[arm])
	}

	// The three subjects: this child's arm in both cutover states, and the older build. The two
	// states are the same server read twice, which is why the corpus of a costArm is keyed by
	// the arm it was seeded on and not by the subject.
	headOn, headOff := head.inCutover(true), head.inCutover(false)
	subjects := []*costArm{headOn, headOff, base}
	corpus := func(subject *costArm) []*readCostShape {
		if subject.head {
			return shapes[head]
		}
		return shapes[base]
	}
	requireReadCostAnswersAgree(t, headOn, headOff, shapes[head])

	// No tick of either server's own runs inside a measured window.
	for _, arm := range []*costArm{head, base} {
		defer costHoldBlockIndex(t, arm)()
	}

	readings := []costReading{}
	held := map[string]map[string]costReading{}
	for _, read := range readCostReads() {
		held[read.name] = map[string]costReading{}
		for shapeIndex := range shapes[head] {
			measured := subjects
			if read.headOnly {
				measured = []*costArm{headOn, headOff}
			}
			shapeName := shapes[head][shapeIndex].name
			taken := costInterleaved(t, read.name, shapeName, costReadReadings, measured, func(subject *costArm, _ int) time.Duration {
				subject.ready(t)
				return read.take(t, subject, corpus(subject)[shapeIndex])
			})
			for _, subject := range measured {
				reading := taken[subject]
				readings = append(readings, reading)
				held[read.name][shapeName+" @ "+subject.name] = reading
			}
		}
	}
	costTableOf(t, readings)

	for _, read := range readCostReads() {
		for shapeIndex := range shapes[head] {
			shapeName := shapes[head][shapeIndex].name
			if read.headOnly {
				on := held[read.name][shapeName+" @ "+headOn.name]
				off := held[read.name][shapeName+" @ "+headOff.name]
				t.Logf("%s over %q: p99 %.1f ms with the cutover rows on and %.1f ms with them off. "+
					"The commit before this child serves no such read - GET /api/v1/people/{login}/nodes "+
					"is this child's own route - so this reading is held to no rise: there is no reading "+
					"at the older arm to rise from.",
					read.name, shapeName, costMilliseconds(on.p99()), costMilliseconds(off.p99()))
				continue
			}
			before := held[read.name][shapeName+" @ "+base.name]
			for _, subject := range []*costArm{headOn, headOff} {
				here := held[read.name][shapeName+" @ "+subject.name]
				rise := here.p99() - before.p99()
				t.Logf("%s over %q, %s: p99 %.1f ms against %.1f ms at the commit before this child, a rise of %.1f ms against the %s budget",
					read.name, shapeName, subject.name, costMilliseconds(here.p99()),
					costMilliseconds(before.p99()), costMilliseconds(rise), costBudget)
				if rise > costBudget {
					t.Errorf("%s over %q, %s: p99 %.1f ms against %.1f ms at the commit before this child, a rise of %.1f ms, over the %s budget (this child's readings %s; the commit before's %s)",
						read.name, shapeName, subject.name, costMilliseconds(here.p99()),
						costMilliseconds(before.p99()), costMilliseconds(rise), costBudget,
						here.printed(), before.printed())
				}
			}
		}
	}

	readCostEventNodeSequence(t, head, shapes[head][0].issue)
}

// requireReadCostAnswersAgree proves the two cutover states are two ways of answering one
// question and not two answers: the reads below are measured in both states, and a state that
// answered fewer rows would be measured as the faster one.
func requireReadCostAnswersAgree(t *testing.T, on, off *costArm, shapes []*readCostShape) {
	t.Helper()
	for _, shape := range shapes {
		on.ready(t)
		with := readCostEdges(t, on, "to", shape.into)
		off.ready(t)
		without := readCostEdges(t, off, "to", shape.into)
		if len(with.Edges) != len(without.Edges) {
			t.Fatalf("%q answers %d edge(s) into %s with the cutover rows on and %d with them off, so the two states are not two readings of one answer",
				shape.name, len(with.Edges), shape.into, len(without.Edges))
		}
		t.Logf("%q: the tuples into its node read %d edge(s) with the cutover rows on and the same with them off",
			shape.name, len(with.Edges))
	}
}

// readCostShapePhrase is the word one shape's main document carries and no other document of the
// corpus does: a search that matched every shape would be one read measured four times.
func readCostShapePhrase(shape string) string { return readCostPhrase + shape }
