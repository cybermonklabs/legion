package daemon

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// newRegistrar is the machines' Registrar (supervise.Deps.Registrar) under runtime: kubernetes:
// the daemon's own agent-secrets client, which registers each role's session for its pod at the
// broker's POST /v1/oauth/exchange/pod-sessions under the daemon's own ServiceAccount token, bound
// to the thumbprint of the role's own key, and writes it again, with the same thumbprint, every
// cipher.RegistrationRenewal while the role's process lives; and which releases at POST
// /v1/oauth/exchange/pod-sessions/release the session names an operator's release frees, before it
// frees them. Every enrolled pod carries the exchange identity whose trade the registration gates,
// since runtime: kubernetes requires runtime.kubernetes.agent_secrets.exchange_audience.
func newRegistrar(client *cipher.Client) supervise.Registrar {
	return brokerRegistrar{client: client}
}

// brokerRegistrar is cipher.Client as supervise.Registrar: each field of the machine's
// registration, its thumbprint included, is the renewal's, and the renewal's stop is the
// machine's. A release's error is the client's own: an *cipher.APIError is a
// supervise.PermanentError and a supervise.RouteUnservedError, which is how the machine tells a
// refusal from a broker that did not answer.
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

func (b brokerRegistrar) ReleasePodSessions(ctx context.Context, names []string) error {
	return b.client.ReleasePodSessions(ctx, names)
}
