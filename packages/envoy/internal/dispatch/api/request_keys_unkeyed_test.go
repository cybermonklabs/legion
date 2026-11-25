package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// flakyIdentity fails its next `failures` lookups with a 503, as an identity read that met a
// database restart does, then answers as inner does.
type flakyIdentity struct {
	inner    identity.Identity
	failures atomic.Int32
}

func (f *flakyIdentity) Login(r *http.Request) (string, error) {
	if f.failures.Add(-1) >= 0 {
		return "", errorf(http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "the identity lookup failed for a moment")
	}
	return f.inner.Login(r)
}

// writeRequest is a write of body to thing as login, under key when key is not empty, its length
// declared or, with undeclared, left for the server to find as it reads.
func writeRequest(t *testing.T, thing string, body []byte, login, key string, undeclared bool) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/test/"+thing+"/writes", bytes.NewReader(body))
	if undeclared {
		request.ContentLength = -1
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", login)
	if key != "" {
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
	}
	return request
}

// lookupRig is one write route behind the keyed layer whose handler resolves its caller and
// stores only once it has one, as a route's handler does, served by an identity whose next
// lookups fail while told to (flakyIdentity).
type lookupRig struct {
	*keyedRig
	flaky  *flakyIdentity
	stored atomic.Int32
}

func newLookupRig(t *testing.T) *lookupRig {
	t.Helper()
	rig := &lookupRig{}
	rig.keyedRig = newKeyedRig(t, rig.write)
	rig.flaky = &flakyIdentity{inner: rig.server.deps.Identity}
	rig.server.deps.Identity = rig.flaky
	return rig
}

func (rig *lookupRig) write(_ int32, w http.ResponseWriter, r *http.Request) {
	if _, _, err := rig.server.optionalActor(r); err != nil {
		rig.server.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]int32{"stored": rig.stored.Add(1)})
}

// Once a write carries an Idempotency-Key, no path of the keyed layer runs its handler without the
// key. The census of the paths that ran a handler with a key present and unused, each closed and
// each a row below:
//
//   - the caller lookup failed (optionalActor's error): the handler ran with no key, and one that
//     resolved the caller again with a lookup that now succeeded made the write, so its resend
//     under the key made it a second time. The layer now answers the refusal itself.
//   - the declared body was past the route's limit (ContentLength): the layer answers the route's
//     413 itself.
//   - a body that declared no length ran past the limit as it was spooled (body.size): the same.
//   - every POST and PATCH row of the table, each a row of the walk: whether its chain held a
//     handler back from a failed lookup was up to the layer that looked first. A row that names
//     OAuth scopes was held back by its scope gate (gateRouteScopes), which resolves the caller
//     ahead of the keyed layer and refuses one it cannot; every other row ran its handler and left
//     the refusal to it, and a handler that never looks up its caller would have written with no
//     key. The layer's own refusal now holds every row back, whatever its handler does.
//
// A write with no key runs unkeyed by design and is no path of this census. Each row's control is
// a subtest of its own, so a row that fails still runs it.
func TestAKeyedWriteNeverRunsUnkeyed(t *testing.T) {
	t.Parallel()
	t.Run("the caller lookup fails for a passing reason", func(t *testing.T) {
		t.Run("control", func(t *testing.T) {
			// The fallback the layer took before: its lookup fails, and the handler, run with no
			// key, resolves the caller again and stores the write.
			rig := newLookupRig(t)
			body, err := json.Marshal(map[string]any{"n": 2})
			if err != nil {
				t.Fatal(err)
			}
			request := writeRequest(t, "b", body, "alice", "", false)
			rig.flaky.failures.Store(1)
			if _, _, err := rig.server.optionalActor(request); err == nil {
				t.Fatal("the layer's lookup should have failed")
			}
			response := httptest.NewRecorder()
			rig.write(0, response, request)
			if response.Code != http.StatusCreated || rig.stored.Load() != 1 {
				t.Fatalf("status=%d stored=%d, want the unkeyed run to store", response.Code, rig.stored.Load())
			}
		})

		rig := newLookupRig(t)
		rig.flaky.failures.Store(1)
		first := rig.send(t, "a", map[string]any{"n": 1}, "alice", "lookup-1")
		if first.Code != http.StatusServiceUnavailable || rig.stored.Load() != 0 {
			t.Fatalf("first: status=%d stored=%d body=%s, want 503 and nothing stored", first.Code, rig.stored.Load(), first.Body.String())
		}
		resend := rig.send(t, "a", map[string]any{"n": 1}, "alice", "lookup-1")
		if resend.Code != http.StatusCreated || replayed(resend) || rig.stored.Load() != 1 {
			t.Fatalf("resend: status=%d replayed=%v stored=%d body=%s, want 201 run once", resend.Code, replayed(resend), rig.stored.Load(), resend.Body.String())
		}
		again := rig.send(t, "a", map[string]any{"n": 1}, "alice", "lookup-1")
		if again.Code != http.StatusCreated || !replayed(again) || rig.stored.Load() != 1 {
			t.Fatalf("again: status=%d replayed=%v stored=%d, want the stored answer replayed", again.Code, replayed(again), rig.stored.Load())
		}
	})

	for _, undeclared := range []bool{false, true} {
		name := "the declared body is past the route's limit"
		if undeclared {
			name = "a body that declared no length runs past the route's limit"
		}
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(strings.Repeat("x", int(maxJSONRequestBytes)+1))
			if err != nil {
				t.Fatal(err)
			}
			t.Run("control", func(t *testing.T) {
				// The same write with no key runs its handler, as the layer used to run it.
				rig := newKeyedRig(t, answerRun)
				response := httptest.NewRecorder()
				rig.handler.ServeHTTP(response, writeRequest(t, "a", body, "alice", "", undeclared))
				if rig.runs.Load() != 1 {
					t.Fatalf("handler runs=%d status=%d, want the unkeyed write to run", rig.runs.Load(), response.Code)
				}
			})

			rig := newKeyedRig(t, answerRun)
			response := httptest.NewRecorder()
			rig.handler.ServeHTTP(response, writeRequest(t, "a", body, "alice", "big-1", undeclared))
			if response.Code != http.StatusRequestEntityTooLarge || rig.runs.Load() != 0 {
				t.Fatalf("keyed: status=%d handler runs=%d body=%s, want 413 and no run", response.Code, rig.runs.Load(), response.Body.String())
			}
		})
	}

	// Every POST and PATCH row of the table, each through the chain register mounts it behind
	// (routeHandler, under the admission and the read deadline register gives a server whose Deps
	// set neither), with its handler replaced by a recorder that resolves no caller, as the
	// read-only walk builds them (read_only_token_test.go), each row the registered server's own so
	// its keyed replay (apiRoute.Replay) is that server's: a keyed write whose caller lookup fails
	// runs no handler, whichever layer of the row looks first, and its resend under the key, once
	// the lookup works, runs it once. Each row's control is the fallback the layer took before: the
	// write handed on with no key, which a row that names OAuth scopes holds back at its scope gate
	// (gateRouteScopes) and every other row runs. The walk's chain is one it builds, so each row is
	// also sent through the server's own mux with a malformed key, which only the keyed layer
	// answers (400 KEY_INVALID, before any handler runs): a row register mounted without that layer
	// fails here. That every write the server mounts is a row of this table is the routes package's
	// TestAKeyedWriteNeverRunsUnkeyed.
	t.Run("the caller lookup fails on a write route of the server", func(t *testing.T) {
		handler, _, _, served := newRegisteredTestServer(t, testServerOptions{testHooks: true})
		// TEST-1, open, is the issue every probe path names, so the issue upload's check of its
		// issue (refuseUploadToAClosedIssue) lets the probe through to the keyed layer.
		createArtifactIssue(t, handler)
		flaky := &flakyIdentity{inner: served.deps.Identity}
		served.deps.Identity = flaky
		limiter := admission.New(admission.DefaultLimits())
		t.Run("control", func(t *testing.T) {
			// A write row mounted with its handler bare, as register would mount one it forgot to
			// wrap, answers the malformed key with its handler's own answer, not the keyed layer's.
			bare := http.NewServeMux()
			bare.HandleFunc("POST /api/v1/issues/{key}/comments", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/TEST-1/comments", strings.NewReader(`{}`))
			request.Header.Set(contracts.IdempotencyKeyHeader, "not a key")
			answer := httptest.NewRecorder()
			bare.ServeHTTP(answer, request)
			if answer.Code == http.StatusBadRequest || strings.Contains(answer.Body.String(), contracts.IdempotencyKeyInvalid) {
				t.Fatalf("a bare mount answered status=%d body=%s, the keyed layer's refusal", answer.Code, answer.Body.String())
			}
		})
		walked, handedOn := 0, 0
		for row, route := range served.routes() {
			if !keyedMethod(route.Method) {
				continue
			}
			name := route.Method + " " + route.Pattern
			walked++
			var ran atomic.Int32
			route.Handler = func(w http.ResponseWriter, _ *http.Request) {
				ran.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}
			// On a mux of its own, as register mounts it, so the path's values reach every layer.
			alone := http.NewServeMux()
			alone.HandleFunc(name, served.routeHandler(route, limiter, defaultReadDeadline))
			// send sends the row's probe under key, none when key is empty, with every caller
			// lookup failing while failing is set.
			send := func(key string, failing bool) *httptest.ResponseRecorder {
				request := httptest.NewRequest(route.Method, routeProbePath(route.Pattern), strings.NewReader(routeProbeBody(route)))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Dispatch-User", "alice")
				if key != "" {
					request.Header.Set(contracts.IdempotencyKeyHeader, key)
				}
				if failing {
					flaky.failures.Store(1 << 30)
					defer flaky.failures.Store(0)
				}
				response := httptest.NewRecorder()
				alone.ServeHTTP(response, request)
				return response
			}
			t.Run(name, func(t *testing.T) {
				t.Run("control", func(t *testing.T) {
					want := int32(1)
					if len(route.OAuthScopes) > 0 {
						want = 0
					} else {
						handedOn++
					}
					before := ran.Load()
					if response := send("", true); ran.Load()-before != want {
						t.Fatalf("handed on with no key: handler runs=%d status=%d body=%s, want %d", ran.Load()-before, response.Code, response.Body.String(), want)
					}
				})

				key := fmt.Sprintf("row-%d", row)
				before := ran.Load()
				if refused := send(key, true); ran.Load() != before || refused.Code != http.StatusServiceUnavailable {
					t.Fatalf("lookup failed: handler runs=%d status=%d body=%s, want the lookup's 503 and no run",
						ran.Load()-before, refused.Code, refused.Body.String())
				}
				if response := send(key, false); ran.Load() != before+1 {
					t.Fatalf("resend: handler runs=%d status=%d body=%s, want one", ran.Load()-before, response.Code, response.Body.String())
				}

				mounted := httptest.NewRequest(route.Method, routeProbePath(route.Pattern), strings.NewReader(routeProbeBody(route)))
				mounted.Header.Set("Content-Type", "application/json")
				mounted.Header.Set("X-Dispatch-User", "alice")
				mounted.Header.Set(contracts.IdempotencyKeyHeader, "not a key")
				answer := httptest.NewRecorder()
				handler.ServeHTTP(answer, mounted)
				if answer.Code != http.StatusBadRequest || !strings.Contains(answer.Body.String(), contracts.IdempotencyKeyInvalid) {
					t.Fatalf("through the server's own mux: status=%d body=%s, want the keyed layer's 400 %s",
						answer.Code, answer.Body.String(), contracts.IdempotencyKeyInvalid)
				}
			})
		}
		// The count is held to the rows the registered server mounts (its route index), as the
		// read-only walk's is, so a walk that skipped a row, or walked none, fails.
		mounted := 0
		for _, entry := range served.routeIndex {
			if keyedMethod(entry.Method) {
				mounted++
			}
		}
		if walked == 0 || walked != mounted || handedOn == 0 {
			t.Fatalf("walked %d write rows (%d handed on with no key), want every one of the %d the server mounts and some handed on", walked, handedOn, mounted)
		}
		t.Logf("walked %d POST and PATCH rows of the table (test hooks in): %d ran a write handed on with no key, %d were held by their scope gate",
			walked, handedOn, walked-handedOn)
	})
}

// A keyed write the server has no room for is refused at admission, 429 OVERLOADED with
// Retry-After, before the keyed layer looks its caller up: admission is outside the keyed layer
// in every row's chain (routeHandler), so a server over its bound neither reads the request pool
// for a caller it would refuse nor answers that caller's 401 or 503 where a client is owed the
// refusal it retries after. Each caller's control is the same write with room, which the keyed
// layer answers with the caller's own refusal after one lookup, so the row tells the two orders
// apart: a chain that looked the caller up ahead of admission answers the full server's write
// as the control is answered, or counts a lookup.
func TestAKeyedWriteTheServerHasNoRoomForIsRefusedBeforeItsCallerIsLookedUp(t *testing.T) {
	t.Parallel()
	limiter := admission.New(admission.Limits{
		Read: 4, Write: 1, ReadWait: 50 * time.Millisecond, WriteWait: 50 * time.Millisecond,
	})
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{admission: limiter})
	// TEST-1, open, so nothing the route checks before the keyed layer refuses the write first.
	createArtifactIssue(t, handler)
	flaky := &flakyIdentity{inner: served.deps.Identity}
	served.deps.Identity = flaky
	for _, caller := range []struct {
		name    string
		login   string
		failing bool
		// roomy is what the keyed layer answers the caller where the server has room.
		roomy int
	}{
		{name: "a caller that resolves to nobody", roomy: http.StatusUnauthorized},
		{name: "a caller whose lookup fails", login: "alice", failing: true, roomy: http.StatusServiceUnavailable},
	} {
		// send sends the caller's keyed comment and answers how many identity lookups it made.
		send := func() (*httptest.ResponseRecorder, int32) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/TEST-1/comments", strings.NewReader(`{"body":"no room"}`))
			request.Header.Set("Content-Type", "application/json")
			if caller.login != "" {
				request.Header.Set("X-Dispatch-User", caller.login)
			}
			request.Header.Set(contracts.IdempotencyKeyHeader, "no-room-1")
			flaky.failures.Store(0)
			if caller.failing {
				flaky.failures.Store(1 << 30)
			}
			before := flaky.failures.Load()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			return response, before - flaky.failures.Load()
		}
		t.Run(caller.name, func(t *testing.T) {
			t.Run("control", func(t *testing.T) {
				if response, lookups := send(); response.Code != caller.roomy || lookups == 0 {
					t.Fatalf("with room: status=%d lookups=%d body=%s, want the keyed layer's %d after a lookup",
						response.Code, lookups, response.Body.String(), caller.roomy)
				}
			})

			held, err := limiter.Admit(context.Background(), admission.Write, 1)
			if err != nil {
				t.Fatalf("hold the write class's one unit: %v", err)
			}
			defer held.Release()
			response, lookups := send()
			if response.Code != http.StatusTooManyRequests || refusalCode(response.Body.Bytes()) != admission.CodeOverloaded {
				t.Fatalf("no room: status=%d body=%s, want admission's 429 %s", response.Code, response.Body.String(), admission.CodeOverloaded)
			}
			if retryAfter := response.Header().Get("Retry-After"); retryAfter != admission.RetryAfter {
				t.Fatalf("no room: Retry-After = %q, want %s", retryAfter, admission.RetryAfter)
			}
			if lookups != 0 {
				t.Fatalf("no room: the refused write looked its caller up %d times, want none before admission", lookups)
			}
		})
	}
}
