package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/going/agentwire"
)

// TestAGitHubReadReadsOnlyWhatTheBarAnswersFrom: a GitHub read of the agent's, of a repository a
// project the person reads maps, reads a commit through the App, and refuses a path that is none of
// the reads the bar answers from (goingGitHubReadable) - the repository's hooks, its collaborators -
// before it asks GitHub anything, though the App could read them.
func TestAGitHubReadReadsOnlyWhatTheBarAnswersFrom(t *testing.T) {
	t.Parallel()
	rig := newGoingAgentRig(t, nil)
	// The rig's GitHub App reads acme/arch (goingFixtureArchitectureGitHub); DEMO maps it.
	rig.exec(t, `update projects set default_repo = 'acme/arch' where key = 'DEMO'`)
	run, token := rig.working(t, "alice", "read acme/arch on GitHub")
	read := func(call int, path string) (agentwire.CallAnswer, error) {
		return rig.call(agentwire.CallRequest{Run: run, LeaseToken: token, Step: 1, Call: call, Kind: agentwire.KindGitHub,
			Repository: "acme/arch", Path: path})
	}

	if answer, err := read(1, "commits/"+shaInfra); err != nil || answer.Status != http.StatusOK {
		t.Fatalf("a read of a commit of acme/arch answered %d %s, %v; want GitHub's 200", answer.Status, answer.Body, err)
	}
	for call, path := range []string{"hooks", "collaborators?per_page=100", "collaborators/alice/permission"} {
		_, err := read(call+2, path)
		requireAgentCode(t, err, agentwire.CodeCallRefused, "a GitHub read of acme/arch's "+path)
	}
}
