package requests

import (
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// TestEveryReaderReadsTheProjectsOfBothCredentials is round 6's m-d and m-f on
// cybermonklabs/legion#4135: the pending list, the record's page and the grant list read the
// projects of both credentials a request has had — the enrollment that asked and the one it hangs
// off now — as the decision does (TestADecisionChecksTheProjectsOfBothEnrollments), so a request
// the decision would refuse a person is one that person is not shown either.
func TestEveryReaderReadsTheProjectsOfBothCredentials(t *testing.T) {
	t.Run("a pending request now on a credential naming PRIV", func(t *testing.T) {
		// The request comes from a pod that names no person, so every signed-in person's list
		// reads it (everyonesPod).
		m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
		pod, podKey := everyonesPod(t, m, "pod-r-shared", "")
		req, err := m.Create(t.Context(), pod, signRequest(t, m, podKey, "rotate the shared key", "SHARED_KEY"), "")
		if err != nil || req.RecordID == nil {
			t.Fatalf("Create(SHARED_KEY) = %+v, %v; want it pending for anyone to decide", req, err)
		}
		if listed := pendingRecords(t, m, outsider); !slices.Contains(listed, *req.RecordID) {
			t.Fatalf("PendingForApprover(%s) before the move = %v; want the request, which names no project", outsider, listed)
		}
		second, _ := newEnrollment(t, m.Store, "session", "box-r-"+t.Name(), new(fixtureOperator), nil)
		nameProjects(t, m, second, privProject)
		if _, err := m.Store.Pool.Exec(t.Context(), `update requests set enrollment_id=$2 where id=$1`, req.ID, second); err != nil {
			t.Fatalf("move the request: %v", err)
		}

		if listed := pendingRecords(t, m, outsider); slices.Contains(listed, *req.RecordID) {
			t.Errorf("PendingForApprover(%s) = %v; want the request hidden: the credential it hangs off names PRIV", outsider, listed)
		}
		if listed := pendingRecords(t, m, insider); !slices.Contains(listed, *req.RecordID) {
			t.Errorf("PendingForApprover(%s) = %v; want the request: PRIV lists them", insider, listed)
		}
		detail, err := m.ReadRecord(t.Context(), *req.RecordID)
		if err != nil || !slices.Contains(detail.SessionProjects, privProject) {
			t.Fatalf("ReadRecord = %+v, %v; want its session projects to name PRIV", detail.SessionProjects, err)
		}
		if err := m.HideUnseen(t.Context(), *req.RecordID, outsider); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("HideUnseen(the moved request, %s) = %v; want pgx.ErrNoRows: the page hides it, so it offers no Approve the decision refuses", outsider, err)
		}
		if err := m.HideUnseen(t.Context(), *req.RecordID, insider); err != nil {
			t.Errorf("HideUnseen(the moved request, %s) = %v; want nil", insider, err)
		}
	})

	t.Run("a grant whose asking credential names PRIV", func(t *testing.T) {
		// The model judge approves SHARED_KEY on a pod of the fixture's service in anyone's stead,
		// so the grant names no approver and no operator and every signed-in person's list reads
		// it, and its record names the pod that asked. A grant's own approver and operator list it
		// whatever project it touches (namedOnGrant), so their lists could not show this.
		m, _, _ := newJudgeFixture(t)
		pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-g-asking-"+t.Name())
		grantID := judgedGrant(t, m, pod, podKey, 600, "SHARED_KEY")
		// An adoption moves the grant and its request to a pod of another account, one no
		// operator's entry maps to a project; the record keeps the pod that asked.
		second, _ := newServiceEnrollment(t, m.Store, fixtureService, "system:serviceaccount:example:moved", "pod-g-moved-"+t.Name())
		if _, err := m.Store.Pool.Exec(t.Context(), `with moved as (update grants set enrollment_id=$2 where id=$1 returning request_id)
			update requests set enrollment_id=$2 where id=(select request_id from moved)`, grantID, second); err != nil {
			t.Fatalf("move the grant and its request: %v", err)
		}
		answerFrom(t, m, map[string]projectaccesstest.Row{
			"INFRA":     {Visibility: "public", Owner: otherPerson},
			privProject: {Visibility: "private", Owner: otherPerson, Listed: []string{insider}},
		})
		if listed := approverGrants(t, m, outsider); !slices.Contains(listed, grantID) {
			t.Fatalf("GrantsForApprover(%s) after the move = %v; want the judged grant, which names no private project", outsider, listed)
		}
		// The asking pod's account is mapped to PRIV here directly, standing in for any way the
		// projects of the two credentials come to differ.
		set, err := exchange.NewSet([]exchange.AccountEntry{{Account: fixtureSubject, Project: privProject, Person: insider}}, nil, nil, nil, "")
		if err != nil {
			t.Fatalf("exchange.NewSet: %v", err)
		}
		m.Exchange = set

		if listed := approverGrants(t, m, outsider); slices.Contains(listed, grantID) {
			t.Errorf("GrantsForApprover(%s) = %v; want the grant hidden: the credential that asked names PRIV", outsider, listed)
		}
		if listed := approverGrants(t, m, insider); !slices.Contains(listed, grantID) {
			t.Errorf("GrantsForApprover(%s) = %v; want the grant: PRIV lists them", insider, listed)
		}
	})
}
