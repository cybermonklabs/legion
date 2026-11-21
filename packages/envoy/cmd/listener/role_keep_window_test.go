package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A walk whose 60 s window (roleKeepDrainWindow) ends inside the confirm of the copy it forwarded
// leaves that confirm unknown: the write and its read-back both run under the ended window. The
// handover settles it with the guard given back, finds it did not land, and runs again; its next
// pass finds the copy's claim as old as the walk's window, which is the claim's own
// (bus.KeptRoleClaimWindow), takes the claim over, and walks the rest of the lane. Three copies are
// kept for a live holder, and the forward of the first is held until the walk's context has ended;
// every copy must be forwarded, the lane drained, and nothing logged at ERROR. Each wait ends only
// at the test's deadline; the row takes about one window (60 s).
//
// The control is the handover at 263ef43b50, which ended on the unknown confirm, logged the drain
// as failed and left the other copies to the sweep: one forward, and the lane left at kept 0 with
// the first copy claimed. The row fails at its count of the forwarded copies.
func TestAWalkWhoseWindowEndsInsideAConfirmGoesOnToTheRestOfTheLane(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := harness.config
	const (
		role   = "kept-window-ends-in-confirm"
		holder = "ses_kept_window_ends_in_confirm"
	)
	var items []contracts.Envelope
	for _, n := range []string{"-1", "-2", "-3"} {
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+n)
		if _, err := keepRoleMessage(cfg, role, item); err != nil {
			t.Fatalf("keep %s; the listener log says why:\n%s", item.DedupeKey, harness.logs.String())
		}
		items = append(items, item)
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	var (
		mu        sync.Mutex
		forwarded []string
		first     = true
	)
	cfg.forwardRole = func(ctx context.Context, _ string, item contracts.Envelope, _ string, _ time.Duration) (func() error, error) {
		return func() error {
			mu.Lock()
			held := first
			first = false
			mu.Unlock()
			if held {
				// The walk's own window ends while its forward is still out.
				<-ctx.Done()
			}
			mu.Lock()
			forwarded = append(forwarded, item.DedupeKey)
			mu.Unlock()
			return nil
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	if err := drainKeptRole(ctx, cfg, role, time.Now); err != nil {
		t.Fatalf("the handover answered %v; log:\n%s", err, harness.logs.String())
	}

	mu.Lock()
	got := append([]string(nil), forwarded...)
	mu.Unlock()
	for _, item := range items {
		found := false
		for _, key := range got {
			found = found || strings.HasSuffix(key, item.DedupeKey)
		}
		if !found {
			t.Fatalf("the handover forwarded %v, which lacks %s; want every copy forwarded; log:\n%s", got, item.DedupeKey, harness.logs.String())
		}
	}
	stored, err := harness.client.ReadKeptRoleCursor(ctx, role)
	if err != nil {
		t.Fatalf("read the cursor: %v", err)
	}
	head, err := readKeptLaneHead(ctx, harness.client, role)
	if err != nil {
		t.Fatalf("read the lane's head: %v", err)
	}
	if stored.Cursor.Claiming != 0 || stored.Cursor.Kept != head.last {
		t.Fatalf("the cursor stands at %+v, want past the lane's last copy %d with nothing claimed", stored.Cursor, head.last)
	}
	if logs := harness.logs.String(); strings.Contains(logs, `"level":"ERROR"`) {
		t.Fatalf("the handover logged at ERROR:\n%s", logs)
	}
}
