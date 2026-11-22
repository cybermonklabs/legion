package writerhold_test

// The writer hold's rows that run a command of `envoy-infra consumer-names` (t-7's), each by the
// name the hold's list gives it. They are package writerhold_test, beside the rows of package
// writerhold, so a check that counts the hold's rows by name over ./writerhold/ counts them, and one
// that counts the rows of package writerhold alone does not. Each runs the commands for real, each
// on holdtest's stack, through the caller-stack seams: writerhold.CommandOn (`envoy-infra hold`),
// writerhold.StreamsCommandOn (`envoy-infra streams`, with consumernames' census readers),
// consumernames.CommandOn (`envoy-infra consumer-names`) and buckets.CommandOn (`envoy-infra
// buckets`). Beside them, TestAKillAfterAWriteNeverAnswersIt is a row of the rows' own harness, the
// kill at a write to the broker's routes (killAt), and no row of the hold's list.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/buckets"
	"github.com/sjawhar/envoy/infra/consumernames"
	"github.com/sjawhar/envoy/infra/streams"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/kvrecords"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/natsgrant"
)

// t7Stack is a row's surrounding: holdtest's whole stack (the broker's hold routes on a test
// Postgres, the callout cluster of three, the auth service reading the row at every decision, the
// hub's Kubernetes objects with the chart's render), the Operator every command reaches the hub
// through, the disks behind its claims, the signing key's store keygen writes, the minter report a
// hold's preflight reads, and the family stores a row makes.
type t7Stack struct {
	t      *testing.T
	s      *holdtest.Stack
	op     *writerhold.Operator
	disks  *holdtest.Disks
	keys   *t7Keys
	report string

	mu sync.Mutex
	// dbs are the family stores by family, and files each one's URL in a file, as the census's
	// readers open a store (consumernames.RecordedFreshNames).
	dbs   map[string]*pgxpool.Pool
	files map[string]string
	// conns are the connections a row opened that are still open: every held command's re-read and
	// every hold set's report admit no connection but the command's own and the auth service's, so
	// each command closes them first (closeConns).
	conns []*nats.Conn
}

// t7Disks is writerhold.Disks over holdtest's disks, by the disk's name.
type t7Disks struct{ *holdtest.Disks }

func (d t7Disks) DiskID(_ context.Context, disk writerhold.DiskRef) (uint64, error) {
	return d.ID(disk.Name)
}

// t7Keys is the signing key's store in memory: Put adds the first version only, as the Secret
// Manager store does.
type t7Keys struct {
	mu       sync.Mutex
	versions map[string][][]byte
}

func (k *t7Keys) Put(_ context.Context, name string, value []byte) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.versions[name]) > 0 {
		return "", errors.New("the secret already holds version 1")
	}
	k.versions[name] = append(k.versions[name], append([]byte(nil), value...))
	return "1", nil
}

func (k *t7Keys) Current(_ context.Context, name string) (string, []byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v := k.versions[name]
	if len(v) == 0 {
		return "", nil, errors.New("no version")
	}
	return fmt.Sprint(len(v)), append([]byte(nil), v[len(v)-1]...), nil
}

// newT7Stack starts a row's stack, stopped when t ends, with the accepted minters' list empty and a
// minter report of now with no finding.
func newT7Stack(t *testing.T) *t7Stack {
	t.Helper()
	s := holdtest.Start(t)
	disks := holdtest.NewDisks(len(s.Cluster.Servers))
	op := &writerhold.Operator{
		Hub: writerhold.KubeHub{Client: s.Kube.Client, Namespace: holdtest.Namespace, StatefulSet: holdtest.StatefulSet,
			ConfigMap: holdtest.ConfigMap, Forward: s.Kube},
		Disks:        t7Disks{disks},
		URLs:         s.Cluster.URLs(),
		Connect:      s.ConnectOperator,
		Probe:        s.DialProbe,
		RestartLimit: 3 * time.Minute,
	}
	h := &t7Stack{t: t, s: s, op: op, disks: disks, keys: &t7Keys{versions: map[string][][]byte{}},
		dbs: map[string]*pgxpool.Pool{}, files: map[string]string{}}
	_, err := s.Kube.Client.CoreV1().ConfigMaps(writerhold.AcceptedMintersNamespace).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: writerhold.AcceptedMintersConfigMap, Namespace: writerhold.AcceptedMintersNamespace},
		Data:       map[string]string{writerhold.AcceptedMintersKey: "[]"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	h.report = filepath.Join(t.TempDir(), "minters.json")
	report := fmt.Sprintf(`{"run_at":%q,"findings":[],"unknown":[]}`, time.Now().UTC().Format(time.RFC3339))
	require.NoError(t, os.WriteFile(h.report, []byte(report), 0o600))
	return h
}

// t7HoldFlags are the flags every word of the hold, the streams and the buckets takes, whose values
// the seams do not read.
var t7HoldFlags = []string{"--broker-url", "https://broker.t7rows", "--token-file", "/dev/null",
	"--verify-keys", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "--signing-key-secret", "projects/t7rows/secrets/" + writerhold.SigningKeySecret}

// t7NameFlags are the flags every consumer-names command takes, and t7NameWriting those every one
// but status takes beside them, whose values consumernames.CommandOn does not read.
var (
	t7NameFlags = []string{"--broker-url", "https://broker.t7rows", "--token-file", "/dev/null",
		"--verify-keys", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "--nats", "nats://t7rows:4222"}
	t7NameWriting = []string{"--signing-key-secret", "projects/t7rows/secrets/" + writerhold.SigningKeySecret}
)

// withReason is args with a --reason when it names none.
func withReason(args []string) []string {
	if slices.Contains(args, "--reason") {
		return args
	}
	return append(args, "--reason", "the row's "+args[0])
}

// holdDeps are the stack's deps of `envoy-infra hold`.
func (h *t7Stack) holdDeps() writerhold.HoldDeps {
	return writerhold.HoldDeps{
		Records:  func(context.Context, bool) (*holdrecord.Client, error) { return h.s.Broker.Client(h.t), nil },
		Keys:     func(context.Context) (writerhold.KeyStore, error) { return h.keys, nil },
		Operator: func(context.Context) (*writerhold.Operator, error) { return h.op, nil },
	}
}

// holdOn runs `envoy-infra hold <args>` on the stack (writerhold.CommandOn) on ctx, with the hold's
// flags and, for prepare and set, the minter report, and for set and prove the hub's URLs and the
// probe's token, after closing the row's own connections; it answers the exit code and the output.
func (h *t7Stack) holdOn(ctx context.Context, args ...string) (int, string, string) {
	h.t.Helper()
	h.closeConns()
	full := append([]string{}, args...)
	if args[0] != "status" && args[0] != "keygen" && args[0] != "prove" {
		full = withReason(full)
	}
	full = append(full, t7HoldFlags...)
	if args[0] == "prepare" || args[0] == "set" {
		full = append(full, "--minters-report", h.report)
	}
	if args[0] == "set" || args[0] == "prove" {
		full = append(full, "--nats", h.s.Cluster.URL(), "--probe-token-file", "/dev/null")
	}
	var stdout, stderr bytes.Buffer
	code := writerhold.CommandOn(ctx, full, &stdout, &stderr, h.holdDeps())
	h.t.Logf("envoy-infra hold %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// hold is holdOn on a context that never ends.
func (h *t7Stack) hold(args ...string) (int, string, string) {
	h.t.Helper()
	return h.holdOn(context.Background(), args...)
}

// nameDeps are the stack's deps of `envoy-infra consumer-names`: the broker's routes, the operator's
// connection decided under the row's phase at its connect (through dialer when it is set), the
// family stores the row made, and the stack's Operator.
func (h *t7Stack) nameDeps(dialer nats.CustomDialer) consumernames.Deps {
	return consumernames.Deps{
		Records: func(context.Context, bool) (*holdrecord.Client, error) { return h.s.Broker.Client(h.t), nil },
		Connect: func(context.Context) (*nats.Conn, error) {
			return h.connectOperator("envoy-infra consumer-names", dialer)
		},
		Database: func(_ context.Context, family string) (consumernames.Database, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			db, ok := h.dbs[family]
			if !ok {
				return nil, fmt.Errorf("the row made no store of %s", family)
			}
			return db, nil
		},
		Operator: func(context.Context) (*writerhold.Operator, error) { return h.op, nil },
	}
}

// namesThrough runs `envoy-infra consumer-names <args>` on the stack (consumernames.CommandOn) on
// ctx, its connection through dialer when it is set, with its common flags, after closing the row's
// own connections.
func (h *t7Stack) namesThrough(ctx context.Context, dialer nats.CustomDialer, args ...string) (int, string, string) {
	h.t.Helper()
	h.closeConns()
	full := append(append([]string{}, args...), t7NameFlags...)
	if args[0] != "status" {
		full = append(full, t7NameWriting...)
	}
	var stdout, stderr bytes.Buffer
	code := consumernames.CommandOn(ctx, full, &stdout, &stderr, h.nameDeps(dialer))
	h.t.Logf("envoy-infra consumer-names %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// namesOn is namesThrough with no dialer.
func (h *t7Stack) namesOn(ctx context.Context, args ...string) (int, string, string) {
	h.t.Helper()
	return h.namesThrough(ctx, nil, args...)
}

// names is namesOn on a context that never ends.
func (h *t7Stack) names(args ...string) (int, string, string) {
	h.t.Helper()
	return h.namesOn(context.Background(), args...)
}

// censusReaders are the census's readers and writer as cmd/envoy-infra passes them:
// consumernames' reader of the recorded session consumers, of the recorded fresh names, and its
// writer of the daemon stores' stream ids.
func censusReaders() writerhold.CensusReaders {
	return writerhold.CensusReaders{Recorded: consumernames.RecordedSessionConsumers, FreshNames: consumernames.RecordedFreshNames,
		Stores: consumernames.ReconcileDaemonStreamIDs}
}

// streamsOn runs `envoy-infra streams <args>` on the stack (writerhold.StreamsCommandOn) on ctx,
// with readers, the hold's flags, the minter report, the hub's URLs, the probe's token and the
// stack's three copies, after closing the row's own connections.
func (h *t7Stack) streamsOn(ctx context.Context, readers writerhold.CensusReaders, args ...string) (int, string, string) {
	h.t.Helper()
	return h.streamsThrough(ctx, h.op, readers, args...)
}

// streamsThrough is streamsOn with op as the command's reach of the hub.
func (h *t7Stack) streamsThrough(ctx context.Context, op *writerhold.Operator, readers writerhold.CensusReaders, args ...string) (int, string, string) {
	h.t.Helper()
	h.closeConns()
	full := append(withReason(append([]string{}, args...)), t7HoldFlags...)
	full = append(full, "--minters-report", h.report, "--nats", h.s.Cluster.URL(), "--probe-token-file", "/dev/null", "--replicas", "3")
	deps := writerhold.StreamsDeps{
		Records:       func(context.Context, bool) (*holdrecord.Client, error) { return h.s.Broker.Client(h.t), nil },
		Operator:      func(context.Context) (*writerhold.Operator, error) { return op, nil },
		CensusReaders: readers,
	}
	var stdout, stderr bytes.Buffer
	code := writerhold.StreamsCommandOn(ctx, full, &stdout, &stderr, deps)
	h.t.Logf("envoy-infra streams %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// operatorThrough is a copy of the stack's Operator whose operator connections go through dialer.
func (h *t7Stack) operatorThrough(dialer nats.CustomDialer) *writerhold.Operator {
	op := *h.op
	op.Connect = func(context.Context) (*nats.Conn, error) { return h.connectOperator("envoy-infra hold", dialer) }
	return &op
}

// connectOperator connects to the hub as the operator under name, reconnecting as the commands'
// own connections do, through dialer when it is set.
func (h *t7Stack) connectOperator(name string, dialer nats.CustomDialer) (*nats.Conn, error) {
	opts := append(h.s.OperatorCredential(), nats.Name(name), nats.MaxReconnects(-1),
		nats.ReconnectWait(100*time.Millisecond), nats.Timeout(10*time.Second))
	if dialer != nil {
		opts = append(opts, nats.SetCustomDialer(dialer))
	}
	return nats.Connect(h.s.Cluster.URL(), opts...)
}

// streamsWith is streamsOn on a context that never ends.
func (h *t7Stack) streamsWith(readers writerhold.CensusReaders, args ...string) (int, string, string) {
	h.t.Helper()
	return h.streamsOn(context.Background(), readers, args...)
}

// streams is streamsWith consumernames' readers.
func (h *t7Stack) streams(args ...string) (int, string, string) {
	h.t.Helper()
	return h.streamsWith(censusReaders(), args...)
}

// censusArgs are `streams census --hold id`'s words over families, with the Dispatch store's file
// when the row made one.
func (h *t7Stack) censusArgs(id string, families ...string) []string {
	args := []string{"census", "--hold", id}
	for _, family := range families {
		args = append(args, "--family", family)
	}
	if file := h.file("dispatch"); file != "" {
		args = append(args, "--dispatch-database-file", file)
	}
	return args
}

// census runs `streams census --hold id` over families (censusArgs) until a run prints CLEAN, three
// runs at most: a run that removes something ends in its stop, and the next one is clean. It
// requires one clean run, and answers every run's output.
func (h *t7Stack) census(id string, families ...string) string {
	h.t.Helper()
	args := h.censusArgs(id, families...)
	var out strings.Builder
	for range 3 {
		code, stdout, stderr := h.streams(args...)
		out.WriteString(stdout + stderr)
		if code == 0 && slices.Contains(strings.Split(strings.TrimSpace(stdout), "\n"), "CLEAN") {
			return out.String()
		}
	}
	h.t.Fatalf("no streams census of hold %s was clean in three runs:\n%s", id, out.String())
	return ""
}

// bucketsRun runs `envoy-infra buckets <args>` on the stack (buckets.CommandOn), with the hold's
// flags, the minter report, the hub's URLs and the probe's token, after closing the row's own
// connections.
func (h *t7Stack) bucketsRun(args ...string) (int, string, string) {
	h.t.Helper()
	h.closeConns()
	full := append(withReason(append([]string{}, args...)), t7HoldFlags...)
	full = append(full, "--minters-report", h.report, "--nats", h.s.Cluster.URL(), "--probe-token-file", "/dev/null")
	deps := buckets.Deps{
		Records:  func(context.Context, bool) (*holdrecord.Client, error) { return h.s.Broker.Client(h.t), nil },
		Operator: func(context.Context) (*writerhold.Operator, error) { return h.op, nil },
	}
	var stdout, stderr bytes.Buffer
	code := buckets.CommandOn(context.Background(), full, &stdout, &stderr, deps)
	h.t.Logf("envoy-infra buckets %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// closeConns closes every connection the row opened that is still open.
func (h *t7Stack) closeConns() {
	h.mu.Lock()
	conns := h.conns
	h.conns = nil
	h.mu.Unlock()
	for _, nc := range conns {
		nc.Close()
	}
}

// track keeps nc to close before the next command.
func (h *t7Stack) track(nc *nats.Conn) *nats.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns = append(h.conns, nc)
	return nc
}

// operator connects as the operator, decided under the row's phase now, closed before the next
// command.
func (h *t7Stack) operator() (*nats.Conn, jetstream.JetStream) {
	h.t.Helper()
	nc, err := h.s.Connect(h.s.Cluster.URL(), h.s.OperatorCredential()...)
	require.NoError(h.t, err)
	js, err := jetstream.New(h.track(nc))
	require.NoError(h.t, err)
	return nc, js
}

// listener connects as today's listener on machine, closed before the next command.
func (h *t7Stack) listener(machine string) (*nats.Conn, jetstream.JetStream) {
	h.t.Helper()
	nc, err := h.s.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(machine), nats.Name("envoy-listener of today's build"))
	require.NoError(h.t, err)
	js, err := jetstream.New(h.track(nc))
	require.NoError(h.t, err)
	return nc, js
}

// listenerAdmitted reports whether a listener's connect is admitted now, and fails the row on any
// answer but an admission or the auth service's refusal.
func (h *t7Stack) listenerAdmitted() bool {
	h.t.Helper()
	nc, err := nats.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(holdtest.ListenerMachines[0]), nats.NoReconnect(), nats.Timeout(10*time.Second))
	if err == nil {
		nc.Close()
		return true
	}
	require.ErrorIs(h.t, err, nats.ErrAuthorization, "the listener's connect failed for another reason")
	return false
}

// row reads the hold row.
func (h *t7Stack) row() holdrecord.Row {
	h.t.Helper()
	row, err := h.s.Records.Row(context.Background())
	require.NoError(h.t, err)
	return row
}

// journal reads hold id's journal.
func (h *t7Stack) journal(id string) holdrecord.Done {
	h.t.Helper()
	d, _, err := h.s.Records.Journal(context.Background(), id)
	require.NoError(h.t, err)
	return d
}

// entries are hold id's journal entries, each "<command> <state>".
func (h *t7Stack) entries(id string) []string {
	h.t.Helper()
	var out []string
	for _, op := range h.journal(id).Ops {
		out = append(out, op.Command+" "+op.State)
	}
	return out
}

// latest is the state of command's latest entry in hold id's journal, "" for none.
func (h *t7Stack) latest(id, command string) string {
	h.t.Helper()
	state := ""
	for _, op := range h.journal(id).Ops {
		if op.Command == command {
			state = op.State
		}
	}
	return state
}

// database makes family's store a database of its own on the test's Postgres server, migrated by
// migrations, with its URL in a file, as --dispatch-database-file and --daemon-database name one.
func (h *t7Stack) database(family string, migrations ...string) *pgxpool.Pool {
	h.t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.DatabaseOn(h.t, os.Getenv("BROKER_TEST_DATABASE_URL"), "t7rows"))
	require.NoError(h.t, err)
	h.t.Cleanup(pool.Close)
	for _, file := range migrations {
		sql, err := os.ReadFile(filepath.FromSlash(file))
		require.NoError(h.t, err)
		_, err = pool.Exec(ctx, string(sql))
		require.NoError(h.t, err, "migrate %s with %s", family, file)
	}
	url := filepath.Join(h.t.TempDir(), "store.url")
	require.NoError(h.t, os.WriteFile(url, []byte(pool.Config().ConnString()+"\n"), 0o600))
	h.mu.Lock()
	h.dbs[family], h.files[family] = pool, url
	h.mu.Unlock()
	return pool
}

// file is family's store's URL file, "" when the row made no store of it.
func (h *t7Stack) file(family string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.files[family]
}

// t7Instance is the Cloud SQL instance connection name the rows' daemon stores are on, as each
// daemon's Deployment annotates it and the operator's file states it.
const t7Instance = "local:postgres:t7rows"

// daemonDeployment renders project's daemon Deployment in the stack's Kubernetes, namespace
// writerhold.DaemonNamespace, annotated with its project key and its store, the database the row
// made for daemon/<project> on t7Instance, and writes t7Instance as the second line of that
// store's file: the census's readers of the daemon stores read it only at that address
// (consumernames.ReadRecordedNames, ReconcileDaemonStreamIDs).
func (h *t7Stack) daemonDeployment(project string) {
	h.t.Helper()
	h.mu.Lock()
	pool, file := h.dbs["daemon/"+project], h.files["daemon/"+project]
	h.mu.Unlock()
	require.NotNil(h.t, pool, "the row made no store of daemon/%s", project)
	require.NoError(h.t, os.WriteFile(file, []byte(pool.Config().ConnString()+"\n"+t7Instance+"\n"), 0o600))
	_, err := h.s.Kube.Client.AppsV1().Deployments(writerhold.DaemonNamespace).Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "legion-daemon-" + strings.ToLower(project), Namespace: writerhold.DaemonNamespace,
			Annotations: map[string]string{
				writerhold.DaemonProjectAnnotation:       project,
				writerhold.DaemonStoreDatabaseAnnotation: pool.Config().ConnConfig.Database,
				writerhold.DaemonStoreInstanceAnnotation: t7Instance,
			}},
	}, metav1.CreateOptions{})
	require.NoError(h.t, err)
}

// dispatchMigrations are Dispatch's store's own migrations, so the table is the one its migration
// makes.
var dispatchMigrations = []string{
	"../../internal/dispatch/store/migrations/20261101143201_duty_holds.up.sql",
	"../../internal/dispatch/store/migrations/20261121002206_intake_consumers.up.sql",
}

// t7StreamID is the notification stream's identity in these rows, as the listener writes it when it
// creates the stream (selection.StreamIdentityMetadataKey).
const t7StreamID = "the-t7-rows-notification-stream"

// noStepsHold moves the released row through a hold of workflow with no steps on the hub (holdtest's
// Prepare and HoldWithoutTheSteps), runs change on the operator's connection under the held grant,
// writes command's intent and done entries around it, and releases the hold: the hub's state a row
// starts from, made the one way the operator makes a protected stream.
func (h *t7Stack) noStepsHold(w holdrecord.Workflow, command string, change func(nc *nats.Conn, js jetstream.JetStream)) {
	h.t.Helper()
	ctx := context.Background()
	h.closeConns()
	id := h.s.Prepare(h.t, w)
	h.s.HoldWithoutTheSteps(h.t, id)
	nc, err := h.s.ConnectOperator(ctx)
	require.NoError(h.t, err)
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	change(nc, js)
	nc.Close()
	_, err = h.s.Records.Admit(ctx, id, holdrecord.Request{Command: command}, nil)
	require.NoError(h.t, err)
	require.NoError(h.t, h.s.Records.AppendDone(ctx, id, command, nil))
	h.s.Release(h.t, id)
}

// seed makes every stream the hub holds before the cutover, at three copies, as the operator makes
// them under a streams-update hold of its own: each copied bucket with its own configuration
// (bus.CreateCopiedBucket), every other protected stream the build declares
// (streamset.EnsureStreams), ENVOY_MEASURE, and ENVOY_NOTIFICATIONS with its identity. The
// selection bucket is not made: create-bucket makes it in the cutover, and the preflight records its
// absence.
func (h *t7Stack) seed() {
	h.t.Helper()
	h.noStepsHold(holdrecord.WorkflowStreamsUpdate, writerhold.CommandStreamsUpdate, func(nc *nats.Conn, js jetstream.JetStream) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		legacy, err := nc.JetStream(nats.MaxWait(time.Minute))
		require.NoError(h.t, err)
		for _, bucket := range bus.CopiedBuckets() {
			_, err := bus.CreateCopiedBucket(ctx, nc, legacy, bucket, 3)
			require.NoError(h.t, err, "create copied bucket %s", bucket)
		}
		require.NoError(h.t, streamset.EnsureStreams(ctx, nc, streamset.Settings{Replicas: 3}))
		// ENVOY_MEASURE, which streams update makes beside the protected streams.
		measure, ok := streamset.Find(streamset.Declared(streamset.Settings{Replicas: 3}), streamset.MeasureStream)
		require.True(h.t, ok)
		require.NoError(h.t, streamset.Ensure(ctx, nc, legacy, measure), "make %s", streamset.MeasureStream)
		for _, bucket := range holdtest.ProtectedBuckets() {
			if bucket == selection.SessionBucket {
				continue
			}
			if _, err := js.Stream(ctx, "KV_"+bucket); err == nil {
				continue
			}
			_, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, Replicas: 3})
			require.NoError(h.t, err, "create bucket %s", bucket)
		}
		stream, err := js.Stream(ctx, bus.Stream)
		require.NoError(h.t, err)
		config := stream.CachedInfo().Config
		if config.Metadata == nil {
			config.Metadata = map[string]string{}
		}
		config.Metadata[selection.StreamIdentityMetadataKey] = t7StreamID
		_, err = js.UpdateStream(ctx, config)
		require.NoError(h.t, err, "write %s's identity", bus.Stream)
	})
	h.awaitLeaders()
}

// present are the protected streams the hub holds now, as member 0's /jsz lists them: the
// monitoring port answers in every phase, where the operator's released grant reads only some of
// the protected streams.
func (h *t7Stack) present() []string {
	h.t.Helper()
	address, stop, err := h.s.Kube.Forward(context.Background(), holdtest.Namespace, holdtest.Pod(0), holdtest.MonitorPort)
	require.NoError(h.t, err)
	defer stop()
	var read struct {
		AccountDetails []struct {
			Streams []struct {
				Name string `json:"name"`
			} `json:"stream_detail"`
		} `json:"account_details"`
	}
	require.NoError(h.t, writerhold.NewHTTPMonitor("http://"+address, nil).Get(context.Background(), "/jsz?accounts=true&streams=true", &read))
	held := map[string]bool{}
	for _, account := range read.AccountDetails {
		for _, stream := range account.Streams {
			held[stream.Name] = true
		}
	}
	var out []string
	for _, name := range writerhold.ProtectedStreams() {
		if held[name] {
			out = append(out, name)
		}
	}
	return out
}

// awaitLeaders waits until every member sees a leader for every protected stream the hub holds:
// after a hold's restarts the groups elect their leaders again, and a held re-read that reads a
// stream with none is UNKNOWN, as it must be.
func (h *t7Stack) awaitLeaders() {
	h.t.Helper()
	h.s.AwaitStreamLeaders(h.t, h.present())
}

// t7Fixed is session's fixed consumer name, s-<id>, as today's build names it.
func t7Fixed(session string) string { return "s-" + session }

// sessionConsumerConfig is a session consumer's configuration as today's listener makes it
// (bus.SessionConsumers): a pull consumer acknowledged one at a time, filtered on the session's
// agent subject, named name.
func sessionConsumerConfig(name, session string) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable: name, AckPolicy: jetstream.AckExplicitPolicy, MaxDeliver: -1, MaxAckPending: bus.SessionConsumerMaxAckPending,
		AckWait: bus.SessionConsumerAckWait, InactiveThreshold: time.Hour,
		FilterSubjects: bus.SessionConsumerFilters([]string{contracts.LegacyAgentSubject(session)}),
	}
}

// scene makes the hub's state before the cutover, as today's build leaves it, through a listener's
// own connection: for each session three messages on its agent subject and its fixed consumer
// s-<id>; and, with dispatch, Dispatch's two durables, as its intakes make them
// (bus.DispatchDeliveryConsumer, bus.DispatchJobConsumer), and Dispatch's store, migrated.
func (h *t7Stack) scene(dispatch bool, sessions ...string) {
	h.t.Helper()
	ctx := context.Background()
	nc, js := h.listener(holdtest.ListenerMachines[0])
	for _, session := range sessions {
		for i := range 3 {
			_, err := js.Publish(ctx, contracts.LegacyAgentSubject(session), []byte(fmt.Sprintf("%s #%d", session, i+1)))
			require.NoError(h.t, err)
		}
		_, err := js.CreateConsumer(ctx, bus.Stream, sessionConsumerConfig(t7Fixed(session), session))
		require.NoError(h.t, err, "make %s", t7Fixed(session))
	}
	if dispatch {
		legacy, err := nc.JetStream()
		require.NoError(h.t, err)
		for _, config := range []nats.ConsumerConfig{bus.DispatchDeliveryConsumer(), bus.DispatchJobConsumer()} {
			_, err := legacy.AddConsumer(bus.Stream, &config)
			require.NoError(h.t, err, "make %s", config.Durable)
		}
		h.database("dispatch", dispatchMigrations...)
	}
	h.closeConns()
}

// consumers are the notification stream's consumer names, as the operator lists them in a held or
// released row.
func (h *t7Stack) consumers() []string {
	h.t.Helper()
	nc, err := h.s.ConnectOperator(context.Background())
	require.NoError(h.t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	stream, err := js.Stream(context.Background(), bus.Stream)
	require.NoError(h.t, err)
	var names []string
	lister := stream.ConsumerNames(context.Background())
	for name := range lister.Name() {
		names = append(names, name)
	}
	require.NoError(h.t, lister.Err())
	slices.Sort(names)
	return names
}

// prepared runs `hold prepare --workflow w` and answers the hold id it prints.
func (h *t7Stack) prepared(w holdrecord.Workflow) string {
	h.t.Helper()
	code, stdout, _ := h.hold("prepare", "--workflow", string(w))
	require.Zero(h.t, code, "hold prepare --workflow %s", w)
	id := strings.TrimSpace(stdout)
	require.NotEmpty(h.t, id)
	return id
}

// set runs `hold set --hold id` with every step on the hub, requires it done, and waits for every
// protected stream's leader after its restarts.
func (h *t7Stack) set(id string) {
	h.t.Helper()
	code, stdout, _ := h.hold("set", "--hold", id)
	require.Zero(h.t, code, "hold set --hold %s", id)
	require.Contains(h.t, stdout, "only the operator")
	require.Equal(h.t, "done", h.latest(id, holdrecord.CommandSet))
	h.awaitLeaders()
}

// heldNoSteps moves the prepared hold id to held as hold set writes its journal, without its steps
// on the hub, recording each declared server's id now as the id after hold set's restart
// (server_id.<pod>), which every held consumer-names command's re-read compares: a row about what
// comes after a set hold rather than the steps themselves.
func (h *t7Stack) heldNoSteps(id string) {
	h.t.Helper()
	ctx := context.Background()
	_, err := h.s.Records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandSet}, nil)
	require.NoError(h.t, err)
	row := h.row()
	_, err = h.s.Records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseHeld, id, "the row's hold")
	require.NoError(h.t, err)
	declared, err := writerhold.Discover(ctx, h.op.Hub)
	require.NoError(h.t, err)
	outputs := map[string]string{}
	for _, server := range declared {
		outputs["server_id."+server.Pod] = server.Varz.ServerID
	}
	require.NoError(h.t, h.s.Records.AppendDone(ctx, id, holdrecord.CommandSet, outputs))
}

// cutoverThroughConvert runs a cutover hold of the scene's families through convert: hold prepare,
// consumer-names prepare, hold set with every step, create-bucket at three copies and convert,
// each required to succeed; it answers the hold id.
func (h *t7Stack) cutoverThroughConvert(convertArgs ...string) string {
	h.t.Helper()
	id := h.prepared(holdrecord.WorkflowCutover)
	code, _, stderr := h.names("prepare", "--hold", id)
	require.Zero(h.t, code, "consumer-names prepare: %s", stderr)
	h.set(id)
	code, _, stderr = h.names("create-bucket", "--hold", id, "--replicas", "3")
	require.Zero(h.t, code, "create-bucket: %s", stderr)
	code, _, stderr = h.names(append([]string{"convert", "--hold", id}, convertArgs...)...)
	require.Zero(h.t, code, "convert: %s", stderr)
	return id
}

// sessionRecord reads session's record in the selection bucket, as the operator under the row's
// phase.
func (h *t7Stack) sessionRecord(session string) selection.Record {
	h.t.Helper()
	nc, _ := h.operator()
	defer h.closeConns()
	record, _, err := selection.NewKVStore(selection.ConnRequester{Conn: nc}).Read(context.Background(), session)
	require.NoError(h.t, err, "%s's record", session)
	return record
}

// emptyHoldLog deletes the hold row and its log as the instance's admin does on a fresh database,
// and starts the broker's routes again, so no floor of the deleted rows is kept in memory: the
// state of a hub's first deployment, whose nats_hold_log holds nothing.
func (h *t7Stack) emptyHoldLog() {
	h.t.Helper()
	ctx := context.Background()
	_, err := h.s.Broker.Pool.Exec(ctx, `delete from nats_hold`)
	require.NoError(h.t, err)
	_, err = h.s.Broker.Pool.Exec(ctx, `delete from nats_hold_log`)
	require.NoError(h.t, err)
	h.s.Broker.Restart()
}

// holdLogRows counts the rows of nats_hold_log.
func (h *t7Stack) holdLogRows() int {
	h.t.Helper()
	var n int
	require.NoError(h.t, h.s.Broker.Pool.QueryRow(context.Background(), `select count(*) from nats_hold_log`).Scan(&n))
	return n
}

// TestHoldPreflightTheFirstDeployment: the first deployment (red team G5). A fresh hub with no
// KV_envoy_session_consumers and an empty nats_hold_log: `hold keygen` writes the signing key's
// private half to its store and prints the public half alone; `hold restore --bootstrap` writes the
// signed released row at gen 1; the protected streams are made under a streams-update hold, as the
// deployment makes them, the selection bucket not among them; then a cutover hold whose preflight
// records the selection bucket's expected absence runs every step from its preflight to its
// release: hold prepare, consumer-names prepare, hold set, create-bucket, convert, the census and
// hold release. Control: `hold restore --bootstrap` on a log that holds a row is refused, and
// writes nothing. It fails when the first deployment cannot reach a released hub, or the bootstrap
// writes over a row.
func TestHoldPreflightTheFirstDeployment(t *testing.T) {
	h := newT7Stack(t)
	// holdtest's stack bootstraps its row at start; the first deployment's log holds none. The auth
	// service has read gen 1 alone, so the bootstrap's gen 1 is not below its floor.
	h.emptyHoldLog()
	require.Zero(t, h.holdLogRows())
	code, _, stderr := h.hold("status")
	require.NotZero(t, code, "hold status read a row in an empty log")
	t.Logf("hold status of an empty log: %s", stderr)

	// hold keygen: the private half to the store, the public half alone printed.
	code, stdout, stderr := h.hold("keygen")
	require.Zero(t, code, "hold keygen")
	public, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(stdout))
	require.NoError(t, err, "keygen's output is no public key")
	require.Len(t, public, ed25519.PublicKeySize)
	_, stored, err := h.keys.Current(context.Background(), "projects/t7rows/secrets/"+writerhold.SigningKeySecret)
	require.NoError(t, err, "keygen wrote no version of the secret")
	seed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(stored)))
	require.NoError(t, err)
	require.Equal(t, ed25519.PublicKey(public), ed25519.NewKeyFromSeed(seed).Public(), "the stored private half is not the printed public half's")
	require.NotContains(t, stdout+stderr, string(stored), "keygen printed the private half")

	// hold restore --bootstrap: released at gen 1, signed; holdtest's broker verifies the stack's
	// own key, which stands in for keygen's in BROKER_NATS_HOLD_SIGNING_KEYS.
	code, stdout, _ = h.hold("restore", "--bootstrap")
	require.Zero(t, code, "hold restore --bootstrap on an empty log")
	require.Contains(t, stdout, "gen 1, phase released")
	first := h.row()
	require.Equal(t, uint64(1), first.Gen)
	require.Equal(t, natsgrant.OperatorPhaseReleased, first.Phase)
	require.Equal(t, 1, h.holdLogRows())

	// Control: a second bootstrap, on a log that holds the row, is refused and writes nothing.
	code, _, stderr = h.hold("restore", "--bootstrap")
	require.Equal(t, 1, code, "control: hold restore --bootstrap on a log that holds a row")
	require.Contains(t, stderr, "--bootstrap writes the first row only into an empty log")
	again := h.row()
	require.Equal(t, first.Gen, again.Gen, "control: the refused bootstrap moved the row")
	require.Equal(t, first.SignedAt, again.SignedAt, "control: the refused bootstrap wrote the row")
	require.Equal(t, 1, h.holdLogRows(), "control: the refused bootstrap wrote the log")

	// The protected streams, made under a hold of their own, and today's build's sessions.
	h.seed()
	h.scene(false, "ses_fd1", "ses_fd2")
	nc, js := h.operator()
	_, err = js.Stream(context.Background(), "KV_"+selection.SessionBucket)
	require.ErrorIs(t, err, jetstream.ErrStreamNotFound, "the fresh hub holds the selection bucket")
	nc.Close()

	// The cutover hold, every step from the preflight: hold set's preflight reads the selection
	// bucket absent, which the cutover's workflow declares.
	id := h.cutoverThroughConvert()
	require.Equal(t, []string{"hold set intent", "hold set done", "create-bucket intent", "create-bucket done",
		"convert intent", "convert done"}, h.entries(id))
	for _, session := range []string{"ses_fd1", "ses_fd2"} {
		record := h.sessionRecord(session)
		require.True(t, strings.HasPrefix(record.Current, "session~"), "%s's record selects %q, no fresh name", session, record.Current)
	}
	for _, name := range h.consumers() {
		require.NotContains(t, []string{t7Fixed("ses_fd1"), t7Fixed("ses_fd2")}, name, "a fixed name outlived convert")
	}
	h.census(id, "session")
	require.True(t, h.journal(id).Complete, "the census did not complete the cutover's record")
	code, _, _ = h.hold("release", "--hold", id)
	require.Zero(t, code, "hold release")
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)
	require.True(t, h.listenerAdmitted(), "a listener was refused after the first deployment's release")
}

// TestHoldPreflightALostSelectionBucket: a lost selection bucket (red team G5). After a cutover the
// selection bucket is lost; `selection-recovery`, whose preflight records the bucket's expected
// absence, runs every step from its preflight: hold prepare, hold set, create-bucket --after-loss,
// and hold release. Control: a buckets-create hold, whose preflight declares only its own targets
// absent, stops at hold set on the absent selection bucket, naming it. It fails when the recovery
// cannot set its hold over the lost bucket, or completes without the bucket and its sentinel.
func TestHoldPreflightALostSelectionBucket(t *testing.T) {
	h := newT7Stack(t)
	h.seed()
	h.scene(false, "ses_lost1")
	cutover := h.cutoverThroughConvert()
	h.census(cutover, "session")
	code, _, _ := h.hold("release", "--hold", cutover)
	require.Zero(t, code, "the cutover's release")
	fresh := h.sessionRecord("ses_lost1").Current

	// The bucket is lost.
	h.loseSelectionBucket()

	// Control: a hold whose workflow does not declare the selection bucket absent stops at its
	// preflight, naming it.
	control := h.prepared(holdrecord.WorkflowBucketsCreate)
	code, _, stderr := h.hold("set", "--hold", control)
	require.NotZero(t, code, "control: a buckets-create hold set over the lost selection bucket")
	require.Contains(t, stderr, "KV_"+selection.SessionBucket, "control: the preflight's refusal names the absent bucket")
	code, _, _ = h.hold("abort", "--hold", control)
	require.Zero(t, code, "the control's hold abort from prepare")
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase, "the control's hold was not ended")

	id := h.prepared(holdrecord.WorkflowSelectionRecovery)
	h.set(id)
	code, stdout, stderr := h.names("create-bucket", "--hold", id, "--after-loss", "--replicas", "3")
	require.Zero(t, code, "create-bucket --after-loss: %s", stderr)
	t.Logf("%s", stdout)
	require.Equal(t, []string{"hold set intent", "hold set done", "create-bucket --after-loss intent", "create-bucket --after-loss done"}, h.entries(id))
	require.True(t, h.journal(id).Complete, "create-bucket --after-loss did not complete the recovery's record")
	nc, _ := h.operator()
	state, err := selection.NewKVStore(selection.ConnRequester{Conn: nc}).State(context.Background())
	require.NoError(t, err, "the recovered bucket's sentinel")
	require.NotZero(t, state.ReplayFrom, "the recovered bucket's sentinel is not in replay mode")
	h.closeConns()
	code, _, _ = h.hold("release", "--hold", id)
	require.Zero(t, code, "the recovery's release")
	require.Contains(t, h.consumers(), fresh, "the recovery removed the session's fresh consumer")
}

// t7Kill is one command killed at a point a row names: from the moment it fires, the broker's
// routes answer every request 503 without serving it, as they answer for a process that died, and
// the command's context is cancelled, so it writes nothing more, to the broker or the hub.
type t7Kill struct {
	mu    sync.Mutex
	fired bool
}

// Fired reports whether the kill has fired.
func (k *t7Kill) Fired() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.fired
}

// killAt kills the command run on the context it answers at the first request to the broker's
// routes that match picks: just after the routes stored that request's write when after is set,
// so its write lands and the kill falls after it; else just before, so it does not. Either way the
// command never reads the routes' answer to that request, as a process killed then would not: an
// after kill serves the request into a recorder and answers the command 503, since a 200 sent while
// its context is cancelled reaches it or not as the transport's race decides. endKill removes the
// kill once the command returned.
func (h *t7Stack) killAt(match func(r *http.Request, body []byte) bool, after bool) (context.Context, *t7Kill) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	k := &t7Kill{}
	h.s.Broker.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		k.mu.Lock()
		fired := k.fired
		hit := !fired && match(r, body)
		if hit {
			k.fired = true
		}
		k.mu.Unlock()
		switch {
		case fired:
			http.Error(w, "the killed process sends nothing", http.StatusServiceUnavailable)
			return true
		case !hit:
			return false
		}
		if after {
			h.s.Broker.Routes().ServeHTTP(httptest.NewRecorder(), r)
			http.Error(w, "killed after this write", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "killed before this write", http.StatusServiceUnavailable)
		}
		cancel()
		return true
	})
	return ctx, k
}

// endKill removes the kill's hook from the broker's routes.
func (h *t7Stack) endKill() { h.s.Broker.Intercept(nil) }

// journalEntry is the entry a write of a hold's journal appends last: a PUT of the done record
// (kind done, key hold) through /v1/nats-hold/records.
func journalEntry(r *http.Request, body []byte) (holdrecord.Operation, bool) {
	if r.Method != http.MethodPut || r.URL.Path != "/v1/nats-hold/records" {
		return holdrecord.Operation{}, false
	}
	var put struct {
		Kind  string          `json:"kind"`
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if json.Unmarshal(body, &put) != nil || put.Kind != "done" || put.Key != "hold" {
		return holdrecord.Operation{}, false
	}
	var d holdrecord.Done
	if json.Unmarshal(put.Value, &d) != nil || len(d.Ops) == 0 {
		return holdrecord.Operation{}, false
	}
	return d.Ops[len(d.Ops)-1], true
}

// entryIs matches the journal write whose last entry is command's in one of states.
func entryIs(command string, states ...string) func(*http.Request, []byte) bool {
	return func(r *http.Request, body []byte) bool {
		op, ok := journalEntry(r, body)
		return ok && op.Command == command && slices.Contains(states, op.State)
	}
}

// cutoverOf matches the write of family's broker cutover row.
func cutoverOf(family string) func(*http.Request, []byte) bool {
	return func(r *http.Request, body []byte) bool {
		var put struct {
			Family string `json:"family"`
		}
		return r.Method == http.MethodPut && r.URL.Path == "/v1/nats-hold/cutover" && json.Unmarshal(body, &put) == nil && put.Family == family
	}
}

// rowTo matches the hold row's move to phase.
func rowTo(phase natsgrant.OperatorPhase) func(*http.Request, []byte) bool {
	return func(r *http.Request, body []byte) bool {
		var put struct {
			Phase string `json:"phase"`
		}
		return r.Method == http.MethodPut && r.URL.Path == "/v1/nats-hold" && json.Unmarshal(body, &put) == nil && put.Phase == string(phase)
	}
}

// killServedThenCancelled is killAt's after kill as it stood before it answered the command 503: at
// the first request match picks, the routes serve that request into the command's own answer, and
// only then is the command's context cancelled, so a command that sent the request on that context
// reads the 200 or context.Canceled as the HTTP transport's race decides.
// TestAKillAfterAWriteNeverAnswersIt runs it as its control.
func (h *t7Stack) killServedThenCancelled(match func(r *http.Request, body []byte) bool) (context.Context, *t7Kill) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	k := &t7Kill{}
	h.s.Broker.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		k.mu.Lock()
		hit := !k.fired && match(r, body)
		k.fired = k.fired || hit
		k.mu.Unlock()
		if !hit {
			return false
		}
		h.s.Broker.Routes().ServeHTTP(w, r)
		cancel()
		return true
	})
	return ctx, k
}

// TestAKillAfterAWriteNeverAnswersIt: killAt's after kill stores the write it matched and never
// answers it with a success, whatever context the writer reads its answer on. The write is the hold
// row's move from released to prepare, sent by the stack's client on a context the kill never
// cancels (holdrecord.Client.exchange reads the answer on the request's context), so the client
// reads whatever answer the routes send it, with no race against the cancel: the move is read back
// stored, and the client's answer is the kill's 503. Control: the same move under the kill as it
// stood before (killServedThenCancelled) is read back stored too, and the client reads the routes'
// 200, which fails the row's assertion. It fails when an after kill lets the killed writer read its
// write's success, which a killed process never reads: TestHoldCompletionPerWorkflow requires the
// census killed after its done entry to exit nonzero.
func TestAKillAfterAWriteNeverAnswersIt(t *testing.T) {
	h := newT7Stack(t)
	// move moves the row from released to prepare under id with kill armed at that move, reads the
	// move back stored once the kill is removed, and answers the client's error.
	move := func(kill func(func(*http.Request, []byte) bool) (context.Context, *t7Kill), id string) error {
		t.Helper()
		before := h.row()
		require.Equal(t, natsgrant.OperatorPhaseReleased, before.Phase, "the row before the move")
		killed, k := kill(rowTo(natsgrant.OperatorPhasePrepare))
		_, err := h.s.Records.MoveRow(context.Background(), before.Gen, natsgrant.OperatorPhasePrepare, id, "the kill's move")
		h.endKill()
		require.True(t, k.Fired(), "the kill never matched the move")
		require.ErrorIs(t, killed.Err(), context.Canceled, "the kill did not cancel the killed context")
		after := h.row()
		require.Equal(t, holdrecord.Row{Gen: before.Gen + 1, Phase: natsgrant.OperatorPhasePrepare, HoldID: id, SignedAt: after.SignedAt}, after,
			"the move the kill fell after was not stored")
		return err
	}

	// Control: the kill as it stood before answers the client the routes' 200.
	err := move(h.killServedThenCancelled, "served")
	require.NoError(t, err, "control: the client did not read the 200 the routes served it")
	_, err = h.s.Records.MoveRow(context.Background(), h.row().Gen, natsgrant.OperatorPhaseReleased, "served", "the control's abort")
	require.NoError(t, err, "the control's move back to released")

	err = move(func(match func(*http.Request, []byte) bool) (context.Context, *t7Kill) { return h.killAt(match, true) }, "killed")
	require.Error(t, err, "the client read a success to the write killAt killed after")
	require.ErrorContains(t, err, "answered 503", "the client's answer is not the kill's 503")
}

// t7KillDialer dials the operator's connection of one consumer-names run and kills that run at its
// first write to the hub that carries marker: the write is not sent, the connection closes, every
// later dial is refused and kill runs, so the run writes nothing more.
type t7KillDialer struct {
	marker []byte
	kill   func()

	mu     sync.Mutex
	killed bool
}

func (d *t7KillDialer) Dial(network, address string) (net.Conn, error) {
	d.mu.Lock()
	killed := d.killed
	d.mu.Unlock()
	if killed {
		return nil, errors.New("the killed process dials nothing")
	}
	conn, err := net.DialTimeout(network, address, 10*time.Second)
	if err != nil {
		return nil, err
	}
	return &t7KillConn{Conn: conn, d: d}, nil
}

// Killed reports whether the dialer's run was killed.
func (d *t7KillDialer) Killed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.killed
}

type t7KillConn struct {
	net.Conn
	d *t7KillDialer
}

func (c *t7KillConn) Write(p []byte) (int, error) {
	c.d.mu.Lock()
	hit := c.d.killed || bytes.Contains(p, c.d.marker)
	first := hit && !c.d.killed
	if hit {
		c.d.killed = true
	}
	c.d.mu.Unlock()
	if !hit {
		return c.Conn.Write(p)
	}
	if first {
		c.d.kill()
	}
	_ = c.Conn.Close()
	return 0, errors.New("the process was killed before this write")
}

// cutovers are the broker's cutover rows, by family, their state.
func (h *t7Stack) cutovers() map[string]string {
	h.t.Helper()
	rows, err := h.s.Records.Cutovers(context.Background())
	require.NoError(h.t, err)
	out := map[string]string{}
	for _, row := range rows {
		out[row.Family] = row.State
	}
	return out
}

// sentinel is the selection bucket's sentinel and its revision, read as the operator: the
// sequence of the sentinel key's last message (lastSeq), then the state through the store's check.
func (h *t7Stack) sentinel() (selection.StoreState, uint64) {
	h.t.Helper()
	revision := h.lastSeq("KV_"+selection.SessionBucket, "$KV."+selection.SessionBucket+"."+selection.SentinelKey)
	nc, _ := h.operator()
	defer h.closeConns()
	state, err := selection.NewKVStore(selection.ConnRequester{Conn: nc}).State(context.Background())
	require.NoError(h.t, err, "the selection bucket's sentinel")
	return state, revision
}

// t7Store is family's Postgres store on db (selection.NewPGStore), failing t when the library
// refuses to make it.
func t7Store(t testing.TB, family string, db *pgxpool.Pool) *selection.PGStore {
	t.Helper()
	store, err := selection.NewPGStore(family, db)
	require.NoError(t, err)
	return store
}

// cohortBoundary is the boundary B hold id's cohort record holds (consumer-names prepare's).
func (h *t7Stack) cohortBoundary(id string) uint64 {
	h.t.Helper()
	records, err := h.s.Records.Records(context.Background(), id, "cohort")
	require.NoError(h.t, err)
	for _, record := range records {
		var cohort struct {
			Boundary uint64 `json:"boundary"`
		}
		require.NoError(h.t, json.Unmarshal(record.Value, &cohort))
		return cohort.Boundary
	}
	h.t.Fatalf("hold %s has no cohort record", id)
	return 0
}

// bucketKeys are the selection bucket's keys and its stream's configuration, read as the operator
// under the held grant.
func (h *t7Stack) bucketKeys() ([]string, jetstream.StreamConfig) {
	h.t.Helper()
	_, js := h.operator()
	defer h.closeConns()
	ctx := context.Background()
	stream, err := js.Stream(ctx, "KV_"+selection.SessionBucket)
	require.NoError(h.t, err)
	info, err := stream.Info(ctx, jetstream.WithSubjectFilter("$KV."+selection.SessionBucket+".>"))
	require.NoError(h.t, err)
	var keys []string
	for subject := range info.State.Subjects {
		keys = append(keys, strings.TrimPrefix(subject, "$KV."+selection.SessionBucket+"."))
	}
	slices.Sort(keys)
	return keys, info.Config
}

// deleteBucket deletes the selection bucket as the operator under the held grant.
func (h *t7Stack) deleteBucket() {
	h.t.Helper()
	_, js := h.operator()
	require.NoError(h.t, js.DeleteStream(context.Background(), "KV_"+selection.SessionBucket))
	h.closeConns()
}

// loseSelectionBucket deletes the selection bucket under a streams-update hold of its own, as a lost
// bucket leaves the hub.
func (h *t7Stack) loseSelectionBucket() {
	h.t.Helper()
	h.noStepsHold(holdrecord.WorkflowStreamsUpdate, writerhold.CommandStreamsUpdate, func(_ *nats.Conn, js jetstream.JetStream) {
		require.NoError(h.t, js.DeleteStream(context.Background(), "KV_"+selection.SessionBucket))
	})
}

// heldCutover runs a cutover hold to where convert runs, with hold set's journal written without
// its steps on the hub (heldNoSteps): hold prepare, consumer-names prepare, the move to held, and
// create-bucket at three copies; it answers the hold id.
func (h *t7Stack) heldCutover() string {
	h.t.Helper()
	id := h.prepared(holdrecord.WorkflowCutover)
	code, _, stderr := h.names("prepare", "--hold", id)
	require.Zero(h.t, code, "consumer-names prepare: %s", stderr)
	h.heldNoSteps(id)
	code, _, stderr = h.names("create-bucket", "--hold", id, "--replicas", "3")
	require.Zero(h.t, code, "create-bucket: %s", stderr)
	return id
}

// fixedOf makes, as today's listener, a consumer of the notification stream with config.
func (h *t7Stack) fixedOf(config jetstream.ConsumerConfig) {
	h.t.Helper()
	_, js := h.listener(holdtest.ListenerMachines[0])
	_, err := js.CreateConsumer(context.Background(), bus.Stream, config)
	require.NoError(h.t, err, "make %s", config.Durable)
	h.closeConns()
}

// TestHoldACrashThenARerun: a crash, then a rerun (red team G1), each command killed at a point the
// row names, deterministically: at a write to the broker's routes (killAt), or at a write to the hub
// (t7KillDialer), after which the killed process writes nothing. In each case the rerun under the
// same hold completes, and nothing completes the record early.
func TestHoldACrashThenARerun(t *testing.T) {
	t.Run("create-bucket killed after its intent, convert killed after half the families, then each run again", func(t *testing.T) {
		h := newT7Stack(t)
		h.seed()
		h.scene(true, "ses_c1", "ses_c2")
		dispatch := h.file("dispatch")
		id := h.prepared(holdrecord.WorkflowCutover)
		code, _, stderr := h.names("prepare", "--hold", id)
		require.Zero(t, code, stderr)
		h.heldNoSteps(id)

		// create-bucket killed after its intent: convert is refused ErrBusy, naming create-bucket.
		ctx, kill := h.killAt(entryIs("create-bucket", "intent"), true)
		code, _, _ = h.namesOn(ctx, "create-bucket", "--hold", id, "--replicas", "3")
		h.endKill()
		require.True(t, kill.Fired(), "create-bucket wrote no intent")
		require.NotZero(t, code, "create-bucket killed after its intent")
		require.Equal(t, "intent", h.latest(id, "create-bucket"))
		code, _, stderr = h.names("convert", "--hold", id, "--dispatch-database-file", dispatch)
		require.Equal(t, 1, code, "convert while create-bucket's intent is pending")
		require.Contains(t, stderr, holdrecord.ErrBusy.Error())
		require.Contains(t, stderr, "create-bucket")
		require.NotContains(t, h.entries(id), "convert intent", "the refused convert wrote its intent")
		code, _, stderr = h.hold("release", "--hold", id)
		require.Equal(t, 1, code, "release while create-bucket's intent is pending")
		require.Contains(t, stderr, holdrecord.ErrBusy.Error())
		code, _, stderr = h.names("create-bucket", "--hold", id, "--replicas", "3")
		require.Zero(t, code, "create-bucket's rerun: %s", stderr)

		// convert killed after it converted every name and marked half the families: the session
		// family's broker cutover row and store marker are done, Dispatch's are not.
		ctx, kill = h.killAt(cutoverOf("dispatch"), false)
		code, _, _ = h.namesOn(ctx, "convert", "--hold", id, "--dispatch-database-file", dispatch)
		h.endKill()
		require.True(t, kill.Fired(), "convert never reached Dispatch's cutover row")
		require.NotZero(t, code, "convert killed after half the families")
		require.Equal(t, "intent", h.latest(id, "convert"))
		require.Equal(t, map[string]string{"session": selection.CutoverDone}, h.cutovers(), "the killed convert's cutover rows")
		marked, revision := h.sentinel()
		require.Equal(t, selection.CutoverDone, marked.Cutover, "the session family's store marker")
		dispatchState, err := t7Store(t, selection.DispatchFamily, h.dbs["dispatch"]).State(context.Background())
		require.True(t, err != nil || dispatchState.Cutover != selection.CutoverDone, "Dispatch's store marker is done before its family was converted")

		// release is refused until convert and the census complete; the census is refused too.
		code, _, stderr = h.hold("release", "--hold", id)
		require.Equal(t, 1, code, "release after the killed convert")
		require.Contains(t, stderr, holdrecord.ErrBusy.Error())
		code, _, stderr = h.streams(h.censusArgs(id, "session", "dispatch")...)
		require.NotZero(t, code, "the census after the killed convert")
		require.Contains(t, stderr, holdrecord.ErrBusy.Error())
		require.NotContains(t, h.entries(id), "streams census intent")

		// convert's rerun converts no name again: it writes each family's cutover row again, leaves
		// the session family's marker, which reads done, unwritten, and marks Dispatch.
		code, stdout, stderr := h.names("convert", "--hold", id, "--dispatch-database-file", dispatch)
		require.Zero(t, code, "convert's rerun: %s", stderr)
		require.Contains(t, stdout, "converted 0 of", "convert's rerun converted a name again")
		_, again := h.sentinel()
		require.Equal(t, revision, again, "convert's rerun wrote the session family's marker again")
		dispatchState, err = t7Store(t, selection.DispatchFamily, h.dbs["dispatch"]).State(context.Background())
		require.NoError(t, err)
		require.Equal(t, selection.CutoverDone, dispatchState.Cutover, "convert's rerun did not mark Dispatch")
		require.Equal(t, map[string]string{"session": selection.CutoverDone, "dispatch": selection.CutoverDone}, h.cutovers())
		code, _, stderr = h.hold("release", "--hold", id)
		require.Equal(t, 1, code, "release before the census")
		require.Contains(t, stderr, holdrecord.ErrOutOfOrder.Error())
		h.census(id, "session", "dispatch")
		require.True(t, h.journal(id).Complete)
		code, _, _ = h.hold("release", "--hold", id)
		require.Zero(t, code, "release after convert's rerun and the census")

		// Control: a done record to which the commands appended no entry releases with a family
		// unconverted, so it is the commands' entries that hold release until convert completes.
		h.fixedOf(sessionConsumerConfig(t7Fixed("ses_c3"), "ses_c3"))
		control := h.prepared(holdrecord.WorkflowCutover)
		h.heldNoSteps(control)
		code, _, _ = h.hold("release", "--hold", control)
		require.Zero(t, code, "control: release of a cutover hold whose record holds no command's entry")
		require.Contains(t, h.consumers(), t7Fixed("ses_c3"), "control: the session family is not unconverted")
	})

	t.Run("create-bucket killed between its create and its sentinel, and the buckets its rerun refuses", func(t *testing.T) {
		h := newT7Stack(t)
		h.seed()
		h.scene(false, "ses_b1")
		id := h.prepared(holdrecord.WorkflowCutover)
		code, _, stderr := h.names("prepare", "--hold", id)
		require.Zero(t, code, stderr)
		h.heldNoSteps(id)
		boundary := h.cohortBoundary(id)
		ctx := context.Background()

		// A bucket present that is not this hold's stops the rerun as UNKNOWN, naming it, and is
		// left as it is.
		withKey := selection.SessionBucketConfig(3)
		other := func(change func(*jetstream.KeyValueConfig)) jetstream.KeyValueConfig {
			config := selection.SessionBucketConfig(3)
			change(&config)
			return config
		}
		for _, c := range []struct {
			what   string
			config jetstream.KeyValueConfig
			key    string
		}{
			{"another replicas", selection.SessionBucketConfig(1), ""},
			{"another storage", other(func(c *jetstream.KeyValueConfig) { c.Storage = jetstream.MemoryStorage }), ""},
			{"another max_bytes", other(func(c *jetstream.KeyValueConfig) { c.MaxBytes = 1 << 20 }), ""},
			{"a key but the sentinel", withKey, "ses_other"},
		} {
			_, js := h.operator()
			kv, err := js.CreateKeyValue(ctx, c.config)
			require.NoError(t, err, "make a bucket with %s", c.what)
			if c.key != "" {
				_, err := kv.Put(ctx, c.key, []byte(`{"current":"session~other"}`))
				require.NoError(t, err)
			}
			h.closeConns()
			keysBefore, _ := h.bucketKeys()
			code, _, stderr := h.names("create-bucket", "--hold", id, "--replicas", "3")
			require.Equal(t, 2, code, "create-bucket's rerun over a bucket with %s", c.what)
			require.Contains(t, stderr, "UNKNOWN")
			require.Contains(t, stderr, selection.SessionBucket, "the UNKNOWN does not name the bucket")
			require.Equal(t, "stop", h.latest(id, "create-bucket"))
			keysAfter, _ := h.bucketKeys()
			for _, key := range keysAfter {
				require.True(t, slices.Contains(keysBefore, key) || strings.HasPrefix(key, "hold.barrier."),
					"create-bucket wrote %s into a bucket with %s", key, c.what)
			}
			if c.key != "" {
				// Control: a rerun that takes any present bucket as its own writes its sentinel over
				// this one, which holds another key.
				nc, _ := h.operator()
				_, err := selection.NewKVStore(selection.ConnRequester{Conn: nc}).
					WriteSentinel(ctx, selection.StoreState{Boundary: boundary, Hold: id}, 0)
				require.NoError(t, err)
				h.closeConns()
				keys, _ := h.bucketKeys()
				require.Subset(t, keys, []string{c.key, selection.SentinelKey},
					"control: the any-bucket rerun did not write its sentinel beside %s", c.key)
				t.Logf("control: a rerun that takes any present bucket as its own writes its sentinel over a bucket holding %s: %v", c.key, keys)
			}
			h.deleteBucket()
		}

		// A bucket whose STREAM.INFO fails: the run's request is refused by the grant and never
		// answered.
		_, js := h.operator()
		_, err := js.CreateKeyValue(ctx, selection.SessionBucketConfig(3))
		require.NoError(t, err)
		h.closeConns()
		info := "STREAM.INFO.KV_" + selection.SessionBucket
		failing := t7TapDialer{tap: func(p []byte) []byte {
			return bytes.ReplaceAll(p, []byte(info), []byte("STREAM.INFX.KV_"+selection.SessionBucket))
		}}
		bounded, cancel := context.WithTimeout(ctx, 3*time.Minute)
		code, _, stderr = h.namesThrough(bounded, failing, "create-bucket", "--hold", id, "--replicas", "3")
		cancel()
		require.Equal(t, 2, code, "create-bucket's rerun whose STREAM.INFO of the bucket fails")
		require.Contains(t, stderr, "UNKNOWN")
		require.Contains(t, stderr, selection.SessionBucket)
		h.deleteBucket()

		// create-bucket killed between its create and its sentinel: its intent is pending over an
		// empty bucket of its own configuration.
		killed, kill := context.WithCancel(ctx)
		defer kill()
		dialer := &t7KillDialer{marker: []byte("$KV." + selection.SessionBucket + "." + selection.SentinelKey + " "), kill: kill}
		code, _, _ = h.namesThrough(killed, dialer, "create-bucket", "--hold", id, "--replicas", "3")
		require.True(t, dialer.Killed(), "create-bucket never wrote its sentinel")
		require.NotZero(t, code, "create-bucket killed before its sentinel")
		require.Equal(t, "intent", h.latest(id, "create-bucket"))
		keys, config := h.bucketKeys()
		require.Empty(t, keys, "the killed create-bucket's bucket holds keys")
		require.Equal(t, 3, config.Replicas)

		// Its rerun adopts the empty bucket, writes the sentinel with B, and completes.
		code, stdout, stderr := h.names("create-bucket", "--hold", id, "--replicas", "3")
		require.Zero(t, code, "create-bucket's rerun: %s", stderr)
		require.Contains(t, stdout, "held no key")
		state, _ := h.sentinel()
		require.Equal(t, boundary, state.Boundary, "the rerun's sentinel does not hold B")
		require.Equal(t, id, state.Hold)
		require.Equal(t, "done", h.latest(id, "create-bucket"))
	})

	t.Run("the census killed after its removals, then run again", func(t *testing.T) {
		h := newT7Stack(t)
		h.seed()
		h.scene(false, "ses_k1")
		planted := jetstream.ConsumerConfig{Durable: "plant", AckPolicy: jetstream.AckExplicitPolicy, FilterSubject: "notifications.dispatch.issue.PRIV.>"}
		h.fixedOf(planted)
		id := h.heldCutover()
		code, _, stderr := h.names("convert", "--hold", id)
		require.Zero(t, code, stderr)

		ctx, kill := h.killAt(entryIs(writerhold.CommandStreamsCensus, "stop", "done"), false)
		code, stdout, _ := h.streamsOn(ctx, censusReaders(), h.censusArgs(id, "session")...)
		h.endKill()
		require.True(t, kill.Fired(), "the census wrote no entry after its intent")
		require.NotZero(t, code)
		require.Contains(t, stdout, "plant", "the killed census did not remove the planted consumer")
		require.NotContains(t, h.consumers(), "plant")
		require.Equal(t, "intent", h.latest(id, writerhold.CommandStreamsCensus))
		require.False(t, h.journal(id).Complete)

		code, stdout, stderr = h.streams(h.censusArgs(id, "session")...)
		require.Zero(t, code, "the census's rerun: %s", stderr)
		require.Contains(t, strings.Split(strings.TrimSpace(stdout), "\n"), "CLEAN", "the rerun is not clean")
		require.NotContains(t, stdout, "CHANGED", "the rerun found a removed object again")
		require.True(t, h.journal(id).Complete, "the rerun did not complete the record")
		code, _, _ = h.hold("release", "--hold", id)
		require.Zero(t, code)

		// Control: a census that writes its done entry before its removals, killed between the two,
		// completes a record while an undeclared object remains.
		planted.Durable = "plant2"
		h.fixedOf(planted)
		control := h.prepared(holdrecord.WorkflowStreamsCensus)
		h.heldNoSteps(control)
		_, err := h.s.Records.Admit(context.Background(), control, holdrecord.Request{Command: writerhold.CommandStreamsCensus}, nil)
		require.NoError(t, err)
		require.NoError(t, h.s.Records.AppendDone(context.Background(), control, writerhold.CommandStreamsCensus, nil))
		require.True(t, h.journal(control).Complete, "control: the done-first census did not complete its record")
		require.Contains(t, h.consumers(), "plant2", "control: the done-first census's record is complete while plant2 remains")
		code, _, _ = h.hold("release", "--hold", control)
		require.Zero(t, code, "control: the done-first census's hold releases over plant2")
	})

	t.Run("replay --final killed after its record write, then run again", func(t *testing.T) {
		h := newT7Stack(t)
		h.seed()
		h.scene(false, "ses_r1")
		cutover := h.heldCutover()
		code, _, stderr := h.names("convert", "--hold", cutover)
		require.Zero(t, code, stderr)
		h.census(cutover, "session")
		code, _, _ = h.hold("release", "--hold", cutover)
		require.Zero(t, code)
		h.loseSelectionBucket()
		id := h.prepared(holdrecord.WorkflowSelectionRecovery)
		h.heldNoSteps(id)
		code, _, stderr = h.names("create-bucket", "--hold", id, "--after-loss", "--replicas", "3")
		require.Zero(t, code, stderr)
		require.True(t, h.journal(id).Complete)

		ctx, kill := h.killAt(entryIs("replay --final", "done"), false)
		code, _, _ = h.namesOn(ctx, "replay", "--hold", id, "--key", "ses_r1", "--from", "2", "--final")
		h.endKill()
		require.True(t, kill.Fired(), "replay --final never reached its done entry")
		require.NotZero(t, code)
		require.Equal(t, "intent", h.latest(id, "replay --final"))
		require.False(t, h.journal(id).Complete, "replay --final's intent left the record complete")
		written := h.sessionRecord("ses_r1")
		require.Equal(t, uint64(2), written.Start, "the killed replay --final did not write its record")

		code, _, stderr = h.names("replay", "--hold", id, "--key", "ses_r1", "--from", "2", "--final")
		require.Zero(t, code, "replay --final's rerun: %s", stderr)
		again := h.sessionRecord("ses_r1")
		require.Equal(t, written.Start, again.Start, "the rerun wrote another start")
		require.Equal(t, written.Current, again.Current, "the rerun wrote another current")
		require.True(t, h.journal(id).Complete, "replay --final's rerun did not complete the record")
		code, _, _ = h.hold("release", "--hold", id)
		require.Zero(t, code)
	})

	t.Run("hold set killed after its intent, and after its row move", func(t *testing.T) {
		h := newT7Stack(t)
		h.seed()

		// Killed after its intent, the row prepare: hold abort from prepare releases.
		aborted := h.prepared(holdrecord.WorkflowCutover)
		ctx, kill := h.killAt(entryIs(holdrecord.CommandSet, "intent"), true)
		code, _, _ := h.holdOn(ctx, "set", "--hold", aborted)
		h.endKill()
		require.True(t, kill.Fired())
		require.NotZero(t, code)
		require.Equal(t, natsgrant.OperatorPhasePrepare, h.row().Phase)
		code, _, _ = h.hold("abort", "--hold", aborted)
		require.Zero(t, code, "hold abort from prepare after the killed hold set")
		require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)

		// Killed after its intent again: its rerun is admitted and goes on.
		rerun := h.prepared(holdrecord.WorkflowCutover)
		ctx, kill = h.killAt(entryIs(holdrecord.CommandSet, "intent"), true)
		code, _, _ = h.holdOn(ctx, "set", "--hold", rerun)
		h.endKill()
		require.True(t, kill.Fired())
		require.NotZero(t, code)
		h.set(rerun)
		code, _, _ = h.hold("release", "--hold", rerun)
		require.Zero(t, code)

		// Killed after its row move: every command but its rerun is refused ErrBusy, and the rerun
		// runs steps 2 to 6 and writes its done entry.
		moved := h.prepared(holdrecord.WorkflowCutover)
		ctx, kill = h.killAt(rowTo(natsgrant.OperatorPhaseHeld), true)
		code, _, _ = h.holdOn(ctx, "set", "--hold", moved)
		h.endKill()
		require.True(t, kill.Fired())
		require.NotZero(t, code)
		require.Equal(t, natsgrant.OperatorPhaseHeld, h.row().Phase)
		require.Equal(t, "intent", h.latest(moved, holdrecord.CommandSet))
		before := h.entries(moved)
		for _, word := range [][]string{{"release", "--hold", moved}, {"abort", "--hold", moved}} {
			code, _, stderr := h.hold(word...)
			require.Equal(t, 1, code, "hold %s after hold set's row move", word[0])
			require.Contains(t, stderr, holdrecord.ErrBusy.Error())
		}
		code, _, _ = h.names("create-bucket", "--hold", moved, "--replicas", "3")
		require.NotZero(t, code, "create-bucket after hold set's row move")
		_, err := h.s.Records.Admit(context.Background(), moved, holdrecord.Request{Command: "create-bucket"}, nil)
		require.ErrorIs(t, err, holdrecord.ErrBusy, "create-bucket's admission after hold set's row move")
		require.Equal(t, before, h.entries(moved), "a refused command wrote to the journal")
		h.set(moved)
		require.Equal(t, append(before, "hold set done"), h.entries(moved))
		code, _, _ = h.hold("release", "--hold", moved)
		require.Zero(t, code)
	})

	t.Run("convert stopped for a name with no safe start", func(t *testing.T) {
		h := newT7Stack(t)
		h.seed()
		h.scene(false, "ses_s1")
		nc, _ := h.listener(holdtest.ListenerMachines[0])
		legacy, err := nc.JetStream()
		require.NoError(t, err)
		events := bus.DispatchDeliveryConsumer()
		_, err = legacy.AddConsumer(bus.Stream, &events)
		require.NoError(t, err)
		h.closeConns()
		h.database("dispatch", dispatchMigrations...)
		dispatch := h.file("dispatch")
		id := h.heldCutover()

		code, _, stderr := h.names("convert", "--hold", id, "--dispatch-database-file", dispatch)
		require.Equal(t, 1, code, "convert with no safe start for %s", bus.DispatchJobDurable)
		require.Contains(t, stderr, "no safe start for "+bus.DispatchJobDurable)
		require.Equal(t, "stop", h.latest(id, "convert"), "convert's refusal wrote no stop entry")

		// Control: a convert that is not Stoppable leaves its intent, under which the replay that is
		// the way out is refused ErrBusy.
		_, err = h.s.Records.Admit(context.Background(), id, holdrecord.Request{Command: "convert"}, nil)
		require.NoError(t, err)
		code, _, stderr = h.names("replay", "--hold", id, "--key", bus.DispatchJobDurable, "--from", "1")
		require.Equal(t, 1, code, "control: replay while a convert's intent is pending")
		require.Contains(t, stderr, holdrecord.ErrBusy.Error(), "control: a convert that cannot stop leaves replay refused")
		require.NoError(t, h.s.Records.AppendStopLeft(context.Background(), id, "convert",
			"the control's intent, of a convert that could not stop", "nothing but this intent"))

		code, _, stderr = h.names("replay", "--hold", id, "--key", bus.DispatchJobDurable, "--from", "1")
		require.Zero(t, code, "replay --key %s: %s", bus.DispatchJobDurable, stderr)
		code, _, stderr = h.names("convert", "--hold", id, "--dispatch-database-file", dispatch)
		require.Zero(t, code, "convert after the replay: %s", stderr)
		h.census(id, "session", "dispatch")
		require.True(t, h.journal(id).Complete)
		code, _, _ = h.hold("release", "--hold", id)
		require.Zero(t, code)
	})
}

// daemonMigrations are a Legion daemon's store's own migrations.
var daemonMigrations = []string{
	"../../../daemon/internal/store/migrations/0063_consumer_positions.up.sql",
	"../../../daemon/internal/store/migrations/20261121001600_intake_consumers.up.sql",
}

// t7TapDialer dials an operator connection whose every write to the hub goes through tap first, which
// may read it, or rewrite it to bytes as long.
type t7TapDialer struct{ tap func(p []byte) []byte }

func (d t7TapDialer) Dial(network, address string) (net.Conn, error) {
	conn, err := net.DialTimeout(network, address, 10*time.Second)
	if err != nil {
		return nil, err
	}
	return &t7TapConn{Conn: conn, tap: d.tap}, nil
}

type t7TapConn struct {
	net.Conn
	tap func(p []byte) []byte
}

func (c *t7TapConn) Write(p []byte) (int, error) {
	if _, err := c.Conn.Write(c.tap(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// t7Exchange is one request to the broker's routes and its answer.
type t7Exchange struct {
	Body   []byte
	Status int
	Answer []byte
}

// recordRestores serves every POST /v1/nats-hold/restore through the routes and records it, each
// answer passed through rewrite first when it is set; it answers the exchanges so far.
func (h *t7Stack) recordRestores(rewrite func(*t7Exchange)) func() []t7Exchange {
	var mu sync.Mutex
	var seen []t7Exchange
	h.s.Broker.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/nats-hold/restore" {
			return false
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		h.s.Broker.Routes().ServeHTTP(recorder, r)
		e := t7Exchange{Body: body, Status: recorder.Code, Answer: recorder.Body.Bytes()}
		if rewrite != nil {
			rewrite(&e)
		}
		mu.Lock()
		seen = append(seen, e)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(e.Status)
		_, _ = w.Write(e.Answer)
		return true
	})
	return func() []t7Exchange {
		mu.Lock()
		defer mu.Unlock()
		return append([]t7Exchange(nil), seen...)
	}
}

// genOf is the gen a restore request carries.
func genOf(t *testing.T, body []byte) uint64 {
	t.Helper()
	var request struct {
		Gen uint64 `json:"gen"`
	}
	require.NoError(t, json.Unmarshal(body, &request))
	return request.Gen
}

// maxLoggedGen is the highest gen nats_hold_log holds.
func (h *t7Stack) maxLoggedGen() uint64 {
	h.t.Helper()
	var gen uint64
	require.NoError(h.t, h.s.Broker.Pool.QueryRow(context.Background(), `select coalesce(max(gen), 0) from nats_hold_log`).Scan(&gen))
	return gen
}

// deleteRow deletes the hold row as the instance's admin does.
func (h *t7Stack) deleteRow() {
	h.t.Helper()
	_, err := h.s.Broker.Pool.Exec(context.Background(), `delete from nats_hold`)
	require.NoError(h.t, err)
}

// rowMissing requires the operator's client to find no row.
func (h *t7Stack) rowMissing(what string) {
	h.t.Helper()
	_, err := h.s.Records.Row(context.Background())
	var refused *holdrecord.RouteError
	require.ErrorAs(h.t, err, &refused, "%s: the row is there", what)
	require.Equal(h.t, "no_row", refused.Code, what)
}

// listenerRefused requires a listener's connect to be refused, as the auth service refuses one.
func (h *t7Stack) listenerRefused(what string) {
	h.t.Helper()
	require.False(h.t, h.listenerAdmitted(), "%s: a listener was admitted", what)
}

// TestHoldRestoreALostRow: a lost row (`hold restore`). Mid-cutover the test's admin deletes the row:
// every client but the operator is refused hold_unknown. `hold restore --hold <id>` brings the row
// back held, signed at the restore_gen that /restore's 409 gen_mismatch names, above every logged
// one; a PUT at a gen read before the delete answers 409; convert and the census run, and release
// succeeds. After completion the admin deletes the row again: `hold restore` brings the row back
// held at a higher gen; a second `hold restore` answers 409 row_exists; and release succeeds,
// printing both gens. Last, a hold lost mid-way: a --bootstrap restore is refused, because its gen
// 1 is not the log's next.
// Controls: a restore that reads the field gen in place of restore_gen sends gen 1 again and is
// refused; the release a comparison of hold_gen with the row's gen (v59's) would refuse for good;
// and a row put back at the gen it had, as a restore that does not move above the log would leave
// it, accepts the stale PUT.
func TestHoldRestoreALostRow(t *testing.T) {
	h := newT7Stack(t)
	// One replica decides every client, so its decisions are counted by reason.
	h.s.Auth.Close()
	replica := h.s.StartReplica(t, holdtest.ReplicaConfig{})
	h.seed()
	h.scene(false, "ses_lr1")
	ctx := context.Background()
	id := h.heldCutover()
	stale := h.row()

	h.deleteRow()
	h.rowMissing("the deleted row")
	// The listener's connect tries each of the three servers, and each refusal is the replica's.
	unknown, held := replica.Decisions("service", "refused", "hold_unknown"), replica.Decisions("service", "refused", "held")
	h.listenerRefused("the row deleted")
	require.Greater(t, replica.Decisions("service", "refused", "hold_unknown"), unknown, "the listener's refusal is not hold_unknown")
	require.Equal(t, held, replica.Decisions("service", "refused", "held"), "the listener was refused held with no row")
	op, err := h.s.ConnectOperator(ctx)
	require.NoError(t, err, "the operator with the row deleted")
	op.Close()

	// Control: a hold restore that reads the field gen, which the refusal does not carry, in place of
	// restore_gen sends gen 1 again, and is refused.
	exchanges := h.recordRestores(func(e *t7Exchange) {
		var answer map[string]any
		if e.Status == http.StatusConflict && json.Unmarshal(e.Answer, &answer) == nil {
			delete(answer, "restore_gen")
			e.Answer, _ = json.Marshal(answer)
		}
	})
	code, _, stderr := h.hold("restore", "--hold", id)
	h.s.Broker.Intercept(nil)
	require.Equal(t, 1, code, "control: a restore that reads gen in place of restore_gen")
	require.Contains(t, stderr, "gen_mismatch")
	for _, e := range exchanges() {
		require.Equal(t, uint64(1), genOf(t, e.Body), "control: the restore that does not read restore_gen sent another gen than 1")
	}
	h.rowMissing("control: the restore that does not read restore_gen")
	t.Logf("control: a restore that reads gen in place of restore_gen sent gen 1 again and was refused gen_mismatch")

	logged := h.maxLoggedGen()
	exchanges = h.recordRestores(nil)
	code, stdout, _ := h.hold("restore", "--hold", id)
	h.s.Broker.Intercept(nil)
	require.Zero(t, code, "hold restore --hold")
	require.Contains(t, stdout, "phase held")
	sent := exchanges()
	require.Len(t, sent, 2, "the restore's requests")
	require.Equal(t, uint64(1), genOf(t, sent[0].Body))
	require.Equal(t, http.StatusConflict, sent[0].Status)
	var refusal struct {
		Error      string `json:"error"`
		RestoreGen uint64 `json:"restore_gen"`
	}
	require.NoError(t, json.Unmarshal(sent[0].Answer, &refusal))
	require.Equal(t, "gen_mismatch", refusal.Error)
	require.Equal(t, logged+1, refusal.RestoreGen, "restore_gen is not one above every logged gen")
	require.Equal(t, refusal.RestoreGen, genOf(t, sent[1].Body), "the restore was not signed again at restore_gen")
	require.Equal(t, http.StatusOK, sent[1].Status)
	restored := h.row()
	require.Equal(t, natsgrant.OperatorPhaseHeld, restored.Phase)
	require.Equal(t, id, restored.HoldID)
	require.Equal(t, logged+1, restored.Gen)
	held = replica.Decisions("service", "refused", "held")
	unknown = replica.Decisions("service", "refused", "hold_unknown")
	h.listenerRefused("the restored row")
	require.Greater(t, replica.Decisions("service", "refused", "held"), held, "the restored row does not refuse a listener held")
	require.Equal(t, unknown, replica.Decisions("service", "refused", "hold_unknown"), "the restored row reads unknown")

	// A PUT at a gen read before the delete answers 409, and the row is unchanged.
	_, err = h.s.Records.MoveRow(ctx, stale.Gen, natsgrant.OperatorPhaseReleased, id, "a release that read the row before the delete")
	var conflict *holdrecord.RouteError
	require.ErrorAs(t, err, &conflict, "the stale PUT")
	require.Equal(t, http.StatusConflict, conflict.Status)
	require.Equal(t, restored, h.row(), "the stale PUT moved the row")

	code, _, stderr = h.names("convert", "--hold", id)
	require.Zero(t, code, "convert after the restore: %s", stderr)
	h.census(id, "session")
	complete := h.journal(id)
	require.True(t, complete.Complete)

	// After completion: the row lost again, and restored held at a higher gen.
	h.deleteRow()
	code, stdout, _ = h.hold("restore", "--hold", id)
	require.Zero(t, code, "hold restore after completion")
	again := h.row()
	require.Equal(t, natsgrant.OperatorPhaseHeld, again.Phase)
	require.Greater(t, again.Gen, restored.Gen, "the second restore is not at a higher gen")
	code, _, stderr = h.hold("restore", "--hold", id)
	require.Equal(t, 1, code, "a second hold restore")
	require.Contains(t, stderr, "row_exists")
	// Control: v59's comparison of the record's hold_gen with the row's gen, which the restore moved,
	// would refuse this release for good.
	require.NotEqual(t, complete.HoldGen, again.Gen, "control: hold_gen equals the restored row's gen, so v59's comparison would not refuse")
	t.Logf("control: v59's hold_gen comparison refuses this release for good: hold_gen %d, the restored row's gen %d", complete.HoldGen, again.Gen)
	code, stdout, _ = h.hold("release", "--hold", id)
	require.Zero(t, code, "release over the restored row")
	require.Contains(t, stdout, fmt.Sprintf("row gen %d", again.Gen+1))
	require.Contains(t, stdout, fmt.Sprintf("hold_gen is %d", complete.HoldGen))

	// Control: a row put back at the gen it had, as a restore at gen 0 (one that does not move above
	// the log) would leave it, accepts a PUT at a gen read before the loss.
	control := h.prepared(holdrecord.WorkflowCutover)
	before := h.row()
	_, err = h.s.Broker.Pool.Exec(ctx, `create table t7_lost_row as select * from nats_hold`)
	require.NoError(t, err)
	h.deleteRow()
	_, err = h.s.Broker.Pool.Exec(ctx, `insert into nats_hold select * from t7_lost_row`)
	require.NoError(t, err)
	_, err = h.s.Records.MoveRow(ctx, before.Gen, natsgrant.OperatorPhaseReleased, control, "a stale writer's PUT")
	require.NoError(t, err, "control: the row put back at its own gen refused the stale PUT")
	t.Logf("control: with the row put back at gen %d, a PUT at the gen read before the loss is accepted", before.Gen)

	// A --bootstrap restore after a loss is refused, since its gen 1 is not the log's next: a hold
	// lost mid-way, which the bootstrap would release with no hold id.
	last := h.prepared(holdrecord.WorkflowCutover)
	h.heldNoSteps(last)
	h.deleteRow()
	code, _, stderr = h.hold("restore", "--bootstrap")
	require.Equal(t, 1, code, "a --bootstrap restore after the loss")
	require.Contains(t, stderr, "gen_mismatch", "the bootstrap's refusal does not say its gen is not the log's next")
	h.rowMissing("the refused bootstrap")
}

// TestHoldACommandOfAnotherWorkflow: a command of another workflow. In a cutover hold, `buckets
// restore` writes no entry and mutates nothing, and stops naming the hold's workflow; in the same
// hold create-bucket, convert and the census each write their entries and run. Control: `streams
// update` in the cutover hold writes nothing and stops, naming the workflow. In a hub-restore hold
// the census after a prove with only an intent writes nothing and stops, naming `hold prove`, and so
// does the census after that prove stopped, which the census's Requires refuse; after a done `hold
// prove` the census writes its entries and runs.
func TestHoldACommandOfAnotherWorkflow(t *testing.T) {
	h := newT7Stack(t)
	h.seed()
	h.scene(false, "ses_w1")
	ctx := context.Background()
	copied := bus.CopiedBuckets()[0]
	stream := "KV_" + copied

	// A backup of a copied bucket, taken under a held row as the operator takes one.
	backup := filepath.Join(t.TempDir(), "backup")
	h.noStepsHold(holdrecord.WorkflowStreamsUpdate, writerhold.CommandStreamsUpdate, func(nc *nats.Conn, _ jetstream.JetStream) {
		_, err := buckets.Backup(ctx, nc, backup, []string{copied}, io.Discard)
		require.NoError(t, err)
	})

	id := h.prepared(holdrecord.WorkflowCutover)
	code, _, stderr := h.names("prepare", "--hold", id)
	require.Zero(t, code, stderr)
	h.heldNoSteps(id)
	streamState := func() (time.Time, uint64) {
		_, js := h.operator()
		defer h.closeConns()
		s, err := js.Stream(ctx, stream)
		require.NoError(t, err)
		return s.CachedInfo().Created, s.CachedInfo().State.LastSeq
	}
	created, last := streamState()
	before := h.entries(id)
	code, _, stderr = h.bucketsRun("restore", "--hold", id, "--bucket", copied, "--from", backup)
	require.Equal(t, 1, code, "buckets restore in a cutover hold")
	require.Contains(t, stderr, "a cutover hold")
	require.Contains(t, stderr, "runs only in a buckets-restore hold")
	require.Equal(t, before, h.entries(id), "buckets restore wrote to the cutover's journal")
	createdAfter, lastAfter := streamState()
	require.Equal(t, created, createdAfter, "buckets restore remade %s", stream)
	require.Equal(t, last, lastAfter, "buckets restore wrote %s", stream)

	code, _, stderr = h.names("create-bucket", "--hold", id, "--replicas", "3")
	require.Zero(t, code, stderr)
	code, _, stderr = h.names("convert", "--hold", id)
	require.Zero(t, code, stderr)
	// Control: streams update in the cutover hold.
	before = h.entries(id)
	code, _, stderr = h.streams("update", "--hold", id)
	require.Equal(t, 1, code, "control: streams update in a cutover hold")
	require.Contains(t, stderr, "a cutover hold")
	require.Contains(t, stderr, "runs only in a streams-update hold")
	require.Equal(t, before, h.entries(id), "control: streams update wrote to the cutover's journal")
	h.census(id, "session")
	require.Equal(t, []string{"hold set intent", "hold set done", "create-bucket intent", "create-bucket done", "convert intent", "convert done",
		"streams census intent", "streams census done"}, h.entries(id))
	code, _, _ = h.hold("release", "--hold", id)
	require.Zero(t, code)

	// hub-restore: the census before a done prove is refused, naming hold prove.
	hub := h.prepared(holdrecord.WorkflowHubRestore)
	h.heldNoSteps(hub)
	_, err := h.s.Records.Admit(ctx, hub, holdrecord.Request{Command: holdrecord.CommandProve, DisksMoved: true}, nil)
	require.NoError(t, err, "a prove's intent")
	before = h.entries(hub)
	code, _, stderr = h.streams(h.censusArgs(hub, "session")...)
	require.Equal(t, 1, code, "the census after a prove with only an intent")
	require.Contains(t, stderr, holdrecord.CommandProve)
	require.Equal(t, before, h.entries(hub), "the refused census wrote to the journal")
	require.NoError(t, h.s.Records.AppendStopLeft(ctx, hub, holdrecord.CommandProve, "the row's prove, which wrote its intent alone", holdrecord.LeftUnchanged))
	before = h.entries(hub)
	code, _, stderr = h.streams(h.censusArgs(hub, "session")...)
	require.Equal(t, 1, code, "the census after a prove that stopped")
	require.Contains(t, stderr, holdrecord.ErrOutOfOrder.Error())
	require.Contains(t, stderr, holdrecord.CommandProve)
	require.Equal(t, before, h.entries(hub), "the refused census wrote to the journal")
	require.Contains(t, holdrecord.Requires(holdrecord.WorkflowHubRestore, writerhold.CommandStreamsCensus), holdrecord.CommandProve,
		"control: the table's Requires of the census, which refused it, do not name hold prove")

	h.disks.Remake(0)
	code, _, _ = h.hold("prove", "--hold", hub, "--restored", holdtest.Claim(0)+"=pvc-"+holdtest.Pod(0))
	require.Zero(t, code, "hold prove of the restored claim")
	h.awaitLeaders()
	h.census(hub, "session")
	require.Equal(t, "done", h.latest(hub, writerhold.CommandStreamsCensus))
	require.True(t, h.journal(hub).Complete)
	code, _, _ = h.hold("release", "--hold", hub)
	require.Zero(t, code)
}

// lastSeq is the sequence of subject's last message in stream, read as the operator.
func (h *t7Stack) lastSeq(stream, subject string) uint64 {
	h.t.Helper()
	nc, _ := h.operator()
	defer h.closeConns()
	body, err := json.Marshal(map[string]string{"last_by_subj": subject})
	require.NoError(h.t, err)
	reply, err := nc.Request("$JS.API.STREAM.MSG.GET."+stream, body, 10*time.Second)
	require.NoError(h.t, err)
	var answer struct {
		Message struct {
			Seq uint64 `json:"seq"`
		} `json:"message"`
	}
	require.NoError(h.t, json.Unmarshal(reply.Data, &answer))
	require.NotZero(h.t, answer.Message.Seq, "no message on %s in %s: %s", subject, stream, reply.Data)
	return answer.Message.Seq
}

// pgCurrent is logical's current name in family's intake_consumers rows on db.
func pgCurrent(t *testing.T, family string, db *pgxpool.Pool, logical string) string {
	t.Helper()
	record, _, err := t7Store(t, family, db).Read(context.Background(), logical)
	require.NoError(t, err, "%s's row", logical)
	return record.Current
}

// TestHoldTheCensusInsideTheCutover: the census inside the cutover. The census takes the bucket
// barrier on envoy_session_consumers before it reads, and keeps a session consumer recorded just
// before it. After convert it removes a planted consumer outside the shapes and a consumer planted
// under a recorded session~<nuid> name with another filter, with each family declared in its fixed
// and fresh forms, and keeps every recorded session~<nuid>, delivery-events~<nuid>,
// delivery-jobs~<nuid> and legion-go-<P>-<kind>~<nuid>. A STREAM.NAMES answer that lacks one stream
// of STREAM.LIST makes the census print UNKNOWN and exit 2, removing nothing. Controls: a census
// that reads the records with no barrier, against a lagging copy that has not applied the last
// record, removes that record's consumer (R21-A); and shapes that list only the fixed names delete
// the fresh ones.
func TestHoldTheCensusInsideTheCutover(t *testing.T) {
	h := newT7Stack(t)
	h.seed()
	ctx := context.Background()
	h.scene(true, "ses_i1")
	const project = "PROJ"
	wake := "legion-go-" + project + "-wake"
	h.fixedOf(jetstream.ConsumerConfig{Durable: wake, FilterSubject: bus.LegionWakeTopic, AckPolicy: jetstream.AckExplicitPolicy})
	daemonDB := h.database("daemon/"+project, daemonMigrations...)
	h.daemonDeployment(project)
	daemon := project + "=" + h.file("daemon/"+project)
	planted := jetstream.ConsumerConfig{Durable: "plant", AckPolicy: jetstream.AckExplicitPolicy, FilterSubject: "notifications.dispatch.issue.PRIV.>"}
	h.fixedOf(planted)
	impostor := selection.FreshName("session")
	h.fixedOf(jetstream.ConsumerConfig{Durable: impostor, AckPolicy: jetstream.AckExplicitPolicy, FilterSubject: "notifications.dispatch.issue.PRIV.>",
		Metadata: map[string]string{selection.LogicalMetadataKey: "ses_i3", bus.SessionIDMetadataKey: "ses_i3"}})
	dispatchDB := h.dbs["dispatch"]

	id := h.heldCutover()
	code, _, stderr := h.names("convert", "--hold", id, "--dispatch-database-file", h.file("dispatch"), "--daemon-database", daemon)
	require.Zero(t, code, "convert: %s", stderr)
	kept := map[string]string{
		h.sessionRecord("ses_i1").Current:                                               "ses_i1's recorded session~<nuid>",
		pgCurrent(t, selection.DispatchFamily, dispatchDB, bus.DispatchDeliveryDurable): "the recorded delivery-events~<nuid>",
		pgCurrent(t, selection.DispatchFamily, dispatchDB, bus.DispatchJobDurable):      "the recorded delivery-jobs~<nuid>",
		pgCurrent(t, "daemon/"+project, daemonDB, wake):                                 "the recorded " + wake + "~<nuid>",
	}
	for name, what := range kept {
		require.Contains(t, name, "~", "%s is no fresh name: %q", what, name)
	}

	// A session recorded just before the census, its consumer made as its record names it, and the
	// impostor's session recorded under the impostor's name.
	nc, _ := h.operator()
	store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	justBefore := selection.FreshName("session")
	config := sessionConsumerConfig(justBefore, "ses_i2")
	config.Metadata = map[string]string{selection.LogicalMetadataKey: "ses_i2", bus.SessionIDMetadataKey: "ses_i2"}
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateConsumer(ctx, bus.Stream, config)
	require.NoError(t, err)
	recordedAt, err := store.Write(ctx, "ses_i2", selection.Record{Current: justBefore, Start: 1}, 0)
	require.NoError(t, err)
	_, err = store.Write(ctx, "ses_i3", selection.Record{Current: impostor, Start: 1}, 0)
	require.NoError(t, err)
	h.closeConns()
	kept[justBefore] = "ses_i2's session~<nuid>, recorded just before the census"

	flags := append(h.censusArgs(id, "session", "dispatch", "daemon/"+project), "--daemon-database", daemon,
		"--daemon-consumer", wake+"="+bus.LegionWakeTopic)

	// STREAM.NAMES lacking one stream of STREAM.LIST: UNKNOWN, exit 2, nothing removed.
	names := []byte(`{"offset":0}`)
	lacking := h.operatorThrough(t7TapDialer{tap: func(p []byte) []byte {
		if bytes.Contains(p, []byte("$JS.API.STREAM.NAMES ")) {
			return bytes.ReplaceAll(p, names, []byte(`{"offset":1}`))
		}
		return p
	}})
	code, stdout, _ := h.streamsThrough(ctx, lacking, censusReaders(), flags...)
	require.Equal(t, 2, code, "the census whose STREAM.NAMES lacks a stream")
	require.Contains(t, stdout, "UNKNOWN")
	require.Contains(t, stdout, "STREAM.NAMES")
	require.NotContains(t, stdout, "CHANGED")
	require.Contains(t, h.consumers(), "plant", "the UNKNOWN census removed the planted consumer")

	code, stdout, _ = h.streams(flags...)
	require.Equal(t, 1, code, "the census that removes the planted consumers")
	require.Contains(t, stdout, "CHANGED "+bus.Stream+" consumer plant: removed")
	require.Contains(t, stdout, "CHANGED "+bus.Stream+" consumer "+impostor+": removed", "the consumer planted under a recorded name with another filter")
	left := h.consumers()
	for name, what := range kept {
		require.Contains(t, left, name, "the census removed %s", what)
	}
	require.Greater(t, h.lastSeq("KV_"+selection.SessionBucket, "$KV."+selection.SessionBucket+".hold.barrier."+id), recordedAt,
		"the census's barrier is not after the record written just before it")
	code, stdout, _ = h.streams(flags...)
	require.Zero(t, code, "the census's second run")
	require.Contains(t, strings.Split(strings.TrimSpace(stdout), "\n"), "CLEAN")
	require.True(t, h.journal(id).Complete)
	code, _, _ = h.hold("release", "--hold", id)
	require.Zero(t, code)

	// The controls, in a streams-census hold of their own, each a census that removes what it
	// should keep.
	control := h.prepared(holdrecord.WorkflowStreamsCensus)
	h.heldNoSteps(control)
	nc, _ = h.operator()
	store = selection.NewKVStore(selection.ConnRequester{Conn: nc})
	lagging := selection.FreshName("session")
	config = sessionConsumerConfig(lagging, "ses_i4")
	config.Metadata = map[string]string{selection.LogicalMetadataKey: "ses_i4", bus.SessionIDMetadataKey: "ses_i4"}
	js, err = jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateConsumer(ctx, bus.Stream, config)
	require.NoError(t, err)
	_, err = store.Write(ctx, "ses_i4", selection.Record{Current: lagging, Start: 1}, 0)
	require.NoError(t, err)
	h.closeConns()
	controlFlags := append(h.censusArgs(control, "session", "dispatch", "daemon/"+project), "--daemon-database", daemon,
		"--daemon-consumer", wake+"="+bus.LegionWakeTopic)

	// R21-A: the records read with no barrier, from a copy that has not applied ses_i4's record.
	noBarrier := censusReaders()
	noBarrier.Recorded = func(ctx context.Context, nc *nats.Conn) ([]streams.ConsumerShape, error) {
		shapes, err := consumernames.RecordedSessionConsumers(ctx, nc)
		return slices.DeleteFunc(shapes, func(s streams.ConsumerShape) bool { return s.Name == lagging }), err
	}
	code, stdout, _ = h.streamsWith(noBarrier, controlFlags...)
	require.Equal(t, 1, code, "control: the census on a lagging copy")
	require.Contains(t, stdout, "CHANGED "+bus.Stream+" consumer "+lagging+": removed",
		"control: a census on a copy that lags the last record keeps its consumer")
	t.Logf("control (R21-A): a census reading a copy that has not applied ses_i4's record removed %s", lagging)

	// Shapes that list only the fixed names: no fresh name recorded.
	fixedOnly := censusReaders()
	fixedOnly.FreshNames = func(context.Context, *nats.Conn, writerhold.DaemonDeployments, []string, string, []string) (writerhold.FreshNamesRead, error) {
		return writerhold.FreshNamesRead{Names: map[string][]string{}}, nil
	}
	events := pgCurrent(t, selection.DispatchFamily, dispatchDB, bus.DispatchDeliveryDurable)
	code, stdout, _ = h.streamsWith(fixedOnly, controlFlags...)
	require.Equal(t, 1, code, "control: the census with the fixed names' shapes alone")
	require.Contains(t, stdout, "CHANGED "+bus.Stream+" consumer "+events+": removed",
		"control: shapes that list only the fixed names keep the fresh %s", events)
	t.Logf("control: shapes that list only the fixed names removed the fresh %s", events)
}

// grantHub is a lone server with JetStream whose no-auth user, the admin, sets the scene, and whose
// user "operator" holds exactly grant: a static user is how a row holds a grant one subject short,
// which the stack's auth service never answers. A grant with no subscribe is written as a deny of
// every subject, since nats-server reads an empty allow as every subject.
func grantHub(t *testing.T, grant natsgrant.Permissions) (string, *nats.Conn) {
	t.Helper()
	quote := func(subjects []string) string {
		quoted := make([]string, len(subjects))
		for i, subject := range subjects {
			quoted[i] = fmt.Sprintf("%q", subject)
		}
		return strings.Join(quoted, ", ")
	}
	subscribe := fmt.Sprintf("allow: [%s]", quote(grant.Subscribe))
	if len(grant.Subscribe) == 0 {
		subscribe = `deny: [">"]`
	}
	config := fmt.Sprintf(`authorization {
  users = [
    { user: "admin", password: "admin" },
    { user: "operator", password: "op-pass", permissions: { publish: { allow: [%s] }, subscribe: { %s } } }
  ]
}
no_auth_user: "admin"
`, quote(grant.Publish), subscribe)
	_, url := testnats.StartConfig(t, "jetstream {}\n"+config, []string{"-js"}, true)
	admin := testnats.Connect(t, url)
	t.Cleanup(admin.Close)
	return url, admin
}

// grantOperator connects to a grantHub as its operator user, with the operator's inbox prefix.
func grantOperator(t *testing.T, url string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url, nats.UserInfo("operator", "op-pass"), nats.CustomInboxPrefix(natsgrant.OperatorInbox), nats.Timeout(10*time.Second))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

// without is grant with entry removed from its publish list, which must hold it.
func without(t *testing.T, grant natsgrant.Permissions, entry string) natsgrant.Permissions {
	t.Helper()
	require.Contains(t, grant.Publish, entry, "the grant names no %s to remove", entry)
	grant.Publish = slices.DeleteFunc(slices.Clone(grant.Publish), func(e string) bool { return e == entry })
	return grant
}

// requireRefused publishes a request to subject on nc and requires the server's refusal of it,
// by name: the server sends its permissions violation before the answer to the flush that follows.
func requireRefused(t *testing.T, nc *nats.Conn, subject string, body []byte, what string) {
	t.Helper()
	require.NoError(t, nc.PublishRequest(subject, nats.NewInbox(), body))
	require.NoError(t, nc.Flush())
	want := fmt.Sprintf("Permissions Violation for Publish to %q", subject)
	last := nc.LastError()
	require.Error(t, last, "%s: %s was not refused", what, subject)
	require.Contains(t, last.Error(), want, "%s: the server did not refuse %s", what, subject)
}

// requireAnswered sends a request to subject on nc and requires a JetStream answer with no error.
func requireAnswered(t *testing.T, nc *nats.Conn, subject string, body []byte, what string) []byte {
	t.Helper()
	reply, err := nc.Request(subject, body, 20*time.Second)
	require.NoError(t, err, "%s: %s", what, subject)
	var answer struct {
		Error *struct {
			Description string `json:"description"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(reply.Data, &answer), "%s: %s", what, subject)
	require.Nil(t, answer.Error, "%s: %s answered %s", what, subject, reply.Data)
	return reply.Data
}

// t7Requests records the requests an operator connection publishes (PUB lines, with their
// payloads), in order, through its tap.
type t7Requests struct {
	mu   sync.Mutex
	list []t7Request
}

type t7Request struct {
	Subject string
	Payload []byte
}

var t7PubHeader = regexp.MustCompile(`(?m)^PUB (\S+) (?:\S+ )?(\d+)\r\n`)

func (r *t7Requests) tap(b []byte) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range t7PubHeader.FindAllSubmatchIndex(b, -1) {
		size, err := strconv.Atoi(string(b[m[4]:m[5]]))
		if err != nil {
			continue
		}
		end := min(m[1]+size, len(b))
		r.list = append(r.list, t7Request{Subject: string(b[m[2]:m[3]]), Payload: bytes.Clone(b[m[1]:end])})
	}
	return b
}

// with are the requests recorded so far whose subject begins with prefix.
func (r *t7Requests) with(prefix string) []t7Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []t7Request
	for _, request := range r.list {
		if strings.HasPrefix(request.Subject, prefix) {
			out = append(out, request)
		}
	}
	return out
}

// TestHoldGrantReleased: OperatorPhaseReleased. Under exactly the released grant (the auth
// service's answer to the operator under a released row): `hold status`, `consumer-names status`,
// the STREAM.INFO reads released lists (ENVOY_NOTIFICATIONS, KV_envoy_session_consumers, each copied
// bucket's and ENVOY_MEASURE) and the MSG.GET walk backup of the selection bucket's records succeed;
// a STREAM.SNAPSHOT of a copied bucket, a create of it, and each read only prepare lists (the
// STREAM.INFO of KV_legion_agents, KV_envoy_dpop_proofs and KV_envoy_webhook_redelivery) are refused.
// Control: without $JS.API.STREAM.MSG.GET.KV_envoy_session_consumers the walk is refused by name. A
// second hold is prepared after a release: hold prepare reads the row and moves it to prepare at its
// gen.
// With the hold rule running and Postgres stopped, the operator is admitted with the released grant
// and a user that expires within 10 minutes, while every other client is refused hold_unknown.
func TestHoldGrantReleased(t *testing.T) {
	h := newT7Stack(t)
	h.seed()
	h.scene(false, "ses_g1")
	ctx := context.Background()
	id := h.heldCutover()
	code, _, stderr := h.names("convert", "--hold", id)
	require.Zero(t, code, stderr)
	h.census(id, "session")
	code, _, _ = h.hold("release", "--hold", id)
	require.Zero(t, code)

	code, stdout, _ := h.hold("status")
	require.Zero(t, code, "hold status under the released grant")
	require.Contains(t, stdout, "phase released")
	// Status reads every family's store, Dispatch's among them, which a migrated store with no row
	// stands for.
	h.database("dispatch", dispatchMigrations...)
	code, _, stderr = h.names("status", "--dispatch-database-file", h.file("dispatch"))
	require.Zero(t, code, "consumer-names status under the released grant: %s", stderr)

	nc, _ := h.operator()
	copied := "KV_" + bus.CopiedBuckets()[0]
	reads := []string{bus.Stream, "KV_" + selection.SessionBucket, streamset.MeasureStream}
	for _, bucket := range bus.CopiedBuckets() {
		reads = append(reads, "KV_"+bucket)
	}
	for _, stream := range reads {
		requireAnswered(t, nc, "$JS.API.STREAM.INFO."+stream, nil, "a read released lists")
	}
	manifest, err := kvrecords.Write(ctx, nc, selection.SessionBucket, filepath.Join(t.TempDir(), "walk"))
	require.NoError(t, err, "the MSG.GET walk of the selection bucket under the released grant")
	t.Logf("the released operator's walk of %s: %+v", selection.SessionBucket, manifest)
	snapshot, err := json.Marshal(map[string]any{"deliver_subject": natsgrant.OperatorInbox + ".snapshot", "chunk_size": 1024})
	require.NoError(t, err)
	requireRefused(t, nc, "$JS.API.STREAM.SNAPSHOT."+copied, snapshot, "a snapshot under the released grant")
	requireRefused(t, nc, "$JS.API.STREAM.CREATE."+copied, []byte(`{"name":"`+copied+`","subjects":["$KV.x.>"]}`), "a create of a copied bucket under the released grant")
	for _, bucket := range []string{bus.LegionAgentBucket, bus.DPoPProofBucket, bus.WebhookRedeliveryBucket} {
		requireRefused(t, nc, "$JS.API.STREAM.INFO.KV_"+bucket, nil, "a read only prepare lists")
	}
	h.closeConns()

	// Control: the walk without its MSG.GET, under the released grant one entry short.
	walk := "$JS.API.STREAM.MSG.GET.KV_" + selection.SessionBucket
	url, admin := grantHub(t, without(t, natsgrant.Operator(natsgrant.OperatorPhaseReleased), walk))
	_, err = selection.CreateSessionBucket(ctx, admin, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	short := grantOperator(t, url)
	_, err = kvrecords.Write(ctx, short, selection.SessionBucket, filepath.Join(t.TempDir(), "walk"))
	require.Error(t, err, "control: the walk without %s", walk)
	require.NoError(t, short.Flush())
	require.Contains(t, fmt.Sprint(err, short.LastError()), fmt.Sprintf("Permissions Violation for Publish to %q", walk), "control: the walk's refusal names no subject")

	// A second hold after a release: prepare reads the row and moves it at its gen.
	before := h.row()
	second := h.prepared(holdrecord.WorkflowCutover)
	after := h.row()
	require.Equal(t, before.Gen+1, after.Gen, "the second hold's prepare did not move the row at the gen it read")
	require.Equal(t, natsgrant.OperatorPhasePrepare, after.Phase)
	code, _, _ = h.hold("abort", "--hold", second)
	require.Zero(t, code)

	// Postgres stopped, the hold rule running: a replica whose clock runs 9 min 45 s behind decides,
	// so a user that expires within 10 minutes of its decision is closed some seconds from now.
	h.s.Auth.Close()
	replica := h.s.StartReplica(t, holdtest.ReplicaConfig{Now: func() time.Time { return time.Now().Add(-9*time.Minute - 45*time.Second) }})
	replica.Postgres.Cut()
	unknown := replica.Decisions("service", "refused", "hold_unknown")
	h.listenerRefused("Postgres stopped")
	require.Greater(t, replica.Decisions("service", "refused", "hold_unknown"), unknown, "a listener with Postgres stopped is not refused hold_unknown")
	closed := make(chan error, 1)
	op, err := nats.Connect(h.s.Cluster.URL(), append(h.s.OperatorCredential(), nats.NoReconnect(), nats.Timeout(10*time.Second),
		nats.ClosedHandler(func(nc *nats.Conn) { closed <- nc.LastError() }))...)
	require.NoError(t, err, "the operator with Postgres stopped")
	connected := time.Now()
	requireAnswered(t, op, "$JS.API.STREAM.INFO."+bus.Stream, nil, "the operator with Postgres stopped")
	requireRefused(t, op, "$JS.API.STREAM.INFO.KV_"+bus.LegionAgentBucket, nil, "the operator with Postgres stopped holds released's grant alone")
	select {
	case err := <-closed:
		lived := time.Since(connected)
		require.Less(t, lived, 10*time.Minute, "the operator's user outlived 10 minutes")
		t.Logf("the operator's user, admitted with Postgres stopped, was closed %s after its connect (%v)", lived.Round(100*time.Millisecond), err)
	case <-time.After(5 * time.Minute):
		op.Close()
		t.Fatal("the operator's user admitted with Postgres stopped did not expire within 10 minutes of its decision")
	}
}

// TestHoldGrantPrepare: OperatorPhasePrepare. `consumer-names prepare` reads B, raises the
// thresholds and writes the cohort record, and every client is still admitted; a spy on the
// command's own connection sees every $JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.* it sends carry
// action update alone. Control: under released, its first threshold update is refused. Under
// exactly the prepare grant, the preflight's complete reads are answered: the STREAM.INFO of every
// protected stream, KV_legion_agents, KV_envoy_dpop_proofs and KV_envoy_webhook_redelivery among
// them. Control: with $JS.API.STREAM.INFO.KV_legion_agents removed from the grant, the preflight
// stops as UNKNOWN, naming that stream.
func TestHoldGrantPrepare(t *testing.T) {
	h := newT7Stack(t)
	h.seed()
	h.scene(false, "ses_p1", "ses_p2")
	ctx := context.Background()

	// Control: under released, the first threshold update.
	_, js := h.operator()
	consumer, err := js.Consumer(ctx, bus.Stream, t7Fixed("ses_p1"))
	require.NoError(t, err)
	config := consumer.CachedInfo().Config
	config.InactiveThreshold = 7 * 24 * time.Hour
	body, err := json.Marshal(map[string]any{"stream_name": bus.Stream, "config": config, "action": "update"})
	require.NoError(t, err)
	nc, _ := h.operator()
	requireRefused(t, nc, "$JS.API.CONSUMER.CREATE."+bus.Stream+"."+t7Fixed("ses_p1"), body, "control: a threshold update under released")
	h.closeConns()

	id := h.prepared(holdrecord.WorkflowCutover)
	spy := &t7Requests{}
	code, stdout, stderr := h.namesThrough(ctx, t7TapDialer{tap: spy.tap}, "prepare", "--hold", id)
	require.Zero(t, code, "consumer-names prepare: %s", stderr)
	require.Contains(t, stdout, "2 thresholds raised")
	creates := spy.with("$JS.API.CONSUMER.CREATE." + bus.Stream + ".")
	require.Len(t, creates, 2, "prepare's consumer writes")
	for _, create := range creates {
		var request struct {
			Action string `json:"action"`
		}
		require.NoError(t, json.Unmarshal(create.Payload, &request), "%s: %s", create.Subject, create.Payload)
		require.Equal(t, "update", request.Action, "prepare sent %s with another action than update", create.Subject)
	}
	require.Positive(t, h.cohortBoundary(id), "prepare's cohort record holds no B")
	_, js = h.operator()
	for _, session := range []string{"ses_p1", "ses_p2"} {
		consumer, err := js.Consumer(ctx, bus.Stream, t7Fixed(session))
		require.NoError(t, err)
		require.Equal(t, 7*24*time.Hour, consumer.CachedInfo().Config.InactiveThreshold, "%s's threshold", t7Fixed(session))
	}
	h.closeConns()
	require.True(t, h.listenerAdmitted(), "a listener was refused while the row is prepare")

	// The preflight's complete reads under exactly the prepare grant.
	nc, _ = h.operator()
	readings, err := writerhold.ReadProtectedStreams(ctx, nc, []string{"KV_" + selection.SessionBucket})
	require.NoError(t, err, "the preflight's reads under the prepare grant")
	require.Len(t, readings, len(writerhold.ProtectedStreams()))
	for _, bucket := range []string{bus.LegionAgentBucket, bus.DPoPProofBucket, bus.WebhookRedeliveryBucket} {
		requireAnswered(t, nc, "$JS.API.STREAM.INFO.KV_"+bucket, nil, "a read prepare lists")
	}
	h.closeConns()

	// Control: the prepare grant without the STREAM.INFO of KV_legion_agents.
	entry := "$JS.API.STREAM.INFO.KV_" + bus.LegionAgentBucket
	url, admin := grantHub(t, without(t, natsgrant.Operator(natsgrant.OperatorPhasePrepare), entry))
	legacy, err := admin.JetStream()
	require.NoError(t, err)
	for _, bucket := range bus.CopiedBuckets() {
		_, err := bus.CreateCopiedBucket(ctx, admin, legacy, bucket, 1)
		require.NoError(t, err)
	}
	require.NoError(t, streamset.EnsureStreams(ctx, admin, streamset.Settings{}))
	_, err = writerhold.ReadProtectedStreams(ctx, grantOperator(t, url), []string{"KV_" + selection.SessionBucket})
	var unknownReading *writerhold.Unknown
	require.ErrorAs(t, err, &unknownReading, "control: the preflight without %s", entry)
	require.Contains(t, err.Error(), "KV_"+bus.LegionAgentBucket, "control: the UNKNOWN does not name the stream")

	code, _, _ = h.hold("abort", "--hold", id)
	require.Zero(t, code)
}

// TestHoldGrantHeld: OperatorPhaseHeld. Under exactly the held grant, create-bucket creates
// envoy_session_consumers and its sentinel with B, and convert deletes F and creates N, its meta
// barrier's stream ENVOY_HOLD_BARRIER created, listed and deleted, as a spy on its own connection
// sees. Control: under the prepare grant, the create-bucket create, the convert delete and the
// barrier create are each refused. Under exactly the held grant, the bucket barrier on
// KV_envoy_sessions (writerhold.ReadBucket) puts hold.barrier.<hold id> in that bucket and reads
// after it, and `buckets backup` and `buckets restore` of it run in their hold. Control: with
// $KV.envoy_sessions.hold.barrier.* removed from the grant, the put gets a permissions violation and
// the read never runs, the refusal naming the subject.
func TestHoldGrantHeld(t *testing.T) {
	h := newT7Stack(t)
	h.seed()
	h.scene(false, "ses_h1")
	ctx := context.Background()
	id := h.prepared(holdrecord.WorkflowCutover)
	code, _, stderr := h.names("prepare", "--hold", id)
	require.Zero(t, code, stderr)

	// Control: the prepare grant refuses each.
	nc, _ := h.operator()
	bucketConfig, err := json.Marshal(map[string]any{"name": "KV_" + selection.SessionBucket, "subjects": []string{"$KV." + selection.SessionBucket + ".>"}})
	require.NoError(t, err)
	requireRefused(t, nc, "$JS.API.STREAM.CREATE.KV_"+selection.SessionBucket, bucketConfig, "control: create-bucket's create under prepare")
	requireRefused(t, nc, "$JS.API.CONSUMER.DELETE."+bus.Stream+"."+t7Fixed("ses_h1"), nil, "control: convert's delete under prepare")
	requireRefused(t, nc, "$JS.API.STREAM.CREATE."+natsgrant.HoldBarrierStream, []byte(`{"name":"`+natsgrant.HoldBarrierStream+`","subjects":["hold.barrier.x"]}`),
		"control: the barrier's create under prepare")
	h.closeConns()
	require.Contains(t, h.consumers(), t7Fixed("ses_h1"))

	h.heldNoSteps(id)
	created := &t7Requests{}
	code, _, stderr = h.namesThrough(ctx, t7TapDialer{tap: created.tap}, "create-bucket", "--hold", id, "--replicas", "3")
	require.Zero(t, code, "create-bucket under the held grant: %s", stderr)
	require.NotEmpty(t, created.with("$JS.API.STREAM.CREATE.KV_"+selection.SessionBucket), "create-bucket sent no create of the bucket")
	state, _ := h.sentinel()
	require.Equal(t, h.cohortBoundary(id), state.Boundary)
	converted := &t7Requests{}
	code, _, stderr = h.namesThrough(ctx, t7TapDialer{tap: converted.tap}, "convert", "--hold", id)
	require.Zero(t, code, "convert under the held grant: %s", stderr)
	var subjects []string
	for _, request := range converted.with("$JS.API.") {
		subjects = append(subjects, request.Subject)
	}
	deleted := slices.Index(subjects, "$JS.API.CONSUMER.DELETE."+bus.Stream+"."+t7Fixed("ses_h1"))
	require.GreaterOrEqual(t, deleted, 0, "convert sent no delete of F: %v", subjects)
	fresh := slices.IndexFunc(subjects, func(s string) bool {
		return strings.HasPrefix(s, "$JS.API.CONSUMER.CREATE."+bus.Stream+".session~")
	})
	require.Greater(t, fresh, deleted, "convert created no N after F's delete: %v", subjects)
	made := slices.Index(subjects, "$JS.API.STREAM.CREATE."+natsgrant.HoldBarrierStream)
	removed := slices.Index(subjects, "$JS.API.STREAM.DELETE."+natsgrant.HoldBarrierStream)
	require.GreaterOrEqual(t, made, 0, "convert's barrier was not created: %v", subjects)
	require.Greater(t, removed, made, "convert's barrier was not deleted after its create: %v", subjects)
	t.Logf("convert's meta barrier, as its connection sent it: %v", subjects[made:removed+1])
	require.NotContains(t, h.consumers(), t7Fixed("ses_h1"))
	h.census(id, "session")
	code, _, _ = h.hold("release", "--hold", id)
	require.Zero(t, code)

	// The bucket barrier on KV_envoy_sessions under exactly the held grant, and its backup and
	// restore in a buckets-restore hold.
	restore := h.prepared(holdrecord.WorkflowBucketsRestore)
	h.heldNoSteps(restore)
	nc, _ = h.operator()
	read := false
	require.NoError(t, writerhold.ReadBucket(ctx, nc, bus.SessionBucket, restore, func(context.Context) error {
		read = true
		return nil
	}), "the bucket barrier on %s under the held grant", bus.SessionBucket)
	require.True(t, read, "the barrier's read did not run")
	h.closeConns()
	_, js := h.operator()
	sessions, err := js.Stream(ctx, "KV_"+bus.SessionBucket)
	require.NoError(t, err)
	barrier := "$KV." + bus.SessionBucket + ".hold.barrier." + restore
	info, err := sessions.Info(ctx, jetstream.WithSubjectFilter(barrier))
	require.NoError(t, err)
	require.Contains(t, info.State.Subjects, barrier, "the barrier key is not in the bucket")
	h.closeConns()
	backup := filepath.Join(t.TempDir(), "backup")
	code, _, _ = h.bucketsRun("backup", "--hold", restore, "--bucket", bus.SessionBucket, "--to", backup)
	require.Zero(t, code, "buckets backup of %s under the held grant", bus.SessionBucket)
	code, _, _ = h.bucketsRun("restore", "--hold", restore, "--bucket", bus.SessionBucket, "--from", backup)
	require.Zero(t, code, "buckets restore of %s under the held grant", bus.SessionBucket)
	code, _, _ = h.hold("release", "--hold", restore)
	require.Zero(t, code)

	// Control: the held grant without the barrier's put.
	put := "$KV." + bus.SessionBucket + ".hold.barrier.*"
	url, admin := grantHub(t, without(t, natsgrant.Operator(natsgrant.OperatorPhaseHeld), put))
	adminJS, err := jetstream.New(admin)
	require.NoError(t, err)
	_, err = adminJS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bus.SessionBucket})
	require.NoError(t, err)
	short := grantOperator(t, url)
	read = false
	err = writerhold.ReadBucket(ctx, short, bus.SessionBucket, "control", func(context.Context) error {
		read = true
		return nil
	})
	require.Error(t, err, "control: the barrier without %s", put)
	require.False(t, read, "control: the read ran after a refused barrier")
	require.NoError(t, short.Flush())
	require.Contains(t, fmt.Sprint(err, short.LastError()), "$KV."+bus.SessionBucket+".hold.barrier.control", "control: the refusal names no subject")
}

// craft writes hold id's journal as change leaves it, signed as the operator's client signs every
// write: a row's model of the journal another version's rule would have written, which no command
// of this build writes.
func (h *t7Stack) craft(id string, change func(*holdrecord.Done)) {
	h.t.Helper()
	ctx := context.Background()
	d, gen, err := h.s.Records.Journal(ctx, id)
	require.NoError(h.t, err)
	change(&d)
	value, err := json.Marshal(d)
	require.NoError(h.t, err)
	_, err = h.s.Records.PutRecord(ctx, id, "done", "hold", gen, value)
	require.NoError(h.t, err)
}

// entered writes command's intent and done entries in hold id, as the command writes its journal
// around a run whose work on the hub the row does not need.
func (h *t7Stack) entered(id, command string) {
	h.t.Helper()
	ctx := context.Background()
	_, err := h.s.Records.Admit(ctx, id, holdrecord.Request{Command: command}, nil)
	require.NoError(h.t, err, "%s's admission", command)
	require.NoError(h.t, h.s.Records.AppendDone(ctx, id, command, nil))
}

// serveRecorded serves r through the routes into w, and answers the status it served.
func (h *t7Stack) serveRecorded(w http.ResponseWriter, r *http.Request) int {
	recorder := httptest.NewRecorder()
	h.s.Broker.Routes().ServeHTTP(recorder, r)
	for key, values := range recorder.Header() {
		w.Header()[key] = values
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
	return recorder.Code
}

// raceAdmissions runs a and b, two operator processes' admissions in one hold, so both read the
// journal before either writes: a's write is held at the routes until b's arrives, then a's lands
// first and b's is served after it. It answers each one's error and the status b's first write was
// answered.
func (h *t7Stack) raceAdmissions(a, b func() error) (error, error, int) {
	h.t.Helper()
	var mu sync.Mutex
	writes, second := 0, 0
	arrived, release, landed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h.s.Broker.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/nats-hold/records" {
			return false
		}
		mu.Lock()
		writes++
		n := writes
		mu.Unlock()
		switch n {
		case 1:
			close(arrived)
			select {
			case <-release:
			case <-time.After(2 * time.Minute):
			}
			h.serveRecorded(w, r)
			close(landed)
		case 2:
			close(release)
			<-landed
			status := h.serveRecorded(w, r)
			mu.Lock()
			second = status
			mu.Unlock()
		default:
			h.serveRecorded(w, r)
		}
		return true
	})
	defer h.s.Broker.Intercept(nil)
	var errA error
	done := make(chan struct{})
	go func() {
		defer close(done)
		errA = a()
	}()
	<-arrived
	errB := b()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return errA, errB, second
}

// TestHoldCompletionPerWorkflow: completion and release, per workflow. cutover, rollback,
// selection-recovery, buckets-create, buckets-recreate, buckets-restore, hub-restore and
// streams-census each run to their final command, which completes the done record, and release
// then succeeds; around them, each clause of the list the hold's journal can show with this build's
// commands, and each control below the subtest that runs it.
func TestHoldCompletionPerWorkflow(t *testing.T) {
	t.Run("cutover", func(t *testing.T) {
		h := newT7Stack(t)
		h.seed()
		h.scene(false, "ses_cp1")
		ctx := context.Background()
		id := h.prepared(holdrecord.WorkflowCutover)
		code, _, stderr := h.names("prepare", "--hold", id)
		require.Zero(t, code, stderr)
		h.heldNoSteps(id)

		// Two processes race for one admission: both read the record, one writes its intent, the
		// other's write is refused 409 gen_mismatch, and its second read is refused ErrBusy. (Two
		// admissions of one command would admit the second as the first's rerun.)
		admit := func(command string) func() error {
			return func() error {
				_, err := h.s.Broker.Client(t).Admit(ctx, id, holdrecord.Request{Command: command}, nil)
				return err
			}
		}
		errA, errB, second := h.raceAdmissions(admit("create-bucket"), admit("replay"))
		require.NoError(t, errA, "the first process's admission")
		require.Equal(t, http.StatusConflict, second, "the second process's write after the first's")
		require.ErrorIs(t, errB, holdrecord.ErrBusy, "the second process's second read")
		require.NotContains(t, h.entries(id), "replay intent", "the refused process wrote its intent")
		code, _, stderr = h.names("create-bucket", "--hold", id, "--replicas", "3")
		require.Zero(t, code, "the admitted process's create-bucket: %s", stderr)
		code, _, stderr = h.names("convert", "--hold", id)
		require.Zero(t, code, stderr)
		require.False(t, h.journal(id).Complete, "convert completed the cutover's record")
		code, _, stderr = h.hold("release", "--hold", id)
		require.Equal(t, 1, code)
		require.Contains(t, stderr, holdrecord.ErrOutOfOrder.Error())

		// The census killed during its final AppendDone, before the write lands: no done entry,
		// release refused, and its rerun is admitted and completes the record.
		killed, kill := h.killAt(entryIs(writerhold.CommandStreamsCensus, "done"), false)
		code, _, _ = h.streamsOn(killed, censusReaders(), h.censusArgs(id, "session")...)
		h.endKill()
		require.True(t, kill.Fired())
		require.NotZero(t, code)
		require.Equal(t, "intent", h.latest(id, writerhold.CommandStreamsCensus))
		code, _, stderr = h.hold("release", "--hold", id)
		require.Equal(t, 1, code, "release over the killed census")
		require.Contains(t, stderr, holdrecord.ErrBusy.Error())
		h.census(id, "session")
		require.True(t, h.journal(id).Complete)

		// After the census completed the record, a replay is refused ErrAlreadyDone and changes
		// nothing, and release succeeds.
		before, record := h.entries(id), h.sessionRecord("ses_cp1")
		code, _, stderr = h.names("replay", "--hold", id, "--key", "ses_cp1", "--from", "1")
		require.Equal(t, 1, code, "a replay after the census completed the record")
		require.Contains(t, stderr, holdrecord.ErrAlreadyDone.Error())
		require.Equal(t, before, h.entries(id))
		require.Equal(t, record, h.sessionRecord("ses_cp1"), "the refused replay wrote the record")
		code, _, _ = h.hold("release", "--hold", id)
		require.Zero(t, code)

		// A hold whose done record lists no operation is released.
		empty := h.prepared(holdrecord.WorkflowCutover)
		h.heldNoSteps(empty)
		code, _, _ = h.hold("release", "--hold", empty)
		require.Zero(t, code, "release of a record that lists no operation")

		// The census killed during its final AppendDone after the write lands: the done entry with
		// complete true, so release succeeds.
		census := h.prepared(holdrecord.WorkflowStreamsCensus)
		h.heldNoSteps(census)
		killed, kill = h.killAt(entryIs(writerhold.CommandStreamsCensus, "done"), true)
		code, _, _ = h.streamsOn(killed, censusReaders(), h.censusArgs(census, "session")...)
		h.endKill()
		require.True(t, kill.Fired())
		require.NotZero(t, code)
		require.Equal(t, "done", h.latest(census, writerhold.CommandStreamsCensus))
		require.True(t, h.journal(census).Complete, "the killed census's done entry did not complete the record")
		code, _, _ = h.hold("release", "--hold", census)
		require.Zero(t, code)

		// A replay that read the record before the census completed it is refused on its second
		// read, ErrAlreadyDone, and release succeeds. The commands' work is not the point, so their
		// entries are written as they write them.
		race := h.prepared(holdrecord.WorkflowCutover)
		h.heldNoSteps(race)
		h.entered(race, "create-bucket")
		h.entered(race, "convert")
		arrived, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		h.s.Broker.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if !entryIs("replay", "intent")(r, body) {
				return false
			}
			held := false
			once.Do(func() { held = true })
			if held {
				close(arrived)
				<-release
			}
			h.serveRecorded(w, r)
			return true
		})
		var errReplay error
		replayed := make(chan struct{})
		go func() {
			defer close(replayed)
			_, errReplay = h.s.Broker.Client(t).Admit(ctx, race, holdrecord.Request{Command: "replay"}, nil)
		}()
		<-arrived
		h.entered(race, writerhold.CommandStreamsCensus)
		close(release)
		<-replayed
		h.s.Broker.Intercept(nil)
		require.ErrorIs(t, errReplay, holdrecord.ErrAlreadyDone, "the replay that read the record before the census completed it")
		require.True(t, h.journal(race).Complete)
		code, _, _ = h.hold("release", "--hold", race)
		require.Zero(t, code)

		// Control: a table that makes convert final lets release through before the census: its
		// done entry, as that table writes it, completes the record.
		require.False(t, holdrecord.Final(holdrecord.WorkflowCutover, "convert"), "the table makes convert final")
		final := h.prepared(holdrecord.WorkflowCutover)
		h.heldNoSteps(final)
		h.entered(final, "create-bucket")
		_, err := h.s.Records.Admit(ctx, final, holdrecord.Request{Command: "convert"}, nil)
		require.NoError(t, err)
		gen := h.row().Gen
		h.craft(final, func(d *holdrecord.Done) {
			d.Ops = append(d.Ops, holdrecord.Operation{Command: "convert", State: "done"})
			d.Complete, d.HoldGen = true, gen
		})
		code, _, _ = h.hold("release", "--hold", final)
		require.Zero(t, code, "control: release after a convert that a table made final")
		t.Logf("control: under a table that makes convert final, release succeeded with no census")

		// Control: the v46 rule, a done entry and then a separate Complete write, killed between the
		// two: the record is incomplete, the rerun is refused ErrAlreadyDone and release is refused.
		v46 := h.prepared(holdrecord.WorkflowStreamsCensus)
		h.heldNoSteps(v46)
		_, err = h.s.Records.Admit(ctx, v46, holdrecord.Request{Command: writerhold.CommandStreamsCensus}, nil)
		require.NoError(t, err)
		h.craft(v46, func(d *holdrecord.Done) {
			d.Ops = append(d.Ops, holdrecord.Operation{Command: writerhold.CommandStreamsCensus, State: "done"})
			d.Complete = false
		})
		_, err = h.s.Records.Admit(ctx, v46, holdrecord.Request{Command: writerhold.CommandStreamsCensus}, nil)
		require.ErrorIs(t, err, holdrecord.ErrAlreadyDone, "control: the census's rerun under v46's two writes")
		code, _, stderr = h.hold("release", "--hold", v46)
		require.Equal(t, 1, code, "control: release under v46's two writes")
		require.Contains(t, stderr, holdrecord.ErrOutOfOrder.Error())
		require.False(t, h.journal(v46).Complete)
		t.Logf("control: v46's done entry and separate Complete write, killed between the two, leave hold %s held for good", v46)
	})

	t.Run("selection-recovery and streams-census", func(t *testing.T) {
		h := newT7Stack(t)
		h.seed()
		h.scene(false, "ses_sr1")
		cutover := h.heldCutover()
		code, _, stderr := h.names("convert", "--hold", cutover)
		require.Zero(t, code, stderr)
		h.census(cutover, "session")
		code, _, _ = h.hold("release", "--hold", cutover)
		require.Zero(t, code)
		h.loseSelectionBucket()

		id := h.prepared(holdrecord.WorkflowSelectionRecovery)
		h.heldNoSteps(id)
		code, _, stderr = h.names("create-bucket", "--hold", id, "--after-loss", "--replicas", "3")
		require.Zero(t, code, stderr)
		require.True(t, h.journal(id).Complete, "create-bucket --after-loss did not complete the record")
		// Two cycles of replay and replay --final: each replay is admitted after completion and
		// sets complete false, so release is refused, and each replay --final completes it again.
		for cycle := 1; cycle <= 2; cycle++ {
			code, _, stderr = h.names("replay", "--hold", id, "--key", "ses_sr1", "--from", "2")
			require.Zero(t, code, "cycle %d's replay: %s", cycle, stderr)
			require.False(t, h.journal(id).Complete, "cycle %d's replay left the record complete", cycle)
			code, _, stderr = h.hold("release", "--hold", id)
			require.Equal(t, 1, code, "cycle %d: release after a replay", cycle)
			require.Contains(t, stderr, holdrecord.ErrOutOfOrder.Error())
			code, _, stderr = h.names("replay", "--hold", id, "--key", "ses_sr1", "--from", "2", "--final")
			require.Zero(t, code, "cycle %d's replay --final: %s", cycle, stderr)
			require.True(t, h.journal(id).Complete, "cycle %d's replay --final did not complete the record", cycle)
		}
		require.True(t, holdrecord.Repeatable(holdrecord.WorkflowSelectionRecovery, "replay --final"),
			"control: replay --final is not Repeatable, as v52's table made it, under which the second cycle never completes")
		code, _, _ = h.hold("release", "--hold", id)
		require.Zero(t, code)

		// hold release killed after its admission, on a record with no other entry, runs again and
		// releases.
		rel := h.prepared(holdrecord.WorkflowCutover)
		h.heldNoSteps(rel)
		killed, kill := h.killAt(entryIs(holdrecord.CommandRelease, "intent"), true)
		code, _, _ = h.holdOn(killed, "release", "--hold", rel)
		h.endKill()
		require.True(t, kill.Fired())
		require.NotZero(t, code)
		require.Equal(t, natsgrant.OperatorPhaseHeld, h.row().Phase)
		code, _, _ = h.hold("release", "--hold", rel)
		require.Zero(t, code, "release's rerun after its admission")
		require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)

		// streams-census: the census with no --hold sets a hold of its own, every step on the hub;
		// the recovery's replay left the session's fresh consumer unselected, which the first run
		// removes and stops on, and the clean run's done entry completes the record.
		code, _, _ = h.streams("census", "--family", "session")
		census := h.row().HoldID
		require.Equal(t, holdrecord.WorkflowStreamsCensus, h.journal(census).Workflow, "the census with no --hold set no streams-census hold (exit %d)", code)
		require.Equal(t, "done", h.latest(census, holdrecord.CommandSet), "the census's own hold set")
		h.census(census, "session")
		require.True(t, h.journal(census).Complete)
		code, _, _ = h.hold("release", "--hold", census)
		require.Zero(t, code)
	})

	t.Run("rollback", func(t *testing.T) {
		h := newT7Stack(t)
		h.seed()
		h.scene(false, "ses_rb1", "ses_rb2")
		ctx := context.Background()
		cutover := h.heldCutover()
		code, _, stderr := h.names("convert", "--hold", cutover)
		require.Zero(t, code, stderr)
		h.census(cutover, "session")
		code, _, _ = h.hold("release", "--hold", cutover)
		require.Zero(t, code)

		id := h.prepared(holdrecord.WorkflowRollback)
		h.heldNoSteps(id)
		// A logical name with no start: ses_rb2's fresh consumer is gone, and its record saves no
		// start and no floor of it.
		nc, js := h.operator()
		store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
		record, revision, err := store.Read(ctx, "ses_rb2")
		require.NoError(t, err)
		require.NoError(t, js.DeleteConsumer(ctx, bus.Stream, record.Current))
		record.Start, record.LastFloor = 0, nil
		_, err = store.Write(ctx, "ses_rb2", record, revision)
		require.NoError(t, err)
		h.closeConns()

		code, _, stderr = h.names("restore", "--hold", id, "--prepare")
		require.Zero(t, code, "restore --prepare: %s", stderr)
		prepared := h.journal(id).Ops
		last := prepared[len(prepared)-1]
		require.Equal(t, "restore --prepare done", last.Command+" "+last.State, "restore --prepare wrote no done entry")
		require.Contains(t, last.Outputs["no_start"], "ses_rb2", "restore --prepare did not mark the name with no start")
		code, _, stderr = h.names("restore", "--hold", id, "--finish", "--probe-token-file", "/dev/null")
		require.NotZero(t, code, "restore --finish with a name with no start")
		require.Contains(t, stderr, "ses_rb2")
		require.Equal(t, "stop", h.latest(id, "restore --finish"))
		require.True(t, holdrecord.Admits(holdrecord.WorkflowRollback, "restore --key"),
			"control: rollback's table does not admit restore --key, as v52's did not, under which the hold never completes")
		code, _, stderr = h.names("restore", "--hold", id, "--key", "ses_rb2", "--from", "1")
		require.Zero(t, code, "restore --key: %s", stderr)
		code, _, stderr = h.names("restore", "--hold", id, "--finish", "--probe-token-file", "/dev/null")
		require.Zero(t, code, "restore --finish after restore --key: %s", stderr)
		h.awaitLeaders()
		require.True(t, h.journal(id).Complete, "restore --finish did not complete the record")
		code, _, _ = h.hold("release", "--hold", id)
		require.Zero(t, code)
		left := h.consumers()
		for _, session := range []string{"ses_rb1", "ses_rb2"} {
			require.Contains(t, left, t7Fixed(session), "the rollback did not make %s again", t7Fixed(session))
		}
	})

	t.Run("hub-restore", func(t *testing.T) {
		h := newT7Stack(t)
		h.seed()
		h.scene(false, "ses_hr1")
		cutover := h.heldCutover()
		code, _, stderr := h.names("convert", "--hold", cutover)
		require.Zero(t, code, stderr)
		h.census(cutover, "session")
		code, _, _ = h.hold("release", "--hold", cutover)
		require.Zero(t, code)

		id := h.prepared(holdrecord.WorkflowHubRestore)
		h.heldNoSteps(id)
		restored := func(i int) string { return holdtest.Claim(i) + "=pvc-" + holdtest.Pod(i) }
		// A prove run before the restore lands, the claim still on its step-1 disk, is refused,
		// naming the claim.
		code, _, stderr = h.hold("prove", "--hold", id, "--restored", restored(0))
		require.Equal(t, 1, code, "a prove before the restore lands")
		require.Contains(t, stderr, holdtest.Claim(0)+" is still on its step-1 disk")
		require.Equal(t, "stop", h.latest(id, holdrecord.CommandProve))
		h.disks.Remake(0)
		// A hold prove killed after its intent: release is refused.
		killed, kill := h.killAt(entryIs(holdrecord.CommandProve, "intent"), true)
		code, _, _ = h.holdOn(killed, "prove", "--hold", id, "--restored", restored(0))
		h.endKill()
		require.True(t, kill.Fired())
		require.NotZero(t, code)
		code, _, stderr = h.hold("release", "--hold", id)
		require.Equal(t, 1, code, "release after a killed prove")
		require.Contains(t, stderr, holdrecord.ErrBusy.Error())
		// A prove of a mistyped disk is refused and stops; the corrected prove is admitted and
		// proves it.
		code, _, stderr = h.hold("prove", "--hold", id, "--restored", holdtest.Claim(0)+"=pvc-mistyped")
		require.Equal(t, 1, code, "a prove of a mistyped disk")
		require.Contains(t, stderr, "pvc-mistyped")
		require.Equal(t, "stop", h.latest(id, holdrecord.CommandProve))
		// A second claim moved off its step-1 disk with no --restored for it stops the prove, naming
		// it; the prove that names both proves them.
		h.disks.Remake(2)
		code, _, stderr = h.hold("prove", "--hold", id, "--restored", restored(0))
		require.Equal(t, 1, code, "a prove that does not name a moved claim")
		require.Contains(t, stderr, holdtest.Claim(2))
		require.Equal(t, "stop", h.latest(id, holdrecord.CommandProve))
		code, _, _ = h.hold("prove", "--hold", id, "--restored", restored(0), "--restored", restored(2))
		require.Zero(t, code, "the corrected prove")
		h.awaitLeaders()
		h.census(id, "session")
		require.True(t, h.journal(id).Complete)
		// After completion, a prove with no disk moved is refused.
		before := h.entries(id)
		code, _, _ = h.hold("prove", "--hold", id, "--restored", restored(0))
		require.Equal(t, 1, code, "a prove with no disk moved after completion")
		require.Equal(t, before, h.entries(id))
		// A restore that lands after the done prove and the census: release is refused naming the
		// disk, a prove of it is admitted, a new census follows, and release succeeds.
		h.disks.Remake(1)
		code, _, stderr = h.hold("release", "--hold", id)
		require.Equal(t, 1, code, "release over a restore after the census")
		require.Contains(t, stderr, holdtest.Claim(1))
		require.Equal(t, "stop", h.latest(id, holdrecord.CommandRelease))
		// The prove names every target off its step-1 disk: a target the latest prove proved is
		// expected on its step-1 disk again by any prove that does not name it (R28-B).
		code, _, _ = h.hold("prove", "--hold", id, "--restored", restored(0), "--restored", restored(1), "--restored", restored(2))
		require.Zero(t, code, "the prove of the restore after the census")
		h.awaitLeaders()
		require.False(t, h.journal(id).Complete, "the new prove left the record complete")
		h.census(id, "session")
		require.True(t, h.journal(id).Complete)
		code, _, _ = h.hold("release", "--hold", id)
		require.Zero(t, code)
	})

	t.Run("buckets-create, buckets-recreate and buckets-restore", func(t *testing.T) {
		h := newT7Stack(t)
		h.seed()
		copied := bus.CopiedBuckets()[0]
		// The hub after its cutover holds the selection bucket, which no buckets workflow declares
		// absent; one copied bucket is lost.
		h.noStepsHold(holdrecord.WorkflowStreamsUpdate, writerhold.CommandStreamsUpdate, func(nc *nats.Conn, js jetstream.JetStream) {
			_, err := selection.CreateSessionBucket(context.Background(), nc, 3, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
			require.NoError(t, err)
			require.NoError(t, js.DeleteStream(context.Background(), "KV_"+copied))
		})
		h.awaitLeaders()
		code, _, _ := h.bucketsRun("create", "--bucket", copied, "--replicas", "3")
		require.Zero(t, code, "buckets create")
		id := h.row().HoldID
		require.Equal(t, holdrecord.WorkflowBucketsCreate, h.journal(id).Workflow)
		require.True(t, h.journal(id).Complete)
		code, _, _ = h.hold("release", "--hold", id)
		require.Zero(t, code)
		h.awaitLeaders()

		for _, w := range []string{"recreate", "restore"} {
			dir := filepath.Join(t.TempDir(), w)
			code, _, _ = h.bucketsRun("backup", "--workflow", w, "--bucket", copied, "--to", dir)
			require.Zero(t, code, "buckets backup --workflow %s", w)
			id = h.row().HoldID
			require.False(t, h.journal(id).Complete, "the backup completed the %s hold", w)
			args := []string{w, "--hold", id, "--bucket", copied}
			if w == "recreate" {
				args = append(args, "--replicas", "3")
			} else {
				args = append(args, "--from", dir)
			}
			code, _, _ = h.bucketsRun(args...)
			require.Zero(t, code, "buckets %s", w)
			require.True(t, h.journal(id).Complete, "buckets %s did not complete its record", w)
			code, _, _ = h.hold("release", "--hold", id)
			require.Zero(t, code)
			h.awaitLeaders()
		}
	})
}

// TestHoldAHubRestoreReadsARetiredProjectsStoreGone: a hub-restore hold whose restore is proved,
// with the cutover row of a project retired after the fresh-name cutover present (daemon/RETIRED:
// no Deployment, its store's address in its entry of ConfigMap
// envoy-operator/envoy-operator-retired-stores, keys RETIRED.database and RETIRED.instance, its
// database dropped), reconciles the daemon stores of every cutover row through the one reader of
// their addresses, writerhold.KubeDaemonDeployments of the hold's own client, with consumernames'
// writer (ReconcileDaemonStreamIDs), as the census inside the hold calls it: it reads the retired
// store gone, prints `GONE daemon/RETIRED: ...`, names gone.daemon/RETIRED in its done entry, which
// completes the record, and the hold is released. It fails for a reader that never reads the
// ConfigMap, as the reader before KubeDaemonDeployments read it did: daemon/RETIRED is then
// unreconciled ("0 Deployments of namespace legion name project RETIRED"), and the census is
// UNKNOWN.
func TestHoldAHubRestoreReadsARetiredProjectsStoreGone(t *testing.T) {
	h := newT7Stack(t)
	h.seed()
	h.scene(false, "ses_r1")
	ctx := context.Background()
	cutover := h.prepared(holdrecord.WorkflowCutover)
	code, _, stderr := h.names("prepare", "--hold", cutover)
	require.Zero(t, code, stderr)
	h.heldNoSteps(cutover)
	code, _, stderr = h.names("create-bucket", "--hold", cutover, "--replicas", "3")
	require.Zero(t, code, stderr)
	code, _, stderr = h.names("convert", "--hold", cutover)
	require.Zero(t, code, stderr)
	h.census(cutover, "session")
	code, _, _ = h.hold("release", "--hold", cutover)
	require.Zero(t, code)

	hub := h.prepared(holdrecord.WorkflowHubRestore)
	h.heldNoSteps(hub)
	h.disks.Remake(0)
	code, _, _ = h.hold("prove", "--hold", hub, "--restored", holdtest.Claim(0)+"=pvc-"+holdtest.Pod(0))
	require.Zero(t, code, "hold prove of the restored claim")
	h.awaitLeaders()
	_, err := h.s.Records.PutCutover(ctx, "daemon/RETIRED", selection.CutoverDone, hub)
	require.NoError(t, err)
	dropped, file := h.droppedStore("RETIRED")
	_, err = h.s.Kube.Client.CoreV1().ConfigMaps("envoy-operator").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "envoy-operator", Name: "envoy-operator-retired-stores"},
		Data:       map[string]string{"RETIRED.database": dropped, "RETIRED.instance": t7Instance},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	families := slices.Sorted(maps.Keys(h.cutovers()))
	require.Contains(t, families, "daemon/RETIRED")

	code, stdout, stderr := h.streams(append(h.censusArgs(hub, families...), "--daemon-database", "RETIRED="+file)...)
	require.Zero(t, code, "the hub-restore census beside a retired project's store: %s%s", stdout, stderr)
	gone := "database " + dropped + " on instance " + t7Instance + " (the operator's file states " + t7Instance + "): "
	require.Contains(t, stdout, "GONE daemon/RETIRED: "+gone)
	require.True(t, slices.Contains(strings.Split(strings.TrimSpace(stdout), "\n"), "CLEAN"), stdout)
	d := h.journal(hub)
	done := d.Ops[len(d.Ops)-1]
	require.Equal(t, writerhold.CommandStreamsCensus+" done", done.Command+" "+done.State)
	require.True(t, strings.HasPrefix(done.Outputs["gone.daemon/RETIRED"], gone), "%q", done.Outputs["gone.daemon/RETIRED"])
	require.Contains(t, done.Outputs["gone.daemon/RETIRED"], "SQLSTATE 3D000")
	require.True(t, d.Complete)
	code, _, _ = h.hold("release", "--hold", hub)
	require.Zero(t, code)
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)
}

// droppedStore is a daemon store's file as --daemon-database names one for project, whose database
// no server holds (the server answers SQLSTATE 3D000 for it, as for a database dropped with its
// project), on the test's Postgres server, with t7Instance on its second line; it answers the
// database's name and the file.
func (h *t7Stack) droppedStore(project string) (string, string) {
	h.t.Helper()
	config, err := pgxpool.ParseConfig(os.Getenv("BROKER_TEST_DATABASE_URL"))
	require.NoError(h.t, err)
	c := config.ConnConfig
	dropped := "t7rows_dropped_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	url := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable&connect_timeout=2", c.User, c.Password, net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port))), dropped)
	file := filepath.Join(h.t.TempDir(), strings.ToLower(project)+".url")
	require.NoError(h.t, os.WriteFile(file, []byte(url+"\n"+t7Instance+"\n"), 0o600))
	return dropped, file
}
