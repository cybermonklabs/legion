package plan

// Words is steps as a plan writes them, in order, each `role` or `role:key` (Step.String): the
// steps of a task in its plan node, a project's default_steps in the settings, and the line of
// `legion config check` that names them. It is never nil, so no step is `[]` on the wire.
func Words(steps []Step) []string {
	words := make([]string, len(steps))
	for i, step := range steps {
		words[i] = step.String()
	}
	return words
}
