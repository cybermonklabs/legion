package experiment

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Arms are what one launch runs in: Variants is each arm the launch's unit was drawn into, in the
// order the project's file declares the experiments, which the runtime names the overlays of
// last in PI_CONFIG_FILES; and Instructions are the daemon's own copies of the instructions files
// those arms name, in the same order, which the launch's system prompt ends with. A launch in no
// arm, and one whose arms name no instructions file, answer none.
type Arms struct {
	Variants     []runtime.Variant
	Instructions []string
}

// Launch resolves the arms a launch runs in from the experiments a project declares. It is built
// once, at boot, and asked at every launch, so the question the declaration alone settles —
// whether any arm names a file at all — is answered once here rather than per spawn.
//
// stateDir is the daemon's own state directory, under which the daemon keeps its copy of each
// arm's instructions file (config.VariantInstructionsFile, written by
// config.WriteVariantInstructions at boot and again whenever a launch reads an edited file). An
// arm carries that copy and never the operator's path, since a pod cannot read the daemon's
// configuration.
type Launch struct {
	declared  []config.Experiment
	stateDir  string
	namesFile bool
}

// ForLaunch builds that resolution over declared, the project's declared experiments in the
// file's order (config.Project.ExperimentsDeclared, the list that maps the `memory` block in as
// the experiment named memory).
func ForLaunch(declared []config.Experiment, stateDir string) Launch {
	namesFile := func(experiment config.Experiment) bool {
		return slices.ContainsFunc(experiment.Variants, func(variant config.Variant) bool {
			return len(variant.ConfigFiles) > 0 || variant.InstructionsPath != ""
		})
	}
	return Launch{declared: declared, stateDir: stateDir, namesFile: slices.ContainsFunc(declared, namesFile)}
}

// Keys are the issue keys whose recorded answers a launch of issue must read: the issue's own,
// which holds every answer it runs under, its issue experiments' and its tree experiments' alike.
// A tree experiment's answer is the issue's own row too, copied at its first start from the
// nearest issue above it that holds one, or drawn for it there (admit's takeTreeDraw, R17 of
// dispatch://INFRA-3320), so for an issue the daemon records no tree the record stores and no
// issue above it is read: a change of parent changes no arm. An issue the daemon holds no record
// of holds no row, and its caller reads the rows of its Dispatch chain in place of its own
// (internal/daemon's launchVariants), so its arm follows that chain at each launch.
//
// It is none at all where no declared arm names a file: an experiment whose every arm names
// neither a config file nor an instructions path contributes nothing to any launch, since there
// is nothing for a drawn unit to run with whichever arm it drew. A project none of whose declared
// experiments names a file — the common deployment, which writes no `memory` block and declares
// no experiment of its own, and so declares the memory experiment at mode off and nothing else —
// therefore has no record to read, and pays no transaction per worker spawn.
func (l Launch) Keys(issue string) []string {
	if !l.namesFile {
		return nil
	}
	return []string{issue}
}

// Arms are the arms recorded puts this launch in: for each experiment the project declares, in
// the file's order, the arm recorded for the issue under that experiment (record.Assignment, the
// issue's own row whatever the experiment's unit, as Keys says, read back at every launch of its
// roles). recorded is what the caller read for Keys, or, for an issue the daemon holds no record
// of, the tree rows its caller took from the nearest issue above it in their place. An issue with
// no row for an experiment, one no experiment was declared for, and one first started before the
// experiment was declared run in no arm of it.
//
// An issue whose recorded arm the project's file no longer names contributes nothing: a launch
// names the files the file still holds, so an arm taken out of the file takes its overlays and
// its instructions with it, and the issue runs as one in no experiment does. Its row stays, so a
// file that names the arm again gives the issue the arm it was drawn into.
//
// A launch of a role the experiment's `roles` leaves out runs in no arm of it, whatever the issue
// drew (config.Experiment.Reaches): the issue keeps its row and the arm it was drawn into, and
// only the roles the experiment names load that arm's overlays and instructions, so the roles it
// leaves out judge every issue with none of this experiment's files, while each other experiment
// that reaches them gives them its own arm as before.
//
// No mode is read: the mode decides which new units are drawn, and an issue already drawn into an
// arm keeps that arm's files whatever the mode is now.
func (l Launch) Arms(recorded map[string][]record.Assignment, issue string, role claim.Role) Arms {
	var arms Arms
	for _, experiment := range l.declared {
		if !experiment.Reaches(role) {
			continue
		}
		drawn := ""
		for _, assignment := range recorded[issue] {
			if assignment.Experiment == experiment.Name {
				drawn = assignment.Variant
			}
		}
		if drawn == "" {
			continue
		}
		for _, variant := range experiment.Variants {
			if variant.Name != drawn {
				continue
			}
			arms.Variants = append(arms.Variants, arm(experiment, variant))
			if variant.InstructionsPath != "" {
				arms.Instructions = append(arms.Instructions, config.VariantInstructionsFile(l.stateDir, experiment.Name, variant))
			}
		}
	}
	return arms
}

// DeclaredVariants is every arm of every experiment declared, in the order the project's file
// declares the experiments and, within one experiment, the order it declares the arms: what each
// runtime is held to as a path PI_CONFIG_FILES can name, and what its readers that hold a pod or
// the deployment and no tree take, since any unit may be drawn into any arm. They are handed over
// whatever each experiment's mode is: the mode decides which new units are drawn, and a unit
// already drawn into an arm names that arm's files under any mode. An empty declared list hands
// over none. No loaded project gives one, since every project declares the memory experiment
// (config.Project.ExperimentsDeclared); a project that writes neither the `memory` block nor
// `experiments.memory` hands over that experiment's two arms, which name no file, so a launch it
// makes is the launch every deployment built before any experiment existed made.
//
// A variant's instructions reach no runtime through its options: an arm carries no such path
// (runtime.Variant), and the daemon names its own copy of each in the launch's prompt instead
// (Launch.Arms, which answers the arms one launch runs in out of the same constructor).
func DeclaredVariants(declared []config.Experiment) []runtime.Variant {
	var variants []runtime.Variant
	for _, experiment := range declared {
		for _, variant := range experiment.Variants {
			variants = append(variants, arm(experiment, variant))
		}
	}
	return variants
}

// arm is one declared arm as a runtime reads it: the experiment's name and the arm's, as a draw
// records them, the overlays the arm names, the key the operator wrote them under, whether the
// `memory` block declares the arm, and the roles its experiment reaches. It is the
// one place a config.Variant becomes a runtime.Variant, so the arms a launch runs in and the arms
// a runtime is built with carry the same fields.
func arm(experiment config.Experiment, variant config.Variant) runtime.Variant {
	return runtime.Variant{
		Experiment:       experiment.Name,
		Name:             variant.Name,
		ConfigFiles:      variant.ConfigFiles,
		ConfigFilesField: variant.ConfigFilesField,
		MemoryBlock:      variant.MemoryBlock,
		Roles:            experiment.Roles,
	}
}
