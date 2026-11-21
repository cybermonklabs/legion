package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The Dispatch-Test-Model header (test_model.go): under the test hooks a draft review, a person's
// Define and a person's Rewrite of an ask call the test model the header names, the detector's
// reader included, in place of their model group; a name DISPATCH_TEST_MODELS does not list is
// refused; and a server without the test hooks refuses the header before any model is called.

// testModelName is the one test model of these tests, called over Anthropic Messages while the
// groups speak OpenAI chat, so a request tells the two apart by its protocol and its model.
const testModelName = "candidate-x"

// testModelRequest is dispatchRequest with the Dispatch-Test-Model header naming model.
func testModelRequest(t *testing.T, handler http.Handler, method, target string, body any, login, model string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", login)
	request.Header.Set(TestModelHeader, model)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// testModelRig is a server with the reviewer, Rewrite and Define on, each group at the stand-in,
// the test model at the same stand-in, and an ask the writer session opened on an issue alice
// holds.
type testModelRig struct {
	handler  http.Handler
	database *store.Store
	stand    *modeltest.Model
	ask      string
}

func newTestModelRig(t *testing.T, hooks bool) *testModelRig {
	t.Helper()
	stand := modeltest.New(t,
		modeltest.Rule{Contains: "For each candidate sentence", Reply: modeltest.Reply{Text: `{"vacuous":[]}`}},
		// The group's reviewer speaks openai-chat, whose schema is strict; the test model
		// speaks Anthropic's protocol, whose schema is not.
		modeltest.Rule{Contains: reviewMarker, Match: func(r modeltest.Request) bool { return r.Protocol == modeltest.Chat }, Reply: modeltest.Reply{Text: noFindings}},
		modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: `{"findings":[]}`}},
		modeltest.Rule{Contains: rewriteMarker, Reply: modeltest.Reply{Text: keptRewrite(t)}},
		modeltest.Rule{Contains: defineMarker, Reply: modeltest.Reply{Text: `{"definitions":[{"words":"store","meaning":"The table where a held text waits.","links":[]}]}`}},
	)
	candidate := reviewer.Model{Model: agentloop.Model{Endpoint: agentloop.Endpoint{
		Kind: agentloop.AnthropicMessages, BaseURL: stand.URL,
		Token: func(context.Context) (string, error) { return "stand-in", nil },
	}, ID: testModelName, Effort: "off"}}
	listenerURL, _ := newRewriteListener(t)
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{
		testHooks:  hooks,
		testModels: map[string]reviewer.Model{testModelName: candidate},
		reviewer: testReviewer(ReviewerConfig{
			Model: reviewerModel(stand.URL), Rules: []reviewer.RuleSet{reviewer.RuleNoslop, reviewer.RuleDetector},
		}),
		rewriteDefine: NewRewriteDefine(RewriteDefineConfig{Model: reviewerModel(stand.URL), Rules: reviewer.DefaultRules, SideQuestionWait: time.Second}),
		envoyURL:      listenerURL, envoyTimeout: 2 * time.Second,
	})
	issue := createInteractionIssue(t, handler, "TMH", "Test model issue", "The store holds the held texts.\n").Key
	response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
		"question": rewriteQuestion,
		"options":  []map[string]string{{"label": "Ship it", "description": "Release it today."}, {"label": "Do not file", "description": "Wait."}},
		"actor":    map[string]any{"kind": "session", "id": heldWriter},
	}, "agent-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("the writer's ask: %d %s", response.Code, response.Body.String())
	}
	return &testModelRig{handler: handler, database: database, stand: stand, ask: decodeBody[struct {
		ID string `json:"id"`
	}](t, response).ID}
}

// modelsSince are the protocol and the model of each request the stand-in got after the first
// from of them.
func (rig *testModelRig) modelsSince(from int) []string {
	var models []string
	for _, request := range rig.stand.Requests()[from:] {
		models = append(models, request.Protocol+" "+request.Model)
	}
	return models
}

// Under the test hooks the header's model answers the draft route, the detector's call included,
// Define and a person's Rewrite, whose calls the ask's run keeps; with no header the group's model
// answers; and a name the list does not hold is refused before any call.
func TestTheTestModelHeaderNamesTheModelUnderTheTestHooks(t *testing.T) {
	rig := newTestModelRig(t, true)
	draft := map[string]any{"kind": "comment", "text": "Status\n" + vacuousPhrase}
	group := wantModels(t, rig, 0, func() *httptest.ResponseRecorder {
		return dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/reviewer-drafts", draft, "alice")
	}, http.StatusOK, "chat reviewer", "chat reviewer")
	chosen := wantModels(t, rig, group.seen, func() *httptest.ResponseRecorder {
		return testModelRequest(t, rig.handler, http.MethodPost, "/api/v1/reviewer-drafts", draft, "alice", testModelName)
	}, http.StatusOK, "anthropic "+testModelName, "anthropic "+testModelName)
	answer := decodeBody[reviewerDraftAnswer](t, chosen.response)
	if len(answer.Calls) != 2 || answer.Calls[0].Deployment != testModelName || answer.Calls[1].Deployment != testModelName ||
		answer.Calls[1].InputTokens == 0 || answer.Calls[1].OutputTokens == 0 {
		t.Errorf("the draft's calls = %+v, want the detector's and the review's, each at the test model with its tokens", answer.Calls)
	}
	refused := wantModels(t, rig, chosen.seen, func() *httptest.ResponseRecorder {
		return testModelRequest(t, rig.handler, http.MethodPost, "/api/v1/reviewer-drafts", draft, "alice", "not-listed")
	}, http.StatusBadRequest)
	wantCode(t, refused.response, "TEST_MODEL_REFUSED")

	defined := wantModels(t, rig, refused.seen, func() *httptest.ResponseRecorder {
		return testModelRequest(t, rig.handler, http.MethodPost, "/api/v1/definitions", map[string]any{"ask_id": rig.ask, "words": []string{"store"}}, "alice", testModelName)
	}, http.StatusCreated, "anthropic "+testModelName)
	if read := decodeBody[definitionRead](t, defined.response); len(read.Calls) != 1 || read.Calls[0].Deployment != testModelName {
		t.Errorf("the definition's calls = %+v, want the test model's", read.Calls)
	}

	if response := testModelRequest(t, rig.handler, http.MethodPost, "/api/v1/asks/"+rig.ask+"/rewrites", nil, "alice", testModelName); response.Code != http.StatusAccepted {
		t.Fatalf("alice's Rewrite naming the test model: %d %s", response.Code, response.Body.String())
	}
	var calls []reviewerCall
	deadline := time.Now().Add(20 * time.Second)
	for {
		var state string
		var encoded []byte
		if err := rig.database.Pool.QueryRow(context.Background(), `select state, calls from ask_rewrites where ask_id = $1::uuid`, rig.ask).Scan(&state, &encoded); err != nil {
			t.Fatal(err)
		}
		if state != "rewriting" {
			if state != "rewritten" || json.Unmarshal(encoded, &calls) != nil {
				t.Fatalf("the rewrite is %s with calls %s, want rewritten", state, encoded)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rewrite did not end")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := rig.modelsSince(defined.seen); len(got) != 1 || got[0] != "anthropic "+testModelName {
		t.Errorf("the rewrite's requests = %v, want one to the test model", got)
	}
	if len(calls) != 1 || calls[0].Purpose != reviewer.PurposeRewrite || calls[0].Deployment != testModelName || calls[0].InputTokens == 0 {
		t.Errorf("the ask's rewrite keeps calls %+v, want the rewrite's at the test model, with its tokens", calls)
	}
}

// Without the test hooks the header is refused on each of the three routes, before any call: a
// deployment's own model never answers a request that names another.
func TestTheTestModelHeaderIsRefusedWithoutTheTestHooks(t *testing.T) {
	rig := newTestModelRig(t, false)
	for _, route := range []struct {
		target string
		body   any
	}{
		{"/api/v1/reviewer-drafts", map[string]any{"kind": "comment", "text": "Status\n" + vacuousPhrase}},
		{"/api/v1/definitions", map[string]any{"ask_id": rig.ask, "words": []string{"store"}}},
		{"/api/v1/asks/" + rig.ask + "/rewrites", nil},
	} {
		refused := wantModels(t, rig, 0, func() *httptest.ResponseRecorder {
			return testModelRequest(t, rig.handler, http.MethodPost, route.target, route.body, "alice", testModelName)
		}, http.StatusBadRequest)
		wantCode(t, refused.response, "TEST_MODEL_REFUSED")
	}
	// The negative control: the same draft with no header is answered by the group's model.
	wantModels(t, rig, 0, func() *httptest.ResponseRecorder {
		return dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/reviewer-drafts", map[string]any{"kind": "comment", "text": "Status\n" + vacuousPhrase}, "alice")
	}, http.StatusOK, "chat reviewer", "chat reviewer")
}

// seenRequest is one request a test made and how many requests the stand-in had seen after it.
type seenRequest struct {
	response *httptest.ResponseRecorder
	seen     int
}

// wantModels makes the request send and wants its status, and the stand-in's requests after the
// first from of them to name want, in order.
func wantModels(t *testing.T, rig *testModelRig, from int, send func() *httptest.ResponseRecorder, status int, want ...string) seenRequest {
	t.Helper()
	response := send()
	if response.Code != status {
		t.Fatalf("status %d %s, want %d", response.Code, response.Body.String(), status)
	}
	got := rig.modelsSince(from)
	if len(got) != len(want) {
		t.Fatalf("requests to the model = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("request %d went by %s, want %s", i, got[i], want[i])
		}
	}
	return seenRequest{response: response, seen: from + len(got)}
}

// wantCode wants response to carry the error code.
func wantCode(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Code != code {
		t.Errorf("answer %s, want the code %s", response.Body.String(), code)
	}
}
