package streams_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/streams"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/natsgrant"
)

// census is a hub set for a census: the declared streams made, a listener's durable and its
// convert form, a session consumer the records name, and the barrier stream standing.
type census struct {
	*hub
	barrierAt, recordsAt atomic.Int64
	calls                atomic.Int64
	// listenerFresh is the listener's durable in its fresh form, which the selection records name.
	listenerFresh string
}

// listenerShape is the durable a listener of machineID creates (cmd/listener, durable.go).
func listenerConsumer(name string) *natsgo.ConsumerConfig {
	return &natsgo.ConsumerConfig{
		Durable: name, DeliverSubject: natsgo.NewInbox(), FilterSubjects: bus.StreamSubjects(),
		AckPolicy: natsgo.AckExplicitPolicy,
	}
}

const sessionConsumer = "s-ses_a"

func startCensus(t *testing.T, grant natsgrant.Permissions) *census {
	t.Helper()
	c := &census{hub: startHub(t, 1, grant), listenerFresh: bus.ListenerDurable("hub") + "~" + nuid.Next()}
	c.makeDeclared(t, streamset.Settings{})
	for _, name := range []string{bus.ListenerDurable("hub"), c.listenerFresh} {
		_, err := c.js.AddConsumer(bus.Stream, listenerConsumer(name))
		require.NoError(t, err)
	}
	_, err := c.js.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable: sessionConsumer, FilterSubjects: []string{"notifications.envoy.agent.ses_a"}, AckPolicy: natsgo.AckExplicitPolicy,
	})
	require.NoError(t, err)
	_, err = c.js.AddStream(&natsgo.StreamConfig{Name: natsgrant.HoldBarrierStream, Subjects: []string{"_ENVOY_HOLD_BARRIER.x"}, Storage: natsgo.MemoryStorage})
	require.NoError(t, err)
	return c
}

// input is the census's input on c: held, every family converted, the barrier and the records'
// read each counted in the order they came, the listener machine ids hub and x, and the listener's
// fresh durable recorded under the session family.
func (c *census) input() streams.CensusInput {
	return streams.CensusInput{
		Phase:    phase(natsgrant.OperatorPhaseHeld),
		Families: []string{"session", "dispatch"},
		Cutovers: func(context.Context) (map[string]string, error) {
			return map[string]string{"session": streams.CutoverDone, "dispatch": streams.CutoverDone}, nil
		},
		Barrier: func(_ context.Context, bucket string) error {
			if bucket == streams.SessionConsumerRecordsBucket {
				c.barrierAt.Store(c.calls.Add(1))
			}
			return nil
		},
		Consumers:          streams.DeclaredConsumers(streams.ConsumerSet{ListenerMachineIDs: []string{"hub", "x"}}),
		RecordedFreshNames: map[string][]string{"session": {c.listenerFresh}},
		RecordedSessionConsumers: func(context.Context) ([]streams.ConsumerShape, error) {
			c.recordsAt.Store(c.calls.Add(1))
			return []streams.ConsumerShape{{
				Stream: bus.Stream, Name: sessionConsumer, FilterSubjects: []string{"notifications.envoy.agent.ses_a"},
				AckPolicy: natsgo.AckExplicitPolicy,
			}}, nil
		},
	}
}

func (c *census) consumers(t *testing.T, stream string) []string {
	t.Helper()
	var names []string
	for name := range c.js.ConsumerNames(stream) {
		names = append(names, name)
	}
	return names
}

// The census, under exactly the held operator's grant, fails naming and removes a stream PLANT
// that sources ENVOY_NOTIFICATIONS, a durable filtered to every private issue event that delivers to
// _INBOX.plant, and a consumer named for listener x whose filter is a private project's, and puts a
// declared bucket that republishes back to the build's configuration; a second run is clean. The
// listener's own durable, its convert form <name>~<nuid>, the recorded session consumer and the
// barrier stream survive, and the census read the records only after its barrier on their bucket.
func TestStreamsCensusRemovesWhatIsNotDeclared(t *testing.T) {
	c := startCensus(t, natsgrant.Operator(natsgrant.OperatorPhaseHeld))
	_, err := c.js.AddStream(&natsgo.StreamConfig{Name: "PLANT", Sources: []*natsgo.StreamSource{{Name: bus.Stream}}, Storage: natsgo.MemoryStorage})
	require.NoError(t, err)
	_, err = c.js.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable: "plant", DeliverSubject: "_INBOX.plant", FilterSubject: "notifications.dispatch.issue.*.>", AckPolicy: natsgo.AckNonePolicy,
	})
	require.NoError(t, err)
	planted := listenerConsumer(bus.ListenerDurable("x"))
	planted.FilterSubjects = []string{"notifications.dispatch.issue.PRIV.>"}
	_, err = c.js.AddConsumer(bus.Stream, planted)
	require.NoError(t, err)
	interests := "KV_" + bus.InterestBucket
	info, err := c.js.StreamInfo(interests)
	require.NoError(t, err)
	republishing := info.Config
	republishing.RePublish = &natsgo.RePublish{Source: ">", Destination: "_INBOX.plant.>"}
	_, err = c.js.UpdateStream(&republishing)
	require.NoError(t, err)

	conn := c.connect(t, operatorInbox, nil)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	report, err := streams.Census(ctx, conn, c.input())
	require.NoError(t, err)
	require.Empty(t, report.Unknown)
	require.Equal(t, 1, report.ExitCode(), "%v", report.Lines())
	requireFailureNaming(t, report, "PLANT", "undeclared")
	requireConsumerFailure(t, report, bus.Stream, "plant", "matches no declared consumer")
	requireConsumerFailure(t, report, bus.Stream, bus.ListenerDurable("x"), "filter subjects")
	requireFailureNaming(t, report, interests, "republish")
	require.Less(t, c.barrierAt.Load(), c.recordsAt.Load(), "the census read the records before its barrier")

	_, err = c.js.StreamInfo("PLANT")
	require.ErrorIs(t, err, natsgo.ErrStreamNotFound, "the census left PLANT")
	left := c.consumers(t, bus.Stream)
	require.NotContains(t, left, "plant")
	require.NotContains(t, left, bus.ListenerDurable("x"))
	require.Contains(t, left, bus.ListenerDurable("hub"))
	require.Contains(t, left, sessionConsumer)
	converted := 0
	for _, name := range left {
		if strings.HasPrefix(name, bus.ListenerDurable("hub")+"~") {
			converted++
		}
	}
	require.Equal(t, 1, converted, "the convert form of the listener's durable did not survive: %v", left)
	info, err = c.js.StreamInfo(interests)
	require.NoError(t, err)
	require.Nil(t, info.Config.RePublish, "the census left the bucket's republish")
	_, err = c.js.StreamInfo(natsgrant.HoldBarrierStream)
	require.NoError(t, err, "the census removed the hold's barrier")

	report, err = streams.Census(ctx, conn, c.input())
	require.NoError(t, err)
	require.Equal(t, 0, report.ExitCode(), "the second census was not clean: %v", report.Lines())
}

// With nothing planted the census is clean and removes nothing.
func TestStreamsCensusOfNothingPlantedIsClean(t *testing.T) {
	c := startCensus(t, natsgrant.Operator(natsgrant.OperatorPhaseHeld))
	c.writes.Take(t)
	report, err := streams.Census(t.Context(), c.connect(t, operatorInbox, nil), c.input())
	require.NoError(t, err)
	require.Equal(t, 0, report.ExitCode(), "%v", report.Lines())
	require.Empty(t, report.Changes)
}

// A census that cannot list every stream is UNKNOWN, exit 2, and changes nothing: under a held
// grant without STREAM.NAMES, as the held lists were before the census needed it, the names never
// agree with the list. A census outside a hold, or before every family is converted, refuses by
// name.
func TestStreamsCensusThatCannotListIsUnknown(t *testing.T) {
	held := natsgrant.Operator(natsgrant.OperatorPhaseHeld)
	var withoutNames natsgrant.Permissions
	withoutNames.Subscribe = held.Subscribe
	for _, entry := range held.Publish {
		if entry != "$JS.API.STREAM.NAMES" {
			withoutNames.Publish = append(withoutNames.Publish, entry)
		}
	}
	c := startCensus(t, withoutNames)
	_, err := c.js.AddStream(&natsgo.StreamConfig{Name: "PLANT", Subjects: []string{"plant.>"}, Storage: natsgo.MemoryStorage})
	require.NoError(t, err)
	conn := c.connect(t, operatorInbox, nil)
	report, err := streams.Census(t.Context(), conn, c.input())
	require.NoError(t, err)
	require.Equal(t, 2, report.ExitCode(), "%v", report.Lines())
	require.Empty(t, report.Changes)
	_, err = c.js.StreamInfo("PLANT")
	require.NoError(t, err, "an UNKNOWN census removed a stream")

	in := c.input()
	in.Phase = phase(natsgrant.OperatorPhasePrepare)
	_, err = streams.Census(t.Context(), conn, in)
	require.ErrorIs(t, err, streams.ErrWrongPhase)
	in = c.input()
	in.Cutovers = func(context.Context) (map[string]string, error) {
		return map[string]string{"session": streams.CutoverDone}, nil
	}
	_, err = streams.Census(t.Context(), conn, in)
	require.ErrorIs(t, err, streams.ErrNotConverted)
	require.ErrorContains(t, err, "dispatch (no row)")
}

func requireConsumerFailure(t *testing.T, report streams.Report, stream, consumer, what string) {
	t.Helper()
	for _, failure := range report.Failures {
		if failure.Stream == stream && failure.Consumer == consumer && strings.Contains(failure.What, what) {
			return
		}
	}
	t.Fatalf("no failure names %s consumer %s %q: %v", stream, consumer, what, failures(report))
}

// Under a hold no creator runs, so a consumer in a family's fresh form, <base>~<nuid>, that no
// selection record names is the orphan of a crashed create: for each family, a listener's durable,
// Dispatch's delivery durable and a daemon's, the census removes the unrecorded fresh name, naming
// it, and keeps the recorded one.
func TestStreamsCensusRemovesAnUnrecordedFreshName(t *testing.T) {
	c := startCensus(t, natsgrant.Operator(natsgrant.OperatorPhaseHeld))
	daemon := streams.ConsumerShape{
		Stream: bus.Stream, Name: "legion-go-PUB-github", FilterSubjects: []string{"notifications.github.>"}, AckPolicy: natsgo.AckExplicitPolicy,
	}
	create := map[string]func(name string) *natsgo.ConsumerConfig{
		"session": listenerConsumer,
		"dispatch": func(name string) *natsgo.ConsumerConfig {
			config := bus.DispatchDeliveryConsumer()
			config.Durable = name
			return &config
		},
		"daemon/PUB": func(name string) *natsgo.ConsumerConfig {
			return &natsgo.ConsumerConfig{Durable: name, FilterSubjects: daemon.FilterSubjects, AckPolicy: natsgo.AckExplicitPolicy}
		},
	}
	bases := map[string]string{"session": bus.ListenerDurable("hub"), "dispatch": bus.DispatchDeliveryDurable, "daemon/PUB": daemon.Name}
	in := c.input()
	in.Consumers = streams.DeclaredConsumers(streams.ConsumerSet{ListenerMachineIDs: []string{"hub", "x"}, Daemons: []streams.ConsumerShape{daemon}})
	unrecorded := map[string]string{}
	var recorded []string
	for family, base := range bases {
		kept, orphan := base+"~"+nuid.Next(), base+"~"+nuid.Next()
		if family == "session" {
			kept = c.listenerFresh
		} else {
			_, err := c.js.AddConsumer(bus.Stream, create[family](kept))
			require.NoError(t, err)
			in.RecordedFreshNames[family] = append(in.RecordedFreshNames[family], kept)
		}
		_, err := c.js.AddConsumer(bus.Stream, create[family](orphan))
		require.NoError(t, err)
		unrecorded[family] = orphan
		recorded = append(recorded, kept)
	}

	report, err := streams.Census(t.Context(), c.connect(t, operatorInbox, nil), in)
	require.NoError(t, err)
	require.Empty(t, report.Unknown)
	left := c.consumers(t, bus.Stream)
	for family, orphan := range unrecorded {
		requireConsumerFailure(t, report, bus.Stream, orphan, "no selection record names")
		require.NotContains(t, left, orphan, "the census kept %s's unrecorded fresh name", family)
	}
	for _, kept := range recorded {
		require.Contains(t, left, kept, "the census removed a recorded fresh name")
	}
}

// A consumer of a family whose store is gone (CensusInput.GoneNames) is left as it is and named,
// whatever its configuration, in its own name or its fresh form: the records that alone tell an
// orphan from a selected name went with the store, which is no evidence to delete anything. Each is
// printed LEFT with why, and counted as neither a change nor a failure, so a census that finds only
// those is clean.
func TestStreamsCensusLeavesAGoneStoresConsumers(t *testing.T) {
	c := startCensus(t, natsgrant.Operator(natsgrant.OperatorPhaseHeld))
	const gone, why = "legion-go-GONE-github", "the project's store is gone"
	fresh := gone + "~" + nuid.Next()
	for _, name := range []string{gone, fresh} {
		_, err := c.js.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
			Durable: name, FilterSubject: "notifications.dispatch.issue.*.>", AckPolicy: natsgo.AckNonePolicy,
		})
		require.NoError(t, err)
	}
	in := c.input()
	in.GoneNames = map[string]string{gone: why}

	report, err := streams.Census(t.Context(), c.connect(t, operatorInbox, nil), in)
	require.NoError(t, err)
	require.Equal(t, 0, report.ExitCode(), "%v", report.Lines())
	require.Empty(t, report.Changes)
	left := c.consumers(t, bus.Stream)
	for _, name := range []string{gone, fresh} {
		require.Contains(t, left, name, "the census removed a gone store's consumer")
		require.Contains(t, report.Lines(), "LEFT "+bus.Stream+" consumer "+name+": "+why)
	}
}
