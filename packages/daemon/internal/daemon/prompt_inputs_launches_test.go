package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// This file carries no build tag, so the prompt-inputs proof, a driver under `-tags e2e` in this
// package, reads the same table the untagged test below holds to the code.

// promptInputsLaunch is one launch the daemon composes a prompt for: a role on a backend, in one
// of the shapes its composition tells apart (an architect on its tree's root or on a child issue,
// a controller the daemon launches headless or the operator's own), with the prompt parts that
// launch is given, in order.
type promptInputsLaunch struct {
	// Name is the launch's stable name: the role's shape, then the backend it runs on.
	Name    string
	Role    claim.Role
	Backend runtime.Backend
	// Parts are the launch's prompt parts, each by its path below packages/daemon/internal/prompts:
	// `roles/<file>` for a shared role part, which boot snapshots to <state>/prompts/shared/<file>,
	// and `go/<file>` for a daemon part, snapshotted to <state>/prompts/go/<file> (prompts.New).
	Parts []string
}

// promptInputsLaunches are every launch the daemon composes a prompt for, each with the parts
// the composition gives it: every role on every backend of runtime.Backends that runs it, the
// architect on its tree's root and on a child issue, and the controller in both of
// ControllerPromptPaths' launches. TestThePromptInputsProofLaunchesEveryLaunch holds this table
// to the code.
var promptInputsLaunches = []promptInputsLaunch{
	{"architect-root/omp", claim.RoleArchitect, runtime.BackendOmp, []string{
		"roles/architect-root.md", "go/architect-root.md", "go/architect-common.md"}},
	{"architect-child/omp", claim.RoleArchitect, runtime.BackendOmp, []string{
		"roles/architect.md", "go/architect.md", "go/architect-common.md"}},
	{"planner/omp", claim.RolePlanner, runtime.BackendOmp, []string{
		"roles/core/common.md", "roles/core/planner.md", "roles/mechanics/headless.md", "roles/planner.md",
		"go/planner.md", "go/worker-common.md"}},
	{"implementer/omp", claim.RoleImplementer, runtime.BackendOmp, []string{
		"roles/core/common.md", "roles/core/implementer.md", "roles/mechanics/headless.md", "roles/implementer.md",
		"go/implementer.md", "go/worker-common.md"}},
	{"tester/omp", claim.RoleTester, runtime.BackendOmp, []string{
		"roles/core/common.md", "roles/core/tester.md", "roles/mechanics/headless.md", "roles/tester.md",
		"go/tester.md", "go/worker-common.md"}},
	{"reviewer/omp", claim.RoleReviewer, runtime.BackendOmp, []string{
		"roles/core/common.md", "roles/core/reviewer.md", "roles/mechanics/headless.md", "roles/reviewer.md",
		"go/reviewer.md", "go/worker-common.md"}},
	{"merger/omp", claim.RoleMerger, runtime.BackendOmp, []string{
		"roles/mechanics/headless.md", "roles/merger.md", "go/merger.md", "go/worker-common.md"}},
	{"controller-headless/omp", claim.RoleController, runtime.BackendOmp, []string{
		"roles/controller-root.md", "go/controller-headless.md"}},
	{"controller-interactive/omp", claim.RoleController, runtime.BackendOmp, []string{
		"roles/controller-root.md", "go/controller-interactive.md"}},
	{"planner/claude", claim.RolePlanner, runtime.BackendClaude, []string{
		"roles/core/common.md", "roles/core/planner.md", "roles/mechanics/headless.md", "roles/planner.md",
		"go/planner.md", "go/worker-common.md", "go/claude-mechanics.md"}},
	{"implementer/claude", claim.RoleImplementer, runtime.BackendClaude, []string{
		"roles/core/common.md", "roles/core/implementer.md", "roles/mechanics/headless.md", "roles/implementer.md",
		"go/implementer.md", "go/worker-common.md", "go/claude-mechanics.md"}},
	{"tester/claude", claim.RoleTester, runtime.BackendClaude, []string{
		"roles/core/common.md", "roles/core/tester.md", "roles/mechanics/headless.md", "roles/tester.md",
		"go/tester.md", "go/worker-common.md", "go/claude-mechanics.md"}},
	{"reviewer/claude", claim.RoleReviewer, runtime.BackendClaude, []string{
		"roles/core/common.md", "roles/core/reviewer.md", "roles/mechanics/headless.md", "roles/reviewer.md",
		"go/reviewer.md", "go/worker-common.md", "go/claude-mechanics.md"}},
	{"merger/claude", claim.RoleMerger, runtime.BackendClaude, []string{
		"roles/mechanics/headless.md", "roles/merger.md", "go/merger.md", "go/worker-common.md", "go/claude-mechanics.md"}},
}

// promptInputsRow is one row of the prompt-inputs proof's evidence (prompt-inputs.sh), one line of
// JSON: what one launch of promptInputsLaunches puts before the model before its first act.
type promptInputsRow struct {
	// Launch is the name of the launch's row in promptInputsLaunches.
	Launch string `json:"launch"`
	// Parts are the launch's prompt parts in the order it is given them.
	Parts []promptInputsPart `json:"parts"`
	// AddressingBytes is the size of the addressing the daemon writes after the parts, and
	// TaskBytes the size of the task the first message carries, 0 where the launch is given none.
	AddressingBytes int `json:"addressing_bytes"`
	TaskBytes       int `json:"task_bytes"`
	// FirstRequest is the launch's first request to the model as the harness assembled it.
	FirstRequest promptInputsRequest `json:"first_request"`
	// RequiredReads are the files the launch is told to read before its first act.
	RequiredReads []promptInputsRead `json:"required_reads"`
	// TotalBytes is the first request's five parts and every required read, summed.
	TotalBytes int `json:"total_bytes"`
}

// promptInputsPart is one prompt part of a launch: its path as promptInputsLaunch.Parts names it,
// its size, and the SHA-256 of its bytes in lower-case hex.
type promptInputsPart struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// promptInputsRequest is a launch's first request to the model, in bytes by part: the system
// text, the tool definitions, the skill list, the dispatch-first skill the Envoy plugin injects,
// and the first message.
type promptInputsRequest struct {
	SystemTextBytes      int `json:"system_text_bytes"`
	ToolDefinitionsBytes int `json:"tool_definitions_bytes"`
	SkillListBytes       int `json:"skill_list_bytes"`
	DispatchFirstBytes   int `json:"dispatch_first_bytes"`
	FirstMessageBytes    int `json:"first_message_bytes"`
}

// promptInputsRead is one file a launch is told to read before its first act, by the path it is
// read at, and its size.
type promptInputsRead struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

// The table names every launch the daemon composes a prompt for, and no other, each with the parts
// the composition gives it, in order. The launches are derived from the code rather than listed:
// every role of claim.Roles and the controller on every backend of runtime.Backends that a runtime
// launches and that runs the role (BackendInfo.Runs), the architect in both of Compose's shapes,
// and the controller the operator starts. Each part list is what specs.SpawnSpec gives such a
// claim — the composer's parts, then the part the backend's entry names — or, for the operator's
// controller, what `legion controller start` composes. So a backend, a role or a part added later
// fails here until the table names it, and the proof never reads a launch the code no longer makes.
func TestThePromptInputsProofLaunchesEveryLaunch(t *testing.T) {
	t.Parallel()
	tabled := map[string]promptInputsLaunch{}
	for _, row := range promptInputsLaunches {
		if _, twice := tabled[row.Name]; twice {
			t.Errorf("promptInputsLaunches names %s twice", row.Name)
		}
		tabled[row.Name] = row
	}
	launched := map[string]bool{}
	for _, launch := range composedLaunches(t) {
		launched[launch.Name] = true
		row, ok := tabled[launch.Name]
		if !ok {
			t.Errorf("promptInputsLaunches misses the launch %s: the %s on %s, with parts %q", launch.Name, launch.Role, launch.Backend, launch.Parts)
			continue
		}
		if row.Role != launch.Role || row.Backend != launch.Backend || !slices.Equal(row.Parts, launch.Parts) {
			t.Errorf("promptInputsLaunches' %s is the %s on %s with parts %q; the daemon composes the %s on %s with parts %q",
				row.Name, row.Role, row.Backend, row.Parts, launch.Role, launch.Backend, launch.Parts)
		}
	}
	for _, row := range promptInputsLaunches {
		if !launched[row.Name] {
			t.Errorf("promptInputsLaunches names %s, which no code launches", row.Name)
		}
	}
}

// composedLaunches are every launch the daemon composes a prompt for, each named as
// promptInputsLaunches names it, with the parts its composition gives it.
func composedLaunches(t *testing.T) []promptInputsLaunch {
	t.Helper()
	stateDir := t.TempDir()
	composer, err := prompts.New(stateDir)
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	var launches []promptInputsLaunch
	add := func(shape string, role claim.Role, backend runtime.Backend, paths []string) {
		parts := make([]string, len(paths))
		for i, path := range paths {
			parts[i] = promptPartSource(t, stateDir, path)
		}
		launches = append(launches, promptInputsLaunch{Name: shape + "/" + string(backend), Role: role, Backend: backend, Parts: parts})
	}
	// A claim of role on issue of the tree ACME-1: the tree's root, or a child issue of it.
	on := func(role claim.Role, issue string) supervise.Claim {
		token, err := claim.NewToken("acme", issue, role)
		if err != nil {
			t.Fatal(err)
		}
		return supervise.Claim{Token: token, Project: "acme", Issue: issue, Tree: "ACME-1", Role: role}
	}
	for _, backend := range slices.Sorted(maps.Keys(runtime.Backends)) {
		info := runtime.Backends[backend]
		if len(info.Runtimes) == 0 { // a backend no runtime launches has no launch
			continue
		}
		s := specs{stateDir: stateDir, project: "acme", prompts: composer, backend: func(claim.Role) runtime.Backend { return backend }}
		spawn := func(c supervise.Claim) []string {
			t.Helper()
			spec, err := s.SpawnSpec(context.Background(), c)
			if err != nil {
				t.Fatalf("SpawnSpec of the %s on %s: %v", c.Role, backend, err)
			}
			if spec.Backend != backend {
				t.Fatalf("SpawnSpec of the %s on %s launches it on %s", c.Role, backend, spec.Backend)
			}
			return spec.Prompt.RolePromptPaths
		}
		for _, role := range append(slices.Clone(claim.Roles), claim.RoleController) {
			if !info.Runs(role) {
				continue
			}
			switch role {
			case claim.RoleArchitect:
				add("architect-root", role, backend, spawn(on(role, "ACME-1")))
				add("architect-child", role, backend, spawn(on(role, "ACME-2")))
			case claim.RoleController:
				add("controller-headless", role, backend, spawn(supervise.Claim{Token: claim.ControllerToken("acme"), Project: "acme", Role: role}))
			default:
				add(string(role), role, backend, spawn(on(role, "ACME-2")))
			}
		}
	}
	// `legion controller start` runs the operator's controller on the operator's own Oh My Pi,
	// outside runtime.Backends, and gives it ControllerPromptPaths(false) alone
	// (cmd/legion/controller.go).
	paths, err := composer.ControllerPromptPaths(false)
	if err != nil {
		t.Fatalf("ControllerPromptPaths(headless false): %v", err)
	}
	add("controller-interactive", claim.RoleController, runtime.BackendOmp, paths)
	return launches
}

// promptPartSource is path, a part prompts.New snapshotted below stateDir, as
// promptInputsLaunch.Parts names it. The snapshot must hold the bytes the repository's file of
// that name holds, so a proof that reads the repository's file measures what the launch reads.
func promptPartSource(t *testing.T, stateDir, path string) string {
	t.Helper()
	rel, err := filepath.Rel(filepath.Join(stateDir, "prompts"), path)
	if err != nil {
		t.Fatalf("part %s: %v", path, err)
	}
	var source string
	switch dir, file, _ := strings.Cut(filepath.ToSlash(rel), "/"); dir {
	case "shared":
		source = "roles/" + file
	case "go":
		source = "go/" + file
	default:
		t.Fatalf("part %s is no part prompts.New snapshots below %s", path, stateDir)
	}
	snapshot, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the snapshotted part %s: %v", path, err)
	}
	repository, err := os.ReadFile(filepath.Join("..", "prompts", filepath.FromSlash(source)))
	if err != nil {
		t.Fatalf("read the repository's part %s: %v", source, err)
	}
	if !bytes.Equal(snapshot, repository) {
		t.Fatalf("the snapshotted part %s does not hold the bytes of packages/daemon/internal/prompts/%s", path, source)
	}
	return source
}

// A row of the prompt-inputs proof's evidence reads from the JSON line prompt-inputs.sh writes,
// with no field this type lacks, and writes back to that line byte for byte, so the driver and
// the script agree on every field's name.
func TestPromptInputsRowRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	const line = `{"launch":"merger/omp",` +
		`"parts":[{"path":"roles/mechanics/headless.md","bytes":1024,"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},` +
		`{"path":"roles/merger.md","bytes":2048,"sha256":"1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},` +
		`{"path":"go/merger.md","bytes":4096,"sha256":"2123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},` +
		`{"path":"go/worker-common.md","bytes":8192,"sha256":"3123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}],` +
		`"addressing_bytes":512,"task_bytes":300,` +
		`"first_request":{"system_text_bytes":40000,"tool_definitions_bytes":20000,"skill_list_bytes":3000,"dispatch_first_bytes":5000,"first_message_bytes":400},` +
		`"required_reads":[{"path":"skill://legion-worker","bytes":7000}],` +
		`"total_bytes":75400}`
	want := promptInputsRow{
		Launch: "merger/omp",
		Parts: []promptInputsPart{
			{Path: "roles/mechanics/headless.md", Bytes: 1024, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			{Path: "roles/merger.md", Bytes: 2048, SHA256: "1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			{Path: "go/merger.md", Bytes: 4096, SHA256: "2123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			{Path: "go/worker-common.md", Bytes: 8192, SHA256: "3123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		},
		AddressingBytes: 512,
		TaskBytes:       300,
		FirstRequest: promptInputsRequest{
			SystemTextBytes: 40000, ToolDefinitionsBytes: 20000, SkillListBytes: 3000, DispatchFirstBytes: 5000, FirstMessageBytes: 400,
		},
		RequiredReads: []promptInputsRead{{Path: "skill://legion-worker", Bytes: 7000}},
		TotalBytes:    75400,
	}
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.DisallowUnknownFields()
	var got promptInputsRow
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("decode the evidence row: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded row = %+v, want %+v", got, want)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode the evidence row: %v", err)
	}
	if string(encoded) != line {
		t.Fatalf("the evidence row encodes as\n%s\nwant\n%s", encoded, line)
	}
}
