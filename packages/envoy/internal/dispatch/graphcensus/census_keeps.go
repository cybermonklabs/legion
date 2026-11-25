package graphcensus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A Keep a release before this one wrote on a decision that carries `shaped` holds a sum over the
// decision's words with `shaped` among them, which this build's refs.DecisionWords leaves out (R32
// of dispatch://INFRA-1253's spec): read by this build, such a Keep no longer matches, and its
// decision would wait for review again though nobody changed a word of it. The Keep pass of the
// backfill (backfillKeepSums) rewrites each Keep whose stored sum is that old sum to the sum of the
// same words without `shaped`, so a Keep that matched before still matches, and leaves every other
// Keep's sum as it stands, so a Keep that no longer matched still does not. The census kind
// keep_shaped_sum (CensusOfKeepSums) counts what the pass still owes. Both are Go and no SQL,
// because only Go renders a decision's words.

// keepShapedSumKind is the census's and the backfill's name of the Keeps a release before summed
// over a decision's words with its `shaped` attribute.
const keepShapedSumKind = "keep_shaped_sum"

// KeepPassLine is the line the backfill's Keep pass logs once it has run, at info, beside the
// kind it reports (keep_shaped_sum), rewrote, the count of Keeps it rewrote, and remaining, the
// count it still owes. The boot's run of the backfill writes no pass's report anywhere
// (cmd/dispatch's startBootBackfill), so this line is what says a boot's pass ran and what it did.
const KeepPassLine = "dispatch: backfill keep pass"

// keepBackfills are the backfill's pass over the Keeps summed over a decision's words with
// `shaped` (backfillKeepSums).
var keepBackfills = []backfillPass{oneKind(backfillKeepSums)}

// keptSum is one Keep, a decision_keeps row, as the pass and the census read it: the row's key
// (artifact, block), the sum it stores, the issue its document stands under, the address of the
// decision node its block is (nil where the issue room records the block in no fragment of the
// document), that node's kept_text_sum now (nil where it holds none), and the sums of the
// decision's words (decisionread.WordSumsOf), read where decision is true: the node is an agent's
// decision whose words read back.
type keptSum struct {
	artifact, block, sum, issue string
	node, value                 *string
	decision                    bool
	sums                        decisionread.WordSums
}

// shaped reports whether the Keep is of a decision that carries `shaped`, whose words with it and
// without it are two sums.
func (k keptSum) shaped() bool {
	return k.decision && k.sums.Shaped != k.sums.Sum
}

// owed reports whether the pass owes the Keep a write: its decision carries `shaped`, and the sum
// its row stores or the kept_text_sum its node holds is the sum of the words with `shaped`.
func (k keptSum) owed() bool {
	return k.shaped() && (k.sum == k.sums.Shaped || k.value != nil && *k.value == k.sums.Shaped)
}

// keptSumColumns and keptSumFrom are the read of a Keep (keptSum) over decision_keeps k: its key,
// its sum and its document's issue; the address of its decision's node, read as the Keep reads it
// (api's decisionNodeAddress): under the document's own ref_key for a document that is its own
// room, and under the ref_key of its room's tree where the room records the block in this
// document's fragment (issue_room_blocks) for a document converted into its issue's room
// (refs.DocumentRoomSQL); and that node's kept_text_sum now, its newest tuple while it asserts
// (graphstore.CurrentValue's read).
var (
	keptSumColumns = `k.artifact_id::text, k.block_id, k.text_sha256, coalesce(d.issue_key, ''), node.address,
	       (select case when v.asserted then v.to_id end from graph_tuples v
	        where v.from_kind = '` + refs.BlockNodeKind + `' and v.from_id = node.address and v.kind = '` + refs.ValueKeptTextSum + `'
	          and v.to_kind = '` + graphstore.KindValue + `'
	        order by v.seq desc limit 1)`
	keptSumFrom = `
	from decision_keeps k
	join artifacts d on d.id = k.artifact_id
	cross join lateral (select ` + refs.DocumentRoomSQL("d.id") + ` as room) placed
	cross join lateral (
		select case when placed.room = d.id then d.ref_key || '#' || k.block_id
		            else (select tree.ref_key || '#' || b.block_id from issue_room_blocks b
		                  join artifacts tree on tree.id = b.tree_artifact_id
		                  where b.tree_artifact_id = placed.room and block_id_key(b.block_id) = block_id_key(k.block_id)
		                    and b.block_id = k.block_id and b.document_id = d.id) end as address
	) node`
)

// keptSumsAfterStatement is at most $3 Keeps past the Keep ($1, $2), in the order of
// decision_keeps' primary key, which each batch reads from the last batch's key on, so a walk reads
// each Keep once however many batches it takes. keptSumsAgainStatement is the Keeps of the keys
// $1 and $2 name, which a batch reads again under its owners' locks.
var (
	keptSumsAfterStatement = `select ` + keptSumColumns + keptSumFrom + `
	where (k.artifact_id, k.block_id) > ($1::uuid, $2::text)
	order by k.artifact_id, k.block_id
	limit $3`
	keptSumsAgainStatement = `select ` + keptSumColumns + keptSumFrom + `
	where (k.artifact_id, k.block_id) in (select * from unnest($1::uuid[], $2::text[]))
	order by k.artifact_id, k.block_id`
)

// readKeptSums reads the Keeps statement answers with args, through q, and the sums of each one's
// decision's words (decisionread.WordSumsOf), read as the run reads every node (backfillActor): a
// decision is a block of a document, which every reader may see.
func readKeptSums(ctx context.Context, q graphstore.Queryer, statement string, args ...any) ([]keptSum, error) {
	rows, err := q.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read the Keeps of the decisions: %w", err)
	}
	keeps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (keptSum, error) {
		var keep keptSum
		err := row.Scan(&keep.artifact, &keep.block, &keep.sum, &keep.issue, &keep.node, &keep.value)
		return keep, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the Keeps of the decisions: %w", err)
	}
	var nodes []decisionread.Node
	var at []int
	for index, keep := range keeps {
		if keep.node != nil {
			nodes = append(nodes, decisionread.Node{Kind: refs.BlockNodeKind, ID: *keep.node, Document: keep.artifact})
			at = append(at, index)
		}
	}
	sums, err := decisionread.WordSumsOf(ctx, q, backfillActor, nodes)
	if err != nil {
		return nil, fmt.Errorf("read the words of the kept decisions: %w", err)
	}
	for index, read := range sums {
		keeps[at[index]].decision, keeps[at[index]].sums = true, read
	}
	return keeps, nil
}

// eachKeptSum reads every Keep through q, a batch at a time in the order of decision_keeps' primary
// key (keptSumsAfterStatement), and hands each batch to visit.
func eachKeptSum(ctx context.Context, q graphstore.Queryer, visit func([]keptSum) error) error {
	artifact, block := zeroUUID, ""
	for {
		batch, err := readKeptSums(ctx, q, keptSumsAfterStatement, artifact, block, backfillBatch)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		if err := visit(batch); err != nil {
			return err
		}
		if len(batch) < backfillBatch {
			return nil
		}
		artifact, block = batch[len(batch)-1].artifact, batch[len(batch)-1].block
	}
}

// countOwedKeeps is how many Keeps the pass owes a write now (keptSum.owed), read through q.
func countOwedKeeps(ctx context.Context, q graphstore.Queryer) (int64, error) {
	var owed int64
	err := eachKeptSum(ctx, q, func(batch []keptSum) error {
		for _, keep := range batch {
			if keep.owed() {
				owed++
			}
		}
		return nil
	})
	return owed, err
}

// backfillKeepSums is the backfill's Keep pass (R32 of dispatch://INFRA-1253's spec): it walks
// every Keep, a batch at a time, and rewrites each one whose decision carries `shaped` and whose
// stored sum is the sum of its decision's words with `shaped` (keptSum.owed) to the sum of the same
// words without it, the sum every read of the decision answers now (writeKeepSumBatch). A Keep
// whose stored sum is any other - a decision changed since it was kept, or one the decisions step
// converted from a section whose text had changed - keeps it, so a Keep that did not match before
// does not match after. decision_keeps takes no marker column, so the old sum itself is the marker:
// a rewritten Keep stores the new sum, which is no decision's old sum, and a second run rewrites
// nothing. It appends no event: the kept_text_sum it states carries the sentinel and is written by
// the run (backfillActor). It reports the Keeps it rewrote as its rows and the Keeps still owed once
// the walk has ended as remaining, and logs both (KeepPassLine).
func backfillKeepSums(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: keepShapedSumKind}
	if err := eachKeptSum(ctx, database.Pool, func(batch []keptSum) error {
		var owed []keptSum
		for _, keep := range batch {
			if keep.owed() {
				owed = append(owed, keep)
			}
		}
		if len(owed) == 0 {
			return nil
		}
		rewrote, tuples, err := writeKeepSumBatch(ctx, database, owed)
		done.Rows += rewrote
		done.Tuples += tuples
		return err
	}); err != nil {
		return done, err
	}
	remaining, err := countOwedKeeps(ctx, database.Pool)
	if err != nil {
		return done, err
	}
	done.Remaining, done.Complete = remaining, remaining == 0
	slog.Info(KeepPassLine, "kind", keepShapedSumKind, "rewrote", done.Rows, "remaining", done.Remaining)
	return done, nil
}

// writeKeepSumBatch rewrites one batch's Keeps in one transaction, in the store's lock order, as
// the Keep writes one (api's keepDecisionNode): the owner rows the batch's Keeps stand under first,
// each once, in ascending key (events.LockOwnerRows), then the Keeps and their decisions' words
// read again under them (keptSumsAgainStatement), then one write phase (store.RunWrite): the
// decision_keeps rows whose sum is still the old sum, then the kept_text_sum of each node whose
// value is still the old sum (graphstore.QueueWrite, which writes no node: the decision's node is
// its writer's), then those nodes' head rows. Every writer of a decision's words and every Keep
// holds the issue's owner row, so what is read under it is what stands as the batch commits. It
// answers how many Keeps it rewrote and how many tuples it stated.
func writeKeepSumBatch(ctx context.Context, database *store.Store, batch []keptSum) (int64, int64, error) {
	var rewrote, stated int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		owners := events.OwnerRows{}
		artifacts, blocks := make([]string, len(batch)), make([]string, len(batch))
		for index, keep := range batch {
			owners.Add(keep.issue, keep.artifact)
			artifacts[index], blocks[index] = keep.artifact, keep.block
		}
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		held, err := readKeptSums(ctx, tx, keptSumsAgainStatement, artifacts, blocks)
		if err != nil {
			return fmt.Errorf("read a batch's Keeps again under their owners: %w", err)
		}
		now := time.Now().UTC()
		var rowArtifacts, rowBlocks, rowSums, rowShaped []string
		var tuples []graphstore.Tuple
		for _, keep := range held {
			if !keep.owed() {
				continue
			}
			rewrote++
			if keep.sum == keep.sums.Shaped {
				rowArtifacts = append(rowArtifacts, keep.artifact)
				rowBlocks = append(rowBlocks, keep.block)
				rowSums = append(rowSums, keep.sums.Sum)
				rowShaped = append(rowShaped, keep.sums.Shaped)
			}
			if keep.value != nil && *keep.value == keep.sums.Shaped {
				tuples = append(tuples, graphstore.Tuple{
					FromKind: refs.BlockNodeKind, FromID: *keep.node, Kind: refs.ValueKeptTextSum,
					ToKind: graphstore.KindValue, ToID: keep.sums.Sum,
					At: now, Who: backfillActor, SourceEvent: graphstore.NoEvent, Asserted: true,
				})
			}
		}
		if rewrote == 0 {
			return nil
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			if len(rowArtifacts) > 0 {
				b.Queue("rewrite the sums of the Keeps", `
					update decision_keeps k set text_sha256 = w.sum
					from unnest($1::uuid[], $2::text[], $3::text[], $4::text[]) w (artifact_id, block_id, sum, shaped)
					where k.artifact_id = w.artifact_id and k.block_id = w.block_id and k.text_sha256 = w.shaped
				`, rowArtifacts, rowBlocks, rowSums, rowShaped)
			}
			graphstore.QueueWrite(b, nil, tuples)
			graphstore.QueueHeads(b, graphstore.HeadsOf(nil, tuples))
			return nil
		}); err != nil {
			return err
		}
		stated = int64(len(tuples))
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("write a batch of Keeps' sums: %w", err)
	}
	return rewrote, stated, nil
}

// CensusOfKeepSums is the census kind keep_shaped_sum, read in tx: every Keep of an agent's
// decision that carries `shaped` (keptSum.shaped), as its rows, beside how many of their decisions'
// nodes hold a kept_text_sum, as its nodes, and how many of those Keeps the backfill's Keep pass
// still owes a write (keptSum.owed), as a class of its own (KindReport.Owed): the Keeps a release
// before this one summed over the words with `shaped`, which read as unmatched until the pass
// rewrites them. It reads zero owed once the pass has run, and its rows and nodes then stand as
// they did. A Keep the pass owes is no difference, as a row the backfill owes its node is none
// (projection.unmarked), and a deployment that holds no Keep of such a decision holds no row of it,
// which is no refusal (EmptyAgrees). The store census reads it after the projections, and a test
// reads it in its own transaction.
func CensusOfKeepSums(ctx context.Context, tx pgx.Tx) (KindReport, error) {
	kind := KindReport{
		Kind: keepShapedSumKind, NodeKind: refs.BlockNodeKind, Table: "decision_keeps",
		Note: "a Keep of an agent's decision that carries shaped: the row side is every decision_keeps row of such a" +
			" decision, the node side the kept_text_sum its node holds now, and owed the Keeps whose stored sum is the sum" +
			" of the decision's words with shaped, which the backfill's Keep pass rewrites to the sum without it",
		CountsOwed: true, EmptyAgrees: true,
	}
	if err := eachKeptSum(ctx, tx, func(batch []keptSum) error {
		for _, keep := range batch {
			if !keep.shaped() {
				continue
			}
			kind.Rows++
			if keep.value != nil {
				kind.Nodes++
			}
			if keep.owed() {
				kind.Owed++
			}
		}
		return nil
	}); err != nil {
		return KindReport{}, fmt.Errorf("census of %s: %w", keepShapedSumKind, err)
	}
	return kind, nil
}
