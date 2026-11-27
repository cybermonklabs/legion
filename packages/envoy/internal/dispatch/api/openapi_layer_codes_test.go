package api

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
)

// layerRefusal is one request a layer the mount wraps a /api/v2 route in refuses before the
// route's handler runs (routeHandler): the layer, the route, the code it answers, and the request.
type layerRefusal struct {
	layer   string
	method  string
	pattern string
	code    string
	request func(t *testing.T) *http.Request
}

// Each layer the mount wraps a /api/v2 route in can refuse a request before the route's handler
// runs, and each operation's x-refusal-codes lists every code those layers can answer it
// (openAPIOperationCodes), as the document's description says. One route of each layer is sent
// the request that layer refuses, and the code answered is held to that operation's list in the
// document.
func TestEachLayersRefusalIsAmongItsOperationsCodes(t *testing.T) {
	tour := newRouteTour(t)
	_, document := readOpenAPI(t, apiV2)
	asPerson := func(request *http.Request) *http.Request {
		request.Header.Set("X-Dispatch-User", "alice")
		return request
	}
	jsonRequest := func(method, target string, body []byte) *http.Request {
		request := httptest.NewRequest(method, target, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		return request
	}
	brokerToken := func(t *testing.T, says func(jkt string) ciphertoken.Introspection) (*brokerCaller, string) {
		t.Helper()
		caller := newBrokerCaller(t)
		token := "layer-token-" + uuid.NewString()
		tour.brokerTokens.Says(token, says(caller.jkt))
		return caller, token
	}
	unreadable := func(method, target string) *http.Request {
		request := httptest.NewRequest(method, target, iotest.ErrReader(errors.New("the connection broke")))
		request.Header.Set("Content-Type", "application/json")
		return request
	}
	retitle := []byte(`{"title":"Retitled by a layer's test"}`)

	for _, refusal := range []layerRefusal{
		{
			layer: "the read-only gate", method: http.MethodPatch, pattern: "/issues/{key}", code: "READ_ONLY_TOKEN",
			request: func(t *testing.T) *http.Request {
				caller, token := brokerToken(t, readOnlyToken)
				return caller.presenting(t, http.MethodPatch, "/api/v2/issues/"+tour.issue(t).Key, token, map[string]any{"title": "Retitled"})
			},
		},
		{
			layer: "the identity layer, a bearer that is no token", method: http.MethodGet, pattern: "/asks", code: "UNAUTHORIZED",
			request: func(*testing.T) *http.Request {
				request := httptest.NewRequest(http.MethodGet, "/api/v2/asks", nil)
				request.Header.Set("Authorization", "Bearer not-a-token")
				return request
			},
		},
		{
			layer: "the identity layer, a broker token the broker does not hold", method: http.MethodGet, pattern: "/asks",
			code: "BROKER_TOKEN_NOT_ACTIVE",
			request: func(t *testing.T) *http.Request {
				caller := newBrokerCaller(t)
				return caller.presenting(t, http.MethodGet, "/api/v2/asks", "a-token-nobody-issued-"+uuid.NewString(), nil)
			},
		},
		{
			layer: "the parameter check, a NUL", method: http.MethodGet, pattern: "/asks", code: "NUL_CHARACTER",
			request: func(*testing.T) *http.Request {
				return asPerson(httptest.NewRequest(http.MethodGet, "/api/v2/asks?session=a%00b", nil))
			},
		},
		{
			layer: "the parameter check, a byte that is not UTF-8", method: http.MethodGet, pattern: "/asks", code: "INVALID_UTF8",
			request: func(*testing.T) *http.Request {
				return asPerson(httptest.NewRequest(http.MethodGet, "/api/v2/asks?session=a%FFb", nil))
			},
		},
		{
			layer: "the id-prefix layer", method: http.MethodGet, pattern: "/comments/{id}", code: contracts.IDAmbiguous,
			request: func(t *testing.T) *http.Request {
				issue := tour.issue(t)
				prefix := fmt.Sprintf("ce%06x", tour.keys.Add(1))
				for nth := range 2 {
					seedExec(t, tour.database, "a comment sharing a prefix", `
						insert into comments (id, issue_key, author, body) values ($1::uuid, $2, $3::jsonb, $4)
					`, chosenID(prefix, nth+1), issue.Key, idPrefixAuthor, fmt.Sprintf("A remark %d.", nth))
				}
				return asPerson(httptest.NewRequest(http.MethodGet, "/api/v2/comments/"+prefix, nil))
			},
		},
		{
			layer: "the login check", method: http.MethodPatch, pattern: "/issues/{key}", code: contracts.DispatchLoginChanged,
			request: func(t *testing.T) *http.Request {
				request := asPerson(jsonRequest(http.MethodPatch, "/api/v2/issues/"+tour.issue(t).Key, retitle))
				request.Header.Set(contracts.DispatchLoginHeader, "bob")
				return request
			},
		},
		{
			layer: "the sign-in check", method: http.MethodPatch, pattern: "/issues/{key}", code: contracts.DispatchSignInRequired,
			request: func(t *testing.T) *http.Request {
				caller, token := brokerToken(t, serviceOnlyToken)
				return caller.presenting(t, http.MethodPatch, "/api/v2/issues/"+tour.issue(t).Key, token, map[string]any{"title": "Retitled"})
			},
		},
		{
			// A token scoped to one project may write another, public project's objects (#4682), so the
			// project check refuses only a private project's object, as a target that resolves to no
			// project.
			layer: "the project check, another project's object", method: http.MethodPatch, pattern: "/issues/{key}", code: "PROJECT_SCOPE",
			request: func(t *testing.T) *http.Request {
				issue := tour.issue(t)
				seedExec(t, tour.database, "the other project made private", `
					update projects set owner = 'alice', visibility = 'private' where key = $1
				`, tour.projectOf(t, issue))
				caller, token := brokerToken(t, liveNamedToken)
				return caller.presenting(t, http.MethodPatch, "/api/v2/issues/"+issue.Key, token, map[string]any{"title": "Retitled"})
			},
		},
		{
			layer: "the project check, a deployment's write", method: http.MethodPost, pattern: "/projects", code: "PROJECT_SCOPE_DEPLOYMENT",
			request: func(t *testing.T) *http.Request {
				caller, token := brokerToken(t, liveNamedToken)
				return caller.presenting(t, http.MethodPost, "/api/v2/projects", token, map[string]any{"key": "LAYER", "name": "Layer"})
			},
		},
		{
			layer: "the session check", method: http.MethodPost, pattern: "/agents/{session_id}/artifacts", code: "SESSION_SCOPE",
			request: func(t *testing.T) *http.Request {
				caller, token := brokerToken(t, liveNamedToken)
				return caller.presenting(t, http.MethodPost, "/api/v2/agents/session-someone-else/artifacts", token,
					map[string]any{"name": "notes.txt", "content": "Notes."})
			},
		},
		{
			layer: "the JSON body layer", method: http.MethodPatch, pattern: "/issues/{key}", code: "REQUEST_BODY_UNREADABLE",
			request: func(t *testing.T) *http.Request {
				return asPerson(unreadable(http.MethodPatch, "/api/v2/issues/"+tour.issue(t).Key))
			},
		},
		{
			layer: "the drain, a body past the bound of a write that reads none", method: http.MethodDelete, pattern: "/artifacts/{id}",
			code: "REQUEST_TOO_LARGE",
			request: func(*testing.T) *http.Request {
				return asPerson(jsonRequest(http.MethodDelete, "/api/v2/artifacts/"+uuid.NewString(), bytes.Repeat([]byte(" "), 3<<20)))
			},
		},
		{
			layer: "the upload layer, a multipart body it cannot read", method: http.MethodPost, pattern: "/agents/{session_id}/artifacts",
			code: "INVALID_BODY",
			request: func(*testing.T) *http.Request {
				request := asPerson(httptest.NewRequest(http.MethodPost, "/api/v2/agents/session-layer/artifacts", strings.NewReader("no parts")))
				request.Header.Set("Content-Type", "multipart/form-data")
				return request
			},
		},
		{
			layer: "the upload layer, a body neither multipart nor JSON", method: http.MethodPost, pattern: "/agents/{session_id}/artifacts",
			code: "JSON_CONTENT_TYPE",
			request: func(*testing.T) *http.Request {
				request := asPerson(httptest.NewRequest(http.MethodPost, "/api/v2/agents/session-layer/artifacts", strings.NewReader("notes")))
				request.Header.Set("Content-Type", "text/plain")
				return request
			},
		},
		{
			layer: "the keyed layer", method: http.MethodPost, pattern: "/agents/{session_id}/artifacts", code: "REQUEST_BODY_UNREADABLE",
			request: func(*testing.T) *http.Request {
				request := asPerson(unreadable(http.MethodPost, "/api/v2/agents/session-layer/artifacts"))
				request.Header.Set(contracts.IdempotencyKeyHeader, "layer-key-"+uuid.NewString())
				return request
			},
		},
		{
			layer: "the closed-issue check of an upload", method: http.MethodPost, pattern: "/issues/{key}/artifacts", code: "ISSUE_CLOSED",
			request: func(t *testing.T) *http.Request {
				issue := tour.issue(t)
				if closed := dispatchRequest(t, tour.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
					t.Fatalf("close %s: %d %s", issue.Key, closed.Code, closed.Body.String())
				}
				return asPerson(jsonRequest(http.MethodPost, "/api/v2/issues/"+issue.Key+"/artifacts", []byte(`{"name":"late.md","content":"# Late"}`)))
			},
		},
	} {
		t.Run(refusal.layer, func(t *testing.T) {
			operation, documented := document.Paths["/api/v2"+refusal.pattern][strings.ToLower(refusal.method)]
			if !documented {
				t.Fatalf("the /api/v2 document has no operation %s /api/v2%s", refusal.method, refusal.pattern)
			}
			response := httptest.NewRecorder()
			tour.handler.ServeHTTP(response, refusal.request(t))
			code, text := refusalOf(t, response)
			if code != refusal.code {
				t.Fatalf("%s %s answered %d %s %q, want the layer's %s", refusal.method, refusal.pattern, response.Code, code, text, refusal.code)
			}
			if _, listed := operation.RefusalCodes[code]; !listed {
				t.Errorf("%s /api/v2%s answers %s, which its x-refusal-codes does not list", refusal.method, refusal.pattern, code)
			}
		})
	}
}
