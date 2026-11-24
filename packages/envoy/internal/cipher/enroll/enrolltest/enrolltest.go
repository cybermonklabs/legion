// Package enrolltest is the enrollment service on a real database, for a test of another
// package: identity/boot's TestStoredPrincipalsStayValid, which replays the identity strings
// today's broker stored and authenticates each with the new code. It is ordinary code rather
// than a _test.go file because a test of another package cannot import one, and no binary
// imports it. enroll's own tests keep their copies of these SQL helpers: a test inside enroll
// cannot import a package that imports enroll.
package enrolltest

import (
	"context"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// NewService is an enroll.Service on a fresh store (storetest.Open, which skips t when
// BROKER_TEST_DATABASE_URL is unset), wired as cmd/cipherd wires it: the real session-name binder
// (oauthstore.New), and the subjects, policy and pod verifier given.
func NewService(t testing.TB, subjects identity.Subjects, policy *exchange.Set, pods identity.PodVerifier) *enroll.Service {
	t.Helper()
	st := storetest.Open(t)
	svc := &enroll.Service{Store: st, Lease: time.Hour, Subjects: subjects, Policy: policy, Pod: pods}
	svc.Sessions = oauthstore.New(st.Pool)
	return svc
}

// TokenLogin issues login through svc.IssueTokenLogin under a fresh key (it sets Thumbprint and
// JWK) and answers the credential as Create sees it (svc.CredentialForKey).
func TokenLogin(t testing.TB, svc *enroll.Service, login enroll.TokenLogin) enroll.Credential {
	t.Helper()
	ctx := context.Background()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	jwk, err := (jose.JSONWebKey{Key: &key.PublicKey}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	login.Thumbprint, login.JWK = thumbprint, jwk
	if login.Expires.IsZero() {
		login.Expires = time.Now().Add(time.Hour)
	}
	if login.TokenExpires.IsZero() {
		login.TokenExpires = login.Expires
	}
	id, err := svc.IssueTokenLogin(ctx, login)
	if err != nil {
		t.Fatalf("IssueTokenLogin(%s): %v", login.Subject, err)
	}
	cred, err := svc.CredentialForKey(ctx, id.String(), thumbprint)
	if err != nil {
		t.Fatalf("CredentialForKey(%s): %v", id, err)
	}
	return cred
}

// InsertLiveGrant writes a granted request and its live grant under enrollment directly (grant
// issuance is requests.Machine's), and answers the grant's id.
func InsertLiveGrant(t testing.TB, svc *enroll.Service, enrollment uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	requestID, grantID := uuid.New(), uuid.New()
	if _, err := svc.Store.Pool.Exec(ctx, `insert into requests (id, enrollment_id, reason, state, rules_version, lifetime_seconds)
		values ($1,$2,'test fixture','granted','v1',3600)`, requestID, enrollment); err != nil {
		t.Fatalf("insert request fixture: %v", err)
	}
	if _, err := svc.Store.Pool.Exec(ctx, `insert into grants (id, request_id, enrollment_id, expires_at) values ($1,$2,$3, now() + interval '1 hour')`,
		grantID, requestID, enrollment); err != nil {
		t.Fatalf("insert grant fixture: %v", err)
	}
	return grantID
}

// GrantHolder is the enrollment a grant hangs off now.
func GrantHolder(t testing.TB, svc *enroll.Service, grant uuid.UUID) uuid.UUID {
	t.Helper()
	var holder uuid.UUID
	if err := svc.Store.Pool.QueryRow(context.Background(), `select enrollment_id from grants where id=$1`, grant).Scan(&holder); err != nil {
		t.Fatalf("read the enrollment of grant %s: %v", grant, err)
	}
	return holder
}

// CarryPrincipal is the principal an enrollment's grants follow, "" for null.
func CarryPrincipal(t testing.TB, svc *enroll.Service, enrollment uuid.UUID) string {
	t.Helper()
	return column(t, svc, "carry_principal", enrollment)
}

// Owner is the owner an enrollment records, "" for null.
func Owner(t testing.TB, svc *enroll.Service, enrollment uuid.UUID) string {
	t.Helper()
	return column(t, svc, "owner", enrollment)
}

// SetCarryPrincipal writes an enrollment's carry_principal, so a test can put back a value today's
// broker stored.
func SetCarryPrincipal(t testing.TB, svc *enroll.Service, enrollment uuid.UUID, value string) {
	t.Helper()
	if _, err := svc.Store.Pool.Exec(context.Background(), `update enrollments set carry_principal=$2 where id=$1`, enrollment, value); err != nil {
		t.Fatalf("write the carry principal of %s: %v", enrollment, err)
	}
}

// column reads one nullable text column of an enrollment, "" for null. The column name is this
// package's own literal, never a caller's.
func column(t testing.TB, svc *enroll.Service, name string, enrollment uuid.UUID) string {
	t.Helper()
	var value *string
	if err := svc.Store.Pool.QueryRow(context.Background(), `select `+name+` from enrollments where id=$1`, enrollment).Scan(&value); err != nil {
		t.Fatalf("read the %s of %s: %v", name, enrollment, err)
	}
	if value == nil {
		return ""
	}
	return *value
}
