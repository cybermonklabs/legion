package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The sessions of c-session-title: one that stamps a title where bob may not see it and decides
// where he may, one that decides only what bob may not see, and one that never stamps a title.
const (
	sessionTitleDecider      = "session-titled-decider-0123456789"
	sessionTitleDeciderTitle = "Legion planner · TITLE-1"
	sessionTitleHidden       = "session-hidden-decider-0123456789"
	sessionTitleHiddenTitle  = "Legion tester · TITLE-1"
	sessionTitleUntitled     = "session-untitled-decider-0123456789"
)

// sessionTitleRead is what c-session-title reads of a tree read: each node's kind and id and its
// title member as the answer spelled it, absent where the answer holds none, and the tuples.
type sessionTitleRead struct {
	Nodes []struct {
		Kind  string          `json:"kind"`
		ID    string          `json:"id"`
		Title json.RawMessage `json:"title"`
	} `json:"nodes"`
	Tuples []model.IssueTreeTuple `json:"tuples"`
	body   string
}

// sessionTitleTree reads the tree of key with query as login, and fails unless it answers 200.
func sessionTitleTree(t *testing.T, handler http.Handler, key, query, login string) sessionTitleRead {
	t.Helper()
	response := treeResponse(t, handler, key, query, login, false)
	if response.Code != http.StatusOK {
		t.Fatalf("%s reads the tree of %s%s: status=%d body=%s", login, key, query, response.Code, response.Body.String())
	}
	read := decodeBody[sessionTitleRead](t, response)
	read.body = response.Body.String()
	return read
}

// session is the title member of each node of read that is the session id, as the answer spelled
// it, one entry per node.
func (read sessionTitleRead) session(id string) []json.RawMessage {
	var titles []json.RawMessage
	for _, node := range read.Nodes {
		if node.Kind == graphstore.KindSession && node.ID == id {
			titles = append(titles, node.Title)
		}
	}
	return titles
}

// namesSession reports whether read answers the session id as a node or as an end of a tuple.
func (read sessionTitleRead) namesSession(id string) bool {
	if len(read.session(id)) > 0 {
		return true
	}
	session := model.IssueTreeEnd{Kind: graphstore.KindSession, ID: id}
	for _, tuple := range read.Tuples {
		if tuple.From == session || tuple.To == session {
			return true
		}
	}
	return false
}

// sessionTitleDecide inserts markdown at the end of the spec of key through the spec's edit route,
// written by the session actor the agent's bearer names.
func sessionTitleDecide(t *testing.T, handler http.Handler, key string, actor map[string]any, markdown string) {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts/spec/edits", map[string]any{
		"ops":     []map[string]any{{"op": "insert", "after": "end", "markdown": markdown}},
		"summary": "a decision",
		"actor":   actor,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("the session %v writes a decision into the spec of %s: status=%d body=%s", actor["id"], key, response.Code, response.Body.String())
	}
}

// sessionTitleDecidedBy is the decided_by the Decisions read of key answers login for the decision
// node, as the response holds it, and fails unless the read answers an entry for it.
func sessionTitleDecidedBy(t *testing.T, handler http.Handler, key, login, node string) map[string]any {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/decisions", nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("%s reads GET /api/v1/issues/%s/decisions: status=%d body=%s", login, key, response.Code, response.Body.String())
	}
	read := decodeBody[struct {
		Entries []struct {
			Node      decisionEntryNode `json:"node"`
			DecidedBy map[string]any    `json:"decided_by"`
		} `json:"entries"`
	}](t, response)
	for _, entry := range read.Entries {
		if entry.Node.Kind == refs.BlockNodeKind && entry.Node.ID == node {
			return entry.DecidedBy
		}
	}
	t.Fatalf("the Decisions read of %s answers %s no entry for the decision %s: %s", key, login, node, response.Body.String())
	return nil
}

// sessionTitleReserve reserves the node of kind and id for its author by SQL, the shape a node only
// its author may see takes, and fails unless bob may then not see it.
func sessionTitleReserve(t *testing.T, database *store.Store, kind, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `update graph_nodes set visibility = $3 where kind = $1 and id = $2`,
		kind, id, graphstore.VisibilityAuthor); err != nil {
		t.Fatalf("reserve %s:%s for its author: %v", kind, id, err)
	}
	target := refs.Target{Kind: kind, ID: id}
	seen, err := refs.Nodes(ctx, database.Pool, model.Actor{Kind: "user", ID: "bob"}, []refs.Target{target})
	if err != nil {
		t.Fatalf("load %s:%s as bob: %v", kind, id, err)
	}
	if _, held := seen[target]; held {
		t.Fatalf("bob still sees %s:%s once it is reserved for its author: this case proves nothing", kind, id)
	}
}

// TestASessionDecidersNodeCarriesItsTitle is c-session-title's first test: every session node the
// tree read answers carries its title (R8). On an issue born converted, a session stamps its title
// on its upload of notes.md, which is then reserved for its author, so bob may not see the
// document where the session stamped its title; the same session writes the decision d1 into the
// spec with no title. A second session stamps its title on the write of the decision h1, which is
// then reserved for its author, so it decides only what bob may not see. The tree read with no
// view and its ?view=decisions, as alice and as bob, answer the first session's node with title
// equal to the Decisions read's decided_by.title for d1, which is the title the collaborators read
// gives bob for that session. bob's answers carry neither the second session's node, nor a tuple
// naming it, nor its title.
func TestASessionDecidersNodeCarriesItsTitle(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "TITLE")
	issue := createDigestIssue(t, handler, database, "TITLE", "Titled deciders",
		requirementMarkdown("r1", "The board names each agent by its title."), false)

	agentUploadAs(t, handler, issue.Key, "notes.md", "The titled session's own notes.\n",
		map[string]any{"actor": claimActorBody(sessionTitleDecider, sessionTitleDeciderTitle)})
	sessionTitleReserve(t, database, graphstore.KindArtifact, readDecisionDocument(t, database, issue.Key, "notes-md").refKey)
	sessionTitleDecide(t, handler, issue.Key, map[string]any{"kind": "session", "id": sessionTitleDecider},
		":::decision{#d1}\nThe titled session decides.\n:::\n")
	sessionTitleDecide(t, handler, issue.Key, claimActorBody(sessionTitleHidden, sessionTitleHiddenTitle),
		":::decision{#h1}\nThe hidden session decides.\n:::\n")
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	decided, hidden := spec.address("d1"), spec.address("h1")
	if tie := requireDecidedByNode(t, database, "the titled session's decision", decided, graphstore.KindSession, sessionTitleDecider); tie.ToID == "" {
		t.FailNow()
	}
	if tie := requireDecidedByNode(t, database, "the hidden session's decision", hidden, graphstore.KindSession, sessionTitleHidden); tie.ToID == "" {
		t.FailNow()
	}
	sessionTitleReserve(t, database, refs.BlockNodeKind, hidden)

	decider := sessionTitleDecidedBy(t, handler, issue.Key, "bob", decided)
	want, titled := decider["title"].(string)
	if !titled || want != sessionTitleDeciderTitle {
		t.Fatalf("the Decisions read answers bob the decided_by %v for %s, want the title %q its session stamped: this case proves nothing",
			decider, decided, sessionTitleDeciderTitle)
	}
	collaborators := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/collaborators", nil, "bob")
	if collaborators.Code != http.StatusOK {
		t.Fatalf("bob reads the collaborators of %s: status=%d body=%s", issue.Key, collaborators.Code, collaborators.Body.String())
	}
	listed := false
	for _, agent := range decodeBody[collaboratorsAnswer](t, collaborators).Agents {
		if agent.SessionID != sessionTitleDecider {
			continue
		}
		listed = true
		if agent.Title == nil {
			t.Errorf("the collaborators read gives bob no title for the session %s, want %q, the decided_by.title of the Decisions read", sessionTitleDecider, want)
		} else if *agent.Title != want {
			t.Errorf("the collaborators read gives bob the title %q for the session %s, want %q, the decided_by.title of the Decisions read", *agent.Title, sessionTitleDecider, want)
		}
	}
	if !listed {
		t.Errorf("the collaborators read of %s lists bob no session %s: %s", issue.Key, sessionTitleDecider, collaborators.Body.String())
	}

	for _, login := range []string{"alice", "bob"} {
		reader := ""
		if login == "bob" {
			reader = "bob "
		}
		for _, query := range []string{"", "?view=decisions"} {
			read := sessionTitleTree(t, handler, issue.Key, query, login)
			titles := read.session(sessionTitleDecider)
			if len(titles) != 1 {
				t.Errorf("the tree read of %s%s answers %s %d session nodes %s, the decider of %s, want one", issue.Key, query, login, len(titles), sessionTitleDecider, decided)
			}
			for _, title := range titles {
				var held string
				switch {
				case len(title) == 0:
					t.Errorf("the session node %s answers %sno title, want %s", sessionTitleDecider, reader, want)
				case json.Unmarshal(title, &held) != nil || held != want:
					t.Errorf("the session node %s answers %sthe title %s in the tree read of %s%s, want %q", sessionTitleDecider, reader, title, issue.Key, query, want)
				}
			}
			if login == "bob" && (read.namesSession(sessionTitleHidden) || strings.Contains(read.body, sessionTitleHiddenTitle)) {
				t.Errorf("bob's answer names the session %s, which decided only what bob may not see: the tree read of %s%s", sessionTitleHidden, issue.Key, query)
			}
		}
	}
}

// TestASessionThatStampedNoTitleCarriesNoTitle is c-session-title's second test: a session whose
// writes stamped no title writes the decision d1 into the spec of an issue born converted, and
// the tree read with no view and its ?view=decisions answer that session's node with no title
// member in the JSON, as the Decisions read's decided_by holds none.
func TestASessionThatStampedNoTitleCarriesNoTitle(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "NOTITLE")
	issue := createDigestIssue(t, handler, database, "NOTITLE", "An untitled decider",
		requirementMarkdown("r1", "The board names each agent it can."), false)
	sessionTitleDecide(t, handler, issue.Key, map[string]any{"kind": "session", "id": sessionTitleUntitled},
		":::decision{#d1}\nThe untitled session decides.\n:::\n")
	decided := readDecisionDocument(t, database, issue.Key, "spec").address("d1")
	if tie := requireDecidedByNode(t, database, "the untitled session's decision", decided, graphstore.KindSession, sessionTitleUntitled); tie.ToID == "" {
		t.FailNow()
	}
	if decider := sessionTitleDecidedBy(t, handler, issue.Key, "alice", decided); decider["title"] != nil {
		t.Fatalf("the Decisions read answers the decided_by %v for %s, whose session stamped no title: this case proves nothing", decider, decided)
	}

	for _, query := range []string{"", "?view=decisions"} {
		read := sessionTitleTree(t, handler, issue.Key, query, "alice")
		titles := read.session(sessionTitleUntitled)
		if len(titles) != 1 {
			t.Errorf("the tree read of %s%s answers %d session nodes %s, the decider of %s, want one", issue.Key, query, len(titles), sessionTitleUntitled, decided)
		}
		for _, title := range titles {
			if len(title) != 0 {
				t.Errorf("the session node %s answers title %s, want none", sessionTitleUntitled, title)
			}
		}
	}
}
