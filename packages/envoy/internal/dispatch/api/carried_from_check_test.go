package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// c-carried-from (R27 of this issue's spec): a carried approval says that it is carried. A review
// Dispatch carries forward to a later version reads kind person with the person as its actor, as
// the person's own approval does, so the review record (GET /api/v1/artifacts/{id}/reviews), the
// artifact's approval (GET /api/v1/artifacts/{id}) and the artifact.approved payload carry
// carried_from_version: the version the approval was carried from, and null for a person's own act
// and for a plan review's. writeCarriedReview writes it for every carry, as a row of
// artifact_review_carries keyed by the review's id, in the transaction that writes the review. A
// row the reads find is what they answer; a carried review that holds no row, one written before
// this build, answers the version its reason's fixed sentence names (docs.CarriedFromVersion) until
// the backfill's pass fills its row.
//
// The tests read the member off the JSON each route answers, where a member left out fails the
// test: every review, approval and payload this build writes carries it, null for a person's own
// act, so a reader never takes its absence for one. They read the row by SQL.

// carriedFromReview is one review as GET /api/v1/artifacts/{id}/reviews answers it, with its
// carried_from_version: nil where the member is null, and the member as the route wrote it, ""
// where the route left it out.
type carriedFromReview struct {
	ID                 string  `json:"id"`
	Version            int     `json:"version"`
	State              string  `json:"state"`
	Kind               string  `json:"kind"`
	Reason             *string `json:"reason"`
	CarriedFrom        *int    `json:"-"`
	CarriedFromWritten string  `json:"-"`
}

// The edits the carried-from tests make to the reading tests' spec (readingSpec) as the agent's
// session. The first two touch its readings alone, which the reading case of the approval carry
// carries an approval across (docs.readingCase), and each stores one version, since neither writes
// a decided ask's block (readingWrite.carryReasons): a reading added under the decision d2, and the
// words of the reading r1 changed. The third changes the prose, which supersedes the approval.
var carriedFromReadingAdded = replaceOp(readingDecisionTwo,
	"::::decision{#d2}\n"+readingDecisionTwo+"\n\n:::reading{#r2}\n"+readingOfTwo+"\n:::\n::::")

var carriedFromReadingEdited = replaceOp("for every service.", "for every public service.")

var carriedFromProseChanged = replaceOp(readingProse, "The service ships behind a flag and a canary.")

// carriedFromFixture serves documents that version an agent's edit in the edit's own transaction
// and settle a person's answer in milliseconds (settlingCarryDocuments), creates project TEST, and
// writes the reading tests' fixture spec, which alice approves once its asks hold their answers and
// its declined ask her decline, each of which settles into a version of its own, so that she
// approves version 4 (approvedReadingSpec). It answers the handler, the store and the fixture.
func carriedFromFixture(t *testing.T, title string) (http.Handler, *store.Store, readingFixture) {
	t.Helper()
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	createRoomProject(t, handler, "TEST")
	return handler, database, approvedReadingSpec(t, handler, database, title)
}

// carriedFromEdit makes ops to fixture's spec through the edits route as the agent's session
// (agentEdit), waits until the spec owes no settlement, and fails t unless its latest version is
// number, the one version the edit stores.
func carriedFromEdit(t *testing.T, handler http.Handler, database *store.Store, fixture readingFixture, number int, ops ...map[string]string) {
	t.Helper()
	agentEdit(t, handler, fixture.artifactID, number, ops...)
	awaitSettled(t, database, fixture.artifactID)
	if stored := documentVersions(t, handler, fixture.artifactID); stored[len(stored)-1].Number != number {
		t.Fatalf("once the spec owes no settlement its latest version is %d, want %d, the one version the edit stores", stored[len(stored)-1].Number, number)
	}
}

// carriedFromMember is the carried_from_version member of members, a review, an approval or an
// event's payload as what names it answers it: nil where the member is null. It fails t where the
// member is left out, and answers nil then.
func carriedFromMember(t *testing.T, what string, members map[string]json.RawMessage) *int {
	t.Helper()
	raw, present := members["carried_from_version"]
	if !present {
		t.Errorf("%s leaves carried_from_version out, want a version or null", what)
		return nil
	}
	if string(raw) == "null" {
		return nil
	}
	var version int
	if err := json.Unmarshal(raw, &version); err != nil {
		t.Fatalf("%s answers carried_from_version %s, want a version or null: %v", what, raw, err)
	}
	return &version
}

// carriedFromReviews is the document's reviews as GET /api/v1/artifacts/{id}/reviews answers them,
// newest first, each with its carried_from_version.
func carriedFromReviews(t *testing.T, handler http.Handler, artifactID string) []carriedFromReview {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/reviews", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the reviews of %s: status=%d body=%s", artifactID, listed.Code, listed.Body.String())
	}
	answered := decodeBody[[]json.RawMessage](t, listed)
	reviews := make([]carriedFromReview, 0, len(answered))
	for _, raw := range answered {
		var review carriedFromReview
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &review); err != nil {
			t.Fatalf("decode a review of %s, %s: %v", artifactID, raw, err)
		}
		if err := json.Unmarshal(raw, &members); err != nil {
			t.Fatalf("decode a review of %s, %s: %v", artifactID, raw, err)
		}
		review.CarriedFrom = carriedFromMember(t, "the reviews route, for the review "+review.ID, members)
		review.CarriedFromWritten = string(members["carried_from_version"])
		reviews = append(reviews, review)
	}
	return reviews
}

// carriedFromReviewOf is the review of reviews whose id is id, failing t where none is.
func carriedFromReviewOf(t *testing.T, reviews []carriedFromReview, id string) carriedFromReview {
	t.Helper()
	for _, review := range reviews {
		if review.ID == id {
			return review
		}
	}
	t.Fatalf("the reviews route answers %+v, want the review %s among them", reviews, id)
	return carriedFromReview{}
}

// carriedFromApproval is the document's approval as GET /api/v1/artifacts/{id} answers it: its
// state, the version it names, 0 where it names none, and its carried_from_version.
func carriedFromApproval(t *testing.T, handler http.Handler, artifactID string) (string, int, *int) {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the artifact %s: status=%d body=%s", artifactID, read.Code, read.Body.String())
	}
	members := decodeBody[struct {
		Approval map[string]json.RawMessage `json:"approval"`
	}](t, read).Approval
	var state string
	var version int
	if raw, present := members["state"]; present {
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatalf("decode the approval state of %s, %s: %v", artifactID, raw, err)
		}
	}
	if raw, present := members["version"]; present && string(raw) != "null" {
		if err := json.Unmarshal(raw, &version); err != nil {
			t.Fatalf("decode the approved version of %s, %s: %v", artifactID, raw, err)
		}
	}
	return state, version, carriedFromMember(t, "the approval read of "+artifactID, members)
}

// carriedFromEventOf is the carried_from_version of the artifact.approved the log of issueKey holds
// of the review of version number of artifactID, failing t where it holds none.
func carriedFromEventOf(t *testing.T, handler http.Handler, issueKey, artifactID string, number int) *int {
	t.Helper()
	for _, event := range carryEvents(t, handler, issueKey) {
		if event.Type != "artifact.approved" {
			continue
		}
		payload := decodePayload[struct {
			ArtifactID string `json:"artifact_id"`
			Version    int    `json:"version"`
		}](t, event)
		if payload.ArtifactID == artifactID && payload.Version == number {
			return carriedFromMember(t, fmt.Sprintf("the artifact.approved of version %d", number), decodePayload[map[string]json.RawMessage](t, event))
		}
	}
	t.Fatalf("the log of %s holds no artifact.approved of version %d of %s", issueKey, number, artifactID)
	return nil
}

// carriedFromRow is the carried_from_version of the artifact_review_carries row of review, read
// by SQL, and whether the table holds one.
func carriedFromRow(t *testing.T, database *store.Store, review string) (int, bool) {
	t.Helper()
	var version int
	err := database.Pool.QueryRow(context.Background(),
		`select carried_from_version from artifact_review_carries where review_id = $1::uuid`, review).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read the artifact_review_carries row of the review %s: %v", review, err)
	}
	return version, true
}

// carriedFromRowWords is what carriedFromRow read, as a failure names it.
func carriedFromRowWords(version int, held bool) string {
	if !held {
		return "no artifact_review_carries row"
	}
	return fmt.Sprintf("the artifact_review_carries row naming version %d", version)
}

// carriedFromWords is a carried_from_version as a failure names it: the version, or null.
func carriedFromWords(version *int) string {
	if version == nil {
		return "null"
	}
	return strconv.Itoa(*version)
}

// requireCarriedFrom fails t unless got, the carried_from_version what answers, is want, nil for
// null.
func requireCarriedFrom(t *testing.T, what string, got, want *int) {
	t.Helper()
	if (got == nil) != (want == nil) || got != nil && *got != *want {
		t.Errorf("%s answers carried_from_version %s, want %s", what, carriedFromWords(got), carriedFromWords(want))
	}
}

// TestACarriedReviewNamesTheVersionItCarriesFrom is c-carried-from's first case (R27). Alice
// approves version 4 of the spec (carriedFromFixture), and the agent's session adds a reading under
// a decision, which carries her approval to version 5. The reviews route answers the review of
// version 5 with carried_from_version 4 and the review of version 4, her own act, with null written
// out, never left out; the approval read answers 4; the artifact.approved of the carry carries 4,
// and that of her own approval null. The session then changes a reading's words, which carries
// that carry to version 6, and the carry of the carry names 4, the version alice approved, through
// the same three reads. A row of artifact_review_carries written by
// SQL for the review of version 6, naming version 3 while its reason names 4, reads 3 through the
// reviews route and the approval read, so a reader that parses the reason alone fails. The session
// then changes the prose, which carries nothing, and alice approves version 7 herself: the reviews
// route and the approval read answer null for it, and it holds no row. Beside each read the test
// reads the row of the review of version 5 by SQL, which names 4 throughout. Last a plan review's
// approval of the issue's plan answers null through the reviews route and the approval read.
func TestACarriedReviewNamesTheVersionItCarriesFrom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, fixture := carriedFromFixture(t, "Carried from")
	approved := fixture.approval.Version
	carriedTo := approved + 1

	carriedFromEdit(t, handler, database, fixture, carriedTo, carriedFromReadingAdded)
	carry := requireReviewCarriedWith(t, handler, fixture.artifactID, carriedTo, approved, readingCarrySentence(approved), nil)
	requireCarryRow := func(beside string) {
		t.Helper()
		if version, held := carriedFromRow(t, database, carry.ID); !held || version != approved {
			t.Errorf("beside %s the review %s of version %d carried from the approval of version %d holds %s, want its artifact_review_carries row naming version %d",
				beside, carry.ID, carriedTo, approved, carriedFromRowWords(version, held), approved)
		}
	}

	// The carry and alice's own approval, through the reviews route, the approval read and the
	// carry's artifact.approved.
	reviews := carriedFromReviews(t, handler, fixture.artifactID)
	requireCarriedFrom(t, fmt.Sprintf("the reviews route, for the review %s carried to version %d", carry.ID, carriedTo),
		carriedFromReviewOf(t, reviews, carry.ID).CarriedFrom, &approved)
	firstOwn := carriedFromReviewOf(t, reviews, fixture.approval.ID)
	requireCarriedFrom(t, fmt.Sprintf("the reviews route, for alice's own approval %s of version %d", fixture.approval.ID, approved),
		firstOwn.CarriedFrom, nil)
	if firstOwn.CarriedFromWritten != "null" {
		t.Errorf("the reviews route writes carried_from_version %q for alice's own approval %s of version %d, want null written out: a member left out reads as no answer, not as her own act",
			firstOwn.CarriedFromWritten, fixture.approval.ID, approved)
	}
	requireCarryRow("the reviews route")
	state, version, carried := carriedFromApproval(t, handler, fixture.artifactID)
	if state != "approved" || version != carriedTo {
		t.Errorf("the approval read answers %s at version %d after the carry, want approved at version %d", state, version, carriedTo)
	}
	requireCarriedFrom(t, fmt.Sprintf("the approval read after the carry to version %d", carriedTo), carried, &approved)
	requireCarryRow("the approval read")
	requireCarriedFrom(t, fmt.Sprintf("the artifact.approved of the carry to version %d", carriedTo),
		carriedFromEventOf(t, handler, fixture.issueKey, fixture.artifactID, carriedTo), &approved)
	requireCarryRow("the carry's artifact.approved")
	requireCarriedFrom(t, fmt.Sprintf("the artifact.approved of alice's own approval of version %d", approved),
		carriedFromEventOf(t, handler, fixture.issueKey, fixture.artifactID, approved), nil)

	// A carry of that carry names the version alice approved.
	again := carriedTo + 1
	carriedFromEdit(t, handler, database, fixture, again, carriedFromReadingEdited)
	second := requireReviewCarriedWith(t, handler, fixture.artifactID, again, approved, readingCarrySentence(approved), nil)
	reviews = carriedFromReviews(t, handler, fixture.artifactID)
	requireCarriedFrom(t, fmt.Sprintf("the reviews route, for the review %s carried to version %d from the carry to version %d", second.ID, again, carriedTo),
		carriedFromReviewOf(t, reviews, second.ID).CarriedFrom, &approved)
	requireCarriedFrom(t, fmt.Sprintf("the reviews route, for the review %s carried to version %d, once that carry is carried again", carry.ID, carriedTo),
		carriedFromReviewOf(t, reviews, carry.ID).CarriedFrom, &approved)
	requireCarryRow("the reviews route after the carry of the carry")
	state, version, carried = carriedFromApproval(t, handler, fixture.artifactID)
	if state != "approved" || version != again {
		t.Errorf("the approval read answers %s at version %d after the carry of the carry, want approved at version %d", state, version, again)
	}
	requireCarriedFrom(t, fmt.Sprintf("the approval read after the carry of the carry to version %d", again), carried, &approved)
	requireCarryRow("the approval read after the carry of the carry")
	requireCarriedFrom(t, fmt.Sprintf("the artifact.approved of the carry of the carry to version %d", again),
		carriedFromEventOf(t, handler, fixture.issueKey, fixture.artifactID, again), &approved)
	if version, held := carriedFromRow(t, database, second.ID); !held || version != approved {
		t.Errorf("the review %s carried to version %d holds %s, want its artifact_review_carries row naming version %d, the version alice approved",
			second.ID, again, carriedFromRowWords(version, held), approved)
	}

	// A row written by SQL that names another version than the reason: the reads answer the row's.
	forged := approved - 1
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_review_carries (review_id, carried_from_version) values ($1::uuid, $2)
		on conflict (review_id) do update set carried_from_version = excluded.carried_from_version`, second.ID, forged); err != nil {
		t.Fatalf("write the artifact_review_carries row of the review %s naming version %d: %v", second.ID, forged, err)
	}
	reviews = carriedFromReviews(t, handler, fixture.artifactID)
	if got := carriedFromReviewOf(t, reviews, second.ID).CarriedFrom; got == nil || *got != forged {
		t.Errorf("the reviews route answers carried_from_version %s for the review %s of version %d, whose artifact_review_carries row names version %d and whose reason names version %d, want %d, the row's",
			carriedFromWords(got), second.ID, again, forged, approved, forged)
	}
	requireCarryRow("the reviews route over the row written by SQL")
	if _, _, got := carriedFromApproval(t, handler, fixture.artifactID); got == nil || *got != forged {
		t.Errorf("the approval read answers carried_from_version %s for the review %s of version %d, whose artifact_review_carries row names version %d and whose reason names version %d, want %d, the row's",
			carriedFromWords(got), second.ID, again, forged, approved, forged)
	}
	requireCarryRow("the approval read over the row written by SQL")

	// Alice's own approval of a later version is her own act.
	later := again + 1
	carriedFromEdit(t, handler, database, fixture, later, carriedFromProseChanged)
	own := reviewDocument(t, handler, fixture.artifactID, "alice", "approved", "")
	if own.Version != later {
		t.Fatalf("alice's own approval is of version %d, want version %d, the latest", own.Version, later)
	}
	reviews = carriedFromReviews(t, handler, fixture.artifactID)
	requireCarriedFrom(t, fmt.Sprintf("the reviews route, for alice's own approval %s of version %d", own.ID, later),
		carriedFromReviewOf(t, reviews, own.ID).CarriedFrom, nil)
	requireCarryRow("the reviews route after alice's own approval")
	state, version, carried = carriedFromApproval(t, handler, fixture.artifactID)
	if state != "approved" || version != later {
		t.Errorf("the approval read answers %s at version %d after alice's own approval, want approved at version %d", state, version, later)
	}
	requireCarriedFrom(t, fmt.Sprintf("the approval read after alice's own approval of version %d", later), carried, nil)
	requireCarryRow("the approval read after alice's own approval")
	if version, held := carriedFromRow(t, database, own.ID); held {
		t.Errorf("alice's own approval %s of version %d holds an artifact_review_carries row naming version %d, want none: she gave it herself", own.ID, later, version)
	}

	// A plan review's approval is no carry.
	plan := uploadDocument(t, handler, "/api/v1/issues/"+fixture.issueKey, "plan.md", "# Plan\n\nShip behind the flag, one region at a time.\n")
	recorded := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews",
		map[string]any{"actor": sessionActor(), "version": plan.Version.Number})
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record the plan review of version %d of the plan: status=%d body=%s", plan.Version.Number, recorded.Code, recorded.Body.String())
	}
	planReview := carriedFromReviewOf(t, carriedFromReviews(t, handler, plan.Artifact.ID), decodeBody[carriedFromReview](t, recorded).ID)
	if planReview.Kind != "plan_review" {
		t.Errorf("the plan's review reads kind %q, want plan_review", planReview.Kind)
	}
	requireCarriedFrom(t, fmt.Sprintf("the reviews route, for the plan review %s of version %d of the plan", planReview.ID, plan.Version.Number),
		planReview.CarriedFrom, nil)
	_, _, carried = carriedFromApproval(t, handler, plan.Artifact.ID)
	requireCarriedFrom(t, "the approval read of the plan its plan review approved", carried, nil)
}

// TestAnUnfilledCarriedReviewTakesItsSourceFromItsReason is c-carried-from's second case (R27).
// Alice approves version 4 of the spec (carriedFromFixture), and the agent's session adds a reading
// under a decision, which carries her approval to version 5: the carry holds its
// artifact_review_carries row. The release before this build writes the same review, its reason
// and its artifact.approved, and no row, so the test takes the row off by SQL and holds the review
// as that release leaves it. Before the backfill, the reviews route and the approval read answer 4
// for it, through its reason; alice's own approval of version 4 answers null and holds no row. The
// backfill (graphcensus.Backfill) writes the carried review's row naming 4, and the same reads
// answer the same after it.
func TestAnUnfilledCarriedReviewTakesItsSourceFromItsReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, fixture := carriedFromFixture(t, "Carried before its row")
	approved := fixture.approval.Version
	carriedTo := approved + 1

	carriedFromEdit(t, handler, database, fixture, carriedTo, carriedFromReadingAdded)
	carry := requireReviewCarriedWith(t, handler, fixture.artifactID, carriedTo, approved, readingCarrySentence(approved), nil)
	if version, held := carriedFromRow(t, database, carry.ID); !held {
		t.Errorf("the review of version %d carried from the approval of version %d holds no artifact_review_carries row", carriedTo, approved)
	} else if version != approved {
		t.Errorf("the review of version %d carried from the approval of version %d holds the artifact_review_carries row naming version %d, want %d",
			carriedTo, approved, version, approved)
	}

	// The review as the release before writes it: its carried reason, and no row.
	if _, err := database.Pool.Exec(ctx, `delete from artifact_review_carries where review_id = $1::uuid`, carry.ID); err != nil {
		t.Fatalf("leave the review %s with no artifact_review_carries row, as the release before writes it: %v", carry.ID, err)
	}
	requireTakenFromItsReason := func(when string) {
		t.Helper()
		reviews := carriedFromReviews(t, handler, fixture.artifactID)
		requireCarriedFrom(t, fmt.Sprintf("%s the reviews route, for the review %s of version %d, whose reason names version %d", when, carry.ID, carriedTo, approved),
			carriedFromReviewOf(t, reviews, carry.ID).CarriedFrom, &approved)
		requireCarriedFrom(t, fmt.Sprintf("%s the reviews route, for alice's own approval %s of version %d", when, fixture.approval.ID, approved),
			carriedFromReviewOf(t, reviews, fixture.approval.ID).CarriedFrom, nil)
		state, version, carried := carriedFromApproval(t, handler, fixture.artifactID)
		if state != "approved" || version != carriedTo {
			t.Errorf("%s the approval read answers %s at version %d, want approved at version %d", when, state, version, carriedTo)
		}
		requireCarriedFrom(t, when+" the approval read", carried, &approved)
		if version, held := carriedFromRow(t, database, fixture.approval.ID); held {
			t.Errorf("%s alice's own approval %s holds an artifact_review_carries row naming version %d, want none: she gave it herself",
				when, fixture.approval.ID, version)
		}
	}
	requireTakenFromItsReason("before the backfill")

	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	if version, held := carriedFromRow(t, database, carry.ID); !held || version != approved {
		t.Errorf("after the backfill the review %s of version %d, whose reason names version %d, holds %s, want its artifact_review_carries row naming version %d",
			carry.ID, carriedTo, approved, carriedFromRowWords(version, held), approved)
	}
	requireTakenFromItsReason("after the backfill")
}
