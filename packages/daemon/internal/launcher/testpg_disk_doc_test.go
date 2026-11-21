package launcher

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testdoc"
)

// testPGDiskDocExtract is the committed copy of the guide's region this test reads, which
// internal/textcheck holds to docs/kubernetes.md's own lines, so this test reads no guide and puts
// none in its job's filter.
const testPGDiskDocExtract = "testpg_disk_doc_extract.txt"

// An agent whose test databases vanish in the middle of a run reads docs/kubernetes.md, the guide
// that holds the test database's own disk, to learn why. Where the test database is on a disk of
// its own, the launcher's check resets the cluster under its floor (checkTestPGDisk), and the one
// trace that reset leaves is the line legion-testpg writes, which starts `legion-testpg: reset`
// (testPGProgram's name). So the guide's section "A test database in the pod" must name that line
// and give a `kubectl logs` command that shows it, on the `roles` container, whose log holds it: a
// guide that names another line, or sends the agent to another container's log, leaves the agent
// nothing to find and no cause. The command is held whole, as the one code span the agent copies:
// the container it names with -c, and a filter after it for the line. The region is read out of
// the committed copy (testPGDiskDocExtract) with its whitespace collapsed, since the guide wraps.
func TestPGDiskTheGuideNamesTheResetLineAndTheCommandThatShowsIt(t *testing.T) {
	_, flat := testdoc.Region(t, testPGDiskDocExtract, "the reset's line and how to read it",
		"`legion-testpg: reset of the cluster in <data directory>", "a role reads it there or in `$LEGION_TEST_PG_LOG`.")
	resetLine := testPGProgram + ": reset"
	var named bool
	var commands []string
	for _, span := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(strings.Join(strings.Fields(flat), " "), -1) {
		named = named || strings.HasPrefix(span[1], resetLine)
		if strings.HasPrefix(span[1], "kubectl logs ") {
			commands = append(commands, span[1])
		}
	}
	if !named {
		t.Errorf("the guide's region in %s names no line starting %q, the line a reset of the test database writes", testPGDiskDocExtract, resetLine)
	}
	for _, command := range commands {
		read, filter, _ := strings.Cut(command, "|")
		if loggedContainer(strings.Fields(read)) == "roles" && strings.Contains(filter, resetLine) {
			return
		}
	}
	t.Errorf("the guide's region in %s gives the kubectl logs commands %q, and none reads the roles container (-c roles) and filters it for %q",
		testPGDiskDocExtract, commands, resetLine)
}

// loggedContainer is the container a `kubectl logs` command's words name, in any of the forms
// kubectl reads (-c roles, -c=roles, --container roles, --container=roles), or "" where they name
// none.
func loggedContainer(words []string) string {
	for i, word := range words {
		for _, flag := range []string{"-c", "--container"} {
			if value, ok := strings.CutPrefix(word, flag+"="); ok {
				return value
			}
			if word == flag && i+1 < len(words) {
				return words[i+1]
			}
		}
	}
	return ""
}
