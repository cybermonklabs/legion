package prodevents

import (
	"maps"
	"slices"
	"strings"
)

// PullAttribution is who a pull request a deploy ships counts for, as the attribution list of the
// pull request states it (evals' attributionList): the projects whose default repository it counts
// for in rank order, and the project it counts for where none of those is the deploy's repository,
// "" for none.
type PullAttribution struct {
	Conditions []string
	Terminal   string
}

// DeployFacts is every input that decides which projects a deploy row counts for
// (DeployProjects): Defaults, the default repository of each project ("" for a project with none);
// Source, the project whose deploy source reads the deploy's repository ("" for none), where the
// deploy ships no pull request; and Pulls, the attribution of each pull request it ships that
// names one, in the order it names them. A reader fills them from the published state
// (evals.DeployCountsFor) or from the stored facts (signals' deployCountsForRow).
type DeployFacts struct {
	Defaults map[string]string
	Source   string
	Pulls    []PullAttribution
}

// DeployProjects is the projects deploy counts for under facts, in the one rule every reader of
// the record's deploys counts by (INFRA-978 R12, INFRA-2916 R15): a posted deploy counts for its
// route's project alone; a dropped GitHub deploy for none; a GitHub deploy that ships no pull
// request for the project that reads its repository (facts.Source); and one that ships pull
// requests for the project each one's list picks, the first of its conditions whose default
// repository is the deploy's, else its terminal, a project that exists (facts.Defaults) alone, and,
// where some list picks none, for each project whose default repository is the deploy's.
func DeployProjects(deploy Event, facts DeployFacts) []string {
	switch {
	case deploy.Dropped():
		return nil
	case deploy.Source == SourceAPI:
		return []string{deploy.ProjectKey}
	case len(facts.Pulls) == 0:
		if facts.Source == "" {
			return nil
		}
		return []string{facts.Source}
	}
	var projects []string
	add := func(project string) {
		if _, exists := facts.Defaults[project]; exists && !slices.Contains(projects, project) {
			projects = append(projects, project)
		}
	}
	readsRepo := func(project string) bool {
		repo, exists := facts.Defaults[project]
		return exists && repo != "" && strings.EqualFold(repo, deploy.Repo)
	}
	unpicked := false
	for _, pull := range facts.Pulls {
		pick := pull.Terminal
		if i := slices.IndexFunc(pull.Conditions, readsRepo); i >= 0 {
			pick = pull.Conditions[i]
		}
		if pick == "" {
			unpicked = true
			continue
		}
		add(pick)
	}
	if unpicked {
		for _, project := range slices.Sorted(maps.Keys(facts.Defaults)) {
			if readsRepo(project) {
				add(project)
			}
		}
	}
	return projects
}
