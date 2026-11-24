package envoy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAListenerRefusalPrintsNoBearerItsAnswerEchoes: an answer in the listener's place - a gateway
// in front of it, or the listener itself - echoes the bearer Dispatch's call carried: a 502 or a
// 403 to a session read (unavailableGetStatus) and to a send (listenerResponseError), a 503 to the
// session list (listFailure), and a lapsed role refusal whose holder is the bearer
// (lapsedRoleHolder). No error holds it, under Error, %v or %+v, and no route sends it back: the
// client reads every refused answer through the record of the request it sent (redact.Carried),
// the holder it would send back among it. Captures envoy.answer-echo/<case>/<call>.
func TestAListenerRefusalPrintsNoBearerItsAnswerEchoes(t *testing.T) {
	calls := map[string]func(*Client) error{
		"session": func(c *Client) error { _, err := c.Session(context.Background(), "s-echo"); return err },
		"send": func(c *Client) error {
			_, err := c.Send(context.Background(), SendInput{TargetSession: "s-echo", Message: "m", IdempotencyKey: "k"})
			return err
		},
		"sessions": func(c *Client) error { _, err := c.Sessions(context.Background()); return err },
		"role":     func(c *Client) error { _, err := c.Role(context.Background(), "legion-planner"); return err },
	}
	for _, c := range []struct {
		name   string
		status int
		body   func(auth string) string
	}{
		{"502-gateway", http.StatusBadGateway, func(auth string) string {
			return `{"error":"upstream refused Authorization: ` + auth + `"}`
		}},
		{"403-listener", http.StatusForbidden, func(auth string) string {
			return `{"error":"the token ` + auth + ` names no Dispatch deployment"}`
		}},
		{"404-lapsed-holder", http.StatusNotFound, func(auth string) string {
			body, _ := json.Marshal(map[string]any{"error": "holder lapsed; asked with " + auth, "reason": "holder_lapsed",
				"holder": strings.TrimPrefix(auth, "Bearer "), "holder_state": "unresponsive", "claim_released": false})
			return string(body)
		}},
	} {
		for call, run := range calls {
			t.Run(c.name+"/"+call, func(t *testing.T) {
				var mu sync.Mutex
				var asked []string
				listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					asked = append(asked, r.URL.EscapedPath())
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(c.status)
					fmt.Fprint(w, c.body(r.Header.Get("Authorization")))
				}))
				defer listener.Close()
				err := run(New(listener.URL, WithToken(redacttest.Token)))
				if err == nil {
					t.Fatalf("the %s answered no error", call)
				}
				mu.Lock()
				routes := strings.Join(asked, "\n")
				mu.Unlock()
				capture := "envoy.answer-echo/" + c.name + "/" + call
				redacttest.RequireNone(t, capture, fmt.Sprintf("%s | %v | %+v\nroutes asked:\n%s", err.Error(), err, err, routes))
			})
		}
	}
}
