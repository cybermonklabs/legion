package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func init() { addTourSteps("documents", documentsTourSteps) }

// documentsTourSteps are the route tour's steps of the document routes: each route, the
// reads with what include= and the filters add, a file's version beside a document's, and the
// review with and without the version it guards.
func documentsTourSteps() []tourStep {
	alice := tourPerson("alice")
	documentMissing := []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.ArtifactNotFound}}
	issueMissing := []tourMissing{{Param: "key", Value: "NOPE-404", Code: apicode.IssueNotFound}}
	projectMissing := []tourMissing{{Param: "key", Value: "NOPE", Code: apicode.ProjectNotFound}}
	onSpec := func(query url.Values, body func(*testing.T, *routeTour, tourIssue) any) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			issue := tour.issue(t)
			request := tourRequest{Path: map[string]string{"id": issue.Document}, Query: query, As: alice}
			if body != nil {
				request.Body = body(t, tour, issue)
			}
			return request
		}
	}
	onProjectDocument := func(query url.Values, body any) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			return tourRequest{Path: map[string]string{"id": tour.document(t)}, Query: query, Body: body, As: alice}
		}
	}
	agentFiles := func(query func(slug string) url.Values) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			session := "session-tour-files-" + uuid.NewString()[:8]
			file := tour.agentFile(t, session)
			return tourRequest{Path: map[string]string{"session_id": session}, Query: query(file.Slug), As: alice}
		}
	}
	upload := func(name string) tourUpload {
		return tourUpload{
			Fields: map[string]string{"name": name}, Filename: name, ContentType: "text/plain", Content: []byte("A file the tour uploads.\n"),
		}
	}
	return []tourStep{
		{
			Method: http.MethodGet, Pattern: "/agents/{session_id}/artifacts", Status: http.StatusOK,
			Setup: agentFiles(func(string) url.Values { return nil }),
		},
		{
			Method: http.MethodGet, Pattern: "/agents/{session_id}/artifacts", Name: "one file by its slug", Status: http.StatusOK,
			Setup: agentFiles(func(slug string) url.Values { return url.Values{"slug": {slug}} }),
		},
		{
			Method: http.MethodPost, Pattern: "/agents/{session_id}/artifacts", Status: http.StatusCreated,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{
					Path: map[string]string{"session_id": "session-tour-files-" + uuid.NewString()[:8]},
					Body: upload("tour-report.txt"), As: alice,
				}
			},
		},
		{
			Method: http.MethodDelete, Pattern: "/artifacts/{id}", Status: http.StatusNoContent,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				document := emptyDocument(t, tour.handler, tour.issue(t).Key, "Tour scratch notes")
				awaitSettled(t, tour.database, document.ID)
				return tourRequest{Path: map[string]string{"id": document.ID}, As: alice}
			},
			Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}", Status: http.StatusOK,
			Setup: onSpec(nil, nil), Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}", Name: "with its text and its blocks", Status: http.StatusOK,
			Setup: onSpec(url.Values{"include": {"text,blocks"}}, nil), Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}", Name: "a file of an agent's conversation", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				file := tour.agentFile(t, "session-tour-files-"+uuid.NewString()[:8])
				return tourRequest{Path: map[string]string{"id": file.ID}, Query: url.Values{"include": {"text"}}, As: alice}
			},
			Missing: documentMissing,
		},
		{
			Method: http.MethodPatch, Pattern: "/artifacts/{id}", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				document := tour.issueDocument(t, tour.issue(t).Key, "Tour notes.md", "# Notes\n\nWhat the tour saw.\n")
				return tourRequest{Path: map[string]string{"id": document.ID}, Body: map[string]any{"name": "Tour notes, renamed.md"}, As: alice}
			},
			Missing: documentMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/approval-requests", Status: http.StatusCreated,
			Setup: onSpec(nil, func(*testing.T, *routeTour, tourIssue) any {
				return map[string]any{"summary": "The tour asks for the spec's approval."}
			}),
			Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/asks", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				document := tour.document(t)
				tour.documentAsk(t, document)
				return tourRequest{Path: map[string]string{"id": document}, As: alice}
			},
			Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/asks", Name: "the open ones, with the thread and the mark", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				document := tour.document(t)
				tour.documentAsk(t, document)
				return tourRequest{
					Path: map[string]string{"id": document}, Query: url.Values{"state": {"open"}, "include": {"thread,mark"}}, As: alice,
				}
			},
			Missing: documentMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/asks", Status: http.StatusCreated,
			Setup:   onProjectDocument(nil, map[string]any{"question": "Does the tour ask on a document?"}),
			Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/blocks/{block_id}", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				return tourRequest{Path: map[string]string{"id": issue.Document, "block_id": tour.firstBlock(t, issue.Document)}, As: alice}
			},
			Missing: append([]tourMissing{{Param: "block_id", Value: "no-such-block", Code: apicode.BlockNotFound}}, documentMissing...),
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/comments", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				document := tour.document(t)
				tour.documentComment(t, document)
				return tourRequest{Path: map[string]string{"id": document}, As: alice}
			},
			Missing: documentMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/comments", Status: http.StatusCreated,
			Setup:   onProjectDocument(nil, map[string]any{"body": "A comment the tour leaves on a document."}),
			Missing: documentMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/edits", Status: http.StatusOK,
			Setup: onSpec(nil, func(*testing.T, *routeTour, tourIssue) any {
				return map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "A line the tour adds."}}}
			}),
			Missing: documentMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/plan-reviews", Status: http.StatusCreated,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				plan := tour.issueDocument(t, tour.issue(t).Key, "plan.md", "# Plan\n\nThe tour's plan.\n")
				return tourRequest{
					Path: map[string]string{"id": plan.ID}, Body: map[string]any{"version": tour.latestVersion(t, plan.ID)}, As: alice,
				}
			},
			Missing: documentMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/rebuilds", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Path: map[string]string{"id": tour.unloadableDocument(t)}, Body: map[string]any{}, As: alice}
			},
			Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/references", Status: http.StatusOK,
			Setup: onProjectDocument(nil, nil), Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/references", Name: "what it cites", Status: http.StatusOK,
			Setup: onProjectDocument(url.Values{"direction": {"out"}}, nil), Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/reviews", Status: http.StatusOK,
			Setup: onSpec(nil, nil), Missing: documentMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/reviews", Status: http.StatusCreated,
			Setup: onSpec(nil, func(*testing.T, *routeTour, tourIssue) any {
				return map[string]any{"state": "approved"}
			}),
			Missing: documentMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/reviews", Name: "guarded by the version read", Status: http.StatusCreated,
			Setup: onSpec(nil, func(t *testing.T, tour *routeTour, issue tourIssue) any {
				return map[string]any{"state": "changes_requested", "reason": "The tour wants more.", "version": tour.latestVersion(t, issue.Document)}
			}),
			Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/subscribers", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				document := tour.document(t)
				tour.subscribeToDocument(t, document)
				return tourRequest{Path: map[string]string{"id": document}, As: alice}
			},
			Missing: documentMissing,
		},
		{
			Method: http.MethodDelete, Pattern: "/artifacts/{id}/subscribers/{session_id}", Status: http.StatusNoContent,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				document := tour.document(t)
				return tourRequest{Path: map[string]string{"id": document, "session_id": tour.subscribeToDocument(t, document)}, As: alice}
			},
			Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/versions", Status: http.StatusOK,
			Setup: onSpec(nil, nil), Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/versions", Name: "newest first, a page of one", Status: http.StatusOK,
			Setup: onSpec(url.Values{"sort": {"-number"}, "limit": {"1"}}, nil), Missing: documentMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/versions", Status: http.StatusCreated,
			Setup: onSpec(nil, func(*testing.T, *routeTour, tourIssue) any {
				return map[string]any{"summary": "The version the tour names."}
			}),
			Missing: documentMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/versions/{number}", Status: http.StatusOK,
			Setup:   tourVersion(nil),
			Missing: append([]tourMissing{{Param: "number", Value: "999", Code: apicode.VersionNotFound}}, documentMissing...),
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/versions/{number}", Name: "its Markdown, downloaded", Status: http.StatusOK,
			Setup:   tourVersion(url.Values{"download": {"true"}}),
			Missing: append([]tourMissing{{Param: "number", Value: "999", Code: apicode.VersionNotFound}}, documentMissing...),
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/versions/{number}", Name: "a file's bytes", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				file := tour.issueFile(t, tour.issue(t).Key)
				return tourRequest{Path: map[string]string{"id": file.ID, "number": "1"}, As: alice}
			},
			Missing: append([]tourMissing{{Param: "number", Value: "2", Code: apicode.VersionNotFound}}, documentMissing...),
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/artifacts", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Path: map[string]string{"key": tour.issue(t).Key}, As: alice}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/artifacts", Name: "the spec by its slug, with its text and its blocks", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				return tourRequest{
					Path:  map[string]string{"key": issue.Key},
					Query: url.Values{"slug": {tour.slugOf(t, issue.Document)}, "include": {"text,blocks"}},
					As:    alice,
				}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/artifacts", Status: http.StatusCreated,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{
					Path: map[string]string{"key": tour.issue(t).Key},
					Body: map[string]any{"name": "tour-findings.md", "content": "# Findings\n\nWhat the tour found.\n"}, As: alice,
				}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/artifacts", Name: "a file", Status: http.StatusCreated,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Path: map[string]string{"key": tour.issue(t).Key}, Body: upload("tour-log.txt"), As: alice}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/artifacts", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				project := tour.project(t)
				createProjectDocument(t, tour.handler, project, "tour-notes.md", "# Notes\n\nA passage the tour reads.")
				return tourRequest{Path: map[string]string{"key": project}, As: alice}
			},
			Missing: projectMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/artifacts", Name: "its own document by its slug, with its text", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				project := tour.project(t)
				document := createProjectDocument(t, tour.handler, project, "tour-notes.md", "# Notes\n\nA passage the tour reads.")
				return tourRequest{
					Path:  map[string]string{"key": project},
					Query: url.Values{"issue": {"none"}, "slug": {document.Slug}, "include": {"text"}},
					As:    alice,
				}
			},
			Missing: projectMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/projects/{key}/artifacts", Status: http.StatusCreated,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{
					Path: map[string]string{"key": tour.project(t)},
					Body: map[string]any{"name": "tour-design.md", "content": "# Design\n\nThe tour's design.\n"}, As: alice,
				}
			},
			Missing: projectMissing,
		},
	}
}

// tourVersion is the setup of a read of the spec's newest version, with query.
func tourVersion(query url.Values) func(*testing.T, *routeTour) tourRequest {
	return func(t *testing.T, tour *routeTour) tourRequest {
		issue := tour.issue(t)
		number := strconv.Itoa(tour.latestVersion(t, issue.Document))
		return tourRequest{Path: map[string]string{"id": issue.Document, "number": number}, Query: query, As: tourPerson("alice")}
	}
}

// issueDocument uploads a markdown document named name to the issue key, as alice, answering it
// once it owes no settlement.
func (tour *routeTour) issueDocument(t *testing.T, key, name, content string) model.Artifact {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts", map[string]string{
		"name": name, "content": content,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("upload %s to %s: %d %s", name, key, created.Code, created.Body.String())
	}
	document := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
	awaitSettled(t, tour.database, document.ID)
	return document
}

// issueFile uploads a text file to the issue key, as alice, answering it.
func (tour *routeTour) issueFile(t *testing.T, key string) model.Artifact {
	t.Helper()
	created := multipartRequest(t, tour.handler, "/api/v1/issues/"+key+"/artifacts", map[string]string{"name": "tour-log.txt"},
		"tour-log.txt", "text/plain", []byte("A file the tour keeps.\n"), "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("upload a file to %s: %d %s", key, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
}

// agentFile uploads a text file to session's conversation, as alice, answering it.
func (tour *routeTour) agentFile(t *testing.T, session string) model.Artifact {
	t.Helper()
	created := multipartRequest(t, tour.handler, "/api/v1/agents/"+session+"/artifacts", map[string]string{"name": "tour-report.txt"},
		"tour-report.txt", "text/plain", []byte("A report the tour hands an agent.\n"), "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("upload a file to %s: %d %s", session, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
}

// documentAsk opens an ask on the project document, as alice, answering its id.
func (tour *routeTour) documentAsk(t *testing.T, document string) string {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/artifacts/"+document+"/asks", map[string]any{
		"question": "Which way does the document go?",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("ask on %s: %d %s", document, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
}

// documentComment comments on the project document, as alice.
func (tour *routeTour) documentComment(t *testing.T, document string) {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/artifacts/"+document+"/comments", map[string]any{
		"body": "A comment the tour reads back.",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("comment on %s: %d %s", document, created.Code, created.Body.String())
	}
}

// latestVersion is the number of the artifact's newest version.
func (tour *routeTour) latestVersion(t *testing.T, artifact string) int {
	t.Helper()
	var number int
	if err := tour.database.Pool.QueryRow(t.Context(), `select max(number) from artifact_versions where artifact_id = $1`, artifact).Scan(&number); err != nil {
		t.Fatalf("read the newest version of %s: %v", artifact, err)
	}
	return number
}

// slugOf is the artifact's slug.
func (tour *routeTour) slugOf(t *testing.T, artifact string) string {
	t.Helper()
	var slug string
	if err := tour.database.Pool.QueryRow(t.Context(), `select slug from artifacts where id = $1`, artifact).Scan(&slug); err != nil {
		t.Fatalf("read the slug of %s: %v", artifact, err)
	}
	return slug
}

// firstBlock is the id of the document's first block, as GET /api/v1/artifacts/{id}/blocks lists it.
func (tour *routeTour) firstBlock(t *testing.T, document string) string {
	t.Helper()
	read := dispatchRequest(t, tour.handler, http.MethodGet, "/api/v1/artifacts/"+document+"/blocks", nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the blocks of %s: %d %s", document, read.Code, read.Body.String())
	}
	blocks := decodeBody[[]model.ArtifactBlock](t, read)
	if len(blocks) == 0 {
		t.Fatalf("the document %s holds no block", document)
	}
	return blocks[0].ID
}

// subscribeToDocument tells the listener a session of its own follows the project document's
// topics, answering the session.
func (tour *routeTour) subscribeToDocument(t *testing.T, document string) string {
	t.Helper()
	var project, slug string
	if err := tour.database.Pool.QueryRow(t.Context(), `select project_key, slug from artifacts where id = $1`, document).Scan(&project, &slug); err != nil {
		t.Fatalf("read the document %s: %v", document, err)
	}
	session := "session-tour-subscriber-" + uuid.NewString()[:8]
	tour.listener.mu.Lock()
	tour.listener.interests[session] = []string{documentSubscriberBase(project, slug) + ".>"}
	tour.listener.mu.Unlock()
	return session
}

// unloadableDocument is a project document whose stored history does not decode, with no room
// holding it and no settlement owed: the document a rebuild is for (makeDocumentUnloadable).
func (tour *routeTour) unloadableDocument(t *testing.T) string {
	t.Helper()
	document := tour.document(t)
	awaitSettled(t, tour.database, document)
	ctx := context.Background()
	if err := tour.served.deps.Docs.Evict(ctx, document); err != nil {
		t.Fatalf("close the document's room: %v", err)
	}
	seedExec(t, tour.database, "the document's owed settlement cleared", `delete from doc_settlements_pending where artifact_id = $1`, document)
	corrupted, err := tour.database.Pool.Exec(ctx, `update doc_updates set update = '\xff'::bytea where artifact_id = $1`, document)
	if err != nil || corrupted.RowsAffected() == 0 {
		t.Fatalf("corrupt the document's history: %d rows, %v", corrupted.RowsAffected(), err)
	}
	return document
}

// sameDocumentRow reports what differs between a /api/v1 read of an artifact, v1 as that read
// answers it, and the /api/v2 row of the same artifact: every member of v1 but versions, which the
// /api/v2 row answers as latest_version, latest_version_at and latest_version_size, the number,
// the time and the size of the newest of them, and version_count, how many they are. members are
// the members of v1 to compare, every one where nil.
func sameDocumentRow(t *testing.T, v1, v2 map[string]json.RawMessage, members []string) {
	t.Helper()
	var versions []model.Version
	if raw, held := v1["versions"]; held {
		if err := json.Unmarshal(raw, &versions); err != nil {
			t.Fatalf("read the /api/v1 versions: %v", err)
		}
		var newest *model.Version
		for index := range versions {
			if newest == nil || versions[index].Number > newest.Number {
				newest = &versions[index]
			}
		}
		if newest == nil {
			t.Fatalf("the /api/v1 read answers no version: %s", v1["versions"])
		}
		if got := string(v2["latest_version"]); got != strconv.Itoa(newest.Number) {
			t.Errorf("the /api/v2 row's latest_version is %s, want %d, the newest of the /api/v1 read's versions", got, newest.Number)
		}
		facts := newestVersionFactsOf(t, v2)
		if facts.At == nil || !facts.At.Equal(newest.CreatedAt) || !reflect.DeepEqual(facts.Size, newest.Size) || *facts.Count != len(versions) {
			t.Errorf("the /api/v2 row says latest_version_at %s, latest_version_size %s and version_count %s; the /api/v1 read "+
				"answers %d versions, the newest written at %s storing %v bytes", v2["latest_version_at"], v2["latest_version_size"],
				v2["version_count"], len(versions), newest.CreatedAt, newest.Size)
		}
		if _, still := v2["versions"]; still {
			t.Errorf("the /api/v2 row carries versions[], which GET /api/v2/artifacts/{id}/versions lists")
		}
	}
	if members == nil {
		members = make([]string, 0, len(v1))
		for member := range maps.Keys(v1) {
			if member != "versions" {
				members = append(members, member)
			}
		}
	}
	for _, member := range members {
		var want, got any
		_ = json.Unmarshal(v1[member], &want)
		_ = json.Unmarshal(v2[member], &got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("member %s: the /api/v2 row answers %s, the /api/v1 read %s", member, v2[member], v1[member])
		}
	}
}

// TestEachV2ArtifactListAnswersTheDocumentItsV1SlugReadAnswers: each
// /api/v1 read by slug - an issue's document by its slug and by its exact file name, a project's
// own, a conversation's file, and each one's text, blocks, summary and version - answers the
// document the /api/v2 list filtered by that slug answers, as one row, with what include= adds;
// and a project's slug, which never reached an issue's document of the project, matches none
// there under issue=none either.
func TestEachV2ArtifactListAnswersTheDocumentItsV1SlugReadAnswers(t *testing.T) {
	tour := newRouteTour(t)
	get := func(t *testing.T, target string) []byte {
		t.Helper()
		response := dispatchRequest(t, tour.handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", target, response.Code, response.Body.String())
		}
		return response.Body.Bytes()
	}
	object := func(t *testing.T, body []byte) map[string]json.RawMessage {
		t.Helper()
		var members map[string]json.RawMessage
		if err := json.Unmarshal(body, &members); err != nil {
			t.Fatalf("read %s: %v", body, err)
		}
		return members
	}
	only := func(t *testing.T, target string) map[string]json.RawMessage {
		t.Helper()
		listed := decodeBody[page[json.RawMessage]](t, dispatchRequest(t, tour.handler, http.MethodGet, target, nil, "alice"))
		if listed.Total != 1 || len(listed.Items) != 1 {
			t.Fatalf("GET %s answered %d of %d rows, want the one document", target, len(listed.Items), listed.Total)
		}
		return object(t, listed.Items[0])
	}

	issue := tour.issue(t)
	notes := tour.issueDocument(t, issue.Key, "Tour Notes.md", "# Notes\n\n- one\n- two\n")
	awaitSettled(t, tour.database, issue.Document, notes.ID)
	v1Issue, v2Issue := "/api/v1/issues/"+issue.Key+"/artifacts/", "/api/v2/issues/"+issue.Key+"/artifacts?"

	t.Run("an issue's document by its slug and by its file name", func(t *testing.T) {
		for _, reference := range []string{notes.Slug, notes.Name} {
			v1 := object(t, get(t, v1Issue+url.PathEscape(reference)))
			sameDocumentRow(t, v1, only(t, v2Issue+url.Values{"slug": {reference}}.Encode()), nil)
		}
	})

	t.Run("its text and its blocks from one tree", func(t *testing.T) {
		text := object(t, get(t, v1Issue+notes.Slug+"/text?blocks=true"))
		row := only(t, v2Issue+url.Values{"slug": {notes.Slug}, "include": {"text,blocks"}}.Encode())
		sameDocumentRow(t, text, row, []string{"markdown", "token", "version", "blocks"})
		blocks := map[string]json.RawMessage{"blocks": get(t, v1Issue+notes.Slug+"/blocks")}
		sameDocumentRow(t, blocks, only(t, v2Issue+url.Values{"slug": {notes.Slug}, "include": {"blocks"}}.Encode()), []string{"blocks"})
		if _, carried := row["approval"]; !carried {
			t.Errorf("the /api/v2 row of a document carries no approval: %v", row)
		}
	})

	t.Run("its version", func(t *testing.T) {
		v1 := get(t, v1Issue+notes.Slug+"/versions/1")
		if v2 := get(t, "/api/v2/artifacts/"+notes.ID+"/versions/1"); string(v1) != string(v2) {
			t.Fatalf("version 1 by slug answered %s, and by id under /api/v2 %s", v1, v2)
		}
	})

	project := tour.project(t)
	design := createProjectDocument(t, tour.handler, project, "Tour design.md", "# Design\n\nA passage.\n")
	awaitSettled(t, tour.database, design.ID)
	v1Project, v2Project := "/api/v1/projects/"+project+"/artifacts/", "/api/v2/projects/"+project+"/artifacts?issue=none&"

	t.Run("a project's own document, its summary, its text, its blocks and its version", func(t *testing.T) {
		for _, reference := range []string{design.Slug, design.Name} {
			v1 := object(t, get(t, v1Project+url.PathEscape(reference)))
			sameDocumentRow(t, v1, only(t, v2Project+url.Values{"slug": {reference}}.Encode()), nil)
		}
		summary := object(t, get(t, v1Project+design.Slug+"/summary"))
		sameDocumentRow(t, summary, only(t, v2Project+url.Values{"slug": {design.Slug}}.Encode()), nil)
		text := object(t, get(t, v1Project+design.Slug+"/text"))
		sameDocumentRow(t, text, only(t, v2Project+url.Values{"slug": {design.Slug}, "include": {"text"}}.Encode()),
			[]string{"markdown", "token", "version"})
		blocks := map[string]json.RawMessage{"blocks": get(t, v1Project+design.Slug+"/blocks")}
		sameDocumentRow(t, blocks, only(t, v2Project+url.Values{"slug": {design.Slug}, "include": {"blocks"}}.Encode()), []string{"blocks"})
		if v1, v2 := get(t, v1Project+design.Slug+"/versions/1"), get(t, "/api/v2/artifacts/"+design.ID+"/versions/1"); string(v1) != string(v2) {
			t.Fatalf("version 1 by slug answered %s, and by id under /api/v2 %s", v1, v2)
		}
	})

	t.Run("a project's slug reaches none of its issues' documents", func(t *testing.T) {
		inProject := tour.issue(t)
		key := inProject.Key
		var owner string
		if err := tour.database.Pool.QueryRow(t.Context(), `select project_key from issues where key = $1`, key).Scan(&owner); err != nil {
			t.Fatalf("read the issue's project: %v", err)
		}
		spec := tour.slugOf(t, inProject.Document)
		plan := tour.issueDocument(t, key, "plan.md", "# Plan\n")
		refused := dispatchRequest(t, tour.handler, http.MethodGet, "/api/v1/projects/"+owner+"/artifacts/"+plan.Slug, nil, "alice")
		if code, _ := refusalOf(t, refused); refused.Code != http.StatusNotFound || code != "ARTIFACT_NOT_FOUND" {
			t.Fatalf("the /api/v1 project read of an issue's document: %d %s, want 404 ARTIFACT_NOT_FOUND", refused.Code, refused.Body.String())
		}
		none := decodeBody[page[json.RawMessage]](t, dispatchRequest(t, tour.handler, http.MethodGet,
			"/api/v2/projects/"+owner+"/artifacts?"+url.Values{"issue": {"none"}, "slug": {plan.Slug}}.Encode(), nil, "alice"))
		if none.Total != 0 || len(none.Items) != 0 {
			t.Fatalf("issue=none&slug=%s answered %d rows, want none: the document is an issue's", plan.Slug, none.Total)
		}
		ofIssue := decodeBody[page[json.RawMessage]](t, dispatchRequest(t, tour.handler, http.MethodGet,
			"/api/v2/projects/"+owner+"/artifacts?"+url.Values{"issue": {key}, "slug": {plan.Slug}}.Encode(), nil, "alice"))
		if ofIssue.Total != 1 {
			t.Fatalf("issue=%s&slug=%s answered %d rows, want the issue's plan", key, plan.Slug, ofIssue.Total)
		}
		specs := decodeBody[page[json.RawMessage]](t, dispatchRequest(t, tour.handler, http.MethodGet,
			"/api/v2/projects/"+owner+"/artifacts?"+url.Values{"slug": {spec}}.Encode(), nil, "alice"))
		if specs.Total != 0 {
			t.Fatalf("slug=%s answered %d rows of the project's list, which leaves out every issue's spec", spec, specs.Total)
		}
	})

	t.Run("a conversation's file and its version", func(t *testing.T) {
		session := "session-tour-files-" + uuid.NewString()[:8]
		file := tour.agentFile(t, session)
		v1 := object(t, get(t, "/api/v1/agents/"+session+"/artifacts/"+file.Slug))
		sameDocumentRow(t, v1, only(t, "/api/v2/agents/"+session+"/artifacts?"+url.Values{"slug": {file.Slug}}.Encode()), nil)
		if v1, v2 := get(t, "/api/v1/agents/"+session+"/artifacts/"+file.Slug+"/versions/1"), get(t, "/api/v2/artifacts/"+file.ID+"/versions/1"); string(v1) != string(v2) {
			t.Fatalf("the file's version 1 by slug answered %q, and by id under /api/v2 %q", v1, v2)
		}
	})
}

// newestVersionFacts are what a /api/v2 artifact row says of the artifact's versions: the number,
// the time and the size of the newest, and how many there are.
type newestVersionFacts struct {
	Number *int
	At     *time.Time
	Size   *int
	Count  *int
}

// newestVersionFactsOf reads newestVersionFacts from row, failing where one of its members is
// absent, since each is on every row (R6), or where version_count is null.
func newestVersionFactsOf(t *testing.T, row map[string]json.RawMessage) newestVersionFacts {
	t.Helper()
	var facts newestVersionFacts
	for member, into := range map[string]any{
		"latest_version": &facts.Number, "latest_version_at": &facts.At, "latest_version_size": &facts.Size, "version_count": &facts.Count,
	} {
		raw, held := row[member]
		if !held {
			t.Fatalf("the row %s carries no %s", row["id"], member)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("read %s of the row %s: %v", member, row["id"], err)
		}
	}
	if facts.Count == nil {
		t.Fatalf("the row %s answers version_count null", row["id"])
	}
	return facts
}

// TestEachV2ArtifactRowCarriesTheFactsOfItsNewestVersion: a reader shows an artifact's newest
// version - its number, when it was written, its size - and how many versions it has from the row
// alone, reading none of the versions. Each row of an issue's /api/v2 list carries them as
// artifact_versions holds them: a document's second version, a file's bytes, a version that
// records no size (its size null), and an artifact with no version at all (null, null, null and
// 0), which the read by id and a document citing it (GET /api/v2/artifacts/{id}/references,
// direction=out) answer alike. The upload of a version answers the row counting the version it
// wrote, and a reference closure's member the facts the list answers of it.
func TestEachV2ArtifactRowCarriesTheFactsOfItsNewestVersion(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	artifacts := "/api/v1/issues/" + issue.Key + "/artifacts"
	first := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "notes.md", "# Notes\n\nThe first draft.\n")
	// The second draft replaces the first, so it names the token of the version it replaces (#3000).
	secondDraft := dispatchRequest(t, handler, http.MethodPost, "/api/v2/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "notes.md", "content": "# Notes\n\nThe second draft, which says a good deal more than the first.\n",
		"precondition": uploadBase(t, handler, first.Artifact.ID),
	}, "alice")
	if secondDraft.Code != http.StatusCreated {
		t.Fatalf("upload the second draft under /api/v2: %d %s", secondDraft.Code, secondDraft.Body.String())
	}
	uploaded := decodeBody[struct {
		Artifact map[string]json.RawMessage `json:"artifact"`
		Version  model.Version              `json:"version"`
	}](t, secondDraft)
	var notes string
	if err := json.Unmarshal(uploaded.Artifact["id"], &notes); err != nil {
		t.Fatalf("read the uploaded document's id: %v", err)
	}
	if facts := newestVersionFactsOf(t, uploaded.Artifact); uploaded.Version.Number != 2 || facts.Number == nil ||
		*facts.Number != 2 || *facts.Count != 2 || facts.At == nil || !facts.At.Equal(uploaded.Version.CreatedAt) ||
		!reflect.DeepEqual(facts.Size, uploaded.Version.Size) {
		t.Fatalf("the upload of version %d answers the row %v, want it newest and the second", uploaded.Version.Number, uploaded.Artifact)
	}
	content := []byte("A log the issue keeps, its bytes counted.\n")
	logged := multipartRequest(t, handler, artifacts, map[string]string{"name": "log.txt"}, "log.txt", "text/plain", content, "alice")
	if logged.Code != http.StatusCreated {
		t.Fatalf("upload the log: %d %s", logged.Code, logged.Body.String())
	}
	file := decodeBody[uploadedArtifact](t, logged).Artifact.ID
	stored := func(slug string) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(t.Context(), `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ($1, 'CORE', $2, $2, 'file', '{"kind":"human","id":"alice"}') returning id::text`, issue.Key, slug).Scan(&id); err != nil {
			t.Fatalf("store the file %s: %v", slug, err)
		}
		return id
	}
	unsized, none := stored("unsized-bin"), stored("unversioned-bin")
	seedExec(t, database, "a version that records no size", `insert into artifact_versions (artifact_id, number) values ($1, 1)`, unsized)

	listed := decodeBody[page[map[string]json.RawMessage]](t, dispatchRequest(t, handler, http.MethodGet, "/api/v2/issues/"+issue.Key+"/artifacts", nil, "alice"))
	rows := map[string]newestVersionFacts{}
	for _, row := range listed.Items {
		var id string
		if err := json.Unmarshal(row["id"], &id); err != nil {
			t.Fatalf("read a row's id: %v", err)
		}
		rows[id] = newestVersionFactsOf(t, row)
		var count int
		if err := database.Pool.QueryRow(t.Context(), `select count(*) from artifact_versions where artifact_id = $1`, id).Scan(&count); err != nil {
			t.Fatalf("count the versions of %s: %v", id, err)
		}
		var number *int
		var at *time.Time
		var size *int
		err := database.Pool.QueryRow(t.Context(), `
			select v.number, v.created_at, `+model.VersionSize+` from artifact_versions v
			where v.artifact_id = $1 order by v.number desc limit 1`, id).Scan(&number, &at, &size)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read the newest version of %s: %v", id, err)
		}
		facts := rows[id]
		if !reflect.DeepEqual(facts.Number, number) || !reflect.DeepEqual(facts.Size, size) || *facts.Count != count ||
			(facts.At == nil) != (at == nil) || (at != nil && !facts.At.Equal(*at)) {
			t.Errorf("the row of %s says latest_version %s, latest_version_at %s, latest_version_size %s and version_count %s; "+
				"artifact_versions holds %d, the newest %v written at %v storing %v bytes", id, row["latest_version"],
				row["latest_version_at"], row["latest_version_size"], row["version_count"], count, number, at, size)
		}
	}
	for id, want := range map[string]struct {
		count int
		sized bool
	}{notes: {2, true}, file: {1, true}, unsized: {1, false}, none: {0, false}} {
		facts, held := rows[id]
		switch {
		case !held:
			t.Errorf("the issue's list holds no row of %s", id)
		case *facts.Count != want.count || (facts.Number == nil) != (want.count == 0) || (facts.At == nil) != (want.count == 0) ||
			(facts.Size != nil) != want.sized:
			t.Errorf("the row of %s says %d versions, the newest %v at %v of %v bytes; want %d versions, a size %t",
				id, *facts.Count, facts.Number, facts.At, facts.Size, want.count, want.sized)
		}
	}
	if size := rows[file].Size; size == nil || *size != len(content) {
		t.Errorf("the log's row says latest_version_size %v, want the %d bytes it stores", size, len(content))
	}
	read := decodeBody[map[string]json.RawMessage](t, dispatchRequest(t, handler, http.MethodGet, "/api/v2/artifacts/"+none, nil, "alice"))
	if facts := newestVersionFactsOf(t, read); facts.Number != nil || facts.At != nil || facts.Size != nil || *facts.Count != 0 {
		t.Errorf("GET /api/v2/artifacts/{id} of an artifact with no version answers %v, want null, null, null and 0", read)
	}

	// The citing issue is another project's, where no issue of the same title asks to be read first.
	createReferenceAPIProject(t, handler, "OPS")
	citing := createReferenceAPIIssue(t, handler, "OPS")
	var slug string
	if err := database.Pool.QueryRow(t.Context(), `select slug from artifacts where id = $1`, notes).Scan(&slug); err != nil {
		t.Fatalf("read the document's slug: %v", err)
	}
	createReferenceAPIComment(t, handler, citing.Key, "dispatch://"+issue.Key+"/artifact/"+slug)
	closure := decodeBody[page[struct {
		Artifact map[string]json.RawMessage `json:"artifact"`
	}]](t, dispatchRequest(t, handler, http.MethodGet, "/api/v2/issues/"+citing.Key+"/references", nil, "alice"))
	if len(closure.Items) != 1 {
		t.Fatalf("the citing issue's closure holds %d members, want the document", len(closure.Items))
	}
	member := closure.Items[0].Artifact
	if facts := newestVersionFactsOf(t, member); !reflect.DeepEqual(facts.Number, rows[notes].Number) ||
		!reflect.DeepEqual(facts.Size, rows[notes].Size) || *facts.Count != *rows[notes].Count || facts.At == nil ||
		!facts.At.Equal(*rows[notes].At) {
		t.Errorf("the closure's member says %v, and the issue's list %+v", member, rows[notes])
	}
	if _, still := member["versions"]; still {
		t.Errorf("the closure's member carries versions[], which GET /api/v2/artifacts/{id}/versions lists")
	}

	// refs.Outgoing answers a cited artifact with no version as holding one numbered 0, which GET
	// /api/v1/artifacts/{id}/references keeps answering; the /api/v2 row counts no version.
	citation := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "citation.md",
		"# Citation\n\nSee dispatch://"+issue.Key+"/artifact/unversioned-bin for the file.\n").Artifact.ID
	out := decodeBody[page[struct {
		Artifact map[string]json.RawMessage `json:"artifact"`
	}]](t, dispatchRequest(t, handler, http.MethodGet, "/api/v2/artifacts/"+citation+"/references?direction=out", nil, "alice"))
	var cited map[string]json.RawMessage
	for _, item := range out.Items {
		var id string
		if json.Unmarshal(item.Artifact["id"], &id) == nil && id == none {
			cited = item.Artifact
		}
	}
	if cited == nil {
		t.Fatalf("the citation's references answer %d items, none the artifact with no version", len(out.Items))
	}
	if facts := newestVersionFactsOf(t, cited); facts.Number != nil || facts.At != nil || facts.Size != nil || *facts.Count != 0 {
		t.Errorf("the citation's reference to an artifact with no version answers %v, want null, null, null and 0", cited)
	}
	v1 := decodeBody[model.ArtifactReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+citation+"/references", nil, "alice"))
	edges := 0
	for _, edge := range v1.Outgoing {
		if edge.Artifact == nil || edge.Artifact.ID != none {
			continue
		}
		edges++
		if len(edge.Artifact.Versions) != 1 || edge.Artifact.Versions[0].Number != 0 {
			t.Errorf("the /api/v1 references answer the artifact with no version as %+v, want one version numbered 0", edge.Artifact.Versions)
		}
	}
	if edges != 1 {
		t.Errorf("the /api/v1 references of the citation answer %d edges to the artifact with no version, want one", edges)
	}
}

// TestAV2ListsThatRenderEachRowTakeAPageOfTwenty: an include member that renders each listed row
// whole holds a page of its list to 20 (includedPageMax): include=text and include=blocks on an
// issue's or a project's artifacts, each document's text up to a MiB and its tree of blocks
// (documentBlocks); include=body on an issue's handoffs, each body up to
// contracts.DispatchHandoffBodyMax; and include=thread on each of the three lists of asks, each
// ask's replies, rewordings, answers and followers (include=blocks and include=thread by the
// architect's rulings of 2026-11-21 on dispatch://INFRA-4071, include=blocks on test round 3). On
// each (list, include) row, all on one store: limit=21 or limit=200 with the include is 400
// INVALID_LIMIT naming 1 to 20 and the members it names, before anything is read, so a missing
// owner's list too; limit=20 with it reads a page of 20, and so does the include naming no limit;
// and with no member that renders, limit=21, limit=200 and no limit read 21, 200 and 50, as the
// row's own reads read theirs: include=mark, which renders nothing, on a list of asks, and the
// handoff description's phase=<phase>&sort=-version&limit=1&include=body. Where a row counts its
// list - 21 asks on an issue and 21 on a project document, one handoff - each page holds as many
// rows as its limit takes, total the whole count, and each row carries the member only where the
// query names it, a handoff's body the one recorded.
func TestAV2ListsThatRenderEachRowTakeAPageOfTwenty(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	document := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")
	const asked = includedPageMax + 1
	for index := range asked {
		for _, owner := range []string{"/api/v1/issues/" + issue.Key + "/asks", "/api/v1/artifacts/" + document.ID + "/asks"} {
			created := dispatchRequest(t, handler, http.MethodPost, owner, map[string]any{"question": "Which way, " + strconv.Itoa(index) + "?"}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("POST %s: %d %s", owner, created.Code, created.Body.String())
			}
		}
	}
	body := `{"plan":"the page"}`
	recordedHandoff(t, handler, issue.Key, "plan", body)

	issueArtifacts, missingIssueArtifacts := "/api/v2/issues/"+issue.Key+"/artifacts", "/api/v2/issues/CORE-404/artifacts"
	projectArtifacts, missingProjectArtifacts := "/api/v2/projects/CORE/artifacts", "/api/v2/projects/NOPE/artifacts"
	marked := map[string]int{"limit=200&include=mark": 200}
	for _, row := range []struct {
		// list is the list's path, include the include value naming members that render each
		// listed row, and names how a refusal names those members.
		list, include, names string
		// missing is the list of an owner that does not exist, "" for a list no owner holds.
		missing string
		// reads are the row's own reads beside those every row makes, each query's limit.
		reads map[string]int
		// rows counts the list, 0 for a list not counted; field is the member each listed row
		// carries only where the query names include, and holds what it holds, "" for anything.
		rows         int
		field, holds string
	}{
		{list: issueArtifacts, include: "text", names: "include=text", missing: missingIssueArtifacts},
		{list: issueArtifacts, include: "blocks", names: "include=blocks", missing: missingIssueArtifacts},
		{list: issueArtifacts, include: "text,blocks", names: "include=text and include=blocks", missing: missingIssueArtifacts},
		{list: projectArtifacts, include: "text", names: "include=text", missing: missingProjectArtifacts},
		{list: projectArtifacts, include: "blocks", names: "include=blocks", missing: missingProjectArtifacts},
		{list: projectArtifacts, include: "text,blocks", names: "include=text and include=blocks", missing: missingProjectArtifacts},
		{
			list: "/api/v2/issues/" + issue.Key + "/handoffs", include: "body", names: "include=body",
			missing: "/api/v2/issues/CORE-404/handoffs", rows: 1, field: "body", holds: body,
			reads: map[string]int{"phase=plan&sort=-version&limit=1&include=body": 1},
		},
		{list: "/api/v2/asks", include: "thread", names: "include=thread", reads: marked, rows: 2 * asked, field: "thread"},
		{
			list: "/api/v2/issues/" + issue.Key + "/asks", include: "thread", names: "include=thread",
			missing: "/api/v2/issues/CORE-404/asks", reads: marked, rows: asked, field: "thread",
		},
		{
			list: "/api/v2/artifacts/" + document.ID + "/asks", include: "thread", names: "include=thread",
			missing: "/api/v2/artifacts/" + uuid.NewString() + "/asks", reads: marked, rows: asked, field: "thread",
		},
	} {
		refused := []string{row.list + "?limit=21&include=" + row.include, row.list + "?limit=200&include=" + row.include}
		if row.missing != "" {
			refused = append(refused, row.missing+"?limit=21&include="+row.include)
		}
		for _, target := range refused {
			answered := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
			if code, text := refusalOf(t, answered); answered.Code != http.StatusBadRequest || code != "INVALID_LIMIT" || !strings.Contains(text, "1 to 20 with "+row.names) {
				t.Errorf("GET %s answered %d %s, want 400 INVALID_LIMIT naming 1 to 20 with %s", target, answered.Code, answered.Body.String(), row.names)
			}
		}
		reads := map[string]int{"limit=20&include=" + row.include: 20, "include=" + row.include: 20, "limit=21": 21, "limit=200": 200, "": 50}
		maps.Copy(reads, row.reads)
		for query, limit := range reads {
			answered := dispatchRequest(t, handler, http.MethodGet, row.list+"?"+query, nil, "alice")
			if answered.Code != http.StatusOK {
				t.Errorf("GET %s?%s answered %d %s, want 200", row.list, query, answered.Code, answered.Body.String())
				continue
			}
			got := decodeBody[page[map[string]json.RawMessage]](t, answered)
			if got.Limit != limit {
				t.Errorf("GET %s?%s read a page of %d, want %d", row.list, query, got.Limit, limit)
			}
			if row.rows > 0 && (len(got.Items) != min(limit, row.rows) || got.Total != row.rows) {
				t.Errorf("GET %s?%s read %d rows of %d, want %d of %d", row.list, query, len(got.Items), got.Total, min(limit, row.rows), row.rows)
			}
			if row.field == "" {
				continue
			}
			named := strings.Contains(query, "include="+row.include)
			for _, item := range got.Items {
				member, held := item[row.field]
				held = held && string(member) != "null"
				if held != named {
					t.Errorf("GET %s?%s answered a row whose %s is %s, want one only with include=%s", row.list, query, row.field, member, row.include)
					break
				}
				var text string
				if held && row.holds != "" && (json.Unmarshal(member, &text) != nil || text != row.holds) {
					t.Errorf("GET %s?%s answered the %s %s, want %q, the one recorded", row.list, query, row.field, member, row.holds)
				}
			}
		}
	}
}

// TestAV2UploadWhoseAnswerCannotBeReadStoresNothing: a /api/v2 upload reads the row it answers on
// its own transaction before it commits, so a read that fails rolls the upload back - answered
// 500, with no version stored and no event appended - and a retry under the same Idempotency-Key
// runs the upload afresh and stores the version once. A trigger makes the read fail: once a
// version is written it leaves the artifact's row with no actor for its author, which no read the
// upload makes before its answer decodes, as the /api/v1 upload, which reads nothing back, shows.
func TestAV2UploadWhoseAnswerCannotBeReadStoresNothing(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	v1Uploads := "/api/v1/issues/" + issue.Key + "/artifacts"
	first := multipartRequest(t, handler, v1Uploads, map[string]string{"name": "log.txt"}, "log.txt", "text/plain", []byte("The log's first day.\n"), "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("upload the log: %d %s", first.Code, first.Body.String())
	}
	log := decodeBody[uploadedArtifact](t, first).Artifact.ID
	upload := func(t *testing.T, key string) *httptest.ResponseRecorder {
		t.Helper()
		body, bodyType, err := apitest.MultipartUpload(map[string]string{"name": "log.txt"}, "log.txt", "text/plain", []byte("The log's second day.\n"))
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v2/issues/"+issue.Key+"/artifacts", body)
		request.Header.Set("Content-Type", bodyType)
		request.Header.Set("X-Dispatch-User", "alice")
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	stored := func(t *testing.T) (versions, events int) {
		t.Helper()
		if err := database.Pool.QueryRow(t.Context(), `
			select (select count(*) from artifact_versions where artifact_id = $1),
			       (select count(*) from events where type = 'artifact.version' and payload->>'artifact_id' = $1::text)
		`, log).Scan(&versions, &events); err != nil {
			t.Fatalf("count the log's versions and version events: %v", err)
		}
		return versions, events
	}
	seedExec(t, database, "a trigger leaving the artifact's author unreadable once a version is written", `
		create function dispatch_test_unreadable_author() returns trigger language plpgsql as $$
		begin
			update artifacts set created_by = '[]'::jsonb where id = new.artifact_id;
			return null;
		end $$;
		create trigger dispatch_test_unreadable_author after insert on artifact_versions
		for each row execute function dispatch_test_unreadable_author();`)

	key := uuid.NewString()
	if failed := upload(t, key); failed.Code != http.StatusInternalServerError {
		t.Fatalf("the upload whose answer cannot be read: %d %s, want 500", failed.Code, failed.Body.String())
	}
	if versions, events := stored(t); versions != 1 || events != 0 {
		t.Fatalf("the failed upload left the log %d versions and %d version events, want 1 and 0", versions, events)
	}
	if control := multipartRequest(t, handler, v1Uploads, map[string]string{"name": "other.txt"}, "other.txt", "text/plain",
		[]byte("Another log.\n"), "alice"); control.Code != http.StatusCreated {
		t.Fatalf("the /api/v1 upload, which reads nothing back, under the same trigger: %d %s, want 201", control.Code, control.Body.String())
	}

	seedExec(t, database, "the trigger dropped", `drop trigger dispatch_test_unreadable_author on artifact_versions`)
	retried := upload(t, key)
	if retried.Code != http.StatusCreated || retried.Header().Get(contracts.IdempotencyReplayedHeader) != "" {
		t.Fatalf("the retry under the failed upload's key: %d (replayed %q) %s, want the upload run afresh, 201",
			retried.Code, retried.Header().Get(contracts.IdempotencyReplayedHeader), retried.Body.String())
	}
	answered := decodeBody[struct {
		Artifact map[string]json.RawMessage `json:"artifact"`
		Version  model.Version              `json:"version"`
	}](t, retried)
	if facts := newestVersionFactsOf(t, answered.Artifact); answered.Version.Number != 2 || facts.Number == nil || *facts.Number != 2 || *facts.Count != 2 {
		t.Fatalf("the retry answers version %d and the row %v, want the log's second version", answered.Version.Number, answered.Artifact)
	}
	if versions, events := stored(t); versions != 2 || events != 1 {
		t.Fatalf("the retry left the log %d versions and %d version events, want 2 and 1", versions, events)
	}
}
