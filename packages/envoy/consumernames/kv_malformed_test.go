package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// copyUnder copies the last message on subject from in stream, its bytes and its headers but its
// Nats-Expected-* ones, onto key of the selection bucket, as a principal holding a direct get of
// from can, and returns the sequence the copy was stored at.
func copyUnder(t *testing.T, conn *natsgo.Conn, stream, from, key string) uint64 {
	t.Helper()
	request, err := json.Marshal(map[string]string{"last_by_subj": from})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := conn.Request("$JS.API.DIRECT.GET."+stream, request, 5*time.Second)
	if err != nil || stored.Header.Get("Status") != "" {
		t.Fatalf("the direct get of %s from %s = %v (status %q)", from, stream, err, stored.Header.Get("Status"))
	}
	header := natsgo.Header{}
	for name, values := range stored.Header {
		if !strings.HasPrefix(name, "Nats-Expected-") {
			header[name] = values
		}
	}
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	ack, err := js.PublishMsg(&natsgo.Msg{Subject: "$KV." + SessionBucket + "." + key, Header: header, Data: stored.Data})
	if err != nil {
		t.Fatalf("store the copy under %s: %v", key, err)
	}
	return ack.Sequence
}

// A key of the bucket holding a value that is not its own record is refused for that key, naming
// the key and the value's sequence, and the sentinel's key holding one is a missing sentinel: a
// well-formed record of key X copied whole under key Y, and the server's STREAM.INFO answer stored
// under a session key and under the sentinel's through a request's reply subject. Ensure creates
// nothing for the refused key, and another key is served. The control of each is today's decode,
// json.Unmarshal into a Record or a StoreState, which takes every one of them.
func TestAKeyHoldingNoRecordOfItsOwnIsRefusedNamingTheKeyAndItsSequence(t *testing.T) {
	_, uri := testnats.Start(t)
	live := newLiveStore(t, uri, &StoreState{Cutover: CutoverDone, Boundary: 1})
	ctx := context.Background()
	names := live.names("owner")
	request := func(logical string) CreateRequest {
		return CreateRequest{Create: live.family.create(logical), First: func(context.Context) (Start, error) { return Start{Seq: 1}, nil }}
	}
	owner, _, err := names.Ensure(ctx, "ses_owner", request("ses_owner"))
	if err != nil {
		t.Fatal(err)
	}

	copied := copyUnder(t, live.conn, "KV_"+SessionBucket, "$KV."+SessionBucket+".ses_owner", "ses_copy")
	if err := live.conn.PublishRequest("$JS.API.STREAM.INFO.KV_"+SessionBucket, "$KV."+SessionBucket+".ses_info", nil); err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]string{"another key's record": "ses_copy", "a STREAM.INFO answer": "ses_info"} {
		t.Run(name, func(t *testing.T) {
			var data []byte
			var revision uint64
			for deadline := time.Now().Add(10 * time.Second); ; {
				if data, revision, err = live.store.last(ctx, key); err == nil || time.Now().After(deadline) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err != nil {
				t.Fatalf("the key %s never held the value the row put there: %v", key, err)
			}
			var lenient Record
			if err := json.Unmarshal(data, &lenient); err != nil {
				t.Fatalf("control: today's decode of %s's value = %v, want it taken", key, err)
			}
			if key == "ses_copy" && (lenient.Current != owner.Record.Current || revision != copied) {
				t.Fatalf("control: today's decode of the copy = %+v at %d, want ses_owner's record naming %s at %d", lenient, revision, owner.Record.Current, copied)
			}
			_, _, err := live.store.Read(ctx, key)
			if !errors.Is(err, ErrMalformedRecord) || !strings.Contains(err.Error(), "key "+key+" ") || !strings.Contains(err.Error(), "sequence "+strconv.FormatUint(revision, 10)) {
				t.Fatalf("Read of %s = %v, want ErrMalformedRecord naming the key and its sequence %d", key, err, revision)
			}
			if sel, created, err := names.Ensure(ctx, key, request(key)); !errors.Is(err, ErrMalformedRecord) || created {
				t.Fatalf("Ensure of %s = %+v, created %v, %v; want ErrMalformedRecord and nothing created", key, sel, created, err)
			}
		})
	}
	if _, created, err := names.Ensure(ctx, "ses_beside", request("ses_beside")); err != nil || !created {
		t.Fatalf("Ensure of another key = created %v, %v; want it served", created, err)
	}
	stream, err := live.js.Stream(ctx, testStream)
	if err != nil {
		t.Fatal(err)
	}
	for name := range stream.ConsumerNames(ctx).Name() {
		if info, err := live.family.Info(ctx, name); err == nil {
			if logical := info.Config.Metadata[LogicalMetadataKey]; logical == "ses_copy" || logical == "ses_info" {
				t.Fatalf("the stream holds %s for the refused key %s", name, logical)
			}
		}
	}

	t.Run("a STREAM.INFO answer under the sentinel's key", func(t *testing.T) {
		if err := live.conn.PublishRequest("$JS.API.STREAM.INFO.KV_"+SessionBucket, "$KV."+SessionBucket+"."+SentinelKey, nil); err != nil {
			t.Fatal(err)
		}
		var data []byte
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if data, _, err = live.store.last(ctx, SentinelKey); err == nil && strings.Contains(string(data), "stream_info_response") {
				break
			}
		}
		var lenient StoreState
		if err := json.Unmarshal(data, &lenient); err != nil || lenient.Cutover != "" {
			t.Fatalf("control: today's decode of the answer = %+v, %v; want a sentinel with no marker, taken", lenient, err)
		}
		if _, err := live.store.State(ctx); !errors.Is(err, ErrNoSentinel) || !strings.Contains(err.Error(), SentinelKey) {
			t.Fatalf("State = %v, want ErrNoSentinel naming %s", err, SentinelKey)
		}
	})
}
