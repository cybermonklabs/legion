package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// A refresh whose statement has committed but whose result is not yet recorded loses to a keyed
// repeat that claims the same queued attempt anew in that gap: the sender must end up holding the
// repeat's claim, the one the row carries, so its next refresh writes the row again and the
// attempt does not lapse into the delivery sweep's hands.
func TestARefreshThatLosesToAKeyedRepeatKeepsTheRepeatsClaim(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		envoyURL: listener.URL, envoyTimeout: 30 * time.Second,
	})
	withMentionClaimRefresh(t, served, time.Hour)
	issue := createInteractionIssue(t, handler, "TEST", "A refresh racing a repeat", "before")
	reached, release := state.stall("s1")
	defer release()

	postMentionOfS1(t, handler, issue.Key, "first")
	<-reached
	keyed := map[string]any{"body": "second", "mentions": []map[string]any{{"target": "session:s1"}}}
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	queued := decodeMentionedComment(t,
		keyedRequest(t, handler, http.MethodPost, comments, keyed, "alice", "refresh-race")).ID
	if !claimedPending(t, database, queued, "session:s1") {
		t.Fatal("the attempt waiting on the held lane is not held by the sender")
	}

	// The refresh's statement commits, then the keyed repeat claims the attempt anew, then the
	// refresh records what its statement returned.
	sender := served.mentions
	realRefresh := sender.refresh
	sender.refresh = func(ctx context.Context, queued []mentionClaim) ([]mentionClaim, error) {
		refreshed, err := realRefresh(ctx, queued)
		time.Sleep(20 * time.Millisecond)
		// What queueLocked records for a keyed repeat that claimed the attempt anew: a newer
		// claim on the row and the same claim in the sender's map.
		var newer time.Time
		if err := database.Pool.QueryRow(context.Background(), `
			update comment_deliveries set claimed_at = now()
			where comment_id = $1 and target = 'session:s1' and state = 'pending' returning claimed_at
		`, queued[0].key.comment).Scan(&newer); err != nil {
			t.Errorf("write the newer claim: %v", err)
		}
		sender.mu.Lock()
		sender.claims[queued[0].key] = newer
		sender.mu.Unlock()
		return refreshed, err
	}
	sender.refreshHeldClaims(context.Background())
	sender.refresh = realRefresh

	row := attemptClaimedAt(t, database, queued, "session:s1")
	sender.mu.Lock()
	var held time.Time
	for key, claimed := range sender.claims {
		if key.comment == queued {
			held = claimed
		}
	}
	sender.mu.Unlock()
	if !held.Equal(row) {
		t.Fatalf("the sender holds claim %v for the queued attempt, the row carries %v: the refresh recorded its "+
			"own result over the repeat's claim", held, row)
	}
	sender.refreshHeldClaims(context.Background())
	if again := attemptClaimedAt(t, database, queued, "session:s1"); !again.After(row) {
		t.Fatalf("the claim on the queued attempt is still %v after the next refresh, want it written again: "+
			"left alone it lapses a lease later and the sweep may send it out of order", again)
	}
	release()
	if sent := settledCommentDeliveries(t, database, queued); len(sent) != 1 || !allSent(sent) {
		t.Fatalf("the repeated comment's attempts = %#v, want its one attempt sent", sent)
	}
}

// A claim refresh that lands while a parked send's try is in flight - a try that reads the rule
// alone, which writes no claim - must leave the sender recording the claim the refresh wrote on
// the row. The refresh writes a row only where it carries the claim the sender records
// (refreshMentionClaims), so a record left behind its row is never written again: the row's claim
// lapses with the send still parked, the lane's own send is then refused as another sender's,
// and the issue's next mention reaches the session first (TestTheMentionSenderClaimModel finds
// all three). The send, the refresh and the rule are the server's own, on the database; only
// their order is the test's. The control row runs the refresh while the send is parked.
func TestARefreshDuringAParkedSendsTryLeavesTheSenderHoldingTheRowsClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		across bool
	}{
		{name: "control: the refresh runs while the send is parked"},
		{name: "the refresh runs while the parked send's try is in flight", across: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, _ := newMentionListener(t)
			handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
				envoyURL: listener.URL, envoyTimeout: 30 * time.Second, outboxPublishes: true,
			})
			served.mentions.wait()
			var gated atomic.Bool
			refreshEntered, refreshGo := make(chan struct{}), make(chan struct{})
			tryEntered, tryGo := make(chan struct{}), make(chan struct{})
			sender := newMentionSender(served.lifetime, time.Hour,
				func(ctx context.Context, item mentionSend) (mentionSend, bool) {
					if item.heldFor != "" && gated.Load() {
						tryEntered <- struct{}{}
						<-tryGo
					}
					return served.sendCommentMention(ctx, item)
				},
				func(ctx context.Context, queued []mentionClaim) ([]mentionClaim, error) {
					if gated.Load() {
						refreshEntered <- struct{}{}
						<-refreshGo
					}
					return served.refreshMentionClaims(ctx, queued)
				},
				served.releaseMentionClaims)
			timers := make(chan func(), 4)
			sender.mu.Lock()
			sender.after = func(_ time.Duration, f func()) { timers <- f }
			sender.mu.Unlock()
			served.mentions = sender

			// No outbox runs, so the issue's earlier events stay unsent and the comment's send
			// waits behind them: its first try claims and parks, and each later try reads the
			// rule alone.
			issue := createInteractionIssue(t, handler, "TEST", "A refresh across a parked send's try", "before")
			comment := postMentionOfS1(t, handler, issue.Key, "the parked one")
			unpark := <-timers
			if tc.across {
				gated.Store(true)
				refreshed := make(chan struct{})
				go func() {
					sender.refreshHeldClaims(context.Background())
					close(refreshed)
				}()
				<-refreshEntered // the refresh holds its snapshot, the parked send in it
				go unpark()
				<-tryEntered // the lane has started the send's try
				close(refreshGo)
				<-refreshed // the refresh wrote the row and recorded what it wrote
				gated.Store(false)
				close(tryGo) // the try reads the rule alone and parks again
			} else {
				sender.refreshHeldClaims(context.Background())
				go unpark()
			}
			<-timers

			key := mentionKey{comment: comment, target: "session:s1", attempt: 1}
			sender.mu.Lock()
			recorded, holds := sender.claims[key]
			sender.mu.Unlock()
			row := attemptClaimedAt(t, database, comment, "session:s1")
			t.Logf("parked again: the sender records claim %v (held %t), the row carries %v", recorded, holds, row)
			if !holds || !recorded.Equal(row) {
				t.Fatalf("the sender records the parked send's claim as %v (held %t) while its row carries %v: the refresh writes only a row that carries the claim the sender records, so this one is never written again and lapses with the send still parked",
					recorded, holds, row)
			}
			sender.refreshHeldClaims(context.Background())
			if again := attemptClaimedAt(t, database, comment, "session:s1"); !again.After(row) {
				t.Fatalf("the parked send's claim is still %v after the next refresh, want it written again", again)
			}
		})
	}
}
