package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func contentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// recordedDigest is the digest of the record of query of the issue issueKey that version number of
// the document artifactID was stated at: the record docs builds (docs.ReadQueryRecord) at the cut
// the pass that stated the version recorded on its version_renders row, its time and its seq. A
// review of such a version pins that digest (artifact_version_text_sha256).
func recordedDigest(t *testing.T, database *store.Store, issueKey, query, artifactID string, number int) string {
	t.Helper()
	ctx := context.Background()
	var cut refs.TreeCut
	if err := database.Pool.QueryRow(ctx, `select at, read_seq from version_renders where artifact_id = $1 and number = $2`,
		artifactID, number).Scan(&cut.At, &cut.ReadSeq); err != nil {
		t.Fatalf("read the cut version %d of %s was stated at: %v", number, artifactID, err)
	}
	record, err := docs.ReadQueryRecord(ctx, database.Pool, issueKey, query, cut)
	if err != nil {
		t.Fatalf("read the %s record of %s at the cut of version %d: %v", query, issueKey, number, err)
	}
	return record.Digest
}

func contentReviews(t *testing.T, handler http.Handler, id string) []model.ArtifactReview {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+id+"/reviews", nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("reviews: %d %s", read.Code, read.Body)
	}
	return decodeBody[[]model.ArtifactReview](t, read)
}

// Dropping the pin from any review/approval/version/event surface breaks this test. The issue is
// born converted, so each version of its spec holds the version_renders row its pass recorded, and a
// review of it pins its record's digest (recordedDigest); the version read names its stored bytes.
func TestEveryApprovalReadNamesTheApprovedBytes(t *testing.T) {
	t.Parallel()
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "HASH", "Approve bytes", "Approve me ☕\n")
	id := issue.PrimaryArtifactID
	h1 := contentHash(versionMarkdown(t, handler, id, 1))
	d1 := recordedDigest(t, database, issue.Key, graphstore.QuerySpec, id, 1)
	reviewDocument(t, handler, id, "alice", "approved", "")
	reviews := contentReviews(t, handler, id)
	if len(reviews) != 1 || reviews[0].ContentSHA256 == nil || *reviews[0].ContentSHA256 != d1 || reviews[0].ContentSHA256Source == nil || *reviews[0].ContentSHA256Source != "insert" {
		t.Fatalf("reviews = %#v", reviews)
	}
	checkRead := func(state string, version int, hash string) {
		t.Helper()
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+id, nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("read: %d %s", read.Code, read.Body)
		}
		artifact := decodeBody[model.Artifact](t, read)
		approval := artifact.Approval
		if approval == nil || approval.State != state || approval.ContentSHA256 == nil || *approval.ContentSHA256 != d1 || approval.ContentSHA256Source == nil || *approval.ContentSHA256Source != "insert" {
			t.Fatalf("approval = %#v", approval)
		}
		read = dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+id+"/versions/"+strconv.Itoa(version), nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("version: %d %s", read.Code, read.Body)
		}
		got := decodeBody[model.Version](t, read)
		if got.SHA256 == nil || *got.SHA256 != hash {
			t.Fatalf("version sha256 = %v, want %s", got.SHA256, hash)
		}
	}
	checkRead("approved", 1, h1)
	found := false
	for _, event := range issueEvents(t, handler, issue.Key) {
		if event.Type != "artifact.approved" {
			continue
		}
		var payload model.ArtifactReviewEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ContentSHA256 == nil || *payload.ContentSHA256 != d1 || payload.ContentSHA256Source == nil || *payload.ContentSHA256Source != "insert" {
			t.Fatalf("event = %#v", payload)
		}
		found = true
	}
	if !found {
		t.Fatal("no artifact.approved event")
	}
	reviseDocument(t, handler, id, "Approve me ☕", "Changed.", 2)
	h2 := contentHash(versionMarkdown(t, handler, id, 2))
	if h1 == h2 {
		t.Fatal("fixture hashes do not differ")
	}
	d2 := recordedDigest(t, database, issue.Key, graphstore.QuerySpec, id, 2)
	checkRead("stale", 2, h2)
	requested := reviewDocument(t, handler, id, "alice", "changes_requested", "Please revise.")
	if requested.ContentSHA256 == nil || *requested.ContentSHA256 != d2 {
		t.Fatalf("changes request = %#v", requested)
	}
	for _, event := range issueEvents(t, handler, issue.Key) {
		if event.Type != "artifact.changes_requested" {
			continue
		}
		var payload model.ArtifactReviewEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ContentSHA256 == nil || *payload.ContentSHA256 != d2 || payload.ContentSHA256Source == nil || *payload.ContentSHA256Source != "insert" {
			t.Fatalf("changes event = %#v", payload)
		}
		return
	}
	t.Fatal("no artifact.changes_requested event")
}

// A carry pins its destination bytes, not the source approval's bytes. The carry here is R10's
// attribute case while two builds serve: the answer is written into the block for the release
// before's pages (docs' rollWindowAskAttributes), so the version it settles into differs from the
// approved one in the block's server-owned attributes alone, and its bytes are not the approved
// version's. The issue is created while a process of the release before serves, so it has not
// converted: its spec's node is no query node, and the version the carry settles into, which this
// build writes, holds no version_renders row. It answers the sha256 of its stored markdown as its
// digest, and the carried review pins that.
func TestACarriedApprovalPinsTheVersionItCarriesTo(t *testing.T) {
	t.Parallel()
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	registerTwoBuilds(t, database)
	issue := createInteractionIssue(t, handler, "CARR", "Carry bytes", "Context\n\n"+waveAsk)
	id := issue.PrimaryArtifactID
	asks := indexedDecisionBlocks(t, handler, issue.Key, id, map[string]string{"wave": "How many regions take the first wave?"})
	awaitVersionText(t, handler, id, "the block written in step with its open ask",
		contains(`:::ask{#wave urgency="med" multiple="false" state="open"}`))
	awaitSettled(t, database, id)
	approved := latestVersion(t, database, id).Number
	approveVersion(t, handler, id, "alice", approved)
	h1 := contentHash(versionMarkdown(t, handler, id, approved))
	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "Two regions first.", nil)
	waitForArtifactVersion(t, handler, id, approved+1)
	requireReviewCarriedWith(t, handler, id, approved+1, approved, attributeRemovalSentence(approved), nil)
	h2 := contentHash(versionMarkdown(t, handler, id, approved+1))
	carried := "version " + strconv.Itoa(approved+1) + " of a spec that has not converted, which the carry settles into"
	requireVersionDigest(t, handler, carried, id, approved+1, h2, h2)
	requireNoVersionRender(t, database, carried, id, approved+1)
	reviews := contentReviews(t, handler, id)
	if h2 == h1 || len(reviews) != 2 || reviews[0].ContentSHA256 == nil || *reviews[0].ContentSHA256 != h2 || reviews[0].ContentSHA256Source == nil || *reviews[0].ContentSHA256Source != "insert" {
		t.Fatalf("carried reviews = %#v; hashes %s / %s", reviews, h1, h2)
	}
}

// A document's version answers digest, what an approval of it covers, on the version read and on
// the artifact read's version list alike. The issue is born converted, so version 1 of its spec, a
// query version the create wrote, holds the version_renders row its pass recorded, and answers its
// record's digest (recordedDigest), which is not the sha256 of its markdown. An upload of notes.md,
// which no query covers, and a version of the spec written straight into artifact_versions as an
// older build wrote one, which no pass has stated, each answer the sha256 of their stored
// markdown. A review of version 1 pins that version's digest, as its approval on the artifact read
// names it. A query's record covers only a query version this build's version write made, so the
// older build's version takes no row when a pass states it: with the spec marked owed as the boot's
// backfill marks a document no pass of this rule stated, the reconciliation states version 2, and
// the version still answers the sha256 of its stored markdown and holds no row. Nor does a version
// this build writes through the edit route of the spec of an issue that has not converted, whose
// document's node is no query node: it answers the sha256 of its stored markdown, which a person's
// approval of it pins. The version read's sha256 names the bytes it answers in every case.
func TestAVersionAnswersTheDigestAnApprovalCovers(t *testing.T) {
	handler, database, documents := settlingCarryDocuments(t, 20*time.Millisecond)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "DGST", "A version's digest", revisedSpec)
	spec := issue.PrimaryArtifactID
	specBytes := contentHash(versionMarkdown(t, handler, spec, 1))
	recorded := recordedDigest(t, database, issue.Key, graphstore.QuerySpec, spec, 1)
	if recorded == specBytes {
		t.Fatalf("version 1 of the spec records %s, the sha256 of its markdown, want its record's digest", recorded)
	}
	digest := requireVersionDigest(t, handler, "version 1 of the converted spec", spec, 1, recorded, specBytes)

	notesMarkdown := "# Notes\n\nNo query covers this document.\n"
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "notes.md", "content": notesMarkdown,
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload notes.md: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	if notes.Kind != "doc" {
		t.Fatalf("notes.md is a %s, want a document", notes.Kind)
	}
	notesBytes := contentHash(versionMarkdown(t, handler, notes.ID, 1))
	requireVersionDigest(t, handler, "version 1 of notes.md", notes.ID, 1, notesBytes, notesBytes)

	review := reviewDocument(t, handler, spec, "alice", "approved", "")
	if review.Version != 1 || review.ContentSHA256 == nil || *review.ContentSHA256 != digest {
		t.Fatalf("alice's approval of version %d pins %s, want version 1's digest %s", review.Version, deref(review.ContentSHA256), digest)
	}
	read := decodeBody[model.Artifact](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+spec, nil, "alice"))
	if approval := read.Approval; approval == nil || approval.ContentSHA256 == nil || *approval.ContentSHA256 != digest {
		t.Fatalf("the spec's approval = %#v, want it to pin version 1's digest %s", approval, digest)
	}

	// The older build's version holds the bytes the spec's room holds, version 1's stored markdown,
	// so a settlement of the room that runs while the reconciliation below does finds nothing to
	// write as a version of its own, which its pass would state in place of version 2.
	var older string
	if err := database.Pool.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1 and number = 1`, spec).Scan(&older); err != nil {
		t.Fatalf("read version 1's stored markdown: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select artifact_id, 2, $2, authors from artifact_versions where artifact_id = $1 and number = 1
	`, spec, older); err != nil {
		t.Fatalf("write version 2 as an older build would: %v", err)
	}
	requireVersionDigest(t, handler, "version 2 of the spec, which no pass stated", spec, 2, contentHash(older), contentHash(older))

	if _, err := database.Pool.Exec(ctx, `
		update document_block_versions set store_owed_at = now(), stated_version = null, stated_rule = null where artifact_id = $1`, spec); err != nil {
		t.Fatalf("mark the spec owed as the backfill marks a document no pass of this rule stated: %v", err)
	}
	for passes := 1; ; passes++ {
		if _, err := documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
			t.Fatalf("pass %d of the block-index reconciliation: %v", passes, err)
		}
		owed := readCoverage(t, handler, issue.Key).OwedDocuments
		if len(owed) == 0 {
			break
		}
		if passes == 8 {
			t.Fatalf("eight passes of the reconciliation left %s owing %+v", issue.Key, owed)
		}
	}
	var stated string
	if err := database.Pool.QueryRow(ctx, `select coalesce(stated_version::text, 'none') from document_block_versions where artifact_id = $1`, spec).Scan(&stated); err != nil {
		t.Fatalf("read the version the reconciliation stated the spec at: %v", err)
	}
	if stated != "2" {
		t.Fatalf("the reconciliation left the spec stated at version %s, want version 2, the older build's", stated)
	}
	reconciled := "version 2 of the spec, which the reconciliation stated"
	requireVersionDigest(t, handler, reconciled, spec, 2, contentHash(older), contentHash(older))
	requireNoVersionRender(t, database, reconciled, spec, 2)

	unconverted := createDigestIssue(t, handler, database, "DGST", "A spec that has not converted", revisedSpec, true)
	written := unconverted.PrimaryArtifactID
	reviseDocument(t, handler, written, "The spec's opening words.", "The spec's first words.", 2)
	edited := newestQueryVersion(t, database, written)
	if edited.number != 2 {
		t.Fatalf("the edit left the spec of %s at version %d, want version 2", unconverted.Key, edited.number)
	}
	editedBytes := contentHash(edited.markdown)
	unconvertedEdit := "version 2 of the spec of an issue that has not converted, which the edit route wrote"
	requireVersionDigest(t, handler, unconvertedEdit, written, 2, editedBytes, editedBytes)
	requireNoVersionRender(t, database, unconvertedEdit, written, 2)
	if review := reviewDocument(t, handler, written, "alice", "approved", ""); review.Version != 2 || review.ContentSHA256 == nil || *review.ContentSHA256 != editedBytes {
		t.Errorf("alice's approval of version %d of the spec of %s pins %q, want version 2's digest, the sha256 of its stored markdown, %s",
			review.Version, unconverted.Key, deref(review.ContentSHA256), editedBytes)
	}
}

// requireNoVersionRender fails t unless version number of the document artifactID holds no
// version_renders row, naming the digest a row it holds records.
func requireNoVersionRender(t *testing.T, database *store.Store, what, artifactID string, number int) {
	t.Helper()
	var digest string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce((select record_sha256 from version_renders where artifact_id = $1 and number = $2), '')`,
		artifactID, number).Scan(&digest); err != nil {
		t.Fatalf("%s: read the version_renders row of version %d: %v", what, number, err)
	}
	if digest != "" {
		t.Errorf("%s: version %d holds a version_renders row recording the digest %s, want none", what, number, digest)
	}
}

// requireVersionDigest fails unless version number of the document artifactID answers digest as
// its digest on the version read and on the artifact read's version list, and bytes as the
// version read's sha256, naming the read that differs; it answers the digest the version read
// answers.
func requireVersionDigest(t *testing.T, handler http.Handler, what, artifactID string, number int, digest, bytes string) string {
	t.Helper()
	version := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/versions/"+strconv.Itoa(number), nil, "alice")
	if version.Code != http.StatusOK {
		t.Fatalf("%s: the version read: status=%d body=%s", what, version.Code, version.Body.String())
	}
	answered := decodeBody[model.Version](t, version)
	if answered.Digest == nil || *answered.Digest != digest {
		t.Errorf("%s: the version read answers digest %q, want %s", what, deref(answered.Digest), digest)
	}
	if answered.SHA256 == nil || *answered.SHA256 != bytes {
		t.Errorf("%s: the version read answers sha256 %q, want %s", what, deref(answered.SHA256), bytes)
	}
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("%s: the artifact read: status=%d body=%s", what, read.Code, read.Body.String())
	}
	listed := -1
	versions := decodeBody[model.Artifact](t, read).Versions
	for index := range versions {
		if versions[index].Number == number {
			listed = index
		}
	}
	if listed < 0 {
		t.Errorf("%s: the artifact read's version list holds no version %d", what, number)
	} else if listedDigest := versions[listed].Digest; listedDigest == nil || *listedDigest != digest {
		t.Errorf("%s: the artifact read's version list answers digest %q, want %s", what, deref(listedDigest), digest)
	}
	return deref(answered.Digest)
}

// restoreEvents is what the issue's event log holds of version number of the document artifactID:
// the artifact.approved of a review of that version, if any, and the version's artifact.version,
// each as the log lists it; the zero carryEvent where the log holds none.
func restoreEvents(t *testing.T, handler http.Handler, issueKey, artifactID string, number int) (approved, version carryEvent) {
	t.Helper()
	for _, event := range carryEvents(t, handler, issueKey) {
		switch event.Type {
		case "artifact.approved":
			if payload := decodePayload[model.ArtifactReviewEventPayload](t, event); payload.ArtifactID == artifactID && payload.Version == number {
				approved = event
			}
		case "artifact.version":
			payload := decodePayload[struct {
				ArtifactID string        `json:"artifact_id"`
				Version    model.Version `json:"version"`
			}](t, event)
			if payload.ArtifactID == artifactID && payload.Version.Number == number {
				version = event
			}
		}
	}
	return approved, version
}

// A version whose text is the text a person approved, and whose record is the record they
// approved, carries their approval in the transaction that writes it, and the carry keeps the order
// the Legion daemon reads a restore by: the carried artifact.approved before the version's own
// artifact.version. The carried review pins the approval's digest, which is the restored version's
// record's too: the write pinned the version's version_renders row to the approval's digest before
// it inserted the review, and the fill read the record by its own walk and found it equal. The
// spec holds a requirement, so its record is not empty. An approval request the agent opened on the
// changed version is answered by the carry, and the edit's own answer, read once its write phase
// has run, says it moved no request.
func TestARestoreOfTheApprovedRecordPinsItsDigestBeforeItsVersion(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "PINS")
	issue := createDigestIssue(t, handler, database, "PINS", "A restore of the approved record", revisedSpec, false)
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	approved := newestQueryVersion(t, database, spec.id)
	pin := requireRecordedDigest(t, database, "the spec as the create wrote it", issue.Key, graphstore.QuerySpec, approved)
	if review := reviewDocument(t, handler, spec.id, "alice", "approved", ""); review.ContentSHA256 == nil || *review.ContentSHA256 != pin {
		t.Fatalf("alice's approval of version %d pins %v, want its record's digest %s", approved.number, review.ContentSHA256, pin)
	}
	agentEdit(t, handler, spec.id, approved.number+1, replaceOp("The spec's opening words.", "The spec's first words."))
	request := requestApproval(t, handler, spec.id)

	advice := agentEdit(t, handler, spec.id, approved.number+2, replaceOp("The spec's first words.", "The spec's opening words."))
	restored := newestQueryVersion(t, database, spec.id)
	if restored.number != approved.number+2 || restored.markdown != approved.markdown {
		t.Fatalf("the undo leaves the spec at version %d holding %q, want version %d holding version %d's text %q",
			restored.number, restored.markdown, approved.number+2, approved.number, approved.markdown)
	}
	requireReviewCarriedWith(t, handler, spec.id, restored.number, approved.number, restoreSentence(approved.number), &request.ID)
	digest := requireRecordedDigest(t, database, "the restored version", issue.Key, graphstore.QuerySpec, restored)
	if digest != pin {
		t.Fatalf("version %d records the digest %s, want the approved record's %s", restored.number, digest, pin)
	}
	if recorded := recordedDigest(t, database, issue.Key, graphstore.QuerySpec, spec.id, restored.number); recorded != pin {
		t.Fatalf("the record of version %d at its row's cut digests to %s, want the approved %s", restored.number, recorded, pin)
	}
	reviews := contentReviews(t, handler, spec.id)
	if len(reviews) != 2 || reviews[0].Version != restored.number || reviews[0].ContentSHA256 == nil || *reviews[0].ContentSHA256 != pin {
		t.Fatalf("reviews = %#v, want alice's approval and its carry to version %d pinning %s", reviews, restored.number, pin)
	}
	approval, version := restoreEvents(t, handler, issue.Key, spec.id, restored.number)
	if approval.Seq == 0 || version.Seq == 0 || approval.Seq >= version.Seq {
		t.Fatalf("artifact.approved seq %d, artifact.version seq %d, want both for version %d with the approval first", approval.Seq, version.Seq, restored.number)
	}
	if payload := decodePayload[model.ArtifactReviewEventPayload](t, approval); payload.ContentSHA256 == nil || *payload.ContentSHA256 != pin {
		t.Fatalf("the carried artifact.approved names the digest %v, want %s", payload.ContentSHA256, pin)
	}
	if advice.MovedApprovalRequest != nil || advice.SupersededApproval != nil {
		t.Fatalf("the undo's moved_approval_request = %#v, superseded_approval = %#v, want neither: its carry answered the request and carried the approval",
			advice.MovedApprovalRequest, advice.SupersededApproval)
	}
	if answered := readBlockAsk(t, handler, request.ID); answered.State != "answered" || answered.Answer == nil || answered.Answer.User != "alice" {
		t.Fatalf("the request after the undo is %q answered by %#v, want it answered in alice's name by the carry", answered.State, answered.Answer)
	}
}

// A version whose text is the text a person approved but whose record has moved since - a result
// recorded under the document's check - is no restore of what they approved, and carries nothing:
// the write pins the version's row to the approval's digest, the fill reads the record by its own
// walk and finds another digest, and the transaction rolls back to the savepoint it took before the
// version and writes the version, what it states and its row again with no carry. No review and no
// artifact.approved of the version stand, its row records its own record's digest, and its node
// is caused by its artifact.version. The edit's answer is read once the write phase has run: the
// approval request the agent opened is moved to the new version and waits on the agent, which the
// dropped carry would have answered, and no approval is named superseded, since the approval was
// stale before the edit (supersededBy).
func TestARestoreOfTheApprovedTextOverAMovedRecordCarriesNothing(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "PINM")
	issue := createDigestIssue(t, handler, database, "PINM", "A restore of the approved text over a moved record", revisedSpec, false)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", revisedChecks)
	verification := readDecisionDocument(t, database, issue.Key, "verification-md")
	approved := newestQueryVersion(t, database, verification.id)
	pin := requireRecordedDigest(t, database, "the Verification document as uploaded", issue.Key, graphstore.QueryVerification, approved)
	if review := reviewDocument(t, handler, verification.id, "alice", "approved", ""); review.ContentSHA256 == nil || *review.ContentSHA256 != pin {
		t.Fatalf("alice's approval of version %d pins %v, want its record's digest %s", approved.number, review.ContentSHA256, pin)
	}
	if recorded := recordResult(t, handler, issue.Key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1})); recorded.Code != http.StatusCreated {
		t.Fatalf("record a result of c1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	moved := newestQueryVersion(t, database, verification.id)
	if moved.number != approved.number+1 || moved.markdown != approved.markdown {
		t.Fatalf("the result leaves the Verification document at version %d holding %q, want version %d holding the approved text", moved.number, moved.markdown, approved.number+1)
	}
	request := requestApproval(t, handler, verification.id)
	const checkText, changedText = "The list reads the urgent asks first.", "The list reads every ask."
	agentEdit(t, handler, verification.id, moved.number+1, replaceOp(checkText, changedText))

	advice := agentEdit(t, handler, verification.id, moved.number+2, replaceOp(changedText, checkText))
	restored := newestQueryVersion(t, database, verification.id)
	if restored.number != moved.number+2 || restored.markdown != approved.markdown {
		t.Fatalf("the undo leaves the Verification document at version %d holding %q, want version %d holding the approved text %q",
			restored.number, restored.markdown, moved.number+2, approved.markdown)
	}
	if reviews := contentReviews(t, handler, verification.id); len(reviews) != 1 || reviews[0].Version != approved.number {
		t.Fatalf("reviews = %#v, want alice's approval of version %d alone: version %d restores the approved text over another record", reviews, approved.number, restored.number)
	}
	approval, version := restoreEvents(t, handler, issue.Key, verification.id, restored.number)
	if approval.Seq != 0 || version.Seq == 0 {
		t.Fatalf("artifact.approved seq %d, artifact.version seq %d, want the version's artifact.version alone", approval.Seq, version.Seq)
	}
	if advice.SupersededApproval != nil {
		t.Fatalf("the undo's superseded_approval = %#v, want none: the approval of version %d was stale before it", advice.SupersededApproval, approved.number)
	}
	if advice.MovedApprovalRequest == nil || advice.MovedApprovalRequest.AskID != request.ID || len(advice.MovedApprovalRequest.Documents) != 1 ||
		advice.MovedApprovalRequest.Documents[0].Version != restored.number || advice.MovedApprovalRequest.Documents[0].RequestedVersion != moved.number {
		t.Fatalf("the undo's moved_approval_request = %#v, want request %s at version %d, handed over at %d: no carry answered it",
			advice.MovedApprovalRequest, request.ID, restored.number, moved.number)
	}
	if open := readBlockAsk(t, handler, request.ID); open.State != "open" || open.Approval == nil || open.Approval.Version != restored.number {
		t.Fatalf("the request after the undo is %q at %#v, want it open at version %d", open.State, open.Approval, restored.number)
	}
	if digest := requireRecordedDigest(t, database, "the restore over a moved record", issue.Key, graphstore.QueryVerification, restored); digest == pin {
		t.Fatalf("version %d records the approved digest %s, want the digest of its own record, which holds the result", restored.number, pin)
	}
	requireRevisedVersion(t, database, "the restore over a moved record", verification.id, verification.refKey, restored.number,
		eventOf(t, database, "artifact.version", "payload->>'artifact_id'", verification.id))
}

// One approval request names a spec and its Verification document, and one settlement of the
// issue's room writes a version of each that restores the text alice approved. The Verification
// document's carry runs first and leaves the request open, since the spec's newest stored review
// approves an older version than the request names; the spec's carry then reads the Verification
// document's carry's queued review, finds both documents approved and answers the request. Where
// the Verification document's record moved since alice approved it - a result recorded under its
// check, which appends a version of the same bytes and moves no request (docs' ReviseQueries) -
// the write phase drops that carry, and the answer the spec's carry decided on its review goes
// with it: the request stays open, moved to the versions the settlement wrote, and no ask event
// answers it, while the spec's own carry stands, approval first, naming the request it was given
// against. Where the record is the one alice approved, both carries stand, each artifact.approved
// before its version's artifact.version, and the request is answered in alice's name.
func TestATwoDocumentRequestClosesOnlyOnTheCarriesTheWritePhaseKeeps(t *testing.T) {
	changedChecks := strings.Replace(revisedChecks, "the urgent asks first", "every ask", 1)
	for _, test := range []struct {
		name, key string
		moved     bool
	}{
		{name: "the Verification document's record moved since the approval", key: "TWOMOVED", moved: true},
		{name: "both records are the approved ones", key: "TWOEXACT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
			documents := deps.Docs.(*docs.Service)
			issue := createInteractionIssue(t, handler, test.key, "Two documents restored at once", restoreSpec)
			spec := issue.PrimaryArtifactID
			verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", revisedChecks).Artifact.ID
			reviewDocument(t, handler, spec, "alice", "approved", "")
			approval := reviewDocument(t, handler, verification, "alice", "approved", "")
			// writeBoth puts both texts into the room's live documents and settles the room once,
			// which writes a version of each document in one transaction.
			writeBoth := func(specText, checksText string) {
				t.Helper()
				writer := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
				for document, text := range map[string]string{spec: specText, verification: checksText} {
					if _, err := replaceDocumentText(database, documents, document, text, writer); err != nil {
						t.Fatalf("write the live text of %s: %v", document, err)
					}
				}
				settleIssue(t, documents, issue.Key)
			}
			writeBoth(strings.Replace(restoreSpec, "behind a flag.", "behind a flag and a canary.", 1), changedChecks)
			request := requestApprovalWithVerification(t, handler, spec)
			if test.moved {
				if recorded := recordResult(t, handler, issue.Key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1})); recorded.Code != http.StatusCreated {
					t.Fatalf("record a result of c1: status=%d body=%s", recorded.Code, recorded.Body.String())
				}
				head, err := docs.ReadQueryRecord(context.Background(), database.Pool, issue.Key, graphstore.QueryVerification, refs.TreeCut{})
				if err != nil {
					t.Fatalf("read the Verification document's record: %v", err)
				}
				if approval.ContentSHA256 == nil || head.Digest == *approval.ContentSHA256 {
					t.Fatalf("the Verification document's record digests to %s, the digest alice's approval pins: the fixture moved no record", head.Digest)
				}
			}

			writeBoth(restoreSpec, revisedChecks)
			specVersion, checksVersion := newestQueryVersion(t, database, spec), newestQueryVersion(t, database, verification)
			if specVersion.number != 3 || specVersion.markdown != versionMarkdown(t, handler, spec, 1) || checksVersion.markdown != versionMarkdown(t, handler, verification, 1) {
				t.Fatalf("the settlement leaves the spec at version %d and the Verification document at version %d, want the spec at 3 and both holding their approved texts",
					specVersion.number, checksVersion.number)
			}
			specReviews := contentReviews(t, handler, spec)
			if len(specReviews) != 2 || specReviews[0].Version != 3 || specReviews[0].AskID == nil || *specReviews[0].AskID != request.ID {
				t.Fatalf("spec reviews = %#v, want alice's approval carried to version 3, naming request %s", specReviews, request.ID)
			}
			answered := false
			for _, event := range carryEvents(t, handler, issue.Key) {
				if event.Type == "ask.answered" && decodePayload[struct {
					ID string `json:"id"`
				}](t, event).ID == request.ID {
					answered = true
				}
			}
			asked := readBlockAsk(t, handler, request.ID)
			specApproval, specVersionEvent := restoreEvents(t, handler, issue.Key, spec, specVersion.number)
			checksApproval, checksVersionEvent := restoreEvents(t, handler, issue.Key, verification, checksVersion.number)
			if specApproval.Seq == 0 || specVersionEvent.Seq == 0 || specApproval.Seq >= specVersionEvent.Seq {
				t.Fatalf("the spec's artifact.approved seq %d, artifact.version seq %d, want both for version 3 with the approval first", specApproval.Seq, specVersionEvent.Seq)
			}
			if test.moved {
				if reviews := contentReviews(t, handler, verification); len(reviews) != 1 || reviews[0].Version != 1 {
					t.Fatalf("Verification reviews = %#v, want alice's approval of version 1 alone: version %d restores its text over another record", reviews, checksVersion.number)
				}
				if checksApproval.Seq != 0 || checksVersionEvent.Seq == 0 {
					t.Fatalf("the Verification document's artifact.approved seq %d, artifact.version seq %d, want the version's artifact.version alone", checksApproval.Seq, checksVersionEvent.Seq)
				}
				if asked.State != "open" || asked.Answer != nil || answered {
					t.Fatalf("the request is %q answered by %#v (ask.answered appended: %t), want it open and unanswered: the carry whose review its answer read was dropped",
						asked.State, asked.Answer, answered)
				}
				return
			}
			if reviews := contentReviews(t, handler, verification); len(reviews) != 2 || reviews[0].Version != checksVersion.number {
				t.Fatalf("Verification reviews = %#v, want alice's approval carried to version %d", reviews, checksVersion.number)
			}
			if checksApproval.Seq == 0 || checksVersionEvent.Seq == 0 || checksApproval.Seq >= checksVersionEvent.Seq {
				t.Fatalf("the Verification document's artifact.approved seq %d, artifact.version seq %d, want both for version %d with the approval first",
					checksApproval.Seq, checksVersionEvent.Seq, checksVersion.number)
			}
			if asked.State != "answered" || asked.Answer == nil || asked.Answer.User != "alice" || !answered {
				t.Fatalf("the request is %q answered by %#v (ask.answered appended: %t), want it answered in alice's name", asked.State, asked.Answer, answered)
			}
		})
	}
}
