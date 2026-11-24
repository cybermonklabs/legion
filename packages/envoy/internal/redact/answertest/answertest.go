// Package answertest makes a redact.Answer for a test that hands one to the code it tests (a
// classifier of GitHub's answers, a page decoder), and reads one back as text for a test, or a
// test harness, that asserts on the bytes an answer carried. The answer Of makes is read through
// redact.ReadAnswer from a response no request led to, so it holds no request record.
package answertest

import (
	"io"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/redact"
)

// Of is the answer a server gave with status and body.
func Of(status int, body string) redact.Answer {
	answer, err := redact.ReadAnswer(&http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, int64(len(body)))
	if err != nil {
		panic("answertest: " + err.Error())
	}
	return answer
}

// Text is the bytes answer carries, as they came (redact.Answer.StreamTo).
func Text(answer redact.Answer) string {
	var text strings.Builder
	_, _ = answer.StreamTo(&text)
	return text.String()
}

// harnessLimit bounds one answer a test harness reads (Read): far past any answer a test asserts
// on.
const harnessLimit = 64 << 20

// Read is resp's body, read through redact.ReadAnswer, as the bytes a test harness hands the test
// that asserts on them (a broker rig's Req, a browser's Response). It does not close the body.
func Read(resp *http.Response) ([]byte, error) {
	answer, err := redact.ReadAnswer(resp, harnessLimit)
	if err != nil {
		return nil, err
	}
	return []byte(Text(answer)), nil
}
