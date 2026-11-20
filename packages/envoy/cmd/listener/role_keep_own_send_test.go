package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/listenerwire"
)

// A keep whose first send went unanswered and whose resend the stream refused as in process may
// have stored its copy (storeKeptRoleCopy's ownSend), so the 503 a kept role-route send answers says
// maybe_stored however the keep then ends: at the end of its window with the stream still answering
// "in process", or with the next send refused by a full stream. The control is a keep whose every
// send is answered as the first was, which ends with the unacknowledged error last.
func TestAKeepWhoseOwnSendWentUnansweredSaysMaybeStoredHoweverItEnds(t *testing.T) {
	unanswered := fmt.Errorf("%w: %w", bus.ErrUnacknowledged, fmt.Errorf("%w: nats: duplicate message id is in process", bus.ErrDuplicateInProcess))
	for _, row := range []struct {
		name  string
		later error
	}{
		{"control: every later send answered as the first was", unanswered},
		{"every later send answered in process until the window ends", fmt.Errorf("%w: nats: duplicate message id is in process", bus.ErrDuplicateInProcess)},
		{"the next send refused by a full stream", fmt.Errorf("%w: maximum bytes exceeded", bus.ErrStreamFull)},
	} {
		t.Run(row.name, func(t *testing.T) {
			cfg := listenerDeliveryHandlerConfig{now: time.Now}
			calls := 0
			publish := func(ctx context.Context, subject string, item contracts.Envelope, msgID string) (bus.StoredAck, error) {
				calls++
				if calls == 1 {
					return bus.StoredAck{}, unanswered
				}
				return bus.StoredAck{}, row.later
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			item := listenerTestEnvelope(contracts.RoleTopicPrefix+"own-send", "own-send-00")
			_, err := storeKeptRoleCopy(ctx, cfg, "own-send", item, publish)
			if err == nil {
				t.Fatal("the keep stored the copy; want it to fail")
			}
			recorder := httptest.NewRecorder()
			status := writeBusUnavailable(recorder, fmt.Errorf("the kept lane of role own-send did not store the message: %w", err))
			var body listenerwire.APIError
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode the answer %q: %v", recorder.Body.String(), err)
			}
			if body.Code != listenerwire.CodeMaybeStored {
				t.Fatalf("after %d sends the keep is answered %d with code %q (%s); want %q", calls, status, body.Code, body.Error, listenerwire.CodeMaybeStored)
			}
		})
	}
}
