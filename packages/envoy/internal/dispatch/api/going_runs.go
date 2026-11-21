package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A going run is one run of the command bar's agent (going_runs, migrations 20261112173940 and
// 20261119201453): what its person asked and where, its level and mode, the conversation it
// belongs to and the run it continues, where it is (its state and why it stopped), and what it
// spent of its level's budget, beside its steps, events, grant, landings and plans. A conversation
// is a chain of runs, each a follow-up of the one before it, named by the id of its first run.
//
// The short answer of a pause in typing (R3) writes no row here: it is a going request with its
// going events (going.go). So every row here is a run, and the claim, History and the sweep read
// only runs. A row an earlier build wrote for a pause holds no state, and nothing here reads it.
//
// POST /api/v1/going/runs writes a run (going_run_stream.go); the agent works it
// (going_agent.go); GET /api/v1/me/going-runs, History, lists its person's conversations, each with
// its runs, while the conversation's newest run is younger than goingRunsKept; and the
// maintenance sweep deletes a conversation whole once that run is older (sweepGoingRuns). Only the
// run's person reads it.

// goingRunsKept is how long History keeps a conversation: it lists it, and every read here
// answers a run of it, until the conversation's newest run is older than this
// (goingConversationListed), and the sweep then deletes it whole.
const goingRunsKept = 7 * 24 * time.Hour

// goingRunColumns is the going_runs select list scanGoingRun reads, in scan order.
const goingRunColumns = `r.id::text, coalesce(r.conversation_id, r.id)::text, r.continues::text, r.words, r.context,
	coalesce(r.level, 'standard'), coalesce(r.mode, 'ask'), r.state, r.stop_reason, r.issues, r.budget_used,
	r.created_at, r.finished_at`

// goingContext is the item the bar's context chip names when the person asked: the page's path,
// and the project, issue and document block that page shows, each empty where it shows none.
type goingContext struct {
	Path    string `json:"path"`
	Project string `json:"project"`
	Issue   string `json:"issue"`
	Block   string `json:"block"`
}

// goingWriteOutcome is how one request of a write or of an undo went: ok, and the HTTP status the
// route it ran through answered, with the refusal's code and message where it refused (both empty
// when ok).
type goingWriteOutcome struct {
	OK      bool   `json:"ok"`
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// goingWriteRequest is one request a write sent, by method, path and the hex SHA-256 of its body
// (goingSend.digest), how it went, and the id of the item it made where it made one
// (goingMadeID). A request is the same request as another only where all three match
// (goingSameSend).
type goingWriteRequest struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	BodySHA256 string            `json:"body_sha256,omitempty"`
	Outcome    goingWriteOutcome `json:"outcome"`
	Made       string            `json:"made,omitempty"`
}

// goingSameSend reports whether the recorded request is the request spec sends: the same method,
// path and body digest. Every request this build records names its digest, so one that names none
// is no request's.
func goingSameSend(recorded goingWriteRequest, spec goingSend) bool {
	return recorded.Method == spec.method && recorded.Path == spec.path &&
		recorded.BodySHA256 != "" && recorded.BodySHA256 == spec.digest()
}

// goingDelivery is one delivery of a message as a send to an agent answers it: its state, as the
// route answered it (model.DeliveryStatePending, ...Sent, ...Queued or ...Failed - queued for a
// session the listener knows and that is not running, which gets the message when it runs
// again), and the error a failed one gave, null for none.
type goingDelivery struct {
	State string  `json:"state"`
	Error *string `json:"error"`
}

// goingRunUsed is what a run has spent of its level's budget: its model calls, the milliseconds
// it worked and the output tokens its model calls wrote. Time it waited for its person is in none
// of it.
type goingRunUsed struct {
	Calls     int   `json:"calls"`
	WorkingMS int64 `json:"working_ms"`
	Tokens    int   `json:"tokens"`
}

// goingRun is one run as its person reads it: its id, its conversation (the id of the
// conversation's first run) and the run it continues (null for a first run), the words and the
// context chip, its level and mode, its state and why it stopped (null where nothing stopped it),
// every issue key a landed request of it touched, what it spent, when it started, and when it
// ended (null while it is queued, works or waits).
type goingRun struct {
	ID           string        `json:"id"`
	Conversation string        `json:"conversation"`
	Continues    *string       `json:"continues"`
	Words        string        `json:"words"`
	Context      *goingContext `json:"context"`
	Level        string        `json:"level" enum:"fast,standard,deep"`
	Mode         string        `json:"mode" enum:"ask,auto_review"`
	State        string        `json:"state" enum:"queued,working,waiting,done,stopped,failed"`
	StopReason   *string       `json:"stop_reason"`
	Issues       []string      `json:"issues"`
	Used         goingRunUsed  `json:"used"`
	CreatedAt    time.Time     `json:"created_at"`
	FinishedAt   *time.Time    `json:"finished_at"`
}

// goingConversation is one conversation as History lists it: its id, when its newest run started,
// and each of its runs, newest first.
type goingConversation struct {
	ID       string     `json:"id"`
	NewestAt time.Time  `json:"newest_at"`
	Runs     []goingRun `json:"runs"`
}

// goingConversationsPage is a page of the caller's conversations, the one whose run started last
// first, paged as GET /api/v1/me/answers is.
type goingConversationsPage struct {
	Rows   []goingConversation `json:"rows"`
	Total  int                 `json:"total"`
	Limit  int                 `json:"limit"`
	Offset int                 `json:"offset"`
}

// goingRunFilter is what History narrows a person's conversations to: those with a run that
// touched one issue key, and those with a run whose words hold a case-insensitive substring. The
// zero value narrows nothing.
type goingRunFilter struct {
	Issue string
	Q     string
}

// goingConversationListed is the condition that holds of the run r while History lists its
// conversation: the conversation's newest run is younger than goingRunsKept, $n being
// goingRunsKept in seconds. A run stored before conversations names none, and is its own.
func goingConversationListed(n int) string {
	return fmt.Sprintf(`greatest(r.created_at, (select max(c.created_at) from going_runs c where c.conversation_id = r.conversation_id))
	  > now() - make_interval(secs => $%d)`, n)
}

// goingNewRun is a run as its start writes it: its id, its person's canonical login, the words
// and the chip, its kind (agentwire.RunNewIssue, or "" for a run of the words), its level and
// mode, the conversation it belongs to (its own id for a first run) and the run it continues (nil
// for a first run).
type goingNewRun struct {
	id, login, words  string
	chip              *goingContext
	kind, level, mode string
	conversation      string
	continues         *string
}

// insertGoingRun writes run in tx, queued at the shape this build saves (goingRunShape), as the
// request that starts it does, and answers the run as the insert wrote it.
func insertGoingRun(ctx context.Context, tx pgx.Tx, run goingNewRun) (goingRun, error) {
	chip, err := nullableJSON(run.chip)
	if err != nil {
		return goingRun{}, err
	}
	written, err := scanGoingRun(tx.QueryRow(ctx, `
		insert into going_runs as r (id, author_login, words, context, conversation_id, continues, kind, level, mode, state, shape)
		values ($1::uuid, $2, $3, $4::jsonb, $5::uuid, $6::uuid, nullif($7, ''), $8, $9, 'queued', $10)
		returning `+goingRunColumns,
		run.id, run.login, run.words, chip, run.conversation, run.continues, run.kind, run.level, run.mode, goingRunShape))
	if err != nil {
		return goingRun{}, fmt.Errorf("record the run: %w", err)
	}
	return written, nil
}

// nullableJSON is value as a jsonb parameter: its JSON text, or SQL null where value is nil.
func nullableJSON[T any](value *T) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil
}

// readGoingRun is the author's one run by id, while History lists its conversation: pgx.ErrNoRows
// for a run of anyone else's, one the record does not hold, one whose conversation History no
// longer lists, and a pause's row of an earlier build, which are one answer to whoever asks.
func readGoingRun(ctx context.Context, q goingQuerier, id, authorLogin string) (goingRun, error) {
	return scanGoingRun(q.QueryRow(ctx, `
		select `+goingRunColumns+` from going_runs r
		where r.id = $1::uuid and r.author_login = $2 and r.state is not null and `+goingConversationListed(3),
		id, authorLogin, goingRunsKept.Seconds()))
}

// listGoingConversations is one page of the author's conversations History lists, narrowed by
// filter, the one whose newest run started last first, each with its runs newest first, and the
// count of every conversation the filter keeps.
func (s *server) listGoingConversations(ctx context.Context, authorLogin string, filter goingRunFilter, limit, offset int) ([]goingConversation, int, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		with conversations as (
			select coalesce(r.conversation_id, r.id) as id, max(r.created_at) as newest
			from going_runs r
			where r.author_login = $1 and r.state is not null
			group by 1
			having max(r.created_at) > now() - make_interval(secs => $2)
			  and ($3::text = '' or bool_or(r.issues @> array[$3::text]))
			  and ($4::text = '' or bool_or(strpos(lower(r.words), lower($4::text)) > 0))
		), page as (
			select id, newest, count(*) over () as total from conversations
			order by newest desc, id desc
			limit $5 offset $6
		)
		select page.id::text, page.newest, page.total, `+goingRunColumns+`
		from page
		join going_runs r on coalesce(r.conversation_id, r.id) = page.id and r.author_login = $1 and r.state is not null
		order by page.newest desc, page.id desc, r.created_at desc, r.id desc
	`, authorLogin, goingRunsKept.Seconds(), filter.Issue, filter.Q, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	conversations := []goingConversation{}
	total := 0
	for rows.Next() {
		var id string
		var newest time.Time
		var count int64
		run, err := scanGoingRun(rows, &id, &newest, &count)
		if err != nil {
			return nil, 0, err
		}
		total = int(count)
		if last := len(conversations) - 1; last < 0 || conversations[last].ID != id {
			conversations = append(conversations, goingConversation{ID: id, NewestAt: newest, Runs: []goingRun{}})
		}
		conversations[len(conversations)-1].Runs = append(conversations[len(conversations)-1].Runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return conversations, total, nil
}

// listMyGoingRuns answers GET /api/v1/me/going-runs, History: the caller's own conversations of
// the last goingRunsKept, keyed on the person actingPerson names, each with its runs. It takes no
// login: no caller reads another person's conversations.
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
	conversations, total, err := s.listGoingConversations(r.Context(), model.CanonicalLogin(caller.ID), filter, limit, offset)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list my going conversations: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, goingConversationsPage{Rows: conversations, Total: total, Limit: limit, Offset: offset})
}

// scanGoingRun decodes one goingRunColumns row, read after the columns into lead, which a select
// names before them.
func scanGoingRun(row pgx.Row, lead ...any) (goingRun, error) {
	var run goingRun
	var chip, used []byte
	targets := append(lead, &run.ID, &run.Conversation, &run.Continues, &run.Words, &chip, &run.Level, &run.Mode,
		&run.State, &run.StopReason, &run.Issues, &used, &run.CreatedAt, &run.FinishedAt)
	if err := row.Scan(targets...); err != nil {
		return goingRun{}, err
	}
	if chip != nil {
		run.Context = &goingContext{}
		if err := json.Unmarshal(chip, run.Context); err != nil {
			return goingRun{}, fmt.Errorf("decode going run context: %w", err)
		}
	}
	if err := json.Unmarshal(used, &run.Used); err != nil {
		return goingRun{}, fmt.Errorf("decode what the going run spent: %w", err)
	}
	if run.Issues == nil {
		run.Issues = []string{}
	}
	return run, nil
}

// sweepGoingRuns deletes each conversation whose newest run is older than goingRunsKept, every
// run of it at once, and with them their steps, events, grants, landings and plans (on delete
// cascade). The reads above already leave such a conversation out, so the sweep only keeps the
// tables from growing; and a conversation is deleted whole, never a run of it alone, so no
// follow-up of a listed conversation names a run that is gone. A run is old by its created_at
// (going_runs_created), and its conversation holds no younger run by going_runs_conversation; a
// conversation whose newest run is old holds no run that is not.
func sweepGoingRuns(ctx context.Context, pool *store.Pool) error {
	_, err := pool.Exec(ctx, `
		delete from going_runs o
		where o.created_at < now() - make_interval(secs => $1)
		  and not exists (
			select 1 from going_runs n
			where n.conversation_id = coalesce(o.conversation_id, o.id) and n.created_at >= now() - make_interval(secs => $1)
		  )
	`, goingRunsKept.Seconds())
	return err
}

// goingReapBatch is how many runs one pass of the reaper ends at the most; the next pass ends the
// rest.
const goingReapBatch = 32

// reapGoingRuns ends each run that works under no live lease and is past its working budget: its
// lease ran out longer ago than what was left of its level's working time, so no agent took it up
// for all the time it had left. Each is stopped with its time budget's reason, its lease freed, and
// its end frame offers Continue, as a run its agent stopped at that bound offers it
// (stopGoingRunIn). It ends no run that waits for its person, which has no deadline, nor one that
// is queued.
func reapGoingRuns(ctx context.Context, pool *store.Pool) error {
	names := make([]string, 0, len(agent.Levels))
	budgets := make([]int64, 0, len(agent.Levels))
	for name, level := range agent.Levels {
		names, budgets = append(names, name), append(budgets, level.Time.Milliseconds())
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		select r.id::text, l.level
		from going_runs r
		join unnest($1::text[], $2::bigint[]) as l(level, budget_ms) on l.level = coalesce(r.level, 'standard')
		where r.state = 'working' and r.lease_ends_at < now()
		  and r.lease_ends_at + make_interval(secs => greatest(l.budget_ms - coalesce((r.budget_used->>'working_ms')::bigint, 0), 0) / 1000.0) < now()
		order by r.lease_ends_at
		limit $3
		for update of r skip locked
	`, names, budgets, goingReapBatch)
	if err != nil {
		return fmt.Errorf("read the runs past their budget: %w", err)
	}
	type overdue struct{ id, level string }
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (overdue, error) {
		var run overdue
		err := row.Scan(&run.id, &run.level)
		return run, err
	})
	if err != nil {
		return fmt.Errorf("read the runs past their budget: %w", err)
	}
	for _, run := range runs {
		reason := fmt.Sprintf("%s: this run's agent stopped, and no agent took it up within the %s a %s run may work; Continue starts a follow-up with a new budget",
			agent.ReasonTimeBudget, agent.Levels[run.level].Time, run.level)
		if err := stopGoingRunIn(ctx, tx, run.id, reason, []string{agentwire.OfferContinue}); err != nil {
			return fmt.Errorf("a run past its budget: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if len(runs) > 0 {
		slog.Info("dispatch: stopped runs past their working budget that no agent held", "runs", len(runs))
	}
	return nil
}
