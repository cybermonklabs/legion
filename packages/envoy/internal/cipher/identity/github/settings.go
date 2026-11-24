package github

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/oidc"
)

// LoadConfig reads this provider's own variables from getenv, in the order config.Load read them:
// BROKER_EXCHANGE_GITHUB_ISSUER and BROKER_EXCHANGE_GITHUB_AUDIENCE (each needing the other),
// then BROKER_EXCHANGE_WORKFLOWS and BROKER_EXCHANGE_JOB_WORKFLOWS. The job entries join
// Workflows after BROKER_EXCHANGE_WORKFLOWS' own, so New numbers a job entry after every other
// workflow entry. Every refusal is the one config.Load answered, word for word.
func LoadConfig(getenv func(string) string) (Config, error) {
	issuer, audience, err := oidc.ConfigFromEnv(getenv, "BROKER_EXCHANGE_GITHUB_ISSUER", "BROKER_EXCHANGE_GITHUB_AUDIENCE")
	if err != nil {
		return Config{}, err
	}
	c := Config{Issuer: issuer, Audience: audience}
	if c.Workflows, err = parseWorkflows("BROKER_EXCHANGE_WORKFLOWS", getenv("BROKER_EXCHANGE_WORKFLOWS")); err != nil {
		return Config{}, err
	}
	if len(c.Workflows) > 0 && c.Issuer == "" {
		return Config{}, fmt.Errorf("BROKER_EXCHANGE_WORKFLOWS needs BROKER_EXCHANGE_GITHUB_ISSUER and BROKER_EXCHANGE_GITHUB_AUDIENCE, whose tokens its runs present")
	}
	jobs, err := parseJobWorkflows("BROKER_EXCHANGE_JOB_WORKFLOWS", getenv("BROKER_EXCHANGE_JOB_WORKFLOWS"))
	if err != nil {
		return Config{}, err
	}
	if len(jobs) > 0 && c.Issuer == "" {
		return Config{}, fmt.Errorf("BROKER_EXCHANGE_JOB_WORKFLOWS needs BROKER_EXCHANGE_GITHUB_ISSUER and BROKER_EXCHANGE_GITHUB_AUDIENCE, whose tokens its runs present")
	}
	// One workflow run has one answer: listed in both settings, it would mint two kinds of token.
	// New refuses it too, but only this refusal can name both settings.
	listed := make(map[string]bool, len(c.Workflows))
	for _, entry := range c.Workflows {
		listed[workflowName(entry)] = true
	}
	for _, entry := range jobs {
		if name := workflowName(entry); listed[name] {
			return Config{}, fmt.Errorf("BROKER_EXCHANGE_JOB_WORKFLOWS and BROKER_EXCHANGE_WORKFLOWS both name the workflow %s; one workflow run has one answer, a token that acts for its person or a job's that names none, so name it in one of them", name)
		}
	}
	c.Workflows = append(c.Workflows, jobs...)
	return c, nil
}

// Serves answers nil for every use: no setting names a GitHub account as an API or NATS caller,
// so no site of one reaches this provider.
func (c Config) Serves(identity.Use, identity.Site) error { return nil }

// Mentions is empty: this provider's settings name workflows, never an account another setting
// could name.
func (c Config) Mentions(identity.Account) []identity.Mention { return nil }

// Services is empty: a workflow entry names a person, and a job workflow's token names the
// workflow itself, never a service another setting could name.
func (c Config) Services() []string { return nil }

// workflowName is a workflow as a refusal names it and as both workflow settings key it:
// <repository>=<workflow path>=<ref>.
func workflowName(entry WorkflowEntry) string {
	return entry.Repository + "=" + entry.Workflow + "=" + entry.Ref
}

// parseWorkflows reads setting's whitespace-separated
// <repository>=<workflow path>=<ref>=<project>=<person>[=<session>] entries, the identities
// BROKER_EXCHANGE_WORKFLOWS lists, in order. It refuses, naming the entry: one not of that form or
// with a blank field, and a repository, workflow and ref listed twice, since one workflow run
// writes as one identity. New is the second gate, and the repository's own form is its to refuse.
func parseWorkflows(setting, value string) ([]WorkflowEntry, error) {
	var entries []WorkflowEntry
	seen := map[string]bool{}
	for _, text := range strings.Fields(value) {
		fields, ok := exchange.EntryFields(text, 6)
		if !ok || fields[0] == "" || fields[1] == "" || fields[2] == "" || fields[3] == "" || fields[4] == "" {
			return nil, fmt.Errorf("%s must be <repository>=<workflow path>=<ref>=<project>=<person>[=<session>] entries, the session everything after the fifth =, got %q", setting, text)
		}
		entry := WorkflowEntry{Repository: fields[0], Workflow: fields[1], Ref: fields[2], Project: fields[3], Person: fields[4], Session: fields[5]}
		key := workflowName(entry)
		if seen[key] {
			return nil, fmt.Errorf("%s names the workflow %s twice; one workflow run writes as one identity, so name it once", setting, key)
		}
		seen[key] = true
		entries = append(entries, entry)
	}
	return entries, nil
}

// parseJobWorkflows reads setting's whitespace-separated
// <repository>=<workflow path>=<ref>=<projects>[=<session>] entries, the job workflows
// BROKER_EXCHANGE_JOB_WORKFLOWS lists, in order, each with its projects as Job and no person and
// no project. It refuses, naming the setting and the entry: one not of that form or with a blank
// repository, workflow or ref; projects exchange.ParseSettingJobProjects refuses; and a
// repository, workflow and ref listed twice, since one workflow run writes as one identity. A
// workflow BROKER_EXCHANGE_WORKFLOWS lists too is LoadConfig's to refuse, since it reads both
// settings. New is the second gate, and the repository's and the session name's own forms are its
// to refuse.
func parseJobWorkflows(setting, value string) ([]WorkflowEntry, error) {
	var entries []WorkflowEntry
	seen := map[string]bool{}
	for _, text := range strings.Fields(value) {
		fields, ok := exchange.EntryFields(text, 5)
		if !ok || fields[0] == "" || fields[1] == "" || fields[2] == "" {
			return nil, fmt.Errorf("%s must be <repository>=<workflow path>=<ref>=<projects>[=<session>] entries, the session everything after the fourth =, got %q", setting, text)
		}
		job, err := exchange.ParseSettingJobProjects(setting, text, fields[3])
		if err != nil {
			return nil, err
		}
		entry := WorkflowEntry{Repository: fields[0], Workflow: fields[1], Ref: fields[2], Job: job, Session: fields[4]}
		key := workflowName(entry)
		if seen[key] {
			return nil, fmt.Errorf("%s names the workflow %s twice, got %q again; one workflow run writes as one identity, so name it once", setting, key, text)
		}
		seen[key] = true
		entries = append(entries, entry)
	}
	return entries, nil
}
