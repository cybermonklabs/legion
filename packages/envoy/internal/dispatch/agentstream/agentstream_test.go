package agentstream_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/testnats"
)

// replayHub starts a NATS server with Dispatch's user, which may do anything, and one session user
// per id in sessions holding that session's own grant: publish its replies (bus.SessionReplies)
// and its frames, subscribe its relay and control subjects. It returns the server's URL, a
// connection as Dispatch's user, and each session's seed by id.
func replayHub(t *testing.T, sessions ...string) (string, *natsgo.Conn, map[string]string) {
	t.Helper()
	seeds := map[string]string{}
	entries := make([]string, 0, len(sessions))
	for _, sessionID := range sessions {
		seed, public := testnats.User(t)
		seeds[sessionID] = seed
		entries = append(entries, fmt.Sprintf("{ nkey: %q, permissions: { publish: { allow: [%q, %q] }, subscribe: { allow: [%q, %q] } } }",
			public, bus.SessionReplies(sessionID), contracts.AgentStreamFramesSubject(sessionID),
			bus.DeliverSubject(sessionID), contracts.AgentStreamControlSubject(sessionID)))
	}
	config := fmt.Sprintf(`authorization {
  users = [
    { user: dispatch, password: dispatch }
    %s
  ]
}
`, strings.Join(entries, "\n    "))
	_, uri := testnats.StartConfig(t, config, nil, false)
	dispatch, err := natsgo.Connect(uri, natsgo.UserInfo("dispatch", "dispatch"))
	if err != nil {
		t.Fatalf("connect as Dispatch's user: %v", err)
	}
	t.Cleanup(dispatch.Close)
	return uri, dispatch, seeds
}

// controlResponder is a session on its control subject, answering every replay request with
// history, as both plugins do; refused records each publish the server refused it.
type controlResponder struct {
	replies atomic.Int32
	mu      sync.Mutex
	reply   string
	refused []string
}

func startControlResponder(t *testing.T, uri, seed, sessionID string, history []byte) *controlResponder {
	t.Helper()
	responder := &controlResponder{}
	conn, err := testnats.ConnectSeed(t, uri, seed, natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
		responder.mu.Lock()
		defer responder.mu.Unlock()
		responder.refused = append(responder.refused, err.Error())
	}))
	if err != nil {
		t.Fatalf("connect as session %s: %v", sessionID, err)
	}
	t.Cleanup(conn.Close)
	if _, err := conn.Subscribe(contracts.AgentStreamControlSubject(sessionID), func(msg *natsgo.Msg) {
		if msg.Reply == "" {
			return
		}
		responder.mu.Lock()
		responder.reply = msg.Reply
		responder.mu.Unlock()
		responder.replies.Add(1)
		_ = msg.Respond(history)
	}); err != nil {
		t.Fatalf("subscribe the control subject of %s: %v", sessionID, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	return responder
}

func (r *controlResponder) refusals() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.refused)
}

// A session holding only its own grant answers Dispatch's replay request, which asks on a reply
// subject naming that session (bus.SessionReplySubject), and Replay returns the history. The
// control is the request Replay made before, on nats.go's own request inbox: the same session's
// answer to it is refused by the server and the request waits out its deadline, so the grant the
// session holds is one that would keep its history from the viewer had Replay not moved.
func TestAReplayIsAnsweredUnderTheSessionsOwnGrant(t *testing.T) {
	const sessionID = "ses_replay"
	history := []byte(`{"v":1,"type":"replay","frames":[]}`)
	uri, dispatch, seeds := replayHub(t, sessionID)
	responder := startControlResponder(t, uri, seeds[sessionID], sessionID, history)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	replay, err := agentstream.NewNATS(dispatch).Replay(ctx, sessionID)
	if err != nil || string(replay) != string(history) {
		t.Fatalf("Replay answered %q, %v; want the session's history %q", replay, err, history)
	}
	responder.mu.Lock()
	reply := responder.reply
	responder.mu.Unlock()
	if !bus.SubjectContains(bus.SessionReplies(sessionID), reply) {
		t.Fatalf("the replay asked on reply subject %q, which the session's grant entry %q does not admit", reply, bus.SessionReplies(sessionID))
	}

	before := responder.replies.Load()
	oldCtx, oldCancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer oldCancel()
	if answer, err := dispatch.RequestWithContext(oldCtx, contracts.AgentStreamControlSubject(sessionID), []byte(`{"v":1,"type":"replay"}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the request on nats.go's own inbox answered %v, %v; want its deadline, the session's answer refused", answer, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(responder.refusals()) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the server refused the session %v, want exactly its one answer on nats.go's request inbox", responder.refusals())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if refusal := responder.refusals()[0]; !strings.Contains(refusal, `Permissions Violation for Publish to "`+natsgo.InboxPrefix) {
		t.Fatalf("the server refused %q, want the answer on nats.go's request inbox", refusal)
	}
	// Read only now: the request's deadline can pass before the session's callback runs, and the
	// refusal above comes after that callback counted the request and answered it.
	if responder.replies.Load() != before+1 {
		t.Fatalf("the session heard %d requests after Replay, want the one", responder.replies.Load()-before)
	}
}

// Replay's two answers that are not a history, on its own reply subject: nobody on the control
// subject is ErrNoResponder, the server's no-responders status, and not the deadline's empty
// answer, and a session that keeps silent is no history and no error once the deadline passes,
// as before.
func TestAReplayNobodyAnswersIsNoResponderOrNothing(t *testing.T) {
	uri, dispatch, seeds := replayHub(t, "ses_silent")
	silent, err := testnats.ConnectSeed(t, uri, seeds["ses_silent"])
	if err != nil {
		t.Fatalf("connect the silent session: %v", err)
	}
	t.Cleanup(silent.Close)
	if _, err := silent.SubscribeSync(contracts.AgentStreamControlSubject("ses_silent")); err != nil {
		t.Fatalf("subscribe the silent session: %v", err)
	}
	if err := silent.Flush(); err != nil {
		t.Fatalf("flush the silent session: %v", err)
	}
	source := agentstream.NewNATS(dispatch)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if replay, err := source.Replay(ctx, "ses_nobody"); !errors.Is(err, agentstream.ErrNoResponder) || replay != nil {
		t.Fatalf("a replay nobody listens for answered %q, %v; want ErrNoResponder", replay, err)
	}

	silentCtx, silentCancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer silentCancel()
	if replay, err := source.Replay(silentCtx, "ses_silent"); err != nil || replay != nil {
		t.Fatalf("a replay a silent session holds answered %q, %v; want no history and no error", replay, err)
	}
}
