package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// ExperimentMode is `projects.<KEY>.experiments.<name>.mode`: what the daemon draws for each unit
// the experiment covers. ExperimentOff draws for no unit, ExperimentOn gives every new unit the
// variant `on_variant` names, and ExperimentRandom draws among the variants by their weights. An
// experiment whose block names no mode is off, so an operator writes the variants first and turns
// the experiment on in one key, as a `memory` block moves from off to on in one key.
type ExperimentMode string

const (
	// ExperimentOff is the mode of an experiment whose block names none: the reader writes this
	// word into the field rather than leaving it empty, so every reader of a declared experiment
	// sees a mode an operator could have written and none needs a fourth name for the absent word.
	ExperimentOff    ExperimentMode = "off"
	ExperimentOn     ExperimentMode = "on"
	ExperimentRandom ExperimentMode = "random"
)

// experimentModes are the words `mode` takes, in the order its refusal names them.
var experimentModes = []ExperimentMode{ExperimentOff, ExperimentOn, ExperimentRandom}

// ExperimentUnit is `projects.<KEY>.experiments.<name>.unit`: what one draw covers, and so what
// the draw is recorded against. ExperimentTree draws once for a tree and gives every issue of that
// tree the same variant; ExperimentIssue draws for each issue, so one tree's issues may differ. An
// experiment whose block names no unit is a tree, the unit the `memory` block has always drawn for.
type ExperimentUnit string

const (
	// ExperimentTree is the unit of an experiment whose block names none, as the `memory` block,
	// which draws once for a tree, names none.
	ExperimentTree  ExperimentUnit = "tree"
	ExperimentIssue ExperimentUnit = "issue"
)

// experimentUnits are the words `unit` takes, in the order its refusal names them.
var experimentUnits = []ExperimentUnit{ExperimentTree, ExperimentIssue}

// experimentNamePattern is what an experiment's name and a variant's name must match: 1 to 32
// characters of a-z, 0-9 and `-`. A draw records both names, and a record is read back by a wire
// reader and written into a pod's environment, so the names stay short and stay in the characters
// every such reader takes.
var experimentNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Experiment is one `projects.<KEY>.experiments.<name>` block: something the daemon decides once
// per unit and records, so a later reading of the record says what that unit ran with. The block
// declares the arms; the mode decides whether a unit is drawn for at all.
type Experiment struct {
	// Name is the mapping key the experiment is declared under, and the name every draw of it
	// records; it is unique within the project, so a record names one experiment of it.
	Name string
	// Mode is `mode`: off draws for no unit, on gives every new unit OnVariant, and random draws
	// among Variants by their weights. A block that names no mode is ExperimentOff.
	Mode ExperimentMode
	// Unit is `unit`: what one draw covers and is recorded against, a tree or an issue. A block
	// that names no unit is ExperimentTree.
	Unit ExperimentUnit
	// OnVariant is `on_variant`: the variant mode on gives every unit, which that mode requires,
	// and which the other two modes may leave out. It is empty, or the name of a variant of this
	// experiment's own Variants.
	OnVariant string
	// Variants are the arms the experiment draws among, in the order the file lists them, each
	// named once. Order is kept because a draw's files are read in the file's order, and a weight
	// is read against its siblings' in that order.
	Variants []Variant
}

// Variant is one arm of an experiment: the name a draw records, the weight that draw gives the arm
// against its siblings, and the files a unit drawn into the arm runs with. An arm that names no
// file at all is the control a randomisation is measured against, which is why nothing here
// requires either file key.
type Variant struct {
	// Name is the variant's own name, unique within its experiment, and what a draw records as
	// the arm it drew.
	Name string
	// Weight is `weight`, the arm's share of a random draw against its siblings' weights: a whole
	// number above zero, 1 where the block names none, so a list of plain variants is a fair coin.
	Weight int
	// ConfigFiles are `config_files`, the Oh My Pi settings overlays every role of a unit drawn
	// into this arm names in PI_CONFIG_FILES, after the operator's own and the role's own
	// (role_config_files), in the file's order; nil where the arm names none.
	ConfigFiles []string
	// InstructionsPath is `instructions`, the one markdown file the daemon reads at boot and
	// appends to the system prompt of every role of a unit drawn into this arm, after the
	// deployment's own instructions; empty where the arm names none.
	InstructionsPath string
}

// memoryExperimentName is the name of the experiment the `memory` block is: the block and the key
// `experiments.memory` are two spellings of one experiment, which is why a project writing both is
// refused (checkExperimentMemory).
const memoryExperimentName = "memory"

// memoryVariantNames are the two variant names `experiments.memory` may declare, and the two the
// `memory` block maps to. They are fixed rather than the operator's to choose because a draw is
// served to every pod in the issue's state body as `memoryGroup`, whose wire enum is
// z.enum(["memory","none"]) (packages/contracts/src/legion-api.ts:477): a third word makes the
// whole body fail to parse (legion-api.test.ts:615-617), so a project declaring the experiment
// with other names would serve every pod of it a body the pod refuses.
var memoryVariantNames = []string{"memory", "none"}

// ExperimentsDeclared is every experiment the project declares, the `memory` block included, once
// the whole file has passed: the block mapped to the experiment named memory, then the
// `experiments` key's own in the file's order. Every reader of experiments below the loader asks
// this one list, so the block and the key behave alike there and a reader needs no second path for
// the block.
//
// The block's experiment comes first. It is one key rather than a member of the ordered mapping,
// so it has no file order against the mapping's members; first is chosen and fixed here so that
// the order is a contract a reader may rely on rather than an accident of how the file was read.
// The block maps to mode the block's own word (Memory.Word, so an absent mode is off), unit tree,
// on_variant memory, and the two variants memory (weight 1, carrying the block's config_files and
// instructions) and none (weight 1, carrying neither) — the group the daemon has always drawn.
//
// A project that writes neither the block nor the key declares nothing and gets nothing back, not
// a memory experiment at mode off: a deployment that declares no experiment must stay a deployment
// that declares no experiment, since a later reader records a draw for every experiment it is
// given.
//
// readMemory and checkMemory stay the block's own reader and checker, and this method is no part
// of either. Seven rows of the refusal table in the documentation quote their texts word for word,
// so the block keeps them; and checkMemory's rule that a mode other than off must name
// config_files contradicts the control variant an experiment may declare, which names no overlay
// at all, so it is the block's rule and not an experiment's.
func (project Project) ExperimentsDeclared() []Experiment {
	var declared []Experiment
	if memory := project.Memory; memory.Mode != "" || len(memory.ConfigFiles) > 0 || memory.InstructionsPath != "" {
		declared = append(declared, Experiment{
			Name:      memoryExperimentName,
			Mode:      ExperimentMode(memory.Word()),
			Unit:      ExperimentTree,
			OnVariant: memoryVariantNames[0],
			Variants: []Variant{
				{Name: memoryVariantNames[0], Weight: 1, ConfigFiles: memory.ConfigFiles, InstructionsPath: memory.InstructionsPath},
				{Name: memoryVariantNames[1], Weight: 1},
			},
		})
	}
	return append(declared, project.Experiments...)
}

// readExperiments is a project's `experiments`: a mapping of an experiment's name to its block,
// read in the file's order and kept in that order, since a later reader loads a drawn unit's files
// in the order the project's file declares the experiments and a mapping's iteration order is no
// contract. A name is 1 to 32 characters of a-z, 0-9 and `-` (experimentNamePattern) and is named
// once, because every draw records it.
func readExperiments(value *yaml.Node, key string) ([]Experiment, error) {
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of an experiment's name to its block", key)
	}
	// An `experiments: {}` declares no experiment, as an absent key does, so the field stays nil
	// and a reader of it needs no second shape for the empty mapping.
	if len(value.Content) == 0 {
		return nil, nil
	}
	experiments := make([]Experiment, 0, len(value.Content)/2)
	for index := 0; index+1 < len(value.Content); index += 2 {
		keyNode, blockNode := value.Content[index], value.Content[index+1]
		name := keyNode.Value
		if keyNode.Kind != yaml.ScalarNode || !experimentNamePattern.MatchString(name) {
			return nil, fmt.Errorf("%s key %q must match %s: an experiment's name is recorded with every draw it makes", key, name, experimentNamePattern)
		}
		if slices.ContainsFunc(experiments, func(e Experiment) bool { return e.Name == name }) {
			return nil, fmt.Errorf("%s names %s twice", key, name)
		}
		experiment, err := readExperiment(blockNode, key+"."+name, name)
		if err != nil {
			return nil, err
		}
		experiments = append(experiments, experiment)
	}
	return experiments, nil
}

// readExperiment is one experiment's block: `mode`, one of the three words (absent is off);
// `unit`, a tree or an issue (absent is a tree); `variants`, the arms it draws among, which it
// must have; and `on_variant`, the arm mode on gives every unit, which that mode requires and
// which must name an arm of this experiment. A mode other than off over one variant is refused:
// a draw among one arm measures nothing, so the file is missing the arm it compares against.
func readExperiment(value *yaml.Node, key, name string) (Experiment, error) {
	if value.Kind != yaml.MappingNode {
		return Experiment{}, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := describedMembers(value, key, "projects.<KEY>.experiments.<NAME>")
	if err != nil {
		return Experiment{}, err
	}
	experiment := Experiment{Name: name, Mode: ExperimentOff, Unit: ExperimentTree}
	mode, err := stringOf(fields["mode"], key+".mode")
	if err != nil {
		return Experiment{}, err
	}
	if mode != "" {
		if experiment.Mode = ExperimentMode(mode); !slices.Contains(experimentModes, experiment.Mode) {
			return Experiment{}, fmt.Errorf("%s.mode must be off, on, or random (got %q)", key, mode)
		}
	}
	unit, err := stringOf(fields["unit"], key+".unit")
	if err != nil {
		return Experiment{}, err
	}
	if unit != "" {
		if experiment.Unit = ExperimentUnit(unit); !slices.Contains(experimentUnits, experiment.Unit) {
			return Experiment{}, fmt.Errorf("%s.unit must be tree or issue (got %q)", key, unit)
		}
	}
	if experiment.OnVariant, err = stringOf(fields["on_variant"], key+".on_variant"); err != nil {
		return Experiment{}, err
	}
	variants := fields["variants"]
	if variants == nil {
		return Experiment{}, fmt.Errorf("%s.variants is required: an experiment draws among the variants its block lists", key)
	}
	if experiment.Variants, err = readVariants(variants, key+".variants"); err != nil {
		return Experiment{}, err
	}
	if experiment.Mode != ExperimentOff && len(experiment.Variants) == 1 {
		return Experiment{}, fmt.Errorf("%s is %s and lists one variant: a draw among one arm measures nothing, so list the arm it is compared against, or set mode: off", key, experiment.Mode)
	}
	names := variantNames(experiment.Variants)
	switch {
	case experiment.Mode == ExperimentOn && experiment.OnVariant == "":
		return Experiment{}, fmt.Errorf("%s is on and names no on_variant: mode on gives every unit one variant, so name the one it gives (%s)", key, strings.Join(names, ", "))
	case experiment.OnVariant != "" && !slices.Contains(names, experiment.OnVariant):
		return Experiment{}, fmt.Errorf("%s.on_variant names %q, which is no variant of the experiment (%s)", key, experiment.OnVariant, strings.Join(names, ", "))
	}
	return experiment, nil
}

// readVariants is an experiment's `variants`: a sequence of at least one arm, each a mapping with
// a `name` the experiment names once, an optional `weight` (a whole number above zero, 1 where the
// arm names none), optional `config_files` read as `role_config_files`' lists are
// (readOverlayFiles), and an optional `instructions` path the daemon reads at boot. Neither file
// key is required: the arm that names no file is the control the others are measured against.
func readVariants(value *yaml.Node, key string) ([]Variant, error) {
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be a sequence of variant blocks", key)
	}
	if len(value.Content) == 0 {
		return nil, fmt.Errorf("%s lists no variant: an experiment draws among the variants its block lists, so give it at least one", key)
	}
	variants := make([]Variant, 0, len(value.Content))
	for index, entry := range value.Content {
		field := variantKey(key, index, entry)
		if entry.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping", field)
		}
		fields, err := describedMembers(entry, field, "projects.<KEY>.experiments.<NAME>.variants[]")
		if err != nil {
			return nil, err
		}
		variant := Variant{Weight: 1}
		if variant.Name, err = stringOf(fields["name"], field+".name"); err != nil {
			return nil, err
		}
		switch {
		case variant.Name == "":
			return nil, fmt.Errorf("%s names no name: a draw records the variant it drew by name", field)
		case !experimentNamePattern.MatchString(variant.Name):
			return nil, fmt.Errorf("%s name %q must match %s: a draw records the variant it drew by name", field, variant.Name, experimentNamePattern)
		case slices.Contains(variantNames(variants), variant.Name):
			return nil, fmt.Errorf("%s names %s twice", key, variant.Name)
		}
		if weight := fields["weight"]; weight != nil {
			read, err := readInt(weight, field+".weight")
			if err != nil {
				return nil, err
			}
			if *read < 1 {
				return nil, fmt.Errorf("%s.weight must be a whole number above zero (got %d): a variant of no weight is never drawn, so take it out of the list instead", field, *read)
			}
			variant.Weight = *read
		}
		if files := fields["config_files"]; files != nil {
			// Read the way `role_config_files` and a `memory` block's lists are: the overlays are
			// the variant's, and the Oh My Pi of a role in a unit drawn into it loads them.
			if variant.ConfigFiles, err = readOverlayFiles(files, field+".config_files", "a role drawn into the variant's Oh My Pi", "the variant"); err != nil {
				return nil, err
			}
		}
		if instructions := fields["instructions"]; instructions != nil {
			instructionsField := field + ".instructions"
			if variant.InstructionsPath, err = stringOf(instructions, instructionsField); err != nil {
				return nil, err
			}
			if !filepath.IsAbs(variant.InstructionsPath) || filepath.Clean(variant.InstructionsPath) != variant.InstructionsPath {
				return nil, fmt.Errorf("%s %q must be a clean absolute path: the daemon reads it at boot from wherever it starts", instructionsField, variant.InstructionsPath)
			}
		}
		variants = append(variants, variant)
	}
	return variants, nil
}

// variantKey is the field name a variant's refusals carry: the variant's own name where its block
// names one the rules take, and its place in the list where it does not, since an arm whose name
// is missing or malformed has nothing else an operator could find it by.
func variantKey(key string, index int, value *yaml.Node) string {
	for i := 0; i+1 < len(value.Content); i += 2 {
		if value.Content[i].Value != "name" {
			continue
		}
		if name := value.Content[i+1].Value; experimentNamePattern.MatchString(name) {
			return key + "." + name
		}
		break
	}
	return fmt.Sprintf("%s[%d]", key, index)
}

// variantNames are the variants' names, in their order.
func variantNames(variants []Variant) []string {
	names := make([]string, 0, len(variants))
	for _, variant := range variants {
		names = append(names, variant.Name)
	}
	return names
}

// checkExperimentMemory refuses the two shapes of `experiments.memory` that only the project as a
// whole shows, which is why they are read here rather than in readExperiments: a project that
// writes the `memory` block and declares the experiment too, since the two are one experiment
// spelled twice and the daemon would draw for it twice; and an `experiments.memory` whose variants
// are not exactly memory and none, which would serve every pod of the project a state body it
// refuses (memoryVariantNames). Every other name of the key declares whatever variants it likes.
func checkExperimentMemory(experiments []Experiment, blockWritten bool, key string) error {
	index := slices.IndexFunc(experiments, func(e Experiment) bool { return e.Name == memoryExperimentName })
	if index < 0 {
		return nil
	}
	if blockWritten {
		return fmt.Errorf("%s.memory and %s.experiments.memory are one experiment written twice: the memory block is the experiment named memory, so write the block or the key, not both", key, key)
	}
	names := variantNames(experiments[index].Variants)
	if !slices.Equal(slices.Sorted(slices.Values(names)), memoryVariantNames) {
		return fmt.Errorf("%s.experiments.memory names the variants %s: its variants must be exactly %s, since a draw of it is served to every pod as the issue state's memoryGroup, whose wire enum is those two words, and a third makes the whole body fail to parse",
			key, strings.Join(names, ", "), strings.Join(memoryVariantNames, " and "))
	}
	return nil
}

// checkExperiments refuses what a project's declared variants name that no role could load as
// named, once the rest of the file is settled: a variant whose overlays an issue role could not
// read, because worker_backends puts that role on a harness other than Oh My Pi, which reads no
// PI_CONFIG_FILES; and, under runtime kubernetes, an overlay outside every runtime.kubernetes.pod
// mount (checkRoleConfigMount), since a pod sees only the image and the operator's mounts, and a
// file the image ships is no operator's.
//
// Both hold whatever the mode is, `off` included, for the reason checkMemory's own two do: the
// mode decides which new units are drawn, and a unit already drawn keeps the files of the variant
// it drew while that variant names them, so an overlay no role could load would stop that unit's
// roles under any mode. Taking the files away is removing them from the variant, not moving the
// mode. checkMemory's third rule, that a mode other than off must name config_files, is the
// `memory` block's own and is not asked of a variant: an experiment's control arm names no overlay
// by design, and an arm may name instructions alone.
//
// The `memory` block's own files are checkMemory's, not read here: this walks the `experiments`
// key's experiments, so the block keeps the refusals the documentation quotes.
func checkExperiments(cfg Config) error {
	for _, name := range slices.Sorted(maps.Keys(cfg.Projects)) {
		for _, experiment := range cfg.Projects[name].Experiments {
			for _, variant := range experiment.Variants {
				if len(variant.ConfigFiles) == 0 {
					continue
				}
				field := "projects." + name + ".experiments." + experiment.Name + ".variants." + variant.Name
				for _, role := range claim.Roles {
					if backend := cfg.BackendFor(role); backend != runtime.BackendOmp {
						return fmt.Errorf("%s names config_files, and worker_backends.%s is %s, which reads no PI_CONFIG_FILES: put every issue role on %s, or take the overlays out of the variant", field, role, backend, runtime.BackendOmp)
					}
				}
				if k := cfg.Runtime.Kubernetes; k != nil {
					for _, file := range variant.ConfigFiles {
						if err := checkRoleConfigMount(k.Pod, field+".config_files", file); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}
