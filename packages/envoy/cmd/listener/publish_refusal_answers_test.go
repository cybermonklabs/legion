package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// stalledRoleKV is a role bucket whose claim read does not answer: a Get blocks until the test
// ends, the way a legacy nats.go KV call waits out the JetStream MaxWait on a bus that has stopped
// answering, and a read of the bucket stream's leader (bus.LeaderReads, which the publish route's
// resolution makes) blocks until its context ends, as the stream message get it stands for does.
// Every other call goes to the real bucket. So only the route's own budget can end a request that
// reads the claim: a route bounded by its budget answers once that budget is spent, and one that
// waited the call out would not answer before the test ends.
type stalledRoleKV struct {
	nats.KeyValue
	stallGet bool
	released <-chan struct{}
}

func (kv stalledRoleKV) Get(key string) (nats.KeyValueEntry, error) {
	if kv.stallGet {
		<-kv.released
		return nil, errors.New("the role bucket's read never answered")
	}
	return kv.KeyValue.Get(key)
}

func (kv stalledRoleKV) NewestFromLeader(ctx context.Context, key string) (nats.KeyValueEntry, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-kv.released:
		return nil, errors.New("the role bucket's leader read never answered")
	}
}

// A role send through the publish route resolves its role's holder within sendRequestBudget of the
// request's own context, so a claim read that does not answer - the read of the role bucket
// stream's leader, which takes that context and stalls here until it ends - is reported inside the
// caller's attempt (the client gives each 5 s, packages/envoy-client/src/transport.ts) rather than
// after it: the bus-unavailable 503 with Retry-After: 1, naming the call that did not answer. The
// read never answers (stalledRoleKV), so an answer at all is the budget ending the request, and
// one before the budget was spent would mean something other than the stalled call ended it. The
// request carries the caller's attempt as its context (contracts.ListenerCallTimeout,
// envoy-client's own), and the answer must come while that
// attempt is still live: a route budgeted at the attempt or past it answers after its caller gave
// up. That is the one bound on time here, and it is the caller's, 1.5 s past the route's budget, not
// a margin of the test's. A lookup's same case is
// TestARoleLookupWhoseStoreDoesNotAnswerIsRefusedAsTheBusNotAnswering's.
func TestARoleResolutionWhoseStoreCallDoesNotAnswerAnswersInsideTheBudget(t *testing.T) {
	const (
		role   = "reviewer"
		holder = "ses_unanswered_holder"
	)
	// The budget ends the role read inside the send window, and the window ends inside one attempt
	// of the caller (contracts.ListenerCallTimeout, envoy-client's own), so the 503 below reaches a
	// caller whose attempt still waits for it.
	if !(sendRequestBudget < publishDeliveryWindow && publishDeliveryWindow < contracts.ListenerCallTimeout) {
		t.Fatalf("sendRequestBudget %s, publishDeliveryWindow %s, contracts.ListenerCallTimeout %s; want sendRequestBudget < publishDeliveryWindow < contracts.ListenerCallTimeout",
			sendRequestBudget, publishDeliveryWindow, contracts.ListenerCallTimeout)
	}
	topic := contracts.RoleTopicPrefix + role
	for _, tc := range []struct {
		name       string
		stallGet   bool
		unanswered string
	}{
		{name: "the claim read", stallGet: true, unanswered: "read role reviewer's claim: context deadline exceeded"},
	} {
		// open seeds a claim whose holder never registered, so a resolution reads the claim, finds
		// its holder gone and reports it lapsed - taking nothing from it - and then stalls the
		// role bucket's tc call.
		open := func(t *testing.T) *listenerDeps {
			t.Helper()
			client := setupPublishTestClient(t)
			registry, sessions := setupSessionsTest(t, nil, nil)
			if _, err := registry.SetRole(holder, "test-machine", role, false); err != nil {
				t.Fatalf("seed the role claim: %v", err)
			}
			released := make(chan struct{})
			t.Cleanup(func() { close(released) })
			setRegistryRoleKV(t, registry, stalledRoleKV{
				KeyValue: registryRoleKV(t, registry),
				stallGet: tc.stallGet,
				released: released,
			})
			return withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions})
		}
		t.Run("publish/"+tc.name, func(t *testing.T) {
			state := open(t)
			logs := &lockedBuffer{}
			recorder := httptest.NewRecorder()
			request := postJSON("/v1/messages/publish", sendBody(map[string]any{
				"topic":          topic,
				"source":         "agent",
				"source_session": "ses_unanswered_publisher",
				"message":        "is the holder there?",
			}))
			attempt, endAttempt := context.WithTimeout(context.Background(), contracts.ListenerCallTimeout)
			defer endAttempt()
			request = request.WithContext(attempt)
			answered := make(chan struct{})
			var attemptEnded bool
			started := time.Now()
			go func() {
				publishHandler(state, logging.NewWithWriter("test", logs)).ServeHTTP(recorder, request)
				attemptEnded = attempt.Err() != nil
				close(answered)
			}()
			testwait.Receive(t, "the publish route's answer once its budget was spent", answered)
			elapsed := time.Since(started)
			if elapsed < sendRequestBudget {
				t.Fatalf("the route answered %s after the request, before its budget (%s) was spent: something other than the stalled read ended it", elapsed, sendRequestBudget)
			}
			if attemptEnded {
				t.Fatalf("the route answered %s after the request, after the caller's attempt (contracts.ListenerCallTimeout, %s) had ended: the caller gave up before the 503 came", elapsed, contracts.ListenerCallTimeout)
			}
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("publish whose role bucket did not answer: status %d, body %s; want 503", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Retry-After"); got != "1" {
				t.Fatalf("Retry-After = %q, want 1", got)
			}
			var refusal listenerwire.APIError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) || !strings.Contains(refusal.Error, tc.unanswered) {
				t.Fatalf("the refusal reads %q, want it to begin %q and name %q", refusal.Error, busUnansweredPrefix, tc.unanswered)
			}
			assertRecordFields(t, soleRecord(t, logs.String()), map[string]any{
				"msg":     "listener publish",
				"level":   "WARN",
				"outcome": "bus_unavailable",
				"status":  float64(http.StatusServiceUnavailable),
				"topic":   topic,
			})
		})
	}
}

// A lookup whose role store does not answer inside its budget is a bus that did not answer, and
// GET /v1/roles/<role> answers it as every other route does (writeBusUnavailable): 503, Retry-After:
// 1, and a body that begins busUnansweredPrefix and names the call that did not answer. A 500 here
// reads as a broken listener rather than an unreachable bus, the distinction the spec gives
// bus-down its 503 for, so an operator's filter on that prefix finds this outage too; and the
// publish route already answers the same stalled store this way, in the test above. The bus is
// connected throughout, so nothing but the stalled role-bucket call can end the lookup. A lookup
// releases nothing, so the claim read is the only role-bucket call it makes. The read never
// answers, so the answer is the budget ending the lookup, and it must come while the caller's
// attempt, the request's context, is still live, as in the test above.
func TestARoleLookupWhoseStoreDoesNotAnswerIsRefusedAsTheBusNotAnswering(t *testing.T) {
	const (
		role   = "reviewer"
		holder = "ses_lookup_unanswered_holder"
	)
	// The budget the lookup's claim read runs under ends inside one attempt of the caller
	// (contracts.ListenerCallTimeout, envoy-client's own), so the 503 below reaches a caller whose
	// attempt still waits for it.
	if sendRequestBudget >= contracts.ListenerCallTimeout {
		t.Fatalf("sendRequestBudget %s, contracts.ListenerCallTimeout %s; want sendRequestBudget < contracts.ListenerCallTimeout", sendRequestBudget, contracts.ListenerCallTimeout)
	}
	for _, tc := range []struct {
		name       string
		stallGet   bool
		unanswered string
	}{
		{name: "the claim read", stallGet: true, unanswered: "read role reviewer's claim: context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := setupPublishTestClient(t)
			registry, sessions := setupSessionsTest(t, nil, nil)
			if _, err := registry.SetRole(holder, "test-machine", role, false); err != nil {
				t.Fatalf("seed the role claim: %v", err)
			}
			released := make(chan struct{})
			t.Cleanup(func() { close(released) })
			setRegistryRoleKV(t, registry, stalledRoleKV{
				KeyValue: registryRoleKV(t, registry),
				stallGet: tc.stallGet,
				released: released,
			})
			state := &listenerDeps{client: client, registry: registry, sessions: sessions}

			recorder := httptest.NewRecorder()
			attempt, endAttempt := context.WithTimeout(context.Background(), contracts.ListenerCallTimeout)
			defer endAttempt()
			request := httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil).WithContext(attempt)
			answered := make(chan struct{})
			var attemptEnded bool
			started := time.Now()
			go func() {
				roleGetHandler(state, logging.New("test")).ServeHTTP(recorder, request)
				attemptEnded = attempt.Err() != nil
				close(answered)
			}()
			testwait.Receive(t, "the lookup's answer once its budget was spent", answered)
			elapsed := time.Since(started)
			if elapsed < sendRequestBudget {
				t.Fatalf("the lookup answered %s after the request, before its budget (%s) was spent: something other than the stalled read ended it", elapsed, sendRequestBudget)
			}
			if attemptEnded {
				t.Fatalf("the lookup answered %s after the request, after the caller's attempt (contracts.ListenerCallTimeout, %s) had ended: the caller gave up before the 503 came", elapsed, contracts.ListenerCallTimeout)
			}
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("lookup whose role store did not answer: status %d, body %s; want 503 - the bus did not answer, the listener did not fail", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Retry-After"); got != "1" {
				t.Fatalf("Retry-After = %q, want 1", got)
			}
			var refusal listenerwire.APIError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) || !strings.Contains(refusal.Error, tc.unanswered) {
				t.Fatalf("the refusal reads %q, want it to begin %q and name %q", refusal.Error, busUnansweredPrefix, tc.unanswered)
			}
		})
	}
}

// A publish the server denies under this listener's own grant (bus.ErrPublishDenied) is answered
// as the permanent refusal it is: 403, no Retry-After, and a body naming the denial and the
// server's own violation text, so the client -- which retries every 5xx and no 4xx
// (packages/envoy-client/src/transport.ts) -- is not sent back to try something that can never
// succeed. The same classification is driven with a plain unreachable bus in the same test, so the
// two are separated rather than merely renamed: that one keeps the 503, the Retry-After and the
// bus_unavailable record. Both routes are driven, because refuseBusUnavailable is the one place
// either of them classifies a bus failure.
//
// What it does not prove: that the NATS server's denial arrives as bus.ErrPublishDenied. That is
// internal/bus/confirm.go's own ground, covered where an nkey-granted server can be started; here
// the error is built exactly as confirmPublished builds it and classified by the code the handlers
// call, so what is pinned is the answer, not the detection.
func TestADeniedPublishIsRefusedPermanentlyAndNotRecordedAsBusUnavailable(t *testing.T) {
	t.Parallel()
	subject := contracts.RoleTopicPrefix + "reviewer"
	// As internal/bus/confirm.go's confirmPublished names it: ErrPublishDenied around the subject
	// and the connection's last error, which is nats.go's permissions violation.
	denied := fmt.Errorf("bus: %w to %q: %w", bus.ErrPublishDenied, subject,
		fmt.Errorf("%w: Permissions Violation for Publish to %q", nats.ErrPermissionViolation, subject))
	unreachable := fmt.Errorf("bus: publish to %q: %w", subject, nats.ErrConnectionClosed)

	for _, route := range []struct {
		name    string
		line    string
		laneKey string
		lane    string
	}{
		{"publish", "listener publish", "topic", subject},
		{"send", "listener send", "target_session", "ses_denied_target"},
	} {
		for _, tc := range []struct {
			name        string
			err         error
			wantStatus  int
			wantRetry   string
			wantBody    string
			wantOutcome string
		}{
			{
				name:        "a publish the grant denies",
				err:         denied,
				wantStatus:  http.StatusForbidden,
				wantRetry:   "",
				wantBody:    publishDeniedPrefix + ": " + denied.Error(),
				wantOutcome: "publish_denied",
			},
			{
				name:        "a bus that did not answer",
				err:         unreachable,
				wantStatus:  http.StatusServiceUnavailable,
				wantRetry:   "1",
				wantBody:    busUnansweredPrefix + ": " + unreachable.Error(),
				wantOutcome: "bus_unavailable",
			},
		} {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				recorder, record := serveRecorded(t, func(logger *logging.Logger) http.HandlerFunc {
					return func(w http.ResponseWriter, r *http.Request) {
						rec := &requestRecord{logger: logger, line: route.line, laneKey: route.laneKey, lane: route.lane}
						rec.refuseBusUnavailable(w, tc.err)
					}
				}, postJSON("/v1/messages/publish", "{}"))
				if recorder.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.wantStatus, recorder.Body.String())
				}
				if got := recorder.Header().Get("Retry-After"); got != tc.wantRetry {
					t.Fatalf("Retry-After = %q, want %q", got, tc.wantRetry)
				}
				var refusal listenerwire.APIError
				if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
					t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
				}
				if refusal.Error != tc.wantBody {
					t.Fatalf("the refusal reads %q, want %q", refusal.Error, tc.wantBody)
				}
				assertRecordFields(t, record, map[string]any{
					"msg":         route.line,
					"level":       "WARN",
					"outcome":     tc.wantOutcome,
					"status":      float64(tc.wantStatus),
					route.laneKey: route.lane,
				})
			})
		}
	}
}
