// Package buckets is `envoy-infra buckets`: the one command that creates the buckets a listener
// copies (bus.CopiedBuckets), and recreates one, once it has checked that no writer of them runs.
//
// A write such a bucket acknowledged is lost when the bucket is deleted and created again, and the
// listener that made it told its caller it landed: the ack names the stream and its sequence, never
// the bucket's incarnation. So no listener creates one, and this command refuses
// while a listener of the deployment it can see runs, and, for the CI state bucket, an
// envoy-dispatch, which writes it too. A listener that finds a bucket missing serves its writes 503
// and names the bucket on /healthz until this command recreates it.
//
// The check is best effort, observation of the places the operator names, and every run says so
// (bestEffortWarning). The coming server-side hold will replace it with exclusion at the
// server: while it holds, the auth callout refuses every connection but the operator's, every other
// connection is kicked, and every server's connz shows the operator alone. Until then it reads:
//
//   - Kubernetes, for each --kube-context: no pod the API lists runs a writer container that has
//     not terminated, and no node's kubelet runtime list (/api/v1/nodes/<node>/proxy/runningpods/)
//     holds one either, since a pod deleted with --force leaves its containers running after the
//     API forgets it. A node whose runtime list does not answer blocks.
//   - Hosts, for each --host, through ssh (or --ssh, a program taking ssh's arguments): no process
//     on the host is named envoy-listener (or envoy-dispatch). An unreachable host blocks, and so
//     does a query that did not run to its end or enumerated no process at all.
//   - This machine, with --this-machine: the same query, run here through sh with no ssh, read and
//     judged as a host's. In a container it reads the processes the container's PID namespace
//     holds, so it proves the machine only where the container shares the machine's (docker's
//     --pid host, compose's pid: host); the single-node compose file runs it so.
//   - The census, for each --connz (a hub server's monitoring URL): no connection named as a
//     writer's is open. It can only add a blocker: an empty census proves no process absent.
//
// --first-deployment adds one check to the proof, and replaces none of it: the server must hold
// neither the notification stream, which every listener and envoy-dispatch create at their first
// connect, nor any of the buckets. An empty server is no proof that no writer runs - a hub on a new
// disk is empty while its old clients still run - so the inventory is required with it as without.
//
// A create that finds every bucket it names already there changes nothing, so it takes no proof:
// it reads each bucket (missingBuckets), says that every one exists and exits 0. That lets a
// provisioning step run on every start - the single-node compose file's buckets service, which
// `docker compose up` runs again while the listener it provisioned still serves - pass without
// stopping that listener. A read the server does not answer fails the run, and a create with any
// bucket missing takes the whole proof. --first-deployment says the server holds no bucket, so a
// create with it always takes its proof, which then refuses a server that holds one.
//
// Until the auth service's cutover lets the operator connect through the auth service, no hub
// has the buckets user's key (the deployment repository declares that user with none), so
// `create --break-glass` is how a missing copied bucket is made: it runs as a listener's NATS user,
// which may create these buckets, takes no writer proof, and creates only a bucket the server
// reports missing (breakGlassCreate). The pull request that removes the create grant from the
// listener users at the cutover deletes the flag with it.
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
	"unicode"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/infra/internal/flagerr"
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

// Grant is the NATS grant the command's user needs, and nothing more: the subjects it publishes to
// - the account's JetStream info, then, for each bucket a listener copies, its stream's info,
// create, update (raising its copies) and delete, and the notification stream's info, which
// --first-deployment reads - and the one it subscribes to, its own reply inboxes (InboxPrefix).
// The same user runs the restore after `check` (`nats stream restore` with
// --inbox-prefix=_INBOX_envoy_buckets), so the grant also holds each copied bucket's restore
// request and the subject its chunks go to (nats-server 2.15.1, jetstream_api.go:91-94 and
// 170-172). The deployment's buckets user is granted exactly this until the auth service's
// auth callout replaces the static user at the cutover.
func Grant() (publish, subscribe []string) {
	publish = []string{"$JS.API.INFO", "$JS.API.STREAM.INFO." + bus.Stream}
	for _, bucket := range bus.CopiedBuckets() {
		for _, verb := range []string{"INFO", "CREATE", "UPDATE", "DELETE", "RESTORE"} {
			publish = append(publish, "$JS.API.STREAM."+verb+".KV_"+bucket)
		}
		publish = append(publish, "$JS.SNAPSHOT.RESTORE.KV_"+bucket+".*")
	}
	return publish, []string{InboxPrefix + ".>"}
}

// bestEffortWarning is printed on every run but a break-glass create, to standard error, before
// anything else: the check is observation, not exclusion, until the coming server-side hold
// excludes every writer at the server - the auth callout refusing every connection but the
// operator's and every other connection kicked - from the check through the change. Each way it
// names is a test that asserts today's outcome and must flip then (buckets_test.go,
// TestBestEffort...). A break-glass create takes no proof, and its run needs no writer stopped
// (breakGlassCreate), so the warning and its instruction are not its to print.
const bestEffortWarning = `envoy-infra buckets: WARNING: this proof is best effort. A writer gets past it four ways:
  - an omitted host or dev box: only the --kube-context, --host and --this-machine you name are checked;
  - a renamed binary: a host is checked by process name (envoy-listener, listener, dispatch, ...);
  - a writer that starts between the final scan and the create: nothing holds writers out;
  - an unheld restore: check holds nothing through a later nats stream restore.
Before a run that creates or deletes a bucket, stop every envoy-listener of the deployment (and
every envoy-dispatch, for envoy_ci_state) and confirm on each host that it has ended. A create
that finds every bucket there changes nothing and takes no proof. A server-side hold, which
leaves no writer a connection, replaces this.
`

const usage = `usage: envoy-infra buckets create|recreate|check --nats URL [options]
       envoy-infra buckets create --break-glass --operator NAME --bucket NAME --nats URL

  create     create every copied bucket that is missing (or each --bucket), once no writer runs;
             with none missing it changes nothing and takes no proof
  recreate   delete each --bucket and create it again, once no writer runs
  check      prove no writer runs, and change nothing

The writers are every envoy-listener of the deployment, and every envoy-dispatch when the CI state
bucket is among the buckets. The proof is required before anything changes:
  --kube-context CTX   a Kubernetes context to check, repeatable ("" is kubectl's current one)
  --host TARGET        a machine to query through ssh, repeatable
  --ssh PROGRAM        the program run as ssh (default ssh), such as a wrapper over SSM
  --this-machine       the machine the command runs on, queried here with no ssh; in a container
                       it sees the machine's processes only with the machine's PID namespace
                       (docker --pid host, compose pid: host)
  --connz URL          a hub server's monitoring URL whose connections to read, repeatable
  --first-deployment   also require that the server holds no notification stream and no bucket

  --nats URL           the NATS server, as a user that may create and delete the buckets'
                       streams (NATS_NKEY_SEED_FILE or NATS_NKEY_SEED in the environment)
  --bucket NAME        a copied bucket to act on, repeatable (default: every one, for create)
  --replicas N         the copies a bucket is created with, 1 to 5 (default ENVOY_NATS_REPLICAS, else 1)

Until the auth service's cutover, a missing bucket is created with a listener's credential instead:
  --break-glass        create each --bucket the server reports missing, as a listener's NATS user
                       (NATS_NKEY_SEED_FILE or NATS_NKEY_SEED), with no writer proof; refused
                       when any --bucket exists, and it updates and deletes nothing
  --operator NAME      who runs it, named in the line the run logs; required with --break-glass
`

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func command(ctx context.Context, args []string, stdout, stderr io.Writer, run Runner, client *http.Client) int {
	if len(args) == 0 || (args[0] != "create" && args[0] != "recreate" && args[0] != "check") {
		fmt.Fprint(stderr, bestEffortWarning)
		help := len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help")
		fmt.Fprint(stderr, usage)
		if help {
			return 0
		}
		return 2
	}
	action := args[0]
	flags := flag.NewFlagSet("envoy-infra buckets "+action, flag.ContinueOnError)
	// The flag package's own message quotes a value it could not parse, so what it writes is
	// printed only after --help, whose listing holds no word given (see below).
	var parsed bytes.Buffer
	flags.SetOutput(&parsed)
	var f fence
	var buckets stringList
	var natsURL, operator string
	var breakGlass bool
	replicas := 0
	flags.Var((*stringList)(&f.kubeContexts), "kube-context", "a Kubernetes context to check")
	flags.Var((*stringList)(&f.hosts), "host", "a machine to query through ssh")
	flags.BoolVar(&f.thisMachine, "this-machine", false, "the machine the command runs on, queried with no ssh")
	flags.StringVar(&f.ssh, "ssh", "ssh", "the program run as ssh")
	flags.Var((*stringList)(&f.connz), "connz", "a hub server's monitoring URL")
	flags.BoolVar(&f.firstDeployment, "first-deployment", false, "the server has never served a listener")
	flags.StringVar(&natsURL, "nats", "", "the NATS server")
	flags.Var(&buckets, "bucket", "a copied bucket")
	flags.IntVar(&replicas, "replicas", 0, "the copies a bucket is created with")
	flags.BoolVar(&breakGlass, "break-glass", false, "create each missing --bucket as a listener's user, with no writer proof")
	flags.StringVar(&operator, "operator", "", "who runs a --break-glass create")
	err := flags.Parse(args[1:])
	if action != "create" || !breakGlass {
		fmt.Fprint(stderr, bestEffortWarning)
	}
	// No word the command was given is echoed where the parse could not read it as one of its own:
	// a seed may have been pasted there, and `envoy-infra seed` and `render` echo none either. A
	// stray argument is named by its position, a malformed value by its flag, and any other word
	// the parse refused not at all.
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprint(stderr, parsed.String())
		case strings.HasPrefix(err.Error(), "flag needs an argument: -"):
			// The flag package says this only of a flag it defines, so the name is one of ours.
			fmt.Fprintf(stderr, "envoy-infra buckets: %v\n%s", err, usage)
		default:
			if name := flagerr.Malformed(flags, err); name != "" {
				fmt.Fprintf(stderr, "envoy-infra buckets: --%s: a malformed value (not echoed: a seed may have been pasted there)\n%s", name, usage)
			} else {
				fmt.Fprintf(stderr, "envoy-infra buckets: a flag this command does not take, or a malformed one (not echoed: a seed may have been pasted there)\n%s", usage)
			}
		}
		return 2
	}
	if flags.NArg() > 0 {
		// The action is word 1 after "buckets", so the first word the flags left is word
		// len(args)-NArg()+1.
		fmt.Fprintf(stderr, "envoy-infra buckets: word %d after \"buckets\" is no flag and no flag's value (not echoed: a seed may have been pasted there)\n%s", len(args)-flags.NArg()+1, usage)
		return 2
	}
	// A flag's value reaches the lines the command writes - a blocker names each --host,
	// --kube-context and --connz, a refusal each --bucket, the break-glass line --operator - and
	// the programs it runs, so a value that holds an nkey seed (bus.HoldsSeed), whole or inside it,
	// is refused naming the flag, never the value, before anything reads it, as seed and render
	// refuse one.
	var seeded []string
	flags.Visit(func(set *flag.Flag) {
		if bus.HoldsSeed(set.Value.String()) {
			seeded = append(seeded, "--"+set.Name)
		}
	})
	if len(seeded) > 0 {
		fmt.Fprintf(stderr, "envoy-infra buckets: %s: an nkey seed, which is never an argument (not echoed)\n", strings.Join(seeded, ", "))
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
	// A bucket named twice is one bucket: it is read, logged and created or recreated once.
	named := map[string]bool{}
	buckets = slices.DeleteFunc(buckets, func(bucket string) bool {
		again := named[bucket]
		named[bucket] = true
		return again
	})
	if refusal := breakGlassUsage(flags, action, breakGlass, operator, len(buckets)); refusal != "" {
		fmt.Fprintf(stderr, "envoy-infra buckets: %s\n", refusal)
		return 2
	}
	if action == "recreate" && len(buckets) == 0 {
		fmt.Fprintln(stderr, "envoy-infra buckets: recreate needs each --bucket it deletes")
		return 2
	}
	if len(buckets) == 0 {
		buckets = bus.CopiedBuckets()
	}
	// The deployment's replica count: --replicas, or else ENVOY_NATS_REPLICAS read as every program
	// of the deployment reads it (bus.ParseNATSReplicas). Either outside 1 to bus.MaxReplicas is a
	// usage error, before anything connects.
	replicasSet := false
	flags.Visit(func(set *flag.Flag) { replicasSet = replicasSet || set.Name == "replicas" })
	if replicasSet {
		if replicas < 1 || replicas > bus.MaxReplicas {
			fmt.Fprintf(stderr, "envoy-infra buckets: --replicas %d is no replica count: want a whole number of copies from 1 to %d\n", replicas, bus.MaxReplicas)
			return 2
		}
	} else if replicas, err = bus.ParseNATSReplicas(os.Getenv(bus.ReplicasEnvVar)); err != nil {
		fmt.Fprintf(stderr, "envoy-infra buckets: %v\n", err)
		return 2
	}
	if breakGlass {
		return breakGlassCreate(stdout, stderr, natsURL, operator, buckets, replicas)
	}
	if len(f.kubeContexts) == 0 && len(f.hosts) == 0 && !f.thisMachine {
		fmt.Fprintln(stderr, "envoy-infra buckets: name every place a writer of the deployment runs: --kube-context for each cluster, --host for each machine and --this-machine for the one the command runs on; --first-deployment proves no writer absent and needs them too")
		return 2
	}
	f.run, f.http, f.pid = run, client, os.Getpid()
	f.dispatch = slices.Contains(buckets, bus.CIStateBucket)

	nc := connect(stderr, natsURL, bus.WithName("envoy-infra-buckets"), bus.WithEnvironment(operatorEnvironment), bus.WithInboxPrefix(InboxPrefix))
	if nc == nil {
		return 1
	}
	defer nc.Close()
	js := nc.JS()

	// A create with nothing to create changes nothing, so it takes no proof (the package comment
	// says why). --first-deployment always takes its proof.
	if action == "create" && !f.firstDeployment {
		missing, err := missingBuckets(js, buckets)
		if err != nil {
			fmt.Fprintf(stderr, "envoy-infra buckets: %v\n", err)
			return 1
		}
		if len(missing) == 0 {
			fmt.Fprintf(stdout, "every bucket named exists (%s): nothing to create, so no writer proof is taken\n", strings.Join(buckets, ", "))
			return 0
		}
	}

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
	fmt.Fprintf(stderr, "envoy-infra buckets %s: refused: a writer of the buckets may still run, and a bucket created under it loses every write the deleted one acknowledged\n", action)
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
		// An operator's command, run by hand until it ends: the create's waits end at the start's
		// hang guard (bus.EnsureKeyValue), under no context of its own.
		if _, err := bus.CreateCopiedBucket(context.Background(), conn, js, bucket, replicas); err != nil {
			fmt.Fprintf(stderr, "envoy-infra buckets: create %s: %v\n", bucket, err)
			return 1
		}
		fmt.Fprintf(stdout, "created %s (%d copies)\n", bucket, replicas)
	}
	return 0
}

// missingBuckets reads each of buckets and returns those the server reports missing. A read the
// server answers with anything but "not found", or does not answer, is an error naming the bucket.
func missingBuckets(js nats.JetStreamContext, buckets []string) ([]string, error) {
	var missing []string
	for _, bucket := range buckets {
		if _, err := js.KeyValue(bucket); errors.Is(err, nats.ErrBucketNotFound) {
			missing = append(missing, bucket)
		} else if err != nil {
			return nil, fmt.Errorf("read %s: %w", bucket, err)
		}
	}
	return missing, nil
}

// breakGlassUsage is why the flags of a run misuse --break-glass or --operator, or "" when they
// do not: the flag is create's alone, takes no proof flag, and needs --operator and each --bucket
// it creates; --operator names a break-glass run and nothing else. An operator name holding a
// control character, a line break among them, is refused, so the run's log line stays one line.
func breakGlassUsage(flags *flag.FlagSet, action string, breakGlass bool, operator string, buckets int) string {
	if !breakGlass {
		if operator != "" {
			return "--operator names who runs a --break-glass create; leave it off without --break-glass"
		}
		return ""
	}
	if action != "create" {
		return "--break-glass is create's alone: it creates a missing bucket and does nothing else, so it neither deletes one (recreate) nor proves that no writer runs (check)"
	}
	var proof []string
	flags.Visit(func(set *flag.Flag) {
		switch set.Name {
		case "kube-context", "host", "this-machine", "ssh", "connz", "first-deployment":
			proof = append(proof, "--"+set.Name)
		}
	})
	if len(proof) > 0 {
		return "--break-glass takes no writer proof, so " + strings.Join(proof, " and ") + " would check nothing: leave them off"
	}
	if strings.TrimSpace(operator) == "" || strings.ContainsFunc(operator, unicode.IsControl) {
		return "--break-glass needs --operator, who runs it, on one line: the run logs it"
	}
	if buckets == 0 {
		return "--break-glass needs each missing --bucket it creates"
	}
	return ""
}

// breakGlassCreate is `create --break-glass`: it reads each of buckets' streams (STREAM.INFO
// KV_<bucket>), and only when the server reports every one of them missing does it create each,
// with its copied-bucket configuration at replicas copies, after one line on standard error naming
// the operator, the hub, the bucket and that the run is break-glass. A bucket that exists, or whose
// read answers anything but "not found", refuses the run, and no bucket is created.
//
// It connects as the user the environment names (NATS_NKEY_SEED_FILE or NATS_NKEY_SEED), a
// listener's until the auth service's cutover, and takes its replies on nats.go's default
// _INBOX. prefix, which a listener's grant may receive on and the buckets user's (InboxPrefix) may
// not.
//
// It takes no writer proof. A missing bucket only gains a create: the writes its deleted stream
// acknowledged were lost when it was deleted, and every listener has answered its writes 503 since,
// so a create while listeners run loses nothing more. And the proof cannot reach the production VM
// hub's processes, so with it this path could not run there.
//
// It only creates: each create is bus.CreateMissingCopiedBucket's, which sends the normal path's
// create (bus.CreateCopiedBucket) with its resend of an unanswered request and its wait for the
// member answering to apply the create, and answers bus.ErrBucketExists, opening, raising and
// updating nothing, for a bucket another client made after the read: found by its own lookup, or
// in the answer to its create. The run then stops there, refused, and creates no bucket after it.
func breakGlassCreate(stdout, stderr io.Writer, natsURL, operator string, buckets []string, replicas int) int {
	nc := connect(stderr, natsURL, bus.WithName("envoy-infra-buckets-break-glass"), bus.WithEnvironment(operatorEnvironment))
	if nc == nil {
		return 1
	}
	defer nc.Close()
	js := nc.JS()
	var refusals []string
	for _, bucket := range buckets {
		stream := "KV_" + bucket
		if _, err := js.StreamInfo(stream); err == nil {
			refusals = append(refusals, stream+" exists")
		} else if !errors.Is(err, nats.ErrStreamNotFound) {
			refusals = append(refusals, fmt.Sprintf("STREAM.INFO %s did not answer that it is missing: %v", stream, err))
		}
	}
	if len(refusals) > 0 {
		fmt.Fprintln(stderr, "envoy-infra buckets create --break-glass: refused: it creates only buckets the server reports missing, and none at all while any --bucket is there or unread")
		for _, refusal := range refusals {
			fmt.Fprintf(stderr, "  refused: %s\n", refusal)
		}
		return 1
	}
	hub := bus.NATSURLWithoutUserinfo(natsURL)
	for _, bucket := range buckets {
		fmt.Fprintf(stderr, "envoy-infra buckets: break-glass: operator %s creates %s on %s, as the environment's NATS user and with no writer proof\n", operator, bucket, hub)
		if _, err := bus.CreateMissingCopiedBucket(context.Background(), nc.Conn, js, bucket, replicas); errors.Is(err, bus.ErrBucketExists) {
			fmt.Fprintf(stderr, "envoy-infra buckets create --break-glass: refused: KV_%s was made by another client after the read, and is left as it is: %v\n", bucket, err)
			return 1
		} else if err != nil {
			fmt.Fprintf(stderr, "envoy-infra buckets: create %s: %v\n", bucket, err)
			return 1
		}
		fmt.Fprintf(stdout, "created %s (%d copies)\n", bucket, replicas)
	}
	return 0
}

// The fixed lines connect prints for a --nats URL it cannot use, which name no part of the URL, not
// even the host: such a URL can hold credential text there.
const (
	unreadableURL = "envoy-infra buckets: connect: the --nats URL does not parse as user info and a server, so no part of it is printed: percent-encode every character of its user and its password but letters, digits and `-._~` (`/`, `?`, `#`, `@` and `:` among them)"
	paddedURL     = "envoy-infra buckets: connect: the --nats URL starts or ends with white space (a space, a tab or a line break), so it is not dialed and no part of it is printed: give it with none"
)

// connect is both paths' connect to natsURL, or nil once it has written why there is none. The
// command hands nats.go any URL, since it sets bus.AllowRemoteEnvVar, so before any dial it refuses:
//
//   - a URL with white space at either end (paddedURL): bus reads the URL trimmed, and nats.go as
//     given, so the two would read different URLs, and nats.go's parse would fail only after the
//     connect's ten tries a second apart;
//   - a URL whose user info a parse does not read where it is written (bus.NATSURLUserinfoParses,
//     unreadableURL): a `/`, `?` or `#` in its user or password ends the authority there, nats.go
//     dials the credential's head as the host, and the failed lookup names it.
//
// A connect that fails prints one line. A URL nats.go still cannot parse - one with no user info,
// such as a port that is no number - fails with a *url.Error, whose text quotes the whole URL: that
// prints unreadableURL. Any other failure names the server without the URL's user info
// (bus.NATSURLWithoutUserinfo) and the error, which then quotes no URL.
func connect(stderr io.Writer, natsURL string, options ...bus.ConnectOption) *bus.Client {
	if natsURL != strings.TrimSpace(natsURL) {
		fmt.Fprintln(stderr, paddedURL)
		return nil
	}
	if !bus.NATSURLUserinfoParses(natsURL) {
		fmt.Fprintln(stderr, unreadableURL)
		return nil
	}
	nc, err := bus.Connect([]string{natsURL}, options...)
	var unparsed *url.Error
	switch {
	case errors.As(err, &unparsed):
		fmt.Fprintln(stderr, unreadableURL)
		return nil
	case err != nil:
		fmt.Fprintf(stderr, "envoy-infra buckets: connect to %s: %v\n", bus.NATSURLWithoutUserinfo(natsURL), err)
		return nil
	}
	return nc
}

// fence is the proof that no writer of the buckets runs.
type fence struct {
	kubeContexts []string
	hosts        []string
	// thisMachine queries the machine the command runs on, through sh rather than ssh; pid is the
	// command's own process id, which proveThisMachine reads.
	thisMachine     bool
	pid             int
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
	if f.thisMachine {
		blockers = append(blockers, f.proveThisMachine(ctx, out)...)
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

// subprocessTimeout bounds each kubectl and ssh call the proof makes: a connected API request or
// remote command that stops answering is killed at it (exec.CommandContext) and blocks the proof,
// rather than holding the command until the operator interrupts it. kubectl's own request timeout
// (kubectlRequestTimeout) sits inside it, so kubectl reports its own expiry first. A test lowers it.
var subprocessTimeout = time.Minute

// kubectlRequestTimeout is kubectl's --request-timeout, within subprocessTimeout.
const kubectlRequestTimeout = "45s"

// call runs name with args through the fence's runner within subprocessTimeout, and answers a call
// that did not finish in it as an error naming the bound.
func (f fence) call(ctx context.Context, name string, args ...string) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	out, err := f.run(bounded, name, args...)
	if errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("%s did not finish within %s: %w", name, subprocessTimeout, bounded.Err())
	}
	return out, err
}

// kubectl runs kubectl in context kube ("" for the current one), with kubectl's own request
// timeout, within subprocessTimeout.
func (f fence) kubectl(ctx context.Context, kube string, args ...string) ([]byte, error) {
	args = append([]string{"--request-timeout=" + kubectlRequestTimeout}, args...)
	if kube != "" {
		args = append([]string{"--context", kube}, args...)
	}
	return f.call(ctx, "kubectl", args...)
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

// proveHost proves that no writer process runs on host, through ssh: the query ran to its end, read
// at least one process, and found none named as a writer (processBlockers).
func (f fence) proveHost(ctx context.Context, out io.Writer, host string) []string {
	raw, err := f.call(ctx, f.ssh, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, hostQuery(f.writerProcesses()))
	if err != nil {
		return []string{fmt.Sprintf("host %s: unreachable or the query failed: %v", host, err)}
	}
	return f.processBlockers(out, "host "+host, raw)
}

// proveThisMachine proves that no writer process runs on the machine the command runs on: the host
// query, run here through sh, judged as a host's (processBlockers). In a container it reads only
// the PID namespace the container holds, which is the machine's with docker's --pid host. A command
// that is process 1 of its PID namespace is a container's entrypoint in a namespace of its own -
// on a machine, process 1 is its init - so its query would read the container's processes alone
// and report the machine clean: it blocks instead. A container whose entrypoint is something else
// (a shell that runs the command) is not caught this way, which the usage and the runbook say.
func (f fence) proveThisMachine(ctx context.Context, out io.Writer) []string {
	if f.pid == 1 {
		return []string{"this machine: the command is process 1 of its PID namespace, so it runs in a container that does not share the machine's, and its query would read that container's processes alone: run it with docker's --pid host, or compose's pid: host"}
	}
	raw, err := f.call(ctx, "sh", "-c", hostQuery(f.writerProcesses()))
	if err != nil {
		return []string{fmt.Sprintf("this machine: the query failed: %v", err)}
	}
	return f.processBlockers(out, "this machine", raw)
}

// processBlockers judges raw, what the host query printed on place: it blocks unless the query ran
// to its end and read at least one process, and on each process named as a writer.
func (f fence) processBlockers(out io.Writer, place string, raw []byte) []string {
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 || lines[len(lines)-1] != hostQueryDone {
		return []string{fmt.Sprintf("%s: the query did not run to its end: %q", place, string(raw))}
	}
	count, err := strconv.Atoi(strings.TrimPrefix(lines[len(lines)-2], "processes="))
	if err != nil || count == 0 {
		return []string{fmt.Sprintf("%s: the query read no process at all, so it proves nothing: %q", place, string(raw))}
	}
	var blockers []string
	for _, line := range lines[:len(lines)-2] {
		blockers = append(blockers, fmt.Sprintf("%s: process %s runs", place, line))
	}
	if len(blockers) == 0 {
		fmt.Fprintf(out, "proven: %s: none of its %d processes is %s\n", place, count, strings.Join(f.writerProcesses(), " or "))
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
