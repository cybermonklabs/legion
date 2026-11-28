package claude

//ci:reads .claude-code-pin TestPinnedVersionIsTheRepositorysPin reads it from the repository root (runtime.Caller + ../../../../..)

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPinnedVersionIsTheRepositorysPin checks the appender's fallback version against the
// repository pin. The contract tests check the real binary against the same PinnedVersion.
func TestPinnedVersionIsTheRepositorysPin(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate this test's source")
	}
	pin, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../../../../.claude-code-pin"))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(pin)); want != PinnedVersion {
		t.Errorf("PinnedVersion = %q, want %q, the version .claude-code-pin names", PinnedVersion, want)
	}
}
