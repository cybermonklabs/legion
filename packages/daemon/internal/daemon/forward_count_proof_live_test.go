//go:build e2e

package daemon

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testdb"
)

// forwardCountProof is one project's liveProof over the run's shared broker, with a store of that
// project's own. `workflowNATS` made ENVOY_NOTIFICATIONS when the run started, and the scratch
// Dispatch reconciles that stream as it starts, so this makes no stream of its own.
//
// The store is this project's alone. Three daemons run side by side in one run, and
// `workflowConfig` names one database for them all: the second daemon then boots on the first's
// claims, and an intake fact one of them applied is dropped by the others as already applied,
// which left a reviewed issue standing in `reviewing` until the run gave up (the run of
// 2026-11-26 08:35Z, `DECOY-1 stands in "reviewing", want accepting`, with the ACME daemon's
// consumer having applied DECOY's review).
func forwardCountProof(t *testing.T, natsURL string) liveProof {
	t.Helper()
	cfg := workflowConfig(t, natsURL)
	cfg.PostgresDSN = testdb.Database(t, "legion_forward_count")
	p := liveProof{t: t, cfg: cfg, board: newLiveDispatch(t, cfg.Project), notices: newNoticeListener(), records: record.NewStore()}
	p.cfg.DispatchURL = p.board.url
	listener := httptest.NewServer(p.notices)
	t.Cleanup(listener.Close)
	p.cfg.EnvoyURL = listener.URL
	var err error
	if p.project, err = claim.ProjectToken(cfg.Project); err != nil {
		t.Fatal(err)
	}
	p.js = workflowJetStream(t, natsURL)
	ctx := context.Background()
	if p.st, err = store.Open(ctx, p.cfg.PostgresDSN); err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(p.st.Close)
	if _, err := p.st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	return p
}
