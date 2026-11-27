package api

import (
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// applyFreshness applies one delivery_freshness payload to g, the settings row's freshness columns
// as migration 0100's trigger writes them, reading nothing.
func (h *deliveryHub) applyFreshness(g *hubGeneration, payload string) {
	var row struct {
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
	g.fresh.lastEventAt, g.fresh.lastReconcileAt, g.fresh.lastError = row.LastEventAt, row.LastReconcileAt, row.LastError
	h.judgeLocked(g, now)
	// The threshold moved with the reconcile time: the worker re-arms its timer.
	signal(g.wakeWork)
}

// verdictLocked is the freshness verdict as of now for the reader whose part of a view key is
// viewer: the row's freshness, which every reader shares but for its last_error, told only to a
// reader that reads the deployment's rows (readsTheDeployment), as the reads' own freshness is
// (deliveryPopulation.lastErrorOf), and that reader's unfetchable count.
func (g *hubGeneration) verdictLocked(now time.Time, viewer string) deliveryFreshnessVerdict {
	stale := []string{}
	if last := g.fresh.lastReconcileAt; last != nil && now.Sub(*last) > deliveryStaleAfter {
		stale = append(stale, deliveryReconciledSources...)
	}
	var lastError *string
	if g.readsDeployment[viewer] {
		lastError = g.fresh.lastError
	}
	return deliveryFreshnessVerdict{lastError: lastError, stale: stale, unfetchable: g.unfetchable[viewer]}
}

// judgeLocked sends each reader's open pages a freshness frame when that reader's verdict changed
// since the last one it was sent, and nothing otherwise; nothing at all before the first recompute
// set the row, nor to a reader before its first read set its verdict. A subscription is sent the
// last one after its first answer (answerLocked).
func (h *deliveryHub) judgeLocked(g *hubGeneration, now time.Time) {
	if !g.known {
		return
	}
	frames := map[string]DeliveryLiveFreshness{}
	for viewer, sent := range g.sent {
		verdict := g.verdictLocked(now, viewer)
		if verdict.unfetchable == sent.unfetchable && slices.Equal(verdict.stale, sent.stale) &&
			equalStringPointers(verdict.lastError, sent.lastError) {
			continue
		}
		g.sent[viewer] = verdict
		frames[viewer] = g.freshnessFrameLocked(verdict)
	}
	if len(frames) == 0 {
		return
	}
	for conn, subs := range g.conns {
		told := map[string]bool{}
		for _, sub := range subs {
			viewer := sub.view.spec.viewer
			if frame, changed := frames[viewer]; changed && !told[viewer] {
				told[viewer] = true
				conn.Send(frame)
			}
		}
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
