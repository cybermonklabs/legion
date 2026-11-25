package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// deliverySweepInterval is how often the maintenance loop runs the delivery sweep
// (api.MaintenanceSweepInterval), in whichever process holds its lease for that interval.
const deliverySweepInterval = api.MaintenanceSweepInterval

// sweepMargin is how long past the bound a test states the last send may land: one pass the
// leased loop may skip, because another process held the lease for that interval, plus the boot
// of the process that sends it, its sends and the poll that sees them settled.
const sweepMargin = deliverySweepInterval + 15*time.Second

// A deploy can find a mention's send in flight - its session's listener still answering - and a
// second comment's send to that same session queued behind it. SIGTERM starts no queued send, so
// the second attempt is left pending with no claim, for the delivery sweep; and
// Dispatch waits for the send in flight to settle before it closes its pool, so the outcome the
// listener answered is recorded rather than lost with the process.
func TestSIGTERMSettlesTheMentionSendInFlightAndLeavesTheQueuedOnePending(t *testing.T) {
	t.Parallel()
	var sends atomic.Int32
	arrived := make(chan struct{}, 2)
	answer := make(chan struct{})
	letAnswer := sync.OnceFunc(func() { close(answer) })
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			_, _ = w.Write([]byte(`{"session_id":"s1","title":"planner","capabilities":["steer"]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			sends.Add(1)
			arrived <- struct{}{}
			select {
			case <-answer:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	defer letAnswer()
	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		Env: map[string]string{"ENVOY_URL": listener.URL},
	})
	process.waitHealthy(t)
	key, _ := process.createIssue(t)
	inFlight := process.mention(t, key, "In flight at SIGTERM.", "session:s1")
	testwait.Receive(t, "the mention's send to reach the listener", arrived)
	queued := process.mention(t, key, "Queued behind it.", "session:s1")

	process.Terminate(t)
	process.WaitForOutput(t, `msg="dispatch: shutting down"`)
	select {
	case <-process.Exited:
		t.Fatalf("Dispatch exited with a mention send still in flight:\n%s", process.Output.String())
	default:
	}
	letAnswer()
	process.WaitExit(t, "SIGTERM")

	attempt := func(commentID string) (state string, claimed bool) {
		t.Helper()
		if err := database.Pool.QueryRow(context.Background(), `
			select state, claimed_at is not null from comment_deliveries where comment_id = $1 and attempt = 1
		`, commentID).Scan(&state, &claimed); err != nil {
			t.Fatalf("read the attempt of %s: %v", commentID, err)
		}
		return state, claimed
	}
	if state, _ := attempt(inFlight); state != "sent" {
		t.Errorf("the send in flight at SIGTERM left its attempt %s, want it settled sent before exit", state)
	}
	if state, claimed := attempt(queued); state != "pending" || claimed {
		t.Errorf("the queued send left its attempt %s (claimed %v), want it pending with no claim", state, claimed)
	}
	if got := sends.Load(); got != 1 {
		t.Errorf("the listener took %d sends, want only the one in flight at SIGTERM", got)
	}
	if t.Failed() {
		t.Logf("Dispatch's output:\n%s", process.Output.String())
	}
}

// mention posts a comment on the issue key mentioning each of targets and returns its id.
func (p *dispatchProcess) mention(t *testing.T, key, body string, targets ...string) string {
	t.Helper()
	id, err := p.postMention(key, body, targets...)
	if err != nil {
		t.Fatalf("create the comment: %v", err)
	}
	return id
}

// postMention posts a comment on the issue key mentioning each of targets and returns its id. It
// reports failures rather than ending the test, so a goroutine can call it.
func (p *dispatchProcess) postMention(key, body string, targets ...string) (string, error) {
	mentions := make([]map[string]string, 0, len(targets))
	for _, target := range targets {
		mentions = append(mentions, map[string]string{"target": target})
	}
	input, err := json.Marshal(map[string]any{"body": body, "mentions": mentions})
	if err != nil {
		return "", fmt.Errorf("encode the comment: %w", err)
	}
	answer, err := p.trySend(http.MethodPost, "/api/v1/issues/"+key+"/comments", string(input), asUser(dispatchTestLogin))
	if err != nil {
		return "", err
	}
	created := answer.body
	if answer.status != http.StatusCreated {
		return "", fmt.Errorf("status %d, want %d: %s", answer.status, http.StatusCreated, created)
	}
	var comment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &comment); err != nil || comment.ID == "" {
		return "", fmt.Errorf("decode the comment: %v: %s", err, created)
	}
	return comment.ID, nil
}

// handover is one send a handoverListener took: the session it was for and its idempotency key.
type handover struct{ session, key string }

// handoverListener is a stand-in Envoy listener for the mention tests that run Dispatch: every
// session it is asked about is live and takes a steer. It takes a send as handed to its recipient
// when it arrives, before it answers, as the real one publishes before it answers; so a resend
// repeats an idempotency key it has seen, and it answers that repeat as a duplicate without
// handing it over again, as the real listener's stream and its hosts drop a repeated Dispatch key.
// No send is answered until the test calls letAnswer. It notes when the last send arrived, by the
// test's clock, so a test that pauses (t.Parallel) still reads when the sends it waits for came.
type handoverListener struct {
	url       string
	letAnswer func()
	arrived   chan struct{}

	mu            sync.Mutex
	sends, handed map[handover]int
	last          time.Time
}

func newHandoverListener(t *testing.T) *handoverListener {
	t.Helper()
	answer := make(chan struct{})
	l := &handoverListener{
		letAnswer: sync.OnceFunc(func() { close(answer) }),
		arrived:   make(chan struct{}, 64),
		sends:     map[handover]int{},
		handed:    map[handover]int{},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch session, lookup := strings.CutPrefix(r.URL.Path, "/v1/registry/"); {
		case r.Method == http.MethodGet && lookup:
			_, _ = fmt.Fprintf(w, `{"session_id":%q,"title":"agent","capabilities":["steer"]}`, session)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession  string `json:"target_session"`
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode a mention send: %v", err)
			}
			send := handover{request.TargetSession, request.IdempotencyKey}
			l.mu.Lock()
			l.sends[send]++
			l.last = time.Now()
			duplicate := l.handed[send] > 0
			if !duplicate {
				l.handed[send]++
			}
			l.mu.Unlock()
			l.arrived <- struct{}{}
			select {
			case <-answer:
			case <-r.Context().Done():
				return
			}
			_, _ = fmt.Fprintf(w, `{"event_id":"mention-envelope","recipient":%q,"duplicate":%t}`, request.TargetSession, duplicate)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(l.letAnswer)
	l.url = server.URL
	return l
}

// awaitSends waits for n sends to arrive.
func (l *handoverListener) awaitSends(t *testing.T, process *dispatchProcess, n int) {
	t.Helper()
	for i := range n {
		testwait.Receive(t, fmt.Sprintf("the mention's send %d of %d reaching the listener", i+1, n), l.arrived)
	}
}

// receipt is one comment handed to one session.
type receipt struct{ comment, session string }

// handedComments counts the times each session was handed each comment, by the comment id that
// leads each idempotency key.
func (l *handoverListener) handedComments() map[receipt]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	got := map[receipt]int{}
	for send, count := range l.handed {
		comment, _, _ := strings.Cut(send.key, ":")
		got[receipt{comment, send.session}] += count
	}
	return got
}

// sendCounts counts the sends the listener took of each comment to each session, repeats included.
func (l *handoverListener) sendCounts() map[receipt]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	got := map[receipt]int{}
	for send, count := range l.sends {
		comment, _, _ := strings.Cut(send.key, ":")
		got[receipt{comment, send.session}] += count
	}
	return got
}

// lastSend is when the last send reached the listener.
func (l *handoverListener) lastSend() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// waitForSent waits until all want attempts of comments are sent, polling the database, and
// fails with process's output when they never are.
func waitForSent(t *testing.T, database *store.Store, process *dispatchProcess, comments []string, want int) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("%d attempts of the comments to be sent", want), func() (withOutput[int], bool) {
		var settled int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from comment_deliveries where comment_id = any($1) and state = 'sent'
		`, comments).Scan(&settled); err != nil {
			t.Fatalf("read the attempts: %v", err)
		}
		return readWith(settled, process.Output), settled == want
	})
}

// A process killed with mention sends in flight - SIGKILL, an out-of-memory kill, a lost node -
// leaves their attempts pending under its claim. The next process on the database sends the
// comment queued behind them at once, and each of them on the first delivery sweep after the
// killed process's claim has lapsed, within a minute and a sweep interval of that process taking
// it. Each resend repeats an idempotency key the listener has seen, so it is answered as a
// duplicate and not handed over again: every recipient is handed each comment exactly once. Both
// processes take one DISPATCH_SIGNING_KEY, as every process of a deployment does: the killed
// process's registry row is still live when the next starts, and a key the next minted in its own
// data dir would hold its start up to a live window, until that row lapsed (processes.Register).
//
// The claim is the real minute. The test pauses at t.Parallel once the next process runs
// (dispatchSlots.parallelHoldingItsProcesses), so the minute passes while the serial tests run; the bound is read
// off when the listener took the last resend, which a pause does not move.
func TestSIGKILLWithMentionSendsInFlightHandsEachRecipientTheCommentOnceTheClaimLapses(t *testing.T) {
	sigkillWithMentionSendsInFlightHandsEachRecipientTheCommentOnceTheClaimLapses(t, benchtime.EveryRun)
}

// TestSIGKILLWithMentionSendsInFlightHandsEachRecipientTheCommentOnceTheClaimLapsesOnTheClock is
// the row above in benchtime's timing lane, where the last resend is held to a claim lapse and a
// sweep interval of the kill.
func TestSIGKILLWithMentionSendsInFlightHandsEachRecipientTheCommentOnceTheClaimLapsesOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), sigkillWithMentionSendsInFlightHandsEachRecipientTheCommentOnceTheClaimLapses)
}

// sigkillWithMentionSendsInFlightHandsEachRecipientTheCommentOnceTheClaimLapses is the scene of
// the two rows above; lane says whether its reading of the clock is held to its figure
// (benchtime.Lane).
func sigkillWithMentionSendsInFlightHandsEachRecipientTheCommentOnceTheClaimLapses(t *testing.T, lane benchtime.Lane) {
	listener := newHandoverListener(t)
	database := storetest.Open(t)
	options := dispatchOptions{SigningKey: "sigkill-mention-sends-key", Env: map[string]string{"ENVOY_URL": listener.url}}
	killed := startDispatchProcessWith(t, database.Pool.Config().ConnString(), options)
	killed.waitHealthy(t)
	key, _ := killed.createIssue(t)
	inFlight := killed.mention(t, key, "In flight when the process was killed.", "session:s1", "session:s2", "session:s3")
	listener.awaitSends(t, killed, 3)
	queued := killed.mention(t, key, "Queued behind it.", "session:s1")

	if err := killed.Cmd.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL Dispatch: %v", err)
	}
	<-killed.Exited
	killedAt := time.Now()
	listener.letAnswer()
	next := startDispatchProcessWith(t, database.Pool.Config().ConnString(), options)
	next.waitHealthy(t)
	dispatchSlots.parallelHoldingItsProcesses(t)

	waitForSent(t, database, next, []string{inFlight, queued}, 4)
	lastResend := listener.lastSend().Sub(killedAt)
	t.Logf("the last resend reached the listener %v after the kill", lastResend)
	// How soon the sweep resends is a service level the claim's lapse and the sweep interval
	// promise, read on this machine's clock against timers inside the next process, so only the
	// row's twin in the timing lane holds it to a figure (benchtime.OnTheClock); that every
	// recipient is handed each comment exactly once is held on every run below.
	if bound := model.DeliveryClaimLapse + deliverySweepInterval + sweepMargin; lane == benchtime.OnTheClock && lastResend > bound {
		t.Errorf("the last resend reached the listener %v after the kill, want within %v", lastResend, bound)
	}

	want := map[receipt]int{
		{inFlight, "s1"}: 1, {inFlight, "s2"}: 1, {inFlight, "s3"}: 1, {queued, "s1"}: 1,
	}
	if got := listener.handedComments(); !maps.Equal(got, want) {
		t.Errorf("comments handed to each recipient = %v, want each of %v once", got, want)
	}
	for send, count := range listener.sendCounts() {
		if send.comment == inFlight && count != 2 {
			t.Errorf("the listener took %d sends of %v, want the killed process's and the one resend under its key", count, send)
		}
	}
}

// A rolling update (gcp:Dispatch with more than one replica rolls with maxSurge 1 and
// maxUnavailable 0) starts the successor and waits for it to be ready before it stops the old
// process. The old process gets SIGTERM with a mention's send in flight - its session's listener
// still answering - a second comment's send to that session queued behind it, and a third comment
// waiting on another writer's lock on the issue. SIGTERM starts no queued send, and the old
// process's sender refuses the third comment's send when the lock is released and the comment
// commits during the HTTP drain: both attempts stay pending with no claim. The delivery sweep
// sends them once they have waited a claim lease, within a lease and one sweep interval of the
// old process's exit, and every recipient is handed each comment exactly once. Both processes
// take one DISPATCH_SIGNING_KEY, as every process of a deployment does: a live server process
// refuses a successor whose signing keys it does not hold (processes.Register).
//
// The old process and its successor run at once, so the test reserves both slots before its first
// start. It pauses at t.Parallel once the old process has exited (dispatchSlots.parallelHoldingItsProcesses), so
// the claim lease and the sweep interval pass while the serial tests run; the bound is read off
// when the listener took the last send, which a pause does not move.
func TestARollingUpdatesSuccessorHandsEachMentionTheOldProcessLeftOnce(t *testing.T) {
	aRollingUpdatesSuccessorHandsEachMentionTheOldProcessLeftOnce(t, benchtime.EveryRun)
}

// TestARollingUpdatesSuccessorHandsEachMentionTheOldProcessLeftOnceOnTheClock is the row above in
// benchtime's timing lane, where the last send is held to a claim lapse and a sweep interval of
// the old process's exit.
func TestARollingUpdatesSuccessorHandsEachMentionTheOldProcessLeftOnceOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), aRollingUpdatesSuccessorHandsEachMentionTheOldProcessLeftOnce)
}

// aRollingUpdatesSuccessorHandsEachMentionTheOldProcessLeftOnce is the scene of the two rows
// above; lane says whether its reading of the clock is held to its figure (benchtime.Lane).
func aRollingUpdatesSuccessorHandsEachMentionTheOldProcessLeftOnce(t *testing.T, lane benchtime.Lane) {
	dispatchSlots.reserve(t, 2)
	listener := newHandoverListener(t)
	database := storetest.Open(t)
	options := dispatchOptions{SigningKey: "rolling-update-mentions-key", Env: map[string]string{"ENVOY_URL": listener.url}}
	old := startDispatchProcessWith(t, database.Pool.Config().ConnString(), options)
	old.waitHealthy(t)
	successor := startDispatchProcessWith(t, database.Pool.Config().ConnString(), options)
	successor.waitHealthy(t)
	key, artifactID := old.createIssue(t)
	// The issue's spec settles on its own timer, which would queue behind the lock too.
	waitForSettlementOwed(t, database, artifactID, false)

	inFlight := old.mention(t, key, "In flight at SIGTERM.", "session:s1")
	listener.awaitSends(t, old, 1)
	queued := old.mention(t, key, "Queued behind it.", "session:s1")
	holder := lockIssue(t, database, key)
	type posted struct {
		id  string
		err error
	}
	answered := make(chan posted, 1)
	go func() {
		id, err := old.postMention(key, "Accepted during the HTTP drain.", "session:s2")
		answered <- posted{id, err}
	}()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the old process's output:\n%s", old.Output.String())
		}
	})
	storetest.WaitForLockWaiters(t, holder, 1)

	old.Terminate(t)
	old.WaitForOutput(t, `msg="dispatch: shutting down"`)
	if err := holder.Rollback(context.Background()); err != nil {
		t.Fatalf("release the issue lock: %v", err)
	}
	got := testwait.Receive(t, "the comment waiting on the lock to be answered once the lock was released", answered)
	if got.err != nil {
		t.Fatalf("the comment accepted during the HTTP drain: %v:\n%s", got.err, old.Output.String())
	}
	drained := got.id
	old.WaitForOutput(t, "comment mention not sent: the mention sender has stopped")
	listener.letAnswer()
	old.WaitExit(t, "SIGTERM")
	exited := time.Now()
	dispatchSlots.parallelHoldingItsProcesses(t)

	waitForSent(t, database, successor, []string{inFlight, queued, drained}, 3)
	lastSend := listener.lastSend().Sub(exited)
	t.Logf("the last send reached the listener %v after the old process exited", lastSend)
	// The same service level as above, and held to its figure by the same lane.
	if bound := model.DeliveryClaimLapse + deliverySweepInterval + sweepMargin; lane == benchtime.OnTheClock && lastSend > bound {
		t.Errorf("the last send reached the listener %v after the old process exited, want within %v", lastSend, bound)
	}

	want := map[receipt]int{{inFlight, "s1"}: 1, {queued, "s1"}: 1, {drained, "s2"}: 1}
	if got := listener.handedComments(); !maps.Equal(got, want) {
		t.Errorf("comments handed to each recipient = %v, want each of %v once", got, want)
	}
	if got := listener.sendCounts(); !maps.Equal(got, want) {
		t.Errorf("sends the listener took = %v, want one of each of %v", got, want)
	}
}
