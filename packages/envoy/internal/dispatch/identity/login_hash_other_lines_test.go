package identity

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// outsider is a restricted member's address, of no domain the Workspace owns.
const outsider = "erin@other.example"

// stubPasskeys is a passkey store whose end always ends.
type stubPasskeys struct{}

func (stubPasskeys) Issuer() string                                        { return "https://outside.example.com" }
func (stubPasskeys) Confirm(context.Context, string, string) (bool, error) { return false, nil }
func (stubPasskeys) End(context.Context, string, string) (bool, error)     { return true, nil }

// unreadablePeople is testPeopleStore whose membership read fails, quoting the address as the
// people store's read does.
type unreadablePeople struct{ *testPeopleStore }

func (unreadablePeople) Membership(_ context.Context, email string) (auth.PersonMembership, bool, error) {
	return auth.PersonMembership{}, false, fmt.Errorf("read membership of %q: %w", email,
		&pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "the database is down"})
}

// The sign-in rules' other lines name a person by their login's hash, a refusal by its words and a
// failure by its kind: the lists that cannot confirm a lapsed passkey sign-in, the end of one no
// list names, a reason a signed-out session's read cannot give, an IAP assertion the rule refuses,
// and the membership pass's question the broker client cannot answer. Each line's own failure
// names the address (AdmitsRestricted's "%s: %w", the store's "%q", googleRefusal's, the broker
// client's), and no line holds it.
//
// Control: log "email", email, "error", err, or "reason", err.Error() again on any of those lines,
// and its row fails on the address.
func TestTheRulesOtherLinesNameThePersonByTheirLoginsHash(t *testing.T) {
	ctx := context.Background()
	for _, row := range []struct {
		name, message, person string
		want                  []string
		run                   func(t *testing.T)
	}{
		{name: "lists that cannot confirm a lapsed passkey sign-in", person: outsider,
			message: "the access lists could not be read to confirm a lapsed passkey sign-in",
			want:    []string{`error_kind="database SQLSTATE 57P01"`},
			run: func(t *testing.T) {
				lister := &stubLister{err: fmt.Errorf("read whether %s is listed: %w", outsider, &pgconn.PgError{Code: "57P01"})}
				m := &Membership{Rule: OutsideRule{DomainRule: DomainRule{Domain: memberDomain}, People: lister}, Passkeys: stubPasskeys{}}
				_ = m.confirmLapsedPasskey(ctx, outsider, "subject-1")
			}},
		{name: "the end of a lapsed passkey sign-in no list names", person: outsider,
			message: "dispatch: sign-in ended: the hourly confirmation no longer admits the person",
			want:    []string{`reason="no Dispatch project's access list names this account"`},
			run: func(t *testing.T) {
				m := &Membership{Rule: OutsideRule{DomainRule: DomainRule{Domain: memberDomain}, People: &stubLister{}}, Passkeys: stubPasskeys{}}
				_ = m.confirmLapsedPasskey(ctx, outsider, "subject-1")
			}},
		{name: "a signed-out session whose reason cannot be read", person: outsider,
			message: "could not read why a person's session ended",
			want:    []string{`error_kind="database SQLSTATE 57P01"`},
			run: func(t *testing.T) {
				m := &Membership{Rule: OutsideRule{DomainRule: DomainRule{Domain: memberDomain}, People: &stubLister{}},
					People: unreadablePeople{newTestPeopleStore(nil)}}
				_ = m.signedOut(ctx, outsider)
			}},
		{name: "an IAP assertion the rule refuses", person: "a.person@other.example",
			message: "dispatch: IAP person refused",
			want:    []string{`reason="not in the domain that may use Dispatch"`, "hd=other.example"},
			run: func(t *testing.T) {
				rig := newIAPRig(t)
				_, _ = rig.login(rig.assertion(t, rig.signing, "other.example", nil), nil)
			}},
		{name: "the membership pass's question", person: "gail@example.com",
			message: "the membership pass could not ask whether a signed-out person has access that outlives the browser",
			want:    []string{`error_kind=*errors.errorString`},
			run: func(t *testing.T) {
				database := storetest.Open(t)
				people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
				if err := people.SignInSubject(ctx, "gail@example.com", "sub-gail", "refresh-gail", "example.com", model.MemberFull); err != nil {
					t.Fatal(err)
				}
				read, _, err := people.Membership(ctx, "gail@example.com")
				if err != nil {
					t.Fatal(err)
				}
				if err := people.KeepAfterSignOut(ctx, "gail@example.com", read.SignedInAt); err != nil {
					t.Fatal(err)
				}
				// No database for the question, so it fails naming the address (cipher.OutlivingAccess.Holds).
				pass := MembershipPass{Membership: &Membership{People: people, Rule: workspace}, People: people, Outliving: cipher.OutlivingAccess{}}
				_ = pass.Run(ctx)
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			var logs logtest.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))
			row.run(t)
			line := loggedLine(t, &logs, row.message)
			for _, want := range append([]string{"login_hash=" + model.LoginHash(row.person)}, row.want...) {
				if !strings.Contains(line, want) {
					t.Errorf("the line %q does not hold %s", line, want)
				}
			}
			holdsNone(t, logs.String(), row.person, "read membership", "the database is down", "is listed", "holds a personal token")
		})
	}
}
