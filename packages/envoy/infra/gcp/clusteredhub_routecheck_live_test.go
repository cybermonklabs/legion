//go:build natslive

package gcp_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
// container: the number of times it was started again, and when the running one started.
type stagedServer struct {
	name                   string
	dir                    string
	client, monitor, route int
	args, vars             []string
	restarts               int32
	startedAt              time.Time
	proc                   *cmdtest.Process
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

func (h *stagedHub) start(t *testing.T, s *stagedServer) {
	t.Helper()
	cmd := exec.Command(h.natsServer, s.args...)
	cmd.Env = append(withoutRouteVars(os.Environ()), s.vars...)
	s.startedAt = time.Now().UTC().Truncate(time.Second)
	s.proc = startReady(t, s.name, cmd, listeningForRoutes(s.route))
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

// Log is the server's whole output since its start, as `kubectl logs` gives the running
// container's.
func (h *stagedHub) Log(_ context.Context, pod string) ([]byte, error) {
	s := h.server(pod)
	if s == nil {
		return nil, fmt.Errorf("no pod %s", pod)
	}
	return []byte(s.proc.Output.String()), nil
}

// RouteInfo is the first line the server's route port sends, as a port-forward to 6222 gives it.
func (h *stagedHub) RouteInfo(ctx context.Context, pod string) ([]byte, error) {
	s := h.server(pod)
	if s == nil {
		return nil, fmt.Errorf("no pod %s", pod)
	}
	return routes.FirstLine(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(s.route)))
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
}
