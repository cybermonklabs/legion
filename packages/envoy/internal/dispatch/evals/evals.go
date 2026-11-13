// Package evals is the eval registry's store (dispatch://INFRA-2916, R1 to R5, R9 and R12):
// every write and read of eval_versions, eval_series and eval_readings, of the day rows
// (eval_days) of the evals whose readings are logged, and of each project's `evals-md` document,
// which holds one section per eval with the eval's method in it.
//
// An eval is one number a project's pages show. Its definition is append-only: each change
// writes the next version (Register), and a reader reads the newest (Current). Its readings are
// append-only too: a correction is a new reading naming the one it replaces (WriteReadings). A
// logged eval's day rows are kept by the readings write in its own transaction, by increment, so
// no window read reads a reading (DayWindow); a computed eval's day rows are the refresher's,
// and a reading logged under a computed eval's slug is stored and answered by its items alone.
//
// The package serves no request (the routes are api's) and makes none: a link is stored as its
// caller wrote it and its far side is never fetched, so the package imports no HTTP client
// (imports_test.go).
package evals

import (
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The values the registry's enumerated columns hold, each the set the column's check in
// 20261113082153_eval_registry.up.sql allows.
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
	// and by a settings write.
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
// and ReplacedBy are "" where they name nothing. Target is nil where the target is off.
type Definition struct {
	EvalID          string        `json:"eval_id"`
	Version         int           `json:"version"`
	ProjectKey      string        `json:"project_key"`
	Slug            string        `json:"slug"`
	Name            string        `json:"name"`
	Question        string        `json:"question"`
	NumberRule      string        `json:"number_rule"`
	Decision        string        `json:"decision"`
	DocBlock        string        `json:"doc_block"`
	FailureModes    []FailureMode `json:"failure_modes"`
	Unit            string        `json:"unit"`
	Direction       string        `json:"direction"`
	Target          *float64      `json:"target"`
	TargetInclusive bool          `json:"target_inclusive"`
	TargetJudges    []string      `json:"target_judges"`
	OwnerRole       string        `json:"owner_role"`
	Person          string        `json:"person"`
	IssueKey        string        `json:"issue_key"`
	Kind            string        `json:"kind"`
	Source          string        `json:"source"`
	State           string        `json:"state"`
	ReplacedBy      string        `json:"replaced_by"`
	Links           []Link        `json:"links"`
	WrittenVia      string        `json:"written_via"`
	WrittenBy       model.Actor   `json:"written_by"`
	CreatedAt       time.Time     `json:"created_at"`
}

// Computed reports whether the eval's value is computed by a function rather than logged: its
// day rows are then the refresher's, and a reading logged under it is never folded into them.
func (d Definition) Computed() bool {
	return strings.HasPrefix(d.Source, SourceComputedPrefix)
}

// Series is one version of one series of an eval, every column of eval_series.
type Series struct {
	EvalID    string    `json:"eval_id"`
	Series    string    `json:"series"`
	Version   int       `json:"version"`
	Unit      string    `json:"unit"`
	Role      string    `json:"role"`
	IsPrimary bool      `json:"is_primary"`
	MinN      *int      `json:"min_n"`
	Listed    bool      `json:"listed"`
	Agg       string    `json:"agg"`
	Links     []Link    `json:"links"`
	CreatedAt time.Time `json:"created_at"`
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

// DayRow is one stored day row, every column of eval_days, and the range of a DayWindow read it
// answered for (RangeWindow or RangeCompare; "" outside that read). Day is the day's UTC midnight.
// Sum, Num, Den, Max and LastValue are nil where the row holds no such part.
type DayRow struct {
	Range         string             `json:"range,omitempty"`
	ProjectKey    string             `json:"project_key"`
	EvalID        string             `json:"eval_id"`
	Series        string             `json:"series"`
	Repo          string             `json:"repo"`
	Day           time.Time          `json:"day"`
	Listed        bool               `json:"listed"`
	N             int                `json:"n"`
	Sum           *float64           `json:"sum"`
	Num           *float64           `json:"num"`
	Den           *float64           `json:"den"`
	Max           *float64           `json:"max"`
	Samples       []float64          `json:"samples"`
	LastValue     *float64           `json:"last_value"`
	LastAt        *time.Time         `json:"last_at"`
	Counts        map[string]float64 `json:"counts"`
	ViaRepository int                `json:"via_repository"`
	BuiltBy       string             `json:"built_by"`
	UpdatedAt     time.Time          `json:"updated_at"`
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

// The stable codes of the registry's refusals. Each is answered with the reason in plain words,
// and a readings write names every refused row by its index in the batch (RowError).
const (
	// CodeSlugInvalid: the slug is not lower-case letters, digits and hyphens starting with a
	// letter or a digit.
	CodeSlugInvalid = "EVAL_SLUG_INVALID"
	// CodeProjectNotFound: no project has the key.
	CodeProjectNotFound = "PROJECT_NOT_FOUND"
	// CodeFieldInvalid: a register write's field is outside the values its column allows (a
	// kind, a source, a state, a direction, a way of writing, a target's judge, a series role or
	// fold), or the write sets both doc_block and doc_text.
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
	// CodeLinkHrefNotAbsolute: a link's href is missing or is not an absolute URL with a scheme
	// and a host.
	CodeLinkHrefNotAbsolute = "LINK_HREF_NOT_ABSOLUTE"
	// CodeLinkRelMissing: a link carries no relation.
	CodeLinkRelMissing = "LINK_REL_MISSING"
	// CodeReadingSeriesInvalid: a row names no series, or one with whitespace, a control
	// character or a `.` in it.
	CodeReadingSeriesInvalid = "READING_SERIES_INVALID"
)

// Error is a write the registry refused before it wrote anything: a stable code, the reason in
// plain words, and for a readings write every refused row. A readings write's code is the code
// of its first refused row; Rows names each.
type Error struct {
	Code    string     `json:"code"`
	Message string     `json:"error"`
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
