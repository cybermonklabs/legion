package daemon

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// The issue-pod start live proof (scripts/e2e/issue-pod-start-kind.sh and
// issue-pod-start-small-kind.sh, whose driver is issue_pod_start_live_test.go, built with -tags
// e2e) writes its legion.yaml and builds its Sandbox runtime with what is here and in
// burstable_kind_test.go. They are untagged so `go test` holds them to the loader's and the
// runtime's current contracts on every run, as the shared clone proof's are.

// issuePodStartLiveProject is the proof's project; its two issues are START-1 and START-2
// (issuePodStartLiveIssues).
const issuePodStartLiveProject = "START"

// issuePodStartLiveIssues are the proof's two issues, launched one after the other on one
// repository: the first, whose launch makes the repository cache and which the driver ends once
// that cache is Ready, and the second, whose start the proof measures.
var issuePodStartLiveIssues = [2]string{issuePodStartLiveProject + "-1", issuePodStartLiveProject + "-2"}

// issuePodStartLiveResources is the proof's runtime.kubernetes reservations, the same in its base
// arm and its head arm. The issue pod's CPU limit is 4, as OS Verify's issue pods have; its memory
// limit, 12Gi, leaves room for the tree the checkout writes, which gVisor counts as the pod's memory.
// The requests only place the pods: the two issue pods and the repository cache request 1.5 CPU and
// 5 GiB, which fit on the one node of a kind cluster on a GitHub-hosted runner (4 CPUs, 16 GiB)
// beside the control plane and Agent Sandbox's controller.
const issuePodStartLiveResources = `    resources:
      issue: {cpu: 4, cpu_request: 500m, memory: 12Gi, memory_request: 2Gi, ephemeral_storage: 20Gi, ephemeral_storage_request: 2Gi}
    repository_cache:
      resources: {cpu: 2, cpu_request: 500m, memory: 4Gi, memory_request: 1Gi, ephemeral_storage: 4Gi, ephemeral_storage_request: 1Gi}
`

// The issue-pod start proof's legion.yaml passes the daemon's loader and builds its runtime as its
// driver does before it reaches the cluster (kindProofOptions), and gives the issue pod and the
// repository cache the reservations the proof states (issuePodStartLiveResources).
func TestTheIssuePodStartKindProofBuildsItsRuntimeFromItsConfiguration(t *testing.T) {
	t.Parallel()
	opts := kindProofOptions(t, issuePodStartLiveProject, "issue-pod-start", ghrepo.MustParse("rust-lang/crates.io-index"), "master",
		issuePodStartLiveResources, issuePodStartLiveIssues[:]...)
	for _, pod := range []struct {
		name             string
		got              corev1.ResourceRequirements
		requests, limits map[corev1.ResourceName]string
	}{
		{"issue pod", opts.Resources.Issue,
			map[corev1.ResourceName]string{corev1.ResourceCPU: "500m", corev1.ResourceMemory: "2Gi", corev1.ResourceEphemeralStorage: "2Gi"},
			map[corev1.ResourceName]string{corev1.ResourceCPU: "4", corev1.ResourceMemory: "12Gi", corev1.ResourceEphemeralStorage: "20Gi"}},
		{"repository cache", opts.RepositoryCache.Resources,
			map[corev1.ResourceName]string{corev1.ResourceCPU: "500m", corev1.ResourceMemory: "1Gi", corev1.ResourceEphemeralStorage: "1Gi"},
			map[corev1.ResourceName]string{corev1.ResourceCPU: "2", corev1.ResourceMemory: "4Gi", corev1.ResourceEphemeralStorage: "4Gi"}},
	} {
		for kind, want := range pod.requests {
			if got := pod.got.Requests[kind]; got.Cmp(resource.MustParse(want)) != 0 {
				t.Errorf("the proof's %s requests %s %s, want %s", pod.name, kind, got.String(), want)
			}
		}
		for kind, want := range pod.limits {
			if got := pod.got.Limits[kind]; got.Cmp(resource.MustParse(want)) != 0 {
				t.Errorf("the proof's %s is limited to %s %s, want %s", pod.name, kind, got.String(), want)
			}
		}
	}
}
