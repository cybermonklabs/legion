package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An answer an agent made for its person under an act - the agent's session and the scopes its
// credential held - is held on the ask's node and its row like any other answer
// (dispatch://INFRA-1150 R3), so on a fleet of one build it writes no version and the person's
// approval stands through it, whatever the act holds. The release before carried an approval to
// the version an answer wrote, and read the act's scopes to decide whether it could; that carry is
// gone, and no carry reads an act: the version the roll window writes while two builds serve is
// carried by R10's case, which compares the two renderings and each ask with its node alone
// (carryVerdict, TestCarryVerdictOverTheApprovedAndTheNewVersion).

// actAnswerSpec is the two-block document these tests decide: region, the block an agent answers,
// and wave, the block the person answers beside it.
const actAnswerSpec = "Context\n\n" +
	":::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n\n" +
	":::ask{#wave urgency=\"low\" multiple=\"false\"}\nHow many regions take the first wave?\n\n- One: One region.\n- Two: Two regions.\n:::\n"

// seededActDocument creates actAnswerSpec as a document, starts a service that settles a live
// write, seeds the document's live text and waits until settlement has indexed both blocks as
// asks of the document - the rows the carry reads.
//
// The settle is a second, as the other carry tests that answer and then name a version use
// (approval_carry_approved_test.go), and not the 20 ms a test that only seeds can afford: a
// settlement firing while one of these tests holds its joined transaction takes the document's
// rows in the other order and the two deadlock, which `-race` is slow enough to hit. The seeding
// below schedules one settlement itself and waits up to three seconds for it, so a second costs
// the seed nothing.
func seededActDocument(t *testing.T) (*store.Store, *Service, string) {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, actAnswerSpec)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Second})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, actAnswerSpec)
	service.ScheduleSettlement(artifactID)
	waitFor(t, 3*time.Second, "settlement to index both blocks", func() bool {
		var rows int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from asks where block_artifact_id = $1 and block_id = any($2::text[])
		`, artifactID, []string{"region", "wave"}).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows == 2
	})
	return database, service, artifactID
}

// answerWithAct is the answer login gave now, carrying act.
func answerWithAct(login string, selected []string, act *model.ActorAct) model.AskAnswer {
	return model.AskAnswer{User: login, Selected: selected, At: time.Now().UTC(), Act: act}
}

// An answer an agent made for its person carries no approval, whatever its act holds, and needs
// none: on a fleet of one build an answer is held on the ask's node and its row alone, so it
// changes no byte of the document, the settlement after it writes no version, and the approval
// the person gave stands at the version she gave it to (dispatch://INFRA-1150 R3: "Once the whole
// fleet runs this build, a person's answer changes no byte of any document, and an approval of
// that document stands through it"). That holds for an act holding the person scope alone, for
// one holding approve, and for a narrow agent's answer beside the person's own, so no act's scopes
// decide anything here. The release before carried an approval to the version an answer wrote,
// and only where the act held approve; that carry is gone, and the version the roll window writes
// while two builds serve is carried by R10's case, whoever answered
// (TestCarryVerdictOverTheApprovedAndTheNewVersion, api's
// TestWhileTwoBuildsServeAnAnswerKeepsTheApprovalCurrent).
func TestAnActWithoutApproveCarriesNoApproval(t *testing.T) {
	narrow := &model.ActorAct{Session: "session-agent", TokenID: "token-1", TokenName: "planner", Scopes: []string{model.ScopePerson}}
	approving := &model.ActorAct{Session: "session-agent", TokenID: "token-1", TokenName: "architect", Scopes: []string{model.ScopePerson, model.ScopeApprove}}
	for _, test := range []struct {
		name         string
		region, wave *model.ActorAct
	}{
		{name: "an act holding person alone", region: narrow, wave: narrow},
		{name: "an act holding approve", region: approving, wave: approving},
		// The person answers the other block themselves, at a browser, which carries no act.
		{name: "a narrow agent's answer beside the person's own", region: narrow, wave: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, service, artifactID := seededActDocument(t)
			if latest := latestVersionNumber(t, service, artifactID); latest != 1 {
				t.Fatalf("the seeded document stands at version %d, want 1, the version alice approves", latest)
			}
			approveStoredVersion(t, database, artifactID, 1, "alice")
			before, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatal(err)
			}

			answerActAsk(t, service, artifactID, "region", answerWithAct("alice", []string{"East"}, test.region))
			answerActAsk(t, service, artifactID, "wave", answerWithAct("alice", []string{"One"}, test.wave))
			settleCurrentGeneration(t, service, artifactID)

			if after, err := service.Text(context.Background(), artifactID); err != nil || after != before {
				t.Fatalf("the answers left the document reading %q (%v), want it as it was: no byte of it holds an answer", after, err)
			}
			if latest := latestVersionNumber(t, service, artifactID); latest != 1 {
				t.Fatalf("the answers left the document at version %d, want version 1, the one alice approved", latest)
			}
			if reviews, approvals := carriedReviews(t, database, artifactID); reviews != 1 || approvals != 0 {
				t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval of version 1 alone, standing, and no event",
					reviews, approvals)
			}
			for _, block := range []string{"region", "wave"} {
				if ask := blockAskOn(t, service, artifactID, block); ask.State != "answered" || ask.Answer == nil || ask.Answer.User != "alice" {
					t.Fatalf("the ask of %s reads %s with answer %+v, want it answered for alice", block, ask.State, ask.Answer)
				}
			}
		})
	}
}

// answerActAsk answers the ask of the block blockID of artifactID with answer as the answer route
// writes it (settleAskAs), in the name of the person answer names, made under the answer's act.
func answerActAsk(t *testing.T, service *Service, artifactID, blockID string, answer model.AskAnswer) {
	t.Helper()
	person := model.Actor{Kind: "user", ID: answer.User, Act: answer.Act}
	change := asks.Answered(answer)
	change.Who, change.At = person, answer.At
	settleAskAs(t, service, artifactID, blockID, blockAskOn(t, service, artifactID, blockID).ID, person, change)
}
