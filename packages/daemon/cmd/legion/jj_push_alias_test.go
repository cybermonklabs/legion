package main

import "testing"

// A push that jj expands into `jj git push` sends the commit as the step left it, in a pod and in
// a tmux pane alike: over C, a commit the other role made and the implementer described with its
// own trailer, under the new bookmark b, `jj p -b b` with aliases.p = ["git", "push"] in the
// repository's own configuration, and a bare `jj` with ui.default-command = ["git", "push", "-b",
// "b"] there, each send b with C as the implementer's, print the step's one rewrite line, and leave
// no change divergent.
func TestAPushThatJJExpandsFromAnAliasOrTheDefaultCommandRunsTheStep(t *testing.T) {
	t.Parallel()
	impl, rev := rolePushImplementer, rolePushReviewer
	for _, row := range []struct {
		name, key, value string
		args             []string
	}{
		{name: "jj p -b b, aliases.p = ['git', 'push'] in the repository's configuration", key: "aliases.p", value: `["git", "push"]`, args: []string{"p", "-b", "b"}},
		{name: "a bare jj, ui.default-command = ['git', 'push', '-b', 'b'] in the repository's configuration", key: "ui.default-command", value: `["git", "push", "-b", "b"]`, args: []string{}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			rolePushEachForm(t, func(t *testing.T, form rolePushForm) {
				f := newRolePushFixture(t, form, rolePushRecords)
				f.jj(impl, "config", "set", "--repo", row.key, row.value)
				c := f.commitOn(rev, impl, "main", rolePushMessage("work", "implementer"))
				f.bookmark("b", c.commit)
				f.pushAndCheck(row.args, rolePushSends("b", c, "implementer", impl))
			})
		})
	}
}
