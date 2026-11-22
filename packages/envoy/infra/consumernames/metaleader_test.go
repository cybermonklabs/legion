package consumernames

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// metaLeaders are the meta leader each declared server's own /varz names, by pod; a server that
// does not answer names "".
func (rig *convertRig) metaLeaders(ctx context.Context) map[string]string {
	leaders := map[string]string{}
	for i := range rig.s.Cluster.Servers {
		pod := holdtest.Pod(i)
		leaders[pod] = ""
		monitor, stop, err := rig.op.Hub.Monitor(ctx, pod)
		if err != nil {
			continue
		}
		var varz struct {
			JetStream struct {
				Meta struct {
					Leader string `json:"leader"`
				} `json:"meta"`
			} `json:"jetstream"`
		}
		err = monitor.Get(ctx, "/varz", &varz)
		stop()
		if err == nil {
			leaders[pod] = varz.JetStream.Meta.Leader
		}
	}
	return leaders
}

// metaLeaderPod is the pod of the declared server that leads the JetStream meta group, once every
// declared server's /varz names that one leader, within a minute. (testnats' AwaitReady connects
// with no credential, which the callout cluster refuses.)
func (rig *convertRig) metaLeaderPod(ctx context.Context) string {
	rig.t.Helper()
	var leaders map[string]string
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		leaders = rig.metaLeaders(ctx)
		agreed, one := "", true
		for _, leader := range leaders {
			if leader == "" || (agreed != "" && leader != agreed) {
				one = false
				break
			}
			agreed = leader
		}
		if i := rig.s.Cluster.Member(agreed); one && i >= 0 {
			return holdtest.Pod(i)
		}
	}
	rig.t.Fatalf("the declared servers name no one meta leader within a minute: %v", leaders)
	return ""
}

// metaLeaderMove evicts the meta leader's pod, as the hub's restart of it does, and waits until
// every declared server names one meta leader again and step 1's reading of the servers passes
// (writerhold.Discover: no route still to the evicted process): the meta leader moves.
func (rig *convertRig) metaLeaderMove(ctx context.Context) string {
	rig.t.Helper()
	pod := rig.metaLeaderPod(ctx)
	require.NoError(rig.t, rig.s.Kube.Client.PolicyV1().Evictions(holdtest.Namespace).Evict(ctx,
		&policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: holdtest.Namespace}}))
	rig.metaLeaderPod(ctx)
	var err error
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, err = writerhold.Discover(ctx, rig.op.Hub); err == nil {
			return pod
		}
	}
	rig.t.Fatalf("the declared servers' reading did not pass within a minute of %s's eviction: %v", pod, err)
	return pod
}

// A meta-leader change after the barrier discards the listing, on three servers: convert's listing
// that a meta leader other than the barrier's may have answered is not taken. The row moves the
// meta leader (an eviction of its pod) after the barrier stream is listed and the consumer names
// read, before the meta group is read again; the term check then sees another term and leader,
// discards the listing, makes the barrier again and lists again, and the conversion goes on from
// the second listing. Control, run first in a hold of its own: with the term check removed,
// convert takes the listing the leader change followed. It fails when a listing answered across a
// meta-leader change is taken.
//
// The row forces the rule, not the miss: a successor that holds an earlier consumer create's apply
// (meta.apply) does not stand for leader while it has a committed entry it has not applied
// (nats-server 2.10.29 raft.go:4212-4217, 2.15.1 raft.go:6250-6255, as writerhold's
// TestForcedOrderHoldBucketBarrierAcrossTheRestart measured for a stream), so a listing that misses
// that consumer cannot be forced, and both runs here list it whole.
func TestAMetaLeaderChangeAfterTheBarrierDiscardsTheListing(t *testing.T) {
	metaLeaderRow(t)
}

// metaLeaderRow is TestAMetaLeaderChangeAfterTheBarrierDiscardsTheListing's row, which
// forced-order.sh also runs on both hooked servers
// (TestForcedOrderAMetaLeaderChangeAfterTheBarrierDiscardsTheListing).
func metaLeaderRow(t *testing.T) {
	rig := newConvertRig(t, 0)
	ctx := context.Background()
	for _, run := range []struct {
		name    string
		fixed   string
		control bool
		want    int
	}{
		// The control's convert lists twice: the set, then the final listing. The row's lists the set
		// a second time, after the discarded listing.
		{name: "control", fixed: "s-m1", control: true, want: 2},
		{name: "the row", fixed: "s-m2", control: false, want: 3},
	} {
		subject := "notifications.agent." + run.fixed[2:]
		rig.publish(subject, 1)
		rig.fixed(run.fixed, subject, 0)
		id := rig.prepared(holdrecord.WorkflowCutover)
		code, _, stderr := rig.run(actionPrepare, "--hold", id)
		require.Zero(t, code, stderr)
		rig.closeWriter()
		rig.held(id)
		if run.control {
			code, _, stderr = rig.run(actionCreateBucket, "--hold", id)
			require.Zero(t, code, stderr)
		}
		listings, moved := 0, ""
		code, stderr = rig.convertWith(ctx, convertRule{noTermCheck: run.control, listed: func(ctx context.Context, names []string) {
			if listings++; listings == 1 {
				require.Contains(t, names, run.fixed, "%s: the first listing", run.name)
				moved = rig.metaLeaderMove(ctx)
			}
		}}, id)
		require.Zero(t, code, "%s: %s", run.name, stderr)
		require.NotEmpty(t, moved, "%s: the meta leader was not moved", run.name)
		require.Equal(t, run.want, listings, "%s: the listings convert made after the meta leader moved", run.name)
		require.Empty(t, rig.fixedNames(), "%s: a fixed name is left", run.name)
		require.Equal(t, "done", rig.lastEntry(id, actionConvert).State, run.name)
		rig.censusDone(id)
		require.NoError(t, rig.release(id), run.name)
		rig.connectWriter()
	}
}
