package routes_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/routes"
)

// fakeKubectl is a kubectl that answers from the files in $FAKE_KUBECTL_DIR, as the commands
// OpenKubectl's Source runs: `config view --minify`, `get statefulset`, `get pod <name>`, `port-forward pod/<name> ...
// :<port>`, which writes its PID to pid-<name>-<port>, then prints the local port in
// port-<name>-<port> and runs until killed, and `logs pod/<name> --container nats --timestamps`.
// It appends every command line it is given to calls, and refuses any other. It cuts its arguments
// joined into one string first, since bash, unlike dash, cuts ${*#...} from each argument apart. A
// forward writes its PID before it prints its port, since the Source goes on once it reads the port
// and a test may then read the PID; $FAKE_KUBECTL_FORWARD_DELAY, when set, is the seconds a forward
// waits between the two.
const fakeKubectl = `#!/bin/sh
dir="$FAKE_KUBECTL_DIR"
args="$*"
printf '%s\n' "$args" >> "$dir/calls"
case "$args" in
*" config view --minify --output json") exec cat "$dir/config.json" ;;
*" get statefulset envoy-nats --output json") exec cat "$dir/statefulset.json" ;;
*" get pod "*" --output json") pod=${args#* get pod }; pod=${pod%% *}; exec cat "$dir/pod-$pod.json" ;;
*" port-forward pod/"*) pod=${args#* port-forward pod/}; pod=${pod%% *}; remote=${args##*:}
  port=$(cat "$dir/port-$pod-$remote") || exit 1
  echo $$ > "$dir/pid-$pod-$remote"
  [ -z "$FAKE_KUBECTL_FORWARD_DELAY" ] || sleep "$FAKE_KUBECTL_FORWARD_DELAY"
  echo "Forwarding from 127.0.0.1:$port -> $remote"
  echo "Forwarding from [::1]:$port -> $remote"
  exec sleep 300 ;;
*" logs pod/"*" --container nats --timestamps") pod=${args#* logs pod/}; pod=${pod%% *}; exec cat "$dir/log-$pod" ;;
esac
echo "unexpected: $args" >&2
exit 3
`

// kubePod is a pod as `kubectl get pod --output json` prints it, of what the Source reads: its
// revision, the StatefulSet's update revision; its nats container's route variables; its Ready
// condition; and the nats container's restart count and running start beside the reloader's.
func kubePod(name, uid, ip string, ready bool, restarts int, startedAt time.Time) map[string]any {
	status := "False"
	if ready {
		status = "True"
	}
	return map[string]any{
		"kind":     "Pod",
		"metadata": map[string]any{"name": name, "namespace": "envoy", "uid": uid, "labels": map[string]any{"controller-revision-hash": "envoy-nats-new"}},
		"spec": map[string]any{"containers": []any{
			map[string]any{"name": "reloader"},
			map[string]any{"name": "nats", "env": []any{
				map[string]any{"name": "GOMEMLIMIT", "value": "1638MiB"},
				map[string]any{"name": "NATS_ROUTE_PASSWORD", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "envoy-nats-route", "key": "route"}}},
				map[string]any{"name": "NATS_ROUTE_URLS", "value": "[nats://route:$(NATS_ROUTE_PASSWORD)@envoy-nats-0.envoy-nats-headless.envoy.svc.cluster.local.:6222]"},
			}},
		}},
		"status": map[string]any{
			"podIP":      ip,
			"conditions": []any{map[string]any{"type": "PodScheduled", "status": "True"}, map[string]any{"type": "Ready", "status": status}},
			"containerStatuses": []any{
				map[string]any{"name": "reloader", "restartCount": 9, "state": map[string]any{"running": map[string]any{"startedAt": "2026-11-01T00:00:00Z"}}},
				map[string]any{"name": "nats", "restartCount": restarts, "state": map[string]any{"running": map[string]any{"startedAt": startedAt.Format(time.RFC3339)}}},
			},
		},
	}
}

// fakeKubeHub writes the files fakeKubectl answers from for h's pods, each pod's monitoring port a
// local HTTP server answering h's /varz and /routez and its route port a local listener that sends
// h's INFO for it, and returns OpenKubectl's Source over that kubectl, under the kubeconfig context
// example-context.
func fakeKubeHub(t *testing.T, h *fakeHub) (routes.Source, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FAKE_KUBECTL_DIR", dir)
	write := func(name string, body []byte) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), body, 0o600))
	}
	write("kubectl", []byte(fakeKubectl))
	require.NoError(t, os.Chmod(filepath.Join(dir, "kubectl"), 0o700))
	sts, err := json.Marshal(map[string]any{"kind": "StatefulSet", "spec": map[string]any{"replicas": len(h.pods)},
		"status": map[string]any{"replicas": len(h.pods), "updatedReplicas": len(h.pods), "currentRevision": "envoy-nats-new", "updateRevision": "envoy-nats-new"}})
	require.NoError(t, err)
	write("statefulset.json", sts)
	write("config.json", []byte(`{"current-context":"example-context","clusters":[{"name":"example","cluster":{"server":"https://example-context.example.invalid"}}]}`))
	for _, p := range h.pods {
		pod, err := json.Marshal(kubePod(p.Name, p.UID, p.IP, p.Ready, int(p.RestartCount), p.StartedAt))
		require.NoError(t, err)
		write("pod-"+p.Name+".json", pod)
		write("log-"+p.Name, []byte(stamp(h.logs[p.Name])))
		name := p.Name
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := h.Monitor(r.Context(), name, r.URL.Path)
			if err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			_, _ = w.Write(body)
		}))
		t.Cleanup(server.Close)
		u, err := url.Parse(server.URL)
		require.NoError(t, err)
		write("port-"+p.Name+"-8222", []byte(u.Port()))
		write("port-"+p.Name+"-6222", []byte(strconv.Itoa(routePort(t, h, name))))
	}
	src, err := routes.OpenKubectl(routes.Target{
		Namespace: "envoy", StatefulSet: "envoy-nats", Kubectl: filepath.Join(dir, "kubectl"), Context: "example-context", MonitorPort: 8222, RoutePort: 6222,
	})
	require.NoError(t, err)
	return src, dir
}

// routePort listens on a local port that sends each connection pod's route port INFO from h, as it
// stands when the connection comes, then answers a CONNECT and its PING with h's answer to that
// login, and closes the connection; and returns the port.
func routePort(t *testing.T, h *fakeHub, pod string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				info, _, err := h.RouteExchange(context.Background(), pod, nil)
				if err != nil {
					return
				}
				_, _ = conn.Write(info)
				reader := bufio.NewReader(conn)
				connect, err := reader.ReadString('\n')
				if err != nil || !strings.HasPrefix(connect, "CONNECT ") {
					return
				}
				if _, err := reader.ReadString('\n'); err != nil {
					return
				}
				if _, answer, err := h.RouteExchange(context.Background(), pod, []byte(connect)); err == nil {
					_, _ = conn.Write(answer)
				}
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

// OpenKubectl's Source reads a live hub's pods as the operator's Role allows, each by name, its
// monitoring port and its route port through one port-forward each per pod for the whole pass, and
// its nats container's log, each command under the given context and namespace; Close stops every
// forward. Over the clean hub its check is Clean. Controls: a pod whose nats container is not
// running is not Ready, so Take refuses; a pod whose monitoring port-forward exits before it
// listens leaves that pod's /varz unread, so the check is Unknown; a route port whose INFO lacks
// auth_required, and one that takes the route user with no password, make it Unknown, and a log
// rotated past the start makes it PasswordRead, each read through kubectl as the clean one was.
func TestKubectlReadsTheHubThroughKubectl(t *testing.T) {
	h := cleanHub()
	src, dir := fakeKubeHub(t, h)
	back := h.atEnv()
	rec, err := routes.Take(context.Background(), src, "envoy", "envoy-nats", started.Add(time.Minute))
	back()
	require.NoError(t, err)
	require.Equal(t, recordOf(t, h), rec)
	report := routes.Check(context.Background(), src, rec)
	require.Equal(t, routes.Clean, report.Verdict(), "findings: %v", report.Findings)
	cluster, err := src.Cluster(context.Background())
	require.NoError(t, err)
	require.Equal(t, routes.Cluster{Context: "example-context", Server: "https://example-context.example.invalid"}, cluster)

	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	require.NoError(t, err)
	forwards := 0
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		require.True(t, strings.HasPrefix(line, "--context example-context --namespace envoy "), "a call outside the context and namespace: %q", line)
		if strings.Contains(line, " port-forward ") {
			forwards++
		}
	}
	require.Equal(t, 6, forwards, "one port-forward per pod and port for the pass, its /varz and /routez both through the monitoring one:\n%s", calls)
	require.Contains(t, string(calls), "--context example-context --namespace envoy config view --minify --output json\n")
	for _, p := range h.pods {
		require.Contains(t, string(calls), "--context example-context --namespace envoy get pod "+p.Name+" --output json\n")
		require.Contains(t, string(calls), "--context example-context --namespace envoy port-forward pod/"+p.Name+" --address 127.0.0.1 :8222\n")
		require.Contains(t, string(calls), "--context example-context --namespace envoy port-forward pod/"+p.Name+" --address 127.0.0.1 :6222\n")
		require.Contains(t, string(calls), "--context example-context --namespace envoy logs pod/"+p.Name+" --container nats --timestamps\n")
	}

	var pids []int
	for _, p := range h.pods {
		for _, port := range []string{"8222", "6222"} {
			pid, err := os.ReadFile(filepath.Join(dir, "pid-"+p.Name+"-"+port))
			require.NoError(t, err)
			n, err := strconv.Atoi(strings.TrimSpace(string(pid)))
			require.NoError(t, err)
			require.NoError(t, syscall.Kill(n, 0), "the forward to %s:%s runs during the pass", p.Name, port)
			pids = append(pids, n)
		}
	}
	require.NoError(t, src.Close())
	for _, pid := range pids {
		require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "Close stopped the forward %d", pid)
	}

	t.Run("control: a nats container not running", func(t *testing.T) {
		h := cleanHub()
		src, dir := fakeKubeHub(t, h)
		defer src.Close()
		pod := kubePod("envoy-nats-1", "uid-envoy-nats-1", "10.0.0.11", true, 0, started)
		statuses := pod["status"].(map[string]any)["containerStatuses"].([]any)
		statuses[1].(map[string]any)["state"] = map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}
		body, err := json.Marshal(pod)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "pod-envoy-nats-1.json"), body, 0o600))
		_, err = routes.Take(context.Background(), src, "envoy", "envoy-nats", started)
		require.ErrorContains(t, err, "envoy-nats-1 not Ready with a running nats container")
	})
	t.Run("control: a port-forward that exits before it listens", func(t *testing.T) {
		h := cleanHub()
		src, dir := fakeKubeHub(t, h)
		defer src.Close()
		rec := recordOf(t, h)
		require.NoError(t, os.Remove(filepath.Join(dir, "port-envoy-nats-2-8222")))
		report := routes.Check(context.Background(), src, rec)
		require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
		require.Contains(t, fmt.Sprint(report.Findings), "envoy-nats-2: unknown: its /varz could not be read: kubectl port-forward pod/envoy-nats-2 exited before it listened")
	})
	t.Run("control: a route port whose INFO lacks auth_required", func(t *testing.T) {
		h := cleanHub()
		src, _ := fakeKubeHub(t, h)
		defer src.Close()
		rec := recordOf(t, h)
		h.setInfo("envoy-nats-1", authInfo("NSERVER1", false))
		report := routes.Check(context.Background(), src, rec)
		require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
		require.Contains(t, fmt.Sprint(report.Findings), "envoy-nats-1: unknown: authorization is not in force: its route port's INFO has no auth_required")
	})
	t.Run("control: a route port that takes the route user with no password", func(t *testing.T) {
		h := cleanHub()
		src, _ := fakeKubeHub(t, h)
		defer src.Close()
		rec := recordOf(t, h)
		h.setAnswer("envoy-nats-2 empty password", `-ERR 'Rejecting connection from gateway "envoy-infra-routes-probe" on the Route port'`)
		report := routes.Check(context.Background(), src, rec)
		require.Equal(t, routes.Unknown, report.Verdict(), "findings: %v", report.Findings)
		require.Contains(t, fmt.Sprint(report.Findings), "envoy-nats-2: unknown: authorization is not in force: its route port took a route login with the user route and no password")
	})
	t.Run("control: a log rotated past the start", func(t *testing.T) {
		h := cleanHub()
		src, dir := fakeKubeHub(t, h)
		defer src.Close()
		rec := recordOf(t, h)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "log-envoy-nats-0"), []byte(stamp(strings.SplitN(h.logs["envoy-nats-0"], "\n", 2)[1])), 0o600))
		report := routes.Check(context.Background(), src, rec)
		require.Equal(t, routes.PasswordRead, report.Verdict(), "findings: %v", report.Findings)
		require.Contains(t, fmt.Sprint(report.Findings), "envoy-nats-0: password read: its log does not begin with the server's start line")
	})
}

// A fake forward's PID file is there once the Source has read the forward's port, as
// TestKubectlReadsTheHubThroughKubectl reads it then, even with 0.2 s between the PID and the
// port. Control: the order before, the port printed first and the PID written after, fails this
// test every time with the same wait, and failed that test 1 run in 600 under load.
func TestKubectlForwardWritesItsPIDBeforeItListens(t *testing.T) {
	t.Setenv("FAKE_KUBECTL_FORWARD_DELAY", "0.2")
	h := cleanHub()
	src, dir := fakeKubeHub(t, h)
	defer src.Close()
	_, err := src.Monitor(context.Background(), "envoy-nats-1", "/varz")
	require.NoError(t, err)
	_, _, err = src.RouteExchange(context.Background(), "envoy-nats-1", nil)
	require.NoError(t, err)
	for _, port := range []string{"8222", "6222"} {
		pid, err := os.ReadFile(filepath.Join(dir, "pid-envoy-nats-1-"+port))
		require.NoError(t, err, "the forward to :%s wrote its PID before it printed its port", port)
		_, err = strconv.Atoi(strings.TrimSpace(string(pid)))
		require.NoError(t, err)
	}
}

// OpenKubectl refuses a kubectl it cannot find, naming it, before it runs anything.
func TestOpenKubectlNeedsKubectl(t *testing.T) {
	_, err := routes.OpenKubectl(routes.Target{Kubectl: filepath.Join(t.TempDir(), "no-kubectl")})
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no-kubectl"), "%v", err)
}
