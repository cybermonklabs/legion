package testnats

import (
	"crypto/rand"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A start that fails after Docker created the container — here a readiness wait that can never
// succeed, as a start timing out under load does — leaves no container behind once it returns: the
// container is shared by the package, so no test's end would remove it. The container is asked for
// by its label on the same Docker daemon testcontainers created it on. A run on an external server
// starts no container, and is how a host with no Docker runs these tests, so it skips this one.
func TestAFailedStartLeavesNoContainer(t *testing.T) {
	if server, _, err := external(); err == nil && server != "" {
		t.Skipf("%s names a running NATS server, so the package starts no container", externalURLEnv)
	}
	label := "legion-testnats-" + rand.Text()
	created, err := start(Image,
		testcontainers.WithLabels(map[string]string{"legion.test": label}),
		testcontainers.WithWaitStrategy(wait.ForLog("a line NATS never prints").WithStartupTimeout(3*time.Second)),
	)
	if err == nil {
		t.Fatal("a start whose readiness wait cannot succeed returned no error")
	}
	if created == nil {
		t.Fatalf("the failed start created no container (%v), so there was nothing to leak", err)
	}
	docker, err := testcontainers.NewDockerClientWithOpts(t.Context())
	if err != nil {
		t.Fatalf("the Docker client testcontainers uses: %v", err)
	}
	defer docker.Close()
	left, err := docker.ContainerList(t.Context(), container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "legion.test="+label))})
	if err != nil {
		t.Fatalf("list containers labelled %s: %v", label, err)
	}
	for _, c := range left {
		t.Errorf("container %s of the failed start is still there (%s)", c.ID, c.State)
		_ = docker.ContainerRemove(t.Context(), c.ID, container.RemoveOptions{Force: true})
	}
}

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// A running server is named by both of its variables, the monitoring endpoint's trailing slash
// dropped, since the reset appends its path; neither set is no external server, and one set without
// the other is refused naming both, never taken as an instruction to start a container.
func TestAnExternalServerIsNamedByBothVariables(t *testing.T) {
	for _, tc := range []struct {
		name, server, monitoring string
		wantServer, wantMonitor  string
		wantErr                  bool
	}{
		{name: "both", server: "nats://127.0.0.1:4222", monitoring: "http://127.0.0.1:8222/", wantServer: "nats://127.0.0.1:4222", wantMonitor: "http://127.0.0.1:8222"},
		{name: "neither"},
		{name: "the client URL alone", server: "nats://127.0.0.1:4222", wantErr: true},
		{name: "the monitoring endpoint alone", monitoring: "http://127.0.0.1:8222", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(externalURLEnv, tc.server)
			t.Setenv(externalMonitorEnv, tc.monitoring)
			server, monitoring, err := external()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), externalURLEnv) || !strings.Contains(err.Error(), externalMonitorEnv) {
					t.Fatalf("external() = %q, %q, %v; want an error naming %s and %s", server, monitoring, err, externalURLEnv, externalMonitorEnv)
				}
				return
			}
			if err != nil || server != tc.wantServer || monitoring != tc.wantMonitor {
				t.Fatalf("external() = %q, %q, %v; want %q, %q", server, monitoring, err, tc.wantServer, tc.wantMonitor)
			}
		})
	}
}

// A test's first create after URL's reset can meet nats-server still removing the account's
// streams directory, and is answered with the store failure; CreateStream tries it again. Every
// other answer is the test's own failure and must not be retried into a slower one or a pass.
func TestRetryStreamStoreFailureRetriesOnlyTheStoreFailure(t *testing.T) {
	storeFailure := &jetstream.APIError{Code: 500, ErrorCode: streamCreateErrorCode, Description: streamStoreFailed}
	t.Run("store failure", func(t *testing.T) {
		attempts := 0
		err := retryStreamStoreFailure(func() error {
			attempts++
			if attempts < 3 {
				return storeFailure
			}
			return nil
		})
		if err != nil || attempts != 3 {
			t.Fatalf("err = %v after %d attempts, want nil after 3", err, attempts)
		}
	})
	t.Run("any other error", func(t *testing.T) {
		other := &jetstream.APIError{Code: 500, ErrorCode: streamCreateErrorCode, Description: "insufficient storage resources available"}
		attempts := 0
		err := retryStreamStoreFailure(func() error {
			attempts++
			return other
		})
		if err != other || attempts != 1 {
			t.Fatalf("err = %v after %d attempts, want %v after 1", err, attempts, other)
		}
	})
}

// The package's tests share one server, and each gets it as a fresh container was: a stream, its
// consumer and its messages left by one test are gone when the next asks for the server.
func TestEachTestGetsTheServerEmpty(t *testing.T) {
	t.Run("leaves a stream, a consumer and a message", func(t *testing.T) {
		js := JetStream(t)
		stream := CreateStream(t, js, jetstream.StreamConfig{Name: "LEFT_BEHIND", Subjects: []string{"left.>"}})
		if _, err := stream.CreateConsumer(t.Context(), jetstream.ConsumerConfig{Durable: "left-behind"}); err != nil {
			t.Fatalf("create consumer: %v", err)
		}
		if _, err := js.Publish(t.Context(), "left.one", []byte("x")); err != nil {
			t.Fatalf("publish: %v", err)
		}
	})
	t.Run("finds none of it", func(t *testing.T) {
		js := JetStream(t)
		names := js.StreamNames(t.Context())
		for name := range names.Name() {
			t.Errorf("stream %s from the previous test is still on the server", name)
		}
		if err := names.Err(); err != nil {
			t.Fatalf("list streams: %v", err)
		}
	})
}

// A test that publishes a burst without waiting for acks leaves messages the server routes after
// the test ends. The next test, recreating a stream on the same subjects, finds none of them: the
// reset waits for the previous test's connections to be gone first.
func TestABurstLeftInFlightDoesNotReachTheNextTest(t *testing.T) {
	const burst = 20000
	config := jetstream.StreamConfig{Name: "BURST", Subjects: []string{"burst.>"}}
	t.Run("publishes a burst and ends", func(t *testing.T) {
		js := JetStream(t)
		CreateStream(t, js, config)
		payload := []byte(strings.Repeat("x", 64))
		for i := range burst {
			if err := js.Conn().Publish(fmt.Sprintf("burst.%d", i%16), payload); err != nil {
				t.Fatalf("publish %d: %v", i, err)
			}
		}
	})
	t.Run("recreates the stream and finds none of it", func(t *testing.T) {
		js := JetStream(t)
		stream := CreateStream(t, js, config)
		time.Sleep(time.Second)
		info, err := stream.Info(t.Context())
		if err != nil {
			t.Fatalf("stream info: %v", err)
		}
		if info.State.Msgs != 0 {
			t.Errorf("the recreated stream holds %d of the previous test's %d messages", info.State.Msgs, burst)
		}
	})
}

// fatalRecorder is t with Fatal recorded and the goroutine ended, as t.Fatal does.
type fatalRecorder struct {
	testing.TB
	message string
}

func (f *fatalRecorder) Fatal(args ...any) { f.message = fmt.Sprint(args...); runtime.Goexit() }
func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// leakRecorder is t with Errorf and Fatalf recorded rather than failing the running test, and a
// name of its own when one is set. The leak check reports a leak through Errorf and refuses to
// arm through Fatalf, and what the refusal is about is the name it looks for among the server's
// connections, so a row that drives the arm needs a test name of its own. Fatalf ends the
// goroutine, as t.Fatalf does, so a row drives it from one of its own.
type leakRecorder struct {
	testing.TB
	name     string
	messages []string
}

func (r *leakRecorder) Name() string {
	if r.name != "" {
		return r.name
	}
	return r.TB.Name()
}

func (r *leakRecorder) Errorf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

func (r *leakRecorder) Fatalf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

// only is the one failure the recorder holds; a check that reported twice, or not at all, is not
// the one the row describes, and the row says what it holds instead.
func (r *leakRecorder) only(t *testing.T) string {
	t.Helper()
	if len(r.messages) != 1 {
		t.Fatalf("the leak check reported %d failures %q, want exactly one", len(r.messages), r.messages)
	}
	return r.messages[0]
}

// A test that takes the server twice, through URL and then JetStream, is refused at once, naming
// itself, instead of waiting on itself for the whole test binary's timeout.
func TestASecondTakeByTheSameTestFailsAtOnce(t *testing.T) {
	URL(t)
	again := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		JetStream(again)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a second take by the same test waited on itself")
	}
	if !strings.Contains(again.message, "already holds the shared NATS server") {
		t.Errorf("a second take by the same test failed with %q, not the re-entry refusal", again.message)
	}
}

// StartNkeyAuthorized's readiness wait refuses a server that admits a client with no credential:
// such a server enforces no nkey users, and every refusal a test expects of it would pass for the
// wrong reason. The shared server, which asks for no credential, stands in for one.
func TestTheNkeyReadinessWaitRefusesAServerThatAdmitsAnyone(t *testing.T) {
	url := URL(t)
	wait := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		answering(wait, url)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the readiness wait is still waiting on a server that admits anyone")
	}
	if want := "admitted a client with no credential"; !strings.Contains(wait.message, want) {
		t.Errorf("the readiness wait ended with %q, want it to say %q", wait.message, want)
	}
}

// The leak check fails the test that left a client of the fixture's open on the shared server,
// and names that test and every connection it left, each by the id the server gave it and the
// name that client carries. The names come from the server's own account rather than from the
// failing test, which is what lets one report tell apart the several clients a test can leave:
// this row leaves one client the fixture named after the test and one its caller named itself,
// and requires the report to carry both names (dispatch://INFRA-1964).
func TestAFixtureClientLeftOpenFailsTheCheckNamingIt(t *testing.T) {
	ownName := "a-client-named-by-its-caller-" + rand.Text()
	recorder, listed := clientsLeftOpen(t, ownName)
	report := recorder.only(t)
	if !strings.Contains(report, t.Name()) {
		t.Errorf("the leak check reported %q, which does not name the test that left the connections open", report)
	}
	for _, name := range []string{t.Name(), ownName} {
		if !strings.Contains(report, listed[name].String()) {
			t.Errorf("the leak check reported %q, which does not name connection %s by its id and its name", report, listed[name])
		}
	}
}

// The check closes the clients it reported, since the fixture opened them and a test owes their
// close to nobody (Connect), so the next test to take the server finds it as empty as the fixture
// promises rather than draining connections the leaker's own process has already forgotten.
func TestTheCheckClosesTheFixtureClientsItReported(t *testing.T) {
	ownName := "a-client-named-by-its-caller-" + rand.Text()
	recorder, listed := clientsLeftOpen(t, ownName)
	if len(recorder.messages) == 0 {
		t.Fatalf("the leak check reported nothing for the %d connections %v left open, so this row proves nothing about the closes", len(listed), listed)
	}
	open, err := clientConnections()
	if err != nil {
		t.Fatalf("read the NATS server's connections after the leak check: %v", err)
	}
	for _, left := range listed {
		if slices.ContainsFunc(open, func(c Connection) bool { return c.CID == left.CID }) {
			t.Errorf("the leak check reported connection %s and left it open: the server still lists %v", left, open)
		}
	}
	next := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		drained(next)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the next test's drain is still waiting for connections the leak check reported it had closed")
	}
	if next.message != "" {
		t.Errorf("the next test's drain failed with %q after the leak check closed the fixture's clients", next.message)
	}
}

// A connection the fixture never opened is one nothing in this process can close — a child
// process's client, or a raw dial of a test's own — so the check records it under the name of the
// test that left it, and the next take is failed at once rather than after the drain's thirty
// seconds. Before the record the next test waited that bound out and named nobody, which is the
// fault this check exists to end (dispatch://INFRA-1964).
func TestAConnectionTheFixtureDidNotOpenFailsTheNextTakeAtOnce(t *testing.T) {
	url := URL(t)
	armed := armedNow(t)
	ownName := "a-client-the-fixture-did-not-open-" + rand.Text()
	raw, err := nats.Connect(url, nats.Name(ownName), nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("dial the NATS server at %s without the fixture: %v", url, err)
	}
	defer raw.Close()
	listed := openedSince(t, armed, ownName)
	recorder := &leakRecorder{TB: t}
	leakCheck(recorder, armed)
	report := recorder.only(t)
	if !strings.Contains(report, listed[ownName].String()) {
		t.Errorf("the leak check reported %q, which does not name connection %s by its id and its name", report, listed[ownName])
	}

	next := &fatalRecorder{TB: t}
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		drained(next)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the next test's drain is waiting out a connection nothing will close")
	}
	if took := time.Since(started); took > time.Second {
		t.Errorf("the next test's drain took %s to fail on a recorded connection, want it refused at once rather than after %s", took, readinessTimeout)
	}
	if !strings.Contains(next.message, t.Name()) {
		t.Errorf("the next test's drain failed with %q, which does not name %s, the test that left the connection", next.message, t.Name())
	}

	// The row leaves the server as it found it: the connection closed, and no record for the
	// test after this one to be failed by.
	raw.Close()
	recordLeft(t.Name(), nil)
}

// The check's own positive control: a reader that answered nothing whatever the server held would
// let every test pass, so arming refuses unless it can find the client the reset has open. This
// row gives the arm a test name no connection carries, which is what a broken reader looks like
// from the inside, and requires it to say so rather than arm on an empty answer.
func TestTheCheckRefusesToArmWhenItCannotSeeItsOwnClient(t *testing.T) {
	URL(t)
	blind := &leakRecorder{TB: t, name: "TestNoConnectionOnTheServerCarriesThisName" + rand.Text()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		armLeakCheck(blind)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("arming the leak check for a test with no connection of its own is still running")
	}
	refusal := blind.only(t)
	if !strings.Contains(refusal, blind.Name()) {
		t.Errorf("the arm refused with %q, want it to name the client %q it looked for", refusal, blind.Name())
	}
}

// Every client this fixture hands a test is listed on the server under that test's name, read
// from the server rather than from the client: that name is what a leak report, a drain failure
// and an operator's own read of /connz have to name the test by.
// packages/envoy/internal/bus/name_test.go holds the same for Envoy's clients.
func TestTheFixtureNamesItsClientAfterTheTest(t *testing.T) {
	Connect(t, URL(t))
	var open []Connection
	var err error
	if !testwait.Holds(leakSettle, leakPoll, func() bool {
		open, err = clientConnections()
		return err == nil && slices.ContainsFunc(open, func(c Connection) bool { return c.Name == t.Name() })
	}) {
		t.Fatalf("the NATS server lists %v (%v), none of them named %s", open, err, t.Name())
	}
}

// clientsLeftOpen takes the shared server for t, opens two clients of the fixture's and leaves
// both open — one the fixture names after t, one its caller names ownName — and runs the leak
// check as the end of t would. It answers what the check reported and the two connections the
// server listed for those clients, by name.
func clientsLeftOpen(t *testing.T, ownName string) (*leakRecorder, map[string]Connection) {
	t.Helper()
	url := URL(t)
	armed := armedNow(t)
	Connect(t, url)
	Connect(t, url, nats.Name(ownName))
	listed := openedSince(t, armed, t.Name(), ownName)
	recorder := &leakRecorder{TB: t}
	leakCheck(recorder, armed)
	return recorder, listed
}

// armedNow is what the server holds now, in the shape armLeakCheck records it: a row that drives
// the check itself arms it itself as well, since the arm URL ran registered a check of its own on
// the real t and that one is what holds the row to its own promise to leave nothing open.
func armedNow(t *testing.T) map[uint64]bool {
	t.Helper()
	open, err := clientConnections()
	if err != nil {
		t.Fatalf("read the NATS server's connections to arm the leak check: %v", err)
	}
	return cids(open)
}

// openedSince waits for the server to list the connections named want, and no others, beyond
// those armed holds, and answers them by name. The wait is the settle the check itself gives the
// server: a client is connected when its dial returns, but the monitoring endpoint is a reader of
// the server's own clock.
func openedSince(t *testing.T, armed map[uint64]bool, want ...string) map[string]Connection {
	t.Helper()
	var found []Connection
	var err error
	byName := func() map[string]Connection {
		listed := make(map[string]Connection, len(found))
		for _, c := range found {
			listed[c.Name] = c
		}
		return listed
	}
	if !testwait.Holds(leakSettle, leakPoll, func() bool {
		found, err = opened(armed)
		if err != nil || len(found) != len(want) {
			return false
		}
		listed := byName()
		for _, name := range want {
			if _, open := listed[name]; !open {
				return false
			}
		}
		return true
	}) {
		t.Fatalf("the NATS server lists %v (%v) beyond the %d connections open before, want the %d just opened, named %v", found, err, len(armed), len(want), want)
	}
	return byName()
}
