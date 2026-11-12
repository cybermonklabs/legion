package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// registerAndReady brings the claim's agent up as the plugin does and answers what it learned:
// the secret its later calls, the turn outcomes among them, authenticate with.
func (d *operatorDaemon) registerAndReady(token legionclaim.Token) legionclaim.RegisterResponse {
	d.t.Helper()
	bootToken := ""
	for _, call := range d.runtime.CallsOf("Spawn") {
		if call.Spec.Claim == token {
			bootToken = call.Spec.BootToken
		}
	}
	if bootToken == "" {
		d.t.Fatalf("no launch of %s was made", token)
	}
	session := "ses_" + string(token)
	var registered legionclaim.RegisterResponse
	d.post("/legion/v1/claims/register", legionclaim.RegisterRequest{
		BootToken: bootToken, SessionID: session, OmpSessionFile: "/sessions/" + string(token) + ".jsonl",
		AgentID: "agent", PluginContract: api.DaemonAPIVersion,
	}, &registered)
	d.post("/legion/v1/claims/ready", legionclaim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: registered.Generation,
	}, nil)
	return registered
}

// A claim whose provider refuses launch_failure_limit turns in a row fails with the provider's
// refusal as its cause, and `legion claims retry --fresh-session` starts it in a new session
// rather than resuming the one every turn of which was refused: the runtime is asked to spawn,
// with no transcript to resume.
func TestClaimsRetryFreshSessionStartsTheRefusedClaimInANewSession(t *testing.T) {
	d := newOperatorDaemon(t)
	d.succeed(d.spawnArgs()...)
	registered := d.registerAndReady(architectClaim)

	var answer legionclaim.TurnOutcomeResponse
	for turn := uint64(1); turn <= 3; turn++ {
		answer = legionclaim.TurnOutcomeResponse{}
		d.post("/legion/v1/claims/turn-outcome", legionclaim.TurnOutcomeRequest{
			ClaimToken: architectClaim, SessionID: "ses_" + string(architectClaim), Secret: registered.Secret,
			Generation: 1, Sequence: turn, Outcome: legionclaim.TurnRefused,
			Message: "the provider refused the request",
		}, &answer)
	}
	if answer != (legionclaim.TurnOutcomeResponse{RefusedRun: 3, Failed: true}) {
		t.Fatalf("the third refusal answered %+v, want a run of 3 that failed the claim", answer)
	}
	if out := d.succeed(append([]string{"list"}, d.reach()...)...); !strings.Contains(out, " failed ") {
		t.Fatalf("the claim list reads %q, want the claim failed by its provider's refusals", out)
	}
	spawns, resumes := len(d.runtime.CallsOf("Spawn")), len(d.runtime.CallsOf("Resume"))

	out := d.succeed(append(append([]string{"retry"}, d.reach()...), "--claim", string(architectClaim), "--fresh-session")...)

	seen := d.lastOperatorRequest()
	wantOperatorRequest(t, seen, http.MethodPost, "/legion/v1/operator/claims/"+string(architectClaim)+"/retry")
	if want := `{"freshSession":true}`; string(seen.body) != want {
		t.Fatalf("retry --fresh-session sent %q, want %q", seen.body, want)
	}
	if !strings.Contains(out, " launching ") {
		t.Fatalf("retry --fresh-session stdout = %q, want the claim launching again", out)
	}
	if got := len(d.runtime.CallsOf("Resume")); got != resumes {
		t.Fatalf("the runtime saw %d resumes, want the refused session left alone", got-resumes)
	}
	started := d.runtime.CallsOf("Spawn")
	if len(started) != spawns+1 {
		t.Fatalf("the runtime saw %d spawns, want the retry's fresh start", len(started)-spawns)
	}
	if file := started[len(started)-1].Spec.ResumeSessionFile; file != "" {
		t.Fatalf("the fresh start resumes %q, want a session of its own", file)
	}
}

// `legion claims retry --fresh-session` sends the one body the retry route reads,
// `{"freshSession":true}`, and the claim starts a new session rather than resuming the one it
// recorded: what a claim whose provider refused its session needs, since resuming it would be
// refused again. A retry without the flag sends no body at all
// (TestClaimsRetryPostsToTheClaimsRetryRoute).
func TestClaimsRetryFreshSessionSendsTheFreshSessionBody(t *testing.T) {
	d := newOperatorDaemon(t)
	refused := fake.SpawnResult{Err: errors.New("the runtime refused the launch")}
	d.runtime.ScriptSpawn(refused, refused, refused)
	if code, _, _ := d.claims(d.spawnArgs()...); code != 1 {
		t.Fatalf("spawn whose every launch the runtime refused exited %d, want 1", code)
	}

	out := d.succeed(append(append([]string{"retry"}, d.reach()...), "--claim", string(architectClaim), "--fresh-session")...)

	seen := d.lastOperatorRequest()
	wantOperatorRequest(t, seen, http.MethodPost, "/legion/v1/operator/claims/"+string(architectClaim)+"/retry")
	if want := `{"freshSession":true}`; string(seen.body) != want {
		t.Fatalf("retry --fresh-session sent %q, want %q", seen.body, want)
	}
	if !strings.Contains(out, " launching ") {
		t.Fatalf("retry --fresh-session stdout = %q, want the claim launching again", out)
	}
	started := d.runtime.CallsOf("Spawn")
	if len(started) != 4 {
		t.Fatalf("the runtime saw %d spawns, want the retry's fresh start after the three refused", len(started))
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
	const implementerClaim = legionclaim.Token("legion-legion-legion-209-implementer")
	retried := api.OperatorClaim{Token: implementerClaim, Tree: "LEGION-208", Issue: "LEGION-209",
		Role: "implementer", Generation: 3, State: "launching", PluginContract: api.DaemonAPIVersion}
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
