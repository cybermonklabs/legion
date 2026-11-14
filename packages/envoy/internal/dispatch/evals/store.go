package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Store is the registry over one Dispatch database. Its writes join the document service's
// transaction (docs.API.Join), so the sections they write to `evals-md` commit with their rows,
// and append the documents' events through the server's broker, which publishes them once the
// transaction has committed.
type Store struct {
	database  *store.Store
	documents docs.API
	events    *events.Broker
}

// New is the registry over database, writing `evals-md` through documents and its events
// through broker: the server's own three, so a document the registry writes is one its readers
// and its live rooms already know.
func New(database *store.Store, documents docs.API, broker *events.Broker) *Store {
	return &Store{database: database, documents: documents, events: broker}
}

// slugPattern is the slug check of eval_versions, so a write is refused with a stable code
// before the database would refuse it with a constraint's name.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// EvalID is the id of the eval slug names in project: `<project_key>/<slug>`, the id all three
// registry tables carry.
func EvalID(project, slug string) string {
	return project + "/" + slug
}

// writeTx is one registry write's transaction and what its commit owes: the document events it
// appended, published once it commits, and the documents it wrote, whose settlement is scheduled
// then, as an upload and an edit schedule theirs. now is the transaction's own now(), which every
// time the write stores is.
type writeTx struct {
	pgx.Tx
	now     time.Time
	project string
	// projectName is the project's name, which titles its `evals-md` document.
	projectName string
	events      []model.Event
	settle      []string
}

// inWrite runs fn in one transaction joined with the document service, after reading the
// project (refused PROJECT_NOT_FOUND when there is none), and commits through the ledger, so the
// document writes fn makes commit with its rows or not at all. ctx, as fn receives it, is the
// context Join returned.
func (s *Store) inWrite(ctx context.Context, project string, fn func(ctx context.Context, w *writeTx) error) error {
	if s.database == nil || s.database.Pool == nil {
		return errors.New("evals: the registry has no database")
	}
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the eval write: %w", err)
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.documents.Join(ctx, tx)
	defer ledger.Discard()
	w, err := openWrite(ctx, tx, project)
	if err != nil {
		return err
	}
	if err := fn(documentCtx, w); err != nil {
		return err
	}
	if err := ledger.Commit(ctx); err != nil {
		return fmt.Errorf("commit the eval write: %w", err)
	}
	s.committed(w)
	return nil
}

// openWrite is a registry write of project in tx: the project's name and the transaction's own
// now() read, refused PROJECT_NOT_FOUND when tx holds no such project.
func openWrite(ctx context.Context, tx pgx.Tx, project string) (*writeTx, error) {
	w := &writeTx{Tx: tx, project: project}
	if err := tx.QueryRow(ctx, `select name, now() from projects where key = $1`, project).Scan(&w.projectName, &w.now); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refusal(CodeProjectNotFound, "no project has the key %q", project)
		}
		return nil, fmt.Errorf("read project %s: %w", project, err)
	}
	w.now = w.now.UTC()
	return w, nil
}

// committed is what w's commit owes, paid once its transaction has committed: the settlement of
// each document it wrote is scheduled, and the document events it appended are published.
func (s *Store) committed(w *writeTx) {
	for _, artifactID := range w.settle {
		s.documents.ScheduleSettlement(artifactID)
	}
	for _, event := range w.events {
		s.events.Publish(event)
	}
}

// lockEval serialises, for the rest of tx, every write of one eval's versions and series: the
// register write and a readings write that registers the eval or a series of it. Each reads the
// newest version after taking it, in a statement of its own, so it reads the version the writer
// before it committed, and the next version number it writes is free. A row lock on the newest
// version would not do that: a statement that waited for it still answers the row its snapshot
// read, the version before the one that writer added.
func lockEval(ctx context.Context, tx pgx.Tx, evalID string) error {
	if _, err := tx.Exec(ctx, lockEvalStatement, evalID); err != nil {
		return fmt.Errorf("lock eval %s: %w", evalID, err)
	}
	return nil
}

// lockEvalStatement takes the lock of the eval $1 names (lockEval).
const lockEvalStatement = `select pg_advisory_xact_lock(hashtext('eval:' || $1))`

// definitionColumns is the column list scanDefinition reads from eval_versions or evals_current,
// its nullable text columns read as "".
const definitionColumns = `eval_id, version, project_key, slug, name, question, number_rule, decision, doc_block,
	failure_modes, unit, coalesce(direction, ''), target, target_inclusive, target_judges, owner_role, person,
	coalesce(issue_key, ''), kind, source, state, coalesce(replaced_by, ''), links, written_via, written_by, created_at`

func scanDefinition(row pgx.Row) (Definition, error) {
	var definition Definition
	var failureModes, links, writtenBy []byte
	if err := row.Scan(
		&definition.EvalID, &definition.Version, &definition.ProjectKey, &definition.Slug, &definition.Name,
		&definition.Question, &definition.NumberRule, &definition.Decision, &definition.DocBlock,
		&failureModes, &definition.Unit, &definition.Direction, &definition.Target, &definition.TargetInclusive,
		&definition.TargetJudges, &definition.OwnerRole, &definition.Person, &definition.IssueKey,
		&definition.Kind, &definition.Source, &definition.State, &definition.ReplacedBy, &links,
		&definition.WrittenVia, &writtenBy, &definition.CreatedAt,
	); err != nil {
		return Definition{}, err
	}
	if err := json.Unmarshal(failureModes, &definition.FailureModes); err != nil {
		return Definition{}, fmt.Errorf("decode the failure modes of %s: %w", definition.EvalID, err)
	}
	if err := json.Unmarshal(links, &definition.Links); err != nil {
		return Definition{}, fmt.Errorf("decode the links of %s: %w", definition.EvalID, err)
	}
	if err := json.Unmarshal(writtenBy, &definition.WrittenBy); err != nil {
		return Definition{}, fmt.Errorf("decode the writer of %s: %w", definition.EvalID, err)
	}
	if definition.TargetJudges == nil {
		definition.TargetJudges = []string{}
	}
	definition.Links = utcLinks(definition.Links)
	definition.CreatedAt = definition.CreatedAt.UTC()
	return definition, nil
}

// seriesColumns is the column list scanSeries reads from eval_series or eval_series_current.
const seriesColumns = `eval_id, series, version, unit, role, is_primary, min_n, listed, agg, links, created_at`

func scanSeries(row pgx.Row) (Series, error) {
	var series Series
	var links []byte
	if err := row.Scan(
		&series.EvalID, &series.Series, &series.Version, &series.Unit, &series.Role, &series.IsPrimary,
		&series.MinN, &series.Listed, &series.Agg, &links, &series.CreatedAt,
	); err != nil {
		return Series{}, err
	}
	if err := json.Unmarshal(links, &series.Links); err != nil {
		return Series{}, fmt.Errorf("decode the links of series %s of %s: %w", series.Series, series.EvalID, err)
	}
	series.Links = utcLinks(series.Links)
	series.CreatedAt = series.CreatedAt.UTC()
	return series, nil
}

// ReadingColumns is the column list scanReading reads from eval_readings: what every read of a
// reading selects, the items read's among them, which evalbench's checks explain as it is sent.
const ReadingColumns = `id::text, eval_id, series, row_key, row_kind, col, value, value_absent_reason, unit, n, n_unit,
	ci_low, ci_high, ci_level, attributes, links, measured_at, recorded_at, recorded_by, source, note,
	coalesce(replaces::text, ''), superseded_at`

func scanReading(row pgx.Row) (Reading, error) {
	var reading Reading
	var attributes, links, recordedBy []byte
	if err := row.Scan(
		&reading.ID, &reading.EvalID, &reading.Series, &reading.RowKey, &reading.RowKind, &reading.Col,
		&reading.Value, &reading.ValueAbsentReason, &reading.Unit, &reading.N, &reading.NUnit,
		&reading.CILow, &reading.CIHigh, &reading.CILevel, &attributes, &links, &reading.MeasuredAt,
		&reading.RecordedAt, &recordedBy, &reading.Source, &reading.Note, &reading.Replaces, &reading.SupersededAt,
	); err != nil {
		return Reading{}, err
	}
	if err := json.Unmarshal(attributes, &reading.Attributes); err != nil {
		return Reading{}, fmt.Errorf("decode the attributes of reading %s: %w", reading.ID, err)
	}
	if err := json.Unmarshal(links, &reading.Links); err != nil {
		return Reading{}, fmt.Errorf("decode the links of reading %s: %w", reading.ID, err)
	}
	if err := json.Unmarshal(recordedBy, &reading.RecordedBy); err != nil {
		return Reading{}, fmt.Errorf("decode the recorder of reading %s: %w", reading.ID, err)
	}
	reading.Links = utcLinks(reading.Links)
	reading.MeasuredAt = reading.MeasuredAt.UTC()
	reading.RecordedAt = reading.RecordedAt.UTC()
	if reading.SupersededAt != nil {
		superseded := reading.SupersededAt.UTC()
		reading.SupersededAt = &superseded
	}
	return reading, nil
}

// dayColumns is the column list scanDay reads from eval_days.
const dayColumns = `project_key, eval_id, series, repo, day, listed, n, sum, num, den, max, samples,
	last_value, last_at, counts, via_repository, built_by, updated_at`

func scanDay(row pgx.Row) (DayRow, error) {
	var day DayRow
	var counts []byte
	if err := row.Scan(
		&day.ProjectKey, &day.EvalID, &day.Series, &day.Repo, &day.Day, &day.Listed, &day.N, &day.Sum,
		&day.Num, &day.Den, &day.Max, &day.Samples, &day.LastValue, &day.LastAt, &counts,
		&day.ViaRepository, &day.BuiltBy, &day.UpdatedAt,
	); err != nil {
		return DayRow{}, err
	}
	if err := json.Unmarshal(counts, &day.Counts); err != nil {
		return DayRow{}, fmt.Errorf("decode the counts of a day row of %s: %w", day.EvalID, err)
	}
	day.Day = day.Day.UTC()
	day.UpdatedAt = day.UpdatedAt.UTC()
	if day.LastAt != nil {
		lastAt := day.LastAt.UTC()
		day.LastAt = &lastAt
	}
	return day, nil
}

// scanWindowRow reads one row of a DayWindow read: whether it answers for the window or the
// compare range, then its eval, series and day, its parts and its updated_at.
func scanWindowRow(row pgx.CollectableRow) (WindowRow, error) {
	var inWindow bool
	var day WindowRow
	part := &day.Part
	if err := row.Scan(&inWindow, &day.EvalID, &part.Series, &part.Day, &part.N, &part.Sum, &part.Num, &part.Den, &part.Max,
		&part.Samples, &part.LastValue, &part.LastAt, &part.Counts, &part.ViaRepository, &day.UpdatedAt); err != nil {
		return WindowRow{}, err
	}
	day.Range = RangeCompare
	if inWindow {
		day.Range = RangeWindow
	}
	part.Day = part.Day.UTC()
	day.UpdatedAt = day.UpdatedAt.UTC()
	if part.LastAt != nil {
		lastAt := part.LastAt.UTC()
		part.LastAt = &lastAt
	}
	return day, nil
}

// utcLinks answers links with each recorded time in UTC, so links read from JSON and links
// stamped by a write compare alike.
func utcLinks(links []Link) []Link {
	if links == nil {
		return []Link{}
	}
	for index := range links {
		links[index].RecordedAt = links[index].RecordedAt.UTC()
	}
	return links
}

// newestDefinition reads the newest version of evalID, found false when the eval has none.
func newestDefinition(ctx context.Context, q pgx.Tx, evalID string) (Definition, bool, error) {
	definition, err := scanDefinition(q.QueryRow(ctx, `
		select `+definitionColumns+` from eval_versions where eval_id = $1 order by version desc limit 1
	`, evalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Definition{}, false, nil
	}
	if err != nil {
		return Definition{}, false, fmt.Errorf("read the newest version of %s: %w", evalID, err)
	}
	return definition, true, nil
}

// currentSeries reads the newest version of each series of evalID, by name.
func currentSeries(ctx context.Context, q pgx.Tx, evalID string) (map[string]Series, error) {
	rows, err := q.Query(ctx, `select `+seriesColumns+` from eval_series_current where eval_id = $1`, evalID)
	if err != nil {
		return nil, fmt.Errorf("read the series of %s: %w", evalID, err)
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Series, error) { return scanSeries(row) })
	if err != nil {
		return nil, fmt.Errorf("read the series of %s: %w", evalID, err)
	}
	byName := make(map[string]Series, len(all))
	for _, series := range all {
		byName[series.Series] = series
	}
	return byName, nil
}

// insertDefinition writes definition as a new version. onConflictNothing makes the insert of a
// version someone else wrote first a no-op, answered inserted false: a readings write's first
// registration of a slug, which two first readings may make at once.
func insertDefinition(ctx context.Context, tx pgx.Tx, definition Definition, onConflictNothing bool) (Definition, bool, error) {
	statement, args, err := definitionInsert(definition, onConflictNothing)
	if err != nil {
		return Definition{}, false, err
	}
	stored, err := scanDefinition(tx.QueryRow(ctx, statement+` returning `+definitionColumns, args...))
	if onConflictNothing && errors.Is(err, pgx.ErrNoRows) {
		return Definition{}, false, nil
	}
	if err != nil {
		return Definition{}, false, fmt.Errorf("write version %d of %s: %w", definition.Version, definition.EvalID, err)
	}
	return stored, true, nil
}

// definitionInsert is the statement that writes definition as a new version, and its arguments;
// onConflictNothing makes it a no-op where that version exists.
func definitionInsert(definition Definition, onConflictNothing bool) (string, []any, error) {
	failureModes, err := json.Marshal(nonNil(definition.FailureModes))
	if err != nil {
		return "", nil, fmt.Errorf("encode failure modes: %w", err)
	}
	links, err := json.Marshal(nonNil(definition.Links))
	if err != nil {
		return "", nil, fmt.Errorf("encode links: %w", err)
	}
	writtenBy, err := json.Marshal(definition.WrittenBy.Recorded())
	if err != nil {
		return "", nil, fmt.Errorf("encode the writer: %w", err)
	}
	conflict := ""
	if onConflictNothing {
		conflict = ` on conflict (eval_id, version) do nothing`
	}
	return `
		insert into eval_versions (eval_id, version, project_key, slug, name, question, number_rule, decision,
			doc_block, failure_modes, unit, direction, target, target_inclusive, target_judges, owner_role, person,
			issue_key, kind, source, state, replaced_by, links, written_via, written_by)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, nullif($12, ''), $13, $14, $15, $16, $17,
			nullif($18, ''), $19, $20, $21, nullif($22, ''), $23, $24, $25)` + conflict, []any{
		definition.EvalID, definition.Version, definition.ProjectKey, definition.Slug, definition.Name,
		definition.Question, definition.NumberRule, definition.Decision, definition.DocBlock, failureModes,
		definition.Unit, definition.Direction, definition.Target, definition.TargetInclusive,
		nonNil(definition.TargetJudges), definition.OwnerRole, definition.Person, definition.IssueKey,
		definition.Kind, definition.Source, definition.State, definition.ReplacedBy, links,
		definition.WrittenVia, writtenBy,
	}, nil
}

// insertSeries writes series as a new version of that series, or nothing where that version
// exists already (a concurrent first reading registered the series first), answered inserted
// false.
func insertSeries(ctx context.Context, tx pgx.Tx, series Series) (Series, bool, error) {
	statement, args, err := seriesInsert(series)
	if err != nil {
		return Series{}, false, err
	}
	stored, err := scanSeries(tx.QueryRow(ctx, statement+` returning `+seriesColumns, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Series{}, false, nil
	}
	if err != nil {
		return Series{}, false, fmt.Errorf("write version %d of series %s of %s: %w", series.Version, series.Series, series.EvalID, err)
	}
	return stored, true, nil
}

// seriesInsert is the statement that writes series as a new version of that series, or nothing
// where that version exists already, and its arguments.
func seriesInsert(series Series) (string, []any, error) {
	links, err := json.Marshal(nonNil(series.Links))
	if err != nil {
		return "", nil, fmt.Errorf("encode links: %w", err)
	}
	return `
		insert into eval_series (eval_id, series, version, unit, role, is_primary, min_n, listed, agg, links)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		on conflict (eval_id, series, version) do nothing`, []any{
		series.EvalID, series.Series, series.Version, series.Unit, series.Role, series.IsPrimary, series.MinN,
		series.Listed, series.Agg, links,
	}, nil
}

// nonNil is values, or an empty list where values is nil, so a list column stores [] rather
// than null.
func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
