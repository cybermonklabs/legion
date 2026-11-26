package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// largeWriteRig is what a large write is made through: the server's routes, the issue the writes
// land on and its spec, the server's own store, and an editor's document service on that store.
type largeWriteRig struct {
	handler  http.Handler
	database *store.Store
	issueKey string
	// specID is the issue's spec, which the editor types into.
	specID string
	// editor serves the document socket as a deployment's server does, knowing who its caller is
	// (docs.Deps.Identity), which the test server's document service does not.
	editor *docs.Service
}

// largeWrite is one write whose statements can pass more than store.LargeArgumentBytesForTest of
// arguments, made through the route its client calls.
type largeWrite struct {
	name  string
	write func(t *testing.T, rig largeWriteRig)
}

// largeWrites is each write of the store that can pass a statement more than 64 KiB of
// arguments: a document's version with its stored history and its blocks' texts, an upload's
// bytes, a text whose display tree is stored beside it, a message and a broadcast, a phase
// handoff, a check result's observed text, an architecture import's snapshot and components, an
// editor's update appended to a document's history, and the JSON fields that carry a client's
// text, an ask's options and an event's payload. Each passes 90 KB or more, past the bound, so
// the statement that carries it is one the release must count. The dashboard's asset publish is
// the one such write no route makes: dashboard's TestPublishPassesALargeAssetInAKindTheReleaseCounts
// holds it.
var largeWrites = []largeWrite{
	{"a document version", func(t *testing.T, rig largeWriteRig) {
		uploadIssueDocument(t, rig.handler, rig.issueKey, "large-notes.md", largeMarkdown("A paragraph of the large document."))
	}},
	{"an upload's text", func(t *testing.T, rig largeWriteRig) {
		uploadVersions(t, rig.handler, "/api/v1/issues/"+rig.issueKey, "large-log.txt", "text/plain",
			[]byte(largeMarkdown("A line of the uploaded log.")))
	}},
	{"a comment, its stored display tree and its event's payload", func(t *testing.T, rig largeWriteRig) {
		created := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issueKey+"/comments",
			map[string]any{"body": largeMarkdown("A paragraph of the large comment.")}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the comment: status=%d body=%s", created.Code, created.Body.String())
		}
	}},
	{"a message", func(t *testing.T, rig largeWriteRig) {
		createIssueMessage(t, rig.handler, rig.issueKey,
			map[string]any{"body": largeMarkdown("A paragraph of the large message.")}, "alice")
	}},
	{"a broadcast and its copies", func(t *testing.T, rig largeWriteRig) {
		response := postBroadcast(t, rig.handler, map[string]any{
			"body": largeMarkdown("A paragraph of the large broadcast."), "delivery": "steer", "session_ids": []string{"planner", "tester"},
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("send the broadcast: status=%d body=%s", response.Code, response.Body.String())
		}
	}},
	{"a phase handoff", func(t *testing.T, rig largeWriteRig) {
		// A handoff's body is one JSON object, which the handoff stores as it was written.
		body, err := json.Marshal(map[string]string{"notes": largeMarkdown("A paragraph of the large handoff.")})
		if err != nil {
			t.Fatalf("encode the handoff: %v", err)
		}
		recordedHandoff(t, rig.handler, rig.issueKey, "implement", string(body))
	}},
	{"a check result's observed text", func(t *testing.T, rig largeWriteRig) {
		uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issueKey, "verification.md", twoChecks)
		recorded := recordResult(t, rig.handler, rig.issueKey, resultBody("k1", "pass", headCommit,
			map[string]any{"surface": "cli", "observed": largeMarkdown("A paragraph of what the check printed.")}))
		if recorded.Code != http.StatusCreated {
			t.Fatalf("record the result: status=%d body=%s", recorded.Code, recorded.Body.String())
		}
	}},
	{"an architecture import", func(t *testing.T, rig largeWriteRig) {
		// A project of its own: a source repository is no repository of the issue's project, whose
		// check results the write above records with no repository to ask about the commit.
		createTestProject(t, rig.handler, "LARGEARCH")
		putSource(t, rig.handler, "LARGEARCH", "legion/arch", "main")
		if synced := syncSource(t, rig.handler, "LARGEARCH", "legion/arch"); synced.LastError != nil || synced.Components != 1 {
			t.Fatalf("import the large note: %s", synced)
		}
	}},
	{"an editor's update", func(t *testing.T, rig largeWriteRig) {
		peer := connectBrowserPeer(t, rig.handler, rig.editor, rig.specID)
		t.Cleanup(peer.close)
		stored := storedUpdateCount(t, rig.database, rig.specID)
		peer.appendParagraph(t, largeMarkdown("A paragraph an editor typed."))
		awaitStoredUpdates(t, rig.database, rig.specID, stored+1)
	}},
	{"an ask's options, a JSON field", func(t *testing.T, rig largeWriteRig) {
		// The question and the options share a bound of 32,000 UTF-16 units
		// (validateAskWording). A character outside ASCII is one unit and three bytes, so the
		// options' JSON passes the bound in bytes while their wording stays inside it.
		response := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issueKey+"/asks", map[string]any{
			"question": "Which way?",
			"options": []map[string]string{
				{"label": "This way", "description": strings.Repeat("字", 30_000)},
				{"label": "That way"},
			},
			"actor": sessionActor(),
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("open the ask: status=%d body=%s", response.Code, response.Body.String())
		}
	}},
	{"an issue's spec", func(t *testing.T, rig largeWriteRig) {
		project, _, _ := strings.Cut(rig.issueKey, "-")
		response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": project, "title": "A large spec", "spec": largeMarkdown("A paragraph of the large spec."),
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create the issue: status=%d body=%s", response.Code, response.Body.String())
		}
	}},
}

// largeMarkdown is paragraph repeated, a paragraph at a time, to about 100 KiB of markdown.
func largeMarkdown(paragraph string) string {
	var text strings.Builder
	for line := 0; text.Len() < 100<<10; line++ {
		fmt.Fprintf(&text, "%s It is paragraph %d of the text.\n\n", paragraph, line)
	}
	return text.String()
}

// largeNoteRepository is a GitHub whose App reads legion/arch, whose architecture is one note of
// about 100 KiB.
func largeNoteRepository() *fakeGitHubApp {
	return &fakeGitHubApp{
		installations: map[string]string{"legion/arch": "read"},
		repos: map[string]*fakeArchitectureRepo{
			"legion/arch": {commit: shaArch, files: map[string]string{
				"large.md": "---\ntitle: Large\n---\n" + largeMarkdown("A paragraph of the large note."),
			}},
		},
	}
}

// A pooled connection gives back a statement's arguments on release only when the arguments it
// counts pass store.LargeArgumentBytesForTest, and it counts only the kinds whose size it reads
// cheaply (store.CountedArgumentBytesForTest): a value pgx encodes by marshalling it is not
// counted. So every statement a large write runs, alone or in a batch, must pass its bytes in a
// kind the release counts, or a connection keeps that write for the half hour it idles.
func TestNoLargeStoreWritePassesAnArgumentTheReleaseCannotCount(t *testing.T) {
	t.Parallel()
	reader := storetest.NewLargeArguments()
	// Registered before the server, so it runs after the server's own cleanups have shut its
	// document services down, and reads a statement a write left running too.
	t.Cleanup(func() { reader.RequireCounted(t) })
	listener, _ := newBroadcastListener(t, broadcastSessions)
	options := architectureAppOptions(t, largeNoteRepository())
	options.tracer = reader
	options.envoyURL = listener.URL
	handler, database, _, srv := newRegisteredTestServer(t, options)
	editor := docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: time.Hour})
	t.Cleanup(func() { _ = editor.Shutdown(context.Background()) })
	issue := createInteractionIssue(t, handler, "LARGE", "Large writes", "A spec.")
	rig := largeWriteRig{
		handler: handler, database: database, issueKey: issue.Key, specID: issue.PrimaryArtifactID, editor: editor,
	}
	names := make([]string, 0, len(largeWrites))
	for _, large := range largeWrites {
		reader.Begin(large.name)
		large.write(t, rig)
		awaitBackgroundWrites(t, srv)
		names = append(names, large.name)
	}
	reader.Begin("a write left running")
	reader.RequireLarge(t, names...)
}
