package writerhold

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

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
// ENVOY_NOTIFICATIONS, which it reads on conn, the census's own operator connection: for each
// family daemon/<P> it opens the store at daemonDatabases' <P>=<file> entry and writes the identity
// where the store's differs, and returns one line per store it changed,
// `daemon/<P>: stream_id "old" -> "new"`. A store it cannot read, or a stream with no identity, is
// its error, and it writes nothing. It is consumernames.ReconcileDaemonStreamIDs, whose package
// imports this one, so cmd/envoy-infra passes it in.
type StreamIDWriter func(ctx context.Context, conn *nats.Conn, families, daemonDatabases []string) ([]string, error)

// ErrNoStoreWriter is a census run with no writer of the daemon stores' StreamIDs: a store left
// with a recreated stream's old identity would resume against sequences of a stream that is gone,
// so the census is UNKNOWN rather than done.
var ErrNoStoreWriter = errors.New("this build passes the census no writer of the daemon stores' stream ids")

// FreshNames reads the fresh consumer names the selection records name (CensusInput's
// RecordedFreshNames), on conn, from each store: family to its sorted fresh names
// `<base>~<nuid>`, for the listeners' durables, Dispatch's (from the database at
// dispatchDatabaseFile) and each daemon's (daemonDatabases' <P>=<file> entries). A store it cannot
// read is its error. It is consumernames.RecordedFreshNames, whose package imports this one, so
// cmd/envoy-infra passes it in.
type FreshNames func(ctx context.Context, conn *nats.Conn, families []string, dispatchDatabaseFile string, daemonDatabases []string) (map[string][]string, error)

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
	flags.Var(&o.daemonDatabases, "daemon-database", "a daemon store's database, <project>=<file> (the file holds its URL); repeat")
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
	var result streams.Report
	c := HeldCommand{HoldID: o.holdID, Reason: o.reason, MintersReport: o.MintersReport}
	switch o.word {
	case "update":
		c.Workflow, c.Command = holdrecord.WorkflowStreamsUpdate, CommandStreamsUpdate
		c.Absent = ProtectedStreams()
		c.Change = func(ctx context.Context, nc *nats.Conn, _ string) (map[string]Change, map[string]string, error) {
			r, err := streams.Update(ctx, nc, settings, held)
			result = r
			return streamChanges(r), nil, leftBy(r, streamsRefusal(r, err))
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
			// The fresh names are read before anything is removed: with no reader, or one that
			// cannot read a store, the census is UNKNOWN and removes nothing.
			if e.readers.FreshNames == nil {
				return nil, nil, unknownf(StepPostcheck, ErrNoFreshNamesReader, "read the recorded fresh consumer names")
			}
			fresh, err := e.readers.FreshNames(ctx, nc, o.families, o.dispatchDatabaseFile, o.daemonDatabases)
			if err != nil {
				return nil, nil, unknownf(StepPostcheck, err, "read the recorded fresh consumer names")
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
					if err := holdrecord.Barrier(ctx, js, bucket, holdID); err != nil {
						return err
					}
					barriers[bucket]++
					return nil
				},
				Consumers: streams.DeclaredConsumers(streams.ConsumerSet{ListenerMachineIDs: o.machineIDs, Daemons: shapes}),
				RecordedSessionConsumers: func(ctx context.Context) ([]streams.ConsumerShape, error) {
					if e.readers.Recorded == nil {
						return nil, ErrNoRecordReader
					}
					return e.readers.Recorded(ctx, nc)
				},
				RecordedFreshNames: fresh,
			})
			result = r
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
				return changes, nil, leftBy(r, refused)
			}
			outputs, err := reconcileStreamIDs(ctx, nc, e.readers.Stores, o.families, o.daemonDatabases, stdout)
			return changes, outputs, leftBy(r, err)
		}
	}
	_, err = RunHeld(ctx, records, op, c, e.now, stdout)
	for _, line := range result.Lines() {
		fmt.Fprintln(stdout, line)
	}
	if err != nil {
		return 0, err
	}
	return result.ExitCode(), nil
}

// leftBy is err, a streams run's failure after its intent, as the stop's record of what the run
// left: a *Left naming each change the report made (a stream made or raised, a consumer or stream
// removed) when it made any, else err itself, which RunHeld records as holdrecord.LeftUnchanged.
// The bucket barrier's keys are the hold's own, and are no change.
func leftBy(r streams.Report, err error) error {
	if err == nil || len(r.Changes) == 0 {
		return err
	}
	changed := make([]string, len(r.Changes))
	for i, f := range r.Changes {
		changed[i] = f.String()
	}
	return &Left{What: strings.Join(changed, "; "), Err: err}
}

// reconcileStreamIDs has stores bring every daemon store's StreamID to ENVOY_NOTIFICATIONS'
// current identity, after the census's complete listing and its phase and convert checks. Each
// line stores returns (`daemon/<P>: stream_id "old" -> "new"`) is printed as a CHANGED line and
// named in the census's done entry, as stream_id.daemon/<P>. The stores are the daemons' own
// databases, no stream of the hub, so the postcheck counts none of these writes. With no stores,
// or an error of theirs (a store it cannot read, a stream with no identity), it is UNKNOWN.
func reconcileStreamIDs(ctx context.Context, nc *nats.Conn, stores StreamIDWriter, families, daemonDatabases []string, stdout io.Writer) (map[string]string, error) {
	if stores == nil {
		return nil, unknownf(StepPostcheck, ErrNoStoreWriter, "reconcile the daemon stores' stream ids")
	}
	changed, err := stores(ctx, nc, families, daemonDatabases)
	if err != nil {
		return nil, unknownf(StepPostcheck, err, "reconcile the daemon stores' stream ids")
	}
	outputs := map[string]string{}
	for _, line := range changed {
		fmt.Fprintln(stdout, "CHANGED "+line)
		store, _, _ := strings.Cut(line, ":")
		outputs["stream_id."+store] = line
	}
	return outputs, nil
}

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
// made is recreated, with no write of its own; one it raised or put back is kept.
func streamChanges(r streams.Report) map[string]Change {
	changes := map[string]Change{}
	for _, f := range r.Changes {
		if f.Consumer == "" && f.What == "made" {
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
