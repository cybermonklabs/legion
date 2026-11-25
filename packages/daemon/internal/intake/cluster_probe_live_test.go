package intake

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/testnatsimages"

	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The idle probe on a three-node cluster of each server the deployment runs: the clustered hub's
// nats-server 2.15.3 and the VM hub's 2.15.1. The notification stream and the daemon's durables are
// replicated on all three, as a clustered hub keeps them.
func TestTheIdleProbeOnAThreeNodeCluster(t *testing.T) {
	for _, hub := range []struct{ name, image string }{
		{"clustered hub 2.15.3", testnatsimages.ClusteredHubImage},
		{"VM hub 2.15.1", testnats.Image},
	} {
		t.Run(hub.name, func(t *testing.T) {
			t.Parallel()
			clusterProbeRows(t, hub.image)
		})
	}
}

// clusterRig is a three-node cluster with the notification stream on all three members, the
// daemon's consumers opened through member daemon, which did not lead the stream when the rig was
// made, and the stream's leader as last read. admin and stream are read through member daemon,
// which no row cuts off from the other two while it reads them, conns is a client of each member,
// and deleted is the server's advisory of each probe it deletes.
type clusterRig struct {
	cluster   *testnats.Cluster
	admin     jetstream.JetStream
	stream    jetstream.Stream
	consumers *Consumers
	standIn   *lagStandIn
	logs      *lockedBuffer
	conns     [3]*nats.Conn
	deleted   *nats.Subscription
	leader    int
	daemon    int
}

func newClusterRig(t *testing.T, image string) *clusterRig {
	t.Helper()
	cluster := testnats.StartCluster(t, image)
	_, first := cluster.Connect(t, 0)
	// The meta leader places a replicated stream only on peers whose JetStream it has heard from,
	// which its peers' server statistics tell it a few seconds after the group formed: until then a
	// create is refused with no suitable peers for placement (10005).
	var stream jetstream.Stream
	testwait.Eventually(t, "the replicated notification stream created", func() bool {
		read, cancel := rowRead(t)()
		defer cancel()
		var err error
		stream, err = first.CreateStream(read, jetstream.StreamConfig{Name: notificationStream, Subjects: []string{"notifications.>"}, Replicas: 3, AllowDirect: true})
		var apiErr *jetstream.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode == 10005 {
			return false
		}
		if err != nil {
			t.Fatalf("create the replicated notification stream: %v", err)
		}
		return true
	})
	r := &clusterRig{cluster: cluster, stream: stream}
	r.leader = r.currentLeader(t)
	r.daemon = (r.leader + 1) % 3
	for i := range r.conns {
		r.conns[i], _ = cluster.Connect(t, i)
	}
	_, r.admin = cluster.Connect(t, r.daemon)
	read, cancel := rowRead(t)()
	var err error
	r.stream, err = r.admin.Stream(read, notificationStream)
	cancel()
	if err != nil {
		t.Fatalf("open the stream through %s: %v", cluster.Name(r.daemon), err)
	}
	if r.deleted, err = r.admin.Conn().SubscribeSync("$JS.EVENT.ADVISORY.CONSUMER.DELETED." + notificationStream + "." + dispatchProbeName("CAPTURE")); err != nil {
		t.Fatalf("watch the probe's deletions: %v", err)
	}
	_, daemon := cluster.Connect(t, r.daemon)
	r.standIn, r.logs = &lagStandIn{JetStream: daemon}, &lockedBuffer{}
	read, cancel = rowRead(t)()
	defer cancel()
	if r.consumers, err = OpenConsumers(read, r.standIn, migratedPool(t), consumerSpec(r.logs)); err != nil {
		t.Fatalf("OpenConsumers on the cluster: %v", err)
	}
	return r
}

// currentLeader waits until the stream has a leader that counts both other members as current
// replicas, and answers it.
func (r *clusterRig) currentLeader(t *testing.T) int {
	t.Helper()
	leader := -1
	testwait.Eventually(t, "the stream led, with both replicas current", func() bool {
		read, cancel := rowRead(t)()
		defer cancel()
		info, err := r.stream.Info(read)
		if err != nil || info.Cluster == nil || info.Cluster.Leader == "" || len(info.Cluster.Replicas) != 2 {
			return false
		}
		for _, replica := range info.Cluster.Replicas {
			if !replica.Current || replica.Offline || replica.Lag != 0 {
				return false
			}
		}
		leader = r.cluster.Member(info.Cluster.Leader)
		return leader >= 0
	})
	return leader
}

// arrange moves the stream's leadership to member lead, and the Dispatch durable's off member cut,
// asking each group to step down with lead as its preferred leader until both stand so: a member
// about to be cut off then leads neither, and the reads of the durable and the stream through the
// quorum go on while it is cut off. A group whose leader is cut off answers nothing until its
// remaining peers elect another, which a stepdown before the cut makes unneeded.
func (r *clusterRig) arrange(t *testing.T, lead, cut int) {
	t.Helper()
	dispatch := dispatchConsumerName(r.consumers.spec.Project)
	preferred := []byte(`{"placement":{"preferred":"` + r.cluster.Name(lead) + `"}}`)
	r.moveLeader(t, "the stream", "$JS.API.STREAM.LEADER.STEPDOWN."+notificationStream, preferred,
		func(ctx context.Context) (string, error) {
			info, err := r.stream.Info(ctx)
			if err != nil || info.Cluster == nil {
				return "", err
			}
			return info.Cluster.Leader, nil
		},
		func(leader string) bool { return leader == r.cluster.Name(lead) })
	r.moveLeader(t, "the Dispatch durable", "$JS.API.CONSUMER.LEADER.STEPDOWN."+notificationStream+"."+dispatch, preferred,
		func(ctx context.Context) (string, error) {
			durable, err := r.stream.Consumer(ctx, dispatch)
			if err != nil || durable.CachedInfo().Cluster == nil {
				return "", err
			}
			return durable.CachedInfo().Cluster.Leader, nil
		},
		func(leader string) bool { return leader != r.cluster.Name(cut) })
	r.leader = r.currentLeader(t)
}

// leaderElectionWait bounds the wait for a group to name a leader again after a stepdown: an
// election of three members takes a few seconds under the race detector on a loaded runner.
const leaderElectionWait = 15 * time.Second

// moveLeader asks group to step down, through the subject stepdown with body, until its leader as
// leaderOf reads it is one placed accepts. Each request waits for the election it starts to name a
// leader other than the one it stepped down before it asks again: a stepdown sent while the group
// is still electing starts another election, and asking on every poll kept a 2.15.1 cluster
// electing for the whole of a minute in CI under the race detector.
func (r *clusterRig) moveLeader(t *testing.T, group, stepdown string, body []byte, leaderOf func(context.Context) (string, error), placed func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(testwait.Bounded(t, 3*time.Minute, 30*time.Second))
	led := func(not string) string {
		until := time.Now().Add(leaderElectionWait)
		for {
			read, cancel := rowRead(t)()
			leader, err := leaderOf(read)
			cancel()
			if err == nil && leader != "" && leader != not {
				return leader
			}
			if time.Now().After(until) {
				return ""
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	leader := led("")
	for attempt := 1; ; attempt++ {
		if leader != "" && placed(leader) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is led by %q after %d stepdowns, want a leader its placement accepts", group, leader, attempt-1)
		}
		read, cancel := rowRead(t)()
		_, err := r.admin.Conn().RequestWithContext(read, stepdown, body)
		cancel()
		if err != nil {
			t.Logf("%s's stepdown %d: %v", group, attempt, err)
		}
		if next := led(leader); next != "" {
			leader = next
		} else {
			leader = led("")
		}
	}
}

// probeThrough is this project's idle probe sending its create through conn, a client of one member.
func (r *clusterRig) probeThrough(conn *nats.Conn) *idleProbe {
	return newIdleProbe(conn, r.consumers.spec.Project)
}

// awaitDeleted waits for the server's advisory that it deleted the probe a create made: a direct
// consumer is no consumer the meta group assigns, so no member answers its info (waitProbeGone
// reads a lone server), and the stream leader that holds it says when it deletes it.
func (r *clusterRig) awaitDeleted(t *testing.T) {
	t.Helper()
	if _, err := r.deleted.NextMsg(testwait.Bounded(t, time.Minute, time.Second)); err != nil {
		t.Fatalf("the server's advisory that it deleted the probe: %v", err)
	}
}

// directLast is the status and data a direct get of subject's last message is answered through
// conn, as a replica answers it from what it holds ("" status for a message found).
func directLast(t *testing.T, conn *nats.Conn, subject string) (string, []byte) {
	t.Helper()
	reply, err := conn.Request("$JS.API.DIRECT.GET."+notificationStream+"."+subject, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("direct get of %s's last message: %v", subject, err)
	}
	return reply.Header.Get("Status"), reply.Data
}

// countThrough is the probe's count from the stream's start, created through conn.
func (r *clusterRig) countThrough(t *testing.T, conn *nats.Conn) (uint64, error) {
	t.Helper()
	read, cancel := rowRead(t)()
	defer cancel()
	answer, config, err := r.probeThrough(conn).create(read, 0)
	if err != nil {
		return 0, err
	}
	return answer.pendingFor(config)
}

func clusterProbeRows(t *testing.T, image string) {
	r := newClusterRig(t, image)
	const subject = "notifications.dispatch.issue.CAPTURE.CAPTURE-3.issue.updated"
	dispatch := dispatchConsumerName(r.consumers.spec.Project)
	daemon := r.daemon
	// The member the first row cuts off, follower, leads nothing when it is cut: not the meta group,
	// which a client cannot ask to step down here, and, once arrange has moved them, neither the
	// stream nor the Dispatch durable. The stream is then led by leader, the third member, so daemon,
	// the member the daemon's consumers read through, is a follower too.
	others := []int{(daemon + 1) % 3, (daemon + 2) % 3}
	meta, err := r.cluster.MetaLeader(daemon)
	if err != nil {
		t.Fatalf("read the meta leader: %v", err)
	}
	follower, leader := others[0], others[1]
	if r.cluster.Name(follower) == meta {
		follower, leader = leader, follower
	}
	leaderJS, err := jetstream.New(r.conns[leader])
	if err != nil {
		t.Fatalf("open the leader's JetStream: %v", err)
	}

	t.Run("controls first: a replica cut off reads the message absent, and the counts read idle", func(t *testing.T) {
		if position := readPosition(t, r.consumers); !position.Idle {
			t.Fatalf("not idle on the cluster with nothing stored:\n%s", r.logs)
		}
		r.awaitDeleted(t)
		r.arrange(t, leader, follower)
		r.cluster.Isolate(t, follower)
		defer func() {
			r.cluster.Rejoin(t, follower)
			r.leader = r.currentLeader(t)
		}()
		sent := publish(t, leaderJS, subject, capturedIssueUpdatedEnvelope(t))
		if status, data := directLast(t, r.conns[follower], subject); status != "404" {
			routes, err := r.cluster.Routes(follower)
			t.Fatalf("the cut-off replica %s answered a direct get of the stored message with status %q and %q (its routes %q, %v; the client reads %s), want 404: it does not show a replica behind",
				r.cluster.Name(follower), status, data, routes, err, r.conns[follower].ConnectedServerName())
		}
		if status, data := directLast(t, r.conns[leader], subject); status != "" || !strings.Contains(string(data), "CAPTURE-3") {
			t.Fatalf("a direct get through the leader %s answered status %q, want the message stored at %d", r.cluster.Name(leader), status, sent.Sequence)
		}
		testwait.Eventually(t, "the durable counting the stored message", func() bool {
			return consumerInfo(t, r.stream, dispatch).NumPending == 1
		})
		r.standIn.uncounted = true
		read, cancel := rowRead(t)()
		handle, err := r.standIn.Consumer(read, notificationStream, dispatch)
		cancel()
		if err != nil || handle.CachedInfo().NumPending != 0 || handle.CachedInfo().NumAckPending != 0 {
			t.Fatalf("the stand-in's counts (%v) do not read zero: a count-only reading would not read idle, so this row does not reach the race", err)
		}
		// An ordinary consumer cannot be put on the cut-off replica: its create goes to the meta
		// leader, which the replica cannot reach, and nats-server takes no placement of a consumer's
		// own, so this rig cannot run the ordinary no-wait candidate on the lagging peer.
		followerJS, err := jetstream.New(r.conns[follower])
		if err != nil {
			t.Fatal(err)
		}
		read, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		_, err = followerJS.CreateConsumer(read, notificationStream, jetstream.ConsumerConfig{FilterSubject: dispatchIssueFilter("CAPTURE"), AckPolicy: jetstream.AckNonePolicy})
		cancel()
		t.Logf("an ordinary R1 consumer's create through the cut-off replica %s: %v", r.cluster.Name(follower), err)

		// The reading through the quorum: the counts read zero and the probe counts the message. The
		// probe is answered through daemon, a follower: only the stream's leader answers a direct
		// consumer's create, as the cut-off replica's silence below shows of a replica that holds
		// the stream itself.
		if position := readPosition(t, r.consumers); position.Idle || position.Reached() || strings.Contains(r.logs.String(), "idle could not be confirmed") {
			t.Fatalf("idle = %v with the message stored and the counts reading zero, want a confirmed not idle:\n%s", position.Idle, r.logs)
		}
		r.awaitDeleted(t)
		if waiting, err := r.countThrough(t, r.conns[daemon]); err != nil || waiting != 1 {
			t.Fatalf("the probe through the follower %s counts %d (%v), want the stored message", r.cluster.Name(daemon), waiting, err)
		}
		r.awaitDeleted(t)
		read, cancel = context.WithTimeout(context.Background(), time.Minute)
		waiting, err := r.probeThrough(r.conns[follower]).pending(read, 0)
		cancel()
		if err == nil {
			t.Fatalf("the probe through the cut-off replica %s was answered %d, want no answer: a replica is no stream leader", r.cluster.Name(follower), waiting)
		}
		t.Logf("the probe through the cut-off replica %s: %v", r.cluster.Name(follower), err)
	})

	t.Run("the probe counts the stored message through every member", func(t *testing.T) {
		for i, conn := range r.conns {
			if waiting, err := r.countThrough(t, conn); err != nil || waiting != 1 {
				t.Fatalf("the probe through %s counts %d (%v), want the stored message", r.cluster.Name(i), waiting, err)
			}
			r.awaitDeleted(t)
		}
	})

	t.Run("a reading while the stream's leader changes is never idle", func(t *testing.T) {
		// A reading whose create the old leader made and whose answer the change lost leaves a probe
		// standing, so the next reading may find it: a reading that cannot confirm is not idle too.
		confirmed, unconfirmed := 0, 0
		for range 4 {
			before := strings.Count(r.logs.String(), "idle could not be confirmed")
			var wg sync.WaitGroup
			wg.Go(func() {
				read, cancel := rowRead(t)()
				defer cancel()
				if _, err := r.admin.Conn().RequestWithContext(read, "$JS.API.STREAM.LEADER.STEPDOWN."+notificationStream, nil); err != nil {
					t.Logf("the stream leader's step down: %v", err)
				}
			})
			read, cancel := context.WithTimeout(context.Background(), time.Minute)
			position, err := r.consumers.DispatchPosition(read)
			cancel()
			wg.Wait()
			switch {
			case err == nil && (position.Idle || position.Reached()):
				t.Fatalf("idle while the stream's leader changed, with the message stored and the counts reading zero:\n%s", r.logs)
			case err != nil || strings.Count(r.logs.String(), "idle could not be confirmed") > before:
				unconfirmed++
			default:
				confirmed++
			}
			r.leader = r.currentLeader(t)
		}
		t.Logf("four readings during a change of the stream's leader: %d confirmed the message waits, %d could not confirm, none idle", confirmed, unconfirmed)
		var counted uint64
		testwait.Eventually(t, "the probe answered by the new leader once the changes are over", func() bool {
			waiting, err := r.countThrough(t, r.conns[daemon])
			counted = waiting
			return err == nil
		})
		if counted != 1 {
			t.Fatalf("the probe after the changes counts %d, want the stored message counted by the new leader %s", counted, r.cluster.Name(r.leader))
		}
	})

	t.Run("with no stream leader the probe is not answered", func(t *testing.T) {
		// The member left alone is daemon, the one the daemon's consumers read through, and no
		// leader of the stream: a leader cut off answers from its own store until it finds it lost
		// its quorum, which is a stale leader's read, not a stream with none. The other two are cut
		// at once, so daemon never wins an election with one of them before both are gone.
		preferred := []byte(`{"placement":{"preferred":"` + r.cluster.Name((daemon+1)%3) + `"}}`)
		r.moveLeader(t, "the stream", "$JS.API.STREAM.LEADER.STEPDOWN."+notificationStream, preferred,
			func(ctx context.Context) (string, error) {
				info, err := r.stream.Info(ctx)
				if err != nil || info.Cluster == nil {
					return "", err
				}
				return info.Cluster.Leader, nil
			},
			func(leader string) bool { return leader != r.cluster.Name(daemon) })
		r.leader = r.currentLeader(t)
		r.cluster.Isolate(t, (daemon+1)%3, (daemon+2)%3)
		defer func() {
			r.cluster.Rejoin(t, (daemon+1)%3)
			r.cluster.Rejoin(t, (daemon+2)%3)
			r.cluster.AwaitReady(t)
			r.leader = r.currentLeader(t)
		}()
		read, cancel := context.WithTimeout(context.Background(), time.Minute)
		waiting, err := r.probeThrough(r.conns[daemon]).pending(read, 0)
		cancel()
		if err == nil {
			t.Fatalf("the probe through %s, alone of the three, was answered %d, want no answer: no member leads the stream", r.cluster.Name(daemon), waiting)
		}
		t.Logf("the probe with no stream leader: %v", err)
		read, cancel = context.WithTimeout(context.Background(), time.Minute)
		position, err := r.consumers.DispatchPosition(read)
		cancel()
		if err == nil && (position.Idle || position.Reached()) {
			t.Fatalf("the reading with no stream leader is idle, want an error or not idle:\n%s", r.logs)
		}
		t.Logf("the reading with no stream leader: %+v, %v", position, err)
	})

	t.Run("an identical create is answered with the standing probe's info", func(t *testing.T) {
		// A name of the row's own, so no probe a reading left stands in its way.
		conn := r.conns[daemon]
		probe := &idleProbe{conn: conn, name: "legion-go-CAPTURE-identical-probe", filter: dispatchIssueFilter("CAPTURE")}
		config := probeConfig{Name: probe.name, Description: "legion idle probe the same each time", DeliverPolicy: "by_start_sequence",
			OptStartSeq: 1, AckPolicy: "none", FilterSubject: probe.filter, ReplayPolicy: "instant", DeliverSubject: conn.NewRespInbox(),
			HeadersOnly: true, InactiveThreshold: time.Minute, Replicas: 1, MemoryStorage: true, Direct: true}
		body, err := json.Marshal(probeRequest{Stream: notificationStream, Config: config, Action: "create"})
		if err != nil {
			t.Fatal(err)
		}
		var answers [2]probeAnswer
		for i := range answers {
			reply, err := conn.Request(probe.probeSubject(), body, 5*time.Second)
			if err != nil {
				t.Fatalf("create %d of the identical probe: %v", i+1, err)
			}
			if err := json.Unmarshal(reply.Data, &answers[i]); err != nil || answers[i].Error != nil {
				t.Fatalf("create %d of the identical probe answered %s (%v)", i+1, reply.Data, err)
			}
		}
		if !answers[1].Created.Equal(answers[0].Created) {
			t.Fatalf("the second identical create made a consumer at %s, the first at %s, want the first's info answered again", answers[1].Created, answers[0].Created)
		}
		t.Logf("an identical create is answered with the standing probe's info, created %s: only the attempt's own description and delivery subject tell a fresh count from it",
			answers[0].Created.Format(time.RFC3339Nano))
		read, cancel := rowRead(t)()
		_, err = probe.pending(read, 0)
		cancel()
		var apiErr *jetstream.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode != jetstream.JSErrCodeConsumerExists {
			t.Fatalf("a create of the same name whose description differs = %v, want the server's refusal (%d)", err, jetstream.JSErrCodeConsumerExists)
		}
	})
}
