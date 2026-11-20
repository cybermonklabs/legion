// The Burstable issue pod live test, which the kind job of ci.yaml runs on the rig of
// internal/kindtest. Two legion.yaml files, one giving the issue pod a memory request below its
// limit and one requesting its limits in full, are each read by the daemon's own loader and
// translated by the daemon's own sandboxOptions, and the Agent Sandbox runtime they configure
// launches one issue's root on the rig's cluster, through Agent Sandbox's controller and the
// gvisor runtime class (burstable_kind_test.go builds both). Both reservations are small, so both
// pods fit the runner's one node. What the test stands in for lies outside the reservation path:
// the agent is a sleep stub (no model, no Dispatch, no NATS), the store answers as the daemon's
// does for an issue's first launch, the repository is a public one of the rig's fake GitHub, which
// the pod and its repository cache clone without a credential, the shared public leak check is
// the rig's stand-in, and the roles' gh files hold a placeholder token.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/kindtest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// TestABurstableIssuePodRendersLive launches an issue pod from each case's configuration
// and holds it to the case's reservation and QoS class, as the API server recorded them.
func TestABurstableIssuePodRendersLive(t *testing.T) {
	fixtureenv.Live(t)
	rig := kindtest.Start(t)
	sessions := rig.StartPostgres(t)
	rig.GitHub.NewRepository(t, burstableLiveRepository.Owner(), burstableLiveRepository.Name(), true)
	leakCheck := kindtest.LeakCheck(t)
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	work := t.TempDir()
	for _, tc := range burstableLiveCases {
		started := time.Now()
		kindLiveProviders(t, rig, tc.project, sessions.InCluster)
		path := writeBurstableLiveConfig(t, filepath.Join(work, tc.name), tc.project, rig.Gateway, rig.Image,
			rig.Kubeconfig, rig.Context, tc.resources)
		cfg, err := config.LoadForValidation(path, nil)
		if err != nil {
			t.Fatalf("%s: load %s: %v", tc.name, path, err)
		}
		deployment := Deployment(cfg)
		fmt.Printf("== %s: %s\n", tc.name, path)
		fmt.Printf("   resource-limits row: %s\n", states(deployment.Report())["resource-limits"].Detail)
		pod := launchBurstableLiveRoot(ctx, t, cfg, leakCheck, log)
		roles := liveContainer(t, pod, runtime.IssuePodContainer)
		fmt.Printf("   pod %s/%s: qosClass %s, runtimeClassName %s, node %s, PodScheduled %s\n", pod.Namespace, pod.Name,
			pod.Status.QOSClass, liveString(pod.Spec.RuntimeClassName), pod.Spec.NodeName, liveCondition(pod, corev1.PodScheduled))
		fmt.Printf("   roles container: requests cpu %s memory %s; limits cpu %s memory %s\n",
			roles.Resources.Requests.Cpu(), roles.Resources.Requests.Memory(), roles.Resources.Limits.Cpu(), roles.Resources.Limits.Memory())
		if pod.Status.QOSClass != tc.qos {
			t.Errorf("%s: pod %s is %q, want %s", tc.name, pod.Name, pod.Status.QOSClass, tc.qos)
		}
		for _, kind := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
			if got, want := roles.Resources.Requests[kind], tc.issue.Requests[kind]; got.Cmp(want) != 0 {
				t.Errorf("%s: the roles container requests %s %s, want %s", tc.name, kind, got.String(), want.String())
			}
			if got, want := roles.Resources.Limits[kind], tc.issue.Limits[kind]; got.Cmp(want) != 0 {
				t.Errorf("%s: the roles container is limited to %s %s, want %s", tc.name, kind, got.String(), want.String())
			}
		}
		if scheduled := liveCondition(pod, corev1.PodScheduled); scheduled != corev1.ConditionTrue {
			t.Errorf("%s: pod %s PodScheduled is %q, want True", tc.name, pod.Name, scheduled)
		}
		fmt.Printf("ok %s in %s\n", tc.name, time.Since(started).Round(time.Second))
	}
}

// kindLiveProviders creates, in the rig's legion namespace, the providers Secret the pods of
// project mount (sandbox.ProvidersSecretName): its key SESSION_DSN, the postgres session store's
// key every kind test's legion.yaml names (session_dsn_secret), holds dsn, the rig's PostgreSQL as
// the pods dial it. A pod whose Secret is missing never starts.
func kindLiveProviders(t *testing.T, rig *kindtest.Rig, project, dsn string) {
	t.Helper()
	token, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	rig.Apply(t, fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata: {name: %s, namespace: legion}\nstringData: {SESSION_DSN: %q}\n",
		sandbox.ProvidersSecretName(token), dsn))
}

// launchBurstableLiveRoot runs the cluster check, builds the Agent Sandbox runtime from cfg
// (burstableLiveRuntime) on a worker stream at cfg's bind, reading the shared public leak check its
// pods install from leakCheck, launches the root architect of an issue of cfg's project through the
// runtime's Spawn, and returns the issue pod as the API server holds it once the launch returned.
func launchBurstableLiveRoot(ctx context.Context, t *testing.T, cfg config.Config, leakCheck sandbox.LeakCheckReader, log *slog.Logger) *corev1.Pod {
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
	issue := cfg.Project + "-1"
	token, err := claim.NewToken(project, issue, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	bootToken := "boot-" + string(token)
	resolve := func(presented string) (claim.Token, uint64, bool, bool, error) {
		if presented != bootToken {
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
	rt, _, err := burstableLiveRuntime(ctx, cfg, rc, listener, leakCheck, log)
	if err != nil {
		t.Fatalf("the Sandbox runtime: %v", err)
	}
	prompt := filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(prompt, []byte("You are the architect.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loc, err := rt.Spawn(ctx, burstableLiveSpec(project, issue, token, bootToken, prompt))
	if err != nil {
		t.Fatalf("Spawn %s: %v", token, err)
	}
	clients, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	pods, err := clients.CoreV1().Pods(k.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		if string(pods.Items[i].UID) == loc.Sandbox.PodUID {
			return &pods.Items[i]
		}
	}
	t.Fatalf("no pod in %s has the launched uid %s", k.Namespace, loc.Sandbox.PodUID)
	return nil
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
