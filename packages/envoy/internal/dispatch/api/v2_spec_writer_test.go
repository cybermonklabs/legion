// v2_spec_writer_test.go holds R1 of INFRA-4764 - on an issue Legion runs, a version of its spec or
// its Verification document from a writer who is no person and no architect of the issue, or of an
// issue above it, is refused 403 DOCUMENT_WRITER_NOT_ARCHITECT (spec_writer.go) - to the /api/v2
// routes, which run the same cores as their /api/v1 predecessors: each /api/v2 route that writes
// such a version declares the refusal on its row, and refuses a worker's write as the /api/v1
// route does, on the rig of spec_writer_test.go. The /api/v2 routes that need the person scope
// (the accept and the reject of a suggestion, the answer and the decline of an ask, the rebuild)
// refuse a worker at their gate with 403 OAUTH_SCOPE_REQUIRED, as their /api/v1 routes do.
package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// v2SpecWriterRoutes are the /api/v2 routes whose write the check judges and may refuse, each with
// the /api/v1 route it answers for: the upload, the edit and the named version of a document, the
// reword and the resolution of a block ask, the create of an issue under a parent, and the issue
// patch, which closes, reopens, moves and takes an issue out of Legion, its status-change options
// among them. The accept and the reject of a suggestion, the answer and the decline of an ask, the
// rebuild and the decision's Keep need the person scope, which the check passes at once, so they
// declare no such refusal, as their /api/v1 routes need it too.
var v2SpecWriterRoutes = []string{
	"POST /api/v2/issues/{key}/artifacts",
	"POST /api/v2/artifacts/{id}/edits",
	"POST /api/v2/artifacts/{id}/versions",
	"PATCH /api/v2/asks/{id}",
	"PUT /api/v2/asks/{id}/state",
	"POST /api/v2/issues",
	"PATCH /api/v2/issues/{key}",
}

// TestTheV2SpecWriterRoutesDeclareTheRefusal: each /api/v2 route of v2SpecWriterRoutes declares the
// check's refusal on its row (specWriterNotArchitectRefusal), so the /api/v2 document lists 403
// DOCUMENT_WRITER_NOT_ARCHITECT on its operation, and no other /api/v2 route declares it.
func TestTheV2SpecWriterRoutesDeclareTheRefusal(t *testing.T) {
	mounted := map[string]bool{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		if route.Version != apiV2 {
			continue
		}
		name := route.Method + " " + route.Pattern
		mounted[name] = true
		declares := slices.Contains(route.Refusals, specWriterNotArchitectRefusal)
		listed := slices.Contains(v2SpecWriterRoutes, name)
		switch {
		case listed && !declares:
			t.Errorf("%s writes a version of an issue's spec or Verification document and declares no %s", name, specWriterNotArchitectRefusal.Code)
		case declares && !listed:
			t.Errorf("%s declares %s, and v2SpecWriterRoutes does not name it", name, specWriterNotArchitectRefusal.Code)
		}
	}
	for _, name := range v2SpecWriterRoutes {
		if !mounted[name] {
			t.Errorf("v2SpecWriterRoutes names %s, which the table does not serve", name)
		}
	}
}

// v2SpecWriteRow is one /api/v2 route's refused write: the write, the route it is sent on, and
// the run that sends it on a rig of its own as C's tester, on C's document.
type v2SpecWriteRow struct {
	route    string
	document string
	run      func(t *testing.T, at specWriteAt)
}

// TestV2SpecWriteRefusesAWorkerOnEveryRoute is TestSpecWriteRefusesAWorkerOnEveryRoute's
// inventory on /api/v2: each route of v2SpecWriterRoutes takes the write its /api/v1 route's row
// sends, from C's tester on C's spec or Verification document, and refuses it with 403
// DOCUMENT_WRITER_NOT_ARCHITECT naming the issue, the document, the writer and the architects on
// C's chain, the project holding the same issues, document versions and issue events after it as
// before it (specWriterRig.requireRefused). The rows' names are the routes they hold, so a route
// of v2SpecWriterRoutes with no row fails the test.
func TestV2SpecWriteRefusesAWorkerOnEveryRoute(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	attempt := func(at specWriteAt, kind specWriteKind, what string, send func(t *testing.T) *httptest.ResponseRecorder) specWriteAttempt {
		return specWriteAttempt{what: fmt.Sprintf("%s's %s on /api/v2", at.writer, what), kind: kind, send: send, refusal: at.refusal()}
	}
	rows := []v2SpecWriteRow{
		{route: "POST /api/v2/issues/{key}/artifacts", document: "verification.md", run: func(t *testing.T, at specWriteAt) {
			body := map[string]any{
				"name": at.document, "content": documentMarkdown(t, at.rig.handler, at.artifact()) + "\nA worker's paragraph.\n",
				"precondition": uploadBase(t, at.rig.handler, at.artifact()),
			}
			at.rig.requireRefused(t, attempt(at, specWriteKindDocument, "upload of "+at.issue.key+"'s "+at.document, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPost, "/api/v2/issues/"+at.issue.key+"/artifacts", body)
			}))
		}},
		{route: "POST /api/v2/artifacts/{id}/edits", document: "spec.md", run: func(t *testing.T, at specWriteAt) {
			at.rig.requireRefused(t, attempt(at, specWriteKindDocument, "edit of "+at.issue.key+"'s "+at.document, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPost, "/api/v2/artifacts/"+at.artifact()+"/edits", map[string]any{"ops": specWriterAppend})
			}))
		}},
		{route: "POST /api/v2/artifacts/{id}/versions", document: "verification.md", run: func(t *testing.T, at specWriteAt) {
			at.rig.requireRefused(t, attempt(at, specWriteKindDocument, "named version of "+at.issue.key+"'s "+at.document, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPost, "/api/v2/artifacts/"+at.artifact()+"/versions", map[string]any{"summary": "A worker's named version"})
			}))
		}},
		{route: "PATCH /api/v2/asks/{id}", document: "spec.md", run: func(t *testing.T, at specWriteAt) {
			ask := at.rig.askIn(t, at.issue, at.document, "tester")
			at.rig.requireRefused(t, attempt(at, specWriteKindDocument, "reword of its own block ask "+ask, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPatch, "/api/v2/asks/"+ask, map[string]any{"question": "Which way does the worker go now?"})
			}))
		}},
		{route: "PUT /api/v2/asks/{id}/state", document: "verification.md", run: func(t *testing.T, at specWriteAt) {
			ask := at.rig.askIn(t, at.issue, at.document, "architect")
			at.rig.requireRefused(t, attempt(at, specWriteKindDocument, "resolution of the architect's block ask "+ask, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPut, "/api/v2/asks/"+ask+"/state", map[string]any{"state": "resolved", "reason": "A worker settles it."})
			}))
			if state := at.rig.askState(t, ask); state != "open" {
				t.Errorf("the refused resolution left the ask %s %s, want it open", ask, state)
			}
		}},
		{route: "POST /api/v2/issues", document: "spec.md", run: func(t *testing.T, at specWriteAt) {
			title := "A worker's issue under " + at.issue.key + " on /api/v2"
			at.rig.requireRefused(t, attempt(at, specWriteKindCreate, "create of an issue under "+at.issue.key, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPost, "/api/v2/issues", map[string]any{
					"project": at.rig.project, "title": title, "parent": at.issue.key, "force": true,
				})
			}))
			if keys := at.rig.issuesTitled(t, title); len(keys) > 0 {
				t.Errorf("the refused create left the issues %v titled %q, want none", keys, title)
			}
		}},
		{route: "PATCH /api/v2/issues/{key}", document: "spec.md", run: func(t *testing.T, at specWriteAt) {
			at.requireCloseRefused(t, func(t *testing.T) *httptest.ResponseRecorder {
				return at.rig.send(t, at.writer, http.MethodPatch, "/api/v2/issues/"+at.issue.key, map[string]any{"status": "done"})
			})
		}},
	}
	for _, name := range v2SpecWriterRoutes {
		if !slices.ContainsFunc(rows, func(row v2SpecWriteRow) bool { return row.route == name }) {
			t.Errorf("%s writes a version of an issue's spec or Verification document, and no row of this test sends one", name)
		}
	}
	for _, row := range rows {
		t.Run(row.route, func(t *testing.T) {
			rig := server.rig(t, specWriterRigShape{})
			row.run(t, specWriteAt{rig: rig, writer: rig.cTester, issue: rig.c, document: row.document})
		})
	}
}

// TestV2SpecWriteKeepsThePersonScopeRefusals is TestSpecWriteKeepsThePersonScopeRefusals on
// /api/v2: the routes that write a version of a document and already need the person scope - the
// accept and the reject of a suggestion (PUT /api/v2/comments/{id}/state), the answer and the
// decline of an ask (PUT /api/v2/asks/{id}/state) and the rebuild (POST
// /api/v2/artifacts/{id}/rebuilds) - answer C's tester 403 OAUTH_SCOPE_REQUIRED, with no new
// version, the suggestion still open and the ask still open; a person's accept of the same
// suggestion is taken and writes a version.
func TestV2SpecWriteKeepsThePersonScopeRefusals(t *testing.T) {
	server := newSpecWriterServer(t, testServerOptions{})
	rig := server.rig(t, specWriterRigShape{})
	spec := rig.c.documents["spec.md"].id
	suggestion := decodeBody[model.Comment](t, rig.requireTaken(t, "C's tester's suggestion on C's spec", http.StatusCreated,
		func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, rig.cTester, http.MethodPost, "/api/v1/issues/"+rig.c.key+"/comments", map[string]any{
				"body": "Name the child.", "anchor": specWriterAnchor(spec, "child issue"),
				"suggestion": map[string]string{"replace_with": "child issue, C"},
			})
		})).ID
	ask := rig.askIn(t, rig.c, "spec.md", "architect")
	for _, route := range []struct {
		what, method, path string
		body               map[string]any
	}{
		{"accept of its suggestion", http.MethodPut, "/api/v2/comments/" + suggestion + "/state", map[string]any{"state": "accepted"}},
		{"reject of its suggestion", http.MethodPut, "/api/v2/comments/" + suggestion + "/state", map[string]any{"state": "rejected"}},
		{"answer of the architect's block ask", http.MethodPut, "/api/v2/asks/" + ask + "/state",
			map[string]any{"state": "answered", "selected": []string{"One"}, "expected_edited_at": nil}},
		{"decline of the architect's block ask", http.MethodPut, "/api/v2/asks/" + ask + "/state",
			map[string]any{"state": "declined", "reason": "Not the tester's to say."}},
		{"rebuild of C's spec", http.MethodPost, "/api/v2/artifacts/" + spec + "/rebuilds", nil},
	} {
		rig.requireRefusedWith(t, http.StatusForbidden, apicode.OAuthScopeRequired.Name(), specWriteAttempt{
			what: "C's tester's " + route.what + " on /api/v2",
			send: func(t *testing.T) *httptest.ResponseRecorder {
				return rig.send(t, rig.cTester, route.method, route.path, route.body)
			},
		})
	}
	if state := rig.askState(t, ask); state != "open" {
		t.Errorf("after the refused answer and decline the ask %s is %s, want it open", ask, state)
	}
	if resolved, _ := rig.commentState(t, suggestion); resolved {
		t.Errorf("after the refused accept and reject the suggestion %s is resolved, want it open", suggestion)
	}

	rig.requireVersionWritten(t, "a person's accept of C's tester's suggestion on /api/v2", http.StatusOK, spec, true,
		func(t *testing.T) *httptest.ResponseRecorder {
			return rig.sendAs(t, specWriterPerson, http.MethodPut, "/api/v2/comments/"+suggestion+"/state", map[string]any{"state": "accepted"})
		})
	if resolved, accepted := rig.commentState(t, suggestion); !resolved || accepted == nil || !*accepted {
		t.Errorf("after a person's accept the suggestion %s is resolved=%t accepted=%v, want it resolved and accepted", suggestion, resolved, accepted)
	}
}
