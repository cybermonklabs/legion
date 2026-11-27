// packages/envoy/internal/cipher/helper/request_audience_test.go
//go:build linux

package helper

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestARequestObjectTheHelperSignsNamesTheBrokersURL: the helper signs a request object for its
// broker twice - the machine login's, sent with the device authorization (Login), and a
// session's, which sign-request answers (signRequest) - and the broker verifies each for its own
// public URL exactly (record.VerifyRequestObject). Its audience is proof.Audience of the address
// (Broker.audience): the address's scheme, host, port and path, with none of the query or user
// information it carries (query, user-information). Control: an address with neither is the
// audience as written (plain). A stand-in at the address records the login's request object,
// answers it 400, and enrols the session; it takes a route at the end of the path or of the
// query, so a call whose route sat inside the address's query still reaches it.
func TestARequestObjectTheHelperSignsNamesTheBrokersURL(t *testing.T) {
	logins := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		at := func(route string) bool {
			return strings.HasSuffix(r.URL.Path, route) || strings.HasSuffix(r.URL.RawQuery, route)
		}
		switch {
		case at("/v1/oauth/device_authorization"):
			_ = r.ParseForm()
			logins <- r.PostForm.Get("request")
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		case at("/v1/enrollments"):
			writeJSON(w, http.StatusCreated, map[string]string{"enrollment_id": "enr-1", "lease_expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	public := srv.URL + "/broker"
	for _, c := range []struct{ name, address string }{
		{"query", public + "?sig=k3y-7Qz4k"},
		{"user-information", "http://" + "ada:pass-7Qz4k@" + host + "/broker"},
		{"plain", public},
	} {
		t.Run(c.name, func(t *testing.T) {
			key, err := proof.NewKey()
			if err != nil {
				t.Fatal(err)
			}
			b := &Broker{URL: cipheraddr.New(c.address), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t),
				HTTP: srv.Client(), Log: slog.New(slog.DiscardHandler)}
			b.cred.Store(&machineCredential{key: key, id: "lcred-1", gone: make(chan struct{})})
			// The stand-in refuses the login: the request object it carried is what the row reads.
			_, loginErr := b.Login(t.Context(), "helper-host")
			select {
			case login := <-logins:
				if _, err := record.VerifyRequestObject(login, public, time.Minute, time.Now()); err != nil {
					t.Errorf("the machine login's request object at the %s address = %v; want it verified for %q", c.name, err, public)
				}
			default:
				t.Errorf("the machine login at the %s address reached no device authorization: %v", c.name, loginErr)
			}
			r := serveBroker(t, b, "", slog.New(slog.DiscardHandler))
			if reg := r.call(t, Request{Op: "register", WaitSeconds: 5}); !reg.OK || reg.State != "enrolled" {
				t.Fatalf("register: %+v", reg)
			}
			signed := r.call(t, Request{Op: "sign-request", Secrets: []string{"DEEL_API_KEY"}, Reason: "x"})
			if !signed.OK {
				t.Fatalf("sign-request: %+v", signed)
			}
			if _, err := record.VerifyRequestObject(signed.RequestObject, public, time.Minute, time.Now()); err != nil {
				t.Errorf("sign-request's request object at the %s address = %v; want it verified for %q", c.name, err, public)
			}
		})
	}
}
