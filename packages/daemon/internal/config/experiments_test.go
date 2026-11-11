package config

import (
	"reflect"
	"strings"
	"testing"
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
// twice, a mode or a unit that is no word of its own, a variants list that is missing, no list or
// empty, one variant where the mode draws, a weight that is not a whole number above zero, an
// overlay or an instructions path no role could load, a mode on that names no on_variant, an
// on_variant that is no variant of its experiment, and the two shapes of `experiments.memory` that
// only the project as a whole shows.
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

// Under kubernetes a variant's overlay must be a file one of the operator's mounts holds, since a
// role's pod and the image probe see only those and the image: a sub_path mount of the file, an
// item a directory mount of a ConfigMap projects, or any file under a whole Secret volume. A path
// no mount holds, a directory mount itself, a key the projection leaves out and a role on a
// harness that reads no overlay are refused at load, which is what `legion start --check-config`
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

// ExperimentsDeclared is the one list every reader below the loader asks: the memory experiment,
// which comes first because it is one key rather than a member of the ordered mapping, then the
// `experiments` key's own in the file's order. Every project declares the memory experiment —
// one that writes no block declares it at mode off, the mode its trees have been drawn and
// recorded under all along — and the only project it is not added to is the one declaring
// `experiments.memory` itself, the other spelling of that one experiment.
func TestProjectExperimentsDeclaredMapsTheMemoryBlockInFirst(t *testing.T) {
	memoryBlock := "{mode: on, config_files: [/etc/legion-operator/memory.yml], instructions: /etc/legion-operator/memory.md}"
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
			},
			{Name: "none", Weight: 1},
		},
	}
	// What a project that writes no `memory` block declares: the same experiment at mode off,
	// carrying no file, which draws every new tree at no arm and records it.
	memoryOff := Experiment{
		Name:      "memory",
		Mode:      ExperimentOff,
		Unit:      ExperimentTree,
		OnVariant: "memory",
		Variants:  []Variant{{Name: "memory", Weight: 1}, {Name: "none", Weight: 1}},
	}
	// The key's own spelling of that experiment, which stands alone: the block's mapping is left
	// out so the project declares one memory experiment and not two.
	keyed := Experiment{
		Name:     "memory",
		Mode:     ExperimentRandom,
		Unit:     ExperimentTree,
		Variants: []Variant{{Name: "memory", Weight: 1}, {Name: "none", Weight: 1}},
	}
	other := Experiment{
		Name:     "other",
		Mode:     ExperimentOff,
		Unit:     ExperimentTree,
		Variants: []Variant{{Name: "treated", Weight: 1}, {Name: "control", Weight: 1}},
	}
	second := Experiment{
		Name:     "another",
		Mode:     ExperimentOff,
		Unit:     ExperimentIssue,
		Variants: []Variant{{Name: "only", Weight: 1}},
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
			if got := cfg.Projects["DEMO"].ExperimentsDeclared(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExperimentsDeclared() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
