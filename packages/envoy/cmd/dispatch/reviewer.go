package main

import (
	"fmt"
	"log/slog"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/modelpace"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
)

// newReviewer builds the reviewer of INFRA-3231's R5 (api.Reviewer), or nil where its model group
// (DISPATCH_REVIEWER_MODEL_*) is unset, which it logs: resolveWritingSettings has then refused
// every DISPATCH_REVIEWER_<KIND> but off, so nothing is reviewed and the draft route answers that
// no model is set. A set group builds the reviewer whatever the kinds say, since the draft route
// answers whenever the group is set. Its calls are charged to the group's share of its deployment
// on pacer, the process's one pacer, and point at the stand-in under DISPATCH_TEST_MODEL_URL. It
// is built from settings resolveWritingSettings has already checked, so an error here is a shape
// that check missed; it calls nothing and reads no file.
func newReviewer(cfg writingSettings, pacer *modelpace.Pacer) (*api.Reviewer, error) {
	group := cfg.ReviewerModel
	if group.Kind == "" {
		slog.Info("dispatch: the reviewer reads no text: its model group is unset", "setting", reviewerModelSpec.prefix+"KIND")
		return nil, nil
	}
	model, err := group.agentloopModel()
	if err != nil {
		return nil, fmt.Errorf("the reviewer's model: %w", err)
	}
	pace, err := group.pace(pacer)
	if err != nil {
		return nil, fmt.Errorf("the reviewer's model: %w", err)
	}
	slog.Info("dispatch: the reviewer reads agents' texts to people", "kind", group.Kind, "deployment", group.Deployment,
		"stand_in", group.StandIn, "asks", cfg.ReviewAsks, "issues", cfg.ReviewIssues, "comments", cfg.ReviewComments,
		"messages", cfg.ReviewMessages, "documents", cfg.ReviewDocuments, "decision_blocks", cfg.ReviewDecisionBlocks,
		"rules", cfg.ReviewerRules)
	return api.NewReviewer(api.ReviewerConfig{
		Asks: cfg.ReviewAsks, Issues: cfg.ReviewIssues, Comments: cfg.ReviewComments, Messages: cfg.ReviewMessages,
		Documents: cfg.ReviewDocuments, DecisionBlocks: cfg.ReviewDecisionBlocks,
		Rules:        cfg.ReviewerRules,
		Model:        reviewer.Model{Model: model, Pace: pace, Timeout: cfg.ReviewerTimeout},
		Timeout:      cfg.ReviewerTimeout,
		WriterWindow: cfg.ReviewerWriterWindow,
		Unedited:     cfg.ReviewerUnedited,
	}), nil
}

// newRewriteDefine builds a person's Rewrite and Define (api.RewriteDefine, INFRA-3231's R7), or
// nil where their model group (DISPATCH_REWRITE_DEFINE_MODEL_*) is unset, which it logs: their
// routes then answer that no model is set. They are on whenever the group is set, because a
// person chooses to use them. Their calls are charged to the group's share of its deployment on
// pacer, the process's one pacer, read the reviewer's rule sets (DISPATCH_REVIEWER_RULES), and
// point at the stand-in under DISPATCH_TEST_MODEL_URL. It calls nothing and reads no file.
func newRewriteDefine(cfg writingSettings, pacer *modelpace.Pacer) (*api.RewriteDefine, error) {
	group := cfg.RewriteDefineModel
	if group.Kind == "" {
		slog.Info("dispatch: Rewrite and Define are off: their model group is unset", "setting", rewriteDefineModelSpec.prefix+"KIND")
		return nil, nil
	}
	model, err := group.agentloopModel()
	if err != nil {
		return nil, fmt.Errorf("Rewrite and Define's model: %w", err)
	}
	pace, err := group.pace(pacer)
	if err != nil {
		return nil, fmt.Errorf("Rewrite and Define's model: %w", err)
	}
	slog.Info("dispatch: Rewrite and Define are on", "kind", group.Kind, "deployment", group.Deployment,
		"stand_in", group.StandIn, "rules", cfg.ReviewerRules)
	return api.NewRewriteDefine(api.RewriteDefineConfig{
		Model: reviewer.Model{Model: model, Pace: pace},
		Rules: cfg.ReviewerRules,
	}), nil
}

// newTestModels builds the models a test request may name in its Dispatch-Test-Model header
// (DISPATCH_TEST_MODELS, under the test hooks alone), by name: each one's model and its share of
// its deployment on pacer, the process's one pacer, at its kind's endpoint or the stand-in under
// DISPATCH_TEST_MODEL_URL. It is nil where the setting names none. Like the groups, it is built from
// settings resolveWritingSettings has already checked; it calls nothing and reads no file.
func newTestModels(cfg writingSettings, pacer *modelpace.Pacer) (map[string]reviewer.Model, error) {
	if len(cfg.TestModels) == 0 {
		return nil, nil
	}
	models := make(map[string]reviewer.Model, len(cfg.TestModels))
	for name, group := range cfg.TestModels {
		model, err := group.agentloopModel()
		if err != nil {
			return nil, fmt.Errorf("the test model %s: %w", name, err)
		}
		pace, err := group.pace(pacer)
		if err != nil {
			return nil, fmt.Errorf("the test model %s: %w", name, err)
		}
		models[name] = reviewer.Model{Model: model, Pace: pace}
	}
	slog.Info("dispatch: a test request may name a model of DISPATCH_TEST_MODELS", "models", len(models))
	return models, nil
}
