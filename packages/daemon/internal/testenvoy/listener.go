// Package testenvoy starts a real Envoy listener for a daemon test, in place of the listener
// stand-in the daemon's other live proofs read their own publishes back from.
//
// this change's checks need the real one twice over: R4's fixes are in the session's plugin and
// must work against the listener production runs, and R5's mark is read out of that listener's own
// session list (`GET /v1/sessions?include=unresponsive`), whose answer no stand-in's shape can
// stand for. Start runs the binary its caller names as its own process against a NATS server the
// caller names, as `internal/intake`'s redelivery proof runs it. A proof of those checks names the
// listener production pins (PinnedBinary, pinned.go), built from the commit its image carries; this
// package's own tests, which hold the helper rather than a requirement, name this checkout's
// (`testbuild.EnvoyBinary(t, "./cmd/listener")`).
//
// It is an ordinary package rather than a _test.go file so every test package can import it; Go
// cannot share test-only code across packages. A package that builds the checkout's listener
// runs `testbuild.Main` and, where it takes the shared NATS server, `testnats.Main` from its own
// TestMain, since the binary it builds is removed by the first and the server emptied by the
// second.
package testenvoy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// readyTimeout bounds the wait for a started listener to report healthy: NATS connected, its
// stream reconciled and its durable bound. A listener that is not healthy by then has not been
// made ready by anything the test can wait longer for.
const readyTimeout = 60 * time.Second

// stopTimeout is how long a stopping listener has to answer SIGINT before it is killed.
const stopTimeout = 10 * time.Second

// Options are what a test asks of the listener it starts. Every zero value leaves the listener's
// own default, which is what production runs: a 5-minute live window, a 1-hour gone horizon and
// the session bucket's own maximum age.
type Options struct {
	// LiveWindow is ENVOY_SESSION_LIVE_WINDOW: how long after its last registration a session
	// reads live. A check that must see a session read unresponsive in seconds sets it.
	LiveWindow time.Duration
	// GoneHorizon is ENVOY_SESSION_GONE_HORIZON: how long a session then reads unresponsive
	// before it reads gone.
	GoneHorizon time.Duration
	// SessionAge is ENVOY_SESSION_MAX_AGE: how long the session bucket keeps a registration
	// record at all. A horizon longer than this age reads a record the bucket dropped as gone
	// before the horizon, so a check that drives a long silence sets both.
	SessionAge time.Duration
	// MachineID is ENVOY_MACHINE_ID, the machine this listener says it serves; the test's own
	// name by default.
	MachineID string
	// Token is the bearer every call carries (ENVOY_API_TOKEN). A token of the helper's own by
	// default, so no test reaches a listener that admits anyone.
	Token string
	// NATSSeedFile is NATS_NKEY_SEED_FILE: the file holding the nkey seed of the NATS user the
	// listener connects as, which a server with production's grants requires (the listener's own
	// user, holding core.ListenerHostPermissions). Empty connects with no credential.
	NATSSeedFile string
	// Env is anything else the listener's environment carries, each `NAME=value`, for a check
	// that drives a setting this type does not name.
	Env []string
}

// Listener is a running Envoy listener of one test.
type Listener struct {
	baseURL string
	token   string
	output  *testlog.Buffer
	binary  string
	env     []string
	port    int

	// mu guards the process now running, which RestartOnExit replaces, and whether the test has
	// stopped it.
	mu       sync.Mutex
	exited   chan struct{}
	cmd      *exec.Cmd
	stopped  bool
	restarts []Restart

	// tokenFile is written on the first TokenFile call and named again after it.
	tokenFileOnce sync.Once
	tokenFile     string
}

// Restart is one exit of a listener RestartOnExit supervises: when its process exited, how, and
// when the next one was started and answered healthy (zero while it has not).
type Restart struct {
	Exited, Started, Healthy time.Time
	State                    string
	Err                      error
}

// BaseURL is where the listener serves, with no trailing slash: what a daemon's `envoy_url` is
// set to.
func (l *Listener) BaseURL() string { return l.baseURL }

// Token is the bearer every call to it carries (`envoy_token_file`'s content).
func (l *Listener) Token() string { return l.token }

// TokenFile is a file holding the bearer, written once and named again for every later caller:
// the daemon reads the listener's token from a file (`envoy_token_file`), so a live proof names
// this path in its configuration rather than carrying the token through it.
func (l *Listener) TokenFile(t testing.TB) string {
	t.Helper()
	l.tokenFileOnce.Do(func() {
		path := filepath.Join(t.TempDir(), "envoy-token")
		if err := os.WriteFile(path, []byte(l.token), 0o600); err != nil {
			t.Fatalf("write the listener's token file: %v", err)
		}
		l.tokenFile = path
	})
	return l.tokenFile
}

// Output is everything the listener has written to its stdout and stderr so far, for a check that
// reads its own lines — a registration it accepted, a role it refused — and for the failure
// message of one that does not get what it waited for.
func (l *Listener) Output() string { return l.output.String() }

// Start runs the listener binary against the NATS at natsURL and returns once it reports healthy.
// It is stopped when t ends.
//
// The port is reserved for the test's whole run (ReservePort), and held open here and handed to
// the listener as a descriptor (ENVOY_LISTEN_FD), as `internal/intake`'s proof does, so no other
// process of a loaded host takes the port between the kernel choosing it and the listener binding
// it, nor between an exit of the listener and RestartOnExit's start of it again on the same port.
func Start(t testing.TB, binary, natsURL string, options Options) *Listener {
	t.Helper()

	port := ReservePort(t)
	held, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("hold the listener's reserved loopback port %d: %v", port, err)
	}

	token := options.Token
	if token == "" {
		token = "testenvoy-listener-token"
	}
	machine := options.MachineID
	if machine == "" {
		machine = "testenvoy"
	}
	environment := []string{
		"PORT=" + strconv.Itoa(port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_LISTEN_FD=3",
		"ENVOY_MACHINE_ID=" + machine,
		"NATS_URLS=" + natsURL,
		"ENVOY_API_TOKEN=" + token,
	}
	if options.NATSSeedFile != "" {
		environment = append(environment, "NATS_NKEY_SEED_FILE="+options.NATSSeedFile)
	}
	for name, value := range map[string]time.Duration{
		"ENVOY_SESSION_LIVE_WINDOW":  options.LiveWindow,
		"ENVOY_SESSION_GONE_HORIZON": options.GoneHorizon,
		"ENVOY_SESSION_MAX_AGE":      options.SessionAge,
	} {
		if value > 0 {
			environment = append(environment, name+"="+value.String())
		}
	}
	environment = append(environment, options.Env...)

	listener := &Listener{
		baseURL: "http://127.0.0.1:" + strconv.Itoa(port),
		token:   token,
		output:  &testlog.Buffer{},
		binary:  binary,
		env:     environment,
		port:    port,
	}
	if err := listener.spawn(held.(*net.TCPListener)); err != nil {
		t.Fatalf("start the listener: %v", err)
	}
	t.Cleanup(func() { listener.stop(t) })

	if err := listener.waitHealthy(); err != nil {
		t.Fatalf("%v\nlistener output:\n%s", err, listener.output.String())
	}
	return listener
}

// spawn starts the listener's process on held, the port's socket, which it hands the process as
// its descriptor and closes here.
func (l *Listener) spawn(held *net.TCPListener) error {
	socket, err := held.File()
	_ = held.Close()
	if err != nil {
		return fmt.Errorf("take the held socket's descriptor: %w", err)
	}
	cmd := exec.Command(l.binary)
	cmd.Env = l.env
	cmd.ExtraFiles = []*os.File{socket}
	cmd.Stdout, cmd.Stderr = l.output, l.output
	err = cmd.Start()
	_ = socket.Close()
	if err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	l.mu.Lock()
	if l.stopped {
		// The test stopped the listener while this one started: it goes with the rest.
		l.mu.Unlock()
		_ = cmd.Process.Kill()
		<-exited
		return errors.New("the test stopped the listener")
	}
	l.cmd, l.exited = cmd, exited
	l.mu.Unlock()
	return nil
}

// current is the process running now and the channel its exit closes.
func (l *Listener) current() (*exec.Cmd, chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cmd, l.exited
}

// RestartOnExit starts the listener again on its own port, its environment and its output the
// same, each time its process exits before the test stops it, after delay: what the Kubernetes
// Deployment production's listener runs under does for a container that exits (restartPolicy
// Always, its first restart 10 s after the exit). The pinned listener ends itself when it has read
// its NATS connection as down for 20 self-health probes 30 s apart (cmd/listener/main.go's
// disconnectedFailureThreshold and runSelfHealthMonitor at PinnedRevision), "so Docker can replace"
// it, so a check that keeps NATS down for 10 minutes needs the restart production gives it.
func (l *Listener) RestartOnExit(delay time.Duration) {
	go func() {
		for {
			_, exited := l.current()
			<-exited
			l.mu.Lock()
			if l.stopped {
				l.mu.Unlock()
				return
			}
			// Recorded at the exit, so a reader between the exit and the restart sees it.
			l.restarts = append(l.restarts, Restart{Exited: time.Now(), State: l.cmd.ProcessState.String()})
			at := len(l.restarts) - 1
			l.mu.Unlock()
			time.Sleep(delay)
			err := l.respawn()
			started := time.Time{}
			if err == nil {
				started = time.Now()
				l.mu.Lock()
				l.restarts[at].Started = started
				l.mu.Unlock()
				err = l.waitHealthy()
			}
			l.mu.Lock()
			l.restarts[at].Err = err
			if err == nil {
				l.restarts[at].Healthy = time.Now()
			}
			stopped := l.stopped
			l.mu.Unlock()
			if stopped || started.IsZero() {
				return
			}
		}
	}()
}

// respawn holds the listener's own port again, which its exit closed and Start's reservation kept
// from every other socket, and starts it there.
func (l *Listener) respawn() error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		l.mu.Lock()
		stopped := l.stopped
		l.mu.Unlock()
		if stopped {
			return errors.New("the test stopped the listener")
		}
		held, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(l.port))
		if err == nil {
			return l.spawn(held.(*net.TCPListener))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("hold the listener's port %d again: %w", l.port, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Restarts is every exit RestartOnExit restarted the listener after, oldest first.
func (l *Listener) Restarts() []Restart {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Restart(nil), l.restarts...)
}

// Stop ends the listener before t does, for a check that drives what a daemon does while Envoy is
// not there at all. Stopping one that has already stopped changes nothing.
func (l *Listener) Stop(t testing.TB) {
	t.Helper()
	l.stop(t)
}

func (l *Listener) stop(t testing.TB) {
	t.Helper()
	l.mu.Lock()
	l.stopped = true
	cmd, exited := l.cmd, l.exited
	l.mu.Unlock()
	select {
	case <-exited:
		return
	default:
	}
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-exited:
	case <-time.After(stopTimeout):
		_ = cmd.Process.Kill()
		<-exited
		t.Errorf("the listener did not stop within %s of SIGINT:\n%s", stopTimeout, l.output.String())
	}
}

// waitHealthy returns once /healthz says healthy, and fails naming what it last read. A listener
// that exits while it is waited for is named as that, since nothing will make it healthy.
func (l *Listener) waitHealthy() error {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(readyTimeout)
	var last string
	for time.Now().Before(deadline) {
		_, exited := l.current()
		select {
		case <-exited:
			return fmt.Errorf("the listener exited before it was healthy")
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL+"/healthz", nil)
		if err != nil {
			cancel()
			return fmt.Errorf("build the health request: %w", err)
		}
		response, err := client.Do(request)
		if err == nil {
			var health struct {
				Status string `json:"status"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&health)
			_ = response.Body.Close()
			cancel()
			if decodeErr == nil && health.Status == "healthy" {
				return nil
			}
			last = health.Status
		} else {
			cancel()
			last = err.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the listener was not healthy within %s; last read %q", readyTimeout, last)
}
