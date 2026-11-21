package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/listenfd"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const testOperatorToken = "operator-bearer-for-daemon-tests"

// testDSN is the one database this run's tests write in, made by TestMain on the devbox and CI
// Postgres LEGION_TEST_PG_DSN names (testdb.RunDatabaseMain). The daemon has no in-memory mode:
// what it does on a boot is what its store recorded, and a boot migrates Legion's schema into
// whatever database it is given, so the run owns one rather than sharing the server's. A test
// that sets LEGION_TEST_PG_DSN itself, to a server its own test needs, gets the value it set.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip(testdb.SkipMessage)
	}
	return testdb.RunDatabaseFor(dsn)
}

// shortTempDir is a state directory short enough for the worker stream's unix socket: a socket
// path is bounded at 108 bytes, and t.TempDir() spells the whole test name. The budget asked of
// testtmp.Dir is streamSocket, the socket daemon.go binds under the state directory.
func shortTempDir(t *testing.T) string {
	t.Helper()
	return testtmp.Dir(t, streamSocket)
}

// testConfig is a daemon of its own: its project name is unique, so the boots and claims it
// records are its own, its port is one nothing else holds, and its state directory is its alone.
// Every limit and timeout is the shipped default.
func testConfig(t *testing.T) config.Config {
	t.Helper()
	stateDir := shortTempDir(t)
	tokenFile := filepath.Join(t.TempDir(), "operator-token")
	if err := os.WriteFile(tokenFile, []byte(testOperatorToken+"\n"), 0o600); err != nil {
		t.Fatalf("write the operator token: %v", err)
	}
	// The API port is held from here until the daemon binds it (heldListen): a port found free
	// and closed again is any process's to take first. The worker stream port is always 0: the
	// listener resolves it at bind time and reports the kernel's choice, so nothing needs holding
	// for it. zero is a value config.Load refuses (config.go:871-874, "worker_stream_port must be a
	// positive integer"): it is a fixture, never a configuration. The derivation from a real port
	// is pinned by TestPrepareDerivesTheWorkerStreamAddressFromWorkerStreamPort.
	port := holdPortOn(t, "127.0.0.1")
	return config.Config{
		// A key Dispatch can hold (projects.key, `^[A-Z][A-Z0-9]{1,9}$`), which a workflow daemon
		// refuses to boot without (claim.IsProjectKey, its boot event): nine of randomSuffix's hex
		// digits, 36 random bits, keep each test's project its own.
		Project:                                 "T" + randomSuffix(t)[:9],
		Port:                                    port,
		Bind:                                    "127.0.0.1",
		PostgresDSN:                             testDSN(t),
		StateDir:                                stateDir,
		Runtime:                                 config.Runtime{Name: "tmux"},
		AdmissionCap:                            4,
		DaemonURL:                               "http://127.0.0.1:" + strconv.Itoa(port),
		WorkerStreamPort:                        0,
		WorkerBootTimeout:                       120 * time.Second,
		WorkerBootRegistrationDeadlineIntervals: 3,
		WorkerRPCTimeout:                        5 * time.Second,
		WorkerStopTimeout:                       10 * time.Second,
		TreeStopTimeout:                         60 * time.Second,
		SlowCommandTimeout:                      300 * time.Second,
		ProbeInterval:                           30 * time.Second,
		LaunchFailureLimit:                      3,
		PromptFailureLimit:                      3,
		PromptRetireLimit:                       2,
		OperatorTokenFile:                       tokenFile,
		EnvoyURL:                                "http://127.0.0.1:9020",
		ControllerLaunch:                        config.ControllerLaunchOperator,
	}
}

// copyDir copies the tree under source to destination.
func copyDir(t *testing.T, source, destination string) {
	t.Helper()
	if err := fs.WalkDir(os.DirFS(source), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(destination, path)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		body, err := os.ReadFile(filepath.Join(source, path))
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	}); err != nil {
		t.Fatalf("copy %s: %v", source, err)
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

// The API port testConfig hands a daemon is held until the daemon binds it: another process
// asking for it in between is refused, and the daemon still boots on it. The worker stream's
// port is never held — nothing is handed out for it — since the listener resolves port 0 itself
// at bind time; the daemon still ends up serving a real, dialable address.
func TestThePortsHandedToADaemonCannotBeTakenBeforeItBinds(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.Runtime = kubernetesConfig(t, "https://127.0.0.1:1").Runtime
	address := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))
	if thief, err := net.Listen("tcp", address); err == nil {
		_ = thief.Close()
		t.Fatalf("another listener took %s between the pick and the daemon's bind", address)
	}
	record := &built{}
	o := fakeRuntime(fake.NewRuntime(), record)
	o.workflowTokens = &workflowTokenRecorder{}
	startDaemon(t, cfg, o)
	record.mu.Lock()
	streamAddress := record.address
	record.mu.Unlock()
	prefix := "tcp://" + cfg.Bind + ":"
	if !strings.HasPrefix(streamAddress, prefix) || strings.HasSuffix(streamAddress, ":0") {
		t.Fatalf("the worker stream listens on %q, want %s<bound port>", streamAddress, prefix)
	}
	if conn, err := net.DialTimeout("tcp", strings.TrimPrefix(streamAddress, "tcp://"), time.Second); err != nil {
		t.Errorf("the worker stream does not accept on %s: %v", streamAddress, err)
	} else {
		conn.Close()
	}
}

// heldPorts are the listeners holdPortOn holds, by address, until a daemon takes one (heldListen).
var heldPorts = struct {
	sync.Mutex
	byAddress map[string]net.Listener
}{byAddress: map[string]net.Listener{}}

// holdPortOn listens on a free port of host for t and returns it. The listener is held until a
// daemon of t's takes it through heldListen, or t ends. It is held under net.JoinHostPort(host,
// port), the address the daemon builds from bind, not the listener's Addr(): Go reports a 0.0.0.0
// listener as [::]:<port>, the dual-stack wildcard.
func holdPortOn(t *testing.T, host string) int {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("hold a free port on %s: %v", host, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	address := net.JoinHostPort(host, strconv.Itoa(port))
	heldPorts.Lock()
	heldPorts.byAddress[address] = listener
	heldPorts.Unlock()
	t.Cleanup(func() {
		heldPorts.Lock()
		delete(heldPorts.byAddress, address)
		heldPorts.Unlock()
		_ = listener.Close()
	})
	return port
}

// takeSocket binds address, whose port may be 0 for one the kernel chooses, and returns a
// descriptor of its own for the listening socket, with the port it holds: the socket stays
// listening once the listener is closed, so no other process can take the port before whatever is
// handed the descriptor serves it. A port another process holds is the bind's error.
func takeSocket(address string) (*os.File, int, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	file, err := listener.(*net.TCPListener).File()
	_ = listener.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("take the descriptor of %s: %w", address, err)
	}
	return file, port, nil
}

// heldSocket holds a loopback port the kernel chooses (takeSocket). The descriptor is closed when
// t ends, if its caller has not closed it first.
func heldSocket(t *testing.T) (*os.File, int) {
	t.Helper()
	file, port, err := takeSocket("127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file, port
}

// rebindHeldPorts hands cfg a freshly held API port. A daemon that has stopped closed the
// listener it was handed, and from then on the port it used is any process's, so a test that
// starts a second daemon on the same config holds one again first. The worker stream needs no
// rebinding: it is always port 0, resolved fresh on every bind.
func rebindHeldPorts(t *testing.T, cfg *config.Config) {
	t.Helper()
	cfg.Port = holdPortOn(t, "127.0.0.1")
	cfg.DaemonURL = "http://127.0.0.1:" + strconv.Itoa(cfg.Port)
}

// heldListen is the daemon's listen under test: the listener holdPortOn holds for address, handed
// over once. An address nothing holds is refused rather than bound, since a port found free and
// bound later is the race holdPortOn closes: a test that starts a daemon again on one config calls
// rebindHeldPorts first.
func heldListen(network, address string) (net.Listener, error) {
	heldPorts.Lock()
	listener, held := heldPorts.byAddress[address]
	delete(heldPorts.byAddress, address)
	heldPorts.Unlock()
	if !held {
		return nil, &net.AddrError{Err: "no held listener: call rebindHeldPorts before starting a daemon again on the same config", Addr: address}
	}
	return listener, nil
}

// pollClient bounds each poll of a daemon's port: holdPortOn's listener queues a connection until a
// daemon takes the listener and serves it, and a daemon that exits before then never answers it.
var pollClient = &http.Client{Timeout: time.Second}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// stillClock is a clock whose timers never fire: the machines arm them and nothing calls them
// back, and a Sleep returns at once, so a test sees only what it drove. Its Now is the wall
// clock, so what is due by the time of day — an outbox row's next_at — still comes due as the
// run goes on.
type stillClock struct{}

func (stillClock) Now() time.Time { return time.Now() }

func (stillClock) AfterFunc(time.Duration, func()) supervise.Cancel { return stopped{} }

func (stillClock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

type stopped struct{}

func (stopped) Stop() bool { return true }

// built is what the daemon handed the runtime it built: the connection directory, the address
// every pane's shim dials, the workflow's App tokens and repository rule, the store it reads, and
// the cache set, the repositories the Sandbox runtime keeps a repository cache of.
type built struct {
	mu         sync.Mutex
	conns      runtime.Conns
	address    string
	apps       appauth.Source
	repository func(context.Context, string) (ghrepo.Repository, string, error)
	store      *store.Store
	cached     func() []ghrepo.Repository
}

// fakeRuntime is a daemon whose runtime is rt — the fake, or a test's runtime built over it: the
// real stream listener, store, and machines, with nothing launched for real.
func fakeRuntime(rt runtime.Runtime, record *built) overrides {
	return overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, apps appauth.Source,
			repository func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, cached func() []ghrepo.Repository) (runtime.Runtime, error) {
			record.mu.Lock()
			defer record.mu.Unlock()
			record.conns, record.address, record.apps, record.repository, record.store, record.cached = listener, address, apps, repository, st, cached
			return rt, nil
		},
		clock:  stillClock{},
		listen: heldListen,
	}
}

// boots is what the store records of cfg's project. The schema is brought forward first, so a test
// that asserts a refused start recorded nothing reads an answer even on a database no daemon has
// migrated yet — a fresh CI service, when that test is the first to touch it.
func boots(t *testing.T, cfg config.Config) (int, time.Time) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	count, firstBootAt, err := st.Boots(ctx, cfg.Project)
	if err != nil {
		t.Fatalf("read the boots: %v", err)
	}
	return count, firstBootAt
}

// A daemon told to stop once its store is open still owes the store a recorded, stamped boot: the
// restart count and the first boot time are what `legion state` answers from. The stop arrives as
// the runtime is built, after the store opened and before the boot is recorded; a stop that
// arrives before the store opens ends the start with nothing recorded
// (TestRunStopsAtOnceWhileItWaitsToOpenItsStore).
func TestRunRecordsEveryBootAndKeepsTheFirstBootTime(t *testing.T) {
	cfg := testConfig(t)

	if err := runStoppedOnceItsStoreIsOpen(cfg); err != nil {
		t.Fatalf("first run: %v", err)
	}
	count, firstBootAt := boots(t, cfg)
	if count != 1 {
		t.Fatalf("boots after one run = %d, want 1", count)
	}
	if firstBootAt.IsZero() {
		t.Fatal("the first boot has no recorded time")
	}

	rebindHeldPorts(t, &cfg)
	if err := runStoppedOnceItsStoreIsOpen(cfg); err != nil {
		t.Fatalf("second run: %v", err)
	}
	countAgain, firstBootAgain := boots(t, cfg)
	if countAgain != count+1 {
		t.Fatalf("boots after a restart = %d, want %d", countAgain, count+1)
	}
	if !firstBootAgain.Equal(firstBootAt) {
		t.Fatalf("the first boot time moved on a restart: %s, was %s", firstBootAgain, firstBootAt)
	}
}

// runStoppedOnceItsStoreIsOpen runs the daemon on cfg with a stop that arrives as its runtime is
// built (openSupervision): after the store opened and migrated, before the boot is recorded.
func runStoppedOnceItsStoreIsOpen(cfg config.Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := fakeRuntime(fake.NewRuntime(), &built{})
	build := o.runtime
	o.runtime = func(buildCtx context.Context, listener *stream.Listener, address string, apps appauth.Source,
		repository func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, mapped func() []ghrepo.Repository) (runtime.Runtime, error) {
		cancel()
		return build(buildCtx, listener, address, apps, repository, st, mapped)
	}
	return run(ctx, cfg, quietLogger(), o)
}

// The refusal an operator reads when Postgres is never there comes after the start's ten attempts,
// each wait logged, and names where the daemon went and never how it would have got in
// (c-boot-db-retry case 5). The waits between the attempts are a millisecond here; the number of
// attempts is the production policy's.
func TestRunRefusesAnUnreachablePostgresByHostAndNotByPassword(t *testing.T) {
	cfg := testConfig(t)
	cfg.PostgresDSN = secretDSN("127.0.0.1:1")
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.storeOpenRetry = &bootprobe.Retry{Initial: time.Millisecond, Max: time.Millisecond, Attempts: storeOpenRetry.Attempts}
	var logs syncBuffer

	refused := make(chan error, 1)
	go func() {
		refused <- run(context.Background(), cfg, slog.New(slog.NewJSONHandler(&logs, nil)), o)
	}()

	select {
	case err := <-refused:
		if err == nil {
			t.Fatal("run started without a Postgres to start against")
		}
		if want := fmt.Sprintf("within its retry budget (%d attempts): connect to Postgres at 127.0.0.1:1: ", storeOpenRetry.Attempts); !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal = %v, want it to hold %q", err, want)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the refusal leaks the DSN password: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not refuse an unreachable Postgres within 10s")
	}
	waits := storeOpenWaits(t, logs.String())
	if len(waits) != storeOpenRetry.Attempts-1 {
		t.Fatalf("the start logged %d waits between its %d attempts, want %d; the log holds:\n%s", len(waits), storeOpenRetry.Attempts, storeOpenRetry.Attempts-1, logs.String())
	}
	for n, wait := range waits {
		if want := fmt.Sprintf("%d/%d", n+1, storeOpenRetry.Attempts); wait["attempt"] != want {
			t.Errorf("wait %d is attempt %v, want %s", n+1, wait["attempt"], want)
		}
		detail, _ := wait["detail"].(string)
		if !strings.Contains(detail, "127.0.0.1:1") || strings.Contains(detail, "hunter2") {
			t.Errorf("wait %d's detail = %q, want the host it could not reach and not the password", n+1, detail)
		}
	}
}

// secretDSN is a DSN for a Postgres at address whose password, hunter2, a refusal must never
// print. It is built from its parts rather than written as one literal, because the pre-push leak
// guard refuses a URL that holds a password in its userinfo.
func secretDSN(address string) string {
	return (&url.URL{Scheme: "postgres", User: url.UserPassword("legion", "hunter2"), Host: address, Path: "/legion", RawQuery: "sslmode=disable"}).String()
}

// pgConnection is what pgProxy does with one connection it accepts.
type pgConnection int

const (
	// forwardConnection relays the connection to the Postgres behind the proxy, both ways.
	forwardConnection pgConnection = iota
	// resetConnection ends the connection with a TCP reset (SO_LINGER 0) as soon as it is accepted.
	resetConnection
	// silentConnection holds the connection open and never answers it.
	silentConnection
)

// pgProxy is dsn's Postgres reached through a TCP proxy on 127.0.0.1 that does with the n-th
// connection it accepts what plan[n] says, and forwards every connection after the plan's last.
// The URL it returns is dsn at the proxy's address with sslmode=disable: under pgx's default,
// prefer, one attempt to open the store can open a TLS connection and then a plain one, and a
// test of the attempts counts one connection to each. The proxy and every connection through it
// close when the test ends.
func pgProxy(t *testing.T, dsn string, plan ...pgConnection) string {
	t.Helper()
	upstream, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("read the address of the Postgres behind the proxy: %v", err)
	}
	server := net.JoinHostPort(upstream.Host, strconv.Itoa(int(upstream.Port)))
	proxied, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse the URL of the Postgres behind the proxy: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the Postgres proxy: %v", err)
	}
	var mu sync.Mutex
	var held []net.Conn
	closed := false
	// hold keeps conns until the test ends, or closes them at once when it has.
	hold := func(conns ...net.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			for _, conn := range conns {
				_ = conn.Close()
			}
			return false
		}
		held = append(held, conns...)
		return true
	}
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		closed = true
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	go func() {
		for n := 0; ; n++ {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			handling := forwardConnection
			if n < len(plan) {
				handling = plan[n]
			}
			switch handling {
			case resetConnection:
				_ = client.(*net.TCPConn).SetLinger(0)
				_ = client.Close()
			case silentConnection:
				hold(client)
			default:
				go func() {
					postgres, err := net.Dial("tcp", server)
					if err != nil {
						_ = client.Close()
						return
					}
					if !hold(client, postgres) {
						return
					}
					go func() {
						_, _ = io.Copy(postgres, client)
						_ = postgres.Close()
						_ = client.Close()
					}()
					_, _ = io.Copy(client, postgres)
					_ = postgres.Close()
					_ = client.Close()
				}()
			}
		}
	}()
	proxied.Host = listener.Addr().String()
	query := proxied.Query()
	query.Set("sslmode", "disable")
	proxied.RawQuery = query.Encode()
	return proxied.String()
}

// refusingPostgres answers each connection's startup message with one FATAL ErrorResponse of code
// and message, as a Postgres that refuses the connection answers it, and returns its address and
// the count of the connections it has accepted.
func refusingPostgres(t *testing.T, code, message string) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := &atomic.Int32{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer conn.Close()
				backend := pgproto3.NewBackend(conn, conn)
				if _, err := backend.ReceiveStartupMessage(); err != nil {
					return
				}
				backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: code, Message: message})
				_ = backend.Flush()
			}()
		}
	}()
	return listener.Addr().String(), accepted
}

// storeOpenWaits is each wait the daemon logged between two attempts to open its store.
func storeOpenWaits(t *testing.T, logs string) []map[string]any {
	t.Helper()
	return slices.DeleteFunc(loggedMessages(t, logs, "boot probe failed transiently; waiting to run it again"), func(line map[string]any) bool {
		return line["probe"] != "Postgres store"
	})
}

// A Postgres whose first two connections are reset opens on the third attempt, and the daemon
// serves (c-boot-db-retry case 1). Each wait is logged with its attempt, its wait and the reset,
// after one line that names the bound of the attempts.
func TestRunOpensItsStoreOnTheThirdAttemptAfterTwoResets(t *testing.T) {
	cfg := testConfig(t)
	cfg.PostgresDSN = pgProxy(t, cfg.PostgresDSN, resetConnection, resetConnection)
	proxy, err := url.Parse(cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	startDaemonLogging(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}), slog.New(slog.NewJSONHandler(&logs, nil)))

	waits := storeOpenWaits(t, logs.String())
	if len(waits) != 2 {
		t.Fatalf("the start logged %d waits to open its store, want 2, one after each reset; the log holds:\n%s", len(waits), logs.String())
	}
	for n, want := range []struct{ attempt, retryIn string }{{"1/10", "1s"}, {"2/10", "2s"}} {
		wait := waits[n]
		if wait["attempt"] != want.attempt || wait["retryIn"] != want.retryIn {
			t.Errorf("wait %d is attempt %v, retryIn %v; want attempt %s, retryIn %s", n+1, wait["attempt"], wait["retryIn"], want.attempt, want.retryIn)
		}
		detail, _ := wait["detail"].(string)
		if !strings.Contains(detail, "connect to Postgres at "+proxy.Host) || !strings.Contains(detail, "reset by peer") {
			t.Errorf("wait %d's detail = %q, want the reset at %s", n+1, detail, proxy.Host)
		}
	}
	bound := loggedMessages(t, logs.String(), "daemon: opening the store; a connection error that the next attempt can clear is tried again")
	if len(bound) != 1 {
		t.Fatalf("the start logged %d lines naming the bound of its attempts, want 1; the log holds:\n%s", len(bound), logs.String())
	}
	for key, want := range map[string]any{"attempts": float64(10), "attemptLimit": "10s", "firstWait": "1s", "longestWait": "16s", "longestTotal": "3m15s"} {
		if bound[0][key] != want {
			t.Errorf("the bound's %s = %v, want %v", key, bound[0][key], want)
		}
	}
}

// A Postgres that refuses the daemon's password answers the same at every attempt, so the start
// exits at its first refusal (c-boot-db-retry case 3): one connection and no wait, and the
// refusal names the host and not the password.
func TestRunExitsAtOnceWhenPostgresRefusesThePassword(t *testing.T) {
	address, connections := refusingPostgres(t, "28P01", `password authentication failed for user "legion"`)
	cfg := testConfig(t)
	cfg.PostgresDSN = secretDSN(address)
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refused := make(chan error, 1)
	go func() {
		refused <- run(ctx, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), fakeRuntime(fake.NewRuntime(), &built{}))
	}()

	select {
	case err := <-refused:
		if err == nil {
			t.Fatal("run started against a Postgres that refuses its password")
		}
		if !strings.Contains(err.Error(), address) || !strings.Contains(err.Error(), "SQLSTATE 28P01") {
			t.Errorf("the refusal = %v, want it to name %s and the server's 28P01", err, address)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the refusal leaks the DSN password: %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		<-refused
		t.Errorf("run did not exit within 5s of a refused password")
	}
	if n := connections.Load(); n != 1 {
		t.Errorf("the start made %d connections to a Postgres that refused its password, want 1", n)
	}
	if waits := storeOpenWaits(t, logs.String()); len(waits) != 0 {
		t.Errorf("the start waited to try a refused password again: %v", waits)
	}
}

// A stop that arrives while the start waits to open its store again ends the start at once and
// cleanly (c-boot-db-retry case 6): run returns nil, names the stop in its log, and records no
// boot.
func TestRunStopsAtOnceWhileItWaitsToOpenItsStore(t *testing.T) {
	cfg := testConfig(t)
	// The store the start would have recorded its boot in, which boots reads.
	recorded := cfg
	cfg.PostgresDSN = secretDSN("127.0.0.1:1")
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), fakeRuntime(fake.NewRuntime(), &built{}))
	}()

	var ended error
	returned := false
	if !testwait.Holds(10*time.Second, testwait.Poll, func() bool {
		select {
		case ended = <-done:
			returned = true
			return true
		default:
		}
		return strings.Contains(logs.String(), `"attempt":"2/10"`)
	}) {
		t.Fatalf("the start logged no second wait to open its store within 10s; the log holds:\n%s", logs.String())
	}
	if returned {
		t.Fatalf("run returned %v before its second wait to open its store; the log holds:\n%s", ended, logs.String())
	}
	cancel()
	stopped := time.Now()
	select {
	case err := <-done:
		took := time.Since(stopped)
		if err != nil {
			t.Fatalf("run stopped during its wait = %v, want a clean stop", err)
		}
		if took > 200*time.Millisecond {
			t.Errorf("run returned %s after its stop, want within 200ms", took)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not return within 30s of its stop")
	}
	if lines := loggedMessages(t, logs.String(), "legion daemon stopped before it opened its store"); len(lines) != 1 || lines[0]["project"] != cfg.Project {
		t.Errorf("the stop's lines = %v, want one naming project %s", lines, cfg.Project)
	}
	if count, _ := boots(t, recorded); count != 0 {
		t.Errorf("boots after a start stopped before its store opened = %d, want 0", count)
	}
}

// A recorded boot is a boot that served. A daemon whose port is already taken never ran, and a
// row for it would inflate `daemon.boots` — the value `legion state` answers with and the stage
// gate asserts on.
func TestRunRecordsNoBootWhenItCannotTakeItsPort(t *testing.T) {
	cfg := testConfig(t)
	address := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))
	// The listener testConfig holds for the port is the other listener: the daemon binds the port
	// itself here, rather than take the held one.
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.listen = nil

	err := run(context.Background(), cfg, quietLogger(), o)
	if err == nil {
		t.Fatal("run returned no error although another listener held its port")
	}
	if !strings.Contains(err.Error(), address) {
		t.Errorf("the refusal does not name the address it could not take: %v", err)
	}
	if count, _ := boots(t, cfg); count != 0 {
		t.Fatalf("boots after a start that never served = %d, want 0", count)
	}
}

// The boot itself says once which shared leak check the deployment runs: run logs it
// (logLeakCheck) before it takes its port, so a boot that then fails on its port has logged it all
// the same — the not-configured line under no setting, and the configured line, which names the
// setting's repository and checker, under the deployment's.
func TestRunLogsTheLeakCheckLineAtBoot(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		setting                   leakguard.Setting
		notConfigured, configured int
	}{
		{name: "no shared leak check", notConfigured: 1},
		{name: "the deployment's check", setting: leakCheckSetting, configured: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.LeakCheck = tc.setting
			o := fakeRuntime(fake.NewRuntime(), &built{})
			o.listen = nil
			var logged strings.Builder
			if err := run(context.Background(), cfg, slog.New(slog.NewJSONHandler(&logged, nil)), o); err == nil {
				t.Fatal("run returned no error although another listener held its port")
			}
			named := `"repository":"` + tc.setting.Repository.String() + `","manifest_sha256":"` + tc.setting.ManifestSHA256 + `"`
			if got := strings.Count(logged.String(), leakCheckNotConfigured); got != tc.notConfigured {
				t.Fatalf("the boot logged the not-configured line %d times, want %d; log:\n%s", got, tc.notConfigured, logged.String())
			}
			if got := strings.Count(logged.String(), leakCheckConfigured+`",`+named); got != tc.configured {
				t.Fatalf("the boot logged the configured line naming %s %d times, want %d; log:\n%s", named, got, tc.configured, logged.String())
			}
		})
	}
}

// `legion start` hands Run the socket it inherited (LEGION_LISTEN_FD), which a harness opened on a
// port the kernel chose and held from then on: the daemon serves its API on that socket without
// binding an address of its own, and its boot line names the address it serves on.
func TestRunServesTheSocketItIsHandedAndLogsItsAddress(t *testing.T) {
	cfg := testConfig(t)
	file, _ := heldSocket(t)
	// The descriptor `legion start` inherits is a bare number no *os.File owns, which Adopt closes.
	descriptor, err := syscall.Dup(int(file.Fd()))
	_ = file.Close()
	if err != nil {
		t.Fatalf("copy the held socket's descriptor: %v", err)
	}
	socket, err := listenfd.Inspect("LEGION_LISTEN_FD", strconv.Itoa(descriptor))
	if err != nil {
		_ = syscall.Close(descriptor)
		t.Fatalf("inspect the held socket: %v", err)
	}
	inherited, err := listenfd.Adopt(socket)
	if err != nil {
		t.Fatalf("adopt the held socket: %v", err)
	}
	address := socket.Addr().String()
	if thief, err := net.Listen("tcp", address); err == nil {
		_ = thief.Close()
		t.Fatalf("another listener took %s before the daemon served it", address)
	}
	cfg.Port = socket.Addr().Port
	cfg.DaemonURL = "http://" + address
	// Run's listen: the handed socket in place of any bind. The socket still holds cfg's address, so
	// a daemon that bound it rather than serve the socket would be refused, and never answer.
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.listen = handed(inherited)
	logs := &syncBuffer{}

	startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(logs, nil)))

	if logged := bootField(t, logs.String(), "address"); logged != address {
		t.Errorf("the boot line names address %q, want %q, the socket the daemon serves on", logged, address)
	}
}

// The API the daemon serves answers from the store it just migrated: this is the whole of
// `legion state` and of the plugin's read.
func TestRunServesTheStateOfItsOwnBoot(t *testing.T) {
	cfg := testConfig(t)
	d := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))

	state := d.state()
	if state.Daemon.Project != cfg.Project {
		t.Errorf("project = %q, want %q", state.Daemon.Project, cfg.Project)
	}
	if state.Daemon.SchemaVersion < 1 {
		t.Errorf("schema version = %d, want the migrated schema", state.Daemon.SchemaVersion)
	}
	if state.Daemon.Boots != 1 {
		t.Errorf("boots = %d, want this daemon's own first boot", state.Daemon.Boots)
	}
	if state.Daemon.StartedAt.IsZero() {
		t.Error("the running daemon reports no start time")
	}
	if state.Admission.Cap != cfg.AdmissionCap {
		t.Errorf("admission cap = %d, want the configured %d", state.Admission.Cap, cfg.AdmissionCap)
	}
	if len(state.Issues) != 0 {
		t.Errorf("issues = %v, want none before a claim is spawned", state.Issues)
	}

	d.stop()
	if _, err := d.client.Get(d.base + "/healthz"); err == nil {
		t.Error("the API still answers after run returned")
	}
}

// The state carries the daemon's resolved settings (api.State.Settings, contract 35): the values
// its configuration runs on, and nothing of its credentials. The configuration carries both Apps
// with a PEM and no Dispatch, so no workflow opens and no token is minted (openWorkflow); the body
// shows the Apps' ids and neither the key nor the store's DSN.
func TestTheStateCarriesTheDaemonsSettingsAndNoSecret(t *testing.T) {
	const privateKey = "-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----"
	cfg := testConfig(t)
	cfg.ReviewRoundCap = 5
	cfg.GitHubApps = config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1001", PrivateKey: privateKey, Installations: map[string]string{"sjawhar": "2001"}},
		Review:    config.GitHubApp{AppID: "1002", PrivateKey: privateKey, Installations: map[string]string{"sjawhar": "2002"}},
	}
	d := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))

	status, body := d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET the state = %d; body %s", status, body)
	}
	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	settings := state.Settings
	if settings.ReviewRoundCap != 5 || settings.WorkerBootTimeoutSeconds != 120 || settings.Runtime.Name != "tmux" {
		t.Errorf("settings review_round_cap %d, worker_boot_timeout_seconds %d, runtime.name %q; want the configured 5, 120, tmux",
			settings.ReviewRoundCap, settings.WorkerBootTimeoutSeconds, settings.Runtime.Name)
	}
	if settings.GitHubApps == nil || settings.GitHubApps.Implement.AppID != "1001" || settings.GitHubApps.Review.AppID != "1002" {
		t.Errorf("settings.github_apps = %+v, want both configured Apps by id", settings.GitHubApps)
	}
	for _, secret := range []string{"BEGIN", "postgres://", cfg.PostgresDSN} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("the state carries %q: %s", secret, body)
		}
	}
}

// A due lifecycle row held behind its issue's running one is on the state the daemon serves
// (api.State.HeldOutboxRows, contract 28). The architect's accepted retry_or_escalate of a held
// phase enqueues the implementer's start while the issue's lifecycle lane is still running an
// earlier row of the issue — here the planner's suspend, under a live lease — so GET
// /legion/v1/state lists each row the start is, by kind and id, with the row it waits on; once
// that row's lease has ended, nothing is held. The retry is applied through the workflow engine the phase/retry
// route applies it with (intake.ApplyFact): the API-only daemon this harness boots carries no
// workflow runtime, and so no lane that would run the rows the test seeds.
func TestTheStateListsTheHeldLifecycleRowBehindItsIssuesRunningOne(t *testing.T) {
	cfg := testConfig(t)
	issue := cfg.Project + "-1"
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	records := record.NewStore()
	held := runningRoot(issue, phase.Held)
	held.Hold = &record.Hold{From: phase.Implementing}
	suspend, err := record.NewOutboxRow(issue, record.SuperviseRequest{Op: "suspend", Tree: issue, Role: claim.RolePlanner, Generation: 1}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, held); err != nil {
			return err
		}
		return records.Enqueue(ctx, tx, suspend)
	}); err != nil {
		t.Fatalf("record the held issue and its running suspend: %v", err)
	}
	var running int64
	if err := st.Pool().QueryRow(ctx, "select id from outbox where issue = $1", issue).Scan(&running); err != nil {
		t.Fatalf("read the suspend's row: %v", err)
	}
	leaseOutboxRow(t, st.Pool(), running)

	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	engine := workflow.New(records, workflow.Config{Project: project, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	result, err := intake.ApplyFact(ctx, st.Pool(), "api", "retry:"+issue, intake.RetryOrEscalate{Issue: issue, Decision: intake.RetryDecision}, engine)
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply the retry = %+v, %v; want it accepted", result.Refusal, err)
	}
	// A phase worker's start is one outbox row, which brings the held role's session back and
	// carries its task (workflow.StartOrOwe). It is a lifecycle row of the issue, so the suspend
	// under a live lease holds it.
	var starts []int64
	rows, err := st.Pool().Query(ctx, "select id from outbox where issue = $1 and kind = $2 and payload->>'op' = 'start' order by id", issue, string(record.OutboxKindSupervise))
	if err != nil {
		t.Fatalf("read the retry's start rows: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan a start row: %v", err)
		}
		starts = append(starts, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the retry's start rows: %v", err)
	}
	if len(starts) != 1 {
		t.Fatalf("the retry queued %d start rows, want the one that carries its task", len(starts))
	}

	d := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))
	got := d.state().HeldOutboxRows
	if len(got) != len(starts) {
		t.Fatalf("heldOutboxRows = %+v, want the retry's one start row alone", got)
	}
	for i, id := range starts {
		if got[i].WaitedSeconds < 0 {
			t.Fatalf("heldOutboxRows[%d] = %+v, want a wait of zero or more seconds", i, got[i])
		}
		got[i].WaitedSeconds = 0
		if want := (api.HeldOutboxRow{ID: id, Kind: "supervise", Issue: issue, WaitsOn: running}); got[i] != want {
			t.Fatalf("heldOutboxRows[%d] = %+v, want %+v", i, got[i], want)
		}
	}

	if _, err := st.Pool().Exec(ctx, "update outbox set lease_until = now() - interval '1 minute' where id = $1", running); err != nil {
		t.Fatalf("end the suspend's lease: %v", err)
	}
	if got := d.state().HeldOutboxRows; got == nil || len(got) != 0 {
		t.Fatalf("heldOutboxRows once the suspend's lease ended = %+v, want an empty list", got)
	}
}

// The wave the wake pacing is letting out, as the state shows it (api.State.WakesWaiting, contract
// 53): this restart finds two launches the previous daemon did not finish and raises both, the
// burst lets one out at once and the pacing holds the other, and the state names the wake it holds
// — the claim it will wake, that claim's issue and role, what kind of wake it is and the moment it
// is due — until that moment comes, when the launch runs and the state names it no longer. The
// daemon keeps the wall clock here, where these tests usually hold it still, because the wake and
// its turn are the whole of what is under test; nothing else of the boot's is armed anywhere near
// as soon, the boot timeout being two minutes out and the probe interval thirty seconds.
func TestTheStateNamesTheWakeThePacingHoldsUntilItRuns(t *testing.T) {
	cfg := testConfig(t)
	// Thirty wakes a minute, one at a time: the wake behind the burst's waits one two-second
	// interval and the part of one the daemon's jitter draws (openSupervision's rand.N), which is
	// long enough to read the state while it waits and short enough that the test waits it out.
	cfg.WorkerWakesPerMinute, cfg.WorkerWakeBurst = 30, 1
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	issue := cfg.Project + "-4"
	unfinished := map[claim.Token]claim.Role{}
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer} {
		token, err := claim.NewToken(project, issue, role)
		if err != nil {
			t.Fatal(err)
		}
		putClaim(t, cfg, supervise.Claim{
			Token: token, Project: project, Tree: issue, Issue: issue, Role: role,
			Generation: 1, State: supervise.StateLaunching, PluginContract: api.DaemonAPIVersion,
		})
		writePrompt(t, cfg, token)
		unfinished[token] = role
	}
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	// The wall clock, in place of these tests' still one (daemon.go's RealClock for a nil
	// override): a wake parked on a clock whose timers never fire would never take its turn.
	o.clock = nil
	d := startDaemon(t, cfg, o)

	// The two relaunches are raised on goroutines of their own (launchUnfinished), so the second
	// has not always reserved its wake when the first has launched: the wait is for the state to
	// name it, which it does within a poll of the reservation and holds for the interval the
	// pacing gives it.
	testwait.Eventually(t, "the restart's first launch, which the burst lets out at once", func() bool {
		return len(rt.CallsOf("Spawn")) == 1
	})
	launched := rt.CallsOf("Spawn")[0].Spec.Claim
	var held api.WakeWaiting
	var startedAt time.Time
	testwait.Eventually(t, "the state to name the wake the pacing holds", func() bool {
		state := d.state()
		if len(state.WakesWaiting) != 1 {
			return false
		}
		held, startedAt = state.WakesWaiting[0], state.Daemon.StartedAt
		return true
	})
	role, mine := unfinished[held.Claim]
	if !mine || held.Claim == launched || held.Issue != issue || held.Role != role || held.Kind != "launch" {
		t.Fatalf("the wake waiting = %+v, want the launch of the restart's other claim on %s, which %s launched",
			held, issue, launched)
	}
	if due := held.DueAt; !due.After(startedAt) || due.After(startedAt.Add(time.Minute)) {
		t.Fatalf("the wake waiting is due at %s, want a moment after the daemon's start, %s, and inside the minute its pace spreads",
			due, startedAt)
	}

	testwait.Eventually(t, "the wake's turn, which runs the launch it held", func() bool {
		return len(rt.CallsOf("Spawn")) == 2
	})
	if second := rt.CallsOf("Spawn")[1].Spec.Claim; second != held.Claim {
		t.Fatalf("the launch at the wake's turn is of %s, want %s, the claim whose wake the pacing held", second, held.Claim)
	}
	if waiting := d.state().WakesWaiting; len(waiting) != 0 {
		t.Fatalf("wakesWaiting once the wake has run = %+v, want none: the wave has gone out", waiting)
	}
}

// The other lane of the same wave (api.State.WakesWaiting): a re-admission whose architect is
// already running writes it a catch-up in place of a launch, and that wake waits on its outbox row
// rather than in a machine (workflow.QueueCatchUp, record.Notice.PacedAt). The state names it like
// any other — the root architect's claim, its issue, architect, kind `catch-up` and the row's own
// moment — and names it no longer once that moment has passed, since the wake the pacing was
// holding is then out. Without this lane a state read during a re-admission wave would show an
// empty queue while every architect of it still waited.
func TestTheStateNamesThePacedCatchUpWaitingOnItsOutboxRow(t *testing.T) {
	cfg := testConfig(t)
	issue := cfg.Project + "-6"
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	records := record.NewStore()
	// Truncated to the microsecond the column keeps, so the moment the state serves is the moment
	// written rather than that moment less a few hundred nanoseconds.
	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(30 * time.Second)
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		root := runningRoot(issue, phase.Implementing)
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		return workflow.QueueCatchUp(ctx, tx, records, config.DesignGateRootIssues.Steps(), root, 1, "", now, due)
	}); err != nil {
		t.Fatalf("record the running root and the catch-up its re-admission's start wrote: %v", err)
	}
	var row int64
	if err := st.Pool().QueryRow(ctx, "select id from outbox where issue = $1 and kind = $2", issue, string(record.OutboxKindNotice)).Scan(&row); err != nil {
		t.Fatalf("read the catch-up's row: %v", err)
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	architect, err := claim.NewToken(project, issue, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}

	// This daemon runs no outbox lane, so the row waits exactly where the start wrote it.
	d := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))
	waiting := d.state().WakesWaiting
	if len(waiting) != 1 {
		t.Fatalf("wakesWaiting while the catch-up waits its turn = %+v, want the one row the pacing holds", waiting)
	}
	got := waiting[0]
	if got.Claim != architect || got.Issue != issue || got.Role != claim.RoleArchitect || got.Kind != "catch-up" || !got.DueAt.Equal(due) {
		t.Fatalf("the wake waiting = %+v, want the catch-up of %s due at %s", got, architect, due)
	}

	if _, err := st.Pool().Exec(ctx, "update outbox set next_at = $2 where id = $1", row, now.Add(-time.Second)); err != nil {
		t.Fatalf("bring the catch-up's moment forward: %v", err)
	}
	if waiting := d.state().WakesWaiting; len(waiting) != 0 {
		t.Fatalf("wakesWaiting once the catch-up's moment has passed = %+v, want none: its wake is out", waiting)
	}
}

// `legion controller start` against this daemon: the operator's bearer buys a capability, the
// controller's Oh My Pi registers with it, and the state names the registered session as the
// external controller record. The record and the registration live in the store, so a restarted
// daemon still shows the controller and still mints its grants — a restart does not cut the
// operator's controller off, as the shipped state file did not.
func TestAControllerRegistrationIsInTheStateAndSurvivesARestart(t *testing.T) {
	cfg := testConfig(t)
	d := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))
	if state := d.state(); state.ControllerLocator != nil {
		t.Fatalf("controllerLocator before any controller = %+v, want none", state.ControllerLocator)
	}

	status, body := d.request(http.MethodPost, "/legion/v1/controller/secret", api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion}, true)
	if status != http.StatusOK {
		t.Fatalf("controller secret = %d; body %s", status, body)
	}
	var capability api.ControllerSecretResponse
	if err := json.Unmarshal(body, &capability); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	status, body = d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: capability.Secret, SessionID: "ses_controller", OmpSessionFile: "/sessions/ses_controller.jsonl",
		AgentID: "ses_controller", PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register the controller = %d; body %s", status, body)
	}
	var registration api.ControllerRegisterResponse
	if err := json.Unmarshal(body, &registration); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}

	want := func(state api.State) {
		t.Helper()
		locator := state.ControllerLocator
		if locator == nil || locator.Runtime != "tmux" || !locator.External || locator.SessionID != "ses_controller" ||
			locator.RegisteredAt.IsZero() {
			t.Fatalf("controllerLocator = %+v, want the external tmux record of ses_controller", locator)
		}
	}
	want(d.state())

	d.stop()
	rebindHeldPorts(t, &cfg)
	d = startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))
	want(d.state())
	status, body = d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: "ses_controller", Secret: registration.Secret}, false)
	if status != http.StatusOK {
		t.Fatalf("the controller's grant after a restart = %d; body %s", status, body)
	}
}

// daemon is one run of the daemon in the test's process.
type daemon struct {
	t         *testing.T
	cfg       config.Config
	cancel    context.CancelFunc
	done      chan error
	transport *http.Transport
	client    *http.Client
	base      string
	// stopped is set once the test has begun stopping this run, before the run closes anything:
	// a goroutine that answers the daemon over a worker stream reads it (stepClaimant.serve) to
	// tell a write the test's own stop failed from one the daemon failed.
	stopped atomic.Bool
}

// startDaemon runs the daemon until the test stops it, and returns once it answers /healthz.
func startDaemon(t *testing.T, cfg config.Config, o overrides) *daemon {
	t.Helper()
	return startDaemonLogging(t, cfg, o, quietLogger())
}

// startDaemonLogging is startDaemon writing the daemon's log to log.
func startDaemonLogging(t *testing.T, cfg config.Config, o overrides, log *slog.Logger) *daemon {
	t.Helper()
	return startDaemonWithin(t, cfg, o, log, 10*time.Second)
}

// startDaemonWithin is startDaemonLogging failing the test unless the daemon answers /healthz
// within the given time. A test that points its workflow at no GitHub of its own gets
// repositoriesGitHub, so every launch on a repository reads its default branch from a stand-in
// rather than from github.com.
func startDaemonWithin(t *testing.T, cfg config.Config, o overrides, log *slog.Logger, within time.Duration) *daemon {
	t.Helper()
	if o.githubAPI == "" {
		o.githubAPI = repositoriesGitHub(t)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// One transport for every request, so the test can close its own connections before it asks
	// the daemon to stop: net/http gives an idle connection five seconds before a Shutdown may
	// close it, and a connection this client dialled is this client's to clean up.
	transport := &http.Transport{}
	d := &daemon{
		t: t, cfg: cfg, cancel: cancel, done: make(chan error, 1), transport: transport,
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second},
		base:   "http://127.0.0.1:" + strconv.Itoa(cfg.Port),
	}
	go func() { d.done <- run(ctx, cfg, log, o) }()
	t.Cleanup(d.stop)
	// Each poll is bounded by a second, so within bounds the wait whatever a poll meets: until the
	// daemon serves, the held listener queues the connection unanswered.
	poll := &http.Client{Transport: transport, Timeout: time.Second}
	var err error
	if !testwait.Holds(within, 20*time.Millisecond, func() bool {
		var response *http.Response
		response, err = poll.Get(d.base + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return true
			}
		}
		select {
		case exited := <-d.done:
			d.stopped.Store(true)
			t.Fatalf("the daemon exited before it answered /healthz: %v", exited)
		default:
		}
		return false
	}) {
		t.Fatalf("the daemon did not answer /healthz on %s within %s: %v", d.base, within, err)
	}
	return d
}

// repositoriesGitHub is GitHub's REST API answering a read of any repository,
// /repos/<owner>/<name>, with default branch main, and every other request with 404: a test
// daemon's launches read their repository's default branch here, and nothing a test leaves
// unanswered reaches github.com.
func repositoriesGitHub(t *testing.T) string {
	t.Helper()
	return githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
		parts := strings.Split(rest, "/")
		if r.Method != http.MethodGet || !isRepository || len(parts) != 2 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
	}})
}

// stop cancels the daemon and requires it to return cleanly. What it waits for is a run that
// never returns, not a measurement of how long the stop took: no row of its callers reads the
// elapsed time, so the wait is stopHangGuard(stopBudget) rather than the contract itself,
// stopBound(stopBudget) — the budget, then the NATS close's, the stamp's and the store close's
// bounds (daemon.go, stopBudget). Held to that contract a row reds on a machine that spends a
// few milliseconds more than the budget on a stop that waited for nothing, 3.005826798 s
// against 3 s, which is what INFRA-985 was filed for (stopHangGuard, inflight_test.go).
func (d *daemon) stop() {
	d.t.Helper()
	if d.stopped.Swap(true) {
		return
	}
	d.transport.CloseIdleConnections()
	d.cancel()
	select {
	case err := <-d.done:
		if err != nil {
			d.t.Fatalf("run: %v", err)
		}
	case <-time.After(stopHangGuard(stopBudget)):
		d.t.Fatal("run did not return after its context was cancelled")
	}
}

// request sends one request; a nil body sends none. It returns the status and the body.
func (d *daemon) request(method, path string, body any, operator bool) (int, []byte) {
	d.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			d.t.Fatalf("encode the request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, d.base+path, reader)
	if err != nil {
		d.t.Fatalf("build the request: %v", err)
	}
	if operator {
		req.Header.Set("Authorization", "Bearer "+testOperatorToken)
	}
	response, err := d.client.Do(req)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answered, err := io.ReadAll(response.Body)
	if err != nil {
		d.t.Fatalf("read the answer to %s %s: %v", method, path, err)
	}
	return response.StatusCode, answered
}

func (d *daemon) state() api.State {
	d.t.Helper()
	status, body := d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		d.t.Fatalf("GET the state = %d; body %s", status, body)
	}
	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		d.t.Fatalf("decode the state %s: %v", body, err)
	}
	return state
}

// claims is the operator's view of every claim the daemon holds, in one read: what a caller that
// asks about more than one of them takes, rather than listing them again per claim. One listing
// serves the rig's release wave (idleWakeRig.awaitReleased), where a read per pod at every poll
// would take the daemon's own Postgres away from the work they wait for.
func (d *daemon) claims() []api.OperatorClaim {
	d.t.Helper()
	status, body := d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true)
	if status != http.StatusOK {
		d.t.Fatalf("list the claims = %d; body %s", status, body)
	}
	var list api.OperatorClaims
	if err := json.Unmarshal(body, &list); err != nil {
		d.t.Fatalf("decode the claims %s: %v", body, err)
	}
	return list.Claims
}

// claim is the operator's view of one claim.
func (d *daemon) claim(token claim.Token) api.OperatorClaim {
	d.t.Helper()
	return listedClaim(d.t, d.claims(), token)
}

// listedClaim is one token's claim in a listing already read. A token the daemon does not list is
// a failure of its own, named here: answered with the zero claim instead, a caller takes its empty
// state for a state the claim is in and a wait takes it for one the claim has not reached yet, so
// the run ends at a deadline rather than at the missing token.
func listedClaim(t *testing.T, claims []api.OperatorClaim, token claim.Token) api.OperatorClaim {
	t.Helper()
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	tokens := make([]string, 0, len(claims))
	for _, c := range claims {
		tokens = append(tokens, string(c.Token))
	}
	slices.Sort(tokens)
	t.Fatalf("the daemon lists no claim %s; it lists %d: %s", token, len(claims), strings.Join(tokens, " "))
	return api.OperatorClaim{}
}

// spawn has the operator spawn a claim and returns its token.
func (d *daemon) spawn(req api.SpawnRequest) claim.Token {
	d.t.Helper()
	status, body := d.request(http.MethodPost, "/legion/v1/operator/claims", req, true)
	if status != http.StatusCreated {
		d.t.Fatalf("spawn = %d; body %s", status, body)
	}
	var spawned api.OperatorClaim
	if err := json.Unmarshal(body, &spawned); err != nil {
		d.t.Fatalf("decode the spawned claim %s: %v", body, err)
	}
	return spawned.Token
}

// lastLaunch is the spec of the claim's latest launch — the boot token its pane carries.
func lastLaunch(t *testing.T, rt *fake.Runtime, token claim.Token) runtime.SpawnSpec {
	t.Helper()
	calls := rt.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if (calls[i].Method == "Spawn" || calls[i].Method == "Resume") && calls[i].Spec.Claim == token {
			return calls[i].Spec
		}
	}
	t.Fatalf("no launch of %s was made; the runtime saw %v", token, rt.Methods())
	return runtime.SpawnSpec{}
}

// shim is the test standing in for `legion worker-shim` on the daemon's worker stream: it dials
// the socket, says hello with a pane's boot token, and speaks the frames the test tells it to.
type shim struct {
	t      *testing.T
	conn   net.Conn
	lines  *bufio.Reader
	writer *shimwire.Writer
	// stopping closes when the test ends the connection (end): a goroutine serving it that is
	// handing a frame on lets it go and returns.
	stopping chan struct{}
	// serving counts the goroutines answering the daemon on conn (serve), each of which end waits
	// for before it closes conn.
	serving sync.WaitGroup
}

// dialShim connects to the worker stream at address and says hello with bootToken, returning
// once the daemon's hello_ack arrives.
func dialShim(t *testing.T, address, bootToken string) *shim {
	t.Helper()
	conn, err := net.Dial("unix", strings.TrimPrefix(address, "unix://"))
	if err != nil {
		t.Fatalf("dial the worker stream %s: %v", address, err)
	}
	s := newTestShim(t, conn)
	s.send(shimwire.Hello2{BootToken: bootToken})
	if frame := s.next(); frame.FrameType() != shimwire.TypeHelloAck {
		t.Fatalf("the daemon answered the hello with %#v, want hello_ack", frame)
	}
	return s
}

// serve runs answer on a goroutine of its own, the pane's reader of every frame the daemon sends
// on this connection, with no read deadline, from the read the hello_ack came through on. answer
// returns once a read fails, and hands a frame on only in a select that also takes s.stopping.
func (s *shim) serve(answer func()) {
	_ = s.conn.SetReadDeadline(time.Time{})
	s.serving.Add(1)
	go func() {
		defer s.serving.Done()
		answer()
	}()
}

// end is the test's end of the connection, run as its cleanup: it stops every goroutine serving
// the connection, cutting short the read each waits in, waits until each has returned, and only
// then closes the connection. An answer a goroutine is writing as the test ends so meets the
// connection still open rather than the test's own close, and no goroutine serving it runs, or
// reports anything, once the test is over.
func (s *shim) end() {
	close(s.stopping)
	_ = s.conn.SetReadDeadline(time.Now())
	s.serving.Wait()
	_ = s.conn.Close()
}

func (s *shim) send(frame shimwire.Frame) {
	s.t.Helper()
	if err := s.writer.WriteFrame(frame); err != nil {
		s.t.Fatalf("send %s: %v", frame.FrameType(), err)
	}
}

// next is the next frame the daemon sends.
func (s *shim) next() shimwire.Frame {
	s.t.Helper()
	if err := s.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		s.t.Fatalf("set the read deadline: %v", err)
	}
	line, err := s.lines.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("read a frame from the daemon: %v", err)
	}
	frame, err := shimwire.Decode(line)
	if err != nil {
		s.t.Fatalf("decode %q: %v", line, err)
	}
	return frame
}

// closed waits for the daemon to close the connection.
func (s *shim) closed() {
	s.t.Helper()
	if err := s.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		s.t.Fatalf("set the read deadline: %v", err)
	}
	if line, err := s.lines.ReadBytes('\n'); err == nil {
		s.t.Fatalf("the daemon sent %q where the connection should have closed", line)
	}
}

// prompt answers the negotiation a connection opens with, when the daemon sends one, and returns
// the prompt frame that follows.
func (s *shim) prompt() shimwire.Prompt {
	s.t.Helper()
	frame := s.next()
	if negotiate, ok := frame.(shimwire.NegotiateProtocol); ok {
		s.send(shimwire.Response{ID: negotiate.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		frame = s.next()
	}
	prompt, ok := frame.(shimwire.Prompt)
	if !ok {
		s.t.Fatalf("the daemon sent %#v, want a prompt", frame)
	}
	return prompt
}

// repoProjectsBoard is the Dispatch of a daemon whose project names no repository: the calls every
// boot makes (servedBootCall) and an empty listing of issues, with every other call answered 404.
// A request under /api/v1/settings/repo-projects, Dispatch's repo-projects table, is answered 404
// too, and told to onRepoProjects, since the daemon reads nothing of it: that case proves the
// daemon sends the route no request (dispatch://INFRA-4401 check a-go, item 1), and
// dispatch://INFRA-3532's own change deletes it, with the rename pass.
func repoProjectsBoard(t *testing.T, project string, onRepoProjects func(*http.Request)) string {
	t.Helper()
	return dispatchBoard(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/settings/repo-projects"):
			onRepoProjects(r)
			http.NotFound(w, r)
		case servedBootCall(t, w, r, project):
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode([]any{}); err != nil {
				t.Errorf("write Dispatch issues: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	})
}

// liveTreeBoard is the Dispatch of a daemon booted over a live tree (bootOverALiveTree),
// repoProjectsBoard failing the test on a request under /api/v1/settings/repo-projects, a case
// dispatch://INFRA-3532's own change deletes with the rename pass.
func liveTreeBoard(t *testing.T, project string) string {
	t.Helper()
	return repoProjectsBoard(t, project, func(r *http.Request) {
		t.Errorf("Dispatch request = %s %s: the daemon reads no repo-projects table", r.Method, r.URL.Path)
	})
}

// bootOverALiveTree boots a daemon whose store holds one live tree of its project, its root
// running in implementing on repo (seedWorkflowTree), with tokens, GitHub's REST API at githubAPI
// and logging to log. It answers the root's key, the running daemon and the state's entry for repo
// once that entry carries what the refresher's preparation of the records' repositories kept (its
// Apps or its read error): the boot itself reads GitHub for no repository here, since the record
// carries its repository columns and no `repository_renames` pair is configured (so neither the
// backfill nor the rename pass reads one), and the preparation runs once the API serves
// (prepareAtStart, INFRA-4401 D4). The daemon's Envoy listener is a notice listener that takes
// every publish (newNoticeListener): a workflow daemon publishes its state events through it, its
// boot event first, and a publish nothing answers is an ERROR line of the outbox's own, which a
// caller reading the log for ERROR lines would read whenever the state lane ran before its read.
func bootOverALiveTree(t *testing.T, repo string, tokens appauth.Source, githubAPI string, log *slog.Logger) (string, *daemon, api.LiveRepositoryView) {
	t.Helper()
	cfg := workflowConfigOn(t, workflowNATS(t), liveTreeBoard)
	listener := httptest.NewServer(newNoticeListener())
	t.Cleanup(listener.Close)
	cfg.EnvoyURL = listener.URL
	root := seedLiveTreeOn(t, cfg, repo)
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.workflowTokens, o.githubAPI = tokens, githubAPI
	d := startDaemonWithin(t, cfg, o, log, 20*time.Second)
	var entry api.LiveRepositoryView
	settled(t, d, "name "+repo+" with what GitHub answered for it", func(state api.State) bool {
		for _, live := range state.Repositories.Live {
			if live.Repo == repo && (live.Apps != nil || live.ReadError != "") {
				entry = live
				return true
			}
		}
		return false
	})
	return root, d, entry
}

// seedLiveTreeOn records one live tree of cfg's project in the store cfg names, its root running
// in implementing on repo with its repository columns filled and no claim (seedWorkflowTree), and
// answers the root's key. The schema is brought forward before the tree is seeded, as boots does:
// the daemon's own boot migrates the store only after this, and this may be the first test of the
// run to touch it.
func seedLiveTreeOn(t *testing.T, cfg config.Config, repo string) string {
	t.Helper()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	root := cfg.Project + "-1"
	st, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	_, err = st.Migrate(context.Background())
	st.Close()
	if err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	seedWorkflowTree(t, cfg.PostgresDSN, project, record.Issue{Key: root, Repo: repo, DefaultBranch: "main", GitHubRepo: repo,
		Project: cfg.Project, Tree: root, Title: "a tree on " + repo, Phase: phase.Implementing, Generation: 1, Status: "in_progress",
		DispatchStatus: "in_progress", HandedOver: true, Rank: "A"})
	return root
}

// warnLinesNaming is each WARN line of logged, a daemon's JSON log, that holds every one of words.
func warnLinesNaming(logged string, words ...string) []string {
	var lines []string
	for line := range strings.SplitSeq(logged, "\n") {
		if !strings.Contains(line, `"level":"WARN"`) {
			continue
		}
		if !slices.ContainsFunc(words, func(word string) bool { return !strings.Contains(line, word) }) {
			lines = append(lines, line)
		}
	}
	return lines
}

// errorLines is every ERROR line of logged, a daemon's JSON log. pollClient bounds each readiness
// poll at a second, and a poll the daemon answers more slowly than that is abandoned by the test;
// the API logs the request whose context went with it at Warn, because the failure is the
// request's own ending (api's logRequestFailure), so an ERROR line here is the daemon's.
func errorLines(logged string) []string {
	var lines []string
	for line := range strings.SplitSeq(logged, "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			lines = append(lines, line)
		}
	}
	return lines
}

// A daemon over a live record on a repository a role App is not installed on boots and serves,
// and its state names the repository with each App's installation, the review App's missing, for
// an operator to install: a project works in each repository both role Apps reach, so a missing
// App refuses no boot (INFRA-4401 A2), and the record is left as it is.
func TestABootOverARecordOnARepositoryAnAppIsNotInstalledOnNamesTheApp(t *testing.T) {
	tokens := &workflowTokenRecorder{missing: map[string]bool{"review acme/widgets": true}}
	_, _, entry := bootOverALiveTree(t, "acme/widgets", tokens, repositoriesGitHub(t), quietLogger())
	if entry.Apps == nil || !entry.Apps.Implement || entry.Apps.Review || entry.ReadError != "" {
		t.Fatalf("state's entry for acme/widgets = %+v, want the implement App installed and the review App not", entry)
	}
	if got, want := entry.NotInstalled(), "the review App is not installed on acme/widgets"; got != want {
		t.Fatalf("state's entry for acme/widgets says %q, want %q", got, want)
	}
}

// A daemon over a live record whose repository's installation read fails boots all the same: its
// API answers, and its state names the repository with the App and GitHub's failure as its read
// error, for the refresher to read again (rereadBadAnswers), where a boot once exited into its
// supervisor's restart and the same failure. The daemon writes no ERROR line: the failure is one
// repository's, said once at WARN, which says a live record names it and when the refresher reads
// it again.
func TestABootOverARecordWhoseInstallationReadFailsAnswersItsState(t *testing.T) {
	failure := errors.New("read whether the implement App is installed on acme/widgets failed (403)")
	tokens := &workflowTokenRecorder{failing: map[string]error{"implement acme/widgets": failure}}
	var logged syncBuffer
	_, _, entry := bootOverALiveTree(t, "acme/widgets", tokens, repositoriesGitHub(t), slog.New(slog.NewJSONHandler(&logged, nil)))
	want := "the implement App's installation on acme/widgets could not be read: " + failure.Error()
	if entry.Apps != nil || entry.ReadError != want {
		t.Fatalf("state's entry for acme/widgets = %+v, want no Apps and the read error %q", entry, want)
	}
	if failed := errorLines(logged.String()); len(failed) != 0 {
		t.Fatalf("ERROR lines = %q, want none: one repository's failed read refuses no boot", failed)
	}
	if warned := warnLinesNaming(logged.String(), "leaves it unworkable for now", `"repo":"acme/widgets"`, failure.Error(), `"live":true`, `"retryIn":"30s"`); len(warned) != 1 {
		t.Fatalf("WARN lines naming the failed read = %q, want one", warned)
	}
}

// A daemon over a live record on a repository GitHub answers 404 for boots and serves: /readyz
// answers 200, the state's entry for the repository carries the 404 as its read error beside both
// Apps installed, one WARN line names it, and the record is unchanged. A boot read of a
// repository a record names never stops the boot (INFRA-4401 A2): a 404 is shown in legion state
// and the log and changes no record.
//
// The control is a boot preparation that returns the 404 as an error that stops the boot
// (INFRA-4401 P4; dispatch://INFRA-4401 check a-go, item 4): the daemon then exits before its
// state names the repository.
func TestABootOverARecordOnAMissingRepositoryServes(t *testing.T) {
	missing := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}})
	var logged syncBuffer
	root, d, entry := bootOverALiveTree(t, "acme/gone", &workflowTokenRecorder{}, missing, slog.New(slog.NewJSONHandler(&logged, nil)))
	if entry.Apps == nil || !entry.Apps.Implement || !entry.Apps.Review || !strings.HasPrefix(entry.ReadError, "the name and default branch of acme/gone could not be read: ") ||
		!strings.Contains(entry.ReadError, "404") {
		t.Fatalf("state's entry for acme/gone = %+v, want both Apps installed and GitHub's 404 as its read error", entry)
	}
	if status, body := d.request(http.MethodGet, "/readyz", nil, false); status != http.StatusOK {
		t.Fatalf("GET /readyz = %d %s, want 200: the daemon serves over a record on a missing repository", status, body)
	}
	if warned := warnLinesNaming(logged.String(), "leaves it unworkable for now", `"repo":"acme/gone"`, "the name and default branch of acme/gone could not be read: "); len(warned) != 1 {
		t.Fatalf("WARN lines naming acme/gone's read error = %q, want one", warned)
	}
	st, err := store.Open(context.Background(), d.cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	kept := storedIssueOf(t, context.Background(), st.Pool(), record.NewStore(), root)
	if kept.Repo != "acme/gone" || kept.GitHubRepo != "acme/gone" || kept.DefaultBranch != "main" || kept.Phase != phase.Implementing {
		t.Fatalf("%s after the boot = %+v, want it unchanged on acme/gone@main in implementing", root, kept)
	}
}

// No boot warning names commit_issue_trailer_repos or a configured rename: the daemon reads no
// table of Dispatch's about repositories, so a trailer repository or a pair's name that no such
// table names is no gap to warn of, and nothing of the boot or of the refresher's first job says
// either (dispatch://INFRA-4401 check a-go, item 9). Here the project's commit_issue_trailer_repos
// names orbit/gadgets and repository_renames pairs sjawhar/legion with cybermonklabs/legion
// (transferredLegion), and no record names any of them. The lines are matched by the phrases of
// the two warnings this change deleted, not by the word rename, since the rename pass and the
// refresher's read of the pairs log WARN lines of their own that name a rename (renameLeftLine,
// readPairNames). Control: with either warning back (the boot's warning of a trailer repository,
// or of a pair naming a repository the project does not serve), a WARN line names it.
func TestNoBootWarningNamesTheTrailerSettingOrARename(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	if cfg.Projects == nil {
		cfg.Projects = map[string]config.Project{}
	}
	project := cfg.Projects[cfg.Project]
	project.CommitIssueTrailerRepos = []ghrepo.Repository{ghrepo.MustParse("orbit/gadgets")}
	cfg.Projects[cfg.Project] = project
	cfg.RepositoryRenames = transferredLegion
	var logged syncBuffer
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(&logged, nil)))
	testwait.Eventually(t, "the refresher's first job", func() bool {
		return strings.Contains(logged.String(), "legion workflow prepared the repositories its records name")
	})
	for line := range strings.SplitSeq(logged.String(), "\n") {
		if !strings.Contains(line, `"level":"WARN"`) {
			continue
		}
		for _, deleted := range []string{
			"a configured repository rename names a repository this project does not serve",
			"commit_issue_trailer_repos names a repository",
		} {
			if strings.Contains(line, deleted) {
				t.Errorf("WARN line %s says %q, a boot warning this change deleted, want none", line, deleted)
			}
		}
	}
}

// servesNoRepositoryLine is the phrase of the boot's line for a project whose live records name no
// repository (workflowRuntime.connect).
const servesNoRepositoryLine = "the daemon serves no repository until an issue names one"

// A boot of a project with no live record logs one INFO line that says the daemon serves no
// repository until an issue names one, naming the project and no mapping, and a boot over one
// live record, a tree running on acme/widgets (bootOverALiveTree), does not log it
// (dispatch://INFRA-4401 check a-go, item 11). Controls: with the line deleted, the first case
// finds no such line; with the line logged whatever the live records name, the second finds one.
func TestABootWithNoLiveRecordSaysItServesNoRepository(t *testing.T) {
	t.Run("no live record", func(t *testing.T) {
		cfg := workflowConfig(t, workflowNATS(t))
		var logged syncBuffer
		o := fakeRuntime(fake.NewRuntime(), &built{})
		o.workflowTokens = &workflowTokenRecorder{}
		startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(&logged, nil)))
		lines := loggedLines(t, logged.String(), servesNoRepositoryLine)
		if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["project"] != cfg.Project {
			t.Fatalf("lines saying %q = %v, want one at INFO naming project %s\nlog:\n%s", servesNoRepositoryLine, lines, cfg.Project, logged.String())
		}
		if line := strings.ToLower(fmt.Sprint(lines[0])); strings.Contains(line, "mapp") {
			t.Fatalf("the line %v names a mapping, want none", lines[0])
		}
	})

	t.Run("one live record", func(t *testing.T) {
		var logged syncBuffer
		bootOverALiveTree(t, "acme/widgets", &workflowTokenRecorder{}, repositoriesGitHub(t), slog.New(slog.NewJSONHandler(&logged, nil)))
		if lines := loggedLines(t, logged.String(), servesNoRepositoryLine); len(lines) != 0 {
			t.Fatalf("lines saying %q = %v, want none: a live record names acme/widgets", servesNoRepositoryLine, lines)
		}
	})
}

// The workflow has to assemble every integration boundary before opening its API. The order in
// this log is the boot sequence operators rely on when an external dependency refuses.
func TestWorkflowBootLogsItsDependencyOrder(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	tokens := &workflowTokenRecorder{}
	var logged syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, logger, overrides{
			listen:         heldListen,
			runtime:        fakeRuntime(fake.NewRuntime(), &built{}).runtime,
			clock:          stillClock{},
			workflowTokens: tokens,
			githubAPI:      repositoriesGitHub(t),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop")
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := pollClient.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not answer /healthz: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// This boot mints no App token: the key proof under the App's JWT mints no token for an owner
	// (proveAppKeys), and the project's records name no repository for another step to read.
	if got := tokens.Roles(); len(got) != 0 {
		t.Fatalf("App token roles = %v, want none: the key proof under the App's JWT mints no token for an owner, and no record names a repository", got)
	}
	want := []string{"prompts", "store", "config", "appauth", "launcher", "intake", "admission", "outbox", "api"}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		var entry struct {
			Message string `json:"msg"`
			Stage   string `json:"stage"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode boot log line %q: %v", line, err)
		}
		if entry.Message == "legion workflow boot stage" {
			got = append(got, entry.Stage)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("workflow boot stages = %v, want %v\nlog:\n%s", got, want, logged.String())
	}
}

// An issue moved to todo while boot reads its Dispatch listing, and missing from that listing, is
// still admitted. A durable consumer created now delivers only what is published after it exists,
// so boot creates the consumers before it lists: what the listing missed, the consumer delivers.
// The admitted issue's branch is then created under the GitHub root the daemon was given.
func TestAnIssueMovedWhileBootListsIsStillAdmitted(t *testing.T) {
	natsURL := workflowNATS(t)
	js := workflowJetStream(t, natsURL)
	captured, err := os.ReadFile("../intake/testdata/dispatch/issue-updated.json")
	if err != nil {
		t.Fatalf("read the captured Dispatch issue.updated event: %v", err)
	}
	var key, moved string
	var once sync.Once
	cfg := workflowConfigOn(t, natsURL, func(t *testing.T, project string) string {
		// The event is this daemon's project's, with an id of its own: processed events are
		// deduplicated by Envoy's event id across the shared database. The captured issue carries
		// no labels; this one is handed to Legion.
		moved = strings.ReplaceAll(strings.ReplaceAll(string(captured), "CAPTURE", project), `"dispatch-15"`, `"dispatch-`+project+`"`)
		moved = strings.Replace(moved, `\"labels\":[]`, `\"labels\":[\"legion\"]`, 1)
		moved = strings.Replace(moved, `\"resolved_repo\":\"sjawhar/legion\"`, `\"resolved_repo\":\"acme/widgets\"`, 1)
		key = project + "-3"
		return dispatchBoard(t, func(w http.ResponseWriter, r *http.Request) {
			if servedBootCall(t, w, r, project) {
				return
			}
			if r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues/"+key {
				// The admission event of an issue not yet recorded is prepared as Dispatch's
				// detail shows the issue now: as the move left it.
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"key": key, "project": project, "title": "Captured workflow issue",
					"status": "todo", "rank": "UUU", "labels": []string{"legion"}, "last_seq": 2, "resolved_repo": "acme/widgets"}); err != nil {
					t.Errorf("write Dispatch issue: %v", err)
				}
				return
			}
			if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues" {
				// Once the move is admitted, the outbox reads and writes this project's issue;
				// those calls are not what this test is about. Another project's issue is never
				// this daemon's.
				if !strings.HasPrefix(r.URL.Path, "/api/v1/issues/"+project+"-") {
					t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
				}
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			// The move lands while the listing is read, which is already past it.
			once.Do(func() {
				if _, err := js.Publish(r.Context(), "notifications.dispatch.issue."+key+".issue.updated", []byte(moved)); err != nil {
					t.Errorf("publish the move: %v", err)
				}
			})
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode([]any{}); err != nil {
				t.Errorf("write Dispatch issues: %v", err)
			}
		})
	})
	github := workflowBranchGitHub(t, branchCreated)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, quietLogger(), overrides{
			listen:  heldListen,
			runtime: fakeRuntime(fake.NewRuntime(), &built{}).runtime, clock: stillClock{}, workflowTokens: &workflowTokenRecorder{},
			githubAPI: github.url,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop")
		}
	})

	awaitHealthz(t, cfg, done)
	// The move reaches admission through the consumer once supervision serves; on a loaded machine
	// that takes seconds, not milliseconds.
	var active []string
	deadline := time.Now().Add(60 * time.Second)
	for {
		var state struct {
			Admission struct {
				Active []string `json:"active"`
			} `json:"admission"`
		}
		if response, err := pollClient.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/legion/v1/state"); err == nil {
			err = json.NewDecoder(response.Body).Decode(&state)
			response.Body.Close()
			if err == nil && response.StatusCode == http.StatusOK {
				active = state.Admission.Active
				if slices.Contains(active, key) {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission is %v, want %s, moved to todo while boot listed Dispatch", active, key)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for deadline := time.Now().Add(60 * time.Second); ; {
		if creates := github.created(); len(creates) > 0 {
			if want := "refs/heads/legion/" + key; creates[0].ref != want {
				t.Fatalf("the GitHub stand-in's first create is of %s, want %s", creates[0].ref, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the GitHub stand-in saw no create of %s's branch", key)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// With no notification stream the daemon would boot with no intake, and nothing Dispatch or GitHub
// says would ever reach it. Boot refuses instead, naming the stream, before the API answers.
func TestRunRefusesToBootWithoutTheNotificationStream(t *testing.T) {
	natsURL := workflowNATS(t)
	if err := workflowJetStream(t, natsURL).DeleteStream(context.Background(), "ENVOY_NOTIFICATIONS"); err != nil {
		t.Fatalf("delete the notification stream: %v", err)
	}
	cfg := workflowConfig(t, natsURL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, quietLogger(), overrides{
			listen:  heldListen,
			runtime: fakeRuntime(fake.NewRuntime(), &built{}).runtime, clock: stillClock{}, workflowTokens: &workflowTokenRecorder{},
			githubAPI: repositoriesGitHub(t),
		})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "ENVOY_NOTIFICATIONS") {
			t.Fatalf("run = %v, want a boot refusal naming ENVOY_NOTIFICATIONS", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon booted with no notification stream")
	}
}

// Intake ending while the daemon runs (here its durable consumer is deleted) stops the daemon with
// that error, instead of the daemon serving on with nothing reading Dispatch or GitHub.
func TestRunStopsWithTheErrorWhenItsIntakeEnds(t *testing.T) {
	natsURL := workflowNATS(t)
	cfg := workflowConfig(t, natsURL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, quietLogger(), overrides{
			listen:  heldListen,
			runtime: fakeRuntime(fake.NewRuntime(), &built{}).runtime, clock: stillClock{}, workflowTokens: &workflowTokenRecorder{},
			githubAPI: repositoriesGitHub(t),
		})
	}()
	stream, err := workflowJetStream(t, natsURL).Stream(context.Background(), "ENVOY_NOTIFICATIONS")
	if err != nil {
		t.Fatalf("open the notification stream: %v", err)
	}
	consumer := "legion-go-" + cfg.Project + "-dispatch"
	testwait.Eventually(t, "intake pulling from the durable Dispatch consumer", func() bool {
		durable, err := stream.Consumer(context.Background(), consumer)
		if err != nil {
			return false
		}
		info, err := durable.Info(context.Background())
		return err == nil && info.NumWaiting > 0
	})
	if err := stream.DeleteConsumer(context.Background(), consumer); err != nil {
		t.Fatalf("delete %s: %v", consumer, err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), consumer) {
			t.Fatalf("run = %v, want the daemon stopped by its intake naming %s", err, consumer)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon kept running after its intake ended")
	}
}

// readinessAttempt and bootprobe.Run are only useful if run() actually wires them together. Here
// a real daemon.Run() boots against a Dispatch stand-in that answers 503 twice before listing no
// issues, under a fast readinessRetry, and reaches /healthz — proving the wiring itself rides out
// the outage, not just the adapter TestReadinessAttempt (readiness_test.go) already covers in
// isolation.
func TestRunWaitsThroughADispatch503BeforeServing(t *testing.T) {
	fastReadiness(t, bootprobe.Retry{Initial: time.Millisecond, Max: 4 * time.Millisecond})
	natsURL := workflowNATS(t)
	var requests atomic.Int64
	cfg := workflowConfigOn(t, natsURL, func(t *testing.T, project string) string {
		return dispatchBoard(t, func(w http.ResponseWriter, r *http.Request) {
			if servedBootCall(t, w, r, project) {
				return
			}
			if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues" {
				t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
				return
			}
			if requests.Add(1) <= 2 {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode([]any{}); err != nil {
				t.Errorf("write Dispatch issues: %v", err)
			}
		})
	})
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	startDaemon(t, cfg, o)

	if got := requests.Load(); got < 3 {
		t.Fatalf("Dispatch saw %d requests, want at least 3 (two 503s then the 200 the daemon booted on)", got)
	}
}

// workflowDispatchToken is the bearer the daemon of workflowConfig presents to Dispatch.
const workflowDispatchToken = "dispatch-test-token"

// workflowConfig is a Stage 3 configuration over natsURL and a Dispatch that lists no issues
// (workflowDispatch).
func workflowConfig(t *testing.T, natsURL string) config.Config {
	t.Helper()
	return workflowConfigOn(t, natsURL, workflowDispatch)
}

// workflowConfigOn is workflowConfig over the Dispatch board that board starts for the
// configuration's project, in place of workflowConfig's own: a test that passes its own board here
// starts no second one.
func workflowConfigOn(t *testing.T, natsURL string, board func(t *testing.T, project string) string) config.Config {
	t.Helper()
	cfg := testConfig(t)
	// Boot reads this process's environment, so the real gh is named here: the first gh on a
	// machine's PATH may be a script, which boot refuses (RefuseScriptGH).
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_GH_PATH", executable)
	cfg.DispatchURL = board(t, cfg.Project)
	cfg.DispatchTokenFile = filepath.Join(t.TempDir(), "dispatch-token")
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte(workflowDispatchToken+"\n"), 0o600); err != nil {
		t.Fatalf("write Dispatch token: %v", err)
	}
	cfg.Projects = map[string]config.Project{cfg.Project: {}}
	cfg.NatsURLs = []string{natsURL}
	return cfg
}

// workflowDispatch is workflowConfig's board (dispatchBoard): the calls every boot makes
// (servedBootCall) and an empty listing of issues, and any other call of the daemon's fails the
// test.
func workflowDispatch(t *testing.T, project string) string {
	t.Helper()
	return dispatchBoard(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case servedBootCall(t, w, r, project):
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode([]any{}); err != nil {
				t.Errorf("write Dispatch issues: %v", err)
			}
		default:
			t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
		}
	})
}

func workflowJetStream(t *testing.T, natsURL string) jetstream.JetStream {
	t.Helper()
	conn := testnats.Connect(t, natsURL)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// workflowAppToken is the GitHub App token of each lease a workflowTokenRecorder's Token mints
// (ReadToken's lease carries it with "-read" after it).
const workflowAppToken = "workflow-test-token"

type workflowTokenRecorder struct {
	mu    sync.Mutex
	roles []appauth.AppRole
	// missing names the installations not made, "<role> <owner/name>"; every other is. failing
	// names the installation reads that fail, with their error.
	missing map[string]bool
	failing map[string]error
}

func (r *workflowTokenRecorder) Token(_ context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roles = append(r.roles, role)
	// Each App has its own bot login, as a real token manager's leases do.
	return appauth.Lease{Token: workflowAppToken, ExpiresAt: time.Now().Add(time.Hour), Identity: appauth.GitIdentity{Name: "legion-" + string(role) + "[bot]"}}, nil
}

// AppLogin answers each App's own bot login, as its lease names it.
func (r *workflowTokenRecorder) AppLogin(_ context.Context, role appauth.AppRole) (string, error) {
	return "legion-" + string(role) + "[bot]", nil
}

// Forget drops nothing: every lease is minted afresh.
func (r *workflowTokenRecorder) Forget(appauth.AppRole, string) {}

// InstalledOn answers each App installed on every repository but those missing names, and fails
// the reads failing names.
func (r *workflowTokenRecorder) InstalledOn(_ context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	key := string(role) + " " + repo.String()
	if err := r.failing[key]; err != nil {
		return false, err
	}
	return !r.missing[key], nil
}

// InstalledUnder answers as InstalledOn does: the recorder answers each name for itself, as GitHub
// answers a name it redirects nowhere.
func (r *workflowTokenRecorder) InstalledUnder(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	return r.InstalledOn(ctx, role, repo)
}

// InstalledOwners answers each App installed on acme, the account of acme/widgets, which every
// rig over the recorder works in: the boot's proof of each App's key reads it (proveAppKeys) and
// records it as that App's installations, so a launch's addressing names acme from the first
// launch. No rig builds a gh bundle over the recorder, since each replaces the runtime that would.
func (r *workflowTokenRecorder) InstalledOwners(context.Context, appauth.AppRole) ([]string, error) {
	return []string{"acme"}, nil
}

// servedBootCall answers the calls every boot makes of Dispatch, whatever the test is about:
// the webhook App check, Dispatch's GitHub App installed on every repository; the publish of the
// project's gated design steps,
// which the boot queues for its own project (daemon.queueGatedStepsPublish) and the outbox's
// first sweep sends; the publish of the documents a gated issue's own gates record, which the
// boot queues for every recorded issue Dispatch has not closed (daemon.queueRecordedGatesPublish)
// and that same sweep sends; the publish of an issue's role sessions, which the boot queues for
// every issue with a recent claim (daemon.queueRoleHoldersPublish) and each boot ready and each
// change of a claim's role holder queues again (roleHoldersChanged, the store's putClaim); and the
// read of a gate's own document, which the boot's catch-up makes for every root it reads
// (bindGateRequest through dispatch.Client.Approval). It reports whether it answered r. A board
// that leaves one of these to its own default arm fails the test with an unexpected request,
// which is what this helper exists to stop: none of them belongs to a test here.
// role_holder_publish_test.go and TestRoleHoldersLandInScratchDispatch are the tests of the
// role-holder publication.
func servedBootCall(t *testing.T, w http.ResponseWriter, r *http.Request, project string) bool {
	t.Helper()
	if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/projects/") &&
		strings.HasSuffix(r.URL.Path, "/gated-steps") {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/issues/") &&
		strings.HasSuffix(r.URL.Path, "/design-gates") {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/issues/") &&
		strings.HasSuffix(r.URL.Path, "/role-holders") {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if r.Method != http.MethodGet {
		return false
	}
	var body any
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/github/app-installation/"):
		body = map[string]bool{"installed": true}
	case strings.HasPrefix(r.URL.Path, "/api/v1/artifacts/") &&
		!strings.Contains(strings.TrimPrefix(r.URL.Path, "/api/v1/artifacts/"), "/"):
		// The document itself, one segment: a path under it is a route no board here models and
		// belongs to the caller's own default arm. A board seeds a gate's row and holds no review
		// of its document, so the document reads as one nobody has asked to approve, and the
		// catch-up applies nothing (bindGateRequest returns at an empty AskID). Only the approval
		// is answered, because only the approval is read here: a reader that needs the issue, the
		// slug or the role — register_gate's document check — meets the zero value and fails,
		// which is the board telling it to model the document rather than guess at one.
		body = map[string]any{"approval": map[string]any{"state": "draft", "latest_version": 1}}
	default:
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("write %s: %v", r.URL.Path, err)
	}
	return true
}

func (r *workflowTokenRecorder) ReadToken(ctx context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	lease, err := r.Token(ctx, role, "")
	lease.Token += "-read"
	return lease, err
}

func (r *workflowTokenRecorder) Roles() []appauth.AppRole {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]appauth.AppRole(nil), r.roles...)
}

func workflowNATS(t *testing.T) string {
	t.Helper()
	url := testnats.URL(t)
	conn := testnats.Connect(t, url)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	testnats.CreateStream(t, js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	return url
}

// The `runtime.tmux` block's take_over reaches the tmux runtime as the value the runtime knows,
// and a block without it, or no block, takes nothing over.
func TestTheTmuxOptionsCarryTheTakeOver(t *testing.T) {
	cfg := config.Config{Runtime: config.Runtime{Name: "tmux", Tmux: &config.Tmux{TakeOver: config.TakeOverKubernetes}}}
	if got := tmuxOptions(cfg, "test", nil, "", "", quietLogger()).TakeOver; got != tmux.TakeOverKubernetes {
		t.Errorf("TakeOver = %q, want %q", got, tmux.TakeOverKubernetes)
	}
	for _, block := range []*config.Tmux{nil, {}} {
		cfg.Runtime.Tmux = block
		if got := tmuxOptions(cfg, "test", nil, "", "", quietLogger()).TakeOver; got != "" {
			t.Errorf("TakeOver with block %+v = %q, want none", block, got)
		}
	}
}

// Every launch secret's pointer is a pane variable the operator may not set
// (runtime.PaneVariableRefusal), refused at load and at boot rather than at every launch.
func TestEveryLaunchSecretsPointerIsRefusedAsAPaneVariable(t *testing.T) {
	cfg := config.Config{EnvoyTokenFile: "/var/run/legion/ENVOY_TOKEN", NatsNkeySeedFile: "/var/run/legion/NATS_NKEY_SEED"}
	names := launchSecretNames(cfg, lookup(nil))
	if len(names) != 2 {
		t.Fatalf("launchSecretNames = %v, want both launch secrets of a daemon configured with each", names)
	}
	for _, name := range names {
		if runtime.PaneVariableRefusal(name+"_FILE", "/etc/acme/secret") == "" {
			t.Errorf("runtime.tmux.pane_env may set %s_FILE, the pointer every launch sets to the launch secret %s", name, name)
		}
	}
}
