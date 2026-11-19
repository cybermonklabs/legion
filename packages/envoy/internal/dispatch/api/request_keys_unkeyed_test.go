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

	"github.com/sjawhar/envoy/internal/contracts"
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
		rig.server.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]int32{"stored": rig.stored.Add(1)})
}

// Once a write carries an Idempotency-Key, nothing it makes lands without the key. The census of
// the paths that hand a keyed request to its handler, or answer it, with no key claimed, each a row
// below:
//
//   - the caller lookup failed (optionalActor's error): the handler ran with no key, and one that
//     resolved the caller again with a lookup that now succeeded made the write, so its resend
//     under the key made it a second time. The handler now meets the layer's refusal at its own
//     lookup, and a transaction it opens that wrote is refused at its COMMIT (writeless).
//   - the declared body was past the route's limit (ContentLength): the layer answers the route's
//     413 itself, and no handler runs.
//   - a body that declared no length ran past the limit as it was spooled (body.size): the handler
//     ran as its caller, and the routes that read no body - the revokes, the comment reopen, the
//     architecture syncs - made their writes with no key claimed. The handler now meets the route's
//     413 at its caller lookup, writeless too (TestAnUndeclaredBodyPastTheLimitReachesNoWrite).
//   - every POST and PATCH row of the table, each a row of the walk, with its handler replaced by
//     one that never looks up its caller and writes: whichever layer of the row looks first, the
//     write does not land.
//
// A write with no key runs unkeyed by design and is no path of this census. Each row's control is
// a subtest of its own, so a row that fails still runs it.
func TestAKeyedWriteNeverWritesUnkeyed(t *testing.T) {
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
			// A handler that never looks up its caller and writes a row, as no route's handler
			// does: what reaches it, and what the second guard (writeless's COMMIT) lets land.
			writing := func(t *testing.T) *keyedRig {
				return newWritingRig(t, func(_ int32, w http.ResponseWriter, r *http.Request, write rigWrite) {
					if err := write(r.Context(), nil); err != nil {
						writeError(w, "WRITE_REFUSED", http.StatusConflict, err.Error())
						return
					}
					w.WriteHeader(http.StatusCreated)
				})
			}
			t.Run("control", func(t *testing.T) {
				// The same write with no key runs its handler, which writes, as the layer used to
				// run it with the key.
				rig := writing(t)
				response := httptest.NewRecorder()
				rig.handler.ServeHTTP(response, writeRequest(t, "a", body, "alice", "", undeclared))
				if rig.runs.Load() != 1 || len(rig.writes(t)) != 1 {
					t.Fatalf("handler runs=%d writes=%v status=%d, want the unkeyed write to run and land", rig.runs.Load(), rig.writes(t), response.Code)
				}
			})

			rig := writing(t)
			response := httptest.NewRecorder()
			rig.handler.ServeHTTP(response, writeRequest(t, "a", body, "alice", "big-1", undeclared))
			if writes, keys := rig.writes(t), rig.rows(t); len(writes) != 0 || keys != 0 {
				t.Fatalf("keyed: status=%d handler runs=%d writes=%v key rows=%d body=%s, want nothing landed and no key kept",
					response.Code, rig.runs.Load(), writes, keys, response.Body.String())
			}
			if !undeclared && (response.Code != http.StatusRequestEntityTooLarge || rig.runs.Load() != 0) {
				t.Fatalf("declared: status=%d handler runs=%d body=%s, want the layer's 413 and no run", response.Code, rig.runs.Load(), response.Body.String())
			}
		})
	}

	// Every POST and PATCH row of the table, each through the chain register mounts it behind
	// (routeHandler), with its handler replaced by one that never looks up its caller and writes a
	// row, as no route's handler does: a keyed write whose caller lookup fails lands nothing,
	// whichever layer of the row looks first, and its resend under the key, once the lookup works,
	// lands once. Each row's control is the same write with no key, which lands where the layers
	// ahead of the handler let it run: every row but those that name OAuth scopes, which their
	// scope gate (gateRouteScopes) holds back. The walk's chain is one it builds, so each row is
	// also sent through the server's own mux with a malformed key, which only the keyed layer
	// answers (400 KEY_INVALID, before any handler runs): a row register mounted without that layer
	// fails here. That every write the server mounts is a row of this table is the routes package's
	// TestAKeyedWriteNeverRunsUnkeyed.
	t.Run("the caller lookup fails on a write route of the server", func(t *testing.T) {
		handler, database, _, served := newRegisteredTestServer(t, testServerOptions{testHooks: true})
		// TEST-1, open, is the issue every probe path names, so the issue upload's check of its
		// issue (refuseUploadToAClosedIssue) lets the probe through to the keyed layer.
		createArtifactIssue(t, handler)
		flaky := &flakyIdentity{inner: served.deps.Identity}
		served.deps.Identity = flaky
		replays := served.keyedReplays()
		if _, err := database.Pool.Exec(context.Background(), `create table unkeyed_walk_writes (route text not null)`); err != nil {
			t.Fatalf("create the walk's table: %v", err)
		}
		landed := func(name string) int {
			t.Helper()
			var count int
			if err := database.Pool.QueryRow(context.Background(),
				`select count(*) from unkeyed_walk_writes where route = $1`, name).Scan(&count); err != nil {
				t.Fatalf("count the walk's writes: %v", err)
			}
			return count
		}
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
		for row, route := range readOnlyTable() {
			if !keyedMethod(route.Method) {
				continue
			}
			name := route.Method + " " + route.Pattern
			walked++
			route.Handler = func(w http.ResponseWriter, r *http.Request) {
				tx, err := database.Pool.Begin(r.Context())
				if err == nil {
					defer tx.Rollback(r.Context())
					if _, err = tx.Exec(r.Context(), `insert into unkeyed_walk_writes (route) values ($1)`, name); err == nil {
						err = tx.Commit(r.Context())
					}
				}
				if err != nil {
					served.writeHandlerError(w, err)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}
			// On a mux of its own, as register mounts it, so the path's values reach every layer.
			alone := http.NewServeMux()
			alone.HandleFunc(name, served.routeHandler(route, replays[name]))
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
					want := 1
					if len(route.OAuthScopes) > 0 {
						want = 0
					} else {
						handedOn++
					}
					before := landed(name)
					if response := send("", true); landed(name)-before != want {
						t.Fatalf("handed on with no key: writes landed=%d status=%d body=%s, want %d", landed(name)-before, response.Code, response.Body.String(), want)
					}
				})

				key := fmt.Sprintf("row-%d", row)
				before := landed(name)
				if refused := send(key, true); landed(name) != before || refused.Code != http.StatusServiceUnavailable {
					t.Fatalf("lookup failed: writes landed=%d status=%d body=%s, want the lookup's 503 and nothing landed",
						landed(name)-before, refused.Code, refused.Body.String())
				}
				if response := send(key, false); landed(name) != before+1 {
					t.Fatalf("resend: writes landed=%d status=%d body=%s, want one", landed(name)-before, response.Code, response.Body.String())
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
		t.Logf("walked %d POST and PATCH rows of the table (test hooks in): %d wrote when handed on with no key, %d were held by their scope gate",
			walked, handedOn, walked-handedOn)
	})
}
