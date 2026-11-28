package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The boot writes back every claim its normalization changed, so the stored row says what the
// restored machine holds. A claim an earlier daemon stored suspended, recording no process and a
// death charged against the process it no longer records, is restored with the charge cleared
// (supervise.NormalizeClaim), and the row is written so; before, the row kept a charge the
// restored claim no longer held until some other write of the claim carried the cleared value.
func TestABootWritesBackTheDeathChargeItsNormalizationClears(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)("LEGION-951", claim.RoleImplementer)

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suspended := earlierBootClaim(project, token, "LEGION-951", "ses_951", supervise.StateSuspended, 1)
	suspended.Locator, suspended.CapabilityHash, suspended.SuspendedBy = nil, nil, supervise.SuspendedByOperator
	suspended.DeathCharged = true
	if err := st.PutClaim(ctx, suspended); err != nil {
		t.Fatalf("record %s: %v", token, err)
	}

	d := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))

	if got := d.claim(token); got.State != string(supervise.StateSuspended) {
		t.Fatalf("%s is %s after the boot, want suspended as it was stored", token, got.State)
	}
	var charged bool
	if err := st.Pool().QueryRow(ctx, `select death_charged from claims where token = $1`, string(token)).Scan(&charged); err != nil {
		t.Fatalf("read %s's stored death charge: %v", token, err)
	}
	if charged {
		t.Errorf("%s, which records no process, is still stored with a death charged after the boot; want the boot to write back the charge its normalization cleared", token)
	}
}
