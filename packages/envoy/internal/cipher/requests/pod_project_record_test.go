package requests

import (
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/oidc"
)

// TestAPrivateProjectsPodRequestIsDecidedListedAndShownOnlyToItsPeople: the production path of a
// private project's agent is a pod whose account an operator's entry maps to the project
// (BROKER_EXCHANGE_ACCOUNTS); its enrollment's own projects column stays empty, and its projects
// come from that entry (Exchange.PodProjects), as the release path already reads them. Each of
// its requests is that project's, so dispatch://INFRA-3885's c-d2-secret-release holds for it as
// for a session that named PRIV: "A private project's request is decided, listed and shown on the
// approval page only to a person who can see the project". Two requests of PRIV's pod: an
// untagged shared human-tier secret whose reason names no issue key (its reason is PRIV's text),
// and PRIV_HUMAN. For each, an outsider's pending list leaves it out, HideUnseen hides it from the
// outsider and shows it to a person on PRIV's list, the record names PRIV among the projects its
// session names (the approval page prints "which the session does not name" from that list), and
// the outsider's approval is refused pgx.ErrNoRows alone, as an id no record has.
//
// Found by the tester of cybermonklabs/legion#4135 on a dev-stack.sh --project-access stack: PRIV's
// pod asked for an untagged shared secret with the reason "carol's unannounced PRIV acquisition:
// rotate the vendor key", and alice, outside PRIV, loaded its approval page (HTTP 200, reason shown)
// and approved it (request.granted, decided_by alice@example.com); the pod's PRIV_HUMAN page told
// carol "PRIV, private, which the session does not name".
func TestAPrivateProjectsPodRequestIsDecidedListedAndShownOnlyToItsPeople(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	set, err := exchange.NewSet([]exchange.AccountEntry{{Account: fixtureSubject, Project: privProject, Person: insider, Issuers: []string{""}}}, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	m.Exchange = set
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-priv-"+t.Name())

	plain, err := m.Create(t.Context(), pod, signRequest(t, m, podKey, "carol's unannounced PRIV acquisition: rotate the vendor key", "SHARED_KEY"), "", nil)
	if err != nil || plain.RecordID == nil {
		t.Fatalf("PRIV's pod: Create(SHARED_KEY) = %+v, %v; want pending", plain, err)
	}
	tagged, err := m.Create(t.Context(), pod, signRequest(t, m, podKey, "deploy", "PRIV_HUMAN"), "", nil)
	if err != nil || tagged.RecordID == nil {
		t.Fatalf("PRIV's pod: Create(PRIV_HUMAN) = %+v, %v; want pending", tagged, err)
	}
	records := map[string]string{*plain.RecordID: "SHARED_KEY, untagged, reason naming no issue", *tagged.RecordID: "PRIV_HUMAN"}

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
	outsiders, insiders := listed(outsider), listed(insider)
	for id, what := range records {
		if slices.Contains(outsiders, id) {
			t.Errorf("%s's pending list holds PRIV's pod's request for %s (record %s); want it left out", outsider, what, id)
		}
		if !slices.Contains(insiders, id) {
			t.Errorf("%s's pending list lacks PRIV's pod's request for %s (record %s)", insider, what, id)
		}
		detail, err := m.ReadRecord(t.Context(), id)
		if err != nil {
			t.Fatalf("ReadRecord(%s): %v", id, err)
		}
		if !slices.Contains(detail.SessionProjects, privProject) {
			t.Errorf("the record of PRIV's pod's request for %s names the session's projects %v; want PRIV, which its exchange entry names", what, detail.SessionProjects)
		}
		if err := m.HideUnseen(t.Context(), id, outsider); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("HideUnseen(PRIV's pod's request for %s, %s) = %v; want pgx.ErrNoRows", what, outsider, err)
		}
		if err := m.HideUnseen(t.Context(), id, insider); err != nil {
			t.Errorf("HideUnseen(PRIV's pod's request for %s, %s) = %v; want nil", what, insider, err)
		}
	}
	if decision, err := m.ApplyDecision(t.Context(), *plain.RecordID, true, outsider); !errors.Is(err, pgx.ErrNoRows) || errors.Is(err, record.ErrNotApprover) {
		t.Errorf("an approval by %s, outside PRIV, of PRIV's pod's request for SHARED_KEY = %+v, %v; want pgx.ErrNoRows alone, as for an id no record has", outsider, decision, err)
	}
}

// recordIssuer sets the URL of the issuer enrollmentID's pod enrolled under, as Create records the
// issuer whose token proved the pod (enrollments.issuer); nil records none, as an enrollment made
// before enrollments recorded one does.
func recordIssuer(t *testing.T, m *Machine, enrollmentID string, issuer *string) {
	t.Helper()
	if _, err := m.Store.Pool.Exec(t.Context(), `update enrollments set issuer=$2 where id=$1`, enrollmentID, issuer); err != nil {
		t.Fatalf("record the pod's issuer: %v", err)
	}
}

// TestAServicesPodNamesItsEntrysProjectOnlyForAnIssuerTheEntryNames: a service's pod names the
// project its account's BROKER_EXCHANGE_ACCOUNTS entry maps only while that entry names the issuer
// whose token enrolled the pod, which the enrollment records by URL and the broker reads as that
// issuer's name in BROKER_K8S_OIDC_ISSUER (Machine.Issuers): the same account name in another
// cluster is another ServiceAccount, whose token the exchange refuses too (exchange.Set.Pod). The
// broker trusts two issuers, dispatch and us-central1, and the entry maps the pod's account to
// PRIV. A pod that names PRIV is given PRIV_KEY at once, and the record of its request for the
// untagged SHARED_KEY names PRIV among its session's projects; a pod that names no project is
// denied PRIV_KEY project-not-named, and its record names no project. An enrollment that records
// no issuer, made before enrollments recorded one, is the first issuer's, and one that records an
// issuer the setting no longer holds is no issuer's.
func TestAServicesPodNamesItsEntrysProjectOnlyForAnIssuerTheEntryNames(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	first := oidc.NamedIssuer{Name: "dispatch", URL: "https://kubernetes.dispatch.example"}
	second := oidc.NamedIssuer{Name: "us-central1", URL: "https://container.googleapis.com/v1/projects/example/locations/us-central1/clusters/example"}
	m.Issuers = []oidc.NamedIssuer{first, second}
	for i, c := range []struct {
		name string
		// enrolledWith is the URL the pod's enrollment records, nil for none.
		enrolledWith *string
		// issuers are the issuer names the pod account's entry names.
		issuers []string
		names   bool
	}{
		{"the second issuer's pod, while its entry names both", &second.URL, []string{first.Name, second.Name}, true},
		{"the second issuer's pod, once its entry names the first alone", &second.URL, []string{first.Name}, false},
		{"a pod recording no issuer, while its entry names the first", nil, []string{first.Name}, true},
		{"a pod recording no issuer, while its entry names the second alone", nil, []string{second.Name}, false},
		{"a pod of an issuer the setting no longer holds", new("https://gone.example"), []string{first.Name, second.Name}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			set, err := exchange.NewSet([]exchange.AccountEntry{{Account: fixtureSubject, Project: privProject, Person: insider, Issuers: c.issuers}}, nil, nil, nil, "")
			if err != nil {
				t.Fatalf("exchange.NewSet: %v", err)
			}
			m.Exchange = set
			pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-issuer-"+strconv.Itoa(i))
			recordIssuer(t, m, pod, c.enrolledWith)

			request, err := m.Create(t.Context(), pod, signRequest(t, m, podKey, "deploy", "PRIV_KEY"), "", nil)
			if c.names && (err != nil || request.GrantID == nil) {
				t.Fatalf("Create(PRIV_KEY) from a pod naming PRIV = %+v, %v; want a grant at once", request, err)
			}
			if !c.names {
				deniedFor(t, "a request for PRIV_KEY from a pod naming no project", request, err, "PRIV_KEY", policy.ReasonProjectNotNamed)
			}
			plain, err := m.Create(t.Context(), pod, signRequest(t, m, podKey, "rotate the vendor key", "SHARED_KEY"), "", nil)
			if err != nil || plain.RecordID == nil {
				t.Fatalf("Create(SHARED_KEY) = %+v, %v; want pending", plain, err)
			}
			detail, err := m.ReadRecord(t.Context(), *plain.RecordID)
			if err != nil {
				t.Fatalf("ReadRecord(%s): %v", *plain.RecordID, err)
			}
			if named := slices.Contains(detail.SessionProjects, privProject); named != c.names {
				t.Fatalf("the record of the pod's request for SHARED_KEY names the session's projects %v; want PRIV among them: %v", detail.SessionProjects, c.names)
			}
		})
	}
}
