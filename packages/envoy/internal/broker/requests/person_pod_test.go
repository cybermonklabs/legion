// person_pod_test.go drives a pod whose Kubernetes account BROKER_POD_OPERATORS maps to a person
// (Machine.PodOperators). Such a pod asks for secrets as that person asks from the person's dev
// box. It acts for that person only while the setting still maps its account to that person.
// The grant log and the person's grant list read the enrollment row, which keeps the person.
package requests

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/store"
)

// personPod is one person's mapped account: the person, the login service of the person's own,
// and the Kubernetes service account that BROKER_POD_OPERATORS maps to the person.
type personPod struct {
	person, service, account string
}

// samisPod is fixtureOperator's mapped account, and alicesPod is otherPerson's.
var (
	samisPod  = personPod{person: fixtureOperator, service: "person-sami", account: "system:serviceaccount:people:sami"}
	alicesPod = personPod{person: otherPerson, service: "person-alice", account: "system:serviceaccount:people:alice"}
)

// The secrets newPersonPodFixture adds to fixtureSecrets. personPodShared is a shared agent-tier
// secret. personPodListed and personPodListedHuman are fixtureOperator's, at agent tier and at
// human tier, and their requesters tag names fixtureOperator alone. listedSecret gives each of
// the two a max-grant of an hour, so every request for them signs anHour.
const (
	personPodShared      = "SHARED_TOKEN"
	personPodSharedValue = "shared-token-v1"
	personPodListed      = "LISTED_KEY"
	personPodListedHuman = "LISTED_HUMAN_KEY"
)

// enroll inserts the rows that a mapped pod's token login and enrollment write, as
// insertEnrollment does for its own rows. The launcher credential has p.person as its operator,
// p.service as its service, p.account as its subject and podUID as its pod. The live pod
// enrollment under it has p.person as its operator and p.account as its subject. enroll answers
// the enrollment's id and its signing key.
func (p personPod) enroll(t *testing.T, st *store.Store, podUID string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate enrollment key: %v", err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	host := strings.ReplaceAll(strings.TrimPrefix(p.account, "system:serviceaccount:"), ":", "/")
	credentialID := uuid.New()
	if _, err := st.Pool.Exec(ctx, `insert into launcher_credentials (id, operator, service, host, subject, pod_uid, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,$3,$4,$5,$6,$7,'{}'::jsonb, now() + interval '1 hour')`, credentialID, p.person, p.service, host, p.account, podUID, uuid.NewString()); err != nil {
		t.Fatalf("insert the launcher credential of %s's pod %s: %v", p.person, podUID, err)
	}
	id := uuid.NewString()
	if _, err := st.Pool.Exec(ctx, `insert into enrollments (id, kind, runtime_id, operator, thumbprint, subject, launcher_credential_id, lease_expires_at)
		values ($1,'pod',$2,$3,$4,$5,$6, now() + interval '1 hour')`, id, podUID, p.person, thumbprint, p.account, credentialID); err != nil {
		t.Fatalf("insert the enrollment of %s's pod %s: %v", p.person, podUID, err)
	}
	return id, key
}

// newPersonPodFixture is newFixture with samisPod and alicesPod mapped in m.PodOperators and the
// secrets above added to the policy. It enrols one pod of samisPod's account and answers the
// Machine, that pod's enrollment id and its signing key.
func newPersonPodFixture(t *testing.T) (*Machine, string, *ecdsa.PrivateKey) {
	t.Helper()
	m, _, _, _ := newFixture(t)
	m.PodOperators = map[string]string{samisPod.account: samisPod.person, alicesPod.account: alicesPod.person}
	retag(t, m,
		policytest.Secret(personPodShared, policy.OwnerShared, policy.TierAgent, personPodSharedValue),
		listedSecret(personPodListed, fixtureOperator, policy.TierAgent, fixtureOperator),
		listedSecret(personPodListedHuman, fixtureOperator, policy.TierHuman, fixtureOperator))
	pod, key := samisPod.enroll(t, m.Store, "pod-sami-"+t.Name())
	return m, pod, key
}

// approveAsPerson applies fixtureOperator's approval to req, a pending request, and answers the
// grant.
func approveAsPerson(t *testing.T, m *Machine, req Request) string {
	t.Helper()
	if req.State != "pending" || req.RecordID == nil {
		t.Fatalf("request %s = %+v; want it pending with a record to approve", req.ID, req)
	}
	decision, err := m.ApplyDecision(context.Background(), *req.RecordID, true, fixtureOperator)
	if err != nil || decision.State != "granted" || decision.GrantID == "" {
		t.Fatalf("ApplyDecision(%s, approved by %s) = %+v, %v; want a grant", *req.RecordID, fixtureOperator, decision, err)
	}
	return decision.GrantID
}

// listsGrant reports whether GrantsForApprover(person) lists grantID.
func listsGrant(t *testing.T, m *Machine, person, grantID string) bool {
	t.Helper()
	grants, err := m.GrantsForApprover(context.Background(), person)
	if err != nil {
		t.Fatalf("GrantsForApprover(%s): %v", person, err)
	}
	return slices.ContainsFunc(grants, func(g ApproverGrant) bool { return g.GrantID == grantID })
}

// TestPersonPodIsDecidedAsItsPerson: a pod whose account the setting maps to a person asks as that
// person asks from the person's dev box (dispatch://INFRA-4511/spec#b-r3). The person's
// agent-tier secret is granted with no approval. The person's human-tier secret waits for the
// person. Another person's agent-tier secret waits for its owner. A shared agent-tier secret is
// granted. A service's secret is denied. The request.created audit row names the person.
func TestPersonPodIsDecidedAsItsPerson(t *testing.T) {
	ctx := context.Background()
	m, pod, key := newPersonPodFixture(t)
	for _, want := range []struct {
		secret, state, approver, value string
	}{
		{secret: "AUTO_TOKEN", state: "granted", value: "auto-v1"},
		{secret: "DEEL_API_KEY", state: "pending", approver: fixtureOperator},
		{secret: "ALICE_KEY", state: "pending", approver: otherPerson},
		{secret: personPodShared, state: "granted", value: personPodSharedValue},
		{secret: "SERVICE_KEY", state: "denied"},
	} {
		t.Run(want.secret, func(t *testing.T) {
			req, err := m.Create(ctx, pod, signRequest(t, m, key, "need it", want.secret), "", nil)
			if err != nil || req.State != want.state {
				t.Fatalf("Create(%s's pod %s, %s) = %+v, %v; want %s", samisPod.person, pod, want.secret, req, err, want.state)
			}
			if _, person, _ := createdAudit(t, m, req.ID); person != samisPod.person {
				t.Fatalf("the request.created audit row of %s names person %q; want %s", req.ID, person, samisPod.person)
			}
			switch want.state {
			case "granted":
				if req.GrantID == nil || req.RecordID != nil {
					t.Fatalf("Create(%s) = %+v; want a grant that no person approved", want.secret, req)
				}
				released, err := m.Values(ctx, *req.GrantID, pod, nil, ReleaseCall{})
				if err != nil || released.Values[want.secret] != want.value {
					t.Fatalf("Values(grant %s of %s) = %v, %v; want %s", *req.GrantID, want.secret, released.Values, err, want.value)
				}
			case "pending":
				if req.GrantID != nil || req.Approver == nil || *req.Approver != want.approver {
					t.Fatalf("Create(%s) = %+v; want it pending for %s with no grant", want.secret, req, want.approver)
				}
			}
		})
	}
}

// TestPersonPodIsOnTheListThatNamesItsPerson: a secret whose requesters tag names the person is
// granted to the person's mapped pod. Another person's mapped pod and a Legion worker pod are
// not on the list, so each is denied requester-not-listed.
func TestPersonPodIsOnTheListThatNamesItsPerson(t *testing.T) {
	ctx := context.Background()
	m, pod, key := newPersonPodFixture(t)
	granted, err := m.Create(ctx, pod, signRequestFor(t, m, key, anHour, personPodListed), "", nil)
	if err != nil || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("Create(%s's pod %s, %s) = %+v, %v; want it granted at once to the person the list names", samisPod.person, pod, personPodListed, granted, err)
	}
	if released, err := m.Values(ctx, *granted.GrantID, pod, nil, ReleaseCall{}); err != nil || released.Values[personPodListed] != listedValue {
		t.Fatalf("Values(grant %s) = %v, %v; want %s", *granted.GrantID, released.Values, err, listedValue)
	}

	alices, alicesKey := alicesPod.enroll(t, m.Store, "pod-alice-"+t.Name())
	worker, workerKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-worker-"+t.Name())
	for _, who := range []struct {
		name, enrollment string
		key              *ecdsa.PrivateKey
	}{
		{name: "another person's mapped pod", enrollment: alices, key: alicesKey},
		{name: "a Legion worker pod", enrollment: worker, key: workerKey},
	} {
		t.Run(who.name, func(t *testing.T) {
			const want = personPodListed + ": " + policy.ReasonRequesterNotListed
			denied, err := m.Create(ctx, who.enrollment, signRequestFor(t, m, who.key, anHour, personPodListed), "", nil)
			if err != nil || denied.State != "denied" || denied.GrantID != nil {
				t.Fatalf("Create(%s %s, %s) = %+v, %v; want it denied with no grant", who.name, who.enrollment, personPodListed, denied, err)
			}
			if denied.Detail == nil || *denied.Detail != want {
				t.Fatalf("the denied request's detail = %v; want %q", denied.Detail, want)
			}
		})
	}
}

// TestPersonPodGrantsFollowTheSetting: the setting drops the pod's account, or maps it to another
// person, while the pod holds grants it got as its person (D4 of
// dispatch://INFRA-4511/artifact/plan-md). The policy's version does not move, so only the
// setting can open the re-check.
//
//   - The automatic grant of the person's agent-tier secret is not reused: a new request for the
//     secret waits for the person. It does not release, through Values or AirlockValues. It is
//     not ended, so it releases again once the entry is back.
//   - A shared agent-tier secret is still granted and released.
//   - A grant the person approved before the change still releases, and is on the person's list.
//     It is not on the other person's list, also after a move to that person.
//   - A grant of a secret whose requesters tag names the person, automatic or approved, ends for
//     good at its next release. It stays ended once the entry is back.
//
// Before the change, a second request for the agent-tier secret reuses its grant. That makes
// the refused reuse after the change reachable. The release once the entry is back shows that
// the setting, and nothing else, refused the grant.
func TestPersonPodGrantsFollowTheSetting(t *testing.T) {
	for _, tc := range []struct {
		name string
		// change applies the setting's change to the map.
		change func(map[string]string)
		moved  bool
	}{
		{name: "the setting drops the account", change: func(m map[string]string) { delete(m, samisPod.account) }},
		{name: "the setting maps the account to another person", change: func(m map[string]string) { m[samisPod.account] = otherPerson }, moved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, pod, key := newPersonPodFixture(t)

			auto, err := m.Create(ctx, pod, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "", nil)
			if err != nil || auto.State != "granted" || auto.GrantID == nil || auto.RecordID != nil {
				t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want an automatic grant to %s's pod %s", auto, err, samisPod.person, pod)
			}
			autoGrant := *auto.GrantID
			asked, err := m.Create(ctx, pod, signRequest(t, m, key, "ship it", "DEEL_API_KEY"), "", nil)
			if err != nil {
				t.Fatalf("Create(DEEL_API_KEY): %v", err)
			}
			approvedGrant := approveAsPerson(t, m, asked)
			listedAuto, err := m.Create(ctx, pod, signRequestFor(t, m, key, anHour, personPodListed), "", nil)
			if err != nil || listedAuto.State != "granted" || listedAuto.GrantID == nil {
				t.Fatalf("Create(%s) = %+v, %v; want an automatic grant to the person the list names", personPodListed, listedAuto, err)
			}
			listedAsked, err := m.Create(ctx, pod, signRequestFor(t, m, key, anHour, personPodListedHuman), "", nil)
			if err != nil {
				t.Fatalf("Create(%s): %v", personPodListedHuman, err)
			}
			listedApproved := approveAsPerson(t, m, listedAsked)

			reused, err := m.Create(ctx, pod, signRequest(t, m, key, "need it again", "AUTO_TOKEN"), "", nil)
			if err != nil || reused.ID != auto.ID || reused.GrantID == nil || *reused.GrantID != autoGrant {
				t.Fatalf("Create(AUTO_TOKEN) again before the change = %+v, %v; want request %s's live grant %s reused", reused, err, auto.ID, autoGrant)
			}
			version := m.Policy.Get().Version

			tc.change(m.PodOperators)
			if now := m.Policy.Get().Version; now != version {
				t.Fatalf("the policy moved from version %s to %s; this case turns on the version standing still, so that the setting alone opens the re-check", version, now)
			}

			again, err := m.Create(ctx, pod, signRequest(t, m, key, "need it after the change", "AUTO_TOKEN"), "", nil)
			if err != nil || again.State != "pending" || again.ID == auto.ID || again.GrantID != nil || again.Approver == nil || *again.Approver != samisPod.person {
				t.Fatalf("Create(AUTO_TOKEN) after the change = %+v, %v; want a new request pending for %s, not request %s's grant %s reused", again, err, samisPod.person, auto.ID, autoGrant)
			}
			if _, person, _ := createdAudit(t, m, again.ID); person != "" {
				t.Fatalf("the request.created audit row of %s names person %q; want none, as the pod acts for no person now", again.ID, person)
			}
			if released, err := m.Values(ctx, autoGrant, pod, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
				t.Fatalf("Values(automatic grant %s of AUTO_TOKEN) after the change = %v, %v; want ErrGrantNotLive", autoGrant, released.Values, err)
			}
			if values, _, err := m.AirlockValues(ctx, autoGrant); !errors.Is(err, ErrGrantNotLive) {
				t.Fatalf("AirlockValues(automatic grant %s of AUTO_TOKEN) after the change = %v, %v; want ErrGrantNotLive", autoGrant, values, err)
			}
			if by, audits := endedForGood(t, m, autoGrant); by != "" || audits != 0 {
				t.Fatalf("automatic grant %s: revoked_by %q with %d grant.revoked rows; want it refused but not ended", autoGrant, by, audits)
			}

			shared, err := m.Create(ctx, pod, signRequest(t, m, key, "need it", personPodShared), "", nil)
			if err != nil || shared.State != "granted" || shared.GrantID == nil {
				t.Fatalf("Create(%s) after the change = %+v, %v; want it granted at once", personPodShared, shared, err)
			}
			if released, err := m.Values(ctx, *shared.GrantID, pod, nil, ReleaseCall{}); err != nil || released.Values[personPodShared] != personPodSharedValue {
				t.Fatalf("Values(grant %s of %s) after the change = %v, %v; want %s", *shared.GrantID, personPodShared, released.Values, err, personPodSharedValue)
			}

			if released, err := m.Values(ctx, approvedGrant, pod, nil, ReleaseCall{}); err != nil || released.Values["DEEL_API_KEY"] != "deel-v1" {
				t.Fatalf("Values(grant %s of DEEL_API_KEY, which %s approved) after the change = %v, %v; want deel-v1", approvedGrant, samisPod.person, released.Values, err)
			}
			if !listsGrant(t, m, samisPod.person, approvedGrant) {
				t.Fatalf("GrantsForApprover(%s) after the change does not list grant %s, which %s approved", samisPod.person, approvedGrant, samisPod.person)
			}
			if listsGrant(t, m, otherPerson, approvedGrant) {
				t.Fatalf("GrantsForApprover(%s) after the change lists grant %s, which %s approved on %s's pod", otherPerson, approvedGrant, samisPod.person, samisPod.person)
			}

			for name, grant := range map[string]string{personPodListed: *listedAuto.GrantID, personPodListedHuman: listedApproved} {
				if released, err := m.Values(ctx, grant, pod, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
					t.Fatalf("Values(grant %s of %s) after the change = %v, %v; want ErrGrantNotLive", grant, name, released.Values, err)
				}
				if by, audits := endedForGood(t, m, grant); by != GrantEndedActor || audits != 1 {
					t.Fatalf("grant %s of %s: revoked_by %q with %d grant.revoked rows by %s; want it ended once", grant, name, by, audits, GrantEndedActor)
				}
			}

			if tc.moved {
				alices, err := m.Create(ctx, pod, signRequest(t, m, key, "need it", "ALICE_KEY"), "", nil)
				if err != nil || alices.State != "pending" || alices.GrantID != nil {
					t.Fatalf("Create(ALICE_KEY) after the move to %s = %+v, %v; want it pending with no grant, as the pod does not act for %s", otherPerson, alices, err, otherPerson)
				}
			}

			m.PodOperators[samisPod.account] = samisPod.person
			if released, err := m.Values(ctx, autoGrant, pod, nil, ReleaseCall{}); err != nil || released.Values["AUTO_TOKEN"] != "auto-v1" {
				t.Fatalf("Values(automatic grant %s of AUTO_TOKEN) once the entry is back = %v, %v; want auto-v1", autoGrant, released.Values, err)
			}
			if _, _, err := m.AirlockValues(ctx, autoGrant); err != nil {
				t.Fatalf("AirlockValues(automatic grant %s of AUTO_TOKEN) once the entry is back = %v; want no refusal", autoGrant, err)
			}
			for name, grant := range map[string]string{personPodListed: *listedAuto.GrantID, personPodListedHuman: listedApproved} {
				if released, err := m.Values(ctx, grant, pod, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
					t.Fatalf("Values(grant %s of %s) once the entry is back = %v, %v; want the grant still ended", grant, name, released.Values, err)
				}
				if by, audits := endedForGood(t, m, grant); by != GrantEndedActor || audits != 1 {
					t.Fatalf("grant %s of %s once the entry is back: revoked_by %q with %d grant.revoked rows; want the one end", grant, name, by, audits)
				}
			}
		})
	}
}

// TestPersonPodGrantsAreThePersonsToSee: the person's grant list (GrantsForApprover, which GET
// /v1/grants serves) lists the mapped pod's grants to the person and to no other person. The
// grant log (store.ReadGrantLog) names the person on each decision of the pod. Both read the
// enrollment row. So a request the pod makes after the setting drops the account still names the
// person in the grant log, although its request.created audit row names no person.
func TestPersonPodGrantsAreThePersonsToSee(t *testing.T) {
	ctx := context.Background()
	m, pod, key := newPersonPodFixture(t)
	since := time.Now().Add(-time.Hour)

	auto, err := m.Create(ctx, pod, signRequest(t, m, key, "need it", "AUTO_TOKEN"), "", nil)
	if err != nil || auto.State != "granted" || auto.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want an automatic grant", auto, err)
	}
	asked, err := m.Create(ctx, pod, signRequest(t, m, key, "ship it", "DEEL_API_KEY"), "", nil)
	if err != nil {
		t.Fatalf("Create(DEEL_API_KEY): %v", err)
	}
	approved := approveAsPerson(t, m, asked)

	for _, grant := range []string{*auto.GrantID, approved} {
		if !listsGrant(t, m, samisPod.person, grant) {
			t.Errorf("GrantsForApprover(%s) does not list grant %s of %s's pod %s", samisPod.person, grant, samisPod.person, pod)
		}
		if listsGrant(t, m, otherPerson, grant) {
			t.Errorf("GrantsForApprover(%s) lists grant %s of %s's pod %s", otherPerson, grant, samisPod.person, pod)
		}
	}

	delete(m.PodOperators, samisPod.account)
	after, err := m.Create(ctx, pod, signRequest(t, m, key, "need it after the change", "AUTO_TOKEN"), "", nil)
	if err != nil || after.State != "pending" || after.ID == auto.ID || after.RecordID == nil {
		t.Fatalf("Create(AUTO_TOKEN) after the setting drops the account = %+v, %v; want a new request pending for %s, not request %s reused", after, err, samisPod.person, auto.ID)
	}
	if _, person, _ := createdAudit(t, m, after.ID); person != "" {
		t.Fatalf("the request.created audit row of %s names person %q; want none, as the pod acts for no person now", after.ID, person)
	}

	events := readGrantLog(t, m, since)
	for _, row := range []struct{ kind, request string }{
		{kind: "request.created", request: auto.ID},
		{kind: "request.granted", request: asked.ID},
		{kind: "request.created", request: after.ID},
	} {
		ev := eventFor(t, events, row.kind, row.request)
		if ev.Person != samisPod.person || ev.Session.Kind != "pod" || ev.EnrollmentID != pod {
			t.Errorf("the grant log's %s row of request %s names person %q on a %q session of enrollment %s; want %s on pod %s", row.kind, row.request, ev.Person, ev.Session.Kind, ev.EnrollmentID, samisPod.person, pod)
		}
	}
}
