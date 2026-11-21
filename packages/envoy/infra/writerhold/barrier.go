package writerhold

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// barrierAttempts bounds how many times a barrier and its listing, or a bucket barrier and its
// read, are made again when the leader that answered them changed in between.
const barrierAttempts = 3

// barrierWithin bounds each wait of a barrier: the barrier stream listed after its create, a bucket
// barrier's acknowledgement, a stream leader's answer.
const barrierWithin = 30 * time.Second

// barrierSubjectPrefix begins the barrier stream's one subject, _ENVOY_HOLD_BARRIER.<hold id>.
const barrierSubjectPrefix = "_ENVOY_HOLD_BARRIER."

// metaGroupName is the Raft group of JetStream's meta layer, as /raftz names it.
const metaGroupName = "_meta_"

// MetaBarrier is step 5, the barrier in the operator's account before a listing that relies on
// it: in the cutover, after the hold's own restart and before convert lists. The barrier is the
// stream natsgrant.HoldBarrierStream, a meta proposal, so it takes no consumer slot: its create is
// answered once the meta layer applied it, and STREAM.NAMES, answered by the meta leader from the
// assignments it applied, lists it only once that leader applied every entry before it. A
// consumer create that a disconnected client left in the meta log is applied before the barrier,
// so the listing that follows sees it; one only a minority stored never lands.
//
// The listing counts only when one meta leader answered the barrier and the listing: before the
// barrier and after the listing every declared server's /raftz?group=_meta_ must answer the same
// term and leader (a term has at most one leader and never goes back). Any other answer, a changed
// term, no leader or a server that does not answer, discards the listing, and the barrier and the
// listing are made again.
type MetaBarrier struct {
	Hub      KubeHub
	Declared Declared
	HoldID   string
}

// MetaTerm is the meta group's term and leader, as every declared server answered them.
type MetaTerm struct {
	Term   uint64
	Leader string
}

// Run makes the barrier on nc, the operator's connection, and then runs listing, the caller's own
// listing that relies on it, between the two readings of the meta group. It returns listing's error
// as listing returned it. Before the barrier it waits, within barrierWithin, until every declared
// server answers one meta term and leader (a meta group just restarted has none yet). A barrier
// that fails, or a barrier and listing not held in one term and leader, is made again, and after
// barrierAttempts the step is UNKNOWN. A nil listing is the barrier alone.
func (b MetaBarrier) Run(ctx context.Context, nc *nats.Conn, listing func(context.Context) error) error {
	if err := oneToken("hold id", b.HoldID); err != nil {
		return unknownf(StepBarrier, err, "the barrier's subject")
	}
	if nc == nil {
		return unknownf(StepBarrier, nil, "the barrier has no operator connection")
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return unknownf(StepBarrier, err, "the operator's JetStream context")
	}
	var last error
	for range barrierAttempts {
		before, err := b.awaitMeta(ctx)
		if err != nil {
			return err
		}
		if err := b.barrier(ctx, nc, js); err != nil {
			last = err
			continue
		}
		if listing != nil {
			if err := listing(ctx); err != nil {
				return err
			}
		}
		after, err := b.readMeta(ctx)
		switch {
		case err != nil:
			last = err
		case after != before:
			last = fmt.Errorf("the meta group was at term %d, leader %s, before the barrier, and at term %d, leader %s, after the listing", before.Term, before.Leader, after.Term, after.Leader)
		default:
			return nil
		}
	}
	return unknownf(StepBarrier, last, "no barrier and listing were answered by one meta leader in %d attempts", barrierAttempts)
}

// awaitMeta is readMeta read again until every declared server answers one term and leader, and
// UNKNOWN, naming the last reading, when none do within barrierWithin.
func (b MetaBarrier) awaitMeta(ctx context.Context) (MetaTerm, error) {
	within, cancel := context.WithTimeout(ctx, barrierWithin)
	defer cancel()
	for {
		term, err := b.readMeta(within)
		if err == nil {
			return term, nil
		}
		select {
		case <-within.Done():
			return MetaTerm{}, unknownf(StepBarrier, err, "the declared servers answered no one meta term and leader within %s", barrierWithin)
		case <-time.After(restartPoll):
		}
	}
}

// readMeta reads /raftz?group=_meta_ on every declared server, and returns the term and leader they
// all answer. A server that does not answer, has no meta group, or no leader, or two that differ,
// is UNKNOWN.
func (b MetaBarrier) readMeta(ctx context.Context) (MetaTerm, error) {
	var agreed MetaTerm
	first := ""
	for _, server := range b.Declared {
		monitor, stop, err := b.Hub.Monitor(ctx, server.Pod)
		if err != nil {
			return MetaTerm{}, unknownf(StepBarrier, err, "declared server %s's monitoring port", server.Pod)
		}
		var raftz Raftz
		err = monitor.Get(ctx, "/raftz?group="+metaGroupName, &raftz)
		stop()
		if err != nil {
			return MetaTerm{}, unknownf(StepBarrier, err, "declared server %s's /raftz", server.Pod)
		}
		group, ok := metaGroup(raftz)
		switch {
		case !ok:
			return MetaTerm{}, unknownf(StepBarrier, nil, "declared server %s answers no meta group", server.Pod)
		case group.Leader == "":
			return MetaTerm{}, unknownf(StepBarrier, nil, "declared server %s's meta group has no leader in term %d", server.Pod, group.Term)
		}
		term := MetaTerm{Term: group.Term, Leader: group.Leader}
		if first == "" {
			agreed, first = term, server.Pod
			continue
		}
		if term != agreed {
			return MetaTerm{}, unknownf(StepBarrier, nil, "declared server %s answers meta term %d, leader %s, and %s term %d, leader %s",
				first, agreed.Term, agreed.Leader, server.Pod, term.Term, term.Leader)
		}
	}
	if first == "" {
		return MetaTerm{}, unknownf(StepBarrier, nil, "no declared server to read the meta group on")
	}
	return agreed, nil
}

// metaGroup is the meta group of one server's /raftz, in whichever account holds it (the system
// account), and false when none does.
func metaGroup(raftz Raftz) (RaftzGroup, bool) {
	for _, groups := range raftz {
		if group, ok := groups[metaGroupName]; ok {
			return group, true
		}
	}
	return RaftzGroup{}, false
}

// barrier creates the barrier stream, waits for the create's answer, pages STREAM.NAMES until it is
// listed, and deletes it. A barrier stream left by another hold, with another subject, is deleted
// and made again.
func (b MetaBarrier) barrier(ctx context.Context, nc *nats.Conn, js jetstream.JetStream) error {
	name := natsgrant.HoldBarrierStream
	cfg := jetstream.StreamConfig{
		Name:     name,
		Subjects: []string{barrierSubjectPrefix + b.HoldID},
		Replicas: 1,
		Storage:  jetstream.MemoryStorage,
		MaxMsgs:  1,
	}
	err := jsCall(ctx, func(ctx context.Context) error { _, err := js.CreateStream(ctx, cfg); return err })
	if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
		if err := jsCall(ctx, func(ctx context.Context) error { return js.DeleteStream(ctx, name) }); err != nil {
			return unknownf(StepBarrier, err, "delete the barrier stream %s another hold left", name)
		}
		err = jsCall(ctx, func(ctx context.Context) error { _, err := js.CreateStream(ctx, cfg); return err })
	}
	if err != nil {
		return unknownf(StepBarrier, err, "create the barrier stream %s", name)
	}
	listed, cancel := context.WithTimeout(ctx, barrierWithin)
	defer cancel()
	for {
		names, err := listAll(listed, nc, StepBarrier, natsserver.JSApiStreams, streamChecks{},
			func(page *listPage[string]) []string { return page.Streams }, func(name string) string { return name })
		if err != nil {
			return err
		}
		if slices.Contains(names, name) {
			break
		}
		select {
		case <-listed.Done():
			return unknownf(StepBarrier, listed.Err(), "STREAM.NAMES did not list the barrier stream %s within %s of its create", name, barrierWithin)
		case <-time.After(restartPoll):
		}
	}
	if err := jsCall(ctx, func(ctx context.Context) error { return js.DeleteStream(ctx, name) }); err != nil {
		return unknownf(StepBarrier, err, "delete the barrier stream %s", name)
	}
	return nil
}

// jsCall runs one JetStream request within jsRequest.
func jsCall(ctx context.Context, call func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, jsRequest)
	defer cancel()
	return call(ctx)
}

// oneToken refuses a value that is not one subject token.
func oneToken(what, token string) error {
	if token == "" || strings.ContainsAny(token, ".*> \t\r\n") {
		return fmt.Errorf("the %s %q is not one subject token", what, token)
	}
	return nil
}

// ReadBucket is a held command's read of bucket behind the bucket barrier: holdrecord.Barrier puts
// hold.barrier.<holdID> in the bucket and returns once the stream's leader acknowledged it, which
// it does only once it has applied every entry before it, a stale write that a quorum stored before
// the restart included; then read reads the bucket. The leader that acknowledged the barrier must
// be the one that answered the read: the stream's leader, from STREAM.INFO, is read before the
// barrier and after the read, and when it changed, the barrier is written again and the read made
// again (a new leader on nats-server 2.10.29 can answer before it has applied its log). A barrier
// put that no stream leader takes yet (no responders, or no acknowledgement) is sent again within
// barrierWithin. It returns read's last error, and is UNKNOWN when the leader changed at each of
// barrierAttempts reads.
func ReadBucket(ctx context.Context, nc *nats.Conn, bucket, holdID string, read func(context.Context) error) error {
	return bucketReader{}.run(ctx, nc, bucket, holdID, read)
}

// bucketReader is ReadBucket's rule, with the seam its forced-order row's control flips.
type bucketReader struct {
	// noLeaderCheck leaves out the leader's reading after the read, so a leader change between the
	// barrier and the read goes unseen (TestForcedOrderHoldBucketBarrierLeaderChange's control).
	noLeaderCheck bool
}

func (b bucketReader) run(ctx context.Context, nc *nats.Conn, bucket, holdID string, read func(context.Context) error) error {
	if nc == nil {
		return unknownf(StepBarrier, nil, "the bucket barrier has no operator connection")
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return unknownf(StepBarrier, err, "the operator's JetStream context")
	}
	stream := kvStreamPrefix + bucket
	var changed error
	for range barrierAttempts {
		before, err := streamLeader(ctx, nc, stream)
		if err != nil {
			return err
		}
		if err := bucketBarrier(ctx, js, bucket, holdID); err != nil {
			return err
		}
		readErr := read(ctx)
		if b.noLeaderCheck {
			return readErr
		}
		after, err := streamLeader(ctx, nc, stream)
		if err != nil {
			return err
		}
		if after == before {
			return readErr
		}
		changed = fmt.Errorf("stream %s's leader was %s at the barrier and %s after the read", stream, before, after)
	}
	return unknownf(StepBarrier, changed, "bucket %s's leader changed between the barrier and the read at each of %d attempts", bucket, barrierAttempts)
}

// bucketBarrier is holdrecord.Barrier, sent again while no stream leader takes it, within
// barrierWithin.
func bucketBarrier(ctx context.Context, js jetstream.JetStream, bucket, holdID string) error {
	within, cancel := context.WithTimeout(ctx, barrierWithin)
	defer cancel()
	for {
		err := jsCall(within, func(ctx context.Context) error { return holdrecord.Barrier(ctx, js, bucket, holdID) })
		if err == nil {
			return nil
		}
		untaken := errors.Is(err, jetstream.ErrNoStreamResponse) || errors.Is(err, nats.ErrNoResponders) || errors.Is(err, context.DeadlineExceeded)
		if !untaken || within.Err() != nil {
			return unknownf(StepBarrier, err, "the bucket barrier of %s", bucket)
		}
		select {
		case <-within.Done():
			return unknownf(StepBarrier, err, "the bucket barrier of %s was not acknowledged within %s", bucket, barrierWithin)
		case <-time.After(restartPoll):
		}
	}
}

// streamLeader is the server_name of stream's leader, from its STREAM.INFO, asked again within
// barrierWithin while no leader answers it. A lone server's stream has no cluster and reads "".
func streamLeader(ctx context.Context, nc *nats.Conn, stream string) (string, error) {
	within, cancel := context.WithTimeout(ctx, barrierWithin)
	defer cancel()
	for {
		info, found, err := streamInfo(within, nc, stream)
		switch {
		case err == nil && !found:
			return "", unknownf(StepBarrier, nil, "stream %s is not found", stream)
		case err == nil && info.Cluster == nil:
			return "", nil
		case err == nil && info.Cluster.Leader != "":
			return info.Cluster.Leader, nil
		case err == nil:
			err = fmt.Errorf("stream %s answers no leader", stream)
		}
		select {
		case <-within.Done():
			return "", unknownf(StepBarrier, err, "stream %s's leader did not answer within %s", stream, barrierWithin)
		case <-time.After(restartPoll):
		}
	}
}
