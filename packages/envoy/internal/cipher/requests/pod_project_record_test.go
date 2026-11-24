package requests

import (
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestAPrivateProjectsPodRequestIsDecidedListedAndShownOnlyToItsPeople: the production path of a
// private project's agent is a pod whose account an operator's entry maps to the project
// (BROKER_EXCHANGE_ACCOUNTS); its enrollment's own projects column stays empty, and its projects
// come from that entry (Exchange.MappedProjects), as the release path already reads them. Each of
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
	set, err := kubernetesPolicy([]kubernetes.AccountEntry{{Account: fixtureSubject, Project: privProject, Person: insider}})
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	m.Exchange = set
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-priv-"+t.Name())

	plain, err := m.Create(t.Context(), pod, signRequest(t, m, podKey, "carol's unannounced PRIV acquisition: rotate the vendor key", "SHARED_KEY"), "")
	if err != nil || plain.RecordID == nil {
		t.Fatalf("PRIV's pod: Create(SHARED_KEY) = %+v, %v; want pending", plain, err)
	}
	tagged, err := m.Create(t.Context(), pod, signRequest(t, m, podKey, "deploy", "PRIV_HUMAN"), "")
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
