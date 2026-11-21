// going_conditions_test.go holds the compare-and-sets of the command bar's in-process caller to
// the one way they reach a route, the request's context (going_conditions.go): each route decides
// the condition it is carried, a write back that carries none its route decides is refused with
// nothing written, and a request off the network that names a condition in its body makes
// nothing conditional, as main's lenient decoding ignores any member a route does not declare.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingConditionFixtures are the items the condition tests write: an issue with its notes, an ask
// on it, a project document, a file of the planner session's, an issue alice holds, one nobody
// holds and a thread alice resolved.
type goingConditionFixtures struct {
	issue, ask, claimed, unclaimed, comment string
}

func newGoingConditionFixtures(t *testing.T, rig goingWritesRig) goingConditionFixtures {
	t.Helper()
	var f goingConditionFixtures
	f.issue = createInteractionIssue(t, rig.mux, "DEMO", "An issue the conditions write", "The spec.\n").Key
	owner := "/api/v1/issues/" + f.issue
	uploadDocument(t, rig.mux, owner, "notes.md", "# Notes\n\nThe first line.\n")
	uploadDocument(t, rig.mux, "/api/v1/projects/DEMO", "guide.md", "# Guide\n\nRead this first.\n")
	if status, body := goingMultipartUpload(t, rig.mux, "/api/v1/agents/"+goingPlanner+"/artifacts", map[string]string{"name": "notes.txt"}, "Plain words."); status != http.StatusCreated {
		t.Fatalf("upload the planner's file: %d %s", status, body)
	}
	f.ask = rig.ask(t, f.issue, "Ship it on Friday?")
	f.claimed = rig.issue(t, "An issue alice holds")
	rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+f.claimed+"/claim", map[string]any{})
	f.unclaimed = rig.issue(t, "An issue nobody holds")
	f.comment = decodeBody[model.Comment](t, rig.fixture(t, "alice", http.MethodPost, owner+"/comments", map[string]any{"body": "A thread to resolve."})).ID
	rig.fixture(t, "alice", http.MethodPost, "/api/v1/comments/"+f.comment+"/resolve", nil)
	return f
}

// goingConditioned sends spec to handler in-process as login's own sign-in, carrying condition on
// its context, as the command bar's caller sends a write back or an upload, and answers its status
// and body.
func goingConditioned(t *testing.T, handler http.Handler, login string, spec goingSend, condition goingCondition) (int, string) {
	t.Helper()
	person := model.Actor{Kind: "user", ID: login, Credential: personCredential, Proven: true}
	spec.condition = &condition
	doer := (&server{inProcess: handler}).goingDoer(goingBarCaller(person, true, goingTestRun), testServerURL)
	return goingInProcessSend(t, doer, spec)
}

// goingJSONSend is one JSON request of body.
func goingJSONSend(t *testing.T, method, path string, body any) goingSend {
	t.Helper()
	spec, err := goingJSONRequest(method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// goingMultipartSend is one multipart upload of fields and one text file.
func goingMultipartSend(t *testing.T, path string, fields map[string]string, content string) goingSend {
	t.Helper()
	form := goingUpload{fields: fields, filename: fields["name"], contentType: "text/plain; charset=utf-8", content: []byte(content)}
	body, contentType, err := form.encode()
	if err != nil {
		t.Fatal(err)
	}
	return goingSend{method: http.MethodPost, path: path, contentType: contentType, body: body}
}

// goingAnswerCode is the code of a refusal's body, "" for a body that is none.
func goingAnswerCode(t *testing.T, answer string) string {
	t.Helper()
	var refusal errorBody
	_ = json.Unmarshal([]byte(answer), &refusal)
	return refusal.Code
}

// goingConditionState is what the items the condition tests write hold, read off the store: the
// issue's title, the ask's question, the number of versions of each document and file, who holds
// each of the two issues and whether the thread is resolved. A refused write leaves it as it was.
func goingConditionState(t *testing.T, rig goingWritesRig, f goingConditionFixtures) string {
	t.Helper()
	var state string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select concat_ws(' | ',
			(select title from issues where key = $1),
			(select question from asks where id = $2::uuid),
			(select string_agg(a.name || '=' || (select count(*) from artifact_versions v where v.artifact_id = a.id), ',' order by a.name) from artifacts a),
			(select coalesce(claimed_by::text, 'unclaimed') from issues where key = $3),
			(select coalesce(claimed_by::text, 'unclaimed') from issues where key = $4),
			(select resolved::text from comments where id = $5::uuid))`,
		f.issue, f.ask, f.claimed, f.unclaimed, f.comment).Scan(&state); err != nil {
		t.Fatalf("read what the items hold: %v", err)
	}
	return state
}

// goingConditionWrites is one write of each route that takes a condition, by name: the issue
// update, the ask edit, the issue's, the project's and a session's upload, the claim of an issue
// nobody holds and of one alice holds, the release, the reopen and the resolve. The issue's and
// the project's uploads replace a document, so each names the token of the version it replaces,
// read off handler when it is sent, as the command bar's upload names it (main's upload
// precondition).
var goingConditionWrites = map[string]func(t *testing.T, handler http.Handler, f goingConditionFixtures) goingSend{
	"issue update": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingJSONSend(t, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{"title": "Written in process"})
	},
	"ask edit": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingJSONSend(t, http.MethodPatch, "/api/v1/asks/"+f.ask, map[string]any{"question": "Written in process?"})
	},
	"issue upload": func(t *testing.T, handler http.Handler, f goingConditionFixtures) goingSend {
		target := "/api/v1/issues/" + f.issue + "/artifacts"
		return goingJSONSend(t, http.MethodPost, target, map[string]any{"name": "notes.md", "content": "# Notes\n\nWritten in process.\n",
			"precondition": uploadBaseNamed(t, handler, target, "notes.md")})
	},
	"project upload": func(t *testing.T, handler http.Handler, f goingConditionFixtures) goingSend {
		target := "/api/v1/projects/DEMO/artifacts"
		return goingJSONSend(t, http.MethodPost, target, map[string]any{"name": "guide.md", "content": "# Guide\n\nWritten in process.\n",
			"precondition": uploadBaseNamed(t, handler, target, "guide.md")})
	},
	"session upload": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingMultipartSend(t, "/api/v1/agents/"+goingPlanner+"/artifacts", map[string]string{"name": "notes.txt"}, "Written in process.")
	},
	"claim": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingJSONSend(t, http.MethodPost, "/api/v1/issues/"+f.unclaimed+"/claim", map[string]any{})
	},
	"claim of a held issue": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingJSONSend(t, http.MethodPost, "/api/v1/issues/"+f.claimed+"/claim", map[string]any{})
	},
	"release": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingSend{method: http.MethodDelete, path: "/api/v1/issues/" + f.claimed + "/claim"}
	},
	"reopen": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingSend{method: http.MethodPost, path: "/api/v1/comments/" + f.comment + "/reopen"}
	},
	"resolve": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingSend{method: http.MethodPost, path: "/api/v1/comments/" + f.comment + "/resolve"}
	},
}

// TestGoingConditionOnTheRequestsContextIsDecidedByItsRoute (deep review round 5, finding C): each
// route that takes a compare-and-set decides the one the command bar's in-process caller carries
// on the request's context, under the lock it takes for the write, as it decided the body member
// that carried it before: a condition the item no longer meets is refused, 409 with the route's
// own code, and nothing is written. These are the conditions TestAnOutsideRequestNamingAnOld
// ConditionMemberIsNotConditional sends in a body, which its routes take unconditioned.
func TestGoingConditionOnTheRequestsContextIsDecidedByItsRoute(t *testing.T) {
	rig := newGoingWritesRig(t)
	f := newGoingConditionFixtures(t, rig)
	never, bob := "2000-01-01T00:00:00Z", &model.Actor{Kind: "user", ID: "bob"}
	for _, tc := range []struct {
		write, code string
		condition   goingCondition
	}{
		{"issue update", "ISSUE_CHANGED", goingCondition{Expect: map[string]json.RawMessage{"title": json.RawMessage(`"Not the title"`)}}},
		{"ask edit", "ASK_CHANGED", goingCondition{Expect: map[string]json.RawMessage{"question": json.RawMessage(`"Not the question"`)}}},
		{"issue upload", "DOCUMENT_CHANGED", goingCondition{ExpectSHA256: strings.Repeat("0", 64)}},
		{"issue upload", "DOCUMENT_EXISTS", goingCondition{ExpectNew: true}},
		{"project upload", "DOCUMENT_EXISTS", goingCondition{ExpectNew: true}},
		{"session upload", "DOCUMENT_EXISTS", goingCondition{ExpectNew: true}},
		{"claim of a held issue", codeClaimHolderChanged, goingCondition{ExpectUnclaimed: true}},
		{"release", codeClaimHolderChanged, goingCondition{ExpectHolder: bob}},
		{"reopen", codeCommentChanged, goingCondition{ExpectResolvedAt: &never}},
		{"resolve", codeCommentChanged, goingCondition{ExpectOpen: true}},
	} {
		t.Run(tc.write+" "+tc.code, func(t *testing.T) {
			before := goingConditionState(t, rig, f)
			status, answer := goingConditioned(t, rig.mux, "alice", goingConditionWrites[tc.write](t, rig.mux, f), tc.condition)
			if status != http.StatusConflict || goingAnswerCode(t, answer) != tc.code {
				t.Fatalf("the %s carrying %+v answered %d %s, want 409 %s", tc.write, tc.condition, status, answer, tc.code)
			}
			if after := goingConditionState(t, rig, f); after != before {
				t.Fatalf("the refused %s left %s, want what stood before it: %s", tc.write, after, before)
			}
		})
	}
}

// TestGoingWriteBackWithoutItsConditionIsRefusedWithNothingWritten (deep review round 5, finding C,
// the architect's rule): a request the command bar's undo sends says it requires its condition
// (goingCondition.WriteBack), so a write back whose condition a change lost is never made
// unconditioned: each route that takes a condition refuses one marked as a write back that
// carries no condition the route decides - none at all, an empty one, one that leaves a field it
// writes unexpected, or an expected content for a file, which holds no document content its
// upload compares (review round 5, thread 7) - 500 GOING_CONDITION_MISSING, and writes nothing.
// The same write carrying no mark is made, as every caller but the undo makes it.
func TestGoingWriteBackWithoutItsConditionIsRefusedWithNothingWritten(t *testing.T) {
	rig := newGoingWritesRig(t)
	f := newGoingConditionFixtures(t, rig)
	empty := ""
	for _, tc := range []struct {
		name, write string
		condition   goingCondition
	}{
		{"an issue update with no condition", "issue update", goingCondition{WriteBack: true}},
		{"an issue update expecting another field than it writes", "issue update",
			goingCondition{WriteBack: true, Expect: map[string]json.RawMessage{"status": json.RawMessage(`"triage"`)}}},
		{"an ask edit with no condition", "ask edit", goingCondition{WriteBack: true}},
		{"an ask edit with an empty expectation", "ask edit", goingCondition{WriteBack: true, Expect: map[string]json.RawMessage{}}},
		{"an issue upload with no condition", "issue upload", goingCondition{WriteBack: true}},
		{"an issue upload with a blank expected content", "issue upload", goingCondition{WriteBack: true, ExpectSHA256: "  "}},
		{"a project upload with no condition", "project upload", goingCondition{WriteBack: true}},
		{"a session upload with no condition", "session upload", goingCondition{WriteBack: true}},
		{"a session upload of a file expecting a content", "session upload", goingCondition{WriteBack: true, ExpectSHA256: strings.Repeat("0", 64)}},
		{"a claim with no condition", "claim", goingCondition{WriteBack: true}},
		{"a release with no condition", "release", goingCondition{WriteBack: true}},
		{"a release expecting a holder of no kind and no id", "release", goingCondition{WriteBack: true, ExpectHolder: &model.Actor{}}},
		{"a reopen with no condition", "reopen", goingCondition{WriteBack: true}},
		{"a reopen expecting an empty moment", "reopen", goingCondition{WriteBack: true, ExpectResolvedAt: &empty}},
		{"a resolve with no condition", "resolve", goingCondition{WriteBack: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := goingConditionState(t, rig, f)
			status, answer := goingConditioned(t, rig.mux, "alice", goingConditionWrites[tc.write](t, rig.mux, f), tc.condition)
			if status != http.StatusInternalServerError || goingAnswerCode(t, answer) != codeGoingConditionMissing {
				t.Fatalf("the %s marked as a write back carrying %+v answered %d %s, want 500 %s", tc.write, tc.condition, status, answer, codeGoingConditionMissing)
			}
			if after := goingConditionState(t, rig, f); after != before {
				t.Fatalf("the refused write back left %s, want what stood before it: %s", after, before)
			}
		})
	}
	// Unmarked, each of the same writes is made: the refusal is the mark's.
	for _, write := range []string{"issue update", "ask edit", "issue upload", "project upload", "session upload", "claim", "release", "reopen", "resolve"} {
		t.Run("unmarked "+write, func(t *testing.T) {
			before := goingConditionState(t, rig, f)
			status, answer := goingConditioned(t, rig.mux, "alice", goingConditionWrites[write](t, rig.mux, f), goingCondition{})
			if status >= http.StatusMultipleChoices {
				t.Fatalf("the unmarked %s answered %d %s, want it made", write, status, answer)
			}
			if after := goingConditionState(t, rig, f); after == before {
				t.Fatalf("the unmarked %s left everything as it was (%s), want its write", write, after)
			}
		})
	}
}

// goingMultipartUpload posts a multipart upload of fields and one text file to target as alice,
// off the network, and answers its status and body.
func goingMultipartUpload(t *testing.T, handler http.Handler, target string, fields map[string]string, content string) (int, string) {
	t.Helper()
	form := goingUpload{fields: fields, filename: fields["name"], contentType: "text/plain; charset=utf-8", content: []byte(content)}
	body, contentType, err := form.encode()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code, response.Body.String()
}

// TestAnOutsideRequestNamingAnOldConditionMemberIsNotConditional (deep review round 5, finding C):
// the compare-and-sets the command bar's undo and uploads make ride on the request's context, so a
// body off the network that names one under its old member - expect, expect_sha256, expect_new
// (JSON and multipart), expect_unclaimed, expect_holder, expect_resolved_at - is answered as one
// that names nothing, the member ignored as main's decodeJSON ignores any member a route does not
// declare: each condition below is one its route would refuse, and each write goes through. Each
// JSON upload replaces a document, so it names the token of the version it replaces, as main's
// upload precondition asks of every caller, and the old member is all that could refuse it.
func TestAnOutsideRequestNamingAnOldConditionMemberIsNotConditional(t *testing.T) {
	rig := newGoingWritesRig(t)
	f := newGoingConditionFixtures(t, rig)
	owner := "/api/v1/issues/" + f.issue
	never := strings.Repeat("0", 64)
	for _, tc := range []struct {
		name, method, target string
		body                 map[string]any
		// went reports whether the write went through as the answer says, unconditioned.
		went func(t *testing.T, answer string) bool
	}{
		{"the issue update's expect", http.MethodPatch, owner,
			map[string]any{"title": "Retitled from outside", "expect": map[string]any{"title": "Not the title"}},
			func(t *testing.T, answer string) bool {
				return strings.Contains(answer, `"title":"Retitled from outside"`)
			}},
		{"the ask edit's expect", http.MethodPatch, "/api/v1/asks/" + f.ask,
			map[string]any{"question": "Ship it on Monday?", "expect": map[string]any{"question": "Not the question"}},
			func(t *testing.T, answer string) bool {
				return strings.Contains(answer, `"question":"Ship it on Monday?"`)
			}},
		{"an issue upload's expect_sha256", http.MethodPost, owner + "/artifacts",
			map[string]any{"name": "notes.md", "content": "# Notes\n\nThe second line.\n", "expect_sha256": never},
			func(t *testing.T, answer string) bool { return strings.Contains(answer, `"previous_version":1`) }},
		{"an issue upload's expect_new", http.MethodPost, owner + "/artifacts",
			map[string]any{"name": "notes.md", "content": "# Notes\n\nThe third line.\n", "expect_new": true},
			func(t *testing.T, answer string) bool { return strings.Contains(answer, `"previous_version":2`) }},
		{"a project upload's expect_new", http.MethodPost, "/api/v1/projects/DEMO/artifacts",
			map[string]any{"name": "guide.md", "content": "# Guide\n\nRead this second.\n", "expect_new": true},
			func(t *testing.T, answer string) bool { return strings.Contains(answer, `"previous_version":1`) }},
		{"a claim's expect_unclaimed", http.MethodPost, "/api/v1/issues/" + f.claimed + "/claim",
			map[string]any{"expect_unclaimed": true},
			func(t *testing.T, answer string) bool { return strings.Contains(answer, `"claim_changed":false`) }},
		{"a release's expect_holder", http.MethodDelete, "/api/v1/issues/" + f.claimed + "/claim",
			map[string]any{"expect_holder": map[string]any{"kind": "user", "id": "bob"}},
			func(t *testing.T, answer string) bool { return strings.Contains(answer, `"claim_changed":true`) }},
		{"a reopen's expect_resolved_at", http.MethodPost, "/api/v1/comments/" + f.comment + "/reopen",
			map[string]any{"expect_resolved_at": "2000-01-01T00:00:00Z"},
			func(t *testing.T, answer string) bool { return strings.Contains(answer, `"resolved":false`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasSuffix(tc.target, "/artifacts") {
				tc.body["precondition"] = uploadBaseNamed(t, rig.mux, tc.target, tc.body["name"].(string))
			}
			response := dispatchRequest(t, rig.mux, tc.method, tc.target, tc.body, "alice")
			if response.Code >= http.StatusMultipleChoices || !tc.went(t, response.Body.String()) {
				t.Fatalf("%s %s naming %v answered %d %s, want the write made with the member ignored",
					tc.method, tc.target, tc.body, response.Code, response.Body.String())
			}
		})
	}
	t.Run("a multipart upload's expect_new field", func(t *testing.T) {
		status, answer := goingMultipartUpload(t, rig.mux, "/api/v1/agents/"+goingPlanner+"/artifacts",
			map[string]string{"name": "notes.txt", "expect_new": "true"}, "More plain words.")
		if status != http.StatusCreated || !strings.Contains(answer, `"previous_version":1`) {
			t.Fatalf("the multipart upload naming expect_new answered %d %s, want the file's next version with the field ignored", status, answer)
		}
	})
}
