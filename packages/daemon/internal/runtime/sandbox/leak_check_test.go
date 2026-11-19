package sandbox

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// testLeakCheckTag is the pin a fake launcher's hello names when a test gives it one.
const testLeakCheckTag = "leak-check-v3"

// fakeLeakCheck stands in for the daemon's reader of the shared public leak check
// (leakguard.Source): it answers body at any tag, sliced as the reader slices it, or refuses every
// read with err, and records the tag of each read.
type fakeLeakCheck struct {
	body []byte
	err  error

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
	return leakguard.Served{Tag: tag, Body: f.body, Frames: frames}, nil
}

func (f *fakeLeakCheck) reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tags)
}

// servedLeakCheck is a reader whose packed check is more than one
// shimwire.LauncherBundlePayloadBytes, so the push is a sequence of slices and not one frame.
func servedLeakCheck() *fakeLeakCheck {
	return &fakeLeakCheck{body: bytes.Repeat([]byte("leak-check\x00"), shimwire.LauncherBundlePayloadBytes/11+4096)}
}

// leakCheckStore is a store holding the issue's saved workspace bundle, as a released issue's
// does when its next pod starts: the bundle's one restore attempt is what a refused start must
// not spend.
func leakCheckStore(t *testing.T) (*store.Store, runtime.WorkspaceBundle) {
	t.Helper()
	st := migratedStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	return st, saved
}

// withLeakCheckAndStore hands the runtime source as its reader of the leak check and st's saved
// workspace bundles.
func withLeakCheckAndStore(source LeakCheckReader, st *store.Store) rigOption {
	return withOptions(func(o *Options) {
		o.LeakCheck = source
		o.SavedWorkspaceBundle, o.WorkspaceBundleRestored = st.TakeWorkspaceBundleForRestore, st.DeleteRestoredWorkspaceBundle
	})
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

// A launcher whose hello names its pin is sent the check the daemon read at that pin before
// anything else of the start: every slice of it, in order, before the first slice of the saved
// workspace bundle and before the role's start, since the launcher installs the check once for
// the pod and every role's shim refuses to start its agent without that install.
func TestALauncherNamingItsPinIsSentTheLeakCheckBeforeTheBundleAndTheStart(t *testing.T) {
	st, saved := leakCheckStore(t)
	source := servedLeakCheck()
	g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, false), withLeakCheckAndStore(source, st))
	spec := workerSpec(t)
	g.spawn(spec)

	frames := g.sent(spec.Claim)
	start := slices.IndexFunc(frames, func(f shimwire.Frame) bool { _, ok := f.(shimwire.LauncherStart); return ok })
	firstBundle := slices.IndexFunc(frames, func(f shimwire.Frame) bool { _, ok := f.(shimwire.LauncherBundle); return ok })
	if start < 0 || firstBundle < 0 {
		t.Fatalf("the launcher was sent %v, want the check's slices, the bundle's slices and then the role's start", frameTypes(frames))
	}
	var body []byte
	var sequence string
	slicesSent := 0
	for i, frame := range frames[:firstBundle] {
		slice, ok := frame.(shimwire.LauncherLeakCheck)
		if !ok {
			t.Fatalf("the launcher was sent %v, want the check's slices before anything else", frameTypes(frames))
		}
		if i == 0 {
			sequence = slice.ID
		}
		if slice.ID != sequence || slice.Index != i || slice.Count != firstBundle || slice.Tag != testLeakCheckTag {
			t.Fatalf("slice %d of %d of %s under %q arrived at position %d of the %d sent before the bundle",
				slice.Index, slice.Count, slice.Tag, slice.ID, i, firstBundle)
		}
		body = append(body, slice.Payload...)
		slicesSent++
	}
	if slicesSent < 2 {
		t.Fatalf("the check crossed in %d slice(s), want the sequence of the %d bytes read", slicesSent, len(source.body))
	}
	if !bytes.Equal(body, source.body) {
		t.Fatalf("the check crossed as %d bytes, want the %d the daemon read", len(body), len(source.body))
	}
	if got := countOf(frames, shimwire.TypeLauncherLeakCheck); got != slicesSent {
		t.Fatalf("the launcher was sent %v: a slice of the check crossed after the bundle began", frameTypes(frames))
	}
	if firstBundle > start {
		t.Fatalf("the launcher was sent %v, want the bundle's slices before the role's start", frameTypes(frames))
	}
	if reads := source.reads(); !slices.Equal(reads, []string{testLeakCheckTag}) {
		t.Fatalf("the daemon read the check at %v, want once at the launcher's pin %s", reads, testLeakCheckTag)
	}
	// The bundle went over as it does with no check at all, and the launcher's report of its
	// restore dropped it.
	if _, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || held {
		t.Fatalf("after the launcher reported its restore the store holds the bundle = %t (%v), want it deleted (%d bytes saved)", held, err, len(saved.Bundle))
	}
}

// A launcher whose hello names no pin takes no check from the daemon — an image older than the
// push, or a pod whose image carries no pin — and its role starts as it always has: the daemon
// reads nothing and sends it no slice.
func TestALauncherNamingNoPinIsSentNoLeakCheckAndStarts(t *testing.T) {
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
// launcher holds none, the restarted daemon pushes the setting's check into it, and its role is
// told the setting's digest, so the check pushed is hashed. A pod the launch builds itself is told
// the digest now (TestADeploymentThatNamesNoLeakCheckPushesNoneAndTellsItsRolesNone).
func TestARoleStartedInAKeptPodIsToldTheCheckerItsPodWasBuiltUnder(t *testing.T) {
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
			told: map[string]string{leakguard.CheckVar: leakguard.InstalledCheck, leakguard.CheckerVar: moved.ManifestSHA256}},
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
			if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) == 0 {
				t.Fatalf("the pod has drifted from nothing, so this case proves nothing")
			}
			kept := resumeTester(t, g, r2, loc)
			if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Fatalf("Resume replaced the drifted pod; this case is about the pod a launch keeps")
			}
			if pushed := countOf(g.sent(workerToken), shimwire.TypeLauncherLeakCheck) > before; pushed != tc.pushed {
				t.Fatalf("the restarted daemon pushed the check to the kept pod's launcher: %t, want %t", pushed, tc.pushed)
			}
			told := map[string]string{}
			for name, value := range startEnv(startSent(t, g, 2)) {
				if name == leakguard.CheckVar || name == leakguard.CheckerVar {
					told[name] = value
				}
			}
			if !maps.Equal(told, tc.told) {
				t.Fatalf("the role started in the kept pod was told %v of the leak check, want %v", told, tc.told)
			}
		})
	}
}

// A change of the deployment's manifest digest alone is drift: an issue pod built under one digest
// is reported drifted, naming both, by a daemon restarted under another, so its update round
// opens and replaces the pod once its workers have pushed — a corrected digest reaches every pod.
// A pod built under no setting records none and has not drifted for it, and neither has a pod
// built under the digest the daemon still runs.
func TestAChangedManifestDigestIsDrift(t *testing.T) {
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
			drifted := driftOf(t, g.ctx, r2, loc)
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
// reaches the launcher: no start frame, so no stop of one either, and no slice of the saved
// workspace bundle, which stays in the store for the start that follows (the bundle's one restore
// attempt is not spent on a pod no role ran in). The error carries the cause sentence, which the
// claim reads (runtime.LeakCheckCause), wrapped as every start error of the launch is.
func TestARefusedLeakCheckStartsNothingAndLeavesTheSavedBundle(t *testing.T) {
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
			st, saved := leakCheckStore(t)
			options := append([]rigOption{withLeakCheckHello(testLeakCheckTag, false), withLeakCheckAndStore(tc.source, st)}, tc.options...)
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
			if want := "launch " + string(spec.Claim) + ": start through its authenticated launcher: " + tc.want; err.Error() != want {
				t.Fatalf("the launch failed with %q, want %q", err, want)
			}
			frames := g.sent(spec.Claim)
			for _, sent := range []string{shimwire.TypeLauncherStart, shimwire.TypeLauncherStop, shimwire.TypeLauncherBundle} {
				if countOf(frames, sent) != 0 {
					t.Fatalf("the launcher was sent %v, want no %s after the refusal", frameTypes(frames), sent)
				}
			}
			pod := g.pod(SandboxName(spec.Claim))
			if pod == nil {
				t.Fatal("the refused launch left no pod")
			}
			bundle, take, err := st.TakeWorkspaceBundleForRestore(g.ctx, testTree, string(pod.UID), pod.CreationTimestamp.Time)
			if err != nil {
				t.Fatal(err)
			}
			if take != runtime.WorkspaceBundleSend || !bytes.Equal(bundle.Bundle, saved.Bundle) {
				t.Fatalf("after the refusal the store answers the pod %d bytes (take %v), want the %d saved still to send",
					len(bundle.Bundle), take, len(saved.Bundle))
			}
		})
	}
}

// The push has its own bound under the start's: a launcher that takes the check and answers
// nothing within it refuses the start with the sentence that names the bound, and no start is
// sent.
func TestALeakCheckHopThatRunsPastItsBoundRefusesTheStart(t *testing.T) {
	g := newRig(t, nil, withLeakCheckHello(testLeakCheckTag, false), withSilentLeakCheck(),
		withOptions(func(o *Options) { o.LeakCheck = servedLeakCheck() }))
	const bound = 50 * time.Millisecond
	g.r.launchers.leakCheckHop = bound
	spec := workerSpec(t)
	g.launcher(spec.Claim)
	_, err := g.r.Spawn(g.ctx, spec)
	want := leakguard.Cause("the pod's launcher did not answer the install of "+testLeakCheckTag+" within 50ms", nil)
	if cause := runtime.LeakCheckCause(err); cause != want {
		t.Fatalf("the launch failed with %v carrying the cause %q, want %q", err, cause, want)
	}
	frames := g.sent(spec.Claim)
	if countOf(frames, shimwire.TypeLauncherStart) != 0 || countOf(frames, shimwire.TypeLauncherStop) != 0 {
		t.Fatalf("the launcher was sent %v, want no start and no stop after the hop ran out", frameTypes(frames))
	}
}

// A launcher that installed the check on this connection is pushed it no more: the next start
// of its role sends the role's start alone.
func TestTwoStartsOnOneConnectionPushTheLeakCheckOnce(t *testing.T) {
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
