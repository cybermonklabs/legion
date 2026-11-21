package natscallout

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/kvrecords"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// startReplica runs one auth service replica against the servers at urls with keys' seeds,
// admitting services, deciding with decide, stopped when the test ends, and returns once its
// subscription has reached the hub.
func startReplica(t *testing.T, urls []string, keys testnats.CalloutKeys, services []ServiceEntry, deps Deps, decide decideFunc) *Service {
	t.Helper()
	dir := t.TempDir()
	write := func(name, seed string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(seed+"\n"), 0o600))
		return path
	}
	settings := Settings{
		URLs:           urls,
		SeedFile:       write("service.seed", keys.ServiceSeed),
		IssuerSeedFile: write("issuer.seed", keys.IssuerSeed),
		XKeySeedFile:   write("xkey.seed", keys.XKeySeed),
		Services:       services,
	}
	service, err := start(t.Context(), settings, deps, decide)
	require.NoError(t, err)
	t.Cleanup(service.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, service.Ready(ctx), "the auth service's subscription never reached the hub")
	return service
}

// readUnsignedToken is a stub ServiceToken: the account, issue and expiry an unsigned platform
// token names (unsignedServiceToken), read without verifying it.
func readUnsignedToken(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", false, time.Time{}, time.Time{}, errors.New("the token is no JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false, time.Time{}, time.Time{}, err
	}
	var claims struct {
		Sub      string `json:"sub"`
		Iat, Exp int64
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false, time.Time{}, time.Time{}, err
	}
	return claims.Sub, false, time.Unix(claims.Iat, 0), time.Unix(claims.Exp, 0), nil
}

// tokenLives is a natscred.TokenReader that answers a fresh unsigned token of account at each
// CONNECT, living the next of lives, and the last of lives at every read past them, so a test sets
// the expiry of each decision a connection's reconnects ask for.
type tokenLives struct {
	account string
	mu      sync.Mutex
	lives   []time.Duration
	reads   int
}

func (r *tokenLives) Read() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	life := r.lives[min(r.reads, len(r.lives)-1)]
	r.reads++
	return unsignedServiceToken(r.account, life), nil
}

// hourLong is a reader whose every token lives an hour.
func hourLong(account string) *tokenLives {
	return &tokenLives{account: account, lives: []time.Duration{time.Hour}}
}

// decided is one decision the auth service admitted, and when its decision ended.
type decided struct {
	Decision
	done time.Time
}

// decisionLog keeps every decision a replica admits, by user name.
type decisionLog struct {
	mu     sync.Mutex
	byName map[string][]decided
}

func newDecisionLog() *decisionLog { return &decisionLog{byName: map[string][]decided{}} }

// wrap is decide, keeping each decision it admits.
func (l *decisionLog) wrap(decide decideFunc) decideFunc {
	return func(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
		decision, err := decide(ctx, deps, services, req)
		if err == nil {
			l.mu.Lock()
			l.byName[decision.Name] = append(l.byName[decision.Name], decided{decision, time.Now()})
			l.mu.Unlock()
		}
		return decision, err
	}
}

// count is how many decisions admitted name.
func (l *decisionLog) count(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.byName[name])
}

// since is every decision admitting name past the first n.
func (l *decisionLog) since(name string, n int) []decided {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.byName[name][n:])
}

// after waits for a decision admitting name past the first n, and returns the latest.
func (l *decisionLog) after(t *testing.T, name string, n int) decided {
	t.Helper()
	testwait.WithinPolling(t, 15*time.Second, 20*time.Millisecond, "a decision admitting "+name, func() bool { return l.count(name) > n })
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byName[name][len(l.byName[name])-1]
}

// requireOperatorDecision requires d to be the operator's user with phase's grant, expiring at most
// operatorLife after its decision.
func requireOperatorDecision(t *testing.T, d decided, phase natsgrant.OperatorPhase) {
	t.Helper()
	require.Equal(t, natsgrant.Operator(phase), d.Grant, "the operator was not answered with %s's grant", phase)
	require.False(t, d.Expires.After(d.done.Add(10*time.Minute)),
		"the operator's user expires %s after its decision, past 10 minutes", d.Expires.Sub(d.done))
}

// The names a decision answers the operator, a listener and the probe with.
var (
	operatorName = "operator/" + operatorAccount
	listenerName = "listener/example-hub"
)

// operatorRig is the clustered hub each phase's grant is exercised on: three members of
// testnats.StartClusterWithCallout, one auth service replica deciding with Decide and keeping its
// decisions, the operator's hold signing key and a HoldRead the test sets, and an admin static
// user that makes the streams the deployment's programs make.
type operatorRig struct {
	cluster   *testnats.Cluster
	keys      testnats.CalloutKeys
	adminSeed string
	signer    holdSigner
	hold      *holdStub
	gen       uint64
	decisions *decisionLog
	registry  *metrics.Registry
	service   *Service
	// listens counts listen's connects, which go to each member in turn.
	listens atomic.Int64
}

// newOperatorRig starts the rig with a hold signing key of its own. read is the replica's
// HoldRead; nil is the rig's own stub, whose row setPhase writes. sessions are the sessions the
// broker's introspection answers live.
func newOperatorRig(t *testing.T, read func(context.Context) (natshold.Reading, bool, error), sessions ...*session) *operatorRig {
	t.Helper()
	return newOperatorRigSigned(t, read, newHoldSigner(t), sessions...)
}

// setPhase writes the rig's stub row: phase at the next gen, signed by the operator's key.
func (r *operatorRig) setPhase(t *testing.T, phase natsgrant.OperatorPhase) {
	t.Helper()
	r.gen++
	r.hold.set(rowOf(r.signer.reading(t, r.gen, phase, "hold-1")))
}

// listen connects once as the listener entry, with an hour-long token and no reconnect, to one
// member, each call the next member, so an authorization request reaches the replica from every
// member in turn, and one connect is one decision: a connect to the cluster's whole list would try
// each member in turn on a refusal, one decision each. It answers the server's refusal, or nil once
// admitted.
func (r *operatorRig) listen() error {
	member := r.cluster.Servers[int(r.listens.Add(1))%len(r.cluster.Servers)]
	nc, err := nats.Connect(member.URL(), nats.UserInfo(natscred.ServiceUser, unsignedServiceToken(listenerAccount, time.Hour)),
		nats.NoReconnect(), nats.Timeout(5*time.Second))
	if err == nil {
		nc.Close()
	}
	return err
}

// admin is a JetStream context of the admin static user, once JetStream answers it.
func (r *operatorRig) admin(t *testing.T) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	nc, err := testnats.ConnectSeed(t, r.cluster.URL(), r.adminSeed)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := nc.JetStream(nats.MaxWait(30 * time.Second))
	require.NoError(t, err)
	testwait.WithinPolling(t, 30*time.Second, 100*time.Millisecond, "JetStream to answer the admin", func() bool {
		_, err := js.AccountInfo()
		return err == nil
	})
	return nc, js
}

// kvBucket is KV_<bucket>, the stream behind a bucket.
func kvBucket(bucket string) string { return "KV_" + bucket }

// The buckets the operator's grant names: envoy-infra buckets' copied buckets, the session
// consumers' records, and the other protected buckets.
var (
	copiedBuckets       = []string{bus.InterestBucket, bus.RoleClaimBucket, bus.SessionBucket, bus.KnownSessionBucket, bus.CIStateBucket}
	sessionConsumers    = "envoy_session_consumers"
	otherProtected      = []string{bus.LegionAgentBucket, bus.DPoPProofBucket, bus.WebhookRedeliveryBucket}
	retiredHoldBucketKV = "$KV.envoy_maintenance.hold"
)

// makeStreams makes, as the admin, every bucket the operator's grant names, envoy_sessions holding
// entries of entryBytes random bytes each, three copies of it and one of the others, and
// ENVOY_NOTIFICATIONS capped at maxConsumers (0 for the server's default), and returns the admin's
// JetStream context.
func (r *operatorRig) makeStreams(t *testing.T, entries, entryBytes, maxConsumers int) nats.JetStreamContext {
	t.Helper()
	_, js := r.admin(t)
	for _, bucket := range slices.Concat(copiedBuckets, []string{sessionConsumers}, otherProtected) {
		replicas := 1
		if bucket == bus.SessionBucket {
			replicas = 3
		}
		kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: replicas})
		require.NoError(t, err, "create bucket %s", bucket)
		if bucket == sessionConsumers {
			_, err := kv.Put("listener.example-hub", []byte(`{"name":"session-a"}`))
			require.NoError(t, err)
		}
		if bucket == bus.SessionBucket {
			for index := range entries {
				value := make([]byte, entryBytes)
				_, _ = rand.Read(value)
				_, err := kv.Put(fmt.Sprintf("ses_%03d", index), value)
				require.NoError(t, err)
			}
		}
	}
	_, err := js.AddStream(&nats.StreamConfig{Name: bus.Stream, Subjects: []string{"notifications.>"}, Storage: nats.FileStorage, MaxConsumers: maxConsumers})
	require.NoError(t, err)
	return js
}

// serverErrors keeps every asynchronous error the server sends a connection.
type serverErrors struct {
	mu    sync.Mutex
	lines []string
}

func (e *serverErrors) handle(_ *nats.Conn, _ *nats.Subscription, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lines = append(e.lines, err.Error())
}

// String is every error the server sent, one a line, for a failure's message: a refused publish
// inside a backup or a restore is named only here.
func (e *serverErrors) String() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.lines, "\n")
}

func (e *serverErrors) seen(want string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.ContainsFunc(e.lines, func(line string) bool { return strings.Contains(line, want) })
}

// operatorConn is the operator's connection as its commands make one: user service and its
// platform token through natscred.Service, the operator's own inbox prefix, the server's
// asynchronous errors kept, and each disconnect and reconnect signalled.
type operatorConn struct {
	*nats.Conn
	errs                      *serverErrors
	disconnected, reconnected chan struct{}
}

// connectOperator connects the operator through reader to the cluster.
func (r *operatorRig) connectOperator(t *testing.T, reader natscred.TokenReader) *operatorConn {
	t.Helper()
	return connectService(t, r.cluster.URL(), reader, natsgrant.OperatorInbox)
}

// connectService connects a service to url through reader, its inboxes under inbox ("" for
// nats.go's own).
func connectService(t *testing.T, url string, reader natscred.TokenReader, inbox string) *operatorConn {
	t.Helper()
	option, _ := natscred.Service(reader)
	o := &operatorConn{errs: &serverErrors{}, disconnected: make(chan struct{}, 16), reconnected: make(chan struct{}, 16)}
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	options := []nats.Option{option, nats.Timeout(5 * time.Second), nats.ErrorHandler(o.errs.handle),
		nats.MaxReconnects(-1), nats.ReconnectWait(100 * time.Millisecond),
		nats.DisconnectErrHandler(func(*nats.Conn, error) { signal(o.disconnected) }),
		nats.ReconnectHandler(func(*nats.Conn) { signal(o.reconnected) })}
	if inbox != "" {
		options = append(options, nats.CustomInboxPrefix(inbox))
	}
	nc, err := nats.Connect(url, options...)
	require.NoError(t, err, "the service's connection was refused")
	t.Cleanup(nc.Close)
	o.Conn = nc
	return o
}

// drain empties ch.
func drain(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// reconnect forces a reconnect, so the connection is decided again, and waits for it.
func (o *operatorConn) reconnect(t *testing.T) {
	t.Helper()
	drain(o.reconnected)
	require.NoError(t, o.ForceReconnect())
	select {
	case <-o.reconnected:
	case <-time.After(15 * time.Second):
		t.Fatal("the forced reconnect never reconnected")
	}
	drain(o.disconnected)
}

// apiAnswer is a JetStream API answer, read as far as its error.
type apiAnswer struct {
	Error *struct {
		Code        int    `json:"code"`
		ErrCode     int    `json:"err_code"`
		Description string `json:"description"`
	} `json:"error"`
	raw []byte
}

// answerWait bounds one JetStream request the grant admits. A create of a stream of three copies is
// answered by its first leader once its members elect one, which on a loaded machine comes after
// ten seconds (testnats.jetStreamWait says so): one such create here got no answer within ten.
const answerWait = 30 * time.Second

// answered requires a request on subject to be answered, and returns the answer.
func answered(t *testing.T, nc *nats.Conn, subject string, body any) apiAnswer {
	t.Helper()
	msg, err := nc.Request(subject, encoded(t, body), answerWait)
	require.NoError(t, err, "%s got no answer", subject)
	answer := apiAnswer{raw: msg.Data}
	require.NoError(t, json.Unmarshal(msg.Data, &answer), "%s answered %q", subject, msg.Data)
	return answer
}

// notFoundRetry bounds succeeds's retry of a 10059, "stream not found", for a stream the test made.
// Twice in about ten runs, on this package's three members of nats-server v2.15.1,
// STREAM.MSG.GET.KV_envoy_session_consumers was answered so just after the same stream's
// STREAM.INFO was answered, with no meta leader change logged in the run; nats-server's
// jsMsgGetRequest answers it only from a meta leader with no assignment for the stream, or from
// the stream leader's failed lookup. The cause is not found, so each retry is logged.
const notFoundRetry = 5 * time.Second

// succeeds requires a request on subject to be answered with no error, and returns the answer.
func succeeds(t *testing.T, nc *nats.Conn, subject string, body any) apiAnswer {
	t.Helper()
	answer := answered(t, nc, subject, body)
	for first := time.Now(); answer.Error != nil && answer.Error.ErrCode == 10059 && time.Since(first) < notFoundRetry; {
		t.Logf("%s answered %s; asking again", subject, answer.raw)
		time.Sleep(250 * time.Millisecond)
		answer = answered(t, nc, subject, body)
	}
	require.Nil(t, answer.Error, "%s was refused: %s", subject, answer.raw)
	return answer
}

// refused requires a request on subject to get no answer, and the server to report a publish
// permissions violation naming subject.
func (o *operatorConn) refused(t *testing.T, subject string, body any) {
	t.Helper()
	msg, err := o.Request(subject, encoded(t, body), time.Second)
	if err == nil {
		t.Fatalf("%s was answered: %s", subject, msg.Data)
	}
	require.ErrorIs(t, err, nats.ErrTimeout, "%s", subject)
	want := `Permissions Violation for Publish to "` + subject + `"`
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, want, func() bool { return o.errs.seen(want) })
}

// encoded is body as a request's payload: nil for none, bytes as they are, anything else as JSON.
func encoded(t *testing.T, body any) []byte {
	t.Helper()
	switch body := body.(type) {
	case nil:
		return nil
	case []byte:
		return body
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return raw
}

// streamBackup is what a backup of a stream holds: jsm.go v0.5.0's two files, backup.json's config
// and state and the archive.
type streamBackup struct {
	stream        string
	config, state json.RawMessage
	messages      uint64
	data          []byte
}

// snapshotChunk is the chunk and the window a backup asks for: one chunk in flight, so every chunk
// after the first waits for the acknowledgement of the one before, as each chunk past the default
// window of 8 MiB does (nats-server v2.15.1 jetstream_api.go streamSnapshot).
const snapshotChunk = 4096

// backupStream takes stream's backup through nc as jsm.go v0.5.0's (*Stream).SnapshotToDirectory
// does (snapshots.go:355-514, :558-613): the stream's info first (Manager.LoadStream, which the
// backup cannot start without), then the snapshot request, its deliver subject a response inbox of
// nc's own, each chunk acknowledged on the reply the server sends it with, until the empty message
// that ends it. It answers an error naming the step that failed. jsm.go itself is not imported:
// v0.5.0 requires antithesis-sdk-go v0.8.0, which would turn the Antithesis SDK nats-server links
// from no-op to enabled in every module of the workspace.
func backupStream(nc *nats.Conn, stream string) (streamBackup, error) {
	info, err := nc.Request("$JS.API.STREAM.INFO."+stream, nil, answerWait)
	if err != nil {
		return streamBackup{}, fmt.Errorf("load stream %s (STREAM.INFO): %w", stream, err)
	}
	var loaded apiAnswer
	if err := json.Unmarshal(info.Data, &loaded); err != nil || loaded.Error != nil {
		return streamBackup{}, fmt.Errorf("load stream %s (STREAM.INFO): %s", stream, info.Data)
	}
	inbox := nc.NewRespInbox()
	done := make(chan error, 1)
	var mu sync.Mutex
	var data []byte
	sub, err := nc.Subscribe(inbox, func(msg *nats.Msg) {
		if len(msg.Data) == 0 {
			if status := msg.Header.Get("Status"); status != "" && !strings.Contains(status, "204") {
				done <- fmt.Errorf("the snapshot of %s ended %s %s", stream, status, msg.Header.Get("Description"))
				return
			}
			done <- nil
			return
		}
		if msg.Reply != "" {
			_ = msg.Respond(nil)
		}
		mu.Lock()
		data = append(data, msg.Data...)
		mu.Unlock()
	})
	if err != nil {
		return streamBackup{}, err
	}
	defer func() { _ = sub.Unsubscribe() }()
	request, _ := json.Marshal(map[string]any{"deliver_subject": inbox, "no_consumers": true, "chunk_size": snapshotChunk, "window_size": snapshotChunk})
	reply, err := nc.Request("$JS.API.STREAM.SNAPSHOT."+stream, request, answerWait)
	if err != nil {
		return streamBackup{}, fmt.Errorf("snapshot %s (STREAM.SNAPSHOT): %w", stream, err)
	}
	var answer struct {
		apiAnswer
		Config json.RawMessage `json:"config"`
		State  json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(reply.Data, &answer); err != nil || answer.Error != nil {
		return streamBackup{}, fmt.Errorf("snapshot %s (STREAM.SNAPSHOT): %s", stream, reply.Data)
	}
	select {
	case err := <-done:
		if err != nil {
			return streamBackup{}, err
		}
	case <-time.After(30 * time.Second):
		return streamBackup{}, fmt.Errorf("the snapshot of %s did not end within 30s", stream)
	}
	var state struct {
		Msgs uint64 `json:"messages"`
	}
	_ = json.Unmarshal(answer.State, &state)
	mu.Lock()
	defer mu.Unlock()
	return streamBackup{stream: stream, config: answer.Config, state: answer.State, messages: state.Msgs, data: data}, nil
}

// restoreChunk is jsm.go v0.5.0's restore chunk, 64 KiB (snapshots.go:706).
const restoreChunk = 64 * 1024

// The bounds of restoreStream's two retries of a server answer that a moment later is different,
// each measured on this package's three-member cluster of nats-server v2.15.1.
const (
	// storeRetry bounds the restore request's retry of 10049, "error creating store for stream":
	// a restore sent at once after a delete of the same three-copy stream was refused so in one
	// of four runs, the member's store of the deleted stream not yet gone
	// (jetstream_cluster.go:7043-7044, processClusterCreateStream, answers any store failure so).
	storeRetry = 5 * time.Second
	// interestRetry bounds a chunk's retry of "no responders": the server subscribes the restore's
	// deliver subject on the member that restores it and answers the request at once
	// (jetstream_api.go:4589, :4598-4599), and the subscription reaches the client's member over
	// the route after the answer can, so the first chunk of one restore in five was answered no
	// responders.
	interestRetry = 2 * time.Second
)

// restoreStream restores b through nc as jsm.go v0.5.0's Manager.RestoreSnapshotFromDirectory does
// (snapshots.go:630-771): the restore request with the backup's config and state, each chunk as a
// request on the deliver subject the server names, then an empty request whose answer is the
// stream's create response; it answers the restored stream's message count. paced runs after each
// chunk, and an ended ctx stops the restore between chunks and cuts the chunk in flight short.
// Beyond jsm.go it retries the two answers above, each within its bound.
func restoreStream(ctx context.Context, nc *nats.Conn, b streamBackup, paced func()) (uint64, error) {
	request, _ := json.Marshal(map[string]json.RawMessage{"config": b.config, "state": b.state})
	var started struct {
		apiAnswer
		DeliverSubject string `json:"deliver_subject"`
	}
	for first := time.Now(); ; time.Sleep(100 * time.Millisecond) {
		reply, err := nc.Request("$JS.API.STREAM.RESTORE."+b.stream, request, answerWait)
		if err != nil {
			return 0, fmt.Errorf("restore %s (STREAM.RESTORE): %w", b.stream, err)
		}
		started.Error = nil
		if err := json.Unmarshal(reply.Data, &started); err != nil {
			return 0, fmt.Errorf("restore %s (STREAM.RESTORE): %s", b.stream, reply.Data)
		}
		if started.Error == nil {
			break
		}
		if started.Error.ErrCode != 10049 || time.Since(first) > storeRetry {
			return 0, fmt.Errorf("restore %s (STREAM.RESTORE): %s", b.stream, reply.Data)
		}
	}
	for offset := 0; offset < len(b.data); offset += restoreChunk {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("restore %s at byte %d: %w", b.stream, offset, err)
		}
		var ack *nats.Msg
		var err error
		for first := time.Now(); ; time.Sleep(50 * time.Millisecond) {
			chunkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			ack, err = nc.RequestWithContext(chunkCtx, started.DeliverSubject, b.data[offset:min(offset+restoreChunk, len(b.data))])
			cancel()
			if !errors.Is(err, nats.ErrNoResponders) || time.Since(first) > interestRetry || ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			return 0, fmt.Errorf("restore %s: the chunk at byte %d to %s: %w", b.stream, offset, started.DeliverSubject, err)
		}
		if len(ack.Data) > 0 {
			return 0, fmt.Errorf("restore %s: the chunk at byte %d: %s", b.stream, offset, ack.Data)
		}
		if paced != nil {
			paced()
		}
	}
	finished, err := nc.Request(started.DeliverSubject, nil, time.Minute)
	if err != nil {
		return 0, fmt.Errorf("restore %s: the end of the chunks: %w", b.stream, err)
	}
	var created struct {
		apiAnswer
		State struct {
			Msgs uint64 `json:"messages"`
		} `json:"state"`
	}
	if err := json.Unmarshal(finished.Data, &created); err != nil || created.Error != nil {
		return 0, fmt.Errorf("restore %s: %s", b.stream, finished.Data)
	}
	return created.State.Msgs, nil
}

// streamConfig is stream's configuration as its info answers it, for an update of it.
func streamConfig(t *testing.T, nc *nats.Conn, stream string) map[string]any {
	t.Helper()
	var info struct {
		Config map[string]any `json:"config"`
	}
	require.NoError(t, json.Unmarshal(succeeds(t, nc, "$JS.API.STREAM.INFO."+stream, nil).raw, &info))
	return info.Config
}

// The operator's grant outside a hold is reads only. It reads every info the hold's first step
// reads, backs up KV_envoy_session_consumers as a records-only copy read message by message
// (kvrecords.Write), each walked message with its sequence, headers and time, and reads what
// consumer-names status reads. It writes nothing: a snapshot delivered to a protected key, and a
// create of an absent ENVOY_MEASURE whose republish targets a protected key followed by a publish
// the grant holds, are each refused, and the key is unchanged; it creates, changes and deletes no
// protected stream, its consumer creates are refused, and the retired hold bucket is not reachable.
// Each refused request gets no answer, and the server reports the publish it refused, by subject.
//
// It fails when released's grant holds a write again: run by hand against the grant that held
// STREAM.SNAPSHOT and STREAM.CREATE.ENVOY_MEASURE, the snapshot writes the protected key and the
// test fails naming it. And it fails when released loses a read the records copy needs.
func TestTheReleasedOperatorReadsAndBacksUp(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseReleased)
	adminNC, js := r.admin(t)
	r.makeStreams(t, 20, 1024, 0)
	_, err := js.AddConsumer(bus.Stream, &nats.ConsumerConfig{Name: "session-a", AckPolicy: nats.AckExplicitPolicy, InactiveThreshold: time.Hour})
	require.NoError(t, err)
	// A retired session's record, deleted: its DEL marker is a stored message with headers, which
	// the copy must keep.
	recordsBucket, err := js.KeyValue(sessionConsumers)
	require.NoError(t, err)
	_, err = recordsBucket.Put("s-ses_retired", []byte(`{"name":"session-retired"}`))
	require.NoError(t, err)
	require.NoError(t, recordsBucket.Delete("s-ses_retired"))

	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseReleased)

	// The records copy first, so a grant without a read it needs fails the test inside it.
	dir := filepath.Join(t.TempDir(), "consumer-names")
	manifest, err := kvrecords.Write(t.Context(), operator.Conn, sessionConsumers, dir)
	require.NoError(t, err, "the released operator's records copy of %s; the server sent:\n%s", kvBucket(sessionConsumers), operator.errs)
	_, records, err := kvrecords.Read(dir)
	require.NoError(t, err)
	want := storedMessages(t, adminNC, kvBucket(sessionConsumers))
	require.Equal(t, len(want), manifest.Records)
	require.Len(t, records, len(want), "the copy holds %d of the %d stored messages", len(records), len(want))
	for index, record := range records {
		require.Equal(t, want[index].Seq, record.Seq, "record %d's sequence", index)
		require.Equal(t, want[index].Subject, record.Subject, "sequence %d's subject", record.Seq)
		require.True(t, want[index].Time.Equal(record.Time), "sequence %d stored at %s, the record says %s", record.Seq, want[index].Time, record.Time)
		require.Equal(t, string(want[index].Header), string(record.Headers.Block()), "sequence %d's headers", record.Seq)
		require.Equal(t, string(want[index].Data), string(record.Data), "sequence %d's data", record.Seq)
	}
	require.True(t, slices.ContainsFunc(records, func(record kvrecords.Record) bool { return len(record.Headers) > 0 }),
		"no record carries headers, so the copy's headers went untested")

	succeeds(t, operator.Conn, "$JS.API.INFO", nil)
	for _, stream := range append([]string{bus.Stream, kvBucket(sessionConsumers)}, mapped(copiedBuckets, kvBucket)...) {
		succeeds(t, operator.Conn, "$JS.API.STREAM.INFO."+stream, nil)
	}
	var names struct {
		Consumers []string `json:"consumers"`
	}
	require.NoError(t, json.Unmarshal(succeeds(t, operator.Conn, "$JS.API.CONSUMER.NAMES."+bus.Stream, nil).raw, &names))
	require.Contains(t, names.Consumers, "session-a")
	succeeds(t, operator.Conn, "$JS.API.CONSUMER.INFO."+bus.Stream+".session-a", nil)

	// The two writes released's grant once held, each aimed at a protected key of its own, so a
	// failure names which one wrote.
	snapshotVictim, measureVictim := putProtectedKey(t, js, "victim-snapshot"), putProtectedKey(t, js, "victim-measure")
	snapshot := map[string]any{"deliver_subject": snapshotVictim.subject, "no_consumers": true, "chunk_size": snapshotChunk}
	measure := map[string]any{"name": "ENVOY_MEASURE", "subjects": []string{"$JS.API.STREAM.INFO.ENVOY_MEASURE"}, "no_ack": true,
		"storage": "memory", "num_replicas": 1, "republish": map[string]any{"src": "$JS.API.STREAM.INFO.ENVOY_MEASURE", "dest": measureVictim.subject}}
	_, _ = operator.Request("$JS.API.STREAM.SNAPSHOT."+kvBucket(bus.InterestBucket), encoded(t, snapshot), time.Second)
	_, _ = operator.Request("$JS.API.STREAM.CREATE.ENVOY_MEASURE", encoded(t, measure), time.Second)
	require.NoError(t, operator.Publish("$JS.API.STREAM.INFO.ENVOY_MEASURE", []byte("chosen by the released operator")))
	require.NoError(t, operator.Flush())
	snapshotVictim.checkUnchanged(t, js)
	measureVictim.checkUnchanged(t, js)
	if t.Failed() {
		t.FailNow()
	}
	operator.refused(t, "$JS.API.STREAM.SNAPSHOT."+kvBucket(bus.InterestBucket), snapshot)
	operator.refused(t, "$JS.API.STREAM.CREATE.ENVOY_MEASURE", measure)
	absent := answered(t, operator.Conn, "$JS.API.STREAM.INFO.ENVOY_MEASURE", nil)
	require.NotNil(t, absent.Error, "ENVOY_MEASURE exists after the released operator's refused create: %s", absent.raw)
	require.Equal(t, 10059, absent.Error.ErrCode, "the info of the absent ENVOY_MEASURE: %s", absent.raw)

	sessions := streamConfig(t, adminNC, kvBucket(bus.SessionBucket))
	for _, request := range []struct {
		subject string
		body    any
	}{
		{"$JS.API.STREAM.CREATE." + kvBucket(bus.SessionBucket), sessions},
		{"$JS.API.STREAM.CREATE." + kvBucket(bus.LegionAgentBucket), map[string]any{"name": kvBucket(bus.LegionAgentBucket)}},
		{"$JS.API.STREAM.UPDATE." + kvBucket(bus.SessionBucket), sessions},
		{"$JS.API.STREAM.DELETE." + kvBucket(bus.SessionBucket), nil},
		{"$JS.API.STREAM.SNAPSHOT." + kvBucket(sessionConsumers), map[string]any{"deliver_subject": operator.NewRespInbox()}},
		{"$JS.API.CONSUMER.CREATE." + bus.Stream + ".x", map[string]any{"stream_name": bus.Stream, "config": map[string]any{"name": "x"}}},
		{retiredHoldBucketKV, []byte("released")},
	} {
		operator.refused(t, request.subject, request.body)
	}
	info, err := js.StreamInfo(kvBucket(bus.SessionBucket))
	require.NoError(t, err, "the released operator's refused delete removed %s", kvBucket(bus.SessionBucket))
	require.EqualValues(t, 22, info.State.Msgs, "KV_envoy_sessions holds other than its 20 entries and the two victim keys")
}

// protectedKey is a key of envoy_sessions, a protected bucket, that the admin puts, and which no
// write outside a hold may change.
type protectedKey struct {
	subject  string
	sequence uint64
	value    string
}

// putProtectedKey puts key in envoy_sessions as the admin and reads back where it is stored.
func putProtectedKey(t *testing.T, js nats.JetStreamContext, key string) protectedKey {
	t.Helper()
	kv, err := js.KeyValue(bus.SessionBucket)
	require.NoError(t, err)
	sequence, err := kv.Put(key, []byte("put by the admin"))
	require.NoError(t, err)
	return protectedKey{subject: "$KV." + bus.SessionBucket + "." + key, sequence: sequence, value: "put by the admin"}
}

// checkUnchanged checks that the key's last stored message stays the admin's put for three
// seconds, past the time a snapshot's chunks or a republish reach it, and fails the test naming
// the key and what was stored over it, going on, so a test checking two keys names both.
func (k protectedKey) checkUnchanged(t *testing.T, js nats.JetStreamContext) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := js.GetLastMsg(kvBucket(bus.SessionBucket), k.subject)
		require.NoError(t, err, "read %s", k.subject)
		if msg.Sequence != k.sequence || string(msg.Data) != k.value {
			t.Errorf("the protected key %s was written: sequence %d (%q) became sequence %d (%q, headers %v)",
				k.subject, k.sequence, k.value, msg.Sequence, msg.Data, msg.Header)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// staticUserEntry is one nkey user of a lone server's configuration, holding exactly grant.
func staticUserEntry(nkey string, grant natsgrant.Permissions) string {
	quoted := func(subjects []string) string {
		out := make([]string, len(subjects))
		for index, subject := range subjects {
			out[index] = fmt.Sprintf("%q", subject)
		}
		return strings.Join(out, ", ")
	}
	return fmt.Sprintf("{ nkey: %q, permissions: { publish: { allow: [%s] }, subscribe: { allow: [%s] } } }",
		nkey, quoted(grant.Publish), quoted(grant.Subscribe))
}

// The measurement job's grant makes no stream: on a lone server, a user holding exactly
// natsgrant.Measure() is refused the create of ENVOY_MEASURE, whose republish would have targeted
// a key of envoy_sessions, and is refused the publish that stream would have captured; the key is
// unchanged. A create carries a configuration of the caller's choosing, and a republish in it
// writes any subject from the stream's own sender.
//
// Run by hand, the control is Measure() as it was, holding STREAM.CREATE.ENVOY_MEASURE and
// envoy-measure.>: the create is answered, the publish is republished onto the key, and the test
// fails naming the key.
func TestTheMeasureUserMakesNoStream(t *testing.T) {
	adminSeed, adminKey := testnats.User(t)
	measureSeed, measureKey := testnats.User(t)
	config := "jetstream {}\nauthorization {\n  users = [\n    " +
		staticUserEntry(adminKey, natsgrant.Permissions{Publish: []string{">"}, Subscribe: []string{">"}}) + "\n    " +
		staticUserEntry(measureKey, natsgrant.Measure()) + "\n  ]\n}\n"
	_, url := testnats.StartConfig(t, config, []string{"-js"}, false)
	admin, err := testnats.ConnectSeed(t, url, adminSeed)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	js, err := admin.JetStream()
	require.NoError(t, err)
	_, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bus.SessionBucket})
	require.NoError(t, err)
	victim := putProtectedKey(t, js, "victim")

	errs := &serverErrors{}
	measure, err := testnats.ConnectSeed(t, url, measureSeed, nats.CustomInboxPrefix("_INBOX_envoy_measure"), nats.ErrorHandler(errs.handle))
	require.NoError(t, err)
	t.Cleanup(measure.Close)
	o := &operatorConn{Conn: measure, errs: errs}
	create := map[string]any{"name": "ENVOY_MEASURE", "subjects": []string{"envoy-measure.x"}, "storage": "memory", "num_replicas": 1,
		"republish": map[string]any{"src": "envoy-measure.x", "dest": victim.subject}}
	_, _ = measure.Request("$JS.API.STREAM.CREATE.ENVOY_MEASURE", encoded(t, create), time.Second)
	require.NoError(t, measure.Publish("envoy-measure.x", []byte("chosen by the measurement user")))
	require.NoError(t, measure.Flush())
	victim.checkUnchanged(t, js)
	if t.Failed() {
		t.FailNow()
	}
	o.refused(t, "$JS.API.STREAM.CREATE.ENVOY_MEASURE", create)
	want := `Permissions Violation for Publish to "envoy-measure.x"`
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, want, func() bool { return errs.seen(want) })
	_, err = js.StreamInfo("ENVOY_MEASURE")
	require.ErrorIs(t, err, nats.ErrStreamNotFound, "the measurement user made ENVOY_MEASURE")
}

// storedMessage is one message as the server stores it, read back with STREAM.MSG.GET.
type storedMessage struct {
	Subject string    `json:"subject"`
	Seq     uint64    `json:"seq"`
	Header  []byte    `json:"hdrs"`
	Data    []byte    `json:"data"`
	Time    time.Time `json:"time"`
}

// storedMessages is every message stream stores, in sequence order, read through nc one sequence
// at a time, a sequence the server no longer stores (10037) left out.
func storedMessages(t *testing.T, nc *nats.Conn, stream string) []storedMessage {
	t.Helper()
	var info struct {
		State struct {
			FirstSeq uint64 `json:"first_seq"`
			LastSeq  uint64 `json:"last_seq"`
		} `json:"state"`
	}
	require.NoError(t, json.Unmarshal(succeeds(t, nc, "$JS.API.STREAM.INFO."+stream, nil).raw, &info))
	var messages []storedMessage
	for seq := max(info.State.FirstSeq, 1); seq <= info.State.LastSeq; seq++ {
		answer := answered(t, nc, "$JS.API.STREAM.MSG.GET."+stream, map[string]uint64{"seq": seq})
		if answer.Error != nil {
			require.Equal(t, 10037, answer.Error.ErrCode, "%s sequence %d: %s", stream, seq, answer.raw)
			continue
		}
		var got struct {
			Message storedMessage `json:"message"`
		}
		require.NoError(t, json.Unmarshal(answer.raw, &got))
		messages = append(messages, got.Message)
	}
	return messages
}

// mapped is each of in through f.
func mapped(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for index, each := range in {
		out[index] = f(each)
	}
	return out
}

// The operator's grant while a hold is prepared raises a session consumer's threshold on
// ENVOY_NOTIFICATIONS with an update, which the grant outside a hold refuses; it still deletes no
// protected stream and no consumer, and lists no stream. The hold's cohort is a record of the
// broker's Postgres, written through the routes (its half of this row is the routes' test).
//
// It fails when prepare's grant loses the consumer update, or released's admits it (the
// control, the first update, made under released before the row turns prepare), or when prepare's
// admits a delete or the stream list.
func TestThePrepareOperatorRaisesThresholdsAndWritesTheCohort(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseReleased)
	_, js := r.admin(t)
	r.makeStreams(t, 1, 16, 0)
	_, err := js.AddConsumer(bus.Stream, &nats.ConsumerConfig{Name: "session-a", AckPolicy: nats.AckExplicitPolicy, InactiveThreshold: time.Hour})
	require.NoError(t, err)
	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseReleased)

	var info struct {
		Config map[string]any `json:"config"`
	}
	require.NoError(t, json.Unmarshal(succeeds(t, operator.Conn, "$JS.API.CONSUMER.INFO."+bus.Stream+".session-a", nil).raw, &info))
	info.Config["inactive_threshold"] = (2 * time.Hour).Nanoseconds()
	raise := map[string]any{"stream_name": bus.Stream, "config": info.Config, "action": "update"}
	subject := "$JS.API.CONSUMER.CREATE." + bus.Stream + ".session-a"
	// The control: the same update under released is refused.
	operator.refused(t, subject, raise)

	r.setPhase(t, natsgrant.OperatorPhasePrepare)
	seen := r.decisions.count(operatorName)
	operator.reconnect(t)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhasePrepare)
	succeeds(t, operator.Conn, subject, raise)
	consumer, err := js.ConsumerInfo(bus.Stream, "session-a")
	require.NoError(t, err)
	require.Equal(t, 2*time.Hour, consumer.Config.InactiveThreshold, "the prepare operator's update did not raise the threshold")

	operator.refused(t, "$JS.API.STREAM.DELETE."+kvBucket(bus.SessionBucket), nil)
	operator.refused(t, "$JS.API.CONSUMER.DELETE."+bus.Stream+".x", nil)
	operator.refused(t, "$JS.API.STREAM.LIST", nil)
}

// The operator's grant while a hold is set: KV_envoy_sessions is snapshotted, deleted, created,
// updated, deleted again and restored from that snapshot with every message; ENVOY_MEASURE is
// created; the stream and consumer lists the writer census reads are answered; consumer-names
// cleanup deletes a retired consumer; and a create of a stream the grant does not name, the unnamed
// consumer create and a filtered consumer create on ENVOY_NOTIFICATIONS each get no answer and a
// permissions violation. The user expires at most 10 minutes after its decision, and a held
// connection is closed at its expiry: its reconnect after the row is written released is decided
// with released's grant alone.
//
// Before the row turns held, the same snapshot is refused under released and under prepare, and
// the create of KV_envoy_sessions, the create of ENVOY_MEASURE, the cleanup's delete and the stream
// list are each refused under prepare: each row's control is its step under the phase before it.
// Run by hand, a grant holding $JS.API.STREAM.CREATE.> creates KV_other and fails the test naming
// it, and a grant without the restore's chunk subject refuses the first chunk and fails it naming
// the deliver subject.
func TestTheHeldOperatorGetsTheFullList(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseReleased)
	adminNC, js := r.admin(t)
	r.makeStreams(t, 20, 1024, 0)
	// Durable: a consumer named but not durable is removed after five seconds with no interest,
	// before the cleanup below reaches it.
	_, err := js.AddConsumer(bus.Stream, &nats.ConsumerConfig{Durable: "retired-1", AckPolicy: nats.AckExplicitPolicy})
	require.NoError(t, err)
	sessions := kvBucket(bus.SessionBucket)
	created := streamConfig(t, adminNC, sessions)
	measure := map[string]any{"name": "ENVOY_MEASURE", "subjects": []string{"envoy.measure.>"}, "storage": "file", "num_replicas": 1}

	// The first token lives a few seconds, so the held user's expiry closes the connection below;
	// every later one an hour.
	reader := &tokenLives{account: operatorAccount, lives: []time.Duration{time.Hour, time.Hour, time.Hour, 4 * time.Second, time.Hour}}
	operator := r.connectOperator(t, reader)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseReleased)
	snapshot := map[string]any{"deliver_subject": operator.NewRespInbox(), "no_consumers": true, "chunk_size": snapshotChunk}
	operator.refused(t, "$JS.API.STREAM.SNAPSHOT."+sessions, snapshot)

	r.setPhase(t, natsgrant.OperatorPhasePrepare)
	seen := r.decisions.count(operatorName)
	operator.reconnect(t)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhasePrepare)
	operator.refused(t, "$JS.API.STREAM.SNAPSHOT."+sessions, snapshot)
	operator.refused(t, "$JS.API.STREAM.CREATE."+sessions, created)
	operator.refused(t, "$JS.API.STREAM.CREATE.ENVOY_MEASURE", measure)
	operator.refused(t, "$JS.API.CONSUMER.DELETE."+bus.Stream+".retired-1", nil)
	operator.refused(t, "$JS.API.STREAM.LIST", nil)

	r.setPhase(t, natsgrant.OperatorPhaseHeld)
	seen = r.decisions.count(operatorName)
	operator.reconnect(t)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhaseHeld)

	backup, err := backupStream(operator.Conn, sessions)
	require.NoError(t, err, "the held operator's snapshot of %s; the server sent:\n%s", sessions, operator.errs)
	require.EqualValues(t, 20, backup.messages)
	succeeds(t, operator.Conn, "$JS.API.STREAM.CREATE.ENVOY_MEASURE", measure)
	succeeds(t, operator.Conn, "$JS.API.STREAM.DELETE."+sessions, nil)
	succeeds(t, operator.Conn, "$JS.API.STREAM.CREATE."+sessions, created)
	updated := streamConfig(t, operator.Conn, sessions)
	updated["description"] = "updated under the hold"
	succeeds(t, operator.Conn, "$JS.API.STREAM.UPDATE."+sessions, updated)
	succeeds(t, operator.Conn, "$JS.API.STREAM.DELETE."+sessions, nil)
	restored, err := restoreStream(t.Context(), operator.Conn, backup, nil)
	require.NoError(t, err, "the held operator's restore of %s; the server sent:\n%s", sessions, operator.errs)
	require.EqualValues(t, 20, restored)
	info, err := js.StreamInfo(sessions)
	require.NoError(t, err)
	require.EqualValues(t, 20, info.State.Msgs, "the restored %s lost messages", sessions)

	succeeds(t, operator.Conn, "$JS.API.STREAM.LIST", nil)
	var names struct {
		Streams []string `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(succeeds(t, operator.Conn, "$JS.API.STREAM.NAMES", nil).raw, &names))
	require.Contains(t, names.Streams, sessions)
	succeeds(t, operator.Conn, "$JS.API.CONSUMER.LIST."+bus.Stream, nil)
	succeeds(t, operator.Conn, "$JS.API.CONSUMER.DELETE."+bus.Stream+".retired-1", nil)
	_, err = js.ConsumerInfo(bus.Stream, "retired-1")
	require.ErrorIs(t, err, nats.ErrConsumerNotFound, "consumer-names cleanup left the retired consumer")

	operator.refused(t, "$JS.API.STREAM.CREATE.KV_other", map[string]any{"name": "KV_other", "subjects": []string{"$KV.other.>"}})
	_, err = js.StreamInfo("KV_other")
	require.ErrorIs(t, err, nats.ErrStreamNotFound, "the held operator created KV_other")
	consumer := map[string]any{"stream_name": bus.Stream, "config": map[string]any{"ack_policy": "explicit"}}
	operator.refused(t, "$JS.API.CONSUMER.CREATE."+bus.Stream, consumer)
	operator.refused(t, "$JS.API.CONSUMER.CREATE."+bus.Stream+".filtered.notifications.x",
		map[string]any{"stream_name": bus.Stream, "config": map[string]any{"name": "filtered", "filter_subject": "notifications.x", "ack_policy": "explicit"}})

	// The held user lives no longer than its token: the next decision's token lives seconds, and the
	// server closes the connection at its expiry. Once the row is released, the reconnect that
	// follows is decided with released's grant alone.
	seen = r.decisions.count(operatorName)
	operator.reconnect(t)
	short := r.decisions.after(t, operatorName, seen)
	requireOperatorDecision(t, short, natsgrant.OperatorPhaseHeld)
	require.Less(t, short.Expires.Sub(short.done), 5*time.Second, "the held user outlived its token")
	r.setPhase(t, natsgrant.OperatorPhaseReleased)
	drain(operator.reconnected)
	select {
	case <-operator.reconnected:
	case <-time.After(15 * time.Second):
		t.Fatal("the held connection was not closed and reconnected at its user's expiry")
	}
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen+1), natsgrant.OperatorPhaseReleased)
	operator.refused(t, "$JS.API.STREAM.LIST", nil)
}

// restoreStall is how long nats-server waits for a restore's next chunk before it ends the restore
// as stalled (v2.15.1 jetstream_api.go:4613, activityInterval).
const restoreStall = 5 * time.Second

// restoreCommand is the held restore as the operator's command runs it on the clustered hub:
// before each attempt a forced reconnect, so the attempt starts on a decision less than a second
// old, and a wait for the reconnect; an attempt that the connection's close cuts short is waited
// out past the server's stall, its leftover stream is deleted (deleteCutRestore), and the restore
// runs again from its first chunk, attempts times at most. After the last attempt's cut it deletes
// the leftover too, so a stopped command leaves no stream, and answers an error naming the bucket
// and every attempt's error.
func restoreCommand(t *testing.T, o *operatorConn, b streamBackup, attempts int, paced func()) error {
	t.Helper()
	var failures []string
	for range attempts {
		o.reconnect(t)
		ctx, cancel := context.WithCancel(context.Background())
		stop := make(chan struct{})
		go func() {
			select {
			case <-o.disconnected:
				cancel()
			case <-stop:
			}
		}()
		_, err := restoreStream(ctx, o.Conn, b, paced)
		close(stop)
		cancel()
		if err == nil {
			return nil
		}
		failures = append(failures, err.Error())
		time.Sleep(restoreStall + time.Second)
		testwait.WithinPolling(t, 15*time.Second, 50*time.Millisecond, "the connection after the cut", o.IsConnected)
		deleteCutRestore(t, o, b.stream)
	}
	return fmt.Errorf("restore of %s stopped after %d closes, with the hold still set: %s", b.stream, len(failures), strings.Join(failures, "; "))
}

// deleteCutRestore deletes the stream a cut restore leaves on the clustered hub, through the held
// grant's STREAM.DELETE of the bucket, and requires it gone after.
//
// A cut restore is no absence there. Measured on this package's three members of nats-server
// v2.15.1, with a three-copy bucket's restore cut after three of eleven chunks: the restoring
// member logs "Stream restore failed ... is stalled"; STREAM.INFO answers 10059, stream not found,
// for about ten seconds, and then an empty three-copy stream of that name appears and stays; and
// a second restore is refused 10130, "stream name already in use, cannot restore". The meta
// leader keeps a failed restore's assignment (jetstream_cluster.go:9276-9278: "Leave failed
// restore assignments for explicit cleanup"), and the members other than the restoring one built
// the stream from it. A delete of that name answers success, and the next restore restores every
// message. So the command deletes the name, rather than read an absence that does not last.
func deleteCutRestore(t *testing.T, o *operatorConn, stream string) {
	t.Helper()
	answer := answered(t, o.Conn, "$JS.API.STREAM.DELETE."+stream, nil)
	if answer.Error != nil {
		require.Equal(t, 10059, answer.Error.ErrCode, "the delete of %s's cut restore: %s", stream, answer.raw)
	}
	gone := answered(t, o.Conn, "$JS.API.STREAM.DELETE."+stream, nil)
	require.NotNil(t, gone.Error, "%s was still there after its cut restore's delete: %s", stream, gone.raw)
	require.Equal(t, 10059, gone.Error.ErrCode, "%s after its cut restore's delete: %s", stream, gone.raw)
}

// A held restore survives one close of its connection: with the held user living a few seconds
// and the chunks paced so a restore takes about five, the first attempt is cut by the server
// closing the connection at the user's expiry; once the server has ended that restore as stalled,
// the stream it leaves is deleted (deleteCutRestore) and is gone, and the second attempt, on the
// reconnect's new decision under held, restores every message. A second close stops the command,
// naming the bucket and both errors, and the stream its cut leaves is deleted and does not come
// back.
//
// Run by hand, the control is the restore without its retry (one attempt): the stream is missing
// after the cut, and the test fails naming the bucket.
func TestARestoreSurvivesOneReconnect(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseHeld)
	_, js := r.admin(t)
	r.makeStreams(t, 40, 16*1024, 0)
	sessions := kvBucket(bus.SessionBucket)
	paced := func() { time.Sleep(500 * time.Millisecond) }

	// Reads: the first connect, the first attempt's forced reconnect (3 s), the reconnect after its
	// cut, the second attempt's forced reconnect: an hour each but the second.
	reader := &tokenLives{account: operatorAccount, lives: []time.Duration{time.Hour, 3 * time.Second, time.Hour, time.Hour}}
	operator := r.connectOperator(t, reader)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseHeld)
	backup, err := backupStream(operator.Conn, sessions)
	require.NoError(t, err)
	require.EqualValues(t, 40, backup.messages)
	require.Greater(t, len(backup.data), 6*restoreChunk, "the backup is too small for the paced restore to outlive the held user")

	succeeds(t, operator.Conn, "$JS.API.STREAM.DELETE."+sessions, nil)
	seen := r.decisions.count(operatorName)
	require.NoError(t, restoreCommand(t, operator, backup, 2, paced), "the restore did not survive one close")
	require.GreaterOrEqual(t, r.decisions.count(operatorName)-seen, 3, "the restore ran again on no new decision")
	for _, decision := range r.decisions.since(operatorName, seen) {
		require.Equal(t, natsgrant.Operator(natsgrant.OperatorPhaseHeld), decision.Grant, "a reconnect of the restore was not given the held grant")
	}
	info, err := js.StreamInfo(sessions)
	require.NoError(t, err, "%s is missing after the restore", sessions)
	require.EqualValues(t, 40, info.State.Msgs, "the restored %s lost messages", sessions)

	t.Run("a second close stops the command", func(t *testing.T) {
		succeeds(t, operator.Conn, "$JS.API.STREAM.DELETE."+sessions, nil)
		reader.mu.Lock()
		reader.lives, reader.reads = []time.Duration{3 * time.Second, time.Hour, 3 * time.Second, time.Hour}, 0
		reader.mu.Unlock()
		err := restoreCommand(t, operator, backup, 2, paced)
		require.Error(t, err, "the restore completed through two closes")
		require.Contains(t, err.Error(), sessions)
		require.Contains(t, err.Error(), "stopped after 2 closes")
		require.Equal(t, 2, strings.Count(err.Error(), "restore "+sessions), "the command did not name both errors: %v", err)
		// The empty stream a cut restore leaves appeared about ten seconds after the stall when
		// nothing deleted it; this waits past that.
		require.Never(t, func() bool {
			_, err := js.StreamInfo(sessions)
			return !errors.Is(err, nats.ErrStreamNotFound)
		}, 12*time.Second, 500*time.Millisecond, "the stopped restore left %s", sessions)
	})
}

// The hold's barrier is a stream: with ENVOY_NOTIFICATIONS at its consumer cap, where an unnamed
// consumer's create is refused with err_code 10026 (so a barrier consumer could never be made),
// the held operator creates the stream natsgrant.HoldBarrierStream names with the hold's settings,
// finds it in STREAM.NAMES and deletes it; a create of any other stream name is refused with a
// permissions violation.
//
// It fails when held's grant loses the barrier stream's create or delete, or when it admits a
// create of a stream name it does not name (the control row, in the test).
func TestTheBarrierIsAStream(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseHeld)
	adminNC, js := r.admin(t)
	r.makeStreams(t, 1, 16, 2)
	for _, name := range []string{"session-a", "session-b"} {
		_, err := js.AddConsumer(bus.Stream, &nats.ConsumerConfig{Durable: name, AckPolicy: nats.AckExplicitPolicy})
		require.NoError(t, err)
	}
	full := answered(t, adminNC, "$JS.API.CONSUMER.CREATE."+bus.Stream,
		map[string]any{"stream_name": bus.Stream, "config": map[string]any{"ack_policy": "explicit"}})
	require.NotNil(t, full.Error, "an unnamed consumer was created at the consumer cap: %s", full.raw)
	require.Equal(t, 10026, full.Error.ErrCode, "the unnamed create at the cap: %s", full.raw)

	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseHeld)
	barrier := map[string]any{"name": natsgrant.HoldBarrierStream, "subjects": []string{"_ENVOY_HOLD_BARRIER.hold-1"},
		"storage": "memory", "num_replicas": 1, "max_msgs": 1}
	succeeds(t, operator.Conn, "$JS.API.STREAM.CREATE."+natsgrant.HoldBarrierStream, barrier)
	var names struct {
		Streams []string `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(succeeds(t, operator.Conn, "$JS.API.STREAM.NAMES", nil).raw, &names))
	require.Contains(t, names.Streams, natsgrant.HoldBarrierStream)
	succeeds(t, operator.Conn, "$JS.API.STREAM.DELETE."+natsgrant.HoldBarrierStream, nil)
	_, err := js.StreamInfo(natsgrant.HoldBarrierStream)
	require.ErrorIs(t, err, nats.ErrStreamNotFound)

	other := map[string]any{"name": "ENVOY_HOLD_OTHER", "subjects": []string{"_ENVOY_HOLD_OTHER.hold-1"}, "storage": "memory", "num_replicas": 1}
	operator.refused(t, "$JS.API.STREAM.CREATE.ENVOY_HOLD_OTHER", other)
}

// The infos of the protected buckets that are not copied, KV_legion_agents, KV_envoy_dpop_proofs
// and KV_envoy_webhook_redelivery, which the hold's preflight reads before the row turns held, are
// answered under prepare and under held, and refused under released.
//
// Run by hand, the control is prepare's grant without them, as it was before they were added: the
// info of KV_legion_agents is refused under prepare, and the test fails naming the stream.
func TestThePrepareOperatorReadsEveryProtectedStream(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseReleased)
	r.makeStreams(t, 1, 16, 0)
	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseReleased)
	for _, bucket := range otherProtected {
		operator.refused(t, "$JS.API.STREAM.INFO."+kvBucket(bucket), nil)
	}
	for _, phase := range []natsgrant.OperatorPhase{natsgrant.OperatorPhasePrepare, natsgrant.OperatorPhaseHeld} {
		r.setPhase(t, phase)
		seen := r.decisions.count(operatorName)
		operator.reconnect(t)
		requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), phase)
		for _, bucket := range otherProtected {
			msg, err := operator.Request("$JS.API.STREAM.INFO."+kvBucket(bucket), nil, 5*time.Second)
			require.NoError(t, err, "the %s operator's info of %s got no answer", phase, kvBucket(bucket))
			var answer apiAnswer
			require.NoError(t, json.Unmarshal(msg.Data, &answer))
			require.Nil(t, answer.Error, "the %s operator's info of %s: %s", phase, kvBucket(bucket), msg.Data)
		}
	}
}

// With the hold row unknown, the operator is admitted with released's grant, its user expiring at
// most 10 minutes after the decision, so the operator can always reach the broker's routes and
// restore the row; a listener on the same replica is refused hold_unknown at the same time.
//
// Run by hand, the control is the rule that refused the operator with hold_unknown too: the
// operator's connect fails, and the test fails naming it.
func TestTheOperatorIsAdmittedWhenTheHoldCannotBeRead(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.hold.set(holdAnswer{err: fmt.Errorf("natshold: read the hold row: %w", context.DeadlineExceeded)})
	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseReleased)
	succeeds(t, operator.Conn, "$JS.API.INFO", nil)
	require.Equal(t, "1", holdUnknownGauge(t, r.registry))

	err := r.listen()
	require.ErrorIs(t, err, nats.ErrAuthorization, "a listener was admitted while the hold was unknown")
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the listener's refusal counted hold_unknown", func() bool {
		return refusedFor(t, r.registry, labelHoldUnknown) == 1
	})
}

// While the row holds held, only the operator entry is answered with the operator's grant: a
// listener and a session are each decided with their own grant, and the listener's delete of
// KV_envoy_sessions gets a permissions violation and no answer.
//
// Run by hand, the control decides every service entry as the operator: the listener's delete is
// answered, and the test fails naming its account.
func TestNoOneButTheOperatorGetsTheOperatorGrant(t *testing.T) {
	a := newSession(t, "ses_a", 15*time.Minute)
	r := newOperatorRig(t, nil, a)
	r.setPhase(t, natsgrant.OperatorPhaseHeld)
	r.makeStreams(t, 1, 16, 0)
	listener := connectService(t, r.cluster.URL(), hourLong(listenerAccount), "")
	decided := r.decisions.after(t, listenerName, 0)
	require.Equal(t, natsgrant.ForCallout(natsgrant.Listener("example-hub")), decided.Grant, "the listener %s was not decided with its own grant", listenerAccount)
	listener.refused(t, "$JS.API.STREAM.DELETE."+kvBucket(bus.SessionBucket), nil)

	_, err := connectSession(t, r.cluster.URL(), a, nil)
	require.NoError(t, err)
	session := r.decisions.after(t, "session/ses_a", 0)
	require.Equal(t, must(natsgrant.Session("ses_a")), session.Grant)
	require.Zero(t, r.decisions.count(operatorName), "a client other than the operator was decided as the operator")
}

// A failed read is unknown at once, whatever the replica read before: a replica that read prepare,
// whose next read times out after the operator wrote held, refuses a listener hold_unknown. A
// replica that kept prepare would admit a writer after the hold's census, since the server never
// asks again about a connection it admitted.
//
// Run by hand, the control is a view that keeps its last reading for a minute: the listener is
// admitted, and the test fails naming it.
func TestAFailedReadRefusesOrdinaryClientsAtOnce(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhasePrepare)
	listener := connectService(t, r.cluster.URL(), hourLong(listenerAccount), "")
	r.decisions.after(t, listenerName, 0)
	listener.Close()

	// The operator writes held; this replica's next read times out, as natshold.Read's does past
	// its deadline.
	r.hold.set(holdAnswer{err: fmt.Errorf("natshold: read the hold row: %w", context.DeadlineExceeded)})
	err := r.listen()
	require.ErrorIs(t, err, nats.ErrAuthorization, "the listener %s was admitted on a replica whose read failed after prepare", listenerAccount)
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the listener's refusal counted hold_unknown", func() bool {
		return refusedFor(t, r.registry, labelHoldUnknown) == 1
	})
	require.Equal(t, 1, r.decisions.count(listenerName))
}

// tE3Rows are t-e-3's decisions, each with the answer its tests require: a session's own grant,
// each listed service kind's grant, and each refusal's label.
type tE3Row struct {
	name  string
	req   Request
	want  Decision
	label string
}

func tE3Rows(t *testing.T, now time.Time, a *session) []tE3Row {
	t.Helper()
	return []tE3Row{
		{"a session", a.request(t, "nonce"), Decision{Name: "session/ses_a", Grant: must(natsgrant.Session("ses_a")), Expires: time.Unix(a.answer.ExpiresAt, 0)}, ""},
		{"dispatch", serviceRequest("dispatch-token"), Decision{Name: "dispatch/example-dispatch", Grant: natsgrant.ForCallout(natsgrant.Dispatch("example-dispatch")), Expires: now.Add(time.Hour)}, ""},
		{"a listener", serviceRequest("listener-token"), Decision{Name: listenerName, Grant: natsgrant.ForCallout(natsgrant.Listener("example-hub")), Expires: now.Add(time.Hour)}, ""},
		{"a daemon", serviceRequest("daemon-token"), Decision{Name: "daemon/" + daemonAccount, Grant: natsgrant.ForCallout(natsgrant.Daemon()), Expires: now.Add(30 * time.Minute)}, ""},
		{"measure", serviceRequest("measure-token"), Decision{Name: "measure/" + measureAccount, Grant: natsgrant.ForCallout(natsgrant.Measure()), Expires: now.Add(time.Hour)}, ""},
		{"no credential", Request{UserNkey: "UUSER", ServerID: "NSERVER"}, Decision{}, labelNoCredential},
		{"an unlisted service", serviceRequest("unlisted-token"), Decision{}, labelServiceNotListed},
		{"a two-hour token", serviceRequest("two-hours-token"), Decision{}, labelTokenTooLong},
		{"a service held to projects", serviceRequest("held-token"), Decision{}, labelHeldToProjects},
	}
}

// tE3Deps are the decision tests' broker answers, for the services tE3Rows names.
func tE3Deps(now time.Time, a *session) Deps {
	return Deps{Introspect: introspector(a), ServiceToken: serviceTokens(map[string]platformToken{
		"dispatch-token":  {account: "system:serviceaccount:dispatch:dispatch", issued: now, expiry: now.Add(time.Hour)},
		"listener-token":  {account: listenerAccount, issued: now, expiry: now.Add(time.Hour)},
		"daemon-token":    {account: daemonAccount, issued: now, expiry: now.Add(30 * time.Minute)},
		"measure-token":   {account: measureAccount, google: true, issued: now, expiry: now.Add(time.Hour)},
		"unlisted-token":  {account: "system:serviceaccount:example-ns:stranger", issued: now, expiry: now.Add(time.Hour)},
		"two-hours-token": {account: daemonAccount, issued: now, expiry: now.Add(2 * time.Hour)},
		"held-token":      {account: "system:serviceaccount:example-ns:held-daemon", issued: now, expiry: now.Add(time.Hour)},
	})}
}

// requireAsBefore requires each of rows to be answered, under deps and services, as t-e-3's tests
// require.
func requireAsBefore(t *testing.T, deps Deps, services []ServiceEntry, rows []tE3Row) {
	t.Helper()
	for _, row := range rows {
		decision, err := Decide(t.Context(), deps, services, row.req)
		if row.label == "" {
			require.NoError(t, err, "%s was refused", row.name)
			require.Equal(t, row.want, decision, "%s was not answered as before", row.name)
			continue
		}
		var refused *Refusal
		require.True(t, errors.As(err, &refused), "%s was admitted: %+v", row.name, decision)
		require.Equal(t, row.label, refused.Label, "%s", row.name)
	}
}

// With BROKER_NATS_HOLD_READ false the broker passes no HoldRead, and the hold rule is inert: every
// decision of t-e-3's decision tests is answered as before, with the operator and the probe listed
// beside them and the operator's signing keys given, and the operator is answered with released's
// grant. (The broker's refusal to start with the setting on and no operator entry is its command's
// test.)
//
// Run by hand, the control is the rule active without its setting, a broker that reads no row as
// unknown: a listener is refused hold_unknown, and the test fails naming it.
func TestTheHoldRuleIsInertWithoutItsSetting(t *testing.T) {
	now := time.Now()
	a := newSession(t, "ses_a", 15*time.Minute)
	deps := tE3Deps(now, a)
	deps.HoldKeys = newHoldSigner(t).HoldKeys
	deps.Now = func() time.Time { return now }
	requireAsBefore(t, deps, holdServices, tE3Rows(t, now, a))
	operator, err := Decide(t.Context(), Deps{ServiceToken: serviceTokens(map[string]platformToken{
		"operator-token": {account: operatorAccount, issued: now, expiry: now.Add(time.Hour)},
	}), Now: deps.Now}, holdServices, serviceRequest("operator-token"))
	require.NoError(t, err)
	require.Equal(t, natsgrant.Operator(natsgrant.OperatorPhaseReleased), operator.Grant)
}

// With no operator entry nothing changes: every decision of t-e-3's decision tests is answered as
// before, with HoldRead nil, which reads nothing, and with a HoldRead answering held, which no
// listed entry's grant follows.
//
// Run by hand, the control is a decision path that decides a listed daemon entry as the operator:
// the daemon row fails, naming it.
func TestWithNoOperatorEntryNothingChanges(t *testing.T) {
	now := time.Now()
	a := newSession(t, "ses_a", 15*time.Minute)
	rows := tE3Rows(t, now, a)
	requireAsBefore(t, tE3Deps(now, a), exampleServices, rows)
	signer := newHoldSigner(t)
	stub := &holdStub{}
	stub.set(rowOf(signer.reading(t, 3, natsgrant.OperatorPhaseHeld, "hold-1")))
	held := tE3Deps(now, a)
	held.HoldRead, held.HoldKeys = stub.read, signer.HoldKeys
	requireAsBefore(t, held, exampleServices, rows)
}

// outcomes counts connections the storm made, refused and admitted, from a moment the test marks.
type outcomes struct {
	admitted, refused atomic.Int64
}

// Postgres stopped refuses every client but the operator, and only while it is down. During a
// storm of listener and session connections, the hold's Postgres goes away: every client that
// connects while it is down is refused hold_unknown, the operator is admitted with released's
// grant, its user expiring at most 10 minutes after the decision, and once Postgres answers again
// every client is admitted from the first decision that reads it.
//
// Run by hand, the control is a view that answers the last phase on an error: a listener is
// admitted during the outage, and the test fails naming it.
func TestAPostgresOutageRefusesOnlyWhileItLasts(t *testing.T) {
	pool, direct, proxy, schema := proxiedHoldPool(t)
	table := holdTable{pool: direct, signer: newHoldSigner(t)}
	a := newSession(t, "ses_a", time.Hour)
	read := func(ctx context.Context) (natshold.Reading, bool, error) { return natshold.Read(ctx, pool, schema) }
	r := newOperatorRigSigned(t, read, table.signer, a)
	table.write(t, 1, natsgrant.OperatorPhaseReleased, "hold-1")

	var phase atomic.Int32 // 0 before the outage, 1 during it, 2 after it
	var during, after outcomes
	stop := make(chan struct{})
	var storm sync.WaitGroup
	for worker := range 4 {
		storm.Add(1)
		go func() {
			defer storm.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				started := phase.Load()
				var err error
				if worker%2 == 0 {
					err = r.listen()
				} else {
					var nc *nats.Conn
					nc, err = connectSessionOnce(r.cluster.URL(), a)
					if err == nil {
						nc.Close()
					}
				}
				ended := phase.Load()
				if started != ended {
					continue
				}
				var counted *outcomes
				switch ended {
				case 1:
					counted = &during
				case 2:
					counted = &after
				default:
					continue
				}
				if err == nil {
					counted.admitted.Add(1)
				} else {
					counted.refused.Add(1)
				}
			}
		}()
	}
	defer func() { close(stop); storm.Wait() }()

	time.Sleep(time.Second)
	proxy.Cut()
	// The outage is marked by Postgres's own answer through the relay, not by the view's gauge: a
	// view that hid the outage would hide it from the gauge too.
	testwait.WithinPolling(t, 10*time.Second, 50*time.Millisecond, "Postgres unreachable through the cut relay", func() bool {
		return postgresAnswers(proxy.URL()) != nil
	})
	phase.Store(1)
	seen := r.decisions.count(operatorName)
	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhaseReleased)
	operator.Close()
	testwait.WithinPolling(t, 15*time.Second, 50*time.Millisecond, "connections during the outage", func() bool {
		return during.refused.Load()+during.admitted.Load() >= 8
	})
	phase.Store(3)
	require.Zero(t, during.admitted.Load(), "a listener or a session was admitted while the hold's Postgres was down (%d refused)", during.refused.Load())
	require.Positive(t, refusedFor(t, r.registry, labelHoldUnknown), "no refusal was counted hold_unknown")
	require.Equal(t, "1", holdUnknownGauge(t, r.registry), "envoy_broker_hold_unknown during the outage")

	proxy.Restore()
	testwait.WithinPolling(t, 10*time.Second, 50*time.Millisecond, "Postgres answering through the restored relay", func() bool {
		return postgresAnswers(proxy.URL()) == nil
	})
	phase.Store(2)
	testwait.WithinPolling(t, 15*time.Second, 50*time.Millisecond, "connections admitted after the outage", func() bool {
		return after.admitted.Load() >= 8
	})
	phase.Store(3)
	require.Zero(t, after.refused.Load(), "a client was refused after the hold's Postgres answered again (%d admitted)", after.admitted.Load())
	require.Equal(t, "0", holdUnknownGauge(t, r.registry), "envoy_broker_hold_unknown after the outage")
}

// postgresAnswers connects to dsn on a connection of its own and pings it, within a second.
func postgresAnswers(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return conn.Ping(ctx)
}

// connectSessionOnce connects to url as s with no reconnect, outside a test's goroutine.
func connectSessionOnce(url string, s *session) (*nats.Conn, error) {
	sign := func(nonce []byte) ([]byte, error) {
		proof, err := cipherclient.ConnectionProof(s.key, s.token, string(nonce))
		return []byte(proof), err
	}
	return nats.Connect(url, nats.UserJWT(func() (string, error) { return "-", nil }, sign), nats.Token(s.token),
		nats.Timeout(5*time.Second), nats.NoReconnect())
}

// The operator sets held; the test's admin deletes the row: a listener is then refused
// hold_unknown, and the operator is admitted with released's grant. The operator restores the row
// as held, at a gen above every logged one: the row is known again, and the operator is answered
// with held's grant. The operator writes released, after which the listener is admitted. The
// operator's writes are the signed rows the broker's routes store, written here by SQL.
//
// Run by hand, the control reads a missing row as released: the listener is admitted after the
// delete, and the test fails naming it.
func TestAMissingHoldRowFailsClosed(t *testing.T) {
	pool, schema := holdPool(t)
	table := holdTable{pool: pool, signer: newHoldSigner(t)}
	read := func(ctx context.Context) (natshold.Reading, bool, error) { return natshold.Read(ctx, pool, schema) }
	r := newOperatorRigSigned(t, read, table.signer)
	table.write(t, 1, natsgrant.OperatorPhaseReleased, "hold-1")
	table.write(t, 2, natsgrant.OperatorPhasePrepare, "hold-1")
	table.write(t, 3, natsgrant.OperatorPhaseHeld, "hold-1")
	listen := r.listen
	require.NoError(t, listen(), "a listener was refused under held, which refuses no other client in this task")

	table.admin(t, `delete from nats_hold`)
	require.ErrorIs(t, listen(), nats.ErrAuthorization, "the listener %s was admitted after the row was deleted", listenerAccount)
	require.Equal(t, 1, refusedFor(t, r.registry, labelHoldUnknown))
	seen := r.decisions.count(operatorName)
	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhaseReleased)

	table.write(t, 4, natsgrant.OperatorPhaseHeld, "hold-1")
	seen = r.decisions.count(operatorName)
	operator.reconnect(t)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhaseHeld)

	table.write(t, 5, natsgrant.OperatorPhaseReleased, "hold-1")
	require.NoError(t, listen(), "the listener was refused after the operator wrote released")
	require.Equal(t, 1, refusedFor(t, r.registry, labelHoldUnknown))
}

// newOperatorRigSigned is newOperatorRig with signer the operator's key its rows are signed with.
func newOperatorRigSigned(t *testing.T, read func(context.Context) (natshold.Reading, bool, error), signer holdSigner, sessions ...*session) *operatorRig {
	t.Helper()
	keys := testnats.NewCalloutKeys(t)
	adminSeed, adminKey := testnats.User(t)
	r := &operatorRig{keys: keys, adminSeed: adminSeed, signer: signer, hold: &holdStub{}, decisions: newDecisionLog(), registry: metrics.New()}
	r.cluster = testnats.StartClusterWithCallout(t, 3, keys, testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}})
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		// A JetStream answer that names a stream missing which exists comes from a meta leader that
		// has not applied the stream's assignment yet: each member's leader changes say whether one
		// came during the test.
		for i, server := range r.cluster.Servers {
			for _, line := range strings.Split(server.Log(), "\n") {
				if strings.Contains(line, "leader") {
					t.Logf("member %s: %s", r.cluster.Name(i), line)
				}
			}
		}
	})
	if read == nil {
		read = r.hold.read
	}
	deps := Deps{Introspect: introspector(sessions...), ServiceToken: readUnsignedToken, HoldRead: read, HoldKeys: signer.HoldKeys,
		Metrics: NewMetrics(r.registry)}
	r.service = startReplica(t, r.cluster.URLs(), keys, holdServices, deps, r.decisions.wrap(Decide))
	return r
}
