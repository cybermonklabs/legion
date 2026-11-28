package daemon

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	legionruntime "github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// idleRigOlderImage is the worker image of the release before the one the rig's daemon is
// configured with (idleRigImage), pinned by digest: the image a pod the previous release launched
// still runs.
const idleRigOlderImage = "ghcr.io/example/worker@sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// A claim whose process registered on this daemon's contract, in an issue pod built on an older
// image than the configured one, is retired after three silent prompt failures, and the relaunch
// that follows comes back on this daemon's contract (dispatch://INFRA-3916 R8). The claim records
// the daemon's contract, so its relaunch's spec carries no plugin pair (stalePluginInstall); the
// pod is kept, since no update round has opened over it yet, and its image's own pi-legion
// declares the contract before this daemon's. The relaunch is a Resume into that kept pod, so the
// Sandbox runtime installs the pair the daemon pins (sandbox.Options.Plugins) and the process it
// starts loads that pair: it registers on this daemon's contract rather than on the image's, which
// the daemon would refuse and answer with one more relaunch.
//
// It runs over the real Sandbox runtime on the fake Kubernetes API, with a real launcher per pod
// and the scripted Oh My Pi of the idle rig (newIdleWakeRig), whose registration names the
// contract of the pi-legion its agent command loads (idleRigRoleProcess.pluginContract).
func TestAClaimRetiredForPromptFailuresInAPodOnAnOlderImageComesBackOnTheDaemonsContract(t *testing.T) {
	t.Parallel()
	pinned := config.PluginsConfig{PiLegion: "9.7.0", PiEnvoy: "9.6.1"}
	rig := newIdleWakeRigWith(t, idleRigOptions{Tune: func(r *idleWakeRig) {
		// The pair this daemon pins (ResolvedPlugins), which its specs and its runtime both read.
		r.cfg.Runtime.Kubernetes.Plugins = &pinned
		// Real timers: the wait for an acknowledged prompt's turn is the machine's own (noTurn),
		// and the waiting task is sent again at the probe, which a second makes quick.
		r.clock = supervise.RealClock{}
		r.cfg.ProbeInterval = time.Second
		// No update round opens over the pod while the test reads it, as none has opened while
		// the project's open-round limits hold it back: the pod stays on the image it was built on
		// (the planner's read for R8).
		r.orphanSweep = time.Hour
	}}, nil, "older")
	issue := rig.issue("older")
	token := rig.token(issue)
	defer func() {
		if t.Failed() {
			t.Log(rig.diagnose())
		}
	}()

	// The boot launched the implementer in a pod on the configured image and gave it its phase's
	// task, which it took a turn on: it now holds nothing, and its process registered on this
	// daemon's contract.
	var registered api.OperatorClaim
	idleRigEventually(t, "the implementer of "+issue.key+" idle on this daemon's contract, holding no task", func() bool {
		registered = rig.api.claim(token)
		return registered.State == string(supervise.StateIdle) && registered.Pending == nil &&
			registered.PluginContract == api.DaemonAPIVersion()
	})

	// That pod becomes one an older release launched: its shape and every container name the older
	// image, whose own pi-legion declares the contract before this daemon's. The claim keeps the
	// daemon's contract it registered on, which is the state an earlier install leaves. From here
	// its process acknowledges each task and starts no turn.
	older := api.DaemonAPIVersion() - 1
	rig.kube.buildOnImage(t, issue.pod, idleRigOlderImage)
	rig.agents.setPlan(issue.key, string(issue.role), idleRigPlan{TakeTurn: false, ImageContract: older})
	if status, body := rig.api.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		api.DeliverRequest{Task: "Carry on with the plan."}, true); status != http.StatusOK {
		t.Fatalf("the operator's deliver to %s answered %d %s, want 200", token, status, body)
	}

	// Three silent prompt failures retire the process, and the claim is relaunched: the first
	// process after the one that registered is the relaunch's.
	var relaunched idleRigRegistration
	idleRigEventually(t, "a process of "+string(token)+" after generation "+strconv.FormatUint(registered.Generation, 10)+" to register", func() bool {
		for _, registration := range rig.agents.role(issue.key, string(issue.role)).Registrations {
			if registration.Generation > registered.Generation {
				relaunched = registration
				return true
			}
		}
		return false
	})
	if retired := rig.logged("supervise: retired after prompt failures", map[string]string{"claim": string(token)}); retired == 0 {
		t.Fatalf("the daemon logged no retirement of %s after prompt failures, so generation %d is no retirement's relaunch",
			token, relaunched.Generation)
	}
	if starts := rig.pods.starts(issue.pod); starts != 1 {
		t.Fatalf("%s's pod was started %d times, want once: the relaunch keeps the pod built on the older image", issue.key, starts)
	}
	if relaunched.Contract != api.DaemonAPIVersion() {
		t.Fatalf("the relaunched process (generation %d) registered on contract %d, want this daemon's %d: its agent command %q loads the older image's pi-legion, which declares %d",
			relaunched.Generation, relaunched.Contract, api.DaemonAPIVersion(), relaunched.Agent, older)
	}
	// Its start installed the pinned pair before Oh My Pi started: the start wrapper the Sandbox
	// runtime puts around the agent command, `/bin/sh -c '<script>' legion-start …`, whose script
	// hands bun both of the pair's packages at their versions.
	agent := relaunched.Agent
	if len(agent) < 3 || agent[0] != "/bin/sh" || agent[1] != "-c" {
		t.Fatalf("the relaunched process's agent command is %q, want the start wrapper that installs the pinned pair", agent)
	}
	for _, source := range []string{legionruntime.PiLegionPackage + "@" + pinned.PiLegion, legionruntime.PiEnvoyPackage + "@" + pinned.PiEnvoy} {
		if !strings.Contains(agent[2], source) {
			t.Errorf("the relaunched process's start wrapper installs no %s:\n%s", source, agent[2])
		}
	}
}

// buildOnImage makes the live pod of the Sandbox name one built on image, as a pod an earlier
// release launched is: the shape its stored template records (the pod-shape annotation every
// template carries, controllerSandboxShapeKey) names image, and so does every container of that
// template and of the pod. The controller stand-in makes no new pod for a template that changed,
// as Agent Sandbox's does not, so the pod and its launchers stay.
func (a *issueSandboxAPI) buildOnImage(t *testing.T, name, image string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	held, pod := a.objects["sandboxes"][name], a.objects["pods"][name]
	if held == nil || pod == nil {
		t.Fatalf("the API holds no Sandbox %s with a pod to build on %s", name, image)
	}
	sandbox := kubeClone(held)
	spec, _ := sandbox["spec"].(map[string]any)
	template, _ := spec["podTemplate"].(map[string]any)
	meta, _ := template["metadata"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	shape, _ := annotations[controllerSandboxShapeKey].(string)
	var recorded struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal([]byte(shape), &recorded); err != nil || recorded.Image == "" {
		t.Fatalf("the Sandbox %s's template records the shape %q, which names no image (%v)", name, shape, err)
	}
	annotations[controllerSandboxShapeKey] = strings.ReplaceAll(shape, recorded.Image, image)
	podSpec, _ := template["spec"].(map[string]any)
	setContainerImages(podSpec, image)
	a.putLocked("sandboxes", name, sandbox)
	running := kubeClone(pod)
	runningSpec, _ := running["spec"].(map[string]any)
	setContainerImages(runningSpec, image)
	a.putLocked("pods", name, running)
}

// setContainerImages sets the image of every init container and container of podSpec.
func setContainerImages(podSpec map[string]any, image string) {
	for _, key := range []string{"initContainers", "containers"} {
		containers, _ := podSpec[key].([]any)
		for _, c := range containers {
			if container, ok := c.(map[string]any); ok {
				container["image"] = image
			}
		}
	}
}
