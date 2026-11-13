package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
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
// A row's keys are a reading's own: series, row_key, row_kind, col, value, value_absent_reason,
// unit, n, n_unit, ci_low, ci_high, ci_level, attributes, links, measured_at, source, note and
// replaces. series, row_kind and measured_at are required, and value or value_absent_reason.
// A link is {rel, href, label}.
type Batch struct {
	Project string
	Slug    string
	Rows    []json.RawMessage
	Caller  model.Actor
	BuiltBy string
}

// Written is what a readings write stored: the eval's definition, whether this write registered
// it, the series it registered, and the readings, in the batch's order.
type Written struct {
	Definition Definition
	Registered bool
	Series     []Series
	Readings   []Reading
}

// WriteReadings stores one batch of readings of one eval in one transaction, or nothing. Every row
// is validated before any is written, and a refusal (*Error) names every bad row by its index
// (RowError).
//
// A slug no eval holds registers its eval as version 1 (written_via reading, kind experiment,
// source logged, no target, state active, the caller as owner), with its section in the
// project's `evals-md` document, the document created first where the project has none. Two first
// writes of one slug register it once: each takes the eval's lock (lockEval) and writes version 1
// with `on conflict do nothing`, and only the write that stored it writes the section. A series no
// version holds registers too (firstSeries), listed where the eval is logged and unlisted where it
// is computed, since a reading under a computed eval's slug is stored and answered by its items
// alone: no day row of a computed eval is written here, and nothing here folds such a reading
// into the eval's value.
//
// A correction (replaces) supersedes the reading it names in the same transaction. A logged
// eval's day rows of every day the batch touches are kept in the same transaction by increment
// (writeDays), so the write reads no stored reading but the readings it replaces and, where a
// replaced reading was its day's last, that day's newest standing reading (newestStanding).
func (s *Store) WriteReadings(ctx context.Context, batch Batch) (Written, error) {
	if !slugPattern.MatchString(batch.Slug) {
		return Written{}, refusal(CodeSlugInvalid, "slug %q is not lower-case letters, digits and hyphens starting with a letter or a digit", batch.Slug)
	}
	if batch.BuiltBy == "" {
		return Written{}, errors.New("evals: a readings write needs the running build, which its day rows record")
	}
	if len(batch.Rows) == 0 {
		return Written{}, refusal(CodeReadingsEmpty, "a readings write carries at least one reading")
	}
	evalID := EvalID(batch.Project, batch.Slug)
	rows, rowErrors := parseRows(evalID, batch.Rows)
	var written Written
	err := s.inWrite(ctx, batch.Project, func(ctx context.Context, w *writeTx) error {
		replaced, replaceErrors, err := lockReplaced(ctx, w.Tx, evalID, rows)
		if err != nil {
			return err
		}
		if all := append(rowErrors, replaceErrors...); len(all) > 0 {
			return rowsRefusal(all)
		}
		definition, registered, series, err := s.registerMissing(ctx, w, batch, rows)
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
			if _, err := w.Exec(ctx, `update eval_readings set superseded_at = now() where id = any($1::text[]::uuid[])`, ids); err != nil {
				return fmt.Errorf("supersede the readings %s corrects: %w", evalID, err)
			}
		}
		if !definition.Computed() {
			byName, err := currentSeries(ctx, w.Tx, evalID)
			if err != nil {
				return err
			}
			if err := writeDays(ctx, w, definition, byName, readings, removed, batch.BuiltBy); err != nil {
				return err
			}
		}
		written = Written{Definition: definition, Registered: registered, Series: series, Readings: readings}
		return nil
	})
	if err != nil {
		return Written{}, err
	}
	return written, nil
}

// registerMissing answers the eval's definition, registering the eval and every series of the
// batch no version holds yet, under the eval's lock, which it takes only when one is missing.
// It answers whether it registered the eval and the series it registered, in the batch's order.
func (s *Store) registerMissing(ctx context.Context, w *writeTx, batch Batch, rows []Reading) (Definition, bool, []Series, error) {
	evalID := EvalID(batch.Project, batch.Slug)
	definition, found, err := newestDefinition(ctx, w.Tx, evalID)
	if err != nil {
		return Definition{}, false, nil, err
	}
	series, err := currentSeries(ctx, w.Tx, evalID)
	if err != nil {
		return Definition{}, false, nil, err
	}
	if found && !slices.ContainsFunc(rows, func(row Reading) bool { _, has := series[row.Series]; return !has }) {
		return definition, false, nil, nil
	}
	if err := lockEval(ctx, w.Tx, evalID); err != nil {
		return Definition{}, false, nil, err
	}
	if definition, found, err = newestDefinition(ctx, w.Tx, evalID); err != nil {
		return Definition{}, false, nil, err
	}
	registered := false
	if !found {
		first := firstDefinition(batch.Project, batch.Slug, batch.Caller)
		first.Version, first.WrittenVia, first.WrittenBy = 1, ViaReading, batch.Caller.Recorded()
		first.DocBlock = uuid.NewString()
		stored, inserted, err := insertDefinition(ctx, w.Tx, first, true)
		if err != nil {
			return Definition{}, false, nil, err
		}
		if inserted {
			sentence := registrationSentence(ViaReading, batch.Caller, w.now)
			if err := s.writeMethod(ctx, w, first.Slug, "", first.DocBlock, sentence, RegistryActor); err != nil {
				return Definition{}, false, nil, err
			}
			definition, registered = stored, true
		} else if definition, _, err = newestDefinition(ctx, w.Tx, evalID); err != nil {
			return Definition{}, false, nil, err
		}
	}
	if series, err = currentSeries(ctx, w.Tx, evalID); err != nil {
		return Definition{}, false, nil, err
	}
	var added []Series
	for _, row := range rows {
		if _, has := series[row.Series]; has {
			continue
		}
		stored, inserted, err := insertSeries(ctx, w.Tx, firstSeries(evalID, row.Series, len(series) == 0, !definition.Computed(), row.Unit))
		if err != nil {
			return Definition{}, false, nil, err
		}
		if !inserted {
			return Definition{}, false, nil, fmt.Errorf("series %s of %s was registered by a write that holds no lock of the eval", row.Series, evalID)
		}
		series[stored.Series] = stored
		added = append(added, stored)
	}
	return definition, registered, added, nil
}

// lockReplaced reads, and locks for the rest of tx, every reading a row of the batch replaces,
// by id, and refuses each row whose reading is not this eval's or was replaced already: by an
// earlier correction, or by an earlier row of the same batch. The row lock is what a second
// correction of the same reading waits on, so it reads the reading the first one superseded.
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
	found, err := tx.Query(ctx, `
		select `+readingColumns+` from eval_readings where id = any($1::text[]::uuid[]) order by id for update
	`, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("read the readings %s corrects: %w", evalID, err)
	}
	stored, err := pgx.CollectRows(found, func(row pgx.CollectableRow) (Reading, error) { return scanReading(row) })
	if err != nil {
		return nil, nil, fmt.Errorf("read the readings %s corrects: %w", evalID, err)
	}
	byID := make(map[string]Reading, len(stored))
	for _, reading := range stored {
		byID[reading.ID] = reading
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

// insertReadings stores rows in one statement, so the batch logs one change row whatever its
// size (the statement-level trigger of 20261113082200), with the server's recorder and time, and
// each link stored at now, and answers them in the batch's order. Each row's id was minted when
// it was parsed.
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
	for index, row := range rows {
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
	}
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
		returning `+readingColumns,
		evalID, recordedBy, ids, series, rowKeys, rowKinds, cols, values, reasons, units, ns, nUnits, ciLows, ciHighs,
		ciLevels, attributes, links, measuredAt, sources, notes, replaces)
	if err != nil {
		return nil, fmt.Errorf("write the readings of %s: %w", evalID, err)
	}
	inserted, err := pgx.CollectRows(stored, func(row pgx.CollectableRow) (Reading, error) { return scanReading(row) })
	if err != nil {
		return nil, fmt.Errorf("write the readings of %s: %w", evalID, err)
	}
	byID := make(map[string]Reading, len(inserted))
	for _, reading := range inserted {
		byID[reading.ID] = reading
	}
	ordered := make([]Reading, len(rows))
	for index, row := range rows {
		ordered[index] = byID[row.ID]
	}
	return ordered, nil
}

// rowsRefusal is the refusal of a batch with refused rows: every one, by index, under the code
// of the first.
func rowsRefusal(refused []RowError) *Error {
	sort.SliceStable(refused, func(i, j int) bool { return refused[i].Index < refused[j].Index })
	var indexes []string
	for _, row := range refused {
		if index := fmt.Sprint(row.Index); !slices.Contains(indexes, index) {
			indexes = append(indexes, index)
		}
	}
	named := "row " + indexes[0] + " cannot be written"
	if len(indexes) > 1 {
		named = "rows " + strings.Join(indexes, ", ") + " cannot be written"
	}
	return &Error{
		Code:    refused[0].Code,
		Message: "the batch stores no reading: " + named + ", and rows says why for each",
		Rows:    refused,
	}
}

// readingKeys are the keys a row may carry, and serverSetKeys the keys of a reading the server
// sets, which a row is refused for carrying (R4).
var (
	readingKeys = []string{
		"series", "row_key", "row_kind", "col", "value", "value_absent_reason", "unit", "n", "n_unit",
		"ci_low", "ci_high", "ci_level", "attributes", "links", "measured_at", "source", "note", "replaces",
	}
	serverSetKeys = []string{"id", "eval_id", "recorded_by", "recorded_at", "superseded_at"}
	linkKeys      = []string{"rel", "href", "label"}
)

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

// parseRows decodes and checks every row of a batch, with no database read, and answers the
// readings it decoded (each with a new id) beside every refusal it found. A row whose JSON cannot
// be decoded answers its refusal and no reading worth storing.
func parseRows(evalID string, raw []json.RawMessage) ([]Reading, []RowError) {
	rows := make([]Reading, len(raw))
	var refused []RowError
	for index, row := range raw {
		reading, problems := parseRow(evalID, index, row)
		rows[index] = reading
		refused = append(refused, problems...)
	}
	return rows, refused
}

func parseRow(evalID string, index int, raw json.RawMessage) (Reading, []RowError) {
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
		ID: uuid.NewString(), EvalID: evalID, Series: body.Series, RowKey: body.RowKey, RowKind: body.RowKind,
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
	if body.MeasuredAt == nil {
		refuse(CodeReadingInvalid, "measured_at", "a reading says when it was measured in measured_at")
	} else {
		// Postgres keeps microseconds, so the reading is held at the time it is stored with, and
		// a day row's last_at compares equal to the reading it came from.
		reading.MeasuredAt = body.MeasuredAt.UTC().Truncate(time.Microsecond)
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
		var link struct {
			Rel   string `json:"rel"`
			Href  string `json:"href"`
			Label string `json:"label"`
		}
		if err := json.Unmarshal(rawLink, &link); err != nil {
			refuse(CodeReadingInvalid, field, "the link cannot be decoded: %v", err)
			continue
		}
		if code, reason := linkProblem(link.Rel, link.Href); code != "" {
			subfield := ".href"
			if code == CodeLinkRelMissing {
				subfield = ".rel"
			}
			refuse(code, field+subfield, "%s", reason)
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
