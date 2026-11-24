//go:build !cipher_no_kubernetes

package boot

import (
	"slices"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
	"github.com/sjawhar/envoy/internal/oidc"
)

func init() {
	register(entry{kind: kubernetes.Kind, rank: 10, open: openKubernetes, accountNoun: "a Kubernetes account", natsAudience: "BROKER_NATS_K8S_AUDIENCE"})
}

// openKubernetes builds the Kubernetes provider: the cluster BROKER_K8S_OIDC_ISSUER names, the
// exchange's account entries (BROKER_EXCHANGE_ACCOUNTS, then BROKER_EXCHANGE_JOB_ACCOUNTS, so a
// job entry is numbered after them in a refusal) and read entries, the pod verifiers at
// BROKER_K8S_OIDC_AUDIENCE and BROKER_POD_REGISTRATION_AUDIENCE, and the API's and the NATS auth
// service's callers at their audiences. A broker told about no cluster reads no key set and
// verifies no pod.
func openKubernetes(cfg config.Config, keys func(string) (*oidc.Keys, error), b *Boot) (opened, error) {
	c := kubernetes.Config{
		Issuer: cfg.K8sOIDCIssuer, ExchangeAudience: cfg.ExchangeK8sAudience,
		Accounts: slices.Concat(cfg.ExchangeAccounts, cfg.ExchangeJobAccounts), Reads: cfg.ExchangeReadAccounts,
	}
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
	b.Pods = p.PodVerifier(cfg.K8sOIDCAudience)
	if cfg.PodRegistrationAudience != "" {
		b.PodRegistration = p.PodVerifier(cfg.PodRegistrationAudience)
	}
	built.callers[identity.UseAPI] = p.CallerVerifier(cfg.K8sOIDCAudience)
	if cfg.NATS.K8sAudience != "" {
		built.callers[identity.UseNATS] = p.CallerVerifier(cfg.NATS.K8sAudience)
	}
	return built, nil
}
