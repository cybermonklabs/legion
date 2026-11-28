package daemon

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// The issue-clone live proof (scripts/e2e/shared-clone-kind.sh, whose driver is
// shared_clone_live_test.go, built with -tags e2e) writes its legion.yaml and builds its Sandbox
// runtime with what is here and in burstable_kind_test.go. They are untagged so `go test` holds
// them to the loader's and the runtime's current contracts on every run, as the Burstable proof's
// are.

// sharedCloneLiveRepository is public, so the pods' workspace-init and repository cache clone it
// without GitHub asking for the placeholder provisioning token. Its default branch is master. It
// is the runtime's cache set (burstableLiveRuntime), so the first pod's launch makes its repository
// cache, and the second pod, launched once that cache is Ready, fills its clone from the cache's
// tar.
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

// The shared clone proof's legion.yaml passes the daemon's loader and builds its runtime as its
// driver does before it reaches the cluster (kindProofOptions), with the proof's repository as the
// cache set, and holds the issue pod's small reservation.
func TestTheSharedCloneKindProofBuildsItsRuntimeFromItsConfiguration(t *testing.T) {
	t.Parallel()
	opts := kindProofOptions(t, "CLONE", "shared-clone", sharedCloneLiveRepository, "master", sharedCloneLiveResources, "CLONE-1", "CLONE-2")
	reservation := opts.Resources.Issue
	for kind, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: "500m", corev1.ResourceMemory: "2Gi", corev1.ResourceEphemeralStorage: "2Gi"} {
		if got := reservation.Requests[kind]; got.Cmp(resource.MustParse(want)) != 0 {
			t.Errorf("the proof's issue pod requests %s %s, want %s", kind, got.String(), want)
		}
	}
}
