package record

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
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

// The commit an agent's step is named as its own is read from the rows each of its deciding
// reviews writes that commit onto (AgentsOtherRow, workflow's shareReviewedHead), and from no
// other: its rows of the role on the task at other places, place 0 among them, and no row of
// another agent, task or role, nor the step's own. Agent "" (an issue whose key names no claims)
// has no such row, since no review writes onto a row that names no claim. Before, the read was a
// scan of its own, which took a row of no claim for agent "" and passed over place 0: it named a
// commit no review shared there, and missed one a review did.
func TestAnAgentsReviewedHeadIsReadFromTheRowsItsReviewsWriteOnto(t *testing.T) {
	const keyA claim.Token = "legion-legion-infra-12-implementer"
	const keyS claim.Token = "legion-legion-infra-12-tester"
	const read = "4444444444444444444444444444444444444444"
	for _, tc := range []struct {
		name  string
		agent claim.Token
		row   PhaseRow
		want  string
	}{
		{name: "the agent's row of an earlier step", agent: keyA, want: read,
			row: PhaseRow{Role: claim.RoleReviewer, StepTask: "t", StepPlace: 2, Claim: keyA, ReviewedHead: read}},
		{name: "the agent's row of a later step", agent: keyA, want: read,
			row: PhaseRow{Role: claim.RoleReviewer, StepTask: "t", StepPlace: 6, Claim: keyA, ReviewedHead: read}},
		{name: "the agent's row at place 0 of the task", agent: keyA, want: read,
			row: PhaseRow{Role: claim.RoleReviewer, StepTask: "t", StepPlace: 0, Claim: keyA, ReviewedHead: read}},
		{name: "another agent's row", agent: keyA,
			row: PhaseRow{Role: claim.RoleReviewer, StepTask: "t", StepPlace: 2, Claim: keyS, ReviewedHead: read}},
		{name: "the agent's row of another task", agent: keyA,
			row: PhaseRow{Role: claim.RoleReviewer, StepTask: "u", StepPlace: 2, Claim: keyA, ReviewedHead: read}},
		{name: "the agent's row of another role", agent: keyA,
			row: PhaseRow{Role: claim.RoleTester, StepTask: "t", StepPlace: 2, Claim: keyA, ReviewedHead: read}},
		{name: "the step's own row", agent: keyA,
			row: PhaseRow{Role: claim.RoleReviewer, StepTask: "t", StepPlace: 4, Claim: keyA, ReviewedHead: read}},
		{name: "a row of no claim, for an agent", agent: keyA,
			row: PhaseRow{Role: claim.RoleReviewer, StepTask: "t", StepPlace: 2, ReviewedHead: read}},
		{name: "a row of no claim, for agent \"\"",
			row: PhaseRow{Role: claim.RoleReviewer, StepTask: "t", StepPlace: 2, ReviewedHead: read}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []PhaseRow{tc.row}
			got := AgentsReviewedHead(rows, claim.RoleReviewer, "t", 4, tc.agent)
			if got != tc.want {
				t.Fatalf("AgentsReviewedHead(reviewer, t, 4, %q) = %q, want %q", tc.agent, got, tc.want)
			}
			step := PhaseRow{Role: claim.RoleReviewer, StepTask: "t", StepPlace: 4, Claim: tc.agent}
			if written := AgentsOtherRow(step, tc.row); written != (got != "") {
				t.Fatalf("AgentsReviewedHead(reviewer, t, 4, %q) = %q, where a review of the agent writes onto the row: %t",
					tc.agent, got, written)
			}
		})
	}
}

// A role that stands at two steps has a row for each, and a reader with no walk reads the row with
// the highest entry: the row of the step the role works now, or worked last, since a step's start
// raises its entry above every row of its role. After a return to step 1, that is step 1's row
// again, raised above step 3's. A row no entry was raised on loses, and of two rows on one entry,
// which only a release before that rule wrote, the later step wins. The claim of the role's worker
// is that row's (WorkerClaim).
func TestARolesRowNowIsTheRowWithTheHighestEntry(t *testing.T) {
	const first, second claim.Token = "legion-legion-infra-12-implementer", "legion-legion-infra-12-reviewer"
	for _, tc := range []struct {
		name  string
		rows  []PhaseRow
		place int
	}{
		{name: "the later step's entry is the highest", place: 3, rows: []PhaseRow{
			{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 1, Claim: first, Entry: 1},
			{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 3, Claim: second, Entry: 3}}},
		{name: "a return raised the earlier step's entry above it", place: 1, rows: []PhaseRow{
			{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 1, Claim: first, Entry: 4},
			{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 3, Claim: second, Entry: 3}}},
		{name: "a row no entry was raised on loses", place: 3, rows: []PhaseRow{
			{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 1, Claim: first},
			{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 3, Claim: second, Entry: 3}}},
		{name: "two rows on one entry, the earlier step read first", place: 3, rows: []PhaseRow{
			{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 1, Claim: first, Entry: 3},
			{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 3, Claim: second, Entry: 3}}},
		{name: "two rows on one entry, the later step read first", place: 3, rows: []PhaseRow{
			{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 3, Claim: second, Entry: 3},
			{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 1, Claim: first, Entry: 3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PhaseRowOf(tc.rows, claim.RoleImplementer); got.StepPlace != tc.place {
				t.Fatalf("PhaseRowOf(implementer) = the row at place %d, want the row at place %d", got.StepPlace, tc.place)
			}
			want := map[int]claim.Token{1: first, 3: second}[tc.place]
			if got, err := WorkerClaim("legion", "INFRA-12", claim.RoleImplementer, tc.rows); err != nil || got != want {
				t.Fatalf("WorkerClaim(implementer) = %q, %v, want %q", got, err, want)
			}
		})
	}
}

// The row of a role where a task stands: the row of its step there where that step is the role's;
// else the row of the role's latest step before it, the retro and the merge (which a task reaches
// only where its issue has a pull request) standing after every step; else the row of its first
// step after it, so a read or a write before the role's first step reaches that step's row, as the
// one row of a role did. A role the task gives no place has its row at place 0. A row that does not
// stand yet is the zero row stamped with the role, the task and the place it stands at. An issue
// with no plan rows has one row for each role (PhaseRowOf).
func TestPhaseRowAtIsTheRowOfARolesStepWhereTheTaskStands(t *testing.T) {
	steps := []PlanStep{{Task: "t", Position: 1, Role: "implementer"}, {Task: "t", Position: 2, Role: "reviewer"}, {Task: "t", Position: 3, Role: "implementer"}}
	rows := []PhaseRow{
		{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 1, Entry: 1},
		{Role: claim.RoleReviewer, StepTask: "t", StepPlace: 2, Entry: 2},
		{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 3, Entry: 3},
		{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 0, Entry: 4},
		{Role: claim.RoleImplementer, Entry: 9},
	}
	step := func(position int) TaskPlace { return TaskPlace{Kind: PlaceStep, Position: position} }
	retro, merge := TaskPlace{Kind: PlaceRetro}, TaskPlace{Kind: PlaceMerge}
	for _, tc := range []struct {
		name string
		role claim.Role
		at   TaskPlace
		rows []PhaseRow
		want PhaseRow
	}{
		{"the implementer at its first step", claim.RoleImplementer, step(1), rows, rows[0]},
		{"the implementer while the reviewer's step runs", claim.RoleImplementer, step(2), rows, rows[0]},
		{"the implementer at its second step", claim.RoleImplementer, step(3), rows, rows[2]},
		{"the implementer's second step before its row stands", claim.RoleImplementer, step(3), rows[:2],
			PhaseRow{Role: claim.RoleImplementer, StepTask: "t", StepPlace: 3}},
		{"the implementer at the retro", claim.RoleImplementer, retro, rows, rows[3]},
		{"the implementer at the merge reads the retro's row", claim.RoleImplementer, merge, rows, rows[3]},
		{"the implementer at no act reads its first step's row", claim.RoleImplementer, TaskPlace{}, rows, rows[0]},
		{"the reviewer before its step", claim.RoleReviewer, step(1), rows, rows[1]},
		{"the reviewer after its step", claim.RoleReviewer, step(3), rows, rows[1]},
		{"the merger before the merge", claim.RoleMerger, step(2), rows, PhaseRow{Role: claim.RoleMerger, StepTask: "t"}},
		{"a role the task gives no place", claim.RoleTester, step(2), rows, PhaseRow{Role: claim.RoleTester, StepTask: "t"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PhaseRowAt(tc.rows, tc.role, "t", tc.at, steps); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("PhaseRowAt(%s, %+v) = %+v, want %+v", tc.role, tc.at, got, tc.want)
			}
		})
	}
	if got := PhaseRowAt(rows[4:], claim.RoleImplementer, "", step(2), nil); got.Entry != 9 {
		t.Fatalf("PhaseRowAt with no plan rows = %+v, want the role's one row", got)
	}
	// A row of no step, one a new generation cleared or the release before wrote, is the role's row
	// until its step's row stands, as it was before the step was written on it.
	if got := PhaseRowAt([]PhaseRow{rows[1], rows[4]}, claim.RoleImplementer, "t", step(1), steps); got.Entry != 9 || got.StepTask != "" {
		t.Fatalf("PhaseRowAt with the implementer's row of no step = %+v, want that row", got)
	}
	// The retro stands after every step for the role of its agent (PlaceWorker): the role of the
	// task's last step of the type implementation, the implementer where the owner does it. In
	// `coder checker`, coder of that type, the coder's row at the retro is its row of place 0, the
	// row the retro's entry is raised on, and its step's row is read while the checker's step runs.
	coded := []PlanStep{{Task: "t", Position: 1, Role: "coder", Type: plan.TypeImplementation}, {Task: "t", Position: 2, Role: "checker", Type: plan.TypeTesting}}
	coderRows := []PhaseRow{{Role: "coder", StepTask: "t", StepPlace: 1, Entry: 1}, {Role: "coder", StepTask: "t", StepPlace: 0, Entry: 2}}
	if got := PhaseRowAt(coderRows, "coder", "t", retro, coded); !reflect.DeepEqual(got, coderRows[1]) {
		t.Fatalf("PhaseRowAt(coder, the retro of coder checker) = %+v, want the coder's row of the retro %+v", got, coderRows[1])
	}
	if got := PhaseRowAt(coderRows, "coder", "t", step(2), coded); !reflect.DeepEqual(got, coderRows[0]) {
		t.Fatalf("PhaseRowAt(coder, step 2 of coder checker) = %+v, want the coder's step row %+v", got, coderRows[0])
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
			name:      "a release's workspace check",
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
