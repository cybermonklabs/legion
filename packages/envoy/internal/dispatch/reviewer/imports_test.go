package reviewer

import (
	"os/exec"
	"strings"
	"testing"
)

// sessionTextPackages are the packages of this module that hold an agent session's own text: its
// transcript, its live turns or an export of them, or a read of what was delivered to it. The
// harness's session type lives under cmd/writing-check, so every command is out as well. Each
// entry is that package and every package below it.
var sessionTextPackages = []string{
	"github.com/sjawhar/envoy/cmd",
	"github.com/sjawhar/envoy/internal/dispatch/api",
	"github.com/sjawhar/envoy/internal/dispatch/agentstream",
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers",
	"github.com/sjawhar/envoy/internal/session",
	"github.com/sjawhar/envoy/internal/legionagents",
}

// sessionTextDeps are the packages of pkg's imports, pkg included, directly or through another
// package (go list -deps), that hold session text.
func sessionTextDeps(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	deps := strings.Fields(string(out))
	if len(deps) == 0 {
		t.Fatalf("go list -deps %s listed no package", pkg)
	}
	var held []string
	for _, dep := range deps {
		for _, session := range sessionTextPackages {
			if dep == session || strings.HasPrefix(dep, session+"/") {
				held = append(held, dep)
			}
		}
	}
	return held
}

// The component reads no session text: it imports no package that holds it. The harness's own
// command imports one, which shows the check finds such an import.
func TestTheComponentImportsNoSessionText(t *testing.T) {
	if held := sessionTextDeps(t, "github.com/sjawhar/envoy/internal/dispatch/reviewer"); len(held) > 0 {
		t.Errorf("the reviewer imports %q, which hold session text", held)
	}
	if held := sessionTextDeps(t, "github.com/sjawhar/envoy/cmd/writing-check"); len(held) == 0 {
		t.Error("the check finds no session text in cmd/writing-check, which is one of the packages it names")
	}
}
