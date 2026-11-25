package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// TestAMessageToAHiddenSessionNeverLeavesAnAttemptOwed: alice, an open viewer who does not see
// PRIV, messages privateSession, which has acted in PRIV and which the walk's listener holds live,
// and retries it. Alice reads that target as none, so the Retry answers the message as one with no
// target and claims nothing (createDelivery). Neither write commits an attempt pending for that
// session at any moment - not the message's own attempt 1, unpinned and headed for it, nor an
// attempt pinned to it - so a process killed between any two of the writes' commits leaves the
// delivery sweep nothing to send: the sweep resolves a session route as the server's reader
// (resolveDeliveryTarget), which sees privateSession, and would send it. A constraint trigger the
// test adds records every pending attempt for privateSession that a transaction commits, as the
// row stands at that commit; each one is then put back as a crash right after that commit leaves
// it, its claim lapsed, and a sweep runs. It sends privateSession nothing and queues nothing on its
// subject.
//
// Control: createStoredMessage's hidden attempt 1 off. The message's commit records its attempt 1
// pending; restored, the sweep sends it to the session and the row fails naming it.
func TestAMessageToAHiddenSessionNeverLeavesAnAttemptOwed(t *testing.T) {
	walk := seedHiddenWrites(t)
	ctx := context.Background()
	if _, err := walk.database.Pool.Exec(ctx, fmt.Sprintf(`
		create table walked_pending_commits (logged bigserial primary key, message_id uuid, attempt int, session_id text);
		create function walked_log_pending() returns trigger language plpgsql as $$
		begin
			insert into walked_pending_commits (message_id, attempt, session_id)
			select d.message_id, d.attempt, d.session_id
			from message_deliveries d join messages m on m.id = d.message_id
			where d.message_id = new.message_id and d.attempt = new.attempt and d.state = 'pending'
			  and (d.session_id = %[1]s or (coalesce(d.session_id, '') = '' and m.target = 'session:' || %[1]s));
			return null;
		end $$;
		create constraint trigger walked_pending_commit after insert or update on message_deliveries
			deferrable initially deferred for each row execute function walked_log_pending();
	`, "'"+privateSession+"'")); err != nil {
		t.Fatalf("record every pending attempt a transaction commits for %s: %v", privateSession, err)
	}
	sent := walk.request(t, callerAlice, http.MethodPost, "/api/v1/issues/"+walk.issue[seedPublic]+"/messages",
		json.RawMessage(`{"body":"To a session alice may not see.","target":"session:`+privateSession+`","delivery":"btw"}`))
	message := createdID(t, sent)
	if part := deliveryPart(t, sent.Body.Bytes()); !strings.Contains(part, `"state":"failed"`) || !strings.Contains(part, "no live session "+privateSession) {
		t.Errorf("alice's message to %s answers %s, want it failed: no live session", privateSession, part)
	}
	retried := walk.request(t, callerAlice, http.MethodPost, "/api/v1/messages/"+message+"/deliveries", json.RawMessage(`{"delivery":"btw"}`))
	if retried.Code != http.StatusNotFound || strings.TrimSpace(retried.Body.String()) != `{"code":"MESSAGE_NOT_FOUND","error":"message not found"}` {
		t.Errorf("alice's Retry of it answers %d %s, want it answered as a message with no target: 404 message not found", retried.Code, retried.Body.String())
	}
	awaitBackgroundWrites(t, walk.server)

	var committed int
	if err := walk.database.Pool.QueryRow(ctx, `select count(*) from walked_pending_commits`).Scan(&committed); err != nil {
		t.Fatalf("read the committed pending attempts: %v", err)
	}
	if committed > 0 {
		t.Errorf("the writes committed %d attempts pending for %s, a session the writer may not see", committed, privateSession)
	}
	// Each committed pending attempt as a crash right after its commit leaves it: pending as it
	// stood then, made and claimed long enough ago that the sweep owes it (owedMessageAttempt).
	if _, err := walk.database.Pool.Exec(ctx, `
		update message_deliveries d
		set state = 'pending', error = null, envelope_id = null, session_id = last.session_id,
		    claimed_at = now() - interval '10 minutes', created_at = now() - interval '10 minutes'
		from (
			select distinct on (message_id, attempt) message_id, attempt, session_id
			from walked_pending_commits order by message_id, attempt, logged desc
		) last
		where d.message_id = last.message_id and d.attempt = last.attempt`); err != nil {
		t.Fatalf("put the committed pending attempts back: %v", err)
	}
	if err := walk.server.resumeOwedDeliveries(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	awaitBackgroundWrites(t, walk.server)
	var reached int
	if err := walk.database.Pool.QueryRow(ctx, `
		select count(*) from message_deliveries where message_id = $1::uuid and state in ('sent', 'queued')
	`, message).Scan(&reached); err != nil {
		t.Fatalf("read the message's attempts: %v", err)
	}
	if reached > 0 {
		t.Errorf("the delivery sweep sent alice's message to %s in %d attempts, a session she may not see", privateSession, reached)
	}
	for _, envelope := range publishedBy(t, walk.deps) {
		if envelope.Topic == contracts.LegacyAgentSubject(privateSession) {
			t.Errorf("Dispatch queued an envelope on %s's own subject (%s)", privateSession, envelope.DedupeKey)
		}
	}
}
