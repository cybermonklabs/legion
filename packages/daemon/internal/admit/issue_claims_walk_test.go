package admit

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A re-admitted member mid-testing whose stored claims include `…-implementer-2`, an agent whose
// name is no role, with the stored role implementer, gets one start that names that claim with
// that role and carries no task, as `…-implementer` beside it gets one naming it. A walk that
// reads the member's claims by the six roles' tokens finds `…-implementer` alone and passes over
// `…-implementer-2`, whose session the re-admitted tree then never brings back.
func TestAReadmittedMemberBringsBackAnAgentWhoseNameIsNoRole(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, member = "LEGION-208", "LEGION-209"
	putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: "root", Tree: root, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A", LastDispatchSeq: 1})
	parent := root
	putIssue(t, pool, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: "member", Tree: root, Parent: &parent, Phase: phase.Testing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "B", LastDispatchSeq: 1})
	implementer, second := keyedToken(t, member, "implementer"), keyedToken(t, member, "implementer-2")
	for _, token := range []claim.Token{implementer, second} {
		seedStoredClaim(t, pool, root, member, token, claim.RoleImplementer, "suspended", "daemon")
	}

	apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "A", HandedOver: handed}, engine)

	reached := startsReaching(t, pool, member)
	for _, token := range []claim.Token{implementer, second} {
		got := reached[token]
		if len(got) != 1 || got[0].Claim != token || got[0].Role != claim.RoleImplementer || got[0].Task != "" {
			t.Errorf("the starts that reach %s = %+v, want one start naming it with its stored role implementer and no task", token, got)
		}
	}
}
