//go:build e2e

// The Burstable issue pod live proof's driver (scripts/e2e/burstable-kind.sh, which builds it with
// `-tags e2e`, owns the throwaway kind cluster it runs on, runs `legion config check` on
// the files this writes, and reads the pods' QoS class back with kubectl). Two legion.yaml files,
// one giving the issue pod a memory request below its limit and one with no `resources` block,
// are each read by the daemon's own loader and translated by the daemon's own sandboxOptions, and
// the Agent Sandbox runtime they configure launches one issue's root on the cluster, through Agent
// Sandbox's controller and the gvisor runtime class (burstable_kind_test.go builds both). What the
// proof stands in for lies outside the reservation path: the agent is a sleep stub (no model, no
// Dispatch, no NATS), the store answers as the daemon's does for an issue's first launch, the
// repository is a public one GitHub clones without a credential, the roles' gh files hold a
// placeholder token, the project's providers Secret, which the driver creates as an operator does
// (putLiveProvidersSecret), holds placeholders, and the shared public leak check is read from a
// stand-in GitHub in this process (serveLiveLeakCheck).
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// TestABurstableIssuePodRendersLive launches an issue pod from each case's configuration
// and holds it to the case's reservation and QoS class, as the API server recorded them. It writes
// `<case> <namespace>/<pod> <legion.yaml>` per case to LEGION_E2E_RESULT for the script.
func TestABurstableIssuePodRendersLive(t *testing.T) {
	fixtureenv.Live(t)
	env := map[string]string{}
	for _, name := range []string{"LEGION_E2E_KUBECONFIG", "LEGION_E2E_KUBE_CONTEXT", "LEGION_E2E_IMAGE", "LEGION_E2E_STREAM_HOST", "LEGION_E2E_WORK", "LEGION_E2E_RESULT"} {
		if env[name] = os.Getenv(name); env[name] == "" {
			t.Fatalf("%s is unset; scripts/e2e/burstable-kind.sh sets it", name)
		}
	}
	// The proof creates pods, so it runs only on a kind cluster, the one its script made, and
	// never on whatever the operator's current context is.
	if !strings.HasPrefix(env["LEGION_E2E_KUBE_CONTEXT"], "kind-") {
		t.Fatalf("LEGION_E2E_KUBE_CONTEXT %q is no kind cluster's context (kind-<name>); the proof runs only on the cluster its script creates", env["LEGION_E2E_KUBE_CONTEXT"])
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	var results []string
	for _, tc := range burstableLiveCases {
		started := time.Now()
		path := writeBurstableLiveConfig(t, filepath.Join(env["LEGION_E2E_WORK"], tc.name), tc.project, env["LEGION_E2E_STREAM_HOST"],
			env["LEGION_E2E_IMAGE"], env["LEGION_E2E_KUBECONFIG"], env["LEGION_E2E_KUBE_CONTEXT"], tc.resources)
		cfg, err := config.LoadForValidation(path, nil)
		if err != nil {
			t.Fatalf("%s: load %s: %v", tc.name, path, err)
		}
		deployment := Deployment(cfg)
		fmt.Printf("== %s: %s\n", tc.name, path)
		fmt.Printf("   resource-limits row: %s\n", states(deployment.Report())["resource-limits"].Detail)
		pod := launchBurstableLiveRoot(ctx, t, cfg, log)
		roles := liveContainer(t, pod, runtime.IssuePodContainer)
		fmt.Printf("   pod %s/%s: qosClass %s, runtimeClassName %s, node %s, PodScheduled %s\n", pod.Namespace, pod.Name,
			pod.Status.QOSClass, liveString(pod.Spec.RuntimeClassName), pod.Spec.NodeName, liveCondition(pod, corev1.PodScheduled))
		fmt.Printf("   roles container: requests cpu %s memory %s; limits cpu %s memory %s\n",
			roles.Resources.Requests.Cpu(), roles.Resources.Requests.Memory(), roles.Resources.Limits.Cpu(), roles.Resources.Limits.Memory())
		// failed is this case's own errors, so its ok line is printed only for a case that held.
		failed := false
		errorf := func(format string, args ...any) {
			t.Helper()
			failed = true
			t.Errorf(format, args...)
		}
		if pod.Status.QOSClass != tc.qos {
			errorf("%s: pod %s is %q, want %s", tc.name, pod.Name, pod.Status.QOSClass, tc.qos)
		}
		for _, kind := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
			if got, want := roles.Resources.Requests[kind], tc.issue.Requests[kind]; got.Cmp(want) != 0 {
				errorf("%s: the roles container requests %s %s, want %s", tc.name, kind, got.String(), want.String())
			}
			if got, want := roles.Resources.Limits[kind], tc.issue.Limits[kind]; got.Cmp(want) != 0 {
				errorf("%s: the roles container is limited to %s %s, want %s", tc.name, kind, got.String(), want.String())
			}
		}
		if scheduled := liveCondition(pod, corev1.PodScheduled); scheduled != corev1.ConditionTrue {
			errorf("%s: pod %s PodScheduled is %q, want True", tc.name, pod.Name, scheduled)
		}
		if !failed {
			fmt.Printf("ok %s in %s\n", tc.name, time.Since(started).Round(time.Second))
		}
		results = append(results, fmt.Sprintf("%s %s/%s %s", tc.name, pod.Namespace, pod.Name, path))
	}
	if err := os.WriteFile(env["LEGION_E2E_RESULT"], []byte(strings.Join(results, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// launchBurstableLiveRoot runs the cluster check, creates cfg's providers Secret
// (putLiveProvidersSecret), builds the Agent Sandbox runtime from cfg (burstableLiveRuntime) on a
// worker stream at cfg's bind, launches the root architect of an issue of cfg's project through
// the runtime's Spawn, and returns the issue pod as the API server holds it once the launch
// returned.
func launchBurstableLiveRoot(ctx context.Context, t *testing.T, cfg config.Config, log *slog.Logger) *corev1.Pod {
	t.Helper()
	return launchLiveRoot(ctx, t, cfg, log, burstableLiveRepository, "master")
}

// launchLiveRoot is launchBurstableLiveRoot on the public repository repo, whose default branch is
// defaultBranch (liveRootSpec), with repo the runtime's cache set.
func launchLiveRoot(ctx context.Context, t *testing.T, cfg config.Config, log *slog.Logger, repo ghrepo.Repository, defaultBranch string) *corev1.Pod {
	t.Helper()
	return startLiveRoots(ctx, t, cfg, log, repo, defaultBranch).launch(cfg.Project + "-1")
}

// liveRoots is the Agent Sandbox runtime a kind proof builds from its legion.yaml
// (burstableLiveRuntime), with the public repository it launches on as its cache set, on a worker
// stream at the file's bind that admits the boot token of each root it launches.
type liveRoots struct {
	ctx           context.Context
	t             *testing.T
	namespace     string
	project       string
	repo          ghrepo.Repository
	defaultBranch string
	rt            *sandbox.Runtime
	clients       *kubernetes.Clientset
	prompt        string
	mu            sync.Mutex
	boots         map[string]claim.Token
}

// startLiveRoots runs the cluster check, creates cfg's providers Secret (putLiveProvidersSecret)
// and builds the runtime of liveRoots from cfg.
func startLiveRoots(ctx context.Context, t *testing.T, cfg config.Config, log *slog.Logger, repo ghrepo.Repository, defaultBranch string) *liveRoots {
	t.Helper()
	k := *cfg.Runtime.Kubernetes
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := kubeClient(k)
	if err != nil {
		t.Fatal(err)
	}
	if err := sandbox.CheckInstalled(ctx, rc, agentSandbox); err != nil {
		t.Fatalf("the cluster check: %v", err)
	}
	putLiveProvidersSecret(ctx, t, cfg, log)
	clients, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	l := &liveRoots{ctx: ctx, t: t, namespace: k.Namespace, project: project, repo: repo, defaultBranch: defaultBranch,
		clients: clients, boots: map[string]claim.Token{}}
	resolve := func(presented string) (claim.Token, uint64, bool, bool, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		token, ok := l.boots[presented]
		if !ok {
			return "", 0, false, false, nil
		}
		return token, 1, false, true, nil
	}
	listener, err := stream.Listen(ctx, "tcp://"+cfg.Bind+":0", resolve, stream.Options{RPCTimeout: cfg.WorkerRPCTimeout, Log: log})
	if err != nil {
		t.Fatalf("the worker stream: %v", err)
	}
	go func() {
		for range listener.Events() {
		}
	}()
	if l.rt, _, err = burstableLiveRuntime(ctx, cfg, rc, listener, serveLiveLeakCheck(t), log, repo); err != nil {
		t.Fatalf("the Sandbox runtime: %v", err)
	}
	l.prompt = filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(l.prompt, []byte("You are the architect.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

// launch launches the root architect of issue through the runtime's Spawn and returns the issue
// pod as the API server holds it once the launch returned.
func (l *liveRoots) launch(issue string) *corev1.Pod {
	l.t.Helper()
	token, err := claim.NewToken(l.project, issue, claim.RoleArchitect)
	if err != nil {
		l.t.Fatal(err)
	}
	bootToken := "boot-" + string(token)
	l.mu.Lock()
	l.boots[bootToken] = token
	l.mu.Unlock()
	loc, err := l.rt.Spawn(l.ctx, liveRootSpec(l.project, issue, token, bootToken, l.prompt, l.repo, l.defaultBranch))
	if err != nil {
		l.t.Fatalf("Spawn %s: %v", token, err)
	}
	pods, err := l.clients.CoreV1().Pods(l.namespace).List(l.ctx, metav1.ListOptions{})
	if err != nil {
		l.t.Fatal(err)
	}
	for i := range pods.Items {
		if string(pods.Items[i].UID) == loc.Sandbox.PodUID {
			return &pods.Items[i]
		}
	}
	l.t.Fatalf("no pod in %s has the launched uid %s", l.namespace, loc.Sandbox.PodUID)
	return nil
}

// putLiveProvidersSecret puts cfg's providers Secret (liveProvidersSecret) on cfg's cluster: the
// operator's step before any pod of the project launches, since every pod mounts it. One already
// there is an earlier driver's on the same cluster (controller-sweep-kind.sh's negative control
// runs its driver after the proof's own), and its data is set to this one's.
func putLiveProvidersSecret(ctx context.Context, t *testing.T, cfg config.Config, log *slog.Logger) {
	t.Helper()
	secret, err := liveProvidersSecret(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := kubeClient(*cfg.Runtime.Kubernetes)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	secrets := clients.CoreV1().Secrets(secret.Namespace)
	verb := "created"
	_, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		var held *corev1.Secret
		if held, err = secrets.Get(ctx, secret.Name, metav1.GetOptions{}); err == nil {
			held.Data, held.StringData = secret.Data, nil
			_, err = secrets.Update(ctx, held, metav1.UpdateOptions{})
			verb = "updated"
		}
	}
	if err != nil {
		t.Fatalf("put the providers Secret %s/%s: %v", secret.Namespace, secret.Name, err)
	}
	fmt.Printf("   providers Secret %s/%s %s: keys %v, each a placeholder\n", secret.Namespace, secret.Name, verb, slices.Sorted(maps.Keys(secret.Data)))
}

// awaitCache waits, within limit, for the repository cache of the proof's repository to be Ready
// as the runtime's stores show it, and answers it.
func (l *liveRoots) awaitCache(limit time.Duration) sandbox.CacheStatus {
	l.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		status, found := l.rt.RepositoryCache(l.repo)
		if found && status.Ready {
			return status
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("the repository cache of %s is not Ready after %s: %+v (found %t)", l.repo, limit, status, found)
		}
		select {
		case <-l.ctx.Done():
			l.t.Fatalf("waiting for the repository cache of %s: %v", l.repo, l.ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

func liveContainer(t *testing.T, pod *corev1.Pod, name string) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("pod %s has no container %s", pod.Name, name)
	return corev1.Container{}
}

func liveCondition(pod *corev1.Pod, kind corev1.PodConditionType) corev1.ConditionStatus {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == kind {
			return condition.Status
		}
	}
	return ""
}

func liveString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
