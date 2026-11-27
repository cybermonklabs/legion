// Package prodevents is the record of a project's deploys and failed changes (dispatch://INFRA-978,
// R6 to R8): the table production_events (migration 20261123002035) and every read and write of
// it. A deploy row is one deploy run of a deploy repository, which the GitHub adapter writes under
// the repository's owner project, or one deploy a system posted. A failure row is one failure
// signal, which names the deploy it blames. Every write commits in a transaction of its own, or
// in its caller's (WriteIn, SetStateIn), that takes the delivery change log's lock nowhere: its
// change rows wait in the outbox (migration 20261123002037), and the write, or its caller, drains
// them after its commit (delivery.DrainAfterCommit).
package prodevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The two kinds of row.
const (
	KindDeploy  = "deploy"
	KindFailure = "failure"
)

// The writers of a row (Event.Source): the GitHub adapter, a system's post, a person's flag, and
// the three automatic failure signals.
const (
	SourceGitHub        = "github"
	SourceAPI           = "api"
	SourcePerson        = "person"
	SourceCheckResults  = "check_results"
	SourceReverts       = "reverts"
	SourceIncidentLabel = "incident_label"
)

// The five failure signals (Event.Signal, INFRA-978 R7).
const (
	SignalCheckFail     = "check_fail"
	SignalRevert        = "revert"
	SignalIncidentIssue = "incident_issue"
	SignalAPI           = "api"
	SignalPerson        = "person"
)

// The states of a failure (Event.State, R8): every signal counts when it arrives, and a reject
// takes it out of the numbers and keeps it.
const (
	StateCounted  = "counted"
	StateRejected = "rejected"
)

// DefaultEnvironment is the environment of a row that names none.
const DefaultEnvironment = "production"

// Event is one row of the record.
type Event struct {
	ID               string       `json:"id"`
	ProjectKey       string       `json:"project_key"`
	Kind             string       `json:"kind" enum:"deploy,failure"`
	Environment      string       `json:"environment"`
	Status           string       `json:"status"`
	Source           string       `json:"source" enum:"github,api,person,check_results,reverts,incident_label"`
	SourceRef        string       `json:"source_ref"`
	StartedAt        *time.Time   `json:"started_at"`
	FinishedAt       *time.Time   `json:"finished_at"`
	Repo             string       `json:"repo"`
	Commits          []string     `json:"commits"`
	PullRequests     []string     `json:"pull_requests"`
	URL              string       `json:"url"`
	Actor            model.Actor  `json:"actor"`
	CreatedAt        time.Time    `json:"created_at"`
	DeployID         *string      `json:"deploy_id"`
	Signal           *string      `json:"signal" enum:"check_fail,revert,incident_issue,api,person"`
	DetectedAt       *time.Time   `json:"detected_at"`
	State            string       `json:"state" enum:"counted,rejected"`
	StateReason      *string      `json:"state_reason"`
	StateBy          *model.Actor `json:"state_by"`
	StateAt          *time.Time   `json:"state_at"`
	RestoredAt       *time.Time   `json:"restored_at"`
	RestoredBy       *model.Actor `json:"restored_by"`
	RestoredByDeploy *string      `json:"restored_by_deploy"`
	// Note is what the writer says of the row, empty where it says nothing.
	Note string `json:"note"`
}

// Error is a refusal a caller can name: its code and its reason, which a route answers as
// {code, error}.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// The codes an Error carries: a field no row can hold, a blamed or restoring deploy no deploy row
// is, and a state change no row takes.
const (
	CodeInput      = "PRODUCTION_EVENT_INPUT"
	CodeNoDeploy   = "PRODUCTION_EVENT_NO_DEPLOY"
	CodeStateInput = "PRODUCTION_EVENT_STATE_INPUT"
)

func refuse(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// NoDeploy is the refusal of id, a deploy's id as the writer named it, a uuid in its canonical
// spelling, where it names no deploy row the write may name: no row, a row of another kind, or a
// deploy a DeployCheck refuses in these same words, so the refusal tells the writer nothing of a
// row outside its reach.
func NoDeploy(id string) error {
	return refuse(CodeNoDeploy, "no deploy row has id %s", id)
}

// DeployCheck checks the attribution fields of a deploy that a route's write names.
// The check runs in the write transaction while the deploy row is held for key share, so a drop
// waits for the write. A nil check is used by automatic writers.
type DeployCheck func(ctx context.Context, tx pgx.Tx, deploy Event) error

// ErrNotFound is a row the project does not hold.
var ErrNotFound = errors.New("no such production event")

// columns is every column of a row, in Event's order, as scan reads them.
const columns = `id::text, project_key, kind, environment, status, source, source_ref, started_at, finished_at,
	repo, commits, pull_requests, url, actor, created_at, deploy_id::text, signal, detected_at, state,
	state_reason, state_by, state_at, restored_at, restored_by, restored_by_deploy::text, note`

// scan reads one row of columns.
func scan(row pgx.Row) (Event, error) {
	var event Event
	var actor []byte
	var stateBy, restoredBy []byte
	err := row.Scan(&event.ID, &event.ProjectKey, &event.Kind, &event.Environment, &event.Status, &event.Source,
		&event.SourceRef, &event.StartedAt, &event.FinishedAt, &event.Repo, &event.Commits, &event.PullRequests,
		&event.URL, &actor, &event.CreatedAt, &event.DeployID, &event.Signal, &event.DetectedAt, &event.State,
		&event.StateReason, &stateBy, &event.StateAt, &event.RestoredAt, &restoredBy, &event.RestoredByDeploy, &event.Note)
	if err != nil {
		return Event{}, err
	}
	if err := json.Unmarshal(actor, &event.Actor); err != nil {
		return Event{}, fmt.Errorf("decode production event %s's actor: %w", event.ID, err)
	}
	if event.StateBy, err = decodeActor(stateBy); err != nil {
		return Event{}, fmt.Errorf("decode production event %s's state writer: %w", event.ID, err)
	}
	if event.RestoredBy, err = decodeActor(restoredBy); err != nil {
		return Event{}, fmt.Errorf("decode production event %s's restorer: %w", event.ID, err)
	}
	if event.Commits == nil {
		event.Commits = []string{}
	}
	if event.PullRequests == nil {
		event.PullRequests = []string{}
	}
	return event, nil
}

func decodeActor(raw []byte) (*model.Actor, error) {
	if raw == nil {
		return nil, nil
	}
	var actor model.Actor
	if err := json.Unmarshal(raw, &actor); err != nil {
		return nil, err
	}
	return &actor, nil
}

// prepared refuses an event no row can hold (validate), and answers its actor as the record keeps
// it, who wrote the row and never how the request authenticated (model.Actor.Recorded), and its
// environment, DefaultEnvironment where it names none.
func prepared(event Event) ([]byte, string, error) {
	if err := validate(event); err != nil {
		return nil, "", err
	}
	actor, err := json.Marshal(event.Actor.Recorded())
	if err != nil {
		return nil, "", fmt.Errorf("encode the actor of production event %s: %w", event.SourceRef, err)
	}
	environment := event.Environment
	if environment == "" {
		environment = DefaultEnvironment
	}
	return actor, environment, nil
}

// Write stores event under its project, or answers the row its project already holds under its
// source_ref: a signal sent twice writes one row (R6), and the second write answers the first
// row and created false. A failure's blamed deploy must be a deploy row; a deploy names none. The
// row's id, its creation time and its state are the server's, whatever event carries.
func Write(ctx context.Context, pool *store.Pool, event Event) (Event, bool, error) {
	return WriteChecked(ctx, pool, event, nil)
}

// WriteChecked is Write with check asked of each deploy event names (DeployCheck), nil for no
// question: a route's post holds the failure's blamed deploy to its project with it.
func WriteChecked(ctx context.Context, pool *store.Pool, event Event, check DeployCheck) (Event, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Event{}, false, fmt.Errorf("begin the write of production event %s: %w", event.SourceRef, err)
	}
	defer tx.Rollback(ctx)
	stored, created, err := writeIn(ctx, tx, event, check)
	if err != nil {
		return Event{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Event{}, false, fmt.Errorf("commit production event %s of %s: %w", event.SourceRef, event.ProjectKey, err)
	}
	if created {
		delivery.DrainAfterCommit(ctx, pool, "drain the change rows of a production event write")
	}
	return stored, created, nil
}

// WriteIn is Write's write in tx, the caller's transaction, for a writer that keeps a row of its
// own beside the event's and commits the two together. It takes the delivery change log's lock
// nowhere either: the caller commits tx and then, where created, drains the change rows the write
// left in the outbox (delivery.DrainAfterCommit), as Write does.
func WriteIn(ctx context.Context, tx pgx.Tx, event Event) (Event, bool, error) {
	return writeIn(ctx, tx, event, nil)
}

// writeIn is WriteIn's write with check asked of each deploy event names (WriteChecked).
func writeIn(ctx context.Context, tx pgx.Tx, event Event, check DeployCheck) (Event, bool, error) {
	actor, environment, err := prepared(event)
	if err != nil {
		return Event{}, false, err
	}
	commits, pulls := nonNil(event.Commits), nonNil(event.PullRequests)
	named := make([]string, 0, 2)
	if event.Kind == KindFailure {
		named = append(named, *event.DeployID)
	}
	if event.RestoredByDeploy != nil {
		named = append(named, *event.RestoredByDeploy)
	}
	for _, id := range named {
		deploy, err := requireDeploy(ctx, tx, id, check != nil)
		if err == nil && check != nil {
			err = check(ctx, tx, deploy)
		}
		if err != nil {
			return Event{}, false, err
		}
	}
	var restoredBy []byte
	if event.RestoredBy != nil {
		if restoredBy, err = json.Marshal(event.RestoredBy.Recorded()); err != nil {
			return Event{}, false, fmt.Errorf("encode the restorer of production event %s: %w", event.SourceRef, err)
		}
	}
	stored, err := scan(tx.QueryRow(ctx, `
		insert into production_events (id, project_key, kind, environment, status, source, source_ref,
			started_at, finished_at, repo, commits, pull_requests, url, actor, deploy_id, signal, detected_at,
			restored_at, restored_by, restored_by_deploy, note)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::uuid, $16, $17, $18, $19, $20::uuid, $21)
		on conflict (project_key, source_ref) do nothing
		returning `+columns,
		uuid.Must(uuid.NewV7()).String(), event.ProjectKey, event.Kind, environment, event.Status, event.Source,
		event.SourceRef, event.StartedAt, event.FinishedAt, event.Repo, commits, pulls, event.URL, actor,
		event.DeployID, event.Signal, event.DetectedAt, event.RestoredAt, restoredBy, event.RestoredByDeploy, event.Note))
	created := true
	if errors.Is(err, pgx.ErrNoRows) {
		created = false
		stored, err = bySourceRef(ctx, tx, event.ProjectKey, event.SourceRef)
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("write production event %s of %s: %w", event.SourceRef, event.ProjectKey, err)
	}
	return stored, created, nil
}

// validate refuses an event no row can hold, naming the field.
func validate(event Event) error {
	switch {
	case strings.TrimSpace(event.ProjectKey) == "":
		return refuse(CodeInput, "project_key is required")
	case strings.TrimSpace(event.SourceRef) == "":
		return refuse(CodeInput, "source_ref is required: it makes a repeated event write no second row")
	case event.Kind != KindDeploy && event.Kind != KindFailure:
		return refuse(CodeInput, "kind must be deploy or failure, not %q", event.Kind)
	}
	switch event.Source {
	case SourceGitHub, SourceAPI, SourcePerson, SourceCheckResults, SourceReverts, SourceIncidentLabel:
	default:
		return refuse(CodeInput, "source %q is not one of the record's writers", event.Source)
	}
	if event.Kind == KindDeploy {
		switch {
		case event.FinishedAt == nil:
			return refuse(CodeInput, "a deploy needs finished_at, the time it reached production")
		case event.DeployID != nil || event.Signal != nil || event.DetectedAt != nil:
			return refuse(CodeInput, "a deploy names no deploy, no signal and no detected time")
		case event.RestoredAt != nil || event.RestoredByDeploy != nil:
			return refuse(CodeInput, "a deploy is not restored; a failure is")
		}
		return nil
	}
	switch {
	case event.DeployID == nil || *event.DeployID == "":
		return refuse(CodeInput, "a failure names the deploy it blames")
	case event.Signal == nil:
		return refuse(CodeInput, "a failure names its signal")
	case event.DetectedAt == nil:
		return refuse(CodeInput, "a failure needs detected_at")
	case event.FinishedAt != nil:
		return refuse(CodeInput, "a failure has no finished_at; it counts on its deploy's day")
	case (event.RestoredAt == nil) != (event.RestoredBy == nil):
		return refuse(CodeInput, "a restore names its time and who restored it together")
	}
	switch *event.Signal {
	case SignalCheckFail, SignalRevert, SignalIncidentIssue, SignalAPI, SignalPerson:
		return nil
	}
	return refuse(CodeInput, "signal %q is not one of the five", *event.Signal)
}

// requireDeploy holds the deploy row for key share, as the foreign key does, before a writer
// references it. A concurrent drop must wait or leave no deploy to reference. Automatic writers
// only need to know the row is a deploy; routes and restores also read its attribution and finish.
func requireDeploy(ctx context.Context, tx pgx.Tx, id string, details bool) (Event, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return Event{}, refuse(CodeNoDeploy, "deploy %q is not a production event id", id)
	}
	var deploy Event
	if details {
		err = tx.QueryRow(ctx, `select id::text, project_key, kind, source, status, repo, pull_requests, finished_at
			from production_events where id = $1 and kind = 'deploy' for key share`, parsed.String()).
			Scan(&deploy.ID, &deploy.ProjectKey, &deploy.Kind, &deploy.Source, &deploy.Status,
				&deploy.Repo, &deploy.PullRequests, &deploy.FinishedAt)
	} else {
		err = tx.QueryRow(ctx, `select kind from production_events where id = $1 and kind = 'deploy' for key share`,
			parsed.String()).Scan(&deploy.Kind)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Event{}, NoDeploy(parsed.String())
	case err != nil:
		return Event{}, fmt.Errorf("read deploy %s: %w", id, err)
	}
	return deploy, nil
}

// StateChange is one write of the state route (R8, R10): a reject, a re-count, or a restore.
type StateChange struct {
	// State is counted, rejected or restored.
	State string
	// Reason is why; a reject and a re-count need one.
	Reason string
	// RestoredByDeploy names the deploy that restored the failure, where one did.
	RestoredByDeploy *string
	// Check is the route's question of the restoring deploy (DeployCheck), nil for none.
	Check DeployCheck
	Actor model.Actor
	At    time.Time
}

// StateRestored is the restore a state change may be, beside the two states a row holds.
const StateRestored = "restored"

// SetState writes change to the failure id of project and answers the row. A deploy has no
// state; a reject and a re-count keep the writer, the time and the reason (R8); a restore sets the
// restore time and its writer, and the restoring deploy where change names one, a deploy that
// finished at or after the failure's deploy finished.
func SetState(ctx context.Context, pool *store.Pool, project, id string, change StateChange) (Event, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("begin the state write of production event %s: %w", id, err)
	}
	defer tx.Rollback(ctx)
	updated, err := SetStateIn(ctx, tx, project, id, change)
	if err != nil {
		return Event{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Event{}, fmt.Errorf("commit production event %s's state: %w", id, err)
	}
	delivery.DrainAfterCommit(ctx, pool, "drain the change rows of a production event state write")
	return updated, nil
}

// SetStateIn is SetState's write in tx, the caller's transaction, for a writer that decides the
// change under a lock of its own and commits it with that lock held. It takes the delivery change
// log's lock nowhere either: the caller commits tx and then drains the change rows the write left
// in the outbox (delivery.DrainAfterCommit), as SetState does.
func SetStateIn(ctx context.Context, tx pgx.Tx, project, id string, change StateChange) (Event, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Event{}, ErrNotFound
	}
	by, err := json.Marshal(change.Actor.Recorded())
	if err != nil {
		return Event{}, fmt.Errorf("encode the writer of production event %s's state: %w", id, err)
	}
	// A restore's restoring deploy is held before the failure, in the order a drop holds a deploy
	// and then writes the failures that blame it (DropGitHubDeploy), so neither waits on the
	// other's second lock. Its refusal waits for the failure's own: a row the project does not
	// hold, a deploy's row.
	var restoring Event
	var noRestorer error
	if change.State == StateRestored && change.RestoredByDeploy != nil {
		var refused *Error
		if restoring, noRestorer = requireDeploy(ctx, tx, *change.RestoredByDeploy, true); noRestorer != nil && !errors.As(noRestorer, &refused) {
			return Event{}, noRestorer
		}
	}
	current, err := scan(tx.QueryRow(ctx, `select `+columns+` from production_events
		where project_key = $1 and id = $2 for update`, project, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("read production event %s: %w", id, err)
	}
	if current.Kind != KindFailure {
		return Event{}, refuse(CodeStateInput, "row %s is a deploy, which has no state: only a failure is rejected or restored", id)
	}
	var updated Event
	switch change.State {
	case StateRejected, StateCounted:
		if strings.TrimSpace(change.Reason) == "" {
			return Event{}, refuse(CodeStateInput, "a %s needs a reason, which the row keeps", change.State)
		}
		if change.RestoredByDeploy != nil {
			return Event{}, refuse(CodeStateInput, "restored_by_deploy goes with state restored alone")
		}
		updated, err = scan(tx.QueryRow(ctx, `
			update production_events set state = $3, state_reason = $4, state_by = $5, state_at = $6
			where project_key = $1 and id = $2
			returning `+columns, project, id, change.State, change.Reason, by, change.At))
	case StateRestored:
		if change.RestoredByDeploy != nil {
			if err := restorable(ctx, tx, current, restoring, noRestorer, change.Check); err != nil {
				return Event{}, err
			}
		}
		updated, err = scan(tx.QueryRow(ctx, `
			update production_events set restored_at = $3, restored_by = $4, restored_by_deploy = $5::uuid
			where project_key = $1 and id = $2
			returning `+columns, project, id, change.At, by, change.RestoredByDeploy))
	default:
		return Event{}, refuse(CodeStateInput, "state must be counted, rejected or restored, not %q", change.State)
	}
	if err != nil {
		return Event{}, fmt.Errorf("write production event %s's state: %w", id, err)
	}
	return updated, nil
}

// restorable refuses restoring, the deploy a restore of failure names (held by requireDeploy, which
// refused it noRestorer where it names no deploy row), where check refuses it, and where it finished
// before the deploy failure blames: deployment-rework-rate counts a restoring deploy on its own
// finish day (evals' failure_days.go), so a deploy that finished before the failed one would count
// as rework in a window the failure is not in. The failed deploy is read and not locked: the
// failure's foreign key keeps its row, and a lock of it after the failure's would take the two
// rows in the order opposite to a drop's (DropGitHubDeploy).
func restorable(ctx context.Context, tx pgx.Tx, failure, restoring Event, noRestorer error, check DeployCheck) error {
	if noRestorer != nil {
		return noRestorer
	}
	if check != nil {
		if err := check(ctx, tx, restoring); err != nil {
			return err
		}
	}
	var failedAt time.Time
	if err := tx.QueryRow(ctx, `select finished_at from production_events where id = $1`, *failure.DeployID).Scan(&failedAt); err != nil {
		return fmt.Errorf("read the finish of production event %s's deploy: %w", failure.ID, err)
	}
	if restoring.FinishedAt.Before(failedAt) {
		return refuse(CodeStateInput,
			"restored_by_deploy %s finished before the deploy the failure blames finished: a deploy restores a failure of a deploy that finished at or before its own finish",
			restoring.ID)
	}
	return nil
}

// Get reads the row id of project.
func Get(ctx context.Context, q delivery.Querier, project, id string) (Event, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Event{}, ErrNotFound
	}
	event, err := scan(q.QueryRow(ctx, `select `+columns+` from production_events
		where project_key = $1 and id = $2`, project, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("read production event %s: %w", id, err)
	}
	return event, nil
}

// ListQuery is one page of a project's rows: a window over a deploy's finished_at and a failure's
// detected_at, [From, To), optionally one kind, one state and the failures that blame one deploy
// (DeployID, a deploy row's uuid), newest first.
type ListQuery struct {
	Project  string
	From     time.Time
	To       time.Time
	Kind     string
	State    string
	DeployID string
	Limit    int
	Offset   int
}

// listMatches is the rows a ListQuery matches, its parameters $1 to $6 being the query's project,
// window, kind, state and deploy, in that order. A DeployID keeps the project's failures that
// blame that deploy, so it matches no row of another project.
const listMatches = `
	from production_events
	where project_key = $1
	  and (kind = 'deploy' and finished_at >= $2 and finished_at < $3
	    or kind = 'failure' and detected_at >= $2 and detected_at < $3)
	  and ($4 = '' or kind = $4)
	  and ($5 = '' or state = $5)
	  and ($6 = '' or kind = 'failure' and deploy_id = nullif($6, '')::uuid)`

// List reads one page of a project's rows and the count of every row the query matches.
func List(ctx context.Context, q delivery.Querier, query ListQuery) ([]Event, int, error) {
	rows, err := q.Query(ctx, `
		with matched as (select *, coalesce(finished_at, detected_at) as at `+listMatches+`)
		select `+columns+`, count(*) over () from matched
		order by at desc, id desc
		limit $7 offset $8`,
		query.Project, query.From, query.To, query.Kind, query.State, query.DeployID, query.Limit, query.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list %s's production events: %w", query.Project, err)
	}
	defer rows.Close()
	events := []Event{}
	total := 0
	for rows.Next() {
		event, err := scan(totalRow{rows, &total})
		if err != nil {
			return nil, 0, fmt.Errorf("read a production event of %s: %w", query.Project, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list %s's production events: %w", query.Project, err)
	}
	if len(events) == 0 && query.Offset > 0 {
		if err := q.QueryRow(ctx, `select count(*) `+listMatches,
			query.Project, query.From, query.To, query.Kind, query.State, query.DeployID).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count %s's production events: %w", query.Project, err)
		}
	}
	return events, total, nil
}

// totalRow scans one row of List's statement: the row's columns, then the window's count into
// total.
type totalRow struct {
	rows  pgx.Rows
	total *int
}

func (r totalRow) Scan(dest ...any) error {
	return r.rows.Scan(append(dest, r.total)...)
}

// DeployForCommit is the deploy row, of any project, whose commits hold sha: the earliest to
// reach production, which is the deploy that first shipped the commit (a check made at a commit
// blames the deploy that shipped it, R7). ok is false where no deploy holds it yet. A dropped
// GitHub deploy (StatusDropped) holds none. The read is a probe of production_events_commits.
func DeployForCommit(ctx context.Context, q delivery.Querier, sha string) (Event, bool, error) {
	event, err := scan(q.QueryRow(ctx, `select `+columns+` from production_events
		where kind = 'deploy' and commits @> array[$1::text] and not `+droppedDeploy+`
		order by finished_at, id limit 1`, strings.ToLower(sha)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("read the deploy of commit %s: %w", sha, err)
	}
	return event, true, nil
}

// DeployForRun is the deploy row the GitHub adapter wrote for run runID of repo, of any project,
// and false where it wrote none or dropped it (StatusDropped).
func DeployForRun(ctx context.Context, q delivery.Querier, repo string, runID int64) (Event, bool, error) {
	event, err := scan(q.QueryRow(ctx, `select `+columns+` from production_events
		where kind = 'deploy' and source = 'github' and lower(repo) = lower($1) and source_ref = $2
		  and not `+droppedDeploy+`
		order by id limit 1`, repo, GitHubSourceRef(runID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("read the deploy of %s run %d: %w", repo, runID, err)
	}
	return event, true, nil
}

// DeploysFrom reads the deploy rows of projects that finished at or after from, oldest finish
// first (ties by id): one range of production_events_window for each project. The failure
// measures read the deploys a project's rows can count from the project's first day on with it
// (evals' failure_days.go).
func DeploysFrom(ctx context.Context, q delivery.Querier, projects []string, from time.Time) ([]Event, error) {
	rows, err := q.Query(ctx, `select `+columns+` from production_events
		where project_key = any($1) and kind = 'deploy' and finished_at >= $2
		order by finished_at, id`, projects, from)
	if err != nil {
		return nil, fmt.Errorf("read the deploys of %d projects from %s on: %w", len(projects), from.Format(time.RFC3339), err)
	}
	return scanAll(rows, "a deploy")
}

// FailuresOf reads the failure rows, of any project and in any state, that blame one of deploys
// (deploy row ids), oldest detection first (ties by id): one probe of
// production_events_failures_of for each deploy.
func FailuresOf(ctx context.Context, q delivery.Querier, deploys []string) ([]Event, error) {
	if len(deploys) == 0 {
		return []Event{}, nil
	}
	rows, err := q.Query(ctx, `select `+columns+` from production_events
		where kind = 'failure' and deploy_id = any($1::uuid[])
		order by detected_at, id`, deploys)
	if err != nil {
		return nil, fmt.Errorf("read the failures of %d deploys: %w", len(deploys), err)
	}
	return scanAll(rows, "a failure")
}

// scanAll reads every row of rows (columns), what naming the rows in an error.
func scanAll(rows pgx.Rows, what string) ([]Event, error) {
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		event, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("read %s of the record: %w", what, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s of the record: %w", what, err)
	}
	return events, nil
}

// The source_ref prefix of each automatic source's rows: the GitHub adapter's deploy of a run
// (GitHubSourceRef), a person's flag (PersonSourceRef), and the production signals' failures
// (signals' CheckSourceRef, RevertSourceRef and IncidentSourceRef). The record holds one row per
// project and source_ref, so a system's post under such a key would leave that source's write of
// its own row writing nothing; the post route refuses one (AutomaticSourceRefPrefix).
const (
	GitHubSourceRefPrefix   = "github:"
	PersonSourceRefPrefix   = "person:"
	CheckSourceRefPrefix    = "check:"
	RevertSourceRefPrefix   = "revert:"
	IncidentSourceRefPrefix = "incident:"
)

// automaticSourceRefPrefixes is every automatic source's source_ref prefix.
var automaticSourceRefPrefixes = [...]string{
	GitHubSourceRefPrefix, PersonSourceRefPrefix, CheckSourceRefPrefix, RevertSourceRefPrefix, IncidentSourceRefPrefix,
}

// AutomaticSourceRefPrefix is the automatic source's prefix ref starts with, and false where it
// starts with none of them.
func AutomaticSourceRefPrefix(ref string) (string, bool) {
	for _, prefix := range automaticSourceRefPrefixes {
		if strings.HasPrefix(ref, prefix) {
			return prefix, true
		}
	}
	return "", false
}

// GitHubSourceRef is the source_ref of the GitHub adapter's deploy row of run runID.
func GitHubSourceRef(runID int64) string {
	return GitHubSourceRefPrefix + strconv.FormatInt(runID, 10)
}

// PersonSourceRef is the source_ref of the flag of deploy (a deploy row's id) by the person
// actorID: one row per person and deploy, so a second flag writes nothing.
func PersonSourceRef(deployID, actorID string) string {
	return PersonSourceRefPrefix + deployID + ":" + actorID
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
