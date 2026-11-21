package writerhold

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// holdAwareName is the test auth service's connection name, as a hold-aware build names its own.
const holdAwareName = "envoy-auth test hold-aware"

// calloutRig is a three-member hub behind an auth callout whose auth service is the test's own
// (callout): the operator as a static user with the operator's inbox prefix (caller), a static
// listener user, and the auth service's own connection, named as the row asks; with the hub's
// Kubernetes objects over it (kube).
type calloutRig struct {
	cluster  *testnats.Cluster
	kube     *holdtest.Kube
	hub      KubeHub
	keys     testnats.CalloutKeys
	callout  *callout
	service  *natsgo.Conn
	caller   *natsgo.Conn
	callerID ConnRef
	// callerSeed is the operator's seed, listenerSeed the listener's.
	callerSeed, listenerSeed, listenerKey string
}

func newCalloutRig(t *testing.T, serviceName string) *calloutRig {
	t.Helper()
	r := &calloutRig{keys: testnats.NewCalloutKeys(t)}
	var callerKey string
	r.callerSeed, callerKey = testnats.User(t)
	r.listenerSeed, r.listenerKey = testnats.User(t)
	r.cluster = testnats.StartClusterWithCallout(t, 3, r.keys,
		testnats.StaticUser{NKey: callerKey, Publish: []string{"$JS.API.>"}, Subscribe: []string{natsgrant.OperatorInbox + ".>"}},
		testnats.StaticUser{NKey: r.listenerKey, Publish: []string{"listener.>"}, Subscribe: []string{"listener.>"}})
	r.callout = newCallout(t, r.keys)
	r.service = dialSeed(t, r.cluster.URL(), r.keys.ServiceSeed, natsgo.Name(serviceName))
	_, err := r.service.Subscribe(natsserver.AuthCalloutSubject, r.callout.handle)
	require.NoError(t, err)
	require.NoError(t, r.service.Flush())
	r.caller = dialSeed(t, r.cluster.URL(), r.callerSeed, natsgo.Name("operator/holdtest"), natsgo.CustomInboxPrefix(natsgrant.OperatorInbox))
	r.callerID, err = callerOf(r.caller)
	require.NoError(t, err)
	r.kube = holdtest.NewKube(t, r.cluster)
	r.hub = hubOf(r.kube)
	return r
}

// dialSeed connects to url as the nkey user whose seed is seed, reconnecting for as long as the
// test runs, and closes the connection when the test ends.
func dialSeed(t *testing.T, url, seed string, opts ...natsgo.Option) *natsgo.Conn {
	t.Helper()
	user, err := nkeys.FromSeed([]byte(seed))
	require.NoError(t, err)
	public, err := user.PublicKey()
	require.NoError(t, err)
	nc, err := natsgo.Connect(url, append([]natsgo.Option{
		natsgo.Nkey(public, user.Sign), natsgo.Timeout(10 * time.Second),
		natsgo.MaxReconnects(-1), natsgo.ReconnectWait(100 * time.Millisecond),
	}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

// evict evicts pod through the fake API's Eviction subresource, which restarts its member.
func evict(t *testing.T, kube *holdtest.Kube, pod string) {
	t.Helper()
	require.NoError(t, kube.Client.PolicyV1().Evictions(holdtest.Namespace).Evict(rowContext(t), &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: holdtest.Namespace},
	}))
}

// step1ThenRestart is step 1's record of the hub, then every member restarted, as steps 3 and 4
// leave the hub before the report: it returns once every server is routed to the others' new ids
// alone and the caller and the auth service are connected again.
func (r *calloutRig) step1ThenRestart(t *testing.T) Declared {
	t.Helper()
	ctx := rowContext(t)
	declared, err := Discover(ctx, r.hub)
	require.NoError(t, err)
	for _, server := range declared {
		evict(t, r.kube, server.Pod)
	}
	before := map[string]string{}
	for _, server := range declared {
		before[server.Varz.ServerID] = server.Pod
	}
	testwait.Eventually(t, "every member restarted and routed to the others' new ids, the caller and the auth service connected", func() bool {
		reading, err := readServers(ctx, StepReport, r.hub.Monitor, declared.pods())
		if err != nil {
			return false
		}
		defer reading.close()
		for _, server := range declared {
			if reading.varz[server.Pod].ServerID == server.Varz.ServerID {
				return false
			}
		}
		return reading.checkTopology(ctx, StepReport, declared.names(), before) == nil && r.caller.IsConnected() && r.service.IsConnected()
	})
	return declared
}

// report is the report of the rig's hub after step1ThenRestart, with wait between its readings.
func (r *calloutRig) report(declared Declared, wait func(context.Context, time.Duration) error) Report {
	return Report{Hub: r.hub, Declared: declared, AuthUser: r.keys.ServicePublic, Wait: wait}
}

// member is the index of the member the connection nc is connected to.
func (r *calloutRig) member(t *testing.T, nc *natsgo.Conn) int {
	t.Helper()
	i := r.cluster.Member(nc.ConnectedServerName())
	require.GreaterOrEqual(t, i, 0, "connected to %q", nc.ConnectedServerName())
	return i
}

// listedCIDs are the client ids pod's server lists now.
func (r *calloutRig) listedCIDs(t *testing.T, pod string) []uint64 {
	t.Helper()
	ctx := rowContext(t)
	monitor, stop, err := r.hub.Monitor(ctx, pod)
	if err != nil {
		return nil
	}
	defer stop()
	var connz Connz
	if monitor.Get(ctx, fmt.Sprintf("/connz?state=open&limit=%d", connzLimit), &connz) != nil {
		return nil
	}
	cids := make([]uint64, len(connz.Connections))
	for i, conn := range connz.Connections {
		cids[i] = conn.CID
	}
	return cids
}

// awaitUnlisted waits until pod's server lists none of cids.
func (r *calloutRig) awaitUnlisted(t *testing.T, pod string, cids []uint64) {
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("pod %s's server no longer lists cids %v", pod, cids), func() bool {
		listed := r.listedCIDs(t, pod)
		if listed == nil {
			return false
		}
		for _, cid := range cids {
			if slices.Contains(listed, cid) {
				return false
			}
		}
		return true
	})
}

// callout is the rig's auth service: it refuses every client it is asked about, but holds, without
// an answer, each request hold accepts, until the test refuses or admits it.
type callout struct {
	t      *testing.T
	issuer nkeys.KeyPair
	curve  nkeys.KeyPair

	mu   sync.Mutex
	hold func(jwt.AuthorizationRequest) bool
	held []*heldConnect
	// asked is every request the callout was sent, held or not.
	asked []*heldConnect
}

// heldConnect is one authorization request the callout holds: a connection the server lists,
// waiting for its CONNECT to be decided.
type heldConnect struct {
	msg     *natsgo.Msg
	request *jwt.AuthorizationRequestClaims
}

func (h *heldConnect) cid() uint64 { return h.request.ClientInformation.ID }

func newCallout(t *testing.T, keys testnats.CalloutKeys) *callout {
	t.Helper()
	issuer, err := nkeys.FromSeed([]byte(keys.IssuerSeed))
	require.NoError(t, err)
	curve, err := nkeys.FromCurveSeed([]byte(keys.XKeySeed))
	require.NoError(t, err)
	return &callout{t: t, issuer: issuer, curve: curve}
}

// holding makes the callout hold each later request hold accepts.
func (c *callout) holding(hold func(jwt.AuthorizationRequest) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hold = hold
}

func (c *callout) handle(msg *natsgo.Msg) {
	opened, err := c.curve.Open(msg.Data, msg.Header.Get(natsserver.AuthRequestXKeyHeader))
	if err != nil {
		c.t.Errorf("open the sealed authorization request: %v", err)
		return
	}
	request, err := jwt.DecodeAuthorizationRequestClaims(string(opened))
	if err != nil {
		c.t.Errorf("decode the authorization request: %v", err)
		return
	}
	c.mu.Lock()
	hold := c.hold != nil && c.hold(request.AuthorizationRequest)
	asked := &heldConnect{msg: msg, request: request}
	c.asked = append(c.asked, asked)
	if hold {
		c.held = append(c.held, asked)
	}
	c.mu.Unlock()
	if !hold {
		c.answer(asked, nil)
	}
}

// answer answers one request: admitted as user, or refused when user is nil.
func (c *callout) answer(h *heldConnect, user *jwt.UserClaims) {
	response := jwt.NewAuthorizationResponseClaims(h.request.UserNkey)
	response.Audience = h.request.Server.ID
	if user == nil {
		response.Error = "holdtest: refused"
	} else {
		token, err := user.Encode(c.issuer)
		if err != nil {
			c.t.Errorf("sign the user JWT: %v", err)
			return
		}
		response.Jwt = token
	}
	token, err := response.Encode(c.issuer)
	if err != nil {
		c.t.Errorf("sign the authorization response: %v", err)
		return
	}
	sealed, err := c.curve.Seal([]byte(token), h.request.Server.XKey)
	if err != nil {
		c.t.Errorf("seal the authorization response: %v", err)
		return
	}
	if err := h.msg.Respond(sealed); err != nil {
		c.t.Errorf("answer the authorization request: %v", err)
	}
}

// cids are the client ids of every request the callout was sent that match.
func (c *callout) cids(match func(*heldConnect) bool) []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var cids []uint64
	for _, h := range c.asked {
		if match(h) {
			cids = append(cids, h.cid())
		}
	}
	return cids
}

// admit admits h as an operator-shaped user in $G.
func (c *callout) admit(h *heldConnect) {
	user := jwt.NewUserClaims(h.request.UserNkey)
	user.Audience = "$G"
	user.Pub.Allow.Add("operator.>")
	user.Sub.Allow.Add(natsgrant.OperatorInbox + ".>")
	c.answer(h, user)
}

// awaitHeld waits until the callout holds n requests that match, and returns them.
func (c *callout) awaitHeld(t *testing.T, n int, match func(*heldConnect) bool) []*heldConnect {
	t.Helper()
	var found []*heldConnect
	testwait.Eventually(t, fmt.Sprintf("the callout holds %d such requests", n), func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		found = nil
		for _, h := range c.held {
			if match(h) {
				found = append(found, h)
			}
		}
		return len(found) >= n
	})
	return found
}

// take returns the requests the callout holds and holds them no more.
func (c *callout) take() []*heldConnect {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.held
	c.held = nil
	return held
}

// user matches the requests whose CONNECT names user.
func user(name string) func(*heldConnect) bool {
	return func(h *heldConnect) bool { return h.request.ConnectOptions.Username == name }
}

// connectAs runs attempts connects to url as CONNECT user and password, named name, one after the
// other, 100 ms apart, each refused or admitted as the callout decides, until ctx ends (attempts
// 0: for ever). Each admitted connection is closed when the test ends. The channel closes once the
// attempts are over.
func connectAs(t *testing.T, ctx context.Context, url, name, connectUser string, attempts int) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	var mu sync.Mutex
	var admitted []*natsgo.Conn
	t.Cleanup(func() {
		<-done
		mu.Lock()
		defer mu.Unlock()
		for _, nc := range admitted {
			nc.Close()
		}
	})
	go func() {
		defer close(done)
		for n := 0; attempts == 0 || n < attempts; n++ {
			if ctx.Err() != nil {
				return
			}
			nc, err := natsgo.Connect(url, natsgo.UserInfo(connectUser, "holdtest-token"), natsgo.Name(name),
				natsgo.Timeout(30*time.Second), natsgo.NoReconnect())
			if err == nil {
				mu.Lock()
				admitted = append(admitted, nc)
				mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	return done
}

// waits records each wait the report asked for, and runs then between readings.
type waits struct {
	mu    sync.Mutex
	asked []time.Duration
	then  func(ctx context.Context) error
}

func (w *waits) wait(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.asked = append(w.asked, d)
	w.mu.Unlock()
	if w.then == nil {
		return nil
	}
	return w.then(ctx)
}

func (w *waits) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.asked)
}

// requireAskedAuthTimeoutPlusOne fails t unless every wait the report asked for was the declared
// servers' auth_timeout plus 1 s: the callout block's 2 s, so 3 s.
func (w *waits) requireAskedAuthTimeoutPlusOne(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, d := range w.asked {
		require.Equal(t, time.Duration(testnats.CalloutAuthTimeout)*time.Second+time.Second, d)
	}
}

// TestHoldReportContinuousChurn shows a client that keeps retrying a connect the callout refuses,
// 100 ms apart, keeps the report from reading clean: each reading lists a new attempt, never the
// same (server id, cid) twice, so after 3 rounds the hold stops as UNKNOWN, having sent nothing on
// NATS (no mutation, and no operation, which the hold's journal records only after the report).
// The wait between readings is the test's: it refuses the attempt the callout holds, waits until
// the server no longer lists it, and returns once the client's next attempt is held. Its control
// is the same hub with no churn, which reads clean. It fails when the report counts a reading with
// unresolved connections as clean, or bounds no rounds.
func TestHoldReportContinuousChurn(t *testing.T) {
	r := newCalloutRig(t, holdAwareName)
	declared := r.step1ThenRestart(t)
	ctx := rowContext(t)
	reading, err := r.report(declared, nil).Run(ctx, r.caller)
	require.NoError(t, err, "control: with no churn the hub reads clean")
	require.Equal(t, 1, reading.Readings)

	r.callout.holding(func(request jwt.AuthorizationRequest) bool { return request.ConnectOptions.Username == "churn" })
	churn, stopChurn := context.WithCancel(ctx)
	done := connectAs(t, churn, r.cluster.Servers[0].URL(), "churn", "churn", 0)
	t.Cleanup(func() {
		stopChurn()
		for _, h := range r.callout.take() {
			r.callout.answer(h, nil)
		}
		<-done
	})
	r.callout.awaitHeld(t, 1, user("churn"))
	sent := r.caller.Stats().OutMsgs
	w := &waits{then: func(context.Context) error {
		previous := r.callout.take()
		cids := make([]uint64, len(previous))
		for i, h := range previous {
			cids[i] = h.cid()
			r.callout.answer(h, nil)
		}
		r.awaitUnlisted(t, holdtest.Pod(0), cids)
		r.callout.awaitHeld(t, 1, user("churn"))
		return nil
	}}
	_, err = r.report(declared, w.wait).Run(ctx, r.caller)
	requireUnknown(t, err, StepReport, "at each of 3 readings again", `name "churn"`)
	require.Equal(t, reportRounds, w.count(), "the report read again once per round")
	w.requireAskedAuthTimeoutPlusOne(t)
	require.Equal(t, sent, r.caller.Stats().OutMsgs, "the report sent a message on NATS")
}

// TestHoldReportAFiniteTransient shows a client that retries a refused connect 5 times and stops,
// listed at the first reading, reads clean on the next round: an unresolved connection blocks only
// when the same (server id, cid) is in two readings. Its control, a predicate that counts every
// listed connection at once, stops the hold on such a transient. It fails when the report stops on
// a connection seen in one reading alone. Each run has a transient of its own, whose first attempt
// the callout holds and the rest it refuses at once, and reads within the server's 2 s
// authorization timeout of that hold, which would refuse the attempt itself.
func TestHoldReportAFiniteTransient(t *testing.T) {
	r := newCalloutRig(t, holdAwareName)
	declared := r.step1ThenRestart(t)
	ctx := rowContext(t)
	held := map[string]bool{}
	r.callout.holding(func(request jwt.AuthorizationRequest) bool {
		name := request.ConnectOptions.Name
		if request.ConnectOptions.Username != "transient" || held[name] {
			return false
		}
		held[name] = true
		return true
	})
	transient := func(name string) (<-chan struct{}, *heldConnect) {
		done := connectAs(t, ctx, r.cluster.Servers[0].URL(), name, "transient", 5)
		return done, r.callout.awaitHeld(t, 1, named(name))[0]
	}
	over := func(name string, done <-chan struct{}) {
		for _, h := range r.callout.take() {
			r.callout.answer(h, nil)
		}
		<-done
		r.awaitUnlisted(t, holdtest.Pod(0), r.callout.cids(named(name)))
	}

	done, first := transient("transient a")
	control := r.report(declared, nil)
	control.predicate.atOnce = true
	_, err := control.Run(ctx, r.caller)
	requireRefusal(t, err, StepReport, fmt.Sprintf("cid %d", first.cid()), `name "transient a"`)
	over("transient a", done)

	done, _ = transient("transient b")
	w := &waits{then: func(context.Context) error {
		over("transient b", done)
		return nil
	}}
	reading, err := r.report(declared, w.wait).Run(ctx, r.caller)
	require.NoError(t, err)
	require.Equal(t, 2, reading.Readings, "the transient was listed at the first reading and gone at the second")
	w.requireAskedAuthTimeoutPlusOne(t)
}

// named matches the requests whose CONNECT names the connection name.
func named(name string) func(*heldConnect) bool {
	return func(h *heldConnect) bool { return h.request.ConnectOptions.Name == name }
}

// TestHoldReportAPausedOperatorConnect shows a second operator CONNECT held mid-callout, listed
// with its CONNECT user `service`, and the Keeper's renewal probe, also `service`, each listed at
// the first reading: the report waits, reads again, and passes once both are gone, sending nothing
// meanwhile. When the paused connection is then admitted as the operator, it is in both readings,
// and the hold stops naming it. Its control, a predicate that stops at once on a non-empty user,
// stops on the renewal probe. It fails when the report takes a listed CONNECT user for a client
// that holds a grant, or lets a connection admitted between two readings pass. Each run pauses a
// pair of its own and reads within the server's 2 s authorization timeout of the pause.
func TestHoldReportAPausedOperatorConnect(t *testing.T) {
	r := newCalloutRig(t, holdAwareName)
	declared := r.step1ThenRestart(t)
	ctx := rowContext(t)
	url := r.cluster.Servers[0].URL()
	r.callout.holding(func(request jwt.AuthorizationRequest) bool { return request.ConnectOptions.Username == "service" })
	pause := func() (paused, probe *heldConnect) {
		connectAs(t, ctx, url, "operator/holdtest-second", "service", 1)
		connectAs(t, ctx, url, "keeper renewal probe", "service", 1)
		return r.callout.awaitHeld(t, 1, named("operator/holdtest-second"))[0], r.callout.awaitHeld(t, 1, named("keeper renewal probe"))[0]
	}
	// end refuses every held request but admitted, which it admits, and waits until the refused
	// ones are gone.
	end := func(admitted *heldConnect) {
		var cids []uint64
		for _, h := range r.callout.take() {
			if h == admitted {
				r.callout.admit(h)
				continue
			}
			cids = append(cids, h.cid())
			r.callout.answer(h, nil)
		}
		r.awaitUnlisted(t, holdtest.Pod(0), cids)
	}

	_, probe := pause()
	control := r.report(declared, nil)
	control.predicate.atOnceOnUser = true
	_, err := control.Run(ctx, r.caller)
	requireRefusal(t, err, StepReport, fmt.Sprintf("cid %d", probe.cid()), `name "keeper renewal probe" user "service"`)
	end(nil)

	pause()
	sent := r.caller.Stats().OutMsgs
	gone := &waits{then: func(context.Context) error {
		end(nil)
		return nil
	}}
	reading, err := r.report(declared, gone.wait).Run(ctx, r.caller)
	require.NoError(t, err, "both were gone at the second reading")
	require.Equal(t, 2, reading.Readings, "both were listed at the first reading")
	require.Equal(t, sent, r.caller.Stats().OutMsgs, "the report sent a message on NATS while a connection was unresolved")

	admitted, _ := pause()
	then := &waits{then: func(context.Context) error {
		end(admitted)
		return nil
	}}
	_, err = r.report(declared, then.wait).Run(ctx, r.caller)
	requireRefusal(t, err, StepReport, "stayed listed across two readings", fmt.Sprintf("cid %d", admitted.cid()), `name "operator/holdtest-second"`)
}

// TestHoldReportOnlyTheCallerAndTheAuthService shows the hold holds with only the caller and the
// auth service connected, the auth service's connection in $G, which /connz lists with an empty
// account. Control: a predicate that compares the account with the string "$G" stops the hold on
// that connection, naming its account. Control: one listener left connected on n3 stops the hold,
// naming its server, cid, name and user. It fails when the report reads an empty account as
// another account, or passes a connection that is neither the caller's nor the auth service's.
func TestHoldReportOnlyTheCallerAndTheAuthService(t *testing.T) {
	r := newCalloutRig(t, holdAwareName)
	declared := r.step1ThenRestart(t)
	ctx := rowContext(t)
	reading, err := r.report(declared, nil).Run(ctx, r.caller)
	require.NoError(t, err)
	require.Equal(t, 1, reading.Readings)
	service := r.authServiceConnection(t, reading)
	require.Empty(t, service.Account, "/connz lists a connection in $G with an empty account")

	control := r.report(declared, (&waits{}).wait)
	control.predicate.accountByName = true
	_, err = control.Run(ctx, r.caller)
	requireRefusal(t, err, StepReport, fmt.Sprintf("cid %d name %q user %q account \"\"", service.CID, holdAwareName, r.keys.ServicePublic))

	listener := dialSeed(t, r.cluster.Servers[2].URL(), r.listenerSeed, natsgo.Name("listener"))
	cid, err := listener.GetClientID()
	require.NoError(t, err)
	_, err = r.report(declared, (&waits{}).wait).Run(ctx, r.caller)
	requireRefusal(t, err, StepReport, "server n3", fmt.Sprintf("cid %d name \"listener\" user %q", cid, r.listenerKey))
}

// authServiceConnection is the auth service's own connection as reading lists it.
func (r *calloutRig) authServiceConnection(t *testing.T, reading *ReportReading) ConnInfo {
	t.Helper()
	for _, server := range reading.Servers {
		for _, conn := range server.Connections {
			if conn.AuthorizedUser == r.keys.ServicePublic && conn.Name == holdAwareName {
				return conn
			}
		}
	}
	t.Fatalf("the reading lists no connection of the auth service: %+v", reading.Servers)
	return ConnInfo{}
}

// TestHoldReportASecondOperator shows a second operator process, connected as the caller's own
// user, stops the hold, named: the predicate admits the caller's own (server id, cid) alone.
// Control: a predicate that admits every connection of the caller's user holds with the second
// operator connected, which is the danger. It fails when the report resolves the caller by its
// user.
func TestHoldReportASecondOperator(t *testing.T) {
	r := newCalloutRig(t, holdAwareName)
	declared := r.step1ThenRestart(t)
	ctx := rowContext(t)
	second := dialSeed(t, r.cluster.URL(), r.callerSeed, natsgo.Name("operator/holdtest-second"))
	cid, err := second.GetClientID()
	require.NoError(t, err)

	control := r.report(declared, (&waits{}).wait)
	control.predicate.byCallerUser = true
	_, err = control.Run(ctx, r.caller)
	require.NoError(t, err, "control: admitting the caller's user lets a second operator through")

	_, err = r.report(declared, (&waits{}).wait).Run(ctx, r.caller)
	requireRefusal(t, err, StepReport, fmt.Sprintf("cid %d name \"operator/holdtest-second\"", cid))
}

// TestHoldReportAnAuthReplicaNotHoldAware shows an auth replica whose connection name lacks
// ` hold-aware`, connected as the auth service's own user, stops the hold. Control: with the name,
// it holds. It fails when the report resolves the auth service by its user alone.
func TestHoldReportAnAuthReplicaNotHoldAware(t *testing.T) {
	r := newCalloutRig(t, holdAwareName)
	declared := r.step1ThenRestart(t)
	ctx := rowContext(t)
	aware := dialSeed(t, r.cluster.URL(), r.keys.ServiceSeed, natsgo.Name("envoy-auth test2 hold-aware"))
	_, err := r.report(declared, (&waits{}).wait).Run(ctx, r.caller)
	require.NoError(t, err, "control: a second replica named hold-aware holds")
	aware.Close()

	older := dialSeed(t, r.cluster.URL(), r.keys.ServiceSeed, natsgo.Name("envoy-auth old"))
	cid, err := older.GetClientID()
	require.NoError(t, err)
	_, err = r.report(declared, (&waits{}).wait).Run(ctx, r.caller)
	requireRefusal(t, err, StepReport, fmt.Sprintf("cid %d name \"envoy-auth old\" user %q", cid, r.keys.ServicePublic))
}

// TestHoldReportPagesPastOneServersLimit shows a server with more than 1,024 connections: the
// reading pages by offset until it holds them all, so a listener whose connection is on the
// second page stops the hold, named, and with none the hold holds over all of them. Control: a
// reading that counts the first page as the whole holds with that listener connected. A /connz
// whose total moves at each page never holds one list, and after 3 readings is UNKNOWN. It fails
// when the report reads one page alone, or counts pages that disagree.
func TestHoldReportPagesPastOneServersLimit(t *testing.T) {
	r := newCalloutRig(t, holdAwareName)
	declared := r.step1ThenRestart(t)
	ctx := rowContext(t)
	crowded := (r.member(t, r.caller) + 1) % len(r.cluster.Servers)
	pod := holdtest.Pod(crowded)
	const replicas = 1100
	var wg sync.WaitGroup
	conns := make(chan *natsgo.Conn, replicas)
	errs := make(chan error, replicas)
	work := make(chan struct{}, replicas)
	for range replicas {
		work <- struct{}{}
	}
	close(work)
	public, sign := nkeyOf(t, r.keys.ServiceSeed)
	for range 16 {
		wg.Go(func() {
			for range work {
				nc, err := natsgo.Connect(r.cluster.Servers[crowded].URL(), natsgo.Nkey(public, sign), natsgo.Name(holdAwareName),
					natsgo.Timeout(time.Minute), natsgo.NoReconnect())
				if err != nil {
					errs <- err
					continue
				}
				conns <- nc
			}
		})
	}
	wg.Wait()
	close(conns)
	close(errs)
	for nc := range conns {
		t.Cleanup(nc.Close)
	}
	for err := range errs {
		require.NoError(t, err)
	}

	reading, err := r.report(declared, nil).Run(ctx, r.caller)
	require.NoError(t, err)
	for _, server := range reading.Servers {
		if server.Pod == pod {
			require.GreaterOrEqual(t, len(server.Connections), replicas, "the reading holds every connection of pod %s, past its first page of %d", pod, connzLimit)
		}
	}

	listener := dialSeed(t, r.cluster.Servers[crowded].URL(), r.listenerSeed, natsgo.Name("listener"))
	cid, err := listener.GetClientID()
	require.NoError(t, err)
	_, err = r.report(declared, (&waits{}).wait).Run(ctx, r.caller)
	requireRefusal(t, err, StepReport, fmt.Sprintf("cid %d name \"listener\"", cid))

	control := r.report(declared, nil)
	control.onePage = true
	_, err = control.Run(ctx, r.caller)
	require.NoError(t, err, "control: the first page alone misses the listener past it")

	moving := r.report(declared, nil)
	moving.open = func(ctx context.Context, open string) (Monitor, func(), error) {
		monitor, stop, err := r.hub.Monitor(ctx, open)
		if err != nil || open != pod {
			return monitor, stop, err
		}
		return &movingTotal{Monitor: monitor}, stop, nil
	}
	_, err = moving.Run(ctx, r.caller)
	requireUnknown(t, err, StepReport, "held no one list of every connection in 3 readings")
}

// nkeyOf is the public key and signer of the nkey seed.
func nkeyOf(t *testing.T, seed string) (string, func([]byte) ([]byte, error)) {
	t.Helper()
	pair, err := nkeys.FromSeed([]byte(seed))
	require.NoError(t, err)
	public, err := pair.PublicKey()
	require.NoError(t, err)
	return public, pair.Sign
}

// movingTotal is a monitoring port whose /connz total grows by one at each answer, as a server
// whose connections keep opening while it is paged.
type movingTotal struct {
	Monitor
	mu    sync.Mutex
	extra int
}

func (m *movingTotal) Get(ctx context.Context, path string, into any) error {
	if err := m.Monitor.Get(ctx, path, into); err != nil || !strings.HasPrefix(path, "/connz") {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.extra++
	into.(*Connz).Total += m.extra
	return nil
}

// TestHoldReportABlindReadingIsUnknown shows a reading that cannot see the caller's own connection
// is UNKNOWN, not clean: a /connz read from the wrong server, and a /connz read without auth=1,
// which shows no connection's user. Its control is the same hub read as the report reads it, which
// is clean. It fails when the report counts a reading that does not show the caller, or its user.
func TestHoldReportABlindReadingIsUnknown(t *testing.T) {
	r := newCalloutRig(t, holdAwareName)
	declared := r.step1ThenRestart(t)
	ctx := rowContext(t)
	_, err := r.report(declared, nil).Run(ctx, r.caller)
	require.NoError(t, err, "control: the same hub, read as the report reads it")

	callerPod := holdtest.Pod(r.member(t, r.caller))
	otherPod := holdtest.Pod((r.member(t, r.caller) + 1) % len(r.cluster.Servers))
	wrong := r.report(declared, nil)
	wrong.open = func(ctx context.Context, pod string) (Monitor, func(), error) {
		monitor, stop, err := r.hub.Monitor(ctx, pod)
		if err != nil || pod != callerPod {
			return monitor, stop, err
		}
		other, stopOther, err := r.hub.Monitor(ctx, otherPod)
		if err != nil {
			stop()
			return nil, nil, err
		}
		return &connzFrom{Monitor: monitor, connz: other}, func() { stop(); stopOther() }, nil
	}
	_, err = wrong.Run(ctx, r.caller)
	requireUnknown(t, err, StepReport, "/connz answered as server")

	noAuth := r.report(declared, nil)
	noAuth.noAuth = true
	_, err = noAuth.Run(ctx, r.caller)
	requireUnknown(t, err, StepReport, "the caller's own connection", "shows no authorized_user")
}

// connzFrom is a monitoring port that answers /connz from another server's port: the wrong server.
type connzFrom struct {
	Monitor
	connz Monitor
}

func (c *connzFrom) Get(ctx context.Context, path string, into any) error {
	if strings.HasPrefix(path, "/connz") {
		return c.connz.Get(ctx, path, into)
	}
	return c.Monitor.Get(ctx, path, into)
}

// TestHoldReportTheCallerAfterItsReconnect shows the caller's server id and client id are read
// again after the restart moved it, so the predicate still finds the caller. Control: with the
// ids read before the restart, the reading does not see the caller, and the hold stops. It fails
// when the report keeps the caller's ids from before its reconnect.
func TestHoldReportTheCallerAfterItsReconnect(t *testing.T) {
	r := newCalloutRig(t, holdAwareName)
	before := r.callerID
	declared := r.step1ThenRestart(t)
	ctx := rowContext(t)

	reading, err := r.report(declared, nil).Run(ctx, r.caller)
	require.NoError(t, err)
	require.NotEqual(t, before.ServerID, reading.Caller.ServerID, "the restart moved the caller to a new server id")

	control := r.report(declared, nil)
	control.caller = func() (ConnRef, error) { return before, nil }
	_, err = control.Run(ctx, nil)
	requireUnknown(t, err, StepReport, "does not see the caller's own connection", before.ServerID)
}
