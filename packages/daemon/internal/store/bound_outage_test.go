package store

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A bounded transaction (BoundAfterAcquire) holds its caller for about its write bound when the
// database stops answering, not for its wait in the pool's queue: the queue is the only thing the
// acquire bound covers. The three ways a database can stop answering between the queue and the
// first statement each run here on the daemon's own pool configuration (PoolConfig), through a
// listener or a proxy that answers nothing, with an acquire bound far longer than the write's:
//
//   - a listener that accepts and never answers: the pool's dial of a new connection ends at
//     PoolConfig's connect_timeout, a claim write's bound (supervise.DefaultClaimWrite);
//   - a server that goes silent before a transaction's BEGIN: the BEGIN runs under the write bound;
//   - a server that goes silent while a connection sits idle past the pool's ping threshold (1 s):
//     a bounded transaction is handed it unpinged, and its BEGIN is the check, under the write
//     bound.
//
// Before, pgxpool.BeginTx ran the dial's wait, the health ping and the BEGIN on the one context
// TransactWith gave the acquire bound, and the DSN set no connect_timeout, so each case held the
// caller for the whole acquire bound (thirty seconds for a claim write, against the five the
// claim's machine is promised).
func TestABoundedTransactionOnADatabaseThatStopsAnsweringEndsAtItsWriteBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("PoolConfig bounds the dial and the ping at a claim write's bound", func(t *testing.T) {
		t.Parallel()
		config, err := PoolConfig(testDSN(t))
		if err != nil {
			t.Fatal(err)
		}
		if config.ConnConfig.ConnectTimeout != supervise.DefaultClaimWrite || config.PingTimeout != supervise.DefaultClaimWrite {
			t.Fatalf("PoolConfig's connect_timeout %s and ping timeout %s, want both %s", config.ConnConfig.ConnectTimeout, config.PingTimeout, supervise.DefaultClaimWrite)
		}
		shorter, err := url.Parse(testDSN(t))
		if err != nil {
			t.Fatal(err)
		}
		q := shorter.Query()
		q.Set("connect_timeout", "2")
		shorter.RawQuery = q.Encode()
		if config, err = PoolConfig(shorter.String()); err != nil || config.ConnConfig.ConnectTimeout != 2*time.Second {
			t.Fatalf("PoolConfig of a DSN naming connect_timeout=2 keeps %v (%v), want the DSN's 2s", config.ConnConfig.ConnectTimeout, err)
		}
	})

	t.Run("a listener that never answers", func(t *testing.T) {
		t.Parallel()
		listener := mute(t)
		pool := poolOn(t, proxiedDSN(t, migratedStore(t), listener))
		const acquire = 20 * time.Second
		took, err := boundedSelect(ctx, pool, acquire, 500*time.Millisecond)
		if err == nil {
			t.Fatal("a transaction on a listener that never answers answered nil")
		}
		if took > supervise.DefaultClaimWrite+2*time.Second {
			t.Fatalf("a transaction on a listener that never answers took %s (%v), want about the %s connect_timeout, far short of its %s acquire bound", took, err, supervise.DefaultClaimWrite, acquire)
		}
	})

	t.Run("a server silent on BEGIN", func(t *testing.T) {
		t.Parallel()
		proxy := silentProxyTo(t)
		pool := poolOn(t, proxiedDSN(t, migratedStore(t), proxy.address()))
		if _, err := boundedSelect(ctx, pool, 3*time.Second, 500*time.Millisecond); err != nil {
			t.Fatalf("a transaction before the server went silent: %v", err)
		}
		proxy.silent.Store(true)
		took, err := boundedSelect(ctx, pool, 3*time.Second, 500*time.Millisecond)
		if err == nil || errors.Is(err, errAcquireBound) {
			t.Fatalf("a transaction whose BEGIN the server never answers = %v, want the write bound's failure, not the acquire bound's", err)
		}
		if took > 1500*time.Millisecond {
			t.Fatalf("a transaction whose BEGIN the server never answers took %s, want about its 500ms write bound, short of its 3s acquire bound", took)
		}
	})

	t.Run("a server silent once a connection sits idle past the ping threshold", func(t *testing.T) {
		t.Parallel()
		proxy := silentProxyTo(t)
		pool := poolOn(t, proxiedDSN(t, migratedStore(t), proxy.address()))
		if _, err := boundedSelect(ctx, pool, 3*time.Second, 500*time.Millisecond); err != nil {
			t.Fatalf("a transaction before the server went silent: %v", err)
		}
		// pgxpool pings a connection idle more than a second before it hands it out
		// (pgxpool.Config.ShouldPing's default).
		time.Sleep(1200 * time.Millisecond)
		proxy.silent.Store(true)
		took, err := boundedSelect(ctx, pool, 3*time.Second, 500*time.Millisecond)
		if err == nil || errors.Is(err, errAcquireBound) {
			t.Fatalf("a transaction on an idle connection of a silent server = %v, want the write bound's failure, not the acquire bound's", err)
		}
		if took > 1500*time.Millisecond {
			t.Fatalf("a transaction on an idle connection of a silent server took %s, want about its 500ms write bound, short of its 3s acquire bound", took)
		}
	})
}

// boundedSelect runs one transaction on pool under BoundAfterAcquire(acquire, write) and says how
// long it took.
func boundedSelect(ctx context.Context, pool *Pool, acquire, write time.Duration) (time.Duration, error) {
	bounded, cancel := BoundAfterAcquire(ForConcurrentWorker(ctx), acquire, write)
	defer cancel()
	started := time.Now()
	err := Transact(bounded, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "select 1")
		return err
	})
	return time.Since(started), err
}

// poolOn is a pool on dsn as the daemon builds its own (PoolConfig), closed at the test's end.
func poolOn(t *testing.T, dsn string) *Pool {
	t.Helper()
	config, err := PoolConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	pool := NewPool(raw)
	t.Cleanup(pool.Close)
	return pool
}

// proxiedDSN is the test's DSN for store's database at address, with TLS off so pgx makes one
// connection attempt, not two.
func proxiedDSN(t *testing.T, store *Store, address string) string {
	t.Helper()
	var name string
	if err := store.Pool().QueryRow(context.Background(), `select current_database()`).Scan(&name); err != nil {
		t.Fatalf("read the test database's name: %v", err)
	}
	u, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path, u.Host = "/"+name, address
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}

// mute is the address of a listener that accepts every connection and never writes a byte,
// closed with every connection it took at the test's end.
func mute(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var taken []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			taken = append(taken, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range taken {
			_ = conn.Close()
		}
	})
	return listener.Addr().String()
}

// silentProxy passes the test's Postgres both ways until silent is set; from then it drops every
// byte the server sends, so the database answers nothing to a client that sends something.
type silentProxy struct {
	listener net.Listener
	silent   atomic.Bool
	mu       sync.Mutex
	conns    []net.Conn
	wg       sync.WaitGroup
}

func silentProxyTo(t *testing.T) *silentProxy {
	t.Helper()
	u, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	upstream := u.Host
	if u.Port() == "" {
		upstream = net.JoinHostPort(u.Hostname(), "5432")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &silentProxy{listener: listener}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = client.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, client, server)
			p.mu.Unlock()
			p.wg.Add(2)
			go func() { defer p.wg.Done(); _, _ = io.Copy(server, client) }()
			go func() { defer p.wg.Done(); p.answer(client, server) }()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		p.mu.Lock()
		for _, conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return p
}

func (p *silentProxy) address() string { return p.listener.Addr().String() }

// answer copies the server's bytes to the client, and drops them while the proxy is silent.
func (p *silentProxy) answer(client, server net.Conn) {
	buffer := make([]byte, 32<<10)
	for {
		n, err := server.Read(buffer)
		if n > 0 && !p.silent.Load() {
			if _, err := client.Write(buffer[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
