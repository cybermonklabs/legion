package kvrecords

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
)

// lineFields is every JSON field of line, read from line's own tags, so a field added to the wire
// struct is checked for presence too. It fails when line gains a field that lineFields leaves out,
// or lineFields names one line does not have.
func TestLineFieldsAreTheWireStructsFields(t *testing.T) {
	kind := reflect.TypeFor[line]()
	tags := make([]string, 0, kind.NumField())
	for index := range kind.NumField() {
		name, _, _ := strings.Cut(kind.Field(index).Tag.Get("json"), ",")
		require.NotEmpty(t, name, "line's field %s has no JSON name", kind.Field(index).Name)
		tags = append(tags, name)
	}
	require.Equal(t, tags, lineFields, "lineFields is not line's JSON fields, in order")
}

// fakeInfo is one answer of a fake stream's info: its last_seq, and when the stream was made.
type fakeInfo struct {
	last    uint64
	created time.Time
}

// fakeStream answers the STREAM.INFO and STREAM.MSG.GET requests of KV_<bucket> on a server with no
// JetStream: infos is what each info request answers in turn, the last one again once they run
// out, and every sequence up to the highest is a message of the key k<seq>.
func fakeStream(t *testing.T, nc *nats.Conn, bucket string, infos ...fakeInfo) {
	t.Helper()
	var asked atomic.Int64
	infoSub, err := nc.Subscribe("$JS.API.STREAM.INFO.KV_"+bucket, func(msg *nats.Msg) {
		info := infos[min(int(asked.Add(1))-1, len(infos)-1)]
		answer, _ := json.Marshal(map[string]any{"created": info.created,
			"state": map[string]uint64{"first_seq": 1, "last_seq": info.last}})
		_ = msg.Respond(answer)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = infoSub.Unsubscribe() })
	getSub, err := nc.Subscribe("$JS.API.STREAM.MSG.GET.KV_"+bucket, func(msg *nats.Msg) {
		var body struct {
			Seq uint64 `json:"seq"`
		}
		_ = json.Unmarshal(msg.Data, &body)
		answer, _ := json.Marshal(map[string]any{"message": map[string]any{
			"subject": fmt.Sprintf("$KV.%s.k%d", bucket, body.Seq), "seq": body.Seq, "data": []byte("v"),
			"time": time.Unix(int64(body.Seq), 0).UTC(),
		}})
		_ = msg.Respond(answer)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = getSub.Unsubscribe() })
	require.NoError(t, nc.Flush())
}

// A walk whose stream's last_seq fell between its info reads fails, naming the fall, and writes
// nothing: a stream made again under the bucket's name starts its sequences over, and a copy
// walked across it is of no one stream. The stream here is a responder on a server with no
// JetStream, whose info answers last_seq 3, then 1, so the fall comes at the first pass's end.
//
// Run by hand, the control is the walk at c8794950fd, whose `last <= LastSeq` takes a fall as the
// end of the walk: it writes a copy of three records at last_seq 3.
func TestAWalkWhoseLastSeqFellFails(t *testing.T) {
	_, url := testnats.StartConfig(t, "", nil, true)
	nc, err := nats.Connect(url, nats.Timeout(5*time.Second), nats.NoReconnect())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	const bucket = "walk_falls"
	made := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	fakeStream(t, nc, bucket, fakeInfo{3, made}, fakeInfo{1, made})
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "copy")
	manifest, err := Write(t.Context(), nc, bucket, dir, key)
	require.Error(t, err, "the walk wrote a copy of %d records at last_seq %d across a fall", manifest.Records, manifest.LastSeq)
	require.ErrorContains(t, err, "last_seq fell from 3 to 1")
	_, statErr := os.Stat(dir)
	require.True(t, errors.Is(statErr, fs.ErrNotExist), "the walk across a fall left a copy")
	require.False(t, slices.Contains(lineFields, ""))
}

// A walk across a stream deleted and made again between two of its info reads fails, naming the
// stream and that it was made again, and writes nothing, though the new stream's last_seq grew
// past the old one's so the fall check cannot see it: every info read's created time must be the
// first's. A real server cannot be made to place the delete and the create between two of the
// walk's reads, since JetStream answers the walk's requests itself, so the stream is a responder on
// a server with no JetStream, whose info answers last_seq 3, made at one time, then last_seq 5,
// made at a later one. A real server does give a stream made again a new created time: the first
// subtest measures it.
//
// Run by hand, the control is the walk at 36406c1949, which reads no created time: it walks on to
// the new stream's last_seq and writes a copy of five records at last_seq 5.
func TestAWalkAcrossAStreamMadeAgainFails(t *testing.T) {
	t.Run("a real server gives a stream made again a new created time", func(t *testing.T) {
		server, _ := testnats.Start(t)
		nc := testnats.Connect(t, server.URL())
		t.Cleanup(nc.Close)
		js, err := nc.JetStream()
		require.NoError(t, err)
		created := func() time.Time {
			_, err := js.AddStream(&nats.StreamConfig{Name: "KV_made_again", Subjects: []string{"$KV.made_again.>"}})
			require.NoError(t, err)
			info, err := js.StreamInfo("KV_made_again")
			require.NoError(t, err)
			return info.Created
		}
		first := created()
		require.NoError(t, js.DeleteStream("KV_made_again"))
		second := created()
		require.False(t, first.IsZero())
		require.True(t, second.After(first), "the stream made again was created at %s, the first at %s", second, first)
	})

	_, url := testnats.StartConfig(t, "", nil, true)
	nc, err := nats.Connect(url, nats.Timeout(5*time.Second), nats.NoReconnect())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	const bucket = "walk_made_again"
	made := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	fakeStream(t, nc, bucket, fakeInfo{3, made}, fakeInfo{5, made.Add(time.Second)})
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "copy")
	manifest, err := Write(t.Context(), nc, bucket, dir, key)
	require.Error(t, err, "the walk wrote a copy of %d records at last_seq %d across a stream made again", manifest.Records, manifest.LastSeq)
	require.ErrorContains(t, err, "KV_"+bucket)
	require.ErrorContains(t, err, "was made again")
	_, statErr := os.Stat(dir)
	require.True(t, errors.Is(statErr, fs.ErrNotExist), "the walk across a stream made again left a copy")
}
