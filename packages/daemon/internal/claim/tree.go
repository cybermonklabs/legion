package claim

// IsTreeRoot is whether the issue is its tree's root. A tree is named for the issue it grew from,
// so the root is the one issue whose key is the tree's, whichever role is on it.
func IsTreeRoot(issue, tree string) bool { return issue == tree }

// IsTreeArchitect is whether a claim is the architect of its tree's own issue: the tree's root
// claim, one per tree, which ends only when the tree closes. An architect on a child issue is a
// sub-architect, the child's own, which the daemon starts with the child's first phase. A worker
// on the root issue is not the root claim.
func IsTreeArchitect(role Role, issue, tree string) bool {
	return role == RoleArchitect && IsTreeRoot(issue, tree)
}

// OwnsPlan is whether a claim on this role owns its issue's plan: the agent that writes the plan
// of its issue and gives it to Legion with register_plan, today each architect, a tree's root
// architect and a sub-architect alike. It is the one rule of who an owner is: the composer gives
// an owner the architect's prompt parts (prompts.Compose), the daemon gives an owner's launch the
// deployment's plan guidance and no other (daemon.OwnsPlan), and the plan routes take an owner's
// grant (the api server's architectGrant), which today gates every operation of a tree's architect
// too. A release that makes an owner of another role changes this body and splits architectGrant:
// its plan routes keep this rule, and a tree architect's operations take a check of the architect
// role, as architectGrant's comment says.
func OwnsPlan(role Role) bool { return role == RoleArchitect }

// IsController is whether a claim is the project controller's: on the controller role, with no
// issue and no tree. IsTreeRoot alone would call such a claim its own tree's root, since its empty
// issue equals its empty tree, so every caller that sorts claims by tree asks this first.
func IsController(role Role, issue, tree string) bool {
	return role == RoleController && issue == "" && tree == ""
}
