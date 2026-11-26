package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	legionruntime "github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The names the Sandbox runtime puts on the controller's pod and in its launcher's private
// directory, written out because they are package sandbox's own (manifest.go's gvisor,
// microVMRuntimeClass and computeClassKey, names.go's labelSandbox and bootTokenKey, drift.go's
// podShapeAnnotation, podkind.go's --microvm): a test of what the daemon renders reads them off the
// pod the fake cluster made, as an operator reads them off the cluster.
const (
	controllerSandboxComputeKey    = "cloud.google.com/compute-class"
	controllerSandboxLabel         = "legion.dev/sandbox"
	controllerSandboxShapeKey      = "legion.dev/pod-shape"
	controllerSandboxGVisorClass   = "gvisor"
	controllerSandboxMicroVMClass  = "microvm"
	controllerSandboxBootTokenFile = "LEGION_BOOT_TOKEN"
	controllerSandboxMicroVMFlag   = "--microvm"
)

// The session every generation of the controller's stand-in registers, the one its relaunch
// resumes, and the owner the controller's GitHub bundle names.
const (
	controllerSandboxSession     = "ses-controller-sandbox"
	controllerSandboxSessionFile = "/legion/sessions/controller-sandbox.jsonl"
	controllerSandboxOwner       = "acme"
)

// The ComputeClass the operator's scheduling names, which a gVisor pod runs on, and the
// project's microvm block in both configurations of the test: a ComputeClass and a disk class of
// its own, and no metadata endpoint. An in-process launcher told the endpoint's flags would serve
// it on the fixed 127.0.0.1:8169 and set GCE_METADATA_HOST in this test process
// (internal/launcher's serveMetadata), whose test-only address this package cannot reach; the
// endpoint is TestTheControllerPodRunsInTheProjectsSandbox's and internal/launcher's to prove.
const (
	controllerSandboxScaleOut     = "Scale-Out"
	controllerSandboxComputeClass = "legion-microvm"
	controllerSandboxDiskClass    = "legion-microvm-disk"
)

// controllerSandboxStep is how long the test waits for one step of the daemon's own: a launch, a
// boot's re-adoption, a relaunch. Each is a few seconds on an idle machine, and the bound is what
// a loaded issue pod running a Postgres, a nats-server and this test's daemon and launchers may
// take, so a step that never comes fails naming itself rather than at the binary's deadline.
const controllerSandboxStep = 3 * time.Minute

// controllerSandboxPoll is how often a wait asks its condition: each ask lists the daemon's
// claims over its API, which a hundred asks a second would take the daemon away from the work
// they wait for.
const controllerSandboxPoll = 250 * time.Millisecond

// controllerSandboxSettled is how long a boot's re-adoption must stand before the test takes it
// for the boot's answer: three of the runtime's sweeps of every watched claim (the rig's probe
// interval is a second), each of which would have reported a pod it was replacing.
const controllerSandboxSettled = 3 * time.Second

// controllerSandboxSetting is one configuration of runtime.kubernetes.sandbox the test boots on:
// its name, which the logged lines carry, and the block. Both of the test's settings name the
// same microvm block, so a move between them changes `type` and nothing else.
type controllerSandboxSetting struct {
	name    string
	sandbox config.SandboxConfig
}

// microVM is whether the setting runs the controller's pod in a microVM: its type is microvm
// (SandboxConfig.Controller in package sandbox).
func (s controllerSandboxSetting) microVM() bool { return s.sandbox.Type == config.SandboxMicroVM }

// controllerSandboxBlock is the microvm block both settings name.
func controllerSandboxBlock() *config.MicroVMConfig {
	return &config.MicroVMConfig{ComputeClass: controllerSandboxComputeClass, StorageClass: controllerSandboxDiskClass}
}

// controllerSandboxScheduling is the operator's scheduling in both settings, as
// TestTheControllerPodRunsInTheProjectsSandbox sets it up: a selector naming the Scale-Out
// ComputeClass and the architecture, and the tolerations of Scale-Out's taint and of one other.
// A microVM pod's placement then differs from a gVisor pod's in the class it selects and
// tolerates and in nothing else.
func controllerSandboxScheduling() config.Scheduling {
	return config.Scheduling{
		NodeSelector: map[string]string{controllerSandboxComputeKey: controllerSandboxScaleOut, "kubernetes.io/arch": "amd64"},
		Tolerations: []config.Toleration{
			{Key: controllerSandboxComputeKey, Operator: "Equal", Value: controllerSandboxScaleOut, Effect: "NoSchedule"},
			{Key: "dedicated", Operator: "Equal", Value: "legion", Effect: "NoSchedule"},
		},
	}
}

// controllerSandboxToleration is a NoSchedule toleration of key=value.
func controllerSandboxToleration(key, value string) corev1.Toleration {
	return corev1.Toleration{Key: key, Operator: corev1.TolerationOpEqual, Value: value, Effect: corev1.TaintEffectNoSchedule}
}

// controllerSandboxPlacement is what the controller's pod of a setting is: its RuntimeClass, its
// exact node selector and tolerations, whether its template carries legion.dev/sandbox=microvm
// and its launcher --microvm, and the sandbox member of the shape its template records (nil for
// none, a gVisor pod's).
type controllerSandboxPlacement struct {
	runtimeClass string
	nodeSelector map[string]string
	tolerations  []corev1.Toleration
	microVM      bool
	shape        map[string]any
}

// placement is the controller's pod a daemon on s renders: under microvm, the microvm
// RuntimeClass, the operator's selector with the block's ComputeClass in place of Scale-Out, the
// operator's other toleration and the class's, Scale-Out's dropped, the label, the flag, and the
// recorded runtime and compute class (the block names no metadata endpoint, and the controller's
// shape records no disk class); under gvisor, the pod as the operator's scheduling is written,
// with none of those.
func (s controllerSandboxSetting) placement() controllerSandboxPlacement {
	dedicated := controllerSandboxToleration("dedicated", "legion")
	if s.microVM() {
		return controllerSandboxPlacement{
			runtimeClass: controllerSandboxMicroVMClass,
			nodeSelector: map[string]string{"kubernetes.io/arch": "amd64", controllerSandboxComputeKey: controllerSandboxComputeClass},
			tolerations:  []corev1.Toleration{dedicated, controllerSandboxToleration(controllerSandboxComputeKey, controllerSandboxComputeClass)},
			microVM:      true,
			shape:        map[string]any{"runtime_class": controllerSandboxMicroVMClass, "compute_class": controllerSandboxComputeClass},
		}
	}
	return controllerSandboxPlacement{
		runtimeClass: controllerSandboxGVisorClass,
		nodeSelector: map[string]string{"kubernetes.io/arch": "amd64", controllerSandboxComputeKey: controllerSandboxScaleOut},
		tolerations:  []corev1.Toleration{controllerSandboxToleration(controllerSandboxComputeKey, controllerSandboxScaleOut), dedicated},
	}
}

// controllerSandboxRoute is how the test ends the controller's generation that the second boot
// re-adopted, which leaves the daemon to launch the next one: end runs against the rig and that
// generation.
type controllerSandboxRoute struct {
	name string
	end  func(r *controllerSandboxRig, readopted controllerSandboxServing)
}

// The two routes to the controller's next launch. The generation's process dies: the stand-in
// exits while its pod is live, which the daemon reads as a worker that died and relaunches, as it
// relaunches a controller whose Oh My Pi crashed. Or the operator suspends the controller's claim
// and resumes it, the route docs/kubernetes.md gives an operator for relaunching a daemon-launched
// controller at once (there, onto a new image).
var (
	controllerGenerationDies = controllerSandboxRoute{name: "the-generation-dies", end: func(r *controllerSandboxRig, readopted controllerSandboxServing) {
		r.endGeneration(readopted.generation)
	}}
	controllerSuspendedAndResumed = controllerSandboxRoute{name: "the-operator-suspends-and-resumes", end: func(r *controllerSandboxRig, _ controllerSandboxServing) {
		r.suspendAndResume()
	}}
)

// A change of runtime.kubernetes.sandbox.type reaches the project controller's pod (`controller:
// daemon`) at the controller's next launch, never at a boot, in both directions. The daemon boots
// on the first setting, through its own boot (run) with the real Sandbox runtime over the
// package's fake Kubernetes API (issueSandboxAPI) and a real single-role `legion launcher` in this
// process for every pod that API reports Ready, its --microvm read from that pod's own container
// command; its controller's pod comes up and its claim reaches idle. The daemon stops and boots
// again on the second setting over the same database, worker stream and API address, and
// re-adopts that pod: the same uid, built as before, its process still the one generation. Then
// the controller's generation ends while its pod is live — its process dies, or the operator
// suspends and resumes the claim — and the launch that follows replaces the pod with one of the
// second setting: a new uid, and its RuntimeClass, selector, tolerations, label, launcher flag and
// recorded shape all that setting's.
//
// The microvm block is in both settings, so the move changes `type` alone, and every address a
// pod is handed is the same across the boot (sandbox's handedAddresses): what the relaunch
// replaces the pod for is the shape its template recorded (relaunch, shapeDrift) and nothing a
// moved address would also explain.
//
// Each step logs the controller's pod as one `sandbox-pod` line, in the form of
// TestTheControllerPodRunsInTheProjectsSandbox's with its uid and the sandbox its shape records,
// and `at=` naming the step: first-boot, second-boot, relaunch.
func TestTheDaemonsControllerMovesIntoTheProjectsSandboxAtItsNextLaunch(t *testing.T) {
	t.Parallel()
	gvisor := controllerSandboxSetting{name: "gvisor", sandbox: config.SandboxConfig{Type: config.SandboxGVisor, MicroVM: controllerSandboxBlock()}}
	microVM := controllerSandboxSetting{name: "microvm", sandbox: config.SandboxConfig{Type: config.SandboxMicroVM, MicroVM: controllerSandboxBlock()}}
	for _, direction := range []struct {
		name     string
		from, to controllerSandboxSetting
		routes   []controllerSandboxRoute
	}{
		{"gvisor-to-microvm", gvisor, microVM, []controllerSandboxRoute{controllerGenerationDies, controllerSuspendedAndResumed}},
		{"microvm-to-gvisor", microVM, gvisor, []controllerSandboxRoute{controllerGenerationDies}},
	} {
		t.Run(direction.name, func(t *testing.T) {
			for _, route := range direction.routes {
				t.Run(route.name, func(t *testing.T) {
					controllerSandboxMove(t, direction.from, direction.to, route)
				})
			}
		})
	}
}

// controllerSandboxMove is one case: the daemon boots on from, boots again on to, re-adopts the
// controller's pod, and replaces it with one of to's at the launch route brings about.
func controllerSandboxMove(t *testing.T, from, to controllerSandboxSetting, route controllerSandboxRoute) {
	r := newControllerSandboxRig(t, from)
	r.boot()
	first := r.awaitServing("the controller's first launch to come up under "+from.name, "", 0)
	r.logPod(from.name, "first-boot", first.pod)
	r.requirePlaced(first.pod, from)

	r.restart(to)
	readopted := r.awaitReadopted(first)
	r.logPod(to.name, "second-boot", readopted.pod)
	// The pod the second boot keeps is still the first setting's, as it was built: the boot
	// re-adopts what runs and renders nothing.
	r.requirePlaced(readopted.pod, from)

	route.end(r, readopted)
	// The wait is the assertion that the pod was replaced: it takes no pod of the first uid, and a
	// launch that kept that pod fails it.
	relaunched := r.awaitServing("the controller's next launch to come up in a pod other than "+first.pod.uid, first.pod.uid, first.generation)
	r.logPod(to.name, "relaunch", relaunched.pod)
	r.requirePlaced(relaunched.pod, to)
	if starts := r.pods.launchers.starts(r.name); starts != 2 {
		t.Errorf("the controller's pods were started %d times, want twice: the first boot's and the relaunch's", starts)
	}
	r.kube.requireServed(t)
}

// controllerSandboxRig is one daemon of the test over everything that outlives a boot: its
// database, its NATS server, its Dispatch board, the fake cluster and the launchers of that
// cluster's pods, and the worker stream and API addresses its pods are handed. Each boot builds a
// daemon over these from the configuration the rig holds when it boots.
type controllerSandboxRig struct {
	t   *testing.T
	cfg config.Config
	// token is the controller's claim, and name its Sandbox's and its pod's.
	token claim.Token
	name  string
	// project is the configuration's project as a claim token carries it.
	project string
	kube    *issueSandboxAPI
	pods    *controllerSandboxPods
	// legion and agent are the runtime's `legion` and agent command, written once for the rig.
	legion string
	agent  []string
	// logs is the daemon's log and its runtime's, every boot's.
	logs *syncBuffer
	d    *daemon
	// apiSocket and streamSocket are the rig's descriptors of the listening sockets on cfg's port
	// and worker_stream_port, held from the rig's start until the test ends; every boot serves on
	// descriptors of its own of the two (heldSocketListen).
	apiSocket, streamSocket *os.File
}

// newControllerSandboxRig readies every part a boot needs and serves the fake cluster, its
// controller stand-in running, with the daemon configured on first.
//
// The configuration is a workflow daemon (workflowConfig: NATS, a Dispatch board) under
// `controller: daemon` and `runtime: kubernetes` (kubernetesConfig, its kubeconfig naming the
// fake cluster, which boot reads whatever runtime it is handed), with the operator's scheduling
// and first's sandbox block. Its session store is the idle rig's: cloudsql-iam over the rig's
// own database (isolatedOutboxPool), which every launcher's session proxy reaches through
// idleRigDialer, so the daemon's relaunch of the controller resumes the session its stand-in
// registered, as a pod's does, rather than a resume no store could answer.
//
// The worker stream listens on a port of the rig's own rather than one the kernel picks at each
// bind (testConfig's 0): a pod's launcher dials the stream named in its command for the pod's
// life, so the second boot must listen where the first pod dials, as a deployment's fixed
// worker_stream_port does. The rig holds a listening socket on that port (heldSocket), and takes
// the one testConfig holds on the API port (takeHeldSocket), from its start until the test ends;
// each boot serves on descriptors of its own of the two (heldSocketListen, through
// overrides.listen and overrides.streamListen). A daemon's stop closes its descriptors and the
// rig's keep both sockets listening, so no other program can take either port before the first
// boot or between the two. A port found free and given back would be any program's to take before
// the daemon bound it, and on a shared CI runner one was taken, which failed the boot with
// `bind: address already in use`. Because the rig's sockets keep listening, a launcher's redial
// between the two boots is queued, not refused as it is in a deployment's restart, where the
// daemon's close frees the port: the kernel completes the dial, the launcher writes its hello and
// waits for the acknowledgement, and the second boot's stream listener accepts it. Nothing this
// test asserts reads a refused redial.
// The probe interval is a second, so the runtime's sweep of every watched claim, which a boot's
// re-adoption is held against (controllerSandboxSettled), runs several times inside the test's
// own window.
func newControllerSandboxRig(t *testing.T, first controllerSandboxSetting) *controllerSandboxRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	if _, err := pool.Exec(context.Background(), idleRigSessionTable()); err != nil {
		t.Fatalf("make the session table the launchers look a resume up in: %v", err)
	}
	natsURL := workflowNATS(t)
	kube := newIssueSandboxAPI()
	kube.serve(t)
	cfg := workflowConfig(t, natsURL)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	apiSocket := takeHeldSocket(t, net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port)))
	streamSocket, streamPort := heldSocket(t)
	cfg.WorkerStreamPort = streamPort
	cfg.ProbeInterval = time.Second
	cfg.Runtime = kubernetesConfig(t, kube.url).Runtime
	k := cfg.Runtime.Kubernetes
	k.Scheduling = controllerSandboxScheduling()
	k.Sandbox = first.sandbox
	k.SessionStore = ompsessions.Store{Kind: ompsessions.StoreCloudSQLIAM, Database: ompsessions.Database{
		Instance: "legion-test:us-central1:sessions", Name: idleRigDatabase(t, pool), User: idleRigUser(t, pool),
	}}
	// Every launcher reaches the session database through its generation's proxy, which hands the
	// launcher's own startup packet to the server unchanged (idleRigSessionPassword).
	idleRigSessionPassword(t, pool)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := claim.ControllerToken(project)
	// The tools directory holds the stand-ins and each generation's run and die files; the
	// longest of those is a die file of a generation number at its widest.
	tools := testtmp.Dir(t, "die-18446744073709551615")
	r := &controllerSandboxRig{t: t, cfg: cfg, token: token, name: sandbox.SandboxName(token), project: project,
		kube: kube, legion: controllerSandboxLegionTool(t, tools), agent: idleRigAgentTool(t, tools), logs: &syncBuffer{},
		apiSocket: apiSocket, streamSocket: streamSocket}
	r.pods = newControllerSandboxPods(t, kube, tools, cfg.DaemonURL, pool, k.SessionStore.Database,
		func(context.Context, sessionproxy.Signer) (sessionproxy.Dialer, error) {
			return idleRigDialer{address: idleRigHostPort(t, pool)}, nil
		})
	kube.onPodReady = r.pods.podReady
	kube.onPodGone = func(pod string) { r.pods.launchers.podGone(kube.prefix, pod) }
	kube.run(t)
	// Registered before any boot, so it runs after every daemon's stop: the logs it prints carry
	// the shutdown's own lines.
	t.Cleanup(r.printLogsOnFailure)
	return r
}

// boot runs the daemon on the rig's configuration as it stands, returning once it answers
// /healthz. Its clock's timers never fire (stillClock), as the idle rig's do not: every step the
// test waits for is a request, a launcher's report or a connection's end, never a machine's timer.
// The client the test asks the daemon with waits as long as a step may, since an operator's
// resume is answered once its machine's decision is made (api's postClaimEvent), and that
// decision may be the whole relaunch, the pod's replacement with it.
func (r *controllerSandboxRig) boot() {
	r.t.Helper()
	cfg := r.cfg
	logger := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.d = startDaemonWithin(r.t, cfg, overrides{
		listen: heldSocketListen(r.apiSocket, cfg.Port), streamListen: heldSocketListen(r.streamSocket, cfg.WorkerStreamPort),
		runtime: r.runtimeFor(cfg), clock: stillClock{}, workflowTokens: &workflowTokenRecorder{},
	}, logger, 30*time.Second)
	r.d.client.Timeout = controllerSandboxStep
}

// restart stops the daemon and boots it again on to's sandbox block, over the same database,
// worker stream port and API port, which the rig held through the stop (heldSocketListen): the
// daemon's URL is an address every generation is handed and the Sandbox records (sandbox's
// handedAddresses), and a boot that moved it would have the controller's next launch replace its
// pod for the moved address whatever its shape.
func (r *controllerSandboxRig) restart(to controllerSandboxSetting) {
	r.t.Helper()
	r.d.stop()
	k := *r.cfg.Runtime.Kubernetes
	k.Sandbox = to.sandbox
	r.cfg.Runtime.Kubernetes = &k
	r.boot()
}

// runtimeFor is the runtime factory a boot on cfg runs with: the real Sandbox runtime
// (sandbox.New) over the fake cluster, built from the daemon's own options (sandboxOptions) and
// the worker stream, store, connection directory and project release mark the boot hands it, as
// idleWakeRig.newRuntime builds it, with what the controller's launch reads beside them: its
// GitHub bundle and the owners' directories, the provisioning tokens and repository set every
// runtime is refused without (checkSupervision), and the rig's `legion` and agent command.
func (r *controllerSandboxRig) runtimeFor(cfg config.Config) runtimeFactory {
	return func(ctx context.Context, listener *stream.Listener, address string, _ appauth.Source,
		_ func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, _ func() []ghrepo.Repository,
		held func() bool,
	) (legionruntime.Runtime, error) {
		opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, r.project, sandbox.DispatchCredential{Bearer: "placeholder-dispatch-token"}, lookup(nil),
			slog.New(slog.NewJSONHandler(r.logs, nil)))
		if err != nil {
			return nil, fmt.Errorf("sandboxOptions: %w", err)
		}
		opts.Conns, opts.StreamURL, opts.Store, opts.ProjectReleaseHolds = listener, address, st, held
		opts.Tokens = idleRigTokens{}
		opts.GitHubCredential = func(_ context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
			rendered := ghconfig.Render("ghs_controller_sandbox", string(appauth.AppRoleFor(role)), time.Now().Add(time.Hour))
			owner := strings.ToLower(repo.Owner())
			return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: rendered}}, nil
		}
		opts.ControllerCredential = func(_ context.Context, defaultOwner string) (ghconfig.Bundle, error) {
			rendered := ghconfig.Render("ghs_controller_sandbox", string(appauth.Review), time.Now().Add(time.Hour))
			owner := controllerSandboxOwner
			if defaultOwner != "" {
				owner = defaultOwner
			}
			return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: rendered}}, nil
		}
		opts.GitHubDirs = func(ghrepo.Repository, []appauth.AppRole) []string { return []string{controllerSandboxOwner} }
		opts.CacheRepositories = func() []ghrepo.Repository { return nil }
		opts.Tools.Legion = r.legion
		opts.Agent = r.agent
		opts.DaemonURL, opts.EnvoyURL = cfg.DaemonURL, cfg.EnvoyURL
		rt, err := sandbox.New(ctx, sandboxRESTConfig(r.kube.url), opts)
		if err != nil {
			return nil, fmt.Errorf("sandbox.New: %w", err)
		}
		listener.SetLauncherResolver(rt.LauncherResolver())
		return rt, nil
	}
}

// controllerSandboxServing is the controller as one wait found it serving: the generation its
// claim runs, and the pod that generation runs in.
type controllerSandboxServing struct {
	generation uint64
	pod        controllerSandboxPodSeen
}

// awaitServing waits for the controller's claim to be idle with no task held, at a generation
// past after, its process in the pod the cluster holds for its Sandbox, that pod not notUID, and
// that generation's stand-in registered with the daemon: a launch that came up and whose start
// turn has ended. A generation that comes up that way in the pod notUID instead is a launch that
// kept that pod, and fails the test at once rather than at the wait's end.
func (r *controllerSandboxRig) awaitServing(what, notUID string, after uint64) controllerSandboxServing {
	r.t.Helper()
	var serving controllerSandboxServing
	kept := false
	r.await(what, func() bool {
		c, listed := r.claim()
		if !listed || c.State != string(supervise.StateIdle) || c.Pending != nil || c.Generation <= after ||
			c.Locator == nil || c.Locator.Sandbox == nil || !r.pods.shim(c.Generation).registered {
			return false
		}
		serving.generation = c.Generation
		if notUID != "" && c.Locator.Sandbox.PodUID == notUID {
			kept = true
			return true
		}
		pod, live := r.kube.controllerSandboxPodOf(r.name)
		if !live || pod.uid == notUID || c.Locator.Sandbox.PodUID != pod.uid {
			return false
		}
		serving.pod = pod
		return true
	})
	if kept {
		r.t.Fatalf("the controller's generation %d came up in the pod %s its generation %d ran in, want a new pod; %s",
			serving.generation, notUID, after, r.diagnose())
	}
	return serving
}

// awaitReadopted waits for the second boot to have re-adopted the controller first found
// serving — its generation's stand-in has said hello to the new daemon, and the claim is idle at
// that generation in that pod — and then holds that for controllerSandboxSettled. The pod gone,
// a pod of another uid, or a second launcher started, at any point of either, is a boot that did
// not keep the pod, and fails the test at once: a daemon's stop and boot leave every pod running.
func (r *controllerSandboxRig) awaitReadopted(first controllerSandboxServing) controllerSandboxServing {
	r.t.Helper()
	notKept := func(pod controllerSandboxPodSeen, live bool) bool {
		return !live || pod.uid != first.pod.uid || r.pods.launchers.starts(r.name) != 1
	}
	var readopted controllerSandboxServing
	moved := false
	r.await("the second boot to re-adopt the controller's generation "+strconv.FormatUint(first.generation, 10)+" in its pod "+first.pod.uid, func() bool {
		pod, live := r.kube.controllerSandboxPodOf(r.name)
		if notKept(pod, live) {
			moved = true
			return true
		}
		if r.pods.shim(first.generation).acks < 2 {
			return false
		}
		c, listed := r.claim()
		if !listed || c.State != string(supervise.StateIdle) || c.Generation != first.generation ||
			c.Locator == nil || c.Locator.Sandbox == nil || c.Locator.Sandbox.PodUID != first.pod.uid {
			return false
		}
		readopted = controllerSandboxServing{generation: c.Generation, pod: pod}
		return true
	})
	if !moved {
		moved = testwait.Holds(controllerSandboxSettled, controllerSandboxPoll, func() bool {
			return notKept(r.kube.controllerSandboxPodOf(r.name))
		})
	}
	if moved {
		pod, _ := r.kube.controllerSandboxPodOf(r.name)
		r.t.Fatalf("the second boot did not keep the controller's pod %s (now %q, %d launchers started), want it re-adopted until the controller's next launch; %s",
			first.pod.uid, pod.uid, r.pods.launchers.starts(r.name), r.diagnose())
	}
	return readopted
}

// endGeneration has the controller's generation's process die while its pod is live: the
// stand-in exits, its launcher reaps it and removes its private directory, and only then does
// its worker stream connection close. That is the order a real shim's death reaches the daemon in
// when the launcher's report wins the race: the daemon takes the connection's end as its cue to
// probe the process, and the probe finds the exit the launcher reported.
func (r *controllerSandboxRig) endGeneration(generation uint64) {
	r.t.Helper()
	if err := r.pods.die(generation); err != nil {
		r.t.Fatal(err)
	}
	r.await("the controller's generation "+strconv.FormatUint(generation, 10)+" to end", func() bool {
		return r.pods.shim(generation).ended
	})
}

// suspendAndResume is the operator's route to the controller's next launch: `legion claims
// suspend` of its claim, then `legion claims resume`, over the routes those commands post to.
func (r *controllerSandboxRig) suspendAndResume() {
	r.t.Helper()
	path := "/legion/v1/operator/claims/" + string(r.token)
	if status, body := r.d.request(http.MethodPost, path+"/suspend", nil, true); status != http.StatusOK && status != http.StatusAccepted {
		r.t.Fatalf("suspend the controller's claim = %d; body %s", status, body)
	}
	r.await("the controller's claim to be suspended", func() bool {
		c, listed := r.claim()
		return listed && c.State == string(supervise.StateSuspended)
	})
	if status, body := r.d.request(http.MethodPost, path+"/resume", nil, true); status != http.StatusOK && status != http.StatusAccepted {
		r.t.Fatalf("resume the controller's claim = %d; body %s", status, body)
	}
}

// claim is the controller's claim as the operator's listing answers it, and whether the listing
// names it: the keeper creates it at its first launch, after the boot answers.
func (r *controllerSandboxRig) claim() (api.OperatorClaim, bool) {
	r.t.Helper()
	for _, c := range r.d.claims() {
		if c.Token == r.token {
			return c, true
		}
	}
	return api.OperatorClaim{}, false
}

// await waits for condition within controllerSandboxStep, asked every controllerSandboxPoll, and
// fails naming what it waited for and where the controller stood.
func (r *controllerSandboxRig) await(what string, condition func() bool) {
	r.t.Helper()
	if !testwait.Holds(testwait.Bounded(r.t, controllerSandboxStep, 10*time.Second), controllerSandboxPoll, condition) {
		r.t.Fatalf("timed out waiting for %s; %s", what, r.diagnose())
	}
}

// diagnose is where the controller stood when a wait gave up: its claim as the daemon lists it,
// its Sandbox and pod as the cluster holds them, how many times its pod's launcher was started and
// every generation's stand-in. The logs are printed once the test has failed (printLogsOnFailure).
func (r *controllerSandboxRig) diagnose() string {
	claimLine := "not listed"
	request, err := http.NewRequest(http.MethodGet, r.d.base+"/legion/v1/operator/claims", nil)
	if err == nil {
		request.Header.Set("Authorization", "Bearer "+testOperatorToken)
		var response *http.Response
		if response, err = r.d.client.Do(request); err == nil {
			var list api.OperatorClaims
			err = json.NewDecoder(response.Body).Decode(&list)
			response.Body.Close()
			for _, c := range list.Claims {
				if c.Token == r.token {
					claimLine = fmt.Sprintf("%s at generation %d, pending %v, locator %+v", c.State, c.Generation, c.Pending != nil, c.Locator)
					if c.Locator != nil && c.Locator.Sandbox != nil {
						claimLine += fmt.Sprintf(" (pod %s)", c.Locator.Sandbox.PodUID)
					}
				}
			}
		}
	}
	if err != nil {
		claimLine = "unread: " + err.Error()
	}
	pod, live := r.kube.controllerSandboxPodOf(r.name)
	return fmt.Sprintf("the controller's claim is %s; its Sandbox %s is %s, its pod %q (live %t); launchers started: %d; stand-ins: %s",
		claimLine, r.name, r.kube.mode(r.name), pod.uid, live, r.pods.launchers.starts(r.name), r.pods.shimsLine())
}

// printLogsOnFailure prints the end of the daemon's log and of the launchers' once the test has
// failed, which is where a launch that never came up says why.
func (r *controllerSandboxRig) printLogsOnFailure() {
	if !r.t.Failed() {
		return
	}
	r.t.Logf("the launchers' log, its last %d bytes:\n%s", 16<<10, controllerSandboxTail(r.pods.launchers.log(), 16<<10))
	r.t.Logf("the daemon's log, its last %d bytes:\n%s", 48<<10, controllerSandboxTail(r.logs.String(), 48<<10))
}

// controllerSandboxTail is the last n bytes of text, from the first whole line in them.
func controllerSandboxTail(text string, n int) string {
	if len(text) <= n {
		return text
	}
	text = text[len(text)-n:]
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[i+1:]
	}
	return text
}

// requirePlaced fails the test unless pod is the controller's pod setting renders
// (controllerSandboxSetting.placement).
func (r *controllerSandboxRig) requirePlaced(pod controllerSandboxPodSeen, setting controllerSandboxSetting) {
	r.t.Helper()
	want := setting.placement()
	spec := pod.pod.Spec
	if got := controllerSandboxRuntimeClass(spec); got != want.runtimeClass {
		r.t.Errorf("the controller's pod %s runs under the RuntimeClass %s, want %s (%s)", pod.uid, got, want.runtimeClass, setting.name)
	}
	if !maps.Equal(spec.NodeSelector, want.nodeSelector) {
		r.t.Errorf("the controller's pod %s selects %v, want exactly %v (%s)", pod.uid, spec.NodeSelector, want.nodeSelector, setting.name)
	}
	if !reflect.DeepEqual(spec.Tolerations, want.tolerations) {
		r.t.Errorf("the controller's pod %s tolerates %+v, want exactly %+v (%s)", pod.uid, spec.Tolerations, want.tolerations, setting.name)
	}
	label, labelled := pod.pod.Labels[controllerSandboxLabel]
	if labelled != want.microVM || (labelled && label != controllerSandboxMicroVMClass) {
		r.t.Errorf("the controller's pod %s is labelled %s=%q (labelled %t), want the label %t (%s)", pod.uid, controllerSandboxLabel, label, labelled, want.microVM, setting.name)
	}
	if flagged := slices.Contains(pod.command(), controllerSandboxMicroVMFlag); flagged != want.microVM {
		r.t.Errorf("the controller's pod %s runs %q, --microvm %t, want %t (%s)", pod.uid, pod.command(), flagged, want.microVM, setting.name)
	}
	member, err := pod.shapeSandbox()
	if err != nil {
		r.t.Errorf("the controller's pod %s: %v", pod.uid, err)
	}
	var got map[string]any
	if len(member) > 0 {
		if err := json.Unmarshal(member, &got); err != nil {
			r.t.Errorf("the controller's pod %s records the sandbox %s: %v", pod.uid, member, err)
		}
	}
	if !reflect.DeepEqual(got, want.shape) {
		r.t.Errorf("the controller's pod %s records the sandbox %v, want %v (%s)", pod.uid, got, want.shape, setting.name)
	}
}

// logPod logs the controller's pod as one line: the configuration the daemon runs, the pod's uid,
// RuntimeClass, node selector and labels in key order, tolerations in the pod's order, whether
// its launcher is told --microvm, the sandbox member of the shape its template records (null for
// none), and the step it was read at.
func (r *controllerSandboxRig) logPod(setting, at string, pod controllerSandboxPodSeen) {
	r.t.Helper()
	spec := pod.pod.Spec
	var tolerations []string
	for _, toleration := range spec.Tolerations {
		tolerations = append(tolerations, toleration.Key+"="+toleration.Value+":"+string(toleration.Effect))
	}
	flag := "none"
	if slices.Contains(pod.command(), controllerSandboxMicroVMFlag) {
		flag = controllerSandboxMicroVMFlag
	}
	shape := "null"
	if member, err := pod.shapeSandbox(); err != nil {
		shape = "unreadable(" + err.Error() + ")"
	} else if len(member) > 0 {
		shape = string(member)
	}
	r.t.Logf("sandbox-pod config=%s pod=controller uid=%s runtimeClassName=%s nodeSelector=%s tolerations=%s labels=%s launcher=%s shape=%s at=%s",
		setting, pod.uid, controllerSandboxRuntimeClass(spec), strings.Join(environPairs(spec.NodeSelector), ","), strings.Join(tolerations, ","),
		strings.Join(environPairs(pod.pod.Labels), ","), flag, shape, at)
}

// controllerSandboxRuntimeClass is the pod's RuntimeClass, "(none)" where it names none.
func controllerSandboxRuntimeClass(spec corev1.PodSpec) string {
	if spec.RuntimeClassName == nil {
		return "(none)"
	}
	return *spec.RuntimeClassName
}

// controllerSandboxPodSeen is the controller's pod as the fake cluster holds it, and the shape
// its Sandbox's template records: the pod copies its template's labels and not its annotations
// (createPodLocked), and the template is the one the pod was made from until a launch replaces
// both.
type controllerSandboxPodSeen struct {
	uid   string
	pod   corev1.Pod
	shape string
}

// command is the pod's controller container's command, the launcher's own argv.
func (p controllerSandboxPodSeen) command() []string {
	for _, c := range p.pod.Spec.Containers {
		if c.Name == legionruntime.ControllerPodContainer {
			return c.Command
		}
	}
	return nil
}

// shapeSandbox is the sandbox member of the recorded shape, as the annotation holds it, empty
// where it records none.
func (p controllerSandboxPodSeen) shapeSandbox() (json.RawMessage, error) {
	if p.shape == "" {
		return nil, errors.New("its template records no shape")
	}
	var record struct {
		Sandbox json.RawMessage `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(p.shape), &record); err != nil {
		return nil, fmt.Errorf("its recorded shape %s: %w", p.shape, err)
	}
	return record.Sandbox, nil
}

// controllerSandboxLegionTool writes, under dir, the `legion` the controller's launcher starts as
// each generation: the stand-in for `legion worker-shim … -- <omp argv>` as the pod's process.
// It says it runs, with a run file named by its generation (LEGION_GENERATION, which the runtime
// hands every generation), and then waits until the test writes that generation's die file, and
// exits as a process that died. The shim's half — the worker stream, the registration and the
// frames — is the test's own (controllerSandboxShim), as registeredLaunch plays it for the fake
// runtime: this binary's TestMain lets a re-entered run skip its guard, which takes out every
// LEGION_* variable a generation is handed, and its database setup only for the idle rig's role
// process (idleRigURLVariable).
func controllerSandboxLegionTool(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "legion")
	script := "#!/bin/sh\n" +
		": >" + shellQuote(filepath.Join(dir, "run-")) + "\"$LEGION_GENERATION\"\n" +
		"while [ ! -e " + shellQuote(filepath.Join(dir, "die-")) + "\"$LEGION_GENERATION\" ]; do sleep 0.1; done\n" +
		"exit 1\n"
	if err := execfile.Write(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the controller's legion stand-in: %v", err)
	}
	return path
}

// controllerSandboxPods is the controller's own part of running every pod the fake cluster
// reports Ready. The pod's one container's `legion launcher` runs as an issue pod's does, in its
// single-role form, through launchers (idleRigPods), which also makes its private directory,
// stops it when the pod is deleted and counts its starts; its Connect and MicroVM are read from
// the pod's own container command (launch), so the stream and the sandbox flag the daemon
// rendered are the ones the launcher runs with. Beside it, for each generation the launcher
// starts, the generation's shim (controllerSandboxShim), which the watch of that launcher's
// private directory starts (watchGenerations).
type controllerSandboxPods struct {
	// launchers runs the launcher of every pod the fake cluster reports Ready; its log holds the
	// rig's own lines too (logf).
	launchers *idleRigPods
	kube      *issueSandboxAPI
	// tools holds the generations' run and die files (controllerSandboxLegionTool).
	tools     string
	daemonURL string
	pool      *store.Pool

	mu    sync.Mutex
	shims map[uint64]*controllerSandboxShim
}

// newControllerSandboxPods is the controller's part over launchers of its own, whose launch is
// the controller's (launch): each launcher dials the stream its pod's own command names, not one
// of the rig's. The part is made first, so that its launch is the one its launchers are made with.
func newControllerSandboxPods(t *testing.T, kube *issueSandboxAPI, tools, daemonURL string, pool *store.Pool, session ompsessions.Database,
	dial func(ctx context.Context, signer sessionproxy.Signer) (sessionproxy.Dialer, error),
) *controllerSandboxPods {
	t.Helper()
	p := &controllerSandboxPods{kube: kube, tools: tools, daemonURL: daemonURL, pool: pool, shims: map[uint64]*controllerSandboxShim{}}
	p.launchers = newIdleRigPods(t, p.launch, session, dial)
	return p
}

// logf writes one line of the rig's own to the launchers' log.
func (p *controllerSandboxPods) logf(format string, args ...any) {
	fmt.Fprintf(p.launchers.logs, "controller sandbox rig: "+format+"\n", args...)
}

// podReady starts the pod's launcher with the controller's launcher token
// (idleRigPods.podReady), and beside it the watch that gives each of its generations a shim. It
// runs on the fake cluster's controller goroutine, off the API's lock, where a t.Fatalf would
// stop that goroutine alone: a pod it starts no launcher for is logged (logf), and a private
// directory idleRigPods.podReady cannot make fails the test through t.Errorf, which is safe off
// the test's goroutine.
func (p *controllerSandboxPods) podReady(pod, uid string, roles map[string]string) {
	role := string(claim.RoleController)
	token := roles[role]
	if token == "" {
		p.logf("the pod %s/%s holds no controller launcher token", pod, uid)
		return
	}
	p.launchers.podReady(p.kube.prefix, pod, uid, map[string]string{role: token}, nil, func(_, dir string, processes *idleRigPodProcesses) {
		processes.run(func(ctx context.Context) { p.watchGenerations(ctx, processes, dir) })
	})
}

// launch is how the launcher of the controller's pod runs (idleRigPods.launch): with the stream
// and the sandbox flag the daemon rendered into the pod's own container command. It reads the pod
// through the API, and starts no launcher for a pod gone before its launcher could start or for
// one whose command names no stream.
func (p *controllerSandboxPods) launch(pod, uid string) (connect string, microVM, ok bool) {
	seen, live := p.kube.controllerSandboxPodOf(pod)
	if !live || seen.uid != uid {
		p.logf("the pod %s/%s was gone before its launcher could start", pod, uid)
		return "", false, false
	}
	command := seen.command()
	if connect = controllerSandboxFlag(command, "--connect"); connect == "" {
		p.logf("the pod %s/%s names no stream: %q", pod, uid, command)
		return "", false, false
	}
	return connect, slices.Contains(command, controllerSandboxMicroVMFlag), true
}

// controllerSandboxFlag is the value after flag in command, "" where command names none.
func controllerSandboxFlag(command []string, flag string) string {
	i := slices.Index(command, flag)
	if i < 0 || i+1 >= len(command) {
		return ""
	}
	return command[i+1]
}

// watchGenerations gives each generation the launcher of the private directory dir starts a
// shim, once that generation's process says it runs (its run file) and its credentials are in
// dir, until the pod is gone. Each shim dials the stream the pod's launcher dials and runs beside
// it (idleRigPodProcesses.run), so the pod's removal waits for it. A generation's run file whose
// credentials are in no directory of this pod's is an earlier pod's generation, and is passed
// over.
func (p *controllerSandboxPods) watchGenerations(ctx context.Context, processes *idleRigPodProcesses, dir string) {
	address := strings.TrimPrefix(processes.connect, "tcp://")
	handled := map[uint64]bool{}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		entries, err := os.ReadDir(p.tools)
		if err != nil {
			p.logf("read the generations' run files: %v", err)
			continue
		}
		for _, entry := range entries {
			number, isRun := strings.CutPrefix(entry.Name(), "run-")
			generation, err := strconv.ParseUint(number, 10, 64)
			if !isRun || err != nil || handled[generation] {
				continue
			}
			generationDir := filepath.Join(dir, shimwire.GenerationDir(generation))
			bootToken, err := os.ReadFile(filepath.Join(generationDir, controllerSandboxBootTokenFile))
			if err != nil {
				continue
			}
			handled[generation] = true
			shim := &controllerSandboxShim{pods: p, address: address, generation: generation, bootToken: strings.TrimSpace(string(bootToken)),
				dir: generationDir, client: &http.Client{Timeout: 20 * time.Second}}
			p.mu.Lock()
			p.shims[generation] = shim
			p.mu.Unlock()
			processes.run(shim.run)
		}
	}
}

// die writes the generation's die file, which its process exits on.
func (p *controllerSandboxPods) die(generation uint64) error {
	path := filepath.Join(p.tools, "die-"+strconv.FormatUint(generation, 10))
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("tell the controller's generation %d to die: %w", generation, err)
	}
	return nil
}

// controllerSandboxShimState is what a generation's shim has done: how many of its hellos a
// daemon acknowledged, whether it registered and reported ready, and whether its process ended.
type controllerSandboxShimState struct {
	acks       int
	registered bool
	ended      bool
}

// shim is what the generation's shim has done, the zero state while it has none.
func (p *controllerSandboxPods) shim(generation uint64) controllerSandboxShimState {
	p.mu.Lock()
	s := p.shims[generation]
	p.mu.Unlock()
	if s == nil {
		return controllerSandboxShimState{}
	}
	return s.state()
}

// shimsLine is every generation's shim, for a failure to name.
func (p *controllerSandboxPods) shimsLine() string {
	p.mu.Lock()
	generations := slices.Sorted(maps.Keys(p.shims))
	p.mu.Unlock()
	var lines []string
	for _, generation := range generations {
		s := p.shim(generation)
		lines = append(lines, fmt.Sprintf("g%d acks=%d registered=%t ended=%t", generation, s.acks, s.registered, s.ended))
	}
	return "[" + strings.Join(lines, ", ") + "]"
}

// controllerSandboxShim is the shim of one controller generation, played by the test in its
// process (controllerSandboxLegionTool says why): it dials the pod's worker stream with the boot
// token the launcher wrote for the generation, registers the generation's agent with the daemon
// once, with this daemon's contract so no relaunch installs a plugin pair (specs'
// stalePluginInstall), and reports it ready, as registeredLaunch and the controller launch tests
// do; it answers the daemon's frames as the idle rig's role process answers them, a prompt with a
// turn; and it dials again whenever the stream drops, as `legion worker-shim` does, so a daemon
// that boots again finds the generation's shim back. It lives as long as its generation: once the
// launcher has removed the generation's private directory, which it does once the generation's
// process has ended, the shim closes its connection and dials no more.
type controllerSandboxShim struct {
	pods       *controllerSandboxPods
	address    string
	generation uint64
	bootToken  string
	dir        string
	client     *http.Client

	mu         sync.Mutex
	conn       net.Conn
	acks       int
	registered bool
	ended      bool
}

func (s *controllerSandboxShim) state() controllerSandboxShimState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return controllerSandboxShimState{acks: s.acks, registered: s.registered, ended: s.ended}
}

// end closes the shim's connection and has it dial no more.
func (s *controllerSandboxShim) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = true
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

func (s *controllerSandboxShim) isEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// run dials until the generation's process has ended or the pod is gone.
func (s *controllerSandboxShim) run(ctx context.Context) {
	var watching sync.WaitGroup
	stop := make(chan struct{})
	watching.Add(1)
	go func() {
		defer watching.Done()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				s.end()
				return
			case <-tick.C:
				if _, err := os.Stat(s.dir); errors.Is(err, fs.ErrNotExist) {
					s.end()
					return
				}
			}
		}
	}()
	defer func() {
		close(stop)
		watching.Wait()
	}()
	for !s.isEnded() {
		err := s.connect()
		if s.isEnded() {
			return
		}
		s.pods.logf("controller shim g%d: %v; dialing again", s.generation, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// connect is one connection: the hello, the wait for its acknowledgement, the registration on the
// first connection a daemon acknowledged, and the daemon's frames until the connection drops.
func (s *controllerSandboxShim) connect() error {
	conn, err := net.DialTimeout("tcp", s.address, 5*time.Second)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		_ = conn.Close()
		return errors.New("the generation ended")
	}
	s.conn = conn
	s.mu.Unlock()
	defer conn.Close()
	writer := shimwire.NewWriter(conn)
	lines := bufio.NewReader(conn)
	if err := writer.WriteFrame(shimwire.Hello2{BootToken: s.bootToken}); err != nil {
		return fmt.Errorf("send the hello: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	for {
		line, err := lines.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("the stream closed before its hello_ack: %w", err)
		}
		if frame, err := shimwire.Decode(line); err == nil && frame.FrameType() == shimwire.TypeHelloAck {
			break
		}
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	s.mu.Lock()
	s.acks++
	first := !s.registered
	s.mu.Unlock()
	if first {
		if err := s.register(); err != nil {
			return err
		}
	}
	for {
		line, err := lines.ReadBytes('\n')
		if err != nil {
			return err
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		if err := s.answer(writer, frame); err != nil {
			return err
		}
	}
}

// register registers the generation's agent and reports it ready. A registration the daemon
// refuses because its machine has not recorded the launch's process yet is asked again, as a
// shim whose hello came before the launch returned would find it, until the shim has ended, so a
// pod's removal, which waits for its shims (idleRigPods.podGone), does not wait out the retries;
// the session's row is written first, as Oh My Pi writes its session before its plugin
// registers, so the daemon's relaunch resumes a session the table holds (internal/launcher's
// sessionWritten).
func (s *controllerSandboxShim) register() error {
	if _, err := s.pods.pool.Exec(context.Background(),
		`insert into omp_session_files (path, mtime_ms) values ($1, $2)
		 on conflict (path) do update set mtime_ms = excluded.mtime_ms`,
		controllerSandboxSessionFile, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("write the session's row: %w", err)
	}
	var registration api.ControllerRegisterResponse
	deadline := time.Now().Add(time.Minute)
	for {
		status, err := s.post("/legion/v1/claims/register", claim.RegisterRequest{
			BootToken: s.bootToken, SessionID: controllerSandboxSession, OmpSessionFile: controllerSandboxSessionFile,
			AgentID: controllerSandboxSession, PluginContract: api.DaemonAPIVersion(),
		}, &registration)
		if err == nil && status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("register generation %d: %d %v", s.generation, status, err)
		}
		time.Sleep(200 * time.Millisecond)
		if s.isEnded() {
			return fmt.Errorf("register generation %d: the shim has ended: %d %v", s.generation, status, err)
		}
	}
	status, err := s.post("/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: registration.ClaimToken, SessionID: controllerSandboxSession, Secret: registration.Secret, Generation: s.generation,
	}, nil)
	if err != nil || status != http.StatusNoContent {
		return fmt.Errorf("report generation %d ready: %d %v", s.generation, status, err)
	}
	s.mu.Lock()
	s.registered = true
	s.mu.Unlock()
	return nil
}

// post sends body to the daemon's path and decodes a 2xx answer into answer, when it is given
// one; it answers the status, and an error for a request that got none or an answer that is no
// 2xx.
func (s *controllerSandboxShim) post(path string, body, answer any) (int, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	response, err := s.client.Post(s.pods.daemonURL+path, "application/json", strings.NewReader(string(encoded)))
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		refusal, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
		return response.StatusCode, fmt.Errorf("%s answered %d %s", path, response.StatusCode, strings.TrimSpace(string(refusal)))
	}
	if answer == nil {
		return response.StatusCode, nil
	}
	return response.StatusCode, json.NewDecoder(response.Body).Decode(answer)
}

// answer answers one of the daemon's frames as the idle rig's role process does
// (idleRigRoleProcess.serve): a prompt with a whole turn, the state as not streaming, the
// protocol as accepted, and a shutdown by having the generation's process exit.
func (s *controllerSandboxShim) answer(writer *shimwire.Writer, frame shimwire.Frame) error {
	switch f := frame.(type) {
	case shimwire.NegotiateProtocol:
		return writer.WriteFrame(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
	case shimwire.GetState:
		streaming := false
		data, err := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
		if err != nil {
			return err
		}
		return writer.WriteFrame(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
	case shimwire.Prompt:
		for _, reply := range []shimwire.Frame{
			shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true},
			shimwire.AgentStart{}, shimwire.AgentEnd{},
			shimwire.PromptResult{ID: f.ID, AgentInvoked: true},
		} {
			if err := writer.WriteFrame(reply); err != nil {
				return err
			}
		}
	case shimwire.AdoptWorkingCopy:
		return writer.WriteFrame(shimwire.AdoptWorkingCopyResult{ID: f.ID, OK: true})
	case shimwire.Shutdown:
		return s.pods.die(s.generation)
	}
	return nil
}
