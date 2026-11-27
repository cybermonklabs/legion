package api

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/swaggest/jsonschema-go"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// The Evals tab reads each measure's value from the eval routes and the evals view alone
// (dispatch://INFRA-2918/spec R16): a route or a view outside them that answers a measure is a
// second source of one number. These are what answering a measure means here: a member named
// `measures`, or one of the types of delivery/measures that carry a measure's value.
var measureTypes = []reflect.Type{
	reflect.TypeFor[measures.Result](),
	reflect.TypeFor[measures.Cell](),
	reflect.TypeFor[measures.Timing](),
	reflect.TypeFor[measures.LeadTimeCell](),
	reflect.TypeFor[measures.Targets](),
}

// measureRoutesUntilTheRoot are the routes outside the evals that answer a measure and keep
// answering it for the open tabs of the old Delivery and Pipeline pages, until the root's pull
// request removes them (dispatch://INFRA-2918/artifact/plan-md, decision 1). Each is named here
// by its row, so a new route that answers a measure is no exception, and a row the root's pull
// request removes, or that stops answering a measure, is taken off the list.
var measureRoutesUntilTheRoot = []string{
	"GET /api/v1/delivery/measures",
	"GET /api/v1/delivery/pipeline",
	"GET /api/v1/delivery/timeline",
}

// measureViewsUntilTheRoot are the hub's view kinds that answer a measure, for the same pages and
// until the same pull request (decision 1).
var measureViewsUntilTheRoot = []string{deliveryLiveMeasures, deliveryLivePipeline, deliveryLiveTimeline}

// isEvalRoute is whether pattern is one of the eval routes, whose answers are the measures: the
// project's evals and everything under them, and the cross-project read.
func isEvalRoute(pattern string) bool {
	for _, root := range []string{"/api/v1/evals", "/api/v1/projects/{key}/evals"} {
		if pattern == root || strings.HasPrefix(pattern, root+"/") {
			return true
		}
	}
	return false
}

// measureIn is where body, a Go type a route or a view answers, holds a measure: the JSON path
// of the first `measures` member or measure type it reaches, or "" where it holds none. A value
// written as one of several shapes (jsonschema.OneOfExposer, as the project timeline's rows are)
// holds what any of its shapes holds, each walked under its type's name.
func measureIn(body reflect.Type) string {
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type, path string) string
	walk = func(t reflect.Type, path string) string {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if slices.Contains(measureTypes, t) {
			return path + " (" + t.String() + ")"
		}
		if seen[t] {
			return ""
		}
		seen[t] = true
		switch t.Kind() {
		case reflect.Slice, reflect.Array:
			return walk(t.Elem(), path+"[]")
		case reflect.Map:
			return walk(t.Elem(), path+"{}")
		case reflect.Struct:
			union, ok := reflect.Zero(t).Interface().(jsonschema.OneOfExposer)
			if !ok {
				union, ok = reflect.New(t).Interface().(jsonschema.OneOfExposer)
			}
			if ok {
				for _, shape := range union.JSONSchemaOneOf() {
					if found := walk(reflect.TypeOf(shape), path+"<"+reflect.TypeOf(shape).Name()+">"); found != "" {
						return found
					}
				}
			}
			for i := range t.NumField() {
				field := t.Field(i)
				name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
				if name == "-" || (!field.IsExported() && !field.Anonymous) {
					continue
				}
				if field.Anonymous && name == "" {
					if found := walk(field.Type, path); found != "" {
						return found
					}
					continue
				}
				if name == "" {
					name = field.Name
				}
				if name == "measures" {
					return path + ".measures"
				}
				if found := walk(field.Type, path+"."+name); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(body, "")
}

// shapeMeasure is where a route's success answers hold a measure (measureIn), over its answer and
// each other success it gives, or "" where none does.
func shapeMeasure(shape routeShape) string {
	bodies := []any{shape.Response}
	for _, also := range shape.Also {
		bodies = append(bodies, also.Body)
	}
	for _, body := range bodies {
		if body == nil {
			continue
		}
		if found := measureIn(reflect.TypeOf(body)); found != "" {
			return found
		}
	}
	return ""
}

func TestNoRouteOrViewOutsideTheEvalsAnswersAMeasure(t *testing.T) {
	mounted := map[string]bool{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		name := route.Method + " " + route.Pattern
		mounted[name] = true
		if isEvalRoute(route.Pattern) {
			continue
		}
		found := shapeMeasure(route.Shape)
		listed := slices.Contains(measureRoutesUntilTheRoot, name)
		switch {
		case found != "" && !listed:
			t.Errorf("%s answers a measure outside the eval routes, at %s", name, found)
		case found == "" && listed:
			t.Errorf("%s is listed as answering a measure until the root's pull request, and answers none: take it off measureRoutesUntilTheRoot", name)
		}
	}
	for _, name := range measureRoutesUntilTheRoot {
		if !mounted[name] {
			t.Errorf("%s is listed as answering a measure until the root's pull request, and is no route: take it off measureRoutesUntilTheRoot", name)
		}
	}

	kinds := make([]string, 0, len(deliveryLiveKinds))
	for kind := range deliveryLiveKinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		if kind == evalsView {
			continue
		}
		found := measureIn(deliveryLiveKinds[kind].body)
		listed := slices.Contains(measureViewsUntilTheRoot, kind)
		switch {
		case found != "" && !listed:
			t.Errorf("the %s view answers a measure outside the evals view, at %s", kind, found)
		case found == "" && listed:
			t.Errorf("the %s view is listed as answering a measure until the root's pull request, and answers none: take it off measureViewsUntilTheRoot", kind)
		}
	}
	for _, kind := range measureViewsUntilTheRoot {
		if _, held := deliveryLiveKinds[kind]; !held {
			t.Errorf("the %s view is listed as answering a measure until the root's pull request, and is no view kind: take it off measureViewsUntilTheRoot", kind)
		}
	}
}
