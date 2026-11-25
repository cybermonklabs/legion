package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// wakeWriteRetry is how long the daemon waits before it makes again a write that keeps its record
// of a paced wake true and that failed (writeAgainUntilMade): long enough that a database which
// stopped answering is not asked every second, and short beside the hour Envoy keeps a role's
// message.
const wakeWriteRetry = 15 * time.Second

// writeAgainUntilMade makes again a write that has just failed, every wakeWriteRetry until it
// succeeds or ctx ends, telling failed each further failure. It is for the writes that follow a
// change in the wake pacing's queues and that nothing else makes before the next boot: the join
// and the let-out that give a held catch-up its due moment (catchUpPacing), and the stop and the
// start of a resume's stuck clock (projectPause.writeResumeClock). write bounds its own try. It
// blocks, so a caller inside a machine's decision, a commit's hook or the pacer's turn runs it on
// a goroutine of its own.
func writeAgainUntilMade(ctx context.Context, write func(context.Context) error, failed func(error)) {
	for backoff.Sleep(ctx, wakeWriteRetry) == nil {
		err := write(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		failed(err)
	}
}

// catchUpPacing is the place in the daemon's wake pacing of each catch-up the workflow engine
// wrote held (workflow.Config.HoldCatchUp, dispatch://INFRA-5682 R1). The catch-up a start writes
// in place of a launch is a wake of its architect, which reads its whole context when the
// catch-up arrives, so it waits its turn with the launches, in the pacer's second queue
// (wakepace.Second). Its row stands in the outbox with no due moment (record.CatchUpHeldAt) until
// the pacer lets its place out, and that let-out makes the row due at the let-out's moment
// (record.ReleaseHeldCatchUp) and wakes its lane.
//
// The row is the durable half and the place the half in memory. A restart loses every place, and
// the boot joins each row still held again, in the order of their ids, before supervision places
// a relaunch (restore). A row that is dropped while it waits gives its place up (drop), and the
// turn it would have taken goes to the wake behind it, since a waiting place has drawn nothing of
// the limiter.
type catchUpPacing struct {
	// ctx ends when the daemon stops, which ends the writes this type makes again.
	ctx  context.Context
	pool *store.Pool
	// pacer is the daemon's one wake pacer (run), and nil while the operator paces nothing: the
	// engine then writes no catch-up held, and a row a daemon that paced left held is made due at
	// once (join).
	pacer *wakepace.Pacer
	now   func() time.Time
	// wake wakes the lane of an issue's notices once its row is due (workflowRuntime.wakeOutbox).
	wake func(string, record.OutboxKind)
	log  *slog.Logger

	mu sync.Mutex
	// places is each held row's place, by the row's id. A nil place is a row whose join is under
	// way and has taken no place yet: a drop that lands meanwhile takes the entry out, which is
	// how the join learns that its row went.
	places map[int64]*wakepace.Place
}

// hold is workflow.Config.HoldCatchUp: the held row id of issue joins the pacing, in the hook of
// the commit of the fact that wrote it, so a fact that rolls back spends no turn.
func (p *catchUpPacing) hold(id int64, issue string) {
	join := func(ctx context.Context) error { return p.join(ctx, id, issue) }
	p.again("queue a held catch-up", id, issue, join(p.ctx), join)
}

// again is what a write of this type that failed comes to: err is logged, and write is made again
// on a goroutine of its own until it lands (writeAgainUntilMade). Nothing else gives a held row a
// due moment before the next boot, and every later notice for its architect waits behind it
// (outbox.deliverNotice), so neither a join nor a let-out is given up after one try.
func (p *catchUpPacing) again(what string, id int64, issue string, err error, write func(context.Context) error) {
	if err == nil || p.ctx.Err() != nil {
		return
	}
	failed := func(err error) {
		p.log.Warn("wake: could not "+what+"; trying again", "row", id, "issue", issue, "in", wakeWriteRetry, "error", err)
	}
	failed(err)
	go writeAgainUntilMade(p.ctx, write, failed)
}

// join takes the held row id a place at the back of the pacer's second queue and binds the row's
// release to it. A place the pacer lets out at once releases the row here. A row that already
// holds a place, or whose join is under way, joins nothing.
//
// The row is read first, and joins only while it is still held: a drop of the row that committed
// before this join began left no row, and one that lands while the read runs takes the entry out
// of places, so a row that is gone takes no place and spends no turn. mu is not held across the
// read, because a reader of the daemon's state holds a connection while it asks for an estimate
// (estimate), and the read needs one.
//
// A daemon that paces nothing has no queue to join, and makes the row due at once.
func (p *catchUpPacing) join(ctx context.Context, id int64, issue string) error {
	if p.pacer == nil {
		return p.release(ctx, id, issue, wakepace.Let{At: p.now()})
	}
	p.mu.Lock()
	if _, joining := p.places[id]; joining {
		p.mu.Unlock()
		return nil
	}
	if p.places == nil {
		p.places = make(map[int64]*wakepace.Place)
	}
	p.places[id] = nil
	p.mu.Unlock()

	read, cancel := context.WithTimeout(store.ForConcurrentWorker(ctx), readyReleaseTimeout)
	held, err := store.ReadOnly(read, p.pool, "read whether a catch-up is still held", func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return record.CatchUpHeld(ctx, tx, id)
	})
	cancel()
	p.mu.Lock()
	_, joining := p.places[id]
	if err != nil || !held || !joining {
		delete(p.places, id)
		p.mu.Unlock()
		return err
	}
	place := p.pacer.Join(wakepace.Second)
	p.places[id] = place
	p.mu.Unlock()

	letOut := func(let wakepace.Let) { p.letOut(id, issue, let) }
	if let, out := place.Bind(letOut); out {
		letOut(let)
	}
	return nil
}

// letOut is the release of a held row's place: the row is made due at the let-out's moment and
// its place is forgotten. It runs on the goroutine of the pacer's turn, or on the joiner's where
// the pacer let the place out at once, so a write that fails is made again on a goroutine of its
// own (again): the place is spent, and nothing else would let the row out.
func (p *catchUpPacing) letOut(id int64, issue string, let wakepace.Let) {
	release := func(ctx context.Context) error {
		if err := p.release(ctx, id, issue, let); err != nil {
			return err
		}
		p.drop([]int64{id})
		return nil
	}
	p.again("let out a held catch-up", id, issue, release(p.ctx), release)
}

// release makes the held row id due at the let-out let, recording how the pacer let it out for the
// line its wake writes (record.ReleaseHeldCatchUp), and wakes the row's lane once that commits. A
// row that was dropped or released already is left as it is, and wakes nothing.
func (p *catchUpPacing) release(ctx context.Context, id int64, issue string, let wakepace.Let) error {
	ctx, cancel := context.WithTimeout(store.ForConcurrentWorker(ctx), readyReleaseTimeout)
	defer cancel()
	return store.Transact(ctx, p.pool, func(ctx context.Context, tx pgx.Tx) error {
		moved, err := record.ReleaseHeldCatchUp(ctx, tx, id, let.At, let.Turns)
		if moved && p.wake != nil {
			store.OnCommit(ctx, func() { p.wake(issue, record.OutboxKindNotice) })
		}
		return err
	})
}

// drop forgets the places of the rows ids, giving up each that still waits: the rows a newer
// catch-up superseded (projectRecords.DropCatchUps, in the hook of that drop's commit), and a row
// its let-out has just released. A join under way for one of them finds its entry gone and takes
// no place.
func (p *catchUpPacing) drop(ids []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range ids {
		if place := p.places[id]; place != nil {
			place.Leave()
		}
		delete(p.places, id)
	}
}

// estimate is the moment the pacer estimates it lets the held row id out
// (wakepace.Place.Estimate), and the zero time for a row that holds no place: one whose join is
// under way, or one this daemon does not hold. A nil catchUpPacing holds none.
func (p *catchUpPacing) estimate(id int64) time.Time {
	if p == nil {
		return time.Time{}
	}
	p.mu.Lock()
	place := p.places[id]
	p.mu.Unlock()
	if place == nil {
		return time.Time{}
	}
	return place.Estimate()
}

// restore is the boot's pass over the catch-ups the previous daemon left held: each joins the
// pacer's second queue again, in the order of their ids, before supervision places a relaunch, so
// a catch-up that was waiting before the restart is not put behind the restart's own backlog. A
// daemon whose operator has turned the pacing off makes each due at once instead (join).
func (p *catchUpPacing) restore(ctx context.Context, records record.Store, project string) error {
	rows, err := store.ReadOnly(ctx, p.pool, "read held catch-ups", func(ctx context.Context, tx pgx.Tx) ([]record.OutboxRow, error) {
		return records.WaitingNotices(ctx, tx, project, record.CatchUpHeldAt.Add(-time.Microsecond))
	})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.NextAt.Equal(record.CatchUpHeldAt) {
			if err := p.join(ctx, row.ID, row.Issue); err != nil {
				return fmt.Errorf("restore held catch-up %d: %w", row.ID, err)
			}
		}
	}
	return nil
}
