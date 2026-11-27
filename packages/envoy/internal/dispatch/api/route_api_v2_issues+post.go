package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/issues", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("Create an issue {project, title, parent?, blocked_by?, advances?, external?, "+
			"force?, spec?, labels?, priority?, assignee?, components?, repo?}, a bearer naming its session in actor: a "+
			"native issue in project, or one imported from a GitHub owner/repo#n named in external into project. Every "+
			"create names its project, an import included: external with no project, or an empty one, is 400 "+
			"INVALID_ISSUE, and an external ref already linked to an issue of another project is 400 "+
			"EXTERNAL_PROJECT_MISMATCH. A credential that names one project creates in another project as a "+
			"credential that names none may, but a project that does not exist and a private project the "+
			"credential does not name are alike 404 NOT_FOUND, with nothing created. assignee defaults to the creating person, the personal token's owner, "+
			"the parent's assignee when the parent is of the issue's own project, or the project's default "+
			"assignee. A title over %d "+
			"characters is 400 CAP_EXCEEDED. An unforced create whose title is a near copy of another issue's is 409 "+
			"POSSIBLE_DUPLICATE with the candidates. Answers 201 the issue with advice beside it, and 200 the issue an "+
			"external ref already imported. After the write commits, a related-item check searches the project by "+
			"meaning and by its rarest words and waits at most 5 seconds; the advice's suggestions say whether it found "+
			"related items (outcome found, none or not_made with the reason in missing), how it searched, the three items "+
			"most like the new issue with each ask item's answer state, words to search with by hand (search_query), and "+
			"under force the near copies forced past (title_matches). A blocked_by that would make the new issue wait on "+
			"itself through its parent is 409 ISSUE_CYCLE. When Legion runs the new issue, because it or an issue of its "+
			"own project above it carries the label legion, a create under a parent by a session that is no person and "+
			"no architect of the parent or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT. A create under a "+
			"parent of another project is judged as a root of the new issue's own project, which this check does not "+
			"judge. A priority of 0 needs the p0 scope: a create that names priority 0 makes the new issue's own "+
			"priority P0, and from a credential that does not hold p0 it is 403 OAUTH_SCOPE_REQUIRED naming p0, under "+
			"a WWW-Authenticate challenge for p0, and writes nothing. A person at a browser holds p0, a personal token "+
			"holds it only where its maker gave it, and a create that names a priority of 1 to 3, or none, needs no "+
			"scope; a priority that an issue above the new issue has is none of its own. A create from a ref that "+
			"Dispatch already imported answers that issue and sets no priority.", contracts.IssueTitleMax),
		// A new child is checked against its parent's status under the same lock as /api/v1.
		Handler:  s.createIssueV2,
		Resource: "issues", Kind: kindCollection, Key: "{key}",
		Body: requestBody{Kind: bodyJSON, Type: createIssueInput{}},
		Shape: routeShape{
			Status: http.StatusCreated, Response: model.Issue{}, Advice: writeAdvice{},
			OtherAdvice: []any{suggestionsOnlyAdvice{}},
			// An import whose reference already has an issue answers that issue with the advice
			// and suggestions a create writes (issue_create.go, the existing-issue arm).
			Also: []routeAnswer{{
				Status: http.StatusOK, Body: model.Issue{},
				Advice: writeAdvice{}, OtherAdvice: []any{suggestionsOnlyAdvice{}},
			}},
			Refusals: []routeAnswer{
				{Status: http.StatusConflict, Body: issueDuplicateError{}},
				{Status: http.StatusConflict, Body: parentStatusConflictBody{}},
			},
		},
		Scope: scopeProjectInBody,
		// The spec is seeded straight into the store (docs.SeedText), which opens no room.
		Refusals: issueFieldRefusals(markdownWriteRefusals(
			dependencyLoopRefusal, issueRefInvalidRefusal,
			parentStatusConflictRefusal,
			apicode.Refusal{Condition: apicode.IssueRefAmbiguous, Code: apicode.AmbiguousIssueRef},
			apicode.Refusal{Condition: apicode.ExternalOfAnotherProject, Code: apicode.ExternalProjectMismatch},
			apicode.Refusal{Condition: apicode.TitleNearCopy, Code: apicode.PossibleDuplicate},
			apicode.Refusal{Condition: apicode.IssueProjectOutOfReach, Code: apicode.NotFound},
			specWriterNotArchitectRefusal,
		)...),
	}
})
