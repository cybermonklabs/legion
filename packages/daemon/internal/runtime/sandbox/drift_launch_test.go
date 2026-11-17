package sandbox

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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
// A launch that installs a stale-plugin pair into such a pod (spec.Plugins, LEGION-164) reads the
// image of its shape alone (podImageDrift). A pod built on another image is replaced: the install
// reaches neither the legion launcher that runs as the pod's process 1 nor its Oh My Pi, so the
// launch's agent is started plain in a fresh pod on the configured image, whose own plugins are
// already current, and nothing is installed. A pod whose reservation alone moved is kept, as it
// always was, and the pair is installed into it: the install, not a fresh pod, is what brings that
// agent onto the current plugin. And a daemon restarted on the configuration its pod was built
// from replaces nothing — the failure that would matter most here, since it would rebuild the
// controller's pod at every launch of it.
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
		"an installing launch into a pod on another image": {func(o *Options) { o.Image = nextImage }, plugins, true},
		"an installing launch into a pod whose reservation alone moved": {movedControllerReservation, plugins, false},
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
			if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) > 0 {
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
			switch want := r2.podShapes[controllerPod{}]; {
			case tc.replaced && shape() != want:
				t.Errorf("the controller's new pod is built from %s, want the configuration as it now reads, %s", shape(), want)
			case !tc.replaced && shape() != built:
				t.Errorf("the kept pod's template records %s, want the shape that pod was built from, %s", shape(), built)
			}
			if tc.replaced {
				// The launch itself ended the pod it found: the Sandbox names that pod as the one
				// the daemon replaced, the pod of the Sandbox's name is now another, and the claim
				// runs in that one, on the configured image.
				if named := g.sandbox(loc.Sandbox.Name).Annotations[displacedAnnotation]; named != loc.Sandbox.PodUID {
					t.Errorf("the Sandbox names %q as the pod the daemon replaced, want the controller's old pod %s", named, loc.Sandbox.PodUID)
				}
				pod := g.pod(loc.Sandbox.Name)
				if pod == nil {
					t.Fatalf("the Sandbox holds no pod, want the new pod %s the controller was resumed in", next.Sandbox.PodUID)
				}
				if string(pod.UID) != next.Sandbox.PodUID {
					t.Fatalf("the Sandbox holds pod %s, want the new pod %s the controller was resumed in, the old %s gone",
						pod.UID, next.Sandbox.PodUID, loc.Sandbox.PodUID)
				}
				if other, drifted := imageDrift(pod, r2.image); drifted {
					t.Errorf("the controller's new pod has a container on image %s, want every container on the configured %s", other, r2.image)
				}
			}
			if tc.plugins != nil {
				// A kept pod's installing launch starts through the install wrapper; a replaced
				// pod's starts the agent plain, since its fresh copies are already current.
				start := lastStart(t, g, controllerToken)
				if start.Generation != spec.Generation {
					t.Fatalf("the controller's last start is generation %d, want this launch's %d", start.Generation, spec.Generation)
				}
				i := slices.Index(start.Argv, "--")
				if i < 0 || i+1 >= len(start.Argv) {
					t.Fatalf("the installing launch's argv %q names no command after --", start.Argv)
				}
				switch wrapped := start.Argv[i+1] == "/bin/sh"; {
				case tc.replaced && wrapped:
					t.Errorf("the controller's pod replaced for its image was started through the install wrapper, want the plain agent: argv %q", start.Argv)
				case !tc.replaced && !wrapped:
					t.Errorf("the controller's kept pod was started plain, want the install wrapper: argv %q", start.Argv)
				}
			}
		})
	}
}

// The same five keys over a pod a round does cover: an issue pod whose shape has moved is kept by
// every launch into it, whatever moved, and the round PodDrift names the reasons of is what
// replaces it. This is the direction that must never break — replacing an issue pod where it
// stands ends every role's turn in it and deletes the disk they work on, with everything unpushed
// (INFRA-484) — and the four keys beside the image reach a launch through nothing but shapeDrift,
// which a pod a round covers is never read by (relaunch, roundCovers). A launch installing a
// stale-plugin pair into an issue pod on another image keeps it too, and installs the pair into
// it, where the same launch into the controller's pod replaces that pod (podImageDrift,
// TestALaunchReplacesAPodNoRoundCoversWhoseShapeHasDrifted).
func TestALaunchKeepsAnIssuePodWhateverOfItsShapeMoved(t *testing.T) {
	plugins := &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
	for name, tc := range map[string]struct {
		now     func(*Options)
		plugins *runtime.PluginInstall
	}{
		"another image":                                    {func(o *Options) { o.Image = nextImage }, nil},
		"another reservation":                              {movedReservation, nil},
		"an operator volume added":                         {withOperatorVolume, nil},
		"an operator mount added":                          {withOperatorMount, nil},
		"another disk size":                                {func(o *Options) { o.IssueVolume = resource.MustParse("40Gi") }, nil},
		"an installing launch into a pod on another image": {func(o *Options) { o.Image = nextImage }, plugins},
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
			if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) == 0 {
				t.Fatalf("PodDrift of the issue pod is empty, so this case proves nothing: its round is what replaces it")
			}
			spec.Generation, spec.BootToken, spec.ResumeSessionFile, spec.Plugins = 2, "boot-tester-g2", resumeSession, tc.plugins
			next, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume the tester: %v", err)
			}
			if next.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Errorf("Resume replaced the issue pod %s with %s, want it kept for its update round",
					loc.Sandbox.PodUID, next.Sandbox.PodUID)
			}
			if tc.plugins != nil {
				start := lastStart(t, g, workerToken)
				if start.Generation != spec.Generation {
					t.Fatalf("the tester's last start is generation %d, want this launch's %d", start.Generation, spec.Generation)
				}
				if i := slices.Index(start.Argv, "--"); i < 0 || i+1 >= len(start.Argv) || start.Argv[i+1] != "/bin/sh" {
					t.Errorf("the installing launch into the kept issue pod started %q, want the install wrapper", start.Argv)
				}
			}
		})
	}
}

// An installing launch into the controller's pod reads one key of that pod's shape alone, the
// image it was built from (podImageDrift), and reads it as the data the shape holds: from the
// recorded shape where the template records one, and from the template's own containers where it
// records none, as a Sandbox written before shapes were recorded is compared (templateImageDrift).
// A shape whose other keys alone moved has drifted (podShapeDrift), but not in its image. A
// recorded shape this daemon cannot read names no image, so it answers no image drift either:
// what the launch does with such a pod is keptBy's, which cannot read that record and replaces
// the pod.
func TestPodImageDriftReadsTheImageAPodWasBuiltFrom(t *testing.T) {
	shapeOn := func(image, cpu string) string {
		t.Helper()
		encoded, err := json.Marshal(podShapeRecord{
			Image:     image,
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
			Disk:      "20Gi",
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	want := shapeOn(testImage, "1")
	recorded := func(held string) *sandbox {
		return &sandbox{Spec: sandboxSpec{PodTemplate: podTemplate{
			Metadata: podMetadata{Annotations: map[string]string{podShapeAnnotation: held}},
		}}}
	}
	unrecorded := func(image string) *sandbox {
		return &sandbox{Spec: sandboxSpec{PodTemplate: podTemplate{Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: initContainer, Image: testImage}},
			Containers:     []corev1.Container{{Name: runtime.IssuePodContainer, Image: image}},
		}}}}
	}
	for name, tc := range map[string]struct {
		s     *sandbox
		image bool // whether podImageDrift answers that the pod was built on another image
		shape bool // whether podShapeDrift names a drift, which a launch installing nothing replaces the pod for
	}{
		"the shape a pod is built from now":                  {recorded(want), false, false},
		"a recorded shape on another image":                  {recorded(shapeOn(nextImage, "1")), true, true},
		"a recorded shape whose reservation alone moved":     {recorded(shapeOn(testImage, "5")), false, true},
		"no recorded shape and a template on another image":  {unrecorded(nextImage), true, true},
		"no recorded shape and a template on the same image": {unrecorded(testImage), false, false},
		"a recorded shape this daemon cannot read":           {recorded("{not json"), false, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := podImageDrift(tc.s, want); got != tc.image {
				t.Errorf("podImageDrift = %t, want %t", got, tc.image)
			}
			if got := len(podShapeDrift(tc.s, want)) > 0; got != tc.shape {
				t.Errorf("podShapeDrift names a drift: %t, want %t, so the case is not the one its name says", got, tc.shape)
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
// round waits for every claim of its pod with a session (daemon's blockingClaims). The round that
// would have replaced the pod is therefore blocked by the pod it is replacing — a resume or a
// relaunch starts a claim into that pod while the round is open — and only an operator drain ends
// it, which is the outage this issue exists to end.
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
			if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) == 0 {
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
