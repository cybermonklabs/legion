// edge_test.go holds the broker's edge (BROKER_EDGE_LISTEN_ADDR) to its place in the boot and the
// stop, and drives the REAL compiled broker with the edge and without it, over HTTP on a real
// Postgres: the edge is a fourth socket the boot binds and the stop drains beside the API, and a
// broker with none of its settings binds exactly the sockets it bound before the edge existed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The steps the edge's boot and stop rows record, beside ordering_test.go's.
const (
	theEdgeBind             = "the edge's bind"
	theEdgeShutdownBegan    = "the edge's shutdown began"
	theEdgeShutdownReturned = "the edge's shutdown returned"
)

// The edge's two URLs the binary's rows start the broker with, and the X-Forwarded-For its load
// balancer writes for a client.
const (
	testEdgeURL              = "https://secrets.edge.example"
	testEdgeMachineLoginURL  = "https://dispatch.edge.example/credentials/machine"
	testEdgeClientForwarding = "203.0.113.70, 35.191.0.1"
)

// TestTheEdgeIsBoundAfterTheAirlockAndBeforeTheMetricsPort pins where the edge's socket is taken:
// inside the boot, after the API's and the airlock's and before the metrics port's, so a pod that
// cannot take its edge fails its boot rather than serving an API whose edge the load balancer
// finds nobody listening on.
//
// The named revert: bind the edge in main, after boot has returned.
func TestTheEdgeIsBoundAfterTheAirlockAndBeforeTheMetricsPort(t *testing.T) {
	var ran steps
	bind := func(step string) func() (net.Listener, error) {
		return func() (net.Listener, error) {
			ran.record(step)
			return net.Listen("tcp", "127.0.0.1:0")
		}
	}
	started, err := boot(t.Context(), brokerBoot{
		LoadPolicy:    func(context.Context) (*policy.Current, error) { return nil, nil },
		StartListener: func(_ context.Context, _ *policy.Current, onConnected func()) { onConnected() },
		Listen:        bind(theBind),
		ListenAirlock: bind(theAirlockBind),
		ListenEdge:    bind(theEdgeBind),
		ListenMetrics: bind(theMetricsBind),
		ListenWait:    bootListenWait,
		Catchup:       func(context.Context, *policy.Current) error { return nil },
		Log:           orderingLog(),
	})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if started.Serving == nil || started.EdgeListener == nil {
		t.Fatalf("boot answered %+v; want a finished boot holding the edge's socket", started)
	}
	t.Cleanup(started.EndServing)
	for _, listener := range []net.Listener{started.Listener, started.AirlockListener, started.EdgeListener, started.MetricsListener} {
		defer listener.Close()
	}
	if want := []string{theBind, theAirlockBind, theEdgeBind, theMetricsBind}; !ran.is(want...) {
		t.Fatalf("the boot ran %s; want %s", &ran, strings.Join(want, " -> "))
	}
}

// TestTheEdgeBindRefusalClosesEverySocketBoundBefore: an edge the pod cannot take is a boot that
// fails, and the two sockets bound before it go with it, since a refused boot must leave no
// address held; and a boot given no edge, the deployment that names none, binds none.
//
// The named revert: leave the API's socket open when the edge's bind fails.
func TestTheEdgeBindRefusalClosesEverySocketBoundBefore(t *testing.T) {
	var bound []net.Listener
	bind := func() (net.Listener, error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			bound = append(bound, listener)
		}
		return listener, err
	}
	refusal := errors.New("the edge's port is already held")
	started, err := boot(t.Context(), brokerBoot{
		LoadPolicy:    func(context.Context) (*policy.Current, error) { return nil, nil },
		StartListener: func(_ context.Context, _ *policy.Current, onConnected func()) { onConnected() },
		Listen:        bind,
		ListenAirlock: bind,
		ListenEdge:    func() (net.Listener, error) { return nil, refusal },
		ListenMetrics: bind,
		ListenWait:    bootListenWait,
		Catchup:       func(context.Context, *policy.Current) error { return nil },
		Log:           orderingLog(),
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("boot with an unbindable edge = %v; want the bind's own refusal", err)
	}
	if started.Serving != nil || started.Listener != nil || started.EdgeListener != nil || started.MetricsListener != nil {
		t.Fatalf("the refused boot answered %+v; want nothing to serve and nothing to stop", started)
	}
	if len(bound) != 2 {
		t.Fatalf("the boot bound %d sockets before the edge; want the API's and the airlock's, and not the metrics port's", len(bound))
	}
	for i, port := range []string{"the API's", "the airlock's"} {
		if err := bound[i].Close(); err == nil {
			t.Errorf("the refused boot left %s socket open: the pod that replaces this one finds the address held", port)
		}
	}

	started, err = boot(t.Context(), brokerBoot{
		LoadPolicy:    func(context.Context) (*policy.Current, error) { return nil, nil },
		StartListener: func(_ context.Context, _ *policy.Current, onConnected func()) { onConnected() },
		Listen:        listenTCP("127.0.0.1:0"),
		ListenMetrics: listenTCP("127.0.0.1:0"),
		ListenWait:    bootListenWait,
		Catchup:       func(context.Context, *policy.Current) error { return nil },
		Log:           orderingLog(),
	})
	if err != nil {
		t.Fatalf("boot with no edge: %v", err)
	}
	t.Cleanup(started.EndServing)
	defer started.Listener.Close()
	defer started.MetricsListener.Close()
	if started.EdgeListener != nil {
		t.Fatalf("a boot given no edge answered an edge listener on %s", started.EdgeListener.Addr())
	}
}

// closeWatched is a bound socket of a boot row: its Close first runs closing, so a row reads when
// the boot closed it.
type closeWatched struct {
	net.Listener
	closing func()
}

func (l closeWatched) Close() error {
	l.closing()
	return l.Listener.Close()
}

// TestARefusedBootClosesEverySocketItBoundBeforeItEndsServing: whichever bind after the API's
// refuses the boot, the airlock's, the edge's or the metrics port's, the boot answers that bind's
// own error, and closes every socket it bound before it, each once and in the order it bound
// them, before it ends the serving context, so nothing the serving context runs outlives a
// socket the refused boot still holds.
//
// The named reverts, each failing every row it touches: end the serving context before the
// sockets close; close the sockets in another order; leave the last socket bound before the
// refusal open.
func TestARefusedBootClosesEverySocketItBoundBeforeItEndsServing(t *testing.T) {
	for _, row := range []struct {
		refused string
		closed  []string
	}{
		{theAirlockBind, []string{theBind}},
		{theEdgeBind, []string{theBind, theAirlockBind}},
		{theMetricsBind, []string{theBind, theAirlockBind, theEdgeBind}},
	} {
		t.Run(row.refused, func(t *testing.T) {
			refusal := errors.New(row.refused + " is already held")
			var serving context.Context
			var closed []string
			bind := func(step string) func() (net.Listener, error) {
				return func() (net.Listener, error) {
					if step == row.refused {
						return nil, refusal
					}
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						return nil, err
					}
					t.Cleanup(func() { listener.Close() })
					return closeWatched{Listener: listener, closing: func() {
						if serving.Err() != nil {
							t.Errorf("the refused boot closed %s after it had ended the serving context", step)
						}
						closed = append(closed, step)
					}}, nil
				}
			}
			started, err := boot(t.Context(), brokerBoot{
				LoadPolicy: func(ctx context.Context) (*policy.Current, error) {
					serving = ctx
					return nil, nil
				},
				StartListener: func(_ context.Context, _ *policy.Current, onConnected func()) { onConnected() },
				Listen:        bind(theBind),
				ListenAirlock: bind(theAirlockBind),
				ListenEdge:    bind(theEdgeBind),
				ListenMetrics: bind(theMetricsBind),
				ListenWait:    bootListenWait,
				Catchup:       func(context.Context, *policy.Current) error { return nil },
				Log:           orderingLog(),
			})
			if !errors.Is(err, refusal) {
				t.Fatalf("boot refused at %s = %v; want that bind's own refusal", row.refused, err)
			}
			if started.Serving != nil || started.Listener != nil {
				t.Fatalf("the refused boot answered %+v; want nothing to serve and nothing to stop", started)
			}
			if !slices.Equal(closed, row.closed) {
				t.Fatalf("the boot refused at %s closed %v; want %v, each once and in the order it bound them", row.refused, closed, row.closed)
			}
			if serving.Err() == nil {
				t.Fatal("the refused boot left its serving context running")
			}
		})
	}
}

// TestTheEdgeDrainsBesideTheAPI pins the edge's drain: it closes out of the one shutdown budget
// beside the API rather than after it, and before the work that outlived the drain stops. The
// API's fake shutdown returns only once the edge's has begun, so a stop that drained the edge
// after the API's is caught by the hang guard rather than read as an order.
//
// The named revert: drop ShutdownEdge from stop, or call it after Shutdown has returned.
func TestTheEdgeDrainsBesideTheAPI(t *testing.T) {
	var ran steps
	edgeBegan := make(chan struct{})
	stop(t.Context(), brokerStop{
		Drain:    stopDrain,
		DrainNow: make(chan struct{}),
		Draining: func(bool) {},
		Shutdown: func(context.Context) error {
			ran.record(theShutdownBegan)
			select {
			case <-edgeBegan:
			case <-time.After(bootListenWait):
				t.Errorf("the API's shutdown waited %s and the edge's never began beside it", bootListenWait)
			}
			ran.record(theShutdownReturned)
			return nil
		},
		ShutdownEdge: func(context.Context) error {
			ran.record(theEdgeShutdownBegan)
			close(edgeBegan)
			if ran.has(theServingEnded) {
				t.Error("the stop ended the serving context before the edge had drained")
			}
			ran.record(theEdgeShutdownReturned)
			return nil
		},
		EndServing: func() { ran.record(theServingEnded) },
		CloseStore: func() {},
		Log:        orderingLog(),
	})
	if !ran.has(theEdgeShutdownReturned) {
		t.Fatalf("the stop ran %s; it never drained the edge", &ran)
	}
	if order := ran.order(); len(order) == 0 || order[len(order)-1] != theServingEnded {
		t.Fatalf("the stop ran %s; want both shutdowns before %s", &ran, theServingEnded)
	}
}

// listeningPorts is every TCP port pid listens on, sorted: each socket of its descriptors
// (/proc/<pid>/fd) that its network namespace's tables (/proc/<pid>/net/tcp and tcp6) list in
// state LISTEN. It fails t when it can read neither the descriptors nor the tables, so an empty
// answer is a process listening on nothing and never one this could not see.
func listeningPorts(t *testing.T, pid int) []int {
	t.Helper()
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		t.Fatalf("read the broker's descriptors: %v", err)
	}
	sockets := map[string]bool{}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(fmt.Sprintf("/proc/%d/fd", pid), fd.Name()))
		if err == nil && strings.HasPrefix(target, "socket:[") {
			sockets[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
		}
	}
	if len(sockets) == 0 {
		t.Fatalf("the broker (pid %d) holds no socket at all, so this cannot read what it listens on", pid)
	}
	var ports []int
	for _, table := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, table))
		if err != nil {
			t.Fatalf("read the broker's %s table: %v", table, err)
		}
		for line := range strings.Lines(string(data)) {
			fields := strings.Fields(line)
			// sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode
			if len(fields) < 10 || fields[3] != "0A" || !sockets[fields[9]] {
				continue
			}
			_, hexPort, _ := strings.Cut(fields[1], ":")
			port, err := strconv.ParseInt(hexPort, 16, 32)
			if err != nil {
				t.Fatalf("read the port of %q: %v", fields[1], err)
			}
			ports = append(ports, int(port))
		}
	}
	slices.Sort(ports)
	return ports
}

// portOf is addr's port.
func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// postDeviceAuthorization posts a device authorization to base, its request object signed for
// audience by a key of its own and naming person, with headers, and answers the status and the
// OAuth body.
func postDeviceAuthorization(t *testing.T, base, audience, person string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	key := proofKey(t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	request, err := record.Sign(key, audience, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: deviceHost},
	}, "", person, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"client_id": {oauthwire.ClientID}, "scope": {oauthwire.Scope}, "dpop_jkt": {thumbprint}, "request": {request}}
	req, err := http.NewRequest(http.MethodPost, base+"/v1/oauth/device_authorization", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("> POST %s/v1/oauth/device_authorization naming %s\n< %d %s", base, person, response.StatusCode, raw)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("the device authorization answered %d %s, which is no JSON object", response.StatusCode, raw)
	}
	return response.StatusCode, body
}

// statusOf is the status target answers a request of method with no body.
func statusOf(t *testing.T, method, target string) int {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}

// answersHelpPrefix is how broker_answers_total's HELP begins: it names every port whose answers
// it counts, since the edge's server counts its answers on the counter the API port's does.
const answersHelpPrefix = "Answers of the broker's API port, its edge and its airlock's proxy port, "

// metricsScrape is what GET /metrics on the metrics port at addr answers, which must be 200.
func metricsScrape(t *testing.T, addr string) string {
	t.Helper()
	response, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics on %s = %d, %v", addr, response.StatusCode, err)
	}
	return string(body)
}

// TestTheEdgeIsAbsentFromABrokerWithoutItsSettings: the real binary started with none of the
// edge's settings binds exactly the two sockets a broker with no airlock bound before the edge
// existed, its API's and its metrics port's, logs no edge, and serves the device grant on its own
// address as it did, a person of the Workspace included, since the edge's refusal of one is the
// edge's alone.
//
// The named revert: bind the edge whether or not BROKER_EDGE_LISTEN_ADDR is set.
func TestTheEdgeIsAbsentFromABrokerWithoutItsSettings(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads the broker's sockets from /proc, which only Linux has")
	}
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_MACHINE_LOGIN_URL=" + testMachineLoginURL,
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
	}})
	requireHealthy(t, broker.Addr)
	if want, got := []int{portOf(t, broker.Addr), portOf(t, broker.MetricsAddr(t))}, listeningPorts(t, broker.Cmd.Process.Pid); !slices.Equal(got, sortedInts(want)) {
		t.Fatalf("the broker with no edge listens on %v, want its API and its metrics port alone, %v", got, sortedInts(want))
	}
	if records := recordsWith(broker.Log(), "broker edge listening", nil); len(records) != 0 {
		t.Fatalf("the broker with no edge logged %v", records)
	}
	status, body := postDeviceAuthorization(t, "http://"+broker.Addr, "http://"+broker.Addr, "ada@"+ciphertest.SignInDomain, nil)
	if status != http.StatusOK || body["verification_uri"] != testMachineLoginURL {
		t.Fatalf("a device authorization on the broker's own address naming a person of the Workspace = %d %v, want 200 with %s", status, body, testMachineLoginURL)
	}
}

// sortedInts is ints sorted.
func sortedInts(ints []int) []int {
	slices.Sort(ints)
	return ints
}

// TestTheEdgeServesTheDeviceLoginFromTheBinary: the real binary started with the edge's three
// settings binds a third socket, the edge's, and logs it; serves the device login there with the
// edge's page as its verification_uri and a request object signed for BROKER_EDGE_URL, through
// the load balancer's X-Forwarded-For; refuses there a person of the Workspace, BROKER_SIGNIN_DOMAIN
// or a BROKER_SIGNIN_WORKSPACE_DOMAINS domain, and a request whose header names no client; answers
// 404 for routes of the broker's own address and 405 for any method but POST on its two login
// paths; and drains with the API: once the broker is asked to stop, the edge's GET /healthz
// answers 503 SHUTTING_DOWN beside the API's, so the load balancer stops choosing the pod while
// the edge still answers.
func TestTheEdgeServesTheDeviceLoginFromTheBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads the broker's sockets from /proc, which only Linux has")
	}
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_MACHINE_LOGIN_URL=" + testMachineLoginURL,
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
		"BROKER_EDGE_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_EDGE_URL=" + testEdgeURL,
		"BROKER_EDGE_MACHINE_LOGIN_URL=" + testEdgeMachineLoginURL,
		"BROKER_SIGNIN_WORKSPACE_DOMAINS=" + ciphertest.EdgeWorkspaceDomain,
		"BROKER_DRAIN_SECONDS=30",
	}})
	line := broker.AwaitRecord(t, "broker edge listening", map[string]any{"url": testEdgeURL, "verification_uri": testEdgeMachineLoginURL})
	edgeAddr, _ := line["addr"].(string)
	if edgeAddr == "" {
		t.Fatalf("the edge's line names no addr: %v", line)
	}
	want := sortedInts([]int{portOf(t, broker.Addr), portOf(t, broker.MetricsAddr(t)), portOf(t, edgeAddr)})
	if got := listeningPorts(t, broker.Cmd.Process.Pid); !slices.Equal(got, want) {
		t.Fatalf("the broker with an edge listens on %v, want its API, its metrics port and its edge, %v", got, want)
	}
	edge := "http://" + edgeAddr
	requireHealthy(t, edgeAddr)

	through := map[string]string{"X-Forwarded-For": testEdgeClientForwarding}
	status, body := postDeviceAuthorization(t, edge, testEdgeURL, "outside@outside.example", through)
	if status != http.StatusOK || body["verification_uri"] != testEdgeMachineLoginURL || body["device_code"] == nil {
		t.Fatalf("a device authorization at the edge = %d %v, want 200 with a device code and %s", status, body, testEdgeMachineLoginURL)
	}
	for _, person := range []string{"ada@" + ciphertest.SignInDomain, "bob@" + ciphertest.EdgeWorkspaceDomain} {
		if status, body := postDeviceAuthorization(t, edge, testEdgeURL, person, through); status != http.StatusBadRequest || body["code"] != "EDGE_WORKSPACE_LOGIN" {
			t.Errorf("a device authorization at the edge naming %s = %d %v, want 400 EDGE_WORKSPACE_LOGIN", person, status, body)
		}
	}
	if status, body := postDeviceAuthorization(t, edge, testEdgeURL, "outside@outside.example", nil); status != http.StatusBadRequest || body["code"] != "REQUEST_INVALID" {
		t.Errorf("a device authorization at the edge with no X-Forwarded-For = %d %v, want 400 REQUEST_INVALID", status, body)
	}
	for _, row := range []struct{ method, path string }{
		{http.MethodGet, "/v1/settings"},
		{http.MethodPost, "/v1/enrollments"},
		{http.MethodPost, "/v1/oauth/introspect"},
		{http.MethodGet, "/v1/pending"},
		{http.MethodGet, metadataPath},
		{http.MethodGet, "/metrics"},
	} {
		if status := statusOf(t, row.method, edge+row.path); status != http.StatusNotFound {
			t.Errorf("%s %s at the edge = %d, want 404: the edge serves the device login alone", row.method, row.path, status)
		}
	}
	for _, path := range []string{"/v1/oauth/device_authorization", "/v1/oauth/token"} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			if status := statusOf(t, method, edge+path); status != http.StatusMethodNotAllowed {
				t.Errorf("%s %s at the edge = %d, want 405: the load balancer's route matches the path alone, and the edge answers POST there and nothing else", method, path, status)
			}
		}
	}
	// The edge's answers count in broker_answers_total beside the API port's, labelled
	// port="edge", and the metric's HELP says so: the two EDGE_WORKSPACE_LOGIN refusals above are
	// samples of it on the metrics port, and the binary records the edge through
	// answer.EdgeMiddleware.
	scrape := metricsScrape(t, broker.MetricsAddr(t))
	if !strings.Contains(scrape, "# HELP broker_answers_total "+answersHelpPrefix) {
		t.Errorf("the metrics port's HELP for broker_answers_total does not begin %q:\n%s", answersHelpPrefix, scrape)
	}
	var edgeRefusals string
	for line := range strings.Lines(scrape) {
		if strings.HasPrefix(line, "broker_answers_total{") && strings.Contains(line, `code="EDGE_WORKSPACE_LOGIN"`) {
			edgeRefusals = strings.TrimSpace(line)
		}
	}
	if !strings.HasSuffix(edgeRefusals, ` 2`) || !strings.Contains(edgeRefusals, `port="edge"`) {
		t.Errorf("broker_answers_total's EDGE_WORKSPACE_LOGIN sample is %q, want the edge's 2 refusals counted with port=\"edge\"", edgeRefusals)
	}

	broker.Terminate(t)
	for _, addr := range []string{broker.Addr, edgeAddr} {
		if !testwait.Holds(listenTimeout, listeningPoll, func() bool {
			return statusOf(t, http.MethodGet, "http://"+addr+"/healthz") == http.StatusServiceUnavailable
		}) {
			t.Fatalf("GET /healthz at %s never answered 503 once the broker was asked to stop:\n%s", addr, broker.Log())
		}
	}
	status, body = postDeviceAuthorization(t, edge, testEdgeURL, "draining@outside.example", through)
	if status != http.StatusOK {
		t.Fatalf("a device authorization at the draining edge = %d %v, want it still answered through the drain", status, body)
	}
}

// TestTheEdgesGateRefusalsAreCountedAndWriteNoLine: the refusals the edge's two login routes make
// before any bucket, which a caller on the internet can send at any rate, write no "broker: answer
// refused" line and are counted in broker_answers_total with port="edge": a device authorization
// naming a person of the Workspace (EDGE_WORKSPACE_LOGIN), one with no X-Forwarded-For line and a
// token request whose body is not a form (REQUEST_INVALID), and a grant the edge does not take
// (EDGE_GRANT_REFUSED). A refusal past the gates still writes its line: a device authorization
// whose request object is signed for another audience, which the device login refuses, is the one
// refusal line the edge writes.
//
// The named revert: drop the gates' NoteQuiet (api.RegisterEdge), and each of the four writes a
// line naming port=edge.
func TestTheEdgesGateRefusalsAreCountedAndWriteNoLine(t *testing.T) {
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_MACHINE_LOGIN_URL=" + testMachineLoginURL,
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
		"BROKER_EDGE_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_EDGE_URL=" + testEdgeURL,
		"BROKER_EDGE_MACHINE_LOGIN_URL=" + testEdgeMachineLoginURL,
		"BROKER_SIGNIN_WORKSPACE_DOMAINS=" + ciphertest.EdgeWorkspaceDomain,
	}})
	edgeAddr, _ := broker.AwaitRecord(t, "broker edge listening", nil)["addr"].(string)
	if edgeAddr == "" {
		t.Fatalf("the edge's line names no addr:\n%s", broker.Log())
	}
	edge := "http://" + edgeAddr
	through := map[string]string{"X-Forwarded-For": testEdgeClientForwarding}
	if status, body := postDeviceAuthorization(t, edge, testEdgeURL, "ada@"+ciphertest.SignInDomain, through); status != http.StatusBadRequest || body["code"] != "EDGE_WORKSPACE_LOGIN" {
		t.Fatalf("a device authorization at the edge naming a person of the Workspace = %d %v, want 400 EDGE_WORKSPACE_LOGIN", status, body)
	}
	if status, body := postDeviceAuthorization(t, edge, testEdgeURL, "outside@outside.example", nil); status != http.StatusBadRequest || body["code"] != "REQUEST_INVALID" {
		t.Fatalf("a device authorization at the edge with no X-Forwarded-For = %d %v, want 400 REQUEST_INVALID", status, body)
	}
	grant := url.Values{"grant_type": {"client_credentials"}}
	if status, body := postEdgeToken(t, edge, "text/plain", grant, through); status != http.StatusBadRequest || body["code"] != "REQUEST_INVALID" {
		t.Fatalf("a token request at the edge whose body is not a form = %d %v, want 400 REQUEST_INVALID", status, body)
	}
	if status, body := postEdgeToken(t, edge, "application/x-www-form-urlencoded", grant, through); status != http.StatusBadRequest || body["code"] != "EDGE_GRANT_REFUSED" {
		t.Fatalf("a client_credentials grant at the edge = %d %v, want 400 EDGE_GRANT_REFUSED", status, body)
	}
	status, body := postDeviceAuthorization(t, edge, "https://another.example", "outside@outside.example", through)
	controlID, _ := body["answer_id"].(string)
	if status != http.StatusBadRequest || body["code"] != "REQUEST_INVALID" || controlID == "" {
		t.Fatalf("a device authorization at the edge signed for another audience = %d %v, want the device login's 400 REQUEST_INVALID with an answer id", status, body)
	}

	// The line is written before its answer, so once the control's line has arrived every line
	// the four refusals before it could write has too.
	broker.AwaitRecord(t, answer.RefusalMessage, map[string]any{"answer_id": controlID, "port": answer.PortEdge, "code": "REQUEST_INVALID"})
	if lines := recordsWith(broker.Log(), answer.RefusalMessage, map[string]any{"port": answer.PortEdge}); len(lines) != 1 {
		t.Errorf("the edge wrote %d refusal lines, want 1, the device login's own refusal %s: %v", len(lines), controlID, lines)
	}
	samples := map[string]string{}
	for line := range strings.Lines(metricsScrape(t, broker.MetricsAddr(t))) {
		if strings.HasPrefix(line, `broker_answers_total{outcome="refused",code="`) && strings.Contains(line, `port="edge"`) {
			code, _, _ := strings.Cut(strings.TrimPrefix(line, `broker_answers_total{outcome="refused",code="`), `"`)
			samples[code] = strings.TrimSpace(line[strings.LastIndexByte(line, ' ')+1:])
		}
	}
	if want := map[string]string{"EDGE_WORKSPACE_LOGIN": "1", "REQUEST_INVALID": "3", "EDGE_GRANT_REFUSED": "1"}; !maps.Equal(samples, want) {
		t.Errorf("broker_answers_total's refused samples at the edge = %v, want %v", samples, want)
	}
}

// postEdgeToken posts form to the edge's token route at base as a body of contentType, with
// headers, and answers the status and the JSON body.
func postEdgeToken(t *testing.T, base, contentType string, form url.Values, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("the edge's token route answered %d %s, which is no JSON object", response.StatusCode, raw)
	}
	return response.StatusCode, body
}
