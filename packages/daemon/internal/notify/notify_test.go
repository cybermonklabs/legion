package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/listenerwire"
)

// The controller topic is the one the plugin's controller subscribes to
// (legionControllerNoticeSubject, packages/contracts/src/subject.ts), byte for byte.
func TestControllerTopicIsTheControllersNoticeSubject(t *testing.T) {
	if got := ControllerTopic("legion"); got != "notifications.legion.legion.controller" {
		t.Fatalf("ControllerTopic = %q, want the controller's notice subject", got)
	}
}

func TestHTTPPublisherSendsPayloadAndOutboxDedupeKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The Envoy listener answers an unauthenticated publish with 401 (packages/envoy/cmd/listener/apiauth.go).
		if r.Header.Get("Authorization") != "Bearer envoy-bearer" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/publish" {
			t.Fatalf("request = %s %s, want POST /v1/messages/publish", r.Method, r.URL.Path)
		}
		// The body is read as the listener's publish route reads it. The daemon keeps its own
		// queue of notices, so it asks the listener to keep nothing and to answer a role nobody
		// holds with its 404 (ErrNoHolder). A missing keep would read as the listener's default,
		// which is to keep.
		var body listenerwire.PublishRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest) // the listener answers "invalid json"
			return
		}
		if body.Topic != "notifications.legion.LEGION.LEGION-208" || body.Message != "worker-died on LEGION-208" || body.DedupeKey != "legion-outbox:42" {
			t.Fatalf("publish body = %#v", body)
		}
		if body.Keep == nil || *body.Keep {
			t.Fatalf("publish keep = %v, want false: the daemon keeps its own notice queue", body.Keep)
		}
		var payload map[string]string
		if body.Payload == nil {
			t.Fatal("publish carried no payload")
		}
		if err := json.Unmarshal([]byte(*body.Payload), &payload); err != nil {
			t.Fatalf("decode publish payload: %v", err)
		}
		if payload["kind"] != "worker-died" {
			t.Fatalf("payload = %#v, want worker-died", payload)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if _, err := New(server.URL, "envoy-bearer").Publish(context.Background(), "notifications.legion.LEGION.LEGION-208", "worker-died on LEGION-208", map[string]string{"kind": "worker-died"}, "legion-outbox:42"); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// A role publish the listener took answers the session it stored the message for: the holder its
// 200 names (listenerwire.PublishResponse.Holder), which is the one evidence a sender has of who
// was told at the write (the listener's roleSend.delivered). An answer that names no holder, a
// 200 with no body, and one whose body is not the listener's JSON are a publish taken for a
// recipient nobody named: no error, and no holder.
func TestHTTPPublisherAnswersTheHolderTheListenerStoredTheMessageFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		holder string
	}{
		{"the listener names the holder", `{"event_id":"evt-1","topic":"notifications.role.legion-demo-tester","holder":"ses_fresh","holder_state":"live"}`, "ses_fresh"},
		{"the listener names no holder", `{"event_id":"evt-1","topic":"notifications.legion.LEGION.LEGION-208"}`, ""},
		{"a 200 with no body", ``, ""},
		{"a 200 whose body is not the listener's JSON", `stored`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			holder, err := New(server.URL, "").Publish(context.Background(), "notifications.role.legion-demo-tester", "hold on DEMO-1", map[string]string{"step": "hold"}, "legion-outbox:9")
			if err != nil || holder != tc.holder {
				t.Fatalf("publish answered by %q = holder %q, %v; want holder %q and no error", tc.body, holder, err, tc.holder)
			}
		})
	}
}

func TestHTTPPublisherRefusalNamesTheListenerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The listener writes the JetStream publish error as {"error": ...} with 500
		// (packages/envoy/cmd/listener/api.go publishHandler); the log line is the only place it surfaces.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"nats: no response from stream"}`))
	}))
	defer server.Close()

	_, err := New(server.URL, "").Publish(context.Background(), "notifications.legion.LEGION.LEGION-208", "pr-blocked on LEGION-208", map[string]string{"kind": "pr-blocked"}, "legion-outbox:7")
	if err == nil || !strings.Contains(err.Error(), "nats: no response from stream") {
		t.Fatalf("publish error = %v, want the listener's error body", err)
	}
}

// A role publish no session can take is refused 404 with the listener's reason
// (packages/envoy/cmd/listener/api.go writeRoleHolderError): nobody claimed the role, or its
// holder's registration lapsed. That is the one refusal a caller holds a notice for, so it is told
// apart from every other failure; a 404 with no such reason (a route an older listener lacks) is
// not a missing holder, and stays an ordinary failure.
func TestHTTPPublisherTellsANoHolderRefusalFromAnyOther(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		noHolder bool
	}{
		{"nobody claimed the role", http.StatusNotFound, `{"error":"no holder for role legion-demo-controller","reason":"unclaimed"}`, true},
		{"the holder lapsed", http.StatusNotFound, `{"error":"role legion-demo-controller holder ses_a lapsed; claim retained (last seen 1)","reason":"holder_lapsed","holder":"ses_a","last_seen":1,"claim_released":false}`, true},
		{"a route the listener lacks", http.StatusNotFound, `{"error":"not found"}`, false},
		{"the stream is down", http.StatusInternalServerError, `{"error":"nats: no response from stream"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := New(server.URL, "").Publish(context.Background(), "notifications.role.legion-demo-controller", "held on DEMO-1", map[string]string{"kind": "held"}, "legion-outbox:9")
			if err == nil || !strings.Contains(err.Error(), tc.body) {
				t.Fatalf("publish error = %v, want the listener's refusal %s", err, tc.body)
			}
			if got := errors.Is(err, ErrNoHolder); got != tc.noHolder {
				t.Fatalf("errors.Is(%v, ErrNoHolder) = %t, want %t", err, got, tc.noHolder)
			}
		})
	}
}

// A kept publish asks the listener to keep a role message no live session holds the role for
// (keep: true), and the listener's answers are told apart by status alone: 200, a live holder
// took it; 202 kept, for a claim whose holder is not live (holder_lapsed) or a role nobody claims
// (unclaimed), the listener of cybermonklabs/legion#4079 keeping the copy on the role's kept lane
// for an hour; and the 404 holder_lapsed a listener before #4079 answers a claim whose holder is
// not live, keeping nothing, which is ErrNoHolder for a holder that is gone and
// ErrHolderUnresponsive for one that is unresponsive. A 202's body is not read, so the envelope a
// kept answer carries, however long, changes nothing.
func TestHTTPPublisherPublishKeptTellsAKeptMessageFromADeliveredOne(t *testing.T) {
	long := strings.Repeat("x", refusalBodyLimit+1)
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		kept         bool
		noHolder     bool
		unresponsive bool
		refused      bool
	}{
		{"a live holder took it", http.StatusOK,
			`{"event_id":"evt-1","topic":"notifications.role.legion-demo-implementer","holder":"ses_a","holder_state":"live"}`, false, false, false, false},
		{"kept for a holder that lapsed", http.StatusAccepted,
			`{"event_id":"evt-2","topic":"notifications.role.legion-demo-implementer","holder":"ses_a","holder_state":"gone","kept":true,"kept_reason":"holder_lapsed","kept_until":1900000000000,"last_seen":1}`, true, false, false, false},
		{"kept for a role nobody claims", http.StatusAccepted,
			`{"event_id":"evt-3","topic":"notifications.role.legion-demo-implementer","kept":true,"kept_reason":"unclaimed","kept_until":1900000000000}`, true, false, false, false},
		{"kept with an envelope past the refusal bound", http.StatusAccepted,
			`{"event_id":"evt-4","message":"` + long + `","kept":true,"kept_reason":"unclaimed","kept_until":1900000000000}`, true, false, false, false},
		{"a listener before #4079 keeps nothing for a holder that is gone", http.StatusNotFound,
			`{"error":"role legion-demo-implementer holder ses_a lapsed","reason":"holder_lapsed","holder":"ses_a","holder_state":"gone","last_seen":1,"claim_released":false}`, false, true, false, true},
		{"a listener before #4079 keeps nothing for a holder that is unresponsive", http.StatusNotFound,
			`{"error":"role legion-demo-implementer holder ses_a lapsed; claim retained (last seen 1)","reason":"holder_lapsed","holder":"ses_a","holder_state":"unresponsive","last_seen":1,"claim_released":false}`, false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Topic     string `json:"topic"`
					Message   string `json:"message"`
					DedupeKey string `json:"dedupe_key"`
					Keep      *bool  `json:"keep"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if body.Topic != "notifications.role.legion-demo-implementer" || body.Message != "the end of DEMO-1's testing phase" || body.DedupeKey != "legion-outbox:11" {
					t.Errorf("kept publish body = %#v", body)
				}
				if body.Keep == nil || !*body.Keep {
					t.Errorf("kept publish keep = %v, want true: the listener is to keep the message for the role's next holder", body.Keep)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			kept, err := New(server.URL, "").PublishKept(context.Background(), "notifications.role.legion-demo-implementer",
				"the end of DEMO-1's testing phase", "the end of DEMO-1's testing phase", "legion-outbox:11")
			if kept != tc.kept {
				t.Fatalf("kept publish kept = %t with %v, want %t", kept, err, tc.kept)
			}
			if (err != nil) != tc.refused {
				t.Fatalf("kept publish error = %v, want an error %t", err, tc.refused)
			}
			if tc.refused && !strings.Contains(err.Error(), tc.body) {
				t.Fatalf("kept publish error = %v, want the listener's answer %s", err, tc.body)
			}
			if got := errors.Is(err, ErrNoHolder); got != tc.noHolder {
				t.Fatalf("errors.Is(%v, ErrNoHolder) = %t, want %t", err, got, tc.noHolder)
			}
			if got := errors.Is(err, ErrHolderUnresponsive); got != tc.unresponsive {
				t.Fatalf("errors.Is(%v, ErrHolderUnresponsive) = %t, want %t", err, got, tc.unresponsive)
			}
		})
	}
}

// A listener that carries the session's state in its refusal tells an unresponsive holder from a
// dead one (dispatch://INFRA-392): `holder_lapsed` with `holder_state` `unresponsive` is a session
// past its live window but inside its gone horizon, which still holds the role, so the publish is
// held and sent again (ErrHolderUnresponsive) rather than read as a role nobody holds. A holder
// already `gone`, a role nobody claimed, and a `holder_lapsed` from a listener that names no state
// at all are each no holder.
func TestHTTPPublisherTellsAnUnresponsiveHolderFromNoHolder(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want error
	}{
		{"the holder is unresponsive", `{"error":"role legion-demo-controller holder ses_a lapsed; claim retained (last seen 1)","reason":"holder_lapsed","holder":"ses_a","holder_state":"unresponsive","last_seen":1,"claim_released":false}`, ErrHolderUnresponsive},
		{"the holder is gone", `{"error":"role legion-demo-controller holder ses_a lapsed","reason":"holder_lapsed","holder":"ses_a","holder_state":"gone","last_seen":1,"claim_released":false}`, ErrNoHolder},
		{"nobody claimed the role", `{"error":"no holder for role legion-demo-controller","reason":"unclaimed","holder_state":"gone"}`, ErrNoHolder},
		{"an older listener names no state", `{"error":"no live holder for role legion-demo-controller","reason":"holder_lapsed","holder":"ses_a"}`, ErrNoHolder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := New(server.URL, "").Publish(context.Background(), "notifications.role.legion-demo-controller", "held on DEMO-1", map[string]string{"kind": "held"}, "legion-outbox:9")
			if !errors.Is(err, tc.want) {
				t.Fatalf("publish error = %v, want %v", err, tc.want)
			}
			if other := ErrNoHolder; tc.want == ErrHolderUnresponsive && errors.Is(err, other) {
				t.Fatalf("publish error = %v, want an unresponsive holder told apart from no holder", err)
			}
		})
	}
}

// A listener that confirms a role message before it answers says why it did not deliver one:
// 504 with receipt_timeout or delivery_failed, naming the holder and the envelope it forwarded
// (listenerwire.RoleSendFailure), 403 with capability when the holder cannot take the payload's
// delivery mode (listenerwire.CapabilityRefusal), or 403 with no reason when
// the NATS server denied the forward under the listener's own grant (writeBusUnavailable,
// packages/envoy/cmd/listener/api.go). Each is told apart from every other failure, and a 504 or
// 403 whose body is not that JSON — a proxy's gateway timeout, a reason this daemon does not
// know — stays an ordinary failure.
func TestHTTPPublisherTellsTheConfirmedSendAnswersApart(t *testing.T) {
	// The words each refusal is wanted to hold as they came: the holder and event id of an
	// ErrNotDelivered, the holder of an ErrRefusedByHolder, the message of an ErrPublishDenied.
	type undelivered struct{ reason, holder, eventID string }
	held := func(s string) *string { return &s }
	const deniedWords = `envoy: the message bus denied the publish under this listener's own permissions: bus: NATS denied the publish to "notifications.agent.ses_a": nats: permissions violation: Permissions Violation for Publish to "notifications.agent.ses_a"`
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		notDelivered *undelivered
		refused      *string
		denied       *string
	}{
		{"the holder sent no receipt", http.StatusGatewayTimeout,
			`{"error":"role legion-demo-architect: event evt-1 was not confirmed by its holder within 4s (receipt_timeout)","reason":"receipt_timeout","event_id":"evt-1","dedupe_key":"legion-outbox:9","holder":"ses_a","attempts":2}`,
			&undelivered{ReasonReceiptTimeout, "ses_a", "evt-1"}, nil, nil},
		{"the forward never reached the server", http.StatusGatewayTimeout,
			`{"error":"role legion-demo-architect: event evt-2 was not confirmed by its holder within 4s (delivery_failed)","reason":"delivery_failed","event_id":"evt-2","dedupe_key":"legion-outbox:9","holder":"","attempts":0}`,
			&undelivered{ReasonDeliveryFailed, "", "evt-2"}, nil, nil},
		{"the holder does not advertise the delivery mode", http.StatusForbidden,
			`{"error":"session ses_a (architect) does not advertise prompt","reason":"capability","holder":"ses_a"}`, nil, held("ses_a"), nil},
		{"the listener's own grant denied the forward", http.StatusForbidden,
			`{"error":"envoy: the message bus denied the publish under this listener's own permissions: bus: NATS denied the publish to \"notifications.agent.ses_a\": nats: permissions violation: Permissions Violation for Publish to \"notifications.agent.ses_a\""}`,
			nil, nil, held(deniedWords)},
		{"a gateway timeout that is not JSON", http.StatusGatewayTimeout, `upstream request timeout`, nil, nil, nil},
		{"a gateway timeout with no reason", http.StatusGatewayTimeout, `{"error":"gateway timeout"}`, nil, nil, nil},
		{"a gateway timeout with another reason", http.StatusGatewayTimeout, `{"error":"not confirmed","reason":"not_confirmed"}`, nil, nil, nil},
		{"a forbidden answer the listener does not write", http.StatusForbidden, `{"error":"forbidden"}`, nil, nil, nil},
		{"a forbidden answer that is not JSON", http.StatusForbidden, `forbidden`, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := New(server.URL, "").Publish(context.Background(), "notifications.role.legion-demo-architect", "held on DEMO-1", map[string]string{"kind": "held"}, "legion-outbox:9")
			if err == nil || !strings.Contains(err.Error(), tc.body) {
				t.Fatalf("publish error = %v, want the listener's answer %s", err, tc.body)
			}
			var notDelivered ErrNotDelivered
			if got := errors.As(err, &notDelivered); got != (tc.notDelivered != nil) || got && (notDelivered.Reason != tc.notDelivered.reason ||
				!notDelivered.Holder.Equal(tc.notDelivered.holder) || !notDelivered.EventID.Equal(tc.notDelivered.eventID)) {
				t.Fatalf("errors.As(%v, ErrNotDelivered) = %t with %+v, want %+v", err, got, notDelivered, tc.notDelivered)
			}
			var refused ErrRefusedByHolder
			if got := errors.As(err, &refused); got != (tc.refused != nil) || got && !refused.Holder.Equal(*tc.refused) {
				t.Fatalf("errors.As(%v, ErrRefusedByHolder) = %t with %+v, want %v", err, got, refused, tc.refused)
			}
			var denied ErrPublishDenied
			if got := errors.As(err, &denied); got != (tc.denied != nil) || got && !denied.Message.Equal(*tc.denied) {
				t.Fatalf("errors.As(%v, ErrPublishDenied) = %t with %+v, want %v", err, got, denied, tc.denied)
			}
			if errors.Is(err, ErrNoHolder) {
				t.Fatalf("errors.Is(%v, ErrNoHolder) = true, want a holder that exists", err)
			}
		})
	}
}
