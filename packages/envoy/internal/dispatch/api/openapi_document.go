package api

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/swaggest/openapi-go/openapi31"
)

// openAPIDocument builds the document. It walks the table with its test hooks in, since a hook's
// row is a row of the table wherever a deployment leaves it out (scopedRoutesNotMounted), and
// marks each hook row x-test-hook so a reader of the document can tell the twelve apart.
func openAPIDocument() (*openapi31.Spec, error) {
	emitter, bothDirections := newOpenAPIEmitter()
	for _, body := range bothDirections {
		emitter.fail("%s is reflected both as a request body and as a response, and one schema "+
			"cannot answer for both: a request body requires the fields tagged required:\"true\" "+
			"and a response the fields Go's marshaller always writes, so either reading is wrong "+
			"for one of its two directions. Give each direction a Go type of its own.",
			openAPIGoTypeName(body))
	}

	spec := emitter.reflector.SpecEns()
	spec.Openapi = "3.1.0"
	spec.Info.WithTitle("Dispatch HTTP API")
	spec.Info.WithVersion("1")
	spec.Info.WithDescription(
		"Generated from the Go route table (internal/dispatch/api/routes_table.go) and the Go " +
			"types beside it (routes_shapes.go) by `envoy-dispatch openapi`. Edit those, never " +
			"this document: it is committed so a client's types can be rendered from it, and " +
			"`envoy-dispatch openapi --check` refuses a tree where it is stale.")

	if err := emitter.resolveRawFields(); err != nil {
		return nil, err
	}

	hooks := openAPIHookRoutes()
	paths := map[string]*openapi31.PathItem{}
	operationIDs := map[string]string{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		name := route.Method + " " + route.Pattern
		operation, err := emitter.operation(route, hooks[name])
		if err != nil {
			return nil, err
		}
		id := *operation.ID
		if taken, clash := operationIDs[id]; clash {
			emitter.fail("operationId %s is taken by both %s and %s", id, taken, name)
		}
		operationIDs[id] = name
		item := paths[route.Pattern]
		if item == nil {
			item = &openapi31.PathItem{}
			paths[route.Pattern] = item
		}
		if err := item.SetOperation(strings.ToLower(route.Method), operation); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	for pattern, item := range paths {
		spec.PathsEns().WithMapOfPathItemValuesItem(pattern, *item)
	}

	for key := range rawFieldSchemas {
		if !emitter.rawFieldsSeen[key] {
			emitter.fail("rawFieldSchemas names %s, which is no field of any type the document "+
				"describes: drop the row or name the field it meant", key)
		}
	}
	for goType, name := range schemaNames {
		if emitter.schemaOwners[name] != goType {
			emitter.fail("schemaNames gives %s the name %s, and the document declares no schema "+
				"of that type: drop the row or name the type it meant", openAPIGoTypeName(goType), name)
		}
	}
	if len(emitter.failures) > 0 {
		sort.Strings(emitter.failures)
		return nil, fmt.Errorf("emit the OpenAPI document:\n\t%s",
			strings.Join(slices.Compact(emitter.failures), "\n\t"))
	}
	return spec, nil
}

// openAPIHookRoutes names every row the table mounts only with Deps.TestHooksEnabled, read as the
// difference between the two tables rather than listed, so a hook added to routes() is marked
// without this file being touched.
func openAPIHookRoutes() map[string]bool {
	served := map[string]bool{}
	for _, route := range (&server{}).routes() {
		served[route.Method+" "+route.Pattern] = true
	}
	hooks := map[string]bool{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		name := route.Method + " " + route.Pattern
		if !served[name] {
			hooks[name] = true
		}
	}
	return hooks
}
