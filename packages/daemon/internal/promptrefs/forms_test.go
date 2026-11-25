package promptrefs

import (
	"maps"
	"slices"
	"testing"
)

// A folder of texts an experiment arm names is held to the agents and skills the worker image ships, so a
// name the reader misses is a name nobody checks: a folder text could dispatch an agent no image
// ships, and the launch would run the folder where the control would have fallen back. The
// reader therefore takes every form a text names an agent or a skill in, not only the one Legion's
// own texts mostly use. This is one row per form, over a text held in memory, so no file is read:
// the form, the agents and the skills it names. The last row is the join of all of them, which
// holds the file once for each name and no name twice.
func TestTheReaderCollectsEachForm(t *testing.T) {
	const file = "roles/form.md"
	for _, tc := range []struct {
		name, text string
		agents     []string
		skills     []string
	}{
		{"agent equals a double-quoted name", `task(agent="x")`, []string{"x"}, nil},
		{"agent equals an escaped double-quoted name", `task(agent=\"x\")`, []string{"x"}, nil},
		{"agent equals a single-quoted name", `task(agent='x')`, []string{"x"}, nil},
		{"agent colon a double-quoted name", `task({ agent: "x" })`, []string{"x"}, nil},
		{"a quoted agent key", `{"agent": "x"}`, []string{"x"}, nil},
		{"a subagent_type under the plugin's namespace", `Agent with subagent_type: "legion:x"`, []string{"x"}, nil},
		{"a skill URL", `Load skill://x first.`, nil, []string{"x"}},
		{"a skill command", `Run /skill:x now.`, nil, []string{"x"}},
		{"a skill URL ending a sentence", `Load skill://x.`, nil, []string{"x"}},
		{"a skill URL with a path", `Read skill://x/references/y.md.`, nil, []string{"x"}},
		{"every form at once, one name in two forms", "agent=\"a\" agent='b' agent: \"c\" subagent_type: \"legion:d\" skill://e /skill:f agent=\"a\" skill://e",
			[]string{"a", "b", "c", "d"}, []string{"e", "f"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names := New()
			names.Text(file, []byte(tc.text))
			for kind, want := range map[Kind][]string{TaskAgents: tc.agents, Skills: tc.skills} {
				got := names[kind]
				if !slices.Equal(slices.Sorted(maps.Keys(got)), want) {
					t.Errorf("%s: Text(%q) collected %v, want %v", kind.Variable(), tc.text, slices.Sorted(maps.Keys(got)), want)
				}
				for name, files := range got {
					if !slices.Equal(files, []string{file}) {
						t.Errorf("%s: %s is named by %q, want [%s] once", kind.Variable(), name, files, file)
					}
				}
			}
		})
	}

	// A form that names nothing a prompt can write is no reference: a placeholder, a plain word
	// and an address that only looks like one are read as none.
	for _, text := range []string{`agent="<name>"`, `skill://<name>/<path>`, `write agent://1-Reviewer`, `the agent: a reviewer`, `/skill: nothing`} {
		names := New()
		names.Text(file, []byte(text))
		if len(names[TaskAgents]) != 0 || len(names[Skills]) != 0 {
			t.Errorf("Text(%q) collected agents %v and skills %v, want none", text, names[TaskAgents], names[Skills])
		}
	}
}
