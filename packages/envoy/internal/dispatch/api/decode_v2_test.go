package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// widgetQuery is a made-up row's query, one field of each kind the decoder reads.
type widgetQuery struct {
	pageQuery
	Project []string            `query:"project"`
	Pinned  bool                `query:"pinned"`
	Issue   queryFilter[string] `query:"issue"`
	Sort    querySort           `query:"sort" enum:"priority,updated_at" default:"-updated_at"`
	Include []string            `query:"include,comma" enum:"thread,mark"`
	From    *time.Time          `query:"from"`
	Days    int                 `query:"days" default:"7" min:"1" max:"30"`
	State   string              `query:"state" enum:"open,closed" default:"open"`
}

// widgetQueryDeclares is every key widgetQuery declares, session among them, sorted: what a
// refusal of an undeclared key names, written out by hand.
var widgetQueryDeclares = []string{"days", "from", "include", "issue", "limit", "offset", "pinned", "project", "session", "sort", "state"}

// widgetBody is a made-up row's JSON body.
type widgetBody struct {
	Title string  `json:"title"`
	Owner *string `json:"owner"`
}

// widgetResolved and widgetAnswered are the two arms of a made-up state union.
type widgetResolved struct {
	State string `json:"state"`
}

type widgetAnswered struct {
	State string `json:"state"`
	Text  string `json:"text"`
}

// widgetUpload is a made-up upload's declared fields.
type widgetUpload struct {
	Name    string  `json:"name"`
	Summary *string `json:"summary"`
}

// madeUpV2Route is row mounted at /api/v2 alone, as register mounts a group's row.
func madeUpV2Route(row routeRow) apiRoute {
	row.Versions = onlyV2
	return mountRow(row)[0]
}

// widgetListRoute is a made-up list read whose query is widgetQuery.
func widgetListRoute(bounds listBounds) apiRoute {
	row := madeUpRow()
	row.Query, row.List = widgetQuery{}, bounds
	return madeUpV2Route(row)
}

// decodeQuery decodes target's query as route declares it, answering the query read or the
// refusal.
func decodeQuery(t *testing.T, route apiRoute, target string) (widgetQuery, error) {
	t.Helper()
	request, err := decodeV2Request(httptest.NewRequest(http.MethodGet, target, nil), route)
	if err != nil {
		return widgetQuery{}, err
	}
	return v2Query[widgetQuery](request), nil
}

// refusalCodeOf is the v2 name of the refusal err is, and "" for an error that is no refusal.
func refusalCodeOf(err error) string {
	var refusal *apiError
	if errors.As(err, &refusal) {
		return refusal.code.Name()
	}
	var unknown *queryKeyUnknownError
	if errors.As(err, &unknown) {
		return apicode.QueryKeyUnknown.Name()
	}
	return ""
}

// assertQueryRefused fails unless target is refused with code, its message naming each of names.
func assertQueryRefused(t *testing.T, route apiRoute, target string, code apicode.Code, names ...string) {
	t.Helper()
	_, err := decodeQuery(t, route, target)
	if refusalCodeOf(err) != code.Name() {
		t.Fatalf("%s: decoded with %v, want %s", target, err, code.Name())
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s: refused %q, which does not name %q", target, err.Error(), name)
		}
	}
}

// A query key the route does not declare is 400 QUERY_KEY_UNKNOWN naming the key and every key
// the route declares, sorted, session among them, whatever else the query holds: a value the
// route would refuse beside it does not hide it.
func TestTheV2DecoderRefusesAnUndeclaredKey(t *testing.T) {
	route := widgetListRoute(listBounds{})
	_, err := decodeQuery(t, route, "/api/v2/widgets?project=A&zz_undeclared=1&pinned=perhaps")
	var unknown *queryKeyUnknownError
	if !errors.As(err, &unknown) || unknown.key != "zz_undeclared" || !slices.Equal(unknown.declared, widgetQueryDeclares) {
		t.Fatalf("decoded with %#v, want the key zz_undeclared refused, naming %v", err, widgetQueryDeclares)
	}

	request := withAPIVersion(apiV2, func(w http.ResponseWriter, r *http.Request) {
		(&server{}).writeV2DecodeError(w, r, err)
	})
	response := httptest.NewRecorder()
	request(response, httptest.NewRequest(http.MethodGet, "/api/v2/widgets?zz_undeclared=1", nil))
	body := decodeBody[queryKeyUnknownBody](t, response)
	if response.Code != http.StatusBadRequest || body.Code != "QUERY_KEY_UNKNOWN" || body.Key != "zz_undeclared" ||
		!slices.Equal(body.Declared, widgetQueryDeclares) || body.Error == "" {
		t.Fatalf("answered %d %s, want 400 {code: QUERY_KEY_UNKNOWN, error, key, declared}", response.Code, response.Body.String())
	}

	// A route that declares no query takes session alone.
	bare := madeUpRow()
	bare.List = listBounds{}
	_, err = decodeQuery(t, madeUpV2Route(bare), "/api/v2/widgets?session=s-1&limit=5")
	if !errors.As(err, &unknown) || unknown.key != "limit" || !slices.Equal(unknown.declared, []string{"session"}) {
		t.Fatalf("a route with no query read limit: %#v, want it refused, naming session alone", err)
	}
}

// Every key the row declares is read by its type, and every key the query leaves out takes its
// default or its zero value; session is read beside them on every route, and requestSession
// answers the decoded one, where a request the decoder did not read names its raw query's.
func TestTheV2DecoderReadsWhatTheRowDeclares(t *testing.T) {
	route := widgetListRoute(listBounds{Default: 20, Max: 100})
	target := "/api/v2/widgets?project=A&project=B&pinned=true&issue=ACME-7&sort=priority&include=thread,mark" +
		"&from=2026-11-17T09:00:00%2B01:00&days=30&state=closed&limit=10&offset=40&session=%20s-1%20"
	request, err := decodeV2Request(httptest.NewRequest(http.MethodGet, target, nil), route)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := v2Query[widgetQuery](request)
	from := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	want := widgetQuery{
		pageQuery: pageQuery{Limit: 10, Offset: 40},
		Project:   []string{"A", "B"}, Pinned: true,
		Issue: queryFilter[string]{Set: true, Value: "ACME-7"},
		Sort:  querySort{Field: "priority"}, Include: []string{"thread", "mark"},
		From: &from, Days: 30, State: "closed",
	}
	if got.From == nil || !got.From.Equal(from) {
		t.Fatalf("from = %v, want %v", got.From, from)
	}
	got.From = want.From
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}
	// The raw query changed after the decoder read it: the session is still the decoded one.
	request.URL.RawQuery = "session=another"
	if session := requestSession(request); session != "s-1" {
		t.Fatalf("session = %q; want s-1 trimmed, read by the decoder", session)
	}
	undecoded := httptest.NewRequest(http.MethodGet, "/api/v1/widgets?session=%20s-2%20", nil)
	if session := requestSession(undecoded); session != "s-2" {
		t.Fatalf("session of a request the decoder did not read = %q; want s-2 trimmed, read off the raw query", session)
	}

	empty, err := decodeQuery(t, route, "/api/v2/widgets")
	if err != nil {
		t.Fatalf("decode an empty query: %v", err)
	}
	if empty.Limit != 20 || empty.Offset != 0 || empty.Sort != (querySort{Field: "updated_at", Descending: true}) ||
		empty.Days != 7 || empty.State != "open" || empty.Issue.Set || empty.From != nil || empty.Project != nil || empty.Pinned {
		t.Fatalf("an empty query decoded as %+v, want every default", empty)
	}
}

// A key given twice is refused unless its field is a slice, which takes every value given.
func TestTheV2DecoderRefusesARepeatedScalar(t *testing.T) {
	route := widgetListRoute(listBounds{})
	assertQueryRefused(t, route, "/api/v2/widgets?state=open&state=closed", apicode.InvalidQuery, "state", "one value")
	assertQueryRefused(t, route, "/api/v2/widgets?session=a&session=b", apicode.InvalidQuery, "session")
	assertQueryRefused(t, route, "/api/v2/widgets?sort=priority&sort=updated_at", apicode.InvalidQuery, "sort")
	if query, err := decodeQuery(t, route, "/api/v2/widgets?project=A&project=B"); err != nil || !slices.Equal(query.Project, []string{"A", "B"}) {
		t.Fatalf("a repeated slice key decoded %v, %v; want both values", query.Project, err)
	}
}

// A boolean is true or false and nothing else (R1.6): 1, yes, and an empty value are refused.
func TestTheV2DecoderRefusesABadBoolean(t *testing.T) {
	route := widgetListRoute(listBounds{})
	for _, value := range []string{"1", "TRUE", "yes", ""} {
		assertQueryRefused(t, route, "/api/v2/widgets?pinned="+value, apicode.InvalidQuery, "pinned", "true or false")
	}
	if query, err := decodeQuery(t, route, "/api/v2/widgets?pinned=false"); err != nil || query.Pinned {
		t.Fatalf("pinned=false decoded %v, %v", query.Pinned, err)
	}
}

// none is the unset value of a filter (R1.6): a filter left out is unset, one set to none selects
// the rows whose field is unset, and one set to a value selects that value.
func TestTheV2DecoderReadsNoneAsTheUnsetValue(t *testing.T) {
	route := widgetListRoute(listBounds{})
	for target, want := range map[string]queryFilter[string]{
		"/api/v2/widgets":              {},
		"/api/v2/widgets?issue=none":   {Set: true, None: true},
		"/api/v2/widgets?issue=ACME-1": {Set: true, Value: "ACME-1"},
	} {
		query, err := decodeQuery(t, route, target)
		if err != nil || query.Issue != want {
			t.Errorf("%s: issue = %+v, %v; want %+v", target, query.Issue, err, want)
		}
	}
}

// A sort names one of the row's sortable fields, ascending or after a - descending (R1.6).
func TestTheV2DecoderRefusesABadSort(t *testing.T) {
	route := widgetListRoute(listBounds{})
	for _, value := range []string{"title", "--priority", "+priority", "priority,updated_at", ""} {
		assertQueryRefused(t, route, "/api/v2/widgets?sort="+value, apicode.InvalidQuery, "sort", "priority", "updated_at")
	}
	if query, err := decodeQuery(t, route, "/api/v2/widgets?sort=-priority"); err != nil || query.Sort != (querySort{Field: "priority", Descending: true}) {
		t.Fatalf("sort=-priority decoded %+v, %v", query.Sort, err)
	}
}

// include is a comma list of the row's include members (R1.9), each of which must be one.
func TestTheV2DecoderRefusesAnUnknownIncludeMember(t *testing.T) {
	route := widgetListRoute(listBounds{})
	for _, value := range []string{"thread,bogus", "bogus", "thread,", ""} {
		assertQueryRefused(t, route, "/api/v2/widgets?include="+value, apicode.InvalidQuery, "include", "thread", "mark")
	}
	query, err := decodeQuery(t, route, "/api/v2/widgets?include=mark&include=thread")
	if err != nil || !slices.Equal(query.Include, []string{"mark", "thread"}) {
		t.Fatalf("include given twice decoded %v, %v", query.Include, err)
	}
}

// A time is RFC3339 (R1.5); a date alone, a Unix time and words are refused.
func TestTheV2DecoderRefusesABadTime(t *testing.T) {
	route := widgetListRoute(listBounds{})
	for _, value := range []string{"2026-11-17", "1763370000", "yesterday", "2026-11-17T09:00:00"} {
		assertQueryRefused(t, route, "/api/v2/widgets?from="+value, apicode.InvalidQuery, "from", "RFC3339")
	}
}

// A limit is held to the row's bounds, naming the bound (R1.4): a row that names none takes 50 and
// 200, a list whose bounds count (listBounds.Counts) takes 0 as well and nothing below it, and a
// catalogue read whole takes any limit of 1 or more, answering every item without one.
func TestTheV2DecoderRefusesALimitAboveTheMaximum(t *testing.T) {
	bounded := widgetListRoute(listBounds{Default: 20, Max: 100})
	assertQueryRefused(t, bounded, "/api/v2/widgets?limit=101", apicode.InvalidLimit, "100")
	for _, value := range []string{"0", "-1", "ten", "1.5", ""} {
		assertQueryRefused(t, bounded, "/api/v2/widgets?limit="+value, apicode.InvalidLimit, "100")
	}
	assertQueryRefused(t, bounded, "/api/v2/widgets?limit=5&limit=6", apicode.InvalidLimit, "100")
	if query, err := decodeQuery(t, bounded, "/api/v2/widgets?limit=100"); err != nil || query.Limit != 100 {
		t.Fatalf("limit=100 decoded %d, %v", query.Limit, err)
	}

	unbounded := widgetListRoute(listBounds{})
	assertQueryRefused(t, unbounded, "/api/v2/widgets?limit=201", apicode.InvalidLimit, "200")
	if query, err := decodeQuery(t, unbounded, "/api/v2/widgets"); err != nil || query.Limit != defaultListLimit {
		t.Fatalf("a row with no bounds answered limit %d, %v; want %d", query.Limit, err, defaultListLimit)
	}

	counting := widgetListRoute(listBounds{Default: 20, Max: 100, Counts: true})
	for _, value := range []string{"-1", "101", "ten"} {
		assertQueryRefused(t, counting, "/api/v2/widgets?limit="+value, apicode.InvalidLimit, "from 0 to 100")
	}
	for target, want := range map[string]int{"/api/v2/widgets": 20, "/api/v2/widgets?limit=0": 0, "/api/v2/widgets?limit=100": 100} {
		if query, err := decodeQuery(t, counting, target); err != nil || query.Limit != want {
			t.Errorf("%s on a list whose bounds count decoded limit %d, %v; want %d", target, query.Limit, err, want)
		}
	}

	whole := widgetListRoute(listBounds{Whole: true})
	assertQueryRefused(t, whole, "/api/v2/widgets?limit=0", apicode.InvalidLimit, "1 or more")
	for target, want := range map[string]int{"/api/v2/widgets": 0, "/api/v2/widgets?limit=100000": 100000} {
		if query, err := decodeQuery(t, whole, target); err != nil || query.Limit != want {
			t.Errorf("%s on a catalogue read whole decoded limit %d, %v; want %d", target, query.Limit, err, want)
		}
	}
}

// An offset is an integer of 0 or more (R1.4).
func TestTheV2DecoderRefusesANegativeOffset(t *testing.T) {
	route := widgetListRoute(listBounds{})
	for _, value := range []string{"-1", "first", ""} {
		assertQueryRefused(t, route, "/api/v2/widgets?offset="+value, apicode.InvalidOffset, "offset", "0 or more")
	}
	if query, err := decodeQuery(t, route, "/api/v2/widgets?offset=0"); err != nil || query.Offset != 0 {
		t.Fatalf("offset=0 decoded %d, %v", query.Offset, err)
	}
}

// madeUpStateRoute is a made-up transition whose body is a union keyed by state.
func madeUpStateRoute() apiRoute {
	row := madeUpRow()
	row.Method, row.Pattern, row.Kind = http.MethodPut, "/widgets/{id}/state", kindState
	row.Body = requestBody{Kind: bodyStateUnion, Arms: map[string]any{"resolved": widgetResolved{}, "answered": widgetAnswered{}}}
	row.Scope = scopeDeployment
	return madeUpV2Route(row)
}

// rawJSONRequest is a request carrying body, JSON text as written.
func rawJSONRequest(method, target, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

// A state body's state picks the arm it is decoded into; a state the union does not take, or none
// at all, is 400 STATE_INVALID naming the states it takes.
func TestTheV2DecoderRefusesAStateUnionWithAnUnknownState(t *testing.T) {
	route := madeUpStateRoute()
	for _, body := range []string{`{"state":"bogus"}`, `{"text":"no state"}`, `{"state":7}`} {
		_, err := decodeV2Request(rawJSONRequest(http.MethodPut, "/api/v2/widgets/w1/state", body), route)
		if refusalCodeOf(err) != "STATE_INVALID" || !strings.Contains(err.Error(), "answered, resolved") {
			t.Errorf("%s: decoded with %v, want STATE_INVALID naming answered, resolved", body, err)
		}
	}
	request, err := decodeV2Request(rawJSONRequest(http.MethodPut, "/api/v2/widgets/w1/state", `{"state":"answered","text":"Yes."}`), route)
	if err != nil {
		t.Fatalf("decode an answered body: %v", err)
	}
	if arm, ok := v2BodyArm(request).(widgetAnswered); !ok || arm.Text != "Yes." {
		t.Fatalf("the answered body decoded as %#v, want its own arm", v2BodyArm(request))
	}
}

// An upload's declared fields are read from its multipart body, and a field it does not declare
// is ignored, as an undeclared JSON member is; its file is read whole.
func TestTheV2DecoderIgnoresAnUndeclaredUploadField(t *testing.T) {
	row := madeUpRow()
	row.Method, row.Pattern = http.MethodPost, "/widgets/{id}/files"
	row.Body = requestBody{Kind: bodyUpload, Type: widgetUpload{}}
	row.Scope = scopeDeployment
	route := madeUpV2Route(row)
	body, contentType, err := apitest.MultipartUpload(map[string]string{
		"name": "notes.txt", "summary": "Two lines", "zz_undeclared": "ignored",
	}, "notes.txt", "text/plain", []byte("first\nsecond\n"))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v2/widgets/w1/files", body)
	request.Header.Set("Content-Type", contentType)
	decoded, err := decodeV2Request(request, route)
	if err != nil {
		t.Fatalf("an upload with an undeclared field was refused: %v", err)
	}
	fields := v2Body[widgetUpload](decoded)
	if fields.Name != "notes.txt" || fields.Summary == nil || *fields.Summary != "Two lines" {
		t.Fatalf("the upload's fields decoded as %+v", fields)
	}
	file := v2Upload(decoded)
	if file == nil || file.Name != "notes.txt" || file.ContentType != "text/plain" || string(file.Content) != "first\nsecond\n" {
		t.Fatalf("the upload's file decoded as %+v", file)
	}
}

// A JSON body's undeclared member is ignored (R4), and the body is put back as the bytes that
// were read, so the keyed layer behind the decoder fingerprints what was decoded.
func TestTheV2DecoderIgnoresAnUndeclaredBodyMember(t *testing.T) {
	row := madeUpRow()
	row.Method, row.Pattern = http.MethodPost, "/widgets"
	row.Body = requestBody{Kind: bodyJSON, Type: widgetBody{}}
	row.Scope = scopeDeployment
	route := madeUpV2Route(row)
	const sent = `{"title":"A widget","zz_undeclared":{"deep":[1,2]}}`
	request, err := decodeV2Request(rawJSONRequest(http.MethodPost, "/api/v2/widgets", sent), route)
	if err != nil {
		t.Fatalf("a body with an undeclared member was refused: %v", err)
	}
	if body := v2Body[widgetBody](request); body.Title != "A widget" || body.Owner != nil {
		t.Fatalf("the body decoded as %+v", body)
	}
	again, err := io.ReadAll(request.Body)
	if err != nil || !bytes.Equal(again, []byte(sent)) {
		t.Fatalf("the body read again is %q, %v; want the bytes sent", again, err)
	}
	var members map[string]json.RawMessage
	decoded, held := decodedV2(request)
	if !held || json.Unmarshal([]byte(sent), &members) != nil || len(decoded.members) != len(members) {
		t.Fatalf("the decoder kept members %v (a decoded request on it: %v), want every member sent", decoded.members, held)
	}
}

// A row whose query or body the decoder cannot read, or whose authority rule could never hold,
// is refused at mount (undecodableRoutes), naming what is wrong; so is a row the decoder does not
// read that declares authorities, which no gate would hold a request to: one of the INFRA-979
// tree, at either of its mounts.
func TestARowTheV2DecoderCannotReadRefusesToMount(t *testing.T) {
	if faults := undecodableRoutes(slices.Concat(mountRow(madeUpRow()), []apiRoute{widgetListRoute(listBounds{}), madeUpStateRoute()})); len(faults) > 0 {
		t.Fatalf("well-formed rows were refused: %q", faults)
	}
	type untagged struct {
		Project string
	}
	type sortWithoutDefault struct {
		Sort querySort `query:"sort" enum:"priority"`
	}
	type enumOnABoolean struct {
		Pinned bool `query:"pinned" enum:"true"`
	}
	type limitWithItsOwnBound struct {
		Limit int `query:"limit" max:"10"`
	}
	type sessionOfItsOwn struct {
		Session string `query:"session"`
	}
	type commaScalar struct {
		Include string `query:"include,comma"`
	}
	type badDefault struct {
		Days int `query:"days" default:"week"`
	}
	type unreadType struct {
		Ratio float64 `query:"ratio"`
	}
	person := routeScopeSet{"person"}
	for name, broken := range map[string]struct {
		change func(*routeRow)
		names  string
	}{
		"an untagged field":                {func(row *routeRow) { row.Query = untagged{} }, "carries no query tag"},
		"a sort with no default":           {func(row *routeRow) { row.Query = sortWithoutDefault{} }, "a sort names the order a query that names none is answered in"},
		"an enum on a boolean":             {func(row *routeRow) { row.Query = enumOnABoolean{} }, "an enum holds a string"},
		"a limit with a bound of its own":  {func(row *routeRow) { row.Query = limitWithItsOwnBound{} }, "the row's List bounds hold it"},
		"a session of its own":             {func(row *routeRow) { row.Query = sessionOfItsOwn{} }, "session is declared once for every /api/v2 route"},
		"a comma list that is no slice":    {func(row *routeRow) { row.Query = commaScalar{} }, "only a slice is a comma list"},
		"a default no value of its own":    {func(row *routeRow) { row.Query = badDefault{} }, `its default "week"`},
		"a type the decoder does not read": {func(row *routeRow) { row.Query = unreadType{} }, "the decoder reads string, bool, int, int64 and time.Time"},
		"a query that is a pointer":        {func(row *routeRow) { row.Query = &widgetQuery{} }, "is no struct"},
		"bounds with no default":           {func(row *routeRow) { row.Query, row.List = widgetQuery{}, listBounds{Max: 10} }, "its List bounds"},
		"a union with no arms":             {func(row *routeRow) { row.Body = requestBody{Kind: bodyStateUnion} }, "a union names its arms"},
		"a body that is a pointer":         {func(row *routeRow) { row.Body = requestBody{Kind: bodyJSON, Type: &widgetBody{}} }, "never nil and never a pointer"},
		"a rule on an undeclared key": {func(row *routeRow) {
			row.Query = widgetQuery{}
			row.Authorities = []authorityRule{{Where: authorityWhere{In: partQuery, Name: "waiting_on"}, Value: "me", Scopes: person}}
		}, "reads a query key the route's Query does not declare"},
		"a rule on a value the enum does not list": {func(row *routeRow) {
			row.Query = widgetQuery{}
			row.Authorities = []authorityRule{{Where: authorityWhere{In: partQuery, Name: "state"}, Value: "opne", Scopes: person}}
		}, "names a value the key's enum does not list"},
		"a rule on a member no type declares": {func(row *routeRow) {
			row.Body = requestBody{Kind: bodyJSON, Type: widgetBody{}}
			row.Authorities = []authorityRule{{Where: authorityWhere{In: partBody, Name: "colour"}, Scopes: person}}
		}, "reads a body member no type of the route's Body declares"},
		"a rule on a state no arm is": {func(row *routeRow) {
			row.Body = requestBody{Kind: bodyStateUnion, Arms: map[string]any{"resolved": widgetResolved{}}}
			row.Authorities = []authorityRule{{Where: authorityWhere{In: partBody, Name: "state"}, Value: "accepted", Scopes: person}}
		}, "names a state no arm of the union is"},
		"a rule with no scope": {func(row *routeRow) {
			row.Body = requestBody{Kind: bodyJSON, Type: widgetBody{}}
			row.Authorities = []authorityRule{{Where: authorityWhere{In: partBody, Name: "owner"}}}
		}, "names no member or key, or no scope"},
	} {
		row := madeUpRow()
		broken.change(&row)
		if faults := undecodableRoutes([]apiRoute{madeUpV2Route(row)}); len(faults) != 1 || !strings.Contains(faults[0], broken.names) {
			t.Errorf("%s: refused %q, want the row refused once, naming %q", name, faults, broken.names)
		}
	}

	infra979 := madeUpRow()
	infra979.OfINFRA979 = true
	infra979.Body = requestBody{Kind: bodyJSON, Type: widgetBody{}}
	infra979.Authorities = []authorityRule{{Where: authorityWhere{In: partBody, Name: "owner"}, Scopes: person}}
	faults := undecodableRoutes(mountRow(infra979))
	if len(faults) != 2 || !strings.HasPrefix(faults[0], "GET /api/v1/widgets: declares Authorities") ||
		!strings.HasPrefix(faults[1], "GET /api/v2/widgets: declares Authorities") {
		t.Fatalf("a row of the INFRA-979 tree that declares authorities was answered %q, want both of its mounts refused", faults)
	}
}

// widgetEcho is what the made-up row of TestABothVersionRowIsDecodedAndGatedAtBothPaths answers:
// what its handler read of the request through v2Query and v2Body.
type widgetEcho struct {
	State   string   `json:"state"`
	Days    int      `json:"days"`
	Include []string `json:"include"`
	Title   string   `json:"title"`
	Owner   *string  `json:"owner"`
}

// A routeRow with no Versions that declares a query, a body and an authority rule is served at
// both paths by its one declaration (R2), and each path reads, gates and answers it as /api/v2
// does (apiRoute.decoded): register mounts it (undecodableRoutes refuses nothing of it); a request
// both paths admit is answered alike, by a handler that reads its query and its body through
// v2Query and v2Body; a query key the row does not declare is 400 QUERY_KEY_UNKNOWN; and a body
// value that needs a scope the credential lacks is refused by the authority gate. Neither refusal
// runs the handler.
func TestABothVersionRowIsDecodedAndGatedAtBothPaths(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	row := madeUpRow()
	row.Method, row.Pattern, row.Kind = http.MethodPatch, "/widgets/{id}", kindItem
	row.Scope = scopeDeployment
	row.Query = widgetQuery{}
	row.Body = requestBody{Kind: bodyJSON, Type: widgetBody{}}
	row.Authorities = []authorityRule{{Where: authorityWhere{In: partBody, Name: "owner"}, Scopes: routeScopeSet{model.ScopePerson}}}
	var runs atomic.Int32
	row.Handler = func(w http.ResponseWriter, r *http.Request) {
		runs.Add(1)
		if decoded, read := decodedV2(r); !read || !decoded.bodyRead {
			WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "the decoder read no query or no body of " + r.Pattern})
			return
		}
		query, body := v2Query[widgetQuery](r), v2Body[widgetBody](r)
		WriteJSON(w, http.StatusOK, widgetEcho{State: query.State, Days: query.Days, Include: query.Include, Title: body.Title, Owner: body.Owner})
	}

	mounts := mountRow(row)
	if faults := undecodableRoutes(mounts); len(faults) > 0 {
		t.Fatalf("the row served in both versions was refused: %q", faults)
	}
	mux := http.NewServeMux()
	var paths []string
	for _, route := range mounts {
		mux.HandleFunc(route.Method+" "+route.Pattern, served.routeHandler(route))
		paths = append(paths, strings.Replace(route.Pattern, "{id}", "w1", 1))
	}
	if want := []string{"/api/v1/widgets/w1", "/api/v2/widgets/w1"}; !slices.Equal(paths, want) {
		t.Fatalf("the row is mounted at %q, want %q", paths, want)
	}
	scopeless := personalTokenOwnedBy(t, database, "alice")
	personOnly := personalTokenOwnedBy(t, database, "alice", model.ScopePerson)

	var answers []string
	for _, path := range paths {
		admitted := tokenRequest(t, mux, http.MethodPatch, path+"?days=3&include=thread", map[string]any{"title": "Renamed"}, scopeless, "")
		echo := decodeBody[widgetEcho](t, admitted)
		if admitted.Code != http.StatusOK || echo.State != "open" || echo.Days != 3 || !slices.Equal(echo.Include, []string{"thread"}) ||
			echo.Title != "Renamed" || echo.Owner != nil {
			t.Errorf("%s: a request the row admits was answered %d %s, want 200 the query and the body as the decoder read them",
				path, admitted.Code, admitted.Body.String())
		}
		answers = append(answers, admitted.Body.String())

		withScope := tokenRequest(t, mux, http.MethodPatch, path, map[string]any{"owner": "bob"}, personOnly, "")
		if owner := decodeBody[widgetEcho](t, withScope).Owner; withScope.Code != http.StatusOK || owner == nil || *owner != "bob" {
			t.Errorf("%s: a member that needs person, with it, was answered %d %s, want 200 naming the owner", path, withScope.Code, withScope.Body.String())
		}

		before := runs.Load()
		unknown := tokenRequest(t, mux, http.MethodPatch, path+"?zz_undeclared=1", map[string]any{"title": "Renamed"}, scopeless, "")
		refusal := decodeBody[queryKeyUnknownBody](t, unknown)
		if unknown.Code != http.StatusBadRequest || refusal.Code != "QUERY_KEY_UNKNOWN" || refusal.Key != "zz_undeclared" ||
			!slices.Equal(refusal.Declared, widgetQueryDeclares) {
			t.Errorf("%s: a query key the row does not declare was answered %d %s, want 400 QUERY_KEY_UNKNOWN naming it and %v",
				path, unknown.Code, unknown.Body.String(), widgetQueryDeclares)
		}
		assertGateRefused(t, path+": a member that needs person",
			tokenRequest(t, mux, http.MethodPatch, path, map[string]any{"title": "Renamed", "owner": "bob"}, scopeless, ""),
			model.ScopePerson, "the member owner")
		if ran := runs.Load() - before; ran != 0 {
			t.Errorf("%s: the handler ran %d times behind the two refusals, want none", path, ran)
		}
	}
	if answers[0] != answers[1] {
		t.Errorf("one request was answered %s at /api/v1 and %s at /api/v2, want one answer", answers[0], answers[1])
	}
}

// Every route the server's table mounts, test hooks included, is one undecodableRoutes refuses
// nothing of. The INFRA-979 tree's rows, which that tree's spec decides and whose handlers read
// their request as /api/v1 wrote it, are left out of the decoder and named by their /api/v1
// mounts; TestABothVersionRowIsDecodedAndGatedAtBothPaths proves that both mounts of every other
// row served in both versions are decoded and gated.
func TestEveryRowTheTableMountsIsDecodable(t *testing.T) {
	served := &server{deps: Deps{TestHooksEnabled: true}}
	routes := served.routes()
	if faults := undecodableRoutes(routes); len(faults) > 0 {
		t.Fatalf("%d route(s) the table mounts are undecodable:\n%s", len(faults), strings.Join(faults, "\n"))
	}
	var infra979 []string
	for _, route := range routes {
		if route.columns && route.OfINFRA979 && route.Version == apiV1 {
			infra979 = append(infra979, route.Method+" "+route.Pattern)
		}
	}
	rows := 0
	for _, build := range rowRegistry {
		if build(served).OfINFRA979 {
			rows++
		}
	}
	if len(infra979) != rows {
		t.Errorf("left out the /api/v1 mounts %q, want each of the %d OfINFRA979 rows in rowRegistry", infra979, rows)
	}
}

// renderedWidgetQuery is a made-up list's query whose include names one member, mark, that
// renders each listed row whole, and one, thread, that does not.
type renderedWidgetQuery struct {
	pageQuery
	Include []string `query:"include,comma" enum:"thread,mark" renders:"mark"`
}

// A list's include field names in its renders tag the members that render each listed row whole:
// a renders tag on a field that is no list, naming a member its enum does not hold, on a query that
// takes no limit, or on two fields is refused at mount (compileQueryField, compileV2Query). The
// decoder holds the page of a request that names such a member to includedPageMax before any
// handler runs (holdIncludedPage): a limit past it is 400 INVALID_LIMIT naming the bound and the
// member, a request that names no limit reads 20, or the row's default where that is smaller, and a
// limit past 20 without the member is read as given.
func TestTheRendersTagHoldsTheListsPage(t *testing.T) {
	type rendersOnAString struct {
		pageQuery
		Include string `query:"include" enum:"text" renders:"text"`
	}
	type rendersOutsideItsEnum struct {
		pageQuery
		Include []string `query:"include,comma" enum:"text" renders:"text,blocks"`
	}
	type rendersWithNoLimit struct {
		Include []string `query:"include,comma" enum:"text" renders:"text"`
	}
	type rendersTwice struct {
		pageQuery
		Include []string `query:"include,comma" enum:"text" renders:"text"`
		Expand  []string `query:"expand,comma" enum:"body" renders:"body"`
	}
	well := madeUpRow()
	well.Query = renderedWidgetQuery{}
	if faults := undecodableRoutes([]apiRoute{madeUpV2Route(well)}); len(faults) > 0 {
		t.Fatalf("a list whose include renders one member was refused: %q", faults)
	}
	for name, broken := range map[string]struct {
		query any
		names string
	}{
		"a renders tag on a field that is no list": {rendersOnAString{}, "renders names the include members that render each listed row, and this field is no list"},
		"a member its enum does not hold":          {rendersOutsideItsEnum{}, `renders names "blocks", which its enum "text" does not hold`},
		"a query that takes no limit":              {rendersWithNoLimit{}, `declares renders on the query key "include" and takes no limit`},
		"a renders tag on two fields":              {rendersTwice{}, `declares renders on the query keys "expand" and "include"`},
	} {
		row := madeUpRow()
		row.Query = broken.query
		if faults := undecodableRoutes([]apiRoute{madeUpV2Route(row)}); len(faults) != 1 || !strings.Contains(faults[0], broken.names) {
			t.Errorf("%s: refused %q, want the row refused once, naming %q", name, faults, broken.names)
		}
	}

	decode := func(t *testing.T, bounds listBounds, target string) (renderedWidgetQuery, error) {
		t.Helper()
		row := madeUpRow()
		row.Query, row.List = renderedWidgetQuery{}, bounds
		request, err := decodeV2Request(httptest.NewRequest(http.MethodGet, target, nil), madeUpV2Route(row))
		if err != nil {
			return renderedWidgetQuery{}, err
		}
		return v2Query[renderedWidgetQuery](request), nil
	}
	for _, target := range []string{"/api/v2/widgets?include=mark&limit=21", "/api/v2/widgets?include=thread,mark&limit=200"} {
		if _, err := decode(t, listBounds{}, target); refusalCodeOf(err) != apicode.InvalidLimit.Name() ||
			!strings.Contains(err.Error(), "limit must be one integer from 1 to 20 with include=mark, which renders each listed row") {
			t.Errorf("%s: decoded with %v, want 400 INVALID_LIMIT naming 1 to 20 and include=mark alone", target, err)
		}
	}
	for _, read := range []struct {
		bounds listBounds
		target string
		limit  int
	}{
		{listBounds{}, "/api/v2/widgets?include=mark", 20},
		{listBounds{}, "/api/v2/widgets?include=thread,mark", 20},
		{listBounds{}, "/api/v2/widgets?include=mark&limit=20", 20},
		{listBounds{}, "/api/v2/widgets?include=mark&limit=5", 5},
		{listBounds{}, "/api/v2/widgets?include=thread&limit=21", 21},
		{listBounds{}, "/api/v2/widgets?limit=21", 21},
		{listBounds{}, "/api/v2/widgets?include=thread", defaultListLimit},
		{listBounds{Default: 10, Max: 100}, "/api/v2/widgets?include=mark", 10},
	} {
		if query, err := decode(t, read.bounds, read.target); err != nil || query.Limit != read.limit {
			t.Errorf("%s on a list bounded %+v decoded limit %d, %v; want %d", read.target, read.bounds, query.Limit, err, read.limit)
		}
	}
}

// Every /api/v2 route the table mounts whose query names members that render each listed row (a
// renders tag) says so from that one declaration where a caller reads it, each worded from the
// row's own List as the decoder reads it: its description carries the list's own limit (limits)
// and the page with those members (includedPageText), and the document's limit parameter the same
// page and members. So a row cannot declare the members and leave its description or the document
// silent, nor word its page from bounds other than its List. The lists that declare them - an
// issue's and a project's artifacts, an issue's handoffs, and the three lists of asks - are among
// the routes walked.
func TestEveryListThatRendersItsRowsSaysItsPage(t *testing.T) {
	tree, _ := readOpenAPITreeOf(t, apiV2)
	document, _ := tree.(map[string]any)
	var rendering []string
	for _, route := range servedAt(apiV2, (&server{deps: Deps{TestHooksEnabled: true}}).routes()) {
		if !route.v2Bound() {
			continue
		}
		name := route.Method + " " + route.Pattern
		query, err := compileV2Query(route.Query)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if query.rendering == nil {
			continue
		}
		rendering = append(rendering, name)
		said := includedPageText(route.List, query.rendering.renders)
		least, fallback, most := route.List.limits()
		if described := fmt.Sprintf("limit %d to %d (default %d), and %s, and offset;", least, most, fallback, said); !strings.Contains(route.Description, described) {
			t.Errorf("%s names renders:%q on its include, and its description does not say %q", name, strings.Join(query.rendering.renders, ","), described)
		}
		parameters, _ := openAPIAt(document, "paths", route.Pattern, strings.ToLower(route.Method))["parameters"].([]any)
		limit := ""
		for _, parameter := range parameters {
			if declared, _ := parameter.(map[string]any); declared["name"] == "limit" && declared["in"] == "query" {
				limit, _ = declared["description"].(string)
			}
		}
		if !strings.Contains(limit, said) {
			t.Errorf("%s names renders:%q on its include, and the document's limit says %q, not %q", name, strings.Join(query.rendering.renders, ","), limit, said)
		}
	}
	for _, list := range []string{
		"GET /api/v2/issues/{key}/artifacts", "GET /api/v2/projects/{key}/artifacts", "GET /api/v2/issues/{key}/handoffs",
		"GET /api/v2/asks", "GET /api/v2/issues/{key}/asks", "GET /api/v2/artifacts/{id}/asks",
	} {
		if !slices.Contains(rendering, list) {
			t.Errorf("%s is no route whose query names members that render each listed row; the routes that do are %q", list, rendering)
		}
	}
}

// includedPageLimitText words a list's page from the bounds it is handed: every list that renders
// its rows declares listBounds{} today, so the walk above passes whether it reads them or not. A
// made-up list bounded at 10 and 100 says its own limit, 1 to 100 (default 10), and the page with
// the member that renders each listed row its own default under includedPageMax, 1 to 20 (default
// 10) (includedPageDefault).
func TestIncludedPageLimitTextWordsTheBoundsItIsHanded(t *testing.T) {
	const want = "limit 1 to 100 (default 10), and 1 to 20 (default 10) with include=mark, and offset;"
	if said := includedPageLimitText(listBounds{Default: 10, Max: 100}, renderedWidgetQuery{}); said != want {
		t.Errorf("includedPageLimitText(listBounds{Default: 10, Max: 100}, renderedWidgetQuery{}) = %q, want %q", said, want)
	}
}
