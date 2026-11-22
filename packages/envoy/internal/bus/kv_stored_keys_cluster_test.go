package bus

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// On a real three-member cluster, with the bucket's stream on all three, StoredKeys takes the
// list the stream's leader answers, whichever member the request reaches, and refuses one a member
// answers while the stream has no leader. The bucket is opened through a member that is not the
// stream's leader, so the list is asked through that member. With the leader running, the leader
// answers, naming itself, and the list holds every key. Once the two other members stop, the
// member left has no leader, and nats-server lets it answer from its own copy
// (jetstream_api.go:2100-2104), naming no leader: that list is refused (ErrKeyListBehind), however
// whole the member's copy is. Until the member sees that it has lost its leader it does not answer
// at all, or answers that the cluster is not available, and no answer before or after is taken as
// a list.
func TestStoredKeysOnAClusterTakesTheLeadersListAndRefusesAMemberWithNoLeader(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	cluster.AwaitReady(t)
	config := func() *nats.KeyValueConfig {
		return &nats.KeyValueConfig{Bucket: "stored-keys-cluster", Replicas: 3, Storage: nats.MemoryStorage}
	}
	client, err := Connect(cluster.URLs())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := EnsureKeyValue(t.Context(), client.Conn, js, config())
	if err != nil {
		t.Fatalf("create the three-copy bucket: %v", err)
	}
	for _, key := range []string{"a", "b"} {
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	barrier, err := kv.WriteBarrier(context.Background())
	if err != nil {
		t.Fatalf("write the barrier: %v", err)
	}
	info, err := js.StreamInfo("KV_" + config().Bucket)
	if err != nil || info.Cluster == nil || cluster.Member(info.Cluster.Leader) < 0 {
		t.Fatalf("read the bucket's stream for its leader: %+v, %v", info, err)
	}
	leader := cluster.Member(info.Cluster.Leader)
	member := (leader + 1) % len(cluster.Servers)
	memberConn, memberJS := memberJetStream(t, cluster, member)
	memberKV, err := EnsureKeyValue(t.Context(), memberConn, memberJS, config())
	if err != nil {
		t.Fatalf("open the bucket through member %s: %v", cluster.Name(member), err)
	}

	keys, err := memberKV.StoredKeys(context.Background(), barrier)
	slices.Sort(keys)
	if err != nil || !slices.Equal(keys, []string{"a", "b"}) {
		t.Fatalf("StoredKeys through member %s with %s leading = %v, %v; want [a b], the leader's list", cluster.Name(member), cluster.Name(leader), keys, err)
	}

	for i := range cluster.Servers {
		if i != member {
			cluster.Servers[i].Stop(t)
		}
	}
	var answers []string
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		keys, err := memberKV.StoredKeys(ctx, barrier)
		cancel()
		if err == nil {
			t.Fatalf("StoredKeys through member %s, the one left of three, took the list %v; want no list from a member with no leader (answers before it: %q)", cluster.Name(member), keys, answers)
		}
		if errors.Is(err, ErrKeyListBehind) {
			if !strings.Contains(err.Error(), "names no leader") {
				t.Fatalf("the member's list was refused for %v, want for naming no leader", err)
			}
			t.Logf("refused after %d earlier answers %q: %v", len(answers), answers, err)
			return
		}
		answers = append(answers, err.Error())
		if time.Now().After(deadline) {
			t.Fatalf("StoredKeys through member %s, alone for 60 s, never answered that the member names no leader; its answers: %q", cluster.Name(member), answers)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
