package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/wire"
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
// whichever process a pass runs, with one call (cipher.Client.RevokePersonSubject), until the
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
// bounds a call at five seconds (cipher.New), which makes this 5; a client bounded at
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
// longer holds the address (cipher.Client.RevokePersonSubject), with the address's session
// generation the replacement left (session_generation), by which the broker refuses an approve
// Dispatch checked before the replacement. One the broker confirms, by answering 200 revoked, is
// settled at the generation the claim read, logged at WARN with what the broker ended. One the
// broker answers superseded is left owed, logged at ERROR and counted
// (Deps.BrokerRevocationsSuperseded), since the broker ended nothing (see the branch below). One it
// did not answer 200 - any error, a 404 included, which the route never answers - is logged at
// ERROR and left owed. Either is tried again brokerRevocationRetry after its claim. It answers when
// the next pass is due: the earliest time an owed revocation can be claimed, the zero time when
// none is owed. A claimed revocation whose account holds its address again is settled and not sent
// (store.BrokerRevocations.SettleHeld): a build without the account binding signed it in with no
// settlement, and paid it would end the holder's broker credentials. A server with
// no broker configured pays nothing, and its sign-ins record nothing either
// (store.WithBrokerRevocations, which main gives a people store only with a broker); a revocation a
// process with a broker recorded waits for a pass of one. The pass runs whatever the fleet holds:
// it records and replaces no account, and pays only what a sign-in recorded while the binding
// held, which ends a replaced account's broker credentials beside a build without the binding too.
//
// Each pass first catches up the replacements of an address's account the database kept while the
// account binding was off (catchUpAccountBindings), broker or none, so a revocation that catch-up
// owes is paid in the same pass; while one is kept and the binding is off, the pass asks for the
// next within accountBindingCatchUpPoll.
func (s *server) revokeOwedBrokerCredentials(ctx context.Context) time.Time {
	pending := s.catchUpAccountBindings(ctx)
	next := s.payOwedBrokerCredentials(ctx)
	if pending {
		if poll := time.Now().Add(accountBindingCatchUpPoll); next.IsZero() || poll.Before(next) {
			next = poll
		}
	}
	return next
}

// accountBindingCatchUpPoll is how often the revocation loop reads the fleet again while a
// replacement kept while the account binding was off waits for it to hold
// (catchUpAccountBindings): the longest a replaced account's broker credentials stand once the
// binding holds, when no sign-in of the address catches it up first. Its tokens are refused from
// the moment the binding holds (tokenAccountReplaced).
const accountBindingCatchUpPoll = 10 * time.Second

// catchUpAccountBindings catches up, for each address with a replacement of its account the
// database kept while the account binding was off (store.AccountBindingReplacementAddresses), what
// the binding owes it, each in a transaction of its own under the address's lock
// (store.CatchUpAccountBindingOf): the personal tokens made since are revoked, a broker revocation
// of each replaced account is recorded when a broker is configured, and the address's rows are
// deleted, so one address is caught up once. It stops at the first address it finds the binding
// off for, since the fleet is then off for every address, and answers whether any replacement is
// still kept. A deployment whose sign-in records no account keeps none. Each address caught up is
// logged at WARN; a read or write that fails is logged at ERROR, and the replacement is kept for
// the next pass.
func (s *server) catchUpAccountBindings(ctx context.Context) (pending bool) {
	if s.deps.Store == nil || s.deps.SignInIssuer == "" {
		return false
	}
	emails, err := store.AccountBindingReplacementAddresses(ctx, s.deps.Store.Pool)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch: read the replacements kept while the account binding was off; the next pass reads them again", "error", err)
		}
		return true
	}
	issuer := ""
	if s.deps.AgentSecrets != nil {
		issuer = s.deps.SignInIssuer
	}
	for _, email := range emails {
		held, caught, err := store.CatchUpAccountBindingOf(ctx, s.deps.Store.Pool, email, issuer)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("dispatch: catch up a replacement kept while the account binding was off; the next pass tries again", "email", email, "error", err)
			}
			pending = true
			continue
		}
		if !held {
			return true
		}
		if caught.Found() {
			store.LogAccountBindingCatchUp(email, caught)
		}
	}
	return pending
}

// payOwedBrokerCredentials pays the owed broker revocations (revokeOwedBrokerCredentials).
func (s *server) payOwedBrokerCredentials(ctx context.Context) time.Time {
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
		held, err := revocations.SettleHeld(ctx, revocation)
		if err != nil {
			slog.Error("dispatch: read whether a claimed broker revocation's account holds its address again; the next claim reads it again", "email", revocation.Email, "error", err)
			continue
		}
		if held {
			slog.Warn("dispatch: a claimed broker revocation names the account its address records again, which took the address back with no settlement (a build without the account binding signed it in); it is settled, not sent",
				"email", revocation.Email, "subject", revocation.Subject)
			continue
		}
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
			slog.Error("dispatch: the secrets broker did not revoke the credentials of an account another Google account took the address of; trying again",
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
			slog.Error("dispatch: the secrets broker revoked the credentials of an account another Google account took the address of, and the revocation could not be settled; it is paid again",
				"email", revocation.Email, "error", err)
			continue
		}
		slog.Warn("dispatch: the secrets broker revoked the credentials of an account another Google account took the address of",
			"email", revocation.Email, "signins", revoked.SignIns, "machine_logins", revoked.MachineLogins, "tokens_ended", revoked.TokensEnded, "settled", settled)
	}
	wait, due, err := revocations.NextDue(ctx, brokerRevocationRetry)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch: read when the next broker revocation is due", "error", err)
		}
		return time.Now().Add(brokerRevocationRetry)
	}
	if !due {
		return time.Time{}
	}
	return time.Now().Add(wait)
}

// brokerRevocationMetricsRead bounds one series' read of the owed revocations.
const brokerRevocationMetricsRead = 2 * time.Second

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
	revocations := store.NewBrokerRevocations(pool)
	read := func() (int64, time.Duration, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), brokerRevocationMetricsRead)
		defer cancel()
		count, oldest, err := revocations.Owed(ctx)
		if err != nil {
			slog.Warn("dispatch: the owed broker revocations could not be read for a metrics scrape", "error", err)
			return 0, 0, false
		}
		return count, oldest, true
	}
	registry.NewOptionalGaugeFunc("envoy_dispatch_broker_revocations_owed",
		"Revocations of a replaced account's credentials that Dispatch owes the secrets broker and the broker has not confirmed; until each settles, the broker still holds those credentials for the secret reads it serves without asking Dispatch",
		func() (int64, bool) {
			count, _, ok := read()
			return count, ok
		})
	registry.NewOptionalGaugeFunc("envoy_dispatch_broker_revocation_oldest_owed_seconds",
		"Seconds since the replacing sign-in of the oldest broker revocation not yet confirmed, written only while one is owed; past a few minutes the broker is not confirming it (envoy-dispatch clear-broker-revocation settles one)",
		func() (int64, bool) {
			count, oldest, ok := read()
			return int64(oldest / time.Second), ok && count > 0
		})
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
