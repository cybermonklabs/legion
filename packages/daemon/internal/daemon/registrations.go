package daemon

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// newRegistrar is the machines' Registrar (supervise.Deps.Registrar) for a deployment whose
// runtime.kubernetes.agent_secrets block is a: the daemon's own agent-secrets client, which
// registers each role's session for its pod at the broker's POST /v1/oauth/exchange/pod-sessions
// under the daemon's own ServiceAccount token and writes it again every
// agentsecrets.RegistrationRenewal while the role's process lives. Only a pod that carries an
// exchange identity trades a token that the registration gates, so with exchange_audience unset
// it is nil, and the daemon makes no registration call at all (R9).
func newRegistrar(a config.AgentSecretsConfig, client *agentsecrets.Client) supervise.Registrar {
	if a.ExchangeAudience == "" {
		return nil
	}
	return brokerRegistrar{client: client}
}

// brokerRegistrar is agentsecrets.Client as supervise.Registrar.
type brokerRegistrar struct{ client *agentsecrets.Client }

func (b brokerRegistrar) KeepPodSession(ctx context.Context, s supervise.PodSession) error {
	return b.client.KeepPodSession(ctx, agentsecrets.PodSessionRenewal{
		PodUID: s.PodUID, Session: s.Session, Token: s.Token, After: s.After, Bound: s.Bound,
	})
}
