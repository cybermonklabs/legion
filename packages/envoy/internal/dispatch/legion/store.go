package legion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Settings is one project's Legion setting: the daemon Dispatch reads that project's state from.
type Settings struct {
	Project   string      `json:"project"`
	StateURL  string      `json:"state_url"`
	CreatedBy model.Actor `json:"created_by"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// settingsColumns is the legion_settings select list scanSettings reads, in scan order; every
// query that returns a setting selects exactly this.
const settingsColumns = `project, state_url, created_by, updated_at`

// ErrNoSettings is "this project names no Legion daemon": the sampler reads nothing for it, the
// board has nothing to show, and an issue's own Legion read answers `configured` false rather
// than refusing (api/legion_issue.go).
var ErrNoSettings = errors.New("this project has no Legion daemon configured")

// StateChannel is the Postgres notification channel one read of a daemon's state announces itself
// on, with the project as its payload, in the shape of delivery.SettingsChannel: nothing commits
// to announce a remote daemon's state, so the sampler's own write is what an open Legion board
// hears (api/delivery_live_hub.go). A read announces itself whether or not it changed an
// observation, since the board's header carries the read itself — when it was made, and why it
// failed.
const StateChannel = "dispatch_legion_state"

// NotifyRead announces a read of project's daemon on StateChannel.
func NotifyRead(ctx context.Context, pool *store.Pool, project string) error {
	if _, err := pool.Exec(ctx, `select pg_notify($1, $2)`, StateChannel, project); err != nil {
		return fmt.Errorf("announce the Legion state read of %s: %w", project, err)
	}
	return nil
}

// scanSettings decodes one settingsColumns row.
func scanSettings(row pgx.Row) (Settings, error) {
	var settings Settings
	var createdBy []byte
	if err := row.Scan(&settings.Project, &settings.StateURL, &createdBy, &settings.UpdatedAt); err != nil {
		return Settings{}, err
	}
	if err := json.Unmarshal(createdBy, &settings.CreatedBy); err != nil {
		return Settings{}, fmt.Errorf("decode the Legion setting's author: %w", err)
	}
	return settings, nil
}

// ListSettings is every project's Legion setting, by project key; a deployment with none reads an
// empty list.
func ListSettings(ctx context.Context, pool *store.Pool) ([]Settings, error) {
	rows, err := pool.Query(ctx, `select `+settingsColumns+` from legion_settings order by project`)
	if err != nil {
		return nil, fmt.Errorf("read the Legion settings: %w", err)
	}
	defer rows.Close()
	listed := []Settings{}
	for rows.Next() {
		settings, err := scanSettings(rows)
		if err != nil {
			return nil, err
		}
		listed = append(listed, settings)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the Legion settings: %w", err)
	}
	return listed, nil
}

// GetSettings is one project's setting, or ErrNoSettings when it has none, as the reader whose
// visibility argument viewer binds (refs.VisibilityArg) reads it: a project that reader does not
// see has none (access.ProjectClause, in the statement itself), whatever the caller's own read of
// the project answered before.
func GetSettings(ctx context.Context, pool *store.Pool, project string, viewer any) (Settings, error) {
	settings, err := scanSettings(pool.QueryRow(ctx, `select `+settingsColumns+` from legion_settings where project = $1 and `+
		access.ProjectClause("$1", "$2"), project, viewer))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Settings{}, ErrNoSettings
		}
		return Settings{}, err
	}
	return settings, nil
}

// projectIssues answers which of keys Dispatch holds as issues of project: the record the sampler
// keeps a read to (State.keepProject), since a daemon's answer names no issue's project. It reads
// as the server, as the sampler does every row it writes, and reads nothing for no key.
func projectIssues(ctx context.Context, pool *store.Pool, project string, keys []string) (map[string]bool, error) {
	own := make(map[string]bool, len(keys))
	if len(keys) == 0 {
		return own, nil
	}
	rows, err := pool.Query(ctx, `select key from issues where project_key = $1 and key = any($2::text[])`, project, keys)
	if err != nil {
		return nil, fmt.Errorf("read which issues the daemon reports are %s's: %w", project, err)
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read which issues the daemon reports are %s's: %w", project, err)
	}
	for _, key := range held {
		own[key] = true
	}
	return own, nil
}

// PutSettings sets the daemon a project is read from, and answers the row as it stands. created_by
// is the actor that first set the project's setting: a later write changes the URL and the time
// and keeps it.
func PutSettings(ctx context.Context, pool *store.Pool, project, stateURL string, actor model.Actor) (Settings, error) {
	createdBy, err := json.Marshal(actor)
	if err != nil {
		return Settings{}, fmt.Errorf("encode the Legion setting's author: %w", err)
	}
	settings, err := scanSettings(pool.QueryRow(ctx, `
		insert into legion_settings (project, state_url, created_by, updated_at)
		values ($1, $2, $3, now())
		on conflict (project) do update set state_url = excluded.state_url, updated_at = now()
		returning `+settingsColumns, project, stateURL, createdBy))
	if err != nil {
		return Settings{}, fmt.Errorf("write the Legion setting of %s: %w", project, err)
	}
	return settings, nil
}

// DeleteSettings removes a project's setting and reports whether it had one. The project's reads
// and observations stay: they are what was observed, and nothing re-reads them.
func DeleteSettings(ctx context.Context, pool *store.Pool, project string) (bool, error) {
	tag, err := pool.Exec(ctx, `delete from legion_settings where project = $1`, project)
	if err != nil {
		return false, fmt.Errorf("delete the Legion setting of %s: %w", project, err)
	}
	return tag.RowsAffected() > 0, nil
}

// RecordRead records a read of project's daemon that answered its state, in one transaction: every
// observation that moved (recordObservations), then the legion_reads row that says the read
// succeeded and how many bytes it sent. It answers how many observations it wrote.
//
// The board's header reads the newest legion_reads row as the last good read, and its columns
// read the newest observations, so the two commit together or not at all: a read whose
// observations Postgres refuses leaves no row saying it succeeded over data it did not store. The
// caller records such a read as failed (RecordFailedRead), and the last good read stands.
func RecordRead(ctx context.Context, pool *store.Pool, project string, at time.Time, bytes int, observations []Observation) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("open the transaction of the Legion state read of %s: %w", project, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	written, err := recordObservations(ctx, tx, project, at, observations)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, insertRead, project, at, true, nil, bytes); err != nil {
		return 0, fmt.Errorf("store the Legion state read of %s: %w", project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit the Legion state read of %s: %w", project, err)
	}
	return written, nil
}

// RecordFailedRead appends the legion_reads row of a read of project's daemon that failed: how
// many bytes the daemon sent, and why the read failed, which the board's header names. It writes
// no observation, so the last good read's observations stand.
func RecordFailedRead(ctx context.Context, pool *store.Pool, project string, at time.Time, bytes int, failure error) error {
	if _, err := pool.Exec(ctx, insertRead, project, at, false, failure.Error(), bytes); err != nil {
		return fmt.Errorf("record the failed Legion state read of %s: %w", project, err)
	}
	return nil
}

// insertRead appends one legion_reads row: project, read_at, ok, error, bytes.
const insertRead = `
	insert into legion_reads (project, read_at, ok, error, bytes)
	values ($1, $2, $3, $4, $5)
`

// ReadRetention is how long legion_reads keeps a read of a daemon. The Legion board reads the
// newest row of a project and nothing reads an older one, so the rest are a week of history for
// whoever asks why a daemon stopped answering — the horizon delivery_changes keeps for the same
// reason, a day, measured against a table that gains a row a minute per project rather than one
// per change.
const ReadRetention = 7 * 24 * time.Hour

// PruneReads deletes every read older than ReadRetention that is not its project's newest, and
// answers how many it deleted: the table is append-only, one row per project per minute, and
// delivery.PruneChanges keeps delivery_changes the same way, from the loop that writes it.
//
// Each project keeps its newest read whatever its age, because that row is the freshness the
// board's header shows: a daemon nobody has read for a week reads as a stale read, never as a
// daemon that was never read. The cutoff is the database's own now(), as each row's read_at is
// the sampler's, so one clock decides whichever replica prunes.
func PruneReads(ctx context.Context, pool *store.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `
		delete from legion_reads old
		where old.read_at < now() - $1::interval
		  and exists (
		    select 1 from legion_reads newer
		    where newer.project = old.project and (newer.read_at, newer.id) > (old.read_at, old.id)
		  )
	`, ReadRetention)
	if err != nil {
		return 0, fmt.Errorf("prune the Legion state reads: %w", err)
	}
	return tag.RowsAffected(), nil
}

// recordObservations appends, inside the transaction of the read that found them, a row for each
// observation whose value differs from the newest row that project, issue and field already hold,
// and a `null` row for each issue and field whose newest row holds a value and that the read does
// not carry: an issue gone from the daemon's state, a role's claim gone from an issue, a tree the
// daemon stopped naming. It answers how many rows it wrote. An unchanged value writes nothing, and
// a first observation whose value is null writes nothing either: nothing moved from nothing.
//
// The `null` row is what makes the newest row of every field the newest good read's own: the
// board's cards (ReadCurrent) read a field the read no longer carries as absent, and the record
// keeps when it left, which is where the metrics' observed timeline ends the issue's phase.
//
// The newest rows it compares against are legion_current's: the trigger of migration
// 20261117022936 keeps that table in the same transaction as every insert here, so the comparison
// reads one row per field rather than the project's whole history. It takes the rows that hold a
// value; a field whose newest row is `null` compares as `null` all the same, as a field with no
// row does.
//
// One statement, so a pass of some hundreds of fields is one round trip, and Postgres's own jsonb
// comparison decides what changed: a Go encoding that ordered an object's keys differently from
// jsonb would otherwise write a row every minute for a value that never moved. A row the lease
// already let another replica write is therefore not written twice even if the leases overlapped.
func recordObservations(ctx context.Context, tx pgx.Tx, project string, at time.Time, observations []Observation) (int64, error) {
	issueKeys := make([]string, len(observations))
	fields := make([]string, len(observations))
	values := make([]string, len(observations))
	for index, observation := range observations {
		issueKeys[index], fields[index], values[index] = observation.IssueKey, observation.Field, string(observation.Value)
	}
	tag, err := tx.Exec(ctx, `
		with found as (
			select observed.issue_key, observed.field, observed.value::jsonb as value
			from unnest($2::text[], $3::text[], $4::text[]) as observed(issue_key, field, value)
		), newest as (
			select issue_key, field, value
			from legion_current
			where project = $1 and value <> 'null'::jsonb
		)
		insert into legion_observations (project, issue_key, field, value, observed_at)
		select $1, coalesce(found.issue_key, newest.issue_key), coalesce(found.field, newest.field),
			coalesce(found.value, 'null'::jsonb), $5::timestamptz
		from found
		full join newest on newest.issue_key = found.issue_key and newest.field = found.field
		where coalesce(newest.value, 'null'::jsonb) is distinct from coalesce(found.value, 'null'::jsonb)
	`, project, issueKeys, fields, values, at)
	if err != nil {
		return 0, fmt.Errorf("store the Legion observations of %s: %w", project, err)
	}
	return tag.RowsAffected(), nil
}

// Newest is one field's newest value, never `null`, and when the sampler recorded it: when the
// field took that value, since a row is written only where a value moved.
type Newest struct {
	Observation
	ObservedAt time.Time
}

// Queryer is what ReadCurrent needs of its connection: the pool, or a transaction a reader holds
// open on it so its other reads of what the sampler writes answer the same moment — the Legion
// board's header (api/legion_board.go's readSampled) and the metrics' observations (theirs).
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Current is what project's legion_current holds as of the newest good read. A caller that reads
// it beside another sampler table does so in one repeatable-read transaction (Queryer), which is
// what makes one answer one snapshot; ReadCurrent is the one statement of which issues the daemon
// reports.
type Current struct {
	// Newest is every field that holds a value: every field of every issue the newest good read
	// carried, and the project's own under AdmissionKey. The Legion board's cards are these.
	Newest []Newest
	// Reported is every issue the daemon reports, against the `tree` the newest good read names
	// for it, "" where it names none (ReadCurrent says which issues those are).
	Reported map[string]string
}

// ReadCurrent reads project's legion_current in one statement, one row per field and none of the
// history: every row that holds a value, and every `phase` row. A field the newest good read no
// longer carries, and every field of an issue gone from the daemon's state, has a `null` newest
// row (recordObservations), so it is in no Newest. It reads as the reader whose visibility
// argument viewer binds (refs.VisibilityArg): a project that reader does not see holds no row here
// (access.ProjectClause, in the statement itself).
//
// Reported is the daemon's half of what makes an issue a Legion issue (TreeNode), which the Legion
// board and the metrics both read here, so the two place an issue in one tree. The daemon reports
// an issue that is in its state, or that Dispatch observed a phase of (dispatch://INFRA-1077's
// spec, "The top issue of a tree"): an issue with a field whose newest row holds a value, which is
// the newest good read carrying it, or with a `phase` row at all. The sampler writes no first
// `null`, so a field that has a row was once given a value, and an issue that left the daemon's
// state keeps its `phase` row, written `null` when it left: it stays a Legion issue, so a child of
// it climbs through it to the tree above, while it carries no card and no tree, since its `tree`
// was written `null` with the rest. The rows under AdmissionKey belong to no issue — the admission
// block and the daemon's settings — so they report nothing.
func ReadCurrent(ctx context.Context, db Queryer, project string, viewer any) (Current, error) {
	rows, err := db.Query(ctx, `
		select issue_key, field, nullif(value, 'null'::jsonb), observed_at
		from legion_current
		where project = $1 and (value <> 'null'::jsonb or field = $2) and `+access.ProjectClause("$1", "$3")+`
	`, project, FieldPhase, viewer)
	if err != nil {
		return Current{}, fmt.Errorf("read the current Legion state of %s: %w", project, err)
	}
	defer rows.Close()
	current := Current{Reported: map[string]string{}}
	for rows.Next() {
		var one Newest
		var value []byte
		if err := rows.Scan(&one.IssueKey, &one.Field, &value, &one.ObservedAt); err != nil {
			return Current{}, fmt.Errorf("scan a current Legion observation of %s: %w", project, err)
		}
		if one.IssueKey != AdmissionKey {
			tree := current.Reported[one.IssueKey]
			if one.Field == FieldTree {
				tree = JSONString(value)
			}
			current.Reported[one.IssueKey] = tree
		}
		if value == nil {
			// A `phase` written `null`, which reports its issue and holds no value.
			continue
		}
		one.Value = value
		current.Newest = append(current.Newest, one)
	}
	if err := rows.Err(); err != nil {
		return Current{}, fmt.Errorf("read the current Legion state of %s: %w", project, err)
	}
	return current, nil
}
