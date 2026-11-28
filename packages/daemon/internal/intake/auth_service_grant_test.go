package intake

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/natsgrant"
	"github.com/sjawhar/envoy/testnatscallout"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// daemonGrantAccount is the ServiceAccount whose projected token the daemon presents to the hub's
// auth service in TestTheConsumersRunUnderTheAuthServicesDaemonGrant, listed under a daemon/-/*
// entry, which the service answers with natsgrant.Daemon() whole.
const daemonGrantAccount = "system:serviceaccount:legion-example:legion-daemon"

// daemonGrantCalls is natsgrant.Daemon() as data: each entry of the daemon's grant, by its
// direction and subject, with the call of the daemon's that needs it and the test whose run makes
// that call under the grant the auth service answers. Every NATS call the daemon makes today is
// one of these: its intake consumers' JetStream API requests and acknowledgements, the role-lane
// exceptions subscription, and the replies every request is answered on. Its notices go over HTTP
// (internal/notify, HTTPPublisher.Publish), so it publishes no core subject.
// TestTheConsumersRunUnderTheAuthServicesDaemonGrant fails on an entry of the grant this table does
// not map, so an entry added to the grant names the call that needs it and the test that makes it.
var daemonGrantCalls = []daemonGrantCall{
	{"publish", "$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS", "OpenConsumers and DispatchTarget (js.Stream)",
		"TestTheConsumersRunUnderTheAuthServicesDaemonGrant"},
	{"publish", "$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS", "Lag and DispatchPosition (GetMsg)",
		"TestTheConsumersRunUnderTheAuthServicesDaemonGrant"},
	{"publish", "$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.>", "stream.Consumer at the reopen, Lag, DispatchPosition",
		"TestTheConsumersRunUnderTheAuthServicesDaemonGrant"},
	{"publish", "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.>", "CreateConsumer, UpdateConsumer",
		"TestTheConsumersRunUnderTheAuthServicesDaemonGrant"},
	{"publish", "$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.>", "Consume",
		"TestTheConsumersRunUnderTheAuthServicesDaemonGrant"},
	{"publish", "$JS.ACK.ENVOY_NOTIFICATIONS.>", "Ack, NakWithDelay, InProgress",
		"TestTheConsumersRunUnderTheAuthServicesDaemonGrant"},
	{"subscribe", "notifications.envoy.exceptions.notifications.role.>", "subscribeNoticeExceptions",
		"natsauth's TestAServiceTokenConnectionIsAdmittedByTheAuthServiceAsTheDaemon, and internal/daemon's TestTheWorkflowConnectsThroughTheAuthServiceByItsServiceToken"},
	{"subscribe", "_INBOX.>", "every JetStream API reply",
		"natsauth's TestAServiceTokenConnectionIsAdmittedByTheAuthServiceAsTheDaemon, and TestTheConsumersRunUnderTheAuthServicesDaemonGrant"},
}

// daemonGrantCall is one row of daemonGrantCalls: a grant entry's direction ("publish" or
// "subscribe") and subject, the call that needs it, and the test that makes that call through the
// auth service.
type daemonGrantCall struct{ direction, subject, call, test string }

// readDaemonGrantToken is the hub's verifier of the daemon's token in the broker's place
// (testnatscallout.Verifier): the account, issue and expiry the unsigned token names, read
// unverified, as natscred's rig reads a platform token (natscred/rig_test.go, readPlatformToken).
func readDaemonGrantToken(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", false, time.Time{}, time.Time{}, errors.New("no JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false, time.Time{}, time.Time{}, err
	}
	var claims struct {
		Sub      string `json:"sub"`
		Iat, Exp int64
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false, time.Time{}, time.Time{}, fmt.Errorf("claims: %w", err)
	}
	return claims.Sub, false, time.Unix(claims.Iat, 0), time.Unix(claims.Exp, 0), nil
}

// daemonGrantTokenFile is a file of its own under t's temporary directory holding a token of
// daemonGrantAccount's that lives an hour, written as the kubelet writes a projected token: whole,
// to a file beside it, renamed into place.
func daemonGrantTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	next := path + ".next"
	if err := os.WriteFile(next, []byte(testnkeys.ServiceToken(daemonGrantAccount, time.Now().Add(time.Hour))+"\n"), 0o644); err != nil {
		t.Fatalf("write the token beside its file: %v", err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatalf("rename the token into its file: %v", err)
	}
	return path
}

// daemonGrantRefusals is every line in which the daemon's log (LogEvents) or the hub's server log
// names a permission the server refused the daemon's connection, the subject among them, joined.
func daemonGrantRefusals(logs *lockedBuffer, hub *testnatscallout.Hub) string {
	var lines []string
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "NATS refused the daemon a permission") {
			lines = append(lines, line)
		}
	}
	for line := range strings.SplitSeq(hub.ServerLog(), "\n") {
		if strings.Contains(line, "Publish Violation") || strings.Contains(line, "Subscription Violation") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// The daemon's intake consumers, its publishes and its probes all run under the grant the real NATS
// auth service answers a daemon/-/* entry with (natsgrant.Daemon()), on a connection
// natsauth.ConnectService makes with the daemon's service token: the class "every NATS call it
// makes today" (R2), whose members are the entries of natsgrant.Daemon(), each mapped in
// daemonGrantCalls to the call that needs it, and the test fails on an entry the table does not
// map. On the hub, with LogEvents capturing the connection's refusals:
//
//   - OpenConsumers creates the three durables, and a second OpenConsumers on a new connection
//     reopens them (a restarted daemon).
//   - The admin publishes the Dispatch fixture testdata/dispatch/issue-updated.json, the GitHub
//     fixture testdata/github/pr-opened.json and a listener wake envelope built here (no wake
//     fixture is in testdata/), and Lag reads each consumer's one waiting event with its time.
//   - DispatchTarget, DispatchPosition and ReconcilePosition answer before the consumers run, and
//     again once they have, when the Dispatch consumer's idle is confirmed by a read of the stream's
//     store.
//   - Run, with a handler that fails its first call, naks that fact and then applies and
//     acknowledges each, and each consumer's ack floor moves to its event.
//
// Nothing is refused: no "NATS refused the daemon a permission" line in the daemon's log, no
// "Permissions Violation" in the hub's, and no read of the stream's store that went unanswered. A
// call the grant lacks fails the test naming the refused subject, from both logs.
func TestTheConsumersRunUnderTheAuthServicesDaemonGrant(t *testing.T) {
	// Not parallel: its Lag and position readings hold only while each NATS read beats consumerOldestBound, 1 s, a wall-clock bound the load of tests beside it can cross.
	grant := natsgrant.Daemon()
	for _, entry := range []struct {
		direction string
		subjects  []string
	}{{"publish", grant.Publish}, {"subscribe", grant.Subscribe}} {
		for _, subject := range entry.subjects {
			if !slices.ContainsFunc(daemonGrantCalls, func(row daemonGrantCall) bool {
				return row.direction == entry.direction && row.subject == subject
			}) {
				t.Errorf("natsgrant.Daemon() grants the %s of %s, which daemonGrantCalls does not map: name the call that needs it and the test that makes that call through the auth service",
					entry.direction, subject)
			}
		}
	}
	for _, denied := range slices.Concat(grant.PublishDeny, grant.SubscribeDeny) {
		t.Errorf("natsgrant.Daemon() denies %s, which daemonGrantCalls does not map: a deny is a call the daemon must not make, which no test here holds", denied)
	}
	for _, row := range daemonGrantCalls {
		t.Logf("the grant's %s of %s: %s, made through the auth service by %s", row.direction, row.subject, row.call, row.test)
	}

	ctx := context.Background()
	hub := testnatscallout.Start(t, "daemon/-/*="+daemonGrantAccount, readDaemonGrantToken)
	admin, err := jetstream.New(hub.Admin(t))
	if err != nil {
		t.Fatalf("open JetStream as the hub's admin: %v", err)
	}
	stream, err := admin.CreateStream(ctx, jetstream.StreamConfig{Name: notificationStream, Subjects: []string{"notifications.>"}})
	if err != nil {
		t.Fatalf("create the notification stream as the hub's admin: %v", err)
	}
	file := daemonGrantTokenFile(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	pool := migratedPool(t)
	refusals := func() string { return daemonGrantRefusals(logs, hub) }
	connect := func(which string) jetstream.JetStream {
		t.Helper()
		conn, err := natsauth.ConnectService([]string{hub.URL()}, file, natsauth.ServiceTokenFileKey, t.Name()+" "+which,
			nats.Timeout(10*time.Second), natsauth.LogEvents(spec.Logger), natsauth.ReconnectForever())
		if err != nil {
			t.Fatalf("connect the %s daemon through the auth service: %v\nthe auth service's log:\n%s", which, err, hub.ServiceLog())
		}
		t.Cleanup(conn.Close)
		js, err := jetstream.New(conn)
		if err != nil {
			t.Fatalf("open JetStream on the %s daemon's connection: %v", which, err)
		}
		return js
	}
	names := []string{dispatchConsumerName(spec.Project), githubConsumerName(spec.Project), wakeConsumerName(spec.Project)}

	first := connect("first")
	if _, err := OpenConsumers(ctx, first, pool, spec); err != nil {
		t.Fatalf("OpenConsumers under the auth service's daemon grant: %v\n%s", err, refusals())
	}
	for _, name := range names {
		if _, err := stream.Consumer(ctx, name); err != nil {
			t.Fatalf("the first OpenConsumers created no %s: %v\n%s", name, err, refusals())
		}
	}
	first.Conn().Close()

	consumers, err := OpenConsumers(ctx, connect("restarted"), pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers on a new connection, over the consumers the first made: %v\n%s", err, refusals())
	}

	dispatchSent := publish(t, admin, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	githubSent := publish(t, admin, "notifications.github.sjawhar.legion.pr.42", capturedGitHubEnvelope(t, "pr-opened.json"))
	wakeSent := publish(t, admin, legionWakeSubject, envelopeJSON(t, "listener-wake-grant", "envoy",
		fmt.Sprintf(`{"kind":"listener_ready","at":%d}`, time.Now().UnixMilli())))
	for _, name := range names {
		awaitWaiting(t, stream, name, 1)
	}
	lags := consumers.Lag(ctx)
	if got := lagNames(lags); !slices.Equal(got, names) {
		t.Fatalf("Lag read %v, want %v", got, names)
	}
	for _, lag := range lags {
		if lag.Waiting != 1 || lag.OldestAt.IsZero() || lag.Unavailable != "" {
			t.Errorf("the lag of %s under the auth service's daemon grant = %+v, want its one event waiting with its time and nothing unavailable\n%s", lag.Name, lag, refusals())
		}
	}
	target, err := consumers.DispatchTarget(ctx)
	if err != nil || target != int64(wakeSent.Sequence) {
		t.Fatalf("DispatchTarget = %d, %v; want the stream's last sequence, %d\n%s", target, err, wakeSent.Sequence, refusals())
	}
	if behind, err := consumers.DispatchPosition(ctx); err != nil || behind.Idle {
		t.Fatalf("DispatchPosition with the Dispatch event waiting = %+v, %v; want a position that is not idle\n%s", behind, err, refusals())
	}
	if _, err := consumers.ReconcilePosition(ctx); err != nil {
		t.Fatalf("ReconcilePosition with the Dispatch event waiting: %v\n%s", err, refusals())
	}

	var calls atomic.Int32
	stop := runConsumers(t, consumers, handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
		if calls.Add(1) == 1 {
			return Result{}, errors.New("fail the first fact, so its consumer naks it")
		}
		return Result{}, nil
	}))
	defer stop()
	testwait.Eventually(t, "the consumers naking the first fact and applying every fact", func() bool { return calls.Load() >= 4 })
	assertAcknowledgedThrough(t, stream, names[0], dispatchSent)
	assertAcknowledgedThrough(t, stream, names[1], githubSent)
	assertAcknowledgedThrough(t, stream, names[2], wakeSent)

	caught, err := consumers.DispatchPosition(ctx)
	if err != nil || !caught.Idle || caught.AckFloorStream != int64(dispatchSent.Sequence) || !caught.Reached(target) {
		t.Errorf("DispatchPosition once every event is acknowledged = %+v, %v; want idle at ack floor %d, which reaches the target %d\n%s",
			caught, err, dispatchSent.Sequence, target, refusals())
	}
	if reconciled, err := consumers.ReconcilePosition(ctx); err != nil || !reconciled.Idle {
		t.Errorf("ReconcilePosition once every event is acknowledged = %+v, %v; want idle, confirmed by the stream's store\n%s", reconciled, err, refusals())
	}
	if lines := refusals(); lines != "" {
		t.Fatalf("the daemon's connection was refused a permission under the auth service's daemon grant:\n%s", lines)
	}
	if strings.Contains(logs.String(), "could not be read") {
		t.Fatalf("a read of the stream's store went unanswered under the auth service's daemon grant:\n%s", logs.String())
	}
}
