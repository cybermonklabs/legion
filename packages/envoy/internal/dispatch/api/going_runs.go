package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A going run's record is one answered pause of the command bar: what the person typed and where,
// what the run found, what the decision model chose, what the answer offered, every write the
// person let it make and every undo of one. It is a row of its own (going_runs, migration
// 20261112173940) because the going request it shares an id with is swept an hour after it is
// asked (sweepGoingRequests), and a turn is undone from any page and after a reload, days later.
//
// Every read and write of that record is here. The run writes the row when it starts
// (recordGoingRun) and fills it as it goes (fillGoingRun), so a run that is abandoned keeps what
// it stored and reads as unfinished. A write and an undo each claim what they are about to change
// before they change it, in one statement that holds no lock and no connection past itself: a
// write appends itself pending (claimGoingRunWrite), unless the run already holds a pending or a
// successful write of that invocation, and an undo appends a pending undo of each write it is
// about to undo (claimGoingRunUndos), unless another undo of the run is under way. Each then
// records how it went in the place of its claim (recordGoingRunWrite, recordGoingRunUndo), or
// gives the claim up where it made nothing (releaseGoingRunWrite, releaseGoingRunUndos). GET
// /api/v1/me/going-runs is the one route that reads a row, and it reads the caller's own alone.

// goingRunsKept is how long a run's record is kept: a row older than this is read by nobody,
// even before sweepGoingRuns deletes it, and takes no write or undo.
const goingRunsKept = 30 * 24 * time.Hour

// goingRunColumns is the going_runs select list scanGoingRun reads, in scan order.
const goingRunColumns = `r.id::text, r.author_login, r.words, r.context, r.hits, r.decision,
	r.proposal, r.writes, r.undos, r.issues, r.structured_output, r.created_at, r.finished_at`

// goingContext is the item the bar's context chip names when the person paused: the page's
// path, and the project, issue and document block that page shows, each empty where it shows
// none.
type goingContext struct {
	Path    string `json:"path"`
	Project string `json:"project"`
	Issue   string `json:"issue"`
	Block   string `json:"block"`
}

// goingRunHit is one hit the run found, as the stream's item frame names it.
type goingRunHit struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title"`
	Href  string `json:"href"`
}

// goingWriteOutcome is how one write or one undo went: ok, and the HTTP status the route it ran
// through answered, with the refusal's code and message where it refused (both empty when ok).
type goingWriteOutcome struct {
	OK      bool   `json:"ok"`
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// goingRunWrite is one write a run made: the write store's id for it, the catalogue action it
// ran and the index of the invocation of that action's offer it ran, the item it changed (target)
// and the fields of that item it changed, the value before it and the value after it as read
// back, how it went, how it came to run - the run made it at once (auto), the person ran it from
// the bar (person), or the person confirmed it when asked (confirmed) - when, the items it made
// before its own request, for a send to an agent the message's last delivery, and the undo the
// row it went through states.
//
// Pending is a write claimed and not yet recorded: the request making it is under way, or it
// ended - its process died, or its record could not be written - after its write may have
// landed. Its outcome is unknown, so it is never written again: a request for its invocation is
// answered that it is under way, and once the request that claimed it can no longer be running
// (goingClaimBound) that its outcome is unknown.
type goingRunWrite struct {
	ID         string            `json:"id"`
	Action     string            `json:"action"`
	Invocation int               `json:"invocation"`
	Target     string            `json:"target"`
	Fields     []string          `json:"fields"`
	Before     json.RawMessage   `json:"before"`
	After      json.RawMessage   `json:"after"`
	Outcome    goingWriteOutcome `json:"outcome"`
	Approved   string            `json:"approved" enum:"auto,person,confirmed"`
	Pending    bool              `json:"pending"`
	At         time.Time         `json:"at"`
	// Landed is the id of each item a write sent before the row's own made: the message a
	// reasoned close posts as its reason. It stands whether the row's own request then went
	// through or was refused, so a refused close still names the reason it left posted.
	Landed []string `json:"landed,omitempty"`
	// Delivery is the last delivery of the message a send to an agent wrote, absent for every
	// other write and for a message that has none: the route answers a send 201 even where its
	// delivery failed, so this is what tells a failed send.
	Delivery *goingDelivery `json:"delivery,omitempty"`
	// Undo is the undo the catalogue row the write went through states (GoingUndo.Kind), which
	// is the undo it gets: the rows of one action may state different undos. It is absent for a
	// write that never reached a row - one refused before its row was chosen and translated.
	Undo goingUndoKind `json:"undo,omitempty" enum:"reverse,write_back,close_created,trace"`
	// AfterUnread is a write that landed and whose value after could not be read: After holds
	// nothing the write left, so its undo cannot tell what to put back and undoes nothing
	// (codeGoingUndoAfterUnread).
	AfterUnread bool `json:"after_unread,omitempty"`
}

// goingDelivery is one delivery of a message as a send to an agent records it: its state
// (`delivered`, `failed`, ...) and the error a failed one gave, null for none.
type goingDelivery struct {
	State string  `json:"state"`
	Error *string `json:"error"`
}

// goingRunUndo is one undo of a write of the run, named by the write's id, with how it went,
// field by field where the write changed several, and when. A write is undone once an undo of it
// is ok, and settled once an undo of it reached a verdict that would only repeat
// (goingUndoSettles) - ok, or refused because another writer changed what the write left - so no
// later undo of the turn tries it again; an undo that failed short of a verdict (the server's own
// failure, or a refusal that says only that this attempt was not heard) leaves it undoable.
// Pending is an undo claimed and not yet recorded (claimGoingRunUndos), which holds off a second
// undo of the run until it is recorded or goingClaimBound has passed.
type goingRunUndo struct {
	WriteID string            `json:"write_id"`
	Outcome goingWriteOutcome `json:"outcome"`
	Settled bool              `json:"settled"`
	Pending bool              `json:"pending"`
	Fields  []goingUndoField  `json:"fields"`
	At      time.Time         `json:"at"`
}

// goingUndoField is how the undo of one field of a write went: put back (ok), or left as another
// writer left it, with what it holds now named in message.
type goingUndoField struct {
	Field   string `json:"field"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// goingRun is one run's record as GET /api/v1/me/going-runs answers it: every part stored, null
// where the run has not written it, finished whether it ended (finished_at is set), and issues
// every issue key a hit or a write of the run touched.
type goingRun struct {
	ID               string          `json:"id"`
	AuthorLogin      string          `json:"author_login"`
	Words            string          `json:"words"`
	Context          *goingContext   `json:"context"`
	Hits             []goingRunHit   `json:"hits"`
	Decision         *goingDecision  `json:"decision"`
	Proposal         *goingProposal  `json:"proposal"`
	Writes           []goingRunWrite `json:"writes"`
	Undos            []goingRunUndo  `json:"undos"`
	Issues           []string        `json:"issues"`
	StructuredOutput *string         `json:"structured_output"`
	Finished         bool            `json:"finished"`
	CreatedAt        time.Time       `json:"created_at"`
	FinishedAt       *time.Time      `json:"finished_at"`
}

// goingRunsPage is a page of the caller's runs, newest first, paged as GET /api/v1/me/answers is.
type goingRunsPage struct {
	Rows   []goingRun `json:"rows"`
	Total  int        `json:"total"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
}

// goingRunParts is what fillGoingRun writes onto a run: each part left nil keeps what the row
// holds, so the run writes each part as it has it. Finished sets finished_at, once: a second
// finish keeps the first moment.
type goingRunParts struct {
	Hits             []going.Hit
	Decision         *goingDecision
	Proposal         *goingProposal
	StructuredOutput *string
	Finished         bool
}

// goingRunFilter is what GET /api/v1/me/going-runs narrows a person's runs to: one issue key the
// run touched, a case-insensitive substring of its words, and whether it holds a write that can
// still be undone. The zero value narrows nothing.
type goingRunFilter struct {
	Issue    string
	Q        string
	Undoable bool
}

// goingRunWindow is the condition that keeps a row past goingRunsKept out of every read and
// write here, $n being goingRunsKept in seconds.
func goingRunWindow(n int) string {
	return fmt.Sprintf("r.created_at > now() - make_interval(secs => $%d)", n)
}

// goingRunUndoable holds where a run has a write that went through and that no undo has
// settled: an undo that was ok, or refused for what another writer changed, settles its write;
// one that failed short of a verdict leaves it undoable.
const goingRunUndoable = `exists (
	select 1 from jsonb_array_elements(r.writes) w
	where (w->'outcome'->>'ok')::boolean
	  and not exists (
	    select 1 from jsonb_array_elements(r.undos) u
	    where u->>'write_id' = w->>'id'
	      and ((u->'outcome'->>'ok')::boolean or coalesce((u->>'settled')::boolean, false))
	  )
)`

// recordGoingRun writes a run's row when the run starts, under its going request's id, keyed on
// the asking person's canonical login. Every other part is written as the run reaches it.
func (s *server) recordGoingRun(ctx context.Context, id, authorLogin, words string, chip *goingContext) error {
	var stored any
	if chip != nil {
		encoded, err := json.Marshal(chip)
		if err != nil {
			return err
		}
		stored = string(encoded)
	}
	_, err := s.deps.Store.Pool.Exec(ctx, `
		insert into going_runs (id, author_login, words, context) values ($1::uuid, $2, $3, $4::jsonb)
	`, id, authorLogin, words, stored)
	return err
}

// fillGoingRun writes the parts the run has reached onto its row, adding the key of every issue
// among the hits to the run's issues. A run the record does not hold is pgx.ErrNoRows.
func (s *server) fillGoingRun(ctx context.Context, id string, parts goingRunParts) error {
	var hits any
	var hitIssues []string
	if parts.Hits != nil {
		rows := make([]goingRunHit, 0, len(parts.Hits))
		for _, hit := range parts.Hits {
			rows = append(rows, goingRunHit{Kind: hit.Kind, ID: hit.ID, Title: hit.Title, Href: hit.Href})
			if hit.Kind == "issue" {
				hitIssues = append(hitIssues, hit.ID)
			}
		}
		encoded, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		hits = string(encoded)
	}
	decision, err := nullableJSON(parts.Decision)
	if err != nil {
		return err
	}
	proposal, err := nullableJSON(parts.Proposal)
	if err != nil {
		return err
	}
	tag, err := s.deps.Store.Pool.Exec(ctx, `
		update going_runs set
		  hits = coalesce($2::jsonb, hits),
		  decision = coalesce($3::jsonb, decision),
		  proposal = coalesce($4::jsonb, proposal),
		  structured_output = coalesce($5::text, structured_output),
		  finished_at = case when $6::boolean then coalesce(finished_at, now()) else finished_at end,
		  issues = array(select distinct key from unnest(issues || $7::text[]) key order by key)
		where id = $1::uuid
	`, id, hits, decision, proposal, parts.StructuredOutput, parts.Finished, hitIssues)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// goingClaimBound is how long a claim on a run (claimGoingRunWrite, claimGoingRunUndos) speaks for
// a request still under way: the longest a keyed request's handler runs (keyedRunLimit), which
// the writes and undo routes hold their unkeyed requests to as well, and the bookkeeping after it
// (keyedBookkeeping, goingBookkeeping) in which that request records what it did. A pending write
// older than it is answered as a write whose outcome is unknown, and no record of it can follow
// that answer; a pending undo older than it no longer holds off another undo of the run.
func (s *server) goingClaimBound() time.Duration {
	return s.keyedRunLimit() + keyedBookkeeping
}

// goingRunWriteClaimed is the condition that holds where the run r already holds a write of
// offer $o and invocation $i that went through or is pending, which a second write of that
// invocation must not be made beside.
func goingRunWriteClaimed(offer, invocation int) string {
	return fmt.Sprintf(`exists (
		select 1 from jsonb_array_elements(r.writes) w
		where w->>'action' = $%d and (w->>'invocation')::int = $%d
		  and ((w->'outcome'->>'ok')::boolean or coalesce((w->>'pending')::boolean, false))
	)`, offer, invocation)
}

// claimGoingRunWrite appends write, pending, to the author's run in one statement, unless the run
// already holds a write of that invocation that went through or is pending: the claim is the
// statement itself, so of two requests for one invocation one claims it and the other finds it
// claimed. Either refusal - the invocation claimed, or a run that is not the author's, that the
// record does not hold, or that is older than goingRunsKept - is pgx.ErrNoRows, which the caller
// tells apart by reading the run again. The claim marks no Idempotency-Key committed
// (store.WithoutBeforeCommit): it writes nothing a repeat must not make again, and the write it
// claims marks the key itself when it lands.
func (s *server) claimGoingRunWrite(ctx context.Context, id, authorLogin string, write goingRunWrite) error {
	write.Pending = true
	encoded, err := json.Marshal(write)
	if err != nil {
		return err
	}
	return s.appendToGoingRun(store.WithoutBeforeCommit(ctx), `
		update going_runs r set writes = r.writes || jsonb_build_array($3::jsonb)
		where r.id = $1::uuid and r.author_login = $2 and `+goingRunWindow(4)+`
		  and not `+goingRunWriteClaimed(5, 6),
		id, authorLogin, string(encoded), goingRunsKept.Seconds(), write.Action, write.Invocation)
}

// recordGoingRunWrite records write on the author's run in the place of its claim, the element of
// its id, or appends it where the run holds none, and adds issue, the key of the issue it changed
// (empty for a write that changes none), to the run's issues, in one statement, so two writes of
// one turn recorded at once both land. A run that is not the author's, that the record does not
// hold, or that is older than goingRunsKept takes nothing and is pgx.ErrNoRows.
func (s *server) recordGoingRunWrite(ctx context.Context, id, authorLogin string, write goingRunWrite, issue string) error {
	write.Pending = false
	encoded, err := json.Marshal(write)
	if err != nil {
		return err
	}
	return s.appendToGoingRun(ctx, `
		update going_runs r set
		  writes = case
		    when exists (select 1 from jsonb_array_elements(r.writes) w where w->>'id' = $6)
		    then (select jsonb_agg(case when w->>'id' = $6 then $3::jsonb else w end order by n)
		          from jsonb_array_elements(r.writes) with ordinality e(w, n))
		    else r.writes || jsonb_build_array($3::jsonb)
		  end,
		  issues = case when $4::text = '' or $4::text = any(r.issues) then r.issues else array_append(r.issues, $4::text) end
		where r.id = $1::uuid and r.author_login = $2 and `+goingRunWindow(5),
		id, authorLogin, string(encoded), strings.TrimSpace(issue), goingRunsKept.Seconds(), write.ID)
}

// releaseGoingRunWrite gives up the claim of a write that made nothing - one its request failed
// before sending, or its own schema refused - so its invocation may be written again. Only a
// pending write is taken off; a run that holds none of that id is left as it is. Like the claim,
// it marks no Idempotency-Key committed.
func (s *server) releaseGoingRunWrite(ctx context.Context, id, authorLogin, writeID string) error {
	return goingReleased(s.appendToGoingRun(store.WithoutBeforeCommit(ctx), `
		update going_runs r set writes = (
		  select coalesce(jsonb_agg(w order by n), '[]'::jsonb)
		  from jsonb_array_elements(r.writes) with ordinality e(w, n)
		  where not (w->>'id' = $3 and coalesce((w->>'pending')::boolean, false))
		)
		where r.id = $1::uuid and r.author_login = $2
		  and exists (select 1 from jsonb_array_elements(r.writes) w
		              where w->>'id' = $3 and coalesce((w->>'pending')::boolean, false))`,
		id, authorLogin, writeID))
}

// goingReleased is a release's error: a release that found nothing to take off has nothing left
// to do, which is no failure.
func goingReleased(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// goingUndoClaimedAt is the moment a pending undo was claimed at as its record spells it, which is
// how the request that claimed it finds its own claim again: JSON's spelling of a time.
func goingUndoClaimedAt(at time.Time) string {
	return at.Format(time.RFC3339Nano)
}

// goingRunUndoPendingAt is the condition that holds of the undo element u where it is the pending
// claim made at $at.
func goingRunUndoPendingAt(at int) string {
	return fmt.Sprintf(`(coalesce((u->>'pending')::boolean, false) and u->>'at' = $%d)`, at)
}

// claimGoingRunUndos appends a pending undo of each of writes, claimed at, to the author's run in
// one statement, unless an undo of the run claimed within goingClaimBound is still pending, or one
// of writes already has an undo that was ok or settled since the caller read the run. A pending
// undo of one of writes older than the bound is taken off in the same statement. Either refusal,
// like a run that is not the author's, that the record does not hold or that is older than
// goingRunsKept, is pgx.ErrNoRows, which the caller tells apart by reading the run again. The
// claim marks no Idempotency-Key committed (store.WithoutBeforeCommit), since it writes nothing a
// repeat must not make again.
func (s *server) claimGoingRunUndos(ctx context.Context, id, authorLogin string, writes []string, at time.Time) error {
	pending := make([]goingRunUndo, 0, len(writes))
	for _, write := range writes {
		pending = append(pending, goingRunUndo{WriteID: write, Pending: true, Fields: []goingUndoField{}, At: at})
	}
	encoded, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	return s.appendToGoingRun(store.WithoutBeforeCommit(ctx), `
		update going_runs r set undos = (
		  select coalesce(jsonb_agg(u order by n), '[]'::jsonb)
		  from jsonb_array_elements(r.undos) with ordinality e(u, n)
		  where not (coalesce((u->>'pending')::boolean, false) and u->>'write_id' = any($3::text[]))
		) || $4::jsonb
		where r.id = $1::uuid and r.author_login = $2 and `+goingRunWindow(5)+`
		  and not exists (
		    select 1 from jsonb_array_elements(r.undos) u
		    where coalesce((u->>'pending')::boolean, false)
		      and (u->>'at')::timestamptz > now() - make_interval(secs => $6))
		  and not exists (
		    select 1 from jsonb_array_elements(r.undos) u
		    where u->>'write_id' = any($3::text[])
		      and ((u->'outcome'->>'ok')::boolean or coalesce((u->>'settled')::boolean, false)))`,
		id, authorLogin, writes, string(encoded), goingRunsKept.Seconds(), s.goingClaimBound().Seconds())
}

// recordGoingRunUndo records one undo on the author's run in the place of its pending claim made
// at claimedAt (goingUndoClaimedAt), or appends it where the run holds no such claim - none was
// made, or another undo took over a claim past goingClaimBound - in one statement. An undo of a
// write the run does not hold is refused with the rest: a run that is not the author's, that the
// record does not hold, or that is older than goingRunsKept takes nothing, and each is
// pgx.ErrNoRows.
func (s *server) recordGoingRunUndo(ctx context.Context, id, authorLogin string, undo goingRunUndo, claimedAt string) error {
	undo.Pending = false
	encoded, err := json.Marshal(undo)
	if err != nil {
		return err
	}
	claim := `u->>'write_id' = $4 and ` + goingRunUndoPendingAt(6)
	return s.appendToGoingRun(ctx, `
		update going_runs r set undos = case
		  when exists (select 1 from jsonb_array_elements(r.undos) u where `+claim+`)
		  then (select jsonb_agg(case when `+claim+` then $3::jsonb else u end order by n)
		        from jsonb_array_elements(r.undos) with ordinality e(u, n))
		  else r.undos || jsonb_build_array($3::jsonb)
		end
		where r.id = $1::uuid and r.author_login = $2 and `+goingRunWindow(5)+`
		  and exists (select 1 from jsonb_array_elements(r.writes) w where w->>'id' = $4)`,
		id, authorLogin, string(encoded), undo.WriteID, goingRunsKept.Seconds(), claimedAt)
}

// releaseGoingRunUndos takes off the pending undo of each of writes claimed at claimedAt, the
// writes its request did not reach, so they are undoable again at once rather than after
// goingClaimBound. The claim of an undo the request made and could not record is not among them:
// it stays pending, so no undo makes it again while the request may still be running. Like the
// claim, it marks no Idempotency-Key committed.
func (s *server) releaseGoingRunUndos(ctx context.Context, id, authorLogin, claimedAt string, writes []string) error {
	released := `(` + goingRunUndoPendingAt(3) + ` and u->>'write_id' = any($4::text[]))`
	return goingReleased(s.appendToGoingRun(store.WithoutBeforeCommit(ctx), `
		update going_runs r set undos = (
		  select coalesce(jsonb_agg(u order by n), '[]'::jsonb)
		  from jsonb_array_elements(r.undos) with ordinality e(u, n)
		  where not `+released+`
		)
		where r.id = $1::uuid and r.author_login = $2
		  and exists (select 1 from jsonb_array_elements(r.undos) u where `+released+`)`,
		id, authorLogin, claimedAt, writes))
}

// appendToGoingRun runs one append to a run in a transaction of the shared pool, so a keyed
// writes or undo request marks its Idempotency-Key committed with the append
// (markRequestKeyCommitted): a repeat finds the write recorded rather than recording it twice.
// An append that matched no row is pgx.ErrNoRows.
func (s *server) appendToGoingRun(ctx context.Context, statement string, arguments ...any) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, statement, arguments...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}

// readGoingRun is the author's one run by id: pgx.ErrNoRows for a run of anyone else's, one the
// record does not hold, and one older than goingRunsKept, which are one answer to whoever asks.
func (s *server) readGoingRun(ctx context.Context, id, authorLogin string) (goingRun, error) {
	return scanGoingRun(s.deps.Store.Pool.QueryRow(ctx, `
		select `+goingRunColumns+` from going_runs r
		where r.id = $1::uuid and r.author_login = $2 and `+goingRunWindow(3),
		id, authorLogin, goingRunsKept.Seconds()))
}

// listGoingRuns is one page of the author's runs inside goingRunsKept, newest first, narrowed by
// filter, with the count of every run the filter keeps.
func (s *server) listGoingRuns(ctx context.Context, authorLogin string, filter goingRunFilter, limit, offset int) ([]goingRun, int, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select `+goingRunColumns+`, count(*) over () as total
		from going_runs r
		where r.author_login = $1 and `+goingRunWindow(2)+`
		  and ($3::text = '' or r.issues @> array[$3::text])
		  and ($4::text = '' or strpos(lower(r.words), lower($4::text)) > 0)
		  and (not $5::boolean or `+goingRunUndoable+`)
		order by r.created_at desc, r.id desc
		limit $6 offset $7
	`, authorLogin, goingRunsKept.Seconds(), filter.Issue, filter.Q, filter.Undoable, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	runs := []goingRun{}
	total := 0
	for rows.Next() {
		var count int64
		run, err := scanGoingRun(rows, &count)
		if err != nil {
			return nil, 0, err
		}
		total = int(count)
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return runs, total, nil
}

// listMyGoingRuns answers GET /api/v1/me/going-runs: the caller's own runs, newest first, keyed
// on the person actingPerson names. It takes no login: no caller reads another person's runs.
func (s *server) listMyGoingRuns(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	limit, offset, err := parseMyAnswersPage(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	query := r.URL.Query()
	filter := goingRunFilter{Issue: strings.TrimSpace(query.Get("issue")), Q: strings.TrimSpace(query.Get("q"))}
	if value, present, valid := parseQueryBool(query, "undoable", false, false); present {
		if !valid {
			s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY", "undoable must be true or false"))
			return
		}
		filter.Undoable = value
	}
	runs, total, err := s.listGoingRuns(r.Context(), caller.ID, filter, limit, offset)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list my going runs: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, goingRunsPage{Rows: runs, Total: total, Limit: limit, Offset: offset})
}

// scanGoingRun decodes one goingRunColumns row, and into extra any columns the select adds after
// them.
func scanGoingRun(row pgx.Row, extra ...any) (goingRun, error) {
	var run goingRun
	var chip, hits, writes, undos []byte
	var decision, proposal []byte
	targets := append([]any{
		&run.ID, &run.AuthorLogin, &run.Words, &chip, &hits, &decision, &proposal, &writes, &undos,
		&run.Issues, &run.StructuredOutput, &run.CreatedAt, &run.FinishedAt,
	}, extra...)
	if err := row.Scan(targets...); err != nil {
		return goingRun{}, err
	}
	if chip != nil {
		run.Context = &goingContext{}
		if err := json.Unmarshal(chip, run.Context); err != nil {
			return goingRun{}, fmt.Errorf("decode going run context: %w", err)
		}
	}
	if hits != nil {
		if err := json.Unmarshal(hits, &run.Hits); err != nil {
			return goingRun{}, fmt.Errorf("decode going run hits: %w", err)
		}
	}
	if err := json.Unmarshal(writes, &run.Writes); err != nil {
		return goingRun{}, fmt.Errorf("decode going run writes: %w", err)
	}
	if err := json.Unmarshal(undos, &run.Undos); err != nil {
		return goingRun{}, fmt.Errorf("decode going run undos: %w", err)
	}
	if decision != nil {
		run.Decision = &goingDecision{}
		if err := json.Unmarshal(decision, run.Decision); err != nil {
			return goingRun{}, fmt.Errorf("decode going run decision: %w", err)
		}
	}
	if proposal != nil {
		run.Proposal = &goingProposal{}
		if err := json.Unmarshal(proposal, run.Proposal); err != nil {
			return goingRun{}, fmt.Errorf("decode going run proposal: %w", err)
		}
	}
	run.Finished = run.FinishedAt != nil
	return run, nil
}

// nullableJSON is part as a jsonb parameter: SQL null where the run has not reached it (nil), so a
// coalesce keeps what the row holds.
func nullableJSON[T any](part *T) (any, error) {
	if part == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(part)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil
}

// sweepGoingRuns deletes every run older than goingRunsKept. The reads above already leave such
// a run out, so the sweep only keeps the table from growing.
func sweepGoingRuns(ctx context.Context, pool *store.Pool) error {
	_, err := pool.Exec(ctx, `
		delete from going_runs where created_at < now() - make_interval(secs => $1)
	`, goingRunsKept.Seconds())
	return err
}
