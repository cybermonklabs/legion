package consumernames

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// wiringOpeners are hubOpeners over the stack that record the writerhold.HubFlags each opener is
// given, so a row can tell that a run's flags reach the broker, the hub's NATS and the hub's
// Kubernetes objects the way the real env reaches them (envFromFlags, realHubOpeners).
type wiringOpeners struct {
	h        *stack
	mu       sync.Mutex
	records  []writerhold.HubFlags
	signing  []bool
	connects []writerhold.HubFlags
	hubs     []writerhold.HubFlags
}

func (w *wiringOpeners) openers(hub func(writerhold.HubFlags, context.Context) (*writerhold.Operator, error)) hubOpeners {
	return hubOpeners{
		records: func(f writerhold.HubFlags, ctx context.Context, signing bool) (*holdrecord.Client, error) {
			w.mu.Lock()
			w.records, w.signing = append(w.records, f), append(w.signing, signing)
			w.mu.Unlock()
			return w.h.e.records(ctx, options{}, signing)
		},
		connect: func(f writerhold.HubFlags) (*nats.Conn, error) {
			w.mu.Lock()
			w.connects = append(w.connects, f)
			w.mu.Unlock()
			return w.h.e.connect(context.Background(), options{})
		},
		hub: func(f writerhold.HubFlags, ctx context.Context) (*writerhold.Operator, error) {
			w.mu.Lock()
			w.hubs = append(w.hubs, f)
			w.mu.Unlock()
			return hub(f, ctx)
		},
	}
}

// wiringRefusal is the hub the real env answered before this command took writerhold.HubFlags:
// every command that reads the hub refused, naming it. A held command's re-read reads the hub, so
// under it every held command refused in a real run.
var wiringRefusal = errors.New("this build takes no flags for the hub's Kubernetes objects yet (writerhold.HubFlags)")

// Every held command of consumer-names reaches the hub as a real run does: its flags, the hold's
// own as `envoy-infra hold` takes them (writerhold.HubFlags), go through envFromFlags to the
// broker's routes (signing), the hub's NATS and the hub's Kubernetes objects, and the command runs
// past its held re-read, which reads the hub: whatever it answers after is the command's own (its
// cohort, its stores, its record). Each subtest names the hub's objects by flags no other row sets
// and asserts the openers got them. Control, in the same hold first: the hub as the real env
// answered it before (wiringRefusal) refuses the command at its re-read, naming the hub, and the
// command writes nothing to the hold's journal.
func TestEveryHeldCommandReachesTheHubThroughItsFlags(t *testing.T) {
	daemonDB := func(h *stack) string {
		h.t.Helper()
		url := testdb.DatabaseOn(h.t, os.Getenv("BROKER_TEST_DATABASE_URL"), "cnames")
		file := filepath.Join(h.t.TempDir(), "daemon.url")
		require.NoError(h.t, os.WriteFile(file, []byte(url+"\n"), 0o600))
		return file
	}
	for _, row := range []struct {
		name     string
		workflow holdrecord.Workflow
		args     func(h *stack) []string
		// bucket is a workflow whose re-read reads the selection bucket present, so the row makes it
		// (the rollback, enroll, sentinel-repair and consumer-cleanup workflows run after a cutover
		// made it).
		bucket bool
	}{
		{"create-bucket", holdrecord.WorkflowCutover, func(*stack) []string { return []string{actionCreateBucket} }, false},
		{"convert", holdrecord.WorkflowCutover, func(*stack) []string { return []string{actionConvert} }, false},
		{"cleanup", holdrecord.WorkflowConsumerCleanup, func(*stack) []string { return []string{actionCleanup} }, true},
		{"replace", holdrecord.WorkflowConsumerCleanup, func(*stack) []string { return []string{actionReplace, "--listener", "hub"} }, true},
		{"create-bucket --after-loss", holdrecord.WorkflowSelectionRecovery, func(*stack) []string {
			return []string{actionCreateBucket, "--after-loss"}
		}, false},
		{"replay", holdrecord.WorkflowSelectionRecovery, func(*stack) []string {
			return []string{actionReplay, "--key", "ses_wiring", "--from", "1"}
		}, false},
		{"restore --prepare", holdrecord.WorkflowRollback, func(*stack) []string { return []string{actionRestore, "--prepare"} }, true},
		{"restore --key", holdrecord.WorkflowRollback, func(*stack) []string {
			return []string{actionRestore, "--key", "ses_wiring", "--from", "1"}
		}, true},
		{"restore --finish", holdrecord.WorkflowRollback, func(*stack) []string {
			return []string{actionRestore, "--finish"}
		}, true},
		{"enroll", holdrecord.WorkflowEnroll, func(h *stack) []string {
			return []string{actionEnroll, "--daemon", "WIRE", "--daemon-database", "WIRE=" + daemonDB(h)}
		}, true},
		{"repair-sentinel", holdrecord.WorkflowSentinelRepair, func(*stack) []string { return []string{actionRepair} }, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newStack(t)
			id := h.heldHold(row.workflow)
			nc, js := h.operator()
			h.notificationStream(js)
			if row.bucket {
				_, err := selection.CreateSessionBucket(context.Background(), nc, 1,
					selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1, Hold: id})
				require.NoError(t, err)
			}
			h.closeOperators()
			flags := []string{"--hold", id, "--kube-namespace", "ns-wiring", "--statefulset", "ss-wiring",
				"--configmap", "cm-wiring"}
			args := append(append(row.args(h), flags...), commonArgs...)
			args = append(args, writingArgs...)
			before := len(h.journal(id).Ops)

			control := &wiringOpeners{h: h}
			var stdout, stderr bytes.Buffer
			code := runCommand(context.Background(), args, &stdout, &stderr, envFromFlags(control.openers(
				func(writerhold.HubFlags, context.Context) (*writerhold.Operator, error) { return nil, wiringRefusal })))
			t.Logf("control: exit %d\n%s%s", code, stdout.String(), stderr.String())
			require.NotZero(t, code, "control: %s ran with no reach of the hub", row.name)
			require.Contains(t, stderr.String(), wiringRefusal.Error(), "control: the refusal names the hub")
			require.Len(t, h.journal(id).Ops, before, "control: a command refused at its re-read wrote to the journal")

			wired := &wiringOpeners{h: h}
			stdout.Reset()
			stderr.Reset()
			code = runCommand(context.Background(), args, &stdout, &stderr, envFromFlags(wired.openers(
				func(writerhold.HubFlags, context.Context) (*writerhold.Operator, error) { return h.op, nil })))
			t.Logf("wired: exit %d\n%s%s", code, stdout.String(), stderr.String())
			require.NotContains(t, stderr.String(), "reach of the hub", "%s did not reach the hub", row.name)
			require.Len(t, wired.hubs, 1, "%s opened the hub's reach %d times", row.name, len(wired.hubs))
			got := wired.hubs[0]
			require.Equal(t, []string{"ns-wiring", "ss-wiring", "cm-wiring"}, []string{got.Namespace, got.StatefulSet, got.ConfigMap},
				"the hub's Kubernetes objects the flags name")
			require.Equal(t, []string{"nats://consumernames-test:4222"}, got.NATS, "the hub's client URLs")
			require.Equal(t, "/dev/null", got.TokenFile, "the operator's token file")
			require.NotEmpty(t, wired.records, "%s opened no client of the broker's routes", row.name)
			require.Equal(t, "https://broker.consumernames-test", wired.records[0].BrokerURL)
			require.True(t, strings.HasSuffix(wired.records[0].KeySecret, "/operator-only-envoy-hold-signing"))
			require.True(t, wired.signing[0], "a held command's client of the broker's routes signs")
			require.NotContains(t, stderr.String(), "the held re-read", "%s stopped at its re-read", row.name)
		})
	}
}
