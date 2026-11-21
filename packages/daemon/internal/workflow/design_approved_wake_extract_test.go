package workflow

import (
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The product half of the design-approved wake's check: design_approved_wake_extract.txt, the copy
// internal/textcheck holds the architect's instructions to, is texts.DesignApprovedWake, the
// sentence this package's resume task keeps (RootResume).
// The text half is internal/textcheck's
// TestTheArchitectsInstructionsTakeTheResumeAsTheDesignApprovedWake; neither alone is the guarantee.
func TestTheDesignApprovedWakeExtractIsTheConstant(t *testing.T) {
	const extract = "design_approved_wake_extract.txt"
	body, err := os.ReadFile(extract)
	if err != nil {
		t.Fatalf("read %s: %v", extract, err)
	}
	_, phrase, ok := strings.Cut(string(body), "\n\n")
	if !ok {
		t.Fatalf("%s has no blank line between its `#` header and the phrase", extract)
	}
	if got := strings.TrimSuffix(phrase, "\n"); got != texts.DesignApprovedWake {
		t.Errorf("%s holds %q, and texts.DesignApprovedWake is %q: change them in one commit", extract, got, texts.DesignApprovedWake)
	}
}
