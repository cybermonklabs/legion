package outbox

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox/copyrule"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The order model (TestTheMentionCopyOrderModel) restates model.MentionCopyHolderSQL as
// moRuleOwedSeq, so this runs both on the same rows, the SQL through copyrule.Holder, the one read
// of the rule both roads of a copy make (mentionCopyHolderFor in internal/dispatch/api, copyHolder
// here; TestOnlyHolderReadsTheCopyRule). Each first-event configuration gets its own issue (T-1
// through T-36) with two earlier events, a comment that mentions s1, and a later unrecorded event.
// The earlier events cover every pairing of published or not, every owed_keys shape (null,
// empty, topic family, s1, s2, or both subjects), and each owed_seq (null, one behind last_seq,
// or at it). The SQL's answer for s1's copy must match the model's, event for event. No role
// route is owed, and the later event holds nothing. A missing clause, a reversed comparison,
// or a later event counted fails on the first disagreement.
func TestTheModelsCopyRuleIsTheSQLRule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	const lastSeq = 4
	route := "session:s1"
	alice := model.Actor{Kind: "user", ID: "alice"}
	seed := func(t *testing.T, key string) ([]int64, model.Event) {
		t.Helper()
		message := func(id, body string) model.Event {
			return appendEvent(t, database, broker, model.Event{
				IssueKey: new(key), Type: "message.created", Actor: alice,
				Payload: model.Message{ID: id, IssueKey: new(key), Body: body},
			})
		}
		first := message("6b1b1c5e-0d3a-4c38-9b7e-0a5f8a0c0201", "first")
		second := message("6b1b1c5e-0d3a-4c38-9b7e-0a5f8a0c0202", "second")
		commentID := seedComment(t, database, key, alice, "@s1 look", nil)
		comment := appendEvent(t, database, broker, model.Event{
			IssueKey: new(key), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: model.Comment{
				ID: commentID, IssueKey: new(key), Body: "@s1 look",
				Mentions: []model.Mention{{Target: "session:s1", Delivery: "steer"}},
			}},
		})
		later := message("6b1b1c5e-0d3a-4c38-9b7e-0a5f8a0c0203", "later")
		ids := []int64{first.ID, second.ID, comment.ID, later.ID}
		var sequence int
		if err := database.Pool.QueryRow(ctx, `select last_seq from issues where key = $1`, key).Scan(&sequence); err != nil {
			t.Fatalf("read %s's last_seq: %v", key, err)
		}
		if sequence != lastSeq || len(ids) != lastSeq {
			t.Fatalf("%s's last_seq is %d and the fixture has %d events, want both to be %d", key, sequence, len(ids), lastSeq)
		}
		if _, err := database.Pool.Exec(ctx, `update events set owed_keys = null, owed_seq = null where id = $1`, later.ID); err != nil {
			t.Fatalf("leave the later event unrecorded: %v", err)
		}
		return ids, comment
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
	seedIssue(t, database, "T-1", &route)
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		select 'T-' || n, 'TT', n, 'Model configuration', $1, '{"kind":"user","id":"alice"}', 'V'
		from generate_series(2, $2::int) n
	`, route, len(configs)); err != nil {
		t.Fatalf("seed one issue per first-event configuration: %v", err)
	}
	write := func(t *testing.T, id int64, cfg config) moEvent {
		t.Helper()
		var keys []string
		if !cfg.owed.null {
			keys = cfg.owed.keys
		}
		if _, err := database.Pool.Exec(ctx, `
			update events set published_at = case when $2 then now() end, owed_keys = $3::text[], owed_seq = $4
			where id = $1
		`, id, cfg.published, keys, cfg.seq); err != nil {
			t.Fatalf("set event %d: %v", id, err)
		}
		e := moEvent{published: cfg.published, owedNull: cfg.owed.null, owed: cfg.owed.bits, owedSeq: -1}
		if cfg.seq != nil {
			e.owedSeq = int8(*cfg.seq)
		}
		return e
	}
	for i, a := range configs {
		t.Run(fmt.Sprintf("first_%02d", i), func(t *testing.T) {
			t.Parallel()
			key := fmt.Sprintf("T-%d", i+1)
			ids, comment := seed(t, key)
			state := moState{n: int8(len(ids)), c: 2}
			state.ev[3] = moEvent{owedNull: true, owedSeq: -1}
			state.ev[0] = write(t, ids[0], a)
			rows, holds := 0, 0
			for _, b := range configs {
				state.ev[1] = write(t, ids[1], b)
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
						t.Fatalf("the SQL holds s1's copy behind event %d, which is none of %s's", by.EventID, key)
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
			t.Logf("%d pairings: %d hold s1's copy, %d let it go", rows, holds, rows-holds)
		})
	}
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
