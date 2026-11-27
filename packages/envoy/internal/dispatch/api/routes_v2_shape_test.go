package api

// routes_v2_shape_test.go is c-shape of dispatch://INFRA-4071: the rules of the
// parent's shape (dispatch://INFRA-2946/spec, R1), decided from each /api/v2 row's declarations and
// its path, over every route the rules of /api/v2 bind, and the rules a /api/v1 row's path and
// shape can decide over the v1 rows.
//
// A singleton is named in the singular (R1.1). A singleton that holds a set takes the singular of
// the plural /api/v1 named it by, and -set, so its name is predictable from v1: /limits is
// /limit-set and /issues/{key}/role-holders /issues/{key}/role-holder-set (the architect's ruling,
// dispatch://INFRA-4071/comment/fa6c9d57-e6a5-462b-8d95-063e5b48b163).

import (
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// shapeActionWords are the words no path segment may be, nor end in as its last hyphen-separated
// word, the word that names what a compound segment is: `syncs` and `replies`
// are plural nouns and no such word, and `search-results` ends in `results`.
var shapeActionWords = []string{
	"resolve", "approve", "deny", "revoke", "sync", "dismiss", "reply", "accept", "reject", "reopen",
	"answer", "decline", "keep", "rebuild", "retract", "read", "snooze", "lookup", "latest", "search",
	"settle", "post", "cancel", "undo", "rewrite", "draft", "history",
}

// shapeFormWords are the forms of an item a read's last segment may not name: a form is an
// `include` of the item's own read (R1.9).
var shapeFormWords = []string{"text", "blocks", "summary", "latest"}

// shapeUnsetWords are spellings of a field's unset value other than `none` (R1.6).
var shapeUnsetWords = []string{"null", "nil", "unset", "empty", "unassigned", "unlinked", "nobody"}

// shapeBooleanSpellings are the values of a two-valued filter that is a boolean spelled as a string.
var shapeBooleanSpellings = []string{"true", "false", "yes", "no", "on", "off", "0", "1"}

// shapeR13Rows are the two rows R1.3 of the parent's spec names as the exemptions from rule 2: the
// cross-project issue list, which lists the rows /projects/{key}/issues lists for several
// projects, and the issue create, whose project comes from its body because an import by an
// external reference takes its project from the repository's mapping.
var shapeR13Rows = map[string]bool{http.MethodGet + " /issues": true, http.MethodPost + " /issues": true}

// shapeRule is one rule of the parent's R1 the table test decides.
type shapeRule struct {
	name, rule string
	// faults is every way route breaks the rule, standing in table.
	faults func(route apiRoute, table shapeTable) []string
	// breaks are made-up rows that, added to the table, break this rule and no other.
	breaks func() []routeRow
}

// shapeTable is what a rule reads of the whole table beside one row.
type shapeTable struct {
	// keys are the keys each resource's rows declare.
	keys map[string]map[string]bool
	// topItems is each resource whose item stands at the top level, /<resource>/<key> (an id
	// unique across Dispatch, R1.2's departure), at that path.
	topItems map[string]string
	// projectCollections are the resources with a collection under /projects/{key}.
	projectCollections map[string]bool
}

func newShapeTable(routes []apiRoute) shapeTable {
	table := shapeTable{keys: map[string]map[string]bool{}, topItems: map[string]string{}, projectCollections: map[string]bool{}}
	for _, route := range routes {
		if route.Key != "" {
			if table.keys[route.Resource] == nil {
				table.keys[route.Resource] = map[string]bool{}
			}
			table.keys[route.Resource][route.Key] = true
		}
		path := shapePath(route)
		if (route.Kind == kindItem || route.Kind == kindBytes) && route.Key != "" && path == "/"+route.Resource+"/"+route.Key {
			table.topItems[route.Resource] = path
		}
		if (route.Kind == kindCollection || route.Kind == kindLog) && path == "/projects/{key}/"+route.Resource {
			table.projectCollections[route.Resource] = true
		}
	}
	return table
}

// shapePath is route's pattern without /api/v2, and shapeSegments its segments.
func shapePath(route apiRoute) string { return strings.TrimPrefix(route.Pattern, "/api/v2") }

func shapeSegments(route apiRoute) []string {
	if path := strings.Trim(shapePath(route), "/"); path != "" {
		return strings.Split(path, "/")
	}
	return nil
}

func isWildcardSegment(segment string) bool { return strings.HasPrefix(segment, "{") }

func lastHyphenWord(segment string) string { return segment[strings.LastIndex(segment, "-")+1:] }

func isPluralWord(word string) bool {
	return strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss")
}

// queryFieldsOf is the keys route's query declares, by key.
func queryFieldsOf(route apiRoute) map[string]queryField {
	schema, err := compileV2Query(route.Query)
	if err != nil {
		return nil
	}
	fields := map[string]queryField{}
	for _, field := range schema.fields {
		fields[field.key] = field
	}
	return fields
}

// shapeMembersOf is the members a JSON value of typ carries by name, an embedded struct's included.
func shapeMembersOf(typ reflect.Type) map[string]reflect.StructField {
	members := map[string]reflect.StructField{}
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == nil || typ.Kind() != reflect.Struct {
		return members
	}
	for index := range typ.NumField() {
		field := typ.Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		embedded := field.Type
		for embedded.Kind() == reflect.Pointer {
			embedded = embedded.Elem()
		}
		if field.Anonymous && name == "" && embedded.Kind() == reflect.Struct {
			maps.Copy(members, shapeMembersOf(embedded))
			continue
		}
		if !field.IsExported() || name == "-" {
			continue
		}
		members[cmpOr(name, field.Name)] = field
	}
	return members
}

func cmpOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// isListEnvelope reports whether a value of response answers {items, total, limit, offset}.
func isListEnvelope(response any) bool {
	if response == nil {
		return false
	}
	members := shapeMembersOf(reflect.TypeOf(response))
	items, hasItems := members["items"]
	_, hasTotal := members["total"]
	_, hasLimit := members["limit"]
	_, hasOffset := members["offset"]
	return hasItems && items.Type.Kind() == reflect.Slice && hasTotal && hasLimit && hasOffset
}

// shapeWindowKeys are the keys a read over time windows its time fields by (R1.5).
func shapeWindowKeys(fields []string) []string {
	if len(fields) == 1 {
		return []string{"from", "to", "compare_from", "compare_to"}
	}
	var keys []string
	for _, field := range fields {
		prefix := strings.TrimSuffix(field, "_at")
		keys = append(keys, prefix+"_from", prefix+"_to")
	}
	return keys
}

// shapeRules are the rules of the parent's R1 the test decides.
func shapeRules() []shapeRule {
	noop := func(http.ResponseWriter, *http.Request) {}
	type widgetQuery struct{ pageQuery }
	row := func(method, pattern, resource string, kind resourceKind) routeRow {
		made := routeRow{
			Method: method, Pattern: pattern, Auth: authAny, Description: "A made-up row.", Handler: noop,
			Versions: onlyV2, Resource: resource, Kind: kind,
			Shape: routeShape{Status: http.StatusOK, Response: struct {
				Name string `json:"name"`
			}{}},
		}
		switch kind {
		case kindCollection, kindLog:
			made.Key = "{id}"
			if method == http.MethodGet {
				made.Query, made.Shape.Response = widgetQuery{}, page[string]{}
			}
		case kindItem:
			made.Key = "{id}"
		}
		if method != http.MethodGet {
			made.Scope = scopeDeployment
		}
		return made
	}
	return []shapeRule{
		{
			name: "action-words", rule: "R1.1: a path is nouns, with no verb",
			faults: func(route apiRoute, _ shapeTable) []string {
				var faults []string
				for _, segment := range shapeSegments(route) {
					if word := actionWordOf(segment); word != "" {
						faults = append(faults, fmt.Sprintf("segment %q is the action word %q", segment, word))
					}
				}
				return faults
			},
			breaks: func() []routeRow {
				return []routeRow{row(http.MethodGet, "/search/widgets", "widgets", kindCollection),
					row(http.MethodGet, "/mark-read/widgets", "widgets", kindCollection)}
			},
		},
		{
			name: "plural-and-singular", rule: "R1.1: a plural for a collection and its items, a singular for a singleton",
			faults: func(route apiRoute, _ shapeTable) []string {
				word := lastHyphenWord(route.Resource)
				switch {
				case route.Resource == "":
					return []string{"declares no resource"}
				case (route.Kind == kindCollection || route.Kind == kindLog || route.Kind == kindItem) && !isPluralWord(word):
					return []string{fmt.Sprintf("a %s of %q, which is no plural", route.Kind, route.Resource)}
				case route.Kind == kindSingleton && isPluralWord(word):
					return []string{fmt.Sprintf("a singleton named %q, a plural; a singleton that holds a set takes the singular and -set", route.Resource)}
				case route.Kind == kindState && route.Resource != "state":
					return []string{fmt.Sprintf("a state named %q, not state", route.Resource)}
				}
				return nil
			},
			breaks: func() []routeRow {
				return []routeRow{row(http.MethodGet, "/widget", "widget", kindCollection),
					row(http.MethodGet, "/settings/widgets", "widgets", kindSingleton)}
			},
		},
		{
			name: "one-address", rule: "R1.2: a resource has one address, and an item's segment is its declared key",
			faults: func(route apiRoute, table shapeTable) []string {
				var faults []string
				path, segments := shapePath(route), shapeSegments(route)
				switch {
				case path == "":
				case (route.Kind == kindItem || route.Kind == kindBytes) && route.Key != "":
					if !strings.HasSuffix(path, "/"+route.Resource+"/"+route.Key) {
						faults = append(faults, fmt.Sprintf("an item of %s whose path does not end in /%s/%s", route.Resource, route.Resource, route.Key))
					}
					if top, held := table.topItems[route.Resource]; held && top != path {
						faults = append(faults, fmt.Sprintf("a second address of the %s item %s", route.Resource, top))
					}
				case route.Kind == kindState:
					if len(segments) < 2 || segments[len(segments)-1] != "state" || !isWildcardSegment(segments[len(segments)-2]) {
						faults = append(faults, "a state whose path does not end in an item's key and /state")
					}
				case segments[len(segments)-1] != route.Resource:
					faults = append(faults, fmt.Sprintf("a %s of %q whose last segment is %q", route.Kind, route.Resource, segments[len(segments)-1]))
				}
				if len(table.keys[route.Resource]) > 1 {
					faults = append(faults, fmt.Sprintf("%s declares the keys %v; a resource has one", route.Resource, slices.Sorted(maps.Keys(table.keys[route.Resource]))))
				}
				owner := ""
				for index := 0; index < len(segments); index++ {
					if !isWildcardSegment(segments[index]) {
						owner = segments[index]
						continue
					}
					keys := slices.Sorted(maps.Keys(table.keys[owner]))
					if len(keys) != 1 {
						faults = append(faults, fmt.Sprintf("%s follows %q, which declares no one key", segments[index], owner))
						break
					}
					parts := strings.Split(keys[0], "/")
					if index+len(parts) > len(segments) || strings.Join(segments[index:index+len(parts)], "/") != keys[0] {
						faults = append(faults, fmt.Sprintf("%s is no key of %s, whose key is %s: a lookup by another key is a filter", segments[index], owner, keys[0]))
						break
					}
					index += len(parts) - 1
				}
				return faults
			},
			breaks: func() []routeRow {
				slug := row(http.MethodGet, "/issues/{key}/artifacts/{slug}", "artifacts", kindItem)
				slug.Key = "{slug}"
				return []routeRow{slug, row(http.MethodGet, "/issues/{key}/asks/{id}", "asks", kindItem)}
			},
		},
		{
			name: "project-path", rule: "R1.2 and R1.3: a project's collection is under /projects/{key}; a read across projects is a top-level list with a repeatable project filter",
			faults: func(route apiRoute, table shapeTable) []string {
				var faults []string
				name := route.Method + " " + shapePath(route)
				segments := shapeSegments(route)
				project, filtered := queryFieldsOf(route)["project"]
				switch {
				case filtered && !route.CrossProject:
					faults = append(faults, "filters by project, and is no read across projects (CrossProject)")
				case route.CrossProject && (route.Method != http.MethodGet || len(segments) != 1):
					faults = append(faults, "reads across projects at a path that is no top-level collection")
				case route.CrossProject && (!filtered || project.kind != queryList || project.hasFallback):
					faults = append(faults, "reads across projects with no repeatable project filter free of a default")
				}
				if route.CrossProject && table.projectCollections[route.Resource] && !shapeR13Rows[name] {
					faults = append(faults, "lists across projects the rows /projects/{key}/"+route.Resource+" lists: two addresses")
				}
				// Only a create is held to it, because a write of an item outside /projects/{key}, a
				// PUT or a PATCH, names its project as a field of the item, not as where the item is
				// created.
				if route.Method == http.MethodPost && route.Body.Kind == bodyJSON && (len(segments) == 0 || segments[0] != "projects") && !shapeR13Rows[name] {
					if member, named := shapeMembersOf(reflect.TypeOf(route.Body.Type))["project"]; named && member.Tag.Get("required") == "true" {
						faults = append(faults, "creates in the one project its body requires, outside /projects/{key}")
					}
				}
				return faults
			},
			breaks: func() []routeRow {
				filtered := row(http.MethodGet, "/widgets", "widgets", kindCollection)
				filtered.Query = struct {
					pageQuery
					Project []string `query:"project"`
				}{}
				create := row(http.MethodPost, "/gadgets", "gadgets", kindCollection)
				create.Body = requestBody{Kind: bodyJSON, Type: struct {
					Project string `json:"project" required:"true"`
				}{}}
				return []routeRow{filtered, create}
			},
		},
		{
			name: "list-envelope", rule: "R1.4: every list answers {items, total, limit, offset} and takes limit and offset within declared bounds",
			faults: func(route apiRoute, _ shapeTable) []string {
				fields := queryFieldsOf(route)
				_, limit := fields["limit"]
				_, offset := fields["offset"]
				list := route.Method == http.MethodGet && (route.Kind == kindCollection || route.Kind == kindLog)
				var faults []string
				switch {
				case list && !isListEnvelope(route.Shape.Response):
					faults = append(faults, fmt.Sprintf("a list answering %T, no envelope", route.Shape.Response))
				case list && (!limit || !offset):
					faults = append(faults, "a list that takes no limit and offset")
				case list && route.List.Whole && (route.List.Default != 0 || route.List.Max != 0):
					faults = append(faults, "a catalogue read whole that names a default or a largest limit")
				case list && !route.List.Whole && route.List != (listBounds{}) && (route.List.Default < 1 || route.List.Max < route.List.Default):
					faults = append(faults, fmt.Sprintf("a list whose bounds %+v hold no limit", route.List))
				case !list && (limit || offset || route.List != (listBounds{})):
					faults = append(faults, "no list, and takes limit and offset")
				case !list && route.Method == http.MethodGet && isListEnvelope(route.Shape.Response):
					faults = append(faults, "a read of no list answering an envelope")
				}
				for _, key := range []string{"cursor", "page", "per_page", "page_size"} {
					if _, named := fields[key]; named {
						faults = append(faults, "pages by "+key+", not limit and offset")
					}
				}
				return faults
			},
			breaks: func() []routeRow {
				bare := row(http.MethodGet, "/widgets", "widgets", kindCollection)
				bare.Shape.Response = []string{}
				bounds := row(http.MethodGet, "/gadgets", "gadgets", kindCollection)
				bounds.List = listBounds{Default: 300, Max: 200}
				return []routeRow{bare, bounds}
			},
		},
		{
			name: "time-window", rule: "R1.5: a window from and to only on a read over time, on its declared time fields",
			faults: func(route apiRoute, _ shapeTable) []string {
				var faults []string
				fields := queryFieldsOf(route)
				window := shapeWindowKeys(route.TimeFields)
				if len(route.TimeFields) > 0 {
					if route.Method != http.MethodGet {
						faults = append(faults, "a write declaring time fields")
					}
					wanted := window
					if len(route.TimeFields) == 1 {
						wanted = window[:2]
					}
					for _, key := range wanted {
						if field, named := fields[key]; !named || field.scalar != timeType {
							faults = append(faults, fmt.Sprintf("time fields %v and no time key %s", route.TimeFields, key))
						}
					}
				}
				for key, field := range fields {
					if field.scalar == timeType && (len(route.TimeFields) == 0 || !slices.Contains(window, key)) {
						faults = append(faults, fmt.Sprintf("the time key %s windows no declared time field", key))
					}
				}
				for _, key := range []string{"since", "until"} {
					if _, named := fields[key]; named {
						faults = append(faults, "windows by "+key+", not from and to")
					}
				}
				return faults
			},
			breaks: func() []routeRow {
				unwindowed := row(http.MethodGet, "/widgets", "widgets", kindCollection)
				unwindowed.Query = struct {
					pageQuery
					From *time.Time `query:"from"`
					To   *time.Time `query:"to"`
				}{}
				undeclared := row(http.MethodGet, "/gadgets", "gadgets", kindCollection)
				undeclared.TimeFields = []string{"created_at"}
				return []routeRow{unwindowed, undeclared}
			},
		},
		{
			name: "log-position", rule: "R1.5 and R5: a log names its position field and takes after and before",
			faults: func(route apiRoute, _ shapeTable) []string {
				fields := queryFieldsOf(route)
				_, after := fields["after"]
				_, before := fields["before"]
				switch {
				case route.Kind == kindLog && route.Position.Field == "":
					return []string{"a log naming no position field"}
				case route.Kind == kindLog && (!after || !before):
					return []string{"a log that does not take both after and before"}
				case route.Kind != kindLog && (route.Position != (positionField{}) || after || before):
					return []string{"no log, and names a position"}
				}
				return nil
			},
			breaks: func() []routeRow {
				unpositioned := row(http.MethodGet, "/widgets", "widgets", kindLog)
				unpositioned.Query = struct {
					pageQuery
					After  *string `query:"after"`
					Before *string `query:"before"`
				}{}
				positioned := row(http.MethodGet, "/gadgets", "gadgets", kindCollection)
				positioned.Query = struct {
					pageQuery
					After *string `query:"after"`
				}{}
				return []routeRow{unpositioned, positioned}
			},
		},
		{
			name: "filters", rule: "R1.6: a boolean is true or false, an unset value none, a sort sort=<field>",
			faults: func(route apiRoute, _ shapeTable) []string {
				var faults []string
				for key, field := range queryFieldsOf(route) {
					if field.kind == querySortValue && (key != "sort" || len(field.enum) == 0 || !field.hasFallback) {
						faults = append(faults, "a sort that is not sort=<field> with its fields and its default")
					}
					if field.kind != querySortValue && len(field.enum) == 2 && !slices.ContainsFunc(field.enum, func(value string) bool {
						return !slices.Contains(shapeBooleanSpellings, value)
					}) {
						faults = append(faults, fmt.Sprintf("%s is a boolean spelled %v, not true or false", key, field.enum))
					}
					for _, value := range field.enum {
						if slices.Contains(shapeUnsetWords, value) {
							faults = append(faults, fmt.Sprintf("%s spells its unset value %s, not none", key, value))
						}
					}
					if slices.Contains([]string{"order", "order_by", "sort_by", "sort_order"}, key) {
						faults = append(faults, "orders by "+key+", not sort")
					}
				}
				return faults
			},
			breaks: func() []routeRow {
				spelled := row(http.MethodGet, "/widgets", "widgets", kindCollection)
				spelled.Query = struct {
					pageQuery
					Archived string `query:"archived" enum:"yes,no"`
					Owner    string `query:"owner" enum:"me,nobody"`
					Order    string `query:"order"`
				}{}
				return []routeRow{spelled}
			},
		},
		{
			name: "write-method", rule: "R1.7: a create is a POST to the collection, a change a PATCH of the item, a transition a PUT of its state, a removal a DELETE",
			faults: func(route apiRoute, _ shapeTable) []string {
				allowed := map[string][]resourceKind{
					http.MethodPost:   {kindCollection, kindComputation},
					http.MethodPatch:  {kindItem},
					http.MethodPut:    {kindState, kindItem, kindSingleton},
					http.MethodDelete: {kindItem, kindSingleton},
					http.MethodGet:    {kindCollection, kindLog, kindItem, kindSingleton, kindReport, kindBytes, kindStream},
				}[route.Method]
				segments := shapeSegments(route)
				// The bulk change of the caller's own marks, one PATCH of the collection (R1.7's departure).
				if route.Method == http.MethodPatch && route.Kind == kindCollection && len(segments) > 0 && segments[0] == "me" {
					allowed = append(allowed, kindCollection)
				}
				var faults []string
				if !slices.Contains(allowed, route.Kind) {
					faults = append(faults, fmt.Sprintf("a %s of a %s", route.Method, route.Kind))
				}
				if route.Kind == kindState && route.Body.Kind != bodyStateUnion {
					faults = append(faults, "a transition whose body is no union keyed by state")
				}
				if route.Method != http.MethodGet && route.Scope == "" {
					faults = append(faults, "a write that declares no write class")
				}
				return faults
			},
			breaks: func() []routeRow {
				transition := row(http.MethodPut, "/widgets/{id}/state", "state", kindState)
				transition.Body = requestBody{Kind: bodyJSON, Type: struct {
					State string `json:"state"`
				}{}}
				return []routeRow{row(http.MethodPost, "/widgets/{id}", "widgets", kindItem), transition}
			},
		},
		{
			name: "authorities", rule: "R1.8: each authority a value needs is declared on a member or key the row declares",
			faults: func(route apiRoute, _ shapeTable) []string {
				// A row the decoder cannot read is no fault of its authorities: register refuses to
				// mount it (undecodableRoutes), so no served row reaches here undecodable, and the
				// column a made-up row breaks is its own rule's fault (list-envelope's bounds).
				decoder, err := newV2Decoder(route)
				if err != nil {
					return nil
				}
				return decoder.authorityFaults()
			},
			breaks: func() []routeRow {
				undeclared := row(http.MethodPatch, "/widgets/{id}", "widgets", kindItem)
				undeclared.Body = requestBody{Kind: bodyJSON, Type: struct {
					Name string `json:"name"`
				}{}}
				undeclared.Authorities = []authorityRule{{Where: authorityWhere{In: partBody, Name: "owner"}, Scopes: routeScopeSet{model.ScopePerson}}}
				return []routeRow{undeclared}
			},
		},
		{
			name: "forms", rule: "R1.9: a form of an item is an include of its read, never a segment",
			faults: func(route apiRoute, _ shapeTable) []string {
				var faults []string
				if segments := shapeSegments(route); route.Method == http.MethodGet && len(segments) > 0 && slices.Contains(shapeFormWords, segments[len(segments)-1]) {
					faults = append(faults, fmt.Sprintf("a read whose last segment names the form %s", segments[len(segments)-1]))
				}
				if include, named := queryFieldsOf(route)["include"]; named && (include.kind != queryList || !include.comma || len(include.enum) == 0) {
					faults = append(faults, "an include that is no comma list of the forms it names")
				}
				return faults
			},
			breaks: func() []routeRow {
				text := row(http.MethodGet, "/settings/text", "text", kindSingleton)
				include := row(http.MethodGet, "/widgets", "widgets", kindCollection)
				include.Query = struct {
					pageQuery
					Include string `query:"include"`
				}{}
				return []routeRow{text, include}
			},
		},
	}
}

// actionWordOf is the action word segment is, or ends in, and "" for a segment that is none.
func actionWordOf(segment string) string {
	if isWildcardSegment(segment) {
		return ""
	}
	for _, word := range []string{segment, lastHyphenWord(segment)} {
		if slices.Contains(shapeActionWords, word) {
			return word
		}
	}
	return ""
}

// TestTheV2TableKeepsTheOneShape is c-shape's /api/v2 half: every route the rules of /api/v2 bind
// keeps every rule, the test names each route it checked and each row of the INFRA-979 tree it
// left out (OfINFRA979), and for each rule a made-up row that breaks it alone is refused by it
// alone.
func TestTheV2TableKeepsTheOneShape(t *testing.T) {
	var checked []apiRoute
	for _, route := range servedAt(apiV2, (&server{}).routes()) {
		if route.OfINFRA979 {
			t.Logf("left out, a row of the INFRA-979 tree: %s %s", route.Method, route.Pattern)
			continue
		}
		if route.v2Bound() {
			checked = append(checked, route)
		}
	}
	rules := shapeRules()
	table := newShapeTable(checked)
	for _, route := range checked {
		t.Logf("checked: %s %s", route.Method, route.Pattern)
		for _, rule := range rules {
			for _, fault := range rule.faults(route, table) {
				t.Errorf("%s %s breaks %s (%s): %s", route.Method, route.Pattern, rule.name, rule.rule, fault)
			}
		}
	}
	t.Logf("checked %d /api/v2 routes against %d rules", len(checked), len(rules))

	for _, rule := range rules {
		t.Run("a made-up row that breaks "+rule.name, func(t *testing.T) {
			var made []apiRoute
			for _, row := range rule.breaks() {
				made = append(made, mountRow(row)...)
			}
			scratch := newShapeTable(slices.Concat(checked, made))
			refused := map[string][]string{}
			for _, route := range made {
				for _, other := range rules {
					for _, fault := range other.faults(route, scratch) {
						refused[other.name] = append(refused[other.name], route.Method+" "+route.Pattern+": "+fault)
					}
				}
			}
			if len(refused[rule.name]) == 0 {
				t.Errorf("%s refused none of its made-up rows", rule.name)
			}
			for name, faults := range refused {
				if name != rule.name {
					t.Errorf("the made-up rows of %s also break %s: %v", rule.name, name, faults)
				}
			}
			t.Logf("%s refused: %s", rule.name, strings.Join(refused[rule.name], "; "))
		})
	}

	t.Run("plural nouns and compound segments are no action words", func(t *testing.T) {
		for _, segment := range []string{"architecture-syncs", "replies", "search-results", "search-coverage", "rebuilds", "machine-login-lookups"} {
			if word := actionWordOf(segment); word != "" {
				t.Errorf("%s is refused as the action word %s", segment, word)
			}
		}
	})
}

// v1ActionWordPaths are the rows of /api/v1 whose path holds an action word, each of which the
// route map renames (dispatch://INFRA-2946/artifact/route-map-md, section (a)), and the ones
// merges of main added after it: #4374's dismiss, whose /api/v2 route is the state write
// PUT /api/v2/credential-requests/{id}/state with {"state": "dismissed"}, and #3512's two history
// reads, whose /api/v2 routes are GET /api/v2/comments/{id}/corrections and
// GET /api/v2/messages/{id}/corrections.
var v1ActionWordPaths = []string{
	"DELETE /api/v1/me/asks/{id}/snooze",
	"GET /api/v1/comments/{id}/history",
	"GET /api/v1/issues/resolve",
	"GET /api/v1/issues/{key}/handoffs/{phase}/latest",
	"GET /api/v1/me/documents/{id}/read",
	"GET /api/v1/messages/{id}/history",
	"GET /api/v1/priorities/{id}/history",
	"GET /api/v1/search",
	"GET /api/v1/search/coverage",
	"GET /api/v1/search/results",
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep",
	"POST /api/v1/artifacts/{id}/rebuild",
	"POST /api/v1/asks/{id}/answer",
	"POST /api/v1/asks/{id}/decline",
	"POST /api/v1/asks/{id}/resolve",
	"POST /api/v1/comments/{id}/accept",
	"POST /api/v1/comments/{id}/reject",
	"POST /api/v1/comments/{id}/reopen",
	"POST /api/v1/comments/{id}/reply",
	"POST /api/v1/comments/{id}/resolve",
	"POST /api/v1/credential-grants/{id}/revoke",
	"POST /api/v1/credential-requests/machine-lookup",
	"POST /api/v1/credential-requests/{id}/approve",
	"POST /api/v1/credential-requests/{id}/deny",
	"POST /api/v1/credential-requests/{id}/dismiss",
	"POST /api/v1/machine-logins/{id}/revoke",
	"POST /api/v1/me/inbox/dismiss",
	"POST /api/v1/me/inbox/read",
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept",
	"POST /api/v1/messages/{id}/reply",
	"POST /api/v1/projects/{key}/architecture-source/sync",
	"POST /api/v1/projects/{key}/architecture-sources/sync",
	"POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync",
	"POST /api/v1/results/{id}/retract",
	"POST /api/v1/signins/{id}/revoke",
	"PUT /api/v1/me/asks/{id}/snooze",
	"PUT /api/v1/me/documents/{id}/read",
	"PUT /api/v1/me/threads/{id}/read",
}

// TestTheShapeRulesRefuseTheV1Rows is c-shape's /api/v1 half: three rules of R1, run on each v1
// row read as a /api/v2 row at its path under /api/v2 with the columns of R1 its path and shape
// decide (a v1 row declares none), refuse the action-word paths the route map lists, every list
// that answers a bare array at a path the map keeps, and the 15 slug addresses. The action-word
// rule reads the path alone. The envelope rule reads a GET answering a list at a kept path as a
// collection. The one-address rule reads a path naming {slug} as an item of the noun before its
// last wildcard, keyed by that wildcard, or as a collection of its last noun, in the /api/v2 table
// with that row beside it.
func TestTheShapeRulesRefuseTheV1Rows(t *testing.T) {
	rules := map[string]shapeRule{}
	for _, rule := range shapeRules() {
		rules[rule.name] = rule
	}
	var v2 []apiRoute
	for _, route := range servedAt(apiV2, (&server{}).routes()) {
		if route.v2Bound() {
			v2 = append(v2, route)
		}
	}
	refuses := func(rule string, route apiRoute, table shapeTable) bool {
		return len(rules[rule].faults(route, table)) > 0
	}
	var actions, lists, bare, slugAddresses, slugs []string
	for _, row := range v1Declarations() {
		name := row.Method + " " + row.Pattern
		route := v1Route(&server{}, row, nil)
		route.Pattern = "/api/v2" + strings.TrimPrefix(row.Pattern, "/api/v1")
		if refuses("action-words", route, shapeTable{}) {
			actions = append(actions, name)
		}
		if response := route.Shape.Response; row.Method == http.MethodGet && response != nil &&
			reflect.TypeOf(response).Kind() == reflect.Slice &&
			successorRoute(row.Successor) == row.Method+" "+strings.TrimPrefix(row.Pattern, "/api/v1") {
			lists = append(lists, name)
			list := route
			list.Kind = kindCollection
			if refuses("list-envelope", list, shapeTable{}) {
				bare = append(bare, name)
			}
		}
		if strings.Contains(row.Pattern, "{slug}") {
			slugAddresses = append(slugAddresses, name)
			segments := shapeSegments(route)
			address := route
			address.Kind = kindCollection
			if last := segments[len(segments)-1]; isWildcardSegment(last) {
				address.Kind, address.Resource, address.Key = kindItem, segments[len(segments)-2], last
			} else {
				address.Resource = last
			}
			if refuses("one-address", address, newShapeTable(append(slices.Clone(v2), address))) {
				slugs = append(slugs, name)
			}
		}
	}
	slices.Sort(actions)
	t.Logf("action words, %d:\n%s", len(actions), strings.Join(actions, "\n"))
	t.Logf("a bare array at a path the map keeps, %d:\n%s", len(bare), strings.Join(bare, "\n"))
	t.Logf("a lookup by a slug, %d:\n%s", len(slugs), strings.Join(slugs, "\n"))
	if !slices.Equal(actions, v1ActionWordPaths) {
		t.Errorf("the action-word rule refuses %v, want the paths the route map renames %v", actions, v1ActionWordPaths)
	}
	if !slices.Equal(bare, lists) || len(bare) < 20 || !slices.Contains(bare, "GET /api/v1/issues") {
		t.Errorf("the envelope rule refuses %d of the %d bare arrays at kept paths (%v), want every one, at least 20 with GET /api/v1/issues",
			len(bare), len(lists), bare)
	}
	if !slices.Equal(slugs, slugAddresses) || len(slugs) != 15 {
		t.Errorf("the one-address rule refuses %d of the %d slug addresses (%v), want every one of 15", len(slugs), len(slugAddresses), slugs)
	}
}
