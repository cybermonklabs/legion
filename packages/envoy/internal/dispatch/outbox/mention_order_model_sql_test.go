package outbox

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox/copyrule"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The order model (TestTheMentionCopyOrderModel) restates model.MentionCopyHolderSQL as
// moRuleOwedSeq, so this runs both on the same rows, the SQL through copyrule.Holder, the one read
// of the rule both roads of a copy make (mentionCopyHolderFor in internal/dispatch/api, copyHolder
// here; TestOnlyHolderReadsTheCopyRule): T-1's two earlier events, a comment that mentions s1 and
// a later event no record has written, the earlier two set to every pairing of published or not,
// each owed_keys the model has (null, nothing, the topic family, s1's subject, s2's subject, both
// subjects) and each owed_seq (null, one behind the owner's last_seq, at it). The SQL's answer for
// s1's copy must be the model's, event for event. The later event's null owed_keys hold nothing
// in either - only earlier events hold a copy - and no role route is owed, so no role's holder is
// asked. A rule the model restated wrongly - a missing clause, a comparison the wrong way round, a
// later event counted - fails here on the first row the two disagree on.
func TestTheModelsCopyRuleIsTheSQLRule(t *testing.T) {
	t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "session:s1"
	seedIssue(t, database, "T-1", &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	message := func(id, body string) model.Event {
		return appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: alice,
			Payload: model.Message{ID: id, IssueKey: new("T-1"), Body: body},
		})
	}
	first := message("6b1b1c5e-0d3a-4c38-9b7e-0a5f8a0c0201", "first")
	second := message("6b1b1c5e-0d3a-4c38-9b7e-0a5f8a0c0202", "second")
	commentID := seedComment(t, database, "T-1", alice, "@s1 look", nil)
	comment := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: commentID, IssueKey: new("T-1"), Body: "@s1 look",
			Mentions: []model.Mention{{Target: "session:s1", Delivery: "steer"}},
		}},
	})
	later := message("6b1b1c5e-0d3a-4c38-9b7e-0a5f8a0c0203", "later")
	ids := []int64{first.ID, second.ID, comment.ID, later.ID}
	var lastSeq int
	if err := database.Pool.QueryRow(ctx, `select last_seq from issues where key = 'T-1'`).Scan(&lastSeq); err != nil {
		t.Fatalf("read T-1's last_seq: %v", err)
	}
	if lastSeq != len(ids) {
		t.Fatalf("T-1's last_seq is %d, want %d: the model's last_seq is its event count", lastSeq, len(ids))
	}
	s1, s2 := contracts.LegacyAgentSubject("s1"), contracts.LegacyAgentSubject("s2")
	type owedCase struct {
		null bool
		keys []string
		bits uint8
	}
	owedCases := []owedCase{
		{null: true},
		{keys: []string{}},
		{keys: []string{model.OwedTopicFamily}, bits: moTopic},
		{keys: []string{s1}, bits: moS1},
		{keys: []string{s2}, bits: moS2},
		{keys: []string{s1, s2}, bits: moS1 | moS2},
	}
	seqCases := []*int{nil, new(lastSeq - 1), new(lastSeq)}
	type config struct {
		published bool
		owed      owedCase
		seq       *int
	}
	var configs []config
	for _, published := range []bool{false, true} {
		for _, owed := range owedCases {
			for _, seq := range seqCases {
				configs = append(configs, config{published: published, owed: owed, seq: seq})
			}
		}
	}
	if _, err := database.Pool.Exec(ctx, `update events set owed_keys = null, owed_seq = null where id = $1`, later.ID); err != nil {
		t.Fatalf("leave the later event unrecorded: %v", err)
	}
	rows, holds := 0, 0
	for _, a := range configs {
		for _, b := range configs {
			state := moState{n: int8(len(ids)), c: 2}
			state.ev[3] = moEvent{owedNull: true, owedSeq: -1}
			for k, cfg := range []config{a, b} {
				var keys []string
				if !cfg.owed.null {
					keys = cfg.owed.keys
				}
				if _, err := database.Pool.Exec(ctx, `
					update events set published_at = case when $2 then now() end, owed_keys = $3::text[], owed_seq = $4
					where id = $1
				`, ids[k], cfg.published, keys, cfg.seq); err != nil {
					t.Fatalf("set event %d: %v", ids[k], err)
				}
				e := moEvent{published: cfg.published, owedNull: cfg.owed.null, owed: cfg.owed.bits, owedSeq: -1}
				if cfg.seq != nil {
					e.owedSeq = int8(*cfg.seq)
				}
				state.ev[k] = e
			}
			by, err := copyrule.Holder(ctx, database.Pool, comment.ID, s1, func(_ context.Context, role string) bool {
				t.Errorf("the rule asked who holds role %q, which no event of these rows owes", role)
				return true
			})
			if err != nil {
				t.Fatalf("read the rule: %v", err)
			}
			want := moRuleOwedSeq(&state)
			got := -1
			if by != nil {
				if got = slices.Index(ids, by.EventID); got < 0 {
					t.Fatalf("the SQL holds s1's copy behind event %d, which is none of T-1's", by.EventID)
				}
			}
			rows++
			if got >= 0 {
				holds++
			}
			if got != want {
				t.Fatalf("earlier events %s and %s: the SQL holds s1's copy behind %s, the model behind %s",
					describeConfig(a.published, a.owed.null, a.owed.keys, a.seq), describeConfig(b.published, b.owed.null, b.owed.keys, b.seq),
					describeIndex(got), describeIndex(want))
			}
		}
	}
	t.Logf("%d pairings of the two earlier events, the SQL and the model agree on each: %d hold s1's copy, %d let it go", rows, holds, rows-holds)
	t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
}

func describeIndex(k int) string {
	if k < 0 {
		return "nothing"
	}
	return fmt.Sprintf("event %d", k+1)
}

func describeConfig(published, null bool, keys []string, seq *int) string {
	owed := "null"
	if !null {
		owed = fmt.Sprint(keys)
	}
	owedSeq := "null"
	if seq != nil {
		owedSeq = fmt.Sprint(*seq)
	}
	return fmt.Sprintf("(published %v, owed_keys %s, owed_seq %s)", published, owed, owedSeq)
}
