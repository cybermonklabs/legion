package daemon

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// newRegistrar is the machines' Registrar (supervise.Deps.Registrar) for a deployment whose
// runtime.kubernetes.agent_secrets block is a: the daemon's own agent-secrets client, which
// registers each role's session for its pod at the broker's POST /v1/oauth/exchange/pod-sessions
// under the daemon's own ServiceAccount token, bound to the thumbprint of the role's own key, and
// writes it again, with the same thumbprint, every cipher.RegistrationRenewal while the
// role's process lives. Only a pod that carries an exchange identity trades a token that the
// registration gates, so with exchange_audience unset it is nil, and the daemon makes no
// registration call at all (R9).
func newRegistrar(a config.AgentSecretsConfig, client *cipher.Client) supervise.Registrar {
	if a.ExchangeAudience == "" {
		return nil
	}
	return brokerRegistrar{client: client}
}

// brokerRegistrar is cipher.Client as supervise.Registrar: each field of the machine's
// registration, its thumbprint included, is the renewal's, and the renewal's stop is the
// machine's.
type brokerRegistrar struct{ client *cipher.Client }

func (b brokerRegistrar) KeepPodSession(ctx context.Context, s supervise.PodSession) (func(), error) {
	var latest func() int64
	if s.Latest != nil {
		latest = func() int64 { return int64(s.Latest()) }
	}
	return b.client.KeepPodSession(ctx, cipher.PodSessionRenewal{
		PodUID: s.PodUID, Session: s.Session, Thumbprint: s.Thumbprint, Generation: int64(s.Generation), Latest: latest,
		Token: s.Token, After: s.After, Bound: s.Bound,
	})
}
