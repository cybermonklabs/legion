package api

import (
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// applyFreshness applies one delivery_freshness payload to g, reading nothing: a deploy source's
// bookkeeping as the trigger of project_deploy_sources writes it (migration 20261123010254), its
// project, the repository it names (null for one reading the project's default repository) and
// its times and last error. It marks every evals view of the source's project that carries the
// project's deploy sources' freshness (deliveryLiveSpec.freshness), whose next read answers it:
// the bookkeeping logs no change, so nothing else marks them. It moves the source repository it
// names, as the last read's rules find it (deliveryFreshnessHeard.rules), and the sources'
// freshness together. A payload of no source the rules hold (an events source, a project with
// delivery off, the release before's singleton row) moves nothing of that: the next read takes the
// sources as they stand.
func (h *deliveryHub) applyFreshness(g *hubGeneration, payload string) {
	var row struct {
		Project         string     `json:"project"`
		Repo            *string    `json:"repo"`
		LastEventAt     *time.Time `json:"last_event_at"`
		LastReconcileAt *time.Time `json:"last_reconcile_at"`
		LastError       *string    `json:"last_error"`
	}
	if err := json.Unmarshal([]byte(payload), &row); err != nil {
		slog.Warn("dispatch: delivery live hub could not read a freshness notification", "error", err)
		return
	}
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, view := range g.views {
		if view.spec.freshness && view.spec.project == row.Project {
			h.markDirtyLocked(g, view, now, deliveryCauseChange)
		}
	}
	repo := row.Repo
	if repo == nil {
		if defaulted, ok := g.fresh.rules.DefaultRepo(row.Project); ok {
			repo = defaulted
		}
	}
	if repo == nil {
		return
	}
	key := delivery.RuleKey(*repo)
	source, held := g.fresh.sources[key]
	if !held || source.Project != row.Project {
		return
	}
	source.LastEventAt, source.LastReconcileAt, source.LastError = row.LastEventAt, row.LastReconcileAt, row.LastError
	g.fresh.sources[key] = source
	g.fresh.combine()
	h.judgeLocked(g, now)
	// The threshold moved with the reconcile time: the worker re-arms its timer.
	signal(g.wakeWork)
}

// verdictLocked is g's freshness verdict as of now.
func (g *hubGeneration) verdictLocked(now time.Time) deliveryFreshnessVerdict {
	stale := []string{}
	if last := g.fresh.lastReconcileAt; last != nil && now.Sub(*last) > deliveryStaleAfter {
		stale = append(stale, deliveryReconciledSources...)
	}
	return deliveryFreshnessVerdict{lastError: g.fresh.lastError, stale: stale, unfetchable: g.fresh.unfetchable}
}

// judgeLocked sends every open page a freshness frame when g's verdict changed since the last one,
// and nothing otherwise; nothing at all before the first recompute set it. A subscription is sent
// the last one after its first answer (answerLocked).
func (h *deliveryHub) judgeLocked(g *hubGeneration, now time.Time) {
	if !g.known {
		return
	}
	verdict := g.verdictLocked(now)
	if verdict.unfetchable == g.sent.unfetchable && slices.Equal(verdict.stale, g.sent.stale) &&
		equalPointers(verdict.lastError, g.sent.lastError) {
		return
	}
	g.sent = verdict
	frame := g.freshnessFrameLocked(verdict)
	for conn := range g.conns {
		conn.Send(frame)
	}
}

// freshnessFrameLocked is the freshness frame naming verdict, with the times as last heard.
func (g *hubGeneration) freshnessFrameLocked(verdict deliveryFreshnessVerdict) DeliveryLiveFreshness {
	return DeliveryLiveFreshness{
		Type: "freshness",
		Freshness: delivery.DeliveryFreshnessView{
			LastEventAt: g.fresh.lastEventAt, LastReconcileAt: g.fresh.lastReconcileAt,
			LastError: verdict.lastError, UnfetchableCount: verdict.unfetchable,
		},
		Stale: verdict.stale,
	}
}
