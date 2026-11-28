package bus

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// A core publish whose flush saw the connection's last error change to anything but this
// publish's denial is unconfirmed, and its error names that last error by its class
// (natsconn.ConnectionClass), never by its text: a last error naming a server by a made-up host
// prints none of the host, and errors.Is still reads it beneath. The control is a server's named
// refusal, which prints as the server wrote it.
func TestAnUnconfirmedPublishNamesTheLastErrorByItsClass(t *testing.T) {
	const host = "madeUpConfirmHost0123456789.example.test"
	subject := "notifications.role.reviewer"
	before := connState{connected: true, reconnects: 2}
	for _, tc := range []struct {
		name     string
		last     error
		says     string
		withheld string
	}{
		{"a last error naming a server by its host",
			&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}},
			"name resolution failed", host},
		{"control: a server's named refusal", errors.New("nats: Authorization Violation"), "nats: Authorization Violation", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := connState{lastError: tc.last, connected: true, reconnects: 2}
			err := confirmedAcross(subject, before, after)
			if !errors.Is(err, ErrPublishUnconfirmed) || !errors.Is(err, tc.last) {
				t.Fatalf("confirmedAcross = %v, want ErrPublishUnconfirmed with the last error beneath", err)
			}
			if !strings.HasSuffix(err.Error(), "NATS reported an error during the publish to \""+subject+"\": "+tc.says) {
				t.Errorf("confirmedAcross = %q, want the last error named as %q", err, tc.says)
			}
			if tc.withheld != "" && strings.Contains(err.Error(), tc.withheld) {
				t.Errorf("confirmedAcross = %q, which names the last error's host", err)
			}
		})
	}
}
