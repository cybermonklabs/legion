package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// deliveryFreshnessRow is the settings row's freshness columns: as migration 0100's trigger writes
// them into a delivery_freshness notification, and as the read after a LISTEN takes them
// (readFreshness).
type deliveryFreshnessRow struct {
	LastEventAt     *time.Time `json:"last_event_at"`
	LastReconcileAt *time.Time `json:"last_reconcile_at"`
	LastError       *string    `json:"last_error"`
}

// applyFreshness applies one delivery_freshness payload to g, reading nothing.
func (h *deliveryHub) applyFreshness(g *hubGeneration, payload string) {
	var row deliveryFreshnessRow
	if err := json.Unmarshal([]byte(payload), &row); err != nil {
		slog.Warn("dispatch: delivery live hub could not read a freshness notification", "error", err)
		return
	}
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hearFreshnessLocked(g, row, now)
}

// readFreshness reads the freshness row and applies it to g, after each LISTEN of g's listener,
// the first included (intake). A write of the freshness columns alone (a reconcile pass, its
// error, a GitHub event) logs no delivery change (migration 0156) and reaches the hub only as a
// delivery_freshness notification, which Postgres sends only to a connection that listens when
// the write commits. For one committed while the listener was down, or before its first LISTEN,
// which the generation's first recompute does not wait for, the hub would keep the older verdict
// until the next such write. The read follows the LISTEN, so it holds every row committed before
// it. It applies its row only when g has heard none since it began (heard): every commit after
// the LISTEN is notified, in commit order, so once a row has been heard the notifications alone
// reach the newest, and the read's row, which may be older, would replace it. A read that fails
// is tried again a second later, on the hub's clock; with no settings row there is nothing to
// hear.
func (h *deliveryHub) readFreshness(g *hubGeneration) {
	h.mu.Lock()
	heard := g.heard
	h.mu.Unlock()
	settings, err := delivery.GetSettings(g.readContext(), h.server.deps.Store.Pool)
	if errors.Is(err, delivery.ErrNoSettings) {
		return
	}
	if err != nil {
		if g.ctx.Err() == nil {
			slog.Warn("dispatch: delivery live hub could not read the freshness row; retrying", "error", err)
			h.clock.AfterFunc(time.Second, func() { signal(g.wakeFreshness) })
		}
		return
	}
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if g.heard != heard {
		return
	}
	h.hearFreshnessLocked(g, deliveryFreshnessRow{
		LastEventAt: settings.LastEventAt, LastReconcileAt: settings.LastReconcileAt, LastError: settings.LastError,
	}, now)
}

// hearFreshnessLocked applies one freshness row to g and counts it (heard), sending every open
// page the verdict when it changed.
func (h *deliveryHub) hearFreshnessLocked(g *hubGeneration, row deliveryFreshnessRow, now time.Time) {
	g.fresh.lastEventAt, g.fresh.lastReconcileAt, g.fresh.lastError = row.LastEventAt, row.LastReconcileAt, row.LastError
	g.heard++
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
