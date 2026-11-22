package requests

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestADecisionChecksTheProjectsOfBothEnrollments is round 5's H1 on cybermonklabs/legion#4135.
// The pending list and the approval page read the projects of the enrollment that asked, which
// the append-only record names, while the decision reads the enrollment the request hangs off
// now, which an adoption rewrites. A request the page would show a person under one of the two
// lists and hide under the other is decided under both: a person outside a private project either
// enrollment names is refused as for a record id no record has, and the request is left pending,
// while a person the project lists decides it as before.
func TestADecisionChecksTheProjectsOfBothEnrollments(t *testing.T) {
	for _, c := range []struct {
		name       string
		asked, now []string
	}{
		{"the enrollment that asked names PRIV and the one it hangs off now names none", []string{privProject}, nil},
		{"the enrollment that asked names none and the one it hangs off now names PRIV", nil, []string{privProject}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
			nameProjects(t, m, enr, c.asked...)
			req, err := m.Create(t.Context(), enr, signRequest(t, m, key, "rotate the shared key", "SHARED_KEY"), "", nil)
			if err != nil || req.State != "pending" || req.RecordID == nil {
				t.Fatalf("Create(SHARED_KEY) = %+v, %v; want it pending for anyone to decide", req, err)
			}
			second, _ := newEnrollment(t, m.Store, "session", "box-q-"+t.Name(), new(fixtureOperator), nil)
			nameProjects(t, m, second, c.now...)
			if _, err := m.Store.Pool.Exec(t.Context(), `update requests set enrollment_id=$2 where id=$1`, req.ID, second); err != nil {
				t.Fatalf("move the request to the second enrollment: %v", err)
			}

			if dec, err := m.ApplyDecision(t.Context(), *req.RecordID, true, outsider); !errors.Is(err, pgx.ErrNoRows) || errors.Is(err, record.ErrNotApprover) {
				t.Fatalf("%s's approval = %+v, %v; want pgx.ErrNoRows alone, as for an id no record has: PRIV does not list them", outsider, dec, err)
			}
			if got := readDecided(t, m, *req.RecordID); got.state != "pending" || got.events != 0 {
				t.Fatalf("after the refusal = %+v; want the request untouched and pending", got)
			}
			if dec, err := m.ApplyDecision(t.Context(), *req.RecordID, true, insider); err != nil || dec.State != "granted" {
				t.Fatalf("%s's approval = %+v, %v; want it granted: PRIV lists them", insider, dec, err)
			}
		})
	}
}
