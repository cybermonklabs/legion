//go:build !cipher_no_github

package boot

import (
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/github"
	"github.com/sjawhar/envoy/internal/oidc"
)

func init() {
	register(entry{
		kind: github.Kind, rank: 30,
		zero: func() any { return github.Config{} },
		load: func(getenv func(string) string) (any, error) { return github.LoadConfig(getenv) },
		open: openGitHub,
		serves: func(cfg any, use identity.Use, site identity.Site) error {
			return cfg.(github.Config).Serves(use, site)
		},
		mentions: func(cfg any, account identity.Account) []identity.Mention {
			return cfg.(github.Config).Mentions(account)
		},
		services:  func(cfg any) []string { return cfg.(github.Config).Services() },
		exchanges: func(cfg any) bool { return cfg.(github.Config).Issuer != "" },
		exchangeFields: func(cfg any) []any {
			c := cfg.(github.Config)
			// The job entries LoadConfig appends to Workflows are counted apart, as the line
			// always counted BROKER_EXCHANGE_JOB_WORKFLOWS.
			jobs := 0
			for _, entry := range c.Workflows {
				if entry.Job.Listed() {
					jobs++
				}
			}
			return []any{"github", c.Issuer != "", "workflows", len(c.Workflows) - jobs, "job_workflows", jobs}
		},
	})
}

// openGitHub builds the GitHub provider from its own settings: the issuer
// BROKER_EXCHANGE_GITHUB_ISSUER names, read only where it is set, its audience, and the
// exchange's workflow entries. No setting wires a GitHub caller of the API or the NATS auth
// service.
func openGitHub(cfg any, _ Shared, keys func(string) (*oidc.Keys, error), _ *Boot) (opened, error) {
	c := cfg.(github.Config)
	if c.Issuer != "" {
		var err error
		if c.Keys, err = keys(c.Issuer); err != nil {
			return opened{}, err
		}
	}
	p, err := github.New(c)
	if err != nil {
		return opened{}, err
	}
	return opened{provider: p}, nil
}
