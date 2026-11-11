package experiment

import (
	"slices"

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
// once, at boot, and asked at every launch, so the two questions the declaration alone settles —
// whether any arm names a file at all, and whether any experiment draws for the issue rather than
// the tree — are answered once here rather than per spawn.
//
// stateDir is the daemon's own state directory, under which each arm's instructions file was
// copied at boot (config.VariantInstructionsFile, written by config.WriteVariantInstructions). An
// arm carries that copy and never the operator's path: a pod cannot read the daemon's
// configuration, and the operator's file may change under a running daemon.
type Launch struct {
	declared  []config.Experiment
	stateDir  string
	namesFile bool
	byIssue   bool
}

// ForLaunch builds that resolution over declared, the project's declared experiments in the
// file's order (config.Project.ExperimentsDeclared, the list Declaring maps the `memory` block
// into as the experiment named memory).
func ForLaunch(declared []config.Experiment, stateDir string) Launch {
	namesFile := func(experiment config.Experiment) bool {
		return slices.ContainsFunc(experiment.Variants, func(variant config.Variant) bool {
			return len(variant.ConfigFiles) > 0 || variant.InstructionsPath != ""
		})
	}
	return Launch{
		declared:  declared,
		stateDir:  stateDir,
		namesFile: slices.ContainsFunc(declared, namesFile),
		byIssue:   slices.ContainsFunc(declared, func(e config.Experiment) bool { return e.Unit == config.ExperimentIssue }),
	}
}

// Keys are the issue keys whose recorded answers a launch of issue, which stands in the tree
// rooted at tree, must read: the tree's own, which every tree experiment is recorded against, and
// the issue's beside it where some declared experiment draws for the issue (config.ExperimentIssue),
// since a deployment whose experiments all draw for the tree has nothing of the issue's own to
// read.
//
// It is none at all where no declared arm names a file: an experiment whose every arm names
// neither a config file nor an instructions path contributes nothing to any launch, since there
// is nothing for a drawn unit to run with whichever arm it drew. A project none of whose declared
// experiments names a file — the common deployment, which writes no `memory` block and declares
// no experiment of its own, and so declares the memory experiment at mode off and nothing else —
// therefore has no record to read, and pays no transaction per worker spawn.
func (l Launch) Keys(tree, issue string) []string {
	if !l.namesFile {
		return nil
	}
	keys := []string{tree}
	if l.byIssue && issue != tree {
		keys = append(keys, issue)
	}
	return keys
}

// Arms are the arms recorded puts this launch in: for each experiment the project declares, in
// the file's order, the arm recorded for the unit that experiment covers — the issue's tree root
// for a tree experiment, the issue itself for an issue experiment (record.Assignment, written
// when the unit was opened and read back at every launch of its roles). recorded is what the
// caller read for Keys. A unit with no row for an experiment, one no experiment was declared for,
// and a root first opened before the experiment was declared run in no arm of it.
//
// A unit whose recorded arm the project's file no longer names contributes nothing: a launch
// names the files the file still holds, so an arm taken out of the file takes its overlays and
// its instructions with it, and the unit runs as one in no experiment does. Its row stays, so a
// file that names the arm again gives the unit the arm it was drawn into.
//
// No mode is read: the mode decides which new units are drawn, and a unit already drawn into an
// arm keeps that arm's files whatever the mode is now.
func (l Launch) Arms(recorded map[string][]record.Assignment, tree, issue string) Arms {
	var arms Arms
	for _, experiment := range l.declared {
		unit := tree
		if experiment.Unit == config.ExperimentIssue {
			unit = issue
		}
		drawn := ""
		for _, assignment := range recorded[unit] {
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
			arms.Variants = append(arms.Variants, runtime.Variant{
				Experiment:       experiment.Name,
				Name:             variant.Name,
				ConfigFiles:      variant.ConfigFiles,
				ConfigFilesField: variant.ConfigFilesField,
			})
			if variant.InstructionsPath != "" {
				arms.Instructions = append(arms.Instructions, config.VariantInstructionsFile(l.stateDir, experiment.Name, variant.Name))
			}
		}
	}
	return arms
}
