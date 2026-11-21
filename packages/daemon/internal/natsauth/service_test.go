package natsauth_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
)

// daemonAccount is the account the daemon's projected token names in these tests.
const daemonAccount = "system:serviceaccount:legion-pub:legion-daemon"

// writeToken writes token to the file at path as the kubelet writes a projected token: whole, in
// place of the one before.
func writeToken(t *testing.T, path, token string) {
	t.Helper()
	next := path + ".next"
	if err := os.WriteFile(next, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

// The daemon's callout connection retries a refusal: the auth service refuses every client while
// it is away, or before it holds the answer a decision needs, so the daemon's connection keeps
// presenting its token. Here the stand-in refuses its first four CONNECTs, then admits it, and the
// connection ends connected, having never closed, and answers a request on its account's own reply
// prefix, the only reply subjects the stand-in lets it subscribe. The control builds the same
// connection without IgnoreAuthErrorAbort: nats.go closes it after the second identical refusal,
// and this test fails naming the close. A seed's connection keeps that close
// (TestADaemonSeedRefusedTwiceCloses).
func TestADaemonCalloutConnectionKeepsRetryingARefusal(t *testing.T) {
	s := testnats.StartCallout(t, 4, natsgrant.InboxPrefix(daemonAccount)+".>", "test.>")
	tokenFile := filepath.Join(t.TempDir(), "nats-token")
	writeToken(t, tokenFile, testnkeys.ServiceToken(daemonAccount, time.Now().Add(time.Hour)))
	connected, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	signal := func(to chan struct{}) func(*nats.Conn) {
		return func(*nats.Conn) {
			select {
			case to <- struct{}{}:
			default:
			}
		}
	}
	conn, err := natsauth.ConnectService([]string{s.URL}, tokenFile, t.Name(),
		nats.RetryOnFailedConnect(true), natsauth.ReconnectForever(), nats.ReconnectWait(50*time.Millisecond),
		nats.ClosedHandler(signal(closed)), nats.ConnectHandler(signal(connected)), nats.ReconnectHandler(signal(connected)))
	if err != nil {
		t.Fatalf("ConnectService with RetryOnFailedConnect answered %v; it should hand back a connection that keeps trying", err)
	}
	defer conn.Close()
	select {
	case <-connected:
	case <-closed:
		t.Fatalf("the daemon's callout connection closed after %d refused CONNECTs: nats.go gave up after the same refusal twice", len(s.Presented()))
	case <-time.After(30 * time.Second):
		t.Fatalf("the daemon's callout connection was never admitted (CONNECTs presented: %d)", len(s.Presented()))
	}
	if conn.IsClosed() {
		t.Fatal("the daemon's callout connection closed on a refusal: nats.go gave up after the same refusal twice")
	}
	if got := len(s.Presented()); got != 5 {
		t.Errorf("the connection presented its token on %d CONNECTs, want 5: four refused, then the one admitted", got)
	}

	if _, err := conn.Subscribe("test.echo", func(msg *nats.Msg) { _ = msg.Respond(msg.Data) }); err != nil {
		t.Fatal(err)
	}
	reply, err := conn.Request("test.echo", []byte("hello"), 5*time.Second)
	if err != nil {
		t.Fatalf("a request got no answer on the account's own reply prefix %s: %v", natsgrant.InboxPrefix(daemonAccount), err)
	}
	if string(reply.Data) != "hello" || !strings.HasPrefix(reply.Subject, natsgrant.InboxPrefix(daemonAccount)+".") {
		t.Errorf("reply = %q on %s, want hello on the prefix %s", reply.Data, reply.Subject, natsgrant.InboxPrefix(daemonAccount))
	}
}

// The user the auth service answers expires with the token the connection presented, so the
// daemon's connection moves to a later token natscred.ServiceRenewBefore before that one lapses.
// The token here lapses that long and two seconds from now; the kubelet's rewrite is a later token
// in the same file; and the stand-in is shown that later token on a CONNECT of its own while the
// first is still live, with the connection connected after it. The control is a connection whose
// keeper does not run: it never presents the later token, and this test fails naming the CONNECTs
// it saw.
func TestConnectWithAServiceTokenReconnectsBeforeExpiry(t *testing.T) {
	s := testnats.StartCallout(t, 0, natsgrant.InboxPrefix(daemonAccount)+".>")
	tokenFile := filepath.Join(t.TempDir(), "nats-token")
	first := testnkeys.ServiceToken(daemonAccount, time.Now().Add(natscred.ServiceRenewBefore+2*time.Second))
	writeToken(t, tokenFile, first)
	conn, err := natsauth.ConnectService([]string{s.URL}, tokenFile, t.Name(), natsauth.ReconnectForever(), nats.ReconnectWait(50*time.Millisecond))
	if err != nil {
		t.Fatalf("ConnectService: %v", err)
	}
	defer conn.Close()
	later := testnkeys.ServiceToken(daemonAccount, time.Now().Add(time.Hour))
	writeToken(t, tokenFile, later)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		presented := s.Presented()
		if len(presented) >= 2 && presented[len(presented)-1] == later && conn.IsConnected() {
			if presented[0] != first {
				t.Fatal("the first CONNECT presented another token than the file held")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the connection never moved to the later token before the first lapsed: %d CONNECTs, connected %t", len(s.Presented()), conn.IsConnected())
}

// The token's file is read at boot, and a file that cannot be read, is empty or holds no token
// that names an account is refused naming the key and the path.
func TestServiceIdentityNamesTheTokensAccount(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "token")
	writeToken(t, good, testnkeys.ServiceToken(daemonAccount, time.Now().Add(time.Hour)))
	account, err := natsauth.ServiceIdentity(good)
	if err != nil || account != daemonAccount {
		t.Fatalf("ServiceIdentity = %q, %v; want %q", account, err, daemonAccount)
	}
	empty := filepath.Join(dir, "empty")
	writeToken(t, empty, "")
	notAJWT := filepath.Join(dir, "not-a-jwt")
	writeToken(t, notAJWT, "not-a-token")
	for _, row := range []struct{ path, says string }{
		{filepath.Join(dir, "missing"), "whose token could not be read"},
		{empty, "whose token could not be read"},
		{notAJWT, "whose token names no account"},
	} {
		_, err := natsauth.ServiceIdentity(row.path)
		if err == nil || !strings.Contains(err.Error(), natsauth.ServiceTokenFileKey+" names "+row.path+", "+row.says) {
			t.Errorf("ServiceIdentity(%s) = %v, want an error naming %s and %q", row.path, err, natsauth.ServiceTokenFileKey, row.says)
		}
	}
}

// While running, the service token's connection waits its reconnects out on the boot's own
// schedule: one second, doubling, capped at a minute.
func TestReconnectBackoffDoublesToItsCap(t *testing.T) {
	options := nats.GetDefaultOptions()
	if err := natsauth.ReconnectBackoff(time.Second, time.Minute)(&options); err != nil {
		t.Fatal(err)
	}
	var got []time.Duration
	for attempt := 1; attempt <= 9; attempt++ {
		got = append(got, options.CustomReconnectDelayCB(attempt))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute, time.Minute}
	if !slices.Equal(got, want) {
		t.Fatalf("ReconnectBackoff waits %v, want %v", got, want)
	}
}

// Each refusal of a reconnect's credential is logged with its reason, as a refusal and not as
// some other asynchronous error; only the service token's connection then tries again.
func TestLogEventsNamesARefusedCredential(t *testing.T) {
	var out bytes.Buffer
	options := nats.GetDefaultOptions()
	if err := natsauth.LogEvents(slog.New(slog.NewTextHandler(&out, nil)))(&options); err != nil {
		t.Fatal(err)
	}
	options.AsyncErrorCB(nil, nil, nats.ErrAuthorization)
	want := `level=WARN msg="NATS refused the connection's credential" error="nats: authorization violation"`
	if !strings.Contains(out.String(), want) {
		t.Fatalf("log = %q, want a line containing %s", out.String(), want)
	}
	if !natsauth.Refused(nats.ErrAuthorization) || natsauth.Refused(nats.ErrNoServers) {
		t.Fatal("Refused does not tell an authorization violation from an unreachable server")
	}
}
