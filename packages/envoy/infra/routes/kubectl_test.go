package routes_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// canaryURL is a URL whose user information holds a planted password, built at run time so that
// no URL with a credential stands in this file's text, which a leak check reads.
func canaryURL(scheme, user, password, rest string) string {
	return scheme + "://" + url.UserPassword(user, password).String() + "@" + rest
}

// exampleCA and otherCA are two clusters' CA certificates, as a client holds them, PEM. Neither is a
// valid certificate: the Source hashes the bytes and parses nothing.
const (
	exampleCA = "-----BEGIN CERTIFICATE-----\nMIIBexampleCA\n-----END CERTIFICATE-----\n"
	otherCA   = "-----BEGIN CERTIFICATE-----\nMIIBotherCA\n-----END CERTIFICATE-----\n"
)

// caDigest is the SHA-256 of a CA certificate's bytes, in hex, as a record holds it.
func caDigest(pem string) string {
	sum := sha256.Sum256([]byte(pem))
	return hex.EncodeToString(sum[:])
}

// fakeKubectl is a kubectl that answers from the files in $FAKE_KUBECTL_DIR, as the commands
// OpenKubectl's Source runs: `config view --minify`, from config.json, and with `--flatten` and the
// jsonpath of the cluster's certificate-authority-data, from ca.b64, each failing as kubectl does
// with no kubeconfig when config.json is absent; `get statefulset`, `get pod <name>`, `port-forward pod/<name> ...
// :<port>`, which writes its PID to pid-<name>-<port>, then prints the local port in
// port-<name>-<port> and runs until killed, and `logs pod/<name> --container nats --timestamps`.
// It appends every command line it is given to calls, and its KUBERC to kuberc-env, and refuses
// any other. It cuts its arguments
// joined into one string first, since bash, unlike dash, cuts ${*#...} from each argument apart. A
// forward writes its PID before it prints its port, since the Source goes on once it reads the port
// and a test may then read the PID; $FAKE_KUBECTL_FORWARD_DELAY, when set, is the seconds a forward
// waits between the two.
//
// The CA call fails as kubectl v1.36.4 does: with flatten-fails present, always, its stderr that
// file's text; and for a context with no CA (no ca.b64) under a kuberc that sets
// allow-missing-template-keys to false for `config view` (kuberc-strict present, KUBERC not off),
// unless the call sets the flag itself, its stderr then the object its jsonpath was given, the
// whole flattened kubeconfig, credentials and all, which kuberc-strict holds.
const fakeKubectl = `#!/bin/sh
dir="$FAKE_KUBECTL_DIR"
args="$*"
printf '%s\n' "$args" >> "$dir/calls"
printf '%s\n' "${KUBERC-unset}" >> "$dir/kuberc-env"
case "$args" in
*" config view --minify --output json") [ -f "$dir/config.json" ] || { echo "error: current-context must exist in order to minify" >&2; exit 1; }
  exec cat "$dir/config.json" ;;
*" config view --minify --flatten "*"--output jsonpath={.clusters[0].cluster.certificate-authority-data}") [ -f "$dir/config.json" ] || { echo "error: current-context must exist in order to minify" >&2; exit 1; }
  [ -f "$dir/flatten-fails" ] && { echo "error: $(cat "$dir/flatten-fails")" >&2; exit 1; }
  [ -f "$dir/ca.b64" ] && exec cat "$dir/ca.b64"
  case "$args" in *" --allow-missing-template-keys=true "*) exit 0 ;; esac
  [ -f "$dir/kuberc-strict" ] && [ "$KUBERC" != off ] && { echo "error: error executing jsonpath: certificate-authority-data is not found. object given to jsonpath engine was: $(cat "$dir/kuberc-strict")" >&2; exit 1; }
  exit 0 ;;
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
	write("ca.b64", []byte(base64.StdEncoding.EncodeToString([]byte(exampleCA))))
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
	require.Equal(t, routes.Cluster{Context: "example-context", Server: "https://example-context.example.invalid", CA: caDigest(exampleCA)}, cluster)

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
	require.Contains(t, string(calls), "--context example-context --namespace envoy config view --minify --flatten --allow-missing-template-keys=true --output jsonpath={.clusters[0].cluster.certificate-authority-data}\n")
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

// In a pod that runs as a ServiceAccount with no kubeconfig, as the operator's Job does, kubectl
// reads the cluster through its in-cluster configuration, and `kubectl config view --minify` fails
// ("current-context must exist in order to minify", kubectl v1.36.4). With no --context given and
// KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT set, the Source names the cluster in-cluster,
// at that address, so routes record and routes check run there; a record taken so is refused, exit
// 2, through a kubeconfig context. Controls: with --context given the same failure stays an error,
// and with a kubeconfig its context and cluster are read as before, the variables set or not.
func TestKubectlClusterInAPodWithNoKubeconfig(t *testing.T) {
	ctx := context.Background()
	h := cleanHub()
	src, dir := fakeKubeHub(t, h)
	defer src.Close()
	kubectl := filepath.Join(dir, "kubectl")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	// A pod whose ServiceAccount holds no CA: the record holds none, and compares the context and
	// server alone (TestCheckRefusesARecordOfAnotherClusterAtTheSameAddress holds one).
	routes.SetServiceAccountCA(t, filepath.Join(t.TempDir(), "no-ca.crt"))
	open := func(context string) routes.Source {
		t.Helper()
		s, err := routes.OpenKubectl(routes.Target{Namespace: "envoy", StatefulSet: "envoy-nats", Kubectl: kubectl, Context: context, MonitorPort: 8222, RoutePort: 6222})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	cluster, err := open("").Cluster(ctx)
	require.NoError(t, err)
	require.Equal(t, routes.Cluster{Context: "example-context", Server: "https://example-context.example.invalid", CA: caDigest(exampleCA)}, cluster,
		"control: a kubeconfig is read as before, though the in-cluster variables are set")

	require.NoError(t, os.Remove(filepath.Join(dir, "config.json")))
	cluster, err = open("").Cluster(ctx)
	require.NoError(t, err)
	require.Equal(t, routes.Cluster{Context: "in-cluster", Server: "https://10.96.0.1:443"}, cluster)
	_, err = open("example-context").Cluster(ctx)
	require.ErrorContains(t, err, "current-context must exist in order to minify", "control: a context given is read from a kubeconfig or not at all")

	run := func(args ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := routes.Command(ctx, append(args, "--namespace", "envoy", "--statefulset", "envoy-nats", "--kubectl", kubectl), &stdout, &stderr, routes.OpenKubectl)
		return code, stdout.String(), stderr.String()
	}
	out := filepath.Join(t.TempDir(), "routes.json")
	back := h.atEnv()
	code, stdout, stderr := run("record", "--out", out)
	back()
	require.Equal(t, 0, code, "stderr %q", stderr)
	require.Contains(t, stdout, `through context "in-cluster" of the cluster at "https://10.96.0.1:443"`)
	code, stdout, stderr = run("check", "--record", out)
	require.Equal(t, 0, code, "the check in the pod: stdout %q stderr %q", stdout, stderr)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"current-context":"example-context","clusters":[{"name":"example","cluster":{"server":"https://example-context.example.invalid"}}]}`), 0o600))
	code, _, stderr = run("check", "--record", out, "--context", "example-context")
	require.Equal(t, 2, code, "a record taken in the pod, checked through a kubeconfig context: stderr %q", stderr)
	require.Contains(t, stderr, `is the record taken through context "in-cluster" of the cluster at "https://10.96.0.1:443", not through context "example-context"`)
}

// Two clusters can share the address a record names: in a pod, the in-cluster name is the
// `kubernetes` Service's address, which every cluster of a kind can share (on GKE Autopilot,
// 34.118.224.1), and a kubeconfig context of one name and server can be pointed at another cluster.
// Their CA certificates differ, so the record holds the SHA-256 of the cluster's CA as the client
// holds it, the pod's ServiceAccount ca.crt or the context's certificate-authority-data, and
// routes check refuses a record of another CA, exit 2, naming both. Control: the same CA, exit 0.
func TestCheckRefusesARecordOfAnotherClusterAtTheSameAddress(t *testing.T) {
	for _, inPod := range []bool{true, false} {
		t.Run(map[bool]string{true: "in a pod, through the Service address", false: "through one context and server"}[inPod], func(t *testing.T) {
			ctx := context.Background()
			h := cleanHub()
			src, dir := fakeKubeHub(t, h)
			require.NoError(t, src.Close())
			setCA := func(pem string) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.b64"), []byte(base64.StdEncoding.EncodeToString([]byte(pem))), 0o600))
			}
			if inPod {
				t.Setenv("KUBERNETES_SERVICE_HOST", "34.118.224.1")
				t.Setenv("KUBERNETES_SERVICE_PORT", "443")
				require.NoError(t, os.Remove(filepath.Join(dir, "config.json")))
				caFile := filepath.Join(t.TempDir(), "ca.crt")
				routes.SetServiceAccountCA(t, caFile)
				setCA = func(pem string) {
					t.Helper()
					require.NoError(t, os.WriteFile(caFile, []byte(pem), 0o600))
				}
			}
			run := func(args ...string) (int, string) {
				t.Helper()
				var stdout, stderr bytes.Buffer
				code := routes.Command(ctx, append(args, "--namespace", "envoy", "--statefulset", "envoy-nats", "--kubectl", filepath.Join(dir, "kubectl")),
					&stdout, &stderr, routes.OpenKubectl)
				return code, stdout.String() + stderr.String()
			}
			out := filepath.Join(t.TempDir(), "routes.json")
			setCA(exampleCA)
			back := h.atEnv()
			code, output := run("record", "--out", out)
			back()
			require.Equal(t, 0, code, output)
			code, output = run("check", "--record", out)
			require.Equal(t, 0, code, "control: the same cluster: %s", output)
			setCA(otherCA)
			code, output = run("check", "--record", out)
			require.Equal(t, 2, code, "another cluster at the same address: %s", output)
			require.Contains(t, output, "is the record of a cluster whose CA certificate has the SHA-256 "+caDigest(exampleCA)+
				"; the cluster read now through the same context and server has "+caDigest(otherCA))
			body, err := os.ReadFile(out)
			require.NoError(t, err)
			require.Contains(t, string(body), `"caSHA256": "`+caDigest(exampleCA)+`"`, "the record holds the CA's digest")
		})
	}
}

// kubectlCanary is a planted kubeconfig, as kubectl's jsonpath printer dumps the object it was
// given: the user's token, key data and proxy password are each a canary no output may hold.
var kubectlCanary = `map[string]interface {}{"users":[{"name":"u","user":{"token":"CANARY-TOKEN-INLINE",` +
	`"client-key-data":"CANARY-KEY-DATA"}}],"clusters":[{"name":"c","cluster":{"proxy-url":"` + canaryURL("http", "u", "CANARY-PROXY-PASSWORD", "proxy") + `"}}]}`

// No credential of the kubeconfig reaches routes record's or routes check's output. kubectl 1.33
// and later read ~/.kube/kuberc, whose defaults can set any flag the Source does not set itself:
// one that sets allow-missing-template-keys to false for `config view` makes the CA call fail for a
// context with no CA and print the whole flattened kubeconfig to stderr, and one for `logs` can cut
// the log the check reads. So every kubectl the Source starts runs with KUBERC=off, the CA call
// allows a missing key itself, and no output of that call's stderr is ever shown: its failure
// names the command and its exit status alone.
func TestKubectlKeepsTheKubeconfigsCredentialsOutOfItsOutput(t *testing.T) {
	setup := func(t *testing.T) (*fakeHub, string, func(args ...string) (int, string)) {
		t.Helper()
		h := cleanHub()
		src, dir := fakeKubeHub(t, h)
		require.NoError(t, src.Close())
		run := func(args ...string) (int, string) {
			t.Helper()
			var stdout, stderr bytes.Buffer
			code := routes.Command(context.Background(), append(args, "--namespace", "envoy", "--statefulset", "envoy-nats", "--kubectl",
				filepath.Join(dir, "kubectl")), &stdout, &stderr, routes.OpenKubectl)
			return code, stdout.String() + stderr.String()
		}
		return h, dir, run
	}
	record := func(t *testing.T, h *fakeHub, run func(args ...string) (int, string)) (string, int, string) {
		t.Helper()
		out := filepath.Join(t.TempDir(), "routes.json")
		back := h.atEnv()
		code, output := run("record", "--out", out)
		back()
		return out, code, output
	}
	noCanary := func(t *testing.T, output string) {
		t.Helper()
		for _, canary := range []string{"CANARY-TOKEN-INLINE", "CANARY-KEY-DATA", "CANARY-PROXY-PASSWORD"} {
			require.NotContains(t, output, canary)
		}
	}

	t.Run("every kubectl runs with KUBERC=off, the port-forwards too", func(t *testing.T) {
		h, dir, run := setup(t)
		out, code, output := record(t, h, run)
		require.Equal(t, 0, code, output)
		code, output = run("check", "--record", out)
		require.Equal(t, 0, code, output)
		calls, err := os.ReadFile(filepath.Join(dir, "calls"))
		require.NoError(t, err)
		env, err := os.ReadFile(filepath.Join(dir, "kuberc-env"))
		require.NoError(t, err)
		require.Contains(t, string(calls), " port-forward pod/", "the check's forwards are among the calls")
		lines := strings.Split(strings.TrimSpace(string(env)), "\n")
		require.Len(t, lines, strings.Count(string(calls), "\n"), "one KUBERC per call")
		for i, line := range lines {
			require.Equal(t, "off", line, "call %d: %s", i, strings.Split(string(calls), "\n")[i])
		}
	})
	t.Run("the CA call allows a missing key itself", func(t *testing.T) {
		h, dir, run := setup(t)
		_, code, output := record(t, h, run)
		require.Equal(t, 0, code, output)
		calls, err := os.ReadFile(filepath.Join(dir, "calls"))
		require.NoError(t, err)
		require.Contains(t, string(calls), " config view --minify --flatten --allow-missing-template-keys=true --output jsonpath=")
	})
	t.Run("a kuberc that forbids a missing key, and a context with no CA", func(t *testing.T) {
		h, dir, run := setup(t)
		require.NoError(t, os.Remove(filepath.Join(dir, "ca.b64")))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "kuberc-strict"), []byte(kubectlCanary), 0o600))
		out, code, output := record(t, h, run)
		noCanary(t, output)
		require.Equal(t, 0, code, output)
		body, err := os.ReadFile(out)
		require.NoError(t, err)
		require.Contains(t, string(body), `"caSHA256": ""`, "a context with no CA records none")
		code, output = run("check", "--record", out)
		noCanary(t, output)
		require.Equal(t, 0, code, output)
	})
	t.Run("the CA call fails with the credentials on its stderr", func(t *testing.T) {
		h, dir, run := setup(t)
		out, code, output := record(t, h, run)
		require.Equal(t, 0, code, output)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "flatten-fails"), []byte(kubectlCanary), 0o600))
		for _, args := range [][]string{{"record", "--out", filepath.Join(t.TempDir(), "again.json")}, {"check", "--record", out}} {
			var code int
			var output string
			if args[0] == "record" {
				_, code, output = record(t, h, run)
			} else {
				code, output = run(args...)
			}
			noCanary(t, output)
			require.Equal(t, 2, code, output)
			require.Contains(t, output, "kubectl config view --minify --flatten --allow-missing-template-keys=true --output "+
				"jsonpath={.clusters[0].cluster.certificate-authority-data}: exit status 1", "%s names the command and its exit status", args[0])
		}
	})
}

// OpenKubectl refuses a kubectl it cannot find, naming it, before it runs anything.
func TestOpenKubectlNeedsKubectl(t *testing.T) {
	_, err := routes.OpenKubectl(routes.Target{Kubectl: filepath.Join(t.TempDir(), "no-kubectl")})
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no-kubectl"), "%v", err)
}
