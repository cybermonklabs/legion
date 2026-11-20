package testdb

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// dispatchURLChildEnv marks the child run of this test binary that
// TestATestWithNoDispatchDatabaseFailsInACIRunAndSkipsOutsideOne starts: in it,
// TestDispatchURLChild calls DispatchURL with the environment its parent gave it.
const dispatchURLChildEnv = "TESTDB_DISPATCH_URL_CHILD"

// TestDispatchURLChild is the child's whole run: it asks DispatchURL for the server, and passes
// when DispatchURL answers one. Outside the child it skips.
func TestDispatchURLChild(t *testing.T) {
	if os.Getenv(dispatchURLChildEnv) != "1" {
		t.Skip("the child run of TestATestWithNoDispatchDatabaseFailsInACIRunAndSkipsOutsideOne")
	}
	DispatchURL(t)
}

// A Postgres-backed Dispatch test with no DISPATCH_TEST_DATABASE_URL fails, naming the variable,
// in a CI run - CI set true, or GITHUB_ACTIONS=true, each on its own - since go test reads a skip
// as a pass, and skips outside one. With the variable set it runs. Each case is a child run of
// this test binary with that environment alone.
func TestATestWithNoDispatchDatabaseFailsInACIRunAndSkipsOutsideOne(t *testing.T) {
	for _, row := range []struct {
		name string
		env  []string
		want string
	}{
		{"CI=true alone", []string{"CI=true"}, "--- FAIL"},
		{"CI=1 alone", []string{"CI=1"}, "--- FAIL"},
		{"GITHUB_ACTIONS=true alone", []string{"GITHUB_ACTIONS=true"}, "--- FAIL"},
		{"neither", nil, "--- SKIP"},
		{"CI=false", []string{"CI=false"}, "--- SKIP"},
		{"a database named, in a CI run", []string{"CI=true", "GITHUB_ACTIONS=true", DispatchURLEnv + "=postgres://example.invalid/postgres"}, "--- PASS"},
	} {
		t.Run(row.name, func(t *testing.T) {
			child := exec.Command(os.Args[0], "-test.run=^TestDispatchURLChild$", "-test.v", "-test.count=1")
			for _, entry := range os.Environ() {
				name, _, _ := strings.Cut(entry, "=")
				if name != "CI" && name != "GITHUB_ACTIONS" && name != DispatchURLEnv {
					child.Env = append(child.Env, entry)
				}
			}
			child.Env = append(child.Env, dispatchURLChildEnv+"=1")
			child.Env = append(child.Env, row.env...)
			out, _ := child.CombinedOutput()
			text := string(out)
			if !strings.Contains(text, row.want+": TestDispatchURLChild") {
				t.Fatalf("with %v the child printed, want %q:\n%s", row.env, row.want, text)
			}
			if row.want == "--- FAIL" && !strings.Contains(text, DispatchURLEnv+" must be set") {
				t.Errorf("with %v the child failed without naming %s:\n%s", row.env, DispatchURLEnv, text)
			}
		})
	}
}
