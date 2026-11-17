package routes_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/routes"
)

// fakeHub is a Source over a hub held in memory: its pods, each pod's server id, routes, log and
// route port INFO, the answer a pod's route port gives a route login ("<pod> no user" or "<pod>
// empty password", the refusal when unset), an error for any "<pod> <path>" read it refuses
// ("<pod> log", "<pod> info" and "<pod> login" among them), a server id a pod's /routez answers in
// place of its own, and a change made the first time a pod's log is read.
type fakeHub struct {
	pods      []routes.Pod
	podsErr   error
	ids       map[string]string
	routezIDs map[string]string
	routes    map[string][]routes.Route
	logs      map[string]string
	infos     map[string]string
	answers   map[string]string
	refused   map[string]error
	onLog     map[string]func()
	closed    bool
	// mu guards infos, which atEnv sets while a fake route port's goroutine may read it.
	mu sync.Mutex
}

func (h *fakeHub) Pods(context.Context) ([]routes.Pod, error) {
	if h.podsErr != nil {
		return nil, h.podsErr
	}
	return slices.Clone(h.pods), nil
}

func (h *fakeHub) Monitor(_ context.Context, pod, path string) ([]byte, error) {
	if err := h.refused[pod+" "+path]; err != nil {
		return nil, err
	}
	switch path {
	case "/varz":
		return json.Marshal(map[string]string{"server_id": h.ids[pod], "server_name": pod})
	case "/routez":
		id := h.ids[pod]
		if other, ok := h.routezIDs[pod]; ok {
			id = other
		}
		return json.Marshal(routes.Routez{ServerID: id, Routes: h.routes[pod]})
	}
	return nil, fmt.Errorf("no %s", path)
}

func (h *fakeHub) Log(_ context.Context, pod string) ([]byte, error) {
	if change := h.onLog[pod]; change != nil {
		delete(h.onLog, pod)
		change()
	}
	if err := h.refused[pod+" log"]; err != nil {
		return nil, err
	}
	return []byte(h.logs[pod]), nil
}

func (h *fakeHub) RouteExchange(_ context.Context, pod string, send []byte) ([]byte, []byte, error) {
	if err := h.refused[pod+" info"]; err != nil {
		return nil, nil, err
	}
	h.mu.Lock()
	info := []byte(h.infos[pod])
	h.mu.Unlock()
	if len(send) == 0 {
		return info, nil, nil
	}
	if err := h.refused[pod+" login"]; err != nil {
		return info, nil, err
	}
	login := "no user"
	if strings.Contains(string(send), `"user":"route","pass":""`) {
		login = "empty password"
	}
	if answer, ok := h.answers[pod+" "+login]; ok {
		return info, []byte(answer + "\r\n"), nil
	}
	return info, []byte("-ERR 'Authorization Violation'\r\n"), nil
}

func (h *fakeHub) Close() error {
	h.closed = true
	return nil
}

// atEnv sets every pod's route port INFO to one without auth_required, as at the env stage, and
// returns the function that sets them back.
func (h *fakeHub) atEnv() (back func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	was := maps.Clone(h.infos)
	for _, p := range h.pods {
		h.infos[p.Name] = authInfo(h.ids[p.Name], false)
	}
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.infos = was
	}
}

var started = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// logLine is a line as nats-server writes its log, with the pid, the time and the level it
// prefixes.
func logLine(level, text string) string {
	return "[7] 2026/11/17 12:00:01.000000 [" + level + "] " + text
}

// removedRoute is the line a server logs for each route URL a rollback removes, with the route
// password in it (reload.go:543). It builds the URL from its parts, and over two lines, so that
// no line of this source holds a URL carrying a user and a password, which the repository's own
// scan of what it pushes reads as a credential.
func removedRoute(password string) string {
	url := "nats://" + "route" + ":" + password
	return logLine("INF", "Removed route "+url+"@envoy-nats-2.envoy-nats-headless:6222")
}

// The lines of each stage a hub server logs: its start, its routes' connections at env, and the
// reloads onto routes and onto authorization, each begun by the reloader's hangup.
var (
	startLines         = []string{logLine("INF", "Starting nats-server"), logLine("INF", "  Version:  2.15.1")}
	routesReload       = []string{logLine("INF", `Trapped "hangup" signal`), logLine("INF", "Reloaded: cluster routes"), logLine("INF", "Reloaded server configuration (abc)")}
	authorizationStart = logLine("INF", `Trapped "hangup" signal`)
	authorizationLines = []string{logLine("INF", "Reloaded: cluster"), logLine("INF", "Reloaded server configuration (def)")}
)

// authInfo is the INFO a route port sends with authorization in force, and without.
func authInfo(id string, required bool) string {
	return fmt.Sprintf(`INFO {"server_id":%q,"port":6222,"auth_required":%v}`+"\r\n", id, required)
}

// cleanHub is three pods of envoy-nats, each Ready since started, each with a route to each of the
// other two, a route port whose INFO requires authorization, and a log that begins with the start
// line, names its two peers' route connections at env, reloads onto routes and onto authorization,
// and names its peers' route connections again after.
func cleanHub() *fakeHub {
	h := &fakeHub{ids: map[string]string{}, routezIDs: map[string]string{}, routes: map[string][]routes.Route{}, logs: map[string]string{},
		infos: map[string]string{}, answers: map[string]string{}, refused: map[string]error{}, onLog: map[string]func(){}}
	for i := range 3 {
		name := fmt.Sprintf("envoy-nats-%d", i)
		h.pods = append(h.pods, routes.Pod{Name: name, UID: "uid-" + name, IP: fmt.Sprintf("10.0.0.%d", 10+i), Ready: true, StartedAt: started})
		h.ids[name] = fmt.Sprintf("NSERVER%d", i)
		h.infos[name] = authInfo(h.ids[name], true)
	}
	for _, p := range h.pods {
		var connections []string
		for _, peer := range h.pods {
			if peer.Name == p.Name {
				continue
			}
			h.routes[p.Name] = append(h.routes[p.Name], routes.Route{RemoteID: h.ids[peer.Name], RemoteName: peer.Name, IP: peer.IP, Port: 6222})
			connections = append(connections, logLine("INF", peer.IP+":6222 - rid:5 - Route connection created"),
				logLine("INF", peer.IP+":41234 - rid:6 - Route connection created"))
		}
		lines := slices.Concat(startLines, connections, routesReload, []string{authorizationStart}, authorizationLines, connections)
		h.logs[p.Name] = strings.Join(lines, "\n") + "\n"
	}
	return h
}

// authorizationReload is the reload that loaded authorization, as it stands in a clean hub's log.
// It is found by its hangup and its "Reloaded: cluster" line together, since the routes reload
// before it begins with the same hangup line.
var authorizationReload = authorizationStart + "\n" + strings.Join(authorizationLines, "\n") + "\n"

// beforeAuthorization puts line into pod's log just before the reload that loaded authorization.
func (h *fakeHub) beforeAuthorization(pod, line string) {
	h.editAuthorizationReload(pod, line+"\n"+authorizationReload)
}

// inAuthorizationReload puts line into pod's log between the hangup that began the reload that
// loaded authorization and that reload's "Reloaded: cluster" line.
func (h *fakeHub) inAuthorizationReload(pod, line string) {
	h.editAuthorizationReload(pod, authorizationStart+"\n"+line+"\n"+strings.Join(authorizationLines, "\n")+"\n")
}

// withoutAuthorizationReload is pod's log with the reload that loaded authorization taken out.
func (h *fakeHub) withoutAuthorizationReload(pod string) { h.editAuthorizationReload(pod, "") }

// editAuthorizationReload replaces the reload that loaded authorization in pod's log with with,
// and panics when the log holds no such reload, so a row cannot edit a log it did not mean to.
func (h *fakeHub) editAuthorizationReload(pod, with string) {
	if strings.Count(h.logs[pod], authorizationReload) != 1 {
		panic("the log of " + pod + " does not hold the authorization reload once")
	}
	h.logs[pod] = strings.Replace(h.logs[pod], authorizationReload, with, 1)
}

// recordOf is the record Take makes of h at env, before its route ports require authorization.
func recordOf(t *testing.T, h *fakeHub) routes.Record {
	t.Helper()
	back := h.atEnv()
	defer back()
	rec, err := routes.Take(context.Background(), h, "envoy", "envoy-nats", started.Add(time.Minute))
	require.NoError(t, err)
	return rec
}

// Check finds a clean hub Clean, and each way a route or a log can show another process, or the
// record of one can be gone, or authorization can be out of force, the verdict and the pod it names.
// Every row starts from the clean hub, which is the control: changed in that one way, it is no
// longer Clean, or, for the rows whose verdict is Clean, a change the check must not count. A
// route's remote_name is never what condemns it or clears it: the impostor rows carry a hub
// server's name. A client's line is one a client can make the server write: its -ERR text whole at
// the end of a "Client Error" line (client.go:2268); no such line meets or trips a rule.
func TestCheckJudgesEachRouteAndLog(t *testing.T) {
	createdAs := func(address, rid string) string {
		return logLine("INF", address+" - rid:"+rid+" - Route connection created")
	}
	created := func(address string) string { return createdAs(address, "9") }
	clientLine := func(text string) string { return logLine("ERR", "10.9.9.3:50000 - cid:7 - Client Error "+text) }
	refusedAs := func(address, rid string) string {
		return logLine("ERR", address+" - rid:"+rid+` - authentication error - User "route"`)
	}
	for _, row := range []struct {
		name    string
		change  func(h *fakeHub)
		verdict routes.Verdict
		pod     string
		detail  string
	}{
		{"clean", func(*fakeHub) {}, routes.Clean, "", ""},
		{"an impostor at a pod IP with a hub server's name", func(h *fakeHub) {
			h.routes["envoy-nats-0"] = append(h.routes["envoy-nats-0"], routes.Route{RemoteID: "NIMPOSTOR", RemoteName: "envoy-nats-1", IP: "10.0.0.11"})
		}, routes.PasswordRead, "envoy-nats-0", `remote_id NIMPOSTOR, remote_name "envoy-nats-1"`},
		{"a route from another address with a hub server's id", func(h *fakeHub) {
			h.routes["envoy-nats-2"][0].IP = "10.9.9.9"
		}, routes.PasswordRead, "envoy-nats-2", "ip 10.9.9.9"},
		{"a route to the server itself", func(h *fakeHub) {
			h.routes["envoy-nats-1"] = append(h.routes["envoy-nats-1"], routes.Route{RemoteID: "NSERVER1", RemoteName: "envoy-nats-0", IP: "10.0.0.10"})
		}, routes.PasswordRead, "envoy-nats-1", "remote_id NSERVER1"},
		{"a route connection with another address since authorization, and none in /routez", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += created("10.9.9.8:6222") + "\n" + created("10.9.9.8:6222") + "\n"
		}, routes.PasswordRead, "envoy-nats-1", "2 route connection(s) with 10.9.9.8 since it loaded authorization"},
		{"an IPv6 route connection since authorization", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += created("[fd00::9]:6222") + "\n"
		}, routes.PasswordRead, "envoy-nats-0", "with fd00::9"},
		{"a route connection begun by the authorization reload's hangup", func(h *fakeHub) {
			h.inAuthorizationReload("envoy-nats-2", created("10.9.9.6:6222"))
		}, routes.PasswordRead, "envoy-nats-2", "with 10.9.9.6"},
		{"an env roll's old pod IP before authorization", func(h *fakeHub) {
			h.beforeAuthorization("envoy-nats-2", created("10.0.0.99:6222"))
			h.beforeAuthorization("envoy-nats-1", created("10.0.0.98:41234"))
		}, routes.Clean, "", ""},
		{"the pod's own loopback since authorization", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += created("127.0.0.1:50000") + "\n" + created("[::1]:50001") + "\n"
		}, routes.Clean, "", ""},
		{"another loopback address since authorization", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += created("127.0.0.2:50000") + "\n"
		}, routes.PasswordRead, "envoy-nats-0", "with 127.0.0.2"},
		{"a failed reload", func(h *fakeHub) {
			h.beforeAuthorization("envoy-nats-1", logLine("ERR", "Failed to reload server configuration: error parsing route url"))
		}, routes.PasswordRead, "envoy-nats-1", "1 failed reload(s)"},
		{"a client's line that ends as a failed reload's", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += clientLine("Failed to reload server configuration: x") + "\n"
		}, routes.Clean, "", ""},
		{"a client's line that ends as a route connection's from another address", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += clientLine("[INF] 10.66.6.6:6222 - rid:1 - Route connection created") + "\n"
		}, routes.Clean, "", ""},
		{"a first line that is a client's, ending as the start line", func(h *fakeHub) {
			rest := strings.SplitN(h.logs["envoy-nats-0"], "\n", 2)[1]
			h.logs["envoy-nats-0"] = clientLine(strings.Repeat("A", 3000)+" [INF] Starting nats-server") + "\n" + rest
		}, routes.PasswordRead, "envoy-nats-0", "does not begin with the server's start line"},
		{"a start line written an hour after its container's start", func(h *fakeHub) {
			rest := strings.SplitN(h.logs["envoy-nats-2"], "\n", 2)[1]
			h.logs["envoy-nats-2"] = "[7] 2026/11/17 13:00:01.000000 [INF] Starting nats-server\n" + rest
		}, routes.PasswordRead, "envoy-nats-2", "was written at 2026-11-17T13:00:01Z, and its container started at 2026-11-17T12:00:00Z"},
		{"a refused route login from another address since authorization", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += createdAs("10.9.9.5:50000", "21") + "\n" + refusedAs("10.9.9.5:50000", "21") + "\n"
		}, routes.Unknown, "envoy-nats-0", "1 route connection(s) with 10.9.9.5 since it loaded authorization whose login the route credential refused"},
		{"a refused login and an accepted connection from one address", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += createdAs("10.9.9.5:50000", "21") + "\n" + refusedAs("10.9.9.5:50000", "21") + "\n" +
				createdAs("10.9.9.5:6222", "22") + "\n"
		}, routes.PasswordRead, "envoy-nats-0", "1 route connection(s) with 10.9.9.5 since it loaded authorization, and"},
		{"a client's line that names a refused login of a route connection", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += created("10.9.9.8:6222") + "\n" + clientLine("10.9.9.8:6222 - rid:9 - authentication error") + "\n"
		}, routes.PasswordRead, "envoy-nats-1", "1 route connection(s) with 10.9.9.8 since it loaded authorization, and"},
		{"a rollback's Removed route line before authorization", func(h *fakeHub) {
			h.beforeAuthorization("envoy-nats-1", removedRoute("routePasswordAbc"))
		}, routes.PasswordRead, "envoy-nats-1", "1 line(s) the server wrote with a URL that carries a user and a password"},
		{"a client's line with a URL that carries a password", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += clientLine("nats://route:x@10.9.9.9:6222") + "\n"
		}, routes.Clean, "", ""},
		{"a route port that takes the route user with no password", func(h *fakeHub) {
			h.answers["envoy-nats-1 empty password"] = `-ERR 'Rejecting connection from gateway "envoy-infra-routes-probe" on the Route port'`
		}, routes.Unknown, "envoy-nats-1", "authorization is not in force: its route port took a route login with the user route and no password"},
		{"a route port that takes a login with no user", func(h *fakeHub) { h.answers["envoy-nats-2 no user"] = "PONG" },
			routes.Unknown, "envoy-nats-2", `its route port took a route login with no user (it answered "PONG"`},
		{"a route login's answer unread", func(h *fakeHub) { h.refused["envoy-nats-0 login"] = errors.New("reset") },
			routes.Unknown, "envoy-nats-0", "its route port's answer to a route login with no user could not be read"},
		{"a restart during the check", func(h *fakeHub) {
			h.onLog["envoy-nats-2"] = func() {
				h.pods[2].RestartCount, h.pods[2].StartedAt = 1, started.Add(time.Hour)
				h.logs["envoy-nats-2"] = strings.Join(slices.Concat(startLines, []string{created("10.0.0.10:6222"), created("10.0.0.11:6222")}), "\n") + "\n"
			}
		}, routes.Unknown, "envoy-nats-2", "changed during the check (at its start: uid uid-envoy-nats-2, ip 10.0.0.12, restartCount 0, startedAt 2026-11-17T12:00:00Z; at its end: uid uid-envoy-nats-2, ip 10.0.0.12, restartCount 1"},
		{"authorization never loaded", func(h *fakeHub) {
			h.withoutAuthorizationReload("envoy-nats-0")
			h.infos["envoy-nats-0"] = authInfo("NSERVER0", false)
		}, routes.Unknown, "envoy-nats-0", "authorization is not in force: its route port's INFO has no auth_required, and no reload has loaded authorization"},
		{"authorization never loaded, and another address before it", func(h *fakeHub) {
			h.withoutAuthorizationReload("envoy-nats-0")
			h.infos["envoy-nats-0"] = authInfo("NSERVER0", false)
			h.logs["envoy-nats-0"] += created("10.9.9.5:6222") + "\n"
		}, routes.Unknown, "envoy-nats-0", "authorization is not in force"},
		{"authorization dropped by a later reload", func(h *fakeHub) { h.infos["envoy-nats-2"] = authInfo("NSERVER2", false) },
			routes.Unknown, "envoy-nats-2", "a reload that changed its cluster block left it without authorization"},
		{"authorization since the server started", func(h *fakeHub) { h.withoutAuthorizationReload("envoy-nats-1") }, routes.Clean, "", ""},
		{"authorization since the start, and another address at env", func(h *fakeHub) {
			h.withoutAuthorizationReload("envoy-nats-1")
			h.logs["envoy-nats-1"] = strings.Replace(h.logs["envoy-nats-1"], strings.Join(routesReload, "\n"), created("10.9.9.4:6222")+"\n"+strings.Join(routesReload, "\n"), 1)
		}, routes.PasswordRead, "envoy-nats-1", "with 10.9.9.4"},
		{"an authorization reload that did not finish", func(h *fakeHub) {
			h.logs["envoy-nats-0"] = strings.Replace(h.logs["envoy-nats-0"], authorizationLines[1]+"\n", "", 1)
		}, routes.Unknown, "envoy-nats-0", `has logged no "[INF] Reloaded server configuration"`},
		{"a route port INFO refused", func(h *fakeHub) { h.refused["envoy-nats-1 info"] = errors.New("refused") }, routes.Unknown, "envoy-nats-1", "its route port's INFO could not be read"},
		{"a route port that sends no INFO", func(h *fakeHub) { h.infos["envoy-nats-1"] = "-ERR 'Unknown Protocol Operation'\r\n" }, routes.Unknown, "envoy-nats-1", "is no INFO"},
		{"a pod with another password", func(h *fakeHub) {
			h.routes["envoy-nats-0"] = nil
			h.routes["envoy-nats-1"] = h.routes["envoy-nats-1"][1:]
			h.routes["envoy-nats-2"] = h.routes["envoy-nats-2"][1:]
			failed := logLine("ERR", `10.0.0.10:41234 - rid:12 - authentication error - User "route"`)
			h.logs["envoy-nats-1"] += failed + "\n" + failed + "\n"
		}, routes.Unknown, "envoy-nats-1", `the hub's servers do not hold one route password: its routes do not reach all its peers, and its log names failed route logins from pod IPs since it loaded authorization (2 from 10.0.0.10)`},
		{"failed route logins from pod IPs in a whole mesh", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += logLine("ERR", `10.0.0.10:41234 - rid:12 - authentication error - User "route"`) + "\n"
		}, routes.Clean, "", ""},
		{"a log that does not begin at the start", func(h *fakeHub) {
			h.logs["envoy-nats-2"] = strings.SplitN(h.logs["envoy-nats-2"], "\n", 2)[1]
		}, routes.PasswordRead, "envoy-nats-2", "does not begin with the server's start line"},
		{"a new restart count", func(h *fakeHub) { h.pods[1].RestartCount = 1 }, routes.PasswordRead, "envoy-nats-1", "restartCount 1"},
		{"a new start", func(h *fakeHub) { h.pods[0].StartedAt = started.Add(time.Hour) }, routes.PasswordRead, "envoy-nats-0", "has run a new container"},
		{"a new uid", func(h *fakeHub) { h.pods[2].UID = "uid-new" }, routes.PasswordRead, "envoy-nats-2", "uid uid-new"},
		{"a route to one peer alone", func(h *fakeHub) { h.routes["envoy-nats-0"] = h.routes["envoy-nats-0"][:1] }, routes.Unknown, "envoy-nats-0", "routes to 1 of its 2 peers"},
		{"a /varz refused", func(h *fakeHub) { h.refused["envoy-nats-1 /varz"] = errors.New("refused") }, routes.Unknown, "envoy-nats-1", "its /varz could not be read"},
		{"a /routez refused", func(h *fakeHub) { h.refused["envoy-nats-2 /routez"] = errors.New("refused") }, routes.Unknown, "envoy-nats-2", "its /routez could not be read"},
		{"a log refused", func(h *fakeHub) { h.refused["envoy-nats-0 log"] = errors.New("refused") }, routes.Unknown, "envoy-nats-0", "log could not be read"},
		{"a pod with no IP", func(h *fakeHub) { h.pods[1].IP = "" }, routes.Unknown, "envoy-nats-1", "has no pod IP"},
		{"a pod gone", func(h *fakeHub) { h.pods = h.pods[:2] }, routes.Unknown, "envoy-nats-2", "not in the StatefulSet now"},
		{"the pods unreadable", func(h *fakeHub) { h.podsErr = errors.New("forbidden") }, routes.Unknown, "", "the pods could not be read: forbidden"},
		{"a /routez of another server", func(h *fakeHub) { h.routezIDs["envoy-nats-1"] = "NOTHER" }, routes.Unknown, "envoy-nats-1", "its /routez answered for the server NOTHER"},
		{"two pods with one server id", func(h *fakeHub) { h.ids["envoy-nats-2"] = "NSERVER0" }, routes.Unknown, "envoy-nats-2", "answered the server id NSERVER0"},
		{"an impostor beside a /varz refused", func(h *fakeHub) {
			h.refused["envoy-nats-1 /varz"] = errors.New("refused")
			h.routes["envoy-nats-0"] = append(h.routes["envoy-nats-0"], routes.Route{RemoteID: "NIMPOSTOR", RemoteName: "envoy-nats-1", IP: "10.9.9.7"})
		}, routes.PasswordRead, "envoy-nats-0", "ip 10.9.9.7"},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := cleanHub()
			rec := recordOf(t, h)
			row.change(h)
			report := routes.Check(context.Background(), h, rec)
			require.Equal(t, row.verdict, report.Verdict(), "findings: %v", report.Findings)
			if row.verdict == routes.Clean {
				require.Empty(t, report.Findings)
				return
			}
			var named []string
			for _, f := range report.Findings {
				if f.Pod == row.pod && f.Verdict == row.verdict && strings.Contains(f.Detail, row.detail) {
					named = append(named, f.Detail)
				}
			}
			require.NotEmpty(t, named, "no %s finding for %q naming %q in %v", row.verdict, row.pod, row.detail, report.Findings)
		})
	}
}

// A /varz refused leaves the IP checks standing and no remote_id judged, so an impostor at a pod IP
// is not cleared, and the verdict is Unknown with no finding of a read password: what the check
// could not read it neither condemns nor clears.
func TestCheckJudgesNoRemoteIDWithoutEveryServerID(t *testing.T) {
	h := cleanHub()
	rec := recordOf(t, h)
	h.refused["envoy-nats-2 /varz"] = errors.New("refused")
	h.routes["envoy-nats-0"] = append(h.routes["envoy-nats-0"], routes.Route{RemoteID: "NIMPOSTOR", RemoteName: "envoy-nats-1", IP: "10.0.0.11"})
	report := routes.Check(context.Background(), h, rec)
	require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
	require.Contains(t, fmt.Sprint(report.Findings), "the hub's server ids are not all known, so routes are judged by IP alone")
}

// Take records every pod, sorted by name, once all are Ready with a running nats container and no
// route port requires authorization, and refuses otherwise, naming each pod that is not so, and a
// pod whose route port's INFO it cannot read.
func TestTakeRecordsOnlyAFinishedRoll(t *testing.T) {
	h := cleanHub()
	slices.Reverse(h.pods)
	h.atEnv()
	rec, err := routes.Take(context.Background(), h, "envoy", "envoy-nats", started.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, routes.Record{Namespace: "envoy", StatefulSet: "envoy-nats", RecordedAt: started.Add(time.Minute), Pods: []routes.PodRecord{
		{Name: "envoy-nats-0", UID: "uid-envoy-nats-0", StartedAt: started},
		{Name: "envoy-nats-1", UID: "uid-envoy-nats-1", StartedAt: started},
		{Name: "envoy-nats-2", UID: "uid-envoy-nats-2", StartedAt: started},
	}}, rec)

	h.infos["envoy-nats-1"] = authInfo("NSERVER1", true)
	h.infos["envoy-nats-0"] = authInfo("NSERVER0", true)
	_, err = routes.Take(context.Background(), h, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "authorization is already in force on envoy-nats-1, envoy-nats-0 (its route port's INFO has auth_required)")
	h.atEnv()

	h.refused["envoy-nats-1 info"] = errors.New("refused")
	_, err = routes.Take(context.Background(), h, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "the route port's INFO of envoy-nats-1 (refused) could not be read")
	delete(h.refused, "envoy-nats-1 info")
	h.pods[0].Ready = false
	h.pods[2].StartedAt = time.Time{}
	_, err = routes.Take(context.Background(), h, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "envoy-nats-2, envoy-nats-0 not Ready with a running nats container")

	h.podsErr = errors.New("forbidden")
	_, err = routes.Take(context.Background(), h, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "read the pods: forbidden")
}
