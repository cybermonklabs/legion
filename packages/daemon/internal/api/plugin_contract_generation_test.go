package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// operatorClaimOf is token's claim as the operator's listing answers it, which is the view
// `legion claims list` renders (operatorView).
func operatorClaimOf(t *testing.T, h *harness, token claim.Token) OperatorClaim {
	t.Helper()
	recorder := h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list = %d; body %s", recorder.Code, recorder.Body)
	}
	var list OperatorClaims
	decodeInto(t, recorder, &list)
	for _, c := range list.Claims {
		if c.Token == token {
			return c
		}
	}
	t.Fatalf("the operator claim list does not carry %s", token)
	return OperatorClaim{}
}

// dispatch://INFRA-2883 R7: a claim standing in a generation that has not registered is shown no
// contract at all, whatever an earlier generation of it declared. This is the fault itself: two
// role pods came back with no pi-legion, and the daemon's own state answered pluginContract 59
// for both claims — the number of a registration the replaced process had made, which the
// process standing there had not.
//
// The claim here goes through the whole of it: it is launched and shows no contract, registers
// generation 1 and shows that registration's, and then has its process found gone, which
// relaunches it at generation 2. The stored record still carries the contract and gains nothing
// else — the contract keeps its lifetime, since every decision reads it — and the view gives 0.
func TestAClaimBeforeRegistrationGivesZeroAsItsContract(t *testing.T) {
	h := newHarness(t)
	token, _ := h.launch("LEGION-208", claim.RoleArchitect)

	if got := operatorClaimOf(t, h, token).PluginContract; got != 0 {
		t.Fatalf("the view of a claim that has never registered = %d, want 0", got)
	}

	h.ready(token, "ses_architect", 1)
	if got := operatorClaimOf(t, h, token).PluginContract; got != DaemonAPIVersion {
		t.Fatalf("the view of the registered generation = %d, want its own contract %d", got, DaemonAPIVersion)
	}

	h.relaunch(token)

	stored := h.stored(token)
	if stored.Generation != 2 {
		t.Fatalf("the relaunched claim stands in generation %d, want 2", stored.Generation)
	}
	if stored.PluginContract != DaemonAPIVersion || stored.PluginContractGeneration != 1 {
		t.Fatalf("the stored claim = contract %d of generation %d, want %d of generation 1: the record keeps the contract, and says which generation declared it",
			stored.PluginContract, stored.PluginContractGeneration, DaemonAPIVersion)
	}
	if got := operatorClaimOf(t, h, token).PluginContract; got != 0 {
		t.Fatalf("the view of the relaunched generation = %d, want 0: the contract %d is the replaced process's",
			got, stored.PluginContract)
	}
	if got := PluginContractOf(stored); got != 0 {
		t.Fatalf("the state body's value for the relaunched generation = %d, want 0", got)
	}
}

// R7 holds whatever state such a claim is in. A relaunched claim that never registered can be
// suspended, fail its launches or be retired, and in each of those states nothing in the record
// but this generation distinguishes a process that registered and then died from one that never
// registered: the capability hash is cleared for every state outside HoldsCapability
// (supervise.NormalizeClaim), and the contract is cleared by nothing.
//
// The claim these states are read over is the daemon's own: it registered generation 1 and was
// relaunched into generation 2, so what varies here is the state alone.
func TestAFailedOrSuspendedGenerationThatNeverRegisteredGivesZero(t *testing.T) {
	h := newHarness(t)
	token, _ := h.launch("LEGION-208", claim.RoleArchitect)
	h.ready(token, "ses_architect", 1)
	h.relaunch(token)
	relaunched := h.stored(token)
	if relaunched.PluginContract == 0 || relaunched.PluginContractGeneration != 1 || relaunched.Generation != 2 {
		t.Fatalf("the relaunched claim = %d of generation %d at generation %d, want a contract of generation 1 at generation 2",
			relaunched.PluginContract, relaunched.PluginContractGeneration, relaunched.Generation)
	}

	for _, state := range []supervise.ClaimState{
		supervise.StateSuspended, supervise.StateFailed, supervise.StateRetired,
		supervise.StateQueued, supervise.StateLaunching, supervise.StateRegistered,
		supervise.StateReady, supervise.StateWorking, supervise.StateIdle,
	} {
		t.Run(string(state), func(t *testing.T) {
			c := relaunched
			c.State = state
			if got := operatorView(c, c.Step).PluginContract; got != 0 {
				t.Errorf("the operator view of a %s claim = %d, want 0: generation %d registered nothing",
					state, got, c.Generation)
			}
			if got := PluginContractOf(c); got != 0 {
				t.Errorf("the state body's value for a %s claim = %d, want 0", state, got)
			}

			// The same claim, in the same state, whose current generation did register: the
			// number is this process's, and is given.
			registered := c
			registered.PluginContractGeneration = registered.Generation
			if got := operatorView(registered, registered.Step).PluginContract; got != registered.PluginContract {
				t.Errorf("the operator view of a %s claim whose own generation registered = %d, want %d",
					state, got, registered.PluginContract)
			}
			if got := PluginContractOf(registered); got != registered.PluginContract {
				t.Errorf("the state body's value for a %s claim whose own generation registered = %d, want %d",
					state, got, registered.PluginContract)
			}
		})
	}
}

// What the registration itself records: the contract the plugin declared and the generation that
// declared it, written together (supervise's register), and given by every view of that claim
// while it stands in that generation. A re-registration of the same process records both again.
func TestARegisteredClaimGivesItsOwnContract(t *testing.T) {
	h := newHarnessUnrelaunchable(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: boot, SessionID: "ses_architect", OmpSessionFile: "/sessions/ses_architect.jsonl",
		AgentID: "agent-ses_architect", PluginContract: 7,
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("register = %d; body %s", recorder.Code, recorder.Body)
	}

	stored := h.stored(token)
	if stored.PluginContract != 7 || stored.PluginContractGeneration != stored.Generation {
		t.Fatalf("the stored claim = contract %d of generation %d at generation %d, want 7 of its own generation",
			stored.PluginContract, stored.PluginContractGeneration, stored.Generation)
	}
	if got := operatorClaimOf(t, h, token).PluginContract; got != 7 {
		t.Fatalf("the operator view = %d, want the 7 this generation registered", got)
	}
	if got := PluginContractOf(stored); got != 7 {
		t.Fatalf("the state body's value = %d, want the 7 this generation registered", got)
	}

	// The recorded session registering again, which is the other writer of the pair.
	again := h.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: boot, SessionID: "ses_architect", OmpSessionFile: "/sessions/ses_architect.jsonl",
		AgentID: "agent-ses_architect", PluginContract: 8,
	}, nil)
	if again.Code != http.StatusOK {
		t.Fatalf("re-register = %d; body %s", again.Code, again.Body)
	}
	stored = h.stored(token)
	if stored.PluginContract != 8 || stored.PluginContractGeneration != stored.Generation {
		t.Fatalf("after the re-registration the stored claim = contract %d of generation %d at generation %d, want 8 of its own generation",
			stored.PluginContract, stored.PluginContractGeneration, stored.Generation)
	}
	if got := operatorClaimOf(t, h, token).PluginContract; got != 8 {
		t.Fatalf("the operator view after the re-registration = %d, want 8", got)
	}
}

// The state body carries two of these numbers — each worker's claim view and the project's
// controller locator — and under R7 neither may be a process other than the one it is of. They
// reach it differently and must still answer the same.
//
// A claim's view needs the generation its registration was of, because a claim outlives the
// process it registered: the record stands through the relaunch, and its contract with it.
// The controller locator needs no column: the registration and its contract are written by one
// statement (store.RegisterController), the next mint of the capability clears the session that
// answers Registered, and a record with no registration is served as no locator at all. So the
// mint below leaves the contract on the row — and the state names no controller rather than
// naming that contract as the registered one's.
func TestTheStateBodyAndTheControllerLocatorGiveTheSame(t *testing.T) {
	h := newHarnessUnrelaunchable(t)
	registeredAt := time.Date(2026, 11, 11, 14, 18, 22, 0, time.UTC)

	generation, err := h.store.MintController(h.ctx, testProject, []byte("capability-one"))
	if err != nil {
		t.Fatalf("mint the controller capability: %v", err)
	}
	registered, err := h.store.RegisterController(h.ctx, testProject, generation, "ses_controller", []byte("secret-one"), registeredAt, 7)
	if err != nil || !registered {
		t.Fatalf("register the controller: registered %t, err %v", registered, err)
	}
	record, _, err := h.store.Controller(h.ctx, testProject)
	if err != nil {
		t.Fatalf("read the controller record: %v", err)
	}

	// The claim of that same registration, as the state body holds a worker's.
	c := supervise.Claim{
		Token: claim.ControllerToken(testProject), Project: testProject, Role: claim.RoleController,
		Generation: generation, State: supervise.StateReady, Session: "ses_controller",
		PluginContract: 7, PluginContractGeneration: generation,
	}

	locator := ControllerLocatorOf("kubernetes", record)
	if locator == nil || locator.PluginContract != 7 {
		t.Fatalf("the controller locator of the registered session = %+v, want the 7 it registered", locator)
	}
	if got := PluginContractOf(c); got != locator.PluginContract {
		t.Fatalf("the state body's claim view = %d and its controller locator = %d, want the one registration's 7",
			got, locator.PluginContract)
	}

	// The capability is minted again and the controller's claim is launched again: neither
	// process of the registration above is standing, and neither surface names its contract.
	next, err := h.store.MintController(h.ctx, testProject, []byte("capability-two"))
	if err != nil {
		t.Fatalf("mint the controller capability again: %v", err)
	}
	minted, _, err := h.store.Controller(h.ctx, testProject)
	if err != nil {
		t.Fatalf("read the controller record after the mint: %v", err)
	}
	if minted.Registered() {
		t.Fatalf("the record after the mint = %+v, want no registration: a mint clears the session", minted)
	}
	if minted.PluginContract != 7 {
		t.Fatalf("the record after the mint = contract %d, want the 7 left on the row: this is what the locator must not serve", minted.PluginContract)
	}
	relaunched := c
	relaunched.Generation = next

	if got := ControllerLocatorOf("kubernetes", minted); got != nil {
		t.Fatalf("the controller locator with no registration = %+v, want none", got)
	}
	if got := PluginContractOf(relaunched); got != 0 {
		t.Fatalf("the state body's claim view of the relaunched controller = %d, want 0", got)
	}
	if got := operatorView(relaunched, relaunched.Step).PluginContract; got != 0 {
		t.Fatalf("the operator view of the relaunched controller = %d, want 0: the two views of a claim answer alike", got)
	}
}
