//go:build e2e

package daemon

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// forwardCountProof is one project's liveProof over the run's shared broker.
// workflowNATS made ENVOY_NOTIFICATIONS when the run started.
func forwardCountProof(t *testing.T, natsURL string) liveProof {
	t.Helper()
	cfg := workflowConfig(t, natsURL)
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
