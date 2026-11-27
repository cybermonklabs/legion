package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// convertDigestIssue converts the issue key, created as before the issue room (createDigestIssue
// with unconverted), into its room through docs.Service.ConvertIssue under the issue room step's
// own gate (docs.IssueRoomGate), as TestAConvertedNodeKeepsItsAuthor converts one, and fails unless
// it converted. A conversion defers an issue one of whose documents owes a settlement or its block
// index, so every settlement the issue's documents owe runs first (settleIssue), and then the
// block-index reconciliation until none owes its index (reconcileUntilNothingOwed).
func convertDigestIssue(t *testing.T, rig *digestRig, key string) {
	t.Helper()
	settleIssue(t, rig.documents, key)
	reconcileUntilNothingOwed(t, rig.handler, Deps{Docs: rig.documents}, key)
	converted, err := rig.documents.ConvertIssue(context.Background(), key, docs.IssueRoomGate, false)
	if err != nil || converted.Outcome != docs.IssueConverted {
		t.Fatalf("the conversion of %s answered %+v, %v; want it converted", key, converted, err)
	}
}

// requireCarriedAcrossTheMove fails unless the approval of version approved of document, which a
// person gave before its issue converted and whose newest review was pre then, reads approved after
// the move at the version the conversion wrote, through the carry Dispatch records in that person's
// name there, and not through a digest match: the newest review is an approval of that version in
// pre's actor's name, whose carried_from_version is approved and whose reason opens with the
// carried prefix naming approved, and the digest that version answers once its fill has run
// (artifact_version_text_sha256) is not the sha256 of stored markdown pre holds. It answers the
// version the conversion wrote.
func requireCarriedAcrossTheMove(t *testing.T, rig *digestRig, what string, document decisionDocument, approved int, pre model.ArtifactReview) int {
	t.Helper()
	awaitDigestsFilled(t, rig.database, document.id)
	moved := newestVersionOf(t, rig.database, document.id)
	if moved <= pre.Version {
		t.Fatalf("%s: the conversion left version %d of %s newest, want a version of its own past version %d", what, moved, document.refKey, pre.Version)
	}
	if got := readApproval(t, rig.handler, document.id); got.Approval == nil || got.Approval.State != "approved" ||
		got.Approval.Version == nil || *got.Approval.Version != moved || got.Approval.LatestVersion != moved {
		t.Errorf("%s: the approval of %s reads %s after the move, want approved at version %d, the version the conversion wrote",
			what, document.refKey, describeApproval(got), moved)
	}
	reviews := listReviews(t, rig.handler, document.id)
	if len(reviews) == 0 {
		t.Fatalf("%s: %s holds no review after the move", what, document.refKey)
	}
	carried, opening := reviews[0], fmt.Sprintf("%s%d: ", model.CarriedReviewReasonPrefix, approved)
	from := "none"
	if carried.CarriedFromVersion != nil {
		from = strconv.Itoa(*carried.CarriedFromVersion)
	}
	if carried.ID == pre.ID || carried.Version != moved || carried.State != "approved" || carried.Actor.Kind != pre.Actor.Kind || carried.Actor.ID != pre.Actor.ID ||
		from != strconv.Itoa(approved) || !strings.HasPrefix(deref(carried.Reason), opening) {
		t.Errorf("%s: the newest review of %s is %s's %s of version %d, carried from version %s, with the reason %q; want %s's approval carried from version %d to version %d, the version the conversion wrote, its reason opening %q",
			what, document.refKey, carried.Actor.ID, carried.State, carried.Version, from, deref(carried.Reason), pre.Actor.ID, approved, moved, opening)
	}
	if digest := versionDigestOf(t, rig.database, document.id, moved); digest == deref(pre.ContentSHA256) {
		t.Errorf("%s: version %d of %s, which the conversion wrote, answers the digest %s, the sha256 of stored markdown the review of version %d holds: the approval stands by a digest match, not by the carry",
			what, moved, document.refKey, digest, pre.Version)
	}
	return moved
}

// digestStepCarriedAcrossTheMove pins the move and the column (R14): an approval given before the
// move still reads approved after it, through the carry Dispatch records in that person's name at
// the new version, and not through a digest match, since a review taken before the cutover holds
// the sha256 of stored markdown, which the length-prefixed record cannot reproduce. On an issue
// that has not converted, alice approves its spec and its Verification document, and each review
// pins the sha256 of its version's stored markdown; docs.ConvertIssue moves the issue into its
// room, and each approval reads approved at the version the conversion wrote, carried in her name,
// while that version answers another digest (requireCarriedAcrossTheMove). A digest comparison
// applies only to a review taken after the cutover, its version's pass and its digest fill: her
// approval of the converted spec pins the digest of its spec record, which the test builds again
// over the query it left untouched. The same carry runs for a review whose content_sha256_source
// is backfill: an approval row written in the shape the release before the digest column wrote it,
// which docs.FillReviewContentHashes pins at boot, carries across the move as one taken at the
// approval does.
func digestStepCarriedAcrossTheMove(t *testing.T, rig *digestRig) {
	ctx := context.Background()
	issue := createDigestIssue(t, rig.handler, rig.database, "CORE", "An approval given before the move",
		":::requirement{#r1}\nThe moved list comes first.\n:::\n", true)
	uploadDocument(t, rig.handler, "/api/v1/issues/"+issue.Key, "verification.md", checkServing("c1", "dispatch://"+issue.Key+"/spec#b-r1"))
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	documents := []struct {
		name     string
		document decisionDocument
	}{
		{"spec", spec},
		{"Verification document", readDecisionDocument(t, rig.database, issue.Key, "verification-md")},
	}
	approvals := make([]model.ArtifactReview, len(documents))
	for index, held := range documents {
		review := reviewDocument(t, rig.handler, held.document.id, "alice", "approved", "")
		if stored := contentHash(versionMarkdown(t, rig.handler, held.document.id, review.Version)); deref(review.ContentSHA256) != stored {
			t.Fatalf("alice's approval of version %d of the %s of %s, an issue that has not converted, pins %q, want the sha256 of its stored markdown, %s",
				review.Version, held.name, issue.Key, deref(review.ContentSHA256), stored)
		}
		approvals[index] = review
	}
	convertDigestIssue(t, rig, issue.Key)
	moved := make([]int, len(documents))
	for index, held := range documents {
		moved[index] = requireCarriedAcrossTheMove(t, rig, "the "+held.name+" of "+issue.Key, held.document, approvals[index].Version, approvals[index])
	}

	// A review taken after the cutover, its version's pass and its digest fill, over the query left
	// untouched since: the record built again digests to what the review holds.
	review := reviewDocument(t, rig.handler, spec.id, "alice", "approved", "")
	if record := rig.record(t, issue.Key, graphstore.QuerySpec); review.Version != moved[0] || deref(review.ContentSHA256) != record.Digest {
		t.Errorf("alice's approval of the converted spec of %s, taken after the cutover, its version's pass and its digest fill, names version %d and pins %q, want version %d and %s, the digest of its spec record built again over the untouched query",
			issue.Key, review.Version, deref(review.ContentSHA256), moved[0], record.Digest)
	}

	// An approval the release before the digest column wrote, which the boot's fill pins.
	legacy := createDigestIssue(t, rig.handler, rig.database, "CORE", "An approval the release before wrote",
		":::requirement{#r1}\nThe backfilled list comes first.\n:::\n", true)
	legacySpec := readDecisionDocument(t, rig.database, legacy.Key, "spec")
	version := newestVersionOf(t, rig.database, legacySpec.id)
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `alter table artifact_reviews disable trigger artifact_reviews_content_sha256_insert`); err != nil {
		t.Fatalf("take off the insert trigger of content_sha256, which the release before the column ran without: %v", err)
	}
	var legacyID string
	if err := tx.QueryRow(ctx, `
		insert into artifact_reviews (artifact_id, version, state, actor)
		values ($1::uuid, $2, 'approved', '{"kind":"user","id":"alice"}') returning id::text`, legacySpec.id, version).Scan(&legacyID); err != nil {
		t.Fatalf("write alice's approval of version %d of %s in the shape the release before the digest column wrote: %v", version, legacySpec.refKey, err)
	}
	// The review-sequence constraint trigger is deferred: its events drain before the insert
	// trigger is put back in this transaction, as the fill's own test drains them.
	if _, err := tx.Exec(ctx, `set constraints all immediate`); err != nil {
		t.Fatalf("drain the deferred constraint triggers: %v", err)
	}
	if _, err := tx.Exec(ctx, `alter table artifact_reviews enable trigger artifact_reviews_content_sha256_insert`); err != nil {
		t.Fatalf("put back the insert trigger of content_sha256: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the approval the release before wrote: %v", err)
	}
	if _, err := docs.FillReviewContentHashes(ctx, rig.database.Pool); err != nil {
		t.Fatalf("run the boot's fill of the review digests: %v", err)
	}
	reviews := listReviews(t, rig.handler, legacySpec.id)
	if len(reviews) != 1 || reviews[0].ID != legacyID {
		t.Fatalf("the reviews of %s read %+v, want the one approval the release before wrote, %s", legacySpec.refKey, reviews, legacyID)
	}
	backfilled := reviews[0]
	if stored := contentHash(versionMarkdown(t, rig.handler, legacySpec.id, version)); deref(backfilled.ContentSHA256Source) != "backfill" || deref(backfilled.ContentSHA256) != stored {
		t.Fatalf("the approval the release before wrote reads content_sha256 %q from %q after the boot's fill, want the sha256 of version %d's stored markdown, %s, from backfill",
			deref(backfilled.ContentSHA256), deref(backfilled.ContentSHA256Source), version, stored)
	}
	convertDigestIssue(t, rig, legacy.Key)
	requireCarriedAcrossTheMove(t, rig, "the spec of "+legacy.Key+", approved by a row the release before the digest column wrote", legacySpec, version, backfilled)
}

// requireAnswerCarry fails unless alice's approval of version approved of document is carried to
// version answered, the version her answer to an ask block wrote, as the answer carry writes it:
// the newest review, a person's approval of that version in her name (requireReviewCarriedWith),
// naming approved as the version it carries from, with the whole reason R10's case of the
// server-owned ask attributes writes (attributeRemovalSentence, docs' attributeRemovalCase), and the
// document approved at answered, its latest version.
func requireAnswerCarry(t *testing.T, rig *digestRig, when string, document decisionDocument, answered, approved int) {
	t.Helper()
	awaitDigestsFilled(t, rig.database, document.id)
	carried := requireReviewCarriedWith(t, rig.handler, document.id, answered, approved, attributeRemovalSentence(approved), nil)
	reviews := listReviews(t, rig.handler, document.id)
	if len(reviews) == 0 || reviews[0].ID != carried.ID || reviews[0].Kind != docs.ReviewKindPerson ||
		reviews[0].CarriedFromVersion == nil || *reviews[0].CarriedFromVersion != approved {
		t.Errorf("%s: the reviews of %s read %+v, want the newest a person's approval carried from version %d to version %d, the version the answer wrote",
			when, document.refKey, reviews, approved, answered)
	}
}

// digestStepAnswerCarryAcrossTheMove pins the answer carry, which the move leaves as it is (R10,
// R14): a person's own answer to an ask block still carries her approval as it does today, once
// before the move and once after, with the same verdict. On an issue that has not converted, an
// agent writes an ask block into the spec through the edits route and settlement makes its ask;
// alice approves the spec and answers the ask, and the version her answer writes, which changes the
// server-owned attributes of the block alone, carries her approval in her name with the reason
// R10's case writes. docs.ConvertIssue then moves the issue into its room, where her approval
// stands at the version the conversion wrote by the conversion's carry; the agent writes a second
// ask block the same way, alice approves the spec and answers it, and the version her answer
// writes carries her approval with that same reason.
func digestStepAnswerCarryAcrossTheMove(t *testing.T, rig *digestRig) {
	issue := createDigestIssue(t, rig.handler, rig.database, "CORE", "An answer carried across the move",
		":::requirement{#r1}\nThe answered list comes first.\n:::\n", true)
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	// answer has the agent write ask, holding the ask block block, into the spec as the version
	// after its newest, settlement make the block's ask, alice approve that version and answer the
	// ask with selected, and settlement write the version of her answer; it answers the version
	// alice approved and the version her answer wrote.
	answer := func(ask, block, question, selected string) (approved, answered int) {
		t.Helper()
		approved = newestVersionOf(t, rig.database, spec.id) + 1
		insertDecisionMarkdown(t, rig.handler, spec.id, ask, approved)
		settleIssue(t, rig.documents, issue.Key)
		asks := indexedDecisionBlocks(t, rig.handler, issue.Key, spec.id, map[string]string{block: question})
		awaitDigestsFilled(t, rig.database, spec.id)
		approveVersion(t, rig.handler, spec.id, "alice", approved)
		answerDecisionBlock(t, rig.handler, asks[block], "alice", []string{selected}, "", nil)
		settleIssue(t, rig.documents, issue.Key)
		return approved, requireStateVersionsPast(t, rig.handler, rig.database, spec.id, approved, 1)
	}

	approved, answered := answer(waveAsk, "wave", "How many regions take the first wave?", "Two")
	requireAnswerCarry(t, rig, "before the move", spec, answered, approved)

	carried := listReviews(t, rig.handler, spec.id)[0]
	convertDigestIssue(t, rig, issue.Key)
	requireCarriedAcrossTheMove(t, rig, "the spec of "+issue.Key+", whose answer carried its approval before the move", spec, approved, carried)

	approved, answered = answer(regionAsk, "region", "Which region goes first?", "East")
	requireAnswerCarry(t, rig, "after the move", spec, answered, approved)
}
