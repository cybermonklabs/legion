// Package evals is the eval registry's store (dispatch://INFRA-2916, R1 to R5, R9, R12, R15 and
// R16): every write and read of eval_versions, eval_series and eval_readings, of the day rows
// (eval_days) and their members (eval_day_members), and of each project's `evals-md` document,
// which holds one section per eval with the eval's method in it.
//
// An eval is one number a project's pages show. Its definition is append-only: each change
// writes the next version (Register), and a reader reads the newest (Current). Its readings are
// append-only too: a correction is a new reading naming the one it replaces (WriteReadings). A
// logged eval's day rows are kept by the readings write in its own transaction (days.go), so no
// window read reads a reading (DayWindow); a computed eval's day rows are the refresher's, which
// builds the delivery and CI evals' from one day's delivery population (delivery_days.go) and the
// Legion evals' from one day's stored Legion facts and filings (legion_days.go), and a reading
// logged under a computed eval's slug is stored and answered by its items alone. Each row records
// which of the two wrote it, and an eval's reads read its own writer's rows alone.
//
// The package serves no request (the routes are api's) and makes none: a link is stored as its
// caller wrote it and its far side is never fetched, so the package's own files import no HTTP
// client (imports_test.go reads their import lists; packages they import, such as delivery for
// its GitHub reads, do import one).
package evals

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The values the registry's enumerated columns hold, each the set the column's check in
// 20261117100005_eval_registry.up.sql allows.
const (
	KindDelivery   = "delivery"
	KindLegion     = "legion"
	KindCI         = "ci"
	KindExperiment = "experiment"

	// SourceLogged is the source of an eval whose values are logged readings; a computed eval's
	// source is SourceComputedPrefix followed by the one function that computes it.
	SourceLogged         = "logged"
	SourceComputedPrefix = "computed:"

	StateActive   = "active"
	StateStopped  = "stopped"
	StateReplaced = "replaced"

	DirectionHigherBetter = "higher_better"
	DirectionLowerBetter  = "lower_better"

	// ViaSeed, ViaRegister, ViaReading and ViaSettings say how a version was written: by the
	// seeded catalogue, by the register write, by a first reading under a slug no eval held,
	// and by a settings write, which the column's check allows and no write of this package makes.
	ViaSeed     = "seed"
	ViaRegister = "register"
	ViaReading  = "reading"
	ViaSettings = "settings"

	RoleHeadline = "headline"
	RoleControl  = "control"

	AggSum          = "sum"
	AggRatio        = "ratio"
	AggQuantile     = "quantile"
	AggMax          = "max"
	AggLast         = "last"
	AggWeightedMean = "weighted_mean"
)

// The sources of the seeded catalogue's computed evals: SourceComputedPrefix and the one function
// each folds through. The catalogue names them, computedSources is keyed by them, and api's items
// route answers each one's rows, so a source is spelled once.
const (
	SourceFoldDeployFrequency   = SourceComputedPrefix + "delivery/measures.FoldDeployFrequency"
	SourceFoldDeployRunSuccess  = SourceComputedPrefix + "delivery/measures.FoldDeployRunSuccess"
	SourceFoldLeadTime          = SourceComputedPrefix + "delivery/measures.FoldLeadTime"
	SourceFoldReworkShare       = SourceComputedPrefix + "delivery/measures.FoldReworkShare"
	SourceFoldChangeFailureRate = SourceComputedPrefix + "delivery/measures.FoldChangeFailureRate"
	SourceFoldTimeToRestore     = SourceComputedPrefix + "delivery/measures.FoldTimeToRestore"
	SourceFoldTiming            = SourceComputedPrefix + "delivery/measures.FoldTiming"
	SourceListUnownedP0         = SourceComputedPrefix + "delivery.ListUnownedP0"
	SourceFoldWorkTime          = SourceComputedPrefix + "legion/metrics.FoldWorkTime"
	SourceFoldPhaseTime         = SourceComputedPrefix + "legion/metrics.FoldPhaseTime"
	SourceFoldIdleTime          = SourceComputedPrefix + "legion/metrics.FoldIdleTime"
	SourceFoldSlotWait          = SourceComputedPrefix + "legion/metrics.FoldSlotWait"
	SourceFoldRework            = SourceComputedPrefix + "legion/metrics.FoldRework"
	SourceFoldSplitting         = SourceComputedPrefix + "legion/metrics.FoldSplitting"
)

// RegistryActor is who the server's own writes to `evals-md` are by: the section a registration
// writes when its caller sends no method text, and the document's creation where that section is
// its first.
var RegistryActor = model.Actor{Kind: "system", ID: "eval-registry"}

// Link is one link to a result outside Dispatch (R9): its relation, as the IANA registry names
// one where one fits (`via`, `describedby`, `enclosure`) or Dispatch's own `source-rule`, any
// other relation stored as written; its absolute address; its label; and when the server stored
// it, which the server sets.
type Link struct {
	Rel        string    `json:"rel"`
	Href       string    `json:"href"`
	Label      string    `json:"label"`
	RecordedAt time.Time `json:"recorded_at"`
}

// FailureMode is one way an eval's number can mislead, and what guards against it.
type FailureMode struct {
	Mode  string `json:"mode"`
	Guard string `json:"guard"`
}

// Definition is one version of an eval's definition, every column of eval_versions. A nullable
// text column is "" where it is null: Direction is "" until someone registers one, and IssueKey
// and ReplacedBy are "" where they name nothing. Target is nil where the target is off. Its
// Computation is the fields that say how the number is computed (R5), which the seeded catalogue
// states of a computed eval; every other field is the version's own number, time and writer, one
// a person sets, or the record of which of those a person has set (SetByPerson).
type Definition struct {
	EvalID     string `json:"eval_id"`
	Version    int    `json:"version"`
	ProjectKey string `json:"project_key"`
	Slug       string `json:"slug"`
	Computation
	DocBlock        string      `json:"doc_block"`
	Target          *float64    `json:"target"`
	TargetInclusive bool        `json:"target_inclusive"`
	TargetJudges    []string    `json:"target_judges"`
	OwnerRole       string      `json:"owner_role"`
	Person          string      `json:"person"`
	IssueKey        string      `json:"issue_key"`
	State           string      `json:"state"`
	ReplacedBy      string      `json:"replaced_by"`
	Links           []Link      `json:"links"`
	WrittenVia      string      `json:"written_via"`
	WrittenBy       model.Actor `json:"written_by"`
	CreatedAt       time.Time   `json:"created_at"`
	// SetByPerson is every field of the eval a person's register write has named, sorted and
	// each once, as a body spells it (Registration.personFields): the record that the field is a
	// person's choice, written whether or not the write moved its value, and carried forward by
	// every version after it, whoever writes it. The seed fills the catalogue's target, a
	// series' min_n and a method only on an eval whose record holds none of them
	// (takeCatalogueFields), so a person who sets a target to none while it is already none is
	// not overridden by the next boot (R5). No route answers it: it is the registry's own record
	// of whose choice a field is, which no reader of the number reads.
	SetByPerson []string `json:"-"`
}

// Computation is every field of a definition that says how its number is computed (R5): its
// name, question, number rule, decision, failure modes, unit, direction, kind and source. On a
// computed eval the catalogue states them and the seed alone writes them (ensure), and a register
// write naming one is refused (computationField); a logged eval's are its register writes'. It is
// one struct so that each of those rules reads or writes all of it at once.
type Computation struct {
	Name         string        `json:"name"`
	Question     string        `json:"question"`
	NumberRule   string        `json:"number_rule"`
	Decision     string        `json:"decision"`
	FailureModes []FailureMode `json:"failure_modes"`
	Unit         string        `json:"unit"`
	Direction    string        `json:"direction"`
	Kind         string        `json:"kind"`
	Source       string        `json:"source"`
}

// same reports whether two computations state one number: every field the same, an empty list of
// failure modes and none alike.
func (c Computation) same(other Computation) bool {
	return c.Name == other.Name && c.Question == other.Question && c.NumberRule == other.NumberRule &&
		c.Decision == other.Decision && slices.Equal(c.FailureModes, other.FailureModes) && c.Unit == other.Unit &&
		c.Direction == other.Direction && c.Kind == other.Kind && c.Source == other.Source
}

// Computed reports whether the eval's value is computed by a function rather than logged: its
// day rows are then the refresher's, and a reading logged under it is never folded into them.
func (d Definition) Computed() bool {
	return strings.HasPrefix(d.Source, SourceComputedPrefix)
}

// The writers of a day row (eval_days.writer, R3): the computation, the refresher and the day
// builders, which write a computed eval's rows; and the readings write, which keeps a logged
// eval's rows, beside the zero rows the refresher fills on that eval's days with no reading. Each
// read of an eval's rows reads its definition's writer's alone (Definition.writer), so an eval
// whose source changes never folds the rows the other writer made.
const (
	writerComputation = "computation"
	writerReadings    = "readings"
)

// writer is the writer whose day rows the eval's fold reads: the computation's for a computed
// eval, the readings write's for a logged one.
func (d Definition) writer() string {
	if d.Computed() {
		return writerComputation
	}
	return writerReadings
}

// Series is one version of one series of an eval, every column of eval_series. Its
// SeriesComputation is every field but the version's own number and time, its min_n, the one
// field of a series a person sets on a computed eval (R5), and the record of whether a person
// set it (SetByPerson).
type Series struct {
	EvalID  string `json:"eval_id"`
	Series  string `json:"series"`
	Version int    `json:"version"`
	SeriesComputation
	MinN      *int      `json:"min_n"`
	CreatedAt time.Time `json:"created_at"`
	// SetByPerson is every field of the series a person's register write has named, as
	// Definition.SetByPerson is of the definition: min_n, the one field of a series a person
	// sets on a computed eval. The seed fills the catalogue's min_n only on a series whose
	// record holds none (seriesToWrite).
	SetByPerson []string `json:"-"`
}

// SeriesComputation is every field of a series that says how its eval's number is computed
// (R5): the catalogue's on a computed eval, which the seed alone writes and a register write
// naming one is refused (computationField), as Computation is of the definition.
type SeriesComputation struct {
	Unit      string `json:"unit"`
	Role      string `json:"role"`
	IsPrimary bool   `json:"is_primary"`
	Listed    bool   `json:"listed"`
	Agg       string `json:"agg"`
	Links     []Link `json:"links"`
}

// same reports whether two series computations are the same: every field, each link's relation,
// address, label and time.
func (c SeriesComputation) same(other SeriesComputation) bool {
	return c.Unit == other.Unit && c.Role == other.Role && c.IsPrimary == other.IsPrimary && c.Listed == other.Listed &&
		c.Agg == other.Agg && sameLinks(c.Links, other.Links)
}

// Reading is one stored reading, every column of eval_readings. Replaces is the reading this one
// corrects, "" for none, and SupersededAt is when a correction replaced this one, nil while it
// stands.
type Reading struct {
	ID                string         `json:"id"`
	EvalID            string         `json:"eval_id"`
	Series            string         `json:"series"`
	RowKey            string         `json:"row_key"`
	RowKind           string         `json:"row_kind"`
	Col               string         `json:"col"`
	Value             *float64       `json:"value"`
	ValueAbsentReason string         `json:"value_absent_reason"`
	Unit              string         `json:"unit"`
	N                 *int64         `json:"n"`
	NUnit             string         `json:"n_unit"`
	CILow             *float64       `json:"ci_low"`
	CIHigh            *float64       `json:"ci_high"`
	CILevel           *float64       `json:"ci_level"`
	Attributes        map[string]any `json:"attributes"`
	Links             []Link         `json:"links"`
	MeasuredAt        time.Time      `json:"measured_at"`
	RecordedAt        time.Time      `json:"recorded_at"`
	RecordedBy        model.Actor    `json:"recorded_by"`
	Source            string         `json:"source"`
	Note              string         `json:"note"`
	Replaces          string         `json:"replaces"`
	SupersededAt      *time.Time     `json:"superseded_at"`
}

// DayRow is one stored day row, every column of eval_days but its writer, which each write of a
// row knows and each read names (Definition.writer): its key's project, eval, deploy repository
// ("" for an eval no deploy repository moves; deploy_repos.go) and repository, its listing and its
// bookkeeping (built_by, updated_at) beside its part, the columns the folds read
// (measures.DayPart: its series, its day, the day's UTC midnight, and its values), so a builder's
// part is written as the row that holds it and a stored row is folded as its part.
type DayRow struct {
	ProjectKey string `json:"project_key"`
	EvalID     string `json:"eval_id"`
	DeployRepo string `json:"deploy_repo"`
	Repo       string `json:"repo"`
	Listed     bool   `json:"listed"`
	measures.DayPart
	BuiltBy   string    `json:"built_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WindowRow is one day row as a DayWindow read answers it: the range it answers for
// (RangeWindow or RangeCompare), its eval, its part as the folds read it, and when it was last
// written. The part holds a sample list only where its series' fold reads one (readsSamples),
// and no counts where the row holds none (Counts nil, not an empty map).
type WindowRow struct {
	Range     string
	EvalID    string
	Part      measures.DayPart
	UpdatedAt time.Time
}

// The two ranges a DayWindow read answers rows for.
const (
	RangeWindow  = "window"
	RangeCompare = "compare"
)

// Field is one field of a write that changes a versioned row: Set says the write sets it, and
// Value is what it sets it to. A field the write leaves unset keeps the value the newest version
// holds (it is copied forward), or takes the default of a row written for the first time. A
// field Set to its zero value sets that value: Set[*float64](nil) turns a target off.
type Field[T any] struct {
	Set   bool
	Value T
}

// Set is the Field that sets value.
func Set[T any](value T) Field[T] {
	return Field[T]{Set: true, Value: value}
}

// applyTo writes the field's value into current when the write sets it.
func (f Field[T]) applyTo(current *T) {
	if f.Set {
		*current = f.Value
	}
}

// MeasuredAtFloor is the earliest measured_at a reading may hold (R18). The refresher writes a
// logged eval's days as explicit zero rows from its first reading on (fillLoggedZeros), so one
// reading measured at 0001-01-01 made one pass write 739,934 zero rows, 240 MB, in about 70 s.
// The floor leaves margin before any reading this deployment's producers are known to hold; a
// producer that needs earlier history asks for the floor to move, and it moves here alone.
var MeasuredAtFloor = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)

// MeasuredAtLead is how far past the server's own time a reading's measured_at may lie (R18): a
// producer's clock may run ahead of the server's, and by no more than this.
const MeasuredAtLead = 24 * time.Hour

// maxKeyBytes is the most bytes a key of the registry may hold: an eval's slug, a series' name,
// and a reading's row_kind, row_key and attributes.repo, each of which a write that takes it
// refuses past this (CodeKeyTooLong). Each is a column of a B-tree index, and Postgres refuses an
// index entry over 2,704 bytes (a third of an 8 kB page, less its headers) with an error that
// would answer 500. The widest entries the writes make are eval_days' primary key (project_key,
// eval_id, writer, series, repo, day) and eval_readings_row (eval_id, row_kind, row_key,
// measured_at). In the worst case, a key that does not compress, an entry is an 8-byte header,
// each text column's bytes with a 4-byte length and up to 3 bytes of alignment, each fixed-size
// column with its alignment, and up to 7 bytes rounding the whole to 8. A project key is at most
// 10 bytes (projects' check), so eval_id is at most 10 + 1 + 512 = 523, and writer at most 11
// ("computation"):
//
//	eval_days:         8 + (10+7) + (523+7) + (11+7) + (512+7) + (512+7) + (4+3) + 7 = 1,625 bytes
//	eval_readings_row: 8 + (523+7) + (512+7) + (512+7) + (8+7) + 7                  = 1,598 bytes
//
// Both are about 60 percent of the limit. The keys of eval_versions (eval_id, version) and
// eval_series (eval_id, series, version) are smaller still.
const maxKeyBytes = 512

// keyTooLong is the reason a key named field is refused for, "" where it fits (maxKeyBytes).
func keyTooLong(field, key string) string {
	if len(key) <= maxKeyBytes {
		return ""
	}
	return fmt.Sprintf("%s holds %d bytes, and a key of the registry holds at most %d", field, len(key), maxKeyBytes)
}

// The stable codes of the registry's refusals. Each is answered with the reason in plain words,
// and a readings write names every refused row by its index in the batch (RowError).
const (
	// CodeSlugInvalid: the slug is not lower-case letters, digits and hyphens starting with a
	// letter or a digit.
	CodeSlugInvalid = "EVAL_SLUG_INVALID"
	// CodeProjectNotFound: no project has the key.
	CodeProjectNotFound = "PROJECT_NOT_FOUND"
	// CodeFieldInvalid: a register write's field is outside the values its column allows (a
	// kind, a state, a direction, a target's judge, a series role or fold, a min_n below 0 or past
	// what the store holds), its source is not logged (only the seeded catalogue makes an eval
	// computed), it sets a min_n on a series no read ranks by one (only pr-ci's job series), or it
	// sets both doc_block and doc_text.
	CodeFieldInvalid = "EVAL_FIELD_INVALID"
	// CodeSeriesInvalid: a register write names a series with no name, or with whitespace, a
	// control character or a `.` in it (a target judge is `<series>.<stat>`).
	CodeSeriesInvalid = "EVAL_SERIES_INVALID"
	// CodeDocTextEmpty: a register write's doc_text holds no text.
	CodeDocTextEmpty = "EVAL_DOC_TEXT_EMPTY"
	// CodeDocumentNotDocument: the project's `evals-md` artifact is a file or an image.
	CodeDocumentNotDocument = "EVALS_DOCUMENT_NOT_A_DOCUMENT"
	// CodeReadingsEmpty: a readings write carries no row.
	CodeReadingsEmpty = "READINGS_EMPTY"
	// CodeReadingInvalid: a row is no JSON object, a field holds the wrong type, a key no reading
	// has, a required field (row_kind, measured_at) is missing, n is negative, or
	// attributes.repo is not a non-empty string.
	CodeReadingInvalid = "READING_INVALID"
	// CodeValueOrReason: a row has neither a value nor the reason it has none.
	CodeValueOrReason = "READING_VALUE_OR_REASON"
	// CodeFieldServerSet: a row, or one of its links, carries a key the server sets: id, eval_id,
	// recorded_by, recorded_at, superseded_at, or a link's recorded_at.
	CodeFieldServerSet = "READING_FIELD_SERVER_SET"
	// CodeReplacesUnknown: replaces names no reading of this eval.
	CodeReplacesUnknown = "READING_REPLACES_UNKNOWN"
	// CodeAlreadyReplaced: replaces names a reading a correction already replaced, or one an
	// earlier row of the same batch replaces.
	CodeAlreadyReplaced = "READING_ALREADY_REPLACED"
	// CodeLinkHrefNotAbsolute: a link's href is missing, or is not an absolute URL: it has no
	// scheme or no host.
	CodeLinkHrefNotAbsolute = "LINK_HREF_NOT_ABSOLUTE"
	// CodeLinkHrefNotHTTP: a link's href is an absolute URL whose scheme is neither http nor
	// https (javascript:, data:, mailto: and urn: among them): a page draws the link for a person
	// to click (R9).
	CodeLinkHrefNotHTTP = "LINK_HREF_NOT_HTTP"
	// CodeLinkRelMissing: a link carries no relation.
	CodeLinkRelMissing = "LINK_REL_MISSING"
	// CodeReadingSeriesInvalid: a row names no series, or one with whitespace, a control
	// character or a `.` in it.
	CodeReadingSeriesInvalid = "READING_SERIES_INVALID"
	// CodeReadingDayOverflow: the row's value, added to its day row of a logged eval, makes a
	// value that row stores (its sum, numerator or denominator) larger than a float holds, which
	// no read could answer.
	CodeReadingDayOverflow = "READING_DAY_OVERFLOW"
	// CodeMeasuredAtOutOfRange: a row's measured_at is before MeasuredAtFloor or more than
	// MeasuredAtLead after the server's time (R18).
	CodeMeasuredAtOutOfRange = "MEASURED_AT_OUT_OF_RANGE"
	// CodeKeyTooLong: a slug, a series' name, or a row's row_kind, row_key or attributes.repo
	// holds more than maxKeyBytes bytes, more than an index entry of the registry holds.
	CodeKeyTooLong = "EVAL_KEY_TOO_LONG"
	// CodeFieldComputed: a register write of a computed eval names a field of its computation,
	// which its code defines (R5): its name, question, number rule, decision, failure modes, unit,
	// direction, kind or source, a series it does not hold, or any field of a series but min_n.
	CodeFieldComputed = "EVAL_FIELD_COMPUTED"
)

// Error is a write the registry refused before it wrote anything: a stable code, the reason in
// plain words, the field of a register write the refusal is about where it is one field, and for
// a readings write every refused row. A readings write's code is the code of its first refused
// row; Rows names each.
type Error struct {
	Code    string     `json:"code"`
	Message string     `json:"error"`
	Field   string     `json:"field,omitempty"`
	Rows    []RowError `json:"rows,omitempty"`
}

func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

// RowError is one refused row of a readings write: its index in the batch, its code, the field
// the refusal is about where it is one field, and the reason.
type RowError struct {
	Index   int    `json:"index"`
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"error"`
}

func refusal(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// fieldRefusal is a refusal of a register write about one of its fields, which field names as
// the body spells it (`series[0].agg`).
func fieldRefusal(code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}
