package approval_test

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
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
	tagged := func(name, owner string) awstest.LocalSecret {
		s := policytest.Secret(name, owner, policy.TierHuman, strings.ToLower(name)+"-v1")
		s.Tags[policy.TagProject] = privProject
		return s
	}
	r := newRig(t, func(s *rigSetup) {
		s.secrets = []awstest.LocalSecret{tagged("PRIV_SECRET", ada), tagged("PRIV_SHARED", policy.OwnerShared)}
		s.projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
		if len(profiles) > 0 {
			s.profiles = policytest.KindsFile(profiles...)
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
	r, _ := projectRig(t, `{"name": "PRIV_SHARED_READ", "source": "PRIV_SHARED", "proxy": "example"}`)
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

// TestTheApprovalPageOfAnUntaggedRequestAsksOnlyThePersonsMemberKind: a request for a secret of no
// project, from a session naming none, prints no project row, and its page asks Dispatch one thing,
// though the broker holds a client of it: what kind of member the signed-in person is. The page
// shows a person Dispatch calls a restricted member no secret request at all (Page.ServeRecord),
// and the list of the other secret requests waiting on the person asks the same
// (requests.Machine.PendingForApprover), which the client answers from that one read. It asks
// nothing of a project, a tag or a session. The enrollment itself asks Dispatch the person's member
// kind (enroll.Service.checkProjects), so the count is read once the request exists.
func TestTheApprovalPageOfAnUntaggedRequestAsksOnlyThePersonsMemberKind(t *testing.T) {
	r, dispatch := projectRig(t)
	enrollmentID, key := r.benSession(t, "ben-plain-box")
	p := r.request(t, enrollmentID, key, "deploy the billing service", nil, "", "TEST_SECRET")
	before, projectsBefore := dispatch.Calls(), dispatch.ProjectCalls()

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	got := pageText(t, b.Page(t, p.recordID).Body)
	if strings.Contains(got, "Project") {
		t.Errorf("the page of an untagged request prints a project row:\n%s", got)
	}
	if calls := dispatch.Calls() - before; calls != 1 {
		t.Errorf("the page of an untagged request asked Dispatch %d times; want once, the signed-in person's member kind", calls)
	}
	if calls := dispatch.ProjectCalls() - projectsBefore; calls != 0 {
		t.Errorf("the page of an untagged request asked Dispatch's project route %d times; want none", calls)
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
// Dispatch answers. Dispatch then cannot answer for a project, its person route still answering,
// and he approves all he was shown: the request naming no issue is granted, and the one naming
// INFRA-12 is decided by nobody and stays pending. The page he lands on answers 200 with each
// outcome, the INFRA-12 one saying only that it is unavailable; the INFRA-12 request's own page
// answers 503 showing that one request as unavailable and nothing of it, its reason included.
// Once Dispatch answers it is shown and decidable again. (While the person route fails too, an
// approval of either is refused, since it asks what kind of member the session's person is.)
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
	down.SetProjectsDown(true)
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
// SHARED_SECRET while Dispatch answers. Dispatch's project routes then go down, its person route
// still answering ben a full member, and he approves all he was shown. The profile check of the
// approval (approval.Page.profilesMoved) reads each record's names and profiles without asking
// Dispatch, so the POST answers 303: the untagged request is granted, and the PRIV_SHARED one is
// decided by nobody and stays pending, its outcome saying only that it is unavailable, as
// TestADispatchOutageShowsOnlyItsRequestAsUnavailable's request naming an issue does. An outage
// is no error of the broker's, so no line of the POST is an error line. An outage of the person
// route too leaves even the untagged request pending, since an approval asks what kind of member
// the session's person is (TestAnApprovalWhosePersonDispatchCannotReadIsUnavailable).
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
		s.secrets = []awstest.LocalSecret{shared}
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
	down.SetProjectsDown(true)
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

// TestAnApprovalWhosePersonDispatchCannotReadIsUnavailable: ben's page prints his request, and
// then Dispatch stops answering altogether, its person route included. His approval is refused,
// since the broker cannot learn whether ben is a restricted member, and the request stays
// pending; the address it lands on carries the refusal's code, error. The page there answers 503:
// it cannot learn whether he may decide secret requests either, so it shows him none and says to
// try again shortly. The broker writes no ERROR line: Dispatch not answering is no fault of the
// page's. Once Dispatch answers, the same address says the request is still pending because the
// broker could not decide it.
func TestAnApprovalWhosePersonDispatchCannotReadIsUnavailable(t *testing.T) {
	access := &projectaccesstest.Swappable{}
	up := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{}, nil)
	access.Serve(up)
	r := newRig(t, func(s *rigSetup) { s.projects = access })
	session, key := r.benSession(t, "ben-member-kind-box")
	req := r.request(t, session, key, "rotate the wiki key", nil, "", "SHARED_SECRET")
	b := r.browser(t)
	b.SignIn(t, req.recordID, person(ben))
	form := decisionForm(t, b.Page(t, req.recordID).Body, req.recordID)
	if form == nil {
		t.Fatalf("ben's page for %s offers no decision while Dispatch answers", req.recordID)
	}
	down := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{}, nil)
	down.SetDown(true)
	access.Serve(down)
	var records logtest.Recorder
	logtest.SetDefault(t, &records)

	res := b.FromPage(t, "/approve", decision(form, "approve"))
	to := res.Header.Get("Location")
	if want := "&refused=" + req.recordID + ":error"; res.Status != http.StatusSeeOther || !strings.Contains(to, want) {
		t.Fatalf("approve while Dispatch is down = %d to %q, want 303 carrying %q:\n%s", res.Status, to, want, res.Body)
	}
	r.requirePending(t, req.recordID)
	landed := b.Get(t, to)
	if text := pageText(t, landed.Body); landed.Status != http.StatusServiceUnavailable || !strings.Contains(text, kindUnreadNote) || strings.Contains(text, "SHARED_SECRET") {
		t.Fatalf("the page the approval lands on = %d; want 503 saying %q, and nothing of the request:\n%s", landed.Status, kindUnreadNote, text)
	}
	for _, rec := range records.Records() {
		if rec.Level >= slog.LevelError {
			t.Errorf("the approval wrote the ERROR line %q; want none: Dispatch not answering is no fault of the page's", rec.Message)
		}
	}
	if down.Calls() == 0 {
		t.Fatal("the down Dispatch was never asked: the test is blind")
	}

	access.Serve(up)
	const couldNotDecide = "SHARED_SECRET: still pending; it was not decided: the secrets broker could not decide it. Try again shortly."
	if back := b.Get(t, to); back.Status != http.StatusOK || !strings.Contains(pageText(t, back.Body), couldNotDecide) {
		t.Fatalf("the address the approval landed on, once Dispatch answers = %d; want 200 saying %q:\n%s", back.Status, couldNotDecide, pageText(t, back.Body))
	}
}

// stalledKinds is a projectaccess.Access whose member-kind reads, once stall is set, Dispatch never
// answers: each waits until its caller's context ends, and records how long that context had left.
// A read whose context has no deadline would wait as long as its caller, so it is answered at once
// with an error and counted as unbounded, which the row reads as the fault. Until stall is set,
// and for every other question, it answers as answers does.
type stalledKinds struct {
	answers   projectaccess.Access
	stall     atomic.Bool
	mu        sync.Mutex
	unbounded int
	bounds    []time.Duration
}

func (s *stalledKinds) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	return s.answers.Access(ctx, project, person)
}

func (s *stalledKinds) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return s.answers.States(ctx, projects)
}

func (s *stalledKinds) MemberKind(ctx context.Context, person string) (string, error) {
	if !s.stall.Load() {
		return s.answers.MemberKind(ctx, person)
	}
	deadline, bounded := ctx.Deadline()
	s.mu.Lock()
	if !bounded {
		s.unbounded++
		s.mu.Unlock()
		return "", errors.New("stalledKinds: a member-kind read with no deadline")
	}
	s.bounds = append(s.bounds, time.Until(deadline))
	s.mu.Unlock()
	<-ctx.Done()
	return "", ctx.Err()
}

// TestTheApprovalPageReadsTheMemberKindWithinItsBound: the page reads the signed-in person's
// member kind before it reads any record, and one call to Dispatch may take longer than a person
// waits on a page (projectaccess's requestTimeout), so that read has a bound of its own,
// projectaccess.RelayedReadBound. ben's page prints his request while Dispatch answers; once
// Dispatch stops answering his member-kind read, the page answers 503 saying it could not ask,
// and every member-kind read it made had no more than that bound to run.
func TestTheApprovalPageReadsTheMemberKindWithinItsBound(t *testing.T) {
	stand := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{}, nil)
	stalled := &stalledKinds{answers: projectaccess.New(stand.URL, stand.Account, nil)}
	r := newRig(t, func(s *rigSetup) { s.projects = stalled })
	session, key := r.benSession(t, "ben-stalled-box")
	req := r.request(t, session, key, "rotate the wiki key", nil, "", "SHARED_SECRET")
	b := r.browser(t)
	b.SignIn(t, req.recordID, person(ben))
	b.Page(t, req.recordID)
	stalled.stall.Store(true)

	got := b.Get(t, "/approve/"+req.recordID)
	if text := pageText(t, got.Body); got.Status != http.StatusServiceUnavailable || !strings.Contains(text, kindUnreadNote) {
		t.Fatalf("ben's page while Dispatch does not answer = %d; want 503 saying %q:\n%s", got.Status, kindUnreadNote, text)
	}
	stalled.mu.Lock()
	defer stalled.mu.Unlock()
	if stalled.unbounded != 0 || len(stalled.bounds) == 0 {
		t.Fatalf("the page asked %d member-kind reads with no deadline and %d with one; want at least one, each bounded", stalled.unbounded, len(stalled.bounds))
	}
	for _, bound := range stalled.bounds {
		if bound > projectaccess.RelayedReadBound {
			t.Fatalf("the page read a member kind with %s to run; want no more than %s", bound, projectaccess.RelayedReadBound)
		}
	}
}

// The page's own words for a person it shows no secret request: one Dispatch calls a restricted
// member, and one whose member kind it cannot learn.
const (
	restrictedNote = "A person Dispatch calls a restricted member is no approver of a secret, and Dispatch calls you one, so this page shows you no secret request."
	kindUnreadNote = "The secrets broker could not ask Dispatch whether you may decide secret requests, so this page shows you none, nor what became of a decision you made. Try again shortly."
)

// TestTheApprovalPageShowsARestrictedMemberNothing: the page signs in only the deployment's
// domain, and does not rest on that alone: a person Dispatch calls a restricted member decides no
// secret request (requests.ErrRestrictedDecider), so the page shows them none, and refuses their
// decision saying why. Here Dispatch held no row for ada when her page printed the request for
// TEST_SECRET, her own secret, and calls her a restricted member before she approves it. Her
// approval is refused, the request stays pending, and the address it lands on carries the
// refusal's code, restricted. That page then answers her 403 NOT_APPROVER saying Dispatch calls her
// a restricted member, and prints nothing of the request, nor offers a decision; the page of an id
// no record has answers her the same, since it reads no record before it refuses. While Dispatch
// answers a kind this broker does not know, the page answers 503, saying so, and prints nothing of
// the request either. The control: once Dispatch holds no row for her again, the same address
// prints the request, its outcome line saying why it is still pending, and its decision.
func TestTheApprovalPageShowsARestrictedMemberNothing(t *testing.T) {
	access := &projectaccesstest.Swappable{}
	dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{}, nil)
	access.Serve(dispatch)
	r := newRig(t, func(s *rigSetup) { s.projects = access })
	session, key := r.benSession(t, "ben-restricted-box")
	p := r.request(t, session, key, "deploy the billing service", nil, "", "TEST_SECRET")
	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
	if form == nil {
		t.Fatalf("%s's page for %s offers no decision while Dispatch holds no row for her", ada, p.recordID)
	}
	dispatch.SetMemberKind(ada, projectaccess.MemberRestricted)
	access.Serve(dispatch)

	res := b.FromPage(t, "/approve", decision(form, "approve"))
	to := res.Header.Get("Location")
	if want := "&refused=" + p.recordID + ":restricted"; res.Status != http.StatusSeeOther || !strings.Contains(to, want) {
		t.Fatalf("%s's Approve as a restricted member = %d to %q; want 303 carrying %q", ada, res.Status, to, want)
	}
	r.requirePending(t, p.recordID)
	shows := func(t *testing.T, what, path string, status int, note string) {
		t.Helper()
		got := b.Get(t, path)
		text := pageText(t, got.Body)
		if got.Status != status || !strings.Contains(text, note) || strings.Contains(text, "TEST_SECRET") || decisionForm(t, got.Body, p.recordID) != nil {
			t.Fatalf("%s = %d; want %d saying %q, and nothing of the request:\n%s", what, got.Status, status, note, text)
		}
	}
	shows(t, "the page her approval lands on", to, http.StatusForbidden, restrictedNote)
	shows(t, "the request's page", "/approve/"+p.recordID, http.StatusForbidden, restrictedNote)
	shows(t, "the page of an id no record has", "/approve/"+strings.Repeat("a", 64), http.StatusForbidden, restrictedNote)

	dispatch.SetMemberKind(ada, "guest-of-honour")
	access.Serve(dispatch)
	shows(t, "the request's page while her kind cannot be read", "/approve/"+p.recordID, http.StatusServiceUnavailable, kindUnreadNote)

	dispatch.SetMemberKind(ada, projectaccess.MemberNone)
	access.Serve(dispatch)
	landed := b.Get(t, to)
	const why = "TEST_SECRET: still pending; it was not decided: Dispatch called you a restricted member, who decides no secret request."
	if text := pageText(t, landed.Body); landed.Status != http.StatusOK || !strings.Contains(text, why) || decisionForm(t, landed.Body, p.recordID) == nil {
		t.Fatalf("the address her approval landed on, once Dispatch holds no row for her = %d; want 200 saying %q, and the decision:\n%s", landed.Status, why, text)
	}
}
