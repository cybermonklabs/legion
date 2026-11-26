package config

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// ExperimentMode is `projects.<KEY>.experiments.<name>.mode`: what the daemon draws for each unit
// the experiment covers. ExperimentOff gives every new unit no arm, ExperimentOn gives every
// new unit the variant `on_variant` names, and ExperimentRandom draws among the variants by
// their weights. An experiment whose block names no mode is off, so an operator writes the
// variants first and turns the experiment on in one key, as a `memory` block moves from off to
// on in one key.
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

// PlanGroup is `projects.<KEY>.experiments.<name>.variants[].plan`: how the issues drawn into an
// arm are done. PlanGroupOwner runs an issue as an issue of no experiment runs: its owner gives
// the plan with register_plan, and an issue whose owner gives none runs the project's default
// steps. PlanGroupDefault runs the project's default steps (Project.RunsByDefault), and Legion
// takes no plan from the issue's owner. PlanGroupOwnerAlone is the owner doing the whole issue
// itself in one step, which this release refuses at load. The zero value is an arm that names no
// plan, whose issues run as with no experiment.
type PlanGroup string

const (
	PlanGroupOwner      PlanGroup = "owner"
	PlanGroupDefault    PlanGroup = "default"
	PlanGroupOwnerAlone PlanGroup = "owner-alone"
)

// planGroups are the words `plan` takes, in the order its refusal names them.
var planGroups = []PlanGroup{PlanGroupOwner, PlanGroupDefault, PlanGroupOwnerAlone}

// experimentNamePattern is what an experiment's name and a variant's name must match: 1 to 32
// characters of a-z, 0-9 and `-`. A draw records both names, which the state's wire carries back
// (api.Issue.Assignments), which name the directory and the file of an `experiments` arm's
// instructions copy under the state directory (VariantInstructionsFile), and which head that copy
// in every prompt it reaches (WriteVariantInstructions), so the names stay short and stay in
// characters a path segment and every such reader take: neither holds `/`. One environment
// variable carries both, as `<experiment>:<variant>` in the `legion.arms` attribute of a launch's
// OTEL_RESOURCE_ATTRIBUTES (runtime.SpendArmsValue), which is why neither holds `:` or `,`.
var experimentNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Experiment is one `projects.<KEY>.experiments.<name>` block: something the daemon decides once
// per unit and records, so a later reading of the record says what that unit ran with. The block
// declares the arms; the mode decides whether a unit is drawn for at all.
type Experiment struct {
	// Name is the mapping key the experiment is declared under, and the name every draw of it
	// records; it is unique within the project, so a record names one experiment of it.
	Name string
	// Mode is `mode`: off gives every new unit no arm, on gives every new unit OnVariant, and random
	// draws among Variants by their weights. A block that names no mode is ExperimentOff.
	Mode ExperimentMode
	// Unit is `unit`: what one draw covers and is recorded against, a tree or an issue. A block
	// that names no unit is ExperimentTree.
	Unit ExperimentUnit
	// Roles is `roles`, the Legion issue roles whose launches load this experiment's arms, in the
	// file's order. A launch of a role the list leaves out loads none of this experiment's
	// overlays or instructions, while its unit keeps the arm it was drawn into; the operator's and
	// the role's own overlays, and other experiments' arms that reach the role, still apply. So an
	// experiment can leave the roles that judge the work (the reviewer, the tester, the architect
	// who signs a merge off) out of its arms, and read their verdicts as its outcome. Nil where
	// the block names none, which is every issue role, as before the key existed (Reaches).
	Roles []claim.Role
	// OnVariant is `on_variant`: the variant mode on gives every unit, which that mode requires,
	// and which the other two modes may leave out. It is empty, or the name of a variant of this
	// experiment's own Variants.
	OnVariant string
	// Variants are the arms the experiment draws among, in the order the file lists them, each
	// named once. Order is kept because a draw's files are read in the file's order, and a weight
	// is read against its siblings' in that order.
	Variants []Variant
}

// Reaches is whether a launch of role loads this experiment's arms: every issue role where the
// block names no `roles`, and only the roles it names where it names some. The controller runs
// in no experiment whatever this answers, since an experiment's unit is a tree or an issue of one
// (internal/daemon's controllerSpawnSpec).
func (experiment Experiment) Reaches(role claim.Role) bool {
	return experiment.Roles == nil || slices.Contains(experiment.Roles, role)
}

// Variant is one arm of an experiment: the name a draw records, the weight that draw gives the arm
// against its siblings, and the files a unit drawn into the arm runs with. An arm that names no
// file at all is the control a randomisation is measured against, which is why nothing here
// requires any file key.
type Variant struct {
	// Name is the variant's own name, unique within its experiment, and what a draw records as
	// the arm it drew.
	Name string
	// Weight is `weight`, the arm's share of a random draw against its siblings' weights: a whole
	// number above zero, 1 where the block names none, so a list of plain variants is a fair coin.
	Weight int
	// ConfigFiles are `config_files`, the Oh My Pi settings overlays each role the experiment
	// reaches (Experiment.Reaches) of a unit drawn into this arm names in PI_CONFIG_FILES, after
	// the operator's own and the role's own (role_config_files), in the file's order; nil where
	// the arm names none.
	ConfigFiles []string
	// InstructionsPath is `instructions`, the one markdown file the daemon reads at boot, and
	// again at each launch, and appends to the system prompt of each role the experiment reaches
	// of a unit drawn into this arm, after the deployment's own instructions; empty where the arm
	// names none.
	InstructionsPath string
	// TextsPath is `texts`, a folder whose files mirror the paths of Legion's texts (the role
	// prompt parts, the agents and the skills) beside one manifest.json, which the daemon reads at
	// boot, and again at each launch: each role the experiment reaches of a unit drawn into this
	// arm reads the folder's text in place of Legion's wherever the folder holds one; empty where
	// the arm names none. The loader refuses it where the experiment reaches a role on a harness
	// other than Oh My Pi, outside the operator's pod mounts under kubernetes, and where another
	// experiment gives a folder to a role this one reaches too (checkTextsArms).
	TextsPath string
	// ConfigFilesField is the key the operator wrote ConfigFiles under, whole:
	// `projects.<KEY>.memory.config_files` for the arm the `memory` block declares, and
	// `projects.<KEY>.experiments.<name>.variants.<arm>.config_files` for an arm of the
	// `experiments` key. It is what a refusal and a pod's drift name the arm's overlays by, so
	// the one spelling the operator can find in the file is carried from here rather than
	// rebuilt, differently, by each reader.
	//
	// Project.ExperimentsDeclared fills it, which is where the project key it begins with is
	// known, so it stands on the arms of that list and on none of the arms Project.Experiments
	// holds as the file was read.
	ConfigFilesField string
	// MemoryBlock is whether the arm is one the project's `memory` block declares rather than one
	// of the `experiments` key, `experiments.memory`'s included. The block's arm keeps what the
	// block had before any experiment was declared: its refusals' words and its instructions
	// copy's name and header (VariantInstructionsFile, WriteVariantInstructions). Like
	// ConfigFilesField, Project.ExperimentsDeclared sets it as it builds the list, so a reader of
	// the declared list takes it from the arm rather than asking Project.MemoryIsBlock again.
	MemoryBlock bool
	// Plan is `plan`: how the issues drawn into this arm are done, or "" where the arm names
	// none. A draw records it beside the arm (record.Assignment.Plan), and the issue keeps that
	// word for its whole life, whatever the file says later.
	Plan PlanGroup
}

// TextsField is the key the operator wrote TextsPath under, whole:
// `projects.<KEY>.experiments.<name>.variants.<arm>.texts`, which a refusal of the folder names
// the arm by. It is the arm's ConfigFilesField with its last element changed, so it stands on the
// arms of the declared list alone, as that key does (Project.ExperimentsDeclared).
func (variant Variant) TextsField() string {
	return strings.TrimSuffix(variant.ConfigFilesField, ".config_files") + ".texts"
}

// MemoryExperimentName is the name of the experiment the `memory` block is: the block and the key
// `experiments.memory` are two spellings of one experiment, which is why a project writing both is
// refused (checkExperimentMemory). It is exported because the one reader of that one experiment
// — internal/projection, which reads an issue's three memory fields off its row — must ask for
// it by this name and no other.
const MemoryExperimentName = "memory"

// MemoryVariantMemory and MemoryVariantNone are the two arms of the experiment named memory: the
// arm whose roles run with the project's memory overlays and its instructions, and the control
// that names neither. They are fixed rather than the operator's to choose because a draw is
// served to every pod in the issue's state body as `memoryGroup`, whose wire enum is
// z.enum(["memory","none"]) (packages/contracts/src/legion-api.ts, legionIssue's memoryGroup): a
// third word makes the whole body fail to parse (legion-api.test.ts, "the state golden carries
// each issue's tree memory draw", which refuses memoryGroup "both"), so a project declaring the
// experiment with other names would serve every pod of it a body the pod refuses. They are
// exported for the same reason the name above is: the one reader of this one experiment asks for
// each arm by its name and no other.
const (
	MemoryVariantMemory = "memory"
	MemoryVariantNone   = "none"
)

// MemoryVariantNames are those two arms as `experiments.memory` must declare them and as the
// `memory` block maps to them, the arm carrying the files first and the control second.
var MemoryVariantNames = []string{MemoryVariantMemory, MemoryVariantNone}

// ExperimentsDeclared is every experiment the project declares, the `memory` block included: the
// block's experiment, named memory, then the `experiments` key's own in the file's order. Every
// reader of experiments below the loader asks this one list, so the block and the key behave
// alike there and a reader needs no second path for the block. key is the project's own path,
// `projects.<KEY>`, which each arm's ConfigFilesField begins with; Config.ExperimentsDeclared
// names that key for the project a daemon serves, so no reader of a loaded file spells it out.
//
// It is a function of the `memory` block and the `experiments` key and of nothing else, so a
// project built by hand — the settings' own (api.ProjectSettings.DeclaredExperiments), and a
// test's — declares what the same file would. The list is built at each call rather than kept
// on the project: every caller is a boot read or a command's, so building it again costs nothing
// that matters, and one place builds both the list and each arm's key path
// (Variant.ConfigFilesField), which is then the one the operator wrote rather than one each
// reader works out again.
//
// The block's experiment comes first. The block is one key rather than a member of the ordered
// mapping, so it has no file order against the mapping's members; first is chosen and fixed here
// so that the order is a contract a reader may rely on rather than an accident of how the file was
// read.
// It is mode the block's own word (Memory.Word, so a block that names no mode and a project that
// writes no block alike are off), unit tree, on_variant memory, and the two variants memory
// (weight 1, carrying the block's config_files and instructions) and none (weight 1, carrying
// neither) — the group the daemon has always drawn.
//
// Every project declares it, whatever it writes: a project that writes neither the block nor
// experiments.memory declares it at mode off, and one that writes experiments.memory declares it
// at that key's own mode (the paragraph after this one). For the first, the mode-off draw records
// each new tree at no variant, which is the record the daemon has
// written for every tree of such a project since the memory draw existed — the draw ran at every
// tree's first lifecycle row whatever the file said, and the empty mode read back as the word
// `off` — and the three memory fields a reader asks of such a tree answer off that row
// (internal/projection's memoryOf: `memory_group` none, `memory_draw_mode` off and the instant of
// the draw). Declaring it for such a project is what keeps those three answers where they are;
// leaving it out would empty them for every tree opened after this release while every older
// tree went on answering them.
//
// The one project it is not added to is the one declaring `experiments.memory` itself, which is
// the same experiment spelled the other way: the loader refuses that key beside the block
// (checkExperimentMemory), so the two spellings never both appear, and the key's own block is the
// whole declaration. That experiment is one of the key's own, so it stands at its own place in the
// file's order, wherever the operator wrote it in the mapping.
//
// Each arm is copied rather than shared: the key path is put on the copy, so Project.Experiments
// stays the file as it was read.
//
// readMemory and checkMemory stay the block's own reader and checker, and this method is no part
// of either. Seven rows of the refusal table in the documentation quote their texts word for word,
// so the block keeps them; and checkMemory's rule that a mode other than off must name
// config_files contradicts the control variant an experiment may declare, which names no overlay
// at all, so it is the block's rule and not an experiment's.
func (project Project) ExperimentsDeclared(key string) []Experiment {
	declared := make([]Experiment, 0, len(project.Experiments)+1)
	if project.MemoryIsBlock() {
		memory := project.Memory
		declared = append(declared, Experiment{
			Name:      MemoryExperimentName,
			Mode:      memory.Word(),
			Unit:      ExperimentTree,
			OnVariant: MemoryVariantMemory,
			Variants: []Variant{
				{Name: MemoryVariantMemory, Weight: 1, ConfigFiles: memory.ConfigFiles,
					InstructionsPath: memory.InstructionsPath, ConfigFilesField: key + ".memory.config_files", MemoryBlock: true},
				{Name: MemoryVariantNone, Weight: 1, MemoryBlock: true},
			},
		})
	}
	for _, experiment := range project.Experiments {
		variants := make([]Variant, len(experiment.Variants))
		for i, variant := range experiment.Variants {
			variant.ConfigFilesField = key + ".experiments." + experiment.Name + ".variants." + variant.Name + ".config_files"
			variants[i] = variant
		}
		experiment.Variants = variants
		declared = append(declared, experiment)
	}
	return declared
}

// MemoryIsBlock is whether the project's experiment named memory is its `memory` block: true
// unless the project declares `experiments.memory`, which the loader refuses beside a written
// block (checkExperimentMemory). A project that writes neither declares the experiment from the
// empty block (ExperimentsDeclared). The block's arm keeps the block's own refusals, checkMemory's
// and ReadMemoryInstructions', so a reader walking the declared list asks this to tell that arm
// from an arm of the `experiments` key.
func (project Project) MemoryIsBlock() bool {
	return !slices.ContainsFunc(project.Experiments, func(experiment Experiment) bool {
		return experiment.Name == MemoryExperimentName
	})
}

// SetsPlan is whether the experiment sets how its issues are done: true where any arm names
// `plan`. The loader holds such an experiment to three rules (checkExperimentPlans,
// checkExperiments): every arm of it names `plan`, it draws for each issue, and no other experiment
// of its project sets `plan`, so an issue holds at most one draw that says how it is done.
func (experiment Experiment) SetsPlan() bool {
	return slices.ContainsFunc(experiment.Variants, func(variant Variant) bool { return variant.Plan != "" })
}

// ExperimentsDeclared is the experiments the project this daemon serves declares, under the key
// that project is written at (Project.ExperimentsDeclared): the one list every reader of a
// loaded file asks, which is what names that key once rather than at each of those readers.
func (cfg Config) ExperimentsDeclared() []Experiment {
	return cfg.Projects[cfg.Project].ExperimentsDeclared("projects." + cfg.Project)
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
	// An `experiments: {}` declares no experiment under the key, as an absent key does, so the
	// field stays nil and a reader of it needs no second shape for the empty mapping; the memory
	// experiment the project declares all the same is ExperimentsDeclared's.
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
// `unit`, a tree or an issue (absent is a tree); `roles`, the issue roles whose launches load its
// arms (absent is every one); `variants`, the arms it draws among, which it must have; and
// `on_variant`, the arm mode on gives every unit, which that mode requires and which must name an
// arm of this experiment. A mode other than off over one variant is refused: a draw among one arm
// measures nothing, so the file is missing the arm it compares against.
//
// The rules of `plan` are checkExperimentPlans', which runs after checkExperimentMemory, so the one
// experiment that may never set `plan`, `memory`, is refused by its own rule first.
func readExperiment(value *yaml.Node, key, name string) (Experiment, error) {
	if value.Kind != yaml.MappingNode {
		return Experiment{}, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := describedMembers(value, key, "projects.<KEY>.experiments.<NAME>")
	if err != nil {
		return Experiment{}, err
	}
	experiment := Experiment{Name: name, Mode: ExperimentOff, Unit: ExperimentTree}
	mode, err := readMode(fields["mode"], key+".mode")
	if err != nil {
		return Experiment{}, err
	}
	if mode != "" {
		experiment.Mode = mode
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
	if roles := fields["roles"]; roles != nil {
		if experiment.Roles, err = readExperimentRoles(roles, key+".roles"); err != nil {
			return Experiment{}, err
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

// readExperimentRoles is an experiment's `roles`: a sequence of at least one Legion issue role
// (claim.Roles), each named once. The controller is no word of it, since a controller runs in no
// experiment. An empty list is refused rather than read as every role: it reads as no role, and an
// experiment whose arms reach no launch treats nothing, so the file would say the opposite of what
// the daemon did.
func readExperimentRoles(value *yaml.Node, key string) ([]claim.Role, error) {
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be a sequence of Legion issue roles, e.g. [planner, implementer]", key)
	}
	if len(value.Content) == 0 {
		return nil, fmt.Errorf("%s names no role: an experiment whose arms reach no launch treats nothing, so name the roles its arms reach, or leave the key out for every role", key)
	}
	roles := make([]claim.Role, 0, len(value.Content))
	for _, entry := range value.Content {
		role := claim.Role(entry.Value)
		if entry.Kind != yaml.ScalarNode || !slices.Contains(claim.Roles, role) {
			return nil, fmt.Errorf("%s names %q, which is not a Legion issue role (%s)", key, entry.Value, runtime.JoinRoles(claim.Roles))
		}
		if slices.Contains(roles, role) {
			return nil, fmt.Errorf("%s names %s twice", key, role)
		}
		roles = append(roles, role)
	}
	return roles, nil
}

// readMode is the `mode` of an experiment's block or of a project's `memory` block: one of the
// three words, or the empty mode for a block that names none, which each caller leaves at the
// word its own absent key stands for. The two keys are one mode (Memory.Mode is an
// ExperimentMode), so they are read once here and refuse a fourth word in the same words.
func readMode(value *yaml.Node, key string) (ExperimentMode, error) {
	word, err := stringOf(value, key)
	if err != nil || word == "" {
		return "", err
	}
	mode := ExperimentMode(word)
	if !slices.Contains(experimentModes, mode) {
		return "", fmt.Errorf("%s must be off, on, or random (got %q)", key, word)
	}
	return mode, nil
}

// readVariants is an experiment's `variants`: a sequence of at least one arm, each a mapping with
// a `name` the experiment names once, an optional `weight` (a whole number above zero, 1 where the
// arm names none), optional `config_files` read as `role_config_files`' lists are
// (readOverlayFiles), an optional `instructions` path the daemon reads at boot, an optional
// `texts` folder the daemon reads at boot too, and an optional `plan`, one of the words of
// PlanGroup. No file key is required: the arm that names no file is the control the others are
// measured against, and an arm may name any one of them alone. `plan: owner-alone` is refused:
// this release does not carry it out.
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
			if variant.InstructionsPath, err = cleanAbsolutePath(instructions, field+".instructions"); err != nil {
				return nil, err
			}
		}
		if texts := fields["texts"]; texts != nil {
			if variant.TextsPath, err = cleanAbsolutePath(texts, field+".texts"); err != nil {
				return nil, err
			}
		}
		plan, err := stringOf(fields["plan"], field+".plan")
		if err != nil {
			return nil, err
		}
		switch variant.Plan = PlanGroup(plan); {
		case plan == "":
		case !slices.Contains(planGroups, variant.Plan):
			return nil, fmt.Errorf("%s.plan must be owner or default (got %q)", field, plan)
		case variant.Plan == PlanGroupOwnerAlone:
			return nil, fmt.Errorf("%s.plan is owner-alone: the owner doing the whole issue itself in one step. This release of Legion does not carry it out yet.", field)
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

// checkExperimentMemory refuses the shapes of `experiments.memory` that the name memory forbids
// and every other name of the key allows, which is why they are read here rather than in
// readExperiments, which reads every name alike: a project that writes the `memory` block and
// declares the experiment too, since the two are one experiment spelled twice and the daemon would
// draw for it twice; an `experiments.memory` whose variants name `plan`, since its draw is the
// tree's memory group and never says how an issue is done; an `experiments.memory` whose variants
// are not exactly memory and none, which would serve every pod of the project a state body it
// refuses (MemoryVariantNames); and one whose unit is not the tree. A draw of the experiment named
// memory is served as the issue state's three memory fields (internal/projection's memoryOf),
// which are the draw of the issue's tree and the same for every issue of it
// (packages/contracts/src/legion-api.ts, legionIssue's memoryGroup), so an issue unit would serve
// each issue an answer of its own under fields that promise its tree's. It runs before
// checkExperimentPlans, so `plan` on `experiments.memory` gets this one refusal and never that
// check's advice to write `unit: issue`, which the unit rule here would refuse. Every other name of
// the key declares whatever variants and unit it likes, but for one whose arms name `plan`, which
// draws for each issue (checkExperimentPlans).
func checkExperimentMemory(experiments []Experiment, blockWritten bool, key string) error {
	index := slices.IndexFunc(experiments, func(e Experiment) bool { return e.Name == MemoryExperimentName })
	if index < 0 {
		return nil
	}
	if blockWritten {
		return fmt.Errorf("%s.memory and %s.experiments.memory are one experiment written twice: the memory block is the experiment named memory, so write the block or the key, not both", key, key)
	}
	if experiments[index].SetsPlan() {
		return fmt.Errorf("%s.experiments.memory names plan: the memory experiment never sets how an issue is done, since a draw of it is the tree's memory group, so take plan out of its variants and set it on an experiment of its own", key)
	}
	// The two names, in whichever order the operator wrote them: the key declares the same
	// experiment either way round, so both sides are sorted rather than this comparison resting
	// on MemoryVariantNames happening to be in alphabetical order.
	names := variantNames(experiments[index].Variants)
	if !slices.Equal(slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(MemoryVariantNames))) {
		return fmt.Errorf("%s.experiments.memory names the variants %s: its variants must be exactly %s, since a draw of it is served to every pod as the issue state's memoryGroup, whose wire enum is those two words, and a third makes the whole body fail to parse",
			key, strings.Join(names, ", "), strings.Join(MemoryVariantNames, " and "))
	}
	if unit := experiments[index].Unit; unit != ExperimentTree {
		return fmt.Errorf("%s.experiments.memory is unit %s: its unit must be tree, since a draw of it is served to every pod as the issue state's memoryGroup, memoryDrawMode and memoryDrawnAt, which are the draw of the issue's tree, one answer for every issue of it",
			key, unit)
	}
	return nil
}

// checkExperimentPlans refuses an experiment that sets `plan` in a shape no issue could be drawn
// into: an arm that names no `plan` beside one that does, since an arm with none would leave its
// issues' way of working to no word; and a unit other than the issue, since how an issue is done is
// read from the draw recorded against that issue itself, and a tree's draw is recorded against the
// tree's root alone. It runs after checkExperimentMemory (stage3's project reader), whose own rule
// refuses `plan` on `experiments.memory`.
func checkExperimentPlans(experiments []Experiment, key string) error {
	for _, experiment := range experiments {
		if !experiment.SetsPlan() {
			continue
		}
		block := key + ".experiments." + experiment.Name
		for _, variant := range experiment.Variants {
			if variant.Plan == "" {
				return fmt.Errorf("%s.variants.%s names no plan: each variant of an experiment that sets plan names how its issues are done", block, variant.Name)
			}
		}
		if experiment.Unit != ExperimentIssue {
			return fmt.Errorf("%s sets plan and is unit %s: an experiment that sets how an issue is done draws for each issue, so write unit: issue", block, experiment.Unit)
		}
	}
	return nil
}

// checkExperiments refuses what a project's declared variants name that no role could load as
// named, once the rest of the file is settled: the two rules every declaration of overlays
// answers — a harness other than Oh My Pi on an issue role the experiment reaches (its `roles`),
// and an overlay outside the operator's pod mounts (checkDeclaredOverlays), which the `memory`
// block answers too.
//
// Both hold whatever the mode is, `off` included, for the reason checkMemory's own two do: the
// mode decides which new units are drawn, and a unit already drawn keeps the files of the variant
// it drew while that variant names them, so an overlay no role could load would stop that unit's
// roles under any mode. Taking the files away is removing them from the variant, not moving the
// mode. checkMemory's third rule, that a mode other than off must name config_files, is the
// `memory` block's own and is not asked of a variant: an experiment's control arm names no overlay
// by design, and an arm may name instructions alone.
//
// The `memory` block's own arm is checkMemory's, not read here, so the block keeps the refusals
// the documentation quotes. It is the one arm of the declared list this skips: an
// `experiments.memory`, which declares the same experiment the other way round, is an arm of the
// `experiments` key like any other and is read here.
//
// The walk is of the declared list rather than of the file as it was read, so an arm is named by
// the key path it carries (Variant.ConfigFilesField) and the spelling an operator reads in a
// refusal is built in one place.
//
// Before those, each project's whole declared list is held to the bound of the attribute that
// names a launch's arms (checkSpendArms), which the `memory` block's experiment counts toward too,
// since every launch drawn into it names its arm there.
//
// It also refuses a project two of whose experiments set `plan` (Experiment.SetsPlan): each draws
// for every new issue, so each issue would hold two words on how it is done, which may disagree,
// and one experiment's measurement would run under the other's arms. After those, the arms that
// name a folder of texts are held to the rules of their own (checkTextsArms), under any mode for
// the same reason as the overlays.
func checkExperiments(cfg Config) error {
	for _, name := range slices.Sorted(maps.Keys(cfg.Projects)) {
		project := cfg.Projects[name]
		block := project.MemoryIsBlock()
		declared := project.ExperimentsDeclared("projects." + name)
		if err := checkSpendArms(declared, "projects."+name); err != nil {
			return err
		}
		setsPlan := ""
		for _, experiment := range declared {
			if experiment.SetsPlan() {
				if setsPlan != "" {
					return fmt.Errorf("projects.%s.experiments.%s and projects.%s.experiments.%s both set plan: one experiment sets how an issue is done", name, setsPlan, name, experiment.Name)
				}
				setsPlan = experiment.Name
			}
			if block && experiment.Name == MemoryExperimentName {
				continue
			}
			for _, variant := range experiment.Variants {
				if err := checkDeclaredOverlays(cfg, variant.ConfigFilesField, "the variant", variant.ConfigFiles, experiment.Roles); err != nil {
					return err
				}
			}
		}
		if err := checkTextsArms(cfg, "projects."+name, declared); err != nil {
			return err
		}
	}
	return nil
}

// checkTextsArms refuses the folders of texts the project's declared arms name
// (Variant.TextsPath) that some role of a unit drawn into them could not read as one version of
// Legion's texts: an arm whose experiment reaches a role on a harness other than Oh My Pi, since
// only an Oh My Pi launch reads a folder, which the architect always is; under runtime kubernetes,
// a folder the operator's pod mounts do not hold whole (checkTextsMount), since a role reads it
// in its pod at the path the daemon reads it at; and two experiments each with an arm naming a
// folder whose roles meet (Experiment.Reaches, every issue role where an experiment names none),
// since a unit drawn into both would have that role read two folders at once. Two arms of one
// experiment may each name a folder: a unit is drawn into one arm of an experiment. key is the
// project's own path, `projects.<KEY>`. The folder's own files are boot's to read
// (internal/daemon's readBoot), so `legion config check` refuses those too.
func checkTextsArms(cfg Config, key string, declared []Experiment) error {
	// giver is the experiment that gives each issue role a folder, of those read so far.
	giver := map[claim.Role]string{}
	for _, experiment := range declared {
		folders := false
		for _, variant := range experiment.Variants {
			if variant.TextsPath == "" {
				continue
			}
			folders = true
			for _, role := range claim.Roles {
				if !experiment.Reaches(role) {
					continue
				}
				if backend := cfg.BackendFor(role); backend != runtime.BackendOmp {
					fix := "put every issue role on " + string(runtime.BackendOmp)
					if experiment.Roles != nil {
						fix = "put every role its roles names on " + string(runtime.BackendOmp)
					}
					return fmt.Errorf("%s names texts, and worker_backends.%s is %s, which reads no folder of texts: %s, or take texts out of the variant",
						strings.TrimSuffix(variant.ConfigFilesField, ".config_files"), role, backend, fix)
				}
			}
			if k := cfg.Runtime.Kubernetes; k != nil {
				if err := checkTextsMount(k.Pod, variant.TextsField(), variant.TextsPath); err != nil {
					return err
				}
			}
		}
		if !folders {
			continue
		}
		for _, role := range claim.Roles {
			if !experiment.Reaches(role) {
				continue
			}
			if other, given := giver[role]; given {
				return fmt.Errorf("%s.experiments.%s and %s.experiments.%s each give a folder of texts to the %s, which would then read two folders: name the %s in the roles of one of the two alone",
					key, other, key, experiment.Name, role, role)
			}
			giver[role] = experiment.Name
		}
	}
	return nil
}

// checkTextsMount refuses folder, the folder of texts the arm's key field names, unless a mount of
// the operator's pod block (runtime.kubernetes.pod.volume_mounts), which every role's pod carries,
// holds it as a directory: the deepest mount at or above it, as the kernel resolves the path,
// mounting a whole volume, the folder itself or one above it. A sub_path mount carries one key of
// its volume, never the folder's tree of files. It is checkRoleConfigMount's directory form: a
// file must be the item of a directory mount or a sub_path mount of its own, and a folder the
// opposite. Which files the folder holds is boot's to read, and whether a volume that projects
// items projects each of them (CheckTextsFilesMounted).
func checkTextsMount(pod PodConfig, field, folder string) error {
	holder := deepestMount(pod, folder)
	switch {
	case holder == nil:
		return fmt.Errorf("%s %s is under no %s.volume_mounts mount_path: a role's pod sees only the image and the operator's mounts, so mount the volume that holds the folder there, at the path the daemon reads it at", field, folder, podKey)
	case holder.SubPath != "":
		return fmt.Errorf("%s %s is under the sub_path mount %s of volume %s, which mounts one key of it and no folder: mount the volume that holds the folder whole, with no sub_path", field, folder, holder.MountPath, holder.Name)
	}
	return nil
}

// CheckTextsFilesMounted refuses a file of the folder of texts the arm's key field names that the
// operator's pod mounts do not carry under runtime kubernetes: files are the files boot read in the
// folder, by their paths there (the manifest and every file it lists), and each must be a file a
// role's pod sees at the folder's path (checkRoleConfigMount), which under a ConfigMap or Secret
// volume that projects items is one of those items, since the kubelet writes nothing else there.
// The loader has held the folder itself to the mounts (checkTextsMount); only boot knows its files.
func CheckTextsFilesMounted(pod PodConfig, field, folder string, files []string) error {
	for _, file := range files {
		if err := checkRoleConfigMount(pod, field, path.Join(folder, file)); err != nil {
			return err
		}
	}
	return nil
}

// checkSpendArms refuses a project whose declared experiments could make one launch's
// `legion.arms` longer than the agent's OpenTelemetry detector takes
// (runtime.SpendAttributeLimit): a launch names one arm of each experiment it runs in
// (runtime.SpendArmsValue), so the longest value is each experiment with its longest arm's name,
// and a value past the bound throws in the detector and costs every launch of that unit every
// attribute it carries, including the operator's project attribute. Every name is ASCII
// (experimentNamePattern), so its length in bytes is its length in the detector's UTF-16 units.
func checkSpendArms(declared []Experiment, key string) error {
	longest := make([]runtime.Variant, 0, len(declared))
	for _, experiment := range declared {
		arm := runtime.Variant{Experiment: experiment.Name}
		for _, variant := range experiment.Variants {
			if len(variant.Name) > len(arm.Name) {
				arm.Name = variant.Name
			}
		}
		longest = append(longest, arm)
	}
	if n := len(runtime.SpendArmsValue(longest)); n > runtime.SpendAttributeLimit {
		return fmt.Errorf("%s declares experiments whose arms one launch could name in %s in %d characters, over the 255 the agent's telemetry takes: shorten the experiments' or their variants' names, or declare fewer experiments", key, runtime.SpendArmsAttribute, n)
	}
	return nil
}
