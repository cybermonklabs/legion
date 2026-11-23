// Two answers of a publish whose stream verdict names no other request's copy: a verdict on the
// request's own first send, which a publish made again can get (bus.StoredAck.Resent), answers as
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
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// closeUnder serves one request through serve while the server behind closing stops answering:
// the first message the request sends goes out on the frozen connection, the connection is closed
// under it, and the server answers again. The bus sends a publish whose caller reads the duplicate
// verdict once (bus.Client.PublishAckWithin), so a publish the close cuts off answers the close
// rather than being sent again. It answers what the route answered.
func closeUnder(t *testing.T, server *testnats.Server, closing *natsgo.Conn, serve func(*httptest.ResponseRecorder)) *httptest.ResponseRecorder {
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
// The last row closes the listener's connection under a send to an unresponsive session
// (closeUnder): the send's publish is the message the close cuts off. The bus sends a publish whose
// caller reads the duplicate verdict once, a close included (sendStored), so the send answers 503
// maybe_stored, and the client's retry under the same key is answered by the stored-copy rule, the
// stream holding exactly one copy. A real resend of a publish whose answer a lost connection took,
// on the topic publish no sender limit counts, is TestATopicPublishNoLimitCountsAnswersItsStoredCopyAcrossALostAck's.
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
		duplicate, err := publishHeld(ctx, client, storedThen(own), &requestRecord{charge: charge}, requestSender{source: item.Source, session: item.SourceSession}, &item)
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
		duplicate, err := publishHeld(ctx, client, storedThen(other), &requestRecord{}, requestSender{source: item.Source, session: item.SourceSession}, &item)
		if err != nil || !duplicate || !reflect.DeepEqual(item, other) {
			t.Fatalf("a verdict on another request's copy answered duplicate %t, %+v (%v); want duplicate and that copy whole, %+v",
				duplicate, item, err, other)
		}
	})

	t.Run("case: a send whose connection closed under it answers 503 maybe_stored, and a retry under the same key gets the one stored copy", func(t *testing.T) {
		// The bus sends a publish whose caller reads the duplicate verdict once, a close of its
		// connection included (bus.Client.PublishAckWithin, sendStored): the send route reads the
		// verdict (publishHeld), so the close is its answer, 503 maybe_stored, since the stream may
		// hold the copy. The client's retry under the same key is what reaches the copy.
		server, uri := testnats.Start(t)
		d, now := stalledSessionsRigOn(t, uri)
		const target = "ses_closed_unresponsive"
		register(t, d, target, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
		now.advance(90 * time.Second)
		body := fmt.Sprintf(`{"target_session":%q,"source":"dispatch","message":"sent once, maybe twice","idempotency_key":%q}`, target, uuid.NewString())
		recorder := closeUnder(t, server, d.client.Connection(), func(recorder *httptest.ResponseRecorder) {
			sendHandler(d, logging.New("test")).ServeHTTP(recorder, postJSON("/v1/messages/send", body))
		})
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"code":"maybe_stored"`) {
			t.Fatalf("the send whose connection closed under it answered %d %s, want 503 maybe_stored", recorder.Code, recorder.Body.String())
		}
		// The client's retry under the same key, on the connection that replaced the closed one: a
		// copy the first send stored makes it a repeat, answered duplicate by the stored-copy rule, which for
		// a sender that names no session and reads no key-taken answer is the envelope its request
		// built (requestSender.answer); no copy makes it the first copy, answered whole with no
		// duplicate. Either way the stream holds exactly one copy afterwards, and nothing is lost.
		retry := httptest.NewRecorder()
		sendHandler(d, logging.New("test")).ServeHTTP(retry, postJSON("/v1/messages/send", body))
		var answer ownCopyAnswer
		if err := json.Unmarshal(retry.Body.Bytes(), &answer); err != nil || retry.Code != http.StatusOK {
			t.Fatalf("the client's retry under the same key answered %d %s (%v), want 200", retry.Code, retry.Body.String(), err)
		}
		subject := contracts.LegacyAgentSubject(target)
		stored := heldCopy(t, d.client, subject)
		switch {
		case answer.Duplicate && (stored.DedupeKey != answer.DedupeKey || stored.EventID == answer.EventID):
			t.Fatalf("the retry answered duplicate with %+v; want a repeat of the first send's copy under its key, %+v", answer.Envelope, stored)
		case !answer.Duplicate && !reflect.DeepEqual(answer.Envelope, stored):
			t.Fatalf("the retry answered no duplicate with %+v; want the one stored copy whole, %+v", answer.Envelope, stored)
		}
		observer, err := testnats.Connect(t, uri).JetStream()
		if err != nil {
			t.Fatalf("open the observer's JetStream context: %v", err)
		}
		info, err := observer.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
		if err != nil {
			t.Fatalf("read what the stream holds on %s: %v", subject, err)
		}
		if held := info.State.Subjects[subject]; held != 1 {
			t.Fatalf("the stream holds %d copies on %s after the retry, want exactly one", held, subject)
		}
		t.Logf("CLOSE-ROW the retry answered duplicate %t with event %s, the one stored copy", answer.Duplicate, stored.EventID)
	})
}

// A verdict that names no copy is the stored-copy 503 at once, with Retry-After: 1 and no envelope,
// recorded stored_copy_unreadable: a role forward the stream acknowledges as a duplicate with no
// sequence (bus.ErrDuplicateWithoutSequence, as a forward's Wait names it). The control is a role
// forward the stream never acknowledges, which is tried again until the send's window ends and
// answered 504 delivery_failed.
//
// A role send whose earlier forward failed with no refusal may have stored the copy a later
// verdict names, so the stored-copy 503 that follows says maybe_stored, whether the verdict names
// no sequence or a copy that cannot be read, as the send and topic routes' does after a resend.
// The controls are the same two verdicts on a send's first forward, which carry no code.
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

	rig := newSenderLimitRig(t)
	rig.limit(60, 10)
	// roleSend publishes one role message whose forwards the stream answers with verdict, handed
	// each forward's number from 1, and returns the route's answer and the forwards it made.
	roleSend := func(t *testing.T, verdict func(n int) (bus.StoredAck, error)) (limitedAnswer, int) {
		t.Helper()
		role := "no-copy-" + id.New()
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, "ses_no_copy_holder_"+id.New(), role, nil)
		saved := rig.deps.delivery
		t.Cleanup(func() { rig.deps.delivery = saved })
		forwards := rig.interceptForwards(func(n int, _ func() (func() (bus.StoredAck, error), error)) (func() (bus.StoredAck, error), error) {
			return func() (bus.StoredAck, error) { return verdict(n) }, nil
		})
		answer := rig.publishFrom(t, "ses_no_copy_sender", contracts.RoleTopicPrefix+role, map[string]any{"idempotency_key": uuid.NewString()})
		return answer, *forwards
	}
	// always answers every forward with err; unansweredThen answers the first forward with
	// bus.ErrForwardNotStored, a forward that failed with no refusal, and every later one with
	// ack and err.
	always := func(err error) func(int) (bus.StoredAck, error) {
		return func(int) (bus.StoredAck, error) { return bus.StoredAck{}, err }
	}
	unansweredThen := func(ack bus.StoredAck, err error) func(int) (bus.StoredAck, error) {
		return func(n int) (bus.StoredAck, error) {
			if n == 1 {
				return bus.StoredAck{}, bus.ErrForwardNotStored
			}
			return ack, err
		}
	}
	noSequence := fmt.Errorf("%w: %w", bus.ErrForwardNotStored, bus.ErrDuplicateWithoutSequence)
	// unreadable is a duplicate verdict at a sequence the stream holds nothing at.
	unreadable := bus.StoredAck{Sequence: 1 << 40, Duplicate: true}
	// requireCode fails unless the answer's body carries code, "" for none.
	requireCode := func(t *testing.T, what string, answer limitedAnswer, code string) {
		t.Helper()
		var body listenerwire.APIError
		if err := json.Unmarshal([]byte(answer.raw), &body); err != nil || body.Code != code {
			t.Fatalf("%s answered %s (%v), want the code %q", what, answer.raw, err, code)
		}
	}

	t.Run("case: a role forward acknowledged as a duplicate with no sequence, which costs one", func(t *testing.T) {
		answer, forwards := roleSend(t, always(noSequence))
		requireUnread(t, "a role forward acknowledged with no sequence", answer.status, answer.retryAfter, answer.raw, answer.line["outcome"])
		if forwards != 1 {
			t.Fatalf("the role send made %d forwards, want 1: answered at once, with no retry of the forward", forwards)
		}
		answer.requireNamed(t, "a role forward acknowledged with no sequence", "ses_no_copy_sender")
		requireCode(t, "a first role forward acknowledged with no sequence", answer, "")
	})

	t.Run("control: a first role forward acknowledged as a duplicate whose copy cannot be read carries no code", func(t *testing.T) {
		answer, forwards := roleSend(t, func(int) (bus.StoredAck, error) { return unreadable, nil })
		requireUnread(t, "a first role forward whose duplicate cannot be read", answer.status, answer.retryAfter, answer.raw, answer.line["outcome"])
		if forwards != 1 {
			t.Fatalf("the role send made %d forwards, want 1", forwards)
		}
		requireCode(t, "a first role forward whose duplicate cannot be read", answer, "")
	})

	for _, row := range []struct {
		name string
		ack  bus.StoredAck
		err  error
	}{
		{"case: a role forward unanswered once and then acknowledged as a duplicate with no sequence says maybe_stored", bus.StoredAck{}, noSequence},
		{"case: a role forward unanswered once and then acknowledged as a duplicate whose copy cannot be read says maybe_stored", unreadable, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			answer, forwards := roleSend(t, unansweredThen(row.ack, row.err))
			requireUnread(t, "a role send whose first forward went unanswered", answer.status, answer.retryAfter, answer.raw, answer.line["outcome"])
			if forwards != 2 {
				t.Fatalf("the role send made %d forwards, want 2: the one unanswered and the one whose verdict names a copy", forwards)
			}
			requireCode(t, "a role send whose first forward went unanswered", answer, listenerwire.CodeMaybeStored)
		})
	}

	t.Run("control: a role forward the stream never acknowledges is tried again and answered 504 when the window ends", func(t *testing.T) {
		answer, forwards := roleSend(t, always(bus.ErrForwardNotStored))
		answer.requireStatus(t, "a role forward never acknowledged", http.StatusGatewayTimeout)
		answer.requireOutcome(t, "a role forward never acknowledged", outcomeDeliveryFailed)
		if forwards < 2 {
			t.Fatalf("the role send made %d forwards, want it tried again until the window ends", forwards)
		}
	})
}
