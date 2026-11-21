package writerhold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// heldStack is a command row's surrounding: holdtest's whole stack (the broker's real hold routes
// on a test Postgres, the callout cluster, the real auth service reading the row, the hub's
// Kubernetes objects with the chart's render), the Operator a run of `envoy-infra hold` reaches
// the hub through, the disks behind the claims, and the env that hands both to run.
type heldStack struct {
	t     *testing.T
	s     *holdtest.Stack
	op    *Operator
	disks *holdtest.Disks
	keys  *memoryKeys
	e     env
	// report is the minter report's path: a report of now with no finding.
	report string
}

// stackDisks is Disks over holdtest's disks, by the disk's name.
type stackDisks struct{ *holdtest.Disks }

func (d stackDisks) DiskID(_ context.Context, disk DiskRef) (uint64, error) { return d.ID(disk.Name) }

// memoryKeys is the signing key's store, in memory: Put adds the first version only, as the Secret
// Manager store does.
type memoryKeys struct {
	mu       sync.Mutex
	versions map[string][][]byte
}

func (m *memoryKeys) Put(_ context.Context, name string, value []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.versions[name]) > 0 {
		return "", errors.New("the secret already holds version 1")
	}
	m.versions[name] = append(m.versions[name], append([]byte(nil), value...))
	return "1", nil
}

func (m *memoryKeys) Current(_ context.Context, name string) (string, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.versions[name]
	if len(v) == 0 {
		return "", nil, errors.New("no version")
	}
	return fmt.Sprint(len(v)), append([]byte(nil), v[len(v)-1]...), nil
}

func newHeldStack(t *testing.T) *heldStack {
	t.Helper()
	return newHeldStackWith(t, holdtest.Options{})
}

func newHeldStackWith(t *testing.T, opts holdtest.Options) *heldStack {
	t.Helper()
	s := holdtest.StartWith(t, opts)
	disks := holdtest.NewDisks(len(s.Cluster.Servers))
	op := &Operator{
		Hub: KubeHub{Client: s.Kube.Client, Namespace: holdtest.Namespace, StatefulSet: holdtest.StatefulSet,
			ConfigMap: holdtest.ConfigMap, Forward: s.Kube},
		Disks:        stackDisks{disks},
		URLs:         s.Cluster.URLs(),
		Connect:      s.ConnectOperator,
		Probe:        s.DialProbe,
		RestartLimit: 3 * time.Minute,
	}
	h := &heldStack{t: t, s: s, op: op, disks: disks, keys: &memoryKeys{versions: map[string][][]byte{}}}
	h.e = env{
		records: func(context.Context, options, bool) (*holdrecord.Client, error) { return s.Broker.Client(t), nil },
		keys:    func(context.Context) (KeyStore, error) { return h.keys, nil },
		hub:     func(context.Context, options) (*Operator, error) { return op, nil },
		now:     time.Now,
	}
	h.acceptMinters(t)
	return h
}

// acceptMinters writes the accepted list, empty, and a minter report of now with no finding.
func (h *heldStack) acceptMinters(t *testing.T) {
	t.Helper()
	_, err := h.s.Kube.Client.CoreV1().ConfigMaps(AcceptedMintersNamespace).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AcceptedMintersConfigMap, Namespace: AcceptedMintersNamespace},
		Data:       map[string]string{AcceptedMintersKey: "[]"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	h.report = filepath.Join(t.TempDir(), "minters.json")
	report, err := json.Marshal(map[string]any{"run_at": time.Now().UTC().Format(time.RFC3339), "findings": []any{}, "unknown": []any{}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(h.report, report, 0o600))
}

// commonArgs are the flags every writing command takes, which the row's env stands in for.
var commonArgs = []string{"--broker-url", "https://broker.holdtest", "--token-file", "/dev/null",
	"--verify-keys", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "--signing-key-secret", "projects/holdtest/secrets/" + SigningKeySecret}

// run runs `envoy-infra hold <args>` in the stack, with the common flags and, for prepare and set,
// the minter report, and returns its exit code and output.
func (h *heldStack) run(args ...string) (int, string, string) {
	h.t.Helper()
	full := append(append([]string{}, args...), commonArgs...)
	if args[0] == actionPrepare || args[0] == actionSet {
		full = append(full, "--minters-report", h.report)
	}
	if args[0] == actionSet || args[0] == actionProve {
		full = append(full, "--nats", h.s.Cluster.URL(), "--probe-token-file", "/dev/null")
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), full, &stdout, &stderr, h.e)
	h.t.Logf("envoy-infra hold %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// row reads the hold row.
func (h *heldStack) row() holdrecord.Row {
	h.t.Helper()
	row, err := h.s.Records.Row(context.Background())
	require.NoError(h.t, err)
	return row
}

// prepared prepares a hold of workflow and returns its id.
func (h *heldStack) prepared(w holdrecord.Workflow) string {
	h.t.Helper()
	code, stdout, _ := h.run(actionPrepare, "--workflow", string(w), "--reason", "the row's hold")
	require.Zero(h.t, code)
	id := strings.TrimSpace(stdout)
	require.NotEmpty(h.t, id)
	return id
}

// heldWithoutTheSteps moves a prepared hold to held the way `hold set` writes its journal, without
// the steps it runs against the hub (holdtest's HoldWithoutTheSteps).
func (h *heldStack) heldWithoutTheSteps(id string) {
	h.t.Helper()
	h.s.HoldWithoutTheSteps(h.t, id)
}

// listenerAdmitted reports whether a listener entry's connection is admitted to the hub now.
func (h *heldStack) listenerAdmitted() bool {
	h.t.Helper()
	nc, err := nats.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(holdtest.ListenerMachines[0]), nats.NoReconnect(), nats.Timeout(10*time.Second))
	if err == nil {
		nc.Close()
		return true
	}
	require.ErrorIs(h.t, err, nats.ErrAuthorization, "the listener's connect failed for another reason")
	return false
}

// A second hold is refused while one is prepared or held; a held command with a missing or stale
// hold id is refused; and a write of the row at a stale gen is refused 409 gen_mismatch, leaving the
// row as it was. The control is the same prepare after the first hold's release, which is accepted.
// It fails when prepare moves a row that is not released, or a command acts on a hold id that is
// not the row's.
func TestHoldASecondHoldAndStaleIds(t *testing.T) {
	h := newHeldStack(t)
	first := h.prepared(holdrecord.WorkflowBucketsCreate)

	code, _, stderr := h.run(actionPrepare, "--workflow", string(holdrecord.WorkflowBucketsCreate), "--reason", "a second hold")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "a second hold is refused")
	require.Equal(t, first, h.row().HoldID, "the second prepare moved the row")

	code, _, stderr = h.run(actionSet, "--workflow", string(holdrecord.WorkflowBucketsCreate), "--reason", "a second hold by set")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "a second hold is refused")

	for _, args := range [][]string{
		{actionRelease, "--hold", "stale-hold-id", "--reason", "stale"},
		{actionAbort, "--hold", "stale-hold-id", "--reason", "stale"},
		{actionSet, "--hold", "stale-hold-id", "--reason", "stale"},
	} {
		code, _, stderr := h.run(args...)
		require.Equal(t, 1, code, "%v", args)
		require.Contains(t, stderr, "stale-hold-id", "%v", args)
		require.Equal(t, first, h.row().HoldID, "%v moved the row", args)
	}
	code, _, stderr = h.run(actionRelease, "--reason", "no hold named")
	require.Equal(t, 2, code)
	require.Contains(t, stderr, "--hold is required")
	require.Equal(t, first, h.row().HoldID)

	row := h.row()
	_, err := h.s.Records.MoveRow(context.Background(), row.Gen-1, natsgrant.OperatorPhaseReleased, first, "a stale gen")
	var refused *holdrecord.RouteError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "gen_mismatch", refused.Code)
	require.Equal(t, row, h.row(), "the stale write moved the row")

	// The control: once the first hold is aborted, a prepare is accepted.
	code, _, _ = h.run(actionAbort, "--hold", first, "--reason", "end the first")
	require.Zero(t, code)
	second := h.prepared(holdrecord.WorkflowBucketsCreate)
	require.NotEqual(t, first, second)
}

// release is refused unless the hold's journal is complete for its workflow or holds no entry but
// hold set's and its own, so nothing changed: a release after a command that is not its workflow's
// final one is refused ErrOutOfOrder, and so is one while another command's intent is pending
// (ErrBusy); after the final command's done entry, which completes the record, release succeeds.
// The control is a hold whose journal holds hold set's entries alone, which releases. It fails when
// release moves the row over a journal that is not complete.
func TestHoldReleaseRefusedUntilComplete(t *testing.T) {
	h := newHeldStack(t)
	ctx := context.Background()

	nothingChanged := h.prepared(holdrecord.WorkflowBucketsCreate)
	h.heldWithoutTheSteps(nothingChanged)
	code, _, _ := h.run(actionRelease, "--hold", nothingChanged, "--reason", "nothing changed")
	require.Zero(t, code, "a hold with nothing changed is released")
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)

	pendingHold := h.prepared(holdrecord.WorkflowCutover)
	h.heldWithoutTheSteps(pendingHold)
	_, err := h.s.Records.Admit(ctx, pendingHold, holdrecord.Request{Command: "create-bucket"}, nil)
	require.NoError(t, err)
	code, _, stderr := h.run(actionRelease, "--hold", pendingHold, "--reason", "over a pending intent")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, holdrecord.ErrBusy.Error())
	require.NoError(t, h.s.Records.AppendDone(ctx, pendingHold, "create-bucket", nil))
	code, _, stderr = h.run(actionRelease, "--hold", pendingHold, "--reason", "before the final command")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, holdrecord.ErrOutOfOrder.Error())
	require.Equal(t, natsgrant.OperatorPhaseHeld, h.row().Phase, "release moved the row over an incomplete journal")
	require.NoError(t, abortHeldForTest(ctx, h, pendingHold))

	complete := h.prepared(holdrecord.WorkflowBucketsCreate)
	h.heldWithoutTheSteps(complete)
	_, err = h.s.Records.Admit(ctx, complete, holdrecord.Request{Command: "buckets create"}, nil)
	require.NoError(t, err)
	code, _, stderr = h.run(actionRelease, "--hold", complete, "--reason", "before the final done")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, holdrecord.ErrBusy.Error())
	require.NoError(t, h.s.Records.AppendDone(ctx, complete, "buckets create", nil))
	code, stdout, _ := h.run(actionRelease, "--hold", complete, "--reason", "complete")
	require.Zero(t, code)
	require.Contains(t, stdout, "hold_gen")
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)
}

// abortHeldForTest releases a held hold left incomplete by a row, as a database admin would end it
// for the test's next hold: through the routes, with the operator's key, outside the journal.
func abortHeldForTest(ctx context.Context, h *heldStack, id string) error {
	row := h.row()
	_, err := h.s.Records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseReleased, id, "the row's cleanup")
	return err
}

// release's write: after it, the row is released at a gen one higher and a listener is admitted
// (refused while the row was held). A release killed after its PUT and before it printed, run again
// on the released row, sends no PUT and exits 0. The control is that second PUT sent anyway, which
// the route refuses 400 bad_transition: so the rerun's rule is what lets a crashed release finish.
func TestHoldReleasesWrite(t *testing.T) {
	h := newHeldStack(t)
	id := h.prepared(holdrecord.WorkflowBucketsCreate)
	h.heldWithoutTheSteps(id)
	require.False(t, h.listenerAdmitted(), "a listener was admitted under a held row")
	before := h.row()

	var puts atomic.Int64
	h.s.Broker.Intercept(func(_ http.ResponseWriter, req *http.Request) bool {
		if req.Method == http.MethodPut && req.URL.Path == "/v1/nats-hold" {
			puts.Add(1)
		}
		return false
	})
	code, _, _ := h.run(actionRelease, "--hold", id, "--reason", "release")
	require.Zero(t, code)
	after := h.row()
	require.Equal(t, natsgrant.OperatorPhaseReleased, after.Phase)
	require.Equal(t, before.Gen+1, after.Gen)
	require.Equal(t, int64(1), puts.Load())
	require.True(t, h.listenerAdmitted(), "a listener was refused after the release")
	last := h.lastEntry(id)
	require.Equal(t, holdrecord.CommandRelease+" done", last.Command+" "+last.State, "the release wrote no done entry, so the hold never ended")

	code, stdout, _ := h.run(actionRelease, "--hold", id, "--reason", "the rerun")
	require.Zero(t, code)
	require.Contains(t, stdout, "already released")
	require.Equal(t, int64(1), puts.Load(), "the rerun sent a PUT")
	require.Equal(t, after, h.row())

	_, err := h.s.Records.MoveRow(context.Background(), after.Gen, natsgrant.OperatorPhaseReleased, id, "the control's second PUT")
	var refused *holdrecord.RouteError
	require.ErrorAs(t, err, &refused, "the control: a second PUT of released was accepted")
	require.Equal(t, "bad_transition", refused.Code)
}
