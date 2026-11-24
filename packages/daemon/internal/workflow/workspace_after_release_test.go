package workflow

import (
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The wake task of a release says what stopped the session and nothing the daemon does not know
// of the workspace: it names stopped_by: release and holds none of the words that unpushed work
// was kept, which the release before said whatever its restore did, nor that nothing unpushed was
// lost, which a side head breaks. A pause keeps the pod, so its wake task names stopped_by: pause.
// The wake's record carries no prose: each reader's prompt says what the daemon knows of the
// workspace, in its one passage on the pod-wake task, and that passage says none of those words
// either. Its own words are held to main's wake by the daemon's step table
// (TestEveryStepTakenOutOfATextIsInItsReadersPrompt), and the prompt's passage on the plugin's
// workspace recreated notice, its words and the words it never says, by the plugin's test
// (packages/pi-shared/src/workspace-recreated.test.ts).
func TestTheReleasesWakeTaskNeverSaysUnpushedWorkWasKept(t *testing.T) {
	kept := []string{"any work you had not pushed are as you left them", "saved", "put back", "restored", "holds nothing that was not pushed"}
	for _, kind := range []string{pods.WakeChecks, pods.WakeRoleMessage, pods.WakeAgentMessage, pods.WakeBackstop, pods.WakeDelivery} {
		task := WakeTask("LEGION-208", kind, "its detail", StoppedByRelease)
		if got := readTaskOf(t, task).fields["stopped_by"]; got != texts.StoppedByRelease {
			t.Errorf("WakeTask(%s) after a release names stopped_by %q, want %q", kind, got, texts.StoppedByRelease)
		}
		for _, word := range kept {
			if strings.Contains(task, word) {
				t.Errorf("WakeTask(%s) after a release = %q, want no %q: the daemon keeps no unpushed work", kind, task, word)
			}
		}
		paused := WakeTask("LEGION-208", kind, "its detail", StoppedByPause)
		if got := readTaskOf(t, paused).fields["stopped_by"]; got != texts.StoppedByPause {
			t.Errorf("WakeTask(%s) after a pause names stopped_by %q, want %q", kind, got, texts.StoppedByPause)
		}
	}

	for _, path := range []string{"../prompts/go/worker-common.md", "../prompts/go/architect-common.md"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var passages []string
		for _, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "A task of the kind `pod-wake`") {
				passages = append(passages, line)
			}
		}
		if len(passages) != 1 {
			t.Fatalf("%s holds %d passages on the pod-wake task, want one", path, len(passages))
		}
		for _, word := range kept {
			if strings.Contains(passages[0], word) {
				t.Errorf("%s's passage on the pod-wake task says %q: the daemon keeps no unpushed work", path, word)
			}
		}
	}
}
