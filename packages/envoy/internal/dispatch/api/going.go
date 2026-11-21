package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A run's record is rows and not process memory because the POST that asks a question and the
// GET that streams its answer are two HTTP requests, which a deployment of more than one pod
// spreads across pods (conn_age.go: kube-proxy and the front door spread load per connection,
// not per request). Every read and write of that record is here; the routes that call them are
// their own.
//
// A run is an interaction node of the node and tuple store (spec requirement r-interaction),
// and its two tables are that node's projections: going_requests projects the node, its author
// column the node's made_by, and each going_events row projects one frame value of it. Every
// writer here writes the node or the value first and then the row, in one transaction
// (recordGoingRequest, appendGoingEvent), so no row stands without its node and no frame without
// its value. The stream reads the rows, as it always did: they are the cache it reads at speed.
//
// A run is kept, its row, its frames and its node, until `envoy-dispatch erase --node
// interaction:<uuid>` erases all three in one transaction (grapherase): a node of the store is
// removed by erasure alone (spec requirement r-interaction). Nothing here deletes a
// run. During a roll, and after a rollback, a process of the release before still deletes a run
// an hour old (contracts.GoingRequestTTL, which this build reads nowhere); the backfill states its
// node gone (graphcensus's backfillGoneRuns).
//
// The window a run has belongs to the record: going_requests.ends_at, written by the POST. A pod
// that dies mid-run stores no terminal frame, so a reader on another pod ends the stream itself
// once the run is past that moment with nothing terminal stored (readGoingStream),
// sweepGoingRequests ends the runs nobody reads, each in the day after its deadline
// (goingSweepWindow), and the boot pass, once at each boot of a process, every older one
// (EndGoingRunsOverdueAtBoot). At the deadline the running answer, a reader and a pass may each
// try to end one run, so which of them ends it is settled by the table (the going_events_terminal
// index of migration 20261105152945) and never by whichever is first: every terminal write is an
// insert that does nothing on a conflict, and the writer that loses reads the stored terminal
// frame and reports that one (endGoingRun).

// goingRequestColumns is the going_requests select list scanGoingRequest reads, in scan order.
const goingRequestColumns = `id::text, author, project, words, created_at, ends_at`

// goingEventColumns is the going_events select list scanGoingEvent reads, in scan order.
const goingEventColumns = `sequence, name, data`

// goingTerminalNames are the two frame names that end a stream (going.Event.Terminal), the pair
// the going_events_terminal index holds as literals.
var goingTerminalNames = []string{contracts.GoingEventEnd, contracts.GoingEventError}

// goingTerminalWrites bounds how many sequences one terminal write tries. Every frame this build
// writes of a run queues at the run's interaction node (appendGoingEvent), so a terminal write
// loses its sequence only to a frame a pod of the release before wrote during a roll, which takes
// no such lock: to a terminal frame, which ends the attempt with that stored frame, or to a frame
// of the run's own, after which the next sequence is free. Two such losses in a row would need
// that pod to be writing faster than this write's round trips; the bound is there so a caller
// cannot spin, not because a third attempt is expected to be needed.
const goingTerminalWrites = 3

// errGoingRunGone is a frame of a run the record no longer holds: one nobody asked, one an erase
// removed, or one a process of the release before deleted an hour old during a roll, whose node
// may still stand. It stores nothing. It wraps pgx.ErrNoRows, which is what a frame the record
// stores none of answers its writer, so an answer still writing reads its run as over either way;
// endGoingRun answers it rather than trying another sequence, and the sweep passes over it.
var errGoingRunGone = fmt.Errorf("the record holds no such run: %w", pgx.ErrNoRows)

// goingRequest is one answer run as the record holds it: the question a reader asked, and the
// window the run has to answer it in.
type goingRequest struct {
	ID        string      `json:"id"`
	Author    model.Actor `json:"author"`
	Project   string      `json:"project"`
	Words     string      `json:"words"`
	CreatedAt time.Time   `json:"created_at"`
	EndsAt    time.Time   `json:"ends_at"`
}

// goingEvent is one stored frame of a run's stream: the frame, and the sequence it holds, which
// the stream writes as the frame's id so a reader that reconnects resumes after it.
type goingEvent struct {
	Sequence int
	Event    going.Event
}

// goingRunState is what a reader needs to decide whether a stream is over: the sequence of the
// run's one terminal frame, 0 while the run has stored none, and whether it is past its ends_at,
// after which a run that stored none is ended by whoever reads it. The terminal frame is a
// sequence and not a bool because a reader ends its stream on having been written that frame and
// not on the run having stored one: a terminal frame committed between a reader's read of the
// frames and its read of this state would otherwise close a stream that never carried it. The
// going_events_terminal index holds that there is at most one such frame, so one sequence says
// all of it.
type goingRunState struct {
	Terminal     int
	PastDeadline bool
}

// recordGoingRequest records one question: the asking actor, the project it was asked under (the
// empty string for every project, as search reads its own filter) and the words of the question,
// and answers the stored row. ends_at is created_at plus contracts.GoingRunWindow: both come
// from one now(), the transaction's own, so the window is exactly that long.
//
// It writes, in its one transaction, the run's interaction node and then the going_requests row
// that projects it (graphstore.InteractionFacts): the node at the run's own uuid, reserved for its
// author, made by the whole credential the POST proved and carrying the run's words, its ends_at
// and its project, with its authored_by tuple and its author's own person or session node and
// alias tuples where graphstore.AuthorOf names somebody of that credential; then the row, whose
// author column is the node's made_by, the same recorded actor in the same encoding. The uuid and
// the two moments are read first, in one statement, so the node is written before the row whose
// columns they are, and the answer is the row as that statement and the request name it, read
// back from nowhere. createGoingRequest refuses an asker who proves nobody, so every run the POST
// records has an author; a run whose author is nobody, which only a direct call with model.Actor{}
// writes, gets a node nobody reads and no authored_by, as the stream reads such a row. The write
// takes no owner row, as a run belongs to no issue and no document, and appends no event.
//
// The insert is made in a transaction of the shared pool rather than as bare statements, because
// the POST that calls it is a keyed write: the commit step marks the request's Idempotency-Key
// committed with the write itself, which is what answers a repeat after a crash KEY_COMMITTED
// rather than recording the question a second time (keyed_write_guard_test.go).
func (s *server) recordGoingRequest(ctx context.Context, author model.Actor, project, words string) (goingRequest, error) {
	run := graphstore.Interaction{Author: author.Recorded(), Project: project, Words: words}
	stored, err := json.Marshal(run.Author)
	if err != nil {
		return goingRequest{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return goingRequest{}, err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `select gen_random_uuid()::text, now(), now() + make_interval(secs => $1)`,
		contracts.GoingRunWindow.Seconds()).Scan(&run.ID, &run.At, &run.EndsAt); err != nil {
		return goingRequest{}, err
	}
	nodes, tuples := graphstore.InteractionFacts(run)
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		return goingRequest{}, fmt.Errorf("write the interaction node of run %s: %w", run.ID, err)
	}
	if _, err := tx.Exec(ctx, `
		insert into going_requests (id, author, project, words, created_at, ends_at)
		values ($1::uuid, $2::jsonb, $3::text, $4::text, $5, $6)
	`, run.ID, stored, project, words, run.At, run.EndsAt); err != nil {
		return goingRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return goingRequest{}, err
	}
	return goingRequest{ID: run.ID, Author: run.Author, Project: project, Words: words, CreatedAt: run.At, EndsAt: run.EndsAt}, nil
}

// readGoingRequest is one run by id, pgx.ErrNoRows when the record holds none: a run that was
// never asked, one an erase removed, or one a process of the release before deleted. What a run
// the caller does not own answers is the caller's to decide, which is why the row carries its
// Author: model.Actor.SameCredential compares it with the reader's.
func (s *server) readGoingRequest(ctx context.Context, id string) (goingRequest, error) {
	return scanGoingRequest(s.deps.Store.Pool.QueryRow(ctx, `
		select `+goingRequestColumns+` from going_requests where id = $1::uuid
	`, id))
}

// readLiveGoingRun is the run id names as a write made for it reads it (withinGoingRun): its
// recorded author, and whether it is live, now() < ends_at, the database's own clock against the
// window the POST wrote from that clock, whatever frames the run has stored. id is the run's uuid
// in the lowercase text its interaction node is keyed by (uuid.UUID.String), the one spelling of
// it the caller sends on, and no other: Postgres reads some of the spellings uuid.Parse admits and
// refuses the URN form (SQLSTATE 22P02). pgx.ErrNoRows where the record holds no such run.
func (s *server) readLiveGoingRun(ctx context.Context, id string) (model.Actor, bool, error) {
	var stored []byte
	var live bool
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select author, now() < ends_at from going_requests where id = $1::uuid
	`, id).Scan(&stored, &live); err != nil {
		return model.Actor{}, false, err
	}
	var author model.Actor
	if err := json.Unmarshal(stored, &author); err != nil {
		return model.Actor{}, false, fmt.Errorf("decode going request author: %w", err)
	}
	return author, live, nil
}

// goingTerminalNamesSQL is goingTerminalNames as the SQL list the going_events_terminal index's
// predicate writes, `'end', 'error'`: a read that names the two as literals is one the planner
// proves that partial index answers, where a bound array leaves a prepared statement's generic
// plan nothing to prove it by.
var goingTerminalNamesSQL = `'` + strings.Join(goingTerminalNames, `', '`) + `'`

// goingNextFrame reads what a frame of the run $1 takes, once the run's node is held: the next
// sequence, the frame's value at that sequence (graphstore.FrameValueSQL over the frame's name $2
// and its JSON $3), whether the run has stored a terminal frame, and the transaction's now(), which
// the frame's going_events row takes as its created_at. The sequence and the ending are one index
// probe each, never a read of every frame the run holds: its newest sequence, read backward on the
// (request_id, sequence) key, and its one terminal frame on going_events_terminal, which the
// literal names (goingTerminalNamesSQL) let the planner use (TestTheNextFrameReadsTwoIndexProbes).
// It takes the run's going_requests row `for key share`, the lock the frame's own row's foreign
// key would take, so a delete of the run - an erase's, or a process of the release before deleting
// it an hour old - waits for the frame rather than failing its insert; a run the record no longer
// holds answers no row.
var goingNextFrame = `
	select next.sequence,
	       ` + graphstore.FrameValueSQL("next.sequence", "$2::text", "$3::jsonb") + `,
	       next.ended,
	       now()
	from going_requests r
	cross join lateral (
		select coalesce((select max(e.sequence) from going_events e where e.request_id = r.id), 0) + 1 as sequence,
		       exists (select 1 from going_events e
		               where e.request_id = r.id and e.name in (` + goingTerminalNamesSQL + `)) as ended
	) next
	where r.id = $1::uuid
	for key share of r`

// goingFrameRow writes a frame's going_events row at the sequence goingNextFrame read. It does
// nothing on a conflict: with the run's terminal frame (going_events_terminal), or with a frame at
// that sequence, either of which a pod of the release before, which takes no node lock, may have
// committed in between.
const goingFrameRow = `
	insert into going_events (request_id, sequence, name, data)
	values ($1::uuid, $2, $3::text, $4::jsonb)
	on conflict do nothing
	returning sequence`

// appendGoingEvent stores event as the run's next frame and answers the sequence it took. It is
// the one path every frame of a run is written by: the answer's own frames, its ending and the
// deadline's ending a reader or the sweep writes, which all three reach through endGoingRun.
// requestID is the run's uuid in the lowercase text its interaction node is keyed by, which every
// caller holds: the id Postgres wrote, or the stream's path id, which canonicalizeUUIDPath has
// spelled so.
//
// Each frame is a transaction of its own. It first takes the run's interaction node row
// `for no key update` (lockGoingRun), so two frames of one run queue there and the second reads
// the sequence the first took. It then takes the run's row and the next sequence in one statement
// (goingNextFrame), whose snapshot is taken once that lock is granted. The two are sent one after
// the other and not as one batch: a connection that prepares a batch's statements before it runs
// the first would take the run's going_requests table, a later step of the lock order, before the
// node's row. A frame of a run with no node that finds, under the run's row, a node another
// transaction committed and holds lets its transaction go and begins again once
// (errGoingRunNodeHeld), so it takes that node's row first and holds nothing while it waits there.
// Last, in one round trip, it appends the frame as one `frame` value tuple of the node holding
// that sequence, the frame's name and its data (graphstore.FrameTuple, queued as graphstore.Write
// appends it by graphstore.QueueFrameTuple), and then writes the going_events row (goingFrameRow);
// the two are queued in that order, the order their tables' locks are taken in. A row that
// conflicts rolls the whole transaction back, the value with it, so no frame stands without its
// row and no row without its frame. It takes no revision fence: that belongs to the block index
// writer alone.
//
// A frame the record stores none of answers pgx.ErrNoRows, which tells an answer still writing
// that its run is over: a run that has stored its terminal frame takes no other, a frame whose
// row conflicts stores nothing, and a run the record no longer holds is errGoingRunGone, which
// wraps it. Only the process running the answer appends a frame that does not end the stream; a
// terminal frame goes through endGoingRun, which the table settles.
//
// The transaction is the frame's alone and none of a request's own work, so it takes no commit
// step a keyed request's context carries (store.WithoutBeforeCommit): the answer's context is
// derived from its POST's, and a frame that committed while that POST was still answering would
// otherwise mark the POST's key, or be rolled back with a key a repeat had taken over.
func appendGoingEvent(ctx context.Context, pool *store.Pool, requestID string, event going.Event) (int, error) {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return 0, err
	}
	tx, err := beginGoingFrame(ctx, pool, requestID)
	if errors.Is(err, errGoingRunNodeHeld) {
		tx, err = beginGoingFrame(ctx, pool, requestID)
	}
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var sequence int
	var value string
	var ended bool
	var at time.Time
	err = tx.QueryRow(ctx, goingNextFrame, requestID, event.Name, data).Scan(&sequence, &value, &ended, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errGoingRunGone
	}
	if err != nil {
		return 0, fmt.Errorf("read the next frame of run %s: %w", requestID, err)
	}
	if ended {
		return 0, pgx.ErrNoRows
	}
	batch := &pgx.Batch{}
	if err := graphstore.QueueFrameTuple(batch, graphstore.FrameTuple(requestID, value, at)); err != nil {
		return 0, fmt.Errorf("append frame %d of run %s: %w", sequence, requestID, err)
	}
	batch.Queue(goingFrameRow, requestID, sequence, event.Name, data)
	results := tx.SendBatch(ctx, batch)
	if _, err := results.Exec(); err != nil {
		_ = results.Close()
		return 0, fmt.Errorf("append frame %d of run %s: %w", sequence, requestID, err)
	}
	rowErr := results.QueryRow().Scan(&sequence)
	if err := results.Close(); rowErr == nil {
		rowErr = err
	}
	if rowErr != nil {
		return 0, rowErr
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return sequence, nil
}

// beginGoingFrame begins one frame's transaction and takes the run's node in it (lockGoingRun),
// and answers that transaction. A transaction it could not lock the node in is rolled back before
// it answers the error.
func beginGoingFrame(ctx context.Context, pool *store.Pool, requestID string) (pgx.Tx, error) {
	tx, err := pool.Begin(store.WithoutBeforeCommit(ctx))
	if err != nil {
		return nil, fmt.Errorf("begin a frame of run %s: %w", requestID, err)
	}
	if err := lockGoingRun(ctx, tx, requestID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// errGoingRunNodeHeld is lockGoingRun's answer for a run whose node another write committed after
// the frame found none, and which another transaction holds: the frame holds the run's
// going_requests row by then, and the erase of a run takes the node's row and then the run's
// (grapherase's lockProjectionRows), so a frame that waited for that node there would wait on the
// erase while the erase waited on it. appendGoingEvent begins the frame again, which takes the
// node's row first and nothing before it.
var errGoingRunNodeHeld = errors.New("the run's interaction node is held by another transaction")

// goingRunNodeTaken is graphstore.NodeRowLockSQL taken without waiting: a row another transaction
// holds is skipped, and no row is taken.
const goingRunNodeTaken = graphstore.NodeRowLockSQL + ` skip locked`

// lockGoingRun takes the interaction node row of the run requestID names `for no key update`
// (graphstore.LockInteraction), the first lock of every frame's transaction, so a frame takes a
// node's row before the run's going_requests row, the order the erase of a run takes them in. A
// run a pod of the release before recorded during a roll has no node yet: its node is written
// first, from its going_requests row, by the one writer of a run's node from its row, which the
// backfill shares (graphstore.WriteRun): it takes that row `for no key update` and also states a
// frame value of every frame that pod stored, and the node is then taken. The node it writes is
// this transaction's own, which no other can see until it commits, let alone hold. A node another
// write committed in between is taken without waiting (goingRunNodeTaken), and where another
// transaction holds it the answer is errGoingRunNodeHeld, which the caller begins again on, so no
// frame waits on a node's row while it holds the run's. A run the record does not hold is
// errGoingRunGone.
func lockGoingRun(ctx context.Context, tx pgx.Tx, requestID string) error {
	locked, err := graphstore.LockInteraction(ctx, tx, requestID)
	if err != nil || locked {
		return err
	}
	written, err := graphstore.WriteRun(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if !written.Held {
		return errGoingRunGone
	}
	taken, err := tx.Exec(ctx, goingRunNodeTaken, graphstore.KindInteraction, requestID)
	if err != nil {
		return fmt.Errorf("lock the interaction node of run %s: %w", requestID, err)
	}
	if taken.RowsAffected() == 0 {
		return errGoingRunNodeHeld
	}
	return nil
}

// endGoingRun ends the run with event, a terminal frame, and answers the terminal frame the run
// actually has: the one this call stored, or the one already stored when another writer ended
// the run first. A run is ended once, by the table and not by whoever got there first, so a
// writer that loses reports the stored frame rather than storing its own at the next sequence:
// the answer, the reader at the deadline and the sweep then all say the same thing about one
// run. event must be terminal (going.Event.Terminal); anything else would end no run and is
// refused here rather than stored as a frame no reader stops on. A run the record no longer
// holds is errGoingRunGone.
func endGoingRun(ctx context.Context, pool *store.Pool, requestID string, event going.Event) (goingEvent, error) {
	if !event.Terminal() {
		return goingEvent{}, fmt.Errorf("end going run %s: %s is not a terminal event", requestID, event.Name)
	}
	for range goingTerminalWrites {
		sequence, err := appendGoingEvent(ctx, pool, requestID, event)
		if err == nil {
			return goingEvent{Sequence: sequence, Event: event}, nil
		}
		if errors.Is(err, errGoingRunGone) || !errors.Is(err, pgx.ErrNoRows) {
			return goingEvent{}, err
		}
		stored, err := readGoingTerminalEvent(ctx, pool, requestID)
		if err == nil {
			return stored, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return goingEvent{}, err
		}
		// The sequence went to a frame of the run's own, not to another terminal write: the next
		// one is free.
	}
	return goingEvent{}, fmt.Errorf("end going run %s: %d writes each lost their sequence", requestID, goingTerminalWrites)
}

// readGoingTerminalEvent is the one terminal frame a run stored, pgx.ErrNoRows while it has
// stored none. The going_events_terminal index holds that there is at most one.
func readGoingTerminalEvent(ctx context.Context, pool *store.Pool, requestID string) (goingEvent, error) {
	return scanGoingEvent(pool.QueryRow(ctx, `
		select `+goingEventColumns+`
		from going_events
		where request_id = $1::uuid and name = any($2::text[])
	`, requestID, goingTerminalNames))
}

// goingEventsAfter is every frame of one run past a sequence, in stream order: $1 the run, $2
// that sequence; scanGoingEvent decodes each row. It is named rather than written inline
// because readGoingStream queues it in a batch beside the state read.
const goingEventsAfter = `
	select ` + goingEventColumns + `
	from going_events
	where request_id = $1::uuid and sequence > $2
	order by sequence`

// goingRunStateOf reads one run's state, $1 the run and $2 the terminal frame names: the
// sequence of its terminal frame, 0 while it has stored none, and whether now is past its
// ends_at. Queued beside goingEventsAfter, for the reason it is named.
const goingRunStateOf = `
	select coalesce(
	         (select e.sequence from going_events e where e.request_id = r.id and e.name = any($2::text[])),
	         0
	       ),
	       now() >= r.ends_at
	from going_requests r
	where r.id = $1::uuid`

// readGoingStream is what one poll of an open stream reads: the frames past after, and the run's
// state, in one round trip (store.Pool.SendBatch), where a poll of a stream costs two. Either
// statement failing is the whole read's failure, which ends the stream, and pgx.ErrNoRows is the
// run deleted under its reader, by an erase or by a process of the release before.
//
// The two statements take their own snapshots, each at its own moment, so a frame committed
// between them is in one answer and not the other. Which order they run in therefore settles
// nothing, and it is immaterial: a terminal frame in the frames ends the stream on being
// written, and a terminal sequence in the state ends it only once the reader has reached that
// sequence (streamGoingAnswer), so neither order can close a stream that never carried its
// ending.
func (s *server) readGoingStream(ctx context.Context, requestID string, after int) ([]goingEvent, goingRunState, error) {
	var frames []goingEvent
	var state goingRunState
	batch := &pgx.Batch{}
	batch.Queue(goingEventsAfter, requestID, after).Query(func(rows pgx.Rows) error {
		collected, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (goingEvent, error) {
			return scanGoingEvent(row)
		})
		frames = collected
		return err
	})
	batch.Queue(goingRunStateOf, requestID, goingTerminalNames).QueryRow(func(row pgx.Row) error {
		return row.Scan(&state.Terminal, &state.PastDeadline)
	})
	if err := s.deps.Store.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return nil, goingRunState{}, err
	}
	return frames, state, nil
}

// scanGoingRequest decodes one goingRequestColumns row.
func scanGoingRequest(row pgx.Row) (goingRequest, error) {
	var request goingRequest
	var author []byte
	if err := row.Scan(&request.ID, &author, &request.Project, &request.Words, &request.CreatedAt, &request.EndsAt); err != nil {
		return goingRequest{}, err
	}
	if err := json.Unmarshal(author, &request.Author); err != nil {
		return goingRequest{}, fmt.Errorf("decode going request author: %w", err)
	}
	return request, nil
}

// scanGoingEvent decodes one goingEventColumns row. The payload stays the JSON the row holds
// (json.RawMessage, which marshals to exactly those bytes) rather than one of going's payload
// types: the frame a reader is written is then the frame that was stored, whatever wrote it, and
// nothing here needs a second switch over the event names to pick a type to decode into.
func scanGoingEvent(row pgx.Row) (goingEvent, error) {
	var event goingEvent
	var data []byte
	if err := row.Scan(&event.Sequence, &event.Event.Name, &data); err != nil {
		return goingEvent{}, err
	}
	event.Event.Data = json.RawMessage(data)
	return event, nil
}

// goingSweepWindow is how far back from now the sweep reads a run's ends_at (goingOverdueRuns): a
// run whose deadline passed longer ago than this is not read by it at all. The window exists
// because a run is kept until an erase removes it, so going_requests holds every run ever asked
// and grows with each, and the sweep reads that table once a maintenance pass, every minute
// (MaintenanceSweepInterval). Read over every overdue run, its cost would grow with the whole of
// that history; bounded, it reads through going_requests_ends_at (migration
// 20261116232634_going_requests_ends_at_index) the runs whose deadline passed in the last day,
// however many runs are kept. A day is far longer than the sweep needs to reach a run: a run past
// its ends_at is inside the window for some 1,440 passes, any one of which ends it. A run that
// leaves the window with no ending is one no pass ended in that day, every process of the
// deployment down for as long, and what ends it then is the boot pass, at the next boot of any
// process (EndGoingRunsOverdueAtBoot), or its reader, if one reads it first: the stream ends a
// run past its ends_at with no terminal frame stored when it reads it (streamGoingAnswer, on
// readGoingStream's PastDeadline).
const goingSweepWindow = 24 * time.Hour

// goingOverdueRead is the read of the runs a pass ends, $1 the terminal frame names: every run
// whose ends_at has passed, within bound, with no terminal frame stored, oldest deadline first.
// Each pass's read is named rather than written inline so that its plan is held to
// going_requests_ends_at (TestTheOverdueReadsGoThroughTheEndsAtIndex).
func goingOverdueRead(bound string) string {
	return `
	select r.id::text
	from going_requests r
	where r.ends_at <= now()` + bound + `
	  and not exists (select 1 from going_events e where e.request_id = r.id and e.name = any($1::text[]))
	order by r.ends_at, r.id`
}

// goingOverdueRuns is the sweep's read of the runs it ends, $2 goingSweepWindow in seconds: every
// run whose ends_at passed within that window of now. Both bounds key going_requests_ends_at, so
// the read is that index's range for the last day and not a scan of every run kept; an older run
// is the boot pass's (goingEveryOverdueRun).
var goingOverdueRuns = goingOverdueRead(`
	  and r.ends_at > now() - make_interval(secs => $2)`)

// goingEveryOverdueRun is the boot pass's read of the runs it ends (EndGoingRunsOverdueAtBoot):
// every run whose ends_at has passed, at any age. It is goingOverdueRuns without the window's
// lower bound, so it reads going_requests_ends_at from its first entry up to now, every run kept
// whose deadline has passed, which is why it runs once at a boot and never once a pass.
var goingEveryOverdueRun = goingOverdueRead("")

// sweepGoingRequests ends every run whose ends_at passed within goingSweepWindow of now with no
// terminal frame stored, with the deadline's error (contracts.GoingErrorAnswerDidNotArrive), once
// a maintenance pass (RunMaintenanceSweep). Ending those runs is what nothing else does: the pod
// that was answering one may have died, and nobody may be reading it. An older run is the boot
// pass's, or its reader's, as goingSweepWindow says.
// It deletes no run: a run is kept, row, frames and node, until an erase removes it
// (spec requirement r-interaction), where the release before deleted every run past
// contracts.GoingRequestTTL. That value is read nowhere in this build.
func sweepGoingRequests(ctx context.Context, pool *store.Pool) error {
	_, err := endOverdueGoingRuns(ctx, pool, goingOverdueRuns, goingTerminalNames, goingSweepWindow.Seconds())
	return err
}

// EndGoingRunsOverdueAtBoot is the boot pass: once at each boot of a process, before the first
// tick of RunMaintenanceSweep (cmd/dispatch runs the two in that order on one goroutine), it ends
// every run past its ends_at with no terminal frame stored, at any age (goingEveryOverdueRun),
// with the deadline's error, through the loop the sweep ends its runs through. The sweep reads the
// last day alone (goingSweepWindow), so a run whose window passed while every process of the
// deployment was down for longer is ended here, at the next boot, whether or not anybody reads
// it. It takes no lease: every process that boots runs it once,
// and endGoingRun does nothing to a run that already holds a terminal frame, so two processes
// booting together still end each run once. It logs one line, how many runs it ended and how long
// it took; a failure is that line at warn, with the error, and stops nothing else: the sweep's
// ticks still run, and a run the pass could not end is left to its reader.
func EndGoingRunsOverdueAtBoot(ctx context.Context, pool *store.Pool) {
	started := time.Now()
	ended, err := endOverdueGoingRuns(ctx, pool, goingEveryOverdueRun, goingTerminalNames)
	if err != nil {
		slog.Warn("dispatch: the boot pass over the overdue answer runs failed",
			"ended", ended, "duration", time.Since(started), "error", err)
		return
	}
	slog.Info("dispatch: the boot pass ended the overdue answer runs", "ended", ended, "duration", time.Since(started))
}

// endOverdueGoingRuns ends every run the read overdue answers, run with args, with the
// deadline's error (contracts.GoingErrorAnswerDidNotArrive), and answers how many of them it
// ended: the one loop of the sweep (sweepGoingRequests) and the boot pass
// (EndGoingRunsOverdueAtBoot), which differ only in what they read.
//
// Each overdue run is ended by endGoingRun, the path every other terminal write takes, so neither
// pass holds a second copy of how a frame is written: the ids are read first, oldest deadline
// first, then each run is ended in a transaction of its own. A run a reader, the answer itself or
// another pass ended between the read and the write keeps that frame, which endGoingRun answers,
// and is counted ended. A run deleted between the read and the write - by an erase, or during a
// roll by a process of the release before, which still deletes a run an hour old - stores nothing
// (errGoingRunGone) and is passed over, and any other failure of one run's ending is kept and
// answered once the pass is done, so one run neither fails the pass nor costs the runs read after
// it their ending. A pass whose context ends - the process shutting down - stops at the next run
// and answers that once, rather than failing every run left at its first statement.
func endOverdueGoingRuns(ctx context.Context, pool *store.Pool, overdue string, args ...any) (int, error) {
	rows, err := pool.Query(ctx, overdue, args...)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	deadline := going.Fault(contracts.GoingErrorAnswerDidNotArrive)
	ended := 0
	var failed []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			failed = append(failed, err)
			break
		}
		_, err := endGoingRun(ctx, pool, id, deadline)
		switch {
		case err == nil:
			ended++
		case !errors.Is(err, errGoingRunGone):
			failed = append(failed, err)
		}
	}
	return ended, errors.Join(failed...)
}
