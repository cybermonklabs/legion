package record

import (
	"os/exec"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// fortyHex is a full commit id of cybermonklabs/legion.
var fortyHex = regexp.MustCompile(`^[0-9a-f]{40}$`)

// keptRuleNames is the rules the replay's comparison runs, by name, in the order the repository
// keeps them: the daemon's own first, then the rules three recorded windows ran under, then the
// candidates of the comparison.
var keptRuleNames = []string{"main", "legion-v20.42.5", "legion-v19.57.3", "legion-v22.14.1", "keep-past-review", "blockers-first",
	"returned-by-rank", "cover", "wait-grace", "wait-grace-1h", "returned-by-first-slot", "rework-first", "rework-first-keep", "arrival-order",
	"p0-at-once", "idle-return-1h", "p0-idle"}

// Every rule the repository keeps names the commit of cybermonklabs/legion its choices come from,
// so a reader of a replay's result can read the code that made them (record.SlotRule.Commit): the
// list holds the comparison's rules by name, the daemon's own first and equal to the rule the daemon
// is built with, each named once and found by its name; and each commit is a full 40-hex id of a
// commit the history of main holds. The history half reads the repository's own clone with git
// (`git cat-file -e <sha>^{commit}` and `git merge-base --is-ancestor <sha> <main>`), and is skipped
// where the clone holds no history to read: a shallow clone, or no clone of main.
func TestEveryRuleNamesItsCommit(t *testing.T) {
	rules := SlotRules()
	var names []string
	for _, rule := range rules {
		names = append(names, rule.Name)
		if !fortyHex.MatchString(rule.Commit) {
			t.Errorf("the rule %q names the commit %q, which is no 40-hex commit id", rule.Name, rule.Commit)
		}
		if named, kept := SlotRuleNamed(rule.Name); !kept || !reflect.DeepEqual(named, rule) {
			t.Errorf("SlotRuleNamed(%q) answers %+v, %t, and SlotRules keeps %+v", rule.Name, named, kept, rule)
		}
	}
	if !slices.Equal(names, keptRuleNames) {
		t.Fatalf("the repository keeps the rules %v, and the comparison runs %v", names, keptRuleNames)
	}
	if !reflect.DeepEqual(rules[0], MainRule) {
		t.Errorf("the first rule kept is %+v, and the daemon is built with %+v", rules[0], MainRule)
	}
	rules[0].KeepsSlotIn[0] = "changed"
	if again := SlotRules(); !reflect.DeepEqual(again[0], MainRule) {
		t.Errorf("a change to a rule SlotRules answered reached the next answer: %+v", again[0])
	}

	t.Run("in the history of main", func(t *testing.T) {
		main := mainOfTheClone(t)
		var commits []string
		named := map[string][]string{}
		for _, rule := range SlotRules() {
			if _, seen := named[rule.Commit]; !seen {
				commits = append(commits, rule.Commit)
			}
			named[rule.Commit] = append(named[rule.Commit], rule.Name)
		}
		for _, commit := range commits {
			rules := strings.Join(named[commit], ", ")
			if out, err := exec.Command("git", "cat-file", "-e", commit+"^{commit}").CombinedOutput(); err != nil {
				t.Errorf("the rules %s name %s, which the clone holds no commit of: %v %s", rules, commit, err, out)
				continue
			}
			if out, err := exec.Command("git", "merge-base", "--is-ancestor", commit, main).CombinedOutput(); err != nil {
				t.Errorf("the rules %s name %s, which %s does not reach: %v %s", rules, commit, main, err, out)
			}
		}
	})
}

// mainOfTheClone is the name the repository's clone gives the main branch, origin/main where it
// holds it and main otherwise, and skips t where the clone holds no history to read: git is not
// there, the directory is in no git clone, the clone is shallow, or it holds no main.
func mainOfTheClone(t *testing.T) string {
	t.Helper()
	shallow, err := exec.Command("git", "rev-parse", "--is-shallow-repository").Output()
	if err != nil {
		t.Skipf("no git clone to read the commits in: %v", err)
	}
	if strings.TrimSpace(string(shallow)) == "true" {
		t.Skip("the clone is shallow and holds no history to read the commits in")
	}
	for _, name := range []string{"origin/main", "main"} {
		if err := exec.Command("git", "rev-parse", "--verify", "--quiet", name+"^{commit}").Run(); err == nil {
			return name
		}
	}
	t.Skip("the clone holds no main branch to read the commits' history from")
	return ""
}
