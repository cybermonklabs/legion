package holdtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// Disks are compute.disks.get over the Kube's volumes: each claim's disk by its volume handle's
// name, pvc-envoy-nats-<i>, with a numeric id a test can change, as a disk deleted and made again
// under its old name gets a new id. A test wraps ID in writerhold.Disks.
type Disks struct {
	mu  sync.Mutex
	ids map[string]uint64
}

// NewDisks are the disks of n members' claims.
func NewDisks(n int) *Disks {
	d := &Disks{ids: map[string]uint64{}}
	for i := range n {
		d.ids["pvc-"+Pod(i)] = uint64(7000000000000000000 + i)
	}
	return d
}

// ID is disk name's id, or compute's 404 for a disk the hub has none of.
func (d *Disks) ID(name string) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, ok := d.ids[name]
	if !ok {
		return 0, fmt.Errorf("compute.disks.get %s: 404 notFound", name)
	}
	return id, nil
}

// Remake gives member i's claim's disk a new id, as a restore from a snapshot under the disk's old
// name does.
func (d *Disks) Remake(i int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids["pvc-"+Pod(i)] += 1000
}

// ConnectOperator connects to the hub as the operator, reconnecting as `envoy-infra hold` does, so
// each reconnect is decided again under the row's phase at that moment.
func (s *Stack) ConnectOperator(context.Context) (*nats.Conn, error) {
	return nats.Connect(s.Cluster.URL(), append(s.OperatorCredential(), nats.Name("envoy-infra hold"),
		nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond), nats.Timeout(10*time.Second))...)
}

// DialProbe dials url as the hold probe, once, and answers the dial's error: nil when the server
// admitted it, one wrapping nats.ErrAuthorization when it refused it.
func (s *Stack) DialProbe(_ context.Context, url string) error {
	nc, err := nats.Connect(url, s.ProbeCredential(), nats.Name("envoy-infra hold probe"), nats.NoReconnect(), nats.Timeout(10*time.Second))
	if err == nil {
		nc.Close()
	}
	return err
}

// ProtectedBuckets are the KV buckets whose streams the hold protects by name: the session
// consumers' record, Legion's agents, the DPoP proofs, the webhook redeliveries and each bucket a
// listener copies.
func ProtectedBuckets() []string {
	return append([]string{bus.SessionConsumersBucket, bus.LegionAgentBucket, bus.DPoPProofBucket, bus.WebhookRedeliveryBucket}, bus.CopiedBuckets()...)
}

// Prepare moves the released row to prepare under a new hold id and starts the hold's journal for
// w, as `envoy-infra hold prepare` does, and returns the id.
func (s *Stack) Prepare(t testing.TB, w holdrecord.Workflow) string {
	t.Helper()
	id := nuid.Next()
	ctx := context.Background()
	row, err := s.Records.Row(ctx)
	if err != nil {
		t.Fatalf("holdtest: read the row: %v", err)
	}
	if _, err := s.Records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhasePrepare, id, "holdtest prepare"); err != nil {
		t.Fatalf("holdtest: prepare hold %s: %v", id, err)
	}
	if err := s.Records.StartJournal(ctx, id, w, nil); err != nil {
		t.Fatalf("holdtest: start hold %s's journal: %v", id, err)
	}
	return id
}

// HoldWithoutTheSteps moves the prepared hold id to held the way `hold set` writes its journal (its
// intent with the row prepare, the move, its done entry), without the steps it runs against the
// hub, for a test of what comes after a set hold rather than the steps themselves.
func (s *Stack) HoldWithoutTheSteps(t testing.TB, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandSet}, nil); err != nil {
		t.Fatalf("holdtest: hold set's intent: %v", err)
	}
	row, err := s.Records.Row(ctx)
	if err != nil {
		t.Fatalf("holdtest: read the row: %v", err)
	}
	if _, err := s.Records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseHeld, id, "holdtest held"); err != nil {
		t.Fatalf("holdtest: move hold %s to held: %v", id, err)
	}
	if err := s.Records.AppendDone(ctx, id, holdrecord.CommandSet, nil); err != nil {
		t.Fatalf("holdtest: hold set's done entry: %v", err)
	}
}

// Release releases the held hold id as `hold release` does: its admission, then the row moved to
// released.
func (s *Stack) Release(t testing.TB, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandRelease}, nil); err != nil {
		t.Fatalf("holdtest: release hold %s: %v", id, err)
	}
	row, err := s.Records.Row(ctx)
	if err != nil {
		t.Fatalf("holdtest: read the row: %v", err)
	}
	if _, err := s.Records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseReleased, id, "holdtest release"); err != nil {
		t.Fatalf("holdtest: move hold %s to released: %v", id, err)
	}
}

// SeedProtectedStreams makes every stream the build declares protected, with the build's own
// configuration at three copies (streamset.EnsureStreams, as `envoy-infra streams update` makes
// them), and each other protected bucket the declared set does not name, as the hub holds them:
// the operator makes them under a hold of its own (the held grant is the one that creates a
// protected stream), which is then released with nothing else changed.
func (s *Stack) SeedProtectedStreams(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	id := s.Prepare(t, holdrecord.WorkflowStreamsUpdate)
	s.HoldWithoutTheSteps(t, id)
	nc, err := s.ConnectOperator(ctx)
	if err != nil {
		t.Fatalf("holdtest: connect as the operator: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	// The whole seed's own minutes: an R3 create on a loaded box can take past jetstream's 5 s
	// default.
	seedCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// The copied buckets first, with their own configuration, as `envoy-infra buckets create`
	// makes them: streamset.EnsureStreams makes no copied bucket.
	legacy, err := nc.JetStream(nats.MaxWait(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, bucket := range bus.CopiedBuckets() {
		if _, err := bus.CreateCopiedBucket(seedCtx, nc, legacy, bucket, 3); err != nil {
			t.Fatalf("holdtest: create copied bucket %s: %v", bucket, err)
		}
	}
	if err := streamset.EnsureStreams(seedCtx, nc, streamset.Settings{Replicas: 3}); err != nil {
		t.Fatalf("holdtest: make the declared streams: %v", err)
	}
	for _, bucket := range ProtectedBuckets() {
		if _, err := js.Stream(seedCtx, "KV_"+bucket); err == nil {
			continue
		}
		if _, err := js.CreateKeyValue(seedCtx, jetstream.KeyValueConfig{Bucket: bucket, Replicas: 3}); err != nil {
			t.Fatalf("holdtest: create bucket %s: %v", bucket, err)
		}
	}
	nc.Close()
	if _, err := s.Records.Admit(ctx, id, holdrecord.Request{Command: "streams update"}, nil); err != nil {
		t.Fatalf("holdtest: the seed's streams update: %v", err)
	}
	if err := s.Records.AppendDone(ctx, id, "streams update", nil); err != nil {
		t.Fatalf("holdtest: the seed's streams update: %v", err)
	}
	s.Release(t, id)
}

// jsz is the part of a server's /jsz?accounts=true&streams=true the stack reads: each stream it
// holds, with the leader of its group as that server sees it.
type jsz struct {
	AccountDetails []struct {
		Streams []struct {
			Name    string `json:"name"`
			Cluster struct {
				Leader string `json:"leader"`
			} `json:"cluster"`
		} `json:"stream_detail"`
	} `json:"account_details"`
}

// AwaitStreamLeaders waits until every member sees a leader for every stream of names: after a
// hold's restarts the R3 groups elect their leaders again, and a hold whose preflight reads a
// stream with none yet is UNKNOWN, as it must be, so a test waits before its next hold.
func (s *Stack) AwaitStreamLeaders(t *testing.T, names []string) {
	t.Helper()
	testwait.Eventually(t, "every stream named has a leader on every member", func() bool {
		for i := range s.Cluster.Servers {
			address, stop, err := s.Kube.Forward(context.Background(), Namespace, Pod(i), 8222)
			if err != nil {
				return false
			}
			resp, err := http.Get("http://" + address + "/jsz?accounts=true&streams=true")
			stop()
			if err != nil {
				return false
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			var read jsz
			if err != nil || json.Unmarshal(body, &read) != nil {
				return false
			}
			led := map[string]bool{}
			for _, account := range read.AccountDetails {
				for _, stream := range account.Streams {
					led[stream.Name] = stream.Cluster.Leader != ""
				}
			}
			for _, name := range names {
				if !led[name] {
					return false
				}
			}
		}
		return true
	})
}
