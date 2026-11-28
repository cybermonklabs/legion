package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// WatchedWorkflow is one entry of a project's `watched_workflows`: a workflow file of one
// repository, and the trigger events whose completed runs on that repository's default branch the
// controller hears, never empty once loaded (`schedule` alone for an entry that names none).
type WatchedWorkflow struct {
	// Repository is the entry's owner and repository, as the file spells them. Envoy names a run's
	// topic with GitHub's spelling and a topic is case-sensitive, so an entry that spells either
	// otherwise hears nothing; the documentation says to write GitHub's.
	Repository ghrepo.Repository
	// File is the workflow's file name in the repository's `.github/workflows/`, such as
	// `nightly.yaml`.
	File string
	// Events are the trigger events whose runs count, as a workflow's `on:` names them, in the
	// order the entry names them.
	Events []string
}

// String is the entry as the file writes it, with the events it resolved to:
// `<owner>/<repo>/<file>@<event>,<event>`.
func (w WatchedWorkflow) String() string {
	return w.Repository.String() + "/" + w.File + "@" + strings.Join(w.Events, ",")
}

var (
	// workflowFilePattern is the name of a file in a repository's `.github/workflows/` that an
	// entry of `watched_workflows` takes: letters, digits, `.`, `_` and `-`, ending in `.yml` or
	// `.yaml`.
	workflowFilePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+\.(yml|yaml)$`)
	// gitHubTriggerEvents is every event that triggers a workflow, each as a workflow's `on:`
	// names it and as a run's `event` carries it, from
	// https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows
	// (gitHubTriggerEventsURL) as read on 2026-11-27: the events an entry of `watched_workflows`
	// may name, each the last token of a topic the controller subscribes. Some never start a run
	// on a repository's own default branch (the pull request events, merge_group, workflow_call,
	// and release and create for a tag); an entry may still name them, and hears nothing of them.
	gitHubTriggerEvents = []string{
		"branch_protection_rule", "check_run", "check_suite", "create", "delete", "deployment",
		"deployment_status", "discussion", "discussion_comment", "fork", "gollum", "image_version",
		"issue_comment", "issues", "label", "merge_group", "milestone", "page_build", "public",
		"pull_request", "pull_request_review", "pull_request_review_comment", "pull_request_target",
		"push", "registry_package", "release", "repository_dispatch", "schedule", "status", "watch",
		"workflow_call", "workflow_dispatch", "workflow_run",
	}
)

// gitHubTriggerEventsURL is GitHub's list of the events that trigger a workflow, which the
// refusal of an event outside gitHubTriggerEvents points to.
const gitHubTriggerEventsURL = "https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows"

// defaultWatchedEvent is the one event an entry of `watched_workflows` that names none watches.
const defaultWatchedEvent = "schedule"

// watchedWorkflowForm is an entry of `watched_workflows` as each refusal of its shape writes it.
const watchedWorkflowForm = `"<owner>/<repo>/<file>", optionally followed by "@" and the trigger events that count, comma-separated`

// readWatchedWorkflows is a project's `watched_workflows`: a list of strings, each a workflow of
// one repository and the trigger events whose completed runs on its default branch the project's
// controller hears (parseWatchedWorkflow). One workflow is named once: two entries whose owner and
// repository are equal without case, as GitHub compares them, and whose files are equal with
// case, as a repository's files are named, are the file saying one thing twice, and their events
// belong in one entry. Each refusal names the key, the entry's place in the list and the entry.
// The entries keep the file's order. An empty list is no workflow, as an absent key is.
func readWatchedWorkflows(value *yaml.Node, key string) ([]WatchedWorkflow, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s is %s, not a list of entries, each %s", key, yamlValueText(value), watchedWorkflowForm)
	}
	if len(value.Content) == 0 {
		return nil, nil
	}
	workflows := make([]WatchedWorkflow, 0, len(value.Content))
	for index, entry := range value.Content {
		field := fmt.Sprintf("%s[%d]", key, index)
		if entry.Kind != yaml.ScalarNode || entry.Tag != "!!str" {
			return nil, fmt.Errorf("%s is %s, not a string: an entry is %s", field, yamlValueText(entry), watchedWorkflowForm)
		}
		workflow, err := parseWatchedWorkflow(field, entry.Value)
		if err != nil {
			return nil, err
		}
		for earlier, named := range workflows {
			if named.File == workflow.File && strings.EqualFold(named.Repository.String(), workflow.Repository.String()) {
				return nil, fmt.Errorf("%s[%d] %+q and %s %+q name one workflow: GitHub compares an owner and a repository without case, and a file with it; name the workflow once, with every event it watches",
					key, earlier, value.Content[earlier].Value, field, entry.Value)
			}
		}
		workflows = append(workflows, workflow)
	}
	return workflows, nil
}

// parseWatchedWorkflow is one entry of `watched_workflows`, `<owner>/<repo>/<file>`, optionally
// followed by `@` and comma-separated events, or a refusal that begins with field, the entry's key
// and place, and the entry. The owner and the repository are a repository ghrepo.Parse takes, the
// file a name workflowFilePattern takes, and each event one of gitHubTriggerEvents, compared with
// case, named once in the entry, in the entry's order; an entry with no `@` watches
// defaultWatchedEvent alone, and an `@` with no event, or an empty one, is refused rather than
// read as that default.
func parseWatchedWorkflow(field, entry string) (WatchedWorkflow, error) {
	at := fmt.Sprintf("%s %+q", field, entry)
	path, events, named := strings.Cut(entry, "@")
	parts := strings.Split(path, "/")
	if len(parts) != 3 {
		return WatchedWorkflow{}, fmt.Errorf("%s is not %s", at, watchedWorkflowForm)
	}
	repository, err := ghrepo.Parse(at+": the repository", parts[0]+"/"+parts[1])
	if err != nil {
		return WatchedWorkflow{}, err
	}
	if !workflowFilePattern.MatchString(parts[2]) {
		return WatchedWorkflow{}, fmt.Errorf(`%s: the workflow file %+q is not a file name of .github/workflows: letters, digits, ".", "_" and "-", ending in ".yml" or ".yaml"`, at, parts[2])
	}
	workflow := WatchedWorkflow{Repository: repository, File: parts[2], Events: []string{defaultWatchedEvent}}
	if !named {
		return workflow, nil
	}
	workflow.Events = strings.Split(events, ",")
	for index, event := range workflow.Events {
		switch {
		case event == "":
			return WatchedWorkflow{}, fmt.Errorf(`%s: "@" must be followed by trigger events, comma-separated, none of them empty; an entry with no "@" watches %s runs alone`, at, defaultWatchedEvent)
		case !slices.Contains(gitHubTriggerEvents, event):
			return WatchedWorkflow{}, fmt.Errorf(`%s: %+q is not one of GitHub's trigger events, which a workflow's "on:" names in lower case (%s)`, at, event, gitHubTriggerEventsURL)
		case slices.Contains(workflow.Events[:index], event):
			return WatchedWorkflow{}, fmt.Errorf("%s names the event %q twice", at, event)
		}
	}
	return workflow, nil
}

// yamlValueText is a value a refusal names when it is not of the kind its key takes: a string
// quoted, any other scalar as the file writes it, and a mapping, a list or an alias by its kind.
func yamlValueText(node *yaml.Node) string {
	switch {
	case node.Kind == yaml.ScalarNode && node.Tag == "!!str":
		return fmt.Sprintf("%+q", node.Value)
	case node.Kind == yaml.ScalarNode && node.Value == "":
		return "null"
	case node.Kind == yaml.ScalarNode:
		return node.Value
	case node.Kind == yaml.MappingNode:
		return "a mapping"
	case node.Kind == yaml.SequenceNode:
		return "a list"
	}
	return "an alias"
}
