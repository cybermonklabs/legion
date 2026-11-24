package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// queuedNoticesOf is every notice of issue the outbox holds, decoded, in the order written.
func queuedNoticesOf(t *testing.T, pool *store.Pool, issue string) []record.Notice {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select payload from outbox where kind = $1 and issue = $2 order by id",
		string(record.OutboxKindNotice), issue)
	if err != nil {
		t.Fatalf("read %s's queued notices: %v", issue, err)
	}
	defer rows.Close()
	var notices []record.Notice
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("read a queued notice: %v", err)
		}
		var notice record.Notice
		if err := json.Unmarshal(payload, &notice); err != nil {
			t.Fatalf("decode the queued notice %s: %v", payload, err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s's queued notices: %v", issue, err)
	}
	return notices
}

// A merge-forward message is never lost to its claim's end while the workflow still delivers it
// and nobody hears of it. An ask to the issue's owner whose claim has failed waits for the owner's
// next start, since the owner's own steps wait on it: the row stays, nothing is published, and no
// line says it finished. An ask to a fresh implementer whose claim has retired, and a review to a
// fresh reviewer whose claim has failed, finish undelivered, and the architect that owns the issue
// is told by one notice of the kind merge-forward-lost, whose role is the role the message did not
// reach and whose phase is the phase the issue stays in.
func TestAMergeForwardMessageIsNotLostToItsClaimsEnd(t *testing.T) {
	t.Parallel()
	asked := &record.MergeForward{Head: "sha-1", Base: "main", Phase: phase.Testing, Why: record.ConflictCheckNotGreen, AskedAt: time.Now()}
	landed := *asked
	landed.Landed, landed.ReviewAsked = "sha-2", true
	ask := record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: "sha-1", Base: "main",
		Phase: phase.Testing, PullRequest: 86, Why: record.ConflictCheckNotGreen}
	review := record.MergeForwardMessage{Role: claim.RoleReviewer, Step: record.MergeForwardReview, Head: "sha-1", Landed: "sha-2", Base: "main",
		Phase: phase.Testing, PullRequest: 86}
	for _, tc := range []struct {
		name    string
		holder  claim.Role
		state   supervise.ClaimState
		asked   *record.MergeForward
		message record.MergeForwardMessage
		waits   bool
	}{
		{name: "the owner's ask, its claim failed", holder: claim.RoleArchitect, state: supervise.StateFailed, asked: asked, message: ask, waits: true},
		{name: "a fresh implementer's ask, its claim retired", holder: claim.RoleImplementer, state: supervise.StateRetired, asked: asked, message: ask},
		{name: "a fresh reviewer's review, its claim failed", holder: claim.RoleReviewer, state: supervise.StateFailed, asked: &landed, message: review},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			publisher := &holderPublisher{}
			runner, sup, pool, logs := mergeForwardRunner(t, publisher, tc.asked)
			token := mustClaimToken(t, "LEGION-208", tc.holder)
			if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
				Role: tc.holder, State: tc.state}, ""); err != nil {
				t.Fatalf("create the %s claim of %s: %v", tc.state, tc.holder, err)
			}
			message := tc.message
			message.Claim = token
			enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", message, time.Now().Add(-time.Minute)))

			drainAt(t, runner, 0, "the row's attempt")
			_, delivered := publisher.snapshot()
			for _, d := range delivered {
				if d.topic == notify.RoleTopicPrefix+string(token) {
					t.Fatalf("the %s was published to %s's claim %s, want nothing published to it", tc.message.Step, tc.state, token)
				}
			}
			ended := loggedLines(t, logs.String(), mergeForwardEndedLine)
			notices := queuedNoticesOf(t, pool, "LEGION-208")
			if tc.waits {
				if left := outboxRows(t, pool); left != 1 || len(ended) != 0 || len(notices) != 0 {
					t.Fatalf("the owner's ask left %d rows, logged %v and queued the notices %+v; want its row kept for the owner's next start, no line of its end and no notice",
						left, ended, notices)
				}
				return
			}
			if len(ended) != 1 || ended[0]["state"] != string(tc.state) {
				t.Fatalf("the row logged %v, want one line of its end naming the %s claim", ended, tc.state)
			}
			if len(notices) != 1 || notices[0].Kind != record.MergeForwardLostNotice || notices[0].Role != tc.message.Role || notices[0].Phase != tc.message.Phase {
				t.Fatalf("the row queued the notices %+v, want one %s to the architect naming the %s it did not reach, in %s",
					notices, record.MergeForwardLostNotice, tc.message.Role, tc.message.Phase)
			}
		})
	}
}
