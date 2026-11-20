package brokertest

import "github.com/sjawhar/envoy/excerpt"

// answerText is body, the broker's answer to a request that carried carried, as a failure or a
// log line names it: the broker's error JSON (an object whose error is a string, as api's
// writeError writes it) as its error and code read by the rule with carried redacted
// (excerpt.ErrorJSON), since whatever answered in the broker's place can write one too. Any other
// body was written by something in the broker's place, such as a gateway or a proxy, which can
// echo the request back, so it is named in one line with carried and any Authorization, Bearer or
// DPoP value redacted (excerpt.BodyForText), its words kept. A 2xx body a failure names is one
// that did not hold what the step asked for, so it is named the same way.
func answerText(body []byte, carried ...string) string {
	if text, ok := excerpt.ErrorJSON(body, carried...); ok {
		return text
	}
	return excerpt.BodyForText(string(body), "the broker's error JSON", carried...)
}

// answered is the broker's answer to one request, and carried, every credential the request
// carried: its auth headers' values and what its body holds (a projected token, a signed request
// object).
type answered struct {
	status  int
	body    []byte
	carried []string
}

// text is the answer's body as a log line and a step's failure name it: the broker's error JSON
// as the broker wrote it, and any other body, which a gateway or a proxy in the broker's place may
// have written echoing the request back, in one line with carried redacted (answerText).
func (a answered) text() string { return answerText(a.body, a.carried...) }
