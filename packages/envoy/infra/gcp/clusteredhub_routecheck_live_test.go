//go:build natslive

package gcp_test

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/routes"
	"github.com/sjawhar/envoy/internal/testwait"
)

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
//   - A bare connection held open while the log is read: one connection from 127.0.0.9 that sends
//     nothing, opened as the check reads that server's log and held while it does, makes the check
//     Unknown, exit 2, naming it still open, not PasswordRead. Controls: the log the check read
//     names the connection and no close line for it; and the server closes it at the auth timeout,
//     after which the check names it a connection whose close line names no server.
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
//     check read the pods, and the check is Unknown, exit 2, naming the change, and naming the new
//     log's start line, which is the new container's, Unknown, not PasswordRead.
//
// Every row's reload onto authorization waits as a rollout's applies do (beforeAuthorization), more
// than 10 s after each server's start; the check's window opens at that reload's hangup.
//
// Each row runs beside the others (t.Parallel). A row's time is almost all that 11 s wait: of the
// 19, eighteen took 10.9 to 13.0 s and one 0.9 s, 204.8 s in all, while the package kept 0.66 of
// one core of CI's four busy. The wait is the product's own bound and no row shortens it; the
// rows pay it at the same time instead of one after another. What keeps one row from another:
// its servers and its peers' client and monitoring listeners are at loopback addresses of their
// own (testport.LoopbackIP); a route listener, a hub's or a peer's, is at the address the row
// names, often one another row names too, on a port reserved for the row on every address
// (reservePort) until the row ends; and every reading a row makes is of its own servers' /routez,
// /varz and output. What the rows do share is read-only: the renders, the installed nats-server
// and the password below.
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
		t.Parallel()
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
		// A client another test left dialing 127.0.0.1 does not reach a server's client port,
		// which listens at an address of the test's own (requireNoStrayReaches).
		for _, s := range h.servers {
			requireNoStrayReaches(t, s.name, s.host, s.client)
		}
		// A stray client can dial a route port, which listens on 127.0.0.1, the pods' IP here:
		// reservePorts stops a second bind, not a dial. The check reads nothing of it, since it
		// skips a route connection from a pod IP and counts a pod IP's refused route login only
		// when the mesh is split (routes/log.go, routeConnections). Each server logs the stray's
		// refused login, so the stray reached it, and the check stays clean. The wait ends on that
		// line, and fails only at the row's deadline, less 30 s to report: a loaded runner logs
		// late, and nothing else bounds when the line comes.
		for _, s := range h.servers {
			before := strings.Count(s.proc.Output.String(), `authentication error - User "route"`)
			strayRouteLogin(t, s)
			within(t, testwait.Bounded(t, 24*time.Hour, 30*time.Second), s.name+" logging the stray's refused route login", func() error {
				if strings.Count(s.proc.Output.String(), `authentication error - User "route"`) == before {
					return fmt.Errorf("%s has logged no refused route login since a stray dialed its route port at 127.0.0.1:%d", s.name, s.route)
				}
				return nil
			})
		}
		report = routes.Check(ctx, h, rec)
		require.Equal(t, routes.Clean, report.Verdict(), "a stray's refused login at each server's route port on 127.0.0.1 changed the check; findings: %v", report.Findings)

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
		// The new container started with authorization, so its log holds no reload that loaded it:
		// no window, and no route connection judged (routes.authorizationWindow).
		require.True(t, findingNames(report, routes.Unknown, h.release+"-2", `holds no "[INF] Reloaded: cluster"`), "findings: %v", report.Findings)
		require.Len(t, report.Findings, 2, "the restart alone: its new container's record and its log with no window, and nothing else: %v", report.Findings)
	})

	t.Run("an impostor named as a hub server", func(t *testing.T) {
		t.Parallel()
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		_, monitor := startPeer(t, natsServer, h.release+"-1", "127.0.0.2", h.routePorts[:1])
		impostor := peerID(t, monitor)
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
			routez, err := routezOf(s.host, s.monitor)
			require.NoError(t, err)
			for _, route := range routez.Routes {
				require.True(t, names[route.RemoteName], "control: %s's route to %s (%s) bears a name that is no hub server's, so a check by name would catch it",
					s.name, route.RemoteID, route.RemoteName)
			}
		}

		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.PasswordRead, report.Verdict(), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.PasswordRead, "", "ip 127.0.0.2", `remote_id "`+impostor+`"`), "findings: %v", report.Findings)
	})

	t.Run("a peer that left before the check", func(t *testing.T) {
		t.Parallel()
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		peer, monitor := startPeer(t, natsServer, h.release+"-2", "127.0.0.3", h.routePorts[:1])
		id := peerID(t, monitor)
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
		require.True(t, findingNames(report, routes.PasswordRead, "", `route connection(s) with "127.0.0.3"`), "findings: %v", report.Findings)
		for _, f := range report.Findings {
			require.NotContains(t, f.Detail, "has a route", "control: no /routez names the peer, so a check of /routez alone passes it: %v", f)
		}
	})

	t.Run("a peer that left before authorization", func(t *testing.T) {
		t.Parallel()
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		peer, monitor := startPeer(t, natsServer, h.release+"-2", "127.0.0.3", h.routePorts[:1])
		id := peerID(t, monitor)
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
		t.Parallel()
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
		t.Parallel()
		h := newStagedHub(t, renders, natsServer, password)
		rec := record(t, h)
		_, monitor := startPeer(t, natsServer, h.release+"-1", "127.0.0.1", h.routePorts[:1])
		impostor := peerID(t, monitor)
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
		// envoy-nats-1 dials the impostor, whose route port refuses each dial for the duplicate
		// name, and dials again, so its /routez can list a route it is closing, with no ip
		// (routes.go's closedRoute). Such a route has no address to judge here; Check judges its
		// remote_id.
		for _, s := range h.servers {
			routez, err := routezOf(s.host, s.monitor)
			require.NoError(t, err)
			for _, route := range routez.Routes {
				if route.IP == "" {
					continue
				}
				require.True(t, route.IP == "127.0.0.1" && names[route.RemoteName],
					"control: %s's route to %s (%s at %s) has an address or a name that is no hub server's, so a check by ip and name would catch it",
					s.name, route.RemoteID, route.RemoteName, route.IP)
			}
		}

		report := routes.Check(ctx, h, rec)
		require.Equal(t, routes.PasswordRead, report.Verdict(), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.PasswordRead, "", "ip 127.0.0.1", `remote_id "`+impostor+`"`), "findings: %v", report.Findings)
		// A closing route of envoy-nats-1's to the impostor is judged by its remote_id, as every
		// other route of the impostor's is: never as a route from an address outside the hub, and
		// never left unjudged for want of an address.
		for _, f := range report.Findings {
			require.NotContains(t, f.Detail, "no pod of the hub", "only remote_id tells the impostor apart: %v", f)
			require.NotContains(t, f.Detail, "a route with no address", "the check judges a closing route by its remote_id: %v", f)
		}
	})

	t.Run("a refused route login from another address", func(t *testing.T) {
		t.Parallel()
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
		require.True(t, findingNames(report, routes.Unknown, h.servers[0].name, `1 route connection(s) with "127.0.0.5" since it loaded authorization`,
			"whose login the route credential refused"), "findings: %v", report.Findings)
	})

	t.Run("a bare connection held open while the log is read", func(t *testing.T) {
		t.Parallel()
		h, rec := staged(t, password)
		s := h.servers[0]
		bare := &bareOnLog{stagedHub: h, t: t, pod: s.name, from: "127.0.0.9"}
		report := routes.Check(ctx, bare, rec)
		port := strconv.Itoa(bare.conn.LocalAddr().(*net.TCPAddr).Port)
		var rid string
		for _, l := range strings.Split(string(bare.read), "\n") {
			if _, after, ok := strings.Cut(l, "] 127.0.0.9:"+port+" - rid:"); ok && strings.HasSuffix(after, " - Route connection created") {
				rid = strings.TrimSuffix(after, " - Route connection created")
			}
		}
		require.NotEmpty(t, rid, "control: the log the check read names the bare connection")
		require.NotContains(t, string(bare.read), " - rid:"+rid+" - Router connection closed: ", "control: the log the check read has no close line for it")
		require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.Unknown, s.name,
			`1 route connection(s) with "127.0.0.9" since it loaded authorization that were still open when the log was read`), "findings: %v", report.Findings)

		within(t, 15*time.Second, "the auth timeout closing the bare connection", func() error {
			if !strings.Contains(s.proc.Output.String(), " - rid:"+rid+" - Router connection closed: Authentication Timeout") {
				return fmt.Errorf("%s has not closed the bare connection yet", s.name)
			}
			return nil
		})
		report = routes.Check(ctx, h, rec)
		require.True(t, findingNames(report, routes.Unknown, s.name, `1 route connection(s) with "127.0.0.9" since it loaded authorization whose peer named no server`,
			"1 whose close line names no server"), "the server closed it at the auth timeout, as the finding says: %v", report.Findings)
	})

	t.Run("lines a client writes", func(t *testing.T) {
		t.Parallel()
		h, rec := staged(t, password)
		s := h.servers[1]
		forged := strings.Repeat("A", 3000) + " [INF] Starting nats-server"
		texts := []string{"[INF] 10.66.6.6:6222 - rid:1 - Route connection created", "Failed to reload server configuration: x",
			"nats://route:x@10.9.9.9:6222", forged, "an ordinary line", forged}
		for _, text := range texts {
			require.Equal(t, "PONG", clientLine(t, s, text), "a client with no credential is the hub's session user")
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
		t.Parallel()
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
		t.Parallel()
		h, _ := staged(t, password)
		_, err := routes.Take(ctx, h, "envoy", h.release, time.Now())
		require.ErrorContains(t, err, "authorization is already in force on "+h.servers[0].name+", "+h.servers[1].name+", "+h.servers[2].name)
	})

	t.Run("an empty password", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
		h, rec := staged(t, password)
		last := h.servers[2]
		report := routes.Check(ctx, &restartOnLog{stagedHub: h, t: t, pod: last.name}, rec)
		require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.Unknown, last.name, "changed during the check", "restartCount 0", "restartCount 1"),
			"findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.Unknown, last.name, "its container started at", "the pod changed during the check"),
			"its new log's start line is the new container's, and the pod changed during the check: %v", report.Findings)
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
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
			require.True(t, findingNames(report, routes.Unknown, s.name, `1 route connection(s) with "`+host+`" since it loaded authorization whose peer named no server`,
				"1 whose close line names no server"), "%s: findings: %v", host, report.Findings)
		}
	})

	t.Run("a route peer with the password that names itself with an empty name", func(t *testing.T) {
		t.Parallel()
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
		require.True(t, findingNames(report, routes.Unknown, s.name, `1 route connection(s) with "127.0.0.5" since it loaded authorization whose peer named no server`,
			"can still have carried the password"), "findings: %v", report.Findings)
		for _, finding := range report.Findings {
			require.NotContains(t, finding.Detail, "formed no route", "the route formed, and %s held it: %v", s.name, finding)
		}
	})

	t.Run("a client's line that names a route connection's refused login", func(t *testing.T) {
		t.Parallel()
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
		require.Equal(t, "PONG", clientLine(t, s, forged), "a client with no credential is the hub's session user")
		within(t, 10*time.Second, "the client's line in the log", func() error {
			if !strings.Contains(s.proc.Output.String(), " - Client Error "+forged+"\n") {
				return fmt.Errorf("%s has not logged the client's line yet", s.name)
			}
			return nil
		})
		report := routes.Check(ctx, h, rec)
		require.True(t, findingNames(report, routes.Unknown, s.name,
			`1 route connection(s) with "127.0.0.5" since it loaded authorization that were still open when the log was read`), "findings: %v", report.Findings)
		require.True(t, findingNames(report, routes.PasswordRead, s.name, "has a route from an address that is no pod of the hub: ip 127.0.0.5"),
			"the route it holds is in /routez: %v", report.Findings)
		for _, finding := range report.Findings {
			require.NotContains(t, finding.Detail, "named no server", "the client's line is no refusal of the route's login: %v", finding)
		}
	})

	t.Run("a route peer's name that ends as a route connection's", func(t *testing.T) {
		t.Parallel()
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
