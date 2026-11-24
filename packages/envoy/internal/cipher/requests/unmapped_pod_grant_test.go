package requests

import (
	"crypto/ecdsa"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestAnUnmappedPersonsPodNamesNoOperatorOnItsGrant: the exemption that lets a grant's operator
// list and end it whatever Dispatch answers (namedOnGrant) reads the person the enrollment that
// holds it acts for (operatorOf), not the operator column alone. dave's pod enrolls while
// BROKER_POD_OPERATORS maps its account to dave, so its row names dave as its operator. The same
// account is also mapped by BROKER_EXCHANGE_ACCOUNTS to the private project PRIV for carol. Once
// the setting drops the account, the pod acts for no person and names PRIV through that entry
// (credentialProjects), and it is granted PRIV_HUMAN, which carol approves. dave is on no list of
// PRIV, so the grant is hidden from him as from any outsider: it is not on his Live grants, and
// his revoke answers as an unknown grant's does and ends nothing. carol, its approver, still lists
// it.
func TestAnUnmappedPersonsPodNamesNoOperatorOnItsGrant(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	dave := personPod{person: "dave@example.com", service: "person-dave", account: "system:serviceaccount:people:dave"}
	set, err := kubernetesPolicy([]kubernetes.AccountEntry{{Account: dave.account, Project: privProject, Person: insider}})
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	m.Exchange = set
	m.PodOperators = map[string]string{dave.account: dave.person}
	pod, key := dave.enroll(t, m.Store, "pod-dave-unmapped")
	delete(m.PodOperators, dave.account)

	req, err := m.Create(t.Context(), pod, signRequest(t, m, key, "after the setting dropped the account", "PRIV_HUMAN"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create(PRIV_HUMAN) from the unmapped pod = %+v, %v; want it pending carol's approval", req, err)
	}
	dec, err := m.ApplyDecision(t.Context(), *req.RecordID, true, insider)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision by %s = %+v, %v; want a grant", insider, dec, err)
	}
	grantID := dec.GrantID
	var operator *string
	if err := m.Store.Pool.QueryRow(t.Context(), `select e.operator from grants g join enrollments e on e.id=g.enrollment_id where g.id=$1`, grantID).Scan(&operator); err != nil || deref(operator) != dave.person {
		t.Fatalf("the grant's enrollment names operator %q, %v; want %s, so the row only cannot decide it", deref(operator), err, dave.person)
	}

	listed := func(login string) bool {
		t.Helper()
		grants, err := m.GrantsForApprover(t.Context(), login)
		if err != nil {
			t.Fatalf("GrantsForApprover(%s): %v", login, err)
		}
		return slices.ContainsFunc(grants, func(g ApproverGrant) bool { return g.GrantID == grantID })
	}
	if !listed(insider) {
		t.Errorf("carol, the grant's approver, does not list grant %s", grantID)
	}
	if listed(dave.person) {
		t.Errorf("dave, on no list of PRIV, lists grant %s of the pod that no longer acts for him", grantID)
	}
	if err := m.RevokeByApprover(t.Context(), grantID, dave.person, record.HumanActor(dave.person)); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("dave's revoke of grant %s = %v; want the answer an unknown grant gets (pgx.ErrNoRows)", grantID, err)
	}
	var revoked bool
	if err := m.Store.Pool.QueryRow(t.Context(), `select revoked_at is not null from grants where id=$1`, grantID).Scan(&revoked); err != nil || revoked {
		t.Fatalf("grant %s revoked = %v, %v after dave's refused revoke; want it still live", grantID, revoked, err)
	}

	// The control: once BROKER_POD_OPERATORS maps the account to dave again, the pod acts for
	// dave, so the grant names him as its operator and he keeps the exemption, though PRIV does
	// not list him: he lists the grant and ends it.
	m.PodOperators[dave.account] = dave.person
	if !listed(dave.person) {
		t.Errorf("dave, whose pod the setting maps to him again, does not list grant %s", grantID)
	}
	if err := m.RevokeByApprover(t.Context(), grantID, dave.person, record.HumanActor(dave.person)); err != nil {
		t.Errorf("dave's revoke of grant %s once the setting maps the pod to him again = %v; want it ended", grantID, err)
	}
	requireRevokedBy(t, m, grantID, dave.person)
}

// TestAnUnmappedPersonsPodsFormerPersonEndsAGrantTheySeeOnlyAsAnyoneMay: the revoke's transaction
// reads the operator the person rule reads (operatorOf), so a former person of a person's pod
// that BROKER_POD_OPERATORS no longer maps is not its grant's operator there either. dave's pod
// enrolls while the setting maps it to dave, and BROKER_EXCHANGE_ACCOUNTS maps the same account to
// the public project TEAM, so once the setting drops the account the pod names TEAM and every
// grant it gets is one dave may see. The pod asks for DEEL_API_KEY, which the fixture's operator
// approves. dave lists the grant, but his revoke is refused NOT_APPROVER, as anyone's who neither
// approved it nor is its operator, and the grant stays live with nothing withheld from the pod. Its
// approver still ends it.
func TestAnUnmappedPersonsPodsFormerPersonEndsAGrantTheySeeOnlyAsAnyoneMay(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	dave := personPod{person: "dave@example.com", service: "person-dave", account: "system:serviceaccount:people:dave"}
	set, err := kubernetesPolicy([]kubernetes.AccountEntry{{Account: dave.account, Project: teamProject, Person: insider}})
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	m.Exchange = set
	m.PodOperators = map[string]string{dave.account: dave.person}
	pod, key := dave.enroll(t, m.Store, "pod-dave-visible")
	delete(m.PodOperators, dave.account)

	req, err := m.Create(t.Context(), pod, signRequest(t, m, key, "deploy the billing service", "DEEL_API_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) from the unmapped pod = %+v, %v; want it pending its owner's approval", req, err)
	}
	dec, err := m.ApplyDecision(t.Context(), *req.RecordID, true, fixtureOperator)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision by %s = %+v, %v; want a grant", fixtureOperator, dec, err)
	}
	grantID := dec.GrantID
	grants, err := m.GrantsForApprover(t.Context(), dave.person)
	if err != nil || !slices.ContainsFunc(grants, func(g ApproverGrant) bool { return g.GrantID == grantID }) {
		t.Fatalf("GrantsForApprover(%s) = %+v, %v; want the grant of TEAM he may see, so the row reaches the transaction", dave.person, grants, err)
	}
	if err := m.RevokeByApprover(t.Context(), grantID, dave.person, record.HumanActor(dave.person)); !errors.Is(err, ErrNotApprover) {
		t.Errorf("dave's revoke of grant %s, which he may see but neither approved nor holds as operator = %v; want ErrNotApprover", grantID, err)
	}
	requireGrantLive(t, m, grantID)
	var withheld int
	if err := m.Store.Pool.QueryRow(t.Context(), `select count(*) from withheld_secrets where enrollment_id=$1`, pod).Scan(&withheld); err != nil || withheld != 0 {
		t.Errorf("names withheld from the pod = %d, %v; want none", withheld, err)
	}
	if err := m.RevokeByApprover(t.Context(), grantID, fixtureOperator, record.HumanActor(fixtureOperator)); err != nil {
		t.Errorf("the approver's revoke of grant %s = %v; want it ended", grantID, err)
	}
	requireRevokedBy(t, m, grantID, fixtureOperator)
}

// TestAPodsAutomaticGrantIsOnThePageOfWhoeverMaySeeIt: a grant a pod took without asking names
// no approver, so it is on a person's Live grants because of whom the pod acts for. A pod that
// acts for no person, a service's or a person's pod BROKER_POD_OPERATORS no longer maps, has its
// grant listed to every signed-in person the person rule shows it to (GrantsForApprover's everyone
// arm), so a private project's grant is on its people's pages and nobody else's, the unmapped pod's
// former person among the others. A mapped person's pod acts for its person, so its grant is on
// that person's page alone. Each pod names its project through its BROKER_EXCHANGE_ACCOUNTS entry.
// The people asked are dave (the pod's person), sami and carol (PRIV's list), alice (PRIV's owner)
// and mallory (on neither project).
func TestAPodsAutomaticGrantIsOnThePageOfWhoeverMaySeeIt(t *testing.T) {
	const dave = "dave@example.com"
	everyone := []string{dave, fixtureOperator, insider, otherPerson, outsider}
	for _, c := range []struct {
		name            string
		pod             string
		project, secret string
		mapped          bool
		wantListedTo    []string
	}{
		{name: "a service's pod, of PRIV", pod: "service", project: privProject, secret: "PRIV_KEY", wantListedTo: []string{fixtureOperator, insider, otherPerson}},
		{name: "an unmapped person's pod, of PRIV", pod: "person", project: privProject, secret: "PRIV_KEY", wantListedTo: []string{fixtureOperator, insider, otherPerson}},
		{name: "a mapped person's pod, of public TEAM", pod: "person", project: teamProject, secret: "SLACK_TOKEN", mapped: true, wantListedTo: []string{dave}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
			var pod string
			var key *ecdsa.PrivateKey
			if c.pod == "person" {
				p := personPod{person: dave, service: "person-dave", account: "system:serviceaccount:people:dave"}
				set, err := kubernetesPolicy([]kubernetes.AccountEntry{{Account: p.account, Project: c.project, Person: insider}})
				if err != nil {
					t.Fatal(err)
				}
				m.Exchange = set
				m.PodOperators = map[string]string{p.account: p.person}
				pod, key = p.enroll(t, m.Store, "pod-dave-"+c.secret)
				if !c.mapped {
					delete(m.PodOperators, p.account)
				}
			} else {
				set, err := kubernetesPolicy([]kubernetes.AccountEntry{{Account: fixtureSubject, Project: c.project, Person: insider}})
				if err != nil {
					t.Fatal(err)
				}
				m.Exchange = set
				pod, key = newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-svc-"+c.secret)
			}
			_, grantID := grantOf(t, m, pod, key, "use the pod's secret", c.secret)
			var listedTo []string
			for _, who := range everyone {
				grants, err := m.GrantsForApprover(t.Context(), who)
				if err != nil {
					t.Fatalf("GrantsForApprover(%s): %v", who, err)
				}
				if slices.ContainsFunc(grants, func(g ApproverGrant) bool { return g.GrantID == grantID }) {
					listedTo = append(listedTo, who)
				}
			}
			if !slices.Equal(listedTo, c.wantListedTo) {
				t.Errorf("the live %s grant %s is on the Live grants of %v; want %v", c.secret, grantID, listedTo, c.wantListedTo)
			}
		})
	}
}
