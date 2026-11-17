// Package buckets is `envoy-infra buckets`: the one command that creates the buckets a listener
// copies (bus.CopiedBuckets), and recreates one, once it has proven that no writer of them runs.
//
// A write such a bucket acknowledged is lost when the bucket is deleted and created again, and the
// listener that made it told its caller it landed: the ack names the stream and its sequence, never
// the bucket's incarnation. So no listener creates one (INFRA-4094), and this command refuses
// while any listener of the deployment runs, and, for the CI state bucket, any envoy-dispatch,
// which writes it too. A listener that finds a bucket missing serves its writes 503 and names the
// bucket on /healthz until this command recreates it.
//
// The proof that each writer ended is the process-end evidence of dispatch://INFRA-4015's plan
// (section 6), as far as it is built here, and never a /connz count alone:
//
//   - Kubernetes, for each --kube-context: no pod the API lists runs a writer container that has
//     not terminated, and no node's kubelet runtime list (/api/v1/nodes/<node>/proxy/runningpods/)
//     holds one either, since a pod deleted with --force leaves its containers running after the
//     API forgets it. A node whose runtime list does not answer blocks.
//   - Hosts, for each --host, through ssh (or --ssh, a program taking ssh's arguments): no process
//     on the host is named envoy-listener (or envoy-dispatch). An unreachable host blocks, and so
//     does a query that did not run to its end or enumerated no process at all.
//   - The census, for each --connz (a hub server's monitoring URL): no connection named as a
//     writer's is open. It can only add a blocker: an empty census proves no process absent.
//
// A first deployment, whose hub has never served a listener, passes --first-deployment instead
// of an inventory: the server must hold neither the notification stream, which every listener and
// envoy-dispatch create at their first connect, nor any of the buckets.
package buckets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// Runner runs a program with args and returns its standard output; an error carries what it wrote
// to standard error. The command runs kubectl and ssh through it.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// execRunner runs the program for real.
func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Command runs `envoy-infra buckets` with args, the words after "buckets", and returns its exit
// code: 0 done, 1 refused or failed, 2 a usage error.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return command(ctx, args, stdout, stderr, execRunner, http.DefaultClient)
}

// operatorEnvironment is the environment the command connects with: the process's own, but for
// ENVOY_ALLOW_REMOTE_NATS, which it always answers "1". The command is the operator's, run from a
// workstation against the deployment's hub on another machine, which bus.Connect refuses unless
// that variable says the run means it (bus.AllowRemoteEnvVar); --nats names the hub explicitly.
func operatorEnvironment(name string) (string, bool) {
	if name == bus.AllowRemoteEnvVar {
		return "1", true
	}
	return os.LookupEnv(name)
}

// InboxPrefix is the prefix the command's reply inboxes take (bus.WithInboxPrefix), so the
// deployment's buckets user is granted subscribe on InboxPrefix.> alone: a user granted _INBOX.>
// would receive every other client's replies, a session's push deliveries among them.
const InboxPrefix = "_INBOX_envoy_buckets"

const usage = `usage: envoy-infra buckets create|recreate|check --nats URL [options]

  create     create every copied bucket that is missing (or each --bucket), once no writer runs
  recreate   delete each --bucket and create it again, once no writer runs
  check      prove no writer runs, and change nothing

The writers are every envoy-listener of the deployment, and every envoy-dispatch when the CI state
bucket is among the buckets. The proof is required before anything changes:
  --kube-context CTX   a Kubernetes context to check, repeatable ("" is kubectl's current one)
  --host TARGET        a machine to query through ssh, repeatable
  --ssh PROGRAM        the program run as ssh (default ssh), such as a wrapper over SSM
  --connz URL          a hub server's monitoring URL whose connections to read, repeatable
  --first-deployment   instead of an inventory: the server holds no notification stream and no bucket

  --nats URL           the NATS server, as a user that may create and delete the buckets'
                       streams (NATS_NKEY_SEED_FILE or NATS_NKEY_SEED in the environment)
  --bucket NAME        a copied bucket to act on, repeatable (default: every one, for create)
  --replicas N         the copies a bucket is created with (default ENVOY_NATS_REPLICAS, else 1)
`

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func command(ctx context.Context, args []string, stdout, stderr io.Writer, run Runner, client *http.Client) int {
	if len(args) == 0 || (args[0] != "create" && args[0] != "recreate" && args[0] != "check") {
		help := len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help")
		fmt.Fprint(stderr, usage)
		if help {
			return 0
		}
		return 2
	}
	action := args[0]
	flags := flag.NewFlagSet("envoy-infra buckets "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var f fence
	var buckets stringList
	var natsURL string
	replicas := 0
	flags.Var((*stringList)(&f.kubeContexts), "kube-context", "a Kubernetes context to check")
	flags.Var((*stringList)(&f.hosts), "host", "a machine to query through ssh")
	flags.StringVar(&f.ssh, "ssh", "ssh", "the program run as ssh")
	flags.Var((*stringList)(&f.connz), "connz", "a hub server's monitoring URL")
	flags.BoolVar(&f.firstDeployment, "first-deployment", false, "the server has never served a listener")
	flags.StringVar(&natsURL, "nats", "", "the NATS server")
	flags.Var(&buckets, "bucket", "a copied bucket")
	flags.IntVar(&replicas, "replicas", 0, "the copies a bucket is created with")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "envoy-infra buckets: unexpected argument %q\n%s", flags.Arg(0), usage)
		return 2
	}
	if natsURL == "" {
		fmt.Fprintf(stderr, "envoy-infra buckets: --nats is required\n%s", usage)
		return 2
	}
	for _, bucket := range buckets {
		if _, ok := bus.CopiedBucketConfig(bucket, 1); !ok {
			fmt.Fprintf(stderr, "envoy-infra buckets: %s is no bucket a listener copies (%s)\n", bucket, strings.Join(bus.CopiedBuckets(), ", "))
			return 2
		}
	}
	if action == "recreate" && len(buckets) == 0 {
		fmt.Fprintln(stderr, "envoy-infra buckets: recreate needs each --bucket it deletes")
		return 2
	}
	if len(buckets) == 0 {
		buckets = bus.CopiedBuckets()
	}
	if replicas == 0 {
		replicas = 1
		if value := os.Getenv(bus.ReplicasEnvVar); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				fmt.Fprintf(stderr, "envoy-infra buckets: %s=%q is no replica count\n", bus.ReplicasEnvVar, value)
				return 2
			}
			replicas = n
		}
	}
	if !f.firstDeployment && len(f.kubeContexts) == 0 && len(f.hosts) == 0 {
		fmt.Fprintln(stderr, "envoy-infra buckets: name every place a writer of the deployment runs: --kube-context for each cluster and --host for each machine (or --first-deployment for a hub that never served one)")
		return 2
	}
	f.run, f.http = run, client
	f.dispatch = slices.Contains(buckets, bus.CIStateBucket)

	nc, err := bus.Connect([]string{natsURL}, bus.WithName("envoy-infra-buckets"), bus.WithEnvironment(operatorEnvironment), bus.WithInboxPrefix(InboxPrefix))
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra buckets: connect to %s: %v\n", natsURL, err)
		return 1
	}
	defer nc.Close()
	js := nc.JS()

	if blockers := f.prove(ctx, stdout, js); len(blockers) > 0 {
		refuse(stderr, action, blockers, "")
		return 1
	}
	switch action {
	case "check":
		fmt.Fprintln(stdout, "proven: no writer of the buckets runs")
		return 0
	case "create":
		return create(stdout, stderr, nc.Conn, js, buckets, replicas)
	}
	for _, bucket := range buckets {
		if err := js.DeleteKeyValue(bucket); err != nil && !errors.Is(err, nats.ErrBucketNotFound) && !errors.Is(err, nats.ErrStreamNotFound) {
			fmt.Fprintf(stderr, "envoy-infra buckets: delete %s: %v\n", bucket, err)
			return 1
		}
		fmt.Fprintf(stdout, "deleted %s\n", bucket)
	}
	// A writer that started between the proof and the delete may have written to the deleted
	// bucket, so the proof is taken again before anything is created.
	if blockers := f.prove(ctx, stdout, js); len(blockers) > 0 {
		refuse(stderr, "create", blockers, "the buckets are deleted and not created: stop each writer above, then run `envoy-infra buckets create`")
		return 1
	}
	return create(stdout, stderr, nc.Conn, js, buckets, replicas)
}

// refuse writes the refusal and each blocker.
func refuse(stderr io.Writer, action string, blockers []string, after string) {
	fmt.Fprintf(stderr, "envoy-infra buckets %s: refused: a writer of the buckets may still run, and a bucket created under it loses every write the deleted one acknowledged (INFRA-4094)\n", action)
	for _, blocker := range blockers {
		fmt.Fprintf(stderr, "  blocked: %s\n", blocker)
	}
	if after != "" {
		fmt.Fprintf(stderr, "  %s\n", after)
	}
}

// create creates each bucket of buckets that is missing, with its copied-bucket configuration.
func create(stdout, stderr io.Writer, conn *nats.Conn, js nats.JetStreamContext, buckets []string, replicas int) int {
	for _, bucket := range buckets {
		if _, err := js.KeyValue(bucket); err == nil {
			fmt.Fprintf(stdout, "%s exists; left as it is\n", bucket)
			continue
		} else if !errors.Is(err, nats.ErrBucketNotFound) {
			fmt.Fprintf(stderr, "envoy-infra buckets: read %s: %v\n", bucket, err)
			return 1
		}
		if _, err := bus.CreateCopiedBucket(conn, js, bucket, replicas); err != nil {
			fmt.Fprintf(stderr, "envoy-infra buckets: create %s: %v\n", bucket, err)
			return 1
		}
		fmt.Fprintf(stdout, "created %s (%d copies)\n", bucket, replicas)
	}
	return 0
}

// fence is the proof that no writer of the buckets runs.
type fence struct {
	kubeContexts    []string
	hosts           []string
	ssh             string
	connz           []string
	firstDeployment bool
	// dispatch says envoy-dispatch is a writer too: the CI state bucket is among the buckets.
	dispatch bool
	run      Runner
	http     *http.Client
}

// writerContainers are the container names a writer runs as in this repository's Kubernetes
// workloads: the clustered hub's listener (infra/gcp/clusteredhub_listener.go), Dispatch's
// listener and Dispatch itself (infra/gcp/dispatch_kube.go).
func (f fence) writerContainers() []string {
	names := []string{"listener", "envoy-listener"}
	if f.dispatch {
		names = append(names, "envoy-dispatch")
	}
	return names
}

// writerProcesses are the names a writer's process runs as on a machine (its /proc/<pid>/comm, the
// executable's name cut to 15 bytes), as shell case patterns: every name this repository builds or
// runs the listener as - envoy-listener (docker/Dockerfile, the plugins, every e2e script),
// listener (`go run ./cmd/listener`, scripts/dev-stack.sh, scripts/e2e-refill.sh) and the held-port
// wrapper's envoy-listener-heldport, whose comm is cut to envoy-listener- - and, when the CI state
// bucket is among the buckets, every name it runs Dispatch as: envoy-dispatch, dispatch, and the
// dispatch-<build> names of the e2e scripts' two-build comparisons (dispatch-e2e, dispatch-head,
// dispatch-base). A binary renamed to anything else is not recognised; that is one of the ways a
// writer gets past this proof (bestEffortWarning).
func (f fence) writerProcesses() []string {
	names := []string{"envoy-listener", "envoy-listener-*", "listener"}
	if f.dispatch {
		names = append(names, "envoy-dispatch", "dispatch", "dispatch-*")
	}
	return names
}

// writerConnections are the names a writer's NATS connections carry: every listener's role lane
// and its session consumers' (internal/bus), and Dispatch's. The listener's main connection carries
// the bus's default name, which other programs share, so the role lane, which every listener
// opens, is what names it.
func (f fence) writerConnections() []string {
	names := []string{"envoy-role-lane", "envoy-session-consumers"}
	if f.dispatch {
		names = append(names, "dispatch")
	}
	return names
}

// prove takes every proof the fence was given and returns what blocks: nothing when every writer
// is proven ended. It writes each proof it took to out.
func (f fence) prove(ctx context.Context, out io.Writer, js nats.JetStreamContext) []string {
	var blockers []string
	if f.firstDeployment {
		blockers = append(blockers, firstDeploymentBlockers(js)...)
		if len(blockers) == 0 {
			fmt.Fprintln(out, "proven: the server holds no notification stream and no copied bucket, so no listener has connected to it")
		}
	}
	for _, kube := range f.kubeContexts {
		blockers = append(blockers, f.provePods(ctx, out, kube)...)
	}
	for _, host := range f.hosts {
		blockers = append(blockers, f.proveHost(ctx, out, host)...)
	}
	for _, monitor := range f.connz {
		blockers = append(blockers, f.census(ctx, out, monitor)...)
	}
	return blockers
}

// firstDeploymentBlockers is what keeps a server from being a first deployment's: the
// notification stream, which every listener and envoy-dispatch create at their first connect, or
// any copied bucket.
func firstDeploymentBlockers(js nats.JetStreamContext) []string {
	var blockers []string
	for _, stream := range append([]string{bus.Stream}, prefixed(bus.CopiedBuckets())...) {
		_, err := js.StreamInfo(stream)
		switch {
		case errors.Is(err, nats.ErrStreamNotFound):
		case err != nil:
			blockers = append(blockers, fmt.Sprintf("--first-deployment: could not read %s: %v", stream, err))
		default:
			blockers = append(blockers, fmt.Sprintf("--first-deployment: the server holds %s, so a writer has run against it: name the writers to prove ended instead", stream))
		}
	}
	return blockers
}

func prefixed(buckets []string) []string {
	streams := make([]string, len(buckets))
	for i, bucket := range buckets {
		streams[i] = "KV_" + bucket
	}
	return streams
}

// kubectl runs kubectl in context kube ("" for the current one).
func (f fence) kubectl(ctx context.Context, kube string, args ...string) ([]byte, error) {
	if kube != "" {
		args = append([]string{"--context", kube}, args...)
	}
	return f.run(ctx, "kubectl", args...)
}

// podList is the part of a Kubernetes PodList the fence reads, from the API and from a kubelet's
// runtime list alike.
type podList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			UID       string `json:"uid"`
		} `json:"metadata"`
		Spec struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Name string `json:"name"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			ContainerStatuses []struct {
				Name  string `json:"name"`
				State struct {
					Terminated *struct{} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// provePods proves, for one Kubernetes context, that no writer container runs: none in a pod the
// API lists that has not terminated, and none in any node's kubelet runtime list. Each read that
// fails blocks.
func (f fence) provePods(ctx context.Context, out io.Writer, kube string) []string {
	label := kube
	if label == "" {
		label = "the current context"
	}
	writers := f.writerContainers()
	var blockers []string
	raw, err := f.kubectl(ctx, kube, "get", "pods", "--all-namespaces", "-o", "json")
	var pods podList
	if err == nil {
		err = json.Unmarshal(raw, &pods)
	}
	if err != nil {
		return []string{fmt.Sprintf("kubernetes %s: list pods: %v", label, err)}
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			if !slices.Contains(writers, container.Name) {
				continue
			}
			terminated := false
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == container.Name && status.State.Terminated != nil {
					terminated = true
				}
			}
			if !terminated {
				blockers = append(blockers, fmt.Sprintf("kubernetes %s: pod %s/%s (uid %s, node %s) runs container %s",
					label, pod.Metadata.Namespace, pod.Metadata.Name, pod.Metadata.UID, pod.Spec.NodeName, container.Name))
			}
		}
	}
	raw, err = f.kubectl(ctx, kube, "get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return append(blockers, fmt.Sprintf("kubernetes %s: list nodes: %v", label, err))
	}
	nodes := strings.Fields(string(raw))
	if len(nodes) == 0 {
		return append(blockers, fmt.Sprintf("kubernetes %s: the node list is empty, so no runtime was read", label))
	}
	for _, node := range nodes {
		raw, err := f.kubectl(ctx, kube, "get", "--raw", "/api/v1/nodes/"+url.PathEscape(node)+"/proxy/runningpods/")
		var running podList
		if err == nil {
			err = json.Unmarshal(raw, &running)
		}
		if err != nil {
			blockers = append(blockers, fmt.Sprintf("kubernetes %s: node %s: read its runtime's running pods: %v", label, node, err))
			continue
		}
		for _, pod := range running.Items {
			for _, container := range pod.Spec.Containers {
				if slices.Contains(writers, container.Name) {
					blockers = append(blockers, fmt.Sprintf("kubernetes %s: node %s's runtime still runs pod %s/%s (uid %s) container %s, whatever the API lists",
						label, node, pod.Metadata.Namespace, pod.Metadata.Name, pod.Metadata.UID, container.Name))
				}
			}
		}
	}
	if len(blockers) == 0 {
		fmt.Fprintf(out, "proven: kubernetes %s: no pod the API lists and no running pod of %d nodes' runtimes runs %s\n", label, len(nodes), strings.Join(writers, " or "))
	}
	return blockers
}

// hostQueryDone ends a host query that ran to its end, after the count of the processes it read.
const hostQueryDone = "envoy-buckets-host-query-done"

// hostQuery is the shell the host query runs: each process whose name is a writer's, its pid and
// name, then how many processes it read and hostQueryDone. A process's name (/proc/<pid>/comm) is
// readable whoever runs the query, where its executable is not.
func hostQuery(names []string) string {
	return `n=0; for d in /proc/[0-9]*; do read -r c < "$d/comm" 2>/dev/null || continue; n=$((n+1)); case "$c" in ` +
		strings.Join(names, "|") + `) echo "${d#/proc/} $c";; esac; done; echo "processes=$n"; echo ` + hostQueryDone
}

// proveHost proves that no writer process runs on host: the query ran to its end, read at least
// one process, and found none named as a writer.
func (f fence) proveHost(ctx context.Context, out io.Writer, host string) []string {
	names := f.writerProcesses()
	raw, err := f.run(ctx, f.ssh, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, hostQuery(names))
	if err != nil {
		return []string{fmt.Sprintf("host %s: unreachable or the query failed: %v", host, err)}
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 || lines[len(lines)-1] != hostQueryDone {
		return []string{fmt.Sprintf("host %s: the query did not run to its end: %q", host, string(raw))}
	}
	count, err := strconv.Atoi(strings.TrimPrefix(lines[len(lines)-2], "processes="))
	if err != nil || count == 0 {
		return []string{fmt.Sprintf("host %s: the query read no process at all, so it proves nothing: %q", host, string(raw))}
	}
	var blockers []string
	for _, line := range lines[:len(lines)-2] {
		blockers = append(blockers, fmt.Sprintf("host %s: process %s runs", host, line))
	}
	if len(blockers) == 0 {
		fmt.Fprintf(out, "proven: host %s: none of its %d processes is %s\n", host, count, strings.Join(names, " or "))
	}
	return blockers
}

// connzPage is the part of a /connz page the census reads.
type connzPage struct {
	Total       int `json:"total"`
	Offset      int `json:"offset"`
	Connections []struct {
		Name string `json:"name"`
		IP   string `json:"ip"`
		Port int    `json:"port"`
		User string `json:"authorized_user"`
	} `json:"connections"`
}

// census reads every page of monitor's /connz and blocks on each connection named as a writer's.
// It proves nothing on its own: a frozen process holds no connection, so it finds a live writer no
// inventory named and is no substitute for one.
func (f fence) census(ctx context.Context, out io.Writer, monitor string) []string {
	names := f.writerConnections()
	var blockers []string
	read := 0
	for offset := 0; ; {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(monitor, "/")+"/connz?auth=true&limit=1024&offset="+strconv.Itoa(offset), nil)
		if err != nil {
			cancel()
			return append(blockers, fmt.Sprintf("connz %s: %v", monitor, err))
		}
		response, err := f.http.Do(request)
		var page connzPage
		if err == nil {
			err = json.NewDecoder(response.Body).Decode(&page)
			_ = response.Body.Close()
		}
		cancel()
		if err != nil {
			return append(blockers, fmt.Sprintf("connz %s: %v", monitor, err))
		}
		for _, connection := range page.Connections {
			if slices.Contains(names, connection.Name) {
				blockers = append(blockers, fmt.Sprintf("connz %s: connection %q from %s:%d as %q is open", monitor, connection.Name, connection.IP, connection.Port, connection.User))
			}
		}
		read += len(page.Connections)
		offset += len(page.Connections)
		if len(page.Connections) == 0 || offset >= page.Total {
			break
		}
	}
	if len(blockers) == 0 {
		fmt.Fprintf(out, "census: connz %s: none of %d connections is named %s (this adds to the proof above and is none on its own)\n", monitor, read, strings.Join(names, " or "))
	}
	return blockers
}
