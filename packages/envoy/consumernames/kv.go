package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// SessionBucket is the KV bucket that holds the session family's selection records, one key per
// session id, and its sentinel (SentinelKey). No running writer creates it: the operator's
// create-bucket does, before the cutover, and the listener opens it by lookup only.
const SessionBucket = "envoy_session_consumers"

// SentinelKey is the key of a KV store's sentinel (StoreState). It holds a '.', which no logical
// name holds (CheckKey), so it is a system key. create-bucket writes it when it first creates the
// bucket; while it is absent every call is refused (ErrNoSentinel), so a bucket recreated empty
// never makes a lost record look like a new key.
const SentinelKey = "envoy.bucket"

// SessionBucketConfig is the configuration create-bucket creates SessionBucket with, at replicas
// copies: one value a key, kept for good (no age, no size limit), on file storage.
func SessionBucketConfig(replicas int) jetstream.KeyValueConfig {
	return jetstream.KeyValueConfig{
		Bucket:      SessionBucket,
		Description: "Envoy session consumers' selection records: each session's current consumer name",
		History:     1,
		Storage:     jetstream.FileStorage,
		Replicas:    max(replicas, 1),
	}
}

// CreateSessionBucket creates SessionBucket at replicas copies (SessionBucketConfig) and writes
// its sentinel state, as the operator's create-bucket does before the cutover, and answers a
// KVStore of it on conn. The sentinel names SessionFamily: state's Family is set to it when empty,
// and another family is refused, since this bucket is the session family's alone. A bucket that
// exists already is refused, since no writer may adopt a bucket another may own. The sentinel is
// written only once the new stream answers, within ctx's deadline, else createBucketWait
// (createBucket).
func CreateSessionBucket(ctx context.Context, conn *nats.Conn, replicas int, state StoreState) (*KVStore, error) {
	switch state.Family {
	case "":
		state.Family = SessionFamily
	case SessionFamily:
	default:
		return nil, fmt.Errorf("consumer names: %s is the %s family's bucket, not %q's", SessionBucket, SessionFamily, state.Family)
	}
	return createBucket(ctx, conn, SessionBucketConfig(replicas), state)
}

// createBucketWait bounds a create's wait for its new stream to answer when ctx carries no
// deadline.
const createBucketWait = 30 * time.Second

// createBucket is CreateSessionBucket of the bucket config describes. On a cluster the new stream
// answers nobody for a moment after its create: until it has a leader, and its interest has
// reached the member conn is on, that member answers a read that the stream does not exist, or
// finds no responder for a read or a write. Each is ErrNoStore, which for a bucket just created
// means only "not yet", so the sentinel's leader read is tried again until the stream answers it
// (no record, the key never written), and the sentinel's write until a responder takes it. Past
// the bound the create fails naming the bucket; the bucket stays, with no sentinel, which every
// call refuses (ErrNoSentinel) until a create-bucket run writes one.
func createBucket(ctx context.Context, conn *nats.Conn, config jetstream.KeyValueConfig, state StoreState) (*KVStore, error) {
	js, err := jetstream.New(conn)
	if err != nil {
		return nil, err
	}
	if _, err := js.CreateKeyValue(ctx, config); err != nil {
		return nil, fmt.Errorf("consumer names: create %s: %w", config.Bucket, err)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, createBucketWait)
		defer cancel()
	}
	store := NewKVStore(config.Bucket, ConnRequester{conn})
	_, _, err = untilAnswered(ctx, config.Bucket, func() ([]byte, uint64, error) { return store.last(ctx, SentinelKey) })
	if err != nil && !errors.Is(err, ErrNoRecord) && !errors.Is(err, ErrRecordDeleted) {
		return nil, err
	}
	_, _, err = untilAnswered(ctx, config.Bucket, func() ([]byte, uint64, error) {
		revision, err := store.WriteSentinel(ctx, state, 0)
		return nil, revision, err
	})
	if err != nil {
		return nil, err
	}
	return store, nil
}

// untilAnswered runs attempt until it answers anything but ErrNoStore, and answers that; once ctx
// ends it answers the last ErrNoStore, naming the bucket.
func untilAnswered(ctx context.Context, bucket string, attempt func() ([]byte, uint64, error)) ([]byte, uint64, error) {
	for {
		data, revision, err := attempt()
		if !errors.Is(err, ErrNoStore) {
			return data, revision, err
		}
		if sleep(ctx, 50*time.Millisecond) != nil {
			return nil, 0, fmt.Errorf("consumer names: %s did not answer after its create: %w", bucket, err)
		}
	}
}

// Requester sends one request and answers its one reply: a JetStream API request or a publish to
// a stream that answers with its acknowledgement. A request the server refuses under the
// connection's permissions is the requester's own error, which KVStore returns wrapped.
type Requester interface {
	Request(ctx context.Context, msg *nats.Msg) (*nats.Msg, error)
}

// ConnRequester is a Requester over a nats.go connection.
type ConnRequester struct{ Conn *nats.Conn }

// Request implements Requester.
func (r ConnRequester) Request(ctx context.Context, msg *nats.Msg) (*nats.Msg, error) {
	return r.Conn.RequestMsgWithContext(ctx, msg)
}

// KVStore is a Store over a KV bucket's stream, through raw requests: each read takes the key's
// last message from the stream's leader (a stream message get, which a direct get is not), so a
// record written a moment ago is never read as absent, and a key whose last message is a delete
// or a purge marker is seen as one (ErrRecordDeleted) rather than as a key never written. Each
// write is a publish conditional on the key's last sequence, which is the record's revision.
type KVStore struct {
	bucket  string
	request Requester
	// timeout bounds a request whose ctx carries no deadline.
	timeout time.Duration
}

// NewKVStore is a KVStore over bucket, through request.
func NewKVStore(bucket string, request Requester) *KVStore {
	return &KVStore{bucket: bucket, request: request, timeout: 5 * time.Second}
}

// Bucket is the bucket the store reads.
func (s *KVStore) Bucket() string { return s.bucket }

// kvOperationHeader marks a KV delete or purge marker (nats.go kv.go).
const kvOperationHeader = "KV-Operation"

// State implements Store: it reads the sentinel.
func (s *KVStore) State(ctx context.Context) (StoreState, error) {
	state, _, err := s.sentinel(ctx)
	return state, err
}

// sentinel reads the sentinel and its revision. A value under SentinelKey that is not the
// sentinel (decodeSentinel) is a missing sentinel, naming the key and the value's sequence.
func (s *KVStore) sentinel(ctx context.Context) (StoreState, uint64, error) {
	data, revision, err := s.last(ctx, SentinelKey)
	if errors.Is(err, ErrNoRecord) || errors.Is(err, ErrRecordDeleted) {
		return StoreState{}, 0, fmt.Errorf("%w: the bucket %s holds no %s", ErrNoSentinel, s.bucket, SentinelKey)
	}
	if err != nil {
		return StoreState{}, 0, err
	}
	state, err := decodeSentinel(SentinelKey, data)
	if err != nil {
		return StoreState{}, 0, fmt.Errorf("%w: the bucket %s's %s at sequence %d holds no sentinel (%w)", ErrNoSentinel, s.bucket, SentinelKey, revision, err)
	}
	return state, revision, nil
}

// Read implements Store. A value under logical that is not logical's record (decodeRecord) is
// ErrMalformedRecord, naming the key and the value's sequence.
func (s *KVStore) Read(ctx context.Context, logical string) (Record, uint64, error) {
	data, revision, err := s.last(ctx, logical)
	if err != nil {
		return Record{}, 0, err
	}
	record, err := decodeRecord(logical, data)
	if err != nil {
		return Record{}, 0, fmt.Errorf("consumer names: the key %s of %s at sequence %d: %w", logical, s.bucket, revision, err)
	}
	return record, revision, nil
}

// Write implements Store. It writes the record under its own key (Record.Key).
func (s *KVStore) Write(ctx context.Context, logical string, record Record, revision uint64) (uint64, error) {
	record.Key = logical
	data, err := json.Marshal(record)
	if err != nil {
		return 0, err
	}
	return s.put(ctx, logical, data, revision)
}

// WriteSentinel writes the sentinel, under its own key (StoreState.Key), if its revision is still
// revision (0: never written): the operator's create-bucket and replay write it, and a test of a
// family writes it as they do.
func (s *KVStore) WriteSentinel(ctx context.Context, state StoreState, revision uint64) (uint64, error) {
	state.Key = SentinelKey
	data, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	return s.put(ctx, SentinelKey, data, revision)
}

// EndReplay clears replay_from and the orphan list from the sentinel when none of the listed
// orphans is in held, the names the stream's consumers carry now, and reports whether replay mode
// is over. The listener's sweep calls it with the consumers it listed.
func (s *KVStore) EndReplay(ctx context.Context, held func(name string) bool) (bool, error) {
	state, revision, err := s.sentinel(ctx)
	if err != nil {
		return false, err
	}
	if state.ReplayFrom == 0 {
		return true, nil
	}
	for _, orphan := range state.Orphans {
		if held(orphan) {
			return false, nil
		}
	}
	state.ReplayFrom, state.Orphans = 0, nil
	if _, err := s.WriteSentinel(ctx, state, revision); err != nil {
		return false, err
	}
	return true, nil
}

func (s *KVStore) subject(key string) string { return "$KV." + s.bucket + "." + key }

func (s *KVStore) stream() string { return "KV_" + s.bucket }

func (s *KVStore) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.timeout)
}

// last reads key's last message from the leader of the bucket's stream.
func (s *KVStore) last(ctx context.Context, key string) ([]byte, uint64, error) {
	ctx, cancel := s.bound(ctx)
	defer cancel()
	body, err := json.Marshal(struct {
		LastBySubject string `json:"last_by_subj"`
	}{s.subject(key)})
	if err != nil {
		return nil, 0, err
	}
	reply, err := s.request.Request(ctx, &nats.Msg{Subject: "$JS.API.STREAM.MSG.GET." + s.stream(), Data: body})
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) {
			return nil, 0, fmt.Errorf("%w: %s (%w)", ErrNoStore, s.stream(), err)
		}
		return nil, 0, fmt.Errorf("consumer names: read %s from %s: %w", key, s.stream(), err)
	}
	var response struct {
		Message *struct {
			Seq    uint64 `json:"seq"`
			Header []byte `json:"hdrs"`
			Data   []byte `json:"data"`
		} `json:"message"`
		Error *jetstream.APIError `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &response); err != nil {
		return nil, 0, fmt.Errorf("consumer names: read the answer for %s from %s: %w", key, s.stream(), err)
	}
	switch {
	case response.Error != nil && response.Error.ErrorCode == jetstream.JSErrCodeMessageNotFound:
		return nil, 0, ErrNoRecord
	case response.Error != nil && response.Error.ErrorCode == jetstream.JSErrCodeStreamNotFound:
		return nil, 0, fmt.Errorf("%w: %s (%w)", ErrNoStore, s.stream(), response.Error)
	case response.Error != nil:
		return nil, 0, fmt.Errorf("consumer names: read %s from %s: %w", key, s.stream(), response.Error)
	case response.Message == nil:
		return nil, 0, fmt.Errorf("consumer names: the answer for %s from %s holds no message", key, s.stream())
	}
	if len(response.Message.Header) > 0 {
		header, err := nats.DecodeHeadersMsg(response.Message.Header)
		if err != nil {
			return nil, 0, fmt.Errorf("consumer names: read %s's header from %s: %w", key, s.stream(), err)
		}
		if op := header.Get(kvOperationHeader); op == "DEL" || op == "PURGE" {
			return nil, 0, fmt.Errorf("%w: %s's last message in %s is a %s marker", ErrRecordDeleted, key, s.bucket, op)
		}
	}
	return response.Message.Data, response.Message.Seq, nil
}

// put publishes data on key if the key's last sequence is revision.
func (s *KVStore) put(ctx context.Context, key string, data []byte, revision uint64) (uint64, error) {
	ctx, cancel := s.bound(ctx)
	defer cancel()
	msg := &nats.Msg{Subject: s.subject(key), Header: nats.Header{}, Data: data}
	msg.Header.Set(jetstream.ExpectedLastSubjSeqHeader, strconv.FormatUint(revision, 10))
	reply, err := s.request.Request(ctx, msg)
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) {
			return 0, fmt.Errorf("%w: %s (%w)", ErrNoStore, s.stream(), err)
		}
		return 0, fmt.Errorf("consumer names: write %s to %s: %w", key, s.bucket, err)
	}
	var ack struct {
		Seq   uint64              `json:"seq"`
		Error *jetstream.APIError `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &ack); err != nil {
		return 0, fmt.Errorf("consumer names: read the acknowledgement of %s's write to %s: %w", key, s.bucket, err)
	}
	switch {
	// A replicated stream answers a write that meets another in flight on the same key with 10164,
	// one copy a stale write with 10071: either is the record changed under the write.
	case ack.Error != nil && (ack.Error.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence ||
		ack.Error.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequenceConstant):
		return 0, fmt.Errorf("%w: %s in %s is past revision %d (%w)", ErrConflict, key, s.bucket, revision, ack.Error)
	case ack.Error != nil:
		return 0, fmt.Errorf("consumer names: write %s to %s: %w", key, s.bucket, ack.Error)
	case ack.Seq == 0:
		return 0, fmt.Errorf("consumer names: the write of %s to %s was acknowledged with no sequence", key, s.bucket)
	}
	return ack.Seq, nil
}
