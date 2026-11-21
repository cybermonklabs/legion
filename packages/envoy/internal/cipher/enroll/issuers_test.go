package enroll

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// withTwoIssuers wires svc to verify the tokens of two named issuers, as BROKER_K8S_OIDC_ISSUER's
// first=<URL>,second=<URL> does, each a local OIDC issuer of its own, and returns the two with
// their signing keys.
func withTwoIssuers(t *testing.T, svc *Service) (first, second *oidctest.Issuer, firstKey, secondKey *oidctest.Key) {
	t.Helper()
	first, second = oidctest.New(t), oidctest.New(t)
	firstKey, secondKey = first.PublishKey(t, "first-key"), second.PublishKey(t, "second-key")
	svc.Issuers = []oidc.NamedIssuer{{Name: "first", URL: first.URL()}, {Name: "second", URL: second.URL()}}
	svc.Pod = K8sPodVerifier{Issuers: oidc.NewIssuerSet(svc.Issuers, testAudience, 0)}
	return first, second, firstKey, secondKey
}

// TestAPodTokenWhoseIssuerCannotBeReadIsErrIssuerUnavailable: a pod's token of a named issuer whose
// discovery document or key set the broker cannot read is ErrIssuerUnavailable at enrollment and at
// renewal, which wraps oidc.ErrIssuerUnavailable and is not ErrPodIdentity: nothing was decided
// about the token. The other issuer's pods enroll meanwhile. A token of an issuer the list does not
// name is still ErrPodIdentity, as a token of another issuer is on a broker of one issuer.
func TestAPodTokenWhoseIssuerCannotBeReadIsErrIssuerUnavailable(t *testing.T) {
	const account = "system:serviceaccount:legion:worker"
	ctx := context.Background()
	for _, c := range []struct {
		name string
		// fault breaks the second issuer; nil leaves both readable and presents a token of a
		// third issuer, which the list does not name.
		fault       func(*oidctest.Issuer)
		unavailable bool
	}{
		{"its discovery document cannot be read", func(issuer *oidctest.Issuer) { issuer.FailDiscovery(http.StatusServiceUnavailable) }, true},
		{"its key set cannot be read", func(issuer *oidctest.Issuer) { issuer.FailKeys(http.StatusServiceUnavailable) }, true},
		{"an issuer the list does not name", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := newService(t)
			first, second, firstKey, secondKey := withTwoIssuers(t, svc)
			cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
			enrolled, err := svc.Create(ctx, cred, Enrollment{
				Kind: "pod", RuntimeID: "pod-of-first", Thumbprint: "tp-first",
				PodToken: mintPodToken(t, first, firstKey, account, "pod-of-first"),
			})
			if err != nil {
				t.Fatalf("Create(a pod of the first issuer) = %v, want it enrolled", err)
			}
			token := func(podUID string) string { return mintPodToken(t, second, secondKey, account, podUID) }
			if c.fault != nil {
				c.fault(second)
			} else {
				third := oidctest.New(t)
				thirdKey := third.PublishKey(t, "third-key")
				token = func(podUID string) string { return mintPodToken(t, third, thirdKey, account, podUID) }
			}
			check := func(what string, err error) {
				t.Helper()
				switch {
				case c.unavailable && (!errors.Is(err, ErrIssuerUnavailable) || !errors.Is(err, oidc.ErrIssuerUnavailable) || errors.Is(err, ErrPodIdentity)):
					t.Fatalf("%s = %v, want ErrIssuerUnavailable, wrapping oidc.ErrIssuerUnavailable and not ErrPodIdentity", what, err)
				case !c.unavailable && (!errors.Is(err, ErrPodIdentity) || errors.Is(err, ErrIssuerUnavailable)):
					t.Fatalf("%s = %v, want ErrPodIdentity, as a token of another issuer is", what, err)
				}
			}
			_, err = svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: "pod-of-second", Thumbprint: "tp-second", PodToken: token("pod-of-second")})
			check("Create(the pod's token)", err)
			_, err = svc.Renew(ctx, enrolled.ID.String(), token("pod-of-first"))
			check("Renew(the pod's token)", err)
			if _, err := svc.Renew(ctx, enrolled.ID.String(), mintPodToken(t, first, firstKey, account, "pod-of-first")); err != nil {
				t.Fatalf("Renew(the first issuer's pod with its own token) = %v, want renewed: the other issuer stops none of its pods", err)
			}
		})
	}
}

// TestATokenLoginsCredentialIsHeldToTheIssuersItsEntryNames: a token login's credential records
// the URL of the issuer whose token made it, in its row and in its token_login audit row, and
// authenticates only while the subject's BROKER_SERVICE_LOGINS entry names that issuer. A
// credential recording none, made before credentials recorded one, counts as the first issuer's,
// and one recording an issuer the setting no longer holds is named by no entry. A later login from
// another issuer moves the credential to that issuer.
func TestATokenLoginsCredentialIsHeldToTheIssuersItsEntryNames(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	first := oidc.NamedIssuer{Name: "first", URL: "https://first.example"}
	second := oidc.NamedIssuer{Name: "second", URL: "https://second.example"}
	svc.Issuers = []oidc.NamedIssuer{first, second}
	svc.ServiceLogins = map[string]ServiceLogin{}
	login := func(t *testing.T, subject, issuer string) string {
		t.Helper()
		key := newLoginKey(t)
		expires := time.Now().Add(time.Hour)
		id, err := svc.IssueTokenLogin(ctx, TokenLogin{
			Subject: subject, Service: "legion-daemon", Host: "legion-system/legion-daemon", Issuer: issuer,
			TokenExpires: expires, Thumbprint: key.thumbprint, JWK: key.jwk, Expires: expires,
		})
		if err != nil {
			t.Fatalf("IssueTokenLogin(%s from %q): %v", subject, issuer, err)
		}
		return id.String()
	}
	recorded := func(t *testing.T, id string) (row, audit *string) {
		t.Helper()
		if err := svc.Store.Pool.QueryRow(ctx, `select issuer from launcher_credentials where id=$1`, id).Scan(&row); err != nil {
			t.Fatal(err)
		}
		if err := svc.Store.Pool.QueryRow(ctx, `select detail->>'issuer' from audit where kind='launcher_credential.token_login' and detail->>'credential_id'=$1 order by id desc limit 1`, id).Scan(&audit); err != nil {
			t.Fatal(err)
		}
		return row, audit
	}
	for i, c := range []struct {
		name string
		// issuer is the URL the login's token named, "" for a credential that records none.
		issuer string
		names  []string
		want   bool
	}{
		{"the second issuer's, while its entry names both", second.URL, []string{"first", "second"}, true},
		{"the second issuer's, once its entry names the first alone", second.URL, []string{"first"}, false},
		{"the first issuer's, while its entry names it", first.URL, []string{"first"}, true},
		{"one recording no issuer, while its entry names the first", "", []string{"first"}, true},
		{"one recording no issuer, while its entry names the second alone", "", []string{"second"}, false},
		{"an issuer the setting no longer holds", "https://gone.example", []string{"first", "second"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			subject := "system:serviceaccount:legion-system:legion-daemon-" + strconv.Itoa(i)
			svc.ServiceLogins[subject] = ServiceLogin{Service: "legion-daemon", Issuers: c.names}
			id := login(t, subject, c.issuer)
			row, audit := recorded(t, id)
			if c.issuer == "" && (row != nil || audit != nil) || c.issuer != "" && (row == nil || *row != c.issuer || audit == nil || *audit != c.issuer) {
				t.Fatalf("the credential records the issuer %v and its audit row %v, want %q in both (none for \"\")", row, audit, c.issuer)
			}
			if _, ok, err := svc.AuthenticateLauncher(ctx, id); err != nil || ok != c.want {
				t.Fatalf("AuthenticateLauncher = %v, %v; want %v: its entry names %v", ok, err, c.want, c.names)
			}
		})
	}
	t.Run("a later login from the first issuer moves the credential to it", func(t *testing.T) {
		const subject = "system:serviceaccount:legion-system:legion-daemon-moved"
		svc.ServiceLogins[subject] = ServiceLogin{Service: "legion-daemon", Issuers: []string{"first"}}
		fromSecond := login(t, subject, second.URL)
		if _, ok, err := svc.AuthenticateLauncher(ctx, fromSecond); err != nil || ok {
			t.Fatalf("AuthenticateLauncher(the second issuer's login) = %v, %v; want refused: the entry names the first alone", ok, err)
		}
		fromFirst := login(t, subject, first.URL)
		if fromFirst != fromSecond {
			t.Fatalf("the first issuer's login answered credential %s, want the subject's one, %s", fromFirst, fromSecond)
		}
		if row, _ := recorded(t, fromFirst); row == nil || *row != first.URL {
			t.Fatalf("the moved credential records the issuer %v, want %s", row, first.URL)
		}
		if _, ok, err := svc.AuthenticateLauncher(ctx, fromFirst); err != nil || !ok {
			t.Fatalf("AuthenticateLauncher(the moved credential) = %v, %v; want it authenticated", ok, err)
		}
	})
}

// TestAPodsEnrollmentRecordsTheIssuerOfItsToken: Create records in a pod's enrollment row the
// URL of the issuer whose token proved the pod (enrollments.issuer), each of two named issuers'
// own, which the request machine reads the pod's exchange entry against; a session's row records
// none.
func TestAPodsEnrollmentRecordsTheIssuerOfItsToken(t *testing.T) {
	const account = "system:serviceaccount:legion:worker"
	ctx := context.Background()
	svc := newService(t)
	first, second, firstKey, secondKey := withTwoIssuers(t, svc)
	recorded := func(t *testing.T, enrolled Enrollment) *string {
		t.Helper()
		var issuer *string
		if err := svc.Store.Pool.QueryRow(ctx, `select issuer from enrollments where id=$1`, enrolled.ID).Scan(&issuer); err != nil {
			t.Fatal(err)
		}
		return issuer
	}
	cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	for _, c := range []struct {
		name, uid string
		issuer    *oidctest.Issuer
		key       *oidctest.Key
	}{
		{"a pod of the first issuer", "pod-of-first", first, firstKey},
		{"a pod of the second issuer", "pod-of-second", second, secondKey},
	} {
		t.Run(c.name, func(t *testing.T) {
			enrolled, err := svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: c.uid, Thumbprint: "tp-" + c.uid, PodToken: mintPodToken(t, c.issuer, c.key, account, c.uid)})
			if err != nil {
				t.Fatalf("Create(%s) = %v, want it enrolled", c.name, err)
			}
			if got := recorded(t, enrolled); got == nil || *got != c.issuer.URL() {
				t.Fatalf("%s's enrollment records the issuer %v, want %s, whose token proved it", c.name, got, c.issuer.URL())
			}
		})
	}
	t.Run("a session", func(t *testing.T) {
		session, err := svc.Create(ctx, mintCredential(t, svc, str("ada@example.com"), nil, "box"), Enrollment{Kind: "session", RuntimeID: "box-1", Thumbprint: "tp-box-1"})
		if err != nil {
			t.Fatalf("Create(a session) = %v, want it enrolled", err)
		}
		if got := recorded(t, session); got != nil {
			t.Fatalf("a session's enrollment records the issuer %q, want none", *got)
		}
	})
}
