package buckets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// heldBuckets is a buckets hold row's surrounding: holdtest's whole stack (the broker's real hold
// routes on a test Postgres, the callout cluster, the real auth service reading the row, the hub's
// Kubernetes objects with the chart's render), the Operator the command reaches the hub through,
// and the env that hands both to the command.
type heldBuckets struct {
	t      *testing.T
	s      *holdtest.Stack
	op     *writerhold.Operator
	e      env
	report string
}

// stackDisks is writerhold.Disks over holdtest's disks, by the disk's name.
type stackDisks struct{ *holdtest.Disks }

func (d stackDisks) DiskID(_ context.Context, disk writerhold.DiskRef) (uint64, error) {
	return d.ID(disk.Name)
}

func newHeldBuckets(t *testing.T) *heldBuckets {
	t.Helper()
	t.Setenv(bus.ReplicasEnvVar, "3")
	s := holdtest.Start(t)
	op := &writerhold.Operator{
		Hub: writerhold.KubeHub{Client: s.Kube.Client, Namespace: holdtest.Namespace, StatefulSet: holdtest.StatefulSet,
			ConfigMap: holdtest.ConfigMap, Forward: s.Kube},
		Disks:        stackDisks{holdtest.NewDisks(len(s.Cluster.Servers))},
		URLs:         s.Cluster.URLs(),
		Connect:      s.ConnectOperator,
		Probe:        s.DialProbe,
		RestartLimit: 3 * time.Minute,
	}
	h := &heldBuckets{t: t, s: s, op: op}
	h.e = env{
		records: func(context.Context, writerhold.HubFlags, bool) (*holdrecord.Client, error) {
			return s.Broker.Client(t), nil
		},
		hub:         func(context.Context, writerhold.HubFlags) (*writerhold.Operator, error) { return op, nil },
		now:         time.Now,
		restoreRuns: 2,
	}
	// The accepted minters, none, and a minter report of now with no finding.
	_, err := s.Kube.Client.CoreV1().ConfigMaps(writerhold.AcceptedMintersNamespace).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: writerhold.AcceptedMintersConfigMap, Namespace: writerhold.AcceptedMintersNamespace},
		Data:       map[string]string{writerhold.AcceptedMintersKey: "[]"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h.report = filepath.Join(t.TempDir(), "minters.json")
	report, _ := json.Marshal(map[string]any{"run_at": time.Now().UTC().Format(time.RFC3339), "findings": []any{}, "unknown": []any{}})
	if err := os.WriteFile(h.report, report, 0o600); err != nil {
		t.Fatal(err)
	}
	s.SeedProtectedStreams(t)
	return h
}

// run runs `envoy-infra buckets <args>` with the hold's flags, which the row's env stands in for.
func (h *heldBuckets) run(args ...string) (int, string, string) {
	h.t.Helper()
	full := append(append([]string{}, args...), "--broker-url", "https://broker.holdtest", "--token-file", "/dev/null",
		"--verify-keys", "AAAA", "--signing-key-secret", "projects/holdtest/secrets/"+writerhold.SigningKeySecret,
		"--minters-report", h.report, "--probe-token-file", "/dev/null", "--nats", h.s.Cluster.URL())
	if args[0] != "backup" {
		full = append(full, "--reason", "the row's "+args[0])
	}
	code, stdout, stderr := runCommand(h.t, h.e, full...)
	h.t.Logf("envoy-infra buckets %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout, stderr)
	return code, stdout, stderr
}

// row reads the hold row.
func (h *heldBuckets) row() holdrecord.Row {
	h.t.Helper()
	row, err := h.s.Records.Row(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return row
}

// release ends the row's hold as `hold release` does, after awaiting stream leaders.
func (h *heldBuckets) release() {
	h.t.Helper()
	row := h.row()
	if row.Phase != natsgrant.OperatorPhaseHeld {
		h.t.Fatalf("the row is %s, not held", row.Phase)
	}
	h.s.Release(h.t, row.HoldID)
	h.s.AwaitStreamLeaders(h.t, protected())
}

// protected are the protected streams the hold's preflight reads.
func protected() []string {
	names := []string{bus.Stream}
	for _, bucket := range holdtest.ProtectedBuckets() {
		names = append(names, "KV_"+bucket)
	}
	return names
}

// writer is a listener of machine that writes key after key into bucket until stopped, as a
// listener does, reconnecting; it records each write the hub acknowledged, and when the hub refused
// its reconnect.
type writer struct {
	nc      *natsgo.Conn
	refused chan struct{}
	once    sync.Once
	acked   atomic.Int64
	lastAck atomic.Int64 // unix nano of the latest acknowledged write
	// refusedAt is when the hub first refused the writer's reconnect, unix nano.
	refusedAt atomic.Int64
	stop      chan struct{}
	finished  chan struct{}
}

func (h *heldBuckets) startWriter(machine, name, bucket string) *writer {
	h.t.Helper()
	w := &writer{refused: make(chan struct{}), stop: make(chan struct{}), finished: make(chan struct{})}
	nc, err := natsgo.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(machine), natsgo.Name(name),
		natsgo.MaxReconnects(-1), natsgo.ReconnectWait(100*time.Millisecond), natsgo.Timeout(10*time.Second),
		natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
			if errors.Is(err, natsgo.ErrAuthorization) {
				w.once.Do(func() { w.refusedAt.Store(time.Now().UnixNano()); close(w.refused) })
			}
		}))
	if err != nil {
		h.t.Fatalf("the writer %s was refused before the hold: %v", name, err)
	}
	w.nc = nc
	h.t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		h.t.Fatal(err)
	}
	go func() {
		defer close(w.finished)
		for i := 0; ; i++ {
			select {
			case <-w.stop:
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if _, err := js.Publish(ctx, "$KV."+bucket+"."+name+"-"+fmt.Sprint(i), []byte("written")); err == nil {
				w.acked.Add(1)
				w.lastAck.Store(time.Now().UnixNano())
			}
			cancel()
			time.Sleep(50 * time.Millisecond)
		}
	}()
	testwait.Eventually(h.t, "the writer's first acknowledged write", func() bool { return w.acked.Load() > 0 })
	return w
}

func (w *writer) end() {
	close(w.stop)
	<-w.finished
}

// An acknowledged write of the writer's after its server's restart disconnected it, and the hub
// refused its reconnect, is a write the bucket's change can lose: the change runs after hold set's
// report, which comes after every server's restart, so the writer's last ack must precede its
// refusal. A write acknowledged before its server restarted lands before the change and is the
// command's to replace, as a recreate replaces every key.
func (h *heldBuckets) noAckAfter(w *writer, since time.Time) {
	h.t.Helper()
	if last := time.Unix(0, w.lastAck.Load()); last.After(since) {
		h.t.Fatalf("the writer's write was acknowledged at %s, after the hub refused its reconnect at %s", last.Format(time.RFC3339Nano), since.Format(time.RFC3339Nano))
	}
}

// refusedTime is when the hub first refused the writer's reconnect.
func (w *writer) refusedTime() time.Time { return time.Unix(0, w.refusedAt.Load()) }

// INFRA-4094's first bypass, a writer on a machine nobody listed: a listener connected before the
// hold is disconnected by its server's restart, its reconnect is refused, and `buckets recreate`
// loses no acknowledged write, since none is acknowledged from the hold's move to held to its
// release. The control is the same recreate's change with nothing held (the change alone, on the
// operator's connection, beside the same listener): the listener's writes are acknowledged by the
// new bucket throughout, so a writer nobody named gets past a command that holds nothing.
func TestHoldBypassUnlistedMachine(t *testing.T) {
	h := newHeldBuckets(t)
	w := h.startWriter(holdtest.ListenerMachines[2], "envoy-listener", bus.SessionBucket)

	code, stdout, _ := h.run("recreate", "--bucket", bus.SessionBucket)
	if code != 0 || !strings.Contains(stdout, "buckets recreate done") {
		t.Fatalf("buckets recreate = %d\n%s", code, stdout)
	}
	select {
	case <-w.refused:
	case <-time.After(30 * time.Second):
		t.Fatal("the listener's reconnect during the hold was not refused")
	}
	h.noAckAfter(w, w.refusedTime())
	acked := w.acked.Load()
	time.Sleep(time.Second)
	if w.acked.Load() != acked {
		t.Fatal("the listener's writes were acknowledged during the hold")
	}
	w.end()
	h.release()

	// The control: the change with nothing held, beside a running listener.
	w = h.startWriter(holdtest.ListenerMachines[2], "envoy-listener", bus.SessionBucket)
	nc, err := h.s.ConnectOperator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	before := w.acked.Load()
	// Under the released grant the operator cannot delete; the change runs as the test's own
	// operator under a hold of no steps (HoldWithoutTheSteps), which restarts nothing.
	id := h.s.Prepare(t, holdrecord.WorkflowBucketsRecreate)
	h.s.HoldWithoutTheSteps(t, id)
	nc.Close()
	nc, err = h.s.ConnectOperator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := recreate(context.Background(), nc, &bytes.Buffer{}, []string{bus.SessionBucket}, 3); err != nil {
		t.Fatalf("control: the change with nothing held: %v", err)
	}
	testwait.Eventually(t, "the listener's writes acknowledged by the new bucket", func() bool { return w.acked.Load() > before+3 })
	t.Logf("control: with nothing held, the listener's writes kept being acknowledged through the recreate (%d acks)", w.acked.Load()-before)
	w.end()
}

// INFRA-4094's second bypass, a renamed binary: a listener whose connection is named /tmp/worker,
// which the old fence's process-name filter missed, is disconnected by the hold's restart and its
// reconnect refused, as any client but the operator is. The control is that it is a writer: before
// the hold its writes are acknowledged.
func TestHoldBypassRenamedBinary(t *testing.T) {
	h := newHeldBuckets(t)
	w := h.startWriter(holdtest.ListenerMachines[1], "/tmp/worker", bus.SessionBucket)
	t.Logf("control: the renamed writer's writes are acknowledged before the hold (%d)", w.acked.Load())

	code, stdout, _ := h.run("recreate", "--bucket", bus.SessionBucket)
	if code != 0 {
		t.Fatalf("buckets recreate = %d\n%s", code, stdout)
	}
	select {
	case <-w.refused:
	case <-time.After(30 * time.Second):
		t.Fatal("the renamed writer's reconnect during the hold was not refused")
	}
	h.noAckAfter(w, w.refusedTime())
	w.end()
}

// INFRA-4094's third bypass, a writer that starts after the proof: during the hold, after its
// report, a listener's connect is refused, so it creates and writes nothing until the release; then
// it connects and writes, which is the control that it is a writer.
func TestHoldBypassWriterAfterTheProof(t *testing.T) {
	h := newHeldBuckets(t)
	code, stdout, _ := h.run("recreate", "--bucket", bus.RoleClaimBucket)
	if code != 0 {
		t.Fatalf("buckets recreate = %d\n%s", code, stdout)
	}
	for _, machine := range holdtest.ListenerMachines {
		nc, err := natsgo.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(machine), natsgo.NoReconnect(), natsgo.Timeout(10*time.Second))
		if err == nil {
			nc.Close()
			t.Fatalf("listener %s connected during the hold", machine)
		}
		if !errors.Is(err, natsgo.ErrAuthorization) {
			t.Fatalf("listener %s's connect during the hold failed for another reason: %v", machine, err)
		}
	}
	h.release()
	w := h.startWriter(holdtest.ListenerMachines[0], "envoy-listener", bus.RoleClaimBucket)
	t.Logf("control: after the release the listener writes (%d acks)", w.acked.Load())
	w.end()
}

// backupNow backs up buckets under the released row and returns the backup's directory.
func (h *heldBuckets) backupNow(buckets ...string) string {
	h.t.Helper()
	dir := filepath.Join(h.t.TempDir(), "backup")
	args := []string{"backup", "--to", dir}
	for _, bucket := range buckets {
		args = append(args, "--bucket", bucket)
	}
	if code, stdout, stderr := h.run(args...); code != 0 {
		h.t.Fatalf("buckets backup = %d\n%s%s", code, stdout, stderr)
	}
	return dir
}

// INFRA-4094's fourth bypass, an unheld restore: `buckets restore --from <backup>` runs inside its
// hold, and a listener started during it is refused; the restore completes with the backup's
// messages. The control is that the listener is a writer: after the release it writes.
func TestHoldBypassUnheldRestore(t *testing.T) {
	h := newHeldBuckets(t)
	w := h.startWriter(holdtest.ListenerMachines[0], "envoy-listener", bus.SessionBucket)
	w.end()
	w.nc.Close()
	dir := h.backupNow(bus.SessionBucket)

	var during atomic.Bool
	refusedDuring := make(chan error, 1)
	op := *h.op
	h.e.hub = func(context.Context, writerhold.HubFlags) (*writerhold.Operator, error) {
		o := op
		connect := o.Connect
		o.Connect = func(ctx context.Context) (*natsgo.Conn, error) {
			if during.Load() {
				_, err := natsgo.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(holdtest.ListenerMachines[1]), natsgo.NoReconnect(), natsgo.Timeout(10*time.Second))
				select {
				case refusedDuring <- err:
				default:
				}
			}
			return connect(ctx)
		}
		return &o, nil
	}
	// Once hold set has run, every further operator connect is the restore's own; a listener
	// connects beside the first of them.
	go func() {
		testwait.Eventually(t, "the row held", func() bool { return h.row().Phase == natsgrant.OperatorPhaseHeld })
		during.Store(true)
	}()
	code, stdout, _ := h.run("restore", "--bucket", bus.SessionBucket, "--from", dir)
	if code != 0 || !strings.Contains(stdout, "restored "+bus.SessionBucket) {
		t.Fatalf("buckets restore = %d\n%s", code, stdout)
	}
	select {
	case err := <-refusedDuring:
		if !errors.Is(err, natsgo.ErrAuthorization) {
			t.Fatalf("a listener that connected during the restore was not refused: %v", err)
		}
	default:
		t.Fatal("no listener connected during the restore")
	}
	h.release()
	w = h.startWriter(holdtest.ListenerMachines[1], "envoy-listener", bus.SessionBucket)
	t.Logf("control: after the release the listener writes (%d acks)", w.acked.Load())
	w.end()
}

// Backup and restore through jsm.go: `buckets backup` runs under exactly
// Operator(OperatorPhaseReleased) and `buckets restore --from` under exactly
// Operator(OperatorPhaseHeld), each on one connection, and both succeed with the backup's every
// message back. The control is a user holding the held grant without $JS.SNAPSHOT.RESTORE.KV_<B>.*:
// its restore is refused, naming the bucket.
func TestHoldBackupAndRestoreThroughJsm(t *testing.T) {
	h := newHeldBuckets(t)
	w := h.startWriter(holdtest.ListenerMachines[0], "envoy-listener", bus.InterestBucket)
	w.end()
	w.nc.Close()
	acked := w.acked.Load()

	var connects atomic.Int64
	op := *h.op
	h.e.hub = func(context.Context, writerhold.HubFlags) (*writerhold.Operator, error) {
		o := op
		connect := o.Connect
		o.Connect = func(ctx context.Context) (*natsgo.Conn, error) { connects.Add(1); return connect(ctx) }
		return &o, nil
	}
	dir := h.backupNow(bus.InterestBucket)
	if connects.Load() != 1 {
		t.Fatalf("the backup made %d connections, want one", connects.Load())
	}
	m, err := ReadManifest(dir, []string{bus.InterestBucket})
	if err != nil || m.Buckets[bus.InterestBucket].Messages != uint64(acked) {
		t.Fatalf("the backup's manifest: %v %+v, want %d messages", err, m, acked)
	}
	code, stdout, _ := h.run("restore", "--bucket", bus.InterestBucket, "--from", dir)
	if code != 0 {
		t.Fatalf("buckets restore = %d\n%s", code, stdout)
	}
	nc, err := h.s.ConnectOperator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	stream, err := js.Stream(context.Background(), "KV_"+bus.InterestBucket)
	if err != nil || stream.CachedInfo().State.Msgs != uint64(acked) {
		t.Fatalf("the restored stream: %v, want %d messages", err, acked)
	}
	h.release()

	// The control: on a server of the test's own, an operator user holding the held grant restores
	// the backup; the same user without the restore's chunk subject is refused, and its run names
	// the bucket.
	restoreAs := func(publish []string) error {
		seed, public := testnats.User(t)
		everything := core.Rules{Allow: []string{">"}}
		_, watcher := testnats.User(t)
		config, err := core.Authorization{Users: []core.User{
			{Name: "operator", NKey: public, Publish: &core.Rules{Allow: publish}, Subscribe: &core.Rules{Allow: []string{natsgrant.OperatorInbox + ".>"}}, HasSeed: true},
			{Name: "watcher", NKey: watcher, Publish: &everything, Subscribe: &everything},
		}, NoAuthUser: "watcher"}.Render(nil)
		if err != nil {
			t.Fatal(err)
		}
		// Three members, as the backup's stream has three copies.
		uri := testnats.StartClusterConfig(t, 3, config).Servers[0].URL()
		nc, err := testnats.ConnectSeed(t, uri, seed, natsgo.CustomInboxPrefix(natsgrant.OperatorInbox))
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		_, _, err = restoreAll(context.Background(), nc, &bytes.Buffer{}, dir, m, []string{bus.InterestBucket}, 1)
		return err
	}
	held := natsgrant.Operator(natsgrant.OperatorPhaseHeld).Publish
	if err := restoreAs(held); err != nil {
		t.Fatalf("control: the restore under exactly the held grant, on a server of the test's own: %v", err)
	}
	var less []string
	for _, s := range held {
		if s != "$JS.SNAPSHOT.RESTORE.KV_"+bus.InterestBucket+".*" {
			less = append(less, s)
		}
	}
	err = restoreAs(less)
	if err == nil || !strings.Contains(err.Error(), bus.InterestBucket) {
		t.Fatalf("control: the restore without $JS.SNAPSHOT.RESTORE.KV_%s.* = %v, want refused naming the bucket", bus.InterestBucket, err)
	}
	t.Logf("control: without the chunk subject the restore is refused: %v", err)
}

// cutDialer dials as nats.go does and closes a connection once it has sent the restore's second
// chunk ($JS.SNAPSHOT.RESTORE.<stream>.<n>), cuts times in all: a restore cut part way.
type cutDialer struct {
	cuts atomic.Int32
	// logf records each cut, the test's own log.
	logf func(format string, args ...any)
}

func (d *cutDialer) Dial(network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).Dial(network, address)
	if err != nil {
		return nil, err
	}
	return &cutConn{Conn: conn, d: d}, nil
}

type cutConn struct {
	net.Conn
	d      *cutDialer
	chunks int
	// cut says this connection is cut: each later write fails, and spends no further cut.
	cut bool
}

func (c *cutConn) Write(p []byte) (int, error) {
	if c.cut {
		return 0, net.ErrClosed
	}
	c.chunks += bytes.Count(p, []byte("PUB $JS.SNAPSHOT.RESTORE."))
	if c.chunks >= 2 && c.d.cuts.Load() > 0 {
		c.cut = true
		left := c.d.cuts.Add(-1)
		if c.d.logf != nil {
			c.d.logf("cut a connection after %d restore chunks (%d cuts left)", c.chunks, left)
		}
		_ = c.Conn.Close()
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}

// bigBucket writes keys of 4 KiB each into bucket as a listener, enough for a restore of several
// 64 KiB chunks, and returns how many were acknowledged.
func (h *heldBuckets) bigBucket(bucket string, keys int) uint64 {
	h.t.Helper()
	nc, err := natsgo.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(holdtest.ListenerMachines[0]), natsgo.Timeout(10*time.Second))
	if err != nil {
		h.t.Fatal(err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	value := make([]byte, 4096)
	for i := range keys {
		// Random bytes, so the archive does not compress to one chunk.
		if _, err := rand.Read(value); err != nil {
			h.t.Fatal(err)
		}
		if _, err := js.Publish(context.Background(), fmt.Sprintf("$KV.%s.k%d", bucket, i), value); err != nil {
			h.t.Fatalf("write key %d: %v", i, err)
		}
	}
	return uint64(keys)
}

// The restore's one rerun: a restore cut part way (its connection closed after its second chunk)
// is run again from the first chunk once the cut's leftover is gone, and completes with every
// message. A second cut stops the command, naming the bucket and both runs' errors, and leaves no
// stream. The control is a restore with no rerun, which a cut leaves with the bucket missing.
func TestHoldTheRestoresOneRerun(t *testing.T) {
	h := newHeldBuckets(t)
	keys := h.bigBucket(bus.KnownSessionBucket, 120)
	dir := h.backupNow(bus.KnownSessionBucket)
	m, err := ReadManifest(dir, []string{bus.KnownSessionBucket})
	if err != nil || m.Buckets[bus.KnownSessionBucket].Messages != keys {
		t.Fatalf("the backup: %v %+v, want %d messages", err, m, keys)
	}
	cut := &cutDialer{logf: t.Logf}
	op := *h.op
	op.Connect = func(context.Context) (*natsgo.Conn, error) {
		return natsgo.Connect(h.s.Cluster.URL(), append(h.s.OperatorCredential(), natsgo.SetCustomDialer(cut),
			natsgo.MaxReconnects(-1), natsgo.ReconnectWait(100*time.Millisecond), natsgo.Timeout(10*time.Second))...)
	}
	h.e.hub = func(context.Context, writerhold.HubFlags) (*writerhold.Operator, error) { return &op, nil }
	stream := "KV_" + bus.KnownSessionBucket
	msgs := func() (uint64, bool) {
		nc, err := h.s.ConnectOperator(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		js, _ := jetstream.New(nc)
		s, err := js.Stream(context.Background(), stream)
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return 0, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return s.CachedInfo().State.Msgs, true
	}

	// One cut: the rerun completes with every message.
	cut.cuts.Store(1)
	code, stdout, stderr := h.run("restore", "--bucket", bus.KnownSessionBucket, "--from", dir)
	if code != 0 || cut.cuts.Load() != 0 {
		t.Fatalf("a restore cut once = %d (cuts left %d)\n%s%s", code, cut.cuts.Load(), stdout, stderr)
	}
	if n, ok := msgs(); !ok || n != keys {
		t.Fatalf("after the rerun %s holds %d messages (present %t), want %d", stream, n, ok, keys)
	}
	h.release()

	// Two cuts: the command stops naming the bucket and both runs, and leaves no stream.
	cut.cuts.Store(2)
	code, stdout, stderr = h.run("restore", "--bucket", bus.KnownSessionBucket, "--from", dir)
	if code == 0 || !strings.Contains(stderr, "restore "+bus.KnownSessionBucket+" stopped") || !strings.Contains(stderr, "run 1") || !strings.Contains(stderr, "run 2") {
		t.Fatalf("a restore cut twice = %d (cuts left %d), want it stopped naming the bucket and both runs\n%s%s", code, cut.cuts.Load(), stdout, stderr)
	}
	if _, ok := msgs(); ok {
		t.Fatalf("a restore cut twice left %s", stream)
	}
	// A buckets restore is not Stoppable (holdrecord's table), so its intent stays pending and
	// release is refused until the restore's own rerun, under the same hold, completes it.
	id := h.row().HoldID
	code, stdout, stderr = h.run("restore", "--hold", id, "--bucket", bus.KnownSessionBucket, "--from", dir)
	if n, ok := msgs(); code != 0 || !ok || n != keys {
		t.Fatalf("the restore's rerun under the same hold = %d, %d messages (present %t), want %d\n%s%s", code, n, ok, keys, stdout, stderr)
	}
	h.release()

	// The control: no rerun, one cut: the bucket is left missing.
	h.e.restoreRuns = 1
	cut.cuts.Store(1)
	code, _, _ = h.run("restore", "--bucket", bus.KnownSessionBucket, "--from", dir)
	if _, ok := msgs(); code == 0 || ok {
		t.Fatalf("control: a restore with no rerun cut once = %d, present %t; want the bucket missing", code, ok)
	}
}

// An interrupted bucket restore (review H6): `buckets restore` killed after it removed its target,
// then run again under the same hold. Its reading records the target's absence as expected, the
// restore completes, and release succeeds. The control is the same absence in a buckets-create hold
// for another bucket, which stops the hold naming the absent stream.
func TestHoldPreflightAnInterruptedRestore(t *testing.T) {
	h := newHeldBuckets(t)
	ctx := context.Background()
	h.bigBucket(bus.SessionBucket, 5)
	dir := h.backupNow(bus.SessionBucket)
	m, err := ReadManifest(dir, []string{bus.SessionBucket})
	if err != nil {
		t.Fatal(err)
	}
	stream := "KV_" + bus.SessionBucket

	// The control: KV_envoy_sessions absent in a buckets-create hold for another bucket stops the
	// hold, naming it.
	id := h.s.Prepare(t, holdrecord.WorkflowBucketsRecreate)
	h.s.HoldWithoutTheSteps(t, id)
	nc, err := h.s.ConnectOperator(ctx)
	if err != nil {
		t.Fatal(err)
	}
	js, _ := jetstream.New(nc)
	for _, s := range []string{stream, "KV_" + bus.RoleClaimBucket} {
		if err := js.DeleteStream(ctx, s); err != nil {
			t.Fatalf("delete %s: %v", s, err)
		}
	}
	nc.Close()
	h.s.Release(t, id)
	code, _, stderr := h.run("create", "--bucket", bus.RoleClaimBucket)
	if code == 0 || !strings.Contains(stderr, stream) {
		t.Fatalf("control: a buckets-create hold beside an absent %s = %d, want it stopped naming it\n%s", stream, code, stderr)
	}
	row := h.row()
	if _, err := h.s.Records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseReleased, row.HoldID, "the control's end"); err != nil {
		t.Fatal(err)
	}
	// The control's other absence goes: KV_envoy_roles made again under a hold of no steps, so the
	// restore's reading below finds only its own target absent.
	id = h.s.Prepare(t, holdrecord.WorkflowBucketsCreate)
	h.s.HoldWithoutTheSteps(t, id)
	nc, err = h.s.ConnectOperator(ctx)
	if err != nil {
		t.Fatal(err)
	}
	js, _ = jetstream.New(nc)
	if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: 3}); err != nil {
		t.Fatalf("make %s again: %v", bus.RoleClaimBucket, err)
	}
	nc.Close()
	h.s.Release(t, id)

	// The restore, killed after it removed its target: its hold set, its intent, the target gone,
	// and no entry after.
	id = h.s.Prepare(t, holdrecord.WorkflowBucketsRestore)
	h.s.HoldWithoutTheSteps(t, id)
	if _, err := h.s.Records.Admit(ctx, id, holdrecord.Request{Command: CommandRestore}, map[string]string{"bucket." + bus.SessionBucket: bus.SessionBucket, "sha256." + bus.SessionBucket: m.Buckets[bus.SessionBucket].SHA256}); err != nil {
		t.Fatal(err)
	}
	// The rerun under the same hold, with the target absent: its reading records the absence as
	// expected, and it restores.
	code, stdout, stderr := h.run("restore", "--hold", id, "--bucket", bus.SessionBucket, "--from", dir)
	if code != 0 || !strings.Contains(stdout, "restored "+bus.SessionBucket) {
		t.Fatalf("the restore's rerun under the same hold = %d\n%s%s", code, stdout, stderr)
	}
	d, _, err := h.s.Records.Journal(ctx, id)
	if err != nil || !d.Complete {
		t.Fatalf("the rerun did not complete the record: %v %+v", err, d)
	}
	h.s.Release(t, id)
}
