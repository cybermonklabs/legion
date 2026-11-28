package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
// An approval covers the digest of the spec query's record (R14), which a change of prose alone
// leaves as it was, so the words the revision changes are a requirement's, and the approval reads
// stale at the version that changes them.
func TestEveryApprovalReadNamesTheApprovedBytes(t *testing.T) {
	t.Parallel()
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "HASH", "Approve bytes", ":::requirement{#r1}\nApprove me ☕\n:::\n")
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

// A carry pins its destination bytes, not the source approval's bytes. The carry here is R5's
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
	notes := decodeBody[uploadedArtifact](t, uploaded).Artifact
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
	older := versionMarkdownAt(t, database, spec, 1)
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
// version_renders row, naming the digest a row it holds records, or none.
func requireNoVersionRender(t *testing.T, database *store.Store, what, artifactID string, number int) {
	t.Helper()
	var digest *string
	err := database.Pool.QueryRow(context.Background(),
		`select record_sha256 from version_renders where artifact_id = $1 and number = $2`, artifactID, number).Scan(&digest)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		t.Fatalf("%s: read the version_renders row of version %d: %v", what, number, err)
	default:
		t.Errorf("%s: version %d holds a version_renders row recording the digest %s, want no row", what, number, valueOrNone(digest))
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
