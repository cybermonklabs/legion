package buckets

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// snapshotOf takes a snapshot of stream as JetStream's API gives one, through conn, an operator's
// connection: the stream's config and state, and its chunks joined.
func snapshotOf(t *testing.T, conn *natsgo.Conn, stream string) (json.RawMessage, json.RawMessage, []byte) {
	t.Helper()
	inbox := natsgo.NewInbox()
	chunks, err := conn.SubscribeSync(inbox)
	if err != nil {
		t.Fatalf("subscribe for the snapshot: %v", err)
	}
	defer func() { _ = chunks.Unsubscribe() }()
	request, _ := json.Marshal(map[string]any{"deliver_subject": inbox, "no_consumers": true, "chunk_size": 64 * 1024})
	reply, err := conn.Request("$JS.API.STREAM.SNAPSHOT."+stream, request, 5*time.Second)
	if err != nil {
		t.Fatalf("snapshot %s: %v", stream, err)
	}
	var answer struct {
		Config json.RawMessage `json:"config"`
		State  json.RawMessage `json:"state"`
		Error  *struct {
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &answer); err != nil || answer.Error != nil {
		t.Fatalf("snapshot %s: %s (%v)", stream, reply.Data, err)
	}
	var data []byte
	for {
		chunk, err := chunks.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("read the snapshot's chunks: %v", err)
		}
		if len(chunk.Data) == 0 {
			// nats-server ends a snapshot with an empty message, headed 204 once a full chunk
			// went before it, unheaded when the whole snapshot fit in its last, partial chunk,
			// and headed 408 or 500 when it failed (stream.go streamSnapshot).
			if status := chunk.Header.Get("Status"); status != "" && status != "204" {
				t.Fatalf("the snapshot ended with status %q", status)
			}
			return answer.Config, answer.State, data
		}
		data = append(data, chunk.Data...)
		if chunk.Reply != "" {
			if err := conn.Publish(chunk.Reply, nil); err != nil {
				t.Fatalf("acknowledge a snapshot chunk: %v", err)
			}
		}
	}
}

// restoreAs restores stream from config, state and data through conn as `nats stream restore`
// does: the restore request, each chunk sent to the subject it answers, and an empty message to
// end. It answers the first refusal or missing answer.
func restoreAs(conn *natsgo.Conn, stream string, config, state json.RawMessage, data []byte) error {
	request, _ := json.Marshal(map[string]json.RawMessage{"config": config, "state": state})
	reply, err := conn.Request("$JS.API.STREAM.RESTORE."+stream, request, 5*time.Second)
	if err != nil {
		return fmt.Errorf("restore request: %w", err)
	}
	var answer struct {
		DeliverSubject string `json:"deliver_subject"`
		Error          *struct {
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &answer); err != nil || answer.Error != nil {
		return fmt.Errorf("restore request answered %s (%v)", reply.Data, err)
	}
	for start := 0; start < len(data); start += 64 * 1024 {
		chunk := data[start:min(start+64*1024, len(data))]
		if _, err := conn.Request(answer.DeliverSubject, chunk, 5*time.Second); err != nil {
			return fmt.Errorf("send a chunk to %s: %w", answer.DeliverSubject, err)
		}
	}
	done, err := conn.Request(answer.DeliverSubject, nil, 30*time.Second)
	if err != nil {
		return fmt.Errorf("end the restore: %w", err)
	}
	if strings.Contains(string(done.Data), `"error"`) {
		return fmt.Errorf("the restore answered %s", done.Data)
	}
	return nil
}

// The restore that follows `check` - one copied bucket restored from its snapshot as `nats stream
// restore --inbox-prefix=_INBOX_envoy_buckets` restores it - runs as a user granted exactly
// Grant(). The control drops the subject the restore's chunks go to
// ($JS.SNAPSHOT.RESTORE.KV_<bucket>.*) and the restore is refused. At 7b262c2e0a the grant named
// neither restore subject.
func TestTheRestoreRunsUnderExactlyTheCommandsGrant(t *testing.T) {
	publish, subscribe := Grant()
	stream := "KV_" + bus.SessionBucket
	chunks := "$JS.SNAPSHOT.RESTORE." + stream + ".*"
	for _, c := range []struct {
		name    string
		publish []string
		refused bool
	}{
		{"the whole grant", publish, false},
		{"control without " + chunks, slices.DeleteFunc(slices.Clone(publish), func(subject string) bool { return subject == chunks }), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			adminSeed, admin := testnats.User(t)
			bucketsSeed, buckets := testnats.User(t)
			_, uri := testnats.StartConfig(t, grantConfig(t, buckets, c.publish, subscribe, admin), []string{"-js"}, false)
			operator, err := testnats.ConnectSeed(t, uri, adminSeed)
			if err != nil {
				t.Fatalf("connect as the operator: %v", err)
			}
			defer operator.Close()
			js, err := operator.JetStream()
			if err != nil {
				t.Fatalf("jetstream: %v", err)
			}
			bucketConfig, _ := bus.CopiedBucketConfig(bus.SessionBucket, 1)
			kv, err := js.CreateKeyValue(&bucketConfig)
			if err != nil {
				t.Fatalf("create the bucket: %v", err)
			}
			if _, err := kv.Put("ses_backed_up", []byte(`{"title":"backed up"}`)); err != nil {
				t.Fatalf("put a record: %v", err)
			}
			streamConfig, state, data := snapshotOf(t, operator, stream)
			if err := js.DeleteStream(stream); err != nil {
				t.Fatalf("delete the bucket before its restore: %v", err)
			}

			user, err := testnats.ConnectSeed(t, uri, bucketsSeed, natsgo.CustomInboxPrefix(InboxPrefix))
			if err != nil {
				t.Fatalf("connect as the buckets user: %v", err)
			}
			defer user.Close()
			err = restoreAs(user, stream, streamConfig, state, data)
			if c.refused {
				if err == nil {
					t.Fatalf("the restore as a user granted Grant() without %s succeeded, want it refused", chunks)
				}
				return
			}
			if err != nil {
				t.Fatalf("the restore as a user granted exactly Grant(): %v", err)
			}
			restored, err := js.KeyValue(bus.SessionBucket)
			if err != nil {
				t.Fatalf("the restored bucket: %v", err)
			}
			if _, err := restored.Get("ses_backed_up"); err != nil {
				t.Fatalf("the restored bucket's record: %v", err)
			}
		})
	}
}
