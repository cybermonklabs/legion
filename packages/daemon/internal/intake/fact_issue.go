package intake

// IssueOf is the issue fact names, for a fact that names one, and false for any other: a fact
// that names none or several — a pull request's by its repository and number, a push, a tree's
// adoption of its children, the controller's tick over many issues, the daemon's own sweeps. Each
// fact type answers it itself (Fact), so no type can be added without its answer.
func IssueOf(fact Fact) (string, bool) {
	return fact.namedIssue()
}
