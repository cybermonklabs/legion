package api

import (
	"context"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
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
		state := sender.held[queued[0].key]
		state.claimed = newer
		sender.held[queued[0].key] = state
		sender.mu.Unlock()
		return refreshed, err
	}
	sender.refreshHeldClaims(context.Background())
	sender.refresh = realRefresh

	row := attemptClaimedAt(t, database, queued, "session:s1")
	sender.mu.Lock()
	var held time.Time
	for key, state := range sender.held {
		if key.comment == queued {
			held = state.claimed
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
				served.rereadMentionClaims, served.releaseMentionClaims)
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
			state, holds := sender.held[key]
			sender.mu.Unlock()
			recorded := state.claimed
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

// recordedClaim is the claim sender records for key's attempt, and whether it records it unsure.
func recordedClaim(sender *mentionSender, key mentionKey) (time.Time, bool) {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	state := sender.held[key]
	return state.claimed, state.unsure
}

// ageClaimAndRecord moves the claim key's row carries and the claim sender records for it back by
// by together, as that much time passing with no statement on the row leaves them: a record that
// agreed with its row still agrees, and one behind its row is still behind it.
func ageClaimAndRecord(t *testing.T, database *store.Store, sender *mentionSender, key mentionKey, by time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set claimed_at = claimed_at - make_interval(secs => $4)
		where comment_id = $1 and target = $2 and attempt = $3
	`, key.comment, key.target, key.attempt, by.Seconds()); err != nil {
		t.Fatalf("age the claim on the attempt of %s: %v", key.comment, err)
	}
	sender.mu.Lock()
	state := sender.held[key]
	state.claimed = state.claimed.Add(-by)
	sender.held[key] = state
	sender.mu.Unlock()
}

// publishIssueEvents marks every event of the issue published, as the outbox does once it has
// sent them: no earlier event of the issue holds a mention's send any longer.
func publishIssueEvents(t *testing.T, database *store.Store, issueKey string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update events set published_at = coalesce(published_at, now()) where issue_key = $1`, issueKey); err != nil {
		t.Fatalf("publish the events of %s: %v", issueKey, err)
	}
}

// A refresh that lands while a send's first try is in flight - after the try took the attempt's
// claim for itself and before its park records that claim - and one after the park leave the
// sender recording the claim the row carries: its refreshes go on writing the row through a
// lease, the delivery sweep finds the attempt held, and s1 takes the parked comment before the
// one posted after it. The send, the refreshes and the rule are the server's own, on the
// database. The control row runs the refresh after the park alone.
//
// The window between the try's answer and its park has no failing run, from this test or from
// the claim model. Before this change serve marked the try done (trying(false)) one step before
// park recorded the try's claim, and a refresh that re-read the claim in that step could record the
// try's claim and write the row past it, which park then set back. That step holds no lock and
// calls none of the sender's send, refresh, reread or timer functions, so no test of the real lane
// can land a refresh in it, and the claim model, whose steps stop only inside those functions, has
// no step there either. The fix is structural: serve no longer marks the try done, and park clears
// the mark under the lock it records the claim under (mentionSender.park), so the window is gone.
// This test passes before the change too, and guards the behaviour around that window.
func TestAParkedSendKeepsTheRefreshsClaimWhenARefreshLandsBetweenItsTryAndItsPark(t *testing.T) {
	for _, tc := range []struct {
		name   string
		during bool
	}{
		{name: "control: the refresh runs after the park alone"},
		{name: "a refresh whose answer is lost and another land during the try, then one after the park", during: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			listener, state := newMentionListener(t)
			handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
				envoyURL: listener.URL, envoyTimeout: 30 * time.Second, outboxPublishes: true,
			})
			served.mentions.wait()
			var lose, refreshed atomic.Bool
			// unsureDuring is whether the lost refresh left the claim unsure while the try was in
			// flight; park's timer, which the test receives, orders its write before the read.
			var unsureDuring bool
			var sender *mentionSender
			sender = newMentionSender(served.lifetime, time.Hour,
				func(ctx context.Context, item mentionSend) (mentionSend, bool) {
					again, waits := served.sendCommentMention(ctx, item)
					if tc.during && item.heldFor == "" && refreshed.CompareAndSwap(false, true) {
						// The try took the attempt's claim and found the earlier events owed;
						// its answer has not reached the lane yet.
						lose.Store(true)
						sender.refreshHeldClaims(ctx)
						_, unsureDuring = recordedClaim(sender, item.key())
						sender.refreshHeldClaims(ctx)
					}
					return again, waits
				},
				func(ctx context.Context, queued []mentionClaim) ([]mentionClaim, error) {
					written, err := served.refreshMentionClaims(ctx, queued)
					if lose.Swap(false) {
						return nil, errLostAfterCommit
					}
					return written, err
				},
				served.rereadMentionClaims, served.releaseMentionClaims)
			timers := make(chan func(), 4)
			sender.mu.Lock()
			sender.after = func(_ time.Duration, f func()) { timers <- f }
			sender.mu.Unlock()
			served.mentions = sender

			// No outbox runs, so the issue's earlier events stay unsent and A-1's first try
			// claims its attempt and parks, its timer left to the test.
			issue := createInteractionIssue(t, handler, "TEST", "A refresh beside a try and its park", "before")
			a1 := postMentionOfS1(t, handler, issue.Key, "first")
			unpark := <-timers
			key := mentionKey{comment: a1, target: "session:s1", attempt: 1}
			if tc.during && !unsureDuring {
				t.Fatal("the refresh whose answer was lost during the try left A-1's claim known: the case is not the one under test")
			}
			sender.refreshHeldClaims(ctx)
			recorded, unsure := recordedClaim(sender, key)
			row := attemptClaimedAt(t, database, a1, "session:s1")
			t.Logf("parked; after the refresh the sender records %v (unsure %t), the row carries %v", recorded, unsure, row)
			if unsure || !recorded.Equal(row) {
				t.Fatalf("the sender trusts claim %v (unsure %t) for A-1 while its row carries %v: the refresh writes only a row that carries the claim the sender records, so this one lapses with the send still parked",
					recorded, unsure, row)
			}

			// A lease passes, the sender's refresh running every third of it as keepClaims runs
			// it, and the attempt is old enough to be owed: the sweep must find it held.
			ageAttempts(t, database, a1)
			for _, step := range []time.Duration{20 * time.Second, 20 * time.Second, 21 * time.Second} {
				ageClaimAndRecord(t, database, sender, key, step)
				sender.refreshHeldClaims(ctx)
			}
			owed, err := served.owedDeliveries(ctx)
			if err != nil {
				t.Fatalf("read the owed deliveries: %v", err)
			}
			for _, delivery := range owed {
				if delivery.commentID == a1 {
					t.Fatalf("a lease on, the delivery sweep finds A-1 owed while the sender holds it parked: its claim lapsed (row %v)",
						attemptClaimedAt(t, database, a1, "session:s1"))
				}
			}

			a2 := postMentionOfS1(t, handler, issue.Key, "second")
			publishIssueEvents(t, database, issue.Key)
			unpark()
			settledCommentDeliveries(t, database, a2)
			states := attemptStates(t, database, "comment_deliveries", "comment_id", a1)
			bodies := state.bodiesTo("s1")
			t.Logf("A-1's attempts %v; s1 took %q", states, bodies)
			if !slices.Equal(states, []string{"1:sent"}) || !slices.Equal(bodies, []string{"first", "second"}) {
				t.Fatalf("A-1's attempts are %v and s1 took %q, want A-1 sent and s1 to take [first second]", states, bodies)
			}
		})
	}
}
