package sandbox

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// movedControllerReservation is the controller pod's reservation with another CPU request, the
// kind's own and nothing the daemon derives (movedReservation is the issue pod's).
func movedControllerReservation(o *Options) {
	resources := reservations()
	resources.Controller.Requests[corev1.ResourceCPU] = resource.MustParse("5")
	resources.Controller.Limits[corev1.ResourceCPU] = resource.MustParse("5")
	o.Resources = resources
}

// A pod no round covers (roundCovers) is replaced by its next launch for every kind of drift, as
// every launch did before the round existed. Every facet of the pod itself but the account reaches
// that launch as a moved address (movedInPod,
// TestTheControllersPodIsNotLeftBehindByAConfigurationItCannotCarry); the kinds of drift that do
// not are the account, which the launch reads itself (accountDrift,
// TestALaunchKeepsAnAccountDriftedPodARoundCoversAndReplacesOneNoRoundCovers), and the shape its
// template was built from — the image, the kind's own reservation, an operator volume or mount,
// and the disk size, each key of podShapeRecord — which
// the launch reads through the same one computation the round's reasons are named from
// (shapeDrift). Nothing else would: PodDrift answers nothing for such a pod, so no round is ever
// opened over it, and a launch that kept it would leave the project controller running for ever in
// a pod the deployment has moved away from, on the image, reservation, volumes, mounts and disk it
// was created with.
//
// A launch that installs a stale-plugin pair into such a pod keeps it, as it always did
// (spec.Plugins, LEGION-164): the install, not a fresh pod on the configured image, is what brings
// that agent onto the current plugin. And a daemon restarted on the configuration its pod was
// built from replaces nothing — the failure that would matter most here, since it would rebuild
// the controller's pod at every launch of it.
func TestALaunchReplacesAPodNoRoundCoversWhoseShapeHasDrifted(t *testing.T) {
	plugins := &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
	for name, tc := range map[string]struct {
		now      func(*Options)
		plugins  *runtime.PluginInstall
		replaced bool
	}{
		"the same configuration":                           {func(*Options) {}, nil, false},
		"another image":                                    {func(o *Options) { o.Image = nextImage }, nil, true},
		"another reservation":                              {movedControllerReservation, nil, true},
		"an operator volume added":                         {withOperatorVolume, nil, true},
		"an operator mount added":                          {withOperatorMount, nil, true},
		"another disk size":                                {func(o *Options) { o.IssueVolume = resource.MustParse("40Gi") }, nil, true},
		"an installing launch into a pod on another image": {func(o *Options) { o.Image = nextImage }, plugins, false},
		"another leak check manifest digest": {func(o *Options) {
			o.LeakCheckSetting = leakguard.Setting{Repository: testLeakCheck.Repository, ManifestSHA256: strings.Repeat("7c", 32)}
		}, nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := controllerSpec(t)
			loc := g.spawn(spec)
			r2 := secondRuntime(t, g, tc.now)()
			g.connectRunning(controllerToken, loc.Sandbox.Generation)
			g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			if drifted := driftOf(t, g, r2, loc); len(drifted) > 0 {
				t.Fatalf("PodDrift = %q of a pod no round covers, want nothing: its launch is what replaces it", drifted)
			}
			shape := func() string {
				return g.sandbox(loc.Sandbox.Name).Spec.PodTemplate.Metadata.Annotations[podShapeAnnotation]
			}
			built := shape()
			spec.Generation, spec.BootToken, spec.ResumeSessionFile, spec.Plugins = 2, "boot-controller-g2", controllerSession, tc.plugins
			next, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume the controller: %v", err)
			}
			if replaced := next.Sandbox.PodUID != loc.Sandbox.PodUID; replaced != tc.replaced {
				t.Fatalf("Resume replaced the controller's pod: %t, want %t (pod %s, then %s)",
					replaced, tc.replaced, loc.Sandbox.PodUID, next.Sandbox.PodUID)
			}
			switch want := r2.podShapes[controllerInGVisor]; {
			case tc.replaced && shape() != want:
				t.Errorf("the controller's new pod is built from %s, want the configuration as it now reads, %s", shape(), want)
			case !tc.replaced && shape() != built:
				t.Errorf("the kept pod's template records %s, want the shape that pod was built from, %s", shape(), built)
			}
		})
	}
}

// The same five keys over a pod a round does cover: an issue pod whose shape has moved is kept by
// every launch into it, whatever moved, and the round PodDrift names the reasons of is what
// replaces it. This is the direction that must never break — replacing an issue pod where it
// stands ends every role's turn in it and deletes the disk they work on, with everything unpushed
// (INFRA-484) — and the four keys beside the image reach a launch through nothing but shapeDrift,
// which a pod a round covers is never read by (relaunch, roundCovers).
func TestALaunchKeepsAnIssuePodWhateverOfItsShapeMoved(t *testing.T) {
	for name, now := range map[string]func(*Options){
		"another image":            func(o *Options) { o.Image = nextImage },
		"another reservation":      movedReservation,
		"an operator volume added": withOperatorVolume,
		"an operator mount added":  withOperatorMount,
		"another disk size":        func(o *Options) { o.IssueVolume = resource.MustParse("40Gi") },
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := workerSpec(t)
			loc := g.spawn(spec)
			r2 := secondRuntime(t, g, now)()
			g.connectRunning(workerToken, loc.Sandbox.Generation)
			g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			if drifted := driftOf(t, g, r2, loc); len(drifted) == 0 {
				t.Fatalf("PodDrift of the issue pod is empty, so this case proves nothing: its round is what replaces it")
			}
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
			next, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume the tester: %v", err)
			}
			if next.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Errorf("Resume replaced the issue pod %s with %s, want it kept for its update round",
					loc.Sandbox.PodUID, next.Sandbox.PodUID)
			}
		})
	}
}

// A launch into a live drifted pod keeps the pod (relaunch) and starts the role in it. Two of the
// drift facets that reach a round are configuration the role's own start command is built from,
// not only configuration the pod carries: the secrets broker's enrollment, which puts the shim's
// three agent-secrets flags in the start (launcher.go, launcherCommand), and a named role's
// overlay, which puts the file in the generation's PI_CONFIG_FILES (roleOperatorEnv). A pod
// created before either moved carries neither the token volume and key directory those flags name
// nor the mount that overlay file is under, so the role is started against a pod it is not in:
// `legion worker-shim` stats --agent-secrets-key-dir and --pod-token-file and exits 1 when either
// is absent (shim.ReadAgentSecretsFlags), and Oh My Pi refuses to start on a PI_CONFIG_FILES entry
// that is not there.
//
// That process never registers, so its claim never answers the round it was started for, and a
// round waits for every claim of its pod with a session that it asks, a launching claim among
// them (daemon's sessionClaims, roundAsks). The round that would have replaced the pod is
// therefore blocked by the pod it is replacing — a resume or a relaunch starts a claim into that
// pod while the round is open — and only an operator drain ends it, which is the outage this
// issue exists to end.
//
// The launch must therefore not start a role against configuration its pod cannot carry: either it
// starts the role as the pod it is in was built (no new flag, no new overlay), or it does not
// start that role in that pod at all.
func TestARoleStartedInAKeptDriftedPodIsStartedAsItsPodWasBuilt(t *testing.T) {
	const overlay = "/etc/legion-operator-roles/fallback.yml"
	roles := corev1.Volume{Name: "operator-roles", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator-roles"},
	}}}
	for name, tc := range map[string]struct {
		now func(*Options)
		// check reports what the start names that the pod the role runs in cannot carry.
		check func(t *testing.T, start map[string]string, argv []string, pod *corev1.Pod)
	}{
		"the secrets broker's enrollment": {
			now: func(o *Options) {
				o.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
			},
			check: func(t *testing.T, _ map[string]string, argv []string, pod *corev1.Pod) {
				t.Helper()
				if !slices.Contains(argv, "--agent-secrets-key-dir") {
					return
				}
				held := func(name string) bool {
					return slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == name })
				}
				if !held(agentSecretsTokenVolume) || !held(roleVolume(agentSecretsKeyVolume, claim.RoleTester)) {
					t.Errorf("the start names the shim's agent-secrets flags %q, which stat their paths, while the pod it runs in carries neither %s nor %s",
						argv[slices.Index(argv, "--agent-secrets-key-dir"):slices.Index(argv, "--agent-secrets-bin")+2], agentSecretsTokenVolume, roleVolume(agentSecretsKeyVolume, claim.RoleTester))
				}
			},
		},
		"a named role's overlay": {
			now: func(o *Options) {
				o.Pod.Volumes = append(o.Pod.Volumes, roles)
				o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, corev1.VolumeMount{Name: roles.Name, MountPath: "/etc/legion-operator-roles", ReadOnly: true})
				o.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {overlay}}
			},
			check: func(t *testing.T, env map[string]string, _ []string, pod *corev1.Pod) {
				t.Helper()
				if !strings.Contains(env["PI_CONFIG_FILES"], overlay) {
					return
				}
				if !slices.ContainsFunc(containerNamed(t, pod.Spec, workerContainer).VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == roles.Name }) {
					t.Errorf("the start's PI_CONFIG_FILES is %q, on which Oh My Pi refuses to start, while the pod it runs in does not mount %s",
						env["PI_CONFIG_FILES"], roles.Name)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := workerSpec(t)
			loc := g.spawn(spec)
			r2 := secondRuntime(t, g, tc.now)()
			g.connectRunning(workerToken, loc.Sandbox.Generation)
			g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			if drifted := driftOf(t, g, r2, loc); len(drifted) == 0 {
				t.Fatalf("the pod has drifted from nothing, so this case proves nothing")
			}
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
			kept, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Fatalf("Resume replaced the drifted pod; this case is about the pod a launch keeps")
			}
			start := lastStart(t, g, workerToken)
			tc.check(t, startEnv(start), start.Argv, g.pod(loc.Sandbox.Name))
		})
	}
}

// The project controller's pod runs in gVisor whatever the sandbox block says, so the shape it is
// built from and compared with is the one a deployment naming no block gives it, byte for byte,
// under every block. No round covers that pod, so its shape is what tells its next launch to
// replace it (relaunch, shapeDrift): a sandbox change that moved it would rebuild the controller's
// pod for nothing at its next launch, so a launch after the project moves to microvm keeps it.
func TestTheControllerPodsShapeDoesNotMoveWithTheSandbox(t *testing.T) {
	plain, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := plain.podShapes[controllerInGVisor]
	for name, block := range map[string]func(*Options){
		"gvisor beside a microvm block":   sandboxBlock(SandboxGVisor),
		"gvisor with a canary":            sandboxBlock(SandboxGVisor, testTree),
		"a microVM project":               sandboxBlock(SandboxMicroVM),
		"a microVM project with a canary": sandboxBlock(SandboxMicroVM, testTree),
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions()
			block(&opts)
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := r.podShapes[controllerInGVisor]; got != want {
				t.Errorf("the controller's pod is built from\n%s\nwant the shape a deployment naming no sandbox block gives it\n%s", got, want)
			}
			if got := r.podTemplate(launchOf(t, r, controllerSpec(t))).Metadata.Annotations[podShapeAnnotation]; got != want {
				t.Errorf("the controller's pod template records\n%s\nwant\n%s", got, want)
			}
		})
	}
	t.Run("its launch after the project moves to microvm keeps it", func(t *testing.T) {
		g := newRig(t, nil)
		spec := controllerSpec(t)
		loc := g.spawn(spec)
		r2 := secondRuntime(t, g, sandboxBlock(SandboxMicroVM))()
		g.connectRunning(controllerToken, loc.Sandbox.Generation)
		g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
			state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
			return connected && state.Child != nil
		})
		spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-controller-g2", controllerSession
		next, err := r2.Resume(g.ctx, &loc, spec)
		if err != nil {
			t.Fatalf("Resume the controller: %v", err)
		}
		if next.Sandbox.PodUID != loc.Sandbox.PodUID {
			t.Errorf("Resume replaced the controller's pod %s with %s, want it kept: its sandbox is gVisor under any block",
				loc.Sandbox.PodUID, next.Sandbox.PodUID)
		}
	})
}

// A covered issue pod built in gVisor is kept by a launch after its issue goes on the canary list
// (relaunch, roundCovers), and the role that launch starts in it is started as that pod was built:
// by the very start command a configuration that never named the canary sends into that pod.
// Nothing the daemon sends a generation is built from the sandbox — the launcher's --microvm and
// metadata flags and the projected token are the pod's own, fixed when it was created
// (podKind.launcherContainer, workloadIdentityVolumes), and the metadata endpoint's variables are
// the launcher's to set — so a start into the kept gVisor pod names nothing of a microVM. Nor does
// the launch write one onto the pod: it patches no template, so the Sandbox still records the
// gVisor pod it runs until the update round PodDrift opens over it replaces that pod.
func TestARoleStartedInAKeptPodWhoseSandboxMovedIsStartedAsItsPodWasBuilt(t *testing.T) {
	spec := workerSpec(t)
	// resumed is a resume of the tester at generation 2 into its issue pod, built under no sandbox
	// block, by a daemon restarted with move: what PodDrift read of the pod first, the start
	// command the resume sent, the writes it made, and the Sandbox it left.
	resumed := func(move func(*Options)) (drifted []string, start shimwire.LauncherStart, writes []action, s *sandbox) {
		t.Helper()
		g := newRig(t, nil)
		loc := g.spawn(spec)
		r2 := restartedOver(t, g, loc, move)
		drifted = driftOf(t, g, r2, loc)
		g.clearActions()
		next := spec
		next.Generation, next.BootToken, next.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
		kept, err := r2.Resume(g.ctx, &loc, next)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
			t.Fatalf("Resume replaced the tester's pod %s with %s, want it kept for its update round", loc.Sandbox.PodUID, kept.Sandbox.PodUID)
		}
		return drifted, startSent(t, g, 2), g.writes(), g.sandbox(loc.Sandbox.Name)
	}
	_, built, _, unmoved := resumed(func(*Options) {})
	drifted, start, writes, s := resumed(sandboxBlock(SandboxGVisor, testTree))
	if want := "sandbox null, now " + testMicroVMSandboxShape; !slices.Equal(drifted, []string{want}) {
		t.Fatalf("PodDrift = %q, want %q alone, or this case proves nothing", drifted, want)
	}
	if !reflect.DeepEqual(start, built) {
		t.Errorf("the start into the kept gVisor pod is\n%+v\nwant the one a configuration naming no canary sends\n%+v", start, built)
	}
	for _, write := range writes {
		if write.resource == "sandboxes" && strings.Contains(write.patch, "/spec/podTemplate") {
			t.Errorf("the launch into the kept pod patched its Sandbox's template: %s", write.patch)
		}
	}
	if !reflect.DeepEqual(s.Spec.PodTemplate, unmoved.Spec.PodTemplate) {
		t.Errorf("the kept pod's Sandbox records the template\n%+v\nwant the gVisor one it was built from\n%+v", s.Spec.PodTemplate, unmoved.Spec.PodTemplate)
	}
}
