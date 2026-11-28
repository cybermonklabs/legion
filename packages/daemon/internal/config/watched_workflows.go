package config

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
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
// belong in one entry. Each refusal names the key and the entry's place in the list, and the entry
// only where it has an entry's shape (watchedWorkflowNamed); a value that is not of the kind its
// place takes is named by its kind (watchedWorkflowsValueText). The entries keep the file's order.
// An empty list is no workflow, as an absent key is.
func readWatchedWorkflows(value *yaml.Node, key string) ([]WatchedWorkflow, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s is %s, not a list of entries, each %s", key, watchedWorkflowsValueText(value), watchedWorkflowForm)
	}
	if len(value.Content) == 0 {
		return nil, nil
	}
	workflows := make([]WatchedWorkflow, 0, len(value.Content))
	for index, entry := range value.Content {
		field := fmt.Sprintf("%s[%d]", key, index)
		if entry.Kind != yaml.ScalarNode || entry.Tag != "!!str" {
			return nil, fmt.Errorf("%s is %s, not a string: an entry is %s", field, watchedWorkflowsValueText(entry), watchedWorkflowForm)
		}
		workflow, err := parseWatchedWorkflow(field, entry.Value)
		if err != nil {
			return nil, err
		}
		for earlier, named := range workflows {
			if named.File == workflow.File && strings.EqualFold(named.Repository.String(), workflow.Repository.String()) {
				return nil, fmt.Errorf("%s and %s name one workflow: GitHub compares an owner and a repository without case, and a file with it; name the workflow once, with every event it watches",
					watchedWorkflowNamed(fmt.Sprintf("%s[%d]", key, earlier), value.Content[earlier].Value), watchedWorkflowNamed(field, entry.Value))
			}
		}
		workflows = append(workflows, workflow)
	}
	return workflows, nil
}

// parseWatchedWorkflow is one entry of `watched_workflows`, `<owner>/<repo>/<file>`, optionally
// followed by `@` and comma-separated events, or a refusal that begins with field, the entry's key
// and place, and the entry where it has an entry's shape (watchedWorkflowNamed). The owner and the
// repository are a repository ghrepo.Parse takes, the file a name workflowFilePattern takes, and
// each event one of gitHubTriggerEvents, compared with case, named once in the entry, in the
// entry's order; an entry with no `@` watches defaultWatchedEvent alone, and an `@` with no event,
// or an empty one, is refused rather than read as that default. A refusal of an entry of no
// entry's shape names a part of it only where that part fits its own grammar and holds no token's
// run: the repository where a refusal may name it (parseRepository, repositoryNamed), in Parse's
// words, and an event where it has a key's shape (keyShape), as every trigger event has; a file
// that is no file name is never named, and an event of no key's shape is named by its place in the
// entry (shapelessWord).
func parseWatchedWorkflow(field, entry string) (WatchedWorkflow, error) {
	at := watchedWorkflowNamed(field, entry)
	path, events, named := strings.Cut(entry, "@")
	parts := strings.Split(path, "/")
	if len(parts) != 3 {
		return WatchedWorkflow{}, fmt.Errorf("%s is not %s", at, watchedWorkflowForm)
	}
	repository, err := parseRepository(at+": the repository", parts[0]+"/"+parts[1])
	if err != nil {
		return WatchedWorkflow{}, err
	}
	if !workflowFilePattern.MatchString(parts[2]) {
		return WatchedWorkflow{}, fmt.Errorf(`%s: the workflow file is not a file name of .github/workflows: letters, digits, ".", "_" and "-", ending in ".yml" or ".yaml"`, at)
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
		case !slices.Contains(gitHubTriggerEvents, event) && !keyShape(event):
			return WatchedWorkflow{}, shapelessWord(fmt.Sprintf("%s event %d", at, index+1),
				`one of GitHub's trigger events, which a workflow's "on:" names in lower case (`+gitHubTriggerEventsURL+")")
		case !slices.Contains(gitHubTriggerEvents, event):
			return WatchedWorkflow{}, fmt.Errorf(`%s: %q is not one of GitHub's trigger events, which a workflow's "on:" names in lower case (%s)`, at, event, gitHubTriggerEventsURL)
		case slices.Contains(workflow.Events[:index], event):
			return WatchedWorkflow{}, fmt.Errorf("%s names the event %q twice", at, event)
		}
	}
	return workflow, nil
}

// watchedWorkflowShape reports whether a refusal may name entry, an entry of `watched_workflows`,
// as written: `<owner>/<repo>/<file>`, its owner and repository of a repository's shape
// (repositoryShape) and its file a name workflowFilePattern takes, optionally followed by "@" and
// comma-separated events, each empty or of a key's shape (keyShape), as every one of
// gitHubTriggerEvents is, the whole holding no token's run in any case (tokenShapedInAnyCase). Any
// other entry can be a token pasted where the entry goes, and is named by its place. A token pasted
// as the whole entry has no such three parts; the grammars of a repository and of a file take
// capitals, as GitHub's names do, so a token of mixed case written in one of those parts
// (`acme/<token>/x.yaml`) fits them, and only its run, read in lower case, shows it.
func watchedWorkflowShape(entry string) bool {
	path, events, _ := strings.Cut(entry, "@")
	parts := strings.Split(path, "/")
	if len(parts) != 3 || !repositoryShape.MatchString(parts[0]+"/"+parts[1]) || !workflowFilePattern.MatchString(parts[2]) || tokenShapedInAnyCase(entry) {
		return false
	}
	for event := range strings.SplitSeq(events, ",") {
		if event != "" && !keyShape(event) {
			return false
		}
	}
	return true
}

// watchedWorkflowNamed is field, an entry's key and place, and entry, the entry there, as a
// refusal names them: field and the entry quoted where it has an entry's shape
// (watchedWorkflowShape), and field alone where it has none, as wordNamed names a word.
func watchedWorkflowNamed(field, entry string) string {
	if watchedWorkflowShape(entry) {
		return field + " " + strconv.Quote(entry)
	}
	return field
}

// watchedWorkflowsValueText is a value of `watched_workflows` that is not of the kind its place
// takes, as a refusal names it: a string quoted where it has an entry's shape
// (watchedWorkflowShape), and any other value by its kind alone, never as written, since a string
// of no entry's shape, a number and a scalar of another tag can each be a token pasted where the
// list or an entry goes.
func watchedWorkflowsValueText(node *yaml.Node) string {
	switch node.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	case yaml.AliasNode:
		return "an alias"
	}
	switch node.Tag {
	case "!!str":
		if watchedWorkflowShape(node.Value) {
			return strconv.Quote(node.Value)
		}
		return "a string"
	case "!!null":
		return "null"
	case "!!bool":
		return "a boolean"
	case "!!int", "!!float":
		return "a number"
	case "!!timestamp":
		return "a timestamp"
	}
	return "a scalar of another tag"
}
