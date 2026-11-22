//go:build twobuild

package consumernames

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// restoreDispatchSubject is a job subject of Dispatch's job intake (notifications.github.*.*.job.>,
// the durable delivery-jobs), the intake that binds with a GitHub App and no delivery settings; the
// delivery intake (delivery-events) waits for its settings row, which the row does not seed.
const restoreDispatchSubject = "notifications.github.acme.widgets.job.restore"

// restoreStartDispatch starts binary, an envoy-dispatch, on the database at databaseURL and the
// stack's cluster, connected as the stack's Dispatch entry (holdtest.DispatchMachines[0]) through
// NATS_SERVICE_TOKEN_FILE, serving on a loopback port the test holds, as cmd/dispatch's process rows
// start it (shutdown_test.go startDispatchProcessWith), and waits until it serves. It runs with a
// GitHub App of a throwaway key whose only API origin is a server of the test's that answers 404
// (cmd/dispatch nats_unreachable_test.go githubAppEnv): without an App the delivery intakes stay
// idle and never take their durables over. It is killed when the test ends if it still runs.
func restoreStartDispatch(t *testing.T, h *stack, binary, databaseURL string) *cmdtest.Process {
	t.Helper()
	var o nats.Options
	require.NoError(t, h.s.DispatchCredential(holdtest.DispatchMachines[0])(&o))
	home := t.TempDir()
	token := filepath.Join(home, "dispatch.token")
	require.NoError(t, os.WriteFile(token, []byte(o.Password), 0o600))
	github := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(github.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemText := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	held := cmdtest.HoldPort(t)
	cmd := exec.Command(binary)
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home,
		"DATABASE_URL=" + databaseURL,
		"DISPATCH_AGENT_TOKEN=row-token",
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + strconv.Itoa(held.Port),
		"DISPATCH_WEB_DIST=" + home,
		"DISPATCH_APP_CLIENT_ID=Iv1.restore",
		"DISPATCH_APP_ID=4242",
		"DISPATCH_APP_SLUG=restore",
		"DISPATCH_APP_PEM_B64=" + base64.StdEncoding.EncodeToString(pemText),
		"DISPATCH_GITHUB_API_BASE=" + github.URL,
		"ENVOY_MACHINE_ID=" + holdtest.DispatchMachines[0],
		"NATS_URLS=" + h.s.Cluster.URL(),
		bus.ServiceTokenFileEnvVar + "=" + token,
	}
	held.Pass(cmd, "DISPATCH_LISTEN_FD")
	process := cmdtest.Start(t, "the previous release's envoy-dispatch", cmd)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the previous release's envoy-dispatch on port %d:\n%s", held.Port, process.Output.String())
		}
	})
	process.WaitForOutput(t, `msg="dispatch: listening" addr=127.0.0.1:`+strconv.Itoa(held.Port))
	return process
}

// restoreDispatchAcked waits up to within until consumer name of the stream has acked every
// message through seq and holds none pending, and answers its info. It reads the info every 250 ms
// on the test's goroutine, so a run that times out names the last info it read.
func (rig *restoreRig) restoreDispatchAcked(name string, seq uint64, within time.Duration) *jetstream.ConsumerInfo {
	rig.t.Helper()
	for deadline := time.Now().Add(within); ; time.Sleep(250 * time.Millisecond) {
		info := rig.consumer(name)
		if info != nil && info.AckFloor.Stream >= seq && info.NumPending == 0 && info.NumAckPending == 0 {
			return info
		}
		if time.Now().After(deadline) {
			rig.t.Fatalf("%s never acked through %d (last info %+v)", name, seq, info)
		}
	}
}

// restoreDispatchDatabase makes the Dispatch store of a row whose previous envoy-dispatch boots on
// it: an empty database, registered as the stack's Dispatch store; the previous build migrates it
// at its first boot, and restoreDispatchSchema adds this tree's intake_consumers after.
func restoreDispatchDatabase(h *stack) (*pgxpool.Pool, string) {
	h.t.Helper()
	pool := h.database(familyDispatch)
	return pool, pool.Config().ConnString()
}

// restoreDispatchSchema adds this tree's intake_consumers to pool's database with its own
// migration (dispatchMigrations' last), after the previous build migrated the rest.
func restoreDispatchSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sql, err := os.ReadFile(filepath.FromSlash(dispatchMigrations[len(dispatchMigrations)-1]))
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), string(sql))
	require.NoError(t, err, "migrate the previous build's database with %s", dispatchMigrations[len(dispatchMigrations)-1])
}

// restoreKillNames are the sessions of the killed-restore half, each named for the point restore is
// killed at for it; restoreKillKept's convert was interrupted with F at a floor its start keeps.
var (
	restoreKillPrepare = map[string]string{"kill-planned": restorePlanned, "kill-deleted": "deleted",
		"kill-source": restoreSourceDeleted, "kill-kept": restoreKept}
	restoreKillFinish = map[string]string{"kill-target": "target", "kill-created": restoreCreated}
)

const restoreKillKept = "kill-kept"

// deliveredAll pulls consumer name as the listener, acking each, until every one of want arrived or
// within passed, and answers what arrived, sorted: a message the previous release pulled and did not
// ack comes back after its ack wait.
func (rig *restoreRig) deliveredAll(name string, want []uint64, within time.Duration) []uint64 {
	rig.t.Helper()
	ctx := context.Background()
	nc, js := rig.listener()
	defer nc.Close()
	consumer, err := js.Consumer(ctx, notifications, name)
	require.NoError(rig.t, err)
	var got []uint64
	for deadline := time.Now().Add(within); time.Now().Before(deadline); {
		batch, err := consumer.Fetch(100, jetstream.FetchMaxWait(2*time.Second))
		require.NoError(rig.t, err)
		for msg := range batch.Messages() {
			meta, err := msg.Metadata()
			require.NoError(rig.t, err)
			require.NoError(rig.t, msg.Ack())
			got = append(got, meta.Sequence.Stream)
		}
		if !slices.ContainsFunc(want, func(seq uint64) bool { return !slices.Contains(got, seq) }) {
			break
		}
	}
	slices.Sort(got)
	return slices.Compact(got)
}

// TestRestoreThenThePreviousBuildDeliversEveryMessage: restore killed after each of its
// phases ends, rerun, at the same target and start, and each delivers every message from its saved
// start; and after restore and release the previous release's listener (build-dispatch-build.sh
// --previous-release) adopts the restored fixed name at its start, and every message from that
// start arrives on it.
//
// The previous release's Dispatch boots on its own database at its own schema, with this tree's
// intake_consumers added after, as the window leaves it: after restore and release its take-over
// adopts the restored durable's start and acks every message from it.
func TestRestoreThenThePreviousBuildDeliversEveryMessage(t *testing.T) {
	t.Run("restore killed after each phase", func(t *testing.T) {
		rig := newRestoreRig(t)
		starts := map[string]uint64{}
		want := map[string][]uint64{}
		var conversions []restoreConversion
		logicals := append(slices.Collect(maps.Keys(restoreKillPrepare)), slices.Collect(maps.Keys(restoreKillFinish))...)
		slices.Sort(logicals)
		for _, logical := range logicals {
			subject := "notifications.agent." + logical
			seqs := rig.publish(subject, 3)
			fixed := sessionFixedPrefix + logical
			if logical == restoreKillKept {
				// F acked through the second message, its convert interrupted at the third: F's
				// floor + 1 is the start, so restore keeps F.
				rig.fixed(restoreFixedConfig(fixed, subject), 2)
				conversions = append(conversions, restoreConversion{fixed: fixed, start: seqs[2], interrupted: true})
				starts[logical], want[logical] = seqs[2], seqs[2:]
				continue
			}
			rig.fixed(restoreFixedConfig(fixed, subject), 1)
			conversions = append(conversions, restoreConversion{fixed: fixed, start: seqs[1]})
			starts[logical], want[logical] = seqs[1], seqs[1:]
		}
		rig.cutover(conversions...)
		rollback := rig.heldHold(holdrecord.WorkflowRollback)

		ended := restoreKilledAt(t, restoreKillPrepare)
		runs := 0
		for code := 1; code != 0; runs++ {
			require.Less(t, runs, len(restoreKillPrepare)+2, "restore --prepare never completed")
			code, _, _ = rig.restore(rollback, "--prepare")
		}
		require.Len(t, *ended, len(restoreKillPrepare), "restore --prepare was killed at each of its phases: %v", *ended)
		ended = restoreKilledAt(t, restoreKillFinish)
		runs = 0
		for code := 1; code != 0; runs++ {
			require.Less(t, runs, len(restoreKillFinish)+2, "restore --finish never completed")
			code, _, _ = rig.restore(rollback, "--finish")
		}
		require.Len(t, *ended, len(restoreKillFinish), "restore --finish was killed at each of its points: %v", *ended)
		require.True(t, rig.journal(rollback).Complete)

		for logical, start := range starts {
			fixed := sessionFixedPrefix + logical
			phase := restoreCreated
			if logical == restoreKillKept {
				phase = restoreKept
			}
			require.Equal(t, &selection.Restore{Target: fixed, Start: start, Phase: phase}, rig.record(familySession, logical).Restore,
				"%s ends at its target and start", logical)
			info := rig.consumer(fixed)
			require.NotNil(t, info, "%s's target is present", logical)
			if phase == restoreCreated {
				require.Equal(t, start, info.Config.OptStartSeq)
			} else {
				require.LessOrEqual(t, info.AckFloor.Stream+1, start, "the kept target passes over nothing from its start")
			}
		}
		require.NoError(t, rig.release(rollback))
		for logical, seqs := range want {
			require.Equal(t, seqs, rig.delivered(sessionFixedPrefix+logical), "%s delivers every message from its start", logical)
		}
	})

	t.Run("the previous release's listener", func(t *testing.T) {
		previous := mixedBuildPreviousRelease(t)
		h := newStack(t)
		h.mixedBuildProvision()
		rig := restoreRigOn(h)
		const session = "prev-a"
		subject := "notifications.agent." + session
		seqs := rig.publish(subject, 4)
		fixed := sessionFixedPrefix + session
		rig.fixed(restoreFixedConfig(fixed, subject), 2)
		rig.cutover(restoreConversion{fixed: fixed, start: seqs[2]})
		want := append(slices.Clone(seqs[2:]), rig.publish(subject, 2)...)
		rollback := rig.heldHold(holdrecord.WorkflowRollback)
		code, _, stderr := rig.restore(rollback, "--prepare")
		require.Zero(t, code, stderr)
		code, _, stderr = rig.restore(rollback, "--finish")
		require.Zero(t, code, stderr)
		require.NoError(t, rig.release(rollback))

		old := h.mixedBuildStartListener(previous.listener(), holdtest.ListenerMachines[1], "ENVOY_SESSION_CONSUMER_SWEEP_INTERVAL=24h")
		// The listener answers 503 "service starting" until its startup is done.
		status, answer := old.register(t, session)
		for deadline := time.Now().Add(3 * time.Minute); status == http.StatusServiceUnavailable && answer["error"] == "service starting" && time.Now().Before(deadline); {
			time.Sleep(500 * time.Millisecond)
			status, answer = old.register(t, session)
		}
		require.Equal(t, http.StatusOK, status, "the previous release (%s) registers %s: %v", previous.commit, session, answer)
		t.Logf("the previous release (%s) answered the registration of %s: %d %v", previous.commit, session, status, answer)
		info := rig.consumer(fixed)
		require.NotNil(t, info, "the previous release kept %s", fixed)
		t.Logf("%s after the registration: %s at %d, filters %v", fixed, info.Config.DeliverPolicy, info.Config.OptStartSeq, info.Config.FilterSubjects)
		require.Equal(t, seqs[2], info.Config.OptStartSeq, "the previous release adopted %s at its restored start, not a create of its own", fixed)
		old.stop(t)
		got := rig.deliveredAll(fixed, want, time.Minute)
		t.Logf("%s delivered %v; every message from %d is %v", fixed, got, seqs[2], want)
		for _, seq := range want {
			require.Contains(t, got, seq, "every message from the restored start arrives on %s", fixed)
		}
	})

	t.Run("the previous release's Dispatch", func(t *testing.T) {
		previous := mixedBuildPreviousRelease(t)
		h := newStack(t)
		h.mixedBuildProvision()
		pool, databaseURL := restoreDispatchDatabase(h)
		rig := &restoreRig{stack: h, pools: map[string]*pgxpool.Pool{familyDispatch: pool},
			flags: []string{"--dispatch-database-file", "/dev/null"}, replicas: 1}

		// The previous release's Dispatch migrates its database, takes its job intake's durable
		// over and acks its first two messages; then it stops, as a deploy stops it.
		first := restoreStartDispatch(t, h, previous.dispatch(), databaseURL)
		require.Eventually(t, func() bool { return rig.consumer(bus.DispatchJobDurable) != nil }, time.Minute, 250*time.Millisecond,
			"the previous release never created %s", bus.DispatchJobDurable)
		seqs := rig.publish(restoreDispatchSubject, 2)
		before := rig.restoreDispatchAcked(bus.DispatchJobDurable, seqs[1], time.Minute)
		first.Terminate(t)
		first.WaitExit(t, "SIGTERM")
		restoreDispatchSchema(t, pool)

		start := before.AckFloor.Stream + 1
		rig.cutover(restoreConversion{fixed: bus.DispatchJobDurable, start: start})
		later := rig.publish(restoreDispatchSubject, 2)
		rollback := rig.heldHold(holdrecord.WorkflowRollback)
		code, _, stderr := rig.restore(rollback, "--prepare")
		require.Zero(t, code, stderr)
		code, _, stderr = rig.restore(rollback, "--finish")
		require.Zero(t, code, stderr)
		restored := rig.consumer(bus.DispatchJobDurable)
		require.NotNil(t, restored, "restore created %s", bus.DispatchJobDurable)
		require.Equal(t, start, restored.Config.OptStartSeq)
		require.NoError(t, rig.release(rollback))

		// The previous release's take-over reads the restored durable's floor, deletes it and
		// creates it again at that floor + 1, then delivers every message from there and acks it.
		second := restoreStartDispatch(t, h, previous.dispatch(), databaseURL)
		after := rig.restoreDispatchAcked(bus.DispatchJobDurable, later[len(later)-1], time.Minute)
		t.Logf("the previous release (%s) took %s over at %s %d, deliver subject %q; it acked through %d with none pending",
			previous.commit, bus.DispatchJobDurable, after.Config.DeliverPolicy, after.Config.OptStartSeq, after.Config.DeliverSubject, after.AckFloor.Stream)
		require.Equal(t, jetstream.DeliverByStartSequencePolicy, after.Config.DeliverPolicy,
			"the previous release took %s over by its start, not by a native policy of its own", bus.DispatchJobDurable)
		require.LessOrEqual(t, after.Config.OptStartSeq, start, "the take-over starts no later than the restored start")
		require.GreaterOrEqual(t, after.Delivered.Stream, later[len(later)-1], "every message from the restored start was delivered")
		second.Terminate(t)
		second.WaitExit(t, "SIGTERM")
	})
}
