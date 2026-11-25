package texts_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A fault is what an operator reads in the daemon's log and a notice, so each cause renders a
// sentence of its own beside the folder, the file and the cause's word, and Causes, the list the
// launch tables of the daemon and the sandbox runtime are held to, holds each cause once. A cause
// added without a sentence reads as a bare refusal, which this fails.
func TestEachFaultSaysWhatIsWrongWhereAndWhy(t *testing.T) {
	if len(texts.Causes) != 19 {
		t.Errorf("Causes holds %d causes, want the 19 of R2, R3 and the plan's fallbacks: %q", len(texts.Causes), texts.Causes)
	}
	sentences := map[string]texts.Cause{}
	for i, cause := range texts.Causes {
		if slices.Contains(texts.Causes[:i], cause) {
			t.Errorf("Causes holds %s twice", cause)
		}
		file := texts.Fault{Dir: "/etc/arm", Path: "roles/reviewer.md", Cause: cause, Detail: "what the check saw"}.Error()
		for _, word := range []string{"/etc/arm", "roles/reviewer.md", "what the check saw", "[" + string(cause) + "]"} {
			if !strings.Contains(file, word) {
				t.Errorf("the %s fault %q does not name %q", cause, file, word)
			}
		}
		sentence := strings.TrimSuffix(strings.TrimPrefix(texts.Fault{Cause: cause}.Error(), "texts folder: "), " ["+string(cause)+"]")
		if sentence == "" || strings.Contains(sentence, "refused") {
			t.Errorf("the %s fault renders %q, which says nothing of what is wrong", cause, sentence)
		}
		if earlier, ok := sentences[sentence]; ok {
			t.Errorf("the %s and %s faults both say %q", earlier, cause, sentence)
		}
		sentences[sentence] = cause
	}
	if folder := (texts.Fault{Dir: "/etc/arm", Cause: texts.Gone}).Error(); !strings.HasPrefix(folder, "texts folder /etc/arm: the folder does not exist") {
		t.Errorf("a fault of the folder as a whole renders %q, want it to name the folder and no file", folder)
	}
}
