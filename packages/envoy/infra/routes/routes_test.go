package routes_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/routes"
)

// fakeHub is a Source over a hub held in memory: its pods, each pod's server id, routes, log and
// route port INFO, an error for any "<pod> <path>" read it refuses ("<pod> log" and "<pod> info"
// among them), and a server id a pod's /routez answers in place of its own.
type fakeHub struct {
	pods      []routes.Pod
	podsErr   error
	ids       map[string]string
	routezIDs map[string]string
	routes    map[string][]routes.Route
	logs      map[string]string
	infos     map[string]string
	refused   map[string]error
	closed    bool
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
	if err := h.refused[pod+" log"]; err != nil {
		return nil, err
	}
	return []byte(h.logs[pod]), nil
}

func (h *fakeHub) RouteInfo(_ context.Context, pod string) ([]byte, error) {
	if err := h.refused[pod+" info"]; err != nil {
		return nil, err
	}
	return []byte(h.infos[pod]), nil
}

func (h *fakeHub) Close() error {
	h.closed = true
	return nil
}

var started = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// logLine is a line as nats-server writes its log, with the pid, the time and the level it
// prefixes.
func logLine(level, text string) string {
	return "[7] 2026/11/17 12:00:01.000000 [" + level + "] " + text
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
		infos: map[string]string{}, refused: map[string]error{}}
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

// recordOf is the record Take makes of h.
func recordOf(t *testing.T, h *fakeHub) routes.Record {
	t.Helper()
	rec, err := routes.Take(context.Background(), h, "envoy", "envoy-nats", started.Add(time.Minute))
	require.NoError(t, err)
	return rec
}

// Check finds a clean hub Clean, and each way a route or a log can show another process, or the
// record of one can be gone, or authorization can be out of force, the verdict and the pod it names.
// Every row starts from the clean hub, which is the control: changed in that one way, it is no
// longer Clean, or, for the rows whose verdict is Clean, a change the check must not count. A
// route's remote_name is never what condemns it or clears it: the impostor rows carry a hub
// server's name.
func TestCheckJudgesEachRouteAndLog(t *testing.T) {
	created := func(address string) string { return logLine("INF", address+" - rid:9 - Route connection created") }
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

// Take records every pod, sorted by name, once all are Ready with a running nats container, and
// refuses otherwise, naming each pod that is not.
func TestTakeRecordsOnlyAFinishedRoll(t *testing.T) {
	h := cleanHub()
	slices.Reverse(h.pods)
	rec, err := routes.Take(context.Background(), h, "envoy", "envoy-nats", started.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, routes.Record{Namespace: "envoy", StatefulSet: "envoy-nats", RecordedAt: started.Add(time.Minute), Pods: []routes.PodRecord{
		{Name: "envoy-nats-0", UID: "uid-envoy-nats-0", StartedAt: started},
		{Name: "envoy-nats-1", UID: "uid-envoy-nats-1", StartedAt: started},
		{Name: "envoy-nats-2", UID: "uid-envoy-nats-2", StartedAt: started},
	}}, rec)

	h.pods[0].Ready = false
	h.pods[2].StartedAt = time.Time{}
	_, err = routes.Take(context.Background(), h, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "envoy-nats-2, envoy-nats-0 not Ready with a running nats container")

	h.podsErr = errors.New("forbidden")
	_, err = routes.Take(context.Background(), h, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "read the pods: forbidden")
}
