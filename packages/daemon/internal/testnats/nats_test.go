package testnats

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
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
	created, err := start(Image, func() []testcontainers.ContainerCustomizer {
		return []testcontainers.ContainerCustomizer{
			testcontainers.WithLabels(map[string]string{"legion.test": label}),
			testcontainers.WithWaitStrategy(wait.ForLog("a line NATS never prints").WithStartupTimeout(3 * time.Second)),
		}
	})
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

// A start Docker refuses because something took a probed port before the publish is tried again
// on a fresh address and ports (start), and the server the retry runs is the one its caller
// configured: an nkey server refuses a client with no credential and admits its user there, as at
// a first start. testcontainers reads a configuration file's reader to its end as it creates the
// container (v0.41.0 lifecycle.go:159), before Docker refuses the publish at the start, so a retry
// handed the first attempt's reader copies an empty configuration in, and that server admits
// anyone. The row takes the first probe's client port at its address once the probe lets it go,
// as another program can, and requires the start to have been tried again: a first start Docker
// did not refuse checks no retry. Docker refuses only where this process can hold the port it
// publishes: on a daemon on this machine. On a daemon on another machine the row fails as UNKNOWN,
// naming the daemon, rather than fail on a probe count that says nothing of the configuration.
func TestAStartRetriedAfterARefusedPublishEnforcesItsUsers(t *testing.T) {
	if server, _, err := external(); err == nil && server != "" {
		t.Skipf("%s names a running NATS server, so the package starts no container", externalURLEnv)
	}
	if local, err := localDaemon(context.Background()); err != nil {
		t.Fatalf("find where the Docker daemon publishes a container's ports: %v", err)
	} else if !local {
		daemon := "another machine"
		if provider, err := testcontainers.NewDockerProvider(); err == nil {
			if host, err := provider.DaemonHost(context.Background()); err == nil {
				daemon = host
			}
			provider.Close()
		}
		t.Fatalf("UNKNOWN: the Docker daemon is at %s, not this machine, so this process cannot hold the port Docker publishes there and cannot make Docker refuse a start; run the row against a daemon on this machine", daemon)
	}
	probe := loopbackPorts
	t.Cleanup(func() { loopbackPorts = probe })
	probes := 0
	loopbackPorts = func(n int) (string, []int, error) {
		host, ports, err := probe(n)
		probes++
		if err != nil || probes > 1 {
			return host, ports, err
		}
		taken, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(ports[0])))
		if err != nil {
			return "", nil, fmt.Errorf("take the probed client port %s:%d: %w", host, ports[0], err)
		}
		t.Cleanup(func() { _ = taken.Close() })
		t.Logf("took %s, the first probe's client port, once the probe let it go", taken.Addr())
		return host, ports, nil
	}
	user, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create an nkey user: %v", err)
	}
	public, err := user.PublicKey()
	if err != nil {
		t.Fatalf("read the nkey user's public key: %v", err)
	}
	_, url := StartNkeyAuthorizedUsers(t, Image, NkeyUser{Public: public})
	if probes < 2 {
		t.Fatalf("start chose ports %d time(s): Docker did not refuse the publish on the taken port, so no retry was checked", probes)
	}
	if conn, err := nats.Connect(url, nats.Timeout(time.Second), nats.NoReconnect()); err == nil {
		conn.Close()
		t.Fatalf("the server the retry started at %s admitted a client with no credential", url)
	} else if !errors.Is(err, nats.ErrAuthorization) {
		t.Fatalf("the server the retry started at %s refused a client with no credential with %v, not its authorization violation", url, err)
	}
	conn, err := nats.Connect(url, nats.Nkey(public, user.Sign), nats.Timeout(time.Second), nats.NoReconnect())
	if err != nil {
		t.Fatalf("the server the retry started at %s refused its own nkey user: %v", url, err)
	}
	conn.Close()
	t.Logf("the server the retry started at %s refuses a client with no credential and admits its user", url)
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

// A test leaves the server as URL handed it over: the stream, consumer and message one test makes
// are gone once it ends, before any other test takes the server, so a program that runs on an
// external server after the test binary — a pod's Bun tests after its Go tests — finds none of
// them. The stream is the one the daemon's tests make, ENVOY_NOTIFICATIONS on all of
// `notifications.>`, whose publish ack answered a relay frame of pi-legion's relay row when one was
// left on such a server. The parent reads the server's own count from its monitoring endpoint
// while it holds the server (take), so no other test of the binary is on it, and not through URL,
// whose reset would empty the server first. The pool hands the parent the server freed last, the
// subtest's, and the parent fails by name when it is handed another. The same read inside the
// subtest, with the stream still there, is the reader's positive control: it sees what it counts.
func TestATestLeavesTheServerEmpty(t *testing.T) {
	var made *server
	t.Run("makes the notification stream, a consumer and a message", func(t *testing.T) {
		js := JetStream(t)
		made = heldServer(t)
		stream := CreateStream(t, js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
		if _, err := stream.CreateConsumer(t.Context(), jetstream.ConsumerConfig{Durable: "left-behind"}); err != nil {
			t.Fatalf("create consumer: %v", err)
		}
		if _, err := js.Publish(t.Context(), "notifications.agent.left", []byte("x")); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if held := jetStreamHeld(t, made); held != (jetStreamCounts{Streams: 1, Consumers: 1, Messages: 1}) {
			t.Fatalf("the server's monitoring endpoint reports %+v while the test holds its stream, want 1 stream, 1 consumer and 1 message: a reader that cannot see them proves nothing when it reads none", held)
		}
	})
	if made == nil {
		t.Fatal("the subtest that makes the stream held no server, so there is nothing to read")
	}
	if s := take(t); s != made {
		t.Fatal("the pool handed this test another NATS server than the one its subtest made the stream on, so its count would prove nothing")
	}
	if held := jetStreamHeld(t, made); held != (jetStreamCounts{}) {
		t.Errorf("the server holds %+v after the test that made them ended, want none: the next program on an external server is handed them", held)
	}
}

// The reset at the start of each test empties a server that something other than a test of this
// package left streams on: a Bun suite on the same external server, a binary killed before its
// cleanups ran, or a leaveEmpty that failed. The second subtest stands in for any of them: it
// holds a server as a test does (take) and makes a stream on a client of its own (dialReady), with
// no URL, so no leaveEmpty runs at its end. The first subtest only starts the server, for a run of
// this test alone. The count inside the second subtest is the reader's positive control, and the
// third fails by name when the pool hands it another server than the second's.
func TestAStreamLeftOutsideTheFixtureIsGoneAtTheNextTake(t *testing.T) {
	var left *server
	t.Run("starts the server", func(t *testing.T) { URL(t) })
	t.Run("leaves a stream that no test's end empties", func(t *testing.T) {
		left = take(t)
		ctx, cancel := context.WithTimeout(t.Context(), readinessTimeout)
		defer cancel()
		conn, js, err := dialReady(ctx, left.url, t.Name())
		if err != nil {
			t.Fatalf("connect to the NATS server: %v", err)
		}
		defer conn.Close()
		if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "LEFT_OUTSIDE", Subjects: []string{"outside.>"}}); err != nil {
			t.Fatalf("create stream: %v", err)
		}
		if held := jetStreamHeld(t, left); held.Streams != 1 {
			t.Fatalf("the server's monitoring endpoint reports %+v with the stream made, want 1 stream: a reader that cannot see it proves nothing when it reads none", held)
		}
	})
	t.Run("finds none of it", func(t *testing.T) {
		js := JetStream(t)
		if left == nil || heldServer(t) != left {
			t.Fatal("this subtest holds another NATS server of the pool than the one the earlier subtest left its stream on, so it would prove nothing")
		}
		names := js.StreamNames(t.Context())
		for name := range names.Name() {
			t.Errorf("stream %s, which no fixture's end emptied, is still on the server the next test is handed", name)
		}
		if err := names.Err(); err != nil {
			t.Fatalf("list streams: %v", err)
		}
	})
}

// jetStreamCounts is what a server's monitoring endpoint reports it holds across its accounts.
type jetStreamCounts struct {
	Streams   int    `json:"streams"`
	Consumers int    `json:"consumers"`
	Messages  uint64 `json:"messages"`
}

// jetStreamHeld reads s's own count of its streams, consumers and messages from its monitoring
// endpoint's /jsz, which no client connection, and so no reset, is needed for.
func jetStreamHeld(t *testing.T, s *server) jetStreamCounts {
	t.Helper()
	response, err := monitorClient.Get(s.monitor + "/jsz")
	if err != nil {
		t.Fatalf("read the NATS server's JetStream counts: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/jsz answered %s", s.monitor, response.Status)
	}
	var counts jetStreamCounts
	if err := json.NewDecoder(response.Body).Decode(&counts); err != nil {
		t.Fatalf("decode %s/jsz: %v", s.monitor, err)
	}
	return counts
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

// A durable an earlier test created does not reach the next test, even when a stream delete whose
// rename failed leaves the earlier test's stream directory in place with every consumer's state
// file (the consumer deletes in empty say how, and name the lines). The first subtest stands in for
// an earlier delete's removal still running: it makes the .<name> directory the next delete
// renames onto. The second checks the delete did leave the stream's directory, so the row cannot
// pass on a delete that moved it aside, and then asks for durables that start at the next
// message. Two durables, so a reset that deleted only one consumer fails too. The stream's name is
// the invocation's own, which also makes the first subtest's look at its directory proof that the
// path is the server's store, and under -count keeps a second invocation's plant out of the
// .<name> the first one's last delete is still removing, which would remove the plant with it. The
// second subtest run alone (-run naming it) lays what the first leaves in a subtest of its own
// before it takes the server, so it still finds an earlier test's durables.
func TestADurableOfAnEarlierTestDoesNotReachTheNextTest(t *testing.T) {
	config := jetstream.StreamConfig{Name: "LEFT_DURABLE_" + rand.Text(), Subjects: []string{"left.>"}, Storage: jetstream.FileStorage}
	var durables []jetstream.ConsumerConfig
	for _, name := range []string{"left-durable-1", "left-durable-2"} {
		durables = append(durables, jetstream.ConsumerConfig{Durable: name, AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: jetstream.DeliverNewPolicy})
	}
	var streams, plant string
	var held *server
	t.Cleanup(func() {
		if plant != "" {
			onServersHost(t, held, "rm", "-rf", plant)
		}
	})
	leave := func(t *testing.T) {
		js := JetStream(t)
		stream := CreateStream(t, js, config)
		for _, durable := range durables {
			if _, err := stream.CreateConsumer(t.Context(), durable); err != nil {
				t.Fatalf("create durable %s: %v", durable.Durable, err)
			}
		}
		streams, held = streamsDirectory(t), heldBy(t)
		onTheServersHost(t, "test", "-d", filepath.Join(streams, config.Name))
		plant = filepath.Join(streams, "."+config.Name)
		onTheServersHost(t, "mkdir", "-p", filepath.Join(plant, "still-being-removed"))
	}
	if !t.Run("creates durables while an earlier delete of their stream is still being removed", leave) {
		return
	}
	t.Run("creates the durables again and finds them at the next message", func(t *testing.T) {
		if plant == "" && !t.Run("an earlier test leaves durables while their stream's removal is held", leave) {
			t.Fatal("the earlier test's durables and the held removal of their stream could not be laid, so this subtest would prove nothing")
		}
		if plant == "" {
			t.Fatal("no earlier test left durables and a held removal of their stream (-run filtered out both subtests that lay them), so this subtest would prove nothing")
		}
		js := JetStream(t)
		// The pool hands a test the server freed last, and this package's tests run one at a
		// time, so this is the server the earlier subtest held; a run that hands it another fails
		// here by name rather than on a store directory that server never had.
		if heldBy(t) != held {
			t.Fatal("this subtest holds another NATS server of the pool than the one the earlier subtest left its durables on, so it would prove nothing")
		}
		// Registered after JetStream(t), so that it runs before the fixture's own end (leaveEmpty):
		// with the held removal gone, that end's delete of the stream can move its directory aside and
		// remove it, and the server's store keeps no directory of this test. Left in place, the delete
		// removes the stream's meta.inf and cannot move the directory onto the held one, which stays
		// in the store for good (nats-server v2.15.1 filestore.go:12231-12237). It empties plant, so
		// the parent's own removal, for a run that never reaches this subtest, has nothing to do.
		t.Cleanup(func() {
			onTheServersHost(t, "rm", "-rf", plant)
			plant = ""
		})
		onTheServersHost(t, "test", "-d", filepath.Join(streams, config.Name))
		stream := CreateStream(t, js, config)
		published, err := js.Publish(t.Context(), "left.one", []byte("x"))
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		for _, durable := range durables {
			consumer, err := stream.CreateConsumer(t.Context(), durable)
			if err != nil {
				t.Fatalf("create durable %s: %v", durable.Durable, err)
			}
			info, err := consumer.Info(t.Context())
			if err != nil {
				t.Fatalf("read durable %s: %v", durable.Durable, err)
			}
			if info.NumPending != 0 {
				t.Errorf("durable %s was created delivering new after the message at stream sequence %d, with %d messages pending, want 0: it took up the earlier test's durable, delivered through stream sequence %d",
					durable.Durable, published.Sequence, info.NumPending, info.Delivered.Stream)
			}
		}
	})
}

// streamsDirectory is the directory the server t holds keeps its streams in: its JetStream store,
// as its monitoring endpoint reports it, and in that the global account's directory, $G, which
// holds every stream of a server with no accounts configured. The row that reads it acts on that
// path where the server runs, so an external server's store must be on this host, writable by the
// tests' user, and named by an absolute path (README.md says so); a relative one, which the
// server resolves from its own working directory, fails t by name.
func streamsDirectory(t *testing.T) string {
	t.Helper()
	monitorURL := heldServer(t).monitor
	response, err := monitorClient.Get(monitorURL + "/jsz")
	if err != nil {
		t.Fatalf("read the NATS server's JetStream configuration: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/jsz answered %s", monitorURL, response.Status)
	}
	var jsz struct {
		Config struct {
			StoreDir string `json:"store_dir"`
		} `json:"config"`
	}
	if err := json.NewDecoder(response.Body).Decode(&jsz); err != nil {
		t.Fatalf("decode %s/jsz: %v", monitorURL, err)
	}
	if jsz.Config.StoreDir == "" {
		t.Fatalf("%s/jsz names no store directory", monitorURL)
	}
	if !filepath.IsAbs(jsz.Config.StoreDir) {
		t.Fatalf("%s/jsz names the store directory %q, a relative path this test cannot resolve: start the server with an absolute -sd", monitorURL, jsz.Config.StoreDir)
	}
	return filepath.Join(jsz.Config.StoreDir, "$G", "streams")
}

// heldServer is the NATS server of the pool t holds, and fails t when it holds none.
func heldServer(t *testing.T) *server {
	t.Helper()
	s := heldBy(t)
	if s == nil {
		t.Fatalf("testnats: %s holds no NATS server of the package: take one through URL or JetStream first", t.Name())
	}
	return s
}

// onTheServersHost runs a command where the server t holds keeps its store (onServersHost).
func onTheServersHost(t *testing.T, name string, args ...string) {
	t.Helper()
	onServersHost(t, heldServer(t), name, args...)
}

// onServersHost runs a command where s keeps its store: in its container, or on this host for an
// external server, which a host with no Docker runs itself (the package comment). It fails t when
// the command fails, with what the command printed.
func onServersHost(t *testing.T, s *server, name string, args ...string) {
	t.Helper()
	command := strings.Join(append([]string{name}, args...), " ")
	container := s.container
	if container == nil {
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s on this host, where the external NATS server keeps its store: %v: %s", command, err, out)
		}
		return
	}
	code, reader, err := container.Exec(context.Background(), append([]string{name}, args...), tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("%s in the NATS container: %v", command, err)
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read the output of %s in the NATS container: %v", command, err)
	}
	if code != 0 {
		t.Fatalf("%s in the NATS container exited %d: %s", command, code, out)
	}
}

// The stream a test is handed must be empty. A create that comes back holding an earlier test's
// work, which the purge in empty says how a create can do, fails the test there, naming each message by
// subject, stream sequence and stored time and each consumer by name, rather than reaching the
// test as a count it reads later and reads as its own. A stream holding a consumer and no message
// names the consumer alone: there is no message to name, and a sequence nothing stored is not
// one. A stream whose range holds a deleted sequence names the stored messages after that gap and
// counts the rest right: with more messages than the failure names one by one, the last of the
// named ones is the message that far past the gap, and the remainder is the count of what is left.
func TestCreateStreamRefusesAStreamThatIsNotEmpty(t *testing.T) {
	js := JetStream(t)
	for _, row := range []struct {
		name   string
		stream string
		leave  func(t *testing.T, stream jetstream.Stream)
		want   []string
		unwant []string
	}{
		{
			name:   "a message",
			stream: "ALREADY_FULL",
			leave: func(t *testing.T, _ jetstream.Stream) {
				if _, err := js.Publish(t.Context(), "full.one", []byte("x")); err != nil {
					t.Fatalf("publish: %v", err)
				}
			},
			want: []string{"ALREADY_FULL is not empty at its create", "message 1 full.one stored "},
		},
		{
			name:   "a consumer and no message",
			stream: "ALREADY_CONSUMED",
			leave: func(t *testing.T, stream jetstream.Stream) {
				if _, err := stream.CreateConsumer(t.Context(), jetstream.ConsumerConfig{Durable: "left-behind"}); err != nil {
					t.Fatalf("create consumer: %v", err)
				}
			},
			want:   []string{"ALREADY_CONSUMED is not empty at its create", "consumer left-behind"},
			unwant: []string{"message"},
		},
		{
			name:   "messages with a deleted sequence between them",
			stream: "ALREADY_GAPPED",
			leave: func(t *testing.T, stream jetstream.Stream) {
				// Twelve messages with sequence 2 deleted leave eleven stored, one more than the
				// failure names one by one. A loop that stepped sequence by sequence would stop
				// ten sequences in, at the stored message 10 rather than 11, and a remainder
				// counted over the range rather than over the messages named would print one
				// more than the one message left.
				for i := 1; i <= 12; i++ {
					if _, err := js.Publish(t.Context(), fmt.Sprintf("full.%d", i), []byte("x")); err != nil {
						t.Fatalf("publish full.%d: %v", i, err)
					}
				}
				if err := stream.DeleteMsg(t.Context(), 2); err != nil {
					t.Fatalf("delete the message at sequence 2: %v", err)
				}
			},
			want: []string{
				"ALREADY_GAPPED is not empty at its create",
				"message 1 full.1 stored ",
				"message 11 full.11 stored ",
				"and 1 more",
			},
			unwant: []string{"message 2", "not found"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			config := jetstream.StreamConfig{Name: row.stream, Subjects: []string{"full.>"}}
			// The row before this one deleted its stream, the account's last, so this create can
			// meet the store failure CreateStream retries (its doc says how): a direct create
			// failed there with "err_code=10049 description=error creating store for stream".
			stream := CreateStream(t, js, config)
			t.Cleanup(func() {
				if err := js.DeleteStream(context.Background(), row.stream); err != nil {
					t.Errorf("delete the stream: %v", err)
				}
			})
			row.leave(t, stream)
			again := &fatalRecorder{TB: t}
			done := make(chan struct{})
			go func() {
				defer close(done)
				CreateStream(again, js, config)
			}()
			<-done
			for _, want := range row.want {
				if !strings.Contains(again.message, want) {
					t.Errorf("CreateStream of a stream that is not empty failed with %q, want it to say %q", again.message, want)
				}
			}
			for _, unwant := range row.unwant {
				if strings.Contains(again.message, unwant) {
					t.Errorf("CreateStream of a stream that is not empty failed with %q, want it to say no %q", again.message, unwant)
				}
			}
		})
	}
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

// cleanupDropped is t with Cleanup dropped, so a client the fixture hands it is held by the
// fixture and nothing registers its close. A row that needs the leak check's own
// closeFixtureConnections to be what closes a connection takes it through this, since a close
// registered on the running test would close it at the row's end instead and say nothing about
// the check.
type cleanupDropped struct{ testing.TB }

func (cleanupDropped) Cleanup(func()) {}

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
	if !strings.Contains(again.message, "already holds a NATS server of the package") {
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
	open, err := readConnections(heldBy(t))
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
		drained(next, heldBy(t))
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
	leakCheck(recorder, heldBy(t), armed)
	report := recorder.only(t)
	if !strings.Contains(report, listed[ownName].String()) {
		t.Errorf("the leak check reported %q, which does not name connection %s by its id and its name", report, listed[ownName])
	}

	next := &fatalRecorder{TB: t}
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		drained(next, heldBy(t))
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
	recordLeft(heldBy(t), t.Name(), nil)
}

// A read of the server that fails after the check has closed the fixture's own clients leaves
// the record of what the read before that close found standing. Without it the next test's drain
// finds connections nothing recorded, waits its whole thirty seconds out and fails naming
// nobody, which is the fault this check exists to end (dispatch://INFRA-1964). Nothing here
// waits on timing: the fixture holds one client no cleanup of this row will close, and the
// reader fails from the moment closeFixtureConnections inside the check closes it, so the first
// settle reads the server and every read after the close fails.
func TestAFailedReadAfterTheCloseKeepsTheRecord(t *testing.T) {
	url := URL(t)
	armed := armedNow(t)
	conn := Connect(cleanupDropped{TB: t}, url)
	listed := openedSince(t, armed, t.Name())
	real := readConnections
	t.Cleanup(func() { readConnections = real })
	readConnections = func(s *server) ([]Connection, error) {
		if conn.IsClosed() {
			return nil, errors.New("the monitoring endpoint cannot be read")
		}
		return real(s)
	}

	recorder := &leakRecorder{TB: t}
	leakCheck(recorder, heldBy(t), armed)

	report := strings.Join(recorder.messages, "\n")
	if !strings.Contains(report, listed[t.Name()].String()) {
		t.Errorf("the leak check reported %q, which does not name connection %s, the one the read before the close found", report, listed[t.Name()])
	}
	leaker, all := leftByTheLastTest(heldBy(t), []Connection{listed[t.Name()]})
	if leaker != t.Name() || !all {
		t.Errorf("after a read that failed once the fixture's clients were closed, the record answers (%q, %t) for connection %s, want (%q, true): that answer is what the next test's drain fails by, at once and in this test's name", leaker, all, listed[t.Name()], t.Name())
	}

	// The row leaves the server as it found it: the check closed the connection the fixture
	// held, and no record for the test after this one to be failed by.
	recordLeft(heldBy(t), t.Name(), nil)
}

// A check whose every read of the server fails leaves no record at all, the one an earlier
// test's check wrote included: the record names the test whose check ran last, whatever that
// check managed to read, so the next test's drain can never fail in the name of a test that is
// not the leaker — a wrong name in the very message this check exists to make right
// (dispatch://INFRA-1964). What that drain gets instead is nothing: it waits its bound out and
// names nobody, which is this issue's old behaviour and no accusation of the wrong test.
// Nothing here waits on timing: the reader fails from before the check runs, so both of the
// check's reads fail, and the record put in place by hand carries a connection id no server
// holds.
func TestACheckWhoseEveryReadFailsLeavesNoEarlierTestsName(t *testing.T) {
	URL(t)
	armed := armedNow(t)
	earlier := "TestATestThatRanBeforeThisOne" + rand.Text()
	recorded := []Connection{{CID: math.MaxUint64, Name: earlier}}
	recordLeft(heldBy(t), earlier, recorded)
	real := readConnections
	t.Cleanup(func() { readConnections = real })
	readConnections = func(*server) ([]Connection, error) {
		return nil, errors.New("the monitoring endpoint cannot be read")
	}

	recorder := &leakRecorder{TB: t}
	leakCheck(recorder, heldBy(t), armed)

	if len(recorder.messages) == 0 {
		t.Fatalf("the leak check reported nothing although every read of the server failed, so this row drove neither of its reads")
	}
	if leaker, _ := leftByTheLastTest(heldBy(t), recorded); leaker != "" {
		t.Errorf("after a check whose every read of the server failed, the record still names %q, the test before it: the next test's drain would fail in that test's name for connections it never left. Want nobody named, whatever else the record answers", leaker)
	}
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
		armLeakCheck(blind, heldBy(t))
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

// A client this fixture hands a test is listed on the server under that test's name, unless its
// caller passed a name of its own, which Connect lets it do. The name is read from the server
// rather than from the client: that name is what a leak report, a drain failure and an operator's
// own read of /connz have to name the test by.
// packages/envoy/internal/bus/name_test.go holds the same for Envoy's clients.
func TestTheFixtureNamesItsClientAfterTheTest(t *testing.T) {
	Connect(t, URL(t))
	var open []Connection
	var err error
	if !testwait.Holds(leakSettle, leakPoll, func() bool {
		open, err = readConnections(heldBy(t))
		return err == nil && slices.ContainsFunc(open, func(c Connection) bool { return c.Name == t.Name() })
	}) {
		t.Fatalf("the NATS server lists %v (%v), none of them named %s", open, err, t.Name())
	}
}

// clientsLeftOpen takes a server of the pool for t, opens two clients of the fixture's and leaves
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
	leakCheck(recorder, heldBy(t), armed)
	return recorder, listed
}

// armedNow is what the server holds now, in the shape armLeakCheck records it: a row that drives
// the check itself arms it itself as well, since the arm URL ran registered a check of its own on
// the real t and that one is what holds the row to its own promise to leave nothing open.
func armedNow(t *testing.T) map[uint64]bool {
	t.Helper()
	open, err := readConnections(heldBy(t))
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
		found, err = opened(func() ([]Connection, error) { return readConnections(heldBy(t)) }, armed)
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
