package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects",
	Auth:        authAny,
	Description: fmt.Sprintf("Create a project {key, name, default_assignee}: any signed-in person or agent session; a bearer names its session in actor. Every project names the person its unowned work lands on, the last step of the chain that decides who a question reaches: default_assignee is the email of a person who has signed in, and a call that names none takes the person creating the project, else the person its credential acts for. A credential that acts for nobody and names no default_assignee is 400 INVALID_PROJECT; a login nobody has signed in with, the one the call names or the one its credential acts for, is 400 ASSIGNEE_NOT_ALLOWED. A key that is not a project key, an empty name, a name holding a control character or a bidi embedding, override or isolate, or a name with no visible character (a letter, number, punctuation or symbol other than the blank fillers U+115F, U+1160, U+3164, U+FFA0 and U+2800, or a code point Go's Unicode tables leave unassigned, such as an emoji newer than them, other than a noncharacter (U+FDD0-U+FDEF and each code point ending in FFFE or FFFF) or a reserved default-ignorable (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF); combining marks, format characters, private-use characters and whitespace don't count) is 400 INVALID_PROJECT; a name over %d characters is 400 CAP_EXCEEDED; a key that already exists is 409 PROJECT_EXISTS. The answer is the project with the default_assignee the create stored.", contracts.ProjectNameMax),
	Handler:     (*server).createProject,
	// Creating a project belongs to the deployment: a credential scoped to one project may not
	// create another.
	WriteClass: scopeDeployment,
	Shape:      routeShape{Status: http.StatusCreated, Response: createdProject{}, Request: createProjectInput{}},
})
