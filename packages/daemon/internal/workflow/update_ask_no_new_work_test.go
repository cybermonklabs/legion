package workflow

import (
	"os"
	"strings"
	"testing"
)

// Main's UpdateTask (supervise/table.go at d46ec4e0) ended the update ask with "Do not start new
// work now.", a step from the moment the role reads the ask. INFRA-3686's R2: "a step that a text
// gives today goes into the reader's prompt where the prompt does not give it". The step row for
// it (texts_steps_test.go) points to "Start no new work after you answer.", which main's prompts
// already held and which forbids new work only after the answer; no field and no other sentence of
// either reader's update paragraph (go/worker-common.md for every phase worker,
// go/architect-common.md for the architect) forbids new work before it.
func TestTheUpdateAskForbidsNewWorkFromTheMomentTheRoleReadsIt(t *testing.T) {
	for _, path := range []string{"../prompts/go/worker-common.md", "../prompts/go/architect-common.md"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var line string
		for _, candidate := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(candidate, "The daemon can send an update message to your role topic.") {
				line = candidate
			}
		}
		if line == "" {
			t.Fatalf("%s has no update paragraph", path)
		}
		var sentences []string
		forbidsNow := false
		for _, sentence := range strings.SplitAfter(line, ". ") {
			if strings.Contains(sentence, "new work") {
				sentences = append(sentences, sentence)
				if !strings.Contains(sentence, "after you answer") {
					forbidsNow = true
				}
			}
		}
		if !forbidsNow {
			t.Errorf("%s: the update paragraph's only sentences on new work are %q: none forbids new work before the answer, as main's \"Do not start new work now.\" did", path, sentences)
		}
	}
}
