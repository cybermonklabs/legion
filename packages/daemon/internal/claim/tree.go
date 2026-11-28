package claim

// IsTreeRoot is whether the issue is the gate root its caller names. The daemon stores no tree:
// the caller computes the gate root on the Dispatch chain (record.Chains.GateRoot or
// record.Chains.StateTree, R15 and R18 of dispatch://INFRA-3320) and passes it as tree, so the
// root is the one issue whose key is that gate root's, whichever role is on it. A claim's or an
// issue's stored tree is not that answer.
func IsTreeRoot(issue, tree string) bool { return issue == tree }

// IsTreeArchitect is whether a claim is the architect of the gate root its caller names (tree, as
// IsTreeRoot reads it): the claim of an issue that holds its own design gate, which ends only when
// that issue leaves Legion's work. An architect on an issue under it is that issue's own, which
// the daemon starts with the issue's first phase. A worker on the gate root is not its architect.
func IsTreeArchitect(role Role, issue, tree string) bool {
	return role == RoleArchitect && IsTreeRoot(issue, tree)
}

// IsController is whether a claim is the project controller's: on the controller role, with no
// issue and no tree. IsTreeRoot alone would call such a claim its own tree's root, since its empty
// issue equals its empty tree, so every caller that sorts claims by tree asks this first.
func IsController(role Role, issue, tree string) bool {
	return role == RoleController && issue == "" && tree == ""
}
