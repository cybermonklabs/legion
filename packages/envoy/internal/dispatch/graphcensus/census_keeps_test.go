package graphcensus

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// keptDecision is one decision seedShapedDecisions wrote: its block id, its node's address, the sum
// of its words as this build sums them, without its shaped attribute, and the words with shaped
// and their sum, as a release before this one summed them.
type keptDecision struct {
	block, address, sum, shaped, shapedWords string
}

// keepKeeper is who keeps every decision the Keep pass's tests seed.
var keepKeeper = model.Actor{Kind: "user", ID: "alice"}

// seedShapedDecisions writes version 1 of the issue document artifactID, as its version write
// writes it: requirement r1, and for each of words a decision d<n> saying it whose shaped names
// pointer, r1's address, indexed as the write indexes it (refs.DecisionFacts and refs.IndexBlocks,
// at the sentinel). It answers each decision with its sums: without shaped, as refs.DecisionWords
// renders its words, and with it, rendered as a release before this one rendered a decision's
// words, the decision alone through pmdoc.Render with the readings it closes with left out and its
// shaped attribute kept.
func seedShapedDecisions(t *testing.T, database *store.Store, artifactID, pointer string, words ...string) []keptDecision {
	t.Helper()
	ctx := context.Background()
	markdown := ":::requirement{#r1}\nThe store keeps every decision.\n:::\n"
	for index, said := range words {
		markdown += fmt.Sprintf("\n:::decision{#d%d shaped=\"%s\"}\n%s\n:::\n", index+1, pointer, said)
	}
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the decisions' document: %v", err)
	}
	pmdoc.EnsureBlockIDs(tree)
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		t.Fatalf("render the decisions' document: %v", err)
	}
	blocks := make([]model.ArtifactBlock, len(offsets))
	for index, offset := range offsets {
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To}
	}
	var refKey string
	var at time.Time
	if err := database.Pool.QueryRow(ctx, `
		with version as (
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1::uuid, 1, $2, '[{"kind":"user","id":"alice"}]') returning created_at)
		select (select ref_key from artifacts where id = $1::uuid), created_at from version
	`, artifactID, rendered).Scan(&refKey, &at); err != nil {
		t.Fatalf("write version 1 of the decisions' document: %v", err)
	}
	sources := refs.TypedSources{Authors: func(ids []string) (map[string]model.Actor, error) {
		authors := make(map[string]model.Actor, len(ids))
		for _, id := range ids {
			authors[id] = keepKeeper
		}
		return authors, nil
	}}
	drivenTx(t, database, "index version 1 of the decisions' document", func(tx pgx.Tx) error {
		versions := refs.OneVersion(artifactID, 1, at)
		record, err := refs.ReadIndexedVersion(ctx, tx, artifactID, artifactID, 1)
		if err != nil {
			return err
		}
		facts, err := refs.DecisionFacts(ctx, tx, artifactID, record, versions, blocks, tree, nil, sources)
		if err != nil {
			return err
		}
		_, _, stated, _, err := refs.IndexBlocks(ctx, tx, artifactID, versions, rendered, blocks, nil, nil, facts)
		if err != nil {
			return err
		}
		return graphstore.Caused(ctx, tx, stated, graphstore.NoEvent)
	})
	read, err := pmdoc.ParseRendering(rendered)
	if err != nil {
		t.Fatalf("read the decisions' document back: %v", err)
	}
	kept := make([]keptDecision, len(words))
	for index := range words {
		id := fmt.Sprintf("d%d", index+1)
		var block *pmdoc.Node
		pmdoc.Walk(read, func(node *pmdoc.Node) bool {
			if node.Type == refs.DecisionSupertag && refs.TypedBlockID(node) == id {
				block = node
			}
			return block == nil
		})
		if block == nil {
			t.Fatalf("the decisions' document holds no decision %s:\n%s", id, rendered)
		}
		own, err := refs.DecisionWords(block)
		if err != nil {
			t.Fatalf("render the words of %s: %v", id, err)
		}
		content, _ := pmdoc.TypedContent(block)
		whole := *block
		whole.Children = content
		shaped, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{&whole}})
		if err != nil {
			t.Fatalf("render the words of %s with shaped: %v", id, err)
		}
		if strings.Contains(own, "shaped=") || !strings.Contains(shaped, "shaped=") {
			t.Fatalf("%s's words read %q, and with shaped %q: want shaped in the second alone", id, own, shaped)
		}
		kept[index] = keptDecision{
			block: id, address: refs.BlockAddress(refKey, id), sum: decisionread.TextSum(own),
			shaped: decisionread.TextSum(shaped), shapedWords: shaped,
		}
	}
	return kept
}

// keepAsTheReleaseBefore keeps decision as alice the way a release before this one kept it: the
// decision_keeps row of its block and the kept_by and kept_text_sum on its node, each holding
// stored.
func keepAsTheReleaseBefore(t *testing.T, database *store.Store, artifactID string, decision keptDecision, stored string) {
	t.Helper()
	ctx := context.Background()
	keptAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	drivenTx(t, database, "keep "+decision.block+" as a release before kept it", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by, kept_at)
			values ($1::uuid, $2, $3, 1, $4, $5)
		`, artifactID, decision.block, stored, keepKeeper.ID, keptAt); err != nil {
			return err
		}
		return graphstore.Write(ctx, tx, []graphstore.Node{graphstore.PersonNode(keepKeeper.ID, keepKeeper, keptAt, graphstore.NoEvent)},
			[]graphstore.Tuple{{
				FromKind: refs.BlockNodeKind, FromID: decision.address, Kind: refs.TupleKeptBy,
				ToKind: graphstore.KindPerson, ToID: keepKeeper.ID, At: keptAt, Who: keepKeeper, SourceEvent: graphstore.NoEvent, Asserted: true,
			}, {
				FromKind: refs.BlockNodeKind, FromID: decision.address, Kind: refs.ValueKeptTextSum,
				ToKind: graphstore.KindValue, ToID: stored, At: keptAt, Who: keepKeeper, SourceEvent: graphstore.NoEvent, Asserted: true,
			}})
	})
}

// seedKeepIssue writes issue LOCK-1 of project LOCK and its spec, as this build writes them,
// before any decision of it, and answers the spec's id and the address of its block r1.
func seedKeepIssue(t *testing.T, database *store.Store) (document, pointer string) {
	t.Helper()
	seedLockProject(t, database)
	drivenTx(t, database, "write issue LOCK-1 and its spec", func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('LOCK-1', 'LOCK', 1, 'Kept', '{"kind":"user","id":"alice"}', 'U')
		`); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('LOCK-1', 'LOCK', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
			returning id::text
		`).Scan(&document)
	})
	issue := "LOCK-1"
	return document, refs.BlockRef(refs.ArtifactRef(&issue, "LOCK", nil, "spec", "doc", true), "r1")
}

// storedKeep is what decision's Keep stores now: its decision_keeps row's sum and its node's
// current kept_text_sum, "" where it holds none.
func storedKeep(t *testing.T, database *store.Store, artifactID string, decision keptDecision) (row, value string) {
	t.Helper()
	ctx := context.Background()
	if err := database.Pool.QueryRow(ctx, `select text_sha256 from decision_keeps where artifact_id = $1::uuid and block_id = $2`,
		artifactID, decision.block).Scan(&row); err != nil {
		t.Fatalf("read the Keep of %s: %v", decision.block, err)
	}
	value, _, err := graphstore.CurrentValue(ctx, database.Pool, refs.BlockNodeKind, decision.address, refs.ValueKeptTextSum)
	if err != nil {
		t.Fatal(err)
	}
	return row, value
}

// requireKeepSumCensus holds the census kind keep_shaped_sum, as the store census reads it, to
// rows Keeps of a decision that carries shaped, owed of them owed a write by the Keep pass.
func requireKeepSumCensus(t *testing.T, database *store.Store, when string, rows, owed int64) {
	t.Helper()
	kind := censusKind(t, storeCensusOf(t, database), keepShapedSumKind)
	if kind.Owed != owed {
		t.Errorf("%s: census kind %s reads %d, want %d", when, keepShapedSumKind, kind.Owed, owed)
	}
	if kind.Rows != rows || kind.Nodes != rows || kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
		t.Errorf("%s: census kind %s reads %d rows, %d nodes and %d and %d differences, want %d Keeps of a decision that carries shaped, each its node's kept_text_sum, and no difference",
			when, keepShapedSumKind, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences, rows)
	}
}

// A Keep a release before this one made on a decision that carries shaped holds a sum over the
// decision's words with shaped (R32 of the issue-tree spec). The Keep pass rewrites the
// Keep of d1, whose stored sum is that sum over its words as they stand, to the sum this build
// reads, in its decision_keeps row and its node's kept_text_sum, so d1 still reads kept and
// waiting for no review; it leaves d2, whose stored sum is over words d2 no longer holds, as it
// stands, so d2 still waits for review. The census kind keep_shaped_sum reads 1 before the pass,
// d1's Keep, and 0 after it, of the two Keeps of decisions that carry shaped. A second run
// rewrites nothing.
func TestTheKeepPassRewritesTheKeepsThatMatchedOverTheWordsWithShaped(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document, pointer := seedKeepIssue(t, database)
	decisions := seedShapedDecisions(t, database, document, pointer, "We keep the node store.", "We shard the log.")
	d1, d2 := decisions[0], decisions[1]
	if d1.sum == d1.shaped {
		t.Fatalf("d1's words sum to %s with shaped and without it: this case proves nothing", d1.sum)
	}
	stale := decisionread.TextSum(strings.Replace(d2.shapedWords, "shard", "split", 1))
	keepAsTheReleaseBefore(t, database, document, d1, d1.shaped)
	keepAsTheReleaseBefore(t, database, document, d2, stale)
	requireKeepSumCensus(t, database, "before the Keep pass", 2, 1)

	done, err := backfillKeepSums(ctx, database)
	if err != nil {
		t.Fatalf("run the Keep pass: %v", err)
	}
	if done.Rows != 1 || done.Tuples != 1 || done.Remaining != 0 || !done.Complete {
		t.Errorf("the Keep pass rewrote %d Keeps, want 1 (its report %+v)", done.Rows, done)
	}
	if row, value := storedKeep(t, database, document, d1); row != d1.sum || value != d1.sum {
		t.Errorf("the Keep of %s stores %s and the kept_text_sum %s after the Keep pass, want %s, the sum of its words without shaped",
			d1.block, row, value, d1.sum)
	}
	if row, value := storedKeep(t, database, document, d2); row != stale || value != stale {
		t.Errorf("the Keep pass rewrote the Keep of %s, which did not match: %s to %s and %s", d2.block, stale, row, value)
	}
	read, err := decisionread.Decisions(ctx, database.Pool, keepKeeper, []decisionread.Node{
		{Kind: refs.BlockNodeKind, ID: d1.address}, {Kind: refs.BlockNodeKind, ID: d2.address},
	})
	if err != nil || len(read) != 2 {
		t.Fatalf("read d1 and d2: %d decisions, %v", len(read), err)
	}
	if read[0].TextSHA256 != d1.sum || read[0].WaitingReview || read[0].Kept == nil || read[0].Kept.KeptBy != keepKeeper.ID {
		t.Errorf("the Keep of %s reads waiting_review %t after the Keep pass, its sum %s and its Keep %+v, want %s kept by alice",
			d1.block, read[0].WaitingReview, read[0].TextSHA256, read[0].Kept, d1.sum)
	}
	if read[1].TextSHA256 != d2.sum || !read[1].WaitingReview {
		t.Errorf("the Keep of %s reads waiting_review %t after the Keep pass, its sum %s, want true: its Keep did not match",
			d2.block, read[1].WaitingReview, read[1].TextSHA256)
	}
	requireKeepSumCensus(t, database, "after the Keep pass", 2, 0)

	again, err := backfillKeepSums(ctx, database)
	if err != nil {
		t.Fatalf("run the Keep pass again: %v", err)
	}
	if again.Rows != 0 || again.Tuples != 0 || !again.Complete {
		t.Errorf("a second Keep pass rewrote %d Keeps, want 0 (its report %+v)", again.Rows, again)
	}
}

// TestTheKeepSumBatchTakesTheStoreLockOrder holds writeKeepSumBatch (R32 of the issue-tree
// spec) to the store's lock order, as TestEveryBackfillBatchTakesTheStoreLockOrder holds the other
// batches: the owner row the Keep stands under, its issue's row, first, then the Keep's
// decision_keeps row. The kept_text_sum is stated at the sentinel, which no repeat key holds, so no
// hold stands on it, and the batch takes no project row. The Keep is one a release before this one
// made of d1, which carries shaped, over its words with shaped. After the run the Keep stores the
// sum of d1's words without shaped, and the backfill reports it rewritten.
func TestTheKeepSumBatchTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	document, pointer := seedKeepIssue(t, database)
	d1 := seedShapedDecisions(t, database, document, pointer, "We keep the node store.")[0]
	keepAsTheReleaseBefore(t, database, document, d1, d1.shaped)

	owner, project := holdLockOwner(t, database)
	row := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the Keep's decision_keeps row", Step: lockstep.StepProjection, Taken: true,
		Statement: "update decision_keeps",
	}, `select 1 from decision_keeps where artifact_id = $1::uuid and block_id = $2 for update`, document, d1.block)
	report := followTheRun(t, database, "writeKeepSumBatch", onePass(backfillKeepSums), []*lockstep.Hold{owner, project, row})
	if stored, value := storedKeep(t, database, document, d1); stored != d1.sum || value != d1.sum {
		t.Errorf("the Keep of %s stores %s and the kept_text_sum %s, want %s: the run the test followed is not one that committed",
			d1.block, stored, value, d1.sum)
	}
	if done := reportedKind(t, report, keepShapedSumKind); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for the Keeps, want 1 Keep, 1 tuple and none left", done)
	}
}
