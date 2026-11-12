package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The Burstable issue pod live proof (scripts/e2e/burstable-kind.sh, whose driver is
// burstable_live_test.go, built with -tags e2e) writes its legion.yaml files and builds its Sandbox
// runtime with the functions here. They are untagged so `go test` holds them to the loader's and
// the runtime's current contracts on every run: a change that refuses the proof's files or its
// runtime's options fails this package's tests, not the live proof the next time a kind cluster
// runs it.

// burstableLiveRepository is a public repository, so the pod's workspace-fetch and its repository
// cache clone it without GitHub asking for the placeholder provisioning token; its default branch
// is master.
var burstableLiveRepository = ghrepo.MustParse("octocat/Hello-World")

// burstableLiveStub is the agent every launched shim wraps: it sleeps, so the pod stays up for
// kubectl to read and nothing dials a model, Dispatch or NATS.
var burstableLiveStub = []string{"/bin/sh", "-c", "exec sleep infinity", "burstable-stub"}

// The two stop-eviction annotation keys the proof reads off the pod the API server holds. They are
// spelled out here, as an operator reading a live pod spells them, because the renderer's own
// constants (internal/runtime/sandbox/manifest.go) are unexported: a rename there that did not
// reach the cluster would show as a proof that reads no annotation.
const (
	liveDoNotDisruptAnnotation = "karpenter.sh/do-not-disrupt"
	liveSafeToEvictAnnotation  = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)

// burstableLiveCases are the two deployments the proof launches: the issue pod requesting 8Gi of
// memory under a 32Gi limit, which must be Burstable and must ask for no extended run time, and a
// file with no `resources` block, whose issue pod must stay Guaranteed at the daemon's defaults
// and ask for it. extendedRunTime is whether that pod carries the Cluster Autoscaler's
// safe-to-evict annotation, which GKE Autopilot answers with a node of the pod's own; Karpenter's
// do-not-disrupt is on both pods, since it is the AWS scheduler's and this rule is Autopilot's.
var burstableLiveCases = []struct {
	name, project, resources string
	qos                      corev1.PodQOSClass
	extendedRunTime          bool
	issue                    corev1.ResourceRequirements
}{
	{
		name: "burstable", project: "BURST", qos: corev1.PodQOSBurstable, extendedRunTime: false,
		resources: "    resources:\n      issue: {memory: 32Gi, memory_request: 8Gi}\n",
		issue: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("8Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("6Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("32Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("80Gi")},
		},
	},
	{
		name: "guaranteed", project: "GUARD", qos: corev1.PodQOSGuaranteed, extendedRunTime: true,
		issue: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("19Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("6Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("19Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("80Gi")},
		},
	},
}

// checkBurstableLiveStopEviction holds the pod the API server returned to the case's stop-eviction
// annotations: Karpenter's on every pod, and the Cluster Autoscaler's only where the case's
// reservation asks GKE Autopilot for extended run time. A Burstable issue pod carrying
// safe-to-evict would be a pod Autopilot still gives a node of its own, which is what lowering a
// request is meant to stop. It lives in this untagged file with the case table it reads, so `go
// vet ./internal/daemon/...` and `go build` hold it to the API types on every run, as the proof's
// configurations are held.
func checkBurstableLiveStopEviction(t *testing.T, name string, pod *corev1.Pod, extendedRunTime bool) {
	t.Helper()
	if got := pod.Annotations[liveDoNotDisruptAnnotation]; got != "true" {
		t.Errorf("%s: pod %s carries %s=%q, want \"true\": every Legion pod carries Karpenter's annotation, whatever it requests",
			name, pod.Name, liveDoNotDisruptAnnotation, got)
	}
	got, carried := pod.Annotations[liveSafeToEvictAnnotation]
	switch {
	case extendedRunTime && got != "false":
		t.Errorf("%s: pod %s carries %s=%q (present %v), want \"false\": a Guaranteed issue pod asks GKE Autopilot for extended run time",
			name, pod.Name, liveSafeToEvictAnnotation, got, carried)
	case !extendedRunTime && carried:
		t.Errorf("%s: pod %s carries %s=%q, want no such annotation: an issue pod requesting less than its limit asks for no extended run time, so Autopilot may share its node",
			name, pod.Name, liveSafeToEvictAnnotation, got)
	}
}

// writeBurstableLiveConfig writes the case's legion.yaml, and the token files it names, under
// dir: the worker stream bound at streamHost, the worker image, the cluster as kubeconfig's
// kubeContext, and extra appended to its runtime.kubernetes block (a `resources` block here, the
// share proof's `pod` block in share_volumes_kind_test.go). Its project maps no repository in the
// file, which the loader refuses; the repository the proof launches on is the runtime's mapping
// (burstableLiveRuntime). The hosts no stub dials are documentation placeholders.
func writeBurstableLiveConfig(t *testing.T, dir, project, streamHost, image, kubeconfig, kubeContext, extra string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"envoy-token", "operator-token", "dispatch-token"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("placeholder-"+file+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body := fmt.Sprintf(`project: %[1]s
postgres_dsn: postgres://legion:legion@127.0.0.1:1/legion
state_dir: %[2]s
bind: %[3]s
daemon_url: http://%[3]s:13370
envoy_url: http://envoy-listener.internal.example:9020
envoy_token_file: ./envoy-token
operator_token_file: ./operator-token
dispatch_url: https://dispatch.internal.example
dispatch_token_file: ./dispatch-token
nats_urls: [nats://nats.internal.example:4222]
worker_boot_timeout_seconds: 300
projects:
  %[1]s: {}
github_apps:
  implement: { app_id: "1", private_key_command: "exit 1" }
  review: { app_id: "2", private_key_command: "exit 1" }
runtime:
  kubernetes:
    namespace: legion
    image: %[4]s
    storage_class: standard
    kubeconfig: %[5]s
    context: %[6]s
    session_store: postgres
    session_dsn_secret: SESSION_DSN
%[7]s`, project, filepath.Join(dir, "state"), streamHost, image, kubeconfig, kubeContext, extra)
	path := filepath.Join(dir, "legion.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// burstableLiveRuntime builds the Sandbox runtime from cfg as boot does for a first launch:
// sandboxOptions over the loaded configuration on the cluster client rc and the worker stream
// listener, with the proof's stand-ins: the store answers as the daemon's does for an issue's
// first launch, the provisioning token is one a public repository never needs, every role's gh
// files hold a placeholder token, the agent is a sleep stub, and the mapping names
// burstableLiveRepository alone, so the repository cache an issue pod's launch makes is that
// repository's. It returns the issue pod's reservation as sandboxOptions translated it.
func burstableLiveRuntime(ctx context.Context, cfg config.Config, rc *rest.Config, listener *stream.Listener, log *slog.Logger) (*sandbox.Runtime, corev1.ResourceRequirements, error) {
	k := *cfg.Runtime.Kubernetes
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		return nil, corev1.ResourceRequirements{}, err
	}
	opts, err := sandboxOptions(cfg, k, project, "placeholder-dispatch-token", lookup(nil), log)
	if err != nil {
		return nil, corev1.ResourceRequirements{}, fmt.Errorf("sandboxOptions: %w", err)
	}
	opts.Conns, opts.StreamURL, opts.Store = listener, listener.Addr(), firstLaunchStore{}
	opts.Tokens = publicCloneTokens{}
	opts.GitHubCredential = func(_ context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
		rendered := ghconfig.Render("ghs_placeholder", string(appauth.AppRoleFor(role)), time.Now().Add(time.Hour))
		owner := strings.ToLower(repo.Owner())
		return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: rendered}}, nil
	}
	// The controller's own bundle, which its launch and its refresh both ask for by owner
	// (sandbox.Options.ControllerCredential, appauth.ControllerCredential): the review App's, over
	// the proof's one owner, since the project serves burstableLiveRepository alone.
	opts.ControllerCredential = func(_ context.Context, defaultOwner string) (ghconfig.Bundle, error) {
		rendered := ghconfig.Render("ghs_placeholder", string(appauth.Review), time.Now().Add(time.Hour))
		owner := strings.ToLower(burstableLiveRepository.Owner())
		if defaultOwner != "" {
			owner = defaultOwner
		}
		return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: rendered}}, nil
	}
	// The stand-in bundle holds the launch's own owner and no other, so the proof's project
	// serves none of its own: every pod it makes projects what it was launched with, and no pod
	// has drifted from a directory the daemon serves (sandbox's movedGhDirs).
	opts.GitHubDirs = func(ghrepo.Repository, []appauth.AppRole) []string { return nil }
	opts.Mapped = func() []ghrepo.Repository { return []ghrepo.Repository{burstableLiveRepository} }
	opts.Agent = burstableLiveStub
	rt, err := sandbox.New(ctx, rc, opts)
	if err != nil {
		return nil, corev1.ResourceRequirements{}, fmt.Errorf("sandbox.New: %w", err)
	}
	listener.SetLauncherResolver(rt.LauncherResolver())
	return rt, opts.Resources.Issue, nil
}

// burstableLiveSpec is the root architect's launch of issue as the proof makes it, on
// burstableLiveRepository, with prompt as its role prompt.
func burstableLiveSpec(project, issue string, token claim.Token, bootToken, prompt string) runtime.SpawnSpec {
	return runtime.SpawnSpec{
		Claim: token, Project: project, Tree: issue, Issue: issue, Role: claim.RoleArchitect, Generation: 1, BootToken: bootToken,
		Secrets: map[string]string{"ENVOY_TOKEN": "placeholder-envoy-token"},
		Prompt:  runtime.PromptParts{RolePromptPaths: []string{prompt}}, Repository: burstableLiveRepository, DefaultBranch: "master",
	}
}

// firstLaunchStore answers the runtime as the daemon's store does for an issue's first launch:
// its tree live, no other tree running it, its Sandbox its own tree's, and no controller claim
// stored.
type firstLaunchStore struct{}

func (firstLaunchStore) TreeLive(context.Context, string, string) (bool, error) { return true, nil }

func (firstLaunchStore) IssueRunsElsewhere(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (firstLaunchStore) IssueSandboxHome(_ context.Context, _, issue string) (string, error) {
	return issue, nil
}

func (firstLaunchStore) ClaimUnretired(context.Context, claim.Token) (bool, error) { return false, nil }
func (firstLaunchStore) Claims(context.Context) ([]supervise.Claim, error)         { return nil, nil }

// publicCloneTokens is the provisioning token for a public repository, which GitHub never asks
// git for.
type publicCloneTokens struct{}

func (publicCloneTokens) Token(context.Context, ghrepo.Repository) (string, error) {
	return "public-repository-needs-no-token", nil
}

// Each of the live proof's legion.yaml files passes the daemon's loader, holds the reservation the
// proof expects of the issue pod, and builds its runtime through sandboxOptions and sandbox.New
// against an API server standing in for the kind cluster; each case's root launch passes the
// spawn-spec checks. Every step is the one the driver takes before it reaches the cluster.
func TestTheBurstableKindProofBuildsItsRuntimesFromItsConfigurations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(newIssueSandboxAPI())
	t.Cleanup(func() { cancel(); server.Close() })
	kubeconfig := writeKubeconfig(t, server.URL, "")
	for _, tc := range burstableLiveCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeBurstableLiveConfig(t, filepath.Join(dir, tc.name), tc.project, "192.0.2.10",
				"ghcr.io/cybermonklabs/legion-worker@sha256:"+strings.Repeat("a", 64), kubeconfig, "test", tc.resources)
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
			_, reservation, err := burstableLiveRuntime(ctx, cfg, rc, listener, quietLogger())
			if err != nil {
				t.Fatalf("the proof's runtime: %v", err)
			}
			for _, kind := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
				if got, want := reservation.Requests[kind], tc.issue.Requests[kind]; got.Cmp(want) != 0 {
					t.Errorf("the file's issue pod requests %s %s, want %s", kind, got.String(), want.String())
				}
				if got, want := reservation.Limits[kind], tc.issue.Limits[kind]; got.Cmp(want) != 0 {
					t.Errorf("the file's issue pod is limited to %s %s, want %s", kind, got.String(), want.String())
				}
			}
			project, err := claim.ProjectToken(cfg.Project)
			if err != nil {
				t.Fatal(err)
			}
			issue := cfg.Project + "-1"
			token, err := claim.NewToken(project, issue, claim.RoleArchitect)
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.ValidateSpawnSpec(burstableLiveSpec(project, issue, token, "boot-token", filepath.Join(dir, "role.md")), nil); err != nil {
				t.Fatalf("the proof's root launch: %v", err)
			}
		})
	}
}
