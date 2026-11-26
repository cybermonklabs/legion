package api

import (
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// results_pass_stands.go answers the gate's question for each check of a results read: whether a
// pass of it stands (passStandsOf), the state of each head a pass must stand at (eachSubjectStands,
// subjectState), and the commits at which a pass and a fail of a `before_merge` check count
// (passAndFailCommits). It is a calculation over what comparisonReader.answer has already placed,
// and reads no database and no GitHub.

// standingCommit is where a result's commit stands while the check's words have not moved
// and no retraction stands against it. It answers no standing without both comparison fields, and
// none for a result that counts for nothing (model.CheckResult.NotCounted).
func standingCommit(result model.CheckResult) string {
	if result.Retracted != nil || result.NotCounted != "" || result.CheckWordsMoved == nil || *result.CheckWordsMoved || result.CommitStands == nil {
		return ""
	}
	return result.CommitStands.Answer
}

// passStandsOf is the one line that answers the gate's question for a check: whether a pass of
// it stands at what it compared with, against the check's words now, and from which roles.
//
// A pass stands when its commit is at one of the bases, the check's words have not moved since
// it was recorded, and no retraction stands against it. The roles are those of the passes that
// stand, in the order the results come, which is newest first. A `before_merge` check compared
// with heads asks that of each head, its subjects (headBases), and a pass stands only where one
// stands at each (eachSubjectStands): one head's fail is never hidden by a pass at another.
//
// R4: a commit cannot change, so it holds one answer. Where a `before_merge` check holds a pass
// and a fail that count at one commit, in either order and from any role, no pass stands there,
// and the state is unknown for model.PassStandsReasonPassAndFail (passAndFailCommits). A later
// pass there does not end that state; a retraction of the wrong result does. This holds with and
// without subjects, so a read at one commit (`?at=`) gives the same answer.
//
// A `before_merge` check read at one commit a reader named (`?at=`) answers as a subject at that
// commit does (namedCommitStands). So a reader of one head, such as the close of an issue read at
// its newest merge's final head, tells a fail there from no result there.
//
// A check whose repository Dispatch could not read answers unknown, since it could compare
// nothing at all. So does a check CI proves with no subjects whose check runs GitHub could not be
// read for, for model.PassStandsReasonReadFailed: it holds no result, and the read that would have
// made one failed. A check CI proves with subjects asks that of each subject alone, at its own head
// (subjectState). So does a check whose standing pass is held up only by a commit Dispatch could
// not place. A `before_merge` check of an issue whose merged pull requests Dispatch cannot order
// (comparison.unordered) answers unknown for model.PassStandsReasonReadFailed too: it names no
// subject, since it cannot tell which merged head is the newest, and a pass at any of them is no
// pass that stands. A check that compared with nothing because there is no pull request and no
// branch answers no: there is nothing for a pass to stand at, and that is a fact about the work
// rather than a failed read.
func passStandsOf(check model.CheckBlock, compared comparison) model.CheckPassStands {
	if compared.live {
		return liveResultStands(check, compared)
	}
	if check.When == checkWhenAfterMerge || check.When == checkWhenDefaultBranch {
		return newestResultStands(check, compared)
	}
	if compared.reason == model.ReasonRepositoryUnreadable {
		return model.CheckPassStands{Answer: model.PassStandsUnknown}
	}
	if (len(compared.subjects) == 0 && check.CI != nil && check.CI.State == model.CIStateUnread) || compared.unordered {
		return model.CheckPassStands{Answer: model.PassStandsUnknown, Reason: model.PassStandsReasonReadFailed}
	}
	conflicted := passAndFailCommits(check)
	if check.When == docs.CheckWhenBeforeMerge {
		if named, ok := compared.namedCommit(); ok {
			return namedCommitStands(check, named, conflicted)
		}
		if len(compared.subjects) > 0 {
			return eachSubjectStands(check, compared, conflicted)
		}
	}
	var roles []string
	unknown, conflict := false, false
	for _, result := range check.Results {
		if result.Verdict != "pass" {
			continue
		}
		switch standingCommit(result) {
		case model.CommitAtBasis:
			if conflicted[result.CommitSHA] {
				conflict = true
				continue
			}
			if !slices.Contains(roles, result.Role) {
				roles = append(roles, result.Role)
			}
		case model.CommitUnknown:
			unknown = true
		}
	}
	switch {
	case len(roles) > 0:
		return model.CheckPassStands{Answer: model.PassStandsYes, Roles: roles}
	case conflict:
		return model.CheckPassStands{Answer: model.PassStandsUnknown, Reason: model.PassStandsReasonPassAndFail}
	case unknown:
		return model.CheckPassStands{Answer: model.PassStandsUnknown}
	}
	return model.CheckPassStands{Answer: model.PassStandsNo}
}

// namedCommitStands is passStandsOf for a `before_merge` check read at one commit a reader named
// (`?at=`): the state that commit has as a subject (subjectState), so a reader of that one head
// reads what a reader of each head reads there. It is yes with the roles whose pass stands there,
// no for model.PassStandsReasonFail where only a fail stands there and for
// model.PassStandsReasonNoResult where nothing does, and unknown for
// model.PassStandsReasonPassAndFail where a pass and a fail count there (R4). Results names the
// results that decide it, as the subject names them, and Commit is the named commit, absent where
// Results is empty. A read at one commit places each result at that commit or at another
// (commitStands), so no pass there is held up by a commit Dispatch could not place. The check's
// `ci` is read at the heads a read naming no commit selects, not at the named commit, so the
// subject takes none: a result derived from a run counts there only where its head is that commit.
func namedCommitStands(check model.CheckBlock, basis model.CheckBasis, conflicted map[string]bool) model.CheckPassStands {
	subject := subjectState(check, basis, conflicted, nil)
	stands := model.CheckPassStands{Answer: subject.Answer, Reason: subject.Reason, Roles: subject.Roles, Results: subject.Results}
	if len(stands.Results) > 0 {
		stands.Commit = basis.Commit
	}
	return stands
}

// eachSubjectStands is passStandsOf for a `before_merge` check compared with heads: yes only
// where a pass stands at each subject; no where one subject holds only a fail or nothing; unknown
// where none is no and one is unknown (R7), or where a branch head could not be read, or where a
// pass is held up only by a commit Dispatch could not place. Every answer names each subject's
// state. A yes names a role only where each subject names it (R6), in the order of the first
// subject: two roles that passed at two heads are no role that passed at each. An unknown answer
// takes the reason model.PassStandsReasonPassAndFail where any subject holds a pass and a fail at
// its commit, whatever the order of the subjects, and a no takes no reason: a subject's
// read_failed is the subject's alone.
func eachSubjectStands(check model.CheckBlock, compared comparison, conflicted map[string]bool) model.CheckPassStands {
	stands := model.CheckPassStands{Answer: model.PassStandsYes, Subjects: make([]model.PassStandsSubject, 0, len(compared.subjects))}
	for _, head := range compared.subjects {
		subject := subjectState(check, head.basis, conflicted, compared.ci[ciHeadKeyOf(head.repo, head.basis.Commit)])
		stands.Subjects = append(stands.Subjects, subject)
		switch {
		case subject.Answer == model.PassStandsNo:
			stands.Answer = model.PassStandsNo
		case subject.Answer == model.PassStandsUnknown && stands.Answer == model.PassStandsYes:
			stands.Answer = model.PassStandsUnknown
		}
	}
	if stands.Answer == model.PassStandsUnknown && slices.ContainsFunc(stands.Subjects, func(subject model.PassStandsSubject) bool {
		return subject.Reason == model.PassStandsReasonPassAndFail
	}) {
		stands.Reason = model.PassStandsReasonPassAndFail
	}
	placedNowhere := slices.ContainsFunc(check.Results, func(result model.CheckResult) bool {
		return result.Verdict == "pass" && standingCommit(result) == model.CommitUnknown
	})
	// A head Dispatch could not read is a subject whose state is unknown, so every subject it
	// read passing is not yet a yes; and a pass held up only by a commit Dispatch could not place
	// may stand at that head, so a no for want of a pass is not yet a no.
	switch {
	case stands.Answer == model.PassStandsYes && compared.unread != "",
		stands.Answer == model.PassStandsNo && placedNowhere:
		stands.Answer, stands.Reason = model.PassStandsUnknown, ""
	}
	if stands.Answer != model.PassStandsYes {
		return stands
	}
	for _, role := range stands.Subjects[0].Roles {
		if !slices.ContainsFunc(stands.Subjects, func(subject model.PassStandsSubject) bool {
			return !slices.Contains(subject.Roles, role)
		}) {
			stands.Roles = append(stands.Roles, role)
		}
	}
	return stands
}

// subjectState is the state of one subject of a check: yes where a pass stands at its commit,
// no where only a fail stands there (model.PassSubjectReasonFail) or nothing does
// (model.PassSubjectReasonNoResult), and unknown where a pass and a fail count at that commit
// (R4, passAndFailCommits), with the newest pass and the newest fail there as its results. It
// walks every result that counts at the commit, newest first, so a yes names each role whose pass
// counts there and the newest pass of each, and a no for a fail names the newest fail
// (model.PassStandsSubject). ci is the read of a check CI proves at the subject's head, in the
// repository of that head (deriveCIResults), nil for every other check and for a head it was not
// read at: the subject carries it, and where GitHub could not be read there, the subject's state is
// unknown (model.PassSubjectReasonReadFailed), since the run it would have read may decide either
// way. The results that count at the commit count whatever repository derived them: a commit
// holds one answer (R4), so a pass of one repository and a fail of another at one commit make it
// not known. A yes or a no names, of the results another head's run derived at the commit, none
// of the verdict this head's own run derived (derivedAtAnotherHead): the head names its own.
func subjectState(check model.CheckBlock, basis model.CheckBasis, conflicted map[string]bool, ci *model.CheckCI) model.PassStandsSubject {
	state := model.PassStandsSubject{Basis: basis, Answer: model.PassStandsNo, Reason: model.PassSubjectReasonNoResult, CI: ci}
	if conflicted[basis.Commit] {
		state.Answer, state.Reason = model.PassStandsUnknown, model.PassStandsReasonPassAndFail
		var pass, fail *model.CheckResult
		for index := range check.Results {
			result := &check.Results[index]
			if result.CommitSHA != basis.Commit || standingCommit(*result) == "" {
				continue
			}
			newest := &fail
			if result.Verdict == "pass" {
				newest = &pass
			}
			if *newest == nil || newerResultFirst(*result, **newest) < 0 {
				*newest = result
			}
		}
		state.Results = []string{pass.ID, fail.ID}
		return state
	}
	if ci != nil && ci.State == model.CIStateUnread {
		state.Answer, state.Reason = model.PassStandsUnknown, model.PassSubjectReasonReadFailed
		return state
	}
	newestFail := ""
	for _, result := range check.Results {
		if result.CommitSHA != basis.Commit || standingCommit(result) != model.CommitAtBasis || derivedAtAnotherHead(result, ci) {
			continue
		}
		switch {
		case result.Verdict != "pass":
			if newestFail == "" {
				newestFail = result.ID
			}
		case !slices.Contains(state.Roles, result.Role):
			state.Roles = append(state.Roles, result.Role)
			state.Results = append(state.Results, result.ID)
		}
	}
	switch {
	case len(state.Roles) > 0:
		state.Answer, state.Reason = model.PassStandsYes, ""
	case newestFail != "":
		state.Reason, state.Results = model.PassSubjectReasonFail, []string{newestFail}
	}
	return state
}

// derivedAtAnotherHead reports whether result is a result another head's check run derived
// (deriveCIResults) with the verdict ci, the read at this head, derived a result of its own: two
// repositories open at one commit each derive their own, and a head names its own as the result
// that decides it, never a run of the other repository. Where this head's run derived none, or one
// of the other verdict, the other head's result is the one at the commit, and is named.
func derivedAtAnotherHead(result model.CheckResult, ci *model.CheckCI) bool {
	return ci != nil && ci.Result != "" && result.ID != ci.Result &&
		strings.HasPrefix(result.ID, ciResultIDPrefix) && result.Verdict == ci.State
}

// passAndFailCommits is each commit at which check, a `before_merge` check, holds both a pass and
// a fail that count, retracted by nobody, counted, and against the check's words now (R4): from
// any role, in either order, and on a check CI proves the results its CI job recorded and the
// result Dispatch derives from its check run alike. A role's result of a check CI proves counts
// for nothing (not_counted), so it joins nothing. A later pass at such a commit does not end its
// state; a retraction of the wrong result, or a change of the check's words, does. The map is
// keyed by commit, so results at two commits never meet, and a new head of a pull request has a
// state of its own. An `after_merge` or `default_branch` check holds none here.
func passAndFailCommits(check model.CheckBlock) map[string]bool {
	conflicted := map[string]bool{}
	if check.When != docs.CheckWhenBeforeMerge {
		return conflicted
	}
	verdicts := map[string][2]bool{}
	for _, result := range check.Results {
		if standingCommit(result) == "" {
			continue
		}
		seen := verdicts[result.CommitSHA]
		if result.Verdict == "pass" {
			seen[0] = true
		} else {
			seen[1] = true
		}
		verdicts[result.CommitSHA] = seen
	}
	for commit, seen := range verdicts {
		if seen[0] && seen[1] {
			conflicted[commit] = true
		}
	}
	return conflicted
}

// newestResultStands reads branch results newest first. The first result that counts decides,
// unless a result Dispatch could not place precedes it, or both verdicts count at its commit.
func newestResultStands(check model.CheckBlock, compared comparison) model.CheckPassStands {
	switch compared.reason {
	case model.ReasonRepositoryUnreadable, model.ReasonReadFailed:
		return model.CheckPassStands{Answer: model.PassStandsUnknown, Reason: "read_failed"}
	case model.ReasonNotMerged, model.ReasonNoPullRequestOrBranch:
		return model.CheckPassStands{Answer: model.PassStandsNo, Reason: "no_result"}
	}
	for _, result := range check.Results {
		switch standingCommit(result) {
		case model.CommitUnknown:
			return model.CheckPassStands{Answer: model.PassStandsUnknown, Reason: "not_placed", Results: []string{result.ID}, Commit: result.CommitSHA}
		case model.CommitAtBasis:
			var pass, fail string
			var roles []string
			for _, at := range check.Results {
				if at.CommitSHA != result.CommitSHA || standingCommit(at) != model.CommitAtBasis {
					continue
				}
				if at.Verdict == "pass" {
					if pass == "" {
						pass = at.ID
					}
					if !slices.Contains(roles, at.Role) {
						roles = append(roles, at.Role)
					}
				} else if fail == "" {
					fail = at.ID
				}
			}
			switch {
			case pass != "" && fail != "":
				return model.CheckPassStands{Answer: model.PassStandsUnknown, Reason: "pass_and_fail", Results: []string{pass, fail}, Commit: result.CommitSHA}
			case fail != "":
				return model.CheckPassStands{Answer: model.PassStandsNo, Reason: "fail", Results: []string{fail}, Commit: result.CommitSHA}
			default:
				return model.CheckPassStands{Answer: model.PassStandsYes, Roles: roles, Results: []string{pass}, Commit: result.CommitSHA}
			}
		}
	}
	return model.CheckPassStands{Answer: model.PassStandsNo, Reason: "no_result"}
}

// liveResultStands reads only the current deploy's interval. Unlike a code check, a live
// check's newest verdict decides even when both verdicts were recorded at that build.
func liveResultStands(check model.CheckBlock, compared comparison) model.CheckPassStands {
	answer := model.CheckPassStands{Answer: model.PassStandsUnknown, Deploy: compared.deploy}
	switch compared.reason {
	case model.ReasonRepositoryUnreadable, model.ReasonReadFailed:
		answer.Reason = "read_failed"
		return answer
	case model.ReasonNotMerged, model.ReasonNoPullRequestOrBranch:
		answer.Answer, answer.Reason = model.PassStandsNo, "no_result"
		return answer
	}
	if compared.deployReason != "" {
		answer.Reason = compared.deployReason
		return answer
	}
	if compared.deployHolds.Answer == model.CommitUnknown {
		answer.Reason = "read_failed"
		return answer
	}
	if compared.deployHolds.Answer != model.CommitAtBasis {
		answer.Reason = "deploy_not_merged"
		return answer
	}
	for _, result := range check.Results {
		if standingCommit(result) != model.CommitAtBasis {
			continue
		}
		answer.Results, answer.Commit = []string{result.ID}, result.CommitSHA
		if result.Verdict == "fail" {
			answer.Answer, answer.Reason = model.PassStandsNo, "fail"
			return answer
		}
		answer.Answer = model.PassStandsYes
		for _, pass := range check.Results {
			if pass.Verdict == "pass" && standingCommit(pass) == model.CommitAtBasis && !slices.Contains(answer.Roles, pass.Role) {
				answer.Roles = append(answer.Roles, pass.Role)
			}
		}
		return answer
	}
	answer.Reason = "no_result_from_deploy"
	return answer
}
