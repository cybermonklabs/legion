//go:build natslive

package gcp_test

// The fixture TestRouteCheckRefusesARouteThatIsNotTheHubs (clusteredhub_routecheck_live_test.go)
// runs on: a staged hub of local nats-servers that moves through routeAuthorization's stages, the
// stamping of each server's output as a container runtime stamps it, the Sources that refuse or
// rotate what the hub gives, and the route peers and clients the scenarios speak to it as.

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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/routes"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testport"
)

// stagedServer is one server of a stagedHub: a nats-server named as a hub pod, on ports of its
// own, on the nats.conf of the stage it has reached, with the route variables a pod has from the
// env stage on. Its client and monitoring ports listen at host, a loopback address of the test's
// own, so a client another test left dialing 127.0.0.1 cannot reach them and write a line into the
// log the check reads (requireNoStrayReaches). Its route port listens on 127.0.0.1, its pod IP here
// (stagedHub.Pods): the check takes a route connection from a pod IP as the hub's own, and a route
// from 127.0.0.1 reads as from 127.0.0.1 on any loopback (on the Linux kernel's loopback every
// connection does, whatever 127/8 address it dials; under gVisor a connection comes from the
// address it dials, here 127.0.0.1). A stray client can dial that route port, since reservePorts
// stops a second bind and not a dial, but the check reads nothing of it: it skips a route
// connection from a pod IP, and counts a pod IP's refused route login only when the mesh is split
// (routes/log.go, routeConnections; the "the hub's servers alone" row probes it). restarts and
// startedAt are what the API server would report for its container: the number of times it was
// started again, and when the running one started. stamps is when each line of its output was
// first seen, as the container runtime stamps each line.
type stagedServer struct {
	name                   string
	dir                    string
	host                   string
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
	host := testport.LoopbackIP().String()
	drawn := reservePortsOn(t, host, 6)
	routePorts := reservePorts(t, 3)
	h := &stagedHub{renders: renders, natsServer: natsServer, release: r.release}
	var urls []string
	for i := range 3 {
		h.routePorts = append(h.routePorts, routePorts[i])
		urls = append(urls, fmt.Sprintf("nats://route:%s@127.0.0.1:%d", password, routePorts[i]))
	}
	h.passwordVar, h.routesVar = "NATS_ROUTE_PASSWORD="+password, "NATS_ROUTE_URLS=["+strings.Join(urls, ", ")+"]"
	for i := range 3 {
		s := &stagedServer{name: fmt.Sprintf("%s-%d", r.release, i), dir: t.TempDir(), host: host, client: drawn[2*i], monitor: drawn[2*i+1], route: routePorts[i]}
		s.args = routeServerArgs(t, r, s.dir, s.host, s.client, s.monitor)
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

// writeConf writes the stage's nats.conf, and auth.conf, into s's directory as its own
// (writeRouteServerConf), and, at env, with the chart's route URLs, which name the pods by their
// headless Service, replaced by the three servers' route ports.
func (h *stagedHub) writeConf(t *testing.T, s *stagedServer, stage string) {
	t.Helper()
	local := map[string]string{}
	if stage == "env" {
		for i, port := range h.routePorts {
			local[fmt.Sprintf("nats://%s-%d.%s-headless:6222", h.release, i, h.release)] = fmt.Sprintf("nats://127.0.0.1:%d", port)
		}
	}
	writeRouteServerConf(t, h.renders[stage], s.dir, "127.0.0.1", s.route, local)
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
// config reloader sends, and waits for each to log it, then for the mesh. Before the reload onto
// authorization it waits as a rollout's applies do (beforeAuthorization).
func (h *stagedHub) reload(t *testing.T, stage string) {
	t.Helper()
	if stage == "authorization" {
		h.beforeAuthorization()
	}
	h.reloadEach(t, stage, "Reloaded server configuration", "Failed to reload server configuration")
	h.awaitMesh(t)
}

// beforeAuthorization waits until every server started more than 11 s ago, so the reload onto
// authorization comes more than the route check's startDelay (10 s) after each container's start.
// A row's peers and reloads before authorization stay out of the check's window, which opens at
// the hangup that begins that reload (routes/log.go, authorizationWindow).
func (h *stagedHub) beforeAuthorization() {
	until := time.Now()
	for _, s := range h.servers {
		if at := s.startedAt.Add(11 * time.Second); at.After(until) {
			until = at
		}
	}
	time.Sleep(time.Until(until))
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
		within(t, 15*time.Second, s.name+"'s /varz", func() error {
			return getJSON("http://"+net.JoinHostPort(s.host, strconv.Itoa(s.monitor))+"/varz", &varz)
		})
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
			routez, err := routezOf(s.host, s.monitor)
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

// Cluster is the one cluster the staged hub stands for.
func (h *stagedHub) Cluster(context.Context) (routes.Cluster, error) {
	return routes.Cluster{Context: "local", Server: "https://127.0.0.1"}, nil
}

// Pods is the three servers as the API server reports the pods: each pod's IP 127.0.0.1, where it
// listens, a uid of its own, a UUID as Kubernetes gives each pod and as routes check requires of a
// record, and its container's restarts and start.
func (h *stagedHub) Pods(context.Context) ([]routes.Pod, error) {
	var pods []routes.Pod
	for i, s := range h.servers {
		pods = append(pods, routes.Pod{Name: s.name, UID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), IP: "127.0.0.1", Ready: true,
			Updated: true, RouteEnv: true, RestartCount: s.restarts, StartedAt: s.startedAt})
	}
	return pods, nil
}

func (h *stagedHub) Monitor(_ context.Context, pod, path string) ([]byte, error) {
	s := h.server(pod)
	if s == nil {
		return nil, fmt.Errorf("no pod %s", pod)
	}
	return monitorGet("http://" + net.JoinHostPort(s.host, strconv.Itoa(s.monitor)) + path)
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

// startPeer starts a nats-server named name whose route port is on routeHost, in the hub's cluster
// by name, soliciting routes to the hub's servers on routeTo, and advertising its own route
// address: the peer dials the hub's route ports at 127.0.0.1, and such a connection comes from
// 127.0.0.1 (on the Linux kernel's loopback whatever address is dialed, under gVisor because it is
// the address dialed), not from the peer's route address, so the address a hub server would see on
// a pod network is the one the peer advertises, which the hub's other servers then dial as an
// implicit route (nats-server 2.15.1 server/route.go:859-871). A reload onto
// authorization closes the routes a server accepted, so the peer's own routes close there; the
// ones the hub's servers dialed stay. Its client and monitoring ports listen at a loopback address
// of its own (testport.LoopbackIP), apart from the route address the rows name it by, so a client
// another test left dialing 127.0.0.1 cannot reach them; it returns the monitoring endpoint.
func startPeer(t *testing.T, natsServer, name, routeHost string, routeTo []int) (*cmdtest.Process, string) {
	t.Helper()
	host := testport.LoopbackIP().String()
	drawn := reservePortsOn(t, host, 2)
	route := reservePorts(t, 1)[0]
	var urls []string
	for _, port := range routeTo {
		urls = append(urls, fmt.Sprintf("nats://127.0.0.1:%d", port))
	}
	p := startReady(t, name, exec.Command(natsServer, "-n", name, "-a", host, "-p", strconv.Itoa(drawn[0]), "-m", strconv.Itoa(drawn[1]),
		"--cluster_name", "envoy-nats", "--cluster", "nats://"+net.JoinHostPort(routeHost, strconv.Itoa(route)),
		"--cluster_advertise", net.JoinHostPort(routeHost, strconv.Itoa(route)), "--routes", strings.Join(urls, ",")),
		listeningForRoutesOn(routeHost, route))
	return p, net.JoinHostPort(host, strconv.Itoa(drawn[1]))
}

// peerID is the server id of the peer whose monitoring endpoint is monitor, a host:port.
func peerID(t *testing.T, monitor string) string {
	t.Helper()
	var varz struct {
		ID string `json:"server_id"`
	}
	within(t, 15*time.Second, "the peer's /varz", func() error { return getJSON("http://"+monitor+"/varz", &varz) })
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
	routez, err := routezOf(s.host, s.monitor)
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

// requireNoStrayReaches fails t, naming the server, when a client dialing 127.0.0.1 at port
// reaches the nats-server named name that listens at host:port: a client another test left
// dialing 127.0.0.1, at a port the kernel has since given this server, must not reach it. A
// listener that answers at 127.0.0.1:port is another program's unless the INFO it sends names
// this server. The control is the same read at host:port, whose INFO must name it, so a read that
// could not see a server's name fails here rather than pass. Off Linux every server listens on
// 127.0.0.1, so there is no other address to dial.
func requireNoStrayReaches(t *testing.T, name, host string, port int) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	named := fmt.Sprintf(`"server_name":%q`, name)
	own := net.JoinHostPort(host, strconv.Itoa(port))
	require.Contains(t, firstLine(t, own), named, "control: the INFO %s sends at %s names it", name, own)
	stray := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", stray, 2*time.Second)
	if err != nil {
		t.Logf("a client at %s reaches nothing (%v); %s listens at %s", stray, err, name, own)
		return
	}
	defer conn.Close()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	info, _ := bufio.NewReader(conn).ReadString('\n')
	require.NotContains(t, info, named, "a client at %s reached %s, which listens at %s", stray, name, own)
	t.Logf("a client at %s reaches another listener, not %s, which listens at %s", stray, name, own)
}

// firstLine is the first line a listener at address sends, its INFO for a nats-server.
func firstLine(t *testing.T, address string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	line, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	return line
}

// strayRouteLogin dials s's route port at 127.0.0.1, its pod IP, as a stray client can, and logs
// in as the route user with a wrong password, which s refuses and logs as an authentication error
// from 127.0.0.1.
func strayRouteLogin(t *testing.T, s *stagedServer) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.route)), 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	reader := bufio.NewReader(conn)
	_, err = reader.ReadString('\n')
	require.NoError(t, err)
	_, err = conn.Write([]byte(`CONNECT {"user":"route","pass":"` + strings.Repeat("s", 32) + `","verbose":false,"pedantic":false,"name":"stray"}` + "\r\nPING\r\n"))
	require.NoError(t, err)
	_, _ = reader.ReadString('\n')
}

// clientLine connects to s's client port as a client with no credential, which the hub's
// no_auth_user takes, and sends one -ERR, whose text the server logs whole at the end of a
// "Client Error" line (client.go:2268); it returns the server's answer to the CONNECT's PING.
func clientLine(t *testing.T, s *stagedServer, text string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(s.host, strconv.Itoa(s.client)), 5*time.Second)
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

// bareOnLog is the hub with a bare connection, which sends nothing, opened from from to one
// server's route port when the check reads that server's log, and held open while it does: what
// any process that reaches the route port can do. It waits for the server to log the connection
// before the log is read, and keeps the log it read.
type bareOnLog struct {
	*stagedHub
	t    *testing.T
	pod  string
	from string
	conn net.Conn
	read []byte
}

func (b *bareOnLog) Log(ctx context.Context, pod string) ([]byte, error) {
	if pod == b.pod && b.conn == nil {
		s := b.server(pod)
		b.conn = dialFrom(b.t, b.from, s.route)
		port := strconv.Itoa(b.conn.LocalAddr().(*net.TCPAddr).Port)
		within(b.t, 10*time.Second, "the bare connection in the log", func() error {
			if !strings.Contains(s.proc.Output.String(), "] "+b.from+":"+port+" - rid:") {
				return fmt.Errorf("%s has not logged the bare connection yet", s.name)
			}
			return nil
		})
	}
	log, err := b.stagedHub.Log(ctx, pod)
	if pod == b.pod {
		b.read = log
	}
	return log, err
}
