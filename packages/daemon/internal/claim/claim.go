// Package claim is the vocabulary of a claim: the token that names one, the agent it names, the
// role it is on, and the wire the agent holding it speaks to the daemon.
//
// It is a leaf on purpose. The runtime, the worker stream, the supervisor, and the API all speak
// about claims, and each of them is reachable from the others; a `Token` declared in any of them
// would make one the root of the rest. Nothing here imports another Legion package.
package claim

// Token names one claim — one agent of one issue, whose name the token ends in — and is the key
// everything about that agent is filed under: its locator, its stream connection, its pending
// delivery, its row in the store. The agent keeps its token across the roles its steps are on.
type Token string

// Agent is the name of one agent of an issue: the last part of its claim token. The agent of a
// role's work is named for the role, so Agent(role) names it; a name can also be none of Roles.
type Agent string

// Role names a role a claim can be on: the role of the step its agent runs now, one of Legion's
// six, a role the deployment defines or a role the plan of the claim's issue defines. A claim takes
// the role of each step its agent runs.
type Role string

const (
	RoleArchitect   Role = "architect"
	RolePlanner     Role = "planner"
	RoleImplementer Role = "implementer"
	RoleTester      Role = "tester"
	RoleReviewer    Role = "reviewer"
	RoleMerger      Role = "merger"
)

// RoleController is the project controller's role. Its claim holds no issue and no tree, so it is
// none of Roles, and NewToken never builds a token of it; its one token is ControllerToken. The
// daemon holds such a claim only when it launches the controller itself (`controller: daemon`).
const RoleController Role = "controller"
