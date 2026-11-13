package main

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/rules"
)

// writingCheckModelRows are the settings of the model the writing check's reader calls, by the
// suffix each takes after DISPATCH_WRITING_CHECK_AZURE_.
const writingCheckModelPrefix = "DISPATCH_WRITING_CHECK_AZURE_"

// writingCheckConfig is the writing check as resolveWritingCheck read it (INFRA-3231). Model is
// false where no model is configured: the structure rules then run alone.
type writingCheckConfig struct {
	Enabled  bool
	RulesOff []string
	Detector bool
	// Model is whether the reader's model group is set and read: the check and its detector
	// on, and the group whole. The fields below are empty where it is false.
	Model           bool
	Endpoint        string
	Deployment      string
	APIVersion      string
	Scope           string
	TokensPerMinute int
	TenantID        string
	ClientID        string
	FederatedToken  string
	// ReaderTimeout is DISPATCH_TEST_WRITING_CHECK_READER_TIMEOUT, read only under the test
	// hooks; zero keeps the reader's own two seconds.
	ReaderTimeout time.Duration
}

// writingCheckReadsSignIn reports whether the writing check's model group is set at all, which
// makes the check a reader of the shared Azure sign-in: the group is accepted and ignored while
// the check or its detector is off, so the sign-in beside it is accepted then too.
func writingCheckReadsSignIn(env settingValues) bool {
	return len(settingsSet(env, writingCheckModelPrefix)) > 0
}

// resolveWritingCheck reads DISPATCH_WRITING_CHECK and the settings beside it, and refuses,
// naming the setting, every shape the check could not be built from: a value that is not on or
// off, a rule id the check does not carry, and, with the check and its detector on, a model
// group set in part. With the check or its detector off the model group is accepted and
// ignored. It calls nothing and reads no file, so a model service that is down never refuses
// boot.
func resolveWritingCheck(env settingValues, testHooksEnabled bool) (writingCheckConfig, error) {
	var cfg writingCheckConfig
	var err error
	if cfg.Enabled, err = onOffSetting(env, "DISPATCH_WRITING_CHECK", false); err != nil {
		return writingCheckConfig{}, err
	}
	if cfg.Detector, err = onOffSetting(env, "DISPATCH_WRITING_CHECK_DETECTOR", true); err != nil {
		return writingCheckConfig{}, err
	}
	known := []string{writingcheck.DetectorRule}
	for _, rule := range rules.All() {
		known = append(known, rule.ID)
	}
	for _, id := range strings.Split(env.get("DISPATCH_WRITING_CHECK_RULES_OFF"), ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !slices.Contains(known, id) {
			return writingCheckConfig{}, fmt.Errorf("DISPATCH_WRITING_CHECK_RULES_OFF names %q, which is no rule of the writing check (expected a comma list of %s)", id, strings.Join(known, ", "))
		}
		if !slices.Contains(cfg.RulesOff, id) {
			cfg.RulesOff = append(cfg.RulesOff, id)
		}
	}
	if err := testHookDuration(env, "DISPATCH_TEST_WRITING_CHECK_READER_TIMEOUT", testHooksEnabled, &cfg.ReaderTimeout); err != nil {
		return writingCheckConfig{}, err
	}
	detectorRuns := cfg.Enabled && cfg.Detector && !slices.Contains(cfg.RulesOff, writingcheck.DetectorRule)
	if !detectorRuns || !writingCheckReadsSignIn(env) {
		return cfg, nil
	}
	cfg.Model = true
	cfg.Endpoint = strings.TrimSpace(env.get("DISPATCH_WRITING_CHECK_AZURE_ENDPOINT"))
	cfg.Deployment = strings.TrimSpace(env.get("DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT"))
	quota := strings.TrimSpace(env.get("DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE"))
	var missing []string
	for _, row := range []struct{ name, value string }{
		{"DISPATCH_WRITING_CHECK_AZURE_ENDPOINT", cfg.Endpoint},
		{"DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT", cfg.Deployment},
		{"DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE", quota},
	} {
		if row.value == "" {
			missing = append(missing, row.name)
		}
	}
	if len(missing) > 0 {
		return writingCheckConfig{}, fmt.Errorf("%s set, but %s missing: with the writing check and its detector on, the reader's model needs its endpoint, its deployment and its tokens-per-minute quota, or none of its settings for no model",
			strings.Join(settingsSet(env, writingCheckModelPrefix), ", "), strings.Join(missing, ", "))
	}
	parsed, err := url.Parse(cfg.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || strings.ContainsAny(cfg.Endpoint, "?#") {
		return writingCheckConfig{}, fmt.Errorf("DISPATCH_WRITING_CHECK_AZURE_ENDPOINT=%q (expected an absolute https URL with a host and no query or fragment, the base URL the chat completions route is appended to)", cfg.Endpoint)
	}
	cfg.TokensPerMinute, err = strconv.Atoi(quota)
	if err != nil || cfg.TokensPerMinute <= 0 {
		return writingCheckConfig{}, fmt.Errorf("DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE=%q (expected a positive integer, the deployment's tokens-per-minute quota)", quota)
	}
	cfg.APIVersion = strings.TrimSpace(env.get("DISPATCH_WRITING_CHECK_AZURE_API_VERSION"))
	cfg.Scope = cmp.Or(strings.TrimSpace(env.get("DISPATCH_WRITING_CHECK_AZURE_SCOPE")), embed.DefaultEntraScope)
	signIn := readAzureSignIn(env)
	cfg.TenantID, cfg.ClientID, cfg.FederatedToken = signIn.TenantID, signIn.ClientID, signIn.TokenFile
	switch {
	case len(signIn.set) == 0:
		return writingCheckConfig{}, fmt.Errorf("the writing check's model needs the keyless Azure sign-in (%s): it takes no API key", azureKeylessNames)
	case len(signIn.missing) > 0:
		return writingCheckConfig{}, signIn.refusePartial()
	}
	return cfg, nil
}

// onOffSetting reads a setting that is on or off, unset or empty being fallback.
func onOffSetting(env settingValues, name string, fallback bool) (bool, error) {
	switch value := strings.TrimSpace(env.get(name)); value {
	case "":
		return fallback, nil
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s=%q (expected on or off)", name, value)
	}
}

// newWritingCheck builds the writing check cfg describes. The reader is a model of its own
// (reader.AzureModel over its own Entra token source) with a rate limiter of its own, sized by
// DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE: it shares neither the answerer's model nor its
// quota. With no model configured the check runs its structure rules alone, which it logs. It
// calls nothing and reads no token file, so a model service or a sign-in being down fails a
// reader call later, which lets the write through, never the boot.
func newWritingCheck(cfg writingCheckConfig) (*api.WritingCheck, error) {
	config := api.WritingCheckConfig{Enabled: cfg.Enabled, RulesOff: cfg.RulesOff, Detector: cfg.Detector}
	if !cfg.Enabled {
		return api.NewWritingCheck(config), nil
	}
	if !cfg.Model {
		slog.Info("dispatch: the writing check is on with no model for its detector, so it runs its structure rules alone",
			"rules_off", cfg.RulesOff, "detector", cfg.Detector)
		return api.NewWritingCheck(config), nil
	}
	tokens, err := embed.NewEntraTokenSource(embed.EntraConfig{
		TenantID:  cfg.TenantID,
		ClientID:  cfg.ClientID,
		TokenFile: cfg.FederatedToken,
		Scope:     cfg.Scope,
	})
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, errors.New("DISPATCH_WRITING_CHECK_AZURE_ENDPOINT is not a URL")
	}
	config.Reader = reader.New(reader.Config{
		Model:           reader.AzureModel(cfg.Endpoint, cfg.Deployment, cfg.APIVersion, tokens.Token),
		BundleModelID:   writingcheck.BundleModelID,
		Timeout:         cfg.ReaderTimeout,
		TokensPerMinute: cfg.TokensPerMinute,
	})
	config.Model = &api.WritingCheckModel{
		EndpointHost: endpoint.Host, Deployment: cfg.Deployment, APIVersion: cfg.APIVersion,
		TokensPerMinute: cfg.TokensPerMinute,
	}
	slog.Info("dispatch: the writing check is on with a model for its detector",
		"endpoint_host", endpoint.Host, "deployment", cfg.Deployment, "rules_off", cfg.RulesOff)
	return api.NewWritingCheck(config), nil
}
