//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go. This file's test reads the
// base release the Verification document's preamble builds once per head (baseRelease), and is
// run as
//
//	go test -tags basebuild -timeout 30m ./cmd/dispatch -run '^TestAnApprovalCarriesAcrossTheRemovalOfTheServerOwnedAskAttributes$' -count=1 -v

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest/newerversion"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// askOpener is the opening line of an ask block, at any depth and under any quote or list prefix.
var askOpener = regexp.MustCompile(`(?m)^[> \t-]*:{3,}ask\{[^\n]*$`)

// askServerAttribute is one of the eight server-owned attributes of an ask block as its opening
// line writes it: a value holds no raw quote, which the renderer writes as a reference.
var askServerAttribute = regexp.MustCompile(` (state|answered_by|answered_at|selected|answer|invalid|copied_from|copied_from_document)="[^"]*"`)

// withoutAskServerAttributes is markdown with the eight server-owned attributes taken off every
// ask block's opening line, and every other byte as it is.
func withoutAskServerAttributes(markdown string) string {
	return askOpener.ReplaceAllStringFunc(markdown, func(line string) string {
		return askServerAttribute.ReplaceAllString(line, "")
	})
}

// removalDocument is a document whose ask blocks the base release writes its attributes into: a
// paragraph of prose, an ask a person answers and an ask still open, each under ids of its own.
func removalDocument(name string) string {
	return "# " + name + "\n\nThe service ships behind a flag.\n\n" +
		":::ask{#decided-" + name + " urgency=\"high\" multiple=\"false\"}\nShip " + name + " first?\n\n- Ship: Now.\n- Hold: Later.\n:::\n\n" +
		":::ask{#pending-" + name + " urgency=\"low\" multiple=\"false\"}\nWhich region takes " + name + "?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
}

// declinedRemovalAsk is an ask block of the first or the second document, name, that the person
// declines through the base release before approving the document, so its ask is resolved and
// every version written once the fleet is one build keeps its state (requireKeptForm).
func declinedRemovalAsk(name string) string {
	return ":::ask{#declined-" + name + " urgency=\"low\" multiple=\"false\"}\nShould " + name + " wait for the census?\n\n- Wait: Until it runs.\n- Go: Now.\n:::\n"
}

// uploadDocument uploads content under name to the issue through p, as the test's person, and
// answers the answer: a new document, or a new version of the issue's document of that name, which
// names the token of the version it replaces.
func uploadDocument(t *testing.T, p *dispatchProcess, issue, name, content string) httpAnswer {
	t.Helper()
	artifacts := "/api/v1/issues/" + issue + "/artifacts"
	body := map[string]any{"name": name, "content": content}
	if base := p.uploadBase(t, artifacts, name); base != nil {
		body["precondition"] = base
	}
	answer := p.send(t, http.MethodPost, artifacts, encodeJSON(t, body), asUser(dispatchTestLogin))
	if answer.status != http.StatusCreated && answer.status != http.StatusOK {
		t.Fatalf("upload %s to %s: %s", name, issue, answer)
	}
	return answer
}

// uploadedID is the id of the artifact an upload answered.
func uploadedID(t *testing.T, answer httpAnswer) string {
	t.Helper()
	var uploaded struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(answer.body, &uploaded); err != nil || uploaded.Artifact.ID == "" {
		t.Fatalf("read the upload back: %v: %s", err, answer)
	}
	return uploaded.Artifact.ID
}

// agentBearer is the header the shared agent bearer's request carries; the body names the session
// it writes as.
func agentBearer() http.Header {
	return http.Header{"Authorization": {"Bearer " + dispatchTestAgentToken}}
}

// restoreSession is the agent's session whose writes move the answered asks out of the specs.
const restoreSession = "ses_restore_approvals_agent"

// asAgentSession sends body, a write the agent's session makes through the shared agent bearer,
// naming that session as its actor, to p, and fails unless p answers status.
func asAgentSession(t *testing.T, p *dispatchProcess, method, path string, body map[string]any, status int) httpAnswer {
	t.Helper()
	body["actor"] = map[string]string{"kind": "session", "id": restoreSession}
	answer := p.send(t, method, path, encodeJSON(t, body), agentBearer())
	if answer.status != status {
		t.Fatalf("the agent's %s %s: %s, want %d", method, path, answer, status)
	}
	return answer
}

// agentUpload uploads content as the document name of issue, as the agent's session, and answers
// its id.
func agentUpload(t *testing.T, p *dispatchProcess, issue, name, content string) string {
	t.Helper()
	answer := asAgentSession(t, p, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]any{"name": name, "content": content}, http.StatusCreated)
	return uploadedID(t, answer)
}

// documentName is the name the document is stored under, which an upload of a new version names.
func documentName(t *testing.T, p *dispatchProcess, artifactID string) string {
	t.Helper()
	var artifact struct {
		Name string `json:"name"`
	}
	readJSON(t, p, "/api/v1/artifacts/"+artifactID, &artifact)
	return artifact.Name
}

// newestReview is the document's newest review as GET /api/v1/artifacts/{id}/reviews lists it.
type newestReview struct {
	Version int     `json:"version"`
	State   string  `json:"state"`
	Reason  *string `json:"reason"`
}

func newestReviewOf(t *testing.T, p *dispatchProcess, artifactID string) newestReview {
	t.Helper()
	var reviews []newestReview
	readJSON(t, p, "/api/v1/artifacts/"+artifactID+"/reviews", &reviews)
	if len(reviews) == 0 {
		t.Fatalf("document %s has no review", artifactID)
	}
	return reviews[0]
}

// approvedDocument is one document the base release wrote and a person approved: where it is, the
// version approved and the markdown and review captured at that version.
type approvedDocument struct {
	name, issue, id string
	version         int
	markdown        string
	review          string
}

// approveAndCapture has the person approve the document at its latest version through p and
// captures what was approved.
func approveAndCapture(t *testing.T, ctx context.Context, p *dispatchProcess, database *pgx.Conn, issue, artifactID, label string) approvedDocument {
	t.Helper()
	settledDocument(t, ctx, p, database, artifactID)
	review := p.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews", `{"state":"approved"}`, http.StatusCreated)
	number, markdown := latestMarkdown(t, p, artifactID)
	return approvedDocument{name: label, issue: issue, id: artifactID, version: number, markdown: markdown, review: string(review)}
}

// reviewHash is a review and the digest it holds of the bytes it approved, content_sha256: the
// insert's trigger takes it of the version's stored markdown (artifact_version_text_sha256,
// migration 20261105084017), and no statement moves it after.
type reviewHash struct {
	review, hash string
}

// reviewHashOf is the review the person gave of document, read off the answer captured with it, and
// the digest that review holds now.
func reviewHashOf(t *testing.T, ctx context.Context, database *pgx.Conn, document approvedDocument) reviewHash {
	t.Helper()
	var answered struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(document.review), &answered); err != nil || answered.ID == "" {
		t.Fatalf("read the review of %s back: %v: %s", document.name, err, document.review)
	}
	held := reviewHash{review: answered.ID}
	if err := database.QueryRow(ctx, `select coalesce(content_sha256, '') from artifact_reviews where id = $1::uuid`, held.review).Scan(&held.hash); err != nil {
		t.Fatalf("read the hash of the review %s of %s: %v", held.review, document.name, err)
	}
	return held
}

// textHash is the sha256 of markdown's UTF-8 bytes in hex, as artifact_version_text_sha256 takes it
// of a stored version.
func textHash(markdown string) string {
	sum := sha256.Sum256([]byte(markdown))
	return hex.EncodeToString(sum[:])
}

// versionHashOf is the digest the store takes now of the bytes the version number of document holds
// (artifact_version_text_sha256), and the hash its newest review there holds (docs.ReviewOrder).
func versionHashOf(t *testing.T, ctx context.Context, database *pgx.Conn, document string, number int) (stored, reviewed string) {
	t.Helper()
	if err := database.QueryRow(ctx, `
		select coalesce(artifact_version_text_sha256($1::uuid, $2), ''),
		       coalesce((select content_sha256 from artifact_reviews where artifact_id = $1::uuid and version = $2
		                 order by `+docs.ReviewOrder+` limit 1), '')
	`, document, number).Scan(&stored, &reviewed); err != nil {
		t.Fatalf("read the hashes of version %d of %s: %v", number, document, err)
	}
	return stored, reviewed
}

// One version whose only change is in the server-owned ask attributes carries a person's
// approval, and nothing wider does (c-attribute-removal). The base release writes the documents, a
// person approves them and an agent requests approval of one; this head writes a document holding
// a reading; the base release serves again beside this head and leaves that document be; while
// both serve, the roll window keeps the attributes; once the base release is gone, the first write
// to each document cuts them to the form R5 keeps and carries its approval, an answer keeps an
// approval request's turn, a copy of an open ask keeps every carry and every turn, and every wider
// write supersedes.
func TestAnApprovalCarriesAcrossTheRemovalOfTheServerOwnedAskAttributes(t *testing.T) {
	ctx := context.Background()
	databaseURL := censusDatabase(t)
	base := baseRelease(t)
	head := cmdtest.BuildStamped(t, "envoy-dispatch", headBuildStamp)
	database, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the run's database: %v", err)
	}
	defer database.Close(ctx)

	// The base release writes the documents, and a person approves each.
	previous := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: base, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay})
	previous.waitHealthy(t)
	baseCommit := announcedBaseCommit(t, ctx, database)
	previous.call(t, http.MethodPost, "/api/v1/projects", `{"key":"RMV","name":"The removal"}`, http.StatusCreated)
	issue, first := createCensusIssue(t, previous, "RMV", "The removal", removalDocument("one")+"\n"+declinedRemovalAsk("one"))
	sourceDocument := uploadedID(t, uploadDocument(t, previous, issue, "source.md", "# Source\n\n:::ask{#shared urgency=\"med\" multiple=\"false\"}\nShare the source?\n\n- Share: Now.\n- Keep: Later.\n:::\n"))
	blockAskOf(t, previous, issue, "shared")
	previous.call(t, http.MethodPost, "/api/v1/artifacts/"+first+"/edits", encodeJSON(t, map[string]any{"ops": []map[string]string{{
		"op": "insert", "after": "end", "markdown": ":::ask{#shared urgency=\"med\" multiple=\"false\"}\nShare the source?\n\n- Share: Now.\n- Keep: Later.\n:::\n",
	}}}), http.StatusOK)
	answerAsk(t, previous, blockAskOf(t, previous, issue, "decided-one"), "Ship", "Ship one now.")
	second := uploadedID(t, uploadDocument(t, previous, issue, "lonely.md", "# Lonely\n\nA line of prose.\n\n"+lonelyAsk+"\n"+declinedRemovalAsk("two")))
	lonelyAskID := blockAskOf(t, previous, issue, "lonely")
	// The person declines one ask of each of the first two documents, as a person resolves one,
	// and the base release settles that state into its block before the person approves.
	for _, name := range []string{"one", "two"} {
		previous.call(t, http.MethodPost, "/api/v1/asks/"+blockAskOf(t, previous, issue, "declined-"+name)+"/decline", `{"note":"Not before the census."}`, http.StatusOK)
	}
	// The fifth and the sixth document: two open asks of each one's own and a copy of the second
	// document's open ask, which the base release settles as a copy, open, naming its source.
	fifth := uploadedID(t, uploadDocument(t, previous, issue, "fifth.md", copyingDocument("fifth")))
	sixth := uploadedID(t, uploadDocument(t, previous, issue, "sixth.md", copyingDocument("sixth")))
	for _, name := range []string{"fifth", "sixth"} {
		blockAskOf(t, previous, issue, name+"-a")
		blockAskOf(t, previous, issue, name+"-b")
	}
	superseding := make([]string, 5)
	for index := range superseding {
		name := fmt.Sprintf("s%d", index+1)
		superseding[index] = uploadedID(t, uploadDocument(t, previous, issue, name+".md", removalDocument(name)))
		answerAsk(t, previous, blockAskOf(t, previous, issue, "decided-"+name), "Ship", "Ship "+name+" now.")
		blockAskOf(t, previous, issue, "pending-"+name)
	}
	// A project document, whose open ask block the base release writes open: the project slug
	// form of the text read reads it.
	projectUpload := previous.send(t, http.MethodPost, "/api/v1/projects/RMV/artifacts", encodeJSON(t, map[string]string{
		"name": "project-notes.md", "content": "# Project notes\n\n:::ask{#project-open urgency=\"med\" multiple=\"false\"}\nIs it open?\n\n- Yes: It is.\n- No: It is not.\n:::\n",
	}), asUser(dispatchTestLogin))
	if projectUpload.status != http.StatusCreated {
		t.Fatalf("upload the project document: %s", projectUpload)
	}
	projectDocument := uploadedID(t, projectUpload)
	settledDocument(t, ctx, previous, database, projectDocument)
	one := approveAndCapture(t, ctx, previous, database, issue, first, "the first document")
	two := approveAndCapture(t, ctx, previous, database, issue, second, "the second document")
	for _, name := range []string{"state", "answered_by", "answered_at", "selected", "answer", "copied_from", "copied_from_document"} {
		if !strings.Contains(one.markdown, " "+name+`="`) {
			t.Fatalf("the base release's first document holds no %s on its ask blocks:\n%s", name, one.markdown)
		}
	}
	if lonely := copyOpenerOf(two.markdown); !strings.Contains(lonely, `state="open"`) || strings.Contains(lonely, "answered") || strings.Contains(lonely, "copied") {
		t.Fatalf("the base release's second document's ask lonely reads %q, want state=\"open\" alone", lonely)
	}
	for _, declined := range []struct{ document, block string }{{one.markdown, "declined-one"}, {two.markdown, "declined-two"}} {
		if opener, _, _ := strings.Cut(askBlockText(declined.document, declined.block), "\n"); !strings.Contains(opener, `state="resolved"`) {
			t.Fatalf("the base release's block %s reads %q once the person declined its ask, want state=\"resolved\"", declined.block, opener)
		}
	}
	captured := make([]approvedDocument, len(superseding))
	for index, id := range superseding {
		captured[index] = approveAndCapture(t, ctx, previous, database, issue, id, fmt.Sprintf("superseding document %d", index+1))
	}
	// The person approves the fifth document; an agent asks the person to approve the sixth, which
	// no approval stands on, so the request waits on the person. Both hold the copy the base
	// release settled open, naming its source.
	five := approveAndCapture(t, ctx, previous, database, issue, fifth, "the fifth document")
	settledDocument(t, ctx, previous, database, sixth)
	sixthBase, sixthBaseText := latestMarkdown(t, previous, sixth)
	for _, read := range []struct{ what, markdown string }{{five.name, five.markdown}, {"the sixth document", sixthBaseText}} {
		if copied := copyOpenerOf(read.markdown); !strings.Contains(copied, `state="open"`) || !strings.Contains(copied, `copied_from="`+lonelyAskID+`"`) || !strings.Contains(copied, ` copied_from_document="`) {
			t.Fatalf("the base release's %s holds the copy %q, want it open, naming its source ask %s and that ask's document", read.what, copied, lonelyAskID)
		}
	}
	sixthRequest := openedRequestID(t, asAgentSession(t, previous, http.MethodPost, "/api/v1/artifacts/"+sixth+"/approval-requests", map[string]any{}, http.StatusCreated))
	if request := approvalRequestOf(t, previous, sixthRequest); !request.waitsOnThePersonAt(sixthBase) {
		t.Fatalf("the sixth document's request reads %s through the base release, want it waiting on the person at version %d", request, sixthBase)
	}
	t.Logf("the base release %s wrote the documents; the person approved the first at version %d and the second at version %d; reviews %s %s",
		baseCommit, one.version, two.version, one.review, two.review)
	// The review's hash, captured before this branch boots: the digest the base release's store
	// took of the bytes the person approved, which is the digest of the markdown captured.
	approvedHashes := map[string]reviewHash{}
	for _, document := range []approvedDocument{one, two} {
		approved := reviewHashOf(t, ctx, database, document)
		if approved.hash == "" || approved.hash != textHash(document.markdown) {
			t.Fatalf("the base release's review %s of %s holds the hash %q, want the sha256 of the markdown the person approved, %s",
				approved.review, document.name, approved.hash, textHash(document.markdown))
		}
		approvedHashes[document.id] = approved
		t.Logf("%s: the person's review %s holds the hash %s of the approved version %d", document.name, approved.review, approved.hash, document.version)
	}
	stopProcess(t, previous)

	// The current build creates and answers the ask. SQL stores the reading as a later build
	// would, because this release cannot introduce reading blocks.
	current := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: head, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay})
	current.waitHealthy(t)
	readingAsk := ":::ask{#read-it urgency=\"med\" multiple=\"false\"}\nRead it?\n\n- Yes: Now.\n- No: Later.\n:::\n"
	third := uploadedID(t, uploadDocument(t, current, issue, "reading.md", "# Reading\n\n"+readingAsk))
	answerAsk(t, current, blockAskOf(t, current, issue, "read-it"), "Yes", "Read it now.")
	settledDocument(t, ctx, current, database, third)
	_, answeredMarkdown := latestMarkdown(t, current, third)
	withReading := strings.Replace(answeredMarkdown, ":::ask{#read-it ", "::::ask{#read-it ", 1)
	withReading = strings.TrimSuffix(withReading, ":::\n") + "\n:::reading{#read-r1}\nThe agent reads it as reading now.\n:::\n::::\n"
	stopProcess(t, current)
	newerversion.StoreNewerVersion(t, database, third, withReading)
	current = startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: head, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay})
	current.waitHealthy(t)
	asAgentSession(t, current, http.MethodPost, "/api/v1/artifacts/"+third+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "Reading", "with": "Reading notes"}},
	}, http.StatusOK)
	settledDocument(t, ctx, current, database, third)
	thirdVersions, thirdText := latestMarkdown(t, current, third)
	if !strings.Contains(thirdText, ":::reading{#read-r1}") {
		t.Fatalf("this head's third document holds no reading:\n%s", thirdText)
	}
	thirdHeld := thirdDocumentHolds(t, ctx, database, third)

	// The base release boots again beside this head, over the database this head migrated.
	again := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: base, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay})
	again.waitHealthy(t)
	var builds []string
	if err := database.QueryRow(ctx, `select coalesce(array_agg(distinct commit order by commit), '{}') from server_processes where heartbeat_at > now() - interval '15 seconds'`).Scan(&builds); err != nil {
		t.Fatalf("read the live builds: %v", err)
	}
	if len(builds) != 2 {
		t.Fatalf("the live processes serve %v, want the base release and this head, both stamped", builds)
	}
	if refused := again.send(t, http.MethodGet, "/api/v1/artifacts/"+third+"/text", "", asUser(dispatchTestLogin)); refused.status != http.StatusConflict || refused.code() != "DOC_NEWER_CONTENT" {
		t.Errorf("the base release answers the third document's text %s, want 409 DOC_NEWER_CONTENT", refused)
	}
	if versions, text := latestMarkdown(t, current, third); versions != thirdVersions || text != thirdText {
		t.Errorf("the third document moved from version %d to %d while the base release served beside this head:\n%s", thirdVersions, versions, text)
	}
	if held := thirdDocumentHolds(t, ctx, database, third); held != thirdHeld {
		t.Errorf("while the base release served beside this head the third document's ask row and reading node moved from %+v to %+v, want them as they were", thirdHeld, held)
	}
	// While both builds are live, an accepted write to the first document keeps the attributes, in
	// step with the asks row: the upload takes them off, and the roll window writes them back.
	uploadDocument(t, current, issue, documentName(t, current, first), one.markdown)
	settledDocument(t, ctx, current, database, first)
	_, rolled := latestMarkdown(t, current, first)
	if withoutAskServerAttributes(rolled) != withoutAskServerAttributes(one.markdown) {
		t.Errorf("while both builds serve the first document reads:\n%s\nwant its text as the person approved it, its attributes aside", rolled)
	}
	var sourceSlug string
	if err := database.QueryRow(ctx, `select slug from artifacts where id = $1::uuid`, sourceDocument).Scan(&sourceSlug); err != nil {
		t.Fatalf("read the source document's slug: %v", err)
	}
	requireBlockInStepWithRow(t, ctx, database, rolled, "decided-one", first, "")
	requireBlockInStepWithRow(t, ctx, database, rolled, "pending-one", first, "")
	requireBlockInStepWithRow(t, ctx, database, rolled, "shared", sourceDocument, "dispatch://"+issue+"/artifact/"+sourceSlug)
	if approval := approvalOf(t, current, first); !approval.approvedLatest() {
		t.Errorf("while both builds serve the first document reads %+v, want its approval carried", approval)
	}
	var rolledReviews []newestReview
	readJSON(t, current, "/api/v1/artifacts/"+first+"/reviews", &rolledReviews)
	t.Logf("while both builds serve the first document's reviews, newest first: %+v", rolledReviews)
	stopProcess(t, again)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := database.QueryRow(ctx, `select coalesce(array_agg(distinct commit order by commit), '{}') from server_processes where heartbeat_at > now() - interval '15 seconds'`).Scan(&builds); err != nil {
			t.Fatalf("read the live builds: %v", err)
		}
		if len(builds) == 1 && builds[0] == headBuildStamp {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the live processes still serve %v once the base release stopped", builds)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// The server's own text read, read with `Dispatch-Asks: 1` as this build's CLI reads it
	// (servedTextOf), once the fleet is one build and before any write of it, while the documents
	// the base release wrote still hold the attributes it and the roll window wrote, live and
	// stored: Verification v43's statements, each read by name.
	slugOf := func(document string) string {
		t.Helper()
		var slug string
		if err := database.QueryRow(ctx, `select slug from artifacts where id = $1::uuid`, document).Scan(&slug); err != nil {
			t.Fatalf("read the slug of %s: %v", document, err)
		}
		return slug
	}
	t.Run("the text read and its two slug forms hold none of the eight attributes whatever the stored bytes hold", func(t *testing.T) {
		for _, read := range []struct {
			what, document string
			paths          []string
		}{
			{"the first document", first, []string{"/api/v1/artifacts/" + first + "/text", "/api/v1/issues/" + issue + "/artifacts/" + slugOf(first) + "/text"}},
			{"the second document", second, []string{"/api/v1/artifacts/" + second + "/text", "/api/v1/issues/" + issue + "/artifacts/" + slugOf(second) + "/text"}},
			{"the project document", projectDocument, []string{"/api/v1/artifacts/" + projectDocument + "/text", "/api/v1/projects/RMV/artifacts/" + slugOf(projectDocument) + "/text"}},
		} {
			_, stored := latestMarkdown(t, current, read.document)
			if !askServerAttribute.MatchString(stored) {
				t.Fatalf("%s's latest stored version holds none of the eight attributes, so its read proves nothing:\n%s", read.what, stored)
			}
			for _, path := range read.paths {
				served := servedTextOf(t, current, path)
				for _, opener := range askOpener.FindAllString(served.Markdown, -1) {
					if askServerAttribute.MatchString(opener) {
						t.Errorf("GET %s serves an ask block of %s with a server-owned attribute: %s", path, read.what, opener)
					}
				}
				if want := withoutAskServerAttributes(stored); served.Markdown != want {
					t.Errorf("GET %s serves %s as\n%s\nwant its stored text without the eight attributes:\n%s", path, read.what, served.Markdown, want)
				}
			}
		}
	})
	t.Run("the asks member answers each ask block from the ask node", func(t *testing.T) {
		served := servedTextOf(t, current, "/api/v1/artifacts/"+first+"/text")
		t.Logf("the first document's asks: %s", encodeJSON(t, served.Asks))
		sourceAsk := blockAskOf(t, current, issue, "shared")
		for _, want := range []struct {
			block, ask, copied string
		}{
			{"decided-one", blockAskOf(t, current, issue, "decided-one"), ""},
			{"pending-one", blockAskOf(t, current, issue, "pending-one"), ""},
			{"shared", sourceAsk, "dispatch://" + issue + "/artifact/" + slugOf(sourceDocument)},
		} {
			entry := textAskOf(served.Asks, want.block)
			var rowState, rowBy string
			var rowWords *string
			if err := database.QueryRow(ctx, `select state, coalesce(answer->>'user', ''), answer->>'text' from asks where id = $1::uuid`, want.ask).Scan(&rowState, &rowBy, &rowWords); err != nil {
				t.Fatalf("read the ask %s: %v", want.ask, err)
			}
			// The asks row is the node's projection; where this head's backfill has stated the
			// node of an ask the base release wrote, its tuples are what the entry must read.
			held := askNodeHolds(t, ctx, database, want.ask)
			nodeState, stated := held[graphstore.TupleAnswerState]
			words, worded := held[graphstore.TupleAnswer]
			if entry == nil || entry.State == "" || entry.State != rowState || entry.AnsweredBy != rowBy ||
				(rowWords == nil) != (entry.Answer == nil) || (rowWords != nil && *rowWords != *entry.Answer) ||
				(stated && nodeState.ToID != entry.State) || (worded && (entry.Answer == nil || words.ToID != *entry.Answer)) {
				t.Errorf("the first document's text answers %s as %s, want the ask %s as its node %v and its row (%s, %q) hold it", want.block, encodeJSON(t, entry), want.ask, held, rowState, rowBy)
				continue
			}
			if want.copied != "" && (entry.CopiedFrom != want.ask || entry.CopiedFromDocument != want.copied) {
				t.Errorf("the first document's text answers the copy %s as %s, want it naming the ask %s on %s", want.block, encodeJSON(t, entry), want.ask, want.copied)
			}
		}
	})
	t.Run("a version's own text is the stored bytes a person approved", func(t *testing.T) {
		for _, document := range []approvedDocument{one, two} {
			var stored string
			if err := database.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1::uuid and number = $2`, document.id, document.version).Scan(&stored); err != nil {
				t.Fatalf("read %s's version %d: %v", document.name, document.version, err)
			}
			var version struct {
				Markdown string `json:"markdown"`
			}
			readJSON(t, current, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", document.id, document.version), &version)
			if version.Markdown != stored || !askServerAttribute.MatchString(stored) {
				t.Errorf("%s's version %d reads\n%s\nwant its stored bytes, which hold the attributes the base release wrote:\n%s", document.name, document.version, version.Markdown, stored)
			}
		}
	})
	// The first accepted write to the first document on one build is the agent's upload of the
	// text dispatch doc-read printed, which the loop below reads as carrying the approval.
	t.Run("through the CLI, doc-read's trailer holds each answer and its text uploads back unchanged", func(t *testing.T) {
		home := t.TempDir()
		read := runCurrentCLI(t, current, home, "doc-read", "--issue", issue)
		cut := strings.Index(read.stdout, "\n\nDocument token: ")
		if read.code != 0 || cut < 0 {
			t.Fatalf("dispatch doc-read --issue %s: %s", issue, read)
		}
		markdown := read.stdout[:cut]
		trailer, _, _ := strings.Cut(read.stdout[cut+len("\n\n"):], "\n\n")
		for _, opener := range askOpener.FindAllString(markdown, -1) {
			if askServerAttribute.MatchString(opener) {
				t.Errorf("dispatch doc-read prints an ask block with a server-owned attribute: %s", opener)
			}
		}
		answered := 0
		for _, entry := range servedTextOf(t, current, "/api/v1/artifacts/"+first+"/text").Asks {
			if entry.State != asks.StateAnswered {
				continue
			}
			answered++
			line := fmt.Sprintf("Answered %s by %s at %s: ", entry.BlockID, entry.AnsweredBy, entry.AnsweredAt)
			if entry.Answer != nil {
				words, _ := json.Marshal(*entry.Answer)
				line += string(words)
			}
			if !strings.Contains(trailer, line) {
				t.Errorf("dispatch doc-read's trailer holds no line opening %q:\n%s", line, trailer)
			}
		}
		if answered == 0 {
			t.Fatalf("the first document answers no answered ask, so its read proves nothing:\n%s", trailer)
		}
		t.Logf("dispatch doc-read's trailer:\n%s", trailer)
		// The upload replaces the version doc-read printed, so it names the token doc-read printed
		// with it: the trailer's first line.
		tokenLine, _, _ := strings.Cut(trailer, "\n")
		uploaded := runCurrentCLI(t, current, home, "artifact", "--issue", issue, "--name", documentName(t, current, first), "--content", markdown, "--force",
			"--precondition-json", encodeJSON(t, map[string]string{"document": strings.TrimPrefix(tokenLine, "Document token: ")}))
		if uploaded.code != 0 {
			t.Fatalf("dispatch artifact --force refused the text dispatch doc-read printed: %s", uploaded)
		}
		settledDocument(t, ctx, current, database, first)
		if served := servedTextOf(t, current, "/api/v1/artifacts/"+first+"/text"); served.Markdown != markdown {
			t.Errorf("the first document after the upload of the text doc-read printed reads\n%s\nwant that text:\n%s", served.Markdown, markdown)
		}
	})

	// The fleet is this head alone: the first accepted write to each of the two documents - for the
	// first, the agent's upload above - cuts the attributes to the form R5 keeps and carries the
	// approval.
	sourceAddress := "dispatch://" + issue + "/artifact/" + sourceSlug
	keptBlocks := map[string][]keptBlock{
		one.id: {{"decided-one", blockAskOf(t, current, issue, "decided-one"), ""}, {"pending-one", blockAskOf(t, current, issue, "pending-one"), ""}, {"shared", blockAskOf(t, current, issue, "shared"), sourceAddress},
			{"declined-one", blockAskOf(t, current, issue, "declined-one"), ""}},
		two.id: {{"lonely", blockAskOf(t, current, issue, "lonely"), ""}, {"declined-two", blockAskOf(t, current, issue, "declined-two"), ""}},
	}
	for _, document := range []approvedDocument{one, two} {
		before := newestReviewOf(t, current, document.id)
		if before.State != "approved" {
			t.Fatalf("%s reads %+v before its first write on one build, want an approval standing", document.name, before)
		}
		wrote := uploadDocument(t, current, document.issue, documentName(t, current, document.id), document.markdown)
		if strings.Contains(string(wrote.body), "superseded_approval") {
			t.Errorf("the first write of %s on one build reports a superseded approval: %s", document.name, wrote.body)
		}
		settledDocument(t, ctx, current, database, document.id)
		number, rendering := latestMarkdown(t, current, document.id)
		// Every ask block, the blocks nobody edited included, holds the form R5 keeps, each value
		// as its ask's node holds it, and none holds the answer's words or selection. Each document
		// holds a block of a resolved ask, so the form's `state` is read where the ask is resolved
		// as well as where it is answered.
		resolved := 0
		for _, block := range keptBlocks[document.id] {
			if requireKeptForm(t, ctx, database, document.name+"'s version "+strconv.Itoa(number), rendering, block) == asks.StateResolved {
				resolved++
			}
		}
		if resolved == 0 {
			t.Errorf("%s's version %d holds no block of a resolved ask, so the form R5 keeps where an ask is resolved was never read:\n%s",
				document.name, number, rendering)
		}
		t.Logf("%s's version %d: %d blocks of resolved asks hold the form R5 keeps", document.name, number, resolved)
		requireNoAnswerWords(t, document.name+"'s version "+strconv.Itoa(number), rendering)
		approval := approvalOf(t, current, document.id)
		carried := newestReviewOf(t, current, document.id)
		// The carried review records the version the person approved, whatever carries stand
		// between it and this one.
		prefix := fmt.Sprintf("Carried forward from the approval of version %d:", document.version)
		if !approval.approvedLatest() || approval.LatestVersion != number || carried.Version != number || carried.Reason == nil || !strings.HasPrefix(*carried.Reason, prefix) {
			t.Errorf("%s reads %+v with newest review %+v at version %d (the newest review before the write: %+v), want its approval carried there with a reason opening %q",
				document.name, approval, carried, number, before, prefix)
		}
		strippedApproved, strippedNew := withoutAskServerAttributes(document.markdown), withoutAskServerAttributes(rendering)
		t.Logf("%s: the approved markdown without the eight is %d bytes, the new rendering %d", document.name, len(strippedApproved), len(strippedNew))
		if strippedApproved != strippedNew {
			t.Errorf("%s: without the eight attributes the approved markdown and the new rendering differ:\n%s\n----\n%s", document.name, strippedApproved, strippedNew)
		}
		// The person's review still holds the hash captured before the upgrade, and the version it
		// names still holds the bytes that hash is the digest of: the carry read the bytes already
		// stored and rewrote none of them. The carried review is a review of its own, over the bytes
		// the new version holds.
		approved := approvedHashes[document.id]
		if now := reviewHashOf(t, ctx, database, document); now.hash != approved.hash {
			t.Errorf("%s: the person's review %s holds the hash %q once the fleet is one build, want the hash %q captured before the upgrade",
				document.name, approved.review, now.hash, approved.hash)
		}
		if stored, _ := versionHashOf(t, ctx, database, document.id, document.version); stored != approved.hash {
			t.Errorf("%s: version %d, which the person approved, holds bytes whose digest is %q once the first write on one build carried the approval, want the review's hash %q captured before the upgrade: the carry rewrote the bytes already stored",
				document.name, document.version, stored, approved.hash)
		}
		if stored, reviewed := versionHashOf(t, ctx, database, document.id, number); reviewed != stored || reviewed != textHash(rendering) || reviewed == approved.hash {
			t.Errorf("%s: the carried review at version %d holds the hash %q, want the digest of that version's bytes, %q (the store's %q), which is not the person's review's %q",
				document.name, number, reviewed, textHash(rendering), stored, approved.hash)
		}
	}

	// An answer keeps the turn of an approval request. On a fleet of one build an agent writes a
	// fourth document holding one open ask and requests its approval, so the request waits on the
	// person; the person's answer settles into a version whose only change is the ask block's kept
	// form, and the request moves there quietly and still waits on the person, who then approves
	// through it.
	fourth := agentUpload(t, current, issue, "fourth.md", "# Fourth\n\nA line of prose.\n\n:::ask{#fourth-ask urgency=\"med\" multiple=\"false\"}\nShip the fourth?\n\n- Ship: Now.\n- Hold: Later.\n:::\n")
	fourthAsk := blockAskOf(t, current, issue, "fourth-ask")
	settledDocument(t, ctx, current, database, fourth)
	fourthNamed, _ := latestMarkdown(t, current, fourth)
	fourthRequest := openedRequestID(t, asAgentSession(t, current, http.MethodPost, "/api/v1/artifacts/"+fourth+"/approval-requests", map[string]any{}, http.StatusCreated))
	if request := approvalRequestOf(t, current, fourthRequest); !request.waitsOnThePersonAt(fourthNamed) {
		t.Fatalf("the fourth document's request reads %s, want it waiting on the person at version %d", request, fourthNamed)
	}
	answerAsk(t, current, fourthAsk, "Ship", "Ship it now.")
	settledDocument(t, ctx, current, database, fourth)
	fourthAnswered, fourthText := latestMarkdown(t, current, fourth)
	requireKeptForm(t, ctx, database, "the fourth document's version "+strconv.Itoa(fourthAnswered), fourthText, keptBlock{"fourth-ask", fourthAsk, ""})
	if request := approvalRequestOf(t, current, fourthRequest); fourthAnswered == fourthNamed || !request.waitsOnThePersonAt(fourthAnswered) {
		t.Errorf("the version an answer wrote moved the approval request to its agent: the fourth document's request reads %s, the answer's version %d, the version requested %d", request, fourthAnswered, fourthNamed)
	}
	if edits := approvalRequestEdits(t, ctx, database, fourthRequest); len(edits) != 1 || !edits[0].quiet || edits[0].notify {
		t.Errorf("the fourth document's request moved to the answer's version with the ask.edited events %+v, want one that is quiet and notifies nobody", edits)
	}
	// The person approves through the request: the review of the version it names answers it.
	current.call(t, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/reviews", fourth, fourthAnswered), `{"state":"approved"}`, http.StatusCreated)
	if approval, request := approvalOf(t, current, fourth), approvalRequestOf(t, current, fourthRequest); !approval.approvedLatest() || approval.LatestVersion != fourthAnswered || request.State != "answered" {
		t.Errorf("the fourth document reads %s and its request %s once the person approved through it, want approved at version %d, its latest, and the request answered", approvalState(approval), request, fourthAnswered)
	}

	// A copy of an open ask keeps every carry, and every turn R5's turn rule keeps. The fifth
	// document's first accepted write on one build is an agent's upload of the text its text route
	// serves: it renders the copy with its source and no state, and carries the approval, and so
	// does the version a person's answer to one of its own asks settles into.
	fifthCopy := keptBlock{"lonely", lonelyAskID, "dispatch://" + issue + "/artifact/" + slugOf(second)}
	fifthCarried := func(what string) {
		t.Helper()
		number, rendering := latestMarkdown(t, current, fifth)
		requireKeptForm(t, ctx, database, "the fifth document's version "+strconv.Itoa(number), rendering, fifthCopy)
		approval, carried := approvalOf(t, current, fifth), newestReviewOf(t, current, fifth)
		prefix := fmt.Sprintf("Carried forward from the approval of version %d:", five.version)
		if number == five.version || !approval.approvedLatest() || approval.LatestVersion != number || carried.Version != number || carried.Reason == nil || !strings.HasPrefix(*carried.Reason, prefix) {
			t.Errorf("the version did not carry its approval across a copy of an open ask: the fifth document's version %d, %s, whose copy block %s opens %q, reads %s with newest review %+v, want its approval carried there with a reason opening %q",
				number, what, fifthCopy.block, copyOpenerOf(rendering), approvalState(approval), carried, prefix)
		}
	}
	served := servedTextOf(t, current, "/api/v1/artifacts/"+fifth+"/text")
	if uploaded := agentReplace(t, current, issue, fifth, served.Markdown); strings.Contains(string(uploaded.body), "superseded_approval") {
		t.Errorf("the agent's upload of the fifth document's served text reports a superseded approval: %s", uploaded.body)
	}
	settledDocument(t, ctx, current, database, fifth)
	fifthCarried("the agent's upload of the text its text route serves, its first accepted write on one build")
	answerAsk(t, current, blockAskOf(t, current, issue, "fifth-a"), "Ship", "Ship the fifth now.")
	settledDocument(t, ctx, current, database, fifth)
	fifthCarried("the version a person's answer to its own ask settled into")

	// The sixth document's request waits on the person at the version the base release wrote. A
	// person's answer to one of its own asks settles into its first accepted write on one build,
	// which renders the copy as the fifth's does: the request moves there and still waits on the
	// person, and so it does across the version the answer to its other own ask writes.
	if request := approvalRequestOf(t, current, sixthRequest); !request.waitsOnThePersonAt(sixthBase) {
		t.Fatalf("the sixth document's request reads %s once the fleet is one build, want it waiting on the person at version %d, the base release's", request, sixthBase)
	}
	sixthCopy := keptBlock{"lonely", lonelyAskID, fifthCopy.copied}
	for _, own := range []string{"sixth-a", "sixth-b"} {
		answerAsk(t, current, blockAskOf(t, current, issue, own), "Ship", "Ship the sixth now.")
		settledDocument(t, ctx, current, database, sixth)
		number, rendering := latestMarkdown(t, current, sixth)
		requireKeptForm(t, ctx, database, "the sixth document's version "+strconv.Itoa(number), rendering, sixthCopy)
		if request := approvalRequestOf(t, current, sixthRequest); number == sixthBase || !request.waitsOnThePersonAt(number) {
			t.Errorf("the sixth document's request moved to its agent at a version whose only change is the kept form of a copy of an open ask: after the answer to %s the request reads %s, the latest version is %d, whose copy block %s opens %q",
				own, request, number, sixthCopy.block, copyOpenerOf(rendering))
		}
	}

	// Five writes that each change one thing beside the removal, and each supersede.
	changes := []struct {
		what   string
		change func(string) string
	}{
		{"a write that changes one word of prose beside the removal", func(markdown string) string {
			return strings.Replace(markdown, "behind a flag", "behind a switch", 1)
		}},
		{"a write that changes an ask's question", func(markdown string) string {
			return strings.Replace(markdown, "Which region takes s2?", "Which region takes s2 last?", 1)
		}},
		{"a write that adds a block", func(markdown string) string { return markdown + "\nA new paragraph.\n" }},
		{"a write that removes a block the approved version held", func(markdown string) string {
			return strings.Replace(markdown, "The service ships behind a flag.\n\n", "", 1)
		}},
		{"a write whose ask disagrees with its node", func(markdown string) string {
			return strings.Replace(markdown, "Ship s5 first?", "Ship s5 last?", 1)
		}},
	}
	for index, change := range changes {
		document := captured[index]
		changed := change.change(document.markdown)
		if changed == document.markdown {
			t.Fatalf("%s changed nothing in %s", change.what, document.name)
		}
		requireSuperseded(t, ctx, current, database, document, changed, change.what)
	}

	// After the carried version, a write that changes one word of prose supersedes it.
	_, carriedText := latestMarkdown(t, current, two.id)
	requireSuperseded(t, ctx, current, database, two, strings.Replace(carriedText, "A line of prose.", "A line of verse.", 1), "a one-word write after the carried version")
}

// requireSuperseded uploads content as the document's next version and requires the write to
// report the approval it superseded, and the document to read no approval of the version the write
// stored, read at once: a write whose ask asks other words than its decided row has them is
// followed by the settlement that writes the decided words back, which can return the text to what
// was approved.
func requireSuperseded(t *testing.T, ctx context.Context, p *dispatchProcess, database *pgx.Conn, document approvedDocument, content, what string) {
	t.Helper()
	wrote := uploadDocument(t, p, document.issue, documentName(t, p, document.id), content)
	if !strings.Contains(string(wrote.body), "superseded_approval") {
		t.Errorf("%s (%s) reports no superseded approval: %s", what, document.name, wrote.body)
	}
	if approval := approvalOf(t, p, document.id); approval.approvedLatest() {
		t.Errorf("%s (%s) left the document reading %+v, want its approval superseded", what, document.name, approval)
	}
	settledDocument(t, ctx, p, database, document.id)
}

// thirdDocumentRead is what the third document's ask and reading stand at: the ask row of its
// block read-it, its state and its answer as stored, and the graph node of its reading read-r1 and
// that node's state.
type thirdDocumentRead struct {
	ask, state, answer string
	reading, nodeState string
}

// thirdDocumentHolds reads the third document's ask row and reading node (thirdDocumentRead).
func thirdDocumentHolds(t *testing.T, ctx context.Context, database *pgx.Conn, artifactID string) thirdDocumentRead {
	t.Helper()
	var held thirdDocumentRead
	if err := database.QueryRow(ctx, `
		select a.id::text, a.state, coalesce(a.answer::text, ''),
		       coalesce((select node_id from document_blocks where artifact_id = $1::uuid and block_id = 'read-r1'), ''),
		       coalesce((select n.state from graph_nodes n join document_blocks b on n.id = b.node_id
		                 where b.artifact_id = $1::uuid and b.block_id = 'read-r1' limit 1), '')
		from asks a where a.block_artifact_id = $1::uuid and a.block_id = 'read-it'
	`, artifactID).Scan(&held.ask, &held.state, &held.answer, &held.reading, &held.nodeState); err != nil {
		t.Fatalf("read the third document's ask row and reading node: %v", err)
	}
	if held.reading == "" || held.nodeState == "" {
		t.Fatalf("the third document's reading has no node: %+v", held)
	}
	return held
}

// requireBlockInStepWithRow fails t unless the ask block blockID of markdown, a stored version,
// holds of the eight server-owned attributes exactly what the release before writes from the ask
// row of the block blockID on rowDocument, each value as the row holds it: the state; for an
// answered ask, who answered, when, the options chosen and the words; and, where copied is the
// address of rowDocument, the row's ask and that address, the block being a copy. It never holds
// invalid, which no stored version holds.
func requireBlockInStepWithRow(t *testing.T, ctx context.Context, database *pgx.Conn, markdown, blockID, rowDocument, copied string) {
	t.Helper()
	var askID, state string
	var answer []byte
	if err := database.QueryRow(ctx, `select id::text, state, answer from asks where block_artifact_id = $1::uuid and block_id = $2`,
		rowDocument, blockID).Scan(&askID, &state, &answer); err != nil {
		t.Fatalf("read the ask row of the block %s: %v", blockID, err)
	}
	want := map[string]any{"state": state}
	if state == "answered" && answer != nil {
		var row struct {
			User     string    `json:"user"`
			Selected []string  `json:"selected"`
			Text     *string   `json:"text"`
			At       time.Time `json:"at"`
		}
		if err := json.Unmarshal(answer, &row); err != nil {
			t.Fatalf("decode the answer of the block %s's ask: %v: %s", blockID, err, answer)
		}
		want["answered_by"], want["answered_at"], want["selected"] = row.User, asks.AnsweredAtValue(row.At), row.Selected
		if row.Text != nil {
			want["answer"] = *row.Text
		}
	}
	if copied != "" {
		want["copied_from"], want["copied_from_document"] = askID, copied
	}
	block := storedAskBlock(t, markdown, blockID)
	for _, name := range []string{"state", "answered_by", "answered_at", "selected", "answer", "invalid", "copied_from", "copied_from_document"} {
		wanted, isWanted := want[name]
		got, held := block.Attrs[name]
		if held != isWanted || (isWanted && mustJSON(t, got) != mustJSON(t, wanted)) {
			t.Errorf("while both builds serve the block %s holds %s=%s (held %t), want %s (held %t), as its asks row holds it",
				blockID, name, mustJSON(t, got), held, mustJSON(t, wanted), isWanted)
		}
	}
}

// lonelyAsk is the second document's one ask, which the fifth and the sixth document copy.
const lonelyAsk = ":::ask{#lonely urgency=\"med\" multiple=\"false\"}\nIs it lonely?\n\n- Yes: It is.\n- No: It is not.\n:::\n"

// copyingDocument is the fifth or the sixth document, name: a paragraph of prose, two open asks of
// its own (name-a and name-b) and a copy of the second document's open ask (lonelyAsk).
func copyingDocument(name string) string {
	return "# The " + name + " document\n\nThe " + name + " document copies a question.\n\n" +
		":::ask{#" + name + "-a urgency=\"high\" multiple=\"false\"}\nShip the " + name + " first?\n\n- Ship: Now.\n- Hold: Later.\n:::\n\n" +
		":::ask{#" + name + "-b urgency=\"low\" multiple=\"false\"}\nShip the " + name + " second?\n\n- Ship: Now.\n- Hold: Later.\n:::\n\n" +
		lonelyAsk
}

// copyOpenerOf is the opening line of the copy of the second document's ask in markdown, or "".
func copyOpenerOf(markdown string) string {
	for _, opener := range askOpener.FindAllString(markdown, -1) {
		if strings.Contains(opener, "{#lonely ") {
			return opener
		}
	}
	return ""
}

// keptBlock is an ask block a version written once the fleet is one build holds: its id, the ask
// whose node its kept attributes are written from - its own, or a copy's source - and, for a copy,
// the address of the source's document.
type keptBlock struct {
	block, ask, copied string
}

// serverOwnedAskAttributes is the eight server-owned attributes of an ask block.
var serverOwnedAskAttributes = []string{"state", "answered_by", "answered_at", "selected", "answer", "invalid", "copied_from", "copied_from_document"}

// requireKeptForm fails t unless the ask block kept.block of markdown, a version written once the
// fleet is one build, holds of the eight server-owned attributes exactly the form R5 keeps, each
// value as the node of kept.ask holds it: state where the ask is answered or resolved and none
// where it is open, answered_by and answered_at where it is answered, and, for a copy,
// copied_from naming kept.ask and copied_from_document naming kept.copied, whatever the source's
// state. It never holds selected, answer or invalid. A block that differs is printed. It answers
// the answer_state the node holds.
func requireKeptForm(t *testing.T, ctx context.Context, database *pgx.Conn, what, markdown string, kept keptBlock) string {
	t.Helper()
	held := askNodeHolds(t, ctx, database, kept.ask)
	state := held[graphstore.TupleAnswerState].ToID
	if state == "" {
		t.Errorf("%s: the node of ask %s holds no answer_state, so no form of the block %s can be read from it: %v", what, kept.ask, kept.block, held)
	}
	want := map[string]string{}
	if state != "" && state != asks.StateOpen {
		want["state"] = state
	}
	if state == asks.StateAnswered {
		want["answered_by"] = strings.TrimSpace(held[graphstore.TupleAnsweredBy].ToID)
		want["answered_at"] = held[graphstore.TupleAnsweredAt].ToID
	}
	if kept.copied != "" {
		want["copied_from"], want["copied_from_document"] = kept.ask, kept.copied
	}
	block := storedAskBlock(t, markdown, kept.block)
	differs := false
	for _, name := range serverOwnedAskAttributes {
		wanted, isWanted := want[name]
		got, isHeld := block.Attrs[name]
		value, _ := got.(string)
		same := isHeld == isWanted && (!isWanted || value == wanted || name == "answered_at" && sameInstant(value, wanted))
		if !same {
			differs = true
			t.Errorf("%s: the block %s holds %s=%s (held %t), want %q (held %t): the form R5 keeps, from the node of ask %s, which holds %q",
				what, kept.block, name, mustJSON(t, got), isHeld, wanted, isWanted, kept.ask, state)
		}
	}
	if differs {
		t.Errorf("%s: the ask block %s, which differs from the form R5 keeps:\n%s", what, kept.block, askBlockText(markdown, kept.block))
	}
	return state
}

// askBlockText is the ask block blockID of markdown as the version writes it, from its opening
// line to its closing fence, for a failure to print; markdown whole where no opening line names
// blockID.
func askBlockText(markdown, blockID string) string {
	lines := strings.Split(markdown, "\n")
	for start, line := range lines {
		if !askOpener.MatchString(line) || !strings.Contains(line, "{#"+blockID+" ") && !strings.Contains(line, "{#"+blockID+"}") {
			continue
		}
		fence := strings.TrimLeft(line, "> \t-")
		fence = fence[:len(fence)-len(strings.TrimLeft(fence, ":"))]
		for end := start + 1; end < len(lines); end++ {
			if strings.TrimRight(strings.TrimLeft(lines[end], "> \t"), " \t") == fence {
				return strings.Join(lines[start:end+1], "\n")
			}
		}
		return strings.Join(lines[start:], "\n")
	}
	return markdown
}

// sameInstant reports whether a and b, two RFC 3339 times, name one instant.
func sameInstant(a, b string) bool {
	at, errA := time.Parse(time.RFC3339Nano, a)
	bt, errB := time.Parse(time.RFC3339Nano, b)
	return errA == nil && errB == nil && at.Equal(bt)
}

// answerWords is an answer's words, its selection or `invalid` on an ask block's opening line.
var answerWords = regexp.MustCompile(` (selected|answer|invalid)="`)

// requireNoAnswerWords fails t unless no ask block of markdown, a stored version, holds an answer's
// words, its selection or `invalid`.
func requireNoAnswerWords(t *testing.T, what, markdown string) {
	t.Helper()
	for _, opener := range askOpener.FindAllString(markdown, -1) {
		if answerWords.MatchString(opener) {
			t.Errorf("%s: an ask block holds an answer's words once the fleet is one build: %s", what, opener)
		}
	}
}

// agentReplace uploads content as a new version of the issue's document artifactID as the agent's
// session (asAgentSession's), naming the token of the version it replaces, and answers the answer.
func agentReplace(t *testing.T, p *dispatchProcess, issue, artifactID, content string) httpAnswer {
	t.Helper()
	artifacts := "/api/v1/issues/" + issue + "/artifacts"
	name := documentName(t, p, artifactID)
	body := map[string]any{"name": name, "content": content, "actor": map[string]string{"kind": "session", "id": restoreSession}}
	if base := p.uploadBase(t, artifacts, name); base != nil {
		body["precondition"] = base
	}
	answer := p.send(t, http.MethodPost, artifacts, encodeJSON(t, body), agentBearer())
	if answer.status != http.StatusCreated && answer.status != http.StatusOK {
		t.Fatalf("the agent's upload of %s to %s: %s", name, issue, answer)
	}
	return answer
}

// openedRequestID is the id of the approval request POST /api/v1/artifacts/{id}/approval-requests
// answered.
func openedRequestID(t *testing.T, answer httpAnswer) string {
	t.Helper()
	var opened struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}
	if err := json.Unmarshal(answer.body, &opened); err != nil || opened.Ask.ID == "" {
		t.Fatalf("read the approval request back: %v: %s", err, answer)
	}
	return opened.Ask.ID
}

// approvalRequest is an approval request as GET /api/v1/asks/{id} reads it: its state, whose reply
// it waits on, the version of its document it names and the version it was last handed to the
// person at.
type approvalRequest struct {
	State     string `json:"state"`
	WaitingOn string `json:"waiting_on"`
	Approval  struct {
		Version          int `json:"version"`
		RequestedVersion int `json:"requested_version"`
	} `json:"approval"`
}

func (r approvalRequest) String() string {
	return fmt.Sprintf("%s waiting on %q at version %d requested at version %d", r.State, r.WaitingOn, r.Approval.Version, r.Approval.RequestedVersion)
}

// waitsOnThePersonAt reports whether the request is open and waits on the person at version: it
// names that version and was handed to the person at it.
func (r approvalRequest) waitsOnThePersonAt(version int) bool {
	return r.State == "open" && r.WaitingOn == "human" && r.Approval.Version == version && r.Approval.RequestedVersion == version
}

func approvalRequestOf(t *testing.T, p *dispatchProcess, askID string) approvalRequest {
	t.Helper()
	var read struct {
		Ask approvalRequest `json:"ask"`
	}
	readJSON(t, p, "/api/v1/asks/"+askID, &read)
	return read.Ask
}

// requestEdit is one ask.edited event of an approval request: whether its payload marks the move
// quiet (model.AskEditEventPayload.Quiet) and whether it notifies.
type requestEdit struct {
	quiet, notify bool
}

// approvalRequestEdits is every ask.edited event of the request askID, oldest first.
func approvalRequestEdits(t *testing.T, ctx context.Context, database *pgx.Conn, askID string) []requestEdit {
	t.Helper()
	rows, err := database.Query(ctx, `
		select coalesce((payload->>'quiet')::boolean, false), notify
		from events where type = 'ask.edited' and payload->>'id' = $1 order by seq
	`, askID)
	if err != nil {
		t.Fatalf("read the ask.edited events of request %s: %v", askID, err)
	}
	defer rows.Close()
	var edits []requestEdit
	for rows.Next() {
		var edit requestEdit
		if err := rows.Scan(&edit.quiet, &edit.notify); err != nil {
			t.Fatalf("read an ask.edited event of request %s: %v", askID, err)
		}
		edits = append(edits, edit)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the ask.edited events of request %s: %v", askID, err)
	}
	return edits
}

// servedText is the answer of a text route (GET /api/v1/artifacts/{id}/text and its slug forms):
// the markdown it serves and its asks.
type servedText struct {
	Markdown string          `json:"markdown"`
	Asks     []model.TextAsk `json:"asks"`
}

// servedTextOf is what the text route path answers through p to a reader of this build, which
// sends `Dispatch-Asks: 1` as this build's CLI does (asReaderOfAsks).
func servedTextOf(t *testing.T, p *dispatchProcess, path string) servedText {
	t.Helper()
	answer := p.send(t, http.MethodGet, path, "", asReaderOfAsks(dispatchTestLogin))
	if answer.status != http.StatusOK {
		t.Fatalf("GET %s: status %d, want %d: %s", path, answer.status, http.StatusOK, answer.body)
	}
	var text servedText
	if err := json.Unmarshal(answer.body, &text); err != nil {
		t.Fatalf("decode GET %s: %v: %s", path, err, answer.body)
	}
	return text
}

// textAskOf is the entry of asks for the ask block blockID, or nil where none is.
func textAskOf(asks []model.TextAsk, blockID string) *model.TextAsk {
	for index := range asks {
		if asks[index].BlockID == blockID {
			return &asks[index]
		}
	}
	return nil
}

// cliOutcome is one run of this head's `dispatch` command.
type cliOutcome struct {
	code           int
	stdout, stderr string
}

func (o cliOutcome) String() string {
	return fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s", o.code, o.stdout, o.stderr)
}

// runCurrentCLI runs this head's `dispatch` command (packages/envoy-client's bin/dispatch.ts,
// under bun) against server with args, as an agent: the shared agent token every test process
// trusts, as the session the two-build suite's CLI acts as. Its environment is written here, never
// inherited, so it reads no developer's envoy.json and no pane's Dispatch.
func runCurrentCLI(t *testing.T, server *dispatchProcess, home string, args ...string) cliOutcome {
	t.Helper()
	entry, err := filepath.Abs(filepath.Join("..", "..", "..", "envoy-client", "bin", "dispatch.ts"))
	if err != nil {
		t.Fatalf("resolve this head's CLI: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bun", append([]string{entry}, args...)...)
	cmd.Dir = home
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"DISPATCH_STATE_DIR=" + home,
		"DISPATCH_HOST=omp",
		"DISPATCH_SESSION_ID=attribute-removal-cli",
		"DISPATCH_URL=" + server.url(""),
		"DISPATCH_TOKEN=" + dispatchTestAgentToken,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	run := cliOutcome{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		run.code = exit.ExitCode()
	case err != nil:
		t.Fatalf("run dispatch %s: %v", strings.Join(args, " "), err)
	}
	return run
}
