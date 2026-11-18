//go:build natslive

package gcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/routes"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// stagedServer is one server of a stagedHub: a nats-server named as a hub pod, on 127.0.0.1 and
// ports of its own, on the nats.conf of the stage it has reached, with the route variables a pod
// has from the env stage on. restarts and startedAt are what the API server would report for its
// container: the number of times it was started again, and when the running one started. stamps
// is when each line of its output was first seen, as the container runtime stamps each line.
type stagedServer struct {
	name                   string
	dir                    string
	client, monitor, route int
	args, vars             []string
	restarts               int32
	startedAt              time.Time
	proc                   *cmdtest.Process
	stamps                 *stamper
}

// stamper records the time it first saw each line of a process's output, polling it every few
// milliseconds, as a container runtime stamps each line it reads from a container's output: a
// time no process in the pod sets.
type stamper struct {
	p      *cmdtest.Process
	mu     sync.Mutex
	seen   int
	stamps []time.Time
}

// stampOutput starts stamping p's output, until p exits.
func stampOutput(p *cmdtest.Process) *stamper {
	st := &stamper{p: p}
	go func() {
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-p.Exited:
				st.read()
				return
			case <-tick.C:
				st.read()
			}
		}
	}()
	return st
}

// read stamps, with now, every line the output has completed since the last read.
func (st *stamper) read() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.p.Output.Len() == st.seen {
		return
	}
	out := st.p.Output.String()
	now := time.Now().UTC()
	for i := strings.IndexByte(out[st.seen:], '\n'); i >= 0; i = strings.IndexByte(out[st.seen:], '\n') {
		st.stamps = append(st.stamps, now)
		st.seen += i + 1
	}
}

// log is the output's complete lines, each after the time it was first seen and a space, as
// `kubectl logs --timestamps` prints a container's log.
func (st *stamper) log() []byte {
	st.read()
	st.mu.Lock()
	defer st.mu.Unlock()
	var out strings.Builder
	lines := strings.SplitAfter(st.p.Output.String()[:st.seen], "\n")
	for i, line := range lines[:len(st.stamps)] {
		out.WriteString(st.stamps[i].Format(time.RFC3339Nano) + " " + line)
	}
	return []byte(out.String())
}

// stagedHub is three servers that move through routeAuthorization's stages as the hub's pods do:
// started on the env render, then each reloaded onto routes, then each onto authorization. It is
// the Source the route check reads, as the API server and a port-forward give it a live hub.
type stagedHub struct {
	renders                map[string]*clusteredHubRender
	natsServer, release    string
	servers                []*stagedServer
	routePorts             []int
	routeURLs              string
	passwordVar, routesVar string
}

// newStagedHub starts the three servers on the env render, with the route password and the route
// URLs in their environment, and waits for their mesh.
func newStagedHub(t *testing.T, renders map[string]*clusteredHubRender, natsServer, password string) *stagedHub {
	t.Helper()
	r := renders["env"]
	require.Equal(t, r.authConf, renders["authorization"].authConf, "the stages change no user")
	drawn := reservePorts(t, 9)
	h := &stagedHub{renders: renders, natsServer: natsServer, release: r.release}
	var urls []string
	for i := range 3 {
		h.routePorts = append(h.routePorts, drawn[3*i+2])
		urls = append(urls, fmt.Sprintf("nats://route:%s@127.0.0.1:%d", password, drawn[3*i+2]))
	}
	h.passwordVar, h.routesVar = "NATS_ROUTE_PASSWORD="+password, "NATS_ROUTE_URLS=["+strings.Join(urls, ", ")+"]"
	for i := range 3 {
		s := &stagedServer{name: fmt.Sprintf("%s-%d", r.release, i), dir: t.TempDir(), client: drawn[3*i], monitor: drawn[3*i+1], route: drawn[3*i+2]}
		s.args = append(podArgs(t, stringList(t, r.container(t, "nats")["args"]), map[string]string{"/etc/nats-config/": s.dir + "/"}),
			"-a", "127.0.0.1", "-p", strconv.Itoa(s.client), "-m", strconv.Itoa(s.monitor), "-P", filepath.Join(s.dir, "nats.pid"))
		s.vars = []string{"SERVER_NAME=" + s.name, h.passwordVar, h.routesVar}
		h.writeConf(t, s, "env")
		h.servers = append(h.servers, s)
	}
	for _, s := range h.servers {
		h.start(t, s)
	}
	h.awaitMesh(t)
	return h
}

// writeConf writes the stage's nats.conf, and auth.conf, into s's directory, with the pod's
// store_dir and route port replaced by s's, and, at env, the chart's route URLs, which name the
// pods by their headless Service, replaced by the three servers' route ports.
func (h *stagedHub) writeConf(t *testing.T, s *stagedServer, stage string) {
	t.Helper()
	r := h.renders[stage]
	conf := r.natsConf
	replace := map[string]string{
		`"store_dir": "/data"`: fmt.Sprintf(`"store_dir": %q`, filepath.Join(s.dir, "data")),
		`"port": 6222`:         fmt.Sprintf(`"host": "127.0.0.1", "port": %d`, s.route),
	}
	if stage == "env" {
		for i, port := range h.routePorts {
			replace[fmt.Sprintf("nats://%s-%d.%s-headless:6222", h.release, i, h.release)] = fmt.Sprintf("nats://127.0.0.1:%d", port)
		}
	}
	for old, local := range replace {
		require.Equal(t, 1, strings.Count(conf, old), "the %s nats.conf sets %s once", stage, old)
		conf = strings.Replace(conf, old, local, 1)
	}
	require.NoError(t, os.WriteFile(filepath.Join(s.dir, "nats.conf"), []byte(conf), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.dir, clusteredHubAuthConfKey), []byte(r.authConf), 0o600))
}

// start runs s on its arguments and route variables, in UTC, the zone of the hub's nats image,
// which sets none and in which the check reads the log's times; records its start as the API
// server reports a container's, to the second, cut down; and stamps its output from its start.
func (h *stagedHub) start(t *testing.T, s *stagedServer) {
	t.Helper()
	cmd := exec.Command(h.natsServer, s.args...)
	cmd.Env = append(withoutRouteVars(os.Environ()), append(s.vars, "TZ=UTC")...)
	s.startedAt = time.Now().UTC().Truncate(time.Second)
	p := startLogged(t, s.name, cmd)
	s.stamps = stampOutput(p)
	if err := awaitReady(p, listeningForRoutes(s.route)); err != nil {
		t.Fatalf("%s did not start: %v\n%s", s.name, err, p.Output.String())
	}
	s.proc = p
}

// reload moves every server, one at a time, onto the stage's nats.conf by the reload the chart's
// config reloader sends, and waits for each to log it, then for the mesh.
func (h *stagedHub) reload(t *testing.T, stage string) {
	t.Helper()
	h.reloadEach(t, stage, "Reloaded server configuration", "Failed to reload server configuration")
	h.awaitMesh(t)
}

// reloadRefused sends every server the stage's nats.conf the same way, and waits for each to log
// that it refused it, as a server does whose new nats.conf it cannot load.
func (h *stagedHub) reloadRefused(t *testing.T, stage string) {
	t.Helper()
	h.reloadEach(t, stage, "Failed to reload server configuration", "Reloaded server configuration")
}

// reloadEach writes each server the stage's nats.conf, sends it the hangup, and waits for one more
// want line in its output, failing on one more unwanted line.
func (h *stagedHub) reloadEach(t *testing.T, stage, want, unwanted string) {
	t.Helper()
	for _, s := range h.servers {
		h.writeConf(t, s, stage)
		out := s.proc.Output.String()
		wantBefore, unwantedBefore := strings.Count(out, want), strings.Count(out, unwanted)
		require.NoError(t, s.proc.Cmd.Process.Signal(syscall.SIGHUP))
		within(t, 15*time.Second, s.name+" reloading onto "+stage, func() error {
			out := s.proc.Output.String()
			if strings.Count(out, unwanted) > unwantedBefore {
				t.Fatalf("%s logged %q on the %s nats.conf:\n%s", s.name, unwanted, stage, out)
			}
			if strings.Count(out, want) == wantBefore {
				return fmt.Errorf("%s has not logged %q yet", s.name, want)
			}
			return nil
		})
	}
}

// restart kills s and starts it again on the nats.conf it has, as the kubelet restarts a container:
// one more restart, a new start, and a log that begins again.
func (h *stagedHub) restart(t *testing.T, s *stagedServer) {
	t.Helper()
	require.NoError(t, s.proc.Cmd.Process.Signal(syscall.SIGKILL))
	s.proc.WaitExit(t, "SIGKILL")
	s.restarts++
	time.Sleep(time.Second) // so the new start is a new second, as startedAt is reported
	h.start(t, s)
	h.awaitMesh(t)
}

// serverIDs is each server's id, from its /varz.
func (h *stagedHub) serverIDs(t *testing.T) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, s := range h.servers {
		var varz struct {
			ID string `json:"server_id"`
		}
		within(t, 15*time.Second, s.name+"'s /varz", func() error { return getJSON(fmt.Sprintf("http://127.0.0.1:%d/varz", s.monitor), &varz) })
		ids[s.name] = varz.ID
	}
	return ids
}

// awaitMesh waits until every server has a route to each of the other two, by server id.
func (h *stagedHub) awaitMesh(t *testing.T) {
	t.Helper()
	ids := h.serverIDs(t)
	within(t, 30*time.Second, "the three servers' mesh", func() error {
		for _, s := range h.servers {
			routez, err := routezOf("127.0.0.1", s.monitor)
			if err != nil {
				return err
			}
			peers := map[string]bool{}
			for _, route := range routez.Routes {
				peers[route.RemoteID] = true
			}
			for _, other := range h.servers {
				if other != s && !peers[ids[other.name]] {
					return fmt.Errorf("%s has no route to %s", s.name, other.name)
				}
			}
		}
		return nil
	})
}

// routezOf is the /routez of the server whose monitoring port is host:monitor.
func routezOf(host string, monitor int) (routes.Routez, error) {
	body, err := monitorGet("http://" + net.JoinHostPort(host, strconv.Itoa(monitor)) + "/routez")
	if err != nil {
		return routes.Routez{}, err
	}
	return routes.ParseRoutez(body)
}

func monitorGet(url string) ([]byte, error) {
	resp, err := monitorClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return body, nil
}

func (h *stagedHub) server(name string) *stagedServer {
	for _, s := range h.servers {
		if s.name == name {
			return s
		}
	}
	return nil
}

// Pods is the three servers as the API server reports the pods: each pod's IP 127.0.0.1, where it
// listens, a uid of its own, and its container's restarts and start.
func (h *stagedHub) Pods(context.Context) ([]routes.Pod, error) {
	var pods []routes.Pod
	for _, s := range h.servers {
		pods = append(pods, routes.Pod{Name: s.name, UID: "uid-" + s.name, IP: "127.0.0.1", Ready: true, RestartCount: s.restarts, StartedAt: s.startedAt})
	}
	return pods, nil
}

func (h *stagedHub) Monitor(_ context.Context, pod, path string) ([]byte, error) {
	s := h.server(pod)
	if s == nil {
		return nil, fmt.Errorf("no pod %s", pod)
	}
	return monitorGet(fmt.Sprintf("http://127.0.0.1:%d%s", s.monitor, path))
}

// Log is the server's whole output since its start, as `kubectl logs --timestamps` gives the
// running container's: each line after the time it was first seen.
func (h *stagedHub) Log(_ context.Context, pod string) ([]byte, error) {
	s := h.server(pod)
	if s == nil {
		return nil, fmt.Errorf("no pod %s", pod)
	}
	return s.stamps.log(), nil
}

// RouteExchange is routes.Exchange with the server's route port, as a port-forward to 6222 gives it.
func (h *stagedHub) RouteExchange(ctx context.Context, pod string, send []byte) ([]byte, []byte, error) {
	s := h.server(pod)
	if s == nil {
		return nil, nil, fmt.Errorf("no pod %s", pod)
	}
	return routes.Exchange(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(s.route)), send)
}

func (h *stagedHub) Close() error { return nil }

// refusedVarz is a Source whose /varz of one pod is refused, as a port-forward the API server
// refuses leaves it.
type refusedVarz struct {
	routes.Source
	pod string
}

func (r refusedVarz) Monitor(ctx context.Context, pod, path string) ([]byte, error) {
	if pod == r.pod && path == "/varz" {
		return nil, fmt.Errorf("port-forward to %s refused", pod)
	}
	return r.Source.Monitor(ctx, pod, path)
}

// rotatedLog is a Source whose log of one pod has lost its first line, as the newest file of a log
// the kubelet rotated begins after the server's start.
type rotatedLog struct {
	routes.Source
	pod string
}

func (r rotatedLog) Log(ctx context.Context, pod string) ([]byte, error) {
	log, err := r.Source.Log(ctx, pod)
	if err != nil || pod != r.pod {
		return log, err
	}
	_, rest, _ := strings.Cut(string(log), "\n")
	return []byte(rest), nil
}

// startPeer starts a nats-server named name on host, in the hub's cluster by name, soliciting
// routes to the hub's servers on routeTo, and advertising its own route address: on loopback every
// outbound connection comes from 127.0.0.1, so the address a hub server would see on a pod network
// is the one the peer advertises, which the hub's other servers then dial as an implicit route
// (nats-server 2.15.1 server/route.go:859-871). A reload onto authorization closes the routes a
// server accepted, so the peer's own routes close there; the ones the hub's servers dialed stay.
func startPeer(t *testing.T, natsServer, name, host string, routeTo []int) (*cmdtest.Process, int) {
	t.Helper()
	drawn := reservePorts(t, 3)
	var urls []string
	for _, port := range routeTo {
		urls = append(urls, fmt.Sprintf("nats://127.0.0.1:%d", port))
	}
	p := startReady(t, name, exec.Command(natsServer, "-n", name, "-a", host, "-p", strconv.Itoa(drawn[0]), "-m", strconv.Itoa(drawn[1]),
		"--cluster_name", "envoy-nats", "--cluster", "nats://"+net.JoinHostPort(host, strconv.Itoa(drawn[2])),
		"--cluster_advertise", net.JoinHostPort(host, strconv.Itoa(drawn[2])), "--routes", strings.Join(urls, ",")),
		listeningForRoutesOn(host, drawn[2]))
	return p, drawn[1]
}

// peerID is the server id of the peer whose monitoring port is host:monitor.
func peerID(t *testing.T, host string, monitor int) string {
	t.Helper()
	var varz struct {
		ID string `json:"server_id"`
	}
	within(t, 15*time.Second, "the peer's /varz", func() error { return getJSON("http://"+net.JoinHostPort(host, strconv.Itoa(monitor))+"/varz", &varz) })
	return varz.ID
}

// routedTo is whether any hub server's /routez lists a route with ip or remote_id.
func routedTo(h *stagedHub, ip, id string) (bool, error) {
	for _, s := range h.servers {
		if ok, err := routedBy(s, ip, id); err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// routedBy is whether s's /routez lists a route with ip or remote_id.
func routedBy(s *stagedServer, ip, id string) (bool, error) {
	routez, err := routezOf("127.0.0.1", s.monitor)
	if err != nil {
		return false, err
	}
	for _, route := range routez.Routes {
		if route.IP == ip || (id != "" && route.RemoteID == id) {
			return true, nil
		}
	}
	return false, nil
}

// findingNames is whether report has a finding of verdict about pod whose detail holds each of
// parts.
func findingNames(report routes.Report, verdict routes.Verdict, pod string, parts ...string) bool {
	for _, f := range report.Findings {
		if f.Verdict != verdict || (pod != "" && f.Pod != pod) {
			continue
		}
		all := true
		for _, part := range parts {
			all = all && strings.Contains(f.Detail, part)
		}
		if all {
			return true
		}
	}
	return false
}

// TestRouteCheckRefusesARouteThatIsNotTheHubs runs `envoy-infra routes record` and `routes check`'s
// own functions over three local servers that move through routeAuthorization's stages as the hub's
// pods do: started on the env render, recorded, then reloaded onto routes and onto authorization.
//
//   - Clean: the three servers alone pass. Controls, on the same servers: checked after the routes
//     reload alone, every pod's route port INFO lacks auth_required and no reload has loaded
//     authorization, so the check is Unknown, exit 2; a pod whose /varz is refused makes it
//     Unknown, exit 2, never Clean; a pod's log that has lost its first line, as a rotated one has,
//     makes it PasswordRead, naming the pod; and a server restarted after the record makes it
//     PasswordRead, naming the pod, though its new log begins with the start line and names no
//     other address. Every server's log has the authorization reload's line, so the log rule's
//     window opens on every server; the peer rows show it judges the lines in it.
//   - An impostor: a fourth server on 127.0.0.2 named envoy-nats-1 joins as a route during env,
//     stays through both reloads, and the check finds it PasswordRead, exit 1, naming its ip and its
//     remote_id. Control: every route's remote_name, the impostor's included, is one of the hub's
//     names, so a check that trusted names would pass it.
//   - A peer that left: a fifth server on 127.0.0.3 joins during env, stays through both reloads,
//     and leaves before the check; it is in no /routez, and its route connections since the
//     authorization reload in the servers' logs make the check PasswordRead, exit 1, naming its
//     address. Control: no /routez names it, so a check that read /routez alone would pass it.
//   - A peer that left before authorization: the fifth server joins during env and leaves before
//     the routes reload, so no server dialed it with the route password, which an implicit route
//     carries only once authorization applies (route.go:1092-1094); the check is Clean. Control:
//     the servers' logs name its route connections before the authorization reload, so a check that
//     judged the whole log, as one does on every env roll's old pod IPs, would condemn a hub that no
//     reader reached.
//   - A password the authorization reload refuses: with a route password of digits alone, the
//     routes reload loads and the authorization reload fails on every server, which logs the
//     password whole; the check is PasswordRead, exit 1, naming the failed reload, and Unknown that
//     authorization is not in force. Control: each server's log holds the password.
//   - An impostor at a pod's own address: the fourth server on 127.0.0.1, where every hub server
//     listens here as a pod at its pod IP, named envoy-nats-1, is held through both reloads, and
//     the check finds it PasswordRead by remote_id alone. Control: every route's ip is a pod IP and
//     every remote_name a hub server's, so a check by ip and name passes it.
//   - A refused route login from another address: one route CONNECT with no credential from
//     127.0.0.5, answered "Authorization Violation", makes the check Unknown, exit 2, naming it,
//     not PasswordRead. Control: the hub is Clean before it.
//   - Lines a client writes: a client's -ERR texts end as a route connection's line, a failed
//     reload's, a URL with a password and the start line, and the check stays Clean; a log rotated
//     to begin at the forged start line is PasswordRead. Control: the same cut at an ordinary
//     client line is PasswordRead too.
//   - A rollback before authorization: routes, env, routes and authorization; each server whose log
//     holds the password in a "Removed route" line is PasswordRead. Control: such lines exist.
//   - A record taken after authorization is refused.
//   - An empty password: through the stages, every server's route port takes a route login with
//     the user route and no password, so the check is Unknown. Control: a stranger presenting that
//     login joins the hub.
//   - A restart during the check: the last server restarts when the check reads its log, after the
//     check read the pods, and the check is not Clean, naming the change.
func TestRouteCheckRefusesARouteThatIsNotTheHubs(t *testing.T) {
	keys := newLiveHubKeys(t)
	renders := map[string]*clusteredHubRender{}
	for _, stage := range gcp.ClusteredHubRouteStages {
		args := liveHubArgs(keys)
		args.RouteAuthorization = routeAuthorization(stage)
		renders[stage] = renderClusteredHubArgs(t, keys, args)
	}
	natsServer := installTools(t, t.TempDir(), liveTool{natsServerTool, "nats-server"})[0]
	password := "routePassword" + strings.Repeat("Ef", 16)
	ctx := context.Background()
	record := func(t *testing.T, h *stagedHub) routes.Record {
		t.Helper()
		rec, err := routes.Take(ctx, h, "envoy", h.release, time.Now())
		require.NoError(t, err)
		return rec
	}

	t.Run("the hub's servers alone", func(t *testing.T) {
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		h.reload(t, "routes")
		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.Unknown, report.Verdict(), "control: routes alone; findings: %v", report.Findings)
		for _, s := range h.servers {
			require.True(t, findingNames(report, routes.Unknown, s.name, "authorization is not in force: its route port's INFO has no auth_required",
				"no reload has loaded authorization"), "control: %s at routes alone; findings: %v", s.name, report.Findings)
		}
		h.reload(t, "authorization")
		report = routes.Check(ctx, h, rec)
		require.Equal(t, routes.Clean, report.Verdict(), "findings: %v", report.Findings)
		for _, s := range h.servers {
			out := s.proc.Output.String()
			require.Contains(t, out, "- Route connection created", "%s's log names its route connections", s.name)
			require.Equal(t, 1, strings.Count(out, "[INF] Reloaded: cluster\n"), "%s's log has the authorization reload's line once, so the log rule's window opens", s.name)
			require.Equal(t, 1, strings.Count(out, "[INF] Reloaded: cluster routes\n"), "%s's log has the routes reload's line once", s.name)
			require.True(t, strings.HasSuffix(strings.SplitN(out, "\n", 2)[0], "[INF] Starting nats-server"),
				"%s's log begins with the start line", s.name)
		}

		report = routes.Check(ctx, refusedVarz{h, h.release + "-1"}, rec)
		require.Equal(t, routes.Unknown, report.Verdict(), "control: a /varz refused; findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.Unknown, h.release+"-1", "its /varz could not be read"), "findings: %v", report.Findings)

		report = routes.Check(ctx, rotatedLog{h, h.release + "-0"}, rec)
		require.Equal(t, routes.PasswordRead, report.Verdict(), "control: a rotated log; findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.PasswordRead, h.release+"-0", "does not begin with the server's start line"), "findings: %v", report.Findings)

		h.restart(t, h.servers[2])
		report = routes.Check(ctx, h, rec)
		require.Equal(t, routes.PasswordRead, report.Verdict(), "control: a restart since the record; findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.PasswordRead, h.release+"-2", "has run a new container since the record", "restartCount 1"),
			"findings: %v", report.Findings)
		require.Len(t, report.Findings, 1, "the restart alone: its new log begins with the start line and names no other address: %v", report.Findings)
	})

	t.Run("an impostor named as a hub server", func(t *testing.T) {
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		_, monitor := startPeer(t, natsServer, h.release+"-1", "127.0.0.2", h.routePorts[:1])
		impostor := peerID(t, "127.0.0.2", monitor)
		within(t, 30*time.Second, "the impostor in the hub's routes", func() error {
			if ok, err := routedBy(h.servers[2], "127.0.0.2", ""); err != nil || !ok {
				return fmt.Errorf("%s has not dialed the impostor yet (%v)", h.servers[2].name, err)
			}
			return nil
		})
		h.reload(t, "routes")
		h.reload(t, "authorization")
		if ok, err := routedTo(h, "127.0.0.2", impostor); err != nil || !ok {
			t.Fatalf("the impostor left the hub's routes at authorization (%v), so this row tests nothing", err)
		}

		names := map[string]bool{}
		for _, s := range h.servers {
			names[s.name] = true
		}
		for _, s := range h.servers {
			routez, err := routezOf("127.0.0.1", s.monitor)
			require.NoError(t, err)
			for _, route := range routez.Routes {
				require.True(t, names[route.RemoteName], "control: %s's route to %s (%s) bears a name that is no hub server's, so a check by name would catch it",
					s.name, route.RemoteID, route.RemoteName)
			}
		}

		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.PasswordRead, report.Verdict(), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.PasswordRead, "", "ip 127.0.0.2", "remote_id "+impostor), "findings: %v", report.Findings)
	})

	t.Run("a peer that left before the check", func(t *testing.T) {
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		peer, monitor := startPeer(t, natsServer, h.release+"-2", "127.0.0.3", h.routePorts[:1])
		id := peerID(t, "127.0.0.3", monitor)
		within(t, 30*time.Second, "the peer in the hub's routes", func() error {
			if ok, err := routedBy(h.servers[1], "127.0.0.3", ""); err != nil || !ok {
				return fmt.Errorf("%s has not dialed the peer yet (%v)", h.servers[1].name, err)
			}
			return nil
		})
		h.reload(t, "routes")
		h.reload(t, "authorization")
		if ok, err := routedTo(h, "127.0.0.3", id); err != nil || !ok {
			t.Fatalf("the peer left the hub's routes at authorization (%v), so it did not stay to read the password", err)
		}
		require.NoError(t, peer.Cmd.Process.Signal(syscall.SIGKILL))
		peer.WaitExit(t, "SIGKILL")
		within(t, 30*time.Second, "the peer gone from every /routez", func() error {
			if ok, err := routedTo(h, "127.0.0.3", id); err != nil || ok {
				return fmt.Errorf("a hub server still routes to the peer (%v)", err)
			}
			return nil
		})

		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.PasswordRead, report.Verdict(), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.PasswordRead, "", "route connection(s) with 127.0.0.3"), "findings: %v", report.Findings)
		for _, f := range report.Findings {
			require.NotContains(t, f.Detail, "has a route", "control: no /routez names the peer, so a check of /routez alone passes it: %v", f)
		}
	})

	t.Run("a peer that left before authorization", func(t *testing.T) {
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		peer, monitor := startPeer(t, natsServer, h.release+"-2", "127.0.0.3", h.routePorts[:1])
		id := peerID(t, "127.0.0.3", monitor)
		within(t, 30*time.Second, "the peer in the hub's routes", func() error {
			if ok, err := routedBy(h.servers[1], "127.0.0.3", ""); err != nil || !ok {
				return fmt.Errorf("%s has not dialed the peer yet (%v)", h.servers[1].name, err)
			}
			return nil
		})
		require.NoError(t, peer.Cmd.Process.Signal(syscall.SIGKILL))
		peer.WaitExit(t, "SIGKILL")
		within(t, 30*time.Second, "the peer gone from every /routez", func() error {
			if ok, err := routedTo(h, "127.0.0.3", id); err != nil || ok {
				return fmt.Errorf("a hub server still routes to the peer (%v)", err)
			}
			return nil
		})
		h.reload(t, "routes")
		h.reload(t, "authorization")

		named := 0
		for _, s := range h.servers {
			before, _, ok := strings.Cut(s.proc.Output.String(), "[INF] Reloaded: cluster\n")
			require.True(t, ok, "%s's log has the authorization reload's line", s.name)
			for _, line := range strings.Split(before, "\n") {
				if strings.Contains(line, "127.0.0.3:") && strings.HasSuffix(line, "- Route connection created") {
					named++
				}
			}
		}
		require.NotZero(t, named, "control: the servers' logs name the peer's route connections before authorization, so a check of the whole log condemns this hub")

		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.Clean, report.Verdict(), "findings: %v", report.Findings)
	})

	t.Run("a password the authorization reload refuses", func(t *testing.T) {
		digits := strings.Repeat("7", 24) + "1234"
		h := newStagedHub(t, renders, natsServer, digits)
		rec := record(t, h)
		h.reload(t, "routes")
		h.reloadRefused(t, "authorization")
		for _, s := range h.servers {
			held := false
			for _, line := range strings.Split(s.proc.Output.String(), "\n") {
				held = held || (strings.Contains(line, "Failed to reload server configuration") && strings.Contains(line, digits))
			}
			require.True(t, held, "control: %s's failed reload line holds the password", s.name)
		}

		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.PasswordRead, report.Verdict(), "findings: %v", report.Findings)
		for _, s := range h.servers {
			require.True(t, findingNames(report, routes.PasswordRead, s.name, "1 failed reload(s)"), "%s: findings: %v", s.name, report.Findings)
			require.True(t, findingNames(report, routes.Unknown, s.name, "authorization is not in force", "no reload has loaded authorization"),
				"%s: findings: %v", s.name, report.Findings)
		}
	})

	// staged is a hub through env, recorded there, then reloaded onto routes and authorization.
	staged := func(t *testing.T, password string) (*stagedHub, routes.Record) {
		t.Helper()
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		h.reload(t, "routes")
		h.reload(t, "authorization")
		return h, rec
	}

	t.Run("an impostor at a pod's own address", func(t *testing.T) {
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		_, monitor := startPeer(t, natsServer, h.release+"-1", "127.0.0.1", h.routePorts[:1])
		impostor := peerID(t, "127.0.0.1", monitor)
		within(t, 30*time.Second, "the impostor in the hub's routes", func() error {
			if ok, err := routedBy(h.servers[2], "", impostor); err != nil || !ok {
				return fmt.Errorf("%s has not dialed the impostor yet (%v)", h.servers[2].name, err)
			}
			return nil
		})
		h.reload(t, "routes")
		h.reload(t, "authorization")
		if ok, err := routedTo(h, "", impostor); err != nil || !ok {
			t.Fatalf("the impostor left the hub's routes at authorization (%v), so this row tests nothing", err)
		}

		names := map[string]bool{}
		for _, s := range h.servers {
			names[s.name] = true
		}
		for _, s := range h.servers {
			routez, err := routezOf("127.0.0.1", s.monitor)
			require.NoError(t, err)
			for _, route := range routez.Routes {
				require.True(t, route.IP == "127.0.0.1" && names[route.RemoteName],
					"control: %s's route to %s (%s at %s) has an address or a name that is no hub server's, so a check by ip and name would catch it",
					s.name, route.RemoteID, route.RemoteName, route.IP)
			}
		}

		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.PasswordRead, report.Verdict(), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.PasswordRead, "", "ip 127.0.0.1", "remote_id "+impostor), "findings: %v", report.Findings)
		for _, f := range report.Findings {
			require.NotContains(t, f.Detail, "no pod of the hub", "only remote_id tells the impostor apart: %v", f)
		}
	})

	t.Run("a refused route login from another address", func(t *testing.T) {
		h, rec := staged(t, password)
		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.Clean, report.Verdict(), "control: before the login; findings: %v", report.Findings)
		require.Equal(t, "-ERR 'Authorization Violation'", refusedRouteLogin(t, "127.0.0.5", h.servers[0].route), "the credential refuses the login")
		within(t, 10*time.Second, "the refusal in the log", func() error {
			if !strings.Contains(h.servers[0].proc.Output.String(), "] 127.0.0.5:") {
				return fmt.Errorf("%s has not logged the login yet", h.servers[0].name)
			}
			return nil
		})
		report = routes.Check(ctx, h, rec)
		require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.Unknown, h.servers[0].name, "1 route connection(s) with 127.0.0.5 since it loaded authorization",
			"whose login the route credential refused"), "findings: %v", report.Findings)
	})

	t.Run("lines a client writes", func(t *testing.T) {
		h, rec := staged(t, password)
		s := h.servers[1]
		forged := strings.Repeat("A", 3000) + " [INF] Starting nats-server"
		texts := []string{"[INF] 10.66.6.6:6222 - rid:1 - Route connection created", "Failed to reload server configuration: x",
			"nats://route:x@10.9.9.9:6222", forged, "an ordinary line", forged}
		for _, text := range texts {
			require.Equal(t, "PONG", clientLine(t, s.client, text), "a client with no credential is the hub's session user")
		}
		within(t, 10*time.Second, "the client's lines in the log", func() error {
			for _, text := range texts {
				if !slices.ContainsFunc(strings.Split(s.proc.Output.String(), "\n"), func(l string) bool {
					return strings.Contains(l, "[ERR] ") && strings.HasSuffix(l, " - Client Error "+text)
				}) {
					return fmt.Errorf("%s has not logged %.40q yet", s.name, text)
				}
			}
			return nil
		})
		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.Clean, report.Verdict(), "a client's lines meet no rule and trip none; findings: %v", report.Findings)
		report = routes.Check(ctx, rotatedAt{h, forged}, rec)
		require.Equal(t, routes.PasswordRead, report.Verdict(), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.PasswordRead, s.name, "does not begin with the server's start line"), "findings: %v", report.Findings)
		report = routes.Check(ctx, rotatedAt{h, "an ordinary line"}, rec)
		require.True(t, findingNames(report, routes.PasswordRead, s.name, "does not begin with the server's start line"),
			"control: the same cut at an ordinary client line; findings: %v", report.Findings)
	})

	t.Run("a rollback before authorization", func(t *testing.T) {
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		h.reload(t, "routes")
		h.reload(t, "env")
		h.reload(t, "routes")
		h.reload(t, "authorization")
		var holders []string
		for _, s := range h.servers {
			for _, line := range strings.Split(s.proc.Output.String(), "\n") {
				if strings.Contains(line, "[INF] Removed route ") && strings.Contains(line, password) {
					holders = append(holders, s.name)
					break
				}
			}
		}
		require.NotEmpty(t, holders, "control: the rollback's Removed route lines hold the password")
		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.PasswordRead, report.Verdict(), "findings: %v", report.Findings)
		for _, name := range holders {
			require.True(t, findingNames(report, routes.PasswordRead, name, "line(s) the server wrote with a URL that carries a user and a password"),
				"%s: findings: %v", name, report.Findings)
		}
	})

	t.Run("a record taken after authorization", func(t *testing.T) {
		h, _ := staged(t, password)
		_, err := routes.Take(ctx, h, "envoy", h.release, time.Now())
		require.ErrorContains(t, err, "authorization is already in force on "+h.servers[0].name+", "+h.servers[1].name+", "+h.servers[2].name)
	})

	t.Run("an empty password", func(t *testing.T) {
		h, rec := staged(t, "")
		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
		for _, s := range h.servers {
			require.True(t, findingNames(report, routes.Unknown, s.name, "authorization is not in force: its route port took a route login with the user route and no password"),
				"%s: findings: %v", s.name, report.Findings)
		}
		drawn := reservePorts(t, 3)
		startReady(t, "stranger", exec.Command(natsServer, "-n", "stranger", "-a", "127.0.0.7", "-p", strconv.Itoa(drawn[0]), "-m", strconv.Itoa(drawn[1]),
			"--cluster_name", "envoy-nats", "--cluster", "nats://"+net.JoinHostPort("127.0.0.7", strconv.Itoa(drawn[2])),
			"--cluster_advertise", net.JoinHostPort("127.0.0.7", strconv.Itoa(drawn[2])),
			"--routes", fmt.Sprintf("nats://%s:@127.0.0.1:%d", "route", h.servers[0].route)), listeningForRoutesOn("127.0.0.7", drawn[2]))
		within(t, 30*time.Second, "the stranger in the hub's routes", func() error {
			if ok, err := routedTo(h, "127.0.0.7", ""); err != nil || !ok {
				return fmt.Errorf("no hub server holds the stranger yet (%v)", err)
			}
			return nil
		})
	})

	t.Run("a restart during the check", func(t *testing.T) {
		h, rec := staged(t, password)
		last := h.servers[2]
		report := routes.Check(ctx, &restartOnLog{stagedHub: h, t: t, pod: last.name}, rec)
		require.NotEqual(t, routes.Clean, report.Verdict(), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.Unknown, last.name, "changed during the check", "restartCount 0", "restartCount 1"),
			"findings: %v", report.Findings)
		require.Equal(t, int32(1), last.restarts, "control: the server restarted between the check's read of the pods and of its log")
	})

	// pastStartWindow waits until every server of h started more than the start line's window ago,
	// as a reader must have: the record is taken at env, and two applies follow it.
	pastStartWindow := func(h *stagedHub) {
		for _, s := range h.servers {
			if wait := time.Until(s.startedAt.Add(11 * time.Second)); wait > 0 {
				time.Sleep(wait)
			}
		}
	}
	// awaitLine waits until s's output holds line as a line of its own.
	awaitLine := func(t *testing.T, s *stagedServer, line string) {
		t.Helper()
		within(t, 10*time.Second, s.name+" logging a forged line", func() error {
			if !strings.Contains(s.proc.Output.String(), "\n"+line+"\n") {
				return fmt.Errorf("%s has not logged %.60q yet", s.name, line)
			}
			return nil
		})
	}

	t.Run("lines a route peer writes through its server name before authorization", func(t *testing.T) {
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		s := h.servers[0]
		now := time.Now().UTC()
		otherPID := serverLine("4141", now, "INF", removedRouteLine("notThePassword"))
		stale := serverLine(strconv.Itoa(s.proc.Cmd.Process.Pid), now.Add(-time.Hour), "INF", removedRouteLine("notThePassword"))
		f := dialFakeRoute(t, "127.0.0.5", s.route, "", "", "x\n"+otherPID+"\n"+stale)
		f.awaitPong(t)
		f.close()
		awaitLine(t, s, otherPID)
		awaitLine(t, s, stale)
		h.reload(t, "routes")
		h.reload(t, "authorization")
		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.Unknown, report.Verdict(), "control: each forged line is in the log in the logger's form; findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.Unknown, s.name, "2 line(s) in nats-server's form that its server did not write when they say"),
			"findings: %v", report.Findings)
	})

	t.Run("a start line a reader writes through its server name, first in a rotated log", func(t *testing.T) {
		h, rec := staged(t, password)
		pastStartWindow(h)
		forged := map[string]string{
			// timed at the container's start, as a reader that asked the hub's VARZ can
			h.servers[0].name: serverLine(strconv.Itoa(h.servers[0].proc.Cmd.Process.Pid), h.servers[0].startedAt.Add(400*time.Millisecond), "INF", "Starting nats-server"),
			// timed now, so its logger time agrees with the runtime's
			h.servers[1].name: serverLine(strconv.Itoa(h.servers[1].proc.Cmd.Process.Pid), time.Now().UTC(), "INF", "Starting nats-server"),
		}
		for _, s := range h.servers[:2] {
			f := dialFakeRoute(t, "127.0.0.3", s.route, "route", password, "x\n"+forged[s.name])
			f.awaitPong(t)
			f.close()
			awaitLine(t, s, forged[s.name])
		}
		for name, detail := range map[string]string{
			h.servers[0].name: "does not begin with the server's start line",
			h.servers[1].name: "its log's first line, the server's start line, was read by the container runtime at",
		} {
			report := routes.Check(ctx, rotatedAt{h, forged[name]}, rec)
			require.Equal(t, routes.PasswordRead, report.Verdict(), "%s: findings: %v", name, report.Findings)
			require.True(t, findingNames(report, routes.PasswordRead, name, detail), "%s: findings: %v", name, report.Findings)
			for _, f := range report.Findings {
				if f.Pod == name {
					require.NotContains(t, f.Detail, "127.0.0.3", "control: the cut took the reader's route connection lines with it: %v", f)
				}
			}
		}
	})

	t.Run("route connections from other addresses that named no server", func(t *testing.T) {
		h, rec := staged(t, password)
		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.Clean, report.Verdict(), "control: before them; findings: %v", report.Findings)
		require.NoError(t, dialFrom(t, "127.0.0.6", h.servers[1].route).Close())
		dialFrom(t, "127.0.0.7", h.servers[2].route)
		_, err := dialFrom(t, "127.0.0.8", h.servers[0].route).Write([]byte(`CONNECT {"user":"route"`))
		require.NoError(t, err)
		closes := map[*stagedServer]string{h.servers[1]: "127.0.0.6", h.servers[2]: "127.0.0.7", h.servers[0]: "127.0.0.8"}
		within(t, 15*time.Second, "each connection's close", func() error {
			for s, host := range closes {
				closed := slices.ContainsFunc(strings.Split(s.proc.Output.String(), "\n"), func(l string) bool {
					return strings.Contains(l, "] "+host+":") && strings.Contains(l, "Router connection closed: ")
				})
				if !closed {
					return fmt.Errorf("%s has not closed the connection from %s yet", s.name, host)
				}
			}
			return nil
		})
		for s, host := range closes {
			for _, l := range strings.Split(s.proc.Output.String(), "\n") {
				if strings.Contains(l, "] "+host+":") && strings.Contains(l, "Router connection closed: ") {
					require.NotContains(t, l, " - Remote: ", "control: the connection from %s named no server", host)
				}
			}
		}
		report = routes.Check(ctx, h, rec)
		require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
		for s, host := range closes {
			require.True(t, findingNames(report, routes.Unknown, s.name, "1 route connection(s) with "+host+" since it loaded authorization whose peer named no server",
				"1 whose close line names no server"), "%s: findings: %v", host, report.Findings)
		}
	})

	t.Run("a route peer with the password that names itself with an empty name", func(t *testing.T) {
		h, rec := staged(t, password)
		s := h.servers[0]
		f := dialFakeRoute(t, "127.0.0.5", s.route, "route", password, "")
		f.awaitPong(t)
		within(t, 10*time.Second, "the route in the server's /routez", func() error {
			if ok, err := routedBy(s, "127.0.0.5", ""); err != nil || !ok {
				return fmt.Errorf("%s does not hold the route yet (%v)", s.name, err)
			}
			return nil
		})
		f.close()
		within(t, 10*time.Second, "the route's close", func() error {
			if !slices.ContainsFunc(strings.Split(s.proc.Output.String(), "\n"), func(l string) bool {
				return strings.Contains(l, "] 127.0.0.5:"+f.localPort()+" - rid:") && strings.Contains(l, "Router connection closed: ")
			}) {
				return fmt.Errorf("%s has not closed the route yet", s.name)
			}
			return nil
		})
		for _, l := range strings.Split(s.proc.Output.String(), "\n") {
			if strings.Contains(l, "] 127.0.0.5:"+f.localPort()+" - rid:") && strings.Contains(l, "Router connection closed: ") {
				require.NotContains(t, l, " - Remote: ", "control: the close line of a route that formed names no server")
			}
		}
		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.Unknown, s.name, "1 route connection(s) with 127.0.0.5 since it loaded authorization whose peer named no server",
			"can still have carried the password"), "findings: %v", report.Findings)
		for _, finding := range report.Findings {
			require.NotContains(t, finding.Detail, "formed no route", "the route formed, and %s held it: %v", s.name, finding)
		}
	})

	t.Run("a client's line that names a route connection's refused login", func(t *testing.T) {
		h, rec := staged(t, password)
		s := h.servers[0]
		f := dialFakeRoute(t, "127.0.0.5", s.route, "route", password, "rogue")
		f.awaitPong(t)
		defer f.close()
		var rid string
		within(t, 10*time.Second, "the route connection in the log", func() error {
			for _, l := range strings.Split(s.proc.Output.String(), "\n") {
				if _, after, ok := strings.Cut(l, "] 127.0.0.5:"+f.localPort()+" - rid:"); ok && strings.HasSuffix(after, " - Route connection created") {
					rid = strings.TrimSuffix(after, " - Route connection created")
					return nil
				}
			}
			return fmt.Errorf("%s has not logged the fake route's connection yet", s.name)
		})
		forged := "127.0.0.5:" + f.localPort() + " - rid:" + rid + " - authentication error"
		require.Equal(t, "PONG", clientLine(t, s.client, forged), "a client with no credential is the hub's session user")
		within(t, 10*time.Second, "the client's line in the log", func() error {
			if !strings.Contains(s.proc.Output.String(), " - Client Error "+forged+"\n") {
				return fmt.Errorf("%s has not logged the client's line yet", s.name)
			}
			return nil
		})
		report := routes.Check(ctx, h, rec)
		require.True(t, findingNames(report, routes.PasswordRead, s.name, "1 route connection(s) with 127.0.0.5 since it loaded authorization, and"),
			"findings: %v", report.Findings)
		for _, finding := range report.Findings {
			require.NotContains(t, finding.Detail, "named no server", "the client's line is no refusal of the route's login: %v", finding)
		}
	})

	t.Run("a route peer's name that ends as a route connection's", func(t *testing.T) {
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		s := h.servers[0]
		name := "x [INF] 10.66.6.6:6222 - rid:1 - Route connection created"
		f := dialFakeRoute(t, "127.0.0.9", s.route, "", "", name)
		f.awaitPong(t)
		defer f.close()
		h.reload(t, "routes")
		h.reload(t, "authorization")
		within(t, 10*time.Second, "the authorization reload closing the fake route", func() error {
			if !strings.Contains(s.proc.Output.String(), " - Remote: "+name+"\n") {
				return fmt.Errorf("%s has not closed the fake route yet", s.name)
			}
			return nil
		})
		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.Clean, report.Verdict(), "control: the close line, since authorization, ends as a route connection's; findings: %v", report.Findings)
	})
}

// serverLine is a line in nats-server's logger form, as a process of id pid writes it at at.
func serverLine(pid string, at time.Time, level, message string) string {
	return "[" + pid + "] " + at.UTC().Format("2006/01/02 15:04:05.000000") + " [" + level + "] " + message
}

// removedRouteLine is the message a server logs for a route URL a rollback removes, with password
// in the URL. It builds the URL from its parts, and over two lines, so that no line of this source
// holds a URL carrying a user and a password, which the repository's own scan of what it pushes
// reads as a credential.
func removedRouteLine(password string) string {
	url := "nats://" + "route" + ":" + password
	return "Removed route " + url + "@10.0.0.1:6222"
}

// dialFrom opens a TCP connection to port on 127.0.0.1 from local, any 127/8 address, closed when
// the test ends.
func dialFrom(t *testing.T, local string, port int) net.Conn {
	t.Helper()
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(local)}, Timeout: 5 * time.Second}
	conn, err := dialer.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// fakeRoute is a route connection the test speaks itself, as a route peer could: it logs in, and
// its INFO names a server, which nats-server keeps as the route's remote name (route.go:824) and
// prints unquoted when the route closes (client.go:2125), with gossip off. It answers the server's
// PINGs until it is closed or the server closes it.
type fakeRoute struct {
	conn net.Conn
	pong chan struct{}
	done chan struct{}
}

// dialFakeRoute opens a fakeRoute to port from local, logging in with user and pass, and naming
// the server name.
func dialFakeRoute(t *testing.T, local string, port int, user, pass, name string) *fakeRoute {
	t.Helper()
	conn := dialFrom(t, local, port)
	reader := bufio.NewReader(conn)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	first, err := reader.ReadString('\n')
	require.NoError(t, err)
	var info map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(first), "INFO ")), &info))
	for _, key := range []string{"nonce", "compression", "route_pool_size", "route_pool_idx", "connect_urls", "auth_required"} {
		delete(info, key)
	}
	id := "NFAKE" + strings.ToUpper(strconv.FormatInt(time.Now().UnixNano(), 36))
	info["server_id"], info["server_name"], info["gossip_mode"] = id, name, 1
	info["ip"] = "nats-route://" + net.JoinHostPort(local, "1") + "/"
	connect, err := json.Marshal(map[string]any{"echo": true, "verbose": false, "pedantic": false, "user": user, "pass": pass,
		"tls_required": false, "headers": true, "name": id, "cluster": "envoy-nats", "dynamic": false, "lnoc": true})
	require.NoError(t, err)
	infoOut, err := json.Marshal(info)
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	_, err = conn.Write([]byte("CONNECT " + string(connect) + "\r\nINFO " + string(infoOut) + "\r\nPING\r\n"))
	require.NoError(t, err)
	f := &fakeRoute{conn: conn, pong: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(f.done)
		ponged := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			switch strings.TrimSpace(line) {
			case "PING":
				_, _ = conn.Write([]byte("PONG\r\n"))
			case "PONG":
				if !ponged {
					ponged = true
					close(f.pong)
				}
			}
		}
	}()
	return f
}

// awaitPong waits for the server's answer to the route's first PING, which it sends once it has
// taken the route's CONNECT and INFO.
func (f *fakeRoute) awaitPong(t *testing.T) {
	t.Helper()
	select {
	case <-f.pong:
	case <-f.done:
		t.Fatal("the server closed the fake route before it answered its PING")
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not answer the fake route's PING within 10 s")
	}
}

// close closes the fake route and waits for its reader to end.
func (f *fakeRoute) close() {
	_ = f.conn.Close()
	<-f.done
}

// localPort is the port the fake route connects from.
func (f *fakeRoute) localPort() string { return strconv.Itoa(f.conn.LocalAddr().(*net.TCPAddr).Port) }

// refusedRouteLogin opens one route connection to port from local (any 127/8 address), reads the
// INFO, sends a route CONNECT with no credential, and returns the server's answer: the login the
// route credential exists to refuse.
func refusedRouteLogin(t *testing.T, local string, port int) string {
	t.Helper()
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(local)}, Timeout: 5 * time.Second}
	conn, err := dialer.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	reader := bufio.NewReader(conn)
	info, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(info, "INFO "), info)
	_, err = conn.Write([]byte(`CONNECT {"verbose":false,"pedantic":false,"tls_required":false,"name":"stranger","cluster":"envoy-nats"}` + "\r\nPING\r\n"))
	require.NoError(t, err)
	answer, _ := reader.ReadString('\n')
	return strings.TrimSpace(answer)
}

// clientLine connects to a hub server's client port as a client with no credential, which the
// hub's no_auth_user takes, and sends one -ERR, whose text the server logs whole at the end of a
// "Client Error" line (client.go:2268); it returns the server's answer to the CONNECT's PING.
func clientLine(t *testing.T, port int, text string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	reader := bufio.NewReader(conn)
	_, err = reader.ReadString('\n')
	require.NoError(t, err)
	_, err = conn.Write([]byte(`CONNECT {"verbose":false,"pedantic":false,"protocol":1}` + "\r\nPING\r\n"))
	require.NoError(t, err)
	pong, _ := reader.ReadString('\n')
	_, err = conn.Write([]byte("-ERR " + text + "\r\n"))
	require.NoError(t, err)
	_, _ = reader.ReadString('\n')
	return strings.TrimSpace(pong)
}

// rotatedAt is a Source whose log of each pod begins at the last line holding marker, as the
// newest file of a log the kubelet rotated begins at whatever line came after the rotation; a log
// without marker is whole.
type rotatedAt struct {
	routes.Source
	marker string
}

func (r rotatedAt) Log(ctx context.Context, pod string) ([]byte, error) {
	log, err := r.Source.Log(ctx, pod)
	if err != nil {
		return log, err
	}
	at := strings.LastIndex(string(log), r.marker)
	if at < 0 {
		return log, nil
	}
	return log[strings.LastIndex(string(log[:at]), "\n")+1:], nil
}

// restartOnLog is the hub with one server restarted when the check reads its log: after the
// check's read of the pods, and before its second.
type restartOnLog struct {
	*stagedHub
	t   *testing.T
	pod string
	did bool
}

func (r *restartOnLog) Log(ctx context.Context, pod string) ([]byte, error) {
	if pod == r.pod && !r.did {
		r.did = true
		r.restart(r.t, r.server(pod))
	}
	return r.stagedHub.Log(ctx, pod)
}
