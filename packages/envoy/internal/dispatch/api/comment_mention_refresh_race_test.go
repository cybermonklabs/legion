package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A refresh whose statement has committed but whose result is not yet recorded loses to a keyed
// repeat that claims the same queued attempt anew in that gap: the sender must end up holding the
// repeat's claim, the one the row carries, so its next refresh writes the row again and the
// attempt does not lapse into the delivery sweep's hands.
func TestARefreshThatLosesToAKeyedRepeatKeepsTheRepeatsClaim(t *testing.T) {
	t.Parallel()
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

	row := attemptClaimedAt(t, database.Pool, queued, "session:s1")
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
	if again := attemptClaimedAt(t, database.Pool, queued, "session:s1"); !again.After(row) {
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
	t.Parallel()
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
			row := attemptClaimedAt(t, database.Pool, comment, "session:s1")
			t.Logf("parked again: the sender records claim %v (held %t), the row carries %v", recorded, holds, row)
			if !holds || !recorded.Equal(row) {
				t.Fatalf("the sender records the parked send's claim as %v (held %t) while its row carries %v: the refresh writes only a row that carries the claim the sender records, so this one is never written again and lapses with the send still parked",
					recorded, holds, row)
			}
			sender.refreshHeldClaims(context.Background())
			if again := attemptClaimedAt(t, database.Pool, comment, "session:s1"); !again.After(row) {
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

// parkGate is the sender's lock with a gate before park's: once armed, the first Lock park calls
// tells the test it has arrived and waits, holding no lock, until the test releases the gate.
// Every other Lock takes the mutex alone. The gate is released when the test ends at the latest,
// so no lane goroutine is left waiting at it.
type parkGate struct {
	sync.Mutex
	armed    atomic.Bool
	arrived  chan struct{}
	open     chan struct{}
	releases sync.Once
}

func newParkGate(t *testing.T) *parkGate {
	gate := &parkGate{arrived: make(chan struct{}), open: make(chan struct{})}
	t.Cleanup(gate.release)
	return gate
}

//go:noinline
func (g *parkGate) Lock() {
	if g.armed.Load() && lockCaller() == parkLockSite && g.armed.CompareAndSwap(true, false) {
		close(g.arrived)
		<-g.open
	}
	g.Mutex.Lock()
}

func (g *parkGate) release() {
	g.releases.Do(func() { close(g.open) })
}

// lockCaller is the name of the function that called the Lock that calls lockCaller, which the
// list of the sender's lock sites names (mentionSenderLockSites, parkLockSite).
//
//go:noinline
func lockCaller() string {
	var pc [1]uintptr
	if runtime.Callers(3, pc[:]) == 0 {
		return ""
	}
	frame, _ := runtime.CallersFrames(pc[:]).Next()
	return frame.Function[strings.LastIndex(frame.Function, ".")+1:]
}

// A refresh that lands while a send's first try is in flight - after the try took the attempt's
// claim for itself and before its park records that claim - and one after the park leave the
// sender recording the claim the row carries: its refreshes go on writing the row through a
// lease, the delivery sweep finds the attempt held, and s1 takes the parked comment before the
// one posted after it. The send, the refreshes and the rule are the server's own, on the
// database. The control row runs the refresh after the park alone.
//
// The last row lands the refresh in the window's last step: the try has answered and serve has
// gone on to park, which waits at the sender's lock (parkGate) while the refresh runs, a refresh
// whose answer was lost during the try having left the claim unknown. A serve that marked the try
// done (trying(false)) before park took that lock to record the try's claim let this refresh
// re-read the row, record the try's claim and write the row past it, and park then set the record
// back to the try's claim: the sender trusted a claim the row no longer carried, and no refresh
// wrote that row again. The try stays marked in flight until park records its claim under that
// lock (mentionSender.park), so the refresh re-reads nothing there.
func TestAParkedSendKeepsTheRefreshsClaimWhenARefreshLandsBetweenItsTryAndItsPark(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		during  bool
		between bool
	}{
		{name: "control: the refresh runs after the park alone"},
		{name: "a refresh whose answer is lost and another land during the try, then one after the park", during: true},
		{name: "a refresh whose answer is lost lands during the try, another as park waits for the lock, then one after the park", between: true},
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
			gate := newParkGate(t)
			var sender *mentionSender
			sender = newMentionSender(served.lifetime, time.Hour,
				func(ctx context.Context, item mentionSend) (mentionSend, bool) {
					again, waits := served.sendCommentMention(ctx, item)
					if (tc.during || tc.between) && item.heldFor == "" && refreshed.CompareAndSwap(false, true) {
						// The try took the attempt's claim and found the earlier events owed;
						// its answer has not reached the lane yet.
						lose.Store(true)
						sender.refreshHeldClaims(ctx)
						_, unsureDuring = recordedClaim(sender, item.key())
						if tc.during {
							sender.refreshHeldClaims(ctx)
						}
						gate.armed.Store(tc.between)
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
			// No goroutine of the sender's takes its lock before a send is queued: keepClaims's
			// first refresh is an hour away.
			sender.mu = gate
			timers := make(chan func(), 4)
			sender.mu.Lock()
			sender.after = func(_ time.Duration, f func()) { timers <- f }
			sender.mu.Unlock()
			served.mentions = sender

			// No outbox runs, so the issue's earlier events stay unsent and A-1's first try
			// claims its attempt and parks, its timer left to the test.
			issue := createInteractionIssue(t, handler, "TEST", "A refresh beside a try and its park", "before")
			a1 := postMentionOfS1(t, handler, issue.Key, "first")
			key := mentionKey{comment: a1, target: "session:s1", attempt: 1}
			if tc.between {
				testwait.Receive(t, "A-1's park reaching the sender's lock after its try", gate.arrived)
				sender.refreshHeldClaims(ctx)
				recorded, unsure := recordedClaim(sender, key)
				t.Logf("as park waits for the lock, the refresh leaves the sender recording %v (unsure %t), the row carrying %v",
					recorded, unsure, attemptClaimedAt(t, database.Pool, a1, "session:s1"))
				gate.release()
			}
			unpark := <-timers
			if (tc.during || tc.between) && !unsureDuring {
				t.Fatal("the refresh whose answer was lost during the try left A-1's claim known: the case is not the one under test")
			}
			sender.refreshHeldClaims(ctx)
			recorded, unsure := recordedClaim(sender, key)
			row := attemptClaimedAt(t, database.Pool, a1, "session:s1")
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
						attemptClaimedAt(t, database.Pool, a1, "session:s1"))
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

// A delivery sweep that takes a parked send's attempt while that send's try is in flight - the
// claim the send parked under lapsed on the row - hands the send back to the sender under the
// claim the sweep took (resumeOwedDelivery, queueLocked). The try, carrying the claim from before,
// is refused as another sender's; the sender must try the send again under the hand-off's claim
// and send it, rather than let it go with its attempt pending under a claim no sender holds, which
// nothing sends until that claim lapses a lease later. The send, the sweep and the rule are the
// server's own, on the database; only their order is the test's: the try is held inside the
// sender's send until the sweep has handed the send over and the earlier events are sent. The
// control row has no hand-off: the try, under the sender's own claim, waits behind the earlier
// events and parks again.
func TestAHandOffDuringASendsTryIsTriedAgainUnderItsClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		handOff bool
	}{
		{name: "control: no hand-off, the try waits and parks again"},
		{name: "the delivery sweep hands the send over while its try is in flight", handOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			listener, state := newMentionListener(t)
			handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
				envoyURL: listener.URL, envoyTimeout: 30 * time.Second, outboxPublishes: true,
			})
			served.mentions.wait()
			var gated atomic.Bool
			tryEntered, tryGo := make(chan struct{}), make(chan struct{})
			// tried is the claim each try of the parked send carried, in order.
			var mu sync.Mutex
			var tried []time.Time
			sender := newMentionSender(served.lifetime, time.Hour,
				func(ctx context.Context, item mentionSend) (mentionSend, bool) {
					if item.heldFor != "" {
						mu.Lock()
						tried = append(tried, item.claimed)
						mu.Unlock()
						if gated.CompareAndSwap(true, false) {
							tryEntered <- struct{}{}
							<-tryGo
						}
					}
					return served.sendCommentMention(ctx, item)
				},
				served.refreshMentionClaims, served.rereadMentionClaims, served.releaseMentionClaims)
			timers := make(chan func(), 4)
			sender.mu.Lock()
			sender.after = func(_ time.Duration, f func()) { timers <- f }
			sender.mu.Unlock()
			served.mentions = sender

			// No outbox runs, so the issue's earlier events stay unsent and the comment's send
			// parks under the claim its first try took, its timer left to the test.
			issue := createInteractionIssue(t, handler, "TEST", "A hand-off during a try", "before")
			comment := postMentionOfS1(t, handler, issue.Key, "the parked one")
			unpark := <-timers
			key := mentionKey{comment: comment, target: "session:s1", attempt: 1}
			// A lease and a second with no refresh: the claim the send parked under lapsed, on
			// its row and in the sender's record alike, and the attempt is old enough to be owed.
			ageAttempts(t, database, comment)
			ageClaimAndRecord(t, database, sender, key, model.DeliveryClaimLapse+time.Second)
			parked, _ := recordedClaim(sender, key)

			gated.Store(true)
			go unpark()
			<-tryEntered // the lane has started the send's try, carrying the lapsed claim
			want := []time.Time{parked}
			if tc.handOff {
				owed, err := served.owedDeliveries(ctx)
				if err != nil {
					t.Fatalf("read the owed deliveries: %v", err)
				}
				index := slices.IndexFunc(owed, func(delivery owedDelivery) bool { return delivery.commentID == comment })
				if index < 0 {
					t.Fatalf("the delivery sweep does not find the parked send's attempt owed under its lapsed claim: %+v", owed)
				}
				// The sweep claims the attempt, finds the earlier events still owed and hands
				// the send to the sender under the claim it took.
				if err := served.resumeOwedDelivery(ctx, owed[index]); err != nil {
					t.Fatalf("the sweep's send of the parked one: %v", err)
				}
				handedOver := attemptClaimedAt(t, database.Pool, comment, "session:s1")
				recorded, _ := recordedClaim(sender, key)
				t.Logf("parked under %v; the sweep handed the send over under %v, and the sender records %v",
					parked, handedOver, recorded)
				if !handedOver.After(parked) || !recorded.Equal(handedOver) {
					t.Fatalf("the sweep's hand-off left the row's claim at %v and the sender's record at %v, want both at a claim past %v",
						handedOver, recorded, parked)
				}
				want = append(want, handedOver)
				publishIssueEvents(t, database, issue.Key)
			}
			close(tryGo)
			if !tc.handOff {
				unpark = <-timers // the try read the rule alone, waited and parked again
				recorded, unsure := recordedClaim(sender, key)
				row := attemptClaimedAt(t, database.Pool, comment, "session:s1")
				t.Logf("parked again: the sender records %v (unsure %t), the row carries %v", recorded, unsure, row)
				if unsure || !recorded.Equal(row) || !row.Equal(parked) {
					t.Fatalf("parked again, the sender records %v (unsure %t) and the row carries %v, want both the claim it parked under, %v",
						recorded, unsure, row, parked)
				}
				publishIssueEvents(t, database, issue.Key)
				want = append(want, parked)
				go unpark()
			}
			waitUntil(t, "the sender done with the parked send", func() bool {
				sender.mu.Lock()
				defer sender.mu.Unlock()
				_, holds := sender.held[key]
				return !holds
			})

			states := attemptStates(t, database, "comment_deliveries", "comment_id", comment)
			bodies := state.bodiesTo("s1")
			mu.Lock()
			carried := slices.Clone(tried)
			mu.Unlock()
			t.Logf("the tries carried %v; the attempts are %v, and s1 took %q", carried, states, bodies)
			if !slices.EqualFunc(carried, want, time.Time.Equal) {
				t.Errorf("the parked send's tries carried %v, want %v", carried, want)
			}
			if !slices.Equal(states, []string{"1:sent"}) || !slices.Equal(bodies, []string{"the parked one"}) {
				t.Fatalf("the sender let the parked send go with its attempts %v and s1 taking %q, want it sent once: a send let go under the hand-off's claim leaves the attempt pending under a claim no sender holds",
					states, bodies)
			}
		})
	}
}

// A hand-off - the delivery sweep's resumeOwedDelivery, or a keyed repeat - that reaches the
// sender after serve's claimedDuring found the claim unmoved and before next lets the send go
// must not be taken into the send that ends: its send would never be tried, held or released, and
// the attempt would wait under its claim, a lease, for the sweep. TestTheMentionSenderClaimModel
// finds it as a pending attempt under a live claim that no sender holds. The window is held open
// at reportLate's WARN, which the lane logs inside it; the control row hands off while the try is
// in flight, which claimedDuring catches.
func TestAHandOffAfterTheRetryCheckIsNotDropped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		atLog bool
	}{
		{name: "control: the hand-off lands while the try is in flight"},
		{name: "the hand-off lands between claimedDuring and next", atLog: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lifetime, stop := context.WithCancel(context.Background())
			var sender *mentionSender
			first := time.Now().Add(-30 * time.Second)
			sweeps := time.Now()
			item := mentionSend{
				comment: model.Comment{ID: "a-first", IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
				attempt: 1, claimed: first,
				// A send the delivery sweep handed over once before (held.send): it waited.
				heldFor: "s1", heldBy: model.MentionCopyHolder{EventID: 7}, heldSince: time.Now().Add(-2 * time.Minute),
			}
			handOff := item
			handOff.claimed = sweeps
			var handedOff atomic.Bool
			doHandOff := func() {
				if handedOff.CompareAndSwap(false, true) {
					sender.enqueue(context.Background(), handOff)
				}
			}
			logs := &storetest.LockedLog{}
			logtest.SetDefault(t, logHook{next: slog.NewTextHandler(logs, nil), hook: func(r slog.Record) {
				if tc.atLog && strings.Contains(r.Message, "waits behind an earlier event") {
					doHandOff()
				}
			}})
			var mu sync.Mutex
			var tried []time.Time
			triedHandOff := make(chan struct{}, 1)
			sender = newMentionSender(lifetime, time.Hour,
				func(_ context.Context, try mentionSend) (mentionSend, bool) {
					mu.Lock()
					tried = append(tried, try.claimed)
					mu.Unlock()
					if try.claimed.Equal(sweeps) {
						triedHandOff <- struct{}{}
						return mentionSend{}, false
					}
					// The row carries the sweep's claim: the try under the older one is refused.
					if !tc.atLog {
						doHandOff()
					}
					return mentionSend{}, false
				},
				func(context.Context, []mentionClaim) ([]mentionClaim, error) { return nil, nil },
				rereadNone,
				func(context.Context, []mentionClaim) error { return nil },
			)
			// However the subtest ends, a failed wait included, the sender's lifetime ends and its
			// goroutines are joined before the next subtest starts: its claim keeper would otherwise
			// outlive the subtest. The stop and the join below run first on the passing path, so
			// the tries are read once nothing can add one; a second stop and join do nothing.
			t.Cleanup(func() {
				stop()
				sender.wait()
			})
			sender.mu.Lock()
			sender.heldWarnAfter = time.Nanosecond
			sender.mu.Unlock()
			sender.enqueue(context.Background(), item)
			holds := func() bool {
				sender.mu.Lock()
				defer sender.mu.Unlock()
				_, held := sender.held[item.key()]
				return held
			}
			testwait.Until(t, fmt.Sprintf("a try under the hand-off's claim %v, which a dropped hand-off never gets: the attempt is left pending under a claim nothing refreshes or sends until it lapses (whether the sender holds an entry for the key)", sweeps), func() (bool, bool) {
				select {
				case <-triedHandOff:
					return holds(), true
				default:
					return holds(), false
				}
			})
			held := holds()
			stop()
			sender.wait()
			mu.Lock()
			defer mu.Unlock()
			t.Logf("handed off %t; tries carried %v; hand-off claim %v; sender still holds the key %t", handedOff.Load(),
				tried, sweeps, held)
		})
	}
}

// logHook passes every record to next and, first, to hook.
type logHook struct {
	next slog.Handler
	hook func(slog.Record)
}

func (h logHook) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h logHook) Handle(ctx context.Context, r slog.Record) error {
	h.hook(r)
	return h.next.Handle(ctx, r)
}

func (h logHook) WithAttrs(attrs []slog.Attr) slog.Handler {
	return logHook{next: h.next.WithAttrs(attrs), hook: h.hook}
}

func (h logHook) WithGroup(name string) slog.Handler {
	return logHook{next: h.next.WithGroup(name), hook: h.hook}
}
