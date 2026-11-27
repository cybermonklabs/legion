package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/issues/{key}/results", Auth: authAny, Versions: onlyV2,
		Description: "Record one result of one check {check, role, verdict, commit, surface?, run?, observed?, " +
			"negative_control?, failed?, skipped?, excluded?, screenshots?}, a bearer naming its session in actor. " +
			"{key} is the issue the result is recorded on; check names a check block of its Verification document or " +
			"of one of its ancestors', by block id or, where a block id is not unique in that set, by its canonical " +
			"reference (409 CHECK_AMBIGUOUS); any other is 400 UNKNOWN_CHECK, and a check CI proves 409 " +
			"CHECK_PROVED_BY_CI. role is at most 32 characters of a-z and -, verdict pass or fail, commit 40 lowercase " +
			"hex (400 RESULT_INPUT otherwise), and the counts default to 0; a pass counting any failed, skipped or " +
			"excluded case is 400 RESULT_CONTRADICTS. The commit must be held by the repository the check's issue " +
			"resolves to, or by one in which the issue's external links name an open or merged GitHub pull request " +
			"(https://github.com/<owner>/<repo>/pull/<n>) and that Dispatch's GitHub App is installed on and can read; " +
			"a record reads at most 10 linked pull requests. A commit no such repository holds is 400 " +
			"RESULT_COMMIT_UNKNOWN, naming the commit, each repository asked and each linked pull request Dispatch " +
			"could not read or set aside. A read of a linked pull request or a linked repository that did not answer " +
			"(a rate limit, a 5xx, a transport failure), where no repository is shown to hold the commit, is 502 " +
			"GITHUB_UNREADABLE naming that repository, and records nothing: record the result again, after the " +
			"Retry-After a rate limit carries. An issue that resolves to no repository takes any commit, and one " +
			"whose repository Dispatch cannot read takes it unless such a linked read did not answer. A result at a " +
			"linked repository's commit keeps that repository, which GET /api/v2/issues/{key}/checks compares it in. " +
			"A closed issue takes a result only for an after_merge or default_branch check " +
			"(409 ISSUE_CLOSED). A check reworded by a document version at each of the two attempts this write makes " +
			"is 409 CHECK_CONTENDED with Retry-After: 1, and nothing recorded: read the check and retry. Answers 201 " +
			"{result, line}, line the pull-request body line the result implies, and appends result.recorded on the " +
			"check's issue. 404 ISSUE_NOT_FOUND.",
		Handler:  s.recordIssueResultV2,
		Resource: "results", Kind: kindCollection, Key: "{id}",
		Body:  requestBody{Kind: bodyJSON, Type: checkResultInput{}},
		Shape: routeShape{Status: http.StatusCreated, Response: checkResultRecorded{}},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		Refusals: checkResultRefusals(issueClosedRefusal, apicode.Refusal{Condition: apicode.CheckReworded, Code: apicode.CheckContended}),
	}
})
