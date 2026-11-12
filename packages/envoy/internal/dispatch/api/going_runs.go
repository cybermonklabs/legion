package api

import (
	"context"
	"encoding/json"
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
// it stored and reads as unfinished; each write and each undo is appended in one statement
// (appendGoingRunWrite, appendGoingRunUndo). GET /api/v1/me/going-runs is the one route that
// reads a row, and it reads the caller's own alone.

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
// ran, the item it changed (target) and the field of that item, the value before it and the
// value after it as read back, how it went, how it came to run - the run made it at once
// (auto), the person ran it from the bar (person), or the person confirmed it when asked
// (confirmed) - and when.
type goingRunWrite struct {
	ID       string            `json:"id"`
	Action   string            `json:"action"`
	Target   string            `json:"target"`
	Field    string            `json:"field"`
	Before   json.RawMessage   `json:"before"`
	After    json.RawMessage   `json:"after"`
	Outcome  goingWriteOutcome `json:"outcome"`
	Approved string            `json:"approved" enum:"auto,person,confirmed"`
	At       time.Time         `json:"at"`
}

// goingRunUndo is one undo of a write of the run, named by the write's id, with how it went and
// when. A write is undone once an undo of it is ok; an undo that failed leaves it undoable.
type goingRunUndo struct {
	WriteID string            `json:"write_id"`
	Outcome goingWriteOutcome `json:"outcome"`
	At      time.Time         `json:"at"`
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
	Decision         json.RawMessage `json:"decision"`
	Proposal         json.RawMessage `json:"proposal"`
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
	Decision         json.RawMessage
	Proposal         json.RawMessage
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
// reverted: an undo that failed leaves its write undoable.
const goingRunUndoable = `exists (
	select 1 from jsonb_array_elements(r.writes) w
	where (w->'outcome'->>'ok')::boolean
	  and not exists (
	    select 1 from jsonb_array_elements(r.undos) u
	    where u->>'write_id' = w->>'id' and (u->'outcome'->>'ok')::boolean
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
	tag, err := s.deps.Store.Pool.Exec(ctx, `
		update going_runs set
		  hits = coalesce($2::jsonb, hits),
		  decision = coalesce($3::jsonb, decision),
		  proposal = coalesce($4::jsonb, proposal),
		  structured_output = coalesce($5::text, structured_output),
		  finished_at = case when $6::boolean then coalesce(finished_at, now()) else finished_at end,
		  issues = array(select distinct key from unnest(issues || $7::text[]) key order by key)
		where id = $1::uuid
	`, id, hits, nullableJSON(parts.Decision), nullableJSON(parts.Proposal), parts.StructuredOutput,
		parts.Finished, hitIssues)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// appendGoingRunWrite appends one write to the author's run, and issue, the key of the issue it
// changed (empty for a write that changes none), to the run's issues, in one statement, so two
// writes of one turn appended at once both land. A run that is not the author's, that the record
// does not hold, or that is older than goingRunsKept takes nothing and is pgx.ErrNoRows.
func (s *server) appendGoingRunWrite(ctx context.Context, id, authorLogin string, write goingRunWrite, issue string) error {
	encoded, err := json.Marshal(write)
	if err != nil {
		return err
	}
	tag, err := s.deps.Store.Pool.Exec(ctx, `
		update going_runs r set
		  writes = r.writes || jsonb_build_array($3::jsonb),
		  issues = case when $4::text = '' or $4::text = any(r.issues) then r.issues else array_append(r.issues, $4::text) end
		where r.id = $1::uuid and r.author_login = $2 and `+goingRunWindow(5),
		id, authorLogin, string(encoded), strings.TrimSpace(issue), goingRunsKept.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// appendGoingRunUndo appends one undo to the author's run in one statement. An undo of a write
// the run does not hold is refused with the rest: a run that is not the author's, that the record
// does not hold, or that is older than goingRunsKept takes nothing, and each is pgx.ErrNoRows.
func (s *server) appendGoingRunUndo(ctx context.Context, id, authorLogin string, undo goingRunUndo) error {
	encoded, err := json.Marshal(undo)
	if err != nil {
		return err
	}
	tag, err := s.deps.Store.Pool.Exec(ctx, `
		update going_runs r set undos = r.undos || jsonb_build_array($3::jsonb)
		where r.id = $1::uuid and r.author_login = $2 and `+goingRunWindow(5)+`
		  and exists (select 1 from jsonb_array_elements(r.writes) w where w->>'id' = $4)`,
		id, authorLogin, string(encoded), undo.WriteID, goingRunsKept.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
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
	run.Decision = json.RawMessage(decision)
	run.Proposal = json.RawMessage(proposal)
	run.Finished = run.FinishedAt != nil
	return run, nil
}

// nullableJSON is raw as a jsonb parameter: SQL null where it holds nothing, so a coalesce keeps
// what the row holds.
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// sweepGoingRuns deletes every run older than goingRunsKept. The reads above already leave such
// a run out, so the sweep only keeps the table from growing.
func sweepGoingRuns(ctx context.Context, pool *store.Pool) error {
	_, err := pool.Exec(ctx, `
		delete from going_runs where created_at < now() - make_interval(secs => $1)
	`, goingRunsKept.Seconds())
	return err
}
