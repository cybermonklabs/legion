package sandbox

import (
	"context"
	"log/slog"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Scheduling is the placement the operator gives Legion's pods, which share the cluster's general
// nodes: a pod selects and tolerates what this names and, in a microVM, its ComputeClass, and
// nothing else (placement).
type Scheduling struct {
	// NodeSelector is every pod's node selector; it may not set legion.dev/pool (poolKey). A
	// microVM pod, an issue's or the project controller's, names its own ComputeClass in place of
	// any this names (placement).
	NodeSelector map[string]string
	// Tolerations are every pod's tolerations, less one of another ComputeClass on a microVM pod,
	// an issue's or the project controller's, which tolerates its own class's taint after them
	// (placement).
	Tolerations   []corev1.Toleration
	PriorityClass string
}

// SandboxType is the sandbox a pod runs in: an issue pod's, its issue's (SandboxConfig.Of), and
// the project controller's, the project's type (SandboxConfig.Controller).
type SandboxType string

const (
	// SandboxGVisor is gVisor: the sandbox of every issue pod the configuration names no other
	// for, of the controller's pod where the project's type is not microvm, and of every other pod
	// the runtime builds, the repository cache pods and the image probe's, whatever it says.
	SandboxGVisor SandboxType = "gvisor"
	// SandboxMicroVM is a GKE Sandbox microVM, on a node of the ComputeClass MicroVM names.
	SandboxMicroVM SandboxType = "microvm"
)

// SandboxConfig is runtime.kubernetes.sandbox, settled, as the daemon hands it over member for
// member (config.SandboxConfig): the sandbox of the project's issue pods and of its controller's
// pod, the issues whose pods run in the other one, and where a microVM pod runs. It places the
// issue pods (Of) and the controller's pod (Controller) alone: the repository cache pods and the
// image probe run in gVisor whatever it says. The zero value runs every pod in gVisor.
type SandboxConfig struct {
	// Type is the sandbox of the project's issue pods and of its controller's pod. Any value but
	// SandboxMicroVM, the zero value's included, reads as gVisor.
	Type SandboxType
	// CanaryIssues are the issue keys whose pods run in the sandbox Type does not name. They name
	// issues, so they never move the controller's pod, which has none.
	CanaryIssues []string
	// MicroVM is where a microVM pod runs, its disk and its metadata endpoint; nil when the
	// configuration names none. New refuses a nil one, or one naming no ComputeClass or no
	// StorageClass, while an issue pod runs in a microVM (AnyMicroVM), which every project whose
	// controller's pod runs in one has: its type is microvm.
	MicroVM *MicroVM
}

// MicroVM is runtime.kubernetes.sandbox.microvm: the GKE ComputeClass whose nodes run the microVM
// pods; the StorageClass of the microVM issue pods' disks, runtime.kubernetes.storage_class where
// the block names none, which the loader fills in (the controller's disk stays on
// runtime.kubernetes.storage_class in either sandbox, diskSpec); whether an issue pod keeps its
// volume; and the metadata endpoint their launchers serve, nil for none.
type MicroVM struct {
	ComputeClass, StorageClass string
	// KeepVolume is microvm.keep_volume. It applies to an issue pod that runs in a microVM alone:
	// that pod's volume is then a claim that the issue's Sandbox owns, and the claim outlives the
	// pod. A gVisor pod and the controller's pod ignore it.
	KeepVolume bool
	Metadata   *MetadataEndpoint
}

// MetadataEndpoint is runtime.kubernetes.sandbox.microvm.metadata, which a microVM pod's launcher
// is told (launcher.MetadataConfig): the cluster's Workload Identity pool,
// `<PROJECT_ID>.svc.id.goog`, which is also the audience of the projected token the launcher
// exchanges for a Google one (workloadIdentityVolumes), and the cluster's identity provider.
type MetadataEndpoint struct {
	WorkloadPool, IdentityProvider string
}

// Of is the sandbox issue's pod runs in: Type, or the other one when CanaryIssues names the
// issue's exact key.
func (c SandboxConfig) Of(issue string) SandboxType {
	microVM := c.Type == SandboxMicroVM
	if slices.Contains(c.CanaryIssues, issue) {
		microVM = !microVM
	}
	if microVM {
		return SandboxMicroVM
	}
	return SandboxGVisor
}

// Controller is the sandbox the project controller's pod runs in: Type, a microVM where it is
// microvm and gVisor otherwise. CanaryIssues never moves it: the list names issues, and the
// controller has none, so the controller of a gVisor project stays in gVisor whatever the list or
// the microvm block says.
func (c SandboxConfig) Controller() SandboxType {
	if c.Type == SandboxMicroVM {
		return SandboxMicroVM
	}
	return SandboxGVisor
}

// AnyMicroVM reports whether a pod may run in a microVM: Type is microvm, under which the
// controller's pod runs in one and so does every issue pod the canary list does not name, or
// CanaryIssues names an issue, whose pod then runs in a microVM on a gVisor project.
func (c SandboxConfig) AnyMicroVM() bool {
	return c.Type == SandboxMicroVM || len(c.CanaryIssues) > 0
}

// Tools are absolute paths inside the worker image. Legion is the `legion` every container runs
// (/opt/legion/bin/legion), whose directory also leads the pod's PATH. AgentSecrets is the
// `agent-secrets` client (/opt/legion/bin/agent-secrets), which the shim runs only for a pod the
// runtime enrolls with the secrets broker (Options.AgentSecrets). GH, Git, and JJ reach no agent: a
// tree agent's gh, git and jj are the ones its PATH resolves in the image, and its credential is
// the gh files under GH_CONFIG_DIR, so the three serve only CheckPod's refusal of an operator's
// mount at, under, or above a path the image owns (operatorpod.go).
type Tools struct{ GH, Git, JJ, Legion, AgentSecrets string }

// Pod is what the operator adds to every pod Legion runs, the image probe's included
// (runtime.kubernetes.pod): variables and volume mounts for the agent's container (the worker's,
// or the probe's), never an init container, the volumes those mounts name, beside Legion's own, and
// the ServiceAccount the pods run as, the namespace's default when unset. ControllerServiceAccount
// is the one the project controller's pod runs as in its place when set (controllerPod's
// serviceAccount); unset, that pod runs as ServiceAccount like every other. A variable's value
// reaches the process as written: the kubelet's `$(NAME)` expansion does not apply
// (kubeletLiteral). CheckPod refuses a name or path of Legion's own or the worker image's. Its
// fields are config.PodConfig's, one for one, since the daemon converts the one into the other.
type Pod struct {
	Env                      map[string]string
	Volumes                  []corev1.Volume
	VolumeMounts             []corev1.VolumeMount
	ServiceAccount           string
	ControllerServiceAccount string
}

// ProvisionTokens mints the installation token a pod's workspace-fetch container clones the
// repository with, for the repository it clones, which the token names and reaches alone.
// The daemon's is appauth; a harness's may be a token file read on every call.
type ProvisionTokens interface {
	Token(ctx context.Context, repo ghrepo.Repository) (string, error)
}

// Store is the durable state the runtime reads: the daemon's store (store.Store), or a test's
// fake.
type Store interface {
	// TreeLive is whether tree's lifecycle is open, or its cleanup reserved and unconfirmed: the
	// tree's issue Sandboxes are then its cleanup's alone, never the orphan sweep's.
	TreeLive(ctx context.Context, project, tree string) (bool, error)
	// IssueRunsElsewhere names a stored claim of issue whose tree is not tree and that may hold a
	// process now, a locator or a state with one (token:state), or "" when none does: a Sandbox
	// labelled for another tree is then this tree's to relabel (ensureSandbox).
	IssueRunsElsewhere(ctx context.Context, project, issue, tree string) (string, error)
	// IssueSandboxHome is the tree issue's Sandbox belongs to now, or "" when none: the issue's own
	// key while a live hand-over of it is pending, else its record's tree while that tree's lifecycle
	// is open or releasing, or, for a tree never opened, while the issue waits for admission or
	// lingers after its own close. The orphan sweep keeps a Sandbox whose home is the tree it is
	// labelled with (sweep); the tree cleanup deletes every Sandbox of the tree it cleans.
	IssueSandboxHome(ctx context.Context, project, issue string) (string, error)
	// ClaimUnretired is whether the store holds token's claim in any state but retired: a claim the
	// daemon's orphan sweep still knows, suspended and failed ones included. The sweep reads it under
	// the controller's pod turn, since the known set it was handed was read before it listed.
	ClaimUnretired(ctx context.Context, token claim.Token) (bool, error)
	// Claims is every stored claim, including retired claims whose session files can
	// remain on a Sandbox-owned volume until its cleanup reaps it.
	Claims(ctx context.Context) ([]supervise.Claim, error)
}

// Options is what a Runtime is built from. Every field without a stated default is required.
type Options struct {
	// Namespace is where every Sandbox, pod, and Secret of the runtime lives; Project is the
	// value of the legion.dev/project label on every one of them, and the informers select on it.
	Namespace, Project string
	// Store is the durable state the runtime reads.
	Store Store
	// Image is the worker image, pinned by digest: New refuses one without "@sha256:".
	Image string
	// StorageClass is the issue volumes' class. Required: production has no default class.
	StorageClass string
	// IssueVolume is each issue volume's size, positive; the daemon's configuration supplies its
	// default (runtime.kubernetes.issue_volume, config.defaultIssueVolume when the file sets none).
	IssueVolume resource.Quantity
	Scheduling  Scheduling
	// Sandbox is the sandbox each issue pod and the controller's pod runs in, and where a microVM
	// pod runs (SandboxConfig). Every pod runs in gVisor under the zero value.
	Sandbox SandboxConfig
	// Resources are the two pods' requirements: every issue pod's one container and its two init
	// containers carry Issue, the controller's pod's one container Controller. The daemon hands
	// both (config.DefaultResources fills what its file leaves out). At
	// the defaults each cpu and memory request equals its limit, so every pod is Guaranteed; a pod
	// the deployment gives a lower request is Burstable. The image probe carries its own
	// (ImageProbe.Resources). New refuses either when it is not a reservation: cpu and memory each a
	// positive request no greater than its positive limit, ephemeral storage bounded the same way,
	// and no other resource named (checkRequirements).
	Resources PodResources
	// StreamURL is the worker stream listener every pod's shim dials, tcp://host:port.
	StreamURL string
	// DaemonURL, EnvoyURL, and DispatchURL are the pane values LEGION_DAEMON_URL, ENVOY_URL, and
	// DISPATCH_URL; an empty one is left unset.
	DaemonURL, EnvoyURL, DispatchURL string
	// Dispatch is the Dispatch credential a launch's role holds, configured exactly when
	// DispatchURL is (DispatchCredential).
	Dispatch DispatchCredential
	// NATSURLs are ENVOY_NATS_URL, comma-joined; none leaves it unset.
	NATSURLs []string
	Tools    Tools
	// AgentSecrets enrolls every pod with the secrets broker; nil enrolls none. See AgentSecrets.
	AgentSecrets *AgentSecrets
	// ControllerSlackChannels is the project's slack_channels, which the controller this runtime
	// launches is told as LEGION_SLACK_CHANNELS, comma-joined and set even when empty, and which a
	// running controller's address record is compared with (environmentAddresses), so a daemon
	// restarted with another list relaunches it, resuming its session.
	ControllerSlackChannels []string
	// Pod is the operator's pod configuration, added to every worker pod and to the probe pod.
	Pod Pod
	// DeploymentApps is each role the deployment defines in its settings (`roles`) with the
	// GitHub App it acts as (the daemon's deploymentApps). Its roles, sorted by name, follow
	// Legion's six in the runtime's issue roles (issueRolesOf), and each takes overlays
	// (RoleConfigFiles) as one of Legion's does. Nil is a deployment that defines no role.
	DeploymentApps appauth.DeploymentApps
	// RoleConfigFiles is the operator's `role_config_files`: absolute paths of Oh My Pi settings
	// overlays a role alone names in PI_CONFIG_FILES, by role, after the operator's own (Pod.Env's
	// PI_CONFIG_FILES), so each outranks those (mainEnvironment, roleOperatorEnv), and the shim
	// hands that value to Oh My Pi untouched (podsafety.Apply). The files are the
	// operator's mounts', which every pod and the image probe's carry, and the image probe reads
	// every role's (probeManifest). Nil is none.
	RoleConfigFiles map[claim.Role][]string
	// ExperimentVariants is every arm of every experiment the project declares
	// (config.Project.ExperimentsDeclared), in the order the file declares the experiments and,
	// within one experiment, the order it declares the arms. A role names the overlays of the
	// arms its own unit was drawn into (runtime.SpawnSpec.Variants) last in PI_CONFIG_FILES,
	// after the operator's own and the role's own, so they outrank both (roleOperatorEnv); this
	// list is every arm any role could be given, which is what the readers that hold a pod or the
	// deployment and no tree take — the pod's own overlay drift (movedRoleOverlays), what a kept
	// pod holds (heldRoleOverlays) and the image probe, which reads one value per role per arm
	// (probeManifest). The files are the operator's mounts', as a role's own are. The daemon
	// hands them over whatever each experiment's mode is, since a unit already drawn into an arm
	// names its files under any mode. Every project declares the memory experiment, so the list
	// always holds its two arms, and a project that writes no `memory` block and no experiment of
	// its own names no file in any arm. Nil is none.
	// A variant's InstructionsPath is the daemon's to compose into the prompt
	// (runtime.ComposePrompt) and is not read here.
	ExperimentVariants []runtime.Variant
	// ProviderKeys maps each variable Oh My Pi reads to the key of the providers Secret
	// (ProvidersSecretName) that holds its value. With any, every pod mounts exactly those keys at
	// ProvidersDir and the worker's shim exports them into Oh My Pi's environment alone; with none,
	// no pod mounts the Secret.
	ProviderKeys map[string]string
	// LaunchSecrets are the secrets every launch's spec carries (runtime.SpawnSpec.Secrets), by
	// name, each reaching the agent as a `<NAME>_FILE` pointer, which CheckPod refuses the operator's
	// pod and a provider key.
	LaunchSecrets []string
	// ProvidersSecrets are the launch secrets, of LaunchSecrets, that the providers Secret carries
	// under their own names. The runtime never copies one into a claim's Secret: every pod mounts
	// the providers Secret's key of that name at ProvidersDir beside the provider keys, and its
	// `<NAME>_FILE` pointer names that file, in the worker's container and the image probe's alike,
	// so the shim never exports it into Oh My Pi's environment.
	ProvidersSecrets []string
	// NATSUser is the public key of the NATS nkey user of the pane seed the daemon hands every pod
	// (nats_nkey_seed_file, never the daemon's own nats_daemon_nkey_seed_file), "" when it has
	// none. The image probe passes only when it read, through its providers secrets' pointers, the
	// seed of this same user (bootprobe.NATSUser), so a providers Secret holding a blank, invalid,
	// or other seed refuses boot instead of every agent's connection.
	NATSUser string
	// SessionStore is where every role's Oh My Pi keeps its session
	// (runtime.kubernetes.session_store), settled. It is required to keep sessions in a database
	// (InDatabase): an issue pod's disk is the pod's own ephemeral volume and is deleted with the
	// pod, so a session kept as a file on it would not survive to be resumed, and configure refuses
	// any other store as it refuses an unpinned image. Every generation is started with
	// OMP_SESSION_STORAGE=sql and OMP_SESSION_SQL_DSN_FILE naming a URL file (mainEnvironment), a
	// resume is held to the session table (the role launcher's check, internal/launcher), and no
	// launch expects any disk to hold a session. Under postgres that file is the providers Secret
	// key the pod and the image probe mount at ProvidersDir/OMP_SESSION_SQL_DSN, which the shim,
	// seeing the pointer, never exports; under cloudsql-iam no pod mounts a session key at all —
	// each pod's launcher container (an issue pod's `roles`, the controller pod's `controller`)
	// is told the instance, database and login once, and the Google service account to
	// impersonate where the store names one (sessionProxyFlags), proxies a socket of each
	// generation's own to Cloud SQL as the pod itself or, impersonating it with the pod's own
	// credentials, as that account, and writes the URL file in that generation's private
	// directory. Where the store names a writer audience too, the launcher is told it, the pod
	// carries the projected token of that audience under RolesDir (sessionWriterVolumes), and the
	// launcher impersonates the account with that token in place of the pod's own credentials; the
	// controller's pod then runs the launcher's roles-dir form, so no role's process reads the
	// token (controllerPod.launcherContainer).
	SessionStore ompsessions.Store
	// Agent is the command the shim wraps, before the Oh My Pi arguments the runtime appends
	// (`--extension <envoy plugin> --extension <legion plugin> --model <the spec's model, or
	// @default>`, `--resume`, `--mode rpc`, `--append-system-prompt <the generation's prompt
	// file>`); Oh My Pi itself when nil.
	Agent []string
	// Setup is the deployment's setup command (runtime.kubernetes.setup), an argv each role runs
	// before each start of its Oh My Pi — after a stale-plugin relaunch's install, in the role's
	// working directory and with the environment Oh My Pi gets — through the start wrapper
	// (startArgv); Oh My Pi starts only once it exits 0. Nil runs none.
	Setup []string
	// TestPostgres is runtime.kubernetes.test_postgres: every issue pod serves its own PostgreSQL
	// for tests, started by the roles container's launcher before it accepts a role start and
	// started again whenever the postmaster exits (launcher.Config.TestPostgres), and every role
	// of such a pod is told TestPGVariable. It is the launcher's and not a role's setup step
	// because a role's generation runs as the init of its own PID namespace, whose end kills
	// every process in it, so a server a role started would die with that generation and take
	// every sibling role's test database with it.
	TestPostgres bool
	// MemoryGuard is what the memory guard in an issue pod's launcher runs by
	// (runtime.kubernetes.memory_guard.issue_pods), handed to it on the daemon's answer to each
	// of its roles' hellos (LauncherResolver). Nil is a deployment that turned the guard off, and
	// the launcher reads the absent member as "run none", as it reads a daemon that does not know
	// the guard. The controller's pod never takes it, however it is set: that pod's settings are
	// ControllerMemoryGuard's.
	//
	// It is no part of the pod shape a template records (podShapeRecord): nothing of a running
	// pod is built from it, since a launcher is told it on every hello, a reconnect after a
	// daemon restart included, so a changed setting reaches every pod without replacing one.
	MemoryGuard *shimwire.LauncherMemoryGuard
	// ControllerMemoryGuard is what the memory guard in the launcher of the project controller's
	// pod runs by (runtime.kubernetes.memory_guard.controller_pod), the launcher that is PID 1 of
	// that pod's one container, handed to it on the daemon's answer to each of its hellos
	// (LauncherResolver). Nil is a deployment that turned that pod's guard off, which the launcher
	// reads as it reads MemoryGuard's absence. No issue pod ever takes it, however it is set: an
	// issue pod's settings are MemoryGuard's.
	//
	// It is no part of the pod shape a template records either, for MemoryGuard's reason: a
	// changed setting reaches the controller's running pod at its launcher's next hello, and
	// replaces no pod.
	ControllerMemoryGuard *shimwire.LauncherMemoryGuard
	// VolumeTrim is what the launcher of an issue pod is told its cache trimmer should run by,
	// handed to it on the daemon's answer to its hello (LauncherResolver). Nil is a deployment
	// that runs no trimmer, which is the absence the launcher reads as "run none". The
	// controller's pod takes none however this is set: its launcher never reaches the block that
	// would run one, and the volume it works on is no issue's.
	//
	// It is no part of the pod shape a template records (podShapeRecord): nothing of a running
	// pod is built from it — a launcher is told it on every hello, a reconnect after a daemon
	// restart included — so a pod whose shape predates it has drifted from nothing.
	VolumeTrim *shimwire.LauncherVolumeTrim
	// BootTimeout bounds each wait of a relaunch, its wait for the API server to answer one of its
	// requests other than overloaded included (resend), and is how long a pod may stay unscheduled
	// before it counts as gone (worker_boot_timeout_seconds).
	BootTimeout time.Duration
	// RegistrationIntervals is worker_boot_registration_deadline_intervals: BootTimeout times it is
	// the registration deadline the daemon retires an unregistered role at, and the bound the
	// runtime sizes the plugin install lock's wait against (installLockWait, plugins.go).
	RegistrationIntervals int
	// BusyBackoff is the first wait before a launch's request the API server answered overloaded is
	// sent again, doubled for each such answer after it up to 32 times (resend, backoff.Wait), which
	// BootTimeout bounds.
	BusyBackoff time.Duration
	// TerminationGrace is the pods' terminationGracePeriodSeconds, and how long Suspend and Release
	// wait for a process to end itself after its shutdown frame (worker_stop_timeout_seconds).
	TerminationGrace time.Duration
	// ProbeInterval is how often every watched claim is evaluated again, beyond the evaluation
	// each change to its Sandbox or pod triggers (probe_interval_seconds).
	ProbeInterval time.Duration
	// AdoptTimeout bounds a working-copy adoption (slow_command_timeout_seconds).
	AdoptTimeout time.Duration
	Tokens       ProvisionTokens
	// GitHubCredential is the daemon's function for a role's gh files
	// (runtime.GitHubCredential): hosts.yml and config.yml rendered from the role's App token. It
	// is called for every launcher role when the pod's role Secrets are written
	// (writeLauncherSecrets), and again for each by the refresher (refreshGitHubCredentials), which
	// rewrites the Secret when the render changed: every workflow role of an issue pod and the
	// controller's, which acts as the review App. Required: configure refuses nil as it refuses a
	// nil Tokens.
	GitHubCredential runtime.GitHubCredential
	// ActsAs answers the role whose App a claim's slot holds while the claim does work other than
	// its stored role's (runtime.ActsAs), which runtime.SlotRole reads at each write of a slot's gh
	// files. Optional: nil gives every slot its claim's stored role.
	ActsAs runtime.ActsAs
	// ControllerCredential is the daemon's function for the controller's own gh bundle
	// (appauth.ControllerCredential). The refresher asks it for the owner the Secret holds
	// (refreshGitHubCredential); a launch asks for none, taking the first owner, which is the
	// default the Secret then records, and a refresh whose render holds no directory for that
	// default is refused rather than written. Required: configure refuses nil as it refuses a nil
	// GitHubCredential.
	ControllerCredential runtime.ControllerCredential
	// GitHubDirs are the directories the gh bundle of a pod created now for repo would hold
	// (appauth.GitHubDirs), sorted: one per GitHub owner the daemon serves, lowercased and with
	// every configured rename alias, and one more under the owner of repo for each other
	// repository that owner serves, less every directory the mints of roles' own Apps found
	// GitHub refuses a lease for, which no bundle of those Apps holds and so no pod of them
	// projects (appauth.LeftOut). A
	// running pod's projection is fixed at the directories of its
	// creation (manifest's volumes), so an owner directory this answers that the pod's volume
	// does not name is a pod whose gh lacks it (movedGhDirs); the `<owner>/<repo>` directories
	// are no address a pod is replaced for, and movedGhDirs compares the owner directories of
	// this set alone. repo is the pod's own, read from its workspace fetch (fetchRepository), and
	// the zero Repository for a pod that fetches none, the controller's; roles are the Apps that
	// pod's own roles run as (podAppRoles), both of them for an issue pod and the review App
	// alone for the project controller's, so what a mint of an App the pod does not run found
	// leaves its answer untouched. It is read on every
	// drift evaluation, so it must be cheap and must mint nothing. Required: configure refuses
	// nil as it refuses a nil GitHubCredential.
	GitHubDirs func(repo ghrepo.Repository, roles []appauth.AppRole) []string
	// Conns is the worker stream listener: Suspend's and Release's shutdown frames, and
	// AdoptWorkingCopy, go through it.
	Conns runtime.Conns
	// RepositoryCache is how the runtime makes each repository's cache (runtime.kubernetes
	// .repository_cache, whose defaults the daemon's configuration supplies): required for
	// supervision, which sweeps and refreshes caches.
	RepositoryCache RepositoryCache
	// CacheRepositories is the cache set: the repositories the project works now, read at every
	// orphan sweep, which are those of the project's live issue records and those a launch
	// worked since the daemon last read the records, a set the daemon holds and answers without
	// I/O (daemon's cacheRepositories). An issue may name any repository both role Apps are
	// installed on, so a repository gets its cache — one volume and one pod — from the moment a
	// record or a launch names it. The sweep makes or repairs each member's cache
	// (EnsureRepositoryCache), and retires the cache of a repository the set no longer holds,
	// which is one no live record and no launch since the last read names, once that cache's
	// retention passes. Required for supervision.
	CacheRepositories func() []ghrepo.Repository
	// KeptPodStart is a role's start in a pod whose Sandbox owns the issue's kept volume
	// (runtime.KeptPodStart): it records the pod with its volume for the issue's update round. At
	// each role start in a pod of such a Sandbox the runtime asks it before the start is sent
	// (launchers.recordKeptPod); when it fails, the runtime refuses the role's start
	// (runtime.KeptPodUnrecorded). A runtime given none records no pod.
	KeptPodStart runtime.KeptPodStart
	// MemoryGuardKilled takes one process an issue pod's memory guard ended, as that pod's
	// launcher reported it on the connection of the role whose process it was
	// (runtime.MemoryGuardKilled): the daemon turns it into the note that tells that role why its
	// process is gone. A runtime given none reads the frame, logs a warning naming where the
	// daemon sets this field, and drops it; it never closes the connection on it, since the
	// launcher would redial a second later and send its waiting reports into the same refusal.
	MemoryGuardKilled runtime.MemoryGuardKilled
	// ControllerMemoryGuardKilled takes one process the memory guard of a gVisor controller pod
	// ended, or one that a microVM controller pod's own kernel stopped (the report's Cause,
	// shimwire.MemoryKillCauseKernel, or shimwire.MemoryKillCauseKernelProtected for a process
	// that runs the controller's session), as that pod's launcher reported it on the controller's
	// connection (runtime.ControllerMemoryGuardKilled): the daemon turns it into the notice that
	// tells the project's controller which of its programs ended and why. A runtime given none
	// drops such a report as MemoryGuardKilled's absence drops an issue pod's: it logs a warning
	// naming where the daemon sets this field, and keeps the connection open.
	ControllerMemoryGuardKilled runtime.ControllerMemoryGuardKilled
	// LeakCheckSetting is the deployment's shared public leak check (leakguard.Setting): the
	// repository it is read from and the sha256 of its checker. Configured, every role is told the
	// pod's install as PUBLIC_LEAK_CHECK and the checker as leakguard.CheckerVar
	// (mainEnvironment), and the check is pushed to each pod's launcher (LeakCheck). The zero
	// Setting is a deployment that runs none: the runtime pushes nothing to a launcher whatever
	// tag its hello names, tells its roles neither variable, and so starts each role without one.
	LeakCheckSetting leakguard.Setting
	// VolumeReported takes one sweep of an issue pod's volume as that pod's launcher reported it
	// (runtime.VolumeReported): the daemon turns it into the fact that tells the issue's
	// architect its volume is filling. A runtime given none reads the frame, logs a warning naming
	// where the daemon sets this field, and drops it; what it must never do is close the
	// connection on it, since the launcher would redial the one connection that carries its
	// reports once a second (ServeLauncher's switch).
	VolumeReported runtime.VolumeReported
	// RoundPodEnds is the daemon's reading of the update round over a pod a launch would keep
	// (runtime.RoundPodEnds): whether the round open over podUID, the live pod of token's issue
	// that the launch replaces for no other reason, has ended by the daemon's rule for a round with
	// no process in its pod (dispatch://INFRA-4109 R7). relaunch asks it under the pod's launch
	// turn, only of a live pod a round covers (roundCovers) and only where the launch would keep
	// that pod. A yes is the round's own replacement of the pod, made by the launch: the Sandbox is
	// suspended under the round's mark (displacing, roundReplacement), and the role starts
	// in a new pod, so it is launched once rather than into a pod the round is about to replace. An
	// error fails the launch as any of its steps does. The daemon builds the runtime before its
	// supervision opens, so it binds this through a holder it fills once the supervision is open
	// (internal/daemon's launchRounds). nil keeps every such pod, as every launch kept it before
	// the hook existed.
	RoundPodEnds runtime.RoundPodEnds
	// LaunchTakes is the daemon's record of a launch's own replacement of an issue pod
	// (runtime.LaunchTakes, R7 of #5285): relaunch hands it what LaunchTook will be
	// handed, before the replacement takes anything, and a record that cannot be written fails
	// the launch, which then takes nothing. nil records nothing.
	LaunchTakes runtime.LaunchTakes
	// LaunchTook is the daemon's word of a launch's own replacement of an issue pod
	// (runtime.LaunchTook, R7 of #5285): relaunch hands it the pod, why the launch
	// replaced it rather than keep it — the pod is not live, a facet fixed for its life moved
	// (movedInPod), its Sandbox has another disk shape (supersededDisk) or a kept volume to reset
	// (volumeReset), its launchers are not bound to it (launcherBound), or its stored template
	// cannot be read (keptBy) — and what the read of its workspace through a launcher of the pod
	// found before the replacement, or why no read could be made (runtime.TakenPod), once the act
	// that takes the disk is done: the suspension for a disk that dies with its pod, the delete of
	// the Sandbox for a volume it owns. A Sandbox that runs no pod of its own is handed where the
	// launch deletes the volume it keeps, with no read made (launchTaken). A round's own
	// replacement (RoundPodEnds) is the daemon's own and is not handed here, nor is a replacement
	// that keeps the issue's kept volume, which takes no disk. The daemon builds the runtime
	// before its supervision opens, so it binds this through the holder RoundPodEnds goes through
	// (internal/daemon's launchRounds). nil tells nobody.
	LaunchTook runtime.LaunchTook
	// LeakCheck reads the shared public leak check at the tag a pod's launcher names in its hello
	// (shimwire.LauncherHello.LeakCheckTag), which the runtime pushes to that launcher before it
	// starts any role of it (launchers.serveLeakCheck): the launcher installs the check once for
	// its pod, and every role's shim reads that install. The daemon fills it from its GitHub App
	// (*leakguard.Source) where LeakCheckSetting is configured; nil under a configured setting is a
	// daemon that runs with no GitHub App, which refuses the start of every role whose launcher
	// names a tag and starts a launcher that names none as it is.
	LeakCheck LeakCheckReader
	// Now stamps observations and ages pods and orphans; time.Now when nil.
	Now func() time.Time
	// Log receives what the runtime decides without being asked; slog.Default() when nil.
	Log *slog.Logger
}

// PodResources are the requirements of the two pods the runtime builds, each a whole pod's: an
// issue pod's, whose one container runs every workflow role, and the project controller's.
type PodResources struct {
	Issue, Controller corev1.ResourceRequirements
}

// DispatchCredential is the Dispatch credential a launch's role holds, in exactly one of two ways.
//
// Bearer is the shared Dispatch bearer (dispatch_token_file): every launch carries it as
// DISPATCH_TOKEN in its start command's Files, which the launcher writes into the generation's
// private directory, and its role reads it through DISPATCH_TOKEN_FILE.
//
// Broker is dispatch_token_broker: a launch into a pod that carries an exchange identity
// (launch.trades) carries no Dispatch token at all, and its role is told DISPATCH_TOKEN_BROKER=1,
// under which its Dispatch client trades that identity at the secrets broker for a token of its
// own session. It needs AgentSecrets.ExchangeAudience, so every pod a launch builds carries the
// identity. Transition, given only with Broker, is the shared token
// dispatch_token_transition_file holds: a launch that keeps a live pod built before the identity
// carries it as a launch carries Bearer, since a role of such a pod has no identity to trade until
// its update round replaces the pod; with no Transition such a launch is refused, nothing of the
// pod touched, and waits for that round (runtime.ErrAwaitingPodRound).
type DispatchCredential struct {
	Bearer     string
	Broker     bool
	Transition string
}

// DispatchTokens is what the runtime counts of the Dispatch tokens its launches hand out
// (Runtime.DispatchTokens): the two counts a deployment under dispatch_token_broker reads before
// it removes dispatch_token_transition_file (dispatch://INFRA-4425 R4).
//
// TransitionLaunches is how many launches since this runtime was built sent a start that carried
// the transition token (DispatchCredential.Transition), and LastTransitionLaunch when the latest
// of them was sent, zero before the first. LiveLaunches is how many claims' live generations
// started with DISPATCH_TOKEN in their start command's Files, whichever daemon started them: the
// launcher deletes those files with the generation, so a token handed to a role lives exactly as
// long as the generation it was handed to.
type DispatchTokens struct {
	TransitionLaunches   int
	LastTransitionLaunch time.Time
	LiveLaunches         int
}

// AgentSecrets is the secrets broker the runtime enrolls every pod with: the URL the
// pod's `agent-secrets` client calls, and the audience and lifetime of the projected token each
// pod carries for it. Nil enrolls none, and no pod carries the token, the key volume, the
// variables, or the shim flags. ExchangeAudience, when set, is the audience of a second projected
// token every enrolled pod carries, the identity the broker's exchange trades for a broker token
// (runtime.kubernetes.agent_secrets.exchange_audience); empty, no pod carries one.
type AgentSecrets struct {
	URL, Audience    string
	TokenExpiry      time.Duration
	ExchangeAudience string
}

// InstallRef names the pieces of Agent Sandbox a cluster must have: the Sandbox CRD, and the
// controller's Deployment. Production's are sandboxes.agents.x-k8s.io and
// agent-sandbox-system/agent-sandbox-controller.
type InstallRef struct{ CRD, ControllerNamespace, ControllerName string }

// sandboxGVR is the Sandbox resource, agents.x-k8s.io/v1beta1 at agent-sandbox v1.0.3.
var sandboxGVR = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

// The operating modes a Sandbox has (sandbox_types.go, SandboxOperatingMode). The CRD defaults an
// absent mode to Running.
const (
	modeRunning   = "Running"
	modeSuspended = "Suspended"
)

// The Sandbox conditions the mapping reads, and the Ready reasons that make it uncertain
// (sandbox_types.go:21-89).
const (
	conditionReady     = "Ready"
	conditionSuspended = "Suspended"
	conditionFinished  = "Finished"

	reasonMultiplePods    = "MultiplePods"
	reasonReconcilerError = "ReconcilerError"
)

// sandbox is the part of an agents.x-k8s.io/v1beta1 Sandbox that Legion writes or reads, with the
// upstream JSON names (api/v1beta1/sandbox_types.go at v1.0.3). Nothing else is declared: the
// schema test walks every emitted field against the vendored CRD, which is the only place a
// misspelled or unknown one would show — the API server prunes it without a word.
type sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              sandboxSpec   `json:"spec"`
	Status            sandboxStatus `json:"status,omitzero"`
}

type sandboxSpec struct {
	PodTemplate          podTemplate           `json:"podTemplate"`
	VolumeClaimTemplates []volumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`
	OperatingMode        string                `json:"operatingMode,omitempty"`
	// Service has the Agent Sandbox controller make a headless Service of the Sandbox's name, whose
	// DNS name resolves to the pod while it is Ready: what a repository cache sets.
	Service *bool `json:"service,omitempty"`
}

// podTemplate is upstream's PodTemplate: labels and annotations, and a core PodSpec.
type podTemplate struct {
	Metadata podMetadata    `json:"metadata"`
	Spec     corev1.PodSpec `json:"spec"`
}

type podMetadata struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type volumeClaimTemplate struct {
	Metadata volumeClaimMetadata              `json:"metadata"`
	Spec     corev1.PersistentVolumeClaimSpec `json:"spec"`
}

type volumeClaimMetadata struct {
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

type sandboxStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// mode is the Sandbox's operating mode, with the CRD's default applied.
func (s *sandbox) mode() string {
	if s.Spec.OperatingMode == "" {
		return modeRunning
	}
	return s.Spec.OperatingMode
}

// condition is the Sandbox's condition of that type, when the controller wrote it for the
// Sandbox's current generation. Every operatingMode patch bumps the generation, so a condition
// left over from the previous pod is never read as this one's (B5).
func (s *sandbox) condition(kind string) *metav1.Condition {
	if c := meta.FindStatusCondition(s.Status.Conditions, kind); c != nil && c.ObservedGeneration == s.Generation {
		return c
	}
	return nil
}
