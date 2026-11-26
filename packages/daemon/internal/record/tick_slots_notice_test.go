package record

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// A slots reading rides the controller's tick notice and no other notice: the tick is the turn
// that posts the day's report, and the reading is the one the state's `admission.slots` gives. A
// tick carrying one is written and read back whole, through the validator every outbox payload
// passes; another controller notice and an architect notice carrying one are refused, by name.
func TestOnlyTheTickNoticeCarriesASlotsReading(t *testing.T) {
	oldest := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	reading := SlotsReading{InUse: 1, Cap: 2, Working: StateCount{Count: 1, Oldest: &oldest},
		BelowFirstWaiting: BelowFirstWaiting{First: "ACME-2"}}
	reading.Line = SlotsLine(reading)

	tick, err := NewOutboxRow("ACME", ControllerNotice{Kind: TickNotice, Slots: &reading}, oldest)
	if err != nil {
		t.Fatalf("write a tick carrying a slots reading: %v", err)
	}
	back, err := DecodeOutboxPayload(tick)
	if err != nil {
		t.Fatalf("read the tick back: %v", err)
	}
	if notice, ok := back.(ControllerNotice); !ok || notice.Slots == nil || !reflect.DeepEqual(*notice.Slots, reading) {
		t.Fatalf("the tick read back as %#v, want its slots reading %#v", back, reading)
	}

	for _, refused := range []struct {
		name    string
		issue   string
		payload OutboxPayload
		reason  string
	}{
		{"a todo wake", "ACME-2", ControllerNotice{Kind: TodoNotice, Slots: &reading}, "carries a slots reading only when it is a tick"},
		{"an architect notice", "ACME-1", Notice{Kind: "held", Slots: &reading}, "only the controller's tick notice does"},
	} {
		if _, err := NewOutboxRow(refused.issue, refused.payload, oldest); err == nil || !strings.Contains(err.Error(), refused.reason) {
			t.Errorf("%s carrying a slots reading was written with %v, want it refused: %s", refused.name, err, refused.reason)
		}
	}
}
