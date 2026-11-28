// daemon_writes_test.go holds the daemon's gate (refuseNonDaemon) to every route whose
// declaration states it (routeDecl.DaemonWrites): a person's call is refused with the route's
// code, ahead of its handler, and the Legion daemon's passes the gate.
package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestADaemonRouteRefusesAPersonAheadOfItsHandler: each route of the table whose declaration
// states DaemonWrites, read from the table so a route that states it later is held here too, is
// built into the chain register mounts it behind (routeHandler) with its handler replaced by a
// stand-in, so only the gate can refuse. Its person's calls - a browser sign-in, a personal token
// holding person that names its own session, and a broker token acting for its person - are each
// answered 403 with the code the declaration's gate names (daemonWrite.code), naming the daemon's
// session, and the stand-in does not run; the daemon's calls run it once. That holds with
// DISPATCH_LEGION_STATUS_SCOPE off, where the daemon writes with the shared token naming its
// session or with its own broker token, and on, where it writes with its own broker token, which
// holds legion-status and proves its session.
func TestADaemonRouteRefusesAPersonAheadOfItsHandler(t *testing.T) {
	t.Parallel()
	for _, setting := range []struct {
		name string
		on   bool
	}{{"with DISPATCH_LEGION_STATUS_SCOPE off", false}, {"with DISPATCH_LEGION_STATUS_SCOPE on", true}} {
		t.Run(setting.name, func(t *testing.T) {
			t.Parallel()
			rig := newBrokerTokenRigOn(t, testServerOptions{legionStatusScope: setting.on})
			if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
				"key": brokerTestProject, "name": "Broker",
			}); created.Code != http.StatusCreated {
				t.Fatalf("create the project as a person: status=%d body=%s", created.Code, created.Body.String())
			}
			created := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": brokerTestProject, "title": "A run of steps", "force": true,
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("create an issue as a person: status=%d body=%s", created.Code, created.Body.String())
			}
			issue := decodeBody[model.Issue](t, created).Key

			caller := newBrokerCaller(t)
			personScoped := liveNamedToken(caller.jkt)
			personScoped.ProjectScope, personScoped.Projects = ciphertoken.ProjectScopePerson, nil
			rig.broker.Says("a-person-scoped-token", personScoped)
			rig.broker.Says("the-daemon-token", liveDaemonToken(caller.jkt))
			personal := personalTokenHolding(t, rig.database, model.ScopePerson)

			// credential is one caller of the walk: its name, and the request it sends a path.
			type credential struct {
				name string
				send func(t *testing.T, method, path string) *http.Request
			}
			persons := []credential{
				{"a browser sign-in", func(t *testing.T, method, path string) *http.Request {
					return withPerson(brokerRequest(t, method, path, map[string]any{}, "", ""), brokerTestPerson)
				}},
				{"a personal token holding person, naming its own session", func(t *testing.T, method, path string) *http.Request {
					return brokerRequest(t, method, path, map[string]any{"actor": sessionActor()}, "Bearer "+personal, "")
				}},
				{"a broker token acting for its person", func(t *testing.T, method, path string) *http.Request {
					return caller.presenting(t, method, path, "a-person-scoped-token", map[string]any{})
				}},
			}
			daemons := []credential{
				{"the daemon's own broker token", func(t *testing.T, method, path string) *http.Request {
					return caller.presenting(t, method, path, "the-daemon-token", map[string]any{})
				}},
			}
			if !setting.on {
				daemons = append(daemons, credential{"the shared token naming the daemon's session", func(t *testing.T, method, path string) *http.Request {
					return brokerRequest(t, method, path, map[string]any{"actor": daemonSessionOf(brokerTestProject)}, "Bearer agent-token", "")
				}})
			}

			walked := 0
			for _, route := range rig.served.routes() {
				if route.DaemonWrites == nil {
					continue
				}
				walked++
				name := route.Method + " " + route.Pattern
				key := issue
				if strings.HasPrefix(route.Pattern, "/api/v1/projects/{key}") {
					key = brokerTestProject
				}
				path := strings.NewReplacer("{key}", key, "{id}", uuid.NewString()).Replace(route.Pattern)
				if strings.Contains(path, "{") {
					t.Fatalf("%s: the walk fills {key} and {id} alone, and %s holds another wildcard: fill it here", name, path)
				}
				ran := 0
				route.Handler = func(w http.ResponseWriter, _ *http.Request) {
					ran++
					w.WriteHeader(http.StatusNoContent)
				}
				// On a mux of its own, as register mounts it, so the path's values reach the gate.
				mux := http.NewServeMux()
				mux.HandleFunc(name, rig.served.routeHandler(route, keyedRoute{}))
				serve := func(request *http.Request) *httptest.ResponseRecorder {
					response := httptest.NewRecorder()
					mux.ServeHTTP(response, request)
					return response
				}
				daemon := legionDaemonSessionOf(key)
				t.Run(name, func(t *testing.T) {
					for _, person := range persons {
						before := ran
						response := serve(person.send(t, route.Method, path))
						if response.Code != http.StatusForbidden || ran != before {
							t.Errorf("%s: status=%d body=%s with its handler run %d times, want 403 %s and no run",
								person.name, response.Code, response.Body.String(), ran-before, route.DaemonWrites.code())
							continue
						}
						code, text := refusalOf(t, response)
						if code != route.DaemonWrites.code() {
							t.Errorf("%s: refused %s (%s), want %s", person.name, code, text, route.DaemonWrites.code())
						}
						if !strings.Contains(text, daemon) {
							t.Errorf("%s: the refusal %q does not name the session %s, which writes %s", person.name, text, daemon, path)
						}
					}
					for _, writer := range daemons {
						before := ran
						if response := serve(writer.send(t, route.Method, path)); response.Code != http.StatusNoContent || ran != before+1 {
							t.Errorf("%s: status=%d body=%s with its handler run %d times, want the gate to pass it to the handler once",
								writer.name, response.Code, response.Body.String(), ran-before)
						}
					}
				})
			}
			if walked == 0 {
				t.Fatal("no route of the table states DaemonWrites, so the walk held no gate")
			}
		})
	}
}
