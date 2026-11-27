package docs

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gws "github.com/gorilla/websocket"
)

// A request ygo's own upgrade would admit is closed with its code by closeAtUpgrade, and not
// answered 403: the two keep one origin rule (sameOrigin). An Origin header that is present and
// empty passes ygo's rule, which reads only a non-empty Origin, and fails gorilla's default
// check, which parses it and compares an empty host with the request's.
func TestAClosedAdmissionKeepsTheUpgradesOriginRule(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closeAtUpgrade(w, r, 4503, "DOC_BUSY")
	}))
	t.Cleanup(server.Close)
	address := "ws" + strings.TrimPrefix(server.URL, "http")

	for _, origin := range [][]string{nil, {""}, {server.URL}} {
		header := http.Header{}
		if origin != nil {
			header["Origin"] = origin
		}
		connection, response, err := gws.DefaultDialer.Dial(address, header)
		if err != nil {
			status := 0
			if response != nil {
				status = response.StatusCode
			}
			t.Fatalf("Origin %q: the upgrade was answered %d (%v), want the socket closed with 4503", origin, status, err)
		}
		_, _, err = connection.ReadMessage()
		var closed *gws.CloseError
		if !errors.As(err, &closed) || closed.Code != 4503 {
			t.Fatalf("Origin %q: read %v, want the close 4503", origin, err)
		}
		_ = connection.Close()
	}

	header := http.Header{"Origin": {"https://elsewhere.example"}}
	_, response, err := gws.DefaultDialer.Dial(address, header)
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("another origin: answered %d (%v), want 403 as ygo's upgrade answers it", status, err)
	}
}
