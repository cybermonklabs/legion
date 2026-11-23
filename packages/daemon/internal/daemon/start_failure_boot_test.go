package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim an earlier daemon left with a running agent is restored without the start failure that
// daemon kept from before the agent came up, in the store as in the claim listing: a start failure
// on a claim is from after its last ready (dispatch://INFRA-5463). An earlier daemon kept the
// sentence until another failed start replaced it, so a claim waiting to launch later showed a
// refusal from days before as the reason it was not running. A claim left suspended keeps its
// start failure: nothing says its agent came up after that start.
func TestABootDropsTheStartFailureOfAClaimWhoseAgentCameUp(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	running, waiting := token("LEGION-941", claim.RoleImplementer), token("LEGION-942", claim.RoleImplementer)
	const refusal = "launch: start through its authenticated launcher: the shared public leak check could not be installed: the served check could not be run: waitid: no child processes"

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	idle := earlierBootClaim(project, running, "LEGION-941", "ses_941", supervise.StateIdle, 1)
	suspended := earlierBootClaim(project, waiting, "LEGION-942", "ses_942", supervise.StateSuspended, 2)
	suspended.Locator, suspended.CapabilityHash, suspended.SuspendedBy = nil, nil, supervise.SuspendedByOperator
	for _, c := range []supervise.Claim{idle, suspended} {
		c.LastStartFailure = refusal
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatalf("record %s: %v", c.Token, err)
		}
	}

	d := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))

	stored := func(tok claim.Token) string {
		t.Helper()
		var failure string
		if err := st.Pool().QueryRow(ctx, `select last_start_failure from claims where token = $1`, string(tok)).Scan(&failure); err != nil {
			t.Fatalf("read %s's stored start failure: %v", tok, err)
		}
		return failure
	}
	if got := d.claim(running); got.State != string(supervise.StateIdle) || got.LastStartFailure != "" {
		t.Errorf("%s, idle with its agent up, is listed %s with start failure %q after the boot; want idle with none", running, got.State, got.LastStartFailure)
	}
	if got := stored(running); got != "" {
		t.Errorf("%s, idle with its agent up, is stored with start failure %q after the boot; want none", running, got)
	}
	if got := d.claim(waiting); got.State != string(supervise.StateSuspended) || got.LastStartFailure != refusal {
		t.Errorf("%s, suspended, is listed %s with start failure %q after the boot; want suspended with %q", waiting, got.State, got.LastStartFailure, refusal)
	}
	if got := stored(waiting); got != refusal {
		t.Errorf("%s, suspended, is stored with start failure %q after the boot; want %q", waiting, got, refusal)
	}
}
