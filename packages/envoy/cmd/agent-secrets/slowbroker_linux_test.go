// packages/envoy/cmd/agent-secrets/slowbroker_linux_test.go
//go:build linux

// `agent-secrets enroll --helper` against a real helper and a real broker (brokertest.NewRig)
// that is slow to answer. This file uses nothing a release before the helper's broker-call bound
// lacks, so the same row also runs against such a release, where it fails: that helper gave itself
// 5 s to write its answer, so when the broker answered after those 5 s (8 s here) it could not
// write the answer and closed the connection without one, while the broker made the enrollment
// the client was never told about. Linux-only for the real helper's pidfd pinning.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// serveRealHelperWith serves a real helper.Server against the broker at brokerURL, sending its
// broker calls through client, with an operator file naming operator (an email, as Dispatch
// names the person who approves) so a test can log it in (Broker.Login). It waits for every
// session to be retired and the server to stop, then closes the registry, which lets a save still
// finishing a registration, a retire or an enrollment complete and makes any later one write
// nothing, and the broker, which does the same for its machine file, before the test's temporary
// directories go.
func serveRealHelperWith(t *testing.T, brokerURL, operator string, client *http.Client) (*helper.Server, string) {
	t.Helper()
	dir := t.TempDir()
	operatorFile := filepath.Join(dir, "operator")
	if err := os.WriteFile(operatorFile, []byte(operator+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &helper.Server{
		Registry: helper.NewRegistry(filepath.Join(dir, "sessions.json")),
		Broker:   &helper.Broker{URL: brokerURL, OperatorFile: operatorFile, MachineFile: filepath.Join(dir, "machine.json"), HTTP: client},
		Hostname: "testhost",
		PeerOf:   helper.PeerOf,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinRenew: 50 * time.Millisecond,
	}
	sock := filepath.Join(shortTempDir(t), "helper.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for len(srv.Registry.List()) > 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		<-served
		srv.Registry.Close()
		srv.Broker.Close()
	})
	return srv, sock
}

// slowEnrollments holds the next holds POST /v1/enrollments from the moment it has the request in
// hand, for hold: a broker that makes the enrollment at once and is slow to say so. A request
// whose context has already ended goes to base as it came, as a transport sends nothing on an
// ended context. A held request goes to base whole, under a context the helper's does not end
// (context.WithoutCancel, which keeps the helper's trace), so the broker makes the enrollment
// however slow a loaded machine makes it, even when its answer comes after the helper's bound.
// The broker's answer is handed on once it is in hand and hold has passed. When the request's
// context ends first, as a transport waiting on a slow answer does, the answer's body is closed
// and the context's error returned, but only once the request is written, so the helper's trace
// has seen it leave. held counts the requests it began to hold; answered counts the broker's
// answers to them, a failure among them, and answer is the last of those, for a test that must
// wait for the broker before it reads the broker's rows.
type slowEnrollments struct {
	base     http.RoundTripper
	hold     time.Duration
	holds    atomic.Int64
	held     atomic.Int64
	answered atomic.Int64
	answer   atomic.Pointer[string]
}

func (s *slowEnrollments) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || req.URL.Path != "/v1/enrollments" || req.Context().Err() != nil || s.holds.Add(-1) < 0 {
		return s.base.RoundTrip(req)
	}
	s.held.Add(1)
	hold := time.NewTimer(s.hold)
	defer hold.Stop()
	wrote := make(chan struct{})
	var once sync.Once
	sent := httptrace.WithClientTrace(context.WithoutCancel(req.Context()), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { once.Do(func() { close(wrote) }) },
	})
	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := s.base.RoundTrip(req.WithContext(sent))
		answer := fmt.Sprint(err)
		if err == nil {
			answer = resp.Status
		}
		s.answer.Store(&answer)
		s.answered.Add(1)
		got <- result{resp, err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			return nil, r.err
		}
		select {
		case <-hold.C:
		case <-req.Context().Done():
		}
		if req.Context().Err() == nil {
			return r.resp, nil
		}
		r.resp.Body.Close()
		return nil, req.Context().Err()
	case <-req.Context().Done():
	}
	select {
	case <-wrote:
		go func() {
			if r := <-got; r.err == nil {
				r.resp.Body.Close()
			}
		}()
	case r := <-got:
		if r.err == nil {
			r.resp.Body.Close()
		}
	}
	return nil, req.Context().Err()
}

// lastAnswer is the last of the broker's answers to a held request, its status or its failure,
// or "none yet".
func (s *slowEnrollments) lastAnswer() string {
	if answer := s.answer.Load(); answer != nil {
		return *answer
	}
	return "none yet"
}

// freshThumbprint is the thumbprint of a new session key.
func freshThumbprint(t *testing.T) string {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return thumbprint
}

// brokerEnrollment is one row of the broker's enrollments table: its id, and whether it is live
// (not revoked and its lease not passed).
type brokerEnrollment struct {
	ID   string
	Live bool
}

// enrollmentsOf reads rig's enrollments of runtimeID, oldest first.
func enrollmentsOf(t *testing.T, rig *brokertest.Rig, runtimeID string) []brokerEnrollment {
	t.Helper()
	// Query's error comes back from the rows, and CollectRows returns it (pgx's Query doc allows this).
	rows, _ := rig.Store.Pool.Query(context.Background(),
		`select id::text, revoked_at is null and lease_expires_at > now() from enrollments where runtime_id = $1 order by created_at`, runtimeID)
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[brokerEnrollment])
	if err != nil {
		t.Fatalf("read the broker's enrollments of %s: %v", runtimeID, err)
	}
	return got
}

// liveIDs are the ids of the live rows among rows.
func liveIDs(rows []brokerEnrollment) []string {
	var ids []string
	for _, e := range rows {
		if e.Live {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// TestEnrollThroughTheHelperAnswersTheBrokersOutcomeWhenTheBrokerIsSlow: the broker makes the
// enrollment at once, and its answer reaches the helper 8 s later. `enroll --helper` exits 0,
// prints the enrollment id alone, records it in the key directory's enrollment file, and the
// broker holds exactly that one live enrollment for the runtime id. The broker's rows are read
// before the exit is judged, and every failure names the exit, both streams and those rows, so a
// failure shows whether the broker made an enrollment the client was not told about.
func TestEnrollThroughTheHelperAnswersTheBrokersOutcomeWhenTheBrokerIsSlow(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	slow := &slowEnrollments{base: http.DefaultTransport, hold: 8 * time.Second}
	_, sock := serveRealHelperWith(t, rig.URL, rig.Operator, &http.Client{Transport: slow})
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("approved machine login (code %s): exit %d, stderr %q; want 0", code, exit, stderr)
	}
	runtimeID := "slow-box-" + t.Name()
	keyDir := filepath.Join(t.TempDir(), "keys")

	slow.holds.Store(1)
	stdout, stderr, exit := runAgentSecrets(t, binary, rig.URL, keyDir, []string{"AGENT_SECRETS_HELPER_SOCK=" + sock},
		"enroll", "--helper", "--runtime-id", runtimeID, "--thumbprint", freshThumbprint(t))
	rows := enrollmentsOf(t, rig, runtimeID)
	outcome := fmt.Sprintf("exit %d, stdout %q, stderr %q; the broker's rows for %s: %+v", exit, stdout, stderr, runtimeID, rows)

	if n := slow.held.Load(); n != 1 {
		t.Fatalf("the transport held %d requests of POST /v1/enrollments, want 1: %s", n, outcome)
	}
	if exit != 0 {
		t.Fatalf("enroll --helper against a broker 8 s slow to answer: %s; want exit 0", outcome)
	}
	live := liveIDs(rows)
	if len(live) != 1 {
		t.Fatalf("enroll --helper against a broker 8 s slow to answer: %s; want exactly one live row", outcome)
	}
	if stdout != live[0]+"\n" {
		t.Fatalf("enroll --helper against a broker 8 s slow to answer: %s; want stdout to be the live row's id alone", outcome)
	}
	if recorded, err := os.ReadFile(filepath.Join(keyDir, "enrollment")); err != nil || string(recorded) != live[0]+"\n" {
		t.Fatalf("the key directory's enrollment file holds %q (%v): %s; want the live row's id", recorded, err, outcome)
	}
}
