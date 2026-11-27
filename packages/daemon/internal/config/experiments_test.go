package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// projectBodyFile is minimalFile with body as DEMO's whole flow mapping and extra appended at the
// top level, so a case that is about two of the project's keys together can write both.
func projectBodyFile(body, extra string) string {
	return strings.Replace(minimalFile, "  DEMO: {}\n", "  DEMO: {"+body+"}\n", 1) + extra
}

// experimentsFile is projectBodyFile with block as DEMO's `experiments` value, so a case names
// only the experiments it is about.
func experimentsFile(block, extra string) string {
	return projectBodyFile("experiments: "+block, extra)
}

// kubernetesExperimentsFile is kubernetesFile with block as LEGSMOKE's `experiments` value and the
// rolePod mounts, which every role's pod and the image probe carry, and extra at the top level.
func kubernetesExperimentsFile(block, extra string) string {
	return strings.Replace(kubernetesFile, "  LEGSMOKE: {}\n", "  LEGSMOKE: {experiments: "+block+"}\n", 1) + rolePod + extra
}

// projects.<KEY>.experiments carries the experiments the file declares, in the file's order and
// with their variants in theirs; an absent mode is off, an absent unit is a tree and an absent
// weight is 1, so a list of plain variants is a fair coin. A variant that names neither overlays
// nor instructions is the control arm, which loads.
func TestProjectExperimentsCarryTheFileOrderAndTheVariants(t *testing.T) {
	block := "{alpha: {mode: off, variants: [{name: only}]}, " +
		"beta: {mode: random, unit: issue, variants: [{name: treated, weight: 3, config_files: [/etc/legion-operator/b.yml, /etc/legion-operator/c.yml], instructions: /etc/legion-operator/b.md}, {name: control}]}}"
	cfg, err := Load(writeConfigFile(t, experimentsFile(block, "")), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []Experiment{
		{
			Name:     "alpha",
			Mode:     ExperimentOff,
			Unit:     ExperimentTree,
			Variants: []Variant{{Name: "only", Weight: 1}},
		},
		{
			Name: "beta",
			Mode: ExperimentRandom,
			Unit: ExperimentIssue,
			Variants: []Variant{
				{
					Name:             "treated",
					Weight:           3,
					ConfigFiles:      []string{"/etc/legion-operator/b.yml", "/etc/legion-operator/c.yml"},
					InstructionsPath: "/etc/legion-operator/b.md",
				},
				{Name: "control", Weight: 1},
			},
		},
	}
	if got := cfg.Projects["DEMO"].Experiments; !reflect.DeepEqual(got, want) {
		t.Fatalf("Experiments = %+v, want %+v", got, want)
	}

	cfg, err = Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load without the key: %v", err)
	}
	if got := cfg.Projects["DEMO"].Experiments; got != nil {
		t.Errorf("Experiments without the key = %+v, want none", got)
	}
}

// A variant may name instructions and no overlay at all: the mode decides which units are drawn,
// and an arm that adds a system prompt and no settings overlay is a shape an experiment declares,
// which is why checkMemory's rule that a mode other than off must name config_files is the
// `memory` block's own and is asked of no variant.
func TestProjectExperimentsTakeAVariantWithInstructionsAndNoOverlay(t *testing.T) {
	block := "{alpha: {mode: off, variants: [{name: prompted, instructions: /etc/legion-operator/alpha.md}]}}"
	cfg, err := Load(writeConfigFile(t, experimentsFile(block, "")), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []Variant{{Name: "prompted", Weight: 1, InstructionsPath: "/etc/legion-operator/alpha.md"}}
	if got := cfg.Projects["DEMO"].Experiments[0].Variants; !reflect.DeepEqual(got, want) {
		t.Fatalf("Variants = %+v, want %+v", got, want)
	}
}

// `roles` carries the issue roles an experiment's arms reach, in the file's order, and a block that
// names none carries no roles, which is every role: Reaches answers both for a role inside and
// outside the list.
func TestProjectExperimentsTakeTheRolesTheirArmsReach(t *testing.T) {
	block := "{alpha: {mode: random, roles: [implementer, planner], variants: [{name: open, config_files: [/etc/legion-operator/open.yml]}, {name: control}]}, " +
		"beta: {mode: off, variants: [{name: only}]}}"
	cfg, err := Load(writeConfigFile(t, experimentsFile(block, "")), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	experiments := cfg.Projects["DEMO"].Experiments
	if want := []claim.Role{claim.RoleImplementer, claim.RolePlanner}; !reflect.DeepEqual(experiments[0].Roles, want) {
		t.Fatalf("alpha's Roles = %q, want %q", experiments[0].Roles, want)
	}
	if experiments[1].Roles != nil {
		t.Fatalf("beta's Roles = %q, want none, which is every role", experiments[1].Roles)
	}
	for _, tc := range []struct {
		experiment Experiment
		role       claim.Role
		want       bool
	}{
		{experiments[0], claim.RoleImplementer, true},
		{experiments[0], claim.RoleReviewer, false},
		{experiments[0], claim.RoleTester, false},
		{experiments[1], claim.RoleReviewer, true},
	} {
		if got := tc.experiment.Reaches(tc.role); got != tc.want {
			t.Errorf("%s.Reaches(%s) = %t, want %t", tc.experiment.Name, tc.role, got, tc.want)
		}
	}
}

// An arm's overlays need Oh My Pi only on the roles its experiment reaches: a reviewer on Claude
// Code beside an experiment that names only the implementer loads, since that reviewer never
// starts on the arm's overlays, and the same file is refused once the experiment names the
// reviewer, in words that name the fix its `roles` allows.
func TestProjectExperimentsAskOhMyPiOnlyOfTheRolesTheyReach(t *testing.T) {
	const extra = "worker_backends: {reviewer: claude}\n"
	reach := func(roles string) string {
		return "{alpha: {mode: random, roles: " + roles + ", variants: [{name: only, config_files: [/etc/a.yml]}, {name: other}]}}"
	}
	if _, err := Load(writeConfigFile(t, experimentsFile(reach("[implementer]"), extra)), noEnv); err != nil {
		t.Fatalf("Load with the Claude reviewer outside the experiment's roles = %v, want it to load", err)
	}
	want := "projects.DEMO.experiments.alpha.variants.only names config_files, and worker_backends.reviewer is claude, which reads no PI_CONFIG_FILES: put every role its roles names on omp, or take the overlays out of the variant"
	if _, err := Load(writeConfigFile(t, experimentsFile(reach("[implementer, reviewer]"), extra)), noEnv); err == nil || err.Error() != want {
		t.Fatalf("Load with the Claude reviewer among the experiment's roles = %v, want %q", err, want)
	}
}

// A project may declare the memory experiment under the key instead of the block, so long as its
// variants are the two the wire enum takes.
func TestProjectExperimentsTakeMemoryUnderTheKey(t *testing.T) {
	block := "{memory: {mode: random, on_variant: memory, variants: [{name: memory, config_files: [/etc/legion-operator/memory.yml], instructions: /etc/legion-operator/memory.md}, {name: none}]}}"
	cfg, err := Load(writeConfigFile(t, experimentsFile(block, "")), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []Experiment{{
		Name:      "memory",
		Mode:      ExperimentRandom,
		Unit:      ExperimentTree,
		OnVariant: "memory",
		Variants: []Variant{
			{
				Name:             "memory",
				Weight:           1,
				ConfigFiles:      []string{"/etc/legion-operator/memory.yml"},
				InstructionsPath: "/etc/legion-operator/memory.md",
			},
			{Name: "none", Weight: 1},
		},
	}}
	if got := cfg.Projects["DEMO"].Experiments; !reflect.DeepEqual(got, want) {
		t.Fatalf("Experiments = %+v, want %+v", got, want)
	}
}

// Every shape no unit could be drawn into is refused at load, naming the experiment, the variant
// and the key it is about: a mapping that is no mapping, a name outside the pattern or named
// twice, a mode or a unit that is no word of its own, a roles list that is no list, empty, names a
// role that is not an issue role or names one twice, a variants list that is missing, no list or
// empty, one variant where the mode draws, a weight that is not a whole number above zero, an
// overlay or an instructions path no role could load, a mode on that names no on_variant, an
// on_variant that is no variant of its experiment, and the four shapes of `experiments.memory`
// the name memory forbids: beside the block, naming `plan`, with other variants, and drawn for each
// issue. The other rules of `plan` are refused as well, each by a row of
// TestThePlanOfAVariantIsRefused.
func TestProjectExperimentsRefusals(t *testing.T) {
	cases := []struct{ name, block, body, extra, want string }{
		{name: "the key is not a mapping", block: "[alpha]",
			want: "projects.DEMO.experiments must be a mapping of an experiment's name to its block"},
		{name: "an experiment name outside the pattern", block: "{Alpha: {variants: [{name: only}]}}",
			want: `projects.DEMO.experiments key "Alpha" must match ^[a-z0-9-]{1,32}$: an experiment's name is recorded with every draw it makes`},
		{name: "an experiment name of 33 characters", block: "{" + strings.Repeat("a", 33) + ": {variants: [{name: only}]}}",
			want: `projects.DEMO.experiments key "` + strings.Repeat("a", 33) + `" must match ^[a-z0-9-]{1,32}$: an experiment's name is recorded with every draw it makes`},
		{name: "an empty experiment name", block: `{"": {variants: [{name: only}]}}`,
			want: `projects.DEMO.experiments key "" must match ^[a-z0-9-]{1,32}$: an experiment's name is recorded with every draw it makes`},
		{name: "an experiment named twice", block: "{alpha: {variants: [{name: only}]}, alpha: {variants: [{name: other}]}}",
			want: "projects.DEMO.experiments names alpha twice"},
		{name: "an experiment that is not a mapping", block: "{alpha: [only]}",
			want: "projects.DEMO.experiments.alpha must be a mapping"},
		{name: "an unknown key in the block", block: "{alpha: {group: on, variants: [{name: only}]}}",
			want: "unknown key projects.DEMO.experiments.alpha.group"},
		{name: "a key the block names twice", block: "{alpha: {mode: off, mode: on, variants: [{name: only}]}}",
			want: "projects.DEMO.experiments.alpha names mode twice"},
		{name: "a mode that is no word of the three", block: "{alpha: {mode: sometimes, variants: [{name: only}, {name: other}]}}",
			want: `projects.DEMO.experiments.alpha.mode must be off, on, or random (got "sometimes")`},
		{name: "a mode that is not a string", block: "{alpha: {mode: [on], variants: [{name: only}]}}",
			want: "projects.DEMO.experiments.alpha.mode must be a string"},
		{name: "a unit that is neither word", block: "{alpha: {unit: pod, variants: [{name: only}]}}",
			want: `projects.DEMO.experiments.alpha.unit must be tree or issue (got "pod")`},
		{name: "a unit that is not a string", block: "{alpha: {unit: [tree], variants: [{name: only}]}}",
			want: "projects.DEMO.experiments.alpha.unit must be a string"},
		{name: "roles that are no sequence", block: "{alpha: {roles: implementer, variants: [{name: only}]}}",
			want: "projects.DEMO.experiments.alpha.roles must be a sequence of Legion issue roles, e.g. [planner, implementer]"},
		{name: "an empty roles list", block: "{alpha: {roles: [], variants: [{name: only}]}}",
			want: "projects.DEMO.experiments.alpha.roles names no role: an experiment whose arms reach no launch treats nothing, so name the roles its arms reach, or leave the key out for every role"},
		{name: "the controller among the roles", block: "{alpha: {roles: [implementer, controller], variants: [{name: only}]}}",
			want: `projects.DEMO.experiments.alpha.roles names "controller", which is not a Legion issue role (architect, planner, implementer, tester, reviewer, merger)`},
		{name: "a role named twice", block: "{alpha: {roles: [implementer, implementer], variants: [{name: only}]}}",
			want: "projects.DEMO.experiments.alpha.roles names implementer twice"},
		{name: "no variants at all", block: "{alpha: {mode: off}}",
			want: "projects.DEMO.experiments.alpha.variants is required: an experiment draws among the variants its block lists"},
		{name: "variants that are no sequence", block: "{alpha: {variants: {name: only}}}",
			want: "projects.DEMO.experiments.alpha.variants must be a sequence of variant blocks"},
		{name: "an empty variants list", block: "{alpha: {variants: []}}",
			want: "projects.DEMO.experiments.alpha.variants lists no variant: an experiment draws among the variants its block lists, so give it at least one"},
		{name: "one variant where the mode draws", block: "{alpha: {mode: random, variants: [{name: only}]}}",
			want: "projects.DEMO.experiments.alpha is random and lists one variant: a draw among one arm measures nothing, so list the arm it is compared against, or set mode: off"},
		{name: "a variant that is not a mapping", block: "{alpha: {variants: [only]}}",
			want: "projects.DEMO.experiments.alpha.variants[0] must be a mapping"},
		{name: "a variant that names no name", block: "{alpha: {variants: [{weight: 2}]}}",
			want: "projects.DEMO.experiments.alpha.variants[0] names no name: a draw records the variant it drew by name"},
		{name: "a variant name outside the pattern", block: "{alpha: {variants: [{name: Only}]}}",
			want: `projects.DEMO.experiments.alpha.variants[0] name "Only" must match ^[a-z0-9-]{1,32}$: a draw records the variant it drew by name`},
		{name: "a variant name that is not a string", block: "{alpha: {variants: [{name: [only]}]}}",
			want: "projects.DEMO.experiments.alpha.variants[0].name must be a string"},
		{name: "a variant named twice", block: "{alpha: {variants: [{name: only}, {name: only}]}}",
			want: "projects.DEMO.experiments.alpha.variants names only twice"},
		{name: "an unknown key in a variant", block: "{alpha: {variants: [{name: only, group: on}]}}",
			want: "unknown key projects.DEMO.experiments.alpha.variants.only.group"},
		{name: "a weight of zero", block: "{alpha: {variants: [{name: only, weight: 0}, {name: other}]}}",
			want: "projects.DEMO.experiments.alpha.variants.only.weight must be a whole number above zero (got 0): a variant of no weight is never drawn, so take it out of the list instead"},
		{name: "a negative weight", block: "{alpha: {variants: [{name: only, weight: -2}, {name: other}]}}",
			want: "projects.DEMO.experiments.alpha.variants.only.weight must be a whole number above zero (got -2): a variant of no weight is never drawn, so take it out of the list instead"},
		{name: "a weight that is no whole number", block: "{alpha: {variants: [{name: only, weight: 1.5}]}}",
			want: "projects.DEMO.experiments.alpha.variants.only.weight must be an integer"},
		{name: "an empty overlay list", block: "{alpha: {variants: [{name: only, config_files: []}]}}",
			want: "projects.DEMO.experiments.alpha.variants.only.config_files names no overlay: give the variant at least one path, or leave it out"},
		{name: "a relative overlay", block: "{alpha: {variants: [{name: only, config_files: [memory.yml]}]}}",
			want: `projects.DEMO.experiments.alpha.variants.only.config_files entry "memory.yml" must be a clean absolute path: a role drawn into the variant's Oh My Pi reads it from wherever it starts`},
		{name: "an unclean overlay", block: "{alpha: {variants: [{name: only, config_files: [/etc/../etc/memory.yml]}]}}",
			want: `projects.DEMO.experiments.alpha.variants.only.config_files entry "/etc/../etc/memory.yml" must be a clean absolute path: a role drawn into the variant's Oh My Pi reads it from wherever it starts`},
		{name: "the separator in an overlay", block: "{alpha: {variants: [{name: only, config_files: [/etc/a:b.yml]}]}}",
			want: "projects.DEMO.experiments.alpha.variants.only.config_files entry \"/etc/a:b.yml\" holds `:`, which separates the overlays PI_CONFIG_FILES names"},
		{name: "a duplicate overlay", block: "{alpha: {variants: [{name: only, config_files: [/etc/a.yml, /etc/b.yml, /etc/a.yml]}]}}",
			want: "projects.DEMO.experiments.alpha.variants.only.config_files names /etc/a.yml twice"},
		{name: "a relative instructions path", block: "{alpha: {variants: [{name: only, instructions: memory.md}]}}",
			want: `projects.DEMO.experiments.alpha.variants.only.instructions "memory.md" must be a clean absolute path: the daemon reads it at boot from wherever it starts`},
		{name: "an unclean instructions path", block: "{alpha: {variants: [{name: only, instructions: /etc/../etc/memory.md}]}}",
			want: `projects.DEMO.experiments.alpha.variants.only.instructions "/etc/../etc/memory.md" must be a clean absolute path: the daemon reads it at boot from wherever it starts`},
		{name: "an instructions path that is not a string", block: "{alpha: {variants: [{name: only, instructions: [/etc/memory.md]}]}}",
			want: "projects.DEMO.experiments.alpha.variants.only.instructions must be a string"},
		{name: "a mode on that names no on_variant", block: "{alpha: {mode: on, variants: [{name: only}, {name: other}]}}",
			want: "projects.DEMO.experiments.alpha is on and names no on_variant: mode on gives every unit one variant, so name the one it gives (only, other)"},
		{name: "an on_variant that is no variant of its experiment", block: "{alpha: {mode: on, on_variant: third, variants: [{name: only}, {name: other}]}}",
			want: `projects.DEMO.experiments.alpha.on_variant names "third", which is no variant of the experiment (only, other)`},
		{name: "the memory block and experiments.memory together",
			body: "memory: {mode: random, config_files: [/etc/legion-operator/memory.yml]}, experiments: {memory: {variants: [{name: memory}, {name: none}]}}",
			want: "projects.DEMO.memory and projects.DEMO.experiments.memory are one experiment written twice: the memory block is the experiment named memory, so write the block or the key, not both"},
		{name: "experiments.memory with a third variant name", block: "{memory: {variants: [{name: memory}, {name: none}, {name: some}]}}",
			want: "projects.DEMO.experiments.memory names the variants memory, none, some: its variants must be exactly memory and none, since a draw of it is served to every pod as the issue state's memoryGroup, whose wire enum is those two words, and a third makes the whole body fail to parse"},
		{name: "experiments.memory with a variant of another name", block: "{memory: {variants: [{name: memory}, {name: nothing}]}}",
			want: "projects.DEMO.experiments.memory names the variants memory, nothing: its variants must be exactly memory and none, since a draw of it is served to every pod as the issue state's memoryGroup, whose wire enum is those two words, and a third makes the whole body fail to parse"},
		{name: "experiments.memory drawn for each issue", block: "{memory: {unit: issue, variants: [{name: memory}, {name: none}]}}",
			want: "projects.DEMO.experiments.memory is unit issue: its unit must be tree, since a draw of it is served to every pod as the issue state's memoryGroup, memoryDrawMode and memoryDrawnAt, which are the draw of the issue's tree, one answer for every issue of it"},
		// experiments.memory naming plan gets the one refusal of its own, whatever its unit, and
		// never the plan rules' advice to write unit: issue, which the memory rule would refuse next.
		{name: "experiments.memory naming plan, unit left out", block: "{memory: {mode: random, variants: [{name: memory, plan: owner}, {name: none, plan: default}]}}",
			want: "projects.DEMO.experiments.memory names plan: the memory experiment never sets how an issue is done, since a draw of it is the tree's memory group, so take plan out of its variants and set it on an experiment of its own"},
		{name: "experiments.memory naming plan, drawn for each issue", block: "{memory: {mode: random, unit: issue, variants: [{name: memory, plan: owner}, {name: none, plan: default}]}}",
			want: "projects.DEMO.experiments.memory names plan: the memory experiment never sets how an issue is done, since a draw of it is the tree's memory group, so take plan out of its variants and set it on an experiment of its own"},
		{name: "a variant's overlays beside a role on claude",
			block: "{alpha: {mode: random, variants: [{name: only, config_files: [/etc/a.yml]}, {name: other}]}}",
			extra: "worker_backends: {implementer: claude}\n",
			want:  "projects.DEMO.experiments.alpha.variants.only names config_files, and worker_backends.implementer is claude, which reads no PI_CONFIG_FILES: put every issue role on omp, or take the overlays out of the variant"},
	}
	t.Logf("refusal rows: %d", len(cases))
	if len(cases) < 24 {
		t.Fatalf("the refusal table has %d rows, want at least 24", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := experimentsFile(tc.block, tc.extra)
			if tc.body != "" {
				file = projectBodyFile(tc.body, tc.extra)
			}
			_, err := Load(writeConfigFile(t, file), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

// planningBlock declares the experiment `planning`, drawn for each issue, whose arm `a` takes the
// plan its issue's owner gives and whose arm `b` runs the project's default steps.
const planningBlock = "planning: {mode: random, unit: issue, variants: [{name: a, plan: owner}, {name: b, plan: default}]}"

// A variant names how the issues drawn into it are done (`plan`), and the declared list carries
// that word on each arm, so an experiment whose arms name it says it sets how an issue is done.
// The `memory` experiment, which every project declares and which names no plan, sets none. The
// fault caught is a loader that reads the key into nothing, which leaves both arms at "".
func TestAVariantNamesHowItsIssuesAreDone(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, experimentsFile("{"+planningBlock+"}", "")), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	declared := cfg.ExperimentsDeclared()
	if len(declared) != 2 || declared[0].Name != MemoryExperimentName || declared[1].Name != "planning" {
		t.Fatalf("declared experiments = %+v, want memory then planning", declared)
	}
	planning := declared[1]
	if got := planning.Variants[0].Plan; got != PlanGroupOwner {
		t.Errorf("variant a's plan = %q, want %q", got, PlanGroupOwner)
	}
	if got := planning.Variants[1].Plan; got != PlanGroupDefault {
		t.Errorf("variant b's plan = %q, want %q", got, PlanGroupDefault)
	}
	if !planning.SetsPlan() {
		t.Error("planning.SetsPlan() = false, want true: its arms name plan")
	}
	if declared[0].SetsPlan() {
		t.Error("memory.SetsPlan() = true, want false: no arm of it names plan")
	}
}

// Each rule of `plan` refuses its own fault with its own words: a word that is none of the three,
// an arm that names none beside an arm that does, an experiment that sets it and draws for a
// whole tree, two experiments of one project that both set it, and `owner-alone`, which this
// release does not carry out. Each row's control is its file with the fault taken out, which
// loads.
func TestThePlanOfAVariantIsRefused(t *testing.T) {
	cases := []struct{ name, block, fixed, want string }{
		{name: "a word that is none of the three",
			block: "{planning: {mode: random, unit: issue, variants: [{name: a, plan: alone}, {name: b, plan: default}]}}",
			fixed: "{planning: {mode: random, unit: issue, variants: [{name: a, plan: owner}, {name: b, plan: default}]}}",
			want:  `projects.DEMO.experiments.planning.variants.a.plan must be owner or default (got "alone")`},
		{name: "an arm that names no plan beside one that does",
			block: "{planning: {mode: random, unit: issue, variants: [{name: a, plan: owner}, {name: b}]}}",
			fixed: "{planning: {mode: random, unit: issue, variants: [{name: a, plan: owner}, {name: b, plan: default}]}}",
			want:  "projects.DEMO.experiments.planning.variants.b names no plan: each variant of an experiment that sets plan names how its issues are done"},
		{name: "an experiment that sets plan and draws for a tree",
			block: "{planning: {mode: random, variants: [{name: a, plan: owner}, {name: b, plan: default}]}}",
			fixed: "{planning: {mode: random, unit: issue, variants: [{name: a, plan: owner}, {name: b, plan: default}]}}",
			want:  "projects.DEMO.experiments.planning sets plan and is unit tree: an experiment that sets how an issue is done draws for each issue, so write unit: issue"},
		{name: "two experiments that set plan",
			block: "{" + planningBlock + ", steps: {mode: random, unit: issue, variants: [{name: x, plan: owner}, {name: y, plan: default}]}}",
			fixed: "{" + planningBlock + ", steps: {mode: random, unit: issue, variants: [{name: x}, {name: y}]}}",
			want:  "projects.DEMO.experiments.planning and projects.DEMO.experiments.steps both set plan: one experiment sets how an issue is done"},
		{name: "the owner alone",
			block: "{planning: {mode: random, unit: issue, variants: [{name: a, plan: owner}, {name: b, plan: owner-alone}]}}",
			fixed: "{planning: {mode: random, unit: issue, variants: [{name: a, plan: owner}, {name: b, plan: default}]}}",
			want:  "projects.DEMO.experiments.planning.variants.b.plan is owner-alone: the owner doing the whole issue itself in one step. This release of Legion does not carry it out yet."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, experimentsFile(tc.block, "")), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
		t.Run(tc.name+", taken out", func(t *testing.T) {
			if _, err := Load(writeConfigFile(t, experimentsFile(tc.fixed, "")), noEnv); err != nil {
				t.Fatalf("Load of the file with its fault taken out: %v", err)
			}
		})
	}
}

// A launch names one arm of each experiment it runs in, in the `legion.arms` attribute of its
// telemetry (runtime.SpendArmsValue), which the agent's detector takes up to 255 characters of and
// throws past, costing the launch every attribute it carries. So a project whose experiments, each
// at its longest arm's name, would name 255 characters loads, and one that would name 256 is
// refused. The memory experiment every project declares counts in both: `memory:memory` is 13 of
// them.
func TestProjectExperimentsHoldTheirArmsToTheTelemetryBound(t *testing.T) {
	var block strings.Builder
	block.WriteString("{")
	// Three experiments of 32-character names, each with a 32-character arm beside a short one:
	// 65 characters each and a comma before each, 198 after memory's 13.
	for _, letter := range []string{"a", "b", "c"} {
		name := strings.Repeat(letter, 32)
		block.WriteString(name + ": {variants: [{name: x}, {name: " + name + "}]}, ")
	}
	file := func(arm int) string {
		// A fourth of a 21-character name: a comma, the name, `:` and the arm, 23 + arm.
		return experimentsFile(block.String()+strings.Repeat("d", 21)+": {variants: [{name: "+strings.Repeat("e", arm)+"}]}}", "")
	}
	if _, err := Load(writeConfigFile(t, file(21)), noEnv); err != nil {
		t.Fatalf("Load of experiments whose longest arms make 255 characters: %v", err)
	}
	want := "projects.DEMO declares experiments whose arms one launch could name in legion.arms in 256 characters, over the 255 the agent's telemetry takes: shorten the experiments' or their variants' names, or declare fewer experiments"
	if _, err := Load(writeConfigFile(t, file(22)), noEnv); err == nil || err.Error() != want {
		t.Fatalf("Load error = %v, want %q", err, want)
	}
}

// Under kubernetes a variant's overlay must be a file one of the operator's mounts holds, since a
// role's pod and the image probe see only those and the image: a sub_path mount of the file, an
// item a directory mount of a ConfigMap projects, or any file under a whole Secret volume. A path
// no mount holds, a directory mount itself, a key the projection leaves out and a role on a
// harness that reads no overlay are refused at load, which is what `legion config check`
// runs — and the mode is off throughout, since a unit already drawn keeps the files its variant
// names.
func TestProjectExperimentsUnderKubernetesMustBeInTheOperatorsMounts(t *testing.T) {
	for _, tc := range []struct{ name, files, extra, want string }{
		{name: "the operator's own sub_path mount", files: "[/etc/legion-operator/overlay.yml]"},
		{name: "an item the directory mount projects", files: "[/etc/legion-route/roles/fallback.yml]"},
		{name: "a file under a whole secret", files: "[/etc/legion-creds/anything.yml]"},
		{name: "a file no mount holds", files: "[/etc/legion-operator/memory.yml]",
			want: "projects.LEGSMOKE.experiments.alpha.variants.treated.config_files entry /etc/legion-operator/memory.yml is under no runtime.kubernetes.pod.volume_mounts mount_path: a role's pod and the image probe's see only the image and the operator's mounts, so mount the ConfigMap that holds it there"},
		{name: "the directory mount itself", files: "[/etc/legion-route]",
			want: "projects.LEGSMOKE.experiments.alpha.variants.treated.config_files entry /etc/legion-route is the mount_path of volume operator-route, which mounts as a directory: name a file in it, or mount the file with sub_path"},
		{name: "a key the projection leaves out", files: "[/etc/legion-route/memory.yml]",
			want: "projects.LEGSMOKE.experiments.alpha.variants.treated.config_files entry /etc/legion-route/memory.yml is not among the items volume operator-route projects at /etc/legion-route (overlay.yml, roles/fallback.yml): add an item with path memory.yml"},
		{name: "a role on a harness that reads no overlay", files: "[/etc/legion-operator/overlay.yml]",
			extra: "worker_backends: {reviewer: claude}\n",
			want:  "projects.LEGSMOKE.experiments.alpha.variants.treated names config_files, and worker_backends.reviewer is claude, which reads no PI_CONFIG_FILES: put every issue role on omp, or take the overlays out of the variant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := "{alpha: {mode: off, variants: [{name: treated, config_files: " + tc.files + "}, {name: control}]}}"
			cfg, err := LoadForValidation(writeConfigFile(t, kubernetesExperimentsFile(block, tc.extra)), noEnv)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("LoadForValidation: %v", err)
			case tc.want == "" && len(cfg.Projects["LEGSMOKE"].Experiments[0].Variants[0].ConfigFiles) != 1:
				t.Fatalf("Variants[0].ConfigFiles = %v, want the variant's one file", cfg.Projects["LEGSMOKE"].Experiments[0].Variants[0].ConfigFiles)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}

// ExperimentsDeclared is the one list every reader below the loader asks: the `memory` block's
// experiment, which comes first because the block is one key rather than a member of the ordered
// mapping, then the `experiments` key's own in the file's order. Every project declares the memory
// experiment — one that writes neither the block nor `experiments.memory` declares it at mode off,
// the mode its trees have been drawn and recorded under all along — and the one project whose list
// holds no block's experiment is the one declaring `experiments.memory` itself, the other spelling
// of that one experiment, which then stands at its own place in the key's order. Each arm carries the
// key the operator wrote its overlays under, whichever spelling declared it: the block's arm the
// block's own `config_files`, an arm of the `experiments` key its own. The block's arms, and
// only theirs, are marked as the block's (Variant.MemoryBlock).
func TestProjectExperimentsDeclaredMapsTheMemoryBlockInFirst(t *testing.T) {
	memoryBlock := "{mode: on, config_files: [/etc/legion-operator/memory.yml], instructions: /etc/legion-operator/memory.md}"
	// blockField is the key the `memory` block's arm carries, which is the block's own
	// `config_files` and not the experiments key's; armField is an arm of the `experiments` key's.
	blockField := "projects.DEMO.memory.config_files"
	armField := func(experiment, variant string) string {
		return "projects.DEMO.experiments." + experiment + ".variants." + variant + ".config_files"
	}
	mapped := Experiment{
		Name:      "memory",
		Mode:      ExperimentOn,
		Unit:      ExperimentTree,
		OnVariant: "memory",
		Variants: []Variant{
			{
				Name:             "memory",
				Weight:           1,
				ConfigFiles:      []string{"/etc/legion-operator/memory.yml"},
				InstructionsPath: "/etc/legion-operator/memory.md",
				ConfigFilesField: blockField,
				MemoryBlock:      true,
			},
			{Name: "none", Weight: 1, MemoryBlock: true},
		},
	}
	// What a project that writes no `memory` block declares: the same experiment at mode off,
	// carrying no file, which draws every new tree at no arm and records it.
	memoryOff := Experiment{
		Name:      "memory",
		Mode:      ExperimentOff,
		Unit:      ExperimentTree,
		OnVariant: "memory",
		Variants:  []Variant{{Name: "memory", Weight: 1, ConfigFilesField: blockField, MemoryBlock: true}, {Name: "none", Weight: 1, MemoryBlock: true}},
	}
	// The key's own spelling of that experiment, which stands alone: the block's mapping is left
	// out so the project declares one memory experiment and not two. Its arms are the key's, so
	// they carry the key's own paths and not the block's.
	keyed := Experiment{
		Name: "memory",
		Mode: ExperimentRandom,
		Unit: ExperimentTree,
		Variants: []Variant{{Name: "memory", Weight: 1, ConfigFilesField: armField("memory", "memory")},
			{Name: "none", Weight: 1, ConfigFilesField: armField("memory", "none")}},
	}
	other := Experiment{
		Name: "other",
		Mode: ExperimentOff,
		Unit: ExperimentTree,
		Variants: []Variant{{Name: "treated", Weight: 1, ConfigFilesField: armField("other", "treated")},
			{Name: "control", Weight: 1, ConfigFilesField: armField("other", "control")}},
	}
	second := Experiment{
		Name:     "another",
		Mode:     ExperimentOff,
		Unit:     ExperimentIssue,
		Variants: []Variant{{Name: "only", Weight: 1, ConfigFilesField: armField("another", "only")}},
	}
	otherBlock := "{other: {variants: [{name: treated}, {name: control}]}, another: {unit: issue, variants: [{name: only}]}}"

	for _, tc := range []struct {
		name, body string
		want       []Experiment
	}{
		{"the block alone", "memory: " + memoryBlock, []Experiment{mapped}},
		{"the key alone", "experiments: " + otherBlock, []Experiment{memoryOff, other, second}},
		{"the block and the key together", "memory: " + memoryBlock + ", experiments: " + otherBlock, []Experiment{mapped, other, second}},
		{"neither", "", []Experiment{memoryOff}},
		{
			"the key's own memory, which the block is refused beside",
			"experiments: {memory: {mode: random, variants: [{name: memory}, {name: none}]}}",
			[]Experiment{keyed},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, projectBodyFile(tc.body, "")), noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Projects["DEMO"].ExperimentsDeclared("projects.DEMO"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExperimentsDeclared() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
