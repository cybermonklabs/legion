// Package buckets is `envoy-infra buckets`: the one command that creates the buckets a listener
// copies (bus.CopiedBuckets), and checks that no writer of them runs.
//
// A write such a bucket acknowledged is lost when the bucket is deleted and created again, and the
// listener that made it told its caller it landed: the ack names the stream and its sequence, never
// the bucket's incarnation. So no listener creates one, and recreate, which would delete one,
// refuses outside INFRA-4219's writer hold (recreateRefusal). A listener that finds a bucket
// missing serves its writes 503 and names the bucket on /healthz until this command creates it.
//
// create takes no writer proof (createBuckets). A bucket that is absent holds no write to lose: the
// writes its deleted stream acknowledged were lost with it, and every listener has answered the
// writes that need it 503 since, so a create while listeners run loses nothing more. create makes
// each absent bucket at the copies asked for, with the bucket's own configuration
// (bus.CopiedBucketConfig, the configuration the listeners' bus declares for it), and never
// changes a bucket that is there. So it can run again: a bucket there at those copies with that
// configuration is present and left as it is, and one there with other copies or another
// configuration refuses the run, naming each difference, before anything is created. That lets a
// provisioning step run on every start, such as the single-node compose file's buckets service,
// which `docker compose up` runs again while the listener it provisioned still serves, and a Job
// run on every apply.
//
// recreate refuses before it reads or calls anything, whatever its arguments: it opens no NATS
// connection, runs no kubectl, ssh or sh, sends no HTTP request and deletes nothing. No observation
// stands in for the hold, because no observation excludes a writer: a writer can connect, write
// and leave between two reads; the canonical auth.conf shows neither the grants each server has
// loaded nor the auth service's per-connection decisions; and a server-side writer (a stream
// source, a republish, a push consumer) needs no open connection. The held recreate, which runs
// only inside an established and verified writer hold from before its first delete through its
// last create, is INFRA-4219's: cybermonklabs/legion#4698 replaces the refusal with it.
//
// check's proof is best effort, observation of the places the operator names, and each of its runs
// says so (bestEffortWarning). It changes nothing and holds nothing once it returns, for a step
// another tool makes, such as a `nats stream restore`. It reads:
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
//     --pid host, compose's pid: host).
//   - The census, for each --connz (a hub server's monitoring URL): no connection named as a
//     writer's is open. It can only add a blocker: an empty census proves no process absent.
//
// --first-deployment adds one check to the proof, and replaces none of it: the server must hold
// neither the notification stream, which every listener and envoy-dispatch create at their first
// connect, nor any of the buckets. An empty server is no proof that no writer runs - a hub on a new
// disk is empty while its old clients still run - so the inventory is required with it as without.
// create takes none of these flags (createUsage): it would check nothing with them.
//
// The command takes its replies under --inbox-prefix: InboxPrefix, the buckets user's, by default,
// and _INBOX. for a listener's user, which subscribes _INBOX.> and no other reply prefix, such as
// a Job that runs create as the clustered hub's gke user. A request of create that the server
// refuses under the run's grant is sent again for up to refusalWait, since a grant just applied
// reaches a hub at its next reload (untilGranted).
//
// Until the auth service's cutover lets the operator connect through the auth service, no hub
// has the buckets user's key (the deployment repository declares that user with none), so
// `create --break-glass` runs the same create as a listener's NATS user, which may create these
// buckets, taking its replies on nats.go's default inbox prefix, which that user may receive on,
// and logging the operator who runs it (createCommand). The pull request that removes the create
// grant from the listener users at the cutover deletes the flag with it.
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
	"reflect"
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

// InboxPrefix is the prefix the command's reply inboxes take by default (--inbox-prefix,
// bus.WithInboxPrefix), so the deployment's buckets user is granted subscribe on InboxPrefix.>
// alone: a user granted _INBOX.> would receive every other client's replies, a session's push
// deliveries among them.
const InboxPrefix = "_INBOX_envoy_buckets"

// Grant is the NATS grant the command's user needs, and nothing more. It publishes to the
// notification stream's info, which every connection reads and --first-deployment reads again, and
// for each bucket a listener copies, its stream's info, which create and --first-deployment read,
// and its create; create reads the account's JetStream info before each create, as nats.go's own
// create does. It subscribes to its own reply inboxes alone (InboxPrefix). No command sends a
// stream update (create raises and changes no bucket) or a stream delete (recreate refuses before
// it connects), so the grant holds neither. The same user runs the restore after check (`nats
// stream restore` with --inbox-prefix=_INBOX_envoy_buckets), which no command of this package
// sends, so the grant also holds each copied bucket's restore request and the subject its chunks
// go to (nats-server 2.15.1, jetstream_api.go:91-94 and 170-172).
// TestAUserGrantedExactlyTheCommandsGrantRunsEveryStep runs every step and that restore under it,
// and fails on a subject it holds that none of them sends. The deployment's buckets user is
// granted exactly this until the auth service's auth callout replaces the static user at the
// cutover.
func Grant() (publish, subscribe []string) {
	publish = []string{"$JS.API.INFO", "$JS.API.STREAM.INFO." + bus.Stream}
	for _, bucket := range bus.CopiedBuckets() {
		for _, verb := range []string{"INFO", "CREATE", "RESTORE"} {
			publish = append(publish, "$JS.API.STREAM."+verb+".KV_"+bucket)
		}
		publish = append(publish, "$JS.SNAPSHOT.RESTORE.KV_"+bucket+".*")
	}
	return publish, []string{InboxPrefix + ".>"}
}

// recreateRefusal is all recreate does until INFRA-4219's writer hold ships it as a held command
// (cybermonklabs/legion#4698). command writes it before it reads any argument, so no run of
// recreate connects, runs kubectl, ssh or sh, sends an HTTP request or deletes anything, whatever
// its arguments. No observation stands in for the hold: the package comment says why.
const recreateRefusal = `envoy-infra buckets recreate: refused: recreate deletes no bucket outside INFRA-4219's writer hold,
which cybermonklabs/legion#4698 adds. No reading of pods, processes or connections proves that no
writer runs, and a bucket deleted under a writer loses every write it acknowledged. Nothing was
read or changed. To make a bucket that is absent, run envoy-infra buckets create.
`

// bestEffortWarning is printed on every check run, and with the usage, to standard error, before
// anything else: the proof is observation, not exclusion. Each way it names is a test that asserts
// today's outcome (buckets_test.go, TestBestEffort...). create takes no proof, and its run needs no
// writer stopped (createBuckets), so the warning and its instruction are not its to print; recreate
// refuses before it prints anything else (recreateRefusal).
const bestEffortWarning = `envoy-infra buckets: WARNING: this proof is best effort. A writer gets past it three ways:
  - an omitted host or dev box: only the --kube-context, --host and --this-machine you name are checked;
  - a renamed binary: a host is checked by process name (envoy-listener, listener, dispatch, ...);
  - an unheld restore: check holds nothing through a later nats stream restore.
Before a check, stop every envoy-listener of the deployment (and every envoy-dispatch, for
envoy_ci_state) and confirm on each host that it has ended. A create takes no proof: it makes
only a bucket that is absent. A server-side hold, which leaves no writer a connection, replaces
this.
`

const usage = `usage: envoy-infra buckets create|recreate|check --nats URL [options]
       envoy-infra buckets create --break-glass --operator NAME --bucket NAME --nats URL

  create     make every copied bucket (or each --bucket) that is absent, at --replicas copies
             with its own configuration, and take no writer proof: an absent bucket holds no
             write to lose. A bucket there at those copies with that configuration is present
             and left as it is (exit 0); one there with other copies or another configuration
             refuses the run, naming each difference, and nothing is created (exit 1)
  recreate   refused until INFRA-4219's writer hold ships it (cybermonklabs/legion#4698): it
             reads, connects to and deletes nothing, whatever its flags (exit 1)
  check      prove no writer runs, and change nothing

The writers are every envoy-listener of the deployment, and every envoy-dispatch when the CI state
bucket is among the buckets. check requires the proof, and create refuses its flags, which would
check nothing:
  --kube-context CTX   a Kubernetes context to check, repeatable ("" is kubectl's current one)
  --host TARGET        a machine to query through ssh, repeatable
  --ssh PROGRAM        the program run as ssh (default ssh), such as a wrapper over SSM
  --this-machine       the machine the command runs on, queried here with no ssh; in a container
                       it sees the machine's processes only with the machine's PID namespace
                       (docker --pid host, compose pid: host)
  --connz URL          a hub server's monitoring URL whose connections to read, repeatable
  --first-deployment   also require that the server holds no notification stream and no bucket

  --nats URL           the NATS server, as a user that may read and create the buckets'
                       streams (NATS_NKEY_SEED_FILE or NATS_NKEY_SEED in the environment)
  --inbox-prefix P     the prefix of the run's reply inboxes, which its user must subscribe
                       (default _INBOX_envoy_buckets, the buckets user's; _INBOX. for a
                       listener's user, which subscribes _INBOX.>)
  --bucket NAME        a copied bucket to act on, repeatable (default: every one, for create)
  --replicas N         the copies a bucket is created with, 1 to 5 (default ENVOY_NATS_REPLICAS, else 1)

create sends a request its user's grant refuses again every 5s, for up to 180s from its start, since
a grant just applied reaches a hub at its next reload; then the refusal fails the run.

Until the auth service's cutover, create runs with a listener's credential instead:
  --break-glass        run create as a listener's NATS user (NATS_NKEY_SEED_FILE or
                       NATS_NKEY_SEED), its replies on the default _INBOX. prefix; each --bucket
                       is judged and made as create does, and nothing is updated or deleted
  --operator NAME      who runs it, named in the line logged before each create; required with
                       --break-glass
`

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func command(ctx context.Context, args []string, stdout, stderr io.Writer, run Runner, client *http.Client) int {
	if len(args) > 0 && args[0] == "recreate" {
		fmt.Fprint(stderr, recreateRefusal)
		return 1
	}
	if len(args) == 0 || (args[0] != "create" && args[0] != "check") {
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
	var natsURL, operator, inboxPrefix string
	var breakGlass bool
	replicas := 0
	flags.Var((*stringList)(&f.kubeContexts), "kube-context", "a Kubernetes context to check")
	flags.Var((*stringList)(&f.hosts), "host", "a machine to query through ssh")
	flags.BoolVar(&f.thisMachine, "this-machine", false, "the machine the command runs on, queried with no ssh")
	flags.StringVar(&f.ssh, "ssh", "ssh", "the program run as ssh")
	flags.Var((*stringList)(&f.connz), "connz", "a hub server's monitoring URL")
	flags.BoolVar(&f.firstDeployment, "first-deployment", false, "the server has never served a listener")
	flags.StringVar(&natsURL, "nats", "", "the NATS server")
	flags.StringVar(&inboxPrefix, "inbox-prefix", InboxPrefix, "the prefix of the run's reply inboxes")
	flags.Var(&buckets, "bucket", "a copied bucket")
	flags.IntVar(&replicas, "replicas", 0, "the copies a bucket is created with")
	flags.BoolVar(&breakGlass, "break-glass", false, "run create as a listener's NATS user")
	flags.StringVar(&operator, "operator", "", "who runs a --break-glass create")
	err := flags.Parse(args[1:])
	if action == "check" {
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
	// A bucket named twice is one bucket: it is read, logged and created or checked once.
	named := map[string]bool{}
	buckets = slices.DeleteFunc(buckets, func(bucket string) bool {
		again := named[bucket]
		named[bucket] = true
		return again
	})
	if refusal := createUsage(flags, action, breakGlass, operator, len(buckets)); refusal != "" {
		fmt.Fprintf(stderr, "envoy-infra buckets: %s\n", refusal)
		return 2
	}
	if breakGlass {
		inboxPrefix = nats.InboxPrefix
	}
	inbox, ok := inboxOption(inboxPrefix)
	if !ok {
		fmt.Fprintf(stderr, "envoy-infra buckets: --inbox-prefix is no inbox prefix: want one or more subject tokens joined by dots, such as %s or %s, with no wildcard and no white space\n%s", InboxPrefix, nats.InboxPrefix, usage)
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
	if action == "create" {
		return createCommand(ctx, stdout, stderr, natsURL, inbox, breakGlass, operator, buckets, replicas)
	}
	if len(f.kubeContexts) == 0 && len(f.hosts) == 0 && !f.thisMachine {
		fmt.Fprintln(stderr, "envoy-infra buckets: name every place a writer of the deployment runs: --kube-context for each cluster, --host for each machine and --this-machine for the one the command runs on; --first-deployment proves no writer absent and needs them too")
		return 2
	}
	f.run, f.http, f.pid = run, client, os.Getpid()
	f.dispatch = slices.Contains(buckets, bus.CIStateBucket)

	nc, err := bus.Connect([]string{natsURL}, append([]bus.ConnectOption{bus.WithName("envoy-infra-buckets"), bus.WithEnvironment(operatorEnvironment)}, inbox...)...)
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra buckets: connect to %s: %v\n", bus.NATSURLWithoutUserinfo(natsURL), err)
		return 1
	}
	defer nc.Close()
	js := nc.JS()

	if blockers := f.prove(ctx, stdout, js); len(blockers) > 0 {
		refuse(stderr, action, blockers)
		return 1
	}
	fmt.Fprintln(stdout, "proven: no writer of the buckets runs")
	return 0
}

// refuse writes the refusal and each blocker.
func refuse(stderr io.Writer, action string, blockers []string) {
	fmt.Fprintf(stderr, "envoy-infra buckets %s: refused: a writer of the buckets may still run, and a bucket created under it loses every write the deleted one acknowledged\n", action)
	for _, blocker := range blockers {
		fmt.Fprintf(stderr, "  blocked: %s\n", blocker)
	}
}

// createUsage is why the flags of a run misuse create's flags, or "" when they do not: create
// takes no writer proof, so it takes no proof flag, which would check nothing; --break-glass is
// create's alone, takes its replies on _INBOX. with no --inbox-prefix, and needs --operator and
// each --bucket it creates; --operator names a break-glass run and nothing else. An operator name
// holding a control character, a line break among them, is refused, so the run's log line stays
// one line.
func createUsage(flags *flag.FlagSet, action string, breakGlass bool, operator string, buckets int) string {
	if !breakGlass && operator != "" {
		return "--operator names who runs a --break-glass create; leave it off without --break-glass"
	}
	if breakGlass && action != "create" {
		return "--break-glass is create's alone: it creates an absent bucket and does nothing else, so it does not prove that no writer runs (check)"
	}
	if action != "create" {
		return ""
	}
	var proof []string
	inboxSet := false
	flags.Visit(func(set *flag.Flag) {
		switch set.Name {
		case "kube-context", "host", "this-machine", "ssh", "connz", "first-deployment":
			proof = append(proof, "--"+set.Name)
		case "inbox-prefix":
			inboxSet = true
		}
	})
	if len(proof) > 0 {
		return "create takes no writer proof, so " + strings.Join(proof, " and ") + " would check nothing: leave them off (check takes them)"
	}
	if !breakGlass {
		return ""
	}
	if inboxSet {
		return "--break-glass takes its replies on " + nats.InboxPrefix + " already: leave --inbox-prefix off"
	}
	if strings.TrimSpace(operator) == "" || strings.ContainsFunc(operator, unicode.IsControl) {
		return "--break-glass needs --operator, who runs it, on one line: the run logs it"
	}
	if buckets == 0 {
		return "--break-glass needs each --bucket it creates"
	}
	return ""
}

// inboxOption is the connection option the prefix --inbox-prefix names, or false when prefix is
// no inbox prefix: none for nats.go's own _INBOX. (written with or without its final dot), which a
// listener's user subscribes (_INBOX.>), and bus.WithInboxPrefix for any other, such as
// InboxPrefix, the buckets user's. A prefix is one or more subject tokens joined by dots, with no
// wildcard and no white space.
func inboxOption(prefix string) ([]bus.ConnectOption, bool) {
	prefix = strings.TrimSuffix(prefix, ".")
	if prefix == "" || strings.ContainsAny(prefix, "*>") || strings.ContainsFunc(prefix, unicode.IsSpace) || slices.Contains(strings.Split(prefix, "."), "") {
		return nil, false
	}
	if prefix+"." == nats.InboxPrefix {
		return nil, true
	}
	return []bus.ConnectOption{bus.WithInboxPrefix(prefix)}, true
}

// createCommand is create: it connects to natsURL as the user the environment names
// (NATS_NKEY_SEED_FILE or NATS_NKEY_SEED), its replies under the prefix inbox sets
// (--inbox-prefix), and makes each of buckets that is absent (createBuckets) until ctx ends, which
// main makes an interrupt or SIGTERM do. The default is the buckets user's prefix, InboxPrefix; a
// listener's user, which subscribes _INBOX.> alone, runs it with _INBOX., as a Job running as the
// clustered hub's gke user does. A break-glass run is a listener user's until the auth service's
// cutover, on _INBOX., and logs one line on standard error before each create, naming the
// operator, the hub, the bucket and that the run is break-glass.
//
// The connection logs no line for the server's refusal of a publish
// (bus.WithPublishRefusalsUnlogged): each refused request answers that refusal, and the run sends
// it again for up to refusalWait and says so once (untilGranted), where a line at ERROR for each
// refused send would fill that wait.
func createCommand(ctx context.Context, stdout, stderr io.Writer, natsURL string, inbox []bus.ConnectOption, breakGlass bool, operator string, buckets []string, replicas int) int {
	name := "envoy-infra-buckets"
	if breakGlass {
		name = "envoy-infra-buckets-break-glass"
	}
	hub := bus.NATSURLWithoutUserinfo(natsURL)
	nc, err := bus.Connect([]string{natsURL}, append([]bus.ConnectOption{bus.WithName(name), bus.WithEnvironment(operatorEnvironment), bus.WithPublishRefusalsUnlogged()}, inbox...)...)
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra buckets: connect to %s: %v\n", hub, err)
		return 1
	}
	defer nc.Close()
	announce := func(string) {}
	if breakGlass {
		announce = func(bucket string) {
			fmt.Fprintf(stderr, "envoy-infra buckets: break-glass: operator %s creates %s on %s, as the environment's NATS user and with no writer proof\n", operator, bucket, hub)
		}
	}
	return createBuckets(ctx, stdout, stderr, nc.Conn, nc.JS(), buckets, replicas, announce)
}

// createBuckets reads each of buckets' streams (STREAM.INFO KV_<bucket>) and judges each against its
// own stream at replicas copies (judgeBucket):
//
//   - a bucket the server reports missing is absent, and is made below;
//   - one that matches is present: it is left as it is, and the run says so;
//   - one that differs refuses the run before anything is created, naming each difference: a bucket
//     there at other copies most often says the copies asked for are not the deployment's, and the
//     absent buckets would be made at those copies too;
//   - a read answering anything but "not found" fails the run, and nothing is created.
//
// It then makes each absent bucket with no writer proof, through bus.CreateMissingCopiedBucket,
// which creates only a bucket the server does not hold, sends a lookup or create the server left
// unanswered again, and opens, raises and updates no bucket it finds there: a create the server
// answers "stream name already in use" is answered so, with no read or update of that stream
// after it, whatever the server reports for the stream, so the run sends no STREAM.UPDATE on any
// server version. announce is called before each create. A create that loses a race to another
// client - the bucket found by the create's own lookup, or made between that lookup and the create
// with another configuration, which the server answers "stream name already in use"
// (bus.ErrBucketExists) - reads the stream again and applies the same rule: present when it matches,
// refused naming each difference when it does not, and the run stops there. A bucket made between
// the lookup and the create with the same configuration is the one this create asked for, which
// JetStream answers as made.
//
// A read or a create the server refuses under the connection's grant is sent again until
// refusalWait has passed since the run began (untilGranted). Once ctx has ended the run stops where
// it is, creates nothing more and fails naming ctx's cause (stopped): every read and wait ends with
// ctx, the check before each create reads it, and the create sends no request once it has ended.
func createBuckets(ctx context.Context, stdout, stderr io.Writer, conn *nats.Conn, js nats.JetStreamContext, buckets []string, replicas int, announce func(bucket string)) int {
	deadline := time.Now().Add(refusalWait)
	version := conn.ConnectedServerVersion()
	var absent, differ []string
	for _, bucket := range buckets {
		missing, differences, err := judgeBucket(ctx, stderr, deadline, js, version, bucket, replicas)
		switch {
		case ctx.Err() != nil:
			return stopped(ctx, stderr)
		case err != nil:
			fmt.Fprintf(stderr, "envoy-infra buckets: %v\n", err)
			return 1
		case missing:
			absent = append(absent, bucket)
		case len(differences) > 0:
			differ = append(differ, fmt.Sprintf("KV_%s: %s", bucket, strings.Join(differences, "; ")))
		}
	}
	if len(differ) > 0 {
		fmt.Fprintf(stderr, "envoy-infra buckets create: refused: a bucket named is there with other copies or another configuration than its own at %d copies, and is left as it is; nothing is created\n", replicas)
		for _, line := range differ {
			fmt.Fprintf(stderr, "  differs: %s\n", line)
		}
		return 1
	}
	for _, bucket := range buckets {
		if !slices.Contains(absent, bucket) {
			fmt.Fprintf(stdout, "%s present at %d copies with its own configuration: left as it is\n", bucket, replicas)
		}
	}
	for _, bucket := range absent {
		if ctx.Err() != nil {
			return stopped(ctx, stderr)
		}
		announce(bucket)
		_, err := untilGranted(ctx, stderr, deadline, "create KV_"+bucket, func() (bus.KeyValue, error) {
			return bus.CreateMissingCopiedBucket(ctx, conn, js, bucket, replicas)
		})
		if err == nil {
			fmt.Fprintf(stdout, "created %s (%d copies)\n", bucket, replicas)
			continue
		}
		if ctx.Err() != nil {
			return stopped(ctx, stderr)
		}
		if !errors.Is(err, bus.ErrBucketExists) {
			fmt.Fprintf(stderr, "envoy-infra buckets: create %s: %v\n", bucket, err)
			return 1
		}
		missing, differences, readErr := judgeBucket(ctx, stderr, deadline, js, version, bucket, replicas)
		switch {
		case ctx.Err() != nil:
			return stopped(ctx, stderr)
		case readErr != nil:
			fmt.Fprintf(stderr, "envoy-infra buckets create: KV_%s was made by another client after the read (%v), and the read again failed: %v\n", bucket, err, readErr)
			return 1
		case missing:
			fmt.Fprintf(stderr, "envoy-infra buckets create: KV_%s was made by another client after the read (%v), and the read again reports it missing\n", bucket, err)
			return 1
		case len(differences) > 0:
			fmt.Fprintf(stderr, "envoy-infra buckets create: refused: KV_%s was made by another client after the read (%v), with other copies or another configuration than its own at %d copies, and is left as it is\n", bucket, err, replicas)
			fmt.Fprintf(stderr, "  differs: KV_%s: %s\n", bucket, strings.Join(differences, "; "))
			return 1
		}
		fmt.Fprintf(stdout, "%s present at %d copies with its own configuration, made by another client after the read: left as it is\n", bucket, replicas)
	}
	return 0
}

// stopped fails a create whose ctx has ended, naming ctx's cause: main ends ctx on an interrupt or
// SIGTERM, whose cause names the signal. The run creates nothing after it; a bucket a line above
// says was created stays.
func stopped(ctx context.Context, stderr io.Writer) int {
	fmt.Fprintf(stderr, "envoy-infra buckets create: stopped: %v; nothing more is created\n", context.Cause(ctx))
	return 1
}

// refusalWait and refusalRetry bound create's requests the server refuses under the connection's
// grant (bus.ErrPublishDenied, which the bus names as soon as the server reports the refusal). A
// grant just applied reaches a hub only when the hub's configuration reloader has reloaded its
// auth.conf, which the apply that made the grant cannot see, so a run right after that apply, such
// as a Job the same apply starts, can be refused for a while. A refused request changes nothing, so
// it is sent again every refusalRetry until refusalWait has passed since the run began, and only
// then fails the run. A test lowers both.
var (
	refusalWait  = 180 * time.Second
	refusalRetry = 5 * time.Second
)

// untilGranted is send's answer, sent again every refusalRetry while the server refuses it under the
// connection's grant, another send fits before deadline and ctx has not ended. Its first refusal is
// logged on stderr, naming what and the refusal, and no later one is: the connection logs none of
// them either (createCommand). A wait between two sends ends when ctx does, and the answer is then
// ctx's error, with no send after it.
func untilGranted[T any](ctx context.Context, stderr io.Writer, deadline time.Time, what string, send func() (T, error)) (T, error) {
	for logged := false; ; logged = true {
		value, err := send()
		if ctx.Err() != nil || !errors.Is(err, bus.ErrPublishDenied) || !time.Now().Add(refusalRetry).Before(deadline) {
			return value, err
		}
		if !logged {
			fmt.Fprintf(stderr, "envoy-infra buckets: %s: refused under this user's grant, sent again every %s for up to %s from the start, since a grant just applied reaches the hub at its next reload: %v\n", what, refusalRetry, refusalWait, err)
		}
		select {
		case <-ctx.Done():
			return value, ctx.Err()
		case <-time.After(refusalRetry):
		}
	}
}

// judgeBucket reads bucket's stream and reports whether the server holds it, and when it does, each
// way it differs from the stream the bucket's create makes at replicas copies on a server reporting
// version: bus.CopiedBucketStreamConfig, the configuration the listeners' bus declares for the
// bucket made into its stream as nats.go makes it (bucketDifferences). A read the server refuses
// under the connection's grant is sent again until deadline (untilGranted). A read answering
// anything else but "not found" is an error naming the bucket. Each read ends with ctx, and at
// the bus's request bound (bus.RequestTimeout) within it.
func judgeBucket(ctx context.Context, stderr io.Writer, deadline time.Time, js nats.JetStreamContext, version, bucket string, replicas int) (missing bool, differences []string, err error) {
	info, err := untilGranted(ctx, stderr, deadline, "read KV_"+bucket, func() (*nats.StreamInfo, error) {
		read, cancel := context.WithTimeout(ctx, bus.RequestTimeout)
		defer cancel()
		return js.StreamInfo("KV_"+bucket, nats.Context(read))
	})
	if errors.Is(err, nats.ErrStreamNotFound) {
		return true, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("read %s: %w", bucket, err)
	}
	want, ok := bus.CopiedBucketStreamConfig(bucket, replicas, version)
	if !ok {
		return false, nil, fmt.Errorf("read %s: no copied bucket's stream", bucket)
	}
	return false, bucketDifferences(bucket, info.Config, want), nil
}

// bucketDifferences names each way have, the stream of bucket the server holds, differs from want,
// the stream its create makes (bus.CopiedBucketStreamConfig, a copy of nats.go@v1.55.0 kv.go:437-523,
// CreateKeyValue): its copies, its history (a key's values kept), its maximum age, storage, byte and
// value-size limits and compression, its discard policy and direct get, the subjects it takes, and
// the streams it mirrors or sources and the subjects it republishes to, each of which a copied
// bucket's own stream has none of. The session bucket's maximum age is left out: the listeners
// set it on the running bucket from ENVOY_SESSION_MAX_AGE (bus.SetKeyValueMaxAge,
// internal/session), so a deployment that sets that variable holds the bucket at another age than
// the one it was made with, which is its own.
func bucketDifferences(bucket string, have, want nats.StreamConfig) []string {
	fields := []struct {
		name       string
		same       bool
		have, want any
	}{
		{"replicas", have.Replicas == want.Replicas, have.Replicas, want.Replicas},
		{"history", have.MaxMsgsPerSubject == want.MaxMsgsPerSubject, have.MaxMsgsPerSubject, want.MaxMsgsPerSubject},
		{"max age", have.MaxAge == want.MaxAge || bucket == bus.SessionBucket, have.MaxAge, want.MaxAge},
		{"storage", have.Storage == want.Storage, have.Storage, want.Storage},
		{"max bytes", have.MaxBytes == want.MaxBytes, have.MaxBytes, want.MaxBytes},
		{"max value size", have.MaxMsgSize == want.MaxMsgSize, have.MaxMsgSize, want.MaxMsgSize},
		{"compression", have.Compression == want.Compression, have.Compression, want.Compression},
		{"discard policy", have.Discard == want.Discard, have.Discard, want.Discard},
		{"direct get", have.AllowDirect == want.AllowDirect, have.AllowDirect, want.AllowDirect},
		{"subjects", slices.Equal(have.Subjects, want.Subjects), listed(have.Subjects), listed(want.Subjects)},
		{"mirror", reflect.DeepEqual(have.Mirror, want.Mirror), sourcesNamed(have.Mirror), sourcesNamed(want.Mirror)},
		{"sources", len(have.Sources)+len(want.Sources) == 0 || reflect.DeepEqual(have.Sources, want.Sources), sourcesNamed(have.Sources...), sourcesNamed(want.Sources...)},
		{"republish", reflect.DeepEqual(have.RePublish, want.RePublish), republished(have.RePublish), republished(want.RePublish)},
	}
	var differences []string
	for _, field := range fields {
		if !field.same {
			differences = append(differences, fmt.Sprintf("%s %v, want %v", field.name, field.have, field.want))
		}
	}
	return differences
}

// listed is subjects as a difference names them: joined by commas, or "none".
func listed(subjects []string) string {
	if len(subjects) == 0 {
		return "none"
	}
	return strings.Join(subjects, ",")
}

// sourcesNamed is the streams a mirror or sources name, as a difference names them: each stream's
// name, joined by commas, or "none".
func sourcesNamed(sources ...*nats.StreamSource) string {
	var names []string
	for _, source := range sources {
		if source != nil {
			names = append(names, source.Name)
		}
	}
	return listed(names)
}

// republished is a stream's republish as a difference names it: the subjects it republishes and
// where to, or "none".
func republished(republish *nats.RePublish) string {
	if republish == nil {
		return "none"
	}
	if republish.HeadersOnly {
		return republish.Source + " to " + republish.Destination + " (headers only)"
	}
	return republish.Source + " to " + republish.Destination
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
