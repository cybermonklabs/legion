// Package buckets is `envoy-infra buckets`: the one command that creates the buckets a listener
// copies (bus.CopiedBuckets), recreates one, restores one from a backup, and backs them up.
//
// A write such a bucket acknowledged is lost when the bucket is deleted and created again, and the
// listener that made it told its caller it landed: the ack names the stream and its sequence, never
// the bucket's incarnation. So no listener creates one, and this command changes a bucket only
// inside the NATS writer hold (package writerhold) on the clustered hub: while the hold is set, no
// client but the operator and the auth service is connected to any server of the hub, none can
// connect, and every server has restarted since the hold began. The hold is exclusion at the
// server, so no writer gets past it by running where nobody looked, under another name, after a
// check, or during a restore.
//
// `create`, `recreate` and `restore` each run as their workflow's one command (buckets-create,
// buckets-recreate, buckets-restore): with no --hold, the command prepares and sets a hold of its
// workflow itself, its preflight recording each target bucket as possibly absent; with --hold it
// runs in that hold, set already or set again (writerhold.RunHeld). Each then changes the buckets on
// the operator's one connection under the held grant (natsgrant.Operator(held)), checks each
// protected stream against what it did (writerhold.Postcheck), and writes its done entry. The hold
// stays set; `envoy-infra hold release --hold <id>` ends it.
//
// A create that finds every bucket it names already there changes nothing, so it needs no hold:
// it reads each bucket's stream under the operator's released grant, says that every one exists,
// and exits 0.
//
// `backup` is a held command too, the first of a buckets-recreate or buckets-restore hold: with
// --workflow recreate|restore it prepares and sets that hold, or with --hold it runs in one. It
// snapshots each bucket's stream into a directory of its own, with a manifest of each archive's
// sha256 and stream state, on the held grant, since a snapshot names its own deliver subject.
// `restore --from` validates that manifest before anything else and restores each named bucket
// from it, in the same hold.
//
// One machine with no auth service, broker or hold (the single-node compose file) makes its
// missing buckets with `create --break-glass`: as the environment's NATS user, creating only
// buckets the server reports missing, deleting nothing, and only while the server's connection list
// (--connz) holds no client but the command's own, so no listener runs at that create.
package buckets

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/infra/internal/flagerr"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// The commands of `envoy-infra buckets`, each in the hold's table: create, recreate and restore
// each their workflow's final command, and backup the first command of the recreate and restore
// workflows.
const (
	CommandCreate   = "buckets create"
	CommandRecreate = "buckets recreate"
	CommandRestore  = "buckets restore"
	CommandBackup   = "buckets backup"
)

// backupWorkflows are the holds a backup with no --hold prepares, by its --workflow.
var backupWorkflows = map[string]holdrecord.Workflow{
	"recreate": holdrecord.WorkflowBucketsRecreate,
	"restore":  holdrecord.WorkflowBucketsRestore,
}

// env is what a run reaches outside the process. Command gives the real one; a test gives its own.
type env struct {
	records func(ctx context.Context, h writerhold.HubFlags, signing bool) (*holdrecord.Client, error)
	hub     func(ctx context.Context, h writerhold.HubFlags) (*writerhold.Operator, error)
	now     func() time.Time
	http    *http.Client
	// restoreRuns is how many times a restore of one bucket is run before it stops: 2, its one
	// rerun; a test's control runs it once.
	restoreRuns int
}

func realEnv() env {
	return env{
		records: func(ctx context.Context, h writerhold.HubFlags, signing bool) (*holdrecord.Client, error) {
			return h.Records(ctx, signing)
		},
		hub:         func(ctx context.Context, h writerhold.HubFlags) (*writerhold.Operator, error) { return h.Operator(ctx) },
		now:         time.Now,
		http:        &http.Client{Timeout: 10 * time.Second},
		restoreRuns: 2,
	}
}

// Command runs `envoy-infra buckets` with args, the words after "buckets", and returns its exit
// code: 0 done, 1 refused or failed, 2 a usage error or an UNKNOWN reading.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return command(ctx, args, stdout, stderr, realEnv())
}

// operatorEnvironment is the environment a break-glass create connects with: the process's own,
// but for ENVOY_ALLOW_REMOTE_NATS, which it always answers "1". --nats names the server explicitly.
func operatorEnvironment(name string) (string, bool) {
	if name == bus.AllowRemoteEnvVar {
		return "1", true
	}
	return os.LookupEnv(name)
}

const usage = `usage: envoy-infra buckets create|recreate|restore [--hold ID] --reason R [options]
       envoy-infra buckets backup (--workflow recreate|restore | --hold ID) --reason R --to DIR [options]
       envoy-infra buckets create --break-glass --operator NAME --bucket NAME --nats URL --connz URL

  create     create every copied bucket that is missing (or each --bucket), inside a
             buckets-create hold; with none missing it changes nothing and needs no hold
  recreate   delete each --bucket and create it again, inside the buckets-recreate hold
             "buckets backup --workflow recreate" set (--hold ID), once that backup is done
  restore    restore each --bucket from --from DIR, a backup's directory, inside a
             buckets-restore hold; the backup's manifest is validated first
  backup     snapshot each --bucket (default: every one) into --to DIR, inside a buckets-recreate or
             buckets-restore hold (--workflow recreate|restore, or --hold ID), as its first command

With no --hold, create, recreate and restore prepare and set a hold of their workflow themselves
(--minters-report, as envoy-infra hold set takes it); with --hold they run in that hold. The hold
stays set: envoy-infra hold release --hold ID ends it.

  --hold ID            the hold to run in
  --reason R           why, as the hold records it
  --bucket NAME        a copied bucket to act on, repeatable (default: every one, for create and backup)
  --replicas N         the copies a bucket is created with, 1 to 5 (default ENVOY_NATS_REPLICAS, else 1)
  --from DIR           restore's backup directory
  --to DIR             backup's directory, new or empty

The hold's flags, as envoy-infra hold takes them: --broker-url, --token-file, --verify-keys,
--signing-key-secret, --minters-report, --nats URL (repeatable), --probe-token-file,
--kube-namespace, --statefulset and --configmap.

On one machine with no auth service, a missing bucket is created as the environment's NATS user
(NATS_NKEY_SEED_FILE or NATS_NKEY_SEED) instead:
  --break-glass        create each --bucket the server reports missing; refused when any --bucket
                       exists or any client but this one is connected; updates and deletes nothing
  --operator NAME      who runs it, named in the line the run logs
  --connz URL          the server's monitoring URL, whose connections it reads first, repeatable
`

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// options are a run's flags.
type options struct {
	writerhold.HubFlags
	action, holdID, reason, from, to, operator, workflow string
	buckets, connz                                       stringList
	breakGlass                                           bool
	replicas                                             int
}

func command(ctx context.Context, args []string, stdout, stderr io.Writer, e env) int {
	actions := []string{"create", "recreate", "restore", "backup"}
	if len(args) == 0 || !slices.Contains(actions, args[0]) {
		fmt.Fprint(stderr, usage)
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			return 0
		}
		return 2
	}
	o, code := parse(args, stderr)
	if code >= 0 {
		return code
	}
	if o.breakGlass {
		return breakGlassCreate(ctx, stdout, stderr, o, e.http)
	}
	return report(stderr, o.action, run(ctx, o, stdout, e))
}

// parse reads args into options, or answers the exit code of a usage error or of --help.
func parse(args []string, stderr io.Writer) (options, int) {
	o := options{action: args[0]}
	flags := flag.NewFlagSet("envoy-infra buckets "+o.action, flag.ContinueOnError)
	// The flag package's own message quotes a value it could not parse, so what it writes is
	// printed only after --help, whose listing holds no word given (see below).
	var parsed bytes.Buffer
	flags.SetOutput(&parsed)
	o.HubFlags.Register(flags)
	flags.StringVar(&o.holdID, "hold", "", "the hold to run in")
	flags.StringVar(&o.reason, "reason", "", "why, as the hold records it")
	flags.Var(&o.buckets, "bucket", "a copied bucket")
	flags.IntVar(&o.replicas, "replicas", 0, "the copies a bucket is created with")
	flags.StringVar(&o.from, "from", "", "restore's backup directory")
	flags.StringVar(&o.to, "to", "", "backup's directory")
	flags.StringVar(&o.workflow, "workflow", "", "backup's hold, with no --hold: recreate or restore")
	flags.BoolVar(&o.breakGlass, "break-glass", false, "create each missing --bucket as the environment's NATS user")
	flags.StringVar(&o.operator, "operator", "", "who runs a --break-glass create")
	flags.Var(&o.connz, "connz", "the server's monitoring URL")
	// No word the command was given is echoed where the parse could not read it as one of its own:
	// a seed may have been pasted there, and `envoy-infra seed` and `render` echo none either. A
	// stray argument is named by its position, a malformed value by its flag, and any other word
	// the parse refused not at all.
	if err := flags.Parse(args[1:]); err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprint(stderr, parsed.String())
			return o, 0
		case strings.HasPrefix(err.Error(), "flag needs an argument: -"):
			fmt.Fprintf(stderr, "envoy-infra buckets: %v\n%s", err, usage)
		default:
			if name := flagerr.Malformed(flags, err); name != "" {
				fmt.Fprintf(stderr, "envoy-infra buckets: --%s: a malformed value (not echoed: a seed may have been pasted there)\n%s", name, usage)
			} else {
				fmt.Fprintf(stderr, "envoy-infra buckets: a flag this command does not take, or a malformed one (not echoed: a seed may have been pasted there)\n%s", usage)
			}
		}
		return o, 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "envoy-infra buckets: word %d after \"buckets\" is no flag and no flag's value (not echoed: a seed may have been pasted there)\n%s", len(args)-flags.NArg()+1, usage)
		return o, 2
	}
	// A flag's value reaches the lines the command writes and the hold's records, so a value that
	// holds an nkey seed (bus.HoldsSeed), whole or inside it, is refused naming the flag, never the
	// value, before anything reads it, as seed and render refuse one.
	var seeded, given []string
	replicasSet := false
	flags.Visit(func(set *flag.Flag) {
		given = append(given, set.Name)
		replicasSet = replicasSet || set.Name == "replicas"
		if bus.HoldsSeed(set.Value.String()) {
			seeded = append(seeded, "--"+set.Name)
		}
	})
	if len(seeded) > 0 {
		fmt.Fprintf(stderr, "envoy-infra buckets: %s: an nkey seed, which is never an argument (not echoed)\n", strings.Join(seeded, ", "))
		return o, 2
	}
	for _, bucket := range o.buckets {
		if _, ok := bus.CopiedBucketConfig(bucket, 1); !ok {
			fmt.Fprintf(stderr, "envoy-infra buckets: %s is no bucket a listener copies (%s)\n", bucket, strings.Join(bus.CopiedBuckets(), ", "))
			return o, 2
		}
	}
	// A bucket named twice is one bucket: it is read, logged and changed once.
	named := map[string]bool{}
	o.buckets = slices.DeleteFunc(o.buckets, func(bucket string) bool {
		again := named[bucket]
		named[bucket] = true
		return again
	})
	if problem := o.usageProblem(given); problem != "" {
		fmt.Fprintf(stderr, "envoy-infra buckets %s: %s\n%s", o.action, problem, usage)
		return o, 2
	}
	if len(o.buckets) == 0 {
		o.buckets = bus.CopiedBuckets()
	}
	// The deployment's replica count: --replicas, or else ENVOY_NATS_REPLICAS read as every program
	// of the deployment reads it (bus.ParseNATSReplicas). Either outside 1 to bus.MaxReplicas is a
	// usage error, before anything connects.
	if replicasSet {
		if o.replicas < 1 || o.replicas > bus.MaxReplicas {
			fmt.Fprintf(stderr, "envoy-infra buckets: --replicas %d is no replica count: want a whole number of copies from 1 to %d\n", o.replicas, bus.MaxReplicas)
			return o, 2
		}
	} else {
		var err error
		if o.replicas, err = bus.ParseNATSReplicas(os.Getenv(bus.ReplicasEnvVar)); err != nil {
			fmt.Fprintf(stderr, "envoy-infra buckets: %v\n", err)
			return o, 2
		}
	}
	return o, -1
}

// holdFlags are the flags of the hold, which a break-glass create does not take.
var holdFlags = []string{"hold", "reason", "broker-url", "token-file", "verify-keys", "signing-key-secret", "minters-report", "probe-token-file", "kube-namespace", "statefulset", "configmap"}

// usageProblem is what the flags given lack or misuse for the run's action, or "".
func (o options) usageProblem(given []string) string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if o.breakGlass {
		if o.action != "create" {
			return "--break-glass is create's alone: it creates a missing bucket and does nothing else"
		}
		var hold []string
		for _, name := range given {
			if slices.Contains(holdFlags, name) || name == "from" || name == "to" {
				hold = append(hold, "--"+name)
			}
		}
		if len(hold) > 0 {
			add("--break-glass runs with no hold, so %s would do nothing: leave them off", strings.Join(hold, " and "))
		}
		if strings.TrimSpace(o.operator) == "" || strings.ContainsFunc(o.operator, unicode.IsControl) {
			add("--break-glass needs --operator, who runs it, on one line: the run logs it")
		}
		if len(o.buckets) == 0 {
			add("--break-glass needs each missing --bucket it creates")
		}
		if len(o.NATS) != 1 {
			add("--break-glass needs one --nats, the server")
		}
		if len(o.connz) == 0 {
			add("--break-glass needs --connz, the server's monitoring URL: it creates only while no other client is connected")
		}
		return strings.Join(problems, "; ")
	}
	if o.operator != "" || len(o.connz) > 0 {
		add("--operator and --connz are --break-glass's; leave them off without it")
	}
	need := func(name, value string) {
		if value == "" {
			add("--%s is required", name)
		}
	}
	need("broker-url", o.BrokerURL)
	need("token-file", o.TokenFile)
	need("verify-keys", o.VerifyKeys)
	if len(o.NATS) == 0 {
		add("--nats is required: the hub's client URLs")
	}
	switch o.action {
	case "backup":
		need("to", o.to)
		if o.from != "" {
			add("--from is restore's")
		}
		if o.holdID == "" {
			if _, ok := backupWorkflows[o.workflow]; !ok {
				add("--workflow recreate or restore is required with no --hold: a backup is the first command of a buckets-recreate or buckets-restore hold")
			}
		} else if o.workflow != "" {
			add("--workflow is not taken with --hold: the hold's workflow is in its record")
		}
		need("signing-key-secret", o.KeySecret)
		need("reason", o.reason)
		if o.holdID == "" {
			need("minters-report", o.MintersReport)
			need("probe-token-file", o.ProbeTokenFile)
		}
		return strings.Join(problems, "; ")
	case "recreate":
		if len(o.buckets) == 0 {
			add("recreate needs each --bucket it deletes")
		}
		// A recreate deletes each bucket, so the hold's table admits it only after a done backup of
		// its hold: it runs in the hold `buckets backup --workflow recreate` set.
		if o.holdID == "" {
			add("recreate needs --hold, the buckets-recreate hold whose `buckets backup --workflow recreate --to DIR` is done")
		}
	case "restore":
		need("from", o.from)
		if len(o.buckets) == 0 {
			add("restore needs each --bucket it restores")
		}
	}
	if o.to != "" {
		add("--to is backup's")
	}
	if o.action != "restore" && o.from != "" {
		add("--from is restore's")
	}
	need("signing-key-secret", o.KeySecret)
	need("reason", o.reason)
	if o.holdID == "" {
		need("minters-report", o.MintersReport)
		need("probe-token-file", o.ProbeTokenFile)
	}
	return strings.Join(problems, "; ")
}

// report writes err's line and answers the exit code: 0 for none, 2 for an UNKNOWN reading, 1 for
// every refusal.
func report(stderr io.Writer, action string, err error) int {
	if err == nil {
		return 0
	}
	var unknown *writerhold.Unknown
	if errors.As(err, &unknown) || errors.Is(err, holdrecord.ErrUnknown) {
		line := err.Error()
		if !strings.HasPrefix(line, "UNKNOWN") {
			line = "UNKNOWN " + line
		}
		fmt.Fprintf(stderr, "envoy-infra buckets %s: %s\n", action, line)
		return 2
	}
	fmt.Fprintf(stderr, "envoy-infra buckets %s: %v\n", action, err)
	return 1
}

// run runs a held command, or a backup.
func run(ctx context.Context, o options, stdout io.Writer, e env) error {
	var manifest *Manifest
	if o.action == "restore" {
		// The archive is validated before the hold's preflight, so a hold is set only for a
		// restore whose archive is whole.
		m, err := ReadManifest(o.from, o.buckets)
		if err != nil {
			return err
		}
		manifest = m
	}
	op, err := e.hub(ctx, o.HubFlags)
	if err != nil {
		return err
	}
	c := writerhold.HeldCommand{
		HoldID:        o.holdID,
		Reason:        o.reason,
		MintersReport: o.MintersReport,
		Absent:        streams(o.buckets),
		Inputs:        map[string]string{},
	}
	for _, bucket := range o.buckets {
		c.Inputs["bucket."+bucket] = bucket
	}
	switch o.action {
	case "create":
		if o.holdID == "" {
			missing, err := missingBuckets(ctx, op, o.buckets)
			if err != nil {
				return err
			}
			if len(missing) == 0 {
				fmt.Fprintf(stdout, "every bucket named exists (%s): nothing to create, so no hold is set\n", strings.Join(o.buckets, ", "))
				return nil
			}
		}
		c.Workflow, c.Command = holdrecord.WorkflowBucketsCreate, CommandCreate
		c.Change = func(ctx context.Context, nc *nats.Conn, _ string) (map[string]writerhold.Change, map[string]string, error) {
			return createMissing(ctx, nc, stdout, o.buckets, o.replicas)
		}
	case "recreate":
		c.Workflow, c.Command = holdrecord.WorkflowBucketsRecreate, CommandRecreate
		c.Change = func(ctx context.Context, nc *nats.Conn, _ string) (map[string]writerhold.Change, map[string]string, error) {
			return recreate(ctx, nc, stdout, o.buckets, o.replicas)
		}
	case "restore":
		c.Workflow, c.Command = holdrecord.WorkflowBucketsRestore, CommandRestore
		for _, bucket := range o.buckets {
			c.Inputs["sha256."+bucket] = manifest.Buckets[bucket].SHA256
		}
		c.Change = func(ctx context.Context, nc *nats.Conn, _ string) (map[string]writerhold.Change, map[string]string, error) {
			return restoreAll(ctx, nc, stdout, o.from, manifest, o.buckets, e.restoreRuns)
		}
	case "backup":
		// The backup is the first command of the buckets-recreate and buckets-restore workflows: a
		// snapshot names its deliver subject, which the server publishes to with no check of the
		// requester's publish rights, so no snapshot runs outside a hold. It writes only the archive,
		// and a failure leaves the hub unchanged. With --hold it runs in that hold's own workflow.
		c.Command, c.Absent = CommandBackup, nil
		if o.holdID == "" {
			c.Workflow = backupWorkflows[o.workflow]
		}
		c.Change = func(ctx context.Context, nc *nats.Conn, _ string) (map[string]writerhold.Change, map[string]string, error) {
			outputs, err := Backup(ctx, nc, o.to, o.buckets, stdout)
			return nil, outputs, err
		}
	}
	records, err := e.records(ctx, o.HubFlags, true)
	if err != nil {
		return err
	}
	_, err = writerhold.RunHeld(ctx, records, op, c, e.now, stdout)
	return err
}

// streams are buckets' streams.
func streams(buckets []string) []string {
	out := make([]string, len(buckets))
	for i, bucket := range buckets {
		out[i] = "KV_" + bucket
	}
	return out
}

// missingBuckets reads each of buckets' streams on the operator's connection under the grant of
// the row's phase now, and returns those the server reports missing. A read the server answers
// with anything but "not found", or does not answer, is UNKNOWN, naming the bucket.
func missingBuckets(ctx context.Context, op *writerhold.Operator, buckets []string) ([]string, error) {
	nc, err := op.Connect(ctx)
	if err != nil {
		return nil, &writerhold.Unknown{Step: writerhold.StepPreflight, What: "connect to the hub as the operator", Err: err}
	}
	defer nc.Close()
	js, err := nc.JetStream(nats.Context(ctx))
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, bucket := range buckets {
		if _, err := js.StreamInfo("KV_" + bucket); errors.Is(err, nats.ErrStreamNotFound) {
			missing = append(missing, bucket)
		} else if err != nil {
			return nil, &writerhold.Unknown{Step: writerhold.StepPreflight, What: "read KV_" + bucket, Err: err}
		}
	}
	return missing, nil
}

// createMissing creates each of buckets the server reports missing, with its copied-bucket
// configuration at replicas copies, and leaves each present one as it is. A failure is a
// *writerhold.Left naming each bucket created before it and the failing one, possibly created, or
// a plain error when it created nothing.
func createMissing(ctx context.Context, nc *nats.Conn, stdout io.Writer, buckets []string, replicas int) (map[string]writerhold.Change, map[string]string, error) {
	js, err := nc.JetStream(nats.Context(ctx))
	if err != nil {
		return nil, nil, err
	}
	changes, outputs := map[string]writerhold.Change{}, map[string]string{}
	var left []string
	failed := func(err error) (map[string]writerhold.Change, map[string]string, error) {
		if len(left) == 0 {
			return nil, nil, err
		}
		return nil, nil, &writerhold.Left{What: strings.Join(left, ", "), Err: err}
	}
	for _, bucket := range buckets {
		if _, err := js.StreamInfo("KV_" + bucket); err == nil {
			fmt.Fprintf(stdout, "%s exists; left as it is\n", bucket)
			outputs["bucket."+bucket] = "exists"
			continue
		} else if !errors.Is(err, nats.ErrStreamNotFound) {
			return failed(&writerhold.Unknown{Step: writerhold.StepPostcheck, What: "read KV_" + bucket, Err: err})
		}
		if _, err := bus.CreateCopiedBucket(ctx, nc, js, bucket, replicas); err != nil {
			left = append(left, "KV_"+bucket+" possibly created")
			return failed(fmt.Errorf("create %s: %w", bucket, err))
		}
		fmt.Fprintf(stdout, "created %s (%d copies)\n", bucket, replicas)
		left = append(left, "KV_"+bucket+" created")
		changes["KV_"+bucket] = writerhold.Change{Kind: writerhold.ChangeRecreated}
		outputs["bucket."+bucket] = "created"
	}
	return changes, outputs, nil
}

// recreate deletes each of buckets and creates it again. A rerun after a delete finds the bucket
// missing and creates it. A failure is a *writerhold.Left naming what the command left: each
// bucket recreated before it, and the failing one deleted or possibly deleted.
func recreate(ctx context.Context, nc *nats.Conn, stdout io.Writer, buckets []string, replicas int) (map[string]writerhold.Change, map[string]string, error) {
	js, err := nc.JetStream(nats.Context(ctx))
	if err != nil {
		return nil, nil, err
	}
	changes, outputs := map[string]writerhold.Change{}, map[string]string{}
	var left []string
	failed := func(what string, err error) (map[string]writerhold.Change, map[string]string, error) {
		return nil, nil, &writerhold.Left{What: strings.Join(append(left, what), ", "), Err: err}
	}
	for _, bucket := range buckets {
		if err := js.DeleteStream("KV_" + bucket); err != nil && !errors.Is(err, nats.ErrStreamNotFound) {
			return failed("KV_"+bucket+" possibly deleted", fmt.Errorf("delete %s: %w", bucket, err))
		}
		fmt.Fprintf(stdout, "deleted %s\n", bucket)
		if _, err := bus.CreateCopiedBucket(ctx, nc, js, bucket, replicas); err != nil {
			return failed("KV_"+bucket+" deleted and possibly not created again", fmt.Errorf("create %s again: %w", bucket, err))
		}
		fmt.Fprintf(stdout, "created %s (%d copies)\n", bucket, replicas)
		left = append(left, "KV_"+bucket+" recreated")
		changes["KV_"+bucket] = writerhold.Change{Kind: writerhold.ChangeRecreated}
		outputs["bucket."+bucket] = "recreated"
	}
	return changes, outputs, nil
}
