package legion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

// ErrNoSettings is "this project names no Legion daemon": the intake reads nothing for it and
// drops its state events, the board has nothing to show, and an issue's own Legion read answers
// `configured` false rather than refusing (api/legion_issue.go).
var ErrNoSettings = errors.New("this project has no Legion daemon configured")

// StateChannel is the Postgres notification channel each record of a daemon's state announces
// itself on, with the project as its payload, in the shape of delivery.SettingsChannel: a batch of
// state events the intake applied, and a whole read (Intake.readWhole). Nothing in Dispatch's own
// event log announces a remote daemon's state, so the intake's own write is what an open Legion
// board hears (api/delivery_live_hub.go). A whole read announces itself whether or not it changed
// an observation, since the board's header carries the read itself — when it was made, and why it
// failed.
const StateChannel = "dispatch_legion_state"

// SettingsChannel is the Postgres notification channel a write of a Legion setting announces
// itself on at commit, with the project as its payload (PutSettings, DeleteSettings): the channel
// whose wake (pglisten.Hub) the server hands the intake, which reads the setting's daemon whole
// where the write left the project no starting point (Intake.SettingsChanged).
const SettingsChannel = "dispatch_legion_settings"

// Execer is what NotifyRead needs of its connection: the pool, or the transaction of the record it
// announces.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// NotifyRead announces a record of project's daemon's state on StateChannel. Inside the record's
// own transaction (applyBatch, recordRead) Postgres sends the announcement at the commit, so an
// open board hears each record that committed and no record that did not.
func NotifyRead(ctx context.Context, db Execer, project string) error {
	if _, err := db.Exec(ctx, `select pg_notify($1, $2)`, StateChannel, project); err != nil {
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
//
// In the same transaction the write resets the project's cursor or leaves it as it is
// (settingWritten), by the rule resetCursor holds whole, which says too when the intake then reads
// the daemon whole. The write reads the URL the setting named before it under a lock of its row
// (writeSetting), a new setting's included, so two writes of one project take turns and each
// compares its URL with the one the write before it left.
func PutSettings(ctx context.Context, pool *store.Pool, project, stateURL string, actor model.Actor) (Settings, error) {
	createdBy, err := json.Marshal(actor)
	if err != nil {
		return Settings{}, fmt.Errorf("encode the Legion setting's author: %w", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Settings{}, fmt.Errorf("open the transaction of the Legion setting of %s: %w", project, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	settings, before, err := writeSetting(ctx, tx, project, stateURL, createdBy)
	if err != nil {
		return Settings{}, err
	}
	if err := settingWritten(ctx, tx, project, before, stateURL); err != nil {
		return Settings{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Settings{}, fmt.Errorf("commit the Legion setting of %s: %w", project, err)
	}
	return settings, nil
}

// writeSetting writes project's setting naming stateURL inside tx, and answers the row as it
// stands and before, the URL the setting named before the write, "" for a new setting. It reads
// before under the lock of the setting's row (`for update`), and writes the row while it holds that
// lock. A project with no setting has no row to lock, so the write inserts one and holds the lock
// of the row it inserted. Where another transaction inserted the row meanwhile, the insert waits
// for it and does nothing, and writeSetting reads that row again under its lock. So no write takes
// a setting another write left as new: two first writes of one URL take turns, and the second
// finds the URL the first wrote.
func writeSetting(ctx context.Context, tx pgx.Tx, project, stateURL string, createdBy []byte) (Settings, string, error) {
	for {
		var before string
		err := tx.QueryRow(ctx, `select state_url from legion_settings where project = $1 for update`, project).Scan(&before)
		if err == nil {
			settings, err := scanSettings(tx.QueryRow(ctx, `
				update legion_settings set state_url = $2, updated_at = now() where project = $1
				returning `+settingsColumns, project, stateURL))
			if err != nil {
				return Settings{}, "", fmt.Errorf("write the Legion setting of %s: %w", project, err)
			}
			return settings, before, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Settings{}, "", fmt.Errorf("read the Legion setting of %s before writing it: %w", project, err)
		}
		settings, err := scanSettings(tx.QueryRow(ctx, `
			insert into legion_settings (project, state_url, created_by, updated_at)
			values ($1, $2, $3, now())
			on conflict (project) do nothing
			returning `+settingsColumns, project, stateURL, createdBy))
		if err == nil {
			return settings, "", nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Settings{}, "", fmt.Errorf("write the Legion setting of %s: %w", project, err)
		}
	}
}

// DeleteSettings removes a project's setting and reports whether it had one. The project's reads
// and observations stay: they are what was observed, and nothing re-reads them. The delete reads
// the URL the setting named as it deletes the row, under the row's lock, and where it had one it
// resets the project's cursor in the same transaction (settingWritten), by the rule resetCursor and
// cursor.reset hold whole (read-rule-cases row 6.5).
func DeleteSettings(ctx context.Context, pool *store.Pool, project string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("open the transaction of the Legion setting of %s: %w", project, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var before string
	err = tx.QueryRow(ctx, `delete from legion_settings where project = $1 returning state_url`, project).Scan(&before)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("delete the Legion setting of %s: %w", project, err)
	}
	if err := settingWritten(ctx, tx, project, before, ""); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit the delete of the Legion setting of %s: %w", project, err)
	}
	return true, nil
}

// settingWritten is what every write of project's setting, a delete included, does in its own
// transaction, before being the URL the setting named before the write ("" for a new setting) and
// stateURL the daemon the setting names from the write on, "" for a delete. It resets the
// project's cursor or leaves it as it is (resetCursor), by the rule that resetCursor and
// cursor.reset hold whole (read-rule-cases rows 6.1 to 6.5), and notifies SettingsChannel, which
// reaches the intake at the write's commit (Intake.SettingsChanged).
func settingWritten(ctx context.Context, tx pgx.Tx, project, before, stateURL string) error {
	err := resetCursor(ctx, tx, project, before, stateURL)
	if err == nil {
		_, err = tx.Exec(ctx, `select pg_notify($2, $1)`, project, SettingsChannel)
	}
	if err != nil {
		return fmt.Errorf("forget where the Legion events of %s stood: %w", project, err)
	}
	return nil
}

// recordRead is the step every whole read's record takes inside its transaction (recordWhole):
// the read's observations where it observed them (recordObservations), its legion_reads row of
// kind `read` at the read's time, naming reason, its failure and its size (readBytes), and its
// announcement on StateChannel (NotifyRead). It answers how many observations it wrote.
//
// The board's header reads the newest legion_reads row as the last read, and its columns read the
// newest observations, so the two commit together or not at all: a read whose observations
// Postgres refuses leaves no row saying it succeeded over data it did not store.
func recordRead(ctx context.Context, tx pgx.Tx, project, reason string, read *wholeRead) (written int64, err error) {
	if read.observed {
		if written, err = recordObservations(ctx, tx, project, read.at, read.observations); err != nil {
			return 0, err
		}
	}
	var failure *string
	if read.failure != nil {
		text := read.failure.Error()
		failure = &text
	}
	if _, err := tx.Exec(ctx, insertRead, project, read.at, read.failure == nil, failure, readBytes(read.bytes), readKindRead, reason); err != nil {
		return 0, fmt.Errorf("store the Legion state read of %s: %w", project, err)
	}
	if err := NotifyRead(ctx, tx, project); err != nil {
		return 0, err
	}
	return written, nil
}

// insertRead appends one legion_reads row: project, read_at, ok, error, bytes, kind and reason.
// recorded_at is the transaction's own time, its default.
const insertRead = `
	insert into legion_reads (project, read_at, ok, error, bytes, kind, reason)
	values ($1, $2, $3, $4, $5, $6, $7)
`

// readBytes is a whole read's size as its legion_reads row records it. bytes is an integer column
// (migration 20261103170923_legion_state), so a size past what it holds, the Content-Length of an
// answer over the cap, is recorded as math.MaxInt32, the most the column holds. The row's error
// names the size whole (responseTooLarge), and the row is written all the same.
func readBytes(size int64) int64 {
	return min(size, math.MaxInt32)
}

// The kinds of legion_reads row (migration 20261118212431): a whole read of a daemon's state, and
// one transaction of state events the intake applied, which its last message writes (applyBatch).
const (
	readKindRead   = "read"
	readKindEvents = "events"
)

// ReadRetention is how long legion_reads keeps a read of a daemon. The Legion board reads the
// newest row of a project and nothing reads an older one, so the rest are a week of history for
// whoever asks why a daemon stopped answering, or how late its events reached Dispatch
// (recorded_at beside read_at). The table gains a row for each transaction of state events the
// intake applies, which its last message writes (applyBatch), and one for each whole read.
const ReadRetention = 7 * 24 * time.Hour

// PruneReads deletes every read older than ReadRetention that is not its project's newest, and
// answers how many it deleted: the table is append-only, a row per batch of state events and per
// whole read, and the intake runs this after a batch at most once an hour (Intake.pruneHourly), as
// delivery.PruneChanges keeps delivery_changes from the loop that writes it.
//
// Each project keeps its newest read whatever its age, because that row is the freshness the
// board's header shows: a daemon nobody has read for a week reads as a stale read, never as a
// daemon that was never read. The newest is the newest Dispatch wrote, by recorded_at and then id,
// as the header reads it (api/legion_board.go): a batch's read_at is the daemon's clock and a whole
// read's is Dispatch's, so read_at does not order the two kinds. The cutoff is the database's own
// now(), so one clock decides whichever replica prunes.
func PruneReads(ctx context.Context, pool *store.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `
		delete from legion_reads old
		where old.read_at < now() - $1::interval
		  and exists (
		    select 1 from legion_reads newer
		    where newer.project = old.project and (newer.recorded_at, newer.id) > (old.recorded_at, old.id)
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
// reads one row per field rather than the project's whole history. A field whose newest row is
// `null` compares as `null`, as a field with no row does.
//
// Every row is observed at one moment: the read's time, or 1 µs after the project's newest row
// where that is later, the `null` rows included. So a read's rows are always their fields' newest,
// and legion_current, which moves only forward, holds them; and every row of the read shares its
// moment, which is how the metrics tell an issue the read dropped (a `null` phase and a `null`
// slot at one moment) from a slot released (metrics' slotSpans). A row an event wrote (the
// intake's recordEvents) is observed at the daemon's commit time, which can be past Dispatch's
// clock when the daemon's runs ahead, and a whole read made after it is the newer state all the
// same; the events follow the same rule the other way round, field by field, so whichever of the
// two Dispatch writes last is the newest. The read's own legion_reads row keeps the read's time.
//
// One statement, so a read of some hundreds of fields is one round trip, and Postgres's own jsonb
// comparison decides what changed: a Go encoding that ordered an object's keys differently from
// jsonb would otherwise write a row at every read for a value that never moved. A row another
// process already wrote is therefore not written twice, even where two holders of the intake's
// durable overlapped.
func recordObservations(ctx context.Context, tx pgx.Tx, project string, at time.Time, observations []Observation) (int64, error) {
	issueKeys, fields, values := observationColumns(observations)
	tag, err := tx.Exec(ctx, `
		with found as (
			select observed.issue_key, observed.field, observed.value::jsonb as value
			from unnest($2::text[], $3::text[], $4::text[]) as observed(issue_key, field, value)
		), newest as (
			select issue_key, field, value
			from legion_current
			where project = $1
		), moment as (
			select greatest($5::timestamptz, max(observed_at) + interval '1 microsecond') as at
			from legion_current
			where project = $1
		)
		insert into legion_observations (project, issue_key, field, value, observed_at)
		select $1, coalesce(found.issue_key, newest.issue_key), coalesce(found.field, newest.field),
			coalesce(found.value, 'null'::jsonb), moment.at
		from found
		full join newest on newest.issue_key = found.issue_key and newest.field = found.field
		cross join moment
		where coalesce(newest.value, 'null'::jsonb) is distinct from coalesce(found.value, 'null'::jsonb)
	`, project, issueKeys, fields, values, at)
	if err != nil {
		return 0, fmt.Errorf("store the Legion observations of %s: %w", project, err)
	}
	return tag.RowsAffected(), nil
}

// observationColumns is observations as the three arrays recordObservations and recordEvents
// unnest, in the observations' order: the issue keys, the fields and the values' JSON text.
func observationColumns(observations []Observation) (issueKeys, fields, values []string) {
	issueKeys = make([]string, len(observations))
	fields = make([]string, len(observations))
	values = make([]string, len(observations))
	for index, observation := range observations {
		issueKeys[index], fields[index], values[index] = observation.IssueKey, observation.Field, string(observation.Value)
	}
	return issueKeys, fields, values
}

// Newest is one field's newest value, never `null`, and when Dispatch recorded it: when the
// field took that value, since a row is written only where a value moved.
type Newest struct {
	Observation
	ObservedAt time.Time
}

// Queryer is what ReadCurrent needs of its connection: the pool, or a transaction a reader holds
// open on it so its other reads of what the intake writes answer the same moment — the Legion
// board's header (api/legion_board.go's readRecorded) and the metrics' observations (theirs).
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Current is what project's legion_current holds as of the newest good read and every batch of
// state events applied after it. A caller that reads it beside another of the intake's tables
// does so in one repeatable-read transaction (Queryer), which is what makes one answer one
// snapshot; ReadCurrent is the one statement of which issues the daemon reports.
type Current struct {
	// Newest is every field that holds a value: every field of every issue the newest good read
	// carried or a later state event gave a value, and the project's own under AdmissionKey. The
	// Legion board's cards are these.
	Newest []Newest
	// Reported is every issue the daemon reports, against the `tree` its newest observation names
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
