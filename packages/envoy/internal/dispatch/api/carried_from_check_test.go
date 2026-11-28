package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// c-carried-from (R27 of the issue-tree spec): a carried approval says that it is carried. A review
// Dispatch carries forward to a later version reads kind person with the person as its actor, as
// the person's own approval does, so the review record (GET /api/v1/artifacts/{id}/reviews), the
// artifact's approval (GET /api/v1/artifacts/{id}) and the artifact.approved payload carry
// carried_from_version: the version the approval was carried from, and null for a person's own act
// and for a plan review's. writeCarriedReview writes it for every carry, as a row of
// artifact_review_carries keyed by the review's id, in the transaction that writes the review. A
// row the reads find is what they answer; a carried review that holds no row, one written before
// this build, answers the version its reason's fixed sentence names (model.CarriedFromVersion)
// until the backfill's pass fills its row.
//
// The tests read the member off the JSON each route answers, where a member left out fails the
// test: every review, approval and payload this build writes carries it, null for a person's own
// act, so a reader never takes its absence for one. They read the row by SQL.

// carriedFromReview is one review as GET /api/v1/artifacts/{id}/reviews answers it, with its
// carried_from_version: nil where the member is null, and the member as the route wrote it, ""
// where the route left it out.
type carriedFromReview struct {
	ID                 string      `json:"id"`
	Version            int         `json:"version"`
	State              string      `json:"state"`
	Kind               string      `json:"kind"`
	Actor              model.Actor `json:"actor"`
	Reason             *string     `json:"reason"`
	CarriedFrom        *int        `json:"-"`
	CarriedFromWritten string      `json:"-"`
}

// carriedFromReviewsWords is reviews as a failure names them: each one's id, who gave it, its
// state, version and kind, its carried_from_version and its reason.
func carriedFromReviewsWords(reviews []carriedFromReview) string {
	words := make([]string, 0, len(reviews))
	for _, review := range reviews {
		reason := "no reason"
		if review.Reason != nil {
			reason = strconv.Quote(*review.Reason)
		}
		words = append(words, fmt.Sprintf("%s, %s's %s of version %d (%s) carried from %s, %s",
			review.ID, review.Actor.ID, review.State, review.Version, review.Kind, carriedFromWords(review.CarriedFrom), reason))
	}
	return "[" + strings.Join(words, "; ") + "]"
}

// carriedFromApproved is the version of the spec alice approves in every carried-from fixture
// (carriedFromRig.approvedAtFour).
const carriedFromApproved = 4

// carriedFromSpec is the spec of a carried-from issue as draft words it: one requirement, whose
// words name the draft, so each draft changes the spec's text and the Spec query's record.
func carriedFromSpec(draft int) string {
	return fmt.Sprintf(":::requirement{#r1}\nThe carried list comes first, as draft %d words it.\n:::\n", draft)
}

// conversionCarrySentence is the reason the conversion's one-time carry records (R14, docs'
// issueConversion.finish), carrying the approval of version from to the version the conversion
// writes.
func conversionCarrySentence(from int) string {
	return fmt.Sprintf("%s%d: the issue room conversion kept what was approved.", model.CarriedReviewReasonPrefix, from)
}

// carriedFromRig is the server of a carried-from test: its handler, its store and its document
// service, which a conversion runs through (convertDigestIssue). Its documents settle nothing on a
// timer within a test (an hour), as the digest tests' server, so every version a test reads is one
// that a write of its own made, and each test knows its numbers.
type carriedFromRig struct {
	handler   http.Handler
	database  *store.Store
	documents *docs.Service
}

// newCarriedFromRig serves the API over a database of its own and creates project TEST.
func newCarriedFromRig(t *testing.T) carriedFromRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createRoomProject(t, handler, "TEST")
	return carriedFromRig{handler: handler, database: database, documents: deps.Docs.(*docs.Service)}
}

// carriedFromIssue is an issue of TEST that has not converted: its key, its spec, and alice's own
// approval of version 4 of that spec.
type carriedFromIssue struct {
	key, spec string
	approval  model.ArtifactReview
}

// approvedAtFour creates, as alice, an issue of TEST that has not converted (createDigestIssue:
// a live process of the release before stands beside the create alone), whose spec holds draft 1
// as its version 1, uploads drafts 2 to 4 over it as versions 2 to 4, and has alice approve
// version 4. It fails unless each upload wrote the version its draft names and alice approved
// version 4.
func (r carriedFromRig) approvedAtFour(t *testing.T, title string) carriedFromIssue {
	t.Helper()
	issue := createDigestIssue(t, r.handler, r.database, "TEST", title, carriedFromSpec(1), true)
	for draft := 2; draft <= carriedFromApproved; draft++ {
		r.uploadSpec(t, issue.Key, issue.PrimaryArtifactID, carriedFromSpec(draft), draft)
	}
	approval := reviewDocument(t, r.handler, issue.PrimaryArtifactID, "alice", "approved", "")
	if approval.Version != carriedFromApproved {
		t.Fatalf("alice approved version %d of the spec of %s, want version %d", approval.Version, issue.Key, carriedFromApproved)
	}
	return carriedFromIssue{key: issue.Key, spec: issue.PrimaryArtifactID, approval: approval}
}

// uploadSpec uploads markdown as alice over the spec of the issue key, whose id is spec, through
// POST /api/v1/issues/{key}/artifacts, and fails unless the upload wrote version number of it.
func (r carriedFromRig) uploadSpec(t *testing.T, key, spec, markdown string, number int) {
	t.Helper()
	uploaded := uploadDocument(t, r.handler, "/api/v1/issues/"+key, "spec", markdown)
	if uploaded.Artifact.ID != spec || uploaded.Version.Number != number {
		t.Fatalf("the upload over the spec %s of %s wrote version %d of %s, want version %d of the spec",
			spec, key, uploaded.Version.Number, uploaded.Artifact.ID, number)
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

// TestACarriedReviewNamesTheVersionItCarriesFrom is c-carried-from's first case (R27). On an issue
// that has not converted, alice approves version 4 of the spec (carriedFromRig.approvedAtFour). The
// issue's conversion (docs.Service.ConvertIssue) writes version 5 of its spec query, and R14's
// one-time cutover carry carries her approval to version 5: one review of version 5, in her name,
// with the conversion's reason. The reviews route answers it with carried_from_version 4 and the
// review of version 4, her own act, with null written out, never left out; the approval read
// answers approved at version 5, carried from 4; the artifact.approved of the carry carries 4, and
// that of her own approval null. Alice then edits the converted spec and approves the version her
// edit wrote herself: the reviews route and the approval read answer null for that approval, and
// it holds no row.
//
// On a second issue that has not converted, main's text carry still runs: alice approves version
// 4 of its spec, an upload changes the text as version 5, which carries nothing, and an upload of
// version 4's text as version 6 carries her approval to version 6 (docs.CarryUploadedApproval),
// naming 4. A carry of that carry, an upload of the same text as version 7, names 4. Each carry
// reads 4 through the reviews route, the approval read and its artifact.approved, and holds its
// row naming 4. A row of artifact_review_carries written by SQL for the review of version 7,
// naming version 3 while its reason names 4, reads 3 through the reviews route and the approval
// read, so a reader that parses the reason alone fails. Beside each read the test reads by SQL the
// row of the review of version 5 the conversion carried, which names 4 throughout. Last a plan
// review's approval of the second issue's plan answers null through the reviews route and the
// approval read.
func TestACarriedReviewNamesTheVersionItCarriesFrom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newCarriedFromRig(t)

	// The conversion's one-time carry (R14).
	converted := rig.approvedAtFour(t, "Carried by the conversion")
	approved := converted.approval.Version
	convertDigestIssue(t, &digestRig{handler: rig.handler, database: rig.database, documents: rig.documents}, converted.key)
	awaitDigestsFilled(t, rig.database, converted.spec)
	carriedTo := newestVersionOf(t, rig.database, converted.spec)
	if carriedTo != approved+1 {
		t.Fatalf("the conversion of %s left version %d of its spec newest, want version %d, the version of its spec query the conversion writes",
			converted.key, carriedTo, approved+1)
	}
	reviews := carriedFromReviews(t, rig.handler, converted.spec)
	var carry carriedFromReview
	carries := 0
	for _, review := range reviews {
		if review.Version == carriedTo {
			carry, carries = review, carries+1
		}
	}
	if carries != 1 || carry.State != "approved" || carry.Kind != docs.ReviewKindPerson || carry.Actor.ID != "alice" ||
		carry.Reason == nil || *carry.Reason != conversionCarrySentence(approved) {
		t.Fatalf("the reviews of the spec of %s after its conversion read %s, want one review of version %d, alice's approval the conversion carried, with the reason %q",
			converted.key, carriedFromReviewsWords(reviews), carriedTo, conversionCarrySentence(approved))
	}
	requireCarryRow := func(beside string) {
		t.Helper()
		if version, held := carriedFromRow(t, rig.database, carry.ID); !held || version != approved {
			t.Errorf("beside %s the review %s of version %d, which the conversion carried from the approval of version %d, holds %s, want its artifact_review_carries row naming version %d",
				beside, carry.ID, carriedTo, approved, carriedFromRowWords(version, held), approved)
		}
	}

	// The conversion's carry and alice's own approval, through the reviews route, the approval read
	// and the artifact.approved of each.
	requireCarriedFrom(t, fmt.Sprintf("the reviews route, for the review %s the conversion carried to version %d", carry.ID, carriedTo),
		carry.CarriedFrom, &approved)
	firstOwn := carriedFromReviewOf(t, reviews, converted.approval.ID)
	requireCarriedFrom(t, fmt.Sprintf("the reviews route, for alice's own approval %s of version %d", converted.approval.ID, approved),
		firstOwn.CarriedFrom, nil)
	if firstOwn.CarriedFromWritten != "null" {
		t.Errorf("the reviews route writes carried_from_version %q for alice's own approval %s of version %d, want null written out: a member left out reads as no answer, not as her own act",
			firstOwn.CarriedFromWritten, converted.approval.ID, approved)
	}
	requireCarryRow("the reviews route")
	state, version, carried := carriedFromApproval(t, rig.handler, converted.spec)
	if state != "approved" || version != carriedTo {
		t.Errorf("the approval read answers %s at version %d after the conversion's carry, want approved at version %d", state, version, carriedTo)
	}
	requireCarriedFrom(t, fmt.Sprintf("the approval read after the conversion's carry to version %d", carriedTo), carried, &approved)
	requireCarryRow("the approval read")
	requireCarriedFrom(t, fmt.Sprintf("the artifact.approved of the conversion's carry to version %d", carriedTo),
		carriedFromEventOf(t, rig.handler, converted.key, converted.spec, carriedTo), &approved)
	requireCarryRow("the carry's artifact.approved")
	requireCarriedFrom(t, fmt.Sprintf("the artifact.approved of alice's own approval of version %d", approved),
		carriedFromEventOf(t, rig.handler, converted.key, converted.spec, approved), nil)

	// Alice's own approval of a later version is her own act.
	edited := editSpec(t, rig.handler, converted.key, []map[string]any{
		{"op": "replace", "find": fmt.Sprintf("as draft %d words it", approved), "with": "as alice words it after the conversion"},
	})
	awaitDigestsFilled(t, rig.database, converted.spec)
	own := reviewDocument(t, rig.handler, converted.spec, "alice", "approved", "")
	if own.Version <= carriedTo || own.Version < edited.Number {
		t.Fatalf("alice's own approval of the converted spec of %s is of version %d, want the version her edit wrote, %d, after version %d",
			converted.key, own.Version, edited.Number, carriedTo)
	}
	reviews = carriedFromReviews(t, rig.handler, converted.spec)
	requireCarriedFrom(t, fmt.Sprintf("the reviews route, for alice's own approval %s of version %d", own.ID, own.Version),
		carriedFromReviewOf(t, reviews, own.ID).CarriedFrom, nil)
	requireCarryRow("the reviews route after alice's own approval")
	state, version, carried = carriedFromApproval(t, rig.handler, converted.spec)
	if state != "approved" || version != own.Version {
		t.Errorf("the approval read answers %s at version %d after alice's own approval, want approved at version %d", state, version, own.Version)
	}
	requireCarriedFrom(t, fmt.Sprintf("the approval read after alice's own approval of version %d", own.Version), carried, nil)
	requireCarryRow("the approval read after alice's own approval")
	if version, held := carriedFromRow(t, rig.database, own.ID); held {
		t.Errorf("alice's own approval %s of version %d holds an artifact_review_carries row naming version %d, want none: she gave it herself", own.ID, own.Version, version)
	}

	// main's text carry, on a second issue that has not converted: an upload that changes the
	// text carries nothing, and an upload of the approved text carries the approval to its version.
	uploaded := rig.approvedAtFour(t, "Carried by an upload")
	changed := approved + 1
	rig.uploadSpec(t, uploaded.key, uploaded.spec, carriedFromSpec(changed), changed)
	if reviews := carriedFromReviews(t, rig.handler, uploaded.spec); len(reviews) != 1 || reviews[0].ID != uploaded.approval.ID {
		t.Fatalf("the upload of version %d of the spec of %s, which changes its text, leaves the reviews %s, want alice's own approval %s of version %d alone: a change of the text carries nothing",
			changed, uploaded.key, carriedFromReviewsWords(reviews), uploaded.approval.ID, approved)
	}
	requireCarryRow("the reviews route after the upload that changes the text")
	requireUploadCarry := func(review string, number int, what string) {
		t.Helper()
		reviews := carriedFromReviews(t, rig.handler, uploaded.spec)
		requireCarriedFrom(t, fmt.Sprintf("the reviews route, for the review %s %s", review, what),
			carriedFromReviewOf(t, reviews, review).CarriedFrom, &approved)
		requireCarryRow("the reviews route after the review " + what)
		state, version, carried := carriedFromApproval(t, rig.handler, uploaded.spec)
		if state != "approved" || version != number {
			t.Errorf("the approval read of the spec of %s answers %s at version %d after the review %s, want approved at version %d", uploaded.key, state, version, what, number)
		}
		requireCarriedFrom(t, "the approval read after the review "+what, carried, &approved)
		requireCarryRow("the approval read after the review " + what)
		requireCarriedFrom(t, "the artifact.approved of the review "+what,
			carriedFromEventOf(t, rig.handler, uploaded.key, uploaded.spec, number), &approved)
		requireCarryRow("the artifact.approved of the review " + what)
		if version, held := carriedFromRow(t, rig.database, review); !held || version != approved {
			t.Errorf("the review %s %s holds %s, want its artifact_review_carries row naming version %d, the version alice approved",
				review, what, carriedFromRowWords(version, held), approved)
		}
	}
	restored := changed + 1
	rig.uploadSpec(t, uploaded.key, uploaded.spec, versionMarkdown(t, rig.handler, uploaded.spec, approved), restored)
	upload := requireReviewCarriedWith(t, rig.handler, uploaded.spec, restored, approved, restoreSentence(approved), nil)
	requireUploadCarry(upload.ID, restored, fmt.Sprintf("carried to version %d by the upload of version %d's text", restored, approved))
	again := restored + 1
	rig.uploadSpec(t, uploaded.key, uploaded.spec, versionMarkdown(t, rig.handler, uploaded.spec, restored), again)
	second := requireReviewCarriedWith(t, rig.handler, uploaded.spec, again, approved, restoreCarriedSentence(approved, restored), nil)
	requireUploadCarry(second.ID, again, fmt.Sprintf("carried to version %d by the upload of version %d's text, a carry of the carry to version %d", again, restored, restored))
	requireCarriedFrom(t, fmt.Sprintf("the reviews route, for the review %s carried to version %d, once that carry is carried again", upload.ID, restored),
		carriedFromReviewOf(t, carriedFromReviews(t, rig.handler, uploaded.spec), upload.ID).CarriedFrom, &approved)

	// A row written by SQL that names another version than the reason: the reads answer the row's.
	forged := approved - 1
	if _, err := rig.database.Pool.Exec(ctx, `
		insert into artifact_review_carries (review_id, carried_from_version) values ($1::uuid, $2)
		on conflict (review_id) do update set carried_from_version = excluded.carried_from_version`, second.ID, forged); err != nil {
		t.Fatalf("write the artifact_review_carries row of the review %s naming version %d: %v", second.ID, forged, err)
	}
	reviews = carriedFromReviews(t, rig.handler, uploaded.spec)
	if got := carriedFromReviewOf(t, reviews, second.ID).CarriedFrom; got == nil || *got != forged {
		t.Errorf("the reviews route answers carried_from_version %s for the review %s of version %d, whose artifact_review_carries row names version %d and whose reason names version %d, want %d, the row's",
			carriedFromWords(got), second.ID, again, forged, approved, forged)
	}
	requireCarryRow("the reviews route over the row written by SQL")
	if _, _, got := carriedFromApproval(t, rig.handler, uploaded.spec); got == nil || *got != forged {
		t.Errorf("the approval read answers carried_from_version %s for the review %s of version %d, whose artifact_review_carries row names version %d and whose reason names version %d, want %d, the row's",
			carriedFromWords(got), second.ID, again, forged, approved, forged)
	}
	requireCarryRow("the approval read over the row written by SQL")

	// A plan review's approval is no carry.
	plan := uploadDocument(t, rig.handler, "/api/v1/issues/"+uploaded.key, "plan.md", "# Plan\n\nShip behind the flag, one region at a time.\n")
	recorded := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews",
		map[string]any{"actor": sessionActor(), "version": plan.Version.Number})
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record the plan review of version %d of the plan: status=%d body=%s", plan.Version.Number, recorded.Code, recorded.Body.String())
	}
	planReview := carriedFromReviewOf(t, carriedFromReviews(t, rig.handler, plan.Artifact.ID), decodeBody[carriedFromReview](t, recorded).ID)
	if planReview.Kind != "plan_review" {
		t.Errorf("the plan's review reads kind %q, want plan_review", planReview.Kind)
	}
	requireCarriedFrom(t, fmt.Sprintf("the reviews route, for the plan review %s of version %d of the plan", planReview.ID, plan.Version.Number),
		planReview.CarriedFrom, nil)
	_, _, carried = carriedFromApproval(t, rig.handler, plan.Artifact.ID)
	requireCarriedFrom(t, "the approval read of the plan its plan review approved", carried, nil)
	requireCarryRow("the plan review's reads")
}

// TestAnUnfilledCarriedReviewTakesItsSourceFromItsReason is c-carried-from's second case (R27).
// On an issue that has not converted, alice approves version 4 of the spec
// (carriedFromRig.approvedAtFour), and a named version that changes nothing writes version 5,
// which holds version 4's text, so main's text carry, the restore, carries her approval to it: the
// carry holds its artifact_review_carries row naming 4. The release before this build writes the
// same review, its reason and its artifact.approved, and no row, so the test takes the row off by
// SQL and holds the review as that release leaves it. Before the backfill, the reviews route and
// the approval read answer 4 for it, through its reason; alice's own approval of version 4 answers
// null and holds no row. The backfill (graphcensus.Backfill) writes the carried review's row naming
// 4, and the same reads answer the same after it.
func TestAnUnfilledCarriedReviewTakesItsSourceFromItsReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newCarriedFromRig(t)
	fixture := rig.approvedAtFour(t, "Carried before its row")
	approved := fixture.approval.Version
	carriedTo := approved + 1

	named := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+fixture.spec+"/versions",
		map[string]string{"summary": "nothing changed"}, "alice")
	if named.Code != http.StatusCreated {
		t.Fatalf("name version %d of the spec of %s, which changes nothing: status=%d body=%s", carriedTo, fixture.key, named.Code, named.Body.String())
	}
	waitForArtifactVersion(t, rig.handler, fixture.spec, carriedTo)
	carry := requireReviewCarriedWith(t, rig.handler, fixture.spec, carriedTo, approved, restoreSentence(approved), nil)
	if version, held := carriedFromRow(t, rig.database, carry.ID); !held {
		t.Errorf("the review of version %d carried from the approval of version %d holds no artifact_review_carries row", carriedTo, approved)
	} else if version != approved {
		t.Errorf("the review of version %d carried from the approval of version %d holds the artifact_review_carries row naming version %d, want %d",
			carriedTo, approved, version, approved)
	}

	// The review as the release before writes it: its carried reason, and no row.
	if _, err := rig.database.Pool.Exec(ctx, `delete from artifact_review_carries where review_id = $1::uuid`, carry.ID); err != nil {
		t.Fatalf("leave the review %s with no artifact_review_carries row, as the release before writes it: %v", carry.ID, err)
	}
	requireTakenFromItsReason := func(when string) {
		t.Helper()
		reviews := carriedFromReviews(t, rig.handler, fixture.spec)
		requireCarriedFrom(t, fmt.Sprintf("%s the reviews route, for the review %s of version %d, whose reason names version %d", when, carry.ID, carriedTo, approved),
			carriedFromReviewOf(t, reviews, carry.ID).CarriedFrom, &approved)
		requireCarriedFrom(t, fmt.Sprintf("%s the reviews route, for alice's own approval %s of version %d", when, fixture.approval.ID, approved),
			carriedFromReviewOf(t, reviews, fixture.approval.ID).CarriedFrom, nil)
		state, version, carried := carriedFromApproval(t, rig.handler, fixture.spec)
		if state != "approved" || version != carriedTo {
			t.Errorf("%s the approval read answers %s at version %d, want approved at version %d", when, state, version, carriedTo)
		}
		requireCarriedFrom(t, when+" the approval read", carried, &approved)
		if version, held := carriedFromRow(t, rig.database, fixture.approval.ID); held {
			t.Errorf("%s alice's own approval %s holds an artifact_review_carries row naming version %d, want none: she gave it herself",
				when, fixture.approval.ID, version)
		}
	}
	requireTakenFromItsReason("before the backfill")

	if _, err := graphcensus.Backfill(ctx, rig.database, io.Discard); err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	if version, held := carriedFromRow(t, rig.database, carry.ID); !held || version != approved {
		t.Errorf("after the backfill the review %s of version %d, whose reason names version %d, holds %s, want its artifact_review_carries row naming version %d",
			carry.ID, carriedTo, approved, carriedFromRowWords(version, held), approved)
	}
	requireTakenFromItsReason("after the backfill")
}
