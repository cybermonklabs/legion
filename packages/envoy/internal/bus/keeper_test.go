package bus

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	// testKeepInterval is the keeper's period in these tests (WithKeepInterval). It sets how often a
	// pass comes and decides no verdict: a test waits for what a pass makes (awaitKept) or counts
	// the passes that ended (keeperPasses).
	testKeepInterval = 250 * time.Millisecond
	// keeperByteLimit is the byte limit of the program whose settings the keeper restores.
	keeperByteLimit = 2_000_000
)

// infoRead is one read of a stream's info: the info, or the error that came instead, so a wait
// that gives up prints what its last read answered.
type infoRead struct {
	info *nats.StreamInfo
	err  error
}

func (r infoRead) String() string { return fmt.Sprintf("%+v (%v)", r.info, r.err) }

// readOf reads stream once.
func readOf(js nats.JetStreamContext, stream string) infoRead {
	info, err := js.StreamInfo(stream)
	return infoRead{info: info, err: err}
}

// awaitKept waits until kept reports the stream holds what the keeper restores, and returns its
// info. The wait ends on that fact alone: a keeper pass and the cluster's change of the stream take
// what they take on a loaded machine, and the test binary's deadline is the one bound
// (testwait.Until). Each ask is one STREAM.INFO the stream's leader answers, at the shared wait's
// own pace.
func awaitKept(t *testing.T, js nats.JetStreamContext, stream, what string, kept func(*nats.StreamInfo) bool) *nats.StreamInfo {
	t.Helper()
	return testwait.Until(t, stream+": "+what+" kept", func() (infoRead, bool) {
		read := readOf(js, stream)
		return read, read.err == nil && kept(read.info)
	}).info
}

// answeredRead reads stream until the server answers, and returns that answer, an error included:
// a read the server leaves unanswered within the JetStream wait (unanswered) says nothing of the
// stream, so it is asked again rather than failing the row on the wait's span.
func answeredRead(t *testing.T, js nats.JetStreamContext, stream string) infoRead {
	t.Helper()
	return testwait.Until(t, "an answered read of "+stream, func() (infoRead, bool) {
		read := readOf(js, stream)
		return read, !unanswered(read.err)
	})

}

// readStream is answeredRead's info, failing the test on an answer that is an error.
func readStream(t *testing.T, js nats.JetStreamContext, stream string) *nats.StreamInfo {
	t.Helper()
	read := answeredRead(t, js, stream)
	if read.err != nil {
		t.Fatalf("read %s: %v", stream, read.err)
	}
	return read.info
}

// closeClient is bustest.Close, which this package's tests cannot import (bustest imports the
// bus): it closes client, as a test's cleanup does once the test is over. Close stops the client,
// closes its connection and returns once its stream keeper has ended (awaitKeeper), which it waits
// for on no context, so the call runs through the shared wait: a keeper that never ends fails the
// test naming the close rather than hanging the binary to its timeout.
func closeClient(t testing.TB, client *Client) {
	t.Helper()
	testwait.Returns(t, "the NATS client to close and its stream keeper to end", client.Close)
}

// keptAtSettings reports whether the stream has the setting program's three copies and byte limit
// and an id, id itself when it is not empty.
func keptAtSettings(id string) func(*nats.StreamInfo) bool {
	return func(info *nats.StreamInfo) bool {
		got := info.Config.Metadata[StreamIDMetadataKey]
		return info.Config.Replicas == 3 && info.Config.MaxBytes == keeperByteLimit && got != "" && (id == "" || got == id)
	}
}

// awaitRestoredLog waits until the keeper's restore lines name every one of wants. The keeper logs
// a restore after the server applied it, so a test that reads the stream as kept can get there
// before the line is written.
func awaitRestoredLog(t *testing.T, logs *capturedLogs, wants ...string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the keeper's lines to restore %q", wants), func() (string, bool) {
		logged := restoredLog(t, logs)
		return logged, !slices.ContainsFunc(wants, func(want string) bool { return !strings.Contains(logged, want) })
	})
}

// restoredLog is every field the keeper's restore lines logged so far, joined.
func restoredLog(t *testing.T, logs *capturedLogs) string {
	t.Helper()
	var fields []string
	for _, record := range logs.recordsWithMsg(t, "envoy nats keeper restored the stream") {
		if record["level"] != "WARN" || record["stream"] != Stream {
			t.Fatalf("keeper record = %v, want a WARN naming %s", record, Stream)
		}
		fields = append(fields, fmt.Sprint(record["restored"]))
	}
	return strings.Join(fields, ", ")
}

// Two programs start against an R1 stream whose subject list is old, so both update it: one with
// three copies and a byte limit, one with the defaults, one copy and no limit. JetStream's stream
// update has no compare-and-swap, and the test forces the order that loses the raise: the default
// program's start reads the stream, the setting program's start raises it, and only then does the
// default program's start write what it built from its read, one copy and no limit. Within a few
// keeper periods the setting program's keeper puts the three copies and the byte limit back and
// logs both, and the stream keeps its id, on every run.
func TestTheKeeperEndsTheMixedSettingsRaceAtThreeCopiesTheByteLimitAndTheID(t *testing.T) {
	t.Parallel()
	cluster, js := startClusterJetStream(t)
	logs := captureLogs()
	const id = "deployed-id"
	deployed := *streamCfg
	deployed.Subjects = slices.Clone(streamCarriedSubjects[:len(streamCarriedSubjects)-1])
	deployed.Replicas = 1
	deployed.Metadata = map[string]string{StreamIDMetadataKey: id}
	if _, err := js.AddStream(&deployed); err != nil {
		t.Fatalf("deploy the R1 stream with the old subjects: %v", err)
	}

	defaults := parseConnectOptions([]ConnectOption(nil)).streamConfig()
	// The hook runs on the goroutine the default program's update is bounded on (Bounded), not on
	// the test's, so it ends no test there: it keeps what the setting program's start answered and
	// the stream that start left, and the row fails on them once the default program's start has
	// returned.
	var (
		hooked     bool
		setting    *Client
		settingErr error
		left       infoRead
	)
	defaultStart := &interleavedJetStream{JetStreamContext: js, beforeUpdate: func() {
		hooked = true
		setting, settingErr = ConnectOwningStream(cluster.URLs(), WithReplicas(3), WithStreamMaxBytes(keeperByteLimit), WithKeepInterval(testKeepInterval), WithLogger(logs.logger))
		if settingErr == nil {
			left = readOf(js, Stream)
		}
	}}
	_, err := ensureStreamWithConfig(t.Context(), logs.logger, defaultStart, &defaults, capsAnUnsetStream, atStart)
	if setting != nil {
		t.Cleanup(func() { closeClient(t, setting) })
	}
	if hooked {
		if settingErr != nil {
			t.Fatalf("the setting program's start: %v", settingErr)
		}
		if left.err != nil || !keptAtSettings(id)(left.info) {
			t.Fatalf("the setting program's start left the stream at %v, want three copies, the byte limit and the id", left)
		}
	}
	if err != nil {
		t.Fatalf("the default program's start: %v", err)
	}
	if n := defaultStart.updates.Load(); n != 1 {
		t.Fatalf("the default program's start sent %d stream updates, want its one stale write", n)
	}

	awaitKept(t, js, Stream, "three copies, the byte limit and the id", keptAtSettings(id))
	// Under load nats-server can leave a replica change unanswered (unanswered); the default
	// program's start then reads the stream again, finds three copies, and counts its write as
	// done. That write may never land, or land later: the keeper only has something to restore
	// once it lands. So the restore line is required only when the server answered the stale write,
	// and an unanswered one must still end at the settings a few periods later.
	if stale := *defaultStart.updateErr.Load(); stale != nil {
		if !unanswered(stale) {
			t.Fatalf("the default program's stale write: %v", stale)
		}
		t.Logf("the server left the stale write unanswered (%v): checking the end state only", stale)
		keeperPasses(t, []*Client{setting}, 4)
		awaitKept(t, js, Stream, "three copies, the byte limit and the id", keptAtSettings(id))
		return
	}
	awaitRestoredLog(t, logs, "replicas 1 -> 3", fmt.Sprintf("max_bytes -1 -> %d", keeperByteLimit))
}

// The keeper's write is the stream as it read it with its own part put back, so a concurrent
// start's write that lands between the keeper's read and its write is lost, its subject included.
// The test forces that order: the keeper of a program with a byte limit reads the stream it must
// restore, a start of a release that compiles one more subject adds that subject, and then the
// keeper's write lands without it. The next pass of that release's keeper adds its subject back
// and logs it, and a pass of the first program's keeper, whose binary does not compile the subject,
// leaves it there.
func TestEachReleasesKeeperAddsBackItsOwnSubjectAConcurrentWriteTookAway(t *testing.T) {
	t.Parallel()
	js := streamTestJetStream(t)
	const added = "notifications.newer.>"
	limited := parseConnectOptions([]ConnectOption{WithStreamMaxBytes(keeperByteLimit)}).streamConfig()
	newer := parseConnectOptions([]ConnectOption(nil)).streamConfig()
	newer.Subjects = append(slices.Clone(streamCarriedSubjects), added)
	if _, err := ensureStreamWithConfig(t.Context(), slog.Default(), js, &limited, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("the limited program's start: %v", err)
	}
	lowered := deployedStream(t, js).Config
	lowered.MaxBytes = -1
	testnats.UpdateStream(t, js, lowered)

	// The hook runs on the goroutine the keeper's update is bounded on (Bounded), not on the
	// test's, so it ends no test there: it keeps what the newer release's start answered and the
	// subjects it left, and the row fails on them once the keeper's pass has returned.
	var (
		hooked   bool
		newerErr error
		left     []string
	)
	keeper := &interleavedJetStream{JetStreamContext: js, beforeUpdate: func() {
		hooked = true
		if _, newerErr = ensureStreamWithConfig(t.Context(), slog.Default(), js, &newer, capsAnUnsetStream, atStart); newerErr != nil {
			return
		}
		var info *nats.StreamInfo
		if info, newerErr = js.StreamInfo(Stream); newerErr == nil {
			left = info.Config.Subjects
		}
	}}
	restored, err := keepStream(t.Context(), slog.Default(), keeper, &limited, capsAnUnsetStream)
	if hooked {
		if newerErr != nil {
			t.Fatalf("the newer release's start: %v", newerErr)
		}
		if !slices.Contains(left, added) {
			t.Fatalf("the newer release's start left the subjects %v, want %s among them", left, added)
		}
	}
	if want := []string{fmt.Sprintf("max_bytes -1 -> %d", keeperByteLimit)}; err != nil || !slices.Equal(restored, want) {
		t.Fatalf("the limited program's keeper restored %v (%v), want %v", restored, err, want)
	}
	if subjects := deployedStream(t, js).Config.Subjects; slices.Contains(subjects, added) {
		t.Fatalf("subjects %v still hold %s: the keeper's write did not land after the newer start's", subjects, added)
	}

	restored, err = keepStream(t.Context(), slog.Default(), js, &newer, capsAnUnsetStream)
	if want := []string{"subjects added " + added}; err != nil || !slices.Equal(restored, want) {
		t.Fatalf("the newer release's keeper restored %v (%v), want %v", restored, err, want)
	}
	if restored, err := keepStream(t.Context(), slog.Default(), js, &limited, capsAnUnsetStream); err != nil || len(restored) != 0 {
		t.Fatalf("the limited program's next pass restored %v (%v), want nothing", restored, err)
	}
	info := deployedStream(t, js)
	if want := newer.Subjects; !slices.Equal(info.Config.Subjects, want) || info.Config.MaxBytes != keeperByteLimit {
		t.Fatalf("stream subjects %v, byte limit %d; want %v, %d", info.Config.Subjects, info.Config.MaxBytes, want, keeperByteLimit)
	}
}

// A start moves the role lanes off a stream that still carries them, and a keeper that read the
// stream before the move never puts them back: on a stream whose subjects capture the role lanes
// it sends nothing and reports why, so its stale write cannot land after the start's. The test
// forces that order through the keeper's read, which answers with the stream as the test read it
// before the start's move. Its next pass, on the stream the start moved them off, restores the
// byte limit with no role lane in the list.
func TestTheKeeperNeverPutsBackARoleLaneAStartMovedOffTheStream(t *testing.T) {
	t.Parallel()
	js := streamTestJetStream(t)
	roleLane := contracts.RoleTopicPrefix + ">"
	legacy := *streamCfg
	legacy.Subjects = append(slices.Clone(streamCarriedSubjects), roleLane)
	legacy.Metadata = map[string]string{StreamIDMetadataKey: "legacy-id"}
	if _, err := js.AddStream(&legacy); err != nil {
		t.Fatalf("deploy the stream with the role lanes: %v", err)
	}
	limited := parseConnectOptions([]ConnectOption{WithStreamMaxBytes(keeperByteLimit)}).streamConfig()
	defaults := parseConnectOptions([]ConnectOption(nil)).streamConfig()

	stale := deployedStream(t, js)
	if _, err := ensureStreamWithConfig(t.Context(), slog.Default(), js, &defaults, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("the start that moves the role lanes off: %v", err)
	}
	keeper := &interleavedJetStream{JetStreamContext: js, firstRead: stale}
	restored, err := keepStream(t.Context(), slog.Default(), keeper, &limited, capsAnUnsetStream)
	if err == nil || !strings.Contains(err.Error(), roleLane) || len(restored) != 0 {
		t.Fatalf("the keeper's pass on the stream it read with the role lanes = %v (%v), want an error naming %s", restored, err, roleLane)
	}
	if n := keeper.updates.Load(); n != 0 {
		t.Fatalf("the keeper sent %d stream updates, want none", n)
	}
	if subjects := deployedStream(t, js).Config.Subjects; streamCapturesRoleLanes(subjects) {
		t.Fatalf("subjects %v capture the role lanes after the start moved them off", subjects)
	}

	restored, err = keepStream(t.Context(), slog.Default(), js, &limited, capsAnUnsetStream)
	if want := []string{fmt.Sprintf("max_bytes -1 -> %d", keeperByteLimit)}; err != nil || !slices.Equal(restored, want) {
		t.Fatalf("the keeper's next pass restored %v (%v), want %v", restored, err, want)
	}
	if subjects := deployedStream(t, js).Config.Subjects; !slices.Equal(subjects, streamCarriedSubjects) {
		t.Fatalf("subjects %v after the keeper's pass, want %v", subjects, streamCarriedSubjects)
	}
}

// The keeper's subject list is the deployed one with only this binary's subjects added that it
// lacks and that overlap none deployed: it keeps every deployed subject, a foreign one, a widened
// one and a narrowed one included.
func TestTheKeepersSubjectsAddOnlyItsOwnThatOverlapNothingDeployed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		deployed, own []string
		want          []string
	}{
		{"a missing subject", []string{"notifications.agent.>"}, []string{"notifications.agent.>", "notifications.slack.>"}, []string{"notifications.agent.>", "notifications.slack.>"}},
		{"a foreign subject", []string{"notifications.agent.>", "notifications.retired.>"}, []string{"notifications.agent.>"}, []string{"notifications.agent.>", "notifications.retired.>"}},
		{"a narrowed own subject", []string{"notifications.github.>"}, []string{"notifications.github.pr.>"}, []string{"notifications.github.>"}},
		{"a widened own subject", []string{"notifications.github.pr.>"}, []string{"notifications.github.>"}, []string{"notifications.github.pr.>"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keptSubjects(tc.deployed, tc.own); !slices.Equal(got, tc.want) {
				t.Fatalf("keptSubjects(%v, %v) = %v, want %v", tc.deployed, tc.own, got, tc.want)
			}
		})
	}
}

// Two releases with different subject lists, one adding a subject and one adding another with a
// subject the first compiles narrowed, never switch the stream back and forth: the second's keeper
// adds its new subject once and leaves the first's wide subject in place, and from then on neither
// keeper sends an update, with both new subjects in the list.
func TestTwoKeepersWithDifferentSubjectListsNeverSwitchTheStream(t *testing.T) {
	t.Parallel()
	js := streamTestJetStream(t)
	alpha := parseConnectOptions([]ConnectOption(nil)).streamConfig()
	alpha.Subjects = append(slices.Clone(streamCarriedSubjects), "notifications.alpha.>")
	beta := parseConnectOptions([]ConnectOption(nil)).streamConfig()
	beta.Subjects = append(slices.Clone(streamCarriedSubjects), "notifications.beta.>")
	beta.Subjects[slices.Index(beta.Subjects, "notifications.github.>")] = "notifications.github.pr.>"
	if _, err := ensureStreamWithConfig(t.Context(), slog.Default(), js, &alpha, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("the alpha release's start: %v", err)
	}

	restored, err := keepStream(t.Context(), slog.Default(), js, &beta, capsAnUnsetStream)
	if want := []string{"subjects added notifications.beta.>"}; err != nil || !slices.Equal(restored, want) {
		t.Fatalf("the beta release's keeper restored %v (%v), want %v", restored, err, want)
	}
	counting := &countingJetStream{JetStreamContext: js}
	for range 3 {
		for _, cfg := range []*nats.StreamConfig{&alpha, &beta} {
			if restored, err := keepStream(t.Context(), slog.Default(), counting, cfg, capsAnUnsetStream); err != nil || len(restored) != 0 {
				t.Fatalf("a keeper pass with subjects %v restored %v (%v), want nothing", cfg.Subjects, restored, err)
			}
		}
	}
	if writes := counting.written(); len(writes) != 0 {
		t.Fatalf("the keepers wrote %v, want nothing", writes)
	}
	if want, subjects := append(slices.Clone(alpha.Subjects), "notifications.beta.>"), deployedStream(t, js).Config.Subjects; !slices.Equal(subjects, want) {
		t.Fatalf("subjects %v, want %v", subjects, want)
	}
}

// An operator update that lowers the stream to one copy, removes its byte limit, deletes its id,
// lengthens its age, sets DiscardOld, removes one of this binary's subjects and adds a foreign one
// is undone within a few periods by the keeper of a program whose settings are three copies and a
// byte limit, which logs each field it restored. The id it restores is derived, and stays as it is
// over the passes after. The removed subject comes back at the end of the list, and the foreign
// subject stays: the keeper only adds subjects.
func TestTheKeeperRestoresWhatAnOperatorTookAwayAndKeepsAForeignSubject(t *testing.T) {
	t.Parallel()
	cluster, js := startClusterJetStream(t)
	logs := captureLogs()
	keeper := startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(keeperByteLimit), WithKeepInterval(testKeepInterval), WithLogger(logs.logger), heldKeeperStepGuard(t))
	info := testnats.AwaitSettledStream(t, js, Stream)
	const removed = "notifications.legion.>"

	lowered := info.Config
	lowered.Replicas = 1
	lowered.MaxBytes = -1
	lowered.Metadata = maps.Clone(info.Config.Metadata)
	delete(lowered.Metadata, StreamIDMetadataKey)
	lowered.Subjects = append(slices.DeleteFunc(slices.Clone(info.Config.Subjects), func(subject string) bool { return subject == removed }), "operations.foreign.>")
	lowered.MaxAge = info.Config.MaxAge + time.Hour
	lowered.Discard = nats.DiscardOld
	open := holdKeeperUpdates(t, keeper)
	testnats.UpdateStream(t, js, lowered)
	open()

	restored := awaitKept(t, js, Stream, "three copies, the byte limit and an id", keptAtSettings(""))
	if want := append(slices.Clone(lowered.Subjects), removed); !slices.Equal(restored.Config.Subjects, want) {
		t.Fatalf("subjects after the keeper's restore = %v, want %v", restored.Config.Subjects, want)
	}
	if restored.Config.MaxAge != streamCfg.MaxAge || restored.Config.Discard != nats.DiscardNew {
		t.Fatalf("age %s, discard %s after the keeper's restore; want %s, %s", restored.Config.MaxAge, restored.Config.Discard, streamCfg.MaxAge, nats.DiscardNew)
	}
	id := restored.Config.Metadata[StreamIDMetadataKey]
	if want := deployedStreamID(restored.Created); id != want {
		t.Fatalf("restored id = %q, want the derived %q", id, want)
	}
	heldFor(t, []*Client{keeper}, js, Stream, "the restored configuration", 4, func(info *nats.StreamInfo) bool {
		return reflect.DeepEqual(info.Config, restored.Config)
	})

	logged := restoredLog(t, logs)
	for _, want := range []string{
		"replicas 1 -> 3",
		fmt.Sprintf("max_bytes -1 -> %d", keeperByteLimit),
		fmt.Sprintf("max_age %s -> %s", lowered.MaxAge, streamCfg.MaxAge),
		"discard DiscardOld -> DiscardNew",
		StreamIDMetadataKey + " added as " + id,
		"subjects added " + removed,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the keeper's lines restored %q, want %q among them", logged, want)
		}
	}
}

// An operator's raise of the byte limit, as of a full stream, stands: the keepers of two programs
// whose setting is the old limit leave the raised limit as it is over several periods, logging
// nothing, and a start with that setting writes nothing. Once an operator lowers the limit below
// the setting, the keeper raises it back to the setting and logs it.
func TestTheKeeperKeepsAnOperatorsRaiseOfTheByteLimitAndRaisesALowerOne(t *testing.T) {
	t.Parallel()
	cluster, js := startClusterJetStream(t)
	logs := captureLogs()
	settings := []ConnectOption{WithReplicas(3), WithStreamMaxBytes(keeperByteLimit), WithKeepInterval(testKeepInterval), WithLogger(logs.logger), heldKeeperStepGuard(t)}
	first := startOnCluster(t, cluster, settings...)
	info := testnats.AwaitSettledStream(t, js, Stream)

	raise := info.Config
	raise.MaxBytes = 4 * keeperByteLimit
	testnats.UpdateStream(t, js, raise)
	raised := testnats.AwaitSettledStream(t, js, Stream).Config
	if raised.MaxBytes != raise.MaxBytes {
		t.Fatalf("stream byte limit = %d after the operator's raise, want %d", raised.MaxBytes, raise.MaxBytes)
	}
	unchanged := func(info *nats.StreamInfo) bool { return reflect.DeepEqual(info.Config, raised) }
	heldFor(t, []*Client{first}, js, Stream, "the operator's raise", 6, unchanged)

	counting := &countingJetStream{JetStreamContext: js}
	cfg := parseConnectOptions(settings).streamConfig()
	if _, err := ensureStreamWithConfig(t.Context(), logs.logger, counting, &cfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("start with the old limit: %v", err)
	}
	if writes := counting.written(); len(writes) != 0 {
		t.Fatalf("a start with the old limit wrote %v, want nothing", writes)
	}
	second := startOnCluster(t, cluster, settings...)
	heldFor(t, []*Client{first, second}, js, Stream, "the operator's raise", 6, unchanged)
	if logged := restoredLog(t, logs); logged != "" {
		t.Fatalf("the keepers restored %q over the operator's raise, want nothing", logged)
	}

	lower := raised
	lower.MaxBytes = keeperByteLimit / 2
	open := holdKeeperUpdates(t, first, second)
	testnats.UpdateStream(t, js, lower)
	open()
	awaitKept(t, js, Stream, "the byte limit", func(info *nats.StreamInfo) bool { return info.Config.MaxBytes == keeperByteLimit })
	awaitRestoredLog(t, logs, fmt.Sprintf("max_bytes %d -> %d", lower.MaxBytes, keeperByteLimit))
}

// The keeper raises a bucket its program names (WithKeptBuckets) back to the deployment's copies
// after an operator lowered it, changing nothing else about it, and logs the raise. A bucket it
// does not name stays lowered. A named bucket that does not exist, as one a program opens only
// for a feature it has not configured, is skipped: the keeper creates nothing, updates nothing and
// logs nothing about it.
func TestTheKeeperRaisesTheBucketsItsProgramNamesBackToTheirCopies(t *testing.T) {
	t.Parallel()
	cluster, js := startClusterJetStream(t)
	logs := captureLogs()
	const listed, unlisted, missing = "envoy_keeper_listed", "envoy_keeper_unlisted", "envoy_keeper_missing"

	watcher := testnats.Connect(t, cluster.URL())
	t.Cleanup(watcher.Close)
	var missingRequests atomic.Int32
	for _, subject := range []string{"$JS.API.STREAM.UPDATE.KV_" + missing, "$JS.API.STREAM.CREATE.KV_" + missing} {
		if _, err := watcher.Subscribe(subject, func(*nats.Msg) { missingRequests.Add(1) }); err != nil {
			t.Fatalf("watch %s: %v", subject, err)
		}
	}
	if err := watcher.FlushWithContext(testnats.Context(t)); err != nil {
		t.Fatalf("flush the watch: %v", err)
	}

	client := startOnCluster(t, cluster, WithReplicas(3), WithKeptBuckets(listed, missing), WithKeepInterval(testKeepInterval), WithLogger(logs.logger), heldKeeperStepGuard(t))
	// Each lowering is read back (testnats.UpdateStream): a replica change on a loaded cluster was
	// answered after jetStreamWait, or never, and failed the row as "context deadline exceeded".
	// The buckets are opened on the client's own context, which holdKeeperUpdates does not hold.
	opened := client.JS()
	open := holdKeeperUpdates(t, client)
	before := map[string]nats.StreamConfig{}
	for _, bucket := range []string{listed, unlisted} {
		if _, err := EnsureKeyValue(t.Context(), client.Conn, opened, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3, History: 5, TTL: time.Hour}); err != nil {
			t.Fatalf("open %s at three copies: %v", bucket, err)
		}
		stream := "KV_" + bucket
		info := testnats.AwaitSettledStream(t, js, stream)
		before[bucket] = info.Config
		lowered := info.Config
		lowered.Replicas = 1
		testnats.UpdateStream(t, js, lowered)
		open()
	}

	raised := awaitKept(t, js, "KV_"+listed, "three copies", func(info *nats.StreamInfo) bool { return info.Config.Replicas == 3 })
	if !reflect.DeepEqual(raised.Config, before[listed]) {
		t.Fatalf("raised %s = %+v, want it as it was before it was lowered, %+v", listed, raised.Config, before[listed])
	}
	heldFor(t, []*Client{client}, js, "KV_"+unlisted, "one copy", 4, func(info *nats.StreamInfo) bool { return info.Config.Replicas == 1 })

	if read := answeredRead(t, js, "KV_"+missing); !errors.Is(read.err, nats.ErrStreamNotFound) {
		t.Fatalf("read KV_%s = %v, want it still missing", missing, read)
	}
	if n := missingRequests.Load(); n != 0 {
		t.Fatalf("the keeper sent %d create or update requests for the missing bucket, want none", n)
	}
	if failures := logs.recordsWithMsg(t, "envoy nats keeper could not raise a bucket"); len(failures) != 0 {
		t.Fatalf("the keeper logged failed raises %v, want none", failures)
	}
	raises := map[string]any{}
	for _, record := range logs.recordsWithMsg(t, "envoy nats keeper raised a bucket") {
		raises[fmt.Sprint(record["bucket"])] = record["replicas"]
	}
	if want := map[string]any{listed: "1 -> 3"}; !reflect.DeepEqual(raises, want) {
		t.Fatalf("the keeper's raises logged %v, want %v", raises, want)
	}
}

// The keeper of a program with the default settings, one copy and no byte limit, on a stream that
// has an id, sends no stream update over several passes, whatever the stream's copies and limit:
// it never lowers either, and raises no bucket it names, at one copy as well. The update count is
// read from the API subject itself, after the passes have ended (keeperPasses) and a marker the
// keeper's own connection sends after them has reached the watch, so every update those passes
// sent is in the count.
func TestAKeeperWithTheDefaultSettingsSendsNoUpdate(t *testing.T) {
	t.Parallel()
	const marker = "envoy.keeper.test.marker"
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(keeperByteLimit))
	testnats.AwaitSettledStream(t, js, Stream)

	watcher := testnats.Connect(t, cluster.URL())
	t.Cleanup(watcher.Close)
	updates, err := watcher.SubscribeSync("$JS.API.STREAM.UPDATE.>")
	if err != nil {
		t.Fatalf("watch stream updates: %v", err)
	}
	markers := make(chan *nats.Msg, 1)
	if _, err := watcher.ChanSubscribe(marker, markers); err != nil {
		t.Fatalf("watch the marker: %v", err)
	}
	if err := watcher.FlushWithContext(testnats.Context(t)); err != nil {
		t.Fatalf("flush the watch: %v", err)
	}
	sent := func() int {
		n, _, err := updates.Pending()
		if err != nil {
			t.Fatalf("read the watch's count: %v", err)
		}
		return n
	}

	client := startOnCluster(t, cluster, WithKeptBuckets("envoy_keeper_default"), WithKeepInterval(testKeepInterval))
	if _, err := EnsureKeyValue(t.Context(), client.Conn, client.JS(), &nats.KeyValueConfig{Bucket: "envoy_keeper_default", Storage: nats.FileStorage}); err != nil {
		t.Fatalf("open a bucket at the default copies: %v", err)
	}
	keeperPasses(t, []*Client{client}, 6)
	// The watch's connection reads what the server sends it in order, and the server relays the
	// messages of the keeper's connection in the order it sent them, so the marker reaches the
	// watch after every update those passes sent.
	if err := client.Connection().Publish(marker, nil); err != nil {
		t.Fatalf("send the marker: %v", err)
	}
	testwait.Receive(t, "the marker the keeper's connection sent after its passes", markers)
	if n := sent(); n != 0 {
		t.Fatalf("the default program sent %d stream updates, want none", n)
	}

	// The watch sees an update when one is sent, so the count above is not vacuous.
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	testnats.UpdateStream(t, js, info.Config)
	testwait.Until(t, "the watch to see the update sent to the stream", func() (int, bool) {
		n := sent()
		return n, n > 0
	})
}

// Close and Drain end the keeper before they return, and a pass the stop interrupts logs nothing:
// the test holds the keeper's update of a lowered stream until the stop has closed the connection,
// so the update fails as a pass in flight at a shutdown does, and the keeper logs no failure for
// it. Once the client stops, its keeper has ended, so nothing raises the stream from its one copy.
func TestStoppingTheClientStopsItsKeeper(t *testing.T) {
	t.Parallel()
	for _, stop := range []struct {
		name string
		stop func(*Client)
	}{
		{name: "close", stop: (*Client).Close},
		{name: "drain", stop: func(client *Client) { _ = client.Drain(5 * time.Second) }},
	} {
		t.Run(stop.name, func(t *testing.T) {
			t.Parallel()
			cluster, js := startClusterJetStream(t)
			logs := captureLogs()
			client := startOnCluster(t, cluster, WithReplicas(3), WithKeepInterval(testKeepInterval), WithLogger(logs.logger))
			info := testnats.AwaitSettledStream(t, js, Stream)

			entered := make(chan struct{})
			client.mu.Lock()
			conn := client.Conn
			// The update is held until the stop has closed the connection: a gate the
			// connection's own close opens. Removing the listener at the row's end closes the gate
			// too, so a stop that never closed the connection leaves no keeper held.
			closed := conn.StatusChanged(nats.CLOSED)
			t.Cleanup(func() { conn.RemoveStatusListener(closed) })
			client.js = &interleavedJetStream{JetStreamContext: client.js, beforeUpdate: func() {
				close(entered)
				<-closed
			}}
			client.mu.Unlock()
			// A pass takes the client's JetStream context once, as it starts (keepPass), so a pass
			// in flight at the swap still holds the context with no gate: had it read the stream
			// after the lowering below, it would raise the stream again unheld, every later pass
			// would find nothing to restore, and the row would wait for an update that never comes.
			// This wait sees that pass end, and a whole pass after it, so every pass that reads
			// the lowered stream took the gated context.
			keeperPasses(t, []*Client{client}, 1)
			lowered := info.Config
			lowered.Replicas = 1
			testnats.UpdateStream(t, js, lowered)
			testwait.Receive(t, "the keeper's update of the lowered stream", entered)

			stopped := make(chan struct{})
			go func() {
				stop.stop(client)
				close(stopped)
			}()
			testwait.Receive(t, stop.name+" of the client to return", stopped)
			select {
			case <-client.keeperDone:
			default:
				t.Fatalf("%s returned with the keeper still running", stop.name)
			}
			if failures := logs.recordsWithMsg(t, "envoy nats keeper could not restore the stream"); len(failures) != 0 {
				t.Fatalf("the keeper logged %v for the pass the stop interrupted, want nothing", failures)
			}
			// The keeper has ended (keeperDone above) and its update failed on the closed
			// connection, so no pass is left to raise the stream again: it stays at one copy.
			if info := readStream(t, js, Stream); info.Config.Replicas != 1 {
				t.Fatalf("the stream after the stop = %+v, want one copy", info)
			}
		})
	}
}

// The keeper of an owning client that connects in the background runs no pass before that
// client's first reconcile has succeeded (ensureStreamOnce). The deployed stream holds
// notifications.github.*, which the reconcile replaces with this binary's notifications.github.>
// (reconcileSubjects), and no byte limit, which the keeper restores. A pass that read the stream
// before the reconcile's write and wrote after it would put the replaced subject back, and as the
// keeper only adds subjects, nothing would ever remove it again. The client is made while the
// server is down and its keeper's period is a millisecond, so a keeper started before the reconcile
// races it on most runs; with the gate the replaced subject is still gone after a hundred passes,
// and no keeper pass fails on a stream not reconciled yet.
func TestTheKeeperOfABackgroundClientRunsNoPassBeforeItsFirstReconcile(t *testing.T) {
	t.Parallel()
	const replaced, replacing = "notifications.github.*", "notifications.github.>"
	srv, uri := testnats.Start(t)
	deploy := testnats.Connect(t, uri)
	js, err := deploy.JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	deployed := *streamCfg
	deployed.Subjects = slices.Clone(streamCarriedSubjects)
	deployed.Subjects[slices.Index(deployed.Subjects, replacing)] = replaced
	deployed.Metadata = map[string]string{StreamIDMetadataKey: "deployed-id"}
	if _, err := js.AddStream(&deployed); err != nil {
		t.Fatalf("deploy the stream holding %s: %v", replaced, err)
	}
	deploy.Close()
	srv.Stop(t)

	logs := captureLogs()
	client, err := ConnectOwningStream([]string{uri}, WithConnectInBackground(), WithStreamMaxBytes(keeperByteLimit), WithKeepInterval(time.Millisecond), WithLogger(logs.logger))
	if err != nil {
		t.Fatalf("connect owning the stream while the server is down: %v", err)
	}
	t.Cleanup(func() { closeClient(t, client) })
	srv.Start(t)

	reader := testnats.Connect(t, uri)
	t.Cleanup(reader.Close)
	if js, err = reader.JetStream(); err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	reconciled := func(info *nats.StreamInfo) bool {
		return slices.Contains(info.Config.Subjects, replacing) && !slices.Contains(info.Config.Subjects, replaced)
	}
	testwait.Until(t, fmt.Sprintf("the first connection's reconcile to replace %s with %s", replaced, replacing), func() (infoRead, bool) {
		read := readOf(js, Stream)
		return read, read.err == nil && reconciled(read.info)
	})

	// A hundred keeper passes begun after the reconcile was read: a pass that read the stream
	// before the reconcile has ended by then, and the subject it put back would stay, since the
	// keeper only adds subjects.
	keeperPasses(t, []*Client{client}, 100)
	if info := readStream(t, js, Stream); !reconciled(info) {
		t.Fatalf("the stream after a hundred keeper passes = %+v, want %s kept and %s gone", info, replacing, replaced)
	}
	if failures := logs.recordsWithMsg(t, "envoy nats keeper could not restore the stream"); len(failures) != 0 {
		t.Fatalf("the keeper logged %v, want no pass before the reconcile", failures)
	}
}

// An owning client that connects in the background and stops before its server ever answered
// started no keeper, and Close and Drain return at once rather than wait for one. The client dials
// a listener that answers nothing and counts its dials (dialsCounted), and the row stops it only
// once its dial loop has dialled again after the constructor's first dial, so a keeper the loop
// started would be running by the stop.
func TestStoppingABackgroundClientBeforeItsFirstConnectionStartsNoKeeper(t *testing.T) {
	t.Parallel()
	for _, stop := range []struct {
		name string
		stop func(*Client)
	}{
		{name: "close", stop: (*Client).Close},
		{name: "drain", stop: func(client *Client) { _ = client.Drain(5 * time.Second) }},
	} {
		t.Run(stop.name, func(t *testing.T) {
			uri, dials := dialsCounted(t)
			client, err := ConnectOwningStream([]string{uri}, WithConnectInBackground(), WithKeepInterval(time.Millisecond))
			if err != nil {
				t.Fatalf("connect owning the stream while no server answers: %v", err)
			}
			testwait.Until(t, "the background client's dial loop to dial again after its first dial", func() (string, bool) {
				n := dials.Load()
				return fmt.Sprintf("%d dials", n), n >= 2
			})
			stopped := make(chan struct{})
			go func() {
				stop.stop(client)
				close(stopped)
			}()
			testwait.Receive(t, stop.name+" of a client that never connected to return", stopped)
			client.mu.Lock()
			started := client.keeperDone != nil
			client.mu.Unlock()
			if started {
				t.Fatalf("a client that never reconciled the stream started a keeper")
			}
		})
	}
}

// dialsCounted listens on a port of its own, closes every connection it accepts at once and
// answers none, as a NATS server that never answered would, and returns its NATS URL and how many
// connections it has accepted so far. It stops listening when the test ends.
func dialsCounted(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the client's dials: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var dials atomic.Int64
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			_ = conn.Close()
		}
	}()
	return "nats://" + listener.Addr().String(), &dials
}

// The keeper names the consumer cap it raised, with the deployed value and the new one, so an
// operator who lowered the cap reads what put it back within a period instead of finding it
// raised with nothing saying so. A stream already at the cap has it named for nothing, and so
// has a stream with no cap on a server that caps no such stream itself, which the keeper leaves
// as it is.
func TestRestoredFieldsNamesTheConsumerCapTheKeeperRaised(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name     string
		deployed int
		version  string
		want     string
	}{
		{"a cap an operator lowered", 1000, "2.10.29", fmt.Sprintf("max_consumers 1000 -> %d", streamMaxConsumers)},
		{"a stream with no cap on the hub's 2.15.1, which caps it at 1000", -1, "2.15.1", fmt.Sprintf("max_consumers -1 -> %d", streamMaxConsumers)},
		{"a stream with no cap on 2.10.29, which leaves it unlimited", -1, "2.10.29", ""},
		{"a stream already at the cap", streamMaxConsumers, "2.15.1", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			info := &nats.StreamInfo{Config: nats.StreamConfig{
				Name:         Stream,
				Subjects:     StreamCarriedSubjects(),
				MaxConsumers: row.deployed,
			}}
			kept := keptStream(info, streamCfg, serverDefaultsAnUnsetCap(row.version))
			restored := restoredFields(&info.Config, &kept)
			if row.want == "" {
				if i := slices.IndexFunc(restored, func(field string) bool { return strings.HasPrefix(field, "max_consumers ") }); i >= 0 {
					t.Fatalf("the keeper restored %q, want no cap named", restored[i])
				}
				return
			}
			if !slices.Contains(restored, row.want) {
				t.Fatalf("the keeper restored %q, want %q among them", restored, row.want)
			}
		})
	}
}
