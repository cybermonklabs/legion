package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// runOwed is the from and the where of the runs whose row names no interaction node, which
// backfillRuns reads in batches and counts once it is done.
const runOwed = `
		from going_requests r
		where not exists (select 1 from graph_nodes n where n.kind = '` + graphstore.KindInteraction + `' and n.id = r.id::text)`

// runRowless is the from and the where of the interaction nodes of the source going_requests
// (graphstore.SourceGoingRequests), not gone, whose row is gone (graphstore.RowlessRunSQL), which
// backfillGoneRuns reads in batches and counts once it is done. A node of another source - a turn
// of a History conversation, whose row is in another table - is no run of the command bar, and
// neither this pass nor the sweep states it gone.
var runRowless = `
		from graph_nodes n
		where ` + graphstore.RowlessRunSQL

// owedBatches reads, by read, the keys a pass owes past after, in batches of backfillBatch, and
// hands each batch that holds a key to write, until a batch comes back short. read is one text
// column in the order the keys compare in, past $1 and at most $2 of them, so the last key of a
// batch is where the next one starts. what names the keys in the error of a read that fails.
func owedBatches(ctx context.Context, database *store.Store, read, what, after string, write func(owed []string) error) error {
	for {
		rows, err := database.Pool.Query(ctx, read, after, backfillBatch)
		if err != nil {
			return fmt.Errorf("read %s: %w", what, err)
		}
		owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("read %s: %w", what, err)
		}
		if len(owed) > 0 {
			if err := write(owed); err != nil {
				return err
			}
		}
		if len(owed) < backfillBatch {
			return nil
		}
		after = owed[len(owed)-1]
	}
}

// backfillRuns gives every run going_requests holds and the store holds no interaction node of its
// node, through the one writer of such a node from its row (graphstore.WriteRun): the node, its
// author's node and tuples, and a frame value of every frame its row holds, in the shape the going
// POST and the frame path write them. It reads the runs owed in batches by id, and writes each in
// a transaction of its own, which takes the run's row first and reads again whether the node is
// there, so a run the frame path or another backfill gave its node meanwhile is left as it is.
func backfillRuns(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: graphstore.KindInteraction}
	err := owedBatches(ctx, database, owedRunsRead, "the runs the store holds no node of", uuid.Nil.String(), func(owed []string) error {
		for _, id := range owed {
			written, err := writeOwedRun(ctx, database, id)
			if err != nil {
				return err
			}
			if written.Nodes > 0 {
				done.Rows++
				done.Nodes += written.Nodes
				done.Tuples += written.Tuples
			}
		}
		return nil
	})
	if err != nil {
		return done, err
	}
	if err := database.Pool.QueryRow(ctx, `select count(*)`+runOwed).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the runs the store holds no node of: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// owedRunsRead reads the runs past $1, in id order, whose row names no interaction node, at most
// $2 of them (owedBatches).
const owedRunsRead = `
		select r.id::text` + runOwed + ` and r.id > $1::uuid
		order by r.id
		limit $2`

// writeOwedRun writes the node of one run in a transaction of its own (graphstore.WriteRun).
func writeOwedRun(ctx context.Context, database *store.Store, id string) (graphstore.RunWritten, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return graphstore.RunWritten{}, fmt.Errorf("begin the node of run %s: %w", id, err)
	}
	defer tx.Rollback(ctx)
	written, err := graphstore.WriteRun(ctx, tx, id)
	if err != nil {
		return graphstore.RunWritten{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return graphstore.RunWritten{}, fmt.Errorf("commit the node of run %s: %w", id, err)
	}
	return written, nil
}

// backfillRunFrames states a frame value of every going_events row whose run's interaction holds
// no frame value of that sequence (graphstore.FrameHeldSQL): a frame a process of the release
// before appended to a run this build or the passes before gave a node. Each owed run is written in
// a transaction of its own (writeRunFrames). Rows counts the frames it stated and Tuples the
// values, one each, with what graphstore.WriteRun stated of a run that had no node by then.
func backfillRunFrames(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: graphstore.AttributeFrame}
	err := owedBatches(ctx, database, owedFrameRunsRead, "the runs holding a frame the store holds no value of", uuid.Nil.String(),
		func(owed []string) error {
			for _, run := range owed {
				frames, written, err := writeRunFrames(ctx, database, run)
				if err != nil {
					return err
				}
				done.Rows += frames
				done.Tuples += frames + written.Tuples
				done.Nodes += written.Nodes
			}
			return nil
		})
	if err != nil {
		return done, err
	}
	var left int64
	if err := database.Pool.QueryRow(ctx, `select count(*) from going_events e where not `+graphstore.FrameHeldSQL).Scan(&left); err != nil {
		return done, fmt.Errorf("count the frames the store holds no value of: %w", err)
	}
	done.Remaining = left
	done.Complete = left == 0
	return done, nil
}

// owedFrameRunsRead reads the runs past $1, in id order, that hold a frame the store holds no
// value of, at most $2 of them (owedBatches). The runs are grouped and ordered by the uuid itself,
// the order $1 compares in, so the read takes going_events' (request_id, sequence) key in its
// order.
const owedFrameRunsRead = `
		select e.request_id::text from going_events e
		where e.request_id > $1::uuid and not ` + graphstore.FrameHeldSQL + `
		group by e.request_id
		order by e.request_id
		limit $2`

// writeRunFrames states, in one transaction, a frame value of each frame of run the store holds no
// value of, and answers how many. It takes the run's interaction node row first
// (graphstore.LockInteraction), as every frame of this build does (api's appendGoingEvent), so a
// frame this build is appending to the run commits first, value and row together, and the read
// under the lock no longer counts it owed. A run with no node by then gets it from its row through
// graphstore.WriteRun, which states every frame the row holds, and which it answers. The values are
// read off the rows (graphstore.ReadRunFrames), exactly as the frame path states them.
func writeRunFrames(ctx context.Context, database *store.Store, run string) (int64, graphstore.RunWritten, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, graphstore.RunWritten{}, fmt.Errorf("begin the frames of run %s: %w", run, err)
	}
	defer tx.Rollback(ctx)
	locked, err := graphstore.LockInteraction(ctx, tx, run)
	if err != nil {
		return 0, graphstore.RunWritten{}, err
	}
	if !locked {
		written, err := graphstore.WriteRun(ctx, tx, run)
		if err != nil {
			return 0, graphstore.RunWritten{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, graphstore.RunWritten{}, fmt.Errorf("commit the node of run %s: %w", run, err)
		}
		return 0, written, nil
	}
	frames, err := graphstore.ReadRunFrames(ctx, tx, run, true)
	if err != nil {
		return 0, graphstore.RunWritten{}, err
	}
	if len(frames) == 0 {
		return 0, graphstore.RunWritten{}, nil
	}
	if err := graphstore.Write(ctx, tx, nil, frames); err != nil {
		return 0, graphstore.RunWritten{}, fmt.Errorf("state the frames of run %s: %w", run, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, graphstore.RunWritten{}, fmt.Errorf("commit the frames of run %s: %w", run, err)
	}
	return int64(len(frames)), graphstore.RunWritten{}, nil
}

// backfillGoneRuns states gone every interaction node of the source going_requests, not gone
// already, whose going_requests row is gone (runRowless): a run a process of the release before
// deleted an hour old, frames with it, whose node it left live, as the sweep does once the node is
// an hour old (api's goRunGone). The store keeps the node, and the census reads a gone node with no
// row as no difference.
func backfillGoneRuns(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "interaction_gone"}
	err := owedBatches(ctx, database, rowlessRunsRead, "the runs whose row is gone", "", func(owed []string) error {
		for _, id := range owed {
			stated, err := writeGoneRun(ctx, database, id)
			if err != nil {
				return err
			}
			if stated {
				done.Rows++
				done.Tuples++
			}
		}
		return nil
	})
	if err != nil {
		return done, err
	}
	if err := database.Pool.QueryRow(ctx, `select count(*)`+runRowless).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the runs whose row is gone: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// rowlessRunsRead reads the interaction nodes past $1, in id order, not gone and whose row is
// gone, at most $2 of them (owedBatches).
var rowlessRunsRead = `
		select n.id` + runRowless + ` and n.id > $1
		order by n.id
		limit $2`

// writeGoneRun states the interaction node id gone where its row is still gone, in a transaction
// of its own, and answers whether it did. It takes the node's row first, as a frame of the run
// does, then reads under that lock the node and whether it is still a run whose row is gone
// (graphstore.RowlessRunSQL): the going POST commits a run's node and its row together, so a node
// it reads has its row whenever the run has one. The write is graphstore.WriteState of the node,
// made by the run (backfillActor) now, as api's goRunGone states a run's node gone.
func writeGoneRun(ctx context.Context, database *store.Store, id string) (bool, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin the end of run %s: %w", id, err)
	}
	defer tx.Rollback(ctx)
	if _, err := graphstore.LockInteraction(ctx, tx, id); err != nil {
		return false, err
	}
	node, held, err := graphstore.ReadNode(ctx, tx, graphstore.KindInteraction, id)
	if err != nil || !held {
		return false, err
	}
	var rowless bool
	if err := tx.QueryRow(ctx, `select `+graphstore.RowlessRunSQL+` from graph_nodes n where n.kind = $1 and n.id = $2`,
		graphstore.KindInteraction, id).Scan(&rowless); err != nil {
		return false, fmt.Errorf("read whether run %s is still a run whose row is gone: %w", id, err)
	}
	if !rowless {
		return false, nil
	}
	if _, err := graphstore.WriteState(ctx, tx, []graphstore.Node{node}, graphstore.StateGone, time.Now().UTC(), backfillActor); err != nil {
		return false, fmt.Errorf("state run %s gone: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit the end of run %s: %w", id, err)
	}
	return true, nil
}
