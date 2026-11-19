package graphcensus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// WriteRun is the backfill's writer for one run of the command bar: it states in the store, in
// the caller's transaction, the run a going_requests row holds where the store holds no
// interaction node of it, which a pod of the release before writes during a roll, recording a run
// with no node and appending its frames with no frame value. It writes, from the row and the run's
// going_events rows, exactly what the going POST and the frame path write of a run
// (graphstore.InteractionFacts, graphstore.FrameTuple): the interaction node, its words and its
// ends_at, its authored_by tuple with its author's own node where the row's author names somebody,
// and one frame value for each going_events row of the run, in sequence, in one graphstore.Write.
// A row whose author names nobody gets a node reserved for an author nobody proves, which nobody
// reads, as the stream reads that row.
//
// id names the run as going_requests.id reads it; the node is keyed by the row's own id::text, the
// lowercase form the POST writes. It answers whether the record holds the run: false where it holds
// no such row, and nothing is written then. A run whose node the store holds already is left as it
// is, answering true: its node and every frame written with it are another write's, and this
// writer mends nothing.
//
// It takes the run's going_requests row `for no key update` before it reads whether the node is
// there, so two writers of one run - two frames this build writes of a run an older pod recorded,
// or the backfill beside one - queue at that row, and the second, reading in a statement of its
// own once the first has committed, finds the node there and writes nothing. The lock conflicts
// with no insert of a frame: a going_events row's foreign key takes the run's row `for key share`.
// It is the step the frame path (api's lockGoingRun) takes for a run with no node, before it
// takes the node's row. The node this writes is its transaction's own, which no other transaction
// can see until it commits, let alone hold; a node another write committed first, which this
// writes nothing of, may be held by then - an erase of the run takes the node's row and then the
// run's - so the frame takes that node's row without waiting and, where another holds it, lets its
// transaction go and begins again, taking the node's row first.
func WriteRun(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	written, err := writeRun(ctx, tx, id)
	return written.held, err
}

// runWritten is what writeRun did for one run: whether the record holds the run, and the nodes
// and tuples it stated of it, none where the store held its node already. The counts are the upper
// bound the backfill's report states: an author's node another write already made moves nothing,
// and its base attributes are stated only by the write that made it.
type runWritten struct {
	held          bool
	nodes, tuples int64
}

// writeRun is WriteRun answering what it wrote, which the backfill's pass over the runs an older
// pod recorded counts (backfillRuns).
func writeRun(ctx context.Context, tx pgx.Tx, id string) (runWritten, error) {
	var run graphstore.Interaction
	var author []byte
	err := tx.QueryRow(ctx, `
		select id::text, author, project, words, created_at, ends_at
		from going_requests
		where id = $1::uuid
		for no key update
	`, id).Scan(&run.ID, &author, &run.Project, &run.Words, &run.At, &run.EndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return runWritten{}, nil
	}
	if err != nil {
		return runWritten{}, fmt.Errorf("read run %s: %w", id, err)
	}
	_, stored, err := graphstore.ReadNode(ctx, tx, graphstore.KindInteraction, run.ID)
	if err != nil {
		return runWritten{}, err
	}
	if stored {
		return runWritten{held: true}, nil
	}
	if err := json.Unmarshal(author, &run.Author); err != nil {
		return runWritten{}, fmt.Errorf("decode the author of run %s: %w", run.ID, err)
	}
	nodes, tuples := graphstore.InteractionFacts(run)
	frames, err := readRunFrames(ctx, tx, run.ID, false)
	if err != nil {
		return runWritten{}, err
	}
	if err := graphstore.Write(ctx, tx, nodes, append(tuples, frames...)); err != nil {
		return runWritten{}, fmt.Errorf("write the interaction node of run %s: %w", run.ID, err)
	}
	written := runWritten{held: true, nodes: int64(len(nodes)), tuples: int64(len(tuples) + len(frames))}
	for _, node := range nodes {
		// The visibility and the state of each node it made, and its project where it names one.
		written.tuples += 2
		if node.Project != "" {
			written.tuples++
		}
	}
	return written, nil
}

// readRunFrames reads a frame value of each going_events row of run, in sequence, as the frame
// path states one (graphstore.FrameTuple): its text read off the row in SQL
// (graphstore.FrameValueSQL), dated at the row's created_at. owedOnly leaves out each frame the
// run's interaction holds a value of already (runFrameHeld), the frames the backfill owes
// (writeRunFrames); writeRun reads them all, for a run whose node it makes.
func readRunFrames(ctx context.Context, tx pgx.Tx, run string, owedOnly bool) ([]graphstore.Tuple, error) {
	owed, problem := "", "read the frames of run %s: %w"
	if owedOnly {
		owed, problem = ` and not `+runFrameHeld, "read the frames of run %s the store holds no value of: %w"
	}
	rows, err := tx.Query(ctx, `
		select `+graphstore.FrameValueSQL("e.sequence", "e.name", "e.data")+`, e.created_at
		from going_events e
		where e.request_id = $1::uuid`+owed+`
		order by e.sequence
	`, run)
	if err != nil {
		return nil, fmt.Errorf(problem, run, err)
	}
	frames, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphstore.Tuple, error) {
		var value string
		var at time.Time
		if err := row.Scan(&value, &at); err != nil {
			return graphstore.Tuple{}, err
		}
		return graphstore.FrameTuple(run, value, at), nil
	})
	if err != nil {
		return nil, fmt.Errorf(problem, run, err)
	}
	return frames, nil
}
