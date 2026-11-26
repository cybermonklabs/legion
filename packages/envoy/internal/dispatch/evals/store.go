package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Store is the registry over one Dispatch database. Its writes join the document service's
// transaction (docs.API.Join), so the sections they write to `evals-md` commit with their rows,
// and append the documents' events through the server's broker, which publishes them once the
// transaction has committed. Its delivery day rows attribute each pull request by the candidates
// attributor resolves (delivery_days.go's readAttribution), under the rename pairs it was built of.
type Store struct {
	database   *store.Store
	documents  docs.API
	events     *events.Broker
	attributor *delivery.Attributor
}

// New is the registry over database, writing `evals-md` through documents and its events
// through broker, and reading what attributes its delivery day rows through attributor: the
// server's own four, so a document the registry writes is one its readers and its live rooms
// already know, and a pull request is attributed by the rename pairs every reader of a repository
// name reads.
func New(database *store.Store, documents docs.API, broker *events.Broker, attributor *delivery.Attributor) *Store {
	return &Store{database: database, documents: documents, events: broker, attributor: attributor}
}

// slugPattern is the slug check of eval_versions, so a write is refused with a stable code
// before the database would refuse it with a constraint's name.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// EvalID is the id of the eval slug names in project: `<project_key>/<slug>`, the id all three
// registry tables carry.
func EvalID(project, slug string) string {
	return project + "/" + slug
}

// slugOf is the slug of evalID, an eval of project: EvalID's inverse. An id of another project's
// eval is answered whole.
func slugOf(project, evalID string) string {
	return strings.TrimPrefix(evalID, project+"/")
}

// writeTx is one registry write's transaction and what its commit owes: the ledger its document
// writes and their events queue on (docs.Ledger), the document events it queued, published once
// it commits, and the documents it wrote that owe a settlement, whose settlement is scheduled
// then, as an upload and an edit schedule theirs: an edit of `evals-md` that applied an
// operation, and a create of it whose seed owes one (createDocument). now is the transaction's
// own now(), which every time the write stores is.
type writeTx struct {
	pgx.Tx
	ledger  *docs.Ledger
	now     time.Time
	project string
	// projectName is the project's name, which titles its `evals-md` document.
	projectName string
	events      []*model.Event
	settle      []string
}

// writeAttempts is how many transactions inWrite runs one write in at most: a second where the
// first found the project's `evals-md` removed while it waited for the document's owner lock
// (errDocumentRemoved), which creates the document anew.
const writeAttempts = 2

// inWrite runs fn in one transaction joined with the document service, after reading the project
// (refused PROJECT_NOT_FOUND when there is none), and commits through the ledger, so the document
// writes fn makes commit with its rows or not at all. ctx, as fn receives it, is the context Join
// returned.
//
// The transaction runs in the two phases every joined writer runs in (docs.Ledger): fn takes its
// locks, reads and does its document work first, holding no transaction id, the document's writes
// and events queued on the ledger as it makes them; then it writes its own rows, and the ledger's
// Commit sends the queued writes as one batch after them and commits. A row lock (`for update`)
// gives the transaction an id as a write does, so fn takes it after the advisory locks it may
// wait for, its evals' locks first (lockEval): every reader of the event log waits for each
// running transaction id (events.Horizon), and a transaction that waited for a lock holding one
// would hold them all back for the wait. fn is run again, in a new transaction, where its first
// run found the document removed under it (errDocumentRemoved), which it answers before it writes
// anything.
//
// Every registry write logs a change for its versions, readings and day rows, whose triggers
// record each in the change log's outbox (delivery_changes_pending, migration 20261117100001) and
// take no lock, so the write never takes the change log's lock (6900, delivery's
// lockChangesStatement). Once it has committed, committed moves a batch of the outbox into the
// log (drainChanges) before the caller answers.
func (s *Store) inWrite(ctx context.Context, project string, fn func(ctx context.Context, w *writeTx) error) error {
	if s.database == nil || s.database.Pool == nil {
		return errors.New("evals: the registry has no database")
	}
	for attempt := 1; ; attempt++ {
		err := s.writeOnce(ctx, project, fn)
		if !errors.Is(err, errDocumentRemoved) || attempt == writeAttempts {
			return err
		}
	}
}

// writeOnce is one transaction of inWrite.
func (s *Store) writeOnce(ctx context.Context, project string, fn func(ctx context.Context, w *writeTx) error) error {
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the eval write: %w", err)
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.documents.Join(ctx, tx)
	defer ledger.Discard()
	w, err := openWrite(ctx, tx, ledger, project)
	if err != nil {
		return err
	}
	if err := fn(documentCtx, w); err != nil {
		return err
	}
	if err := ledger.Commit(ctx); err != nil {
		return fmt.Errorf("commit the eval write: %w", err)
	}
	s.committed(ctx, w)
	return nil
}

// openWrite is a registry write of project in tx, whose document writes queue on ledger: the
// project's name and the transaction's own now() read, refused PROJECT_NOT_FOUND when tx holds no
// such project.
func openWrite(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, project string) (*writeTx, error) {
	w := &writeTx{Tx: tx, ledger: ledger, project: project}
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
// each document it wrote that owes one is scheduled, the document events it queued are published,
// and a batch of the change log's outbox, its own change rows among them where no backlog lies
// ahead of them, is moved into the change log (drainChanges).
func (s *Store) committed(ctx context.Context, w *writeTx) {
	for _, artifactID := range w.settle {
		s.documents.ScheduleSettlement(artifactID)
	}
	for _, event := range w.events {
		s.events.Publish(*event)
	}
	s.drainChanges(ctx)
}

// drainChanges moves one batch of the change rows committed writes recorded in the change log's
// outbox into the log (delivery.DrainAfterCommit), the lowest ids first, in a short transaction of
// its own. One batch and no more: every eval write drains here, a readings or register write
// before its route answers, so a backlog another writer left (a whole-project facts pass leaves
// thousands of rows) would otherwise be moved whole by the next write that answers a person. A
// write that finds more than a batch ahead of its own rows leaves them pending, as it does when
// its drain fails, which is logged and fails nothing: the write committed, its pending rows with
// it, and the next drain of any process moves them, or else the next process's boot before it
// serves (delivery.DrainPending states the bound).
func (s *Store) drainChanges(ctx context.Context) {
	delivery.DrainAfterCommit(ctx, s.database.Pool, "dispatch: an eval write could not move its changes into the delivery change log; the next drain moves them")
}

// lockEval serialises, for the rest of tx, every write of one eval's versions and series and of
// its readings' day rows: the register write, every readings write, and the zero fill of a logged
// eval's days (lockEvals). Each reads the newest version after taking it, in a statement of its
// own, so it reads the version the writer before it committed, and the next version number it
// writes is free. A row lock on the newest version would not do that: a statement that waited for
// it still answers the row its snapshot read, the version before the one that writer added. A
// readings write that read a series' listing without it could write that listing into its day
// rows after a register write had listed or unlisted the series and its stored rows (relistDays).
func lockEval(ctx context.Context, tx pgx.Tx, evalID string) error {
	return lockEvals(ctx, tx, []string{evalID})
}

// lockEvals takes the lock of each eval evalIDs names (lockEval), in one statement, one lock per
// distinct key and in the order of the keys rather than of the ids: two ids whose keys collide
// share one lock, and ordering by id could take that lock before a third eval's in one
// transaction and after it in another, a cycle Postgres breaks with `deadlock detected` (40P01),
// which is why store.LockOwners sorts by its lock key too. In key order, two writers that each
// take several evals' locks through it take any two keys in one order, so they wait on each other
// in turn and never in a cycle. The order by is the outer statement's, so Postgres sorts the keys
// before it takes the lock of each: a volatile function of the select list runs after the sort.
// The zero fill takes them so, before it reads the evals' listings or writes a day row
// (fillLoggedZeros), and the seed before it reads what the project holds (ensure).
func lockEvals(ctx context.Context, tx pgx.Tx, evalIDs []string) error {
	if _, err := tx.Exec(ctx, `
		select pg_advisory_xact_lock(key)
		from (select distinct hashtext('eval:' || id) as key from unnest($1::text[]) as id) keys
		order by key
	`, evalIDs); err != nil {
		return fmt.Errorf("lock the evals %v: %w", evalIDs, err)
	}
	return nil
}

// definitionColumns is the column list scanDefinition reads from eval_versions or evals_current,
// its nullable text columns read as "".
const definitionColumns = `eval_id, version, project_key, slug, name, question, number_rule, decision, doc_block,
	failure_modes, unit, coalesce(direction, ''), target, target_inclusive, target_judges, owner_role, person,
	coalesce(issue_key, ''), kind, source, state, coalesce(replaced_by, ''), links, written_via, written_by, created_at,
	set_by_person`

func scanDefinition(row pgx.Row) (Definition, error) {
	var definition Definition
	var failureModes, links, writtenBy []byte
	if err := row.Scan(
		&definition.EvalID, &definition.Version, &definition.ProjectKey, &definition.Slug, &definition.Name,
		&definition.Question, &definition.NumberRule, &definition.Decision, &definition.DocBlock,
		&failureModes, &definition.Unit, &definition.Direction, &definition.Target, &definition.TargetInclusive,
		&definition.TargetJudges, &definition.OwnerRole, &definition.Person, &definition.IssueKey,
		&definition.Kind, &definition.Source, &definition.State, &definition.ReplacedBy, &links,
		&definition.WrittenVia, &writtenBy, &definition.CreatedAt, &definition.SetByPerson,
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
const seriesColumns = `eval_id, series, version, unit, role, is_primary, min_n, listed, agg, links, created_at, set_by_person`

func scanSeries(row pgx.Row) (Series, error) {
	var series Series
	var links []byte
	if err := row.Scan(
		&series.EvalID, &series.Series, &series.Version, &series.Unit, &series.Role, &series.IsPrimary,
		&series.MinN, &series.Listed, &series.Agg, &links, &series.CreatedAt, &series.SetByPerson,
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
	reading.SupersededAt = utcTime(reading.SupersededAt)
	return reading, nil
}

// dayColumns is the column list scanDay reads from eval_days.
const dayColumns = `project_key, eval_id, deploy_repo, series, repo, day, listed, n, sum, num, den, max, samples,
	last_value, last_at, last_recorded_at, coalesce(last_reading::text, ''), counts, via_repository, built_by, updated_at`

func scanDay(row pgx.Row) (DayRow, error) {
	var day DayRow
	var counts []byte
	if err := row.Scan(
		&day.ProjectKey, &day.EvalID, &day.DeployRepo, &day.Series, &day.Repo, &day.Day, &day.Listed, &day.N, &day.Sum,
		&day.Num, &day.Den, &day.Max, &day.Samples, &day.LastValue, &day.LastAt, &day.LastRecordedAt, &day.LastReading,
		&counts, &day.ViaRepository, &day.BuiltBy, &day.UpdatedAt,
	); err != nil {
		return DayRow{}, err
	}
	if err := json.Unmarshal(counts, &day.Counts); err != nil {
		return DayRow{}, fmt.Errorf("decode the counts of a day row of %s: %w", day.EvalID, err)
	}
	day.UpdatedAt = day.UpdatedAt.UTC()
	utcPart(&day.DayPart)
	return day, nil
}

// scanWindowRow reads one row of a DayWindowIn read that names no repository and folds at most one
// deploy repository per project: whether it answers for the window or the compare range, then its
// eval, series and day, its parts, its updated_at and the owner it is stored under (owners.go).
// Every row of that read is stored under "", which the row's repo, left empty, says.
func scanWindowRow(row pgx.CollectableRow) (WindowRow, error) {
	return scanWindowRowOf(row, false, false)
}

// scanWindowRowOf reads one row of a DayWindowIn read, scanning repo and deployRepo after owner
// when the query selects them.
func scanWindowRowOf(row pgx.CollectableRow, repo, deployRepo bool) (WindowRow, error) {
	var inWindow bool
	var day WindowRow
	part := &day.Part
	fields := append(make([]any, 0, 20), &inWindow, &day.EvalID, &part.Series, &part.Day, &part.N, &part.Sum, &part.Num, &part.Den, &part.Max,
		&part.Samples, &part.LastValue, &part.LastAt, &part.LastRecordedAt, &part.LastReading, &part.Counts, &part.ViaRepository,
		&day.UpdatedAt, &day.owner)
	if repo {
		fields = append(fields, &day.repo)
	}
	if deployRepo {
		fields = append(fields, &day.deployRepo)
	}
	if err := row.Scan(fields...); err != nil {
		return WindowRow{}, err
	}
	day.Range = RangeCompare
	if inWindow {
		day.Range = RangeWindow
	}
	day.UpdatedAt = day.UpdatedAt.UTC()
	utcPart(part)
	return day, nil
}

// utcPart sets each time of part, as a row is read back, in UTC.
func utcPart(part *measures.DayPart) {
	part.Day = part.Day.UTC()
	part.LastAt, part.LastRecordedAt = utcTime(part.LastAt), utcTime(part.LastRecordedAt)
}

// utcTime is at in UTC, nil where at is.
func utcTime(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	utc := at.UTC()
	return &utc
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

// insertDefinition writes definition as a new version. The writer holds the eval's lock
// (lockEval), under which it read the version before it, so the version's number is free: a
// version a write that holds no such lock stored first refuses the insert.
func insertDefinition(ctx context.Context, tx pgx.Tx, definition Definition) (Definition, error) {
	statement, args, err := definitionInsert(definition)
	if err != nil {
		return Definition{}, err
	}
	stored, err := scanDefinition(tx.QueryRow(ctx, statement+` returning `+definitionColumns, args...))
	if err != nil {
		return Definition{}, fmt.Errorf("write version %d of %s: %w", definition.Version, definition.EvalID, err)
	}
	return stored, nil
}

// definitionInsert is the statement that writes definition as a new version, and its arguments.
func definitionInsert(definition Definition) (string, []any, error) {
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
	return `
		insert into eval_versions (eval_id, version, project_key, slug, name, question, number_rule, decision,
			doc_block, failure_modes, unit, direction, target, target_inclusive, target_judges, owner_role, person,
			issue_key, kind, source, state, replaced_by, links, written_via, written_by, set_by_person)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, nullif($12, ''), $13, $14, $15, $16, $17,
			nullif($18, ''), $19, $20, $21, nullif($22, ''), $23, $24, $25, $26)`, []any{
		definition.EvalID, definition.Version, definition.ProjectKey, definition.Slug, definition.Name,
		definition.Question, definition.NumberRule, definition.Decision, definition.DocBlock, failureModes,
		definition.Unit, definition.Direction, definition.Target, definition.TargetInclusive,
		nonNil(definition.TargetJudges), definition.OwnerRole, definition.Person, definition.IssueKey,
		definition.Kind, definition.Source, definition.State, definition.ReplacedBy, links,
		definition.WrittenVia, writtenBy, nonNil(definition.SetByPerson),
	}, nil
}

// insertSeries writes series as a new version of that series, or nothing where that version
// exists already, answered inserted false. Every writer of a series holds its eval's lock
// (lockEval, lockEvals), under which it read the version before it, so only a write that holds no
// such lock could have stored that version first, and each caller answers that as an error.
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
		insert into eval_series (eval_id, series, version, unit, role, is_primary, min_n, listed, agg, links, set_by_person)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		on conflict (eval_id, series, version) do nothing`, []any{
		series.EvalID, series.Series, series.Version, series.Unit, series.Role, series.IsPrimary, series.MinN,
		series.Listed, series.Agg, links, nonNil(series.SetByPerson),
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
