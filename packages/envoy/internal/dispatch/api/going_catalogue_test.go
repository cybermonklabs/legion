// going_catalogue_test.go holds the catalogue to the tool specs it is built from and to the
// route table it is served beside: every row a write tool names is a write of that table, and
// the actions the catalogue serves are the twenty the answer run's decision was measured over.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// withThrowawayToolRoutes gives one tool a set of rows for the length of a test, so a build is
// shown failing on a row that is no write of the table. It writes those rows into goingWriteTools,
// the package's own map that every server reads at Register, and takes them out at the test's
// end, so a test that calls it does not run in parallel.
func withThrowawayToolRoutes(t *testing.T, tool string, rows ...goingToolRoute) {
	t.Helper()
	if _, held := goingWriteTools[tool]; held {
		t.Fatalf("%q already names rows; name a tool the catalogue does not hold", tool)
	}
	goingWriteTools[tool] = goingWriteTool{Routes: rows, Undo: goingUndoNotUndone, Class: goingClassField, Event: goingNoEvent}
	t.Cleanup(func() { delete(goingWriteTools, tool) })
}

// goingDecisionActionNames is the twenty actions the answer run's decision offers, which
// going/decision_models.json's measurement was made over.
var goingDecisionActionNames = []string{
	"dispatch_issue", "dispatch_issue_update", "dispatch_claim", "dispatch_ask", "dispatch_edit_ask",
	"dispatch_resolve_ask", "dispatch_resolve_comment", "dispatch_follow", "dispatch_comment",
	"dispatch_suggest", "dispatch_message", "dispatch_send_to_agent", "dispatch_doc_edit",
	"dispatch_request_approval", "dispatch_plan_approve", "dispatch_result", "dispatch_artifact",
	"dispatch_project", "dispatch_architecture_sync", "dispatch_handoff_write",
}

// TestTheCatalogueIsTheDispatchToolsThatWrite: the catalogue holds every write tool and nothing
// else — each entry carries that spec's own description and its own argument schema, unchanged
// — every row an entry names is a write of the table the server mounts, and the actions it
// serves are the twenty the answer run's decision offers, dispatch_review not among them.
func TestTheCatalogueIsTheDispatchToolsThatWrite(t *testing.T) {
	// Not parallel: it writes tools' rows into goingWriteTools (withThrowawayToolRoutes, and an
	// entry of its own below), the table every server of the package reads at Register.
	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	catalogue, err := buildGoingCatalogue(routes)
	if err != nil {
		t.Fatalf("build the catalogue of the served table: %v", err)
	}

	specs := make(map[string]contracts.DispatchTool, len(contracts.DispatchTools))
	for _, spec := range contracts.DispatchTools {
		specs[spec.Name] = spec
	}
	mutating := mutatingRoutes(routes)
	for name := range goingWriteTools {
		action, held := catalogue.Lookup(name)
		if !held {
			t.Errorf("the catalogue holds no %q, a write tool", name)
			continue
		}
		spec := specs[name]
		if action.Description != spec.Description {
			t.Errorf("%q serves a description the tool spec does not: %q", name, action.Description)
		}
		if !reflect.DeepEqual(action.Request, spec.Arguments) {
			t.Errorf("%q serves arguments the tool spec does not", name)
		}
		if string(action.Example) != string(spec.Example) {
			t.Errorf("%q serves the example %s, want the tool spec's %s", name, action.Example, spec.Example)
		}
		if len(action.Routes) == 0 {
			t.Errorf("%q names no route, so nothing performs it", name)
		}
		for _, route := range action.Routes {
			if !mutating[route.Method+" "+route.Pattern] {
				t.Errorf("%q names %s %s, which is no write of the route table", name, route.Method, route.Pattern)
			}
		}
	}
	if _, held := catalogue.Lookup("dispatch_search"); held {
		t.Error("the catalogue holds dispatch_search, a tool that writes nothing")
	}

	proposed := make([]string, 0, len(catalogue.Actions()))
	for _, action := range catalogue.Actions() {
		proposed = append(proposed, action.Action)
	}
	t.Logf("the catalogue holds %d write tools and serves %d actions", len(goingWriteTools), len(proposed))
	if !slices.Equal(proposed, goingDecisionActionNames) {
		t.Fatalf("the catalogue serves the actions %v, want the twenty the decision was measured over %v", proposed, goingDecisionActionNames)
	}

	t.Run("a tool naming a row the table does not hold fails the build", func(t *testing.T) {
		withThrowawayToolRoutes(t, "dispatch_whoami", goingToolRoute{
			Method: http.MethodPost, Pattern: "/api/v1/widgets/{id}/paint",
			Path: map[string]string{"id": "widget"},
		})
		_, err := buildGoingCatalogue(routes)
		if err == nil || !strings.Contains(err.Error(), "dispatch_whoami: POST /api/v1/widgets/{id}/paint is no row of the route table") {
			t.Fatalf("the build answered %v, want the row named as no row of the table", err)
		}
	})

	t.Run("a tool naming a read row fails the build", func(t *testing.T) {
		withThrowawayToolRoutes(t, "dispatch_whoami", goingToolRoute{
			Method: http.MethodGet, Pattern: "/api/v1/projects", Path: map[string]string{},
		})
		_, err := buildGoingCatalogue(routes)
		if err == nil || !strings.Contains(err.Error(), "dispatch_whoami: GET /api/v1/projects is no write of the route table") {
			t.Fatalf("the build answered %v, want the row named as no write", err)
		}
	})

	t.Run("rows named for a tool that does not exist fail the build", func(t *testing.T) {
		withThrowawayToolRoutes(t, "dispatch_paint_widget", goingToolRoute{
			Method: http.MethodPost, Pattern: "/api/v1/projects", Path: map[string]string{},
		})
		_, err := buildGoingCatalogue(routes)
		if err == nil || !strings.Contains(err.Error(), "dispatch_paint_widget: no dispatch tool of that name") {
			t.Fatalf("the build answered %v, want the tool name refused", err)
		}
	})

	t.Run("a placeholder no argument of the tool fills fails the build", func(t *testing.T) {
		withThrowawayToolRoutes(t, "dispatch_whoami", goingToolRoute{
			Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/claim",
			Path: map[string]string{"key": "issue"},
		})
		_, err := buildGoingCatalogue(routes)
		if err == nil || !strings.Contains(err.Error(), `dispatch_whoami: {key} is bound to "issue", which is no argument of the tool`) {
			t.Fatalf("the build answered %v, want the binding refused", err)
		}
	})

	t.Run("an entry naming rows for a tool whose spec binds its routes fails the build", func(t *testing.T) {
		tool := goingWriteTools["dispatch_rename_document"]
		t.Cleanup(func() { goingWriteTools["dispatch_rename_document"] = tool })
		named := tool
		named.Routes = []goingToolRoute{{Method: http.MethodPatch, Pattern: "/api/v1/artifacts/{id}",
			Path: map[string]string{"id": "document"}, Translate: goingTranslateNoBody}}
		goingWriteTools["dispatch_rename_document"] = named
		_, err := buildGoingCatalogue(routes)
		if err == nil || !strings.Contains(err.Error(), "dispatch_rename_document: its rows are stated by its entry and its spec's routes both") {
			t.Fatalf("the build answered %v, want the two statements of the rows refused", err)
		}
	})
}

// TestTheCatalogueRefusesAToolTheBarNeitherServesNorLeavesOut: find_tools answers every tool of
// Dispatch going.UnofferedTools leaves in, so a tool of Dispatch that binds no route, that neither
// goingWriteTools nor goingReadTools names and that the list does not name - a tool main adds -
// would be found by the bar's model and refused when it called it. The build refuses such a tool
// by name, as it refuses a spec that binds its routes and that neither table names; each tool the
// list names, a read tool of the table and the served table's own build are the controls.
func TestTheCatalogueRefusesAToolTheBarNeitherServesNorLeavesOut(t *testing.T) {
	t.Parallel()
	unserved := contracts.DispatchTool{Name: "dispatch_paint_widget", Arguments: []contracts.ToolField{
		{Name: "widget", Kind: contracts.ToolFieldString, Required: true},
	}}
	// Each refusal names the tool and the place a developer settles it: the list that leaves a tool
	// out for one that binds no route, the tables for one that binds its routes.
	if refusal := goingUnheldTool(unserved); !strings.HasPrefix(refusal, "dispatch_paint_widget: ") || !strings.Contains(refusal, "going.UnofferedTools") {
		t.Errorf("the build judges a tool that binds no route and that no table and no list names %q, want it refused by name, naming going.UnofferedTools", refusal)
	}
	bound := unserved
	bound.Routes = []contracts.ToolRoute{{Method: http.MethodGet, Pattern: "/api/v1/widgets/{id}", Path: map[string]string{"id": "widget"}}}
	if refusal := goingUnheldTool(bound); !strings.HasPrefix(refusal, "dispatch_paint_widget: ") || !strings.Contains(refusal, "goingWriteTools") {
		t.Errorf("the build judges a tool that binds its routes and that no table names %q, want it refused by name, naming the tables", refusal)
	}

	specs := make(map[string]contracts.DispatchTool, len(contracts.DispatchTools))
	for _, spec := range contracts.DispatchTools {
		specs[spec.Name] = spec
	}
	for name := range going.UnofferedTools {
		spec, stated := specs[name]
		if !stated {
			t.Errorf("going.UnofferedTools names %s, which is no tool of Dispatch", name)
			continue
		}
		if refusal := goingUnheldTool(spec); refusal != "" {
			t.Errorf("the build refuses %s, which going.UnofferedTools leaves out: %s", name, refusal)
		}
	}
	if refusal := goingUnheldTool(specs["dispatch_whoami"]); refusal != "" {
		t.Errorf("the build refuses dispatch_whoami, a read tool of goingReadTools: %s", refusal)
	}
	if _, err := BuildGoingCatalogue(); err != nil {
		t.Fatalf("build the catalogue of the served table: %v", err)
	}
}

// TestEveryGoingActionAcceptsItsExampleAndRefusesAMissingArgument: each entry's schema accepts
// the call the tool spec carries, and refuses that call with any one required argument left out.
func TestEveryGoingActionAcceptsItsExampleAndRefusesAMissingArgument(t *testing.T) {
	t.Parallel()
	catalogue, err := buildGoingCatalogue((&server{}).routes())
	if err != nil {
		t.Fatalf("build the catalogue: %v", err)
	}
	for _, action := range catalogue.Actions() {
		t.Run(action.Action, func(t *testing.T) {
			var example map[string]any
			if err := json.Unmarshal(action.Example, &example); err != nil {
				t.Fatalf("decode the example call: %v", err)
			}
			if refusal := checkGoingArguments(action.Request, example); refusal != "" {
				t.Fatalf("the schema refused its own example %s: %s", action.Example, refusal)
			}
			for _, field := range action.Request {
				if !field.Required {
					continue
				}
				short := maps.Clone(example)
				delete(short, field.Name)
				refusal := checkGoingArguments(action.Request, short)
				if !strings.Contains(refusal, `argument "`+field.Name+`" is required`) {
					t.Errorf("a call without %q was refused %q, want it named as required", field.Name, refusal)
				}
			}
		})
	}
}

// countingDoer is an HTTP client that records what it was asked to send, so a test can prove
// nothing was sent at all.
type countingDoer struct {
	requests []*http.Request
}

func (d *countingDoer) Do(request *http.Request) (*http.Response, error) {
	d.requests = append(d.requests, request)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: request}, nil
}

// TestAGoingActionTheCatalogueDoesNotHoldIsRefusedWithNoRequestMade: a proposal naming an action
// nobody serves is refused with the caller's own error, and the client is never asked to send
// anything. The action it does hold is the control: that one is sent.
func TestAGoingActionTheCatalogueDoesNotHoldIsRefusedWithNoRequestMade(t *testing.T) {
	t.Parallel()
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatalf("build the catalogue: %v", err)
	}
	doer := &countingDoer{}
	caller := GoingCaller{Catalogue: catalogue, BaseURL: "https://dispatch.invalid", Doer: doer}

	_, err = caller.Do(context.Background(), "dispatch_paint_widget", map[string]any{"id": "7"})
	var unknown GoingUnknownActionError
	if !errors.As(err, &unknown) || unknown.Action != "dispatch_paint_widget" {
		t.Fatalf("the caller answered %v, want the unknown action refused by name", err)
	}
	if len(doer.requests) != 0 {
		t.Fatalf("the caller sent %d request(s) for an action the catalogue does not hold", len(doer.requests))
	}

	// A read tool is no action of the catalogue either: the catalogue is the tools that write.
	_, err = caller.Do(context.Background(), "dispatch_search", map[string]any{"query": "astrolabe"})
	if !errors.As(err, &unknown) || unknown.Action != "dispatch_search" {
		t.Fatalf("the caller answered %v, want the read tool refused by name", err)
	}

	// A call the action's own schema refuses is the same: refused before anything is sent.
	_, err = caller.Do(context.Background(), "dispatch_issue", map[string]any{"project": "INFRA"})
	var refused GoingArgumentError
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, `argument "title" is required`) {
		t.Fatalf("the caller answered %v, want the missing title refused", err)
	}
	if len(doer.requests) != 0 {
		t.Fatalf("the caller sent %d request(s) for a call the schema refuses", len(doer.requests))
	}

	response, err := caller.Do(context.Background(), "dispatch_issue", map[string]any{"project": "INFRA", "title": "A new issue"})
	if err != nil {
		t.Fatalf("perform an action the catalogue holds: %v", err)
	}
	if response.StatusCode != http.StatusOK || len(doer.requests) != 1 {
		t.Fatalf("the caller sent %d request(s), want the one action it holds", len(doer.requests))
	}
	sent := doer.requests[0]
	if sent.Method != http.MethodPost || sent.URL.String() != "https://dispatch.invalid/api/v1/issues" {
		t.Fatalf("the caller sent %s %s", sent.Method, sent.URL)
	}

	t.Run("a placeholder is filled from the arguments rather than sent as a body field", func(t *testing.T) {
		doer := &countingDoer{}
		caller := GoingCaller{Catalogue: catalogue, BaseURL: "https://dispatch.invalid", Doer: doer}
		if _, err := caller.Do(context.Background(), "dispatch_claim", map[string]any{"issue": "INFRA-1042"}); err != nil {
			t.Fatalf("claim an issue: %v", err)
		}
		sent := doer.requests[0]
		if sent.Method != http.MethodPost || sent.URL.Path != "/api/v1/issues/INFRA-1042/claim" {
			t.Fatalf("the caller sent %s %s", sent.Method, sent.URL.Path)
		}
	})

	t.Run("the argument that tells two rows apart picks the row", func(t *testing.T) {
		doer := &countingDoer{}
		caller := GoingCaller{Catalogue: catalogue, BaseURL: "https://dispatch.invalid", Doer: doer}
		if _, err := caller.Do(context.Background(), "dispatch_claim", map[string]any{"issue": "INFRA-1042", "release": true}); err != nil {
			t.Fatalf("release a claim: %v", err)
		}
		sent := doer.requests[0]
		if sent.Method != http.MethodDelete || sent.URL.Path != "/api/v1/issues/INFRA-1042/claim" {
			t.Fatalf("the caller sent %s %s", sent.Method, sent.URL.Path)
		}
	})

	t.Run("a row addressing the calling session takes it from the caller", func(t *testing.T) {
		doer := &countingDoer{}
		caller := GoingCaller{Catalogue: catalogue, BaseURL: "https://dispatch.invalid", Session: "sess-7", Doer: doer}
		arguments := map[string]any{"ask": "01234567-0000-4000-8000-000000000001", "action": "follow"}
		if _, err := caller.Do(context.Background(), "dispatch_follow", arguments); err != nil {
			t.Fatalf("follow an ask: %v", err)
		}
		sent := doer.requests[0]
		if sent.Method != http.MethodPut || sent.URL.Path != "/api/v1/asks/01234567-0000-4000-8000-000000000001/followers/sess-7" {
			t.Fatalf("the caller sent %s %s", sent.Method, sent.URL.Path)
		}

		// A caller that is no session fills no such row, and the refusal says what each row
		// wanted rather than sending a request to a path with a hole in it.
		doer = &countingDoer{}
		caller = GoingCaller{Catalogue: catalogue, BaseURL: "https://dispatch.invalid", Doer: doer}
		_, err := caller.Do(context.Background(), "dispatch_follow", arguments)
		var refused GoingArgumentError
		if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "the calling session") {
			t.Fatalf("the caller answered %v, want the rows' needs named", err)
		}
		if len(doer.requests) != 0 {
			t.Fatalf("the caller sent %d request(s) for a call no row takes", len(doer.requests))
		}
	})
}

// TestACallTheBarCanNeverSendIsRefusedWithNoRequestMade: a call naming what the bar has no way to
// send - a picture file, a place given by a dispatch:// link that names no place such a write goes
// (the link of a comment, of a block or of a version of a document), a file to upload, an upload
// with no text, a closing note on an update that does not close the issue, a sync's repository not
// written as owner/name - is refused as the call's own argument error before any request, read or
// write, is made; the same call without it goes on to its requests. A picture list written empty
// names no picture, and the schema's bound refuses it first: a list of pictures holds one at least.
func TestACallTheBarCanNeverSendIsRefusedWithNoRequestMade(t *testing.T) {
	t.Parallel()
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatalf("build the catalogue: %v", err)
	}
	const issue = "INFRA-1042"
	edit := []any{map[string]any{"op": "replace", "find": "first line", "with": "opening line"}}
	for _, tc := range []struct {
		name, action      string
		refused, sendable map[string]any
	}{
		{"a comment with a picture", "dispatch_comment",
			map[string]any{"issue": issue, "body": "Reads well.", "images": []any{"shot.png"}},
			map[string]any{"issue": issue, "body": "Reads well."}},
		{"a comment with a list of no picture", "dispatch_comment",
			map[string]any{"issue": issue, "body": "Reads well.", "images": []any{}},
			map[string]any{"issue": issue, "body": "Reads well."}},
		{"a comment placed by the link of a comment", "dispatch_comment",
			map[string]any{"issue": issue, "body": "Reads well.", "ref": "dispatch://" + issue + "/comment/1"},
			map[string]any{"issue": issue, "body": "Reads well."}},
		{"a question with a picture", "dispatch_ask",
			map[string]any{"issue": issue, "question": "Ship it?", "images": []any{"shot.png"}},
			map[string]any{"issue": issue, "question": "Ship it?"}},
		{"a message with a picture", "dispatch_message",
			map[string]any{"issue": issue, "body": "Where are we?", "images": []any{"shot.png"}},
			map[string]any{"issue": issue, "body": "Where are we?"}},
		{"a suggestion placed by the link of a block", "dispatch_suggest",
			map[string]any{"quote": "says hello", "replace_with": "says hi", "ref": "dispatch://" + issue + "/spec#b-intro"},
			map[string]any{"issue": issue, "artifact": "spec", "quote": "says hello", "replace_with": "says hi"}},
		{"an edit placed by the link of a version", "dispatch_doc_edit",
			map[string]any{"ops": edit, "ref": "dispatch://" + issue + "/artifact/notes@v2"},
			map[string]any{"issue": issue, "artifact": "notes", "ops": edit}},
		{"an upload of a file", "dispatch_artifact",
			map[string]any{"issue": issue, "name": "notes.md", "path": "notes.md"},
			map[string]any{"issue": issue, "name": "notes.md", "content": "# Notes\n"}},
		{"an upload with no text", "dispatch_artifact",
			map[string]any{"issue": issue, "name": "notes.md"},
			map[string]any{"issue": issue, "name": "notes.md", "content": "# Notes\n"}},
		{"a closing note on an update that does not close the issue", "dispatch_issue_update",
			map[string]any{"issue": issue, "status": "in_progress", "reason": "Shipped in the last release."},
			map[string]any{"issue": issue, "status": "done", "reason": "Shipped in the last release."}},
		{"a sync of a repository not written as owner/name", "dispatch_architecture_sync",
			map[string]any{"project": "INFRA", "repo": "acme"},
			map[string]any{"project": "INFRA", "repo": "acme/arch"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doer := &countingDoer{}
			// In-process, as the bar calls: an upload carries its create-only condition on its
			// context, which only an in-process request takes.
			caller := GoingCaller{Catalogue: catalogue, Doer: doer}
			_, err := caller.Do(context.Background(), tc.action, tc.refused)
			var refused GoingArgumentError
			if !errors.As(err, &refused) || len(doer.requests) != 0 {
				t.Fatalf("the call answered %v after %d request(s), want it refused as its own argument error with none made", err, len(doer.requests))
			}
			if _, err := caller.Do(context.Background(), tc.action, tc.sendable); len(doer.requests) == 0 {
				t.Fatalf("the same call without what the bar cannot send answered %v with no request made, want it to go on to its requests", err)
			}
		})
	}
}

// TestAGoingRelationOutsideBothEdgeFamiliesFails: a catalogue entry names the relations its
// verbs write, and a name in neither edge family — the global relation names
// (model.GraphEdgeKinds) and the `block[]` attributes of the typed blocks (pmdoc.LinkKinds) — is
// refused, so no entry can promise an edge the graph has no kind for.
func TestAGoingRelationOutsideBothEdgeFamiliesFails(t *testing.T) {
	// Not parallel: it adds a tool's rows to goingWriteTools (withThrowawayToolRoutes), the table every
	// server of the package reads at Register.
	families := edgeFamilies()
	for _, relation := range append(slices.Clone(model.GraphEdgeKinds), pmdoc.LinkKinds()...) {
		if !slices.Contains(families, relation) {
			t.Errorf("the catalogue reads no %q, which one of the two edge families declares", relation)
		}
	}
	if slices.Contains(families, "reminds_of") {
		t.Fatal("reminds_of is a relation one of the edge families declares; pick a name neither has")
	}

	withThrowawayToolRoutes(t, "dispatch_whoami", goingToolRoute{
		Method: http.MethodPost, Pattern: "/api/v1/projects", Path: map[string]string{},
		Relations: []string{"mentions", "reminds_of"},
	})
	_, err := buildGoingCatalogue((&server{}).routes())
	if err == nil || !strings.Contains(err.Error(), `relation "reminds_of" is neither a structural edge kind`) {
		t.Fatalf("the build answered %v, want the relation in neither family named", err)
	}
	if strings.Contains(err.Error(), `relation "mentions"`) {
		t.Fatalf("the build refused a relation one family declares: %v", err)
	}
}

// TestTheGoingCatalogueIsMountedOnTheServedTable: the server builds its catalogue from the table
// it mounts, so the route serves what the tests above check rather than a copy built elsewhere.
func TestTheGoingCatalogueIsMountedOnTheServedTable(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/going/catalogue", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET the catalogue: status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeBody[goingCatalogueBody](t, response)
	if body.Total != len(body.Actions) || body.Total != len(goingDecisionActionNames) {
		t.Fatalf("the catalogue served %d of %d actions, want the %d the decision offers", len(body.Actions), body.Total, len(goingDecisionActionNames))
	}
	for _, action := range body.Actions {
		if action.Action != "dispatch_issue" {
			continue
		}
		want := ""
		for _, tool := range contracts.DispatchTools {
			if tool.Name == action.Action {
				want = tool.Description
			}
		}
		if want == "" || action.Description != want {
			t.Fatalf("the issue create's description is %q, want the tool spec's own, %q", action.Description, want)
		}
		if len(action.Routes) != 1 || action.Routes[0].Method != http.MethodPost || action.Routes[0].Pattern != "/api/v1/issues" {
			t.Fatalf("the issue create names the routes %v", action.Routes)
		}
		return
	}
	t.Fatalf("the served catalogue holds no issue create: %d actions", len(body.Actions))
}
