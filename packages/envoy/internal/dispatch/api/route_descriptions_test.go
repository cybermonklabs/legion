package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// everyCallerPromises are the phrases a route description uses to say that any caller reads what
// the route answers. A read held to the caller's projects, people, sessions, graph or deployment
// answers each caller only what it may see, so its description must not promise it to everyone.
var everyCallerPromises = []string{"any authenticated caller", "every authenticated caller", "any signed-in"}

// TestNoReadDescriptionPromisesEveryCaller walks routes(): no GET row whose read classes include a
// class other than none and caller describes its answer as one every caller reads. A description
// is no comment: GET /api/v1 lists it and the OpenAPI document and every generated client carry
// it, so a sentence left from before a project could be private tells every reader the opposite of
// the rule the read binds. The walk fails naming the row and the phrase.
//
// The sub-test is the walk's control: GET /api/v1/priorities with the description it carried at
// 6237057940, which said any authenticated caller makes the read, fails it, named.
func TestNoReadDescriptionPromisesEveryCaller(t *testing.T) {
	t.Parallel()
	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	held := 0
	for _, route := range routes {
		if heldToTheCaller(route) {
			held++
		}
	}
	// A walk that held no row to the rule read nothing, which is no pass.
	if held == 0 {
		t.Fatal("the route table holds no GET row read for what its caller may see, so no description was checked")
	}
	for _, promise := range readDescriptionPromises(routes) {
		t.Error(promise)
	}

	t.Run("the priorities description of 6237057940 fails the walk", func(t *testing.T) {
		const before = "A priority belongs to no project, so this read is of the whole deployment and any authenticated caller makes it, the shared token every pod carries included."
		restored := slices.Clone(routes)
		index := slices.IndexFunc(restored, func(route apiRoute) bool {
			return route.Method == http.MethodGet && route.Pattern == "/api/v1/priorities"
		})
		if index < 0 {
			t.Fatal("the route table holds no GET /api/v1/priorities")
		}
		restored[index].Description += " " + before
		want := `GET /api/v1/priorities (reads deployment, project) says "any authenticated caller"`
		if got := readDescriptionPromises(restored); !slices.ContainsFunc(got, func(promise string) bool {
			return strings.HasPrefix(promise, want)
		}) {
			t.Errorf("the walk over the restored description answers %q, want a failure starting %q", got, want)
		}
	})
}

// heldToTheCaller reports whether route is a GET row that reads something other than rows of no
// one (none) and the caller's own (caller).
func heldToTheCaller(route apiRoute) bool {
	return route.Method == http.MethodGet && slices.ContainsFunc(route.Reads, func(class readclass.Class) bool {
		return class != readclass.None && class != readclass.Caller
	})
}

// readDescriptionPromises names each GET row held to its caller whose description makes one of
// everyCallerPromises, compared without regard to case, with the phrase it makes.
func readDescriptionPromises(routes []apiRoute) []string {
	var promises []string
	for _, route := range routes {
		if !heldToTheCaller(route) {
			continue
		}
		description := strings.ToLower(route.Description)
		for _, promise := range everyCallerPromises {
			if strings.Contains(description, promise) {
				promises = append(promises, fmt.Sprintf("%s %s (reads %s) says %q in its description, which promises its answer to every caller",
					route.Method, route.Pattern, readClassNames(route.Reads), promise))
			}
		}
	}
	return promises
}

// readClassNames is a row's read classes as a failure prints them.
func readClassNames(classes []readclass.Class) string {
	names := make([]string, len(classes))
	for index, class := range classes {
		names[index] = string(class)
	}
	return strings.Join(names, ", ")
}
