package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestACommentMentionOfAHiddenSessionNeverLeavesAnAttemptOwed: alice, an open viewer who does not
// see PRIV, comments on PUB's issue mentioning privateSession, which has acted in PRIV and which
// the walk's listener holds live, and her Retry of that mention opens an attempt of its own
// (deliverResolvedCommentMention, unbound, under her reader, as POST
// /api/v1/comments/{id}/deliveries calls it). The delivery sweep resumes an attempt nothing
// resolved before a crash, and resolves it as the comment's author (resumeOwedDelivery), so:
//
//   - the comment's attempt 1 stranded as a lookup cut short by its bound leaves it, naming neither
//     a session nor an error, is not sent to privateSession by the sweep;
//   - the Retry commits no attempt pending and unresolved for privateSession: it writes its
//     attempt failed, "no live session", in its claim transaction (openHiddenCommentDeliveryAttempt);
//   - each pending attempt any of these writes did commit, put back as a crash right after that
//     commit leaves it, is not sent to privateSession by the sweep either.
//
// A constraint trigger the test adds records every pending attempt for privateSession a
// transaction commits, as the row stands at that commit.
//
// Controls: resumeOwedDelivery resolving with no reader (the server's), and the stranded attempt 1
// is sent to privateSession; openHiddenCommentDeliveryAttempt off, and the Retry commits its attempt
// pending and unresolved.
func TestACommentMentionOfAHiddenSessionNeverLeavesAnAttemptOwed(t *testing.T) {
	walk := seedHiddenWrites(t)
	ctx := context.Background()
	target := "session:" + privateSession
	if _, err := walk.database.Pool.Exec(ctx, `
		create table walked_pending_mentions (logged bigserial primary key, comment_id uuid, attempt int, session_id text, resolve_error text);
		create function walked_log_pending_mention() returns trigger language plpgsql as $$
		begin
			insert into walked_pending_mentions (comment_id, attempt, session_id, resolve_error)
			select d.comment_id, d.attempt, d.session_id, d.resolve_error
			from comment_deliveries d
			where d.comment_id = new.comment_id and d.target = new.target and d.attempt = new.attempt
			  and d.state = 'pending' and d.target = 'session:`+privateSession+`';
			return null;
		end $$;
		create constraint trigger walked_pending_mention after insert or update on comment_deliveries
			deferrable initially deferred for each row execute function walked_log_pending_mention();
	`); err != nil {
		t.Fatalf("record every pending attempt a transaction commits for %s: %v", privateSession, err)
	}
	commentID := createdID(t, walk.request(t, callerAlice, http.MethodPost, "/api/v1/issues/"+walk.issue[seedPublic]+"/comments",
		map[string]any{"body": "@" + privateSession + " look", "delivery": "btw", "mentions": []map[string]string{{"target": target}}}))
	awaitBackgroundWrites(t, walk.server)

	alice, err := walk.server.readerFor(ctx, model.BrowserActor(seedAlice))
	if err != nil {
		t.Fatalf("resolve what alice sees: %v", err)
	}
	comment, err := walk.server.loadComment(ctx, walk.database.Pool, everyReader, commentID)
	if err != nil {
		t.Fatalf("load alice's comment: %v", err)
	}
	created, err := walk.server.loadCommentCreatedEvent(ctx, walk.database.Pool, comment)
	if err != nil {
		t.Fatalf("load alice's comment's event: %v", err)
	}
	retried, held, err := walk.server.deliverResolvedCommentMention(withReader(ctx, alice), comment, created,
		ResolvedMention{Target: target, Delivery: "btw"}, model.BrowserActor(seedAlice), attemptClaim{}, walk.server.newTakeoverRetry())
	if err != nil || held != nil {
		t.Fatalf("alice's Retry of her mention of %s: %v (held %v)", privateSession, err, held)
	}
	if retried.State != model.DeliveryStateFailed || retried.Error == nil || *retried.Error != noLiveSessionPrefix+" "+privateSession {
		t.Errorf("alice's Retry answers attempt %d %s %v, want it failed: %s %s", retried.Attempt, retried.State, retried.Error, noLiveSessionPrefix, privateSession)
	}
	awaitBackgroundWrites(t, walk.server)

	var unresolved int
	if err := walk.database.Pool.QueryRow(ctx, `
		select count(*) from walked_pending_mentions where resolve_error is null and attempt > 1`).Scan(&unresolved); err != nil {
		t.Fatalf("read the Retry's committed pending attempts: %v", err)
	}
	if unresolved > 0 {
		t.Errorf("alice's Retry committed %d attempts pending and unresolved for %s, a session she may not see", unresolved, privateSession)
	}
	// Every pending commit the writes made, read before the sweeps below, whose own restores the
	// trigger records too.
	rows, err := walk.database.Pool.Query(ctx, `select attempt, session_id, resolve_error from walked_pending_mentions order by logged`)
	if err != nil {
		t.Fatalf("read the committed pending attempts: %v", err)
	}
	type committed struct {
		attempt       int
		session, fail *string
	}
	var commits []committed
	for rows.Next() {
		var c committed
		if err := rows.Scan(&c.attempt, &c.session, &c.fail); err != nil {
			t.Fatalf("read a committed pending attempt: %v", err)
		}
		commits = append(commits, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the committed pending attempts: %v", err)
	}
	if len(commits) == 0 {
		t.Fatalf("no write committed a pending attempt for %s, not even the comment's attempt 1: the log trigger records nothing, and the row proves nothing", privateSession)
	}

	// sweep puts attempt back pending as a crash leaves it - its claim lapsed, made long enough ago
	// that the sweep owes it - runs the sweep, and holds it to sending privateSession nothing.
	sweep := func(state string, attempt int, session, resolveError *string) {
		t.Helper()
		if _, err := walk.database.Pool.Exec(ctx, `
			update comment_deliveries
			set state = 'pending', error = null, envelope_id = null, duplicate = false, session_id = $3, resolve_error = $4,
			    claimed_at = now() - interval '10 minutes', created_at = now() - interval '10 minutes'
			where comment_id = $1::uuid and target = $5 and attempt = $2`,
			commentID, attempt, session, resolveError, target); err != nil {
			t.Fatalf("%s: put attempt %d back pending: %v", state, attempt, err)
		}
		walk.calls.take()
		if err := walk.server.resumeOwedDeliveries(ctx); err != nil {
			t.Fatalf("%s: sweep: %v", state, err)
		}
		awaitBackgroundWrites(t, walk.server)
		var reached int
		if err := walk.database.Pool.QueryRow(ctx, `
			select count(*) from comment_deliveries where comment_id = $1::uuid and state in ('sent', 'queued')`,
			commentID).Scan(&reached); err != nil {
			t.Fatalf("%s: read the mention's attempts: %v", state, err)
		}
		if reached > 0 {
			t.Errorf("%s: the delivery sweep sent alice's comment to %s in %d attempts, a session she may not see", state, privateSession, reached)
		}
		for _, call := range walk.calls.take() {
			if strings.HasPrefix(call, "POST /v1/messages/send") {
				t.Errorf("%s: the delivery sweep asked the listener to send (%s)", state, call)
			}
		}
	}
	sweep("attempt 1 stranded as a lookup cut short by its bound leaves it", 1, nil, nil)
	for _, c := range commits {
		encoded, _ := json.Marshal(c.fail)
		sweep(fmt.Sprintf("attempt %d as its commit left it, resolve_error %s", c.attempt, encoded), c.attempt, c.session, c.fail)
	}
}

// TestWithNoPrivateProjectTheSweepResolvesAMentionAsBefore (R9): the delivery sweep resolves a
// stranded mention attempt as the comment's author (resumeOwedDelivery, attemptWriter), one read
// per attempt, and with no private project that author sees every session, so the sweep answers
// the attempt as it did before projects could be private, when it resolved as the server: once
// PRIV is public again, alice's mention of privateSession on PUB's issue, its attempt 1 put back as
// a lookup cut short by its bound leaves it, is sent to privateSession.
//
// Control: resumeOwedDelivery resolving as the closed viewer (closedViewer) in place of the
// author; the sweep then answers the attempt "no live session" and sends nothing, and the row
// fails.
func TestWithNoPrivateProjectTheSweepResolvesAMentionAsBefore(t *testing.T) {
	walk := seedHiddenWrites(t)
	ctx := context.Background()
	walk.setVisibility(t, seedPrivate, model.ProjectPublic)
	target := "session:" + privateSession
	commentID := createdID(t, walk.request(t, callerAlice, http.MethodPost, "/api/v1/issues/"+walk.issue[seedPublic]+"/comments",
		map[string]any{"body": "@" + privateSession + " look", "delivery": "btw", "mentions": []map[string]string{{"target": target}}}))
	awaitBackgroundWrites(t, walk.server)
	if _, err := walk.database.Pool.Exec(ctx, `
		update comment_deliveries
		set state = 'pending', error = null, envelope_id = null, duplicate = false, session_id = null, resolve_error = null,
		    claimed_at = now() - interval '10 minutes', created_at = now() - interval '10 minutes'
		where comment_id = $1::uuid and target = $2 and attempt = 1`, commentID, target); err != nil {
		t.Fatalf("put attempt 1 back as a lookup cut short by its bound leaves it: %v", err)
	}
	walk.calls.take()
	if err := walk.server.resumeOwedDeliveries(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	awaitBackgroundWrites(t, walk.server)
	var state, session, failure string
	if err := walk.database.Pool.QueryRow(ctx, `
		select state, coalesce(session_id, ''), coalesce(error, '') from comment_deliveries
		where comment_id = $1::uuid and target = $2 and attempt = 1`,
		commentID, target).Scan(&state, &session, &failure); err != nil {
		t.Fatalf("read attempt 1: %v", err)
	}
	if (state != model.DeliveryStateSent && state != "queued") || session != privateSession {
		t.Errorf("PRIV public: the sweep answers alice's stranded attempt 1 %s to %q (error %q), want it sent to %s, as before",
			state, session, failure, privateSession)
	}
	sent := false
	for _, call := range walk.calls.take() {
		sent = sent || strings.HasPrefix(call, "POST /v1/messages/send")
	}
	if !sent {
		t.Errorf("PRIV public: the sweep asked the listener for no send of alice's stranded attempt 1")
	}
}

// TestARetryResumesNoAttemptPinnedToASessionItsCallerMayNotSee: carol mentions a role alice may
// read on PUB's issue, and the mention's attempt 1 is left pending by a sender that never came
// back, its claim lapsed, pinned to privateSession, which alice may not see. alice's Retry of the
// mention (deliverResolvedCommentMention, unbound, under her reader, as POST
// /api/v1/comments/{id}/deliveries calls it) does not resume that attempt: it answers an attempt
// of its own that names nothing of privateSession, and leaves attempt 1 to its own sender: pending,
// or superseded once her own attempt reached the role's holder. Neither then, nor after the
// delivery sweep, is attempt 1 settled with alice's refusal of privateSession.
//
// Control: claimCommentDeliveryAttempt's unbound read taking a pending attempt whatever session
// it is pinned to; alice's Retry answers attempt 1, failed, "no live session" privateSession.
func TestARetryResumesNoAttemptPinnedToASessionItsCallerMayNotSee(t *testing.T) {
	walk := seedHiddenWrites(t)
	ctx := context.Background()
	target := "role:" + walkedRole
	commentID := createdID(t, walk.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+walk.issue[seedPublic]+"/comments",
		map[string]any{"body": "@" + walkedRole + " look", "delivery": "btw", "mentions": []map[string]string{{"target": target}}}))
	awaitBackgroundWrites(t, walk.server)
	if _, err := walk.database.Pool.Exec(ctx, `
		update comment_deliveries
		set state = 'pending', error = null, envelope_id = null, duplicate = false, session_id = $3, resolve_error = null,
		    claimed_at = now() - interval '10 minutes', created_at = now() - interval '10 minutes'
		where comment_id = $1::uuid and target = $2 and attempt = 1`, commentID, target, privateSession); err != nil {
		t.Fatalf("leave attempt 1 pending, pinned to %s, its claim lapsed: %v", privateSession, err)
	}
	alice, err := walk.server.readerFor(ctx, model.BrowserActor(seedAlice))
	if err != nil {
		t.Fatalf("resolve what alice sees: %v", err)
	}
	comment, err := walk.server.loadComment(ctx, walk.database.Pool, everyReader, commentID)
	if err != nil {
		t.Fatalf("load carol's comment: %v", err)
	}
	created, err := walk.server.loadCommentCreatedEvent(ctx, walk.database.Pool, comment)
	if err != nil {
		t.Fatalf("load carol's comment's event: %v", err)
	}
	retried, _, err := walk.server.deliverResolvedCommentMention(withReader(ctx, alice), comment, created,
		ResolvedMention{Target: target, Delivery: "btw"}, model.BrowserActor(seedAlice), attemptClaim{}, walk.server.newTakeoverRetry())
	if err != nil {
		t.Fatalf("alice's Retry of carol's mention: %v", err)
	}
	answer, _ := json.Marshal(retried)
	if retried.Attempt == 1 || strings.Contains(string(answer), privateSession) {
		t.Errorf("alice's Retry answers %s: it resumed the attempt pinned to %s, a session she may not see", answer, privateSession)
	}
	awaitBackgroundWrites(t, walk.server)
	var state string
	var failure *string
	if err := walk.database.Pool.QueryRow(ctx, `
		select state, error from comment_deliveries where comment_id = $1::uuid and target = $2 and attempt = 1`,
		commentID, target).Scan(&state, &failure); err != nil {
		t.Fatalf("read attempt 1: %v", err)
	}
	// alice's own attempt reaches the role's holder, so attempt 1 either stays pending or is settled
	// superseded by it (supersedeOvertakenAttempts: one copy of the mention to its recipient), and
	// never with alice's refusal of privateSession.
	if state != "pending" && deref(failure) != mentionSupersededError {
		t.Errorf("after alice's Retry attempt 1 is %s (%s), want it pending or superseded by her own attempt, never settled with her refusal of %s", state, deref(failure), privateSession)
	}
	if err := walk.server.resumeOwedDeliveries(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	awaitBackgroundWrites(t, walk.server)
	if err := walk.database.Pool.QueryRow(ctx, `
		select state, error from comment_deliveries where comment_id = $1::uuid and target = $2 and attempt = 1`,
		commentID, target).Scan(&state, &failure); err != nil {
		t.Fatalf("read attempt 1 after the sweep: %v", err)
	}
	if state == "pending" || (failure != nil && *failure == noLiveSessionPrefix+" "+privateSession) {
		t.Errorf("after the sweep attempt 1 is %s (%v), want it settled as carol's, not with alice's refusal of %s", state, deref(failure), privateSession)
	}
}
