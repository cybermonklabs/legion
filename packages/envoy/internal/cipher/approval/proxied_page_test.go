// packages/envoy/internal/cipher/approval/proxied_page_test.go
// What the approval page says about a record that asks for a stored secret its proxy tag puts
// behind a kind and that no profile serves: the kind is the whole of where an approval sends its
// credential, so the page prints the kind, its hosts and the requests it allows, and binds the
// approval to that kind as it binds one to a profile.
package approval_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// proxiedKinds are the kinds of these rows: deel, on api.deel.example, which allows two requests;
// open, on two hosts, which allows every request there; and sealed, which allows none.
const proxiedKinds = `[` +
	`{"name": "deel", "hosts": ["api.deel.example"], "ways": ["swap"], "allow": [{"method": "GET", "pathPrefix": "/v1/payroll/"}, {"method": "POST", "pathPrefix": "/v1/payroll/exports"}]}, ` +
	`{"name": "open", "hosts": ["api.open.example", "uploads.open.example"], "ways": ["swap"]}, ` +
	`{"name": "sealed", "hosts": ["api.sealed.example"], "ways": ["swap"], "allow": []}]`

// proxiedSecret is DEEL_TOKEN, ada's human-tier stored secret, which ben's request waits on her
// for, with its proxy tag naming kind, or none for "".
func proxiedSecret(kind string) awstest.LocalSecret {
	secret := policytest.Secret("DEEL_TOKEN", ada, policy.TierHuman, "deel-v1")
	if kind != "" {
		secret = policytest.Proxied(secret, kind)
	}
	return secret
}

// TestTheApprovalPagePrintsTheKindOfASecretWithNoProfile: a record for a stored secret served by
// its own name behind a kind prints, beside the name it asks for, the kind, the hosts the kind lets
// its credential go to and the requests the kind allows there, each its own row, so a page that
// leaves one out fails that row alone; it prints no profile and no source, since the secret has
// none. A kind with no allow is printed as allowing every request on its hosts, and one with an
// empty allow as allowing none, rather than as no line. The decision form carries the secret's
// fingerprint, as it carries a profile's.
func TestTheApprovalPagePrintsTheKindOfASecretWithNoProfile(t *testing.T) {
	for _, c := range []struct {
		kind string
		rows []string
	}{
		{"deel", []string{"Behind the proxy DEEL_TOKEN", "Kind deel", "Proxy hosts api.deel.example", "Allowed requests GET /v1/payroll/, POST /v1/payroll/exports"}},
		{"open", []string{"Kind open", "Proxy hosts api.open.example, uploads.open.example", "Allowed requests any method on any path of these hosts"}},
		{"sealed", []string{"Kind sealed", "Proxy hosts api.sealed.example", "Allowed requests none: the kind allows no request"}},
	} {
		t.Run(c.kind, func(t *testing.T) {
			r := newRig(t)
			local := r.machine.Secrets.(aws.Store).Client.(*awstest.Local)
			local.Put(proxiedSecret(c.kind))
			r.machine.Policy = policytest.ProfiledCurrent(t, local, policytest.KindsFileOf(proxiedKinds))
			served := r.machine.Policy.Get().Secrets["DEEL_TOKEN"]
			if served.Proxy != c.kind || served.Profile != nil || served.ProfileDigest == "" {
				t.Fatalf("DEEL_TOKEN is served behind %q with profile %v and fingerprint %q; want behind %s by its tag alone, with a fingerprint", served.Proxy, served.Profile, served.ProfileDigest, c.kind)
			}
			enrollmentID, key := r.benSession(t, "ben-box")
			p := r.request(t, enrollmentID, key, "export the payroll", nil, "", "DEEL_TOKEN")

			b := r.browser(t)
			b.SignIn(t, p.recordID, person(ada))
			body := b.Page(t, p.recordID).Body
			block := pageText(t, recordBlock(t, body, p.recordID))
			for _, row := range c.rows {
				if !strings.Contains(block, row) {
					t.Errorf("the record's block does not print %q:\n%s", row, block)
				}
			}
			for _, absent := range []string{"Profile DEEL_TOKEN", "Source"} {
				if strings.Contains(block, absent) {
					t.Errorf("the block of a secret with no profile prints %q:\n%s", absent, block)
				}
			}
			form := decisionForm(t, body, p.recordID)
			if form == nil {
				t.Fatalf("the page for %s offers no decision", p.recordID)
			}
			if want := p.recordID + " DEEL_TOKEN " + served.ProfileDigest; len(form["profile_digest"]) != 1 || form["profile_digest"][0] != want {
				t.Fatalf("the decision form carries the fingerprints %q; want [%q], the one the page printed", form["profile_digest"], want)
			}
		})
	}
}

// TestAnApprovalIsBoundToTheKindOfASecretWithNoProfile: an approval of a stored secret behind a
// kind is bound to the kind its page printed, as an approval of a profile is bound to its profile:
// once the secret's proxy tag names another kind, or is taken off, after the page was made, the
// approval is refused, 403 Profile changed with nothing decided, since what it would grant is not
// what the person read; and so is the approval of a page that printed the secret behind no kind
// once a tag puts it behind one. The page opened again prints the secret as it is now, and that
// page's approval grants. The control: the same flow with no retag grants from the first page.
func TestAnApprovalIsBoundToTheKindOfASecretWithNoProfile(t *testing.T) {
	for _, c := range []struct {
		name, printed, live string
		moved               bool
	}{
		{"the control: no retag", "deel", "deel", false},
		{"the proxy tag names another kind", "deel", "open", true},
		{"the proxy tag is taken off", "deel", "", true},
		{"a proxy tag is put on", "", "deel", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			local := r.machine.Secrets.(aws.Store).Client.(*awstest.Local)
			local.Put(proxiedSecret(c.printed))
			r.machine.Policy = policytest.ProfiledCurrent(t, local, policytest.KindsFileOf(proxiedKinds))
			enrollmentID, key := r.benSession(t, "ben-box")
			p := r.request(t, enrollmentID, key, "export the payroll", nil, "", "DEEL_TOKEN")

			b := r.browser(t)
			b.SignIn(t, p.recordID, person(ada))
			form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
			if form == nil {
				t.Fatalf("the page for %s offers no decision", p.recordID)
			}
			if c.printed != c.live {
				// The owner's retag, which the next policy read serves.
				local.Put(proxiedSecret(c.live))
				if err := r.machine.Policy.Refresh(t.Context()); err != nil {
					t.Fatalf("policy refresh: %v", err)
				}
				if got := r.machine.Policy.Get().Secrets["DEEL_TOKEN"].Proxy; got != c.live {
					t.Fatalf("after the retag DEEL_TOKEN is served behind %q; want %q", got, c.live)
				}
			}
			res := b.FromPage(t, "/approve", decision(form, "approve"))
			if !c.moved {
				if res.Status != http.StatusSeeOther || r.grants(t, p.recordID) != 1 {
					t.Fatalf("an approval from the page with no retag since = %d with %d grants; want 303 and one grant:\n%s", res.Status, r.grants(t, p.recordID), res.Body)
				}
				return
			}
			if res.Status != http.StatusForbidden || !strings.Contains(pageText(t, res.Body), "Profile changed") {
				t.Fatalf("an approval from the page printed before the retag = %d, want 403 Profile changed:\n%s", res.Status, res.Body)
			}
			r.requirePending(t, p.recordID)

			b.Decide(t, p.recordID, true)
			if grants := r.grants(t, p.recordID); grants != 1 {
				t.Fatalf("the approval from the page opened again made %d grants; want one", grants)
			}
		})
	}
}
