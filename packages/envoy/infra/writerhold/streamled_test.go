package writerhold

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// ledResponder answers STREAM.INFO of one stream on a plain server from a script: each read takes
// the next entry, an empty entry leaving that read unanswered, and the last entry answers every
// read after it.
type ledResponder struct {
	mu     sync.Mutex
	script []string
	reads  int
}

func newLedResponder(t *testing.T, url, stream string, script ...string) *ledResponder {
	t.Helper()
	r := &ledResponder{script: script}
	conn, err := testnats.ConnectSeed(t, url, "")
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	_, err = conn.Subscribe(fmt.Sprintf(natsserver.JSApiStreamInfoT, stream), func(msg *nats.Msg) {
		r.mu.Lock()
		body := r.script[min(r.reads, len(r.script)-1)]
		r.reads++
		r.mu.Unlock()
		if body != "" {
			_ = msg.Respond([]byte(body))
		}
	})
	require.NoError(t, err)
	require.NoError(t, conn.Flush())
	return r
}

// count is how many reads the responder was sent.
func (r *ledResponder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// ledInfo is the info of stream at three copies with leader, empty for none, and its other copies.
func ledInfo(t *testing.T, stream, leader string, copies ...jetstream.PeerInfo) string {
	t.Helper()
	cluster := &jetstream.ClusterInfo{Leader: leader}
	for i := range copies {
		cluster.Replicas = append(cluster.Replicas, &copies[i])
	}
	body, err := json.Marshal(jetstream.StreamInfo{Config: jetstream.StreamConfig{Name: stream, Replicas: 3}, Cluster: cluster})
	require.NoError(t, err)
	return string(body)
}

// streamNotFound is a STREAM.INFO answer of err_code 10059.
const streamNotFound = `{"type":"io.nats.jetstream.api.v1.stream_info_response","error":{"code":404,"err_code":10059,"description":"stream not found"}}`

// After each pod's restart the hold waits for each present protected stream's group, read again
// until its info names a leader and every copy current (awaitStreamLed): nats-server answers a
// clustered stream's info from the group's leader alone, and a member whose JetStream started
// under 10 s ago answers nothing while its group has no leader (isGroupLeaderless), so the held
// command's first read after the set's last restart once drew no answer for jsRequest's 10 s
// (TestHoldTheRestoresOneRerun at a load average near 1,100). Scripted on a plain server: two reads
// unanswered, then no leader, then a copy behind, then a copy offline, then led with every copy
// current, where the wait returns, at the sixth read. The control is the held command's one read of
// a stream whose group answers nothing: UNKNOWN when its wait ends. A group that never names a
// leader is UNKNOWN at the limit, naming the stream and why. Before the first eviction an absent
// stream is absent and not waited for; after a restart the same answer is waited on.
func TestHoldTheRestartWaitsForEachStreamsGroup(t *testing.T) {
	_, url := testnats.StartConfig(t, "", nil, true)
	nc, err := testnats.ConnectSeed(t, url, "", nats.CustomInboxPrefix(natsgrant.OperatorInbox))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	const read = 300 * time.Millisecond

	sessions := kvStreamPrefix + bus.SessionBucket
	n2, n3 := jetstream.PeerInfo{Name: "n2", Current: true}, jetstream.PeerInfo{Name: "n3", Current: true}
	led := newLedResponder(t, url, sessions, "", "",
		ledInfo(t, sessions, ""),
		ledInfo(t, sessions, "n1", jetstream.PeerInfo{Name: "n2", Lag: 4}, n3),
		ledInfo(t, sessions, "n1", n2, jetstream.PeerInfo{Name: "n3", Offline: true}),
		ledInfo(t, sessions, "n1", n2, n3))
	present, err := awaitStreamLed(ctx, nc, "after pod envoy-nats-2's restart", sessions, read, false)
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, 6, led.count(), "the wait returned before the group was led with every copy current")

	// The control: the held command's one read, of a stream whose group answers nothing.
	interests := kvStreamPrefix + bus.InterestBucket
	newLedResponder(t, url, interests, "")
	once, cancelOnce := context.WithTimeout(ctx, time.Second)
	defer cancelOnce()
	_, _, err = streamInfo(once, nc, interests)
	require.ErrorIs(t, err, context.DeadlineExceeded, "control: the one read of a group that answers nothing")
	t.Logf("control, the one read: %v", err)

	// A group that never names a leader is UNKNOWN at the limit.
	roles := kvStreamPrefix + bus.RoleClaimBucket
	newLedResponder(t, url, roles, ledInfo(t, roles, ""))
	limited, cancelLimited := context.WithTimeout(ctx, 2*time.Second)
	defer cancelLimited()
	_, err = awaitStreamLed(limited, nc, "after pod envoy-nats-0's restart", roles, read, false)
	unknown := requireUnknown(t, err, StepRestart, roles, "no leader yet", "after pod envoy-nats-0's restart")
	t.Logf("a group with no leader at the limit: %v", unknown)

	// Absent before the first eviction is absent; after a restart it is waited on.
	agents := kvStreamPrefix + bus.LegionAgentBucket
	newLedResponder(t, url, agents, streamNotFound)
	present, err = awaitStreamLed(ctx, nc, "before the first eviction", agents, read, true)
	require.NoError(t, err)
	require.False(t, present)
	gone, cancelGone := context.WithTimeout(ctx, 2*time.Second)
	defer cancelGone()
	_, err = awaitStreamLed(gone, nc, "after pod envoy-nats-1's restart", agents, read, false)
	requireUnknown(t, err, StepRestart, agents, "stream not found")
}
