package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A pause that stops a claim owing it a launch writes that launch's start naming the claim it
// stopped (record.SuperviseRequest.Claim), so the start brings that claim back and no other. In the
// plan `implementer:a tester:a reviewer` the agent of key a, `…-implementer`, works the tester's
// step: a start naming no claim would reach whichever claim the record gives the tester's step
// when the row runs. The tester's own claim is held to the same rule.
func TestAPauseOwesItsLaunchToTheClaimItStopped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		agent claim.Role
	}{
		{name: "a keyed agent on the tester's step", agent: claim.RoleImplementer},
		{name: "the tester's own claim", agent: claim.RoleTester},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPauseRig(t)
			stopped := r.claim(tc.agent, supervise.StateIdle)
			stopped.Role = claim.RoleTester
			if err := r.st.PutClaim(r.ctx, stopped); err != nil {
				t.Fatalf("seed %s on the tester's step: %v", stopped.Token, err)
			}
			held := r.pauseLegion()
			stopped.State, stopped.SuspendedBy = supervise.StateSuspended, supervise.SuspendedByReleased
			r.mustStop(stopped, supervise.PauseStop{Pause: held.ID, LaunchOwed: true})

			rows, err := r.pool.Query(r.ctx, `select coalesce(payload->>'claim', '') from outbox
				where kind = 'supervise' and issue = $1 and payload->>'op' = 'start' and payload->>'role' = 'tester' order by id`, pauseIssue)
			if err != nil {
				t.Fatalf("read the tester's starts: %v", err)
			}
			defer rows.Close()
			var named []string
			for rows.Next() {
				var token string
				if err := rows.Scan(&token); err != nil {
					t.Fatal(err)
				}
				named = append(named, token)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(named) != 1 || named[0] != string(stopped.Token) {
				t.Fatalf("the starts the pause owes the tester's step name %q, want one naming the claim it stopped, %s", named, stopped.Token)
			}
		})
	}
}
