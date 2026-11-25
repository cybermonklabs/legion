//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go.

package main

// The read corpora and readers a read cost tool of this tag uses: the four corpora, what each
// shape is held to, the state before the backfill the head's arm is put in, and the eight reads
// measured over each shape in each state (readCostMeasure). #3178's check c-read-cost, which drove
// them, is gone from main; the INFRA-1098 tree's branches use readCostEdges, readCostOther and
// readCostPointerAt. graph_cost_test.go holds the two arms, the clock and the order statistic,
// and write_cost_test.go the document generator and the corpus writers.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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
	// readCostBefore and readCostAfter are the two states of this child's arm every read is
	// measured in, named on every line and in every failure that reports a reading of either.
	readCostBefore = "before the backfill"
	readCostAfter  = "after the backfill"
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
	// intoNode is the store's address of the block into names, `<document ref_key>#<block id>`,
	// as the server's own reference read names it: block_refs and graph_tuples name a block by
	// that address and not by the dispatch:// address a reader writes.
	intoNode string
	// stated reads, out of one arm's database, the counts the check states this shape by, each
	// with the least the check's figure allows. The chain states its length, which the walk is
	// held to instead (chainSteps), so it states no count here.
	stated func(t *testing.T, arm *costArm, shape *readCostShape) []readCostCount
	// leastTuples is how many asserted tuples into intoNode this child's arm holds once its
	// backfill has run. The inbound shape is stated in tuples, and the commit before this child
	// has no store, so that figure is read on this child's arm alone.
	leastTuples int64
	// counts is what the shape holds on its arm (readCostCounts), printed before any reading is
	// taken and held to the check's figures and to the other arm's (requireReadCostShapes).
	counts []readCostCount
}

// readCostCount is one count of what a shape holds, read out of one arm's database. least is the
// smallest count the check's words allow, and zero for a count the words give no figure for,
// which is held to the other arm's count alone.
type readCostCount struct {
	what  string
	count int64
	least int64
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
		stated: func(t *testing.T, arm *costArm, shape *readCostShape) []readCostCount {
			return []readCostCount{
				{"block(s) of the document of 8,001 blocks", arm.count(t,
					`select count(*) from document_blocks where artifact_id = $1`, shape.document), readCostHeavyBlocks},
				{"level(s) that document nests", int64(costNestingDepth(t, arm, shape.document)), writeCostNesting},
			}
		},
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
		leastTuples: readCostInbound,
		stated: func(t *testing.T, arm *costArm, shape *readCostShape) []readCostCount {
			return []readCostCount{{"reference(s) into the node the inbound read reads", arm.count(t,
				`select count(*) from block_refs where to_kind = 'block' and to_id = $1`, shape.intoNode), readCostInbound}}
		},
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
	history := make([]string, 0, readCostHistoryDocuments)
	for index := range readCostHistoryDocuments {
		history = append(history, costUploadDocumentAs(t, arm, readCostOther, issue, fmt.Sprintf("history%d.md", index),
			costDocumentText(readCostHistoryBlocks, writeCostNesting, costPointerCount(readCostHistoryBlocks), targets, index, -1)))
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
		stated: func(t *testing.T, arm *costArm, _ *readCostShape) []readCostCount {
			return []readCostCount{{"block row(s) of the other person's history", arm.count(t,
				`select count(*) from document_blocks where artifact_id = any($1::uuid[])`, history), readCostHistoryRows}}
		},
	}
}

// costUploadDocumentAs writes one version of a document as another person, so a corpus can hold
// history that is not the person's whose page is read, and answers its artifact id.
func costUploadDocumentAs(t *testing.T, arm *costArm, login, issue, name, content string) string {
	t.Helper()
	body := arm.server.documentUpload(t, issue, name, content, login)
	answer := arm.server.send(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", body, asUser(login))
	if answer.status != http.StatusCreated {
		t.Fatalf("%s: upload %s as %s: status %d: %s", arm.name, name, login, answer.status, answer.body)
	}
	var created struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(answer.body, &created); err != nil || created.Artifact.ID == "" {
		t.Fatalf("%s: upload %s as %s: %v: %s", arm.name, name, login, err, answer.body)
	}
	return created.Artifact.ID
}

// ───── what each shape holds, and what each read addresses on it ──────────

// settleReadCost finishes each shape once its arm has indexed it and stands in the state its
// first readings are taken in: it resolves the addresses the reads need, which are the server's
// own answers and not a guess, and reads back what each shape holds. The addresses serve both
// states of this child's arm, because the backfill names the node of a row and moves no row.
func settleReadCost(t *testing.T, arm *costArm, shapes []*readCostShape) {
	t.Helper()
	for _, shape := range shapes {
		into := readCostEdges(t, arm, "to", shape.into)
		if into.Node.Kind != refs.BlockNodeKind || into.Node.ID == "" {
			t.Fatalf("%s: the reference read of %s names the node %+v, want a block's", arm.name, shape.into, into.Node)
		}
		shape.intoNode = into.Node.ID
		shape.outOf = readCostPointerAt(t, arm, shape.into, into)
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
		shape.counts = readCostCounts(t, arm, shape)
	}
}

// readCostCounts is what one shape holds on one arm: the rows of the shape's own issue that its
// reads run over, and the counts the check states the shape by. It prints them, because a reader
// of the table has to know what each reading was taken over.
func readCostCounts(t *testing.T, arm *costArm, shape *readCostShape) []readCostCount {
	t.Helper()
	counts := []readCostCount{
		{what: "block(s) of the shape's own documents", count: arm.count(t,
			`select count(*) from document_blocks b join artifacts a on a.id = b.artifact_id where a.issue_key = $1`, shape.issue)},
		{what: "block reference(s) out of them", count: arm.count(t,
			`select count(*) from block_refs r join artifacts a on a.id = r.artifact_id where a.issue_key = $1`, shape.issue)},
		{what: "comment(s)", count: arm.count(t, `select count(*) from comments where issue_key = $1`, shape.issue)},
	}
	if shape.stated != nil {
		counts = append(counts, shape.stated(t, arm, shape)...)
	}
	printed := make([]string, len(counts))
	for index, count := range counts {
		printed[index] = fmt.Sprintf("%d %s", count.count, count.what)
	}
	t.Logf("%s: the shape %q holds %s, and its chain read takes %d read(s)",
		arm.name, shape.name, strings.Join(printed, ", "), shape.chainSteps)
	return counts
}

// requireReadCostShapes holds each arm's corpus to the shapes c-read-cost states, and the two arms
// to one corpus. A document of fewer than 8,001 blocks, or nesting fewer than eight deep, a node
// with fewer than 5,000 references into it, or fewer than 50,000 rows of another person's history
// is not the shape the check measures. A count on one arm that differs from the same count on the
// other means the two arms are read over two corpora, and a comparison of their readings then
// compares nothing. Either ends the run before a reading is taken, naming every count that failed.
func requireReadCostShapes(t *testing.T, state string, head, base *costArm, shapes map[*costArm][]*readCostShape) {
	t.Helper()
	var failed []string
	for index, shape := range shapes[head] {
		other := shapes[base][index]
		for _, held := range []struct {
			arm   *costArm
			shape *readCostShape
		}{{head, shape}, {base, other}} {
			for _, count := range held.shape.counts {
				if count.count < count.least {
					failed = append(failed, fmt.Sprintf("%s: %q holds %d %s, and the check states at least %d",
						held.arm.name, shape.name, count.count, count.what, count.least))
				}
			}
		}
		if len(shape.counts) != len(other.counts) {
			failed = append(failed, fmt.Sprintf("%q: %s reads %d count(s) and %s reads %d",
				shape.name, head.name, len(shape.counts), base.name, len(other.counts)))
			continue
		}
		for at, count := range shape.counts {
			if count.count != other.counts[at].count {
				failed = append(failed, fmt.Sprintf("%q holds %d %s on %s and %d on %s",
					shape.name, count.count, count.what, head.name, other.counts[at].count, base.name))
			}
		}
	}
	if len(failed) > 0 {
		t.Fatalf("%s, %s: the corpora are not the shapes c-read-cost states, so no reading of them is the measurement it asks for:\n%s",
			head.name, state, strings.Join(failed, "\n"))
	}
	t.Logf("%s, %s: each shape holds at least what c-read-cost states, and every count is the same on both arms", head.name, state)
}

// requireReadCostTuples holds this child's arm, once its backfill has run, to the tuples the
// inbound shape is stated in: at least 5,000 asserted tuples into the node its tuples-into read
// reads. The commit before this child holds no store, so its corpus is held to the references
// alone (requireReadCostShapes).
func requireReadCostTuples(t *testing.T, head *costArm, shapes []*readCostShape) {
	t.Helper()
	for _, shape := range shapes {
		tuples := head.count(t,
			`select count(*) from graph_tuples where to_kind = 'block' and to_id = $1 and asserted`, shape.intoNode)
		t.Logf("%s, %s: %d asserted tuple(s) into %s, the node the tuples-into read of %q reads",
			head.name, readCostAfter, tuples, shape.intoNode, shape.name)
		if tuples < shape.leastTuples {
			t.Fatalf("%s, %s: %q holds %d asserted tuple(s) into %s, and the check states at least %d",
				head.name, readCostAfter, shape.name, tuples, shape.intoNode, shape.leastTuples)
		}
	}
}

// readCostPointerAt is the address of one block that points at the block ref names, read out of
// that block's own inbound edges: the tuples-out read and the chain walk both start there.
func readCostPointerAt(t *testing.T, arm *costArm, ref string, edges model.GraphReferences) string {
	t.Helper()
	for _, edge := range edges.Edges {
		if edge.Node.Kind == refs.BlockNodeKind && edge.Node.Ref != "" {
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

// ───── the state before the backfill ──────────────────────────────────────

// readCostProjectionTables are the three tables whose rows graph_edges answers from an old arm
// while the row names no node, and from the store's own arm once it names one.
var readCostProjectionTables = []string{"comments", "document_blocks", "block_refs"}

// readCostBeforeTheBackfill puts this child's arm in the state between its migration and its
// backfill, which is also what a pod of the release before writes during a roll: every row of
// the corpus as that release writes it, naming no node, and the store holding nothing the
// backfill writes for those rows. The view then answers every row from its old arms, which is
// the first of the two readings R21 and R22 ask for. The run holds the arm to that state before
// it reads anything, and analyses what the clearing left, as it analyses the corpus it wrote.
func readCostBeforeTheBackfill(t *testing.T, head *costArm) {
	t.Helper()
	readCostTakeTheStoreOff(t, head)
	requireReadCostNodes(t, head, readCostBefore, false)
	head.analyze(t)
	var kinds []string
	if err := head.pool.QueryRow(context.Background(),
		`select coalesce(array_agg(distinct kind order by kind), '{}') from graph_nodes`).Scan(&kinds); err != nil {
		t.Fatalf("%s: read the node kinds the store holds: %v", head.name, err)
	}
	t.Logf("%s, %s: the store holds nodes of the kinds %v alone%s", head.name, readCostBefore, kinds, costStoreCounts(t, head))
}

// readCostTakeTheStoreOff is graph_edges_cutover_test.go's unwriteTheStoreOfADocument over the
// whole corpus, which is all this database holds. It takes off every column this child's
// migrations add to a projection row and the release before leaves null (each row's node, a
// block's parent, a document's store version and the backfill's mark), and every node and tuple
// of the kinds the backfill writes for those rows: blocks, comments and their authors, and the
// sessions, roles and pages beside them. The type nodes the migrations seed stay, as they stand
// before any backfill. It is the test's own fixture and no production path.
func readCostTakeTheStoreOff(t *testing.T, arm *costArm) {
	t.Helper()
	ctx := context.Background()
	tx, err := arm.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("%s: begin taking the store off the corpus: %v", arm.name, err)
	}
	defer tx.Rollback(ctx)
	// The rows go first: each names its node under a foreign key, so a node deleted while a row
	// still names it would be refused.
	for _, statement := range []string{
		`update comments set node_kind = null, node_address = null`,
		`update document_blocks set node_kind = null, node_address = null, parent_block_id = null`,
		`update block_refs set node_kind = null, node_address = null`,
		`update document_block_versions set store_version = null, store_owed_at = null`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: take the store's columns off the corpus (%s): %v", arm.name, statement, err)
		}
	}
	kinds := []string{refs.BlockNodeKind, graphstore.KindComment, graphstore.KindPerson,
		graphstore.KindSession, graphstore.KindRole, graphstore.KindPage}
	for _, statement := range []string{
		`delete from graph_tuples where from_kind = any($1) or to_kind = any($1)`,
		`delete from graph_nodes where kind = any($1)`,
	} {
		if _, err := tx.Exec(ctx, statement, kinds); err != nil {
			t.Fatalf("%s: take the store's rows of the corpus away (%s): %v", arm.name, statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("%s: commit taking the store off the corpus: %v", arm.name, err)
	}
}

// requireReadCostNodes holds this child's arm to the state its readings are named by: before the
// backfill no row of the three projection tables names its node, and after it every row does. It
// prints how many rows of each table it read and fails on a table holding none, because a rule
// over every row of an empty table holds of nothing.
func requireReadCostNodes(t *testing.T, arm *costArm, state string, named bool) {
	t.Helper()
	held := make([]string, 0, len(readCostProjectionTables))
	for _, table := range readCostProjectionTables {
		rows := arm.count(t, `select count(*) from `+table)
		naming := arm.count(t, `select count(*) from `+table+` where node_kind is not null`)
		switch {
		case rows == 0:
			t.Fatalf("%s, %s: %s holds no row, so a rule over the node_kind of every row of the corpus holds of nothing",
				arm.name, state, table)
		case !named && naming != 0:
			t.Fatalf("%s, %s: %d of the %d %s row(s) name their node (node_kind is not null), want none: graph_edges answers such a row from the store's own arm, so a reading of it is no reading of the old arms' per-row test",
				arm.name, state, naming, rows, table)
		case named && naming != rows:
			t.Fatalf("%s, %s: %d of the %d %s row(s) name no node (node_kind is null), want none: graph_edges answers such a row from an old arm, so a reading of it is no reading of the store's own arm",
				arm.name, state, rows-naming, rows, table)
		}
		held = append(held, fmt.Sprintf("%d of the %d %s row(s) name their node", naming, rows, table))
	}
	t.Logf("%s, %s: %s", arm.name, state, strings.Join(held, ", "))
}

// ───── the measurement ────────────────────────────────────────────────────

// readCostMeasure takes every reading of the eight reads over the four shapes with this child's
// arm in one state, interleaved with the same reads at the commit before this child, prints them
// all, and holds each read's p99 to the budget. The commit before this child is read again beside
// each state, because two readings are compared only where they were taken together, under one
// load of this pod. Every line and every failure names the state.
func readCostMeasure(t *testing.T, state string, head, base *costArm, shapes map[*costArm][]*readCostShape) {
	t.Helper()
	// No tick of either server's own runs inside a measured window. The lease goes back once
	// this state's readings are taken, because the backfill between the two states leaves a
	// mark that this child's own reconciliation drains.
	for _, arm := range []*costArm{head, base} {
		defer costHoldBlockIndex(t, arm)()
	}
	headName := head.name + ", " + state
	readings := []costReading{}
	held := map[string]map[string]costReading{}
	for _, read := range readCostReads() {
		held[read.name] = map[string]costReading{}
		for shapeIndex, shape := range shapes[head] {
			measured := []*costArm{head, base}
			if read.headOnly {
				measured = []*costArm{head}
			}
			taken := costInterleaved(t, read.name, shape.name, costReadReadings, measured, func(subject *costArm, _ int) time.Duration {
				subject.ready(t)
				return read.take(t, subject, shapes[subject][shapeIndex])
			})
			for _, subject := range measured {
				reading := taken[subject]
				if subject == head {
					reading.arm = headName
				}
				readings = append(readings, reading)
				held[read.name][shape.name+" @ "+subject.name] = reading
			}
		}
	}
	t.Logf("the readings with %s:", headName)
	costTableOf(t, readings)

	for _, read := range readCostReads() {
		for _, shape := range shapes[head] {
			here := held[read.name][shape.name+" @ "+head.name]
			if read.headOnly {
				t.Logf("%s over %q, %s: p99 %.1f ms. "+
					"The commit before this child serves no such read - GET /api/v1/people/{login}/nodes "+
					"is this child's own route - so this reading is held to no rise: there is no reading "+
					"at the older arm to rise from.",
					read.name, shape.name, headName, costMilliseconds(here.p99()))
				continue
			}
			before := held[read.name][shape.name+" @ "+base.name]
			rise := here.p99() - before.p99()
			t.Logf("%s over %q, %s: p99 %.1f ms against %.1f ms at the commit before this child, a rise of %.1f ms against the %s budget",
				read.name, shape.name, headName, costMilliseconds(here.p99()),
				costMilliseconds(before.p99()), costMilliseconds(rise), costBudget)
			if rise > costBudget {
				t.Errorf("%s over %q, %s: p99 %.1f ms against %.1f ms at the commit before this child, a rise of %.1f ms, over the %s budget (this child's readings %s; the commit before's %s)",
					read.name, shape.name, headName, costMilliseconds(here.p99()),
					costMilliseconds(before.p99()), costMilliseconds(rise), costBudget,
					here.printed(), before.printed())
			}
		}
	}
}

// readCostShapePhrase is the word one shape's main document carries and no other document of the
// corpus does: a search that matched every shape would be one read measured four times.
func readCostShapePhrase(shape string) string { return readCostPhrase + shape }
