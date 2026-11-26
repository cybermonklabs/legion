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

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/cipher"
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
// Its transient outcomes (a pod that never finished, a kubelet failure, a probe that exited
// bootprobe.NoVerdictExit because a check had no verdict) can repeat for a reason no wait
// changes, such as a memory limit too small for Oh My Pi or a command that never answers, so
// an unbounded retry would hold a deterministic refusal as a boot that never ends. A probe pod
// the pool has no room for is no such outcome: it is waited on at the policy's longest interval,
// outside the six (bootprobe.Run, Outcome.Waiting), since a full pool is the pool's state and not
// the image's, and the daemon exiting would not make room.
var imageProbeRetry = bootprobe.Image

// sandboxReads is what Agent Sandbox needs that readBoot reads: the cluster's client and the
// runtime's Options.
type sandboxReads struct {
	client *rest.Config
	opts   sandbox.Options
}

// readSandbox is Agent Sandbox's share of readBoot (C1's translation, C3): the cluster's client from
// runtime.kubernetes' kubeconfig, and the Options every value of the configuration becomes, with
// credential, the launch's Dispatch credential readBoot read (sandbox.DispatchCredential), and
// paneNatsUser, the public key of the pane NATS nkey seed's user ("" with none), which the image
// probe holds the providers Secret's seed to. None of the host's own agent machinery is read: no
// Oh My Pi invocation or plugin gate (the image probe proves the image's), no Dispatch token file
// (a launch carries its Dispatch token in its start command's Files), no secretsd provider keys
// (a pod mounts its keys from the providers Secret), and no host gh, git, or jj (a pod runs the
// image's).
func readSandbox(cfg config.Config, project string, credential sandbox.DispatchCredential, paneNatsUser string, lookup func(string) (string, bool), log *slog.Logger) (sandboxReads, error) {
	k := *cfg.Runtime.Kubernetes
	rc, err := kubeClient(k)
	if err != nil {
		return sandboxReads{}, err
	}
	opts, err := sandboxOptions(cfg, k, project, credential, lookup, log)
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
	return &runtime.PluginInstall{PiLegion: k.Plugins.PiLegion, PiEnvoy: k.Plugins.PiEnvoy, Contract: api.DaemonAPIVersion()}, source
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
	// The holder the runtime's launches read the update round over a pod through, and tell what a
	// launch's own replacement of an issue pod took through, made here and filled once the
	// supervision is open: openSupervision builds the runtime before the supervision exists
	// (launchRounds, daemon.run). The runtime takes it in its Options, so the factory carries it as
	// it carries every other option; the plan holds it by pointer, as it holds the two holders
	// above.
	p.launchRounds = &launchRounds{}
	opts := reads.opts
	opts.RoundPodEnds = p.launchRounds.ends
	opts.LaunchTakes = p.launchRounds.takes
	opts.LaunchTook = p.launchRounds.took
	p.newRuntime = sandboxRuntime(reads.client, opts, p.repositories.servesRepository, p.repositories.serving, cfg.RepositoryRenames, p.installed, p.left, o.githubAPI,
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
// setting), checks the image's capability rows and the roles' overlays, runs no lane, and exits.
// Its 500m CPU was set while the command also ran a TLS check of the image's clients, the longest
// part of its run, which it no longer makes, and every timing measured for the value came from a
// probe that ran that check: five runs at 250m / 1Gi, before that check held gcloud, bq, az, mise,
// wget, git, go, kubectl, redis-cli and socat, took 57.68–77.43 s, so 1Gi held the probe; five
// runs of `legion probe-image --skip-agent-models` at 1 CPU under gVisor (2026-11-18) took
// 51.9–76.2 s, 19.40–35.99 s of it on CPU; and runs in the worker image of 1c7c825367 in a
// CPU-quota scope on a development host took 121.9–147.3 s at 500m (eleven runs) and
// 73.5–104.4 s at 1 CPU (sixteen runs), with a peak of 374 MiB, against the 240 s command budget
// of the defaults. No run without the TLS check has been measured, so the value is kept until the
// command's other checks are audited, and a measurement of the probe as it then runs sizes it.
// The reservation still fits a pool that cannot place a controller or issue pod. It has a 5Gi
// bound on node disk and a 1Gi disk request. The probe runs at every boot, before anything is
// served, and a probe that reserved a pod's share — the controller's 1 CPU and 4Gi, or an issue
// pod's 2 CPU and 8Gi, at the defaults — could leave the daemon unable to boot on a pool full
// enough to place nothing that large (stage 4b's daemon-controller-liveness restart, whose
// controller reservation no node can hold, met exactly that).
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
		Contract: api.DaemonAPIVersion(), Budget: budget, Retry: imageProbeRetry,
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
// credential is the launch's Dispatch credential (readBoot): the bearer dispatch_token_file holds,
// or under dispatch_token_broker the role's own trade (sandbox.DispatchCredential.Broker) and the
// transition token.
func sandboxOptions(cfg config.Config, k config.Kubernetes, project string, credential sandbox.DispatchCredential, lookup func(string) (string, bool), log *slog.Logger) (sandbox.Options, error) {
	issueVolume, err := resource.ParseQuantity(k.IssueVolume)
	if err != nil {
		return sandbox.Options{}, fmt.Errorf("runtime.kubernetes.issue_volume: %w", err)
	}
	// The test database's own disk, parsed once here as the issue volume is; nil while the file
	// sets none, which keeps the cluster in the pod's emptyDir.
	var testPostgresDisk *resource.Quantity
	if k.TestPostgresDisk != "" {
		size, err := resource.ParseQuantity(k.TestPostgresDisk)
		if err != nil {
			return sandbox.Options{}, fmt.Errorf("runtime.kubernetes.test_postgres_disk: %w", err)
		}
		testPostgresDisk = &size
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
		DaemonURL:  cfg.DaemonURL, EnvoyURL: cfg.EnvoyURL, DispatchURL: cfg.DispatchURL,
		Dispatch: credential,
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
		TestPostgresDisk:        testPostgresDisk,
		// The guard's settings reach an accepted launcher on the answer to its hello (sandbox
		// Runtime.LauncherResolver), each pod kind's from its own block: an issue pod's launchers
		// runtime.kubernetes.memory_guard.issue_pods, the controller pod's launcher
		// runtime.kubernetes.memory_guard.controller_pod. Nil is what the launcher reads as "run no
		// guard", so a deployment that turned a kind off and a daemon built before the guard write
		// the same ack (shimwire.LauncherHelloAck.MemoryGuard, INFRA-3557).
		MemoryGuard:           launcherMemoryGuard(k.MemoryGuard.IssuePods),
		ControllerMemoryGuard: launcherMemoryGuard(k.MemoryGuard.ControllerPod),
		LeakCheckSetting:      cfg.LeakCheck,
		BootTimeout:           cfg.WorkerBootTimeout,
		RegistrationIntervals: cfg.WorkerBootRegistrationDeadlineIntervals,
		BusyBackoff:           busyBackoffFirst,
		TerminationGrace:      cfg.WorkerStopTimeout,
		ProbeInterval:         cfg.ProbeInterval,
		AdoptTimeout:          cfg.SlowCommandTimeout,
		AgentSecrets:          agentSecrets,
		RepositoryCache:       cache,
		VolumeTrim:            volumeTrim,
		Log:                   log,
	}
	opts.ExperimentVariants = experiment.DeclaredVariants(cfg.ExperimentsDeclared())
	return opts, nil
}

// launcherMemoryGuard is one pod kind's settled memory guard as its launchers are handed it on the
// answer to each hello, nil where the kind's block turned the guard off.
func launcherMemoryGuard(g config.MemoryGuardPodConfig) *shimwire.LauncherMemoryGuard {
	if !g.Enabled {
		return nil
	}
	return &shimwire.LauncherMemoryGuard{
		ActPercent: g.ActPercent, FloorPercent: g.FloorPercent,
		WaitSeconds: g.WaitSeconds, IntervalMilliseconds: g.IntervalMilliseconds,
	}
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
// quantity, the intervals as durations, the reservation as a container's requirements, and the
// bucket with the ServiceAccount the cache pods alone run as, as the file wrote them, both "" while
// off.
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
		Resources: resources, Bucket: c.Bucket, ServiceAccount: c.ServiceAccount,
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
// logs its own outcome (cipher.Client.Login); one that fails is retried by the client, and
// pod enrollment waits for it. With the block's exchange_audience set, the same client is the
// machines' Registrar too (newRegistrar), which registers each role's session for its pod; unset,
// the Registrar is nil and no registration is ever made. The client itself is returned too,
// read-only, so the state route can show the login's current status (source.State,
// cipher.Client.LoginStatus). Never part of launchSecrets, so no pod is ever handed the
// daemon's key, token or credential. Nil, nil, nil without the block.
func newSecretsLogin(cfg config.Config, log *slog.Logger) (supervise.Enroller, supervise.Registrar, *cipher.Client) {
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
	client := &cipher.Client{URL: k.AgentSecrets.URL, HTTP: broker, TokenFile: k.AgentSecrets.TokenFile, Log: log}
	go func() { _ = client.Start(context.Background()) }()
	return brokerEnroller{client: client}, newRegistrar(*k.AgentSecrets, client), client
}

// brokerEnroller is cipher.Client as supervise.Enroller; an *cipher.APIError is a
// PermanentError when its status says so, which is how the machine tells a refusal from an outage,
// and an ExpiredTokenError for POD_TOKEN_EXPIRED, a stale token under a key that stands.
type brokerEnroller struct{ client *cipher.Client }

func (b brokerEnroller) Enroll(ctx context.Context, e supervise.PodEnrollment) (string, error) {
	enrolled, err := b.client.Enroll(ctx, cipher.PodEnrollment{
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
// from while served answers the name it moved to is served), with the
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
// (repositories.serving, the repository state's own, so a configured rename's mint follows the
// owner whose installation holds the repository now), with the repositories the project works as
// the ones its repository caches follow (cached: those of its live issue records and of the
// launches since their last read, workflowRuntime.cacheRepositories), and registers its launcher
// acceptor on the stream listener. A daemon with no GitHub Apps hands it no
// tokens, and sandbox.New's own refusal names what is missing. Boot has already run the cluster
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
func sandboxRuntime(rc *rest.Config, opts sandbox.Options, served func(ghrepo.Repository) bool,
	serving func(ghrepo.Repository) ghrepo.Repository, renames ghrepo.Renames, installed *appauth.Installed, left *appauth.LeftOut, githubAPI string, kills *memoryGuardKills,
	reports *volumeTrimReports) runtimeFactory {
	return func(ctx context.Context, listener *stream.Listener, address string, apps appauth.Source,
		_ func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, cached func() []ghrepo.Repository) (runtime.Runtime, error) {
		opts.Conns, opts.StreamURL, opts.Store, opts.CacheRepositories = listener, address, st, cached
		// Each role's start in a pod whose Sandbox owns the issue's kept volume records that pod
		// with its volume, which the issue's update round reads, and no role starts there until
		// it is recorded.
		opts.KeptPodStart = keptPodStart(st, opts.Project)
		// The cache trimmer's own half: every report a pod's trimmer sends — of a sweep that
		// left the volume above its low mark, or of the trimmer switching itself off — reaches
		// the daemon through this, and the daemon turns it into the one fact that can tell the
		// issue's architect (volumeTrimReports.report). The workflow is what applies that fact,
		// and it opens after this factory is built, so the pair the apply needs is held in
		// reports and filled when it does (plan.volumeReports).
		opts.VolumeReported = reports.report
		// The shared public leak check each pod's launcher is pushed before any role of the pod
		// starts, read from the repository the deployment names (opts.LeakCheckSetting) with the
		// implement App's read lease on its owner (leakCheckToken). A deployment that names none
		// is pushed none; a daemon with no Apps reads none, and the runtime refuses the start of a
		// role whose launcher names a tag under a named check with the cause that says so.
		opts.LeakCheck = leakCheckSource(apps, githubAPI, opts.LeakCheckSetting)
		// Each process an issue pod's memory guard ends, and each the controller pod's ends,
		// reaches the daemon through the kill holder (memoryGuardKills).
		opts.MemoryGuardKilled = kills.killed
		opts.ControllerMemoryGuardKilled = kills.controllerKilled
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
			opts.GitHubCredential = appauth.GitHubCredential(apps, served, renames, installed, left, opts.Log)
			opts.ControllerCredential = appauth.ControllerCredential(apps, served, renames, installed, left, opts.Log)
			opts.GitHubDirs = func(repo ghrepo.Repository, roles []appauth.AppRole) []string {
				return appauth.GitHubDirs(served, renames, installed, left, roles, repo)
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

// keptPodStart is st's record of a role's start in a pod of project whose Sandbox owns the
// issue's kept volume, in the shape the runtime calls it (runtime.KeptPodStart). project is the
// runtime's project token (sandbox.Options.Project). A pod is project's to record only where
// project records its issue (store.Store.StartKeptPod): on a database shared with another
// project's daemon, a pod of that project's issue is that daemon's. Each call for an issue that
// project records records the pod with its kept volume, which the issue's update round reads
// (store.Store.PodHoldsEarlierWork).
func keptPodStart(st *store.Store, project string) runtime.KeptPodStart {
	return func(ctx context.Context, issue, podUID string, keptSince time.Time) error {
		return st.StartKeptPod(ctx, project, issue, podUID, keptSince)
	}
}

// memoryGuardKills turns one process a pod's memory guard ended, as the pod's launcher reported
// it, into the fact the workflow applies: intake.MemoryGuardKill for an issue pod's (killed) and
// intake.ControllerMemoryGuardKill for the controller pod's (controllerKilled). It is the seam
// between the Kubernetes runtime factory (sandboxRuntime), which prepareSandbox builds before the
// workflow opens, and the fact path, which exists only after it: prepareSandbox makes one and
// hands it to the factory, and the boot fills it the moment the workflow is open
// (plan.memoryGuardKills, daemon.run). A holder nobody filled refuses every report with
// errMemoryGuardKillsUnbound, which the launcher session logs with the report it drops.
//
// It is filled once, in the boot's own goroutine, before the runtime is built and so before any
// launcher can connect: there is nothing to lock, and nothing ever replaces what was filled.
type memoryGuardKills struct {
	pool     *store.Pool
	handlers []intake.Handler
}

// errMemoryGuardKillsUnbound is what a holder the boot never filled answers every report with. Its
// text names the line that fills it, since a log line is then the only place the drop shows: the
// role or the controller whose process was ended is told nothing. An unbound holder can only be a
// boot bug, never a daemon that runs no workflow: prepareSandbox makes the holder only under
// runtime: kubernetes, and configuration load refuses a runtime: kubernetes file that names no
// dispatch_url (config's checkKubernetesKeys, "the workflow is what launches every pod"), so every
// daemon with a holder opens the workflow whose boot fills it.
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
	return m.apply(ctx, report, intake.MemoryGuardKill{Issue: issue, Token: token, PodUID: podUID, Report: report}, issue, podUID)
}

// controllerKilled applies one kill of the controller's pod as one intake fact
// (runtime.ControllerMemoryGuardKilled), crediting it to token, the claim whose connection carried
// the report, and to podUID, the pod that launcher runs in, both the session's and never the
// report's. It runs where killed runs, under the same bounded context and hold mark, and answers
// a holder nobody bound with the same errMemoryGuardKillsUnbound, for the same reason.
//
// The event id is the one killed applies under (memoryGuardKillEventID): the guard's id is minted
// by each launcher process, so a kill of the controller's pod and one of an issue pod never share
// it, and a report the controller's launcher sent again on its next connection is applied once, so
// the controller is told of one kill once.
func (m *memoryGuardKills) controllerKilled(ctx context.Context, token claim.Token, podUID string, report shimwire.LauncherMemoryKill) error {
	return m.apply(ctx, report, intake.ControllerMemoryGuardKill{Token: token, PodUID: podUID, Report: report}, "the controller", podUID)
}

// apply applies fact, the intake fact of report, under the kill's own event id
// (memoryGuardKillEventID) and a hold mark of its own (store.ForConcurrentWorker), which is what
// killed and controllerKilled share. An apply that fails names the kill's pod in its error: owner
// is whose pod podUID is, an issue's key or `the controller`.
func (m *memoryGuardKills) apply(ctx context.Context, report shimwire.LauncherMemoryKill, fact intake.Fact, owner, podUID string) error {
	if m.pool == nil || len(m.handlers) == 0 {
		// Nothing bound the holder to the workflow, so there is no fact path to apply the report
		// through. That is a boot that skipped plan.memoryGuardKills.bind, never a daemon that runs
		// no workflow: only a runtime: kubernetes daemon has a holder, and configuration load refuses
		// one whose file names no dispatch_url (errMemoryGuardKillsUnbound). Every report of every
		// pod meets it, so it is returned rather than swallowed: the launcher session logs it with
		// the report it dropped (sandbox's memoryKilled), which is what makes the drop findable.
		return errMemoryGuardKillsUnbound
	}
	if _, err := intake.ApplyFact(store.ForConcurrentWorker(ctx), m.pool, "launcher", memoryGuardKillEventID(report), fact, m.handlers...); err != nil {
		return fmt.Errorf("apply the memory guard's kill of pid %d in %s's pod %s: %w", report.PID, owner, podUID, err)
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
// configured rename moved to another owner is turned here, once, into the name whose installation
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

// leakCheckSource is the daemon's reader of the shared public leak check the deployment names
// (setting, config's leak_check), minting through apps (leakCheckToken) and reading the setting's
// repository under githubAPI, GitHub's REST root ("" is https://api.github.com), which the Sandbox
// runtime pushes to each pod's launcher before any role of the pod starts
// (sandbox.Options.LeakCheck). Where apps drops an owner's leases (appauth.Forgetter), a read
// GitHub answers 401 drops the lease leakCheckToken mints, so the next read mints a new one. A
// deployment that names no check, and a daemon with no GitHub Apps, have no reader: the answer is
// then the nil interface, never a nil *leakguard.Source inside one. The runtime reads the first as
// a deployment that runs no check (sandbox.Options.LeakCheckSetting) and pushes nothing, and the
// second, under a configured setting, as the daemon holding no credential to read it with.
func leakCheckSource(apps appauth.Tokens, githubAPI string, setting leakguard.Setting) sandbox.LeakCheckReader {
	if apps == nil || !setting.Configured() {
		return nil
	}
	repository := setting.Repository
	source := &leakguard.Source{Repository: repository, Token: leakCheckToken(apps, repository), API: githubAPI}
	if forgetter, ok := apps.(appauth.Forgetter); ok {
		source.Forget = func() { forgetter.Forget(appauth.Implement, repository.Owner()) }
	}
	return source
}

// leakCheckToken mints the token the shared public leak check is read with: the implement App's
// read lease on repository's owner (appauth.Tokens.ReadToken, whose read permissions hold all a
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
func leakCheckToken(apps appauth.Tokens, repository ghrepo.Repository) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		lease, err := apps.ReadToken(ctx, appauth.Implement, repository.Owner())
		switch {
		case err == nil:
			return lease.Token, nil
		case appauth.NarrowingRefused(err):
			return "", fmt.Errorf("GitHub refused the daemon's implement App the reading permissions on %s, and the daemon reads %s with no wider token: %w", repository.Owner(), repository, err)
		default:
			return "", fmt.Errorf("the daemon could not mint its read-only token for %s: %w", repository, err)
		}
	}
}
