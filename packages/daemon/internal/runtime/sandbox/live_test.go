// The Stage 4a live test (LEGION-208 Stage 4 plan, Task 4.7): this package's runtime driven on the
// kind rig of internal/kindtest — a kind cluster with Agent Sandbox's controller and gVisor, and
// the fake GitHub of internal/fakegithub, which the cluster's pods reach as github.com and
// api.github.com — through a restricted identity of the cluster's own, the legion-daemon
// ServiceAccount under the grants docs/kubernetes.md ("RBAC the Go daemon needs") gives the
// daemon (liveIdentity), with the rig's admin kubeconfig only for what an operator does beside it:
// exec, PVC reads, a ConfigMap's patch. The kind job of ci.yaml runs it
// (.github/workflows/ci-job-kind.yaml), one group of its checks in each of four parts, and
// newKindLiveEnv sets up everything it runs on.
//
// The checks run in groups (liveGroups), each group a run of its own on a kind rig of its own, so a
// part of the kind job runs one group by -run alone ('^TestStage4aSandboxRuntimeLive$/^<group>$').
// Each check runs as a subtest of its group's, by its own name, prints what it observed, each line
// naming the identity that observed it, and then `CHECK <name>: PASS`. The first check of a group
// that does not hold prints `CHECK <name>: FAIL: <why>` and ends its group, since a later check of
// the group starts from the state the earlier ones left; no group starts from another's, and a
// group redoes first what its checks need of the state a check of another group leaves
// (liveGroup.setup). The agent every pod runs is a stub (decision 7): the Go shim bridges it as
// it would Oh My Pi, and it appends its own role, pod uid and generation to a marker file on the
// issue pod's own disk, and sleeps. That disk is the pod's own ephemeral volume and dies with it,
// so the marker proves a role's identity within one pod and nothing across pods. Within the pod it is
// shared: SandboxName (see names.go) is one name for all six of an issue's role claims, so one issue
// pod's resident roles each append their own launches to the same file, and a check reads only its
// own claim's lines back out of it (roleIncarnations); a pod replacement leaves a fresh, empty one.
// Continuity across a pod replacement is the session table's instead. Every runtime of the run keeps
// sessions in the rig's PostgreSQL in the cluster (session_store postgres, liveSessionsSecretKey),
// the harness records each claim's session there as `legion sessions import` copies one
// (recordSession), and every resume names that recorded path, which the role launcher looks up in the
// table — never on the disk — before any child runs.
//
// This file is the rig: the run's setup, the boot-token registry, the observation and log
// records, the runtime's lifecycle, and the launches and reads every check shares. The checks
// live beside it: live_install_test.go (the image probe), live_lifecycle_test.go (the claims'
// lifecycle), live_setup_test.go (runtime.kubernetes.setup), live_secrets_test.go (agent-secrets,
// against this repository's own broker), live_cache_test.go, live_pod_update_test.go,
// live_displaced_test.go, live_operator_test.go, live_sessions_test.go, and live_app_test.go (the
// implement App the provisioning token comes from).
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sjawhar/envoy/brokertest"
	"github.com/sjawhar/envoy/testprereq"
	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fakegithub"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/kindtest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// liveCheck is one check of the harness: the name it prints, what it runs, and — when non-nil —
// whether this run's setup supports it at all (SKIPPED-BLOCKED, a failure under GitHub Actions).
type liveCheck struct {
	name    string
	run     func(*liveRig) error
	blocked func(*liveRig) string
}

// liveGroup is a group of the harness's checks, a run of its own on a kind rig of its own: its
// name, which the subtest that runs it carries and a part of the kind job selects with -run; setup,
// what the group redoes first of the state a check of another group leaves its checks, or nil
// where they need none; and its checks, in the order they run.
type liveGroup struct {
	name   string
	setup  func(*liveRig) error
	checks []liveCheck
}

// liveGroups are the harness's checks, in groups that each stay within a part of the kind job
// (.github/scripts/ci_job_reads.py's KIND_PARTS names the part that runs each). A check that reads
// state an earlier check left, and puts back no claim of it itself, is in that check's group:
// secrets-automatic-grant, secrets-cross-pod-negative, secrets-copied-token-negative and
// secrets-self-enroll-negative start from the root and the worker secrets-two-roles-enrolled runs
// and enrolls, and secrets-cross-pod-negative from the grant and request secrets-automatic-grant
// records; stale-incarnation reads kill-role-init's kill and relaunch; and repository-cache,
// cache-second-pod and cache-lost-volume read the runtime root-ready starts and the cache its
// launch makes. Every other check puts each claim it starts from where it needs it itself
// (ensureRunning, ensureSuspended), or launches one no check before it in its group launched. A
// group that does not hold root-ready redoes first what root-ready leaves every later launch on
// the run's repository (mapRunRepository), and roles, whose checks start from the run's first
// issue, what root-ready and worker-shares-issue-pod leave them too (runFirstIssue).
var liveGroups = []liveGroup{
	// root: the run's first issue pod — its launch, which makes the run's repository's cache, the
	// pod and the worker beside it, their enrollments with the broker, another issue's enrollment
	// across a replacement of its pod — and the cache.
	{name: "root", checks: []liveCheck{
		{"root-ready", (*liveRig).checkRootReady, nil},
		{"gvisor", (*liveRig).checkGVisor, nil},
		{"resources", (*liveRig).checkResources, nil},
		{"operator-token", (*liveRig).checkOperatorToken, nil},
		{"pod-baseline", (*liveRig).checkPodBaseline, nil},
		{"setup-ran", (*liveRig).checkSetupRan, nil},
		{"setup-negative", (*liveRig).checkSetupNegative, nil},
		{"adopt-working-copy", (*liveRig).checkAdoptWorkingCopy, nil},
		{"worker-shares-issue-pod", (*liveRig).checkWorkerSharesIssuePod, nil},
		{"secrets-two-roles-enrolled", (*liveRig).checkSecretsTwoRolesEnrolled, secretsBlocked},
		{"secrets-automatic-grant", (*liveRig).checkSecretsAutomaticGrant, secretsBlocked},
		{"secrets-cross-pod-negative", (*liveRig).checkSecretsCrossPodNegative, secretsBlocked},
		{"secrets-copied-token-negative", (*liveRig).checkSecretsCopiedTokenNegative, secretsBlocked},
		{"secrets-self-enroll-negative", (*liveRig).checkSecretsSelfEnrollNegative, secretsBlocked},
		{"secrets-approval-ask", (*liveRig).checkSecretsApprovalAsk, secretsBlocked},
		{"secrets-old-uid-and-revocation", (*liveRig).checkSecretsOldUIDAndRevocation, secretsBlocked},
		{"repository-cache", (*liveRig).checkRepositoryCache, nil},
		{"cache-second-pod", (*liveRig).checkCacheSecondPod, nil},
		{"cache-lost-volume", (*liveRig).checkCacheLostVolume, nil},
	}},
	// roles: the roles of that issue's pod — a stop, isolation, a resume, a plugin install, a
	// refused resume, a kill and its stale incarnation, a respawn before a hello — a second tree's
	// two issues, the re-adoption of every live claim by a fresh runtime, the orphan sweep and the
	// issue's cleanup.
	{name: "roles", setup: (*liveRig).runFirstIssue, checks: []liveCheck{
		{"suspend", (*liveRig).checkSuspend, nil},
		{"role-isolation", (*liveRig).checkRoleIsolation, nil},
		{"resume", (*liveRig).checkResume, nil},
		{"stale-plugin-install", (*liveRig).checkStalePluginInstall, nil},
		{"same-agent-negative", (*liveRig).checkSameAgentNegative, nil},
		{"kill-role-init", (*liveRig).checkKillRoleInit, nil},
		{"stale-incarnation", (*liveRig).checkStaleIncarnation, nil},
		{"respawn-before-register", (*liveRig).checkRespawnBeforeRegister, nil},
		{"independent-provision", (*liveRig).checkIndependentProvision, nil},
		{"re-adopt", (*liveRig).checkReAdopt, nil},
		{"orphan-sweep", (*liveRig).checkOrphanSweep, nil},
		{"cleanup-reaps-the-issue", (*liveRig).checkCleanupReapsTheIssue, nil},
	}},
	// image: the worker image's probe and its three refusals.
	{name: "image", setup: (*liveRig).mapRunRepository, checks: []liveCheck{
		{"image-probe", (*liveRig).checkImageProbe, nil},
		{"image-probe-negative", (*liveRig).checkImageProbeRefusal, nil},
		{"image-probe-capability-negative", (*liveRig).checkImageProbeCapabilityRefusal, nil},
		{"image-probe-missing-volume-negative", (*liveRig).checkImageProbeMissingVolumeRefusal, nil},
	}},
	// issues: issues of their own, a check each: one on another owner's repository, a role its
	// pod's replacement displaces, a resume from the session table alone, and an issue closed and
	// resumed. The image's checks were this group's until the two together ran past the part
	// budget (1,463 s and 1,520 s of 1,461 in CI runs 44766893801 and 44744458379).
	{name: "issues", setup: (*liveRig).mapRunRepository, checks: []liveCheck{
		{"second-owner", (*liveRig).checkSecondOwner, nil},
		{"displaced", (*liveRig).checkDisplaced, nil},
		{"postgres-resume", (*liveRig).checkPostgresResume, nil},
		{"issue-suspend-resume", (*liveRig).checkIssueSuspendResume, nil},
	}},
	// pod-update: the update round over one drifted issue pod, and over two issues' pods a change
	// drifts at once.
	{name: "pod-update", setup: (*liveRig).mapRunRepository, checks: []liveCheck{
		{"pod-update", (*liveRig).checkPodUpdate, nil},
		{"pod-update-fleet", (*liveRig).checkPodUpdateFleet, nil},
	}},
}

// The runtime's settings for the run: the boot timeout covers the worker image's pull from the
// rig's registry, which is what a first pod on the node waits for.
const (
	liveBootTimeout   = 5 * time.Minute
	liveGrace         = 15 * time.Second
	liveProbeInterval = 10 * time.Second
	liveAdoptTimeout  = time.Minute
	// liveRunningLimit bounds a launch's wait for its pod to run and its Sandbox to be Ready.
	liveRunningLimit = 15 * time.Minute
	// liveGoneLimit bounds a wait for a pod or an object to be deleted.
	liveGoneLimit = 5 * time.Minute
	// liveSettle is how long a check keeps listening for observations it must not see.
	liveSettle = 2*liveProbeInterval + 5*time.Second
)

// liveIssueVolume is the issue volume's size, the daemon configuration's default, which kind's
// local-path StorageClass provisions on the node's disk. The run sets no scheduling beyond the
// Legion pool the runtime selects: no pod asks for another pod's node or keeps off one (every issue
// pod owns its volume, LEGION-632), and every pod reserves what liveResources gives its roles.
var liveIssueVolume = resource.MustParse("20Gi")

// liveRepositoryCache is the daemon's default repository cache (config.DefaultRepositoryCache), as
// sandboxOptions translates it, with its pod reserving a fifth of a CPU and 768Mi, requested in
// full, rather than the daemon's one CPU and 4Gi: the kind job's node, one runner of four CPUs
// and 16Gi, holds both of the run's caches beside its issue pods and the cluster's own.
func liveRepositoryCache() RepositoryCache {
	return RepositoryCache{
		Volume: resource.MustParse("20Gi"), Refresh: 10 * time.Minute, Retain: 168 * time.Hour,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("768Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("768Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("2Gi")},
		},
	}
}

// mappedRepositories is the run's mapping as the runtime reads it.
func (r *liveRig) mappedRepositories() []ghrepo.Repository {
	r.mappedMu.Lock()
	defer r.mappedMu.Unlock()
	return slices.Clone(r.mapped)
}

// mapRepository adds repo to the run's mapping, as Dispatch's mapping names a repository before
// any issue of it launches.
func (r *liveRig) mapRepository(repo ghrepo.Repository) {
	r.mappedMu.Lock()
	defer r.mappedMu.Unlock()
	if !slices.Contains(r.mapped, repo) {
		r.mapped = append(r.mapped, repo)
	}
}

// liveIssueOverride is the issue pod's reservation the rig sets itself, as a deployment's
// `runtime.kubernetes.resources.issue` does, field by field: its cpu and memory, which the stub
// agents need little of, and its ephemeral-storage limit and request, both explicitly configured
// and equal; the controller's pod keeps config.DefaultResources(). The stub agent sleeps, so the
// override sizes the issue pod down from the default 3 CPU and 19 GiB to a fifth of a CPU and
// 768Mi: the kind job's node, one runner of four CPUs and 16Gi, holds the dozen issue pods the run
// keeps up at once beside its caches and the cluster's own pods.
var liveIssueOverride = config.Reservation{CPU: "200m", Memory: "768Mi", EphemeralStorage: "2Gi", EphemeralStorageRequest: "2Gi"}

// liveResources are the reservations the rig hands the runtime (Options.Resources): the daemon's
// defaults as it translates them (reservations, manifest_test.go) with liveIssueOverride applied
// field by field to the issue pod's, as resolveKubernetes settles a file's `resources` block. Cpu
// and memory are each the request and the limit alike, and so is ephemeral storage.
func (r *liveRig) liveResources() PodResources {
	resources := reservations()
	issue := *resources.Issue.DeepCopy()
	for name, value := range map[corev1.ResourceName]string{corev1.ResourceCPU: liveIssueOverride.CPU, corev1.ResourceMemory: liveIssueOverride.Memory} {
		quantity := resource.MustParse(value)
		issue.Requests[name] = quantity
		issue.Limits[name] = quantity.DeepCopy()
	}
	issue.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse(liveIssueOverride.EphemeralStorage)
	issue.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse(liveIssueOverride.EphemeralStorageRequest)
	resources.Issue = issue
	return resources
}

// The run's one provider key: the variable its agents' Oh My Pi gets, and the key of the providers
// Secret (ProvidersSecretName) newKindLiveEnv creates for the run, holding a value no model route
// reads.
const (
	liveProviderKey        = "STAGE4A_PROVIDER_KEY"
	liveProvidersSecretKey = "stage4a"
)

// fixtureConfigMap is the ConfigMap the operator fixture's pod.yml names; the run creates its own
// copy, named for its project, and points the fixture at it, as a run on a cluster other runs
// share does.
const fixtureConfigMap = "legion-operator-route"

// The marker is the runtime locator's process incarnation: an unchanged pod UID plus the launch
// generation, prefixed with the role that launched it since the marker is one issue pod's, shared
// by every resident role (SandboxName). A role restart keeps the pod UID but must append a new
// incarnation. It lives on the pod's own disk, so it says nothing about any earlier pod: what a
// role carries across a pod replacement is its session, which the table holds (recordSession).
var stubAgent = []string{"/bin/sh", "-c", `printf '%s:%s/%s\n' "$LEGION_ROLE" "$POD_UID" "$LEGION_GENERATION" >>"$LEGION_E2E_MARKER" && exec sleep infinity`, "stage4a-stub"}

// liveEnv is what the run is set up with (newKindLiveEnv): the rig's cluster and fake GitHub, and
// what the run puts in them before its first check.
type liveEnv struct {
	// kubeconfig and kubeContext reach the cluster as its admin: the operator's identity, which
	// every operator step (liveRig.kubectl) takes. runtimeToken is liveDaemonAccount's, the
	// runtime's own identity on the same cluster.
	kubeconfig, kubeContext, runtimeToken string
	namespace, project, claimProject      string
	// image is the image every pod of the run starts, which trusts the fake GitHub's authority
	// (kindtest.Rig.Image); probeImage is the worker image under test, unmodified, which the
	// image probe's checks start, since what they check is the image itself.
	image, probeImage string
	// repo is the run's private repository of the fake GitHub, and repoSecondOwner a private
	// repository under another owner, the implement App installed on both, where second-owner
	// provisions an issue. github serves both; repoBare is the bare repository it serves repo
	// from, which the harness's own writes to GitHub land in (liveRig.bareGit).
	repo, repoSecondOwner ghrepo.Repository
	github                *fakegithub.Server
	repoBare              string
	// streamHost is the kind network's gateway on this host, where the harness's listener binds
	// and every pod dials it; streamPort is fixed for the run, since a live claim's shim dials the
	// same address again once re-adopt and orphan-sweep replace the listener.
	streamHost, streamPort string
	// appID and appTokenCommand are the fake GitHub's implement App and a token command whose
	// every token the fake takes (kindtest.Rig.AppTokenCommand); resolveApp's token manager runs
	// it with `sh -c` for every lease.
	appID, appTokenCommand string
	// storageClass is the issue volumes' StorageClass, kind's own.
	storageClass string
	// work is where the runtime's log goes: the rig's evidence directory, which the kind job
	// uploads when the test fails. scratch holds the run's own files.
	work, scratch string
	// operatorPodFile is the run's copy of deploy/kubernetes/operator-route/pod.yml, its
	// placeholders filled, the operator's pod every launch carries; operatorConfigMap is the run's
	// copy of the ConfigMap it names.
	operatorPodFile, operatorConfigMap string
	// The agent-secrets checks' inputs: this repository's own broker, in this process at the kind
	// network's gateway (startLiveBroker), the sha256 of the value it serves as LEGION_E2E_AUTO,
	// the checkout's agent-secrets binary, and the token file the harness logs in with as the
	// daemon does.
	agentSecretsURL, agentSecretsAutoSHA, agentSecretsBin, agentSecretsTokenFile string
	// sessionDSNLocalFile holds the URL of the rig's PostgreSQL as this process dials it, for the
	// harness's own connection (writeSession), and sessionDBAddress is its address as the pods dial
	// it, which the providers Secret's liveSessionsSecretKey holds a URL of.
	sessionDSNLocalFile, sessionDBAddress string
	// The stale-plugin-install checkpoint's own inputs: this checkout's packed pi-legion and
	// pi-envoy (packLivePlugins) and their versions. Its negative control names the published
	// pi-legion@9.6.1 by exact version, so bun resolves it from the registry inside the pod.
	piLegionTarball, piEnvoyTarball, piLegionVersion, piEnvoyVersion string
}

// The run's names in the rig's cluster: its project, the ServiceAccount the runtime acts as and
// the one every pod of the run runs as, the audience the operator fixture's projected token is
// issued for and the model gateway its models.yml names, which no stub agent dials (RFC 2606's
// .invalid), and the audience the pods' and the daemon's tokens carry to the broker.
const (
	liveProject              = "s4a-kind"
	liveDaemonAccount        = "legion-daemon"
	liveWorkerAccount        = "legion-worker"
	liveModelAudience        = "legion-model-gateway"
	liveModelBaseURL         = "https://model-gateway.invalid/anthropic"
	liveAgentSecretsAudience = "agent-secrets"
	// liveApprovalOwner owns LEGION_E2E_APPROVAL, a human-tier secret no person signed in holds.
	liveApprovalOwner = "stage4a-approver@example.com"
)

// liveIdentity is the runtime's identity in the cluster: the legion-daemon ServiceAccount, with
// what docs/kubernetes.md's "RBAC the Go daemon needs" grants it — Sandboxes, Secrets but never a
// list of them, pod and Service reads, pods/log, events, and a get by name of the Sandbox CRD and
// of Agent Sandbox's controller — and no pod or PVC verb; and the ServiceAccount every pod of the
// run runs as (the operator fixture's service_account).
func liveIdentity(namespace string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata: {name: %[2]s, namespace: %[1]s}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: %[3]s, namespace: %[1]s}
automountServiceAccountToken: false
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: %[2]s, namespace: %[1]s}
rules:
- {apiGroups: [agents.x-k8s.io], resources: [sandboxes], verbs: [create, get, list, watch, patch, delete]}
- {apiGroups: [agents.x-k8s.io], resources: [sandboxes/status], verbs: [get]}
- {apiGroups: [""], resources: [secrets], verbs: [create, get, update, delete]}
- {apiGroups: [""], resources: [pods], verbs: [get, list, watch]}
- {apiGroups: [""], resources: [pods/log], verbs: [get]}
- {apiGroups: [""], resources: [services], verbs: [list, watch]}
- {apiGroups: [""], resources: [events], verbs: [list]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: %[2]s, namespace: %[1]s}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: %[2]s}
subjects: [{kind: ServiceAccount, name: %[2]s, namespace: %[1]s}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: %[2]s-install}
rules:
- {apiGroups: [apiextensions.k8s.io], resources: [customresourcedefinitions], resourceNames: [sandboxes.agents.x-k8s.io], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: %[2]s-install}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: %[2]s-install}
subjects: [{kind: ServiceAccount, name: %[2]s, namespace: %[1]s}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: %[2]s-install, namespace: agent-sandbox-system}
rules:
- {apiGroups: [apps], resources: [deployments], resourceNames: [agent-sandbox-controller], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: %[2]s-install, namespace: agent-sandbox-system}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: %[2]s-install}
subjects: [{kind: ServiceAccount, name: %[2]s, namespace: %[1]s}]
`, namespace, liveDaemonAccount, liveWorkerAccount)
}

// newKindLiveEnv sets up the run on rig before its first check: the runtime's identity
// (liveIdentity), two private repositories of the fake GitHub under two owners and the implement
// App installed on both, the rig's PostgreSQL for the sessions, the operator route's ConfigMap and
// pod fixture with their placeholders filled, the providers Secret, the broker, the checkout's
// agent-secrets and its two packed plugins.
func newKindLiveEnv(t *testing.T, rig *kindtest.Rig) liveEnv {
	t.Helper()
	root := kindtest.RepositoryRoot(t)
	env := liveEnv{
		kubeconfig: rig.Kubeconfig, kubeContext: rig.Context, namespace: "legion", project: liveProject,
		// The claim token's project is a project token, [a-z0-9]+; the label keeps the dashes.
		claimProject: strings.ReplaceAll(liveProject, "-", ""), image: rig.Image, probeImage: rig.WorkerImage,
		github: rig.GitHub, streamHost: rig.Gateway, storageClass: "standard", work: rig.Evidence, scratch: t.TempDir(),
	}
	held, err := net.Listen("tcp", net.JoinHostPort(rig.Gateway, "0"))
	if err != nil {
		t.Fatalf("pick the worker stream's port on %s: %v", rig.Gateway, err)
	}
	_, env.streamPort, _ = net.SplitHostPort(held.Addr().String())
	_ = held.Close()
	rig.Apply(t, liveIdentity(env.namespace))
	if env.runtimeToken, err = rig.ServiceAccountToken(env.namespace, liveDaemonAccount, "", 4*time.Hour); err != nil {
		t.Fatalf("mint the runtime's token: %v", err)
	}

	first := rig.GitHub.NewRepository(t, "legion-kind", "stage4a", false)
	second := rig.GitHub.NewRepository(t, "legion-kind-other", "stage4a", false)
	env.repo, env.repoSecondOwner = ghrepo.MustParse(first.Slug()), ghrepo.MustParse(second.Slug())
	env.repoBare = first.Bare
	app := rig.GitHub.AddApp(t, "legion-implement-kind", first.Owner, second.Owner)
	env.appID, env.appTokenCommand = strconv.FormatInt(app.ID, 10), rig.AppTokenCommand(t, app, first.Owner, second.Owner)

	sessions := rig.StartPostgres(t)
	env.sessionDSNLocalFile = filepath.Join(env.scratch, "session-dsn-local")
	if err := os.WriteFile(env.sessionDSNLocalFile, []byte(sessions.Host+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inCluster, err := url.Parse(sessions.InCluster)
	if err != nil {
		t.Fatal(err)
	}
	env.sessionDBAddress = inCluster.Host

	route := filepath.Join(root, "deploy", "kubernetes", "operator-route")
	fill := func(name string, placeholders map[string]string) string {
		t.Helper()
		text, err := os.ReadFile(filepath.Join(route, name))
		if err != nil {
			t.Fatal(err)
		}
		filled := string(text)
		for placeholder, value := range placeholders {
			if !strings.Contains(filled, placeholder) {
				t.Fatalf("the operator route's %s has no %s to fill", name, placeholder)
			}
			filled = strings.ReplaceAll(filled, placeholder, value)
		}
		path := filepath.Join(env.scratch, name)
		if err := os.WriteFile(path, []byte(filled), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	models := fill("models.yml", map[string]string{"${MODEL_BASE_URL}": liveModelBaseURL})
	env.operatorPodFile = fill("pod.yml", map[string]string{"${MODEL_TOKEN_AUDIENCE}": liveModelAudience, "${POD_SERVICE_ACCOUNT}": liveWorkerAccount})
	env.operatorConfigMap = fixtureConfigMap + "-" + env.project
	rig.Kubectl(t, "-n", env.namespace, "create", "configmap", env.operatorConfigMap,
		"--from-file=models.yml="+models, "--from-file=overlay.yml="+filepath.Join(route, "overlay.yml"))
	note("operator", "ConfigMap %s: models.yml (baseUrl %s) and overlay.yml from %s", env.operatorConfigMap, liveModelBaseURL, route)
	rig.Kubectl(t, "-n", env.namespace, "create", "secret", "generic", ProvidersSecretName(env.project),
		"--from-literal="+liveProvidersSecretKey+"="+randomHex(t, 16), "--from-literal="+liveSessionsSecretKey+"="+sessions.InCluster)
	note("operator", "Secret %s: %s (a random value no route reads), %s (the rig's PostgreSQL as the pods dial it)",
		ProvidersSecretName(env.project), liveProvidersSecretKey, liveSessionsSecretKey)

	startLiveBroker(t, rig, &env, sessions.Host)
	env.agentSecretsBin = filepath.Join(env.scratch, "agent-secrets")
	build := exec.Command("go", "build", "-o", env.agentSecretsBin, "./cmd/agent-secrets")
	build.Dir = filepath.Join(root, "packages", "envoy")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agent-secrets from the checkout: %v: %s", err, out)
	}
	packLivePlugins(t, root, &env)
	return env
}

// startLiveBroker starts this repository's own broker in this process (brokertest), at the kind
// network's gateway where the pods reach it, on the rig's PostgreSQL as this process dials it
// (dsn), verifying the cluster's own tokens for liveAgentSecretsAudience: the daemon's login as
// liveDaemonAccount, which logs in as the legion-daemon service, and the pods of liveWorkerAccount,
// which that login enrols. It serves LEGION_E2E_AUTO, shared and agent-tier, which every session
// gets at once, and LEGION_E2E_APPROVAL, human-tier under liveApprovalOwner alone. The daemon's
// token file holds an hour's token, the longest the broker takes, minted again every half hour
// while the test runs, as the kubelet rotates a daemon pod's.
func startLiveBroker(t *testing.T, rig *kindtest.Rig, env *liveEnv, dsn string) {
	t.Helper()
	t.Setenv("BROKER_TEST_DATABASE_URL", dsn)
	account := func(name string) string { return "system:serviceaccount:" + env.namespace + ":" + name }
	broker := brokertest.NewRig(t, brokertest.WithListen(net.JoinHostPort(rig.Gateway, "0")),
		brokertest.WithClusterIssuer(rig.Issuer, liveAgentSecretsAudience, rig.IssuerClient(t),
			map[string]string{account(liveDaemonAccount): "legion-daemon"}, map[string]string{account(liveWorkerAccount): "legion-daemon"}))
	auto := randomHex(t, 16)
	broker.PutSecret(t, "LEGION_E2E_AUTO", brokertest.OwnerShared, brokertest.TierAgent, auto)
	broker.PutSecret(t, "LEGION_E2E_APPROVAL", liveApprovalOwner, brokertest.TierHuman, randomHex(t, 16))
	sum := sha256.Sum256([]byte(auto))
	env.agentSecretsURL, env.agentSecretsAutoSHA = broker.URL, hex.EncodeToString(sum[:])
	env.agentSecretsTokenFile = filepath.Join(env.scratch, "agent-secrets-token")
	mint := func() error {
		token, err := rig.ServiceAccountToken(env.namespace, liveDaemonAccount, liveAgentSecretsAudience, time.Hour)
		if err != nil {
			return err
		}
		next := env.agentSecretsTokenFile + ".next"
		if err := os.WriteFile(next, []byte(token), 0o600); err != nil {
			return err
		}
		return os.Rename(next, env.agentSecretsTokenFile)
	}
	if err := mint(); err != nil {
		t.Fatalf("mint %s's token for the broker: %v", liveDaemonAccount, err)
	}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Minute):
				if err := mint(); err != nil {
					fmt.Printf("   [operator] could not mint a fresh token for %s; the run's logins fail once the last one expires: %v\n", liveDaemonAccount, err)
				}
			}
		}
	}()
	note("harness", "broker %s: LEGION_E2E_AUTO (shared, agent tier) and LEGION_E2E_APPROVAL (human tier, %s); %s logs in as legion-daemon and enrols the pods of %s",
		broker.URL, liveApprovalOwner, liveDaemonAccount, liveWorkerAccount)
}

// packLivePlugins packs this checkout's pi-envoy and pi-legion through scripts/e2e/lib/pack-plugin.sh,
// as every check that installs a branch-built plugin packs them, which needs bun and the
// workspace's own install at the checkout's root, and reads their versions from their manifests.
func packLivePlugins(t *testing.T, root string, env *liveEnv) {
	t.Helper()
	_, err := exec.LookPath("bun")
	testprereq.Require(t, err == nil, "kind", "stale-plugin-install packs this checkout's two plugins with bun, which is not on PATH")
	pack := func(name string) (tarball, version string) {
		t.Helper()
		command := exec.Command("bash", filepath.Join(root, "scripts", "e2e", "lib", "pack-plugin.sh"), name, filepath.Join(env.scratch, "plugins", name))
		command.Dir = root
		var stderr bytes.Buffer
		command.Stderr = &stderr
		out, err := command.Output()
		if err != nil {
			t.Fatalf("pack %s: %v: %s", name, err, stderr.String())
		}
		manifest, err := os.ReadFile(filepath.Join(root, "packages", name, "package.json"))
		if err != nil {
			t.Fatal(err)
		}
		var pkg struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(manifest, &pkg); err != nil || pkg.Version == "" {
			t.Fatalf("packages/%s/package.json names no version: %v", name, err)
		}
		return strings.TrimSpace(string(out)), pkg.Version
	}
	env.piEnvoyTarball, env.piEnvoyVersion = pack("pi-envoy")
	env.piLegionTarball, env.piLegionVersion = pack("pi-legion")
	note("harness", "packed pi-envoy@%s (%s) and pi-legion@%s (%s) from the checkout", env.piEnvoyVersion, env.piEnvoyTarball, env.piLegionVersion, env.piLegionTarball)
}

// randomHex is n random bytes, hex-encoded.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

// readOperatorPod reads the operator fixture through the daemon's own loader, with its ConfigMap named
// for this run.
func readOperatorPod(env liveEnv) (Pod, error) {
	pod, err := config.ReadPodFile(env.operatorPodFile)
	if err != nil {
		return Pod{}, err
	}
	renamed := 0
	for _, volume := range pod.Volumes {
		if volume.ConfigMap != nil && volume.ConfigMap.Name == fixtureConfigMap {
			volume.ConfigMap.Name = env.operatorConfigMap
			renamed++
		}
	}
	if renamed != 1 {
		return Pod{}, fmt.Errorf("%s names ConfigMap %s in %d volumes, want one", env.operatorPodFile, fixtureConfigMap, renamed)
	}
	return Pod(pod), nil
}

// claimState is where a claim stands, as the daemon's machine would record it.
type claimState int

const (
	stateNone claimState = iota
	stateRunning
	stateSuspended
	stateDead
	stateReleased
)

// liveClaim is one claim of the run and what the harness recorded for it.
type liveClaim struct {
	token             claim.Token
	tree, issue, name string
	role              claim.Role
	// repo is the claim's repository when it is not LEGION_E2E_REPO (second-owner's).
	repo   ghrepo.Repository
	marker string
	// session is the claim's recorded session, the path every resume of it names. No pod ever
	// holds a file there — the disk it would have sat on dies with the pod — so the role launcher
	// finds it in the session table alone; recorded says the harness has put it there.
	session   string
	recorded  bool
	state     claimState
	gen       uint64
	bootToken string
	// loc is the running process; last is the latest locator, kept as Resume's hint once the
	// process stopped.
	loc, last *runtime.Locator
}

// minted is one boot token the harness minted, for one generation of one claim.
type minted struct {
	claim claim.Token
	gen   uint64
	hash  string
	armed bool
}

// registration is a hello the listener registered: the claim and generation of the Hello event,
// the hash of the token the resolver accepted for it, and — when the shim's hello carried one —
// the pod's agent-secrets session identity.
type registration struct {
	claim    claim.Token
	gen      uint64
	hash     string
	at       time.Time
	identity *stream.AgentSecretsIdentity
}

// registry is the harness's store of boot tokens, which outlives every listener and runtime of
// the run as the daemon's database does. Its resolver accepts only each claim's current,
// armed generation, and records every hello it judges.
type registry struct {
	mu       sync.Mutex
	byToken  map[string]minted
	current  map[claim.Token]uint64
	pending  map[claim.Token]string
	accepted []registration
	refused  map[claim.Token]int
	changed  chan struct{}
}

func newRegistry() *registry {
	return &registry{
		byToken: map[string]minted{}, current: map[claim.Token]uint64{}, pending: map[claim.Token]string{},
		refused: map[claim.Token]int{}, changed: make(chan struct{}),
	}
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// mint makes the claim's next generation current. An unarmed token is refused as unknown, so a
// pod on it can never register.
func (g *registry) mint(c claim.Token, armed bool) (string, uint64) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	token := hex.EncodeToString(raw)
	g.mu.Lock()
	defer g.mu.Unlock()
	gen := g.current[c] + 1
	g.current[c] = gen
	g.byToken[token] = minted{claim: c, gen: gen, hash: tokenHash(token), armed: armed}
	return token, gen
}

func (g *registry) resolve(bootToken string) (claim.Token, uint64, bool, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, ok := g.byToken[bootToken]
	switch {
	case !ok:
		return "", 0, false, false, nil
	case !m.armed:
		g.refused[m.claim]++
		return "", 0, false, false, nil
	case m.gen != g.current[m.claim]:
		g.refused[m.claim]++
		return m.claim, m.gen, true, true, nil
	}
	g.pending[m.claim] = m.hash
	return m.claim, m.gen, false, true, nil
}

func (g *registry) hello(event stream.Hello) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.accepted = append(g.accepted, registration{
		claim: event.Claim, gen: event.Generation, hash: g.pending[event.Claim], at: time.Now(), identity: event.AgentSecrets,
	})
	close(g.changed)
	g.changed = make(chan struct{})
}

// await is the claim's registration at gen after since, within limit.
func (g *registry) await(c claim.Token, gen uint64, since time.Time, limit time.Duration) (registration, bool) {
	deadline := time.After(limit)
	for {
		g.mu.Lock()
		for _, reg := range g.accepted {
			if reg.claim == c && reg.gen == gen && !reg.at.Before(since) {
				g.mu.Unlock()
				return reg, true
			}
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			return registration{}, false
		}
	}
}

func (g *registry) registrations(c claim.Token) []registration {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []registration
	for _, reg := range g.accepted {
		if reg.claim == c {
			out = append(out, reg)
		}
	}
	return out
}

func (g *registry) refusals(c claim.Token) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refused[c]
}

// observed is one observation the runtime delivered, numbered in delivery order across every
// runtime instance of the run.
type observed struct {
	runtime.Observation
	seq int
}

type observations struct {
	mu      sync.Mutex
	list    []observed
	changed chan struct{}
}

func (o *observations) add(obs runtime.Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.list = append(o.list, observed{Observation: obs, seq: len(o.list) + 1})
	close(o.changed)
	o.changed = make(chan struct{})
}

// mark is the sequence number of the latest observation: what came after it has a larger one.
func (o *observations) mark() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.list)
}

func (o *observations) since(after int) []observed {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.list[after:])
}

// await is the first observation after `after` that match accepts, within limit.
func (o *observations) await(after int, limit time.Duration, match func(runtime.Observation) bool) (observed, bool) {
	deadline := time.After(limit)
	for {
		o.mu.Lock()
		for _, obs := range o.list[after:] {
			if match(obs.Observation) {
				o.mu.Unlock()
				return obs, true
			}
		}
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			return observed{}, false
		}
	}
}

// logRecorder keeps every record the runtime and the listener log, in memory for the checks
// that read them and as JSON lines in the run's evidence.
type logRecorder struct {
	mu      sync.Mutex
	json    slog.Handler
	records []map[string]string
}

type recordingHandler struct {
	rec   *logRecorder
	attrs []slog.Attr
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) WithGroup(string) slog.Handler            { return h }
func (h recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return recordingHandler{rec: h.rec, attrs: append(slices.Clone(h.attrs), attrs...)}
}

func (h recordingHandler) Handle(ctx context.Context, record slog.Record) error {
	fields := map[string]string{"msg": record.Message, "level": record.Level.String()}
	add := func(a slog.Attr) bool { fields[a.Key] = a.Value.String(); return true }
	for _, a := range h.attrs {
		add(a)
	}
	record.Attrs(add)
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	h.rec.records = append(h.rec.records, fields)
	return h.rec.json.WithAttrs(h.attrs).Handle(ctx, record)
}

func (l *logRecorder) find(msg string) (map[string]string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.records) - 1; i >= 0; i-- {
		if l.records[i]["msg"] == msg {
			return l.records[i], true
		}
	}
	return nil, false
}

// liveEnrollment is one pod's agent-secrets enrollment as the harness (playing the daemon's
// machine) holds it: the broker's id, and the pod incarnation it was enrolled for — so a claim
// relaunched since (suspend/resume, kill/relaunch) is recognized as needing a fresh enrollment.
type liveEnrollment struct {
	id, incarnation string
}

// liveRig is the run: the two identities, the registry, and the current runtime instance.
type liveRig struct {
	t      *testing.T
	env    liveEnv
	ctx    context.Context
	cancel context.CancelFunc

	rc   *rest.Config // the runtime identity, as the runtime gets it
	kube kubernetes.Interface
	dyn  dynamic.Interface

	tokens   implementTokens
	identity runtime.GitIdentity
	// defaultBranch is LEGION_E2E_REPO's default branch as GitHub names it (resolveApp), which
	// every launch names and independent-provision holds each workspace to; secondDefaultBranch is
	// LEGION_E2E_REPO_SECOND_OWNER's, which second-owner's launch names.
	defaultBranch, secondDefaultBranch string
	prompt                             string
	// pod is the operator's pod every launch carries: the fixture, as the daemon reads it.
	pod  Pod
	reg  *registry
	obs  *observations
	logs *logRecorder
	log  *slog.Logger
	// trees is the durable state every runtime of the run reads (Options.Store): every tree live
	// until a check records its cleanup confirmed.
	trees *fakeStore
	// mapped is the repositories the run maps (Options.Mapped): each added just before its first
	// issue pod launches (root-ready, second-owner, or the setup of a group that does not hold
	// root-ready), so no sweep makes a cache the run's first launch of that repository is to make.
	mappedMu sync.Mutex
	mapped   []ghrepo.Repository

	// The current runtime instance and its listener; stop ends both.
	rt      *Runtime
	ln      *stream.Listener
	stop    context.CancelFunc
	stopped sync.WaitGroup

	// claims are the run's claims in newLiveRig's order, each tree's root before its other claims.
	claims []*liveClaim
	// What kill-role-init recorded for stale-incarnation.
	killed struct {
		old, fresh runtime.Locator
		gone       observed
	}

	// leakCheck is the shared public leak check every runtime of the run hands its launchers
	// (Options.LeakCheck): the rig's stand-in, read as the daemon reads the real one
	// (kindtest.LeakCheck). leakCheckSetting is the deployment setting that names it
	// (Options.LeakCheckSetting, kindtest.LeakCheckSetting): its repository and the manifest
	// digest of its files, under which the runtime pushes it to each launcher and tells every role
	// the install as PUBLIC_LEAK_CHECK and the digest beside it, as a deployment that names its
	// check has it done.
	leakCheck        LeakCheckReader
	leakCheckSetting leakguard.Setting

	// secrets is the broker client, nil when the token login started in newLiveRig was refused
	// (secretsBlocked); secretsBlockReason is empty exactly when secrets is set, and holds that
	// refusal for secretsBlocked to report, since a login outcome discovered at rig setup has
	// nowhere else to surface before any check runs.
	// enrollments is the harness's own enrollment of each claim's running pod, as the daemon's
	// machine holds it; grants and requests are what checkSecretsAutomaticGrant records for
	// checkSecretsCrossPodNegative to attack.
	secrets            *agentsecrets.Client
	secretsBlockReason string
	enrollments        map[claim.Token]liveEnrollment
	grants             map[claim.Token]string
	requests           map[claim.Token]string
	// sessionStore is where every runtime of the run keeps its sessions (Options.SessionStore,
	// runtime.kubernetes.session_store postgres): liveSessionsSecretKey, the providers Secret key
	// holding the rig's PostgreSQL's URL, from the rig's first runtime on. The daemon loads no store
	// that keeps sessions on the issue's disk, so there is no run of this harness without a
	// database.
	sessionStore ompsessions.Store
}

// TestStage4aSandboxRuntimeLive runs each group of liveGroups as a subtest of its name, on a kind
// rig of its own (runLiveGroup). A group starts from nothing another group left, so one that fails
// stops no other.
func TestStage4aSandboxRuntimeLive(t *testing.T) {
	fixtureenv.Live(t)
	for _, group := range liveGroups {
		t.Run(group.name, func(t *testing.T) { runLiveGroup(t, group) })
	}
}

// runLiveGroup runs group as t: a kind rig of the group's own and the run's setup on it, the
// group's own setup, then its checks in their order, each as a subtest of its name, stopping at the
// first that fails.
func runLiveGroup(t *testing.T, group liveGroup) {
	rig := kindtest.Start(t)
	r := newLiveRig(t, newKindLiveEnv(t, rig))
	defer r.close()
	if group.setup != nil {
		fmt.Printf("== setup of %s\n", group.name)
		if err := group.setup(r); err != nil {
			t.Fatalf("the setup of %s: %v", group.name, err)
		}
	}
	for _, check := range group.checks {
		if !t.Run(check.name, func(t *testing.T) { r.run(t, check) }) {
			return
		}
	}
	fmt.Printf("stage 4a harness: every check of %s passed\n", group.name)
}

// run runs check as t. A check the run cannot support prints its reason and fails t under GitHub
// Actions, where the kind job gives the run all it needs (testprereq), and prints SKIPPED-BLOCKED
// and skips elsewhere.
func (r *liveRig) run(t *testing.T, check liveCheck) {
	if check.blocked != nil {
		if reason := check.blocked(r); reason != "" {
			if testprereq.Enforced() {
				fmt.Printf("CHECK %s: FAIL: blocked: %s\n", check.name, reason)
			} else {
				fmt.Printf("CHECK %s: SKIPPED-BLOCKED: %s\n", check.name, reason)
			}
			testprereq.Require(t, false, "kind", "%s is blocked: %s", check.name, reason)
			return
		}
	}
	fmt.Printf("== %s\n", check.name)
	started := time.Now()
	if err := check.run(r); err != nil {
		fmt.Printf("CHECK %s: FAIL: %v\n", check.name, err)
		t.FailNow()
	}
	fmt.Printf("CHECK %s: PASS (%s)\n", check.name, time.Since(started).Round(time.Second))
}

// note prints one piece of evidence, naming the identity that observed it: runtime (the Legion
// daemon's restricted identity, through the runtime or the harness's own client under it),
// operator (the rig's admin kubeconfig, through kubectl), or harness (the listener and its
// resolver).
func note(who, format string, args ...any) {
	fmt.Printf("   [%s] %s\n", who, fmt.Sprintf(format, args...))
}

func newLiveRig(t *testing.T, env liveEnv) *liveRig {
	ctx, cancel := context.WithCancel(context.Background())
	r := &liveRig{
		t: t, env: env, ctx: ctx, cancel: cancel, reg: newRegistry(), obs: &observations{changed: make(chan struct{})}, trees: newFakeStore(),
		enrollments: map[claim.Token]liveEnrollment{}, grants: map[claim.Token]string{},
		sessionStore: ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: liveSessionsSecretKey},
	}
	logFile, err := os.OpenFile(filepath.Join(env.work, "runtime.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	r.logs = &logRecorder{json: slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})}
	r.log = slog.New(recordingHandler{rec: r.logs})

	// The runtime's identity is the legion-daemon ServiceAccount's token on the admin
	// kubeconfig's cluster, its own credentials dropped (liveIdentity).
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: env.kubeconfig}
	admin, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: env.kubeContext}).ClientConfig()
	if err != nil {
		t.Fatalf("the context %s in %s: %v", env.kubeContext, env.kubeconfig, err)
	}
	r.rc = rest.AnonymousClientConfig(admin)
	r.rc.BearerToken = env.runtimeToken
	note("runtime", "the runtime acts as system:serviceaccount:%s:%s on %s", env.namespace, liveDaemonAccount, r.rc.Host)
	// The harness's own reads under the runtime identity run faster than client-go's default
	// rate; the runtime paces its own (clientQPS).
	own := rest.CopyConfig(r.rc)
	own.QPS, own.Burst = 50, 100
	if r.kube, err = kubernetes.NewForConfig(own); err != nil {
		t.Fatal(err)
	}
	if r.dyn, err = dynamic.NewForConfig(own); err != nil {
		t.Fatal(err)
	}
	r.leakCheck, r.leakCheckSetting = kindtest.LeakCheck(t), kindtest.LeakCheckSetting(t)
	// The run logs in as the daemon does, with legion-daemon's token for the broker; no person
	// approves it, and the client keeps logging in for as long as the run lasts.
	client := &agentsecrets.Client{URL: env.agentSecretsURL, TokenFile: env.agentSecretsTokenFile, Log: r.log}
	if err := client.Start(r.ctx); err != nil {
		r.secretsBlockReason = fmt.Sprintf("the broker at %s refused %s's token login: %v", env.agentSecretsURL, liveDaemonAccount, err)
	} else {
		r.secrets = client
	}

	if r.pod, err = readOperatorPod(env); err != nil {
		t.Fatal(err)
	}
	note("operator", "pod from %s: service account %s, %d volumes, %d mounts, ConfigMap %s",
		env.operatorPodFile, r.pod.ServiceAccount, len(r.pod.Volumes), len(r.pod.VolumeMounts), env.operatorConfigMap)

	prompt := filepath.Join(env.scratch, "role-prompt.md")
	if err := os.WriteFile(prompt, []byte("The Stage 4a stub agent; no model reads this.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.prompt = prompt

	cacheIssue, updateIssue, fleetIssue := cacheRunIssue(env.project), podUpdateRunIssue(env.project), podUpdateFleetRunIssue(env.project)
	for _, c := range []struct {
		name, tree, issue string
		role              claim.Role
	}{
		{"root", "S4A-1", "S4A-1", claim.RoleArchitect},
		{"worker", "S4A-1", "S4A-1", claim.RoleImplementer},
		{"second", "S4A-1", "S4A-1", claim.RoleTester},
		{"fresh", "S4A-1", "S4A-1", claim.RoleReviewer},
		// orphan is a separate issue pod: its unrecorded Sandbox can be swept or deleted without
		// taking the root issue's resident roles with it. Its role is its own tree's architect, the
		// claim a tree's first launch is in the daemon.
		{"orphan", "S4A-4", "S4A-4", claim.RoleArchitect},
		// sessions is postgres-resume's own tree: the one claim whose session the check writes into
		// the table itself, dated before its pod's workspace, rather than letting the rig record it.
		{"sessions", "S4A-5", "S4A-5", claim.RoleArchitect},
		// root2 and child2 are two issues of one tree, each in a Sandbox and on a volume of its own
		// (independent-provision).
		{"root2", "S4A-2", "S4A-2", claim.RoleArchitect},
		{"child2", "S4A-2", "S4A-3", claim.RolePlanner},
		// displaced and displacer are two roles of one more issue pod, which the displacer's launch
		// replaces while the displaced role still runs in it (displaced).
		{"displaced", "S4A-6", "S4A-6", claim.RoleArchitect},
		{"displacer", "S4A-6", "S4A-6", claim.RoleImplementer},
		// update-a and update-b are the two workers of one more issue pod, which an update round
		// replaces once each of them has pushed its work and answered (pod-update). Their issue is
		// its run's own (podUpdateRunIssue), since each of them pushes the issue's branch onto the
		// repository every run shares.
		{"update-a", updateIssue, updateIssue, claim.RoleArchitect},
		{"update-b", updateIssue, updateIssue, claim.RoleImplementer},
		// fleet-a and fleet-b are the two workers of a second issue pod, which the one pod-template
		// change drifts beside update-a and update-b's (pod-update-fleet): each issue's pod is a
		// round of its own. Their issue is its run's own (podUpdateFleetRunIssue), since they push
		// its branch onto the repository every run shares, as pod-update's workers push theirs.
		{"fleet-a", fleetIssue, fleetIssue, claim.RoleArchitect},
		{"fleet-b", fleetIssue, fleetIssue, claim.RoleImplementer},
		// cache-b is one more issue pod on the run's repository, launched while its cache is Ready
		// (cache-second-pod) and again after its volume is lost (cache-lost-volume). Its issue is
		// its run's own (cacheRunIssue), since cache-lost-volume pushes the issue's branch onto the
		// repository every run shares.
		{"cache-b", cacheIssue, cacheIssue, claim.RoleArchitect},
		// second-owner is an issue pod on the repository of the run's other owner (second-owner).
		{"second-owner", "S4A-7", "S4A-7", claim.RoleImplementer},
		// setup-negative is the throwaway issue the setup-negative check launches on a second
		// runtime whose setup exits 7, then deletes.
		{"setup-negative", "S4A-8", "S4A-8", claim.RoleArchitect},
		// closed is the issue whose close takes its whole pod, disk included, and whose next launch
		// brings a disk of its own while the agent carries on from the table (issue-suspend-resume).
		{"closed", "S4A-9", "S4A-9", claim.RoleArchitect},
	} {
		token, err := claim.NewToken(env.claimProject, c.issue, c.role)
		if err != nil {
			t.Fatal(err)
		}
		r.claims = append(r.claims, &liveClaim{
			token: token, tree: c.tree, issue: c.issue, role: c.role, name: c.name,
			// The marker sits at the root of the issue's volume: TreeRoot is mounted read-write in
			// the pod's one container and seen by every role there (issuePod.launcherContainer), so the six
			// roles of an issue share the one file, and it is the pod's own ephemeral disk, so it
			// dies with the pod — the property this file's header claims. No pod mounts
			// ompSessionsDir any more, which is why the marker is not there.
			marker:  TreeRoot + "/" + SandboxName(token) + ".marker",
			session: ompSessionsDir + "/" + SandboxName(token) + "-" + string(c.role) + ".jsonl",
		})
	}
	r.claim("second-owner").repo = env.repoSecondOwner
	return r
}

func (r *liveRig) claim(name string) *liveClaim {
	i := slices.IndexFunc(r.claims, func(c *liveClaim) bool { return c.name == name })
	return r.claims[i]
}

func (r *liveRig) close() {
	r.stopRuntime()
	r.cancel()
	if r.tokens.apps != nil {
		r.tokens.apps.Close()
	}
}

// sameLocator compares two locators by what they name; the struct holds pointers.
func sameLocator(a, b runtime.Locator) bool {
	return a.Runtime == b.Runtime && a.Claim == b.Claim && a.Incarnation == b.Incarnation &&
		(a.Sandbox == nil) == (b.Sandbox == nil) && (a.Sandbox == nil || *a.Sandbox == *b.Sandbox)
}

// runtimeOptions are the Options every runtime of the run is built from, as a daemon boot builds
// them: the operator's pod, the run's provider key, the stub agent, the shared setup command, the
// shared public leak check every launcher installs before its first role (the rig's stand-in, and
// the setting that names it), the broker, and the worker stream at address, through ln.
func (r *liveRig) runtimeOptions(address string, ln *stream.Listener) Options {
	return Options{
		Namespace: r.env.namespace, Project: r.env.project, Image: r.env.image, StorageClass: r.env.storageClass, IssueVolume: liveIssueVolume,
		Resources: r.liveResources(),
		StreamURL: address,
		Tools: Tools{
			GH: "/usr/local/bin/gh", Git: "/usr/bin/git", JJ: "/usr/local/bin/jj", Legion: "/opt/legion/bin/legion",
			AgentSecrets: "/opt/legion/bin/agent-secrets",
		},
		// Stage4a drives Runtime directly to prove Kubernetes mechanics; the daemon's store is the
		// in-memory one, which outlives each runtime as Postgres outlives a daemon restart.
		Store:                 r.trees,
		Pod:                   r.pod,
		ProviderKeys:          map[string]string{liveProviderKey: liveProvidersSecretKey},
		Agent:                 stubAgent,
		Setup:                 liveSetup(r.env.repo),
		BootTimeout:           liveBootTimeout,
		RegistrationIntervals: 3,
		BusyBackoff:           250 * time.Millisecond,
		TerminationGrace:      liveGrace,
		ProbeInterval:         liveProbeInterval,
		AdoptTimeout:          liveAdoptTimeout,
		Tokens:                r.tokens,
		GitHubCredential:      r.tokens.Credential,
		ControllerCredential:  r.tokens.ControllerCredential,
		GitHubDirs:            r.tokens.ProjectDirs,
		Conns:                 ln,
		Log:                   r.log,
		SessionStore:          r.sessionStore,
		RepositoryCache:       liveRepositoryCache(),
		Mapped:                r.mappedRepositories,
		LeakCheck:             r.leakCheck,
		LeakCheckSetting:      r.leakCheckSetting,
		AgentSecrets:          &AgentSecrets{URL: r.env.agentSecretsURL, Audience: liveAgentSecretsAudience, TokenExpiry: time.Hour},
	}
}

// streamAddress is where the harness's own listener binds and what every pod's shim dials: the
// kind network's gateway on this host, at the run's one port. Every runtime of the run is built on
// it.
func (r *liveRig) streamAddress() string {
	return "tcp://" + net.JoinHostPort(r.env.streamHost, r.env.streamPort)
}

// startRuntime binds a fresh listener on streamAddress, builds a fresh runtime whose pods are
// handed it to dial, and registers the runtime's launcher acceptor, as a daemon boot does
// (sandboxRuntime), with an Observe feeding the run's observation record.
func (r *liveRig) startRuntime() error {
	if r.tokens.apps == nil {
		if err := r.resolveApp(); err != nil {
			return err
		}
	}
	ctx, stop := context.WithCancel(r.ctx)
	bind := r.streamAddress()
	ln, err := stream.Listen(ctx, bind, r.reg.resolve, stream.Options{RPCTimeout: 30 * time.Second, Log: r.log})
	if err != nil {
		stop()
		holder, _ := exec.Command("ss", "-Hltnp", "sport = :"+r.env.streamPort).CombinedOutput()
		return fmt.Errorf("the worker stream cannot bind %s: %v; the port's holder: %s", bind, err, strings.TrimSpace(string(holder)))
	}
	opts := r.runtimeOptions(r.streamAddress(), ln)
	rt, err := New(ctx, r.rc, opts)
	if err != nil {
		stop()
		return err
	}
	ln.SetLauncherResolver(rt.LauncherResolver())
	observe, err := rt.Observe(ctx)
	if err != nil {
		stop()
		return err
	}
	r.rt, r.ln, r.stop = rt, ln, stop
	r.stopped.Add(2)
	go func() {
		defer r.stopped.Done()
		for obs := range observe {
			r.obs.add(obs)
		}
	}()
	go func() {
		defer r.stopped.Done()
		for event := range ln.Events() {
			if hello, ok := event.(stream.Hello); ok {
				r.reg.hello(hello)
			}
		}
	}()
	return nil
}

// startRuntimeOnce starts the runtime unless one is running.
func (r *liveRig) startRuntimeOnce() error {
	if r.rt != nil {
		return nil
	}
	return r.startRuntime()
}

// stopRuntime ends the current runtime and its listener, as a daemon exit does: the informers,
// the Observe, every connection.
func (r *liveRig) stopRuntime() {
	if r.stop == nil {
		return
	}
	r.stop()
	r.stopped.Wait()
	r.rt, r.ln, r.stop = nil, nil, nil
}

// ---- operator steps ----------------------------------------------------------------------------

// kubectl runs one operator step in the namespace through the rig's admin kubeconfig.
func (r *liveRig) kubectl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Minute)
	defer cancel()
	full := append([]string{"--kubeconfig", r.env.kubeconfig, "--context", r.env.kubeContext, "-n", r.env.namespace}, args...)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("kubectl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// roleEnter is the shell that runs a command inside one running role of an issue pod's one
// container (`legion launcher role-init`), from `kubectl exec -c roles`: $1 the role, the rest the
// command. It finds the role's role-init by its argv (argv[1] `launcher`, argv[2] `role-init`, then
// `--role <role>` before the shim's `--`), refusing, exit 125, a role that runs none or two; enters
// that role-init's user, mount, PID and IPC namespaces (uid 0 there, the container's runtime user
// outside, with --preserve-credentials, since gVisor refuses the setgroups a plain nsenter makes),
// where /proc is the role's own and role-init is its PID 1; and from there enters the user
// namespace of the role's worker-shim (role-init's child, PPid 1, whose argv[1] is `worker-shim`:
// role-init reaps every orphan of its namespace, so a process the agent detached is its child too,
// and the word anywhere else in an argv is no shim), so the command runs as the role's own user
// with the role's own mounts: its gh, private, state and agent-secrets directories at their fixed
// paths, its home layer, its /tmp, and an empty roles directory. Pids it reads are the role's own,
// and so are those it signals. With "pid" for the command it prints the role-init's pid in the
// container's PID namespace instead, and runs nothing.
const roleEnter = `role=$1; shift
found=
for d in /proc/[0-9]*; do
  args=$(tr '\0' '\n' <"$d/cmdline" 2>/dev/null) || continue
  [ "$(printf '%s\n' "$args" | sed -n 2,3p | tr '\n' ' ')" = "launcher role-init " ] || continue
  [ "$(printf '%s\n' "$args" | sed -n '/^--$/q; /^--role$/{n;p;q;}')" = "$role" ] || continue
  [ -z "$found" ] || { echo "role $role runs two role-inits, pids $found and ${d#/proc/}" >&2; exit 125; }
  found=${d#/proc/}
done
[ -n "$found" ] || { echo "role $role runs no role-init in this container" >&2; exit 125; }
[ "$1" != pid ] || { echo "$found"; exit 0; }
exec nsenter -t "$found" -U -m -p -i --preserve-credentials -- sh -c '
shim=
for d in /proc/[0-9]*; do
  [ "$(sed -n "s/^PPid:[[:space:]]*//p" "$d/status" 2>/dev/null)" = 1 ] || continue
  [ "$(tr "\0" "\n" <"$d/cmdline" 2>/dev/null | sed -n 2p)" = worker-shim ] || continue
  [ -z "$shim" ] || { echo "the role runs two worker-shims under its role-init" >&2; exit 125; }
  shim=${d#/proc/}
done
[ -n "$shim" ] || { echo "the role runs no worker-shim under its role-init" >&2; exit 125; }
exec nsenter -t "$shim" -U --preserve-credentials -- "$@"' role "$@"`

// exec runs a command inside a claim's running role (roleEnter), through the admin context.
func (r *liveRig) exec(c *liveClaim, command ...string) (string, error) {
	return r.execPod(c, append([]string{"sh", "-c", roleEnter, "role-enter", string(c.role)}, command...)...)
}

// rolePid is the pid of c's role's role-init in the issue pod's container.
func (r *liveRig) rolePid(c *liveClaim) (string, error) {
	return r.execPod(c, "sh", "-c", roleEnter, "role-enter", string(c.role), "pid")
}

// execPod runs a command in the claim's issue pod's one container, in the container's own view:
// what every role shares (the issue's disk at TreeRoot, the markers among it), whether or not the
// claim's role runs.
func (r *liveRig) execPod(c *liveClaim, command ...string) (string, error) {
	out, err := r.kubectl(append([]string{"exec", SandboxName(c.token), "-c", runtime.IssuePodContainer, "--"}, command...)...)
	return strings.TrimSpace(out), err
}

// liveDisk is one issue pod's disk as the operator reads it. Nothing is selected by a label here:
// the disk is a generic ephemeral volume declared in the pod's own template (issuePod.disk),
// and the pinned Sandbox CRD declares that template's metadata a bare object, so the API server
// prunes any label set there — an issue's disk carries none, and is found by its name alone. owner
// is the claim's controlling owner as `<kind>/<name>`: a Pod, never the Sandbox that owned the
// issue's volume before INFRA-80.
type liveDisk struct {
	name, phase, uid, owner string
}

// issueDisk reads c's issue pod's disk; found is false when there is none, which is what a pod
// that has gone leaves behind, its disk deleted with it. The restricted runtime identity has no
// PVC verb, so this is an operator step.
func (r *liveRig) issueDisk(c *liveClaim) (disk liveDisk, found bool, err error) {
	out, err := r.kubectl("get", "pvc", issueDiskName(c.token), "--ignore-not-found",
		"-o", `jsonpath={.metadata.name} {.status.phase} {.metadata.uid} {.metadata.ownerReferences[?(@.controller==true)].kind}/{.metadata.ownerReferences[?(@.controller==true)].name}`)
	if err != nil {
		return liveDisk{}, false, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return liveDisk{}, false, nil
	}
	if len(fields) != 4 {
		return liveDisk{}, false, fmt.Errorf("the operator read %s as %q, want its name, phase, uid and owner", issueDiskName(c.token), strings.TrimSpace(out))
	}
	return liveDisk{name: fields[0], phase: fields[1], uid: fields[2], owner: fields[3]}, true, nil
}

// assertPodOwnedDisk holds c's disk to what an issue pod's disk is now: Bound, and owned by that
// pod itself, so it is created with the pod and deleted with it. It returns the disk, whose uid a
// check comparing a pod replacement's disk with the replaced one's keeps.
func (r *liveRig) assertPodOwnedDisk(c *liveClaim) (liveDisk, error) {
	disk, found, err := r.issueDisk(c)
	if err != nil {
		return liveDisk{}, err
	}
	if !found {
		return liveDisk{}, fmt.Errorf("the issue pod %s has no disk %s", SandboxName(c.token), issueDiskName(c.token))
	}
	pod, err := r.getPod(SandboxName(c.token))
	if err != nil {
		return liveDisk{}, err
	}
	if disk.phase != "Bound" || disk.owner != "Pod/"+pod.Name {
		return liveDisk{}, fmt.Errorf("the disk %s is %s owned by %s, want Bound owned by its own pod %s", disk.name, disk.phase, disk.owner, "Pod/"+pod.Name)
	}
	return disk, nil
}

// awaitDiskGone waits until c's issue pod's disk is gone, which is what a pod's own ephemeral
// volume does when the pod goes: the kubelet's garbage collection follows the pod's ownership.
func (r *liveRig) awaitDiskGone(c *liveClaim) error {
	return r.poll(liveGoneLimit, "the disk "+issueDiskName(c.token)+" of issue "+c.issue+" to go with its pod", func() (bool, error) {
		_, found, err := r.issueDisk(c)
		return !found, err
	})
}

// markerLines reads the marker with `test ! -e || cat`, not a bare `cat`: a marker a poll reads
// before the stub agent has written to it does not exist yet, and a bare `cat`'s non-zero exit
// would reach the caller as an error — a poll returns on the first error, never retrying — rather
// than "not yet" (awaitRunning's own poll, the first launch of an issue: root at root-ready, root2
// and child2 at independent-provision, an orphan's first launch). It reads the container's view of
// the marker's path (containerHomePath), so a stopped role's marker reads too.
func (r *liveRig) markerLines(c *liveClaim) ([]string, error) {
	out, err := r.execPod(c, "sh", "-c", `test ! -e "$1" || cat -- "$1"`, "sh", containerHomePath(c.marker))
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// roleIncarnations is markerLines filtered to one role's own "role:incarnation" entries, with the
// role prefix stripped back to the bare incarnation: the marker is the issue pod's (SandboxName),
// shared by every resident role that has started in it, so its raw lines also hold every sibling
// role's own launches. They are that pod's alone — the disk the marker sits on died with any pod
// before it — so a check across a pod replacement reads the session table, never this.
func roleIncarnations(lines []string, role claim.Role) []string {
	prefix := string(role) + ":"
	var own []string
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			own = append(own, rest)
		}
	}
	return own
}

// recordSession puts c's recorded session in the session table, as `legion sessions import` copies
// one (ompsessions.Import), the first time it is asked and nothing after: the stub agent is no Oh
// My Pi and writes no session of its own, so the harness records it where the daemon's would be, as
// the machine recording a claim's session file does. It is called once c's pod runs (awaitRunning),
// so the row is written after that pod provisioned its workspace and before any later pod's: a
// resume in the same pod is told its workspace was not recreated, and a resume in a replacement pod
// is (launcher.WorkspaceRecreatedVariable, from Import's own mtime).
func (r *liveRig) recordSession(c *liveClaim) error {
	if c.recorded {
		return nil
	}
	content := `{"type":"session","version":3,"id":"` + string(c.token) + `"}` + "\n"
	if err := r.writeSession(c.session, content, time.Now()); err != nil {
		return fmt.Errorf("record %s's session %s in the table: %w", c.name, c.session, err)
	}
	c.recorded = true
	return nil
}

// writeSession puts content in the rig's PostgreSQL's session table at path, last written at
// written, as `legion sessions import` copies a session file (ompsessions.Import), through the URL
// file newKindLiveEnv wrote for the harness's own connection (sessionDSNLocalFile: the node's
// address on the kind network and the Service's node port, which this process dials).
func (r *liveRig) writeSession(path, content string, written time.Time) error {
	ctx, cancel := context.WithTimeout(r.ctx, time.Minute)
	defer cancel()
	conn, err := ompsessions.Connect(ctx, r.env.sessionDSNLocalFile)
	if err != nil {
		return fmt.Errorf("the rig's PostgreSQL (%s): %w", r.env.sessionDSNLocalFile, err)
	}
	defer conn.Close(context.Background())
	outcome, err := ompsessions.Import(ctx, conn, path, []byte(content), written)
	if err != nil {
		return err
	}
	if outcome != ompsessions.Copied {
		return fmt.Errorf("the session table already held %s (%s), want a fresh copy", path, outcome)
	}
	return nil
}

// ---- reads under the runtime identity ----------------------------------------------------------

func (r *liveRig) getSandbox(name string) (*sandbox, error) {
	ctx, cancel := call(r.ctx)
	defer cancel()
	u, err := r.dyn.Resource(sandboxGVR).Namespace(r.env.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return decodeSandbox(u)
}

func (r *liveRig) getPod(name string) (*corev1.Pod, error) {
	ctx, cancel := call(r.ctx)
	defer cancel()
	return r.kube.CoreV1().Pods(r.env.namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *liveRig) initLog(name string) (string, error) {
	ctx, cancel := call(r.ctx)
	defer cancel()
	raw, err := r.kube.CoreV1().Pods(r.env.namespace).GetLogs(name, &corev1.PodLogOptions{Container: initContainer}).DoRaw(ctx)
	return string(raw), err
}

// poll re-checks cond every two seconds until it holds, fails, or limit passes.
func (r *liveRig) poll(limit time.Duration, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(limit)
	for {
		ok, err := cond()
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", limit, what)
		}
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ---- launching, as the machine does -----------------------------------------------------------

// spec is the claim's launch: on its repository (claimRepository) at that repository's default
// branch.
func (r *liveRig) spec(c *liveClaim, resume string) runtime.SpawnSpec {
	repo, branch := r.claimRepository(c), r.defaultBranch
	if !c.repo.IsZero() {
		branch = r.secondDefaultBranch
	}
	return runtime.SpawnSpec{
		Claim: c.token, Project: r.env.claimProject, Tree: c.tree, Issue: c.issue, Role: c.role,
		Generation: c.gen, BootToken: c.bootToken, Env: map[string]string{"LEGION_E2E_MARKER": c.marker},
		Prompt: runtime.PromptParts{RolePromptPaths: []string{r.prompt}}, Repository: repo, DefaultBranch: branch,
		ResumeSessionFile: resume,
	}
}

// claimRepository is c's repository: LEGION_E2E_REPO, or, for a claim on another repository
// (liveClaim.repo), LEGION_E2E_REPO_SECOND_OWNER.
func (r *liveRig) claimRepository(c *liveClaim) ghrepo.Repository {
	if !c.repo.IsZero() {
		return c.repo
	}
	return r.env.repo
}

// spawn is a Spawn of the claim's next generation; armed false mints a token no hello registers.
func (r *liveRig) spawn(c *liveClaim, armed bool) (runtime.Locator, error) {
	if err := r.startRuntimeOnce(); err != nil {
		return runtime.Locator{}, err
	}
	c.bootToken, c.gen = r.reg.mint(c.token, armed)
	loc, err := r.rt.Spawn(r.ctx, r.spec(c, ""))
	if err != nil {
		return runtime.Locator{}, err
	}
	c.loc, c.last, c.state = &loc, &loc, stateRunning
	return loc, nil
}

// resume is a Resume of the claim's next generation from file, with its last locator as prev.
func (r *liveRig) resume(c *liveClaim, file string) (runtime.Locator, error) {
	if err := r.startRuntimeOnce(); err != nil {
		return runtime.Locator{}, err
	}
	c.bootToken, c.gen = r.reg.mint(c.token, true)
	loc, err := r.rt.Resume(r.ctx, c.last, r.spec(c, file))
	if err != nil {
		return runtime.Locator{}, err
	}
	c.loc, c.last, c.state = &loc, &loc, stateRunning
	return loc, nil
}

// resumeWithPlugins is resume with spec.Plugins set, for a launch that installs a stale-plugin
// pair into a kept pod (stale-plugin-install) rather than loading the image's own copies.
func (r *liveRig) resumeWithPlugins(c *liveClaim, file string, plugins *runtime.PluginInstall) (runtime.Locator, error) {
	if err := r.startRuntimeOnce(); err != nil {
		return runtime.Locator{}, err
	}
	c.bootToken, c.gen = r.reg.mint(c.token, true)
	spec := r.spec(c, file)
	spec.Plugins = plugins
	loc, err := r.rt.Resume(r.ctx, c.last, spec)
	if err != nil {
		return runtime.Locator{}, err
	}
	c.loc, c.last, c.state = &loc, &loc, stateRunning
	return loc, nil
}

// awaitRunning waits for the claim's current pod to run with its Sandbox Ready, then for its
// shim's hello at the current generation, then for the stub agent's own marker line and setup
// record, and records the claim's session in the table (recordSession) once all of it holds. The
// hello only proves the shim dialed in before spawning its child (shim.go:204/317);
// checkSetupRecord holds the setup line before the agent line and its cwd, uid and parent pid for
// every successful role launch. A pod that runs and never says hello within one boot interval is a
// network-path failure, named as one.
func (r *liveRig) awaitRunning(c *liveClaim, since time.Time) (registration, error) {
	name := SandboxName(c.token)
	var pod *corev1.Pod
	err := r.poll(liveRunningLimit, "pod "+name+" to run with its Sandbox Ready", func() (bool, error) {
		p, err := r.getPod(name)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if string(p.UID) != c.loc.Sandbox.PodUID {
			return false, fmt.Errorf("pod %s is uid %s, not the pod uid %s the launch returned in process %s", name, p.UID, c.loc.Sandbox.PodUID, c.loc.Incarnation)
		}
		if terminal(p) {
			log, _ := r.initLog(name)
			return false, fmt.Errorf("pod %s (uid %s) ended %s before it ran: %s; init log: %s", name, p.UID, p.Status.Phase, containerStates(p), strings.TrimSpace(log))
		}
		s, err := r.getSandbox(name)
		if err != nil {
			return false, err
		}
		ready := s.condition(conditionReady)
		pod = p
		return p.Status.Phase == corev1.PodRunning && ready != nil && ready.Status == metav1.ConditionTrue, nil
	})
	if err != nil {
		return registration{}, err
	}
	reg, ok := r.reg.await(c.token, c.gen, since, liveBootTimeout)
	if !ok {
		return registration{}, r.networkPathFailure(pod)
	}
	wrote := string(c.role) + ":" + c.loc.Incarnation
	if err := r.poll(liveBootTimeout, "marker "+c.marker+" to hold "+wrote, func() (bool, error) {
		lines, err := r.markerLines(c)
		if err != nil {
			return false, err
		}
		return slices.Contains(lines, wrote), nil
	}); err != nil {
		return registration{}, err
	}
	if err := r.checkSetupRecord(c); err != nil {
		return registration{}, err
	}
	if err := r.recordSession(c); err != nil {
		return registration{}, err
	}
	return reg, nil
}

// awaitHelloAgain is c's shim saying hello, since the runtime and listener were replaced at since,
// with its current generation's boot token: a live claim reconnecting to a new runtime.
func (r *liveRig) awaitHelloAgain(c *liveClaim, since time.Time) (registration, error) {
	reg, ok := r.reg.await(c.token, c.gen, since, 2*time.Minute)
	if !ok || reg.hash != tokenHash(c.bootToken) {
		return reg, fmt.Errorf("%s's shim did not say hello again with its generation-%d token", c.name, c.gen)
	}
	return reg, nil
}

// networkPathFailure names what a pod that runs but never says hello points at: the address and
// port it dials, the kind network's gateway, where this process listens.
func (r *liveRig) networkPathFailure(pod *corev1.Pod) error {
	return fmt.Errorf("network path: pod %s (uid %s) runs on node %s but no hello reached %s within %s; the node reaches this process at the kind network's gateway",
		pod.Name, pod.UID, pod.Spec.NodeName, r.streamAddress(), liveBootTimeout)
}

// suspend is a role-process stop. It leaves the issue pod in place; its launcher reports the
// child exit, so Runtime.Suspend returns only after that role's process is gone.
func (r *liveRig) suspend(c *liveClaim) error {
	if err := r.rt.Suspend(r.ctx, *c.loc); err != nil {
		return err
	}
	c.loc, c.state = nil, stateSuspended
	return nil
}

// ensureRunning and ensureSuspended put a claim where a check starts from: nothing to do where an
// earlier check of its group left it there, and the same runtime calls where none did. A claim that
// has run before comes back from its recorded session, which the table holds whatever became of
// the pod that first ran it.
func (r *liveRig) ensureRunning(c *liveClaim) error {
	if c.state == stateRunning {
		return nil
	}
	since := time.Now()
	var err error
	if c.state == stateNone {
		_, err = r.spawn(c, true)
	} else {
		_, err = r.resume(c, c.session)
	}
	if err == nil {
		_, err = r.awaitRunning(c, since)
	}
	if err != nil {
		return fmt.Errorf("setup: %s running: %w", c.name, err)
	}
	note("runtime", "setup: %s running at generation %d", c.name, c.gen)
	return nil
}

func (r *liveRig) ensureSuspended(c *liveClaim) error {
	if c.state == stateSuspended {
		return nil
	}
	if err := r.ensureRunning(c); err != nil {
		return err
	}
	if err := r.suspend(c); err != nil {
		return fmt.Errorf("setup: suspend %s: %w", c.name, err)
	}
	note("runtime", "setup: %s suspended", c.name)
	return nil
}

// mapRunRepository is what root-ready leaves every later launch on the run's repository, which a
// group that does not hold root-ready redoes first: the runtime running, and the repository in the
// run's mapping, as Dispatch's mapping names a repository before any issue of it launches.
func (r *liveRig) mapRunRepository() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	r.mapRepository(r.env.repo)
	note("runtime", "setup: %s mapped", r.env.repo)
	return nil
}

// runFirstIssue is what root-ready and worker-shares-issue-pod leave the checks of the run's first
// issue after them, which the roles group redoes first: mapRunRepository's, then the root running,
// whose launch makes the run's repository's cache, and the worker running beside it in its pod.
func (r *liveRig) runFirstIssue() error {
	if err := r.mapRunRepository(); err != nil {
		return err
	}
	for _, name := range []string{"root", "worker"} {
		if err := r.ensureRunning(r.claim(name)); err != nil {
			return err
		}
	}
	return nil
}

func containerStates(p *corev1.Pod) string {
	var parts []string
	for _, statuses := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, s := range statuses {
			switch {
			case s.State.Terminated != nil:
				parts = append(parts, fmt.Sprintf("%s terminated %s exit %d", s.Name, s.State.Terminated.Reason, s.State.Terminated.ExitCode))
			case s.State.Waiting != nil:
				parts = append(parts, fmt.Sprintf("%s waiting %s", s.Name, s.State.Waiting.Reason))
			case s.State.Running != nil:
				parts = append(parts, s.Name+" running")
			}
		}
	}
	return strings.Join(parts, "; ")
}

func short(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// live are the claims with a running process, in the roster's order.
func (r *liveRig) live() []*liveClaim {
	var out []*liveClaim
	for _, c := range r.claims {
		if c.state == stateRunning {
			out = append(out, c)
		}
	}
	return out
}

// known is every claim the daemon has not retired, but for skip.
func (r *liveRig) known(skip *liveClaim) []runtime.Known {
	var out []runtime.Known
	for _, c := range r.claims {
		if c == skip || c.state == stateNone || c.state == stateReleased {
			continue
		}
		out = append(out, runtime.Known{Claim: c.token, Locator: c.loc})
	}
	return out
}
