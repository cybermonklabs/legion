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

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// startReplica runs one auth service replica against the servers at urls with keys' seeds,
// admitting services, deciding with decide, stopped when the test ends, and returns once its
// subscription has reached the hub.
func startReplica(t *testing.T, urls []string, keys testnats.CalloutKeys, services []config.NATSService, deps Deps, decide decideFunc) *Service {
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
// token names (unsignedServiceToken), read without verifying it, from the one issuer of a bare
// BROKER_K8S_OIDC_ISSUER URL, whose name is "".
func readUnsignedToken(_ context.Context, raw string) (ServiceIdentity, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ServiceIdentity{}, errors.New("the token is no JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ServiceIdentity{}, err
	}
	var claims struct {
		Sub      string `json:"sub"`
		Iat, Exp int64
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ServiceIdentity{}, err
	}
	return ServiceIdentity{Account: claims.Sub, Issued: time.Unix(claims.Iat, 0), Expiry: time.Unix(claims.Exp, 0)}, nil
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
	return func(ctx context.Context, deps Deps, services []config.NATSService, req Request) (Decision, error) {
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

// latest is the last decision admitting name, the zero decided when there is none.
func (l *decisionLog) latest(name string) decided {
	l.mu.Lock()
	defer l.mu.Unlock()
	if decisions := l.byName[name]; len(decisions) > 0 {
		return decisions[len(decisions)-1]
	}
	return decided{}
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
// decisions, the operator's hold signing key and a hold view whose read the test sets, and an
// admin static user that makes the streams the deployment's programs make.
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

// newOperatorRig starts the rig with a hold signing key of its own. read is the replica's hold
// view's read; nil is the rig's own stub, whose row setPhase writes. sessions are the sessions the
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
// JetStream context. Each create is sent until the cluster answers it (testnats.SendCreate): a
// create can land while its reply is lost, and the same create sent again is answered with the
// stream it made.
func (r *operatorRig) makeStreams(t *testing.T, entries, entryBytes, maxConsumers int) nats.JetStreamContext {
	t.Helper()
	admin, js := r.admin(t)
	for _, bucket := range slices.Concat(copiedBuckets, []string{sessionConsumers}, otherProtected) {
		replicas := 1
		if bucket == bus.SessionBucket {
			replicas = 3
		}
		kv := testnats.CreateKeyValueOn(t, admin, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: replicas})
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
	testnats.CreateStreamOn(t, admin, nats.StreamConfig{Name: bus.Stream, Subjects: []string{"notifications.>"}, Storage: nats.FileStorage, MaxConsumers: maxConsumers})
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

// requestAnswer sends one request and returns its decoded answer. A caller that can safely retry
// a request after its reply is lost reads the timeout rather than failing here.
func requestAnswer(t *testing.T, nc *nats.Conn, subject string, body any) (apiAnswer, error) {
	t.Helper()
	msg, err := nc.Request(subject, encoded(t, body), answerWait)
	if err != nil {
		return apiAnswer{}, err
	}
	answer := apiAnswer{raw: msg.Data}
	require.NoError(t, json.Unmarshal(msg.Data, &answer), "%s answered %q", subject, msg.Data)
	return answer, nil
}

// answered requires a request on subject to be answered, and returns the answer.
func answered(t *testing.T, nc *nats.Conn, subject string, body any) apiAnswer {
	t.Helper()
	answer, err := requestAnswer(t, nc, subject, body)
	require.NoError(t, err, "%s got no answer", subject)
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

type streamRequester func(t *testing.T, nc *nats.Conn, subject string, body any) (apiAnswer, error)

// createsStream creates config's named stream with a raw create on subject, sent on nc until the
// cluster answers it (testnats.SendCreate). A create can land while its reply is lost, and the
// same create sent again is answered with the stream it made. Any refusal fails the test at once
// and names it: an answer that carries an error, a name in use with other settings among them,
// and a create nc's grant does not cover, which draws no answer and which the server reports on
// nc. The held operator's grant names the barrier's create and delete and no read of it, so the
// answer alone decides.
func createsStream(t *testing.T, nc *nats.Conn, subject string, config map[string]any) {
	t.Helper()
	createsStreamWith(t, nc, subject, config, requestAnswer)
}

// createsStreamWith is createsStream with the request function the test uses. Its caller can make
// a reply unavailable after the server created the stream, the loss the send again recovers.
func createsStreamWith(t *testing.T, nc *nats.Conn, subject string, config map[string]any, ask streamRequester) {
	t.Helper()
	err := testnats.SendCreate(t, nc, subject, answerWait, []string{subject}, func() error {
		answer, err := ask(t, nc, subject, config)
		if err != nil {
			return err
		}
		if answer.Error != nil {
			return fmt.Errorf("answered %s", answer.raw)
		}
		return nil
	})
	require.NoError(t, err, "%s was refused", subject)
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

// streamConfig is stream's configuration as its info answers it, for an update of it.
func streamConfig(t *testing.T, nc *nats.Conn, stream string) map[string]any {
	t.Helper()
	var info struct {
		Config map[string]any `json:"config"`
	}
	require.NoError(t, json.Unmarshal(succeeds(t, nc, "$JS.API.STREAM.INFO."+stream, nil).raw, &info))
	return info.Config
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
	metrics := NewMetrics(r.registry)
	deps := Deps{Introspect: introspector(sessions...), ServiceToken: readUnsignedToken, Metrics: metrics,
		Hold: NewHoldView(read, signer.HoldKeys, nil, metrics)}
	r.service = startReplica(t, r.cluster.URLs(), keys, holdServices, deps, r.decisions.wrap(Decide))
	return r
}
