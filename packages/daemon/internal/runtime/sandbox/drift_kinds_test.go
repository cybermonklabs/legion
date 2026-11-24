package sandbox

// The drift read gives each reason with the kind of the facet it read (runtime.DriftKind), so the
// daemon's round decision reads kinds and never phrases: a pod whose drift only drains keeps
// working however long, and its next pod after a release takes the configured shape
// (dispatch://INFRA-4754 R1, R3, R4, R5).

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// shippedContracts are the contracts this change ships, which a test of the shipped floor runs
// under.
var shippedContracts = Contracts{Daemon: api.DaemonAPIVersion, Floor: api.DaemonAPIFloor, FirstFloor: api.DaemonAPIFirstFloor}

// testContracts are what every rig's runtime runs under unless a test sets its own (testOptions):
// the shipped floor, under a daemon contract of the tests' own, so a pod a rig makes records a
// number no bump of DaemonAPIVersion moves, and the manifest goldens stay what they pin.
var testContracts = Contracts{Daemon: 300, Floor: api.DaemonAPIFloor, FirstFloor: api.DaemonAPIFirstFloor}

// facetCase is one facet of the four drift sources, the recorded shape that cannot be read, or
// the floor, moved alone: the configuration the pod is built under (before), the one the restarted
// daemon runs (now, or before where now is nil), and an edit of the stored objects after the restart.
type facetCase struct {
	before, now func(*Options)
	edit        func(*testing.T, *rig, *Runtime, runtime.Locator)
	kind        runtime.DriftKind
}

// facetCases is every facet a pod's drift can name, each by the one kind it gives.
func facetCases() map[string]facetCase {
	const overlay = "/etc/legion-operator-extra/fallback.yml"
	enrolled := func(o *Options) {
		o.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	}
	return map[string]facetCase{
		"another image":            {now: func(o *Options) { o.Image = nextImage }, kind: runtime.DriftImage},
		"another reservation":      {now: movedReservation, kind: runtime.DriftResources},
		"an operator volume added": {now: withOperatorVolume, kind: runtime.DriftVolumes},
		"an operator mount added":  {before: withOperatorVolume, now: withOperatorMount, kind: runtime.DriftMounts},
		"another disk size":        {now: func(o *Options) { o.IssueVolume = resource.MustParse("40Gi") }, kind: runtime.DriftDisk},
		"another sandbox":          {now: sandboxBlock(SandboxMicroVM), kind: runtime.DriftSandbox},
		"a shape it cannot read": {edit: func(t *testing.T, g *rig, r *Runtime, loc runtime.Locator) {
			holdPodShape(t, g, r, loc.Sandbox.Name, "{not a shape")
		}, kind: runtime.DriftUnreadableShape},
		"another leak check digest": {now: func(o *Options) {
			o.LeakCheckSetting = leakguard.Setting{Repository: testLeakCheck.Repository, ManifestSHA256: strings.Repeat("7c", 32)}
		}, kind: runtime.DriftLeakCheck},
		"another account": {now: func(o *Options) { o.Pod.ServiceAccount = otherAccount }, kind: runtime.DriftAccount},
		"a role's overlays": {before: func(o *Options) {
			withOperatorMount(o)
			o.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {overlay}}
		}, edit: func(t *testing.T, g *rig, r *Runtime, loc runtime.Locator) {
			// The pod's container lacks the mount that holds the overlay, while its recorded shape
			// still says it was built with it: the overlay facet alone moves.
			name := loc.Sandbox.Name
			g.update(g.pod(name), func(pod *corev1.Pod) {
				for i := range pod.Spec.Containers {
					pod.Spec.Containers[i].VolumeMounts = slices.DeleteFunc(pod.Spec.Containers[i].VolumeMounts,
						func(m corev1.VolumeMount) bool { return m.Name == operatorMount.Name })
				}
			})
			g.eventually("the pod store to see "+name+" without the overlay's mount", func() bool {
				pod := r.storedPod(name)
				return pod != nil && !slices.ContainsFunc(pod.Spec.Containers, func(c corev1.Container) bool {
					return slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == operatorMount.Name })
				})
			})
		}, kind: runtime.DriftRoleOverlays},
		"the broker's enrollment": {now: enrolled, kind: runtime.DriftEnrollment},
		"the exchange identity": {before: enrolled, now: func(o *Options) {
			o.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour,
				ExchangeAudience: testExchangeAudience}
		}, kind: runtime.DriftExchangeIdentity},
		"the session store": {now: func(o *Options) { o.SessionStore = cloudSQLStore }, kind: runtime.DriftSessionStore},
		"an owner it lacks": {now: servingAddedOwner, kind: runtime.DriftGhOwner},
		"a repository directory": {edit: func(t *testing.T, g *rig, r *Runtime, loc runtime.Locator) {
			projectARepositoryDirectory(t, g, r, loc.Sandbox.Name)
		}, kind: runtime.DriftGhRepositoryDir},
		"a gh projection with no pairs file": {edit: func(t *testing.T, g *rig, r *Runtime, loc runtime.Locator) {
			projectGhItems(t, g, r, loc.Sandbox.Name, ownerRoutedItems(staticDirs...))
		}, kind: runtime.DriftGhNoPairs},
		"a contract below the floor": {now: func(o *Options) {
			o.Contracts = Contracts{Daemon: testContracts.Daemon + 20, Floor: testContracts.Daemon + 10, FirstFloor: api.DaemonAPIFirstFloor}
		}, kind: runtime.DriftFloor},
	}
}

// staged is the rig over a tester's pod built under tc.before, the daemon restarted under tc.now
// (which carries whatever of tc.before it keeps), or under tc.before where tc.now is nil, with
// that tester's launcher reporting its child to it, and tc.edit made.
func (tc facetCase) staged(t *testing.T, options ...rigOption) (*rig, *Runtime, runtime.Locator) {
	t.Helper()
	before, now := tc.before, tc.now
	if before == nil {
		before = func(*Options) {}
	}
	if now == nil {
		now = before
	}
	g := newRig(t, nil, append([]rigOption{withOptions(before)}, options...)...)
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, now)
	if tc.edit != nil {
		tc.edit(t, g, r2, loc)
	}
	return g, r2, loc
}

// For each facet of the four sources, the recorded shape that cannot be read and the floor, a pod
// drifted on that facet alone gives reasons of that facet's own kind alone, each with its phrase.
func TestPodDriftGivesEachKind(t *testing.T) {
	for name, tc := range facetCases() {
		t.Run(name, func(t *testing.T) {
			g, r2, loc := tc.staged(t)
			drifted := driftReasonsOf(t, g, r2, loc)
			if got := drifted.Kinds(); !slices.Equal(got, []runtime.DriftKind{tc.kind}) {
				t.Fatalf("PodDrift = %+v, want reasons of the one kind %q", drifted, tc.kind)
			}
			for _, reason := range drifted {
				if reason.Text == "" {
					t.Errorf("PodDrift gave a reason of kind %q with no phrase", reason.Kind)
				}
			}
		})
	}
}

// ageBy rewrites the stored pod of name as created age before the rig's clock, and waits for r's
// store to hold it.
func ageBy(t *testing.T, g *rig, r *Runtime, name string, age time.Duration) {
	t.Helper()
	created := g.now.Load().Add(-age)
	g.update(g.pod(name), func(pod *corev1.Pod) { pod.CreationTimestamp = metav1.NewTime(created) })
	g.eventually("the pod store to see "+name+" created "+age.String()+" ago", func() bool {
		pod := r.storedPod(name)
		return pod != nil && pod.CreationTimestamp.Time.Equal(created)
	})
}

// For each kind that drains, a role started in a live issue pod 30 days old, drifted on that kind
// alone, while another role of that pod runs, is started in that pod (the same uid), and the role
// running there is not displaced: neither the pod's age nor drift that drains replaces it.
func TestAStartKeepsAnIssuePodWithDrainingDrift(t *testing.T) {
	for name, tc := range facetCases() {
		if !tc.kind.Drains() {
			continue
		}
		t.Run(name, func(t *testing.T) {
			g, r2, loc := tc.staged(t)
			ageBy(t, g, r2, loc.Sandbox.Name, 30*24*time.Hour)
			if kinds := driftReasonsOf(t, g, r2, loc).Kinds(); !slices.Contains(kinds, tc.kind) {
				t.Fatalf("PodDrift names kinds %v, want %q among them, or this case proves nothing", kinds, tc.kind)
			}
			reviewer := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
			if reviewer.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Fatalf("the reviewer started in pod %s, want the tester's pod %s kept", reviewer.Sandbox.PodUID, loc.Sandbox.PodUID)
			}
			if state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID); !connected || state.Child == nil ||
				state.Child.Generation != loc.Sandbox.Generation {
				t.Errorf("the tester's launcher reports %+v (connected %t), want its generation %d still running", state, connected, loc.Sandbox.Generation)
			}
			observed := readopt(t, g.ctx, r2, loc, reviewer)
			i := slices.IndexFunc(observed, func(o runtime.Observation) bool { return o.Locator.Claim == workerToken })
			if i < 0 || observed[i].Kind != runtime.Alive || observed[i].Locator != loc {
				t.Errorf("the tester reads %+v, want it alive at its locator %+v", observed, loc)
			}
		})
	}
}

// After the idle release of a pod drifted on its image and its volumes, the issue's next launch
// makes a pod whose recorded shape is the configured one, on an ephemeral disk, whose release
// deletes the Sandbox, and on a microVM's kept volume, whose release keeps it.
func TestTheNextPodAfterAReleaseTakesTheConfiguredShape(t *testing.T) {
	for name, tc := range map[string]struct {
		block func(*Options)
		key   podShapeKey
	}{
		"an ephemeral disk":       {func(*Options) {}, issueInGVisor},
		"a microVM's kept volume": {keptVolumes(SandboxMicroVM), issueInMicroVM},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil, withOptions(tc.block))
			spec := workerSpec(t)
			loc := g.spawn(spec)
			r2 := restartedOver(t, g, loc, func(o *Options) {
				tc.block(o)
				o.Image = nextImage
				withOperatorVolume(o)
			})
			if kinds := driftReasonsOf(t, g, r2, loc).Kinds(); !slices.Contains(kinds, runtime.DriftImage) || !slices.Contains(kinds, runtime.DriftVolumes) {
				t.Fatalf("PodDrift names kinds %v, want the image and the volumes, or this case proves nothing", kinds)
			}
			if err := r2.ReleaseIssue(g.ctx, testTree, testTree, authorized); err != nil {
				t.Fatalf("ReleaseIssue: %v", err)
			}
			next := g.relaunchOnNewPod(loc, &spec)
			if shape, want := g.sandbox(next.Sandbox.Name).Spec.PodTemplate.Metadata.Annotations[podShapeAnnotation], r2.podShapes[tc.key]; shape != want {
				t.Errorf("the next pod records the shape\n%s\nwant the configured one\n%s", shape, want)
			}
			pod := g.pod(next.Sandbox.Name)
			if pod == nil || string(pod.UID) != next.Sandbox.PodUID {
				t.Fatalf("the next pod %s is not in the tracker", next.Sandbox.PodUID)
			}
			if other, drifted := imageOtherThan(&pod.Spec, nextImage); drifted {
				t.Errorf("a container of the next pod runs %s, want the configured %s", other, nextImage)
			}
			if !slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == operatorVolume.Name }) {
				t.Errorf("the next pod carries no %s, the configured operator volume", operatorVolume.Name)
			}
		})
	}
}

// A launch that installs the daemon's plugin pair into a live issue pod on an older image keeps
// the pod (the same uid) and carries that pair: the stale-plugin lane moves an Oh My Pi claim onto
// the daemon's plugins in its pod, however old the image the pod runs.
func TestAnInstallingLaunchKeepsAPodOnAnOlderImage(t *testing.T) {
	plugins := &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	r2 := restartedOver(t, g, loc, func(o *Options) { o.Image = nextImage })
	if kinds := driftReasonsOf(t, g, r2, loc).Kinds(); !slices.Equal(kinds, []runtime.DriftKind{runtime.DriftImage}) {
		t.Fatalf("PodDrift names kinds %v, want the image alone, or this case proves nothing", kinds)
	}
	spec.Generation, spec.BootToken, spec.ResumeSessionFile, spec.Plugins = 2, "boot-tester-g2", resumeSession, plugins
	next, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume the tester with the plugin pair: %v", err)
	}
	if next.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("the installing launch replaced the pod %s with %s, want it kept", loc.Sandbox.PodUID, next.Sandbox.PodUID)
	}
	start := startSent(t, g, spec.Generation)
	argv := strings.Join(start.Argv, " ")
	for _, want := range []string{"/bin/sh", "@sjawhar/pi-legion@" + plugins.PiLegion, "@sjawhar/pi-envoy@" + plugins.PiEnvoy} {
		if !strings.Contains(argv, want) {
			t.Errorf("the installing launch's argv %q does not carry %q", argv, want)
		}
	}
}

// The drift read of a pod whose creation timestamp is 30 days old, and that has no other drift,
// gives no reason: a pod's age is no drift.
func TestPodDriftOfAnOldPodWithNoOtherDriftIsEmpty(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, func(*Options) {})
	ageBy(t, g, r2, loc.Sandbox.Name, 30*24*time.Hour)
	if drifted := driftReasonsOf(t, g, r2, loc); len(drifted) > 0 {
		t.Errorf("PodDrift of a pod 30 days old with nothing else moved = %+v, want nothing", drifted)
	}
}

// holdContract sets the contract the stored Sandbox's pod template records its maker at, or drops
// the record where contract is empty, and waits for the store r reads to hold it.
func holdContract(t *testing.T, g *rig, r *Runtime, name, contract string) {
	t.Helper()
	s := g.sandbox(name)
	if s == nil {
		t.Fatalf("sandbox %s is absent", name)
	}
	annotations := map[string]string{}
	for key, value := range s.Spec.PodTemplate.Metadata.Annotations {
		if key != contractAnnotation {
			annotations[key] = value
		}
	}
	if contract != "" {
		annotations[contractAnnotation] = contract
	}
	s.Spec.PodTemplate.Metadata.Annotations = annotations
	u, err := encodeSandbox(*s)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.dyn.Tracker().Update(sandboxGVR, u, testNamespace); err != nil {
		t.Fatal(err)
	}
	g.eventually("the sandbox store to see "+name+" recording the contract "+contract, func() bool {
		stored, err := r.storedSandbox(name)
		if err != nil || stored == nil {
			return false
		}
		held, recorded := stored.Spec.PodTemplate.Metadata.Annotations[contractAnnotation]
		return held == contract && recorded == (contract != "")
	})
}

// floorReasons is every reason of the floor's kind in drifted.
func floorReasons(drifted runtime.DriftReasons) runtime.DriftReasons {
	return slices.DeleteFunc(slices.Clone(drifted), func(r runtime.DriftReason) bool { return r.Kind != runtime.DriftFloor })
}

// recordedAt is a rig over a tester's pod whose Sandbox records its maker at contract ("" for no
// record), read by a daemon restarted under contracts.
func recordedAt(t *testing.T, contract string, contracts Contracts) (*rig, *Runtime, runtime.Locator) {
	t.Helper()
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, func(o *Options) { o.Contracts = contracts })
	holdContract(t, g, r2, loc.Sandbox.Name, contract)
	return g, r2, loc
}

// A new issue pod's Sandbox records the contract of the daemon that made it.
func TestANewPodRecordsItsMakersContract(t *testing.T) {
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.Contracts = Contracts{Daemon: api.DaemonAPIVersion + 3, Floor: api.DaemonAPIFloor, FirstFloor: api.DaemonAPIFirstFloor}
	}))
	loc := g.spawn(workerSpec(t))
	held, recorded := g.sandbox(loc.Sandbox.Name).Spec.PodTemplate.Metadata.Annotations[contractAnnotation]
	if want := strconv.Itoa(api.DaemonAPIVersion + 3); !recorded || held != want {
		t.Errorf("the new pod's Sandbox records its maker's contract %q (recorded %t), want %q", held, recorded, want)
	}
}

// A pod recorded below the floor gives the floor kind, and its phrase names the floor.
func TestTheFloorKindMarksAPodBelowTheFloor(t *testing.T) {
	const floor = 230
	g, r2, loc := recordedAt(t, "225", Contracts{Daemon: 240, Floor: floor, FirstFloor: api.DaemonAPIFirstFloor})
	got := floorReasons(driftReasonsOf(t, g, r2, loc))
	if len(got) != 1 || !strings.Contains(got[0].Text, "floor "+strconv.Itoa(floor)) {
		t.Errorf("the floor's reasons of a pod recorded at 225 under the floor %d = %+v, want one naming the floor", floor, got)
	}
}

// A pod recorded above the daemon's own contract, as a pin moved back leaves one, gives no floor
// kind.
func TestAPodAboveTheDaemonHasNoFloorKind(t *testing.T) {
	g, r2, loc := recordedAt(t, "250", Contracts{Daemon: 240, Floor: 230, FirstFloor: api.DaemonAPIFirstFloor})
	if got := floorReasons(driftReasonsOf(t, g, r2, loc)); len(got) > 0 {
		t.Errorf("a pod recorded at 250 under a daemon at 240 gives %+v, want no floor kind", got)
	}
}

// A pod-shape record written before this change, which carries no contract, reads as no record:
// it gives neither the floor kind nor the kind of a recorded shape that cannot be read.
func TestAnOldRecordReadsAsNoRecord(t *testing.T) {
	g, r2, loc := recordedAt(t, "", shippedContracts)
	for _, reason := range driftReasonsOf(t, g, r2, loc) {
		if reason.Kind == runtime.DriftFloor || reason.Kind == runtime.DriftUnreadableShape {
			t.Errorf("a pod whose record carries no contract gives %+v, want neither the floor's kind nor the unreadable shape's", reason)
		}
	}
}

// A pod with no record counts as made at the first floor: no floor kind while the floor is the
// first floor, and the floor kind once the floor stands above the first floor.
func TestAPodWithNoRecordCountsAsMadeAtTheFirstFloor(t *testing.T) {
	g, r2, loc := recordedAt(t, "", Contracts{Daemon: 240, Floor: 219, FirstFloor: 219})
	if got := floorReasons(driftReasonsOf(t, g, r2, loc)); len(got) > 0 {
		t.Errorf("a pod with no record under the floor at the first floor gives %+v, want no floor kind", got)
	}
	g, r3, loc := recordedAt(t, "", Contracts{Daemon: 240, Floor: 220, FirstFloor: 219})
	if got := floorReasons(driftReasonsOf(t, g, r3, loc)); len(got) != 1 {
		t.Errorf("a pod with no record under a floor above the first floor gives %+v, want the floor kind", got)
	}
}

// Under the constants this change ships, the floor and the first floor are each the one literal
// number this test writes, and neither DaemonAPIVersion: so a pod with no record and a pod
// recorded at that number give no floor kind.
func TestTheShippedFloorIsTheFirstFloor(t *testing.T) {
	const first = 220
	if api.DaemonAPIFloor != first || api.DaemonAPIFirstFloor != first {
		t.Fatalf("the shipped floor is %d and the first floor %d, want both %d", api.DaemonAPIFloor, api.DaemonAPIFirstFloor, first)
	}
	for _, contract := range []string{"", strconv.Itoa(first)} {
		g, r2, loc := recordedAt(t, contract, shippedContracts)
		if got := floorReasons(driftReasonsOf(t, g, r2, loc)); len(got) > 0 {
			t.Errorf("a pod recorded at %q under the shipped contracts gives %+v, want no floor kind", contract, got)
		}
	}
}

// With the floor below the daemon's contract, a pod recorded at the floor gives no floor kind.
func TestAPodAtTheFloorHasNoFloorKind(t *testing.T) {
	g, r2, loc := recordedAt(t, "230", Contracts{Daemon: 240, Floor: 230, FirstFloor: api.DaemonAPIFirstFloor})
	if got := floorReasons(driftReasonsOf(t, g, r2, loc)); len(got) > 0 {
		t.Errorf("a pod recorded at the floor 230 gives %+v, want no floor kind", got)
	}
}
