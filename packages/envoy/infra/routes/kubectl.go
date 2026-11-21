package routes

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
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// natsContainer is the hub pods' NATS server container, as the nats chart names it.
const natsContainer = "nats"

// Target is the hub a command reads, and how: the namespace and the StatefulSet, the kubectl to
// run and the kubeconfig context to give it, and the pods' monitoring and route ports.
type Target struct {
	Namespace   string
	StatefulSet string
	Kubectl     string
	Context     string
	MonitorPort int
	RoutePort   int
}

// Opener opens the Source a command reads target through.
type Opener func(target Target) (Source, error)

// OpenKubectl is the Source a command reads a live hub through: kubectl, as the caller's
// kubeconfig allows it. It needs get on the StatefulSet and on each of its pods, and create on
// pods/portforward and get on pods/log of each pod, each of which a Role can grant by name.
func OpenKubectl(target Target) (Source, error) {
	path, err := exec.LookPath(target.Kubectl)
	if err != nil {
		return nil, fmt.Errorf("find %s: %w", target.Kubectl, err)
	}
	return &kubectlSource{Target: target, path: path, forwards: map[string]*forward{}}, nil
}

// kubectlSource reads a hub through kubectl: `get statefulset` for its replica count and `get pod`
// for each ordinal pod, a `port-forward` per pod to its monitoring port and one to its route port,
// each held for the check's pass over it, and `logs -c nats`. OpenKubectl makes the one value of
// it that works: its zero value has no forwards map.
type kubectlSource struct {
	Target
	path     string
	mu       sync.Mutex
	forwards map[string]*forward
}

// forward is a running `kubectl port-forward` and the local port it listens on.
type forward struct {
	cmd  *exec.Cmd
	port int
	done chan struct{}
}

// kubectl runs kubectl with args after the context and namespace and returns its stdout, or an
// error carrying its stderr.
func (k *kubectlSource) kubectl(ctx context.Context, args ...string) ([]byte, error) {
	cmd := k.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// command is kubectl with args after the context and namespace, in the caller's environment with
// KUBERC=off. kubectl 1.33 and later read ~/.kube/kuberc, whose defaults set any flag a command
// does not set itself: one for `logs` could cut the log the check reads, and one that sets
// allow-missing-template-keys to false for `config view` makes a jsonpath print the whole object
// it was given, the flattened kubeconfig with its credentials, to stderr.
func (k *kubectlSource) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, k.path, k.args(args...)...)
	cmd.Env = append(os.Environ(), "KUBERC=off")
	return cmd
}

func (k *kubectlSource) args(args ...string) []string {
	var all []string
	if k.Context != "" {
		all = append(all, "--context", k.Context)
	}
	return append(append(all, "--namespace", k.Namespace), args...)
}

// caArgs is the call that reads the context's CA certificate: its certificate-authority-data,
// which `--flatten` gives for a CA held as data or as a file, with a jsonpath of that field alone,
// so no credential of the kubeconfig's user is in its output, and empty, not an error, for a context
// with no CA.
var caArgs = []string{"config", "view", "--minify", "--flatten", "--allow-missing-template-keys=true", "--output",
	"jsonpath={.clusters[0].cluster.certificate-authority-data}"}

// clusterCA reads the context's CA certificate (caArgs). Its error names the command and its exit
// status alone: kubectl's jsonpath printer writes the whole object it was given, the flattened
// kubeconfig with its credentials, to stderr when it fails, so that stderr is read by nothing.
func (k *kubectlSource) clusterCA(ctx context.Context) ([]byte, error) {
	cmd := k.command(ctx, caArgs...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kubectl could not read the context's CA certificate: kubectl %s: %w (its error output is not shown, since it can hold "+
			"the kubeconfig's credentials)", strings.Join(caArgs, " "), err)
	}
	ca, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stdout.String()))
	if err != nil {
		return nil, fmt.Errorf("decode the cluster's certificate-authority-data: %w", err)
	}
	return ca, nil
}

// inClusterContext names the cluster a pod reads through its ServiceAccount, with no kubeconfig.
const inClusterContext = "in-cluster"

// serviceAccountCA is where a pod holds its cluster's CA certificate beside its ServiceAccount's
// token, which client-go's in-cluster configuration reads (k8s.io/client-go/rest, InClusterConfig).
var serviceAccountCA = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

// Cluster reads the context kubectl reads through, the one given or its kubeconfig's current one,
// and that context's API server, from `kubectl config view --minify`, which keeps that context and
// its cluster alone and reads no object of the cluster; it reads the server as kubectl does, and
// refuses one that carries user information or is no URL (clusterServer), naming the context and
// not the value, so no later kubectl call runs against it; and the digest of that cluster's CA
// certificate (caDigest) from its certificate-authority-data, which `--flatten` gives for a CA held
// as data or as a file, and a jsonpath of that field alone keeps the user's credentials out of
// kubectl's output. In a pod with no kubeconfig, as the operator's Job runs, kubectl reads the
// cluster through the pod's ServiceAccount (client-go's in-cluster configuration), and `config
// view --minify` fails, having no current context: with no context given, and
// KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT set, as the kubelet sets them in every pod,
// the cluster is in-cluster, at that address, with the digest of the pod's ServiceAccount CA.
func (k *kubectlSource) Cluster(ctx context.Context) (Cluster, error) {
	out, err := k.kubectl(ctx, "config", "view", "--minify", "--output", "json")
	if err != nil {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if k.Context == "" && host != "" && port != "" {
			ca, caErr := os.ReadFile(serviceAccountCA)
			if caErr != nil && !errors.Is(caErr, os.ErrNotExist) {
				return Cluster{}, fmt.Errorf("read the pod's ServiceAccount CA certificate: %w", caErr)
			}
			return Cluster{Context: inClusterContext, Server: "https://" + net.JoinHostPort(host, port), CA: caDigest(ca)}, nil
		}
		return Cluster{}, err
	}
	var config struct {
		CurrentContext string `json:"current-context"`
		Clusters       []struct {
			Cluster struct {
				Server string `json:"server"`
			} `json:"cluster"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(out, &config); err != nil {
		return Cluster{}, fmt.Errorf("decode kubectl's configuration: %w", err)
	}
	if config.CurrentContext == "" || len(config.Clusters) != 1 || config.Clusters[0].Cluster.Server == "" {
		return Cluster{}, errors.New("kubectl's configuration names no context with one cluster and its server")
	}
	server, err := clusterServer(config.Clusters[0].Cluster.Server)
	if err != nil {
		return Cluster{}, fmt.Errorf("the server of context %q %s", config.CurrentContext, err)
	}
	ca, err := k.clusterCA(ctx)
	if err != nil {
		return Cluster{}, err
	}
	return Cluster{Context: config.CurrentContext, Server: server, CA: caDigest(ca)}, nil
}

// clusterServer reads a kubeconfig cluster's server as kubectl does (k8s.io/client-go/rest,
// DefaultServerURL): a value with no scheme or no host is read again with https:// before it, and
// then holds no path. It refuses a server that carries user information: kubectl sends it as Basic
// auth, and kubectl's errors can print the URL whole, as Go's net/http does for a user with no
// password. Its errors hold no part of the value, since the value can hold a credential.
func clusterServer(server string) (string, error) {
	errNotURL := errors.New("is no URL or host:port, as kubectl reads a server")
	u, err := url.Parse(server)
	if err != nil || u.Scheme == "" || u.Host == "" {
		if u, err = url.Parse("https://" + server); err != nil || (u.Path != "" && u.Path != "/") {
			return "", errNotURL
		}
	}
	if u.User != nil {
		return "", errors.New("carries user information, which kubectl would send and could print: remove it from the kubeconfig, " +
			"and give the user's credentials in its users entry")
	}
	return u.String(), nil
}

// caDigest is the SHA-256, in hex, of a CA certificate's bytes as the client holds them, or empty
// when it holds none: a pod whose ServiceAccount has no CA, or a context with no
// certificate-authority, which trusts the system's roots or skips verification. Those bytes are
// public; the digest is what the record keeps of them.
func caDigest(ca []byte) string {
	if len(ca) == 0 {
		return ""
	}
	sum := sha256.Sum256(ca)
	return hex.EncodeToString(sum[:])
}

// Pods reads the StatefulSet's replica count and the state of its latest roll, and then each of its
// pods by name, <statefulset>-<i>, as a Role that names the pods allows; a list of the namespace's
// pods would need more. A pod is Updated when the roll has finished, the StatefulSet's controller
// having observed its spec's generation, every replica updated and the current revision the update
// revision, and the pod's controller-revision-hash is that revision. Until the controller observes
// a new generation, the status is the last roll's, finished as it may be.
func (k *kubectlSource) Pods(ctx context.Context) ([]Pod, error) {
	out, err := k.kubectl(ctx, "get", "statefulset", k.StatefulSet, "--output", "json")
	if err != nil {
		return nil, err
	}
	var sts struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64  `json:"observedGeneration"`
			UpdatedReplicas    int    `json:"updatedReplicas"`
			CurrentRevision    string `json:"currentRevision"`
			UpdateRevision     string `json:"updateRevision"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &sts); err != nil {
		return nil, fmt.Errorf("decode the StatefulSet %s: %w", k.StatefulSet, err)
	}
	replicas := 1
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}
	rolled := sts.Status.ObservedGeneration == sts.Metadata.Generation && sts.Status.UpdateRevision != "" &&
		sts.Status.CurrentRevision == sts.Status.UpdateRevision && sts.Status.UpdatedReplicas == replicas
	pods := make([]Pod, 0, replicas)
	for i := range replicas {
		name := fmt.Sprintf("%s-%d", k.StatefulSet, i)
		out, err := k.kubectl(ctx, "get", "pod", name, "--output", "json")
		if err != nil {
			return nil, err
		}
		pod, revision, err := decodePod(out)
		if err != nil {
			return nil, fmt.Errorf("decode the pod %s: %w", name, err)
		}
		pod.Updated = rolled && revision == sts.Status.UpdateRevision
		pods = append(pods, pod)
	}
	return pods, nil
}

// The nats container's route variables, gcp:ClusteredNatsHub's routeAuthorization gives it from
// its env stage on: the password from the Secret, and the route URLs that carry it.
const (
	routePasswordEnv = "NATS_ROUTE_PASSWORD"
	routeURLsEnv     = "NATS_ROUTE_URLS"
)

// decodePod is the Pod a `kubectl get pod --output json` answer describes: its Ready condition,
// whether its nats container names both route variables, and its nats container's restart count
// and running start, zero when that container is not running; and the pod's revision, its
// controller-revision-hash label.
func decodePod(body []byte) (Pod, string, error) {
	var pod struct {
		Metadata struct {
			Name   string            `json:"name"`
			UID    string            `json:"uid"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Name string `json:"name"`
				Env  []struct {
					Name string `json:"name"`
				} `json:"env"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			PodIP      string `json:"podIP"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				Name         string `json:"name"`
				RestartCount int32  `json:"restartCount"`
				State        struct {
					Running *struct {
						StartedAt time.Time `json:"startedAt"`
					} `json:"running"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &pod); err != nil {
		return Pod{}, "", err
	}
	p := Pod{Name: pod.Metadata.Name, UID: pod.Metadata.UID, IP: pod.Status.PodIP}
	for _, c := range pod.Status.Conditions {
		if c.Type == "Ready" {
			p.Ready = c.Status == "True"
		}
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != natsContainer {
			continue
		}
		names := map[string]bool{}
		for _, e := range c.Env {
			names[e.Name] = true
		}
		p.RouteEnv = names[routePasswordEnv] && names[routeURLsEnv]
	}
	found := false
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name != natsContainer {
			continue
		}
		found = true
		p.RestartCount = c.RestartCount
		if c.State.Running != nil {
			p.StartedAt = c.State.Running.StartedAt
		}
	}
	if !found {
		p.Ready = false
	}
	return p, pod.Metadata.Labels["controller-revision-hash"], nil
}

// forwardLine is the line `kubectl port-forward --address 127.0.0.1 :<port>` prints once it
// listens, naming the local port it chose.
var forwardLine = regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:(\d+) -> \d+`)

// forwardTimeout is how long a port-forward may take to print its local port.
const forwardTimeout = 30 * time.Second

// forwardTo is the running port-forward to pod's port, started at the first call for that pair.
func (k *kubectlSource) forwardTo(ctx context.Context, pod string, port int) (*forward, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := pod + ":" + strconv.Itoa(port)
	if f, ok := k.forwards[key]; ok {
		return f, nil
	}
	// The forward outlives this call, so it runs under no context of the call's, and Close ends it.
	cmd := k.command(context.Background(), "port-forward", "pod/"+pod, "--address", "127.0.0.1", ":"+strconv.Itoa(port))
	// Its stdout goes through an io.Pipe rather than StdoutPipe, so Wait, which waits for exec's
	// copy into the pipe, may run while the scanner still reads.
	reader, writer := io.Pipe()
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = writer, &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("kubectl port-forward pod/%s: %w", pod, err)
	}
	f := &forward{cmd: cmd, done: make(chan struct{})}
	ports := make(chan int, 1)
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			if match := forwardLine.FindStringSubmatch(scanner.Text()); match != nil {
				port, _ := strconv.Atoi(match[1])
				select {
				case ports <- port:
				default:
				}
			}
		}
		_, _ = io.Copy(io.Discard, reader)
	}()
	go func() {
		_ = cmd.Wait()
		_ = writer.Close()
		close(f.done)
	}()
	timer := time.NewTimer(forwardTimeout)
	defer timer.Stop()
	select {
	case f.port = <-ports:
		k.forwards[key] = f
		return f, nil
	case <-f.done:
		return nil, fmt.Errorf("kubectl port-forward pod/%s exited before it listened: %s", pod, strings.TrimSpace(stderr.String()))
	case <-timer.C:
	case <-ctx.Done():
	}
	_ = cmd.Process.Kill()
	<-f.done
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("kubectl port-forward pod/%s printed no local port within %s", pod, forwardTimeout)
}

// monitorClient is the HTTP client of the monitoring port, through the forward.
var monitorClient = &http.Client{Timeout: 10 * time.Second}

// Monitor answers a GET of path on the pod's monitoring port through its port-forward.
func (k *kubectlSource) Monitor(ctx context.Context, pod, path string) ([]byte, error) {
	f, err := k.forwardTo(ctx, pod, k.MonitorPort)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", f.port, path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := monitorClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s of %s: %w", path, pod, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("GET %s of %s: %w", path, pod, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s of %s: %s", path, pod, resp.Status)
	}
	return body, nil
}

// Log is the pod's nats container log, as `kubectl logs --timestamps` returns it: the running
// container's newest file, each line after the time the container runtime read it at.
func (k *kubectlSource) Log(ctx context.Context, pod string) ([]byte, error) {
	return k.kubectl(ctx, "logs", "pod/"+pod, "--container", natsContainer, "--timestamps")
}

// exchangeTimeout is how long a route port may take to send its INFO and to answer.
const exchangeTimeout = 10 * time.Second

// RouteExchange is Exchange with the pod's route port through its port-forward: the INFO a server
// sends every route connection it accepts at once (route.go:1981, :2097-2100), and its answer to
// send.
func (k *kubectlSource) RouteExchange(ctx context.Context, pod string, send []byte) ([]byte, []byte, error) {
	f, err := k.forwardTo(ctx, pod, k.RoutePort)
	if err != nil {
		return nil, nil, err
	}
	return Exchange(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port)), send)
}

// Exchange dials address and returns the first line it sends; and, when send is not empty, writes
// send and returns the first line after it that is no INFO and no PING, the answer; all within
// exchangeTimeout. The connection is closed after that, so a route login it sends never stays.
func Exchange(ctx context.Context, address string, send []byte) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, nil, err
	}
	reader := bufio.NewReader(conn)
	info, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, nil, fmt.Errorf("read the first line from %s: %w", address, err)
	}
	if len(send) == 0 {
		return info, nil, nil
	}
	if _, err := conn.Write(send); err != nil {
		return info, nil, fmt.Errorf("write to %s: %w", address, err)
	}
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return info, nil, fmt.Errorf("read the answer from %s: %w", address, err)
		}
		if text := strings.TrimSpace(string(line)); text != "PING" && !strings.HasPrefix(text, "INFO ") {
			return info, line, nil
		}
	}
}

// Close stops every port-forward and waits for each to exit.
func (k *kubectlSource) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	var errs []error
	for key, f := range k.forwards {
		if err := f.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			errs = append(errs, fmt.Errorf("stop the port-forward to %s: %w", key, err))
		}
		<-f.done
		delete(k.forwards, key)
	}
	return errors.Join(errs...)
}
