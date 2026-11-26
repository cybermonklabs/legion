package sandbox

import (
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The pod's init containers: workspace-fetch, the one container of the pod that holds the
// provisioning token, which mounts nothing an agent of the issue can write; and workspace-init,
// which works on the issue's volume without the token. The pod's one container is its kind's
// (podKind.container).
const (
	fetchContainer = "workspace-fetch"
	initContainer  = "workspace-init"
)

// The pod's volumes. ghVolume is the prefix of each launcher role's gh volume (roleVolume), the
// projection of its role Secret's gh files, which only that role's namespace sees at GHConfigDir.
// roleHomesVolume is the disk emptyDir of every role's home overlay, temporary directories and own
// config home of a pod whose launcher runs the roles-dir form: an issue pod, and the controller's
// pod under a writer audience (roleHomesVolumes). roleProbeVolume is the image probe's disk
// emptyDir its role-namespace check runs a role generation over (probeManifest).
const (
	provisionVolume = "provision"
	stateVolume     = "state"
	tempVolume      = "tmp"
	configVolume    = "config"
	providersVolume = "providers"
	ghVolume        = "gh"
	roleHomesVolume = "role-homes"
	roleProbeVolume = "role-probe"
	// testPGVolume is the disk emptyDir an issue pod keeps its own test PostgreSQL's cluster on
	// (TestPGDir), when the deployment sets runtime.kubernetes.test_postgres.
	testPGVolume = "testpg"
	// workloadIdentityTokenVolume is the projected ServiceAccount token a microVM pod's launcher,
	// an issue pod's or the controller's, exchanges for a Google one to serve the pod's metadata
	// endpoint (WorkloadIdentityDir, ControllerWorkloadIdentityDir, workloadIdentityVolumes), when
	// the deployment names that endpoint (runtime.kubernetes.sandbox.microvm.metadata).
	workloadIdentityTokenVolume = "workload-identity-token"
	// sessionWriterTokenVolume is the projected ServiceAccount token a pod's launcher gives its
	// session proxy, which exchanges it to impersonate the session database's service account
	// (SessionWriterDir, sessionWriterVolumes), when the session database names a writer audience
	// (runtime.kubernetes.session_database.writer_audience).
	sessionWriterTokenVolume = "session-writer-token"
)

// The agent-secrets volumes: agentSecretsTokenVolume alone carries the projected token for the
// broker's audience, and agentSecretsKeyVolume the memory-backed directory the client keeps its
// key and enrollment id in. Neither exists when the runtime enrolls no pod.
// agentSecretsExchangeVolume carries the projected token for the exchange's audience, the
// identity a pod's sessions trade for a broker token; it exists only where the pod is enrolled and
// the deployment sets that audience (AgentSecrets.ExchangeAudience).
const (
	agentSecretsKeyVolume      = "agent-secrets-key"
	agentSecretsTokenVolume    = "agent-secrets-token"
	agentSecretsExchangeVolume = "agent-secrets-exchange"
)

// The Legion pool every pod runs on: its taint, tolerated, and its label, selected
// (the cluster's `legion` NodePool). The gVisor RuntimeClass's own selector also
// matches another pool, so the pool label is what keeps a pod on Legion's nodes.
const (
	poolKey   = "legion.dev/pool"
	poolValue = "legion"
	gvisor    = "gvisor"
)

// A microVM pod's placement (placement): GKE Sandbox's microVM RuntimeClass, which adds the
// node affinity and the toleration of the sandbox's own nodes (sandbox.gke.io/runtime) to the pod
// at admission, on the nodes of the ComputeClass the pod names by computeClassKey. That key is the
// node label of a custom ComputeClass and the key of the taint GKE puts on the class's nodes,
// `cloud.google.com/compute-class=<class>:NoSchedule`, which the pod tolerates itself: GKE's pages
// say the RuntimeClass adds the sandbox's own toleration, and not that anything adds the class's.
const (
	microVMRuntimeClass = "microvm"
	computeClassKey     = "cloud.google.com/compute-class"
)

// doNotDisruptAnnotation and safeToEvictAnnotation are the autoscaler annotation keys:
// Karpenter's own (AWS), and the Cluster Autoscaler's, which GKE Autopilot honours as a request
// for extended-duration protection. podTemplate sets both against eviction, unconditionally;
// probeManifest sets only the first; and cachePodTemplate sets the first and the second as "true",
// so the cluster autoscaler may move a cache pod and remove the node it held.
const (
	doNotDisruptAnnotation = "karpenter.sh/do-not-disrupt"
	safeToEvictAnnotation  = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)

// The pod runs as the image's legion user.
const podUser = 1000

// runtimeOwned are the main container's variables the runtime sets itself, the whole of each
// value: with the composed names (composed), whose value the operator begins and the runtime
// finishes, and, where the deployment names an exchange audience, the exchange identity's two
// (exchangeOwned), it is exactly what the container is told by the runtime under Dispatch's shared
// bearer (TestRuntimeOwnedIsWhatTheWorkerContainerIsToldByTheRuntime). Under
// dispatch_token_broker a launch into a pod that carries an exchange identity is told
// DISPATCH_TOKEN_BROKER (brokerOwned) in place of DISPATCH_TOKEN_FILE, which stays owned, since a
// launch into a kept pod without that identity carries the transition token behind it. The shared
// validator refuses a name of any of them in a spec's Env and as a secret's pointer
// (Runtime.specOwned), and CheckPod refuses a runtime-owned one, or an exchange or broker one where
// the deployment names the audience or sets the broker, in the operator's pod — a composed one is
// the operator's to set, since composing is what the runtime does with it. The image probe's
// container sets none of its own. The two CLAUDE_* names and claudelaunch.SessionVariable are told
// a Claude role alone, as the controller's Slack channels are the controller's, and owned in every
// pod: an operator that could place a Claude Code worker's configuration directory, let the clock
// prompt it or name another session for its launcher to keep would be undoing the launch
// (claudelaunch.Environment). CLAUDE_CODE_DISABLE_BACKGROUND_TASKS, CLAUDE_CODE_USE_FOUNDRY and
// the rest of Claude Code's own variables are not here, and stay the operator's to set.
// runtime.SessionExclusiveVariable is told a launch that resumes a recorded Oh My Pi session alone,
// and owned in every pod: an operator that could set it could turn off what holds a resumed role
// to the session the daemon recorded.
var runtimeOwned = func() map[string]bool {
	owned := map[string]bool{
		"LEGION_TREE": true, "LEGION_ISSUE": true, "LEGION_ISSUE_TRAILER": true, "LEGION_ROLE": true, "LEGION_CONTROLLER": true,
		"LEGION_GENERATION": true, "LEGION_PROJECT": true, "LEGION_DAEMON_URL": true,
		"LEGION_STATE_DIR": true, "LEGION_WORKSPACE": true, "ENVOY_NATS_URL": true, "ENVOY_URL": true,
		"DISPATCH_URL": true, "GH_CONFIG_DIR": true, "GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_HOST": true,
		"LEGION_GH_OWNERS_DIR": true, "LEGION_GH_PATH": true,
		"PATH": true, "PI_SHELL_PREFIX": true, "GIT_TERMINAL_PROMPT": true, leakguard.CheckVar: true, leakguard.CheckerVar: true,
		"XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true,
		"TMPDIR": true, "GOCACHE": true, "GOMODCACHE": true, "BUN_INSTALL_CACHE_DIR": true,
		"POD_UID": true, bootTokenKey + "_FILE": true, dispatchTokenKey + "_FILE": true,
		"AGENT_SECRETS_URL": true, "AGENT_SECRETS_KEY_DIR": true, slackChannelsVariable: true,
		"UV_PYTHON_INSTALL_DIR": true, "UV_CACHE_DIR": true, "UV_LINK_MODE": true,
		"CLAUDE_CONFIG_DIR": true, "CLAUDE_CODE_DISABLE_CRON": true,
		claudelaunch.SessionVariable: true, ompsessions.StorageVariable: true, ompsessions.DSNFileVariable: true,
		// The pod's own test database, which an issue pod's roles are told where the deployment
		// serves one (runtime.kubernetes.test_postgres): owned in every pod, since a spec or an
		// operator's pod env naming another server would point every test of the pod at it.
		launcher.TestPGVariable: true,
		// What holds a resumed Oh My Pi to the session it resumes (mainEnvironment).
		runtime.SessionExclusiveVariable: true,
	}
	// GIT_CONFIG_COUNT and each GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n> GitConfigEnv sets.
	for _, entry := range GitConfigEnv() {
		owned[entry.Name] = true
	}
	return owned
}()

// composed are the main container's variables neither side sets alone: the operator's pod env may
// set one, the runtime extends the value the operator wrote with what this launch adds
// (roleOperatorEnv), and a launch spec may not set one. A spec's own pair would be a second pair
// of that name in the process's environment, where the C library's lookup answers with the first,
// so the value the runtime composed could go unread — and for the spend attributes the runtime's
// pairs are written last on purpose (runtime.SpendAttributesVariable). The composing is the shape
// settingsOverlays has had here all along; what this set adds is the refusal, which the overlays
// are not held to: no spec has ever been refused for naming PI_CONFIG_FILES, and taking that away
// is a change of its own.
var composed = map[string]bool{runtime.SpendAttributesVariable: true}

// runtimeOwnedOrComposed is every variable a launch spec may not set where the deployment names no
// exchange audience (Runtime.specOwned): the runtime's own and the composed ones together, since
// the shared refusal reads one set (runtime.ValidateSpawnSpec). Neither set changes after init, so
// the union is built once here rather than per launch.
var runtimeOwnedOrComposed = func() map[string]bool {
	names := maps.Clone(runtimeOwned)
	maps.Copy(names, composed)
	return names
}()

// exchangeOwned are the exchange identity's two variables, which the runtime sets itself only
// where the deployment names an exchange audience (runtime.kubernetes.agent_secrets.exchange_audience,
// launch.trades). A deployment that names none tells no role either, so each stays the operator's
// pod env's and a launch spec's to set, as it was before the key existed: they are no part of
// runtimeOwned, and the reservations that take them (Runtime.specOwned, CheckPod) read whether the
// deployment names the audience.
var exchangeOwned = map[string]bool{machineIdentityVariable: true, machineTokenFileVariable: true}

// runtimeOwnedComposedOrExchange is every variable a launch spec may not set where the deployment
// names an exchange audience: runtimeOwnedOrComposed and exchangeOwned together, built once.
var runtimeOwnedComposedOrExchange = func() map[string]bool {
	names := maps.Clone(runtimeOwnedOrComposed)
	maps.Copy(names, exchangeOwned)
	return names
}()

// brokerOwned is the one variable the runtime sets itself only where the deployment sets
// dispatch_token_broker (Options.Dispatch.Broker), as exchangeOwned is only where it names an
// exchange audience: a deployment without the setting tells no role DISPATCH_TOKEN_BROKER, so the
// name stays the operator's pod env's and a launch spec's to set, as it was before the setting
// existed. The runtime tells it, as 1, to every role of a pod that carries an exchange identity
// under the setting (launch.brokersDispatch), and so never beside DISPATCH_TOKEN_FILE. It is no
// address: a role started under another word holds its credential for its life and is never
// relaunched for the word (handedAddresses leaves it out).
var brokerOwned = map[string]bool{shimwire.DispatchTokenBrokerVariable: true}

// runtimeOwnedComposedExchangeOrBroker is every variable a launch spec may not set under
// dispatch_token_broker, which configure takes only beside an exchange audience:
// runtimeOwnedComposedOrExchange and brokerOwned together, built once.
var runtimeOwnedComposedExchangeOrBroker = func() map[string]bool {
	names := maps.Clone(runtimeOwnedComposedOrExchange)
	maps.Copy(names, brokerOwned)
	return names
}()

// specOwned is every variable a launch spec may not set under r (runtime.ValidateSpawnSpec): the
// runtime's own and the composed ones, the exchange identity's two where the deployment names
// an exchange audience (exchangeOwned), and DISPATCH_TOKEN_BROKER where it sets
// dispatch_token_broker as well (brokerOwned), as PodExchange reads the two.
func (r *Runtime) specOwned() map[string]bool {
	switch exchangeOf(r.agentSecrets, r.dispatch.Broker) {
	case ExchangeAndBroker:
		return runtimeOwnedComposedExchangeOrBroker
	case ExchangeIdentity:
		return runtimeOwnedComposedOrExchange
	}
	return runtimeOwnedOrComposed
}

// slackChannelsVariable is the project's slack_channels as the runtime tells the controller it
// launches (Options.ControllerSlackChannels): comma-joined, set even when empty, so no other value
// stands in for the project's. The controller's plugin subscribes to each channel's app mentions.
const slackChannelsVariable = "LEGION_SLACK_CHANNELS"

// machineIdentityVariable and machineTokenFileVariable are the two variables a pod's broker
// clients read to decide which identity the machine proves and where its token is
// (packages/cipher-client/src/environment.ts's IDENTITY_VAR and MACHINE_TOKEN_FILE_VAR, and
// packages/envoy/internal/cipherclient's IdentityVar and MachineTokenFileVar), and
// kubernetesIdentity the value naming a projected Kubernetes ServiceAccount token. A role is told
// them where its pod trades (launch.trades), and they are Legion's only where the deployment names
// an exchange audience (exchangeOwned).
const (
	machineIdentityVariable  = "AGENT_SECRETS_MACHINE_IDENTITY"
	machineTokenFileVariable = "AGENT_SECRETS_MACHINE_TOKEN_FILE"
	kubernetesIdentity       = "kubernetes"
)

// testPGStartDSN is the pod's test database as the daemon's start command names it
// (launcher.TestPGVariable in mainEnvironment). It is deliberately not the DSN constant of the
// launcher package, which this daemon's own build carries: the deployment pins the daemon and the
// worker image apart, and the launcher that serves the pod's server comes from the image,
// so the daemon cannot know which database that server holds. This is the one database every
// image's server serves, `postgres`, which a launcher of any age leaves standing; a launcher that
// creates a database of its own replaces this value in every generation it starts with the DSN it
// serves (the launcher package's testPGEnv).
const testPGStartDSN = "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable"

// sessionStoreVariables are Oh My Pi's two variables that place a session: the runtime sets both
// under either database store (Options.SessionStore) and the pod baseline the first to file
// otherwise (internal/podsafety), so neither is ever the operator's or a provider key's to set.
var sessionStoreVariables = []string{ompsessions.StorageVariable, ompsessions.DSNFileVariable}

// providersDSNFile is where the providers Secret's session database URL key
// (ompsessions.Store.DSNSecret) is mounted under the postgres store: ompsessions.DSNFileName is
// the variable whose `_FILE` pointer names it, OMP_SESSION_SQL_DSN_FILE, so the shim, which skips
// a file whose pointer its environment carries, never exports the URL into Oh My Pi's environment
// whatever the Secret's key is called.
const providersDSNFile = ProvidersDir + "/" + ompsessions.DSNFileName

// sessionProxyFlagNames are the flags a pod's `legion launcher` is always told the Cloud SQL
// session database by, in the order sessionProxyFlags writes them: the instance's connection name,
// the database in it, and the IAM login the pod signs in as, which is the login of the service
// account it impersonates where the store names one (sessionProxyAccountFlag). All three are
// required, so a launcher carrying them is what marks a pod as created under cloudsql-iam
// (sessionProxyFlagsIn).
var sessionProxyFlagNames = [3]string{"--session-instance", "--session-database", "--session-user"}

// sessionProxyAccountFlag names the Google service account a pod's `legion launcher` impersonates
// with the pod's own credentials to sign in to Cloud SQL (Database.ImpersonateServiceAccount). It
// is optional, so it is no member of sessionProxyFlagNames: a pod that signs in as itself carries
// no such flag at all, never an empty one, so a deployment that names no account keeps the very
// command its pods were created with before the flag existed, and none of them drifts for it.
const sessionProxyAccountFlag = "--session-impersonate-service-account"

// sessionWriterAudienceFlag names the audience of the session writer token a pod's `legion
// launcher` reads at <roles-dir>/session-writer/token and exchanges, in place of the pod's own
// credentials, to impersonate the service account sessionProxyAccountFlag names
// (Database.WriterAudience, SessionWriterDir). It follows that flag, and only where the store
// names a writer audience, which the store names only with an account: a pod without one carries
// no such flag at all, so a deployment that names none keeps the very command its pods were
// created with before the flag existed, and none of them drifts for it.
const sessionWriterAudienceFlag = "--session-writer-audience"

// legionVolumeNames are the volumes Legion puts in a pod, an issue pod's, the controller's or the
// probe's, whose names the operator's volumes may not take: the shared ones, the role homes, the
// test database's, and each launcher role's private ones. The agent-secrets volumes are reserved
// whether or not this deployment enrolls, the workload identity token whether or not a pod of it,
// an issue's or the controller's, serves the metadata endpoint, the session writer token whether
// or not its session database names a writer audience, and the test database's whether or not it
// sets `runtime.kubernetes.test_postgres`: an operator's pod may never claim them. The exchange
// identity's (agentSecretsExchangeVolume) is Legion's only where exchange says the deployment
// names an exchange audience: one that names none keeps that name the operator's, as it was before
// the key existed. Every launcher role, the controller's included, has a key volume when the pod
// enrolls and a gh volume always.
func legionVolumeNames(exchange bool) []string {
	names := []string{
		issueVolume, provisionVolume, tempVolume, configVolume, providersVolume, agentSecretsTokenVolume, workloadIdentityTokenVolume,
		sessionWriterTokenVolume, roleHomesVolume, roleProbeVolume, testPGVolume,
	}
	if exchange {
		names = append(names, agentSecretsExchangeVolume)
	}
	for _, role := range launcherRoles {
		names = append(names, roleVolume("launcher", role), roleVolume("private", role), roleVolume(stateVolume, role), roleVolume(ghVolume, role),
			roleVolume(agentSecretsKeyVolume, role))
	}
	slices.Sort(names)
	return names
}

// legionMountPaths are where Legion mounts a volume in the containers the operator's mounts join,
// the launcher container of either pod and the image probe's, or where a role's namespace binds
// one: an operator's mount may be neither at, under, nor above one. AgentSecretsKeyDir and
// AgentSecretsTokenDir are reserved whether or not this deployment enrolls, AgentSecretsExchangeDir
// only where exchange says it names an exchange audience (one that names none keeps the path the
// operator's, as it was before the key existed), GHConfigDir whether or
// not the pod is an issue's, FeedDir, which TreeRoot already covers, is named for the read-only
// mount every issue pod's container carries there, RolesDir is where an issue pod mounts every
// role's own volumes, which each role's namespace covers whole, HomeMountsDir is where an issue
// pod's roles container mounts what belongs under the home (homeMountsView), which each role's
// namespace covers too once it has bound each back, RoleProbeDir is the image probe's volume
// for its role-namespace check, ControllerWorkloadIdentityDir is where the controller's pod mounts
// the metadata endpoint's token, reserved whether or not that pod runs in a microVM or the
// deployment names the endpoint (an issue pod's, WorkloadIdentityDir, is under RolesDir), and
// TestPGLogFile is reserved whether or not the deployment serves a test database: it is where each
// role's namespace binds the pod's test server log when the pod serves one.
func legionMountPaths(exchange bool) []string {
	paths := []string{
		TreeRoot, FeedDir, LauncherDir, LauncherPrivateDir, StateDir, xdgConfigHome, ProvidersDir,
		AgentSecretsKeyDir, AgentSecretsTokenDir, GHConfigDir, RolesDir, HomeMountsDir, RoleProbeDir,
		ControllerWorkloadIdentityDir, TestPGLogFile,
	}
	if exchange {
		paths = append(paths, AgentSecretsExchangeDir)
	}
	slices.Sort(paths)
	return paths
}

// roleNamespacePaths are the directories each role's namespace puts filesystems of its own at
// (`legion launcher role-init`): its temporary directories, binds of its directories under
// RoleHomesDir, and small private memory filesystems. role-init carries back a mount it finds under
// one of them, so an operator's mount may be under one, but one at or above one would be covered in
// every role's namespace. role-init resolves each before it covers it, so a mount spelled through
// one of the worker image's links to them (imageLinks: /var/lock and /var/run/lock to /run/lock,
// /run/shm and /var/run/shm to /dev/shm) lands on what it covers, which CheckPod compares through
// those links.
var roleNamespacePaths = []string{"/dev/shm", "/run/lock", "/tmp", "/var/tmp"}

// roleReplacedPaths are the paths each role's namespace replaces with a filesystem of its own and
// carries no mount back onto: role-init's privateTerminals mounts a new devpts instance at
// /dev/pts and binds that instance's ptmx over /dev/ptmx, privateQueues mounts its own IPC
// namespace's mqueue at /dev/mqueue, and prepareRoleNamespace mounts the role's own /proc last of
// all. An operator's mount at, under or above one would vanish from every role, so CheckPod
// refuses every one, where it refuses only a mount at or above a roleNamespacePaths entry, whose
// mounts role-init carries back. role-init's other mounts are guarded elsewhere: its binds under
// launcher.FixedRoot and its covers of RolesDir and HomeMountsDir are legionMountPaths, and its
// home overlay carries every operator mount under the home back (homeMountsView), xdgStateHome
// refusing one at or above it.
var roleReplacedPaths = []string{"/dev/mqueue", "/dev/ptmx", "/dev/pts", "/proc"}

// launch is one relaunch's inputs, checked and resolved before anything touches the cluster.
type launch struct {
	spec runtime.SpawnSpec
	name string
	// kind is the pod's kind, decided once from the claim (prepare): every facet of the pod that an
	// issue pod and the project controller's build differently is the kind's.
	kind podKind
	// microVM is where the pod runs when it runs in a microVM (Options.Sandbox's MicroVM), decided
	// once (prepare): an issue pod's from its issue, the controller's from the project's type
	// (SandboxConfig.Controller). It is nil for a gVisor pod and for every launch built other than
	// by prepare. Every facet of the pod a microVM pod builds differently reads it (placement, the
	// label, the launcher's flags, the workload identity token, the shape the template records,
	// launch.shapeKey, and an issue pod's disk's class, diskSpec); a role started in a pod the
	// launch keeps reads none of them.
	microVM *MicroVM
	// roles are the pod's launcher roles, all of them served by the pod's one container: the roles
	// of the kind its labels name (podKindOf).
	roles []claim.Role
	// workspace is the launchers' working directory (podKind.prepare): an issue pod's workspace,
	// which workspace-init provisions on the issue's volume (workspace.Location under TreeRoot), or
	// the root of the controller's own volume, TreeRoot.
	workspace string
	// secrets are the claim's launch credentials, each reaching the agent as a `<NAME>_FILE`
	// pointer into its generation's private directory: the boot token, the spec's but the providers
	// Secret's own (Options.ProvidersSecrets, which the runtime points at the providers mount
	// whatever the spec carries), and the Dispatch bearer when Dispatch is configured with one
	// (DispatchCredential.Bearer) — or, under dispatch_token_broker, the transition token for a
	// launch that keeps a pod with no exchange identity (relaunch's dispatchCredential), and no
	// Dispatch token at all for every other launch.
	secrets map[string]string
	// prompt is the launch's composed system prompt (runtime.ComposePrompt), which reaches the agent
	// as a file and never as an argv string: launcherCommand carries it in the start command's Files
	// (systemPromptPath says where the launcher writes it and who can read it).
	prompt string
	// cacheURL is the address of the issue's repository's cache (cacheURL, set by
	// issuePod.provision), which its fetch container is told (fetchEnvironment).
	cacheURL string
	// gitHubDirs are the directories of the pod's gh bundle, sorted, whose files each role's gh
	// volume projects — a GitHub owner, or one of its repositories as `<owner>/<repo>` — and
	// gitHubDefault the owner whose directory is every agent's GH_CONFIG_DIR: the
	// bundle writeLauncherSecrets mints for a new pod, or, for a pod a launch keeps, the default its
	// role Secret records (GitHubDefaultKey, launcherBound), the projection being the pod's own.
	gitHubDirs    []string
	gitHubDefault string
	// gh is the image's real gh (Tools.GH), every agent's LEGION_GH_PATH.
	gh string
	// claude is what a Claude Code launch needs beyond an Oh My Pi one, resolved once in prepare
	// and zero for every other backend (claudeInputs).
	claude claudeInputs
	// omp is the session an Oh My Pi launch that trades an exchange identity starts its agent
	// under (ompInputs): prepare fills it where the configuration trades, and relaunch clears it
	// for a launch that keeps a pod that trades nothing, so it is zero for a Claude Code launch and
	// for every launch that trades nothing, and its readers ask nothing else.
	omp ompInputs
	// kept is what the live pod this launch keeps rather than replaces already carries for its
	// role, read from that pod's stored template (relaunch's keptBy). Nil is a launch that builds
	// the pod itself — every replacing launch, and every caller that only builds a manifest — whose
	// pod carries the configuration as it now reads.
	kept *keptPod
}

// keptPod is what a live pod a launch keeps was built with, for the one role that launch starts in
// it: the facets of the pod the role's own start command names, which a pod cannot be changed to
// carry (INFRA-484). A pod that has drifted from the configuration is replaced by an update round,
// which first asks every role in it to do the steps of its update message (supervise.UpdateTask)
// and answer, and not by the launch that finds it; so the launch starts its role as that pod was
// built, and the role runs degraded — without the secrets broker, without an overlay added
// since — until the round replaces the pod. It can still do those steps and answer the round,
// which a process that refused to start could not.
type keptPod struct {
	// enrolled is the enrollment decision (Runtime.enrollsIn) read of this pod when the launch
	// began: the runtime enrols its pods and the pod carries that enrollment for the role, both
	// the projected token volume and the role's own key volume, the two the shim's agent-secrets
	// flags name and stat (carriesEnrollment, launcherCommand).
	enrolled bool
	// trades is the exchange decision (Runtime.tradesIn) read of this pod when the launch began:
	// it is enrolled, the runtime names an exchange audience, and the pod carries the exchange
	// token that audience projects (carriesExchangeIdentity), the file the role's
	// AGENT_SECRETS_MACHINE_TOKEN_FILE would name.
	trades bool
	// overlays are the role's own overlay files (Options.RoleConfigFiles) the pod's container
	// actually holds, in configuration order: the only ones its generation's PI_CONFIG_FILES may
	// name (heldRoleOverlays, roleOperatorEnv).
	overlays []string
	// variantFiles are the overlay files of the project's declared variants
	// (Options.ExperimentVariants) the pod's container actually holds, in configuration order:
	// what the pod holds for every role, as overlays is (heldRoleOverlays). Which of them a
	// launch is told is a launch's own question, and launch.overlays asks it once — a launch
	// names the files of the arms its unit was drawn into (runtime.SpawnSpec.Variants), and of
	// those only the ones this list holds, after the role's own (roleOperatorEnv).
	variantFiles []string
	// store is the session store this pod was built under (heldSessionStore), the kind alone: it
	// is all a start command names, since the URL itself is in the file the pod projects or its
	// own launcher writes (sessionDSNFile). A pod created before the store was configured carries
	// none, and its role is told neither of Oh My Pi's two session variables rather than a URL
	// file it cannot read.
	store ompsessions.StoreKind
	// checker is the manifest digest of the deployment's leak check this pod's install holds, as
	// the pod records it (recordedLeakChecker): the digest its template was built under
	// (leakCheckerAnnotation), the install made at the image's own pin, or else the digest of the
	// tarball the daemon served into it (leakCheckPushedAnnotation); and so the one its role is
	// told (launch.leakChecker). recorded is whether the pod records one at all. A pod that records
	// none was built under no setting, or by a daemon before the record: its role is told none where
	// its launcher holds an install from before, which a daemon of a release before the setting
	// pushed and hashed nothing of. Where it holds none, serveLeakCheck records the manifest of the
	// served tarball for the reconnected session, then tells that digest to the role.
	checker  string
	recorded bool
}

// leakCheckerAnnotation records on a pod template's metadata the manifest digest of the
// deployment's leak check (Options.LeakCheckSetting's ManifestSHA256) the pod was built under,
// written where the template is built (podTemplate) and only under a configured setting. A pod's
// install is made once, at its own image's pin, so a pod kept after the deployment moved its pin
// and its digest together holds the install of the old pin, whose digest is the old one. A role
// started in such a pod is told the digest its pod was built under (keptPod.checker), so its shim
// starts against the install its pod holds, and it can push its work and answer the update round
// that replaces the pod (leakCheckDrift), where told the digest now it would start no agent at all.
const leakCheckerAnnotation = "legion.dev/leak-checker-manifest-sha256"

// leakChecker is the manifest digest this launch's generation is told (leakguard.CheckerVar): the
// one the live pod it keeps records (keptPod.checker), and the deployment's own for a launch that
// builds the pod itself. "" tells none.
func (l launch) leakChecker(r *Runtime) string {
	if l.kept != nil {
		return l.kept.checker
	}
	return r.leakCheck.ManifestSHA256
}

// enrollsIn is the one decision whether a generation of role started now in a pod holds the
// secrets broker: the runtime enrols its pods at all (runtime.kubernetes.agent_secrets), and the
// pod that generation runs in carries that enrollment — the broker's projected token volume and
// the role's own key volume (carriesEnrollment). A nil pod is a launch that builds the pod
// itself, which carries the configuration as it now reads.
//
// Everything a generation's broker reaches reads this one answer, so its argv and its environment
// can never disagree again (INFRA-484): the shim's three agent-secrets flags (launcherCommand)
// and the AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR an agent reads to decide it holds a broker
// at all (mainEnvironment), both through launch.enrolls; the record of what the generation was
// handed (handedAddresses, recordFor, from the launch's own answer); and the comparison an
// observation makes against that record (movedEnvironment, from the live pod the generation runs
// in). A role in a kept pod that carries no enrollment is told of none, so it reports a resource
// it cannot reach rather than a secrets grant it never had, and its generation is recorded as
// carrying no broker URL, so moving the broker's URL never reads as StaleAddress and never
// repoints that role mid-turn: the update round replaces its pod once every worker has answered,
// no turn runs and no claim's activity report holds it (internal/daemon's roundEnd.holding), or
// at the round's bound.
//
// The launch reads the pod from the Sandbox's stored template, the pod's own build (keptBy), and
// an observation from the live pod; neither a kubelet nor an admission webhook takes a volume off
// a pod, so the two read the same enrollment.
func (r *Runtime) enrollsIn(pod *corev1.PodSpec, role claim.Role) bool {
	return r.agentSecrets != nil && (pod == nil || carriesEnrollment(*pod, role))
}

// enrolls is this launch's reading of that one decision: of the live pod it keeps, as the launch
// read that pod's stored template when it began (keptPod.enrolled), or of the pod it builds
// itself.
func (l launch) enrolls(r *Runtime) bool {
	if l.kept != nil {
		return l.kept.enrolled
	}
	return r.enrollsIn(nil, l.spec.Role)
}

// tradesIn is the one decision whether a generation of role started now in a pod holds an
// identity the broker's exchange trades: it holds the broker (enrollsIn), the runtime names an
// exchange audience (runtime.kubernetes.agent_secrets.exchange_audience), and the pod carries the
// projected token for it (carriesExchangeIdentity). A nil pod is a launch that builds the pod
// itself, which carries the configuration as it now reads. A role in a kept pod built before the
// audience was named is told no identity, so its broker clients keep the credential they had
// rather than fail every trade on a token file that is not there, until the update round replaces
// the pod (movedExchangeIdentity).
func (r *Runtime) tradesIn(pod *corev1.PodSpec, role claim.Role) bool {
	return r.enrollsIn(pod, role) && r.agentSecrets.ExchangeAudience != "" && (pod == nil || carriesExchangeIdentity(*pod, r.agentSecrets))
}

// trades is this launch's reading of that decision: of the live pod it keeps, as the launch read
// that pod's stored template when it began (keptPod.trades), or of the pod it builds itself.
func (l launch) trades(r *Runtime) bool {
	if l.kept != nil {
		return l.kept.trades
	}
	return r.tradesIn(nil, l.spec.Role)
}

// brokersDispatch is whether this launch's role holds no Dispatch token and trades its pod's
// exchange identity for one of its own: the deployment sets dispatch_token_broker and the pod the
// role runs in carries that identity (trades). Such a role is told DISPATCH_TOKEN_BROKER=1
// (mainEnvironment), and its launch carries neither the bearer nor the transition token; under
// the setting, a launch that is not one keeps a pod built before the identity, and carries the
// transition token or waits for that pod's round (relaunch's dispatchCredential).
func (l launch) brokersDispatch(r *Runtime) bool {
	return r.dispatch.Broker && l.trades(r)
}

// overlays are this launch's settings overlays, which its generation is told in PI_CONFIG_FILES
// after the operator's own (roleOperatorEnv): the role's own overlays, then the overlays of each
// arm this launch's unit was drawn into (runtime.SpawnSpec.Variants), in the order the project's
// file declares the experiments, which outrank them. They are the ones a kept pod holds, which
// may be fewer than role_config_files and the declared variants now name, and all of the
// configuration's for a launch that builds the pod. This is the one place a launch's variants
// decide what it is told, for a kept pod as for a fresh one: the pod holds every declared
// variant's overlays for every role of it (keptPod.variantFiles).
//
// A pod built before an arm's mount was added holds none of that arm's files, so a unit recorded
// in it starts its role without them — in neither arm, until the update round replaces the pod —
// since Oh My Pi refuses to start on an entry that is not there and a role that never starts
// never answers that round (INFRA-484). Each such drop is logged once, at the launch that makes
// it, naming the issue, the role, the experiment and the arm.
//
// carried are the arms the launch runs in: each arm of l.spec.Variants, in its order, of which
// the launch names every overlay — every arm for a launch that builds its pod, and for one that
// keeps a pod, each arm none of whose overlays the pod lacks. An arm the pod holds only some of
// is not carried, though the launch still names the overlays it holds. They are what the launch's
// spend attributes name as its arms (roleOperatorEnv, runtime.SpendArmsAttribute), so each model
// call of this generation says which arm's files it ran with, which nothing else records.
func (l launch) overlays(r *Runtime) (files []string, carried []runtime.Variant) {
	own := r.roleConfigFiles[l.spec.Role]
	if l.kept != nil {
		own = l.kept.overlays
	}
	var variants []string
	for _, variant := range l.spec.Variants {
		held := variant.ConfigFiles
		var dropped []string
		if l.kept != nil {
			held, dropped = splitHeld(l.kept.variantFiles, variant.ConfigFiles)
		}
		variants = append(variants, held...)
		if len(dropped) > 0 {
			r.log.Warn("sandbox runtime: the pod this launch keeps was built before this variant's overlays were mounted; its role starts without them, in neither arm, until the update round replaces the pod",
				"issue", l.spec.Issue, "role", l.spec.Role, "experiment", variant.Experiment, "variant", variant.Name,
				"dropped", dropped)
			continue
		}
		carried = append(carried, variant)
	}
	return withVariantOverlays(own, variants), carried
}

// splitHeld is want in two, each part in want's order: the files held holds, and the files it
// does not. For one variant's overlays against a kept pod, that is the share the pod carries,
// which the launch names, and the share it lacks, which the launch's one log line names.
func splitHeld(held, want []string) (kept, missing []string) {
	for _, file := range want {
		if slices.Contains(held, file) {
			kept = append(kept, file)
		} else {
			missing = append(missing, file)
		}
	}
	return kept, missing
}

// withVariantOverlays is one PI_CONFIG_FILES list: a role's own overlays and then the variants'
// own, which outrank them, and own itself where there is no variant overlay to add, so a
// deployment whose project declares no experiment of its own and writes no `memory` block, and
// one whose variants name no overlay, hands the launch the list it handed before any experiment
// existed (launch.overlays, its one caller; the image probe's values are
// runtime.RoleOverlayValues').
func withVariantOverlays(own, variants []string) []string {
	if len(variants) == 0 {
		return own
	}
	return slices.Concat(own, variants)
}

// sessionStore is the kind of store this launch's generation keeps its session in: the one the
// kept pod was built under, which is the only one its role can reach (keptPod.store), and the
// configuration's for a launch that builds the pod itself. The kind is all of a store a start
// command names, since the URL itself is in the file the pod projects or its own launcher writes
// (sessionDSNFile). Oh My Pi refuses to start on a URL file it cannot read, so a role of a kept
// pod told the configuration's store — a key the pod's providers volume does not project, a
// proxy its launcher does not serve — would never register and never answer the round that is to
// replace that pod (INFRA-484). What it costs is that such a role writes its session into the
// store its pod has until the round replaces the pod, which a role that never started could not
// do at all.
func (l launch) sessionStore(r *Runtime) ompsessions.StoreKind {
	if l.kept != nil {
		return l.kept.store
	}
	return r.sessionStore.Kind
}

// claudeInputs are the parts of a Claude Code launch that are decided before the pod is touched:
// the `legion` the pod runs claude-bridge from, the session this generation starts, and the MCP
// document its launcher writes. Each is resolved in prepare rather than where it is used, so one
// generation's agent command and its launcher file name the same session and the same document
// however often either is rebuilt.
type claudeInputs struct {
	// legion is the image's `legion` (Tools.Legion), whose claude-bridge runs the worker's turns.
	legion string
	// session is the uuid the agent command names: the one the recorded transcript continues when
	// the launch resumes (prepare read it from spec.ResumeSessionFile with
	// claudelaunch.ResumeSession), and a fresh one otherwise.
	session string
	// resume is whether session is a recorded transcript's rather than a fresh uuid, which is the
	// difference between the agent command's `--resume` and its `--session-id` (claudelaunch.Argv).
	resume bool
	// mcp is the `--mcp-config` document (claudelaunch.MCPConfig), which the launcher writes into
	// the generation's private directory as claudeMCPFile.
	mcp string
}

// ompInputs is the session an Oh My Pi launch that trades an exchange identity (launch.trades)
// starts its agent under, decided before the pod is touched as a Claude Code launch's is
// (claudeInputs): the id its agent command names (agentArgv) is the id the runtime answers for the
// launch (nameSession, LaunchSession), which the daemon registers for the pod at the shim's hello,
// before the agent registers (supervise's ensureRegistered). prepare fills it where the
// configuration trades, and relaunch clears it, once it knows the pod it keeps, for a launch into
// a kept pod that trades nothing (keptPod.trades). So an Oh My Pi launch's is zero exactly when
// the launch trades nothing, and each reader decides from it alone (agentArgv, nameSession,
// ompResumeRefusal), save the lane agentArgv's first case owns: a resume of a session the claim
// records with no file (spec.ResumeSession), which agentArgv passes as the one word
// `--resume=<id>` whether the launch trades or not, so that lane needs no resume. A launch that
// trades nothing names no id the runtime chose and keeps the argv it had before the runtime chose
// one, since its pod may run the Oh My Pi of an image older than `--session-id`, which refuses a
// flag it does not know, and a role that trades nothing gains nothing from a chosen id.
type ompInputs struct {
	// session is the id the agent command names: the one the claim recorded when the launch
	// resumes, and a fresh UUIDv7 otherwise (ompSessionOf). It is "" for a resume that has no id
	// to resume by, which a launch that trades is refused for (ompResumeRefusal).
	session string
	// resume is whether agentArgv passes session as `--resume <id>`, a recorded session's id,
	// rather than as `--session-id <id>`, a fresh one. It is false for a resume by
	// spec.ResumeSession, which agentArgv's first case passes as `--resume=<id>` before it reads
	// resume.
	resume bool
}

// ompSessionOf is the session an Oh My Pi launch of spec starts its agent under when it trades: a
// fresh UUIDv7 for a launch that starts a session, and for one that resumes, the session id the
// resume names with no file (spec.ResumeSession: the claim records the session and no file of
// it), else the session id the claim recorded beside its file (spec.ResumeSessionID), or, for a
// claim that recorded none, the id the recorded file's name gives, "" where the name gives none. Oh My Pi names a session file
// `<timestamp>_<id>.jsonl`, and the id is read by the rule the claim's file-id hold is
// (supervise.RecordedHolds, HoldFileID), so the launch resumes by a name the supervisor held to the
// claim before it asked for the resume (holdRecorded). Nothing is refused here: whether the
// launch trades, and so needs an id at all, is known only once relaunch knows the pod it keeps
// (ompResumeRefusal).
func ompSessionOf(spec runtime.SpawnSpec) (ompInputs, error) {
	if spec.ResumeSession != "" {
		return ompInputs{session: spec.ResumeSession}, nil
	}
	if spec.ResumeSessionFile == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return ompInputs{}, fmt.Errorf("choose its Oh My Pi session id: %w", err)
		}
		return ompInputs{session: id.String()}, nil
	}
	if spec.ResumeSessionID != "" {
		return ompInputs{session: spec.ResumeSessionID, resume: true}, nil
	}
	for _, hold := range supervise.RecordedHolds("", spec.ResumeSessionFile) {
		if hold.Kind == supervise.HoldFileID {
			return ompInputs{session: hold.Name, resume: true}, nil
		}
	}
	return ompInputs{resume: true}, nil
}

// ompResumeRefusal refuses a launch that trades and resumes (l.omp.resume, which relaunch clears
// for a launch that keeps a pod that trades nothing), when the id it would pass as `--resume <id>`
// is none Oh My Pi reads as a session id: there is none, because the claim recorded none and its
// file's name gives none (`--resume <id>` takes no path), or the id has a shape no session id has
// (sessionIDFault), such as one an argument parser would take for a flag. relaunch asks it once
// the pod it keeps is known and before anything of the pod is touched, so the refusal reaches the
// supervisor as the launch's own. A launch that trades nothing resumes by the file as before and
// is refused nothing, and a resume of a claim's session recorded with no file (spec.ResumeSession)
// is not l.omp.resume at all (ompSessionOf): agentArgv's first case owns it, passing that id as
// the one word `--resume=<id>`, which carries any id a two-word `--resume <id>` cannot, one that
// begins with `-` included.
func (l launch) ompResumeRefusal() error {
	if !l.omp.resume {
		return nil
	}
	if l.omp.session == "" {
		return fmt.Errorf("the recorded session %s gives no session id to resume by: its name is not an Oh My Pi session file's, <timestamp>_<id>.jsonl",
			l.spec.ResumeSessionFile)
	}
	fault := sessionIDFault(l.omp.session)
	if fault == "" {
		return nil
	}
	source := "the session id its claim recorded"
	if l.spec.ResumeSessionID == "" {
		source = "the session id the name of " + l.spec.ResumeSessionFile + " gives"
	}
	return fmt.Errorf("%s, %+q, %s: no `--resume <id>` can pass it", source, l.omp.session, fault)
}

// sessionIDFault is why id, the non-empty id ompResumeRefusal holds, cannot be passed as a word of
// its own in Oh My Pi's argv and be read as the session id it is, and "" for an id that can: one
// that begins with `-` (an argument parser reads such a word as a flag), or holds white space or a
// control character. An empty id is a resume with no id at all, which ompResumeRefusal refuses
// before it asks. No id the runtime chooses has any of them (ompSessionOf's UUIDv7). The bound on
// a session name's length is the registration's (claim.MaxSessionNameBytes), and no recorded name
// past it gets this far (holdRecorded refuses the resume first).
func sessionIDFault(id string) string {
	switch {
	case strings.HasPrefix(id, "-"):
		return "begins with -, which an argument parser reads as a flag"
	case strings.ContainsFunc(id, unicode.IsSpace):
		return "holds white space"
	case strings.ContainsFunc(id, unicode.IsControl):
		return "holds a control character"
	}
	return ""
}

// runsClaude is whether the launch's role runs Claude Code rather than the image's Oh My Pi: its
// agent command, its launcher file and three of its variables are the Claude lane's.
func (l launch) runsClaude() bool { return runtime.Of(l.spec.Backend) == runtime.BackendClaude }

// prepare checks spec and resolves everything a launch needs from it, reading the prompt files on
// the daemon's disk, so a launch that cannot be honoured is refused before any API call: the
// shared refusal, over every name a spec may not set — the runtime's own, the composed ones and,
// where the deployment names an exchange audience, the exchange identity's (runtime.ValidateSpawnSpec,
// Runtime.specOwned) — then the sandbox's own. A secret named
// for the provisioning token is refused, the one key the runtime writes whose pointer the worker
// container is never told (the boot token's and the Dispatch bearer's pointers are runtime-owned,
// so the shared refusal already covers them). prepare decides the pod's kind from the claim's role
// (podKindFor, which an update round reads a recorded process's kind by too): the project
// controller's claim (`controller: daemon`) runs the controller's pod, every workflow claim
// its issue's pod, which refuses a spec with no repository (issuePod.prepare). The labels the kind
// gives the pod must name that kind back (podKindOf), so a launch and the Sandbox it leaves cannot
// disagree on the pod's roles. It decides the pod's sandbox too, once: an issue pod's is its
// issue's (SandboxConfig.Of), and the controller's pod's the project's type
// (SandboxConfig.Controller), which no canary list moves. The system prompt is composed here
// (runtime.ComposePrompt), and a prompt whose start frame no launcher would read is refused here
// too (runtime.PromptTooLarge, wrapped so the supervisor can tell it from a launch that failed).
// So is a Claude Code spec that names its session by its id alone (ResumeSession), whoever was
// to resolve that id: a Claude Code claim's recorded session is its transcript's path, which the
// id is read from (claudelaunch.ResumeSession), and supervise forgets a Claude Code session
// recorded with no file rather than look it up, so no Claude Code command is started on an id it
// would find no transcript of.
func (r *Runtime) prepare(spec runtime.SpawnSpec) (launch, error) {
	if err := runtime.ValidateSpawnSpec(spec, r.specOwned()); err != nil {
		return launch{}, err
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("sandbox launch %s: "+format, append([]any{spec.Claim}, args...)...)
	}
	if _, ok := spec.Secrets[provisionTokenKey]; ok {
		return launch{}, refuse("secret %s is a key the runtime writes itself", provisionTokenKey)
	}
	l := launch{spec: spec, name: SandboxName(spec.Claim), kind: podKindFor(spec.Role), gh: r.tools.GH}
	if spec.Role != claim.RoleController {
		l.cacheURL = r.cacheURL(CacheSandboxName(r.project, spec.Repository))
	}
	where := r.sandboxConfig.Controller()
	if _, issue := l.kind.(issuePod); issue {
		where = r.sandboxConfig.Of(spec.Issue)
	}
	if where == SandboxMicroVM {
		l.microVM = r.sandboxConfig.MicroVM
	}
	if l.runsClaude() {
		if spec.ResumeSession != "" {
			return launch{}, refuse("a Claude Code claim resumes its session from its recorded transcript, never by the session id %s alone", spec.ResumeSession)
		}
		mcp, err := claudelaunch.MCPConfig(claudeBun, claudePluginDir, claudeConfigDir(spec.Role))
		if err != nil {
			return launch{}, refuse("its Claude Code MCP configuration: %v", err)
		}
		l.claude = claudeInputs{legion: r.tools.Legion, session: claudelaunch.NewSessionID(), mcp: string(mcp)}
		// A launch that resumes continues the recorded transcript, by the id read from its path,
		// which is the rule a Claude claim's recorded session is held to and the tmux lane's too
		// (claudeResumeSession, runtime/tmux/claude.go): a transcript of this pod's own Claude
		// Code directory and nothing else. One that starts a session names a fresh uuid.
		if spec.ResumeSessionFile != "" {
			session, err := claudelaunch.ResumeSession(claudeConfigDir(spec.Role), spec.ResumeSessionFile)
			if err != nil {
				return launch{}, refuse("%v", err)
			}
			l.claude.session, l.claude.resume = session, true
		}
	}
	// An Oh My Pi launch that trades starts its agent under an id the runtime chose (ompInputs).
	// Here trades is the configuration's answer, for the pod a launch builds; relaunch clears the
	// choice for a launch that keeps a pod that trades nothing, once it knows that pod (keptBy).
	if !l.runsClaude() && l.trades(r) {
		omp, err := ompSessionOf(spec)
		if err != nil {
			return launch{}, refuse("%v", err)
		}
		l.omp = omp
	}
	if err := l.kind.prepare(&l); err != nil {
		return launch{}, refuse("%v", err)
	}
	named, err := podKindOf(r.labels(l))
	switch {
	case err != nil:
		return launch{}, refuse("its pod: %v", err)
	case named != l.kind:
		return launch{}, refuse("its pod's labels %v name another kind of pod than the one it builds", r.labels(l))
	}
	l.roles = named.roles()
	prompt, err := runtime.ComposePrompt(spec.Prompt)
	if err != nil {
		return launch{}, refuse("%w", err)
	}
	secrets := maps.Clone(spec.Secrets)
	if secrets == nil {
		secrets = map[string]string{}
	}
	for _, name := range r.providersSecrets {
		delete(secrets, name)
	}
	// The shared bearer, when Dispatch is configured with one. Under dispatch_token_broker no
	// launch carries it: a launch that keeps a pod with no exchange identity is given the
	// transition token instead, once relaunch knows it keeps that pod (dispatchCredential).
	if r.dispatch.Bearer != "" {
		secrets[dispatchTokenKey] = r.dispatch.Bearer
	}
	l.secrets, l.prompt = secrets, prompt
	// The prompt travels to the pod in the start command's Files, one frame a launcher reads whole
	// (checkStartFrame), so a prompt that would overrun it is refused now, before anything of the
	// pod is touched; relaunch checks the start it sends again.
	if err := l.checkStartFrame(launcherCommand(l, r)); err != nil {
		return launch{}, refuse("%w", err)
	}
	return l, nil
}

// SessionOnVolume is where a recorded session file sits on a tree or controller volume mounted at
// root: an agent's sessions directory was the volume's SessionsSubPath, so a session's path below
// the one is its path below the other. No pod keeps sessions on a volume any more (INFRA-80), so
// its one caller is `legion sessions import --tree-volume`, the copy of a legion-v10.0.0
// deployment's session files into the session table. It refuses a path no pod ever kept a session
// at, which names no place on such a volume.
func SessionOnVolume(file, root string) (string, error) {
	rest, ok := strings.CutPrefix(file, ompSessionsDir+"/")
	if !ok || rest == "" || filepath.Clean(rest) != rest || strings.HasPrefix(rest, "../") {
		return "", fmt.Errorf("recorded OMP session file %s is not under %s, the only directory a pod kept sessions in; it names no place on a tree volume",
			file, ompSessionsDir)
	}
	return filepath.Join(root, SessionsSubPath, rest), nil
}

// SessionFromVolume is the recorded session path a file on a tree or controller
// volume had under the pod's sessions directory. It is SessionOnVolume's
// inverse and refuses a file outside the volume's sessions subdirectory.
func SessionFromVolume(file, root string) (string, error) {
	sessions := filepath.Join(root, SessionsSubPath)
	relative, err := filepath.Rel(sessions, filepath.Clean(file))
	if err != nil || relative == "." || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("session file %s is not under %s", file, sessions)
	}
	return filepath.Join(ompSessionsDir, relative), nil
}

// agentArgv is the command the shim runs, the launch's own harness: the Claude Code command for a
// Claude role (claudeArgv), and otherwise the image's Oh My Pi with its two explicit extensions,
// the Envoy plugin first, `--model @default` so the session runs the model the pod's overlay puts
// on the `default` role, the session, then RPC mode and `--append-system-prompt` naming the file
// the launcher writes the launch's system prompt to (systemPromptPath), never the text.
//
// The session: a launch that resumes a claim's session that records no file names the session's
// id as one `--resume=<id>` (ResumeSession), which the launcher replaces with the file it looks up
// where it was told the id (launcherCommand) and which Oh My Pi resolves itself where the launch
// has the agent resolve it (ResumeByAgent); a launch that trades runs under that same id
// (ompSessionOf). Otherwise the session is the one prepare chose where the launch trades an
// exchange identity (ompInputs, which is zero for a launch that trades nothing): `--session-id
// <id>` for a launch that starts a session and `--resume <id>` for one that resumes, and no path,
// while the start command still names the recorded file (launcherCommand's ResumeFile), by which
// the launcher looks the session up. A launch that trades nothing names no chosen id:
// `--resume=<file>` on the recorded session when it resumes, and nothing when it starts one.
//
// Oh My Pi 18.16.0 reads an `--append-system-prompt` value with no newline as a path and appends
// that file's contents, so the prompt meets no argv bound however long the deployment's
// instructions grow (TestOhMyPiAppendsTheWholePromptFileItIsNamed). Were the file missing, Oh My
// Pi would silently take the path itself as the prompt's text, which is why the launcher writes
// every Files entry before it starts the child (launcherCommand). For a plain
// launch the two roots are the image's own (envoyPlugin, legionPlugin); for an installing launch
// (l.spec.Plugins set, LEGION-164) they are the pair launcherCommand's install wrapper lands under
// this generation's own directory (pluginRoots), installed before Oh My Pi starts rather than the
// image's. Extension discovery stays on either way: a pod runs the
// repository's .omp/extensions, hooks, TypeScript commands, `extensions:` settings and installed
// profile plugins exactly as a tmux pane always has (the boot gate's pane lane is built on
// discovery). Repository code runs in the agent's process, which holds the role's credential grant
// file, the credential helper and the Envoy seed, and may register a provider; the operator's
// overlay still outranks the repository's settings by Oh My Pi's own order, and the operator
// route's `enabledModels` still bounds the session's model. The two plugins are roots the worker
// image leaves unlinked from its profile: Oh My Pi loads a plugin that is both installed and named
// by --extension twice, which the boot gate's pod lane refuses an image for.
//
// `--model @default` rides every launch, not the resuming one alone: @default is Oh My Pi's name
// for the `default` model role, which every pod's operator overlay sets, the operator's current
// role is resolved per launch, and a resumed session without the flag runs the session file's
// recorded model. From Oh My Pi 18.18.6 of the fork (upstream oh-my-pi #18534), a resumed launch
// under this role alias appends a replacement `model_change` for its resolved model, which later
// launches without the flag read; that replacement is the role as it stood at the launch that
// wrote it, so it stands in for no later launch's flag. A profile with no `default` role is not
// refused here: Oh My Pi launches silently on a built-in model instead, which is the deployment's
// to notice (deploy/kubernetes/operator-route/README.md, "Changing a role's model").
func (l launch) agentArgv(agent []string) []string {
	if l.runsClaude() {
		return l.claudeArgv()
	}
	envoy, legion := envoyPlugin, legionPlugin
	if l.spec.Plugins != nil {
		envoy, legion = pluginRoots(pluginGenDir(roleStateHome(l.spec.Role), l.spec.Generation))
	}
	argv := append(slices.Clone(agent), "--extension", envoy, "--extension", legion, "--model", "@default")
	switch {
	case l.spec.ResumeSession != "":
		argv = append(argv, "--resume="+l.spec.ResumeSession)
	case l.omp.resume:
		argv = append(argv, "--resume", l.omp.session)
	case l.omp.session != "":
		argv = append(argv, "--session-id", l.omp.session)
	case l.spec.ResumeSessionFile != "":
		argv = append(argv, "--resume="+l.spec.ResumeSessionFile)
	}
	return append(argv, "--mode", "rpc", "--append-system-prompt", systemPromptPath(l.spec.Generation))
}

// claudeArgv is the command the shim runs for a Claude Code role: the agent command
// claudelaunch.Argv builds around the `legion claude-bridge` the pod runs it under, naming the
// Claude Code this generation's start script installed (claudeCodeBinary, under the role's state
// home: the image carries none) and the image's claude-envoy root (claudePluginDir), reading the
// MCP document its launcher writes into this generation's private directory (claudeMCPFile), and
// `--append-system-prompt-file` naming the system prompt file the same launcher writes beside it
// (systemPromptPath), the file the Oh My Pi branch names too: a pod cannot read the daemon's
// prompt files, so the daemon composes the text and the start command carries it (l.prompt).
// Claude Code sends the whole file and refuses to start on a missing one.
// The session is the launch's own (claudeInputs): `--resume <uuid>` of the transcript a replaced
// pod left, by the id prepare read from the recorded path, and `--session-id <fresh uuid>` when
// the launch starts a session. Claude Code resumes by id and never by path, so the recorded file
// name itself is never passed. Options.Agent, the operator's Oh My Pi override, does not reach
// here: the Claude command is the runtime's, as the tmux lane's is the host's boot-resolved one.
func (l launch) claudeArgv() []string {
	claude := claudeCodeBinary(roleStateHome(l.spec.Role), l.spec.Generation)
	paths := claudelaunch.Paths{Legion: l.claude.legion, Claude: claude, PluginDir: claudePluginDir}
	return claudelaunch.Argv(paths, generationDir(l.spec.Generation)+"/"+claudeMCPFile, l.claude.session, l.claude.resume,
		systemPromptPath(l.spec.Generation))
}

// labels are the pod's resource labels, as its kind gives them (podKind.labels).
func (r *Runtime) labels(l launch) map[string]string { return l.kind.labels(r.project, l) }

// diskSpec is the claim of l's pod's disk, wherever it is declared: claimSpec of the runtime's
// storage class and volume size, for the launch's kind and microVM block (nil for a gVisor pod).
// The controller's Sandbox declares it as the claim template it owns, and so does the Sandbox of
// an issue whose pod keeps its volume (keptVolume). Every other issue pod declares it as the
// template of its own ephemeral volume. So the disks are provisioned alike and differ only in who
// deletes them.
func (r *Runtime) diskSpec(l launch) corev1.PersistentVolumeClaimSpec {
	return claimSpec(r.storageClass, r.volumeSize, l.kind, l.microVM)
}

// claimSpec is the claim of the disk of a pod of kind: ReadWriteOnce at size
// (runtime.kubernetes.issue_volume), on the class microVMDiskClass gives where it names one, a
// microVM issue pod's (MicroVM.StorageClass), and on storageClass
// (runtime.kubernetes.storage_class) for every other pod, the controller's in either sandbox
// included. Whatever declares a pod's disk takes its class from here: a launch (diskSpec) and the
// boot census, which runs before any runtime is built (IssueDisks.templates).
func claimSpec(storageClass string, size resource.Quantity, kind podKind, microVM *MicroVM) corev1.PersistentVolumeClaimSpec {
	if class := microVMDiskClass(kind, microVM); class != "" {
		storageClass = class
	}
	return corev1.PersistentVolumeClaimSpec{
		AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		StorageClassName: &storageClass,
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: size},
		},
	}
}

// microVMDiskClass is the class the microVM block gives the disk of a pod of kind running in
// microVM (nil in gVisor): the block's own for an issue pod (MicroVM.StorageClass, which New
// refuses empty while an issue pod may run in a microVM), and "" for the controller's, whose disk
// stays on runtime.kubernetes.storage_class in either sandbox: the CRD holds its Sandbox's claim
// template immutable (replaceSupersededSandbox), so a class that followed the sandbox would take a
// new Sandbox, and a new volume, at every change of the project's type. The disk (claimSpec) and
// the shape that records it (podShape) both read it.
func microVMDiskClass(kind podKind, microVM *MicroVM) string {
	if _, issue := kind.(issuePod); issue && microVM != nil {
		return microVM.StorageClass
	}
	return ""
}

// sandboxManifest is the Sandbox a relaunch creates when none exists: Suspended, so no pod starts
// before the claim's Secret is written, with whatever volume its kind has the Sandbox own
// (podKind.disk). An issue's Sandbox owns none: its pod's disk is a generic ephemeral
// volume the pod itself owns, created with the pod and deleted with it (INFRA-80), so nothing of
// an issue survives the pod that did it. The one exception is an issue whose microVM pod keeps its
// volume (keptVolume): its Sandbox owns the one `issue` template, labelled kept, and the claim
// made from it outlives the issue's pods until the Sandbox goes. The controller's Sandbox still
// owns its one `issue` template, so the working root its launcher runs in outlives the pod that
// left it. Its pod template never runs: the relaunch's Running patch replaces it with the template
// the launch computes before the controller creates a pod. Its metadata is a name, a namespace and
// the launch's labels, and no annotation: the one a Sandbox is ever created carrying — the pod a
// replacing launch displaced — is set on the object by that launch (replaceSupersededSandbox).
func (r *Runtime) sandboxManifest(l launch) sandbox {
	templates, _ := l.kind.disk(r, l)
	return sandbox{
		TypeMeta:   metav1.TypeMeta{APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{Name: l.name, Namespace: r.namespace, Labels: r.labels(l)},
		Spec: sandboxSpec{
			PodTemplate: r.podTemplate(l), OperatingMode: modeSuspended,
			VolumeClaimTemplates: templates,
		},
	}
}

// podTemplate is the pod a launch runs (decisions 7 and 10). Every pod mounts one disk at the same
// volume name (issueVolume), from the source its kind gives it (podKind.disk): an issue pod's is
// its own ephemeral claim, made with the pod and deleted with it, or, where the pod keeps its
// volume (keptVolume), the kept claim its Sandbox owns and that outlives the pod; the
// controller's is the claim its Sandbox owns and that outlives every pod it makes. The pod runs as
// the operator's ServiceAccount (the namespace's default when the operator names none), and its
// launcher containers mount the providers Secret's configured keys and the operator's mounts, are
// told the operator's variables, and start Oh My Pi on the pod's baseline (`--pod-safety`,
// internal/podsafety). Its init containers and the volumes only they mount are its kind's
// (podKind). Beyond the Legion pool's selector and tolerations, and a microVM pod's ComputeClass
// (placement), it asks nothing of its placement: an ephemeral disk is made where the pod is
// scheduled, so the scheduler puts such a pod wherever the pool has room. A claim that outlives
// its pods binds each later pod to the zones of its volume. The controller applies a template only
// to the next pod it creates, so the template is rebuilt for every relaunch. Its metadata
// carries, beside the two stop-eviction annotations, the configuration the pod is built from
// (podShapeAnnotation, podShape), its kind's in the sandbox the launch runs it in
// (launch.shapeKey): the one write of it, so that a later sweep can read what a pod was built from
// without rebuilding the launch that built it (drift.go). A microVM pod's labels carry
// labelSandbox beside its kind's, on the template alone, so the Sandbox and its Secrets carry
// exactly the labels a gVisor pod's do.
func (r *Runtime) podTemplate(l launch) podTemplate {
	_, providersMounts := r.providers()
	runtimeClass, selector, tolerations := r.placement(l)
	spec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyAlways,
		TerminationGracePeriodSeconds: new(int64(math.Ceil(r.terminationGrace.Seconds()))),
		AutomountServiceAccountToken:  new(false),
		ServiceAccountName:            r.pod.ServiceAccount,
		EnableServiceLinks:            new(false),
		// OnRootMismatch: every file on the pod's disk is already the pod user's, so the kubelet skips
		// re-owning the whole volume at each mount; on a full volume that walk took about 35 s.
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: new(true), RunAsUser: new(int64(podUser)), RunAsGroup: new(int64(podUser)), FSGroup: new(int64(podUser)),
			FSGroupChangePolicy: new(corev1.FSGroupChangeOnRootMismatch),
		},
		RuntimeClassName:  new(runtimeClass),
		NodeSelector:      selector,
		Tolerations:       tolerations,
		PriorityClassName: r.scheduling.PriorityClass,
		Volumes:           r.volumes(l),
		InitContainers:    l.kind.initContainers(r, l),
		Containers:        []corev1.Container{r.launcherContainer(l, providersMounts)},
	}
	for i := range spec.InitContainers {
		kubeletLiteral(&spec.InitContainers[i])
	}
	for i := range spec.Containers {
		kubeletLiteral(&spec.Containers[i])
	}
	labels := r.labels(l)
	if l.microVM != nil {
		labels[labelSandbox] = string(SandboxMicroVM)
	}
	annotations := map[string]string{
		doNotDisruptAnnotation: "true", safeToEvictAnnotation: "false",
		podShapeAnnotation: r.podShapes[l.shapeKey()],
	}
	if r.leakCheck.Configured() {
		annotations[leakCheckerAnnotation] = r.leakCheck.ManifestSHA256
	}
	return podTemplate{
		Metadata: podMetadata{Labels: labels, Annotations: annotations},
		Spec:     spec,
	}
}

// launcherContainer is the pod's one container, its kind's (podKind.launcherContainer): an issue
// pod's `roles`, whose launcher runs every workflow role, or the controller's `controller`. It runs
// only `legion launcher`, told the Cloud SQL session database its proxies serve this pod's
// generations when the deployment keeps sessions there (sessionProxyFlags); the per-generation
// worker-shim argv and plain environment arrive in the launcher's start command, while values the
// kubelet must resolve (the downward API, the operator's secret refs) are set on the container.
func (r *Runtime) launcherContainer(l launch, providersMounts []corev1.VolumeMount) corev1.Container {
	resolved, _ := r.launchEnvironment(l)
	return l.kind.launcherContainer(r, l, resolved, providersMounts)
}

// kubeletLiteral escapes a container's command and env values against the kubelet's expansion,
// in which `$(NAME)` is another variable's value and `$$` a literal `$`: every `$` is doubled, so
// the process receives the text as written, an operator's own values included, as a tmux pane
// does. The system prompt never meets the kubelet at all: it rides the launcher's start command,
// with the worker's argv and plain environment, and reaches the agent as a file (systemPromptPath).
func kubeletLiteral(c *corev1.Container) {
	for i := range c.Command {
		c.Command[i] = kubeletEscape(c.Command[i])
	}
	for i := range c.Env {
		c.Env[i].Value = kubeletEscape(c.Env[i].Value)
	}
}

// kubeletEscape is text as a container's command or env value carries it: every `$` doubled.
func kubeletEscape(text string) string {
	return strings.ReplaceAll(text, "$", "$$")
}

// volumes are the pod's disk (issueVolume, from its kind's source: an issue pod's own ephemeral
// claim, which the kubelet creates with the pod and deletes with it, or the claim the controller's
// Sandbox owns) and jj config home with, between them, the volumes only its kind's init
// containers mount (podKind.initVolumes), then each launcher role's token projection, private
// credential directory, state and gh volume: each bundle directory's two gh files of its role
// Secret (GitHubHostsKey(dir) as <dir>/hosts.yml, GitHubConfigKey(dir) as <dir>/config.yml) for
// the launch's directories (gitHubDirs), and the bundle's configured renames beside them in the
// root (GitHubPairsKey as ghconfig.PairsFile, which `legion gh` and `legion git-credential` route
// a moved repository by), read-only to the pod's user, from the same Secret as the
// launcher token, so the refresher's one Update reaches the role's gh directory. A directory
// names one GitHub owner account, never one of its repositories, since a bundle holds no
// repository directory (ghconfig refuses one). A projection's items are fixed for the pod's
// life, so a pod holds the directories of its creation, and a pod created before the pairs file
// existed holds no item for it, which the drift check reads and replaces the pod for
// (movedGhDirs). A launch
// with no
// directory — a controller whose project has no GitHub owner to mint for, and the Sandbox's first
// template (sandboxManifest), which never runs — gets an empty directory instead, the pairs file
// included: a Secret
// projection with no items would project every key of the Secret, the launcher token among them.
// Then the volumes its kind's one container alone mounts (podKind.mainVolumes), the agent-secrets
// volumes, a microVM pod's workload identity token (workloadIdentityVolumes), the session writer
// token (sessionWriterVolumes), the providers and the operator's.
func (r *Runtime) volumes(l launch) []corev1.Volume {
	providers, _ := r.providers()
	memory := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}
	roleVolumes := make([]corev1.Volume, 0, len(l.roles)*4)
	ghItems := make([]corev1.KeyToPath, 0, 2*len(l.gitHubDirs)+1)
	for _, dir := range l.gitHubDirs {
		ghItems = append(ghItems,
			corev1.KeyToPath{Key: GitHubHostsKey(dir), Path: dir + "/" + ghconfig.HostsFile},
			corev1.KeyToPath{Key: GitHubConfigKey(dir), Path: dir + "/" + ghconfig.ConfigFile},
		)
	}
	if len(ghItems) > 0 {
		ghItems = append(ghItems, corev1.KeyToPath{Key: GitHubPairsKey, Path: ghconfig.PairsFile})
	}
	for _, role := range l.roles {
		roleVolumes = append(roleVolumes,
			corev1.Volume{Name: roleVolume("launcher", role), VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: roleSecretName(l.name, role), Items: []corev1.KeyToPath{{Key: LauncherTokenFile, Path: LauncherTokenFile}},
				DefaultMode: new(int32(0o440)),
			}}},
			corev1.Volume{Name: roleVolume("private", role), VolumeSource: memory},
			corev1.Volume{Name: roleVolume(stateVolume, role), VolumeSource: memory},
		)
		gh := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}
		if len(ghItems) > 0 {
			gh = corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  roleSecretName(l.name, role),
				Items:       slices.Clone(ghItems),
				DefaultMode: new(int32(0o440)),
			}}
		}
		roleVolumes = append(roleVolumes, corev1.Volume{Name: roleVolume(ghVolume, role), VolumeSource: gh})
	}
	_, disk := l.kind.disk(r, l)
	volume := corev1.Volume{Name: issueVolume, VolumeSource: disk}
	config := corev1.Volume{Name: configVolume, VolumeSource: memory}
	return slices.Concat([]corev1.Volume{volume}, l.kind.initVolumes(l), []corev1.Volume{config}, roleVolumes,
		l.kind.mainVolumes(r), r.agentSecretsVolumes(l.roles), workloadIdentityVolumes(l), r.sessionWriterVolumes(), providers, r.pod.Volumes)
}

// agentSecretsVolumes are, for a pod the runtime enrolls with the secrets broker
// (runtime.kubernetes.agent_secrets), one shared projected ServiceAccount token volume (the
// admission policy permits one volume per audience) and one private memory key directory per role
// of roles, the controller's included; none when the runtime enrolls no pod. Every role of every pod
// enrolls, so the broker can grant a pod's agents the secrets the Legion daemon's service owns and
// the shared ones, such as the controller's Slack bot token. Where the deployment names an exchange
// audience (AgentSecrets.ExchangeAudience) a second shared projected volume follows them, the pod's
// identity for the broker's exchange (exchangeTokenProjection). This builds the pod, so it reads
// r.agentSecrets whole, as its mounts do (agentSecretsMounts); what a generation is handed reads
// the enrollment and exchange decisions instead (enrollsIn, tradesIn), since the pod a launch
// keeps is never rebuilt.
func (r *Runtime) agentSecretsVolumes(roles []claim.Role) []corev1.Volume {
	broker := r.agentSecrets
	if broker == nil {
		return nil
	}
	volumes := []corev1.Volume{{Name: agentSecretsTokenVolume, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		DefaultMode: new(int32(0o440)),
		Sources:     []corev1.VolumeProjection{{ServiceAccountToken: brokerTokenProjection(broker)}},
	}}}}
	for _, role := range roles {
		volumes = append(volumes, corev1.Volume{Name: roleVolume(agentSecretsKeyVolume, role), VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(1<<20, resource.BinarySI)},
		}})
	}
	if broker.ExchangeAudience != "" {
		volumes = append(volumes, corev1.Volume{Name: agentSecretsExchangeVolume, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: new(int32(0o440)),
			Sources:     []corev1.VolumeProjection{{ServiceAccountToken: exchangeTokenProjection(broker)}},
		}}})
	}
	return volumes
}

// brokerTokenProjection is the ServiceAccount token a pod enrolled with broker projects for it: the
// configured audience and expiry, at AgentSecretsTokenFile.
func brokerTokenProjection(broker *AgentSecrets) *corev1.ServiceAccountTokenProjection {
	expiry := int64(math.Ceil(broker.TokenExpiry.Seconds()))
	return &corev1.ServiceAccountTokenProjection{Audience: broker.Audience, ExpirationSeconds: &expiry, Path: AgentSecretsTokenFile}
}

// workloadIdentityTokenSeconds is the lifetime of the tokens workloadIdentityVolumes and
// sessionWriterVolumes project: an hour, the cluster admission policy's cap on a projected token
// (the agent-secrets token's bound, configureProber). The kubelet writes a fresh one before it
// expires, and the launcher and its session proxy read the file at every exchange.
const workloadIdentityTokenSeconds = 3600

// workloadIdentityVolumes are, for a microVM launch whose block names the metadata endpoint
// (MicroVM.Metadata), an issue pod's or the controller's, the one projected ServiceAccount token
// the pod's launcher exchanges for a Google one at Google's security token service: for the
// Workload Identity pool's audience, which is the audience that exchange takes, at
// launcher.WorkloadIdentityTokenFile, readable by the pod's group, which is the fsGroup every file
// of the pod's volumes carries (podTemplate). Its one mount is the pod's one container's: an issue
// pod's under RolesDir, where no role's namespace shows it (issuePod.launcherContainer), and the
// controller's at ControllerWorkloadIdentityDir in the single-role form, where its agent can read
// it, and under RolesDir in the roles-dir form a writer audience gives it
// (controllerPod.launcherContainer). None for any other launch, a gVisor pod's included.
func workloadIdentityVolumes(l launch) []corev1.Volume {
	if l.microVM == nil || l.microVM.Metadata == nil {
		return nil
	}
	return []corev1.Volume{projectedTokenVolume(workloadIdentityTokenVolume, l.microVM.Metadata.WorkloadPool, launcher.WorkloadIdentityTokenFile)}
}

// projectedTokenVolume is a volume of one projected ServiceAccount token for audience, at path in
// the volume, for workloadIdentityTokenSeconds and readable by the pod's group, which is the
// fsGroup every file of the pod's volumes carries (podTemplate): the shape of each token the
// pod's launcher reads and no role does (workloadIdentityVolumes, sessionWriterVolumes).
func projectedTokenVolume(name, audience, path string) corev1.Volume {
	expiry := int64(workloadIdentityTokenSeconds)
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		DefaultMode: new(int32(0o440)),
		Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
			Audience: audience, ExpirationSeconds: &expiry, Path: path,
		}}},
	}}}
}

// sessionWriterAudience is the audience of the session writer token the pods of r carry
// (Database.WriterAudience): the session database's under cloudsql-iam, and empty under any other
// store and where that database names none. It is the one reading of whether a pod carries the
// token (sessionWriterVolumes, podTokenMounts), whether its launcher is told the audience
// (sessionProxyFlags), and whether the controller's pod runs the roles-dir form
// (controllerPod.launcherContainer), so the four never disagree.
func (r *Runtime) sessionWriterAudience() string {
	if r.sessionStore.Kind != ompsessions.StoreCloudSQLIAM {
		return ""
	}
	return r.sessionStore.Database.WriterAudience
}

// sessionWriterVolumes are, where the session database names a writer audience
// (sessionWriterAudience), the one projected ServiceAccount token the pod's launcher gives its
// session proxy. The proxy exchanges it at Google's security token service and impersonates the
// session database's service account with the federated token, never with the pod's own
// credentials. The token is for the writer audience, which is also the audience that exchange
// names, at launcher.SessionWriterTokenFile, readable by the pod's group, which is the fsGroup
// every file of the pod's volumes carries (podTemplate). Its one mount is the launcher
// container's, under RolesDir (podTokenMounts): an issue pod's, and the controller's, whose
// launcher then runs the roles-dir form (controllerPod.launcherContainer). None where the
// database names no writer audience, and none in the image probe's pod, which proxies no session.
func (r *Runtime) sessionWriterVolumes() []corev1.Volume {
	audience := r.sessionWriterAudience()
	if audience == "" {
		return nil
	}
	return []corev1.Volume{projectedTokenVolume(sessionWriterTokenVolume, audience, launcher.SessionWriterTokenFile)}
}

// exchangeTokenExpirySeconds is how long the kubelet mints a pod's exchange token for: an hour,
// the most the cluster's admission policy admits for a Legion worker token, which the kubelet
// rewrites once four fifths of it have passed.
const exchangeTokenExpirySeconds = 3600

// exchangeTokenProjection is the ServiceAccount token a pod enrolled with broker projects for the
// exchange's audience (AgentSecrets.ExchangeAudience), at AgentSecretsExchangeFile.
func exchangeTokenProjection(broker *AgentSecrets) *corev1.ServiceAccountTokenProjection {
	return &corev1.ServiceAccountTokenProjection{Audience: broker.ExchangeAudience, ExpirationSeconds: new(int64(exchangeTokenExpirySeconds)), Path: AgentSecretsExchangeFile}
}

// exchangeTokenMounts is a container's read-only mount of the exchange token at
// AgentSecretsExchangeDir where the pod carries one (agentSecretsVolumes), none otherwise.
func exchangeTokenMounts(broker *AgentSecrets) []corev1.VolumeMount {
	if broker == nil || broker.ExchangeAudience == "" {
		return nil
	}
	return []corev1.VolumeMount{{Name: agentSecretsExchangeVolume, MountPath: AgentSecretsExchangeDir, ReadOnly: true}}
}

// agentSecretsMounts are the controller's container's mounts of the agent-secrets volumes when it
// enrolls with broker and runs the single-role form, none when broker is nil: the one configured
// audience projection, the role's own key directory, and the exchange token where the deployment
// names its audience. A container that runs the roles-dir form mounts each role's key directory
// under the role's directory of RolesDir instead (rolesDirLayout), and the two tokens once for the
// pod (podTokenMounts).
func agentSecretsMounts(broker *AgentSecrets, role claim.Role) []corev1.VolumeMount {
	if broker == nil {
		return nil
	}
	return append([]corev1.VolumeMount{
		{Name: agentSecretsTokenVolume, MountPath: AgentSecretsTokenDir, ReadOnly: true},
		{Name: roleVolume(agentSecretsKeyVolume, role), MountPath: AgentSecretsKeyDir},
	}, exchangeTokenMounts(broker)...)
}

// podTokenMounts are the projected tokens a container that runs the launcher's roles-dir form
// mounts once for every role it runs, each read-only: the session writer token at
// SessionWriterDir where the session database names a writer audience (sessionWriterVolumes),
// under RolesDir, which no role's namespace shows; then, where the pod enrolls with the secrets
// broker, the broker's token at AgentSecretsTokenDir and the exchange's where the deployment names
// its audience (exchangeTokenMounts), which every role of the pod reads at those paths. No init
// container mounts any of them.
func (r *Runtime) podTokenMounts() []corev1.VolumeMount {
	var mounts []corev1.VolumeMount
	if r.sessionWriterAudience() != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: sessionWriterTokenVolume, MountPath: SessionWriterDir, ReadOnly: true})
	}
	if r.agentSecrets != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: agentSecretsTokenVolume, MountPath: AgentSecretsTokenDir, ReadOnly: true})
		mounts = append(mounts, exchangeTokenMounts(r.agentSecrets)...)
	}
	return mounts
}

// providers are the providers Secret's volume and its read-only mount at ProvidersDir: the
// configured keys alone, each a file named for the variable Oh My Pi reads, which is what the shim
// exports into Oh My Pi's environment (--provider-env-dir, shim.ReadProviderEnv); each providers
// secret (Options.ProvidersSecrets), a file of its own name that the shim skips, since the
// container's `<NAME>_FILE` points at it (providersPointers); and, under the postgres session
// store alone, that store's URL key at providersDSNFile, which the shim skips for the same reason
// (mainEnvironment's OMP_SESSION_SQL_DSN_FILE). None of them without any of the three, so a
// deployment with none needs no such Secret.
func (r *Runtime) providers() ([]corev1.Volume, []corev1.VolumeMount) {
	if !r.mountsProviders() {
		return nil, nil
	}
	var items []corev1.KeyToPath
	for _, variable := range sortedKeys(r.providerKeys) {
		items = append(items, corev1.KeyToPath{Key: r.providerKeys[variable], Path: variable})
	}
	for _, name := range r.providersSecrets {
		items = append(items, corev1.KeyToPath{Key: name, Path: name})
	}
	if key := r.sessionStore.DSNSecretKey(); key != "" {
		items = append(items, corev1.KeyToPath{Key: key, Path: ompsessions.DSNFileName})
	}
	return []corev1.Volume{{Name: providersVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: ProvidersSecretName(r.project), Items: items, DefaultMode: new(int32(0o440)),
		}}}},
		[]corev1.VolumeMount{{Name: providersVolume, MountPath: ProvidersDir, ReadOnly: true}}
}

// mountsProviders reports whether every pod mounts the providers Secret: some provider key,
// providers secret, or session database URL key is configured.
func (r *Runtime) mountsProviders() bool {
	return len(r.providerKeys) > 0 || len(r.providersSecrets) > 0 || r.sessionStore.DSNSecretKey() != ""
}

// providersKeys are the providers Secret's keys every pod mounts, sorted: what the image probe
// names when the kubelet cannot mount one.
func (r *Runtime) providersKeys() []string {
	keys := slices.Concat(slices.Collect(maps.Values(r.providerKeys)), r.providersSecrets)
	if key := r.sessionStore.DSNSecretKey(); key != "" {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// sessionDSNPointer is OMP_SESSION_SQL_DSN_FILE naming the mounted session database URL, none
// without one: in the image probe's container it is what keeps `legion probe-image`, which reads
// the providers directory as the shim does, from exporting the URL into Oh My Pi's environment.
// Under cloudsql-iam the probe's pod mounts no session key, so it is told no pointer either.
func (r *Runtime) sessionDSNPointer() []corev1.EnvVar {
	if r.sessionStore.DSNSecretKey() == "" {
		return nil
	}
	return []corev1.EnvVar{{Name: ompsessions.DSNFileVariable, Value: providersDSNFile}}
}

// sessionDSNFile is the file a generation of l reads its session database's URL from: the
// providers mount's projection of the Secret key under postgres, and, under cloudsql-iam, the file
// the generation's own launcher writes in its private directory beside the socket it proxies. The
// store is the one the pod that generation runs in was built under, which is the configuration's
// for a launch that builds the pod and the kept pod's own otherwise (launch.sessionStore).
func (r *Runtime) sessionDSNFile(l launch) string {
	if l.sessionStore(r) == ompsessions.StoreCloudSQLIAM {
		return generationDir(l.spec.Generation) + "/" + ompsessions.DSNFileName
	}
	return providersDSNFile
}

// sessionProxyFlags are what a pod's `legion launcher` is told of the Cloud SQL session database
// it proxies its generations' sessions to, none under any other store: the three of
// sessionProxyFlagNames, then sessionProxyAccountFlag only where the store names a service account
// to impersonate, so the login the pod signs in as is its own identity's, or that account's, and
// then sessionWriterAudienceFlag only where the store names a writer audience, so the launcher
// impersonates that account with the writer token rather than with the pod's own credentials.
// There is no address among them: the launcher binds the socket in the generation's own private
// directory, which it knows from --roles-dir and the role in a container that runs the roles-dir
// form, and from --private-dir in the controller's single-role form.
func (r *Runtime) sessionProxyFlags() []string {
	if r.sessionStore.Kind != ompsessions.StoreCloudSQLIAM {
		return nil
	}
	database := r.sessionStore.Database
	values := [len(sessionProxyFlagNames)]string{database.Instance, database.Name, database.User}
	flags := make([]string, 0, 2*len(values)+4)
	for i, value := range values {
		flags = append(flags, sessionProxyFlagNames[i], value)
	}
	if database.ImpersonateServiceAccount != "" {
		flags = append(flags, sessionProxyAccountFlag, database.ImpersonateServiceAccount)
	}
	if audience := r.sessionWriterAudience(); audience != "" {
		flags = append(flags, sessionWriterAudienceFlag, audience)
	}
	return flags
}

// providersPointers are the `<NAME>_FILE` pointer of each providers secret to its file in the
// providers mount, in the worker's container and the image probe's alike: set by the runtime for
// every pod that mounts the Secret, whatever a spec carries, so the shim never exports the secret.
func (r *Runtime) providersPointers() []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, name := range r.providersSecrets {
		env = append(env, corev1.EnvVar{Name: name + "_FILE", Value: ProvidersDir + "/" + name})
	}
	return env
}

// operatorEnv is the operator's variables for the agent's container, in name order.
func (r *Runtime) operatorEnv() []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, name := range sortedKeys(r.pod.Env) {
		env = append(env, corev1.EnvVar{Name: name, Value: r.pod.Env[name]})
	}
	return env
}

// settingsOverlays is Oh My Pi's list of settings overlays, which the operator's pod env may set
// and a role's own overlays extend (roleOperatorEnv).
const settingsOverlays = "PI_CONFIG_FILES"

// roleOperatorEnv is operatorEnv as the launch's role is told it: two names carry the operator's
// own value with this launch's share of it written in — settingsOverlays and
// runtime.SpendAttributesVariable — and every other entry is the operator's as the operator wrote
// it. The `composed` set is the subset of those two a launch spec is refused for naming: the
// spend attributes alone, since no spec has ever been refused for naming PI_CONFIG_FILES.
//
// PI_CONFIG_FILES carries the launch's own overlays (launch.overlays, the role's from
// Options.RoleConfigFiles and then those of the arms its unit was drawn into, from
// runtime.SpawnSpec.Variants) after the operator's, so each of them outranks the operator's, and
// is inserted in name order when the operator's pod env names none. A role with none is told
// operatorEnv unchanged.
//
// runtime.SpendAttributesVariable carries the launch's issue, its role and the arms it runs in
// after the operator's own attributes (runtime.SpendResourceAttributes), so a pod's resource
// attributes name the issue it serves and each arm whose files it runs with: the detector that
// reads the value assigns last-wins, which is why the daemon's pairs are written after the
// operator's rather than before. The arms are the ones launch.overlays carries, not every arm the
// unit was drawn into, so a launch that keeps a pod lacking an arm's overlays names no such arm.
// Every launch has a role, so this entry is in every role's environment, inserted in name order
// where the operator sets no attributes at all. It is the launch's own, not the pod's: it travels
// in the generation's start command (launchEnvironment), so no pod is replaced for it.
//
// The overlays are the launch's, not the configuration's, so a role started in a live pod the
// launch keeps is told only the files that pod holds (keptPod): Oh My Pi refuses to start on an
// entry that is not there, and a role of a drifted pod that never starts never answers the update
// round that is to replace that pod (INFRA-484). A role whose kept pod holds none of its overlays
// is told operatorEnv unchanged, exactly as a role with none configured is, and runs without them
// until the round replaces its pod.
func (r *Runtime) roleOperatorEnv(l launch) []corev1.EnvVar {
	overlays, carried := l.overlays(r)
	env := composeEntry(r.operatorEnv(), settingsOverlays, func(operator string) string {
		own := strings.Join(overlays, ":")
		switch {
		case own == "":
			return operator
		case operator == "":
			return own
		default:
			return operator + ":" + own
		}
	})
	spec := l.spec
	spec.Variants = carried
	return composeEntry(env, runtime.SpendAttributesVariable, func(operator string) string {
		return runtime.SpendResourceAttributes(operator, spec)
	})
}

// composeEntry is env, the operator's entries in name order, with name's value replaced by what
// compose makes of the operator's own value of it — "" where the operator sets none — and the
// entry inserted in name order where the operator has none, so one pair of that name reaches the
// launch's process and the entries stay sorted for the next search (slices.BinarySearchFunc). A
// composition that answers "" where the operator set nothing leaves env exactly as it is: a role
// that adds nothing to a name nobody set is told no empty entry invented for it.
func composeEntry(env []corev1.EnvVar, name string, compose func(operator string) string) []corev1.EnvVar {
	at, found := slices.BinarySearchFunc(env, name, func(v corev1.EnvVar, name string) int { return strings.Compare(v.Name, name) })
	operator := ""
	if found {
		operator = env[at].Value
	}
	switch value := compose(operator); {
	case found:
		env[at].Value = value
	case value != "":
		env = slices.Insert(env, at, corev1.EnvVar{Name: name, Value: value})
	}
	return env
}

// The XDG base directories, at the standard offsets from the image's HOME: what the issue pod's
// workspace-init is told (xdgEnvironment), the state home xdgStateHome itself there. A role is
// told none of the three home-directory ones: its cache and data homes are the issue's
// scratch's, shared with every other role of the pod, and its state home a directory per role under
// that scratch (roleEnvironment). The config home is the one path a role is told the same as
// workspace-init. There it is the pod's shared in-memory volume: jj keeps a repository's `--repo`
// configuration under $XDG_CONFIG_HOME/jj/repos/, so workspace-init writes Legion's settings of the
// issue's clone there (git.abandon-unreachable-commits false, no repository identity). In an issue
// pod's roles container that volume is under launcher.HomeMountsDir, which no role sees, and each
// role's role-init binds the role's own config home at the path instead, a copy of the shared one
// made once per pod (launcher.ConfigHomeDir): the role's jj reads Legion's settings, and what a
// role writes there — a git `core.fsmonitor`, a jj alias — never runs in another role's git or
// jj. The shared volume starts empty in every pod, and no role writes it, so nothing an agent wrote
// reaches workspace-init's jj.
const (
	xdgConfigHome = podHome + "/" + launcher.ConfigHomeDir
	xdgCacheHome  = podHome + "/.cache"
	xdgDataHome   = podHome + "/.local/share"
	xdgStateHome  = podHome + "/.local/state"
)

// xdgEnvironment is the four XDG base directories on the image's HOME: what the issue pod's
// workspace-init is told (initEnvironment), the one container of a pod told any of them besides
// what each role is told — workspace-fetch is told none (fetchEnvironment). A role is told none of
// them but the config home, at whose path it sees its own; its cache and data homes are the issue's
// scratch's and its state home a directory per role there (roleEnvironment).
func xdgEnvironment() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "XDG_CONFIG_HOME", Value: xdgConfigHome},
		{Name: "XDG_CACHE_HOME", Value: xdgCacheHome},
		{Name: "XDG_DATA_HOME", Value: xdgDataHome},
		{Name: "XDG_STATE_HOME", Value: xdgStateHome},
	}
}

// scratchRoot is the issue's scratch, on the issue's volume, which every launcher container
// mounts whole at TreeRoot: the four directories every role of the pod is told the same one of
// (roleEnvironment) and `state`, which holds one state home per role (roleStateHome).
const scratchRoot = TreeRoot + "/scratch"

// roleEnvironment is where a role's agent puts what it writes without being told where:
// its temporary directory, the three XDG homes a tool caches under, and the three tool caches that
// resolve to a path of their own (Go's build and module caches, bun's install cache). Every one of
// them is on the issue's volume, where neither the pod's ephemeral storage nor the pod's memory
// bounds it: a bun install cache alone is 1.4 GB for this repository, and charging it to the
// container's filesystem is what GKE Autopilot evicted issue pods for. All but the state home are
// the issue's, one directory every role of it is told the
// same: the temporary directory, the cache and data homes and bun's install cache directly under
// scratchRoot, and the two Go caches (goBuildCache, goModCache), as uv's cache already was
// (uvCacheDir). A directory per role is what this replaces, by the human's decision of
// 2026-10-20: the roles of an issue already share the pod, the issue's disk whole, the one
// workspace they all work in, uv's Pythons and cache and the two Go caches, and each can read and
// write what the others write, so a cache per role bought an isolation the pod does not have and
// cost a copy of every cache per role. Bun hardlinks out of its install cache into whatever it
// installs for (LEGION-198, where that hardlinking was first met), and the issue's roles share one
// checkout — workspace.Location names the repository and the issue, never a role — so a file
// edited in place inside node_modules is already in the packages every role runs from, whatever we
// do about caches. XDG_STATE_HOME alone stays the role's own (roleStateHome), and for a mechanism
// rather than caution: Oh My Pi names its browser broker's lock from the state root, the name is
// an abstract unix socket, and an abstract socket name is one name across every container of the
// pod, so one state home would block every role's broker but the first. Only the config home is
// neither the issue's scratch nor a directory of it: each role sees its own at xdgConfigHome,
// which role-init seeds from the pod's shared in-memory volume, where workspace-init writes the jj
// configuration the agents' jj reads. None of these directories outlives the pod: the issue's disk
// is the pod's own generic ephemeral volume, so a later pod of the issue downloads and builds
// again, and the sharing here is between the roles of one pod, not between pods.
func roleEnvironment(role claim.Role) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "XDG_CONFIG_HOME", Value: xdgConfigHome},
		{Name: "XDG_CACHE_HOME", Value: scratchRoot + "/cache"},
		{Name: "XDG_DATA_HOME", Value: scratchRoot + "/data"},
		{Name: "XDG_STATE_HOME", Value: roleStateHome(role)},
		{Name: "TMPDIR", Value: scratchRoot + "/tmp"},
		{Name: "GOCACHE", Value: goBuildCache},
		{Name: "GOMODCACHE", Value: goModCache},
		{Name: "BUN_INSTALL_CACHE_DIR", Value: scratchRoot + "/bun/install/cache"},
	}
}

// roleStateHome is the XDG_STATE_HOME a role's agent is told, `/legion/scratch/state/<role>`: the
// one directory of the issue's scratch that is one per role rather than one for the issue, in a pod
// whose roles share the network namespace, the workspace path and the rest of that
// scratch. Oh My Pi keeps its browser broker's lock under its state root (`run/daemons/<hash of
// the workspace's real path>/broker.pid`), taken as an abstract unix socket named from the lock
// path, and an abstract socket name is one name across every role of the pod, whose namespaces
// share the pod's network namespace; with one state root the first role's lock would block every
// other role's broker, and their `browser.open` would fail while `broker.sock` sits in the first
// role's own view of the filesystem. The per-role homes hang
// under one `state` directory so that the shared and the per-role halves of the scratch stay
// visibly apart and no role's name can collide with a shared directory's. The shim makes Oh My
// Pi's profile directory under it before Oh My Pi starts (podsafety.EnsureStateHome), since Oh My
// Pi reads the variable only where that directory exists. It is on the issue's volume, which is
// neither the pod's memory nor the pod's ephemeral storage, so Oh My Pi's logs, Chromium's
// `browser-profiles` and the plugin generation an installing launch puts there (plugins.go) are
// charged to neither — and, unlike the container's own filesystem, the state home outlives a
// restart of the `roles` container, though not the pod, whose own ephemeral volume the issue's
// disk is.
func roleStateHome(role claim.Role) string {
	return scratchRoot + "/state/" + string(role)
}

// claudeConfigDir is a Claude Code role's CLAUDE_CONFIG_DIR, `<role state home>/claude`: its
// transcripts, its plugin data and the rest of Claude Code's own state. It is the role's own
// directory of the issue's scratch (roleStateHome), so it is writable by the pod's uid and gid
// 1000 and is no other role's; like the rest of the pod's disk it lives and dies with the pod
// (INFRA-80). Nothing in the image makes it: the generation's start script does, before the agent
// execs (startScript).
func claudeConfigDir(role claim.Role) string {
	return roleStateHome(role) + "/claude"
}

// uv's settings in the worker container, which the pod's environment hands the image's uv.
const (
	// uvPythonRoot holds the Pythons uv installs, one directory per issue (uvPythonDir), and
	// uvCacheDir its cache, both on the issue's volume beside the workspaces. A project's .venv, in
	// an issue's workspace on that volume, links to an interpreter in that issue's directory, so
	// every role of the issue's pod finds the interpreter and the environment works there
	// as it is; the cache lets every role of the pod reuse what another downloaded. Neither outlives
	// the pod (roleEnvironment).
	uvPythonRoot = TreeRoot + "/uv/python"
	uvCacheDir   = TreeRoot + "/uv/cache"
	// uvLinkMode is how uv puts a package from uvCacheDir into a .venv: a copy. With the cache and
	// the .venv on one filesystem uv would otherwise hardlink them, and an edit made in place in one
	// workspace's .venv would change the cache and every other .venv of the tree that installed the
	// package, the shared-inode failure LEGION-198 hit with bun's cache.
	uvLinkMode = "copy"
)

// uvPythonDir is issue's own directory under uvPythonRoot, named by the issue as a DNS label
// (dnsName). uv serializes the installs into a directory with a file lock there, and a gVisor
// pod's lock reaches no other pod, so two pods first installing one Python into a shared directory
// at once could each delete the other's interpreter. One directory per issue keeps every other
// issue's pods out; only the pods of one issue share it, and they share one volume.
func uvPythonDir(issue string) string {
	return uvPythonRoot + "/" + dnsName(issue, maxNameLength)
}

// The go command's two caches, which roleEnvironment hands every role of the issue the same pair
// of.
const (
	// goBuildCache is the GOCACHE every role of an issue is told and goModCache its
	// GOMODCACHE, both on the issue's volume beside the workspaces and uv's cache, and both shared
	// by the issue's roles rather than split per role. Each holds content addressed by its hash,
	// and the roles of one issue may fill them at once: of the build cache `go help cache` says
	// "The cache is safe for concurrent invocations of the go command", and what reaches the
	// module cache is a module zip or go.mod the go command has verified against a known hash
	// (`go help module-auth`) in a directory it then makes read-only, which only `go build
	// -modcacherw` leaves writable. They are the issue's rather than the tree's because each issue
	// has a volume of its own; a pair is 751 MiB of build cache and 375 MiB of module cache for this
	// repository, so the six roles of an issue pod with a pair each would hold five redundant
	// copies, about 5.5 GiB of the issue's volume spent on nothing. They keep their own place at
	// `/legion/go`, beside the workspaces rather than under the issue's scratch (scratchRoot),
	// where they were the issue's one pair before the rest of the scratch became the issue's too.
	// The state home is the one directory of that scratch no two roles share: Oh My Pi's browser
	// broker takes its lock as an abstract unix socket named from a path under the state home, one
	// name across every container of the pod, so one state home would block every role's broker
	// but the first (roleStateHome).
	goBuildCache = TreeRoot + "/go/build"
	goModCache   = TreeRoot + "/go/mod"
)

// fetchEnvironment is `workspace-init fetch`'s: the image's PATH alone, its own TMPDIR, the mounted
// provisioning token, and, for an issue pod, its repository's cache — its address, which resolves
// only while the cache is Ready, so the fetch waits for it, and its bearer token's file, from the
// same Secret — which `fetch` streams a fresh feed from alone before it asks GitHub for the delta,
// and the default branch the daemon recorded for the issue (LEGION_DEFAULT_BRANCH), which the
// delta brings current beside the one GitHub names now, since `workspace-init provision` starts an
// issue with no branch there. It is a variable and not a flag so that an image from before it
// ignores it rather than refusing the command, and a daemon from before it leaves it unset, which
// the fetch reads as no recorded default. Its git reads no configuration but its own, so it is
// told no config home.
func fetchEnvironment(l launch) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PATH", Value: imagePath},
		{Name: "TMPDIR", Value: initTempDir},
		{Name: "LEGION_PROVISION_TOKEN_FILE", Value: ProvisionDir + "/" + provisionTokenKey},
		{Name: "LEGION_REPOSITORY_CACHE_URL", Value: l.cacheURL},
		{Name: repocache.CacheTokenFileEnv, Value: ProvisionDir + "/" + cacheTokenKey},
		{Name: "LEGION_DEFAULT_BRANCH", Value: l.spec.DefaultBranch},
	}
}

// initEnvironment is `workspace-init provision`'s contract (research runtime §2.3), the one
// provisioning every role of the issue pod shares. Its PATH is the image's alone, naming no
// directory on the issue's disk, so the git and jj it resolves from PATH are never ones an agent
// put there; it carries no tool-path variables, and it is never pointed at the provisioning token.
// It carries no role's session path either: a missing transcript must not prevent sibling
// launchers from starting, and no session lives on the disk at all (INFRA-80). It expects nothing
// of the disk. A pod's own ephemeral disk is created with the pod and empty, so its provision is
// the first. A kept volume (keptVolume) holds the clone and the workspace that the issue's
// earlier pods left, and its provision reuses what the volume holds (workspace.Provision). Nothing
// here sizes a wait on another pod: each issue pod provisions its own clone on the issue's own
// disk, which no other pod mounts at the same time, so no two provisions share a repository.
func (r *Runtime) initEnvironment(launch) []corev1.EnvVar {
	return append([]corev1.EnvVar{{Name: "PATH", Value: imagePath}}, xdgEnvironment()...)
}

// ProvisionBound satisfies runtime.Runtime: workspace.FetchBudget plus workspace.CloneTimeout plus
// workspace.CheckoutTimeout. Each term is the real bound of one step whose duration follows the
// repository's size, and the three run one after the other: workspace-fetch's whole budget (its
// wait for the repository cache, then one BucketFetchTimeout for its attempt on the cache's
// bucket, then one FetchTimeout for the verification of what the cache's /tar streamed when that
// attempt failed, then one of its own for the delta from GitHub), workspace-init's shared clone of
// the feed, whose size-following steps share one CloneTimeout, and the issue's own checkout, the
// one command that writes the whole working copy (`jj sparse reset`, workspace.Provision), under
// CheckoutTimeout. It is how much longer than the boot timeout a launch may wait for its pod's
// launcher, counted from when it first sees one of the pod's init containers running
// (awaitRoleLauncher). The fetch started no later than that, the shared clone after the fetch
// ended and the checkout after the clone, so the three end inside the bound, each failing on its
// own, naming why, when it runs out, while the commands between them take the moments they do.
// The stream from the repository cache is the one part of the fetch outside those bounds: it
// ends when it falls below the low-speed bound rather than on any clock
// (workspace.CacheStreamLowSpeedBytes), so a stream that keeps receiving runs on into the slack
// this bound holds over what its fetch actually waited and checked.
//
// The sum counts no term for the commands that keep CommandTimeout, and never has: workspace-
// init's dozen other commands, local work that waits on no other pod. What covers them is the
// boot timeout past the sum, so the sum is no worst case: when the fetch and the clone each run
// their whole bound, whatever those commands take comes out of the checkout's hour and that boot
// timeout. A test shortens the sum (provisionBound).
func (r *Runtime) ProvisionBound() time.Duration {
	return r.provisionBound
}

// LaunchBound satisfies runtime.Runtime: the longest one launch try may run once it holds its
// pod's launch turn (relaunch), the sum of a replacing try's waits and of its bounded calls at
// their full budgets (launchBound): the boot timeout three times — ensureSandbox's wait for a
// Sandbox being deleted, awaitPodGone, awaitNewPod — then awaitRoleLauncher's backstop, the
// longest start bound a pod of the runtime gets (startBound: the kept-volume bound where the
// microvm block keeps an issue pod's volume and that bound is longer, the boot timeout otherwise)
// plus the boot timeout, ProvisionBound and two recheck intervals (launcherBackstop), then the
// termination grace the start frame has past the launcher deadline, then relaunchCalls times
// apiTimeout for every client call, mint, store read and launcher frame between those waits. The
// wait for the launch turn itself (lockPod) is outside it: that is another launch's time, bounded
// by that launch's own try, and counting it here would end a try that had not yet begun. Generous
// by design — a try that runs every call to its budget is still legitimate, and the one thing the
// bound must never be is shorter than a provisioning pod's rightful wait; a hung try ends at the
// bound either way, charged to the launch budget, and the next try runs. A test shortens it
// (launchBound).
func (r *Runtime) LaunchBound() time.Duration {
	return r.launchBound
}

// mainEnvironment is the pane contract with a pod's values (decision 10): what the pod's kind tells
// its agents (podKind.agentEnv: a tree agent its tree, issue, workspace and the gh files its
// credential is read from, the controller LEGION_CONTROLLER=1), then the rest of the variables
// every tmux pane is told (runtime/tmux/spawn.go, panePairs), then the operator's
// (runtime.kubernetes.pod, with the two names this runtime composes carrying the launch's share
// of the operator's own value: the role's overlays after the operator's in PI_CONFIG_FILES, and
// the launch's issue and role after the operator's attributes in OTEL_RESOURCE_ATTRIBUTES, so
// every pod's attributes name the issue it serves — roleOperatorEnv), then the spec's own,
// then one `<NAME>_FILE` pointer per secret into the generation's private launcher directory,
// then one per providers secret into the providers
// mount, then OMP_SESSION_STORAGE=sql and OMP_SESSION_SQL_DSN_FILE naming the URL file of the
// session database every session is a row in (Options.SessionStore, sessionDSNFile) — of the pod
// the generation runs in, which is the configuration's store for a launch that builds the pod and
// the kept pod's own otherwise (launch.sessionStore). Oh My Pi refuses to start on an unreadable
// URL file, so a generation told a file its pod does not carry never starts, never registers and
// never answers the round that is to replace that pod (INFRA-484); a pod created before the store
// was configured carries no URL file at all, and its generation is told neither variable rather
// than one it cannot read, until the round replaces the pod (movedSessionStore, keptPod.store).
// None of them repeats another: the runtime refuses a spec naming one of its own (runtimeOwned),
// and the daemon an operator's variable naming one of the runtime's or a spec's. The temporary
// directory, XDG homes and tool caches are the issue pod's disk's (roleEnvironment) — the issue's
// scratch and its Go caches, which every role of it is told the same of, but the state home, one
// per role — carried to the role's shim by the start command (launcherCommand's Env). POD_UID is
// the pod's own incarnation, from the downward API. The secrets broker is told when the runtime
// enrolls pods. PI_SHELL_PREFIX puts the `legion` directory first on PATH alone: a pod's gh is the
// image's, so no directory of the issue's disk leads PATH. The GIT_CONFIG pairs are the GitHub
// credential of every git the role runs, in the issue's clone or outside it, and two settings that
// would run a program another role planted in the clone, the fsmonitor and the hooks, turned off
// (GitConfigEnv); other settings of the shared clone can still run one. A Claude Code role, and no
// other, is also told Claude Code's own three (claudelaunch.Environment, runsClaude) and the
// session its launcher keeps (claudelaunch.SessionVariable). An Oh My Pi role that resumes a
// recorded session, and no other launch, is told last that the session is its alone
// (runtime.SessionExclusiveVariable=1).
func (r *Runtime) mainEnvironment(l launch) []corev1.EnvVar {
	spec := l.spec
	env := l.kind.agentEnv(l)
	add := func(name, value string) { env = append(env, corev1.EnvVar{Name: name, Value: value}) }
	add("LEGION_ROLE", string(spec.Role))
	add("LEGION_GENERATION", strconv.FormatUint(spec.Generation, 10))
	add("LEGION_PROJECT", spec.Project)
	if r.daemonURL != "" {
		add("LEGION_DAEMON_URL", r.daemonURL)
	}
	add("LEGION_STATE_DIR", StateDir)
	if len(r.natsURLs) > 0 {
		add("ENVOY_NATS_URL", strings.Join(r.natsURLs, ","))
	}
	if r.envoyURL != "" {
		add("ENVOY_URL", r.envoyURL)
	}
	if r.dispatchURL != "" {
		add("DISPATCH_URL", r.dispatchURL)
	}
	legionDir := filepath.Dir(r.tools.Legion)
	add("PATH", podPath(legionDir))
	add("PI_SHELL_PREFIX", shellprefix.For(legionDir))
	add("GIT_TERMINAL_PROMPT", "0")
	env = append(env, GitConfigEnv()...)
	// The shared public leak check every write of this role runs first: its `legion gh` and its
	// pushes each resolve PUBLIC_LEAK_CHECK before anything else (leakguard's Resolve) and, told
	// the deployment's checker (leakguard.CheckerVar), refuse the write when they find no check.
	// The check is not in the image — it is published by tag from the repository the deployment
	// names, the image carries the pin, and the daemon reads the check at that pin and pushes it
	// to the pod's launcher, which installs it once for the pod before any role starts
	// (launchers.serveLeakCheck; each role's shim only reads that install) — so what is named here
	// is the installed copy, under the scratch the roles of this pod share. The path is the
	// installer's own constant, so the path a role is told and the path the install writes are
	// one string and cannot drift (TestEveryRoleIsToldTheLeakCheckItsPodInstalls). The digest is
	// the manifest digest of the check the deployment names (leakguard.Manifest), as the pod the
	// role starts in was built under (launch.leakChecker): a kept pod holds the install of its own
	// image's pin, so its role is told the digest that install has; a pod recording none is told
	// the setting's where its launcher holds no install, since the check pushed into it then is the
	// setting's, and none where it holds one from before (relaunch). It is the sign, to the shim
	// and the guard, that the deployment runs a check at all; `legion gh` trusts its read rule
	// under the program that rule was proven against, whose digest it compiles in, and not under
	// this digest. A deployment that names none (Options.LeakCheckSetting) tells neither: its pods
	// are pushed no check, and its roles start without one and refuse no write for its absence.
	if r.leakCheck.Configured() {
		add(leakguard.CheckVar, leakguard.InstalledCheck)
		if checker := l.leakChecker(r); checker != "" {
			add(leakguard.CheckerVar, checker)
		}
	}
	// The two variables an agent reads to decide it holds a broker at all, gated on the same one
	// decision as the shim's agent-secrets flags (enrollsIn, launcherCommand): a generation in a
	// kept pod that carries no enrollment is started with neither the flags nor these, so its
	// role reports a resource it cannot reach rather than a secrets grant it never had.
	if l.enrolls(r) {
		add("AGENT_SECRETS_URL", r.agentSecrets.URL)
		add("AGENT_SECRETS_KEY_DIR", AgentSecretsKeyDir)
	}
	// The identity the broker's exchange trades, gated on the one exchange decision (tradesIn) as
	// the two above are on the enrollment: the pod's projected token for the exchange's audience,
	// which the broker clients of every session in the role (packages/cipher-client's
	// environment.ts, machineIdentity) trade for a broker token of the session's own.
	if l.trades(r) {
		add(machineIdentityVariable, kubernetesIdentity)
		add(machineTokenFileVariable, AgentSecretsExchangeDir+"/"+AgentSecretsExchangeFile)
	}
	// Under dispatch_token_broker, the word that this role's Dispatch credential is the token that
	// identity trades for, and nothing else (brokersDispatch): its launch carries no Dispatch
	// token, so it is told no DISPATCH_TOKEN_FILE either (the pointers below follow l.secrets).
	if l.brokersDispatch(r) {
		add(shimwire.DispatchTokenBrokerVariable, "1")
	}
	// The pod's own test database, when the deployment serves one (runtime.kubernetes.test_postgres):
	// an issue pod's roles alone, whose launcher serves it before it accepts a role start
	// (launcher's testpg.go). The controller's pod runs no tests and serves none. The name is the
	// runtime's (podVariables), so no operator's pod env can point it elsewhere. The value is the
	// database every image's server serves (testPGStartDSN), which the pod's own launcher replaces
	// with the DSN it serves when it serves another.
	if _, issue := l.kind.(issuePod); issue && r.testPostgres {
		add(launcher.TestPGVariable, testPGStartDSN)
	}
	if spec.Role == claim.RoleController {
		add(slackChannelsVariable, r.slackChannelsValue())
	}
	if l.runsClaude() {
		// Claude Code's own two (claudelaunch.Environment): where its state lives, and its
		// scheduled prompts off, since a prompt of the clock's is a turn no task of the daemon's
		// asked for. Its background tasks stay on: a task that finishes wakes Claude Code into a
		// turn of its own, which the bridge reports to the daemon as a turn (internal/claudebridge),
		// so the result is read in a turn the daemon sees rather than after one it thinks has
		// ended. Both names are the runtime's (runtimeOwned), so neither a spec nor an operator's
		// pod.env may place them.
		for _, pair := range claudelaunch.Environment(claudeConfigDir(spec.Role)) {
			name, value, _ := strings.Cut(pair, "=")
			add(name, value)
		}
		// The session this generation's agent command starts or resumes, which tells the role's
		// launcher to keep its transcript and phase-reminder record in the session table
		// (claudelaunch.SessionVariable): the pod's disk dies with the pod, so those files would
		// otherwise die with it and the next pod's `--resume` would find nothing. It is a variable of
		// the start command, never a field of shimwire.LauncherStart, and the image's launcher reads
		// it: the contract number (api.DaemonAPIVersion) and the image probe hold the image to this
		// daemon, so no launcher that predates it runs this launch. It is plain, so it travels in the
		// start command alone (launchEnvironment) and changes nothing of the pod.
		add(claudelaunch.SessionVariable, l.claude.session)
	}
	env = append(env, roleEnvironment(spec.Role)...)
	env = append(env, corev1.EnvVar{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"},
	}})
	env = append(env, r.roleOperatorEnv(l)...)
	for _, name := range sortedKeys(spec.Env) {
		add(name, spec.Env[name])
	}
	// The launcher writes each launch credential into its private directory before the child
	// starts (shimwire.LauncherStart.Files): no kubelet Secret propagation stands in the way of a
	// role started in a running pod.
	for _, name := range sortedKeys(l.secrets) {
		add(name+"_FILE", generationDir(l.spec.Generation)+"/"+name)
	}
	env = append(env, r.providersPointers()...)
	// Oh My Pi's own two variables, which place the session in the database the generation's URL
	// file names (sessionDSNFile): every pod this runtime builds gets them, since the store must
	// keep sessions in a database and nothing in a pod may keep a session as a file
	// (internal/podsafety's baseline covers only a container the runtime tells neither, the image
	// probe's). A Claude role is told them too: Claude Code ignores them, and its launcher reaches
	// the table through them to keep the Claude session's files (claudelaunch.SessionVariable).
	//
	// They are gated on the store of the pod the generation runs in, as the broker's two are on
	// that pod's enrollment (launch.sessionStore, l.enrolls): a kept pod created before any store
	// was configured projects no URL file, and a role told one it cannot read would refuse to
	// start, never register and never answer the round that is to replace that pod — so it is told
	// neither and keeps its session as a file on the pod's disk until the round replaces the pod
	// (INFRA-484, keptPod.store).
	if store := l.sessionStore(r); store.InDatabase() {
		add(ompsessions.StorageVariable, ompsessions.SQLStorage)
		add(ompsessions.DSNFileVariable, r.sessionDSNFile(l))
	}
	// A launch that resumes a recorded Oh My Pi session holds its Oh My Pi to that session
	// (runtime.SessionExclusiveVariable): a resume of a session another live process still holds
	// exits non-zero after the lease wait rather than moving the role onto a session id the daemon
	// never recorded. A resume of a session named by its id alone (spec.ResumeSession, the file the
	// launcher finds or Oh My Pi's own lookup) resumes a recorded session too, so it is held the
	// same way. A fresh launch is told nothing of it, and neither is a Claude role, whose resume is
	// Claude Code's. The name is the runtime's (runtimeOwned), so neither a spec nor an operator's
	// pod env may set it, and it comes after both. It is plain, so it travels in the start command
	// alone (launchEnvironment) and changes nothing of the pod.
	if !l.runsClaude() && (spec.ResumeSessionFile != "" || spec.ResumeSession != "") {
		add(runtime.SessionExclusiveVariable, runtime.SessionExclusiveValue)
	}
	return env
}

// gitConfigPairs are the git configuration entries GitConfigEnv sets: the helper for
// https://github.com reset, then set to `legion git-credential` (workspace.GitHubCredentialHelper),
// and credential.useHttpPath on for github.com, so git hands the helper the path whose owner picks
// the token; then core.fsmonitor off and core.hooksPath at /dev/null, so no git a role runs starts
// the fsmonitor program or a hook any configuration file names. The issue's roles share the
// clone, and each can write its `.git/config` and hooks, so without these a `git status` or `git
// commit` of one role would run a program another planted, with the first role's credentials.
// These are two of the settings that can do so, not all: a diff driver, a filter driver or a
// signing program a role plants in the shared clone still runs in a sibling's git, and the roles
// are no boundary against each other's code (docs/kubernetes.md). Last come the four pack
// settings of workspace.PackWindowConfig, so every git a role runs, the git jj runs for a push or a
// fetch among them, maps packs in small windows whatever image the deployment runs; a role's own
// `-c` still overrides one for its command.
var gitConfigPairs = append([][2]string{
	{"credential.https://github.com.helper", ""},
	{"credential.https://github.com.helper", workspace.GitHubCredentialHelper},
	{"credential.https://github.com.useHttpPath", "true"},
	{"core.fsmonitor", ""},
	{"core.hooksPath", "/dev/null"},
}, workspace.PackWindowConfig()...)

// GitConfigPairs is gitConfigPairs, the git settings every process of a pod gets, for `legion
// gh`, which hands the git gh runs a caller's GIT_CONFIG pair only where it is one of these, key
// and value alike (cmd/legion's ghChildGitConfig).
func GitConfigPairs() [][2]string { return slices.Clone(gitConfigPairs) }

// GitConfigEnv is gitConfigPairs as GIT_CONFIG_COUNT, GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n>,
// which git reads after every configuration file, the repository's own `.git/config` among them,
// as it reads `-c`. Every git a role process runs then answers https://github.com from the role's
// gh files, in the issue's clone or outside it: a clone the setup command makes
// (runtime.kubernetes.setup: Oh My Pi's marketplace clone among them), or a repository an agent
// clones for itself. The reset drops every helper a configuration file names for github.com, the
// image's or a repository's, and the helper it sets is the one the issue's clone already names, so
// git in the clone asks what it asked before. Any other host is left to the configuration files.
// `legion git-credential` answers an owner of the project from that owner's directory of
// LEGION_GH_OWNERS_DIR (ghAgentEnv), and a fork of a repository of such an owner, whose own owner
// has no directory, from the directory of the fork's parent or else its source, which it reads
// from GitHub with GH_CONFIG_DIR's token; `legion gh` adds its own entry after these
// (withGitHubUseHTTPPath, cmd/legion/gh.go). Exported for rolegitconfig, which prints it for a Job
// that runs no daemon.
func GitConfigEnv() []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: "GIT_CONFIG_COUNT", Value: strconv.Itoa(len(gitConfigPairs))}}
	for i, pair := range gitConfigPairs {
		env = append(env,
			corev1.EnvVar{Name: "GIT_CONFIG_KEY_" + strconv.Itoa(i), Value: pair[0]},
			corev1.EnvVar{Name: "GIT_CONFIG_VALUE_" + strconv.Itoa(i), Value: pair[1]})
	}
	return env
}

// launchEnvironment is mainEnvironment split into its two carriers: the variables the kubelet
// resolves (the downward API, the operator's Secret references), set once on the launcher's
// container, and the plain NAME=value pairs the launcher's start command carries to each
// generation's child.
func (r *Runtime) launchEnvironment(l launch) ([]corev1.EnvVar, []string) {
	var resolved []corev1.EnvVar
	var plain []string
	for _, entry := range r.mainEnvironment(l) {
		if entry.ValueFrom != nil {
			resolved = append(resolved, entry)
		} else {
			plain = append(plain, entry.Name+"="+entry.Value)
		}
	}
	return resolved, plain
}

// connectFlag is the flag naming the worker stream listener a role's launcher and its worker-shim
// dial.
const connectFlag = "--connect"

// handedAddress is one address the runtime hands every role process it launches from the daemon's
// configuration: name is where the process carries it, the launcher's connectFlag or one of
// mainEnvironment's variables, and value is what a process launched now carries there, "" for a
// variable the runtime leaves unset.
type handedAddress struct{ name, value string }

// handedAddresses are every address a role process launched now carries, whatever its role: the
// worker stream listener its launcher dials (podKind.launcherContainer, fixed when the pod is created),
// then the daemon's API, NATS, Envoy, Dispatch and the secrets broker as the agent's environment
// names them (mainEnvironment, carried by each generation's start command, launcherCommand), the
// broker only where enrolled, the one enrollment decision that generation's environment and argv
// are built from (enrollsIn), and unset otherwise — so the record of a generation in a pod that
// carries no enrollment says what it was handed, which is no broker at all. The stream is fixed
// for the pod's life, so a pod a daemon created under another stream dials it until the pod is
// replaced; the other five are fixed for the generation's life, and the Sandbox records them per
// role (recordAddresses) so a daemon restarted under other addresses can tell (evaluate).
// TestHandedAddressesAreEveryAddressAPodCarries keeps this list equal to what those two build.
func (r *Runtime) handedAddresses(enrolled bool) []handedAddress {
	brokerURL := ""
	if enrolled {
		brokerURL = r.agentSecrets.URL
	}
	return []handedAddress{
		{connectFlag, r.streamURL},
		{"LEGION_DAEMON_URL", r.daemonURL},
		{"ENVOY_NATS_URL", strings.Join(r.natsURLs, ",")},
		{"ENVOY_URL", r.envoyURL},
		{"DISPATCH_URL", r.dispatchURL},
		{"AGENT_SECRETS_URL", brokerURL},
	}
}

// podPath is a main container's PATH: the directory of the `legion` every container runs, then the
// image's PATH, which a container's env PATH replaces, with that directory once — in the worker
// image it is the image PATH's first entry. No directory of the tree volume: the gh, git and jj an
// agent runs are the image's, and its credential is the gh files under GH_CONFIG_DIR.
func podPath(legionDir string) string {
	entries := []string{legionDir}
	for _, entry := range strings.Split(imagePath, ":") {
		if entry != legionDir {
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, ":")
}

// nodeSelector is the Legion pool's label and the configured selector, which configure keeps off
// the pool's key.
func (r *Runtime) nodeSelector() map[string]string {
	selector := map[string]string{poolKey: poolValue}
	for key, value := range r.scheduling.NodeSelector {
		selector[key] = value
	}
	return selector
}

// tolerations are the Legion pool's taint, tolerated, then the configured ones.
func (r *Runtime) tolerations() []corev1.Toleration {
	return append([]corev1.Toleration{{
		Key: poolKey, Operator: corev1.TolerationOpEqual, Value: poolValue, Effect: corev1.TaintEffectNoSchedule,
	}}, r.scheduling.Tolerations...)
}

// placement is where a launch's pod runs: its RuntimeClass, node selector and tolerations. A
// gVisor pod's are every other pod's (the repository cache pods' and the image probe's, which run
// in gVisor whatever the sandbox block says): gVisor, on the Legion pool (nodeSelector,
// tolerations). A microVM pod (launch.microVM), an issue's or the controller's, runs under
// microVMRuntimeClass on the Legion pool's nodes of its ComputeClass: its selector is the gVisor
// pod's with computeClassKey set to that class, in place of any class the operator's selector
// names, and its tolerations are the gVisor pod's, less any the operator gave of computeClassKey,
// which tolerates the class being replaced, then the taint GKE puts on that class's nodes. The
// RuntimeClass adds the sandbox's own selector and toleration at admission.
func (r *Runtime) placement(l launch) (runtimeClass string, selector map[string]string, tolerations []corev1.Toleration) {
	selector, tolerations = r.nodeSelector(), r.tolerations()
	if l.microVM == nil {
		return gvisor, selector, tolerations
	}
	class := l.microVM.ComputeClass
	selector[computeClassKey] = class
	tolerations = slices.DeleteFunc(tolerations, func(t corev1.Toleration) bool { return t.Key == computeClassKey })
	tolerations = append(tolerations, corev1.Toleration{
		Key: computeClassKey, Operator: corev1.TolerationOpEqual, Value: class, Effect: corev1.TaintEffectNoSchedule,
	})
	return microVMRuntimeClass, selector, tolerations
}

// restrictedContainer is the Pod Security "restricted" container context
// (RESTRICTED_CONTAINER_SECURITY_CONTEXT, k8s-manifests.ts).
func restrictedContainer() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
