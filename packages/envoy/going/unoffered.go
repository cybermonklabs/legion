package going

// unoffered.go holds the tools of Dispatch the command bar does not offer its model, each with its
// reason. The bar's agent (going/agent) leaves each out of what find_tools answers and refuses a
// call_tool or a plan's change that names one, with its reason; the server's catalogue build
// (internal/dispatch/api buildGoingCatalogue) refuses a tool of Dispatch that binds no route, that
// neither of its tables names and that this list does not name, so a new tool of Dispatch is
// served or named here, and never found by the model and refused when it calls it.

// UnofferedTools is each tool of Dispatch the command bar does not offer its model, by its spec's
// name, with the reason: code, a refusal of a call that names it and a reviewer read it, and no
// tool spec or prompt the model is given says it.
var UnofferedTools = map[string]string{
	"dispatch_api":            "sends generic requests by path; the command bar uses the tool for each route instead",
	"dispatch_project_update": "turns a project's proven-sessions check on or off, which only the project's roll-out issue does; the loop that proposes operations on Dispatch's graph never proposes it",
}
