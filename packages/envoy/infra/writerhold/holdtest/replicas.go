package holdtest

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/metrics"
)

// HoldRead is a reading of the hold row, natscallout.Deps.HoldRead's type.
type HoldRead = func(ctx context.Context) (natshold.Reading, bool, error)

// ReplicaConfig is how a replica StartReplica starts differs from the stack's own (Stack.Auth):
// each field's zero value is the stack's.
type ReplicaConfig struct {
	// NoHoldRead starts the replica with no HoldRead, a broker whose BROKER_NATS_HOLD_READ is
	// false: it reads no row and answers every decision as if the row held released.
	NoHoldRead bool
	// Wrap, when set, is given the replica's own read of the row (natshold.Read on the replica's
	// pool) and answers the read the replica decides on: a test's model of an older view.
	Wrap func(read HoldRead) HoldRead
	// HoldKeys are the replica's BROKER_NATS_HOLD_SIGNING_KEYS; nil is the broker's one key.
	HoldKeys []ed25519.PublicKey
	// Now is the replica's clock (natscallout.Deps.Now); nil is time.Now.
	Now func() time.Time
}

// Replica is one more replica of the stack's auth service, in the replicas' queue group beside
// any other that runs: the same hub, keys, services and platform tokens as Stack.Auth, and its own
// metrics and its own Postgres pool, which reaches the stack's Postgres through Postgres, a path
// a test can stall or cut as a network partition or a stopped Postgres looks to this replica
// alone. Its every reading of the row, as the replica decided on it, is kept (Readings).
type Replica struct {
	Service *natscallout.Service
	// Registry holds the replica's broker_nats_callout_decisions_total and
	// envoy_broker_hold_unknown (natscallout.NewMetrics).
	Registry *metrics.Registry
	// Postgres is the path from the replica's pool to the stack's Postgres; nil with NoHoldRead.
	Postgres *PostgresPath

	mu       sync.Mutex
	readings []Reading
}

// Reading is one reading of the hold row a replica decided on: the phase the read answered, or
// that it found no row, or its error.
type Reading struct {
	Phase string
	Found bool
	Err   error
}

// StartReplica starts a replica of the auth service on the stack's hub, closed when t ends, and
// returns it once its subscription reaches the hub.
func (s *Stack) StartReplica(t testing.TB, cfg ReplicaConfig) *Replica {
	t.Helper()
	r := &Replica{Registry: metrics.New()}
	deps := natscallout.Deps{ServiceToken: s.tokens.verify, Metrics: natscallout.NewMetrics(r.Registry), Now: cfg.Now}
	if !cfg.NoHoldRead {
		schema := schemaOf(t, s.Broker.Pool)
		upstream := s.Broker.Pool.Config()
		r.Postgres = startPostgresPath(t, net.JoinHostPort(upstream.ConnConfig.Host, strconv.Itoa(int(upstream.ConnConfig.Port))))
		poolConfig := s.Broker.Pool.Config()
		poolConfig.ConnConfig.Host, poolConfig.ConnConfig.Port = "127.0.0.1", uint16(r.Postgres.port)
		poolConfig.ConnConfig.Fallbacks = nil
		pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
		if err != nil {
			t.Fatalf("holdtest: open the replica's pool: %v", err)
		}
		t.Cleanup(pool.Close)
		read := HoldRead(func(ctx context.Context) (natshold.Reading, bool, error) {
			return natshold.Read(ctx, pool, schema)
		})
		if cfg.Wrap != nil {
			read = cfg.Wrap(read)
		}
		deps.HoldRead = func(ctx context.Context) (natshold.Reading, bool, error) {
			reading, found, err := read(ctx)
			r.mu.Lock()
			r.readings = append(r.readings, Reading{Phase: reading.Phase, Found: found, Err: err})
			r.mu.Unlock()
			return reading, found, err
		}
		deps.HoldKeys = cfg.HoldKeys
		if deps.HoldKeys == nil {
			deps.HoldKeys = []ed25519.PublicKey{s.Broker.VerifyKey}
		}
	}
	dir := t.TempDir()
	write := func(name, seed string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(seed+"\n"), 0o600); err != nil {
			t.Fatalf("holdtest: write %s: %v", name, err)
		}
		return path
	}
	service, err := natscallout.Start(context.Background(), natscallout.Settings{
		URLs:           s.Cluster.URLs(),
		SeedFile:       write("service.seed", s.Keys.ServiceSeed),
		IssuerSeedFile: write("issuer.seed", s.Keys.IssuerSeed),
		XKeySeedFile:   write("xkey.seed", s.Keys.XKeySeed),
		Services:       stackServices(t),
	}, deps)
	if err != nil {
		t.Fatalf("holdtest: start a replica of the auth service: %v", err)
	}
	t.Cleanup(service.Close)
	ready, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := service.Ready(ready); err != nil {
		t.Fatalf("holdtest: the replica's subscription never reached the hub: %v", err)
	}
	r.Service = service
	return r
}

// stackServices are the services the stack's auth service lists (Start): the operator, the probe
// and a listener entry of each of ListenerMachines.
func stackServices(t testing.TB) []config.NATSService {
	t.Helper()
	entries := []string{"operator/-/*=" + OperatorAccount, "probe/-/*=" + ProbeAccount}
	for _, machine := range ListenerMachines {
		entries = append(entries, "listener/"+machine+"/*="+listenerAccountPrefix+machine)
	}
	services, err := config.ParseNATSServices(strings.Join(entries, " "))
	if err != nil {
		t.Fatalf("holdtest: the stack's services: %v", err)
	}
	return services
}

// schemaOf is the schema pool's connections resolve the hold's tables in.
func schemaOf(t testing.TB, pool *pgxpool.Pool) string {
	t.Helper()
	var schema string
	if err := pool.QueryRow(context.Background(), `select current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("holdtest: read the test schema's name: %v", err)
	}
	return schema
}

// Close closes the replica: it leaves the queue group, and the hub sends its requests to the
// replicas that remain.
func (r *Replica) Close() { r.Service.Close() }

// Readings are the replica's readings of the row so far, oldest first.
func (r *Replica) Readings() []Reading {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Reading(nil), r.readings...)
}

// Decisions is how many decisions the replica counted of credential kind (session, service or
// none) with outcome (admitted or refused) and reason (the refusal's label, "" for an admission),
// read from its broker_nats_callout_decisions_total.
func (r *Replica) Decisions(kind, outcome, reason string) int64 {
	want := []string{`kind="` + kind + `"`, `outcome="` + outcome + `"`, `reason="` + reason + `"`}
	var total int64
	for _, line := range r.scrape() {
		series, value, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(series, "broker_nats_callout_decisions_total{") {
			continue
		}
		matches := true
		for _, label := range want {
			matches = matches && strings.Contains(series, label)
		}
		if n, err := strconv.ParseInt(value, 10, 64); matches && err == nil {
			total += n
		}
	}
	return total
}

// HoldUnknown is the replica's envoy_broker_hold_unknown as its metrics port serves it, "" when
// the gauge is not served.
func (r *Replica) HoldUnknown() string {
	for _, line := range r.scrape() {
		if value, ok := strings.CutPrefix(line, "envoy_broker_hold_unknown "); ok {
			return value
		}
	}
	return ""
}

// scrape is the replica's metrics as the broker's metrics port serves them, line by line.
func (r *Replica) scrape() []string {
	recorder := httptest.NewRecorder()
	r.Registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	var lines []string
	scanner := bufio.NewScanner(recorder.Body)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

// PostgresPath is a TCP path from one replica's pool to the stack's Postgres, which a test stalls
// (every byte held in both directions, so a read waits past its deadline, as a network partition
// makes it) or cuts (every connection closed and every new one refused, as a stopped Postgres
// looks to the replica), and opens again.
type PostgresPath struct {
	listener net.Listener
	port     int
	upstream string

	mu    sync.Mutex
	cond  *sync.Cond
	state pathState
	conns map[net.Conn]struct{}
}

type pathState int

const (
	pathOpen pathState = iota
	pathStalled
	pathCut
	pathClosed
)

// startPostgresPath listens on a port of its own on 127.0.0.1 and forwards each connection to
// upstream, until t ends.
func startPostgresPath(t testing.TB, upstream string) *PostgresPath {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("holdtest: listen for the replica's Postgres path: %v", err)
	}
	p := &PostgresPath{listener: listener, port: listener.Addr().(*net.TCPAddr).Port, upstream: upstream, conns: map[net.Conn]struct{}{}}
	p.cond = sync.NewCond(&p.mu)
	go p.accept()
	t.Cleanup(func() {
		p.set(pathClosed)
		_ = listener.Close()
	})
	return p
}

// Stall holds every byte on the path, in both directions, until Open.
func (p *PostgresPath) Stall() { p.set(pathStalled) }

// Cut closes every connection on the path and refuses every new one until Open.
func (p *PostgresPath) Cut() { p.set(pathCut) }

// Open lets the path carry its bytes again.
func (p *PostgresPath) Open() { p.set(pathOpen) }

func (p *PostgresPath) set(state pathState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = state
	if state == pathCut || state == pathClosed {
		for conn := range p.conns {
			_ = conn.Close()
		}
		clear(p.conns)
	}
	p.cond.Broadcast()
}

func (p *PostgresPath) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refused := p.state == pathCut || p.state == pathClosed
		p.mu.Unlock()
		if refused {
			_ = client.Close()
			continue
		}
		server, err := net.DialTimeout("tcp", p.upstream, 10*time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		if !p.track(client, server) {
			continue
		}
		go p.pipe(client, server)
		go p.pipe(server, client)
	}
}

// track records both halves of one forwarded connection, or closes them when the path was cut
// meanwhile.
func (p *PostgresPath) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == pathCut || p.state == pathClosed {
		for _, conn := range conns {
			_ = conn.Close()
		}
		return false
	}
	for _, conn := range conns {
		p.conns[conn] = struct{}{}
	}
	return true
}

// pipe copies from to to, waiting before each write while the path is stalled, and closes both
// when either ends or the path is cut.
func (p *PostgresPath) pipe(from, to net.Conn) {
	defer func() {
		_ = from.Close()
		_ = to.Close()
	}()
	buffer := make([]byte, 32<<10)
	for {
		n, err := from.Read(buffer)
		if n > 0 {
			if !p.flowing() {
				return
			}
			if _, werr := to.Write(buffer[:n]); werr != nil {
				return
			}
		}
		// A read ends with the peer's close, the path's cut or the test's end; each ends the pipe.
		if err != nil {
			return
		}
	}
}

// flowing waits while the path is stalled, and reports whether it is open.
func (p *PostgresPath) flowing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.state == pathStalled {
		p.cond.Wait()
	}
	return p.state == pathOpen
}
