package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/testnats"
)

// An identity token the listener reaches no verdict on, because its issuer's key set cannot be
// read, is answered 503 with Retry-After: 1, and not 401: Dispatch's client makes a call refused
// 401 again on the shared token with no role mark, so a 401 here would send its role-route message
// unmarked to the old holder, where no takeover copies it for the role's next holder. The real
// Dispatch client, holding its account token and the shared token, sends a marked message to the
// role's old holder A after B took the role. With the keys unavailable the send fails as
// unavailable, to be retried, and neither A nor B receives an unmarked copy. The control is the
// same send with the keys served: kept for the role, marked, and B receives it.
func TestAnIdentityTokenTheListenerCouldNotCheckIsNoRefusal(t *testing.T) {
	warm, cold := trailingListeners(t)
	startListenerDelivery(t, warm)
	useDispatchPrincipal(cold, rigDispatchPrincipal)
	rig := &roleRouteRig{dispatchRig: &dispatchRig{warm: warm, cold: cold}, nats: testnats.URL(t)}

	for _, keysServed := range []bool{true, false} {
		name := "the issuer's key set cannot be read: the send is answered 503 and makes no unmarked copy"
		if keysServed {
			name = "control: the keys are served: the send is kept for the role, marked, and the next holder receives it"
		}
		t.Run(name, func(t *testing.T) {
			tag := fmt.Sprintf("served-%t", keysServed)
			role := "issuer-unavailable-" + tag
			issuer := newClusterIssuer(t)
			if !keysServed {
				issuer.issuer.FailKeys(http.StatusServiceUnavailable)
			}
			var statuses []int
			listener := apiAuth(listenerAPIAuth{
				token: rigSharedToken, verifier: issuer.verifier,
				accounts: []string{rigDispatchPrincipal},
				logger:   listenerRigLogger(t, "listener auth "+tag),
			}, v1Routes(t, cold))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				recorder := httptest.NewRecorder()
				listener.ServeHTTP(recorder, r)
				if r.URL.Path == "/v1/messages/send" {
					statuses = append(statuses, recorder.Code)
				}
				for key, values := range recorder.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
			}))
			t.Cleanup(server.Close)
			client := dispatchenvoy.New(server.URL,
				dispatchenvoy.WithAccountTokenFile(rig.accountTokenFile(t, issuer.token(t, rigDispatchPrincipal))),
				dispatchenvoy.WithToken(rigSharedToken))

			a := rig.register(t, "ses_issuer_a_"+tag, "A")
			rig.claim(t, a.id, role)
			b := rig.register(t, "ses_issuer_b_"+tag, "B")
			rig.claim(t, b.id, role)
			text := "issuer message " + tag
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := client.Send(ctx, dispatchenvoy.SendInput{
				TargetSession: a.id, Message: text, Payload: json.RawMessage(`{}`),
				IdempotencyKey: "issuer-" + tag, Role: role,
			})

			if keysServed {
				if err != nil || !result.Kept {
					t.Fatalf("control: the send = %+v, %v; want kept for role %s", result, err, role)
				}
				frames := received(b, 20*time.Second, text)[text]
				if len(frames) != 1 || frames[0].item.Role != role {
					t.Fatalf("control: B received %d copies, the first marked %q; want one marked %s", len(frames), roleOf(frames), role)
				}
				return
			}
			if !errors.Is(err, dispatchenvoy.ErrUnavailable) {
				t.Fatalf("the send while the key set cannot be read = %+v, %v; want unavailable, to be retried", result, err)
			}
			if len(statuses) != 1 || statuses[0] != http.StatusServiceUnavailable {
				t.Fatalf("the listener answered the sends %v; want one 503 and no retry on the shared token", statuses)
			}
			for _, s := range []*rigSession{a, b} {
				if frames := received(s, 3*time.Second, text)[text]; len(frames) != 0 {
					t.Fatalf("%s received %d copies of a send the listener could not check, the first marked %q; want none",
						s.id, len(frames), strings.TrimSpace(roleOf(frames)))
				}
			}
		})
	}
}
