package main

import "github.com/sjawhar/envoy/internal/redact/answertest"

// resultOf is the callResult of an answer with status and body, read as callRawKeyed reads one:
// its answer (redact.Answer) and its bytes.
func resultOf(status int, body string) callResult {
	return callResult{status: status, answer: answertest.Of(status, body), body: []byte(body)}
}
