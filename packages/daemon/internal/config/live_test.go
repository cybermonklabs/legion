package config

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// newLive is a holder over a fresh copy of the smallest file that loads, with the route overlay
// the caller's store would have seeded laid back over it through the route arm, as the boot's
// own restore lays it (internal/settings' Keeper.Restore). It answers the file's path so a test
// can rewrite it.
func newLive(t *testing.T, body string, overlay map[string]any) (*Live, string) {
	t.Helper()
	path := writeConfigFile(t, body)
	live, err := NewLive(path, noEnv, nil)
	if err != nil {
		t.Fatalf("NewLive: %v", err)
	}
	if len(overlay) > 0 {
		if _, refused, err := live.Apply(SourceRoute, "", overlay); err != nil || len(refused) > 0 {
			t.Fatalf("lay the stored overlay back over %s: %v, refused %+v", path, err, refused)
		}
	}
	return live, path
}

// rewrite replaces the file the holder reads, as an operator's edit or a ConfigMap update does.
func rewrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewrite the config file: %v", err)
	}
}

// kubernetesLiveFile is kubernetesFile with App keys this holder can resolve: NewLive runs the
// loader whole, every `private_key_command` with it, and that fixture's commands are written to
// fail.
var kubernetesLiveFile = strings.ReplaceAll(kubernetesFile, `private_key_command: "exit 1"`, "private_key: app-test-key")

// tmuxLiveFile is that same file with its one `runtime` key written as the other runtime's,
// which is the swap docs/deployment.md has an operator make before a restart. Everything else
// is one deployment's, so a rewrite from either file to the other changes that key alone, and
// the tmux side carries a block of its own: a settle must keep the whole of whichever block
// the daemon booted on.
var tmuxLiveFile = runtimeSwapped(kubernetesLiveFile, "runtime:\n  tmux:\n    pi_config_files: [overlay.json]\n")

// runtimeSwapped is file with its `runtime` key, the last key kubernetesFile holds, replaced by
// block. It panics on a file carrying no such key, so a fixture that stops holding one cannot
// pass by swapping nothing.
func runtimeSwapped(file, block string) string {
	head, _, found := strings.Cut(file, "runtime:\n")
	if !found {
		panic("the fixture carries no runtime key to swap")
	}
	return head + block
}

// bootDaemonURL is the `daemon_url` kubernetesFile boots on, which a pod's launch is recorded
// with (sandbox.LaunchSettings).
const bootDaemonURL = "daemon_url: http://10.0.0.5:13370"

// foreignRuntimeFile is that same deployment's file rewritten as the other runtime's — the edit
// docs/deployment.md has an operator make before a restart — with two keys the loader checks
// under `kubernetes` alone edited by the same hand: `daemon_url`, required there and optional
// here, dropped, and `worker_stop_timeout_seconds` put under the floor a pod's lease release
// needs (shim.MinStopGrace, config.go's resolve). It loads, because it names the runtime whose
// rules it keeps.
var foreignRuntimeFile = runtimeSwapped("worker_stop_timeout_seconds: 3\n"+edited(kubernetesLiveFile, bootDaemonURL+"\n", ""), "runtime: tmux\n")

// sameRuntimeFile is that same pair of keys edited under the runtime the daemon booted on: the
// address a pane is launched with moved, and a stop timeout well over the floor. Both settle,
// which is what makes the refusal of the reading above a refusal of the reading and not a
// guard over those two keys.
var sameRuntimeFile = "worker_stop_timeout_seconds: 20\n" + edited(kubernetesLiveFile, bootDaemonURL, "daemon_url: http://10.0.0.6:13370")

// edited is body with the one occurrence of old written as next. It panics on a body that does
// not hold old, as runtimeSwapped does, so a fixture that stops holding a key cannot pass by
// editing nothing.
func edited(body, old, next string) string {
	if !strings.Contains(body, old) {
		panic("the fixture carries no " + old)
	}
	return strings.Replace(body, old, next, 1)
}

func TestNewLiveResolvesTheFileUnderItsStoredOverlay(t *testing.T) {
	live, _ := newLive(t, minimalFile, map[string]any{"admission_cap": 9})

	if cap := live.Snapshot().AdmissionCap; cap != 9 {
		t.Errorf("AdmissionCap = %d, want the stored overlay's 9", cap)
	}
	if project := live.Snapshot().Project; project != "DEMO" {
		t.Errorf("Project = %q, want the file's", project)
	}
}

// A route change of a decision path is in the next snapshot, and the caller is told it took
// effect at once.
func TestARouteChangeOfADecisionPathAppliesAtOnce(t *testing.T) {
	live, _ := newLive(t, minimalFile, nil)
	if cap := live.Snapshot().AdmissionCap; cap != defaultAdmissionCap {
		t.Fatalf("AdmissionCap = %d, want the default %d", cap, defaultAdmissionCap)
	}

	applied, refused, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{"admission_cap": 12})

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(refused) != 0 {
		t.Fatalf("refused %+v, want none", refused)
	}
	if cap := live.Snapshot().AdmissionCap; cap != 12 {
		t.Errorf("AdmissionCap = %d, want 12", cap)
	}
	want := Change{
		Path: "admission_cap", Old: "4", New: "12", Class: ClassDecision, Outcome: OutcomeAtOnce,
		Source: SourceRoute, By: "sami@theorem.dev",
	}
	if len(applied) != 1 || applied[0] != want {
		t.Errorf("applied %+v, want exactly %+v", applied, want)
	}
}

// A fixed path is refused by name with its reason, and nothing of the configuration moves —
// including the paths the same call asked for beside it.
func TestARouteChangeOfAFixedPathIsRefusedByName(t *testing.T) {
	live, _ := newLive(t, minimalFile, nil)
	before := live.Snapshot()

	applied, refused, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{"project": "OTHER"})

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied %+v, want none", applied)
	}
	if len(refused) != 1 || refused[0].Path != "project" || refused[0].Reason != whyProject {
		t.Fatalf("refused %+v, want project with its own reason", refused)
	}
	if after := live.Snapshot(); after.Project != before.Project {
		t.Errorf("Project = %q, want the file's %q", after.Project, before.Project)
	}
	if _, held := live.overlay["project"]; held {
		t.Error("the refused path is in the stored overlay")
	}
}

func TestARouteChangeOfAnUnknownPathIsRefused(t *testing.T) {
	live, _ := newLive(t, minimalFile, nil)

	applied, refused, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{"admision_cap": 12})

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied %+v, want none", applied)
	}
	if len(refused) != 1 || refused[0].Path != "admision_cap" || refused[0].Reason != "unknown setting" {
		t.Fatalf("refused %+v, want the typo refused as an unknown setting", refused)
	}
}

// A value the loader refuses is refused here, by the loader's own words, and the holder is left
// exactly as it was: the snapshot, and the stored overlay a later apply would lay over the file.
func TestAValueTheLoaderRefusesChangesNothing(t *testing.T) {
	live, _ := newLive(t, minimalFile, nil)
	before := live.Snapshot()

	applied, refused, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{"admission_cap": 0})

	if err == nil || !strings.Contains(err.Error(), "admission_cap must be a positive integer") {
		t.Fatalf("Apply error = %v, want the loader's refusal of admission_cap", err)
	}
	if len(applied) != 0 || len(refused) != 0 {
		t.Errorf("applied %+v and refused %+v, want neither", applied, refused)
	}
	if live.Snapshot() != before {
		t.Error("the snapshot was swapped under a refused resolution")
	}
	if len(live.overlay) != 0 {
		t.Errorf("the stored overlay is %+v, want it untouched", live.overlay)
	}

	// And the holder still applies the next good change, so nothing of the refusal is kept.
	if _, _, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{"admission_cap": 6}); err != nil {
		t.Fatalf("Apply after the refusal: %v", err)
	}
	if cap := live.Snapshot().AdmissionCap; cap != 6 {
		t.Errorf("AdmissionCap = %d, want 6", cap)
	}
}

// The file arm reads the file again. A path the stored route overlay holds keeps the route's
// value, because the overlay lies over the file; every other path takes the file's.
func TestTheFileArmReadsAgainAndTheRouteOverlayStillWins(t *testing.T) {
	live, path := newLive(t, minimalFile, nil)
	if _, _, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{"admission_cap": 12}); err != nil {
		t.Fatalf("Apply the route change: %v", err)
	}

	rewrite(t, path, minimalFile+"admission_cap: 5\nreview_round_cap: 7\n")
	applied, refused, err := live.Apply(SourceFile, "", nil)

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(refused) != 0 {
		t.Fatalf("refused %+v, want none", refused)
	}
	if cap := live.Snapshot().AdmissionCap; cap != 12 {
		t.Errorf("AdmissionCap = %d, want the route's 12 over the file's 5", cap)
	}
	if rounds := live.Snapshot().ReviewRoundCap; rounds != 7 {
		t.Errorf("ReviewRoundCap = %d, want the file's 7", rounds)
	}
	want := Change{
		Path: "review_round_cap", Old: "3", New: "7", Class: ClassDecision, Outcome: OutcomeAtOnce,
		Source: SourceFile,
	}
	if len(applied) != 1 || applied[0] != want {
		t.Errorf("applied %+v, want exactly %+v: the path the overlay holds did not change", applied, want)
	}
}

// A fixed path is fixed of the snapshot, and not of the route alone. Every settle reads the
// whole file again, so an operator who edits `project` in `legion.yaml` ahead of a planned
// restart would otherwise move the value every reader takes from Snapshot — the workflow
// engine's project, the subject filter and durable names a NATS rebuild opens its consumers on
// — while admission, intake and the claim tokens stayed on the one the daemon booted on. The
// settle pins it back and names the path with the reason the route refuses it by, and the
// daemon runs on the value it booted on until it is restarted.
//
// The pin is narrow: `review_round_cap`, a decision key the same edit changed, is applied.
func TestTheFileArmPinsAFixedPathToTheValueBootSettled(t *testing.T) {
	// Both projects are in the file from the start, so the one key this edit changes is the
	// fixed `project` itself and not the `projects` block beside it.
	body := strings.Replace(minimalFile, "  DEMO: {}", "  DEMO: {}\n  OTHER: {}", 1)
	live, path := newLive(t, body, nil)
	if project := live.Snapshot().Project; project != "DEMO" {
		t.Fatalf("the boot settled project %q, want the file's DEMO", project)
	}

	rewrite(t, path, strings.Replace(body, "project: DEMO", "project: OTHER", 1)+"review_round_cap: 7\n")
	applied, refused, err := live.Apply(SourceFile, "", nil)

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if project := live.Snapshot().Project; project != "DEMO" {
		t.Errorf("the settled project after the file edit = %q, want the boot's DEMO: every claim, every record and the registry entry are named by it", project)
	}
	if len(refused) != 1 || refused[0].Path != "project" || refused[0].Reason != whyProject {
		t.Fatalf("the settle refused %+v, want the one pinned path named with its own reason", refused)
	}
	if rounds := live.Snapshot().ReviewRoundCap; rounds != 7 {
		t.Errorf("ReviewRoundCap = %d, want the file's 7: the pin is of the fixed path alone", rounds)
	}
	want := Change{
		Path: "review_round_cap", Old: "3", New: "7", Class: ClassDecision, Outcome: OutcomeAtOnce,
		Source: SourceFile,
	}
	if len(applied) != 1 || applied[0] != want {
		t.Errorf("applied %+v, want exactly %+v: a pinned path is no change of the daemon's", applied, want)
	}
}

// The pin is of every resolution and not of the file arm alone: the route reads the file again
// too, so a route change of some other key would otherwise settle the file's edited `project`
// as a side effect of the key its caller asked about. The caller is answered for the paths it
// named and no others — the file's edit is the file arm's to report, at the settle the watcher
// makes of it.
func TestARouteChangeSettlesNoFixedPathTheFileEditedMeanwhile(t *testing.T) {
	body := strings.Replace(minimalFile, "  DEMO: {}", "  DEMO: {}\n  OTHER: {}", 1)
	live, path := newLive(t, body, nil)

	rewrite(t, path, strings.Replace(body, "project: DEMO", "project: OTHER", 1))
	applied, refused, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{"admission_cap": 11})

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(refused) != 0 {
		t.Fatalf("the route refused %+v, want nothing: its caller named one decision path", refused)
	}
	if project := live.Snapshot().Project; project != "DEMO" {
		t.Errorf("the settled project after a route change of another key = %q, want the boot's DEMO", project)
	}
	if cap := live.Snapshot().AdmissionCap; cap != 11 {
		t.Errorf("AdmissionCap = %d, want the route's 11", cap)
	}
	for _, change := range applied {
		if change.Path == "project" {
			t.Errorf("the route reported %+v, want no change of a path fixed for the process", change)
		}
	}
}

// The file arm carries no overlay of its own: that is the route's arm, and a caller that mixed
// the two would write a route change nobody recorded.
func TestTheFileArmRefusesAnOverlayArgument(t *testing.T) {
	live, _ := newLive(t, minimalFile, nil)

	if _, _, err := live.Apply(SourceFile, "", map[string]any{"admission_cap": 12}); err == nil {
		t.Fatal("Apply(file) with an overlay was accepted")
	}
	if _, _, err := live.Apply("watcher", "", nil); err == nil {
		t.Fatal("Apply with an unknown source was accepted")
	}
}

// Each class reports the outcome its holder means, and a launch or rebuild row names the holder
// the daemon builds again.
func TestAChangeCarriesItsClassHolderAndOutcome(t *testing.T) {
	live, _ := newLive(t, minimalFile, nil)

	applied, refused, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{
		"gates.design":               "off",
		"omp_launch_prefix":          []any{"mise", "x", "--"},
		"worker_rpc_timeout_seconds": 9,
	})

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(refused) != 0 {
		t.Fatalf("refused %+v, want none", refused)
	}
	want := []Change{
		// `gates.design` holds the steps it gates, so a change of it is reported as that list
		// (Render): `root-issues` is the one step spec, `off` the empty list.
		{Path: "gates.design", Old: `["spec"]`, New: "[]", Class: ClassDecision, Outcome: OutcomeAtOnce, Holder: HolderAPIServer, Source: SourceRoute, By: "sami@theorem.dev"},
		{Path: "omp_launch_prefix", Old: "null", New: `["mise","x","--"]`, Class: ClassLaunch, Outcome: OutcomeNextLaunch, Holder: HolderRuntime, Source: SourceRoute, By: "sami@theorem.dev"},
		{Path: "worker_rpc_timeout_seconds", Old: "5s", New: "9s", Class: ClassRebuild, Outcome: OutcomeRebuilt, Holder: HolderWorkerStream, Source: SourceRoute, By: "sami@theorem.dev"},
	}
	if !slices.Equal(applied, want) {
		t.Errorf("applied %+v, want %+v", applied, want)
	}
}

// A secret's new value is never printed, and a rotation is still seen: the diff compares the
// value and the Change shows the mark.
func TestASecretsChangeIsSeenAndNotShown(t *testing.T) {
	live, path := newLive(t, minimalFile, nil)

	rewrite(t, path, strings.Replace(minimalFile, "dispatch_url: http://127.0.0.1:8080", "dispatch_url: http://user:password@127.0.0.1:8080", 1))
	applied, _, err := live.Apply(SourceFile, "", nil)

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 1 || applied[0].Path != "dispatch_url" {
		t.Fatalf("applied %+v, want the dispatch_url change alone", applied)
	}
	if applied[0].Old != withheldValue || applied[0].New != withheldValue {
		t.Errorf("the change shows %q → %q, want both withheld", applied[0].Old, applied[0].New)
	}
	if strings.Contains(applied[0].New, "password") {
		t.Error("the change carries the address's userinfo")
	}
}

// A block the file names under one runtime and not the other reads as unset on the side that has
// none, so the paths it holds are changes rather than silence.
//
// The second row is the same drop under a boot whose block held a fixed value: the pin carries
// that value over in a block of this reading's own (pinBlock), and every other member of that
// carrier is a zero value the pin wrote rather than anything the file says. Read as the value
// it holds, one file dropping `runtime.tmux` reported `pi_config_files` two ways for the same
// configured value — "(unset)" under a boot that took over on nothing, "null" under a boot that
// took over on kubernetes.
func TestABlockTheFileDropsReadsAsUnset(t *testing.T) {
	for _, tc := range []struct{ name, booted, carries string }{
		{name: "a boot that took over on nothing", booted: ""},
		{name: "a boot whose block held a fixed value", booted: "    take_over: kubernetes\n", carries: TakeOverKubernetes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, path := newLive(t, minimalFile+"runtime:\n  tmux:\n    pi_config_files: [overlay.json]\n"+tc.booted, nil)
			if live.Snapshot().Runtime.Tmux == nil {
				t.Fatal("the tmux block was not read")
			}

			rewrite(t, path, minimalFile)
			applied, _, err := live.Apply(SourceFile, "", nil)

			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			index := slices.IndexFunc(applied, func(c Change) bool { return c.Path == "runtime.tmux.pi_config_files" })
			if index < 0 {
				t.Fatalf("applied %+v, want the dropped tmux overlay among them", applied)
			}
			if !strings.Contains(applied[index].Old, "overlay.json") || applied[index].New != UnsetValue {
				t.Errorf("the dropped path reads %q → %q, want the file's own overlay → %q",
					applied[index].Old, applied[index].New, UnsetValue)
			}
			block := live.Snapshot().Runtime.Tmux
			switch {
			case tc.carries == "" && block != nil:
				t.Errorf("the settled block is %+v, want none: the reading dropped it and no fixed value held it", block)
			case tc.carries != "" && (block == nil || block.TakeOver != tc.carries):
				t.Errorf("the settled block is %+v, want the take_over boot settled carried over", block)
			}
		})
	}
}

// A file that swaps the whole `runtime` key settles nothing: the daemon keeps the runtime it
// booted on with every member of its block, and the settle answers *OtherRuntime, naming the
// key, the runtime the file now names and the one the daemon booted on.
//
// The selector alone would be worse than nothing. `runtime` is one key carrying the selector
// and the block it names (readRuntime), so a settle that pinned the name and took the file's
// block would hold a configuration no loader produces: `kubernetes` beside no
// `runtime.kubernetes`, whose every member every reader of the block then reads as unset — and
// whose nil block the Sandbox build dereferences (internal/daemon's readSandbox), inside this
// apply, under the holder's lock, in a daemon supervising pods. The whole reading is refused
// instead, which is what keeps the keys beside that one from settling too
// (TestAReadingThatNamesAnotherRuntimeSettlesNothing).
func TestAFileThatSwapsTheRuntimeKeyKeepsTheWholeBlockBootSettled(t *testing.T) {
	for _, tc := range []struct{ name, booted, swapped, names string }{
		{"kubernetes to tmux", kubernetesLiveFile, tmuxLiveFile, "tmux"},
		{"tmux to kubernetes", tmuxLiveFile, kubernetesLiveFile, "kubernetes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, path := newLive(t, tc.booted, nil)
			booted := live.Snapshot().Runtime

			rewrite(t, path, tc.swapped)
			applied, refused, err := live.Apply(SourceFile, "", nil)

			var other *OtherRuntime
			if !errors.As(err, &other) {
				t.Fatalf("the settle answered %v, want the reading refused whole", err)
			}
			if want := (&OtherRuntime{Path: "runtime", File: tc.names, Booted: booted.Name}); *other != *want {
				t.Fatalf("the settle answered %+v, want %+v: the key the file swapped, with both runtimes", other, want)
			}
			if len(refused) != 0 {
				t.Fatalf("the settle refused %+v, want nothing beside the reading itself: no path of it was refused on its own account", refused)
			}
			if settled := live.Snapshot().Runtime; !reflect.DeepEqual(settled, booted) {
				t.Fatalf("the settled runtime = %+v, want the whole of the boot's %+v: the daemon supervises that runtime's processes", settled, booted)
			}
			if len(applied) != 0 {
				t.Errorf("applied %+v, want nothing at all: a reading of the other runtime settles no key", applied)
			}
		})
	}
}

// A reading that names a runtime other than the one the daemon booted on settles nothing at
// all — not one of the keys it shares with the booted deployment — and the daemon answers
// *OtherRuntime, naming both runtimes.
//
// The loader's cross-key checks are the checks of the runtime the reading names:
// `checkPodReachable` and the `worker_stop_timeout_seconds` floor at shim.MinStopGrace run
// under `kubernetes` alone, and `daemon_url` is required there and optional under tmux
// (config.go's resolve). So every other key of such a reading was checked by rules this daemon
// does not run, and a key of it settled under the runtime the daemon booted on reaches the
// pods it is supervising: this reading's three-second stop timeout is every Sandbox's
// TerminationGrace, a third of what a shim's lease release alone needs, and the `daemon_url`
// it drops is a launch key recorded in sandbox.LaunchSettings. Pinning `runtime` alone settled
// both.
//
// The second row is the control: that same pair of keys, edited in the booted runtime's own
// reading, settles. So the first row is a reading refused and not two keys guarded.
func TestAReadingThatNamesAnotherRuntimeSettlesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, reading string
		other         bool
		applied       []string
		url           string
		stop          time.Duration
	}{
		{
			name:    "the other runtime's reading",
			reading: foreignRuntimeFile,
			other:   true,
			url:     "http://10.0.0.5:13370",
			stop:    10 * time.Second,
		},
		{
			name:    "the booted runtime's own reading",
			reading: sameRuntimeFile,
			applied: []string{"daemon_url", "worker_stop_timeout_seconds"},
			url:     "http://10.0.0.6:13370",
			stop:    20 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, path := newLive(t, kubernetesLiveFile, nil)
			booted := live.Snapshot()

			rewrite(t, path, tc.reading)
			applied, refused, err := live.Apply(SourceFile, "", nil)

			var other *OtherRuntime
			switch {
			case tc.other && !errors.As(err, &other):
				t.Fatalf("the settle answered %v, want the reading refused whole", err)
			case tc.other && *other != (OtherRuntime{Path: "runtime", File: "tmux", Booted: "kubernetes"}):
				t.Fatalf("the settle answered %+v, want the key and both runtimes", other)
			case !tc.other && err != nil:
				t.Fatalf("Apply: %v", err)
			}
			if len(refused) != 0 {
				t.Fatalf("the settle refused %+v, want nothing: no path of this reading was refused on its own account", refused)
			}
			if paths := appliedPaths(applied); !slices.Equal(paths, tc.applied) {
				t.Fatalf("the settle applied %v, want %v", paths, tc.applied)
			}
			settled := live.Snapshot()
			if tc.other && settled != booted {
				t.Fatalf("the settle swapped the snapshot for %+v: a reading of another runtime settles nothing, so the daemon keeps the very configuration it had", settled)
			}
			if settled.Runtime.Name != "kubernetes" || settled.Runtime.Kubernetes == nil {
				t.Fatalf("the settled runtime = %+v, want the kubernetes deployment the daemon booted on", settled.Runtime)
			}
			if settled.DaemonURL != tc.url {
				t.Errorf("daemon_url = %q, want %q: it is a launch key every pod's record carries", settled.DaemonURL, tc.url)
			}
			if settled.WorkerStopTimeout != tc.stop {
				t.Errorf("worker_stop_timeout_seconds = %s, want %s: it is every Sandbox's TerminationGrace", settled.WorkerStopTimeout, tc.stop)
			}
		})
	}
}

// The route arm answers such a reading the same way. An operator's change of some other key is
// resolved over the file as it now reads (resolve), so a file naming the other runtime settles
// nothing of that change either: the snapshot stands, the overlay stands — the next settle of
// a file this daemon can read runs on the file's own value, not on the change — and the caller
// is answered *OtherRuntime.
//
// The typo beside it is the path refused on its own account: that refusal is answered beside
// the error on either row, since what is wrong with it is wrong whatever runtime the file
// names, and a caller told only about the runtime would never learn of it.
//
// The second row is the control: the same call over the booted runtime's own file applies and
// is kept in the overlay, which is what the first row would look like if the route arm had
// been left settling what the file arm refuses.
func TestARouteChangeSettlesNothingWhileTheFileNamesAnotherRuntime(t *testing.T) {
	for _, tc := range []struct {
		name, reading string
		other         bool
		cap           int
	}{
		{
			name:    "the other runtime's reading",
			reading: foreignRuntimeFile,
			other:   true,
			cap:     defaultAdmissionCap,
		},
		{
			name:    "the booted runtime's own reading",
			reading: sameRuntimeFile,
			cap:     12,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, path := newLive(t, kubernetesLiveFile, nil)
			booted := live.Snapshot()
			rewrite(t, path, tc.reading)

			applied, refused, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{
				"admission_cap": 12, "admision_cap": 3,
			})

			var other *OtherRuntime
			switch {
			case tc.other && !errors.As(err, &other):
				t.Fatalf("the route change answered %v, want the reading refused whole", err)
			case tc.other && *other != (OtherRuntime{Path: "runtime", File: "tmux", Booted: "kubernetes"}):
				t.Fatalf("the route change answered %+v, want the key and both runtimes", other)
			case !tc.other && err != nil:
				t.Fatalf("Apply: %v", err)
			}
			want := []Refusal{{Path: "admision_cap", Reason: "unknown setting"}}
			if !slices.Equal(refused, want) {
				t.Fatalf("the route change was refused %+v, want exactly %+v: a path refused on its own account is answered whatever the file names", refused, want)
			}
			if tc.other {
				if len(applied) > 0 {
					t.Fatalf("the route change applied %+v, want nothing: the file it would have been resolved over is another runtime's", applied)
				}
				if settled := live.Snapshot(); settled != booted {
					t.Fatalf("the route change swapped the snapshot for %+v, want the very configuration the daemon had", settled)
				}
			}
			if cap := live.Snapshot().AdmissionCap; cap != tc.cap {
				t.Fatalf("admission_cap = %d, want %d", cap, tc.cap)
			}

			// And the overlay: the file reads as this daemon's deployment again, and what
			// stands over it is what the route settled and nothing else.
			rewrite(t, path, kubernetesLiveFile)
			if _, _, err := live.Apply(SourceFile, "", nil); err != nil {
				t.Fatalf("settle the file the daemon booted on: %v", err)
			}
			if cap := live.Snapshot().AdmissionCap; cap != tc.cap {
				t.Fatalf("admission_cap once the file reads as this daemon's again = %d, want %d: the overlay keeps what settled and no more", cap, tc.cap)
			}
		})
	}
}

// The reason a person meets carries the whole of what one needs: the key that named the
// runtime, the runtime the file names, the runtime this daemon booted on, and the two ways
// out. The Dispatch panel shows the verdict of the key the person edited and nothing beside
// it, so someone changing `admission_cap` reads this sentence alone — a reason naming one
// runtime leaves them unable to tell which deployment the file has become, and one naming no
// way out leaves them with a daemon that refuses every change and no act that ends it.
//
// The second row is the control: the booted runtime's own reading answers no error at all, so
// the sentence is the refusal's and not a line every settle writes.
func TestTheRefusalOfAnotherRuntimesReadingNamesBothRuntimesAndTheWayOut(t *testing.T) {
	for _, tc := range []struct {
		name, reading string
		other         bool
	}{
		{name: "the other runtime's reading", reading: foreignRuntimeFile, other: true},
		{name: "the booted runtime's own reading", reading: sameRuntimeFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, path := newLive(t, kubernetesLiveFile, nil)
			rewrite(t, path, tc.reading)

			_, _, err := live.Apply(SourceFile, "", nil)

			var other *OtherRuntime
			if !tc.other {
				if err != nil {
					t.Fatalf("Apply: %v", err)
				}
				return
			}
			if !errors.As(err, &other) {
				t.Fatalf("the settle answered %v, want the reading refused whole", err)
			}
			const want = `the configuration file under the daemon names runtime "tmux", not the "kubernetes" it booted on: ` +
				`no setting changes until the file names "kubernetes" again or the daemon restarts`
			if reason := other.Error(); reason != want {
				t.Fatalf("the reason reads\n\t%s\nwant\n\t%s", reason, want)
			}
		})
	}
}

// A fixed member is no change of the daemon's, whatever the reading carries, so `applied`
// never names one.
//
// A deployment writing `runtime: tmux` with no block of its own, whose file or route then adds
// one key of that block, carries a `runtime.tmux.take_over` the boot read as unset and this
// reading reads as "": the pin leaves it where it is, both readings holding the zero value, so
// a filter over the paths the pin moved drops nothing and the diff reports
// `runtime.tmux.take_over "(unset)" -> ""` — a key fixed for the process, reported changed, to
// a caller that named no such key. The class decides instead, so no reading can report one.
//
// Both arms are here because either can bring the block: the file arm reads the whole file
// again at every settle, and the route arm lays its caller's key over it.
func TestAFixedMemberNeverAppearsInApplied(t *testing.T) {
	const block = "runtime:\n  tmux:\n    pi_config_files: [overlay.json]\n"
	for _, tc := range []struct {
		name  string
		apply func(t *testing.T, live *Live, path string) []Change
	}{
		{
			name: "the file arm brings the block",
			apply: func(t *testing.T, live *Live, path string) []Change {
				t.Helper()
				rewrite(t, path, minimalFile+block)
				applied, refused, err := live.Apply(SourceFile, "", nil)
				if err != nil || len(refused) != 0 {
					t.Fatalf("Apply: %v, refused %+v", err, refused)
				}
				return applied
			},
		},
		{
			name: "the route arm brings the block",
			apply: func(t *testing.T, live *Live, _ string) []Change {
				t.Helper()
				applied, refused, err := live.Apply(SourceRoute, "sami@theorem.dev",
					map[string]any{"runtime.tmux.pi_config_files": []any{"overlay.json"}})
				if err != nil || len(refused) != 0 {
					t.Fatalf("Apply: %v, refused %+v", err, refused)
				}
				return applied
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, path := newLive(t, minimalFile, nil)
			if live.Snapshot().Runtime.Tmux != nil {
				t.Fatal("the boot carries a tmux block: this case is the deployment that writes none")
			}

			applied := tc.apply(t, live, path)

			// The launch keys of the block the reading brought are changes an operator asked
			// for and reads: `pane_env` arrives unset beside the one this reading names.
			if paths := appliedPaths(applied); !slices.Equal(paths, []string{"runtime.tmux.pane_env", "runtime.tmux.pi_config_files"}) {
				t.Fatalf("the settle applied %v, want the block's launch keys and no fixed member: a fixed member is no change of the daemon's", paths)
			}
			if block := live.Snapshot().Runtime.Tmux; block == nil || block.TakeOver != "" {
				t.Fatalf("the settled tmux block = %+v, want the launch key with no take-over: the daemon adopted nothing at boot", block)
			}
		})
	}
}

// A fixed member keeps the value boot settled whether or not either reading carries its block.
//
// `runtime: tmux` with no `runtime.tmux` block is what most deployments write, and
// `runtime.tmux.take_over` is fixed because it is read once at boot, as the daemon adopts the
// claims of the server it takes over (whyTakeOve). A pin that descends only into the blocks
// both readings carry pins nothing there: the file that adds the block to such a deployment
// settles a take-over the tmux runtime those launch keys rebuild then runs on for the whole of
// its life (internal/daemon's tmuxOptions), and the file that drops a block the boot took over
// on settles the take-over away. An absent block reads as its members' zero values on either
// side, so the member is pinned both ways: back to "" where the boot carried no block, and
// into a block of the new reading's own carrying that member and nothing else where the new
// reading carries none.
//
// The third row is the control: a reading that carries the block and leaves `take_over` as
// boot read it settles both launch keys and is refused nothing. So the two rows above are a
// member pinned inside a block that applied, and not a block refused.
func TestAFixedMemberIsPinnedWhenOnlyOneReadingCarriesItsBlock(t *testing.T) {
	const tookOver = "runtime:\n  tmux:\n    take_over: kubernetes\n    pane_env: {TOOLS_HOME: /opt/tools}\n    pi_config_files: [overlay.json]\n"
	for _, tc := range []struct {
		name, booted, reading string
		refused               []Refusal
		applied               []string
		takeOver              string
		paneEnv               map[string]string
	}{
		{
			name:    "the reading brings a block the boot had none of",
			booted:  minimalFile,
			reading: minimalFile + tookOver,
			refused: []Refusal{{Path: "runtime.tmux.take_over", Reason: whyTakeOve}},
			applied: []string{"runtime.tmux.pane_env", "runtime.tmux.pi_config_files"},
			paneEnv: map[string]string{"TOOLS_HOME": "/opt/tools"},
		},
		{
			name:     "the reading drops the block the boot took over on",
			booted:   minimalFile + tookOver,
			reading:  minimalFile,
			refused:  []Refusal{{Path: "runtime.tmux.take_over", Reason: whyTakeOve}},
			applied:  []string{"runtime.tmux.pane_env", "runtime.tmux.pi_config_files"},
			takeOver: TakeOverKubernetes,
		},
		{
			name:     "the reading carries the block and leaves take_over as boot read it",
			booted:   minimalFile + tookOver,
			reading:  minimalFile + "runtime:\n  tmux:\n    take_over: kubernetes\n    pane_env: {TOOLS_HOME: /opt/other}\n    pi_config_files: [other.json]\n",
			applied:  []string{"runtime.tmux.pane_env", "runtime.tmux.pi_config_files"},
			takeOver: TakeOverKubernetes,
			paneEnv:  map[string]string{"TOOLS_HOME": "/opt/other"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, path := newLive(t, tc.booted, nil)

			rewrite(t, path, tc.reading)
			applied, refused, err := live.Apply(SourceFile, "", nil)

			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !slices.Equal(refused, tc.refused) {
				t.Fatalf("the settle refused %+v, want exactly %+v", refused, tc.refused)
			}
			if paths := appliedPaths(applied); !slices.Equal(paths, tc.applied) {
				t.Fatalf("the settle applied %v, want %v: a pinned path is no change of the daemon's", paths, tc.applied)
			}
			block := live.Snapshot().Runtime.Tmux
			if block == nil {
				t.Fatalf("the settled runtime carries no tmux block: a reading that drops one whose fixed member boot set carries that member alone")
			}
			if block.TakeOver != tc.takeOver {
				t.Errorf("runtime.tmux.take_over = %q, want %q: the runtime a rebuild takes over on is the one boot read", block.TakeOver, tc.takeOver)
			}
			if !reflect.DeepEqual(block.PaneEnv, tc.paneEnv) {
				t.Errorf("runtime.tmux.pane_env = %v, want %v: the launch keys beside a pinned member are the reading's own", block.PaneEnv, tc.paneEnv)
			}
		})
	}
}

// appliedPaths is the paths one settle applied, sorted, which is what a test compares against
// the paths it names.
func appliedPaths(applied []Change) []string {
	paths := make([]string, 0, len(applied))
	for _, change := range applied {
		paths = append(paths, change.Path)
	}
	slices.Sort(paths)
	return paths
}

// Observers see the configuration the pointer now holds and the changes that got it there, in the
// order they registered.
func TestOnChangeObserversSeeTheNewConfigAndTheChanges(t *testing.T) {
	live, _ := newLive(t, minimalFile, nil)
	var order []string
	var seen []Change
	var capacity int
	live.OnChange(func(cfg *Config, applied []Change) {
		order = append(order, "first")
		seen = applied
		capacity = cfg.AdmissionCap
	})
	live.OnChange(func(cfg *Config, applied []Change) {
		order = append(order, "second")
		if cfg != live.Snapshot() {
			t.Error("an observer was handed a configuration other than the one the pointer holds")
		}
	})

	applied, _, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{"admission_cap": 11})

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Equal(order, []string{"first", "second"}) {
		t.Errorf("the observers ran %q, want them in registration order", order)
	}
	if capacity != 11 {
		t.Errorf("the observer read AdmissionCap = %d, want 11", capacity)
	}
	if !slices.Equal(seen, applied) {
		t.Errorf("the observer saw %+v, want the applied changes %+v", seen, applied)
	}
}

// An apply that changes nothing reports nothing, so a watcher that fires on an untouched file
// wakes no holder.
func TestAnApplyThatChangesNothingReportsNothing(t *testing.T) {
	live, _ := newLive(t, minimalFile, nil)
	live.OnChange(func(_ *Config, applied []Change) {
		if len(applied) != 0 {
			t.Errorf("the observer was told of %+v, want nothing", applied)
		}
	})

	applied, refused, err := live.Apply(SourceFile, "", nil)

	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 0 || len(refused) != 0 {
		t.Errorf("applied %+v and refused %+v, want neither", applied, refused)
	}
}

// A nested path the file does not carry is created, and one it carries is replaced, so the loader
// reads the two layers as one file.
func TestTheOverlayFillsAndReplacesNestedKeys(t *testing.T) {
	live, _ := newLive(t, strings.Replace(minimalFile, "gates:\n  design: root-issues\n", "", 1), nil)
	if steps := live.Snapshot().Gates.Steps(); !slices.Equal(steps, DesignGateRootIssues.Steps()) {
		t.Fatalf("Gates.Design = %v, want the default", steps)
	}

	if _, _, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{"gates.design": "off"}); err != nil {
		t.Fatalf("Apply into a block the file omits: %v", err)
	}
	if steps := live.Snapshot().Gates.Steps(); !slices.Equal(steps, DesignGateOff.Steps()) {
		t.Errorf("Gates.Design = %v, want off", steps)
	}
	if _, _, err := live.Apply(SourceRoute, "sami@theorem.dev", map[string]any{"gates.design": "root-issues"}); err != nil {
		t.Fatalf("Apply over the overlay's own value: %v", err)
	}
	if steps := live.Snapshot().Gates.Steps(); !slices.Equal(steps, DesignGateRootIssues.Steps()) {
		t.Errorf("Gates.Design = %v, want root-issues", steps)
	}
}

// Every path of the census is one the table holds a class for, so no change can be reported with
// no class at all.
func TestEveryCensusPathHasARow(t *testing.T) {
	live, _ := newLive(t, minimalFile, nil)
	for path := range settingsOf(live.Snapshot()) {
		if _, known := ClassOf(path); !known {
			t.Errorf("the census holds %s, which the class table does not", path)
		}
	}
}

// distinctByType is two different values of each type the walk below cannot build a pair of
// itself: a type whose fields are unexported, so only its own package can make one. Each is a
// type a `legion.yaml` key is read into, and each pair is two values an operator could write.
var distinctByType = map[reflect.Type][2]any{
	reflect.TypeFor[ghrepo.Repository](): {
		ghrepo.MustParse("sjawhar/legion-skills"),
		ghrepo.MustParse("acme/other-skills"),
	},
	reflect.TypeFor[ghrepo.Renames](): {
		ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")),
		ghrepo.MustRenames(ghrepo.MustRename("acme/widgets", "acme/gadgets")),
	},
}

// distinctValues is two values of typ that are not the same setting: the zero value with one
// leaf of it filled differently in each, which is the smallest move an operator can make in that
// key. A struct is varied in the first exported field a pair can be built of, a slice and a map
// in their one entry's value, and a pointer in what it points at; a type whose own fields are
// unexported is built only from distinctByType, since nothing outside its package can make one.
// seen holds the types the walk is already inside, so a type that reaches itself ends the walk
// rather than running forever.
func distinctValues(typ reflect.Type, seen []reflect.Type) (first, second reflect.Value, built bool) {
	if pair, named := distinctByType[typ]; named {
		return reflect.ValueOf(pair[0]), reflect.ValueOf(pair[1]), true
	}
	if slices.Contains(seen, typ) {
		return reflect.Value{}, reflect.Value{}, false
	}
	seen = append(seen, typ)
	scalar := func(a, b any) (reflect.Value, reflect.Value, bool) {
		return reflect.ValueOf(a).Convert(typ), reflect.ValueOf(b).Convert(typ), true
	}
	switch typ.Kind() {
	case reflect.Bool:
		return scalar(false, true)
	case reflect.String:
		return scalar("one", "two")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return scalar(int64(1), int64(2))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return scalar(uint64(1), uint64(2))
	case reflect.Float32, reflect.Float64:
		return scalar(1.0, 2.0)
	case reflect.Pointer:
		elemFirst, elemSecond, ok := distinctValues(typ.Elem(), seen)
		if !ok {
			break
		}
		first, second = reflect.New(typ.Elem()), reflect.New(typ.Elem())
		first.Elem().Set(elemFirst)
		second.Elem().Set(elemSecond)
		return first, second, true
	case reflect.Slice:
		elemFirst, elemSecond, ok := distinctValues(typ.Elem(), seen)
		if !ok {
			break
		}
		first, second = reflect.MakeSlice(typ, 1, 1), reflect.MakeSlice(typ, 1, 1)
		first.Index(0).Set(elemFirst)
		second.Index(0).Set(elemSecond)
		return first, second, true
	case reflect.Map:
		key, _, keyed := distinctValues(typ.Key(), seen)
		elemFirst, elemSecond, ok := distinctValues(typ.Elem(), seen)
		if !keyed || !ok {
			break
		}
		first, second = reflect.MakeMap(typ), reflect.MakeMap(typ)
		first.SetMapIndex(key, elemFirst)
		second.SetMapIndex(key, elemSecond)
		return first, second, true
	case reflect.Struct:
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			fieldFirst, fieldSecond, ok := distinctValues(field.Type, seen)
			if !ok {
				continue
			}
			first, second = reflect.New(typ).Elem(), reflect.New(typ).Elem()
			first.Field(i).Set(fieldFirst)
			second.Field(i).Set(fieldSecond)
			return first, second, true
		}
	}
	return reflect.Value{}, reflect.Value{}, false
}

// Every value the class table names renders differently for two different values. Render is the
// whole of what the daemon compares two readings of the settings by (settingsOf, diff) and the
// whole of what a pod's template records its launch keys as (sandbox.LaunchSettings), so a type
// it renders the same for two values is a key whose change is invisible: the route answers that
// nothing moved, no holder of the row is built again, and no pod reads as drifted. That is what
// a composite type with unexported fields did until ghrepo.Repository and ghrepo.Renames carried
// marshallers of their own — every repository, the zero one included, rendered as `{}`.
//
// A block row is left out: the diff descends into it and compares its members, so nothing ever
// renders the block itself.
func TestEveryRowRendersTwoDifferentValuesDifferently(t *testing.T) {
	named := map[reflect.Type]bool{}
	for _, typ := range settingsConfigTypes() {
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			row, classed := rowOf(typ.Name() + "." + field.Name)
			if !classed || row.Block {
				continue
			}
			if _, held := distinctByType[field.Type]; held {
				named[field.Type] = true
			}
			first, second, built := distinctValues(field.Type, nil)
			if !built {
				t.Errorf("%s (%s) is a setting this test cannot write two different values of: add the pair to distinctByType", row.Path, field.Type)
				continue
			}
			if rendered := Render(first); rendered == Render(second) {
				t.Errorf("%s renders %s for two different values (%s), so a change of it alone is no Change at all: give %s a marshaller that tells its values apart",
					row.Path, rendered, field.Type, field.Type)
			}
		}
	}
	for typ := range distinctByType {
		if !named[typ] {
			t.Errorf("distinctByType holds %s, which no settings row is read into: drop the pair", typ)
		}
	}
}
