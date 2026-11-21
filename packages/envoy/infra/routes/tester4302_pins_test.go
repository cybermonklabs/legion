package routes_test

// Red tests from the independent test of the route check at 7c7131eece. Each failed at that head
// and names the behaviour the change must have.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/routes"
)

// routes record refuses a hub whose env roll has not finished: a pod not on the StatefulSet's
// update revision, or whose nats container has no route variables, runs a container that the env
// roll replaces, so the check against such a record says "password read" (exit 1) of a hub whose
// logs reach back to env. Control: every pod on the update revision, with the route variables.
func TestKubectlRecordRefusesARollThatHasNotFinished(t *testing.T) {
	h := cleanHub()
	src, dir := fakeKubeHub(t, h)
	defer src.Close()
	back := h.atEnv()
	defer back()
	routeEnv := []any{
		map[string]any{"name": "GOMEMLIMIT", "value": "1638MiB"},
		map[string]any{"name": "NATS_ROUTE_PASSWORD", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "envoy-nats-route", "key": "route"}}},
		map[string]any{"name": "NATS_ROUTE_URLS", "value": "[nats://route:$(NATS_ROUTE_PASSWORD)@envoy-nats-0.envoy-nats-headless:6222]"},
	}
	write := func(updated int, currentRevision string, revisions map[string]string, env map[string][]any) {
		t.Helper()
		sts, err := json.Marshal(map[string]any{"kind": "StatefulSet", "spec": map[string]any{"replicas": len(h.pods)},
			"status": map[string]any{"replicas": len(h.pods), "readyReplicas": len(h.pods), "updatedReplicas": updated,
				"currentRevision": currentRevision, "updateRevision": "envoy-nats-new"}})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "statefulset.json"), sts, 0o600))
		for _, p := range h.pods {
			pod := kubePod(p.Name, p.UID, p.IP, true, 0, started)
			pod["metadata"].(map[string]any)["labels"] = map[string]any{"controller-revision-hash": revisions[p.Name]}
			pod["spec"] = map[string]any{"containers": []any{map[string]any{"name": "nats", "env": env[p.Name]}}}
			body, err := json.Marshal(pod)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "pod-"+p.Name+".json"), body, 0o600))
		}
	}
	allNew := map[string]string{"envoy-nats-0": "envoy-nats-new", "envoy-nats-1": "envoy-nats-new", "envoy-nats-2": "envoy-nats-new"}
	allEnv := map[string][]any{"envoy-nats-0": routeEnv, "envoy-nats-1": routeEnv, "envoy-nats-2": routeEnv}

	write(3, "envoy-nats-new", allNew, allEnv)
	_, err := routes.Take(context.Background(), src, "envoy", "envoy-nats", started.Add(time.Minute))
	require.NoError(t, err, "control: the env roll has finished on every pod")

	write(2, "envoy-nats-old", map[string]string{"envoy-nats-0": "envoy-nats-old", "envoy-nats-1": "envoy-nats-new", "envoy-nats-2": "envoy-nats-new"}, allEnv)
	_, err = routes.Take(context.Background(), src, "envoy", "envoy-nats", started.Add(time.Minute))
	require.ErrorContains(t, err, "envoy-nats-0", "a pod the roll has not reached yet")

	write(3, "envoy-nats-new", allNew, map[string][]any{"envoy-nats-0": routeEnv, "envoy-nats-1": routeEnv,
		"envoy-nats-2": {map[string]any{"name": "GOMEMLIMIT", "value": "1638MiB"}}})
	_, err = routes.Take(context.Background(), src, "envoy", "envoy-nats", started.Add(time.Minute))
	require.ErrorContains(t, err, "envoy-nats-2", "a pod whose nats container has no route variables: env has not been applied")
}

// The record names the kubeconfig context it was read through, and routes check refuses, exit 2,
// a record taken through another, when a side holds no CA certificate, as this fake hub holds none
// (sameCluster); with a CA on both sides the certificates decide (TestCheckKnowsTheClusterByItsCA).
// A hub of the same namespace and StatefulSet on another cluster has other pods, which the check
// otherwise reads as a new container on every pod, exit 1, and the operator is told to rotate a
// password nothing read. Control: the same context, exit 0.
func TestCheckRefusesARecordOfAnotherContext(t *testing.T) {
	h := cleanHub()
	h.atEnv()
	out := filepath.Join(t.TempDir(), "routes.json")
	code, _, stderr, _ := runRoutes(h, "record", "--namespace", "envoy", "--statefulset", "envoy-nats", "--out", out, "--context", "staging")
	require.Equal(t, 0, code, stderr)

	code, stdout, stderr, _ := runRoutes(cleanHub(), "check", "--namespace", "envoy", "--statefulset", "envoy-nats", "--record", out, "--context", "staging")
	require.Equal(t, 0, code, "control: the context the record was taken through: stdout %q stderr %q", stdout, stderr)

	code, stdout, stderr, _ = runRoutes(cleanHub(), "check", "--namespace", "envoy", "--statefulset", "envoy-nats", "--record", out, "--context", "production")
	require.Equal(t, 2, code, "a record taken through another context: stdout %q stderr %q", stdout, stderr)
}
