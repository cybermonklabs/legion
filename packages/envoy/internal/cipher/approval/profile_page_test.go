// packages/envoy/internal/cipher/approval/profile_page_test.go
// What the approval page says about a record that asks for a profile: the scope the person
// approves, as the profiles file gives it and as the record's signed target narrows it.
package approval_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestTheApprovalPagePrintsTheProfilesScope: a record for a GitHub profile prints, beside the
// name it asks for, the profile's source, its permissions, its accounts, its repositories, the
// target the record's signed request narrows it to and the hosts its kind sends the credential to,
// each read from the live policy and the record itself. Each is its own row, so a page that
// leaves one of them out fails that row alone.
func TestTheApprovalPagePrintsTheProfilesScope(t *testing.T) {
	r := newRig(t)
	local := r.machine.Secrets.(*memory.Store)
	local.Put(policytest.TypeSecret("ADA_APP", ada, policy.TierHuman, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)))
	r.machine.Policy = policytest.ProfiledCurrent(t, local, policytest.KindsFile(
		`{"name": "GITHUB_WRITE", "source": "ADA_APP", "github": {"accounts": ["acme", "ada"], "repositories": ["acme/site", "ada/lean", "ada/proofs"], "permissions": {"pull_requests": "write", "contents": "write", "metadata": "read"}}, "proxy": "github"}`,
	))
	enrollmentID, key := r.benSession(t, "ben-box")
	compact, err := record.Sign(key, r.url, []record.AuthorizationDetail{
		{Type: record.KindAgentSecret, Identifier: "GITHUB_WRITE", Actions: []string{"inject"}, Locations: []string{"https://github.com/ada/proofs"}},
	}, "push the proof", "", nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	status, out := r.session(t, key, enrollmentID, http.MethodPost, "/v1/requests", map[string]any{"request": compact})
	created := decode[struct {
		State    string  `json:"state"`
		RecordID *string `json:"record_id"`
	}](t, out)
	if status != http.StatusOK || created.State != "pending" || created.RecordID == nil {
		t.Fatalf("POST /v1/requests for GITHUB_WRITE --target ada/proofs = %d %s, want it pending with a record", status, out)
	}

	b := r.browser(t)
	b.SignIn(t, *created.RecordID, person(ada))
	block := pageText(t, recordBlock(t, b.Page(t, *created.RecordID).Body, *created.RecordID))
	for _, row := range []struct{ field, want string }{
		{"source", "Source ADA_APP"},
		{"permissions", "Permissions contents: write, metadata: read, pull_requests: write"},
		{"accounts", "GitHub accounts acme, ada"},
		{"repositories", "Repositories acme/site, ada/lean, ada/proofs"},
		{"target", "Target ada/proofs"},
		{"proxy hosts", "Proxy hosts api.github.com, github.com, uploads.github.com"},
	} {
		t.Run(row.field, func(t *testing.T) {
			if !strings.Contains(block, row.want) {
				t.Fatalf("the record's block does not print the profile's %s, %q:\n%s", row.field, row.want, block)
			}
		})
	}
}

// TestTheApprovalPagePrintsAGoogleProfilesScopes: a record for a profile of a
// google-service-account source prints the profile's google scopes, which are what its token is
// minted for and so what an approval grants, rather than the wider scopes its source's tag names.
// The source's rules make its owner its one requester, so ben owns it and decides it here.
func TestTheApprovalPagePrintsAGoogleProfilesScopes(t *testing.T) {
	r := newRig(t)
	local := r.machine.Secrets.(*memory.Store)
	source := policytest.TypeSecret("GCP_FLEET", ben, policy.TierHuman, policy.TypeGoogleServiceAccount, policytest.TypeValue(policy.TypeGoogleServiceAccount))
	source.Tags[policy.TagScopes] = "bigquery cloud-platform devstorage.read_only"
	local.Put(source)
	r.machine.Policy = policytest.ProfiledCurrent(t, local, policytest.ProfilesFile(
		`{"name": "GCP_FLEET_READ", "source": "GCP_FLEET", "google": {"scopes": ["devstorage.read_only", "bigquery"]}}`,
	))
	enrollmentID, key := r.benSession(t, "ben-box")
	hour := 3600
	compact, err := record.Sign(key, r.url, []record.AuthorizationDetail{
		{Type: record.KindAgentSecret, Identifier: "GCP_FLEET_READ", Actions: []string{"inject"}},
	}, "read the fleet's tables", "", &hour, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	status, out := r.session(t, key, enrollmentID, http.MethodPost, "/v1/requests", map[string]any{"request": compact})
	created := decode[struct {
		State    string  `json:"state"`
		RecordID *string `json:"record_id"`
	}](t, out)
	if status != http.StatusOK || created.State != "pending" || created.RecordID == nil {
		t.Fatalf("POST /v1/requests for GCP_FLEET_READ = %d %s, want it pending with a record", status, out)
	}

	b := r.browser(t)
	b.SignIn(t, *created.RecordID, person(ben))
	block := pageText(t, recordBlock(t, b.Page(t, *created.RecordID).Body, *created.RecordID))
	for _, row := range []struct{ field, want string }{
		{"source", "Source GCP_FLEET"},
		{"google scopes", "Google scopes bigquery, devstorage.read_only"},
	} {
		t.Run(row.field, func(t *testing.T) {
			if !strings.Contains(block, row.want) {
				t.Fatalf("the record's block does not print the profile's %s, %q:\n%s", row.field, row.want, block)
			}
		})
	}
	if strings.Contains(block, "Proxy hosts") {
		t.Errorf("the block of a profile behind no kind prints proxy hosts:\n%s", block)
	}
}

// TestTheApprovalPagePrintsTheHostsANarrowedProfileReaches: a profile whose own allow names a
// host in every row sends its credential to those hosts alone, whatever else its kind binds, so
// the page prints those, as the person approving it must read them: an administrator's token of
// the github kind narrowed to api.github.com prints api.github.com and not github.com or
// uploads.github.com. Control: the same profile with one row that names no host reaches every host
// of its kind, and prints them all.
func TestTheApprovalPagePrintsTheHostsANarrowedProfileReaches(t *testing.T) {
	const rows = `{"method": "GET", "pathPrefix": "/enterprises/acme/audit-log/stream-key", "host": "api.github.com"}, ` +
		`{"method": "PUT", "pathPrefix": "/enterprises/acme/actions/cache/storage-limit", "host": "api.github.com"}`
	for _, c := range []struct {
		name, allow, want string
	}{
		{"every row on api.github.com", rows, "Proxy hosts api.github.com"},
		{"the control: one row on no host", rows + `, {"method": "GET", "pathPrefix": "/enterprises/acme/settings"}`, "Proxy hosts api.github.com, github.com, uploads.github.com"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			local := r.machine.Secrets.(*memory.Store)
			local.Put(policytest.Narrowed(policytest.Proxied(policytest.Secret("ENTERPRISE_ADMIN", ada, policy.TierHuman, "enterprise-v1"), "github")))
			r.machine.Policy = policytest.ProfiledCurrent(t, local, policytest.KindsFile(`{"name": "ENTERPRISE_ADMIN", "allow": [`+c.allow+`]}`))
			enrollmentID, key := r.benSession(t, "ben-box")
			p := r.request(t, enrollmentID, key, "raise the cache limit", nil, "", "ENTERPRISE_ADMIN")
			b := r.browser(t)
			b.SignIn(t, p.recordID, person(ada))
			block := pageText(t, recordBlock(t, b.Page(t, p.recordID).Body, p.recordID))
			if !strings.Contains(block, c.want) {
				t.Fatalf("the record's block does not print %q:\n%s", c.want, block)
			}
			if c.want == "Proxy hosts api.github.com" && (strings.Contains(block, "uploads.github.com") || strings.Contains(block, "api.github.com, ")) {
				t.Errorf("the block of a profile narrowed to api.github.com prints another host of its kind:\n%s", block)
			}
		})
	}
}

// TestAnApprovalIsBoundToTheProfileItsPagePrinted: a page's decision forms carry the fingerprint
// of each profile the page printed, and an approval is refused, 403 PROFILE_CHANGED with nothing
// decided, when a profile's live fingerprint is another: a profiles roll between the page and the
// click must not grant a scope the person did not read. The page opened again prints the profile
// as it is now, with its fingerprint, and that page's approval grants.
func TestAnApprovalIsBoundToTheProfileItsPagePrinted(t *testing.T) {
	r := newRig(t)
	local := r.machine.Secrets.(*memory.Store)
	local.Put(policytest.TypeSecret("ADA_APP", ada, policy.TierHuman, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)))
	read := policytest.ProfilesFile(`{"name": "GITHUB_WRITE", "source": "ADA_APP", "github": {"accounts": ["ada"], "permissions": {"contents": "read", "metadata": "read"}}}`)
	write := policytest.ProfilesFile(`{"name": "GITHUB_WRITE", "source": "ADA_APP", "github": {"accounts": ["ada"], "permissions": {"contents": "write", "metadata": "read"}}}`)
	r.machine.Policy = policytest.ProfiledCurrent(t, local, read)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "read the proofs", nil, "", "GITHUB_WRITE")

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
	if form == nil {
		t.Fatalf("the page for %s offers no decision", p.recordID)
	}
	printed := p.recordID + " GITHUB_WRITE " + policytest.Profiles(t, read).Digest("GITHUB_WRITE")
	if got := form["profile_digest"]; len(got) != 1 || got[0] != printed {
		t.Fatalf("the decision form carries the profile fingerprints %q; want [%q], the one profile the page printed", got, printed)
	}

	r.machine.Policy = policytest.ProfiledCurrent(t, local, write)
	if res := b.FromPage(t, "/approve", decision(form, "approve")); res.Status != http.StatusForbidden || !strings.Contains(pageText(t, res.Body), "Profile changed") {
		t.Fatalf("an approval from the page printed before the profile changed = %d, want 403 Profile changed:\n%s", res.Status, res.Body)
	}
	r.requirePending(t, p.recordID)

	b.Decide(t, p.recordID, true)
	if grants := r.grants(t, p.recordID); grants != 1 {
		t.Fatalf("the approval from the page opened again made %d grants; want one", grants)
	}
}

// TestAnApprovalWhoseTargetARollTookOutIsARefusal: a profiles roll can take a pending request's
// target out of its profile before the request's page is opened. The page prints the live profile
// and its fingerprint, so the approval passes profilesMoved, and the approval's own target check
// refuses it (requests.ErrTargetRefused). That is the ordinary outcome of the roll, as the judge
// reads the same refusal, so the page decides nothing and logs the approval refused in the
// broker's own words, as it logs every other refusal of one, and writes no error line. The
// approval lands on the request's page with the refusal's policy code, and that page says the
// current policy no longer lets the person approve it.
func TestAnApprovalWhoseTargetARollTookOutIsARefusal(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	r := newRig(t)
	local := r.machine.Secrets.(*memory.Store)
	local.Put(policytest.TypeSecret("ADA_APP", ada, policy.TierHuman, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)))
	const block = `"permissions": {"contents": "read", "metadata": "read"}`
	before := policytest.ProfilesFile(`{"name": "GITHUB_READ", "source": "ADA_APP", "github": {"accounts": ["ada"], "repositories": ["ada/lean", "ada/proofs"], ` + block + `}}`)
	after := policytest.ProfilesFile(`{"name": "GITHUB_READ", "source": "ADA_APP", "github": {"accounts": ["ada"], "repositories": ["ada/lean"], ` + block + `}}`)
	r.machine.Policy = policytest.ProfiledCurrent(t, local, before)
	enrollmentID, key := r.benSession(t, "ben-box")
	compact, err := record.Sign(key, r.url, []record.AuthorizationDetail{
		{Type: record.KindAgentSecret, Identifier: "GITHUB_READ", Actions: []string{"inject"}, Locations: []string{"https://github.com/ada/proofs"}},
	}, "read the proofs", "", nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	status, out := r.session(t, key, enrollmentID, http.MethodPost, "/v1/requests", map[string]any{"request": compact})
	created := decode[struct {
		State    string  `json:"state"`
		RecordID *string `json:"record_id"`
	}](t, out)
	if status != http.StatusOK || created.State != "pending" || created.RecordID == nil {
		t.Fatalf("POST /v1/requests for GITHUB_READ --target ada/proofs = %d %s, want it pending with a record", status, out)
	}
	recordID := *created.RecordID

	r.machine.Policy = policytest.ProfiledCurrent(t, local, after)
	b := r.browser(t)
	b.SignIn(t, recordID, person(ada))
	form := decisionForm(t, b.Page(t, recordID).Body, recordID)
	if form == nil {
		t.Fatalf("the page for %s offers no decision", recordID)
	}
	res := b.FromPage(t, "/approve", decision(form, "approve"))
	if res.Status != http.StatusSeeOther {
		t.Fatalf("the approval of a target the rolled profile does not hold = %d, want 303 back to the record:\n%s", res.Status, res.Body)
	}
	r.requirePending(t, recordID)
	to := res.Header.Get("Location")
	if want := "&refused=" + recordID + ":policy"; !strings.Contains(to, want) {
		t.Errorf("the refused approval redirects to %q; want it to carry %q", to, want)
	}
	if landed := pageText(t, b.Get(t, to).Body); !strings.Contains(landed, "GITHUB_READ: still pending; it was not decided: the current policy no longer lets you approve it.") {
		t.Errorf("the page the refused approval lands on does not name the policy:\n%s", landed)
	}

	requireNoErrorLine(t, "the refused approval", capture.Records())
	var outcomes []any
	for _, logged := range capture.Records() {
		if fields := logFields(logged); logged.Message == "approval page decision" && fields["record"] == recordID {
			outcomes = append(outcomes, fields["outcome"])
		}
	}
	if len(outcomes) != 1 {
		t.Fatalf("the approval logged the outcomes %v; want one", outcomes)
	}
	outcome, _ := outcomes[0].(string)
	if !strings.HasPrefix(outcome, "refused: ") || !strings.Contains(outcome, "ada/proofs") {
		t.Fatalf("the approval's outcome = %q; want it refused, naming the target ada/proofs", outcome)
	}
}

// TestARestrictedMembersApprovalOfAProfileIsRefusedBeforeAnyProfileCheck: a person Dispatch calls
// a restricted member is no approver of a profile's request either, and is told so before the
// page's profile check (profilesMoved) says anything of the profile. ada owns ADA_APP, so she
// decides ben's request for GITHUB_READ, a profile of it under a name of its own. Her page prints
// the request while Dispatch holds no row for her, and Dispatch then calls her a restricted
// member. Her approval from that page is refused as a restricted member's, nothing decided, the
// address it lands on carrying the code restricted and that page showing her nothing of the
// request: while the profile is as the page printed it, and after a profiles roll changed it,
// where 403 PROFILE_CHANGED would tell her that it changed. The control: once Dispatch holds no
// row for her again, the same form is refused PROFILE_CHANGED, and the page opened again grants.
func TestARestrictedMembersApprovalOfAProfileIsRefusedBeforeAnyProfileCheck(t *testing.T) {
	access := &projectaccesstest.Swappable{}
	dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{}, nil)
	access.Serve(dispatch)
	r := newRig(t, func(s *rigSetup) { s.projects = access })
	local := r.machine.Secrets.(*memory.Store)
	local.Put(policytest.TypeSecret("ADA_APP", ada, policy.TierHuman, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)))
	read := policytest.ProfilesFile(`{"name": "GITHUB_READ", "source": "ADA_APP", "github": {"accounts": ["ada"], "permissions": {"contents": "read", "metadata": "read"}}}`)
	write := policytest.ProfilesFile(`{"name": "GITHUB_READ", "source": "ADA_APP", "github": {"accounts": ["ada"], "permissions": {"contents": "write", "metadata": "read"}}}`)
	r.machine.Policy = policytest.ProfiledCurrent(t, local, read)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "read the proofs", nil, "", "GITHUB_READ")

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
	if form == nil {
		t.Fatalf("%s's page for %s offers no decision while Dispatch holds no row for her", ada, p.recordID)
	}
	dispatch.SetMemberKind(ada, projectaccess.MemberRestricted)
	access.Serve(dispatch)

	for _, profile := range []struct{ name, file string }{
		{"as the page printed it", read},
		{"after a roll changed it", write},
	} {
		r.machine.Policy = policytest.ProfiledCurrent(t, local, profile.file)
		res := b.FromPage(t, "/approve", decision(form, "approve"))
		to := res.Header.Get("Location")
		if want := "&refused=" + p.recordID + ":restricted"; res.Status != http.StatusSeeOther || !strings.Contains(to, want) {
			t.Fatalf("%s's approval as a restricted member, the profile %s = %d to %q; want 303 carrying %q:\n%s", ada, profile.name, res.Status, to, want, pageText(t, res.Body))
		}
		r.requirePending(t, p.recordID)
		landed := b.Get(t, to)
		if text := pageText(t, landed.Body); landed.Status != http.StatusForbidden || !strings.Contains(text, restrictedNote) || strings.Contains(text, "GITHUB_READ") {
			t.Fatalf("the page her approval lands on, the profile %s = %d; want 403 saying %q, and nothing of the request:\n%s", profile.name, landed.Status, restrictedNote, text)
		}
	}

	dispatch.SetMemberKind(ada, projectaccess.MemberNone)
	access.Serve(dispatch)
	if res := b.FromPage(t, "/approve", decision(form, "approve")); res.Status != http.StatusForbidden || !strings.Contains(pageText(t, res.Body), "Profile changed") {
		t.Fatalf("her approval from the page printed before the roll, once Dispatch holds no row for her = %d; want 403 Profile changed:\n%s", res.Status, pageText(t, res.Body))
	}
	r.requirePending(t, p.recordID)
	b.Decide(t, p.recordID, true)
	if grants := r.grants(t, p.recordID); grants != 1 {
		t.Fatalf("her approval from the page opened again made %d grants; want one", grants)
	}
}
