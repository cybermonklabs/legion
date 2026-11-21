package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/metrics"
)

// A removal that takes a restricted member off their last access list owes the secrets broker the
// revocation of the member's machine logins (removePerson, recordMachineLoginRevocation, migration
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

// machineLoginRevocationBatch is the most machine-login revocations one pass claims: one. A row
// costs 1 + N broker calls, the person's list and a revoke for each login they approved before the
// removal, and N is not known when the pass claims, so brokerRevocationBatch, which counts one call
// a row, cannot size it. The pass asks before every call whether its claim may still send one
// (store.MachineLoginRevocations.Sendable), so the calls of one claimed row stop where the window
// brokerRevocationBatch counts would; the rest of the row is the next claim's, which lists only the
// logins not yet revoked. Other rows are due at once (NextDue), a pass apart.
const machineLoginRevocationBatch = 1

// errRevocationWindowSpent is a claimed revocation whose claim may send no more broker calls: its
// next claim pays the rest.
var errRevocationWindowSpent = errors.New("the claim may start no more broker calls")

// revokeOwedMachineLogins is the loop's pass over the machine-login revocations owed: it claims
// machineLoginRevocationBatch of them, and for each revokes the machine logins the person approved
// for their own machines before the removal that owes it (revokeMachineLoginsOf), asking before
// every broker call whether the claim may still send one, then settles it at the generation the
// claim read. One the broker does not confirm is logged at ERROR, and one whose claim ran out of
// time at INFO; either is left owed and tried again brokerRevocationRetry after its claim. It answers
// when the next pass is due, the zero time when none is owed. A server with no broker pays nothing.
func (s *server) revokeOwedMachineLogins(ctx context.Context) time.Time {
	client := s.deps.AgentSecrets
	if client == nil || s.deps.Store == nil {
		return time.Time{}
	}
	revocations := store.NewMachineLoginRevocations(s.deps.Store.Pool)
	owed, err := revocations.Claim(ctx, brokerRevocationRetry, machineLoginRevocationBatch)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch: claim the owed machine-login revocations", "error", err)
		}
		return time.Now().Add(brokerRevocationRetry)
	}
	// A call this pass starts must be one its own client gives up on before another process may
	// claim the row again, brokerRevocationRetry after this claim.
	sendWithin := brokerRevocationRetry - client.HTTP.Timeout
	for _, revocation := range owed {
		mayCall := func() (bool, error) { return revocations.Sendable(ctx, revocation, sendWithin) }
		revoked, err := revokeMachineLoginsOf(ctx, client, revocation, mayCall)
		if errors.Is(err, errRevocationWindowSpent) {
			slog.Info("dispatch: a claimed machine-login revocation sent no more calls: its claim is older than a call may start, or a removal recorded it again; the next claim sends what is owed",
				"email", revocation.Email, "revoked", revoked, "send_within", sendWithin, "retry_in", brokerRevocationRetry)
			continue
		}
		if err != nil {
			slog.Error("dispatch: the machine logins of a restricted member taken off an access list or reset by a project's owner were not revoked: the secrets broker did not confirm, or the claim could not be read; trying again",
				"email", revocation.Email, "revoked", revoked, "retry_in", brokerRevocationRetry, "error", err)
			continue
		}
		settled, err := revocations.Settle(ctx, revocation)
		if err != nil {
			slog.Error("dispatch: the secrets broker revoked the machine logins of a restricted member taken off an access list or reset by a project's owner, and the revocation could not be settled; it is paid again",
				"email", revocation.Email, "error", err)
			continue
		}
		slog.Warn("dispatch: the secrets broker revoked the machine logins of a restricted member taken off an access list or reset by a project's owner",
			"email", revocation.Email, "machine_logins", revoked, "settled", settled)
	}
	wait, due, err := revocations.NextDue(ctx, brokerRevocationRetry)
	return nextPassAt(ctx, wait, due, err, "machine-login revocation")
}

// revokeMachineLoginsOf revokes at the broker the machine logins revocation's person approved for
// their own machines (ownMachineLogins) before the removal that owes it (its RecordedAt), and
// answers how many it revoked. A login approved after it, once the person was put back on a list,
// is no debt of that removal's and survives. The broker's list holds every service's logins beside
// the person's own, which it leaves. A machine login is no project's (each trade of it names its own
// project), so every such login is revoked, and the person approves a new one for a project that
// still lists them. A revoke ends the login's refresh and every token traded from it (the broker's
// enroll.Service.RevokeCredential ends each access token naming the credential); a login the broker
// revoked already is not listed again, so a pass that meets a failure revokes the rest next time.
//
// It asks mayCall before every broker call, the list and each revoke, and stops with
// errRevocationWindowSpent when it answers false.
//
// The login's issued_at is the broker's clock and RecordedAt is Dispatch's database's. Where the two
// disagree, a login approved within that skew of the removal is judged by it: one approved just
// before the removal can survive, and its tokens still end within the 325-second bound, since every
// trade is checked against Dispatch's lists.
func revokeMachineLoginsOf(ctx context.Context, client *agentsecrets.Client, revocation store.OwedMachineLoginRevocation, mayCall func() (bool, error)) (int, error) {
	next := func() error {
		ok, err := mayCall()
		if err != nil {
			return err
		}
		if !ok {
			return errRevocationWindowSpent
		}
		return nil
	}
	person := revocation.Email
	if err := next(); err != nil {
		return 0, err
	}
	own, err := ownMachineLogins(ctx, client, person)
	if err != nil {
		return 0, fmt.Errorf("list %s's machine logins: %w", person, err)
	}
	revoked := 0
	for _, login := range own {
		if !login.IssuedAt.Before(revocation.RecordedAt) {
			continue
		}
		if err := next(); err != nil {
			return revoked, err
		}
		if _, err := client.RevokeMachineLogin(ctx, login.CredentialID, person); err != nil {
			return revoked, fmt.Errorf("revoke %s's machine login %s: %w", person, login.CredentialID, err)
		}
		revoked++
	}
	return revoked, nil
}

// RegisterMachineLoginRevocationMetrics puts the machine-login revocations owed on registry, the
// process's (GET /metrics), each series read from pool's machine_login_revocations when a scrape
// asks for it (store.MachineLoginRevocations.Owed), so every process reports the deployment's
// whichever recorded them, as RegisterBrokerRevocationMetrics does for replaced accounts:
// envoy_dispatch_machine_login_revocations_owed, how many the broker has not confirmed, and
// envoy_dispatch_machine_login_revocation_oldest_owed_seconds, how long the oldest has been owed
// since its removal, written only while one is owed. A pass pays one a few seconds after its
// removal commits, so an age past a few minutes is one the broker does not confirm, whose member's
// machine logins still refresh until it settles; every trade of theirs names a project the broker
// checks against Dispatch, so the member gets no token of a project that dropped them either way.
// A read that fails writes no sample, so a database outage never reads as nothing owed
// (registerOwedGauges).
func RegisterMachineLoginRevocationMetrics(registry *metrics.Registry, pool *store.Pool) {
	registerOwedGauges(registry, store.NewMachineLoginRevocations(pool).Owed, "machine-login revocations",
		"envoy_dispatch_machine_login_revocations_owed",
		"Revocations of a restricted member's machine logins that Dispatch owes the secrets broker after a removal from an access list, and the broker has not confirmed; until each settles, those logins still refresh",
		"envoy_dispatch_machine_login_revocation_oldest_owed_seconds",
		"Seconds since the removal of the oldest machine-login revocation not yet confirmed, written only while one is owed; past a few minutes the broker is not confirming it")
}
