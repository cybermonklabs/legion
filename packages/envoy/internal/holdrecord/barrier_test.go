package holdrecord_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/testnats"
)

// lastBySubject is the bucket stream's last message on subject, read with STREAM.MSG.GET, which
// the stream's leader answers (a direct get may be answered by a replica that has not applied the
// write yet). It answers "" and a nil error when the stream holds no message on the subject.
func lastBySubject(t *testing.T, conn *natsgo.Conn, stream, subject string) (string, error) {
	t.Helper()
	request, err := json.Marshal(map[string]string{"last_by_subj": subject})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := conn.Request("$JS.API.STREAM.MSG.GET."+stream, request, 10*time.Second)
	if err != nil {
		return "", err
	}
	var answer struct {
		Message *struct {
			Data []byte `json:"data"`
		} `json:"message"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &answer); err != nil {
		t.Fatalf("decode the STREAM.MSG.GET answer %q: %v", reply.Data, err)
	}
	if answer.Error != nil {
		if answer.Error.Code == 404 {
			return "", nil
		}
		t.Fatalf("STREAM.MSG.GET of %s on %s: %d %s", subject, stream, answer.Error.Code, answer.Error.Message)
	}
	return string(answer.Message.Data), nil
}

// The barrier is an acknowledged put to the bucket's own barrier key: on a three-server cluster
// Barrier returns once the bucket's three-copy stream has stored the put, so its leader holds it
// for the next read, and a barrier on a bucket no stream holds fails, since no stream acknowledges
// it. A barrier that published without waiting for its acknowledgement would return nil there,
// and fail this test. A bucket or hold id that is not one subject token, whose put the held grant
// would not cover, is refused before anything is sent.
func TestTheBarrierIsAnAcknowledgedPut(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	cluster.AwaitReady(t)
	conn := testnats.Connect(t, cluster.URL())
	t.Cleanup(conn.Close)
	cluster.CreateKeyValue(t, conn, &natsgo.KeyValueConfig{Bucket: "envoy_sessions", Replicas: 3, Storage: natsgo.FileStorage})
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if got, err := lastBySubject(t, conn, "KV_envoy_sessions", "$KV.envoy_sessions.hold.barrier.h-1"); err != nil || got != "" {
		t.Fatalf("before the barrier the stream holds %q, %v on its barrier key", got, err)
	}
	if err := holdrecord.Barrier(ctx, js, "envoy_sessions", "h-1"); err != nil {
		t.Fatalf("Barrier on envoy_sessions: %v", err)
	}
	if got, err := lastBySubject(t, conn, "KV_envoy_sessions", "$KV.envoy_sessions.hold.barrier.h-1"); err != nil || got != "h-1" {
		t.Fatalf("right after Barrier returned, the leader answers %q, %v on the barrier key, want the put", got, err)
	}
	if err := holdrecord.Barrier(ctx, js, "envoy_absent", "h-1"); err == nil {
		t.Fatal("Barrier on a bucket no stream holds answered no error, so it did not wait for an acknowledgement")
	}
	for _, bad := range [][2]string{{"envoy.sessions", "h-1"}, {"envoy_sessions", "h.1"}, {"envoy_sessions", "*"}, {"", "h-1"}, {"envoy_sessions", ""}} {
		if err := holdrecord.Barrier(ctx, js, bad[0], bad[1]); err == nil {
			t.Errorf("Barrier on bucket %q, hold id %q answered no error", bad[0], bad[1])
		}
	}
}
