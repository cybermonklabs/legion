package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// revocationLogPerson is the address whose revocations the rows below owe.
const revocationLogPerson = "ada@example.com"

// revocationLogIssuer is the deployment's sign-in issuer, under which every revocation is owed.
const revocationLogIssuer = "https://accounts.google.com"

// revocationLogRig is a server whose broker revocation loop pays what its database owes at a fake
// broker, its store calls failing where a row names one (beforeRevocationStoreCall).
type revocationLogRig struct {
	broker   *fakeBrokerRig
	database *store.Store
	served   *server
}

func newRevocationLogRig(t *testing.T) *revocationLogRig {
	t.Helper()
	broker := newFakeBrokerRig(t)
	database := storetest.Open(t)
	deps, err := NewDeps(withDocumentService(t, DepsInput{
		Store: database, Identity: headerIdentity(database), ServerURL: "https://dispatch.example", Events: events.NewBroker(),
		AgentSecretsURL: broker.server.URL, AgentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
		SignInIssuer: revocationLogIssuer,
	}))
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	return &revocationLogRig{broker: broker, database: database, served: &server{deps: deps, lifetime: t.Context()}}
}

// exec runs statement on the rig's database.
func (r *revocationLogRig) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	if _, err := r.database.Pool.Exec(context.Background(), statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// owe signs the person in under holder-account and records the broker revocation of subject at
// their address, naming holder-account as the holder, owed and claimable now.
func (r *revocationLogRig) owe(t *testing.T, subject string) {
	t.Helper()
	if err := peopleOf(r.database).SignInSubject(context.Background(), revocationLogPerson, "holder-account", "refresh-holder", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	storetest.TestOnlyWriteBrokerRevocations(t, r.database, `
		insert into broker_revocations (email, issuer, subject, holder_subject, recorded_at, session_generation)
		values ($1, $2, $3, 'holder-account', now(), (select generation from user_sessions where login = $1))`,
		revocationLogPerson, revocationLogIssuer, subject)
}

// oweMachineLogins records the revocation of the person's machine logins, as a removal does, and
// has the broker list one login the person approved.
func (r *revocationLogRig) oweMachineLogins(t *testing.T) {
	t.Helper()
	storetest.RecordMachineLoginRevocation(t, r.database, revocationLogPerson)
	r.broker.bodyByRoute["GET /v1/launcher-credentials"] = `{"credentials":[{"credential_id":"cred-ada","host":"ada-laptop","service":null,"subject":null,"approved_by":"` +
		revocationLogPerson + `","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false}]}`
}

// failing has the server's store call named call fail with an error that quotes the person, as the
// store's own errors do.
func (r *revocationLogRig) failing(call string) {
	r.served.beforeRevocationStoreCall = func(name string) error {
		if name != call {
			return nil
		}
		return fmt.Errorf("%s of %s: %w", call, revocationLogPerson, &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "the database is down"})
	}
}

const (
	revokeRoute    = "POST /v1/person-subjects/revoke"
	revokedAnswer  = `{"state":"revoked","signins":1,"machine_logins":2,"tokens_ended":3}`
	supersedeReply = `{"state":"superseded","superseded_by":99}`
	databaseDown   = "database SQLSTATE 57P01"
)

// Each line the broker revocation loop logs about a person names them by their login's hash and a
// failure by its kind, never the address or the store's or the broker client's error, each of
// which quotes it: the account-binding catch-up that fails; a claimed revocation whose SettleHeld
// fails, whose account holds its address again, whose Sendable fails, whose claim may send nothing,
// that the broker does not confirm, that the broker answers superseded after another pass settled
// it, superseded while still owed (with the address's generation read and unread), whose Settle
// fails, and that the broker revokes; and a machine-login revocation whose claim may send nothing,
// that the broker does not confirm, whose Settle fails, that stays owed, and that settles. Each
// keeps the fields it logged beside the person.
//
// Control: log "email", email (or revocation.Email) or "error", err again on any of the loop's
// lines, or the session generation's error as text, and its row fails on the address.
func TestTheRevocationLoopsLinesNameThePersonByTheirLoginsHash(t *testing.T) {
	for _, row := range []struct {
		name    string
		set     func(t *testing.T, r *revocationLogRig)
		machine bool
		level   string
		message string
		kind    string
		fields  []string
	}{
		{name: "a claimed revocation whose SettleHeld fails", set: func(t *testing.T, r *revocationLogRig) {
			r.owe(t, "replaced-account")
			r.failing(callSettleHeld)
		}, level: "ERROR", message: "read whether a claimed broker revocation's account holds its address again", kind: databaseDown},
		{name: "a claimed revocation whose account holds its address again", set: func(t *testing.T, r *revocationLogRig) {
			// replaced-account takes the address back through a build without the binding, which
			// settles nothing.
			r.owe(t, "replaced-account")
			storetest.TestOnlyRecordAccountUnkept(t, r.database, revocationLogPerson, "replaced-account")
		}, level: "WARN", message: "it is settled, not sent", fields: []string{"subject=replaced-account"}},
		{name: "a claimed revocation whose Sendable fails", set: func(t *testing.T, r *revocationLogRig) {
			r.owe(t, "replaced-account")
			r.failing(callSendable)
		}, level: "ERROR", message: "read whether a claimed broker revocation may still be sent", kind: databaseDown},
		{name: "a claimed revocation whose claim may send nothing", set: func(t *testing.T, r *revocationLogRig) {
			r.owe(t, "replaced-account")
			r.served.deps.AgentSecrets.HTTP.Timeout = brokerRevocationRetry
		}, level: "INFO", message: "a claimed broker revocation was not sent", fields: []string{"send_within=0s"}},
		{name: "a revocation the broker does not confirm", set: func(t *testing.T, r *revocationLogRig) {
			r.owe(t, "replaced-account")
			r.broker.statusByRoute[revokeRoute] = http.StatusServiceUnavailable
		}, level: "ERROR", message: "did not revoke the credentials of an account another Google account took the address of", kind: "broker HTTP 503"},
		{name: "a revocation superseded after another pass settled it", set: func(t *testing.T, r *revocationLogRig) {
			r.owe(t, "replaced-account")
			r.broker.bodyByRoute[revokeRoute] = supersedeReply
			r.broker.beforeAnswer[revokeRoute] = func() { r.exec(t, `update broker_revocations set done_at = now() where done_at is null`) }
		}, level: "WARN", message: "superseded that another pass has settled since", fields: []string{"subject=replaced-account", "superseded_by=99"}},
		{name: "a revocation superseded while still owed", set: func(t *testing.T, r *revocationLogRig) {
			r.owe(t, "replaced-account")
			r.broker.bodyByRoute[revokeRoute] = supersedeReply
		}, level: "ERROR", message: "answered a revocation superseded: Dispatch's session generation", fields: []string{"superseded_by=99", "address_session_generation=1"}},
		{name: "a revocation superseded whose address's generation cannot be read", set: func(t *testing.T, r *revocationLogRig) {
			r.owe(t, "replaced-account")
			r.broker.bodyByRoute[revokeRoute] = supersedeReply
			r.failing(callSessionGeneration)
		}, level: "ERROR", message: "answered a revocation superseded: Dispatch's session generation",
			fields: []string{`address_session_generation="unread: ` + databaseDown + `"`, "still_owed_unread=false"}},
		{name: "a revocation the broker revoked whose Settle fails", set: func(t *testing.T, r *revocationLogRig) {
			r.owe(t, "replaced-account")
			r.broker.bodyByRoute[revokeRoute] = revokedAnswer
			r.failing(callSettle)
		}, level: "ERROR", message: "and the revocation could not be settled", kind: databaseDown},
		{name: "a revocation the broker revoked", set: func(t *testing.T, r *revocationLogRig) {
			r.owe(t, "replaced-account")
			r.broker.bodyByRoute[revokeRoute] = revokedAnswer
		}, level: "WARN", message: `msg="dispatch: the secrets broker revoked the credentials of an account another Google account took the address of"`,
			fields: []string{"signins=1", "machine_logins=2", "tokens_ended=3", "settled=true"}},
		{name: "a machine-login revocation whose claim may send nothing", machine: true, set: func(t *testing.T, r *revocationLogRig) {
			r.oweMachineLogins(t)
			r.served.deps.AgentSecrets.HTTP.Timeout = brokerRevocationRetry
		}, level: "INFO", message: "a claimed machine-login revocation sent no more calls", fields: []string{"revoked=0", "send_within=0s"}},
		{name: "a machine-login revocation the broker does not confirm", machine: true, set: func(t *testing.T, r *revocationLogRig) {
			r.oweMachineLogins(t)
			r.broker.statusByRoute["GET /v1/launcher-credentials"] = http.StatusServiceUnavailable
		}, level: "ERROR", message: "were not revoked: the secrets broker did not confirm", kind: "broker HTTP 503", fields: []string{"revoked=0"}},
		{name: "a machine-login revocation whose Settle fails", machine: true, set: func(t *testing.T, r *revocationLogRig) {
			r.oweMachineLogins(t)
			removalAged(t, r.database, machineLoginSettleAfter)
			r.failing(callMachineSettle)
		}, level: "ERROR", message: "and the revocation could not be settled; it is paid again", kind: databaseDown},
		{name: "a machine-login revocation that stays owed", machine: true, set: func(t *testing.T, r *revocationLogRig) {
			r.oweMachineLogins(t)
		}, level: "INFO", message: "the revocation stays owed", fields: []string{"machine_logins=1"}},
		{name: "a machine-login revocation that settles", machine: true, set: func(t *testing.T, r *revocationLogRig) {
			r.oweMachineLogins(t)
			removalAged(t, r.database, machineLoginSettleAfter)
		}, level: "WARN", message: `msg="dispatch: the secrets broker revoked the machine logins of a restricted member taken off an access list or reset by a project's owner"`,
			fields: []string{"machine_logins=1"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRevocationLogRig(t)
			row.set(t, r)
			var logs logtest.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))
			if row.machine {
				r.served.revokeOwedMachineLogins(t.Context())
			} else {
				r.served.revokeOwedBrokerCredentials(t.Context())
			}
			lines := logs.Lines(row.message)
			if strings.Count(lines, "\n") != 1 {
				t.Fatalf("logged %q; want one line holding %q", logs.String(), row.message)
			}
			want := append([]string{"level=" + row.level, "login_hash=" + model.LoginHash(revocationLogPerson)}, row.fields...)
			if row.kind != "" {
				want = append(want, fmt.Sprintf("error_kind=%q", row.kind))
			}
			for _, field := range want {
				if !strings.Contains(lines, field) {
					t.Errorf("the line %q does not hold %s", lines, field)
				}
			}
			all := logs.String()
			if strings.Contains(strings.ToLower(all), revocationLogPerson) {
				t.Errorf("the log holds the address %s:\n%s", revocationLogPerson, all)
			}
			for _, word := range []string{"the database is down", "agent-secrets:", "broker returned"} {
				if strings.Contains(all, word) {
					t.Errorf("the log holds %q, a word of an error's own text:\n%s", word, all)
				}
			}
		})
	}
}
