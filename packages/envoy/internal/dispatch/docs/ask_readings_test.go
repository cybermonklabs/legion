package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// shipAsk is seedIndexedShipAsk's ask under blockID, closing with no reading.
func shipAsk(blockID string) string {
	return ":::ask{#" + blockID + " urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n" +
		"- Ship: Release it\n- Hold: Wait for review\n:::\n"
}

// readingNode is the reading r1, as a browser edit appends it to an ask (appendToAsk).
func readingNode() *pmdoc.Node {
	return &pmdoc.Node{
		Type:     "reading",
		Attrs:    pmdoc.Attrs{pmdoc.BlockIDAttr: "r1"},
		Children: []*pmdoc.Node{{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "The agent reads it so."}}}},
	}
}

// shipAskWithReading is seedIndexedShipAsk's ask closing with one reading, r1.
func shipAskWithReading(blockID string) string {
	return "::::ask{#" + blockID + " urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n" +
		"- Ship: Release it\n- Hold: Wait for review\n\n" +
		":::reading{#r1}\nThe agent reads the answer as shipping this week.\n:::\n::::\n"
}

// A reading reads a decision, and a person's ask is one only once they answer it, so a reading
// inside an ask that holds no answer is refused on a write that takes markdown - a new version
// and a document's first text here; a rebuild from supplied markdown runs the same check - naming
// the reading and the ask and saying the ask has no answer yet; a browser's write, which is never
// refused, has the ask block flagged `invalid` by settlement with the same reason, and its row
// left as it was. Once the ask is answered, the same markdown is taken.
func TestAReadingInAnAskWithNoAnswerIsRefused(t *testing.T) {
	service, artifactID := seedIndexedShipAsk(t, "ship")
	ctx := context.Background()
	var askID, issueKey string
	if err := service.store.Pool.QueryRow(ctx, `
		select id::text, issue_key from asks where block_artifact_id = $1 and block_id = 'ship'
	`, artifactID).Scan(&askID, &issueKey); err != nil {
		t.Fatalf("read the seeded ask: %v", err)
	}
	want := `reading "r1" stands in the ask dispatch://` + issueKey + `/ask/` + askID +
		`, and that ask has no answer yet: a reading reads a decision, and an ask is one only once a person answers it, so write the reading after the answer`
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

	_, err := joinedReplaceText(service, artifactID, shipAskWithReading("ship"), actor)
	var invalid *ErrInvalidAskBlock
	if !errors.As(err, &invalid) || invalid.Error() != want {
		t.Fatalf("a new version holding a reading in the open ask = %v, want it refused with\n%s", err, want)
	}

	second := createIssueDocument(t, service.store, 2, "# Second")
	err = joinedWrite(service, func(ctx context.Context) error {
		_, err := service.SeedText(ctx, second, shipAskWithReading("fresh"), actor)
		return err
	})
	const fresh = `reading "r1" stands in the ask of ask block "fresh", which no ask row holds yet, and that ask has no answer yet: a reading reads a decision, and an ask is one only once a person answers it, so write the reading after the answer`
	if !errors.As(err, &invalid) || invalid.Error() != fresh {
		t.Fatalf("a first text holding a reading in a new ask = %v, want it refused with\n%s", err, fresh)
	}

	editLiveTree(t, service, artifactID, appendToAsk(&pmdoc.Node{
		Type:     "reading",
		Attrs:    pmdoc.Attrs{pmdoc.BlockIDAttr: "r1"},
		Children: []*pmdoc.Node{{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "The agent reads it so."}}}},
	}))
	settleCurrentGeneration(t, service, artifactID)
	assertAskMarkedInvalidAndKept(t, service, artifactID, "ship", want)

	answerBlockAs(t, service, artifactID, "ship", "alice", []string{"Ship"})
	if _, err := joinedReplaceText(service, artifactID, shipAskWithReading("ship"), actor); err != nil {
		t.Fatalf("a new version holding a reading in the answered ask = %v, want it taken", err)
	}
}

// A block copied from an answered ask on another document of its issue opens no ask of its own
// and shows its source's answer (copiedAskSources), so a reading inside it stands in an ask that
// holds an answer. A browser's write of such a reading into a copy settled without one is kept by
// settlement: the block opens no ask, shows the source's answer, names its source, keeps its
// reading and is flagged nothing. A copy's first text carrying the reading (SeedText, a write that
// takes markdown with no tree before it, so every reading in it is introduced) is taken, and
// settlement keeps the block the same way.
func TestAReadingInACopyOfAnAnsweredAskIsKept(t *testing.T) {
	service, original := seedIndexedShipAsk(t, "ship")
	answerBlockAsk(t, service, original)
	source := blockAskOn(t, service, original, "ship")
	if source.State != asks.StateAnswered {
		t.Fatalf("the source ask reads %s, want it answered, so this test would prove nothing", source.State)
	}

	edited := createIssueDocument(t, service.store, 1, "# Edited copy")
	seedServiceText(t, service, edited, "Record\n\n"+shipAsk("ship"))
	settleCurrentGeneration(t, service, edited)
	editLiveTree(t, service, edited, appendToAsk(readingNode()))
	settleCurrentGeneration(t, service, edited)
	requireCopyKeptWithReading(t, service, edited, source.ID, "the copy a browser wrote the reading into")

	written := createIssueDocument(t, service.store, 1, "# Copy")
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	if err := joinedWrite(service, func(ctx context.Context) error {
		_, err := service.SeedText(ctx, written, "Record\n\n"+shipAskWithReading("ship"), actor)
		return err
	}); err != nil {
		t.Fatalf("a first text holding a reading in a copy of the answered ask = %v, want it taken", err)
	}
	settleCurrentGeneration(t, service, written)
	requireCopyKeptWithReading(t, service, written, source.ID, "the copy written with its reading")
}

// requireCopyKeptWithReading holds document, a copy, to its settlement having kept its ask block
// ship as a copy of source: no ask of its own, the block's entry in the asks member an agent reads
// beside the text (TextWithAsks) showing the source's answer and naming it, while the block in the
// document holds only what a version keeps of it on a fleet of one build - the source's state and
// its provenance (keptAskState) - and none of the answer's words or selection, its reading r1 still
// in it, and no block.invalid written.
func requireCopyKeptWithReading(t *testing.T, service *Service, document, source, what string) {
	t.Helper()
	if count := asksOn(t, service, document); count != 0 {
		t.Errorf("%s opened %d asks, want none", what, count)
	}
	ask := firstAsk(liveTree(t, service, document))
	if invalid, flagged := ask.Attrs["invalid"]; flagged && invalid != nil {
		t.Errorf("%s is flagged invalid: %v", what, invalid)
	}
	if state, copied := ask.Attrs["state"], ask.Attrs["copied_from"]; state != asks.StateAnswered || copied != source {
		t.Errorf("%s holds state %v copied from %v in the document, want what a version keeps of its source's ask: answered, copied from %s", what, state, copied, source)
	}
	for _, name := range []string{"selected", "answer"} {
		if value, held := ask.Attrs[name]; held {
			t.Errorf("%s holds %s=%v in the document, want none of the answer's words", what, name, value)
		}
	}
	text, err := service.TextWithAsks(context.Background(), document, false)
	if err != nil {
		t.Fatalf("read %s as an agent reads it: %v", what, err)
	}
	requireNoAskServerAttributes(t, document, text.Markdown)
	if len(text.Asks) != 1 || text.Asks[0].BlockID != "ship" || text.Asks[0].State != asks.StateAnswered || text.Asks[0].CopiedFrom != source {
		t.Errorf("%s shows an agent the asks %s, want ship answered as its source %s is, naming it", what, askLines(text.Asks), source)
	}
	if readings := closingReadings(ask); !slices.Equal(readings, []askReading{{ask: "ship", reading: "r1", kind: "reading"}}) {
		t.Errorf("%s closes with %+v, want its reading r1 kept", what, readings)
	}
	requireNoBlockInvalid(t, service, what)
}

// requireNoBlockInvalid fails the test where any settlement wrote a block.invalid.
func requireNoBlockInvalid(t *testing.T, service *Service, what string) {
	t.Helper()
	var reasons []string
	rows, err := service.store.Pool.Query(context.Background(), `select payload->>'reason' from events where type = 'block.invalid' order by id`)
	if err == nil {
		reasons, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil {
		t.Fatalf("read the block.invalid events: %v", err)
	}
	if len(reasons) > 0 {
		t.Errorf("after %s the settlements wrote block.invalid %q, want none", what, reasons)
	}
}

// An ask settlement retracted while it held an answer, whose block comes back holding a reading,
// is restored with that answer (restoreRetractedAsk, which gives the ask back the state its newest
// ask.answered records), so the reading stands in an ask that holds one. Settlement restores the
// ask and flags nothing, whether a browser's write brought the block back or a write that takes
// markdown did, which is taken.
func TestARetractedAnsweredAskReturningWithAReadingIsRestored(t *testing.T) {
	for _, back := range []struct {
		name  string
		write func(t *testing.T, service *Service, artifactID string)
	}{
		{"a browser's write", func(t *testing.T, service *Service, artifactID string) {
			editLiveTree(t, service, artifactID, appendBlocks(t, shipAskWithReading("ship")))
		}},
		{"a new version", func(t *testing.T, service *Service, artifactID string) {
			actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
			if _, err := joinedReplaceText(service, artifactID, "Context\n\n"+shipAskWithReading("ship"), actor); err != nil {
				t.Fatalf("a new version bringing the retracted answered ask back with a reading = %v, want it taken", err)
			}
		}},
	} {
		t.Run(back.name, func(t *testing.T) {
			service, artifactID := seedIndexedShipAsk(t, "ship")
			answerAsRecorded(t, service, artifactID, "ship", []string{"Ship"})
			editLiveTree(t, service, artifactID, func(*pmdoc.Node) *pmdoc.Node {
				return &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "Context"}}}}}
			})
			settleCurrentGeneration(t, service, artifactID)
			retractAnswered(t, service, artifactID, "ship")
			if retracted := blockAskOn(t, service, artifactID, "ship"); !settlementRetracted(retracted) {
				t.Fatalf("the ask reads %s after its retraction, want settlement's retraction", retracted.State)
			}

			back.write(t, service, artifactID)
			settleCurrentGeneration(t, service, artifactID)
			restored := blockAskOn(t, service, artifactID, "ship")
			if restored.State != asks.StateAnswered || restored.Resolution != nil || restored.Answer == nil || !slices.Equal(restored.Answer.Selected, []string{"Ship"}) {
				t.Errorf("the ask reads state %s, resolution %+v and answer %+v once its block came back, want it restored with its answer Ship",
					restored.State, restored.Resolution, restored.Answer)
			}
			ask := firstAsk(liveTree(t, service, artifactID))
			if invalid, flagged := ask.Attrs["invalid"]; flagged && invalid != nil {
				t.Errorf("the returning ask block is flagged invalid: %v", invalid)
			}
			if readings := closingReadings(ask); !slices.Equal(readings, []askReading{{ask: "ship", reading: "r1", kind: "reading"}}) {
				t.Errorf("the returning ask block closes with %+v, want its reading r1 kept", readings)
			}
			requireNoBlockInvalid(t, service, "the block's return")
		})
	}
}

// answerAsRecorded answers the ask of blockID on artifactID as alice the way the answer route
// records an answer: through the one writer of an ask's answer state (asks.Settle), with the
// ask.answered event that records it - the event a restoration reads the state back from
// (stateBeforeRetraction) - and the block kept in step with it (KeepAskBlockInStep, which on a
// fleet of one build writes the ones a version keeps, R3), in one joined transaction.
func answerAsRecorded(t *testing.T, service *Service, artifactID, blockID string, selected []string) {
	t.Helper()
	ask := blockAskOn(t, service, artifactID, blockID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	answer := model.AskAnswer{User: alice.ID, Selected: selected, At: time.Now().UTC()}
	joined(t, service, func(ctx context.Context, tx pgx.Tx) error {
		owner, _, err := lockArtifactOwner(ctx, tx, artifactID)
		if err != nil {
			return err
		}
		var held graphstore.Uncaused
		change := asks.Answered(answer)
		change.Who, change.At, change.Held = alice, answer.At, &held
		settled, err := asks.Settle(ctx, tx, ask.ID, change)
		if err != nil {
			return err
		}
		ask.State, ask.Answer, ask.Resolution = settled.State, settled.Answer, settled.Resolution
		appended, err := service.events.Append(ctx, tx, documentAskEvent(owner, artifactID, "ask.answered", alice,
			model.NewAskEventPayload(ask, model.ReferenceChanges{})))
		if err != nil {
			return err
		}
		if err := graphstore.Caused(ctx, tx, held, appended.ID); err != nil {
			return err
		}
		return service.KeepAskBlockInStep(ctx, ask, alice)
	})
}

// retractAnswered writes onto the answered ask of blockID on artifactID the retraction settlement
// writes when an ask's block leaves its document (nameVersion: by SettlementActor, its reason
// naming the document's latest version), with the ask.resolved that records it. The answer goes
// with it, since the asks table holds a resolved row to no answer (asks_answer_state_check). No
// writer of this release retracts an answered ask - settlement retracts an open one alone - so the
// test writes the store as such a retraction leaves it; the restoration of one reads the answer
// back from its ask.answered (stateBeforeRetraction).
func retractAnswered(t *testing.T, service *Service, artifactID, blockID string) {
	t.Helper()
	ask := blockAskOn(t, service, artifactID, blockID)
	if ask.State != asks.StateAnswered {
		t.Fatalf("the ask of %s reads %s, want it answered before its retraction", blockID, ask.State)
	}
	joined(t, service, func(ctx context.Context, tx pgx.Tx) error {
		owner, _, err := lockArtifactOwner(ctx, tx, artifactID)
		if err != nil {
			return err
		}
		var version int
		if err := tx.QueryRow(ctx, `select max(number) from artifact_versions where artifact_id = $1`, artifactID).Scan(&version); err != nil {
			return err
		}
		resolution := model.AskResolution{
			Kind: "retracted", Reason: fmt.Sprintf("%s %d", SettlementRetractionReason, version), Actor: SettlementActor, At: time.Now().UTC(),
		}
		var held graphstore.Uncaused
		change := asks.Recorded(asks.StateResolved, nil, &resolution)
		change.Who, change.At, change.Held = SettlementActor, resolution.At, &held
		settled, err := asks.Settle(ctx, tx, ask.ID, change)
		if err != nil {
			return err
		}
		ask.State, ask.Answer, ask.Resolution = settled.State, settled.Answer, settled.Resolution
		appended, err := service.events.Append(ctx, tx, documentAskEvent(owner, artifactID, "ask.resolved", SettlementActor,
			model.NewAskEventPayload(ask, model.ReferenceChanges{})))
		if err != nil {
			return err
		}
		return graphstore.Caused(ctx, tx, held, appended.ID)
	})
}
