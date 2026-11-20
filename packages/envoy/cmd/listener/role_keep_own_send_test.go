package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/listenerwire"
)

// A keep whose first send went unanswered and whose resend the stream refused as in process may
// have stored its copy (storeKeptRoleCopy's ownSend), so the 503 a kept role-route send answers says
// maybe_stored however the keep then ends: at the end of its window with the stream still answering
// "in process", with the next send refused by a full stream, or with the next send refused because
// the stream captures no subject of the kept lane (bus.ErrStreamLacksSubject), whose own 503 says
// that nothing stored the message and so yields to the maybe-stored one (writeBusUnavailable). The
// control is a keep whose every send is answered as the first was, which ends with the
// unacknowledged error last.
func TestAKeepWhoseOwnSendWentUnansweredSaysMaybeStoredHoweverItEnds(t *testing.T) {
	unanswered := fmt.Errorf("%w: %w", bus.ErrUnacknowledged, fmt.Errorf("%w: nats: duplicate message id is in process", bus.ErrDuplicateInProcess))
	for _, row := range []struct {
		name  string
		later error
	}{
		{"control: every later send answered as the first was", unanswered},
		{"every later send answered in process until the window ends", fmt.Errorf("%w: nats: duplicate message id is in process", bus.ErrDuplicateInProcess)},
		{"the next send refused by a full stream", fmt.Errorf("%w: maximum bytes exceeded", bus.ErrStreamFull)},
		{"the next send refused for want of the kept lane's subject", fmt.Errorf("%w: %s at generation 1000000 carries no subject capturing %q: %w",
			bus.ErrStreamLacksSubject, bus.Stream, bus.KeptRoleSubject("own-send"), natsgo.ErrNoStreamResponse)},
	} {
		t.Run(row.name, func(t *testing.T) {
			cfg := listenerDeliveryHandlerConfig{now: time.Now}
			calls := 0
			publish := func(ctx context.Context, subject string, item contracts.Envelope, msgID string) (bus.StoredAck, error) {
				calls++
				if calls == 1 {
					return bus.StoredAck{}, unanswered
				}
				return bus.StoredAck{}, row.later
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			item := listenerTestEnvelope(contracts.RoleTopicPrefix+"own-send", "own-send-00")
			_, err := storeKeptRoleCopy(ctx, cfg, "own-send", item, publish)
			if err == nil {
				t.Fatal("the keep stored the copy; want it to fail")
			}
			recorder := httptest.NewRecorder()
			status, _ := writeBusUnavailable(recorder, fmt.Errorf("the kept lane of role own-send did not store the message: %w", err))
			var body listenerwire.APIError
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode the answer %q: %v", recorder.Body.String(), err)
			}
			if body.Code != listenerwire.CodeMaybeStored {
				t.Fatalf("after %d sends the keep is answered %d with code %q (%s); want %q", calls, status, body.Code, body.Error, listenerwire.CodeMaybeStored)
			}
		})
	}
}

// A role send whose earlier forward went out and got no answer may have stored the message for its
// holder (sendToRole's unanswered: the bus marks such a forward bus.ErrUnacknowledged), so a later
// refusal for want of a subject is answered with the maybe-stored 503, recorded bus_unavailable,
// and not with the missing-subject 503 that says nothing stored it: the same rule as the keep's
// above. That holds for a later forward the stream refuses for want of the holder's subject, and
// for the keep the send makes once the holder let the role go, which the stream refuses for want
// of the kept lane's subject. A forward with no answer is one whose acknowledgement did not come
// within its window, or whose publish the client could not confirm. The controls are the same
// refusals where no earlier forward went unanswered, which still answer the missing-subject 503,
// recorded stream_lacks_subject: the refusal of the send's first forward, and refusals after a
// forward the server answered, that nothing took it (no responders) or with the stream's own error,
// since such a forward stored nothing.
func TestARoleSendWhoseEarlierForwardWentUnansweredSaysMaybeStoredWhenTheNextLacksTheSubject(t *testing.T) {
	const holder = "ses_forward_own_send_holder"
	subject := contracts.LegacyAgentSubject(holder)
	noResponders := bus.ForwardAnswered(natsgo.ErrNoResponders)
	streamError := bus.ForwardAnswered(&natsgo.APIError{Code: 503, ErrorCode: 10077, Description: "maximum messages exceeded"})
	unconfirmed := fmt.Errorf("%w: %w", bus.ErrForwardNotStored, fmt.Errorf("%w: the connection's last error changed", bus.ErrPublishUnconfirmed))
	lacks := fmt.Errorf("%w: %s at generation 1000000 carries no subject capturing %q: %w",
		bus.ErrStreamLacksSubject, bus.Stream, subject, noResponders)
	for i, row := range []struct {
		name     string
		answers  []error // what each forward's wait returns; the last one answers every later forward
		letGo    bool    // the holder lets the role go while the first forward is out, so the send keeps
		forwards int32
		code     string
		prefix   string
		outcome  requestOutcome
	}{
		{"control: the first forward refused for want of the holder's subject", []error{lacks}, false, 1, "", streamLacksSubjectPrefix, outcomeStreamLacksSubject},
		{"the forward after an unanswered one refused for want of the holder's subject", []error{bus.ErrForwardNotStored, lacks}, false, 2, listenerwire.CodeMaybeStored, busUnansweredPrefix, outcomeBusUnavailable},
		{"the forward after one whose unconfirmed publish went unanswered refused for want of the holder's subject", []error{unconfirmed, lacks}, false, 2, listenerwire.CodeMaybeStored, busUnansweredPrefix, outcomeBusUnavailable},
		{"control: the forward after one the server answered no responders refused for want of the holder's subject", []error{noResponders, lacks}, false, 2, "", streamLacksSubjectPrefix, outcomeStreamLacksSubject},
		{"control: the forward after one the stream answered with its error refused for want of the holder's subject", []error{streamError, lacks}, false, 2, "", streamLacksSubjectPrefix, outcomeStreamLacksSubject},
		{"the keep after an unanswered forward, the role let go, refused for want of the kept lane's subject", []error{bus.ErrForwardNotStored}, true, 1, listenerwire.CodeMaybeStored, busUnansweredPrefix, outcomeBusUnavailable},
		{"control: the keep after a forward the server answered no responders, the role let go, refused for want of the kept lane's subject", []error{noResponders}, true, 1, "", streamLacksSubjectPrefix, outcomeStreamLacksSubject},
	} {
		t.Run(row.name, func(t *testing.T) {
			role := fmt.Sprintf("forward-own-send-%d", i)
			harness := newListenerDeliveryHarness(t)
			claims, err := harness.client.JS().KeyValue(bus.RoleClaimBucket)
			if err != nil {
				t.Fatalf("open the role claim bucket %s: %v", bus.RoleClaimBucket, err)
			}
			forwards, keeps := &atomic.Int32{}, &atomic.Int32{}
			config := harness.config
			config.forwardRole = func(string, contracts.Envelope, string, time.Duration) (func() error, error) {
				answer := row.answers[min(int(forwards.Add(1)), len(row.answers))-1]
				return func() error {
					if row.letGo {
						// The holder lets the role go while this forward is out, so the send's
						// next resolution reads the role unclaimed and keeps the message.
						if err := claims.Delete(role); err != nil {
							t.Errorf("delete role %s's claim: %v", role, err)
						}
					}
					return answer
				}, nil
			}
			config.keepPublish = func(_ context.Context, kept string, _ contracts.Envelope, _ string) (bus.StoredAck, error) {
				keeps.Add(1)
				return bus.StoredAck{}, fmt.Errorf("%w: %s at generation 1000000 carries no subject capturing %q: %w",
					bus.ErrStreamLacksSubject, bus.Stream, kept, natsgo.ErrNoStreamResponse)
			}
			deps := harness.publishDeps(t)
			deps.delivery = &config
			if err := harness.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
				t.Fatalf("register the holder: %v", err)
			}
			claim := httptest.NewRecorder()
			roleSetHandler(&listenerDeps{registry: harness.registry, sessions: harness.sessions, delivery: &harness.config}, "test-machine", logging.New("test")).ServeHTTP(claim,
				postJSON("/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, holder, role)))
			if claim.Code != http.StatusOK {
				t.Fatalf("claim %s for %s: status %d, body %s", role, holder, claim.Code, claim.Body.String())
			}
			recorder, record := serveRecorded(t, func(logger *logging.Logger) http.HandlerFunc { return publishHandler(deps, logger) },
				postJSON("/v1/messages/publish", fmt.Sprintf(`{"topic":%q,"message":"role event","source":"agent"}`, contracts.RoleTopicPrefix+role)))
			var body listenerwire.APIError
			_ = json.Unmarshal(recorder.Body.Bytes(), &body)
			retryAfter := recorder.Header().Get("Retry-After")
			if recorder.Code != http.StatusServiceUnavailable || retryAfter != "1" || body.Code != row.code ||
				!strings.HasPrefix(body.Error, row.prefix+": ") || !strings.Contains(body.Error, bus.ErrStreamLacksSubject.Error()) {
				t.Errorf("after %d forwards and %d keeps the send is answered %d, Retry-After %q, code %q: %s; want 503, 1, code %q and words that open with %q and name the missing subject",
					forwards.Load(), keeps.Load(), recorder.Code, retryAfter, body.Code, body.Error, row.code, row.prefix)
			}
			if record["outcome"] != string(row.outcome) {
				t.Errorf("the record's outcome is %v, want %s", record["outcome"], row.outcome)
			}
			if got := forwards.Load(); got != row.forwards {
				t.Errorf("the send made %d forwards, want %d", got, row.forwards)
			}
			if got, want := keeps.Load() > 0, row.letGo; got != want {
				t.Errorf("the send made %d keeps; want a keep %t", keeps.Load(), want)
			}
		})
	}
}
