// repeated_query_keys_test.go holds every row of the table to the rule refuseRepeatedQueryKeys
// keeps: a query naming one key more than once is refused 400 INVALID_QUERY naming the key, before
// the row's handler runs, unless the row declares that key a list, whose every value the handler is
// handed.
package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// repeatProbeKey is a key no row declares, sent to every row: named twice it is refused as a
// declared one-value key is, and named once it reaches the handler as sent.
const repeatProbeKey = "undeclared_probe"

// notTheRepeatRefusal says how response differs from the refusal of a query naming key twice, and
// is empty when it is that refusal: 400 INVALID_QUERY with namedOnce's message naming key, the one
// wording every repeated key is refused in.
func notTheRepeatRefusal(response *httptest.ResponseRecorder, key string) string {
	want := namedOnce(url.Values{key: {"", ""}}, key).Error()
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusBadRequest || refusal.Code != "INVALID_QUERY" || refusal.Error != want {
		return fmt.Sprintf("answered %d %s, want 400 INVALID_QUERY %q",
			response.Code, strings.TrimSpace(response.Body.String()), want)
	}
	return ""
}

// contractQueryKeys are the query keys each operation of the committed document declares, by row
// (method and pattern): true for a list, a key whose schema is an array, and false for one value.
// It reads the document every client is generated from (committedOpenAPI) rather than the Go
// shapes the layer reads, so the walk below holds the layer to what a client is told and not to
// the layer's own input.
func contractQueryKeys(t *testing.T) map[string]map[string]bool {
	t.Helper()
	var document struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name   string `json:"name"`
				In     string `json:"in"`
				Schema struct {
					Type string `json:"type"`
				} `json:"schema"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(committedOpenAPI(t), &document); err != nil {
		t.Fatalf("read the committed document: %v", err)
	}
	rows := map[string]map[string]bool{}
	for pattern, item := range document.Paths {
		for method, operation := range item {
			keys := map[string]bool{}
			for _, parameter := range operation.Parameters {
				if parameter.In == "query" {
					keys[parameter.Name] = parameter.Schema.Type == "array"
				}
			}
			rows[strings.ToUpper(method)+" "+pattern] = keys
		}
	}
	return rows
}

// TestEverySingleValueQueryKeyRefusesARepeat builds every row's own chain as register does
// (routeHandler), test hooks in, with the row's handler replaced by one that records the query it
// is handed. Each row is sent, by the query keys the committed contract gives its operation: each
// one-value key twice, and repeatProbeKey twice, each answered 400 INVALID_QUERY naming the key
// with the handler not run; each list key twice, whose two values the handler is handed; and each
// of those keys once, which the handler is handed as sent.
//
// The server has no database and the requests carry no path parameter. A row behind a scope gate
// (OAuthScopes) is sent as a signed-in person, whose sign-in holds every scope; every other row is
// sent no credential, which each layer in front of a handler leaves to the handler's own refusal.
// A row is counted once its assertions have run, and the count is held to the rows the registered
// server mounts, so a walk that skipped a row, or walked none, fails.
func TestEverySingleValueQueryKeyRefusesARepeat(t *testing.T) {
	t.Parallel()
	served := register(http.NewServeMux(), t.Context(), Deps{
		TestHooksEnabled: true,
		Identity:         identity.HeaderIdentity{Header: "X-Dispatch-User", People: unrecordedPeople{}},
	})
	contract := contractQueryKeys(t)
	replays := served.keyedReplays()
	walked, oneValueKeys, listKeys := 0, 0, 0
	for _, route := range served.routes() {
		name := route.Method + " " + route.Pattern
		keys, described := contract[name]
		if !described {
			t.Errorf("%s is mounted and the committed contract describes no operation for it", name)
			continue
		}
		declared := slices.Collect(maps.Keys(keys))
		if params := route.Shape.Params; params != nil {
			declared = append(declared, queryKeysOf(params)...)
		}
		if slices.Contains(declared, repeatProbeKey) {
			t.Fatalf("%s declares %q, the key this walk sends as one no row declares: give the probe another name",
				name, repeatProbeKey)
		}

		ran := 0
		var handed url.Values
		route.Handler = func(w http.ResponseWriter, r *http.Request) {
			ran++
			handed = r.URL.Query()
			w.WriteHeader(http.StatusNoContent)
		}
		chain := served.routeHandler(route, replays[name])
		send := func(query url.Values) *httptest.ResponseRecorder {
			ran, handed = 0, nil
			request := httptest.NewRequest(route.Method, readOnlyProbePath(route.Pattern)+"?"+query.Encode(), nil)
			if len(route.OAuthScopes) > 0 {
				request.Header.Set("X-Dispatch-User", "alice")
			}
			response := httptest.NewRecorder()
			chain.ServeHTTP(response, request)
			return response
		}

		for _, key := range append(slices.Sorted(maps.Keys(keys)), repeatProbeKey) {
			twice := url.Values{key: {"first", "second"}}
			response := send(twice)
			switch list, declaredHere := keys[key]; {
			case list:
				listKeys++
				if ran != 1 || response.Code != http.StatusNoContent || !slices.Equal(handed[key], twice[key]) {
					t.Errorf("%s: %q, a list in the contract, named twice: the handler ran %d times with %s=%v (%d %s), "+
						"want it run once with both values", name, key, ran, key, handed[key], response.Code,
						strings.TrimSpace(response.Body.String()))
				}
			default:
				what := "one value in the contract"
				if !declaredHere {
					what = "a key the row does not declare"
				} else {
					oneValueKeys++
				}
				if ran != 0 {
					t.Errorf("%s: %q, %s, named twice: the handler ran %d times with %s=%v in place of 400 INVALID_QUERY",
						name, key, what, ran, key, handed[key])
				} else if why := notTheRepeatRefusal(response, key); why != "" {
					t.Errorf("%s: %q, %s, named twice: %s", name, key, what, why)
				}
			}

			once := url.Values{key: {"first"}}
			if response := send(once); ran != 1 || response.Code != http.StatusNoContent || !maps.EqualFunc(handed, once, slices.Equal) {
				t.Errorf("%s: %q named once: the handler ran %d times with %v (%d %s), want it run once with %v",
					name, key, ran, handed, response.Code, strings.TrimSpace(response.Body.String()), once)
			}
		}
		walked++
	}

	if mounted := len(served.routeIndex); walked == 0 || walked != mounted {
		t.Fatalf("walked %d rows, want every one of the %d the server mounts", walked, mounted)
	}
	if oneValueKeys == 0 || listKeys == 0 {
		t.Fatalf("the contract gave %d one-value keys and %d list keys over %d rows, so the walk proves nothing of one kind",
			oneValueKeys, listKeys, walked)
	}
	t.Logf("walked %d rows of the table (test hooks in): %d one-value keys, %d list keys and %q on each row",
		walked, oneValueKeys, listKeys, repeatProbeKey)
}
