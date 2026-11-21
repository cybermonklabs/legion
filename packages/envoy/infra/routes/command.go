package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const usageText = `usage: envoy-infra routes record --namespace <ns> --statefulset <name> --out <file> [--context <kubeconfig context>] [--kubectl <path>] [--route-port <port>]
       envoy-infra routes check --namespace <ns> --statefulset <name> --record <file> [--context <kubeconfig context>] [--kubectl <path>] [--monitor-port <port>] [--route-port <port>]
  record, once routeAuthorization's env stage has rolled every pod, before routes, run as the person
    who deploys, through their kubeconfig context: saves each pod's uid and its nats container's
    restart count and start, and the context and server of the cluster it read and the digest of
    each of its CA certificates ("in-cluster" in a pod with no kubeconfig and no --context, which
    reads the cluster as the pod's ServiceAccount); exit 0 when recorded, 2 otherwise, and while
    any pod's route port already requires authorization. A record is valid only for a secretKey
    that has never been under authorization: after a rollback below authorization, or any exit 1 of
    check, rotate to a new secretKey first.
  check, once authorization is on every pod, run as the operator's Job, of the record's cluster:
    one CA certificate shared, or, when either side holds none, its context and server; exit 0
    when authorization is in force with a password and every route since it loaded joined two of
    the hub's servers, which says nothing of any time before the record; 1 when another process
    held one or may have been handed the password, or a log line may hold it (treat the route
    password as read, cut its reach, rotate the password, then record and check again); 2 when it
    could not read all it needs, or the record is of another cluster, when authorization is not in
    force, left no line of the reload that loaded it, or loaded within 10 s of a pod's start, when
    the mesh is not whole or the servers do not hold one password, when a route connection from
    another address named no server or was still open when the log was read, when the log holds no
    hangup that began the reload that loaded authorization, when the log holds a line in the
    server's form the server did not write, or when a pod changed during the check.
`

// timeout bounds a command's reads of the hub: each kubectl call, each wait for a port-forward to
// listen, and each read through one. A port-forward's own process runs under no deadline; Close
// stops it when the command ends.
const timeout = 5 * time.Minute

// Command runs `envoy-infra routes` on args, the words after "routes", reading the hub through
// the Source open makes, and returns its exit code.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer, open Opener) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "-h", "-help", "--help":
		fmt.Fprint(stderr, usageText)
		return 0
	case "record", "check":
	default:
		// The word given is not echoed, as the rest of the command echoes none: a seed pasted there
		// must not reach the terminal.
		fmt.Fprintln(stderr, "envoy-infra routes: the subcommand is record or check")
		fmt.Fprint(stderr, usageText)
		return 2
	}
	fs := flag.NewFlagSet("envoy-infra routes "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var target Target
	var out, recordPath string
	fs.StringVar(&target.Namespace, "namespace", "", "the hub's namespace")
	fs.StringVar(&target.StatefulSet, "statefulset", "", "the hub's StatefulSet")
	fs.StringVar(&target.Context, "context", "", "the kubeconfig context")
	fs.StringVar(&target.Kubectl, "kubectl", "kubectl", "the kubectl to run")
	fs.IntVar(&target.RoutePort, "route-port", 6222, "the pods' route port")
	if sub == "record" {
		fs.StringVar(&out, "out", "", "the file the record is written to")
	} else {
		fs.StringVar(&recordPath, "record", "", "the file routes record wrote")
		fs.IntVar(&target.MonitorPort, "monitor-port", 8222, "the pods' monitoring port")
	}
	usage := func(problem string) int {
		fmt.Fprintf(stderr, "envoy-infra routes %s: %s\n", sub, problem)
		fmt.Fprint(stderr, usageText)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stderr, usageText)
			return 0
		}
		// The flag package's message quotes the word or value it refused; this one does not.
		return usage("a flag it does not take, or a value it cannot parse")
	}
	if fs.NArg() > 0 {
		return usage(fmt.Sprintf("it takes no words after its flags, and was given %d", fs.NArg()))
	}
	if target.Namespace == "" || target.StatefulSet == "" {
		return usage("--namespace and --statefulset are required")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if sub == "record" && out == "" {
		return usage("--out is required")
	}
	if sub == "check" && recordPath == "" {
		return usage("--record is required")
	}
	type portFlag struct {
		name  string
		value int
	}
	ports := []portFlag{{"--route-port", target.RoutePort}}
	if sub == "check" {
		ports = append(ports, portFlag{"--monitor-port", target.MonitorPort})
	}
	for _, port := range ports {
		if port.value < 1 || port.value > 65535 {
			return usage(port.name + " is a port, 1 to 65535")
		}
	}
	if sub == "record" {
		return record(ctx, target, out, stdout, stderr, open)
	}
	return check(ctx, target, recordPath, stdout, stderr, open)
}

func record(ctx context.Context, target Target, out string, stdout, stderr io.Writer, open Opener) int {
	src, err := open(target)
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra routes record: %v\n", err)
		return 2
	}
	defer src.Close()
	// The cluster first: its read refuses a server kubectl's errors could print, before any call
	// that reads an object of the cluster.
	cluster, err := src.Cluster(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra routes record: not recorded: which cluster it reads is not known: %v\n", err)
		return 2
	}
	rec, err := Take(ctx, src, target.Namespace, target.StatefulSet, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra routes record: not recorded: %v\n", err)
		return 2
	}
	rec.Cluster = cluster
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra routes record: %v\n", err)
		return 2
	}
	if err := os.WriteFile(out, append(body, '\n'), 0o644); err != nil {
		fmt.Fprintf(stderr, "envoy-infra routes record: not recorded: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "recorded the %d pods of %s/%s through context %q of the cluster at %q at %s in %s\n", len(rec.Pods), rec.Namespace,
		rec.StatefulSet, rec.Cluster.Context, rec.Cluster.Server, rec.RecordedAt.Format(time.RFC3339), out)
	fmt.Fprintln(stdout, "the record is valid only for a secretKey that has never been under authorization: after a rollback below authorization, "+
		"or any exit 1 of routes check, rotate to a new secretKey and record at its env stage (docs/nats-hub.md, Route authorization)")
	return 0
}

func check(ctx context.Context, target Target, recordPath string, stdout, stderr io.Writer, open Opener) int {
	rec, err := readRecord(recordPath)
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra routes check: %v\n", err)
		return 2
	}
	if rec.Namespace != target.Namespace || rec.StatefulSet != target.StatefulSet {
		fmt.Fprintf(stderr, "envoy-infra routes check: %s is the record of %s/%s, not of %s/%s\n", recordPath,
			rec.Namespace, rec.StatefulSet, target.Namespace, target.StatefulSet)
		return 2
	}
	src, err := open(target)
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra routes check: %v\n", err)
		return 2
	}
	defer src.Close()
	cluster, err := src.Cluster(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra routes check: which cluster it reads is not known: %v\n", err)
		return 2
	}
	if !sameCluster(rec.Cluster, cluster) {
		if len(rec.Cluster.CA) > 0 && len(cluster.CA) > 0 {
			fmt.Fprintf(stderr, "envoy-infra routes check: %s is the record of a cluster whose CA certificates have the SHA-256 %s; the cluster read "+
				"now through context %q at %q has %s, and they share none, so it is another cluster\n", recordPath,
				strings.Join(rec.Cluster.CA, ", "), cluster.Context, cluster.Server, strings.Join(cluster.CA, ", "))
			return 2
		}
		fmt.Fprintf(stderr, "envoy-infra routes check: %s is the record taken through context %q of the cluster at %q, not through context %q of "+
			"the cluster at %q, and with no CA certificate on both sides the cluster is told by its context and server\n", recordPath,
			rec.Cluster.Context, rec.Cluster.Server, cluster.Context, cluster.Server)
		return 2
	}
	report := Check(ctx, src, rec)
	for _, f := range report.Findings {
		fmt.Fprintln(stdout, f)
	}
	verdict := report.Verdict()
	switch verdict {
	case Clean:
		fmt.Fprintf(stdout, "clean: authorization is in force on the %d pods of %s/%s, every route joins two of the hub's servers, and their logs "+
			"since the record of %s name no failed reload and, since authorization loaded, no other peer\n",
			len(rec.Pods), rec.Namespace, rec.StatefulSet, rec.RecordedAt.Format(time.RFC3339))
	case PasswordRead:
		fmt.Fprintln(stdout, "password read: treat the route password as read; cut the reach of each process named above, rotate the password "+
			"(docs/nats-hub.md, Route authorization), and end the rotation with routes record and routes check again")
	default:
		fmt.Fprintln(stdout, "unknown: the check could not read all it needs, or the hub is not yet fit to judge, or it can neither condemn nor clear "+
			"a process named above, so it concludes nothing; fix or find what is named above and run it again")
	}
	return int(verdict)
}

// readRecord decodes the record at path, refusing a field it does not know and a record that
// names no pod.
func readRecord(path string) (Record, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return Record{}, fmt.Errorf("decode the record %s: %w", path, err)
	}
	if len(rec.Pods) == 0 {
		return Record{}, fmt.Errorf("the record %s names no pod", path)
	}
	return rec, nil
}
