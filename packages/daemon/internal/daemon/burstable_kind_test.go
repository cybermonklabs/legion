package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
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

// burstableLiveCases are the two deployments the proof launches: the issue pod requesting 8Gi of
// memory under a 32Gi limit, which must be Burstable, and a file with no `resources` block, whose
// issue pod must stay Guaranteed at the daemon's defaults.
var burstableLiveCases = []struct {
	name, project, resources string
	qos                      corev1.PodQOSClass
	issue                    corev1.ResourceRequirements
}{
	{
		name: "burstable", project: "BURST", qos: corev1.PodQOSBurstable,
		resources: "    resources:\n      issue: {memory: 32Gi, memory_request: 8Gi}\n",
		issue: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("6Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("32Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("80Gi")},
		},
	},
	{
		name: "guaranteed", project: "GUARD", qos: corev1.PodQOSGuaranteed,
		issue: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("6Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("80Gi")},
		},
	},
}

// writeBurstableLiveConfig writes the case's legion.yaml, and the token files it names, under
// dir: the worker stream bound at streamHost, the worker image, the cluster as kubeconfig's
// kubeContext, and extra appended to its runtime.kubernetes block (a `resources` block here, the
// share proof's `pod` block in share_volumes_kind_test.go). Its project names no repository in the
// file, which the loader refuses; the repository the proof launches on is the runtime's cache set
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
// files hold a placeholder token, the agent is a sleep stub, the cache set names cache alone, the
// public repository the proof launches on, so the repository cache an issue pod's launch makes is
// that repository's, and the shared public leak check is the stand-in setting's
// (liveLeakCheckSetting), read by boot's own reader (leakCheckSource) through the stand-in App
// outboxTokens under leakCheckAPI (serveLiveLeakCheck). It returns the options the runtime was
// built from, which hold the issue pod's reservation as sandboxOptions translated it and the
// runtime's reader of the leak check.
func burstableLiveRuntime(ctx context.Context, cfg config.Config, rc *rest.Config, listener *stream.Listener, leakCheckAPI string, log *slog.Logger, cache ghrepo.Repository) (*sandbox.Runtime, sandbox.Options, error) {
	k := *cfg.Runtime.Kubernetes
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		return nil, sandbox.Options{}, err
	}
	if cfg.LeakCheck, err = liveLeakCheckSetting(); err != nil {
		return nil, sandbox.Options{}, err
	}
	opts, err := sandboxOptions(cfg, k, project, sandbox.DispatchCredential{Bearer: "placeholder-dispatch-token"}, lookup(nil), log)
	if err != nil {
		return nil, sandbox.Options{}, fmt.Errorf("sandboxOptions: %w", err)
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
	// the proof's one owner, since the project serves cache alone.
	opts.ControllerCredential = func(_ context.Context, defaultOwner string) (ghconfig.Bundle, error) {
		rendered := ghconfig.Render("ghs_placeholder", string(appauth.Review), time.Now().Add(time.Hour))
		owner := strings.ToLower(cache.Owner())
		if defaultOwner != "" {
			owner = defaultOwner
		}
		return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: rendered}}, nil
	}
	// The stand-in bundle holds the launch's own owner and no other, so the proof's project
	// serves none of its own: every pod it makes projects what it was launched with, and no pod
	// has drifted from a directory the daemon serves (sandbox's movedGhDirs).
	opts.GitHubDirs = func(ghrepo.Repository, []appauth.AppRole) []string { return nil }
	opts.CacheRepositories = func() []ghrepo.Repository { return []ghrepo.Repository{cache} }
	opts.Agent = burstableLiveStub
	// Each pod's launcher is pushed the check before any role of the pod starts, and a launch
	// whose launcher names the image's pin is refused without a reader (launchers.serveLeakCheck).
	opts.LeakCheck = leakCheckSource(outboxTokens{}, leakCheckAPI, opts.LeakCheckSetting)
	rt, err := sandbox.New(ctx, rc, opts)
	if err != nil {
		return nil, sandbox.Options{}, fmt.Errorf("sandbox.New: %w", err)
	}
	listener.SetLauncherResolver(rt.LauncherResolver())
	return rt, opts, nil
}

// serveLiveLeakCheck stands in for GitHub's REST root under the kind proofs' runtimes: the root
// boot's own reader of the shared public leak check (leakCheckSource) reads the check under, at
// the tag a pod's launcher names, before that launcher is pushed it. It answers the tarball route
// of the repository the proofs' setting names (liveLeakCheckSetting), at any tag, to the bearer
// of the read lease the stand-in App outboxTokens mints on that repository's owner and to no
// other, with a tarball of GitHub's shape holding the stand-in check (liveLeakCheckMembers). So no
// credential is read, and no request reaches GitHub.
func serveLiveLeakCheck(t *testing.T) string {
	t.Helper()
	setting, err := liveLeakCheckSetting()
	if err != nil {
		t.Fatal(err)
	}
	lease, err := outboxTokens{}.ReadToken(context.Background(), appauth.Implement, setting.Repository.Owner())
	if err != nil {
		t.Fatal(err)
	}
	tarball := leakguardtest.GitHubTarball(t, liveLeakCheckMembers())
	route := "/repos/" + setting.Repository.String() + "/tarball/"
	return githubtest.Server(t, githubtest.Spec{Token: lease.Token, Handle: func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, route) {
			githubtest.Unexpected(t, w, r)
			return
		}
		_, _ = w.Write(tarball)
	}})
}

// liveLeakCheckMembers is the stand-in check the kind proofs' pods install: every file its
// manifest digest covers (leakguard.ManifestFiles), whose launcher answers its --help as the real
// one does (leakguardtest.AnswersHelp). It passes the launcher's install and its --help
// verification and holds none of the real check's rules, which no write of a stub agent reaches.
func liveLeakCheckMembers() map[string]leakguardtest.Member {
	return map[string]leakguardtest.Member{
		leakguardtest.CheckName:    {Body: "#!/bin/sh\n" + leakguardtest.AnswersHelp(os.DevNull), Mode: 0o755},
		leakguardtest.ProgramName:  {Body: leakguardtest.Program, Mode: 0o644},
		leakguardtest.ReadmeName:   {Body: leakguardtest.Readme, Mode: 0o644},
		leakguardtest.MustPassName: {Body: leakguardtest.MustPass, Mode: 0o644},
	}
}

// liveLeakCheckSetting is the deployment's shared leak check as the kind proofs configure it: the
// stand-in repository these tests read (leakCheckSetting), with the manifest digest of the
// stand-in check the proofs serve (liveLeakCheckMembers), so a role's install under it is the
// check its pod was told (leakguard.Installed).
func liveLeakCheckSetting() (leakguard.Setting, error) {
	members := liveLeakCheckMembers()
	digest, err := leakguard.Manifest(func(name string) ([]byte, error) {
		member, ok := members[name]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(member.Body), nil
	})
	if err != nil {
		return leakguard.Setting{}, fmt.Errorf("the stand-in check's manifest digest: %w", err)
	}
	return leakguard.Setting{Repository: leakCheckSetting.Repository, ManifestSHA256: digest}, nil
}

// liveSessionDSN is the session database URL the kind proofs' providers Secret holds: a
// documentation placeholder, which no pod dials, since every role's agent is a sleep stub and no
// Oh My Pi opens a session.
const liveSessionDSN = "postgres://legion:not-a-secret@sessions.internal.example:5432/sessions"

// liveProvidersSecret is the providers Secret an operator keeps for cfg's project
// (sandbox.ProvidersSecretName), as the kind proofs stand it in: every key cfg's runtime mounts in
// every pod, as sandboxOptions translates the file — the session database's URL key under
// session_store postgres (runtime.kubernetes.session_dsn_secret), and each provider key and
// providers secret, the keys the runtime's own list names (sandbox's Runtime.providers) — each
// holding a placeholder. Without it the kubelet cannot mount any pod's providers volume, and the
// pod waits at Init until its launch times out. The proofs' files configure no provider key and no
// providers secret, so it holds the URL key alone.
func liveProvidersSecret(cfg config.Config, log *slog.Logger) (*corev1.Secret, error) {
	k := *cfg.Runtime.Kubernetes
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		return nil, err
	}
	opts, err := sandboxOptions(cfg, k, project, sandbox.DispatchCredential{Bearer: "placeholder-dispatch-token"}, lookup(nil), log)
	if err != nil {
		return nil, fmt.Errorf("sandboxOptions: %w", err)
	}
	data := map[string][]byte{}
	for _, key := range opts.ProviderKeys {
		data[key] = []byte("placeholder-" + key)
	}
	for _, name := range opts.ProvidersSecrets {
		data[name] = []byte("placeholder-" + name)
	}
	if key := opts.SessionStore.DSNSecretKey(); key != "" {
		data[key] = []byte(liveSessionDSN)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: sandbox.ProvidersSecretName(project), Namespace: k.Namespace}, Data: data}, nil
}

// requireLiveProvidersSecret holds liveProvidersSecret of cfg to the Secret every pod of cfg's
// runtime mounts: the project's providers Secret, in cfg's namespace, holding the session
// database's URL under the key the file names and nothing else.
func requireLiveProvidersSecret(t *testing.T, cfg config.Config) {
	t.Helper()
	secret, err := liveProvidersSecret(cfg, quietLogger())
	if err != nil {
		t.Fatalf("the proof's providers Secret: %v", err)
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	k := cfg.Runtime.Kubernetes
	key := k.SessionStore.DSNSecretKey()
	if secret.Name != sandbox.ProvidersSecretName(project) || secret.Namespace != k.Namespace || len(secret.Data) != 1 || string(secret.Data[key]) != liveSessionDSN {
		t.Fatalf("the proof's providers Secret is %s/%s holding %d keys, want %s/%s holding %s alone",
			secret.Namespace, secret.Name, len(secret.Data), k.Namespace, sandbox.ProvidersSecretName(project), key)
	}
}

// burstableLiveSpec is the root architect's launch of issue as the proof makes it, on
// burstableLiveRepository, with prompt as its role prompt.
func burstableLiveSpec(project, issue string, token claim.Token, bootToken, prompt string) runtime.SpawnSpec {
	return liveRootSpec(project, issue, token, bootToken, prompt, burstableLiveRepository, "master")
}

// liveRootSpec is the root architect's launch of issue on repo, whose default branch is
// defaultBranch, with prompt as its role prompt: burstableLiveSpec on another public repository.
func liveRootSpec(project, issue string, token claim.Token, bootToken, prompt string, repo ghrepo.Repository, defaultBranch string) runtime.SpawnSpec {
	return runtime.SpawnSpec{
		Claim: token, Project: project, Tree: issue, Issue: issue, Role: claim.RoleArchitect, Generation: 1, BootToken: bootToken,
		Secrets: map[string]string{"ENVOY_TOKEN": "placeholder-envoy-token"},
		Prompt:  runtime.PromptParts{RolePromptPaths: []string{prompt}}, Repository: repo, DefaultBranch: defaultBranch,
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
// against an API server standing in for the kind cluster; the providers Secret the driver creates
// holds the key the file names, the runtime's reader of the leak check serves a check a pod's
// launcher installs, and each case's root launch passes the spawn-spec checks. Every step is the
// one the driver takes before it reaches the cluster.
func TestTheBurstableKindProofBuildsItsRuntimesFromItsConfigurations(t *testing.T) {
	t.Parallel()
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
			_, opts, err := burstableLiveRuntime(ctx, cfg, rc, listener, serveLiveLeakCheck(t), quietLogger(), burstableLiveRepository)
			if err != nil {
				t.Fatalf("the proof's runtime: %v", err)
			}
			reservation := opts.Resources.Issue
			for _, kind := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
				if got, want := reservation.Requests[kind], tc.issue.Requests[kind]; got.Cmp(want) != 0 {
					t.Errorf("the file's issue pod requests %s %s, want %s", kind, got.String(), want.String())
				}
				if got, want := reservation.Limits[kind], tc.issue.Limits[kind]; got.Cmp(want) != 0 {
					t.Errorf("the file's issue pod is limited to %s %s, want %s", kind, got.String(), want.String())
				}
			}
			requireLiveProvidersSecret(t, cfg)
			requireLiveLeakCheckInstalls(t, opts.LeakCheck)
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

// requireLiveLeakCheckInstalls holds reader, a kind proof's runtime's reader of the shared public
// leak check, to the check a pod's launcher installs: reader answers the check at the image's pin
// (leakCheckTag), the launcher's own install (leakguard.InstallServed: the unpack, the tag and the
// --help verification) takes what it served, and a role told the proofs' manifest digest
// (liveLeakCheckSetting, under leakguard.CheckerVar) finds that install as its check
// (leakguard.Installed). A runtime with no reader, or a change to the read, the install or the
// digest that the stand-in check no longer meets, fails on every `go test`, not the next time a
// kind cluster runs the proofs.
func requireLiveLeakCheckInstalls(t *testing.T, reader sandbox.LeakCheckReader) {
	t.Helper()
	if reader == nil {
		t.Fatal("the proof's runtime has no reader of the shared public leak check, so every launch whose launcher names the image's pin is refused")
	}
	setting, err := liveLeakCheckSetting()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	served, err := reader.Get(ctx, leakCheckTag)
	if err != nil {
		t.Fatalf("the runtime's read of the stand-in check: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "leak-check")
	// The test process reaps no child it did not start, so the install's run starts as cmd.Start
	// starts it (procgroup.NoReaper); a pod's launcher passes its guard's starter instead.
	if err := leakguard.InstallServed(ctx, leakguard.ServedInstall{Dir: dir, Tag: served.Tag, Body: served.Body, Start: procgroup.NoReaper}); err != nil {
		t.Fatalf("the launcher's install of the stand-in check: %v", err)
	}
	env := []string{leakguard.TagVariable + "=" + leakCheckTag, leakguard.CheckerVar + "=" + setting.ManifestSHA256}
	if installed, err := leakguard.Installed(env, dir); err != nil || installed == "" {
		t.Fatalf("the install at %s reads back as %q, %v; want the check at %s under the manifest digest %s", dir, installed, err, leakCheckTag, setting.ManifestSHA256)
	}
}
