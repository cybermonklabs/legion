package main

// dispatch://INFRA-1149's c-write-cost: no write path this child changes gets slower.
//
// The test is TestNoWritePathThisChildChangesGetsSlower at the end of this file; everything
// above it is the corpus it writes and the five paths it measures. graph_cost_test.go holds the
// two arms, the clock and the order statistic the two cost measurements share.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The corpus, as c-write-cost states it. The two documents are a stated shape and not a guess:
// each nests eight deep, each holds 40 pointers for each 1,000 blocks, and each sits in an issue
// of 2,000 earlier comments.
const (
	// writeCostProject holds every write of this measurement.
	writeCostProject = "WCOST"
	// writeCostEarlierComments is the pile of comments the issue already holds when a comment
	// create is measured on it.
	writeCostEarlierComments = 2_000
	// The two document shapes: one block under the count a write indexes in its own request
	// (docs' deferBlockIndexBlocks, 2,000) and one far past it.
	writeCostUnderBlocks = 1_999
	writeCostOverBlocks  = 8_001
	// writeCostNesting is how deep each document nests, and writeCostPointersPerThousand how
	// many pointers at blocks of another document it holds for each 1,000 blocks of its own.
	writeCostNesting             = 8
	writeCostPointersPerThousand = 40
	// writeCostTickDocuments is the batch one measured reconciliation tick indexes, and
	// writeCostTickBlocks the blocks of each: one block past the bound, so each document's own
	// write defers its blocks and the tick is the pass that writes them, their nodes, their
	// tuples and their projection rows.
	writeCostTickDocuments = 8
	writeCostTickBlocks    = 2_001
	// writeCostSession is the session every measured delivery record is addressed to; the stub
	// Envoy listener answers for it (costEnvoy).
	writeCostSession = "session:s1"
	// writeCostAbsolute is the bound the 8,001-block request carries beyond the relative one:
	// IndexDocumentReferences already spends 347 ms on that version inside a request measured
	// at 939 to 1,130 ms (docs/reference_excerpts.go), so a change inside the 100 ms relative
	// budget can still put that request over its own bound. The run asserts both.
	writeCostAbsolute = time.Second
	// writeCostTickPoll is how often the tick measurement reads the owed count: the span it
	// measures is seconds, so this is resolution enough and little load of its own.
	writeCostTickPoll = 5 * time.Millisecond
	// writeCostTickWait bounds the wait for a tick to run. One comes within
	// docs.BlockIndexReconcileInterval (30 s) of the pass's lease being given back.
	writeCostTickWait = 5 * time.Minute
	// writeCostTickPath names the tick among the five, which the run measures differently from
	// the four that are requests.
	writeCostTickPath = "reconciliation tick"
)

// The readings already in the source tree, which the run prints beside its own. They were taken
// on another machine and are no arm of this comparison: what a reader takes from them is the
// shape of the cost, never a figure to hold this pod's to.
var writeCostRecorded = []string{
	"1,999 blocks, indexed in its own write: 234, 239 and 252 ms (docs/reference_excerpts.go, deferBlockIndexBlocks)",
	"2,001 blocks, whose index the write defers: 144, 145 and 157 ms (docs/reference_excerpts.go, deferBlockIndexBlocks)",
	"8,001 blocks: 347 ms inside IndexDocumentReferences, of a request total of 939-1,130 ms (sjawhar/legion#2519, docs/reference_excerpts.go)",
}

// writeCostCorpus is what one arm holds and what this measurement addresses on it.
type writeCostCorpus struct {
	arm *costArm
	// issue is the issue of 2,000 earlier comments; under and over the two measured documents,
	// by artifact id; mentioned the comment every delivery record is recorded against; tick the
	// documents one measured reconciliation tick indexes; targets the addresses the documents'
	// pointers point at.
	issue     string
	under     string
	over      string
	mentioned string
	tick      []string
	targets   []string
	// underBlocks and overBlocks are the blocks each document was read back holding.
	underBlocks, overBlocks int
}

// ───── the corpus ─────────────────────────────────────────────────────────

// seedWriteCost writes one arm's whole corpus through that arm's own API: the project, the issue
// and its 2,000 earlier comments, the document the pointers point at, the two measured documents
// and the tick's batch. No row here is written by SQL, so each arm holds what its own build
// writes for these requests - on this child's arm the nodes and tuples beside every projection
// row, on the older arm the projection rows alone.
func seedWriteCost(t *testing.T, arm *costArm) *writeCostCorpus {
	t.Helper()
	started := time.Now()
	arm.server.call(t, http.MethodPost, "/api/v1/projects",
		`{"key":"`+writeCostProject+`","name":"The write-cost corpus"}`, http.StatusCreated)
	corpus := &writeCostCorpus{arm: arm}
	corpus.issue = createCostIssue(t, arm, writeCostProject, "The issue every measured write lands on")
	corpus.targets = costPointerTargets(t, arm, corpus.issue, "wtargets.md", costPointerCount(writeCostOverBlocks))

	corpus.under = costUploadDocument(t, arm, corpus.issue, "under.md",
		costDocumentText(writeCostUnderBlocks, writeCostNesting, costPointerCount(writeCostUnderBlocks), corpus.targets, 0))
	corpus.over = costUploadDocument(t, arm, corpus.issue, "over.md",
		costDocumentText(writeCostOverBlocks, writeCostNesting, costPointerCount(writeCostOverBlocks), corpus.targets, 0))
	for index := range writeCostTickDocuments {
		corpus.tick = append(corpus.tick, costUploadDocument(t, arm, corpus.issue, writeCostTickName(index),
			costDocumentText(writeCostTickBlocks, writeCostNesting, costPointerCount(writeCostTickBlocks), corpus.targets, 0)))
	}
	corpus.mentioned = costMentionComment(t, arm, corpus.issue,
		"The comment every measured delivery record is recorded against.", writeCostSession)
	seedCostComments(t, arm, corpus.issue, writeCostEarlierComments)

	// The blocks each document really holds, read back from the arm's own answer rather than
	// counted by the generator: a corpus whose shape is assumed proves nothing about the shape
	// the check states.
	corpus.underBlocks = costBlockCount(t, arm, corpus.under)
	corpus.overBlocks = costBlockCount(t, arm, corpus.over)
	t.Logf("%s: seeded in %s - %s holds %d comment(s), under.md %d block(s), over.md %d block(s), and %d tick document(s) of %d block(s)",
		arm.name, time.Since(started).Round(time.Second), corpus.issue,
		arm.count(t, `select count(*) from comments where issue_key = $1`, corpus.issue),
		corpus.underBlocks, corpus.overBlocks, len(corpus.tick), costBlockCount(t, arm, corpus.tick[0]))
	return corpus
}

func writeCostTickName(index int) string { return fmt.Sprintf("tick%d.md", index) }

// requireWriteCostShapes holds each arm's corpus to the shape c-write-cost states: 1,999 blocks
// and 8,001 blocks, each nesting eight deep with 40 pointers for each 1,000 blocks, in an issue
// of 2,000 earlier comments. A corpus that is not that shape is not the measurement the check
// asks for, so this ends the run rather than reporting figures taken over something else.
func requireWriteCostShapes(t *testing.T, corpus *writeCostCorpus) {
	t.Helper()
	arm := corpus.arm
	if corpus.underBlocks != writeCostUnderBlocks {
		t.Fatalf("%s: under.md holds %d blocks, want %d", arm.name, corpus.underBlocks, writeCostUnderBlocks)
	}
	if corpus.overBlocks != writeCostOverBlocks {
		t.Fatalf("%s: over.md holds %d blocks, want %d", arm.name, corpus.overBlocks, writeCostOverBlocks)
	}
	comments := arm.count(t, `select count(*) from comments where issue_key = $1`, corpus.issue)
	if comments < writeCostEarlierComments {
		t.Fatalf("%s: %s holds %d comment(s), want at least the %d earlier ones the shape states",
			arm.name, corpus.issue, comments, writeCostEarlierComments)
	}
	for _, document := range []struct {
		name string
		id   string
		want int
	}{
		{"under.md", corpus.under, costPointerCount(writeCostUnderBlocks)},
		{"over.md", corpus.over, costPointerCount(writeCostOverBlocks)},
	} {
		pointers := arm.count(t, `select count(*) from block_refs where artifact_id = $1`, document.id)
		if pointers != int64(document.want) {
			t.Fatalf("%s: %s holds %d pointer(s) at other blocks, want the %d its shape states (40 for each 1,000 blocks)",
				arm.name, document.name, pointers, document.want)
		}
		if deep := costNestingDepth(t, arm, document.id); deep < writeCostNesting {
			t.Fatalf("%s: %s nests %d level(s) deep, want %d", arm.name, document.name, deep, writeCostNesting)
		}
	}
	t.Logf("%s: the corpus is the stated shape - under.md %d blocks with %d pointers, over.md %d blocks with %d pointers, "+
		"each nesting %d levels deep, in an issue of %d earlier comments",
		arm.name, corpus.underBlocks, costPointerCount(writeCostUnderBlocks), corpus.overBlocks,
		costPointerCount(writeCostOverBlocks), writeCostNesting, comments-1)
}

// costPointerCount is 40 pointers for each 1,000 blocks of a document, as the shape states.
func costPointerCount(blocks int) int {
	return blocks * writeCostPointersPerThousand / 1000
}

// ───── the five paths ─────────────────────────────────────────────────────

// writeCostCommentCreate is one comment create on the issue of 2,000 earlier comments. On this
// child's arm the write also makes the comment's node, its author's node and their tuples.
func writeCostCommentCreate(t *testing.T, corpus *writeCostCorpus, run int) time.Duration {
	t.Helper()
	body, err := json.Marshal(map[string]any{"body": fmt.Sprintf("The comment create measured at run %d.", run)})
	if err != nil {
		t.Fatalf("encode the comment: %v", err)
	}
	taken, _ := corpus.arm.timed(t, http.MethodPost, "/api/v1/issues/"+corpus.issue+"/comments",
		string(body), http.StatusCreated)
	return taken
}

// writeCostDeliveryRecord is one comment delivery record: the mention is sent again to the
// session the stub listener answers for, which records the attempt, sends it and records the
// outcome. On this child's arm it also makes the session's node and appends the delivered_to
// tuple.
func writeCostDeliveryRecord(t *testing.T, corpus *writeCostCorpus, _ int) time.Duration {
	t.Helper()
	taken, _ := corpus.arm.timed(t, http.MethodPost, "/api/v1/comments/"+corpus.mentioned+"/deliveries",
		`{"delivery":"btw"}`, http.StatusCreated)
	return taken
}

// writeCostVersion is one document version write: the whole document again with one word of
// every item changed, which is a version of the same shape as the one before it.
func writeCostVersion(t *testing.T, corpus *writeCostCorpus, name string, blocks, run int) time.Duration {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name":    name,
		"content": costDocumentText(blocks, writeCostNesting, costPointerCount(blocks), corpus.targets, run+1),
	})
	if err != nil {
		t.Fatalf("encode the version: %v", err)
	}
	taken, _ := corpus.arm.timed(t, http.MethodPost, "/api/v1/issues/"+corpus.issue+"/artifacts",
		string(body), http.StatusCreated)
	return taken
}

// writeCostTick measures one reconciliation tick over a batch of documents whose blocks their
// own writes deferred.
//
// The tick is a loop of the server's own and no request, so it is measured where it commits: the
// pass indexes each owed document in a transaction of its own, so the owed count falls once for
// each document it finishes. The run holds the pass's own lease while it writes the batch's new
// versions (costHoldBlockIndex), so no tick runs over a half-written batch; gives the lease
// back; and reads the owed count every 5 ms. The reading is from the first document that tick
// committed to the last, which is the tick's own indexing work with the wait before it left out.
// Both arms are measured that way, each on its own server's own loop, so the one quantity is
// compared with itself.
func writeCostTick(t *testing.T, corpus *writeCostCorpus, run int) time.Duration {
	t.Helper()
	arm := corpus.arm
	release := costHoldBlockIndex(t, arm)
	for index := range corpus.tick {
		body, err := json.Marshal(map[string]any{
			"name":    writeCostTickName(index),
			"content": costDocumentText(writeCostTickBlocks, writeCostNesting, costPointerCount(writeCostTickBlocks), corpus.targets, run+2),
		})
		if err != nil {
			release()
			t.Fatalf("encode the tick document: %v", err)
		}
		arm.server.call(t, http.MethodPost, "/api/v1/issues/"+corpus.issue+"/artifacts", string(body), http.StatusCreated)
	}
	owed := arm.count(t, `select count(*) from document_block_versions where version is null`)
	if owed != int64(len(corpus.tick)) {
		release()
		t.Fatalf("%s: %d document(s) are owed a block index before the measured tick, want the %d of the batch",
			arm.name, owed, len(corpus.tick))
	}
	release()

	deadline := time.Now().Add(writeCostTickWait)
	var first time.Time
	for {
		left := arm.count(t, `select count(*) from document_block_versions where version is null`)
		now := time.Now()
		if left < owed && first.IsZero() {
			first = now
		}
		if left == 0 {
			if first.IsZero() {
				t.Fatalf("%s: the tick indexed the whole batch between two %s polls, so this reading would be of the poll and not of the tick",
					arm.name, writeCostTickPoll)
			}
			return now.Sub(first)
		}
		if now.After(deadline) {
			t.Fatalf("%s: %d document(s) were still owed a block index %s after the pass's lease was given back:\n%s",
				arm.name, left, writeCostTickWait, arm.server.Output.String())
		}
		time.Sleep(writeCostTickPoll)
	}
}

// ───── the run ────────────────────────────────────────────────────────────

// writeCostPath is one of the five paths, as the run measures it on either arm.
type writeCostPath struct {
	name string
	// absolute is the bound this path carries beyond the relative one, zero for a path that
	// carries none.
	absolute time.Duration
	take     func(t *testing.T, corpus *writeCostCorpus, run int) time.Duration
}

func writeCostPaths() []writeCostPath {
	return []writeCostPath{
		{name: "comment create", take: writeCostCommentCreate},
		{name: "comment delivery record", take: writeCostDeliveryRecord},
		{name: "document version, 1,999 blocks", take: func(t *testing.T, corpus *writeCostCorpus, run int) time.Duration {
			return writeCostVersion(t, corpus, "under.md", writeCostUnderBlocks, run)
		}},
		{name: "document version, 8,001 blocks", absolute: writeCostAbsolute, take: func(t *testing.T, corpus *writeCostCorpus, run int) time.Duration {
			return writeCostVersion(t, corpus, "over.md", writeCostOverBlocks, run)
		}},
		{name: writeCostTickPath, take: writeCostTick},
	}
}

// No write path this child changes gets slower.
//
// Five write paths are measured at this child's head and at the commit this branch forked from,
// in one run, interleaved, over one corpus written to each arm by one seeding pass: a comment
// create, a comment delivery record, a document version of 1,999 blocks, one of 8,001 blocks and
// one reconciliation tick. Every reading is printed, so a swinging one is visible rather than
// hidden in a summary, and the statistic each path is held to is a real order statistic of the
// 20 readings by nearest rank - of 20 readings the p99 is the twentieth of them sorted, their
// maximum.
//
// A path whose p99 rises by more than 100 ms fails. The 8,001-block path carries a second bound,
// 1 s absolute, because IndexDocumentReferences already spends 347 ms on that version inside a
// request measured at 939 to 1,130 ms: a change inside the relative budget can still put that
// request over its own bound. The failure says which of the two bounds it broke, and an absolute
// failure with the relative one passing says so in as many words, because that case is the
// architect's to decide and not the implementer's to fix.
func TestNoWritePathThisChildChangesGetsSlower(t *testing.T) {
	t.Log(costGVisorNote)
	t.Log("how each arm is driven: both arms are server processes of their own build, each serving " +
		"a database of its own which that build migrated at boot, and each is driven over its own " +
		"HTTP API by this test, which encodes every request body before the clock starts and reads " +
		"every answer to its end before it stops. The reconciliation tick is a request on neither " +
		"arm: it is measured on each arm's own loop, from the first document that pass committed to " +
		"the last, with the pass's own lease held while the batch was written so that no tick ran " +
		"over a half-written batch.")
	for _, recorded := range writeCostRecorded {
		t.Logf("already recorded in the source tree: %s", recorded)
	}

	listener := costEnvoy(t)
	listener.Accept(true)
	head, base := startCostArms(t, listener.URL)
	arms := []*costArm{head, base}
	corpora := map[*costArm]*writeCostCorpus{}
	for _, arm := range arms {
		corpora[arm] = seedWriteCost(t, arm)
	}
	// This child's arm is measured in the state a deployment serves after the backfill: every
	// projection row beside its node, and every row of graph_store_cutover true, which is the
	// state in which graph_edges answers from the tuple arm.
	costRunBackfill(t, head)
	costCutover(t, head, true)
	for _, arm := range arms {
		costBlockIndexDrained(t, arm, 10*time.Minute)
		requireWriteCostShapes(t, corpora[arm])
		t.Logf("%s: the corpus holds %d comment(s), %d document block(s) and %d block reference(s)%s",
			arm.name,
			arm.count(t, `select count(*) from comments`),
			arm.count(t, `select count(*) from document_blocks`),
			arm.count(t, `select count(*) from block_refs`),
			costStoreCounts(t, arm))
	}
	requireWriteCostReachesTheStore(t, head, corpora[head])
	readings := map[string]map[*costArm]costReading{}
	for _, path := range writeCostPaths() {
		// The four request paths are measured with the block-index pass held off on both arms,
		// so no tick of either server's own runs inside a measured window: the versions these
		// writes leave owed are indexed after the window, not inside it. The tick path takes
		// that lease itself, for the window in which it writes its batch.
		var held []func()
		if path.name != writeCostTickPath {
			for _, arm := range arms {
				held = append(held, costHoldBlockIndex(t, arm))
			}
		}
		readings[path.name] = costInterleaved(t, path.name, "", arms, func(arm *costArm, run int) time.Duration {
			return path.take(t, corpora[arm], run)
		})
		for _, release := range held {
			release()
		}
		if path.name != writeCostTickPath {
			for _, arm := range arms {
				costBlockIndexDrained(t, arm, 10*time.Minute)
			}
		}
	}

	ordered := []costReading{}
	for _, path := range writeCostPaths() {
		ordered = append(ordered, readings[path.name][head], readings[path.name][base])
	}
	costTableOf(t, ordered)
	writeCostWhereTheTimeWent(t, arms, corpora)
	absolute := readings["document version, 8,001 blocks"][head]
	verdict := "passed"
	if absolute.p99() >= writeCostAbsolute {
		verdict = "FAILED"
	}
	t.Logf("the 8,001-block request's own absolute bound: p99 %.1f ms against %s - %s. The readings already in the "+
		"source tree, beside it: 234, 239 and 252 ms for 1,999 blocks indexed in its own write, 144, 145 and 157 ms for "+
		"2,001 blocks whose index the write defers, and 347 ms inside IndexDocumentReferences for 8,001 blocks, of a "+
		"request total of 939-1,130 ms (docs/reference_excerpts.go).",
		costMilliseconds(absolute.p99()), writeCostAbsolute, verdict)

	for _, path := range writeCostPaths() {
		here, before := readings[path.name][head], readings[path.name][base]
		rise := here.p99() - before.p99()
		relative := rise <= costBudget
		absolute := path.absolute == 0 || here.p99() < path.absolute
		t.Logf("%s: p99 %.1f ms at this child against %.1f ms at the commit before it, a rise of %.1f ms against the %s budget%s",
			path.name, costMilliseconds(here.p99()), costMilliseconds(before.p99()),
			costMilliseconds(rise), costBudget, costAbsoluteNote(path.absolute, here.p99()))
		if !relative {
			t.Errorf("%s: p99 %.1f ms at this child against %.1f ms at the commit before this child, a rise of %.1f ms, over the %s budget (this child's readings %s; the commit before's %s)",
				path.name, costMilliseconds(here.p99()), costMilliseconds(before.p99()),
				costMilliseconds(rise), costBudget, here.printed(), before.printed())
		}
		if !absolute {
			t.Errorf("%s: p99 %.1f ms at this child, at or above the %s this request is held to (this child's readings %s)",
				path.name, costMilliseconds(here.p99()), path.absolute, here.printed())
		}
		if !absolute && relative {
			t.Errorf("%s: the absolute bound failed while the relative one passed. This request costs %.1f ms at this child "+
				"and %.1f ms at the commit before this child, a rise of %.1f ms, which is inside the %s relative budget, and "+
				"what it broke is the request's own %s bound. This child did not make it slower, so the bound is the "+
				"architect's call and not an implementer's to fix.",
				path.name, costMilliseconds(here.p99()), costMilliseconds(before.p99()),
				costMilliseconds(rise), costBudget, path.absolute)
		}
	}
}

// writeCostWhereTheTimeWent prints what one version of the 1,999-block document did on each arm:
// the rows it wrote, table by table, and - on this child's arm - what writing that many nodes and
// tuples costs through the store's own writer in one transaction, against what the whole request
// costs. A p99 says a path got slower; these two numbers say whether what it pays is the volume
// of rows or a round trip per block.
//
// The rows are counted around one more version of the same document, written here and not timed;
// the store write is the same writer (graphstore.Write) over a batch of that many nodes and that
// many relations, in a transaction this rolls back, so the corpus the measurement ran over is
// left as it was.
func writeCostWhereTheTimeWent(t *testing.T, arms []*costArm, corpora map[*costArm]*writeCostCorpus) {
	t.Helper()
	for _, arm := range arms {
		corpus := corpora[arm]
		counts := []struct {
			name      string
			statement string
			head      bool
		}{
			{"document_blocks", `select count(*) from document_blocks`, false},
			{"document_block_texts", `select count(*) from document_block_texts`, false},
			{"block_refs", `select count(*) from block_refs`, false},
			{"artifact_versions", `select count(*) from artifact_versions`, false},
			{"graph_nodes", `select count(*) from graph_nodes`, true},
			{"graph_tuples", `select count(*) from graph_tuples`, true},
		}
		before := make([]int64, len(counts))
		for index, count := range counts {
			if count.head && !arm.head {
				continue
			}
			before[index] = arm.count(t, count.statement)
		}
		release := costHoldBlockIndex(t, arm)
		taken := writeCostVersion(t, corpus, "under.md", writeCostUnderBlocks, costRuns+7)
		release()
		written := []string{}
		var nodes, tuples int64
		for index, count := range counts {
			if count.head && !arm.head {
				continue
			}
			delta := arm.count(t, count.statement) - before[index]
			written = append(written, fmt.Sprintf("%s %+d", count.name, delta))
			switch count.name {
			case "graph_nodes":
				nodes = delta
			case "graph_tuples":
				tuples = delta
			}
		}
		t.Logf("%s: one more version of the 1,999-block document took %.1f ms and wrote %s",
			arm.name, costMilliseconds(taken), strings.Join(written, ", "))
		if !arm.head {
			continue
		}
		storeWrite, storeNodes, storeTuples := writeCostStoreWrite(t, arm, nodes, tuples-2*nodes)
		t.Logf("%s: writing %d node(s) and %d tuple(s) through the store's own writer in one transaction takes %.1f ms, "+
			"against the %.1f ms of the whole request. The store writer issues one statement for the nodes and one for "+
			"each kind of tuple, so what is left of the request is the rest of the pass and not a round trip for each block.",
			arm.name, storeNodes, storeTuples, costMilliseconds(storeWrite), costMilliseconds(taken))
	}
}

// writeCostStoreWrite times one store write of nodes nodes and relations relations through
// graphstore.Write, in a transaction it rolls back, and answers what it wrote.
func writeCostStoreWrite(t *testing.T, arm *costArm, nodes, relations int64) (time.Duration, int64, int64) {
	t.Helper()
	if nodes <= 0 {
		return 0, 0, 0
	}
	ctx := context.Background()
	actor := model.Actor{Kind: "user", ID: costLogin}
	at := time.Now().UTC()
	batch := make([]graphstore.Node, 0, nodes)
	for index := range int(nodes) {
		batch = append(batch, graphstore.Node{
			Kind: "block", ID: fmt.Sprintf("00000000-0000-4000-9000-%012d", index),
			Supertag: "paragraph", Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			MadeAt: at, MadeBy: actor, SourceEvent: graphstore.NoEvent,
		})
	}
	stated := make([]graphstore.Tuple, 0, max(relations, 0))
	for index := range int(max(relations, 0)) {
		stated = append(stated, graphstore.Tuple{
			FromKind: "block", FromID: batch[index%len(batch)].ID, Kind: "part_of",
			ToKind: "artifact", ToID: fmt.Sprintf("00000000-0000-4000-9001-%012d", index),
			At: at, Who: actor, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
	}
	conn, err := arm.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("%s: take a connection for the store write: %v", arm.name, err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("%s: open the store write's transaction: %v", arm.name, err)
	}
	defer tx.Rollback(ctx)
	var nodesBefore, tuplesBefore int64
	if err := tx.QueryRow(ctx, `select count(*) from graph_nodes`).Scan(&nodesBefore); err != nil {
		t.Fatalf("%s: count the nodes: %v", arm.name, err)
	}
	if err := tx.QueryRow(ctx, `select count(*) from graph_tuples`).Scan(&tuplesBefore); err != nil {
		t.Fatalf("%s: count the tuples: %v", arm.name, err)
	}
	start := time.Now()
	if err := graphstore.Write(ctx, tx, batch, stated); err != nil {
		t.Fatalf("%s: the store write: %v", arm.name, err)
	}
	taken := time.Since(start)
	var nodesAfter, tuplesAfter int64
	if err := tx.QueryRow(ctx, `select count(*) from graph_nodes`).Scan(&nodesAfter); err != nil {
		t.Fatalf("%s: count the nodes again: %v", arm.name, err)
	}
	if err := tx.QueryRow(ctx, `select count(*) from graph_tuples`).Scan(&tuplesAfter); err != nil {
		t.Fatalf("%s: count the tuples again: %v", arm.name, err)
	}
	return taken, nodesAfter - nodesBefore, tuplesAfter - tuplesBefore
}

// costAbsoluteNote is the part of a path's summary line that names its absolute bound, empty for
// a path that carries none.
func costAbsoluteNote(absolute, here time.Duration) string {
	if absolute == 0 {
		return ""
	}
	return fmt.Sprintf(", and %.1f ms against its own %s absolute bound", costMilliseconds(here), absolute)
}

// requireWriteCostReachesTheStore proves the measured paths really do this child's new work on
// this child's arm: a comment create that wrote no node, or a delivery record that appended no
// delivered_to tuple, would be measured below as costing nothing new, and the comparison would
// mean nothing.
func requireWriteCostReachesTheStore(t *testing.T, head *costArm, corpus *writeCostCorpus) {
	t.Helper()
	comments := head.count(t, `select count(*) from graph_nodes where kind = 'comment'`)
	if comments == 0 {
		t.Fatal("this child's arm holds no comment node, so the comment create measured below writes nothing of this child")
	}
	head.server.call(t, http.MethodPost, "/api/v1/comments/"+corpus.mentioned+"/deliveries",
		`{"delivery":"btw"}`, http.StatusCreated)
	sessions := head.count(t, `select count(*) from graph_nodes where kind = 'session'`)
	if sessions == 0 {
		t.Fatalf("this child's arm wrote no session node for a delivery record, so the path measured below writes nothing of this child:\n%s",
			head.server.Output.String())
	}
	t.Logf("this child's arm: the measured paths reach the store - %d comment node(s) and %d session node(s) stand before the measurement. "+
		"What a delivery record writes in its own request is the session's node and its base attributes (graphstore.Reached); the "+
		"delivered_to tuple of a delivery is the outbox publisher's, written when it records the batch of that event "+
		"(outbox.deliveredGraph), out of this request and on both arms alike.",
		comments, sessions)
}

// costStoreCounts is the store's own row counts, which only this child's arm holds.
func costStoreCounts(t *testing.T, arm *costArm) string {
	t.Helper()
	if !arm.head {
		return ", and no node and tuple store, which the commit before this child has not got"
	}
	return fmt.Sprintf(", %d node(s) and %d tuple(s)",
		arm.count(t, `select count(*) from graph_nodes`),
		arm.count(t, `select count(*) from graph_tuples`))
}

// ───── writing a corpus ───────────────────────────────────────────────────

// createCostIssue creates one issue and answers its key. The spec is forced past the near-copy
// gate: these issues are written for what hangs under them, and what they say is beside the
// point.
func createCostIssue(t *testing.T, arm *costArm, project, title string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"project": project, "title": title, "force": true,
		"spec": "# " + title + "\n\nThe corpus of one cost measurement hangs under this issue.\n",
	})
	if err != nil {
		t.Fatalf("encode the issue: %v", err)
	}
	created := arm.server.call(t, http.MethodPost, "/api/v1/issues", string(body), http.StatusCreated)
	var issue struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" {
		t.Fatalf("%s: create the issue %q: %v: %s", arm.name, title, err, created)
	}
	return issue.Key
}

// costUploadDocument writes one version of a document by name and answers its artifact id. A
// second write of the same name is the next version of that document, which is the document
// version write path.
func costUploadDocument(t *testing.T, arm *costArm, issue, name, content string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"name": name, "content": content})
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	created := arm.server.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", string(body), http.StatusCreated)
	var answer struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(created, &answer); err != nil || answer.Artifact.ID == "" {
		t.Fatalf("%s: upload %s: %v: %s", arm.name, name, err, created)
	}
	return answer.Artifact.ID
}

// costBlock is one block of a document, as GET /api/v1/artifacts/{id}/blocks answers it.
type costBlock struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// costBlocksOf reads a document's blocks as that arm answers them.
func costBlocksOf(t *testing.T, arm *costArm, artifact string) []costBlock {
	t.Helper()
	read := arm.server.call(t, http.MethodGet, "/api/v1/artifacts/"+artifact+"/blocks", "", http.StatusOK)
	var blocks []costBlock
	if err := json.Unmarshal(read, &blocks); err != nil {
		t.Fatalf("%s: read the blocks of %s: %v: %s", arm.name, artifact, err, read)
	}
	return blocks
}

func costBlockCount(t *testing.T, arm *costArm, artifact string) int {
	t.Helper()
	return len(costBlocksOf(t, arm, artifact))
}

// costNestingDepth is how many levels of block the deepest block of a document sits under, as
// the arm's own block-path read answers it: that read answers the path from the top-level block
// down, which for a bullet list is the list and its item at each level, so the levels are half
// its length. It stops at the first block deep enough for the shape under test, because reading
// the path of every block of an 8,001-block document would cost more than the measurement.
func costNestingDepth(t *testing.T, arm *costArm, artifact string) int {
	t.Helper()
	deepest := 0
	for _, block := range costBlocksOf(t, arm, artifact) {
		read := arm.server.call(t, http.MethodGet,
			"/api/v1/artifacts/"+artifact+"/blocks/"+block.ID, "", http.StatusOK)
		var answer struct {
			Path []struct {
				Type string `json:"type"`
			} `json:"path"`
		}
		if err := json.Unmarshal(read, &answer); err != nil {
			t.Fatalf("%s: read where block %s of %s stands: %v: %s", arm.name, block.ID, artifact, err, read)
		}
		deepest = max(deepest, len(answer.Path)/2)
		if deepest >= writeCostNesting {
			return deepest
		}
	}
	return deepest
}

// costPointerTargets writes the document a corpus's pointers point at and answers want of their
// addresses, each the address of one block of it.
func costPointerTargets(t *testing.T, arm *costArm, issue, name string, want int) []string {
	t.Helper()
	var text strings.Builder
	text.WriteString("# The blocks this corpus points at\n\n")
	for index := range want {
		fmt.Fprintf(&text, "Target paragraph %d of this document.\n\n", index)
	}
	artifact := costUploadDocument(t, arm, issue, name, text.String())
	address := costDocumentAddress(t, arm, artifact)
	targets := []string{}
	for _, block := range costBlocksOf(t, arm, artifact) {
		if block.Type != "paragraph" {
			continue
		}
		targets = append(targets, address+"#b-"+block.ID)
	}
	if len(targets) < want {
		t.Fatalf("%s: %s holds %d paragraph(s) to point at, want %d", arm.name, name, len(targets), want)
	}
	return targets[:want]
}

// costDocumentAddress is the dispatch:// address of a document, which is not its ref_key: an
// issue's primary document is dispatch://<issue>/spec and another of its documents sits under
// dispatch://<issue>/artifact/<slug> (refs.ArtifactRef). A pointer written at a ref_key resolves
// to nothing, and the document holding it would be measured as a document with no pointer at
// all, so this reads the fields the address is built from and builds it.
func costDocumentAddress(t *testing.T, arm *costArm, artifact string) string {
	t.Helper()
	read := arm.server.call(t, http.MethodGet, "/api/v1/artifacts/"+artifact, "", http.StatusOK)
	var answer struct {
		IssueKey *string `json:"issue_key"`
		Slug     string  `json:"slug"`
		Kind     string  `json:"kind"`
		Primary  bool    `json:"primary"`
	}
	if err := json.Unmarshal(read, &answer); err != nil || answer.IssueKey == nil || answer.Slug == "" {
		t.Fatalf("%s: read the address of %s: %v: %s", arm.name, artifact, err, read)
	}
	if answer.Primary && answer.Kind == "doc" {
		return "dispatch://" + *answer.IssueKey + "/spec"
	}
	return "dispatch://" + *answer.IssueKey + "/artifact/" + answer.Slug
}

// costMentionComment writes one comment mentioning a target and answers its id.
func costMentionComment(t *testing.T, arm *costArm, issue, body, target string) string {
	t.Helper()
	input, err := json.Marshal(map[string]any{
		"body": body, "mentions": []map[string]string{{"target": target}},
	})
	if err != nil {
		t.Fatalf("encode the mention: %v", err)
	}
	created := arm.server.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", string(input), http.StatusCreated)
	var comment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &comment); err != nil || comment.ID == "" {
		t.Fatalf("%s: create the mentioning comment: %v: %s", arm.name, err, created)
	}
	return comment.ID
}

// seedCostComments writes the earlier comments an issue holds before anything is measured on it.
func seedCostComments(t *testing.T, arm *costArm, issue string, count int) {
	t.Helper()
	for index := range count {
		body, err := json.Marshal(map[string]any{
			"body": fmt.Sprintf("Earlier comment %d, written before anything here was measured.", index),
		})
		if err != nil {
			t.Fatalf("encode the earlier comment: %v", err)
		}
		arm.server.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", string(body), http.StatusCreated)
	}
}

// ───── the documents ──────────────────────────────────────────────────────

// costDocumentText is one document of the shape these measurements state: a heading, a spine of
// bullet lists nesting deep levels deep, a paragraph between the two, and a flat list filling it
// out to blocks blocks, of whose items pointers point at blocks of another document. revision
// changes one word of every item, so a second write of the same shape is a version that really
// changed.
//
// The block count is arithmetic: the heading and the paragraph are one block each, each level of
// the spine is a list block and the paragraph of its one item, the flat list is one block and
// each of its items a paragraph. It is never taken on trust - every reading here is taken over a
// document whose blocks were read back from the server and found to be the count the shape
// states (requireWriteCostShapes).
func costDocumentText(blocks, deep, pointers int, targets []string, revision int) string {
	var text strings.Builder
	text.WriteString("# The document this run measures\n\n")
	for level := range deep {
		text.WriteString(strings.Repeat("  ", level))
		fmt.Fprintf(&text, "- nesting level %d\n", level+1)
	}
	text.WriteString("\nThe blocks below are the body of this document.\n\n")
	// The heading, the separator paragraph and the flat list's own block are the three blocks
	// beside the spine's two for each level: without the separator the flat list's items join
	// the spine's outermost list, which is one block fewer than the shape states.
	items := blocks - 3 - 2*deep
	every := 0
	if pointers > 0 {
		every = items / pointers
	}
	written := 0
	for index := range items {
		if every > 0 && index%every == 0 && written < pointers && len(targets) > 0 {
			fmt.Fprintf(&text, "- item %d of revision %d points at %s\n", index, revision, targets[written%len(targets)])
			written++
			continue
		}
		fmt.Fprintf(&text, "- item %d of revision %d\n", index, revision)
	}
	return text.String()
}
