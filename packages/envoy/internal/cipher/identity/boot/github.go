//go:build !cipher_no_github

package boot

import (
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/identity/github"
	"github.com/sjawhar/envoy/internal/oidc"
)

func init() { register(entry{kind: github.Kind, rank: 30, open: openGitHub}) }

// openGitHub builds the GitHub provider: the issuer BROKER_EXCHANGE_GITHUB_ISSUER names, read
// only where it is set, its audience, and the exchange's workflow entries. No setting wires a
// GitHub caller of the API or the NATS auth service.
func openGitHub(cfg config.Config, keys func(string) (*oidc.Keys, error), _ *Boot) (opened, error) {
	c := github.Config{Issuer: cfg.ExchangeGitHubIssuer, Audience: cfg.ExchangeGitHubAudience, Workflows: cfg.ExchangeWorkflows}
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
