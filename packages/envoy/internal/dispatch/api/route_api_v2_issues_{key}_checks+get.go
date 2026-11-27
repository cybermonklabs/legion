package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/checks", Auth: authAny, Versions: onlyV2,
		Description: "The checks of the issue's Verification document in document order, each with its attributes " +
			"(when, surface, run, negative_control, ci_check, subject, program), its text - a link's text, never its target - the version " +
			"its words last changed in and their digest, and results, every result recorded against it from any issue, " +
			"newest first by when the recording transaction began. Each check carries compared_with, the commits and " +
			"branches its results are compared with: for a before_merge check every pull request head Dispatch holds " +
			"for the check's issue, {kind: pull_request, commit, pull_request, learned_at, repo?}: the open and merged " +
			"pull requests attributed to the issue, and those its external links name " +
			"(https://github.com/<owner>/<repo>/pull/<n>) in a repository it does not resolve to, a linked one's head " +
			"carrying that repository as repo (owner/name); for an after_merge check " +
			"{kind: branch, branch} for the branch that issue's pull request merged into and the repository's default " +
			"branch; for a default_branch check the default branch alone. Where that list is empty the check carries " +
			"compared_nothing: no_pull_request (the check's issue holds no pull request), no_repository (the issue " +
			"resolves to no repository), repository_unreadable (Dispatch's GitHub App cannot read the repository) or " +
			"read_failed (a read Dispatch makes did not answer). Each check carries pass_stands {answer, roles, " +
			"reason, subjects}, yes with the roles whose passes stand, no, or unknown. A before_merge check compared " +
			"with pull request heads names its subjects, the heads a pass must stand at: the head of each open pull " +
			"request of the issue, or with none open the final head of the newest merged one, the one that merged " +
			"last; each {basis, answer, reason, roles, results, ci}, its state from the results at its own commit " +
			"alone: yes, no for fail or no_result, or unknown for pass_and_fail (a pass and a fail at that commit) or " +
			"read_failed (a check CI proves whose check runs at that head could not be read). A pass of the check " +
			"stands only where one stands at each subject; where Dispatch cannot tell which merged pull request " +
			"merged last, it names no subject and answers unknown for read_failed. Each result carries commit_stands {answer, basis, reason, " +
			"repo} (at_basis, other_commit, or unknown). A result whose commit the record took to be of a linked " +
			"repository is decided by that repository: a before_merge one by the pull request heads compared with, and " +
			"an after_merge or default_branch one by that repository's branches (for after_merge the branch its merged " +
			"linked pull request merged into, then its default branch). A basis in a linked repository carries it as " +
			"repo, and an other_commit or unknown answer that repository decided names it as repo, so a reader can " +
			"tell it from the issue's own repository, which carries none. Each result also carries check_words_moved, " +
			"and retracted {reason, actor, created_at} where a " +
			"retraction stands against it, which makes it no evidence. A check CI proves carries ci {run, state, case, " +
			"detail, line, commit, repo, result}, read from that check run at the head of each open pull request of " +
			"the issue, each in that pull request's own repository, or with none open at the final head of the " +
			"newest merged one: ci is the read of the open pull request of the highest number, each subject carries " +
			"the read at its own head, and a result is derived from the run at each head on every read, standing at " +
			"the head it was read at. at=<commit> compares every " +
			"check with that one commit, {kind: named, commit}, 40 lowercase hex characters (400 INVALID_QUERY " +
			"otherwise). limit 1 to 200 (default 50) and offset; {items, total, limit, offset, issue, " +
			"verification_artifact_id}; an issue with no Verification document answers no checks. A caller that sent " +
			"Dispatch-Trees: 1 is also given each check's text_tree, its whole content as the tree a reader is shown, " +
			"links and references kept, and each result's observed_tree, the tree its observed text is shown as. " +
			"404 ISSUE_NOT_FOUND.",
		Handler:  s.listIssueChecksV2,
		Resource: "checks", Kind: kindCollection,
		Query:    issueChecksV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: issueChecksV2{}},
		Refusals: documentRefusals(listRefusals(issueMissingRefusal)...),
	}
})
