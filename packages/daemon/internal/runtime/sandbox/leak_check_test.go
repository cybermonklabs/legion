package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// testLeakCheckTag is the pin a fake launcher's hello names when a test gives it one.
const testLeakCheckTag = "leak-check-v3"

// fakeLeakCheck stands in for the daemon's reader of the shared public leak check
// (leakguard.Source): it answers body at any tag, sliced as the reader slices it, or refuses every
// read with err, and records the tag of each read.
type fakeLeakCheck struct {
	body     []byte
	manifest string
	err      error

	mu   sync.Mutex
	tags []string
}

func (f *fakeLeakCheck) Get(_ context.Context, tag string) (leakguard.Served, error) {
	f.mu.Lock()
	f.tags = append(f.tags, tag)
	f.mu.Unlock()
	if f.err != nil {
		return leakguard.Served{}, f.err
	}
	frames, err := shimwire.SplitLeakCheck(tag, f.body)
	if err != nil {
		return leakguard.Served{}, err
	}
	return leakguard.Served{Tag: tag, Body: f.body, ManifestSHA256: f.manifest, Frames: frames}, nil
}

func (f *fakeLeakCheck) reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tags)
}

// servedLeakCheck is a reader whose packed check is more than one
// shimwire.LauncherSlicePayloadBytes, so the push is a sequence of slices and not one frame.
func servedLeakCheck() *fakeLeakCheck {
	return &fakeLeakCheck{body: bytes.Repeat([]byte("leak-check\x00"), shimwire.LauncherSlicePayloadBytes/11+4096),
		manifest: testLeakCheck.ManifestSHA256}
}

// countOf is how many of frames are of the frame type of want.
func countOf(frames []shimwire.Frame, want string) int {
	n := 0
	for _, frame := range frames {
		if frame.FrameType() == want {
			n++
		}
	}
	return n
}

// frameTypes names what a launcher was sent, for a failure an operator reads.
func frameTypes(frames []shimwire.Frame) []string {
	types := make([]string, 0, len(frames))
	for _, frame := range frames {
		types = append(types, frame.FrameType())
	}
	return types
}

// A launcher whose hello names its pin is sent the check the daemon read at that pin before
// anything else of the start: every slice of it, in order, before the role's start, since the
// launcher installs the check once for the pod and every role's shim refuses to start its agent
// without that install.
func TestALauncherNamingItsPinIsSentTheLeakCheckBeforeTheStart(t *testing.T) {
	t.Parallel()
	source := servedLeakCheck()
	g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, false), withOptions(func(o *Options) { o.LeakCheck = source }))
	spec := workerSpec(t)
	g.spawn(spec)

	frames := g.sent(spec.Claim)
	start := slices.IndexFunc(frames, func(f shimwire.Frame) bool { _, ok := f.(shimwire.LauncherStart); return ok })
	if start < 0 {
		t.Fatalf("the launcher was sent %v, want the check's slices and then the role's start", frameTypes(frames))
	}
	var body []byte
	var sequence string
	for i, frame := range frames[:start] {
		slice, ok := frame.(shimwire.LauncherLeakCheck)
		if !ok {
			t.Fatalf("the launcher was sent %v, want the check's slices before anything else", frameTypes(frames))
		}
		if i == 0 {
			sequence = slice.ID
		}
		if slice.ID != sequence || slice.Index != i || slice.Count != start || slice.Tag != testLeakCheckTag {
			t.Fatalf("slice %d of %d of %s under %q arrived at position %d of the %d sent before the start",
				slice.Index, slice.Count, slice.Tag, slice.ID, i, start)
		}
		body = append(body, slice.Payload...)
	}
	if start < 2 {
		t.Fatalf("the check crossed in %d slice(s), want the sequence of the %d bytes read", start, len(source.body))
	}
	if !bytes.Equal(body, source.body) {
		t.Fatalf("the check crossed as %d bytes, want the %d the daemon read", len(body), len(source.body))
	}
	if got := countOf(frames, shimwire.TypeLauncherLeakCheck); got != start {
		t.Fatalf("the launcher was sent %v: a slice of the check crossed after the role's start", frameTypes(frames))
	}
	if reads := source.reads(); !slices.Equal(reads, []string{testLeakCheckTag}) {
		t.Fatalf("the daemon read the check at %v, want once at the launcher's pin %s", reads, testLeakCheckTag)
	}
}

// A launcher whose hello names no pin takes no check from the daemon — an image older than the
// push, or a pod whose image carries no pin — and its role starts as it always has: the daemon
// reads nothing and sends it no slice.
func TestALauncherNamingNoPinIsSentNoLeakCheckAndStarts(t *testing.T) {
	t.Parallel()
	source := servedLeakCheck()
	g := newRig(t, nil, withOptions(func(o *Options) { o.LeakCheck = source }))
	spec := workerSpec(t)
	loc := g.spawn(spec)
	if loc.Sandbox.Generation != spec.Generation {
		t.Fatalf("the launch left %+v, want generation %d", loc, spec.Generation)
	}
	frames := g.sent(spec.Claim)
	if countOf(frames, shimwire.TypeLauncherLeakCheck) != 0 || countOf(frames, shimwire.TypeLauncherStart) != 1 {
		t.Fatalf("the launcher was sent %v, want the role's start and no slice of the check", frameTypes(frames))
	}
	if reads := source.reads(); len(reads) != 0 {
		t.Fatalf("the daemon read the check at %v for a launcher that names no pin", reads)
	}
}

// A deployment that names no shared leak check (Options.LeakCheckSetting, the zero Setting)
// pushes none, whatever pin a launcher's hello names — the worker image carries its pin whatever
// the deployment runs — and starts the role: the daemon reads nothing, the launcher is sent no
// slice, and the role's start carries neither PUBLIC_LEAK_CHECK nor the checker
// (leakguard.CheckerVar), so its shim reads no install (leakguard.Installed) and its guard refuses
// no write for the check's absence. Under the deployment's own setting the same launcher is pushed
// the check and its role is told both, the checker being the setting's own.
func TestADeploymentThatNamesNoLeakCheckPushesNoneAndTellsItsRolesNone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		setting leakguard.Setting
		pushed  bool
		told    map[string]string
	}{
		{name: "no setting", setting: leakguard.Setting{}, told: map[string]string{}},
		{name: "the deployment's setting", setting: testLeakCheck, pushed: true, told: map[string]string{
			leakguard.CheckVar: leakguard.InstalledCheck, leakguard.CheckerVar: testLeakCheck.ManifestSHA256,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := servedLeakCheck()
			g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, false),
				withOptions(func(o *Options) { o.LeakCheck, o.LeakCheckSetting = source, tc.setting }))
			spec := workerSpec(t)
			loc := g.spawn(spec)
			if loc.Sandbox.Generation != spec.Generation {
				t.Fatalf("the launch left %+v, want generation %d", loc, spec.Generation)
			}
			frames := g.sent(spec.Claim)
			if pushed := countOf(frames, shimwire.TypeLauncherLeakCheck) > 0; pushed != tc.pushed || countOf(frames, shimwire.TypeLauncherStart) != 1 {
				t.Fatalf("the launcher was sent %v, want the role's start with the check pushed = %t", frameTypes(frames), tc.pushed)
			}
			if reads := source.reads(); (len(reads) > 0) != tc.pushed {
				t.Fatalf("the daemon read the check at %v, want a read = %t", reads, tc.pushed)
			}
			told := map[string]string{}
			for _, frame := range frames {
				start, ok := frame.(shimwire.LauncherStart)
				if !ok {
					continue
				}
				for _, pair := range start.Env {
					if name, value, _ := strings.Cut(pair, "="); name == leakguard.CheckVar || name == leakguard.CheckerVar {
						told[name] = value
					}
				}
			}
			if !maps.Equal(told, tc.told) {
				t.Fatalf("the role's start told it %v of the leak check, want %v", told, tc.told)
			}
		})
	}
}

// A pod records the manifest digest it was built under on its template (leakCheckerAnnotation),
// and a role started in that pod after the daemon restarted under another digest — the deployment
// moved its worker image's pin and its digest together, and the pod is kept until its update
// round — is told the digest its pod was built under, which is the one the install of that pod's
// own pin has. Told the digest now, its shim would start no agent, so it would never answer the
// round that is to replace the pod. A pod built under no setting records none: where its launcher
// holds an install from before, its role is told the installed check and no digest; where its
// launcher holds none, the restarted daemon serves its check and tells the manifest of the served
// tarball, so the pushed check is hashed. A pod the launch builds itself is told the digest now
// (TestADeploymentThatNamesNoLeakCheckPushesNoneAndTellsItsRolesNone).
func TestARoleStartedInAKeptPodIsToldTheCheckerItsPodWasBuiltUnder(t *testing.T) {
	t.Parallel()
	moved := leakguard.Setting{Repository: testLeakCheck.Repository, ManifestSHA256: strings.Repeat("7c", 32)}
	for _, tc := range []struct {
		name     string
		built    leakguard.Setting
		held     bool
		recorded string
		pushed   bool
		told     map[string]string
	}{
		{name: "built under the digest before", built: testLeakCheck, held: true, recorded: testLeakCheck.ManifestSHA256,
			told: map[string]string{leakguard.CheckVar: leakguard.InstalledCheck, leakguard.CheckerVar: testLeakCheck.ManifestSHA256}},
		{name: "built under no setting, its launcher holding an install from before", built: leakguard.Setting{}, held: true,
			told: map[string]string{leakguard.CheckVar: leakguard.InstalledCheck}},
		{name: "built under no setting, its launcher holding none", built: leakguard.Setting{}, held: false, pushed: true,
			told: map[string]string{leakguard.CheckVar: leakguard.InstalledCheck, leakguard.CheckerVar: testLeakCheck.ManifestSHA256}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, tc.held),
				withOptions(func(o *Options) { o.LeakCheck, o.LeakCheckSetting = servedLeakCheck(), tc.built }))
			loc := g.spawn(workerSpec(t))
			if got := g.sandbox(loc.Sandbox.Name).Spec.PodTemplate.Metadata.Annotations[leakCheckerAnnotation]; got != tc.recorded {
				t.Fatalf("the pod's template records the digest %q, want %q", got, tc.recorded)
			}
			before := countOf(g.sent(workerToken), shimwire.TypeLauncherLeakCheck)
			r2 := restartedOver(t, g, loc, func(o *Options) {
				o.LeakCheck, o.LeakCheckSetting = servedLeakCheck(), moved
				movedReservation(o)
			})
			if drifted := driftOf(t, g, r2, loc); len(drifted) == 0 {
				t.Fatalf("the pod has drifted from nothing, so this case proves nothing")
			}
			kept := resumeTester(t, g, r2, loc)
			if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Fatalf("Resume replaced the drifted pod; this case is about the pod a launch keeps")
			}
			if pushed := countOf(g.sent(workerToken), shimwire.TypeLauncherLeakCheck) > before; pushed != tc.pushed {
				t.Fatalf("the restarted daemon pushed the check to the kept pod's launcher: %t, want %t", pushed, tc.pushed)
			}
			if told := leakCheckTold(startSent(t, g, 2)); !maps.Equal(told, tc.told) {
				t.Fatalf("the role started in the kept pod was told %v of the leak check, want %v", told, tc.told)
			}
		})
	}
}

// leakCheckTold is what start tells its role of the leak check: the two variables of it it sets.
func leakCheckTold(start shimwire.LauncherStart) map[string]string {
	told := map[string]string{}
	for name, value := range startEnv(start) {
		if name == leakguard.CheckVar || name == leakguard.CheckerVar {
			told[name] = value
		}
	}
	return told
}

// A pod built under no setting records no digest on its template. Once a daemon running the
// setting keeps it and pushes a check into it, the Sandbox records the manifest digest served for
// that pod (leakCheckPushedAnnotation), on its own metadata: the template, the Sandbox's
// generation and the pod are as they were, and the pod has drifted from nothing. Its launcher
// holds that install from then on and says so in the hello of every later connection, and a role
// started after such a reconnect is told the digest pushed, so its guard hashes the check it runs;
// a daemon restarted under another digest reads the record as drift. The control rows are the
// record dropped and a record of another pod: each reads as no record at all, so the later role is
// told no digest and its guard would run the pushed check unhashed, which is what every such role
// was told before the record existed, and the restarted daemon reads no leak check drift.
func TestADigestPushedIntoAPodThatRecordsNoneIsToldToItsLaterRoles(t *testing.T) {
	t.Parallel()
	moved := leakguard.Setting{Repository: testLeakCheck.Repository, ManifestSHA256: strings.Repeat("7c", 32)}
	movedReason := "leak check manifest " + testLeakCheck.ManifestSHA256 + ", now " + moved.ManifestSHA256
	for _, tc := range []struct {
		name string
		// edit changes the Sandbox's own annotations after the push and before the reconnect; nil
		// keeps the record as the push wrote it.
		edit func(annotations map[string]string)
		told map[string]string
		// drifts is whether the daemon restarted under the moved digest reads the record as drift.
		drifts bool
	}{
		{name: "the record the push wrote", drifts: true,
			told: map[string]string{leakguard.CheckVar: leakguard.InstalledCheck, leakguard.CheckerVar: testLeakCheck.ManifestSHA256}},
		{name: "the record dropped", edit: func(a map[string]string) { delete(a, leakCheckPushedAnnotation) },
			told: map[string]string{leakguard.CheckVar: leakguard.InstalledCheck}},
		{name: "a record of another pod", edit: func(a map[string]string) {
			a[leakCheckPushedAnnotation] = `{"pod":"another-pod","manifestSHA256":"` + testLeakCheck.ManifestSHA256 + `"}`
		}, told: map[string]string{leakguard.CheckVar: leakguard.InstalledCheck}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, false),
				withOptions(func(o *Options) { o.LeakCheck, o.LeakCheckSetting = servedLeakCheck(), leakguard.Setting{} }))
			loc := g.spawn(workerSpec(t))
			name := loc.Sandbox.Name
			built := g.sandbox(name)
			if _, recorded := built.Spec.PodTemplate.Metadata.Annotations[leakCheckerAnnotation]; recorded {
				t.Fatalf("a pod built under no setting records the digest %q on its template", built.Spec.PodTemplate.Metadata.Annotations[leakCheckerAnnotation])
			}
			if pushed := countOf(g.sent(workerToken), shimwire.TypeLauncherLeakCheck); pushed != 0 {
				t.Fatalf("the daemon running no setting pushed %d slices of a check", pushed)
			}

			// The setting is turned on, and a launch that keeps the pod pushes the setting's check.
			r2 := restartedOver(t, g, loc, func(o *Options) { o.LeakCheck, o.LeakCheckSetting = servedLeakCheck(), testLeakCheck })
			kept := resumeTester(t, g, r2, loc)
			if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Fatalf("Resume replaced the pod; this row is about the pod a launch keeps")
			}
			if countOf(g.sent(workerToken), shimwire.TypeLauncherLeakCheck) == 0 {
				t.Fatalf("the daemon running the setting pushed nothing into a launcher that holds no install, so this row proves nothing")
			}
			if told := leakCheckTold(startSent(t, g, 2)); told[leakguard.CheckerVar] != testLeakCheck.ManifestSHA256 {
				t.Fatalf("the role the push was made for was told %v of the leak check, want the setting's digest", told)
			}
			pushed := g.sandbox(name)
			var record pushedLeakCheck
			if err := json.Unmarshal([]byte(pushed.Annotations[leakCheckPushedAnnotation]), &record); err != nil ||
				record != (pushedLeakCheck{Pod: loc.Sandbox.PodUID, ManifestSHA256: testLeakCheck.ManifestSHA256}) {
				t.Fatalf("the Sandbox records %s=%q (%v), want the pod %s and the digest pushed, %s", leakCheckPushedAnnotation,
					pushed.Annotations[leakCheckPushedAnnotation], err, loc.Sandbox.PodUID, testLeakCheck.ManifestSHA256)
			}
			if !equality.Semantic.DeepEqual(pushed.Spec.PodTemplate, built.Spec.PodTemplate) || pushed.Generation != built.Generation {
				t.Fatalf("the record changed the Sandbox's pod template or its generation (%d, built at %d)", pushed.Generation, built.Generation)
			}
			if pod := g.pod(name); pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
				t.Fatalf("the pod is %v after the record, want the one built, %s", pod, loc.Sandbox.PodUID)
			}
			if drifted := driftOf(t, g, r2, kept); len(drifted) != 0 {
				t.Fatalf("PodDrift = %q after the record, want nothing", drifted)
			}
			if tc.edit != nil {
				tc.edit(pushed.Annotations)
				u, err := encodeSandbox(*pushed)
				if err != nil {
					t.Fatal(err)
				}
				if err := g.dyn.Tracker().Update(sandboxGVR, u, testNamespace); err != nil {
					t.Fatal(err)
				}
			}

			// The launcher reconnects, holding the install this daemon pushed. connectRunning has
			// the runtime accept that hello before it returns, so the session state reads once it is
			// ready is the reconnected launcher's.
			g.leakCheckInstalled = true
			g.connectRunning(workerToken, kept.Sandbox.Generation)
			g.eventually("the tester's reconnected launcher to report its running generation", func() bool {
				state, ready := r2.launchers.state(workerToken, kept.Sandbox.PodUID)
				return ready && state.Child != nil
			})
			before := countOf(g.sent(workerToken), shimwire.TypeLauncherLeakCheck)
			spec := workerSpec(t)
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 3, "boot-tester-g3", resumeSession
			later, err := r2.Resume(g.ctx, &kept, spec)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if later.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Fatalf("the later Resume replaced the pod; this row is about the pod a launch keeps")
			}
			if after := countOf(g.sent(workerToken), shimwire.TypeLauncherLeakCheck); after != before {
				t.Fatalf("the daemon pushed the check again to a launcher that holds it installed")
			}
			if told := leakCheckTold(startSent(t, g, 3)); !maps.Equal(told, tc.told) {
				t.Fatalf("the role started after the reconnect was told %v of the leak check, want %v", told, tc.told)
			}

			// A daemon restarted under another digest reads the record as the digest the pod holds.
			r3 := restartedOver(t, g, later, func(o *Options) { o.LeakCheck, o.LeakCheckSetting = servedLeakCheck(), moved })
			if drifted := slices.Contains(driftOf(t, g, r3, later), movedReason); drifted != tc.drifts {
				t.Fatalf("PodDrift under the moved digest names %q: %t, want %t", movedReason, drifted, tc.drifts)
			}
		})
	}
}

// A launcher can reconnect while the setting turns on. The digest that guards its role must come
// from the exact tarball the reconnected launcher receives, not from the setting that selected
// the repository: an image pin can still name an older tag. The served digest is recorded for the
// kept pod, so this role and later roles run it, and it is immediately PodDrift against the newer
// setting. Deciding the digest before the start from what an earlier hello said, rather than at the
// serve, records and tells configured.ManifestSHA256 instead, so this row fails.
func TestAKeptPodRecordsAndTellsTheManifestItsReconnectedLauncherWasServed(t *testing.T) {
	t.Parallel()
	configured := leakguard.Setting{Repository: testLeakCheck.Repository, ManifestSHA256: strings.Repeat("7c", 32)}
	source := servedLeakCheck()
	g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, false),
		withOptions(func(o *Options) { o.LeakCheck, o.LeakCheckSetting = source, leakguard.Setting{} }))
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, func(o *Options) {
		o.LeakCheck, o.LeakCheckSetting = source, configured
	})

	kept := resumeTester(t, g, r2, loc)
	if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the pod; this row is about the kept pod")
	}
	if got := leakCheckTold(startSent(t, g, 2))[leakguard.CheckerVar]; got != source.manifest {
		t.Fatalf("the role the served check starts was told %q, want the served manifest %s", got, source.manifest)
	}
	pushed := g.sandbox(kept.Sandbox.Name)
	var record pushedLeakCheck
	if err := json.Unmarshal([]byte(pushed.Annotations[leakCheckPushedAnnotation]), &record); err != nil ||
		record != (pushedLeakCheck{Pod: kept.Sandbox.PodUID, ManifestSHA256: source.manifest}) {
		t.Fatalf("the Sandbox records %s=%q (%v), want the served manifest %s for pod %s",
			leakCheckPushedAnnotation, pushed.Annotations[leakCheckPushedAnnotation], err, source.manifest, kept.Sandbox.PodUID)
	}
	reason := "leak check manifest " + source.manifest + ", now " + configured.ManifestSHA256
	if drifted := slices.Contains(driftOf(t, g, r2, kept), reason); !drifted {
		t.Fatalf("PodDrift = %q, want the served manifest drift %q", driftOf(t, g, r2, kept), reason)
	}

	before := countOf(g.sent(workerToken), shimwire.TypeLauncherLeakCheck)
	spec := workerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 3, "boot-tester-g3", resumeSession
	later, err := r2.Resume(g.ctx, &kept, spec)
	if err != nil {
		t.Fatalf("Resume after the served check: %v", err)
	}
	if later.Sandbox.PodUID != kept.Sandbox.PodUID {
		t.Fatalf("the later Resume replaced the kept pod")
	}
	if got := leakCheckTold(startSent(t, g, 3))[leakguard.CheckerVar]; got != source.manifest {
		t.Fatalf("the later role was told %q, want the served manifest %s", got, source.manifest)
	}
	if after := countOf(g.sent(workerToken), shimwire.TypeLauncherLeakCheck); after != before {
		t.Fatalf("the later role pushed %d more leak-check slices after its launcher installed the served check", after-before)
	}
}

// A change of the deployment's manifest digest alone is drift: an issue pod built under one digest
// is reported drifted, naming both, by a daemon restarted under another, so its update round
// opens and replaces the pod once its workers have pushed — a corrected digest reaches every pod.
// A pod built under no setting records none and has not drifted for it, and neither has a pod
// built under the digest the daemon still runs.
func TestAChangedManifestDigestIsDrift(t *testing.T) {
	t.Parallel()
	moved := leakguard.Setting{Repository: testLeakCheck.Repository, ManifestSHA256: strings.Repeat("7c", 32)}
	for _, tc := range []struct {
		name   string
		built  leakguard.Setting
		now    leakguard.Setting
		reason string
	}{
		{name: "another digest", built: testLeakCheck, now: moved,
			reason: "leak check manifest " + testLeakCheck.ManifestSHA256 + ", now " + moved.ManifestSHA256},
		{name: "no setting now", built: testLeakCheck, now: leakguard.Setting{},
			reason: "leak check manifest " + testLeakCheck.ManifestSHA256 + ", now none"},
		{name: "the same digest", built: testLeakCheck, now: testLeakCheck},
		{name: "a pod that records none", built: leakguard.Setting{}, now: moved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, true),
				withOptions(func(o *Options) { o.LeakCheck, o.LeakCheckSetting = servedLeakCheck(), tc.built }))
			loc := g.spawn(workerSpec(t))
			r2 := restartedOver(t, g, loc, func(o *Options) { o.LeakCheck, o.LeakCheckSetting = servedLeakCheck(), tc.now })
			drifted := driftOf(t, g, r2, loc)
			switch {
			case tc.reason == "" && len(drifted) != 0:
				t.Fatalf("PodDrift = %q, want nothing", drifted)
			case tc.reason != "" && !slices.Contains(drifted, tc.reason):
				t.Fatalf("PodDrift = %q, want %q among it", drifted, tc.reason)
			}
		})
	}
}

// A launcher whose hello says it holds its pin installed — its pod's launcher process took the
// check before the daemon restarted — is sent nothing of it, and the daemon does not read
// GitHub for a running pod.
func TestALauncherHoldingItsPinInstalledIsSentNothingAndStarts(t *testing.T) {
	t.Parallel()
	source := servedLeakCheck()
	g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, true), withOptions(func(o *Options) { o.LeakCheck = source }))
	spec := workerSpec(t)
	g.spawn(spec)
	frames := g.sent(spec.Claim)
	if countOf(frames, shimwire.TypeLauncherLeakCheck) != 0 || countOf(frames, shimwire.TypeLauncherStart) != 1 {
		t.Fatalf("the launcher was sent %v, want the role's start and no slice of the check", frameTypes(frames))
	}
	if reads := source.reads(); len(reads) != 0 {
		t.Fatalf("the daemon read the check at %v for a launcher that holds it installed", reads)
	}
}

// A check that could not be put in place refuses the role's start before anything of the start
// reaches the launcher: no start frame, so no stop of one either. The error carries the cause
// sentence, which the claim reads (runtime.LeakCheckCause), wrapped as every start error of the
// launch is.
func TestARefusedLeakCheckStartsNothing(t *testing.T) {
	t.Parallel()
	podsLauncher := leakguard.Cause("the pod's launcher could not unpack the check the daemon served", errors.New("gzip: invalid header"))
	for name, tc := range map[string]struct {
		source  LeakCheckReader
		options []rigOption
		want    string
	}{
		"the daemon holds no credential": {
			want: leakguard.Cause("the daemon runs with no GitHub App, so it cannot read example-org/deployment", nil),
		},
		"the daemon's read is refused": {
			source: &fakeLeakCheck{err: errors.New(leakguard.Cause("GitHub answered the read of example-org/deployment at "+testLeakCheckTag+" with HTTP 404: Not Found", nil))},
			want:   leakguard.Cause("GitHub answered the read of example-org/deployment at "+testLeakCheckTag+" with HTTP 404: Not Found", nil),
		},
		"the served manifest differs from a new pod's configured manifest": {
			source: func() *fakeLeakCheck {
				source := servedLeakCheck()
				source.manifest = strings.Repeat("7c", 32)
				return source
			}(),
			want: leakguard.Cause("the check the daemon served at "+testLeakCheckTag+" has manifest digest "+strings.Repeat("7c", 32)+
				", but the role would be told "+testLeakCheck.ManifestSHA256, nil),
		},
		"the pod's launcher refuses the install": {
			source:  servedLeakCheck(),
			options: []rigOption{withRefusedLeakCheck(podsLauncher)},
			want:    podsLauncher,
		},
		"the pod's launcher refuses in words of its own": {
			source:  servedLeakCheck(),
			options: []rigOption{withRefusedLeakCheck("the staging directory could not be made: no space left on device")},
			want:    leakguard.RefusalPrefix + "the staging directory could not be made: no space left on device",
		},
		"the pod's launcher refuses with no cause": {
			source:  servedLeakCheck(),
			options: []rigOption{withCauselessLeakCheck()},
			want: leakguard.Cause("the pod's launcher answered slice 0 of "+testLeakCheckTag+" with a malformed result",
				shimwire.LauncherLeakCheckResult{ID: "x"}.Validate()),
		},
	} {
		t.Run(name, func(t *testing.T) {
			options := append([]rigOption{withLeakCheckHello(testLeakCheckTag, false), withOptions(func(o *Options) { o.LeakCheck = tc.source })}, tc.options...)
			g := newRig(t, nil, options...)
			spec := workerSpec(t)
			g.launcher(spec.Claim)
			_, err := g.r.Spawn(g.ctx, spec)
			if err == nil {
				t.Fatalf("the launch started the role, want it refused with %q", tc.want)
			}
			cause := runtime.LeakCheckCause(err)
			if cause != tc.want || !strings.HasPrefix(cause, leakguard.RefusalPrefix) {
				t.Fatalf("the launch failed with %q carrying the cause %q, want %q", err, cause, tc.want)
			}
			if want := "launch " + string(spec.Claim) + ": serve the shared public leak check: " + tc.want; err.Error() != want {
				t.Fatalf("the launch failed with %q, want %q", err, want)
			}
			frames := g.sent(spec.Claim)
			for _, sent := range []string{shimwire.TypeLauncherStart, shimwire.TypeLauncherStop} {
				if countOf(frames, sent) != 0 {
					t.Fatalf("the launcher was sent %v, want no %s after the refusal", frameTypes(frames), sent)
				}
			}
		})
	}
}

// The push has its own bound under the start's: a launcher that takes the check and answers
// nothing within it, or reads nothing so the push never leaves its write, refuses the start with
// the sentence that names the bound. The write's deadline is the hop's, and a write that runs into
// it ends with the connection's timeout, before the hop's own timer may have ended the hop: it is
// still the hop's bound that ran out (requestSession). A loaded machine makes a launcher that
// reads too slowly the second row.
func TestALeakCheckHopThatRunsPastItsBoundRefusesTheStart(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		launcher rigOption
		// check is what else holds after the refusal, of what the launcher read and whether the
		// daemon still holds its connection.
		check func(t *testing.T, g *rig, token claim.Token)
	}{
		"a launcher that answers nothing": {withSilentLeakCheck(), func(t *testing.T, g *rig, token claim.Token) {
			frames := g.sent(token)
			if countOf(frames, shimwire.TypeLauncherStart) != 0 || countOf(frames, shimwire.TypeLauncherStop) != 0 {
				t.Fatalf("the launcher was sent %v, want no start and no stop after the hop ran out", frameTypes(frames))
			}
		}},
		"a launcher that reads nothing": {withDeafLauncher(), func(t *testing.T, g *rig, token claim.Token) {
			if session := launcherSessionOf(g, token); session != nil {
				t.Fatal("the daemon still holds the connection whose write ran out, want it dropped so the launcher dials again")
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, false), tc.launcher,
				withOptions(func(o *Options) { o.LeakCheck = servedLeakCheck() }))
			g.r.launchers.leakCheckHop = 50 * time.Millisecond
			spec := workerSpec(t)
			g.launcher(spec.Claim)
			_, err := g.r.Spawn(g.ctx, spec)
			want := leakguard.Cause("the pod's launcher did not answer the install of "+testLeakCheckTag+" within 50ms", nil)
			if cause := runtime.LeakCheckCause(err); cause != want {
				t.Fatalf("the launch failed with %v carrying the cause %q, want %q", err, cause, want)
			}
			tc.check(t, g, spec.Claim)
		})
	}
}

// A launcher that installed the check on this connection is pushed it no more: the next start
// of its role sends the role's start alone.
func TestTwoStartsOnOneConnectionPushTheLeakCheckOnce(t *testing.T) {
	t.Parallel()
	source := servedLeakCheck()
	g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, false), withOptions(func(o *Options) { o.LeakCheck = source }))
	spec := workerSpec(t)
	first := g.spawn(spec)
	pushed := countOf(g.sent(spec.Claim), shimwire.TypeLauncherLeakCheck)
	if pushed < 2 {
		t.Fatalf("the first start pushed %d slice(s) of the check, want its sequence", pushed)
	}
	spec.Generation = first.Sandbox.Generation + 1
	second := g.spawn(spec)
	if second.Sandbox.PodUID != first.Sandbox.PodUID {
		t.Fatalf("the second start ran in pod %s, want the first's %s on the same connection", second.Sandbox.PodUID, first.Sandbox.PodUID)
	}
	frames := g.sent(spec.Claim)
	if got := countOf(frames, shimwire.TypeLauncherLeakCheck); got != pushed || countOf(frames, shimwire.TypeLauncherStart) != 2 {
		t.Fatalf("the launcher was sent %v, want the check's %d slices once and two starts", frameTypes(frames), pushed)
	}
}

// A role's environment, as the runtime builds it for a deployment that names a shared leak check,
// carries the deployment's checker under the role's own name (leakguard.CheckerVar) and neither of
// the daemon's two variables (leakguard.RepositoryVar, leakguard.ManifestVar), so a configuration
// loaded in the role — `legion config check` in a pod, which reads the process
// environment — loads with no refusal of a leak_check half-set.
func TestARolesEnvironmentLoadsAConfigurationWithNoLeakCheckRefusal(t *testing.T) {
	t.Parallel()
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	_, plain := r.launchEnvironment(launchOf(t, r, workerSpec(t)))
	env := map[string]string{}
	for _, pair := range plain {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	if env[leakguard.CheckerVar] != testLeakCheck.ManifestSHA256 {
		t.Fatalf("the role is told %s=%q, want the deployment's checker %q", leakguard.CheckerVar, env[leakguard.CheckerVar], testLeakCheck.ManifestSHA256)
	}
	for _, name := range []string{leakguard.RepositoryVar, leakguard.ManifestVar} {
		if value, told := env[name]; told {
			t.Errorf("the role is told the daemon's own %s=%q, which a configuration it loads would read", name, value)
		}
	}
	file := filepath.Join(t.TempDir(), "legion.yaml")
	if err := os.WriteFile(file, []byte("project: DEMO\nstate_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(file, func(name string) string { return env[name] }); err != nil {
		t.Fatalf("a configuration loaded in the role's environment is refused: %v", err)
	}
}
