// results_ci_at_named_head_test.go holds GET /api/v1/issues/{key}/results?at= to the --at clause of
// R8 of dispatch://INFRA-4303/spec (approved v3): "Where `--at` names the head of an open or merged
// pull request of the issue, that head is the one whose CI run the check reads" (ciHeadsOf). The
// first cases are red at main ac9e87136e, where the read takes the open pull request of the
// highest number whatever commit the reader named. It also holds the heads both kinds of read
// take from one walk (headsOf), and the gate's answer at a named commit that heads a pull request
// in each of two repositories (unreadWithNoSubject).
package api

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestCIResultAtANamedHeadIsReadAtThatHead: #70 is at H1, where go-checks passed, and #71 is open
// at H2, where it failed. A read with ?at=H1 names #70's head, so the check's own CI read is H1's
// and names the result H1's run derived, no result derived at H2 is the check's, and a pass
// stands. R8 names the head of a merged pull request as it names an open one's, so the case runs
// with #70 open and with #70 merged at its final head H1 beside the open #71.
func TestCIResultAtANamedHeadIsReadAtThatHead(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		store func(rig ciRepositoryRig) func(repo string, number int, sha string)
	}{
		{"#70 open", func(rig ciRepositoryRig) func(string, int, string) { return rig.write }},
		{"#70 merged", func(rig ciRepositoryRig) func(string, int, string) { return rig.merged }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rig := newCIRepositoryRig(t)
			row.store(rig)("acme/widgets", 70, ciEachHeadH1)
			rig.write("acme/widgets", 71, ciEachHeadH2)
			rig.github.set("acme/widgets", ciEachHeadH1, completedRun(63, "go-checks", "success"))
			rig.github.set("acme/widgets", ciEachHeadH2, completedRun(64, "go-checks", "failure"))
			read := rig.readAt(t, ciEachHeadH1)

			ciReadWants(t, row.name+": c1's own read at ?at=H1", read.CI,
				map[string]any{"state": "pass", "case": "success", "commit": ciEachHeadH1, "repo": "acme/widgets", "result": "ci-run:63"})
			for _, result := range read.Results {
				if strings.HasPrefix(result.ID, ciResultIDPrefix) && result.CommitSHA != ciEachHeadH1 {
					t.Errorf("%s: c1 read at ?at=H1 holds %s, derived at %s, a head the read did not name", row.name, result.ID, result.CommitSHA)
				}
			}
			if read.PassStands.Answer != "yes" {
				t.Errorf("%s: c1 read at ?at=H1: pass_stands %q (%q), want yes", row.name, read.PassStands.Answer, read.PassStands.Reason)
			}
		})
	}
}

// TestANamedHeadIsDecidedByItsOwnCIRead: with ?at= there are no subjects, so the gate's answer
// turns on the reads at the named commit (unreadWithNoSubject). The read that decides must be the
// named head's, never the read of the open pull request of the highest number.
func TestANamedHeadIsDecidedByItsOwnCIRead(t *testing.T) {
	t.Parallel()
	t.Run("another head unread", func(t *testing.T) {
		t.Parallel()
		rig := newCIRepositoryRig(t)
		rig.write("acme/widgets", 70, ciEachHeadH1)
		rig.write("acme/widgets", 71, ciEachHeadH2)
		rig.github.set("acme/widgets", ciEachHeadH1, completedRun(63, "go-checks", "success"))
		// H2 holds no runs: the stand-in answers 422, so #71's read is unread.
		read := rig.readAt(t, ciEachHeadH1)
		if read.PassStands.Answer != "yes" {
			t.Errorf("?at=H1, whose run passed, while #71's read failed: pass_stands %q (%q), want yes",
				read.PassStands.Answer, read.PassStands.Reason)
		}
	})
	t.Run("the named head unread", func(t *testing.T) {
		t.Parallel()
		rig := newCIRepositoryRig(t)
		rig.write("acme/widgets", 70, ciEachHeadH1)
		rig.write("acme/widgets", 71, ciEachHeadH2)
		// H1 holds no runs: the stand-in answers 422, so #70's read is unread.
		rig.github.set("acme/widgets", ciEachHeadH2, completedRun(64, "go-checks", "success"))
		read := rig.readAt(t, ciEachHeadH1)
		if read.PassStands.Answer != "unknown" {
			t.Errorf("?at=H1, whose run GitHub could not be read for: pass_stands %q (%q), want unknown",
				read.PassStands.Answer, read.PassStands.Reason)
		}
	})
}

// TestANamedCommitOfTwoRepositoriesIsDecidedByTheReadOfEach: R8 where the named commit is the head
// of acme/widgets#70 and of a pull request of acme/infra. Each head is read in its own repository,
// and go-checks passed at acme/widgets'. Where GitHub cannot be read for acme/infra's, a pass at
// the named commit does not stand: ?at= reads unknown for read_failed, as the read that names no
// commit reads unknown, and it does so whichever pull request's number is higher. The control:
// where acme/infra's head holds no run of go-checks, its read derives nothing and both reads read
// yes.
func TestANamedCommitOfTwoRepositoriesIsDecidedByTheReadOfEach(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name                  string
		infraNumber           int
		infraRuns             []map[string]any
		wantNamed, wantReason string
		wantUnnamed           string
	}{
		{"acme/infra#9 unread beside acme/widgets#70's pass", 9, nil, "unknown", "read_failed", "unknown"},
		{"acme/infra#90 unread beside acme/widgets#70's pass", 90, nil, "unknown", "read_failed", "unknown"},
		{"the control: acme/infra#9 holds no run of go-checks", 9, []map[string]any{completedRun(65, "lint", "success")}, "yes", "", "yes"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rig := newCIRepositoryRig(t)
			rig.write("acme/widgets", 70, ciRepositoryShared)
			rig.write("acme/infra", row.infraNumber, ciRepositoryShared)
			rig.github.set("acme/widgets", ciRepositoryShared, completedRun(63, "go-checks", "success"))
			if row.infraRuns != nil {
				rig.github.set("acme/infra", ciRepositoryShared, row.infraRuns...)
			}
			named := rig.readAt(t, ciRepositoryShared)
			if named.PassStands.Answer != row.wantNamed || named.PassStands.Reason != row.wantReason {
				t.Errorf("%s: ?at= the shared head reads %q (%q), want %q (%q)",
					row.name, named.PassStands.Answer, named.PassStands.Reason, row.wantNamed, row.wantReason)
			}
			if unnamed := rig.read(t); unnamed.PassStands.Answer != row.wantUnnamed {
				t.Errorf("%s: the read that names no commit reads %q (%q), want %q",
					row.name, unnamed.PassStands.Answer, unnamed.PassStands.Reason, row.wantUnnamed)
			}
		})
	}
}

// TestTheGateReadsEveryHeadAtANamedCommit: the gate's rule alone (passStandsOf), for a case a route
// read cannot set up: at a named commit that heads a pull request in two repositories, the check's
// own `ci` is the read of acme/widgets', which passed, and acme/infra's read is unread, by a read
// of GitHub that did not answer (github_unreadable) or by Dispatch's own read of the repository
// the issue resolves to, which says whether the CI job's result there takes the run's place
// (read_failed, jobAtHeadUnresolved). Either leaves the check unknown for read_failed. The
// control: acme/infra's read holds no result, and the pass stands.
func TestTheGateReadsEveryHeadAtANamedCommit(t *testing.T) {
	t.Parallel()
	moved := false
	pass := model.CheckResult{ID: "ci-run:63", Verdict: "pass", CommitSHA: ciRepositoryShared, Role: ciResultRole,
		CommitStands: &model.ResultCommitStands{Answer: model.CommitAtBasis}, CheckWordsMoved: &moved}
	widgets := &model.CheckCI{Run: "go-checks", State: model.CIStatePass, Case: ciConclusionSuccess,
		Commit: ciRepositoryShared, Repo: "acme/widgets", Result: pass.ID}
	for _, row := range []struct {
		state, ciCase      string
		answer, wantReason string
	}{
		{model.CIStateUnread, ciCaseUnreadable, model.PassStandsUnknown, model.PassStandsReasonReadFailed},
		{model.CIStateUnread, ciCaseReadFailed, model.PassStandsUnknown, model.PassStandsReasonReadFailed},
		{model.CIStateNoResult, ciCaseAbsent, model.PassStandsYes, ""},
	} {
		infra := &model.CheckCI{Run: "go-checks", State: row.state, Case: row.ciCase, Commit: ciRepositoryShared, Repo: "acme/infra"}
		check := model.CheckBlock{BlockID: "c1", When: docs.CheckWhenBeforeMerge, CI: widgets, Results: []model.CheckResult{pass}}
		compared := comparison{
			bases: []model.CheckBasis{{Kind: model.CheckBasisNamed, Commit: ciRepositoryShared}},
			ci: map[ciHeadKey]*model.CheckCI{
				ciHeadKeyOf("acme/widgets", ciRepositoryShared): widgets,
				ciHeadKeyOf("acme/infra", ciRepositoryShared):   infra,
			},
		}
		stands := passStandsOf(check, compared)
		if stands.Answer != row.answer || stands.Reason != row.wantReason {
			t.Errorf("acme/infra's read %s (%s) at the named commit: pass_stands %q (%q), want %q (%q)",
				row.state, row.ciCase, stands.Answer, stands.Reason, row.answer, row.wantReason)
		}
	}
}

// TestTwoPullRequestsOfOneRepositoryAtOneHeadReadAsTheHigherNumber: #72 and #71 of acme/widgets
// are open at H1, where go-checks passed, and Dispatch learned #71 last, so it lists #71 first
// (delivery.Attributor.HeadsAttributedTo). One commit of one repository is read once, as the head
// of the pull request of the higher number, whether the read names that commit or none (headsOf):
// the check's `ci` names acme/widgets#72 both ways.
func TestTwoPullRequestsOfOneRepositoryAtOneHeadReadAsTheHigherNumber(t *testing.T) {
	t.Parallel()
	rig := newCIRepositoryRig(t)
	rig.write("acme/widgets", 72, ciEachHeadH1)
	rig.write("acme/widgets", 71, ciEachHeadH1)
	rig.learned("acme/widgets", 71, 72)
	rig.github.set("acme/widgets", ciEachHeadH1, completedRun(63, "go-checks", "success"))
	want := fmt.Sprintf("CI run go-checks concluded success at %s, the head of acme/widgets#72.", ciEachHeadH1)
	for _, each := range []struct {
		what string
		read ciRepositoryRead
	}{
		{"the read that names no commit", rig.read(t)},
		{"?at=H1", rig.readAt(t, ciEachHeadH1)},
	} {
		if detail := each.read.CI["detail"]; detail != want {
			t.Errorf("%s: c1's own read says %q, want %q", each.what, detail, want)
		}
	}
}

// TestANamedCommitThatIsNoHeadReadsAsBefore: where ?at= names a commit no pull request of the issue
// has as its head, R8 leaves the read as it is: the check's own CI read is the open pull request
// of the highest number, #71 at H2.
func TestANamedCommitThatIsNoHeadReadsAsBefore(t *testing.T) {
	t.Parallel()
	rig := newCIRepositoryRig(t)
	rig.write("acme/widgets", 70, ciEachHeadH1)
	rig.write("acme/widgets", 71, ciEachHeadH2)
	rig.github.set("acme/widgets", ciEachHeadH1, completedRun(63, "go-checks", "success"))
	rig.github.set("acme/widgets", ciEachHeadH2, completedRun(64, "go-checks", "failure"))
	read := rig.readAt(t, ciEachHeadH3)

	ciReadWants(t, "c1's own read at ?at=H3, no head of the issue", read.CI,
		map[string]any{"state": "fail", "case": "failure", "commit": ciEachHeadH2, "repo": "acme/widgets", "result": "ci-run:64"})
}
