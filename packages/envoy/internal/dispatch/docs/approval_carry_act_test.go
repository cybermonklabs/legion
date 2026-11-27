package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// An answer an agent made for its person under an act - the agent's session and the scopes its
// credential held - is held on the ask's node and its row like any other answer, and writes one
// version. When the answer is that version's whole change (changesAskAttributesAlone: text a
// person was editing in the browser can settle into the same version), the version changes only
// the server-owned attributes of the ask's block that a version keeps (keptAskState: the state,
// who answered and when), so R10's case carries the person's approval across it, whatever the act
// holds. The act's scopes decide the answer carry alone (answerCase, approverAnswers), the case
// for a block the approved version did not hold as the version shows it, such as one added since
// the approval: there an answer carries the approval only where the person could have approved
// with it, at a browser or under an act holding the approve scope.

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
// below schedules one settlement itself and waits for it, so a second costs the seed nothing.
func seededActDocument(t *testing.T) (*store.Store, *Service, string) {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, actAnswerSpec)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Second})
	testcleanup.Add(t, func() { _ = service.Shutdown(context.Background()) })
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

// An answer an agent made for its person to a block the approved version held carries the approval
// as any answer does, whatever its act holds: the answers write one version, which changes only
// the server-owned attributes of the ask blocks a version keeps (keptAskState), never the answers'
// words, and R10's case carries the approval the person gave to it, naming that change alone. That
// holds for an act holding the person scope alone, for one holding approve, and for a narrow
// agent's answer beside the person's own, so no act's scopes decide anything here
// (TestCarryVerdictOverTheApprovedAndTheNewVersion, api's
// TestWhileTwoBuildsServeAnAnswerKeepsTheApprovalCurrent); they decide the carry of an answer to a
// block added since the approval (TestAnAnswerToABlockAddedSinceTheApprovalCarriesIt).
func TestNoActDecidesTheCarryOfTheVersionAnAnswerWrites(t *testing.T) {
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
			approved := latestVersionNumber(t, service, artifactID)
			approveStoredVersion(t, database, artifactID, approved, "alice")
			before, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatal(err)
			}

			answerActAsk(t, service, artifactID, "region", answerWithAct("alice", []string{"East"}, test.region))
			answerActAsk(t, service, artifactID, "wave", answerWithAct("alice", []string{"One"}, test.wave))
			settleCurrentGeneration(t, service, artifactID)

			after, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatal(err)
			}
			if same, err := renderedWithoutAskServerAttributes(before, after); err != nil || !same {
				t.Fatalf("the answers left the document reading %q (%v), want it as it was but for its ask blocks' server-owned attributes:\n%s", after, err, before)
			}
			if strings.Count(after, ` state="answered" answered_by="alice" answered_at="`) != 2 || strings.Contains(after, "selected=") {
				t.Fatalf("the answers left the document reading %q, want each block holding its state, answerer and time and no answer's words", after)
			}
			if latest := latestVersionNumber(t, service, artifactID); latest != approved+1 {
				t.Fatalf("the answers left the document at version %d, want version %d, the one version they wrote", latest, approved+1)
			}
			if reviews, approvals := carriedReviews(t, database, artifactID); reviews != 2 || approvals != 1 {
				t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval of version %d carried to version %d",
					reviews, approvals, approved, approved+1)
			}
			if reason := approvedReasonAt(t, service, artifactID, approved+1); reason != attributeRemovalReason(approved) {
				t.Fatalf("the review carried to version %d reads %q, want %q", approved+1, reason, attributeRemovalReason(approved))
			}
			for _, block := range []string{"region", "wave"} {
				if ask := blockAskOn(t, service, artifactID, block); ask.State != "answered" || ask.Answer == nil || ask.Answer.User != "alice" {
					t.Fatalf("the ask of %s reads %s with answer %+v, want it answered for alice", block, ask.State, ask.Answer)
				}
			}
		})
	}
}

// renderedWithoutAskServerAttributes reports whether a and b, two markdown renderings of a
// document, are the same once every ask block of each has its server-owned attributes taken off
// (withoutAskServerAttributes), as R5's case compares two versions.
func renderedWithoutAskServerAttributes(a, b string) (bool, error) {
	trees := make([]*pmdoc.Node, 2)
	for index, markdown := range []string{a, b} {
		tree, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			return false, err
		}
		trees[index], _ = withoutAskServerAttributes(tree)
	}
	return renderSame(trees[0], trees[1])
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

// waveBlock is the ask block an agent adds to a document after the person approved it.
const waveBlock = ":::ask{#wave urgency=\"low\" multiple=\"false\"}\nHow many regions take the first wave?\n\n- One: One region.\n- Two: Two regions.\n:::\n"

// An agent adds an ask block to a document after the person approved it, and the person answers
// it: that answer is the whole change since the version they approved, so the version it settles
// into carries their approval (answerCase, LEGION-573), naming the answer as that change, and they
// are not asked to approve again what they just decided. The version the agent's addition wrote
// carries nothing, since nobody had answered the block. The answer carries the approval only where
// the person could have approved with it: at a browser, or by an agent under an act holding the
// approve scope. An agent's answer under the person scope alone, and another person's answer,
// carry nothing.
func TestAnAnswerToABlockAddedSinceTheApprovalCarriesIt(t *testing.T) {
	narrow := &model.ActorAct{Session: "session-agent", TokenID: "token-1", TokenName: "planner", Scopes: []string{model.ScopePerson}}
	approving := &model.ActorAct{Session: "session-agent", TokenID: "token-1", TokenName: "architect", Scopes: []string{model.ScopePerson, model.ScopeApprove}}
	for _, test := range []struct {
		name    string
		by      string
		act     *model.ActorAct
		carries bool
	}{
		{name: "the person's own answer at a browser", by: "alice", carries: true},
		{name: "an agent's answer under an act holding approve", by: "alice", act: approving, carries: true},
		{name: "an agent's answer under an act holding the person scope alone", by: "alice", act: narrow},
		{name: "another person's answer", by: "bob"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			service, artifactID := seedIndexedShipAsk(t, "ship")
			database := service.store
			approved := latestVersionNumber(t, service, artifactID)
			approveStoredVersion(t, database, artifactID, approved, "alice")
			var approvedAt time.Time
			if err := database.Pool.QueryRow(ctx, `select created_at from artifact_reviews where artifact_id = $1`, artifactID).Scan(&approvedAt); err != nil {
				t.Fatalf("read when alice approved: %v", err)
			}

			text, err := service.Text(ctx, artifactID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := joinedReplaceText(service, artifactID, text+"\n"+waveBlock, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}); err != nil {
				t.Fatalf("the agent's addition of the block: %v", err)
			}
			settleCurrentGeneration(t, service, artifactID)
			added := latestVersionNumber(t, service, artifactID)
			if ask := blockAskOn(t, service, artifactID, "wave"); added <= approved || ask.State != asks.StateOpen {
				t.Fatalf("the agent's addition left the document at version %d with the block's ask %s, want a version past %d and the ask open", added, ask.State, approved)
			}
			if reviews, approvals := carriedReviews(t, database, artifactID); reviews != 1 || approvals != 0 {
				t.Fatalf("the agent's addition left reviews = %d, artifact.approved events = %d; want alice's approval alone, carried nowhere", reviews, approvals)
			}

			answerActAsk(t, service, artifactID, "wave", model.AskAnswer{User: test.by, Selected: []string{"One"}, At: approvedAt.Add(time.Second), Act: test.act})
			settleCurrentGeneration(t, service, artifactID)
			answered := latestVersionNumber(t, service, artifactID)
			if answered <= added {
				t.Fatalf("the answer left the document at version %d, want a version past %d", answered, added)
			}
			reviews, approvals := carriedReviews(t, database, artifactID)
			if !test.carries {
				if reviews != 1 || approvals != 0 {
					t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval of version %d carried nowhere", reviews, approvals, approved)
				}
				return
			}
			if reviews != 2 || approvals != 1 {
				t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval of version %d carried to version %d", reviews, approvals, approved, answered)
			}
			if reason, want := approvedReasonAt(t, service, artifactID, answered), fmt.Sprintf(carriedReasonPrefix, approved)+answerCase.change+carriedReasonEnd; reason != want {
				t.Fatalf("the review carried to version %d reads %q, want %q", answered, reason, want)
			}
		})
	}
}
