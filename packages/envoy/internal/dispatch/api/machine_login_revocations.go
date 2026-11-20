package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A removal that takes a restricted member off an access list owes the secrets broker the
// revocation of the member's machine logins (recordMachineLoginRevocation, migration
// 20261120150130), and the broker revocation loop pays it after the commit (revokeOwedMachineLogins),
// beside the revocations of replaced accounts and on the same wake. Nothing the 325-second bound on
// a removal rests on waits for it: every trade of a restricted member's machine login names one
// project, which the broker checks against Dispatch each time (projectaccess.Visible), so no token
// of the project is minted once the broker's 30-second cache of the answer runs out, and a token
// minted before then lives at most 270 seconds (exchange.RestrictedLifetime). The revoke adds what
// the check cannot: the end of the login's refresh, and of every token traded from it at once.

// revokeBrokerDebts is one pass of the broker revocation loop (RunBrokerRevocations): it pays the
// owed revocations of replaced accounts (revokeOwedBrokerCredentials) and of restricted members'
// machine logins (revokeOwedMachineLogins), and answers when the loop next needs a pass, the
// earlier of the two, the zero time when neither owes one.
func (s *server) revokeBrokerDebts(ctx context.Context) time.Time {
	accounts := s.revokeOwedBrokerCredentials(ctx)
	logins := s.revokeOwedMachineLogins(ctx)
	switch {
	case accounts.IsZero():
		return logins
	case logins.IsZero() || accounts.Before(logins):
		return accounts
	}
	return logins
}

// revokeOwedMachineLogins is the loop's pass over the machine-login revocations owed: it claims up
// to brokerRevocationBatch(the broker client's own bound on a call) of them, and for each revokes
// every machine login the person approved for their own machines (revokeMachineLoginsOf), then
// settles it at the generation the claim read. One the broker does not confirm is logged at ERROR
// and left owed, tried again brokerRevocationRetry after its claim. It answers when the next pass is
// due, the zero time when none is owed. A server with no broker pays nothing.
func (s *server) revokeOwedMachineLogins(ctx context.Context) time.Time {
	client := s.deps.AgentSecrets
	if client == nil || s.deps.Store == nil {
		return time.Time{}
	}
	revocations := store.NewMachineLoginRevocations(s.deps.Store.Pool)
	owed, err := revocations.Claim(ctx, brokerRevocationRetry, brokerRevocationBatch(client.HTTP.Timeout))
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch: claim the owed machine-login revocations", "error", err)
		}
		return time.Now().Add(brokerRevocationRetry)
	}
	for _, revocation := range owed {
		revoked, err := revokeMachineLoginsOf(ctx, client, revocation.Email)
		if err != nil {
			slog.Error("dispatch: the secrets broker did not revoke the machine logins of a restricted member taken off an access list; trying again",
				"email", revocation.Email, "revoked", revoked, "retry_in", brokerRevocationRetry, "error", err)
			continue
		}
		settled, err := revocations.Settle(ctx, revocation)
		if err != nil {
			slog.Error("dispatch: the secrets broker revoked the machine logins of a restricted member taken off an access list, and the revocation could not be settled; it is paid again",
				"email", revocation.Email, "error", err)
			continue
		}
		slog.Warn("dispatch: the secrets broker revoked the machine logins of a restricted member taken off an access list",
			"email", revocation.Email, "machine_logins", revoked, "settled", settled)
	}
	wait, due, err := revocations.NextDue(ctx, brokerRevocationRetry)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch: read when the next machine-login revocation is due", "error", err)
		}
		return time.Now().Add(brokerRevocationRetry)
	}
	if !due {
		return time.Time{}
	}
	return time.Now().Add(wait)
}

// revokeMachineLoginsOf revokes at the broker every machine login person approved for their own
// machines that the broker still lists (ownMachineLogins), and answers how many it revoked. The
// broker's list holds every service's logins beside the person's own, which it leaves. A machine
// login is no project's (each trade of it names its own project), so every login of the person's
// is revoked, and they approve a new one for a project that still lists them. A revoke ends the
// login's refresh and every token traded from it (the broker's enroll.Service.RevokeCredential ends
// each access token naming the credential); a login the broker revoked already is not listed
// again, so a pass that meets a failure revokes the rest next time.
func revokeMachineLoginsOf(ctx context.Context, client *agentsecrets.Client, person string) (int, error) {
	own, err := ownMachineLogins(ctx, client, person)
	if err != nil {
		return 0, fmt.Errorf("list %s's machine logins: %w", person, err)
	}
	revoked := 0
	for _, login := range own {
		if _, err := client.RevokeMachineLogin(ctx, login.CredentialID, person); err != nil {
			return revoked, fmt.Errorf("revoke %s's machine login %s: %w", person, login.CredentialID, err)
		}
		revoked++
	}
	return revoked, nil
}
