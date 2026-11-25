package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// c-one-leader-read (dispatch://INFRA-5459) in Dispatch's reply check. A message and a mention for
// a route to role R go to holder A. The NATS server then stores a message under R's key on its
// own - a RePublish of another stream - whose body is a claim naming B, a live session. No writer
// wrote it, and a direct get cannot tell it from a claim: it reads the message as B's claim (the
// control), and so does the listener's role lookup, GET /v1/roles/<role>, which reads the claim
// by a direct get (the stand-in listener answers B, live, as that lookup does). B's reply and its
// error callback, on the message and on the mention, are not the holder's: the reply check reads
// R's claim from the role bucket stream's leader with the listener's one decoding
// (roles.Buckets.RoleClaim), which names no holder for such a message, and refuses them 403
// REPLY_FORBIDDEN. The control that the check reads that bucket and is not refusing every
// session: once B claims R by a writer's write, B's reply is accepted.
func TestARoleReplyOverAServerWrittenClaimIsNotTheHoldersReply(t *testing.T) {
	t.Parallel()
	const (
		role    = "reply-server-written"
		holderA = "ses_reply_written_a"
		holderB = "ses_reply_written_b"
	)
	ctx := context.Background()
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect Dispatch's bus client: %v", err)
	}
	t.Cleanup(client.Close)
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	registry, err := envoystore.Open(t.Context(), conn)
	if err != nil {
		t.Fatalf("open the role registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(t.Context(), conn)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	for _, live := range []string{holderA, holderB} {
		if err := sessions.Put(live, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
			t.Fatalf("register %s live: %v", live, err)
		}
	}
	if _, err := registry.SetRole(holderA, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holderA, err)
	}

	listener := newRoleRouteListener(t)
	listener.claim(role, holderA, true)
	options := testServerOptions{envoyURL: listener.server.URL}
	withRoleHolders(&options, roles.NewBuckets(client, roles.Windows{}))
	handler, database, _ := newTestServer(t, options)
	issue := createInteractionIssue(t, handler, "TEST", "Replies over a server-written claim", "before")
	rig := &roleRouteReplies{listener: listener, handler: handler, database: database, issue: issue.Key}
	message := rig.message(t, role, holderA, "Who carries the release?")
	failed := rig.message(t, role, holderA, "Can you rerun the build?")
	comment := rig.mention(t, role, holderA, "Who reviews the plan?")

	body, err := json.Marshal(envoystore.RoleClaim{HolderSessionID: holderB, ClaimedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode the claim the RePublish stores: %v", err)
	}
	testnats.RepublishesKey(t, conn, bus.RoleClaimBucket, role, body)
	if direct, err := registry.RoleClaim(ctx, role); err != nil || direct.HolderSessionID != holderB {
		t.Fatalf("a direct get of %s's claim reads %+v, %v; want %s's claim, or the control shows nothing a reader could take for one", role, direct, err, holderB)
	}
	// What the listener's role lookup answers over that record: its direct get names B, live.
	listener.claim(role, holderB, true)

	replyRefused(t, "B's reply to the message over the server-written claim", rig.replyMessage(t, message, holderB, map[string]any{"body": "I carry it."}, false))
	replyRefused(t, "B's error callback on the message over the server-written claim", rig.replyMessage(t, failed, holderB, map[string]any{"error": "no build host"}, false))
	replyRefused(t, "B's reply to the mention over the server-written claim", rig.replyComment(t, comment, holderB, map[string]any{"body": "I review it."}))
	if session, state, replyID := rig.messageAttempt(t, message); session != holderA || state != "sent" || replyID != nil {
		t.Fatalf("the message attempt after B's refused reply = session %s state %s reply %v, want A's attempt, sent and unanswered", session, state, replyID)
	}

	// A writer's put of B's claim over the record, the same bytes, as the claim route writes one.
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	roleBucket, err := js.KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.RoleClaimBucket, err)
	}
	if _, err := roleBucket.Put(role, body); err != nil {
		t.Fatalf("put %s's claim of %s: %v", holderB, role, err)
	}
	if leader, err := registry.RoleClaimFromLeader(ctx, role); err != nil || leader.HolderSessionID != holderB {
		t.Fatalf("the leader's read of %s's claim after B's claim = %+v, %v; want B's claim", role, leader, err)
	}
	if reply := rig.replyMessage(t, message, holderB, map[string]any{"body": "I carry it now."}, false); reply.Code != http.StatusCreated {
		t.Fatalf("B's reply once B's own claim stands: status=%d body=%s, want 201", reply.Code, reply.Body.String())
	}
}
