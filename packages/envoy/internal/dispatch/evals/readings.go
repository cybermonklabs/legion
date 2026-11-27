package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Batch is one readings write: the eval it logs under, by project and slug; its rows as the
// caller sent them, each one JSON object, so the validator reads every key a row carries, the
// ones the server sets included; who writes them; and BuiltBy, the running build, which the day
// rows the write keeps record (eval_days.built_by).
//
// A row's keys are a stored reading's (Reading) less those the server sets (readingKeys), and a
// link's a stored link's less its recorded_at (linkKeys). series, row_kind and measured_at are
// required, and value or value_absent_reason.
type Batch struct {
	Project string
	Slug    string
	Rows    []json.RawMessage
	Caller  model.Actor
	BuiltBy string
}

// Written is what a readings write stored: the eval's definition, whether this write registered
// it, and the readings, in the batch's order.
type Written struct {
	Definition Definition
	Registered bool
	Readings   []Reading
}

// WriteReadings stores one batch of readings of one eval in one transaction, or nothing: a refusal
// (*Error) rolls back every statement the write sent before it. The rows are judged in steps, and a
// refusal names by index (RowError) the bad rows the first step to find one finds: each row alone
// and the reading each corrects (parseRows, lockReplaced), before the write stores anything; the
// series and the versions the batch would register (registerMissing), before it registers
// anything; each reading's size (insertReadings), after the registration and before any reading
// is inserted; and the day rows the readings would leave (writeDays), once they are inserted. A
// row's measured_at lies from MeasuredAtFloor to MeasuredAtLead after the
// transaction's now(), the clock every other time the write stores is read from (R18).
//
// A slug no eval holds registers its eval as version 1 (written_via reading, kind experiment,
// source logged, no target, state active, the caller as owner), with its section in the
// project's `evals-md` document, the document created first where the project has none. Every
// write parses its rows (parseRows) and then takes the eval's lock (lockEval), before it locks the
// readings it corrects (lockReplaced) and reads the eval's definition and series, so two first
// writes of one slug register it once: the second finds the first's version 1 once it commits.
// The parse reads no database, so it runs before the lock and no other write of the eval waits on
// it. The lock comes before the readings' row locks because a row lock gives the transaction an
// id, and every reader of the event log waits for each running transaction id (events.Horizon): a
// correction that waited for the eval's lock behind a register write or another readings write
// would hold every one of them back for as long as it waited. A write that registers its eval
// takes the document's owner lock before it stores anything (lockDocument) and does the section's
// document work there, in its read phase, queued for the commit after its readings and day rows
// (writeMethod), so the section commits with the eval and the write holds a transaction id only
// for its database statements. A series no version holds registers too (firstSeries), listed
// where the eval is logged and unlisted where it is computed, since a reading under a computed
// eval's slug is stored and answered by its items alone: no day row of a computed eval is written
// here, and nothing here folds such a reading into the eval's value. A register write that lists
// or unlists a series of the eval takes the eval's lock too, so it relists the series' rows before
// this write reads the listing or after it commits, and the day rows the write keeps carry the
// listing that stands at its commit.
//
// A correction (replaces) supersedes the reading it names in the same transaction. A logged
// eval's day rows of every day the batch touches are kept in the same transaction (writeDays):
// appended to where the batch replaces no reading of their day, and folded again from the
// readings that stand where it does, so the write reads no stored reading but the readings it
// replaces and the standing readings of the days it corrects; and the zero row of every day those
// rows leave owed, up to each series' next row or through today, is written beside them
// (writeLoggedZeros), so the write's one change of the eval's day rows holds every row it owes. A
// batch that would leave a day row holding a value past what a float holds is refused
// (CodeReadingDayOverflow), and so is one that would leave a day row holding more than
// MaxDaySamples values (CodeReadingDaySamplesLimit), or an eval holding more than MaxEvalSeries
// series (CodeSeriesLimit, registerMissing), or register a version past MaxVersionBytes
// (CodeVersionTooLarge, registerMissing), or a reading too large for any statement within
// statementBytes (CodeReadingTooLarge). Every list the write sends that grows with its batch
// goes in statements of at most statementBytes, all in its transaction: its readings
// (insertReadings), the ids it corrects (lockReplaced, the supersede, foldedReadings), and the day
// rows, standing readings and series writeDays writes and reads. A refusal names at most
// MaxRefusedRows row errors and counts the rest (rowsRefusal), so its answer is bounded whatever
// the body. The same transaction folds again the stored figures of the days whose rows it moved,
// its zero rows' among them, where BuiltBy writes them (figures.go), under the eval's lock it
// holds, those days each run apart (daySpans), where they are at most inlineFoldDaySeries days and
// series (daySeries). A write past that, such as a first reading far in the past, whose zero rows
// reach every day since, folds of them only the first paint of each cut its rows reach, and marks
// those cuts stale for the refresher's refold, which folds the rest in budgeted batches
// (foldWrittenPaints): a read of the eval folds its day rows meanwhile wherever no stored row it
// would read is folded again, so every read answers what it answers after the refold.
func (s *Store) WriteReadings(ctx context.Context, batch Batch) (Written, error) {
	if !slugPattern.MatchString(batch.Slug) {
		return Written{}, refusal(CodeSlugInvalid, "slug %q is not lower-case letters, digits and hyphens starting with a letter or a digit", batch.Slug)
	}
	if reason := keyTooLong("slug", batch.Slug); reason != "" {
		return Written{}, fieldRefusal(CodeKeyTooLong, "slug", "%s", reason)
	}
	if batch.BuiltBy == "" {
		return Written{}, errors.New("evals: a readings write needs the running build, which its day rows record")
	}
	if len(batch.Rows) == 0 {
		return Written{}, refusal(CodeReadingsEmpty, "a readings write carries at least one reading")
	}
	evalID := EvalID(batch.Project, batch.Slug)
	var written Written
	err := s.inWrite(ctx, batch.Project, func(ctx context.Context, w *writeTx) error {
		rows, rowErrors := parseRows(evalID, batch.Rows, w.now)
		if err := lockEval(ctx, w.Tx, evalID); err != nil {
			return err
		}
		replaced, replaceErrors, err := lockReplaced(ctx, w.Tx, evalID, rows)
		if err != nil {
			return err
		}
		if all := append(rowErrors, replaceErrors...); len(all) > 0 {
			return rowsRefusal(all)
		}
		definition, registered, byName, err := s.registerMissing(ctx, w, batch, rows)
		if err != nil {
			return err
		}
		readings, err := insertReadings(ctx, w.Tx, evalID, rows, batch.Caller, w.now)
		if err != nil {
			return err
		}
		removed := make([]Reading, 0, len(replaced))
		for _, row := range rows {
			if row.Replaces != "" {
				removed = append(removed, replaced[row.Replaces])
			}
		}
		if len(removed) > 0 {
			ids := make([]string, len(removed))
			for index, reading := range removed {
				ids[index] = reading.ID
			}
			// The ids grow with the batch, so they go in statements of at most statementBytes;
			// lockReplaced locked every one of them already.
			for chunk := range statementChunks(ids, paramBytes(1), textBytes) {
				if _, err := w.Exec(ctx, `update eval_readings set superseded_at = now() where id = any($1::text[]::uuid[])`, chunk); err != nil {
					return fmt.Errorf("supersede the readings %s corrects: %w", evalID, err)
				}
			}
		}
		if !definition.Computed() {
			folded, err := foldedReadings(ctx, w.Tx, evalID, removed)
			if err != nil {
				return err
			}
			// spans is the days whose figures the write may have moved, each run apart: those of the
			// zero rows its day rows leave owed and of the rows a correction empties, which writeDays
			// writes and deletes, and the day of each of its readings and of those they replace;
			// repos is the repository of each row they count in, the project's own ("") among them.
			spans, repos := daySpans{}, []string{""}
			if err := writeDays(ctx, w, definition, byName, readings, folded, batch.BuiltBy, spans); err != nil {
				return err
			}
			for _, reading := range slices.Concat(readings, folded) {
				if keys, _, ok := dayKeysOf(reading, -1); ok {
					spans.add(evalID, keys[0].day, keys[0].day)
					for _, key := range keys[1:] {
						repos = append(repos, key.repo)
					}
				}
			}
			if len(spans) > 0 {
				// The stored figures of the days moved, folded under the definition and the series this
				// write read under the eval's lock (registerMissing), which a read of them again
				// (foldFiguresInTx) would answer alike; their order reaches no figure, since
				// foldKeyOf and encodeFigures read the series by name.
				scope, ok, err := readFigureScope(ctx, w.Tx, batch.BuiltBy, batch.Project)
				if err != nil {
					return err
				}
				entry := Entry{Definition: definition, Series: slices.Collect(maps.Values(byName))}
				switch {
				case !ok:
				case daySeries(spans, entry, scope.through) > inlineFoldDaySeries:
					slices.Sort(repos)
					if err := foldWrittenPaints(ctx, w.Tx, scope, entry, slices.Compact(repos)); err != nil {
						return err
					}
				default:
					if err := refoldFigures(ctx, w.Tx, scope, map[string]Entry{evalID: entry}, spans.sets()...); err != nil {
						return err
					}
				}
			}
		}
		written = Written{Definition: definition, Registered: registered, Readings: readings}
		return nil
	})
	if err != nil {
		return Written{}, err
	}
	return written, nil
}

// inlineFoldDaySeries is the most day and series pairs (daySeries) whose stored figures a readings
// write folds in its own transaction, under the eval's lock and with a transaction id every reader
// of the event log waits for (events.Horizon); a write whose days pass it folds the first paint of
// each cut it writes and leaves the rest to the refresher's refold, in budgeted batches
// (foldWrittenPaints). It holds that fold to about refoldBatchBudget's 250 ms at the largest eval
// the routes take (MaxEvalSeries), whose fold costs the most for each day and series: on the build
// farm a first reading of each of 234 series folded 1, 7, 30 and 120 days in 8, 41, 342 and 2,398
// ms (49 µs a day and series at 30 days, 85 at 120), and one of 30 series 120 and 365 days in 70
// and 204 ms (19 µs); so 5,000 pairs are 21 days of 234 series, and 166 of 30. The most a write
// may owe, a first reading of each of 234 series on MeasuredAtFloor, is about 162,000 pairs.
const inlineFoldDaySeries = 5000

// daySeries is how many day and series pairs of entry's eval a fold of its stored figures over
// spans folds: each day of its runs before through, the last end day the figures are folded
// through (figureScope), past which no figure is written, times the eval's listed series, each of
// which every range and day of it folds.
func daySeries(spans daySpans, entry Entry, through time.Time) int {
	listed, _ := listedAndHeadline(entry)
	days := 0
	for _, run := range spans[entry.Definition.EvalID] {
		end := run.last.AddDate(0, 0, 1)
		if end.After(through) {
			end = through
		}
		if end.After(run.first) {
			days += rangeDays(Range{From: run.first, To: end})
		}
	}
	return days * len(listed)
}

// foldedReadings is each of replaced, readings of the eval evalID, that its day rows hold: one
// written while the eval was logged (writtenWhileLogged). A reading written while the eval was
// computed was stored and never folded (WriteReadings), so a correction of it after a register
// write made the eval logged appends to its day rows as a write that replaces nothing does. The
// ids it reads by grow with the batch, so they go in statements of at most statementBytes.
func foldedReadings(ctx context.Context, tx pgx.Tx, evalID string, replaced []Reading) ([]Reading, error) {
	if len(replaced) == 0 {
		return nil, nil
	}
	ids := make([]string, len(replaced))
	for index, reading := range replaced {
		ids[index] = reading.ID
	}
	held := make(map[string]bool, len(ids))
	for chunk := range statementChunks(ids, paramBytes(1, evalID), textBytes) {
		rows, err := tx.Query(ctx, `
			with `+loggedSpans+`
			select r.id::text from eval_readings r
			where r.eval_id = $1 and r.id = any($2::text[]::uuid[]) and `+writtenWhileLogged, evalID, chunk)
		if err != nil {
			return nil, fmt.Errorf("read which corrected readings were folded: %w", err)
		}
		var id string
		if _, err := pgx.ForEachRow(rows, []any{&id}, func() error {
			held[id] = true
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read which corrected readings were folded: %w", err)
		}
	}
	return slices.DeleteFunc(slices.Clone(replaced), func(reading Reading) bool { return !held[reading.ID] }), nil
}

// registerMissing answers the eval's definition, registering the eval and every series of the
// batch no version holds yet. It runs under the eval's lock (lockEval), which WriteReadings takes
// first, before it reads either, whether or not anything is missing, as the register write does:
// so the listing of each series it answers is the one the last register write of the eval
// committed, and no register write lists or unlists a series of it until this write commits, the
// listing writeDays writes into the day rows among what it waits for (relistDays). It answers
// whether it registered the eval, and the newest version of every series of the eval by name,
// those it registered among them. A batch whose new series would leave the eval holding more than
// MaxEvalSeries series is refused, naming the first row whose series passes the bound and how many
// the eval would hold (CodeSeriesLimit), counted under that lock, before the write registers
// anything: one row, so the refusal of a batch of thousands of new series stays a short answer. A
// computed eval is held to the bound as a logged one is: MaxEvalSeries bounds the series an eval
// holds, listed or not, and each series a reading registers under a computed eval's slug is one of
// them, unlisted. Each version it would register is judged against MaxVersionBytes before it stores
// any, as a register write's is: the eval's, whose writer is the caller (written_by, from the
// body's actor for a bearer caller), refused naming `actor`, and each new series', whose unit is
// the first row's of it, refused at that row naming `unit` (CodeVersionTooLarge). An eval it
// registers gets its section first, before its version's insert: the write takes the document's
// owner lock (lockDocument) and queues the section's writes for the commit (writeMethod), so that
// document work runs before the write's first statement that writes.
func (s *Store) registerMissing(ctx context.Context, w *writeTx, batch Batch, rows []Reading) (Definition, bool, map[string]Series, error) {
	evalID := EvalID(batch.Project, batch.Slug)
	definition, found, err := newestDefinition(ctx, w.Tx, evalID)
	if err != nil {
		return Definition{}, false, nil, err
	}
	series, err := currentSeries(ctx, w.Tx, evalID)
	if err != nil {
		return Definition{}, false, nil, err
	}
	if index, count, total := seriesPastLimit(series, len(rows), func(index int) string { return rows[index].Series }); index >= 0 {
		return Definition{}, false, nil, rowsRefusal([]RowError{{Index: index, Code: CodeSeriesLimit, Field: "series",
			Message: seriesLimitReason(evalID, rows[index].Series, count, total)}})
	}
	if !found {
		definition = firstDefinition(batch.Project, batch.Slug, batch.Caller)
		definition.Version, definition.WrittenVia, definition.WrittenBy = 1, ViaReading, batch.Caller.Recorded()
		definition.DocBlock, definition.CreatedAt = uuid.NewString(), w.now
		if err := definitionTooLarge(definition); err != nil {
			return Definition{}, false, nil, err
		}
	}
	// The first version of each series no version holds, in the batch's order, each judged
	// before any is stored; series holds it from then on, and the stored version once it is.
	var added []Series
	var refused []RowError
	for index, row := range rows {
		if _, has := series[row.Series]; has {
			continue
		}
		first := firstSeries(evalID, row.Series, len(series) == 0, !definition.Computed(), row.Unit)
		first.CreatedAt = w.now
		size, field, err := seriesVersionSize(first, "")
		if err != nil {
			return Definition{}, false, nil, err
		}
		if field != "" {
			refused = append(refused, RowError{Index: index, Code: CodeVersionTooLarge, Field: field, Message: versionTooLargeReason(seriesVersionName(first), size, field)})
		}
		series[first.Series] = first
		added = append(added, first)
	}
	if len(refused) > 0 {
		return Definition{}, false, nil, rowsRefusal(refused)
	}
	registered := false
	if !found {
		document, documentFound, err := lockDocument(ctx, w)
		if err != nil {
			return Definition{}, false, nil, err
		}
		sentence := registrationSentence(ViaReading, batch.Caller, w.now)
		if err := s.writeMethod(ctx, w, document, documentFound, definition.Slug, "", definition.DocBlock, sentence, RegistryActor); err != nil {
			return Definition{}, false, nil, err
		}
		stored, err := insertDefinition(ctx, w.Tx, definition)
		if err != nil {
			return Definition{}, false, nil, err
		}
		definition, registered = stored, true
	}
	for _, first := range added {
		stored, inserted, err := insertSeries(ctx, w.Tx, first)
		if err != nil {
			return Definition{}, false, nil, err
		}
		if !inserted {
			return Definition{}, false, nil, fmt.Errorf("series %s of %s was registered by a write that holds no lock of the eval", first.Series, evalID)
		}
		series[stored.Series] = stored
	}
	return definition, registered, series, nil
}

// lockReplaced reads, and locks for the rest of tx, every reading a row of the batch replaces,
// by id, and refuses each row whose reading is not this eval's or was replaced already: by an
// earlier correction, or by an earlier row of the same batch. The row lock is what a second
// correction of the same reading waits on, so it reads the reading the first one superseded. It
// runs under the eval's lock, which WriteReadings takes first: a row lock gives the transaction
// an id, which the write must not hold while it waits for that lock (inWrite). The ids grow with
// the batch, so they go sorted and each once in statements of at most statementBytes, each
// locking its own in id order: the statements lock the readings in the order one statement does.
func lockReplaced(ctx context.Context, tx pgx.Tx, evalID string, rows []Reading) (map[string]Reading, []RowError, error) {
	var ids []string
	for _, row := range rows {
		if row.Replaces != "" {
			ids = append(ids, row.Replaces)
		}
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	// A replaced id is a parsed uuid's canonical text, whose bytes sort as the uuids do.
	slices.Sort(ids)
	ids = slices.Compact(ids)
	byID := make(map[string]Reading, len(ids))
	for chunk := range statementChunks(ids, paramBytes(1), textBytes) {
		found, err := tx.Query(ctx, `
			select `+ReadingColumns+` from eval_readings where id = any($1::text[]::uuid[]) order by id for update
		`, chunk)
		if err != nil {
			return nil, nil, fmt.Errorf("read the readings %s corrects: %w", evalID, err)
		}
		stored, err := pgx.CollectRows(found, func(row pgx.CollectableRow) (Reading, error) { return scanReading(row) })
		if err != nil {
			return nil, nil, fmt.Errorf("read the readings %s corrects: %w", evalID, err)
		}
		for _, reading := range stored {
			byID[reading.ID] = reading
		}
	}
	var refused []RowError
	replacedBy := map[string]int{}
	for index, row := range rows {
		if row.Replaces == "" {
			continue
		}
		reading, has := byID[row.Replaces]
		earlier, taken := replacedBy[row.Replaces]
		switch {
		case !has || reading.EvalID != evalID:
			refused = append(refused, RowError{Index: index, Code: CodeReplacesUnknown, Field: "replaces",
				Message: fmt.Sprintf("no reading of %s has the id %s", evalID, row.Replaces)})
		case reading.SupersededAt != nil:
			refused = append(refused, RowError{Index: index, Code: CodeAlreadyReplaced, Field: "replaces",
				Message: fmt.Sprintf("reading %s was replaced at %s; correct the reading that replaced it", row.Replaces, reading.SupersededAt.Format(time.RFC3339))})
		case taken:
			refused = append(refused, RowError{Index: index, Code: CodeAlreadyReplaced, Field: "replaces",
				Message: fmt.Sprintf("row %d of this batch replaces reading %s already", earlier, row.Replaces)})
		default:
			replacedBy[row.Replaces] = index
		}
	}
	return byID, refused, nil
}

// textBytes is the most bytes text adds to an array of texts as pgx encodes it in binary: 4 bytes
// of length and its bytes. Each list of ids a readings write sends grows with its batch, and is cut
// by it into statements of at most statementBytes (statementChunks).
func textBytes(text *string) int {
	return 4 + len(*text)
}

// readingArrays is how many arrays insertReadings' statement takes, one for each column of a
// reading it is given, beside its two scalars (the eval and the recorder).
const readingArrays = 19

// readingBytes is the bytes row adds to the parameters of insertReadings' statement, as pgx encodes
// them in binary, attributes and links being the JSON it stores of them: in each of the
// statement's arrays 4 bytes of length and the element, each text its bytes (the id a correction
// replaces among them, none for a reading that replaces none), measured_at 8, and each of its
// value, n, ci_low, ci_high and ci_level 8 where the row holds it and nothing where it is null,
// which pgx sends as its length alone. It is what the statement sends of the row, never less, so
// a run of rows statementChunks cuts at statementBytes is sent within it, and no more, so a batch
// whose readings fit one statement is sent in one.
func readingBytes(row *Reading, attributes, links string) int {
	return readingArrays*4 + len(row.ID) + len(row.Series) + len(row.RowKey) + len(row.RowKind) + len(row.Col) + len(row.ValueAbsentReason) +
		len(row.Unit) + len(row.NUnit) + len(attributes) + len(links) + len(row.Source) + len(row.Note) + len(row.Replaces) + 8 +
		numberBytes(row.Value) + numberBytes(row.N) + numberBytes(row.CILow) + numberBytes(row.CIHigh) + numberBytes(row.CILevel)
}

// numberBytes is what a number of a reading (value, n, ci_low, ci_high, ci_level) adds to its
// array beside its length: 8 bytes where the reading holds it, none where it is null.
func numberBytes[T float64 | int64](number *T) int {
	if number == nil {
		return 0
	}
	return 8
}

// largestText is the field of row, as a row spells it, whose text takes the most bytes as the
// write sends it: the field a refusal of a reading too large to store names.
func largestText(row *Reading, attributes, links string) string {
	field, largest := "", -1
	for _, text := range []struct {
		field string
		bytes int
	}{
		{"note", len(row.Note)}, {"attributes", len(attributes)}, {"links", len(links)}, {"source", len(row.Source)},
		{"value_absent_reason", len(row.ValueAbsentReason)}, {"col", len(row.Col)}, {"unit", len(row.Unit)}, {"n_unit", len(row.NUnit)},
	} {
		if text.bytes > largest {
			field, largest = text.field, text.bytes
		}
	}
	return field
}

// insertReadings stores rows, with the server's recorder and time and each link stored at now, and
// answers them in the batch's order. Each row's id was minted when it was parsed. The rows go in
// the batch's order in statements whose parameters take at most statementBytes as
// statementChunks cuts them (the statement's own estimated by paramBytes and each row's by
// readingBytes), all in tx; each statement widens the one eval_readings change the transaction
// logs of the eval (the statement-level trigger of 20261117100012), so the batch logs one change
// row whatever its size. A reading whose row alone would pass the bound, a note or attributes of
// some hundred kilobytes in a body of up to a megabyte, is refused, naming its largest field
// (CodeReadingTooLarge), before the first of these statements is sent: no statement within the
// bound could store it.
func insertReadings(ctx context.Context, tx pgx.Tx, evalID string, rows []Reading, caller model.Actor, now time.Time) ([]Reading, error) {
	recordedBy, err := json.Marshal(caller.Recorded())
	if err != nil {
		return nil, fmt.Errorf("encode the recorder: %w", err)
	}
	count := len(rows)
	ids, series, rowKeys, rowKinds, cols := make([]string, count), make([]string, count), make([]string, count), make([]string, count), make([]string, count)
	values, ciLows, ciHighs, ciLevels := make([]*float64, count), make([]*float64, count), make([]*float64, count), make([]*float64, count)
	reasons, units, nUnits, sources, notes := make([]string, count), make([]string, count), make([]string, count), make([]string, count), make([]string, count)
	ns := make([]*int64, count)
	attributes, links := make([]string, count), make([]string, count)
	measuredAt := make([]time.Time, count)
	replaces := make([]*string, count)
	sizes := make([]int, count)
	// base is the statement's parameters with no row: its arrays, the eval and the recorder.
	base := paramBytes(readingArrays, evalID) + 4 + len(recordedBy)
	var refused []RowError
	for index := range rows {
		row := &rows[index]
		ids[index], series[index], rowKeys[index], rowKinds[index], cols[index] = row.ID, row.Series, row.RowKey, row.RowKind, row.Col
		values[index], ciLows[index], ciHighs[index], ciLevels[index] = row.Value, row.CILow, row.CIHigh, row.CILevel
		reasons[index], units[index], nUnits[index], sources[index], notes[index] = row.ValueAbsentReason, row.Unit, row.NUnit, row.Source, row.Note
		ns[index], measuredAt[index] = row.N, row.MeasuredAt
		encodedAttributes, err := json.Marshal(row.Attributes)
		if err != nil {
			return nil, fmt.Errorf("encode the attributes of row %d: %w", index, err)
		}
		for linkIndex := range row.Links {
			row.Links[linkIndex].RecordedAt = now
		}
		encodedLinks, err := json.Marshal(nonNil(row.Links))
		if err != nil {
			return nil, fmt.Errorf("encode the links of row %d: %w", index, err)
		}
		attributes[index], links[index] = string(encodedAttributes), string(encodedLinks)
		if row.Replaces != "" {
			replaces[index] = &row.Replaces
		}
		if sizes[index] = readingBytes(row, attributes[index], links[index]); base+sizes[index] > statementBytes {
			refused = append(refused, RowError{Index: index, Code: CodeReadingTooLarge, Field: largestText(row, attributes[index], links[index]), Message: fmt.Sprintf(
				"the reading takes %d bytes as the write sends it, and the parameters of one statement of this write take at most %d beside the eval and the recorder it names (%d in all), so no statement could store it",
				sizes[index], statementBytes-base, statementBytes)})
		}
	}
	if len(refused) > 0 {
		return nil, rowsRefusal(refused)
	}
	byID := make(map[string]Reading, count)
	from := 0
	for run := range statementChunks(sizes, base, func(size *int) int { return *size }) {
		to := from + len(run)
		stored, err := tx.Query(ctx, `
			insert into eval_readings (id, eval_id, series, row_key, row_kind, col, value, value_absent_reason, unit, n,
				n_unit, ci_low, ci_high, ci_level, attributes, links, measured_at, recorded_by, source, note, replaces)
			select r.id::uuid, $1, r.series, r.row_key, r.row_kind, r.col, r.value, r.reason, r.unit, r.n, r.n_unit,
				r.ci_low, r.ci_high, r.ci_level, r.attributes::jsonb, r.links::jsonb, r.measured_at, $2::jsonb, r.source,
				r.note, r.replaces::uuid
			from unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::float8[], $9::text[], $10::text[],
				$11::int8[], $12::text[], $13::float8[], $14::float8[], $15::float8[], $16::text[], $17::text[],
				$18::timestamptz[], $19::text[], $20::text[], $21::text[])
				as r(id, series, row_key, row_kind, col, value, reason, unit, n, n_unit, ci_low, ci_high, ci_level,
					attributes, links, measured_at, source, note, replaces)
			returning `+ReadingColumns,
			evalID, recordedBy, ids[from:to], series[from:to], rowKeys[from:to], rowKinds[from:to], cols[from:to], values[from:to],
			reasons[from:to], units[from:to], ns[from:to], nUnits[from:to], ciLows[from:to], ciHighs[from:to], ciLevels[from:to],
			attributes[from:to], links[from:to], measuredAt[from:to], sources[from:to], notes[from:to], replaces[from:to])
		if err != nil {
			return nil, fmt.Errorf("write the readings of %s: %w", evalID, err)
		}
		inserted, err := pgx.CollectRows(stored, func(row pgx.CollectableRow) (Reading, error) { return scanReading(row) })
		if err != nil {
			return nil, fmt.Errorf("write the readings of %s: %w", evalID, err)
		}
		for _, reading := range inserted {
			byID[reading.ID] = reading
		}
		from = to
	}
	ordered := make([]Reading, len(rows))
	for index, row := range rows {
		ordered[index] = byID[row.ID]
	}
	return ordered, nil
}

// rowsRefusal is the refusal of a batch with refused rows, under the code of the first by index:
// the first MaxRefusedRows row errors by index, each text bounded (fieldTextBytes,
// errorTextBytes), and how many more there are (RowsOmitted), so the refusal of a megabyte of bad
// rows is an answer of at most MaxAnswerBytes.
func rowsRefusal(refused []RowError) *Error {
	sort.SliceStable(refused, func(i, j int) bool { return refused[i].Index < refused[j].Index })
	var indexes []string
	rows := 0
	for at, row := range refused {
		if at > 0 && row.Index == refused[at-1].Index {
			continue
		}
		if rows++; rows <= 20 {
			indexes = append(indexes, strconv.Itoa(row.Index))
		}
	}
	listed := refused[:min(len(refused), MaxRefusedRows)]
	for at := range listed {
		listed[at].Field, listed[at].Message = boundText(listed[at].Field, fieldTextBytes), boundText(listed[at].Message, errorTextBytes)
	}
	var named string
	switch {
	case rows == 1:
		named = "row " + indexes[0] + " cannot be written"
	case rows <= len(indexes):
		named = "rows " + strings.Join(indexes, ", ") + " cannot be written"
	default:
		named = fmt.Sprintf("%d rows cannot be written, the first row %s", rows, indexes[0])
	}
	message := "the batch stores no reading: " + named + ", and rows says why for each"
	omitted := len(refused) - len(listed)
	if omitted > 0 {
		message = fmt.Sprintf("the batch stores no reading: %s; rows says why for the first %d refusals by row, and rows_omitted counts the other %d",
			named, len(listed), omitted)
	}
	return &Error{Code: refused[0].Code, Message: boundText(message, errorTextBytes), Rows: listed, RowsOmitted: omitted}
}

// readingKeys are the keys a row may carry and linkKeys those of each of its links, the json tags
// of the types a row and a link are decoded into (readingBody, readingLink), read once when the
// package starts, so the keys a row is held to are the keys it is decoded by; serverSetKeys are
// the keys of a reading the server sets, which a row is refused for carrying (R4). A row takes
// every key of a stored reading (Reading) but those, and a link every key of a stored link (Link)
// but its recorded_at, which TestARowTakesTheKeysOfAStoredReadingButThoseTheServerSets holds: the
// readings route documents a row and a link as the stored reading and link less the same keys.
var (
	readingKeys   = jsonKeysOf(reflect.TypeFor[readingBody]())
	linkKeys      = jsonKeysOf(reflect.TypeFor[readingLink]())
	serverSetKeys = []string{"id", "eval_id", "recorded_by", "recorded_at", "superseded_at"}
)

// jsonKeysOf is the keys a JSON object of shape, a struct whose every field names its key, takes:
// each field's json name.
func jsonKeysOf(shape reflect.Type) []string {
	keys := make([]string, 0, shape.NumField())
	for field := range shape.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		keys = append(keys, name)
	}
	return keys
}

// readingBody is the decoding of one row's keys.
type readingBody struct {
	Series            string            `json:"series"`
	RowKey            string            `json:"row_key"`
	RowKind           string            `json:"row_kind"`
	Col               string            `json:"col"`
	Value             *float64          `json:"value"`
	ValueAbsentReason string            `json:"value_absent_reason"`
	Unit              string            `json:"unit"`
	N                 *int64            `json:"n"`
	NUnit             string            `json:"n_unit"`
	CILow             *float64          `json:"ci_low"`
	CIHigh            *float64          `json:"ci_high"`
	CILevel           *float64          `json:"ci_level"`
	Attributes        map[string]any    `json:"attributes"`
	Links             []json.RawMessage `json:"links"`
	MeasuredAt        *time.Time        `json:"measured_at"`
	Source            string            `json:"source"`
	Note              string            `json:"note"`
	Replaces          *string           `json:"replaces"`
}

// readingLink is the decoding of one link of a row.
type readingLink struct {
	Rel   string `json:"rel"`
	Href  string `json:"href"`
	Label string `json:"label"`
}

// parseRows decodes and checks every row of a batch, with no database read, and answers the
// readings it decoded beside every refusal it found. now is the write's time, which bounds each
// row's measured_at. A row whose JSON cannot be decoded answers its refusal and no reading worth
// storing. Each reading's id is a new UUIDv7, minted in the batch's order, so the ids of one
// batch sort as its rows do: the order a day row's refold reads the readings of one write in
// (standingParts), which is the order its append took them in.
func parseRows(evalID string, raw []json.RawMessage, now time.Time) ([]Reading, []RowError) {
	rows := make([]Reading, len(raw))
	var refused []RowError
	for index, row := range raw {
		reading, problems := parseRow(evalID, index, row, now)
		rows[index] = reading
		refused = append(refused, problems...)
	}
	return rows, refused
}

func parseRow(evalID string, index int, raw json.RawMessage, now time.Time) (Reading, []RowError) {
	var refused []RowError
	refuse := func(code, field, format string, args ...any) {
		refused = append(refused, RowError{Index: index, Code: code, Field: field, Message: fmt.Sprintf(format, args...)})
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil || keys == nil {
		refuse(CodeReadingInvalid, "", "a reading is a JSON object")
		return Reading{}, refused
	}
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		switch {
		case slices.Contains(serverSetKeys, key):
			refuse(CodeFieldServerSet, key, "%s is set by the server, and a reading carries none", key)
		case !slices.Contains(readingKeys, key):
			refuse(CodeReadingInvalid, key, "no reading has the key %q", key)
		}
	}
	var body readingBody
	if err := json.Unmarshal(raw, &body); err != nil {
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &typeError) {
			refuse(CodeReadingInvalid, typeError.Field, "%s holds a JSON %s, not a %s", typeError.Field, typeError.Value, typeError.Type)
		} else {
			refuse(CodeReadingInvalid, "", "the reading cannot be decoded: %v", err)
		}
		return Reading{}, refused
	}
	reading := Reading{
		ID: uuid.Must(uuid.NewV7()).String(), EvalID: evalID, Series: body.Series, RowKey: body.RowKey, RowKind: body.RowKind,
		Col: body.Col, Value: body.Value, ValueAbsentReason: body.ValueAbsentReason, Unit: body.Unit, N: body.N,
		NUnit: body.NUnit, CILow: body.CILow, CIHigh: body.CIHigh, CILevel: body.CILevel,
		Attributes: body.Attributes, Source: body.Source, Note: body.Note, Links: []Link{},
	}
	if reading.Attributes == nil {
		reading.Attributes = map[string]any{}
	}
	if err := validSeriesName(body.Series); err != nil {
		refuse(CodeReadingSeriesInvalid, "series", "series %q: %v", body.Series, err)
	}
	if body.RowKind == "" {
		refuse(CodeReadingInvalid, "row_kind", "a reading names its row kind in row_kind")
	}
	for _, key := range []struct{ field, value string }{{"series", body.Series}, {"row_kind", body.RowKind}, {"row_key", body.RowKey}} {
		if reason := keyTooLong(key.field, key.value); reason != "" {
			refuse(CodeKeyTooLong, key.field, "%s", reason)
		}
	}
	if body.MeasuredAt == nil {
		refuse(CodeReadingInvalid, "measured_at", "a reading says when it was measured in measured_at")
	} else {
		// Postgres keeps microseconds, so the reading is held at the time it is stored with, and
		// a day row's last_at compares equal to the reading it came from.
		reading.MeasuredAt = body.MeasuredAt.UTC().Truncate(time.Microsecond)
		if ceiling := now.Add(MeasuredAtLead); reading.MeasuredAt.Before(MeasuredAtFloor) || reading.MeasuredAt.After(ceiling) {
			refuse(CodeMeasuredAtOutOfRange, "measured_at", "measured_at %s is outside the range a reading may hold, %s to %s, %g hours after the server's time",
				body.MeasuredAt.Format(time.RFC3339Nano), MeasuredAtFloor.Format(time.RFC3339), ceiling.Format(time.RFC3339), MeasuredAtLead.Hours())
		}
	}
	if body.Value == nil && strings.TrimSpace(body.ValueAbsentReason) == "" {
		refuse(CodeValueOrReason, "value", "a reading has a value, or says in value_absent_reason why it has none")
	}
	if body.N != nil && *body.N < 0 {
		refuse(CodeReadingInvalid, "n", "n is how many items the value was measured over, and %d is below 0", *body.N)
	}
	if repo, has := reading.Attributes["repo"]; has {
		if name, ok := repo.(string); !ok || name == "" {
			refuse(CodeReadingInvalid, "attributes.repo", "attributes.repo names a repository, and %v is no repository's name", repo)
		} else if reason := keyTooLong("attributes.repo", name); reason != "" {
			refuse(CodeKeyTooLong, "attributes.repo", "%s", reason)
		}
	}
	for linkIndex, rawLink := range body.Links {
		field := fmt.Sprintf("links[%d]", linkIndex)
		var linkKeysFound map[string]json.RawMessage
		if err := json.Unmarshal(rawLink, &linkKeysFound); err != nil || linkKeysFound == nil {
			refuse(CodeReadingInvalid, field, "a link is a JSON object")
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(linkKeysFound)) {
			switch {
			case key == "recorded_at":
				refuse(CodeFieldServerSet, field+".recorded_at", "a link's recorded_at is set by the server, and a link carries none")
			case !slices.Contains(linkKeys, key):
				refuse(CodeReadingInvalid, field+"."+key, "no link has the key %q", key)
			}
		}
		var link readingLink
		if err := json.Unmarshal(rawLink, &link); err != nil {
			refuse(CodeReadingInvalid, field, "the link cannot be decoded: %v", err)
			continue
		}
		if code, reason := linkProblem(link.Rel, link.Href); code != "" {
			refuse(code, field+"."+linkField(code), "%s", reason)
		}
		reading.Links = append(reading.Links, Link{Rel: link.Rel, Href: link.Href, Label: link.Label})
	}
	if body.Replaces != nil {
		if parsed, err := uuid.Parse(*body.Replaces); err != nil {
			refuse(CodeReplacesUnknown, "replaces", "replaces names a reading by its id, and %q is no reading's id", *body.Replaces)
		} else {
			reading.Replaces = parsed.String()
		}
	}
	return reading, refused
}
