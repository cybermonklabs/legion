package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// mentionFrame is what a comment mention's send carries for its listener (comment_delivery.go,
// deliverResolvedCommentMention): the comment's comment.created event, by the owner and project
// that event names, and the attempt it is.
type mentionFrame struct {
	Event struct {
		IssueKey   *string `json:"issue_key"`
		ArtifactID *string `json:"artifact_id"`
		Project    string  `json:"project"`
		Type       string  `json:"type"`
	} `json:"event"`
	Delivery struct {
		Attempt   int    `json:"attempt"`
		CommentID string `json:"comment_id"`
	} `json:"delivery"`
}

// A mention in a comment on a project document is sent to its session in a frame whose event names
// the document's project, on every road a send of it takes: the comment's create, which builds the
// frame from the event it appended, and the three that read that event back from the log
// (loadCommentCreatedEvent) - a keyed repeat of the create that finds the mention's one attempt
// left pending, a retry, and the delivery sweep. The event has no issue, so its project is all a
// reader of the frame has to tell whose document it is, and a listener that hands an outside
// session only the frames of the projects its token lists withholds a frame that names none. A
// mention in an issue's comment names the issue's project the same way.
//
// A process that died between its send and its settle leaves the attempt pending, so each road
// that resumes one is given its row as that process leaves it, written here directly.
func TestACommentsMentionFrameNamesItsOwnersProject(t *testing.T) {
	t.Parallel()
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`
	var mu sync.Mutex
	frames := []mentionFrame{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				Payload string `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode mention send: %v", err)
			}
			var frame mentionFrame
			if err := json.Unmarshal([]byte(request.Payload), &frame); err != nil {
				t.Errorf("decode mention delivery frame %s: %v", request.Payload, err)
			}
			mu.Lock()
			frames = append(frames, frame)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	// OWN is the mentioned session's own project, OTHER a project its token does not list.
	createTestProject(t, handler, "OWN", "OTHER")
	strand := func(t *testing.T, what, statement, commentID string) {
		t.Helper()
		if _, err := database.Pool.Exec(context.Background(), statement, commentID); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	for name, row := range map[string]struct {
		project string
		issue   bool
	}{
		"a mention on a document of the session's own project carries that project": {project: "OWN"},
		"a mention on a document of another project carries the other project":      {project: "OTHER"},
		"a mention on an issue carries the issue's project":                         {project: "TASK", issue: true},
	} {
		t.Run(name, func(t *testing.T) {
			var path, issue, document string
			if row.issue {
				issue = createTestIssue(t, handler, row.project, "Runbook")
				path = "/api/v1/issues/" + issue + "/comments"
			} else {
				document = createProjectDocument(t, handler, row.project, "Runbook", "# Runbook\n\nStep one.\n").ID
				path = "/api/v1/artifacts/" + document + "/comments"
			}
			body := map[string]any{
				"body": "@s1 is step one right?", "mentions": []map[string]any{{"target": "session:s1"}},
			}
			key := "mention-" + row.project
			comment := decodeMentionedComment(t, keyedRequest(t, handler, http.MethodPost, path, body, "alice", key))
			settledCommentDeliveries(t, database, comment.ID)

			// The create's process died after its send: a repeat of the create sends the attempt again.
			strand(t, "leave attempt 1 pending after its send", `
				update comment_deliveries set state = 'pending', envelope_id = null, claimed_at = now() - interval '2 minutes'
				where comment_id = $1 and attempt = 1
			`, comment.ID)
			if repeat := keyedRequest(t, handler, http.MethodPost, path, body, "alice", key); repeat.Code != http.StatusCreated || !replayed(repeat) {
				t.Fatalf("repeat the create: status=%d body=%s, want the made comment replayed", repeat.Code, repeat.Body.String())
			}
			settledCommentDeliveries(t, database, comment.ID)

			retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries",
				map[string]any{"delivery": "steer"}, "alice")
			if retry.Code != http.StatusCreated {
				t.Fatalf("retry the mention: status=%d body=%s", retry.Code, retry.Body.String())
			}

			// A retry's process died before its send: once its claim lapses, the sweep sends it.
			strand(t, "leave attempt 3 owed", `
				insert into comment_deliveries (comment_id, target, attempt, delivery, state, claimed_at, created_at)
				values ($1, 'session:s1', 3, 'steer', 'pending', now() - interval '2 minutes', now() - interval '2 minutes')
			`, comment.ID)
			if err := served.resumeOwedDeliveries(context.Background()); err != nil {
				t.Fatalf("sweep: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			roads := []string{"the create", "the keyed repeat", "the retry", "the delivery sweep"}
			attempts := []int{}
			for _, frame := range frames {
				if frame.Delivery.CommentID != comment.ID {
					continue
				}
				road := "an unexpected send"
				if len(attempts) < len(roads) {
					road = roads[len(attempts)]
				}
				attempts = append(attempts, frame.Delivery.Attempt)
				event := frame.Event
				if event.Type != "comment.created" || deref(event.IssueKey) != issue || deref(event.ArtifactID) != document {
					t.Errorf("%s's frame (attempt %d) carries a %s event of issue %q, document %q; want the comment.created of issue %q, document %q",
						road, frame.Delivery.Attempt, event.Type, deref(event.IssueKey), deref(event.ArtifactID), issue, document)
				}
				if event.Project != row.project {
					t.Errorf("%s's frame (attempt %d) names project %q, want its owner's project %q",
						road, frame.Delivery.Attempt, event.Project, row.project)
				}
			}
			if !slices.Equal(attempts, []int{1, 1, 2, 3}) {
				t.Fatalf("the listener was sent attempts %v of comment %s, want 1 by %s, 1 by %s, 2 by %s and 3 by %s",
					attempts, comment.ID, roads[0], roads[1], roads[2], roads[3])
			}
		})
	}
}
