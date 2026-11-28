// The tests of daemon-pane.go's own copies of the daemon, compiled with the program as
// cmd/daemon-pane's main_test.go inside this checkout's daemon module (goInDaemonModule in
// daemon-pane.ts, run by a row of daemon-pane.test.ts). They name the daemon's own functions, which
// an older checkout lacks, so no row compiles them in another checkout: they hold each copy to the
// daemon of the checkout they belong to.
package main

import (
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The program's rule of an owner's launch answers daemon.OwnsPlan for the rig's claim of each role,
// on the root issue of its tree and on a child under it, its read of a guidance file answers what
// config.ReadPlanGuidance answers, and its copy of a plan guidance is the file
// config.WritePlanGuidance writes, so a change of any of them in the daemon fails here until the
// program takes it.
func TestTheToolRestatesTheDaemonsPlanGuidance(t *testing.T) {
	compared := 0
	for _, role := range claim.Roles {
		for _, root := range []bool{true, false} {
			c, err := rigClaim(role, root)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := ownsPlan(c), daemon.OwnsPlan(c); got != want {
				t.Errorf("ownsPlan of the %s on %s (tree %s) answers %t, daemon.OwnsPlan %t", role, c.Issue, rigRoot, got, want)
			}
			compared++
		}
	}
	if compared == 0 || compared != 2*len(claim.Roles) {
		t.Errorf("compared %d claims, want two for each of the %d roles of claim.Roles, and more than none", compared, len(claim.Roles))
	}

	const guidance = "Plant 7f3a.\n"
	stateDir := t.TempDir()
	if err := config.WritePlanGuidance([]byte(guidance), stateDir, rigProject); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(config.PlanGuidanceCopy(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if got := planGuidanceCopy(rigProject, []byte(guidance)); got != string(written) {
		t.Errorf("planGuidanceCopy(%q, %q) = %q, want the file config.WritePlanGuidance writes, %q", rigProject, guidance, got, written)
	}

	dir := t.TempDir()
	// Each file the daemon's read takes or refuses: a guidance, a blank one, an absent one, the
	// largest the bound takes and one byte over it, a directory and a FIFO.
	files := map[string]string{
		"guidance": guidance, "blank": "\n  \n", "absent": "",
		"largest": strings.Repeat("g", config.PlanGuidanceMaxBytes), "over": strings.Repeat("g", config.PlanGuidanceMaxBytes+1),
	}
	paths := map[string]string{"a directory": dir, "a FIFO": dir + "/fifo.md"}
	if err := syscall.Mkfifo(paths["a FIFO"], 0o600); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		paths[name] = dir + "/" + name + ".md"
		if name != "absent" {
			if err := os.WriteFile(paths[name], []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, file := range paths {
		got, gotErr := readPlanGuidance(file)
		want, wantErr := config.ReadPlanGuidance(file)
		if string(got) != string(want) || (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
			t.Errorf("readPlanGuidance of %s = %q, %v; config.ReadPlanGuidance = %q, %v", name, got, gotErr, want, wantErr)
		}
	}
}

// The prompt the program prints for a launch, with a plan guidance and without one, is the prompt
// runtime.ComposePrompt composes from the same parts: the program's join of the role's parts, the
// addressing and the guidance copy is the daemon's, part for part and blank line for blank line.
func TestTheToolJoinsAPromptAsTheDaemonDoes(t *testing.T) {
	guidance := []byte("Plant 7f3a.\n")
	guidanceDir := t.TempDir()
	if err := config.WritePlanGuidance(guidance, guidanceDir, rigProject); err != nil {
		t.Fatal(err)
	}
	copied := config.PlanGuidanceCopy(guidanceDir)
	compared := 0
	for _, role := range claim.Roles {
		for _, root := range []bool{true, false} {
			c, err := rigClaim(role, root)
			if err != nil {
				t.Fatal(err)
			}
			stateDir := t.TempDir()
			paths, addressing, err := rolePromptInputs(stateDir, rigProject, c)
			if err != nil {
				t.Fatal(err)
			}
			// The daemon gives the guidance to an owner's launch alone (workerSpawnSpec), so the
			// parts it composes for every other role name no guidance file.
			parts := runtime.PromptParts{RolePromptPaths: paths, Addressing: addressing}
			if daemon.OwnsPlan(c) {
				parts.PlanGuidancePath = copied
			}
			want, err := runtime.ComposePrompt(parts)
			if err != nil {
				t.Fatal(err)
			}
			got, err := guidedPrompt(stateDir, c, guidance)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("the prompt of the %s on %s with a plan guidance is\n%q\nand runtime.ComposePrompt's is\n%q", role, c.Issue, got, want)
			}
			compared++
		}
	}
	if compared == 0 || compared != 2*len(claim.Roles) {
		t.Errorf("compared %d prompts, want two for each of the %d roles of claim.Roles, and more than none", compared, len(claim.Roles))
	}
}
