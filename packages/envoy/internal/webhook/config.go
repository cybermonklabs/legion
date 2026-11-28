package webhook

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/ghrename"
)

// BodyBudgetEnvVar sets the listener's webhook body budget (WebhookConfig.BodyBudget).
const BodyBudgetEnvVar = "ENVOY_WEBHOOK_BODY_BUDGET"

// WebhookConfig holds parsed configuration for enabled webhook providers.
// Nil pointer = provider not enabled.
type WebhookConfig struct {
	GitHub     *GitHubWebhook
	Slack      *SlackWebhook
	GhostWispr *GhostWisprWebhook
	// BodyBudget is the bytes of webhook body the listener holds at once across every route
	// (BodyBudget), from BodyBudgetEnvVar: a positive whole number of bytes, DefaultBodyBudget
	// when unset or blank.
	BodyBudget int64
}

// GitHubWebhook holds GitHub-specific webhook configuration.
type GitHubWebhook struct {
	Secret         string
	MentionTrigger string
	ReviewerAppID  string
	// VerdictAppID is ENVOY_VERDICT_APP_ID, trimmed: the App whose own check suites the listener
	// leaves out of the CI record. It is optional; empty leaves no suite out.
	VerdictAppID string
	// RepositoryRenames is each repository GitHub renamed, as ENVOY_GITHUB_REPOSITORY_RENAMES
	// configures it: the listener publishes a copy of every envelope of the current repository
	// under the former name, so a session that subscribed before the rename keeps hearing about
	// it. The pair is one-way — a current name is published under its former name, never the
	// other way round — and the zero value configures none.
	RepositoryRenames ghrename.Renames
}

// SlackWebhook holds Slack-specific webhook configuration.
type SlackWebhook struct {
	Secret string
}

// GhostWisprWebhook holds Ghost Wispr-specific webhook configuration. Secret is never empty:
// LoadWebhookConfig refuses the start when ENVOY_GHOSTWISPR_SIGNING_SECRET is empty.
type GhostWisprWebhook struct {
	Secret string
}

// LoadWebhookConfig parses ENVOY_WEBHOOKS, validates required secrets and reads the body budget
// (parseBodyBudget).
func LoadWebhookConfig() (*WebhookConfig, error) {
	budget, err := parseBodyBudget(os.Getenv(BodyBudgetEnvVar))
	if err != nil {
		return nil, err
	}
	cfg := &WebhookConfig{BodyBudget: budget}
	raw := os.Getenv("ENVOY_WEBHOOKS")
	if raw == "" {
		return cfg, nil
	}

	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		switch p {
		case "github":
			secret := strings.TrimSpace(os.Getenv("ENVOY_GITHUB_WEBHOOK_SECRET"))
			if secret == "" {
				return nil, fmt.Errorf("ENVOY_GITHUB_WEBHOOK_SECRET required when github enabled")
			}
			trigger := strings.TrimSpace(os.Getenv("ENVOY_GITHUB_MENTION_TRIGGER"))
			if trigger == "" {
				trigger = "@legion"
			}
			renames, err := ghrename.Parse(os.Getenv("ENVOY_GITHUB_REPOSITORY_RENAMES"))
			if err != nil {
				return nil, fmt.Errorf("ENVOY_GITHUB_REPOSITORY_RENAMES: %w", err)
			}
			reviewerAppID := strings.TrimSpace(os.Getenv("ENVOY_REVIEWER_APP_ID"))
			if reviewerAppID == "" {
				return nil, fmt.Errorf("ENVOY_REVIEWER_APP_ID required when github enabled")
			}
			// A verdict App id that is not a GitHub App's would match no suite and leave the
			// filter off without a word, so it refuses the start; unset or blank is no verdict App.
			verdictAppID := strings.TrimSpace(os.Getenv("ENVOY_VERDICT_APP_ID"))
			if verdictAppID != "" && !contracts.IsGithubAppID(verdictAppID) {
				return nil, fmt.Errorf("ENVOY_VERDICT_APP_ID %q is not a GitHub App id: a whole number above 0, with no leading zero; leave it unset for no verdict App", verdictAppID)
			}
			cfg.GitHub = &GitHubWebhook{
				Secret:            secret,
				MentionTrigger:    trigger,
				ReviewerAppID:     reviewerAppID,
				VerdictAppID:      verdictAppID,
				RepositoryRenames: renames,
			}
		case "slack":
			secret := strings.TrimSpace(os.Getenv("ENVOY_SLACK_SIGNING_SECRET"))
			if secret == "" {
				return nil, fmt.Errorf("ENVOY_SLACK_SIGNING_SECRET required when slack enabled")
			}
			cfg.Slack = &SlackWebhook{Secret: secret}
		case "ghostwispr":
			secret := strings.TrimSpace(os.Getenv("ENVOY_GHOSTWISPR_SIGNING_SECRET"))
			if secret == "" {
				return nil, fmt.Errorf("ENVOY_GHOSTWISPR_SIGNING_SECRET required when ghostwispr enabled")
			}
			cfg.GhostWispr = &GhostWisprWebhook{Secret: secret}
		default:
			return nil, fmt.Errorf("unknown webhook provider %q in ENVOY_WEBHOOKS", p)
		}
	}
	return cfg, nil
}

// parseBodyBudget reads BodyBudgetEnvVar: unset or blank is DefaultBodyBudget, and otherwise a
// positive whole number of bytes. 0, a negative number or a word is refused naming the variable
// and the value, and the listener does not start: an operator who wrote a value meant a budget.
func parseBodyBudget(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return DefaultBodyBudget, nil
	}
	budget, err := strconv.ParseInt(value, 10, 64)
	if err != nil || budget < 1 {
		return 0, fmt.Errorf("invalid %s %q: want a positive whole number of bytes, or unset for %d", BodyBudgetEnvVar, value, DefaultBodyBudget)
	}
	return budget, nil
}

// DescribeBodyBudget is the listener's start line for a body budget of limit bytes.
func DescribeBodyBudget(limit int64) string {
	return fmt.Sprintf("listener webhook body budget: %d bytes (%.1f MiB) held at once across every webhook route", limit, float64(limit)/(1<<20))
}
