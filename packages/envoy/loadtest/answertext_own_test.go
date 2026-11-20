package main

import (
	"net/http"
	"testing"
)

// TestBodyTextKeepsDispatchsOwnRefusal: Dispatch's own error JSON is named as it always was, its
// first 300 bytes after the status, and a request that got no answer names no body. It sits apart
// from answertext_test.go, whose proxy-echo rows the check also runs on a tree without bodyText.
func TestBodyTextKeepsDispatchsOwnRefusal(t *testing.T) {
	const own = `{"error":"issue LGA-7 has no references document","code":"NOT_FOUND"}`
	a := newAPI("http://127.0.0.1:1", echoAgentBearer, 1)
	if got := a.bodyText(callResult{status: http.StatusNotFound, body: []byte(own)}); got != own {
		t.Errorf("bodyText of Dispatch's own refusal = %q, want it whole", got)
	}
	if got := a.bodyText(callResult{body: []byte("unread")}); got != "" {
		t.Errorf("bodyText of a request that got no answer = %q, want nothing", got)
	}
}
