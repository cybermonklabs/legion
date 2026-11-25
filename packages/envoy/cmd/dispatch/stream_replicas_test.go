package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The Dispatch server's starts of the stream here go through dispatchStreamOptions over what
// resolveBootConfig read from the settings table, connecting in the background as main's do
// (serverClient, connectDispatchStream), on an in-process three-server cluster
// (testnats.StartCluster), one per test or subtest, unless a test says single server.

// replicasTestClientID is the GitHub App the tests' webhook sweeper signs as.
const replicasTestClientID = "Iv1.replicas"

// dispatchStart is one Dispatch publisher start under test: the settings it resolved, the
// client, the buffer that holds the client's log lines, and the overrides a failure names.
type dispatchStart struct {
	boot      bootConfig
	client    *bus.Client
	logged    *cmdtest.LockedBuffer
	overrides map[string]string
}

// connectDispatchStream is the Dispatch server's publisher connect for the settings overrides
// names, exactly as main makes it on the servers at urls: ConnectOwningStream with serverClient
// over dispatchStreamOptions, with no option of the test's own but extra, which a caller passes
// to hand the client a clock it drives itself (bus.WithClock) where main leaves it the wall
// clock. It returns the start it made (dispatchStart).
// The client returns before it has connected, and its first connection reconciles the stream and
// only then starts its keeper (bus.WithConnectInBackground), so a caller waits for what it expects
// itself (dispatchStart.awaitReconciled, dispatchStart.awaitHookFailure). The helper sets the
// default logger to record the client's lines until the test ends: a test that calls it must not
// run in parallel.
func connectDispatchStream(t *testing.T, urls []string, overrides map[string]string, extra ...bus.ConnectOption) dispatchStart {
	t.Helper()
	env := bootEnvironment(overrides)
	boot, err := resolveBootConfig(env)
	if err != nil {
		t.Fatalf("resolveBootConfig(%v): %v", overrides, err)
	}
	logged := &cmdtest.LockedBuffer{}
	logtest.SetDefault(t, slog.NewJSONHandler(logged, nil))
	client, err := bus.ConnectOwningStream(urls, serverClient(append(dispatchStreamOptions(boot, env), extra...)...)...)
	if err != nil {
		t.Fatalf("connect Dispatch's publisher for %v: %v", overrides, err)
	}
	t.Cleanup(func() { bustest.Close(t, client) })
	return dispatchStart{boot: boot, client: client, logged: logged, overrides: overrides}
}

// awaitReconciled waits through the shared wait until the stream holds what the start's
// settings ask of it (reconciledFor), and fails the test when it does not. The wait is for this
// client's own reconcile, not for a stream that already looks reconciled: it ends only once the
// client's hooks have covered a connection event (bus.Client.HookedConnectionEvents). An earlier
// start of the same test can leave the stream holding what the settings ask of it, and a wait
// that ended on that alone would let a check that the next start wrote nothing pass before this
// start had written at all.
//
// A failed run of the reconnect hooks does not end the wait, because the bus runs those hooks
// again. The wait reads no log at all. Its failure message names how long the hooks have been due
// and the error of the last run that failed (bus.Client.HookStall).
func (s dispatchStart) awaitReconciled(t *testing.T) {
	t.Helper()
	reconciled := reconciledFor(s.boot)
	testwait.Until(t, fmt.Sprintf("Dispatch's start for %v to reconcile the stream", s.overrides), func() (string, bool) {
		if !s.client.Connected() || s.client.HookedConnectionEvents() < 1 {
			return fmt.Sprintf("hooked events %d", s.client.HookedConnectionEvents()), false
		}
		info, err := s.client.JS().StreamInfo(bus.Stream)
		if err == nil && reconciled(info.Config) {
			return "", true
		}
		stall, hookErr := s.client.HookStall()
		return fmt.Sprintf("%+v (%v), reconnect hooks due for %s, last failed run %v", info, err, stall, hookErr), false
	})
}

// awaitHookFailure is the first failed run of the start's reconnect hooks that the Dispatch
// publisher logs (reconcileFailure), for a start whose settings the servers refuse. It leaves the
// client open and the caller decides what follows: a caller that wants no further reconcile closes
// the client, a caller that repairs what the start failed on waits for the reconcile the bus runs
// after its backoff (awaitReconciled). It fails the test when the start's hooks run through
// instead; a publisher that never logs such a failure fails at the shared wait's end, naming the
// hooked events it last read.
func (s dispatchStart) awaitHookFailure(t *testing.T) hookFailure {
	t.Helper()
	var failure hookFailure
	testwait.Until(t, fmt.Sprintf("Dispatch's start for %v to log a failed run of its reconnect hooks", s.overrides), func() (string, bool) {
		if failure = reconcileFailure(s.logged.String()); failure.Message != "" {
			return "", true
		}
		if s.client.HookedConnectionEvents() >= 1 {
			t.Fatalf("Dispatch's start for %v ran its reconnect hooks through, so the servers accepted the start, want them to refuse it", s.overrides)
		}
		return fmt.Sprintf("hooked events %d", s.client.HookedConnectionEvents()), false
	})
	return failure
}

// startDispatchStream is the Dispatch server's publisher connect for the settings overrides names,
// built exactly as main builds it, with extra over it (connectDispatchStream), waited for through
// the shared wait until this client's own first connection has reconciled the stream to what the
// settings ask of it (dispatchStart.awaitReconciled). It sets the default logger
// (connectDispatchStream), so a test that calls it must not run in parallel.
func startDispatchStream(t *testing.T, urls []string, overrides map[string]string, extra ...bus.ConnectOption) *bus.Client {
	t.Helper()
	start := connectDispatchStream(t, urls, overrides, extra...)
	start.awaitReconciled(t)
	return start.client
}

// reconciledFor reports whether a stream holds what a Dispatch start with boot's settings writes:
// every subject this binary compiles in, at least boot's copies, at least its byte limit when it
// sets one, the binary's age, duplicate window and DiscardNew, and an id.
func reconciledFor(boot bootConfig) func(natsgo.StreamConfig) bool {
	return func(c natsgo.StreamConfig) bool {
		for _, subject := range bus.StreamSubjects() {
			if !slices.Contains(c.Subjects, subject) {
				return false
			}
		}
		return c.Replicas >= boot.NATSReplicas && (boot.StreamMaxBytes == 0 || c.MaxBytes >= boot.StreamMaxBytes) &&
			c.MaxAge == contracts.DeliveryDuplicateWindow && c.Duplicates == contracts.DeliveryDuplicateWindow &&
			c.Discard == natsgo.DiscardNew && c.Metadata[bus.StreamIDMetadataKey] != ""
	}
}

// retriedHookFailure and uncoveredHookFailure are the two messages the bus logs a failed run of
// the reconnect hooks under, by how long the hooks have been uncovered (internal/bus,
// bus.RepairGrace): the WARN under that grace, the ERROR at or past it.
const (
	retriedHookFailure   = "envoy nats reconnect hook failed; it is retried"
	uncoveredHookFailure = "envoy nats reconnect hooks have not succeeded"
)

// hookFailure is one failed run of the Dispatch publisher's reconnect hooks, as the bus logged it:
// the message the bus chose for it, and the error of the run. The zero value says that the
// publisher logged no such run (Message == "").
type hookFailure struct {
	Message string
	Error   string
}

// reconcileFailure is the first failed run of the reconnect hooks that the Dispatch publisher
// logged in logs, JSON lines, and the zero hookFailure when it logged none. Both messages are
// read, so that a failure a slow start logs past the grace is read as well, and the Message a
// caller gets back names which of the two the bus chose.
func reconcileFailure(logs string) hookFailure {
	for line := range strings.Lines(logs) {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["name"] != "dispatch" {
			continue
		}
		switch message := record["msg"]; message {
		case retriedHookFailure, uncoveredHookFailure:
			return hookFailure{Message: fmt.Sprint(message), Error: fmt.Sprint(record["error"])}
		}
	}
	return hookFailure{}
}

// startDispatchNATS is all the Dispatch server's boot writes to NATS for overrides when it holds
// an App key: the publisher's connect (startDispatchStream, which fails the test itself when it
// does not reconcile) and the webhook sweeper's state bucket (webhookSweeper's Open, which the
// first sweep calls), opened at the settings' replica count. The error it returns is the sweeper's.
func startDispatchNATS(t *testing.T, urls []string, overrides map[string]string) error {
	t.Helper()
	client := startDispatchStream(t, urls, overrides)
	_, pemText := githubapptest.Key(t)
	app := &auth.AppConfig{ClientID: replicasTestClientID, PEM: pemText}
	// The store only carries the GitHub rate limits a send reads; nothing here sends, and the
	// bucket this opens reads none, so an empty one is every database this needs.
	sweeper, err := webhookSweeper(client, &store.Store{}, app, "", resolveWith(t, overrides).NATSReplicas)
	if err != nil {
		return err
	}
	_, err = sweeper.Open(t.Context())
	return err
}

// A Dispatch start whose first reconcile fails on a cluster with no quorum waits that failure out:
// the bus runs the reconnect hooks again, the start's own log line for the first failure is the
// retried WARN, and an operator who starts the missing members again finds the start's stream at
// the three copies the settings ask of it, once the cluster is ready again. Two of three members
// are stopped, since nats-server v2.15.1 places a three-copy stream with one member down (one
// offline peer assigned) and the start would succeed. Every step runs on the test's own
// goroutine, because testnats.Server.Start, testnats.Server.Stop and testnats.Cluster.AwaitReady
// call t.Fatal, which Go forbids off the test goroutine.
func TestADispatchStartWaitsOutAReconcileFailureTheBusRetries(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault).
	cluster := testnats.StartCluster(t, 3)
	cluster.Servers[1].Stop(t)
	cluster.Servers[2].Stop(t)
	overrides := map[string]string{"ENVOY_NATS_REPLICAS": "3"}

	start := connectDispatchStream(t, cluster.URLs(), overrides)
	failure := start.awaitHookFailure(t)
	if failure.Message != retriedHookFailure {
		t.Fatalf("the start's first logged failure of the reconnect hooks is %q (%s), want the retried %q", failure.Message, failure.Error, retriedHookFailure)
	}
	cluster.Servers[1].Start(t)
	cluster.Servers[2].Start(t)
	cluster.AwaitReady(t)
	start.awaitReconciled(t)

	testnats.AwaitStreamCopies(t, cluster.JetStream(t), bus.Stream, 3)
}

// On an existing three-copy stream with a byte limit, DiscardOld and an id, a Dispatch start whose
// stream differs in its subjects, its age or its duplicate window keeps the three copies, the byte
// limit and the id, and corrects the subjects, the age, the window and discard, to DiscardNew: with
// the deployment's settings (ENVOY_NATS_REPLICAS=3 and the byte limit), and with both settings
// unset, a replica count of 1 and no byte limit, which neither lowers the copies nor drops the limit.
func TestADispatchStartKeepsAThreeCopyStreamsCopiesByteLimitAndIDAsItCorrectsTheStream(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault).
	for _, drift := range bustest.StreamDrifts() {
		for _, settings := range []struct {
			name      string
			overrides map[string]string
		}{
			{"the deployment's settings", map[string]string{"ENVOY_NATS_REPLICAS": "3", "ENVOY_STREAM_MAX_BYTES": strconv.Itoa(bustest.StreamMaxBytes)}},
			{"the settings unset", nil},
		} {
			t.Run(drift.Name+"/"+settings.name, func(t *testing.T) {
				cluster := testnats.StartCluster(t, 3)
				js := cluster.JetStream(t)
				deployed := bustest.DeployedThreeCopyStream(drift.Change)
				cluster.CreateStream(t, deployed)
				before := testnats.AwaitSettledStream(t, js, bus.Stream).Config

				startDispatchStream(t, cluster.URLs(), settings.overrides)

				bustest.RequireKeptThreeCopyStream(t, before, testnats.AwaitSettledStream(t, js, bus.Stream))
			})
		}
	}
}

// `redeliver-webhooks` with ENVOY_NATS_REPLICAS=3 raises the R1 webhook redelivery bucket an
// earlier sweep at the default created to three copies, as the server's sweeper does.
func TestRedeliverWebhooksRaisesAnR1RedeliveryBucketToTheReplicaSetting(t *testing.T) {
	t.Parallel()
	cluster := testnats.StartCluster(t, 3)
	conn := testnats.Connect(t, cluster.URL())
	t.Cleanup(conn.Close)
	// The earlier sweep's own start creates the bucket, through a context bounded by the ten
	// seconds every production caller opens its own with (bus.RequestTimeout,
	// cmd/listener/brokerauth.go:152). That bound is a per-send hang guard and not a budget for
	// one request: a create or a lookup the server leaves unanswered is sent again rather than
	// waited out (bus.EnsureKeyValue), so a clustered create that is merely slow is answered on
	// the send it is answered on, and what a step that draws no answer at all costs is counted in
	// that function's doc rather than here. The command below connects for itself and looks the
	// bucket up on whichever member answers it, so every member answers for the bucket first.
	//
	// The create is the step a slow or unsettled cluster fails this row at, and its error alone
	// says nothing of why: `open envoy_webhook_redelivery KV bucket: context deadline exceeded`
	// in a CI log names neither how long the call waited nor what the members were doing while
	// it did. So the failure prints the wall time of this one call, which tells a refusal the
	// server answered at once from a wait that ran out, and every member's captured server log,
	// where a meta leader election or a member that never came up is written down. A CI log is
	// read from its last line backwards, so the short line that says what failed belongs at the
	// end, and three logs of hundreds of lines each belong in blocks a reader can tell apart
	// rather than inside that one line. A passing run prints none of it.
	buckets, err := conn.JetStream(natsgo.MaxWait(bus.RequestTimeout))
	if err != nil {
		t.Fatalf("open the JetStream context the earlier sweep's start uses: %v", err)
	}
	start := time.Now()
	_, err = redeliver.OpenState(t.Context(), conn, buckets, 1)
	elapsed := time.Since(start)
	if err != nil {
		for i, server := range cluster.Servers {
			t.Logf("member %s logged:\n%s", cluster.Name(i), server.Log())
		}
		t.Fatalf("create the R1 redelivery bucket: %v (the call took %s)", err, elapsed.Round(time.Millisecond))
	}
	cluster.AwaitBucket(t, bus.WebhookRedeliveryBucket)
	js := cluster.JetStream(t)
	stream := "KV_" + bus.WebhookRedeliveryBucket
	if info := testnats.AwaitSettledStream(t, js, stream); testnats.Copies(info.Cluster) != 1 {
		t.Fatalf("the redelivery bucket has %d copies, want 1", testnats.Copies(info.Cluster))
	}
	key, pemText := githubapptest.Key(t)
	github := githubapptest.NewWebhook(t, &key.PublicKey, replicasTestClientID, nil)
	// redeliver-webhooks opens the database that holds the GitHub rate limits every process obeys.
	database := storetest.Open(t)

	var stdout, stderr bytes.Buffer
	code := runSubcommand(context.Background(), []string{"redeliver-webhooks", "--since", "1h", "--dry-run"}, envGetter(map[string]string{
		"NATS_URLS":                cluster.URL(),
		"DATABASE_URL":             database.Pool.Config().ConnString(),
		"XDG_CONFIG_HOME":          t.TempDir(),
		"DISPATCH_APP_CLIENT_ID":   replicasTestClientID,
		"DISPATCH_APP_PEM_B64":     pemText,
		"DISPATCH_GITHUB_API_BASE": github.URL(),
		"ENVOY_NATS_REPLICAS":      "3",
	}), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("redeliver-webhooks --dry-run: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if info := testnats.AwaitSettledStream(t, js, stream); info.Config.Replicas != 3 || testnats.Copies(info.Cluster) != 3 {
		t.Fatalf("the redelivery bucket has replicas %d with %d copies, want 3", info.Config.Replicas, testnats.Copies(info.Cluster))
	}
}

// On a single server with the default settings, the Dispatch server's start creates the stream and
// the webhook redelivery bucket at one copy, and its next start creates and updates nothing.
func TestADefaultDispatchStartOnASingleServerCreatesEverythingAtOneCopyAndTheNextWritesNothing(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault).
	_, url := testnats.Start(t)
	streams := []string{bus.Stream, "KV_" + bus.WebhookRedeliveryBucket}

	bustest.RequireFirstStartAtOneCopyAndNoSecondWrite(t, url, streams, func() {
		t.Helper()
		if err := startDispatchNATS(t, []string{url}, nil); err != nil {
			t.Fatalf("start the Dispatch server's NATS work: %v", err)
		}
	})
}

// keeperPeriod is the keeper's period in this row, set through main's interval override
// (bus.WithKeepInterval), since the bus's own clock (bus.WithClock) lands with the rest of the
// Envoy module. A pass runs every keeperPeriod, so the restore comes within one period of the
// lowering, and the wait for it is the shared wait.
const keeperPeriod = 250 * time.Millisecond

// The Dispatch server's publisher, started as main starts it, keeps the stream once its
// background reconcile has run: an operator who lowers the stream to one copy and half the byte
// limit finds both back at the deployment's settings on the keeper's next pass.
func TestTheDispatchPublishersKeeperRestoresALoweredStream(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault).
	cluster := testnats.StartCluster(t, 3)
	js := cluster.JetStream(t)
	startDispatchStream(t, cluster.URLs(), map[string]string{"ENVOY_NATS_REPLICAS": "3", "ENVOY_STREAM_MAX_BYTES": strconv.Itoa(bustest.StreamMaxBytes)}, bus.WithKeepInterval(keeperPeriod))
	lowered := testnats.AwaitSettledStream(t, js, bus.Stream).Config
	lowered.Replicas = 1
	lowered.MaxBytes = bustest.StreamMaxBytes / 2
	// The lowering is sent once, on a JetStream context whose wait for the answer is the test
	// binary's deadline (testnats.MaxWait), as Cluster.CreateStream sends a create: the stream's
	// leader answers it, which on a loaded machine can outlast jetStreamWait, the bound of each
	// request of cluster.JetStream's context.
	operatorConn := testnats.Connect(t, cluster.URL())
	t.Cleanup(operatorConn.Close)
	operator, err := operatorConn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("open the operator's JetStream context: %v", err)
	}
	if _, err := operator.UpdateStream(&lowered); err != nil {
		t.Fatalf("operator lowering: %v", err)
	}
	testwait.Until(t, "the keeper's pass to restore the lowered stream to the deployment's settings", func() (string, bool) {
		info, err := js.StreamInfo(bus.Stream)
		if err != nil {
			return err.Error(), false
		}
		return fmt.Sprintf("%+v", info.Config), info.Config.Replicas == 3 && info.Config.MaxBytes == bustest.StreamMaxBytes
	})
	if info := testnats.AwaitSettledStream(t, js, bus.Stream); testnats.Copies(info.Cluster) != 3 {
		t.Fatalf("the restored stream has %d copies, want 3", testnats.Copies(info.Cluster))
	}
}
