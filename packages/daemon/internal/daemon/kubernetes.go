package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// agentSandbox is the Agent Sandbox a cluster must have installed for the runtime (LEGION-206
// Requirement 10): the Sandbox CRD and the controller's Deployment, as agent-sandbox v1.0.3 names
// them.
var agentSandbox = sandbox.InstallRef{
	CRD: "sandboxes.agents.x-k8s.io", ControllerNamespace: "agent-sandbox-system", ControllerName: "agent-sandbox-controller",
}

// workerImageTools are the worker image's own gh, git, jj, legion and agent-secrets
// (packages/daemon/docker/worker.Dockerfile: git from the distribution, gh and jj copied to
// /usr/local/bin, legion and agent-secrets under /opt/legion/bin).
var workerImageTools = sandbox.Tools{
	GH: "/usr/local/bin/gh", Git: "/usr/bin/git", JJ: "/usr/local/bin/jj", Legion: "/opt/legion/bin/legion",
	AgentSecrets: "/opt/legion/bin/agent-secrets",
}

// imageProbeRetry is how often the daemon tries its worker image again after an attempt that said
// nothing definitive: the daemon's backoff, bounded like `legion probe-image`'s at six attempts.
// Its transient outcomes (a pod that never finished, a kubelet failure) can repeat for a reason no
// wait changes, such as a memory limit too small for Oh My Pi, so an unbounded retry would hold a
// deterministic refusal as a boot that never ends. A probe pod the pool has no room for is no such
// outcome: it is waited on at the policy's longest interval, outside the six (bootprobe.Run,
// Outcome.Waiting), since a full pool is the pool's state and not the image's, and the daemon
// exiting would not make room.
var imageProbeRetry = bootprobe.Image

// sandboxReads is what Agent Sandbox needs that readBoot reads: the cluster's client and the
// runtime's Options.
type sandboxReads struct {
	client *rest.Config
	opts   sandbox.Options
}

// readSandbox is Agent Sandbox's share of readBoot (C1's translation, C3): the cluster's client from
// runtime.kubernetes' kubeconfig, and the Options every value of the configuration becomes, with
// paneNatsUser, the public key of the pane NATS nkey seed's user ("" with none), which the image
// probe holds the providers Secret's seed to. None of the host's own agent machinery is read: no Oh
// My Pi invocation or plugin gate (the image probe proves the image's), no Dispatch token file (a
// pod reads its bearer from its claim's Secret), no secretsd provider keys (a pod mounts its keys
// from the providers Secret), and no host gh, git, or jj (a pod runs the image's).
func readSandbox(cfg config.Config, project, dispatchToken, paneNatsUser string, lookup func(string) (string, bool), log *slog.Logger) (sandboxReads, error) {
	k := *cfg.Runtime.Kubernetes
	rc, err := kubeClient(k)
	if err != nil {
		return sandboxReads{}, err
	}
	opts, err := sandboxOptions(cfg, k, project, dispatchToken, lookup, log)
	if err != nil {
		return sandboxReads{}, err
	}
	opts.NATSUser = paneNatsUser
	return sandboxReads{client: rc, opts: opts}, nil
}

// PluginsFromFile and PluginsFromBinary name ResolvedPlugins' source: the file's
// `runtime.kubernetes.plugins`, or the release binary's linked pair (cmd/legion's
// piLegionVersion/piEnvoyVersion, applyLinkedPlugins).
const (
	PluginsFromFile   = "legion.yaml"
	PluginsFromBinary = "binary"
)

// ResolvedPlugins is the plugin pair this Kubernetes daemon installs for a stale-plugin relaunch
// (LEGION-164), and where it came from: the file's `runtime.kubernetes.plugins` when the file sets
// one (PluginsFromFile), else the binary's linked pair when main filled cfg.Runtime.Kubernetes.Plugins
// from it (cmd/legion's piLegionVersion/piEnvoyVersion, PluginsFromBinary field true; PluginsFromBinary
// source), else nil pair and "" source — the daemon knows no plugin versions, and every stale claim is
// recorded and reported, never relaunched or refused (supervise.Deps.PluginRelaunchable false). Nil
// under tmux, whose pane always resumes on the plugin installed on the daemon's host. `legion
// config check` and the boot log both read this.
func ResolvedPlugins(cfg config.Config) (*runtime.PluginInstall, string) {
	k := cfg.Runtime.Kubernetes
	if k == nil || k.Plugins == nil {
		return nil, ""
	}
	source := PluginsFromFile
	if k.PluginsFromBinary {
		source = PluginsFromBinary
	}
	return &runtime.PluginInstall{PiLegion: k.Plugins.PiLegion, PiEnvoy: k.Plugins.PiEnvoy, Contract: api.DaemonAPIVersion}, source
}

// probeBudgetMargin is how far the image probe's attempt budget must clear
// worker_boot_timeout_seconds (ProbeBudget): long enough that the capacity stop (stuck,
// internal/runtime/sandbox/probe.go) can see a probe pod's Unschedulable age pass the boot
// timeout within the same attempt, rather than the attempt ending first and the Outcome.Waiting
// branch never firing (LEGION-636: a deployment whose worker_boot_timeout_seconds reaches or
// exceeds slow_command_timeout_seconds never gets there).
const probeBudgetMargin = 60 * time.Second

// ProbeBudget is how long the image probe's one attempt waits for its pod (ImageProbe.Budget):
// slow_command_timeout_seconds, or, when worker_boot_timeout_seconds plus probeBudgetMargin is
// larger, that instead — derived true in the second case. sandboxBootOf (a booting daemon) and
// probeBoot (`legion probe-boot`) both build their ImageProbe through imageProbe, so both carry
// the same derived budget.
func ProbeBudget(cfg config.Config) (budget time.Duration, derived bool) {
	if bound := cfg.WorkerBootTimeout + probeBudgetMargin; bound > cfg.SlowCommandTimeout {
		return bound, true
	}
	return cfg.SlowCommandTimeout, false
}

// ProbeBudgetLine is the plain line `legion config check` prints under
// `runtime: kubernetes` when ProbeBudget's bound exceeds slow_command_timeout_seconds: how long
// each attempt now waits, and which keys pushed it there. "", false when the bound is
// slow_command_timeout_seconds itself.
func ProbeBudgetLine(cfg config.Config) (line string, derived bool) {
	budget, derived := ProbeBudget(cfg)
	if !derived {
		return "", false
	}
	return fmt.Sprintf("image probe: each attempt waits %ds: worker_boot_timeout_seconds (%d) + %ds is above slow_command_timeout_seconds (%d)",
		int(budget.Seconds()), int(cfg.WorkerBootTimeout.Seconds()), int(probeBudgetMargin.Seconds()), int(cfg.SlowCommandTimeout.Seconds())), true
}

// logProbeBudget is the boot's one line when ProbeBudget derives a bound above
// slow_command_timeout_seconds: an Info line naming the derived bound and both keys. Nothing is
// logged when the bound is slow_command_timeout_seconds itself.
func logProbeBudget(log *slog.Logger, cfg config.Config) {
	budget, derived := ProbeBudget(cfg)
	if !derived {
		return
	}
	log.Info("daemon: the image probe's attempt outlives slow_command_timeout_seconds",
		"budgetSeconds", int(budget.Seconds()),
		"slow_command_timeout_seconds", int(cfg.SlowCommandTimeout.Seconds()),
		"worker_boot_timeout_seconds", int(cfg.WorkerBootTimeout.Seconds()))
}

// logPlugins is the boot's one line on what this Kubernetes daemon will install for a
// stale-plugin relaunch: an Info line naming the pair and where it came from, or, with none, the
// Warn naming the gap: a stale claim is recorded and reported but never relaunched or refused.
func logPlugins(log *slog.Logger, plugins *runtime.PluginInstall, source string) {
	if plugins == nil {
		log.Warn("daemon: this daemon knows no plugin versions to relaunch a stale process onto; set runtime.kubernetes.plugins or run a release build")
		return
	}
	log.Info("daemon: stale-plugin relaunches install", "piLegion", plugins.PiLegion, "piEnvoy", plugins.PiEnvoy, "source", source)
}

// prepareSandbox is the runtime over readSandbox's client and Options, and the part of boot its
// worker pod can refuse (sandboxBootOf). The worker stream listens on tcp://<bind>:<worker_stream_port>;
// openSupervision hands the factory the address every pod dials (shimAddress).
func prepareSandbox(cfg config.Config, o overrides, reads sandboxReads, p *plan) error {
	p.stream = "tcp://" + net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.WorkerStreamPort))
	if o.runtime != nil {
		p.newRuntime, p.probe = o.runtime, o.probe
		return nil
	}
	boot, err := sandboxBootOf(cfg, reads)
	if err != nil {
		return err
	}
	// The memory guards' kill holder (memoryGuardKills), on the plan by pointer, so the plan's own
	// copy carries the one holder the factory closed over, as plan.repositories does.
	p.memoryGuardKills = &memoryGuardKills{}
	// The holder the pods' cache trimmers report through, made here and filled once the workflow
	// is open: the factory is built before the workflow exists, and the fact path does not
	// (volumeTrimReports, daemon.Run). It hangs off the plan by pointer, so the plan's own copy
	// carries the one holder the factory closed over, as plan.repositories does.
	p.volumeReports = &volumeTrimReports{}
	p.newRuntime = sandboxRuntime(reads.client, reads.opts, p.repositories.Accepted, p.repositories.serving, cfg.RepositoryRenames, p.installed, p.left, o.githubAPI,
		p.memoryGuardKills, p.volumeReports)
	p.clusterCheck = boot.clusterCheck
	p.claimsCheck = func(ctx context.Context, st *store.Store) error {
		legacy, err := st.HasLegacySandboxClaims(ctx, p.project)
		if err != nil {
			return err
		}
		if legacy {
			return fmt.Errorf("refuse the Kubernetes runtime before any schema write: project %s still has legacy per-claim Sandbox locators", p.project)
		}
		return nil
	}
	p.probe = func(ctx context.Context, rt runtime.Runtime) (bootprobe.ImageReport, error) {
		sandboxed, ok := rt.(*sandbox.Runtime)
		if !ok {
			return bootprobe.ImageReport{}, fmt.Errorf("the image probe needs the Agent Sandbox runtime, not %T", rt)
		}
		return sandboxed.ProbeImage(ctx, boot.probe)
	}
	return nil
}

// sandboxBoot is the part of a Kubernetes daemon's boot its worker pod can refuse: the cluster's
// refusals and the image probe. prepareSandbox hands both to the boot plan, and probe-boot
// (probeBoot) runs them alone, so the two run one check and one probe by construction.
type sandboxBoot struct {
	// clusterCheck is the cluster's half of boot's refusals (plan.clusterCheck), each read-only:
	// Agent Sandbox is installed, and no Sandbox of a layout before this runtime's remains in the
	// namespace for the project. The census logs what it accepts but will not keep — an issue
	// Sandbox whose next launch replaces its disk — to the runtime's own logger.
	clusterCheck func(context.Context) error
	// probe is the image probe the runtime runs (sandbox.ImageProbe), over the references of the
	// role prompts every worker pod is handed (prompts.RoleReferences).
	probe sandbox.ImageProbe
}

// sandboxBootOf is sandboxBoot for the configuration and boot's reads.
func sandboxBootOf(cfg config.Config, reads sandboxReads) (sandboxBoot, error) {
	probe, err := imageProbe(cfg, prompts.RoleReferences())
	if err != nil {
		return sandboxBoot{}, err
	}
	clusterCheck := func(ctx context.Context) error {
		checking, cancel := context.WithTimeout(ctx, cfg.SlowCommandTimeout)
		defer cancel()
		if err := sandbox.CheckInstalled(checking, reads.client, agentSandbox); err != nil {
			return err
		}
		return sandbox.CensusLegacyIssueSandboxes(ctx, reads.client, reads.opts.Namespace, reads.opts.Project, reads.opts.IssueDisks(), reads.opts.Log)
	}
	return sandboxBoot{clusterCheck: clusterCheck, probe: probe}, nil
}

// probeReservation is the image probe pod's own reservation, fixed: `legion probe-image` starts
// one Oh My Pi at a time, each briefly (pi.agents, the plugin's load, the session-storage
// setting), runs no lane, holds its TLS clients and headless Chromium to the chains its own server
// serves and to two public test sites, and exits. It reserves 500m so the probe ends at least 60 s
// inside its 240 s command budget at the defaults. Five runs of `legion probe-image
// --skip-agent-models` at 1 CPU under gVisor (2026-11-18) took 51.9–76.2 s, 19.40–35.99 s of it on
// CPU. Predicted at 250m (CPU time × 4, plus the wall time off CPU), the worst takes 184 s, 56 s
// inside the budget, short of the 60 s; at 500m (CPU time × 2) it takes 112 s, 128 s inside. The
// runs are at 1 CPU because the builds namespace refuses a Job under 1 CPU, so the 500m figures are
// predictions. Once the probe served its own TLS chains (2026-11-20), its runs in the worker image
// of 1c7c825367 in a CPU-quota scope on a development host took 125.4–147.3 s at 500m (eight runs,
// 47.1–56.6 s on CPU), 93 s inside the budget at the slowest, and 73.5–104.4 s at 1 CPU (thirteen
// runs), with a peak of 358 MiB; on that host main's probe took 66.5–91.1 s at 1 CPU, slower than
// under gVisor.
// Five fresh runs at 250m / 1Gi, before the probe held gcloud, bq, az, mise, wget, git, go,
// kubectl, redis-cli and socat, took 57.68–77.43 s, so 1Gi held the probe, and the reservation
// still fits a pool that cannot place a controller or issue pod. It has a 5Gi bound on node disk
// and a 1Gi disk request. The probe runs at every boot, before anything is served, and a probe that
// reserved a pod's share — the controller's 1 CPU and 4Gi, or an issue pod's 3 CPU and 19Gi —
// could leave the daemon unable to boot on a pool full enough to place nothing that large (stage
// 4b's daemon-controller-liveness restart, whose controller reservation no node can hold, met
// exactly that).
var probeReservation = config.Reservation{CPU: "500m", CPURequest: "500m", Memory: "1Gi", MemoryRequest: "1Gi", EphemeralStorage: "5Gi", EphemeralStorageRequest: "1Gi"}

// imageProbe is the probe of the daemon's worker image: this daemon's contract, the role prompts
// every pod is handed — this daemon's, inlined at each launch, so the probe resolves what they name
// rather than the image's copy — and the probe's own reservation (probeReservation), translated as
// every pod's is (requirements), its requests equal to its limits so the probe pod is Guaranteed
// whatever an issue pod requests: a deployment's Burstable issue pods never reach the probe. The error is
// the translation's, which only an edit to probeReservation can cause. Budget is ProbeBudget's.
func imageProbe(cfg config.Config, references promptrefs.Names) (sandbox.ImageProbe, error) {
	resources, err := requirements("the image probe's reservation", probeReservation)
	if err != nil {
		return sandbox.ImageProbe{}, err
	}
	budget, _ := ProbeBudget(cfg)
	return sandbox.ImageProbe{
		Contract: api.DaemonAPIVersion, Budget: budget, Retry: imageProbeRetry,
		RoleReferences: references,
		Resources:      resources,
	}, nil
}

// kubeClient is the client of runtime.kubernetes: the kubeconfig's context it names, or the
// kubeconfig's current one, which must then exist (decision 4: the restricted role's kubeconfig
// sets none, so the configuration names it); with no kubeconfig, the pod's in-cluster credentials.
func kubeClient(k config.Kubernetes) (*rest.Config, error) {
	if k.Kubeconfig == "" {
		rc, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("runtime.kubernetes names no kubeconfig, so the daemon reads in-cluster credentials: %w", err)
		}
		return rc, nil
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: k.Kubeconfig}, &clientcmd.ConfigOverrides{CurrentContext: k.Context})
	if k.Context == "" {
		raw, err := loader.RawConfig()
		if err != nil {
			return nil, fmt.Errorf("read runtime.kubernetes.kubeconfig %s: %w", k.Kubeconfig, err)
		}
		if raw.CurrentContext == "" {
			return nil, fmt.Errorf("runtime.kubernetes.context is required: the kubeconfig %s sets no current context", k.Kubeconfig)
		}
	}
	rc, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("runtime.kubernetes.kubeconfig %s: %w", k.Kubeconfig, err)
	}
	return rc, nil
}

// sandboxOptions translates the configuration into the runtime's Options, all but the worker
// stream's address, the connection directory and the token source, which boot hands the factory.
// The issue pod's one container gets runtime.kubernetes.resources.issue as its requirements, and
// the daemon-launched controller's container runtime.kubernetes.resources.controller (requirements).
// Every arm of every experiment the project declares reaches the runtime whatever the
// experiment's mode is (experiment.DeclaredVariants), as they reach tmuxOptions: a unit already
// drawn into an arm names that arm's overlays under any mode. A project that declares no
// experiment of its own still declares the memory experiment and hands over its two arms, which
// name no file where the project writes no `memory` block and so add nothing to a launch.
// lookup is the daemon's environment, which can name the NATS nkey seed (launchSecrets).
func sandboxOptions(cfg config.Config, k config.Kubernetes, project, dispatchToken string, lookup func(string) (string, bool), log *slog.Logger) (sandbox.Options, error) {
	issueVolume, err := resource.ParseQuantity(k.IssueVolume)
	if err != nil {
		return sandbox.Options{}, fmt.Errorf("runtime.kubernetes.issue_volume: %w", err)
	}
	var tolerations []corev1.Toleration
	for _, t := range k.Scheduling.Tolerations {
		tolerations = append(tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value, Effect: corev1.TaintEffect(t.Effect),
		})
	}
	issue, err := requirements("runtime.kubernetes.resources.issue", k.Resources.Issue)
	if err != nil {
		return sandbox.Options{}, err
	}
	controller, err := requirements("runtime.kubernetes.resources.controller", k.Resources.Controller)
	if err != nil {
		return sandbox.Options{}, err
	}
	var agentSecrets *sandbox.AgentSecrets
	if a := k.AgentSecrets; a != nil {
		agentSecrets = &sandbox.AgentSecrets{URL: a.URL, Audience: a.Audience, TokenExpiry: time.Duration(a.TokenExpirySeconds) * time.Second, ExchangeAudience: a.ExchangeAudience}
	}
	cache, err := repositoryCache(k.RepositoryCache)
	if err != nil {
		return sandbox.Options{}, err
	}
	// The guard's settings reach an accepted issue-pod launcher on the answer to its hello
	// (sandbox Runtime.LauncherResolver). Nil is what the launcher reads as "run no guard", so a
	// deployment that turned runtime.kubernetes.memory_guard.issue_pods off and a daemon built
	// before the guard write the same ack (shimwire.LauncherHelloAck.MemoryGuard, INFRA-3557).
	var memoryGuard *shimwire.LauncherMemoryGuard
	if g := k.MemoryGuard.IssuePods; g.Enabled {
		memoryGuard = &shimwire.LauncherMemoryGuard{
			ActPercent: g.ActPercent, FloorPercent: g.FloorPercent,
			WaitSeconds: g.WaitSeconds, IntervalMilliseconds: g.IntervalMilliseconds,
		}
	}
	// The trimmer's settings reach an accepted issue-pod launcher on the answer to its hello
	// (sandbox Runtime.LauncherResolver). Nil is what the launcher reads as "no settings", on
	// which its trimmer reads and deletes nothing, so a deployment that turned
	// `runtime.kubernetes.volume_trim` off and a daemon built before this work write the same ack
	// (shimwire.LauncherHelloAck.VolumeTrim, INFRA-2009).
	var volumeTrim *shimwire.LauncherVolumeTrim
	if t := k.VolumeTrim; t.Enabled {
		volumeTrim = &shimwire.LauncherVolumeTrim{
			HighPercent: t.HighPercent, LowPercent: t.LowPercent,
			MinAgeMinutes: t.MinAgeMinutes, IntervalSeconds: t.IntervalSeconds,
		}
	}
	opts := sandbox.Options{
		Namespace: k.Namespace, Project: project, Image: k.Image, StorageClass: k.StorageClass, IssueVolume: issueVolume,
		Scheduling: sandbox.Scheduling{NodeSelector: k.Scheduling.NodeSelector, Tolerations: tolerations, PriorityClass: k.Scheduling.PriorityClass},
		Sandbox:    sandboxConfig(k.Sandbox),
		Resources:  sandbox.PodResources{Issue: issue, Controller: controller},
		DaemonURL:  cfg.DaemonURL, EnvoyURL: cfg.EnvoyURL, DispatchURL: cfg.DispatchURL, DispatchToken: dispatchToken,
		NATSURLs: cfg.NatsURLs,
		// The controller this runtime launches is told the project's channels, and is relaunched
		// when they change (sandbox's address record).
		ControllerSlackChannels: cfg.Projects[cfg.Project].SlackChannels,
		Tools:                   workerImageTools,
		Pod:                     sandbox.Pod(k.Pod),
		RoleConfigFiles:         cfg.RoleConfigFiles,
		ProviderKeys:            providerSecretKeys(cfg.ProviderKeys),
		LaunchSecrets:           launchSecretNames(cfg, lookup),
		ProvidersSecrets:        providersSecrets(cfg, lookup),
		SessionStore:            k.SessionStore,
		Setup:                   k.Setup,
		TestPostgres:            k.TestPostgres,
		MemoryGuard:             memoryGuard,
		BootTimeout:             cfg.WorkerBootTimeout,
		RegistrationIntervals:   cfg.WorkerBootRegistrationDeadlineIntervals,
		BusyBackoff:             busyBackoffFirst,
		TerminationGrace:        cfg.WorkerStopTimeout,
		ProbeInterval:           cfg.ProbeInterval,
		AdoptTimeout:            cfg.SlowCommandTimeout,
		AgentSecrets:            agentSecrets,
		RepositoryCache:         cache,
		VolumeTrim:              volumeTrim,
		Log:                     log,
	}
	opts.ExperimentVariants = experiment.DeclaredVariants(cfg.ExperimentsDeclared())
	return opts, nil
}

// sandboxConfig is runtime.kubernetes.sandbox as the runtime takes it: the settled block, member
// for member, the microVM storage class already defaulted to runtime.kubernetes.storage_class.
func sandboxConfig(c config.SandboxConfig) sandbox.SandboxConfig {
	settled := sandbox.SandboxConfig{Type: sandbox.SandboxType(c.Type), CanaryIssues: c.CanaryIssues}
	if m := c.MicroVM; m != nil {
		settled.MicroVM = &sandbox.MicroVM{ComputeClass: m.ComputeClass, StorageClass: m.StorageClass, KeepVolume: m.KeepVolume}
		if endpoint := m.Metadata; endpoint != nil {
			settled.MicroVM.Metadata = &sandbox.MetadataEndpoint{WorkloadPool: endpoint.WorkloadPool, IdentityProvider: endpoint.IdentityProvider}
		}
	}
	return settled
}

// repositoryCache is runtime.kubernetes.repository_cache as the runtime takes it: the volume as a
// quantity, the intervals as durations, and the reservation as a container's requirements.
func repositoryCache(c config.RepositoryCacheConfig) (sandbox.RepositoryCache, error) {
	volume, err := resource.ParseQuantity(c.Volume)
	if err != nil {
		return sandbox.RepositoryCache{}, fmt.Errorf("runtime.kubernetes.repository_cache.volume: %w", err)
	}
	resources, err := requirements("runtime.kubernetes.repository_cache.resources", c.Resources)
	if err != nil {
		return sandbox.RepositoryCache{}, err
	}
	return sandbox.RepositoryCache{
		Volume: volume, Refresh: time.Duration(c.RefreshMinutes) * time.Minute, Retain: time.Duration(c.RetainHours) * time.Hour,
		Resources: resources,
	}, nil
}

// providersSecrets are the launch secrets a pod reads from the providers Secret's key of the same
// name, never from a copy in its own Secret: the NATS nkey seed, when the daemon has one. The one
// copy the cluster holds is the deployment's, in the Secret every pod already mounts; the daemon's
// own file (or variable) is where the daemon reads it, and the two must hold the same seed.
func providersSecrets(cfg config.Config, lookup func(string) (string, bool)) []string {
	if natsauth.Configured(cfg.NatsNkeySeedFile, lookup) {
		return []string{natsauth.SeedVariable}
	}
	return nil
}

// newSecretsLogin is the daemon's agent-secrets login as the machines' Enroller: it constructs the
// client from runtime.kubernetes.agent_secrets and starts it at boot on a background context, so
// the daemon logs in with its own projected ServiceAccount token (token_file) and keeps logging in
// before each credential expires; no person approves any of it, a restart included. Every login
// logs its own outcome (agentsecrets.Client.Login); one that fails is retried by the client, and
// pod enrollment waits for it. With the block's exchange_audience set, the same client is the
// machines' Registrar too (newRegistrar), which registers each role's session for its pod; unset,
// the Registrar is nil and no registration is ever made. The client itself is returned too,
// read-only, so the state route can show the login's current status (source.State,
// agentsecrets.Client.LoginStatus). Never part of launchSecrets, so no pod is ever handed the
// daemon's key, token or credential. Nil, nil, nil without the block.
func newSecretsLogin(cfg config.Config, log *slog.Logger) (supervise.Enroller, supervise.Registrar, *agentsecrets.Client) {
	k := cfg.Runtime.Kubernetes
	if k == nil || k.AgentSecrets == nil {
		return nil, nil, nil
	}
	// Every call this client makes to the broker carries a Proof header of its own, so a 3xx
	// answered for the broker - by the broker, or by anyone who can answer for it - must not
	// hand that proof to a host this deployment never named. The refusal is written here as
	// internal/modeltoken writes it, since envoy's redirects package is internal to that
	// module; its comment is where the whole reason is.
	broker := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	client := &agentsecrets.Client{URL: k.AgentSecrets.URL, HTTP: broker, TokenFile: k.AgentSecrets.TokenFile, Log: log}
	go func() { _ = client.Start(context.Background()) }()
	return brokerEnroller{client: client}, newRegistrar(*k.AgentSecrets, client), client
}

// brokerEnroller is agentsecrets.Client as supervise.Enroller; an *agentsecrets.APIError is a
// PermanentError when its status says so, which is how the machine tells a refusal from an outage,
// and an ExpiredTokenError for POD_TOKEN_EXPIRED, a stale token under a key that stands.
type brokerEnroller struct{ client *agentsecrets.Client }

func (b brokerEnroller) Enroll(ctx context.Context, e supervise.PodEnrollment) (string, error) {
	enrolled, err := b.client.Enroll(ctx, agentsecrets.PodEnrollment{
		PodUID: e.PodUID, Slot: e.Slot, Thumbprint: e.Thumbprint, PodToken: e.PodToken, Session: e.Session, Claim: e.Claim,
	})
	if err != nil {
		return "", err
	}
	return enrolled.ID, nil
}

func (b brokerEnroller) Revoke(ctx context.Context, id string) error { return b.client.Revoke(ctx, id) }

func (b brokerEnroller) Carried(ctx context.Context, sessionIDs []string) ([]supervise.CarriedEnrollment, error) {
	carried, err := b.client.Carried(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	out := make([]supervise.CarriedEnrollment, len(carried))
	for i, c := range carried {
		out[i] = supervise.CarriedEnrollment(c)
	}
	return out, nil
}

// providerSecretKeys are provider_keys as the runtime takes them: each variable Oh My Pi reads,
// to the key of the providers Secret that holds it; nil when the file names none.
func providerSecretKeys(keys []config.ProviderKey) map[string]string {
	if len(keys) == 0 {
		return nil
	}
	secretKeys := make(map[string]string, len(keys))
	for _, key := range keys {
		secretKeys[key.Env] = key.Secret
	}
	return secretKeys
}

// requirements is a reservation as a container's requirements: its cpu and memory limits, and its
// cpu and memory requests, each at most its limit (the loader holds them there) — equal to it at
// the defaults, so the pod the containers make is Guaranteed and bursts past nothing, and below it
// where the file set a lower request, which makes the pod Burstable (the kubelet's QoS reads cpu
// and memory alone); and its ephemeral storage, the limit on what the container writes to the
// node's disk and the smaller request the scheduler fits to the node's allocatable disk. The two
// lists are built apart so neither edit reaches the other. key names the reservation in an error:
// a pod's configuration key, or the probe's.
func requirements(key string, reservation config.Reservation) (corev1.ResourceRequirements, error) {
	requests, limits := corev1.ResourceList{}, corev1.ResourceList{}
	for _, field := range []struct {
		name          corev1.ResourceName
		list          corev1.ResourceList
		key, quantity string
	}{
		{corev1.ResourceCPU, limits, "cpu", reservation.CPU},
		{corev1.ResourceCPU, requests, "cpu_request", reservation.CPURequest},
		{corev1.ResourceMemory, limits, "memory", reservation.Memory},
		{corev1.ResourceMemory, requests, "memory_request", reservation.MemoryRequest},
		{corev1.ResourceEphemeralStorage, limits, "ephemeral_storage", reservation.EphemeralStorage},
		{corev1.ResourceEphemeralStorage, requests, "ephemeral_storage_request", reservation.EphemeralStorageRequest},
	} {
		quantity, err := resource.ParseQuantity(field.quantity)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("%s.%s: %w", key, field.key, err)
		}
		field.list[field.name] = quantity
	}
	return corev1.ResourceRequirements{Requests: requests, Limits: limits}, nil
}

// sandboxRuntime builds the Agent Sandbox runtime, whose informers run for ctx (supervision's
// lifetime), over the worker stream and with the workflow's App tokens as every pod's provisioning
// token source (the implement App's, minted for the repository's owner) and every tree role's gh
// bundle (gitHubCredential, a directory for each owner the role's App is installed on, read from
// GitHub at every bundle, and one for each owner a configured rename moved a repository away
// from while repos holds the name it moved to), with the
// controller's own bundle beside it
// (appauth.ControllerCredential), which a controller's launch and its refresh both ask by owner,
// and the directory set those directories are compared with (appauth.GitHubDirs), which the
// runtime reads a running pod's gh projection against — a pod lacking an owner's directory
// holds no token of that owner (sandbox's movedGhDirs) — the three over two records of what
// the bundles found, both the plan's own (plan.installed, plan.left): the owners each App's
// last bundle read installed (appauth.Installed),
// which is what the drift check may honestly compare a pod against without a call to GitHub of
// its own, and the directories the mints found GitHub refuses (appauth.LeftOut), so the set
// a pod is compared against is the set a mint produces and an owner left out of every bundle of
// the Apps that pod runs is named by no comparison of it — the roles the runtime asks for being
// its pod's own, so the implement App's refusal of an owner costs the controller's pod, which
// runs the review App alone, nothing — with
// serving as the rule every provisioning token's repository is read through
// (repositories.serving, the mapping's own, so a configured rename's mint follows the owner
// whose installation holds the repository now), with the repositories the project works as the
// ones its repository caches follow (mapped: the mapping's and those of its live issue records,
// workflowRuntime.cacheRepositories), with the store's own workspace bundles as what a
// replacement pod's launcher is sent and what its restore drops, and registers its launcher
// acceptor on the stream listener. A daemon with no GitHub Apps hands it neither tokens nor a
// mapping, and sandbox.New's own refusal names what is missing. Boot has already run the cluster
// check (plan.clusterCheck): Agent Sandbox is installed and no Sandbox of a layout before this
// one — per-claim pods, or issue pods on one tree volume — remains. githubAPI is GitHub's REST
// root ("" is https://api.github.com), which the shared public leak check is read under
// (leakCheckSource).
//
// kills is the memory guards' kill holder (memoryGuardKills).
//
// reports is the holder the launchers' volume reports reach the workflow's fact path through
// (volumeTrimReports): it is built with this factory, before the workflow opens, and filled once
// it has (plan.volumeReports). One that is never filled refuses every report it is given with
// errVolumeReportsUnbound, which the launcher session logs.
func sandboxRuntime(rc *rest.Config, opts sandbox.Options, repos func() []ghrepo.Repository,
	serving func(ghrepo.Repository) ghrepo.Repository, renames ghrepo.Renames, installed *appauth.Installed, left *appauth.LeftOut, githubAPI string, kills *memoryGuardKills,
	reports *volumeTrimReports) runtimeFactory {
	return func(ctx context.Context, listener *stream.Listener, address string, apps appauth.Source,
		_ func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, mapped func() []ghrepo.Repository) (runtime.Runtime, error) {
		opts.Conns, opts.StreamURL, opts.Store, opts.Mapped = listener, address, st, mapped
		// The two halves of the release's saved work: an idle release puts the issue's unpushed
		// work here (outbox's release), and the runtime hands it to the launcher of the pod that
		// replaces the released one before any role of that pod starts. The confirmation deletes
		// the bundle of the release the launcher's report names and no other, so a report that
		// arrives late takes no newer release's work with it. The read is the bundle's one restore
		// attempt: the first pod that is not the one the work came off takes it, whether the
		// bundle is sent or that pod is refused as no newer than the save, and nothing is
		// answered for that bundle again.
		opts.SavedWorkspaceBundle, opts.WorkspaceBundleRestored = savedWorkspaceBundle(st, opts.Project), st.DeleteRestoredWorkspaceBundle
		// A pod whose Sandbox owns the issue's kept volume, and was created before the save,
		// mounts the volume the work was taken off. The runtime asks this drop first for such a
		// pod, and the store deletes the bundle the take would answer, without its body, so the
		// pod is sent nothing and the bundle holds the issue's pod no longer.
		opts.HeldWorkspaceBundle = heldWorkspaceBundle(st, opts.Project)
		// The cache trimmer's own half: every report a pod's trimmer sends — of a sweep that
		// left the volume above its low mark, or of the trimmer switching itself off — reaches
		// the daemon through this, and the daemon turns it into the one fact that can tell the
		// issue's architect (volumeTrimReports.report). The workflow is what applies that fact,
		// and it opens after this factory is built, so the pair the apply needs is held in
		// reports and filled when it does (plan.volumeReports).
		opts.VolumeReported = reports.report
		// The shared public leak check each pod's launcher is pushed before any role of the pod
		// starts, read with the implement App's read lease on the check's owner (leakCheckToken);
		// a daemon with no Apps reads none, and the runtime refuses the start of a role whose
		// launcher names a tag with the cause that says so.
		opts.LeakCheck = leakCheckSource(apps, githubAPI)
		// Each process an issue pod's memory guard ends reaches the daemon through the kill
		// holder (memoryGuardKills).
		opts.MemoryGuardKilled = kills.killed
		if apps != nil {
			// Two records per project, written by the two credentials and read by the directory
			// set for the Apps of the pod it answers for: the owners each App's last bundle read
			// installed (appauth.Installed), which is all the drift check may say about a pod
			// without calling GitHub per pod per sweep, and the directories a mint found GitHub
			// refuses, so no pod is replaced for a directory no mint of its own Apps can produce
			// (appauth.LeftOut). Both are the daemon's own (plan.installed, plan.left), which
			// every launch's addressing reads its role's App's owners out of beside the drift
			// check.
			opts.Tokens = newImplementTokens(apps, serving)
			opts.GitHubCredential = appauth.GitHubCredential(apps, repos, renames, installed, left, opts.Log)
			opts.ControllerCredential = appauth.ControllerCredential(apps, repos, renames, installed, left, opts.Log)
			opts.GitHubDirs = func(repo ghrepo.Repository, roles []appauth.AppRole) []string {
				return appauth.GitHubDirs(repos, renames, installed, left, roles, repo)
			}
		}
		rt, err := sandbox.New(ctx, rc, opts)
		if err != nil {
			return nil, err
		}
		listener.SetLauncherResolver(rt.LauncherResolver())
		return rt, nil
	}
}

// savedWorkspaceBundle is st's take of a release's saved work for a pod of project, the runtime's
// project token (sandbox.Options.Project), in the shape the runtime calls it
// (runtime.SavedWorkspaceBundle). A bundle is project's only where project records its issue
// (store.Store.TakeWorkspaceBundleForRestore): on a database shared with another project's
// daemon, a bundle of that project's issue is that daemon's, whatever pod of this daemon asks.
func savedWorkspaceBundle(st *store.Store, project string) runtime.SavedWorkspaceBundle {
	return func(ctx context.Context, issue, podUID string, podCreated time.Time) (runtime.WorkspaceBundle, runtime.WorkspaceBundleTake, error) {
		return st.TakeWorkspaceBundleForRestore(ctx, project, issue, podUID, podCreated)
	}
}

// heldWorkspaceBundle is st's drop of a release's saved work that the kept volume of a pod of
// project already holds, in the shape the runtime calls it (runtime.HeldWorkspaceBundleDrop).
// project is the runtime's project token (sandbox.Options.Project). The drop follows the take's
// project rule (store.Store.DropHeldWorkspaceBundle): a bundle of an issue that project does not
// record is another project's daemon's, and a pod of this daemon drops none of it.
func heldWorkspaceBundle(st *store.Store, project string) runtime.HeldWorkspaceBundleDrop {
	return func(ctx context.Context, issue, podUID string, keptSince time.Time) (runtime.WorkspaceBundle, bool, error) {
		return st.DropHeldWorkspaceBundle(ctx, project, issue, podUID, keptSince)
	}
}

// memoryGuardKills turns one process an issue pod's memory guard ended, as the pod's launcher
// reported it, into the fact the workflow applies (intake.MemoryGuardKill). It is the seam between
// the Kubernetes runtime factory (sandboxRuntime), which prepareSandbox builds before the workflow
// opens, and the fact path, which exists only after it: prepareSandbox makes one and hands it to
// the factory, and the boot fills it the moment the workflow is open (plan.memoryGuardKills,
// daemon.run). A holder nobody filled refuses every report with errMemoryGuardKillsUnbound, which
// the launcher session logs with the report it drops.
//
// It is filled once, in the boot's own goroutine, before the runtime is built and so before any
// launcher can connect: there is nothing to lock, and nothing ever replaces what was filled.
type memoryGuardKills struct {
	pool     *store.Pool
	handlers []intake.Handler
}

// errMemoryGuardKillsUnbound is what a holder the boot never filled answers every report with. Its
// text names the line that fills it, since a log line is then the only place the drop shows: the
// role whose process was ended is told nothing. An unbound holder can only be a boot bug, never a
// daemon that runs no workflow: prepareSandbox makes the holder only under runtime: kubernetes, and
// configuration load refuses a runtime: kubernetes file that names no dispatch_url (config's
// checkKubernetesKeys, "the workflow is what launches every pod"), so every daemon with a holder
// opens the workflow whose boot fills it.
var errMemoryGuardKillsUnbound = errors.New("the daemon's memory guard kill holder was never bound to the workflow " +
	"(daemon.go, plan.memoryGuardKills.bind), so this report and every later one is dropped")

// bind gives the holder the daemon's guarded pool (store.Pool) and the handlers a fact applies
// through, which is what the workflow has and the factory does not.
func (m *memoryGuardKills) bind(pool *store.Pool, handlers []intake.Handler) {
	m.pool, m.handlers = pool, handlers
}

// killed applies one kill of issue's pod as one intake fact (runtime.MemoryGuardKilled), crediting
// it to token, the role whose connection carried the report, and to podUID, the pod that launcher
// runs in, both the session's and never the report's. It runs on the launcher's own frame loop
// under that loop's bounded context, so an apply that cannot be made is returned to the launcher
// session, which logs it and drops the report. Every launcher's frame loop runs beside the others
// and beside the daemon's own loops, so each report applies under a hold mark of its own
// (store.ForConcurrentWorker): this is a fact source, and a fact source marks its context where
// it starts (store.ErrNestedAcquire).
//
// The event id is the kill's own identity, the guard's id and its sequence
// (memoryGuardKillEventID), and never the daemon's receipt: a launcher that wrote a report on a
// connection it then lost sends it again on the next, and ApplyFact applies one id once, so the
// role is told of one kill once however often its report arrives.
func (m *memoryGuardKills) killed(ctx context.Context, issue string, token claim.Token, podUID string, report shimwire.LauncherMemoryKill) error {
	if m.pool == nil || len(m.handlers) == 0 {
		// Nothing bound the holder to the workflow, so there is no fact path to apply the report
		// through. That is a boot that skipped plan.memoryGuardKills.bind, never a daemon that runs
		// no workflow: only a runtime: kubernetes daemon has a holder, and configuration load refuses
		// one whose file names no dispatch_url (errMemoryGuardKillsUnbound). Every report of every
		// pod meets it, so it is returned rather than swallowed: the launcher session logs it with
		// the report it dropped (sandbox's memoryKilled), which is what makes the drop findable.
		return errMemoryGuardKillsUnbound
	}
	fact := intake.MemoryGuardKill{Issue: issue, Token: token, PodUID: podUID, Report: report}
	if _, err := intake.ApplyFact(store.ForConcurrentWorker(ctx), m.pool, "launcher", memoryGuardKillEventID(report), fact, m.handlers...); err != nil {
		return fmt.Errorf("apply the memory guard's kill of pid %d in %s's pod %s: %w", report.PID, issue, podUID, err)
	}
	return nil
}

// memoryGuardKillEventID is the event id a memory guard's kill report is applied under:
// `memory-guard-kill:<guard>:<sequence>`, the guard's own id (minted once per launcher process)
// and its count of kills, which together name one kill of one pod's guard.
func memoryGuardKillEventID(report shimwire.LauncherMemoryKill) string {
	return fmt.Sprintf("memory-guard-kill:%s:%d", report.Guard, report.Sequence)
}

// volumeTrimReports turns one issue pod's volume sweep, as its launcher's cache trimmer reported
// it, into the fact the workflow applies (intake.VolumeNearFull). It is the seam between the
// Kubernetes runtime, which is built before the workflow opens, and the fact path, which exists
// only after it: prepareSandbox makes one and hands it to the factory, and the boot fills it the
// moment the workflow is open (plan.volumeReports, daemon.Run). A holder nobody filled refuses
// every report with errVolumeReportsUnbound, which the launcher session logs with the report it
// drops.
//
// It is filled once, in the boot's own goroutine, before the runtime is built and so before any
// launcher can connect: there is nothing to lock, and nothing ever replaces what was filled.
type volumeTrimReports struct {
	pool     *store.Pool
	handlers []intake.Handler
}

// errVolumeReportsUnbound is what a holder the boot never filled answers every report with. Its
// text names the line that fills it, since a log line is the only place the drop shows: no
// architect is told, and the trimmer goes on sweeping.
var errVolumeReportsUnbound = errors.New("the daemon's volume report holder was never bound to the workflow " +
	"(daemon.go, plan.volumeReports.bind), so this report and every later one is dropped")

// bind gives the holder the daemon's guarded pool (store.Pool) and the handlers a fact applies
// through, which is what the workflow has and the factory does not.
func (v *volumeTrimReports) bind(pool *store.Pool, handlers []intake.Handler) {
	v.pool, v.handlers = pool, handlers
}

// report applies one sweep of issue's pod as one intake fact (runtime.VolumeReported). It runs on
// the launcher's own frame loop under that loop's bounded context, so an apply that cannot be
// made is returned to the launcher session, which logs it and drops the report; the trimmer's
// next sweep that ends above the low mark reports the volume again, and a dropped report of a
// trimmer that switched itself off is not sent again. Every launcher's frame loop runs beside
// the others and beside the daemon's own loops, so each report applies under a hold mark of its
// own (store.ForConcurrentWorker): this is a fact source, and a fact source marks its context
// where it starts (store.ErrNestedAcquire).
//
// The event id is unique per report and never per sweep: nothing in the frame numbers a report,
// and ApplyFact applies one id exactly once — an id built from the report's own contents would
// silently drop the second report of an episode whose numbers had not moved, which is precisely
// the pod that is full and not changing. So the id carries the daemon's own receipt — the issue,
// the pod, the trimmer and the episode — with a random tail, as api.requestFactID gives each of
// the API's own requests, which is unique across processes as well as within one.
func (v *volumeTrimReports) report(ctx context.Context, issue, podUID string, report shimwire.LauncherVolumeReport) error {
	if v.pool == nil || len(v.handlers) == 0 {
		// Nothing bound the holder to the workflow, so there is no fact path to apply the report
		// through. That is a boot that skipped plan.volumeReports.bind, and every report of every
		// pod meets it, so it is returned rather than swallowed: the launcher session logs it with
		// the report it dropped (sandbox's volumeReported), which is what makes the drop findable.
		return errVolumeReportsUnbound
	}
	fact := intake.VolumeNearFull{Issue: issue, PodUID: podUID, Report: record.VolumeNearFull{
		Trimmer: report.Trimmer, Episode: report.Episode, RaisedBy: report.RaisedBy,
		UsedBytes: report.UsedBytes, FreeBytes: report.FreeBytes, FreedBytes: report.FreedBytes,
		StopReason: report.StopReason, RefusedReads: report.RefusedReads, RefusedReadsOmitted: report.RefusedReadsOmitted,
		Sizes: volumeSizes(report.Sizes), SizesOmitted: report.SizesOmitted,
		SizesAt: report.SizesAt, SizesPartial: report.SizesPartial,
		SizesMissing: volumeMissing(report.SizesMissing), SizesMissingOmitted: report.SizesMissingOmitted,
		Faults: report.Faults, FaultsOmitted: report.FaultsOmitted, SwitchedOff: report.SwitchedOff,
	}}
	eventID := fmt.Sprintf("volume-near-full:%s:%s:%s:%d:%s", issue, podUID, report.Trimmer, report.Episode, rand.Text())
	if _, err := intake.ApplyFact(store.ForConcurrentWorker(ctx), v.pool, "launcher", eventID, fact, v.handlers...); err != nil {
		return fmt.Errorf("apply the volume report of %s's pod %s: %w", issue, podUID, err)
	}
	return nil
}

// volumeSizes and volumeMissing are the wire's size walk as the record keeps it. The two shapes
// are the same members by design — the record's is durable and the wire's is not — and these are
// the one place they are copied across.
func volumeSizes(sizes []shimwire.LauncherVolumeSize) []record.VolumeSize {
	if len(sizes) == 0 {
		return nil
	}
	kept := make([]record.VolumeSize, 0, len(sizes))
	for _, size := range sizes {
		kept = append(kept, record.VolumeSize{Path: size.Path, Bytes: size.Bytes,
			BytesOverHour: size.BytesOverHour, BytesOverDay: size.BytesOverDay,
			Refused: size.Refused, Refusals: size.Refusals})
	}
	return kept
}

// volumeMissing is the walk's unmeasured targets, and the volume root or an immediate child of
// it where the walk did not list one in full, each with its reason, as volumeSizes says.
func volumeMissing(missing []shimwire.LauncherVolumeMissing) []record.VolumeMissing {
	if len(missing) == 0 {
		return nil
	}
	kept := make([]record.VolumeMissing, 0, len(missing))
	for _, one := range missing {
		kept = append(kept, record.VolumeMissing{Path: one.Path, Reason: one.Reason})
	}
	return kept
}

// implementTokens is the workflow's implement App as a sandbox's provisioning token source: every
// pod's workspace-fetch clones with an installation token minted for the owner of the repository
// it fetches and narrowed to reading (the read lease's set, appauth's readPermissions, each
// permission at read: appauth.Tokens.ReadToken), nothing a clone, a fetch or a repository cache
// could write with. Every refusal is returned as it came, with no second mint: GitHub's trouble
// (appauth.TransientError, a 5xx or a rate-limited 422), an App with no installation on the
// owner (appauth.ErrNotInstalled), and an installation that is not granted what the reading
// lease asks for (appauth.ErrNarrowingRefused) alike.
//
// That last one once fell back to the owner's own write token, since a request naming a
// repository the installation did not reach was refused with the same 422 as a permission it
// was not granted, and a pod that could not clone was worth a wider token. No request names a
// repository any longer (INFRA-2543), so the owner's write token now reaches every repository of
// the owner's installation: a fallback would answer a refused reading lease with the widest
// credential the daemon holds, handed to the process that provisions a pod. A refused reading
// lease fails the mint instead.
//
// It is the one token source the Sandbox runtime's three mint sites share (runtime/sandbox's
// provision, its gh credential refresh and its repository cache Secret), so the repository a
// configured rename moved to another owner is mapped here, once, to the name whose installation
// holds it now (serving): nothing under runtime/sandbox knows of a rename.
//
// Only the token follows the rename. Every address those three sites build — the pod's feed, its
// clone, and the repository cache's volume and Service — stays the repository the record names,
// which a transfer must not move: GitHub's own documentation of a transfer says "All links to the
// previous repository location are automatically redirected to the new location. When you use
// `git clone`, `git fetch`, or `git push` on a transferred repository, these commands will
// redirect to the new repository location or URL", so a clone or a fetch addressed at the name
// the record keeps reaches the repository GitHub holds today.
type implementTokens struct {
	apps    appauth.Tokens
	serving func(ghrepo.Repository) ghrepo.Repository
}

// newImplementTokens is the adapter over apps, with serving as the map a mint's repository goes
// through.
func newImplementTokens(apps appauth.Tokens, serving func(ghrepo.Repository) ghrepo.Repository) *implementTokens {
	return &implementTokens{apps: apps, serving: serving}
}

func (t *implementTokens) Token(ctx context.Context, repo ghrepo.Repository) (string, error) {
	if t.serving != nil {
		repo = t.serving(repo)
	}
	lease, err := t.apps.ReadToken(ctx, appauth.Implement, repo.Owner())
	if err != nil {
		return "", err
	}
	return lease.Token, nil
}

// leakCheckSource is the daemon's reader of the shared public leak check, minting through apps
// (leakCheckToken) and reading under githubAPI, GitHub's REST root ("" is
// https://api.github.com), which the Sandbox runtime pushes to each pod's launcher before any role
// of the pod starts (sandbox.Options.LeakCheck). Where apps drops an owner's leases
// (appauth.Forgetter), a read GitHub answers 401 drops the lease leakCheckToken mints, so the next
// read mints a new one. A daemon with no GitHub Apps has no reader: the answer is then
// the nil interface, never a nil *leakguard.Source inside one, so the runtime's comparison with
// nil reads the daemon as holding no credential.
func leakCheckSource(apps appauth.Tokens, githubAPI string) sandbox.LeakCheckReader {
	if apps == nil {
		return nil
	}
	source := &leakguard.Source{Token: leakCheckToken(apps), API: githubAPI}
	if forgetter, ok := apps.(appauth.Forgetter); ok {
		source.Forget = func() { forgetter.Forget(appauth.Implement, leakguard.CheckRepository.Owner()) }
	}
	return source
}

// leakCheckToken mints the token the shared public leak check is read with: the implement App's
// read lease on the check's owner (appauth.Tokens.ReadToken, whose read permissions hold all a
// tarball read needs). No request names a repository any longer (INFRA-2543), so the lease reaches
// every repository that owner's installation holds and the reading permissions are the whole of
// what keeps it from writing. It mints nothing else: an installation the account has not granted
// those permissions is refused, with no wider token in its place, since the owner's own write
// token would add only what a tarball read never uses.
//
// The error's text is the cause sentence's own words after leakguard.RefusalPrefix
// (leakguard.Source.Token): GitHub refusing the reading permissions (appauth.NarrowingRefused) is
// told from every other failed mint, since the two are fixed in different places — what the
// account grants the App's installation, and the App's configuration or its installation on the
// owner.
func leakCheckToken(apps appauth.Tokens) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		lease, err := apps.ReadToken(ctx, appauth.Implement, leakguard.CheckRepository.Owner())
		switch {
		case err == nil:
			return lease.Token, nil
		case appauth.NarrowingRefused(err):
			return "", fmt.Errorf("GitHub refused the daemon's implement App the reading permissions on %s, and the daemon reads %s with no wider token: %w", leakguard.CheckRepository.Owner(), leakguard.CheckSlug, err)
		default:
			return "", fmt.Errorf("the daemon could not mint its read-only token for %s: %w", leakguard.CheckSlug, err)
		}
	}
}
