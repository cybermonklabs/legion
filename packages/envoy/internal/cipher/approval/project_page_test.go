package approval_test

import (
	"crypto/ecdsa"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/logtest"
)

// privProject is the private project of the page's project tests: ada owns it and its access
// list names ben, whose machine every request comes from; carol is on no list.
const privProject = "PRIV"

// projectRig is newRig with a Dispatch stand-in holding PRIV, served to the request machine and
// the enrollment, and two secrets of PRIV beside the rig's four: PRIV_SECRET, ada's and human
// tier, which ada approves; PRIV_SHARED, shared and human tier, which anyone signed in may approve
// by owner and tier. profiles, each one entry's JSON, are the rig's profiles file, none for none.
func projectRig(t *testing.T, profiles ...string) (*rig, *projectaccesstest.Server) {
	t.Helper()
	dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		privProject: {Visibility: "private", Owner: ada, Listed: []string{ben}},
	}, nil)
	tagged := func(name, owner string) secrets.LocalSecret {
		s := policytest.Secret(name, owner, policy.TierHuman, strings.ToLower(name)+"-v1")
		s.Tags[policy.TagProject] = privProject
		return s
	}
	r := newRig(t, func(s *rigSetup) {
		s.secrets = []secrets.LocalSecret{tagged("PRIV_SECRET", ada), tagged("PRIV_SHARED", policy.OwnerShared)}
		s.projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
		if len(profiles) > 0 {
			s.profiles = policytest.ProfilesFile(profiles...)
		}
	})
	return r, dispatch
}

// nameProjects sets the projects enrollmentID's session names, as its enrollment recorded them.
func (r *rig) nameProjects(t *testing.T, enrollmentID string, projects ...string) {
	t.Helper()
	if _, err := r.st.Pool.Exec(t.Context(), `update enrollments set projects=$2 where id=$1`, enrollmentID, projects); err != nil {
		t.Fatalf("name the session's projects: %v", err)
	}
}

// requestSignedElsewhere has the session ask for names with reason in a request object signed for
// another broker address, as when a session asked a broker of another BROKER_PUBLIC_URL that
// shares this database, and answers its record's id: a record this broker does not verify
// (requests.Machine.ReadRecord refuses it) and no list of it shows.
func (r *rig) requestSignedElsewhere(t *testing.T, enrollmentID string, key *ecdsa.PrivateKey, reason string, names ...string) string {
	t.Helper()
	const elsewhere = "https://the-old-public-url.example"
	details := make([]record.AuthorizationDetail, len(names))
	for i, n := range names {
		details[i] = record.AuthorizationDetail{Type: record.KindAgentSecret, Identifier: n, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(key, elsewhere, details, reason, "", nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	current := r.machine.Audience
	r.machine.Audience = elsewhere
	created, err := r.machine.Create(t.Context(), enrollmentID, compact, "")
	r.machine.Audience = current
	if err != nil || created.RecordID == nil || created.State != "pending" {
		t.Fatalf("Create under %s = %+v, %v; want a pending request on a record", elsewhere, created, err)
	}
	return *created.RecordID
}

// TestTheApprovalPageShowsAPrivateSecretsProject: the page of a request for PRIV's secret, from
// a session naming PRIV, says which project the secret belongs to, that it is private and that
// the session names it, and which projects the session names.
func TestTheApprovalPageShowsAPrivateSecretsProject(t *testing.T) {
	r, _ := projectRig(t)
	enrollmentID, key := r.benSession(t, "ben-priv-box")
	r.nameProjects(t, enrollmentID, privProject)
	p := r.request(t, enrollmentID, key, "deploy PRIV", nil, "", "PRIV_SECRET")

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	got := pageText(t, b.Page(t, p.recordID).Body)
	for _, want := range []string{
		"Project of PRIV_SECRET PRIV, private, which the session names",
		"Projects the session names PRIV",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the page of %s does not say %q:\n%s", p.recordID, want, got)
		}
	}
}

// TestAPrivateProjectsProfileIsShownAndHiddenAsItsSource: PRIV_SHARED_READ, a profile of
// PRIV_SHARED under a name of its own, belongs to PRIV as its source does. The page of a request
// for it, from a session naming PRIV, says its project is PRIV, private and named by the session,
// as TestTheApprovalPageShowsAPrivateSecretsProject's page says of PRIV_SECRET; and carol, outside
// PRIV, who may approve the profile by owner and tier, gets the 404 an id no record has, as for
// PRIV_SHARED (TestAnOutsiderSeesNoPrivateRequest).
func TestAPrivateProjectsProfileIsShownAndHiddenAsItsSource(t *testing.T) {
	r, _ := projectRig(t, `{"name": "PRIV_SHARED_READ", "source": "PRIV_SHARED", "proxy": {"hosts": ["api.example.com"]}}`)
	enrollmentID, key := r.benSession(t, "ben-profile-box")
	r.nameProjects(t, enrollmentID, privProject)
	profiled := r.request(t, enrollmentID, key, "rotate the shared key", nil, "", "PRIV_SHARED_READ")
	wikiSession, wikiKey := r.benSession(t, "ben-profile-wiki-box")
	visible := r.request(t, wikiSession, wikiKey, "rotate the wiki key", nil, "", "SHARED_SECRET")

	owner := r.browser(t)
	owner.SignIn(t, profiled.recordID, person(ada))
	if got, want := pageText(t, owner.Page(t, profiled.recordID).Body), "Project of PRIV_SHARED_READ PRIV, private, which the session names"; !strings.Contains(got, want) {
		t.Errorf("the page of %s does not say %q:\n%s", profiled.recordID, want, got)
	}

	outsider := r.browser(t)
	outsider.SignIn(t, visible.recordID, person(carol))
	if res := outsider.Get(t, "/approve/"+profiled.recordID); res.Status != http.StatusNotFound || !strings.Contains(pageText(t, res.Body), "No such request") {
		t.Errorf("carol's page of %s = %d:\n%s\nwant the 404 an id no record has", profiled.recordID, res.Status, pageText(t, res.Body))
	}
}

// TestTheApprovalPageOfAnUntaggedRequestIsUnchanged: a request for a secret of no project, from a
// session naming none, prints no project row, and its page asks Dispatch nothing, though the
// broker holds a client of it. The enrollment itself asks Dispatch the person's member kind
// (enroll.Service.checkProjects), so the count is read once the request exists.
func TestTheApprovalPageOfAnUntaggedRequestIsUnchanged(t *testing.T) {
	r, dispatch := projectRig(t)
	enrollmentID, key := r.benSession(t, "ben-plain-box")
	p := r.request(t, enrollmentID, key, "deploy the billing service", nil, "", "TEST_SECRET")
	before := dispatch.Calls()

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	got := pageText(t, b.Page(t, p.recordID).Body)
	if strings.Contains(got, "Project") {
		t.Errorf("the page of an untagged request prints a project row:\n%s", got)
	}
	if calls := dispatch.Calls() - before; calls != 0 {
		t.Errorf("the page of an untagged request asked Dispatch %d times; want none", calls)
	}
}

// TestAnOutsiderSeesNoPrivateRequest: carol, outside PRIV, may approve PRIV_SHARED by owner and
// tier, yet its page answers her the 404 an id no record has, it is not among the other requests
// waiting for her, a decision's outcome line naming it says nothing of it, and her Approve leaves
// it pending. ben's request for an untagged secret whose reason names a PRIV issue is hidden the
// same way, and so is one signed for another broker address, which the page cannot read: its page
// and an outcome line naming it answered 503 before, since whether she may see it was read from
// its signed request. A request her page printed while it touched no private project, whose
// session then names PRIV, is refused her as "no such record" when she approves it, and her
// decision lands on a page whose address carries no refusal code, as a decision of an id no
// record has does: a code would say the request exists and why it was not decided. A person on
// PRIV's list sees the first two.
func TestAnOutsiderSeesNoPrivateRequest(t *testing.T) {
	r, _ := projectRig(t)
	enrollmentID, key := r.benSession(t, "ben-shared-box")
	r.nameProjects(t, enrollmentID, privProject)
	tagged := r.request(t, enrollmentID, key, "rotate the shared key", nil, "", "PRIV_SHARED")
	plainSession, plainKey := r.benSession(t, "ben-reason-box")
	reasoned := r.request(t, plainSession, plainKey, "PRIV-12: rotate the shared key", nil, "", "SHARED_SECRET")
	staleSession, staleKey := r.benSession(t, "ben-stale-box")
	stale := pending{recordID: r.requestSignedElsewhere(t, staleSession, staleKey, "PRIV-12: rotate the shared key", "SHARED_SECRET")}
	// A session of its own: a second request of one session for the same secret and approver
	// joins the pending one rather than opening a card of its own.
	wikiSession, wikiKey := r.benSession(t, "ben-wiki-box")
	visible := r.request(t, wikiSession, wikiKey, "rotate the wiki key", nil, "", "SHARED_SECRET")

	b := r.browser(t)
	b.SignIn(t, visible.recordID, person(carol))
	for _, hidden := range []pending{tagged, reasoned, stale} {
		if res := b.Get(t, "/approve/"+hidden.recordID); res.Status != http.StatusNotFound || !strings.Contains(pageText(t, res.Body), "No such request") {
			t.Errorf("carol's page of %s = %d:\n%s\nwant the 404 an id no record has", hidden.recordID, res.Status, pageText(t, res.Body))
		}
	}
	page := b.Get(t, "/approve/"+visible.recordID+"?decided="+tagged.recordID+","+reasoned.recordID+","+stale.recordID)
	if page.Status != http.StatusOK {
		t.Fatalf("carol's page of %s = %d", visible.recordID, page.Status)
	}
	for _, hidden := range []pending{tagged, reasoned, stale} {
		if strings.Contains(page.Body, hidden.recordID) {
			t.Errorf("carol's page names %s, a request of PRIV:\n%s", hidden.recordID, page.Body)
		}
	}
	if text := pageText(t, page.Body); strings.Contains(text, "PRIV") {
		t.Errorf("carol's page says PRIV:\n%s", text)
	}
	form := decisionForm(t, page.Body, visible.recordID)
	if form == nil {
		t.Fatalf("carol's page offers no form for %s", visible.recordID)
	}
	form.Set("record", tagged.recordID)
	b.FromPage(t, "/approve", decision(form, "approve"))
	r.requirePending(t, tagged.recordID)

	turnedSession, turnedKey := r.benSession(t, "ben-turned-box")
	turned := r.request(t, turnedSession, turnedKey, "rotate the turned key", nil, "", "SHARED_SECRET")
	turnedForm := decisionForm(t, b.Page(t, turned.recordID).Body, turned.recordID)
	if turnedForm == nil {
		t.Fatalf("carol's page of %s, which touches no private project yet, offers no form", turned.recordID)
	}
	r.nameProjects(t, turnedSession, privProject)
	res := b.FromPage(t, "/approve", decision(turnedForm, "approve"))
	if to := res.Header.Get("Location"); res.Status != http.StatusSeeOther || strings.Contains(to, "&refused=") {
		t.Errorf("carol's Approve of %s once its session names PRIV = %d to %q; want 303 to an address carrying no refusal code", turned.recordID, res.Status, to)
	}
	r.requirePending(t, turned.recordID)

	insider := r.browser(t)
	insider.SignIn(t, tagged.recordID, person(ben))
	for _, shown := range []pending{tagged, reasoned} {
		if res := insider.Get(t, "/approve/"+shown.recordID); res.Status != http.StatusOK {
			t.Errorf("ben's page of %s = %d, want 200: he is on PRIV's list", shown.recordID, res.Status)
		}
	}
}

// TestASignInToAHiddenRequestNamesNoApprover is round 3's third hardening item on
// cybermonklabs/legion#4135: no sign-in to a request of a private project, by its secret's tag or
// by an issue its reason names, carries its approver as login_hint to a person outside it. A
// signed-out visitor's sign-in names none for any record, since the page reads no record before
// a person signs in. The one sign-in that names an approver, ?switch=1 from a live session, is
// answered 404 for a request the person may not see, as for an id no record has, and starts no
// sign-in; for a request touching no private project it names its approver, so the person it
// waits on is asked nothing at a provider they are already signed in to.
func TestASignInToAHiddenRequestNamesNoApprover(t *testing.T) {
	r, _ := projectRig(t)
	enrollmentID, key := r.benSession(t, "ben-hint-box")
	r.nameProjects(t, enrollmentID, privProject)
	tagged := r.request(t, enrollmentID, key, "deploy PRIV", nil, "", "PRIV_SECRET")
	reasonSession, reasonKey := r.benSession(t, "ben-hint-reason-box")
	reasoned := r.request(t, reasonSession, reasonKey, "PRIV-12: deploy the billing service", nil, "", "TEST_SECRET")
	plainSession, plainKey := r.benSession(t, "ben-hint-plain-box")
	plain := r.request(t, plainSession, plainKey, "deploy the billing service", nil, "", "TEST_SECRET")

	for _, p := range []pending{tagged, reasoned, plain} {
		if auth := r.browser(t).StartSignIn(t, "/approve/"+p.recordID); auth.Query().Has("login_hint") {
			t.Errorf("a signed-out sign-in to %s hints %q; want no login_hint for any record", p.recordID, auth.Query().Get("login_hint"))
		}
	}
	outsider := r.browser(t)
	outsider.SignIn(t, plain.recordID, person(carol))
	for _, hidden := range []pending{tagged, reasoned} {
		res := outsider.Get(t, "/approve/"+hidden.recordID+"?switch=1")
		if res.Status != http.StatusNotFound || !strings.Contains(pageText(t, res.Body), "No such request") || res.Header.Get("Location") != "" {
			t.Errorf("carol's switch to %s, a request of PRIV = %d Location %q:\n%s\nwant the 404 an id no record has, and no sign-in", hidden.recordID, res.Status, res.Header.Get("Location"), pageText(t, res.Body))
		}
	}
	if auth := outsider.StartSignIn(t, "/approve/"+plain.recordID+"?switch=1"); auth.Query().Get("login_hint") != ada {
		t.Errorf("carol's switch to %s, a request of no project, hints %q; want its approver %s", plain.recordID, auth.Query().Get("login_hint"), ada)
	}
}

// unansweredNote is what the page says of a request Dispatch cannot answer for: whether the
// person may see it is unknown, so the page shows nothing of it and nothing decides it.
const unansweredNote = "the secrets broker could not ask Dispatch whether you may see this request, so it shows nothing of it and cannot decide it until Dispatch answers. Try again shortly."

// TestADispatchOutageShowsOnlyItsRequestAsUnavailable: nothing is tagged, and ben's request for
// the shared secret names an issue, INFRA-12, beside one that names none. Both are listed for ben,
// the person the session acts for (requests.responsiblePerson), and his page prints both while
// Dispatch answers. Dispatch then goes down and he approves all he was shown: the request naming
// no issue is granted, and the one naming INFRA-12 is decided by nobody and stays pending. The
// page he lands on answers 200 with each outcome, the INFRA-12 one saying only that it is
// unavailable; the INFRA-12 request's own page answers 503 showing that one request as
// unavailable and nothing of it, its reason included. Once Dispatch answers it is shown and
// decidable again.
func TestADispatchOutageShowsOnlyItsRequestAsUnavailable(t *testing.T) {
	up := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{}, nil)
	access := &projectaccesstest.Swappable{}
	access.Serve(up)
	r := newRig(t, func(s *rigSetup) { s.projects = access })
	plainSession, plainKey := r.benSession(t, "ben-plain-box")
	plain := r.request(t, plainSession, plainKey, "rotate the wiki key", nil, "", "SHARED_SECRET")
	keyedSession, keyedKey := r.benSession(t, "ben-keyed-box")
	keyed := r.request(t, keyedSession, keyedKey, "INFRA-12: rotate the deploy key", nil, "", "SHARED_SECRET")

	b := r.browser(t)
	b.SignIn(t, plain.recordID, person(ben))
	all, label := approveAll(t, b.Page(t, plain.recordID).Body)
	if label != "Approve all 2 shown" {
		t.Fatalf("ben's page offers %q; want both requests shown while Dispatch answers", label)
	}
	down := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{}, nil)
	down.SetDown(true)
	access.Serve(down)

	res := b.FromPage(t, "/approve", all)
	if res.Status != http.StatusSeeOther {
		t.Fatalf("approve all while Dispatch is down = %d, want 303:\n%s", res.Status, res.Body)
	}
	if state := r.state(t, plain.recordID); state != "approved" {
		t.Errorf("the request naming no issue = %s, want approved: Dispatch is asked nothing for it", state)
	}
	r.requirePending(t, keyed.recordID)
	landed := b.Get(t, res.Header.Get("Location"))
	text := pageText(t, landed.Body)
	if landed.Status != http.StatusOK || !strings.Contains(text, keyed.recordID+": unavailable: "+unansweredNote) {
		t.Fatalf("the page approve all lands on = %d; want 200 with the INFRA-12 request's outcome %q:\n%s", landed.Status, unansweredNote, text)
	}
	if strings.Contains(text, "INFRA-12") || strings.Contains(text, "rotate the deploy key") {
		t.Errorf("the page shows the reason of a request Dispatch could not answer for:\n%s", text)
	}

	own := b.Get(t, "/approve/"+keyed.recordID)
	text = pageText(t, own.Body)
	if own.Status != http.StatusServiceUnavailable || !strings.Contains(text, "Request "+keyed.recordID) || !strings.Contains(text, "The "+strings.TrimPrefix(unansweredNote, "the ")) {
		t.Fatalf("the INFRA-12 request's page while Dispatch is down = %d; want 503 showing it unavailable:\n%s", own.Status, text)
	}
	if strings.Contains(text, "INFRA-12") || strings.Contains(text, "SHARED_SECRET") || decisionForm(t, own.Body, keyed.recordID) != nil {
		t.Errorf("the INFRA-12 request's page while Dispatch is down shows its names, its reason or a button:\n%s", text)
	}
	if down.Calls() == 0 {
		t.Fatal("the down Dispatch was never asked: the test is blind")
	}

	access.Serve(projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{}, nil))
	if back := b.Page(t, keyed.recordID); back.Status != http.StatusOK || decisionForm(t, back.Body, keyed.recordID) == nil {
		t.Fatalf("the INFRA-12 request's page once Dispatch answers = %d with no button; want it decidable again:\n%s", back.Status, pageText(t, back.Body))
	}
}

// TestADispatchOutageLeavesATaggedRequestUnavailableAndDecidesTheRest: ben's page prints his
// request for PRIV_SHARED, from a session naming PRIV, beside his request for the untagged
// SHARED_SECRET while Dispatch answers. Dispatch then goes down and he approves all he was shown.
// The profile check of the approval (approval.Page.profilesMoved) reads each record's names and
// profiles without asking Dispatch, so the POST answers 303: the untagged request is granted, and
// the PRIV_SHARED one is decided by nobody and stays pending, its outcome saying only that it is
// unavailable, as TestADispatchOutageShowsOnlyItsRequestAsUnavailable's request naming an issue
// does. An outage is no error of the broker's, so no line of the POST is an error line.
func TestADispatchOutageLeavesATaggedRequestUnavailableAndDecidesTheRest(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	up := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		privProject: {Visibility: "private", Owner: ada, Listed: []string{ben}},
	}, nil)
	access := &projectaccesstest.Swappable{}
	access.Serve(up)
	r := newRig(t, func(s *rigSetup) {
		shared := policytest.Secret("PRIV_SHARED", policy.OwnerShared, policy.TierHuman, "priv-shared-not-a-secret")
		shared.Tags[policy.TagProject] = privProject
		s.secrets = []secrets.LocalSecret{shared}
		s.projects = access
	})
	plainSession, plainKey := r.benSession(t, "ben-plain-box")
	plain := r.request(t, plainSession, plainKey, "rotate the wiki key", nil, "", "SHARED_SECRET")
	taggedSession, taggedKey := r.benSession(t, "ben-tagged-box")
	r.nameProjects(t, taggedSession, privProject)
	tagged := r.request(t, taggedSession, taggedKey, "rotate the shared key", nil, "", "PRIV_SHARED")

	b := r.browser(t)
	b.SignIn(t, plain.recordID, person(ben))
	all, label := approveAll(t, b.Page(t, plain.recordID).Body)
	if label != "Approve all 2 shown" {
		t.Fatalf("ben's page offers %q; want both requests shown while Dispatch answers", label)
	}
	down := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{}, nil)
	down.SetDown(true)
	access.Serve(down)
	logged := len(capture.Records())

	res := b.FromPage(t, "/approve", all)
	// Errorf, not Fatalf: a POST that answers otherwise is still checked for its error line below,
	// and the page it lands on is read only when it answered the redirect.
	if res.Status != http.StatusSeeOther {
		t.Errorf("approve all while Dispatch is down = %d, want 303:\n%s", res.Status, pageText(t, res.Body))
	}
	if state := r.state(t, plain.recordID); state != "approved" {
		t.Errorf("the untagged request = %s, want approved: Dispatch is asked nothing for it", state)
	}
	// Read from its rows: the rig's requirePending reads the record through ReadRecord, which asks
	// the down Dispatch for PRIV_SHARED's project.
	if pending, grants := r.count(t, `select count(*) from requests where record_id = $1 and state = 'pending'`, tagged.recordID), r.grants(t, tagged.recordID); pending != 1 || grants != 0 {
		t.Errorf("the PRIV_SHARED request has %d pending request rows and %d grants; want it still pending with none", pending, grants)
	}
	if res.Status == http.StatusSeeOther {
		landed := b.Get(t, res.Header.Get("Location"))
		if text := pageText(t, landed.Body); landed.Status != http.StatusOK || !strings.Contains(text, tagged.recordID+": unavailable: "+unansweredNote) {
			t.Errorf("the page approve all lands on = %d; want 200 with the PRIV_SHARED request's outcome %q:\n%s", landed.Status, unansweredNote, text)
		}
	}
	if down.Calls() == 0 {
		t.Fatal("the down Dispatch was never asked: the test is blind")
	}
	requireNoErrorLine(t, "approve all while Dispatch is down", capture.Records()[logged:])
}
