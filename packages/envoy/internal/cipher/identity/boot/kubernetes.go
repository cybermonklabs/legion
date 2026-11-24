//go:build !cipher_no_kubernetes

package boot

import (
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
	"github.com/sjawhar/envoy/internal/oidc"
)

func init() {
	register(entry{
		kind: kubernetes.Kind, rank: 10,
		zero:            func() any { return kubernetes.Config{} },
		load:            func(getenv func(string) string) (any, error) { return kubernetes.LoadConfig(getenv) },
		open:            openKubernetes,
		parseAccount:    kubernetes.ParseAccount,
		parsePodAccount: kubernetes.ParsePodAccount,
		accountForm:     kubernetes.AccountForm,
		serves: func(cfg any, use identity.Use, site identity.Site) error {
			return cfg.(kubernetes.Config).Serves(use, site)
		},
		podsServed: func(cfg any, setting string) error { return cfg.(kubernetes.Config).PodsServed(setting) },
		mentions: func(cfg any, account identity.Account) []identity.Mention {
			return cfg.(kubernetes.Config).Mentions(account)
		},
		services:  func(cfg any) []string { return cfg.(kubernetes.Config).Services() },
		exchanges: func(cfg any) bool { return cfg.(kubernetes.Config).ExchangeAudience != "" },
		exchangeFields: func(cfg any) []any {
			c := cfg.(kubernetes.Config)
			// The job entries LoadConfig appends to Accounts are counted apart, as the line
			// always counted BROKER_EXCHANGE_JOB_ACCOUNTS.
			jobs := 0
			for _, entry := range c.Accounts {
				if entry.Job.Listed() {
					jobs++
				}
			}
			return []any{"kubernetes", c.ExchangeAudience != "", "accounts", len(c.Accounts) - jobs, "read_accounts", len(c.Reads), "job_accounts", jobs}
		},
	})
}

// openKubernetes builds the Kubernetes provider from its own settings: the cluster
// BROKER_K8S_OIDC_ISSUER names, the exchange's account and read entries, the pod verifiers at
// BROKER_K8S_OIDC_AUDIENCE and BROKER_POD_REGISTRATION_AUDIENCE, and the API's and the NATS auth
// service's callers at their audiences. A broker told about no cluster reads no key set and
// verifies no pod.
func openKubernetes(cfg any, _ Shared, keys func(string) (*oidc.Keys, error), b *Boot) (opened, error) {
	c := cfg.(kubernetes.Config)
	if c.Issuer != "" {
		var err error
		if c.Keys, err = keys(c.Issuer); err != nil {
			return opened{}, err
		}
	}
	p, err := kubernetes.New(c)
	if err != nil {
		return opened{}, err
	}
	built := opened{provider: p, callers: map[identity.Use]identity.CallerVerifier{}}
	if c.Issuer == "" {
		return built, nil
	}
	b.Pods = p.PodVerifier(c.Audience)
	if c.PodRegistrationAudience != "" {
		b.PodRegistration = p.PodVerifier(c.PodRegistrationAudience)
	}
	built.callers[identity.UseAPI] = p.CallerVerifier(c.Audience)
	if c.NATSAudience != "" {
		built.callers[identity.UseNATS] = p.CallerVerifier(c.NATSAudience)
	}
	return built, nil
}
