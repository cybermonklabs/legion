// A session's interest record can be larger than one NATS message. POST /v1/interests/subscribe
// still registers such a session, reads its record back whole and renews the claims of its
// process at every heartbeat. A write the bus still refuses for its size is the caller's own input,
// answered 413 with the size and the bound, never the bus-unavailable 503 a retry cannot clear.
// Every case drives the real route over the package's own NATS server.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// wideTopics is a registration's topics of the shape a session with many issues sends: one
// wildcard over a project's issues, count issue topics under it whose last token holds
// tokenBytes letters, and the lane of role. Every issue topic is inside the wildcard, so the
// session consumer's filter reduces to a few subjects whatever their length, and only the
// interest record grows with them.
func wideTopics(count, tokenBytes int, role string) []string {
	topics := make([]string, 0, count+2)
	topics = append(topics, "notifications.dispatch.issue.K-0.>")
	for i := range count {
		topics = append(topics, fmt.Sprintf("notifications.dispatch.issue.K-0.t%04d-%s", i, strings.Repeat("x", tokenBytes)))
	}
	return append(topics, contracts.RoleTopicPrefix+role)
}

// registerTopics sends one registration of topics for sessionID by holdingProcess through
// subscribe, asking for the session's own consumer, and returns the answer. The time it logs is
// for the reader only: no case here reads it.
func registerTopics(t *testing.T, subscribe http.Handler, sessionID string, topics []string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id":         sessionID,
		"topics":             topics,
		"delivery":           map[string]any{"consumer": true, "epoch": "e"},
		"client_instance_id": holdingProcess,
	})
	if err != nil {
		t.Fatalf("encode the registration: %v", err)
	}
	recorder := httptest.NewRecorder()
	sent := time.Now()
	subscribe.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", bytes.NewReader(body)))
	took := time.Since(sent)
	answer := strings.TrimSpace(recorder.Body.String())
	if len(answer) > 400 {
		answer = answer[:400] + "..."
	}
	t.Logf("registration of %s: %d topics, %d bytes of them as JSON: answered %d in %s (Retry-After %q) %s",
		sessionID, len(topics), len(mustJSON(t, topics)), recorder.Code, took.Round(time.Millisecond), recorder.Header().Get("Retry-After"), answer)
	return recorder
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %T: %v", value, err)
	}
	return encoded
}

// requireWideRegistrationRenews registers sessionID's wide topics twice, the first registration
// and the heartbeat after it, and requires each to be answered 200, to renew the claim the
// session's process holds, and to leave a record that reads back whole from the bucket. pastMax
// says which side of the server's max payload the topics' JSON must be on, so the case and its
// control cannot drift to the same side.
//
// Each registration is given the time left before the test binary's deadline
// (budgetsUntilTheDeadline) in place of its 3.5 s (sendRequestBudget), and the cases assert what
// the registration writes, not how soon. The renewal runs last, on what the registration's work
// left of its budget, and a record past the max payload is about 1.2 MB the race build encodes,
// splits, writes and reads back on the runner's CPUs. Alone on four idle CPUs each registration
// took 0.38 to 0.47 s; beside 24 busy processes on those four CPUs, 4.8 to 7.2 s, and the
// renewal ran out of the product's budget in every run. CI's race job runs other packages' test
// binaries beside this one, and there the case failed with its claim unrenewed (envoy-go-race job
// 139003345250). The rows that assert the route's own budget keep it
// (write_routes_frozen_bus_test.go).
func requireWideRegistrationRenews(t *testing.T, sessionID string, tokenBytes int, pastMax bool) {
	t.Helper()
	const role = "legion-controller"
	topics := wideTopics(300, tokenBytes, role)
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	if size, maxPayload := len(mustJSON(t, topics)), int(conn.MaxPayload()); (size > maxPayload) != pastMax {
		t.Fatalf("the topics take %d bytes as JSON against the server's max payload of %d bytes: the case wants past it %v", size, maxPayload, pastMax)
	}
	rig := newRoleClaimRenewalRig(t)
	budgetsUntilTheDeadline(t, rig.deps)
	rig.seed(role, lapsedClaim(sessionID, ""))

	for _, heartbeat := range []string{"the first registration", "the heartbeat after it"} {
		registered := time.Now()
		recorder := registerTopics(t, rig.subscribe, sessionID, topics)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s was answered %d, want 200", heartbeat, recorder.Code)
		}
		var answer listenerwire.SubscribeResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the answer to %s: %v", heartbeat, err)
		}
		claim := rig.stored(role)
		if claim.ClientInstanceID != holdingProcess || claim.RenewedAt < registered.UnixMilli() {
			t.Fatalf("after %s the claim is %+v, want it adopted by %q and renewed at %d or later", heartbeat, claim, holdingProcess, registered.UnixMilli())
		}
		row, _, err := rig.registry.RefreshAt(t.Context(), sessionID)
		if err != nil {
			t.Fatalf("read %s's record from the bucket after %s: %v", sessionID, heartbeat, err)
		}
		if !slices.Equal(row.Topics, answer.Interest.Topics) {
			t.Fatalf("after %s the bucket's record holds %d topics, want the %d the answer named", heartbeat, len(row.Topics), len(answer.Interest.Topics))
		}
		for _, topic := range topics {
			if !slices.Contains(row.Topics, topic) {
				t.Fatalf("after %s the bucket's record lacks %s, one of the %d topics registered", heartbeat, topic, len(topics))
			}
		}
		t.Logf("%s: the record reads back whole, %d topics; the claim was renewed at %d", heartbeat, len(row.Topics), claim.RenewedAt)
		time.Sleep(5 * time.Millisecond) // the next renewal is later than this one by the clock it reads
	}
}

// The case: 300 issue topics of about 3,950 bytes each, about 1.18 MB as JSON, past the server's
// 1 MiB max payload.
func TestARegistrationPastTheMaxPayloadIsStoredWholeAndRenewsItsClaim(t *testing.T) {
	t.Parallel()
	requireWideRegistrationRenews(t, "ses_wide_long_topics", 3900, true)
}

// The control: the same 300 issue topics, each of about 50 bytes, under the max payload.
func TestARegistrationUnderTheMaxPayloadIsStoredWholeAndRenewsItsClaim(t *testing.T) {
	t.Parallel()
	requireWideRegistrationRenews(t, "ses_wide_short_topics", 10, false)
}

// An interest bucket whose largest value is smaller than a record refuses that record's write
// however often it is sent, so the registration is answered 413 naming the record's size and the
// bucket's bound, with no Retry-After, and the client is not told to retry a write that cannot
// land. A registration whose record fits the same bucket is the control: it is answered 200.
func TestARegistrationTheBusRefusesForItsSizeIsAnsweredTooLarge(t *testing.T) {
	t.Parallel()
	const valueBound = 4096
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, MaxValueSize: valueBound, Storage: natsgo.FileStorage}); err != nil {
		t.Fatalf("create the interest bucket with a largest value of %d bytes: %v", valueBound, err)
	}
	rig := newRoleClaimRenewalRig(t)

	if control := registerTopics(t, rig.subscribe, "ses_record_fits", []string{"notifications.team.heartbeat"}); control.Code != http.StatusOK {
		t.Fatalf("a registration whose record fits the bucket was answered %d, want 200", control.Code)
	}
	recorder := registerTopics(t, rig.subscribe, "ses_record_too_large", wideTopics(100, 40, "legion-controller"))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a registration whose record is larger than the bucket's largest value was answered %d, want 413", recorder.Code)
	}
	if retry := recorder.Header().Get("Retry-After"); retry != "" {
		t.Fatalf("the 413 carries Retry-After %q, want none: the same write is refused at every retry", retry)
	}
	var refusal listenerwire.APIError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}
	if !strings.Contains(refusal.Error, bus.ErrTooLarge.Error()) || !strings.Contains(refusal.Error, fmt.Sprint(valueBound)) || strings.Contains(refusal.Error, busUnansweredPrefix) {
		t.Fatalf("the refusal says %q, want it to name %q and the bound of %d bytes, and not %q", refusal.Error, bus.ErrTooLarge, valueBound, busUnansweredPrefix)
	}
}

// A registration whose topics no wildcard among them covers keeps every topic in its session
// consumer's filter, and a filter past the server's max payload makes a consumer create request
// the bus refuses however often it is sent. So the registration is answered 413 naming the filter's
// size and the bound, with no Retry-After, rather than a 503 the client retries for good. The
// control is the same topics under a wildcard that covers them, whose filter reduces to a few
// subjects: it is answered 200, and its record, larger than one message, is stored in chunks.
func TestARegistrationWhoseConsumerFilterIsPastTheMaxPayloadIsAnsweredTooLarge(t *testing.T) {
	// Not parallel: its two registrations of about 1.18 MB of topics must each be answered inside
	// the route's own 3.5 s budget (sendRequestBudget), and load beside it stretched such a
	// registration past it.
	rig := newRoleClaimRenewalRig(t)
	covered := wideTopics(300, 3900, "legion-controller")
	if control := registerTopics(t, rig.subscribe, "ses_filter_reduces", covered); control.Code != http.StatusOK {
		t.Fatalf("the topics under a wildcard that covers them were answered %d, want 200", control.Code)
	}
	recorder := registerTopics(t, rig.subscribe, "ses_filter_too_large", covered[1:])
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("the topics no wildcard covers were answered %d, want 413", recorder.Code)
	}
	if retry := recorder.Header().Get("Retry-After"); retry != "" {
		t.Fatalf("the 413 carries Retry-After %q, want none: the same create is refused at every retry", retry)
	}
	var refusal deliveryRefusal
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}
	if !strings.Contains(refusal.Error, "max payload") || !strings.Contains(refusal.Error, "1048576") {
		t.Fatalf("the refusal says %q, want it to name the server's max payload of 1048576 bytes", refusal.Error)
	}
	t.Logf("the refusal: %+v", refusal)
}

// requireStoppedSessionKnown registers sessionID's wide topics of tokenBytes letters each, and a
// team topic beside them, stops it cleanly and removes its interest row, as the reaper does for a
// session gone that long, so the known-session record is the one store left that knows it. It then
// sends to the session and publishes on the team topic, and requires the send to be stored for it
// (200, stopped) rather than refused 404, the publish to count it as a recipient that is not
// connected, and the record to hold every topic registered. The publish is on a team topic because
// the publish route refuses a `notifications.dispatch.` topic, which Dispatch alone publishes. Every
// one of the three is read and logged before any is judged, so a run that fails names all three
// answers.
//
// The registration is the case's setup, and it is given the time left before the test binary's
// deadline (budgetsUntilTheDeadline) in place of its 3.5 s: the known-session record is its last
// write, and a write its budget no longer covers is a WARN and no record at all. Beside 32 busy
// processes on four CPUs that write ran out of the product's budget in every run, and the case
// failed on a send answered 404 for a session it had just registered.
func requireStoppedSessionKnown(t *testing.T, sessionID string, tokenBytes int) {
	t.Helper()
	const topic = "notifications.team.counted"
	rig := newRecipientRig(t)
	budgetsUntilTheDeadline(t, rig.deps)
	topics := wideTopics(300, tokenBytes, "legion-controller")
	topics = append(topics[:len(topics)-1], topic) // no role lane: the session holds no role
	rig.register(t, recipientSender, []string{topic})
	rig.knownSessionRig.register(t, sessionID, map[string]any{"topics": topics})
	rig.stop(t, sessionID)
	rig.reapInterest(t, sessionID)

	sent := rig.send(t, sessionID, nil)
	t.Logf("a send to the stopped %s: status %d, state %q, recipient_state %q, error %q", sessionID, sent.status, sent.State, sent.RecipientState, sent.Error)
	published := rig.publish(t, topic, "stopped-"+sessionID)
	t.Logf("a publish on %s: %s", topic, strings.TrimSpace(published.Body.String()))
	record, known := rig.known.Get(sessionID)
	t.Logf("%s's known-session record: known %v, %d topics, %d bytes of them as JSON", sessionID, known, len(record.Topics), len(mustJSON(t, record.Topics)))

	sent.requireAnswer(t, "a send to the stopped "+sessionID, sessionStateStopped, session.Gone)
	assertCounts(t, published, 1, 0)
	if !known {
		t.Fatalf("the known-session store holds no record of %s", sessionID)
	}
	for _, registered := range topics {
		if !slices.Contains(record.Topics, registered) {
			t.Fatalf("%s's known-session record lacks %s, one of the %d topics registered", sessionID, registered, len(topics))
		}
	}
}

// The case: a session whose topics take about 1.18 MB as JSON, past the server's 1 MiB max payload.
// Its known-session record is stored in chunks, so once it stops a send to it is stored for it and
// a publish on its topics counts it. Before the record was stored in chunks its write was refused
// for its size, the registration answered 200 with a WARN, and the session had no record: the send
// answered 404 and the publish counted nobody.
func TestAStoppedSessionPastTheMaxPayloadIsStillSentToAndCounted(t *testing.T) {
	t.Parallel()
	requireStoppedSessionKnown(t, "ses_known_wide_long", 3900)
}

// The control: the same 300 topics, each of about 50 bytes, under the max payload.
func TestAStoppedSessionUnderTheMaxPayloadIsSentToAndCounted(t *testing.T) {
	t.Parallel()
	requireStoppedSessionKnown(t, "ses_known_wide_short", 10)
}
