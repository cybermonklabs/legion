package testnats

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
)

// jetStreamWait bounds each JetStream request of a context Cluster.JetStream opens. It is too short
// for a create of a stream of more than one copy on a cluster: nats-server answers that create
// from the stream's first leader, once its members have elected one (processStreamLeaderChange),
// and on a loaded machine that election can come after jetStreamWait, which ends the request with
// `context deadline exceeded` although the server makes the stream. Such a create goes through
// Cluster.CreateStream.
const jetStreamWait = 10 * time.Second

// JetStream is a JetStream context, its requests bounded by jetStreamWait, on whichever running
// member of the cluster answers (Connect), closed when the test ends. A test creates a stream of
// more than one copy with Cluster.CreateStream, and updates a stream with UpdateStream, not
// through it.
func (c *Cluster) JetStream(t testing.TB) natsgo.JetStreamContext {
	t.Helper()
	conn := Connect(t, c.URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(natsgo.MaxWait(jetStreamWait))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// MemberJetStream is a JetStream context on member i alone. Its requests use the same bound as
// Cluster.JetStream, and its connection closes when t ends.
func (c *Cluster) MemberJetStream(t testing.TB, i int) natsgo.JetStreamContext {
	t.Helper()
	conn := Connect(t, c.Servers[i].URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(natsgo.MaxWait(jetStreamWait))
	if err != nil {
		t.Fatalf("open JetStream on member %s: %v", c.Name(i), err)
	}
	return js
}

// CreateStream creates the stream config names on a connection of its own to the cluster
// (Connect), sending the create once through the context createJetStream opens, and fails the
// test with the server's answer when the create is refused. A successful create of a stream of
// more than one copy is answered by that stream's first leader, once its members have elected one
// (processStreamLeaderChange), which on a loaded machine can come after jetStreamWait; a refusal,
// such as a placement the cluster cannot make, is answered before any election
// (jsClusteredStreamRequest).
func (c *Cluster) CreateStream(t testing.TB, config natsgo.StreamConfig) {
	t.Helper()
	conn := Connect(t, c.URL())
	defer conn.Close()
	if _, err := createJetStream(t, conn).AddStream(&config); err != nil {
		t.Fatalf("create stream %s: %v", config.Name, err)
	}
}

const (
	// updateReadWait bounds each read of the stream UpdateStream makes while it waits for its
	// update to show. A read sent while the stream's group elects a leader can draw no answer, as
	// the update can, so a read cut short is sent again rather than waited out.
	updateReadWait = 500 * time.Millisecond
	// updateReadInterval is the pause between those reads.
	updateReadInterval = 10 * time.Millisecond
	// updateMargin is what UpdateStream leaves of the test binary's deadline when it gives up on
	// an update that never shows, so the test fails by name rather than at go test's own timeout,
	// which names no test.
	updateMargin = 10 * time.Second
)

// LeaderlessStream is a three-copy stream after HoldStreamWithoutLeader stopped its leader and
// one peer, and a copy that answered a read with no leader. Restart starts those members again.
type LeaderlessStream struct {
	cluster *Cluster
	Info    *natsgo.StreamInfo
	Stopped []int
	Kept    int
}

// Restart starts the members HoldStreamWithoutLeader stopped. The caller waits for the particular
// fact it needs after the recovery, rather than this helper deciding an election duration.
func (h *LeaderlessStream) Restart(t testing.TB) {
	t.Helper()
	for _, member := range h.Stopped {
		h.cluster.Servers[member].Start(t)
	}
}

// HoldStreamWithoutLeader stops a stream's leader and one peer, and returns only after the copy
// left running has answered a read naming no leader. A member can take leadership alone while the
// others first stop; that attempt is restored and selected again. readWait is the caller's read
// limit, so a test keeps the limit its behaviour asserts rather than duplicating election setup.
func HoldStreamWithoutLeader(t testing.TB, cluster *Cluster, stream string, readWait time.Duration) *LeaderlessStream {
	t.Helper()
	for attempt := 1; attempt <= 8; attempt++ {
		read := AwaitStreamCopies(t, cluster.JetStream(t), stream, 3)
		members := []int{cluster.Member(read.Cluster.Leader)}
		for _, peer := range read.Cluster.Replicas {
			members = append(members, cluster.Member(peer.Name))
		}
		if slices.Contains(members, -1) || len(slices.Compact(slices.Sorted(slices.Values(members)))) != 3 {
			t.Fatalf("the read of %s names members %v, not three distinct members of the cluster", stream, members)
		}
		stopped, kept := members[:2], members[2]
		for _, member := range stopped {
			cluster.Servers[member].Stop(t)
		}
		js := cluster.MemberJetStream(t, kept)
		var lost error
		var answer *natsgo.StreamInfo
		for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
			answer, lost = js.StreamInfo(stream, natsgo.MaxWait(readWait))
			if lost == nil {
				break
			}
		}
		if answer == nil {
			t.Fatalf("no read of %s on member %d was answered within a minute; the last drew %v", stream, kept, lost)
		}
		if answer.Cluster == nil {
			t.Fatalf("the read of %s on member %d names no cluster: %+v", stream, kept, answer)
		}
		t.Logf("attempt %d: stopped members %v; the copy left running, member %d, names leader %q", attempt, stopped, kept, answer.Cluster.Leader)
		if answer.Cluster.Leader == "" {
			return &LeaderlessStream{cluster: cluster, Info: answer, Stopped: stopped, Kept: kept}
		}
		for _, member := range stopped {
			cluster.Servers[member].Start(t)
		}
		cluster.AwaitReady(t)
	}
	t.Fatalf("in 8 attempts the copy left running led %s after the stops each time, so no update went to a group with no leader", stream)
	return nil
}

// UpdateStream is a test's own update of a stream: a change an operator or another program makes
// for the code under test to find. config is a read of the stream with the test's changes made to
// it. UpdateStream sends config once through js, and returns the stream's info as soon as the
// update is answered or a read of the stream holds every setting of config. It fails the test at
// once when the server refuses the update, with the server's answer.
//
// The answer may never come. nats-server answers an update only from the member that leads the
// stream's group when it applies the update (processClusterUpdateStream, nats-server v2.15.1). So
// an update applied while the group elects a leader, as happens on a loaded machine, gets no
// answer, although every member applies it. UpdateStream therefore reads the stream from the
// moment it sends, and does not wait out js's own wait first. Reading at once also finds the
// change before a keeper of the code under test puts back what the change took away (keepStream,
// whose period these tests set to 250 ms).
//
// It reads until the test binary's deadline, less updateMargin, and has no bound of its own. When
// no read holds config by then, it fails the test naming the stream and each setting the last read
// held otherwise. It never sends the update again. An update replaces the whole configuration
// with no compare, and nats-server has no conditional update (the corrected rule on a stream
// update), so a second send of config could write over a change that the code under test made
// after the first send landed.
//
// An update that takes away a subject the stream held before this test's first update, and leaves
// no subject overlapping it, is refused before it is sent
// (refuseSubjectsTakenWithNothingInPlace). Only the keeper's own restoration row may pass
// ForTheKeeperToPutBack.
//
// The server's own metadata, the keys under "_nats.", is not compared: the server sets those keys
// itself.
func UpdateStream(t testing.TB, js natsgo.JetStreamContext, config natsgo.StreamConfig, options ...UpdateOption) *natsgo.StreamInfo {
	t.Helper()
	until := updateDeadline(t)
	if slices.Contains(options, ForTheKeeperToPutBack) {
		if !allowsKeeperRestoration(t) {
			t.Fatalf("testnats.ForTheKeeperToPutBack is reserved for %s", keeperRestorationRow)
		}
	} else {
		refuseSubjectsTakenWithNothingInPlace(t, until, js, config)
	}
	type answer struct {
		info *natsgo.StreamInfo
		err  error
	}
	answered := make(chan answer, 1)
	sent := config
	go func() {
		info, err := js.UpdateStream(&sent)
		answered <- answer{info, err}
	}()
	var unanswered, lostRead error
	var last *natsgo.StreamInfo
	for {
		select {
		case got := <-answered:
			if got.err == nil {
				return got.info
			}
			var refusal natsgo.JetStreamError
			if errors.As(got.err, &refusal) && refusal.APIError() != nil {
				t.Fatalf("the server refused the update of stream %s: %v", config.Name, got.err)
			}
			unanswered = got.err
		default:
		}
		read, err := js.StreamInfo(config.Name, natsgo.MaxWait(updateReadWait))
		switch {
		case err != nil:
			lostRead = err
		case len(settingsNotHeld(config, read.Config)) == 0:
			return read
		default:
			last = read
		}
		if !until.IsZero() && time.Now().After(until) {
			sendResult := "no answer yet"
			if unanswered != nil {
				sendResult = fmt.Sprintf("no answer: %v", unanswered)
			}
			if last == nil {
				t.Fatalf("the update of stream %s drew %s, and no read of the stream was answered by %s before the test's deadline; the last read drew %v",
					config.Name, sendResult, updateMargin, lostRead)
			}
			t.Fatalf("the update of stream %s drew %s, and by %s before the test's deadline no read held it: %s",
				config.Name, sendResult, updateMargin, strings.Join(settingsNotHeld(config, last.Config), "; "))
		}
		time.Sleep(updateReadInterval)
	}
}

// UpdateOption changes what UpdateStream lets a test's update do.
type UpdateOption int

// ForTheKeeperToPutBack lets a test's update take subjects away with nothing left in their place,
// for a row of a keeper itself, whose keeper is to put them back
// (TestTheKeeperRestoresWhatAnOperatorTookAwayAndKeepsAForeignSubject in internal/bus).
const ForTheKeeperToPutBack UpdateOption = 1

// firstUpdateSubjects holds the subjects every test's first UpdateStream read found, keyed by the
// test and stream. Later updates compare against that baseline, so widening a subject and then
// narrowing it cannot make a keeper restoration look safe.
var firstUpdateSubjects sync.Map

type firstUpdateSubjectKey struct {
	test, stream string
}

// keeperRestorationRow is the one row that may pass ForTheKeeperToPutBack: the keeper row whose
// assertion is that its own keeper restores a bare narrowing.
const keeperRestorationRow = "TestTheKeeperRestoresWhatAnOperatorTookAwayAndKeepsAForeignSubject"

// allowsKeeperRestoration is true only for keeperRestorationRow by its whole name, so a row whose
// name merely begins with it may not pass the option.
func allowsKeeperRestoration(t testing.TB) bool {
	return t.Name() == keeperRestorationRow
}

// subjectsBeforeFirstUpdate returns the stream subjects read before t's first update of stream,
// retaining that read for the test's later updates and dropping it when the test ends.
func subjectsBeforeFirstUpdate(t testing.TB, stream string, subjects []string) []string {
	key := firstUpdateSubjectKey{test: t.Name(), stream: stream}
	saved := slices.Clone(subjects)
	actual, loaded := firstUpdateSubjects.LoadOrStore(key, saved)
	if !loaded {
		t.Cleanup(func() { firstUpdateSubjects.Delete(key) })
	}
	return actual.([]string)
}

// updateDeadline is the test binary's deadline less updateMargin: the one bound on every read
// UpdateStream makes, its narrowing guard's among them, so a failed update fails by name before go
// test's own timeout. It is zero, no bound, when t reports no deadline: a testing.TB with no
// Deadline method, such as a benchmark's, or a test run with -timeout 0. The test's Context is no
// such bound: the testing package cancels it before the test's cleanups run, and a cleanup that
// puts a stream back updates it.
func updateDeadline(t testing.TB) time.Time {
	if test, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		if deadline, bounded := test.Deadline(); bounded {
			return deadline.Add(-updateMargin)
		}
	}
	return time.Time{}
}

// refuseSubjectsTakenWithNothingInPlace fails t when config takes away a subject that the stream
// held before the test's first update and leaves no subject overlapping it
// (subjectsTakenWithNothingInPlace). A lost read says nothing of the stream, so it is tried again
// until a read answers or the bound until, from updateDeadline, passes; an unchecked narrowing is
// never sent.
// Stream-not-found is an answered server refusal that UpdateStream reports itself.
func refuseSubjectsTakenWithNothingInPlace(t testing.TB, until time.Time, js natsgo.JetStreamContext, config natsgo.StreamConfig) {
	t.Helper()
	for {
		held, err := js.StreamInfo(config.Name, natsgo.MaxWait(updateReadWait))
		switch {
		case err == nil:
			before := subjectsBeforeFirstUpdate(t, config.Name, held.Config.Subjects)
			if taken := subjectsTakenWithNothingInPlace(before, config.Subjects); len(taken) > 0 {
				t.Fatalf("the update of stream %s takes away %s and leaves no subject overlapping it, so a keeper puts it back at its next pass: leave testnats.KeptByEveryKeeper(<its prefix>) in its place",
					config.Name, strings.Join(taken, ", "))
			}
			return
		case errors.Is(err, natsgo.ErrStreamNotFound):
			return
		case !until.IsZero() && time.Now().After(until):
			t.Fatalf("the update of stream %s was not sent: by %s before the test's deadline no read of its narrowing guard was answered; the last drew %v",
				config.Name, updateMargin, err)
		}
		time.Sleep(updateReadInterval)
	}
}

// subjectsTakenWithNothingInPlace is each subject of held that no subject of sent overlaps: a
// subject that sent keeps, or one sent keeps in its place, overlaps it.
func subjectsTakenWithNothingInPlace(held, sent []string) []string {
	var taken []string
	for _, subject := range held {
		if !slices.ContainsFunc(sent, func(other string) bool { return natsserver.SubjectsCollide(subject, other) }) {
			taken = append(taken, subject)
		}
	}
	return taken
}

// settingsNotHeld names each setting of sent that held does not hold, as "<json name>: sent <its
// value>, the stream holds <the value held>". An empty value (nil, zero, or an empty list or map)
// holds an empty one, and metadata is compared without the server's own keys (serverMetadata).
func settingsNotHeld(sent, held natsgo.StreamConfig) []string {
	sent.Metadata, held.Metadata = withoutServerMetadata(sent.Metadata), withoutServerMetadata(held.Metadata)
	sentValue, heldValue := reflect.ValueOf(sent), reflect.ValueOf(held)
	var notHeld []string
	for i := range sentValue.NumField() {
		field := sentValue.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		want, got := sentValue.Field(i), heldValue.Field(i)
		if emptySetting(want) && emptySetting(got) || reflect.DeepEqual(want.Interface(), got.Interface()) {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		notHeld = append(notHeld, fmt.Sprintf("%s: sent %s, the stream holds %s", name, settingValue(want), settingValue(got)))
	}
	return notHeld
}

// serverMetadata is the prefix of the metadata keys nats-server sets on a stream itself, its
// version and API level among them.
const serverMetadata = "_nats."

// withoutServerMetadata is metadata without the keys the server sets itself (serverMetadata).
func withoutServerMetadata(metadata map[string]string) map[string]string {
	own := maps.Clone(metadata)
	maps.DeleteFunc(own, func(key, _ string) bool { return strings.HasPrefix(key, serverMetadata) })
	return own
}

// emptySetting reports whether value is a setting left empty: its zero value, or a list or map
// with nothing in it.
func emptySetting(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Slice, reflect.Map:
		return value.Len() == 0
	default:
		return value.IsZero()
	}
}

// settingValue is value as a failure names it: what a pointer points to, not its address.
func settingValue(value reflect.Value) string {
	if value.Kind() == reflect.Pointer && !value.IsNil() {
		return fmt.Sprintf("%+v", value.Elem().Interface())
	}
	return fmt.Sprintf("%v", value.Interface())
}

// KeptByEveryKeeper is the subject a test's narrowing of a stream under prefix leaves in the place
// of the subjects it takes away: nothing a test sends is published on it, and it overlaps what was
// taken away, so no keeper puts that back. A keeper puts back a subject of its own build only when
// no subject the stream holds overlaps it (bus's keptSubjects), at every pass, the first 30 s
// after its client's start. On a loaded machine a test's set-up outlasts that, and a narrowing
// that left nothing in the place of what it took away was taken back before the test sent what
// the narrowing was to refuse: the outbox's own client's keeper had the session route of
// TestABackedOffEventHoldsBackOnlyItsDestination published, not refused.
func KeptByEveryKeeper(prefix string) string { return prefix + "nothing-is-sent-here" }

// Copies is how many copies of a stream or a consumer a cluster holds, its leader's and its
// replicas', as its info's Cluster reports them: 0 for none reported.
func Copies(cluster *natsgo.ClusterInfo) int {
	if cluster == nil {
		return 0
	}
	return 1 + len(cluster.Replicas)
}

// settled reports whether cluster shows n settled copies: a leader and n-1 replicas, every one
// current with the leader and none offline. Every wait below settles on it.
func settled(cluster *natsgo.ClusterInfo, n int) bool {
	return cluster != nil && cluster.Leader != "" && len(cluster.Replicas) == n-1 &&
		!slices.ContainsFunc(cluster.Replicas, func(peer *natsgo.PeerInfo) bool { return !peer.Current || peer.Offline })
}

// AwaitSettledStream waits, within connectTimeout, until stream holds as many settled copies as
// its configuration asks (settled), and returns its info. A test that asserts the copies a write
// left reads them from the info it returns.
func AwaitSettledStream(t testing.TB, js natsgo.JetStreamContext, stream string) *natsgo.StreamInfo {
	t.Helper()
	return awaitInfo(t, connectTimeout, fmt.Sprintf("stream %s did not settle", stream),
		func() (*natsgo.StreamInfo, error) { return js.StreamInfo(stream) },
		func(info *natsgo.StreamInfo) bool { return settled(info.Cluster, info.Config.Replicas) },
		describeStream)
}

// AwaitStreamCopies waits, within connectTimeout, until stream is configured with n copies and
// holds n settled ones (settled), and returns its info.
func AwaitStreamCopies(t testing.TB, js natsgo.JetStreamContext, stream string, n int) *natsgo.StreamInfo {
	t.Helper()
	return AwaitStreamCopiesWithin(t, js, stream, n, connectTimeout)
}

// AwaitStreamCopiesWithin is AwaitStreamCopies bounded by within rather than by connectTimeout,
// for a caller whose own bound for the work it is waiting on is the longer one: a member that was
// killed or lame-ducked mid-write rejoins and catches its streams up, which takes longer than the
// connect this package's other waits bound.
func AwaitStreamCopiesWithin(t testing.TB, js natsgo.JetStreamContext, stream string, n int, within time.Duration) *natsgo.StreamInfo {
	t.Helper()
	return awaitInfo(t, within, fmt.Sprintf("stream %s did not hold %d settled copies", stream, n),
		func() (*natsgo.StreamInfo, error) { return js.StreamInfo(stream) },
		func(info *natsgo.StreamInfo) bool { return info.Config.Replicas == n && settled(info.Cluster, n) },
		describeStream)
}

// AwaitConsumerCopies waits, within connectTimeout, until stream's consumer holds n settled copies
// (settled), as a durable that sets no replica count of its own does once it has followed its
// stream to n, and returns its info.
func AwaitConsumerCopies(t testing.TB, js natsgo.JetStreamContext, stream, consumer string, n int) *natsgo.ConsumerInfo {
	t.Helper()
	return awaitInfo(t, connectTimeout, fmt.Sprintf("consumer %s of %s did not hold %d settled copies", consumer, stream, n),
		func() (*natsgo.ConsumerInfo, error) { return js.ConsumerInfo(stream, consumer) },
		func(info *natsgo.ConsumerInfo) bool { return settled(info.Cluster, n) },
		describeConsumer)
}

// AwaitStreamLeader waits, within connectTimeout, until stream has a leader other than not, the
// server_name of a member the test stopped or "" for any, and returns its info.
func AwaitStreamLeader(t testing.TB, js natsgo.JetStreamContext, stream, not string) *natsgo.StreamInfo {
	t.Helper()
	return awaitInfo(t, connectTimeout, fmt.Sprintf("stream %s had no leader other than %q", stream, not),
		func() (*natsgo.StreamInfo, error) { return js.StreamInfo(stream) },
		func(info *natsgo.StreamInfo) bool {
			return info.Cluster != nil && info.Cluster.Leader != "" && info.Cluster.Leader != not
		},
		describeStream)
}

// describeStream is what a stream wait prints of the info it read last when it ran out: the copies
// the stream's configuration asks for, and every copy the cluster reports (describeCopies).
func describeStream(info *natsgo.StreamInfo) string {
	if info == nil {
		return "no stream info was read"
	}
	return describeCopies(info.Config.Replicas, info.Cluster)
}

// describeConsumer is describeStream for a consumer wait. A durable that sets no replica count of
// its own reports none configured and follows its stream's count.
func describeConsumer(info *natsgo.ConsumerInfo) string {
	if info == nil {
		return "no consumer info was read"
	}
	return describeCopies(info.Config.Replicas, info.Cluster)
}

// describeCopies is the line both describers print: the copies the configuration asks for, and
// the state of every copy the cluster reports (describeCluster). Go gives a type parameter no way
// to reach those two fields, so the two describers stay and share this.
func describeCopies(configured int, cluster *natsgo.ClusterInfo) string {
	return fmt.Sprintf("%d copies configured, %s", configured, describeCluster(cluster))
}

// describeCluster names every peer the cluster reports and the state settled reads: the leader,
// and each replica's name, whether it is current with the leader, whether it is offline, how long
// since it was last active and how far behind it is. A wait that ran out prints this because
// printing the info itself prints the ClusterInfo as an address, so no CI log of a failed wait
// could name the peer that was behind.
func describeCluster(cluster *natsgo.ClusterInfo) string {
	if cluster == nil {
		return "the server reported no cluster for it"
	}
	peers := make([]string, 0, len(cluster.Replicas))
	for _, peer := range cluster.Replicas {
		if peer == nil {
			peers = append(peers, "a nil peer")
			continue
		}
		peers = append(peers, fmt.Sprintf("%s current=%t offline=%t active=%s lag=%d",
			peer.Name, peer.Current, peer.Offline, peer.Active, peer.Lag))
	}
	return fmt.Sprintf("%d copies reported, leader %q, replicas [%s]",
		Copies(cluster), cluster.Leader, strings.Join(peers, "; "))
}

// awaitInfo reads an info every retryInterval until a read succeeds and done holds for it, and
// returns that info; after within it fails the test with failure, what describe says of the info
// it read last, and the last error.
func awaitInfo[T any](t testing.TB, within time.Duration, failure string, read func() (*T, error), done func(*T) bool, describe func(*T) string) *T {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		info, err := read()
		if err == nil && done(info) {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s within %s: %s (%v)", failure, within, describe(info), err)
		}
		time.Sleep(retryInterval)
	}
}

// StreamWrites records every stream create and update a JetStream account's clients ask for: a
// request on the account's $JS.API.STREAM.CREATE or $JS.API.STREAM.UPDATE subject, a bucket's
// stream (KV_<bucket>) among them, reaches its subscriber as it reaches the server.
type StreamWrites struct {
	conn *natsgo.Conn
	sub  *natsgo.Subscription
}

// WatchStreamWrites starts recording the stream writes asked of the account uri connects to, a
// lone server's (Start) or a test's own (URL), until the test ends.
func WatchStreamWrites(t testing.TB, uri string) *StreamWrites {
	t.Helper()
	conn := Connect(t, uri)
	t.Cleanup(conn.Close)
	sub, err := conn.SubscribeSync("$JS.API.STREAM.*.*")
	if err != nil {
		t.Fatalf("watch stream writes: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("watch stream writes: %v", err)
	}
	return &StreamWrites{conn: conn, sub: sub}
}

// Take returns the subjects of the creates and updates asked since the last Take. Each request
// reached the subscription before the server answered it, so the flush, a round trip after the
// writer's answer, leaves every one queued here.
func (w *StreamWrites) Take(t testing.TB) []string {
	t.Helper()
	if err := w.conn.Flush(); err != nil {
		t.Fatalf("flush the stream-write watch: %v", err)
	}
	var subjects []string
	for {
		msg, err := w.sub.NextMsg(50 * time.Millisecond)
		if err != nil {
			return subjects
		}
		if strings.HasPrefix(msg.Subject, "$JS.API.STREAM.CREATE.") || strings.HasPrefix(msg.Subject, "$JS.API.STREAM.UPDATE.") {
			subjects = append(subjects, msg.Subject)
		}
	}
}
