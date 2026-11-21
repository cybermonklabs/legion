package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheOutsideOriginIsItsHostWithItsDefaultPortWrittenWithLeadingZeros (R5, red test by the
// tester of a47015a75c): a URL parser reads a port as a number (WHATWG URL: "0443" is the port 443,
// https's default, so https://outside.example.com:0443 serializes as the outside origin itself),
// and a client may send that Host. Such a request is answered as the outside origin: GET
// /auth/start answers its 404, and the passkey ceremonies serve. The row asks what
// TestTheOutsideOriginIsItsHostHoweverSpelled asks of ":443".
//
// At a47015a75c it failed: publicOrigin's sameHost then dropped the port only when it was written
// "443", so /auth/start answered 302, the canonical origin's redirect, for every Host below.
func TestTheOutsideOriginIsItsHostWithItsDefaultPortWrittenWithLeadingZeros(t *testing.T) {
	r := newPasskeyRig(t, nil)
	for _, host := range []string{passkeyRPID + ":0443", passkeyRPID + ":00443", strings.ToUpper(passkeyRPID) + ".:0443"} {
		start := httptest.NewRequest(http.MethodGet, "https://"+host+"/auth/start", nil)
		started := httptest.NewRecorder()
		r.handler.ServeHTTP(started, start)
		wantCode(t, "GET /auth/start with Host "+host, started, http.StatusNotFound, "NOT_FOUND")
		begin := httptest.NewRequest(http.MethodPost, "https://"+host+"/auth/passkey/login/begin", strings.NewReader("{}"))
		begin.Header.Set("Content-Type", "application/json")
		begin.Header.Set("Origin", passkeyOrigin)
		begun := httptest.NewRecorder()
		r.handler.ServeHTTP(begun, begin)
		if begun.Code != http.StatusOK {
			t.Errorf("a sign-in's begin with Host %s answered %d %s, want 200, the outside origin's", host, begun.Code, begun.Body.String())
		}
	}
	// Control: a port that is not the default is another origin, answered as the canonical one.
	other := httptest.NewRequest(http.MethodGet, "https://"+passkeyRPID+":8443/auth/start", nil)
	answered := httptest.NewRecorder()
	r.handler.ServeHTTP(answered, other)
	if answered.Code != http.StatusFound {
		t.Fatalf("GET /auth/start with Host %s:8443 answered %d %s, want 302: only the default port is the outside origin", passkeyRPID, answered.Code, answered.Body.String())
	}
}
