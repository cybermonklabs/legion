package testnats

import (
	"strings"
	"testing"
)

// messageCounter counts a message once its payload and the CRLF after it have passed, however the
// stream is split, and counts nothing a control line or a payload only looks like: Reload waits
// until the server has taken in as many messages as the relay counted, so a count one too high
// waits out the whole bound and one too low reloads over a message still in flight.
func TestTheMessageCounterCountsEachWholeMessageWhereverTheStreamIsCut(t *testing.T) {
	stream := "CONNECT {\"verbose\":false,\"headers\":true}\r\nPING\r\n" +
		"SUB _INBOX.reply 1\r\n" +
		"PUB $JS.API.INFO _INBOX.reply.1 0\r\n\r\n" +
		// A payload that holds a whole PUB line and a CRLF is one message, not two.
		"PUB notes.one 21\r\nPUB notes.two 2\r\nab\r\n\r\n" +
		"HPUB notes.headed _INBOX.reply.2 22 27\r\nNATS/1.0\r\nKey: one\r\n\r\nhello\r\n" +
		"hpub notes.lower 12 12\r\nNATS/1.0\r\n\r\n\r\n" +
		"pub notes.lower 3\r\nabc\r\n" +
		"UNSUB 1\r\nPONG\r\n" +
		// A message whose payload has not all passed is not yet one.
		"PUB notes.cut 10\r\n01234"
	const whole = 5
	for _, cut := range []int{1, 2, 3, 7, 64, len(stream)} {
		var counter messageCounter
		counted := 0
		for start := 0; start < len(stream); start += cut {
			messages, err := counter.count([]byte(stream[start:min(start+cut, len(stream))]))
			if err != nil {
				t.Fatalf("pieces of %d bytes: %v", cut, err)
			}
			counted += messages
		}
		if counted != whole {
			t.Errorf("pieces of %d bytes counted %d messages, want %d", cut, counted, whole)
		}
		// The server takes a message at the LF after its payload, so neither the payload's last
		// byte nor the CR after it completes one.
		for _, rest := range []struct {
			piece string
			want  int
		}{{"56789", 0}, {"\r", 0}, {"\n", 1}} {
			messages, err := counter.count([]byte(rest.piece))
			if err != nil || messages != rest.want {
				t.Errorf("pieces of %d bytes: %q of the cut message counted %d (%v), want %d", cut, rest.piece, messages, err, rest.want)
			}
		}
	}
}

// A stream that stops reading as NATS client protocol cannot be counted, and says so rather than
// count wrong; nothing after it is counted.
func TestTheMessageCounterRefusesAStreamItCannotRead(t *testing.T) {
	for _, row := range []struct{ name, stream, says string }{
		{"a PUB with no size", "PUB notes.one\r\n", "a PUB line has 1 arguments"},
		{"a PUB with too many arguments", "PUB a b c 3\r\nabc\r\n", "a PUB line has 4 arguments"},
		{"an HPUB with one size", "HPUB notes.one 3\r\nabc\r\n", "an HPUB line has 2 arguments"},
		{"a size that is not a number", "PUB notes.one three\r\nabc\r\n", "is not a count of bytes"},
		{"a negative size", "PUB notes.one -1\r\n", "is not a count of bytes"},
		{"an endless control line", "PUB " + strings.Repeat("a", maxControlLine), "runs past"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var counter messageCounter
			messages, err := counter.count([]byte(row.stream))
			if err == nil || !strings.Contains(err.Error(), row.says) {
				t.Fatalf("counted %d with error %v, want an error saying %q", messages, err, row.says)
			}
			if messages, err := counter.count([]byte("PUB notes.after 0\r\n\r\n")); messages != 0 || err == nil {
				t.Errorf("after the refusal it counted %d with error %v, want 0 and the refusal again", messages, err)
			}
		})
	}
}
