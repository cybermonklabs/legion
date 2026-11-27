//go:build e2e

// The shared clone live proof's driver (scripts/e2e/shared-clone-kind.sh, which builds it with
// `-tags e2e`, owns the throwaway kind cluster, and reads the issue pods' clones and feeds with
// kubectl). Its legion.yaml is read by the daemon's own loader, and the Agent Sandbox runtime the
// daemon's own sandboxOptions builds launches two issues' roots on a public repository of some
// tens of megabytes, sharedCloneLiveRepository, through Agent Sandbox's controller and the gvisor
// runtime class, as the Burstable and share volumes proofs launch theirs (startLiveRoots;
// burstable_kind_test.go builds the runtime, shared_clone_kind_test.go holds the file's parts).
// The first pod's launch makes the repository cache, the second's feed comes from that cache,
// and each pod's workspace-init provisions the issue's clone from its feed: the code under proof. What the proof stands in for lies outside the clone, as the
// Burstable proof's does: the agent is a sleep stub, the store answers as the daemon's does for an
// issue's first launch, and the roles' gh files hold a placeholder token.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// TestASharedCloneReadsTheFeedInAnIssuePodLive writes the proof's legion.yaml and launches two
// issues' roots on sharedCloneLiveRepository: the first as the cache set's first launch, whose
// launch makes the repository cache, and, once that cache is
// Ready, the second, whose feed comes from the cache's tar (the e2e check cache-second-pod's pod).
// It writes `<namespace>/<pod> <repository>` for the first and `<namespace>/<pod> <repository>
// <cache Sandbox>` for the second to LEGION_E2E_RESULT, one line each, for the script, which reads
// the clones and the feeds in the pods.
func TestASharedCloneReadsTheFeedInAnIssuePodLive(t *testing.T) {
	// Not parallel: it gives the process back the operator's own environment, as t.Setenv does (fixtureenv.Live).
	fixtureenv.Live(t)
	env := map[string]string{}
	for _, name := range []string{"LEGION_E2E_KUBECONFIG", "LEGION_E2E_KUBE_CONTEXT", "LEGION_E2E_IMAGE", "LEGION_E2E_STREAM_HOST", "LEGION_E2E_WORK", "LEGION_E2E_RESULT"} {
		if env[name] = os.Getenv(name); env[name] == "" {
			t.Fatalf("%s is unset; scripts/e2e/shared-clone-kind.sh sets it", name)
		}
	}
	if !strings.HasPrefix(env["LEGION_E2E_KUBE_CONTEXT"], "kind-") {
		t.Fatalf("LEGION_E2E_KUBE_CONTEXT %q is no kind cluster's context (kind-<name>); the proof runs only on the cluster its script creates", env["LEGION_E2E_KUBE_CONTEXT"])
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	path := writeBurstableLiveConfig(t, filepath.Join(env["LEGION_E2E_WORK"], "shared-clone"), "CLONE", env["LEGION_E2E_STREAM_HOST"],
		env["LEGION_E2E_IMAGE"], env["LEGION_E2E_KUBECONFIG"], env["LEGION_E2E_KUBE_CONTEXT"], burstableLiveNoNATS, sharedCloneLiveResources)
	cfg, err := config.LoadForValidation(path, nil)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	roots := startLiveRoots(ctx, t, cfg, log, sharedCloneLiveRepository, "master")
	launched := func(issue string) string {
		started := time.Now()
		pod := roots.launch(issue)
		fmt.Printf("ok launched %s/%s for %s on %s in %s: runtimeClassName %s, node %s\n", pod.Namespace, pod.Name, issue, sharedCloneLiveRepository,
			time.Since(started).Round(time.Second), liveString(pod.Spec.RuntimeClassName), pod.Spec.NodeName)
		if liveString(pod.Spec.RuntimeClassName) != "gvisor" {
			t.Errorf("pod %s runs under %q, want gvisor", pod.Name, liveString(pod.Spec.RuntimeClassName))
		}
		return pod.Namespace + "/" + pod.Name
	}
	first := launched("CLONE-1")
	waited := time.Now()
	cache := roots.awaitCache(20 * time.Minute)
	fmt.Printf("ok the repository cache %s of %s is Ready at %s after %s\n", cache.Sandbox, sharedCloneLiveRepository, cache.Address,
		time.Since(waited).Round(time.Second))
	second := launched("CLONE-2")
	result := fmt.Sprintf("%s %s\n%s %s %s\n", first, sharedCloneLiveRepository, second, sharedCloneLiveRepository, cache.Sandbox)
	if err := os.WriteFile(env["LEGION_E2E_RESULT"], []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
}
