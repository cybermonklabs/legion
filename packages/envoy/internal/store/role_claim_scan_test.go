package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// On 2026-11-22 the webhook listener's start began the role claim scan through a Service that
// still pointed at the VM hub. The Service's switch to the clustered hub reset the connection one
// second in, nats.go reconnected to the clustered hub a second later, and no further entry came:
// the scan's idle bound (kvwatch.ScanIdleTimeout) ended it after 4,577 of the bucket's 17,454
// entries, and the listener exited. A run that spans a reconnect is not shown to have read the
// whole bucket, complete or not: after the reconnect either the idle bound fires first, or nats.go
// makes the ordered consumer again at the next stream sequence on whatever server the connection
// now reaches, where that sequence need not name the same entry. So such a run is made again
// (scanRoleClaims). The first case is that morning: the forwarder in front of the client points at
// server A and then at server B, whose buckets differ in three claims, and the claims Open keeps
// must be B's alone, never one only A holds that the first run read before the switch. The second
// is a reconnect to the same server, the third a reconnect that has to wait for the server's port.
// The fourth is a reconnect whose handshake the server does not answer for a while: nats.go holds
// the connection's lock through that handshake, so every read of the connection's status or count
// waits for it, and the scan must notice the reconnect within a second all the same.
func TestARoleClaimScanThatSpansAReconnectRunsAgain(t *testing.T) {
	const watchdog = 60 * time.Second

	t.Run("the forwarder switches from server A to server B mid-scan", func(t *testing.T) {
		t.Parallel()
		a, b := twoRoleScanServers(t)
		forwarder := startScanForwarder(t, a.address)
		conn := connectForScan(t, forwarder.url())
		probe := &scanProbe{}
		switched := make(chan struct{})
		result := openWithin(t, watchdog, conn, probe.seams(nil, func(run, claims int) {
			if run == 1 && claims == 2000 {
				before := conn.Stats().Reconnects
				forwarder.switchTo(b.address)
				close(switched)
				awaitDisconnected(t, conn, before)
			}
		}))
		requireClosed(t, switched, "the forwarder never switched to B: the first run did not deliver 2,000 of A's claims")
		if result.err != nil {
			t.Fatalf("Open over a switch from A to B mid-scan failed after %d runs: %v", probe.count(), result.err)
		}
		if runs := probe.count(); runs < 2 {
			t.Fatalf("Open returned after %d run of the scan, which spanned the switch; want two or more", runs)
		}
		if revision, held := result.registry.restoredRoleRevisions["a-only"]; held {
			t.Fatalf("Open restored the claim only A holds at revision %d: a run that spanned the switch, or its result, reached the start", revision)
		}
		requireRevisions(t, result.registry.restoredRoleRevisions, b.revisions, "B's")
	})

	t.Run("a forced reconnect to the same server mid-scan", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, roleBucket(roleScanEntries))
		conn := connectForScan(t, server.uri)
		probe := &scanProbe{}
		forced := make(chan struct{})
		result := openWithin(t, watchdog, conn, probe.seams(nil, func(run, claims int) {
			if run == 1 && claims == 2000 {
				if err := conn.ForceReconnect(); err != nil {
					t.Errorf("force a reconnect: %v", err)
				}
				close(forced)
			}
		}))
		requireClosed(t, forced, "the test never forced the reconnect: the first run did not deliver 2,000 claims")
		if result.err != nil {
			t.Fatalf("Open over a forced reconnect mid-scan failed after %d runs: %v", probe.count(), result.err)
		}
		if runs := probe.count(); runs != 2 {
			t.Fatalf("Open returned after %d runs of the scan; want 2: the one that spanned the reconnect and the one after it", runs)
		}
		requireRevisions(t, result.registry.restoredRoleRevisions, server.revisions, "the bucket's")
	})

	t.Run("the forwarder's port is closed for 15 s mid-scan", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, roleBucket(roleScanEntries))
		forwarder := startScanForwarder(t, server.address)
		conn := connectForScan(t, forwarder.url())
		probe := &scanProbe{}
		const closedFor = 15 * time.Second
		dropped := make(chan time.Time, 1)
		reopened := make(chan error, 1)
		result := openWithin(t, watchdog, conn, probe.seams(nil, func(run, claims int) {
			if run == 1 && claims == 2000 {
				before := conn.Stats().Reconnects
				forwarder.closePort()
				dropped <- time.Now()
				time.AfterFunc(closedFor, func() { reopened <- forwarder.openPort() })
				awaitDisconnected(t, conn, before)
			}
		}))
		var droppedAt time.Time
		select {
		case droppedAt = <-dropped:
		default:
			t.Fatalf("the forwarder never dropped the connection: the first run did not deliver 2,000 claims (Open: %v)", result.err)
		}
		if result.err != nil {
			t.Fatalf("Open over a connection that was down for %s mid-scan failed after %d runs: %v", closedFor, probe.count(), result.err)
		}
		if err := <-reopened; err != nil {
			t.Fatalf("the forwarder could not listen again: %v", err)
		}
		if waited := result.at.Sub(droppedAt); waited < closedFor {
			t.Fatalf("Open returned %s after the drop, before the forwarder listened again %s after it", waited, closedFor)
		}
		if runs := probe.count(); runs != 2 {
			t.Fatalf("Open returned after %d runs of the scan; want 2: the one that ended while disconnected and the one once connected", runs)
		}
		requireRevisions(t, result.registry.restoredRoleRevisions, server.revisions, "the bucket's")
	})

	t.Run("a reconnect whose handshake stalls mid-scan", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, roleBucket(roleScanEntries))
		hold := testnats.StartHandshakeHold(t, server.uri)
		t.Cleanup(hold.Release)
		conn := connectForScan(t, hold.URL())
		probe := &scanProbe{}
		// heldFor is how long the reconnect's handshake is held once its CONNECT reaches the
		// server: inside the connection's own Timeout (5 s, connectForScan), so the held attempt
		// is the one that connects, and past the second in which the scan must notice it.
		const heldFor = 3 * time.Second
		holds := make(chan (<-chan struct{}), 1)
		heldAt := make(chan time.Time, 1)
		ended := make(chan struct{})
		t.Cleanup(func() { close(ended) })
		go func() {
			var held <-chan struct{}
			select {
			case held = <-holds:
			case <-ended:
				return
			}
			select {
			case <-held:
			case <-ended:
				return
			}
			heldAt <- time.Now()
			select {
			case <-time.After(heldFor):
			case <-ended:
			}
			hold.Release()
		}()
		result := openWithin(t, watchdog, conn, probe.seams(nil, func(run, claims int) {
			if run == 1 && claims == 2000 {
				before := conn.Stats().Reconnects
				holds <- hold.Hold()
				awaitDisconnected(t, conn, before)
			}
		}))
		var held time.Time
		select {
		case held = <-heldAt:
		default:
			t.Fatalf("no reconnect's handshake reached the server under the hold (Open after %d runs: %v)", probe.count(), result.err)
		}
		noticed, seen := probe.noticedAt(1)
		if !seen {
			t.Fatalf("the scan never noticed the reconnect while the first run was in progress (Open after %d runs: %v)", probe.count(), result.err)
		}
		late := noticed.Sub(held)
		if late > time.Second {
			t.Fatalf("the scan noticed the reconnect %s after its handshake reached the server and stalled; want within 1s", late)
		}
		t.Logf("the scan noticed the reconnect %s after its handshake reached the server (negative: before it)", late)
		if result.err != nil {
			t.Fatalf("Open over a reconnect whose handshake stalled for %s mid-scan failed after %d runs: %v", heldFor, probe.count(), result.err)
		}
		if runs := probe.count(); runs != 2 {
			t.Fatalf("Open returned after %d runs of the scan; want 2: the one the reconnect spanned and the one after it", runs)
		}
		requireRevisions(t, result.registry.restoredRoleRevisions, server.revisions, "the bucket's")
	})
}

// A run that meets a reconnect is made again, and the scan that never ends cleanly must still end
// the start: at most four runs (roleClaimScanRuns), and a deadline 70 s after the first run started
// (roleClaimScanDeadline) for the reconnect path alone. Each case holds one bound and the line it
// must not cross. Runs that each span a reconnect stop at four; a reconnect in a slow run, or a wait
// for a connection that does not come back, ends at the deadline; a reconnect after the deadline
// ends the run in progress within a second. A run that meets no reconnect keeps main's bounds and
// no other: one that stalls fails on the idle bound after one run, and one that is merely slow,
// past the deadline, completes. A closed connection fails the start, and is not taken for a
// reconnect. The eighth case holds the deadline while a reconnect's handshake holds nats.go's
// connection lock, which the teardown of the run's watch needs: the start must fail at the
// deadline, not when the handshake lets the lock go.
func TestARoleClaimScanThatNeverCompletesFailsTheStart(t *testing.T) {
	const watchdog = 120 * time.Second
	// paced is the watch of the cases whose runs last past the deadline: one entry every 8 s, inside
	// the idle bound (kvwatch.ScanIdleTimeout, 10 s), so a run ends only by completing or by the
	// scan's own ending of it.
	paced := func(kv bus.KeyValue) bus.KeyValue {
		return bus.NewKeyValue(kv.Legacy(), pacedKV{KeyValue: kv.WatchHandle(), every: 8 * time.Second}, kv.Streams())
	}

	t.Run("every run spans a switch between two servers", func(t *testing.T) {
		t.Parallel()
		a, b := twoRoleScanServers(t)
		forwarder := startScanForwarder(t, a.address)
		conn := connectForScan(t, forwarder.url())
		probe := &scanProbe{}
		servers := []string{a.address, b.address}
		result := openWithin(t, watchdog, conn, probe.seams(nil, func(run, claims int) {
			if claims == 1 {
				before := conn.Stats().Reconnects
				forwarder.switchTo(servers[run%2])
				awaitDisconnected(t, conn, before)
			}
		}))
		requireScanFailure(t, result.err)
		if runs := probe.count(); runs != roleClaimScanRuns {
			t.Fatalf("Open failed after %d runs of the scan, each spanning a switch; want exactly %d: %v", runs, roleClaimScanRuns, result.err)
		}
	})

	t.Run("a reconnect in a run that no idle bound ends", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(20))
		conn := connectForScan(t, server.uri)
		force := idleTimer(t, func() { _ = conn.ForceReconnect() })
		probe := &scanProbe{onRun: func(int) { force.Reset(2 * time.Second) }}
		result := openWithin(t, watchdog, conn, probe.seams(paced, nil))
		requireDeadline(t, result, probe)
		if runs := probe.count(); runs >= roleClaimScanRuns {
			t.Fatalf("Open failed after %d runs; the deadline should have ended the scan in fewer than %d", runs, roleClaimScanRuns)
		}
	})

	t.Run("a scan that stalls with no reconnect", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(3000))
		conn := connectForScan(t, server.uri)
		probe := &scanProbe{}
		stalled := func(kv bus.KeyValue) bus.KeyValue {
			return bus.NewKeyValue(kv.Legacy(), earlyEndKV{KeyValue: kv.WatchHandle(), after: 2000}, kv.Streams())
		}
		result := openWithin(t, watchdog, conn, probe.seams(stalled, nil))
		if !errors.Is(result.err, kvwatch.ErrScanTimedOut) {
			t.Fatalf("Open over a scan that stalled after 2,000 entries returned %v; want the scan's %v, as on main", result.err, kvwatch.ErrScanTimedOut)
		}
		requireScanFailure(t, result.err)
		if runs := probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs of a scan that met no reconnect; want 1", runs)
		}
		idle := result.at.Sub(probe.lastClaimAt())
		if idle < kvwatch.ScanIdleTimeout-500*time.Millisecond || idle > kvwatch.ScanIdleTimeout+5*time.Second {
			t.Fatalf("Open failed %s after the scan's last entry; want about the idle bound, %s", idle, kvwatch.ScanIdleTimeout)
		}
	})

	t.Run("a long healthy scan past the deadline", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(11))
		conn := connectForScan(t, server.uri)
		probe := &scanProbe{}
		result := openWithin(t, watchdog, conn, probe.seams(paced, nil))
		if result.err != nil {
			t.Fatalf("Open over a slow scan that met no reconnect failed after %d runs: %v", probe.count(), result.err)
		}
		if runs := probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs of a scan that met no reconnect; want 1", runs)
		}
		if lasted := result.at.Sub(probe.firstRunAt()); lasted < roleClaimScanDeadline+5*time.Second {
			t.Fatalf("the scan lasted %s, not past the deadline (%s): this case tests nothing", lasted, roleClaimScanDeadline)
		}
		requireRevisions(t, result.registry.restoredRoleRevisions, server.revisions, "the bucket's")
	})

	t.Run("a closed connection", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(3000))
		conn := connectForScan(t, server.uri)
		probe := &scanProbe{}
		closed := make(chan time.Time, 1)
		result := openWithin(t, watchdog, conn, probe.seams(nil, func(run, claims int) {
			if run == 1 && claims == 2000 {
				closed <- time.Now()
				conn.Close()
			}
		}))
		var closedAt time.Time
		select {
		case closedAt = <-closed:
		default:
			t.Fatalf("the test never closed the connection: the first run did not deliver 2,000 claims (Open: %v)", result.err)
		}
		if runs := probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs on a connection closed during the first; a closed connection is no reconnect, want 1: %v", runs, result.err)
		}
		requireScanFailure(t, result.err)
		if !errors.Is(result.err, natsgo.ErrConnectionClosed) || !strings.Contains(result.err.Error(), "closed NATS connection") {
			t.Fatalf("Open's error does not name the closed connection: %v", result.err)
		}
		if took := result.at.Sub(closedAt); took > 15*time.Second {
			t.Fatalf("Open failed %s after the connection closed; want within 15s", took)
		}
	})

	t.Run("a wait for the connection past the deadline", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(3000))
		forwarder := startScanForwarder(t, server.address)
		conn := connectForScan(t, forwarder.url())
		probe := &scanProbe{}
		result := openWithin(t, watchdog, conn, probe.seams(nil, func(run, claims int) {
			if run == 1 && claims == 2000 {
				before := conn.Stats().Reconnects
				forwarder.closePort()
				awaitDisconnected(t, conn, before)
			}
		}))
		requireDeadline(t, result, probe)
		if runs := probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs while the connection never came back; want 1", runs)
		}
	})

	t.Run("a reconnect after the deadline", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(11))
		conn := connectForScan(t, server.uri)
		const forceAt = 80 * time.Second
		forced := make(chan time.Time, 1)
		force := idleTimer(t, func() {
			forced <- time.Now()
			_ = conn.ForceReconnect()
		})
		probe := &scanProbe{onRun: func(run int) {
			if run == 1 {
				force.Reset(forceAt)
			}
		}}
		result := openWithin(t, watchdog, conn, probe.seams(paced, nil))
		var forcedAt time.Time
		select {
		case forcedAt = <-forced:
		default:
			t.Fatalf("Open returned %s after the first run started, before the test forced the reconnect at %s: %v", result.at.Sub(probe.firstRunAt()), forceAt, result.err)
		}
		requireScanFailure(t, result.err)
		if !errors.Is(result.err, errRoleClaimScanLateReconnect) || !strings.Contains(result.err.Error(), "reconnected") {
			t.Fatalf("Open's error does not name the reconnect after the deadline: %v", result.err)
		}
		if took := result.at.Sub(forcedAt); took > 2*time.Second {
			t.Fatalf("Open failed %s after the reconnect past the deadline; want within 2s", took)
		}
		if runs := probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs; want 1", runs)
		}
	})

	t.Run("a deadline while a reconnect's handshake holds the connection", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(20))
		hold := testnats.StartHandshakeHold(t, server.uri)
		// A connect Timeout far past the hold's release, so the held attempt holds nats.go's lock
		// from the hold to its release; and the release registered after the connection's close, so
		// it runs first and the close does not wait that Timeout out.
		conn := connectForScan(t, hold.URL(), natsgo.Timeout(time.Minute))
		t.Cleanup(hold.Release)
		// The hold begins 8 s before the deadline, so the reconnect's handshake holds the lock across
		// it, and is released 10 s after it, which is when a scan that waits for its watch's
		// teardown, which needs that lock, would end the start.
		const holdAt, releaseAt = roleClaimScanDeadline - 8*time.Second, roleClaimScanDeadline + 10*time.Second
		heldAt := make(chan time.Time, 1)
		begin := idleTimer(t, func() {
			select {
			case <-hold.Hold():
				heldAt <- time.Now()
			case <-time.After(30 * time.Second):
			}
		})
		release := idleTimer(t, hold.Release)
		probe := &scanProbe{onRun: func(run int) {
			if run == 1 {
				begin.Reset(holdAt)
				release.Reset(releaseAt)
			}
		}}
		result := openWithin(t, watchdog, conn, probe.seams(paced, nil))
		var held time.Time
		select {
		case held = <-heldAt:
		default:
			t.Fatalf("no reconnect's handshake was held before Open returned %s after the first run started: %v", result.at.Sub(probe.firstRunAt()), result.err)
		}
		if since := held.Sub(probe.firstRunAt()); since >= roleClaimScanDeadline {
			t.Fatalf("the reconnect's handshake was held only %s after the first run started, past the deadline, so this run cannot show what the deadline does while a handshake holds the connection", since)
		}
		requireDeadline(t, result, probe)
		if late := result.at.Sub(probe.firstRunAt()) - roleClaimScanDeadline; late > time.Second {
			t.Fatalf("Open failed %s after the deadline while a reconnect's handshake held the connection; want within 1s", late)
		}
		// Open answered while the handshake still held the lock its watch's teardown needs.
		testnats.RequireLockHeld(t, conn)
		hold.Release()
		if runs := probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs; want 1", runs)
		}
	})
}

// A run whose watch cannot be made fails the start as a run that ended incomplete does, and its
// error names the role claim scan as every other failure of the scan does: on its own, the error
// of the watch's construction (a consumer create the server refused, say), which
// kvwatch.ScanExistingKeys returns as nats.go gave it, names nothing a reader of the listener's
// exit can trace to the scan.
func TestARoleClaimScanWhoseWatchCannotBeMadeNamesTheScan(t *testing.T) {
	server := startRoleScanServer(t, claimsOnly(1))
	conn := connectForScan(t, server.uri)
	probe := &scanProbe{}
	refused := errors.New("the consumer create was refused")
	unmade := func(kv bus.KeyValue) bus.KeyValue {
		return bus.NewKeyValue(kv.Legacy(), unmadeWatchKV{KeyValue: kv.WatchHandle(), err: refused}, kv.Streams())
	}
	result := openWithin(t, 60*time.Second, conn, probe.seams(unmade, nil))
	if !errors.Is(result.err, refused) {
		t.Fatalf("Open over a watch that could not be made returned %v; want the watch's own error, %v", result.err, refused)
	}
	requireScanFailure(t, result.err)
	if runs := probe.count(); runs != 1 {
		t.Fatalf("Open made %d runs of a scan whose watch could not be made, with no reconnect; want 1", runs)
	}
}

// unmadeWatchKV refuses every watch with err, as a server that refuses the watch's consumer create
// does.
type unmadeWatchKV struct {
	jetstream.KeyValue

	err error
}

func (k unmadeWatchKV) Watch(context.Context, string, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return nil, k.err
}

// withRoleScanSeams sets the role claim scan's test seams (roleScanSeams) for one Open.
func withRoleScanSeams(seams roleScanSeams) OpenOption {
	return func(o *openOpts) { o.roleScan = seams }
}

// roleScanEntries is the size of the role claim bucket of the cases that read a bucket the size of
// the deployment's (17,454 entries on 2026-11-22), so a run takes long enough to be switched mid-way.
const roleScanEntries = 20000

// roleEntry is one write a test makes to the role claim bucket: a claim of key, or a delete
// marker of it.
type roleEntry struct {
	key    string
	marker bool
}

// roleBucket is n writes to the role claim bucket, of keys role-00001 on, a claim and a delete
// marker by turns, so a scan streams past markers among the claims as it does in the deployment.
func roleBucket(n int) []roleEntry {
	entries := make([]roleEntry, n)
	for i := range entries {
		entries[i] = roleEntry{key: fmt.Sprintf("role-%05d", i+1), marker: i%2 == 1}
	}
	return entries
}

// claimsOnly is n claims, of keys role-00001 on, and no marker, so a count of the claims a run is
// handed is a count of its entries too.
func claimsOnly(n int) []roleEntry {
	entries := make([]roleEntry, n)
	for i := range entries {
		entries[i] = roleEntry{key: fmt.Sprintf("role-%05d", i+1)}
	}
	return entries
}

// roleScanServer is a NATS server whose role claim bucket a case scans: its client URL, its address
// for a forwarder, and the revision of each claim its bucket holds, which is what Open must restore
// from a scan of it.
type roleScanServer struct {
	uri       string
	address   string
	revisions map[string]uint64
}

// startRoleScanServer starts a NATS server with the interest and role claim buckets and writes
// entries to the role claim bucket in order. The writes are asynchronous publishes to the keys'
// subjects, a delete marker one with the KV-Operation header nats.go's Delete sends, so a bucket of
// tens of thousands is written in seconds; each write's acknowledgement gives its revision.
func startRoleScanServer(t *testing.T, entries []roleEntry) roleScanServer {
	t.Helper()
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	defer conn.Close()
	js, err := conn.JetStream(natsgo.PublishAsyncMaxPending(1024))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	for _, bucket := range []string{bus.InterestBucket, bus.RoleClaimBucket} {
		if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bucket, Storage: natsgo.FileStorage}); err != nil {
			t.Fatalf("create the %s bucket: %v", bucket, err)
		}
	}
	futures := make([]natsgo.PubAckFuture, len(entries))
	for i, entry := range entries {
		msg := natsgo.NewMsg("$KV." + bus.RoleClaimBucket + "." + entry.key)
		if entry.marker {
			msg.Header.Set("KV-Operation", "DEL")
		} else {
			msg.Data = []byte(`{"holder_session_id":"ses_` + entry.key + `"}`)
		}
		if futures[i], err = js.PublishMsgAsync(msg, natsgo.StallWait(time.Minute)); err != nil {
			t.Fatalf("write entry %d of the role claim bucket: %v", i+1, err)
		}
	}
	revisions := map[string]uint64{}
	for i, future := range futures {
		select {
		case ack := <-future.Ok():
			if entries[i].marker {
				delete(revisions, entries[i].key)
			} else {
				revisions[entries[i].key] = ack.Sequence
			}
		case err := <-future.Err():
			t.Fatalf("write entry %d of the role claim bucket: %v", i+1, err)
		}
	}
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("read the server's URL: %v", err)
	}
	return roleScanServer{uri: uri, address: server.Host, revisions: revisions}
}

// twoRoleScanServers starts servers A and B with the same role claim bucket but for three claims:
// one only A holds, written among the first thousand entries so a first run reads it early; one
// only B holds, at the same place in B's bucket; and B's first claim, which B writes once more, so
// its revision there is not A's.
func twoRoleScanServers(t *testing.T) (a, b roleScanServer) {
	t.Helper()
	entries := roleBucket(roleScanEntries)
	onA, onB := slices.Clone(entries), slices.Clone(entries)
	onA[500] = roleEntry{key: "a-only"}
	onB[500] = roleEntry{key: "b-only"}
	onB = append(onB, entries[0])
	a, b = startRoleScanServer(t, onA), startRoleScanServer(t, onB)
	if _, held := a.revisions["a-only"]; !held {
		t.Fatalf("A's bucket holds no a-only claim")
	}
	if a.revisions[entries[0].key] == b.revisions[entries[0].key] {
		t.Fatalf("%s is at revision %d on both servers; B's write of it again did not land", entries[0].key, a.revisions[entries[0].key])
	}
	return a, b
}

// connectForScan connects to uri as the listener's bus client does in what this test reads: it
// reconnects for ever, a second after a drop (bus's ReconnectWait), so a scan can meet a reconnect.
// options follow those, so a case replaces one. It closes when the test ends, before any server or
// forwarder it reaches.
func connectForScan(t *testing.T, uri string, options ...natsgo.Option) *natsgo.Conn {
	t.Helper()
	conn, err := natsgo.Connect(uri, append([]natsgo.Option{natsgo.Name(t.Name()), natsgo.Timeout(5 * time.Second),
		natsgo.MaxReconnects(-1), natsgo.ReconnectWait(time.Second)}, options...)...)
	if err != nil {
		t.Fatalf("connect to %s: %v", uri, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// awaitDisconnected waits until conn has seen the drop the test just made: it is no longer
// connected, or its count of reconnects has risen past before. It runs inside a run's entry
// callback, so the run goes on only once the connection knows of the drop, and cannot complete
// from what it had already read before the client noticed it.
func awaitDisconnected(t *testing.T, conn *natsgo.Conn, before uint64) {
	if !testwait.Holds(10*time.Second, 5*time.Millisecond, func() bool {
		return conn.Status() != natsgo.CONNECTED || conn.Stats().Reconnects > before
	}) {
		t.Errorf("the connection did not notice the drop within 10s")
	}
}

// idleTimer is a timer that runs fire once Reset arms it, and never after the test has ended. It
// is made on the test's goroutine, so a seam that arms it from a run's goroutine registers nothing
// with the test.
func idleTimer(t *testing.T, fire func()) *time.Timer {
	timer := time.AfterFunc(time.Hour, fire)
	timer.Stop()
	t.Cleanup(func() { timer.Stop() })
	return timer
}

// scanProbe counts the role claim scan's runs and records when the first started, when a run was
// last handed a claim and when the scan first noticed a reconnect in each run, through the scan's
// test seams, and runs onRun as each run starts.
type scanProbe struct {
	onRun func(run int)

	mu        sync.Mutex
	runs      int
	firstRun  time.Time
	lastClaim time.Time
	noticed   map[int]time.Time
}

// seams are the scan's test seams for one Open: watch wraps the bucket each run reads (nil reads
// it as it is), and handed runs at each claim a run is handed, after the probe records it.
func (p *scanProbe) seams(watch func(bus.KeyValue) bus.KeyValue, handed func(run, claims int)) roleScanSeams {
	return roleScanSeams{
		watch: watch,
		run: func(n int) {
			p.mu.Lock()
			p.runs++
			if n == 1 {
				p.firstRun = time.Now()
			}
			p.mu.Unlock()
			if p.onRun != nil {
				p.onRun(n)
			}
		},
		handed: func(run, claims int) {
			p.mu.Lock()
			p.lastClaim = time.Now()
			p.mu.Unlock()
			if handed != nil {
				handed(run, claims)
			}
		},
		noticed: func(run int) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.noticed == nil {
				p.noticed = map[int]time.Time{}
			}
			if _, seen := p.noticed[run]; !seen {
				p.noticed[run] = time.Now()
			}
		},
	}
}

// noticedAt is when the scan first noticed a reconnect while run was in progress, and whether it
// did.
func (p *scanProbe) noticedAt(run int) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	at, seen := p.noticed[run]
	return at, seen
}

func (p *scanProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs
}

func (p *scanProbe) firstRunAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firstRun
}

func (p *scanProbe) lastClaimAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastClaim
}

// openResult is what one Open returned, and when.
type openResult struct {
	registry *Registry
	err      error
	at       time.Time
}

// openWithin opens the registry on conn with seams and returns what Open returned, failing the
// test when Open has not returned within watchdog, the case's own bound. A registry Open returns
// after the watchdog is stopped all the same.
func openWithin(t *testing.T, watchdog time.Duration, conn *natsgo.Conn, seams roleScanSeams) openResult {
	t.Helper()
	done := make(chan openResult, 1)
	go func() {
		registry, err := Open(context.Background(), conn, WithReplicas(1), withRoleScanSeams(seams))
		done <- openResult{registry: registry, err: err, at: time.Now()}
	}()
	timer := time.NewTimer(watchdog)
	defer timer.Stop()
	select {
	case result := <-done:
		if result.registry != nil {
			t.Cleanup(result.registry.StopWatch)
		}
		return result
	case <-timer.C:
		go func() {
			if late := <-done; late.registry != nil {
				late.registry.StopWatch()
			}
		}()
		t.Fatalf("store.Open had not returned %s after it was called, the case's watchdog", watchdog)
		return openResult{}
	}
}

// requireClosed fails the test with why unless done is closed.
func requireClosed(t *testing.T, done <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Fatal(why)
	}
}

// requireScanFailure fails the test unless err is a failed start that names the role claim scan.
func requireScanFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Open returned no error")
	}
	if !strings.Contains(err.Error(), "role claim scan") {
		t.Fatalf("Open's error does not name the role claim scan: %v", err)
	}
}

// requireDeadline fails the test unless Open failed naming the scan's deadline, between the
// deadline and five seconds after it, counted from the first run's start.
func requireDeadline(t *testing.T, result openResult, probe *scanProbe) {
	t.Helper()
	requireScanFailure(t, result.err)
	if !errors.Is(result.err, errRoleClaimScanDeadline) || !strings.Contains(result.err.Error(), "deadline") {
		t.Fatalf("Open's error does not name the scan's deadline: %v", result.err)
	}
	if took := result.at.Sub(probe.firstRunAt()); took < roleClaimScanDeadline || took > roleClaimScanDeadline+5*time.Second {
		t.Fatalf("Open failed %s after the first run started; want between %s and %s", took, roleClaimScanDeadline, roleClaimScanDeadline+5*time.Second)
	}
}

// requireRevisions fails the test unless got is want, claim for claim and revision for revision;
// whose names want in the failure.
func requireRevisions(t *testing.T, got, want map[string]uint64, whose string) {
	t.Helper()
	if maps.Equal(got, want) {
		return
	}
	var wrong []string
	for key, revision := range got {
		if want[key] != revision {
			wrong = append(wrong, fmt.Sprintf("%s at %d, want %d", key, revision, want[key]))
		}
	}
	for key, revision := range want {
		if _, held := got[key]; !held {
			wrong = append(wrong, fmt.Sprintf("%s missing, want %d", key, revision))
		}
	}
	slices.Sort(wrong)
	if len(wrong) > 10 {
		wrong = append(wrong[:10], fmt.Sprintf("and %d more", len(wrong)-10))
	}
	t.Fatalf("Open restored %d claims, not %s %d:\n%s", len(got), whose, len(want), strings.Join(wrong, "\n"))
}

// scanForwarder is the TCP forwarder the role claim scan's cases put between their client and its
// NATS server, as the Service in front of the hub was on 2026-11-22. It relays each connection it
// accepts to the server it points at. switchTo points it at another server and drops every
// connection it relays, as the Service's switch reset them; closePort stops it listening and drops
// them, so a reconnect fails at TCP, and openPort listens again. Its address is a port the kernel
// chose at an address of its own (testport.LoopbackIP), held for the test's life by a reservation
// that never listens (testport.Reserve): a dial while the port is closed is refused, and a program
// that binds the port without SO_REUSEADDR meanwhile is refused too.
type scanForwarder struct {
	address string

	mu       sync.Mutex
	listener net.Listener
	target   string
	// generation moves on at each switch and close, so a connection accepted before one is closed
	// rather than joined to the server it was dialing.
	generation int
	relayed    map[net.Conn]struct{}
	ended      bool
}

// startScanForwarder starts a forwarder to target, a server's host and port, until the test ends.
func startScanForwarder(t *testing.T, target string) *scanForwarder {
	t.Helper()
	ip := testport.LoopbackIP().To4()
	address := &syscall.SockaddrInet4{}
	copy(address.Addr[:], ip)
	reservation, port, err := testport.Reserve(address)
	if err != nil {
		t.Fatalf("reserve the forwarder's port: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(reservation) })
	f := &scanForwarder{address: net.JoinHostPort(ip.String(), strconv.Itoa(port)), target: target, relayed: map[net.Conn]struct{}{}}
	if err := f.openPort(); err != nil {
		t.Fatalf("start the forwarder: %v", err)
	}
	t.Cleanup(f.end)
	return f
}

// url is the NATS URL of the forwarder's address.
func (f *scanForwarder) url() string { return "nats://" + f.address }

// openPort listens on the forwarder's address, beside its reservation. It listens no more once the
// test has ended.
func (f *scanForwarder) openPort() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ended || f.listener != nil {
		return nil
	}
	listener, err := net.Listen("tcp", f.address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", f.address, err)
	}
	f.listener = listener
	go f.serve(listener)
	return nil
}

// closePort stops listening, so a dial of the address is refused, and drops every connection.
func (f *scanForwarder) closePort() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listener != nil {
		_ = f.listener.Close()
		f.listener = nil
	}
	f.dropLocked()
}

// switchTo points the forwarder at target and drops every connection, so the client's reconnect
// reaches target.
func (f *scanForwarder) switchTo(target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.target = target
	f.dropLocked()
}

// end closes the forwarder for good, as the test ends.
func (f *scanForwarder) end() {
	f.closePort()
	f.mu.Lock()
	f.ended = true
	f.mu.Unlock()
}

// dropLocked closes every relayed connection and moves the generation on. f.mu is held.
func (f *scanForwarder) dropLocked() {
	f.generation++
	for conn := range f.relayed {
		_ = conn.Close()
		delete(f.relayed, conn)
	}
}

// serve joins each connection listener accepts to the server the forwarder points at.
func (f *scanForwarder) serve(listener net.Listener) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		target, generation := f.target, f.generation
		f.mu.Unlock()
		go f.join(client, target, generation)
	}
}

// join dials target for client and relays the two into each other, unless the forwarder has been
// switched or closed since client was accepted.
func (f *scanForwarder) join(client net.Conn, target string, generation int) {
	server, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	f.mu.Lock()
	if f.generation != generation {
		f.mu.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	f.relayed[client] = struct{}{}
	f.relayed[server] = struct{}{}
	f.mu.Unlock()
	go f.relay(server, client)
	go f.relay(client, server)
}

// relay copies src to dst until either ends, then closes both, which ends the other direction too.
func (f *scanForwarder) relay(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
	f.mu.Lock()
	delete(f.relayed, dst)
	delete(f.relayed, src)
	f.mu.Unlock()
}

// pacedKV hands each watch's entries on one every `every`, the first `every` after the watch was
// made, and the end of the existing keys, the nil entry, as soon as the entry before it: a bucket
// whose server delivers it slowly and steadily, so a run through it stays inside the scan's idle
// bound however long it lasts. It takes the real watch's entries as they arrive, so a reconnect a
// case forces does not stall it.
type pacedKV struct {
	jetstream.KeyValue

	every time.Duration
}

func (k pacedKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return nil, err
	}
	paced := &pacedWatcher{KeyWatcher: watcher, updates: make(chan jetstream.KeyValueEntry), stop: make(chan struct{})}
	taken := make(chan jetstream.KeyValueEntry, 1024)
	go func() {
		defer close(taken)
		for entry := range watcher.Updates() {
			select {
			case taken <- entry:
			case <-paced.stop:
				for range watcher.Updates() {
				}
				return
			}
		}
	}()
	go paced.hand(taken, k.every)
	return paced, nil
}

// pacedWatcher is the watcher pacedKV hands out. Its Stop stops the real watcher and closes its
// own updates, so the drain a scan leaves behind (bus.DrainWatcher) ends.
type pacedWatcher struct {
	jetstream.KeyWatcher

	updates  chan jetstream.KeyValueEntry
	stop     chan struct{}
	stopping sync.Once
}

func (w *pacedWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

func (w *pacedWatcher) Stop() error {
	w.stopping.Do(func() { close(w.stop) })
	return w.KeyWatcher.Stop()
}

// hand passes on what taken carries, each entry every after the one before it and the nil entry at
// once, until taken closes or the watcher stops.
func (w *pacedWatcher) hand(taken <-chan jetstream.KeyValueEntry, every time.Duration) {
	defer close(w.updates)
	for {
		var entry jetstream.KeyValueEntry
		select {
		case next, open := <-taken:
			if !open {
				return
			}
			entry = next
		case <-w.stop:
			return
		}
		if entry != nil {
			select {
			case <-time.After(every):
			case <-w.stop:
				return
			}
		}
		select {
		case w.updates <- entry:
		case <-w.stop:
			return
		}
	}
}
