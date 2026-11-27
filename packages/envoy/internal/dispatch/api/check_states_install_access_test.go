// check_states_install_access_test.go holds the settler's check of the former heads the install
// seeded (verifyFormerHeads) to the one rule that may delete a candidate: a commit list GitHub
// answered. The rig is check_states_test.go's; the candidates are seedInstallCandidates'.
package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAFormerHeadTheInstallSeededOutlastsAReadGitHubDidNotAnswer: the tester recorded k1 passing at
// H1 before the install, #70 is now at H2, and the install seeded H1. The first settle's read of
// #70's commits gets no list: GitHub answers 502, or 404 (a repository or pull request the App
// cannot see right now), or the deployment holds no App key. Each time H1 stays an unverified
// candidate and nothing is published there. Where a retry can answer (502, 404) the issue keeps
// a mark due after the backoff; with no App key, which no retry answers
// (delivery.MembershipUnreadable), it keeps none, so no test handoff waits for the read. Once
// GitHub answers and the issue is owed again, the settle reads #70's commits, which list H1, and
// publishes k1 yes at H1.
func TestAFormerHeadTheInstallSeededOutlastsAReadGitHubDidNotAnswer(t *testing.T) {
	for _, failure := range []string{"502", "404", "no App key"} {
		t.Run(failure, func(t *testing.T) {
			rig := newCheckStatesRig(t, checkStatesTwoChecks)
			eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
			upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
			forgetCandidates(t, rig)
			settleOwed(t, rig)
			rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
			seedInstallCandidates(t, rig, eachHeadH1)
			client := rig.served.deps.GitHub
			switch failure {
			case "502":
				rig.github.failPullCommits(http.StatusBadGateway)
			case "404":
				rig.github.failPullCommits(http.StatusNotFound)
			case "no App key":
				rig.served.deps.GitHub = nil
			}
			settleOwed(t, rig)
			if got := candidatesOf(t, rig); len(got) != 1 || got[eachHeadH1] {
				t.Fatalf("#70 holds the former heads %v after a read of its commits that got no list (%s), want H1, unverified", got, failure)
			}
			if _, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k1"); held {
				t.Fatalf("k1 is published at H1, which no read of #70's commits has confirmed (%s)", failure)
			}
			var later int
			if err := rig.database.Pool.QueryRow(context.Background(), `
				select count(*) from check_states_owed where issue_key = $1 and due_at > now() + interval '10 seconds'`, rig.issue).Scan(&later); err != nil {
				t.Fatalf("read the marks of %s: %v", rig.issue, err)
			}
			want := 1
			if failure == "no App key" {
				want = 0
			}
			if later != want {
				t.Fatalf("%s holds %d marks due after the backoff after a read that got no list (%s), want %d", rig.issue, later, failure, want)
			}

			rig.served.deps.GitHub = client
			rig.github.failPullCommits(0)
			if _, err := rig.database.Pool.Exec(context.Background(), `select check_states_owe($1)`, rig.issue); err != nil {
				t.Fatalf("mark %s owed: %v", rig.issue, err)
			}
			settleOwed(t, rig)
			if row, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k1"); !held || row.state == nil ||
				*row.state != model.PassStandsYes {
				t.Fatalf("k1 at H1 once GitHub lists #70's commits again is published as %s, want yes (%s)", publishedLine(row), failure)
			}
		})
	}
}
