//go:build natslive

package gcp_test

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// routeLiteral finds a route credential written into nats.conf as a literal: a quoted password
// in the cluster block's authorization, or a route URL that carries a user and password. The
// rendering this component writes names the variables instead, and holds neither.
var routeLiteral = regexp.MustCompile(`"password":\s*"[^"]*"|nats://[^"\s,@/]+:[^"\s,@/]+@`)

// What the chart renders at each stage of routeAuthorization, and what nats-server makes of it.
//
//   - The nats container: env gives it NATS_ROUTE_PASSWORD from the Secret's key, before
//     NATS_ROUTE_URLS, which names it as $(NATS_ROUTE_PASSWORD) so Kubernetes expands it; every
//     later stage keeps both. Without routeAuthorization (the control) neither is there.
//   - nats.conf: at env it is byte for byte the control's, so the env apply rolls the pods and
//     changes no route; routes names $NATS_ROUTE_URLS as the cluster's routes; authorization
//     names the route user and $NATS_ROUTE_PASSWORD as its authorization. No stage writes the
//     credential: routeLiteral finds none in any of them. Control: the chart's own
//     config.cluster.routeURLs.user and password, which this component does not use, write it into
//     nats.conf four times, in each of the three route URLs and in the authorization, and
//     routeLiteral finds all four.
//   - nats-server -t accepts the authorization render with the variables set and refuses it
//     without them, naming the missing variable, which is the rollout's hazard; the env render,
//     the control, passes without them. It also refuses a password of digits alone: nats-server
//     types a variable's value as it types a literal of nats.conf, so such a password is an
//     integer, and the route password holds letters, a letter first (ClusteredHubRouteAuthorizationArgs).
//   - Three servers run on the authorization render, each with its own copy of the variables as a
//     pod has them, form their mesh through the credential, and a server with no credential and
//     one with a wrong password are each refused, every peer logging the authentication failure.
//     Control: the same server with no credential joins a server run on the env render.
func TestClusteredNatsHubRouteAuthorizationRendersAndRuns(t *testing.T) {
	keys := newLiveHubKeys(t)
	control := renderClusteredHubArgs(t, keys, liveHubArgs(keys))
	renders := map[string]*clusteredHubRender{}
	for _, stage := range gcp.ClusteredHubRouteStages {
		args := liveHubArgs(keys)
		args.RouteAuthorization = routeAuthorization(stage)
		renders[stage] = renderClusteredHubArgs(t, keys, args)
	}

	t.Run("the nats container's variables", func(t *testing.T) {
		require.NotContains(t, envNames(t, control), "NATS_ROUTE_PASSWORD", "control: no route variable without routeAuthorization")
		require.NotContains(t, envNames(t, control), "NATS_ROUTE_URLS")
		for stage, r := range renders {
			env := r.container(t, "nats")["env"].([]any)
			names := envNames(t, r)
			password, urls := slices.Index(names, "NATS_ROUTE_PASSWORD"), slices.Index(names, "NATS_ROUTE_URLS")
			require.True(t, password >= 0 && urls > password, "%s: the password's variable comes before the URLs': %v", stage, names)
			require.Equal(t, map[string]any{"name": "NATS_ROUTE_PASSWORD", "valueFrom": map[string]any{
				"secretKeyRef": map[string]any{"name": "envoy-nats-route", "key": "route"},
			}}, env[password], "%s: the password is the Secret's, by reference", stage)
			require.Equal(t, map[string]any{"name": "NATS_ROUTE_URLS", "value": wantRouteEnv()["NATS_ROUTE_URLS"]}, env[urls], stage)
		}
	})

	t.Run("nats.conf names the variables and never the credential", func(t *testing.T) {
		require.Equal(t, control.natsConf, renders["env"].natsConf, "env leaves nats.conf as it was")
		compact := func(stage string) string { return strings.Join(strings.Fields(renders[stage].natsConf), "") }
		require.Contains(t, compact("routes"), `"routes":$NATS_ROUTE_URLS`)
		require.NotContains(t, compact("routes"), `"authorization"`)
		require.Contains(t, compact("authorization"), `"authorization":{"password":$NATS_ROUTE_PASSWORD,"user":"route"}`)
		require.Contains(t, compact("authorization"), `"routes":$NATS_ROUTE_URLS`)
		for stage, r := range renders {
			require.Empty(t, routeLiteral.FindAllString(r.natsConf, -1), "%s writes no route credential:\n%s", stage, r.natsConf)
		}

		planted := "planted" + hex.EncodeToString([]byte(t.Name()))[:16]
		values := chartValues(t, runClusteredHub(t, liveHubArgs(keys)))
		values["config"].(map[string]any)["cluster"].(map[string]any)["routeURLs"] = map[string]any{"user": "route", "password": planted}
		manifests := helmTemplate(t, control.archive, control.release, control.namespace, values)
		literal := (&clusteredHubRender{release: control.release, manifests: manifests}).one(t, "ConfigMap", control.release+"-config")
		found := routeLiteral.FindAllString(literal["data"].(map[string]any)["nats.conf"].(string), -1)
		require.Len(t, found, 4, "control: the chart's routeURLs credential is written into nats.conf, three URLs and the authorization: %v", found)
		for _, f := range found {
			require.Contains(t, f, planted)
		}
	})

	natsServer := installTools(t, t.TempDir(), liveTool{natsServerTool, "nats-server"})[0]
	t.Run("nats-server -t", func(t *testing.T) {
		password := "routePassword" + strings.Repeat("Ab", 16)
		vars := func(password string) []string {
			return []string{"NATS_ROUTE_PASSWORD=" + password, "NATS_ROUTE_URLS=[nats://route:" + password + "@127.0.0.1:6222]"}
		}
		out, err := checkConfig(t, natsServer, renders["authorization"], vars(password))
		require.NoError(t, err, "with the variables set: %s", out)
		out, err = checkConfig(t, natsServer, renders["authorization"], nil)
		require.Error(t, err, "without the variables the render is refused")
		require.Regexp(t, `variable reference for 'NATS_ROUTE_(PASSWORD|URLS)' on line \d+ can not be found`, out)
		out, err = checkConfig(t, natsServer, renders["env"], nil)
		require.NoError(t, err, "control: the env render names no route variable: %s", out)
		out, err = checkConfig(t, natsServer, renders["authorization"], vars(strings.Repeat("7", 32)))
		require.Error(t, err, "a password of digits alone is read as an integer")
		require.Contains(t, out, "variable reference for 'NATS_ROUTE_PASSWORD' on line")
		require.Contains(t, out, "could not be parsed")
	})

	t.Run("the route credential fences the cluster", func(t *testing.T) {
		password := "routePassword" + strings.Repeat("Cd", 16)
		drawn := reservePorts(t, 9)
		var routeURLs []string
		for i := range 3 {
			routeURLs = append(routeURLs, fmt.Sprintf("nats://route:%s@127.0.0.1:%d", password, drawn[3*i+2]))
		}
		r := renders["authorization"]
		servers := make([]*routeServer, 3)
		for i := range servers {
			servers[i] = startRouteServer(t, r, natsServer, fmt.Sprintf("%s-%d", r.release, i), drawn[3*i:3*i+3], []string{
				"NATS_ROUTE_PASSWORD=" + password, "NATS_ROUTE_URLS=[" + strings.Join(routeURLs, ", ") + "]",
			})
		}
		within(t, 30*time.Second, "the three servers' mesh", func() error {
			for _, s := range servers {
				remotes, err := routeRemotesAt("127.0.0.1", s.monitor)
				if err != nil {
					return err
				}
				if want := otherNames(servers, s); !maps.Equal(remotes, want) {
					return fmt.Errorf("%s routes to %v, want %v", s.name, remotes, want)
				}
			}
			return nil
		})

		for _, rogue := range []struct{ name, userinfo, peerLog string }{
			{"no credential", "", "authentication error"},
			{"a wrong password", "route:" + strings.Repeat("w", 32) + "@", `authentication error - User "route"`},
		} {
			t.Run(rogue.name, func(t *testing.T) {
				var targets []string
				for _, s := range servers {
					targets = append(targets, fmt.Sprintf("nats://%s127.0.0.1:%d", rogue.userinfo, s.route))
				}
				before := make([]int, len(servers))
				for i, s := range servers {
					before[i] = strings.Count(s.proc.Output.String(), rogue.peerLog)
				}
				p := startRogue(t, natsServer, "rogue", reservePorts(t, 3), targets)
				within(t, 15*time.Second, "every peer refusing the rogue", func() error {
					for i, s := range servers {
						if strings.Count(s.proc.Output.String(), rogue.peerLog) == before[i] {
							return fmt.Errorf("%s has logged no %q since the rogue started", s.name, rogue.peerLog)
						}
					}
					return nil
				})
				for _, s := range servers {
					remotes, err := routeRemotesAt("127.0.0.1", s.monitor)
					require.NoError(t, err)
					require.NotContains(t, remotes, "rogue", "%s routes to the rogue", s.name)
				}
				stopRogue(t, p)
			})
		}

		t.Run("control: a server with no credential joins the env render", func(t *testing.T) {
			open := startRouteServer(t, renders["env"], natsServer, renders["env"].release+"-0", reservePorts(t, 3), nil)
			p := startRogue(t, natsServer, "rogue", reservePorts(t, 3), []string{fmt.Sprintf("nats://127.0.0.1:%d", open.route)})
			within(t, 15*time.Second, "the rogue in the open server's routes", func() error {
				remotes, err := routeRemotesAt("127.0.0.1", open.monitor)
				if err != nil {
					return err
				}
				if !remotes["rogue"] {
					return fmt.Errorf("%s routes to %v", open.name, remotes)
				}
				return nil
			})
			stopRogue(t, p)
		})
	})
}

// envNames is the names of the hub's nats container's env, in the order the rendered StatefulSet
// lists them.
func envNames(t *testing.T, r *clusteredHubRender) []string {
	t.Helper()
	var names []string
	for _, e := range r.container(t, "nats")["env"].([]any) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	return names
}

// checkConfig runs nats-server -t on r's nats.conf and auth.conf, in a directory of their own, with
// SERVER_NAME set as each pod sets it and vars beside it, and no other NATS_ROUTE_ variable.
func checkConfig(t *testing.T, natsServer string, r *clusteredHubRender, vars []string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nats.conf"), []byte(r.natsConf), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, clusteredHubAuthConfKey), []byte(r.authConf), 0o600))
	cmd := exec.Command(natsServer, "-t", "-c", filepath.Join(dir, "nats.conf"))
	cmd.Env = append(withoutRouteVars(os.Environ()), append([]string{"SERVER_NAME=" + r.release + "-0"}, vars...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// withoutRouteVars is env without any NATS_ROUTE_ variable, so the test's own environment cannot
// supply one.
func withoutRouteVars(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, "NATS_ROUTE_") })
}

// routeServer is one nats-server on a rendered nats.conf whose routes and authorization are the
// file's own (writeRouteServerConf, routeServerArgs).
type routeServer struct {
	name                   string
	client, monitor, route int
	proc                   *cmdtest.Process
}

// writeRouteServerConf writes r's nats.conf and auth.conf into dir as one local server's, the
// pod's store_dir and route port replaced by the server's own, on 127.0.0.1, and each of local's
// keys by its value; the rendered file must set each once. The routes and the authorization stay
// the file's: no command-line flag sets them, since nats-server's --cluster flag clears the file's
// cluster authorization (server/opts.go, overrideCluster).
func writeRouteServerConf(t *testing.T, r *clusteredHubRender, dir string, route int, local map[string]string) {
	t.Helper()
	replace := map[string]string{
		`"store_dir": "/data"`: fmt.Sprintf(`"store_dir": %q`, filepath.Join(dir, "data")),
		`"port": 6222`:         fmt.Sprintf(`"host": "127.0.0.1", "port": %d`, route),
	}
	maps.Copy(replace, local)
	conf := r.natsConf
	for old, value := range replace {
		require.Equal(t, 1, strings.Count(conf, old), "the rendered nats.conf sets %s once", old)
		conf = strings.Replace(conf, old, value, 1)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nats.conf"), []byte(conf), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, clusteredHubAuthConfKey), []byte(r.authConf), 0o600))
}

// routeServerArgs are r's nats container's arguments for a local server whose configuration
// writeRouteServerConf wrote into dir: the pod's configuration directory dir, and its client and
// monitoring ports and PID file, on 127.0.0.1.
func routeServerArgs(t *testing.T, r *clusteredHubRender, dir string, client, monitor int) []string {
	t.Helper()
	return append(podArgs(t, stringList(t, r.container(t, "nats")["args"]), map[string]string{"/etc/nats-config/": dir + "/"}),
		"-a", "127.0.0.1", "-p", strconv.Itoa(client), "-m", strconv.Itoa(monitor), "-P", filepath.Join(dir, "nats.pid"))
}

// startRouteServer starts a server named name on r's nats.conf and auth.conf, on the three ports
// drawn, with vars in its environment beside SERVER_NAME.
func startRouteServer(t *testing.T, r *clusteredHubRender, natsServer, name string, drawn []int, vars []string) *routeServer {
	t.Helper()
	dir := t.TempDir()
	writeRouteServerConf(t, r, dir, drawn[2], nil)
	s := &routeServer{name: name, client: drawn[0], monitor: drawn[1], route: drawn[2]}
	cmd := exec.Command(natsServer, routeServerArgs(t, r, dir, drawn[0], drawn[1])...)
	cmd.Env = append(withoutRouteVars(os.Environ()), append([]string{"SERVER_NAME=" + name}, vars...)...)
	s.proc = startReady(t, name, cmd, listeningForRoutes(drawn[2]))
	return s
}

// startRogue starts a nats-server named name with no configuration file and no JetStream, in the
// hub's cluster by name, soliciting routes, on the three reserved ports drawn.
func startRogue(t *testing.T, natsServer, name string, drawn []int, routes []string) *cmdtest.Process {
	t.Helper()
	return startReady(t, name, exec.Command(natsServer, "-n", name, "-a", "127.0.0.1", "-p", strconv.Itoa(drawn[0]), "-m", strconv.Itoa(drawn[1]),
		"--cluster_name", "envoy-nats", "--cluster", fmt.Sprintf("nats://127.0.0.1:%d", drawn[2]), "--routes", strings.Join(routes, ",")),
		listeningForRoutes(drawn[2]))
}

// stopRogue stops the rogue and waits for it to exit, so the next row starts with no rogue left.
func stopRogue(t *testing.T, p *cmdtest.Process) {
	t.Helper()
	require.NoError(t, p.Cmd.Process.Kill())
	p.WaitExit(t, "SIGKILL")
}

// listeningForRoutes is ready once the server's output says it listens on its route port on
// 127.0.0.1, its last bind.
func listeningForRoutes(port int) func(*cmdtest.Process) error {
	return listeningForRoutesOn("127.0.0.1", port)
}

// listeningForRoutesOn is ready once the server's output says it listens on its route port on host,
// its last bind.
func listeningForRoutesOn(host string, port int) func(*cmdtest.Process) error {
	return func(p *cmdtest.Process) error {
		if !strings.Contains(p.Output.String(), "Listening for route connections on "+net.JoinHostPort(host, strconv.Itoa(port))) {
			return errors.New("not listening yet")
		}
		return nil
	}
}

// otherNames is the set of the names of servers but s.
func otherNames(servers []*routeServer, s *routeServer) map[string]bool {
	names := map[string]bool{}
	for _, other := range servers {
		if other != s {
			names[other.name] = true
		}
	}
	return names
}
