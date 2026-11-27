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

// StartGeneration is the generation a start of role serves on issue, of tree, whose own generation
// is generation and whose tree's run is treeRun (0 where the record names none, when the issue's own
// generation stands for it): the tree's run for the tree's architect (IsTreeArchitect), which a
// root's next change leaves, and the issue's own generation for every other start, a child's own
// architect's among them. It is the one rule: record.Issue.GenerationOf reads it for a start, and
// the store, which reads no record.Issue, reads it for the resumes a cancelled release queues
// (store.IssueRun.StartOf), so every writer of a start and the outbox's fence agree.
func StartGeneration(role Role, issue, tree string, generation, treeRun uint64) uint64 {
	if IsTreeArchitect(role, issue, tree) && treeRun != 0 {
		return treeRun
	}
	return generation
}

// IsController is whether a claim is the project controller's: on the controller role, with no
// issue and no tree. IsTreeRoot alone would call such a claim its own tree's root, since its empty
// issue equals its empty tree, so every caller that sorts claims by tree asks this first.
func IsController(role Role, issue, tree string) bool {
	return role == RoleController && issue == "" && tree == ""
}
