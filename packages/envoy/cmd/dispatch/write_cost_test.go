//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go.

package main

// The write corpus and paths a write cost tool of this tag uses: the corpus, the document
// generator, and the paths measured (writeCostPaths). #3178's check c-write-cost, which drove
// them, is gone from main; the INFRA-1098 tree's branches use writeCostNesting and
// costStoreCounts. graph_cost_test.go holds the two arms, the clock and the order statistic.
// cost_bounds_test.go, which carries no build tag, holds the 8,001-block path's allowance
// (writeCostOverAllowance) to its rule with no database and no arm.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
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
	// writeCostAbsolute is the bound the 8,001-block request carries beyond its ratio and its
	// rise (writeCostOverAllowance): IndexDocumentReferences already spends 347 ms on that
	// version inside a request measured at 939 to 1,130 ms (docs/reference_excerpts.go), so a
	// change inside the allowance this path is held to can still put that request over its own
	// bound. The run asserts all three.
	//
	// That reading was taken off gVisor. This pod runs syscall-heavy work 2.2 to 3.8 times
	// slower, and the commit before this child measures this path at 1.9 to 4.4 seconds here,
	// so a flat 1 s would fail on code this child has not touched. writeCostAbsoluteBound
	// therefore reads the bound from the environment the run measures in.
	writeCostAbsolute = time.Second
	// writeCostTickPoll is how often the tick measurement reads the owed count: the span it
	// measures is seconds, so this is resolution enough and little load of its own.
	writeCostTickPoll = 5 * time.Millisecond
	// writeCostTickWait bounds the wait for a tick to run. One comes within
	// docs.BlockIndexReconcileInterval (30 s) of the pass's lease being given back.
	writeCostTickWait = 5 * time.Minute
	// writeCostTickPath names the tick among the paths, which the run measures differently from
	// the ones that are requests, and writeCostOverPath the 8,001-block request, whose own
	// absolute bound is read beside the table: a name written out at each of those sites would
	// quietly read no reading at all the day one of them was renamed.
	writeCostTickPath = "reconciliation tick"
	writeCostOverPath = "document version, 8,001 blocks"
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
		costDocumentText(writeCostUnderBlocks, writeCostNesting, costPointerCount(writeCostUnderBlocks), corpus.targets, 0, -1))
	corpus.over = costUploadDocument(t, arm, corpus.issue, "over.md",
		costDocumentText(writeCostOverBlocks, writeCostNesting, costPointerCount(writeCostOverBlocks), corpus.targets, 0, -1))
	for index := range writeCostTickDocuments {
		corpus.tick = append(corpus.tick, costUploadDocument(t, arm, corpus.issue, writeCostTickName(index),
			costDocumentText(writeCostTickBlocks, writeCostNesting, costPointerCount(writeCostTickBlocks), corpus.targets, 0, -1)))
	}
	corpus.mentioned = costMentionComment(t, arm, corpus.issue,
		"The comment every measured delivery record is recorded against.", writeCostSession)
	seedCostComments(t, arm, corpus.issue, writeCostEarlierComments)

	// The blocks each document really holds, read back from the arm's own answer rather than
	// counted by the generator: a corpus whose shape is assumed proves nothing about the shape
	// the check states.
	corpus.underBlocks = costBlockCount(t, arm, corpus.under)
	corpus.overBlocks = costBlockCount(t, arm, corpus.over)
	// The planner chooses from statistics, and a corpus written in one burst and measured at
	// once has none: a live deployment's tables are analysed long before a person writes to
	// them. costArm.analyze says what that costs when it is skipped.
	arm.analyze(t)
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

// ───── the paths ──────────────────────────────────────────────────────────

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

// writeCostVersion is one document version write. changedOnly names the one item whose text
// this version moves, which is the edit a person makes: Dispatch carries a block's id across
// versions, so the index sees one new block and the rest unchanged. -1 instead is the whole
// document rewritten, every block new to the index, which a bulk action makes and a person
// does not. The body, which names the token of the version it replaces (documentUpload), is read
// and encoded before the clock starts.
func writeCostVersion(t *testing.T, corpus *writeCostCorpus, name string, blocks, run, changedOnly int) time.Duration {
	t.Helper()
	body := corpus.arm.server.documentUpload(t, corpus.issue, name,
		costDocumentText(blocks, writeCostNesting, costPointerCount(blocks), corpus.targets, run+1, changedOnly), costLogin)
	taken, _ := corpus.arm.timed(t, http.MethodPost, "/api/v1/issues/"+corpus.issue+"/artifacts",
		body, http.StatusCreated)
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
		body := arm.server.documentUpload(t, corpus.issue, writeCostTickName(index),
			costDocumentText(writeCostTickBlocks, writeCostNesting, costPointerCount(writeCostTickBlocks), corpus.targets, run+2, -1),
			dispatchTestLogin)
		arm.server.call(t, http.MethodPost, "/api/v1/issues/"+corpus.issue+"/artifacts", body, http.StatusCreated)
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

// The ratios the request paths are held to: each path's deciding statistic at this child over
// the same statistic at the commit before this child. The rewrite of all 1,999 blocks is held to
// what each store row it writes may cost instead (writeCostRewriteRowBudget), and the tick to no
// verdict at all. A ratio rather than a rise in milliseconds, because a rise cannot catch a path
// that gets 1.8 times slower while it stays under its budget, which is how the stated negative
// control, a part_of tuple for every block of the document, passed. What decides that control is
// the count of the tuples the edit appends (writeCostEditTuples).
//
// Each bound is the worst ratio of three clean runs of 2026-11-22 times 1.1, rounded up to the
// next 0.25. The runs are the Jobs infra-1149-i16-wc-clean1-r7zl6, infra-1149-i16-wc-clean2-75fxf
// and infra-1149-i16-wc-clean3-r5wbd, each of 772fb8e06f40 against 7a56b1c7eeeb in a builds Job
// of 8 CPUs:
//
//	path                                          statistic  clean 1  clean 2  clean 3  worst  bound
//	comment create                                p99        1.38     1.29     1.25     1.38   1.75
//	comment delivery record                       p99        0.96     0.99     0.98     0.99   1.25
//	document version, one changed block of 1,999  p50        1.48     1.16     1.16     1.48   1.75
//	document version, 8,001 blocks                p50        0.91     0.49     0.53     0.91   1.25
//
// The 8,001-block path keeps, beside its ratio, the rise of at most 100 ms (costBudget) it was
// held to before it carried one, and the lower of the two allowances decides
// (writeCostOverAllowance). That is what keeps every bound at least as strict as Verification
// version 150's, which held this path to that rise and to no ratio: a ratio allows a path of
// seconds a rise of hundreds of milliseconds, and at the 845.3 to 1,299.9 ms the commit before
// this child read on this path in the three runs above, 1.25 alone would have allowed a rise of
// 211 to 325 ms where the rise bound allows 100. Below a commit before of 400 ms the ratio is the
// lower and decides; above it the rise does. Its ratio (writeCostOverRatio), its rise
// (costBudget) and the rule that takes the lower are in cost_bounds_test.go.
//
// When a change moves a path, its bound is measured again the same way, over three clean runs,
// and never edited by hand.
const (
	writeCostCommentCreateRatio  = 1.75
	writeCostDeliveryRecordRatio = 1.25
	writeCostEditRatio           = 1.75
)

// writeCostPath is one of the paths writeCostPaths lists, as the run measures it on either arm.
type writeCostPath struct {
	name string
	// absolute is the bound this path carries beyond its ratio, zero for a path that carries
	// none.
	absolute time.Duration
	// ratio is the bound on this path's deciding statistic at this child over the same
	// statistic at the commit before this child (the ratios above), and perStoreRow is the
	// allowance for each store row the path writes, where the path is judged that way instead.
	// A rewrite makes every block of the document new to the index, so a total would refuse
	// the model rather than test it: the rows are what the model requires, and the cost of one
	// row is the store's own efficiency, which is the half that can regress.
	ratio       float64
	perStoreRow time.Duration
	// readings is how many readings this path takes on each arm, which follows what the path
	// costs. A path of tens of milliseconds takes costReadReadings, because p99 by nearest rank
	// is the ceil(0.99 x n)th and at twenty readings that is the slowest of them, so one
	// scheduling hiccup of this pod decides the cell - measured here on the comment create,
	// whose eighteen quiet readings of 17.0 to 32.1 ms were failed by two of 85.9 and 100.5. A
	// path of seconds takes costWriteRuns, because two hundred readings of a 40-second tick is
	// hours of wall time for nothing.
	readings int
	take     func(t *testing.T, corpus *writeCostCorpus, run int) time.Duration
}

func writeCostPaths() []writeCostPath {
	return []writeCostPath{
		{name: "comment create", ratio: writeCostCommentCreateRatio, readings: costReadReadings, take: writeCostCommentCreate},
		{name: "comment delivery record", ratio: writeCostDeliveryRecordRatio, readings: costReadReadings, take: writeCostDeliveryRecord},
		// The edit a person makes: one block of a 1,999-block document moved, the rest carried
		// across the version by their own ids. This is the shape Dispatch serves all day.
		{name: "document version, one changed block of 1,999", ratio: writeCostEditRatio, readings: costWriteRuns, take: func(t *testing.T, corpus *writeCostCorpus, run int) time.Duration {
			return writeCostVersion(t, corpus, "under.md", writeCostUnderBlocks, run, writeCostUnderBlocks/2)
		}},
		// The whole document rewritten, every block new to the index: a bulk action, rare and
		// started by a person who asked for it, measured apart from the edit above because a
		// path that writes four thousand node rows cannot be held to a ratio of one that writes
		// none.
		{name: "document version, 1,999 blocks rewritten", perStoreRow: writeCostRewriteRowBudget, readings: costWriteRuns, take: func(t *testing.T, corpus *writeCostCorpus, run int) time.Duration {
			return writeCostVersion(t, corpus, "under.md", writeCostUnderBlocks, run, -1)
		}},
		// The 8,001-block version is held to two allowances at once, its ratio and the 100 ms
		// rise (costBudget) it was held to before it carried a ratio, and the lower of the two
		// decides (writeCostOverAllowance), so no bound on it got weaker than Verification version
		// 150's. It carries the 1 s absolute bound beside them (writeCostAbsolute).
		{name: writeCostOverPath, absolute: writeCostAbsolute, ratio: writeCostOverRatio, readings: costWriteRuns, take: func(t *testing.T, corpus *writeCostCorpus, run int) time.Duration {
			return writeCostVersion(t, corpus, "over.md", writeCostOverBlocks, run, -1)
		}},
		{name: writeCostTickPath, readings: costWriteRuns, take: writeCostTick},
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
// It returns the store rows the head arm's rewrite wrote, which is what the rewrite path's own
// budget is read against: a rewrite makes every block of the document new to the index, so the
// rows are the model's requirement and the cost of one row is the store's own efficiency, which
// is the only half of it that can regress.
func writeCostWhereTheTimeWent(t *testing.T, arms []*costArm, corpora map[*costArm]*writeCostCorpus) (storeRows int64) {
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
		taken := writeCostVersion(t, corpus, "under.md", writeCostUnderBlocks, costWriteRuns+7, -1)
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
		storeRows = nodes + tuples
		storeWrite, storeNodes, storeTuples := writeCostStoreWrite(t, arm, nodes, tuples-2*nodes)
		t.Logf("%s: writing %d node(s) and %d tuple(s) through the store's own writer in one transaction takes %.1f ms, "+
			"against the %.1f ms of the whole request. The store writer issues one statement for the nodes and one for "+
			"each kind of tuple, so what is left of the request is the rest of the pass and not a round trip for each block.",
			arm.name, storeNodes, storeTuples, costMilliseconds(storeWrite), costMilliseconds(taken))
	}
	return storeRows
}

// writeCostWhatTheEditWrote prints, on each arm, the projection rows one edit of one block of the
// 1,999-block document wrote, each row as it stood before the edit and as it stands after: the
// document's rows of document_blocks and block_refs, the document_block_texts rows it appended,
// and on this child's arm the nodes of those blocks and the tuples it appended out of them. The
// edit is the measured path's own (writeCostVersion with one item changed), written here and not
// timed against anything. On this child's arm the tuples it appended must be the tuples of the
// changed block alone (writeCostEditTuples), and a count of any other size fails the run.
//
// The rewrite above was the document's last version, and it moved every item, so an edit written
// straight after it would move every item back. A first edit brings the document to the edit's
// shape - every item but one at revision 0 - and the second, which changes that one item again,
// is the edit whose rows are printed.
func writeCostWhatTheEditWrote(t *testing.T, arms []*costArm, corpora map[*costArm]*writeCostCorpus) {
	t.Helper()
	const changed = writeCostUnderBlocks / 2
	for _, arm := range arms {
		corpus := corpora[arm]
		release := costHoldBlockIndex(t, arm)
		writeCostVersion(t, corpus, "under.md", writeCostUnderBlocks, costWriteRuns+8, changed)
		release()
		costBlockIndexDrained(t, arm)
		before := readWriteCostEditRows(t, arm, corpus.under, nil)
		release = costHoldBlockIndex(t, arm)
		taken := writeCostVersion(t, corpus, "under.md", writeCostUnderBlocks, costWriteRuns+9, changed)
		release()
		costBlockIndexDrained(t, arm)
		after := readWriteCostEditRows(t, arm, corpus.under, &before)
		var printed strings.Builder
		for _, table := range before.tables {
			writeCostRowChanges(&printed, table, before.rows[table], after.rows[table])
		}
		for _, table := range after.appendedTables {
			rows := after.appended[table]
			fmt.Fprintf(&printed, "\n  %s: %d row(s) appended", table, len(rows))
			for index, row := range rows {
				if index == writeCostPrintedRows {
					fmt.Fprintf(&printed, "\n    and %d more", len(rows)-index)
					break
				}
				fmt.Fprintf(&printed, "\n    %s", row)
			}
		}
		t.Logf("%s: one edit of one block of the 1,999-block document took %.1f ms and wrote these rows, each before and after:%s",
			arm.name, costMilliseconds(taken), printed.String())
		// The commit before this child has no graph_tuples, so only this child's arm is held
		// to the count.
		if !arm.head {
			continue
		}
		if appended := len(after.appended["graph_tuples"]); appended != writeCostEditTuples {
			t.Errorf("one edit of one block of the 1,999-block document appended %d graph_tuples row(s), want %d: the tuples of the changed block alone%s",
				appended, writeCostEditTuples, printed.String())
		}
	}
}

// writeCostPrintedRows is how many rows of one table the edit's print shows. One edit writes a
// handful; a table past this is an edit that moved more than one block, and the count printed
// beside it says so without the run printing two thousand rows.
const writeCostPrintedRows = 20

// writeCostEditTuples is how many graph_tuples rows one edit of one block of the 1,999-block
// document appends on this child's arm: the tuples of the changed block alone. They are the
// changed item's new block's in_document, part_of, visibility and state = live, and the dropped
// block's state = gone. It was measured on 2026-11-22 in Job infra-1149-i16-rows-clean-v5zz4 at
// 772fb8e06f40, where the stated control, a part_of tuple for every block of the document (refs'
// planBlockParents), appended 2,003 in Job infra-1149-i16-rows-control-qphjc. The count is what
// decides that control, which a ratio of time may still pass: a part_of stated again for every
// block appends thousands of rows where the changed block alone appends 5.
const writeCostEditTuples = 5

// writeCostEditRows is what one document's version is read to have written. rows holds, by
// table, every row of the document that a later version may change or delete, keyed by what
// keys it in its table; appended holds the rows a table that is only appended to gained since
// the earlier read, whose marks are textVersion and tupleSeq.
type writeCostEditRows struct {
	tables, appendedTables []string
	rows                   map[string]map[string]string
	appended               map[string][]string
	nodeIDs                []string
	textVersion, tupleSeq  int64
}

// readWriteCostEditRows reads the rows of document a version writes on arm, each whole as
// to_jsonb renders it. Given the read taken before the version, it reads the rows the version
// appended since, and the nodes of the blocks either read holds, so a block the version deleted
// is read as its node now stands rather than missed.
func readWriteCostEditRows(t *testing.T, arm *costArm, document string, before *writeCostEditRows) writeCostEditRows {
	t.Helper()
	ctx := context.Background()
	read := writeCostEditRows{rows: map[string]map[string]string{}, appended: map[string][]string{}}
	keyed := func(table, statement string, args ...any) {
		rows, err := arm.pool.Query(ctx, statement, args...)
		if err != nil {
			t.Fatalf("%s: read the %s rows of %s: %v", arm.name, table, document, err)
		}
		defer rows.Close()
		held := map[string]string{}
		for rows.Next() {
			var key, row string
			if err := rows.Scan(&key, &row); err != nil {
				t.Fatalf("%s: read a %s row of %s: %v", arm.name, table, document, err)
			}
			held[key] = row
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: read the %s rows of %s: %v", arm.name, table, document, err)
		}
		read.tables = append(read.tables, table)
		read.rows[table] = held
	}
	appended := func(table, statement string, args ...any) {
		rows, err := arm.pool.Query(ctx, statement, args...)
		if err != nil {
			t.Fatalf("%s: read the %s rows %s appended: %v", arm.name, table, document, err)
		}
		defer rows.Close()
		held := []string{}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				t.Fatalf("%s: read a %s row %s appended: %v", arm.name, table, document, err)
			}
			held = append(held, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: read the %s rows %s appended: %v", arm.name, table, document, err)
		}
		read.appendedTables = append(read.appendedTables, table)
		read.appended[table] = held
	}

	keyed("document_blocks", `
		select d.block_id, to_jsonb(d)::text from document_blocks d where d.artifact_id = $1::uuid`, document)
	keyed("block_refs", `
		select r.block_id || ' ' || r.kind || ' -> ' || r.to_kind || ':' || r.to_id, to_jsonb(r)::text
		from block_refs r where r.artifact_id = $1::uuid`, document)
	read.textVersion = arm.count(t, `
		select coalesce(max(version), 0) from document_block_texts where artifact_id = $1::uuid`, document)
	if before != nil {
		appended("document_block_texts", `
			select to_jsonb(b)::text from document_block_texts b
			where b.artifact_id = $1::uuid and b.version > $2 order by b.version, b.block_id`, document, before.textVersion)
	}
	if !arm.head {
		return read
	}
	for _, row := range read.rows["document_blocks"] {
		var block struct {
			NodeID string `json:"node_id"`
		}
		if err := json.Unmarshal([]byte(row), &block); err != nil {
			t.Fatalf("%s: read a block row of %s: %v", arm.name, document, err)
		}
		read.nodeIDs = append(read.nodeIDs, block.NodeID)
	}
	nodes := read.nodeIDs
	if before != nil {
		nodes = append(slices.Clone(before.nodeIDs), nodes...)
	}
	keyed("graph_nodes", `
		select n.id, to_jsonb(n)::text from graph_nodes n where n.kind = 'block' and n.id = any($1)`, nodes)
	read.tupleSeq = arm.count(t, `select coalesce(max(seq), 0) from graph_tuples`)
	if before != nil {
		appended("graph_tuples", `
			select to_jsonb(g)::text from graph_tuples g
			where g.seq > $1 and g.from_kind = 'block' and g.from_id = any($2) order by g.seq`, before.tupleSeq, nodes)
	}
	return read
}

// writeCostRowChanges writes the rows of one table that differ between two reads of one
// document, each as it stood before and as it stands after, with how many changed, came and
// went.
func writeCostRowChanges(printed *strings.Builder, table string, before, after map[string]string) {
	keys := []string{}
	for key, row := range after {
		if before[key] != row {
			keys = append(keys, key)
		}
	}
	for key := range before {
		if _, held := after[key]; !held {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	fmt.Fprintf(printed, "\n  %s: %d row(s) written of the %d the document held", table, len(keys), len(before))
	for index, key := range keys {
		if index == writeCostPrintedRows {
			fmt.Fprintf(printed, "\n    and %d more", len(keys)-index)
			break
		}
		was, had := before[key]
		is, has := after[key]
		if !had {
			was = "(no row)"
		}
		if !has {
			is = "(no row)"
		}
		fmt.Fprintf(printed, "\n    %s\n      before %s\n      after  %s", key, was, is)
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
		"What a delivery record writes in its own request is the session's node and its base attributes (graphstore.QueueReached); the "+
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
	body := arm.server.documentUpload(t, issue, name, content, dispatchTestLogin)
	created := arm.server.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", body, http.StatusCreated)
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
// costDocumentText renders the corpus's document. revision is the number every item carries,
// which is what makes a later render change every block; changedOnly names the one item that
// takes it instead, every other item keeping revision 0, which is the edit a person makes:
// Dispatch carries a block's id across versions (pmdoc.carryBlockIDs), so only the block whose
// text moved is new to the index. costDocumentEditOnly is that second shape's name for -1's
// opposite, and -1 is the whole-document rewrite.
func costDocumentText(blocks, deep, pointers int, targets []string, revision, changedOnly int) string {
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
		at := revision
		if changedOnly >= 0 && index != changedOnly {
			at = 0
		}
		if every > 0 && index%every == 0 && written < pointers && len(targets) > 0 {
			fmt.Fprintf(&text, "- item %d of revision %d points at %s\n", index, at, targets[written%len(targets)])
			written++
			continue
		}
		fmt.Fprintf(&text, "- item %d of revision %d\n", index, at)
	}
	return text.String()
}

// writeCostAbsoluteBound is the absolute bound of the 8,001-block path in the environment the
// run measures in, with the words that say where it came from. The stated bound was read off
// gVisor; this pod runs syscall-heavy work 2.2 to 3.8 times slower, and where the commit before
// this child already measures above the stated bound on this path, holding this child to it
// would fail it for the pod rather than for the change. The path then carries no absolute
// bound at all - zero, which is what the caller reads as "unbounded" - and its ratio and its
// rise alone (writeCostOverAllowance) decide whether this child made the path slower. Answering
// that commit's own reading instead would hold this child to being faster than it, which is a
// bound nobody stated.
func writeCostAbsoluteBound(before costReading) (time.Duration, string) {
	if before.p50() <= writeCostAbsolute {
		return writeCostAbsolute, fmt.Sprintf("the stated %s, which the commit before this child measures inside at %.1f ms",
			writeCostAbsolute, costMilliseconds(before.p50()))
	}
	return 0, fmt.Sprintf("no absolute bound - the ratio and the rise alone decide. The commit before this child, which this "+
		"change has not touched, already measures %.1f ms here, above the stated %s: this pod runs syscall-heavy work 2.2 to "+
		"3.8 times slower than the machine the stated bound was read on",
		costMilliseconds(before.p50()), writeCostAbsolute)
}
