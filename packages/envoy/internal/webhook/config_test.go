package webhook

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ghrename"
)

func TestLoadWebhookConfig(t *testing.T) {
	cases := []struct {
		name              string
		env               map[string]string
		wantGitHub        bool
		wantSlack         bool
		wantGhostW        bool
		wantReviewerAppID string
		wantErr           bool
	}{
		{name: "empty ENVOY_WEBHOOKS", env: map[string]string{"ENVOY_WEBHOOKS": ""}},
		{name: "unset ENVOY_WEBHOOKS", env: map[string]string{}},
		{
			name:    "github enabled without reviewer app ID → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "github", "ENVOY_GITHUB_WEBHOOK_SECRET": "s"},
			wantErr: true,
		},
		{
			name:              "github enabled with secret and reviewer app ID",
			env:               map[string]string{"ENVOY_WEBHOOKS": "github", "ENVOY_GITHUB_WEBHOOK_SECRET": "s", "ENVOY_REVIEWER_APP_ID": "app123"},
			wantGitHub:        true,
			wantReviewerAppID: "app123",
		},
		{
			name: "github reviewer app ID is trimmed",
			env: map[string]string{
				"ENVOY_WEBHOOKS":              "github",
				"ENVOY_GITHUB_WEBHOOK_SECRET": "s",
				"ENVOY_REVIEWER_APP_ID":       " 12345 ",
			},
			wantGitHub:        true,
			wantReviewerAppID: "12345",
		},
		{
			name:    "github enabled without secret → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "github"},
			wantErr: true,
		},
		{
			name:      "slack enabled with secret",
			env:       map[string]string{"ENVOY_WEBHOOKS": "slack", "ENVOY_SLACK_SIGNING_SECRET": "s"},
			wantSlack: true,
		},
		{
			name:    "slack enabled without secret → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "slack"},
			wantErr: true,
		},
		{
			name:    "ghostwispr enabled without secret → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "ghostwispr"},
			wantErr: true,
		},
		{
			name: "ghostwispr enabled with secret",
			env: map[string]string{
				"ENVOY_WEBHOOKS":                  "ghostwispr",
				"ENVOY_GHOSTWISPR_SIGNING_SECRET": "s",
			},
			wantGhostW: true,
		},
		{
			name: "all three enabled",
			env: map[string]string{
				"ENVOY_WEBHOOKS":                  "github,slack,ghostwispr",
				"ENVOY_GITHUB_WEBHOOK_SECRET":     "s",
				"ENVOY_SLACK_SIGNING_SECRET":      "s",
				"ENVOY_GHOSTWISPR_SIGNING_SECRET": "s",
				"ENVOY_REVIEWER_APP_ID":           "app123",
			},
			wantGitHub: true, wantSlack: true, wantGhostW: true, wantReviewerAppID: "app123",
		},
		{
			name: "whitespace trimming",
			env: map[string]string{
				"ENVOY_WEBHOOKS":              " github , slack ",
				"ENVOY_GITHUB_WEBHOOK_SECRET": "s",
				"ENVOY_SLACK_SIGNING_SECRET":  "s",
				"ENVOY_REVIEWER_APP_ID":       "app123",
			},
			wantGitHub: true, wantSlack: true, wantReviewerAppID: "app123",
		},
		{
			name:    "unknown provider → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "unknown"},
			wantErr: true,
		},
		{
			name:    "github with whitespace-only secret → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "github", "ENVOY_GITHUB_WEBHOOK_SECRET": "  "},
			wantErr: true,
		},
		{
			name:    "slack with whitespace-only secret → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "slack", "ENVOY_SLACK_SIGNING_SECRET": "  \t "},
			wantErr: true,
		},
		{
			name:    "github with whitespace-only reviewer app ID → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "github", "ENVOY_GITHUB_WEBHOOK_SECRET": "s", "ENVOY_REVIEWER_APP_ID": "  "},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENVOY_REVIEWER_APP_ID", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := LoadWebhookConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if (cfg.GitHub != nil) != tc.wantGitHub {
				t.Errorf("GitHub enabled = %v, want %v", cfg.GitHub != nil, tc.wantGitHub)
			}
			if (cfg.Slack != nil) != tc.wantSlack {
				t.Errorf("Slack enabled = %v, want %v", cfg.Slack != nil, tc.wantSlack)
			}
			if (cfg.GhostWispr != nil) != tc.wantGhostW {
				t.Errorf("GhostWispr enabled = %v, want %v", cfg.GhostWispr != nil, tc.wantGhostW)
			}
			if cfg.GitHub != nil && cfg.GitHub.ReviewerAppID != tc.wantReviewerAppID {
				t.Errorf("GitHub.ReviewerAppID = %q, want %q", cfg.GitHub.ReviewerAppID, tc.wantReviewerAppID)
			}
		})
	}
}

// TestLoadWebhookConfigRefusesGhostWisprWithoutASecret reads the start-up path the listener's main
// calls and stops on: Ghost Wispr enabled with no signing secret refuses the start with an error
// naming ENVOY_GHOSTWISPR_SIGNING_SECRET, as Slack and GitHub with no secret do, and Ghost Wispr
// with a secret gets its route with that secret.
func TestLoadWebhookConfigRefusesGhostWisprWithoutASecret(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        map[string]string
		wantErr    string // the variable the error names; empty wants the Ghost Wispr route
		wantSecret string
	}{
		{
			name:    "ghostwispr with no signing secret",
			env:     map[string]string{"ENVOY_WEBHOOKS": "ghostwispr"},
			wantErr: "ENVOY_GHOSTWISPR_SIGNING_SECRET",
		},
		{
			name:    "ghostwispr with a blank signing secret",
			env:     map[string]string{"ENVOY_WEBHOOKS": "ghostwispr", "ENVOY_GHOSTWISPR_SIGNING_SECRET": " \t "},
			wantErr: "ENVOY_GHOSTWISPR_SIGNING_SECRET",
		},
		{
			name: "ghostwispr beside github and slack, which have their secrets",
			env: map[string]string{
				"ENVOY_WEBHOOKS":              "github,slack,ghostwispr",
				"ENVOY_GITHUB_WEBHOOK_SECRET": "s",
				"ENVOY_SLACK_SIGNING_SECRET":  "s",
			},
			wantErr: "ENVOY_GHOSTWISPR_SIGNING_SECRET",
		},
		{
			name:    "control: slack with no signing secret",
			env:     map[string]string{"ENVOY_WEBHOOKS": "slack"},
			wantErr: "ENVOY_SLACK_SIGNING_SECRET",
		},
		{
			name:    "control: github with no webhook secret",
			env:     map[string]string{"ENVOY_WEBHOOKS": "github"},
			wantErr: "ENVOY_GITHUB_WEBHOOK_SECRET",
		},
		{
			name:       "control: ghostwispr with a signing secret",
			env:        map[string]string{"ENVOY_WEBHOOKS": "ghostwispr", "ENVOY_GHOSTWISPR_SIGNING_SECRET": "s3cret"},
			wantSecret: "s3cret",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"ENVOY_GHOSTWISPR_SIGNING_SECRET", "ENVOY_SLACK_SIGNING_SECRET", "ENVOY_GITHUB_WEBHOOK_SECRET"} {
				t.Setenv(k, "")
			}
			t.Setenv("ENVOY_REVIEWER_APP_ID", "1")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := LoadWebhookConfig()
			route := cfg != nil && cfg.GhostWispr != nil
			t.Logf("LoadWebhookConfig(ENVOY_WEBHOOKS=%s) = ghostwispr route %v, err %v", tc.env["ENVOY_WEBHOOKS"], route, err)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want an error naming %s", err, tc.wantErr)
				}
				if cfg != nil {
					t.Fatalf("cfg = %+v beside the error, want none", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want the Ghost Wispr route", err)
			}
			if !route || cfg.GhostWispr.Secret != tc.wantSecret {
				t.Fatalf("cfg.GhostWispr = %+v, want the route with secret %q", cfg.GhostWispr, tc.wantSecret)
			}
		})
	}
}

// TestLoadWebhookConfigRepositoryRenames covers ENVOY_GITHUB_REPOSITORY_RENAMES, which the github
// branch reads: the pairs it holds reach the config, and a value ghrename.Parse refuses refuses
// the listener's start with a line naming the variable, since that line is all an operator gets.
func TestLoadWebhookConfigRepositoryRenames(t *testing.T) {
	cases := []struct {
		name    string
		renames string
		want    []ghrename.Rename
		wantErr bool
	}{
		{name: "unset", renames: ""},
		{
			name:    "one pair",
			renames: "sjawhar/legion=cybermonklabs/legion",
			want:    []ghrename.Rename{{Former: "sjawhar/legion", Current: "cybermonklabs/legion"}},
		},
		{
			name:    "two pairs",
			renames: "sjawhar/legion=cybermonklabs/legion, acme/old=acme/new",
			want: []ghrename.Rename{
				{Former: "sjawhar/legion", Current: "cybermonklabs/legion"},
				{Former: "acme/old", Current: "acme/new"},
			},
		},
		{name: "an entry that is no = pair → error", renames: "sjawhar/legion", wantErr: true},
		{name: "an entry with two = → error", renames: "a/b=c/d=e/f", wantErr: true},
		{name: "a side that is no owner/name → error", renames: "legion=cybermonklabs/legion", wantErr: true},
		{name: "a pair whose sides are one repository → error", renames: "sjawhar/legion=SJAWHAR/legion", wantErr: true},
		{
			name:    "a repository named in two pairs → error",
			renames: "sjawhar/legion=cybermonklabs/legion,sjawhar/legion=acme/new",
			wantErr: true,
		},
		{
			name:    "a chain of two pairs → error",
			renames: "sjawhar/legion=cybermonklabs/legion,cybermonklabs/legion=acme/new",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENVOY_WEBHOOKS", "github")
			t.Setenv("ENVOY_GITHUB_WEBHOOK_SECRET", "s")
			t.Setenv("ENVOY_REVIEWER_APP_ID", "app123")
			t.Setenv("ENVOY_GITHUB_REPOSITORY_RENAMES", tc.renames)

			cfg, err := LoadWebhookConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ENVOY_GITHUB_REPOSITORY_RENAMES=%q loaded %+v, want a refusal", tc.renames, cfg.GitHub.RepositoryRenames.Pairs())
				}
				if !strings.Contains(err.Error(), "ENVOY_GITHUB_REPOSITORY_RENAMES") {
					t.Errorf("ENVOY_GITHUB_REPOSITORY_RENAMES=%q was refused with %q, want a refusal naming the variable", tc.renames, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			renames := cfg.GitHub.RepositoryRenames
			if renames.IsZero() != (len(tc.want) == 0) {
				t.Errorf("GitHub.RepositoryRenames.IsZero() = %v, want %v", renames.IsZero(), len(tc.want) == 0)
			}
			pairs := renames.Pairs()
			if len(pairs) != len(tc.want) {
				t.Fatalf("GitHub.RepositoryRenames.Pairs() = %v, want %v", pairs, tc.want)
			}
			for i, want := range tc.want {
				if pairs[i] != want {
					t.Errorf("GitHub.RepositoryRenames.Pairs()[%d] = %+v, want %+v", i, pairs[i], want)
				}
			}
		})
	}
}
