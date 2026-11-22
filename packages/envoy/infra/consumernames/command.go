package consumernames

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/infra/internal/flagerr"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// The words of `envoy-infra consumer-names`.
const (
	actionStatus       = "status"
	actionBackup       = "backup"
	actionPrepare      = "prepare"
	actionCreateBucket = "create-bucket"
	actionReplay       = "replay"
	actionConvert      = "convert"
	actionCleanup      = "cleanup"
	actionRestore      = "restore"
	actionReplace      = "replace"
	actionEnroll       = "enroll"
	actionRepair       = "repair-sentinel"
)

// actions are the words, in the usage's order.
var actions = []string{actionStatus, actionBackup, actionPrepare, actionCreateBucket, actionReplay, actionConvert, actionCleanup,
	actionRestore, actionReplace, actionEnroll, actionRepair}

const usage = `usage: envoy-infra consumer-names status|backup|prepare|create-bucket|replay|convert|cleanup|restore|replace|enroll|repair-sentinel [options]

  status         [--hold ID]                               (the row released)
  backup         --out DIR                                 (the row released, outside a hold)
  prepare        --hold ID                                 (the row prepare)
  create-bucket  --hold ID [--replicas N]                  (the row held, and every command below)
  create-bucket  --hold ID --after-loss [--replay-from SEQ] [--backup DIR] [--replicas N]
  replay         --hold ID --key K --from SEQ [--final]
  convert        --hold ID
  cleanup        --hold ID [--gone PROJECT-KEY] [--final]  (cleanup and replace: a consumer-cleanup hold)
  restore        --hold ID (--prepare | --finish | --key K --from SEQ)
  replace        --hold ID --listener MACHINE-ID [--final]
  enroll         --hold ID --daemon PROJECT-KEY
  repair-sentinel --hold ID

  Every command: --broker-url U (the secrets broker), --token-file F (the operator's
  ServiceAccount token, for the broker's routes and the hub), --verify-keys K
  (BROKER_NATS_HOLD_SIGNING_KEYS) and --nats URL..., the hub's client URLs. Every command but
  status: --signing-key-secret projects/P/secrets/operator-only-envoy-hold-signing. Every held
  command reads the hub's Kubernetes objects: --kube-namespace (envoy), --statefulset (envoy-nats)
  and --configmap (envoy-nats-config), with the in-cluster configuration or KUBECONFIG's;
  restore --finish also restarts the hub's servers and needs --probe-token-file F, the hold probe's
  token. Dispatch's store: --dispatch-database-file F, a file holding its database URL with the
  operator's credential; each Legion daemon's: --daemon-database PROJECT-KEY=F, once per daemon.
  backup makes --out DIR, refusing one that exists, writes into it the walk of
  KV_envoy_session_consumers that create-bucket --after-loss --backup DIR loads, and prints its
  sha256, which that recovery records. cleanup and replace run only in a consumer-cleanup hold,
  whose record a run with --final completes; cleanup --gone P removes daemon P's consumers only when
  its store, at the address P's Deployment names, answers that its database does not exist.

Exit codes: 0 done; 1 refused, naming what; 2 a usage error, or UNKNOWN: a reading that is not
whole, which is never a pass.
`

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// options are a run's flags. The hold's and the hub's are writerhold.HubFlags, the ones `envoy-infra
// hold` and `envoy-infra buckets` take: the broker's routes, the operator's token and signing key,
// the hub's client URLs, the hold probe's token and the hub's Kubernetes objects.
type options struct {
	action string
	writerhold.HubFlags
	holdID               string
	dispatchDatabaseFile string
	daemonDatabases      stringList
	replicas             int
	afterLoss            bool
	replayFrom           uint64
	backup               string
	out                  string
	key                  string
	from                 uint64
	final                bool
	prepare, finish      bool
	listener             string
	daemon               string
	gone                 string
}

// daemonDatabase is the database file --daemon-database names for project, and whether it names
// one.
func (o options) daemonDatabase(project string) (string, bool) {
	for _, pair := range o.daemonDatabases {
		if p, file, _ := strings.Cut(pair, "="); p == project {
			return file, true
		}
	}
	return "", false
}

// daemonProjects are the projects --daemon-database names, in its order (parseDaemonDatabases).
func (o options) daemonProjects() []string {
	projects, _ := parseDaemonDatabases(o.daemonDatabases)
	return projects
}

// storeFile is the file the flag that names family's store names, "" when it names none, and that
// flag as an error names it: --dispatch-database-file for Dispatch, --daemon-database
// <project>=<file> for a daemon.
func (o options) storeFile(family string) (file, flagName string) {
	if project, ok := strings.CutPrefix(family, familyDaemonPrefix); ok {
		file, _ = o.daemonDatabase(project)
		return file, "--daemon-database " + project + "=<file>"
	}
	return o.dispatchDatabaseFile, "--dispatch-database-file"
}

// parseDaemonDatabases reads --daemon-database's entries, each <project>=<file>, as the commands
// and the census's readers take them (censusNamedProjects): the projects of the entries that are
// well formed, in their order, and the problem of each entry that is not <project>=<file> or names
// a project again.
func parseDaemonDatabases(entries []string) (projects, problems []string) {
	seen := map[string]bool{}
	for _, entry := range entries {
		project, file, ok := strings.Cut(entry, "=")
		switch {
		case !ok || project == "" || file == "":
			problems = append(problems, fmt.Sprintf("--daemon-database %q is not <project>=<file>", entry))
		case seen[project]:
			problems = append(problems, "--daemon-database names project "+project+" twice")
		default:
			projects = append(projects, project)
		}
		seen[project] = true
	}
	return projects, problems
}

// Command runs `envoy-infra consumer-names` with args, the words after "consumer-names", and
// returns its exit code.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runCommand(ctx, args, stdout, stderr, realEnv())
}

func runCommand(ctx context.Context, args []string, stdout, stderr io.Writer, e env) int {
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
	r := &run{o: o, e: e, stdout: stdout, stderr: stderr}
	defer r.close()
	return report(stderr, o.action, r.dispatch(ctx))
}

// parse reads args into options, or answers the exit code of a usage error (or of --help).
func parse(args []string, stderr io.Writer) (options, int) {
	o := options{action: args[0]}
	flags := flag.NewFlagSet("envoy-infra consumer-names "+o.action, flag.ContinueOnError)
	var parsed bytes.Buffer
	flags.SetOutput(&parsed)
	o.HubFlags.Register(flags)
	flags.StringVar(&o.holdID, "hold", "", "the hold's id")
	flags.StringVar(&o.dispatchDatabaseFile, "dispatch-database-file", "", "a file holding Dispatch's database URL")
	flags.Var(&o.daemonDatabases, "daemon-database", "a Legion daemon's project key and a file holding its database URL, <project>=<file>")
	flags.IntVar(&o.replicas, "replicas", 0, "the selection bucket's copies (default ENVOY_NATS_REPLICAS, else 1)")
	flags.BoolVar(&o.afterLoss, "after-loss", false, "create the selection bucket again after it was lost")
	flags.Uint64Var(&o.replayFrom, "replay-from", 0, "where a key with no record starts after a loss (default the stream's first sequence)")
	flags.StringVar(&o.backup, "backup", "", "a directory holding a backup of "+selectionStreamName+" to restore first")
	flags.StringVar(&o.out, "out", "", "backup's new directory, for the walk of "+selectionStreamName)
	flags.StringVar(&o.key, "key", "", "a logical name")
	flags.Uint64Var(&o.from, "from", 0, "the stream sequence the logical name starts at")
	flags.BoolVar(&o.final, "final", false, "the last run: replay's of a selection recovery, cleanup's or replace's of a consumer cleanup")
	flags.BoolVar(&o.prepare, "prepare", false, "restore's first step")
	flags.BoolVar(&o.finish, "finish", false, "restore's last step")
	flags.StringVar(&o.listener, "listener", "", "the listener's machine id")
	flags.StringVar(&o.daemon, "daemon", "", "the Legion daemon's project key")
	flags.StringVar(&o.gone, "gone", "", "cleanup's Legion daemon whose store is gone, by its project key: cleanup removes its consumers")
	if err := flags.Parse(args[1:]); err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprint(stderr, parsed.String())
			return o, 0
		case strings.HasPrefix(err.Error(), "flag needs an argument: -"):
			fmt.Fprintf(stderr, "envoy-infra consumer-names: %v\n%s", err, usage)
		default:
			if name := flagerr.Malformed(flags, err); name != "" {
				fmt.Fprintf(stderr, "envoy-infra consumer-names: --%s: a malformed value (not echoed)\n%s", name, usage)
			} else {
				fmt.Fprintf(stderr, "envoy-infra consumer-names: a flag this command does not take, or a malformed one (not echoed)\n%s", usage)
			}
		}
		return o, 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "envoy-infra consumer-names: word %d after \"consumer-names\" is no flag and no flag's value (not echoed)\n%s", len(args)-flags.NArg()+1, usage)
		return o, 2
	}
	// A flag's value reaches the lines the command writes and the hold's journal, so a value that
	// holds an nkey seed is refused naming the flag, never the value, as seed, buckets and hold
	// refuse one. --nats is read from its list: its flag.Func value prints nothing.
	var seeded []string
	flags.Visit(func(set *flag.Flag) {
		if bus.HoldsSeed(set.Value.String()) {
			seeded = append(seeded, "--"+set.Name)
		}
	})
	if slices.ContainsFunc(o.NATS, bus.HoldsSeed) {
		seeded = append(seeded, "--nats")
	}
	if len(seeded) > 0 {
		fmt.Fprintf(stderr, "envoy-infra consumer-names: %s: an nkey seed, which is never an argument (not echoed)\n", strings.Join(seeded, ", "))
		return o, 2
	}
	if problem := o.usageProblem(); problem != "" {
		fmt.Fprintf(stderr, "envoy-infra consumer-names %s: %s\n%s", o.action, problem, usage)
		return o, 2
	}
	return o, -1
}

// usageProblem is what the flags of a run lack or misuse for its action, or "".
func (o options) usageProblem() string {
	var problems []string
	add := func(p string) {
		if p != "" {
			problems = append(problems, p)
		}
	}
	need := func(flagName string, set bool) {
		if !set {
			add("--" + flagName + " is required")
		}
	}
	need("broker-url", o.BrokerURL != "")
	need("token-file", o.TokenFile != "")
	need("verify-keys", o.VerifyKeys != "")
	need("nats", len(o.NATS) > 0)
	if o.action != actionStatus {
		need("signing-key-secret", o.KeySecret != "")
	}
	if o.action != actionStatus && o.action != actionBackup {
		need("hold", o.holdID != "")
	}
	if o.action == actionBackup && o.holdID != "" {
		add("--hold is not backup's: backup runs outside a hold, with the row released")
	}
	if o.action == actionRestore && o.finish {
		// restore --finish restarts every declared server and proves each refuses the hold's probe.
		need("probe-token-file", o.ProbeTokenFile != "")
	}
	only := func(action, flagName string, set bool) {
		if set && o.action != action {
			add("--" + flagName + " is " + action + "'s alone")
		}
	}
	only(actionCreateBucket, "after-loss", o.afterLoss)
	only(actionCreateBucket, "backup", o.backup != "")
	only(actionCreateBucket, "replicas", o.replicas != 0)
	only(actionBackup, "out", o.out != "")
	only(actionReplace, "listener", o.listener != "")
	only(actionEnroll, "daemon", o.daemon != "")
	only(actionCleanup, "gone", o.gone != "")
	if o.final && o.action != actionReplay && o.action != actionCleanup && o.action != actionReplace {
		add("--final is replay's, cleanup's and replace's alone")
	}
	if o.action != actionReplay && o.action != actionRestore {
		if o.key != "" || o.from != 0 {
			add("--key and --from are replay's and restore's alone")
		}
	}
	if o.action != actionRestore && (o.prepare || o.finish) {
		add("--prepare and --finish are restore's alone")
	}
	if o.replayFrom != 0 && !o.afterLoss {
		add("--replay-from is create-bucket --after-loss's alone")
	}
	if o.backup != "" && !o.afterLoss {
		add("--backup is create-bucket --after-loss's alone")
	}
	if o.replicas < 0 || o.replicas > 5 {
		add("--replicas is 1 to 5")
	}
	switch o.action {
	case actionBackup:
		need("out", o.out != "")
	case actionReplay:
		need("key", o.key != "")
		need("from", o.from != 0)
	case actionRestore:
		modes := 0
		for _, set := range []bool{o.prepare, o.finish, o.key != ""} {
			if set {
				modes++
			}
		}
		if modes != 1 {
			add("exactly one of --prepare, --finish and --key is required")
		}
		if (o.key != "") != (o.from != 0) {
			add("--key and --from go together")
		}
	case actionCleanup:
		if o.gone != "" {
			if _, ok := o.daemonDatabase(o.gone); !ok {
				add("--daemon-database " + o.gone + "=<file> is required: cleanup --gone reads that store at the address its Deployment, or its retired-stores entry, names")
			}
		}
	case actionReplace:
		need("listener", o.listener != "")
	case actionEnroll:
		need("daemon", o.daemon != "")
		if o.daemon != "" {
			if _, ok := o.daemonDatabase(o.daemon); !ok {
				add("--daemon-database " + o.daemon + "=<file> is required: enroll writes that store's marker")
			}
		}
	}
	_, malformed := parseDaemonDatabases(o.daemonDatabases)
	problems = append(problems, malformed...)
	return strings.Join(problems, "; ")
}

// report writes err's line and answers the exit code: 0 for none, 2 for an UNKNOWN reading
// (isUnknown), and 1 for every refusal.
func report(stderr io.Writer, action string, err error) int {
	if err == nil {
		return 0
	}
	if isUnknown(err) {
		line := err.Error()
		if !strings.HasPrefix(line, "UNKNOWN") {
			line = "UNKNOWN " + line
		}
		fmt.Fprintf(stderr, "envoy-infra consumer-names %s: %s\n", action, line)
		return 2
	}
	fmt.Fprintf(stderr, "envoy-infra consumer-names %s: %v\n", action, err)
	return 1
}

// isUnknown reports whether err is an UNKNOWN reading: an *Unknown, or holdrecord.ErrUnknown, a
// row or record that does not verify.
func isUnknown(err error) bool {
	var unknown *Unknown
	return errors.As(err, &unknown) || errors.Is(err, holdrecord.ErrUnknown)
}

// Unknown is a reading that is not whole: the command acts on nothing it did not read, and exits
// 2, never passing.
type Unknown struct {
	What string
	Err  error
}

func (u *Unknown) Error() string {
	if u.Err == nil {
		return "UNKNOWN " + u.What
	}
	return "UNKNOWN " + u.What + ": " + u.Err.Error()
}

func (u *Unknown) Unwrap() error { return u.Err }

func unknownf(err error, format string, args ...any) error {
	return &Unknown{What: fmt.Sprintf(format, args...), Err: err}
}

// formatSeq is a stream sequence as the journal's entries write it.
func formatSeq(seq uint64) string { return strconv.FormatUint(seq, 10) }
