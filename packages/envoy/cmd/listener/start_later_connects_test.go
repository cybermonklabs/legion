package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
)

// laterStepWatchdog bounds each wait of the tests below on a connect of the start.
const laterStepWatchdog = time.Minute

// mainsTenTries is when main's connect ends on ten dials that all fail for another reason than a
// refusal: each dial is followed by a wait of a second, the tenth too (connectTrying), and a sleep
// never ends before its wait, so no such connect ends sooner.
const mainsTenTries = 10 * time.Second

// mainsEndMargin is how much later than main's own connect, run beside it against the same port,
// a start of ten such failed dials may end. Both make ten dials to a refused loopback port, which
// take milliseconds each, and ten waits of a second; all that differs is when the scheduler wakes
// each one's timers and goroutines, which on this box's runs came to under a tenth of a second
// (nine waits measured 9.02 to 9.09 s). Half a second is five times that, and stays under the one
// second an eleventh wait, or any other hang of a second or more after the tenth dial, adds.
const mainsEndMargin = 500 * time.Millisecond

// mainsConnect runs main's connect, bus.Connect with no credential wait, against urls on a
// goroutine of its own, beside a start of the test's, and answers a channel that carries how it
// ended and how long it took (requireMainsEnd reads it).
func mainsConnect(urls []string) <-chan mainsOutcome {
	ended := make(chan mainsOutcome, 1)
	began := time.Now()
	go func() {
		client, err := bus.Connect(urls, bus.WithName("mains-connect"))
		ended <- mainsOutcome{client: client, err: err, took: time.Since(began)}
	}()
	return ended
}

// mainsOutcome is how main's connect (mainsConnect) ended.
type mainsOutcome struct {
	client *bus.Client
	err    error
	took   time.Duration
}

// requireMainsEnd fails the test unless took, how long a start of ten dials that failed for another
// reason than a refusal ran, is no shorter than mainsTenTries and no longer than main's own connect
// beside it (mainsConnect, read from mains) took plus mainsEndMargin.
func requireMainsEnd(t *testing.T, what string, took time.Duration, mains <-chan mainsOutcome) {
	t.Helper()
	var got mainsOutcome
	select {
	case got = <-mains:
	case <-time.After(laterStepWatchdog):
		t.Fatalf("watchdog: main's connect beside %s had not returned %s after %s ended", what, laterStepWatchdog, what)
	}
	if got.err == nil {
		got.client.Close()
		t.Fatalf("main's connect beside %s connected to a port where nothing listens", what)
	}
	t.Logf("%s ended %s after it began; main's connect beside it, %s", what, took, got.took)
	if took < mainsTenTries {
		t.Errorf("%s ended %s after it began, sooner than main's ten tries a second apart, the tenth followed by its wait too, end, at %s", what, took, mainsTenTries)
	}
	if most := got.took + mainsEndMargin; took > most {
		t.Errorf("%s ended %s after it began, later than main's connect beside it (%s) and %s of margin, %s", what, took, got.took, mainsEndMargin, most)
	}
}

// TestALaterConnectBegunNearTheCredentialBoundKeepsMainsTenTries: a connect of the start begun
// with half a second of the start's 180 s left, as the role lane is when the main client took
// 179.5 s to get in, still makes main's ten tries a second apart on a dial that fails for another
// reason than a refusal, here a port where nothing listens, and ends where main's connect beside
// it ends, a second after its tenth dial: no sooner, so no start ends sooner than main's would,
// and no later than mainsEndMargin past it, so the wait after the tenth dial hides no hang. A
// connect that ended at the bound would end after two tries, half a second apart.
func TestALaterConnectBegunNearTheCredentialBoundKeepsMainsTenTries(t *testing.T) {
	start := newNATSStart(listenerCredentialWait)
	start.once.Do(func() { start.began = time.Now().Add(-listenerCredentialWait + 500*time.Millisecond) })
	cfg := config.Service{MachineID: "start-near-the-bound", NATSURLs: []string{"nats://" + testport.Refused(t)}, NATSReplicas: 1}
	type outcome struct {
		lane roleLane
		err  error
		at   time.Time
	}
	began := time.Now()
	mains := mainsConnect(cfg.NATSURLs)
	returned := make(chan outcome, 1)
	go func() {
		lane, err := startRoleLane(cfg, nil, func(*nats.Msg) {}, nil, metrics.New(), logging.NewWithWriter("test", io.Discard), start)
		returned <- outcome{lane, err, time.Now()}
	}()
	var got outcome
	select {
	case got = <-returned:
	case <-time.After(laterStepWatchdog):
		t.Fatalf("watchdog: the role lane's connect had not returned %s after it began", laterStepWatchdog)
	}
	took := got.at.Sub(began)
	if got.err == nil {
		got.lane.stopWatch()
		got.lane.client.Close()
		t.Fatalf("the role lane connected to a port where nothing listens, %s after it began", took)
	}
	t.Logf("the role lane's connect ended: %v", got.err)
	requireMainsEnd(t, "the role lane's connect, begun with half a second of the start's bound left,", took, mains)
	if refusal := start.refusal.Load(); refusal != nil {
		t.Fatalf("the start reported a refused dial as a refusal of its credential: %s", *refusal)
	}
}

// TestALaterStepThatFailsAfterItsConnectGotInNamesNoCredential: the refusal a connect of the start
// waited out is forgotten once that connect gets in, not once the whole step it belongs to has
// succeeded. The role lane's connect waits out a refusal of the nkey, the server then admits the
// nkey but refuses its subscription to the role lane (in the shared queue group and in the
// machine's), and the step fails after its connect got in. /healthz then answers as on main, and
// the line main ends the start with (natsStart.stepFailed) names the step's error and no
// nats_credential: the credential is not what failed. Forgetting the refusal at the step's end
// would leave /healthz reporting it and the line naming the credential.
func TestALaterStepThatFailsAfterItsConnectGotInNamesNoCredential(t *testing.T) {
	seed, public := testnats.User(t)
	_, fallback := testnats.User(t)
	server, uri := testnats.StartConfig(t, nkeyAuthorization(fallback), []string{"-js"}, true)
	t.Setenv(bus.NkeySeedEnvVar, seed)
	cfg := config.Service{MachineID: "start-later-step", NATSURLs: []string{uri}, NATSReplicas: 1}
	start := newNATSStart(listenerCredentialWait)
	base := serveStartRoutes(t, start)
	type outcome struct {
		lane roleLane
		err  error
	}
	began := time.Now()
	returned := make(chan outcome, 1)
	go func() {
		lane, err := startRoleLane(cfg, nil, func(*nats.Msg) {}, nil, metrics.New(), logging.NewWithWriter("test", io.Discard), start)
		returned <- outcome{lane, err}
	}()
	for {
		status, _ := probe(t, base, "/healthz")
		if status == http.StatusServiceUnavailable {
			break
		}
		if time.Since(began) > laterStepWatchdog {
			t.Fatalf("watchdog: /healthz did not report the refusal of the role lane's connect within %s", laterStepWatchdog)
		}
		select {
		case got := <-returned:
			t.Fatalf("the role lane's step returned before the server admitted its nkey: %v", got.err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	refusesRoleLane := fmt.Sprintf("{nkey: %q, permissions: {subscribe: {deny: [%q]}}}", public, contracts.RoleTopicPrefix+">")
	if err := server.Reload(t, nkeyAuthorization(fallback, refusesRoleLane)); err != nil {
		t.Fatalf("reload the server with the nkey admitted and its role-lane subscription refused: %v", err)
	}
	var got outcome
	select {
	case got = <-returned:
	case <-time.After(laterStepWatchdog):
		t.Fatalf("watchdog: the role lane's step had not returned %s after the server admitted its nkey", laterStepWatchdog)
	}
	if got.err == nil {
		got.lane.stopWatch()
		got.lane.client.Close()
		t.Fatalf("the role lane subscribed where the server refuses its subscription")
	}
	t.Logf("the role lane's step failed after its connect got in: %v", got.err)
	// Both checks below report, so a run shows each of the two places the refusal outlived its
	// connect.
	if status, fields := probe(t, base, "/healthz"); status != http.StatusOK || fields["status"] != "starting" {
		t.Errorf("/healthz after the role lane's connect got in and its subscription failed = %d %v, want main's 200 starting", status, fields)
	}
	var line bytes.Buffer
	start.stepFailed(logging.NewWithWriter("test", &line), "core role subscription failed", got.err, os.LookupEnv)
	if !strings.Contains(line.String(), "core role subscription failed") {
		t.Fatalf("the line that ends the start is %q, want it to name the step", line.String())
	}
	if strings.Contains(line.String(), "nats_credential") || strings.Contains(line.String(), public) {
		t.Errorf("the line that ends the start, %q, names the credential, though its connect got in and the step failed afterwards", line.String())
	}
}

// laterConnectRefusedFor is how long TestTheListenerStartWaitsOutACredentialRefusalAtItsLaterConnects
// has the server refuse the nkey while a later connect of the start runs: past the ten tries a
// second apart that a connect without the wait makes, about ten seconds, so such a connect has
// ended with the refusal by then.
const laterConnectRefusedFor = 15 * time.Second

// TestTheListenerStartWaitsOutACredentialRefusalAtItsLaterConnects: the listener's start does not
// end once its main client has connected (connectListenerBus). It goes on to open the role lane
// (startRoleLane) and the session-consumer connection (startSessionDelivery), each on a connection
// of its own, and each ends the start when it fails. Behind a hub whose servers reload a new nkey
// one at a time, the main client can get in through the one server that has reloaded while a later
// connect meets one that has not. So each later connect waits out a refusal of the credential as
// the main client does, under the start's own bound (natsStart.option), and /healthz answers 503
// naming the refusal while it does. The server here admits the nkey for the main client, then
// refuses it for laterConnectRefusedFor while a later connect runs, then admits it again: the
// connect has not returned by the time the server admits the nkey again, and then it connects, and
// /healthz answers as on main. A later connect without the wait ends with the refusal after its ten
// tries, before the server admits the nkey again.
func TestTheListenerStartWaitsOutACredentialRefusalAtItsLaterConnects(t *testing.T) {
	const watchdog = 60 * time.Second
	logger := logging.NewWithWriter("test", io.Discard)
	for _, row := range []struct {
		name string
		// connect is the later connect under test, as main makes it, answering what closes it.
		connect func(cfg config.Service, client *bus.Client, start *natsStart) (func(), error)
	}{
		{
			name: "the role lane",
			connect: func(cfg config.Service, _ *bus.Client, start *natsStart) (func(), error) {
				lane, err := startRoleLane(cfg, nil, func(*nats.Msg) {}, nil, metrics.New(), logger, start)
				if err != nil {
					return nil, err
				}
				return func() { lane.stopWatch(); lane.client.Close() }, nil
			},
		},
		{
			name: "the session-consumer connection",
			connect: func(cfg config.Service, client *bus.Client, start *natsStart) (func(), error) {
				delivery, err := startSessionDelivery(cfg, bus.SessionConsumerInactiveThreshold, client, logger, nil, start)
				if err != nil {
					return nil, err
				}
				return delivery.close, nil
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			seed, public := testnats.User(t)
			_, fallback := testnats.User(t)
			server, uri := testnats.StartConfig(t, nkeyAuthorization(fallback, nkeyUser(public)), []string{"-js"}, true)
			t.Setenv(bus.NkeySeedEnvVar, seed)
			cfg := config.Service{MachineID: "start-later-connects", NATSURLs: []string{uri}, NATSReplicas: 1}
			start := newNATSStart(listenerCredentialWait)
			base := serveStartRoutes(t, start)
			client, err := connectListenerBus(cfg, nil, start)
			if err != nil {
				t.Fatalf("the start's main client did not connect while the server admitted its nkey: %v", err)
			}
			t.Cleanup(client.Close)

			if err := server.Reload(t, nkeyAuthorization(fallback)); err != nil {
				t.Fatalf("reload the server without the listener's nkey: %v", err)
			}
			type outcome struct {
				closer func()
				err    error
				at     time.Time
			}
			began := time.Now()
			returned := make(chan outcome, 1)
			go func() {
				closer, err := row.connect(cfg, client, start)
				returned <- outcome{closer, err, time.Now()}
			}()
			select {
			case got := <-returned:
				if got.closer != nil {
					got.closer()
				}
				t.Fatalf("%s's connect returned %s after it began, while the server refused the nkey and before it admitted it again: %v", row.name, got.at.Sub(began), got.err)
			case <-time.After(laterConnectRefusedFor):
			}
			if status, fields := probe(t, base, "/healthz"); status != http.StatusServiceUnavailable || !strings.Contains(fmt.Sprint(fields["error"]), authorizationViolation) {
				t.Fatalf("/healthz answered %d %v while %s's connect waited out a refusal, want 503 naming %q", status, fields, row.name, authorizationViolation)
			}
			if err := server.Reload(t, nkeyAuthorization(fallback, nkeyUser(public))); err != nil {
				t.Fatalf("reload the server with the listener's nkey: %v", err)
			}
			var got outcome
			select {
			case got = <-returned:
			case <-time.After(watchdog):
				t.Fatalf("watchdog: %s's connect had not returned %s after the server admitted the nkey again", row.name, watchdog)
			}
			if got.err != nil {
				t.Fatalf("%s's connect returned %v %s after it began, want it to connect once the server admits the nkey again", row.name, got.err, got.at.Sub(began))
			}
			t.Cleanup(got.closer)
			if status, fields := probe(t, base, "/healthz"); status != http.StatusOK || fields["status"] != "starting" {
				t.Fatalf("/healthz after %s's connect = %d %v, want main's 200 starting", row.name, status, fields)
			}
		})
	}
}

// The line a start on NATS ends with names the credential only when the start met a refusal of
// it: an operator who reads "the NATS credential: nkey user ..." looks for that user in the hub's
// authorization, which is the wrong place when the start ended for another reason, a stream the
// cluster could not reconcile, say. The JSON lines the later connects end the start with add the
// credential on the same terms (natsStart.refusedCredential).
func TestTheStartsLastLineNamesTheCredentialOnlyAfterARefusal(t *testing.T) {
	t.Parallel()
	seed, public := testnats.User(t)
	environment := testnats.SeedOnly(seed)
	start := newNATSStart(listenerCredentialWait)
	failed := errors.New("ensure stream ENVOY_NOTIFICATIONS: ENVOY_NATS_REPLICAS=3: replicas > 1 not supported in non-clustered mode")
	line := startFailureLine(failed, start, environment)
	if want := "connect to NATS and reconcile the " + bus.Stream + " stream: " + failed.Error(); !strings.HasPrefix(line, want) {
		t.Fatalf("the start's last line is %q, want it to begin %q", line, want)
	}
	if strings.Contains(line, public) || strings.Contains(line, "credential") {
		t.Fatalf("the start's last line %q names the credential, though nothing refused it", line)
	}
	if attrs := start.refusedCredential(environment); len(attrs) != 0 {
		t.Fatalf("a start that met no refusal adds %v to the line that ends it, want nothing", attrs)
	}

	start.refused(errors.New("nats: Authorization Violation"))
	line = startFailureLine(errors.New("bus: the NATS server still refused the credential when the start's 3m0s wait ran out: nats: Authorization Violation"), start, environment)
	if !strings.Contains(line, "the NATS credential: nkey user "+public) {
		t.Fatalf("the start's last line %q does not name the credential the server refused, nkey user %s", line, public)
	}
	if attrs := start.refusedCredential(environment); len(attrs) != 1 || attrs[0].Key != "nats_credential" || attrs[0].Value.String() != "nkey user "+public {
		t.Fatalf("a start that met a refusal adds %v to the line that ends it, want nats_credential=nkey user %s", attrs, public)
	}
}
