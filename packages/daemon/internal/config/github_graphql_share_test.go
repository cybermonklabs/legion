package config

import (
	"testing"
)

// github_graphql_share takes a share of a whole, a decimal or 1, and a file that names none is
// half (DefaultGitHubGraphQLShare): the deployment sets it near 1 divided by the daemons that
// share the installation, and the daemon pauses (1 - share) / share of a query's time after it.
func TestGitHubGraphQLShareTakesAShareOfAWhole(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64
	}{
		{"0.25", 0.25},
		{"0.5", 0.5},
		{"1", 1},
		{"1.0", 1},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"github_graphql_share: "+tc.value+"\n"), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.GitHubGraphQLShare != tc.want {
				t.Errorf("GitHubGraphQLShare = %v, want %v", cfg.GitHubGraphQLShare, tc.want)
			}
		})
	}
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.GitHubGraphQLShare != DefaultGitHubGraphQLShare || DefaultGitHubGraphQLShare != 0.5 {
		t.Errorf("GitHubGraphQLShare with no key = %v, want the default, 0.5", cfg.GitHubGraphQLShare)
	}
}

// A share at or below 0, which would pause forever after the first query, above 1, which would
// ask for a pause below nothing, and anything that is not a number are refused.
func TestGitHubGraphQLShareRefuses(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"0", "github_graphql_share must be above 0 and at most 1"},
		{"-0.5", "github_graphql_share must be above 0 and at most 1"},
		{"1.5", "github_graphql_share must be above 0 and at most 1"},
		{".nan", "github_graphql_share must be above 0 and at most 1"},
		{".inf", "github_graphql_share must be above 0 and at most 1"},
		{"1/4", "github_graphql_share must be a number"},
		{"\"0.5\"", "github_graphql_share must be a number"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+"github_graphql_share: "+tc.value+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Errorf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}
