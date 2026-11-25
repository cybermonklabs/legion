package workspace

//ci:reads packages/contracts/fixtures/daemon-api/* TestTheTaskDirFixture reads it (the write is under -update, which CI never passes)

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateTaskDirs = flag.Bool("update", false, "rewrite the fixture of TaskDir's answers that the plugin reads")

// taskDirsFixture is the file the plugin reads: TaskDir's answer for each case, the directory or a
// refusal. The plugin derives the working copy of its session's step from LEGION_WORKSPACE and the
// step's branch (`stepCopy`, packages/pi-shared/src/handoff-record.ts), never from a path anyone
// sends, and packages/pi-shared/src/handoff-record.test.ts holds its answer to each case here.
type taskDirsFixture struct {
	Cases []taskDirCase `json:"cases"`
}

// taskDirCase is one call of TaskDir: its two arguments and its answer, Dir where it answers one and
// Refused where it refuses. The refusal's words are Go's own and stay here, since the plugin words
// its refusals itself.
type taskDirCase struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Branch    string `json:"branch"`
	Dir       string `json:"dir,omitempty"`
	Refused   bool   `json:"refused,omitempty"`
}

// The working copy of the issue whose cases these are, as Location names it under a state
// directory. TaskDir reads no file, so the path need not exist.
const fixtureWorkspace = "/state/workspaces/acme/widgets/widgets-42"

// taskDirInputs are the cases the fixture holds: every form of a task's branch, each word no slug
// may be, the shapes of a part that is no task's, and working copies outside Location's layout.
var taskDirInputs = []taskDirCase{
	{Name: "no branch", Workspace: fixtureWorkspace, Branch: ""},
	{Name: "the issue's own branch", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42"},
	{Name: "a slug's first give", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.b"},
	{Name: "a slug's second give", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.b.2"},
	{Name: "a give past nine", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.b.10"},
	{Name: "a slug of digits and a hyphen", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.t-0a"},
	{Name: "a slug of forty characters", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42." + strings.Repeat("s", 40)},
	{Name: "a slug of forty-one characters", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42." + strings.Repeat("s", 41)},
	{Name: "a branch of another issue", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-43.b"},
	{Name: "a branch of no issue", Workspace: fixtureWorkspace, Branch: "main"},
	{Name: "a key in lower case", Workspace: fixtureWorkspace, Branch: "legion/widgets-42.b"},
	{Name: "a slug in upper case", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.B"},
	{Name: "a part that leaves the directory", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.d/../x"},
	{Name: "an empty part", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42."},
	{Name: "a slug that begins with a hyphen", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.-b"},
	{Name: "the word lock", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.lock"},
	{Name: "the word removing", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.removing"},
	{Name: "the word join", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.join"},
	{Name: "a later give of the word join", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.join.2"},
	{Name: "a first give written with its number", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.b.1"},
	{Name: "a give written with a leading zero", Workspace: fixtureWorkspace, Branch: "legion/WIDGETS-42.b.02"},
	{Name: "a working copy in upper case", Workspace: "/state/workspaces/acme/widgets/WIDGETS-42", Branch: "legion/WIDGETS-42.b"},
	{Name: "a working copy with a trailing slash", Workspace: fixtureWorkspace + "/", Branch: "legion/WIDGETS-42.b"},
	{Name: "a working copy outside the directory of workspaces", Workspace: "/state/elsewhere/acme/widgets/widgets-42", Branch: "legion/WIDGETS-42.b"},
	{Name: "a working copy too shallow for the layout", Workspace: "/widgets-42", Branch: "legion/WIDGETS-42.b"},
	{Name: "a working copy of an owner GitHub refuses", Workspace: "/state/workspaces/-acme/widgets/widgets-42", Branch: "legion/WIDGETS-42.b"},
	{Name: "a working copy of a name GitHub refuses", Workspace: "/state/workspaces/acme/wid gets/widgets-42", Branch: "legion/WIDGETS-42.b"},
	{Name: "a working copy of a name ending in .git", Workspace: "/state/workspaces/acme/widgets.git/widgets-42", Branch: "legion/WIDGETS-42.b"},
	{Name: "the working copy of an issue in another repository", Workspace: "/state/workspaces/acme/gadgets/widgets-42", Branch: "legion/WIDGETS-42.b"},
}

// The plugin derives the directory of its step's working copy by TaskDir's rule, in TypeScript,
// so the two must agree on every case: this test writes TaskDir's answer to each one into
// packages/contracts/fixtures/daemon-api/task-dirs.json with -update, and fails where the file
// and what TaskDir answers now differ. The plugin's test reads the same file and holds its own
// answer to each case to it, so a rule changed on one side alone fails a test on the other.
func TestTheTaskDirFixture(t *testing.T) {
	fixture := taskDirsFixture{Cases: make([]taskDirCase, 0, len(taskDirInputs))}
	var answered, refused int
	for _, input := range taskDirInputs {
		answer := input
		dir, err := TaskDir(input.Workspace, input.Branch)
		if err != nil {
			answer.Refused = true
			refused++
		} else {
			answer.Dir = dir
			answered++
		}
		fixture.Cases = append(fixture.Cases, answer)
	}
	// A fixture of refusals alone, or of answers alone, would hold the plugin to half the rule.
	if answered == 0 || refused == 0 {
		t.Fatalf("TaskDir answered %d cases and refused %d: the fixture must hold both", answered, refused)
	}
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatalf("marshal task-dirs.json: %v", err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", "task-dirs.json")
	if *updateTaskDirs {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write task-dirs.json: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read task-dirs.json (run `go test ./internal/workspace/ -run TestTheTaskDirFixture -update`): %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("task-dirs.json is stale: TaskDir now answers\n%s\nwant\n%s", encoded, want)
	}
}
