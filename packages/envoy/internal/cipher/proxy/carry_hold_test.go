package proxy

import (
	"log/slog"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/hold"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestCarryHoldLogsEveryKindThatDecidesNothing: carryHold leaves a carried call alone whenever the
// gate's verdict decides nothing, but it logs that read, as a failed read is logged. A served
// person logs nothing. Dispatch refusing the broker's own account (hold.BrokerRefused), which a
// verdict now reports apart from hold.Uncheckable, leaves the call alone and is logged.
//
// The fault: logging only hold.DeadlinePassed and hold.Uncheckable; the broker-refused carried
// call passes with no line, and the test fails naming the missing record.
func TestCarryHoldLogsEveryKindThatDecidesNothing(t *testing.T) {
	const operator = "ada@example.com"
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{operator: projectaccess.MemberFull})
	for _, row := range []struct {
		name    string
		account projectaccesstest.StaticAccount
		logged  bool
	}{
		{"a served person", dispatch.Account, false},
		{"Dispatch refusing the broker's own account", projectaccesstest.StaticAccount("not-the-broker"), true},
	} {
		t.Run(row.name, func(t *testing.T) {
			client := projectaccess.New(dispatch.URL, row.account, nil)
			gate := hold.New(hold.Config{Access: client, Now: time.Now})
			var records logtest.Recorder
			s := &Server{hold: gate, log: slog.New(&records)}
			if cut := s.carryHold(t.Context(), operator); cut != nil {
				t.Fatalf("carryHold(%s) = %+v; want nil, the call left alone", operator, cut)
			}
			lines := 0
			for _, r := range records.Records() {
				if r.Message == holdUnreadable {
					lines++
				}
			}
			switch {
			case row.logged && lines != 1:
				t.Fatalf("carryHold(%s) logged %q %d times; want 1", operator, holdUnreadable, lines)
			case !row.logged && lines != 0:
				t.Fatalf("carryHold(%s) logged %q %d times; want none, the person is served", operator, holdUnreadable, lines)
			}
		})
	}
}
