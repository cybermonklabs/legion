//go:build e2e

// The issue-pod start live proof's driver (scripts/e2e/issue-pod-start-kind.sh, which builds it
// with `-tags e2e`, owns each arm's throwaway kind cluster, and reads the second pod's
// workspace-init log, workspace and clone with kubectl). Its legion.yaml is read by the daemon's
// own loader, and the Agent Sandbox runtime the daemon's own sandboxOptions builds launches two
// issues' roots, one after the other, on the public repository the script names, through Agent
// Sandbox's controller and the gvisor runtime class, as the shared clone proof launches its two
// (TestAnIssuePodGetsOneRepositoryLive; burstable_kind_test.go builds the runtime,
// issue_pod_start_kind_test.go holds the file's parts). The first pod's launch makes the repository
// cache. Once that cache is Ready the driver ends the first pod through the runtime's own release
// (ReleaseIssue), since the typed clients cannot delete a Sandbox, so the second pod's
// workspace-init, the start the proof measures, runs with the first pod gone and the cache idle.
// What the proof stands in for lies outside the start, as the shared clone proof's does: the agent
// is a sleep stub, the store answers as the daemon's does for an issue's first launch, and the
// roles' gh files hold a placeholder token.

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// issuePodStartLiveWait bounds each of the proof's waits on the cluster: each pod's launch, whose
// Spawn returns once the pod's workspace-init has ended and its launcher has connected, and the
// repository cache's Ready.
const issuePodStartLiveWait = 30 * time.Minute

// issuePodStartLiveTipWait bounds the driver's wait for the script's read of GitHub's tip before
// the second launch (LEGION_E2E_TIP_BEFORE).
const issuePodStartLiveTipWait = 10 * time.Minute

// issuePodStartLiveBound is the driver's whole run: the first launch, the cache, the tip and the
// second launch, each at its bound, and five minutes for the release and the reads between them.
// The script runs the test binary with a -test.timeout above it.
const issuePodStartLiveBound = 3*issuePodStartLiveWait + issuePodStartLiveTipWait + 5*time.Minute

// issuePodStartLiveInit is the issue pod's one init container, whose run the proof measures
// (internal/runtime/sandbox's initContainer).
const issuePodStartLiveInit = "workspace-init"

// TestTwoIssuePodsStartLive writes the proof's legion.yaml and launches two issues' roots on the
// repository LEGION_E2E_REPOSITORY names, whose default branch is LEGION_E2E_DEFAULT_BRANCH: the
// first, whose launch makes the repository cache, then, once that cache is Ready, it reads the
// volume the first pod's claim is bound to, ends the first pod through the runtime
// (ReleaseIssue), writes that pod's name to LEGION_E2E_FIRST_ENDED, waits for the script to write
// GitHub's tip of the default branch to LEGION_E2E_TIP_BEFORE, and launches the second. A container
// that the kernel killed for memory (OOMKilled) fails the proof, naming it. It writes, one
// `<key> <value>` line each, to LEGION_E2E_RESULT: both pods (`<namespace>/<pod>`), their claims and
// the volumes those are bound to, the cache's Sandbox, the second pod's issue, whose bookmark its
// clone holds, and, read from the second pod as the API server holds it, the run of its
// workspace-init (`run`, its finishedAt less its startedAt) and the time from its creation to its
// roles container's start (`create-to-roles`), in whole seconds, the precision of a container's
// state.
func TestTwoIssuePodsStartLive(t *testing.T) {
	// Not parallel: it gives the process back the operator's own environment, as t.Setenv does (fixtureenv.Live).
	fixtureenv.Live(t)
	env := map[string]string{}
	for _, name := range []string{"LEGION_E2E_KUBECONFIG", "LEGION_E2E_KUBE_CONTEXT", "LEGION_E2E_IMAGE", "LEGION_E2E_STREAM_HOST", "LEGION_E2E_WORK",
		"LEGION_E2E_RESULT", "LEGION_E2E_REPOSITORY", "LEGION_E2E_DEFAULT_BRANCH", "LEGION_E2E_FIRST_ENDED", "LEGION_E2E_TIP_BEFORE"} {
		if env[name] = os.Getenv(name); env[name] == "" {
			t.Fatalf("%s is unset; scripts/e2e/issue-pod-start-kind.sh sets it", name)
		}
	}
	if !strings.HasPrefix(env["LEGION_E2E_KUBE_CONTEXT"], "kind-") {
		t.Fatalf("LEGION_E2E_KUBE_CONTEXT %q is no kind cluster's context (kind-<name>); the proof runs only on the cluster its script creates", env["LEGION_E2E_KUBE_CONTEXT"])
	}
	repo, err := ghrepo.Parse("LEGION_E2E_REPOSITORY", env["LEGION_E2E_REPOSITORY"])
	if err != nil {
		t.Fatal(err)
	}
	branch := env["LEGION_E2E_DEFAULT_BRANCH"]
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), issuePodStartLiveBound)
	defer cancel()

	path := writeBurstableLiveConfig(t, filepath.Join(env["LEGION_E2E_WORK"], "issue-pod-start"), issuePodStartLiveProject, env["LEGION_E2E_STREAM_HOST"],
		env["LEGION_E2E_IMAGE"], env["LEGION_E2E_KUBECONFIG"], env["LEGION_E2E_KUBE_CONTEXT"], issuePodStartLiveResources)
	cfg, err := config.LoadForValidation(path, nil)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	roots := startLiveRoots(ctx, t, cfg, log, repo, branch)
	// A launch that fails ends the test before its own read of the pods, so a container the kernel
	// killed for memory is named here too.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		read, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if killed, err := outOfMemory(read, roots); err != nil {
			t.Errorf("list the pods of %s for a container killed for memory: %v", roots.namespace, err)
		} else if len(killed) > 0 {
			t.Errorf("%s", outOfMemoryText(killed))
		}
	})
	launched := func(issue string) *corev1.Pod {
		started := time.Now()
		pod := launchWithin(roots, issue, issuePodStartLiveWait)
		fmt.Printf("ok launched %s/%s for %s on %s in %s: runtimeClassName %s, node %s\n", pod.Namespace, pod.Name, issue, repo,
			time.Since(started).Round(time.Second), liveString(pod.Spec.RuntimeClassName), pod.Spec.NodeName)
		if liveString(pod.Spec.RuntimeClassName) != "gvisor" {
			t.Errorf("pod %s runs under %q, want gvisor", pod.Name, liveString(pod.Spec.RuntimeClassName))
		}
		return pod
	}

	firstIssue, secondIssue := issuePodStartLiveIssues[0], issuePodStartLiveIssues[1]
	first := launched(firstIssue)
	waited := time.Now()
	cache := roots.awaitCache(issuePodStartLiveWait)
	fmt.Printf("ok the repository cache %s of %s is Ready at %s after %s\n", cache.Sandbox, repo, cache.Address, time.Since(waited).Round(time.Second))
	firstClaim, firstVolume := issuePodVolume(roots, first)
	fmt.Printf("ok pod %s's claim %s is bound to volume %s\n", first.Name, firstClaim, firstVolume)
	refuseOutOfMemory(ctx, roots)

	released := time.Now()
	authorize := func(context.Context) (bool, error) { return true, nil }
	if took, err := roots.rt.ReleaseIssue(ctx, firstIssue, firstIssue, authorize, nil); err != nil || !took {
		t.Fatalf("release %s, whose pod is %s: %v (released %t)", firstIssue, first.Name, err, took)
	}
	if _, err := roots.clients.CoreV1().Pods(first.Namespace).Get(ctx, first.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pod %s after the release of %s: %v, want it gone", first.Name, firstIssue, err)
	}
	fmt.Printf("ok ended pod %s through the runtime's release of %s in %s\n", first.Name, firstIssue, time.Since(released).Round(time.Second))
	if err := os.WriteFile(env["LEGION_E2E_FIRST_ENDED"], []byte(first.Namespace+"/"+first.Name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tip := awaitLiveFile(ctx, t, env["LEGION_E2E_TIP_BEFORE"], issuePodStartLiveTipWait)
	fmt.Printf("   GitHub's tip of %s before the second launch: %s\n", branch, tip)

	second := awaitRolesRunning(roots, launched(secondIssue), 2*time.Minute)
	secondClaim, secondVolume := issuePodVolume(roots, second)
	fmt.Printf("ok pod %s's claim %s is bound to volume %s\n", second.Name, secondClaim, secondVolume)
	refuseOutOfMemory(ctx, roots)
	provision := liveContainerStatus(t, second, second.Status.InitContainerStatuses, issuePodStartLiveInit)
	ended := provision.State.Terminated
	if ended == nil {
		t.Fatalf("pod %s's %s has not ended: %+v", second.Name, issuePodStartLiveInit, provision.State)
	}
	// A run the kubelet restarted is two provisions, the second on what the first left, so its
	// last run is not the start the proof measures.
	if provision.RestartCount != 0 {
		t.Fatalf("pod %s's %s ran %d times; its last run is not one provision's", second.Name, issuePodStartLiveInit, provision.RestartCount+1)
	}
	roles := liveContainerStatus(t, second, second.Status.ContainerStatuses, runtime.IssuePodContainer)
	run := ended.FinishedAt.Sub(ended.StartedAt.Time)
	createToRoles := roles.State.Running.StartedAt.Sub(second.CreationTimestamp.Time)
	fmt.Printf("ok pod %s: %s ran from %s to %s (%s, exit %d); created %s, roles started %s (%s)\n", second.Name, issuePodStartLiveInit,
		ended.StartedAt.UTC().Format(time.RFC3339), ended.FinishedAt.UTC().Format(time.RFC3339), run, ended.ExitCode,
		second.CreationTimestamp.UTC().Format(time.RFC3339), roles.State.Running.StartedAt.UTC().Format(time.RFC3339), createToRoles)

	result := fmt.Sprintf("first-pod %s/%s\nfirst-claim %s\nfirst-volume %s\ncache %s\nsecond-pod %s/%s\nsecond-issue %s\nsecond-claim %s\nsecond-volume %s\nrun %d\ncreate-to-roles %d\n",
		first.Namespace, first.Name, firstClaim, firstVolume, cache.Sandbox, second.Namespace, second.Name, secondIssue, secondClaim, secondVolume,
		int64(run/time.Second), int64(createToRoles/time.Second))
	if err := os.WriteFile(env["LEGION_E2E_RESULT"], []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
}

// launchWithin is roots.launch with its Spawn bounded by limit: the launch runs under a context that
// ends at limit, and roots keeps its own context for what follows.
func launchWithin(roots *liveRoots, issue string, limit time.Duration) *corev1.Pod {
	roots.t.Helper()
	outer := roots.ctx
	ctx, cancel := context.WithTimeout(outer, limit)
	defer cancel()
	roots.ctx = ctx
	defer func() { roots.ctx = outer }()
	return roots.launch(issue)
}

// issuePodVolume answers pod's issue claim, the one Kubernetes makes from the pod's ephemeral
// volume and names `<pod>-<volume>`, and the PersistentVolume that claim is bound to, waiting a
// minute at most for the binding.
func issuePodVolume(roots *liveRoots, pod *corev1.Pod) (string, string) {
	t := roots.t
	t.Helper()
	var claims []string
	for _, volume := range pod.Spec.Volumes {
		if volume.Ephemeral != nil {
			claims = append(claims, pod.Name+"-"+volume.Name)
		}
	}
	if len(claims) != 1 {
		t.Fatalf("pod %s has the ephemeral volume claims %v, want its issue volume's alone", pod.Name, claims)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		held, err := roots.clients.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(roots.ctx, claims[0], metav1.GetOptions{})
		if err == nil && held.Status.Phase == corev1.ClaimBound && held.Spec.VolumeName != "" {
			return claims[0], held.Spec.VolumeName
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("phase %q, volume %q", held.Status.Phase, held.Spec.VolumeName)
			}
			t.Fatalf("pod %s's claim %s is not bound after a minute: %v", pod.Name, claims[0], err)
		}
		select {
		case <-roots.ctx.Done():
			t.Fatalf("waiting for pod %s's claim %s: %v", pod.Name, claims[0], roots.ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// awaitRolesRunning reads pod again until its roles container is running, within limit, and
// answers it as the API server then holds it: a launch returns once the pod's launcher has
// connected, and the pod it listed may not show the container's start yet.
func awaitRolesRunning(roots *liveRoots, pod *corev1.Pod, limit time.Duration) *corev1.Pod {
	t := roots.t
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == runtime.IssuePodContainer && status.State.Running != nil {
				return pod
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s's %s container is not running after %s: %+v", pod.Name, runtime.IssuePodContainer, limit, pod.Status.ContainerStatuses)
		}
		select {
		case <-roots.ctx.Done():
			t.Fatalf("waiting for pod %s's %s container: %v", pod.Name, runtime.IssuePodContainer, roots.ctx.Err())
		case <-time.After(2 * time.Second):
		}
		read, err := roots.clients.CoreV1().Pods(pod.Namespace).Get(roots.ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("read pod %s: %v", pod.Name, err)
		}
		pod = read
	}
}

// liveContainerStatus is the status of pod's container name among statuses.
func liveContainerStatus(t *testing.T, pod *corev1.Pod, statuses []corev1.ContainerStatus, name string) corev1.ContainerStatus {
	t.Helper()
	for _, status := range statuses {
		if status.Name == name {
			return status
		}
	}
	t.Fatalf("pod %s has no status of its %s container", pod.Name, name)
	return corev1.ContainerStatus{}
}

// refuseOutOfMemory fails the proof when outOfMemory names a container, or cannot read the pods.
func refuseOutOfMemory(ctx context.Context, roots *liveRoots) {
	roots.t.Helper()
	killed, err := outOfMemory(ctx, roots)
	if err != nil {
		roots.t.Fatalf("list the pods of %s for a container killed for memory: %v", roots.namespace, err)
	}
	if len(killed) > 0 {
		roots.t.Fatal(outOfMemoryText(killed))
	}
}

// outOfMemory names, as `<pod>/<container>`, each container of a pod of the proof's namespace
// that the kernel killed for memory (OOMKilled), in its state now or in the one before its last
// restart.
func outOfMemory(ctx context.Context, roots *liveRoots) ([]string, error) {
	pods, err := roots.clients.CoreV1().Pods(roots.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var killed []string
	for _, pod := range pods.Items {
		for _, status := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
			for _, ended := range []*corev1.ContainerStateTerminated{status.State.Terminated, status.LastTerminationState.Terminated} {
				if ended != nil && ended.Reason == "OOMKilled" {
					killed = append(killed, pod.Name+"/"+status.Name)
					break
				}
			}
		}
	}
	return killed, nil
}

// outOfMemoryText is the failure that names the containers outOfMemory found: the issue pod's
// memory limit is then too small for the start, and both arms' limit is raised alike
// (issuePodStartLiveResources).
func outOfMemoryText(killed []string) string {
	return fmt.Sprintf("OOMKilled: the kernel killed %s for memory; raise both arms' memory limit alike (issuePodStartLiveResources)", strings.Join(killed, ", "))
}

// awaitLiveFile answers the trimmed content of the file at path once it holds any, within limit.
// The script writes the file whole and renames it into place, so a read never sees part of it.
func awaitLiveFile(ctx context.Context, t *testing.T, path string, limit time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		content, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(content)) != "" {
			return strings.TrimSpace(string(content))
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s holds nothing after %s: the script read no GitHub tip before the second launch", path, limit)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", path, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
