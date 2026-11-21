// Two answers of a publish whose stream verdict names no other request's copy: a verdict on the
// request's own first send, which the bus's resend after a closed connection can get, answers as
// a first send does, and a verdict that names no copy at all answers the stored-copy 503 at once.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// resendUnderClose serves one request through serve while the server behind closing stops
// answering: the request's publish goes out on the frozen connection, the connection is closed
// under it, and the server answers again, so the bus sends the publish again on the connection it
// dials in its place (bus.StoredAck.Resent). It answers what the route answered.
func resendUnderClose(t *testing.T, server *testnats.Server, closing *natsgo.Conn, serve func(*httptest.ResponseRecorder)) *httptest.ResponseRecorder {
	t.Helper()
	thaw := server.Freeze(t)
	t.Cleanup(thaw)
	before := closing.Stats().OutMsgs
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		serve(recorder)
		answered <- recorder
	}()
	waitFor(t, 3*time.Second, "the publish to go out on the frozen connection", func() bool {
		return closing.Stats().OutMsgs > before
	})
	closing.Close()
	thaw()
	select {
	case recorder := <-answered:
		return recorder
	case <-time.After(15 * time.Second):
		t.Fatal("the publish whose connection closed under it did not answer")
		return nil
	}
}

// ownCopyAnswer is what a resent request answered, as these rows read it.
type ownCopyAnswer struct {
	contracts.Envelope
	Duplicate bool `json:"duplicate"`
}

// A duplicate verdict on the request's own copy answers that copy with no duplicate, and one on
// another request's copy answers it with duplicate: publishHeld tells the two apart by the copy's
// event_id. The first two rows stand in for the stream's answer to a resend (bus.StoredAck.Resent)
// after the copy the verdict names was really stored, one carrying this request's envelope and one
// another request's, so each reaches the rule whichever order the server would have taken. The
// resend's own copy costs its sender one, as any copy the request stored does (senderCharge).
//
// The last two rows are the real resend, on the topic publish and on a send to an unresponsive
// session: the listener's own server stops answering, the publish goes out on the frozen
// connection, the connection is closed under it, and the bus sends it again on the connection it
// dials in its place. The stream holds one copy, which carries this request's own event_id, and
// the 200 names it with no duplicate; the publish costs one and the send is counted as stored for
// an unresponsive session. Whether the first send or the resend stored that copy is the server's
// order, so these rows show the routes end to end, and the first row is the one that fails, every
// run, for a rule that marks every duplicate verdict or forgives a resend's. Each row compares the
// whole answered envelope with the copy the stream holds, so an answer that names the copy's
// event_id over another body fails too.
func TestADuplicateVerdictOnTheRequestsOwnCopyAnswersNoDuplicate(t *testing.T) {
	client := setupPublishTestClient(t)
	topic := "notifications.team.own-copy-" + id.New()
	key := publishDedupePrefix + uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// storedThen stores stored on topic under key's message id, as the stream holds the first
	// send's copy, and answers the publish as the stream answers the resend.
	storedThen := func(stored contracts.Envelope) storedPublish {
		return func(ctx context.Context, item contracts.Envelope) (bus.StoredAck, error) {
			ack, err := client.PublishStoredAck(ctx, item.Topic, stored, bus.ForwardMsgID(item.DedupeKey, item.Topic))
			if err != nil {
				return bus.StoredAck{}, err
			}
			return bus.StoredAck{Sequence: ack.Sequence, Duplicate: true, Resent: true}, nil
		}
	}
	envelope := func() contracts.Envelope {
		return contracts.MessageEnvelope(contracts.Message{SourceSession: "ses_own_copy", Text: "sent twice"}, topic, key)
	}

	t.Run("case: the verdict names this request's own copy, answered with no duplicate, and costs one", func(t *testing.T) {
		item := envelope()
		own := item
		charge := &senderCharge{sender: "ses_own_copy", giveBack: func() {}}
		duplicate, err := publishHeld(ctx, client, storedThen(own), charge, requestSender{source: item.Source, session: item.SourceSession}, &item)
		if err != nil || duplicate || !reflect.DeepEqual(item, own) {
			t.Fatalf("a resend's verdict on the request's own copy answered duplicate %t, %+v (%v); want no duplicate and its own copy whole, %+v",
				duplicate, item, err, own)
		}
		if got := charge.counted(); got != "ses_own_copy" {
			t.Fatalf("a resend's own copy cost the sender %q, want ses_own_copy: the request stored it", got)
		}
	})

	t.Run("control: the verdict names another request's copy, answered with duplicate and that copy", func(t *testing.T) {
		item := envelope()
		item.DedupeKey = publishDedupePrefix + uuid.NewString()
		other := item
		other.EventID = id.New()
		duplicate, err := publishHeld(ctx, client, storedThen(other), nil, requestSender{source: item.Source, session: item.SourceSession}, &item)
		if err != nil || !duplicate || !reflect.DeepEqual(item, other) {
			t.Fatalf("a verdict on another request's copy answered duplicate %t, %+v (%v); want duplicate and that copy whole, %+v",
				duplicate, item, err, other)
		}
	})

	t.Run("case: a publish sent again after its connection closed under it answers its own copy with no duplicate, and costs one", func(t *testing.T) {
		rig, server := newSenderLimitRigOnServer(t)
		rig.limit(60, 5)
		const sender = "ses_resent_publish_sender"
		subject := "notifications.team.resent-" + id.New()
		body := fmt.Sprintf(`{"topic":%q,"source":"agent","source_session":%q,"message":"sent once, maybe twice","idempotency_key":%q}`, subject, sender, uuid.NewString())
		logged := len(rig.harness.logs.String())
		recorder := resendUnderClose(t, server, rig.harness.client.Connection(), func(recorder *httptest.ResponseRecorder) {
			rig.mux.ServeHTTP(recorder, postJSON("/v1/messages/publish", body))
		})
		var answer ownCopyAnswer
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != http.StatusOK {
			t.Fatalf("the resent publish answered %d %s (%v), want 200", recorder.Code, recorder.Body.String(), err)
		}
		stored := heldCopy(t, rig.harness.client, subject)
		if answer.Duplicate || !reflect.DeepEqual(answer.Envelope, stored) {
			t.Fatalf("the resent publish answered duplicate %t and %+v; want no duplicate and the one stored copy whole, %+v",
				answer.Duplicate, answer.Envelope, stored)
		}
		// The copy is this request's own, which the stream stored for it: it costs one.
		lines := linesIn(rig.harness.logs.String()[logged:], "listener publish", nil)
		if len(lines) != 1 || lines[0]["sender"] != sender {
			t.Fatalf("the resent publish wrote record lines %v, want one naming the sender %s it cost one", lines, sender)
		}
	})

	t.Run("case: a send to an unresponsive session sent again after its connection closed under it is counted as stored", func(t *testing.T) {
		server, uri := testnats.Start(t)
		d, now := stalledSessionsRigOn(t, uri)
		const target = "ses_resent_unresponsive"
		register(t, d, target, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
		now.advance(90 * time.Second)
		met := metrics.New()
		d.sendsToUnresponsive = met.NewCounter("envoy_sends_unresponsive_total", "Sends accepted and stored for a recipient past the live window")
		body := fmt.Sprintf(`{"target_session":%q,"source":"dispatch","message":"sent once, maybe twice","idempotency_key":%q}`, target, uuid.NewString())
		recorder := resendUnderClose(t, server, d.client.Connection(), func(recorder *httptest.ResponseRecorder) {
			sendHandler(d, logging.New("test")).ServeHTTP(recorder, postJSON("/v1/messages/send", body))
		})
		var answer ownCopyAnswer
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != http.StatusOK {
			t.Fatalf("the resent send answered %d %s (%v), want 200", recorder.Code, recorder.Body.String(), err)
		}
		stored := heldCopy(t, d.client, contracts.LegacyAgentSubject(target))
		if answer.Duplicate || !reflect.DeepEqual(answer.Envelope, stored) {
			t.Fatalf("the resent send answered duplicate %t and %+v; want no duplicate and the one stored copy whole, %+v",
				answer.Duplicate, answer.Envelope, stored)
		}
		scrape := httptest.NewRecorder()
		met.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if !strings.Contains(scrape.Body.String(), "\nenvoy_sends_unresponsive_total 1\n") {
			t.Fatalf("envoy_sends_unresponsive_total after the resent send stored for an unresponsive session is not 1:\n%s", scrape.Body.String())
		}
	})
}

// A verdict that names no copy is the stored-copy 503 at once, with Retry-After: 1 and no envelope,
// recorded stored_copy_unreadable, on each route that meets one: a publish the stream refuses while
// an earlier one under the same message id is in process (bus.ErrDuplicateInProcess, as the bus
// names nats-server's 10158), and a role forward the stream acknowledges as a duplicate with no
// sequence (bus.ErrDuplicateWithoutSequence, as a forward's Wait names it). The control is a role
// forward the stream never acknowledges, which is tried again until the send's window ends and
// answered 504 delivery_failed.
func TestADuplicateVerdictThatNamesNoCopyAnswers503AtOnce(t *testing.T) {
	requireUnread := func(t *testing.T, what string, status int, retryAfter string, raw string, outcome any) {
		t.Helper()
		if status != http.StatusServiceUnavailable || retryAfter != "1" {
			t.Fatalf("%s answered %d with Retry-After %q, want 503 with 1; body %s", what, status, retryAfter, raw)
		}
		if !strings.Contains(raw, `"error":"`+storedCopyUnreadablePrefix+": ") || strings.Contains(raw, `"event_id"`) {
			t.Fatalf("%s answered %s, want the stored-copy error and no envelope", what, raw)
		}
		if outcome != string(outcomeStoredCopyUnreadable) {
			t.Fatalf("%s was recorded %v, want %s", what, outcome, outcomeStoredCopyUnreadable)
		}
	}

	t.Run("case: a publish refused while an earlier one under its id is in process", func(t *testing.T) {
		inProcess := fmt.Errorf("%w: %w", bus.ErrDuplicateInProcess,
			&natsgo.APIError{Code: 409, ErrorCode: 10158, Description: "duplicate message id is in process"})
		item := contracts.MessageEnvelope(contracts.Message{Text: "in process"}, "notifications.team.in-process", publishDedupePrefix+uuid.NewString())
		_, err := publishHeld(context.Background(), nil, func(context.Context, contracts.Envelope) (bus.StoredAck, error) {
			return bus.StoredAck{}, inProcess
		}, nil, requestSender{}, &item)
		var logs strings.Builder
		record := &requestRecord{logger: logging.NewWithWriter("test", &logs), line: "listener publish", laneKey: "topic", item: &item}
		recorder := httptest.NewRecorder()
		record.refuseBusUnavailable(recorder, err)
		lines := linesIn(logs.String(), "listener publish", nil)
		if len(lines) != 1 {
			t.Fatalf("the refusal wrote %d record lines, want one:\n%s", len(lines), logs.String())
		}
		requireUnread(t, "a publish in process under an earlier one", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String(), lines[0]["outcome"])
	})

	rig := newSenderLimitRig(t)
	rig.limit(60, 5)
	roleSend := func(t *testing.T, waitErr error) (limitedAnswer, int) {
		t.Helper()
		role := "no-copy-" + id.New()
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, "ses_no_copy_holder_"+id.New(), role, nil)
		saved := rig.deps.delivery
		t.Cleanup(func() { rig.deps.delivery = saved })
		forwards := rig.interceptForwards(func(int, func() (func() (bus.StoredAck, error), error)) (func() (bus.StoredAck, error), error) {
			return func() (bus.StoredAck, error) { return bus.StoredAck{}, waitErr }, nil
		})
		answer := rig.publishFrom(t, "ses_no_copy_sender", contracts.RoleTopicPrefix+role, map[string]any{"idempotency_key": uuid.NewString()})
		return answer, *forwards
	}

	t.Run("case: a role forward acknowledged as a duplicate with no sequence, which costs one", func(t *testing.T) {
		answer, forwards := roleSend(t, fmt.Errorf("%w: %w", bus.ErrForwardNotStored, bus.ErrDuplicateWithoutSequence))
		requireUnread(t, "a role forward acknowledged with no sequence", answer.status, answer.retryAfter, answer.raw, answer.line["outcome"])
		if forwards != 1 {
			t.Fatalf("the role send made %d forwards, want 1: answered at once, with no retry of the forward", forwards)
		}
		answer.requireNamed(t, "a role forward acknowledged with no sequence", "ses_no_copy_sender")
	})

	t.Run("control: a role forward the stream never acknowledges is tried again and answered 504 when the window ends", func(t *testing.T) {
		answer, forwards := roleSend(t, bus.ErrForwardNotStored)
		answer.requireStatus(t, "a role forward never acknowledged", http.StatusGatewayTimeout)
		answer.requireOutcome(t, "a role forward never acknowledged", outcomeDeliveryFailed)
		if forwards < 2 {
			t.Fatalf("the role send made %d forwards, want it tried again until the window ends", forwards)
		}
	})
}
