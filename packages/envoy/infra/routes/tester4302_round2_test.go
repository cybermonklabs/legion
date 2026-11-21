package routes_test

// Red tests from the independent test of the route check at 772cc6d6ce: each fails at that head.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/routes"
)

// inClusterConfigView is how kubectl 1.36.4 answers `config view --minify --output json` with no
// kubeconfig, as in a pod that reaches the API server through its service account (measured: the
// same kubectl's `get` then goes to https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT).
const inClusterConfigView = `*" config view --minify --output json") echo "error: current-context must exist in order to minify" >&2; exit 1 ;;`

// `routes record` and `routes check` run where the operator runs them under its Role: in a pod of
// its own, as its ServiceAccount, with no kubeconfig, which kubectl serves from the pod's service
// account. There `kubectl config view --minify` names no context, and the record and the check must
// still name the cluster they read and run. Control: the same hub through a kubeconfig context is
// recorded and checked, exit 0 each.
func TestRecordAndCheckRunWithoutAKubeconfig(t *testing.T) {
	run := func(t *testing.T, script string) (int, int, string) {
		h := cleanHub()
		src, dir := fakeKubeHub(t, h)
		require.NoError(t, src.Close())
		require.NoError(t, os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o700))
		out := filepath.Join(t.TempDir(), "routes.json")
		var stdout, stderr bytes.Buffer
		base := []string{"--namespace", "envoy", "--statefulset", "envoy-nats", "--kubectl", filepath.Join(dir, "kubectl")}
		back := h.atEnv()
		record := routes.Command(context.Background(), append(append([]string{"record"}, base...), "--out", out), &stdout, &stderr, routes.OpenKubectl)
		back()
		if record != 0 {
			return record, -1, stderr.String()
		}
		check := routes.Command(context.Background(), append(append([]string{"check"}, base...), "--record", out), &stdout, &stderr, routes.OpenKubectl)
		return record, check, stdout.String() + stderr.String()
	}
	t.Run("control: through a kubeconfig context", func(t *testing.T) {
		record, check, out := run(t, fakeKubectl)
		require.Equal(t, []int{0, 0}, []int{record, check}, out)
	})
	t.Run("in a pod, with no kubeconfig", func(t *testing.T) {
		t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
		t.Setenv("KUBERNETES_SERVICE_PORT", "443")
		script := strings.Replace(fakeKubectl, `*" config view --minify --output json") exec cat "$dir/config.json" ;;`, inClusterConfigView, 1)
		require.NotEqual(t, fakeKubectl, script, "the fake's config view answers as in a pod")
		record, check, out := run(t, script)
		require.Equal(t, []int{0, 0}, []int{record, check}, out)
	})
}

// `routes record` refuses while the StatefulSet's controller has not yet observed its latest spec
// (status.observedGeneration below metadata.generation), as `kubectl rollout status` does first:
// until then the status's revisions are the roll before, so right after a rotation's env apply,
// whose pods already name the route variables, every pod reads as on the update revision of a
// finished roll, and the record is of pods the roll then replaces. Control: the generation observed.
func TestKubectlRecordWaitsForTheControllerToObserveTheSpec(t *testing.T) {
	h := cleanHub()
	src, dir := fakeKubeHub(t, h)
	defer src.Close()
	back := h.atEnv()
	defer back()
	write := func(generation, observed int) {
		t.Helper()
		sts, err := json.Marshal(map[string]any{"kind": "StatefulSet", "metadata": map[string]any{"generation": generation},
			"spec": map[string]any{"replicas": len(h.pods)},
			"status": map[string]any{"observedGeneration": observed, "replicas": len(h.pods), "updatedReplicas": len(h.pods),
				"currentRevision": "envoy-nats-new", "updateRevision": "envoy-nats-new"}})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "statefulset.json"), sts, 0o600))
	}
	write(5, 5)
	_, err := routes.Take(context.Background(), src, "envoy", "envoy-nats", started)
	require.NoError(t, err, "control: the controller has observed generation 5")
	write(6, 5)
	_, err = routes.Take(context.Background(), src, "envoy", "envoy-nats", started)
	require.Error(t, err, "generation 6 not yet observed: the status's revisions are the roll before")
}

// A lock on the two halves of the finished-roll rule that no other test holds apart: every replica
// updated with the current revision still the one before (the controller has not marked the roll
// done), and a finished roll's status beside a pod whose controller-revision-hash is another
// revision. Each is refused; it passes at 772cc6d6ce, and fails when Pods drops the current
// revision's condition, or counts a pod as updated by the StatefulSet's status alone. Control: a
// finished roll with every pod on its revision is recorded.
func TestKubectlRecordNeedsBothHalvesOfAFinishedRoll(t *testing.T) {
	h := cleanHub()
	src, dir := fakeKubeHub(t, h)
	defer src.Close()
	back := h.atEnv()
	defer back()
	write := func(current string, pod2Revision string) {
		t.Helper()
		sts, err := json.Marshal(map[string]any{"kind": "StatefulSet", "spec": map[string]any{"replicas": len(h.pods)},
			"status": map[string]any{"replicas": len(h.pods), "updatedReplicas": len(h.pods), "currentRevision": current, "updateRevision": "envoy-nats-new"}})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "statefulset.json"), sts, 0o600))
		p := h.pods[2]
		pod := kubePod(p.Name, p.UID, p.IP, p.Ready, int(p.RestartCount), p.StartedAt)
		pod["metadata"].(map[string]any)["labels"] = map[string]any{"controller-revision-hash": pod2Revision}
		body, err := json.Marshal(pod)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "pod-"+p.Name+".json"), body, 0o600))
	}
	write("envoy-nats-new", "envoy-nats-new")
	_, err := routes.Take(context.Background(), src, "envoy", "envoy-nats", started)
	require.NoError(t, err, "control: a finished roll, every pod on its revision")
	write("envoy-nats-old", "envoy-nats-new")
	_, err = routes.Take(context.Background(), src, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "not on the update revision of a finished roll", "every replica updated, the current revision still the old")
	write("envoy-nats-new", "envoy-nats-old")
	_, err = routes.Take(context.Background(), src, "envoy", "envoy-nats", started)
	require.ErrorContains(t, err, "envoy-nats-2 not on the update revision of a finished roll", "a finished roll's status, one pod on another revision")
}
