package workflow

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Main's note of the memory guard's kill (workflow/engine.go, memoryGuardNote and
// memoryKillHeavyWork, at d46ec4e0 and c03d3afe) ends: "Heavy work — a build, a whole test suite,
// anything that needs much memory — runs where your deployment's instructions say, not in this pod:
// read skill://legion-worker/references/tools.md before you run it again." dispatch://INFRA-3557's
// W4 (done, a child that its parent dispatch://INFRA-2763's spec, approved at version 10, covers in
// its R7) has that note say it: the daemon "publishes a note on that role's own Envoy role topic
// ..., saying that the guard stopped that process for memory, named by its executable, its pid,
// its size and the limit, and that heavy work runs where its deployment's instructions say." The
// Design's "Approved requirements this changes" names neither issue, and dispatch://INFRA-3686's R8
// says that "a fact or a step that it has the text give stays as a field". So the guard's
// memory-kill record holds a field that says where heavy work runs, beside the cause, the process,
// its pid, its size, the pod's reading and the limit.
func TestAGuardMemoryKillSaysAsAFieldWhereHeavyWorkRuns(t *testing.T) {
	report := shimwire.LauncherMemoryKill{PID: 4001, Executable: "node", SizeBytes: 4 << 30, ReadingBytes: 7 << 30, CounterBytes: 8 << 30,
		LimitBytes: 8 << 30, Cause: shimwire.MemoryKillCauseGuard}
	encoded, err := json.Marshal(MemoryKillMessage("LEGION-41", report))
	if err != nil {
		t.Fatalf("encode the memory-kill message: %v", err)
	}
	var published map[string]any
	if err := json.Unmarshal(encoded, &published); err != nil {
		t.Fatalf("decode %s: %v", encoded, err)
	}
	var kept []string
	for key := range published {
		switch key {
		case "kind", "issue", "cause", "process", "pid", "size", "pod_memory", "limit":
		default:
			kept = append(kept, key)
		}
	}
	if len(kept) == 0 {
		t.Errorf("the guard's memory-kill message is %s: no field says that heavy work runs where the deployment's instructions say (INFRA-3557 W4)", encoded)
	}
}

// memoryKillParagraph is the paragraph of the prompt files at paths that explains the memory-kill
// message, "" where none does.
func memoryKillParagraph(t *testing.T, paths []string) string {
	t.Helper()
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "A `memory-kill` message") {
				return line
			}
		}
	}
	return ""
}

// Main's note of the memory guard's kill (workflow/engine.go, memoryGuardNote, at d46ec4e0 and
// c03d3afe) says what its two sizes measure: "%s (pid %d) held %s of anonymous memory while the
// pod's anonymous memory read %s of its %s limit." The kernel's note measures the pod's memory
// counter instead: "held %s when the pod's memory read %s of its %s limit" (kernelKillLead). The
// branch's record keeps the numbers (MemoryKillMessage puts the guard's anonymous reading in
// pod_memory and the kernel's counter in the same field), and dispatch://INFRA-3686's R2 has the
// reader's prompt give what the text no longer does: "a step that a text gives today goes into
// the reader's prompt where the prompt does not give it", the record's fields being "the state
// that the reader acts on". So every reader's prompt (each role whose process the guard can stop:
// the five phase workers and both architects) says that with `cause: guard` the sizes are
// anonymous memory.
func TestEveryReaderOfAGuardMemoryKillIsToldItsSizesAreAnonymousMemory(t *testing.T) {
	report := shimwire.LauncherMemoryKill{PID: 4001, Executable: "node", SizeBytes: 4 << 30, ReadingBytes: 7 << 30, CounterBytes: 8 << 30,
		LimitBytes: 8 << 30, Cause: shimwire.MemoryKillCauseGuard}
	message := MemoryKillMessage("LEGION-41", report)
	if message.Cause != "guard" || message.PodMemory != binarySize(report.ReadingBytes) {
		t.Fatalf("the guard's message holds cause %q and pod_memory %q, not the guard's anonymous reading %q", message.Cause, message.PodMemory,
			binarySize(report.ReadingBytes))
	}
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("prompts: %v", err)
	}
	readers := []struct {
		name string
		role claim.Role
		root bool
	}{
		{"planner", claim.RolePlanner, false}, {"implementer", claim.RoleImplementer, false}, {"tester", claim.RoleTester, false},
		{"reviewer", claim.RoleReviewer, false}, {"merger", claim.RoleMerger, false},
		{"root architect", claim.RoleArchitect, true}, {"child architect", claim.RoleArchitect, false},
	}
	for _, reader := range readers {
		parts, err := composer.Compose(reader.role, reader.root)
		if err != nil {
			t.Fatalf("%s: compose: %v", reader.name, err)
		}
		paragraph := memoryKillParagraph(t, parts.RolePromptPaths)
		if paragraph == "" {
			t.Errorf("%s: no prompt file explains the memory-kill message", reader.name)
			continue
		}
		if !strings.Contains(paragraph, "anonymous") {
			t.Errorf("%s: the prompt's memory-kill paragraph names `size` and `pod_memory` as how much the process held and the pod's memory reading, "+
				"and never says that with `cause: guard` both are anonymous memory, as main's note did: %q", reader.name, paragraph[:min(len(paragraph), 330)])
		}
	}
}
