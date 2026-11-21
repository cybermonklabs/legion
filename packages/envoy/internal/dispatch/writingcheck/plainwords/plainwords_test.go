package plainwords

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// skillDirectory is the skill this package copies, from the package's own directory.
const skillDirectory = "../../../../../../skills/plain-words"

// filesUnder is every regular file under root, by its path below root, with its bytes.
func filesUnder(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = content
		return nil
	})
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	return files
}

// The embedded rules are the skill's text: skill/ holds every file of skills/plain-words, byte
// for byte, and no other file. An edit of the skill fails here until the copy is made again.
func TestTheCopyIsTheSkill(t *testing.T) {
	skill, copied := filesUnder(t, skillDirectory), filesUnder(t, "skill")
	if len(skill) == 0 {
		t.Fatalf("%s holds no file", skillDirectory)
	}
	var differ []string
	for name, content := range skill {
		if held, ok := copied[name]; !ok || !bytes.Equal(held, content) {
			differ = append(differ, name)
		}
	}
	for name := range copied {
		if _, ok := skill[name]; !ok {
			differ = append(differ, name)
		}
	}
	if len(differ) > 0 {
		slices.Sort(differ)
		t.Fatalf("skill/ is not skills/plain-words: %s differ. Run `go generate ./internal/dispatch/writingcheck/plainwords` from packages/envoy", strings.Join(differ, ", "))
	}
}

func TestTheRulesAreTheSkillsBodyAndItsReferences(t *testing.T) {
	if !strings.HasPrefix(Skill, "# Write so a reader understands it on the first read\n") {
		t.Fatalf("Skill starts %q, want SKILL.md's first heading after its frontmatter", Skill[:min(len(Skill), 60)])
	}
	if strings.Contains(Skill, "name: plain-words") {
		t.Fatal("Skill holds the frontmatter")
	}
	for _, want := range []string{
		"## The tests",
		"## The phrases to stop",
		"| land the change, land the PR |",
	} {
		if !strings.Contains(Skill, want) {
			t.Errorf("Skill does not hold %q", want)
		}
	}
	if !strings.HasPrefix(Patterns, "# Pattern catalog\n") || !strings.HasPrefix(Terminology, "# Terminology\n") {
		t.Fatalf("the references start %q and %q", Patterns[:min(len(Patterns), 30)], Terminology[:min(len(Terminology), 30)])
	}
	if want := Skill + "\n" + Patterns + "\n" + Terminology; Rules != want {
		t.Fatal("Rules is not Skill, Patterns and Terminology, in that order")
	}
}

func TestWithoutFrontmatter(t *testing.T) {
	for text, want := range map[string]string{
		"---\nname: x\n---\n\n# Body\n": "# Body\n",
		"# Body\n":                      "# Body\n",
		"---\nname: x\n# Body\n":        "---\nname: x\n# Body\n",
	} {
		if got := withoutFrontmatter(text); got != want {
			t.Errorf("withoutFrontmatter(%q) = %q, want %q", text, got, want)
		}
	}
}
