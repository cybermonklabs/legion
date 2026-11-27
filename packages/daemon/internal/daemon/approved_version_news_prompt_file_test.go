//go:build e2e

package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Part two reads each pane's prompt back from the file its `--append-system-prompt` word names
// (launchedPrompt) and holds the arm to it (checkArm): the head arm's prompt carries the head's
// sentence and the base arm's does not. A pane whose file holds no prompt at all is neither arm's,
// so the run must stop rather than count that pane's outcome for the base arm: a base run judged
// on a pane with no role prompt would make the control bite for a reason the prompts under test
// do not give. A file that holds the base commit's prompt is the base arm's, and passes.
//
// checkArm ends the run through its testing.T, so each case runs it in a child of this test
// binary and reads whether that child failed.
func TestTheBaseArmRefusesAPaneWhosePromptFileHoldsNoPrompt(t *testing.T) {
	t.Parallel()
	if os.Getenv(promptFileChildEnv) != "" {
		runPromptFileChild(t)
		return
	}
	for _, tc := range []struct {
		name   string
		prompt string
		fails  bool
	}{
		{"an empty prompt file", "", true},
		{"a file holding only whitespace", "\n\n", true},
		{"a file holding the base commit's prompt", "# Legion Reviewer\n\nReview the pull request.\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "reviewer-system-prompt.md")
			if err := os.WriteFile(file, []byte(tc.prompt), 0o600); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run", "^TestTheBaseArmRefusesAPaneWhosePromptFileHoldsNoPrompt$", "-test.count=1")
			child.Env = append(os.Environ(), promptFileChildEnv+"="+file)
			out, err := child.CombinedOutput()
			if failed := err != nil; failed != tc.fails {
				t.Fatalf("the base arm's check of a pane launched with %s failed %t, want %t; the child said:\n%s", tc.name, failed, tc.fails, out)
			}
		})
	}
}

const promptFileChildEnv = "APPROVED_VERSION_NEWS_PROMPT_FILE_CHILD"

// runPromptFileChild runs checkArm over one pane of the base arm: a child process of this test,
// standing in for the shim's Oh My Pi, launched with the prompt file the parent names, and the
// operator's claim listing of a stand-in daemon naming this process as that claim's shim.
func runPromptFileChild(t *testing.T) {
	file := os.Getenv(promptFileChildEnv)
	omp := filepath.Join(t.TempDir(), "omp")
	pane := exec.Command("/bin/sh", "-c", "sleep 30", promptWord, file)
	pane.Args[0] = omp
	if err := pane.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pane.Process.Kill(); _ = pane.Wait() })

	const project = "as0001"
	member := &sessionsMember{key: "AS0001-2"}
	token, err := claim.NewToken(project, member.key, claim.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.OperatorClaims{Claims: []api.OperatorClaim{{Token: token, State: "working",
			Locator: &runtime.Locator{Runtime: "tmux", Claim: token, Incarnation: strconv.Itoa(os.Getpid()) + ":1"}}}})
	}))
	t.Cleanup(listing.Close)
	run := &sessionsRun{arm: sessionsBase}
	run.t, run.project, run.omp = t, project, omp
	run.d = &daemon{t: t, client: listing.Client(), base: listing.URL}
	run.checkArm(member, claim.RoleReviewer, sessionsWorkerMark)
}
