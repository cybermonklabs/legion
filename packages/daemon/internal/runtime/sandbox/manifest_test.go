package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

var updateGolden = flag.Bool("update", false, "rewrite the manifest goldens this package pins")

// goldenOptions are production's settings: the budgets a golden's grace is read from and the
// Legion priority class, over testOptions' reservations at the daemon's defaults.
func goldenOptions() Options {
	opts := testOptions()
	opts.BootTimeout = 120 * time.Second
	opts.TerminationGrace = 30 * time.Second
	opts.Scheduling = Scheduling{PriorityClass: "legion"}
	return opts
}

// reservations are the daemon's default reservations (config.DefaultResources) as the daemon
// translates them for the runtime (internal/daemon/kubernetes.go, requirements): an issue pod's
// and the controller's pod's, its cpu and memory requested as the defaults set them — each equal to
// its limit — and its ephemeral storage the limit over the smaller request.
func reservations() PodResources {
	defaults := config.DefaultResources()
	return PodResources{Issue: requirementsOf(defaults.Issue), Controller: requirementsOf(defaults.Controller)}
}

// probeReservation is the image probe's own reservation as the daemon hands it
// (internal/daemon/kubernetes.go, probeReservation), translated the same way: 500m and 1Gi, with a
// 5Gi disk bound over a 1Gi request — not a pod's share, written here as data as the defaults are.
func probeReservation() corev1.ResourceRequirements {
	return requirementsOf(config.Reservation{CPU: "500m", CPURequest: "500m", Memory: "1Gi", MemoryRequest: "1Gi", EphemeralStorage: "5Gi", EphemeralStorageRequest: "1Gi"})
}

// requirementsOf is reservation as a container's requirements, translated as the daemon's
// requirements translates it.
func requirementsOf(reservation config.Reservation) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(reservation.CPURequest), corev1.ResourceMemory: resource.MustParse(reservation.MemoryRequest),
			corev1.ResourceEphemeralStorage: resource.MustParse(reservation.EphemeralStorageRequest),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(reservation.CPU), corev1.ResourceMemory: resource.MustParse(reservation.Memory),
			corev1.ResourceEphemeralStorage: resource.MustParse(reservation.EphemeralStorage),
		},
	}
}

// podQOS is the QoS class the API server records for a pod of spec (status.qosClass), by the
// kubelet's rule over cpu and memory alone: no request or limit anywhere is BestEffort; every
// container, init containers included, limited in both and the pod's summed requests equal to its
// summed limits is Guaranteed; anything else is Burstable.
func podQOS(spec corev1.PodSpec) corev1.PodQOSClass {
	requests, limits := corev1.ResourceList{}, corev1.ResourceList{}
	guaranteed := true
	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		limited := 0
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			if request, ok := c.Resources.Requests[name]; ok && !request.IsZero() {
				sum := requests[name]
				sum.Add(request)
				requests[name] = sum
			}
			if limit, ok := c.Resources.Limits[name]; ok && !limit.IsZero() {
				limited++
				sum := limits[name]
				sum.Add(limit)
				limits[name] = sum
			}
		}
		if limited != 2 {
			guaranteed = false
		}
	}
	if len(requests) == 0 && len(limits) == 0 {
		return corev1.PodQOSBestEffort
	}
	if guaranteed && len(requests) == len(limits) {
		for name, request := range requests {
			if limit, ok := limits[name]; !ok || limit.Cmp(request) != 0 {
				guaranteed = false
			}
		}
		if guaranteed {
			return corev1.PodQOSGuaranteed
		}
	}
	return corev1.PodQOSBurstable
}

// Every container of every pod the runtime builds at the daemon's default reservations reserves cpu
// and memory and bursts past neither: each has a non-zero request for both, equal to its limit,
// which is the kubelet's rule for the Guaranteed class — judged over every container of the pod,
// the init containers included — and each is bounded on the node's disk, an ephemeral-storage
// request under its limit. An issue pod's one container and its two init containers carry the
// issue pod's reservation, whichever role's launch creates it; the controller's
// pod carries the controller's on its one container; and the image probe's one container carries
// the probe's own, which the daemon hands it. A deployment with no `resources` block therefore runs
// only Guaranteed pods.
func TestEveryContainerOfEveryPodIsGuaranteed(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := reservations()
	probe := r.probeManifest(probeSandboxName, ImageProbe{Contract: 3, Resources: probeReservation(), RoleReferences: testRoleReferences}, time.Now())
	for name, tc := range map[string]struct {
		pod        corev1.PodSpec
		want       corev1.ResourceRequirements
		containers int
	}{
		"root":       {podOf(t, r, rootSpec(t)), want.Issue, 3},
		"worker":     {podOf(t, r, workerSpec(t)), want.Issue, 3},
		"controller": {podOf(t, r, controllerSpec(t)), want.Controller, 1},
		"probe":      {probe.Spec.PodTemplate.Spec, probeReservation(), 1},
	} {
		t.Run(name, func(t *testing.T) {
			if qos := podQOS(tc.pod); qos != corev1.PodQOSGuaranteed {
				t.Errorf("the pod is %s, want %s", qos, corev1.PodQOSGuaranteed)
			}
			containers := slices.Concat(tc.pod.InitContainers, tc.pod.Containers)
			if len(containers) != tc.containers {
				t.Fatalf("the pod runs %d containers, want %d", len(containers), tc.containers)
			}
			for _, c := range containers {
				if !reflect.DeepEqual(c.Resources, tc.want) {
					t.Errorf("container %s carries %+v, want the pod's reservation %+v", c.Name, c.Resources, tc.want)
				}
				for _, kind := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
					request, limit := c.Resources.Requests[kind], c.Resources.Limits[kind]
					if request.Sign() <= 0 {
						t.Errorf("container %s requests %s %s, want a positive reservation", c.Name, kind, request.String())
					}
					if request.Cmp(limit) != 0 {
						t.Errorf("container %s requests %s %s but is limited to %s; the pod is Guaranteed only when they are equal", c.Name, kind, request.String(), limit.String())
					}
				}
				if request, limit := c.Resources.Requests[corev1.ResourceEphemeralStorage], c.Resources.Limits[corev1.ResourceEphemeralStorage]; request.Sign() <= 0 || request.Cmp(limit) > 0 {
					t.Errorf("container %s requests ephemeral-storage %s under a limit of %s, want a positive request no greater than the limit", c.Name, request.String(), limit.String())
				}
			}
		})
	}
}

// An issue-pod reservation that requests less memory than its limit — 8Gi under 32Gi — is what
// every container of every issue pod carries, the init containers included, whichever role's launch
// created the pod, and every issue pod is Burstable. The controller's pod, sized by its own
// reservation, and the image probe's, sized by the probe's, stay Guaranteed.
func TestARequestBelowItsLimitRendersABurstablePod(t *testing.T) {
	opts := goldenOptions()
	bursting := config.Reservation{CPU: "3", CPURequest: "3", Memory: "32Gi", MemoryRequest: "8Gi", EphemeralStorage: "80Gi", EphemeralStorageRequest: "6Gi"}
	opts.Resources.Issue = requirementsOf(bursting)
	r, err := configure(opts)
	if err != nil {
		t.Fatalf("configure with the issue pod requesting 8Gi under a 32Gi limit: %v", err)
	}
	want := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("8Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("6Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("32Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("80Gi")},
	}
	for name, spec := range map[string]runtime.SpawnSpec{"root": rootSpec(t), "implementer": childSpec(t)} {
		t.Run(name, func(t *testing.T) {
			pod := podOf(t, r, spec)
			if qos := podQOS(pod); qos != corev1.PodQOSBurstable {
				t.Errorf("the issue pod is %s, want %s", qos, corev1.PodQOSBurstable)
			}
			if len(pod.InitContainers) == 0 {
				t.Fatal("the issue pod has no init containers")
			}
			for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
				if !reflect.DeepEqual(c.Resources, want) {
					t.Errorf("container %s carries %+v, want the issue pod's %+v", c.Name, c.Resources, want)
				}
			}
		})
	}
	if qos := podQOS(podOf(t, r, controllerSpec(t))); qos != corev1.PodQOSGuaranteed {
		t.Errorf("the controller's pod is %s, want %s", qos, corev1.PodQOSGuaranteed)
	}
	probe := r.probeManifest(probeSandboxName, ImageProbe{Contract: 3, Resources: probeReservation(), RoleReferences: testRoleReferences}, time.Now())
	if qos := podQOS(probe.Spec.PodTemplate.Spec); qos != corev1.PodQOSGuaranteed {
		t.Errorf("the probe pod is %s, want %s", qos, corev1.PodQOSGuaranteed)
	}
}

// resumeSession is a session file Oh My Pi recorded in a pod.
const resumeSession = ompSessionsDir + "/--legion-workspaces-sjawhar-legion-smoke-legion-208--/2026-09-23T12-00-00-000Z_0198.jsonl"

// manifestCases are the Sandboxes the goldens pin: the root; a worker; a resume; the root enrolled
// with the secrets broker; the root of a microVM project whose block names the metadata endpoint,
// and the same root with keep_volume on, whose Sandbox owns its kept volume; the project
// controller's pod, launched fresh and resuming, and launched in that microVM project; and a worker
// on Claude Code, a case of its own so every Oh My Pi one keeps pinning that pod unchanged by the
// Claude backend. The Claude worker's Sandbox is the Oh My Pi worker's byte for byte: its lane is
// the launcher's start command (launcherCommand), which
// TestAClaudeLaunchCarriesItsMCPDocumentAsALauncherFile holds, so a pod change made for Claude Code
// alone shows in its golden and leaves worker's.
func manifestCases(t *testing.T) map[string]manifestCase {
	resume := workerSpec(t)
	resume.Generation, resume.BootToken, resume.ResumeSessionFile = 2, "boot-g2", resumeSession
	controllerResume := controllerSpec(t)
	controllerResume.Generation, controllerResume.BootToken = 2, "boot-g2"
	controllerResume.ResumeSessionFile = controllerSession
	return map[string]manifestCase{
		"root":              {spec: rootSpec(t)},
		"worker":            {spec: workerSpec(t)},
		"claude-worker":     {spec: claudeWorkerSpec(t)},
		"resume":            {spec: resume},
		"controller":        {spec: controllerSpec(t)},
		"controller-resume": {spec: controllerResume},
		// controller-microvm is the controller's pod of root-microvm's project, whose type is
		// microvm: the pod runs in a microVM and serves the metadata endpoint, on its Sandbox's disk
		// of runtime.kubernetes.storage_class (TestTheControllerPodRunsInTheProjectsSandbox).
		"controller-microvm": {spec: controllerSpec(t), sandbox: SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()}},
		"root-enrolled":      {spec: rootSpec(t), agentSecrets: rootEnrolled()},
		"root-microvm":       {spec: rootSpec(t), sandbox: SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()}},
		// root-microvm-kept is root-microvm with keep_volume on: the one case whose issue Sandbox
		// owns a claim template, its kept volume (TestOnlyAMicroVMPodKeepsItsVolume).
		"root-microvm-kept": {spec: rootSpec(t), sandbox: SandboxConfig{Type: SandboxMicroVM, MicroVM: keptMicroVM()}},
		// root-exchange-identity is root-enrolled with agent_secrets.exchange_audience set: the
		// one case whose pod carries an identity the broker's exchange trades
		// (TestAnEnrolledPodCarriesAnIdentityItCanTrade).
		"root-exchange-identity": {spec: rootSpec(t), agentSecrets: exchangeEnrolled()},
	}
}

// manifestCase is one Sandbox the goldens pin: its launch's spec, and what the case sets on the
// runtime beside goldenOptions — agentSecrets, the enrollment, for root-enrolled and
// root-exchange-identity alone, and sandbox, the sandbox block, for root-microvm,
// root-microvm-kept and controller-microvm alone. The tests that build every case's manifest on one
// shared runtime apply each case to it first (apply), so every other case runs unenrolled and in
// gVisor.
type manifestCase struct {
	spec         runtime.SpawnSpec
	agentSecrets *AgentSecrets
	sandbox      SandboxConfig
}

// apply sets on r what the case configures beside goldenOptions, and the pod shapes that sandbox
// block gives r, which configure encodes once from the block it is built with (podShapes).
func (c manifestCase) apply(r *Runtime) {
	r.agentSecrets, r.sandboxConfig = c.agentSecrets, c.sandbox
	r.podShapes = podShapes(r)
}

// manifestOf is the Sandbox a launch of spec leaves running, as the API server holds it: the one
// the runtime creates, with the relaunch's Running patch applied — the launch's pod template, and
// the Running mode.
func manifestOf(t *testing.T, r *Runtime, spec runtime.SpawnSpec) any {
	t.Helper()
	l := launchOf(t, r, spec)
	s := r.sandboxManifest(l)
	s.Spec.PodTemplate, s.Spec.OperatingMode = r.podTemplate(l), modeRunning
	u, err := encodeSandbox(s)
	if err != nil {
		t.Fatal(err)
	}
	return wire(t, u.Object)
}

// The goldens pin every byte of the Sandboxes a launch creates. A pod contract change shows
// here as a diff to read, never as a surprise in a cluster.
func TestManifestGoldens(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			tc.apply(r)
			checkGolden(t, name, manifestOf(t, r, tc.spec))
		})
	}
}

// The root issue pod under the Go live harnesses' operator pod (deploy/kubernetes/operator-route/
// pod.yml), which Stage 4b's pod shape (shape_problems in scripts/e2e/stage4b-sandbox-tree.sh)
// judges: the golden is the pod a live run's daemon builds from that fixture, so the shape's rules
// are run against what the renderer puts where (scripts/e2e/lib/stage4b-pods.test.ts).
func TestOperatorRouteGolden(t *testing.T) {
	opts := goldenOptions()
	opts.Pod = operatorRouteFixture(t)
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "root-operator-route", manifestOf(t, r, rootSpec(t)))
}

// checkGolden compares manifest, encoded as the goldens are, with testdata/golden/<name>.json, or
// writes it there under -update.
func checkGolden(t *testing.T, name string, manifest any) {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		t.Fatal(err)
	}
	encoded := buffer.Bytes()
	path := filepath.Join("testdata", "golden", name+".json")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/runtime/sandbox/ -update`): %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s is stale\n got: %s\nwant: %s", path, encoded, want)
	}
}

// Every issue pod and the project controller's pod carry the two stop-eviction annotations
// unconditionally, a microVM pod's as a gVisor pod's (the image probe carries only Karpenter's,
// TestTheProbePodCarriesNoGKEExtendedDurationAnnotation): Karpenter's own, which protects a pod on
// an AWS cluster, and the Cluster Autoscaler's, which GKE Autopilot honours; GKE ignores
// Karpenter's, so a pod on Autopilot without the second can be evicted mid-turn by consolidation or
// a node upgrade. Each carries, beside them, the configuration it was built from, its own kind's
// in its own sandbox (podShapeAnnotation): the one record an update round reads a pod's drift from
// (podShapeDrift), written here and nowhere else; and, under a configured leak check setting, the
// checker it was built under (leakCheckerAnnotation), which a launch keeping the pod tells its role.
func TestEveryIssueAndControllerPodIsProtectedFromAutoscalerEviction(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			tc.apply(r)
			l, err := r.prepare(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				doNotDisruptAnnotation: "true",
				safeToEvictAnnotation:  "false",
				podShapeAnnotation:     podShape(r, l.kind, l.microVM),
				leakCheckerAnnotation:  testLeakCheck.ManifestSHA256,
			}
			if got := r.podTemplate(l).Metadata.Annotations; !maps.Equal(got, want) {
				t.Errorf("pod annotations = %v, want exactly %v", got, want)
			}
		})
	}
}

// Every field of every manifest, podTemplate.spec included, is one the v1.0.3 CRD declares with
// that type, a microVM pod's placement, label and projected token included: the API server prunes
// an unknown field without a word, so this walk is the only place a misspelled or misplaced one
// shows (decision 9, N18).
func TestManifestMatchesTheSandboxCRD(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	schema := sandboxSchema(t)
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			tc.apply(r)
			if found := schemaViolations(manifestOf(t, r, tc.spec), schema, ""); len(found) > 0 {
				t.Fatalf("the manifest has fields the CRD does not declare as sent:\n%s", strings.Join(found, "\n"))
			}
		})
	}
}

// What the operator adds to a pod — a Secret, a ConfigMap by sub_path, a projected token, a
// variable, an account, and the providers Secret's keys — is sent in fields the CRD declares, in
// the worker's Sandbox and the probe's alike, so the API server keeps every one of them.
func TestTheOperatorsPodIsSentInFieldsTheSandboxCRDDeclares(t *testing.T) {
	opts := goldenOptions()
	opts.Pod = operatorPod()
	opts.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "anthropic"}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := encodeProbe(r.probeManifest("legion-probe", ImageProbe{Contract: 5, RoleReferences: testRoleReferences}, time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	schema := sandboxSchema(t)
	for name, manifest := range map[string]any{"worker": manifestOf(t, r, workerSpec(t)), "probe": wire(t, probe.Object)} {
		if found := schemaViolations(manifest, schema, ""); len(found) > 0 {
			t.Errorf("the %s manifest has fields the CRD does not declare as sent:\n%s", name, strings.Join(found, "\n"))
		}
	}
}

// podOf is the pod template a launch of spec runs.
func podOf(t *testing.T, r *Runtime, spec runtime.SpawnSpec) corev1.PodSpec {
	t.Helper()
	l := launchOf(t, r, spec)
	return r.podTemplate(l).Spec
}

// workerContainer is the container workerSpec's tester runs in: its issue pod's one container.
const workerContainer = "roles"

// workerOf is the worker-shim process a launch of spec starts: its pod's one container as the pod
// runs it (mounts, kubelet-resolved environment), with the launcher start command's argv as
// Command and its plain environment appended. That is the process's whole environment: the
// launcher passes its own on, then the start command's.
func workerOf(t *testing.T, r *Runtime, spec runtime.SpawnSpec) corev1.Container {
	t.Helper()
	l := launchOf(t, r, spec)
	c := containerNamed(t, r.podTemplate(l).Spec, l.kind.container())
	start := launcherCommand(l, r)
	c.Command = start.Argv
	for _, entry := range start.Env {
		name, value, _ := strings.Cut(entry, "=")
		c.Env = append(c.Env, corev1.EnvVar{Name: name, Value: value})
	}
	return c
}

func envOf(container corev1.Container) map[string]string {
	env := map[string]string{}
	for _, v := range container.Env {
		env[v.Name] = v.Value
	}
	return env
}

// containerNamed is the pod's init or main container of that name.
func containerNamed(t *testing.T, pod corev1.PodSpec, name string) corev1.Container {
	t.Helper()
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the pod has no container %s", name)
	return corev1.Container{}
}

// podDiskSource is where an issue pod's disk, its issue volume, comes from, either source
// issuePod.disk gives: a generic ephemeral volume of the pod's own, or the claim of the kept
// volume its Sandbox owns.
func podDiskSource(t *testing.T, pod corev1.PodSpec) corev1.VolumeSource {
	t.Helper()
	at := slices.IndexFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == issueVolume })
	if at < 0 {
		t.Fatalf("the pod's volumes %+v carry no %s volume", pod.Volumes, issueVolume)
	}
	return pod.Volumes[at].VolumeSource
}

// podDisk is the claim template of an issue pod's own disk: its issue volume is a generic
// ephemeral volume, which Kubernetes creates with the pod and deletes with the pod, never a claim
// that outlived an earlier pod (INFRA-80).
func podDisk(t *testing.T, pod corev1.PodSpec) *corev1.PersistentVolumeClaimTemplate {
	t.Helper()
	disk := podDiskSource(t, pod)
	if disk.PersistentVolumeClaim != nil {
		t.Fatalf("the pod's %s volume is the claim %s, want an ephemeral volume of its own", issueVolume, disk.PersistentVolumeClaim.ClaimName)
	}
	if disk.Ephemeral == nil || disk.Ephemeral.VolumeClaimTemplate == nil {
		t.Fatalf("the pod's %s volume is %+v, want an ephemeral volume with a claim template", issueVolume, disk)
	}
	return disk.Ephemeral.VolumeClaimTemplate
}

// mountHolding is the deepest of c's mounts at or above path, the one path lands on, or nil when
// none holds it.
func mountHolding(c corev1.Container, path string) *corev1.VolumeMount {
	var holding *corev1.VolumeMount
	for i, m := range c.VolumeMounts {
		if (path == m.MountPath || strings.HasPrefix(path, m.MountPath+"/")) && (holding == nil || len(m.MountPath) > len(holding.MountPath)) {
			holding = &c.VolumeMounts[i]
		}
	}
	return holding
}

// PI_SHELL_PREFIX is a shell command Oh My Pi's bash tool runs before each command, in tmux's form
// over the pod's one directory of its own — legion's in the image; no directory of the tree volume,
// since the agent's gh is the image's — never a path list (P3).
func TestPIShellPrefixIsTmuxsFormOverThePodsDirectories(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(workerOf(t, r, workerSpec(t)))
	got := kubeExpand(env["PI_SHELL_PREFIX"], env)
	want := shellprefix.For("/opt/legion/bin")
	if got != want {
		t.Fatalf("PI_SHELL_PREFIX\n got: %s\nwant: %s", got, want)
	}
}

// Every git a role process runs answers https://github.com from the role's own helper, outside
// any clone too — a clone the setup command makes, Oh My Pi's marketplace clone among them: the
// GIT_CONFIG pairs the role is told reset every helper a configuration file names for github.com
// (a planted global one here) and set `legion git-credential` with the repository's path, whose
// owner picks the token. Another scheme, host or port is left to the configuration files. A real
// git runs `git credential fill` in a directory that is no repository, with a stand-in `legion`
// first on PATH, for a workflow role and the controller alike.
func TestEveryRolesGitAsksItsOwnHelperForGitHubOutsideAnyClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git in this environment")
	}
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// The stand-in answers `git-credential get` with a token naming the owner of git's path, and
	// nothing without a path, as `legion git-credential` does (cmd/legion/gitcredential.go).
	legion := "#!/bin/sh\n[ \"$1 $2\" = 'git-credential get' ] || exit 0\n" +
		"while read -r line; do case $line in path=*) p=${line#path=} ;; esac; done\n" +
		"[ -n \"$p\" ] || exit 1\necho username=x-access-token\necho \"password=role-token-for-${p%%/*}\"\n"
	if err := os.WriteFile(filepath.Join(bin, "legion"), []byte(legion), 0o700); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(global, []byte("[credential]\n\thelper = \"!f() { echo username=planted; echo password=planted; }; f\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "no-repository")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]runtime.SpawnSpec{"a workflow role": workerSpec(t), "the controller": controllerSpec(t)} {
		t.Run(name, func(t *testing.T) {
			var gitEnv []string
			for name, value := range envOf(workerOf(t, r, spec)) {
				if strings.HasPrefix(name, "GIT_CONFIG_") {
					gitEnv = append(gitEnv, name+"="+value)
				}
			}
			if len(gitEnv) == 0 {
				t.Fatal("the role is told no GIT_CONFIG pairs")
			}
			for _, tc := range []struct{ url, want string }{
				{"https://github.com/acme/agent-skills.git", "role-token-for-acme"},
				{"https://github.com/sjawhar/legion-smoke", "role-token-for-sjawhar"},
				{"http://github.com/acme/agent-skills.git", "planted"},
				{"https://github.com:8443/acme/agent-skills.git", "planted"},
				{"https://github.com.evil.example/acme/agent-skills.git", "planted"},
				{"https://evil.example/acme/agent-skills.git", "planted"},
			} {
				fill := exec.Command("git", "credential", "fill")
				fill.Dir = outside
				fill.Stdin = strings.NewReader("url=" + tc.url + "\n\n")
				fill.Env = fixtureenv.Isolated(t.TempDir())
				fill.Env = append(fill.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
					"GIT_CONFIG_GLOBAL="+global, "GIT_CONFIG_NOSYSTEM=1", "GIT_CEILING_DIRECTORIES="+dir, "GIT_TERMINAL_PROMPT=0")
				fill.Env = append(fill.Env, gitEnv...)
				out, err := fill.Output()
				got := ""
				for _, line := range strings.Split(string(out), "\n") {
					if value, ok := strings.CutPrefix(line, "password="); ok {
						got = value
					}
				}
				if err != nil || got != tc.want {
					t.Errorf("%s: password %q (%v), want %q", tc.url, got, err, tc.want)
				}
			}
		})
	}
}

// The init containers run where the provisioning Secret is mounted, or on the issue volume every
// agent of the issue can write, so nothing either executes may come from the issue volume: each
// one's PATH names the image's directories only, and neither is told a tool path (#1258 deep
// review, finding 3).
func TestTheInitContainersPathNamesNoIssueVolumeDirectory(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, init := range podOf(t, r, workerSpec(t)).InitContainers {
		env := envOf(init)
		path, ok := env["PATH"]
		if !ok {
			t.Fatalf("%s's PATH is left to the image; it must be stated", init.Name)
		}
		for _, dir := range filepath.SplitList(path) {
			if dir == TreeRoot || strings.HasPrefix(dir, TreeRoot+"/") {
				t.Errorf("%s's PATH names %s, on the issue volume", init.Name, dir)
			}
		}
		for name := range env {
			if strings.HasPrefix(name, "LEGION_") && strings.HasSuffix(name, "_PATH") {
				t.Errorf("%s is told %s; it resolves its tools from the image's PATH", init.Name, name)
			}
		}
	}
}

// jj keeps a repository's `--repo` configuration under $XDG_CONFIG_HOME, so what workspace-init
// sets there reaches the agent's jj only when both containers have one config home, on a volume
// both mount (#1258 deep review, finding 4); each slot's role-init seeds the slot's own config
// home from it (launcher's TestARoleRunsInNamespacesOfItsOwn). It is in memory, so every
// pod's starts empty and nothing an agent wrote reaches workspace-init's jj. The config home is
// the one home the two agree on: a role's cache and data homes are the issue's scratch's on the
// issue's volume and its state home a directory per slot there, named for the agent that runs in
// it (TestTheIssuesRolesShareOneScratchAndOneGoCachePair,
// TestEachRolesAgentIsToldAStateHomeOfItsOwn), while the setup containers keep the image's home
// directory ones, so nothing of the issue's scratch reaches a container that holds the
// provisioning token or provisions the issue's clone.
func TestBothContainersShareOneInMemoryXDGConfigHome(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	pod := podOf(t, r, workerSpec(t))
	init, main := containerNamed(t, pod, initContainer), workerOf(t, r, workerSpec(t))
	configHome := envOf(main)["XDG_CONFIG_HOME"]
	if configHome == "" || configHome != envOf(init)["XDG_CONFIG_HOME"] {
		t.Errorf("XDG_CONFIG_HOME: init %q, main %q; the two containers must agree",
			envOf(init)["XDG_CONFIG_HOME"], configHome)
	}
	// The rendered value is the path role-init binds the role's own config home at, under the
	// image's home: a value that moved off it would leave every role's XDG_CONFIG_HOME pointing
	// where role-init binds nothing (launcher.Home, launcher.ConfigHomeDir).
	if want := launcher.Home + "/" + launcher.ConfigHomeDir; configHome != want {
		t.Errorf("XDG_CONFIG_HOME is rendered %q, want %q, the path each role's role-init binds its own config home at", configHome, want)
	}
	for name, want := range map[string]string{
		"XDG_CACHE_HOME": podHome + "/.cache",
		"XDG_DATA_HOME":  podHome + "/.local/share",
		"XDG_STATE_HOME": podHome + "/.local/state",
	} {
		if got := envOf(init)[name]; got != want {
			t.Errorf("%s is told %s=%q, want the image's home directory's %q", initContainer, name, got, want)
		}
	}
	for _, setup := range pod.InitContainers {
		for _, v := range setup.Env {
			if v.Value == scratchRoot || strings.HasPrefix(v.Value, scratchRoot+"/") {
				t.Errorf("%s is told %s=%q, under the roles' scratch; a setup container keeps nothing there",
					setup.Name, v.Name, v.Value)
			}
		}
	}
	// The issue pod's roles container holds the config home under HomeMountsDir, which each
	// role's role-init copies into the role's own config home at XDG_CONFIG_HOME (homeMountsView).
	volumeAt := func(c corev1.Container, path string) string {
		for _, mount := range c.VolumeMounts {
			if mount.MountPath == path {
				return mount.Name
			}
		}
		return ""
	}
	shared := volumeAt(init, configHome)
	if shared == "" || shared != volumeAt(main, containerHomePath(configHome)) {
		t.Fatalf("XDG_CONFIG_HOME %s is mounted from %q in the init container and %q in the main one", configHome, shared, volumeAt(main, containerHomePath(configHome)))
	}
	for _, volume := range pod.Volumes {
		if volume.Name == shared && (volume.EmptyDir == nil || volume.EmptyDir.Medium != corev1.StorageMediumMemory) {
			t.Fatalf("the config home's volume %q is not an in-memory emptyDir: %+v", shared, volume.VolumeSource)
		}
	}
}

// The issue's roles share one clone, and any of them can write its `.git/config` and hooks, or a
// git configuration in any config home it reaches. So every role's git is told, at command scope,
// which git reads after every configuration file, to start no fsmonitor program and no hook: a
// real git run with exactly the role's GIT_CONFIG pairs, in a clone whose `.git/config` and global
// configuration name a recording fsmonitor and a recording hooks directory, runs neither through
// `git status` and `git commit`. The same git without the pairs runs both, so the plant is live.
// These are the two settings the pairs turn off; others a role can plant still run.
func TestEveryRolesGitStartsNoPlantedFsmonitorOrHook(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("this test runs the real git: %v", err)
	}
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sink := filepath.Join(dir, "ran")
	recorder := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+name+" >> '"+sink+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	monitor := recorder("fsmonitor")
	hooks := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"pre-commit", "post-commit"} {
		if err := os.Rename(recorder(hook), filepath.Join(hooks, hook)); err != nil {
			t.Fatal(err)
		}
	}
	home, configHome, clone := filepath.Join(dir, "home"), filepath.Join(dir, "config"), filepath.Join(dir, "clone")
	planted := "[core]\n\tfsmonitor = " + monitor + "\n\thooksPath = " + hooks + "\n"
	if err := os.MkdirAll(filepath.Join(configHome, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "git", "config"), []byte(planted), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + configHome, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.invalid", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.invalid"}
	run := func(env []string, args ...string) {
		t.Helper()
		fixturegit.Runner{TB: t, Env: env}.Run(clone, append([]string{git}, args...)...)
	}
	// The init creates the clone, so it runs in the directory holding it rather than in the one
	// the test process happens to have; every later command runs in the clone itself.
	fixturegit.Isolated(t, home).Run(dir, append([]string{git}, fixturegit.InitWorkTree(clone, "")[1:]...)...)
	if err := os.WriteFile(filepath.Join(clone, ".git", "config"), append(mustRead(t, filepath.Join(clone, ".git", "config")), planted...), 0o644); err != nil {
		t.Fatal(err)
	}
	ran := func() string {
		body, err := os.ReadFile(sink)
		if errors.Is(err, fs.ErrNotExist) {
			return ""
		} else if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(sink)
		return strings.Join(strings.Fields(string(body)), " ")
	}
	run(base, "status")
	run(base, "commit", "--quiet", "--allow-empty", "-m", "control")
	if got := ran(); !strings.Contains(got, "fsmonitor") || !strings.Contains(got, "pre-commit") || !strings.Contains(got, "post-commit") {
		t.Fatalf("without the role's pairs the planted programs ran %q, want the fsmonitor and both hooks: the plant is not live", got)
	}
	for _, role := range r.launcherRoles() {
		env := slices.Clone(base)
		for name, value := range envOf(workerOf(t, r, roleSpec(t, role))) {
			if strings.HasPrefix(name, "GIT_CONFIG_") {
				env = append(env, name+"="+value)
			}
		}
		run(env, "status")
		run(env, "commit", "--quiet", "--allow-empty", "-m", string(role))
		if got := ran(); got != "" {
			t.Errorf("%s's git ran %q, programs another role planted in the clone's and the global configuration", role, got)
		}
	}
}

// Every role's git maps packs in small windows from the role environment's pairs alone, whatever
// image the deployment runs: a real git given nothing but the GIT_CONFIG block of the role's start
// command, with no system or global file, an empty home and no repository around it, reads
// core.packedGitWindowSize 32m, core.packedGitLimit 256m, pack.windowMemory 64m and
// core.deltaBaseCacheLimit 64m at command scope, for a workflow role and the controller alike. The
// same git without the block reads none of them, so nothing but the pairs sets them here. A role's
// own `-c core.packedGitLimit=1g` still wins for its command, and the pairs win over an image's own
// system file: with GIT_CONFIG_SYSTEM naming a file that sets pack.windowMemory 256m, which the
// same git without the block reads, the role's git reads 64m.
func TestEveryRolesGitMapsPacksInSmallWindows(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("this test runs the real git: %v", err)
	}
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outside, home := filepath.Join(dir, "no-repository"), filepath.Join(dir, "home")
	for _, made := range []string{outside, home} {
		if err := os.Mkdir(made, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	system := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(system, []byte("[pack]\n\twindowMemory = 256m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := []string{"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CEILING_DIRECTORIES=" + dir}
	// imageFile is env with git reading system as its system file, as a git does in an image that
	// writes its own /etc/gitconfig: GIT_CONFIG_NOSYSTEM unset, and GIT_CONFIG_SYSTEM naming it.
	imageFile := func(env []string) []string {
		env = slices.DeleteFunc(slices.Clone(env), func(entry string) bool { return strings.HasPrefix(entry, "GIT_CONFIG_NOSYSTEM=") })
		return append(env, "GIT_CONFIG_SYSTEM="+system)
	}
	// read is `git <ahead…> config --show-scope --get key` under env, outside any repository.
	read := func(env []string, key string, ahead ...string) (string, error) {
		command := exec.Command(git, slices.Concat(ahead, []string{"config", "--show-scope", "--get", key})...)
		command.Dir = outside
		command.Env = env
		out, err := command.Output()
		return string(out), err
	}
	windows := [][2]string{
		{"core.packedGitWindowSize", "32m"},
		{"core.packedGitLimit", "256m"},
		{"pack.windowMemory", "64m"},
		{"core.deltaBaseCacheLimit", "64m"},
	}
	for _, setting := range windows {
		// git exits 1, printing nothing, for a key no configuration sets.
		var exit *exec.ExitError
		if out, err := read(base, setting[0]); out != "" || !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("without the role's pairs git reads %s as %q (%v), want nothing and exit 1: another source sets it", setting[0], out, err)
		}
	}
	if out, err := read(imageFile(base), "pack.windowMemory"); out != "system\t256m\n" || err != nil {
		t.Fatalf("without the role's pairs git reads the image's file's pack.windowMemory as %q (%v), want 256m from the system scope: the file is not read", out, err)
	}
	for name, spec := range map[string]runtime.SpawnSpec{"a workflow role": workerSpec(t), "the controller": controllerSpec(t)} {
		t.Run(name, func(t *testing.T) {
			env := slices.Clone(base)
			for _, entry := range launcherCommand(launchOf(t, r, spec), r).Env {
				if name, _, _ := strings.Cut(entry, "="); name == "GIT_CONFIG_COUNT" || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
					env = append(env, entry)
				}
			}
			for _, setting := range windows {
				if out, err := read(env, setting[0]); out != "command\t"+setting[1]+"\n" || err != nil {
					t.Errorf("the role's git reads %s as %q (%v), want %s at command scope", setting[0], out, err, setting[1])
				}
			}
			if out, err := read(env, "core.packedGitLimit", "-c", "core.packedGitLimit=1g"); out != "command\t1g\n" || err != nil {
				t.Errorf("the role's git given -c core.packedGitLimit=1g reads it as %q (%v), want 1g: a role's own -c wins for its command", out, err)
			}
			if out, err := read(imageFile(env), "pack.windowMemory"); out != "command\t64m\n" || err != nil {
				t.Errorf("the role's git under an image's file that sets pack.windowMemory 256m reads it as %q (%v), want 64m: the pairs win over the image's file", out, err)
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The roles of one issue pod share the pod's network namespace, the workspace path, the
// Oh My Pi profile path and the rest of the issue's scratch, and Oh My Pi's browser broker lock is
// an abstract unix socket named from the lock path under its state root: one state root would
// make it one name across every agent of the pod, the first agent's lock blocking every other
// agent's broker.
// So the agent of each slot is told a XDG_STATE_HOME of its own, its slot's directory under the
// scratch's `state` (`/legion/scratch/state/<slot>`, the slot being the agent's name whatever role
// its step is on), differing from every other slot's, and the
// controller's agent one by the same rule; workspace-init, which runs alone, keeps the plain
// home-directory state home. The value is on the issue's volume, deliberately: Oh My Pi's logs,
// Chromium's profiles and the installed plugin generation are charged to neither the pod's memory
// nor the pod's ephemeral storage, and they outlive a restart of the pod's launcher container (an
// issue's disk is its pod's own and goes with the pod; the controller's is its Sandbox's and
// outlives its pod).
// Which mount holds it is the argument above, not housekeeping: a state home reached through a
// sub-path mount, or one overlapping the workspace or uv's directories, is a directory whose
// contents are no longer the agent's alone, and the broker-lock reasoning quietly stops holding.
// So each slot's value is checked to sit on the issue volume's own mount and to overlap none of
// those three, exactly as the shared six are
// (TestTheIssuesRolesShareOneScratchAndOneGoCachePair).
func TestEachRolesAgentIsToldAStateHomeOfItsOwn(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	told := map[string]claim.Role{}
	for _, role := range r.launcherRoles() {
		worker := workerOf(t, r, roleSpec(t, role))
		env := envOf(worker)
		got := env["XDG_STATE_HOME"]
		if want := TreeRoot + "/scratch/state/" + string(role); got != want {
			t.Errorf("%s's agent is told XDG_STATE_HOME=%q, want %q", role, got, want)
		}
		if mount := mountHolding(worker, got); mount == nil || mount.Name != issueVolume || mount.SubPath != "" {
			t.Errorf("%s: XDG_STATE_HOME %q is on mount %+v, want the issue volume's own mount at %s", role, got, mount, TreeRoot)
		}
		for _, theirs := range []string{"LEGION_WORKSPACE", "UV_PYTHON_INSTALL_DIR", "UV_CACHE_DIR"} {
			if path := env[theirs]; path != "" && overlaps(got, path) {
				t.Errorf("%s: XDG_STATE_HOME %q overlaps %s %q", role, got, theirs, path)
			}
		}
		if other, taken := told[got]; taken {
			t.Errorf("%s's agent is told XDG_STATE_HOME=%q, %s's too", role, got, other)
		}
		told[got] = role
	}
	init := containerNamed(t, podOf(t, r, workerSpec(t)), initContainer)
	if got := envOf(init)["XDG_STATE_HOME"]; got != xdgStateHome {
		t.Errorf("workspace-init is told XDG_STATE_HOME=%q, want %q", got, xdgStateHome)
	}
}

// roleSpec is the spawn spec of role in the test tree's root issue: the controller's own, or a
// workflow role's claim of that issue.
func roleSpec(t *testing.T, role claim.Role) runtime.SpawnSpec {
	t.Helper()
	if role == claim.RoleController {
		return controllerSpec(t)
	}
	token, err := claim.NewToken("legion", testTree, role)
	if err != nil {
		t.Fatal(err)
	}
	return testSpec(t, token, role, testTree)
}

// The six directories a role's tools write to without being told where are the issue's, not the
// role's: every role of a workflow pod and the controller is told the same temporary
// directory, cache home, data home and bun install cache — the four directly under the issue's
// scratch on its volume — and the same Go build and module cache pair beside the workspaces. A
// directory per role is what this replaces, by the human's decision of 2026-10-20: disk is cheap,
// an issue gets the space it needs, and the roles within a pod are not kept from sharing a cache.
// Splitting them bought no isolation the pod has anyway — the roles of an issue share the pod,
// the disk whole, the one checkout they all work in (workspace.Location names the repository
// and the issue, never a role) and uv's Pythons and cache, and each can
// read and write what the others write — while costing a copy of every cache per role. Bun
// hardlinks out of its install cache into whatever it installs for (LEGION-198, where that
// hardlinking was first met), so a file edited in place in node_modules is already in the
// packages every role runs from, whatever we do about caches; the one install Legion renders
// itself copies rather than links (plugins.go). The two Go caches hold content addressed by its
// hash and the roles of one issue may fill them at once: of the build cache `go help cache` says
// "The cache is safe for concurrent invocations of the go command", and what reaches the module
// cache is a module zip or go.mod the go command has verified against a known hash (`go help
// module-auth`) in a directory it then makes read-only, which only `go build -modcacherw` leaves
// writable; a pair is 751 MiB of build cache and 375 MiB of module cache for this repository, so
// the six roles of a pod with a pair each would hold five redundant copies, about 5.5 GiB spent
// on nothing, and each role of the deployment one more.
// Each directory is held by the issue volume's own mount, exactly as uv's are
// (TestUvKeepsItsPythonsAndCacheOnTheIssueVolume), and overlaps neither the workspace nor uv's
// directories, and no two of the six are one directory. The state home is the one thing of the
// scratch that stays one per slot, the agent's, for the browser broker's lock
// (TestEachRolesAgentIsToldAStateHomeOfItsOwn).
func TestTheIssuesRolesShareOneScratchAndOneGoCachePair(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	shared := [][2]string{
		{"TMPDIR", TreeRoot + "/scratch/tmp"},
		{"XDG_CACHE_HOME", TreeRoot + "/scratch/cache"},
		{"XDG_DATA_HOME", TreeRoot + "/scratch/data"},
		{"BUN_INSTALL_CACHE_DIR", TreeRoot + "/scratch/bun/install/cache"},
		{"GOCACHE", TreeRoot + "/go/build"},
		{"GOMODCACHE", TreeRoot + "/go/mod"},
	}
	for _, role := range r.launcherRoles() {
		main := workerOf(t, r, roleSpec(t, role))
		env := envOf(main)
		told := map[string]string{}
		for _, pair := range shared {
			name, want := pair[0], pair[1]
			dir := env[name]
			if dir != want {
				t.Errorf("%s is told %s=%q, want %q, the one directory every role of the issue is told", role, name, dir, want)
				continue
			}
			if mount := mountHolding(main, dir); mount == nil || mount.Name != issueVolume || mount.SubPath != "" {
				t.Errorf("%s: %s %q is on mount %+v, want the issue volume's own mount at %s", role, name, dir, mount, TreeRoot)
			}
			for _, theirs := range []string{"LEGION_WORKSPACE", "UV_PYTHON_INSTALL_DIR", "UV_CACHE_DIR"} {
				if path := env[theirs]; path != "" && overlaps(dir, path) {
					t.Errorf("%s: %s %q overlaps %s %q", role, name, dir, theirs, path)
				}
			}
			if other, taken := told[dir]; taken {
				t.Errorf("%s is told %s=%q and %s=%q; these are six directories, and the go command keeps its two caches apart", role, other, dir, name, dir)
			}
			told[dir] = name
		}
	}
}

// Every role of every pod is told the shared public leak check its own pod installs, at one path:
// the installer's constant (leakguard.InstalledCheck), under the scratch the roles of the pod
// share. The runtime names the path and the pod's launcher writes it, installing the check the
// daemon pushes to it once for the pod before any role starts, so a constant of the installer's
// is what keeps the two from drifting into a role told a check nothing ever put there — every
// write path of that role resolves PUBLIC_LEAK_CHECK first and refuses when the file is not
// there. It is under the scratch because that is the place of the pod's disk every role is told
// the same of and nothing outlives the pod: a check that publishes as often as this one is
// installed per pod and never kept past it. The controller's pod is in the cases here too: it
// writes to GitHub as a role does, and its one container mounts the same volume at TreeRoot.
func TestEveryRoleIsToldTheLeakCheckItsPodInstalls(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(leakguard.InstalledCheck, scratchRoot+"/") {
		t.Errorf("the install puts the check at %s, want it under the issue's scratch %s", leakguard.InstalledCheck, scratchRoot)
	}
	for name, tc := range manifestCases(t) {
		main := workerOf(t, r, tc.spec)
		if told := envOf(main)["PUBLIC_LEAK_CHECK"]; told != leakguard.InstalledCheck {
			t.Errorf("%s is told PUBLIC_LEAK_CHECK=%q, want the installed check %q", name, told, leakguard.InstalledCheck)
		}
		if mount := mountHolding(main, leakguard.InstallDir); mount == nil || mount.Name != issueVolume || mount.SubPath != "" {
			t.Errorf("%s: the install directory %q is on mount %+v, want the pod's own volume mounted at %s", name, leakguard.InstallDir, mount, TreeRoot)
		}
	}
}

// Every directory the runtime tells a role to write in is one the shim makes, and every directory
// the shim makes is one the runtime names. The shim reads its list from podsafety.ScratchVariables
// and the runtime renders roleEnvironment, so without this the two could drift silently: a role
// would be told a temporary directory nothing ever created, and mktemp and every library
// equivalent would fail in its first turn, which is the failure EnsureScratch exists to prevent.
// So each name on the shim's list, plus XDG_STATE_HOME, which the shim makes elsewhere
// (podsafety.EnsureStateHome, which makes Oh My Pi's profile directory under it), is set to an
// absolute path on the issue's volume — the issue's shared scratch for four of them, the issue's
// Go caches for two, and the slot's state home, one per slot, for the last — and, the other way,
// every
// variable the runtime sets to a path on that volume is on that list, bar the three it names for
// a reason of their own, so neither a new directory of the issue's scratch nor a new per-slot one
// can appear that nothing makes.
func TestTheRuntimeTellsTheShimEveryScratchDirectoryItMakes(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(workerOf(t, r, workerSpec(t)))
	made := append(slices.Clone(podsafety.ScratchVariables), "XDG_STATE_HOME")
	for _, name := range made {
		if dir := env[name]; !strings.HasPrefix(dir, TreeRoot+"/") {
			t.Errorf("the shim makes %s, which the runtime sets to %q; want an absolute path under %s", name, dir, TreeRoot)
		}
	}
	// The runtime names these four on the issue's volume for a reason of their own and does not
	// ask the shim's EnsureScratch to make them: LEGION_WORKSPACE is the issue's checkout, which
	// workspace-init provisions before the pod's launcher container starts; UV_PYTHON_INSTALL_DIR and
	// UV_CACHE_DIR are uv's own, which uv makes and the tree's pods share by design
	// (TestUvKeepsItsPythonsAndCacheOnTheIssueVolume); and PUBLIC_LEAK_CHECK is no directory a
	// role writes in at all but a file the pod's launcher installs there, from the check the
	// daemon pushes to it before any role starts, and which the shim reads before it starts the
	// agent (leakguard.Installed), so a pod whose launcher could not install it fails the launch
	// rather than leaving a role to meet it missing. Every other path the runtime names there is
	// a directory it tells the role to write in, so a fifth exemption has to be argued for rather
	// than slipped in.
	theirOwn := []string{"LEGION_WORKSPACE", "UV_PYTHON_INSTALL_DIR", "UV_CACHE_DIR", "PUBLIC_LEAK_CHECK"}
	for name, value := range env {
		if !strings.HasPrefix(value, TreeRoot+"/") || slices.Contains(made, name) || slices.Contains(theirOwn, name) {
			continue
		}
		t.Errorf("the runtime tells the role %s=%q on the issue's volume; the shim makes %v and EnsureStateHome the state home, so nothing would make it",
			name, value, podsafety.ScratchVariables)
	}
}

// uv links a project's .venv, in an issue's workspace on the issue's volume, to an interpreter
// under UV_PYTHON_INSTALL_DIR, and installs into it from UV_CACHE_DIR. Both are on the volume's own
// mount (no other mount covering them) and outside the workspace, whose tree is the project's, so
// every role of the pod runs the .venv as it is and reuses what another role of the pod
// downloaded. No later pod reuses any of it: the disk is the pod's own and dies with it
// (INFRA-80), so a second pod of the issue downloads afresh. The cache is at the same path in
// every pod, and the interpreter directory is the issue's, so two pods of one issue name the same
// paths on disks of their own, and a second issue of the tree names another.
func TestUvKeepsItsPythonsAndCacheOnTheIssueVolume(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	child, err := claim.NewToken("legion", "LEGION-209", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	var envs []map[string]string
	for _, spec := range []runtime.SpawnSpec{rootSpec(t), workerSpec(t), testSpec(t, child, claim.RoleImplementer, "LEGION-209")} {
		main := workerOf(t, r, spec)
		env := envOf(main)
		for _, name := range []string{"UV_PYTHON_INSTALL_DIR", "UV_CACHE_DIR"} {
			dir := env[name]
			if mount := mountHolding(main, dir); mount == nil || mount.Name != issueVolume || mount.SubPath != "" {
				t.Errorf("%s %s: %s %q is on mount %+v, want the issue volume's own mount at %s", spec.Issue, spec.Role, name, dir, mount, TreeRoot)
			}
			if overlaps(dir, env["LEGION_WORKSPACE"]) {
				t.Errorf("%s %s: %s %q overlaps the workspace %s", spec.Issue, spec.Role, name, dir, env["LEGION_WORKSPACE"])
			}
		}
		envs = append(envs, env)
	}
	architect, tester, other := envs[0], envs[1], envs[2]
	if architect["UV_PYTHON_INSTALL_DIR"] != tester["UV_PYTHON_INSTALL_DIR"] {
		t.Errorf("two pods of one issue install Pythons into %q and %q; the later would not find the earlier's interpreter",
			architect["UV_PYTHON_INSTALL_DIR"], tester["UV_PYTHON_INSTALL_DIR"])
	}
	if other["UV_PYTHON_INSTALL_DIR"] == architect["UV_PYTHON_INSTALL_DIR"] {
		t.Errorf("two issues of one tree share the Python directory %q", other["UV_PYTHON_INSTALL_DIR"])
	}
	if architect["UV_CACHE_DIR"] != tester["UV_CACHE_DIR"] || other["UV_CACHE_DIR"] != architect["UV_CACHE_DIR"] {
		t.Errorf("the tree's pods use the caches %q, %q and %q; want one", architect["UV_CACHE_DIR"], tester["UV_CACHE_DIR"], other["UV_CACHE_DIR"])
	}
}

// A pod's own IP changes on every restart, so a daemon that runs as a pod hands pods a stable
// address instead, a Kubernetes Service's DNS name: StreamURL is advertise_host at the worker
// stream's port (shimAddress, internal/daemon/daemon.go) and DaemonURL is daemon_url. Neither
// Options field needs an IP: the manifest carries each exactly as configured, on every role
// launcher's --connect and in the worker process the start command runs.
func TestTheManifestCarriesADNSNamedStreamAndDaemonURL(t *testing.T) {
	opts := goldenOptions()
	opts.StreamURL = "tcp://legion-daemon-widgets.legion.svc:13371"
	opts.DaemonURL = "http://legion-daemon-widgets.legion.svc:13370"
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range podOf(t, r, workerSpec(t)).Containers {
		if !slices.Contains(c.Command, "tcp://legion-daemon-widgets.legion.svc:13371") {
			t.Errorf("the %s launcher's argv = %v, want it to dial tcp://legion-daemon-widgets.legion.svc:13371", c.Name, c.Command)
		}
	}
	worker := workerOf(t, r, workerSpec(t))
	if !slices.Contains(worker.Command, "tcp://legion-daemon-widgets.legion.svc:13371") {
		t.Errorf("the shim's argv = %v, want it to dial tcp://legion-daemon-widgets.legion.svc:13371", worker.Command)
	}
	if got := envOf(worker)["LEGION_DAEMON_URL"]; got != "http://legion-daemon-widgets.legion.svc:13370" {
		t.Errorf("LEGION_DAEMON_URL = %q, want http://legion-daemon-widgets.legion.svc:13370", got)
	}
}

// The provisioning token never shares a process with anything a tree agent can write (Stage 4b
// Task 4b.6b), and it reaches the container that holds it as a file, never as an environment
// value (INFRA-1254). Two rules, counted apart:
//
//   - The volume route — the claim's Secret as a Secret volume, or projected — belongs to
//     workspace-fetch alone, the one container of the pod that holds the token. It is required
//     for it, the only container told where the token is, and forbidden for every other
//     container, the worker's included. The holder's mount of that volume carries an empty
//     subPath, because the daemon rewrites the claim's Secret as the lease is renewed and the
//     kubelet rewrites a whole-volume Secret projection in place, never a subPath copy, which is
//     taken once when the container starts.
//   - The environment route — an env entry whose secretKeyRef names the claim Secret's
//     provisioning-token key, or an envFrom of that Secret — is forbidden everywhere, the
//     holder included: an environment value is fixed for the container's life, so a container
//     holding one serves the token the pod started with however often the Secret is rewritten.
//
// Beside them, no other container can write a volume the holder mounts. The feed workspace-fetch
// fills is read-only in workspace-init, and its TMPDIR, where its one-shot credential goes, is an
// in-memory volume of its own that no other container mounts. So workspace-fetch mounts neither
// the tree volume nor the config home, and carries no operator's mount (runtime.kubernetes.pod),
// which reaches the launcher containers and the image probe's and no init container. Every pod's
// manifest holds to it, by every route Kubernetes offers into a Secret, the worker's container
// included; the controller's pod has no workspace-fetch, so none of its containers reaches the
// token at all.
//
// What the boundary protects is identity, not a repository-write token kept off the tree volume:
// an implement role now holds the implement App's token — for the issue's owner the App's own
// installation token, which writes, where the provisioning token is that same App narrowed to
// reading, so the two are no longer one lease (INFRA-1254) — read-only in its own gh directory
// (GHConfigDir), and a review role the review App's, save the architect's slot, which holds the
// App of a step it does itself while it does that step, and of its own implementation step through
// the retro (runtime.ActsAs). What the fetch container's boundary still keeps is that no slot holds
// the implement App's token but while it works as a role of the type implementation, which is what
// lets the daemon read a push's App
// from the App that made it, and so tell the review App's red tests from the implementation's
// code.
func TestTheProvisionTokenSharesNoContainerWithAnythingTheTreeCanWrite(t *testing.T) {
	opts := goldenOptions()
	// An operator pod with volumes and mounts of its own, so the no-operator-mount assertion
	// below has something to find: with none configured, that check would pass on a manifest
	// that handed every one of them to the token-holding container.
	opts.Pod = operatorPod()
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			l, err := r.prepare(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			template := r.podTemplate(l)
			pod := template.Spec
			claimSecret := secretName(l.name)
			volumes := map[string]corev1.Volume{}
			for _, volume := range pod.Volumes {
				volumes[volume.Name] = volume
			}
			// A Secret source that maps no keys maps every key.
			maps := func(items []corev1.KeyToPath) bool {
				return len(items) == 0 || slices.ContainsFunc(items, func(item corev1.KeyToPath) bool { return item.Key == provisionTokenKey })
			}
			// The volume route is the token as a file the kubelet rewrites in place when the
			// Secret changes. Each one carries the mount it came by, since a mount with a subPath
			// is a copy taken when the container starts that no rewrite reaches.
			type route struct {
				mount corev1.VolumeMount
				named string
			}
			volumeRoutes := func(c corev1.Container) []route {
				var routes []route
				for _, mount := range c.VolumeMounts {
					volume := volumes[mount.Name]
					if secret := volume.Secret; secret != nil && secret.SecretName == claimSecret && maps(secret.Items) {
						routes = append(routes, route{mount, "secret volume " + mount.Name})
					}
					if projected := volume.Projected; projected != nil {
						for _, source := range projected.Sources {
							if secret := source.Secret; secret != nil && secret.Name == claimSecret && maps(secret.Items) {
								routes = append(routes, route{mount, "projected volume " + mount.Name})
							}
						}
					}
				}
				return routes
			}
			// The environment route is a copy fixed for the container's life, which no container
			// may hold, the token holder included.
			envRoutes := func(c corev1.Container) []string {
				var routes []string
				for _, v := range c.Env {
					if from := v.ValueFrom; from != nil && from.SecretKeyRef != nil && from.SecretKeyRef.Name == claimSecret && from.SecretKeyRef.Key == provisionTokenKey {
						routes = append(routes, "env "+v.Name)
					}
				}
				for _, from := range c.EnvFrom {
					if from.SecretRef != nil && from.SecretRef.Name == claimSecret {
						routes = append(routes, "envFrom "+from.SecretRef.Name)
					}
				}
				return routes
			}
			containers := slices.Concat(pod.InitContainers, pod.Containers)
			for _, c := range containers {
				_, pointed := envOf(c)["LEGION_PROVISION_TOKEN_FILE"]
				holds := c.Name == fetchContainer
				mounted := volumeRoutes(c)
				var named []string
				for _, r := range mounted {
					named = append(named, r.named)
				}
				if (len(mounted) > 0) != holds || pointed != holds {
					t.Errorf("%s reaches the provisioning token's file by %v, pointed at %t; want %t for both", c.Name, named, pointed, holds)
				}
				for _, r := range mounted {
					if r.mount.SubPath != "" {
						t.Errorf("%s takes the provisioning token from %s with subPath %q, want the whole volume: the kubelet rewrites a whole-volume Secret projection in place when the Secret changes and never a subPath copy, so a renewed token would not reach the file",
							c.Name, r.named, r.mount.SubPath)
					}
				}
				if routes := envRoutes(c); len(routes) > 0 {
					t.Errorf("%s reaches the provisioning token through the environment by %v, which no container may, %s included: an environment value is fixed for the container's life, so a renewed token never reaches it",
						c.Name, routes, fetchContainer)
				}
			}
			kind, err := podKindOf(template.Metadata.Labels)
			if err != nil {
				t.Fatal(err)
			}
			if kind == (controllerPod{}) {
				if slices.ContainsFunc(containers, func(c corev1.Container) bool { return c.Name == fetchContainer }) {
					t.Errorf("the controller's pod runs %s", fetchContainer)
				}
				return
			}
			in := func(c corev1.Container, path string) string {
				for _, mount := range c.VolumeMounts {
					if mount.MountPath == path {
						return mount.Name
					}
				}
				return ""
			}
			// The one container that holds the token, workspace-fetch: no other container may
			// write what it mounts. The feed, a subPath of the issue volume, is reachable through
			// another container's read-write mount of the volume (or of a subPath above the feed)
			// unless that container mounts the feed read-only over exactly the path the
			// read-write mount reaches it at.
			holder := containerNamed(t, pod, fetchContainer)
			for _, mount := range holder.VolumeMounts {
				for _, other := range containers {
					if other.Name == holder.Name {
						continue
					}
					for _, theirs := range other.VolumeMounts {
						if theirs.Name != mount.Name || theirs.ReadOnly {
							continue
						}
						below, ok := strings.CutPrefix(mount.SubPath, theirs.SubPath)
						if !ok || (theirs.SubPath != "" && below != "" && !strings.HasPrefix(below, "/")) {
							continue
						}
						reached := path.Join(theirs.MountPath, below)
						shadowed := slices.ContainsFunc(other.VolumeMounts, func(m corev1.VolumeMount) bool {
							return m.Name == mount.Name && m.ReadOnly && m.SubPath == mount.SubPath && m.MountPath == reached
						})
						if !shadowed {
							t.Errorf("%s can write volume %s at %s, which %s mounts at %s", other.Name, mount.Name, reached, holder.Name, mount.MountPath)
						}
					}
				}
			}
			// The holder's TMPDIR is an in-memory volume of its own: a TMPDIR another container
			// shared would be one that container could read the one-shot credential in.
			temp := envOf(holder)["TMPDIR"]
			volume := in(holder, temp)
			if dir := volumes[volume].EmptyDir; temp == "" || dir == nil || dir.Medium != corev1.StorageMediumMemory {
				t.Errorf("%s's TMPDIR %q is not an in-memory volume of its own", holder.Name, temp)
			}
			// "Of its own" is about the volume, not the path: another container that mounted the
			// same volume read-only somewhere else would read the credential just as well, and
			// the write-overlap check above passes a read-only mount by design. So compare by
			// volume name across every mount of every other container, whatever its path and
			// whether it is read-only.
			for _, other := range containers {
				if other.Name == holder.Name {
					continue
				}
				for _, mount := range other.VolumeMounts {
					if mount.Name == volume {
						t.Errorf("%s mounts %s's TMPDIR volume %s at %s (readOnly %t)",
							other.Name, holder.Name, volume, mount.MountPath, mount.ReadOnly)
					}
				}
			}
			// No operator mount reaches the token holder: the operator's pod block
			// (runtime.kubernetes.pod) names volumes of its own, and one of them in the container
			// holding the token would share the token with whatever the operator put there.
			if len(r.pod.VolumeMounts) == 0 {
				t.Fatal("the operator's pod carries no mount, so this check would pass on a manifest that gave the token holder every one of them")
			}
			for _, mount := range holder.VolumeMounts {
				if slices.ContainsFunc(r.pod.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == mount.Name }) {
					t.Errorf("%s carries the operator's mount %s at %s", holder.Name, mount.Name, mount.MountPath)
				}
			}
			if feed := in(containerNamed(t, pod, initContainer), FeedDir); feed == "" || feed != in(holder, FeedDir) {
				t.Errorf("workspace-init reads the feed from %q, and %s fills %q", feed, holder.Name, in(holder, FeedDir))
			}
		})
	}
}

// A launch the runtime cannot honour exactly is refused before any API call.
func TestALaunchItCannotHonourIsRefused(t *testing.T) {
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		edit func(*runtime.SpawnSpec)
		want string
	}{
		"no repository":     {func(s *runtime.SpawnSpec) { s.Repository = ghrepo.Repository{} }, "no repository"},
		"no default branch": {func(s *runtime.SpawnSpec) { s.DefaultBranch = "" }, "no default branch for repository sjawhar/legion-smoke"},
		"runtime-owned env": {func(s *runtime.SpawnSpec) { s.Env["PATH"] = "/bin" }, "Env sets PATH"},
		"composed env": {func(s *runtime.SpawnSpec) { s.Env[runtime.SpendAttributesVariable] = "legion.issue=OTHER-1" },
			"Env sets " + runtime.SpendAttributesVariable},
		"credential in env":   {func(s *runtime.SpawnSpec) { s.Env["ANTHROPIC_API_KEY"] = "x" }, "credential-shaped"},
		"gh token in env":     {func(s *runtime.SpawnSpec) { s.Env["GH_TOKEN"] = "x" }, "Env sets GH_TOKEN"},
		"boot token secret":   {func(s *runtime.SpawnSpec) { s.Secrets[bootTokenKey] = "x" }, "is a variable the runtime sets itself"},
		"provisioning secret": {func(s *runtime.SpawnSpec) { s.Secrets[provisionTokenKey] = "x" }, "a key the runtime writes itself"},
		"dispatch secret":     {func(s *runtime.SpawnSpec) { s.Secrets[dispatchTokenKey] = "x" }, "DISPATCH_TOKEN_FILE is a variable the runtime sets itself"},
		"prompt past one start frame": {func(s *runtime.SpawnSpec) { s.Prompt.Addressing = strings.Repeat("x", shimwire.MaxFrameBytes) },
			"the start frame that carries it to its pod's launcher would be"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := workerSpec(t)
			tc.edit(&spec)
			if _, err := r.prepare(spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("prepare: %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// New refuses options no cluster could run: an image not pinned by digest, an issue volume with no
// storage class on a cluster that has no default, a stream pods cannot dial, a pool the runtime
// does not choose, a microVM issue pod with no ComputeClass to run on or no storage class for its
// disk, no source for the provisioning token or the roles' gh files, and a Resources
// map that would leave a container unreserved — a role or the controller without an entry, or an
// entry that is no reservation: a cpu or memory request past its limit, no memory, an
// ephemeral-storage request past its limit or none at all, or a resource the daemon never sizes —
// each refusal naming the role. A request below its limit is a reservation
// (TestARequestBelowItsLimitRendersABurstablePod).
func TestNewRefusesOptionsNoPodCouldRun(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Options)
		want string
	}{
		"tag image":                 {func(o *Options) { o.Image = "ghcr.io/cybermonklabs/legion-worker:latest" }, "not pinned by digest"},
		"no class":                  {func(o *Options) { o.StorageClass = "" }, "no storage class"},
		"no volume size":            {func(o *Options) { o.IssueVolume = resource.Quantity{} }, "no issue volume size"},
		"unix stream":               {func(o *Options) { o.StreamURL = "unix:///run/legion.sock" }, "is not tcp://host:port"},
		"relative tool":             {func(o *Options) { o.Tools.Git = "git" }, "git path \"git\" is not absolute"},
		"bad project":               {func(o *Options) { o.Project = "s4a run" }, "is not a label value"},
		"url no bearer":             {func(o *Options) { o.DispatchURL = "https://dispatch.internal" }, "configured together"},
		"bearer no url":             {func(o *Options) { o.DispatchToken = "dispatch-bearer" }, "configured together"},
		"another pool":              {func(o *Options) { o.Scheduling.NodeSelector = map[string]string{poolKey: "gpu"} }, "legion.dev/pool is the runtime's"},
		"the pool restated":         {func(o *Options) { o.Scheduling.NodeSelector = map[string]string{poolKey: poolValue} }, "legion.dev/pool is the runtime's"},
		"no tokens":                 {func(o *Options) { o.Tokens = nil }, "no provisioning token source"},
		"no gh credential":          {func(o *Options) { o.GitHubCredential = nil }, "no github credential function"},
		"no gh dirs":                {func(o *Options) { o.GitHubDirs = nil }, "no github directories function"},
		"no repository set":         {func(o *Options) { o.CacheRepositories = nil }, "no repository set for the repository caches"},
		"no cache volume":           {func(o *Options) { o.RepositoryCache.Volume = resource.Quantity{} }, "repository cache volume 0 is not a positive quantity"},
		"no cache retention":        {func(o *Options) { o.RepositoryCache.Retain = 0 }, "repository cache retention 0s is not positive"},
		"an unreserved cache":       {func(o *Options) { o.RepositoryCache.Resources = corev1.ResourceRequirements{} }, "the repository cache reserves no cpu"},
		"the issue pod unreserved":  {func(o *Options) { o.Resources.Issue = corev1.ResourceRequirements{} }, "the issue pod reserves no cpu"},
		"the controller unreserved": {func(o *Options) { o.Resources.Controller = corev1.ResourceRequirements{} }, "the controller's pod reserves no cpu"},
		"the file session store": {
			func(o *Options) { o.SessionStore = ompsessions.Store{Kind: ompsessions.StoreVolume} },
			"no session store that keeps sessions in a database",
		},
		"no session store": {func(o *Options) { o.SessionStore = ompsessions.Store{} }, "no session store that keeps sessions in a database"},
		"cpu request past limit": {
			func(o *Options) {
				o.Resources.Issue.Requests[corev1.ResourceCPU] = resource.MustParse("3500m")
			},
			"the issue pod requests cpu 3500m past its limit 3",
		},
		"memory request past limit": {
			func(o *Options) {
				o.Resources.Issue.Requests[corev1.ResourceMemory] = resource.MustParse("20Gi")
			},
			"the issue pod requests memory 20Gi past its limit 19Gi",
		},
		"no memory": {
			func(o *Options) {
				delete(o.Resources.Controller.Requests, corev1.ResourceMemory)
				delete(o.Resources.Controller.Limits, corev1.ResourceMemory)
			},
			"the controller's pod reserves no memory",
		},
		"ephemeral-storage request past its limit": {
			func(o *Options) {
				o.Resources.Issue.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("90Gi")
			},
			"the issue pod requests ephemeral-storage 90Gi past its limit 80Gi",
		},
		"ephemeral-storage unset": {
			func(o *Options) {
				delete(o.Resources.Issue.Requests, corev1.ResourceEphemeralStorage)
				delete(o.Resources.Issue.Limits, corev1.ResourceEphemeralStorage)
			},
			"the issue pod bounds no ephemeral-storage",
		},
		"an extended resource": {
			func(o *Options) {
				o.Resources.Issue.Limits["nvidia.com/gpu"] = resource.MustParse("1")
			},
			"the issue pod names nvidia.com/gpu in its limits",
		},
		"a microVM project with no microvm block": {
			func(o *Options) { o.Sandbox = SandboxConfig{Type: SandboxMicroVM} },
			`no compute class for the pods that run in a microVM (sandbox type "microvm", canary issues [])`,
		},
		"a canary with no compute class": {
			func(o *Options) {
				o.Sandbox = SandboxConfig{CanaryIssues: []string{testTree}, MicroVM: &MicroVM{StorageClass: testMicroVMStorageClass}}
			},
			`no compute class for the pods that run in a microVM (sandbox type "", canary issues [LEGION-208])`,
		},
		"a microVM block with no storage class": {
			func(o *Options) {
				o.Sandbox = SandboxConfig{Type: SandboxMicroVM, MicroVM: &MicroVM{ComputeClass: testComputeClass}}
			},
			"no storage class for the disks of the issue pods that run in a microVM",
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions()
			tc.edit(&opts)
			if _, err := configure(opts); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("configure: %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// runtimeOwned and the composed names together, with the exchange identity's two where the
// deployment names an exchange audience (Runtime.specOwned), are exactly what the worker container
// is told by the runtime itself, a tree agent's, a resumed tree agent's, a Claude Code worker's and
// the controller's together: every name mainEnvironment sets with Env empty and no secret of the
// spec's, every optional value configured — the tree agent works a repository the project names,
// so it is told the issue key it stamps its commits with — under an enrollment that names an
// exchange audience and under one that names none, which tells no role either exchange variable.
// A name added to the environment and in no set is one a spec could override; a name left in one
// of them that the environment no longer sets is one a spec is refused for nothing. The sets are
// kept apart by what an operator may do with a name, not by who writes the value the container
// carries: a composed name reaches the container with the operator's own value inside it
// (roleOperatorEnv), and every launch has a role, so the spend attributes are in every one of
// these environments.
func TestRuntimeOwnedIsWhatTheWorkerContainerIsToldByTheRuntime(t *testing.T) {
	for name, agentSecrets := range map[string]*AgentSecrets{
		"the exchange identity on":  exchangeEnrolled(),
		"the exchange identity off": rootEnrolled(),
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions()
			opts.DispatchURL, opts.DispatchToken = "https://dispatch.internal", "dispatch-bearer"
			opts.AgentSecrets = agentSecrets
			opts.TestPostgres = true
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			stamping := workerSpec(t)
			stamping.IssueTrailer = stamping.Issue
			// The resumed tree agent is told what holds it to the session it resumes, which no fresh
			// launch is told (runtime.SessionExclusiveVariable).
			resuming := stamping
			resuming.Generation, resuming.BootToken, resuming.ResumeSessionFile = 2, "boot-g2", resumeSession
			told := map[string]bool{}
			for _, spec := range []runtime.SpawnSpec{stamping, resuming, claudeWorkerSpec(t), controllerSpec(t)} {
				spec.Env, spec.Secrets = nil, nil
				for name := range envOf(workerOf(t, r, spec)) {
					told[name] = true
				}
			}
			if owned := r.specOwned(); !maps.Equal(told, owned) {
				t.Errorf("the worker container is told %v by the runtime, and the names a spec may not set are %v",
					slices.Sorted(maps.Keys(told)), slices.Sorted(maps.Keys(owned)))
			}
		})
	}
}

// A pane stamps `Dispatch: <KEY>` into its commits where the project names its repository
// (projects.<KEY>.commit_issue_trailer_repos, which internal/daemon's specs reads into
// runtime.SpawnSpec.IssueTrailer), and the key reaches its jj overlay as LEGION_ISSUE_TRAILER.
// The runtime sets it itself, from the spec's own field and never from its Env, beside the issue
// every pane is told: a launch of any harness is told it by the one environment the pod's kind
// builds for every role (issuePod.agentEnv under mainEnvironment), so a Claude Code worker
// carries it as an Oh My Pi one does. A launch that stamps nothing carries no such variable
// rather than an empty one, and the controller's pod, which works no repository and no issue,
// carries none under any spec (validateControllerShape refuses one that asks).
func TestAPodIsToldTheIssueKeyItStampsItsCommitsWith(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	stamping := workerSpec(t)
	stamping.IssueTrailer = stamping.Issue
	env := envOf(workerOf(t, r, stamping))
	for name, expect := range map[string]struct {
		want  string
		owned bool
	}{
		"LEGION_ISSUE":         {want: stamping.Issue, owned: true},
		"LEGION_ISSUE_TRAILER": {want: stamping.Issue, owned: true},
	} {
		if env[name] != expect.want {
			t.Errorf("a pane whose repository the project names is told %s=%q, want %q", name, env[name], expect.want)
		}
		if owned := runtimeOwned[name]; owned != expect.owned {
			t.Errorf("runtimeOwned[%s] = %t, want %t: the runtime owns the names it sets and no other", name, owned, expect.owned)
		}
	}
	claude := stamping
	claude.Backend = runtime.BackendClaude
	if got := envOf(workerOf(t, r, claude))["LEGION_ISSUE_TRAILER"]; got != stamping.Issue {
		t.Errorf("a Claude Code worker of the same claim is told LEGION_ISSUE_TRAILER=%q, want %q", got, stamping.Issue)
	}
	for name, spec := range map[string]runtime.SpawnSpec{
		"a worker whose repository the project does not name": workerSpec(t),
		"the controller": controllerSpec(t),
	} {
		if value, set := envOf(workerOf(t, r, spec))["LEGION_ISSUE_TRAILER"]; set {
			t.Errorf("%s is told LEGION_ISSUE_TRAILER=%q, want no such variable", name, value)
		}
	}
}

// The seven stall limits a Legion session's turn runs on reach a pod as ordinary entries of the
// worker process's environment: the daemon merges the ones it holds into the spec's Env (the
// daemon package's SpawnSpec, after its fork), and Env travels verbatim as plain pairs of the
// generation's start command (launchEnvironment), so nothing of them is this runtime's own — they
// are no part of runtimeOwned, which a spec may not set — and the controller's pod is told them
// exactly as a phase worker's is.
func TestTheWorkerContainersEnvCarriesTheStallLimits(t *testing.T) {
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	limits := map[string]string{
		"LEGION_STALL_ARCHITECT_AFTER": "3",
		"LEGION_STALL_CALL_BOUND_MS":   "3600000",
		"LEGION_STALL_CALL_GRACE_MS":   "300000",
		"LEGION_STALL_REMINDER_MS":     "45000",
		"LEGION_STALL_RESUME_MS":       "15000",
		"LEGION_STALL_STEP_MS":         "120000",
		"LEGION_STALL_WAITING_MS":      "900000",
	}
	for kind, spec := range map[string]runtime.SpawnSpec{"a phase worker": workerSpec(t), "the controller": controllerSpec(t)} {
		t.Run(kind, func(t *testing.T) {
			maps.Copy(spec.Env, limits)
			env := envOf(workerOf(t, r, spec))
			for name, want := range limits {
				if env[name] != want {
					t.Errorf("%s's container is told %s=%q, want %q", kind, name, env[name], want)
				}
			}
		})
	}
}

// handedAddresses is exactly the addresses a role process launched now carries, every optional
// address configured: each address-valued argument of every launcher's command and of the
// generation's worker-shim argv (the words before `--`), by its flag, and each address-valued
// variable the runtime itself sets in the generation's environment, an address being a value with a
// scheme. The git configuration entries (GitConfigEnv) are not addresses but git configuration
// keys the binary fixes, `credential.https://github.com.helper` among them, so no configuration
// moves them. An address added to a launch and not to handedAddresses is one the daemon could move
// without any role it re-adopts being relaunched (evaluate). A workflow role's launch and the
// controller's, each enrolled with the secrets broker here, are each held to that one list.
func TestHandedAddressesAreEveryAddressAPodCarries(t *testing.T) {
	opts := testOptions()
	opts.DispatchURL, opts.DispatchToken = "https://dispatch.internal.example", "dispatch-bearer"
	opts.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]runtime.SpawnSpec{"a workflow role": workerSpec(t), "the controller": controllerSpec(t)} {
		t.Run(name, func(t *testing.T) {
			l, err := r.prepare(spec)
			if err != nil {
				t.Fatal(err)
			}
			start := launcherCommand(l, r)
			carried := map[string]string{}
			words := [][]string{start.Argv[:slices.Index(start.Argv, "--")]}
			for _, c := range podOf(t, r, spec).Containers {
				words = append(words, c.Command)
			}
			for _, command := range words {
				for i := 1; i < len(command); i++ {
					if strings.Contains(command[i], "://") {
						carried[command[i-1]] = command[i]
					}
				}
			}
			gitConfig := map[string]bool{}
			for _, entry := range GitConfigEnv() {
				gitConfig[entry.Name] = true
			}
			for _, pair := range start.Env {
				if name, value, _ := strings.Cut(pair, "="); runtimeOwned[name] && !gitConfig[name] && strings.Contains(value, "://") {
					carried[name] = value
				}
			}
			handed := map[string]string{}
			for _, a := range r.handedAddresses(l.enrolls(r)) {
				if a.value != "" {
					handed[a.name] = a.value
				}
			}
			if !maps.Equal(carried, handed) {
				t.Errorf("a role launched now carries the addresses %v, and handedAddresses is %v", carried, handed)
			}
		})
	}
}

// The Dispatch bearer is the runtime's to carry, not the spec's: with Dispatch configured, every
// claim's Secret holds it as DISPATCH_TOKEN and the worker container reads it through
// DISPATCH_TOKEN_FILE; without it, neither exists.
func TestTheDispatchBearerIsARuntimeOption(t *testing.T) {
	for name, tc := range map[string]struct {
		url, bearer string
	}{
		"configured":     {"https://dispatch.internal", "dispatch-bearer"},
		"not configured": {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions()
			opts.DispatchURL, opts.DispatchToken = tc.url, tc.bearer
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			l, err := r.prepare(workerSpec(t))
			if err != nil {
				t.Fatal(err)
			}
			pointer, pointed := envOf(workerOf(t, r, workerSpec(t)))["DISPATCH_TOKEN_FILE"]
			if got := l.secrets[dispatchTokenKey]; got != tc.bearer || pointed != (tc.bearer != "") {
				t.Fatalf("the claim's Secret carries %q as %s and DISPATCH_TOKEN_FILE is %q (set: %t), want %q",
					got, dispatchTokenKey, pointer, pointed, tc.bearer)
			}
			if want := generationDir(workerSpec(t).Generation) + "/" + dispatchTokenKey; pointed && pointer != want {
				t.Fatalf("DISPATCH_TOKEN_FILE = %q, want the generation's private %s", pointer, want)
			}
		})
	}
}

// workspace-init provisions the issue's workspace at the repository's default branch, which the
// spec names as GitHub does, slash and all: the init container's command passes it verbatim as
// --default-branch's value, and no container of the pod is told main.
func TestTheInitContainerProvisionsAtTheSpecsDefaultBranch(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	spec.DefaultBranch = "release/v7.1"
	command := containerNamed(t, podOf(t, r, spec), initContainer).Command
	at := slices.Index(command, "--default-branch")
	if at < 0 || at+1 >= len(command) || command[at+1] != "release/v7.1" {
		t.Fatalf("the init container's command %q names no --default-branch release/v7.1", command)
	}
	if slices.Contains(command, "main") {
		t.Errorf("the init container's command %q names main", command)
	}
}

// Without agent_secrets a pod carries no projected token of Legion's (the one token a pod carries,
// if any, is the operator's own); with it, exactly one — alone in its volume, for the configured
// audience and lifetime, mounted read-only in the worker container alone — beside a memory-backed
// key directory, the two AGENT_SECRETS_* variables, and the shim's three flags. The probe pod
// carries none of it: it runs no shim and enrolls nothing.
func TestAPodCarriesLegionsTokenExactlyWhenItIsEnrolled(t *testing.T) {
	for name, tc := range map[string]struct {
		secrets *AgentSecrets
		account string
	}{
		"not enrolled, the operator's account": {nil, "operator-worker"},
		"not enrolled, no account":             {nil, ""},
		"enrolled":                             {&AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}, "legion-worker"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := goldenOptions()
			opts.Pod.ServiceAccount = tc.account
			opts.AgentSecrets = tc.secrets
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			l, err := r.prepare(manifestCases(t)["root"].spec)
			if err != nil {
				t.Fatal(err)
			}
			spec := r.podTemplate(l).Spec
			if spec.ServiceAccountName != tc.account || spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
				t.Fatalf("serviceAccountName %q automount %v", spec.ServiceAccountName, spec.AutomountServiceAccountToken)
			}
			var tokens []corev1.Volume
			for _, v := range spec.Volumes {
				if v.Projected != nil && slices.ContainsFunc(v.Projected.Sources, func(s corev1.VolumeProjection) bool { return s.ServiceAccountToken != nil }) {
					tokens = append(tokens, v)
				}
			}
			role := manifestCases(t)["root"].spec.Role
			worker := workerOf(t, r, manifestCases(t)["root"].spec)
			keyVolume := roleVolume(agentSecretsKeyVolume, role)
			env := map[string]string{}
			for _, e := range worker.Env {
				env[e.Name] = e.Value
			}
			if tc.secrets == nil {
				if len(tokens) != 0 || env["AGENT_SECRETS_URL"] != "" || env["AGENT_SECRETS_KEY_DIR"] != "" || slices.Contains(worker.Command, "--agent-secrets-key-dir") {
					t.Fatalf("an unenrolled pod carries agent-secrets pieces: tokens %d, env %v, command %v", len(tokens), env, worker.Command)
				}
				return
			}
			if len(tokens) != 1 || tokens[0].Name != agentSecretsTokenVolume || len(tokens[0].Projected.Sources) != 1 {
				t.Fatalf("token volumes %+v, want exactly %s with one source", tokens, agentSecretsTokenVolume)
			}
			token := tokens[0].Projected.Sources[0].ServiceAccountToken
			if token.Audience != "agent-secrets" || token.ExpirationSeconds == nil || *token.ExpirationSeconds != 3600 || token.Path != AgentSecretsTokenFile {
				t.Fatalf("token source %+v", token)
			}
			mounts := map[string]corev1.VolumeMount{}
			for _, m := range worker.VolumeMounts {
				mounts[m.Name] = m
			}
			if m := mounts[agentSecretsTokenVolume]; m.MountPath != AgentSecretsTokenDir || !m.ReadOnly {
				t.Fatalf("token mount %+v, want read-only at %s", m, AgentSecretsTokenDir)
			}
			if m := mounts[keyVolume]; m.MountPath != roleDir(role)+"/"+launcher.RoleAgentSecretsDir || m.ReadOnly {
				t.Fatalf("key mount %+v, want writable at %s", m, roleDir(role)+"/"+launcher.RoleAgentSecretsDir)
			}
			key := slices.IndexFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == keyVolume })
			if key < 0 || spec.Volumes[key].EmptyDir == nil || spec.Volumes[key].EmptyDir.Medium != corev1.StorageMediumMemory {
				t.Fatalf("key volume %+v, want a memory-backed emptyDir", spec.Volumes[key])
			}
			for _, init := range spec.InitContainers {
				for _, m := range init.VolumeMounts {
					if m.Name == agentSecretsTokenVolume || strings.HasPrefix(m.Name, agentSecretsKeyVolume) {
						t.Fatalf("init container %s mounts %s", init.Name, m.Name)
					}
				}
			}
			if env["AGENT_SECRETS_URL"] != tc.secrets.URL || env["AGENT_SECRETS_KEY_DIR"] != AgentSecretsKeyDir {
				t.Fatalf("env %v", env)
			}
			want := []string{"--agent-secrets-key-dir", AgentSecretsKeyDir, "--pod-token-file", AgentSecretsTokenDir + "/" + AgentSecretsTokenFile, "--agent-secrets-bin", opts.Tools.AgentSecrets}
			joined := strings.Join(worker.Command, "\x00")
			if !strings.Contains(joined, strings.Join(want, "\x00")) || strings.Index(joined, "--agent-secrets-key-dir") > strings.Index(joined, "\x00--\x00") {
				t.Fatalf("shim command %v, want the three flags before --", worker.Command)
			}
			probe := r.probeManifest("legion-probe-test", ImageProbe{Contract: 7}, time.Now().Add(time.Hour)).Spec.PodTemplate.Spec
			for _, v := range probe.Volumes {
				if v.Name == agentSecretsTokenVolume || strings.HasPrefix(v.Name, agentSecretsKeyVolume) {
					t.Fatalf("the probe pod carries %s", v.Name)
				}
			}
		})
	}
}

// A tree role's shim is told --warm-codegraph, before `--`, and the LEGION_WORKSPACE the flag
// needs: once its Oh My Pi has started, the shim builds that workspace's CodeGraph index behind
// the launch (cmd/legion/worker_shim.go). The controller's shim, whose pod has no workspace, is
// not (TestTheControllersAgentIsToldItIsTheControllerAndNothingOfATree).
func TestATreeRolesShimIsToldToWarmTheWorkspacesCodegraphIndex(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	worker := workerOf(t, r, workerSpec(t))
	shim := worker.Command[:slices.Index(worker.Command, "--")]
	if !slices.Contains(shim, "--warm-codegraph") {
		t.Fatalf("the shim runs as %v, want --warm-codegraph before --", worker.Command)
	}
	if envOf(worker)["LEGION_WORKSPACE"] == "" {
		t.Fatalf("the shim is told --warm-codegraph with no LEGION_WORKSPACE to warm; env %v", worker.Env)
	}
	// The shim's wait for that warm-up on its way out is bounded by the stop grace its launcher
	// kills it at, so the shim is told the same TerminationGrace the launcher runs with.
	at := slices.Index(shim, "--stop-grace")
	if at < 0 || at+1 >= len(shim) || shim[at+1] != r.terminationGrace.String() {
		t.Fatalf("the shim runs as %v, want --stop-grace %s, the launcher's own", shim, r.terminationGrace)
	}
}

// New refuses an agent-secrets configuration no pod could run: no broker URL, no token audience,
// an expiry outside the API server's floor (10m) and the cluster's admission cap (1h), an exchange
// audience that is the enrollment's own (the cluster projects one volume per audience), an
// enrollment's or exchange audience that is the Workload Identity pool a microVM pod's metadata
// endpoint holds a token for (the same rule), or the image's agent-secrets binary missing.
func TestNewRefusesAnAgentSecretsOptionNoPodCouldRun(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Options)
		want string
	}{
		"no url":      {func(o *Options) { o.AgentSecrets = &AgentSecrets{Audience: "a", TokenExpiry: time.Hour} }, "agent secrets: no broker URL"},
		"no audience": {func(o *Options) { o.AgentSecrets = &AgentSecrets{URL: "https://s", TokenExpiry: time.Hour} }, "agent secrets: no token audience"},
		"expiry too long": {func(o *Options) {
			o.AgentSecrets = &AgentSecrets{URL: "https://s", Audience: "a", TokenExpiry: 2 * time.Hour}
		}, "agent secrets: token expiry 2h0m0s is not between 10m0s and 1h0m0s"},
		"the exchange audience is the enrollment's": {func(o *Options) {
			o.AgentSecrets = &AgentSecrets{URL: "https://s", Audience: "a", TokenExpiry: time.Hour, ExchangeAudience: "a"}
		}, `agent secrets: the exchange audience is the enrollment's own, "a"`},
		"the enrollment's audience is the Workload Identity pool's": {func(o *Options) {
			o.Sandbox = SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()}
			o.AgentSecrets = &AgentSecrets{URL: "https://s", Audience: testWorkloadPool, TokenExpiry: time.Hour}
		}, `agent secrets: the enrollment's audience is the Workload Identity pool's, "` + testWorkloadPool + `"`},
		"the exchange audience is the Workload Identity pool's": {func(o *Options) {
			o.Sandbox = SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()}
			o.AgentSecrets = &AgentSecrets{URL: "https://s", Audience: "a", TokenExpiry: time.Hour, ExchangeAudience: testWorkloadPool}
		}, `agent secrets: the exchange audience is the Workload Identity pool's, "` + testWorkloadPool + `"`},
		"the enrollment's audience is the Workload Identity pool's on a canary": {func(o *Options) {
			o.Sandbox = SandboxConfig{CanaryIssues: []string{testTree}, MicroVM: testMicroVM()}
			o.AgentSecrets = &AgentSecrets{URL: "https://s", Audience: testWorkloadPool, TokenExpiry: time.Hour}
		}, `agent secrets: the enrollment's audience is the Workload Identity pool's, "` + testWorkloadPool + `"`},
		"no binary": {func(o *Options) {
			o.AgentSecrets = &AgentSecrets{URL: "https://s", Audience: "a", TokenExpiry: time.Hour}
			o.Tools.AgentSecrets = ""
		}, "the image's agent-secrets path \"\" is not absolute"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := goldenOptions()
			tc.edit(&opts)
			_, err := configure(opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("configure() = %v, want %q", err, tc.want)
			}
		})
	}
}

// New accepts a broker audience equal to the Workload Identity pool of a microvm block no issue pod
// uses (a gVisor project with no canary), since no pod carries the pool's token: a block kept for a
// revert changes nothing.
func TestNewAcceptsABrokerAudienceEqualToThePoolOfABlockNoPodUses(t *testing.T) {
	unused := SandboxConfig{Type: SandboxGVisor, MicroVM: testMicroVM()}
	for name, secrets := range map[string]*AgentSecrets{
		"the enrollment's audience": {URL: "https://s", Audience: testWorkloadPool, TokenExpiry: time.Hour},
		"the exchange audience":     {URL: "https://s", Audience: "a", ExchangeAudience: testWorkloadPool, TokenExpiry: time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			sandboxRuntime(t, unused, func(o *Options) { o.AgentSecrets = secrets })
		})
	}
}

// New accepts a microvm block no issue pod uses that names no compute class, as the loader does
// (it requires compute_class only while an issue pod runs in a microVM), so a revert that empties
// canary_issues can leave such a block in place.
func TestNewAcceptsAMicroVMBlockNoPodUsesWithNoComputeClass(t *testing.T) {
	sandboxRuntime(t, SandboxConfig{Type: SandboxGVisor, MicroVM: &MicroVM{StorageClass: testMicroVMStorageClass}})
}

// operatorPod is an operator's runtime.kubernetes.pod: a variable, a Secret volume at mode 0440
// (default_mode), a ConfigMap volume mounted by sub_path into the profile's agent directory, a
// projected ServiceAccount token alone in its volume, an existing share by its
// PersistentVolumeClaim mounted read-only, an NFS export mounted writable, and the account the pods
// run as.
func operatorPod() Pod {
	expiry := int64(3600)
	return Pod{
		Env: map[string]string{"PI_CONFIG_FILES": "/etc/legion-operator/overlay.yml"},
		Volumes: []corev1.Volume{
			{Name: "operator-creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "legion-operator-creds", DefaultMode: new(int32(0o440))}}},
			{Name: "operator-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator"},
				Items:                []corev1.KeyToPath{{Key: "models.yml", Path: "models.yml"}},
			}}},
			{Name: "operator-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
				ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "operator-audience", ExpirationSeconds: &expiry, Path: "token"},
			}}}}},
			{Name: "operator-share", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "team-share"}}},
			{Name: "operator-nas", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nas.internal.example", Path: "/exports/builds"}}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "operator-creds", MountPath: "/etc/legion-operator/creds", ReadOnly: true},
			{Name: "operator-config", MountPath: ompAgentDir + "/models.yml", SubPath: "models.yml", ReadOnly: true},
			{Name: "operator-token", MountPath: "/var/run/operator", ReadOnly: true},
			{Name: "operator-share", MountPath: "/mnt/team", ReadOnly: true},
			{Name: "operator-nas", MountPath: "/mnt/nas"},
		},
		ServiceAccount: "operator-worker",
	}
}

// The operator's pod (runtime.kubernetes.pod) reaches every pod Legion runs — the root's, a
// worker's, the daemon-launched controller's and the image probe's — exactly as configured: its
// volumes beside Legion's, its variables and mounts in the agent's container alone, never in an
// init container, and its account as the pod's. The controller's account being the issue pods'
// is what makes it the Legion daemon service's own pod to the secrets broker, so it is granted the
// secrets that service owns.
func TestTheOperatorsPodReachesEveryPodLegionRuns(t *testing.T) {
	opts := testOptions()
	opts.Pod = operatorPod()
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	probeSpec := r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec
	for _, tc := range []struct {
		name  string
		pod   corev1.PodSpec
		agent corev1.Container
	}{
		{"root", podOf(t, r, rootSpec(t)), workerOf(t, r, rootSpec(t))},
		{"worker", podOf(t, r, workerSpec(t)), workerOf(t, r, workerSpec(t))},
		{"controller", podOf(t, r, controllerSpec(t)), workerOf(t, r, controllerSpec(t))},
		{"probe", probeSpec, containerNamed(t, probeSpec, probeContainer)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.pod.ServiceAccountName != opts.Pod.ServiceAccount {
				t.Errorf("serviceAccountName = %q, want the operator's %q", tc.pod.ServiceAccountName, opts.Pod.ServiceAccount)
			}
			for _, want := range opts.Pod.Volumes {
				var got []corev1.Volume
				for _, volume := range tc.pod.Volumes {
					if volume.Name == want.Name {
						got = append(got, volume)
					}
				}
				if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
					t.Errorf("the pod's volumes named %s are %+v, want exactly %+v", want.Name, got, want)
				}
			}
			agent := tc.agent
			for _, want := range opts.Pod.VolumeMounts {
				// An issue pod's roles container holds what belongs under the home under
				// HomeMountsDir (homeMountsView); every other pod mounts it as configured.
				if tc.name == "root" || tc.name == "worker" {
					want = homeMountsView([]corev1.VolumeMount{want})[0]
				}
				if !slices.ContainsFunc(agent.VolumeMounts, func(m corev1.VolumeMount) bool { return reflect.DeepEqual(m, want) }) {
					t.Errorf("%s mounts %+v, want %+v among them", agent.Name, agent.VolumeMounts, want)
				}
			}
			if got := envOf(agent)["PI_CONFIG_FILES"]; got != opts.Pod.Env["PI_CONFIG_FILES"] {
				t.Errorf("%s's PI_CONFIG_FILES = %q, want the operator's %q", agent.Name, got, opts.Pod.Env["PI_CONFIG_FILES"])
			}
			for _, init := range tc.pod.InitContainers {
				for _, mount := range init.VolumeMounts {
					if strings.HasPrefix(mount.Name, "operator-") {
						t.Errorf("%s mounts the operator's %s", init.Name, mount.Name)
					}
				}
				if _, set := envOf(init)["PI_CONFIG_FILES"]; set {
					t.Errorf("%s is told the operator's PI_CONFIG_FILES", init.Name)
				}
			}
		})
	}
}

// The providers Secret — the TypeScript runtime's legion-<project token>-providers — reaches every
// pod Legion runs, a worker's and the image probe's alike, as exactly its configured keys, read-only
// at ProvidersDir in the agent's container alone: each provider key as a file named for the variable
// Oh My Pi reads, which the worker's shim exports into Oh My Pi's environment (--provider-env-dir),
// each providers secret (the NATS nkey seed) as a file of its own name that the container's
// `<NAME>_FILE` names, so the shim exports it to no one, and the session database's URL key last.
// The pointer is the runtime's, not the spec's: a spec that carries no seed still gets it, and the
// claim's Secret never holds a copy. With neither a provider key nor a providers secret the Secret
// is still mounted under the postgres store, holding the session database's URL alone, so no pod of
// that store runs without the volume and no shim without the directory.
func TestTheProvidersSecretReachesEveryPodAsItsOwnFiles(t *testing.T) {
	const providersSecret = "legion-" + testProject + "-providers"
	const seed = "SUAIBDPBAUTWCWBKIO6XHQNINK5FWJW4OHLXC3HQ2KFE4PEJUA44CNHTC4"
	nats := []string{"NATS_NKEY_SEED"}
	natsItem := corev1.KeyToPath{Key: "NATS_NKEY_SEED", Path: "NATS_NKEY_SEED"}
	anthropic := corev1.KeyToPath{Key: "anthropic", Path: "ANTHROPIC_API_KEY"}
	sessionItem := corev1.KeyToPath{Key: testSessionDSNKey, Path: ompsessions.DSNFileName}
	for _, tc := range []struct {
		name      string
		keys      map[string]string
		providers []string
		specSeed  bool
		items     []corev1.KeyToPath
	}{
		{"provider keys", map[string]string{"GEMINI_API_KEY": "gemini", "ANTHROPIC_API_KEY": "anthropic"}, nil, false,
			[]corev1.KeyToPath{anthropic, {Key: "gemini", Path: "GEMINI_API_KEY"}, sessionItem}},
		{"the session database's URL alone", nil, nil, false, []corev1.KeyToPath{sessionItem}},
		{"the seed with provider keys", map[string]string{"ANTHROPIC_API_KEY": "anthropic"}, nats, true, []corev1.KeyToPath{anthropic, natsItem, sessionItem}},
		{"the seed alone", nil, nats, true, []corev1.KeyToPath{natsItem, sessionItem}},
		{"the seed, on a spec that carries none", nil, nats, false, []corev1.KeyToPath{natsItem, sessionItem}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			opts.ProviderKeys, opts.ProvidersSecrets = tc.keys, tc.providers
			opts.LaunchSecrets = []string{"ENVOY_TOKEN", "NATS_NKEY_SEED"}
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			spec := workerSpec(t)
			spec.Secrets = map[string]string{"ENVOY_TOKEN": "envoy-bearer"}
			if tc.specSeed {
				spec.Secrets["NATS_NKEY_SEED"] = seed
			}
			l, err := r.prepare(spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, copied := l.secrets["NATS_NKEY_SEED"]; copied {
				t.Errorf("the claim's Secret carries NATS_NKEY_SEED: %v", slices.Sorted(maps.Keys(l.secrets)))
			}
			worker := r.podTemplate(l).Spec
			probe := r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec
			workerProcess := workerOf(t, r, spec)
			for _, pod := range []struct {
				spec      corev1.PodSpec
				container string
				agent     corev1.Container
			}{{worker, workerContainer, workerProcess}, {probe, probeContainer, containerNamed(t, probe, probeContainer)}} {
				var volumes []corev1.Volume
				for _, volume := range pod.spec.Volumes {
					if volume.Secret != nil && volume.Secret.SecretName == providersSecret {
						volumes = append(volumes, volume)
					}
					if volume.Secret != nil && strings.HasPrefix(volume.Secret.SecretName, SandboxName(workerToken)) {
						for _, item := range volume.Secret.Items {
							if item.Key == "NATS_NKEY_SEED" {
								t.Errorf("%s's boot projection carries NATS_NKEY_SEED", pod.container)
							}
						}
					}
				}
				mounted := func(c corev1.Container) []corev1.VolumeMount {
					var mounts []corev1.VolumeMount
					for _, mount := range c.VolumeMounts {
						if slices.ContainsFunc(volumes, func(v corev1.Volume) bool { return v.Name == mount.Name }) || mount.MountPath == ProvidersDir {
							mounts = append(mounts, mount)
						}
					}
					return mounts
				}
				agent := pod.agent
				for _, init := range pod.spec.InitContainers {
					if got := mounted(init); len(got) != 0 {
						t.Errorf("%s mounts the providers Secret: %+v", init.Name, got)
					}
				}
				want := &corev1.SecretVolumeSource{SecretName: providersSecret, Items: tc.items, DefaultMode: new(int32(0o440))}
				if len(volumes) != 1 || !reflect.DeepEqual(volumes[0].Secret, want) {
					t.Errorf("%s's pod holds the providers Secret as %+v, want once as %+v", pod.container, volumes, *want)
				}
				if got := mounted(agent); len(got) != 1 || got[0].MountPath != ProvidersDir || !got[0].ReadOnly || got[0].SubPath != "" {
					t.Errorf("%s mounts the providers Secret as %+v, want once, read-only, at %s", pod.container, got, ProvidersDir)
				}
				env := envOf(agent)
				pointer, pointed := env["NATS_NKEY_SEED_FILE"]
				if want := tc.providers != nil; pointed != want || (want && pointer != ProvidersDir+"/NATS_NKEY_SEED") {
					t.Errorf("%s's NATS_NKEY_SEED_FILE = %q (set: %t), want %s/NATS_NKEY_SEED set %t", pod.container, pointer, pointed, ProvidersDir, want)
				}
				for name, value := range env {
					if strings.Contains(value, seed) {
						t.Errorf("%s's %s carries the seed", pod.container, name)
					}
				}
				if _, set := env["NATS_NKEY_SEED"]; set {
					t.Errorf("%s sets NATS_NKEY_SEED", pod.container)
				}
			}
			main := workerProcess
			shim := main.Command[:slices.Index(main.Command, "--")]
			at := slices.Index(shim, "--provider-env-dir")
			if at < 0 || at+1 == len(shim) || shim[at+1] != ProvidersDir {
				t.Errorf("the shim runs as %v, want --provider-env-dir %s", shim, ProvidersDir)
			}
		})
	}
}

// A providers secret must be a launch secret: CheckPod refuses a provider key or operator variable
// colliding with a launch secret's pointer, which is what keeps a providers secret's file and
// pointer the runtime's alone.
func TestNewRefusesAProvidersSecretThatIsNoLaunchSecret(t *testing.T) {
	opts := testOptions()
	opts.LaunchSecrets, opts.ProvidersSecrets = []string{"ENVOY_TOKEN"}, []string{"NATS_NKEY_SEED"}
	if _, err := configure(opts); err == nil || err.Error() != "sandbox runtime: providers secret NATS_NKEY_SEED is not a launch secret (ENVOY_TOKEN)" {
		t.Fatalf("configure = %v, want the refusal naming NATS_NKEY_SEED", err)
	}
}

// legionVolumeNames, legionMountPaths, and runtimeOwned with the composed names — what CheckPod
// knows of as Legion's in the operator's pod, the exchange identity's names among them where the
// deployment names an exchange audience — are exactly what Legion's own pods carry: every
// volume of every pod a launch or
// the image probe runs, and every mount and variable of the containers the operator's pieces join
// (the agent's and the probe's), with every optional piece the runtime adds configured and nothing
// of a spec's or the operator's, under an enrollment that names an exchange audience and under one
// that names none, whose pods carry none of the exchange identity, and under a session store that
// names a writer audience and one that names none. Among the goldens' cases is a
// Claude Code worker, which alone is
// told three of the variables; none of them works a repository the project names, so the worker
// below, which does, is built here for the one variable such a pod alone carries. A piece added
// to a pod and not to its list is one an operator could collide with unrefused; one left in a
// list that no pod carries is refused for nothing.
func TestLegionsOwnNamesAreWhatItsPodsCarry(t *testing.T) {
	opts := goldenOptions()
	opts.DispatchURL, opts.DispatchToken = "https://dispatch.internal", "dispatch-bearer"
	opts.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "anthropic"}
	// With every optional piece configured: the pod's own test database is one (testPGVolume), and
	// a microVM project's metadata endpoint another (workloadIdentityTokenVolume, under RolesDir in
	// an issue pod and at ControllerWorkloadIdentityDir in the controller's).
	opts.TestPostgres = true
	opts.Sandbox = SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()}
	for name, agentSecrets := range map[string]*AgentSecrets{
		"the exchange identity on":  exchangeEnrolled(),
		"the exchange identity off": rootEnrolled(),
	} {
		t.Run(name, func(t *testing.T) {
			var r *Runtime
			volumes, mounts, env := map[string]bool{}, map[string]bool{}, map[string]bool{}
			carry := func(pod corev1.PodSpec, agent corev1.Container) {
				for _, volume := range pod.Volumes {
					volumes[volume.Name] = true
				}
				for _, mount := range agent.VolumeMounts {
					// Every role's own mounts lie under RolesDir, which is reserved whole; a
					// container that runs the launcher's roles-dir form holds what belongs under the
					// home under HomeMountsDir, which is reserved whole too, at the path each is
					// bound back at.
					switch path := mount.MountPath; {
					case path == RolesDir || strings.HasPrefix(path, RolesDir+"/"):
						mounts[RolesDir] = true
					case strings.HasPrefix(path, HomeMountsDir+"/"):
						mounts[HomeMountsDir] = true
						mounts[podHome+strings.TrimPrefix(path, HomeMountsDir)] = true
					default:
						mounts[path] = true
					}
				}
				for _, variable := range agent.Env {
					env[variable.Name] = true
				}
			}
			// Each session store the pods are built under: the goldens' own, and one that names a
			// writer audience, whose pods carry the session writer token (sessionWriterTokenVolume,
			// under RolesDir) and whose controller lays its role out under RolesDir as an issue pod
			// does, mounting nothing at the fixed paths its single-role form mounts at. Neither
			// store's pods carry every name of Legion's alone.
			for _, store := range []ompsessions.Store{opts.SessionStore, cloudSQLWriterStore} {
				opts := opts
				opts.AgentSecrets, opts.SessionStore = agentSecrets, store
				var err error
				if r, err = configure(opts); err != nil {
					t.Fatal(err)
				}
				for _, tc := range manifestCases(t) {
					spec := tc.spec
					spec.Env, spec.Secrets = nil, nil
					pod := podOf(t, r, spec)
					carry(pod, workerOf(t, r, spec))
				}
				// A pane whose repository the project names is told the issue key it stamps its
				// commits with (LEGION_ISSUE_TRAILER), and no golden case works such a repository.
				stamping := workerSpec(t)
				stamping.Env, stamping.Secrets = nil, nil
				stamping.IssueTrailer = stamping.Issue
				carry(podOf(t, r, stamping), workerOf(t, r, stamping))
				probe := r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec
				carry(probe, containerNamed(t, probe, probeContainer))
			}
			// The one path no container mounts: role-init binds the pod's test server log there in
			// every role's namespace, over the file the worker image makes.
			mounts[TestPGLogFile] = true
			exchange := agentSecrets.ExchangeAudience != ""
			for _, list := range []struct {
				name    string
				got     []string
				carried map[string]bool
			}{
				{"legionVolumeNames", legionVolumeNames(append(slices.Clone(claim.Roles), claim.RoleController), exchange), volumes},
				{"legionMountPaths", legionMountPaths(exchange), mounts},
				{"the names a spec may not set", slices.Collect(maps.Keys(r.specOwned())), env},
			} {
				listed := map[string]bool{}
				for _, entry := range list.got {
					listed[entry] = true
				}
				if !maps.Equal(listed, list.carried) {
					t.Errorf("%s is %v, and Legion's pods carry %v", list.name, slices.Sorted(maps.Keys(listed)), slices.Sorted(maps.Keys(list.carried)))
				}
			}
		})
	}
}

// imageOwnedPaths covers every path in the image a pod runs or loads from, which an operator's
// mount there would hide: Oh My Pi, the Envoy and Legion plugins the agent loads, the Go legion
// every container runs, the profile's installed plugins, the databases Oh My Pi keeps in the
// profile's agent directory, and a Claude role's bun, claude-envoy root and the Claude Code pin its
// start script installs from. Claude Code itself is not one: the image carries none.
func TestImageOwnedPathsCoverWhatAPodRunsFromTheImage(t *testing.T) {
	for _, used := range []string{
		defaultAgent, envoyPlugin, legionPlugin, testOptions().Tools.Legion,
		ompProfileDir + "/plugins/node_modules", ompAgentDir + "/agent.db", ompAgentDir + "/models.db",
		claudeBun, claudePluginDir, claudeCodePin + "/package.json", claudeCodePin + "/package-lock.json",
	} {
		if !slices.ContainsFunc(imageOwnedPaths(), func(owned string) bool {
			return used == owned || strings.HasPrefix(used, owned+"/")
		}) {
			t.Errorf("%s is not under imageOwnedPaths %v", used, imageOwnedPaths())
		}
	}
}

// Every Legion pod, an issue's and the controller's alike, asks nothing of its placement beyond the
// Legion pool and, for a microVM pod, its ComputeClass: of its template's fields, the ones that
// bear on scheduling are the node selector and tolerations and the Legion priority class, and no
// other — nothing ties it to, or keeps it off, another pod's node. Each issue pod owns a
// ReadWriteOnce volume of its own and shares nothing with the other pods of its tree, so the
// scheduler places it wherever the pool has room (LEGION-632). Pinned as the exact key set of the
// pod spec the manifest sends, so a field added to the template shows here by name; the
// controller's pod alone sends no initContainers, since it runs none. The selector and the
// tolerations are pinned whole too: the pool's label and taint alone on a gVisor pod, and on a
// microVM pod — an issue's whose issue runs in one (SandboxConfig.Of), or the controller's of a
// microvm project (SandboxConfig.Controller) — those and its ComputeClass's label and taint alone.
func TestEveryLegionPodAsksNothingOfItsPlacementBeyondThePool(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"automountServiceAccountToken", "containers", "enableServiceLinks", "initContainers", "nodeSelector",
		"priorityClassName", "restartPolicy", "runtimeClassName", "securityContext", "terminationGracePeriodSeconds",
		"tolerations", "volumes",
	}
	taint := func(key, value string) map[string]any {
		return map[string]any{"key": key, "operator": "Equal", "value": value, "effect": "NoSchedule"}
	}
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			tc.apply(r)
			manifest := manifestOf(t, r, tc.spec).(map[string]any)
			pod := manifest["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"].(map[string]any)
			expected := want
			if tc.spec.Role == claim.RoleController {
				expected = slices.DeleteFunc(slices.Clone(want), func(key string) bool { return key == "initContainers" })
			}
			if got := slices.Sorted(maps.Keys(pod)); !slices.Equal(got, expected) {
				t.Errorf("the pod spec sent carries %v, want exactly %v", got, expected)
			}
			selector, tolerations := map[string]any{poolKey: poolValue}, []any{taint(poolKey, poolValue)}
			where := tc.sandbox.Of(tc.spec.Issue)
			if tc.spec.Role == claim.RoleController {
				where = tc.sandbox.Controller()
			}
			if where == SandboxMicroVM {
				class := tc.sandbox.MicroVM.ComputeClass
				selector[computeClassKey] = class
				tolerations = append(tolerations, taint(computeClassKey, class))
			}
			if got := pod["nodeSelector"]; !reflect.DeepEqual(got, selector) {
				t.Errorf("the pod selects %v, want exactly %v", got, selector)
			}
			if got := pod["tolerations"]; !reflect.DeepEqual(got, tolerations) {
				t.Errorf("the pod tolerates %v, want exactly %v", got, tolerations)
			}
		})
	}
}

// An issue's Sandbox owns no volume at all: its pod's disk is a generic ephemeral volume declared
// in the pod template, which Kubernetes creates with the pod and deletes with the pod, so an
// issue's work never outlives the pod that did it (INFRA-80). The claim it is made from is the
// operator's StorageClass, a microVM pod's the microVM block's, at the operator's size, and
// carries no metadata: the pinned CRD declares ephemeral.volumeClaimTemplate.metadata a bare object
// with no properties, so a label set there would be pruned by the API server without a word
// (TestManifestMatchesTheSandboxCRD). The one exception is a microVM pod under keep_volume, whose
// Sandbox owns its kept volume (TestOnlyAMicroVMPodKeepsItsVolume), so its case is left out here.
func TestAnIssuePodOwnsItsDiskAndItsSandboxOwnsNoVolume(t *testing.T) {
	opts := goldenOptions()
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			tc.apply(r)
			l, err := r.prepare(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			if l.kind == (controllerPod{}) {
				t.Skip("the controller's Sandbox owns its volume")
			}
			if keptVolume(issuePod{}, tc.sandbox.MicroVM) {
				t.Skip("a microVM pod's Sandbox owns its kept volume (TestOnlyAMicroVMPodKeepsItsVolume)")
			}
			if got := r.sandboxManifest(l).Spec.VolumeClaimTemplates; len(got) != 0 {
				t.Errorf("the issue's Sandbox owns %+v, want no volume claim template: its pod owns its disk", got)
			}
			template := podDisk(t, r.podTemplate(l).Spec)
			if !reflect.ValueOf(template.ObjectMeta).IsZero() {
				t.Errorf("its claim template carries metadata %+v, want none: the CRD prunes every field of it", template.ObjectMeta)
			}
			storageClass := opts.StorageClass
			if tc.sandbox.Of(tc.spec.Issue) == SandboxMicroVM {
				storageClass = tc.sandbox.MicroVM.StorageClass
			}
			want := corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: &storageClass,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: opts.IssueVolume},
				},
			}
			if !reflect.DeepEqual(template.Spec, want) {
				t.Errorf("its claim is %+v, want %+v", template.Spec, want)
			}
		})
	}
}

// The controller's Sandbox is the one this runtime still has own a volume claim template, so a
// relaunch finds the working root its last pod left on it; its session is a row in the session
// database, as every role's is. The template's labels are its pod's: the project and the
// controller role, and no tree, which the controller has none of. The daemon's
// restricted identity has no PVC verb, so the labels the claim is created with are the labels it
// keeps for life (LEGION-632).
func TestTheControllersClaimTemplateCarriesItsProjectAndRoleAlone(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			tc.apply(r)
			l, err := r.prepare(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			if l.kind != (controllerPod{}) {
				t.Skip("an issue's Sandbox owns no claim template, or its kept volume's (TestOnlyAMicroVMPodKeepsItsVolume)")
			}
			templates := r.sandboxManifest(l).Spec.VolumeClaimTemplates
			if len(templates) != 1 || templates[0].Metadata.Name != issueVolume {
				t.Fatalf("the controller's Sandbox owns %+v, want the one %s template", templates, issueVolume)
			}
			want := map[string]string{labelProject: testProject, labelRole: string(claim.RoleController)}
			if got := templates[0].Metadata.Labels; !maps.Equal(got, want) {
				t.Errorf("the claim template carries %v, want exactly %v", got, want)
			}
		})
	}
}

// The microVM block the tests configure, each name a placeholder: the ComputeClass whose nodes run
// the microVM pods, their disks' own StorageClass, and the metadata endpoint's Workload Identity
// pool and identity provider.
const (
	testComputeClass        = "example-microvm"
	testMicroVMStorageClass = "example-microvm-rwo"
	testWorkloadPool        = "example-project.svc.id.goog"
	testIdentityProvider    = "https://container.googleapis.com/v1/projects/example-project/locations/us-central1/clusters/example-cluster"
)

// testMicroVM is the tests' microvm block, with the metadata endpoint.
func testMicroVM() *MicroVM {
	return &MicroVM{
		ComputeClass: testComputeClass, StorageClass: testMicroVMStorageClass,
		Metadata: &MetadataEndpoint{WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider},
	}
}

// keptMicroVM is testMicroVM with keep_volume on.
func keptMicroVM() *MicroVM {
	block := testMicroVM()
	block.KeepVolume = true
	return block
}

// sandboxRuntime is the runtime of goldenOptions under the sandbox block, with edits applied.
func sandboxRuntime(t *testing.T, block SandboxConfig, edits ...func(*Options)) *Runtime {
	t.Helper()
	opts := goldenOptions()
	opts.Sandbox = block
	for _, edit := range edits {
		edit(&opts)
	}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// sandboxOf is a settled runtime.kubernetes.sandbox as the daemon hands it to the runtime
// (internal/daemon/kubernetes.go, sandboxConfig): member for member.
func sandboxOf(c config.SandboxConfig) SandboxConfig {
	block := SandboxConfig{Type: SandboxType(c.Type), CanaryIssues: c.CanaryIssues}
	if m := c.MicroVM; m != nil {
		block.MicroVM = &MicroVM{ComputeClass: m.ComputeClass, StorageClass: m.StorageClass, KeepVolume: m.KeepVolume}
		if endpoint := m.Metadata; endpoint != nil {
			block.MicroVM.Metadata = &MetadataEndpoint{WorkloadPool: endpoint.WorkloadPool, IdentityProvider: endpoint.IdentityProvider}
		}
	}
	return block
}

// poolToleration is the Legion pool's taint, tolerated, and computeClassToleration the taint GKE
// puts on the nodes of the custom ComputeClass class.
var poolToleration = corev1.Toleration{Key: poolKey, Operator: corev1.TolerationOpEqual, Value: poolValue, Effect: corev1.TaintEffectNoSchedule}

func computeClassToleration(class string) corev1.Toleration {
	return corev1.Toleration{Key: computeClassKey, Operator: corev1.TolerationOpEqual, Value: class, Effect: corev1.TaintEffectNoSchedule}
}

// metadataFlags are the launcher's two flags of the metadata endpoint.
var metadataFlags = []string{"--metadata-workload-pool", "--metadata-identity-provider"}

// renderedSandbox is the sandbox template's pod runs in, read from each mark a microVM pod carries
// and a gVisor pod does not: the microVM RuntimeClass, the selector and the toleration of
// testComputeClass, the sandbox label, and --microvm on the launcher's command. A pod with every
// mark runs in a microVM, and one with none in gVisor, whose RuntimeClass must then be gVisor's and
// whose labels must hold no sandbox label at all; a pod with some marks and not others fails the
// test.
func renderedSandbox(t *testing.T, template podTemplate) SandboxType {
	t.Helper()
	pod := template.Spec
	marks := map[string]bool{
		"runtimeClassName " + microVMRuntimeClass:                       pod.RuntimeClassName != nil && *pod.RuntimeClassName == microVMRuntimeClass,
		"the node selector " + computeClassKey + "=" + testComputeClass: pod.NodeSelector[computeClassKey] == testComputeClass,
		"the toleration of " + testComputeClass:                         slices.Contains(pod.Tolerations, computeClassToleration(testComputeClass)),
		"the label " + labelSandbox + "=microvm":                        template.Metadata.Labels[labelSandbox] == "microvm",
		"--microvm on the launcher":                                     slices.Contains(pod.Containers[0].Command, "--microvm"),
	}
	var carried, lacked []string
	for _, mark := range slices.Sorted(maps.Keys(marks)) {
		if marks[mark] {
			carried = append(carried, mark)
		} else {
			lacked = append(lacked, mark)
		}
	}
	switch {
	case len(lacked) == 0:
		return SandboxMicroVM
	case len(carried) > 0:
		t.Fatalf("the pod carries %q of a microVM pod's marks and lacks %q", carried, lacked)
	}
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != gvisor {
		t.Fatalf("a pod with no microVM mark runs under the RuntimeClass %s, want %s", runtimeClassText(pod.RuntimeClassName), gvisor)
	}
	if value, ok := template.Metadata.Labels[labelSandbox]; ok {
		t.Fatalf("a gVisor pod carries the label %s=%q", labelSandbox, value)
	}
	return SandboxGVisor
}

// checkNoMetadataEndpoint fails the test when pod carries any part of the metadata endpoint: the
// workload identity token's volume, a mount of it or at WorkloadIdentityDir or
// ControllerWorkloadIdentityDir in any container, or either metadata flag on any container's
// command.
func checkNoMetadataEndpoint(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	for _, volume := range pod.Volumes {
		if volume.Name == workloadIdentityTokenVolume {
			t.Errorf("the pod carries the volume %+v", volume)
		}
	}
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		for _, mount := range c.VolumeMounts {
			if mount.Name == workloadIdentityTokenVolume || mount.MountPath == WorkloadIdentityDir || mount.MountPath == ControllerWorkloadIdentityDir {
				t.Errorf("%s mounts %+v", c.Name, mount)
			}
		}
		for _, flag := range metadataFlags {
			if slices.Contains(c.Command, flag) {
				t.Errorf("%s runs %q, which carries %s", c.Name, c.Command, flag)
			}
		}
	}
}

// A project that names no sandbox, or names gVisor beside a microvm block no issue pod uses (what
// a revert of a canary leaves in place), renders today's pods: every gVisor case of the goldens
// byte for byte, and each pod with none of a microVM pod's marks — no microvm RuntimeClass, no
// legion.dev/sandbox label, no compute class selector or toleration, no --microvm — and no part of
// the metadata endpoint, which the inert block names.
func TestAGVisorProjectRendersTodaysIssuePod(t *testing.T) {
	for name, block := range map[string]SandboxConfig{
		"no sandbox block":                     {},
		"gvisor beside an inert microvm block": {Type: SandboxGVisor, MicroVM: testMicroVM()},
	} {
		t.Run(name, func(t *testing.T) {
			r := sandboxRuntime(t, block)
			for name, tc := range manifestCases(t) {
				if tc.sandbox.AnyMicroVM() {
					continue
				}
				t.Run(name, func(t *testing.T) {
					// The case's enrollment alone: its sandbox, gVisor's, is the block's here.
					r.agentSecrets = tc.agentSecrets
					checkGolden(t, name, manifestOf(t, r, tc.spec))
					template := r.podTemplate(launchOf(t, r, tc.spec))
					if got := renderedSandbox(t, template); got != SandboxGVisor {
						t.Fatalf("the pod runs in %s, want gVisor", got)
					}
					if class, ok := template.Spec.NodeSelector[computeClassKey]; ok {
						t.Errorf("the pod selects the compute class %q", class)
					}
					for _, toleration := range template.Spec.Tolerations {
						if toleration.Key == computeClassKey {
							t.Errorf("the pod tolerates %+v", toleration)
						}
					}
					checkNoMetadataEndpoint(t, template.Spec)
				})
			}
		})
	}
}

// A microVM project runs each of its issue pods on GKE Sandbox's microVM RuntimeClass, on the Legion
// pool's nodes of its ComputeClass: the selector is the pool's and the operator's with the class
// beside them, the tolerations the pool's and the operator's with the class's taint after them, and
// the pod is labelled legion.dev/sandbox=microvm, the label a NetworkPolicy selects it by.
func TestAMicroVMProjectRendersItsIssuePodsOnTheMicroVMClass(t *testing.T) {
	dedicated := corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "legion", Effect: corev1.TaintEffectNoSchedule}
	r := sandboxRuntime(t, SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()}, func(o *Options) {
		o.Scheduling.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
		o.Scheduling.Tolerations = []corev1.Toleration{dedicated}
	})
	for name, spec := range map[string]runtime.SpawnSpec{"the root's": rootSpec(t), "another issue's": childSpec(t)} {
		t.Run(name, func(t *testing.T) {
			template := r.podTemplate(launchOf(t, r, spec))
			pod := template.Spec
			if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "microvm" {
				t.Errorf("the pod runs under the RuntimeClass %s, want microvm", runtimeClassText(pod.RuntimeClassName))
			}
			selector := map[string]string{poolKey: poolValue, "kubernetes.io/arch": "amd64", computeClassKey: testComputeClass}
			if !maps.Equal(pod.NodeSelector, selector) {
				t.Errorf("the pod selects %v, want exactly %v", pod.NodeSelector, selector)
			}
			tolerations := []corev1.Toleration{poolToleration, dedicated, computeClassToleration(testComputeClass)}
			if !reflect.DeepEqual(pod.Tolerations, tolerations) {
				t.Errorf("the pod tolerates %+v, want exactly %+v", pod.Tolerations, tolerations)
			}
			if got := template.Metadata.Labels[labelSandbox]; got != "microvm" {
				t.Errorf("the pod is labelled %s=%q, want microvm", labelSandbox, got)
			}
			if got := renderedSandbox(t, template); got != SandboxMicroVM {
				t.Errorf("the pod runs in %s, want a microVM", got)
			}
		})
	}
}

// An issue's sandbox is the project's type, or the other one when the canary list names the
// issue's exact key: a key the list's entry is only a prefix of takes the type, and the zero
// value, which no loaded file holds, reads as gVisor.
func TestAnIssuesSandboxIsTheTypeOrTheOtherOneForItsExactKeyOnTheCanaryList(t *testing.T) {
	gVisorProject := SandboxConfig{Type: SandboxGVisor, CanaryIssues: []string{"LEGSMOKE-7"}}
	microVMProject := SandboxConfig{Type: SandboxMicroVM, CanaryIssues: []string{"LEGSMOKE-7"}}
	for _, tc := range []struct {
		name   string
		config SandboxConfig
		issue  string
		want   SandboxType
	}{
		{"a canary of a gVisor project", gVisorProject, "LEGSMOKE-7", SandboxMicroVM},
		{"another issue of a gVisor project", gVisorProject, "LEGSMOKE-8", SandboxGVisor},
		{"a key the canary's is a prefix of", gVisorProject, "LEGSMOKE-70", SandboxGVisor},
		{"a canary of a microVM project", microVMProject, "LEGSMOKE-7", SandboxGVisor},
		{"another issue of a microVM project", microVMProject, "LEGSMOKE-8", SandboxMicroVM},
		{"an issue of a gVisor project with no canary", SandboxConfig{Type: SandboxGVisor}, "LEGSMOKE-7", SandboxGVisor},
		{"an issue under the zero value", SandboxConfig{}, "LEGSMOKE-7", SandboxGVisor},
	} {
		if got := tc.config.Of(tc.issue); got != tc.want {
			t.Errorf("%s: Of(%s) = %s, want %s", tc.name, tc.issue, got, tc.want)
		}
	}
}

// A gVisor project whose canary list names an issue runs that issue's pod in a microVM, and every
// other issue's in gVisor, as a project with no sandbox block renders it: the root's Sandbox is its
// golden byte for byte.
func TestACanaryIssueOfAGVisorProjectRendersAMicroVMPod(t *testing.T) {
	r := sandboxRuntime(t, SandboxConfig{Type: SandboxGVisor, CanaryIssues: []string{"LEGION-209"}, MicroVM: testMicroVM()})
	if got := renderedSandbox(t, r.podTemplate(launchOf(t, r, childSpec(t)))); got != SandboxMicroVM {
		t.Errorf("the canary LEGION-209's pod runs in %s, want a microVM", got)
	}
	if got := renderedSandbox(t, r.podTemplate(launchOf(t, r, rootSpec(t)))); got != SandboxGVisor {
		t.Errorf("LEGION-208's pod runs in %s, want gVisor", got)
	}
	checkGolden(t, "root", manifestOf(t, r, rootSpec(t)))
}

// An issue on the canary list of a microVM project runs in gVisor, the sandbox the project's type
// does not name: its Sandbox is the one a project with no sandbox block renders for it, while every
// other issue's pod runs in a microVM.
func TestAnIssueOfAMicroVMProjectOnTheCanaryListRendersAGVisorPod(t *testing.T) {
	r := sandboxRuntime(t, SandboxConfig{Type: SandboxMicroVM, CanaryIssues: []string{"LEGION-209"}, MicroVM: testMicroVM()})
	if got := renderedSandbox(t, r.podTemplate(launchOf(t, r, childSpec(t)))); got != SandboxGVisor {
		t.Errorf("the canary LEGION-209's pod runs in %s, want gVisor", got)
	}
	plain := sandboxRuntime(t, SandboxConfig{})
	if got, want := manifestOf(t, r, childSpec(t)), manifestOf(t, plain, childSpec(t)); !reflect.DeepEqual(got, want) {
		t.Errorf("the canary's Sandbox is\n%v\nwant a gVisor project's\n%v", got, want)
	}
	if got := renderedSandbox(t, r.podTemplate(launchOf(t, r, rootSpec(t)))); got != SandboxMicroVM {
		t.Errorf("LEGION-208's pod runs in %s, want a microVM", got)
	}
}

// microVMProjectFile is the smallest legion.yaml of a Kubernetes deployment, as config's own tests
// write it (kubernetesFile), on its own storage class, whose sandbox block runs every issue pod in
// a microVM and whose microvm block names a ComputeClass and no storage_class.
const microVMProjectFile = `project: LEGION
state_dir: /var/lib/legion
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
bind: 10.0.0.5
daemon_url: http://10.0.0.5:13370
envoy_url: http://envoy-listener.internal.example:9020
nats_urls: [nats://nats.internal.example:4222]
envoy_token_file: /var/run/legion/ENVOY_TOKEN
operator_token_file: /var/run/legion/OPERATOR_TOKEN
dispatch_url: https://dispatch.internal.example
dispatch_token_file: /var/run/legion/DISPATCH_TOKEN
projects:
  LEGION: {}
github_apps:
  implement: { app_id: "1", private_key_command: "exit 1" }
  review: { app_id: "2", private_key_command: "exit 1" }
runtime:
  kubernetes:
    namespace: legion
    image: ` + testImage + `
    storage_class: example-standard-rwo
    session_store: postgres
    session_dsn_secret: SESSION_DSN
    sandbox:
      type: microvm
      microvm:
        compute_class: ` + testComputeClass + `
`

// A microVM pod's disk is on the microvm block's StorageClass, while a gVisor issue pod's and the
// controller's stay on runtime.kubernetes.storage_class. A block that names no class takes the
// daemon's: the loader settles a microvm block without storage_class on
// runtime.kubernetes.storage_class (config's readMicroVM), and the daemon hands the runtime that
// class as the block's (internal/daemon/kubernetes.go, sandboxConfig).
func TestAMicroVMPodTakesTheMicroVMStorageClassOrTheDaemons(t *testing.T) {
	t.Run("the block's own class", func(t *testing.T) {
		r := sandboxRuntime(t, SandboxConfig{CanaryIssues: []string{"LEGION-209"}, MicroVM: testMicroVM()})
		for what, tc := range map[string]struct {
			spec runtime.SpawnSpec
			want string
		}{
			"the canary's microVM pod": {childSpec(t), testMicroVMStorageClass},
			"a gVisor issue pod":       {rootSpec(t), "gp2"},
		} {
			if got := podDisk(t, podOf(t, r, tc.spec)).Spec.StorageClassName; got == nil || *got != tc.want {
				t.Errorf("%s's disk is on the class %v, want %s", what, got, tc.want)
			}
		}
		templates := r.sandboxManifest(launchOf(t, r, controllerSpec(t))).Spec.VolumeClaimTemplates
		if len(templates) != 1 || templates[0].Spec.StorageClassName == nil || *templates[0].Spec.StorageClassName != "gp2" {
			t.Errorf("the controller's Sandbox claims %+v, want its one disk on gp2", templates)
		}
	})
	t.Run("the daemon's class, which the loader fills in", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legion.yaml")
		if err := os.WriteFile(path, []byte(microVMProjectFile), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadForValidation(path, func(string) string { return "" })
		if err != nil {
			t.Fatalf("LoadForValidation: %v", err)
		}
		k := cfg.Runtime.Kubernetes
		r := sandboxRuntime(t, sandboxOf(k.Sandbox), func(o *Options) { o.StorageClass = k.StorageClass })
		if got := podDisk(t, podOf(t, r, rootSpec(t))).Spec.StorageClassName; got == nil || *got != "example-standard-rwo" {
			t.Errorf("the microVM pod's disk is on the class %v, want the daemon's example-standard-rwo", got)
		}
		if got := renderedSandbox(t, r.podTemplate(launchOf(t, r, rootSpec(t)))); got != SandboxMicroVM {
			t.Errorf("the pod runs in %s, want a microVM", got)
		}
	})
}

// runtime.kubernetes.sandbox.microvm.keep_volume is a row of the key table whose default is
// `false`. A microvm block without the key settles false, and `keep_volume: true` settles true. The
// loader refuses a value that is not a boolean with a line that names the key (config's
// readMicroVM).
func TestTheMicroVMKeepVolumeKey(t *testing.T) {
	const key = "runtime.kubernetes.sandbox.microvm.keep_volume"
	var row *config.ConfigNestedKey
	for _, entry := range config.ConfigReference() {
		for i, nested := range entry.Nested {
			if nested.Path+"."+nested.Name == key {
				row = &entry.Nested[i]
			}
		}
	}
	switch {
	case row == nil:
		t.Errorf("the config reference has no row for %s", key)
	case row.Default != "`false`":
		t.Errorf("the config reference gives %s the default %q, want `false`", key, row.Default)
	}
	load := func(t *testing.T, lines string) (config.Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "legion.yaml")
		if err := os.WriteFile(path, []byte(microVMProjectFile+lines), 0o600); err != nil {
			t.Fatal(err)
		}
		return config.LoadForValidation(path, func(string) string { return "" })
	}
	for _, tc := range []struct {
		name, lines string
		want        bool
	}{
		{name: "no key", want: false},
		{name: "keep_volume: true", lines: "        keep_volume: true\n", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := load(t, tc.lines)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if got := cfg.Runtime.Kubernetes.Sandbox.MicroVM.KeepVolume; got != tc.want {
				t.Errorf("%s settled %v, want %v", key, got, tc.want)
			}
		})
	}
	t.Run("keep_volume: maybe", func(t *testing.T) {
		_, err := load(t, "        keep_volume: maybe\n")
		if want := key + " must be true or false"; err == nil || err.Error() != want {
			t.Errorf("LoadForValidation error = %v, want %q", err, want)
		}
	})
}

// keep_volume keeps the volume of an issue pod that runs in a microVM, by either route to one, and
// of no other pod. The pod's Sandbox owns one claim template, named `issue`, labelled with the
// project, the issue and legion.dev/volume=kept and with no tree. It is on the microvm block's
// StorageClass at issue_volume's size, ReadWriteOnce, and the pod mounts at /legion the claim Agent
// Sandbox makes from it (IssueClaimName). A gVisor issue pod keeps the ephemeral volume of its own,
// and a gVisor project with no canary renders the root's Sandbox of a project with no sandbox
// block, byte for byte. The controller's Sandbox is the one of today for its project's kind,
// whatever keep_volume says: the controller golden in a gvisor project, and the controller-microvm
// golden in a microvm project, whose controller's pod runs in a microVM (#4485, INFRA-4567). The
// stored side reads the kept Sandbox as kept, decoded and as the API server holds it, and every
// other Sandbox, the controller's `issue` template included, as not kept. With the setting off, a
// microVM project's root is the root-microvm golden byte for byte.
func TestOnlyAMicroVMPodKeepsItsVolume(t *testing.T) {
	opts := goldenOptions()
	specs := map[string]runtime.SpawnSpec{"LEGION-208": rootSpec(t), "LEGION-209": childSpec(t)}
	for name, tc := range map[string]struct {
		block SandboxConfig
		// kept are the issues whose pods keep their volume.
		kept []string
	}{
		"a gvisor project with no canary": {SandboxConfig{Type: SandboxGVisor, MicroVM: keptMicroVM()}, nil},
		"a gvisor project whose canary is LEGION-209": {
			SandboxConfig{Type: SandboxGVisor, CanaryIssues: []string{"LEGION-209"}, MicroVM: keptMicroVM()}, []string{"LEGION-209"},
		},
		"a microvm project whose canary is LEGION-209": {
			SandboxConfig{Type: SandboxMicroVM, CanaryIssues: []string{"LEGION-209"}, MicroVM: keptMicroVM()}, []string{"LEGION-208"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := sandboxRuntime(t, tc.block)
			for issue, spec := range specs {
				l := launchOf(t, r, spec)
				manifest := r.sandboxManifest(l)
				templates := manifest.Spec.VolumeClaimTemplates
				pod := r.podTemplate(l).Spec
				kept := slices.Contains(tc.kept, issue)
				if decoded, stored := keptReadings(t, manifest); decoded != kept || stored != kept {
					t.Errorf("%s's Sandbox reads as kept %v decoded and %v stored, want %v", issue, decoded, stored, kept)
				}
				if !kept {
					if len(templates) != 0 {
						t.Errorf("%s's Sandbox owns %+v, want no claim template: its pod owns its disk", issue, templates)
					}
					podDisk(t, pod)
					continue
				}
				if len(templates) != 1 || templates[0].Metadata.Name != issueVolume {
					t.Fatalf("%s's Sandbox owns %+v, want the one %s template of its kept volume", issue, templates, issueVolume)
				}
				labels := map[string]string{labelProject: testProject, labelIssue: issue, labelVolume: volumeKept}
				if got := templates[0].Metadata.Labels; !maps.Equal(got, labels) {
					t.Errorf("%s's claim template carries %v, want exactly %v and no tree", issue, got, labels)
				}
				storageClass := testMicroVMStorageClass
				want := corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: &storageClass,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: opts.IssueVolume},
					},
				}
				if got := templates[0].Spec; !reflect.DeepEqual(got, want) {
					t.Errorf("%s's kept claim is %+v, want %+v", issue, got, want)
				}
				if mount := mountHolding(containerNamed(t, pod, l.kind.container()), TreeRoot); mount == nil || mount.Name != issueVolume || mount.MountPath != TreeRoot {
					t.Errorf("%s's pod mounts %+v at %s, want its %s volume", issue, mount, TreeRoot, issueVolume)
				}
				disk := podDiskSource(t, pod)
				if disk.PersistentVolumeClaim == nil || disk.PersistentVolumeClaim.ClaimName != IssueClaimName(spec.Claim) || disk.Ephemeral != nil {
					t.Errorf("%s's pod's %s volume is %+v, want the claim %s alone", issue, issueVolume, disk, IssueClaimName(spec.Claim))
				}
			}
			if len(tc.block.CanaryIssues) == 0 {
				checkGolden(t, "root", manifestOf(t, r, rootSpec(t)))
			}
			controllerGolden := "controller"
			if tc.block.Controller() == SandboxMicroVM {
				controllerGolden = "controller-microvm"
			}
			checkGolden(t, controllerGolden, manifestOf(t, r, controllerSpec(t)))
			if decoded, stored := keptReadings(t, r.sandboxManifest(launchOf(t, r, controllerSpec(t)))); decoded || stored {
				t.Errorf("the controller's Sandbox reads as kept %v decoded and %v stored, want neither", decoded, stored)
			}
		})
	}
	t.Run("keep_volume false", func(t *testing.T) {
		r := sandboxRuntime(t, SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()})
		checkGolden(t, "root-microvm", manifestOf(t, r, rootSpec(t)))
	})
}

// keptReadings are how the stored side reads s: decoded (ownsKeptVolume), and as the API server
// holds it, without a decode (storedOwnsKeptVolume).
func keptReadings(t *testing.T, s sandbox) (decoded, stored bool) {
	t.Helper()
	u, err := encodeSandbox(s)
	if err != nil {
		t.Fatal(err)
	}
	return ownsKeptVolume(&s), storedOwnsKeptVolume(u)
}

// A microVM pod carries the two stop-eviction annotations a gVisor pod does, its kind's pod
// shape in a microVM, and the checker it was built under
// (TestEveryIssueAndControllerPodIsProtectedFromAutoscalerEviction): the sandbox it runs in changes
// nothing of what asks the autoscaler to leave its node alone.
func TestAMicroVMPodKeepsTheStopEvictionAnnotations(t *testing.T) {
	r := sandboxRuntime(t, SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()})
	template := r.podTemplate(launchOf(t, r, rootSpec(t)))
	if got := renderedSandbox(t, template); got != SandboxMicroVM {
		t.Fatalf("the pod runs in %s, want a microVM", got)
	}
	want := map[string]string{doNotDisruptAnnotation: "true", safeToEvictAnnotation: "false", podShapeAnnotation: podShape(r, issuePod{}, testMicroVM()),
		leakCheckerAnnotation: testLeakCheck.ManifestSHA256}
	if got := template.Metadata.Annotations; !maps.Equal(got, want) {
		t.Errorf("the microVM pod's annotations are %v, want exactly %v", got, want)
	}
}

// A microVM pod tolerates the taint GKE puts on its ComputeClass's nodes,
// cloud.google.com/compute-class=<class>:NoSchedule, after the pool's and the operator's
// tolerations. The class it names replaces any the operator's scheduling names, the class the
// project's gVisor pods run on: the operator's compute class selector takes the microVM class's
// value and the operator's toleration of a compute class is dropped, every other entry of both kept
// as the operator wrote it. A gVisor pod of the same project keeps the operator's class, and the
// operator's scheduling itself is left as it was.
func TestAMicroVMPodToleratesItsComputeClass(t *testing.T) {
	scaleOut := computeClassToleration("Scale-Out")
	dedicated := corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "legion", Effect: corev1.TaintEffectNoSchedule}
	r := sandboxRuntime(t, SandboxConfig{CanaryIssues: []string{"LEGION-209"}, MicroVM: testMicroVM()}, func(o *Options) {
		o.Scheduling.NodeSelector = map[string]string{computeClassKey: "Scale-Out", "kubernetes.io/arch": "amd64"}
		o.Scheduling.Tolerations = []corev1.Toleration{scaleOut, dedicated}
	})
	for what, tc := range map[string]struct {
		spec        runtime.SpawnSpec
		class       string
		tolerations []corev1.Toleration
	}{
		"the canary's microVM pod": {childSpec(t), testComputeClass, []corev1.Toleration{poolToleration, dedicated, computeClassToleration(testComputeClass)}},
		"a gVisor issue pod":       {rootSpec(t), "Scale-Out", []corev1.Toleration{poolToleration, scaleOut, dedicated}},
	} {
		pod := podOf(t, r, tc.spec)
		selector := map[string]string{poolKey: poolValue, computeClassKey: tc.class, "kubernetes.io/arch": "amd64"}
		if !maps.Equal(pod.NodeSelector, selector) {
			t.Errorf("%s selects %v, want exactly %v", what, pod.NodeSelector, selector)
		}
		if !reflect.DeepEqual(pod.Tolerations, tc.tolerations) {
			t.Errorf("%s tolerates %+v, want exactly %+v", what, pod.Tolerations, tc.tolerations)
		}
	}
	if r.scheduling.NodeSelector[computeClassKey] != "Scale-Out" || !reflect.DeepEqual(r.scheduling.Tolerations, []corev1.Toleration{scaleOut, dedicated}) {
		t.Errorf("the operator's scheduling now reads %+v, want it as the operator wrote it", r.scheduling)
	}
}

// The label legion.dev/sandbox=microvm marks a microVM pod and nothing else. A microVM launch
// through the cluster puts it on the Sandbox's pod template, where the controller copies a pod's
// labels from, and so on the pod, and on neither the Sandbox nor any of its Secrets — the
// provisioning Secret and each role's — which carry exactly the labels of the issue pod's kind, as
// a gVisor pod's do. A gVisor issue pod carries no such label, and nor does the controller's pod of
// this gVisor project: its canary list names an issue, which never moves the controller's pod into
// a microVM (SandboxConfig.Controller; TestTheControllerPodRunsInTheProjectsSandbox).
func TestOnlyAMicroVMPodCarriesTheSandboxLabel(t *testing.T) {
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.Sandbox = SandboxConfig{CanaryIssues: []string{testTree}, MicroVM: testMicroVM()}
	}))
	loc := g.spawn(rootSpec(t))
	name := loc.Sandbox.Name
	s := g.sandbox(name)
	if s == nil {
		t.Fatalf("the launch left no Sandbox %s", name)
	}
	if got := s.Spec.PodTemplate.Metadata.Labels[labelSandbox]; got != "microvm" {
		t.Errorf("the Sandbox's pod template is labelled %s=%q, want microvm", labelSandbox, got)
	}
	if pod := g.pod(name); pod == nil || pod.Labels[labelSandbox] != "microvm" {
		t.Errorf("the pod %s is not labelled %s=microvm: %+v", name, labelSandbox, pod)
	}
	kind := (issuePod{}).labels(testProject, launchOf(t, g.r, rootSpec(t)))
	if !maps.Equal(s.Labels, kind) {
		t.Errorf("the Sandbox is labelled %v, want exactly its kind's %v", s.Labels, kind)
	}
	secrets := []string{secretName(name)}
	for _, role := range claim.Roles {
		secrets = append(secrets, roleSecretName(name, role))
	}
	for _, secret := range secrets {
		held := g.secret(secret)
		if held == nil {
			t.Errorf("the launch wrote no Secret %s", secret)
			continue
		}
		if !maps.Equal(held.Labels, kind) {
			t.Errorf("the Secret %s is labelled %v, want exactly its pod kind's %v", secret, held.Labels, kind)
		}
	}
	for what, spec := range map[string]runtime.SpawnSpec{"a gVisor issue pod": childSpec(t), "the controller's pod": controllerSpec(t)} {
		if value, ok := g.r.podTemplate(launchOf(t, g.r, spec)).Metadata.Labels[labelSandbox]; ok {
			t.Errorf("%s is labelled %s=%q", what, labelSandbox, value)
		}
	}
}

// The project controller's pod runs in the project's sandbox (SandboxConfig.Controller): in a
// microVM where the project's type is microvm, and in gVisor otherwise, whatever the microvm block
// or the canary list says. A repository cache pod and the image probe's pod run in gVisor under
// every configuration. Under microvm the controller's pod is placed, labelled and told as a
// microVM issue pod is: the microvm RuntimeClass; the operator's selector with the microVM class
// in place of the one it names (Scale-Out here), and its tolerations with the class's in place of
// Scale-Out's; legion.dev/sandbox=microvm on its template and on neither its Sandbox nor its
// Secrets; --microvm and the metadata endpoint's two flags; and the endpoint's token mounted
// read-only at the single-role launcher's fixed path. Its recorded shape names the microVM's
// runtime class, compute class and metadata endpoint, and no storage class: its disk stays on
// runtime.kubernetes.storage_class under every configuration, since its Sandbox's claim template
// is immutable (diskSpec). Under each gVisor configuration the controller's Sandbox is the one a
// project with no sandbox block renders, byte for byte. Each pod logs one line of its placement
// and of its launcher's sandbox flags, and the controller's pod one of its disk.
func TestTheControllerPodRunsInTheProjectsSandbox(t *testing.T) {
	scaleOut := computeClassToleration("Scale-Out")
	dedicated := corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "legion", Effect: corev1.TaintEffectNoSchedule}
	scheduling := func(o *Options) {
		o.Scheduling.NodeSelector = map[string]string{computeClassKey: "Scale-Out", "kubernetes.io/arch": "amd64"}
		o.Scheduling.Tolerations = []corev1.Toleration{scaleOut, dedicated}
	}
	storageClass := goldenOptions().StorageClass
	block := testMicroVM()
	if block.StorageClass == storageClass || block.Metadata == nil {
		t.Fatalf("the microvm block %+v names the storage class %s or no metadata endpoint, so this test proves nothing", block, storageClass)
	}
	issue := rootSpec(t)
	// encoded is the controller's Sandbox as a launch leaves it running, its pod template among it,
	// as the API server is sent it.
	encoded := func(r *Runtime) []byte {
		t.Helper()
		sent, err := json.Marshal(manifestOf(t, r, controllerSpec(t)))
		if err != nil {
			t.Fatal(err)
		}
		return sent
	}
	plainSandbox := encoded(sandboxRuntime(t, SandboxConfig{}, scheduling))
	for _, tc := range []struct {
		config  string
		sandbox SandboxConfig
		// controller is the sandbox the controller's pod must run in.
		controller SandboxType
	}{
		{"microvm", SandboxConfig{Type: SandboxMicroVM, MicroVM: block}, SandboxMicroVM},
		{"gvisor-canary", SandboxConfig{Type: SandboxGVisor, CanaryIssues: []string{issue.Issue}, MicroVM: block}, SandboxGVisor},
		{"gvisor-block", SandboxConfig{Type: SandboxGVisor, MicroVM: block}, SandboxGVisor},
		{"no-block", SandboxConfig{}, SandboxGVisor},
	} {
		t.Run(tc.config, func(t *testing.T) {
			r := sandboxRuntime(t, tc.sandbox, scheduling)
			l := launchOf(t, r, controllerSpec(t))
			s := r.sandboxManifest(l)
			controller := r.podTemplate(l)
			pods := []struct {
				kind     string
				template podTemplate
			}{
				{"controller", controller},
				{"issue", r.podTemplate(launchOf(t, r, issue))},
				{"cache", r.cacheManifest(cacheRepo).Spec.PodTemplate},
				{"probe", r.probeManifest(probeSandboxName, ImageProbe{Contract: 3, RoleReferences: testRoleReferences}, time.Now()).Spec.PodTemplate},
			}
			for _, pod := range pods {
				spec := pod.template.Spec
				var tolerations []string
				for _, toleration := range spec.Tolerations {
					tolerations = append(tolerations, toleration.Key+"="+toleration.Value+":"+string(toleration.Effect))
				}
				t.Logf("sandbox-pod config=%s pod=%s runtimeClassName=%s nodeSelector=%s tolerations=%s labels=%s launcher=%s",
					tc.config, pod.kind, runtimeClassText(spec.RuntimeClassName), sortedPairs(spec.NodeSelector), strings.Join(tolerations, ","),
					sortedPairs(pod.template.Metadata.Labels), sandboxFlags(spec.Containers[0].Command))
			}
			templates := s.Spec.VolumeClaimTemplates
			if len(templates) != 1 || templates[0].Spec.StorageClassName == nil {
				t.Fatalf("the controller's Sandbox owns the claim templates %+v, want one with a storage class", templates)
			}
			claimClass := *templates[0].Spec.StorageClassName
			var volumes []string
			for _, volume := range controller.Spec.Volumes {
				volumes = append(volumes, volume.Name)
			}
			t.Logf("controller-disk config=%s volumes=%s claimStorageClass=%s", tc.config, strings.Join(volumes, ","), claimClass)
			if claimClass != storageClass {
				t.Errorf("the controller's Sandbox claims its disk on %s, want runtime.kubernetes.storage_class, %s", claimClass, storageClass)
			}
			if carried, want := slices.Contains(volumes, workloadIdentityTokenVolume), tc.controller == SandboxMicroVM; carried != want {
				t.Errorf("the controller's pod carries the volume %s: %t, want %t", workloadIdentityTokenVolume, carried, want)
			}
			if tc.controller == SandboxMicroVM {
				pod := controller.Spec
				if pod.RuntimeClassName == nil || *pod.RuntimeClassName != microVMRuntimeClass {
					t.Errorf("the controller's pod runs under the RuntimeClass %s, want %s", runtimeClassText(pod.RuntimeClassName), microVMRuntimeClass)
				}
				selector := map[string]string{poolKey: poolValue, "kubernetes.io/arch": "amd64", computeClassKey: testComputeClass}
				if !maps.Equal(pod.NodeSelector, selector) {
					t.Errorf("the controller's pod selects %v, want exactly %v", pod.NodeSelector, selector)
				}
				if want := []corev1.Toleration{poolToleration, dedicated, computeClassToleration(testComputeClass)}; !reflect.DeepEqual(pod.Tolerations, want) {
					t.Errorf("the controller's pod tolerates %+v, want exactly %+v, Scale-Out's not among them", pod.Tolerations, want)
				}
				if got := controller.Metadata.Labels[labelSandbox]; got != string(SandboxMicroVM) {
					t.Errorf("the controller's pod template is labelled %s=%q, want microvm", labelSandbox, got)
				}
				if value, ok := s.Labels[labelSandbox]; ok {
					t.Errorf("the controller's Sandbox is labelled %s=%q, want no such label", labelSandbox, value)
				}
				for _, secret := range []string{secretName(l.name), roleSecretName(l.name, claim.RoleController)} {
					if value, ok := r.secretMeta(&s, l, secret).Labels[labelSandbox]; ok {
						t.Errorf("the controller's Secret %s is labelled %s=%q, want no such label", secret, labelSandbox, value)
					}
				}
				c := containerNamed(t, pod, runtime.ControllerPodContainer)
				if !slices.Contains(c.Command, "--microvm") {
					t.Errorf("the controller's launcher runs %q, want --microvm", c.Command)
				}
				for flag, value := range map[string]string{"--metadata-workload-pool": testWorkloadPool, "--metadata-identity-provider": testIdentityProvider} {
					if got, ok := flagValue(c.Command, flag); !ok || got != value {
						t.Errorf("the controller's launcher runs %q, want %s %s", c.Command, flag, value)
					}
				}
				const tokenDir = "/var/run/legion/workload-identity"
				tokenFile := tokenDir + "/" + launcher.WorkloadIdentityTokenFile
				if mount := mountHolding(c, tokenFile); mount == nil || mount.Name != workloadIdentityTokenVolume || !mount.ReadOnly || mount.MountPath != tokenDir {
					t.Errorf("the controller's token file %s is held by %+v, want the read-only mount of %s at %s", tokenFile, mount, workloadIdentityTokenVolume, tokenDir)
				}
				shape := controller.Metadata.Annotations[podShapeAnnotation]
				if shape == "" {
					t.Fatal("the controller's pod template records no shape, so no launch could tell it moved")
				}
				var record struct {
					Sandbox map[string]any `json:"sandbox"`
				}
				if err := json.Unmarshal([]byte(shape), &record); err != nil {
					t.Fatalf("the controller's recorded shape %s: %v", shape, err)
				}
				if class, ok := record.Sandbox["storage_class"]; ok {
					t.Errorf("the controller's recorded shape names the storage class %v, which its disk is not on", class)
				}
				want := map[string]any{
					"runtime_class": microVMRuntimeClass, "compute_class": testComputeClass,
					"metadata": map[string]any{"workload_pool": testWorkloadPool, "identity_provider": testIdentityProvider},
				}
				if !reflect.DeepEqual(record.Sandbox, want) {
					t.Errorf("the controller's recorded sandbox is %v, want exactly %v", record.Sandbox, want)
				}
			} else if sent := encoded(r); !bytes.Equal(sent, plainSandbox) {
				t.Errorf("the controller's Sandbox is\n%s\nwant the one a project with no sandbox block renders, byte for byte\n%s", sent, plainSandbox)
			}
			for _, pod := range pods {
				if pod.kind != "cache" && pod.kind != "probe" {
					continue
				}
				spec := pod.template.Spec
				if spec.RuntimeClassName == nil || *spec.RuntimeClassName != gvisor {
					t.Errorf("the %s pod runs under the RuntimeClass %s, want %s", pod.kind, runtimeClassText(spec.RuntimeClassName), gvisor)
				}
				if want := map[string]string{poolKey: poolValue, computeClassKey: "Scale-Out", "kubernetes.io/arch": "amd64"}; !maps.Equal(spec.NodeSelector, want) {
					t.Errorf("the %s pod selects %v, want exactly the pool's and the operator's %v", pod.kind, spec.NodeSelector, want)
				}
				if want := []corev1.Toleration{poolToleration, scaleOut, dedicated}; !reflect.DeepEqual(spec.Tolerations, want) {
					t.Errorf("the %s pod tolerates %+v, want exactly the pool's and the operator's %+v", pod.kind, spec.Tolerations, want)
				}
				if value, ok := pod.template.Metadata.Labels[labelSandbox]; ok {
					t.Errorf("the %s pod is labelled %s=%q", pod.kind, labelSandbox, value)
				}
				for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
					if slices.Contains(c.Command, "--microvm") {
						t.Errorf("the %s pod's %s runs %q, with --microvm", pod.kind, c.Name, c.Command)
					}
				}
				checkNoMetadataEndpoint(t, spec)
			}
		})
	}
}

// runtimeClassText is a pod's runtimeClassName as a failure message names it: the class, or
// `none` where the pod names none, rather than the pointer's address.
func runtimeClassText(name *string) string {
	if name == nil {
		return "none"
	}
	return *name
}

// sortedPairs is m as `key=value` entries in key order, comma-joined.
func sortedPairs(m map[string]string) string {
	pairs := make([]string, 0, len(m))
	for _, key := range sortedKeys(m) {
		pairs = append(pairs, key+"="+m[key])
	}
	return strings.Join(pairs, ",")
}

// sandboxFlags are the flags of command that tell a launcher its sandbox, comma-joined: --microvm,
// and each metadata flag with its value; "none" where it names none.
func sandboxFlags(command []string) string {
	var flags []string
	if slices.Contains(command, "--microvm") {
		flags = append(flags, "--microvm")
	}
	for _, flag := range metadataFlags {
		if value, ok := flagValue(command, flag); ok {
			flags = append(flags, flag+"="+value)
		}
	}
	if len(flags) == 0 {
		return "none"
	}
	return strings.Join(flags, ",")
}

// A microVM pod whose block names the metadata endpoint carries one projected ServiceAccount token
// for it, workload-identity-token: for the Workload Identity pool's audience, an hour long, at
// `token`, readable by the pod's group (0440: the pod runs with an fsGroup). Its roles container
// alone mounts it, read-only, at RolesDir/workload-identity, which makes the token the file the
// launcher reads by default under its --roles-dir; that is under RolesDir, which every role's
// namespace covers, and in no role's own directory there, which role-init binds back, nor in the
// role homes. The launcher is told --microvm and the endpoint's pool and identity provider.
func TestAMicroVMPodWithMetadataMountsAProjectedTokenNoRoleSees(t *testing.T) {
	r := sandboxRuntime(t, SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()})
	pod := podOf(t, r, rootSpec(t))
	var tokens []corev1.Volume
	for _, volume := range pod.Volumes {
		if volume.Name == workloadIdentityTokenVolume {
			tokens = append(tokens, volume)
		}
	}
	expiry := int64(3600)
	want := []corev1.Volume{{Name: "workload-identity-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		DefaultMode: new(int32(0o440)),
		Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
			Audience: testWorkloadPool, ExpirationSeconds: &expiry, Path: "token",
		}}},
	}}}}
	if !reflect.DeepEqual(tokens, want) {
		t.Errorf("the pod's token volumes are %+v, want exactly %+v", tokens, want)
	}
	roles := containerNamed(t, pod, workerContainer)
	command := roles.Command
	rolesDir, _ := flagValue(command, "--roles-dir")
	tokenFile := filepath.Join(rolesDir, launcher.WorkloadIdentityDirName, launcher.WorkloadIdentityTokenFile)
	mount := mountHolding(roles, tokenFile)
	if mount == nil || mount.Name != workloadIdentityTokenVolume || !mount.ReadOnly || mount.MountPath != RolesDir+"/workload-identity" {
		t.Fatalf("the launcher's token file %s is held by %+v, want the read-only mount of %s at %s/workload-identity",
			tokenFile, mount, workloadIdentityTokenVolume, RolesDir)
	}
	if rest := strings.TrimPrefix(tokenFile, mount.MountPath+"/"); rest != want[0].Projected.Sources[0].ServiceAccountToken.Path {
		t.Errorf("the launcher reads %s in the mount, and the volume projects the token at %s", rest, want[0].Projected.Sources[0].ServiceAccountToken.Path)
	}
	if !strings.HasPrefix(mount.MountPath, RolesDir+"/") || strings.HasPrefix(mount.MountPath, RoleHomesDir+"/") {
		t.Errorf("the token is mounted at %s, want it under %s and outside %s", mount.MountPath, RolesDir, RoleHomesDir)
	}
	for _, role := range claim.Roles {
		if dir := roleDir(role); mount.MountPath == dir || strings.HasPrefix(mount.MountPath, dir+"/") {
			t.Errorf("the token is mounted at %s, in %s's own directory %s, which role-init binds back", mount.MountPath, role, dir)
		}
	}
	for _, c := range pod.InitContainers {
		if slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == workloadIdentityTokenVolume }) {
			t.Errorf("the init container %s mounts the token", c.Name)
		}
	}
	if !slices.Contains(command, "--microvm") {
		t.Errorf("the launcher runs %q, want --microvm", command)
	}
	for flag, value := range map[string]string{"--metadata-workload-pool": testWorkloadPool, "--metadata-identity-provider": testIdentityProvider} {
		if got, ok := flagValue(command, flag); !ok || got != value {
			t.Errorf("the launcher runs %q, want %s %s", command, flag, value)
		}
	}
}

// A microVM pod whose block names no metadata endpoint runs its launcher with --microvm and nothing
// of the endpoint: no token volume, no mount at WorkloadIdentityDir, no metadata flag. A gVisor pod
// carries none of them and no --microvm either, beside a canary whose block names the endpoint as
// in a project that names no sandbox.
func TestAMicroVMPodWithoutMetadataMountsNoToken(t *testing.T) {
	block := testMicroVM()
	block.Metadata = nil
	r := sandboxRuntime(t, SandboxConfig{Type: SandboxMicroVM, MicroVM: block})
	pod := podOf(t, r, rootSpec(t))
	if command := containerNamed(t, pod, workerContainer).Command; !slices.Contains(command, "--microvm") {
		t.Errorf("the microVM pod's launcher runs %q, want --microvm", command)
	}
	checkNoMetadataEndpoint(t, pod)
	for name, block := range map[string]SandboxConfig{
		"beside a canary whose block names the endpoint": {CanaryIssues: []string{"LEGION-209"}, MicroVM: testMicroVM()},
		"in a project that names no sandbox":             {},
	} {
		t.Run(name, func(t *testing.T) {
			r := sandboxRuntime(t, block)
			pod := podOf(t, r, rootSpec(t))
			if command := containerNamed(t, pod, workerContainer).Command; slices.Contains(command, "--microvm") {
				t.Errorf("the gVisor pod's launcher runs %q, with --microvm", command)
			}
			checkNoMetadataEndpoint(t, pod)
		})
	}
}

// kubeExpand is the kubelet's expansion of a container's command, args, and env values
// (k8s.io/kubernetes third_party/forked/golang/expansion): `$$` is a literal `$`, and `$(NAME)` is
// the value of a variable the container defines, left as written when it defines none.
func kubeExpand(input string, env map[string]string) string {
	var out strings.Builder
	for i := 0; i < len(input); i++ {
		if input[i] != '$' || i+1 == len(input) {
			out.WriteByte(input[i])
			continue
		}
		switch next := input[i+1]; {
		case next == '$':
			out.WriteByte('$')
			i++
		case next == '(' && strings.Contains(input[i+2:], ")"):
			name, _, _ := strings.Cut(input[i+2:], ")")
			if value, ok := env[name]; ok {
				out.WriteString(value)
			} else {
				out.WriteString("$(" + name + ")")
			}
			i += len(name) + 2
		default:
			out.WriteByte('$')
		}
	}
	return out.String()
}

// Whatever text a launch carries reaches the process as written: the worker's argv, plain
// environment and system prompt file travel in the launcher's start command, which no kubelet
// expands, so `$(NAME)` and `$$` in the system prompt, an operator's instructions, and a spec's
// env values mean what they say in a pod as in a pane. The `roles` container's own command and
// env, which the kubelet does expand, still survive it.
func TestTextSurvivesTheKubeletsExpansion(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	literal := "Run `echo $(HOME)` and `kill $$`; your issue is not $(LEGION_ISSUE), and $LEGION_WORKSPACE is yours. $"
	spec := workerSpec(t)
	if err := os.WriteFile(spec.Prompt.DeploymentInstructionsPath, []byte(literal+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.Env["LEGION_E2E_NOTE"] = literal
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	start := launcherCommand(l, r)
	argv := l.agentArgv(r.agent)
	if got := start.Argv[len(start.Argv)-len(argv):]; !slices.Equal(got, argv) {
		t.Errorf("the agent's argv reaches the launcher as %q, want %q", got, argv)
	}
	env := map[string]string{}
	for _, entry := range start.Env {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	for name, want := range map[string]string{
		"LEGION_E2E_NOTE": literal,
		"PI_SHELL_PREFIX": shellprefix.For(filepath.Dir(r.tools.Legion)),
	} {
		if got := env[name]; got != want {
			t.Errorf("%s reaches the agent as %q, want %q", name, got, want)
		}
	}
	role := containerNamed(t, r.podTemplate(l).Spec, l.kind.container())
	for _, arg := range role.Command {
		if expanded := kubeExpand(arg, envOf(role)); strings.ReplaceAll(arg, "$$", "$") != expanded {
			t.Errorf("the launcher's argument %q reaches it as %q", arg, expanded)
		}
	}
	if _, prompt := systemPromptOf(t, spec.Role, start); !strings.Contains(prompt, literal) {
		t.Fatalf("the system prompt file does not carry the instructions as written: %q", prompt)
	}
}

// uuid is the form a Claude Code session id takes (claudelaunch.NewSessionID), the only word of a
// Claude launch's agent command that is not fixed.
var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// A Claude Code role's pod runs Claude Code under `legion claude-bridge`, word for word: the
// Claude Code this generation's start script installed under the slot's state home on the issue's
// volume (the image carries none, only the pin it is installed from), the image's claude-envoy
// root, every permission granted, only the MCP document this generation's launcher writes into its
// private directory, the claude-envoy plugin and the generated Legion plugin, a fresh session, and
// `--append-system-prompt-file` naming the system prompt file the same launcher writes beside the
// MCP document, whose text is the Oh My Pi launch's, since a pod cannot read the daemon's prompt
// files whichever harness it runs. A launch with no recorded session starts a session of its own,
// which is what the fresh uuid here says; one that resumes names the transcript's id instead
// (TestAResumingClaudePodContinuesItsTranscriptByItsID). The binary's path is the generation's
// own, so a later generation names its own install, never the one its start script has just
// removed.
func TestAClaudePodsAgentCommandIsClaudeCodeUnderTheBridge(t *testing.T) {
	opts := goldenOptions()
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	omp := launchOf(t, r, spec)
	spec.Backend = runtime.BackendClaude
	l := launchOf(t, r, spec)
	argv := l.agentArgv(r.agent)
	_, ompPrompt := systemPromptOf(t, spec.Role, launcherCommand(omp, r))
	if _, claudePrompt := systemPromptOf(t, spec.Role, launcherCommand(l, r)); claudePrompt != ompPrompt {
		t.Fatalf("the Claude launch's system prompt is %q, want the Oh My Pi launch's %q", claudePrompt, ompPrompt)
	}
	session := ""
	if at := slices.Index(argv, "--session-id"); at < 0 || at+1 == len(argv) {
		t.Fatalf("the agent command %q names no --session-id", argv)
	} else if session = argv[at+1]; !uuid.MatchString(session) {
		t.Errorf("the session is %q, want a version 4 uuid", session)
	}
	claude := func(generation uint64) string {
		return "/legion/scratch/state/" + string(spec.Role) + "/legion-claude-code/g" + strconv.FormatUint(generation, 10) +
			"/node_modules/@anthropic-ai/claude-code-linux-x64/claude"
	}
	want := []string{
		"/opt/legion/bin/legion", "claude-bridge", "--", claude(spec.Generation),
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--replay-user-messages",
		"--thinking-display", "summarized",
		"--permission-mode", "bypassPermissions",
		"--strict-mcp-config", "--mcp-config", generationDir(spec.Generation) + "/claude-mcp.json",
		"--plugin-dir", "/opt/legion/claude-envoy",
		"--plugin-dir", "/opt/legion/claude-envoy/legion",
		"--session-id", session,
		"--append-system-prompt-file", generationDir(spec.Generation) + "/system-prompt.md",
	}
	if !slices.Equal(argv, want) {
		t.Errorf("the Claude pod's agent command is %q, want %q", argv, want)
	}
	later := spec
	later.Generation, later.BootToken = spec.Generation+1, "boot-later"
	if got := launchOf(t, r, later).agentArgv(r.agent); !slices.Contains(got, claude(later.Generation)) || slices.Contains(got, claude(spec.Generation)) {
		t.Errorf("generation %d's agent command is %q, want it to name %s and not generation %d's install", later.Generation, got, claude(later.Generation), spec.Generation)
	}
	// Options.Agent is the Oh My Pi override and nothing else: a Claude role's command is the
	// runtime's, as the tmux lane's is the host's boot-resolved one.
	opts.Agent = []string{"/opt/omp/bin/omp", "--operator"}
	overriding, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	overridden := launchOf(t, overriding, spec).agentArgv(overriding.agent)
	if slices.Contains(overridden, "--operator") {
		t.Errorf("with Options.Agent set the Claude pod's command is %q, want the generation's Claude Code alone", overridden)
	}
}

// A Claude Code role is told where Claude Code keeps its state and that the clock may not prompt
// it, and an Oh My Pi role is told neither. The directory is the slot's own under its state home,
// so it is no other slot's. Nothing in the pod turns Claude Code's background tasks off: a task
// that finishes wakes a turn of Claude Code's own, which the bridge reports to the daemon as a
// turn, so the name is not the runtime's and an operator's pod may carry it.
func TestOnlyAClaudeWorkerIsToldClaudeCodesOwnVariables(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := claudeWorkerSpec(t)
	claude := envOf(workerOf(t, r, spec))
	for name, expect := range map[string]struct {
		want  string
		owned bool
	}{
		"CLAUDE_CONFIG_DIR":                    {want: scratchRoot + "/state/" + string(spec.Role) + "/claude", owned: true},
		"CLAUDE_CODE_DISABLE_CRON":             {want: "1", owned: true},
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": {want: "", owned: false},
	} {
		if claude[name] != expect.want {
			t.Errorf("a Claude worker is told %s=%q, want %q", name, claude[name], expect.want)
		}
		if omp := envOf(workerOf(t, r, workerSpec(t))); omp[name] != "" {
			t.Errorf("an Oh My Pi worker is told %s=%q, want nothing", name, omp[name])
		}
		if owned := runtimeOwned[name]; owned != expect.owned {
			t.Errorf("runtimeOwned[%s] = %t, want %t: the runtime owns the names it sets and no other", name, owned, expect.owned)
		}
	}
}

// A Claude Code role's relaunch continues the transcript its replaced pod left: the agent command
// ends `--resume <uuid>`, the id parsed from the recorded path, and names neither a --session-id
// nor the path itself — Claude Code resumes by id alone. The recorded path still reaches the role
// launcher, which looks it up in the session table and restores it from its row before the child
// starts (internal/launcher). prepare is where the rule is applied, so a Claude claim still
// recording an Oh My Pi session is refused there, naming the backend change that left it recorded.
func TestAResumingClaudePodContinuesItsTranscriptByItsID(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := claudeResumeSpec(t)
	l := launchOf(t, r, spec)
	argv := l.agentArgv(r.agent)
	want := []string{"--resume", claudeTranscriptID, "--append-system-prompt-file", systemPromptPath(spec.Generation)}
	if got := argv[max(len(argv)-4, 0):]; !slices.Equal(got, want) {
		t.Errorf("the resuming Claude command ends %q, want %q", got, want)
	}
	if slices.Contains(argv, "--session-id") {
		t.Errorf("the resuming Claude command %q names --session-id, want the recorded session continued", argv)
	}
	for _, arg := range argv {
		if strings.Contains(arg, spec.ResumeSessionFile) {
			t.Errorf("the resuming Claude command carries the recorded path in %q, want its session id alone", arg)
		}
	}
	if start := launcherCommand(l, r); start.ResumeFile != spec.ResumeSessionFile {
		t.Errorf("the start command resumes %q, want the recorded transcript %s", start.ResumeFile, spec.ResumeSessionFile)
	}
	stale := claudeWorkerSpec(t)
	stale.Generation, stale.BootToken, stale.ResumeSessionFile = 2, "boot-g2", resumeSession
	if _, err := r.prepare(stale); err == nil || !strings.Contains(err.Error(), "the role's backend changed since it was recorded") {
		t.Errorf("prepare of a Claude claim recording the Oh My Pi session %s = %v, want a refusal naming the backend change", resumeSession, err)
	}
}

// A Claude claim's recorded session is held to its harness's rule, where the launch resolves it: a
// transcript of the pod's own Claude Code directory and nothing else, which prepare reads through
// claudelaunch.ResumeSession as the tmux lane's resume does: one of another slot's directory, an
// Oh My Pi session file, or a name that is no session id is refused, and a session of the other
// harness is refused naming the backend change that left it recorded.
func TestARecordedSessionIsHeldToItsHarnesssRule(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	resuming := func(file string) (launch, error) {
		spec := claudeResumeSpec(t)
		spec.ResumeSessionFile = file
		return r.prepare(spec)
	}
	transcript := claudeTranscript(claim.RoleTester)
	l, err := resuming(transcript)
	if err != nil || l.claude.session != claudeTranscriptID || !l.claude.resume {
		t.Fatalf("prepare resuming %s = %q (resume %v), %v; want the transcript's session id %s",
			transcript, l.claude.session, l.claude.resume, err, claudeTranscriptID)
	}
	for name, file := range map[string]string{
		"a transcript of another role's directory": claudeTranscript(claim.RolePlanner),
		"an Oh My Pi session file":                 resumeSession,
	} {
		if _, err := resuming(file); err == nil || !strings.Contains(err.Error(), file) ||
			!strings.Contains(err.Error(), "the role's backend changed since it was recorded") {
			t.Errorf("prepare of a Claude claim resuming %s (%s) = %v, want a refusal naming it and the backend change", name, file, err)
		}
	}
	named := claudeConfigDir(claim.Agent(claim.RoleTester)) + "/projects/--cwd--/2026-09-23T12-00-00-000Z_0198.jsonl"
	if _, err := resuming(named); err == nil ||
		!strings.Contains(err.Error(), "not named for a Claude Code session id") {
		t.Errorf("prepare of a Claude claim resuming %s = %v, want it refused as no session id", named, err)
	}
}

// Every Oh My Pi launch of a role whose settings name no model names the model the pod's overlay
// puts on the `default` role. @default is Oh My Pi's own name for that role. A resumed launch names
// it too, since without the flag a resumed session runs the session file's recorded model. A role
// of `roles` or of the issue's plan that names a model is launched with that model instead. From
// Oh My Pi 18.18.6 of the fork (upstream oh-my-pi #18534), a resumed launch under this role alias
// appends a replacement
// `model_change` for its resolved model, which later launches without the flag read.
// (TestTheResumedAgentRunsTheConfiguredModel measures that durable replacement on the real
// binary.) Its place in the argv is fixed here, after the two extension roots and before
// `--resume`. A Claude role's command is claudelaunch's and names a model only where a role of
// `roles` names one.
func TestAgentArgvCarriesTheConfiguredModel(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	fresh := launchOf(t, r, workerSpec(t))
	resuming := workerSpec(t)
	resuming.Generation, resuming.BootToken, resuming.ResumeSessionFile = 2, "boot-g2", resumeSession
	resumed := launchOf(t, r, resuming)
	modelled := workerSpec(t)
	modelled.Model = "checker-model"
	named := launchOf(t, r, modelled)
	for _, tc := range []struct {
		name string
		l    launch
		want []string
	}{
		{"a launch that starts a session", fresh, append(slices.Clone(r.agent),
			"--extension", envoyPlugin, "--extension", legionPlugin, "--model", "@default",
			"--mode", "rpc", "--append-system-prompt", systemPromptPath(fresh.spec.Generation))},
		{"a launch that resumes one", resumed, append(slices.Clone(r.agent),
			"--extension", envoyPlugin, "--extension", legionPlugin, "--model", "@default",
			"--resume="+resumeSession, "--mode", "rpc", "--append-system-prompt", systemPromptPath(resumed.spec.Generation))},
		{"a launch whose spec names its role's model", named, append(slices.Clone(r.agent),
			"--extension", envoyPlugin, "--extension", legionPlugin, "--model", "checker-model",
			"--mode", "rpc", "--append-system-prompt", systemPromptPath(named.spec.Generation))},
	} {
		if got := tc.l.agentArgv(r.agent); !slices.Equal(got, tc.want) {
			t.Errorf("%s runs %q, want %q", tc.name, got, tc.want)
		}
	}
	modelledClaude := claudeWorkerSpec(t)
	modelledClaude.Model = "checker-model"
	if claude := launchOf(t, r, modelledClaude).agentArgv(r.agent); !slices.Contains(claude, "--model") ||
		claude[slices.Index(claude, "--model")+1] != "checker-model" {
		t.Errorf("the Claude pod's command for a spec naming its role's model = %q, want it to hold --model checker-model", claude)
	}
	if claude := launchOf(t, r, claudeWorkerSpec(t)).agentArgv(r.agent); slices.Contains(claude, "--model") {
		t.Errorf("the Claude pod's command %q names a model, want claudelaunch's own command", claude)
	}
}

// A launch that resumes a recorded Oh My Pi session, a worker's or the controller's, is told
// OMP_SESSION_EXCLUSIVE=1 once in its start command, which holds its Oh My Pi to that session: a
// resume of a session another live process still holds exits rather than moving the role onto a
// session id the daemon never recorded. A fresh launch is told nothing of it, and neither is a
// Claude Code role that resumes its transcript. The variable is the start command's alone: no
// container of the pod carries it, so a resume changes nothing of the pod it relaunches in.
func TestOnlyAResumedOhMyPiLaunchIsHeldToItsSession(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	resumed := workerSpec(t)
	resumed.Generation, resumed.BootToken, resumed.ResumeSessionFile = 2, "boot-g2", resumeSession
	controllerResumed := controllerSpec(t)
	controllerResumed.Generation, controllerResumed.BootToken, controllerResumed.ResumeSessionFile = 2, "boot-g2", controllerSession
	for name, tc := range map[string]struct {
		spec runtime.SpawnSpec
		want []string
	}{
		"a resumed Oh My Pi worker":    {resumed, []string{"1"}},
		"the resumed controller":       {controllerResumed, []string{"1"}},
		"a fresh Oh My Pi worker":      {workerSpec(t), nil},
		"the fresh controller":         {controllerSpec(t), nil},
		"a resumed Claude Code worker": {claudeResumeSpec(t), nil},
	} {
		l := launchOf(t, r, tc.spec)
		var values []string
		for _, entry := range launcherCommand(l, r).Env {
			if variable, value, _ := strings.Cut(entry, "="); variable == "OMP_SESSION_EXCLUSIVE" {
				values = append(values, value)
			}
		}
		if !slices.Equal(values, tc.want) {
			t.Errorf("%s's start command sets OMP_SESSION_EXCLUSIVE to %q, want %q", name, values, tc.want)
		}
		pod := r.podTemplate(l).Spec
		for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
			for _, v := range c.Env {
				if v.Name == "OMP_SESSION_EXCLUSIVE" {
					t.Errorf("%s's pod container %s carries OMP_SESSION_EXCLUSIVE=%q, want it in the start command alone", name, c.Name, v.Value)
				}
			}
		}
	}
}
