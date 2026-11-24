package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A hook's prompt to a role the deployment defines reaches the worker on that role's step: at a
// step the owner works (`critic:owner`), the owner's own claim (record.OwnerClaim), whatever the
// role's phases row names. The row can name the architect above, which the start writes there while
// the issue's own architect holds no role (ownerAgent), and which does not do the step, as a hook's
// prompt to one of Legion's roles of a step reaches the owner where that step is the owner's
// (workAgent). LEGION-209, a child back at the critic's step of `implementer critic:owner
// acceptance` from its acceptance, whose critic row names LEGION-208's architect: the firing of a
// hook that names the critic prompts LEGION-209's own architect.
//
// The controls: the same child whose critic row names its own architect, which the prompt reaches
// too, so the first case fails only where the prompt follows the row to the architect above; and
// the child at the step of a fresh critic (`implementer critic acceptance`), whose row names an
// agent of its own, which the prompt reaches, so a role's row still names the worker of a step the
// owner does not work.
func TestAHookPromptToADeploymentRoleAtTheOwnersStepReachesTheOwnerWhateverItsRowNames(t *testing.T) {
	childOwner := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleArchitect))
	above := claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleArchitect))
	implementer := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleImplementer))
	acceptance := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleAcceptance))
	fresh, err := claim.AgentToken("legion", "LEGION-209", "critic-2")
	if err != nil {
		t.Fatalf("the token of a fresh critic: %v", err)
	}
	const critic claim.Role = "critic"
	for _, tc := range []struct {
		name  string
		words []string
		row   claim.Token
		want  claim.Token
	}{
		{name: "the owner's step, its row naming the architect above", words: []string{"implementer", "critic:owner", "acceptance"}, row: above, want: childOwner},
		{name: "control: the owner's step, its row naming the owner", words: []string{"implementer", "critic:owner", "acceptance"}, row: childOwner, want: childOwner},
		{name: "control: a fresh critic's step", words: []string{"implementer", "critic", "acceptance"}, row: fresh, want: fresh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := childStandsAt(t, phase.Step, 2, map[string]string{string(critic): "review"}, tc.words, []claim.Token{implementer, tc.row, acceptance})
			engine := returnEngine()
			hook := Hook{Name: "critic-loop", On: HookEventWorkBack, Every: 1, Roles: []claim.Role{critic}, Prompt: "Say what the round missed."}
			onStored(t, pool, engine, "LEGION-209", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
				return engine.fireHook(ctx, tx, issue, hook, phase.Accepting, phase.Step, 1, map[string]int{phase.Pair(phase.Accepting, phase.Step): 1}, nil)
			})
			prompts := hookPrompts(t, pool)
			if len(prompts) != 1 || prompts[0].Role != critic || prompts[0].Claim != tc.want {
				t.Fatalf("the firing prompted %+v, want one prompt to the critic on the claim %s", prompts, tc.want)
			}
		})
	}
}
