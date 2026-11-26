package api

// decode_v2.go is the one reader of a /api/v2 route's request (R4 of dispatch://INFRA-4071/spec),
// and of the /api/v1 path of a row served in both versions (apiRoute.decoded, R2). A route's row
// declares the types its query and its body are read into
// (routeRow.Query, routeRow.Body), and decodeV2Request reads them from those types alone: a query
// key the type does not declare is refused 400 QUERY_KEY_UNKNOWN before any lookup or write, and a
// body member it does not declare is ignored, as decodeJSON ignores one. The handler takes what
// was read from the request's context, v2Query[T](r) and v2Body[T](r), and reads no r.URL.Query(),
// no r.FormValue and no r.Form (TestNoV2HandlerReadsTheRawQuery).
//
// register reads a request in three places of a route's chain (routeHandler):
//
//   - the query just inside the route's scope gate (gateRouteScopes), so an undeclared key is
//     refused before any lookup or write; the handlers authenticate themselves, so an anonymous
//     caller that sends one on a route with no scopes is answered 400 too, which tells it nothing
//     the public route index does not;
//   - a JSON body - one object, or a union keyed by `state` or by a mark's `kind` - after the
//     project, session and login checks and before the authority gate and the keyed layer: it is
//     read once into a buffer bounded by the row's BodyLimit, decoded, and put back as r.Body, so
//     the authority gate (gateV2Authorities) runs on the decoded body, a refused write claims no
//     idempotency key, and the keyed layer fingerprints the very bytes that were decoded;
//   - an upload inside the keyed layer, which spools its body as it spools every upload's, just
//     around the handler.
//
// The query type
//
// A row's Query is a zero value of a struct whose exported fields each carry a `query:"<key>"`
// tag; an embedded struct with no tag (pageQuery) declares its own fields' keys. A field is read
// by its Go type:
//
//   - string, bool, int, int64 or time.Time: one value. A bool is `true` or `false` and nothing
//     else (R1.6), a time RFC3339 (R1.5: `from`, `to`, `compare_from`, `compare_to`,
//     `<field>_from`, `<field>_to`). A key given twice is refused: only a slice repeats.
//     `default:"…"` is the value of a query that leaves the key out, otherwise the zero value.
//   - a pointer to one of those: nil where the query leaves the key out.
//   - a slice of one of those: the key repeated (`?project=A&project=B`, R1.3), and with the tag
//     option `query:"<key>,comma"` each value a comma list as well (`?include=thread,mark`, R1.9).
//   - queryFilter[T]: a filter that may be left out, set to a value, or set to `none`, the unset
//     value of its field (R1.6).
//   - querySort, under the one key `sort`: `sort=<field>` or `sort=-<field>` (R1.6), its fields
//     the `enum` tag's list and the order a query that names none is answered in its `default`
//     tag, both required.
//   - `limit` and `offset`, the two int fields of pageQuery, which every list embeds: read against
//     the row's List bounds (R1.4), never against tags of their own.
//
// `enum:"a,b,c"` holds a string's value (each member of a slice's, a filter's value) to its list,
// and `min:"n"` and `max:"n"` an integer's. A value outside what its field takes is 400
// INVALID_QUERY naming the key and what it takes; a limit outside the row's bounds is 400
// INVALID_LIMIT naming the bound, and an offset that is no integer of 0 or more 400 INVALID_OFFSET.
//
// `renders:"a,b"` on a list's include field names the members of its enum that render each listed
// row whole - include=text on a list of artifacts, each document's text up to a MiB, or
// include=thread on a list of asks, each thread's replies, rewordings, answers and followers - so a
// request that names one reads a page of at most includedPageMax rows (holdIncludedPage): a limit it
// names past that is 400 INVALID_LIMIT naming the bound and the members it named, and one that
// names no limit reads the row's default, held to includedPageMax (includedPageDefault). A query
// declares it on one slice field at most, each member one its enum holds, and only beside a limit
// (pageQuery); the route's description says so (includedPageLimitText), and so does the document's
// limit.
//
// `session` is declared on every route the decoder reads, once, beside the row's own keys (R4):
// actingPerson and resolveIDPrefixes read the session it names through requestSession, the decoded
// request's on such a route, and the raw query's on every other.
//
// register refuses to mount a table whose rows declare a query or a body the decoder cannot read,
// or an authority rule that could never hold (undecodableRoutes), so a wrong tag fails the
// server's start and every test, never a request.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// pageQuery is a list's limit and offset (R1.4), which every list's query type embeds:
//
//	type askListQuery struct {
//		pageQuery
//		Project []string `query:"project"`
//	}
//
// The decoder reads both against the row's List bounds: Limit is an integer from 1 to List.Max,
// List.Default where the query names none (50 and 200 for a row whose List is zero, the bounds of
// a list that had none), and Offset an integer of 0 or more, 0 where the query names none. A row
// whose List is Counts takes a limit of 0 too, which its handler answers with no item. A row
// whose List is Whole - a catalogue read whole, as the route index is - takes any limit of 1 or
// more, and its Limit stays 0 where the query names none, which pageOf reads as every item.
type pageQuery struct {
	Limit  int `query:"limit"`
	Offset int `query:"offset"`
}

// The bounds of a list whose row names none (R1.4): those of the caller's own answers list
// (answers.go).
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// queryFilter is a filter a query may leave out, set to a value, or set to `none`, the unset
// value of the field it filters (R1.6): `?issue=none` lists the documents no issue holds. Set is
// false where the query leaves the key out; None is true where it names `none`; Value is the value
// it names otherwise, read as a field of type T is read, an `enum` tag holding it to its list.
type queryFilter[T any] struct {
	Set   bool
	None  bool
	Value T
}

// queryNone is the value a queryFilter reads as the unset value of its field (R1.6).
const queryNone = "none"

// filterParts is how the decoder reaches the three parts of a queryFilter of any T.
func (filter *queryFilter[T]) filterParts() (set, none *bool, value reflect.Value) {
	return &filter.Set, &filter.None, reflect.ValueOf(&filter.Value).Elem()
}

// queryFilterParts is what a *queryFilter[T] is to the decoder, whatever T is.
type queryFilterParts interface {
	filterParts() (set, none *bool, value reflect.Value)
}

// querySort is a list's order (R1.6): `sort=<field>` is Field ascending and `sort=-<field>` Field
// descending. The field is one of the row's sortable fields, which the field's `enum` tag lists,
// and the field's `default` tag is the order a query that names none is answered in, which the
// route's description documents.
type querySort struct {
	Field      string
	Descending bool
}

// uploadFilePart is the multipart part an upload's file is sent in, as uploadArtifactFor has
// always read it.
const uploadFilePart = "file"

// uploadFile is the file part of a multipart upload, read whole: its name, the content type its
// part names (empty where it names none), and its bytes.
type uploadFile struct {
	Name        string
	ContentType string
	Content     []byte
}

// v2Request is what the decoder read of one request to a route it reads (apiRoute.decoded).
// register's decoding layers put it on the request's context, the query layer first and each
// later layer a copy with its own part added, and the handler and the layers behind them read it
// through v2Query, v2Body, v2BodyArm, v2Upload and requestSession.
type v2Request struct {
	// query is the row's Query type decoded, a value of that type; nil for a row that declares
	// none.
	query any
	// values are the query's values by key, as the decoder accepted them, a comma list split into
	// its members: what an authority rule on a query value reads (gateV2Authorities).
	values map[string][]string
	// session is the query's `session`, trimmed, and "" where it names none.
	session string
	// body is the decoded body: a value of the row's Body.Type, or of the arm a union's
	// discriminating member picked. Nil until the body has been read, and for a row with none.
	body any
	// bodyRead marks a request whose body the decoder has read, an empty optional one included.
	bodyRead bool
	// members are a JSON body's top-level members as the request sent them, by name: what an
	// authority rule on a body member reads.
	members map[string]json.RawMessage
	// raw is a JSON body's bytes as the request sent them: what a handler reads that the decoded
	// value cannot say, such as whether a replace named its with (replaceFieldsRefusal).
	raw []byte
	// file is an upload's file part; nil for an upload sent as JSON and for every other body.
	file *uploadFile
}

// v2RequestKey is the request-context key of the *v2Request.
type v2RequestKey struct{}

// decodedV2 is what the decoder read of r, and false on a route it does not read: a route of the
// v1 table, a test hook, a row of the INFRA-979 tree.
func decodedV2(r *http.Request) (*v2Request, bool) {
	decoded, ok := r.Context().Value(v2RequestKey{}).(*v2Request)
	return decoded, ok
}

// withDecodedV2 is r carrying decoded.
func withDecodedV2(r *http.Request, decoded *v2Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), v2RequestKey{}, decoded))
}

// v2Query is the decoded query of a request to a route the decoder reads, as the row's Query type
// T. A handler that asks for another type than its row declares, or a route the decoder does not
// read, is a mistake in the table that the route's tests meet at once, so it panics naming both.
func v2Query[T any](r *http.Request) T {
	decoded, ok := decodedV2(r)
	if !ok {
		panic(fmt.Sprintf("v2Query[%T]: the decoder read no query of %s %s: only the handler of a route it reads reads one", *new(T), r.Method, r.Pattern))
	}
	query, ok := decoded.query.(T)
	if !ok {
		panic(fmt.Sprintf("v2Query[%T]: the row of %s %s declares its query as %T", *new(T), r.Method, r.Pattern, decoded.query))
	}
	return query
}

// v2Body is the decoded body of a request to a route the decoder reads, as T: the row's
// Body.Type, or on a union the arm the request's discriminating member picked (a handler that
// serves several arms switches on v2BodyArm instead). It panics, as v2Query does, for a type the
// request's body is not.
func v2Body[T any](r *http.Request) T {
	body, ok := v2BodyArm(r).(T)
	if !ok {
		panic(fmt.Sprintf("v2Body[%T]: the body of this request to %s %s was decoded as %T", *new(T), r.Method, r.Pattern, v2BodyArm(r)))
	}
	return body
}

// v2BodyArm is the decoded body of a request to a route the decoder reads as it was decoded: a
// value of the row's Body.Type, or of the union arm its discriminating member picked, for the
// handler of a union to switch on. It panics on a route whose body the decoder has not read.
func v2BodyArm(r *http.Request) any {
	decoded, ok := decodedV2(r)
	if !ok || !decoded.bodyRead {
		panic(fmt.Sprintf("v2BodyArm: the decoder read no body of %s %s: only a route it reads that declares one has one", r.Method, r.Pattern))
	}
	return decoded.body
}

// v2BodyBytes is the JSON body of a request to a route the decoder reads, as the request sent its
// bytes: nil for an optional body sent empty and for an upload. It panics, as v2BodyArm does, on a
// route whose body the decoder has not read.
func v2BodyBytes(r *http.Request) []byte {
	decoded, ok := decodedV2(r)
	if !ok || !decoded.bodyRead {
		panic(fmt.Sprintf("v2BodyBytes: the decoder read no body of %s %s", r.Method, r.Pattern))
	}
	return decoded.raw
}

// v2Upload is the file part of an upload to a route the decoder reads, and nil for an upload sent
// as the JSON object its row's Body.Type describes. Its declared fields are v2Body's.
func v2Upload(r *http.Request) *uploadFile {
	decoded, ok := decodedV2(r)
	if !ok || !decoded.bodyRead {
		panic(fmt.Sprintf("v2Upload: the decoder read no upload of %s %s", r.Method, r.Pattern))
	}
	return decoded.file
}

// requestSession is the session r names in its `session` query parameter, trimmed, "" where it
// names none: on a route the decoder reads, the one it decoded (R4); on every other - a route of
// the v1 table, a test hook, a row of the INFRA-979 tree - the raw query's, as it always was
// (actingPerson, resolveIDPrefixes).
func requestSession(r *http.Request) string {
	if request, decoded := decodedV2(r); decoded {
		return request.session
	}
	return strings.TrimSpace(r.URL.Query().Get("session"))
}

// v2Bound reports whether the rules of /api/v2 bind route: a routeRow's mount at /api/v2, but a
// row of the INFRA-979 tree (OfINFRA979), which that tree's own spec decides, and a test hook,
// which no rule of the parent's binds (dispatch://INFRA-2946/spec, R1). Only such a route is
// keyed on every write method (R8, keyedMethod) and carries the v2 document's members of a row
// (openAPIV2Members). The decoder reads it and the authority gate holds it, as they do every
// route decoded names.
func (route apiRoute) v2Bound() bool {
	return route.Version == apiV2 && route.decoded()
}

// decoded reports whether the decoder reads route's request (R4) and the authority gate holds it
// to the authorities its row declares (R1.8): every mount of a routeRow outside the INFRA-979
// tree, at /api/v2 and at /api/v1 alike. A routeRow is mounted at /api/v1 only where it is served
// in both versions, so the /api/v1 mounts it names are such a row's: one a later change adds has
// no older /api/v1 answer to keep, and its one declaration is read, gated and answered at both
// paths as at /api/v2 (R2). Its /api/v1 PUT and DELETE take no Idempotency-Key, as every /api/v1
// route's (keyedMethod reads v2Bound). A route of the v1 table, a test hook and a row of the
// INFRA-979 tree read their request as /api/v1 wrote it.
func (route apiRoute) decoded() bool {
	return route.columns && !route.OfINFRA979
}

// v2Decoder is a route's query and body declarations, read once when the route is mounted.
type v2Decoder struct {
	route apiRoute
	query v2QuerySchema
}

// newV2Decoder reads route's declarations, and refuses one the decoder cannot read: the error
// names the field or the arm and what is wrong with it. A route built with no body bound - a
// test's hand-made apiRoute - is held to the bound its Body takes (routeRow.bodyLimit), as
// mountRow holds every mounted row.
func newV2Decoder(route apiRoute) (*v2Decoder, error) {
	query, err := compileV2Query(route.Query)
	if err != nil {
		return nil, fmt.Errorf("its Query: %w", err)
	}
	if _, pages := query.field("limit"); pages && !route.List.Whole && route.List != (listBounds{}) &&
		(route.List.Default < 1 || route.List.Max < route.List.Default) {
		return nil, fmt.Errorf("its List bounds %+v: a list's default limit is 1 or more, and its largest limit no smaller", route.List)
	}
	if err := checkV2Body(route.Body); err != nil {
		return nil, fmt.Errorf("its Body: %w", err)
	}
	if route.BodyLimit.TooLarge == nil {
		route.BodyLimit = route.bodyLimit()
	}
	return &v2Decoder{route: route, query: query}, nil
}

// mustV2Decoder is newV2Decoder for a route register mounts, whose declarations undecodableRoutes
// has already held to what the decoder reads: a route that reaches here undecodable is a test's
// made-up row, and it panics naming the fault.
func mustV2Decoder(route apiRoute) *v2Decoder {
	decoder, err := newV2Decoder(route)
	if err != nil {
		panic(fmt.Sprintf("%s %s: %v", route.Method, route.Pattern, err))
	}
	return decoder
}

// decodeV2Request reads r as route's row declares it - its query, then its body, a JSON body, a
// union or an upload alike - and answers r carrying what it read (v2Query, v2Body), or the
// refusal of a request it cannot read. It is the decoder whole, in one call, as a test drives it;
// register runs its parts at their own places in the chain (routeHandler), and writes a refusal
// with writeV2DecodeError.
func decodeV2Request(r *http.Request, route apiRoute) (*http.Request, error) {
	decoder, err := newV2Decoder(route)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", route.Method, route.Pattern, err)
	}
	r, err = decoder.readQuery(r)
	if err != nil {
		return r, err
	}
	return decoder.readBody(r)
}

// queryKeyUnknownError is a query key the route does not declare (R4), which writeV2DecodeError
// answers 400 QUERY_KEY_UNKNOWN with queryKeyUnknownBody.
type queryKeyUnknownError struct {
	key      string
	declared []string
}

func (e *queryKeyUnknownError) Error() string {
	return fmt.Sprintf("this route does not declare the query key %q; it declares %s", e.key, strings.Join(e.declared, ", "))
}

// queryKeyUnknownBody is the body of 400 QUERY_KEY_UNKNOWN: errorBody's two members, the key the
// route does not declare, and every key it declares, sorted, `session` among them (R4).
type queryKeyUnknownBody struct {
	Code     string   `json:"code"`
	Error    string   `json:"error"`
	Key      string   `json:"key"`
	Declared []string `json:"declared"`
}

// writeV2DecodeError answers a request the decoder refused: 400 QUERY_KEY_UNKNOWN with the key and
// the declared keys, and every other refusal as writeHandlerError writes it.
func (s *server) writeV2DecodeError(w http.ResponseWriter, r *http.Request, err error) {
	var unknown *queryKeyUnknownError
	if errors.As(err, &unknown) {
		name, status := refusalPair(r, apicode.QueryKeyUnknown)
		WriteJSON(w, status, queryKeyUnknownBody{Code: name, Error: unknown.Error(), Key: unknown.key, Declared: unknown.declared})
		return
	}
	s.writeHandlerError(w, r, err)
}

// readV2Query is the decoder's query layer, which register wraps just inside the route's scope
// gate.
func (s *server) readV2Query(decoder *v2Decoder, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		decoded, err := decoder.readQuery(r)
		if err != nil {
			s.writeV2DecodeError(w, r, err)
			return
		}
		handler(w, decoded)
	}
}

// readV2Body is the decoder's JSON body layer, which register wraps after the project, session and
// login checks and outside the authority gate and the keyed layer; a route whose body is no JSON
// body - none, an upload, bytes it reads itself - is returned unwrapped.
func (s *server) readV2Body(decoder *v2Decoder, handler http.HandlerFunc) http.HandlerFunc {
	switch decoder.route.Body.Kind {
	case bodyJSON, bodyStateUnion, bodyKindUnion:
	default:
		return handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		decoded, err := decoder.readBody(r)
		if err != nil {
			s.writeV2DecodeError(w, r, err)
			return
		}
		handler(w, decoded)
	}
}

// readV2Upload is the decoder's upload layer, which register wraps just around the handler of an
// upload route, inside the keyed layer that spooled the upload's body; every other route is
// returned unwrapped.
func (s *server) readV2Upload(decoder *v2Decoder, handler http.HandlerFunc) http.HandlerFunc {
	if decoder.route.Body.Kind != bodyUpload {
		return handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		decoded, err := decoder.readBody(r)
		if err != nil {
			s.writeV2DecodeError(w, r, err)
			return
		}
		handler(w, decoded)
	}
}

// queryFieldKind is how the decoder reads one field of a query type.
type queryFieldKind int

const (
	// queryOne is a field of one value: a scalar.
	queryOne queryFieldKind = iota
	// queryOptional is a pointer to a scalar, nil where the query leaves its key out.
	queryOptional
	// queryList is a slice of scalars: the key repeated, or a comma list.
	queryList
	// queryFilterValue is a queryFilter[T].
	queryFilterValue
	// querySortValue is the querySort under `sort`.
	querySortValue
	// queryLimit and queryOffset are pageQuery's two fields.
	queryLimit
	queryOffset
)

// queryField is one key of a query type, as the decoder reads it.
type queryField struct {
	key   string
	index []int
	kind  queryFieldKind
	// scalar is the type each value is read into: the field's own, its element's, or a filter's
	// Value's.
	scalar reflect.Type
	// comma is a list each of whose values is also a comma list.
	comma bool
	// enum is the values a string takes, or the fields a sort orders by; empty for any.
	enum []string
	// fallback is the value of a query that leaves the key out, read once when the route is
	// mounted; hasFallback is false where the field takes its zero value.
	fallback    reflect.Value
	hasFallback bool
	// min and max hold an integer to a bound, where the field's tags name one.
	min, max *int64
	// renders is what a list's `renders` tag names: the members of its enum that render each
	// listed row whole, which hold the page of a request that names one to includedPageMax
	// (holdIncludedPage); empty for every other field.
	renders []string
}

// v2QuerySchema is one query type's keys, sorted, and the type itself; a nil type is a row that
// declares no query, which takes `session` alone.
type v2QuerySchema struct {
	typ    reflect.Type
	fields []queryField
	// rendering is the one field of fields whose renders names members, a list's include, and
	// nil for a query that declares none.
	rendering *queryField
}

var (
	timeType      = reflect.TypeFor[time.Time]()
	querySortType = reflect.TypeFor[querySort]()
	filterParts   = reflect.TypeFor[queryFilterParts]()
)

// compileV2Query reads a row's Query into the schema the decoder reads requests by, and refuses a
// declaration it cannot read: among them a `renders` tag on two fields, or on a query that takes
// no limit (compileQueryField refuses one on a field that is no list, or naming a member its enum
// does not hold).
func compileV2Query(query any) (v2QuerySchema, error) {
	if query == nil {
		return v2QuerySchema{}, nil
	}
	typ := reflect.TypeOf(query)
	if typ.Kind() != reflect.Struct {
		return v2QuerySchema{}, fmt.Errorf("%T is no struct: a query is a zero value of a struct whose fields carry query tags", query)
	}
	schema := v2QuerySchema{typ: typ}
	if err := schema.collect(typ, nil); err != nil {
		return v2QuerySchema{}, fmt.Errorf("%s: %w", typ, err)
	}
	slices.SortFunc(schema.fields, func(left, right queryField) int { return strings.Compare(left.key, right.key) })
	for index := 1; index < len(schema.fields); index++ {
		if schema.fields[index].key == schema.fields[index-1].key {
			return v2QuerySchema{}, fmt.Errorf("%s declares the query key %q twice", typ, schema.fields[index].key)
		}
	}
	for index := range schema.fields {
		field := &schema.fields[index]
		if len(field.renders) == 0 {
			continue
		}
		if schema.rendering != nil {
			return v2QuerySchema{}, fmt.Errorf("%s declares renders on the query keys %q and %q: one include names the members that render each listed row", typ, schema.rendering.key, field.key)
		}
		schema.rendering = field
	}
	if _, pages := schema.field("limit"); schema.rendering != nil && !pages {
		return v2QuerySchema{}, fmt.Errorf("%s declares renders on the query key %q and takes no limit: only a list (pageQuery) holds its page to includedPageMax", typ, schema.rendering.key)
	}
	return schema, nil
}

// collect adds the fields of typ, reached by index from the query type, an embedded struct's own.
func (schema *v2QuerySchema) collect(typ reflect.Type, index []int) error {
	for position := range typ.NumField() {
		field := typ.Field(position)
		at := append(slices.Clone(index), position)
		tag, tagged := field.Tag.Lookup("query")
		if field.Anonymous && !tagged && field.Type.Kind() == reflect.Struct {
			if err := schema.collect(field.Type, at); err != nil {
				return err
			}
			continue
		}
		if !field.IsExported() {
			continue
		}
		if !tagged {
			return fmt.Errorf("field %s carries no query tag", field.Name)
		}
		compiled, err := compileQueryField(field, tag)
		if err != nil {
			return fmt.Errorf("field %s: %w", field.Name, err)
		}
		compiled.index = at
		schema.fields = append(schema.fields, compiled)
	}
	return nil
}

// compileQueryField reads one field's tags against its type.
func compileQueryField(field reflect.StructField, tag string) (queryField, error) {
	key, option, _ := strings.Cut(tag, ",")
	compiled := queryField{key: key, comma: option == "comma"}
	switch {
	case key == "" || key == "-":
		return queryField{}, errors.New("its query tag names no key")
	case key == "session":
		return queryField{}, errors.New("session is declared once for every /api/v2 route, beside the row's own keys; a row's query does not declare it")
	case option != "" && option != "comma":
		return queryField{}, fmt.Errorf("its query tag's option %q is none the decoder reads (comma)", option)
	}
	if values, ok := field.Tag.Lookup("enum"); ok {
		compiled.enum = strings.Split(values, ",")
	}
	if members, ok := field.Tag.Lookup("renders"); ok {
		if field.Type.Kind() != reflect.Slice {
			return queryField{}, errors.New("renders names the include members that render each listed row, and this field is no list (a slice)")
		}
		compiled.renders = strings.Split(members, ",")
		for _, member := range compiled.renders {
			if !slices.Contains(compiled.enum, member) {
				return queryField{}, fmt.Errorf("renders names %q, which its enum %q does not hold", member, field.Tag.Get("enum"))
			}
		}
	}
	typ := field.Type
	switch {
	case key == "limit" || key == "offset":
		if typ.Kind() != reflect.Int || field.Tag.Get("default") != "" || field.Tag.Get("min") != "" || field.Tag.Get("max") != "" || compiled.enum != nil {
			return queryField{}, fmt.Errorf("%s is an int with no tags of its own: the row's List bounds hold it (pageQuery)", key)
		}
		compiled.kind, compiled.scalar = queryLimit, typ
		if key == "offset" {
			compiled.kind = queryOffset
		}
		return compiled, nil
	case typ == querySortType || key == "sort":
		return compileQuerySort(compiled, field)
	case reflect.PointerTo(typ).Implements(filterParts):
		compiled.kind = queryFilterValue
		_, _, value := reflect.New(typ).Interface().(queryFilterParts).filterParts()
		compiled.scalar = value.Type()
	case typ.Kind() == reflect.Pointer:
		compiled.kind, compiled.scalar = queryOptional, typ.Elem()
	case typ.Kind() == reflect.Slice:
		compiled.kind, compiled.scalar = queryList, typ.Elem()
	default:
		compiled.kind, compiled.scalar = queryOne, typ
	}
	if compiled.comma && compiled.kind != queryList {
		return queryField{}, errors.New("only a slice is a comma list")
	}
	if !queryScalar(compiled.scalar) {
		return queryField{}, fmt.Errorf("its values are %s: the decoder reads string, bool, int, int64 and time.Time", compiled.scalar)
	}
	if compiled.enum != nil && compiled.scalar.Kind() != reflect.String {
		return queryField{}, errors.New("an enum holds a string, and its values are no string")
	}
	for _, bound := range []struct {
		tag  string
		into **int64
	}{{"min", &compiled.min}, {"max", &compiled.max}} {
		raw, ok := field.Tag.Lookup(bound.tag)
		if !ok {
			continue
		}
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || compiled.scalar.Kind() == reflect.String || compiled.scalar.Kind() == reflect.Bool || compiled.scalar == timeType {
			return queryField{}, fmt.Errorf("%s:%q bounds an integer, and this one is no integer or bounds none", bound.tag, raw)
		}
		*bound.into = &parsed
	}
	if fallback, ok := field.Tag.Lookup("default"); ok {
		if compiled.kind != queryOne {
			return queryField{}, errors.New("only a field of one value takes a default: a pointer, a slice and a filter are absent where the query leaves them out")
		}
		value := reflect.New(compiled.scalar).Elem()
		if err := compiled.parse(fallback, value); err != nil {
			return queryField{}, fmt.Errorf("its default %q is none of its values: %w", fallback, err)
		}
		compiled.fallback, compiled.hasFallback = value, true
	}
	return compiled, nil
}

// compileQuerySort reads the sort field's enum and default.
func compileQuerySort(compiled queryField, field reflect.StructField) (queryField, error) {
	switch {
	case field.Type != querySortType || compiled.key != "sort":
		return queryField{}, errors.New("a list's order is a querySort under the one key sort (R1.6)")
	case compiled.comma:
		return queryField{}, errors.New("a sort names one field")
	case len(compiled.enum) == 0:
		return queryField{}, errors.New("a sort lists the fields it orders by in an enum tag")
	}
	compiled.kind, compiled.scalar = querySortValue, querySortType
	fallback, ok := field.Tag.Lookup("default")
	if !ok {
		return queryField{}, errors.New("a sort names the order a query that names none is answered in, in a default tag (R1.6)")
	}
	value := reflect.New(querySortType).Elem()
	if err := compiled.parse(fallback, value); err != nil {
		return queryField{}, fmt.Errorf("its default %q: %w", fallback, err)
	}
	compiled.fallback, compiled.hasFallback = value, true
	return compiled, nil
}

// queryScalar reports whether the decoder reads one value of typ.
func queryScalar(typ reflect.Type) bool {
	if typ == timeType {
		return true
	}
	switch typ.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int64:
		return true
	}
	return false
}

// declared is every key the schema's route declares, `session` among them, sorted.
func (schema v2QuerySchema) declared() []string {
	keys := []string{"session"}
	for _, field := range schema.fields {
		keys = append(keys, field.key)
	}
	slices.Sort(keys)
	return keys
}

// field is the schema's field of key.
func (schema v2QuerySchema) field(key string) (queryField, bool) {
	at, found := slices.BinarySearchFunc(schema.fields, key, func(field queryField, key string) int {
		return strings.Compare(field.key, key)
	})
	if !found {
		return queryField{}, false
	}
	return schema.fields[at], true
}

// readQuery decodes r's query into the row's Query type and answers r carrying it. Every key is
// judged before any value is read, so an undeclared key is refused whatever else the query holds,
// and the page of a request whose include names a member that renders each listed row is held to
// includedPageMax once every key is read (holdIncludedPage), before any handler reads anything.
func (d *v2Decoder) readQuery(r *http.Request) (*http.Request, error) {
	given, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return r, errorf(apicode.InvalidQuery, "the query is no query string the decoder can read: %v", err)
	}
	for _, key := range slices.Sorted(maps.Keys(given)) {
		if _, declared := d.query.field(key); !declared && key != "session" {
			return r, &queryKeyUnknownError{key: key, declared: d.query.declared()}
		}
	}
	decoded := &v2Request{values: map[string][]string{}}
	if sessions := given["session"]; len(sessions) > 0 {
		if len(sessions) > 1 {
			return r, errorf(apicode.InvalidQuery, "session is given %d times; it names one session", len(sessions))
		}
		decoded.session = strings.TrimSpace(sessions[0])
		decoded.values["session"] = sessions
	}
	if d.query.typ != nil {
		value := reflect.New(d.query.typ).Elem()
		for _, field := range d.query.fields {
			values, present := given[field.key]
			accepted, err := field.decode(values, present, value.FieldByIndex(field.index), d.route.List)
			if err != nil {
				return r, err
			}
			if present {
				decoded.values[field.key] = accepted
			}
		}
		if err := d.query.holdIncludedPage(value, decoded.values, d.route.List); err != nil {
			return r, err
		}
		decoded.query = value.Interface()
	}
	return withDecodedV2(r, decoded), nil
}

// decode reads the values the query gives field (none where present is false) into into, and
// answers them as the decoder accepted them, a comma list split into its members.
func (field queryField) decode(values []string, present bool, into reflect.Value, bounds listBounds) ([]string, error) {
	switch field.kind {
	case queryLimit:
		return values, field.decodeLimit(values, present, into, bounds)
	case queryOffset:
		if !present {
			return nil, nil
		}
		offset, err := strconv.ParseInt(soleValue(values), 10, 0)
		if len(values) != 1 || err != nil || offset < 0 {
			return nil, errorf(apicode.InvalidOffset, "offset must be one integer of 0 or more")
		}
		into.SetInt(offset)
		return values, nil
	case queryList:
		if !present {
			return nil, nil
		}
		var members []string
		for _, value := range values {
			if field.comma {
				members = append(members, strings.Split(value, ",")...)
			} else {
				members = append(members, value)
			}
		}
		list := reflect.MakeSlice(into.Type(), len(members), len(members))
		for index, member := range members {
			if err := field.parse(member, list.Index(index)); err != nil {
				return nil, err
			}
		}
		into.Set(list)
		return members, nil
	}
	if !present {
		if field.hasFallback {
			into.Set(field.fallback)
		}
		return nil, nil
	}
	if len(values) != 1 {
		return nil, errorf(apicode.InvalidQuery, "%s is given %d times; it takes one value", field.key, len(values))
	}
	switch field.kind {
	case queryOptional:
		value := reflect.New(field.scalar)
		if err := field.parse(values[0], value.Elem()); err != nil {
			return nil, err
		}
		into.Set(value)
	case queryFilterValue:
		set, none, value := into.Addr().Interface().(queryFilterParts).filterParts()
		*set = true
		if values[0] == queryNone {
			*none = true
		} else if err := field.parse(values[0], value); err != nil {
			return nil, err
		}
	default:
		if err := field.parse(values[0], into); err != nil {
			return nil, err
		}
	}
	return values, nil
}

// decodeLimit reads a list's limit against the row's bounds (R1.4).
func (field queryField) decodeLimit(values []string, present bool, into reflect.Value, bounds listBounds) error {
	least, fallback, most := bounds.limits()
	if !present {
		into.SetInt(int64(fallback))
		return nil
	}
	limit, err := strconv.ParseInt(soleValue(values), 10, 0)
	switch {
	case bounds.Whole && (len(values) != 1 || err != nil || limit < int64(least)):
		return errorf(apicode.InvalidLimit, "limit must be one integer of 1 or more")
	case !bounds.Whole && (len(values) != 1 || err != nil || limit < int64(least) || limit > int64(most)):
		return errorf(apicode.InvalidLimit, "limit must be one integer from %d to %d", least, most)
	}
	into.SetInt(limit)
	return nil
}

// holdIncludedPage holds the page of query, a value of the schema's type the decoder read with
// values, to includedPageMax where its include names a member that renders each listed row (the
// rendering field's `renders` tag): a limit the query names past it is 400 INVALID_LIMIT naming the
// bound and the members it named, and a query that names no limit reads includedPageDefault.
func (schema v2QuerySchema) holdIncludedPage(query reflect.Value, values map[string][]string, bounds listBounds) error {
	if schema.rendering == nil {
		return nil
	}
	var named []string
	for _, member := range schema.rendering.renders {
		if slices.Contains(values[schema.rendering.key], member) {
			named = append(named, member)
		}
	}
	if len(named) == 0 {
		return nil
	}
	field, _ := schema.field("limit")
	limit := query.FieldByIndex(field.index)
	switch _, given := values["limit"]; {
	case given && limit.Int() > includedPageMax:
		return errorf(apicode.InvalidLimit, "limit must be one integer from 1 to %d with %s%s",
			includedPageMax, includeMembersText(named, "and"), rendersEachRowText(named))
	case !given:
		limit.SetInt(int64(includedPageDefault(bounds)))
	}
	return nil
}

// soleValue is the one value of values, and "" where there is not exactly one.
func soleValue(values []string) string {
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

// parse reads one value of the field into into, a value of its scalar type or a querySort.
func (field queryField) parse(raw string, into reflect.Value) error {
	switch {
	case into.Type() == querySortType:
		name, descending := strings.CutPrefix(raw, "-")
		if !slices.Contains(field.enum, name) {
			return errorf(apicode.InvalidQuery, "sort takes one of %s, each ascending, or descending after a -", strings.Join(field.enum, ", "))
		}
		into.Set(reflect.ValueOf(querySort{Field: name, Descending: descending}))
	case into.Type() == timeType:
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return errorf(apicode.InvalidQuery, "%s must be an RFC3339 time, such as 2026-11-17T09:00:00Z", field.key)
		}
		into.Set(reflect.ValueOf(at.UTC()))
	case into.Kind() == reflect.String:
		if field.enum != nil && !slices.Contains(field.enum, raw) {
			return errorf(apicode.InvalidQuery, "%s takes %s; %q is none of them", field.key, strings.Join(field.enum, ", "), raw)
		}
		into.SetString(raw)
	case into.Kind() == reflect.Bool:
		switch raw {
		case "true", "false":
			into.SetBool(raw == "true")
		default:
			return errorf(apicode.InvalidQuery, "%s takes true or false", field.key)
		}
	default:
		number, err := strconv.ParseInt(raw, 10, into.Type().Bits())
		if err != nil || (field.min != nil && number < *field.min) || (field.max != nil && number > *field.max) {
			return errorf(apicode.InvalidQuery, "%s must be %s", field.key, field.integerRange())
		}
		into.SetInt(number)
	}
	return nil
}

// integerRange words the integers a field takes, for its refusal.
func (field queryField) integerRange() string {
	switch {
	case field.min != nil && field.max != nil:
		return fmt.Sprintf("an integer from %d to %d", *field.min, *field.max)
	case field.min != nil:
		return fmt.Sprintf("an integer of %d or more", *field.min)
	case field.max != nil:
		return fmt.Sprintf("an integer of %d or less", *field.max)
	}
	return "an integer"
}

// checkV2Body refuses a body declaration the decoder cannot read: a JSON body or an upload with no
// type, a type that is a pointer, an upload's type that is no struct (its fields are the form's),
// a union with no arms.
func checkV2Body(body requestBody) error {
	switch body.Kind {
	case bodyJSON, bodyUpload:
		if body.Type == nil || reflect.TypeOf(body.Type).Kind() == reflect.Pointer {
			return errors.New("a JSON body or an upload names the zero value of its type, never nil and never a pointer")
		}
		if body.Kind == bodyUpload && reflect.TypeOf(body.Type).Kind() != reflect.Struct {
			return errors.New("an upload's type is a struct, whose members are the form's fields")
		}
	case bodyStateUnion, bodyKindUnion:
		if len(body.Arms) == 0 {
			return errors.New("a union names its arms")
		}
		for name, arm := range body.Arms {
			if name == "" || arm == nil || reflect.TypeOf(arm).Kind() == reflect.Pointer {
				return fmt.Errorf("its arm %q names no value of a type, or a pointer", name)
			}
		}
		if body.Optional {
			return errors.New("a union is picked by a member of its body, so its body is never optional")
		}
	}
	return nil
}

// readBody decodes r's body as the row declares it, and answers r carrying it, its body put back
// as the bytes that were read. A route whose body the decoder does not read (none, bytes the
// handler reads itself) is answered r carrying no body.
func (d *v2Decoder) readBody(r *http.Request) (*http.Request, error) {
	decoded := &v2Request{}
	if read, ok := decodedV2(r); ok {
		copied := *read
		decoded = &copied
	}
	body := d.route.Body
	switch body.Kind {
	case bodyJSON, bodyStateUnion, bodyKindUnion:
		if err := d.readJSON(r, decoded); err != nil {
			return r, err
		}
	case bodyUpload:
		if err := d.readUpload(r, decoded); err != nil {
			return r, err
		}
	default:
		return r, nil
	}
	decoded.bodyRead = true
	return withDecodedV2(r, decoded), nil
}

// readJSON reads a JSON body, an object or a union of objects, once, into a buffer bounded by the
// row's BodyLimit; decodes it into the type it declares, or into the arm its discriminating member
// picks; and puts the bytes back as r.Body for the layers behind it. A member the type does not
// declare is ignored and logged (logUndeclaredMembers), as decodeJSON does. A string the body holds
// that carries U+0000 is refused, as decodeJSON refuses it, unless the row's body is TextChecked.
func (d *v2Decoder) readJSON(r *http.Request, decoded *v2Request) error {
	body := d.route.Body
	if body.Optional && r.ContentLength == 0 {
		decoded.body = reflect.Zero(reflect.TypeOf(body.Type)).Interface()
		return nil
	}
	if refusal := jsonContentTypeRefusal(r); refusal != nil {
		return refusal
	}
	data, err := readBounded(r, d.route.BodyLimit)
	if err != nil {
		return err
	}
	if body.Optional && len(data) == 0 {
		decoded.body = reflect.Zero(reflect.TypeOf(body.Type)).Interface()
		return nil
	}
	typ, err := d.armOf(r, data)
	if err != nil {
		return err
	}
	value := reflect.New(typ)
	r.Body = io.NopCloser(bytes.NewReader(data))
	read := readJSONBody
	if body.TextChecked {
		read = readJSONBodyAllowingNUL
	}
	if _, err := read(r, value.Interface(), d.route.BodyLimit.Bytes, d.route.BodyLimit.TooLarge); err != nil {
		return err
	}
	logUndeclaredMembers(r, value.Interface(), data)
	r.Body = io.NopCloser(bytes.NewReader(data))
	var members map[string]json.RawMessage
	if json.Unmarshal(data, &members) == nil {
		decoded.members = members
	}
	decoded.raw = data
	decoded.body = value.Elem().Interface()
	return nil
}

// armOf is the type a JSON body is decoded into: the row's Body.Type, or on a union the arm its
// discriminating member names. A union keyed by `state` reads the body's `state` member, and an
// unknown or missing state is 400 STATE_INVALID naming the states; a union keyed by a mark's
// `kind` reads the path's {kind} where the pattern holds one (PATCH /api/v2/me/marks/{kind}/{id})
// and the body's `kind` member otherwise, and an unknown kind is 400 INVALID_KIND naming the
// kinds. The member is matched as encoding/json matches it, so the arm decoded is the one the
// member names however a caller cases it.
func (d *v2Decoder) armOf(r *http.Request, data []byte) (reflect.Type, error) {
	body := d.route.Body
	if body.Kind != bodyStateUnion && body.Kind != bodyKindUnion {
		return reflect.TypeOf(body.Type), nil
	}
	if !json.Valid(data) {
		return nil, errorf(apicode.InvalidJSON, "invalid JSON body")
	}
	arms := slices.Sorted(maps.Keys(body.Arms))
	if body.Kind == bodyStateUnion {
		var head struct {
			State *string `json:"state"`
		}
		if json.Unmarshal(data, &head) != nil || head.State == nil || body.Arms[*head.State] == nil {
			return nil, errorf(apicode.StateInvalid, "state must be one of %s", strings.Join(arms, ", "))
		}
		return reflect.TypeOf(body.Arms[*head.State]), nil
	}
	kind, named := r.PathValue("kind"), patternNames(d.route.Pattern, "kind")
	if !named {
		var head struct {
			Kind *string `json:"kind"`
		}
		if json.Unmarshal(data, &head) == nil && head.Kind != nil {
			kind = *head.Kind
		}
	}
	if body.Arms[kind] == nil {
		return nil, errorf(apicode.InvalidKind, "kind must be one of %s", strings.Join(arms, ", "))
	}
	return reflect.TypeOf(body.Arms[kind]), nil
}

// patternNames reports whether pattern holds the wildcard {name}.
func patternNames(pattern, name string) bool {
	return slices.Contains(patternWildcards(pattern), name)
}

// jsonContentTypeRefusal is the refusal of a JSON body sent under another content type, 415
// JSON_CONTENT_TYPE, and nil for application/json and every +json type.
func jsonContentTypeRefusal(r *http.Request) *apiError {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && !strings.HasSuffix(contentType, "+json")) {
		return errorf(apicode.JSONContentType, "JSON mutations require Content-Type application/json")
	}
	return nil
}

// readBounded reads r's body whole, refusing one past limit with the route's own 413.
func readBounded(r *http.Request, limit routeBodyLimit) ([]byte, error) {
	data, err := io.ReadAll(http.MaxBytesReader(maxBytesDiscarder{}, r.Body, limit.Bytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, limit.TooLarge
	case err != nil:
		return nil, errorf(apicode.RequestBodyUnreadable, "could not read the request body: %v", err)
	}
	return data, nil
}

// readUpload reads an upload: a multipart body's declared fields and its file part, or the JSON
// object the row's Body.Type describes. A multipart field is the member of Body.Type its JSON tag
// names: a string field takes the part's text, and any other field the part's text read as JSON,
// as /api/v1's upload reads its `actor` part (uploadArtifactFor). A field the type does not
// declare by its exact name (one spelt in another case, which uploadFields does not read, among
// them), and a part other than the file under another name, is ignored and logged, as an
// undeclared JSON member is. The file is read whole and its spooled parts removed before the
// handler runs, as uploadArtifactFor reads it.
func (d *v2Decoder) readUpload(r *http.Request, decoded *v2Request) error {
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "multipart/form-data" {
		return d.readJSON(r, decoded)
	}
	limit := d.route.BodyLimit
	r.Body = http.MaxBytesReader(maxBytesDiscarder{}, r.Body, limit.Bytes)
	reader, err := r.MultipartReader()
	if err != nil {
		return errorf(apicode.InvalidBody, "invalid multipart body: %v", err)
	}
	form, err := reader.ReadForm(1 << 20)
	if err != nil {
		var tooLarge *http.MaxBytesError
		var spool *fs.PathError
		switch {
		case errors.As(err, &tooLarge):
			return limit.TooLarge
		case errors.Is(err, multipart.ErrMessageTooLarge):
			return errorf(apicode.CapExceededTooLarge, "the upload's form fields are too large to read")
		case errors.As(err, &spool):
			return fmt.Errorf("spool a multipart upload: %w", err)
		}
		return errorf(apicode.InvalidBody, "invalid multipart body: %v", err)
	}
	defer func() {
		if err := form.RemoveAll(); err != nil {
			slog.Error("dispatch: remove a multipart upload's spooled files", "error", err)
		}
	}()
	if refusal := unstorableForm(form.Value); refusal != nil {
		return refusal
	}
	typ := reflect.TypeOf(d.route.Body.Type)
	fields, err := uploadFields(typ, form.Value)
	if err != nil {
		return err
	}
	value := reflect.New(typ)
	if err := json.Unmarshal(fields, value.Interface()); err != nil {
		return errorf(apicode.InvalidBody, "an upload's field holds no value of its member: %v", err)
	}
	declared := membersDeclaredBy(typ)
	var ignored []string
	for name := range form.Value {
		if _, ok := declared[name]; !ok {
			ignored = append(ignored, name)
		}
	}
	for name := range form.File {
		if name != uploadFilePart {
			ignored = append(ignored, name)
		}
	}
	if len(ignored) > 0 {
		slices.Sort(ignored)
		slog.DebugContext(r.Context(), "dispatch: upload fields the route does not declare were ignored", "route", r.Pattern, "fields", ignored)
	}
	decoded.body = value.Elem().Interface()
	files := form.File[uploadFilePart]
	switch len(files) {
	case 0:
		return nil
	case 1:
	default:
		return errorf(apicode.InvalidBody, "an upload holds one %s part; this one holds %d", uploadFilePart, len(files))
	}
	file, err := files[0].Open()
	if err != nil {
		return fmt.Errorf("open an upload's spooled file: %w", err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("read an upload's spooled file: %w", err)
	}
	decoded.file = &uploadFile{Name: files[0].Filename, ContentType: files[0].Header.Get("Content-Type"), Content: content}
	return nil
}

// uploadFields is the JSON object a multipart upload's declared fields make: each field typ
// declares that the form gives once, a string field's part as its text and any other's part read as
// JSON. A field given twice is refused, since only one of its values could be read.
func uploadFields(typ reflect.Type, values map[string][]string) ([]byte, error) {
	object := map[string]json.RawMessage{}
	for index := range typ.NumField() {
		field := typ.Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !field.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		given, ok := values[name]
		if !ok {
			continue
		}
		if len(given) != 1 {
			return nil, errorf(apicode.InvalidBody, "the upload's field %s is given %d times; it takes one value", name, len(given))
		}
		member := field.Type
		if member.Kind() == reflect.Pointer {
			member = member.Elem()
		}
		if member.Kind() == reflect.String {
			encoded, err := json.Marshal(given[0])
			if err != nil {
				return nil, fmt.Errorf("encode the upload's field %s: %w", name, err)
			}
			object[name] = encoded
			continue
		}
		if !json.Valid([]byte(given[0])) {
			return nil, errorf(apicode.InvalidBody, "the upload's field %s holds no JSON value", name)
		}
		object[name] = json.RawMessage(given[0])
	}
	return json.Marshal(object)
}

// undecodableRoutes names every route the decoder reads (apiRoute.decoded), at /api/v2 and at a
// both-version row's /api/v1 path alike, whose query type or body type it cannot read
// (newV2Decoder) or whose authority rule could never hold (authorityFaults), and every other route
// that declares authorities, which no gate would hold a request to: the authority gate holds only
// a request the decoder read. register refuses to mount a table it names anything in.
func undecodableRoutes(routes []apiRoute) []string {
	var faults []string
	for _, route := range routes {
		name := route.Method + " " + route.Pattern
		switch {
		case route.decoded():
			decoder, err := newV2Decoder(route)
			if err != nil {
				faults = append(faults, name+": "+err.Error())
				continue
			}
			for _, fault := range decoder.authorityFaults() {
				faults = append(faults, name+": "+fault)
			}
		case len(route.Authorities) > 0:
			faults = append(faults, name+": declares Authorities, which the authority gate holds only on a route the decoder reads, and it reads no row of the INFRA-979 tree, route of the v1 table or test hook")
		}
	}
	slices.Sort(faults)
	return faults
}
