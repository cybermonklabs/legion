package config

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

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
func TestABlockTheFileDropsReadsAsUnset(t *testing.T) {
	live, path := newLive(t, minimalFile+"runtime:\n  tmux:\n    pi_config_files: [overlay.json]\n", nil)
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
	if applied[index].New != UnsetValue {
		t.Errorf("the dropped path reads %q, want %q", applied[index].New, UnsetValue)
	}
}

// A file that swaps the whole `runtime` key settles neither half of the swap: the daemon keeps
// the runtime it booted on with every member of its block, and the edit is refused once, as
// `runtime`.
//
// The selector alone would be worse than nothing. `runtime` is one key carrying the selector
// and the block it names (readRuntime), so a settle that pinned the name and took the file's
// block would hold a configuration no loader produces: `kubernetes` beside no
// `runtime.kubernetes`, whose every member every reader of the block then reads as unset — and
// whose nil block the Sandbox build dereferences (internal/daemon's readSandbox), inside this
// apply, under the holder's lock, in a daemon supervising pods.
func TestAFileThatSwapsTheRuntimeKeyKeepsTheWholeBlockBootSettled(t *testing.T) {
	for _, tc := range []struct{ name, booted, swapped string }{
		{"kubernetes to tmux", kubernetesLiveFile, tmuxLiveFile},
		{"tmux to kubernetes", tmuxLiveFile, kubernetesLiveFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, path := newLive(t, tc.booted, nil)
			booted := live.Snapshot().Runtime

			rewrite(t, path, tc.swapped)
			applied, refused, err := live.Apply(SourceFile, "", nil)

			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			want := []Refusal{{Path: "runtime", Reason: whyRuntime}}
			if !slices.Equal(refused, want) {
				t.Fatalf("the settle refused %+v, want exactly %+v: the key the file swapped, named once", refused, want)
			}
			if settled := live.Snapshot().Runtime; !reflect.DeepEqual(settled, booted) {
				t.Fatalf("the settled runtime = %+v, want the whole of the boot's %+v: the daemon supervises that runtime's processes", settled, booted)
			}
			for _, change := range applied {
				if change.Path == "runtime" || strings.HasPrefix(change.Path, "runtime.") {
					t.Errorf("applied carries %+v, want no `runtime` path at all: a pinned path is no change of the daemon's", change)
				}
			}
		})
	}
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
