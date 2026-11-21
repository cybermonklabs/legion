package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/broker/wire"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/metrics"
)

// The broker revocation loop. When the Google account behind an address changes
// (store.PgPeopleStore.SignInSubject replacing the account the address recorded, of either member
// kind), everything the replaced account held ends. Its sessions and personal tokens end in the
// sign-in's own transaction; its credentials at the secrets broker - the machine logins of the
// address's own machines it approved and its sign-ins on a machine, with every access token minted
// under them - only the broker can end. So the sign-in records the debt in its transaction, naming
// the replaced account by its issuer and `sub` and the account that took the address by its `sub`
// (store.BrokerRevocations, migration 20261116232217), and this loop pays it after the commit, in
// whichever process a pass runs, with one call (agentsecrets.Client.RevokePersonSubject), until the
// broker confirms: a process that dies between the commit and the call, or a broker that does not
// answer, leaves the revocation owed for the next pass.
//
// The broker records the account behind each person sign-in and the approving account of each
// machine login, so it ends everything the replaced account holds under the address whenever it
// was issued, a sign-in it makes after the replacement with a Google token it got before included,
// refuses it anything new, and leaves the new holder's credentials alone, whenever they were made.
//
// Dispatch itself holds each broker token naming the address to the account its credential records
// (brokerPersonAdmitted, brokerTokenSubject), from the replacing commit, whether or not the broker
// has paid: the new holder's own acts, the replaced account's is refused since people no longer
// records its account, and one recording no account is refused when its credential was made before
// the replacing sign-in recorded its account, which every credential the replaced account holds
// was (brokerAccountReplaced). So nothing Dispatch admits waits on this loop; it is what ends the
// replaced account's credentials at the broker, for the secret reads the broker serves without
// asking Dispatch.

// brokerRevocationRetry is how long after a pass claimed a revocation no pass claims one of its
// address again: one the broker did not confirm, or whose pass died, is tried again then, and the
// next revocation of the address, once this one settles, is paid then. A claim stamps it
// (store.BrokerRevocations.Claim), so two processes woken by one commit do not both call the broker
// for one address within this window. A claim takes up to brokerRevocationBatch rows and the pass
// calls the broker for them one after another, each call bounded by the broker client's own
// timeout, so a pass sends a claimed row only while its claim is younger than this window less that
// bound (store.BrokerRevocations.Sendable): it starts no call its own client would let run past the
// moment another process may claim the address's next revocation, and a row past the window waits
// for the next claim. The broker may go on working on a call this client gave up on, so nothing
// here rests on the call itself being over.
const brokerRevocationRetry = 30 * time.Second

// brokerRevocationBatch is the most revocations one pass claims, for a broker client that bounds a
// call at timeout: as many as the pass can still send inside its send window,
// (brokerRevocationRetry - timeout) / timeout, and at least one. The pass sends them one after
// another, so its kth call starts (k-1) × timeout after the claim, and the window
// (brokerRevocationRetry - timeout) admits exactly that many. The rest are the next pass's, due at
// once (store.BrokerRevocations.NextDue), a pass apart (brokerRevocationMinGap).
//
// Claiming more would starve the rows the window skipped: a claim stamps every row it takes, and
// the next claim orders by tried_at, the same moment for all of them, then recorded_at, so a pass
// would send the same first rows again each time and never reach the others. Production's client
// bounds a call at five seconds (agentsecrets.New), which makes this 5; a client bounded at
// nothing, or at the retry window or more, claims one row a pass, since no batch size is safe
// under a call the client never gives up on.
func brokerRevocationBatch(timeout time.Duration) int {
	if timeout <= 0 || timeout >= brokerRevocationRetry {
		return 1
	}
	return max(1, int((brokerRevocationRetry-timeout)/timeout))
}

// brokerRevocationMinGap is the least time between two passes' starts, whatever woke them
// (duty.WokenLoop.MinGap): the sign-ins of a burst share one pass.
const brokerRevocationMinGap = time.Second

// RunBrokerRevocations runs the broker revocation loop until ctx ends: a pass (pass, the server's
// Background.RevokeBrokerCredentials) at once, then one on each wake - in the server, the pglisten.Hub
// wake on store.BrokerRevocationsChannel, which a recorded revocation announces at commit, and which
// the hub also signals after each successful LISTEN - and at the time the last pass asked for, when
// a revocation is due to be claimed. It takes no lease: a pass claims each revocation it pays
// (store.BrokerRevocations.Claim), so two processes never pay one address at once. With nothing owed
// it holds no timer and makes no statement.
func RunBrokerRevocations(ctx context.Context, pass func(context.Context) time.Time, wake <-chan struct{}) {
	duty.RunWoken(ctx, duty.WokenLoop{Wake: wake, MinGap: brokerRevocationMinGap, Pass: pass})
}

// revokeOwedBrokerCredentials is one pass of the loop: it claims up to
// brokerRevocationBatch(the broker client's own bound on a call) owed revocations, at most one of
// an address, and for each tells the broker the replaced account no
// longer holds the address (agentsecrets.Client.RevokePersonSubject), with the address's session
// generation the replacement left (session_generation), by which the broker refuses an approve
// Dispatch checked before the replacement. One the broker confirms, by answering 200 revoked, is
// settled at the generation the claim read, logged at WARN with what the broker ended. One the
// broker answers superseded is left owed, logged at ERROR and counted
// (Deps.BrokerRevocationsSuperseded), since the broker ended nothing (see the branch below). One it
// did not answer 200 - any error, a 404 included, which the route never answers - is logged at
// ERROR and left owed. Either is tried again brokerRevocationRetry after its claim. It answers when
// the next pass is due: the earliest time an owed revocation can be claimed, the zero time when
// none is owed. A server with
// no broker configured pays nothing, and its sign-ins record nothing either
// (store.WithBrokerRevocations, which main gives a people store only with a broker); a revocation a
// process with a broker recorded waits for a pass of one.
func (s *server) revokeOwedBrokerCredentials(ctx context.Context) time.Time {
	client := s.deps.AgentSecrets
	if client == nil || s.deps.Store == nil {
		return time.Time{}
	}
	ctx = store.WithTransactionTracking(ctx)
	revocations := store.NewBrokerRevocations(s.deps.Store.Pool)
	owed, err := revocations.Claim(ctx, brokerRevocationRetry, brokerRevocationBatch(client.HTTP.Timeout))
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch: claim the owed broker revocations", "error", err)
		}
		return time.Now().Add(brokerRevocationRetry)
	}
	// A call this pass starts must be one its own client gives up on before another process may
	// claim the address's next revocation, brokerRevocationRetry after this claim; the batch is the
	// most calls that fit in that window.
	sendWithin := brokerRevocationRetry - client.HTTP.Timeout
	for _, revocation := range owed {
		sendable, err := revocations.Sendable(ctx, revocation, sendWithin)
		if err != nil {
			slog.Error("dispatch: read whether a claimed broker revocation may still be sent; the next claim sends it", "email", revocation.Email, "error", err)
			continue
		}
		if !sendable {
			slog.Info("dispatch: a claimed broker revocation was not sent: its claim is older than a call may start, or another pass settled it; the next claim sends what is owed",
				"email", revocation.Email, "send_within", sendWithin, "retry_in", brokerRevocationRetry)
			continue
		}
		revoked, err := client.RevokePersonSubject(ctx, wire.RevokePersonSubjectRequest{
			Person: revocation.Email, Issuer: revocation.Issuer, Subject: revocation.Subject, HolderSubject: revocation.HolderSubject,
			SessionGeneration: revocation.SessionGeneration,
		})
		if err != nil {
			slog.Error(revokedLog(s.passkeyAccount(revocation), "did not revoke", "; trying again"),
				"email", revocation.Email, "retry_in", brokerRevocationRetry, "error", err)
			continue
		}
		if revoked.State == "superseded" {
			// The broker answers this only when the revocation's generation is below the greatest
			// one it applied at the address. While Dispatch's database and the broker's agree, the
			// loop never hears it for a row still owed: an address's revocations are paid in the
			// order of their generations, which only rise, one at a time, and a call is over before
			// the next can be claimed. So it re-reads the row first: one another pass settled since
			// (a call of its own that ran late) is logged at WARN and not counted. Heard for a row
			// still owed, it means the two disagree about the address's generations - a Dispatch
			// database recreated or restored without the broker's, or a generation written at the
			// broker by hand - and the replaced account's broker credentials were not ended. So the
			// revocation stays owed, where the owed gauges show it, the ERROR line names the
			// address's generation now beside the two the broker compared, and the runbook's
			// After a restore page gives the recovery. The client answers "superseded" only with
			// superseded_by; the generation sent is boxed against a broker that supersedes a revoke
			// that carried none.
			var sent any
			if revocation.SessionGeneration != nil {
				sent = *revocation.SessionGeneration
			}
			still, err := revocations.StillOwed(ctx, revocation)
			if err == nil && !still {
				slog.Warn("dispatch: the secrets broker answered a revocation superseded that another pass has settled since; nothing to recover",
					"email", revocation.Email, "subject", revocation.Subject, "session_generation", sent, "superseded_by", *revoked.SupersededBy)
				continue
			}
			var current any
			if generation, found, err := store.NewPgSessionStore(s.deps.Store.Pool).CurrentSessionGeneration(ctx, revocation.Email); err != nil {
				current = "unread: " + err.Error()
			} else if found {
				current = generation
			}
			s.deps.BrokerRevocationsSuperseded.Inc()
			slog.Error("dispatch: the secrets broker answered a revocation superseded: Dispatch's session generation for the address is below the one the broker recorded, so the replaced account's broker credentials were not ended; the revocation stays owed until an operator settles it, and a lone Dispatch restore requires the runbook's reset before that (after-a-restore.md)",
				"email", revocation.Email, "issuer", revocation.Issuer, "subject", revocation.Subject,
				"session_generation", sent, "superseded_by", *revoked.SupersededBy, "address_session_generation", current,
				"still_owed_unread", err != nil, "retry_in", brokerRevocationRetry)
			continue
		}
		settled, err := revocations.Settle(ctx, revocation)
		if err != nil {
			slog.Error(revokedLog(s.passkeyAccount(revocation), "revoked", ", and the revocation could not be settled; it is paid again"),
				"email", revocation.Email, "error", err)
			continue
		}
		slog.Warn(revokedLog(s.passkeyAccount(revocation), "revoked", ""),
			"email", revocation.Email, "signins", revoked.SignIns, "machine_logins", revoked.MachineLogins, "tokens_ended", revoked.TokensEnded, "settled", settled)
	}
	wait, due, err := revocations.NextDue(ctx, brokerRevocationRetry)
	return nextPassAt(ctx, wait, due, err, "broker revocation")
}

// nextPassAt is when the loop next needs a pass for one debt, from that debt's NextDue read (wait,
// due, err): wait from now for one owed, the zero time when none is, and brokerRevocationRetry
// from now when the read failed, which is logged naming what unless ctx ended.
func nextPassAt(ctx context.Context, wait time.Duration, due bool, err error, what string) time.Time {
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch: read when the next "+what+" is due", "error", err)
		}
		return time.Now().Add(brokerRevocationRetry)
	}
	if !due {
		return time.Time{}
	}
	return time.Now().Add(wait)
}

// passkeyAccount reports whether revocation ends a passkey's account (its issuer the outside
// origin, Deps.Passkeys), which the project owner's Reset replaces, rather than one another Google
// account took the address of.
func (s *server) passkeyAccount(revocation store.OwedBrokerRevocation) bool {
	return s.deps.Passkeys != nil && revocation.Issuer == s.deps.Passkeys.Issuer()
}

// revokedLog is the revocation pass's message for what the broker did ("revoked", "did not
// revoke") with the credentials of the account a revocation ends, followed by then: a passkey's
// account is named as the one an owner's Reset replaced, every other as the one another Google
// account took the address of.
func revokedLog(passkey bool, did, then string) string {
	if passkey {
		return "dispatch: the secrets broker " + did + " the credentials of a passkey account the project owner's Reset replaced" + then
	}
	return "dispatch: the secrets broker " + did + " the credentials of an account another Google account took the address of" + then
}

// brokerRevocationMetricsRead bounds one series' read of the owed revocations.
const brokerRevocationMetricsRead = 2 * time.Second

// registerOwedGauges puts on registry the two series of one revocation debt, each read by owed
// (a store's Owed) when a scrape asks for it, two reads a scrape: owedName, how many the broker
// has not confirmed, and oldestName, how long the oldest has been owed, written only while one is.
// A read that fails writes no sample, so a database outage never reads as nothing owed, and is
// logged naming what.
func registerOwedGauges(registry *metrics.Registry, owed func(context.Context) (int64, time.Duration, error), what, owedName, owedHelp, oldestName, oldestHelp string) {
	read := func() (int64, time.Duration, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), brokerRevocationMetricsRead)
		defer cancel()
		count, oldest, err := owed(ctx)
		if err != nil {
			slog.Warn("dispatch: the owed "+what+" could not be read for a metrics scrape", "error", err)
			return 0, 0, false
		}
		return count, oldest, true
	}
	registry.NewOptionalGaugeFunc(owedName, owedHelp, func() (int64, bool) {
		count, _, ok := read()
		return count, ok
	})
	registry.NewOptionalGaugeFunc(oldestName, oldestHelp, func() (int64, bool) {
		count, oldest, ok := read()
		return int64(oldest / time.Second), ok && count > 0
	})
}

// RegisterBrokerRevocationMetrics puts the broker revocations owed on registry, the process's
// (GET /metrics), each series read from pool's broker_revocations when a scrape asks for it
// (store.BrokerRevocations.Owed), so every process reports the deployment's whichever recorded them,
// at two reads a scrape: envoy_dispatch_broker_revocations_owed, how many the broker has not
// confirmed, and envoy_dispatch_broker_revocation_oldest_owed_seconds, how long the oldest has been
// owed since its replacing commit, written only while one is owed. A revocation the broker confirms
// is owed for the pass its commit wakes, a second or so, and 30 s more for each earlier revocation
// of its address it waits behind, so an age past a few minutes is one the broker does not confirm,
// whose replaced account's credentials the broker still holds for the secret reads it serves without
// asking Dispatch, until it settles or an operator revokes them at the broker and clears it
// (`envoy-dispatch clear-broker-revocation`); Dispatch refuses them from the replacing commit
// either way (brokerPersonAdmitted). A read that fails writes no sample, so a database
// outage never reads as nothing owed. A revocation the broker answers superseded stays owed, so
// both show it; NewBrokerRevocationsSuperseded counts each such answer.
func RegisterBrokerRevocationMetrics(registry *metrics.Registry, pool *store.Pool) {
	registerOwedGauges(registry, store.NewBrokerRevocations(pool).Owed, "broker revocations",
		"envoy_dispatch_broker_revocations_owed",
		"Revocations of a replaced account's credentials that Dispatch owes the secrets broker and the broker has not confirmed; until each settles, the broker still holds those credentials for the secret reads it serves without asking Dispatch",
		"envoy_dispatch_broker_revocation_oldest_owed_seconds",
		"Seconds since the replacing sign-in of the oldest broker revocation not yet confirmed, written only while one is owed; past a few minutes the broker is not confirming it (envoy-dispatch clear-broker-revocation settles one)")
}

// NewBrokerRevocationsSuperseded registers envoy_dispatch_broker_revocations_superseded_total on
// registry, the process's, and answers it for DepsInput.BrokerRevocationsSuperseded: each answer
// superseded this process's loop heard, for a revocation it then leaves owed and logs at ERROR
// (revokeOwedBrokerCredentials), once a claim, so a revocation left that way adds one each retry.
// Any count above zero means Dispatch's session generation for an address is below the broker's,
// and a replaced account's broker credentials were not ended; the runbook's After a restore page
// gives the recovery, a reset of every person's sessions, tokens and broker credentials.
func NewBrokerRevocationsSuperseded(registry *metrics.Registry) *metrics.Counter {
	return registry.NewCounter("envoy_dispatch_broker_revocations_superseded_total",
		"Revocation calls the secrets broker answered superseded, which Dispatch leaves owed: Dispatch's session generation for the address is below the broker's, and the replaced account's broker credentials stay live until the runbook's reset after a lone restore ends them (after-a-restore.md)")
}
