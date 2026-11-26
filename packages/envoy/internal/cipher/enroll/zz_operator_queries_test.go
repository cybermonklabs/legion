package enroll

// Scratch, never merged: runs the two queries of the operate page's "Holding a sign-in's sessions
// to its credential", as written there, against the enroll package's migrated test database.

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
)

const operatorMachinesQuery = `select c.host, split_part(split_part(c.subject, '|', 2), '#', 1) as machine_account,
       c.operator as person, max(c.expires_at) as credential_expires_at, count(e.id) as live_sessions
from launcher_credentials c
left join enrollments e on e.launcher_credential_id = c.id and e.kind = 'session'
  and e.revoked_at is null and enrollment_lease_live(e.lease_expires_at)
where c.subject like 'signin:%' and c.revoked_at is null
  and c.expires_at > now() - interval '7 days'
group by 1, 2, 3
order by 1;`

const operatorAtRiskQuery = `select e.id as enrollment_id, c.operator as person, c.host,
       split_part(split_part(c.subject, '|', 2), '#', 1) as machine_account,
       e.session_id, c.expires_at as credential_expires_at,
       c.revoked_at is not null as credential_revoked
from enrollments e
join launcher_credentials c on c.id = e.launcher_credential_id
where e.kind = 'session' and e.revoked_at is null
  and enrollment_lease_live(e.lease_expires_at)
  and c.subject like 'signin:%'
  and (c.revoked_at is not null or c.expires_at < now() + interval '5 minutes')
order by c.expires_at;`

func TestZZOperatorQueries(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	svc.VMServicesByAccount = map[string]string{machineAccountEmail: machineFleet}
	type seat struct {
		person, host string
		expires      string // an SQL interval from now()
		revoked      bool
		session      bool
	}
	seats := []seat{
		{"ada@example.com", "host-ada", "1 hour", false, true},
		{"bob@example.com", "host-bob", "8 minutes", false, true},
		{"cy@example.com", "host-cy", "3 minutes", false, true},
		{"dee@example.com", "host-dee", "-1 second", false, true},
		{"eve@example.com", "host-eve", "30 minutes", true, true},
		{"fay@example.com", "host-fay", "-8 days", false, false},
		{"gus@example.com", "host-gus", "-2 days", false, false},
	}
	for i, s := range seats {
		thumbprint := "tp-" + s.host
		tx, err := svc.Store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		credID, err := svc.IssueTokenLoginTx(ctx, tx, TokenLogin{
			Subject: exchange.PersonSubject(s.person, machineAccountEmail, thumbprint), Service: machineFleet,
			Operator: s.person, Host: s.host, TokenExpires: time.Now().Add(time.Hour), Thumbprint: thumbprint,
			JWK: json.RawMessage(`{"kty":"EC"}`), Expires: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("seat %d: %v", i, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if s.session {
			cred, err := svc.CredentialForKey(ctx, credID.String(), thumbprint)
			if err != nil {
				t.Fatalf("seat %d: CredentialForKey: %v", i, err)
			}
			operator := s.person
			if _, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "box-" + s.host, Operator: &operator, Thumbprint: "tp-session-" + s.host}); err != nil {
				t.Fatalf("seat %d: Create: %v", i, err)
			}
		}
		if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() + $2::interval,
			revoked_at = case when $3 then now() else null end where id = $1`, credID, s.expires, s.revoked); err != nil {
			t.Fatalf("seat %d: %v", i, err)
		}
	}

	rows, err := svc.Store.Pool.Query(ctx, operatorMachinesQuery)
	if err != nil {
		t.Fatalf("the machines query: %v", err)
	}
	var hosts []string
	for rows.Next() {
		var host, account, person string
		var expires time.Time
		var live int64
		if err := rows.Scan(&host, &account, &person, &expires, &live); err != nil {
			t.Fatal(err)
		}
		t.Logf("machines: host=%s machine_account=%s person=%s credential_expires_at=%s live_sessions=%d", host, account, person, expires.UTC().Format(time.RFC3339), live)
		hosts = append(hosts, host)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"host-ada", "host-bob", "host-cy", "host-dee", "host-gus"}; !slices.Equal(hosts, want) {
		t.Errorf("the machines query listed %q, want %q", hosts, want)
	}

	rows, err = svc.Store.Pool.Query(ctx, operatorAtRiskQuery)
	if err != nil {
		t.Fatalf("the at-risk query: %v", err)
	}
	var people []string
	for rows.Next() {
		var id, person, host, account string
		var session *string
		var expires time.Time
		var revoked bool
		if err := rows.Scan(&id, &person, &host, &account, &session, &expires, &revoked); err != nil {
			t.Fatal(err)
		}
		t.Logf("at risk: person=%s host=%s machine_account=%s credential_expires_at=%s revoked=%v", person, host, account, expires.UTC().Format(time.RFC3339), revoked)
		people = append(people, person)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(people)
	if want := []string{"cy@example.com", "dee@example.com", "eve@example.com"}; !slices.Equal(people, want) {
		t.Errorf("the at-risk query listed %q, want %q", people, want)
	}
}
