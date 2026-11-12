package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/dispatch/api"
)

// decisionConfig is the decision model the command bar asks before it answers, as
// resolveDecisionModel checked it: the model's name (DISPATCH_DECISION_MODEL), the base URL of
// its System One endpoint (DISPATCH_DECISION_MODEL_ENDPOINT) and the file holding the endpoint's
// bearer (DISPATCH_DECISION_MODEL_TOKEN_FILE). Each is empty where the deployment set none.
type decisionConfig struct {
	Model     string
	Endpoint  string
	TokenFile string
}

// resolveDecisionModel reads the three decision-model settings and refuses, naming the one
// missing, an endpoint without its token file or a token file without its endpoint: the two are
// set together or not at all. The model is optional, as a deployment renders the endpoint and the
// file before it names a model. The endpoint must be an absolute https URL with a host and no
// query or fragment, as the chat endpoints must (chatEndpoint). It calls nothing and reads no
// file, so an endpoint that is down, or a token file not written yet, never refuses boot.
func resolveDecisionModel(env settingValues) (decisionConfig, error) {
	cfg := decisionConfig{
		Model:     strings.TrimSpace(env.get("DISPATCH_DECISION_MODEL")),
		Endpoint:  strings.TrimSpace(env.get("DISPATCH_DECISION_MODEL_ENDPOINT")),
		TokenFile: strings.TrimSpace(env.get("DISPATCH_DECISION_MODEL_TOKEN_FILE")),
	}
	switch {
	case cfg.Endpoint != "" && cfg.TokenFile == "":
		return decisionConfig{}, errors.New("DISPATCH_DECISION_MODEL_TOKEN_FILE is required when DISPATCH_DECISION_MODEL_ENDPOINT is set: the file holding the bearer token every decision call presents")
	case cfg.Endpoint == "" && cfg.TokenFile != "":
		return decisionConfig{}, errors.New("DISPATCH_DECISION_MODEL_ENDPOINT is required when DISPATCH_DECISION_MODEL_TOKEN_FILE is set: the base URL of the System One endpoint the token file's bearer is presented to")
	case cfg.Endpoint == "":
		return cfg, nil
	}
	parsed, err := url.Parse(cfg.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || strings.ContainsAny(cfg.Endpoint, "?#") {
		return decisionConfig{}, fmt.Errorf("DISPATCH_DECISION_MODEL_ENDPOINT=%q (expected an absolute https URL with a host and no query or fragment, the base URL the System One route is appended to)", cfg.Endpoint)
	}
	return cfg, nil
}

// newDecider builds the decision model cfg names, or nil where there is none to build, which is
// a server that boots and decides nothing rather than one that refuses to boot: no endpoint, no
// model named, or a model the committed measurement does not hold (going.DecisionRecord), since
// a model nobody measured has no thresholds and is no decision model (R11). Each of those logs
// one line saying why. The decider's token is the file's contents at the moment of each call
// (going.FileTokenSource), so a file that is absent or empty fails that call as the decision
// model that cannot be reached (api.ErrDecisionModelUnreachable) and never the boot. The line it
// logs for a decider names the model, the endpoint's host and the record's thresholds, never a
// credential.
func newDecider(cfg decisionConfig) *api.Decider {
	switch {
	case cfg.Endpoint == "" && cfg.Model == "":
		slog.Warn("dispatch: no decision model configured, so the command bar decides nothing", "setting", "DISPATCH_DECISION_MODEL")
		return nil
	case cfg.Endpoint == "":
		slog.Warn("dispatch: a decision model is named but no endpoint is configured, so the command bar decides nothing", "model", cfg.Model, "setting", "DISPATCH_DECISION_MODEL_ENDPOINT")
		return nil
	case cfg.Model == "":
		slog.Warn("dispatch: a decision endpoint is configured but no model is named, so the command bar decides nothing", "setting", "DISPATCH_DECISION_MODEL")
		return nil
	}
	decider, measured := api.NewDecider(agentloop.Model{
		Endpoint: agentloop.Endpoint{
			Kind:    agentloop.SystemOne,
			BaseURL: cfg.Endpoint,
			Token:   going.FileTokenSource{Path: cfg.TokenFile}.Token,
		},
		ID: cfg.Model,
	})
	if !measured {
		slog.Warn("dispatch: the decision model is not one the committed measurement holds, so it has no thresholds and the command bar decides nothing", "model", cfg.Model, "record", "packages/envoy/going/decision_models.json")
		return nil
	}
	host := cfg.Endpoint
	if parsed, err := url.Parse(cfg.Endpoint); err == nil {
		host = strings.ToLower(parsed.Host)
	}
	record := decider.Record()
	slog.Info("dispatch: decides with a measured decision model", "model", cfg.Model, "host", host,
		"action_threshold", threshold(record.ActionThreshold), "chart_threshold", threshold(record.ChartThreshold))
	return decider
}

// threshold is a record's threshold as the boot line writes it: its value, or "none" where the
// fit did not hold.
func threshold(value *float64) any {
	if value == nil {
		return "none"
	}
	return *value
}
