package agentloop

import (
	"strings"

	aisdk "github.com/grafana/ai-sdk"
)

// partialEveryDelta is how long a Final reply's text may grow while each of its deltas is
// parsed for a snapshot: 16 KiB, about the 4,096 output tokens the answer of the command bar is
// bounded to (going's answerOutputTokens), so every reply a model writes within its bound reaches
// OnPartial at each delta, as it streams. Past it a snapshot is parsed only once the text has
// grown by half since the last one, so the parses past it add up to at most three times the
// reply's length.
const partialEveryDelta = 16 << 10

// partialHead is how much of a reply's start a finalPartials keeps to tell a new model call's
// reply from more of the one it was reading.
const partialHead = 64

// finalPartials is a Final object's output with its partial parse kept linear in the reply.
//
// The SDK parses the whole reply so far on every text delta of a structured call: it copies the
// text and scans each byte of it, and hands each snapshot on, which its own check and the run's
// OnPartial read whole again. Over one reply that is work in the square of its length: a 4 MiB
// reply in 32 KiB deltas was 130 parses and 280 MB scanned, past the 30 s the command bar gives
// an answer under the race detector, before its 4 MiB StreamLimit was ever reached. So nothing is
// parsed of a reply whose first byte that is not white space can start no JSON value: such a
// reply is no JSON, which ParseComplete refuses whatever its snapshots said. And past
// partialEveryDelta a snapshot is parsed only once the text has grown by half since the last.
// The reply's last value is read whole by ParseComplete either way.
//
// The SDK calls ParsePartial from the one goroutine that reads the stream, with the text of the
// model call in flight, which only grows within a call; a new call's reply starts shorter or
// starts otherwise, and starts the count again.
type finalPartials struct {
	aisdk.Output
	// head is the start of the text read so far, at most partialHead bytes, and seen its length.
	head string
	seen int
	// blank is how many bytes of the text's start are white space, read up to the first other
	// byte; decided says that byte has been read, and json whether it can start a JSON value.
	blank   int
	decided bool
	json    bool
	// parsed is the text's length at the last parse.
	parsed int
}

// ParsePartial is a snapshot of text, a Final reply in the making, where the parse is due
// (finalPartials), and false where it is not or the text does not parse yet.
func (p *finalPartials) ParsePartial(text string) (any, bool) {
	if len(text) < p.seen || !strings.HasPrefix(text, p.head) {
		*p = finalPartials{Output: p.Output}
	}
	if len(p.head) < partialHead {
		// A copy, so the head holds none of the SDK's buffer, which grows with the reply.
		p.head = strings.Clone(text[:min(len(text), partialHead)])
	}
	p.seen = len(text)
	if !p.decided {
		for p.blank < len(text) && isJSONSpace(text[p.blank]) {
			p.blank++
		}
		if p.blank == len(text) {
			return nil, false
		}
		p.decided, p.json = true, startsJSONValue(text[p.blank])
	}
	if !p.json || len(text) > partialEveryDelta && len(text)-p.parsed < p.parsed/2 {
		return nil, false
	}
	p.parsed = len(text)
	return p.Output.ParsePartial(text)
}

// isJSONSpace is whether b is white space JSON allows between tokens.
func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// startsJSONValue is whether b can be the first byte of a JSON value.
func startsJSONValue(b byte) bool {
	switch b {
	case '{', '[', '"', '-', 't', 'f', 'n':
		return true
	}
	return b >= '0' && b <= '9'
}
