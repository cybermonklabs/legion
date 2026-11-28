package api

import (
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// readFreshness reads the settings row's freshness columns and applies them to g, as a
// delivery_freshness notification carries them (applyFreshness). The listener runs it at each of
// its connections, the first included, after the LISTEN and before it applies any notification of
// that connection (OnConnected, pglisten.Listener.listen): Postgres sends a notification only to a
// connection that listens, so one sent while the listener was down, or before its first LISTEN,
// reaches nobody and nothing sends it again. Without this read the Delivery page's banner, and the
// last reconcile and last event times beside it, stay as they were until the next freshness
// change, an hour or more where that change is the reconcile pass's own. The same window is
// already a defect for the Legion views, which the intake marks at each connection for exactly
// this reason (dirtyLegionViews).
//
// Three writers set g's freshness: this read, a notification (applyFreshness) and the first
// recompute (recompute, while !g.known). This read and the notifications run on the listener's one
// goroutine, so no notification of the connection is applied between this read and its write, and
// each one committed after the read is applied after it. The first recompute keeps what either of
// them wrote (g.heard), since its own read can be older than theirs. It judges under the hub's
// lock, as applyFreshness does, so a verdict that changed sends its frame to every open page. A
// read that fails is logged and left: the next connection runs it again, and a notification that
// arrives first moves the same fields.
func (h *deliveryHub) readFreshness(g *hubGeneration) {
	settings, err := delivery.GetSettings(g.readContext(), h.server.deps.Store.Pool)
	if err != nil {
		if g.ctx.Err() == nil {
			slog.Warn("dispatch: delivery live hub could not read the freshness its listener may have missed", "error", err)
		}
		return
	}
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	g.fresh.lastEventAt, g.fresh.lastReconcileAt, g.fresh.lastError = settings.LastEventAt, settings.LastReconcileAt, settings.LastError
	g.heard = true
	h.judgeLocked(g, now)
	// The threshold moved with the reconcile time: the worker re-arms its timer.
	signal(g.wakeWork)
}

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
	g.heard = true
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
		equalStringPointers(verdict.lastError, g.sent.lastError) {
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
