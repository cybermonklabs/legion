package cipher

import (
	"context"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// OutlivingAccess answers whether a person has access that outlives their browser session: an
// unrevoked personal (dsp_) token in Dispatch's own agent_tokens, or at the secrets broker a live
// machine login of their own, an unrevoked sign-in on a machine, an exchange entry that names them,
// or a Kubernetes service account BROKER_POD_OPERATORS maps to them, live login or none. A sign-out
// keeps such a person's refresh token, for membership checks only, so that Google's later refusal
// still holds them; Dispatch's membership pass asks again of each signed-out row it keeps one for,
// and under IAP identity the Directory check of an address created again asks it too.
// api.NewDeps builds the one Dispatch serves with (api.Deps.Outliving).
type OutlivingAccess struct {
	// Pool is Dispatch's database, which agent_tokens is read from; nil is no database, which
	// answers an error.
	Pool *store.Pool
	// Broker is the secrets broker's client; nil (no DISPATCH_AGENT_SECRETS_URL) answers from
	// agent_tokens alone.
	Broker *Client
}

// errNoDatabase is Holds without a database to read agent_tokens from.
var errNoDatabase = errors.New("no database to read personal tokens from")

// Holds answers true when agent_tokens holds an unrevoked token whose owner is person, as
// canonical_login spells both, without asking the broker. Otherwise, with a broker client, it
// asks the broker (Client.PersonAccess, bounded by the client's window, defaultTimeout) and answers
// true when any of its four counts is above 0. Any failure, to read agent_tokens or to ask the
// broker, is an error naming the person, and the caller cannot tell what the person has.
func (o OutlivingAccess) Holds(ctx context.Context, person string) (bool, error) {
	person = model.CanonicalLogin(person)
	if o.Pool == nil {
		return false, fmt.Errorf("agent-secrets: read whether %s holds a personal token: %w", person, errNoDatabase)
	}
	var tokens bool
	if err := o.Pool.QueryRow(ctx, `
		select exists (select 1 from agent_tokens where canonical_login(owner) = $1 and revoked_at is null)
	`, person).Scan(&tokens); err != nil {
		return false, fmt.Errorf("agent-secrets: read whether %s holds a personal token: %w", person, err)
	}
	if tokens || o.Broker == nil {
		return tokens, nil
	}
	access, err := o.Broker.PersonAccess(ctx, person)
	if err != nil {
		return false, err
	}
	return access.MachineLogins > 0 || access.SignIns > 0 || access.ExchangeEntries > 0 || access.PodOperatorMappings > 0, nil
}
