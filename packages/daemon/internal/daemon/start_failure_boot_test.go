package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A row stored ready, working or idle with a start failure, as a daemon of an earlier release
// sharing the database writes one after migration 20261123212127 ran, is restored without it, in
// the store as in the claim listing (dispatch://INFRA-5463). That holds for such a row with no
// process recorded too, which the restore repairs to launch_uncertain: the start failure goes by
// the state the claim's agent last reached. A claim stored suspended keeps its start failure:
// nothing says its agent came up after that start.
func TestABootDropsTheStartFailureOfAClaimWhoseAgentCameUp(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	running := token("LEGION-941", claim.RoleImplementer)
	unrecorded := token("LEGION-942", claim.RoleImplementer)
	waiting := token("LEGION-943", claim.RoleImplementer)
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
	// Written after the migration, as the earlier release's daemon of another project writes them.
	idle := earlierBootClaim(project, running, "LEGION-941", "ses_941", supervise.StateIdle, 1)
	ready := earlierBootClaim(project, unrecorded, "LEGION-942", "ses_942", supervise.StateReady, 2)
	ready.Locator = nil
	suspended := earlierBootClaim(project, waiting, "LEGION-943", "ses_943", supervise.StateSuspended, 3)
	suspended.Locator, suspended.CapabilityHash, suspended.SuspendedBy = nil, nil, supervise.SuspendedByOperator
	for _, c := range []supervise.Claim{idle, ready, suspended} {
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
	// Its relaunch may record a start failure of its own; the earlier one is gone.
	if got := d.claim(unrecorded); got.LastStartFailure == refusal {
		t.Errorf("%s, stored ready with no process, is listed %s with the start failure from before its agent came up; want it gone", unrecorded, got.State)
	}
	if got := stored(unrecorded); got == refusal {
		t.Errorf("%s, stored ready with no process, is stored with the start failure from before its agent came up after the boot; want it gone", unrecorded)
	}
	if got := d.claim(waiting); got.State != string(supervise.StateSuspended) || got.LastStartFailure != refusal {
		t.Errorf("%s, suspended, is listed %s with start failure %q after the boot; want suspended with %q", waiting, got.State, got.LastStartFailure, refusal)
	}
	if got := stored(waiting); got != refusal {
		t.Errorf("%s, suspended, is stored with start failure %q after the boot; want %q", waiting, got, refusal)
	}
}
