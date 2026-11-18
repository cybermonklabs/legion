//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go. This file's test reads the
// base release the Verification document's preamble builds once per head (baseRelease), and is
// run as
//
//	go test -tags basebuild -timeout 30m ./cmd/dispatch -run '^TestAnApprovalCarriesAcrossTheRemovalOfTheServerOwnedAskAttributes$' -count=1 -v

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
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

// uploadDocument uploads content under name to the issue through p, as the test's person, and
// answers the answer: a new document, or a new version of the issue's document of that name.
func uploadDocument(t *testing.T, p *dispatchProcess, issue, name, content string) httpAnswer {
	t.Helper()
	answer := p.send(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", encodeJSON(t, map[string]string{"name": name, "content": content}), asUser(dispatchTestLogin))
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

// One version whose only change is the removal of the server-owned ask attributes carries a
// person's approval, and nothing wider does (dispatch://INFRA-1150 R10, c-attribute-removal). The
// base release writes the documents and a person approves them; this head writes a document
// holding a reading; the base release serves again beside this head and leaves that document be;
// while both serve, the roll window keeps the attributes; once the base release is gone, the first
// write to each document drops them and carries its approval, and every wider write supersedes.
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
	issue, first := createCensusIssue(t, previous, "RMV", "The removal", removalDocument("one"))
	sourceDocument := uploadedID(t, uploadDocument(t, previous, issue, "source.md", "# Source\n\n:::ask{#shared urgency=\"med\" multiple=\"false\"}\nShare the source?\n\n- Share: Now.\n- Keep: Later.\n:::\n"))
	blockAskOf(t, previous, issue, "shared")
	previous.call(t, http.MethodPost, "/api/v1/artifacts/"+first+"/edits", encodeJSON(t, map[string]any{"ops": []map[string]string{{
		"op": "insert", "after": "end", "markdown": ":::ask{#shared urgency=\"med\" multiple=\"false\"}\nShare the source?\n\n- Share: Now.\n- Keep: Later.\n:::\n",
	}}}), http.StatusOK)
	answerAsk(t, previous, blockAskOf(t, previous, issue, "decided-one"), "Ship", "Ship one now.")
	second := uploadedID(t, uploadDocument(t, previous, issue, "lonely.md", "# Lonely\n\nA line of prose.\n\n:::ask{#lonely urgency=\"med\" multiple=\"false\"}\nIs it lonely?\n\n- Yes: It is.\n- No: It is not.\n:::\n"))
	blockAskOf(t, previous, issue, "lonely")
	superseding := make([]string, 5)
	for index := range superseding {
		name := fmt.Sprintf("s%d", index+1)
		superseding[index] = uploadedID(t, uploadDocument(t, previous, issue, name+".md", removalDocument(name)))
		answerAsk(t, previous, blockAskOf(t, previous, issue, "decided-"+name), "Ship", "Ship "+name+" now.")
		blockAskOf(t, previous, issue, "pending-"+name)
	}
	one := approveAndCapture(t, ctx, previous, database, issue, first, "the first document")
	two := approveAndCapture(t, ctx, previous, database, issue, second, "the second document")
	for _, name := range []string{"state", "answered_by", "answered_at", "selected", "answer", "copied_from", "copied_from_document"} {
		if !strings.Contains(one.markdown, " "+name+`="`) {
			t.Fatalf("the base release's first document holds no %s on its ask blocks:\n%s", name, one.markdown)
		}
	}
	if lonely := askOpener.FindString(two.markdown); !strings.Contains(lonely, `state="open"`) || strings.Contains(lonely, "answered") || strings.Contains(lonely, "copied") {
		t.Fatalf("the base release's second document's only ask reads %q, want state=\"open\" alone", lonely)
	}
	captured := make([]approvedDocument, len(superseding))
	for index, id := range superseding {
		captured[index] = approveAndCapture(t, ctx, previous, database, issue, id, fmt.Sprintf("superseding document %d", index+1))
	}
	t.Logf("the base release %s wrote the documents; the person approved the first at version %d and the second at version %d; reviews %s %s",
		baseCommit, one.version, two.version, one.review, two.review)
	stopProcess(t, previous)

	// This head writes a third document holding a reading inside an answered ask.
	current := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: head, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay})
	current.waitHealthy(t)
	readingAsk := ":::ask{#read-it urgency=\"med\" multiple=\"false\"}\nRead it?\n\n- Yes: Now.\n- No: Later.\n:::\n"
	third := uploadedID(t, uploadDocument(t, current, issue, "reading.md", "# Reading\n\n"+readingAsk))
	answerAsk(t, current, blockAskOf(t, current, issue, "read-it"), "Yes", "Read it now.")
	uploadDocument(t, current, issue, "reading.md", "# Reading\n\n::::ask{#read-it urgency=\"med\" multiple=\"false\"}\nRead it?\n\n- Yes: Now.\n- No: Later.\n\n"+
		":::reading{#read-r1}\nThe agent reads it as reading now.\n:::\n::::\n")
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

	// The fleet is this head alone: the first accepted write to each of the two documents drops the
	// attributes and carries the approval.
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
		for _, opener := range askOpener.FindAllString(rendering, -1) {
			if askServerAttribute.MatchString(opener) {
				t.Errorf("%s's ask block still holds a server-owned attribute once the fleet is one build: %s", document.name, opener)
			}
		}
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
