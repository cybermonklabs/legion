package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// AccountBindingProtocol is the account binding this build serves under, which it registers in
// server_processes (account_binding; processes.Register) and names in the dashboard_builds row it
// publishes (dashboard.Publish), migration 20261120222130_account_binding_fleet: 1, a build that
// holds every credential of an address to the Google account Dispatch records for it (people.sub
// and sub_recorded_at: api.tokenAccountReplaced, api.brokerAccountReplaced, api.accountHolder),
// records the account a mint's credential is bound to (agent_tokens.subject), and revokes the
// address's tokens and owes the broker a revocation when another account signs in
// (PgPeopleStore.SignInSubject). A row at 0, the column's default, is written by a build without
// it, which checks no account, records none and revokes nothing at a replacement.
const AccountBindingProtocol = 1

// AccountBindingGateKey is the second key of the account binding's gate: OwnerLockGateNamespace
// ("OWNG") and 1, a transaction advisory lock the migration's functions take and nothing in Go
// does. A build without the binding takes it exclusively as it arrives, at its dashboard publish
// (public.account_binding_legacy_arrival) and its registration (for at most 2 seconds,
// public.account_binding_legacy_registration); a transaction of this build that reads the fleet as
// having no such build in it tries it shared and holds it to its end (AccountBindingHolds). The
// owner lock gate is key 0 of the same namespace, so the two never wait on each other. Exported,
// and spelled here as the migration spells it, so a test reads the gate out of pg_locks.
const AccountBindingGateKey int32 = 1

// AccountBindingHoldsSQL is the SQL expression of whether the account binding holds over the
// fleet: no build without it is in the fleet (public.account_binding_fleet, migration
// 20261120222130_account_binding_fleet), over legacyWindow and markWindow, the expressions of the
// statement that are AccountBindingWindows' two values. It is the same fence the owner lock reads
// (OwnerLockLegacyWindow and OwnerLockMarkWindow): such a build is in the fleet while a
// server_processes row at account_binding 0 heartbeated within the one window, or while the mark
// its last arrival stamped (its dashboard publish, which its boot makes before it writes anything
// else, or its registration) is younger than the other. When it finds none it tries the binding's
// gate shared (AccountBindingGateKey) and reads again, and a try that fails answers that such a
// build is in the fleet. Run in a transaction, a holding answer keeps the gate shared to the
// transaction's end, so such a build's arrival commits only after it; run alone, a statement's
// gate is gone when it ends, which a read that writes nothing of the binding needs no more of.
//
// While such a build is in the fleet, it checks no account and records none, and a credential of
// a replaced account acts on it whatever this build does; so this build does as it does, and
// writes nothing of the binding that an account it lets take the address back would leave
// standing. Each caller ANDs this with whether the deployment's sign-in records accounts at all.
func AccountBindingHoldsSQL(legacyWindow, markWindow string) string {
	return "(not public.account_binding_fleet(" + legacyWindow + "::float8, " + markWindow + "::float8))"
}

// AccountBindingWindows is the two windows a statement passes for AccountBindingHoldsSQL, in
// seconds: OwnerLockLegacyWindow and OwnerLockMarkWindow, the owner lock's own, since the two read
// one fence.
func AccountBindingWindows() (legacyWindow, markWindow float64) {
	return OwnerLockLegacyWindow.Seconds(), OwnerLockMarkWindow.Seconds()
}

// AccountBindingHolds reads in tx, once, whether the account binding holds over the fleet
// (AccountBindingHoldsSQL), for a transaction that writes what the binding records or decides on
// it: a sign-in, a mint, a decision's re-read. A holding answer keeps the binding's gate shared to
// tx's end, so a build without the binding that arrives meanwhile waits for tx to commit; every
// statement after it in tx reads the answer it gave.
func AccountBindingHolds(ctx context.Context, tx pgx.Tx) (bool, error) {
	legacy, mark := AccountBindingWindows()
	var holds bool
	if err := tx.QueryRow(ctx, `select `+AccountBindingHoldsSQL("$1", "$2"), legacy, mark).Scan(&holds); err != nil {
		return false, fmt.Errorf("read whether a build without the account binding is in the fleet: %w", err)
	}
	return holds, nil
}

// AccountBindingCatchUp is what one catch-up of an address did (CatchUpAccountBinding): the
// accounts its kept changes replaced, in the order they were replaced; whether one of them was the
// address's first recording, which replaced none; how many of its personal tokens it revoked; the
// accounts it recorded a broker revocation of; and the account that holds the address, which each
// revocation names as the holder.
type AccountBindingCatchUp struct {
	Replaced       []string
	FirstRecording bool
	TokensRevoked  int64
	Owed           []string
	Holder         string
}

// Found reports whether the catch-up found a kept change to catch up.
func (c AccountBindingCatchUp) Found() bool {
	return len(c.Replaced) > 0 || c.FirstRecording
}

// CatchUpAccountBinding catches up, in tx, the changes of email's Google account the database kept
// while the account binding was off (account_binding_replacements, migration 20261121121717),
// doing what the sign-ins that made them would have done with the binding on: for a caller that
// holds email's lock (addressLockSQL) and read the binding as holding in tx (AccountBindingHolds).
// email is the address as people.email holds it.
//
// It revokes every personal token of email that records no account and was made at or after the
// earliest kept change, whichever build minted it and whichever account's credential did, the
// holder's own included: none of them can be told from one a credential from before the change
// minted. For a kept first recording it also revokes every token that records an account other
// than the holder's or none, as a first recording does (revokeOtherAccountsTokens). When issuer is
// set (WithBrokerRevocations: a broker is configured), it records a broker revocation
// (recordBrokerRevocationSQL) of each account replaced, or recorded by a token it revoked, that
// does not hold email now, naming the holder, after it settles what is still owed for the holder
// (settleReclaimedBrokerRevocationsSQL), as a sign-in does; an account that took the address back
// holds it, and is owed nothing. It deletes the address's rows, so a second catch-up finds none and
// does nothing. A token that records the holder's account is left as it is, and so, outside a first
// recording, is one that records another: the binding refuses it once the account it records no
// longer holds the address (api.tokenAccountReplaced).
func CatchUpAccountBinding(ctx context.Context, tx pgx.Tx, email, issuer string) (AccountBindingCatchUp, error) {
	var caught AccountBindingCatchUp
	rows, err := tx.Query(ctx, `
		select replaced_subject, replaced_at from account_binding_replacements where email = $1 order by replaced_at`, email)
	if err != nil {
		return caught, fmt.Errorf("read the account changes of %q kept while the account binding was off: %w", email, err)
	}
	var earliest time.Time
	kept := 0
	for rows.Next() {
		var subject *string
		var at time.Time
		if err := rows.Scan(&subject, &at); err != nil {
			rows.Close()
			return caught, fmt.Errorf("read the account changes of %q kept while the account binding was off: %w", email, err)
		}
		if kept == 0 {
			earliest = at
		}
		kept++
		if subject == nil {
			caught.FirstRecording = true
		} else {
			caught.Replaced = append(caught.Replaced, *subject)
		}
	}
	if err := rows.Err(); err != nil {
		return caught, fmt.Errorf("read the account changes of %q kept while the account binding was off: %w", email, err)
	}
	if kept == 0 {
		return caught, nil
	}
	if err := tx.QueryRow(ctx, `select coalesce(sub, '') from people where email = $1`, email).Scan(&caught.Holder); err != nil {
		return caught, fmt.Errorf("read the Google account %q records: %w", email, err)
	}
	revoked, err := tx.Exec(ctx, `
		update agent_tokens set revoked_at = now()
		where canonical_login(owner) = $1 and revoked_at is null and subject is null and created_at >= $2`, email, earliest)
	if err != nil {
		return caught, fmt.Errorf("revoke the personal tokens of %q made since an account change kept while the account binding was off: %w", email, err)
	}
	caught.TokensRevoked = revoked.RowsAffected()
	owed := caught.Replaced
	if caught.FirstRecording && caught.Holder != "" {
		others, accounts, err := revokeOtherAccountsTokens(ctx, tx, email, caught.Holder)
		if err != nil {
			return caught, err
		}
		caught.TokensRevoked += others
		owed = append(slices.Clone(owed), accounts...)
	}
	for _, subject := range owed {
		if subject != caught.Holder && !slices.Contains(caught.Owed, subject) {
			caught.Owed = append(caught.Owed, subject)
		}
	}
	if issuer != "" && caught.Holder != "" && len(caught.Owed) > 0 {
		if _, err := tx.Exec(ctx, settleReclaimedBrokerRevocationsSQL, email, issuer, caught.Holder); err != nil {
			return caught, fmt.Errorf("settle the broker revocations of %q owed for the Google account that holds it: %w", email, err)
		}
		for _, account := range caught.Owed {
			if _, err := tx.Exec(ctx, recordBrokerRevocationSQL, email, issuer, account, caught.Holder); err != nil {
				return caught, fmt.Errorf("record the broker revocation of %q owed for an account change kept while the account binding was off: %w", email, err)
			}
		}
	} else {
		caught.Owed = nil
	}
	if _, err := tx.Exec(ctx, `delete from account_binding_replacements where email = $1`, email); err != nil {
		return caught, fmt.Errorf("delete the account changes of %q kept while the account binding was off: %w", email, err)
	}
	return caught, nil
}

// CatchUpAccountBindingOf catches up email's kept replacements in a transaction of its own
// (CatchUpAccountBinding), after it takes email's lock and reads whether the binding holds, as a
// sign-in does: held is false, and nothing is written, while a build without the binding is in the
// fleet. The revocation loop runs it for every address with a kept replacement
// (AccountBindingReplacementAddresses).
func CatchUpAccountBindingOf(ctx context.Context, pool *Pool, email, issuer string) (held bool, caught AccountBindingCatchUp, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, caught, fmt.Errorf("catch up the account binding of %q: %w", email, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, addressLockSQL, email); err != nil {
		return false, caught, fmt.Errorf("catch up the account binding of %q: lock the address: %w", email, err)
	}
	if held, err = AccountBindingHolds(ctx, tx); err != nil || !held {
		return false, caught, err
	}
	if caught, err = CatchUpAccountBinding(ctx, tx, email, issuer); err != nil {
		return false, caught, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, caught, fmt.Errorf("catch up the account binding of %q: %w", email, err)
	}
	return true, caught, nil
}

// AccountBindingReplacementAddresses answers every address with a replacement kept while the
// account binding was off (account_binding_replacements), the earliest kept first.
func AccountBindingReplacementAddresses(ctx context.Context, pool *Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `select email from account_binding_replacements group by email order by min(replaced_at)`)
	if err != nil {
		return nil, fmt.Errorf("read the addresses with a replacement kept while the account binding was off: %w", err)
	}
	emails, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the addresses with a replacement kept while the account binding was off: %w", err)
	}
	return emails, nil
}
