package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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
// A run is readable for one hour, by its going_requests row's created_at
// (contracts.GoingRequestTTL), as the release before kept it; then the sweep states its node gone
// and removes its rows, and the node, its words and its frames stay in the store
// (sweepGoingRequests). No statement here deletes a row of graph_nodes or of graph_tuples. During
// a roll, and after a rollback, a process of the release before deletes a run's rows an hour old
// and leaves its node live; the sweep states that node gone too, as the backfill does
// (graphcensus's backfillGoneRuns).
//
// The window a run has belongs to the record: going_requests.ends_at, written by the POST. A pod
// that dies mid-run stores no terminal frame, so a reader on another pod ends the stream itself
// once the run is past that moment with nothing terminal stored (readGoingStream), and
// sweepGoingRequests ends the runs nobody reads. At the deadline the running answer, a reader and
// the sweep may each try to end one run, so which of them ends it is settled by the table (the
// going_events_terminal index of migration 20261105152945) and never by whichever is first: every
// terminal write is an insert that does nothing on a conflict, and the writer that loses reads the
// stored terminal frame and reports that one (endGoingRun).
//
// What a run gave its model is part of the record too: each comment and issue message whose text
// the run read for its model is a going_request_posts row (recordGoingPosts), so a reader on any
// pod ends the stream at the delete of one of them (goingWithdrawal).

// goingRequestColumns is the going_requests select list scanGoingRequest reads, in scan order.
const goingRequestColumns = `id::text, author, project, words, created_at, ends_at`

// goingEventColumns is the going_events select list scanGoingEvent reads, in scan order.
const goingEventColumns = `sequence, name, data`

// goingTerminalWrites bounds how many sequences one terminal write tries. Every frame this build
// writes of a run queues at the run's interaction node (appendGoingEvent), so a terminal write
// loses its sequence only to a frame a pod of the release before wrote during a roll, which takes
// no such lock: to a terminal frame, which ends the attempt with that stored frame, or to a frame
// of the run's own, after which the next sequence is free. Two such losses in a row would need
// that pod to be writing faster than this write's round trips; the bound is there so a caller
// cannot spin, not because a third attempt is expected to be needed.
const goingTerminalWrites = 3

// errGoingRunGone is a frame of a run the record no longer holds: one nobody asked, or one whose
// rows went an hour after it was asked - the sweep's removal, which states its node gone with
// them, or a process of the release before's delete during a roll, whose node may still stand
// then. It stores nothing. It wraps pgx.ErrNoRows, which is what a frame the record stores none of
// answers its writer, so an answer still writing reads its run as over either way; endGoingRun
// answers it rather than trying another sequence, and the sweep passes over it.
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
// run's one terminal frame, 0 while the run has stored none, whether it is past its ends_at,
// after which a run that stored none is ended by whoever reads it, and whether it is withdrawn,
// after which a reader is written goingWithdrawal's frame and nothing else. The terminal frame is
// a sequence and not a bool because a reader ends its stream on having been written that frame
// and not on the run having stored one: a terminal frame committed between a reader's read of
// the frames and its read of this state would otherwise close a stream that never carried it.
// The going_events_terminal index holds that there is at most one such frame, so one sequence
// says all of it.
type goingRunState struct {
	Terminal     int
	PastDeadline bool
	Withdrawn    bool
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
// never asked, or one whose rows went an hour after it was asked (sweepGoingRequests, or a process
// of the release before). What a run the caller does not own answers is the caller's to decide,
// which is why the row carries its Author: model.Actor.SameCredential compares it with the
// reader's.
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

// goingNextFrame reads what a frame of the run $1 takes, once the run's node is held: the next
// sequence (graphstore.RunNextSequenceSQL), the frame's value at that sequence
// (graphstore.FrameValueSQL over the frame's name $2 and its JSON $3), whether the run has stored a
// terminal frame, and the transaction's now(), which the frame's going_events row takes as its
// created_at. The sequence and the ending are one index probe each, never a read of every frame the
// run holds: its newest sequence, read backward on the (request_id, sequence) key, and its one
// terminal frame on going_events_terminal (graphstore.RunEndingSQL), whose literal names let the
// planner use it (TestTheNextFrameReadsTwoIndexProbes).
// It takes the run's going_requests row `for key share`, the lock the frame's own row's foreign
// key would take, so a delete of the run's row an hour on by a process of the release before,
// which takes no node lock, waits for the frame rather than failing its insert; the sweep's own
// removal takes the run's node first and so is never under way when a frame of this build reaches
// this read. A run the record no longer holds answers no row.
var goingNextFrame = `
	select next.sequence,
	       ` + graphstore.FrameValueSQL("next.sequence", "$2::text", "$3::jsonb") + `,
	       next.ended,
	       now()
	from going_requests r
	cross join lateral (
		select ` + graphstore.RunNextSequenceSQL("r.id") + ` as sequence,
		       ` + graphstore.RunEndingSQL("r.id") + ` is not null as ended
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
// going_requests row by then, and appendGoingEvent begins the frame again, which takes the node's
// row first and nothing before it; the sweep passes such a run over, and its next pass takes it.
// The sweep holds a run's node and then deletes the run's row (goRunGone), which waits on that
// hold of the row, so a frame that waited for the node while it held the row could wait on the
// sweep as the sweep waits on it; letting the transaction go is what keeps that from happening,
// and it costs such a frame one more transaction.
var errGoingRunNodeHeld = errors.New("the run's interaction node is held by another transaction")

// goingRunNodeTaken is graphstore.NodeRowLockSQL taken without waiting: a row another transaction
// holds is skipped, and no row is taken.
const goingRunNodeTaken = graphstore.NodeRowLockSQL + ` skip locked`

// lockGoingRun takes the interaction node row of the run requestID names `for no key update`
// (graphstore.LockInteraction), the first lock of every frame's transaction and of the sweep's
// transaction of a run past the hour (goRunGone), so each takes a node's row before the run's
// going_requests row, and frames and the sweep of one run queue at the node. A run a pod of the
// release before recorded during a roll has no node yet: its node is written first, from its
// going_requests row, by the one writer of a run's node from its row, which the backfill shares
// (graphstore.WriteRun): it takes that row `for no key update` and also states a frame value of
// every frame that pod stored, and the node is then taken. The node it writes is this
// transaction's own, which no other can see until it commits, let alone hold. A node another
// write committed in between is taken without waiting (goingRunNodeTaken), and where another
// transaction holds it the answer is errGoingRunNodeHeld, which a frame begins again on and the
// sweep passes over, so nothing waits on a node's row while it holds the run's. A run the record
// holds neither a row nor a node of is errGoingRunGone.
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
		where request_id = $1::uuid and name in (`+graphstore.TerminalFrameNamesSQL+`)
	`, requestID))
}

// goingEventsAfter is every frame of one run past a sequence, in stream order: $1 the run, $2
// that sequence; scanGoingEvent decodes each row. It is named rather than written inline
// because readGoingStream queues it in a batch beside the state read.
const goingEventsAfter = `
	select ` + goingEventColumns + `
	from going_events
	where request_id = $1::uuid and sequence > $2
	order by sequence`

// goingRunStateOf reads one run's state, $1 the run: the sequence of its terminal frame, 0 while it
// has stored none (graphstore.RunEndingSQL), whether now is past its ends_at, and whether a post the
// run gave its model no longer stands (goingWithdrawal), by the one test of that
// (graphstore.RunWithdrawnSQL), which the graph read of the run's own address reads too. Queued
// beside goingEventsAfter, for the reason it is named.
var goingRunStateOf = `
	select coalesce(` + graphstore.RunEndingSQL("r.id") + `, 0),
	       now() >= r.ends_at,
	       ` + graphstore.RunWithdrawnSQL("r.id") + `
	from going_requests r
	where r.id = $1::uuid`

// readGoingStream is what one poll of an open stream reads: the frames past after, and the run's
// state, in one round trip (store.Pool.SendBatch), where a poll of a stream costs two. Either
// statement failing is the whole read's failure, which ends the stream, and pgx.ErrNoRows is the
// run's rows removed under its reader an hour after it was asked, by the sweep or by a process of
// the release before.
//
// The two statements take their own snapshots, each at its own moment, so a frame committed
// between them is in one answer and not the other. For the run's ending that order settles
// nothing: a terminal frame in the frames ends the stream on being written, and a terminal
// sequence in the state ends it only once the reader has reached that sequence
// (streamGoingAnswer), so neither order can close a stream that never carried its ending. For
// the withdrawal it does, which is why the state is queued second: the verdict is then never
// older than the frames it is read beside, so a poll whose frames were read after a delete
// committed is answered withdrawn, and the stream writes none of them (goingWithdrawal).
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
	batch.Queue(goingRunStateOf, requestID).QueryRow(func(row pgx.Row) error {
		return row.Scan(&state.Terminal, &state.PastDeadline, &state.Withdrawn)
	})
	if err := s.deps.Store.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return nil, goingRunState{}, err
	}
	return frames, state, nil
}

// goingWithdrawal is the one frame a reader of a withdrawn run is written, and all a reader of it
// is written from then on. A run is withdrawn once a comment or an issue message whose text it
// gave its model (recordGoingPosts) no longer stands - its author deleted it, or its row is gone
// - since the run's answer may quote that post. Every read of the stream after the delete, from
// the start or from a Last-Event-ID before the run's ending, therefore gives none of the run's
// stored frames, no text and no item among them, and ends on an error frame carrying
// contracts.GoingErrorAnswerWithdrawn; a read resumed at or past the ending's sequence is written
// nothing (the last paragraph). The stored frames are left as they are, as an event Dispatch
// already wrote keeps its stored text while every read of the log masks it (attachPostMarks), and
// a frame a reader was written before the delete was a delivery already sent. A run given no post
// that was deleted streams as it always has.
//
// The verdict is read at stream time, on every poll (goingRunStateOf), and not written by the
// delete: the delete then knows nothing of answer runs, and the one read covers every frame
// alike - those stored before the delete, those a run still running stores after it, and the
// ending the run, a reader at the deadline or the sweep stored. A run still running is let past
// it by nothing: the poll that finds the run withdrawn writes none of the frames it read beside
// the verdict, and a run with no ending stored yet is ended here with the withdrawal
// (endGoingRun), so the answer stores nothing after it (appendGoingEvent) and stops at the next
// frame it tries to store.
//
// The frame takes the sequence of the run's one ending, the one stored or the one this call
// stored, so a reader is written exactly one ending, in the ending's place: a reader that
// already holds that sequence was written the run's ending, and streamGoingAnswer writes it
// nothing more.
//
// The read of the run's own address, `dispatch://interaction/<uuid>`, is a second view of the same
// frames, and answers what this does rather than deciding it again (refs' interactionFrames): for a
// run the same test calls withdrawn (graphstore.RunWithdrawnSQL), the node's frames are this one
// frame, the one value both read (graphstore.RunWithdrawal), at the sequence of the run's stored
// ending, or, where the run has stored none, at the sequence this call would store it at
// (graphstore.RunNextSequenceSQL), and none of the frames the run stored. That read writes
// nothing.
func (s *server) goingWithdrawal(ctx context.Context, requestID string, state goingRunState) (goingEvent, error) {
	ending := state.Terminal
	if ending == 0 {
		stored, err := endGoingRun(ctx, s.deps.Store.Pool, requestID, graphstore.RunWithdrawal)
		if err != nil {
			return goingEvent{}, err
		}
		ending = stored.Sequence
	}
	return goingEvent{Sequence: ending, Event: graphstore.RunWithdrawal}, nil
}

// recordGoingPosts records, as the run's, each comment and issue message among hits that texts
// holds a body for: the posts whose text the run is about to give its model, which is what a
// later delete of one withdraws the run's answer by (goingWithdrawal). It is called with the
// bodies and before they are handed on (goingBodies), so a post's row is committed before any
// frame its text can be in is stored, and a reader that reads such a frame reads the row too. A
// body the answerer then leaves out for the run's rune budget is recorded all the same, since
// only the answerer knows which it kept, and a post two searches of one run both found is two
// rows, which the verdict reads as one.
func (s *server) recordGoingPosts(ctx context.Context, requestID string, hits []going.Hit, texts map[string]string) error {
	var comments, messages []string
	for _, hit := range hits {
		if _, given := texts[hit.Key()]; !given {
			continue
		}
		switch refs.ItemKind(hit.Kind) {
		case refs.ItemComment:
			comments = append(comments, hit.ID)
		case refs.ItemMessage:
			messages = append(messages, hit.ID)
		}
	}
	if len(comments) == 0 && len(messages) == 0 {
		return nil
	}
	_, err := s.deps.Store.Pool.Exec(ctx, `
		insert into going_request_posts (request_id, comment_id, message_id)
		select $1::uuid, post, null::uuid from unnest($2::uuid[]) as post
		union all
		select $1::uuid, null::uuid, post from unnest($3::uuid[]) as post
	`, requestID, comments, messages)
	return err
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

// goingOverdueRuns is the sweep's read of the runs it ends: every run whose ends_at has passed
// with no terminal frame stored (graphstore.RunEndingSQL), oldest deadline first. Once the sweep
// runs, going_requests holds about an hour of runs, since the sweep removes a run's rows an hour
// after it was asked (contracts.GoingRequestTTL, goRunsPastTheHour), so the read is of that hour.
var goingOverdueRuns = `
	select r.id::text
	from going_requests r
	where r.ends_at <= now()
	  and ` + graphstore.RunEndingSQL("r.id") + ` is null
	order by r.ends_at, r.id`

// sweepGoingRequests is the sweep of the runs of the command bar, once a maintenance pass
// (RunMaintenanceSweep), in two steps.
//
// It first ends every run past its ends_at with no terminal frame stored, with the deadline's
// error (contracts.GoingErrorAnswerDidNotArrive), as it always did (endOverdueGoingRuns). Ending
// those runs is what nothing else does: the pod that was answering one may have died, and nobody
// may be reading it.
//
// Then it takes the runs past contracts.GoingRequestTTL, an hour by the run's created_at
// (goRunsPastTheHour): a run is readable for one hour, as the release before kept it, and then the
// sweep states its interaction node gone and removes its going_requests and going_events rows.
// The node then reads gone, so no read answers it - not its address, not its stream, not an edge
// at it - and the node, its words value, its frame values and every tuple into it, a
// changed_by_run among them, stay in the store: no statement of the sweep deletes a row of
// graph_nodes or of graph_tuples. It also states gone the node of a run whose rows a process of the
// release before deleted an hour old, which leaves the node live.
//
// A failure of either step is answered once both have run, so a run one step could not reach
// costs the other step nothing.
func sweepGoingRequests(ctx context.Context, pool *store.Pool) error {
	return errors.Join(endOverdueGoingRuns(ctx, pool), goRunsPastTheHour(ctx, pool))
}

// endOverdueGoingRuns ends every run goingOverdueRuns answers with the deadline's error
// (contracts.GoingErrorAnswerDidNotArrive), through the sweep's one loop over runs (eachGoingRun).
// Each overdue run is ended by endGoingRun, the path every other terminal write takes, so the
// sweep holds no second copy of how a frame is written. A run a reader, the answer itself or
// another pass ended between the read and the write keeps that frame, which endGoingRun answers.
// A run whose rows went between the read and the write - an hour after it was asked, by another
// process's sweep or, during a roll, by a process of the release before - stores nothing
// (errGoingRunGone) and is passed over.
func endOverdueGoingRuns(ctx context.Context, pool *store.Pool) error {
	deadline := going.Fault(contracts.GoingErrorAnswerDidNotArrive)
	return eachGoingRun(ctx, pool, "read the overdue runs", goingOverdueRuns, nil, func(id string) error {
		if _, err := endGoingRun(ctx, pool, id, deadline); err != nil && !errors.Is(err, errGoingRunGone) {
			return err
		}
		return nil
	})
}

// eachGoingRun is the one loop of the sweep over runs, which both its steps take
// (endOverdueGoingRuns, goRunsPastTheHour): it reads the ids of the runs statement answers, run
// with args, and then calls take with each in the order the statement answers them, each run in a
// transaction of its own, which take begins. A failure of the read is answered alone, named by
// read. A run's failure is kept and answered once the loop is done, joined with the others, so one
// run neither fails the loop nor costs the runs read after it their turn. A loop whose context
// ends - the process shutting down - stops at the next run and answers that once, joined with the
// runs' failures before it, rather than failing every run left at its first statement. A loop
// begun on a context that has already ended fails at its read, which answers that end named by
// read, as goRunsPastTheHour's second read does after a shutdown during its first.
func eachGoingRun(ctx context.Context, pool *store.Pool, read, statement string, args []any, take func(id string) error) error {
	rows, err := pool.Query(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", read, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("%s: %w", read, err)
	}
	var failed []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failed, err)...)
		}
		if err := take(id); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// goingGoneBatch bounds how many runs each read of goRunsPastTheHour answers in one pass. Once the
// sweep runs, each read answers the runs asked in about one minute an hour back, far fewer than
// this; the bound is what keeps a pass short after a backlog - every process down, or every pass
// failing, for a while - whose rest the passes after it take, a batch a pass.
const goingGoneBatch = 200

// goingSweepActor is who the sweep states a run's node gone as: the server's sweep, which acts
// for no person, as graphstore.FrameWriter, which states the frame it ends a run with, acts for
// none. The run's author asked the question; its end an hour on is none of their writing.
var goingSweepActor = model.Actor{Kind: "system", ID: "going-sweep"}

// goingRunsPastTheHour is the sweep's first read of the runs past the hour, $1
// contracts.GoingRequestTTL in seconds and $2 goingGoneBatch: the runs the record holds whose
// going_requests row was written longer ago than that, oldest first, at most $2 of them, each
// with no interaction node or one of the source going_requests (graphstore.SourceGoingRequests).
// Once the sweep runs, the table holds about an hour of runs, so the read is of that hour and no
// more; no index keys created_at, and a table that small needs none. Each run's node is read by a
// probe of graph_nodes' primary key keyed by the run's id, which ends in an offset 0: without it
// the planner turns the test into an anti-join over every interaction node the store holds, the
// gone ones among them, which grow with every run asked, as it did over an empty database
// (TestTheSweepReadsOfTheRunsPastTheHourAreBounded).
var goingRunsPastTheHour = `
	select r.id::text
	from going_requests r
	where r.created_at < now() - make_interval(secs => $1)
	  and not exists (
	        select 1 from graph_nodes n
	        where n.kind = '` + graphstore.KindInteraction + `' and n.id = r.id::text
	          and n.attributes->>'` + graphstore.AttributeSource + `' is distinct from '` + graphstore.SourceGoingRequests + `'
	        offset 0)
	order by r.created_at, r.id
	limit $2`

// goingRowlessRuns is the sweep's second read, $1 contracts.GoingRequestTTL in seconds and $2
// goingGoneBatch: the runs whose rows a process of the release before deleted, leaving the node
// live (graphstore.RowlessRunSQL), made longer ago than that, oldest first, at most $2 of them.
// The gone nodes grow with every run asked, so the read binds the predicate's literals, which
// graph_nodes_interaction_live is partial on (migration 20261122230407), and reads that index's
// range of made_at, the moment the run was asked, which holds about an hour of runs, and never
// every interaction ever written (TestTheSweepReadsOfTheRunsPastTheHourAreBounded).
var goingRowlessRuns = `
	select n.id
	from graph_nodes n
	where ` + graphstore.RowlessRunSQL + `
	  and n.made_at < now() - make_interval(secs => $1)
	order by n.made_at
	limit $2`

// goRunsPastTheHour states gone every run past contracts.GoingRequestTTL, an hour, and removes its
// rows, through two reads, each bounded at goingGoneBatch runs a pass: the runs whose
// going_requests row is more than an hour old (goingRunsPastTheHour), and then the interaction
// nodes, not gone and more than an hour old, whose row a process of the release before deleted
// (goingRowlessRuns). Each run either read answers is taken by goRunGone through the sweep's one
// loop over runs (eachGoingRun). Both reads always run: a pass whose context ended fails the read
// that comes after, under that read's name, so a shutdown during the first read's loop can name
// the end twice, once from that loop and once from the second read. It logs one line where it did
// anything: how many runs of the first read and nodes of the second it stated gone, and how many
// runs' rows it removed.
func goRunsPastTheHour(ctx context.Context, pool *store.Pool) error {
	var runsStated, rowlessStated, removed int
	var failed []error
	reads := []struct {
		name      string
		statement string
		stated    *int
	}{
		{"read the runs past the hour", goingRunsPastTheHour, &runsStated},
		{"read the runs past the hour whose rows are gone", goingRowlessRuns, &rowlessStated},
	}
	for _, read := range reads {
		err := eachGoingRun(ctx, pool, read.name, read.statement, []any{contracts.GoingRequestTTL.Seconds(), goingGoneBatch}, func(id string) error {
			done, err := goRunGone(ctx, pool, id)
			if done.stated {
				*read.stated++
			}
			if done.removed {
				removed++
			}
			return err
		})
		if err != nil {
			failed = append(failed, err)
		}
	}
	if runsStated+rowlessStated+removed > 0 {
		slog.Info("dispatch: the going sweep stated the runs past the hour gone",
			"runs_stated_gone", runsStated, "rows_removed", removed, "rowless_nodes_stated_gone", rowlessStated)
	}
	return errors.Join(failed...)
}

// goingRunGone is what goRunGone did of one run: whether it stated the run's interaction node
// gone, and whether it removed the run's rows.
type goingRunGone struct {
	stated, removed bool
}

// goRunGone states one run past the hour gone and removes its rows, in a transaction of its own,
// and answers what it did. Its id comes from either read of goRunsPastTheHour, each of which has
// found the run more than an hour old already, by its row or by its node, which no write makes
// younger, so nothing here reads the run's age again.
//
// It takes the locks in the order every other writer of a run takes them: the run's interaction
// node row first, `for no key update` (lockGoingRun, through graphstore.LockInteraction); then the
// run's going_requests row, which its delete takes `for update`; and then, through that row's
// foreign key, which cascades, the run's going_events rows - the order a frame takes them in
// (appendGoingEvent: the node, the run's row `for key share`, the frame's row). No writer waits on
// it the other way. No writer of this build holds a run's row before its node but
// graphstore.WriteRun, for a run with no node yet: it writes that node in its own transaction,
// where no other can see it, let alone hold it, and takes a node another write committed
// meanwhile without waiting, letting its transaction go where another holds it (lockGoingRun,
// errGoingRunNodeHeld). A process of the release before takes no node lock at all: its frame or
// its delete may hold the run's row, which this then waits for, and neither waits on a lock this
// holds.
//
// Under the node's lock it reads the node, and states it gone where it is not (graphstore.WriteState)
// as the sweep (goingSweepActor), now, with no event, as no node of a run appends one: the node row
// moves to gone and one state tuple is appended, and every other tuple of the node or into it
// stays. It then deletes the run's going_requests row, and its going_events rows with it. A run
// the record holds with no node yet - one a process of the release before recorded during a roll,
// which no frame of this build and no backfill has reached - has its node written first, from its
// row, by the one writer of a run's node from its row (graphstore.WriteRun, which lockGoingRun
// calls), and then stated gone and its rows removed, all in this one transaction. Passing it over
// until the backfill wrote its node would leave the census reading its row, with no node, as a
// difference for as long; written and stated gone with its rows removed, it is a gone node with no
// row, which the census reads as none.
//
// A node of any source but going_requests is passed over, as is a run the record holds neither a
// row nor a node of, a run whose node another process stated gone and removed between the read and
// the write (another pass), and a run whose node another transaction holds by the time this takes
// it (errGoingRunNodeHeld), which the next pass takes. A run whose rows a process of the release
// before deleted between the read and the write is a node whose row is gone, which this states
// gone, as the second read would.
func goRunGone(ctx context.Context, pool *store.Pool, id string) (goingRunGone, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return goingRunGone{}, fmt.Errorf("begin the end of run %s: %w", id, err)
	}
	defer tx.Rollback(ctx)
	err = lockGoingRun(ctx, tx, id)
	if errors.Is(err, errGoingRunGone) || errors.Is(err, errGoingRunNodeHeld) {
		return goingRunGone{}, nil
	}
	if err != nil {
		return goingRunGone{}, err
	}
	// lockGoingRun answers nil only where this transaction holds the node's row, so the read below
	// finds it.
	node, _, err := graphstore.ReadNode(ctx, tx, graphstore.KindInteraction, id)
	if err != nil {
		return goingRunGone{}, err
	}
	if node.Attributes[graphstore.AttributeSource] != graphstore.SourceGoingRequests {
		return goingRunGone{}, nil
	}
	var done goingRunGone
	if node.State != graphstore.StateGone {
		if _, err := graphstore.WriteState(ctx, tx, []graphstore.Node{node}, graphstore.StateGone, time.Now().UTC(), goingSweepActor); err != nil {
			return goingRunGone{}, fmt.Errorf("state run %s gone: %w", id, err)
		}
		done.stated = true
	}
	// A node id of any shape but the one Postgres writes a uuid in names no row (goingRowlessRuns),
	// and is read as none rather than cast, so the node is still stated gone.
	removed, err := tx.Exec(ctx, `delete from going_requests where id = `+store.CanonicalUUID("$1::text"), id)
	if err != nil {
		return goingRunGone{}, fmt.Errorf("remove the rows of run %s: %w", id, err)
	}
	done.removed = removed.RowsAffected() > 0
	if done == (goingRunGone{}) {
		return done, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return goingRunGone{}, fmt.Errorf("commit the end of run %s: %w", id, err)
	}
	return done, nil
}
