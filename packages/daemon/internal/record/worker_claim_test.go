package record

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The claim of a role's worker is the claim its row names while that row runs a step of the
// issue's plan: a keyed agent on its second step works the tester's step in the claim it started
// on its first. Every other row gives the role's own token: a row a new generation cleared, a row
// of the release before with no claim on it, and a role with no row at all.
func TestWorkerClaimIsTheClaimOfTheRolesStep(t *testing.T) {
	const keyed claim.Token = "legion-legion-infra-12-implementer"
	const own claim.Token = "legion-legion-infra-12-tester"
	for _, tc := range []struct {
		name string
		rows []PhaseRow
		want claim.Token
	}{
		{name: "the row of a step names its agent's claim", want: keyed,
			rows: []PhaseRow{{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 1, Claim: keyed}, {Role: claim.RoleTester, StepTask: "t", StepPlace: 2, Claim: keyed}}},
		{name: "a row a new generation cleared", want: own,
			rows: []PhaseRow{{Role: claim.RoleTester, Claim: keyed}}},
		{name: "a row of the release before, whose entry no completion wrote a claim on", want: own,
			rows: []PhaseRow{{Role: claim.RoleTester, StepTask: "t", StepPlace: 2, Entry: 3}}},
		{name: "no row of the role", want: own,
			rows: []PhaseRow{{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 1, Claim: keyed}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WorkerClaim("legion", "INFRA-12", claim.RoleTester, tc.rows)
			if err != nil {
				t.Fatalf("WorkerClaim: %v", err)
			}
			if got != tc.want {
				t.Fatalf("WorkerClaim(tester) = %q, want %q", got, tc.want)
			}
		})
	}
}

// A key that names no role yields no claim, as claim.NewToken refuses it, so a caller that reads
// that refusal as "no claim of this key" keeps reading it. A deployment's role is any name of the
// role grammar, so the name here is outside it.
func TestWorkerClaimRefusesARoleThatIsNoRole(t *testing.T) {
	if got, err := WorkerClaim("legion", "INFRA-12", claim.Role("Operator"), nil); err == nil {
		t.Fatalf("WorkerClaim(Operator) = %q, want the refusal of a role that is no role", got)
	}
}

// Under the task key a phases row is one agent's (PhaseRow.Key), so a later entry of a step for
// another agent writes a row of its own beside the earlier agent's, and both name the step. The
// step is one, worked by the row of its latest entry: the claim of the role's worker is that row's
// claim. Two steps of the role worked by two agents at once, the steps of two tasks, name no one
// worker, and the read is refused so the caller names the claim it means.
func TestWorkerClaimReadsEachStepAtItsLatestEntry(t *testing.T) {
	const keyed claim.Token = "legion-legion-infra-12-implementer"
	const later claim.Token = "legion-legion-infra-12-reviewer"
	moved := []PhaseRow{
		{Role: claim.RoleTester, StepTask: "a", StepPlace: 2, Entry: 2, Claim: keyed},
		{Role: claim.RoleTester, StepTask: "a", StepPlace: 2, Entry: 3, Claim: later},
	}
	if got, err := WorkerClaim("legion", "INFRA-12", claim.RoleTester, moved); err != nil || got != later {
		t.Fatalf("WorkerClaim of a step a later entry moved to %s = %q, %v; want %s, the claim of its latest entry", later, got, err, later)
	}
	twoSteps := []PhaseRow{
		{Role: claim.RoleTester, StepTask: "a", StepPlace: 2, Entry: 2, Claim: keyed},
		{Role: claim.RoleTester, StepTask: "b", StepPlace: 2, Entry: 2, Claim: later},
	}
	if got, err := WorkerClaim("legion", "INFRA-12", claim.RoleTester, twoSteps); err == nil {
		t.Fatalf("WorkerClaim of two tasks' tester steps worked by two agents = %q, want the refusal that names no one worker", got)
	}
}

// A row whose producer holds the claim it is for carries it, and a row the release before wrote
// carries none: each of the three payloads decodes both, the claim it names kept and none read as
// "" so the runner reads the worker's claim from the record.
func TestAPayloadNamesTheClaimItsProducerHolds(t *testing.T) {
	const keyed claim.Token = "legion-legion-legion-208-implementer"
	for _, tc := range []struct {
		name      string
		kind      OutboxKind
		named     string
		unnamed   string
		claimOf   func(OutboxPayload) claim.Token
		roundTrip OutboxPayload
	}{
		{
			name:      "a supervise request",
			kind:      OutboxKindSupervise,
			named:     `{"op":"start","tree":"LEGION-208","role":"tester","generation":1,"phase":"testing","entry":2,"claim":"` + string(keyed) + `"}`,
			unnamed:   `{"op":"start","tree":"LEGION-208","role":"tester","generation":1,"phase":"testing","entry":2}`,
			claimOf:   func(p OutboxPayload) claim.Token { return p.(SuperviseRequest).Claim },
			roundTrip: SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 1, Claim: keyed},
		},
		{
			name:      "a release's workspace save",
			kind:      OutboxKindIssueBundle,
			named:     `{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"tester","claim":"` + string(keyed) + `"}`,
			unnamed:   `{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"tester"}`,
			claimOf:   func(p OutboxPayload) claim.Token { return p.(IssueWorkspaceCheck).Claim },
			roundTrip: IssueWorkspaceCheck{Tree: "LEGION-208", Generation: 1, TreeGeneration: 1, Role: claim.RoleTester, Claim: keyed},
		},
		{
			name:      "an update round's ask",
			kind:      OutboxKindUpdateAsk,
			named:     `{"role":"tester","round":"r1","text":"Push.","claim":"` + string(keyed) + `"}`,
			unnamed:   `{"role":"tester","round":"r1","text":"Push."}`,
			claimOf:   func(p OutboxPayload) claim.Token { return p.(UpdateAsk).Claim },
			roundTrip: UpdateAsk{Role: claim.RoleTester, Round: "r1", Text: "Push.", Claim: keyed},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			named, err := DecodeOutboxPayload(OutboxRow{ID: 1, Kind: tc.kind, Payload: json.RawMessage(tc.named)})
			if err != nil {
				t.Fatalf("decode a row that names its claim: %v", err)
			}
			if got := tc.claimOf(named); got != keyed {
				t.Fatalf("the claim of a row that names one = %q, want %q", got, keyed)
			}
			unnamed, err := DecodeOutboxPayload(OutboxRow{ID: 2, Kind: tc.kind, Payload: json.RawMessage(tc.unnamed)})
			if err != nil {
				t.Fatalf("decode a row of the release before: %v", err)
			}
			if got := tc.claimOf(unnamed); got != "" {
				t.Fatalf("the claim of a row of the release before = %q, want none", got)
			}
			row, err := NewOutboxRow("LEGION-208", tc.roundTrip, time.Now())
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			back, err := DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("decode what was encoded: %v", err)
			}
			if got := tc.claimOf(back); got != keyed {
				t.Fatalf("the claim after a round trip = %q, want %q", got, keyed)
			}
		})
	}
}

// A claim a row names is an agent's claim token on an issue; anything else is refused when the row
// is written, rather than reaching no machine when it runs.
func TestAPayloadRefusesAClaimThatIsNoClaimToken(t *testing.T) {
	for _, payload := range []OutboxPayload{
		SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 1, Claim: "tester"},
		IssueWorkspaceCheck{Tree: "LEGION-208", Generation: 1, TreeGeneration: 1, Role: claim.RoleTester, Claim: "tester"},
		UpdateAsk{Role: claim.RoleTester, Round: "r1", Text: "Push.", Claim: "tester"},
		HookPrompt{Hook: "back", Role: claim.RoleTester, Claim: "tester", From: phase.Testing, To: phase.Implementing, Count: 1, Prompt: "Read it."},
		MergeForwardMessage{Role: claim.RoleImplementer, Claim: "tester", Step: MergeForwardAsk, Head: "c0de", Base: "main",
			Phase: phase.Testing, PullRequest: 42, Why: ConflictMergeNext},
	} {
		if _, err := NewOutboxRow("LEGION-208", payload, time.Now()); err == nil {
			t.Errorf("%T naming the claim %q was written, want it refused", payload, "tester")
		}
	}
}
