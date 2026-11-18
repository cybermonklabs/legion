package testnats

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// A test that calls URL twice gets one server, and a JetStream call in the same test reaches that
// server too: the client JetStream opens is connected to the URL URL returned, the server URL
// started lists it under the test's name, and the process runs one server for the test.
func TestURLTwiceAndJetStreamInOneTestGiveOneServer(t *testing.T) {
	first := URL(t)
	if again := URL(t); again != first {
		t.Fatalf("a second URL in %s returned %s, want the first call's %s", t.Name(), again, first)
	}
	js := JetStream(t)
	if connected := js.Conn().ConnectedUrl(); connected != first {
		t.Errorf("JetStream's client is connected to %s, want the server URL gave the test at %s", connected, first)
	}
	listed := listedAs(t, t.Name())
	if cid, err := js.Conn().GetClientID(); err != nil || listed[t.Name()].CID != cid {
		t.Errorf("the server URL started lists %v, want JetStream's client, CID %d (%v), among its clients", listed, cid, err)
	}
	serversMu.Lock()
	var started []string
	for s := range running {
		if s.test == t.Name() {
			started = append(started, s.url)
		}
	}
	serversMu.Unlock()
	if len(started) != 1 {
		t.Errorf("the process runs %d servers for %s (%v), want the one URL started", len(started), t.Name(), started)
	}
}

// A stream a test makes through JetStream is on the server at the address URL gives the same test:
// a client of that address finds the stream and the message published to it.
func TestAStreamMadeThroughJetStreamIsSeenAtTheURLOfTheSameTest(t *testing.T) {
	js := JetStream(t)
	CreateStream(t, js, jetstream.StreamConfig{Name: "SEEN", Subjects: []string{"seen.>"}})
	if _, err := js.Publish(t.Context(), "seen.one", []byte("x")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	other, err := jetstream.New(Connect(t, URL(t)), RequestTimeout(t))
	if err != nil {
		t.Fatalf("open JetStream on a client of URL's address: %v", err)
	}
	stream, err := other.Stream(t.Context(), "SEEN")
	if err != nil {
		t.Fatalf("a client of the address URL gave %s cannot find the stream it made through JetStream: %v", t.Name(), err)
	}
	info, err := stream.Info(t.Context())
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Errorf("a client of URL's address reads %d messages on the stream, want the 1 published through JetStream", info.State.Msgs)
	}
}

// Each test gets a server empty of what an earlier test made: a stream, its consumer and its
// messages left by one test are not on the server the next one asks for.
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
			t.Errorf("stream %s from the previous test is on the server", name)
		}
		if err := names.Err(); err != nil {
			t.Fatalf("list streams: %v", err)
		}
	})
}

// childEnv names the test a run of this binary as its own child process is for (runChild).
const childEnv = "LEGION_TESTNATS_CHILD"

// inChild reports whether this run of t is the child process runChild started for t. A test that
// has to end, or a run that has to finish, for its subject to happen (a test failed by its own
// leak check, Main failing a run) runs itself again as that child and reads the child's report.
func inChild(t *testing.T) bool { return os.Getenv(childEnv) == t.Name() }

// runChild runs this test binary again for t alone, as the child inChild tells apart, and returns
// its combined output and its exit code. The child is killed when t's wait would give up
// (testwait.Context).
func runChild(t *testing.T) (string, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find this test binary: %v", err)
	}
	child := exec.CommandContext(testwait.Context(t), self, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), childEnv+"="+t.Name())
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(output), 0
	case errors.As(err, &exit):
		return string(output), exit.ExitCode()
	}
	t.Fatalf("run %s as a child process: %v\n%s", t.Name(), err, output)
	return "", 0
}

// A test that leaves a client of its server open at its end is failed by the leak check, which
// names the test and the client by its CID and its name. The test runs as a child process, so
// that its end is a real one: the cleanup URL registered runs after the test's own, on the test's
// own t, and the child's report is what go test prints for it. The client is one the fixture
// opened for a stand-in of the test that drops its cleanups, so nothing closes it before the check
// does; the check fails such a client at once, so the child ends with no wait.
func TestATestThatLeavesAClientOfItsServerOpenFailsNamingIt(t *testing.T) {
	const name = "a-client-left-open-at-the-end-of-its-test"
	if inChild(t) {
		Connect(cleanupDropped{TB: t}, URL(t), nats.Name(name))
		return
	}
	output, code := runChild(t)
	if code == 0 || !strings.Contains(output, "--- FAIL: "+t.Name()) {
		t.Fatalf("the test that left a client of its server open passed (exit %d), want it failed by the leak check; it printed:\n%s", code, output)
	}
	named := regexp.MustCompile(regexp.QuoteMeta(t.Name()) + ` ended with 1 client\(s\) \[\d+ ` + regexp.QuoteMeta(strconv.Quote(name)) + `\]`)
	if !named.MatchString(output) {
		t.Errorf("the leak check's failure does not name %s and the client it left by its CID and name %q; the test printed:\n%s", t.Name(), name, output)
	}
}

// A client the fixture did not open (the daemon's own, a child process's, a raw nats.Connect) is
// waited for, and one the server still holds when the shared wait gives up fails the test, naming
// the test and the client by its CID and its name. The check runs on a stand-in for t whose
// deadline has already passed, so the wait asks the server once and gives up, with no wait of its
// own; testwait writes that failure to stderr as it makes it, which is the "gave up waiting" line
// this test prints.
func TestAClientTheFixtureDidNotOpenFailsTheCheckNamingIt(t *testing.T) {
	url := URL(t)
	name := "a-client-the-fixture-did-not-open-" + rand.Text()
	raw, err := nats.Connect(url, nats.Name(name))
	if err != nil {
		t.Fatalf("dial the NATS server at %s without the fixture: %v", url, err)
	}
	defer raw.Close()
	listed := listedAs(t, name)
	check := &leakRecorder{T: t, expired: true}
	runCheck(t, check)
	report := check.only(t)
	if !strings.Contains(report, t.Name()) {
		t.Errorf("the leak check reported %q, which does not name the test that left the client open", report)
	}
	if !strings.Contains(report, listed[name].String()) {
		t.Errorf("the leak check reported %q, which does not name connection %s by its CID and its name", report, listed[name])
	}
}

// A server of the fixture still running after the last test fails the run: Main names it, by its
// URL and the test that started it, and exits 1 although every test passed. The test runs as a
// child process, since only a run's end reaches Main; there it starts its server for a stand-in
// that drops its cleanups, so the stop URL registers never runs.
func TestAServerLeftRunningPastTheLastTestFailsMain(t *testing.T) {
	if inChild(t) {
		URL(cleanupDropped{TB: t})
		return
	}
	output, code := runChild(t)
	if !strings.Contains(output, "--- PASS: "+t.Name()) {
		t.Fatalf("the test that left its server running did not pass, so its run's exit code says nothing of Main; it printed:\n%s", output)
	}
	if code != 1 {
		t.Errorf("a run that left a server running exited %d, want 1; it printed:\n%s", code, output)
	}
	if want := "that " + t.Name() + " started is still running after the last test"; !strings.Contains(output, want) {
		t.Errorf("Main did not name the server %s left running (want %q); the run printed:\n%s", t.Name(), want, output)
	}
}

// The leak check fails the test that left a client of the fixture's open on its server at once, and
// names that test and every connection it left, each by the id the server gave it and the name
// that client carries. The names come from the clients themselves rather than from the failing
// test, which is what lets one report tell apart the several clients a test can leave: this row
// leaves one client the fixture named after the test and one its caller named itself, and requires
// the report to carry both names (dispatch://INFRA-1964).
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

// The check closes the fixture's clients it reported, since the fixture opened them and a test owes
// their close to nobody (Connect), so the server goes down holding none of them, and its wait for
// the rest ends with no second failure.
func TestTheCheckClosesTheFixtureClientsItReported(t *testing.T) {
	ownName := "a-client-named-by-its-caller-" + rand.Text()
	recorder, listed := clientsLeftOpen(t, ownName)
	recorder.only(t)
	open := Connections(t)
	for _, left := range listed {
		if slices.ContainsFunc(open, func(c Connection) bool { return c.CID == left.CID }) {
			t.Errorf("the leak check reported connection %s and left it open: the server still lists %v", left, open)
		}
	}
}

// A test's first create after another test, or a driver of the same end-to-end run, deleted the
// account's last stream can meet nats-server still removing the account's streams directory, and is
// answered with the store failure; CreateStream asks again. Every other answer is the test's own
// failure and must not be retried into a slower one or a pass.
func TestRetryStreamStoreFailureRetriesOnlyTheStoreFailure(t *testing.T) {
	storeFailure := &jetstream.APIError{Code: 500, ErrorCode: streamCreateErrorCode, Description: streamStoreFailed}
	t.Run("store failure", func(t *testing.T) {
		attempts := 0
		err := retryStreamStoreFailure(t, func() error {
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
		err := retryStreamStoreFailure(t, func() error {
			attempts++
			return other
		})
		if err != other || attempts != 1 {
			t.Fatalf("err = %v after %d attempts, want %v after 1", err, attempts, other)
		}
	})
}

// The stream a test is handed must be empty. A create that comes back holding earlier work, which
// CreateStream's comment says how a create can do, fails the test there, naming each message by
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
			stream, err := js.CreateStream(t.Context(), config)
			if err != nil {
				t.Fatalf("create the stream: %v", err)
			}
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
			testwait.Receive(t, "CreateStream of a stream that is not empty to end", done)
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

// StartNkeyAuthorized's readiness wait refuses a server that admits a client with no credential:
// such a server enforces no nkey users, and every refusal a test expects of it would pass for the
// wrong reason. A server started with no credential at all stands in for one (URL's server asks a
// token, so it is no such stand-in).
func TestTheNkeyReadinessWaitRefusesAServerThatAdmitsAnyone(t *testing.T) {
	url := start(t, &natsserver.Options{}, false).url
	wait := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		answering(wait, url)
	}()
	testwait.Receive(t, "the readiness wait on a server that admits anyone to end", done)
	if want := "admitted a client with no credential"; !strings.Contains(wait.message, want) {
		t.Errorf("the readiness wait ended with %q, want it to say %q", wait.message, want)
	}
}

// A client this fixture hands a test is listed on the server under that test's name, unless its
// caller passed a name of its own, which Connect lets it do. The name is read from the server
// rather than from the client: that name is what a leak report and an operator's own read of the
// server's connections have to name the test by.
// packages/envoy/internal/bus/name_test.go holds the same for Envoy's clients.
func TestTheFixtureNamesItsClientAfterTheTest(t *testing.T) {
	Connect(t, URL(t))
	listedAs(t, t.Name())
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

// leakRecorder is t with Errorf and Fatalf recorded rather than failing the running test, so a row
// runs the leak check mid-test and reads what it reported. The check reports a fixture client
// through Errorf and a client the shared wait gave up on through Fatalf, which ends the goroutine
// as t.Fatalf does, so a row runs the check on one of its own (runCheck). With expired set, its
// deadline has passed, so a wait under it asks once and gives up.
type leakRecorder struct {
	*testing.T
	expired  bool
	messages []string
}

func (r *leakRecorder) Deadline() (time.Time, bool) {
	if r.expired {
		return time.Now(), true
	}
	return r.T.Deadline()
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

// runCheck runs the leak check of t's server on check, from a goroutine of its own, and returns
// once it has ended.
func runCheck(t *testing.T, check *leakRecorder) {
	t.Helper()
	s := serverOf(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.leakCheck(check)
	}()
	testwait.Receive(t, "the leak check to end", done)
}

// cleanupDropped is t with Cleanup dropped, so a client or a server the fixture hands it is one
// nothing registers the close or the stop of.
type cleanupDropped struct{ testing.TB }

func (cleanupDropped) Cleanup(func()) {}

// clientsLeftOpen takes t's server, opens two clients of the fixture's on it — one the fixture
// names after t, one its caller names ownName — and runs the leak check while both are open. It
// answers what the check reported and the two connections the server listed for those clients, by
// name.
func clientsLeftOpen(t *testing.T, ownName string) (*leakRecorder, map[string]Connection) {
	t.Helper()
	url := URL(t)
	Connect(t, url)
	Connect(t, url, nats.Name(ownName))
	listed := listedAs(t, t.Name(), ownName)
	recorder := &leakRecorder{T: t}
	runCheck(t, recorder)
	return recorder, listed
}

// listedAs waits for t's server to list the client connections named want, and no others, and
// answers them by name. A client is connected when its dial returns, which is after the server
// has read its CONNECT, but the wait costs nothing on that path and holds should a later
// nats-server answer the dial first.
func listedAs(t *testing.T, want ...string) map[string]Connection {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("the NATS server of %s to list the clients %q and no others", t.Name(), want), func() (map[string]Connection, bool) {
		listed := map[string]Connection{}
		for _, c := range Connections(t) {
			listed[c.Name] = c
		}
		if len(listed) != len(want) {
			return listed, false
		}
		for _, name := range want {
			if _, open := listed[name]; !open {
				return listed, false
			}
		}
		return listed, true
	})
}
