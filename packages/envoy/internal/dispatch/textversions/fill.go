package textversions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The fill writes the keyword table from the event log in batches, each one transaction of its
// own that reads the events above its cursor, writes their rows and moves the cursor past them
// (filler.batch), and in passes, each up to one read of the event log's safe horizon
// (filler.pass).
const (
	// batchEvents and batchBytes bound one batch: at most 500 events, and at most 2 MB of the text
	// their vectors are made from. This is the one statement of what the bytes count.
	//
	// The read of the events counts first (readEventsQuery): each event of a type a rule reads
	// counts the bytes of its payload, and a post whose event cut its body counts that body as
	// its present row holds it too, the one source a cut body is most often read from; an event
	// of any other type makes no vector and counts nothing. The rules count the rest as they
	// decide (decideVersions, decider.size): an event that starts a version counts what that
	// version's vector reads besides, an issue's row the markdown of the primary document version
	// it names (version 1 for an issue.created, the version carried forward for an issue.updated
	// or an issue.closed, and the version named for an artifact.version), which the read cannot
	// know before the rules carry an issue's primary document version forward, and a cut post the
	// whole body the rules give it in place of its row's body where that is longer. An event that
	// starts no version adds nothing to what the read counted. So the read bounds what the batch
	// reads of the events and their rows' bodies, and the rules what its vectors read.
	//
	// Each of the two ends the batch at the first event after the first whose count would take
	// the total past the bound, and the next batch starts at it: the first event is in the batch
	// whatever its count, so an event that counts more than 2 MB alone is a batch alone. Neither
	// counts the title an artifact.version's row carries forward, at most contracts.IssueTitleMax
	// (about 3 KB) a row. The bytes counted bound each statement of the batch (statementTimeout).
	// A full batch measured 0.6 to 1.3 s.
	batchEvents = 500
	batchBytes  = 2 << 20
	// batchStatementBase and batchStatementPerMB bound each statement of a batch: 10 s, and 1 s
	// more for each MB of text the batch holds.
	batchStatementBase  = 10 * time.Second
	batchStatementPerMB = time.Second
	// rereadFirst and rereadMax pace the reads of the horizon a fill makes while the horizon
	// stands below an id an observation saw: 1 s, doubling to 8 s.
	rereadFirst = time.Second
	rereadMax   = 8 * time.Second
	// progressEvery bounds the fill's progress line while it catches up to one each 30 s.
	progressEvery = 30 * time.Second
)

// fillHoldName is the name of the duty hold the fill runs under in a serving process, one process
// at a time (duty.TryHold): each batch of a holder checks that duty_holds still names it.
const fillHoldName = "search-text-versions-fill"

// The lines of the fill's batches that tests and measurements read: each batch's (filler.record),
// each re-read batch's, and that of a batch that found the event log reset under the cursor.
const (
	batchMessage  = "dispatch: the keyword fill wrote a batch"
	rereadMessage = "dispatch: the keyword fill read again what a fill of a lower rules version passed over"
	resetMessage  = "dispatch: the event log was reset under the keyword fill's cursor; the fill emptied its table and starts again from the first event"
)

// FillToHorizon writes the rows of every event up to the event log's safe horizon once, from the
// fill's cursor, in batches with their rest, and returns: with no hold, so two fills that run at
// once take turns on the cursor row, and with no pause. It reads the horizon again while the
// horizon stands below an id an observation of it saw, 1 s and then up to 8 s apart, so every
// event committed before the call has its rows when it returns. A server runs the fill under its
// hold instead; this is for tests and tools.
//
// An event whose payload the rules cannot read stops it, with the rows of the events before that
// one written and an error that names it.
func FillToHorizon(ctx context.Context, db *store.Store, horizon *events.Horizon) error {
	return newFiller(db.Pool, horizon).toHorizon(ctx)
}

// txBeginner is what the fill needs of a pool: transactions. *store.Pool, the shared pool
// FillToHorizon writes through, and *pgxpool.Pool, a lease pool's reservation, both satisfy it.
type txBeginner interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// filler is one fill: the pool its batches run on, the horizon its passes read, and what a
// serving process sets on it. A filler is used by one goroutine at a time.
type filler struct {
	pool    txBeginner
	horizon *events.Horizon
	logger  *slog.Logger
	// holder is the holder of the hold fillHoldName the fill's batches run under: each batch
	// checks that duty_holds still names holder for it, and writes holder into the cursor row.
	// uuid.Nil runs with no hold.
	holder uuid.UUID
	// pause, when set, runs before each batch and holds it back until it returns; an error ends
	// the pass with it.
	pause func(ctx context.Context) error
	// rest waits after each batch that read events for as long as the batch ran, so the fill
	// takes at most half of one backend's time.
	rest func(ctx context.Context, d time.Duration) error
	// maxEvents and maxBytes are the bounds of one batch (batchEvents, batchBytes).
	maxEvents int
	maxBytes  int64
	// rules is the rules the fill decides by: this build's (currentRules), or in a test a lower
	// version's, standing for a fill of an earlier build.
	rules *ruleSet
	// reread is where the re-read under way stands (rereadBatch): the cursor it reads up to and
	// the last event its committed batches read. It is the zero value while none is under way.
	reread rereadProgress
	// progress is what the batches since the last progress line wrote, and breaches the events
	// this fill wrote at or before a reach it had stored.
	progress progress
	breaches int
}

// progress counts the batches' work since the last progress line, which since names.
type progress struct {
	since             time.Time
	events, rows, cut int
}

// newFiller is a fill on pool, up to horizon, with this build's rules, no hold, no pause and the
// production bounds.
func newFiller(pool txBeginner, horizon *events.Horizon) *filler {
	return &filler{
		pool:      pool,
		horizon:   horizon,
		logger:    slog.Default(),
		rest:      sleep,
		maxEvents: batchEvents,
		maxBytes:  batchBytes,
		rules:     &currentRules,
	}
}

// sleep waits d, or until ctx ends with its error.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// toHorizon runs passes until one leaves no id an observation saw above the horizon it read: a
// pass that ends with the horizon held below such an id (Horizon.Gap), or with the horizon proven
// past the safe it read by another reader of it, is followed by another 1 s later, doubling to
// 8 s. The horizon waits on every event write of this database, and one that rolls back ends with
// no notification, so nothing else says when to read it again.
func (f *filler) toHorizon(ctx context.Context) error {
	wait := rereadFirst
	for {
		safe, err := f.pass(ctx)
		if err != nil {
			return err
		}
		if !f.heldBelow(safe) {
			return nil
		}
		if err := sleep(ctx, wait); err != nil {
			return err
		}
		wait = min(2*wait, rereadMax)
	}
}

// heldBelow is whether the horizon stands below an id an observation of it saw, or another reader
// of it has proven it past safe, the horizon the pass read: either way the log holds events above
// what the pass wrote that a later read of the horizon reaches.
func (f *filler) heldBelow(safe int64) bool {
	return f.horizon.Gap() != events.GapNone || f.horizon.Proven() > safe
}

// pass reads the event log's safe horizon once (Horizon.Safe, or Horizon.Await while Safe answers
// 0, which it does until an observation is proven) and writes batches up to it, resting after
// each as long as it ran, and answers the horizon it wrote up to. A batch that finds the log
// reset under the cursor empties the table (filler.batch), and one that finds the horizon read
// from a log since reset writes nothing: either way the horizon names ids of an old log, and the
// pass forgets it (Horizon.Reset) and reads it again, so no id of the new log still open is
// passed.
func (f *filler) pass(ctx context.Context) (int64, error) {
	for {
		safe, err := f.horizon.Safe(ctx)
		if err != nil {
			return 0, fmt.Errorf("keyword fill: read the event log's safe horizon: %w", err)
		}
		if safe == 0 {
			if safe, err = f.horizon.Await(ctx); err != nil {
				return 0, fmt.Errorf("keyword fill: await the event log's safe horizon: %w", err)
			}
		}
		again, err := f.batchesTo(ctx, safe)
		if err != nil || !again {
			return safe, err
		}
		f.horizon.Reset()
	}
}

// batchesTo writes batches up to safe until one reads no event or reads every event left up to
// safe, and answers whether the horizon must be read again, the log having been reset. A re-read
// batch (rereadBatch) is followed by the next batch whatever it read, after its rest: the batches
// of the re-read run until one raises the rules version, and then the batches of the events above
// the cursor.
func (f *filler) batchesTo(ctx context.Context, safe int64) (bool, error) {
	for {
		if f.pause != nil {
			if err := f.pause(ctx); err != nil {
				return false, err
			}
		}
		result, err := f.batch(ctx, safe)
		if result.events > 0 || result.reread {
			f.record(result)
		}
		switch {
		case err != nil:
			return false, err
		case result.reset || result.oldHorizon:
			return true, nil
		case result.reread:
			if err := f.rest(ctx, result.took); err != nil {
				return false, err
			}
			continue
		case result.events == 0:
			return false, nil
		}
		if err := f.rest(ctx, result.took); err != nil {
			return false, err
		}
		if !result.full {
			return false, nil
		}
	}
}

// batchResult is what one batch did.
type batchResult struct {
	// events is how many events it wrote the rows of, from first to the cursor it moved to;
	// bytes the text they count against the batch's bound (batchBytes), rows the rows they
	// started, cut how many of those hold a body cut short.
	events      int
	first       int64
	bytes       int64
	rows, cut   int
	cursor      int64
	reach       *time.Time // the reach the cursor row holds once the batch committed
	took        time.Duration
	full        bool // events up to the horizon were left for the next batch
	breaches    int  // events written whose created_at is at or before the reach stored before
	storedReach *time.Time
	// reset is a batch that found the log reset under the cursor and emptied the table, and
	// oldHorizon one that found the horizon above the log's newest id, read from a log since
	// reset, and wrote nothing.
	reset, oldHorizon bool
	// reread is a re-read batch (rereadBatch): events is then how many events of the types it
	// reads it read, from first, rows the versions it decided of those it read again, redone
	// the rows it decided again and changed, and raised whether it read everything and raised the
	// cursor row's rules version, so the next batch writes the events above the cursor.
	reread, raised bool
	redone         int
}

// The statements of a batch's transaction (filler.batch) but its read of the events.
const (
	// batchBoundsQuery bounds the batch's transaction: a lock it waits 5 s for at most, a pause
	// between its statements of 10 s at most, which ends the transaction of a holder frozen
	// inside a batch and so frees the cursor row, and each statement's time ($1).
	batchBoundsQuery = `select set_config('lock_timeout', '5s', true),
	       set_config('idle_in_transaction_session_timeout', '10s', true),
	       set_config('statement_timeout', $1, true)`
	// batchStatementQuery sets each statement's time once the rules have decided the batch and
	// counted its bytes ($1), before its insert, the statement that makes the vectors; a batch
	// that holds no text keeps the time batchBoundsQuery set, which is the same.
	batchStatementQuery = `select set_config('statement_timeout', $1, true)`
	// lockCursorQuery locks the fill's cursor row for the batch's transaction, so two fills write
	// one after the other: the second reads the cursor the first moved. It reads the rules
	// version stored too, which a batch of a later rules version reads again from (rereadBatch).
	lockCursorQuery = `select event_id, event_at, reach, rules from public.search_text_versions_fill where id = 1 for update`
	// createCursorQuery creates the cursor row where none is: no event written, no reach, the
	// holder $1 and this build's rules version $2.
	createCursorQuery = `insert into public.search_text_versions_fill (id, event_id, holder, rules) values (1, 0, $1, $2)
	    on conflict (id) do nothing`
	// holdCheckQuery answers whether holder $2 may write a batch under hold $1: duty_holds names
	// it live, and the cursor row names no holder, this one, or one whose hold is gone.
	holdCheckQuery = `select exists (select from public.duty_holds h
	                    where h.name = $1 and h.holder = $2 and h.expires_at > now())
	       and (f.holder is null or f.holder = $2
	            or not exists (select from public.duty_holds o
	                            where o.name = $1 and o.holder = f.holder and o.expires_at > now()))
	  from public.search_text_versions_fill f
	 where f.id = 1`
	// emptyTableQuery and emptyCursorQuery empty both tables of a log that was reset: delete,
	// not truncate, so no read of them waits on an exclusive lock. The cursor row is written
	// again after them, at no event (createCursorQuery).
	emptyTableQuery  = `delete from public.search_text_versions`
	emptyCursorQuery = `delete from public.search_text_versions_fill`
	// advanceCursorQuery moves the cursor to the last event the batch wrote ($1, its created_at
	// $2), stores the reach $3 where it is later than the one stored (greatest ignores a null, so
	// a reach computed now never lowers the stored one, and none computed leaves it), records the
	// holder $4 that wrote it, and keeps in rules the lowest rules version that wrote the rows up
	// to the cursor: the one stored, or this batch's $5 where that is lower. So a batch of a later
	// build after an older build's leaves the older version there, and the next batch of the later
	// build reads again what the older rules passed over (rereadBatch); only that read again
	// raises it (raiseRulesQuery).
	advanceCursorQuery = `update public.search_text_versions_fill
	   set event_id = $1, event_at = $2, reach = greatest(reach, $3::timestamptz), holder = $4,
	       rules = least(rules, $5::smallint), updated_at = now()
	 where id = 1
	returning reach`
	// raiseRulesQuery raises the cursor row's rules to $1 once a re-read has read again
	// everything the lower version stored passed over up to the cursor, in the transaction of its
	// last batch, which holds the cursor row (rereadBatch). The cursor stays where it is.
	raiseRulesQuery = `update public.search_text_versions_fill set rules = $1::smallint, updated_at = now() where id = 1`
)

// askBodySQL is the body of the ask event e, as the ask's own search column makes it of the
// ask's row (asks_search, migrations 0019 and 0087): the question, a space and ask_search_suffix
// of the options and the answer, over the event's payload; null where the payload holds no
// question. It reads any JSON value without failing. readEventsQuery reads it of each ask event
// into event.AskBody, so the body askVersion compares and the insert writes is made in SQL alone,
// by the function the ask's column reads.
const askBodySQL = `(e.payload->>'question') || ' ' || ask_search_suffix(e.payload->'options', e.payload->'answer')`

// readEventsQuery reads one batch's events: those above the cursor ($1) and at or below the safe
// horizon ($2), in id order, at most $3 of them, and of those the first whose bytes come to at
// most $4, the first one whatever its size. Of an event of a type a rule reads ($5) it counts the
// bytes of its payload and, of a post whose event cut its body, that body as the post's present
// row holds it: the read's part of the batch's bound, which batchBytes states. Of an ask's event
// ($6, askTypes) it reads the ask's body (askBodySQL), and null of every other event. Each read
// of a payload's value as a uuid is guarded by Postgres's own uuid input (pg_input_is_valid), so
// a payload no rule can read reaches the rules, which name it, rather than failing the statement.
// The rules' guard (isUUID) passes only the canonical form, a subset of the forms Postgres reads
// that holds each form a Dispatch writer writes: an id of any other form, even one Postgres
// reads, stops the fill at its event, named. Its project is the one the log names for it: its
// own, its issue's or its document's. Its document is the one that owns it, for an event of a
// document of no issue, through which presentBody finds the later events of a comment on that
// document.
const readEventsQuery = eventsCandidatesSQL + eventsSizedSQL

// rereadLogTypes is the event types a re-read batch reads of the log, as the SQL list its
// statement names (rereadEventsQuery): the types that change a post's text after the event that
// wrote it (changeTypes), version 2's three among them. The partial index events_post_text_changes
// (migration 20261121160853) holds the events of exactly these types, in id order, and the list is
// written into the statement rather than bound, so even the generic plan a cached statement
// settles on proves the index's predicate from it. TestTheReReadsLogReadUsesItsIndex holds the
// list to changeTypes and to the migration's text, and the plan to the index.
const rereadLogTypes = `'comment.deleted', 'comment.edited', 'message.deleted', 'message.edited'`

// rereadEventsQuery is readEventsQuery over the events of rereadLogTypes alone, which a re-read
// batch reads (filler.rereadBatch): the types that change a post's text, of which those a lower
// stored rules version passed over are read again. It reads them through events_post_text_changes
// from the id above $1, until it has $3 of them or meets $2.
const rereadEventsQuery = eventsCandidatesSQL + ` and e.type in (` + rereadLogTypes + `)` + eventsSizedSQL

// eventsCandidatesSQL and eventsSizedSQL are readEventsQuery up to its filter on the events and
// after it.
const eventsCandidatesSQL = `
with candidates as (
       select e.id, e.type, e.issue_key, e.artifact_id, e.project_key, e.seq, e.created_at, e.payload,
              case when e.type = any($6::text[]) then ` + askBodySQL + ` end as ask_body,
              case when e.type = any($5::text[]) then octet_length(e.payload::text) else 0 end as payload_size,
              case when e.type = any($5::text[])
                        and e.payload->>'body_truncated' = 'true'
                        and pg_input_is_valid(e.payload->>'id', 'uuid')
                   then coalesce(case when e.type like 'comment.%'
                                        then (select octet_length(c.body) from public.comments c
                                               where c.id = (e.payload->>'id')::uuid)
                                      when e.type like 'message.%'
                                        then (select octet_length(m.body) from public.messages m
                                               where m.id = (e.payload->>'id')::uuid)
                                 end, 0)
                   else 0 end as row_body_size
         from public.events e
        where e.id > $1 and e.id <= $2`

const eventsSizedSQL = `
        order by e.id
        limit $3),
     sized as (
       select c.*, sum(c.payload_size + c.row_body_size) over (order by c.id) as running,
              row_number() over (order by c.id) as n, count(*) over () as candidates
         from candidates c)
select s.id, s.type, s.issue_key, s.artifact_id::text,
       coalesce(s.project_key, i.project_key, ar.project_key, ''), s.seq,
       s.created_at, s.payload, s.ask_body, s.payload_size, s.row_body_size, s.candidates
  from sized s
  left join public.issues i on i.key = s.issue_key
  left join public.artifacts ar on ar.id = s.artifact_id
 where s.n = 1 or s.running <= $4
 order by s.id`

// batch writes one batch, up to safe, in one READ COMMITTED transaction: it locks the cursor row
// (creating it at no event where none is); under a hold, checks that the hold is still this
// holder's and writes nothing otherwise (duty.ErrHoldLost); checks the log against the cursor
// (events.EventLogWasReset) and empties both tables of a log that was reset; where the cursor row
// holds a lower rules version than the fill's, writes a re-read batch in place of this one
// (rereadBatch), and nothing above the cursor until the re-read raises it; reads the events above
// the cursor up to safe within the batch's bounds on what that read counts (readEventsQuery);
// decides their versions with the rules, which count what the versions read and end the batch
// where its bytes reach the bound (decideVersions, batchBytes); bounds its statements by the bytes
// the rules counted (statementTimeout) and writes the versions; moves the cursor to the last event
// decided; computes the reach (store.ComputeTextVersionsReach) and stores it where it is later
// than the one stored; and commits. A batch that reads no event still stores the reach, so a fill
// caught up keeps it current. An event whose payload the rules cannot read stops the batch there:
// the events before it are written and answered, with the error that names it.
func (f *filler) batch(ctx context.Context, safe int64) (batchResult, error) {
	started := time.Now()
	ctx = store.WithTransactionTracking(ctx)
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return batchResult{}, fmt.Errorf("keyword fill: begin a batch: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, batchBoundsQuery, statementTimeout(0)); err != nil {
		return batchResult{}, fmt.Errorf("keyword fill: bound the batch: %w", err)
	}
	cursor, cursorAt, stored, storedRules, err := f.lockCursor(ctx, tx)
	if err != nil {
		return batchResult{}, err
	}
	if f.holder != uuid.Nil {
		var holds bool
		if err := tx.QueryRow(ctx, holdCheckQuery, fillHoldName, f.holder).Scan(&holds); err != nil {
			return batchResult{}, fmt.Errorf("keyword fill: check the hold %q: %w", fillHoldName, err)
		}
		if !holds {
			return batchResult{}, fmt.Errorf("keyword fill: write a batch under the hold %q: %w", fillHoldName, duty.ErrHoldLost)
		}
	}
	newest, atCursor, err := events.EventCursorCheck(ctx, tx, cursor)
	if err != nil {
		return batchResult{}, fmt.Errorf("keyword fill: check the event log against the cursor: %w", err)
	}
	var at time.Time
	if cursorAt != nil {
		at = *cursorAt
	}
	if events.EventLogWasReset(cursor, at, newest, atCursor) {
		if err := f.empty(ctx, tx); err != nil {
			return batchResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return batchResult{}, fmt.Errorf("keyword fill: commit the emptied table: %w", err)
		}
		f.logger.Warn(resetMessage, "cursor", cursor, "newest", newest)
		return batchResult{reset: true}, nil
	}
	if safe > newest {
		return batchResult{oldHorizon: true}, nil
	}
	if storedRules < f.rules.version {
		return f.rereadBatch(ctx, tx, started, cursor, storedRules)
	}
	read, candidates, err := f.readEvents(ctx, tx, cursor, safe)
	if err != nil {
		return batchResult{}, err
	}
	got, n, err := decideVersions(ctx, tx, f.logger, f.rules, read, f.maxBytes)
	if err != nil && !errors.Is(err, errUnreadablePayload) {
		return batchResult{}, fmt.Errorf("keyword fill: %w", err)
	}
	unreadable := err
	read = read[:n]
	if got.Bytes > 0 {
		if _, err := tx.Exec(ctx, batchStatementQuery, statementTimeout(got.Bytes)); err != nil {
			return batchResult{}, fmt.Errorf("keyword fill: bound the batch's statements: %w", err)
		}
	}
	result := batchResult{
		events:      len(read),
		bytes:       got.Bytes,
		rows:        len(got.Versions),
		cut:         got.Cut,
		cursor:      cursor,
		full:        unreadable == nil && (n < candidates || candidates == f.maxEvents),
		storedReach: stored,
	}
	eventAt := cursorAt
	if len(read) > 0 {
		last := read[len(read)-1]
		result.first, result.cursor, eventAt = read[0].ID, last.ID, &last.CreatedAt
	}
	if err := insertVersions(ctx, tx, got.Versions, false); err != nil {
		return batchResult{}, fmt.Errorf("keyword fill: %w", err)
	}
	reach, computed, err := store.ComputeTextVersionsReach(ctx, tx, result.cursor)
	if err != nil {
		return batchResult{}, fmt.Errorf("keyword fill: %w", err)
	}
	var reachArg *time.Time
	if computed {
		reachArg = &reach
	}
	if err := tx.QueryRow(ctx, advanceCursorQuery, result.cursor, eventAt, reachArg, f.holderArg(), f.rules.version).
		Scan(&result.reach); err != nil {
		return batchResult{}, fmt.Errorf("keyword fill: move the cursor to event %d: %w", result.cursor, err)
	}
	if stored != nil {
		for _, e := range read {
			if !e.CreatedAt.After(*stored) {
				result.breaches++
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return batchResult{}, fmt.Errorf("keyword fill: commit the batch: %w", err)
	}
	result.took = time.Since(started)
	if unreadable != nil {
		return result, fmt.Errorf("keyword fill: %w", unreadable)
	}
	return result, nil
}

// insertVersionsQuery writes the rows of decided versions, given as parallel arrays, in one
// statement. Each row's body is its version's Body, and its vector is made with the functions the
// items' own search triggers call, so the vector of a version of an unchanged item equals the
// item's search column: an issue's from its key, its title and the markdown of the version of its
// primary document the row names (issue_search_vector, 20261105145242), and a comment's, a
// message's or an ask's from its body (search_vector over search_text, 0087), an ask's body being
// the question, a space and ask_search_suffix of the options and the answer that the ask's
// trigger reads too, as readEventsQuery made it (askBodySQL).
const insertVersionsQuery = `
	insert into public.search_text_versions
	       (kind, id, issue_key, project_key, event_id, event_seq, started_at, body, body_from,
	        spec_artifact_id, spec_version, search)
	select r.kind, r.id, r.issue_key, r.project_key, r.event_id, r.event_seq, r.started_at, r.body,
	       r.body_from, r.spec_artifact_id::uuid, r.spec_version,
	       case r.kind
	         when 'issue' then issue_search_vector(r.id, r.body,
	           (select a.markdown from public.artifact_versions a
	             where a.artifact_id = r.spec_artifact_id::uuid and a.number = r.spec_version))
	         else search_vector('', search_text(r.body))
	       end
	  from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::bigint[], $6::integer[],
	              $7::timestamptz[], $8::text[], $9::text[], $10::text[], $11::integer[])
	       as r (kind, id, issue_key, project_key, event_id, event_seq, started_at, body, body_from,
	             spec_artifact_id, spec_version)`

// insertAgainSQL ends insertVersionsQuery for a re-read batch, whose versions the table can hold
// already: those an earlier re-read wrote, of a pass a later rules version 1 batch made run again.
const insertAgainSQL = `
	    on conflict (kind, id, event_seq) do nothing`

// insertVersions writes versions as rows of the keyword table in tx, in one statement
// (insertVersionsQuery), and writes nothing for none. Each row's body is its version's Body. A
// batch's row is never written twice: a version whose (kind, id, event_seq) the table holds fails
// the statement. A re-read batch's (again) writes nothing for such a version and keeps the row.
func insertVersions(ctx context.Context, tx pgx.Tx, versions []version, again bool) error {
	if len(versions) == 0 {
		return nil
	}
	n := len(versions)
	kinds, ids, projects, bodies, bodyFroms := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	issueKeys, specArtifacts := make([]*string, n), make([]*string, n)
	eventIDs, seqs, specVersions := make([]int64, n), make([]int32, n), make([]*int32, n)
	startedAts := make([]time.Time, n)
	for index, v := range versions {
		kinds[index], ids[index], issueKeys[index], projects[index] = v.Kind, v.ID, v.IssueKey, v.ProjectKey
		eventIDs[index], seqs[index], startedAts[index] = v.EventID, v.EventSeq, v.StartedAt
		bodies[index], bodyFroms[index] = v.Body, v.BodyFrom
		specArtifacts[index], specVersions[index] = v.SpecArtifactID, v.SpecVersion
	}
	query := insertVersionsQuery
	if again {
		query += insertAgainSQL
	}
	tag, err := tx.Exec(ctx, query, kinds, ids, issueKeys, projects, eventIDs, seqs, startedAts, bodies,
		bodyFroms, specArtifacts, specVersions)
	if err != nil {
		return fmt.Errorf("write %d keyword versions: %w", n, err)
	}
	if !again && tag.RowsAffected() != int64(n) {
		return fmt.Errorf("write %d keyword versions: the insert wrote %d", n, tag.RowsAffected())
	}
	return nil
}

// lockCursor locks the cursor row for tx, creating it at no event where none is, with the fill's
// rules version, and answers the cursor, the created_at of the event at it, the reach stored and
// the rules version stored. A row a reset emptied and wrote again while this lock waited is gone
// from the lock's snapshot, so it is read again.
func (f *filler) lockCursor(ctx context.Context, tx pgx.Tx) (cursor int64, cursorAt, reach *time.Time, rules int, err error) {
	for attempt := 0; ; attempt++ {
		err = tx.QueryRow(ctx, lockCursorQuery).Scan(&cursor, &cursorAt, &reach, &rules)
		switch {
		case err == nil:
			return cursor, cursorAt, reach, rules, nil
		case !errors.Is(err, pgx.ErrNoRows) || attempt > 0:
			return 0, nil, nil, 0, fmt.Errorf("keyword fill: lock the cursor: %w", err)
		}
		if _, err := tx.Exec(ctx, createCursorQuery, f.holderArg(), f.rules.version); err != nil {
			return 0, nil, nil, 0, fmt.Errorf("keyword fill: create the cursor: %w", err)
		}
	}
}

// empty empties both tables in tx for a log that was reset, and writes the cursor row again at no
// event, with no reach (createCursorQuery).
func (f *filler) empty(ctx context.Context, tx pgx.Tx) error {
	for _, statement := range []string{emptyTableQuery, emptyCursorQuery} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("keyword fill: empty the table of a reset log: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, createCursorQuery, f.holderArg(), f.rules.version); err != nil {
		return fmt.Errorf("keyword fill: start the cursor of a reset log: %w", err)
	}
	return nil
}

// holderArg is the holder the cursor row records: this fill's, or null with no hold.
func (f *filler) holderArg() any {
	if f.holder == uuid.Nil {
		return nil
	}
	return f.holder
}

// statementTimeout is the statement_timeout of a batch that counts bytes of text against its
// bound (batchBytes): 10 s and 1 s more for each MB, as set_config takes it.
func statementTimeout(bytes int64) string {
	const mb = 1 << 20
	timeout := batchStatementBase + time.Duration((bytes+mb-1)/mb)*batchStatementPerMB
	return fmt.Sprintf("%dms", timeout.Milliseconds())
}

// ruleTypes is the event types a rule of this build reads (versionRules), sorted: the types whose
// payload readEventsQuery counts.
var ruleTypes = currentRules.types

// askTypes is the types of ruleTypes that are an ask's (ask.*), whose rule is askVersion: the
// types readEventsQuery reads the ask's body of (askBodySQL).
var askTypes = currentRules.askTypes

// readEvents reads the batch's events above cursor up to safe (readEventsQuery), each with the
// bytes the read counted of it, and how many events the read took before its bound on bytes, so
// a batch cut short by its bytes is known to leave events for the next one.
func (f *filler) readEvents(ctx context.Context, tx pgx.Tx, cursor, safe int64) ([]event, int, error) {
	return f.readLog(ctx, tx, readEventsQuery, cursor, safe, f.rules.types, f.rules.askTypes)
}

// readLog reads events above after up to through with query, readEventsQuery or its re-read's
// shape (rereadEventsQuery), within the batch's bounds, counting the payloads of types and reading
// the ask's body of asks.
func (f *filler) readLog(ctx context.Context, tx pgx.Tx, query string, after, through int64, types, asks []string) ([]event, int, error) {
	rows, err := tx.Query(ctx, query, after, through, f.maxEvents, f.maxBytes, types, asks)
	if err != nil {
		return nil, 0, fmt.Errorf("keyword fill: read the events after event %d: %w", after, err)
	}
	defer rows.Close()
	read := make([]event, 0, f.maxEvents)
	var candidates int
	for rows.Next() {
		var e event
		var payload []byte
		var taken int64
		if err := rows.Scan(&e.ID, &e.Type, &e.IssueKey, &e.ArtifactID, &e.Project, &e.Seq, &e.CreatedAt, &payload, &e.AskBody,
			&e.PayloadBytes, &e.RowBodyBytes, &taken); err != nil {
			return nil, 0, fmt.Errorf("keyword fill: read the events after event %d: %w", after, err)
		}
		e.Payload = payload
		read = append(read, e)
		candidates = int(taken)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("keyword fill: read the events after event %d: %w", after, err)
	}
	return read, candidates, nil
}

// record logs a batch that wrote events: a debug line for each batch, which a measurement reads;
// a progress line at most each progressEvery while the fill catches up, with what the batches
// since the last one wrote; and an error for events written at or before the reach stored before,
// each a breach of what the reach said. A re-read batch is logged at INFO alone (rereadMessage):
// it runs only after a fill of a lower rules version wrote, and a search at a cut up to the reach
// missed the versions it writes, which the line counts.
func (f *filler) record(r batchResult) {
	if r.reread {
		f.logger.Info(rereadMessage,
			"events", r.events, "first_event", r.first, "cursor", r.cursor, "rows", r.rows, "redone", r.redone,
			"cut_bodies", r.cut, "bytes", r.bytes, "raised", r.raised, "batch_ms", r.took.Milliseconds())
		return
	}
	f.logger.Debug(batchMessage,
		"events", r.events, "first_event", r.first, "cursor", r.cursor, "rows", r.rows, "cut_bodies", r.cut,
		"bytes", r.bytes, "reach", r.reach, "batch_ms", r.took.Milliseconds())
	now := time.Now()
	if f.progress.since.IsZero() {
		f.progress.since = now
	}
	f.progress.events += r.events
	f.progress.rows += r.rows
	f.progress.cut += r.cut
	if r.full && now.Sub(f.progress.since) >= progressEvery {
		f.logger.Info("dispatch: the keyword fill is catching up",
			"events", f.progress.events, "rows", f.progress.rows, "cut_bodies", f.progress.cut,
			"cursor", r.cursor, "reach", r.reach, "batch_ms", r.took.Milliseconds())
		f.progress = progress{since: now}
	}
	if r.breaches > 0 {
		f.breaches += r.breaches
		f.logger.Error("dispatch: the keyword fill wrote events whose created_at is at or before the reach it had stored; a search at a cut up to that reach missed their versions",
			"breaches", r.breaches, "breaches_total", f.breaches, "stored_reach", r.storedReach,
			"first_event", r.first, "cursor", r.cursor)
	}
}
