package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A human's approval is what a request to approve again follows. A request opened on a document
// with an earlier approval names that approval's version, time and approver in its approval
// (approval.approved: version, at, by), so the human reads what he is approving since the version
// he approved rather than from nothing; a first request names none. A person's answer to an
// ask block is held once, on the ask's node, and changes no byte of the document,
// so it writes no version and an approval stands through it; the
// carries that remain are those of a version restoring the approved text, of R2's readings
// (reading_approval_test.go) and of R10's case (docs.carryApproval).

// carriedAsk is an approval ask as the carry tests read it: the document version it names and the
// earlier approval it carries, absent when the document was never approved.
type carriedAsk struct {
	ID       string `json:"id"`
	Approval struct {
		Version          int                        `json:"version"`
		RequestedVersion int                        `json:"requested_version"`
		Approved         *model.AskApprovalApproved `json:"approved"`
	} `json:"approval"`
}

// approvalCarryHandler serves documents that version on an agent's edit in the edit's own
// transaction and settle a human's live write - an answer to an ask block, a resolution - in
// milliseconds, so a test waits for the version that write settles into rather than racing it.
func approvalCarryHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, _, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	return handler
}

// settlingCarryDocuments serves documents as approvalCarryHandler does, settling settle after a
// live write, and answers the store and the document service beside the handler, for a test that
// writes a document as the server's own code does, as a browser's socket edit does
// (connectBrowserPeer, whose X-Dispatch-User header the service's Identity admits), or settles it
// at once. A test that needs two writes in one version - an answer's settlement and an agent's
// edit - uses a window wide enough that both land in it, and settles the document at once
// (docs.SettleNow) wherever it needs no window, rather than waiting each window out.
func settlingCarryDocuments(t *testing.T, settle time.Duration) (http.Handler, *store.Store, *docs.Service) {
	t.Helper()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		service := docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: settle})
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		documentService = service
		return service
	})
	return handler, database, documentService
}

// reviewDocument reviews the document's latest settled version from its header as login and
// returns the review it wrote.
func reviewDocument(t *testing.T, handler http.Handler, artifactID, login, state, reason string) model.ArtifactReview {
	t.Helper()
	body := map[string]any{"state": state}
	if reason != "" {
		body["reason"] = reason
	}
	reviewed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews", body, login)
	if reviewed.Code != http.StatusCreated {
		t.Fatalf("%s reviews %s: status=%d body=%s", login, state, reviewed.Code, reviewed.Body.String())
	}
	return decodeBody[model.ArtifactReview](t, reviewed)
}

// reviseDocument replaces find with with as the asking agent through the edits route, which
// versions the document in the route's own transaction, and waits until that version reads.
func reviseDocument(t *testing.T, handler http.Handler, artifactID, find, with string, number int) {
	t.Helper()
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": find, "with": with}},
		"actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("revise the document to version %d: status=%d body=%s", number, edited.Code, edited.Body.String())
	}
	waitForArtifactVersion(t, handler, artifactID, number)
}

// requestApproval opens the asking agent's approval request on the document and returns the ask
// the route answers with.
func requestApproval(t *testing.T, handler http.Handler, artifactID string) carriedAsk {
	t.Helper()
	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	return decodeBody[struct {
		Ask carriedAsk `json:"ask"`
	}](t, requested).Ask
}

// readCarriedAsk reads the ask as GET /api/v1/asks/{id} answers it.
func readCarriedAsk(t *testing.T, handler http.Handler, askID string) carriedAsk {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read approval ask: status=%d body=%s", read.Code, read.Body.String())
	}
	return decodeBody[struct {
		Ask carriedAsk `json:"ask"`
	}](t, read).Ask
}

// requireCarriedApproval fails t unless ask carries exactly review: its version, its created_at as
// RFC 3339 text, and its actor.
func requireCarriedApproval(t *testing.T, ask carriedAsk, review model.ArtifactReview) {
	t.Helper()
	carried := ask.Approval.Approved
	if carried == nil {
		t.Fatalf("approval = %#v, want approved", ask.Approval)
	}
	if carried.Version != review.Version {
		t.Fatalf("approved.version = %d, want %d", carried.Version, review.Version)
	}
	approvedAt, err := time.Parse(time.RFC3339, carried.At)
	if err != nil {
		t.Fatalf("approved.at %q is not RFC 3339: %v", carried.At, err)
	}
	if !approvedAt.Equal(review.CreatedAt) {
		t.Fatalf("approved.at = %s, want the review's created_at %s", approvedAt.Format(time.RFC3339Nano), review.CreatedAt.Format(time.RFC3339Nano))
	}
	if !reflect.DeepEqual(carried.By, review.Actor) {
		t.Fatalf("approved.by = %#v, want the approving actor %#v", carried.By, review.Actor)
	}
}

// A request on a document a human approved at an earlier version names that approval: the version
// he approved, when, and that it was him.
func TestARequestOnAnApprovedDocumentCarriesTheApproval(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approve me again", "A spec")
	review := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	if review.Version != 1 || review.Actor.Kind != "user" || review.Actor.ID != "alice" {
		t.Fatalf("review = %#v, want alice's approval of version 1", review)
	}
	reviseDocument(t, handler, issue.PrimaryArtifactID, "A spec", "A revised spec", 2)

	ask := requestApproval(t, handler, issue.PrimaryArtifactID)
	if ask.Approval.Version != 2 || ask.Approval.RequestedVersion != 2 {
		t.Fatalf("request approval = %#v, want a request at version 2", ask.Approval)
	}
	requireCarriedApproval(t, ask, review)
	requireCarriedApproval(t, readCarriedAsk(t, handler, ask.ID), review)
}

// A first request, on a document nobody has approved, carries no approved key: not a null value,
// no key at all.
func TestAFirstRequestCarriesNoEarlierApproval(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approve me", "A spec")
	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	approval := decodeBody[struct {
		Ask struct {
			Approval map[string]json.RawMessage `json:"approval"`
		} `json:"ask"`
	}](t, requested).Ask.Approval
	if string(approval["version"]) != "1" || string(approval["requested_version"]) != "1" {
		t.Fatalf("approval = %s, want version and requested_version 1", requested.Body.String())
	}
	if raw, present := approval["approved"]; present {
		t.Fatalf("a first request carries approved = %s, want the key absent", raw)
	}
}

// Two humans approved at different versions: the request names the newest approval, whoever gave
// it, and a later request for changes, which approves nothing, does not displace it.
func TestARequestCarriesTheNewestApprovalWhoeverGaveIt(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approved twice", "A spec")
	alices := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	reviseDocument(t, handler, issue.PrimaryArtifactID, "A spec", "A second spec", 2)
	bobs := reviewDocument(t, handler, issue.PrimaryArtifactID, "bob", "approved", "")
	if bobs.Version != 2 || bobs.Actor.ID != "bob" || bobs.Actor.ID == alices.Actor.ID {
		t.Fatalf("second review = %#v, want bob's approval of version 2", bobs)
	}
	reviseDocument(t, handler, issue.PrimaryArtifactID, "A second spec", "A third spec", 3)

	requireCarriedApproval(t, requestApproval(t, handler, issue.PrimaryArtifactID), bobs)

	// Alice answers the open request from the header asking for changes, which is a newer review
	// but no approval; the request the next revision opens still follows bob's approval.
	changes := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "changes_requested", "Name the rollback path.")
	if changes.Version != 3 || changes.State != "changes_requested" {
		t.Fatalf("changes requested = %#v, want a changes_requested review of version 3", changes)
	}
	reviseDocument(t, handler, issue.PrimaryArtifactID, "A third spec", "A fourth spec", 4)
	requireCarriedApproval(t, requestApproval(t, handler, issue.PrimaryArtifactID), bobs)
}

// A version that moves the open request rewords it to the new version and re-encodes its whole
// approval, so the earlier approval it carries survives the move unchanged.
func TestAVersionMoveKeepsTheCarriedApproval(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Moving carried approval", "A spec")
	review := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	reviseDocument(t, handler, issue.PrimaryArtifactID, "A spec", "A revised spec", 2)
	ask := requestApproval(t, handler, issue.PrimaryArtifactID)
	requireCarriedApproval(t, ask, review)

	reviseDocument(t, handler, issue.PrimaryArtifactID, "A revised spec", "A twice revised spec", 3)
	moved := readCarriedAsk(t, handler, ask.ID)
	if moved.ID != ask.ID || moved.Approval.Version != 3 || moved.Approval.RequestedVersion != 2 {
		t.Fatalf("ask after the version move = %#v, want the same request at version 3, requested at version 2", moved)
	}
	requireCarriedApproval(t, moved, review)
}

// The ask blocks the carry tests write: each a block id, a question and two options.
const (
	waveAsk   = ":::ask{#wave urgency=\"med\" multiple=\"false\"}\nHow many regions take the first wave?\n\n- Two: The two with the fewest tenants.\n- Five: Half of them.\n:::\n"
	regionAsk = ":::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
)

// insertDecisionBlocks inserts markdown, holding the ask blocks named by blocks (id to question),
// at the end of the document as the asking agent (insertDecisionMarkdown); it then waits for
// settlement to index every block and returns each block's ask id by block id.
func insertDecisionBlocks(t *testing.T, handler http.Handler, issueKey, artifactID, markdown string, number int, blocks map[string]string) map[string]string {
	t.Helper()
	insertDecisionMarkdown(t, handler, artifactID, markdown, number)
	return indexedDecisionBlocks(t, handler, issueKey, artifactID, blocks)
}

// insertDecisionMarkdown inserts markdown at the end of the document as the asking agent, which
// versions the document to number in the edit's own transaction, and waits until that version
// reads.
func insertDecisionMarkdown(t *testing.T, handler http.Handler, artifactID, markdown string, number int) {
	t.Helper()
	inserted := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": markdown}},
	})
	if inserted.Code != http.StatusOK {
		t.Fatalf("insert the ask blocks: status=%d body=%s", inserted.Code, inserted.Body.String())
	}
	waitForArtifactVersion(t, handler, artifactID, number)
}

// indexedDecisionBlocks waits for settlement to index each block of blocks (id to question) on the
// document and returns each block's ask id by block id.
func indexedDecisionBlocks(t *testing.T, handler http.Handler, issueKey, artifactID string, blocks map[string]string) map[string]string {
	t.Helper()
	asks := make(map[string]string, len(blocks))
	for blockID, question := range blocks {
		awaitIndexedAskBlock(t, handler, artifactID, blockID, question)
		asks[blockID] = blockAskID(t, handler, issueKey, blockID)
	}
	return asks
}

// answerDecisionBlock answers the block ask as login with selected and text, on the revision a
// block ask nobody reworded carries (no edited_at); replaces, when set, is the `at` of the answer
// this one changes.
func answerDecisionBlock(t *testing.T, handler http.Handler, askID, login string, selected []string, text string, replaces *time.Time) {
	t.Helper()
	body := map[string]any{"selected": selected, "expected_edited_at": nil}
	if text != "" {
		body["text"] = text
	}
	if replaces != nil {
		body["expected_answer_at"] = replaces.UTC().Format(time.RFC3339Nano)
	}
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", body, login)
	if answered.Code != http.StatusOK {
		t.Fatalf("%s answers the ask block: status=%d body=%s", login, answered.Code, answered.Body.String())
	}
}

// servedReview is one review as GET /api/v1/artifacts/{id}/reviews serves it, its actor kept as
// the JSON the route wrote so two reviews' actors compare byte for byte.
type servedReview struct {
	ID      string          `json:"id"`
	Version int             `json:"version"`
	State   string          `json:"state"`
	Actor   json.RawMessage `json:"actor"`
	Reason  *string         `json:"reason"`
	AskID   *string         `json:"ask_id"`
}

// documentReviews is the document's reviews, newest first.
func documentReviews(t *testing.T, handler http.Handler, artifactID string) []servedReview {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/reviews", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list reviews: status=%d body=%s", listed.Code, listed.Body.String())
	}
	return decodeBody[[]servedReview](t, listed)
}

// requireReviewCarriedWith fails t unless the document's newest review is the carry of the
// approval of version from to version: approved, in the approving actor's name byte for byte, with
// reason and askID, and unless the document now reads approved at version, which is its latest.
// It returns that review.
func requireReviewCarriedWith(t *testing.T, handler http.Handler, artifactID string, version, from int, reason string, askID *string) servedReview {
	t.Helper()
	reviews := documentReviews(t, handler, artifactID)
	var approval *servedReview
	for index := range reviews {
		if reviews[index].Version == from && reviews[index].State == "approved" {
			approval = &reviews[index]
			break
		}
	}
	if approval == nil {
		t.Fatalf("reviews = %+v, want an approval of version %d among them", reviews, from)
	}
	carried := reviews[0]
	if carried.Version != version || carried.State != "approved" {
		t.Fatalf("newest review = %+v, want an approval of version %d", carried, version)
	}
	if string(carried.Actor) != string(approval.Actor) {
		t.Fatalf("carried review actor = %s, want the approval's %s", carried.Actor, approval.Actor)
	}
	if carried.Reason == nil || *carried.Reason != reason {
		t.Fatalf("carried review reason = %v, want %q", carried.Reason, reason)
	}
	if !reflect.DeepEqual(carried.AskID, askID) {
		t.Fatalf("carried review ask_id = %v, want %v", carried.AskID, askID)
	}
	got := readApproval(t, handler, artifactID)
	if got.Approval == nil || got.Approval.State != "approved" || got.Approval.LatestVersion != version || got.Approval.Version == nil || *got.Approval.Version != version {
		t.Fatalf("approval after the carry = %#v, want approved at version %d, the latest", got.Approval, version)
	}
	return carried
}

// requireNoCarry fails t unless the document still has exactly reviews reviews and reads state.
func requireNoCarry(t *testing.T, handler http.Handler, artifactID string, reviews int, state string) {
	t.Helper()
	if listed := documentReviews(t, handler, artifactID); len(listed) != reviews {
		t.Fatalf("reviews = %+v, want the %d written before the answer and no carried one", listed, reviews)
	}
	if got := readApproval(t, handler, artifactID); got.Approval == nil || got.Approval.State != state {
		t.Fatalf("approval = %#v, want %s", got.Approval, state)
	}
}

// carryEvent is one of the issue's events as the carry tests read them: its order on the issue,
// its actor and its payload.
type carryEvent struct {
	Seq     int             `json:"seq"`
	Type    string          `json:"type"`
	Actor   model.Actor     `json:"actor"`
	Payload json.RawMessage `json:"payload"`
}

// carryEvents is the issue's event log, oldest first.
func carryEvents(t *testing.T, handler http.Handler, issueKey string) []carryEvent {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/events?limit=200", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", listed.Code, listed.Body.String())
	}
	return decodeBody[[]carryEvent](t, listed)
}

// decodePayload decodes event's payload into T.
func decodePayload[T any](t *testing.T, event carryEvent) T {
	t.Helper()
	var payload T
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode %s payload %s: %v", event.Type, event.Payload, err)
	}
	return payload
}

// storedAnswerAttributes are the seven server-owned attributes of an ask block a stored version
// could hold, as their markdown spells them. A version written on a fleet of one build holds
// none.
var storedAnswerAttributes = []string{` state="`, ` answered_by="`, ` answered_at="`, ` selected="`, ` answer="`, ` copied_from="`, ` copied_from_document="`}

// requireNoAnswerAttributes fails t unless markdown, a stored version's, holds none of the seven
// server-owned ask attributes.
func requireNoAnswerAttributes(t *testing.T, what, markdown string) {
	t.Helper()
	for _, attribute := range storedAnswerAttributes {
		if strings.Contains(markdown, attribute) {
			t.Fatalf("%s = %s, want no%s attribute: an answer is held on the ask's node alone", what, markdown, strings.TrimSuffix(attribute, `="`))
		}
	}
}

// requireNoVersionPast fails t unless, once the document owes no settlement, its latest version is
// still number: what the test wrote since - an answer, a resolution - is held on the asks' nodes
// and rows and changed no byte of the document.
func requireNoVersionPast(t *testing.T, handler http.Handler, database *store.Store, artifactID string, number int) {
	t.Helper()
	awaitSettled(t, database, artifactID)
	versions := documentVersions(t, handler, artifactID)
	if latest := versions[len(versions)-1].Number; latest != number {
		t.Fatalf("latest version = %d, want %d: an answer or a resolution writes no version", latest, number)
	}
}

// A person's answer is held once, on the ask's node, and changes no byte of the document: once
// the whole fleet runs this build, a person's answer changes no byte of any document, and an
// approval of that document stands through it. So his answer to a
// block the agent wrote since his approval writes no version and carries nothing: the approval
// stays where it stood, stale. Once he approves the version holding the block, his change of the
// answer and the resolution of the other block write no version either, and his approval stands
// at the latest version: no review is added, no approval request is opened, and no stored version
// holds the answer.
func TestAnAnswerWritesNoVersionAndTheApprovalStandsThroughIt(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "The approval stands through the answer",
		"# Plan\n\nThe service ships behind a flag.\n\n> Rollback is a flag flip.\n\n- Announce the dates.\n- Pick the regions.\n")
	artifactID := issue.PrimaryArtifactID
	reviewDocument(t, handler, artifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, artifactID, waveAsk+"\n"+regionAsk, 2,
		map[string]string{"wave": "How many regions take the first wave?", "region": "Which region goes first?"})
	if got := readApproval(t, handler, artifactID); got.Approval.State != "stale" {
		t.Fatalf("approval after the block insert = %#v, want stale", got.Approval)
	}

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "Two regions first.", nil)
	requireNoVersionPast(t, handler, database, artifactID, 2)
	requireNoCarry(t, handler, artifactID, 1, "stale")

	approveVersion(t, handler, artifactID, "alice", 2)
	first := readBlockAsk(t, handler, asks["wave"])
	if first.State != "answered" || first.Answer == nil || first.Answer.User != "alice" {
		t.Fatalf("ask after the answer = state %q answer %#v, want alice's answer", first.State, first.Answer)
	}
	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Five"}, "Half of them.", &first.Answer.At)
	if resolved := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks["region"]+"/resolve", map[string]any{
		"kind": "resolved", "reason": "Decided in the kickoff.", "actor": sessionActor(),
	}); resolved.Code != http.StatusOK {
		t.Fatalf("resolve the second block: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	requireNoVersionPast(t, handler, database, artifactID, 2)
	requireNoCarry(t, handler, artifactID, 2, "approved")
	if got := readApproval(t, handler, artifactID); got.Approval.Version == nil || *got.Approval.Version != 2 || got.Approval.LatestVersion != 2 {
		t.Fatalf("approval after the changed answer and the resolution = %#v, want approved at version 2, the latest", got.Approval)
	}
	if changed := readBlockAsk(t, handler, asks["wave"]); changed.Answer == nil || !reflect.DeepEqual(changed.Answer.Selected, []string{"Five"}) {
		t.Fatalf("ask after the change = %#v, want alice's answer Five", changed.Answer)
	}
	if region := readBlockAsk(t, handler, asks["region"]); region.State != "resolved" {
		t.Fatalf("second block's ask = %q, want resolved", region.State)
	}
	requireNoAnswerAttributes(t, "version 2", versionMarkdown(t, handler, artifactID, 2))
	for _, event := range carryEvents(t, handler, issue.Key) {
		switch event.Type {
		case "artifact.version":
			if number := decodePayload[struct {
				Version model.Version `json:"version"`
			}](t, event).Version.Number; number > 2 {
				t.Fatalf("the answers appended artifact.version %d: %s", number, event.Payload)
			}
		case "ask.opened":
			if decodePayload[struct {
				Kind string `json:"kind"`
			}](t, event).Kind == "approval" {
				t.Fatalf("the answers opened an approval request: %s", event.Payload)
			}
		}
	}
}

// An agent's edit through the edits route versions the document in its own transaction, and a
// person's answer given just before it lands in no version: it is held on the ask's node and its
// row (R3). So the version the edit writes holds the edit and no answer, and the edit, inside
// another block's question, supersedes the approval as any further character does (R10: "A write
// that changes one further character supersedes the approval as it does today").
func TestAnAgentEditBesideAnAnswerSupersedesTheApprovalAndItsVersionHoldsNoAnswer(t *testing.T) {
	handler, _, documents := settlingCarryDocuments(t, time.Second)
	issue := createInteractionIssue(t, handler, "TEST", "Answer beside an edit", "Context\n")
	insertDecisionMarkdown(t, handler, issue.PrimaryArtifactID, waveAsk+"\n"+regionAsk, 2)
	asks := settledDecisionBlocks(t, handler, documents, issue.Key, issue.PrimaryArtifactID,
		map[string]string{"wave": "How many regions take the first wave?", "region": "Which region goes first?"})
	approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 2)

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	reviseDocument(t, handler, issue.PrimaryArtifactID, "Which region goes first?", "Which cloud region goes first?", 3)
	markdown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 3)
	if !strings.Contains(markdown, "Which cloud region goes first?") {
		t.Fatalf("version 3 = %s, want it to hold the agent's edit", markdown)
	}
	requireNoAnswerAttributes(t, "version 3", markdown)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
	if ask := readBlockAsk(t, handler, asks["wave"]); ask.State != "answered" || ask.Answer == nil || ask.Answer.User != "alice" {
		t.Fatalf("the answered block's ask = state %q answer %#v, want alice's answer on it", ask.State, ask.Answer)
	}
}

// versionMarkdown is the markdown of the document's version number.
func versionMarkdown(t *testing.T, handler http.Handler, artifactID string, number int) string {
	t.Helper()
	version := dispatchRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", artifactID, number), nil, "alice")
	if version.Code != http.StatusOK {
		t.Fatalf("read version %d: status=%d body=%s", number, version.Code, version.Body.String())
	}
	return decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, version).Markdown
}

// Stale, then restored: the agent rewords a block the human answered. The human answered the first
// block, which writes no version (R3); the agent added a second, and the human approved that
// version and answered the second block, which writes none either. The agent's edit of the first
// block's question, through the edits route, versions in the route's own transaction: it shows his
// answer's block under wording nobody decided, a further character his approval does not hold, so
// nothing is carried (R10: "A write that changes one further character supersedes the approval").
// Settlement then writes the decided wording back, and that version is the one he approved byte
// for byte, which carries his approval (docs.restoresApprovedText).
func TestARewordOfAnAnsweredBlockDoesNotCarryAndTheDecidedWordingWrittenBackDoes(t *testing.T) {
	handler, _, documents := settlingCarryDocuments(t, time.Second)
	issue := createInteractionIssue(t, handler, "TEST", "Reword under the answer", "Context\n\n"+regionAsk)
	asks := settledDecisionBlocks(t, handler, documents, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
	insertDecisionMarkdown(t, handler, issue.PrimaryArtifactID, waveAsk, 2)
	for id, ask := range settledDecisionBlocks(t, handler, documents, issue.Key, issue.PrimaryArtifactID, map[string]string{"wave": "How many regions take the first wave?"}) {
		asks[id] = ask
	}
	approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 2)

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	reviseDocument(t, handler, issue.PrimaryArtifactID, "Which region goes first?", "Which region goes last?", 3)
	markdown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 3)
	if !strings.Contains(markdown, "Which region goes last?") {
		t.Fatalf("version 3 = %s, want it to hold the agent's reword", markdown)
	}
	requireNoAnswerAttributes(t, "version 3", markdown)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")

	settledVersion(t, handler, documents, issue.PrimaryArtifactID, 4)
	if restored, approved := versionMarkdown(t, handler, issue.PrimaryArtifactID, 4), versionMarkdown(t, handler, issue.PrimaryArtifactID, 2); restored != approved || strings.Contains(restored, "goes last?") {
		t.Fatalf("version 4 = %s, want settlement to have written the decided wording back, giving the approved version 2's text %s", restored, approved)
	}
	requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 4, 2, restoreSentence(2), nil)
}

// Stale: a server-owned attribute forged on a decided block - each of the eight a block of the
// release before held (askServerAttributeNames), forged alone - in a version a person approved
// the document before. The human answered the first block, which writes no version (R3); the
// agent added a second, and the human approved that version and answered the second block, which
// writes none either. A browser's socket edit then puts a value on the first block, and the agent
// names a version, in the route's own transaction, before settlement runs, while a process of the
// release before serves: the one fleet whose version writes keep the attributes a version can
// store, since on a fleet of one build a version written before settlement takes them off (docs'
// storableVersionTree). That version's block shows a state its row does not hold, so it does not
// agree with its node and nothing is carried (R10: "Every ask of the document must still agree
// with its node"). Once the release before's process has left, settlement takes the forgery off
// the block, as it takes each of the seven off a block once the fleet is one build, and that
// version is the one he approved byte for byte, which carries his approval
// (docs.restoresApprovedText); for `invalid`, which no rendering writes and no version write takes
// off, settlement's repair changes no markdown and versions nothing, so the approval stays stale.
func TestAForgedServerAttributeOnADecidedBlockCarriesNothing(t *testing.T) {
	for _, tc := range []struct {
		attribute string
		forged    any
		rendered  string // what version 3's markdown shows for the forgery; "" for an attribute no rendering writes
	}{
		{"state", "resolved", `state="resolved"`},
		{"answered_by", "mallory", `answered_by="mallory"`},
		{"answered_at", "2026-01-01T00:00:00Z", `answered_at="2026-01-01T00:00:00Z"`},
		{"selected", []string{"West"}, `selected="[&#x22;West&#x22;]"`},
		{"answer", "Pick West.", `answer="Pick West."`},
		{"invalid", "forged", ""},
		{"copied_from", "00000000-0000-0000-0000-000000000000", `copied_from="00000000-0000-0000-0000-000000000000"`},
		{"copied_from_document", "dispatch://TEST/artifact/other", `copied_from_document="dispatch://TEST/artifact/other"`},
	} {
		t.Run(tc.attribute, func(t *testing.T) {
			handler, database, documents := settlingCarryDocuments(t, time.Second)
			issue := createInteractionIssue(t, handler, "TEST", "Forged "+tc.attribute+" under the answer", "Context\n\n"+regionAsk)
			asks := settledDecisionBlocks(t, handler, documents, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
			answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
			insertDecisionMarkdown(t, handler, issue.PrimaryArtifactID, waveAsk, 2)
			for id, ask := range settledDecisionBlocks(t, handler, documents, issue.Key, issue.PrimaryArtifactID, map[string]string{"wave": "How many regions take the first wave?"}) {
				asks[id] = ask
			}
			approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 2)

			answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
			setLiveBlockAttributes(t, handler, documents, issue.PrimaryArtifactID, "region", map[string]any{tc.attribute: tc.forged})
			whileTheReleaseBeforeServes(t, database, func() {
				named := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]any{"summary": "Name the forged " + tc.attribute, "actor": sessionActor()})
				if named.Code != http.StatusCreated {
					t.Fatalf("name the version holding the forged %s: status=%d body=%s", tc.attribute, named.Code, named.Body.String())
				}
			})
			waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
			markdown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 3)
			if !strings.Contains(markdown, "How many regions take the first wave?") {
				t.Fatalf("version 3 = %s, want it to hold the second block", markdown)
			}
			if tc.rendered != "" && !strings.Contains(markdown, tc.rendered) {
				t.Fatalf("version 3 = %s, want it to show the forged %s", markdown, tc.attribute)
			}
			requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")

			if tc.rendered == "" {
				// Settlement takes the forged attribute off the live block, but a repair no rendering
				// writes changes no markdown, so it versions nothing: the approval stays stale and the
				// forgery costs the carry, which the human's next approval replaces. Nothing is approved.
				settledWithoutVersion(t, database, documents, issue.PrimaryArtifactID)
				if latest := readApproval(t, handler, issue.PrimaryArtifactID).Approval.LatestVersion; latest != 3 {
					t.Fatalf("latest version = %d, want settlement's repair of %s to write none", latest, tc.attribute)
				}
				requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
				return
			}
			settledVersion(t, handler, documents, issue.PrimaryArtifactID, 4)
			if restored, approved := versionMarkdown(t, handler, issue.PrimaryArtifactID, 4), versionMarkdown(t, handler, issue.PrimaryArtifactID, 2); restored != approved {
				t.Fatalf("version 4 = %s, want settlement to have taken the forged %s off, giving the approved version 2's text %s", restored, tc.attribute, approved)
			}
			requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 4, 2, restoreSentence(2), nil)
		})
	}
}

// setLiveBlockAttributes writes attributes onto the typed block blockID of the live document as a
// browser's socket edit does: a document-socket peer sets them on the block in its own tree, which
// no route's checks hold, and stays connected for the rest of the test, as an open editor does. No
// server write puts a block's attributes on it alone any more: an answer is held on the ask's node
// alone, and the roll window's copy is settlement's (docs' rollWindowAskAttributes).
func setLiveBlockAttributes(t *testing.T, handler http.Handler, documents *docs.Service, artifactID, blockID string, attributes map[string]any) {
	t.Helper()
	peer := connectBrowserPeer(t, handler, documents, artifactID)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		var block *pmdoc.Node
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id == blockID {
				block = node
			}
			return block == nil
		})
		if block == nil {
			return fmt.Errorf("the live document holds no block %q", blockID)
		}
		for name, value := range attributes {
			block.Attrs[name] = value
		}
		return nil
	})
}

// settledVersion settles the document at once and waits until version number reads: the version a
// write's settlement writes, without waiting out the settle window the write armed. The settlement
// is its room's (artifactRoom), the issue room a converted document is edited in.
func settledVersion(t *testing.T, handler http.Handler, documents *docs.Service, artifactID string, number int) {
	t.Helper()
	documents.SettleNow(artifactRoom(t, handler, artifactID))
	waitForArtifactVersion(t, handler, artifactID, number)
}

// settledDecisionBlocks settles the document's room at once and returns its blocks' ask ids as
// indexedDecisionBlocks does, once settlement has indexed them.
func settledDecisionBlocks(t *testing.T, handler http.Handler, documents *docs.Service, issueKey, artifactID string, blocks map[string]string) map[string]string {
	t.Helper()
	documents.SettleNow(artifactRoom(t, handler, artifactID))
	return indexedDecisionBlocks(t, handler, issueKey, artifactID, blocks)
}

// settledWithoutVersion settles the document's room at once, for a document whose settlement is
// owed, and waits until that settlement has committed: its pending-settlement row, which a live
// write leaves and only a settlement's commit clears, is gone. The row is keyed by the room the
// document is edited in (documentRoomOf), as the settlement that clears it is. It fails t
// when the document owes no settlement, since then there would be nothing to wait for.
func settledWithoutVersion(t *testing.T, database *store.Store, documents *docs.Service, artifactID string) {
	t.Helper()
	room, _ := documentRoomOf(t, database.Pool, artifactID)
	owed := func() bool {
		var pending bool
		if err := database.Pool.QueryRow(context.Background(), `select exists(select 1 from doc_settlements_pending where artifact_id = $1)`, room).Scan(&pending); err != nil {
			t.Fatalf("read the document's pending settlement: %v", err)
		}
		return pending
	}
	if !owed() {
		t.Fatal("the document owes no settlement, want the live write's settlement still owed")
	}
	documents.SettleNow(room)
	for deadline := time.Now().Add(5 * time.Second); owed(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the document's settlement never committed")
		}
	}
}

// Stale: the agent changed a paragraph outside the blocks after his approval and before his answer.
// The paragraph edit supersedes the approval as any further character does (R10), and the answer,
// which writes no version (R3), changes nothing of that: the approval stays stale.
func TestAParagraphEditBeforeTheAnswerDoesNotCarry(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "Paragraph changed", "The service ships behind a flag.\n")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 2)
	reviseDocument(t, handler, issue.PrimaryArtifactID, "behind a flag.", "behind a flag and a canary.", 3)

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	requireNoVersionPast(t, handler, database, issue.PrimaryArtifactID, 3)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}

// Approved: answers by humans other than the approver - here alice's answer to one block and bob's
// own to the other, on a document only bob approved - write no version, so his approval stands
// at the latest version through both - a person's answer changes no byte of any document, and an
// approval of that document stands through it - whoever answered.
func TestAnswersByHumansOtherThanTheApproverWriteNoVersionAndTheApprovalStands(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "Approved by another", "Context\n\n"+waveAsk+"\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID,
		map[string]string{"wave": "How many regions take the first wave?", "region": "Which region goes first?"})
	approveVersion(t, handler, issue.PrimaryArtifactID, "bob", 1)

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	answerDecisionBlock(t, handler, asks["region"], "bob", []string{"East"}, "", nil)
	requireNoVersionPast(t, handler, database, issue.PrimaryArtifactID, 1)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "approved")
	got := readApproval(t, handler, issue.PrimaryArtifactID)
	if got.Approval.Version == nil || *got.Approval.Version != 1 || got.Approval.LatestVersion != 1 || got.Approval.By == nil || got.Approval.By.ID != "bob" {
		t.Fatalf("approval after both answers = %s, want bob's approval standing at version 1, the latest", describeApproval(got))
	}
	for block, answerer := range map[string]string{"wave": "alice", "region": "bob"} {
		if ask := readBlockAsk(t, handler, asks[block]); ask.State != "answered" || ask.Answer == nil || ask.Answer.User != answerer {
			t.Fatalf("the %s block's ask = state %q answer %#v, want %s's answer", block, ask.State, ask.Answer, answerer)
		}
	}
}

// Changes requested: the answerer's latest review of the document asks for changes, and his answer
// writes no version (R3), so the request for changes stands and nothing is approved.
func TestAnAnswerByAHumanWhoseLatestReviewRequestsChangesDoesNotCarry(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "Answerer requested changes", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "changes_requested", "Name the rollback path.")

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	requireNoVersionPast(t, handler, database, issue.PrimaryArtifactID, 2)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 2, "changes_requested")
}

// Changes requested: a second human asked for changes after the answerer's approval, and the
// answer writes no version (R3), so the request for changes stands and nothing is approved over it.
func TestAnAnswerAfterAnotherHumanRequestedChangesDoesNotCarry(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "Another requested changes", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	reviewDocument(t, handler, issue.PrimaryArtifactID, "bob", "changes_requested", "Name the rollback path.")

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	requireNoVersionPast(t, handler, database, issue.PrimaryArtifactID, 2)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 2, "changes_requested")
}

// The human deletes a block he answered, whose document he then approved, and the agent inserts it
// again with its id and wording. Its row stays answered - settlement retracts only an open ask -
// and on a fleet of one build no block holds an answer state (R3), so the inserted block agrees
// with its row and the insert's version is the approved version's text byte for byte: it carries
// his approval (docs.restoresApprovedText). An agent reading the text reads his answer on the
// block again, rendered from the store, and settlement writes nothing back into the block, so no
// version follows; the delete's version carried nothing. The agent cannot make the delete
// itself: no agent takes a person's answered decision out of a document, so its delete is refused
// and writes no version.
func TestADeletedAndRestoredAnsweredBlockIsApprovedOnceItsTextIsTheApprovedText(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "Answered block restored", "Context\n\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
	approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 1)
	deleteRegion := map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "delete", "block": "region"}},
	}
	if refused := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", deleteRegion); refused.Code != http.StatusConflict ||
		decodeBody[errorBody](t, refused).Code != codeAnsweredAskKept {
		t.Fatalf("the agent deletes the answered block: status=%d body=%s, want 409 %s", refused.Code, refused.Body.String(), codeAnsweredAskKept)
	}
	requireNoVersionPast(t, handler, database, issue.PrimaryArtifactID, 1)
	deleted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", deleteRegion, "alice")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete the answered block: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")

	restored := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": regionAsk}},
	})
	if restored.Code != http.StatusOK {
		t.Fatalf("insert the answered block again: status=%d body=%s", restored.Code, restored.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	if restoredText, approvedText := versionMarkdown(t, handler, issue.PrimaryArtifactID, 3), versionMarkdown(t, handler, issue.PrimaryArtifactID, 1); restoredText != approvedText {
		t.Fatalf("version 3 = %s, want the approved version 1's text, %s", restoredText, approvedText)
	}
	if ask := readBlockAsk(t, handler, asks["region"]); ask.State != "answered" || ask.Answer == nil || ask.Answer.User != "alice" {
		t.Fatalf("the block's ask after the delete and the insert = state %q answer %#v, want alice's answer kept", ask.State, ask.Answer)
	}
	awaitTextShows(t, handler, issue.PrimaryArtifactID, "the row's answer on the restored block",
		contains(`:::ask{#region urgency="low" multiple="false"}`, `#region state="answered" answered_by="alice"`), nil)
	requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 3, 1, restoreSentence(1), nil)
	requireNoVersionPast(t, handler, database, issue.PrimaryArtifactID, 3)
	if carried := documentReviews(t, handler, issue.PrimaryArtifactID); len(carried) != 2 || carried[0].Version != 3 || carried[1].Version != 1 {
		t.Fatalf("reviews = %+v, want his approval of version 1 and the carry to version 3 alone: the delete's version carried nothing", carried)
	}
}

// A record copy of the spec holds the spec's block as a copy of its ask, which settlement keeps on
// the store and no stored version of the copy holds (R3: "copied_from is a tuple from the copy to
// the ask it was copied from"); an agent reading the copy's text reads the block as a copy of the
// spec's ask, rendered from the store. The human approved both documents; his answer to the
// spec's ask writes no version of either, so both approvals stand at their latest versions, and
// the copy's text then shows his answer on its block.
func TestAnAnswerThroughACopysSourceWritesNoVersionOfEitherDocument(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "Copied block", "Context\n\n"+waveAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"wave": "How many regions take the first wave?"})
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": documentMarkdown(t, handler, issue.PrimaryArtifactID),
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the record copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	awaitTextShows(t, handler, copied, "its open source", contains(`state="open" copied_from="`+asks["wave"]+`"`), nil)
	awaitSettled(t, database, copied)
	copyVersions := documentVersions(t, handler, copied)
	copyLatest := copyVersions[len(copyVersions)-1].Number
	requireNoAnswerAttributes(t, "the copy's latest version", versionMarkdown(t, handler, copied, copyLatest))
	approveVersion(t, handler, copied, "alice", copyLatest)
	approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 1)

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	awaitTextShows(t, handler, copied, "its source's answer", contains(`state="answered" answered_by="alice"`, `copied_from="`+asks["wave"]+`"`), nil)
	requireNoVersionPast(t, handler, database, issue.PrimaryArtifactID, 1)
	requireNoVersionPast(t, handler, database, copied, copyLatest)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "approved")
	requireNoCarry(t, handler, copied, 1, "approved")
}

// The version write that carries an approval writes one ask's parts twice: it moves the open
// request to the new version, which rewords its question, and the carry then answers it. The
// second plan reads the rows the first queued, so each write is a revision of its own: the
// question moves to revision 2 and the answer arrives at revision 3, and the question's part
// records the time of the rewording that moved it, not the time of the answer. The version is the
// agent's undo of its own edit, which holds the approved text (docs.restoresApprovedText): an
// answer writes no version here, so no answer carries.
func TestTheCarrysAnswerAndTheRequestsMoveTakeARevisionEach(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "Request moved and answered at once", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	reviseDocument(t, handler, issue.PrimaryArtifactID, "Context", "Context, revised", 2)
	request := requestApproval(t, handler, issue.PrimaryArtifactID)
	if request.Approval.Version != 2 {
		t.Fatalf("request = %#v, want one at version 2", request.Approval)
	}
	requireAskParts(t, database, request.ID, "the request", "b1@1 b2@1 b3@1", 1)

	reviseDocument(t, handler, issue.PrimaryArtifactID, "Context, revised", "Context", 3)
	answered := readBlockAsk(t, handler, request.ID)
	if answered.State != "answered" || answered.Approval == nil || answered.Approval.Version != 3 {
		t.Fatalf("request after the carry = state %q approval %#v, want it answered at version 3", answered.State, answered.Approval)
	}
	requireAskParts(t, database, request.ID, "the carry", "b1@2 b2@1 b3@1 b4@3", 3)
	if answered.EditedAt == nil {
		t.Fatal("the request carries no edited_at after the move")
	}
	edited, err := time.Parse(time.RFC3339Nano, *answered.EditedAt)
	if err != nil {
		t.Fatalf("read the request's edited_at %q: %v", *answered.EditedAt, err)
	}
	if changed := askPartChangedAt(t, database, request.ID, "b1"); !changed.Equal(edited) {
		t.Fatalf("the question part changed at %v, want the rewording's own time %v", changed, edited)
	}
}

// requestApprovalWithVerification opens the asking agent's approval request on the document and
// the issue's Verification document together, and returns the ask the route answers with.
func requestApprovalWithVerification(t *testing.T, handler http.Handler, artifactID string) carriedAsk {
	t.Helper()
	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests",
		map[string]any{"actor": sessionActor(), "include_verification": true})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval of both documents: status=%d body=%s", requested.Code, requested.Body.String())
	}
	return decodeBody[struct {
		Ask carriedAsk `json:"ask"`
	}](t, requested).Ask
}

// A request that covers a spec and its issue's Verification document is answered only once both
// stand approved at the versions it names. A carry on the spec - here the version that undoes the
// agent's edit, which holds the approved text (docs.restoresApprovedText) - writes the spec's
// review in the human's name and leaves the request open, because nobody has approved the
// Verification document; his approval of that document then answers the request, reviewing both.
func TestACarryOnOneDocumentOfATwoDocumentRequestLeavesItOpen(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Carry one of two", "Context\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", "# Verification\n\nOne.\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	reviseDocument(t, handler, issue.PrimaryArtifactID, "Context", "Context, revised", 2)
	request := requestApprovalWithVerification(t, handler, issue.PrimaryArtifactID)
	if request.Approval.Version != 2 || request.Approval.Approved == nil || request.Approval.Approved.Version != 1 {
		t.Fatalf("request = %#v, want one at spec version 2 following the approval of version 1", request.Approval)
	}

	// The undo restores the text he approved, so the spec's review is carried; the Verification
	// document, which nobody has approved, keeps the request open.
	reviseDocument(t, handler, issue.PrimaryArtifactID, "Context, revised", "Context", 3)
	requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 3, 1, restoreSentence(1), &request.ID)
	if still := readBlockAsk(t, handler, request.ID); still.State != "open" {
		t.Fatalf("request after the carry = %q, want it still open for the Verification document", still.State)
	}
	requireNoCarry(t, handler, verification.Artifact.ID, 0, "awaiting")

	// Approving the Verification document answers that request, and reviews both documents at the
	// versions it names.
	reviewDocument(t, handler, verification.Artifact.ID, "alice", "approved", "")
	answered := readBlockAsk(t, handler, request.ID)
	if answered.State != "answered" {
		t.Fatalf("request after the Verification document's approval = %q, want answered", answered.State)
	}
	for _, both := range []struct {
		artifactID string
		version    int
	}{{issue.PrimaryArtifactID, 3}, {verification.Artifact.ID, 1}} {
		got := readApproval(t, handler, both.artifactID)
		if got.Approval == nil || got.Approval.State != "approved" || got.Approval.Version == nil || *got.Approval.Version != both.version {
			t.Fatalf("approval of %s = %#v, want approved at version %d", both.artifactID, got.Approval, both.version)
		}
	}
}

// The mirror: the spec already stands approved at the version the request names, so a carry on
// the Verification document - the version undoing the agent's edit to it, which holds the text he
// approved - answers that request in the same transaction, as a carry on a one-document request
// does.
func TestACarryOnTheVerificationDocumentAnswersTheRequestWhenTheSpecStandsApproved(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Carry the verification", "Context\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", "# Verification\n\nOne.\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	reviewDocument(t, handler, verification.Artifact.ID, "alice", "approved", "")
	reviseDocument(t, handler, verification.Artifact.ID, "One.", "One, revised.", 2)
	request := requestApprovalWithVerification(t, handler, issue.PrimaryArtifactID)
	if request.Approval.Version != 1 {
		t.Fatalf("request = %#v, want one at spec version 1", request.Approval)
	}

	reviseDocument(t, handler, verification.Artifact.ID, "One, revised.", "One.", 3)
	requireReviewCarriedWith(t, handler, verification.Artifact.ID, 3, 1, restoreSentence(1), &request.ID)
	answered := readBlockAsk(t, handler, request.ID)
	if answered.State != "answered" || answered.Answer == nil || answered.Answer.User != "alice" ||
		!reflect.DeepEqual(answered.Answer.Selected, []string{"Approve"}) {
		t.Fatalf("request after the carry = state %q answer %#v, want Approve by alice", answered.State, answered.Answer)
	}
}
