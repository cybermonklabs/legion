package writerhold

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sjawhar/envoy/infra/internal/flagerr"
	"github.com/sjawhar/envoy/infra/streams"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/natsgrant"
)

// The writer hold's two stream commands, by their names in the hold's table.
const (
	CommandStreamsUpdate = "streams update"
	CommandStreamsCensus = "streams census"
)

// RecordedSessionConsumers reads the session consumers the hold records (KV_envoy_session_consumers),
// on nc, after the bucket barrier: the census keeps each as a declared consumer. Its reader is
// package consumernames' (envoy-infra consumer-names), which imports this package, so
// cmd/envoy-infra passes it in.
type RecordedSessionConsumers func(ctx context.Context, nc *nats.Conn) ([]streams.ConsumerShape, error)

// ErrNoRecordReader is a census run with no reader of the recorded session consumers: it cannot
// tell a session's consumer from a planted one, so it is UNKNOWN rather than remove either.
var ErrNoRecordReader = errors.New("this build passes the census no reader of the recorded session consumers")

const streamsUsage = `usage: envoy-infra streams update [--hold ID] --reason R [settings] <the hold's flags>
       envoy-infra streams census [--hold ID] --reason R --family F... [--listener-machine-id M]...
                                  [--daemon-consumer <name>=<filter>[,<filter>...]]...
                                  [--daemon-database <project>=<file>]... [--dispatch-database-file F]
                                  [settings] <the hold's flags>

The writer hold's two stream commands. update makes each missing protected stream with the build's
configuration and raises each by a start's own rules, inside a streams-update hold; census, once
every --family's cutover row is done, removes each stream and consumer the declared set does not
name, inside a streams-census, cutover or hub-restore hold. With no --hold each prepares and sets a
hold of its own workflow (streams-update, streams-census); with --hold it runs in that hold. The
hold stays set: envoy-infra hold release --hold ID ends it.

The census reads each --daemon-database store only at the address one source names: its daemon's
Deployment in namespace legion (its envoy-infra.consumer-names/project, store-database and
store-instance annotations), or, for a project retired after the cutover, which has none, its entry
of ConfigMap envoy-operator/envoy-operator-retired-stores (keys <project>.database and
<project>.instance). The file's URL must name that database and its second line that instance. A
store whose database does not exist there is gone: the census prints it GONE, removes none of that
daemon's consumers and prints each LEFT. A store it cannot match or read leaves it UNKNOWN.

settings: --replicas (ENVOY_NATS_REPLICAS), --stream-max-bytes (ENVOY_STREAM_MAX_BYTES),
          --session-max-age (ENVOY_SESSION_MAX_AGE)
The hold's flags, as envoy-infra hold takes them: --broker-url, --token-file, --verify-keys,
--signing-key-secret, --minters-report, --nats URL (repeatable), --probe-token-file,
--kube-namespace, --statefulset and --configmap.
Exit: 0 clean, 1 a failure or a refusal, 2 UNKNOWN or a usage error.
`

// streamsOptions are a streams run's flags.
type streamsOptions struct {
	HubFlags
	word, holdID, reason              string
	families, machineIDs, daemons     stringList
	replicas, maxBytes, sessionMaxAge string
	daemonDatabases                   stringList
	dispatchDatabaseFile              string
}

// StreamIDWriter reconciles each daemon store's recorded StreamID with the current identity of
// ENVOY_NOTIFICATIONS, which it reads on conn, the census's own operator connection. Its stores
// are one for each daemonDatabases <P>=<file> entry, and each family daemon/<P> must be one of
// them. It reads a store only at the address its one source names (deployments: the daemon's
// Deployment, or a retired project's entry of RetiredStoresConfigMap), and writes the identity where
// the store's differs. A store whose database the server answers does not exist is gone, and a
// store it cannot match, reach or read is unreconciled; while any store is unreconciled, it writes
// nothing. What it did is its StreamIDResult, which places each of its stores, and so each daemon
// family of families, in exactly one list (StreamIDResult.Covers). A malformed daemonDatabases
// entry, a listing of the daemon stores' address sources it cannot read (the Deployments, or the
// retired projects' ConfigMap), and a stream with no identity are its error, and it then writes
// nothing; a result answered with an error carries no promise, and no caller reads it. It is
// consumernames.ReconcileDaemonStreamIDs, whose package imports this one, so cmd/envoy-infra passes
// it in.
type StreamIDWriter func(ctx context.Context, conn *nats.Conn, deployments DaemonDeployments, families, daemonDatabases []string) (StreamIDResult, error)

// StreamIDResult is what a StreamIDWriter did to the daemon stores, each list in the order of the
// stores' project keys. A result its writer answered with no error places each store in exactly
// one list (Covers), which the census's reconcile (reconcileStreamIDs) checks before it prints or
// records any store.
type StreamIDResult struct {
	Changed      []StreamIDChange    // stores whose recorded identity it rewrote, by project key
	Gone         []GoneStore         // stores confirmed gone, which no daemon can use
	Reconciled   []ReconciledStore   // stores read and found already consistent, which it left unchanged
	Unreconciled []UnreconciledStore // stores left as they are, and why
}

// Covers is nil when r places each daemon family of families (daemon/<project key>) in exactly one
// of its lists, Changed, Gone, Reconciled or Unreconciled, as a StreamIDWriter's result with no
// error does; a family of no daemon (session, dispatch) names no store and is passed over, and so
// is a store of r that no family names. Otherwise it is an error naming each daemon family in no
// list, and each in more than one, or twice in one, with the lists it is in, so a store the writer
// did not answer for, or answered for twice, is never read as reconciled.
func (r StreamIDResult) Covers(families []string) error {
	lists := map[string][]string{}
	for _, c := range r.Changed {
		lists[c.Project] = append(lists[c.Project], "Changed")
	}
	for _, g := range r.Gone {
		lists[g.Project] = append(lists[g.Project], "Gone")
	}
	for _, s := range r.Reconciled {
		lists[s.Project] = append(lists[s.Project], "Reconciled")
	}
	for _, u := range r.Unreconciled {
		lists[u.Project] = append(lists[u.Project], "Unreconciled")
	}
	var problems []string
	checked := map[string]bool{}
	for _, family := range families {
		project, ok := strings.CutPrefix(family, daemonFamilyPrefix)
		if !ok || checked[project] {
			continue
		}
		checked[project] = true
		switch in := lists[project]; {
		case len(in) == 0:
			problems = append(problems, family+" is in none of Changed, Gone, Reconciled and Unreconciled")
		case len(in) > 1:
			problems = append(problems, fmt.Sprintf("%s is in %d lists: %s", family, len(in), strings.Join(in, ", ")))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("the daemon stores' stream-id result places a daemon family in no list or in more than one: %s", strings.Join(problems, "; "))
}

// StreamIDChange is one store's recorded identity rewritten: daemon/<Project>'s store recorded
// From, and now records To.
type StreamIDChange struct{ Project, From, To string }

// GoneStore is a store confirmed gone: daemon/<Project>'s one source of its address (its Deployment,
// or its entry of RetiredStoresConfigMap) names its database Database on the Cloud SQL instance
// Instance, the operator's --daemon-database file states the instance
// StatedInstance (a store is gone only when the two are equal, and both are printed so a reader
// sees what was compared), and the server answered Answer, that the database does not exist
// (SQLSTATE 3D000).
type GoneStore struct{ Project, Database, Instance, StatedInstance, Answer string }

// ReconciledStore is a store read and found already consistent with the stream, which the writer
// left unchanged: daemon/<Project>'s store records StreamID, the stream's current identity, or ""
// when it records none yet (its table holds no sentinel row, one enrol has not written).
type ReconciledStore struct{ Project, StreamID string }

// UnreconciledStore is a store the writer left as it was: daemon/<Project>'s, for Reason.
type UnreconciledStore struct{ Project, Reason string }

// The daemons' Deployments as the deployment renders them: each Legion daemon runs as a
// Deployment in namespace DaemonNamespace, annotated with its project key
// (DaemonProjectAnnotation) and the store it reads: the store's database name
// (DaemonStoreDatabaseAnnotation) and the connection name of the Cloud SQL instance that holds it
// (DaemonStoreInstanceAnnotation). The census's readers of the daemon stores read them with the
// operator's Kubernetes credential, to check that the store an operator's --daemon-database file
// reaches is that daemon's.
const (
	DaemonNamespace               = "legion"
	DaemonProjectAnnotation       = "envoy-infra.consumer-names/project"
	DaemonStoreDatabaseAnnotation = "envoy-infra.consumer-names/store-database"
	DaemonStoreInstanceAnnotation = "envoy-infra.consumer-names/store-instance"
)

// The retired projects' stores as the deployment renders them: a Legion project retired after the
// fresh-name cutover runs no Deployment, and its cutover row (daemon/<project key>) stays, so the
// deployment renders its store's address in ConfigMap RetiredStoresConfigMap of namespace
// RetiredStoresNamespace, the namespace of the accepted minters' ConfigMap: one pair of data keys
// for each retired project, <project key>.database (RetiredStoreDatabaseSuffix), the store's
// database name, and <project key>.instance (RetiredStoreInstanceSuffix), the connection name of
// the Cloud SQL instance that holds it. The readers of the daemon stores read it with the same
// credential as the Deployments, so a retired project's store is read, and found gone, at that
// address.
const (
	RetiredStoresNamespace     = AcceptedMintersNamespace
	RetiredStoresConfigMap     = "envoy-operator-retired-stores"
	RetiredStoreDatabaseSuffix = ".database"
	RetiredStoreInstanceSuffix = ".instance"
)

// DaemonDeployment is one source of a daemon store's address: a Deployment in the daemons'
// namespace, by its name and annotations, or, Retired, one retired project's entry of ConfigMap
// RetiredStoresConfigMap, named by its project key, with its keys read as the annotations a
// Deployment carries (KubeDaemonDeployments).
type DaemonDeployment struct {
	Name        string
	Annotations map[string]string
	Retired     bool
}

// DaemonDeployments lists the sources of the daemon stores' addresses.
type DaemonDeployments func(ctx context.Context) ([]DaemonDeployment, error)

// KubeDaemonDeployments is the one reader of the daemon stores' addresses, which every reader of a
// daemon store shares (the census's readers, cleanup --gone, status and enroll). Through client it
// lists the Deployments of DaemonNamespace, all of them in one list, and then each retired
// project's entry of ConfigMap RetiredStoresConfigMap, in the order of their
// project keys: Retired, named by its project key and annotated with it (DaemonProjectAnnotation)
// and with each of its two keys present (DaemonStoreDatabaseAnnotation,
// DaemonStoreInstanceAnnotation), so an entry missing a key reads as one missing that annotation,
// which the readers refuse naming the key. A ConfigMap that does not exist holds no retired project.
// A list or read the API server refuses or does not answer, and a key of the ConfigMap of neither
// form, are its error, which fails the readers of the daemon stores, and the census is UNKNOWN.
func KubeDaemonDeployments(client kubernetes.Interface) DaemonDeployments {
	return func(ctx context.Context) ([]DaemonDeployment, error) {
		list, err := client.AppsV1().Deployments(DaemonNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list the Deployments of namespace %s: %w", DaemonNamespace, err)
		}
		deployments := make([]DaemonDeployment, len(list.Items))
		for i, d := range list.Items {
			deployments[i] = DaemonDeployment{Name: d.Name, Annotations: d.Annotations}
		}
		where := "ConfigMap " + RetiredStoresNamespace + "/" + RetiredStoresConfigMap
		retired, err := client.CoreV1().ConfigMaps(RetiredStoresNamespace).Get(ctx, RetiredStoresConfigMap, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return deployments, nil
		case err != nil:
			return nil, fmt.Errorf("read %s: %w", where, err)
		}
		entries := map[string]map[string]string{}
		for key, value := range retired.Data {
			project, annotation := strings.TrimSuffix(key, RetiredStoreDatabaseSuffix), DaemonStoreDatabaseAnnotation
			if project == key {
				project, annotation = strings.TrimSuffix(key, RetiredStoreInstanceSuffix), DaemonStoreInstanceAnnotation
			}
			if project == key || project == "" {
				return nil, fmt.Errorf("%s holds key %q, of neither form <project>%s nor <project>%s, so no retired project's store is read",
					where, key, RetiredStoreDatabaseSuffix, RetiredStoreInstanceSuffix)
			}
			if entries[project] == nil {
				entries[project] = map[string]string{DaemonProjectAnnotation: project}
			}
			entries[project][annotation] = value
		}
		for _, project := range slices.Sorted(maps.Keys(entries)) {
			deployments = append(deployments, DaemonDeployment{Name: project, Annotations: entries[project], Retired: true})
		}
		return deployments, nil
	}
}

// ErrNoStoreWriter is a census run with no writer of the daemon stores' StreamIDs: a store left
// with a recreated stream's old identity would resume against sequences of a stream that is gone,
// so the census is UNKNOWN rather than done.
var ErrNoStoreWriter = errors.New("this build passes the census no writer of the daemon stores' stream ids")

// FreshNames reads the fresh consumer names the selection records name, on conn, from each store
// (FreshNamesRead): for the listeners' durables, Dispatch's (from the database at
// dispatchDatabaseFile) and each daemon's (daemonDatabases' <P>=<file> entries, each read at the
// address its one source names, as StreamIDWriter reads it). A store it cannot read is its error,
// and so is a daemon store it cannot match with its source; a daemon store whose database is
// gone holds no names, and the reader names that daemon's consumers instead (FreshNamesRead.Gone).
// It is consumernames.RecordedFreshNames, whose package imports this one, so cmd/envoy-infra
// passes it in.
type FreshNames func(ctx context.Context, conn *nats.Conn, deployments DaemonDeployments, families []string, dispatchDatabaseFile string, daemonDatabases []string) (FreshNamesRead, error)

// FreshNamesRead is what a FreshNames reader read, as the census takes it.
type FreshNamesRead struct {
	// Names are family to its sorted fresh names `<base>~<nuid>` (CensusInput's
	// RecordedFreshNames, its fresh).
	Names map[string][]string
	// Gone are the consumer names of each daemon whose store is gone, each to why
	// (CensusInput's RecordedFreshNames, its gone): the census keeps each of their consumers and
	// names it.
	Gone map[string]string
}

// ErrNoFreshNamesReader is a census run with no reader of the recorded fresh names: every consumer
// in a fresh form would read as a crashed create's orphan and be removed, so the census is UNKNOWN
// and removes nothing.
var ErrNoFreshNamesReader = errors.New("this build passes the census no reader of the recorded fresh consumer names")

// CensusReaders are the census's readers and writer from package consumernames (t-7), which
// imports this package and so cannot be imported here: the recorded session consumers, the
// recorded fresh names, and the writer of the daemon stores' stream ids. A census with any of the
// three nil is UNKNOWN (exit 2) and removes nothing.
type CensusReaders struct {
	Recorded   RecordedSessionConsumers
	FreshNames FreshNames
	Stores     StreamIDWriter
}

// streamsEnv is what a streams run reaches outside the process.
type streamsEnv struct {
	env
	readers CensusReaders
}

// StreamsCommand runs `envoy-infra streams update|census` with args, the words after "streams",
// and returns its exit code. readers are the census's readers and writer. The census is the final
// command of every workflow that can recreate ENVOY_NOTIFICATIONS (the cutover, hub-restore,
// streams-update's own and streams-census), so its stream-id reconcile covers each.
func StreamsCommand(ctx context.Context, args []string, stdout, stderr io.Writer, readers CensusReaders) int {
	return runStreams(ctx, args, stdout, stderr, streamsEnv{env: realEnv(), readers: readers})
}

// StreamsDeps are what StreamsCommandOn reaches outside the process, in place of the broker,
// Secret Manager and the hub its flags name: a test of another package runs the held words on its
// own stack with them (writerhold/holdtest). The hold's flags are still required and checked, and
// their values are not read.
type StreamsDeps struct {
	// Records opens the operator's client of the broker's hold routes, signing when signing.
	Records func(ctx context.Context, signing bool) (*holdrecord.Client, error)
	// Operator is the operator's reach of the hub.
	Operator func(ctx context.Context) (*Operator, error)
	// Now is the clock the minter report is checked by; time.Now when nil.
	Now func() time.Time
	// CensusReaders are the census's readers and writer, as StreamsCommand takes them.
	CensusReaders
}

// StreamsCommandOn is StreamsCommand with deps in place of the broker, Secret Manager and hub its
// flags name.
func StreamsCommandOn(ctx context.Context, args []string, stdout, stderr io.Writer, deps StreamsDeps) int {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	e := env{
		records: func(ctx context.Context, _ options, signing bool) (*holdrecord.Client, error) {
			return deps.Records(ctx, signing)
		},
		hub: func(ctx context.Context, _ options) (*Operator, error) { return deps.Operator(ctx) },
		now: now,
	}
	return runStreams(ctx, args, stdout, stderr, streamsEnv{env: e, readers: deps.CensusReaders})
}

func runStreams(ctx context.Context, args []string, stdout, stderr io.Writer, e streamsEnv) int {
	if len(args) == 0 || (args[0] != "update" && args[0] != "census") {
		fmt.Fprint(stderr, streamsUsage)
		return 2
	}
	o := streamsOptions{word: args[0]}
	flags := flag.NewFlagSet("envoy-infra streams "+o.word, flag.ContinueOnError)
	var parsed bytes.Buffer
	flags.SetOutput(&parsed)
	o.HubFlags.Register(flags)
	flags.StringVar(&o.holdID, "hold", "", "the hold to run in")
	flags.StringVar(&o.reason, "reason", "", "why, as the hold records it")
	flags.Var(&o.families, "family", "a cutover family the declared set names (session, dispatch, daemon/<project key>); repeat")
	flags.Var(&o.machineIDs, "listener-machine-id", "a machine id of the deployment's listeners; repeat")
	flags.Var(&o.daemons, "daemon-consumer", "a Legion daemon's consumer, <name>=<filter>[,<filter>...]; repeat")
	flags.Var(&o.daemonDatabases, "daemon-database", "a daemon store's database, <project>=<file> (the file's first line holds its URL, its second the Cloud SQL instance that URL reaches); repeat")
	flags.StringVar(&o.dispatchDatabaseFile, "dispatch-database-file", "", "the file holding Dispatch's database URL, whose selection records name its fresh consumer names")
	flags.StringVar(&o.replicas, "replicas", os.Getenv(bus.ReplicasEnvVar), "the deployment's replica count")
	flags.StringVar(&o.maxBytes, "stream-max-bytes", os.Getenv(bus.StreamMaxBytesEnvVar), "the notification stream's byte limit")
	flags.StringVar(&o.sessionMaxAge, "session-max-age", os.Getenv(bus.SessionMaxAgeEnvVar), "the session bucket's age")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stderr, parsed.String())
			return 0
		}
		if name := flagerr.Malformed(flags, err); name != "" {
			fmt.Fprintf(stderr, "envoy-infra streams %s: --%s: a malformed value (not echoed)\n%s", o.word, name, streamsUsage)
		} else {
			fmt.Fprintf(stderr, "envoy-infra streams %s: a flag this command does not take, or a malformed one (not echoed)\n%s", o.word, streamsUsage)
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "envoy-infra streams %s: word %d after \"streams\" is no flag and no flag's value (not echoed)\n", o.word, len(args)-flags.NArg()+1)
		return 2
	}
	var problems []string
	for name, value := range map[string]string{"broker-url": o.BrokerURL, "token-file": o.TokenFile, "verify-keys": o.VerifyKeys,
		"signing-key-secret": o.KeySecret, "reason": o.reason} {
		if value == "" {
			problems = append(problems, "--"+name+" is required")
		}
	}
	if len(o.NATS) == 0 {
		problems = append(problems, "--nats is required: the hub's client URLs")
	}
	if o.holdID == "" && (o.MintersReport == "" || o.ProbeTokenFile == "") {
		problems = append(problems, "--minters-report and --probe-token-file are required to set the hold, with no --hold")
	}
	if o.word == "census" && len(o.families) == 0 {
		problems = append(problems, "census needs at least one --family")
	}
	settings, err := readStreamSettings(o.replicas, o.maxBytes, o.sessionMaxAge)
	if err != nil {
		problems = append(problems, err.Error())
	}
	shapes, err := daemonShapes(o.daemons)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		fmt.Fprintf(stderr, "envoy-infra streams %s: %s\n%s", o.word, strings.Join(problems, "; "), streamsUsage)
		return 2
	}
	code, err := heldStreams(ctx, o, settings, shapes, stdout, e)
	if err != nil {
		return report(stderr, "streams "+o.word, err)
	}
	return code
}

// heldStreams runs the word inside its hold (RunHeld), and answers the report's exit code.
func heldStreams(ctx context.Context, o streamsOptions, settings streamset.Settings, shapes []streams.ConsumerShape, stdout io.Writer, e streamsEnv) (int, error) {
	records, err := e.records(ctx, options{HubFlags: o.HubFlags}, true)
	if err != nil {
		return 0, err
	}
	op, err := e.hub(ctx, options{HubFlags: o.HubFlags})
	if err != nil {
		return 0, err
	}
	held := func(ctx context.Context) (natsgrant.OperatorPhase, error) {
		row, err := records.Row(ctx)
		return row.Phase, err
	}
	// result is the word's report, and reported says it holds one: a run that stops before
	// streams.Update or streams.Census answered (an unread reader, a refused admission, a failed set
	// or connect), or one they refused at their phase or convert check, which return a zero report
	// beside the error, prints no report, since the zero report's last line reads CLEAN.
	var result streams.Report
	var reported bool
	madeReport := func(r streams.Report, err error) bool {
		return err == nil || len(r.Changes)+len(r.Failures)+len(r.Unknown) > 0
	}
	c := HeldCommand{HoldID: o.holdID, Reason: o.reason, MintersReport: o.MintersReport}
	switch o.word {
	case "update":
		c.Workflow, c.Command = holdrecord.WorkflowStreamsUpdate, CommandStreamsUpdate
		c.Absent = ProtectedStreams()
		c.Change = func(ctx context.Context, nc *nats.Conn, _ string) (map[string]Change, map[string]string, error) {
			r, err := streams.Update(ctx, nc, settings, held)
			result, reported = r, madeReport(r, err)
			return streamChanges(r), nil, leftBy(r, nil, streamsRefusal(r, err))
		}
	case "census":
		c.Command = CommandStreamsCensus
		if o.holdID == "" {
			c.Workflow = holdrecord.WorkflowStreamsCensus
		}
		c.Change = func(ctx context.Context, nc *nats.Conn, holdID string) (map[string]Change, map[string]string, error) {
			js, err := jetstream.New(nc)
			if err != nil {
				return nil, nil, err
			}
			// With no reader of the fresh names the census is UNKNOWN and removes nothing. The reader
			// runs inside streams.Census (RecordedFreshNames), after its checks and its barrier on the
			// selection bucket, so every record it reads is one the bucket's leader has applied, and
			// before anything is removed; one that cannot read a store makes the census UNKNOWN. The
			// daemons' Deployments and the retired projects' entries, which both readers of the daemon
			// stores check each store's address against, are read with the hold's own Kubernetes client.
			if e.readers.FreshNames == nil {
				return nil, nil, unknownf(StepPostcheck, ErrNoFreshNamesReader, "read the recorded fresh consumer names")
			}
			deployments := KubeDaemonDeployments(op.Hub.Client)
			freshNames := func(ctx context.Context) (map[string][]string, map[string]string, error) {
				fresh, err := e.readers.FreshNames(ctx, nc, deployments, o.families, o.dispatchDatabaseFile, o.daemonDatabases)
				return fresh.Names, fresh.Gone, err
			}
			if censusFreshNamesFirst {
				names, gone, err := freshNames(ctx)
				if err != nil {
					return nil, nil, unknownf(StepPostcheck, err, "read the recorded fresh consumer names")
				}
				freshNames = func(context.Context) (map[string][]string, map[string]string, error) { return names, gone, nil }
			}
			barriers := map[string]uint64{}
			r, err := streams.Census(ctx, nc, streams.CensusInput{
				Settings: settings,
				Phase:    held,
				Families: o.families,
				Cutovers: func(ctx context.Context) (map[string]string, error) {
					rows, err := records.Cutovers(ctx)
					if err != nil {
						return nil, err
					}
					states := make(map[string]string, len(rows))
					for _, row := range rows {
						states[row.Family] = row.State
					}
					return states, nil
				},
				Barrier: func(ctx context.Context, bucket string) error {
					// Counted before the put: a put whose answer is lost may have landed, and the
					// stop names each key the census may have left (leftBy). A put that fails
					// stops the census before its postcheck, so the postcheck counts only puts
					// that were acknowledged.
					barriers[bucket]++
					return holdrecord.Barrier(ctx, js, bucket, holdID)
				},
				Consumers: streams.DeclaredConsumers(streams.ConsumerSet{ListenerMachineIDs: o.machineIDs, Daemons: shapes}),
				RecordedSessionConsumers: func(ctx context.Context) ([]streams.ConsumerShape, error) {
					if e.readers.Recorded == nil {
						return nil, ErrNoRecordReader
					}
					return e.readers.Recorded(ctx, nc)
				},
				RecordedFreshNames: freshNames,
			})
			result, reported = r, madeReport(r, err)
			changes := streamChanges(r)
			write := func(stream string, n uint64) {
				change := changes[stream]
				if change.Kind == "" {
					change.Kind = ChangeKept
				}
				change.Writes += n
				changes[stream] = change
			}
			// Each barrier is one write the census made into its bucket, which the postcheck counts.
			for bucket, n := range barriers {
				write(kvStreamPrefix+bucket, n)
			}
			if refused := streamsRefusal(r, err); refused != nil {
				return changes, nil, leftBy(r, barriers, refused)
			}
			outputs, err := reconcileStreamIDs(ctx, nc, e.readers.Stores, deployments, o.families, o.daemonDatabases, stdout)
			return changes, outputs, leftBy(r, barriers, err)
		}
	}
	_, err = RunHeld(ctx, records, op, c, e.now, stdout)
	if reported {
		for _, line := range result.Lines() {
			fmt.Fprintln(stdout, line)
		}
	}
	if err != nil {
		return 0, err
	}
	return result.ExitCode(), nil
}

// censusBarrierNoChange is the census stop's rule before it counted this hold's bucket barrier
// key: the key, the hold's own, was no change of the census that put it. It is a var so the row's
// control (TestHoldACensusStopNamesTheBarrierKeyItPut) runs the census under that rule.
var censusBarrierNoChange bool

// censusFreshNamesFirst is the census's earlier order: the recorded fresh names read before
// streams.Census's checks and its bucket barrier, so a record the bucket's leader has not yet
// applied could be missed. It is a var so the row's control
// (TestHoldTheCensusReadsTheFreshNamesBehindTheBarrier) runs the census in that order.
var censusFreshNamesFirst bool

// leftBy is err, a streams run's failure after its intent, as the stop's record of what the run
// left: a *Left naming each change the report made (a stream made or raised, a consumer or stream
// removed), this hold's barrier key in each bucket of barriers, which the census put (a stop names
// every change its command's runs made and did not undo, and nothing the census does deletes the
// key), and each change a *Left err carries (a daemon store's stream id rewritten before the
// census stopped, reconcileStreamIDs) when there is any, else err itself, which RunHeld records as
// holdrecord.LeftUnchanged.
func leftBy(r streams.Report, barriers map[string]uint64, err error) error {
	if err == nil {
		return nil
	}
	var changed []string
	for _, f := range r.Changes {
		changed = append(changed, f.String())
	}
	if !censusBarrierNoChange {
		var keys []string
		for bucket := range barriers {
			keys = append(keys, "this hold's barrier key in "+kvStreamPrefix+bucket)
		}
		slices.Sort(keys)
		changed = append(changed, keys...)
	}
	var inner *Left
	if errors.As(err, &inner) {
		changed = append(changed, inner.What)
		err = inner.Err
	}
	if len(changed) == 0 {
		return err
	}
	return &Left{What: strings.Join(changed, "; "), Err: err}
}

// reconcileStreamIDs has stores bring every daemon store's StreamID to ENVOY_NOTIFICATIONS'
// current identity, after the census's complete listing and its phase and convert checks, each
// store read at the address its one source names (deployments). Each store it changed is printed
// as `CHANGED daemon/<P>: stream_id "old" -> "new"` and named in the census's done entry as
// stream_id.daemon/<P>; each store gone as `GONE daemon/<P>: database <db> on instance <instance>
// (the operator's file states <instance>): <answer>`, named as gone.daemon/<P> with the text after
// `daemon/<P>: `; and each store already consistent, which the writer left unchanged, as
// `RECONCILED daemon/<P>: stream_id "id" unchanged`, or `RECONCILED daemon/<P>: no stream_id
// recorded, unchanged` for one that records none yet, named in no output, since the census wrote
// nothing there. The stores are the daemons' own databases, no stream of the hub, so the
// postcheck counts none of these writes. With no stores, an error of theirs (a malformed
// --daemon-database, a listing of the daemon stores' address sources it cannot read, a stream with
// no identity), or any store left unreconciled, which it names with why, it is UNKNOWN and the
// census writes no done entry; a store changed before that is then named as what the run left (a
// *Left), so the stop does not record the run as unchanged.
func reconcileStreamIDs(ctx context.Context, nc *nats.Conn, stores StreamIDWriter, deployments DaemonDeployments, families, daemonDatabases []string, stdout io.Writer) (map[string]string, error) {
	if stores == nil {
		return nil, unknownf(StepPostcheck, ErrNoStoreWriter, "reconcile the daemon stores' stream ids")
	}
	result, err := stores(ctx, nc, deployments, families, daemonDatabases)
	if err != nil {
		return nil, unknownf(StepPostcheck, err, "reconcile the daemon stores' stream ids")
	}
	if err := result.Covers(families); err != nil {
		return nil, unknownf(StepPostcheck, err, "reconcile the daemon stores' stream ids")
	}
	outputs := map[string]string{}
	var changed []string
	for _, c := range result.Changed {
		line := fmt.Sprintf("%s%s: stream_id %q -> %q", daemonFamilyPrefix, c.Project, c.From, c.To)
		fmt.Fprintln(stdout, "CHANGED "+line)
		outputs["stream_id."+daemonFamilyPrefix+c.Project] = line
		changed = append(changed, "stream_id of "+line)
	}
	for _, g := range result.Gone {
		text := fmt.Sprintf("database %s on instance %s (the operator's file states %s): %s", g.Database, g.Instance, g.StatedInstance, g.Answer)
		fmt.Fprintf(stdout, "GONE %s%s: %s\n", daemonFamilyPrefix, g.Project, text)
		outputs["gone."+daemonFamilyPrefix+g.Project] = text
	}
	for _, s := range result.Reconciled {
		recorded := fmt.Sprintf("stream_id %q", s.StreamID)
		if s.StreamID == "" {
			recorded = "no stream_id recorded,"
		}
		fmt.Fprintf(stdout, "RECONCILED %s%s: %s unchanged\n", daemonFamilyPrefix, s.Project, recorded)
	}
	if len(result.Unreconciled) == 0 {
		return outputs, nil
	}
	reasons := make([]string, len(result.Unreconciled))
	for i, u := range result.Unreconciled {
		reasons[i] = daemonFamilyPrefix + u.Project + ": " + u.Reason
	}
	err = unknownf(StepPostcheck, nil, "reconcile the daemon stores' stream ids: left unreconciled: %s", strings.Join(reasons, "; "))
	if len(changed) > 0 {
		return nil, &Left{What: strings.Join(changed, "; "), Err: err}
	}
	return nil, err
}

// daemonFamilyPrefix begins each daemon's cutover family, daemon/<project key>.
const daemonFamilyPrefix = "daemon/"

// streamsRefusal is a streams run's error, or its report's verdict when it is not clean: an UNKNOWN
// report is an *Unknown, a failing one a *Refusal, so the run writes no done entry for either.
func streamsRefusal(r streams.Report, err error) error {
	switch {
	case err != nil:
		return err
	case len(r.Unknown) > 0:
		return unknownf(StepPostcheck, nil, "%s", strings.Join(r.Unknown, "; "))
	case r.ExitCode() != 0:
		var failed []string
		for _, f := range r.Failures {
			failed = append(failed, f.String())
		}
		return refusef(StepPostcheck, "%s", strings.Join(failed, "; "))
	}
	return nil
}

// streamChanges is what a streams run did to each protected stream, for the postcheck: a stream it
// made is recreated, with no write of its own; one it raised or put back is kept. ENVOY_MEASURE,
// which update makes and the hold does not protect (streamset.MeasureStream), is not the
// postcheck's: step 1 reads only the protected streams.
func streamChanges(r streams.Report) map[string]Change {
	changes := map[string]Change{}
	for _, f := range r.Changes {
		if f.Consumer == "" && f.What == "made" && isProtectedStream(f.Stream) {
			changes[f.Stream] = Change{Kind: ChangeRecreated}
		}
	}
	return changes
}

// readStreamSettings reads the deployment's stream settings as every program of it reads them.
func readStreamSettings(replicas, maxBytes, maxAge string) (streamset.Settings, error) {
	var settings streamset.Settings
	var err error
	if settings.Replicas, err = bus.ParseNATSReplicas(replicas); err != nil {
		return settings, err
	}
	if settings.StreamMaxBytes, err = bus.ParseStreamMaxBytes(maxBytes); err != nil {
		return settings, err
	}
	if settings.SessionMaxAge, err = bus.ParseSessionMaxAge(maxAge); err != nil {
		return settings, err
	}
	return settings, nil
}

// daemonShapes reads each --daemon-consumer, <name>=<filter>[,<filter>...], as a Legion daemon's
// pull consumer of the notification stream, acknowledged one message at a time.
func daemonShapes(entries []string) ([]streams.ConsumerShape, error) {
	var shapes []streams.ConsumerShape
	for _, entry := range entries {
		name, filters, ok := strings.Cut(entry, "=")
		if !ok || name == "" || filters == "" {
			return nil, fmt.Errorf("--daemon-consumer %q is not <name>=<filter>[,<filter>...]", entry)
		}
		shapes = append(shapes, streams.ConsumerShape{
			Stream: bus.Stream, Name: name, FilterSubjects: strings.Split(filters, ","), AckPolicy: nats.AckExplicitPolicy,
		})
	}
	return shapes, nil
}
