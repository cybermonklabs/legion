package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// The projects and people of the project tests. PRIV is private, owned by otherPerson (alice),
// and its access list names the fixture's operator and insider; TEAM starts public and is the
// project a test turns private or drops a person from; GONE is a key Dispatch holds no project
// for; outsider is on no list.
const (
	privProject = "PRIV"
	teamProject = "TEAM"
	goneProject = "GONE"
	insider     = "carol@example.com"
	outsider    = "mallory@example.com"
)

// taggedSecret is a namespace secret tagged with project.
func taggedSecret(name, owner, tier, project, value string) secrets.LocalSecret {
	s := policytest.Secret(name, owner, tier, value)
	s.Tags[policy.TagProject] = project
	return s
}

// projectSecrets are the tagged secrets every project test's broker serves beside
// fixtureSecrets: PRIV_KEY, shared and agent tier, which owner and tier alone would give every
// session at once; PRIV_HUMAN, shared and human tier, which anyone may approve; TEAM_TOKEN, the
// operator's own agent-tier secret of TEAM; GONE_KEY, tagged with a project Dispatch does not
// hold.
func projectSecrets() []secrets.LocalSecret {
	slack := policytest.Secret("SLACK_TOKEN", policy.OwnerShared, policy.TierAgent, "the-real-slack-bot-value")
	slack.Tags[policy.TagProjectChannels] = policy.ProjectChannelsSlack
	return []secrets.LocalSecret{
		taggedSecret("PRIV_KEY", policy.OwnerShared, policy.TierAgent, privProject, "priv-v1"),
		taggedSecret("PRIV_HUMAN", policy.OwnerShared, policy.TierHuman, privProject, "priv-human-v1"),
		taggedSecret("TEAM_TOKEN", fixtureOperator, policy.TierAgent, teamProject, "team-v1"),
		taggedSecret("GONE_KEY", policy.OwnerShared, policy.TierAgent, goneProject, "gone-v1"),
		// The Legion Slack bot token as #t-d-12 tags it: no project of its own, and a limit a
		// private project's credential carries to the proxy (slack_channels_test.go).
		slack,
	}
}

// dispatchRows is what the stand-in Dispatch holds: PRIV private with its list, and TEAM as the
// test sets it.
func dispatchRows(team projectaccesstest.Row) map[string]projectaccesstest.Row {
	return map[string]projectaccesstest.Row{
		privProject: {Visibility: "private", Owner: otherPerson, Listed: []string{fixtureOperator, insider}},
		teamProject: team,
	}
}

// publicTeam is TEAM while it is public; privateTeam is TEAM private, its list naming listed.
var publicTeam = projectaccesstest.Row{Visibility: "public", Owner: otherPerson}

func privateTeam(listed ...string) projectaccesstest.Row {
	return projectaccesstest.Row{Visibility: "private", Owner: otherPerson, Listed: listed}
}

// everyonesPod is a pod of a service account no operator's entry names a person for, with no
// claim and no service: its requests for a shared secret name no responsible person
// (responsiblePerson), so they wait on every signed-in person's list, as a Legion pod's do whose
// issue nobody is assigned. A shared secret's request from the fixture's own session names its
// operator, whose list alone would hold it. On a list everyone reads, the project rule is what
// must hide a private project's rows from the people outside it, so the tests of that rule make
// their requests here. project, when set, is the project an operator's exchange entry maps the
// pod's account to (Exchange.MappedProjects), which its credential then names; m's exchange set
// is replaced by that one entry.
func everyonesPod(t *testing.T, m *Machine, name, project string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	subject := "system:serviceaccount:example:" + name
	if project != "" {
		set, err := exchange.NewSet([]exchange.AccountEntry{{Account: subject, Project: project, Person: insider}}, nil, nil, nil, "")
		if err != nil {
			t.Fatalf("exchange.NewSet: %v", err)
		}
		m.Exchange = set
	}
	return insertEnrollment(t, m.Store, "pod", name, nil, nil, &subject)
}

// newProjectFixture is newPlaceholderFixture on a broker that checks projects: its loader serves
// projectSecrets (Loader.ProjectChecks), rules are in force, and Dispatch answers rows. The
// enrollment is a session of the fixture's operator naming no project.
func newProjectFixture(t *testing.T, rules proxyrules.Rules, team projectaccesstest.Row) (m *Machine, enrollmentID string, key *ecdsa.PrivateKey, dispatch *projectaccesstest.Server) {
	t.Helper()
	st := storetest.Open(t)
	local := secrets.NewLocal(append(fixtureSecrets(), projectSecrets()...)...)
	enrollmentID, key = newEnrollment(t, st, "session", "box-p-"+t.Name(), new(fixtureOperator), nil)
	loader := policytest.Loader(local, fixtureService)
	loader.ProjectChecks = true
	loader.AirlockDigest = rules.Digest
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}
	m = &Machine{
		Store:             st,
		Policy:            current,
		Secrets:           policytest.Store(local),
		PendingTTL:        12 * time.Hour,
		Audience:          testAudience,
		Skew:              time.Minute,
		Replay:            replayer(st),
		ServicesByAccount: map[string]string{fixtureSubject: fixtureService},
	}
	m.Chain = NewChainVerifier(st, testAudience, time.Minute, m.Airlock)
	m.Airlock, m.AirlockProxyURL, m.AirlockCACertificate = rules, testProxyURL, testCACertificate
	return m, enrollmentID, key, answerFrom(t, m, dispatchRows(team))
}

// answerFrom points m at a fresh Dispatch stand-in serving rows, through a fresh client, so no
// answer an earlier stand-in gave stands in for it: what a running broker reads once its
// client's cache (projectaccess.CacheTTL) has let the earlier answer go.
func answerFrom(t *testing.T, m *Machine, rows map[string]projectaccesstest.Row) *projectaccesstest.Server {
	t.Helper()
	dispatch := projectaccesstest.NewServer(t, rows, nil)
	m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	return dispatch
}

// nameProjects sets the projects enrollmentID's session names, as its enrollment recorded them.
func nameProjects(t *testing.T, m *Machine, enrollmentID string, projects ...string) {
	t.Helper()
	if projects == nil {
		projects = []string{}
	}
	if _, err := m.Store.Pool.Exec(t.Context(), `update enrollments set projects=$2 where id=$1`, enrollmentID, projects); err != nil {
		t.Fatalf("name the session's projects: %v", err)
	}
}

// deniedFor fails t unless request was denied with a detail naming reason for name.
func deniedFor(t *testing.T, what string, request Request, err error, name, reason string) {
	t.Helper()
	if err != nil || request.State != "denied" || request.Detail == nil || !strings.Contains(*request.Detail, name+": "+reason) {
		detail := "<none>"
		if request.Detail != nil {
			detail = *request.Detail
		}
		t.Fatalf("%s = state %q detail %q, %v; want denied naming %s: %s", what, request.State, detail, err, name, reason)
	}
}

// TestARequestForAPrivateSecretFromASessionNamingNoProjectIsDenied: a session whose person is on
// PRIV's list but whose enrollment names no project is denied PRIV's secret project-not-named,
// though owner and tier would give it at once; a secret tagged with a project Dispatch does not
// hold is denied with the same words, so the session cannot tell the two apart.
func TestARequestForAPrivateSecretFromASessionNamingNoProjectIsDenied(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read it", "PRIV_KEY"), "", nil)
	deniedFor(t, "a request for PRIV_KEY from a session naming no project", request, err, "PRIV_KEY", policy.ReasonProjectNotNamed)
	request, err = m.Create(t.Context(), enr, signRequest(t, m, key, "read it", "GONE_KEY"), "", nil)
	deniedFor(t, "a request for GONE_KEY, tagged with a project Dispatch does not hold", request, err, "GONE_KEY", policy.ReasonProjectNotNamed)
	if strings.Contains(*request.Detail, policy.ReasonProjectMissing) {
		t.Fatalf("the session was told %q; want project-not-named alone, so a refusal never says whether a project exists", *request.Detail)
	}
}

// TestASessionNamingThePrivateProjectGetsItsSecret: the same session, its enrollment naming PRIV,
// gets PRIV_KEY at once and its value on release.
func TestASessionNamingThePrivateProjectGetsItsSecret(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	nameProjects(t, m, enr, privProject)
	_, grantID := grantOf(t, m, enr, key, "read it", "PRIV_KEY")
	release, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{})
	if err != nil || release.Values["PRIV_KEY"] != "priv-v1" {
		t.Fatalf("Values(PRIV_KEY) = %v, %v; want its value", release.Values, err)
	}
}

// TestAPodOfThePrivateProjectGetsItsSecret: a pod whose account an operator's entry maps to PRIV,
// for a person on PRIV's list, names PRIV and gets its secret; a pod whose entry maps another
// project is denied it.
func TestAPodOfThePrivateProjectGetsItsSecret(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	const otherSubject = "system:serviceaccount:example:other-sa"
	set, err := exchange.NewSet([]exchange.AccountEntry{
		{Account: fixtureSubject, Project: privProject, Person: insider},
		{Account: otherSubject, Project: teamProject, Person: insider},
	}, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	m.Exchange = set
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-priv-"+t.Name())
	if _, grantID := grantOf(t, m, pod, podKey, "deploy", "PRIV_KEY"); grantID == "" {
		t.Fatal("the PRIV pod got no grant")
	}
	other, otherKey := newServiceEnrollment(t, m.Store, fixtureService, otherSubject, "pod-team-"+t.Name())
	request, err := m.Create(t.Context(), other, signRequest(t, m, otherKey, "deploy", "PRIV_KEY"), "", nil)
	deniedFor(t, "a request for PRIV_KEY from a pod mapped to TEAM", request, err, "PRIV_KEY", policy.ReasonProjectNotNamed)
}

// TestAGrantOfAPrivateSecretEndsWhenTheSessionNoLongerNamesIt: a grant of TEAM's secret, made
// while TEAM was public, ends for good at its next release once TEAM turns private and the
// session does not name it, and TEAM turning public again does not revive it; a grant made while
// the session named private TEAM ends once its person leaves TEAM's list.
func TestAGrantOfAPrivateSecretEndsWhenTheSessionNoLongerNamesIt(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	_, grantID := grantOf(t, m, enr, key, "read it", "TEAM_TOKEN")
	if _, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{}); err != nil {
		t.Fatalf("Values while TEAM is public: %v", err)
	}
	answerFrom(t, m, dispatchRows(privateTeam(fixtureOperator)))
	if release, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || release.Values != nil {
		t.Fatalf("Values of grant %s after TEAM turned private = %v, %v; want ErrGrantNotLive", grantID, release.Values, err)
	}
	if by, _ := endedForGood(t, m, grantID); by != GrantEndedActor {
		t.Fatalf("grant %s revoked by %q, want %q", grantID, by, GrantEndedActor)
	}
	answerFrom(t, m, dispatchRows(publicTeam))
	if _, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values of grant %s after TEAM turned public again = %v; want it still ended", grantID, err)
	}

	answerFrom(t, m, dispatchRows(privateTeam(fixtureOperator)))
	nameProjects(t, m, enr, teamProject)
	_, named := grantOf(t, m, enr, key, "read it again", "TEAM_TOKEN")
	if _, err := m.Values(t.Context(), named, enr, nil, ReleaseCall{}); err != nil {
		t.Fatalf("Values while the session names private TEAM: %v", err)
	}
	answerFrom(t, m, dispatchRows(privateTeam()))
	if _, err := m.Values(t.Context(), named, enr, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values of grant %s after its person left TEAM's list = %v; want ErrGrantNotLive", named, err)
	}
}

// TestAskingAgainAfterTheProjectIsLostSaysProjectNotNamed: a session asking again for a secret
// whose live grant TEAM's turning private ended is told project-not-named, not that its grant
// is gone, and the grant it held is ended (reuseLiveGrant).
func TestAskingAgainAfterTheProjectIsLostSaysProjectNotNamed(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	_, grantID := grantOf(t, m, enr, key, "read it", "TEAM_TOKEN")
	answerFrom(t, m, dispatchRows(privateTeam(fixtureOperator)))
	request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read it", "TEAM_TOKEN"), "", nil)
	deniedFor(t, "asking again for TEAM_TOKEN after TEAM turned private", request, err, "TEAM_TOKEN", policy.ReasonProjectNotNamed)
	if by, _ := endedForGood(t, m, grantID); by != GrantEndedActor {
		t.Fatalf("grant %s revoked by %q, want %q: asking again ends the grant it would have reused", grantID, by, GrantEndedActor)
	}
}

// TestK3OutageWithholdsOnlyTaggedSecrets: while Dispatch cannot be reached, a request for a
// tagged secret fails ErrProjectAccessUnavailable and a release of a tagged grant releases
// nothing and leaves the grant live, while an untagged secret is requested and released as
// before.
func TestK3OutageWithholdsOnlyTaggedSecrets(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	_, tagged := grantOf(t, m, enr, key, "read it", "TEAM_TOKEN")
	dispatch := answerFrom(t, m, dispatchRows(publicTeam))
	dispatch.SetDown(true)
	if request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read it", "PRIV_KEY"), "", nil); !errors.Is(err, ErrProjectAccessUnavailable) {
		t.Fatalf("a request for PRIV_KEY while Dispatch is down = %+v, %v; want ErrProjectAccessUnavailable", request, err)
	}
	if release, err := m.Values(t.Context(), tagged, enr, nil, ReleaseCall{}); !errors.Is(err, ErrProjectAccessUnavailable) || release.Values != nil {
		t.Fatalf("Values of the TEAM grant while Dispatch is down = %v, %v; want ErrProjectAccessUnavailable", release.Values, err)
	}
	if by, _ := endedForGood(t, m, tagged); by != "" {
		t.Fatalf("an outage ended grant %s (revoked by %q); want it live once Dispatch answers", tagged, by)
	}
	_, untagged := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	if release, err := m.Values(t.Context(), untagged, enr, nil, ReleaseCall{}); err != nil || release.Values["AUTO_TOKEN"] != "auto-v1" {
		t.Fatalf("Values(AUTO_TOKEN) while Dispatch is down = %v, %v; want its value", release.Values, err)
	}
	dispatch.SetDown(false)
	if _, err := m.Values(t.Context(), tagged, enr, nil, ReleaseCall{}); err != nil {
		t.Fatalf("Values of the TEAM grant once Dispatch answers: %v", err)
	}
}

// TestOnlyAPersonWhoSeesTheProjectDecidesItsRequest: anyone may approve PRIV_HUMAN by owner and
// tier, but a person outside PRIV, approving or denying it, is refused pgx.ErrNoRows alone, the
// answer every read of the record gives them and the one an id no record has, and never
// record.ErrNotApprover, which a reader of the refusal would tell from a missing id by the order
// of its checks alone; the request stays pending. A person on PRIV's list approves it.
func TestOnlyAPersonWhoSeesTheProjectDecidesItsRequest(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	nameProjects(t, m, enr, privProject)
	request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read it", "PRIV_HUMAN"), "", nil)
	if err != nil || request.RecordID == nil {
		t.Fatalf("Create(PRIV_HUMAN) = %+v, %v; want pending", request, err)
	}
	for _, approve := range []bool{true, false} {
		if _, err := m.ApplyDecision(t.Context(), *request.RecordID, approve, outsider); !errors.Is(err, pgx.ErrNoRows) || errors.Is(err, record.ErrNotApprover) {
			t.Fatalf("a decision (approve %v) by %s, outside PRIV = %v; want pgx.ErrNoRows alone, not record.ErrNotApprover", approve, outsider, err)
		}
		if got, err := m.Get(t.Context(), request.ID); err != nil || got.State != "pending" {
			t.Fatalf("after the outsider's decision (approve %v) the request is %+v, %v; want pending", approve, got, err)
		}
	}
	if decision, err := m.ApplyDecision(t.Context(), *request.RecordID, true, insider); err != nil || decision.GrantID == "" {
		t.Fatalf("an approval by %s, on PRIV's list = %+v, %v; want a grant", insider, decision, err)
	}
}

// TestOnlyAPersonWhoSeesTheProjectDismissesItsExpiredRequest: PRIV_HUMAN's request is expired,
// and its approver is anyone. A person outside PRIV who names its record is answered
// pgx.ErrNoRows, the answer an id no record has, and nothing is dismissed: the request stays on
// the expired list of the fixture's operator, the person responsible for it, who is on PRIV's
// list. A person on PRIV's list then dismisses it. An id no record has answers the same (control).
func TestOnlyAPersonWhoSeesTheProjectDismissesItsExpiredRequest(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	nameProjects(t, m, enr, privProject)
	request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read it", "PRIV_HUMAN"), "", nil)
	if err != nil || request.RecordID == nil {
		t.Fatalf("Create(PRIV_HUMAN) = %+v, %v; want pending", request, err)
	}
	pastDeadline(t, m, request.ID)
	if _, err := m.Dismiss(t.Context(), *request.RecordID, outsider); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a dismissal by %s, outside PRIV = %v; want pgx.ErrNoRows, as for an id no record has", outsider, err)
	}
	if _, err := m.Dismiss(t.Context(), "0000000000000000000000000000000000000000000000000000000000000000", outsider); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a dismissal of an id no record has = %v; want pgx.ErrNoRows (control)", err)
	}
	if _, listed := listedRow(t, m, fixtureOperator, PendingStates{Expired: true}, *request.RecordID); !listed {
		t.Fatalf("%s's expired list lacks %s after the outsider's dismissal; want it still there", fixtureOperator, *request.RecordID)
	}
	if d, err := m.Dismiss(t.Context(), *request.RecordID, insider); err != nil || !d.Wrote {
		t.Fatalf("a dismissal by %s, on PRIV's list = %+v, %v; want it written", insider, d, err)
	}
}

// TestPendingLeavesOutAPrivateRequestForAnOutsider: an outsider's pending list holds neither the
// request for PRIV's secret nor a request for an untagged shared secret whose reason names a PRIV
// issue, and may not read either record; a person on PRIV's list sees both. Every issue key of a
// reason counts, not the first alone: "SHA-256" has an issue key's form and names no project, so a
// reason naming it before a PRIV issue is still PRIV's. The requests come from pods that name no
// person, so every signed-in person's list reads them (everyonesPod).
func TestPendingLeavesOutAPrivateRequestForAnOutsider(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	privPod, privKey := everyonesPod(t, m, "pod-t-priv", privProject)
	tagged, err := m.Create(t.Context(), privPod, signRequest(t, m, privKey, "read it", "PRIV_HUMAN"), "", nil)
	if err != nil || tagged.RecordID == nil {
		t.Fatalf("Create(PRIV_HUMAN) = %+v, %v", tagged, err)
	}
	plain, plainKey := everyonesPod(t, m, "pod-q-plain", "")
	reasoned, err := m.Create(t.Context(), plain, signRequest(t, m, plainKey, "PRIV-12: rotate the deploy key", "SHARED_KEY"), "", nil)
	if err != nil || reasoned.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v", reasoned, err)
	}
	hashed, hashedKey := everyonesPod(t, m, "pod-h-hashed", "")
	hashedReason, err := m.Create(t.Context(), hashed, signRequest(t, m, hashedKey, "SHA-256 of the PRIV-12 bundle", "SHARED_KEY"), "", nil)
	if err != nil || hashedReason.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v", hashedReason, err)
	}
	ofPRIV := []string{*tagged.RecordID, *reasoned.RecordID, *hashedReason.RecordID}
	listed := func(login string) []string {
		pending, err := m.PendingForApprover(t.Context(), login, PendingStates{})
		if err != nil {
			t.Fatalf("PendingForApprover(%s): %v", login, err)
		}
		var ids []string
		for _, p := range pending {
			ids = append(ids, p.RecordID)
		}
		return ids
	}
	if got := listed(outsider); slices.ContainsFunc(ofPRIV, func(id string) bool { return slices.Contains(got, id) }) {
		t.Fatalf("%s's pending list %v holds a request of PRIV (%v)", outsider, got, ofPRIV)
	}
	if got := listed(insider); slices.ContainsFunc(ofPRIV, func(id string) bool { return !slices.Contains(got, id) }) {
		t.Fatalf("%s's pending list %v lacks a request of PRIV it may see (%v)", insider, got, ofPRIV)
	}
	for _, id := range ofPRIV {
		if err := m.HideUnseen(t.Context(), id, outsider); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("HideUnseen(record %s, %s) = %v; want pgx.ErrNoRows", id, outsider, err)
		}
		if err := m.HideUnseen(t.Context(), id, insider); err != nil {
			t.Fatalf("HideUnseen(record %s, %s) = %v; want nil", id, insider, err)
		}
	}
}

// TestADispatchOutageLeavesOutOnlyTheRowsItCannotAnswerFor: nothing is tagged, the broker holds a
// client of Dispatch, and Dispatch is down. A pending request and a grant whose reasons name an
// issue key are rows Dispatch cannot answer for: each is left out of its list (fail closed for
// that row alone) and logged once with its id and the error, however often the list is read, and
// the rest of each list is answered. An approval of the pending one answers
// ErrProjectAccessUnavailable and leaves it pending, while the request whose reason names no key
// is approved. Once Dispatch answers, the request is listed again. The two pending requests come
// from pods that name no person, so the insider's list reads them (everyonesPod), and the two
// grants are of SERVICE_KEY on pods of the fixture's service, which name no approver and no
// operator, so the insider's list reads them too: a grant's own approver and operator list it
// whatever Dispatch answers (namedOnGrant).
func TestADispatchOutageLeavesOutOnlyTheRowsItCannotAnswerFor(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	keyedService, keyedServiceKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-g-keyed-"+t.Name())
	_, keyedGrant := grantOf(t, m, keyedService, keyedServiceKey, "INFRA-13: deploy the broker", "SERVICE_KEY")
	plainService, plainServiceKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-g-plain-"+t.Name())
	_, plainGrant := grantOf(t, m, plainService, plainServiceKey, "no issue named", "SERVICE_KEY")
	keyedPod, keyedKey := everyonesPod(t, m, "pod-o-keyed", "")
	keyed, err := m.Create(t.Context(), keyedPod, signRequest(t, m, keyedKey, "INFRA-12: rotate the deploy key", "SHARED_KEY"), "", nil)
	if err != nil || keyed.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY, INFRA-12) = %+v, %v; want pending", keyed, err)
	}
	plainPod, plainKey := everyonesPod(t, m, "pod-o-plain", "")
	plain, err := m.Create(t.Context(), plainPod, signRequest(t, m, plainKey, "no issue named", "SHARED_KEY"), "", nil)
	if err != nil || plain.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY, no issue) = %+v, %v; want pending", plain, err)
	}
	dispatch := answerFrom(t, m, dispatchRows(publicTeam))
	dispatch.SetDown(true)
	logged := policytest.CaptureLog(t)

	pendingIDs := func() []string {
		t.Helper()
		pending, err := m.PendingForApprover(t.Context(), insider, PendingStates{})
		if err != nil {
			t.Fatalf("PendingForApprover(%s) while Dispatch is down: %v; want the list answered", insider, err)
		}
		var ids []string
		for _, p := range pending {
			ids = append(ids, p.RecordID)
		}
		return ids
	}
	for range 2 {
		if ids := pendingIDs(); slices.Contains(ids, *keyed.RecordID) || !slices.Contains(ids, *plain.RecordID) {
			t.Fatalf("%s's pending list while Dispatch is down = %v; want %s (no issue named) and not %s (INFRA-12)", insider, ids, *plain.RecordID, *keyed.RecordID)
		}
		grants, err := m.GrantsForApprover(t.Context(), insider)
		if err != nil {
			t.Fatalf("GrantsForApprover(%s) while Dispatch is down: %v; want the list answered", insider, err)
		}
		var ids []string
		for _, g := range grants {
			ids = append(ids, g.GrantID)
		}
		if slices.Contains(ids, keyedGrant) || !slices.Contains(ids, plainGrant) {
			t.Fatalf("%s's grants while Dispatch is down = %v; want %s (no issue named) and not %s (INFRA-13)", insider, ids, plainGrant, keyedGrant)
		}
	}
	for _, id := range []string{*keyed.RecordID, keyedGrant} {
		if n := strings.Count(logged.String(), id); n != 1 {
			t.Errorf("the log names %s %d times after two readings of its list; want once:\n%s", id, n, logged)
		}
	}
	if !strings.Contains(logged.String(), "dispatch answered /api/v1/access/projects/INFRA 503") {
		t.Errorf("the log does not carry Dispatch's error:\n%s", logged)
	}

	if decision, err := m.ApplyDecision(t.Context(), *keyed.RecordID, true, insider); !errors.Is(err, ErrProjectAccessUnavailable) {
		t.Fatalf("an approval of %s (INFRA-12) while Dispatch is down = %+v, %v; want ErrProjectAccessUnavailable", *keyed.RecordID, decision, err)
	}
	if got, err := m.Get(t.Context(), keyed.ID); err != nil || got.State != "pending" {
		t.Fatalf("after the approval Dispatch could not answer, the request is %+v, %v; want pending", got, err)
	}
	if decision, err := m.ApplyDecision(t.Context(), *plain.RecordID, true, insider); err != nil || decision.GrantID == "" {
		t.Fatalf("an approval of %s (no issue named) while Dispatch is down = %+v, %v; want a grant", *plain.RecordID, decision, err)
	}
	if dispatch.Calls() == 0 {
		t.Fatal("Dispatch was never asked, so the outage was never met: the test is blind")
	}

	dispatch.SetDown(false)
	if ids := pendingIDs(); !slices.Contains(ids, *keyed.RecordID) {
		t.Fatalf("%s's pending list once Dispatch answers = %v; want %s again", insider, ids, *keyed.RecordID)
	}
}

// TestAnUntaggedDeploymentAsksDispatchNothing: with no secret tagged and no session naming a
// project, a request, an approval, a release, an airlock connection and both approver lists ask
// Dispatch nothing of a tag, a session or a channel. Their reasons name an issue key, as nearly
// every agent's does (the judge's issue check needs one), and "SHA-256", which has an issue key's
// form. A broker with no project-access client then asks Dispatch nothing at all. A broker holding
// one asks one thing and nothing else: whether the person reading a row may see the project of
// each key its reason names (HideUnseen), every key and not the first alone, since a reason
// naming a private project's issue is that project's text. "Asks Dispatch nothing" is true of the
// first broker; of the second it is true of everything but those reads.
func TestAnUntaggedDeploymentAsksDispatchNothing(t *testing.T) {
	const reason = "INFRA-12: check the SHA-256 of the deploy bundle"
	reasonReads := []string{"access INFRA", "access SHA"}
	for _, c := range []struct {
		name   string
		client bool
	}{{"no project-access client", false}, {"a project-access client", true}} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{Secrets: []proxyrules.SecretRule{rule("AUTO_TOKEN", "")}}, publicTeam)
			dispatch := answerFrom(t, m, dispatchRows(publicTeam))
			asked := &askLog{inner: m.Projects}
			m.Projects = asked
			if !c.client {
				m.Projects = nil
			}
			_, grantID := grantOf(t, m, enr, key, reason, "AUTO_TOKEN")
			if _, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{}); err != nil {
				t.Fatalf("Values: %v", err)
			}
			if _, err := m.AirlockConnection(t.Context(), placeholderRows(t, m, grantID)["AUTO_TOKEN/"+SlotValue]); err != nil {
				t.Fatalf("AirlockConnection: %v", err)
			}
			if _, _, err := m.AirlockValues(t.Context(), grantID); err != nil {
				t.Fatalf("AirlockValues: %v", err)
			}
			pending, err := m.Create(t.Context(), enr, signRequest(t, m, key, reason, "SHARED_KEY"), "", nil)
			if err != nil || pending.RecordID == nil {
				t.Fatalf("Create(SHARED_KEY) = %+v, %v", pending, err)
			}
			// A shared secret's request of the operator's session is listed for the operator,
			// the person responsible for the agent (responsiblePerson).
			if listed, err := m.PendingForApprover(t.Context(), fixtureOperator, PendingStates{}); err != nil || len(listed) != 1 {
				t.Fatalf("PendingForApprover(%s) = %+v, %v; want the one request", fixtureOperator, listed, err)
			}
			if _, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, insider); err != nil {
				t.Fatalf("ApplyDecision: %v", err)
			}
			if listed, err := m.GrantsForApprover(t.Context(), fixtureOperator); err != nil || len(listed) != 2 {
				t.Fatalf("GrantsForApprover(%s) = %+v, %v; want both grants", fixtureOperator, listed, err)
			}
			questions := asked.questions()
			if !c.client {
				if calls := dispatch.Calls(); calls != 0 || len(questions) != 0 {
					t.Fatalf("a broker with no project-access client asked Dispatch %v (%d calls); want nothing at all", questions, calls)
				}
				return
			}
			for _, q := range questions {
				if !slices.Contains(reasonReads, q) {
					t.Errorf("an untagged deployment asked Dispatch %q; want only the reason keys' project reads %v", q, reasonReads)
				}
			}
			for _, want := range reasonReads {
				if !slices.Contains(questions, want) {
					t.Errorf("an untagged deployment never asked Dispatch %q (it asked %v): a key of the reason went unread", want, questions)
				}
			}
		})
	}
}

// askLog is a projectaccess.Access that writes down each question it passes on, so a test can
// say which questions the broker asked Dispatch and not only how many, and whom each access
// question asked about.
type askLog struct {
	inner   projectaccess.Access
	mu      sync.Mutex
	asked   []string
	persons []string
}

func (a *askLog) note(question string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, question)
}

// questions is every question asked so far, in order.
func (a *askLog) questions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.asked)
}

// askedAbout is the person of every access question asked so far, canonical, in order.
func (a *askLog) askedAbout() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.persons)
}

func (a *askLog) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	a.mu.Lock()
	a.persons = append(a.persons, record.CanonicalLogin(person))
	a.mu.Unlock()
	a.note("access " + project)
	return a.inner.Access(ctx, project, person)
}

func (a *askLog) MemberKind(ctx context.Context, person string) (string, error) {
	a.note("member kind of " + person)
	return a.inner.MemberKind(ctx, person)
}

func (a *askLog) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	a.note("states of " + strings.Join(projects, " "))
	return a.inner.States(ctx, projects)
}

// lockProbe is a projectaccess.Access that, before each question it passes on, tries to take
// the enrollment row's lock itself with NOWAIT from a transaction of its own. A caller asking
// Dispatch from inside a transaction that holds that row (Create's lockLiveEnrollment, the
// approve path's for share) makes the probe fail at once, which is what the rule forbids: a call
// to Dispatch made under a lock holds every writer of that row behind it.
type lockProbe struct {
	inner        projectaccess.Access
	t            *testing.T
	m            *Machine
	enrollmentID string
	probes       atomic.Int32
}

func (p *lockProbe) probe(ctx context.Context, question string) {
	p.probes.Add(1)
	tx, err := p.m.Store.Pool.Begin(ctx)
	if err != nil {
		p.t.Errorf("the probe could not begin: %v", err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from enrollments where id=$1 for update nowait`, p.enrollmentID); err != nil {
		p.t.Errorf("Dispatch was asked %s while a transaction held the enrollment's row: %v", question, err)
	}
}

func (p *lockProbe) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	p.probe(ctx, "about "+project)
	return p.inner.Access(ctx, project, person)
}

func (p *lockProbe) MemberKind(ctx context.Context, person string) (string, error) {
	p.probe(ctx, "the member kind of "+person)
	return p.inner.MemberKind(ctx, person)
}

func (p *lockProbe) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	p.probe(ctx, "the states of "+strings.Join(projects, " "))
	return p.inner.States(ctx, projects)
}

// TestNoK3CallRunsInsideATransaction: every question a request, an approval, a release, an
// airlock connection and an approver's list ask Dispatch is asked while no transaction holds the
// requesting enrollment's row (lockProbe). The probe must have run, or the test proves nothing.
func TestNoK3CallRunsInsideATransaction(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{Secrets: []proxyrules.SecretRule{rule("PRIV_KEY", "")}}, publicTeam)
	nameProjects(t, m, enr, privProject)
	probe := &lockProbe{inner: m.Projects, t: t, m: m, enrollmentID: enr}
	m.Projects = probe
	_, grantID := grantOf(t, m, enr, key, "read it", "PRIV_KEY")
	if _, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{}); err != nil {
		t.Fatalf("Values: %v", err)
	}
	if _, err := m.AirlockConnection(t.Context(), placeholderRows(t, m, grantID)["PRIV_KEY/"+SlotValue]); err != nil {
		t.Fatalf("AirlockConnection: %v", err)
	}
	if _, _, err := m.AirlockValues(t.Context(), grantID); err != nil {
		t.Fatalf("AirlockValues: %v", err)
	}
	pending, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read it", "PRIV_HUMAN"), "", nil)
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(PRIV_HUMAN) = %+v, %v", pending, err)
	}
	if _, err := m.PendingForApprover(t.Context(), insider, PendingStates{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, insider); err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	if n := probe.probes.Load(); n == 0 {
		t.Fatal("Dispatch was asked nothing, so the probe saw no call: the test is blind")
	}
}
