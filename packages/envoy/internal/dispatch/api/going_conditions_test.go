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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// goingConditionFixtures are the items the condition tests write: an issue with its notes, an ask
// on it, a project document, a file of the planner session's, an issue alice holds, one nobody
// holds, a thread alice resolved and an issue message of hers.
type goingConditionFixtures struct {
	issue, ask, claimed, unclaimed, comment, message string
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
	f.message = createIssueMessage(t, rig.mux, f.issue, map[string]any{"body": "A message to edit."}, "alice").ID
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
// each of the two issues, whether the thread is resolved, and the thread's and the message's
// bodies. A refused write leaves it as it was.
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
			(select resolved::text from comments where id = $5::uuid),
			(select body from comments where id = $5::uuid),
			(select body from messages where id = $6::uuid))`,
		f.issue, f.ask, f.claimed, f.unclaimed, f.comment, f.message).Scan(&state); err != nil {
		t.Fatalf("read what the items hold: %v", err)
	}
	return state
}

// goingConditionWrites is one write of each route that takes a condition, by name: the issue
// update, the ask edit, the issue's, the project's and a session's upload, the claim of an issue
// nobody holds and of one alice holds, the release, the reopen, the resolve, and the edits of a
// comment and of an issue message. The issue's and the project's uploads replace a document, so
// each names the token of the version it replaces, read off handler when it is sent, as the
// command bar's upload names it (main's upload precondition); each is there naming no token too,
// since the token a write back names is a condition its route decides. Each edit of a post
// writes a body no send before it wrote (goingConditionEditBody), so a write a row should have
// refused cannot leave the post holding the very body a later row's write then writes, which
// would read as that write changing nothing.
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
	"issue upload naming no token": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingJSONSend(t, http.MethodPost, "/api/v1/issues/"+f.issue+"/artifacts",
			map[string]any{"name": "notes.md", "content": "# Notes\n\nWritten in process, naming no token.\n"})
	},
	"project upload naming no token": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingJSONSend(t, http.MethodPost, "/api/v1/projects/DEMO/artifacts",
			map[string]any{"name": "guide.md", "content": "# Guide\n\nWritten in process, naming no token.\n"})
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
	"comment edit": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingJSONSend(t, http.MethodPatch, "/api/v1/comments/"+f.comment, map[string]any{"body": goingConditionEditBody()})
	},
	"message edit": func(t *testing.T, _ http.Handler, f goingConditionFixtures) goingSend {
		return goingJSONSend(t, http.MethodPatch, "/api/v1/messages/"+f.message, map[string]any{"body": goingConditionEditBody()})
	},
}

// goingConditionEdits counts the post edits goingConditionWrites built, across the tests that run
// at once.
var goingConditionEdits atomic.Int64

// goingConditionEditBody is the body of one post edit of goingConditionWrites, which no edit
// before it wrote.
func goingConditionEditBody() string {
	return fmt.Sprintf("Written in process, edit %d.", goingConditionEdits.Add(1))
}

// TestGoingConditionOnTheRequestsContextIsDecidedByItsRoute (deep review round 5, finding C): each
// route that takes a compare-and-set decides the one the command bar's in-process caller carries
// on the request's context, under the lock it takes for the write, as it decided the body member
// that carried it before: a condition the item no longer meets is refused, 409 with the route's
// own code, and nothing is written. These are the conditions TestAnOutsideRequestNamingAnOld
// ConditionMemberIsNotConditional sends in a body, which its routes take unconditioned, and the
// document an undo's write back names beside its token (ExpectArtifact), here one the name does
// not find.
func TestGoingConditionOnTheRequestsContextIsDecidedByItsRoute(t *testing.T) {
	t.Parallel()
	rig := newGoingWritesRig(t)
	f := newGoingConditionFixtures(t, rig)
	never, bob, other := "2000-01-01T00:00:00Z", &model.Actor{Kind: "user", ID: "bob"}, "Not the body"
	for _, tc := range []struct {
		write, code string
		condition   goingCondition
	}{
		{"issue update", "ISSUE_CHANGED", goingCondition{Expect: map[string]json.RawMessage{"title": json.RawMessage(`"Not the title"`)}}},
		{"ask edit", "ASK_CHANGED", goingCondition{Expect: map[string]json.RawMessage{"question": json.RawMessage(`"Not the question"`)}}},
		{"issue upload", "DOCUMENT_CHANGED", goingCondition{ExpectArtifact: "00000000-0000-0000-0000-000000000000"}},
		{"issue upload", "DOCUMENT_EXISTS", goingCondition{ExpectNew: true}},
		{"project upload", "DOCUMENT_EXISTS", goingCondition{ExpectNew: true}},
		{"session upload", "DOCUMENT_EXISTS", goingCondition{ExpectNew: true}},
		{"claim of a held issue", codeClaimHolderChanged, goingCondition{ExpectUnclaimed: true}},
		{"release", codeClaimHolderChanged, goingCondition{ExpectHolder: bob}},
		{"reopen", codeCommentChanged, goingCondition{ExpectResolvedAt: &never}},
		{"resolve", codeCommentChanged, goingCondition{ExpectOpen: true}},
		{"comment edit", codeCommentChanged, goingCondition{ExpectBody: &other}},
		{"message edit", codeMessageChanged, goingCondition{ExpectBody: &other}},
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

// TestACommandBarUploadOverAFileWrittenSinceItsReadIsRefused: the command bar's upload over a file
// its read found carries that file at the version the read found (goingCondition.ExpectVersion),
// which the upload route decides under the owner's lock: a version another writer wrote since, or
// another artifact under the name, refuses it, 409 DOCUMENT_CHANGED, and nothing is written; at
// the file's latest version it lands as the next version.
func TestACommandBarUploadOverAFileWrittenSinceItsReadIsRefused(t *testing.T) {
	t.Parallel()
	rig := newGoingWritesRig(t)
	target := "/api/v1/agents/" + goingPlanner + "/artifacts"
	status, body := goingMultipartUpload(t, rig.mux, target, map[string]string{"name": "plan-notes.txt"}, "The first words.")
	if status != http.StatusCreated {
		t.Fatalf("upload the planner's file: %d %s", status, body)
	}
	var uploaded uploadedArtifact
	if err := json.Unmarshal([]byte(body), &uploaded); err != nil {
		t.Fatalf("read the upload's answer %s: %v", body, err)
	}
	file := uploaded.Artifact.ID
	if status, body := goingMultipartUpload(t, rig.mux, target, map[string]string{"name": "plan-notes.txt"}, "Another writer's words."); status != http.StatusCreated {
		t.Fatalf("write the file's second version: %d %s", status, body)
	}
	const versions = `select count(*) from artifact_versions where artifact_id = $1::uuid`
	for _, stale := range []goingArtifactVersion{{Artifact: file, Version: 1}, {Artifact: "00000000-0000-0000-0000-000000000000", Version: 2}} {
		status, answer := goingConditioned(t, rig.mux, "alice", goingMultipartSend(t, target, map[string]string{"name": "plan-notes.txt"}, "The bar's words."),
			goingCondition{ExpectVersion: &stale})
		if status != http.StatusConflict || goingAnswerCode(t, answer) != "DOCUMENT_CHANGED" {
			t.Fatalf("the upload expecting %+v answered %d %s, want 409 DOCUMENT_CHANGED", stale, status, answer)
		}
		if count := rigCount(t, rig, versions, file); count != 2 {
			t.Fatalf("after the upload expecting %+v the file holds %d versions, want its 2", stale, count)
		}
	}
	status, answer := goingConditioned(t, rig.mux, "alice", goingMultipartSend(t, target, map[string]string{"name": "plan-notes.txt"}, "The bar's words."),
		goingCondition{ExpectVersion: &goingArtifactVersion{Artifact: file, Version: 2}})
	if status != http.StatusCreated || rigCount(t, rig, versions, file) != 3 {
		t.Fatalf("the upload expecting the file's latest version answered %d %s, want 201 and its version 3", status, answer)
	}
}

// rigCount is the one number query answers over rig's database.
func rigCount(t *testing.T, rig goingWritesRig, query string, arguments ...any) int {
	t.Helper()
	var count int
	if err := rig.database.Pool.QueryRow(context.Background(), query, arguments...).Scan(&count); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return count
}

// TestACommandBarUploadThatSaysWhatItReplacesIsNoUnguardedReplacement: the count of uploads that
// replaced a document unchecked (noteUnguardedReplacement) leaves out the command bar's own, which
// say what they replace: an undo's write back, which names the document by its id
// (goingCondition.ExpectArtifact) beside the token of the version it replaces (main's upload
// precondition), and a plan row's upload over a document with no token, which expects its version
// (ExpectVersion), a condition the route decides under its lock. Each is taken, and the counter
// reads none.
func TestACommandBarUploadThatSaysWhatItReplacesIsNoUnguardedReplacement(t *testing.T) {
	t.Parallel()
	registry := metrics.New()
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour, metrics: registry})
	issue := createArtifactIssue(t, handler)
	artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nbefore\n")
	target := "/api/v1/issues/" + issue.Key + "/artifacts"
	status, answer := goingConditioned(t, handler, "alice", goingJSONSend(t, http.MethodPost, target,
		map[string]any{"name": "notes.md", "content": "# Notes\n\nwritten back\n", "precondition": uploadBase(t, handler, artifactID)}),
		goingCondition{WriteBack: true, ExpectArtifact: artifactID})
	if status != http.StatusCreated {
		t.Fatalf("the write back naming the token the document carries answered %d %s, want 201", status, answer)
	}
	if sample := unguardedReplacementSample(t, registry); sample != "" {
		t.Fatalf("after the write back the counter reads %q, want no replacement counted: it said what it replaced", sample)
	}
	status, answer = goingConditioned(t, handler, "alice", goingJSONSend(t, http.MethodPost, target,
		map[string]any{"name": "notes.md", "content": "# Notes\n\nthe plan's\n"}), goingCondition{ExpectVersion: &goingArtifactVersion{Artifact: artifactID, Version: 2}})
	if status != http.StatusCreated {
		t.Fatalf("the upload expecting the document's latest version answered %d %s, want 201", status, answer)
	}
	if sample := unguardedReplacementSample(t, registry); sample != "" {
		t.Fatalf("after the upload expecting the version the counter reads %q, want no replacement counted: it said what it replaced", sample)
	}
}

// TestACommandBarWriteBackNeverWritesOverADocumentMadeUnderTheSlugOfTheOneItRead: an undo's write
// back names the document it read by its id (ExpectArtifact of goingCondition), which the upload
// route decides under the owner's lock, beside the token its read returned, which the write checks.
// Where that document was removed and another made under its slug since the read - holding the
// very content read, as two documents made by typing the same title do - the write back is
// refused, 409 DOCUMENT_CHANGED, though it names the other document's own token, which alone would
// let it through, and the other document keeps its one empty version. Naming the document the
// name finds, the same write back lands. An expected document with no token is refused, 400
// ARTIFACT_INPUT, with nothing written.
func TestACommandBarWriteBackNeverWritesOverADocumentMadeUnderTheSlugOfTheOneItRead(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createArtifactIssue(t, handler)
	read := emptyDocument(t, handler, issue.Key, "notes.md")
	held := readDocumentPrecondition(t, handler, read.ID).Markdown
	removedDocument(t, handler, read.ID)
	made := emptyDocument(t, handler, issue.Key, "notes.md")
	if made.Slug != read.Slug || made.ID == read.ID || readDocumentPrecondition(t, handler, made.ID).Markdown != held {
		t.Fatalf("the document made after the removal is %s at slug %s, want another document at the removed one's slug %s holding its content", made.ID, made.Slug, read.Slug)
	}
	target := "/api/v1/issues/" + issue.Key + "/artifacts"
	// writeBack sends the write back carrying condition, its body naming token where token is not nil.
	writeBack := func(condition goingCondition, token map[string]string) (int, string) {
		body := map[string]any{"name": read.Slug, "content": "# Notes\n\nWritten back.\n"}
		if token != nil {
			body["precondition"] = token
		}
		return goingConditioned(t, handler, "alice", goingJSONSend(t, http.MethodPost, target, body), condition)
	}
	// untouched fails t unless the made document still holds its one empty version.
	untouched := func(t *testing.T, after string) {
		t.Helper()
		if versions, text := len(readDocument(t, handler, made.ID).Versions), readDocumentPrecondition(t, handler, made.ID).Markdown; versions != 1 || text != held {
			t.Fatalf("after %s the document made under the slug holds %d versions reading %q, want its one version reading %q", after, versions, text, held)
		}
	}
	token := uploadBase(t, handler, made.ID)
	status, answer := writeBack(goingCondition{WriteBack: true, ExpectArtifact: read.ID}, token)
	if status != http.StatusConflict || goingAnswerCode(t, answer) != "DOCUMENT_CHANGED" {
		t.Fatalf("the write back naming the removed document answered %d %s, want 409 DOCUMENT_CHANGED", status, answer)
	}
	untouched(t, "the write back naming the removed document")
	status, answer = writeBack(goingCondition{WriteBack: true, ExpectArtifact: made.ID}, nil)
	if status != http.StatusBadRequest || goingAnswerCode(t, answer) != "ARTIFACT_INPUT" {
		t.Fatalf("the write back naming a document and no token answered %d %s, want 400 ARTIFACT_INPUT", status, answer)
	}
	untouched(t, "the write back naming a document and no token")
	status, answer = writeBack(goingCondition{WriteBack: true, ExpectArtifact: made.ID}, token)
	if versions := len(readDocument(t, handler, made.ID).Versions); status != http.StatusCreated || versions != 2 {
		t.Fatalf("the write back naming the document the name finds answered %d %s and left %d versions, want 201 and its version 2", status, answer, versions)
	}
}

// TestACommandBarUploadOverAProjectDocumentSeesAVersionWrittenUnderItsOwnersLock: a project
// document is its own owner, whose lock the document service's version writers hold (an edit's
// snapshot here, as named versions, settlements and rebuilds do) and which the upload takes only
// after the project's upload lock and its read of the document. A version written under that lock
// while the command bar's upload waits for it is seen: the upload expecting the version it read is
// refused, 409 DOCUMENT_CHANGED, and the document keeps that writer's text and its two versions.
func TestACommandBarUploadOverAProjectDocumentSeesAVersionWrittenUnderItsOwnersLock(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createArtifactIssue(t, handler)
	guide := uploadDocument(t, handler, "/api/v1/projects/TEST", "guide.md", "# Guide\n\nRead this first.\n")
	ctx := context.Background()
	hold, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if err := store.LockOwner(ctx, hold, store.ArtifactOwner(guide.Artifact.ID)); err != nil {
		t.Fatalf("hold the document's owner lock: %v", err)
	}
	// The command bar's upload, expecting the version 1 it read, is sent while the lock is held.
	spec := goingJSONSend(t, http.MethodPost, "/api/v1/projects/TEST/artifacts", map[string]any{"name": "guide.md", "content": "# Guide\n\nThe bar's.\n"})
	spec.condition = &goingCondition{ExpectVersion: &goingArtifactVersion{Artifact: guide.Artifact.ID, Version: 1}}
	request, err := spec.build(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	person := model.Actor{Kind: "user", ID: "alice", Credential: personCredential, Proven: true}
	doer := (&server{inProcess: handler}).goingDoer(goingBarCaller(person, true, goingTestRun), testServerURL)
	type answered struct {
		status int
		body   string
		err    error
	}
	sent := make(chan answered, 1)
	go func() {
		response, err := doer.Do(request)
		if err != nil {
			sent <- answered{err: err}
			return
		}
		defer response.Body.Close()
		var body bytes.Buffer
		_, _ = body.ReadFrom(response.Body)
		sent <- answered{status: response.StatusCode, body: body.String()}
	}()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if waiting, _ := storetest.OwnerLockWaiters(t, database); waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the upload never came to wait on the document's owner lock")
		}
	}
	// Bob's edit writes version 2 under the held lock, as the document service writes a version.
	joined, ledger := deps.Docs.Join(ctx, hold)
	defer ledger.Discard()
	if _, err := deps.Docs.ApplyOps(joined, guide.Artifact.ID, []model.EditOp{{Op: "replace", Find: "Read this first.", With: "Bob's."}},
		model.Actor{Kind: "user", ID: "bob"}, nil); err != nil {
		t.Fatalf("edit the document under its owner lock: %v", err)
	}
	if _, err := deps.Docs.SnapshotVersion(joined, guide.Artifact.ID, model.Actor{Kind: "user", ID: "bob"}); err != nil {
		t.Fatalf("write the edit's version under the owner lock: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the edit's version: %v", err)
	}
	var answer answered
	select {
	case answer = <-sent:
	case <-time.After(30 * time.Second):
		t.Fatal("the upload did not answer once the owner lock was released")
	}
	if answer.err != nil || answer.status != http.StatusConflict || goingAnswerCode(t, answer.body) != "DOCUMENT_CHANGED" {
		t.Fatalf("the upload expecting version 1 answered %d %s (%v), want 409 DOCUMENT_CHANGED", answer.status, answer.body, answer.err)
	}
	if versions := uploadedVersionCount(t, database, guide.Artifact.ID); versions != 2 {
		t.Fatalf("the document holds %d versions, want its 2: the refused upload wrote one", versions)
	}
	if text := readDocumentPrecondition(t, handler, guide.Artifact.ID).Markdown; !strings.Contains(text, "Bob's.") {
		t.Fatalf("the document reads %q, want Bob's text kept", text)
	}
}

// TestGoingWriteBackWithoutItsConditionIsRefusedWithNothingWritten (deep review round 5, finding C,
// the architect's rule): a request the command bar's undo sends says it requires its condition
// (goingCondition.WriteBack), so a write back whose condition a change lost is never made
// unconditioned: each route that takes a condition refuses one marked as a write back that
// carries no condition the route decides - none at all, an empty one, one that leaves a field it
// writes unexpected, or an upload naming no token of the version it replaces - 500
// GOING_CONDITION_MISSING, and writes nothing. The same write carrying no mark is made, as every
// caller but the undo makes it.
func TestGoingWriteBackWithoutItsConditionIsRefusedWithNothingWritten(t *testing.T) {
	t.Parallel()
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
		{"an issue upload naming no token, with no condition", "issue upload naming no token", goingCondition{WriteBack: true}},
		{"a project upload naming no token, with no condition", "project upload naming no token", goingCondition{WriteBack: true}},
		{"a session upload with no condition", "session upload", goingCondition{WriteBack: true}},
		{"a claim with no condition", "claim", goingCondition{WriteBack: true}},
		{"a release with no condition", "release", goingCondition{WriteBack: true}},
		{"a release expecting a holder of no kind and no id", "release", goingCondition{WriteBack: true, ExpectHolder: &model.Actor{}}},
		{"a reopen with no condition", "reopen", goingCondition{WriteBack: true}},
		{"a reopen expecting an empty moment", "reopen", goingCondition{WriteBack: true, ExpectResolvedAt: &empty}},
		{"a resolve with no condition", "resolve", goingCondition{WriteBack: true}},
		{"a comment edit with no condition", "comment edit", goingCondition{WriteBack: true}},
		{"a message edit with no condition", "message edit", goingCondition{WriteBack: true}},
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
	for _, write := range []string{"issue update", "ask edit", "issue upload", "issue upload naming no token", "project upload", "project upload naming no token",
		"session upload", "claim", "release", "reopen", "resolve", "comment edit", "message edit"} {
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
// body off the network that names one under its old member - expect, expect_new (JSON and
// multipart), expect_unclaimed, expect_holder, expect_resolved_at - is answered as one that names
// nothing, the member ignored as main's decodeJSON ignores any member a route does not declare:
// each condition below is one its route would refuse, and each write goes through. Each JSON
// upload replaces a document, so it names the token of the version it replaces, as main's upload
// precondition asks of every caller, and the old member is all that could refuse it.
func TestAnOutsideRequestNamingAnOldConditionMemberIsNotConditional(t *testing.T) {
	t.Parallel()
	rig := newGoingWritesRig(t)
	f := newGoingConditionFixtures(t, rig)
	owner := "/api/v1/issues/" + f.issue
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
		{"an issue upload's expect_new", http.MethodPost, owner + "/artifacts",
			map[string]any{"name": "notes.md", "content": "# Notes\n\nThe second line.\n", "expect_new": true},
			func(t *testing.T, answer string) bool { return strings.Contains(answer, `"previous_version":1`) }},
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
