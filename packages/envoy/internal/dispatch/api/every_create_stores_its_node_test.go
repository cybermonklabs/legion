package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestEveryCreateStoresItsNode holds every write route to leaving no row of a project-owned kind
// without its node in the project, or between the parties, the row belongs to
// (the node a read limited to a project depends on). It calls every POST row of writeClasses -
// read from the table, so the set is the server's own - on a server seeded with one object of
// each kind, then asks graphcensus.ProjectComplete, the census's completeness read, which names
// any row left without a node, with no project, or with another than its row's.
//
// A POST row needs a request the route takes. postCalls holds one for every row: a create gets
// the body that makes its object, which the test holds to a 2xx, and any other row the empty body,
// whose answer the test does not read, since it creates nothing of these kinds. A POST row of the
// table postCalls does not name fails the test by name: a create route added later is caught there
// and called once its request is written down.
//
// The control is the ask create's node write skipped (api/asks.go): the test then fails, naming the
// ask the create left without a node.
func TestEveryCreateStoresItsNode(t *testing.T) {
	rig := newStoreWriterRig(t)
	ctx := context.Background()
	fixture := seedEveryKind(t, rig)

	var posts []string
	for name := range writeClasses {
		if strings.HasPrefix(name, http.MethodPost+" ") {
			posts = append(posts, name)
		}
	}
	slices.Sort(posts)
	if len(posts) == 0 {
		t.Fatal("writeClasses holds no POST row, so the test calls nothing")
	}
	for _, name := range posts {
		call, listed := postCalls[name]
		if !listed {
			t.Errorf("%s is a POST row of writeClasses that postCalls holds no request for: write down how to call it", name)
			continue
		}
		pattern := strings.TrimPrefix(name, http.MethodPost+" ")
		response := call.send(t, rig.handler, fixture.path(pattern))
		if call.creates && (response.Code < 200 || response.Code > 299) {
			t.Errorf("%s answered %d %s, want the 2xx of a create", name, response.Code, response.Body.String())
		}
	}

	incomplete, err := graphcensus.ProjectComplete(ctx, rig.database.Pool)
	if err != nil {
		t.Fatalf("read the rows whose node names no project: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("after every POST route, these rows have no node in their project: %v", incomplete)
	}
	t.Logf("called %d POST routes of writeClasses", len(posts))
}

// everyKindFixture is the object of each kind the routes address: the rig's issue and project
// document, an ask on the issue, a comment a session's mention answered, and a direct message a
// session was sent.
type everyKindFixture struct {
	issue, project, document, documentSlug string
	ask, comment, message                  string
}

// seedEveryKind writes the objects postCalls' paths name, through the API.
func seedEveryKind(t *testing.T, rig *storeWriterRig) everyKindFixture {
	t.Helper()
	fixture := everyKindFixture{issue: rig.issue, project: "LOCK", document: rig.document}
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select slug from artifacts where id = $1::uuid`, rig.document).Scan(&fixture.documentSlug); err != nil {
		t.Fatalf("read the slug of the project document: %v", err)
	}
	fixture.ask = createIssueAsk(t, rig.handler, rig.issue, "Which region goes first?")
	mentioned := decodeMentionedComment(t, postMentionedComment(t, rig.handler, rig.issue, map[string]any{
		"body": "Does this ship, @s1?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, rig.database, mentioned.ID)
	fixture.comment = mentioned.ID
	direct := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Status, please.", "delivery": "btw",
	}, "alice")
	if direct.Code != http.StatusCreated {
		t.Fatalf("send the direct message: status=%d body=%s", direct.Code, direct.Body.String())
	}
	fixture.message = decodeBody[model.Message](t, direct).ID
	return fixture
}

// pathParameter matches one parameter of a route pattern.
var pathParameter = regexp.MustCompile(`\{([a-z_]+)\}`)

// path fills a route pattern's parameters from the fixture: a key is the project's under
// /projects/ and the issue's elsewhere, an id is the object of the collection it follows, and a
// parameter no object answers takes a value that addresses nothing.
func (f everyKindFixture) path(pattern string) string {
	return pathParameter.ReplaceAllStringFunc(pattern, func(match string) string {
		at := strings.Index(pattern, match)
		before := pattern[:at]
		switch match {
		case "{key}":
			if strings.HasSuffix(before, "/projects/") {
				return f.project
			}
			return f.issue
		case "{slug}":
			if strings.HasPrefix(pattern, "/api/v1/projects/") {
				return f.documentSlug
			}
			return "spec"
		case "{session_id}":
			return "s1"
		case "{attempt}":
			return "1"
		case "{phase}":
			return "spec"
		case "{block_id}":
			return "b1"
		case "{owner}", "{repo}":
			return "nobody"
		case "{id}":
			switch {
			case strings.HasSuffix(before, "/artifacts/"):
				return f.document
			case strings.HasSuffix(before, "/asks/"):
				return f.ask
			case strings.HasSuffix(before, "/comments/"):
				return f.comment
			case strings.HasSuffix(before, "/messages/"):
				return f.message
			}
		}
		return "00000000-0000-4000-8000-000000000000"
	})
}

// postCall is how the test calls one POST row: its body, who sends it, whether it is a file upload
// rather than a JSON body, and whether it creates an object of a project-owned kind, which the
// test then holds to a 2xx.
type postCall struct {
	body    any
	session bool
	upload  bool
	creates bool
}

func (c postCall) send(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	if c.upload {
		return multipartRequest(t, handler, path, map[string]string{"name": "shot.png"}, "shot.png", "image/png", pngBytes("every create"), "alice")
	}
	body := c.body
	if body == nil {
		body = map[string]any{}
	}
	if c.session {
		return bearerRequest(t, handler, http.MethodPost, path, body)
	}
	return dispatchRequest(t, handler, http.MethodPost, path, body, "alice")
}

// noObject is a POST row that creates no row of a project-owned kind: it is called with the empty
// body, and its answer is not read.
var noObject = postCall{}

// postCalls is how every POST row of writeClasses is called. The creates make one object each of
// the kinds whose node names its project; every other row creates none.
var postCalls = map[string]postCall{
	"POST /api/v1/issues":                       {creates: true, body: map[string]any{"project": "LOCK", "title": "Every create stores its node", "spec": "A spec.\n"}},
	"POST /api/v1/issues/{key}/artifacts":       {creates: true, body: map[string]any{"name": "notes.md", "content": "# Notes\n"}},
	"POST /api/v1/projects/{key}/artifacts":     {creates: true, body: map[string]any{"name": "Playbook.md", "content": "# Playbook\n"}},
	"POST /api/v1/issues/{key}/asks":            {creates: true, body: map[string]any{"question": "Does the issue's ask name its project?"}},
	"POST /api/v1/artifacts/{id}/asks":          {creates: true, body: map[string]any{"question": "Does the document's ask name its project?"}},
	"POST /api/v1/issues/{key}/comments":        {creates: true, body: map[string]any{"body": "A remark on the issue."}},
	"POST /api/v1/artifacts/{id}/comments":      {creates: true, body: map[string]any{"body": "A remark on the document."}},
	"POST /api/v1/issues/{key}/messages":        {creates: true, body: map[string]any{"body": "A message on the issue."}},
	"POST /api/v1/agents/{session_id}/messages": {creates: true, body: map[string]any{"body": "Another direct message.", "delivery": "btw"}},
	"POST /api/v1/broadcasts":                   {creates: true, body: map[string]any{"body": "To everyone.", "delivery": "btw", "session_ids": []string{"s1"}}},
	"POST /api/v1/comments/{id}/reply": {creates: true, session: true, body: map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Yes, it ships."}},
	"POST /api/v1/messages/{id}/reply": {creates: true, session: true, body: map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "All green."}},
	"POST /api/v1/artifacts/{id}/approval-requests": {creates: true, session: true, body: map[string]any{"actor": map[string]any{"kind": "session", "id": "s1"}}},

	"POST /api/v1/issues/{key}/claim":                     noObject,
	"POST /api/v1/issues/{key}/artifacts/{slug}/versions": noObject,
	"POST /api/v1/issues/{key}/artifacts/{slug}/edits":    noObject,
	"POST /api/v1/issues/{key}/results":                   noObject,
	"POST /api/v1/issues/{key}/handoffs/{phase}":          noObject,
	"POST /api/v1/results/{id}/retract":                   noObject,
	// The reasoning nodes of dispatch://INFRA-1950, a node's state and the pre-mortem conversion:
	// each writes nodes and tuples of the store alone and no row of a project-owned table, and the
	// fixture's paths name no node of the issue under {id} and no pre-mortem under {slug}, so each
	// is called with the empty body and refused.
	"POST /api/v1/issues/{key}/lenses":                                     noObject,
	"POST /api/v1/issues/{key}/risks":                                      noObject,
	"POST /api/v1/issues/{key}/constraints":                                noObject,
	"POST /api/v1/issues/{key}/nodes/{id}/findings":                        noObject,
	"POST /api/v1/issues/{key}/nodes/{id}/pointers":                        noObject,
	"POST /api/v1/issues/{key}/nodes/{id}/state":                           noObject,
	"POST /api/v1/issues/{key}/nodes/{id}/settle":                          noObject,
	"POST /api/v1/issues/{key}/artifacts/{slug}/premortem-convert":         noObject,
	"POST /api/v1/projects/{key}/artifacts/{slug}/versions":                noObject,
	"POST /api/v1/projects/{key}/artifacts/{slug}/edits":                   noObject,
	"POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync": noObject,
	"POST /api/v1/projects/{key}/architecture-sources/sync":                noObject,
	"POST /api/v1/projects/{key}/architecture-source/sync":                 noObject,
	"POST /api/v1/projects/{key}/related":                                  noObject,
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep":                   noObject,
	"POST /api/v1/artifacts/{id}/rebuild":                                  noObject,
	"POST /api/v1/artifacts/{id}/reviews":                                  noObject,
	"POST /api/v1/artifacts/{id}/versions/{number}/reviews":                noObject,
	"POST /api/v1/artifacts/{id}/plan-reviews":                             noObject,
	"POST /api/v1/artifacts/{id}/versions":                                 noObject,
	"POST /api/v1/artifacts/{id}/edits":                                    noObject,
	"POST /api/v1/artifacts/{id}/_test/outside-schema":                     noObject,
	"POST /api/v1/artifacts/{id}/_test/newer-content":                      noObject,
	"POST /api/v1/artifacts/{id}/_test/hold-persistence":                   noObject,
	"POST /api/v1/asks/{id}/answer":                                        noObject,
	"POST /api/v1/asks/{id}/resolve":                                       noObject,
	"POST /api/v1/asks/{id}/decline":                                       noObject,
	"POST /api/v1/comments/{id}/resolve":                                   noObject,
	"POST /api/v1/comments/{id}/reopen":                                    noObject,
	"POST /api/v1/comments/{id}/deliveries":                                noObject,
	"POST /api/v1/comments/{id}/accept":                                    noObject,
	"POST /api/v1/comments/{id}/reject":                                    noObject,
	"POST /api/v1/messages/{id}/deliveries":                                noObject,
	"POST /api/v1/going/requests":                                          noObject,
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept":               noObject,
	"POST /api/v1/agents/{session_id}/artifacts":                           {creates: true, upload: true},
	"POST /api/v1/agents/{session_id}/stream/_test/publish":                noObject,
	"POST /api/v1/me/agent-tokens":                                         noObject,
	"POST /api/v1/me/inbox/read":                                           noObject,
	"POST /api/v1/me/inbox/dismiss":                                        noObject,
	"POST /api/v1/projects":                                                noObject,
	"POST /api/v1/priorities":                                              noObject,
	"POST /api/v1/delivery/session-groups":                                 noObject,
	"POST /api/v1/credential-requests/{id}/approve":                        noObject,
	"POST /api/v1/credential-requests/{id}/deny":                           noObject,
	"POST /api/v1/credential-requests/machine-lookup":                      noObject,
	"POST /api/v1/credential-grants/{id}/revoke":                           noObject,
	"POST /api/v1/machine-logins/{id}/revoke":                              noObject,
	"POST /api/v1/signins/{id}/revoke":                                     noObject,
	"POST /api/v1/live/_test/disconnect":                                   noObject,
	"POST /api/v1/live/_test/rotate":                                       noObject,
	"POST /api/v1/artifacts/_test/quiesce":                                 noObject,
	"POST /api/v1/settings/new-issue/_test":                                noObject,
	"POST /api/v1/writes/_test/slow":                                       noObject,
	"POST /api/v1/markdown/round-trip":                                     noObject,
}
