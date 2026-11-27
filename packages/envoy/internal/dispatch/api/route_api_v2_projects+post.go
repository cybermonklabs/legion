package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/projects", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("Create a project {key, name, default_assignee?}, a bearer naming its session in "+
			"actor. Every project names a default assignee, %s. default_assignee is the email of a person who has "+
			"signed in, and a call that names none takes the person creating the project, else the person its "+
			"credential acts for. A login nobody has signed in with is 400 ASSIGNEE_NOT_ALLOWED. A credential that "+
			"acts for nobody and names no default_assignee, a key that is not a project key, an empty name, a name "+
			"holding a control character or a bidi embedding, override or isolate, or a name with no visible "+
			"character is 400 INVALID_PROJECT; a name over %d characters is 400 CAP_EXCEEDED; a key that already "+
			"exists is 409 "+
			"PROJECT_EXISTS. A credential scoped to one project is refused, since a project belongs to the deployment. "+
			"Answers 201 the project as GET /api/v2/projects lists it, with the default_assignee the create stored, "+
			"and appends project.created.",
			projectDefaultAssigneeText, contracts.ProjectNameMax),
		Handler:  s.createProjectV2,
		Resource: "projects", Kind: kindCollection, Key: "{key}",
		Body:  requestBody{Kind: bodyJSON, Type: createProjectInput{}},
		Shape: routeShape{Status: http.StatusCreated, Response: projectV2{}},
		Scope: scopeDeployment,
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectInputRefusal, assigneeNotSignedInRefusal,
			valuePastBoundRefusal, projectKeyHeldRefusal},
	}
})
