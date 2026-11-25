package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The hourly confirmation's two ends of a passkey sign-in (PgPasskeys.settle) name the person by
// their login's hash, never the address: the end of a person no list names, which deletes their
// passkeys, and the end of a sign-in with no passkey left. Each keeps the fields it logged beside
// the person. The enrollment's line is read against a real enrollment in
// cmd/dispatch's TestPasskeyEnrollsOnlyAListedOutsideAddress.
//
// Control: log "email", login again on either line, and its row fails on the address.
func TestThePasskeyStoresEndsNameThePersonByTheirLoginsHash(t *testing.T) {
	ctx := context.Background()
	for _, row := range []struct {
		name     string
		passkeys int
		settle   func(passkeys *PgPasskeys, email string) (bool, error)
		message  string
		fields   []string
	}{
		{"the end of a person no list names", 1, func(passkeys *PgPasskeys, email string) (bool, error) {
			return passkeys.End(ctx, email, "passkey-p1")
		}, "ended the account of a person no list names and deleted their passkeys", []string{"level=WARN", "passkeys_deleted=1"}},
		{"the end of a sign-in with no passkey left", 0, func(passkeys *PgPasskeys, email string) (bool, error) {
			return passkeys.Confirm(ctx, email, "passkey-p1")
		}, "ended a passkey sign-in with no passkey left", []string{"level=WARN"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			database := migratedEmptyStore(t)
			people := brokerPeople(database)
			passkeys := NewPgPasskeys(people, passkeyBindingIssuer)
			const email = "outsider@binding.example"
			recordPasskeyAccount(t, people, email, "passkey-p1")
			if row.passkeys > 0 {
				if _, err := database.Pool.Exec(ctx, `insert into passkeys (credential_id, email, user_handle, public_key, aaguid, flags, project_key, made_by)
					values ('\x01', $1, 'passkey-p1', '\x01', '', 0, 'PRIV', 'alice@example.com')`, email); err != nil {
					t.Fatal(err)
				}
			}
			logs := captureLogs(t)
			if standing, err := row.settle(passkeys, email); err != nil || standing {
				t.Fatalf("%s answered %t, %v; want the sign-in ended", row.name, standing, err)
			}
			out := logs.String()
			if strings.Count(out, row.message) != 1 {
				t.Fatalf("logged %q; want one line holding %q", out, row.message)
			}
			for _, want := range append([]string{"login_hash=" + model.LoginHash(email)}, row.fields...) {
				if !strings.Contains(out, want) {
					t.Errorf("the line %q does not hold %s", out, want)
				}
			}
			if strings.Contains(strings.ToLower(out), email) {
				t.Errorf("the log holds the address %s:\n%s", email, out)
			}
		})
	}
}
