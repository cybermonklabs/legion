package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

func testerQuestion(id, identity, session string, received time.Time) AnswerQuestion {
	return AnswerQuestion{
		ID: id, Project: "legion", Identity: identity, Session: session, Token: testerToken, Issue: "LEGION-209", Role: claim.RoleTester,
		Kind: "message", Handoff: []byte(`{"target_session":"` + session + `"}`), ReceivedAt: received,
	}
}

// A question handed off again — across a restart too, since the row is the record — is the same
// question: its deliveries count up and nothing else changes, so it is answered once. A newer
// conversation of the same agent is still that question; the same identity for another agent is a
// question of its own.
func TestAQuestionHandedOffAgainIsRecordedOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := migratedStore(t)
	first, inserted, err := s.ReceiveQuestion(ctx, testerQuestion("q1", "dispatch:m1", "ses_tester", at(1)))
	if err != nil || !inserted || first.State != QuestionReceived || first.Deliveries != 1 {
		t.Fatalf("first hand-off = %+v, %t, %v; want a received question", first, inserted, err)
	}
	moved := first
	moved.State, moved.ReplyText, moved.UpdatedAt = QuestionAppended, "Plum.", at(2)
	if err := s.UpdateQuestion(ctx, moved, QuestionReceived); err != nil {
		t.Fatal(err)
	}
	again, inserted, err := s.ReceiveQuestion(ctx, testerQuestion("q1-again", "dispatch:m1", "ses_tester", at(3)))
	if err != nil || inserted {
		t.Fatalf("second hand-off inserted = %t, %v; want the known question", inserted, err)
	}
	if again.ID != "q1" || again.State != QuestionAppended || again.ReplyText != "Plum." || again.Deliveries != 2 || !again.LastSeenAt.Equal(at(3)) {
		t.Fatalf("second hand-off = %+v, want q1 as it was with two deliveries, last seen at 3", again)
	}
	newer := testerQuestion("q1-newer", "dispatch:m1", "ses_tester_2", at(4))
	same, inserted, err := s.ReceiveQuestion(ctx, newer)
	if err != nil || inserted || same.ID != "q1" || same.Session != "ses_tester" {
		t.Fatalf("the message handed off for a newer conversation of the agent = %+v, %t, %v; want q1 as recorded", same, inserted, err)
	}
	other := testerQuestion("q2", "dispatch:m1", "ses_reviewer", at(5))
	other.Token, other.Role = "legion-legion-legion-209-reviewer", claim.RoleReviewer
	if _, inserted, err := s.ReceiveQuestion(ctx, other); err != nil || !inserted {
		t.Fatalf("the same comment to another agent inserted = %t, %v; want a question of its own", inserted, err)
	}
}

// An update moves a question only from a state it names, so two writers never both move it; the
// open questions are the ones with work left, oldest first.
func TestAQuestionMovesOnlyFromTheStateItIsIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := migratedStore(t)
	for i, id := range []string{"qa", "qb", "qc", "qd"} {
		if _, _, err := s.ReceiveQuestion(ctx, testerQuestion(id, "dispatch:"+id, "ses_tester", at(i))); err != nil {
			t.Fatal(err)
		}
	}
	posted := AnswerQuestion{ID: "qa", State: QuestionPosted, UpdatedAt: at(9)}
	if err := s.UpdateQuestion(ctx, posted, QuestionAppended); !errors.Is(err, ErrQuestionMoved) {
		t.Fatalf("moving a received question as though appended = %v, want ErrQuestionMoved", err)
	}
	if err := s.UpdateQuestion(ctx, posted, QuestionReceived); err != nil {
		t.Fatal(err)
	}
	forwarded := AnswerQuestion{ID: "qb", State: QuestionForwarded, UpdatedAt: at(9)}
	if err := s.UpdateQuestion(ctx, forwarded, QuestionReceived); err != nil {
		t.Fatal(err)
	}
	delivered := AnswerQuestion{ID: "qc", State: QuestionForwarded, ForwardDelivered: true, UpdatedAt: at(9)}
	if err := s.UpdateQuestion(ctx, delivered, QuestionReceived); err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenQuestions(ctx, "legion")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, q := range open {
		ids = append(ids, q.ID)
	}
	if len(ids) != 2 || ids[0] != "qb" || ids[1] != "qd" {
		t.Fatalf("open questions = %v, want the held forward qb then the received qd", ids)
	}
}

// The day's spend adds up within a UTC day, starts again on the next, and survives the store being
// read afresh, as a restart reads it.
func TestTheAnswerSpendIsKeptByProjectAndUTCDay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := migratedStore(t)
	morning := time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC)
	evening := time.Date(2026, 10, 26, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600)) // 2026-10-27 00:30 UTC
	for _, add := range []struct {
		at  time.Time
		usd float64
	}{{morning, 0.5}, {morning.Add(time.Hour), 0.25}, {evening, 2}} {
		if err := s.AddAnswerSpend(ctx, "legion", add.at, add.usd); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.AnswerSpend(ctx, "legion", morning); err != nil || got != 0.75 {
		t.Fatalf("spend of 2026-10-26 = %v, %v; want 0.75", got, err)
	}
	if got, err := s.AnswerSpend(ctx, "legion", evening); err != nil || got != 2 {
		t.Fatalf("spend of 2026-10-27 = %v, %v; want 2", got, err)
	}
	if got, err := s.AnswerSpend(ctx, "other", morning); err != nil || got != 0 {
		t.Fatalf("spend of another project = %v, %v; want 0", got, err)
	}
}

// Every gh call of a question reads back in seq order, finished or not.
func TestAQuestionsGHCallsReadBackInOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := migratedStore(t)
	for _, seq := range []int{2, 1} {
		if err := s.StartAnswerToolCall(ctx, AnswerToolCall{Question: "q1", Seq: seq, Tool: ToolGH, Args: []string{"pr", "view"}, App: "review", Owner: "o", StartedAt: at(seq)}); err != nil {
			t.Fatal(err)
		}
	}
	exit := 0
	if err := s.FinishAnswerToolCall(ctx, "q1", 1, "open", &exit, at(3)); err != nil {
		t.Fatal(err)
	}
	calls, err := s.AnswerToolCalls(ctx, "q1")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Seq != 1 || calls[0].Result == nil || *calls[0].Result != "open" || calls[1].Seq != 2 || calls[1].Result != nil {
		t.Fatalf("calls = %+v, want seq 1 finished then seq 2 of unknown outcome", calls)
	}
}

// The read a gh write is taken from (AnswerToolCallArgsTx) holds each call's question, seq, tool,
// arguments, owner and exit, by question and in seq order, and never its result, which a call's
// output fills to gh_output_chars: what `legion state` lists of a question is its writes, not the
// output the model read. A question with no recorded call has none.
func TestTheWritesReadOfTheGHCallsHoldsNoResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := migratedStore(t)
	for _, call := range []AnswerToolCall{
		{Question: "q1", Seq: 2, Tool: ToolGH, Args: []string{"pr", "view", "7"}, App: "review", Owner: "acme", StartedAt: at(2)},
		{Question: "q1", Seq: 1, Tool: ToolGH, Args: []string{"pr", "merge", "7"}, App: "review", Owner: "acme", StartedAt: at(1)},
		{Question: "q2", Seq: 1, Tool: ToolGH, Args: []string{"pr", "comment", "7"}, App: "implement", StartedAt: at(3)},
	} {
		if err := s.StartAnswerToolCall(ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	exit := 0
	if err := s.FinishAnswerToolCall(ctx, "q1", 1, "Merged pull request acme/widgets#7, the whole of gh's output", &exit, at(4)); err != nil {
		t.Fatal(err)
	}
	var calls map[string][]AnswerToolCall
	if err := Transact(ctx, s.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		calls, err = AnswerToolCallArgsTx(ctx, tx, []string{"q1", "q2", "q3"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	zero := 0
	want := map[string][]AnswerToolCall{
		"q1": {
			{Question: "q1", Seq: 1, Tool: ToolGH, Args: []string{"pr", "merge", "7"}, Owner: "acme", Exit: &zero},
			{Question: "q1", Seq: 2, Tool: ToolGH, Args: []string{"pr", "view", "7"}, Owner: "acme"},
		},
		"q2": {{Question: "q2", Seq: 1, Tool: ToolGH, Args: []string{"pr", "comment", "7"}}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("the writes' read of the calls = %+v, want %+v: each call's question, seq, tool, arguments, owner and exit, and no result", calls, want)
	}
}
