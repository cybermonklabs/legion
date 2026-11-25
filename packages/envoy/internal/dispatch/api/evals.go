package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The eval routes (dispatch://INFRA-2916, R3 to R5, R7, R9, R11, R12 and R16): every number a
// project's pages show is an eval of the registry (internal/dispatch/evals), read here and
// written here.
//
//   - GET /api/v1/projects/{key}/evals and GET /api/v1/evals read the list: each eval's current
//     definition with each listed series' statistics over a window of days and a compare range,
//     folded from the stored day rows of those days (evals.FoldStatistics, the one fold each
//     source names), never from the population again (R12). The cross-project read takes
//     `project` repeatably and reads every project when it names none: no read here picks a
//     project for its caller (R11, R14).
//   - GET /api/v1/projects/{key}/evals/{slug} reads one eval's versions, newest first, the newest
//     with the window's values, and the method its section of the project's `evals-md` holds (R5).
//   - GET /api/v1/projects/{key}/evals/{slug}/items reads the rows behind the numbers, one shape
//     for every row kind (evals_items.go).
//   - POST /api/v1/projects/{key}/evals/{slug}/readings logs one batch of readings, which the
//     registry never refuses for an eval it does not hold (R3), under a required
//     Idempotency-Key (R4); POST /api/v1/projects/{key}/evals registers or changes a definition.
//     A reading is append-only: PUT and DELETE of one answer 405 READING_APPEND_ONLY.
//
// Every answer is the envelope {items, total, limit, offset}, and every refusal {code, error}
// with a stable code (R7).
//
// A window is the UTC days its [from, to) covers, a window ending inside a day covering that day:
// the day rows are whole days, so the answer states the days it folded rather than the instants
// it was asked for (evalDaysOf).

const (
	// evalPageDefault and evalPageMax are an eval read's page: limit is 1 to evalPageMax,
	// evalPageDefault where the caller names none.
	evalPageDefault = 50
	evalPageMax     = 200

	// evalRepoMax is the most repositories an eval read's `repo` names, a repository the query
	// repeats counted once, and evalRepoBytes the most bytes one of them holds, the bound of a
	// reading's attributes.repo (evals' key bound): a longer name is the repository of no stored
	// row. Every statement of the read sends the list, and at these bounds it takes at most
	// 100 × (512 + 4) bytes, about 50 KiB of the 256 KiB a statement sends (evals'
	// statementBytes), as a slug's 512 bytes keep each statement of a write under it.
	evalRepoMax   = 100
	evalRepoBytes = 512

	codeEvalNotFound           = "EVAL_NOT_FOUND"
	codeIdempotencyKeyRequired = "IDEMPOTENCY_KEY_REQUIRED"
	codeReadingAppendOnly      = "READING_APPEND_ONLY"

	// evalSessionQueryKey is the one query key every eval route takes beside those its shape
	// declares: the `session` parameter every operation carries (actingPerson, openapi.go's
	// openAPISessionParameter).
	evalSessionQueryKey = "session"
)

// evalQueryKeys is the query keys one eval read takes, read from the query struct its route shape
// names (routeShapes' Params): every key it declares, the struct's `query` tags (queryKeysOf),
// any other of which the read refuses (undeclaredQueryKey), and those it takes one value of, in
// name order, each of which it refuses named twice (repeatedQueryKey): the fields that are no
// list (oneValueQueryKeys) and the global session parameter, which every route reads one value of
// (actingPerson). A list field there is a key the read takes any number of values of.
type evalQueryKeys struct {
	declared, oneValue []string
}

// evalQueryKeysOf is the query keys of params, an eval read's query struct.
func evalQueryKeysOf(params any) evalQueryKeys {
	oneValue := append(oneValueQueryKeys(params), evalSessionQueryKey)
	slices.Sort(oneValue)
	return evalQueryKeys{declared: queryKeysOf(params), oneValue: oneValue}
}

// The query keys each eval read takes (evalQueryKeysOf), read once when the package starts. A
// readings write declares none.
var (
	evalListQueryKeys   = evalQueryKeysOf(evalListQuery{})
	evalsQueryKeys      = evalQueryKeysOf(evalsQuery{})
	evalDetailQueryKeys = evalQueryKeysOf(evalDetailQuery{})
	evalItemsQueryKeys  = evalQueryKeysOf(evalItemsQuery{})
)

// queryKeysOf is the keys params, a route's query struct, declares: each field's `query` tag.
func queryKeysOf(params any) []string {
	structure := reflect.TypeOf(params)
	keys := make([]string, 0, structure.NumField())
	for index := range structure.NumField() {
		if key, tagged := structure.Field(index).Tag.Lookup("query"); tagged {
			keys = append(keys, key)
		}
	}
	return keys
}

// oneValueQueryKeys is the keys params, a route's query struct, declares for one value: the
// `query` tag of each field that is no list, in the order params declares them. A list field is a
// key the route takes any number of values of.
func oneValueQueryKeys(params any) []string {
	structure := reflect.TypeOf(params)
	keys := make([]string, 0, structure.NumField())
	for index := range structure.NumField() {
		field := structure.Field(index)
		if key, tagged := field.Tag.Lookup("query"); tagged && field.Type.Kind() != reflect.Slice {
			keys = append(keys, key)
		}
	}
	return keys
}

// undeclaredQueryKey refuses a query carrying a key its eval route does not take: 400
// INVALID_QUERY naming the first such key in name order, where declared is the keys the route's
// shape declares and the global session parameter is taken by every route; nil where every key
// is one of those. A read that passed over a key it does not take would answer a question other
// than the one asked: a caller still sending the old `scope=` would be answered every issue as if
// it had been answered the roots.
func undeclaredQueryKey(query url.Values, declared []string) error {
	for _, key := range slices.Sorted(maps.Keys(query)) {
		if key == evalSessionQueryKey || slices.Contains(declared, key) {
			continue
		}
		// The key is the request's, of any length, and the refusal of a write is the answer its
		// idempotency key keeps, so it names the key cut to a field's bound.
		if len(declared) == 0 {
			return errorf(http.StatusBadRequest, "INVALID_QUERY", "%q is no query key of this route, which takes none but %s", evals.BoundedField(key), evalSessionQueryKey)
		}
		return errorf(http.StatusBadRequest, "INVALID_QUERY", "%q is no query key of this route, which takes %s and %s",
			evals.BoundedField(key), strings.Join(declared, ", "), evalSessionQueryKey)
	}
	return nil
}

// bodyKeys is the keys a JSON object of one Go struct takes, each spelled exactly as its field's
// `json` tag spells it, with the keys the value under each takes in turn: nil for a value whose
// keys are not checked here (a scalar, a map, a time, json.RawMessage). A list's value, and a
// pointer's, takes the keys of what it holds, so `series` takes evalSeriesInput's in each element.
type bodyKeys map[string]bodyKeys

// The keys each eval write's body takes (bodyKeysOf), read once when the package starts: the
// register write's, every field the registry can set, and the readings write's, whose rows the
// registry reads key by key itself (evals.Batch).
var (
	evalRegisterBodyKeys = bodyKeysOf(reflect.TypeFor[evalRegisterInput]())
	evalReadingsBodyKeys = bodyKeysOf(reflect.TypeFor[evalReadingsBody]())
)

// bodyKeysOf is the keys a JSON value of shape takes: each exported field's `json` name, or its Go
// name where its tag names none, with the keys of that field's own type; an embedded struct with
// no name of its own lends its fields, as encoding/json promotes them. A type that decodes itself
// (json.Unmarshaler) takes keys of its own making, so it is not checked.
func bodyKeysOf(shape reflect.Type) bodyKeys {
	for shape.Kind() == reflect.Pointer || shape.Kind() == reflect.Slice {
		shape = shape.Elem()
	}
	if shape.Kind() != reflect.Struct || reflect.PointerTo(shape).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return nil
	}
	keys := bodyKeys{}
	for field := range shape.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch {
		case name == "-" || (!field.IsExported() && !field.Anonymous):
		case field.Anonymous && name == "":
			maps.Copy(keys, bodyKeysOf(field.Type))
		case name == "":
			keys[field.Name] = bodyKeysOf(field.Type)
		default:
			keys[name] = bodyKeysOf(field.Type)
		}
	}
	return keys
}

// unspelledBodyKey refuses a JSON body holding a key its type does not spell exactly (keys): 400
// INVALID_JSON naming the first such key, by where it stands, in name order; nil where every key
// at every depth keys checks is one keys holds. encoding/json matches a key to a field without
// regard to case, so `"Target": 10` would set the target the typed decode reads while the write,
// which reads the keys a body names to tell a field it sets from one it leaves out
// (decodeEvalRegistration's fieldOf), found no `target` and answered 200 with the target it had:
// a write accepted and then ignored (R5). raw is a body the typed decode already took, so a value
// that is no object or list is one keys does not check.
func unspelledBodyKey(raw json.RawMessage, keys bodyKeys, at string) error {
	value := bytes.TrimLeft(raw, " \t\r\n")
	if len(value) == 0 {
		return nil
	}
	switch value[0] {
	case '[':
		var elements []json.RawMessage
		if json.Unmarshal(raw, &elements) != nil {
			return nil
		}
		for index, element := range elements {
			if err := unspelledBodyKey(element, keys, fmt.Sprintf("%s[%d]", at, index)); err != nil {
				return err
			}
		}
	case '{':
		var named map[string]json.RawMessage
		if json.Unmarshal(raw, &named) != nil {
			return nil
		}
		where := "the body"
		if at != "" {
			where = at
		}
		for _, key := range slices.Sorted(maps.Keys(named)) {
			inner, spelled := keys[key]
			if !spelled {
				// The key is the body's, of any length; the refusal names it cut to a field's
				// bound, so the answer an idempotency key keeps stays bounded whatever the body.
				return errorf(http.StatusBadRequest, "INVALID_JSON", "%s holds the key %q, which is none of the keys it takes, each spelled exactly so: %s",
					where, evals.BoundedField(key), strings.Join(slices.Sorted(maps.Keys(keys)), ", "))
			}
			if inner == nil {
				continue
			}
			path := key
			if at != "" {
				path = at + "." + key
			}
			if err := unspelledBodyKey(named[key], inner, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// decodeEvalBody reads an eval write's JSON body into value, whose keys are keys
// (evalRegisterBodyKeys, evalReadingsBodyKeys), and answers the body as sent. A body that is no
// JSON value of value's type, or one holding a key keys does not spell exactly (unspelledBodyKey),
// is 400 INVALID_JSON.
func decodeEvalBody(r *http.Request, value any, keys bodyKeys) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := decodeJSON(r, &raw); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return nil, errorf(http.StatusBadRequest, "INVALID_JSON", "invalid JSON body: %v", err)
	}
	if err := unspelledBodyKey(raw, keys, ""); err != nil {
		return nil, err
	}
	return raw, nil
}

// evalRange is a range of whole UTC days an answer folded: [from, to), both UTC midnights, and how
// many days that is.
type evalRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Days int       `json:"days"`
}

func newEvalRange(days evals.Range) evalRange {
	return evalRange{From: days.From, To: days.To, Days: int(days.To.Sub(days.From) / (24 * time.Hour))}
}

// evalCompareRange is the answer's compare range, null where the request named none.
func evalCompareRange(compare *evals.Range) *evalRange {
	if compare == nil {
		return nil
	}
	return new(newEvalRange(*compare))
}

// evalFreshness is how fresh the day rows an answer folded are: the oldest updated_at among them,
// the moment each row's values last moved (a rebuild to the same values leaves it, so the live
// evals view, which hears of a row only when its values move, answers the same; null where it
// folded none), and the day-row refresher's age (evals.RefresherAgeIn), in seconds: the time
// since its last pass ran, or since a pass last read the delivery change log to its end,
// whichever is older, so a pass catching up a backlog of changes reads as old as its last read of
// the whole log; null where no pass has run on this database. The refresher is the deployment's
// one, so its age says how fresh the computed evals' rows are and never whether a project has any.
type evalFreshness struct {
	RowsUpdatedAt       *time.Time `json:"rows_updated_at"`
	RefresherAgeSeconds *int       `json:"refresher_age_seconds"`
}

// evalOwner is who an eval answers to: the Legion role and the person its owner columns name,
// and the writer of its current version (written_by).
type evalOwner struct {
	Role      string      `json:"role"`
	Person    string      `json:"person"`
	WrittenBy model.Actor `json:"written_by"`
}

// evalItem is one eval as the list read states it, and one version of one as the detail read
// does: the definition (every field of eval_versions, R1), its owner, each listed series with its
// values, and the verdict of its target (null where the target is off). The detail read states
// the series and the verdict of the newest version alone; an older version's are null.
type evalItem struct {
	Definition evals.Definition     `json:"definition"`
	Owner      evalOwner            `json:"owner"`
	Series     []evals.SeriesValues `json:"series"`
	Verdict    *evals.Verdict       `json:"verdict"`
}

// evalListItem is one eval of the list read: its id, `<project_key>/<slug>` (the definition's
// eval_id), and the eval as evalItem states it. The id is what a page and the live hub's evals
// view (live_frames.go) tell the list's evals apart by, as every list a live view sends item by
// item carries one; the detail read's items, versions of one eval, carry none.
type evalListItem struct {
	ID string `json:"id"`
	evalItem
}

// evalListResponse is GET /api/v1/projects/{key}/evals and GET /api/v1/evals: one page of evals,
// by project and then by slug, with the window and the compare range they were folded over and
// how fresh the folded rows are.
type evalListResponse struct {
	Items     []evalListItem `json:"items"`
	Total     int            `json:"total"`
	Limit     int            `json:"limit"`
	Offset    int            `json:"offset"`
	Window    evalRange      `json:"window"`
	Compare   *evalRange     `json:"compare"`
	Freshness evalFreshness  `json:"freshness"`
}

// evalDetailResponse is GET /api/v1/projects/{key}/evals/{slug}: one page of the eval's versions,
// newest first, the newest carrying the window's values, and doc_text, the method its section of
// the project's `evals-md` holds (null where the document holds no section for it).
type evalDetailResponse struct {
	Items     []evalItem    `json:"items"`
	Total     int           `json:"total"`
	Limit     int           `json:"limit"`
	Offset    int           `json:"offset"`
	Window    evalRange     `json:"window"`
	Compare   *evalRange    `json:"compare"`
	Freshness evalFreshness `json:"freshness"`
	DocText   *string       `json:"doc_text"`
}

// evalLinkInput is one link a write gives: its relation, its absolute address and its label. The
// server sets when it was stored, and a link carrying recorded_at is refused.
type evalLinkInput struct {
	Rel   string `json:"rel" required:"true"`
	Href  string `json:"href" required:"true"`
	Label string `json:"label"`
}

// evalReadingInput is one reading of a readings write, the keys a reading's row may carry
// (evals.Batch). The server sets id, eval_id, recorded_by, recorded_at and superseded_at, and a
// row carrying any of them is refused, naming the row (READING_FIELD_SERVER_SET).
type evalReadingInput struct {
	Series            string          `json:"series" required:"true"`
	RowKey            string          `json:"row_key"`
	RowKind           string          `json:"row_kind" required:"true"`
	Col               string          `json:"col"`
	Value             *float64        `json:"value"`
	ValueAbsentReason string          `json:"value_absent_reason"`
	Unit              string          `json:"unit"`
	N                 *int64          `json:"n"`
	NUnit             string          `json:"n_unit"`
	CILow             *float64        `json:"ci_low"`
	CIHigh            *float64        `json:"ci_high"`
	CILevel           *float64        `json:"ci_level"`
	Attributes        map[string]any  `json:"attributes"`
	Links             []evalLinkInput `json:"links"`
	MeasuredAt        time.Time       `json:"measured_at" required:"true"`
	Source            string          `json:"source"`
	Note              string          `json:"note"`
	Replaces          string          `json:"replaces"`
}

// evalReadingsInput is POST /api/v1/projects/{key}/evals/{slug}/readings's body: the batch, and
// the acting session for a bearer caller.
type evalReadingsInput struct {
	Actor    *model.Actor       `json:"actor"`
	Readings []evalReadingInput `json:"readings" required:"true"`
}

// evalReadingsBody is the readings write's body as its handler decodes it: each reading as the
// caller sent it, so the registry reads every key a row carries, those the server sets included
// (evals.Batch). evalReadingsInput is the same body as a client writes it.
type evalReadingsBody struct {
	Actor    *model.Actor      `json:"actor"`
	Readings []json.RawMessage `json:"readings"`
}

// evalReadingsResponse is the readings write's answer: the readings stored, in the batch's order,
// the eval's definition, and whether this write registered it (R3).
type evalReadingsResponse struct {
	Items      []evals.Reading  `json:"items"`
	Total      int              `json:"total"`
	Limit      int              `json:"limit"`
	Offset     int              `json:"offset"`
	Definition evals.Definition `json:"definition"`
	Registered bool             `json:"registered"`
}

// evalSeriesInput is one series a register write registers or changes, by name; a field it leaves
// out keeps the newest version's value (evals.SeriesChange).
type evalSeriesInput struct {
	Series    string          `json:"series" required:"true"`
	Unit      string          `json:"unit"`
	Role      string          `json:"role"`
	IsPrimary bool            `json:"is_primary"`
	MinN      *int            `json:"min_n"`
	Listed    bool            `json:"listed"`
	Agg       string          `json:"agg"`
	Links     []evalLinkInput `json:"links"`
}

// evalRegisterInput is POST /api/v1/projects/{key}/evals's body: the eval by slug, and each field
// the write sets. A field the body leaves out keeps the newest version's value, or the default of
// an eval registered here first (evals.Registration); a field the body names sets it, `target:
// null` turning the target off. doc_text, when given, is the eval's new method, written into its
// section of the project's `evals-md`.
type evalRegisterInput struct {
	Actor           *model.Actor        `json:"actor"`
	Slug            string              `json:"slug" required:"true"`
	Name            string              `json:"name"`
	Question        string              `json:"question"`
	NumberRule      string              `json:"number_rule"`
	Decision        string              `json:"decision"`
	DocBlock        string              `json:"doc_block"`
	DocText         string              `json:"doc_text"`
	FailureModes    []evals.FailureMode `json:"failure_modes"`
	Unit            string              `json:"unit"`
	Direction       string              `json:"direction"`
	Target          *float64            `json:"target"`
	TargetInclusive bool                `json:"target_inclusive"`
	TargetJudges    []string            `json:"target_judges"`
	OwnerRole       string              `json:"owner_role"`
	Person          string              `json:"person"`
	IssueKey        string              `json:"issue_key"`
	Kind            string              `json:"kind"`
	Source          string              `json:"source"`
	State           string              `json:"state"`
	ReplacedBy      string              `json:"replaced_by"`
	Links           []evalLinkInput     `json:"links"`
	Series          []evalSeriesInput   `json:"series"`
}

// evalRegistered is what a register write left: the eval's newest definition, the newest version
// of each of its series, and whether the write stored a new version of the definition or of a
// series.
type evalRegistered struct {
	Definition evals.Definition `json:"definition"`
	Series     []evals.Series   `json:"series"`
	Wrote      bool             `json:"wrote"`
}

// evalRegisterResponse is the register write's answer: the one version it left.
type evalRegisterResponse struct {
	Items  []evalRegistered `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// registry is the eval registry over this server's database, writing `evals-md` through its
// document service and its events through its broker, and attributing the pull requests its day
// rows count through its Attributor (Deps.Attributor).
func (s *server) registry() *evals.Store {
	return evals.New(s.deps.Store, s.deps.Docs, s.deps.Events, s.deps.Attributor)
}

// evalRanges is the window and the compare range one eval read asked for: each as the whole UTC
// days it covers, and each as the instants it was asked for, [from, to), whose end the
// failing-jobs rows look back measures.FailingWindow from, as the Pipeline route looks back from
// its at.
type evalRanges struct {
	window      evals.Range
	compare     *evals.Range
	from, to    time.Time
	compareFrom time.Time
	compareTo   time.Time
}

// evalDaysOf is the UTC days [from, to) covers: from the midnight of from's day to the midnight
// after the day of to's last instant, so a window ending inside a day covers that day.
func evalDaysOf(from, to time.Time) evals.Range {
	first := measures.Midnight(from)
	return evals.Range{From: first, To: measures.Midnight(to.Add(-time.Nanosecond)).AddDate(0, 0, 1)}
}

// evalRangeDays is the UTC days of days, a range of whole UTC days (evalDaysOf), oldest first, as
// YYYY-MM-DD: the days a fold over the range's day rows puts its parts on.
func evalRangeDays(days evals.Range) []string {
	var shown []string
	for day := days.From; day.Before(days.To); day = day.AddDate(0, 0, 1) {
		shown = append(shown, day.Format(time.DateOnly))
	}
	return shown
}

// parseEvalRanges reads an eval read's window and compare range: from and to as every delivery
// route reads them (parseDeliveryWindow: RFC3339 in the UTC years 1 to 9999, the last 7 days by
// default, at most 84 days), and compare_from and compare_to, both or neither, read the same way
// and held to the same bound, though the two ranges need not be of one length. A range whose days
// end past the year 9999 is refused too (R18): its answer states the midnight after its last day,
// so a to on 9999-12-31 would end it at 10000-01-01, which no JSON answer can write.
func parseEvalRanges(query url.Values) (evalRanges, error) {
	from, to, err := parseDeliveryWindow(query)
	if err != nil {
		return evalRanges{}, err
	}
	ranges := evalRanges{window: evalDaysOf(from, to), from: from.UTC(), to: to.UTC()}
	if err := evalDaysAnswerable("to", ranges.window); err != nil {
		return evalRanges{}, err
	}
	if query.Has("compare_from") != query.Has("compare_to") {
		return evalRanges{}, errors.New("compare_from and compare_to come together: name both, or neither")
	}
	if !query.Has("compare_from") {
		return ranges, nil
	}
	compareFrom, err := parseOptionalRFC3339(query, "compare_from", time.Time{})
	if err != nil {
		return evalRanges{}, err
	}
	compareTo, err := parseOptionalRFC3339(query, "compare_to", time.Time{})
	if err != nil {
		return evalRanges{}, err
	}
	if !compareFrom.Before(compareTo) {
		return evalRanges{}, errors.New("compare_from must be before compare_to")
	}
	if compareTo.Sub(compareFrom) > maxDeliveryWindow {
		return evalRanges{}, errors.New("compare_from..compare_to must not span more than 84 days")
	}
	ranges.compare = new(evalDaysOf(compareFrom, compareTo))
	if err := evalDaysAnswerable("compare_to", *ranges.compare); err != nil {
		return evalRanges{}, err
	}
	ranges.compareFrom, ranges.compareTo = compareFrom.UTC(), compareTo.UTC()
	return ranges, nil
}

// evalDaysAnswerable refuses days whose end, the midnight after their last day, is past the year
// 9999, naming the key, to or compare_to, that ends them there.
func evalDaysAnswerable(key string, days evals.Range) error {
	if days.To.Year() > 9999 {
		return fmt.Errorf("%s ends its days at %s, past the year 9999, which no answer can write: the last day a window may cover is 9999-12-30",
			key, days.To.Format(time.RFC3339))
	}
	return nil
}

// evalReadQuery is what every eval read reads of its query: the window and the compare range, the
// repositories whose day rows it folds (repo, none for the project's own rows), and the page.
type evalReadQuery struct {
	ranges evalRanges
	repos  []string
	limit  int
	offset int
}

// parseEvalReadQuery reads what every eval read takes of its query; keys is the keys the reading
// route takes (evalListQueryKeys, evalsQueryKeys, evalDetailQueryKeys, evalItemsQueryKeys), any
// other of which is refused (undeclaredQueryKey), as is a key named more than once that the read
// takes one value of (repeatedQueryKey). A malformed key is 400 INVALID_QUERY naming it; the page
// is limit, 1 to evalPageMax (evalPageDefault where absent), and offset, from 0.
func parseEvalReadQuery(query url.Values, keys evalQueryKeys) (evalReadQuery, error) {
	if err := undeclaredQueryKey(query, keys.declared); err != nil {
		return evalReadQuery{}, err
	}
	if err := repeatedQueryKey(query, keys.oneValue); err != nil {
		return evalReadQuery{}, err
	}
	ranges, err := parseEvalRanges(query)
	if err != nil {
		return evalReadQuery{}, invalidEvalQuery(err)
	}
	repos, err := parseEvalRepos(query)
	if err != nil {
		return evalReadQuery{}, invalidEvalQuery(err)
	}
	limit, offset, err := parseOffsetPage(query, evalPageDefault, evalPageMax)
	if err != nil {
		return evalReadQuery{}, err
	}
	return evalReadQuery{ranges: ranges, repos: repos, limit: limit, offset: offset}, nil
}

// invalidEvalQuery is err as the 400 INVALID_QUERY an eval read answers a malformed key with.
func invalidEvalQuery(err error) error {
	return errorf(http.StatusBadRequest, "INVALID_QUERY", "%s", err.Error())
}

// repeatedQueryKey refuses a query naming more than once a key its route takes one value of: 400
// INVALID_QUERY naming the first such key in the order oneValue gives them, nil where none is. A
// delivery read's oneValue is the keys of its shape's query struct that are no list, in the order
// the shape declares them (oneValueQueryKeys); an eval read's is those and the global session
// parameter, in name order (evalQueryKeys), and a readings write's the session parameter alone.
// Such a key is read as its first value, so `?kind=legion&kind=ci` would be answered the Legion
// evals alone, and ?from=A&from=B the window from A: a question other than the one asked, as a key
// the route does not take would be (undeclaredQueryKey). A list key of the shape (the
// cross-project read's projects, every read's repositories, the delivery reads' facets) is taken
// any number of times, and a delivery route passes over a key it does not read.
func repeatedQueryKey(query url.Values, oneValue []string) error {
	for _, key := range oneValue {
		if err := namedOnce(query, key); err != nil {
			return err
		}
	}
	return nil
}

// namedOnce refuses a query naming key more than once, for a route that reads one value of it:
// 400 INVALID_QUERY naming the key and how many times the query names it, nil where it names the
// key once or not at all.
func namedOnce(query url.Values, key string) error {
	if named := len(query[key]); named > 1 {
		return errorf(http.StatusBadRequest, "INVALID_QUERY", "%q is named %d times, and this route reads one value of it", key, named)
	}
	return nil
}

// parseEvalRepos reads `repo`, repeatable: the repositories whose day rows a read folds in place
// of the project's own, each once, in the order the query first names it. An empty value names no
// repository and is refused, and so is one of more than evalRepoBytes bytes, and one starting
// with ':', which the stored rows keep the day rows of what a repository attributes and of what
// reaches a project only through its default repository under (evals.OwnerKey,
// legionfacts.DefaultRepo): no repository is called that, and a read names repositories alone. A
// query naming more than evalRepoMax repositories is refused, so the list stays a small part of
// each statement of the read that sends it.
func parseEvalRepos(query url.Values) ([]string, error) {
	named := query["repo"]
	if slices.Contains(named, "") {
		return nil, errors.New("repo names a repository, and an empty repo names none")
	}
	if at := slices.IndexFunc(named, func(repo string) bool { return len(repo) > evalRepoBytes }); at >= 0 {
		return nil, fmt.Errorf("repo %q holds %d bytes, and a repository name holds at most %d", evals.BoundedField(named[at]), len(named[at]), evalRepoBytes)
	}
	if at := slices.IndexFunc(named, func(repo string) bool { return strings.HasPrefix(repo, ":") }); at >= 0 {
		return nil, fmt.Errorf("repo names a repository, and %q is no repository name", named[at])
	}
	var repos []string
	seen := make(map[string]bool, len(named))
	for _, repo := range named {
		if !seen[repo] {
			seen[repo] = true
			repos = append(repos, repo)
		}
	}
	if len(repos) > evalRepoMax {
		return nil, fmt.Errorf("repo names %d repositories, and a read takes at most %d", len(repos), evalRepoMax)
	}
	return repos, nil
}

// evalListRequest is one read of the eval list: what every eval read reads of its query, the kind
// it keeps, "" for every kind, and projectsIn, which reads in the answer's snapshot the projects it
// reads, in order: the project route's project, found (loadProject), and the cross-project read's
// (evalProjectsIn).
type evalListRequest struct {
	evalReadQuery
	kind       string
	projectsIn func(ctx context.Context, tx pgx.Tx) ([]string, error)
}

// parseEvalListQuery reads the list read's query, every key but `project`, which only the
// cross-project read takes; keys is the keys the reading route takes (evalListQueryKeys,
// evalsQueryKeys), any other of which is refused (undeclaredQueryKey). A malformed key is 400
// INVALID_QUERY naming it.
func parseEvalListQuery(query url.Values, keys evalQueryKeys) (evalListRequest, error) {
	read, err := parseEvalReadQuery(query, keys)
	if err != nil {
		return evalListRequest{}, err
	}
	kind := query.Get("kind")
	if kind != "" && !slices.Contains(evals.Kinds, kind) {
		return evalListRequest{}, invalidEvalQuery(fmt.Errorf("kind %q is not one of %s", kind, strings.Join(evals.Kinds, ", ")))
	}
	return evalListRequest{evalReadQuery: read, kind: kind}, nil
}

// listProjectEvals answers GET /api/v1/projects/{key}/evals: one page of the project's evals
// (readProjectEvalList).
func (s *server) listProjectEvals(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	answer, err := s.readProjectEvalList(r.Context(), r.URL.Query(), r.PathValue("key"), time.Now().UTC())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// readProjectEvalList is the body of GET /api/v1/projects/{key}/evals for query, as of now: the
// list read of project's evals (readEvalList), after the query is read (parseEvalListQuery), the
// project found in the answer's snapshot, 404 PROJECT_NOT_FOUND for a project no row holds
// (loadProject). The live hub's evals view reads its body through this too, so the two answer one
// query alike.
func (s *server) readProjectEvalList(ctx context.Context, query url.Values, project string, now time.Time) (evalListResponse, error) {
	request, err := parseEvalListQuery(query, evalListQueryKeys)
	if err != nil {
		return evalListResponse{}, err
	}
	request.projectsIn = func(ctx context.Context, tx pgx.Tx) ([]string, error) {
		if _, err := s.loadProject(ctx, tx, project); err != nil {
			return nil, err
		}
		return []string{project}, nil
	}
	return s.readEvalList(ctx, request, now)
}

// listEvals answers GET /api/v1/evals: the list read across projects, each item the shape the
// project read answers. `project`, repeatable, names the projects read, in the order named; with
// none named it reads every project the store holds, by key, and never picks one for its caller
// (R11). A named project no row holds is 404 PROJECT_NOT_FOUND naming it.
func (s *server) listEvals(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	request, err := parseEvalListQuery(query, evalsQueryKeys)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	named := query["project"]
	if slices.Contains(named, "") {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "project names a project, and an empty project names none")
		return
	}
	request.projectsIn = func(ctx context.Context, tx pgx.Tx) ([]string, error) {
		return evalProjectsIn(ctx, tx, named)
	}
	answer, err := s.readEvalList(r.Context(), request, time.Now().UTC())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// evalProjectsIn is the projects a cross-project read reads, read in tx, the answer's snapshot:
// those named, each once in the order first named, or every project by key where none is named
// (evals.ProjectsIn). A name no project holds is 404 PROJECT_NOT_FOUND naming it.
func evalProjectsIn(ctx context.Context, tx pgx.Tx, named []string) ([]string, error) {
	keys, err := evals.ProjectsIn(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(named) == 0 {
		return keys, nil
	}
	var projects []string
	for _, key := range named {
		if !slices.Contains(keys, key) {
			return nil, errorf(http.StatusNotFound, "PROJECT_NOT_FOUND", "no project has the key %q", key)
		}
		if !slices.Contains(projects, key) {
			projects = append(projects, key)
		}
	}
	return projects, nil
}

// readEvalList is the list read: one page of the evals of request's projects, by project in the
// order given and then by slug (evals.CurrentIn, through evals_current), each with its values over
// the window and the compare range (readEvalValues). The page runs across the projects: total is
// every project's count, and offset counts from the first project's first eval. It is the body of
// both list routes, which the live hub's view of the evals reads too. Each of its reads reads
// every project at once, so a read of many projects makes as many statements as a read of one.
//
// One answer, one snapshot: every read it makes (its projects, request.projectsIn, the page, the
// deploy repository its values read, the one recorded with the stored rows, evals.DeployRepoIn,
// and the reads its values are stated from, readEvalValues) runs in one read-only
// repeatable-read transaction on one connection (evals.Store.BeginSnapshot), committed before the
// folds (evalValues.items). A write that commits while the answer reads, as a settings rewrite
// commits its day rows, their stored figures and the settings they stand built under, is in the
// whole answer or in none of it, so the live evals view, which sends what moved between two
// answers, sends it as one delta of the whole new state, never as an eval's old window figures
// beside its new days, nor the new deploy repository's rows as the old production rule built
// them. The transaction holds ctx's connection mark, so a read of the pool made under it is
// refused (store.ErrNestedAcquire).
func (s *server) readEvalList(ctx context.Context, request evalListRequest, now time.Time) (evalListResponse, error) {
	tx, err := s.registry().BeginSnapshot(ctx)
	if err != nil {
		return evalListResponse{}, fmt.Errorf("begin the read of the eval list: %w", err)
	}
	defer tx.Rollback(ctx)
	projects, err := request.projectsIn(ctx, tx)
	if err != nil {
		return evalListResponse{}, err
	}
	page, err := evals.CurrentIn(ctx, tx, projects, request.kind, request.limit, request.offset)
	if err != nil {
		return evalListResponse{}, err
	}
	deployRepo, err := evals.DeployRepoIn(ctx, tx)
	if err != nil {
		return evalListResponse{}, err
	}
	values, err := readEvalValues(ctx, tx, page.Items, deployRepo, request.repos, request.ranges, now)
	if err != nil {
		return evalListResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return evalListResponse{}, fmt.Errorf("end the read of the evals of %v: %w", projects, err)
	}
	items, freshness := values.items()
	answer := evalListResponse{
		Items: make([]evalListItem, len(items)), Total: page.Total, Limit: request.limit, Offset: request.offset,
		Window: newEvalRange(request.ranges.window), Compare: evalCompareRange(request.ranges.compare), Freshness: freshness,
	}
	for index, item := range items {
		answer.Items[index] = evalListItem{ID: item.Definition.EvalID, evalItem: item}
	}
	return answer, nil
}

// evalValues is what one answer states its evals' values from, read in its snapshot
// (readEvalValues): each eval's values where the stored figures answer it, the indexes of those
// they do not (unstated) with the day rows and the first days their values are folded from, the
// oldest updated_at of the stored rows read, and the refresher's age.
type evalValues struct {
	entries  []evals.Entry
	ranges   evalRanges
	values   [][]evals.SeriesValues
	unstated []int
	rows     []evals.WindowRow
	firsts   map[string]map[string]time.Time
	oldest   *time.Time
	age      time.Duration
	aged     bool
}

// readEvalValues reads in tx, one answer's snapshot (readEvalList), the refresher's age at now
// (evals.RefresherAgeIn) and what each of entries' values over ranges is stated from. Each eval
// that reads the deploy repository is stated from the rows kept for deployRepo, the repository
// recorded with the stored rows (evals.DeployRepoOf, evals.DeployRepoIn), so a move of the deploy
// repository is this selection and rewrites nothing. It reads each series' first day
// (evals.FirstDaysIn), and for an offered range (evals.FiguresOffered: a window and a compare range
// each of a length the Evals tab offers, of the projects' own rows or of the one repository named)
// the stored figures of that cut (evals.ReadStoredFiguresIn: the read of how far each project is
// built and which of its evals a stale mark of the cut names, then two primary-key reads of the
// evals they serve), from which it states each eval they answer. An eval they cannot answer (an
// eval of a project whose figures are not built yet, one whose rows in the cut a move of a default
// repository moved and the refresher has not folded again, or one whose stored row was folded
// under another fold key than the eval's) and every eval of any other read, one that names two or
// more repositories among them, is stated from one read of the day rows of the window and the
// compare range of those evals alone (evals.DayWindowIn, the rows of repos where it names any),
// folded once the snapshot is committed (evalValues.items). Both answer the same bits
// (evals.StoredFigures.SeriesValues). Each read reads every project the entries it reads are evals
// of at once.
func readEvalValues(ctx context.Context, tx pgx.Tx, entries []evals.Entry, deployRepo string, repos []string, ranges evalRanges, now time.Time) (evalValues, error) {
	read := evalValues{entries: entries, ranges: ranges, values: make([][]evals.SeriesValues, len(entries))}
	var err error
	if read.age, read.aged, err = evals.RefresherAgeIn(ctx, tx, now); err != nil {
		return evalValues{}, err
	}
	if len(entries) == 0 {
		return read, nil
	}
	if read.firsts, err = evals.FirstDaysIn(ctx, tx, entries, deployRepo); err != nil {
		return evalValues{}, err
	}
	if evals.FiguresOffered(repos, ranges.window, ranges.compare) {
		stored, err := evals.ReadStoredFiguresIn(ctx, tx, evalProjects(entries), entries, deployRepo, repos, ranges.window, ranges.compare)
		if err != nil {
			return evalValues{}, err
		}
		for index, entry := range entries {
			series, at, ok := stored.SeriesValues(entry, read.firsts[entry.Definition.EvalID], ranges.window, ranges.compare)
			if !ok {
				read.unstated = append(read.unstated, index)
				continue
			}
			read.values[index] = series
			if at != nil {
				read.earliest(*at)
			}
		}
	} else {
		for index := range entries {
			read.unstated = append(read.unstated, index)
		}
	}
	if len(read.unstated) == 0 {
		return read, nil
	}
	rest := make([]evals.Entry, len(read.unstated))
	for position, index := range read.unstated {
		rest[position] = entries[index]
	}
	if read.rows, err = evals.DayWindowIn(ctx, tx, evalProjects(rest), rest, deployRepo, repos, ranges.window, ranges.compare); err != nil {
		return evalValues{}, err
	}
	return read, nil
}

// earliest makes at v's oldest updated_at where it is older than the one v holds.
func (v *evalValues) earliest(at time.Time) {
	if v.oldest == nil || at.Before(*v.oldest) {
		v.oldest = &at
	}
}

// items is each of v's evals with its values, in the order read, and the answer's freshness: the
// oldest updated_at of the rows its values fold, and the refresher's age. It folds the day rows of
// the evals the stored figures did not answer (evals.SeriesValuesOf) and reads nothing, so it runs
// once the snapshot v was read in is committed.
func (v *evalValues) items() ([]evalItem, evalFreshness) {
	byEval := map[string][]evals.WindowRow{}
	for _, row := range v.rows {
		byEval[row.EvalID] = append(byEval[row.EvalID], row)
		v.earliest(row.UpdatedAt)
	}
	for _, index := range v.unstated {
		id := v.entries[index].Definition.EvalID
		v.values[index] = evals.SeriesValuesOf(v.entries[index], byEval[id], v.firsts[id], v.ranges.window, v.ranges.compare)
	}
	items := make([]evalItem, len(v.entries))
	for index, entry := range v.entries {
		items[index] = evalItem{
			Definition: entry.Definition,
			Owner:      evalOwnerOf(entry.Definition),
			Series:     v.values[index],
			Verdict:    evals.VerdictOf(entry, evals.WindowStatistics(v.values[index])),
		}
	}
	freshness := evalFreshness{RowsUpdatedAt: v.oldest}
	if v.aged {
		freshness.RefresherAgeSeconds = new(int(v.age / time.Second))
	}
	return items, freshness
}

// evalProjects is the projects entries are evals of, in the order they first appear.
func evalProjects(entries []evals.Entry) []string {
	var projects []string
	for _, entry := range entries {
		if !slices.Contains(projects, entry.Definition.ProjectKey) {
			projects = append(projects, entry.Definition.ProjectKey)
		}
	}
	return projects
}

func evalOwnerOf(definition evals.Definition) evalOwner {
	return evalOwner{Role: definition.OwnerRole, Person: definition.Person, WrittenBy: definition.WrittenBy}
}

// getProjectEval answers GET /api/v1/projects/{key}/evals/{slug}: one page of the eval's versions,
// newest first, read with its current definition and series (evals.EvalVersionsIn, which reads the
// page's versions alone), the newest stating the window's values as the list read states them
// (readEvalValues), and the method its section of the project's `evals-md` holds
// (evals.Store.Method). One answer, one snapshot, as the list read's (readEvalList): the project,
// the eval, its versions, the deploy repository (evals.DeployRepoIn) and the values' reads run in
// one read-only repeatable-read transaction, committed before the folds. It is begun on a
// connection mark of its own (store.ForConcurrentRead), so the method is read beside the values:
// the documents service answers it from the document's room where one is open, not from a
// database moment a snapshot can hold. A query key the route does not take is 400 INVALID_QUERY
// (undeclaredQueryKey), 404 PROJECT_NOT_FOUND for a project no row holds, 404 EVAL_NOT_FOUND for a
// slug no eval of it holds.
func (s *server) getProjectEval(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	read, err := parseEvalReadQuery(r.URL.Query(), evalDetailQueryKeys)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx := r.Context()
	project, slug := r.PathValue("key"), r.PathValue("slug")
	registry := s.registry()
	evalID := evals.EvalID(project, slug)
	snapshot, err := store.ForConcurrentRead(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := registry.BeginSnapshot(snapshot)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("begin the read of eval %s: %w", evalID, err))
		return
	}
	defer tx.Rollback(snapshot)
	if _, err := s.loadProject(snapshot, tx, project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	entry, versions, found, err := evals.EvalVersionsIn(snapshot, tx, evalID, read.limit, read.offset)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !found {
		s.writeHandlerError(w, evalNotFound(project, slug))
		return
	}
	deployRepo, err := evals.DeployRepoIn(snapshot, tx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer := evalDetailResponse{
		Items: []evalItem{}, Total: versions.Total, Limit: read.limit, Offset: read.offset,
		Window: newEvalRange(read.ranges.window), Compare: evalCompareRange(read.ranges.compare),
	}
	// The newest version states the window's values where the page holds it.
	var stated []evals.Entry
	if slices.ContainsFunc(versions.Items, func(version evals.Definition) bool { return version.Version == entry.Definition.Version }) {
		stated = []evals.Entry{entry}
	}
	var values evalValues
	var method string
	var held bool
	if err := store.ReadSideBySide(ctx,
		func(context.Context) (err error) {
			if values, err = readEvalValues(snapshot, tx, stated, deployRepo, read.repos, read.ranges, time.Now().UTC()); err != nil {
				return err
			}
			if err := tx.Commit(snapshot); err != nil {
				return fmt.Errorf("end the read of eval %s: %w", evalID, err)
			}
			return nil
		},
		func(ctx context.Context) (err error) {
			method, held, err = registry.Method(ctx, project, entry.Definition.DocBlock)
			return err
		},
	); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	newest, freshness := values.items()
	answer.Freshness = freshness
	for _, version := range versions.Items {
		if version.Version != entry.Definition.Version {
			answer.Items = append(answer.Items, evalItem{Definition: version, Owner: evalOwnerOf(version)})
			continue
		}
		answer.Items = append(answer.Items, newest...)
	}
	if held {
		answer.DocText = &method
	}
	WriteJSON(w, http.StatusOK, answer)
}

// evalNotFound is the 404 EVAL_NOT_FOUND of a slug no eval of project holds.
func evalNotFound(project, slug string) error {
	return errorf(http.StatusNotFound, codeEvalNotFound, "project %s holds no eval %q", project, slug)
}

// writeEvalError answers err: a registry refusal (*evals.Error) as its own body, {code, error}
// and for a readings write its refused rows, the first evals.MaxRefusedRows of them, and
// rows_omitted, how many more there are, every text in it bounded, so the answer the write's
// idempotency key keeps stays within the statement bound whatever the body; 404 for a project no
// row holds, 409 for a project whose `evals-md` slug names a file and 400 for every other;
// anything else as writeHandlerError does.
func (s *server) writeEvalError(w http.ResponseWriter, err error) {
	var refused *evals.Error
	if !errors.As(err, &refused) {
		s.writeHandlerError(w, err)
		return
	}
	status := http.StatusBadRequest
	switch refused.Code {
	case evals.CodeProjectNotFound:
		status = http.StatusNotFound
	case evals.CodeDocumentNotDocument:
		status = http.StatusConflict
	}
	WriteJSON(w, status, refused)
}

// logEvalReadings answers POST /api/v1/projects/{key}/evals/{slug}/readings: one batch of readings
// of one eval, stored in one transaction or not at all (evals.Store.WriteReadings), every refused
// row named by its index. A slug no eval holds registers its eval with the caller as owner (R3),
// and a reading under a computed eval's slug is stored and never folded into its value. The write
// needs an Idempotency-Key (400 IDEMPOTENCY_KEY_REQUIRED), so a retry is answered from the keyed
// layer (keyedWrites) rather than written twice (R4). It takes no query key but the `session`
// parameter every operation carries (actingPerson), once: a window means nothing to a write, so
// any other key, and that one named twice, is 400 INVALID_QUERY naming it (R7). The body's actor
// follows Dispatch's bearer rule (bearerSessionActor); the server sets recorded_by and
// recorded_at, and a row carrying either is refused (READING_FIELD_SERVER_SET). The day rows the
// write keeps record s.deps.Build as the build that wrote them.
func (s *server) logEvalReadings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	if err := undeclaredQueryKey(query, nil); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := repeatedQueryKey(query, []string{evalSessionQueryKey}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, present, _ := requestKeyHeader(r); !present {
		writeError(w, codeIdempotencyKeyRequired, http.StatusBadRequest, fmt.Sprintf(
			"a readings write names its intent in an %s header, so a retry is answered with the first write's answer rather than stored twice",
			contracts.IdempotencyKeyHeader))
		return
	}
	var body evalReadingsBody
	if _, err := decodeEvalBody(r, &body, evalReadingsBodyKeys); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, body.Actor)
	if !ok {
		return
	}
	written, err := s.registry().WriteReadings(r.Context(), evals.Batch{
		Project: r.PathValue("key"), Slug: r.PathValue("slug"), Rows: body.Readings, Caller: actor, BuiltBy: s.deps.Build,
	})
	if err != nil {
		s.writeEvalError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, evalReadingsResponse{
		Items: written.Readings, Total: len(written.Readings), Limit: len(written.Readings),
		Definition: written.Definition, Registered: written.Registered,
	})
}

// refuseReadingChange answers PUT and DELETE /api/v1/projects/{key}/evals/{slug}/readings/{id}
// with 405 READING_APPEND_ONLY, whatever the id names: a reading is never updated or deleted, and
// a correction is a new reading naming the one it replaces (R4). The resource allows no method,
// which an empty Allow header says.
func (s *server) refuseReadingChange(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	w.Header().Set("Allow", "")
	writeError(w, codeReadingAppendOnly, http.StatusMethodNotAllowed,
		"a reading is append-only: a correction is a new reading, POSTed to the eval's readings with replaces naming the reading it corrects")
}

// registerEval answers POST /api/v1/projects/{key}/evals: the next version of one eval's
// definition, and of the series the body names, written in one transaction with every field the
// body leaves out copied forward (evals.Store.Register). doc_text, when given and not the method
// the eval's section of the project's `evals-md` holds already, replaces that method. It answers
// 201 with what it wrote, a version of the definition or of a series, and 200 with the versions
// that stand where the body changed no field of the definition or of a series it names. A field
// the body names that no earlier write of the eval named is a change of the version's record of
// what a person set, so `target: null` on an eval whose target is already off is answered 201,
// and sent again 200 (evals.Definition's SetByPerson).
func (s *server) registerEval(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	change, supplied, err := decodeEvalRegistration(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, supplied)
	if !ok {
		return
	}
	change.Project, change.Caller = r.PathValue("key"), actor
	registered, err := s.registry().Register(r.Context(), change)
	if err != nil {
		s.writeEvalError(w, err)
		return
	}
	status := http.StatusOK
	if registered.Wrote {
		status = http.StatusCreated
	}
	WriteJSON(w, status, evalRegisterResponse{
		Items: []evalRegistered{{Definition: registered.Definition, Series: registered.Series, Wrote: registered.Wrote}},
		Total: 1, Limit: 1,
	})
}

// decodeEvalRegistration reads a register write's body into the registration it asks for: each
// field the body names is set, `null` included (a null target is off), and each it leaves out is
// left unset, so the registry copies it forward. The body is decoded twice, once into its type,
// which refuses a key no field spells exactly (decodeEvalBody), and once by key, which says which
// keys it named.
func decodeEvalRegistration(r *http.Request) (evals.Registration, *model.Actor, error) {
	var input evalRegisterInput
	raw, err := decodeEvalBody(r, &input, evalRegisterBodyKeys)
	if err != nil {
		return evals.Registration{}, nil, err
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(raw, &named); err != nil {
		return evals.Registration{}, nil, errorf(http.StatusBadRequest, "INVALID_JSON", "invalid JSON body: %v", err)
	}
	change := evals.Registration{
		Slug:            input.Slug,
		Name:            fieldOf(named, "name", input.Name),
		Question:        fieldOf(named, "question", input.Question),
		NumberRule:      fieldOf(named, "number_rule", input.NumberRule),
		Decision:        fieldOf(named, "decision", input.Decision),
		DocBlock:        fieldOf(named, "doc_block", input.DocBlock),
		DocText:         fieldOf(named, "doc_text", input.DocText),
		FailureModes:    fieldOf(named, "failure_modes", input.FailureModes),
		Unit:            fieldOf(named, "unit", input.Unit),
		Direction:       fieldOf(named, "direction", input.Direction),
		Target:          fieldOf(named, "target", input.Target),
		TargetInclusive: fieldOf(named, "target_inclusive", input.TargetInclusive),
		TargetJudges:    fieldOf(named, "target_judges", input.TargetJudges),
		OwnerRole:       fieldOf(named, "owner_role", input.OwnerRole),
		Person:          fieldOf(named, "person", input.Person),
		IssueKey:        fieldOf(named, "issue_key", input.IssueKey),
		Kind:            fieldOf(named, "kind", input.Kind),
		Source:          fieldOf(named, "source", input.Source),
		State:           fieldOf(named, "state", input.State),
		ReplacedBy:      fieldOf(named, "replaced_by", input.ReplacedBy),
		Links:           fieldOf(named, "links", evalLinks(input.Links)),
	}
	if len(input.Series) > 0 {
		var namedSeries []map[string]json.RawMessage
		if err := json.Unmarshal(named["series"], &namedSeries); err != nil {
			return evals.Registration{}, nil, errorf(http.StatusBadRequest, "INVALID_JSON", "invalid JSON body: series: %v", err)
		}
		for index, series := range input.Series {
			keys := namedSeries[index]
			change.Series = append(change.Series, evals.SeriesChange{
				Series:    series.Series,
				Unit:      fieldOf(keys, "unit", series.Unit),
				Role:      fieldOf(keys, "role", series.Role),
				IsPrimary: fieldOf(keys, "is_primary", series.IsPrimary),
				MinN:      fieldOf(keys, "min_n", series.MinN),
				Listed:    fieldOf(keys, "listed", series.Listed),
				Agg:       fieldOf(keys, "agg", series.Agg),
				Links:     fieldOf(keys, "links", evalLinks(series.Links)),
			})
		}
	}
	return change, input.Actor, nil
}

// fieldOf is value as a field of a register write: set where the body named key, unset where it
// left key out.
func fieldOf[T any](named map[string]json.RawMessage, key string, value T) evals.Field[T] {
	if _, set := named[key]; set {
		return evals.Set(value)
	}
	return evals.Field[T]{}
}

// evalLinks is links as the registry stores them, each recorded_at the registry's to set.
func evalLinks(links []evalLinkInput) []evals.Link {
	stored := make([]evals.Link, len(links))
	for index, link := range links {
		stored[index] = evals.Link{Rel: link.Rel, Href: link.Href, Label: link.Label}
	}
	return stored
}
