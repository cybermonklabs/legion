package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
)

// A worker whose agent registered a session and was then stopped still records that session, and
// `legion claims retry --fresh-session` starts the claim in a new session rather than resuming
// it: the runtime is asked to spawn, with no transcript to resume. A retry without the flag
// resumes the session a stopped claim records, so the new session here is the flag's doing. The
// claim is a phase worker's because the daemon refuses the operator's stop of the tree's root
// claim.
func TestClaimsRetryFreshSessionStartsAStoppedClaimInANewSession(t *testing.T) {
	t.Parallel()
	d := newOperatorDaemon(t)
	d.succeed(d.spawnArgs()...)
	d.ready(architectClaim)
	const workerClaim = legionclaim.Token("legion-legion-legion-209-implementer")
	d.succeed(append(append([]string{"spawn"}, d.reach()...), "--tree", "LEGION-208", "--issue", "LEGION-209",
		"--role", "implementer", "--prompt-file", writeFile(t, "implementer.md", "Implement."))...)
	d.ready(workerClaim)

	stopped := d.succeed(append(append([]string{"stop"}, d.reach()...), "--claim", string(workerClaim))...)
	if !strings.Contains(stopped, " retired ") {
		t.Fatalf("stop stdout = %q, want the worker retired", stopped)
	}
	m, ok := d.claimSupervisor.Machine(workerClaim)
	if !ok {
		t.Fatalf("the daemon supervises no %s", workerClaim)
	}
	if c := m.Claim(); c.Session != "ses_"+string(workerClaim) || c.SessionFile == "" {
		t.Fatalf("the stopped worker records session %q (%q), want the one its agent registered", c.Session, c.SessionFile)
	}
	spawns, resumes := len(d.runtime.CallsOf("Spawn")), len(d.runtime.CallsOf("Resume"))

	out := d.succeed(append(append([]string{"retry"}, d.reach()...), "--claim", string(workerClaim), "--fresh-session")...)

	seen := d.lastOperatorRequest()
	wantOperatorRequest(t, seen, http.MethodPost, "/legion/v1/operator/claims/"+string(workerClaim)+"/retry")
	if want := `{"freshSession":true}`; string(seen.body) != want {
		t.Fatalf("retry --fresh-session sent %q, want %q", seen.body, want)
	}
	if !strings.Contains(out, " launching ") {
		t.Fatalf("retry --fresh-session stdout = %q, want the claim launching again", out)
	}
	if got := len(d.runtime.CallsOf("Resume")); got != resumes {
		t.Fatalf("the runtime saw %d resumes, want the stopped session left alone", got-resumes)
	}
	started := d.runtime.CallsOf("Spawn")
	if len(started) != spawns+1 {
		t.Fatalf("the runtime saw %d spawns, want the retry's fresh start", len(started)-spawns)
	}
	if file := started[len(started)-1].Spec.ResumeSessionFile; file != "" {
		t.Fatalf("the fresh start resumes %q, want a session of its own", file)
	}
}

// A fresh session whose phase restart the workflow refused answers 200 with the claim and that
// refusal (api.FreshSessionResponse.PhaseNotRestarted, served on the operator's retry as
// api.OperatorClaim.PhaseNotRestarted). The claim's own line says the claim is back, so the
// refusal is a line of its own under it: without it an operator reads a success and no word of
// the work nothing handed the claim. Every other answer carries no refusal and prints the
// claim's line alone.
//
// The claim is a phase worker's, since that is the only claim the daemon writes this answer for:
// an architect is never started through a phase at all (api's freshSession).
func TestARetryWhosePhaseRestartWasRefusedPrintsTheRefusalUnderTheClaim(t *testing.T) {
	t.Parallel()
	const implementerClaim = legionclaim.Token("legion-legion-legion-209-implementer")
	retried := api.OperatorClaim{Token: implementerClaim, Tree: "LEGION-208", Issue: "LEGION-209",
		Role: "implementer", Generation: 3, State: "launching", PluginContract: api.DaemonAPIVersion()}
	line := string(implementerClaim) + " implementer LEGION-209 launching - 3 " + thisBinarysContract + " - - - -\n"
	refusal := "LEGION-209 is still held after the retry of its phase implementing, so nothing relaunched " +
		string(implementerClaim) + ": its tree lingers, and a lingering tree's members stay where they stood"
	for _, tc := range []struct {
		name  string
		claim api.OperatorClaim
		want  string
	}{
		{name: "the phase was restarted", claim: retried, want: line},
		{
			name: "the workflow refused the restart",
			claim: func() api.OperatorClaim {
				c := retried
				c.PhaseNotRestarted = "the claim is back in a new session owing no task, and nothing restarted its phase: " + refusal
				return c
			}(),
			want: line + "the claim is back in a new session owing no task, and nothing restarted its phase: " + refusal + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer, err := json.Marshal(tc.claim)
			if err != nil {
				t.Fatalf("marshal the claim the route answers: %v", err)
			}
			var out bytes.Buffer
			if err := printClaim(&out, answer); err != nil {
				t.Fatalf("print the claim: %v", err)
			}
			if out.String() != tc.want {
				t.Fatalf("printed %q, want %q", out.String(), tc.want)
			}
		})
	}
}
