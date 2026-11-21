package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/natsversion"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The daemon's durables on fresh names (ConsumerSpec.Names, durable_names.go): the compatibility
// rows of the fixed names, the refusals of the fresh ones, and the readers, the checkpoints and
// the operator's cleanup on the selection record. The race regressions are in
// durable_names_race_test.go and, on the hooked server, durable_names_forced_order_test.go.

// On fixed names, the default and "fixed" alike, OpenConsumers is main's: it creates the three
// fixed names, updates them at the next boot, and writes nothing to intake_consumers, whose
// sentinel is all it reads (TestFixedNamesRefuseADatabaseTheCutoverConverted): with the table gone
// it opens the same.
func TestFixedNamesAreTodaysNamesAndReadNoSelectionRecord(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    consumernames.Mode
		noTable bool
	}{
		{name: "the default"},
		{name: "fixed", mode: consumernames.ModeFixed},
		{name: "fixed with no selection table", mode: consumernames.ModeFixed, noTable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			if tc.noTable {
				if _, err := pool.Exec(context.Background(), `drop table intake_consumers`); err != nil {
					t.Fatalf("drop the selection table: %v", err)
				}
			}
			js, stream := testJetStream(t)
			calls := watchConsumerCalls(t, js.Conn())
			spec := consumerSpec(&lockedBuffer{})
			spec.Names = tc.mode
			fixed := fixedNames(spec.Project)

			openIntake(t, js, pool, spec)
			if created := calls.created(t); !slices.Equal(created, fixed) {
				t.Fatalf("the first boot created %v, want the fixed names %v", created, fixed)
			}
			openIntake(t, js, pool, spec)
			if created := calls.created(t); !slices.Equal(created, fixed) {
				t.Fatalf("after the second boot the creates are %v, want the first boot's alone, %v", created, fixed)
			}
			want := slices.Sorted(slices.Values(fixed))
			if names := consumerNames(t, stream); !slices.Equal(names, want) {
				t.Fatalf("the stream holds %v, want %v", names, want)
			}
			if !tc.noTable {
				if rows := selectionRows(t, pool); rows != 0 {
					t.Fatalf("intake_consumers holds %d rows after two boots on fixed names, want none", rows)
				}
			}
		})
	}
}

// On fresh names each durable's consumer is reached on a NATS connection of its own, named for its
// generation, <logical>/<generation> (GenerationConnection), which a grant held to the daemon's
// projects admits that one consumer on: the boot opens one for each current, a consumer made again
// gets its own and the gone one's is closed, and Close closes them all. The control is the fixed
// names, which open no connection beside the daemon's own.
func TestEachFreshNameIsReachedOnAConnectionOfItsOwn(t *testing.T) {
	connections := func(t *testing.T, project string) []string {
		t.Helper()
		var names []string
		for _, connection := range testnats.Connections(t) {
			if strings.HasPrefix(connection.Name, "legion-go-"+project+"-") {
				names = append(names, connection.Name)
			}
		}
		slices.Sort(names)
		return names
	}
	t.Run("control: fixed names", func(t *testing.T) {
		pool := migratedPool(t)
		js, _ := testJetStream(t)
		spec := consumerSpec(&lockedBuffer{})
		openIntake(t, js, pool, spec)
		if names := connections(t, spec.Project); len(names) != 0 {
			t.Fatalf("fixed names opened the connections %v, want none beside the daemon's own", names)
		}
	})
	t.Run("fresh names", func(t *testing.T) {
		pool := migratedPool(t)
		cutover(t, pool, consumernames.CutoverDone)
		js, stream, _ := freshJetStream(t)
		spec := freshSpec(t, &lockedBuffer{}, js)
		want := func() []string {
			var names []string
			for _, logical := range fixedNames(spec.Project) {
				names = append(names, GenerationConnection(currentName(t, pool, logical)))
			}
			slices.Sort(names)
			return names
		}
		awaitConnections := func(what string, want []string) {
			t.Helper()
			testwait.Eventually(t, what, func() bool { return slices.Equal(connections(t, spec.Project), want) })
		}
		first := openIntake(t, js, pool, spec)
		awaitConnections("one connection for each current, named for its generation", want())
		wake := wakeConsumerName(spec.Project)
		gone := currentName(t, pool, wake)
		if GenerationConnection(gone) != wake+"/"+strings.TrimPrefix(gone, wake+"~") {
			t.Fatalf("GenerationConnection(%s) = %s, want the logical name, '/' and the generation", gone, GenerationConnection(gone))
		}
		first.Close()
		awaitConnections("no connection once the consumers are closed", nil)

		if err := stream.DeleteConsumer(context.Background(), gone); err != nil {
			t.Fatalf("delete %s outside the daemon: %v", gone, err)
		}
		second := openIntake(t, js, pool, spec)
		if again := currentName(t, pool, wake); again == gone {
			t.Fatalf("the boot after %s went selected it again", gone)
		}
		awaitConnections("the gone consumer's connection closed and its successor's open", want())
		second.Close()
		awaitConnections("no connection once the consumers are closed", nil)
	})
}

// On fixed names the daemon reads its database's cutover marker at the boot and refuses to start
// once the operator's convert has written "done", naming consumer_names: the fixed names it would
// create are the ones the cutover converted away, so creating them again would run today's race on
// a converted store. With no marker, before any cutover, and after a rollback, which restores the
// fixed names, it opens them as main does.
func TestFixedNamesRefuseADatabaseTheCutoverConverted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		marker  string
		write   bool
		refused bool
	}{
		{name: "the cutover done", marker: consumernames.CutoverDone, write: true, refused: true},
		{name: "control: no sentinel"},
		{name: "control: a sentinel before the cutover", write: true},
		{name: "control: after a rollback", marker: consumernames.CutoverRolledBack, write: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			if tc.write {
				cutover(t, pool, tc.marker)
			}
			js, stream := testJetStream(t)
			calls := watchConsumerCalls(t, js.Conn())
			spec := consumerSpec(&lockedBuffer{})
			spec.Names = consumernames.ModeFixed

			consumers, err := OpenConsumers(context.Background(), js, pool, spec)
			if !tc.refused {
				if err != nil {
					t.Fatalf("OpenConsumers = %v, want the fixed names opened", err)
				}
				if created := calls.created(t); !slices.Equal(created, fixedNames(spec.Project)) {
					t.Fatalf("the boot created %v, want the fixed names", created)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "consumer_names") || !strings.Contains(err.Error(), "fresh") {
				t.Fatalf("OpenConsumers = %v, %v; want a refusal naming consumer_names and fresh", consumers, err)
			}
			if touched := calls.touched(t); len(touched) != 0 {
				t.Fatalf("the refused boot sent the server %v, want no consumer call", touched)
			}
			if names := consumerNames(t, stream); len(names) != 0 {
				t.Fatalf("the stream holds %v after the refused boot, want no consumer", names)
			}
		})
	}
}

// The boot logs how the daemon names its durable consumers, once, before it opens them: the
// default as fixed, and fresh as fresh.
func TestTheBootLogsItsConsumerNamingMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode consumernames.Mode
		want string
	}{
		{name: "the default", want: "fixed"},
		{name: "fresh", mode: consumernames.ModeFresh, want: "fresh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			cutover(t, pool, consumernames.CutoverDone)
			if tc.mode == "" {
				if _, err := pool.Exec(context.Background(), `delete from intake_consumers`); err != nil {
					t.Fatalf("clear the sentinel for the fixed names: %v", err)
				}
			}
			js, _, _ := freshJetStream(t)
			logs := &lockedBuffer{}
			spec := freshSpec(t, logs, js)
			spec.Names = tc.mode
			openIntake(t, js, pool, spec)
			var lines []map[string]any
			for line := range strings.SplitSeq(logs.String(), "\n") {
				var fields map[string]any
				if json.Unmarshal([]byte(line), &fields) == nil && fields["msg"] == "intake: the durable consumers' naming" {
					lines = append(lines, fields)
				}
			}
			if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["consumer_names"] != tc.want {
				t.Fatalf("the boot's naming lines = %v, want one INFO line with consumer_names %s; logs:\n%s", lines, tc.want, logs)
			}
		})
	}
}

// On fresh names the daemon refuses every consumer call until the operator's convert has written
// the cutover marker "done" into its database: with the table missing, the sentinel missing, the
// cutover not run and after a rollback, the boot stops with the ERROR line, before any consumer is
// created or read, and never falls back to the fixed names.
func TestFreshNamesRefuseUntilTheCutoverIsDone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker string
		write  bool
		drop   bool
		want   error
	}{
		{name: "no selection table", drop: true, want: consumernames.ErrNoStore},
		{name: "no sentinel", want: consumernames.ErrNoSentinel},
		{name: "the cutover not run", write: true, want: consumernames.ErrCutoverNotRun},
		{name: "after a rollback", write: true, marker: consumernames.CutoverRolledBack, want: consumernames.ErrCutoverNotRun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			switch {
			case tc.drop:
				if _, err := pool.Exec(context.Background(), `drop table intake_consumers`); err != nil {
					t.Fatalf("drop the selection table: %v", err)
				}
			case tc.write:
				cutover(t, pool, tc.marker)
			}
			js, stream, _ := freshJetStream(t)
			calls := watchConsumerCalls(t, js.Conn())
			logs := &lockedBuffer{}
			spec := freshSpec(t, logs, js)

			consumers, err := OpenConsumers(context.Background(), js, pool, spec)
			if !errors.Is(err, tc.want) {
				t.Fatalf("OpenConsumers = %v, %v; want the refusal %v", consumers, err, tc.want)
			}
			if touched := calls.touched(t); len(touched) != 0 {
				t.Fatalf("the refused boot sent the server %v, want no consumer call", touched)
			}
			if names := consumerNames(t, stream); len(names) != 0 {
				t.Fatalf("the stream holds %v after the refused boot, want no consumer", names)
			}
			line := oneErrorLine(t, logs, dispatchConsumerName(spec.Project), consumernames.OperationSelect)
			if line["physical"] != "" {
				t.Fatalf("the ERROR line names the physical name %v, want none: no record was read", line["physical"])
			}
			if !strings.Contains(fmt.Sprint(line["error"]), tc.want.Error()) {
				t.Fatalf("the ERROR line's error = %v, want it to hold %q", line["error"], tc.want)
			}
		})
	}
}

// A fresh daemon creates each durable under a fresh name of its logical one, carrying the logical
// name in its metadata, at the native policy a new project has today (DeliverNew), with the record's
// start the stream's end at the reservation and its last floor written; no fixed name is made.
// The next boot keeps each consumer and writes its floor again. The daemon's checkpoint stays keyed
// by the logical name across the two boots, so a fact applied once is applied once (family 8).
func TestAFreshDaemonCreatesEachDurableAtAFreshNameAndKeepsItAcrossBoots(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	cutover(t, pool, consumernames.CutoverDone)
	js, stream, id := freshJetStream(t)
	calls := watchConsumerCalls(t, js.Conn())
	spec := freshSpec(t, &lockedBuffer{}, js)
	before := publishWakes(t, js, 2)

	stop := startConsume(t, js, spec, pool, factMarks())
	logicals := fixedNames(spec.Project)
	currents := map[string]string{}
	for _, logical := range logicals {
		rec := selectionOf(t, pool, logical)
		if !isFresh(logical, rec.Current) || rec.Op != nil {
			t.Fatalf("the record of %s = %+v, want a fresh name of it selected and no operation open", logical, rec)
		}
		info := heldInfo(t, stream, rec.Current)
		if info == nil {
			t.Fatalf("the stream does not hold %s, the current of %s", rec.Current, logical)
		}
		if info.Config.Metadata[consumernames.LogicalMetadataKey] != logical || info.Config.DeliverPolicy != jetstream.DeliverNewPolicy {
			t.Fatalf("%s is %v with metadata %v, want DeliverNew and %s=%s", rec.Current, info.Config.DeliverPolicy, info.Config.Metadata, consumernames.LogicalMetadataKey, logical)
		}
		if rec.Start != before[1]+1 {
			t.Fatalf("the record of %s saves start %d, want the stream's end at the reservation, %d", logical, rec.Start, before[1]+1)
		}
		if lf := rec.LastFloor; lf == nil || lf.Name != rec.Current || lf.StreamID != id || lf.Floor != info.AckFloor.Stream {
			t.Fatalf("the record of %s holds the last floor %+v, want %s's floor %d on stream %s", logical, rec.LastFloor, rec.Current, info.AckFloor.Stream, id)
		}
		currents[logical] = rec.Current
	}
	github := githubConsumerName(spec.Project)
	first := publish(t, js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))
	assertAcknowledgedThrough(t, stream, currents[github], first)
	testwait.Eventually(t, "the first event applied", func() bool { return slices.Equal(writeHandlers(t, pool), []string{"pr-1"}) })
	stop()
	if seq := storedStreamSeq(t, pool, github); seq != int64(first.Sequence) {
		t.Fatalf("the checkpoint of %s is at %d, want the applied message's %d, kept under the logical name", github, seq, first.Sequence)
	}

	stop = startConsume(t, js, spec, pool, factMarks())
	for _, logical := range logicals {
		if rec := selectionOf(t, pool, logical); rec.Current != currents[logical] {
			t.Fatalf("the second boot selected %s for %s, want %s kept", rec.Current, logical, currents[logical])
		}
	}
	if lf := selectionOf(t, pool, github).LastFloor; lf == nil || lf.Floor != first.Sequence {
		t.Fatalf("the second boot wrote the last floor %+v of %s, want %d", lf, github, first.Sequence)
	}
	second := publish(t, js, "notifications.github.sjawhar.legion.pr.2", pullRequestOpenedEnvelope(t, 2))
	assertAcknowledgedThrough(t, stream, currents[github], second)
	testwait.Eventually(t, "the second event applied", func() bool { return len(writeHandlers(t, pool)) >= 2 })
	stop()
	if got := writeHandlers(t, pool); !slices.Equal(got, []string{"pr-1", "pr-2"}) {
		t.Fatalf("applied %q across two boots, want each event once", got)
	}
	if seq := storedStreamSeq(t, pool, github); seq != int64(second.Sequence) {
		t.Fatalf("the checkpoint of %s is at %d after the second boot, want %d", github, seq, second.Sequence)
	}
	var physicalRows int
	if err := pool.QueryRow(context.Background(), `select count(*) from consumer_positions where consumer like '%~%'`).Scan(&physicalRows); err != nil {
		t.Fatalf("count the checkpoints kept under a physical name: %v", err)
	}
	if physicalRows != 0 {
		t.Fatalf("%d checkpoints are kept under a physical name, want every one under its logical name", physicalRows)
	}
	if created := calls.created(t); len(created) != len(logicals) {
		t.Fatalf("two boots created %v, want one consumer for each of %v", created, logicals)
	}
	calls.noNameCreatedTwice(t, spec.Project)
}

// The fixed-name spy (`TestNoConsumerNameIsCreatedTwice`): across a project's first boot, a second
// boot, a repositories change, a consumer deleted outside the daemon and the boot that makes it
// again, and two daemons booting at once on one database and stream, the server is sent no create
// of one name twice, no fixed name at all, and no delete from the daemon.
func TestNoConsumerNameIsCreatedTwice(t *testing.T) {
	t.Run("boots, a repositories change and a missing current", func(t *testing.T) {
		pool := migratedPool(t)
		cutover(t, pool, consumernames.CutoverDone)
		js, stream, _ := freshJetStream(t)
		calls := watchConsumerCalls(t, js.Conn())
		spec := freshSpec(t, &lockedBuffer{}, js)
		github := githubConsumerName(spec.Project)

		openIntake(t, js, pool, spec)
		consumers := openIntake(t, js, pool, spec)
		if err := consumers.SetRepositories(context.Background(), []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion"), ghrepo.MustParse("sjawhar/other")}); err != nil {
			t.Fatalf("SetRepositories: %v", err)
		}
		gone := currentName(t, pool, github)
		if err := stream.DeleteConsumer(context.Background(), gone); err != nil {
			t.Fatalf("delete %s outside the daemon: %v", gone, err)
		}
		openIntake(t, js, pool, spec)
		if again := currentName(t, pool, github); again == gone || !isFresh(github, again) {
			t.Fatalf("the boot after %s went selected %s, want another fresh name", gone, again)
		}
		calls.noNameCreatedTwice(t, spec.Project)
		if deleted := calls.deleted(t); !slices.Equal(deleted, []string{gone}) {
			t.Fatalf("the server was sent deletes of %v, want only the test's own of %s", deleted, gone)
		}
	})
	t.Run("two daemons booting at once", func(t *testing.T) {
		pool := migratedPool(t)
		cutover(t, pool, consumernames.CutoverDone)
		js, stream, _ := freshJetStream(t)
		calls := watchConsumerCalls(t, js.Conn())
		spec := freshSpec(t, &lockedBuffer{}, js)

		var boots sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			boots.Go(func() { _, errs[i] = OpenConsumers(context.Background(), js, pool, spec) })
		}
		boots.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("daemon %d: OpenConsumers = %v", i+1, err)
			}
		}
		var currents []string
		for _, logical := range fixedNames(spec.Project) {
			currents = append(currents, currentName(t, pool, logical))
		}
		slices.Sort(currents)
		if names := consumerNames(t, stream); !slices.Equal(names, currents) {
			t.Fatalf("the stream holds %v, want the three currents %v and nothing else", names, currents)
		}
		calls.noNameCreatedTwice(t, spec.Project)
		if deleted := calls.deleted(t); len(deleted) != 0 {
			t.Fatalf("the daemons sent deletes of %v, want none: the daemon's grant has no consumer delete", deleted)
		}
	})
}

// Every reader of a daemon durable finds it through the record (`TestEveryReaderFindsTheCurrentName`),
// with every fixed name gone from the stream: the boot's reconcile reads DispatchPosition, the state
// route reads Lag, a mapping refresh updates the GitHub consumer's filters, and Run consumes. Lag
// keeps the logical names for display.
func TestEveryReaderFindsTheCurrentName(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream, _ := freshJetStream(t)
	fixedSpec := consumerSpec(&lockedBuffer{})
	openIntake(t, js, pool, fixedSpec)
	for _, name := range fixedNames(fixedSpec.Project) {
		if err := stream.DeleteConsumer(context.Background(), name); err != nil {
			t.Fatalf("delete the fixed name %s, as the cutover's convert does: %v", name, err)
		}
	}
	cutover(t, pool, consumernames.CutoverDone)
	spec := freshSpec(t, &lockedBuffer{}, js)
	consumers := openIntake(t, js, pool, spec)
	github := githubConsumerName(spec.Project)
	ctx := context.Background()

	waiting := publish(t, js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))
	position, err := consumers.DispatchPosition(ctx)
	if err != nil {
		t.Fatalf("DispatchPosition: %v", err)
	}
	if !position.Idle {
		t.Fatalf("DispatchPosition = %+v, want the Dispatch consumer idle: nothing was published on its filter", position)
	}
	lags := consumers.Lag(ctx)
	var names []string
	for _, lag := range lags {
		names = append(names, lag.Name)
		if lag.Unavailable != "" {
			t.Fatalf("Lag of %s is unavailable: %s", lag.Name, lag.Unavailable)
		}
	}
	if want := fixedNames(spec.Project); !slices.Equal(names, want) {
		t.Fatalf("Lag names %v, want the logical names %v", names, want)
	}
	if lags[1].Waiting != 1 {
		t.Fatalf("Lag of %s waits %d, want the one message published", github, lags[1].Waiting)
	}

	repositories := []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion"), ghrepo.MustParse("sjawhar/other")}
	if err := consumers.SetRepositories(ctx, repositories); err != nil {
		t.Fatalf("SetRepositories: %v", err)
	}
	current := currentName(t, pool, github)
	if info := heldInfo(t, stream, current); info == nil || !slices.Equal(info.Config.FilterSubjects, githubFilters(repositories)) {
		t.Fatalf("%s, the GitHub current, filters on %v after the repositories change, want %v", current, info, githubFilters(repositories))
	}
	stop := runConsumers(t, consumers, factMarks())
	defer stop()
	assertAcknowledgedThrough(t, stream, current, waiting)
	testwait.Eventually(t, "the event applied through the current", func() bool { return slices.Equal(writeHandlers(t, pool), []string{"pr-1"}) })
	for _, name := range fixedNames(spec.Project) {
		if heldInfo(t, stream, name) != nil {
			t.Fatalf("the fixed name %s exists again: a reader made it", name)
		}
	}
}

// A missing current is created again at the start its record proves, by start sequence: the last
// floor of that very consumer on this stream + 1; else the saved start, when the last floor was read
// on another stream, or from the predecessor, whose later floor never raises the saved start
// (family 3).
func TestAMissingCurrentIsCreatedAgainAtTheStartItsRecordProves(t *testing.T) {
	for _, tc := range []struct {
		name string
		// record is the wake consumer's record with gone as its current, on the stream id, over
		// messages 1 to 5.
		record func(gone, id string) consumernames.Record
		start  uint64
	}{
		{name: "its own last floor on this stream", start: 4, record: func(gone, id string) consumernames.Record {
			return consumernames.Record{Current: gone, Start: 2, LastFloor: &consumernames.Floor{Floor: 3, Name: gone, StreamID: id}}
		}},
		{name: "a last floor read on another stream", start: 2, record: func(gone, _ string) consumernames.Record {
			return consumernames.Record{Current: gone, Start: 2, LastFloor: &consumernames.Floor{Floor: 3, Name: gone, StreamID: "a stream since recreated"}}
		}},
		{name: "the predecessor's later floor", start: 2, record: func(gone, id string) consumernames.Record {
			return consumernames.Record{Current: gone, Start: 2, LastFloor: &consumernames.Floor{Floor: 4, Name: consumernames.FreshName("predecessor"), StreamID: id}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			cutover(t, pool, consumernames.CutoverDone)
			js, stream, id := freshJetStream(t)
			logs := &lockedBuffer{}
			spec := freshSpec(t, logs, js)
			wake := wakeConsumerName(spec.Project)
			seqs := publishWakes(t, js, 5)
			gone := consumernames.FreshName(wake)
			putRecord(t, pool, wake, tc.record(gone, id))

			openIntake(t, js, pool, spec)
			rec := selectionOf(t, pool, wake)
			if rec.Current == gone || !isFresh(wake, rec.Current) {
				t.Fatalf("the boot selected %s, want a fresh name in place of %s", rec.Current, gone)
			}
			info := heldInfo(t, stream, rec.Current)
			if info.Config.DeliverPolicy != jetstream.DeliverByStartSequencePolicy || info.Config.OptStartSeq != tc.start {
				t.Fatalf("%s starts %v at %d, want by start sequence at %d", rec.Current, info.Config.DeliverPolicy, info.Config.OptStartSeq, tc.start)
			}
			if got, want := fetchSeqs(t, stream, rec.Current), seqs[tc.start-1:]; !slices.Equal(got, want) {
				t.Fatalf("the consumer made again delivered %v, want %v", got, want)
			}
			if lines := errorLines(t, logs); len(lines) != 0 {
				t.Fatalf("a settled missing current logged %v, want no ERROR line", lines)
			}
		})
	}
}

// Step 0 at a boot settles a create another daemon left open (family 2): a candidate the server
// holds is adopted with no create; a candidate that lands while the boot waits out the settle age
// is adopted too; one that never lands is retired once its reservation is two seconds old on this
// boot's clock, and a fresh one is created at the saved start.
func TestABootSettlesTheCreateAnotherDaemonLeftOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		// lands is when the candidate reaches the server: before the boot, during it, or never.
		lands   string
		adopted bool
	}{
		{name: "killed after its create, before its publish", lands: "before", adopted: true},
		{name: "a reservation younger than the settle age, whose candidate lands", lands: "during", adopted: true},
		{name: "a reservation younger than the settle age, whose candidate never lands", lands: "never"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			cutover(t, pool, consumernames.CutoverDone)
			js, stream, _ := freshJetStream(t)
			spec := freshSpec(t, &lockedBuffer{}, js)
			wake := wakeConsumerName(spec.Project)
			seqs := publishWakes(t, js, 3)
			candidate := consumernames.FreshName(wake)
			// The settle age is the reservation's on the boot's clock, so the never-lands row measures
			// from here, not from the boot's start.
			reserved := time.Now()
			putRecord(t, pool, wake, consumernames.Record{Start: seqs[1], Op: &consumernames.Op{
				Kind: consumernames.OpCreate, Owner: "another daemon", Candidate: candidate, ReservedAt: reserved,
			}})
			land := func() {
				if _, err := stream.CreateConsumer(context.Background(), jetstream.ConsumerConfig{
					Durable: candidate, FilterSubject: legionWakeSubject, AckPolicy: jetstream.AckExplicitPolicy, AckWait: spec.AckWait,
					DeliverPolicy: jetstream.DeliverByStartSequencePolicy, OptStartSeq: seqs[1],
					Metadata: map[string]string{consumernames.LogicalMetadataKey: wake},
				}); err != nil {
					t.Errorf("create the candidate %s as the other daemon did: %v", candidate, err)
				}
			}
			if tc.lands == "before" {
				land()
			}
			calls := watchConsumerCalls(t, js.Conn())
			if tc.lands == "during" {
				timer := time.AfterFunc(300*time.Millisecond, land)
				defer timer.Stop()
			}

			openIntake(t, js, pool, spec)
			rec := selectionOf(t, pool, wake)
			if tc.adopted {
				if rec.Current != candidate || slices.Contains(rec.Retired, candidate) {
					t.Fatalf("the record = %+v, want the candidate %s adopted", rec, candidate)
				}
				if slices.ContainsFunc(calls.created(t), func(name string) bool { return consumernames.LogicalOf(name) == wake && name != candidate }) {
					t.Fatalf("the boot created another wake consumer beside the adopted %s: %v", candidate, calls.created(t))
				}
			} else {
				if aged := time.Since(reserved); aged < 2*time.Second {
					t.Fatalf("the boot retired a candidate reserved %s before it returned, want at least the settle age, 2s", aged)
				}
				if rec.Current == candidate || !isFresh(wake, rec.Current) || !slices.Contains(rec.Retired, candidate) {
					t.Fatalf("the record = %+v, want %s retired and a fresh current", rec, candidate)
				}
				if info := heldInfo(t, stream, rec.Current); info.Config.OptStartSeq != seqs[1] {
					t.Fatalf("the fresh current starts at %d, want the saved start %d", info.Config.OptStartSeq, seqs[1])
				}
				if slices.Contains(calls.created(t), candidate) {
					t.Fatalf("the boot created the retired candidate %s", candidate)
				}
			}
			if got := fetchSeqs(t, stream, rec.Current); !slices.Equal(got, seqs[1:]) {
				t.Fatalf("the settled current delivered %v, want %v from the saved start", got, seqs[1:])
			}
			calls.noNameCreatedTwice(t, spec.Project)
		})
	}
}

// A daemon name is retired only after a create nobody answered, and the daemon itself never deletes
// it (its grant has no consumer delete): the operator's status lists it from the daemon's records
// (consumernames.PGStore.List on the daemon's database), and its cleanup deletes it, once it has
// landed, and touches nothing the record selects.
func TestARetiredDaemonNameIsListedByStatusAndDeletedByCleanup(t *testing.T) {
	pool := migratedPool(t)
	cutover(t, pool, consumernames.CutoverDone)
	js, stream, _ := freshJetStream(t)
	spec := freshSpec(t, &lockedBuffer{}, js)
	wake := wakeConsumerName(spec.Project)
	unanswered := consumernames.FreshName(wake)
	putRecord(t, pool, wake, consumernames.Record{Start: 1, Op: &consumernames.Op{
		Kind: consumernames.OpCreate, Owner: "a daemon whose create went unanswered", Candidate: unanswered, ReservedAt: time.Now().Add(-time.Minute),
	}})
	calls := watchConsumerCalls(t, js.Conn())
	openIntake(t, js, pool, spec)
	current := currentName(t, pool, wake)
	// The unanswered create lands after the boot retired it.
	if _, err := stream.CreateConsumer(context.Background(), jetstream.ConsumerConfig{Durable: unanswered, FilterSubject: legionWakeSubject, AckPolicy: jetstream.AckExplicitPolicy}); err != nil {
		t.Fatalf("land the unanswered create: %v", err)
	}
	if deleted := calls.deleted(t); len(deleted) != 0 {
		t.Fatalf("the daemon sent deletes of %v, want none", deleted)
	}

	listed, err := consumernames.NewPGStore(pool).List(context.Background())
	if err != nil {
		t.Fatalf("status: list the daemon's records: %v", err)
	}
	if rec, ok := listed[wake]; !ok || rec.Current != current || !slices.Equal(rec.Retired, []string{unanswered}) {
		t.Fatalf("status lists %s as %+v, want %s current and %s retired", wake, listed[wake], current, unanswered)
	}
	cleanup := consumernames.New(consumernames.Config{
		Store: consumernames.NewPGStore(pool), Server: operatorServer{stream},
		Prefix: func(logical string) string { return logical }, Owner: "the operator's cleanup",
		Floor:    func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID: func(context.Context) (string, error) { return "", nil },
	})
	deleted, err := cleanup.Cleanup(context.Background(), wake)
	if err != nil || deleted != 1 {
		t.Fatalf("cleanup = %d, %v; want the one retired name deleted", deleted, err)
	}
	if heldInfo(t, stream, unanswered) != nil {
		t.Fatalf("%s is still on the stream after cleanup", unanswered)
	}
	if heldInfo(t, stream, current) == nil {
		t.Fatalf("cleanup deleted %s, the current", current)
	}
}

// On fresh names as on fixed ones, the daemon opens nothing on a server that can skip a message on
// a consumer filtering on a wildcard and a literal at the same token (natsversion), and its refusal
// comes before any selection record is read or written: nats:2.10, v2.10.29, the image the hub ran
// before 2.15.1.
func TestFreshNamesRefuseAServerThatSkipsAFilterAsFixedNamesDo(t *testing.T) {
	pool := migratedPool(t)
	cutover(t, pool, consumernames.CutoverDone)
	url := testnats.StartImage(t, testnats.SkippingImage)
	conn := testnats.Connect(t, url)
	refused, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("JetStream on nats:2.10: %v", err)
	}
	if _, err := refused.CreateStream(context.Background(), jetstream.StreamConfig{Name: notificationStream, Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create the stream on nats:2.10: %v", err)
	}
	logs := &lockedBuffer{}
	consumers, err := OpenConsumers(context.Background(), refused, pool, freshSpec(t, logs, refused))
	if err == nil || !strings.Contains(err.Error(), "2.10.29") || !strings.Contains(err.Error(), natsversion.FixedReleases) {
		t.Fatalf("OpenConsumers on nats:2.10 on fresh names = %v, %v; want the refusal naming 2.10.29 and %q", consumers, err, natsversion.FixedReleases)
	}
	if rows := selectionRows(t, pool); rows != 1 {
		t.Fatalf("intake_consumers holds %d rows after the refusal, want the sentinel alone", rows)
	}
	if lines := errorLines(t, logs); len(lines) != 0 {
		t.Fatalf("the version refusal logged %v, want no name step: none ran", lines)
	}
}
