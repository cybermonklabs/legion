package api

// routes_v2_tour_refusals_test.go holds each refusal the route tour draws to the committed /api/v2
// document, as R6 of dispatch://INFRA-4071 asks: the document states what the server answers, so
// the operation a refusal answers on names its code, at the status it was answered, in its
// x-refusal-codes, and declares a response of that status, or a default one, whose schema its
// body holds to. A code the document lists once for the whole of /api/v2 tells a client nothing
// about the operation it calls.

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// tourRefusal is a refusal a route's handler answers that no other subtest of the tour sends: the
// change to its step's request that draws it, and the code it is answered with.
type tourRefusal struct {
	Name   string
	Change func(t *testing.T, tour *routeTour, request tourRequest) tourRequest
	Code   apicode.Code
}

// tourRefusals are the refusals of tourRefusal by the route their step calls, "<method>
// /api/v2<pattern>". Each is listed in its operation's x-refusal-codes at its status, with a declared response body.
var tourRefusals = map[string][]tourRefusal{
	"GET /api/v2/deploy-runs/{id}": {
		{Name: "an id that is not a positive integer", Change: refusalPath("id", "0"), Code: apicode.RunIDInput},
		{Name: "an id no run of the deploy repository has", Change: refusalPath("id", "999999"), Code: apicode.NotFound},
		{Name: "no delivery settings", Change: withoutDeliverySettings, Code: apicode.DeliveryNotConfigured},
	},
	"GET /api/v2/nodes": {
		{Name: "a project that is no project key", Change: refusalQuery("project", "lower"), Code: apicode.InvalidProject},
	},
	"GET /api/v2/agents/{session_id}/answers": {
		{Name: "a session_id that is no session id", Change: refusalPath("session_id", "no session"), Code: apicode.InvalidSessionID},
	},
	"GET /api/v2/search-results": {
		{Name: "a project that is no project key", Change: refusalQuery("project", "lower"), Code: apicode.InvalidProject},
		{Name: "a number_project that is no project key", Change: refusalQuery("number_project", "lower"), Code: apicode.InvalidNumberProject},
		{Name: "a status that is no issue status", Change: refusalQuery("status", "nope"), Code: apicode.InvalidStatus},
		{Name: "an author that is no actor id", Change: refusalQuery("author", "has space"), Code: apicode.InvalidAuthor},
	},
	"GET /api/v2/related-check-stats": {
		{Name: "a project that is no project key", Change: refusalQuery("project", "lower"), Code: apicode.InvalidProject},
	},
	"GET /api/v2/repositories/{owner}/{repo}/app-installation": {
		{Name: "an owner that is no GitHub owner", Change: refusalPath("owner", "bad owner"), Code: apicode.InvalidRepository},
	},
	"GET /api/v2/reference-targets": {
		{Name: "a ref that is no reference", Change: refusalQuery("ref", "not a ref"), Code: apicode.InvalidReference},
	},
	"DELETE /api/v2/projects/{key}/architecture-sources/{owner}/{repo}": {
		{Name: "a repo that is no GitHub name", Change: refusalPath("repo", "bad repo"), Code: apicode.SourceInput},
	},
	"PUT /api/v2/projects/{key}/architecture-sources/{owner}/{repo}": {
		{Name: "a blank branch", Change: refusalBody("branch", "  "), Code: apicode.SourceInput},
	},
	"POST /api/v2/session-groups": {
		{Name: "a blank name", Change: refusalBody("name", "  "), Code: apicode.SessionGroupInput},
	},
	"DELETE /api/v2/session-groups/{id}": {
		{Name: "an id that is no uuid", Change: refusalPath("id", "not-a-uuid"), Code: apicode.SessionGroupNotFound},
	},
	"PATCH /api/v2/session-groups/{id}": {
		{Name: "an id that is no uuid", Change: refusalPath("id", "not-a-uuid"), Code: apicode.SessionGroupNotFound},
	},
	"POST /api/v2/projects": {
		{Name: "a key that is no project key", Change: refusalBody("key", "lower"), Code: apicode.InvalidProject},
	},
	"POST /api/v2/priorities": {
		{Name: "a blank title", Change: refusalBody("title", "  "), Code: apicode.PriorityInput},
	},
	"PATCH /api/v2/priorities/{id}": {
		{Name: "an expected title the priority no longer stores", Change: refusalBody("expected", map[string]any{"title": "A title the tour never wrote"}), Code: apicode.PriorityChanged},
		{Name: "an expected description beside no description written", Change: refusalBody("expected", map[string]any{"description": "A description the tour never wrote"}), Code: apicode.PriorityInput},
	},
	"POST /api/v2/artifacts/{id}/edits": {
		{Name: "a precondition behind the document", Change: refusalBody("precondition", map[string]any{"document": tourStaleDocumentToken}), Code: apicode.PreconditionFailed},
		{Name: "a precondition naming neither document nor blocks", Change: refusalBody("precondition", map[string]any{}), Code: apicode.InvalidPrecondition},
		{Name: "a quote the spec does not hold", Change: refusalBody("ops", []map[string]any{{"op": "replace", "find": "words the tour never wrote", "with": "new words"}}), Code: apicode.TargetNotFound},
		{Name: "a quote the spec holds in several places", Change: refusalBody("ops", []map[string]any{{"op": "replace", "find": "a", "with": "the"}}), Code: apicode.TargetAmbiguous},
		{Name: "a check block that breaks a rule of a check CI proves", Change: refusalBody("ops", []map[string]any{{"op": "insert", "after": "end", "markdown": tourCICheckBroken}}), Code: apicode.InvalidCICheck},
	},
	"POST /api/v2/artifacts/{id}/comments": {
		{Name: "a quote the document does not hold", Change: refusalDocumentAnchor("quote", "words the tour never wrote"), Code: apicode.TargetNotFound},
		{Name: "a quote the document holds in several places", Change: refusalDocumentAnchor("quote", "a"), Code: apicode.TargetAmbiguous},
		{Name: "a mark_id the document does not hold", Change: refusalDocumentAnchor("mark_id", "mark-the-tour-never-made"), Code: apicode.AnchorMissing},
	},
	"POST /api/v2/artifacts/{id}/asks": {
		{Name: "a quote the document does not hold", Change: refusalDocumentAnchor("quote", "words the tour never wrote"), Code: apicode.TargetNotFound},
		{Name: "a quote the document holds in several places", Change: refusalDocumentAnchor("quote", "a"), Code: apicode.TargetAmbiguous},
		{Name: "a mark_id the document does not hold", Change: refusalDocumentAnchor("mark_id", "mark-the-tour-never-made"), Code: apicode.AnchorMissing},
	},
	"POST /api/v2/issues/{key}/comments": {
		{Name: "a quote the spec does not hold", Change: refusalBody("anchor", map[string]any{"artifact": "spec", "quote": "words the tour never wrote"}), Code: apicode.TargetNotFound},
		{Name: "a quote the spec holds in several places", Change: refusalBody("anchor", map[string]any{"artifact": "spec", "quote": "a"}), Code: apicode.TargetAmbiguous},
		{Name: "an anchor naming no document of the issue", Change: refusalBody("anchor", map[string]any{"artifact": "no-such-document", "quote": "a"}), Code: apicode.NotFound},
	},
	"POST /api/v2/issues/{key}/asks": {
		{Name: "a quote the spec does not hold", Change: refusalBody("anchor", map[string]any{"artifact": "spec", "quote": "words the tour never wrote"}), Code: apicode.TargetNotFound},
		{Name: "a quote the spec holds in several places", Change: refusalBody("anchor", map[string]any{"artifact": "spec", "quote": "a"}), Code: apicode.TargetAmbiguous},
		{Name: "an anchor naming no document of the issue", Change: refusalBody("anchor", map[string]any{"artifact": "no-such-document", "quote": "a"}), Code: apicode.NotFound},
	},
	"PATCH /api/v2/issues/{key}": {
		{Name: "a status-change option beside ordinary patch fields",
			Change: refusalBody("include_children", true), Code: apicode.StatusChangeInput},
		{Name: "a preview blocked by an open child",
			Change: func(t *testing.T, tour *routeTour, request tourRequest) tourRequest {
				key := request.Path["key"]
				tourStatusChild(t, tour, tourIssue{Key: key})
				request.Body = map[string]any{"status": "done", "dry_run": true}
				return request
			}, Code: apicode.ParentStatusConflict},
		{Name: "a keyed status change with no credential", Change: refusalKeyedStatusChange, Code: apicode.NoIdentity},
	},
	"POST /api/v2/issues": {
		{Name: "a spec that is no Proof document", Change: refusalBody("spec", tourMarkdownRefused), Code: apicode.InvalidMarkdown},
		{Name: "a spec whose ask breaks its content rule", Change: refusalBody("spec", tourAskBlockBroken), Code: apicode.InvalidAskBlock},
		{Name: "a spec whose check breaks a rule of a check CI proves", Change: refusalBody("spec", tourCICheckBroken), Code: apicode.InvalidCICheck},
	},
	"POST /api/v2/issues/{key}/artifacts": {
		{Name: "content that is no Proof document", Change: refusalBody("content", tourMarkdownRefused), Code: apicode.InvalidMarkdown},
		{Name: "an ask that breaks its content rule", Change: refusalBody("content", tourAskBlockBroken), Code: apicode.InvalidAskBlock},
		{Name: "a check that breaks a rule of a check CI proves", Change: refusalBody("content", tourCICheckBroken), Code: apicode.InvalidCICheck},
		{Name: "a precondition behind the document it replaces", Change: refusalStaleIssueUpload, Code: apicode.PreconditionFailed},
		{Name: "a precondition behind the file it replaces", Change: refusalStaleFileUpload("/api/v2/issues/{key}/artifacts"), Code: apicode.PreconditionFailed},
	},
	"POST /api/v2/projects/{key}/artifacts": {
		{Name: "content that is no Proof document", Change: refusalBody("content", tourMarkdownRefused), Code: apicode.InvalidMarkdown},
		{Name: "a precondition behind the document it replaces", Change: refusalStaleProjectUpload, Code: apicode.PreconditionFailed},
		{Name: "a precondition behind the file it replaces", Change: refusalStaleFileUpload("/api/v2/projects/{key}/artifacts"), Code: apicode.PreconditionFailed},
	},
	"POST /api/v2/agents/{session_id}/artifacts": {
		{Name: "a precondition behind the file it replaces", Change: refusalStaleFileUpload("/api/v2/agents/{session_id}/artifacts"), Code: apicode.PreconditionFailed},
	},
	"GET /api/v2/credential-grants": {
		{Name: "the broker's refusal of the approver", Change: brokerRefuses(http.MethodGet, "/v1/grants", "APPROVER_REQUIRED", http.StatusBadRequest), Code: apicode.ApproverRequired},
	},
	"DELETE /api/v2/credential-grants/{id}": {
		{Name: "a grant the broker saw move", Change: brokerRefuses(http.MethodPost, "/v1/grants/{id}/revoke-by-approver", "GRANT_MOVING", http.StatusConflict), Code: apicode.GrantMoving},
		{Name: "a code of the broker's the route does not declare", Change: brokerRefuses(http.MethodPost, "/v1/grants/{id}/revoke-by-approver", "GRANT_SHIFTING", http.StatusConflict), Code: apicode.AgentSecretsRefusalUndeclared},
	},
	"GET /api/v2/credential-requests": {
		{Name: "the broker's refusal of the states", Change: brokerRefuses(http.MethodGet, "/v1/pending", "STATE_INPUT", http.StatusBadRequest), Code: apicode.StateInput},
	},
	"PUT /api/v2/credential-requests/{id}/state": {
		{Name: "a code that is not the login's", Change: brokerRefuses(http.MethodPost, "/v1/credential-requests/{id}/approve", "CODE_MISMATCH", http.StatusForbidden), Code: apicode.CodeMismatch},
		{Name: "a secret request, decided on the broker's page", Change: brokerAnswers(http.MethodPost, "/v1/credential-requests/{id}/approve", http.StatusForbidden, map[string]string{
			"code": "DECIDE_ON_BROKER_PAGE", "error": "decide it on the broker's page", "approve_url": "https://secrets.example.com/approve/tour", "answer_id": "tour-broker-answer",
		}), Code: apicode.DecideOnBrokerPage},
	},
	"POST /api/v2/machine-login-lookups": {
		{Name: "a code no machine login has", Change: brokerRefuses(http.MethodPost, "/v1/machine-logins/lookup", "NO_SUCH_CODE", http.StatusNotFound), Code: apicode.NoSuchCode},
	},
	"GET /api/v2/machine-logins": {
		{Name: "Dispatch's own account, which the broker does not list", Change: brokerRefuses(http.MethodGet, "/v1/launcher-credentials", "ACCOUNT_NOT_LISTED", http.StatusForbidden), Code: apicode.AccountNotListed},
	},
	"DELETE /api/v2/machine-logins/{id}": {
		{Name: "a machine login the caller did not approve", Change: brokerRefuses(http.MethodPost, "/v1/launcher-credentials/{id}/revoke-by-approver", "NOT_APPROVER", http.StatusForbidden), Code: apicode.NotApprover},
	},
	"GET /api/v2/me/signins": {
		{Name: "the broker's refusal of the person", Change: brokerRefuses(http.MethodGet, "/v1/person-signins", "PERSON_REQUIRED", http.StatusBadRequest), Code: apicode.PersonRequired},
	},
	"DELETE /api/v2/me/signins/{id}": {
		{Name: "a sign-in id the broker refuses", Change: brokerRefuses(http.MethodPost, "/v1/person-signins/{id}/revoke", "SIGNIN_ID_INPUT", http.StatusBadRequest), Code: apicode.SigninIDInput},
		{Name: "Dispatch's own account, past the broker's rate", Change: brokerRefuses(http.MethodPost, "/v1/person-signins/{id}/revoke", "RATE_LIMITED", http.StatusTooManyRequests), Code: apicode.RateLimited},
	},
}

// TestV2RouteTourRefusalsAreListedOnTheirOperations sends, for each step of the route tour, the
// refusals the tour's own subtests send - a query key the route does not declare
// (unknown-keys), each path value that names no stored object (missing-objects), no credential
// on a route that is not public (unauthenticated) - and the refusals of tourRefusals, and holds
// each answer to its operation in the committed /api/v2 document (holdRefusal).
func TestV2RouteTourRefusalsAreListedOnTheirOperations(t *testing.T) {
	tour := newRouteTour(t)
	contract := newTourContract(t)
	steps := tourSteps()
	first := map[string]tourStep{}
	for _, step := range steps {
		if _, seen := first[step.route()]; !seen {
			first[step.route()] = step
		}
		t.Run(step.name(), func(t *testing.T) {
			route := tour.stepRoute(t, step)
			t.Run("an undeclared query key", func(t *testing.T) {
				request := step.Setup(t, tour)
				request.Query = withQueryValue(request.Query, "zz_undeclared", "1")
				contract.holdRefusal(t, route, tour, request, apicode.QueryKeyUnknown)
			})
			for _, missing := range step.Missing {
				t.Run("{"+missing.Param+"} naming no stored object", func(t *testing.T) {
					request := step.Setup(t, tour)
					request.Path = maps.Clone(request.Path)
					request.Path[missing.Param] = missing.Value
					contract.holdRefusal(t, route, tour, request, missing.Code)
				})
			}
			if route.Auth != authPublic {
				t.Run("no credential", func(t *testing.T) {
					request := step.Setup(t, tour)
					request.As = tourCaller{}
					contract.holdRefusal(t, route, tour, request, cmp.Or(step.NoCredential, apicode.NoIdentity))
				})
			}
		})
	}
	for _, name := range slices.Sorted(maps.Keys(tourRefusals)) {
		step, stepped := first[name]
		if !stepped {
			t.Errorf("tourRefusals names %s, which no step of the route tour calls", name)
			continue
		}
		for _, refusal := range tourRefusals[name] {
			t.Run(name+": "+refusal.Name, func(t *testing.T) {
				route := tour.stepRoute(t, step)
				request := refusal.Change(t, tour, step.Setup(t, tour))
				contract.holdRefusal(t, route, tour, request, refusal.Code)
			})
		}
	}
}

// holdRefusal sends request to route and holds its answer: answered code at its status, a code
// the operation lists at that status in x-refusal-codes, and a body that holds to the response
// the operation declares at that status, or to its default response where it declares none.
func (contract *tourContract) holdRefusal(t *testing.T, route apiRoute, tour *routeTour, request tourRequest, want apicode.Code) {
	t.Helper()
	response := tour.send(t, route, request, "")
	code, _ := refusalOf(t, response)
	if code != want.Name() || response.Code != want.Status() {
		t.Fatalf("answered %d %s, want %d %s", response.Code, response.Body.String(), want.Status(), want.Name())
	}
	method, status := strings.ToLower(route.Method), strconv.Itoa(response.Code)
	operation := openAPIAt(contract.document, "paths", route.Pattern, method)
	listed, _ := operation["x-refusal-codes"].(map[string]any)
	if at, held := listed[code]; !held || fmt.Sprint(at) != status {
		t.Errorf("%s %s answered %s %s, and its x-refusal-codes lists it at %v (absent: %v)",
			route.Method, route.Pattern, status, code, at, !held)
	}
	if openAPIAt(operation, "responses", status) != nil {
		contract.hold(t, route, response)
		return
	}
	if openAPIAt(operation, "responses", "default", "content", "application/json") == nil {
		t.Fatalf("%s %s answered the refusal %s %s, and declares neither a response at %s nor a default JSON one",
			route.Method, route.Pattern, status, code, status)
	}
	schema := contract.schemaAt(t, "paths", route.Pattern, method, "responses", "default", "content", "application/json", "schema")
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(response.Body.Bytes()))
	if err != nil {
		t.Fatalf("answered %s %s, which is no JSON: %v", status, response.Body.String(), err)
	}
	if err := schema.Validate(value); err != nil {
		t.Errorf("the refusal %s %s of %s %s holds to no default response the document declares:\n%v",
			status, code, route.Method, route.Pattern, err)
	}
}

// refusalPath changes the value of the path wildcard name to value.
func refusalPath(name, value string) func(*testing.T, *routeTour, tourRequest) tourRequest {
	return func(_ *testing.T, _ *routeTour, request tourRequest) tourRequest {
		request.Path = maps.Clone(request.Path)
		request.Path[name] = value
		return request
	}
}

// refusalQuery sets the query key name to value.
func refusalQuery(name, value string) func(*testing.T, *routeTour, tourRequest) tourRequest {
	return func(_ *testing.T, _ *routeTour, request tourRequest) tourRequest {
		request.Query = withQueryValue(request.Query, name, value)
		return request
	}
}

// withoutDeliverySettings deletes the deployment's delivery settings for the length of the
// subtest, and writes them back after it, so later steps find them as their setup left them.
func withoutDeliverySettings(t *testing.T, tour *routeTour, request tourRequest) tourRequest {
	t.Helper()
	settings, err := delivery.GetSettings(t.Context(), tour.database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	if _, err := tour.database.Pool.Exec(t.Context(), `delete from delivery_settings`); err != nil {
		t.Fatalf("delete the delivery settings: %v", err)
	}
	t.Cleanup(func() {
		if _, err := delivery.PutSettings(context.Background(), tour.database.Pool, settings, model.Actor{Kind: "system", ID: "tour"}); err != nil {
			t.Errorf("write the delivery settings back: %v", err)
		}
	})
	return request
}
