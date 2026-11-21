package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// registrationGate is the advisory lock the capability write of a registration waits on in
// TestARegistrationTheClientGaveUpOnIsAppliedBeforeItsRepeat, which the test holds while it
// wants that write held.
const registrationGate = 3353

// The plugin gives up on a claims/register the daemon took and did not answer within its
// attempt's five-minute limit: it ends that request, closing its connection, and sends the next
// with the same boot token (pi-shared's retryWhileDaemonRestarts, R1). The daemon runs a
// registration's decision to its end whatever the client does (server.decision), so the
// registration given up on is still applied, and issues a secret nobody reads. R1 requires that
// this leave the worker with no secret the daemon no longer accepts.
//
// This drives that case through the real server over a real connection. The given-up decision is
// held inside its store write of the capability hash while the client hangs up and sends its
// repeat; the repeat reaches the server while the held decision still holds the claim's machine,
// and waits behind it; then the write is let go. Both registrations are applied, in the order
// they were sent, so the secret the worker holds is the one the daemon applied last, and
// claims/ready accepts it.
func TestARegistrationTheClientGaveUpOnIsAppliedBeforeItsRepeat(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleImplementer)
	// Each registration that reaches the server is told here as it arrives, before the route
	// reads it.
	arrived := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/legion/v1/claims/register" {
			arrived <- struct{}{}
		}
		h.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	// The store write of a registration's capability hash waits on registrationGate, and records
	// the hash it writes in the order the writes pass it. The gate is held by a transaction of
	// the test's own, and let go when that transaction ends.
	gate, err := h.store.Pool().Begin(h.ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the gate: %v", err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	if _, err := gate.Exec(h.ctx, `select pg_advisory_xact_lock($1)`, registrationGate); err != nil {
		t.Fatalf("hold the gate: %v", err)
	}
	if err := h.store.Tx(h.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(h.ctx, fmt.Sprintf(`create table capability_writes (seq bigserial primary key, hash bytea not null);
			create function hold_capability() returns trigger language plpgsql as
			$$ begin
				perform pg_advisory_lock(%[1]d);
				perform pg_advisory_unlock(%[1]d);
				insert into capability_writes (hash) values (new.capability_hash);
				return new;
			end $$;
			create trigger hold_capability before update on claims for each row
			when (new.capability_hash is not null and new.capability_hash is distinct from old.capability_hash)
			execute function hold_capability();`, registrationGate))
		return err
	}); err != nil {
		t.Fatalf("install the gate on the capability write: %v", err)
	}

	client := server.Client()
	register := func(ctx context.Context) (*http.Response, error) {
		body, err := json.Marshal(claim.RegisterRequest{
			BootToken: boot, SessionID: "ses_implementer", OmpSessionFile: "/sessions/ses_implementer.jsonl",
			AgentID: "agent-ses_implementer", PluginContract: DaemonAPIVersion,
		})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/legion/v1/claims/register", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return client.Do(req)
	}

	// The first registration: the daemon admits it, and its decision waits in the capability write.
	gaveUp, giveUp := context.WithCancel(h.ctx)
	defer giveUp()
	first := make(chan error, 1)
	go func() {
		resp, err := register(gaveUp)
		if err == nil {
			resp.Body.Close()
			err = fmt.Errorf("the registration given up on was answered %s", resp.Status)
		}
		first <- err
	}()
	<-arrived
	waitUntil(t, "the first registration waits in its capability write", func() bool {
		return gateWaiters(t, h) == 1
	})
	if !h.decisions.Holds(token) {
		t.Fatal("the held registration is not a decision the server is running")
	}

	// The client's limit runs out: it ends the request, closing its connection, before it sends
	// the next one, as the plugin does.
	giveUp()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("the client's given-up registration ended with %v, want its own cancellation", err)
	}

	// The repeat, with the same boot token, reaches the server while the given-up registration's
	// decision still holds the claim's machine, and is not answered while it does.
	type answer struct {
		status int
		body   []byte
		err    error
	}
	second := make(chan answer, 1)
	go func() {
		resp, err := register(h.ctx)
		if err != nil {
			second <- answer{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		second <- answer{status: resp.StatusCode, body: body, err: err}
	}()
	<-arrived
	select {
	case got := <-second:
		t.Fatalf("the repeat was answered %d (%v) while the given-up registration was held; body %s", got.status, got.err, got.body)
	default:
	}
	if !h.decisions.Holds(token) {
		t.Fatal("the given-up registration's decision ended before the repeat arrived")
	}

	// The daemon serves again: the held write goes through, then the repeat's.
	if err := gate.Rollback(h.ctx); err != nil {
		t.Fatalf("let the gate go: %v", err)
	}
	got := <-second
	if got.err != nil || got.status != http.StatusOK {
		t.Fatalf("the repeat answered %d, %v; body %s", got.status, got.err, got.body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(got.body, &registered); err != nil {
		t.Fatalf("decode the repeat's registration: %v", err)
	}

	// The worker's secret is the one the daemon accepts.
	ready, err := json.Marshal(claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_implementer", Secret: registered.Secret, Generation: registered.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post(server.URL+"/legion/v1/claims/ready", "application/json", bytes.NewReader(ready))
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("ready with the secret the worker holds = %d, want 204; body %s", resp.StatusCode, body)
	}

	// The registration given up on was applied after the client gave it up, and before the
	// repeat: two capability writes, the repeat's last.
	writes := capabilityWrites(t, h)
	if len(writes) != 2 {
		t.Fatalf("the store took %d capability writes, want the given-up registration's and the repeat's", len(writes))
	}
	if !bytes.Equal(writes[1], secretHash(registered.Secret)) || bytes.Equal(writes[0], writes[1]) {
		t.Fatalf("capability writes %x, want a first secret's hash and then the repeat's %x", writes, secretHash(registered.Secret))
	}
	if stored := h.stored(token); !bytes.Equal(stored.CapabilityHash, secretHash(registered.Secret)) {
		t.Fatalf("the claim holds capability hash %x, want the repeat's %x", stored.CapabilityHash, secretHash(registered.Secret))
	}
}

// waitUntil reads cond until it holds, failing the test naming what it waited for after 30
// seconds: the states it waits on (a lock a session waits for, a decision a route has begun) are
// read from Postgres and the server, which say nothing when they change.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited 30 s and %s did not happen", what)
}

// gateWaiters is how many sessions of this test's database wait for registrationGate.
func gateWaiters(t *testing.T, h *harness) int {
	t.Helper()
	var waiting int
	if err := h.store.Pool().QueryRow(h.ctx, `select count(*) from pg_locks l join pg_database d on d.oid = l.database
		where l.locktype = 'advisory' and not l.granted and d.datname = current_database() and l.objid::bigint = $1`,
		registrationGate).Scan(&waiting); err != nil {
		t.Fatalf("read the gate's waiters: %v", err)
	}
	return waiting
}

// capabilityWrites is every capability hash the gated write recorded, in the order it took them.
func capabilityWrites(t *testing.T, h *harness) [][]byte {
	t.Helper()
	rows, err := h.store.Pool().Query(h.ctx, `select hash from capability_writes order by seq`)
	if err != nil {
		t.Fatalf("read the capability writes: %v", err)
	}
	writes, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read the capability writes: %v", err)
	}
	return writes
}
