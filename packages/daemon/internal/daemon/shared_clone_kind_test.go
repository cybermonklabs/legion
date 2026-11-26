package daemon

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// The shared clone live proof (scripts/e2e/shared-clone-kind.sh, whose driver is
// shared_clone_live_test.go, built with -tags e2e) writes its legion.yaml and builds its Sandbox
// runtime with what is here and in burstable_kind_test.go. They are untagged so `go test` holds
// them to the loader's and the runtime's current contracts on every run, as the Burstable proof's
// are.

// sharedCloneLiveRepository is a public repository, so the pods' workspace-fetch and the
// repository cache clone it without GitHub asking for the placeholder provisioning token, and one
// large enough that a copy of its objects stands far above what a clone writes itself: about 27 MB
// on GitHub, on 2026-11-24. Its default branch is master. It is the runtime's cache set
// (burstableLiveRuntime), so the first pod's launch makes its repository cache, and the second pod,
// launched once that cache is Ready, fills its feed from the cache's tar.
var sharedCloneLiveRepository = ghrepo.MustParse("tmux/tmux")

// sharedCloneLiveResources is the proof's runtime.kubernetes reservations, sized so two issue pods
// and the repository cache fit on the one node of a kind cluster on a GitHub-hosted runner (4 CPUs,
// 16 GiB) beside the control plane and Agent Sandbox's controller: the daemon's defaults reserve 2
// CPUs and 8Gi for each issue pod. The agent is a sleep stub, so the pods need little but the
// provision's git and jj.
const sharedCloneLiveResources = `    resources:
      issue: {cpu: 2, cpu_request: 500m, memory: 4Gi, memory_request: 2Gi, ephemeral_storage: 20Gi, ephemeral_storage_request: 2Gi}
    repository_cache:
      resources: {cpu: 1, memory: 2Gi, ephemeral_storage: 4Gi, ephemeral_storage_request: 1Gi}
`

// The shared clone proof's legion.yaml passes the daemon's loader, holds the issue pod's small
// reservation, and builds its runtime, with the proof's repository as the cache set, through
// sandboxOptions and sandbox.New against an API server standing in for the kind cluster; both of
// its root launches pass the spawn-spec checks. Every step is the one the driver takes before it
// reaches the cluster.
func TestTheSharedCloneKindProofBuildsItsRuntimeFromItsConfiguration(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(newIssueSandboxAPI())
	t.Cleanup(func() { cancel(); server.Close() })
	dir := t.TempDir()
	// The shared-clone proof dials no NATS, so its nats_urls entry is a host nothing answers on, as
	// every other case of these files that dials none names (burstableLiveNoNATS).
	path := writeBurstableLiveConfig(t, filepath.Join(dir, "shared-clone"), "CLONE", "192.0.2.10",
		"ghcr.io/cybermonklabs/legion-worker@sha256:"+strings.Repeat("a", 64), writeKubeconfig(t, server.URL, ""), "test", burstableLiveNoNATS, sharedCloneLiveResources)
	cfg, err := config.LoadForValidation(path, nil)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	rc, err := kubeClient(*cfg.Runtime.Kubernetes)
	if err != nil {
		t.Fatal(err)
	}
	refuse := func(string) (claim.Token, uint64, bool, bool, error) { return "", 0, false, false, nil }
	listener, err := stream.Listen(ctx, "tcp://127.0.0.1:0", refuse, stream.Options{RPCTimeout: cfg.WorkerRPCTimeout, Log: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	_, reservation, err := burstableLiveRuntime(ctx, cfg, rc, listener, quietLogger(), sharedCloneLiveRepository)
	if err != nil {
		t.Fatalf("the proof's runtime: %v", err)
	}
	for kind, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: "500m", corev1.ResourceMemory: "2Gi", corev1.ResourceEphemeralStorage: "2Gi"} {
		if got := reservation.Requests[kind]; got.Cmp(resource.MustParse(want)) != 0 {
			t.Errorf("the proof's issue pod requests %s %s, want %s", kind, got.String(), want)
		}
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range []string{"CLONE-1", "CLONE-2"} {
		token, err := claim.NewToken(project, issue, claim.RoleArchitect)
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.ValidateSpawnSpec(liveRootSpec(project, issue, token, "boot-token", filepath.Join(dir, "role.md"), sharedCloneLiveRepository, "master"), nil); err != nil {
			t.Fatalf("the proof's launch of %s: %v", issue, err)
		}
	}
}
