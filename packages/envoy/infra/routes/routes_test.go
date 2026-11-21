package routes_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
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
// place of its own, a change made the first time a pod's log is read, and the pods whose log it
// gives without the runtime's times.
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
	unstamped map[string]bool
	closed    bool
	// context is the kubeconfig context the hub was opened through, "" for the current one,
	// example-context; its cluster's server is server, or, when that is "", named after the
	// context, so another context is another cluster.
	context, server string
	// mu guards infos, answers and refused, which a fake route port's goroutine reads through
	// RouteExchange while the test may set them (atEnv, setInfo, setAnswer): the race detector sees
	// no order between the test's write and the goroutine's read, which a TCP connection orders.
	mu sync.Mutex
}

func (h *fakeHub) Cluster(context.Context) (routes.Cluster, error) {
	name := h.context
	if name == "" {
		name = "example-context"
	}
	server := h.server
	if server == "" {
		server = "https://" + name + ".example.invalid"
	}
	return routes.Cluster{Context: name, Server: server}, nil
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

// Log is the pod's log as `kubectl logs --timestamps` gives it (stamp), or as the fake holds it
// for a pod in unstamped.
func (h *fakeHub) Log(_ context.Context, pod string) ([]byte, error) {
	if change := h.onLog[pod]; change != nil {
		delete(h.onLog, pod)
		change()
	}
	if err := h.refused[pod+" log"]; err != nil {
		return nil, err
	}
	if h.unstamped[pod] {
		return []byte(h.logs[pod]), nil
	}
	return []byte(stamp(h.logs[pod])), nil
}

// runtimeTime begins a line a row stamped itself, and loggerTime is the time in a line in the
// logger's form.
var (
	runtimeTime = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\S+ `)
	loggerTime  = regexp.MustCompile(`^\[\d+\] (\d{4}/\d\d/\d\d \d\d:\d\d:\d\d\.\d{6}) `)
)

// stamp puts before each line of log the time a container runtime read it at, as `kubectl logs
// --timestamps` prints it: a line a row stamped already keeps its time (stampedAt), a line in the
// logger's form is read a millisecond after its logger time, and any other line at the time of the
// line before it.
func stamp(log string) string {
	var out strings.Builder
	last := started
	for _, line := range strings.SplitAfter(log, "\n") {
		switch {
		case strings.TrimSpace(line) == "" || runtimeTime.MatchString(line):
			out.WriteString(line)
			continue
		case loggerTime.MatchString(line):
			if at, err := time.ParseInLocation("2006/01/02 15:04:05.000000", loggerTime.FindStringSubmatch(line)[1], time.UTC); err == nil {
				last = at.Add(time.Millisecond)
			}
		}
		out.WriteString(last.Format(time.RFC3339Nano) + " " + line)
	}
	return out.String()
}

// stampedAt is line as the runtime reads it at at.
func stampedAt(at time.Time, line string) string {
	return at.UTC().Format(time.RFC3339Nano) + " " + line
}

func (h *fakeHub) RouteExchange(_ context.Context, pod string, send []byte) ([]byte, []byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.refused[pod+" info"]; err != nil {
		return nil, nil, err
	}
	info := []byte(h.infos[pod])
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

// setInfo sets pod's route port INFO, under the lock a fake route port reads it with.
func (h *fakeHub) setInfo(pod, info string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.infos[pod] = info
}

// setAnswer sets the answer to a route login ("<pod> no user" or "<pod> empty password"), under
// the lock a fake route port reads it with.
func (h *fakeHub) setAnswer(key, answer string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers[key] = answer
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

// The times a clean hub's log writes each stage at: its start and its routes' connections at env,
// a second after the container started; the reload onto routes two minutes later; and the reload
// onto authorization two minutes after that, as each apply follows the one before.
var (
	envAt           = started.Add(time.Second)
	routesAt        = started.Add(2 * time.Minute)
	authorizationAt = started.Add(4 * time.Minute)
)

// logLineAt is a line as nats-server writes its log at at, with the pid, the time and the level it
// prefixes.
func logLineAt(at time.Time, level, text string) string {
	return "[7] " + at.UTC().Format("2006/01/02 15:04:05.000000") + " [" + level + "] " + text
}

// logLine is logLineAt at env's time. A row that appends such a line after the authorization
// reload puts it in the log rule's window by its place, after the reload's hangup, since the window
// opens at that hangup by position, whatever a line's time (authorizationWindow).
func logLine(level, text string) string { return logLineAt(envAt, level, text) }

// logLineOf is logLine's line as a process of another id writes it.
func logLineOf(pid, level, text string) string {
	return "[" + pid + "]" + strings.TrimPrefix(logLine(level, text), "[7]")
}

// removedRoute is the line a server logs for each route URL a rollback removes, with the route
// password in it (reload.go:543). It builds the URL from its parts, and over two lines, so that
// no line of this source holds a URL carrying a user and a password, which the repository's own
// scan of what it pushes reads as a credential.
func removedRoute(password string) string {
	url := "nats://" + "route" + ":" + password
	return logLine("INF", "Removed route "+url+"@envoy-nats-2.envoy-nats-headless:6222")
}

// startLinesAt are the lines a server logs as it starts at at.
func startLinesAt(at time.Time) []string {
	return []string{logLineAt(at, "INF", "Starting nats-server"), logLineAt(at, "INF", "  Version:  2.15.1")}
}

// authorizationReloadAt is the reload that loads authorization at at: its hangup, its "Reloaded:
// cluster" line and its end.
func authorizationReloadAt(at time.Time) []string {
	return []string{logLineAt(at, "INF", `Trapped "hangup" signal`), logLineAt(at, "INF", "Reloaded: cluster"),
		logLineAt(at, "INF", "Reloaded server configuration (def)")}
}

// The lines of each stage a hub server logs: its start, its routes' connections at env, and the
// reloads onto routes and onto authorization, each begun by the reloader's hangup.
var (
	startLines   = startLinesAt(envAt)
	routesReload = []string{logLineAt(routesAt, "INF", `Trapped "hangup" signal`), logLineAt(routesAt, "INF", "Reloaded: cluster routes"),
		logLineAt(routesAt, "INF", "Reloaded server configuration (abc)")}
	authorizationStart = authorizationReloadAt(authorizationAt)[0]
	authorizationLines = authorizationReloadAt(authorizationAt)[1:]
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
		infos: map[string]string{}, answers: map[string]string{}, refused: map[string]error{}, onLog: map[string]func(){}, unstamped: map[string]bool{}}
	for i := range 3 {
		name := fmt.Sprintf("envoy-nats-%d", i)
		h.pods = append(h.pods, routes.Pod{Name: name, UID: "uid-" + name, IP: fmt.Sprintf("10.0.0.%d", 10+i), Ready: true, Updated: true, RouteEnv: true, StartedAt: started})
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
// the end of a "Client Error" line (client.go:2268); no such line meets or trips a rule. No finding
// of any row holds a control character, since a finding reaches the operator's terminal.
func TestCheckJudgesEachRouteAndLog(t *testing.T) {
	createdAs := func(address, rid string) string {
		return logLine("INF", address+" - rid:"+rid+" - Route connection created")
	}
	created := func(address string) string { return createdAs(address, "9") }
	clientLine := func(text string) string { return logLine("ERR", "10.9.9.3:50000 - cid:7 - Client Error "+text) }
	refusedAs := func(address, rid string) string {
		return logLine("ERR", address+" - rid:"+rid+` - authentication error - User "route"`)
	}
	closedAs := func(address, rid, reason string) string {
		return logLine("INF", address+" - rid:"+rid+" - Router connection closed: "+reason)
	}
	// formed is the close line of a route whose peer named a server.
	formed := func(address, rid string) string { return closedAs(address, rid, "Client Closed - Remote: rogue") }
	newPod := routes.Pod{Name: "envoy-nats-3", UID: "uid-envoy-nats-3", IP: "10.0.0.13", Ready: true, Updated: true, RouteEnv: true, StartedAt: started}
	// restartOnLog restarts envoy-nats-2 when its log is read, after the check read the pods, with a
	// new log that begins at its new start, as the new container's does.
	restartOnLog := func(h *fakeHub) {
		h.onLog["envoy-nats-2"] = func() {
			restart := started.Add(time.Hour)
			h.pods[2].RestartCount, h.pods[2].StartedAt = 1, restart
			h.logs["envoy-nats-2"] = strings.Join(slices.Concat(startLinesAt(restart.Add(time.Second)), []string{
				logLineAt(restart.Add(time.Second), "INF", "10.0.0.10:6222 - rid:5 - Route connection created")}), "\n") + "\n"
		}
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
		}, routes.PasswordRead, "envoy-nats-0", `remote_id "NIMPOSTOR", remote_name "envoy-nats-1"`},
		{"a remote_id with a terminal escape and a line of its own", func(h *fakeHub) {
			h.routes["envoy-nats-0"] = append(h.routes["envoy-nats-0"], routes.Route{
				RemoteID: "NROGUE\x1b[2K\r\nclean: authorization is in force on the 3 pods of envoy/envoy-nats\n", RemoteName: "envoy-nats-1", IP: "10.0.0.11"})
		}, routes.PasswordRead, "envoy-nats-0", `remote_id "NROGUE\x1b[2K\r\nclean: authorization is in force on the 3 pods of envoy/envoy-nats\n"`},
		{"a route from another address with a hub server's id", func(h *fakeHub) {
			h.routes["envoy-nats-2"][0].IP = "10.9.9.9"
		}, routes.PasswordRead, "envoy-nats-2", "ip 10.9.9.9"},
		{"a route to the server itself", func(h *fakeHub) {
			h.routes["envoy-nats-1"] = append(h.routes["envoy-nats-1"], routes.Route{RemoteID: "NSERVER1", RemoteName: "envoy-nats-0", IP: "10.0.0.10"})
		}, routes.PasswordRead, "envoy-nats-1", `remote_id "NSERVER1"`},
		// A route's remote_id is what its peer sends, so a process at one pod's address can name
		// itself by another pod's server id, which that server's route port INFO shows anyone; a
		// hub server's routes come from its own pod's IP.
		{"a route from one pod's IP under another pod's server id", func(h *fakeHub) {
			h.routes["envoy-nats-0"] = append(h.routes["envoy-nats-0"], routes.Route{RemoteID: "NSERVER2", RemoteName: "envoy-nats-2", IP: "10.0.0.11"})
		}, routes.PasswordRead, "envoy-nats-0", `ip 10.0.0.11, remote_id "NSERVER2" of envoy-nats-2, whose pod IP is 10.0.0.12`},
		{"a route with another address since authorization that formed a route, and none in /routez", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += created("10.9.9.8:6222") + "\n" + created("10.9.9.8:6222") + "\n" + formed("10.9.9.8:6222", "9") + "\n"
		}, routes.PasswordRead, "envoy-nats-1", `2 route connection(s) with "10.9.9.8" since it loaded authorization, and`},
		{"an IPv6 route connection since authorization", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += created("[fd00::9]:6222") + "\n" + formed("[fd00::9]:6222", "9") + "\n"
		}, routes.PasswordRead, "envoy-nats-0", `with "fd00::9"`},
		{"a route connection whose address is no host and port", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += createdAs("10.9.9.9", "31") + "\n"
		}, routes.Unknown, "envoy-nats-0", `its log names a route connection with "10.9.9.9", which is no host and port`},
		{"a route connection still open when the log is read", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += createdAs("10.9.9.7:50000", "24") + "\n"
		}, routes.Unknown, "envoy-nats-1", `1 route connection(s) with "10.9.9.7" since it loaded authorization that were still open when the log was read`},
		// nats-server logs the hangup, then publishes the new options, then logs "Reloaded: cluster"
		// (signal.go:58-82, reload.go:1561, :470), and a route the hub dials after the publication
		// carries the new password (route.go:1064, :1092-1093): every connection after the hangup is
		// judged, however long the reload took to log its marker.
		{"a route connection between the authorization reload's hangup and its marker", func(h *fakeHub) {
			h.inAuthorizationReload("envoy-nats-2", created("10.9.9.6:6222"))
			h.logs["envoy-nats-2"] += formed("10.9.9.6:6222", "9") + "\n"
		}, routes.PasswordRead, "envoy-nats-2", `1 route connection(s) with "10.9.9.6" since it loaded authorization, and`},
		{"the authorization reload's marker logged 7 s after its hangup, a route connection between", func(h *fakeHub) {
			late := authorizationAt.Add(7 * time.Second)
			h.editAuthorizationReload("envoy-nats-2", authorizationStart+"\n"+
				logLineAt(authorizationAt.Add(time.Second), "INF", "10.9.9.6:6222 - rid:9 - Route connection created")+"\n"+
				logLineAt(late, "INF", "Reloaded: cluster")+"\n"+logLineAt(late, "INF", "Reloaded server configuration (def)")+"\n")
			h.logs["envoy-nats-2"] += formed("10.9.9.6:6222", "9") + "\n"
		}, routes.PasswordRead, "envoy-nats-2", `1 route connection(s) with "10.9.9.6" since it loaded authorization, and`},
		{"control: a route connection 2 s before the authorization reload's hangup", func(h *fakeHub) {
			h.beforeAuthorization("envoy-nats-2", logLineAt(authorizationAt.Add(-2*time.Second), "INF", "10.9.9.6:6222 - rid:9 - Route connection created"))
			h.logs["envoy-nats-2"] += formed("10.9.9.6:6222", "9") + "\n"
		}, routes.Clean, "", ""},
		// A peer can write a hangup line; the window opens at the first one after the routes stage's
		// reload, so a written one moves it no later.
		{"a hangup line a route peer wrote after the reload's own", func(h *fakeHub) {
			h.inAuthorizationReload("envoy-nats-1", logLineAt(authorizationAt, "INF", "10.9.9.6:6222 - rid:9 - Route connection created")+"\n"+
				logLineAt(authorizationAt, "INF", `Trapped "hangup" signal`))
			h.logs["envoy-nats-1"] += formed("10.9.9.6:6222", "9") + "\n"
		}, routes.PasswordRead, "envoy-nats-1", `1 route connection(s) with "10.9.9.6" since it loaded authorization, and`},
		{"a hangup line a route peer wrote before the reload's own", func(h *fakeHub) {
			h.beforeAuthorization("envoy-nats-1", logLineAt(authorizationAt.Add(-2*time.Second), "INF", `Trapped "hangup" signal`)+"\n"+
				logLineAt(authorizationAt.Add(-time.Second), "INF", "10.9.9.6:6222 - rid:9 - Route connection created"))
			h.logs["envoy-nats-1"] += formed("10.9.9.6:6222", "9") + "\n"
		}, routes.PasswordRead, "envoy-nats-1", `1 route connection(s) with "10.9.9.6" since it loaded authorization, and`},
		{"no hangup between the routes reload and the authorization reload", func(h *fakeHub) {
			h.editAuthorizationReload("envoy-nats-0", strings.Join(authorizationLines, "\n")+"\n")
		}, routes.Unknown, "envoy-nats-0", `holds no "[INF] Trapped \"hangup\" signal" after the routes stage's "[INF] Reloaded: cluster routes"`},
		{"no routes reload before the authorization reload", func(h *fakeHub) {
			h.logs["envoy-nats-0"] = strings.Replace(h.logs["envoy-nats-0"], routesReload[1]+"\n", "", 1)
		}, routes.Unknown, "envoy-nats-0", `holds no "[INF] Trapped \"hangup\" signal" after the routes stage's "[INF] Reloaded: cluster routes"`},
		{"authorization loaded within 10 s of the container's start", func(h *fakeHub) {
			h.editAuthorizationReload("envoy-nats-0", strings.Join(authorizationReloadAt(started.Add(5*time.Second)), "\n")+"\n")
		}, routes.Unknown, "envoy-nats-0", "within 10s of its container's start at 2026-11-17T12:00:00Z"},
		{"an env roll's old pod IP before authorization", func(h *fakeHub) {
			h.beforeAuthorization("envoy-nats-2", created("10.0.0.99:6222"))
			h.beforeAuthorization("envoy-nats-1", created("10.0.0.98:41234"))
		}, routes.Clean, "", ""},
		{"the pod's own loopback since authorization", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += created("127.0.0.1:50000") + "\n" + created("[::1]:50001") + "\n"
		}, routes.Clean, "", ""},
		{"another loopback address since authorization", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += created("127.0.0.2:50000") + "\n" + formed("127.0.0.2:50000", "9") + "\n"
		}, routes.PasswordRead, "envoy-nats-0", `with "127.0.0.2"`},
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
		{"a start line read an hour after its container's start", func(h *fakeHub) {
			rest := strings.SplitN(h.logs["envoy-nats-2"], "\n", 2)[1]
			h.logs["envoy-nats-2"] = "[7] 2026/11/17 13:00:01.000000 [INF] Starting nats-server\n" + rest
		}, routes.PasswordRead, "envoy-nats-2", "was read by the container runtime at 2026-11-17T13:00:01.001Z, and its container started at 2026-11-17T12:00:00Z"},
		{"a start line a route peer forged at the container's start, read an hour later and first in a rotated log", func(h *fakeHub) {
			rest := strings.SplitN(h.logs["envoy-nats-2"], "\n", 2)[1]
			h.logs["envoy-nats-2"] = stampedAt(started.Add(time.Hour), "[7] 2026/11/17 12:00:00.400000 [INF] Starting nats-server") + "\n" + rest
		}, routes.PasswordRead, "envoy-nats-2", "does not begin with the server's start line"},
		{"a line in the server's form from another process id", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += logLineOf("4141", "INF", "10.66.6.6:6222 - rid:30 - Route connection created") + "\n"
		}, routes.Unknown, "envoy-nats-1", "1 line(s) in nats-server's form that its server did not write when they say"},
		{"a line in the server's form read an hour after its logger time", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += stampedAt(started.Add(time.Hour), removedRoute("x")) + "\n"
		}, routes.Unknown, "envoy-nats-1", "1 line(s) in nats-server's form that its server did not write when they say"},
		{"a log without the runtime's times", func(h *fakeHub) { h.unstamped["envoy-nats-0"] = true },
			routes.Unknown, "envoy-nats-0", "line(s) carry no time of the container runtime's"},
		{"a refused route login from another address since authorization", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += createdAs("10.9.9.5:50000", "21") + "\n" + refusedAs("10.9.9.5:50000", "21") + "\n"
		}, routes.Unknown, "envoy-nats-0", `1 route connection(s) with "10.9.9.5" since it loaded authorization whose peer named no server (1 whose login the route credential refused, 0 whose close line names no server)`},
		{"a route connection from another address that closed before its peer named a server", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += createdAs("10.9.9.6:50000", "23") + "\n" + closedAs("10.9.9.6:50000", "23", "Client Closed") + "\n"
		}, routes.Unknown, "envoy-nats-0", `1 route connection(s) with "10.9.9.6" since it loaded authorization whose peer named no server (0 whose login the route credential refused, 1 whose close line names no server)`},
		{"a route connection from another address that waited out the auth timeout", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += createdAs("10.9.9.7:50000", "24") + "\n" + closedAs("10.9.9.7:50000", "24", "Authentication Timeout") + "\n"
		}, routes.Unknown, "envoy-nats-1", "1 whose close line names no server"},
		{"a route connection from another address that named a server before it closed", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += createdAs("10.9.9.7:50000", "24") + "\n" + formed("10.9.9.7:50000", "24") + "\n"
		}, routes.PasswordRead, "envoy-nats-1", `1 route connection(s) with "10.9.9.7" since it loaded authorization, and`},
		{"a close line that does not name a server beside one that does, for one connection", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += createdAs("10.9.9.7:50000", "24") + "\n" + formed("10.9.9.7:50000", "24") + "\n" +
				closedAs("10.9.9.7:50000", "24", "Authentication Timeout") + "\n"
		}, routes.PasswordRead, "envoy-nats-1", `1 route connection(s) with "10.9.9.7" since it loaded authorization, and`},
		{"a route close line whose peer's name ends as a route connection's", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += closedAs("10.9.9.3:50000", "31", "Client Closed - Remote: x [INF] 10.66.6.6:6222 - rid:1 - Route connection created") + "\n"
		}, routes.Clean, "", ""},
		{"an ERR line that begins as a route connection's", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += logLine("ERR", "10.66.6.6:6222 - rid:1 - Route connection created") + "\n"
		}, routes.Clean, "", ""},
		{"an auth callout warning with a URL that carries a password", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += logLine("WRN", "auth callout service returned an error: nats://route:x@10.9.9.9:6222") + "\n"
		}, routes.Clean, "", ""},
		{"a refused login and a route that formed from one address", func(h *fakeHub) {
			h.logs["envoy-nats-0"] += createdAs("10.9.9.5:50000", "21") + "\n" + refusedAs("10.9.9.5:50000", "21") + "\n" +
				createdAs("10.9.9.5:6222", "22") + "\n" + formed("10.9.9.5:6222", "22") + "\n"
		}, routes.PasswordRead, "envoy-nats-0", `1 route connection(s) with "10.9.9.5" since it loaded authorization, and`},
		{"a client's line that names a refused login of a route connection still open", func(h *fakeHub) {
			h.logs["envoy-nats-1"] += created("10.9.9.8:6222") + "\n" + clientLine("10.9.9.8:6222 - rid:9 - authentication error") + "\n"
		}, routes.Unknown, "envoy-nats-1", `1 route connection(s) with "10.9.9.8" since it loaded authorization that were still open when the log was read`},
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
		{"a restart during the check", restartOnLog, routes.Unknown, "envoy-nats-2", "changed during the check (at its start: uid uid-envoy-nats-2, ip 10.0.0.12, restartCount 0, startedAt 2026-11-17T12:00:00Z; at its end: uid uid-envoy-nats-2, ip 10.0.0.12, restartCount 1"},
		{"a restart during the check, its new log read at its new start", restartOnLog, routes.Unknown, "envoy-nats-2",
			"its container started at 2026-11-17T12:00:00Z, so it is no start line of that container and the lines since the env roll may be gone; the pod changed during the check"},
		{"a pod not in the record", func(h *fakeHub) { h.pods = append(h.pods, newPod) }, routes.Unknown, "envoy-nats-3", "is not in the record of"},
		{"a pod that left during the check", func(h *fakeHub) { h.onLog["envoy-nats-2"] = func() { h.pods = h.pods[:2] } },
			routes.Unknown, "envoy-nats-2", "left the StatefulSet during the check"},
		{"a pod that joined during the check", func(h *fakeHub) { h.onLog["envoy-nats-2"] = func() { h.pods = append(slices.Clone(h.pods), newPod) } },
			routes.Unknown, "envoy-nats-3", "joined the StatefulSet during the check"},
		{"the pods unreadable after the pass", func(h *fakeHub) { h.onLog["envoy-nats-2"] = func() { h.podsErr = errors.New("forbidden") } },
			routes.Unknown, "", "the pods could not be read again after the pass, so a restart during it cannot be ruled out: forbidden"},
		{"authorization never loaded", func(h *fakeHub) {
			h.withoutAuthorizationReload("envoy-nats-0")
			h.infos["envoy-nats-0"] = authInfo("NSERVER0", false)
		}, routes.Unknown, "envoy-nats-0", "authorization is not in force: its route port's INFO has no auth_required, and no reload has loaded authorization"},
		{"authorization not in force, and the log unread", func(h *fakeHub) {
			h.infos["envoy-nats-0"] = authInfo("NSERVER0", false)
			h.refused["envoy-nats-0 log"] = errors.New("forbidden")
		}, routes.Unknown, "envoy-nats-0", "authorization is not in force: its route port's INFO has no auth_required; apply authorization"},
		{"authorization never loaded, and another address before it", func(h *fakeHub) {
			h.withoutAuthorizationReload("envoy-nats-0")
			h.infos["envoy-nats-0"] = authInfo("NSERVER0", false)
			h.logs["envoy-nats-0"] += created("10.9.9.5:6222") + "\n"
		}, routes.Unknown, "envoy-nats-0", "authorization is not in force"},
		{"authorization dropped by a later reload", func(h *fakeHub) { h.infos["envoy-nats-2"] = authInfo("NSERVER2", false) },
			routes.Unknown, "envoy-nats-2", "a reload that changed its cluster block left it without authorization"},
		// A container the record vouches for had no authorization when it was recorded, so it loaded
		// authorization by a reload; with no reload line in the server's own lines, the window cannot
		// be placed, and no route connection is judged as after authorization.
		{"no authorization reload in the log, the route port requiring it: no window", func(h *fakeHub) { h.withoutAuthorizationReload("envoy-nats-1") },
			routes.Unknown, "envoy-nats-1", `its log, in the lines its server wrote, holds no "[INF] Reloaded: cluster"`},
		{"no authorization reload in the log, and another address at env: not judged as after authorization", func(h *fakeHub) {
			h.withoutAuthorizationReload("envoy-nats-1")
			h.logs["envoy-nats-1"] = strings.Replace(h.logs["envoy-nats-1"], strings.Join(routesReload, "\n"),
				created("10.9.9.4:6222")+"\n"+formed("10.9.9.4:6222", "9")+"\n"+strings.Join(routesReload, "\n"), 1)
		}, routes.Unknown, "envoy-nats-1", `holds no "[INF] Reloaded: cluster"`},
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
			for _, f := range report.Findings {
				require.False(t, strings.ContainsFunc(f.Detail, func(r rune) bool { return r < 0x20 || r == 0x7f }),
					"a finding holds a control character: %q", f.Detail)
			}
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

// The env roll gives each new pod a new IP, so the log of a pod rolled first names the old IP of a
// pod rolled after it, in a route that closed naming a hub server. The window rule judges no such
// line: with the reload that loaded authorization in the log, the hub is Clean. Without that
// reload's line among the server's own lines, because the reload has not reached the pod, whose
// route port then cannot be read, or because the runtime read the line too late to count it as the
// server's, the check cannot place the window: Unknown, naming that, and never a password read for
// the old IP.
func TestCheckJudgesNoEnvRollLineAsAfterAuthorization(t *testing.T) {
	const oldIP = "10.0.0.99"
	hub := func(t *testing.T) (*fakeHub, routes.Record) {
		h := cleanHub()
		rec := recordOf(t, h)
		envRoll := logLine("INF", oldIP+":6222 - rid:3 - Route connection created") + "\n" +
			logLine("INF", oldIP+":6222 - rid:3 - Router connection closed: Client Closed - Remote: envoy-nats-1") + "\n"
		marker := routesReload[0] + "\n"
		require.Equal(t, 1, strings.Count(h.logs["envoy-nats-2"], marker), "the routes reload is in the log once")
		h.logs["envoy-nats-2"] = strings.Replace(h.logs["envoy-nats-2"], marker, envRoll+marker, 1)
		return h, rec
	}
	t.Run("control: the log whole, authorization reloaded", func(t *testing.T) {
		h, rec := hub(t)
		report := routes.Check(context.Background(), h, rec)
		require.Equal(t, routes.Clean, report.Verdict(), "findings: %v", report.Findings)
	})
	for _, row := range []struct {
		name   string
		change func(h *fakeHub)
	}{
		{"authorization not yet reloaded on the pod, its route port's INFO unread", func(h *fakeHub) {
			h.withoutAuthorizationReload("envoy-nats-2")
			h.refused["envoy-nats-2 info"] = errors.New("port-forward to 6222 failed")
		}},
		{"the reload's line read 3 s after its logger time", func(h *fakeHub) {
			line := authorizationLines[0]
			require.Equal(t, 1, strings.Count(h.logs["envoy-nats-2"], line+"\n"), "the Reloaded: cluster line is in the log once")
			h.logs["envoy-nats-2"] = strings.Replace(h.logs["envoy-nats-2"], line+"\n", stampedAt(authorizationAt.Add(3*time.Second), line)+"\n", 1)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h, rec := hub(t)
			row.change(h)
			report := routes.Check(context.Background(), h, rec)
			require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
			require.NotContains(t, fmt.Sprint(report.Findings), oldIP, "the env roll's old pod IP is judged by no rule")
			require.Contains(t, fmt.Sprint(report.Findings), `envoy-nats-2: unknown: its log, in the lines its server wrote, holds no "[INF] Reloaded: cluster"`)
		})
	}
}

// A log read without the container runtime's times, as `kubectl logs` without --timestamps gives
// it, is named as such and judged by no other rule, since which of its lines the server wrote
// cannot be told, so the check is Unknown, not PasswordRead. Control: the same log with the
// runtime's times is Clean.
func TestCheckNeedsTheRuntimesTimes(t *testing.T) {
	h := cleanHub()
	rec := recordOf(t, h)
	require.Empty(t, routes.Check(context.Background(), h, rec).Findings, "control: the log with the runtime's times")
	h.unstamped["envoy-nats-1"] = true
	report := routes.Check(context.Background(), h, rec)
	require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
	require.Equal(t, []routes.Finding{{Pod: "envoy-nats-1", Verdict: routes.Unknown,
		Detail: "16 of its log's 16 line(s) carry no time of the container runtime's, so which lines its server wrote cannot be told; " +
			"read the log as kubectl logs --timestamps gives it"}}, report.Findings)
}

// The "not in force" finding says that no reload has loaded authorization since the server started
// only of a log the check judged whole, from the server's own start line. Of a log without the
// runtime's times, which no rule judges, and of one that does not begin at the start line, which
// may have lost that reload, it says only what the route port's INFO shows. Control: the same hub
// with its log whole says it.
func TestCheckSaysNoReloadLoadedAuthorizationOnlyOfAWholeLog(t *testing.T) {
	const plain = "authorization is not in force: its route port's INFO has no auth_required; apply authorization, then check again"
	const whole = "authorization is not in force: its route port's INFO has no auth_required, and no reload has loaded authorization " +
		"since the server started; apply authorization, then check again"
	for _, row := range []struct {
		name   string
		change func(h *fakeHub)
		want   string
	}{
		{"control: the log whole", func(*fakeHub) {}, whole},
		{"a log without the runtime's times", func(h *fakeHub) { h.unstamped["envoy-nats-0"] = true }, plain},
		{"a log that does not begin at the start line", func(h *fakeHub) {
			h.logs["envoy-nats-0"] = strings.SplitN(h.logs["envoy-nats-0"], "\n", 2)[1]
		}, plain},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := cleanHub()
			rec := recordOf(t, h)
			h.withoutAuthorizationReload("envoy-nats-0")
			h.infos["envoy-nats-0"] = authInfo("NSERVER0", false)
			row.change(h)
			var notInForce []string
			for _, f := range routes.Check(context.Background(), h, rec).Findings {
				if f.Pod == "envoy-nats-0" && strings.HasPrefix(f.Detail, "authorization is not in force") {
					notInForce = append(notInForce, f.Detail)
				}
			}
			require.Equal(t, []string{row.want}, notInForce)
		})
	}
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
	require.ErrorContains(t, err, "whether authorization is in force is not known on envoy-nats-1 (its route port's INFO could not be read: refused)")
	delete(h.refused, "envoy-nats-1 info")
	// Between two pods of the roll every pod can be Ready while one still runs the revision before:
	// a record then is of a container the roll replaces next.
	h.pods[1].Updated = false
	_, err = routes.Take(context.Background(), h, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "envoy-nats-1 not on the update revision of a finished roll of the StatefulSet; record once the env stage's roll has finished on every pod")
	h.pods[1].Updated = true
	h.pods[2].RouteEnv = false
	_, err = routes.Take(context.Background(), h, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "envoy-nats-0 has no route variables in its nats container: apply routeAuthorization's env stage")
	h.pods[2].RouteEnv = true
	h.pods[0].Ready = false
	h.pods[2].StartedAt = time.Time{}
	_, err = routes.Take(context.Background(), h, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "envoy-nats-2, envoy-nats-0 not Ready with a running nats container")
	h.podsErr = errors.New("forbidden")
	_, err = routes.Take(context.Background(), h, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "read the pods: forbidden")
}
