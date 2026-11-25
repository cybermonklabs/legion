package api

// v2_every_create_stores_its_node_test.go holds what #5569 gave the /api/v1 creates to their
// /api/v2 successors: each /api/v2 route that answers for a row TestEveryCreateStoresItsNode makes
// an issue, a document, an ask or a message through leaves that object's node in its project, or
// between its parties, as the /api/v1 row does. Each runs the core its /api/v1 row runs
// (createIssueFrom, storeUpload, openAsk, writeComment, changeIssueStatusFrom, postIssueMessage,
// sendAgentMessage, sendBroadcast, answerCommentDelivery, answerMessageDelivery, requestApproval,
// applyEdits, commentActionAs, rebuildDocument), and this test holds the /api/v2 route to it.

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
)

// TestEveryV2CreateStoresItsNode holds every /api/v2 POST row, and each /api/v2 route that answers
// for a create of /api/v1, to leaving no row of a project-owned kind without its node in the
// project, or between the parties, the row belongs to, as TestEveryCreateStoresItsNode holds every
// /api/v1 POST row. It reads the successor of each /api/v1 POST row postCalls names from the table
// (apiRoute.Successor): the successor of a row postCalls calls as a create, or as a write of a
// document's text, must be a route v2CreateCalls holds a request for, and every other /api/v2 POST
// row must answer for a row postCalls calls with the empty body (noObject). Either miss fails the
// test by name. On a server seeded with one object of each kind (seedEveryKind), it calls each
// route v2CreateCalls holds with its request, held to a 2xx, and every other /api/v2 POST row with
// the empty body, whose answer it does not read; it settles every document the calls left owing a
// settlement, requires each new ask block's ask, and asks graphcensus.ProjectComplete, which names
// any row left without a node, with no project, or with another than its row's.
func TestEveryV2CreateStoresItsNode(t *testing.T) {
	rig := newStoreWriterRig(t)
	ctx := context.Background()
	fixture := seedEveryKind(t, rig)

	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	emptyBody := map[string]bool{}
	for _, route := range servedAt(apiV1, routes) {
		name := route.Method + " " + route.Pattern
		call, listed := postCalls[name]
		if route.Method != http.MethodPost || !listed {
			continue
		}
		answering := route.Successor.Method + " /api/v2" + route.Successor.Pattern
		if !call.succeeds {
			emptyBody[answering] = true
			continue
		}
		if _, held := v2CreateCalls[answering]; !held {
			t.Errorf("%s answers for %s, a create postCalls names, and v2CreateCalls holds no request for it: write down how to call it", answering, name)
		}
	}
	calls := maps.Clone(v2CreateCalls)
	for _, route := range servedAt(apiV2, routes) {
		name := route.Method + " " + route.Pattern
		if route.Method != http.MethodPost {
			continue
		}
		if _, held := calls[name]; held {
			continue
		}
		if !emptyBody[name] {
			t.Errorf("%s is a POST row of /api/v2 that answers for no /api/v1 row postCalls calls with the empty body, and v2CreateCalls holds no request for it: write down how to call it", name)
			continue
		}
		calls[name] = v2CreateCall{postCall: noObject}
	}
	names := slices.Sorted(maps.Keys(calls))

	var blocks []string
	for _, name := range names {
		call := calls[name]
		method, pattern, _ := strings.Cut(name, " ")
		if call.id != nil {
			pattern = strings.Replace(pattern, "{id}", call.id(fixture), 1)
		}
		response := call.sendAs(t, rig.handler, method, fixture.path(pattern))
		if call.succeeds && (response.Code < 200 || response.Code > 299) {
			t.Errorf("%s answered %d %s, want a 2xx", name, response.Code, response.Body.String())
			continue
		}
		if call.askBlock != "" {
			blocks = append(blocks, call.askBlock)
		}
	}
	asks := settledAskBlocks(t, rig, blocks)

	incomplete, err := graphcensus.ProjectComplete(ctx, rig.database.Pool)
	if err != nil {
		t.Fatalf("read the rows whose node names no project: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("after every /api/v2 create, these rows have no node in their project: %v", incomplete)
	}
	t.Logf("called %d /api/v2 routes; the settlements opened the asks %v", len(names), asks)
}

// v2CreateCall is how TestEveryV2CreateStoresItsNode calls one /api/v2 route: postCall's request,
// and, where fixture.path would give the path's {id} another object than the route takes, the
// object it takes.
type v2CreateCall struct {
	postCall
	id func(everyKindFixture) string
}

// sendAs sends the call as method to path: a POST as postCall.send sends it, and any other
// method's JSON body from alice, which is how each /api/v2 PATCH and PUT v2CreateCalls holds is
// sent.
func (c v2CreateCall) sendAs(t *testing.T, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	if method == http.MethodPost {
		return c.send(t, handler, path)
	}
	body := c.body
	if body == nil {
		body = map[string]any{}
	}
	return dispatchRequest(t, handler, method, path, body, "alice")
}

// v2CreateCalls is the request of each /api/v2 route that answers for an /api/v1 row postCalls
// calls as a create or a write of a document's text (its Successor in the table): the request
// postCalls sends that row, which each /api/v2 body takes as it stands, but for the accept of a
// suggestion, which /api/v2 writes as the comment's state. The three /api/v1 edits and the three
// named versions answer to one /api/v2 route each, which takes the document by its id. The accept
// takes the fixture's suggestion and the rebuild its unloadable document, as their /api/v1 rows
// do (everyKindFixture.path).
var v2CreateCalls = map[string]v2CreateCall{
	"POST /api/v2/issues":                           {postCall: postCalls["POST /api/v1/issues"]},
	"POST /api/v2/issues/{key}/artifacts":           {postCall: postCalls["POST /api/v1/issues/{key}/artifacts"]},
	"POST /api/v2/projects/{key}/artifacts":         {postCall: postCalls["POST /api/v1/projects/{key}/artifacts"]},
	"POST /api/v2/issues/{key}/asks":                {postCall: postCalls["POST /api/v1/issues/{key}/asks"]},
	"POST /api/v2/artifacts/{id}/asks":              {postCall: postCalls["POST /api/v1/artifacts/{id}/asks"]},
	"POST /api/v2/issues/{key}/comments":            {postCall: postCalls["POST /api/v1/issues/{key}/comments"]},
	"PATCH /api/v2/issues/{key}":                    {postCall: postCalls["POST /api/v1/issues/{key}/status-change"]},
	"POST /api/v2/artifacts/{id}/comments":          {postCall: postCalls["POST /api/v1/artifacts/{id}/comments"]},
	"POST /api/v2/issues/{key}/messages":            {postCall: postCalls["POST /api/v1/issues/{key}/messages"]},
	"POST /api/v2/agents/{session_id}/messages":     {postCall: postCalls["POST /api/v1/agents/{session_id}/messages"]},
	"POST /api/v2/broadcasts":                       {postCall: postCalls["POST /api/v1/broadcasts"]},
	"POST /api/v2/comments/{id}/replies":            {postCall: postCalls["POST /api/v1/comments/{id}/reply"]},
	"POST /api/v2/messages/{id}/replies":            {postCall: postCalls["POST /api/v1/messages/{id}/reply"]},
	"POST /api/v2/artifacts/{id}/approval-requests": {postCall: postCalls["POST /api/v1/artifacts/{id}/approval-requests"]},
	"POST /api/v2/agents/{session_id}/artifacts":    {postCall: postCalls["POST /api/v1/agents/{session_id}/artifacts"]},
	"POST /api/v2/artifacts/{id}/edits":             {postCall: postCalls["POST /api/v1/artifacts/{id}/edits"]},
	"POST /api/v2/artifacts/{id}/versions":          {postCall: postCalls["POST /api/v1/artifacts/{id}/versions"]},
	"POST /api/v2/artifacts/{id}/rebuilds": {
		postCall: postCalls["POST /api/v1/artifacts/{id}/rebuild"],
		id:       func(f everyKindFixture) string { return f.unloadable },
	},
	"PUT /api/v2/comments/{id}/state": {
		postCall: postCall{succeeds: true, askBlock: acceptedAskBlock, body: map[string]any{"state": "accepted"}},
		id:       func(f everyKindFixture) string { return f.suggestion },
	},
}
