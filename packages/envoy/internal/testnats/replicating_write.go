package testnats

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// wrongLastSequenceReplicated is JetStream's wrong last sequence as the leader of a stream of more
// than one copy answers it for a write that came while another write of the same subject was still
// being replicated: error 10164, where a write over a revision already replaced is answered 10071.
var wrongLastSequenceReplicated = &natsgo.APIError{Code: 400, ErrorCode: natsgo.JSErrCodeStreamWrongLastSequenceConstant}

// MoveStreamLeader makes member i the leader of stream, a stream of more than one copy on the
// cluster, and returns once the stream reports member i as its leader with every copy current. It
// asks the leader to step down with member i preferred ($JS.API.STREAM.LEADER.STEPDOWN with a
// placement), and asks again while another member leads: the stream's group elects the preferred
// member when it can, and an election lost to another member is asked again. Each read and each
// ask waits readWait for its answer, as awaitInfo's reads do, so one sent across a leader change
// that draws no answer is sent again. A test moves a stream's leader to the member its client is
// connected to, so a write that client sends reaches the leader without a route
// (HoldReplicatingWrite).
func (c *Cluster) MoveStreamLeader(t testing.TB, stream string, i int) {
	t.Helper()
	conn := Connect(t, c.URL())
	defer conn.Close()
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	want := c.Name(i)
	request, err := json.Marshal(map[string]any{"placement": map[string]string{"preferred": want}})
	if err != nil {
		t.Fatalf("encode the step-down request: %v", err)
	}
	awaitInfo(t, fmt.Sprintf("stream %s to be led by %s with every copy current", stream, want),
		func() (*natsgo.StreamInfo, error) {
			info, err := js.StreamInfo(stream, natsgo.MaxWait(readWait))
			if err != nil || info.Cluster == nil || info.Cluster.Leader == want || info.Cluster.Leader == "" {
				return info, err
			}
			if _, err := conn.Request("$JS.API.STREAM.LEADER.STEPDOWN."+stream, request, readWait); err != nil {
				return info, fmt.Errorf("ask %s to step down: %w", info.Cluster.Leader, err)
			}
			return info, nil
		},
		func(info *natsgo.StreamInfo) bool {
			return info.Cluster != nil && info.Cluster.Leader == want && settled(info.Cluster, info.Config.Replicas)
		},
		describeStream)
}

// HoldReplicatingWrite puts a write of subject in stream, a stream of more than one copy on the
// cluster, where the stream's leader has proposed it and no other copy has taken it, and returns the
// func that lets it land and returns once every copy has applied it (awaitApplied), which runs once
// however often it is called and once more when the test ends. While the write is held, the leader
// answers every other write of subject that names the subject's last sequence (a Create, an
// Update, a Delete or Purge with nats.LastRevision, a publish with
// nats.ExpectLastSequencePerSubject) with JetStream's wrong last sequence as error 10164
// (nats-server's checkMsgHeadersPreClusteredProposal, its expectedPerSubjectInProcess), the answer
// such a write meets on a replicated bucket when it loses a race to a write still being replicated:
// on a stream of one copy every such loser is answered 10071.
//
// The held write is the subject's last message written again over that message's sequence, so the
// record it leaves is the one that was there, at a new revision, as a writer that rewrote the same
// record leaves it: the same data and the headers that make the record what it is
// (heldRecordHeaders), so a delete marker is rewritten as a delete marker and a purge as a purge.
// The subject must hold a message. The cluster's routes are frozen while it is held, so nothing a
// test sends may need a route meanwhile: its client connects to the leader's member
// (MoveStreamLeader), and a read waits too, since any copy may be the one that answers a direct get
// (a Create that lost reads the key back, and that read can wait out its JetStream MaxWait). The
// hold returns only once the leader answers a write of subject with 10164, so the next write meets
// the held one.
func (c *Cluster) HoldReplicatingWrite(t testing.TB, stream, subject string) func() {
	t.Helper()
	info := AwaitSettledStream(t, c.JetStream(t), stream)
	if info.Config.Replicas < 2 {
		t.Fatalf("stream %s has %d copy, want more than one: only a replicated stream holds a write in process", stream, info.Config.Replicas)
	}
	leader := c.Member(info.Cluster.Leader)
	if leader < 0 {
		t.Fatalf("stream %s is led by %q, which is no member of the cluster", stream, info.Cluster.Leader)
	}
	conn := Connect(t, c.Servers[leader].URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("open JetStream on %s: %v", info.Cluster.Leader, err)
	}
	last, err := js.GetLastMsg(stream, subject)
	if err != nil {
		t.Fatalf("read the last message of %s in %s: %v", subject, stream, err)
	}

	thaws := make([]func(), 0, len(c.routes))
	for _, route := range c.routes {
		thaws = append(thaws, route.freeze())
	}
	thaw := func() {
		for _, thaw := range thaws {
			thaw()
		}
	}
	held := natsgo.NewMsg(subject)
	held.Data = last.Data
	for _, name := range heldRecordHeaders {
		if values := last.Header.Values(name); len(values) > 0 {
			held.Header[name] = values
		}
	}
	held.Header.Set(natsgo.ExpectedLastSubjSeqHdr, strconv.FormatUint(last.Sequence, 10))
	ack, err := js.PublishMsgAsync(held)
	if err != nil {
		thaw()
		t.Fatalf("send the held write of %s: %v", subject, err)
	}
	// release runs its body once through a sync.Once's Do, not sync.OnceFunc: a give-up in either
	// wait below fails t through t.Fatalf, which ends the goroutine with runtime.Goexit. Once.Do
	// lets that Goexit through, so the row fails by name, where OnceFunc's recover reads nil and
	// calls panic(nil) (sync/oncefunc.go:23-31), which ends the whole test binary.
	var once sync.Once
	release := func() {
		once.Do(func() {
			thaw()
			var refused error
			testwait.Until(t, fmt.Sprintf("the held write of %s in %s to be answered once released", subject, stream), func() (string, bool) {
				select {
				case <-ack.Ok():
					return "acknowledged", true
				case refused = <-ack.Err():
					return fmt.Sprintf("refused: %v", refused), true
				default:
					return "no answer yet", false
				}
			})
			if refused != nil {
				t.Errorf("the held write of %s in %s did not land: %v", subject, stream, refused)
				return
			}
			c.awaitApplied(t, stream, info.Cluster.RaftGroup, leader)
		})
	}
	t.Cleanup(release)

	// A probe names a sequence the subject does not hold, so the leader never proposes it: it is
	// answered 10071 until the held write is in process, and 10164 from then on. Each probe waits
	// readWait for its answer, so one that draws none is sent again. A hold the shared wait gives
	// up on is released by the cleanup registered above.
	testwait.Until(t, fmt.Sprintf("the leader of %s to hold the write of %s in process (a probe answered 10164)", stream, subject), func() (error, bool) {
		_, err := js.Publish(subject, last.Data, natsgo.ExpectLastSequencePerSubject(last.Sequence+1), natsgo.AckWait(readWait))
		return err, errors.Is(err, wrongLastSequenceReplicated)
	})
	return release
}

// heldRecordHeaders are the headers of a stored KV record that make it the record it is, which
// HoldReplicatingWrite copies onto its rewrite. nats.go v1.55.0's KV stores none on a Put, and
// Nats-Expected-Last-Subject-Sequence alone on a Create or an Update; a Delete stores
// KV-Operation: DEL, and a Purge KV-Operation: PURGE and Nats-Rollup: sub (kv.go:842-847), with
// Nats-Expected-Last-Subject-Sequence beside either when it names a revision. KV-Operation makes a
// record a marker, which a read answers as no key, and Nats-Rollup makes a purge drop the subject's
// earlier records, as the purge it rewrites did. Nats-Expected-Last-Subject-Sequence is no part of
// the record but the condition its writer sent it under; the rewrite carries its own.
var heldRecordHeaders = []string{"KV-Operation", natsgo.MsgRollup}

// AwaitReplicated waits until every copy of stream, a stream of more than one copy on the cluster,
// has applied every write its leader has committed (awaitApplied), and fails the test when the
// shared wait gives up (testwait.Until). A test calls it between a write and a read of what the
// write left: the leader answers a write once a quorum holds it, and the direct get every bucket
// read sends may be answered by the copy outside that quorum, which then answers the record the
// write replaced.
func (c *Cluster) AwaitReplicated(t testing.TB, stream string) {
	t.Helper()
	info := AwaitSettledStream(t, c.JetStream(t), stream)
	leader := c.Member(info.Cluster.Leader)
	if leader < 0 {
		t.Fatalf("stream %s is led by %q, which is no member of the cluster", stream, info.Cluster.Leader)
	}
	c.awaitApplied(t, stream, info.Cluster.RaftGroup, leader)
}

// awaitApplied waits until every running member has applied, in stream's Raft group named group,
// every entry the group's leader, member leader, has committed, and fails the test when the shared
// wait gives up (testwait.Until), whose failure prints the member's Raft group as it last read it,
// or that the member listed no Raft group of that name.
// Any copy may answer a direct get, and the leader answers a write once a quorum holds it, so a
// copy outside that quorum can answer a read of a record the write replaced; once every copy has
// applied the write, none does. The stream is the global account's, as a stream of a cluster
// StartCluster started is: nats-server lists a stream's Raft group under the account that holds the
// stream (Server.Raftz).
func (c *Cluster) awaitApplied(t testing.TB, stream, group string, leader int) {
	t.Helper()
	raft := func(i int) *natsserver.RaftzGroup {
		srv := c.Servers[i].natsServer()
		if srv == nil {
			return nil
		}
		status := srv.Raftz(&natsserver.RaftzOptions{AccountFilter: natsserver.DEFAULT_GLOBAL_ACCOUNT, GroupFilter: group})
		if status == nil {
			return nil
		}
		for _, groups := range *status {
			if g, ok := groups[group]; ok {
				return &g
			}
		}
		return nil
	}
	led := raft(leader)
	if led == nil {
		t.Errorf("member %s holds no Raft group %s of stream %s", c.Name(leader), group, stream)
		return
	}
	for i := range c.Servers {
		testwait.Until(t, fmt.Sprintf("member %s to apply stream %s's entries up to %d", c.Name(i), stream, led.Committed), func() (string, bool) {
			g := raft(i)
			reading := fmt.Sprintf("member %s lists no Raft group %s", c.Name(i), group)
			if g != nil {
				reading = fmt.Sprintf("%+v", *g)
			}
			return reading, c.Servers[i].natsServer() == nil || g != nil && g.Applied >= led.Committed
		})
	}
}

// RacedKeyValue is a handle of a bucket of more than one copy on a cluster whose next conditional
// writes each lose a race to a write of the same key still being replicated (Cluster.RaceKeyValue).
type RacedKeyValue struct {
	natsgo.KeyValue
	cluster *Cluster
	t       testing.TB

	mu     sync.Mutex
	losses int
	lost   int
}

// RaceKeyValue wraps kv, a handle of a bucket of more than one copy on the cluster, so that each of
// its next losses writes that name a revision - a Create, an Update, and a Delete or Purge with
// nats.LastRevision - is sent while HoldReplicatingWrite holds a rewrite of the key's record in
// process at the bucket's leader, and that rewrite lands once the write has its answer: the write
// loses to a writer whose write was still being replicated, and the leader answers it 10164, as a
// replicated bucket answers the loser of such a race. A raced write answered anything else fails
// the test, so a race that did not happen is never read as one that was answered as wanted, and
// Lost counts the writes that lost. Every later write, and every other call, reaches the bucket as
// it is, and so does every write once t has ended: a race a failed test left armed does not reach
// into the next test that writes through the same handle.
//
// What HoldReplicatingWrite requires holds for each raced write: kv's connection reaches the
// bucket's leader without a route (MoveStreamLeader), the key holds a record (a Create of a key
// that has none cannot lose this way), and the write names a revision (an unconditional one would
// wait for the frozen routes). A hold that fails calls t.Fatalf, which ends the test from the
// test's goroutine and panics the test binary from any other (a caller's bus.Bounded).
func (c *Cluster) RaceKeyValue(t testing.TB, kv natsgo.KeyValue, losses int) *RacedKeyValue {
	raced := &RacedKeyValue{KeyValue: kv, cluster: c, t: t, losses: losses}
	t.Cleanup(func() {
		raced.mu.Lock()
		raced.losses = 0
		raced.mu.Unlock()
	})
	return raced
}

// Lost is how many writes have lost a race so far.
func (kv *RacedKeyValue) Lost() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.lost
}

// race sends write, which writes key, losing it a race while races are left.
func (kv *RacedKeyValue) race(key string, write func() error) error {
	kv.mu.Lock()
	racing := kv.losses > 0
	if racing {
		kv.losses--
	}
	kv.mu.Unlock()
	if !racing {
		return write()
	}
	kv.t.Helper()
	bucket := kv.Bucket()
	release := kv.cluster.HoldReplicatingWrite(kv.t, "KV_"+bucket, "$KV."+bucket+"."+key)
	err := write()
	release()
	if !errors.Is(err, wrongLastSequenceReplicated) {
		kv.t.Errorf("the write of %s raced against a write still being replicated was answered %v, want the replicated stream's wrong last sequence, 10164", key, err)
	}
	kv.mu.Lock()
	kv.lost++
	kv.mu.Unlock()
	return err
}

func (kv *RacedKeyValue) Create(key string, value []byte) (uint64, error) {
	var revision uint64
	err := kv.race(key, func() (err error) { revision, err = kv.KeyValue.Create(key, value); return err })
	return revision, err
}

func (kv *RacedKeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	var revision uint64
	err := kv.race(key, func() (err error) { revision, err = kv.KeyValue.Update(key, value, last); return err })
	return revision, err
}

func (kv *RacedKeyValue) Delete(key string, opts ...natsgo.DeleteOpt) error {
	return kv.race(key, func() error { return kv.KeyValue.Delete(key, opts...) })
}

func (kv *RacedKeyValue) Purge(key string, opts ...natsgo.DeleteOpt) error {
	return kv.race(key, func() error { return kv.KeyValue.Purge(key, opts...) })
}
