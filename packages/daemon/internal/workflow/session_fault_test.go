package workflow

// A worker that lost its session, and a worker no start can reach because the session store
// cannot be asked, are both reported to the people who can act on them: dispatch://INFRA-714 R8.

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

func seedTestingIssue(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: key, Project: "LEGION", Title: key, Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true})
}

// controllerNoticeKinds is the kind of every notice the outbox holds for the project's controller.
func controllerNoticeKinds(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return outboxStrings(t, pool, "select payload->>'kind' from outbox where kind = 'controller_notice' and issue = $1 order by id", "LEGION-209")
}

// A forgotten session is posted on the issue and told to its architect and to the controller: the
// worker is running, so nothing is held, and the report is the whole point. Its cause decides the
// words of the message and the notice's kind and fields. A session no store holds is told as lost,
// as it always was: a session-lost notice of the role and the phase with the runtime's account
// (detail). One the pod's launcher could not look up by its id, and Oh My Pi did not resume by that
// id, is told as forgotten, with that reason in the message, and nothing says it was lost
// (dispatch://INFRA-4108 R3): the notice's kind is part of what the architect and the controller
// hear, since the outbox publishes each notice as `<kind> on <issue>`, so it is a session-forgotten
// notice of the role and the phase that names the session forgotten and carries no detail, and no
// notice row says lost. What each kind means, the fresh agent among it, is its readers' prompts'.
func TestALostSessionIsToldOnTheIssueToItsArchitectAndToTheController(t *testing.T) {
	const session = "01a1f5de-4835-7667-b005-e99e3e17edd3"
	for _, row := range []struct {
		name   string
		fact   intake.SessionLost
		kind   texts.Kind
		says   []string
		denies []string
		// session is the session the notice names as a field of its own, and detail what its
		// detail holds; "" is a notice with none.
		session, detail string
	}{
		{
			name: "gone",
			fact: intake.SessionLost{Issue: "LEGION-209", Role: claim.RoleTester, Cause: "gone", Session: "ses-abc",
				Detail: "the session ses-abc this claim recorded is in no session store the runtime could resume it from"},
			kind:   texts.SessionLost,
			says:   []string{"ses-abc", "lost the session it recorded", "fresh agent"},
			detail: "ses-abc",
		},
		{
			name: "not-resumed-by-id",
			fact: intake.SessionLost{Issue: "LEGION-209", Role: claim.RoleTester, Cause: "not-resumed-by-id", Session: session,
				Detail: "the pod's launcher, which is older than this release, could not look the session " + session +
					" up by its id, and Oh My Pi did not resume it by that id, so this claim forgot it and runs a fresh agent"},
			kind: texts.SessionForgotten,
			says: []string{
				session,
				"The tester of LEGION-209 forgot the session it recorded",
				"its pod's launcher is older than this release and could not look the session up by its id",
				"Oh My Pi did not resume it by that id",
				"The daemon deleted nothing of that session",
				"The tester is running a fresh agent",
			},
			denies:  []string{"lost", "no session store"},
			session: session,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedTestingIssue(t, pool, "LEGION-209")

			if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "session-lost:legion-legion-209-tester:3", row.fact,
				testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("apply the forgotten session: %v", err)
			}

			posted := messageBodies(t, pool)
			if len(posted) != 1 {
				t.Fatalf("the issue was told %v, want one message", posted)
			}
			for _, want := range row.says {
				if !strings.Contains(posted[0], want) {
					t.Errorf("the issue was told %q, want it to say %q", posted[0], want)
				}
			}
			payloads := outboxStrings(t, pool, "select payload::text from outbox where kind in ('notice', 'controller_notice') and issue = $1 order by id", "LEGION-209")
			for _, banned := range row.denies {
				if strings.Contains(posted[0], banned) {
					t.Errorf("the issue was told %q, which says %q: the session was not lost and a store may still hold it", posted[0], banned)
				}
				for _, payload := range payloads {
					if strings.Contains(payload, banned) {
						t.Errorf("a notice of the forget is %s, which says %q: the session was not lost and a store may still hold it", payload, banned)
					}
				}
			}
			if kinds := outboxStrings(t, pool, "select payload->>'kind' from outbox where kind = 'notice' and issue = $1 order by id", "LEGION-209"); len(kinds) != 1 || kinds[0] != string(row.kind) {
				t.Errorf("the architect was told %v, want one %s notice", kinds, row.kind)
			}
			told := noticesOfKind(t, pool, "LEGION-209", row.kind)
			if len(told) != 1 || told[0].Role != claim.RoleTester || told[0].Phase != phase.Testing || told[0].Reason != "" ||
				told[0].Session != row.session || (told[0].Detail == "") != (row.detail == "") || !strings.Contains(told[0].Detail, row.detail) {
				t.Errorf("the architect was told %+v, want one %s of the tester in testing naming the session %q and a detail holding %q", told, row.kind, row.session, row.detail)
			}
			if got := controllerNoticeKinds(t, pool); len(got) != 1 || got[0] != string(row.kind) {
				t.Errorf("the controller was told %v, want one %s notice", got, row.kind)
			}
		})
	}
}

// A session store that cannot be asked is told the same way, and holds no phase: the claim keeps
// its session and the daemon keeps trying.
func TestASessionStoreFaultIsToldOnTheIssueAndToTheController(t *testing.T) {
	pool := migratedPool(t)
	seedTestingIssue(t, pool, "LEGION-209")

	if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "session-store-fault:legion-legion-209-tester:1", intake.SessionStoreFault{
		Issue: "LEGION-209", Role: claim.RoleTester, Detail: "dial tcp 10.0.0.3:5432: connection refused",
	}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the store fault: %v", err)
	}

	posted := messageBodies(t, pool)
	if len(posted) != 1 || !strings.Contains(posted[0], "connection refused") || !strings.Contains(posted[0], "every minute") {
		t.Fatalf("the issue was told %v, want one message naming the fault and the retry", posted)
	}
	if got := controllerNoticeKinds(t, pool); len(got) != 1 || got[0] != "session-store-fault" {
		t.Fatalf("the controller was told %v, want the store fault", got)
	}
	var got phase.Phase
	if err := pool.QueryRow(t.Context(), "select phase from issues where key = 'LEGION-209'").Scan(&got); err != nil {
		t.Fatalf("read the issue: %v", err)
	}
	if got != phase.Testing {
		t.Fatalf("the issue stands in %s, want testing: a store fault holds no phase", got)
	}
}
