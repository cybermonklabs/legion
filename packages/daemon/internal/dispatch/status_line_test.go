package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestARefusalWithNoWordsNamesItsStatusWithNoPhraseItWrote: Dispatch, or whatever answers in its
// place, refuses a read with no body under a reason phrase that quotes a credential's URL. The
// *Error the client answers names the status by its code and the standard phrase
// (oauth2call.StatusLine), and none of the phrase the server wrote. Captures
// dispatch.status-line/no-words.
func TestARefusalWithNoWordsNamesItsStatusWithNoPhraseItWrote(t *testing.T) {
	_, err := New(oauth2calltest.StatusLineServer(t, http.StatusBadGateway, "", ""), BearerTransport(dispatchToken, nil)).GetIssue(context.Background(), "an-issue")
	var dispatchErr *Error
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("get issue error = %T %v, want *dispatch.Error", err, err)
	}
	oauth2calltest.RequireNone(t, "dispatch.status-line/no-words", fmt.Sprintf("%s | %+v", err.Error(), dispatchErr))
	if dispatchErr.Status != http.StatusBadGateway || dispatchErr.Message != "502 Bad Gateway" {
		t.Errorf("Dispatch error = %#v, want 502 with the message 502 Bad Gateway", dispatchErr)
	}
}
