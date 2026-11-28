package evals

// admission.go is the writer of each project's admission eval `<KEY>/admission`
// (dispatch://INFRA-2918, R22; root R39's Design part 'The admission eval'): the operations the
// Legion intake runs on it (AdmissionWriter), which dispatch/legion is given as functions since this
// package imports it; the one list of entries each state the intake applies gives, built from the
// daemon's two records, `capChanges` and `signalChanges` (admissionEntries), and the cap the state
// reports (observedCap); and their write in the intake's transaction, through the readings write
// that takes its caller's (WriteReadingsIn).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// slugAdmission is the slug of each project's admission eval.
const slugAdmission = "admission"

// The admission eval's series: the cap in force; each signal's value and limit, under the `col`
// `value` and `limit`; whether each signal holds admission, one `col` for each signal and part; and
// whether admission by signals is on.
const (
	seriesAdmissionCap       = "admission-cap"
	seriesAdmissionIdle      = "admission-idle"
	seriesAdmissionQueue     = "admission-queue"
	seriesAdmissionHeadroom  = "admission-headroom"
	seriesAdmissionKills     = "admission-kills"
	seriesAdmissionApprovals = "admission-approvals"
	seriesAdmissionHolds     = "admission-holds"
	seriesAdmissionSwitch    = "admission-switch"
)

// The row kinds of the admission eval's readings: a change of the cap the daemon recorded, a cap
// the daemon's state reported, and a change of admission by signals the daemon recorded.
const (
	rowKindCapChange    = "cap_change"
	rowKindCapObserved  = "cap_observed"
	rowKindSignalChange = "signal_change"
)

// What a signal entry changed (legion.SignalChange.What): a signal's value, its limit, whether it
// holds admission, or the switch of admission by signals.
const (
	signalValue  = "value"
	signalLimit  = "limit"
	signalHolds  = "holds"
	signalSwitch = "switch"
)

// capUnit is the unit of the cap's readings: the cap counts the issues at work, each of which holds
// a slot.
const capUnit = "issues"

// limitCleared is the absent reason of a reading of a limit a person cleared, whose entry carries
// no value and no absent reason: the line of the limit ends at it.
const limitCleared = "limit cleared"

// admissionActor is who records each reading of the admission eval (recorded_by): the Legion
// intake, which copies the daemon's record.
var admissionActor = model.Actor{Kind: "system", ID: "legion-intake"}

// AdmissionSkippedLine is the log line, at WARN on the logger of the intake that applies the state
// (legion.Admissions.Write), of each reading of a project's admission eval the writer cannot write
// and skips, writing the state's other readings, so one bad entry does not stop the project's
// intake: a reading R29's rules refuse, one the database refuses for good, and one of an eval no
// row holds, each with its code, its row kind and its row key. The entry stays in the daemon's
// record, which every later state carries whole, so the writer logs it once in each process
// (loggedEntries).
const AdmissionSkippedLine = "legion intake: an admission reading cannot be written, so it is skipped and the state's other readings are written"

// AdmissionUnknownLine is the log line, at WARN on the logger of the intake that applies the state
// (legion.Admissions.Write), of each entry of a state's `signalChanges` whose signal, part or what
// the writer does not know (signalSeries), which is skipped, with its row key, once in each process
// as AdmissionSkippedLine is.
const AdmissionUnknownLine = "legion intake: an admission signal entry is of a signal, part or what this build does not know, so it is skipped"

// AdmissionRefusedLine is the log line, at WARN on the logger of the intake that registers the eval
// (legion.Admissions.Register), of a registration of a project's admission eval that the register
// write refuses (*Error), with its code: no row then holds the eval, so the writer skips the
// state's readings and logs each as a reading of an eval no row holds (AdmissionSkippedLine), and
// the intake registers it again at the next state that serves either record.
const AdmissionRefusedLine = "legion intake: the register write refused the admission eval, so the state's admission readings are skipped"

// AdmissionWriter is the operations of each project's admission eval the Legion intake runs
// (legion.Admissions), on store, every readings write recording builtBy as the build that kept its
// day rows (Batch.BuiltBy): the eval's lock (lockEval), which the intake takes first in each of its
// transactions that may write the eval's readings; whether a project holds a row of the eval
// (admissionWriter.registered); its registration (admissionWriter.register); and the write of the
// readings of a state the intake applies (admissionWriter.write). The server builds one for its
// process, so its set of the entries it logged (loggedEntries) is the process's.
func AdmissionWriter(store *Store, builtBy string) legion.Admissions {
	writer := admissionWriter{store: store, builtBy: builtBy, logged: newLoggedEntries()}
	return legion.Admissions{Lock: lockAdmission, Registered: writer.registered, Register: writer.register, Write: writer.write}
}

// lockAdmission takes the lock of project's admission eval for the rest of tx (lockEval): a hash of
// the eval's id, so it reads nothing.
func lockAdmission(ctx context.Context, tx pgx.Tx, project string) error {
	return lockEval(ctx, tx, EvalID(project, slugAdmission))
}

// admissionWriter writes the admission eval's readings over store, as the build builtBy, and logs
// each entry it skips once (logged).
type admissionWriter struct {
	store   *Store
	builtBy string
	logged  *loggedEntries
}

// registered answers whether a row of project's admission eval exists: a version of it.
func (w admissionWriter) registered(ctx context.Context, project string) (bool, error) {
	var held bool
	if err := w.store.database.Pool.QueryRow(ctx, `select exists (select from eval_versions where eval_id = $1)`,
		EvalID(project, slugAdmission)).Scan(&held); err != nil {
		return false, fmt.Errorf("read whether %s holds its admission eval: %w", project, err)
	}
	return held, nil
}

// register registers project's admission eval through the register write (Store.Register), in that
// write's own transaction, with the root's definition (admissionEval, admissionRegistration), its
// person the project's default assignee. A registration the register write refuses (*Error) is
// logged on logger with its code (AdmissionRefusedLine) and answers nil, since a retry would be
// refused for ever; any other error is one a retry may fix, and is answered.
func (w admissionWriter) register(ctx context.Context, logger *slog.Logger, project string) error {
	// A project no row holds reads no person, and the register write refuses it PROJECT_NOT_FOUND.
	var person string
	if err := w.store.database.Pool.QueryRow(ctx, `select default_assignee from projects where key = $1`, project).Scan(&person); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read the default assignee of %s: %w", project, err)
	}
	_, err := w.store.Register(ctx, admissionRegistration(project, person))
	var refusal *Error
	if errors.As(err, &refusal) {
		logger.Warn(AdmissionRefusedLine, "project", project, "code", refusal.Code, "field", refusal.Field, "error", refusal.Message)
		return nil
	}
	if err != nil {
		return fmt.Errorf("register the admission eval of %s: %w", project, err)
	}
	return nil
}

// admissionRegistration is the register write of project's admission eval: every field of its
// definition (admissionEval) and of each of its series, written by the Legion intake
// (admissionActor), person its person, and its section of the project's `evals-md` the method a
// seeded eval's section holds (catalogueEntry.method). It names no target and no direction, which
// an eval registered for the first time holds none of (firstDefinition).
func admissionRegistration(project, person string) Registration {
	definition := admissionEval.definition
	series := make([]SeriesChange, len(admissionEval.series))
	for index, one := range admissionEval.series {
		series[index] = SeriesChange{Series: one.Series, SeriesComputationChange: SeriesComputationChange{
			Unit: Set(one.Unit), Role: Set(one.Role), IsPrimary: Set(one.IsPrimary), Listed: Set(one.Listed), Agg: Set(one.Agg),
		}}
	}
	return Registration{
		Project: project, Slug: slugAdmission, Caller: admissionActor,
		ComputationChange: ComputationChange{
			Name: Set(definition.Name), Question: Set(definition.Question), NumberRule: Set(definition.NumberRule),
			Decision: Set(definition.Decision), FailureModes: Set(definition.FailureModes), Unit: Set(definition.Unit),
			Kind: Set(definition.Kind), Source: Set(definition.Source),
		},
		OwnerRole: Set(definition.OwnerRole), Person: Set(person), DocText: Set(admissionEval.method()), Charts: Set(definition.Charts),
		Series: series,
	}
}

// loggedEntryBound is the most keys a writer's loggedEntries holds: 4,096, some 80 times the
// newest 50 entries of the cap that the daemon's state holds for one project. A key is a project,
// a row kind, a row key and a code: 4,096 keys whose row key is a store id and an entry id took
// 852,304 bytes of heap in the map and the order together, about 208 bytes a key (measured with
// Go 1.27, 2026-11-27), so the set holds under a megabyte. A key past the bound first drops the
// older half of the keys, so an entry whose key was dropped and that the daemon's record still
// holds is logged once more.
const loggedEntryBound = 4096

// loggedEntry is the key of one skipped or unknown entry of a project's admission eval: its row
// kind and row key, and the code it was skipped for, "" for an entry of a signal, part or what the
// writer does not know, which has none.
type loggedEntry struct {
	project, rowKind, rowKey, code string
}

// loggedEntries is the set of the entries a writer has logged as skipped or unknown, each by its
// key, in the order it logged them, at most loggedEntryBound of them. Every state the intake
// applies carries the daemon's whole record, so without it each state would log every entry it
// skips again. Its runners write concurrently, so a lock guards it.
type loggedEntries struct {
	mu    sync.Mutex
	keys  map[loggedEntry]struct{}
	order []loggedEntry
}

func newLoggedEntries() *loggedEntries {
	return &loggedEntries{keys: map[loggedEntry]struct{}{}}
}

// first reports whether key is logged for the first time, and records it where it is: a set that
// holds loggedEntryBound keys first drops the older half of them.
func (l *loggedEntries) first(key loggedEntry) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, logged := l.keys[key]; logged {
		return false
	}
	if len(l.order) == loggedEntryBound {
		half := loggedEntryBound / 2
		for _, old := range l.order[:half] {
			delete(l.keys, old)
		}
		l.order = append(l.order[:0], l.order[half:]...)
	}
	l.keys[key] = struct{}{}
	l.order = append(l.order, key)
	return true
}

// write writes, in tx and under the eval's lock tx holds, the readings of state: one for each entry
// of the daemon's records its admission blocks carry (admissionEntries), and then the cap the
// newest block reports (observedCap), only where the eval holds no admission-cap reading or that
// cap differs from the newest one, once the entries are written (root R39's two cases, read as
// dispatch://INFRA-2918's plan reads them: the first is a project with no admission-cap reading). A
// reading is written only where no reading of the eval holds its row kind and row key
// (heldRowKeys), so a state applied again, by a replay or by a whole read of a state a batch
// applied, adds no row. A project that holds no row of the eval and whose state serves neither
// record gets no reading and no log line; one whose state serves either has each reading skipped
// and logged on logger as one of an eval no row holds (writeEntries). It answers the move of the
// readings' change rows into the delivery change log, nil where it wrote none, and an error only for
// a write a retry may fix.
func (w admissionWriter) write(ctx context.Context, tx pgx.Tx, logger *slog.Logger, state legion.AdmissionState) (func(context.Context), error) {
	if len(state.Blocks) == 0 {
		return nil, nil
	}
	evalID := EvalID(state.Project, slugAdmission)
	entries := admissionEntries(state, w.logged, logger)
	reported := state.Blocks[len(state.Blocks)-1].Cap
	if len(entries) == 0 && reported == nil {
		return nil, nil
	}
	if !state.ServesRecord() {
		held, err := evalHeld(ctx, tx, evalID)
		if err != nil || !held {
			return nil, err
		}
	}
	var observed *admissionEntry
	if reported != nil {
		entry := observedCap(state, *reported)
		observed = &entry
	}
	held, err := heldRowKeys(ctx, tx, evalID, entries, observed)
	if err != nil {
		return nil, err
	}
	entries = slices.DeleteFunc(entries, func(entry admissionEntry) bool { return held[entry.key()] })
	drain, err := w.writeEntries(ctx, tx, logger, state.Project, entries)
	if err != nil || observed == nil || held[observed.key()] {
		return drain, err
	}
	newest, found, err := newestCap(ctx, tx, evalID)
	if err != nil {
		return nil, err
	}
	if found && newest != nil && *newest == *observed.Value {
		return drain, nil
	}
	observedDrain, err := w.writeEntries(ctx, tx, logger, state.Project, []admissionEntry{*observed})
	if observedDrain != nil {
		drain = observedDrain
	}
	return drain, err
}

// writeEntries writes entries as readings of project's admission eval in tx (WriteReadingsIn), and
// logs on logger each it skips (AdmissionSkippedLine), once in the writer's process for its row
// kind, row key and code (loggedEntries): each row WriteReadingsIn skips, and every row of a batch
// it refuses whole (*Error), which a retry of the intake's transaction would refuse again for ever.
// It answers WriteReadingsIn's drain, nil where it wrote no reading.
func (w admissionWriter) writeEntries(ctx context.Context, tx pgx.Tx, logger *slog.Logger, project string, entries []admissionEntry) (func(context.Context), error) {
	if len(entries) == 0 {
		return nil, nil
	}
	rows := make([]json.RawMessage, len(entries))
	for index, entry := range entries {
		row, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("encode the %s reading %s of %s: %w", entry.RowKind, entry.RowKey, project, err)
		}
		rows[index] = row
	}
	written, drain, err := w.store.WriteReadingsIn(ctx, tx, Batch{Project: project, Slug: slugAdmission, Rows: rows, Caller: admissionActor, BuiltBy: w.builtBy})
	var refusal *Error
	switch {
	case errors.As(err, &refusal):
		for index := range entries {
			written.Skipped = append(written.Skipped, RowError{Index: index, Code: refusal.Code, Field: refusal.Field, Message: refusal.Message})
		}
	case err != nil:
		return nil, err
	}
	for _, skipped := range written.Skipped {
		entry := entries[skipped.Index]
		if !w.logged.first(loggedEntry{project: project, rowKind: entry.RowKind, rowKey: entry.RowKey, code: skipped.Code}) {
			continue
		}
		logger.Warn(AdmissionSkippedLine, "project", project, "series", entry.Series, "row_kind", entry.RowKind, "row_key", entry.RowKey,
			"code", skipped.Code, "field", skipped.Field, "error", skipped.Message)
	}
	return drain, nil
}

// admissionEntry is one reading of the admission eval as the writer gives it, in the keys of a row
// of the readings write (readingBody): its series, `col` and unit, its row kind and row key, when it
// was measured, its value or why it has none, its attributes and its note.
type admissionEntry struct {
	Series     string         `json:"series"`
	Col        string         `json:"col"`
	Unit       string         `json:"unit,omitempty"`
	RowKind    string         `json:"row_kind"`
	RowKey     string         `json:"row_key"`
	MeasuredAt time.Time      `json:"measured_at"`
	Value      *float64       `json:"value"`
	Absent     string         `json:"value_absent_reason,omitempty"`
	Attributes map[string]any `json:"attributes"`
	Note       string         `json:"note"`
}

// key is the entry's row kind and row key, by which a reading the eval holds already stands for it.
func (entry admissionEntry) key() [2]string {
	return [2]string{entry.RowKind, entry.RowKey}
}

// admissionRowKey is the row key of an entry of the daemon's store stateID: the store's id and the
// entry's id, or the sequence of the state that reported a cap. The store id keeps apart the entries
// of a replaced store, whose ids can repeat.
func admissionRowKey(stateID string, id int64) string {
	return stateID + ":" + strconv.FormatInt(id, 10)
}

// admissionEntries is the one list of entries the admission blocks of state give
// (dispatch://INFRA-2918's build ledger, item 65): a cap_change entry for each entry of
// `capChanges` (capEntry), and a signal_change entry for each entry of `signalChanges`
// (signalEntry), each taken once by its id, so by its row key, from the first block that carries
// it; the cap's in the order of their ids, then the signals'. An entry of a signal, part or what
// this build does not know is logged on logger (AdmissionUnknownLine), once in logged's process for
// its row key (loggedEntries), and left out.
func admissionEntries(state legion.AdmissionState, logged *loggedEntries, logger *slog.Logger) []admissionEntry {
	caps, signals := map[int64]legion.CapChange{}, map[int64]legion.SignalChange{}
	for _, block := range state.Blocks {
		for _, change := range block.CapChanges.Entries {
			if _, taken := caps[change.ID]; !taken {
				caps[change.ID] = change
			}
		}
		for _, change := range block.SignalChanges.Entries {
			if _, taken := signals[change.ID]; !taken {
				signals[change.ID] = change
			}
		}
	}
	entries := make([]admissionEntry, 0, len(caps)+len(signals))
	for _, id := range slices.Sorted(maps.Keys(caps)) {
		entries = append(entries, capEntry(state.StateID, caps[id]))
	}
	for _, id := range slices.Sorted(maps.Keys(signals)) {
		change := signals[id]
		entry, known := signalEntry(state.StateID, change)
		if !known {
			if logged.first(loggedEntry{project: state.Project, rowKind: entry.RowKind, rowKey: entry.RowKey}) {
				logger.Warn(AdmissionUnknownLine, "project", state.Project, "row_key", entry.RowKey,
					"signal", textOrNone(change.Signal), "part", textOrNone(change.Part), "what", change.What)
			}
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// capEntry is the cap_change reading of change, an entry of the daemon's store stateID: the cap the
// daemon admitted under from it on (capAfter, a clear's too, whose entry carries no cap), at the
// time the change was made; its attributes the cap before, the ceiling, who made it (setBy, the
// daemon's own member) and why; its note the same in words.
func capEntry(stateID string, change legion.CapChange) admissionEntry {
	note := fmt.Sprintf("cap %d to %d", change.CapBefore, change.CapAfter)
	if change.Cap == nil {
		note += ", its setting cleared"
	}
	note += fmt.Sprintf(", ceiling %d, set by %s", change.Ceiling, actorText(change.SetBy))
	if change.Reason != "" {
		note += ": " + change.Reason
	}
	return admissionEntry{
		Series: seriesAdmissionCap, Unit: capUnit, RowKind: rowKindCapChange, RowKey: admissionRowKey(stateID, change.ID),
		MeasuredAt: change.SetAt, Value: new(float64(change.CapAfter)),
		Attributes: map[string]any{"cap_before": change.CapBefore, "ceiling": change.Ceiling, "set_by": change.SetBy, "reason": change.Reason},
		Note:       note,
	}
}

// observedCap is the cap_observed reading of the cap a state reports: set by the daemon, keyed by
// the state's sequence, at the state's time (legion.AdmissionState).
func observedCap(state legion.AdmissionState, reported int) admissionEntry {
	return admissionEntry{
		Series: seriesAdmissionCap, Unit: capUnit, RowKind: rowKindCapObserved, RowKey: admissionRowKey(state.StateID, state.Seq),
		MeasuredAt: state.At, Value: new(float64(reported)),
		Attributes: map[string]any{"set_by": legion.Actor{Kind: "daemon"}},
		Note:       fmt.Sprintf("cap %d, as the daemon's state at sequence %d reports it", reported, state.Seq),
	}
}

// signalOf is a signal and its part, "" for none: the part `headroom` of `provider`.
type signalOf struct {
	signal, part string
}

// column is the `col` of the signal's hold in admission-holds: the signal, or `<signal>.<part>`.
func (of signalOf) column() string {
	if of.part == "" {
		return of.signal
	}
	return of.signal + "." + of.part
}

// signalValueSeries is the series and unit of the value and limit of each signal the writer knows
// (root R39's Design part 'The admission eval'; the daemon's record at dispatch://INFRA-5899's
// version 14, after dispatch://INFRA-3199's version 52, which leaves `provider` the part
// `headroom` alone).
var signalValueSeries = map[signalOf]struct{ series, unit string }{
	{signal: "idle"}:                       {seriesAdmissionIdle, "share"},
	{signal: "queue"}:                      {seriesAdmissionQueue, "hours"},
	{signal: "provider", part: "headroom"}: {seriesAdmissionHeadroom, "share of limit"},
	{signal: "kills"}:                      {seriesAdmissionKills, "count"},
	{signal: "approvals"}:                  {seriesAdmissionApprovals, "count"},
}

// signalSeries is the series, `col` and unit of the reading of change: a value or a limit in its
// signal's series (signalValueSeries) under the `col` `value` or `limit`, in the signal's unit; a
// hold in admission-holds under its signal's column, with no unit; the switch, an entry of no
// signal and no part, in admission-switch under the `col` "", with no unit. known is false for an
// entry whose signal, part or what the writer does not know.
func signalSeries(change legion.SignalChange) (series, col, unit string, known bool) {
	if change.What == signalSwitch {
		if change.Signal == nil && change.Part == nil {
			return seriesAdmissionSwitch, "", "", true
		}
		return "", "", "", false
	}
	if change.Signal == nil {
		return "", "", "", false
	}
	of := signalOf{signal: *change.Signal}
	if change.Part != nil {
		of.part = *change.Part
	}
	values, known := signalValueSeries[of]
	switch {
	case !known:
		return "", "", "", false
	case change.What == signalValue || change.What == signalLimit:
		return values.series, change.What, values.unit, true
	case change.What == signalHolds:
		return seriesAdmissionHolds, of.column(), "", true
	}
	return "", "", "", false
}

// signalEntry is the signal_change reading of change, an entry of the daemon's store stateID, in
// its series, `col` and unit (signalSeries), at the time the change was made: its value, or, where
// it has none, the entry's absent reason, or limitCleared for a limit a person cleared; its
// attributes who made the change and why (setBy and reason, null in an entry that names neither);
// its note the signal, what changed and the new value, and for a limit or the switch who made it and
// why. known is false for an entry signalSeries does not know, whose row key alone is set.
func signalEntry(stateID string, change legion.SignalChange) (admissionEntry, bool) {
	entry := admissionEntry{RowKind: rowKindSignalChange, RowKey: admissionRowKey(stateID, change.ID), MeasuredAt: change.At}
	series, col, unit, known := signalSeries(change)
	if !known {
		return entry, false
	}
	entry.Series, entry.Col, entry.Unit, entry.Value = series, col, unit, change.Value
	entry.Attributes = map[string]any{"set_by": change.SetBy, "reason": change.Reason}
	name := signalSwitch
	if change.Signal != nil {
		name = signalOf{signal: *change.Signal, part: textOrEmpty(change.Part)}.column() + " " + change.What
	}
	value := "none"
	switch absent := textOrEmpty(change.Absent); {
	case change.Value != nil:
		value = strconv.FormatFloat(*change.Value, 'g', -1, 64)
	case absent != "":
		entry.Absent, value = absent, "none ("+absent+")"
	case change.What == signalLimit:
		entry.Absent, value = limitCleared, "cleared"
	}
	entry.Note = name + " " + value
	if change.What == signalLimit || change.What == signalSwitch {
		if change.SetBy != nil {
			entry.Note += ", set by " + actorText(*change.SetBy)
		}
		if reason := textOrEmpty(change.Reason); reason != "" {
			entry.Note += ": " + reason
		}
	}
	return entry, true
}

// actorText is who made a change, as a note names them: the kind of actor, and the session where
// the daemon names one (`controller (session s-1)`, `operator`).
func actorText(actor legion.Actor) string {
	if actor.Session == "" {
		return actor.Kind
	}
	return actor.Kind + " (session " + actor.Session + ")"
}

// textOrEmpty is *text, or "" for nil.
func textOrEmpty(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}

// textOrNone is *text as a log line's attribute, or "none" for nil.
func textOrNone(text *string) string {
	if text == nil {
		return "none"
	}
	return *text
}

// evalHeld reports whether a version of evalID exists, read in tx.
func evalHeld(ctx context.Context, tx pgx.Tx, evalID string) (bool, error) {
	var held bool
	if err := tx.QueryRow(ctx, `select exists (select from eval_versions where eval_id = $1)`, evalID).Scan(&held); err != nil {
		return false, fmt.Errorf("read whether %s is registered: %w", evalID, err)
	}
	return held, nil
}

// heldRowKeys is each row kind and row key of entries, and of observed where it is set, that a
// reading of evalID holds already, read in tx under the eval's lock (eval_readings_row).
func heldRowKeys(ctx context.Context, tx pgx.Tx, evalID string, entries []admissionEntry, observed *admissionEntry) (map[[2]string]bool, error) {
	kinds, keys := make([]string, 0, len(entries)+1), make([]string, 0, len(entries)+1)
	for _, entry := range entries {
		kinds, keys = append(kinds, entry.RowKind), append(keys, entry.RowKey)
	}
	if observed != nil {
		kinds, keys = append(kinds, observed.RowKind), append(keys, observed.RowKey)
	}
	rows, err := tx.Query(ctx, `
		select r.row_kind, r.row_key from eval_readings r
		join unnest($2::text[], $3::text[]) as given(row_kind, row_key)
			on r.row_kind = given.row_kind and r.row_key = given.row_key
		where r.eval_id = $1
	`, evalID, kinds, keys)
	if err != nil {
		return nil, fmt.Errorf("read which readings of %s are written already: %w", evalID, err)
	}
	held := map[[2]string]bool{}
	var kind, key string
	if _, err := pgx.ForEachRow(rows, []any{&kind, &key}, func() error {
		held[[2]string{kind, key}] = true
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read which readings of %s are written already: %w", evalID, err)
	}
	return held, nil
}

// newestCap is the value of evalID's newest standing admission-cap reading, read in tx, nil for a
// reading with no value, and found false where the eval holds none.
func newestCap(ctx context.Context, tx pgx.Tx, evalID string) (*float64, bool, error) {
	var value *float64
	err := tx.QueryRow(ctx, `
		select value from eval_readings
		where eval_id = $1 and series = $2 and superseded_at is null
		order by measured_at desc, id desc limit 1
	`, evalID, seriesAdmissionCap).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the newest cap of %s: %w", evalID, err)
	}
	return value, true, nil
}
